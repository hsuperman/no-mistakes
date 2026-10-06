package db

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestReviewedRecoveryStampRefusesChangedTupleOrLane(t *testing.T) {
	for _, kind := range []string{"exact", "head", "review", "status", "push", "newer", "published", "submitted"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/fixture", "/remote", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "submitted", "base")
			if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, "reviewed"); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "reviewed"); err != nil {
				t.Fatal(err)
			}
			expected, _ := d.GetRun(run.ID)
			switch kind {
			case "head":
				d.UpdateRunHeadSHA(run.ID, "other")
			case "review":
				d.UpdateRunReviewApprovedHeadSHA(run.ID, "other")
			case "status":
				d.UpdateRunStatus(run.ID, types.RunRunning)
			case "push":
				d.SetRunPushActive(run.ID, true)
			case "newer":
				d.InsertRun(repo.ID, "feature", "other", "base")
			case "published":
				d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "other", TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"})
			case "submitted":
				expected.SubmittedHeadSHA = nil
			}
			updated, err := d.SetReviewedRunCustodyReturned(expected)
			if err != nil {
				t.Fatal(err)
			}
			if updated != (kind == "exact") {
				t.Fatalf("conditional update=%v", updated)
			}
			actual, _ := d.GetRun(run.ID)
			if (actual.CustodyReturnedAt != nil) != updated {
				t.Fatal("unexpected stamp")
			}
		})
	}
}
