package steps

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
 "github.com/kunchenguid/no-mistakes/internal/scm"
 "github.com/kunchenguid/no-mistakes/internal/pipeline"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIStepLateFindingRevalidatesEvenWhenOrdinaryRepairsPublish(t *testing.T) {
	for _, mode := range []string{"repaired", "fix-error", "no-change", "no-code-needed", "closed", "no-code-needed-with-files", "empty-agent-commit"} {
		t.Run(mode, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if mode == "fix-error" {
					return nil, errors.New("controlled repair failure")
				}
				if mode == "no-change" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"claimed fix without changes","code_change_needed":true}`)}, nil
				}
				if mode == "no-code-needed" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"no code needed","code_change_needed":false}`)}, nil
				}

				if mode == "empty-agent-commit" {
					gitCmd(t, opts.CWD, "commit", "--allow-empty", "-m", "empty repair")
					return &agent.Result{Output: json.RawMessage(`{"summary":"empty repair","code_change_needed":true}`)}, nil
				}
				if err := os.WriteFile(filepath.Join(opts.CWD, "late-fix.txt"), []byte("new requirement\n"), 0o644); err != nil {
					return nil, err
				}
				if mode == "no-code-needed-with-files" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"declined repair despite edits","code_change_needed":false}`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"apply new requirement","code_change_needed":true}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.CI.RevalidateRepairs = false
			gitCmd(t, dir, "push", "origin", "feature")
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			state := "OPEN"
			if mode == "closed" {
				state = "CLOSED"
			}
			sctx.Env = fakeCIGH(t, state, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
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
			raw := `{"findings":[{"id":"late-1","description":"new requirement","severity":"error","action":"ask-user","category":"ci-late-finding"}]}`
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
			if mode == "closed" {
				if len(ag.calls) != 0 || !outcome.SkipRemaining || outcome.RestartFrom != "" {
					t.Fatalf("closed PR entered fixer: outcome=%+v calls=%d", outcome, len(ag.calls))
				}
				persisted, _ := sctx.DB.GetRun(sctx.Run.ID)
				if persisted.PRState == nil || *persisted.PRState != "closed" || persisted.Status != types.RunCompleted {
					t.Fatalf("terminal lifecycle lost: %+v", persisted)
				}
				return
			}
			if mode != "repaired" {
				if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "late-1") || !strings.Contains(outcome.Findings, "ci-late-finding") || outcome.RestartFrom != "" || len(ag.calls) != 1 {
					t.Fatalf("unresolved amendment escaped: outcome=%+v calls=%d", outcome, len(ag.calls))
				}
				persisted, _ := sctx.DB.GetRun(sctx.Run.ID)
				if persisted.CIReadyAt != nil || persisted.ReviewApprovedHeadSHA != nil || persisted.HeadSHA != head {
					t.Fatal("unresolved amendment regained readiness or changed head")
				}
				if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
					t.Fatal("unresolved amendment published")
				}
				return
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

			for _, terminal := range []string{"CLOSED", "MERGED"} {
				recordReviewApproval(t, sctx, sctx.Run.HeadSHA)
				sctx.Fixing = false
				sctx.Env = fakeCIGH(t, terminal, `[]`)
				if _, err := (&PushStep{}).Execute(sctx); err == nil {
					t.Fatalf("published amended head after PR became %s", terminal)
				}
				if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
					t.Fatal("terminal PR allowed publication")
				}
			}
		})
	}
}

func TestLateCIAdmissionRequiresLiveOpenOwnedPR(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MERGED", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Env = fakeCIGH(t, state, `[]`)
			if err := (&CIStep{}).VerifyLateCIAdmission(sctx); (err == nil) != (state == "OPEN") {
				t.Fatalf("state=%s admission=%v", state, err)
			}
		})
	}
}

func TestTerminalOwnedPRNeverBindsReplacement(t *testing.T) {
	for _, state := range []scm.PRState{scm.PRStateClosed, scm.PRStateMerged} {
		for _, discovered := range []*scm.PR{nil, {Number: "99", URL: "https://github.com/test/repo/pull/99"}, {Number: "42", URL: "https://github.com/test/repo/pull/42"}} {
			owned := "https://github.com/test/repo/pull/42"
			sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned}}
			pr, err := bindExistingPR(sctx, &recordingRetargetHost{state: state}, discovered)
			if err == nil || pr != nil { t.Fatalf("terminal owned PR selected replacement: %v %v", pr, err) }
		}
	}
}
