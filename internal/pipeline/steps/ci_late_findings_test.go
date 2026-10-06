package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCIStepLateFindingRevalidatesEvenWhenOrdinaryRepairsPublish(t *testing.T) {
	for _, mode := range []string{"repaired", "fix-error", "no-change", "no-code-needed", "closed", "no-code-needed-with-files", "no-code-needed-with-commit", "empty-agent-commit"} {
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
				if mode == "no-code-needed-with-commit" {
					gitCmd(t, opts.CWD, "add", "late-fix.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "declined repair")
					return &agent.Result{Output: json.RawMessage(`{"summary":"declined repair despite commit","code_change_needed":false}`)}, nil
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
			if err == nil || pr != nil {
				t.Fatalf("terminal owned PR selected replacement: %v %v", pr, err)
			}
		}
	}
}

func newLateCIRepairFixture(t *testing.T, ag *mockAgent) *ciRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Agent = ag
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	ci, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.StepResultID = ci.ID
	if err := f.sctx.DB.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[{"id":"late-1","description":"new requirement","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"preserve the operator guidance"},{"id":"deferred-1","description":"second requirement","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"preserve deferred guidance"}]}`
	if err := f.sctx.DB.AdmitLateCIFindings(f.sctx.Run.ID, ci.ID, f.headSHA, raw); err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	selected := types.FindingsMetadata(findings)
	selected.Items = findings.Items[:1]
	deferred := types.FindingsMetadata(findings)
	deferred.Items = findings.Items[1:]
	f.sctx.PreviousFindings, err = types.MarshalFindingsJSON(selected)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.DeferredFindings, err = types.MarshalFindingsJSON(deferred)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.Fixing = true
	return f
}

func assertLateCIRequestParked(t *testing.T, outcome *pipeline.StepOutcome) {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval || outcome.Skipped || outcome.RestartFrom != "" {
		t.Fatalf("unresolved amendment escaped: %+v", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"late-1": "preserve the operator guidance", "deferred-1": "preserve deferred guidance"}
	for _, finding := range findings.Items {
		if guidance, ok := want[finding.ID]; ok {
			if finding.UserInstructions != guidance {
				t.Fatalf("guidance lost for %s: %+v", finding.ID, finding)
			}
			delete(want, finding.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("original amendment missing: %v", want)
	}
}

func TestLateCIRepairRetainedProtectedRetry(t *testing.T) {
	for _, mode := range []string{"retry", "empty-retained", "repeat-refusal", "closed", "merged", "unreadable", "credentials", "host"} {
		t.Run(mode, func(t *testing.T) {
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if mode == "empty-retained" {
					gitCmd(t, opts.CWD, "commit", "--allow-empty", "-m", "empty retained repair")
				} else {
					if err := os.WriteFile(filepath.Join(opts.CWD, "repair.txt"), []byte("material repair"), 0o644); err != nil {
						return nil, err
					}
					gitCmd(t, opts.CWD, "add", "repair.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "retained repair")
				}
				if err := os.WriteFile(filepath.Join(opts.CWD, "blocked.lock"), []byte("blocked edit"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"material repair","code_change_needed":true}`)}, nil
			}}
			f := newLateCIRepairFixture(t, ag)
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			if !pipeline.HasProtectedPathRefusal(outcome.Findings) || f.sctx.Run.HeadSHA == f.headSHA {
				t.Fatalf("repair custody/refusal missing: %+v", outcome)
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.DeferredFindings = ""
			if mode != "repeat-refusal" {
				if err := os.Remove(filepath.Join(f.dir, "blocked.lock")); err != nil {
					t.Fatal(err)
				}
			}
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			switch mode {
			case "closed":
				f.sctx.Env = fakeCIGH(t, "CLOSED", `[]`)
			case "merged":
				f.sctx.Env = fakeCIGH(t, "MERGED", `[]`)
			case "unreadable":
				f.sctx.Env = fakeCIGHStateError(t, "cannot read owned PR", `[]`)
			case "credentials":
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_AUTH_ERR=not logged in")
			case "host":
				f.sctx.Run.PRURL = nil
				f.sctx.Repo.UpstreamURL = "https://unsupported.example/test/repo"
			}
			if mode != "retry" && mode != "empty-retained" && mode != "repeat-refusal" {
				if err := os.WriteFile(filepath.Join(f.dir, "uncommitted.txt"), []byte("must remain uncommitted"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			beforeHead := f.localHead(t)
			beforeStatus := gitCmd(t, f.dir, "status", "--porcelain")
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				t.Fatal("retained late repair resumed monitoring")
				return nil
			}}
			outcome, err = step.Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "retry":
				if outcome.RestartFrom != types.StepReview {
					t.Fatalf("retained material repair skipped Review: %+v", outcome)
				}
			case "closed", "merged":
				if !outcome.SkipRemaining || outcome.RestartFrom != "" {
					t.Fatalf("terminal lifecycle lost: %+v", outcome)
				}
			default:
				assertLateCIRequestParked(t, outcome)
				if mode == "repeat-refusal" {
					if !pipeline.HasProtectedPathRefusal(outcome.Findings) {
						t.Fatal("repeat refusal lost its refusal finding")
					}
					persistCIRefusal(t, f, outcome)
					sr, err := f.sctx.DB.GetStepResult(f.sctx.StepResultID)
					if err != nil || sr.FindingsJSON == nil {
						t.Fatalf("recovery gate missing: %v", err)
					}
					assertLateCIRequestParked(t, &pipeline.StepOutcome{NeedsApproval: true, Findings: *sr.FindingsJSON})
					selected, err := types.ParseFindingsJSON(*sr.FindingsJSON)
					if err != nil {
						t.Fatal(err)
					}
					for i := range selected.Items {
						if selected.Items[i].ID == "late-1" {
							selected.Items[i].UserInstructions = "updated operator guidance"
						}
					}
					f.sctx.PreviousFindings, err = types.MarshalFindingsJSON(selected)
					if err != nil {
						t.Fatal(err)
					}
					updated, err := step.Execute(f.sctx)
					if err != nil || updated == nil || !updated.NeedsApproval {
						t.Fatalf("updated request escaped: %+v, %v", updated, err)
					}
					findings, err := types.ParseFindingsJSON(updated.Findings)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, finding := range findings.Items {
						if finding.ID == "late-1" {
							found = finding.UserInstructions == "updated operator guidance"
						}
					}
					if !found {
						t.Fatal("retry replaced current operator instructions with the stored snapshot")
					}
				}
			}
			if len(ag.calls) != 1 || f.localHead(t) != beforeHead || gitCmd(t, f.dir, "status", "--porcelain") != beforeStatus || f.remoteHead(t) != f.headSHA {
				t.Fatal("retry mutated Git or reran fixer unexpectedly")
			}
			persisted, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || persisted.CIReadyAt != nil || persisted.ReviewApprovedHeadSHA != nil || persisted.LastPushedSHA == nil || *persisted.LastPushedSHA != f.headSHA {
				t.Fatalf("retained repair regained readiness/publication: %+v, %v", persisted, err)
			}
		})
	}
}

