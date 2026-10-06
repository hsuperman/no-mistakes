package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorLateCIFindingJoinsMonitorAndRevalidatesSameRun(t *testing.T) {
	for _, terminal := range []string{"", "merged", "closed"} {
		t.Run("terminal="+terminal, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			dir := t.TempDir()
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			gitCmd := func(args ...string) string {
				t.Helper()
				command := exec.Command(realGit, args...)
				command.Dir = dir
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s: %v", args, output, err)
				}
				return strings.TrimSpace(string(output))
			}
			gitCmd("init", "-b", "feature")
			gitCmd("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
			head := gitCmd("rev-parse", "HEAD")
			run.HeadSHA = head
			if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var order []types.StepName
			ciCalls := 0
			pass := func(name types.StepName) Step {
				return &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
					order = append(order, name)
					if name == types.StepReview {
						return &StepOutcome{ReviewApprovedHeadSHA: head}, nil
					}
					return &StepOutcome{}, nil
				}}
			}
			ciStep := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
				ciCalls++
				if ciCalls == 1 {
					if err := database.SetRunCIReady(run.ID, true); err != nil {
						return nil, err
					}
					sctx.CIReadinessChanged(true, false)
					close(started)
					<-sctx.Ctx.Done()
					close(cancelled)
					<-release
					if terminal != "" {
						if err := database.UpdateRunPRState(run.ID, terminal); err != nil {
							return nil, err
						}
					}
					// Simulate a provider poll completing after cancellation/admission.
					if err := database.SetRunCIReady(run.ID, true); err != nil {
						return nil, err
					}
					sctx.CIReadinessChanged(true, false)
					return nil, sctx.Ctx.Err()
				}
				if ciCalls == 2 {
					if !sctx.Fixing || (!strings.Contains(sctx.PreviousFindings, "new acceptance condition") || !strings.Contains(sctx.PreviousFindings, "preserve parser behavior")) || sctx.DeferredFindings != "" {
						return nil, fmt.Errorf("repair lost selected finding: fixing=%v selected=%s deferred=%s", sctx.Fixing, sctx.PreviousFindings, sctx.DeferredFindings)
					}
					persisted, err := database.GetRun(run.ID)
					if err != nil {
						return nil, err
					}
					if persisted.ReviewApprovedHeadSHA != nil {
						return nil, fmt.Errorf("old review authority survived admission")
					}
					return &StepOutcome{RestartFrom: types.StepReview}, nil
				}
				return &StepOutcome{}, nil
			}}
			ci := &lateAdmissionTestStep{adaptiveCallStep: ciStep}
			cfg := &config.Config{}
			executor := NewExecutor(database, p, cfg, nil, []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), pass(types.StepPush), pass(types.StepPR), ci}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- executor.Execute(ctx, run, repo, dir) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("monitor did not start")
			}
			before, _ := database.GetRun(run.ID)
			finding := []types.Finding{{ID: "late-1", Description: "new acceptance condition", Action: types.ActionAutoFix, Severity: types.FindingSeverityError}}
			if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", "stale"); err == nil {
				t.Fatal("stale head admitted")
			}
			for _, state := range []string{"closed", "merged", "unavailable"} {
				ci.refusal = state
				if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", head); err == nil {
					t.Fatalf("%s PR admitted", state)
				}
				after, _ := database.GetRun(run.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("refused admission mutated run")
				}
			}
			ci.refusal = ""
			response := make(chan error, 1)
			instructions := map[string]string{"late-1": "preserve parser behavior"}
			if terminal == "" {
				finding[0].UserInstructions = "preserve parser behavior"
				instructions = nil
			}
			go func() {
				response <- executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, instructions, finding, "", head)
			}()
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("monitor not cancelled")
			}
			during, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRecoveredRun(database, during, executor.steps); err != nil {
				t.Fatalf("admitted gate cannot survive daemon restart: %v", err)
			}
			results, _ := database.GetStepsByRun(run.ID)
			for _, result := range results {
				if result.StepName != types.StepCI {
					continue
				}
				rounds, _ := database.GetRoundsByStep(result.ID)
				var retained types.Findings
				if len(rounds) == 0 || rounds[len(rounds)-1].FindingsJSON == nil {
					t.Fatal("missing durable finding")
				}
				if err := json.Unmarshal([]byte(*rounds[len(rounds)-1].FindingsJSON), &retained); err != nil {
					t.Fatal(err)
				}
				if len(retained.Items) != 1 || retained.Items[0].ID != "late-1" || retained.Items[0].UserInstructions != "preserve parser behavior" {
					t.Fatalf("instructions lost: %+v", retained)
				}
			}
			if during.CIReadyAt != nil || during.ReviewApprovedHeadSHA != nil {
				t.Fatal("admission retained readiness/approval")
			}
			if during.LastPushedSHA == nil || *during.LastPushedSHA != head || *during.PushGeneration != *before.PushGeneration || *during.PRURL != *before.PRURL {
				t.Fatalf("admission changed publication custody: before=%+v during=%+v", before, during)
			}
			if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", head); err == nil {
				t.Fatal("concurrent finding admitted")
			}
			select {
			case err := <-response:
				t.Fatalf("acknowledged before monitor joined: %v", err)
			default:
			}
			close(release)
			select {
			case err := <-response:
				if (err != nil) != (terminal != "") {
					t.Fatalf("terminal=%q response=%v", terminal, err)
				}
			case <-ctx.Done():
				t.Fatal("handoff never acknowledged")
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("run did not finish")
			}
			if terminal != "" {
				if ciCalls != 1 {
					t.Fatalf("terminal PR launched a repair: %d CI calls", ciCalls)
				}
				terminalRun, _ := database.GetRun(run.ID)
				if terminalRun.Status != types.RunCompleted || terminalRun.PRState == nil || *terminalRun.PRState != terminal {
					t.Fatalf("terminal state lost: %+v", terminalRun)
				}
				results, _ := database.GetStepsByRun(run.ID)
				for _, result := range results {
					if result.StepName == types.StepCI && result.Status != types.StepStatusCompleted {
						t.Fatalf("terminal CI resurrected: %+v", result)
					}
				}
				return
			}
			if ciCalls != 3 {
				t.Fatalf("CI calls=%d", ciCalls)
			}
			want := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR}
			if fmt.Sprint(order) != fmt.Sprint(append(want, want...)) {
				t.Fatalf("validation order=%v", order)
			}
			after, _ := database.GetRun(run.ID)
			if after.CIReadyAt != nil {
				t.Fatal("cancelled monitor restored readiness")
			}

		})
	}
}

