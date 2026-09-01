package pimbackup

import (
	"context"
	"errors"
	"fmt"

	"github.com/lauritsk/backup/internal/pimbackup/model"
)

type issueRecorder struct{ report *model.VerifyReport }

func (recorder issueRecorder) add(issue model.VerificationIssue) {
	recorder.report.Failed++
	if len(recorder.report.Issues) < 1000 {
		recorder.report.Issues = append(recorder.report.Issues, issue)
	} else {
		recorder.report.IssuesTruncated = true
	}
}

func (s *Service) verify(ctx context.Context, request model.VerifyRequest) (model.VerifyReport, error) {
	report := model.VerifyReport{}
	if request.MessageID != 0 && request.ObjectID != 0 {
		return report, errors.New("verify accepts either message_id or object_id, not both")
	}
	recorder := issueRecorder{report: &report}
	if err := s.catalog.QuickCheck(ctx); err != nil {
		recorder.add(model.VerificationIssue{Path: "pim.db", Error: err.Error()})
	}
	if request.ObjectID == 0 {
		if err := s.verifyMessages(ctx, request, &report, recorder); err != nil {
			return report, err
		}
	}
	if request.MessageID == 0 {
		if err := s.verifyObjects(ctx, request, &report, recorder); err != nil {
			return report, err
		}
	}
	if request.MessageID == 0 && request.ObjectID == 0 && request.AccountID == "" {
		scan := s.store.Scan(ctx)
		if err := ctx.Err(); err != nil {
			return report, err
		}
		for _, scanErr := range scan.Errors {
			recorder.add(model.VerificationIssue{Error: scanErr.Error()})
		}
		for _, scanErr := range s.objectStore.Scan(ctx).Errors {
			recorder.add(model.VerificationIssue{Error: scanErr.Error()})
		}
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("verification found %d issue(s)", report.Failed)
	}
	return report, nil
}

func (s *Service) verifyMessages(ctx context.Context, request model.VerifyRequest, report *model.VerifyReport, recorder issueRecorder) error {
	var messages []model.Message
	if request.MessageID != 0 {
		message, err := s.catalog.GetMessage(ctx, request.MessageID)
		if err != nil {
			return err
		}
		if request.AccountID != "" && request.AccountID != message.AccountID {
			return fmt.Errorf("message %d does not belong to account %q", request.MessageID, request.AccountID)
		}
		messages = []model.Message{message}
	} else {
		var err error
		messages, err = s.catalog.AllMessages(ctx, request.AccountID)
		if err != nil {
			return err
		}
	}
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		report.Checked++
		verifyErr := s.store.Verify(ctx, message)
		if err := s.catalog.UpdateVerification(ctx, message.ID, verifyErr); err != nil {
			return err
		}
		if verifyErr != nil {
			recorder.add(model.VerificationIssue{MessageID: message.ID, Path: message.Path, Error: verifyErr.Error()})
		} else {
			report.Passed++
		}
	}
	return nil
}

func (s *Service) verifyObjects(ctx context.Context, request model.VerifyRequest, report *model.VerifyReport, recorder issueRecorder) error {
	var objects []model.Object
	if request.ObjectID != 0 {
		object, err := s.catalog.GetObject(ctx, request.ObjectID)
		if err != nil {
			return err
		}
		if request.AccountID != "" && request.AccountID != object.AccountID {
			return fmt.Errorf("object %d does not belong to account %q", request.ObjectID, request.AccountID)
		}
		objects = []model.Object{object}
	} else {
		var err error
		objects, err = s.catalog.AllObjects(ctx, request.AccountID)
		if err != nil {
			return err
		}
	}
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		report.Checked++
		verifyErr := s.objectStore.Verify(ctx, object)
		if err := s.catalog.UpdateObjectVerification(ctx, object.ID, verifyErr); err != nil {
			return err
		}
		if verifyErr != nil {
			recorder.add(model.VerificationIssue{ObjectID: object.ID, Path: object.Path, Error: verifyErr.Error()})
		} else {
			report.Passed++
		}
	}
	return nil
}
