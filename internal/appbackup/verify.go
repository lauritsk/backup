package appbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/safeerror"
)

func (s *Service) verify(ctx context.Context, runID string, request model.VerifyRequest) (model.VerifyReport, error) {
	points, err := s.catalog.AllRecoveryPoints(ctx, request.RecoveryPointID)
	if err != nil {
		return model.VerifyReport{}, err
	}
	report := model.VerifyReport{RecoveryPoints: len(points)}
	var resticErr error
	if len(points) > 0 {
		resticErr = s.restic.Check(ctx)
	}
	for _, summary := range points {
		point, err := s.store.readRecoveryPoint(summary.ID)
		if err != nil {
			return report, err
		}
		record, verifyErr := s.verifyOne(ctx, runID, point, resticErr)
		report.Checked += record.Passed + record.Failed + record.Unknown
		report.Passed += record.Passed
		report.Failed += record.Failed
		report.Unknown += record.Unknown
		remaining := 1000 - len(report.Issues)
		if remaining > len(record.Issues) {
			remaining = len(record.Issues)
		}
		if remaining > 0 {
			report.Issues = append(report.Issues, record.Issues[:remaining]...)
		}
		if remaining < len(record.Issues) {
			report.IssuesTruncated = true
		}
		if verifyErr != nil && errors.Is(verifyErr, context.Canceled) {
			return report, verifyErr
		}
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("verification found %d issue(s)", report.Failed)
	}
	return report, nil
}

func (s *Service) verifyOne(ctx context.Context, runID string, point model.RecoveryPoint, resticErr error) (model.VerificationRecord, error) {
	record := model.VerificationRecord{SchemaVersion: 1, RecoveryPointID: point.ID, VerifiedAt: time.Now().UTC()}
	finish := func(verifyErr error) error {
		return errors.Join(verifyErr, s.finishVerification(ctx, &point, record))
	}
	add := func(component, status string, err error) {
		switch status {
		case "passed":
			record.Passed++
		case "unknown":
			record.Unknown++
		default:
			record.Failed++
		}
		if err != nil || status != "passed" {
			issue := model.VerificationIssue{RecoveryPointID: point.ID, Component: component, Status: status}
			if err != nil {
				issue.Error = safeerror.Clean(err).Error()
			}
			record.Issues = append(record.Issues, issue)
		}
	}
	if point.SnapshotID == "" {
		err := errors.New("recovery point has no Restic snapshot")
		add("restic", "failed", err)
		return record, finish(err)
	}
	if resticErr != nil {
		add("restic", "failed", resticErr)
		return record, finish(resticErr)
	}
	add("restic", "passed", nil)
	if len(point.Dumps) > 0 {
		target, err := s.store.stagingDir(runID, "verify-"+point.ApplicationID)
		if err != nil {
			add("restore", "failed", err)
			return record, finish(err)
		}
		defer os.RemoveAll(filepath.Join(s.config.DataDir, "staging", runID))
		if err := s.restic.Restore(ctx, point.SnapshotID, target); err != nil {
			add("restore", "failed", err)
			return record, finish(err)
		}
		application, found := s.config.Application(point.ApplicationID)
		if !found {
			err := errors.New("application is no longer configured")
			add("configuration", "failed", err)
			return record, finish(err)
		}
		databases := map[string]config.DatabaseConfig{}
		for _, database := range application.Databases {
			databases[database.ID] = database
		}
		for _, dump := range point.Dumps {
			database, found := databases[dump.ID]
			if !found {
				add("database:"+dump.ID, "failed", errors.New("database is no longer configured"))
				continue
			}
			restored := filepath.Join(target, strings.TrimPrefix(filepath.Clean(dump.Path), string(filepath.Separator)))
			size, digest, err := hashFile(ctx, restored)
			if err == nil && (size != dump.Size || digest != dump.SHA256) {
				err = errors.New("restored dump hash does not match manifest")
			}
			if err != nil {
				add("database:"+dump.ID, "failed", err)
				continue
			}
			database.VerifyCommand = s.config.EffectiveVerificationCommand(database)
			verifyCtx, cancel := context.WithTimeout(ctx, database.Timeout.Duration)
			status, err := s.databases.VerifyDump(verifyCtx, database, restored)
			cancel()
			if err != nil {
				status = "failed"
			}
			add("database:"+dump.ID, status, err)
		}
	}
	var verifyErr error
	if record.Failed > 0 {
		verifyErr = fmt.Errorf("recovery point %s has %d verification issue(s)", point.ID, record.Failed)
	}
	return record, finish(verifyErr)
}

func (s *Service) finishVerification(ctx context.Context, point *model.RecoveryPoint, record model.VerificationRecord) error {
	point.Verification = &record
	manifestPath, manifestErr := s.store.writeRecoveryPoint(*point)
	if manifestErr != nil {
		return manifestErr
	}
	return s.catalog.ApplyRecoveryPoint(context.WithoutCancel(ctx), *point, manifestPath)
}
