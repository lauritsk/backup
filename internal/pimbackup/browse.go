package pimbackup

import (
	"context"
	"os"

	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (s *Service) ListAccounts(ctx context.Context) ([]model.Account, error) {
	return s.catalog.ListAccounts(ctx)
}

func (s *Service) ListMailboxes(ctx context.Context, accountID string, includeInactive bool) ([]model.Mailbox, error) {
	return s.catalog.ListMailboxes(ctx, accountID, includeInactive)
}

func (s *Service) ListMessages(ctx context.Context, filter model.MessageFilter) ([]model.Message, error) {
	return s.catalog.ListMessages(ctx, filter)
}

func (s *Service) GetMessage(ctx context.Context, id int64) (model.Message, error) {
	return s.catalog.GetMessage(ctx, id)
}

func (s *Service) OpenMessage(ctx context.Context, id int64) (model.Message, *os.File, error) {
	message, err := s.catalog.GetMessage(ctx, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	file, err := s.store.OpenMessage(message)
	return message, file, err
}

func (s *Service) ListCollections(ctx context.Context, accountID, kind string, includeInactive bool) ([]model.Collection, error) {
	return s.catalog.ListCollections(ctx, accountID, kind, includeInactive)
}

func (s *Service) ListObjects(ctx context.Context, filter model.ObjectFilter) ([]model.Object, error) {
	return s.catalog.ListObjects(ctx, filter)
}

func (s *Service) GetObject(ctx context.Context, id int64) (model.Object, error) {
	return s.catalog.GetObject(ctx, id)
}

func (s *Service) OpenObject(ctx context.Context, id int64) (model.Object, *os.File, error) {
	object, err := s.catalog.GetObject(ctx, id)
	if err != nil {
		return model.Object{}, nil, err
	}
	file, err := s.objectStore.Open(object)
	return object, file, err
}

func (s *Service) GetRun(ctx context.Context, id string) (catalog.Run, error) {
	return s.catalog.GetRun(ctx, id)
}

func (s *Service) ListRuns(ctx context.Context, limit, offset int) ([]catalog.Run, error) {
	return s.catalog.ListRuns(ctx, limit, offset)
}
