// Package model contains Cloud Backup request and response records.
package model

import "time"

type BackupRequest struct {
	Sources []string `json:"sources,omitempty"`
}
type VerifyRequest struct {
	SourceID string `json:"source_id,omitempty"`
	Path     string `json:"path,omitempty"`
}
type RestoreRequest struct {
	SourceID string   `json:"source_id"`
	Paths    []string `json:"paths"`
	Confirm  bool     `json:"confirm"`
}

type File struct {
	SourceID          string     `json:"source_id"`
	Path              string     `json:"path"`
	Size              int64      `json:"size"`
	SHA256            string     `json:"sha256"`
	ModTime           time.Time  `json:"mod_time"`
	LastRunID         string     `json:"last_run_id"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	VerificationError string     `json:"verification_error,omitempty"`
}

type Source struct {
	ID       string `json:"id"`
	Remote   string `json:"remote"`
	Disabled bool   `json:"disabled"`
	Files    int64  `json:"files"`
	Bytes    int64  `json:"bytes"`
}

type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	RunID         string         `json:"run_id"`
	SourceID      string         `json:"source_id"`
	Remote        string         `json:"remote"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   time.Time      `json:"completed_at"`
	Files         []ManifestFile `json:"files"`
}
type ManifestSummary struct {
	RunID       string    `json:"run_id"`
	SourceID    string    `json:"source_id"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Files       int       `json:"files"`
	Bytes       int64     `json:"bytes"`
}

type ManifestFile struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	ModTime time.Time `json:"mod_time"`
	Status  string    `json:"status"`
}

type BackupReport struct {
	Sources []SourceBackupResult `json:"sources"`
	Added   int                  `json:"added"`
	Changed int                  `json:"changed"`
	Skipped int                  `json:"skipped"`
	Files   int                  `json:"files"`
	Bytes   int64                `json:"bytes"`
	Errors  int                  `json:"errors"`
}
type SourceBackupResult struct {
	SourceID string `json:"source_id"`
	Manifest string `json:"manifest,omitempty"`
	Added    int    `json:"added"`
	Changed  int    `json:"changed"`
	Skipped  int    `json:"skipped"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Error    string `json:"error,omitempty"`
}

type VerificationIssue struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	Error    string `json:"error"`
}
type VerifyReport struct {
	Checked         int                 `json:"checked"`
	Passed          int                 `json:"passed"`
	Failed          int                 `json:"failed"`
	Unknown         int                 `json:"unknown"`
	Issues          []VerificationIssue `json:"issues,omitempty"`
	IssuesTruncated bool                `json:"issues_truncated,omitempty"`
}

type RestoreReport struct {
	Directory string         `json:"directory"`
	Restored  int            `json:"restored"`
	Bytes     int64          `json:"bytes"`
	Failed    int            `json:"failed"`
	Files     []RestoredFile `json:"files"`
}
type RestoredFile struct {
	SourceID   string `json:"source_id"`
	Path       string `json:"path"`
	OutputPath string `json:"output_path,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Error      string `json:"error,omitempty"`
}

type CheckReport struct {
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks"`
}
type CheckResult struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Message  string        `json:"message,omitempty"`
	Duration time.Duration `json:"duration"`
}
