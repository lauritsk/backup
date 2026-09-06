package pimbackup

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	"github.com/lauritsk/backup/internal/safeerror"
)

var ErrOperationBusy = errors.New("another backup, verification, restore, or repair is already running")

type Service struct {
	config             config.Config
	catalog            *catalog.Catalog
	store              *mailstore.Store
	objectStore        *objectstore.Store
	dialer             imapbackup.Dialer
	gate               *operationlock.Gate
	logger             *slog.Logger
	redactor           safeerror.Redactor
	operations         *operationexecutor.Executor[catalog.Run]
	initializeMutex    sync.Mutex
	initialized        atomic.Bool
	deferFullReconcile bool
}

type ServiceOptions struct {
	Dialer             imapbackup.Dialer
	Logger             *slog.Logger
	DeferFullReconcile bool
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
		_ = fileStore.Close()
		catalogStore.Close()
		return nil, err
	}
	gate, err := operationlock.New(cfg.DataDir, ".pimbackup.lock", ErrOperationBusy)
	if err != nil {
		_ = fileStore.Close()
		_ = objectStore.Close()
		catalogStore.Close()
		return nil, err
	}
	secretValues := make([]string, 0, len(cfg.Accounts)*2)
	for _, account := range cfg.Accounts {
		secretValues = append(secretValues, account.ResolvedPassword, account.ResolvedToken)
	}
	service := &Service{
		config: cfg, catalog: catalogStore, store: fileStore, objectStore: objectStore,
		dialer: options.Dialer, gate: gate, logger: options.Logger, redactor: safeerror.New(secretValues...), deferFullReconcile: options.DeferFullReconcile,
	}
	service.operations = operationexecutor.New(catalogStore, gate.TryAcquire, service.ensureInitialized, func(record catalog.Run) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, options.Logger, service.redactor.Clean)
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
	interrupted, err := s.catalog.MarkInterrupted(ctx)
	if err != nil {
		return err
	}
	if interrupted > 0 {
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
	if (s.catalog.Created() || interrupted > 0) && !s.deferFullReconcile {
		s.logger.Info("reconciling canonical files", "reason", reconciliationReason(s.catalog.Created(), interrupted))
		return s.reconcile(ctx)
	}
	return nil
}

func reconciliationReason(created bool, interrupted int64) string {
	if created {
		return "new_catalog"
	}
	if interrupted > 0 {
		return "interrupted_operation"
	}
	return "explicit_repair"
}

func (s *Service) Close() error {
	return errors.Join(s.store.Close(), s.objectStore.Close(), s.gate.Close(), s.catalog.Close())
}

func (s *Service) Config() config.Config { return s.config }

func (s *Service) cleanError(err error) string {
	if err == nil {
		return ""
	}
	return s.redactor.Clean(err).Error()
}

func (s *Service) Backup(ctx context.Context, request model.BackupRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationBackup, request, func(runContext context.Context, _ string) (any, error) { return s.backup(runContext, request) })
}

func (s *Service) Verify(ctx context.Context, request model.VerifyRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationVerify, request, func(runContext context.Context, _ string) (any, error) { return s.verify(runContext, request) })
}

func (s *Service) Restore(ctx context.Context, request model.RestoreRequest) (catalog.Run, error) {
	return s.operations.Run(ctx, runmodel.OperationRestore, request, func(runContext context.Context, _ string) (any, error) { return s.restore(runContext, request) })
}
