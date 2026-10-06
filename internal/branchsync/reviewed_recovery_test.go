package branchsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// S is submitted, P is published, R is a reviewed rebase whose deliberate fix
// changes S/P content. Ordinary content containment must continue to refuse.
func newReviewedRecoveryFixture(t *testing.T) (*recoverFixture, ReviewedRecoveryRequest) {
	t.Helper()
	f := newRecoverFixture(t, types.RunFailed)
	published := f.preserved
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustRun(t, pipeline, "checkout", "--orphan", "reviewed-rebase")
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "reviewed replacement for feature\n")
	mustRun(t, pipeline, "commit", "-am", "reviewed rewrite")
	f.preserved = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	if err := custody.PreserveRecoveryHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	return f, ReviewedRecoveryRequest{RunID: f.run.ID, ExpectedLocalHead: f.submitted, ReviewedHead: f.preserved}
}

func TestReviewedRecoveryAdoptsAlteredReviewedRewriteOnlyWithExactConsent(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	ordinary := f.service.Recover(f.ctx, false)
	if ordinary.Recovered || ordinary.Safety != "blocked_recover_diverged" {
		t.Fatalf("ordinary recovery widened: %#v", ordinary)
	}
	beforeRefs := mustRun(t, f.local, "show-ref")
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Diff, "reviewed replacement for feature") || plan.PublishedHead == "" || plan.Digest == "" {
		t.Fatalf("incomplete preview: %#v", plan)
	}
	if refs := mustRun(t, f.local, "show-ref"); refs != beforeRefs {
		t.Fatal("preview mutated refs")
	}
	if got := f.service.AdoptReviewedRecovery(f.ctx, request, ""); got.Recovered || f.custodyReturned() {
		t.Fatal("implicit consent")
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if !got.Recovered || !got.Changed || !f.custodyReturned() {
		t.Fatalf("adoption: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.preserved {
		t.Fatal("wrong materialized head")
	}
	content, err := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if err != nil || string(content) != "reviewed replacement for feature\n" {
		t.Fatalf("content: %q %v", content, err)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost original object/ref: %#v", binding)
		}
	}
}

func TestReviewedRecoveryAdoptsFromExactPublishedCaller(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	request.ExpectedLocalHead = *f.run.LastPushedSHA
	mustRun(t, f.local, "fetch", f.gate, request.ExpectedLocalHead)
	mustRun(t, f.local, "merge", "--ff-only", request.ExpectedLocalHead)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if !got.Recovered || mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead {
		t.Fatalf("published caller adoption: %#v", got)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost published/submitted object: %#v", binding)
		}
	}
}

func TestReviewedRecoveryRefusesStaleOrUnauthorizedPlansWithoutMovingCaller(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"wrong run", "wrong local", "wrong review", "wrong repository", "wrong branch", "dirty", "untracked", "missing anchor", "symbolic anchor", "unreviewed", "active", "newer terminal", "active race", "caller race", "review race", "anchor race", "gate race", "wrong digest"} {
		t.Run(name, func(t *testing.T) {
			f, request := newReviewedRecoveryFixture(t)
			plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			consent := plan.Digest
			switch name {
			case "wrong run":
				request.RunID = "wrong"
			case "wrong local":
				request.ExpectedLocalHead = f.base
			case "wrong review":
				request.ReviewedHead = f.submitted
			case "wrong repository":
				repo, err := f.db.InsertRepo(filepath.Join(filepath.Dir(f.local), "foreign"), f.remote, "main")
				if err != nil {
					t.Fatal(err)
				}
				f.service.Repo = repo
			case "wrong branch":
				mustRun(t, f.local, "checkout", "-b", "foreign")
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "file.txt"), "private dirty edit\n")
			case "untracked":
				mustWrite(t, filepath.Join(f.local, "untracked.txt"), "private untracked\n")
			case "missing anchor":
				mustRun(t, f.gate, "update-ref", "-d", f.anchorRef())
			case "symbolic anchor":
				mustRun(t, f.gate, "symbolic-ref", f.anchorRef(), "refs/heads/feature/recover")
			case "unreviewed":
				if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.submitted); err != nil {
					t.Fatal(err)
				}
			case "active", "newer terminal":
				other, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if name == "newer terminal" {
					if err := f.db.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
						t.Fatal(err)
					}
				}
			case "caller race":
				f.service.beforeRecoverBranchMove = func() {
					mustWrite(t, filepath.Join(f.local, "file.txt"), "raced committed edit\n")
					mustRun(t, f.local, "commit", "-am", "caller race")
				}
			case "active race":
				f.service.beforeRecoverBranchMove = func() {
					if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base); err != nil {
						t.Fatal(err)
					}
				}
			case "review race":
				f.service.beforeRecoverBranchMove = func() {
					if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.submitted); err != nil {
						t.Fatal(err)
					}
				}
			case "anchor race":
				f.service.beforeRecoverBranchMove = func() { mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted) }
			case "gate race":
				f.service.beforeRecoverBranchMove = func() { mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted) }
			case "wrong digest":
				consent = strings.Repeat("0", 64)
			}
			before := mustRun(t, f.local, "rev-parse", "HEAD")
			got := f.service.AdoptReviewedRecovery(f.ctx, request, consent)
			if got.Recovered || f.custodyReturned() {
				t.Fatalf("refusal reported success: %#v", got)
			}
			after := mustRun(t, f.local, "rev-parse", "HEAD")
			if name != "caller race" && after != before {
				t.Fatalf("refusal moved caller: %s→%s", before, after)
			}
			if name == "caller race" && after == f.preserved {
				t.Fatal("overwrote raced commit")
			}
		})
	}
}

