package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/lauritsk/backup/internal/operationexecutor"
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
	operations      *operationexecutor.Executor[catalog.Run]
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
		_ = objectStore.Close()
		catalogStore.Close()
		return nil, err
	}
	serviceContext, cancel := context.WithCancel(ctx)
	service := &Service{
		config: cfg, catalog: catalogStore, store: fileStore, objectStore: objectStore,
		dialer: options.Dialer, gate: gate, logger: options.Logger, ctx: serviceContext, cancel: cancel,
	}
	service.operations = operationexecutor.New(serviceContext, catalogStore, gate.TryAcquire, service.ensureInitialized, func(record catalog.Run) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, options.Logger)
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
	s.operations.Wait()
	return errors.Join(s.objectStore.Close(), s.gate.Close(), s.catalog.Close())
}

func (s *Service) Config() config.Config { return s.config }

func (s *Service) submit(ctx context.Context, operation runmodel.Operation, request any, action operationexecutor.Action, queued bool) (catalog.Run, error) {
	if queued {
		return s.operations.Queue(ctx, operation, request, action)
	}
	return s.operations.Run(ctx, operation, request, action)
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runContext context.Context, _ string) (any, error) { return s.backup(runContext, request) }, false)
}

func (s *Service) QueueBackup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runContext context.Context, _ string) (any, error) { return s.backup(runContext, request) }, true)
}

func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runContext context.Context, _ string) (any, error) { return s.verify(runContext, request) }, false)
}

func (s *Service) QueueVerify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runContext context.Context, _ string) (any, error) { return s.verify(runContext, request) }, true)
}

func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runContext context.Context, _ string) (any, error) { return s.restore(runContext, request) }, false)
}

func (s *Service) QueueRestore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runContext context.Context, _ string) (any, error) { return s.restore(runContext, request) }, true)
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
