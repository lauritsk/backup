// Package model contains Application Backup records and operation reports.
package model

import "time"

type BackupRequest struct {
	Applications []string `json:"applications,omitempty"`
}

type VerifyRequest struct {
	RecoveryPointID string `json:"recovery_point_id,omitempty"`
	ApplicationID   string `json:"application_id,omitempty"`
	All             bool   `json:"all,omitempty"`
}

type ExportRequest struct {
	RecoveryPointID string `json:"recovery_point_id"`
	Confirm         bool   `json:"confirm"`
}

type Application struct {
	ID             string     `json:"id"`
	Disabled       bool       `json:"disabled"`
	RecoveryPoints int64      `json:"recovery_points"`
	LastBackupAt   *time.Time `json:"last_backup_at,omitempty"`
}

type RecoveryPoint struct {
	SchemaVersion int                 `json:"schema_version"`
	ID            string              `json:"id"`
	RunID         string              `json:"run_id"`
	ApplicationID string              `json:"application_id"`
	Status        string              `json:"status"`
	StartedAt     time.Time           `json:"started_at"`
	CompletedAt   *time.Time          `json:"completed_at,omitempty"`
	SnapshotID    string              `json:"snapshot_id,omitempty"`
	Paths         []string            `json:"paths,omitempty"`
	Components    []ComponentResult   `json:"components,omitempty"`
	Dumps         []DatabaseDump      `json:"dumps,omitempty"`
	Hooks         []HookResult        `json:"hooks,omitempty"`
	ToolVersions  map[string]string   `json:"tool_versions,omitempty"`
	Verification  *VerificationRecord `json:"verification,omitempty"`
	Error         string              `json:"error,omitempty"`
}

type RecoveryPointSummary struct {
	ID                 string     `json:"id"`
	ApplicationID      string     `json:"application_id"`
	Status             string     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	SnapshotID         string     `json:"snapshot_id,omitempty"`
	Dumps              int        `json:"dumps"`
	VerificationStatus string     `json:"verification_status,omitempty"`
}

type ComponentResult struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type DatabaseDump struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
}

type HookResult struct {
	Phase      string     `json:"phase"`
	Index      int        `json:"index"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type BackupReport struct {
	Applications []ApplicationBackupResult `json:"applications"`
	Succeeded    int                       `json:"succeeded"`
	Failed       int                       `json:"failed"`
}

type ApplicationBackupResult struct {
	ApplicationID   string `json:"application_id"`
	RecoveryPointID string `json:"recovery_point_id,omitempty"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	Dumps           int    `json:"dumps"`
	Error           string `json:"error,omitempty"`
}

type VerifyReport struct {
	RecoveryPoints  int                 `json:"recovery_points"`
	Checked         int                 `json:"checked"`
	Passed          int                 `json:"passed"`
	Failed          int                 `json:"failed"`
	Unknown         int                 `json:"unknown"`
	Issues          []VerificationIssue `json:"issues,omitempty"`
	IssuesTruncated bool                `json:"issues_truncated,omitempty"`
}

type VerificationIssue struct {
	RecoveryPointID string `json:"recovery_point_id"`
	Component       string `json:"component"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}

type VerificationRecord struct {
	SchemaVersion   int                 `json:"schema_version"`
	RecoveryPointID string              `json:"recovery_point_id"`
	VerifiedAt      time.Time           `json:"verified_at"`
	Passed          int                 `json:"passed"`
	Failed          int                 `json:"failed"`
	Unknown         int                 `json:"unknown"`
	Issues          []VerificationIssue `json:"issues,omitempty"`
}

type ExportReport struct {
	RecoveryPointID string `json:"recovery_point_id"`
	Directory       string `json:"directory"`
	SnapshotID      string `json:"snapshot_id"`
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
