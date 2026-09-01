package cloudbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/catalog"
	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
	rcloneprocess "github.com/lauritsk/backup/internal/cloudbackup/rclone"
	"github.com/lauritsk/backup/internal/operationlock"
	runmodel "github.com/lauritsk/backup/internal/run"
)

type Rclone interface {
	Version(context.Context) error
	CheckSource(context.Context, config.SourceConfig) error
	Copy(context.Context, config.SourceConfig, string) error
}

type ServiceOptions struct {
	Rclone Rclone
	Logger *slog.Logger
}

type Service struct {
	config          config.Config
	catalog         *catalog.Catalog
	store           *fileStore
	rclone          Rclone
	gate            *operationlock.Gate
	logger          *slog.Logger
	ctx             context.Context
	cancel          context.CancelFunc
	async           sync.WaitGroup
	initializeMutex sync.Mutex
	initialized     atomic.Bool
}

var ErrOperationBusy = errors.New("another cloud backup, verification, or restore is already running")

func OpenService(ctx context.Context, cfg config.Config, options ServiceOptions) (*Service, error) {
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cat, err := catalog.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	store, err := newFileStore(cfg.DataDir)
	if err != nil {
		cat.Close()
		return nil, err
	}
	gate, err := operationlock.New(cfg.DataDir, ".cloudbackup.lock", ErrOperationBusy)
	if err != nil {
		cat.Close()
		return nil, err
	}
	if options.Rclone == nil {
		options.Rclone = rcloneprocess.Runner{Binary: cfg.Rclone.Binary, ConfigPath: cfg.Rclone.ConfigPath, DataDir: cfg.DataDir}
	}
	serviceContext, cancel := context.WithCancel(ctx)
	service := &Service{config: cfg, catalog: cat, store: store, rclone: options.Rclone, gate: gate, logger: options.Logger, ctx: serviceContext, cancel: cancel}
	release, err := gate.TryAcquire()
	if errors.Is(err, ErrOperationBusy) {
		return service, nil
	}
	if err != nil {
		service.Close()
		return nil, err
	}
	initErr := service.ensureInitialized(ctx)
	releaseErr := release()
	if err := errors.Join(initErr, releaseErr); err != nil {
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
	if count, err := s.catalog.MarkInterrupted(ctx); err != nil {
		return err
	} else if count > 0 {
		s.logger.Warn("marked unfinished runs as interrupted", "runs", count)
	}
	for _, source := range s.config.Sources {
		if err := s.catalog.UpsertSource(ctx, source.ID, source.Remote); err != nil {
			return err
		}
		if _, err := s.store.prepareSource(source.ID); err != nil {
			return err
		}
	}
	manifests, err := s.store.readLatestManifests(ctx)
	if err != nil {
		return err
	}
	for _, manifest := range manifests {
		if err := s.catalog.ApplyManifest(ctx, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Close() error {
	s.cancel()
	s.async.Wait()
	return errors.Join(s.gate.Close(), s.catalog.Close())
}

func (s *Service) Config() config.Config { return s.config }

type operationAction func(context.Context, string) (any, error)

type preparedOperation struct {
	service *Service
	run     catalog.Run
	release func() error
	action  operationAction
}

func (s *Service) prepare(ctx context.Context, operation runmodel.Operation, request any, action operationAction) (*preparedOperation, error) {
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

func (p *preparedOperation) execute(ctx context.Context) (result catalog.Run, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, p.release()) }()
	if err := p.service.catalog.StartRun(context.Background(), p.run.ID); err != nil {
		return p.run, err
	}
	detail, operationErr := invokeOperation(ctx, p.run.ID, p.action)
	status := runmodel.StatusSucceeded
	if operationErr != nil {
		status = runmodel.StatusFailed
		if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
			status = runmodel.StatusCanceled
		}
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.service.catalog.FinishRun(finishCtx, p.run.ID, status, safeError(operationErr), detail); err != nil {
		return p.run, errors.Join(operationErr, err)
	}
	finished, err := p.service.catalog.GetRun(finishCtx, p.run.ID)
	return finished, errors.Join(operationErr, err)
}

func invokeOperation(ctx context.Context, runID string, action operationAction) (detail any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("operation panic: %v", recovered)
		}
	}()
	return action(ctx, runID)
}

func (p *preparedOperation) queue() {
	p.service.async.Add(1)
	go func() {
		defer p.service.async.Done()
		run, err := p.execute(p.service.ctx)
		if err != nil {
			p.service.logger.Error("asynchronous operation failed", "run_id", run.ID, "error", err)
		}
	}()
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	value := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(value) > 2000 {
		value = value[:2000] + "..."
	}
	return errors.New(value)
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) {
		return s.backup(runCtx, runID, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueBackup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) {
		return s.backup(runCtx, runID, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue()
	return prepared.run, nil
}

func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, _ string) (any, error) {
		return s.verify(runCtx, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueVerify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, _ string) (any, error) {
		return s.verify(runCtx, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue()
	return prepared.run, nil
}

func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) {
		return s.restore(runCtx, runID, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	return prepared.execute(ctx)
}

func (s *Service) QueueRestore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) {
		return s.restore(runCtx, runID, request)
	})
	if err != nil {
		return catalog.Run{}, err
	}
	prepared.queue()
	return prepared.run, nil
}
