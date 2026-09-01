package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lauritsk/backup/internal/operationlock"
	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/config"
	imapbackup "github.com/lauritsk/backup/internal/pimbackup/imap"
	"github.com/lauritsk/backup/internal/pimbackup/mailstore"
	"github.com/lauritsk/backup/internal/pimbackup/model"
	"github.com/lauritsk/backup/internal/pimbackup/objectstore"
	runmodel "github.com/lauritsk/backup/internal/run"
)

var ErrOperationBusy = errors.New("another backup, verification, restore, or rebuild is already running")

type Service struct {
	config          config.Config
	catalog         *catalog.Catalog
	store           *mailstore.Store
	objectStore     *objectstore.Store
	dialer          imapbackup.Dialer
	gate            *operationlock.Gate
	logger          *slog.Logger
	ctx             context.Context
	cancel          context.CancelFunc
	async           sync.WaitGroup
	initializeMutex sync.Mutex
	initialized     atomic.Bool
}

type ServiceOptions struct {
	Dialer imapbackup.Dialer
	Logger *slog.Logger
}

func OpenService(ctx context.Context, cfg config.Config, options ServiceOptions) (*Service, error) {
	if options.Dialer == nil {
		options.Dialer = imapbackup.NetworkDialer{}
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	catalogStore, err := catalog.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	fileStore, err := mailstore.New(cfg.DataDir)
	if err != nil {
		catalogStore.Close()
		return nil, err
	}
	objectStore, err := objectstore.New(cfg.DataDir)
	if err != nil {
		catalogStore.Close()
		return nil, err
	}
	gate, err := operationlock.New(cfg.DataDir, ".pimbackup.lock", ErrOperationBusy)
	if err != nil {
		catalogStore.Close()
		return nil, err
	}
	serviceContext, cancel := context.WithCancel(ctx)
	service := &Service{
		config: cfg, catalog: catalogStore, store: fileStore, objectStore: objectStore,
		dialer: options.Dialer, gate: gate, logger: options.Logger, ctx: serviceContext, cancel: cancel,
	}
	release, lockErr := service.gate.TryAcquire()
	if errors.Is(lockErr, ErrOperationBusy) {
		service.logger.Warn("startup reconciliation skipped because another process holds the operation lock")
		return service, nil
	}
	if lockErr != nil {
		service.Close()
		return nil, lockErr
	}
	initializeErr := service.ensureInitialized(ctx)
	releaseErr := release()
	if err := errors.Join(initializeErr, releaseErr); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}

func (s *Service) ensureInitialized(ctx context.Context) error {
	s.initializeMutex.Lock()
	defer s.initializeMutex.Unlock()
	if s.initialized.Load() {
		return nil
	}
	if err := s.initialize(ctx); err != nil {
		return err
	}
	s.initialized.Store(true)
	return nil
}

func (s *Service) initialize(ctx context.Context) error {
	if interrupted, err := s.catalog.MarkInterrupted(ctx); err != nil {
		return err
	} else if interrupted > 0 {
		s.logger.Warn("marked unfinished runs as interrupted", "runs", interrupted)
	}
	for _, account := range s.config.Accounts {
		if account.Protocol != "imap" {
			mode := "explicit"
			if account.URL == "" {
				mode = "discovery"
			}
			s.logger.Info("PIM endpoint", "account", account.ID, "protocol", account.Protocol, "mode", mode)
		}
		if err := s.catalog.UpsertAccount(ctx, account.ID, account.Protocol); err != nil {
			return err
		}
	}
	return s.reconcile(ctx)
}

func (s *Service) Close() error {
	s.cancel()
	s.async.Wait()
	return errors.Join(s.gate.Close(), s.catalog.Close())
}

func (s *Service) Config() config.Config { return s.config }

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationBackup, request, func(runContext context.Context) (any, error) {
		return s.backup(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueBackup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationBackup, request, func(runContext context.Context) (any, error) {
		return s.backup(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue(s.ctx, s.logger)
	return prepared.run, nil
}

func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationVerify, request, func(runContext context.Context) (any, error) {
		return s.verify(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueVerify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationVerify, request, func(runContext context.Context) (any, error) {
		return s.verify(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue(s.ctx, s.logger)
	return prepared.run, nil
}

func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationRestore, request, func(runContext context.Context) (any, error) {
		return s.restore(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueRestore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	prepared, err := s.prepareOperation(ctx, runmodel.OperationRestore, request, func(runContext context.Context) (any, error) {
		return s.restore(runContext, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue(s.ctx, s.logger)
	return prepared.run, nil
}

type operationAction func(context.Context) (any, error)

type preparedOperation struct {
	service *Service
	run     catalog.Run
	release func() error
	action  operationAction
}

func (s *Service) prepareOperation(ctx context.Context, operation runmodel.Operation, request any, action operationAction) (*preparedOperation, error) {
	release, err := s.gate.TryAcquire()
	if err != nil {
		return nil, err
	}
	if err := s.ensureInitialized(ctx); err != nil {
		_ = release()
		return nil, err
	}
	run, err := s.catalog.CreateRun(ctx, operation, request)
	if err != nil {
		_ = release()
		return nil, err
	}
	return &preparedOperation{service: s, run: run, release: release, action: action}, nil
}

func (p *preparedOperation) queue(ctx context.Context, logger *slog.Logger) {
	p.service.async.Add(1)
	go func() {
		defer p.service.async.Done()
		run, err := p.execute(ctx)
		if err != nil {
			logger.Error("asynchronous operation failed", "run_id", run.ID, "operation", run.Operation, "error", err)
		} else {
			logger.Info("asynchronous operation finished", "run_id", run.ID, "operation", run.Operation, "status", run.Status)
		}
	}()
}

func (p *preparedOperation) execute(ctx context.Context) (result catalog.Run, resultErr error) {
	defer func() {
		if err := p.release(); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("release operation lock: %w", err)
		}
	}()

	startContext, startCancel := context.WithTimeout(context.Background(), 10*time.Second)
	startErr := p.service.catalog.StartRun(startContext, p.run.ID)
	startCancel()
	if startErr != nil {
		return p.run, startErr
	}
	p.service.logger.Info("operation started", "run_id", p.run.ID, "operation", p.run.Operation)
	detail, operationErr := invokeOperation(ctx, p.action)
	if ctxErr := ctx.Err(); ctxErr != nil {
		operationErr = errors.Join(ctxErr, operationErr)
	}
	status := runmodel.StatusSucceeded
	if operationErr != nil {
		if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
			status = runmodel.StatusCanceled
		} else {
			status = runmodel.StatusFailed
		}
	}

	finishContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.service.catalog.FinishRun(finishContext, p.run.ID, status, safeRunError(operationErr), detail); err != nil {
		return p.run, errors.Join(operationErr, err)
	}
	finished, err := p.service.catalog.GetRun(finishContext, p.run.ID)
	if err != nil {
		return p.run, errors.Join(operationErr, err)
	}
	p.service.logger.Info("operation finished", "run_id", finished.ID, "operation", finished.Operation, "status", finished.Status)
	return finished, operationErr
}

func invokeOperation(ctx context.Context, action operationAction) (detail any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("operation panic: %v", recovered)
		}
	}()
	return action(ctx)
}

func safeRunError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 2000 {
		message = message[:2000] + "..."
	}
	return errors.New(message)
}

func (s *Service) Ready(ctx context.Context) error {
	if !s.initialized.Load() {
		return errors.New("startup reconciliation is waiting for the operation lock")
	}
	if err := s.catalog.Ping(ctx); err != nil {
		return err
	}
	info, err := os.Stat(s.config.DataDir)
	if err != nil {
		return fmt.Errorf("stat data directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("data path is not a directory")
	}
	return s.checkStorage()
}