func TestLateCIRepairAvailabilityFailureParksRequest(t *testing.T) {
	for _, mode := range []string{"credentials", "host"} {
		t.Run(mode, func(t *testing.T) {
			ag := &mockAgent{name: "test"}
			f := newLateCIRepairFixture(t, ag)
			if mode == "credentials" {
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_AUTH_ERR=not logged in")
			} else {
				f.sctx.Run.PRURL = nil
				f.sctx.Repo.UpstreamURL = "https://unsupported.example/test/repo"
			}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			if len(ag.calls) != 0 || f.localHead(t) != f.headSHA || f.remoteHead(t) != f.headSHA {
				t.Fatal("unavailable repair entry mutated Git or ran fixer")
			}
		})
	}
}

func TestLateCIRepairTimeoutRetainedCommitRevalidates(t *testing.T) {
	for _, negative := range []bool{false, true} {
		t.Run(fmt.Sprint("negative=", negative), func(t *testing.T) {
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					if err := os.WriteFile(filepath.Join(opts.CWD, "repair.txt"), []byte("material repair"), 0o644); err != nil {
						return nil, err
					}
					gitCmd(t, opts.CWD, "add", "repair.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "retained repair")
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &agent.Result{Output: json.RawMessage(fmt.Sprintf(`{"summary":"retained work","code_change_needed":%t}`, !negative))}, nil
			}}
			f := newLateCIRepairFixture(t, ag)
			f.sctx.Config.AgentTimeout = time.Second
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			retained := f.localHead(t)
			if retained == f.headSHA || f.sctx.Run.HeadSHA != retained {
				t.Fatal("timed-out repair was not retained")
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.DeferredFindings = ""
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			if negative {
				assertLateCIRequestParked(t, outcome)
			} else if outcome.RestartFrom != types.StepReview {
				t.Fatalf("retained timeout repair skipped Review: %+v", outcome)
			}
			if calls != 2 || f.localHead(t) != retained || f.remoteHead(t) != f.headSHA {
				t.Fatal("retained timeout repair was changed or published")
			}
		})
	}
}
