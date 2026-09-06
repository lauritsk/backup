package appbackup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/appbackup/model"
	"uuid"
)

func (s *Service) backup(ctx context.Context, runID string, request model.BackupRequest) (model.BackupReport, error) {
	applications, err := s.selectedApplications(request.Applications)
	if err != nil {
		return model.BackupReport{}, err
	}
	if len(applications) == 0 {
		return model.BackupReport{}, errors.New("no enabled applications are configured")
	}
	if err := s.restic.EnsureRepository(ctx); err != nil {
		return model.BackupReport{}, err
	}
	resticVersion, err := s.restic.Version(ctx)
	if err != nil {
		return model.BackupReport{}, err
	}
	report := model.BackupReport{}
	for _, application := range applications {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		appCtx, cancel := context.WithTimeout(ctx, application.Timeout.Duration)
		result, pointErr := s.backupApplication(appCtx, runID, application, resticVersion)
		cancel()
		if pointErr != nil {
			result.Error = s.cleanError(pointErr)
			report.Failed++
		} else {
			report.Succeeded++
		}
		report.Applications = append(report.Applications, result)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("backup completed with %d application error(s)", report.Failed)
	}
	return report, nil
}

func (s *Service) backupApplication(ctx context.Context, runID string, application config.ApplicationConfig, resticVersion string) (model.ApplicationBackupResult, error) {
	point := model.RecoveryPoint{SchemaVersion: 1, ID: uuid.NewV7().String(), RunID: runID, ApplicationID: application.ID, Status: "running", StartedAt: time.Now().UTC(), Paths: append([]string(nil), application.Paths...), ToolVersions: map[string]string{"restic": resticVersion}}
	result := model.ApplicationBackupResult{ApplicationID: application.ID, RecoveryPointID: point.ID}
	manifestPath, err := s.store.writeRecoveryPoint(point)
	if err != nil {
		return result, err
	}
	if err := s.catalog.ApplyRecoveryPoint(ctx, point, manifestPath); err != nil {
		return result, err
	}
	staging, err := s.store.stagingDir(point.ID, application.ID)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := s.store.removeStaging(point.ID); err != nil {
			s.logger.Warn("could not remove recovery-point staging directory", "recovery_point_id", point.ID, "error", s.cleanError(err))
		}
	}()
	quiesceStarted := false
	var operationErr error
	if operationErr == nil {
		operationErr = s.runHookPhase(ctx, config.HookPreBackup, application.Hooks.PreBackup, &point)
	}
	if operationErr == nil && len(application.Hooks.Quiesce) > 0 {
		quiesceStarted = true
		operationErr = s.runHookPhase(ctx, config.HookQuiesce, application.Hooks.Quiesce, &point)
	}
	if operationErr == nil {
		for _, database := range application.Databases {
			component := model.ComponentResult{ID: "database:" + database.ID, Type: "database:" + database.Type, Status: "running"}
			dump, err := s.createDump(ctx, staging, database)
			if err != nil {
				component.Status, component.Error = "failed", s.cleanError(err)
				point.Components = append(point.Components, component)
				operationErr = err
				break
			}
			component.Status = "succeeded"
			point.Components = append(point.Components, component)
			point.Dumps = append(point.Dumps, dump)
		}
	}
	if operationErr == nil {
		paths := append([]string(nil), application.Paths...)
		if len(application.Databases) > 0 {
			paths = append(paths, staging)
		}
		component := model.ComponentResult{ID: "restic", Type: "restic", Status: "running"}
		point.SnapshotID, operationErr = s.restic.Backup(ctx, paths, []string{"appbackup", "application:" + application.ID, "recovery-point:" + point.ID})
		result.SnapshotID = point.SnapshotID
		if operationErr != nil {
			component.Status, component.Error = "failed", s.cleanError(operationErr)
		} else {
			component.Status = "succeeded"
		}
		point.Components = append(point.Components, component)
		if operationErr == nil {
			_, operationErr = s.store.writeRecoveryPoint(point)
		}
	}
	if quiesceStarted {
		operationErr = errors.Join(operationErr, s.runHookPhase(context.WithoutCancel(ctx), config.HookUnquiesce, application.Hooks.Unquiesce, &point))
	}
	operationErr = errors.Join(operationErr, s.runHookPhase(context.WithoutCancel(ctx), config.HookPostBackup, application.Hooks.PostBackup, &point))
	point.CompletedAt = nowPointer()
	point.Status = "succeeded"
	if operationErr != nil {
		point.Status = "failed"
		point.Error = s.cleanError(operationErr)
	}
	if _, err := s.store.writeRecoveryPoint(point); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	if err := s.catalog.ApplyRecoveryPoint(context.WithoutCancel(ctx), point, manifestPath); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	result.Dumps = len(point.Dumps)
	if operationErr == nil && application.VerifyAfterBackup {
		verification, verifyErr := s.verifyOne(ctx, runID, point, nil)
		point.Verification = &verification
		if verifyErr != nil {
			operationErr = verifyErr
			point.Status = "failed"
			point.Error = s.cleanError(verifyErr)
			_, _ = s.store.writeRecoveryPoint(point)
			_ = s.catalog.ApplyRecoveryPoint(context.WithoutCancel(ctx), point, manifestPath)
		}
	}
	return result, operationErr
}

