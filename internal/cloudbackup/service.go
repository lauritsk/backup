package cloudbackup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/lauritsk/backup/internal/cloudbackup/catalog"
	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
	rcloneprocess "github.com/lauritsk/backup/internal/cloudbackup/rclone"
	"github.com/lauritsk/backup/internal/operationexecutor"
	"github.com/lauritsk/backup/internal/operationlock"
	runmodel "github.com/lauritsk/backup/internal/run"
)

type Rclone interface {
	Version(context.Context) error
	CheckSource(context.Context, config.SourceConfig) error
	Inventory(context.Context, config.SourceConfig) ([]model.RemoteFile, error)
	Download(context.Context, config.SourceConfig, string, io.Writer) error
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
	operations      *operationexecutor.Executor[catalog.Run]
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
		_ = store.Close()
		cat.Close()
		return nil, err
	}
	if options.Rclone == nil {
		options.Rclone = rcloneprocess.Runner{ConfigPath: cfg.Rclone.ConfigPath}
	}
	serviceContext, cancel := context.WithCancel(ctx)
	service := &Service{config: cfg, catalog: cat, store: store, rclone: options.Rclone, gate: gate, logger: options.Logger, ctx: serviceContext, cancel: cancel}
	service.operations = operationexecutor.New(serviceContext, cat, gate.TryAcquire, service.ensureInitialized, func(record catalog.Run) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, options.Logger)
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
	s.operations.Wait()
	return errors.Join(s.store.Close(), s.gate.Close(), s.catalog.Close())
}

func (s *Service) Config() config.Config { return s.config }

func (s *Service) submit(ctx context.Context, operation runmodel.Operation, request any, action operationexecutor.Action, queued bool) (catalog.Run, error) {
	if queued {
		return s.operations.Queue(ctx, operation, request, action)
	}
	return s.operations.Run(ctx, operation, request, action)
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) { return s.backup(runCtx, runID, request) }, false)
}

func (s *Service) QueueBackup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) { return s.backup(runCtx, runID, request) }, true)
}

func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, _ string) (any, error) { return s.verify(runCtx, request) }, false)
}

func (s *Service) QueueVerify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, _ string) (any, error) { return s.verify(runCtx, request) }, true)
}

func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) { return s.restore(runCtx, runID, request) }, false)
}

func (s *Service) QueueRestore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) { return s.restore(runCtx, runID, request) }, true)
}
