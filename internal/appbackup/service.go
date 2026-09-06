package appbackup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/lauritsk/backup/internal/appbackup/catalog"
	"github.com/lauritsk/backup/internal/appbackup/config"
	databaseprocess "github.com/lauritsk/backup/internal/appbackup/database"
	hookprocess "github.com/lauritsk/backup/internal/appbackup/hooks"
	"github.com/lauritsk/backup/internal/appbackup/model"
	resticprocess "github.com/lauritsk/backup/internal/appbackup/restic"
	"github.com/lauritsk/backup/internal/operationexecutor"
	"github.com/lauritsk/backup/internal/operationlock"
	runmodel "github.com/lauritsk/backup/internal/run"
	"github.com/lauritsk/backup/internal/safeerror"
)

type Restic interface {
	Version(context.Context) (string, error)
	EnsureRepository(context.Context) error
	CheckRepository(context.Context) error
	Check(context.Context) error
	Backup(context.Context, []string, []string) (string, error)
	Restore(context.Context, string, string) error
	List(context.Context, string, int, int) ([]string, error)
}
type Databases interface {
	Version(context.Context, config.DatabaseConfig) (string, error)
	Dump(context.Context, config.DatabaseConfig, string) error
	VerifyDump(context.Context, config.DatabaseConfig, string) (string, error)
	Check(context.Context, config.DatabaseConfig) error
}
type Hooks interface {
	Run(context.Context, config.CommandConfig) error
}
type ServiceOptions struct {
	Restic             Restic
	Databases          Databases
	Hooks              Hooks
	Logger             *slog.Logger
	DeferFullReconcile bool
}
type Service struct {
	config             config.Config
	catalog            *catalog.Catalog
	store              *fileStore
	restic             Restic
	databases          Databases
	hooks              Hooks
	gate               *operationlock.Gate
	logger             *slog.Logger
	redactor           safeerror.Redactor
	operations         *operationexecutor.Executor[catalog.Run]
	initializeMutex    sync.Mutex
	initialized        atomic.Bool
	deferFullReconcile bool
}

var ErrOperationBusy = errors.New("another application backup, verification, export, or repair is already running")

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
	gate, err := operationlock.New(cfg.DataDir, ".appbackup.lock", ErrOperationBusy)
	if err != nil {
		_ = store.Close()
		cat.Close()
		return nil, err
	}
	if options.Restic == nil {
		options.Restic = resticprocess.Runner{Binary: cfg.Restic.Binary, Repository: filepath.Join(cfg.DataDir, "restic"), Password: cfg.Restic.ResolvedPassword, PasswordFile: cfg.Restic.ResolvedPasswordFile, DataDir: cfg.DataDir, Timeout: cfg.Restic.Timeout.Duration}
	}
	if options.Databases == nil {
		options.Databases = databaseprocess.Runner{DataDir: cfg.DataDir}
	}
	if options.Hooks == nil {
		options.Hooks = hookprocess.Runner{}
	}
	secretValues := []string{cfg.Restic.ResolvedPassword}
	for _, application := range cfg.Applications {
		for _, database := range application.Databases {
			secretValues = append(secretValues, database.ResolvedPassword)
		}
	}
	service := &Service{config: cfg, catalog: cat, store: store, restic: options.Restic, databases: options.Databases, hooks: options.Hooks, gate: gate, logger: options.Logger, redactor: safeerror.New(secretValues...), deferFullReconcile: options.DeferFullReconcile}
	service.operations = operationexecutor.New(cat, gate.TryAcquire, service.ensureInitialized, func(record catalog.Run) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, options.Logger, service.redactor.Clean)
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
	interrupted, err := s.catalog.MarkInterrupted(ctx)
	if err != nil {
		return err
	}
	if interrupted > 0 {
		s.logger.Warn("marked unfinished runs as interrupted", "runs", interrupted)
	}
	for _, app := range s.config.Applications {
		if err := s.catalog.UpsertApplication(ctx, app.ID); err != nil {
			return err
		}
	}
	if (s.catalog.Created() || interrupted > 0) && !s.deferFullReconcile {
		if err := s.reconcileRecoveryPoints(ctx); err != nil {
			return err
		}
	}
	return s.store.cleanStaging()
}

func (s *Service) reconcileRecoveryPoints(ctx context.Context) error {
	points, err := s.store.readRecoveryPoints(ctx)
	if err != nil {
		return err
	}
	return s.applyRecoveryPoints(ctx, points)
}

func (s *Service) applyRecoveryPoints(ctx context.Context, points []model.RecoveryPoint) error {
	for _, point := range points {
		if point.Status == "running" {
			var cleanupErr error
			application, found := s.config.Application(point.ApplicationID)
			if found {
				if phaseStarted(point.Hooks, config.HookQuiesce) {
					cleanupErr = errors.Join(cleanupErr, s.runHookPhase(ctx, config.HookUnquiesce, application.Hooks.Unquiesce, &point))
				}
				cleanupErr = errors.Join(cleanupErr, s.runHookPhase(ctx, config.HookPostBackup, application.Hooks.PostBackup, &point))
			} else {
				cleanupErr = errors.New("application is no longer configured, so cleanup hooks could not run")
			}
			point.Status = "interrupted"
			point.Error = "process stopped before the recovery point completed"
			if cleanupErr != nil {
				point.Error = s.cleanError(errors.Join(errors.New(point.Error), cleanupErr))
			}
			point.CompletedAt = nowPointer()
			if _, err := s.store.writeRecoveryPoint(point); err != nil {
				return err
			}
		}
		path, err := s.store.manifestPath(point.ID)
		if err != nil {
			return err
		}
		if err := s.catalog.ApplyRecoveryPoint(ctx, point, path); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Close() error {
	return errors.Join(s.store.Close(), s.gate.Close(), s.catalog.Close())
}
func (s *Service) Config() config.Config { return s.config }
func (s *Service) cleanError(err error) string {
	if err == nil {
		return ""
	}
	return s.redactor.Clean(err).Error()
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) { return s.backup(runCtx, runID, request) })
}
func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, runID string) (any, error) { return s.verify(runCtx, runID, request) })
}
func (s *Service) Export(ctx context.Context, request model.ExportRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationExport, request, func(runCtx context.Context, runID string) (any, error) { return s.export(runCtx, runID, request) })
}
