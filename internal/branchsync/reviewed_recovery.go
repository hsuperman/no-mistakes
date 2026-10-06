package branchsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// ReviewedRecoveryRequest selects facts, never refs to resolve or guessed review
// authority. Consent is supplied separately, after inspecting the full preview.
type ReviewedRecoveryRequest struct {
	RunID, ExpectedLocalHead, ReviewedHead string
}

// ReviewedRecoveryPlan is a read-only, exact caller-to-reviewed-tree preview.
// Digest is consent to this whole plan, not a claim of content containment.
type ReviewedRecoveryPlan struct {
	Request                                             ReviewedRecoveryRequest
	RepositoryID, Caller, CommonDir, Branch             string
	GateDir                                             string
	SubmittedHead, PublishedHead, GateHead, RecoveryRef string
	SourceTree, TargetTree, Status                      string
	VerifiedAt                                          int64
	Diff, Digest                                        string
}

func exactCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value) && strings.ToLower(value) == value && strings.Trim(value, "0") != ""
}

// PreviewReviewedRecovery never fetches, creates anchors, migrates a database,
// changes custody or contacts a remote. Callers must open their DB read-only.
func (s *Service) PreviewReviewedRecovery(ctx context.Context, request ReviewedRecoveryRequest) (ReviewedRecoveryPlan, error) {
	return s.reviewedRecoveryPlan(ctx, request, request.ExpectedLocalHead)
}

