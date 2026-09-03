package appbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/catalog"
	"github.com/lauritsk/backup/internal/appbackup/config"
	databaseprocess "github.com/lauritsk/backup/internal/appbackup/database"
	engineprocess "github.com/lauritsk/backup/internal/appbackup/engine"
	hookprocess "github.com/lauritsk/backup/internal/appbackup/hooks"
	"github.com/lauritsk/backup/internal/appbackup/model"
	resticprocess "github.com/lauritsk/backup/internal/appbackup/restic"
	"github.com/lauritsk/backup/internal/operationlock"
	runmodel "github.com/lauritsk/backup/internal/run"
	"github.com/lauritsk/backup/internal/safeerror"
)

type Restic interface {
	Version(context.Context) (string, error)
	EnsureRepository(context.Context) error
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
type Engine interface {
	Check(context.Context, config.EngineConfig) error
}

type ServiceOptions struct {
	Restic    Restic
	Databases Databases
	Hooks     Hooks
	Engine    Engine
	Logger    *slog.Logger
}
type Service struct {
	config          config.Config
	catalog         *catalog.Catalog
	store           *fileStore
	restic          Restic
	databases       Databases
	hooks           Hooks
	engine          Engine
	gate            *operationlock.Gate
	logger          *slog.Logger
	ctx             context.Context
	cancel          context.CancelFunc
	async           sync.WaitGroup
	initializeMutex sync.Mutex
	initialized     atomic.Bool
}

var ErrOperationBusy = errors.New("another application backup, verification, or restore is already running")

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
		cat.Close()
		return nil, err
	}
	if options.Restic == nil {
		options.Restic = resticprocess.Runner{Binary: cfg.Restic.Binary, Repository: cfg.Restic.Repository, Password: cfg.Restic.ResolvedPassword, DataDir: cfg.DataDir, Timeout: cfg.Restic.Timeout.Duration}
	}
	if options.Databases == nil {
		options.Databases = databaseprocess.Runner{DataDir: cfg.DataDir}
	}
	if options.Hooks == nil {
		options.Hooks = hookprocess.Runner{}
	}
	if options.Engine == nil {
		options.Engine = engineprocess.Runner{}
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	service := &Service{config: cfg, catalog: cat, store: store, restic: options.Restic, databases: options.Databases, hooks: options.Hooks, engine: options.Engine, gate: gate, logger: options.Logger, ctx: serviceCtx, cancel: cancel}
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
	for _, app := range s.config.Applications {
		if err := s.catalog.UpsertApplication(ctx, app.ID); err != nil {
			return err
		}
	}
	points, err := s.store.readRecoveryPoints(ctx)
	if err != nil {
		return err
	}
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
				point.Error = safeerror.Clean(errors.Join(errors.New(point.Error), cleanupErr)).Error()
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
	return s.store.cleanStaging()
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
	if err := p.service.catalog.FinishRun(finishCtx, p.run.ID, status, safeerror.Clean(operationErr), detail); err != nil {
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

type executionMode int

const (
	executeNow executionMode = iota
	executeQueued
)

func (s *Service) submit(ctx context.Context, operation runmodel.Operation, request any, action operationAction, mode executionMode) (catalog.Run, error) {
	prepared, err := s.prepare(ctx, operation, request, action)
	if err != nil {
		return catalog.Run{}, err
	}
	if mode == executeQueued {
		prepared.queue()
		return prepared.run, nil
	}
	return prepared.execute(ctx)
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) { return s.backup(runCtx, runID, request) }, executeNow)
}
func (s *Service) QueueBackup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationBackup, request, func(runCtx context.Context, runID string) (any, error) { return s.backup(runCtx, runID, request) }, executeQueued)
}
func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, runID string) (any, error) { return s.verify(runCtx, runID, request) }, executeNow)
}
func (s *Service) QueueVerify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationVerify, request, func(runCtx context.Context, runID string) (any, error) { return s.verify(runCtx, runID, request) }, executeQueued)
}
func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) { return s.restore(runCtx, runID, request) }, executeNow)
}
func (s *Service) QueueRestore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.submit(ctx, runmodel.OperationRestore, request, func(runCtx context.Context, runID string) (any, error) { return s.restore(runCtx, runID, request) }, executeQueued)
}