func TestReviewedRecoveryMaterializationFailurePreservesConcurrentEditsAndCustody(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.service.afterRecoverBranchMove = func() { mustWrite(t, filepath.Join(f.local, "file.txt"), "late private edit\n") }
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || f.custodyReturned() || got.Safety != "blocked_recover_worktree_busy" {
		t.Fatalf("partial outcome: %#v", got)
	}
	content, _ := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if string(content) != "late private edit\n" {
		t.Fatal("destroyed concurrent edit")
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != request.ExpectedLocalHead {
		t.Fatal("branch not restored by CAS")
	}
	if !exactRawCommitRef(f.ctx, f.local, custody.RecoveryLocalRef(f.run.ID), request.ExpectedLocalHead) {
		t.Fatal("lost pre-recovery anchor")
	}
}

func TestReviewedRecoveryLateRunMutationLeavesMaterializedHeadWithoutCustody(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.service.afterRecoverBranchMove = func() {
		if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || f.custodyReturned() || !strings.Contains(got.Error, "materialized") {
		t.Fatalf("false success: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead {
		t.Fatal("unexpected rollback of materialized result")
	}
}

func TestReviewedRecoveryPreviewDisablesExternalDiffAndRefusesReplacedCommits(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	marker := filepath.Join(filepath.Dir(f.local), "external-diff-ran")
	helper := filepath.Join(filepath.Dir(f.local), "external-diff.sh")
	mustWrite(t, helper, "#!/bin/sh\nprintf external > '"+marker+"'\nprintf misleading-diff\n")
	if err := os.Chmod(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "config", "diff.external", helper)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil || !strings.Contains(plan.Diff, "reviewed replacement for feature") {
		t.Fatalf("diff: %v %#v", err, plan)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("external diff executed")
	}
	mustRun(t, f.gate, "replace", f.preserved, f.submitted)
	if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil {
		t.Fatal("replaced commit accepted as exact evidence")
	}
}

func TestReviewedRecoveryRejectsOversizedProofInsteadOfTruncatingConsent(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustWrite(t, filepath.Join(pipeline, "large.txt"), strings.Repeat("reviewed line\n", 400000))
	mustRun(t, pipeline, "add", "large.txt")
	mustRun(t, pipeline, "commit", "-m", "large reviewed diff")
	request.ReviewedHead = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, request.ReviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, request.ReviewedHead); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "update-ref", f.anchorRef(), request.ReviewedHead)
	if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("oversize not refused: %v", err)
	}
	if f.custodyReturned() || mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
		t.Fatal("oversized proof mutated caller")
	}
}

func TestReviewedRecoveryRefusesOlderTerminalPushOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"flag", "running", "fixing"} {
		t.Run(kind, func(t *testing.T) {
			f, request := newReviewedRecoveryFixture(t)
			older := f.run.ID
			latest, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatusWithVerifiedHead(latest.ID, types.RunFailed, f.preserved); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunReviewApprovedHeadSHA(latest.ID, f.preserved); err != nil {
				t.Fatal(err)
			}
			if err := custody.PreserveRecoveryHead(f.ctx, f.gate, latest.ID, f.preserved); err != nil {
				t.Fatal(err)
			}
			request.RunID = latest.ID
			plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "flag" {
				if err := f.db.SetRunPushActive(older, true); err != nil {
					t.Fatal(err)
				}
			} else {
				step, err := f.db.InsertStepResult(older, types.StepPush)
				if err != nil {
					t.Fatal(err)
				}
				status := types.StepStatusRunning
				if kind == "fixing" {
					status = types.StepStatusFixing
				}
				if err := f.db.UpdateStepStatus(step.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil {
				t.Fatal("older unsettled push offered preview")
			}
			got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
			latest, err = f.db.GetRun(latest.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Recovered || latest.CustodyReturnedAt != nil || mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
				t.Fatalf("unsettled push adopted: %#v", got)
			}
		})
	}
}
