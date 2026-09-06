package appbackup

import (
	"context"
	"errors"
)

// Repair reconciles every recovery-point manifest with the catalog.
func (s *Service) Repair(ctx context.Context) (resultErr error) {
	release, err := s.gate.TryAcquire()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := s.ensureInitialized(ctx); err != nil {
		return err
	}
	points, err := s.store.readRecoveryPoints(ctx)
	if err != nil {
		return err
	}
	if err := s.catalog.PrepareRebuild(ctx); err != nil {
		return err
	}
	for _, application := range s.config.Applications {
		if err := s.catalog.UpsertApplication(ctx, application.ID); err != nil {
			return err
		}
	}
	return errors.Join(s.applyRecoveryPoints(ctx, points), s.store.cleanStaging())
}
