package steps

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"testing"
)

func TestCIStepLateFindingRevalidatesEvenWhenOrdinaryRepairsPublish(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "late-fix.txt"), []byte("new requirement\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"apply new requirement","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = false
	gitCmd(t, dir, "push", "origin", "feature")
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, sctx, head)
	ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[{"id":"late-1","description":"new requirement","severity":"error","action":"ask-user"}]}`
	if err := sctx.DB.AdmitLateCIFindings(sctx.Run.ID, ci.ID, head, raw); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = raw
	before := gitCmd(t, dir, "rev-parse", "origin/feature")
	outcome, err := (&CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RestartFrom != types.StepReview || len(ag.calls) != 1 {
		t.Fatalf("outcome=%+v calls=%d", outcome, len(ag.calls))
	}
	if sctx.Run.HeadSHA == head {
		t.Fatal("repair did not create amended bytes")
	}
	if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
		t.Fatal("amendment published before revalidation")
	}
	persisted, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ReviewApprovedHeadSHA != nil || persisted.LastPushedSHA == nil || *persisted.LastPushedSHA != head {
		t.Fatal("repair regained old authority or changed publication binding")
	}
}
