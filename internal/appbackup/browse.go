package appbackup

import (
	"context"

	"github.com/lauritsk/backup/internal/appbackup/catalog"
	"github.com/lauritsk/backup/internal/appbackup/model"
)

func (s *Service) ListApplications(ctx context.Context) ([]model.Application, error) {
	applications, err := s.catalog.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	disabled := map[string]bool{}
	for _, application := range s.config.Applications {
		disabled[application.ID] = application.Disabled
	}
	for i := range applications {
		applications[i].Disabled = disabled[applications[i].ID]
	}
	return applications, nil
}
func (s *Service) ListRecoveryPoints(ctx context.Context, application string, limit, offset int) ([]model.RecoveryPointSummary, error) {
	return s.catalog.ListRecoveryPoints(ctx, application, limit, offset)
}
func (s *Service) GetRecoveryPoint(_ context.Context, id string) (model.RecoveryPoint, error) {
	return s.store.readRecoveryPoint(id)
}
func (s *Service) ListRecoveryPointContents(ctx context.Context, id string, limit, offset int) ([]string, error) {
	point, err := s.store.readRecoveryPoint(id)
	if err != nil {
		return nil, err
	}
	if point.SnapshotID == "" {
		return nil, catalog.ErrNotFound
	}
	return s.restic.List(ctx, point.SnapshotID, limit, offset)
}
func (s *Service) ListRuns(ctx context.Context, limit, offset int) ([]catalog.Run, error) {
	return s.catalog.ListRuns(ctx, limit, offset)
}
func (s *Service) GetRun(ctx context.Context, id string) (catalog.Run, error) {
	return s.catalog.GetRun(ctx, id)
}
