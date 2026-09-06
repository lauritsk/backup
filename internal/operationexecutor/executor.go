// Package operationexecutor runs cataloged operations with a shared lifecycle.
package operationexecutor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	runmodel "github.com/lauritsk/backup/internal/run"
	"github.com/lauritsk/backup/internal/safeerror"
)

const catalogTimeout = 10 * time.Second

type Catalog[R any] interface {
	CreateRun(context.Context, runmodel.Operation, any) (R, error)
	StartRun(context.Context, string) error
	FinishRun(context.Context, string, runmodel.Status, error, any) error
	GetRun(context.Context, string) (R, error)
}

type Action func(context.Context, string) (any, error)

type Executor[R any] struct {
	catalog    Catalog[R]
	acquire    func() (func() error, error)
	initialize func(context.Context) error
	identity   func(R) (string, runmodel.Operation)
	logger     *slog.Logger
	clean      func(error) error
}

func New[R any](catalog Catalog[R], acquire func() (func() error, error), initialize func(context.Context) error, identity func(R) (string, runmodel.Operation), logger *slog.Logger, clean func(error) error) *Executor[R] {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if clean == nil {
		clean = safeerror.Clean
	}
	return &Executor[R]{catalog: catalog, acquire: acquire, initialize: initialize, identity: identity, logger: logger, clean: clean}
}

func (e *Executor[R]) Run(ctx context.Context, operation runmodel.Operation, request any, action Action) (R, error) {
	prepared, err := e.prepare(ctx, operation, request, action)
	if err != nil {
		var zero R
		return zero, err
	}
	return prepared.execute(ctx)
}

type prepared[R any] struct {
	executor  *Executor[R]
	record    R
	id        string
	operation runmodel.Operation
	release   func() error
	action    Action
}

func (e *Executor[R]) prepare(ctx context.Context, operation runmodel.Operation, request any, action Action) (*prepared[R], error) {
	release, err := e.acquire()
	if err != nil {
		return nil, err
	}
	if err := e.initialize(ctx); err != nil {
		_ = release()
		return nil, err
	}
	record, err := e.catalog.CreateRun(ctx, operation, request)
	if err != nil {
		_ = release()
		return nil, err
	}
	id, recordedOperation := e.identity(record)
	return &prepared[R]{executor: e, record: record, id: id, operation: recordedOperation, release: release, action: action}, nil
}

func (p *prepared[R]) execute(ctx context.Context) (result R, resultErr error) {
	result = p.record
	defer func() { resultErr = errors.Join(resultErr, p.release()) }()

	startCtx, cancelStart := context.WithTimeout(context.Background(), catalogTimeout)
	startErr := p.executor.catalog.StartRun(startCtx, p.id)
	cancelStart()
	if startErr != nil {
		return result, startErr
	}
	p.executor.logger.Info("operation started", "run_id", p.id, "operation", p.operation)

	detail, operationErr := invoke(ctx, p.id, p.action)
	if ctxErr := ctx.Err(); ctxErr != nil {
		operationErr = errors.Join(ctxErr, operationErr)
	}
	status := runmodel.StatusSucceeded
	if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
		status = runmodel.StatusCanceled
	} else if operationErr != nil {
		status = runmodel.StatusFailed
	}

	finishCtx, cancelFinish := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancelFinish()
	if err := p.executor.catalog.FinishRun(finishCtx, p.id, status, p.executor.clean(operationErr), detail); err != nil {
		return result, errors.Join(operationErr, err)
	}
	finished, err := p.executor.catalog.GetRun(finishCtx, p.id)
	if err != nil {
		return result, errors.Join(operationErr, err)
	}
	p.executor.logger.Info("operation finished", "run_id", p.id, "operation", p.operation, "status", status)
	return finished, operationErr
}

func invoke(ctx context.Context, id string, action Action) (detail any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("operation panic: %v", recovered)
		}
	}()
	return action(ctx, id)
}
