package cloudbackup

import (
	"context"
	"errors"
	"fmt"

	"github.com/lauritsk/backup/internal/cloudbackup/catalog"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) verify(ctx context.Context, request model.VerifyRequest) (model.VerifyReport, error) {
	report := model.VerifyReport{}
	if request.Path != "" && request.SourceID == "" {
		return report, errors.New("verify path requires source_id")
	}
	record := func(file model.File) error {
		if request.Path != "" && file.Path != request.Path {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		report.Checked++
		verifyErr := s.store.verify(ctx, file)
		if err := s.catalog.UpdateVerification(ctx, file.SourceID, file.Path, verifyErr); err != nil {
			return err
		}
		if verifyErr != nil {
			report.Failed++
			if len(report.Issues) < 1000 {
				report.Issues = append(report.Issues, model.VerificationIssue{SourceID: file.SourceID, Path: file.Path, Error: verifyErr.Error()})
			} else {
				report.IssuesTruncated = true
			}
		} else {
			report.Passed++
		}
		return nil
	}
	if err := s.catalog.ForEachFile(ctx, request.SourceID, record); err != nil {
		return report, err
	}
	if request.Path != "" && report.Checked == 0 {
		return report, catalog.ErrNotFound
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("verification found %d issue(s)", report.Failed)
	}
	return report, nil
}
