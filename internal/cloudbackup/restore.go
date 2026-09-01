package cloudbackup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) restore(ctx context.Context, runID string, request model.RestoreRequest) (model.RestoreReport, error) {
	report := model.RestoreReport{Directory: filepath.Join(s.config.DataDir, "restores", runID)}
	if !request.Confirm {
		return report, errors.New("restore requires explicit confirmation")
	}
	if request.SourceID == "" || len(request.Paths) == 0 {
		return report, errors.New("restore requires source_id and at least one path")
	}
	if len(request.Paths) > 1000 {
		return report, errors.New("restore accepts at most 1000 paths")
	}
	seen := map[string]bool{}
	for _, path := range request.Paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		entry := model.RestoredFile{SourceID: request.SourceID, Path: path}
		file, err := s.catalog.GetFile(ctx, request.SourceID, path)
		if err == nil {
			err = s.store.verify(ctx, file)
		}
		if err == nil {
			entry, err = s.store.restore(ctx, runID, file)
		}
		if err != nil {
			entry.Error = err.Error()
			report.Failed++
		} else {
			report.Restored++
			report.Bytes += entry.Size
		}
		report.Files = append(report.Files, entry)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("restore completed with %d error(s)", report.Failed)
	}
	return report, nil
}
