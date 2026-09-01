package pimbackup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (s *Service) runScheduler(ctx context.Context) {
	cfg := s.config.Schedule
	if !cfg.Enabled {
		return
	}
	s.logger.Info("interval scheduler started", "interval", cfg.Interval.Duration, "run_on_start", cfg.RunOnStart)
	trigger := func() {
		run, err := s.QueueBackup(ctx, model.BackupRequest{})
		switch {
		case errors.Is(err, ErrOperationBusy):
			s.logger.Warn("scheduled backup skipped because another operation is running")
		case err != nil:
			s.logger.Error("could not queue scheduled backup", "error", err)
		default:
			s.logger.Info("scheduled backup queued", slog.String("run_id", run.ID))
		}
	}
	if cfg.RunOnStart {
		trigger()
	}
	ticker := time.NewTicker(cfg.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trigger()
		}
	}
}
