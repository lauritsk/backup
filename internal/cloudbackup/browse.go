package cloudbackup

import (
	"context"
	"os"

	"github.com/lauritsk/backup/internal/cloudbackup/catalog"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) ListSources(ctx context.Context) ([]model.Source, error) {
	sources, err := s.catalog.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	disabled := map[string]bool{}
	for _, source := range s.config.Sources {
		disabled[source.ID] = source.Disabled
	}
	for i := range sources {
		sources[i].Disabled = disabled[sources[i].ID]
	}
	return sources, nil
}

func (s *Service) ListFiles(ctx context.Context, source, prefix string, limit, offset int) ([]model.File, error) {
	return s.catalog.ListFiles(ctx, source, prefix, limit, offset)
}

func (s *Service) ListManifests(ctx context.Context, source string, limit, offset int) ([]model.ManifestSummary, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	manifests, err := s.store.readManifests(ctx)
	if err != nil {
		return nil, err
	}
	var summaries []model.ManifestSummary
	for index := len(manifests) - 1; index >= 0; index-- {
		manifest := manifests[index]
		if source != "" && source != manifest.SourceID {
			continue
		}
		var bytes int64
		for _, file := range manifest.Files {
			bytes += file.Size
		}
		summaries = append(summaries, model.ManifestSummary{RunID: manifest.RunID, SourceID: manifest.SourceID, StartedAt: manifest.StartedAt, CompletedAt: manifest.CompletedAt, Files: len(manifest.Files), Bytes: bytes})
	}
	if offset >= len(summaries) {
		return []model.ManifestSummary{}, nil
	}
	end := offset + limit
	if end > len(summaries) {
		end = len(summaries)
	}
	return summaries[offset:end], nil
}

func (s *Service) GetManifest(ctx context.Context, source, runID string) (model.Manifest, error) {
	manifests, err := s.store.readManifests(ctx)
	if err != nil {
		return model.Manifest{}, err
	}
	for _, manifest := range manifests {
		if manifest.SourceID == source && manifest.RunID == runID {
			return manifest, nil
		}
	}
	return model.Manifest{}, catalog.ErrNotFound
}

func (s *Service) GetFile(ctx context.Context, source, path string) (model.File, error) {
	return s.catalog.GetFile(ctx, source, path)
}

func (s *Service) OpenFile(ctx context.Context, source, path string) (model.File, *os.File, error) {
	file, err := s.catalog.GetFile(ctx, source, path)
	if err != nil {
		return file, nil, err
	}
	opened, err := s.store.open(source, path)
	return file, opened, err
}

func (s *Service) ListRuns(ctx context.Context, limit, offset int) ([]catalog.Run, error) {
	return s.catalog.ListRuns(ctx, limit, offset)
}

func (s *Service) GetRun(ctx context.Context, id string) (catalog.Run, error) {
	return s.catalog.GetRun(ctx, id)
}