func (s *Service) createDump(ctx context.Context, staging string, database config.DatabaseConfig) (model.DatabaseDump, error) {
	if err := s.store.prepareDumpDir(staging); err != nil {
		return model.DatabaseDump{}, err
	}
	extension := ".dump"
	if database.Type == "mysql" || database.Type == "mariadb" {
		extension = ".sql"
	}
	if database.Type == "sqlite" {
		extension = ".sqlite"
	}
	path := filepath.Join(staging, "dumps", database.ID+extension)
	version, err := s.databases.Version(ctx, database)
	if err != nil {
		return model.DatabaseDump{}, err
	}
	dumpCtx, cancel := context.WithTimeout(ctx, database.Timeout.Duration)
	defer cancel()
	if err := s.databases.Dump(dumpCtx, database, path); err != nil {
		return model.DatabaseDump{}, err
	}
	size, digest, err := hashFile(ctx, path)
	if err != nil {
		return model.DatabaseDump{}, err
	}
	return model.DatabaseDump{ID: database.ID, Type: database.Type, Path: path, Size: size, SHA256: digest, Version: version}, nil
}

func (s *Service) runHookPhase(ctx context.Context, phase string, commands []config.CommandConfig, point *model.RecoveryPoint) error {
	for index, command := range commands {
		if hookSucceeded(point.Hooks, phase, index) {
			continue
		}
		result := model.HookResult{Phase: phase, Index: index, Status: "running", StartedAt: time.Now().UTC()}
		point.Hooks = append(point.Hooks, result)
		if _, err := s.store.writeRecoveryPoint(*point); err != nil {
			return err
		}
		err := s.hooks.Run(ctx, command)
		result.FinishedAt = nowPointer()
		result.Status = "succeeded"
		if err != nil {
			result.Status = "failed"
			result.Error = s.cleanError(err)
		}
		point.Hooks[len(point.Hooks)-1] = result
		_, writeErr := s.store.writeRecoveryPoint(*point)
		if err := errors.Join(err, writeErr); err != nil {
			return err
		}
	}
	return nil
}

func hookSucceeded(results []model.HookResult, phase string, index int) bool {
	for _, result := range results {
		if result.Phase == phase && result.Index == index && result.Status == "succeeded" {
			return true
		}
	}
	return false
}

func phaseStarted(results []model.HookResult, phase string) bool {
	for _, result := range results {
		if result.Phase == phase {
			return true
		}
	}
	return false
}

func (s *Service) selectedApplications(ids []string) ([]config.ApplicationConfig, error) {
	if len(ids) == 0 {
		return s.config.EnabledApplications(), nil
	}
	seen := map[string]bool{}
	var result []config.ApplicationConfig
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		application, found := s.config.Application(id)
		if !found {
			return nil, fmt.Errorf("application %q is not configured", id)
		}
		if application.Disabled {
			return nil, fmt.Errorf("application %q is disabled", id)
		}
		result = append(result, application)
	}
	return result, nil
}
