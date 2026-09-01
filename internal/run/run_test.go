package run

import "testing"

func TestOperations(t *testing.T) {
	for _, operation := range []Operation{OperationBackup, OperationVerify, OperationRestore} {
		if !operation.Valid() {
			t.Errorf("operation %q is not valid", operation)
		}
	}
	if Operation("browse").Valid() {
		t.Error("browse should not be recorded as a run")
	}
}

func TestTerminalStatuses(t *testing.T) {
	tests := map[Status]bool{
		StatusQueued:      false,
		StatusRunning:     false,
		StatusSucceeded:   true,
		StatusFailed:      true,
		StatusCanceled:    true,
		StatusInterrupted: true,
	}

	for status, want := range tests {
		if got := status.Terminal(); got != want {
			t.Errorf("Status(%q).Terminal() = %t, want %t", status, got, want)
		}
		if !status.Valid() {
			t.Errorf("status %q is not valid", status)
		}
	}
}
