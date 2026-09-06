package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/model"
)

func TestRecoveryPointsForVerificationSelectsNewestSuccessfulPointPerApplication(t *testing.T) {
	ctx := context.Background()
	catalog, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	points := []model.RecoveryPoint{
		{ID: "a-old", RunID: "r1", ApplicationID: "a", Status: "succeeded", StartedAt: base, SnapshotID: "s1"},
		{ID: "a-new", RunID: "r2", ApplicationID: "a", Status: "succeeded", StartedAt: base.Add(time.Hour), SnapshotID: "s2"},
		{ID: "a-failed", RunID: "r3", ApplicationID: "a", Status: "failed", StartedAt: base.Add(2 * time.Hour), SnapshotID: "s3"},
		{ID: "b-only", RunID: "r4", ApplicationID: "b", Status: "succeeded", StartedAt: base.Add(3 * time.Hour), SnapshotID: "s4"},
	}
	for _, point := range points {
		if err := catalog.ApplyRecoveryPoint(ctx, point, "recovery-points/"+point.ID+".json"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := catalog.RecoveryPointsForVerification(ctx, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b-only" || got[1].ID != "a-new" {
		t.Fatalf("RecoveryPointsForVerification() = %#v", got)
	}
	got, err = catalog.RecoveryPointsForVerification(ctx, "", "a", false)
	if err != nil || len(got) != 1 || got[0].ID != "a-new" {
		t.Fatalf("RecoveryPointsForVerification(a) = %#v, %v", got, err)
	}
}
