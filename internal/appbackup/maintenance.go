package appbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/safeerror"
)

func (s *Service) Check(ctx context.Context) model.CheckReport {
	report := model.CheckReport{Status: "ok"}
	appendCheck := func(name, status, message string, started time.Time) {
		entry := model.CheckResult{Name: name, Status: status, Message: message, Duration: time.Since(started)}
		report.Checks = append(report.Checks, entry)
		if status == "error" {
			report.Status = "error"
		}
	}
	run := func(name string, action func() error) {
		started := time.Now()
		err := action()
		if err != nil {
			appendCheck(name, "error", safeerror.Clean(err).Error(), started)
		} else {
			appendCheck(name, "ok", "", started)
		}
	}
	run("storage", s.checkStorage)
	run("sqlite", func() error { return s.catalog.QuickCheck(ctx) })
	run("restic", func() error { _, err := s.restic.Version(ctx); return err })
	run("restic_repository", func() error { return s.restic.Check(ctx) })
	for _, executable := range s.configExecutables() {
		executable := executable
		run(executable.name, func() error { _, err := exec.LookPath(executable.binary); return err })
	}
	for _, application := range s.config.EnabledApplications() {
		for _, database := range application.Databases {
			database := database
			run("database:"+application.ID+":"+database.ID, func() error {
				checkCtx, cancel := context.WithTimeout(ctx, database.Timeout.Duration)
				defer cancel()
				return s.databases.Check(checkCtx, database)
			})
		}
	}
	if s.config.Engine != nil {
		started := time.Now()
		appendCheck("engine_socket_privilege", "warning", "access to a container engine socket grants broad host privilege", started)
		run("engine", func() error { return s.engine.Check(ctx, *s.config.Engine) })
	}
	return report
}
func (s *Service) checkStorage() error {
	file, err := os.CreateTemp(s.config.DataDir, ".appbackup-check-")
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
	if _, err := s.restic.Version(ctx); err != nil {
		return err
	}
	for _, application := range s.config.EnabledApplications() {
		for _, database := range application.Databases {
			if _, err := s.databases.Version(ctx, database); err != nil {
				return err
			}
		}
	}
	for _, executable := range s.configExecutables() {
		if _, err := exec.LookPath(executable.binary); err != nil {
			return err
		}
	}
	return s.checkStorage()
}

type configuredExecutable struct{ name, binary string }

func (s *Service) configExecutables() []configuredExecutable {
	var result []configuredExecutable
	for _, application := range s.config.EnabledApplications() {
		for _, database := range application.Databases {
			if command := s.config.EffectiveVerificationCommand(database); command != nil {
				result = append(result, configuredExecutable{"verify_command:" + application.ID + ":" + database.ID, command.Binary})
			}
		}
		for _, phase := range application.Hooks.Phases() {
			for index, command := range phase.Commands {
				result = append(result, configuredExecutable{fmt.Sprintf("hook:%s:%s:%d", application.ID, phase.Name, index), command.Binary})
			}
		}
	}
	return result
}