func TestExecutorLateCIFindingRecoveredGateRequiresExplicitFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	names := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	steps := make([]Step, 0, len(names))
	ciCalls := 0
	var ciID string
	for _, name := range names {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(result.ID); err != nil {
			t.Fatal(err)
		}
		if name != types.StepCI {
			if err := database.CompleteStep(result.ID, 0, 0, "historical.log"); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, newPassStep(name))
			continue
		}
		ciID = result.ID
		steps = append(steps, &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
			ciCalls++
			if ciCalls == 1 {
				if !sctx.Fixing || !strings.Contains(sctx.PreviousFindings, "retained acceptance") || !strings.Contains(sctx.PreviousFindings, "retained guidance") {
					return nil, fmt.Errorf("recovery lost amendment: fixing=%v selected=%s", sctx.Fixing, sctx.PreviousFindings)
				}
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}})
	}
	raw := `{"findings":[{"id":"late-1","description":"retained acceptance","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"retained guidance"}]}`
	if err := database.AdmitLateCIFindings(run.ID, ciID, run.HeadSHA, raw); err != nil {
		t.Fatal(err)
	}
	retained, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked := make(chan struct{}, 1)
	executor := NewExecutor(database, p, &config.Config{}, nil, steps, func(event ipc.Event) {
		if event.StepName != nil && *event.StepName == types.StepCI && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parked <- struct{}{}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- executor.Resume(ctx, retained, repo, t.TempDir()) }()
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("recovered amendment not parked")
	}
	if ciCalls != 0 {
		t.Fatal("recovery automatically evaluated the retained amendment")
	}
	if err := executor.Respond(types.StepCI, types.ActionFix, []string{"late-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("recovery failed to resume")
	}
	if ciCalls != 2 {
		t.Fatalf("CI calls=%d, want fix then post-validation monitor", ciCalls)
	}
}

type lateAdmissionTestStep struct {
	*adaptiveCallStep
	refusal string
}

func (s *lateAdmissionTestStep) VerifyLateCIAdmission(*StepContext) error {
	if s.refusal != "" {
		return fmt.Errorf("owned PR: %s", s.refusal)
	}
	return nil
}
