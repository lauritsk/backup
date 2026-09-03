package appbackup

import (
	"context"
	"errors"

	"github.com/lauritsk/backup/internal/appbackup/model"
)

func (s *Service) restore(ctx context.Context, runID string, request model.RestoreRequest) (model.RestoreReport, error) {
	report := model.RestoreReport{RecoveryPointID: request.RecoveryPointID}
	if !request.Confirm {
		return report, errors.New("restore requires explicit confirmation")
	}
	if request.RecoveryPointID == "" {
		return report, errors.New("restore requires recovery_point_id")
	}
	point, err := s.store.readRecoveryPoint(request.RecoveryPointID)
	if err != nil {
		return report, err
	}
	if point.SnapshotID == "" {
		return report, errors.New("recovery point has no Restic snapshot")
	}
	target, err := s.store.restoreDir(runID, point.ID)
	if err != nil {
		return report, err
	}
	report.Directory, report.SnapshotID = target, point.SnapshotID
	if err := s.restic.Restore(ctx, point.SnapshotID, target); err != nil {
		return report, err
	}
	return report, nil
}
