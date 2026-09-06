// Package run defines operation and status values shared by the backup tools.
package run

import "time"

// Operation is a recorded, potentially long-running action. Browse requests
// are read-only and are not run records.
type Operation string

const (
	OperationBackup  Operation = "backup"
	OperationVerify  Operation = "verify"
	OperationRestore Operation = "restore"
	OperationExport  Operation = "export"
)

// Valid reports whether the operation belongs to the common contract.
func (o Operation) Valid() bool {
	switch o {
	case OperationBackup, OperationVerify, OperationRestore, OperationExport:
		return true
	default:
		return false
	}
}

// Status describes the lifecycle of a recorded operation.
type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
	StatusInterrupted Status = "interrupted"
)

// Valid reports whether the status is recognized.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// Terminal reports whether no more work should occur for the run.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// Record contains fields common to persisted and API run representations.
// Each tool stores domain-specific detail separately.
type Record struct {
	ID          string     `json:"id"`
	Operation   Operation  `json:"operation"`
	Status      Status     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}
