package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
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
			ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
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
					if !sctx.Fixing || !strings.Contains(sctx.PreviousFindings, "new acceptance condition") || sctx.DeferredFindings != "" {
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
			response := make(chan error, 1)
			go func() {
				response <- executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", head)
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
