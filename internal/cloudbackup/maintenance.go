package cloudbackup

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) Check(ctx context.Context) model.CheckReport {
	report := model.CheckReport{Status: "ok"}
	appendCheck := func(name string, started time.Time, err error) {
		entry := model.CheckResult{Name: name, Status: "ok", Duration: time.Since(started)}
		if err != nil {
			entry.Status = "error"
			entry.Message = err.Error()
			report.Status = "error"
		}
		report.Checks = append(report.Checks, entry)
	}
	started := time.Now()
	appendCheck("storage", started, s.checkStorage())
	started = time.Now()
	appendCheck("sqlite", started, s.catalog.QuickCheck(ctx))
	started = time.Now()
	appendCheck("rclone", started, s.rclone.Version(ctx))
	for _, source := range s.config.EnabledSources() {
		started = time.Now()
		sourceCtx, cancel := context.WithTimeout(ctx, source.Timeout.Duration)
		err := s.rclone.CheckSource(sourceCtx, source)
		cancel()
		appendCheck("source:"+source.ID, started, err)
	}
	return report
}

func (s *Service) checkStorage() error {
	file, err := os.CreateTemp(s.config.DataDir, ".cloudbackup-check-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.WriteString("ok"); err != nil {
		file.Close()
		return err
	}
	return errors.Join(file.Sync(), file.Close(), os.Remove(name))
}

func (s *Service) Ready(ctx context.Context) error {
	if !s.initialized.Load() {
		return errors.New("startup reconciliation is waiting for the operation lock")
	}
	if err := s.catalog.Ping(ctx); err != nil {
		return err
	}
	if err := s.rclone.Version(ctx); err != nil {
		return err
	}
	return s.checkStorage()
}