func (s *Service) reviewedRecoveryPlan(ctx context.Context, request ReviewedRecoveryRequest, localHead string) (ReviewedRecoveryPlan, error) {
	var plan ReviewedRecoveryPlan
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return plan, fmt.Errorf("%s", refusal.Error)
	}
	if request.RunID == "" || !exactCommitID(request.ExpectedLocalHead) || !exactCommitID(request.ReviewedHead) {
		return plan, fmt.Errorf("an exact run, caller commit and reviewed commit are required")
	}
	state, run, _ := s.inspect(ctx)
	if run == nil || run.ID != request.RunID || run.RepoID != s.Repo.ID || !terminalRunStatus(run.Status) || run.PushActive || pushStepRunning(s.DB, run.ID) || run.CustodyReturnedAt != nil || run.TerminalHeadVerifiedAt == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != request.ReviewedHead || run.HeadSHA != request.ReviewedHead {
		return plan, fmt.Errorf("the selected lane does not have this exact verified terminal review binding")
	}
	active, err := s.DB.GetActiveRun(s.Repo.ID, run.Branch)
	if err != nil || active != nil {
		return plan, fmt.Errorf("an active run owns the lane or its ownership could not be read")
	}
	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		return plan, err
	}
	for _, other := range runs {
		if other.Branch == run.Branch && other.ID > run.ID {
			return plan, fmt.Errorf("a newer run owns the selected lane")
		}
	}
	registered, err := s.DB.GetRepo(s.Repo.ID)
	if err != nil || registered == nil || !samePath(registered.WorkingPath, s.Repo.WorkingPath) {
		return plan, fmt.Errorf("the repository registration changed")
	}
	if !state.Local.Clean || state.Local.Branch != run.Branch || state.Local.Head != localHead {
		return plan, fmt.Errorf("the exact registered caller branch and clean head are required")
	}
	for _, dir := range []string{s.workDir(), s.GateDir} {
		replacements, err := git.Run(ctx, dir, "replace", "-l")
		if err != nil || replacements != "" {
			return plan, fmt.Errorf("commit replacement refs prevent exact reviewed recovery")
		}
	}
	if request.ExpectedLocalHead != ptr(run.SubmittedHeadSHA) && request.ExpectedLocalHead != ptr(run.LastPushedSHA) {
		return plan, fmt.Errorf("the caller must be the exact submitted or published commit")
	}
	anchor := custody.RecoveryRef(run.ID)
	if !exactRawCommitRef(ctx, s.GateDir, anchor, request.ReviewedHead) {
		return plan, fmt.Errorf("the exact nonsymbolic run recovery anchor is required")
	}
	gateHead, exists, err := git.ExactRefTarget(ctx, s.GateDir, "refs/heads/"+run.Branch)
	if err != nil || !exists || !exactRawCommitRef(ctx, s.GateDir, "refs/heads/"+run.Branch, gateHead) {
		return plan, fmt.Errorf("the exact gate branch could not be verified")
	}
	for _, commit := range []string{ptr(run.SubmittedHeadSHA), ptr(run.LastPushedSHA), request.ExpectedLocalHead, request.ReviewedHead} {
		if commit != "" && (!exactCommitID(commit) || !objectExists(ctx, s.GateDir, commit)) {
			return plan, fmt.Errorf("a required submitted, published or reviewed commit is missing")
		}
	}
	for _, binding := range reviewedRecoveryAnchors(run, request.ExpectedLocalHead) {
		if compatible, err := recoveryRefCompatible(ctx, s.workDir(), binding.ref, binding.head); err != nil || !compatible {
			return plan, fmt.Errorf("a caller preservation anchor conflicts with the preview")
		}
	}
	caller, err := git.FindGitRoot(s.workDir())
	if err != nil {
		return plan, err
	}
	caller, err = filepath.EvalSymlinks(caller)
	if err != nil {
		return plan, err
	}
	common, err := git.Run(ctx, s.workDir(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return plan, err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return plan, err
	}
	gateDir, err := filepath.EvalSymlinks(s.GateDir)
	if err != nil {
		return plan, err
	}
	gateDir, err = filepath.Abs(gateDir)
	if err != nil {
		return plan, err
	}
	sourceTree, err := git.Run(ctx, s.GateDir, "rev-parse", request.ExpectedLocalHead+"^{tree}")
	if err != nil {
		return plan, err
	}
	targetTree, err := git.Run(ctx, s.GateDir, "rev-parse", request.ReviewedHead+"^{tree}")
	if err != nil {
		return plan, err
	}
	diff, err := git.RunRaw(ctx, s.GateDir, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--no-color", request.ExpectedLocalHead, request.ReviewedHead, "--")
	if err != nil {
		return plan, err
	}
	if len(diff) > 4*1024*1024 {
		return plan, fmt.Errorf("reviewed recovery diff exceeds the 4 MiB consent proof limit; reconcile manually")
	}
	plan = ReviewedRecoveryPlan{Request: request, RepositoryID: s.Repo.ID, Caller: caller, CommonDir: common, GateDir: gateDir, Branch: run.Branch, SubmittedHead: ptr(run.SubmittedHeadSHA), PublishedHead: ptr(run.LastPushedSHA), GateHead: gateHead, RecoveryRef: anchor, SourceTree: sourceTree, TargetTree: targetTree, Status: string(run.Status), VerifiedAt: *run.TerminalHeadVerifiedAt, Diff: string(diff)}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	sum := sha256.Sum256(encoded)
	plan.Digest = hex.EncodeToString(sum[:])
	return plan, nil
}

func exactRawCommitRef(ctx context.Context, dir, ref, head string) bool {
	if symbolic, err := git.Run(ctx, dir, "symbolic-ref", "-q", ref); err == nil && symbolic != "" {
		return false
	}
	target, exists, err := git.ExactRefTarget(ctx, dir, ref)
	if err != nil || !exists || target != head || !exactCommitID(head) {
		return false
	}
	commit, err := git.Run(ctx, dir, "rev-parse", "--verify", head+"^{commit}")
	return err == nil && commit == head
}

type reviewedAnchor struct{ ref, head string }

func reviewedRecoveryAnchors(run *db.Run, local string) []reviewedAnchor {
	anchors := []reviewedAnchor{{custody.RecoveryRef(run.ID), run.HeadSHA}, {custody.RecoveryLocalRef(run.ID), local}}
	if ptr(run.SubmittedHeadSHA) != "" {
		anchors = append(anchors, reviewedAnchor{"refs/no-mistakes/recover-submitted/" + run.ID, *run.SubmittedHeadSHA})
	}
	if ptr(run.LastPushedSHA) != "" {
		anchors = append(anchors, reviewedAnchor{"refs/no-mistakes/recover-published/" + run.ID, *run.LastPushedSHA})
	}
	return anchors
}

func recoveryRefCompatible(ctx context.Context, dir, ref, head string) (bool, error) {
	if symbolic, err := git.Run(ctx, dir, "symbolic-ref", "-q", ref); err == nil && symbolic != "" {
		return false, nil
	}
	target, exists, err := git.ExactRefTarget(ctx, dir, ref)
	return !exists || target == head, err
}

// AdoptReviewedRecovery is an explicit operator choice, never a weakened
// ordinary Recover containment proof. No push, rerun or validation is implied.
func (s *Service) AdoptReviewedRecovery(ctx context.Context, request ReviewedRecoveryRequest, consent string) State {
	state, run, _ := s.inspect(ctx)
	refuse := func(err error) State {
		return blockedPlan(state, state.State, "blocked_reviewed_recovery", err.Error())
	}
	plan, err := s.PreviewReviewedRecovery(ctx, request)
	if err != nil {
		return refuse(err)
	}
	if consent == "" || consent != plan.Digest {
		return refuse(fmt.Errorf("explicit consent to the exact displayed recovery digest is required"))
	}
	run, err = s.DB.GetRun(request.RunID)
	if err != nil || run == nil {
		return refuse(fmt.Errorf("the selected run is no longer readable"))
	}
	validate := func(local string) bool {
		fresh, err := s.reviewedRecoveryPlan(ctx, request, local)
		return err == nil && fresh.Digest == consent
	}
	if !validate(request.ExpectedLocalHead) {
		return refuse(fmt.Errorf("the recovery evidence changed before preservation"))
	}
	for _, anchor := range reviewedRecoveryAnchors(run, request.ExpectedLocalHead) {
		if !objectExists(ctx, s.workDir(), anchor.head) {
			if err := git.FetchRemoteRef(ctx, s.workDir(), s.GateDir, anchor.head, anchor.head); err != nil {
				return refuse(fmt.Errorf("could not import reviewed recovery objects: %w", err))
			}
		}
		if err := custody.PreserveRecoveryAnchor(ctx, s.workDir(), anchor.ref, anchor.head); err != nil {
			return refuse(fmt.Errorf("could not preserve recovery evidence: %w", err))
		}
	}
	return s.recoverMovePreserved(ctx, run, state, request.ReviewedHead, true, func() bool { return validate(request.ExpectedLocalHead) }, func() State {
		state, _, _ = s.inspect(ctx)
		state.Changed = state.Local.Head != request.ExpectedLocalHead
		if !validate(request.ReviewedHead) {
			return refuse(fmt.Errorf("reviewed head materialized but evidence changed; preserved refs remain, custody was not returned"))
		}
		updated, err := s.DB.SetReviewedRunCustodyReturned(run)
		if err != nil || !updated {
			return refuse(fmt.Errorf("reviewed head materialized but conditional custody stamp refused; preserved refs remain, inspect before retrying"))
		}
		result, _, _ := s.inspect(ctx)
		result.Changed, result.Recovered = true, true
		return result
	})
}
