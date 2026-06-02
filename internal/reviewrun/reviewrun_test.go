package reviewrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	testHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestReviewRunExitCodesMatchCommandTaxonomy(t *testing.T) {
	if exitOK != exitcode.Success || exitFailed != exitcode.Failure || exitAuth != exitcode.AuthConfigError || exitUpstream != exitcode.UpstreamError {
		t.Fatalf("reviewrun exit constants = ok:%d failed:%d auth:%d upstream:%d, want command taxonomy", exitOK, exitFailed, exitAuth, exitUpstream)
	}
}

func TestRunFreshPlansPostsAndCompletes(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("fresh")

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 || result.Outbox.Outcome != ledger.OutcomeComment {
		t.Fatalf("result = %#v, want comment exit 0", result)
	}
	if result.Run.Outcome == nil || *result.Run.Outcome != ledger.OutcomeComment {
		t.Fatalf("result run outcome = %v, want comment", result.Run.Outcome)
	}
	if planner.calls != 1 || planner.runs[0].RunID != "fresh-1" {
		t.Fatalf("planner calls/runs = %d %#v, want fresh-1", planner.calls, planner.runs)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 1 {
		t.Fatalf("issue comments = %d, want rollup post", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 1 {
		t.Fatalf("reviews = %d, want submit_review", len(reviews))
	}
	run, err := fixture.store.GetRun(ctx, "fresh-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeComment {
		t.Fatalf("run outcome = %#v, want comment", run.Outcome)
	}
}

func TestRunPrunesRetentionBeforeFreshAllocation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	old := fixture.allocateOldRetainedRun(t, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	provider := &retentionProvider{
		Fake: fixture.fake,
		beforeLive: func() {
			if _, err := fixture.store.GetRun(ctx, old.RunID); !errors.Is(err, ledger.ErrNotFound) {
				t.Fatalf("expired live run before provider GetPR error = %v, want ErrNotFound", err)
			}
		},
	}
	opts := fixture.opts(planner)
	opts.Provider = provider
	opts.NewRunID = sequence("fresh")

	if _, err := Run(ctx, opts, Request{Pipeline: fixture.req}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := fixture.store.GetRun(ctx, old.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("expired live run GetRun error = %v, want ErrNotFound", err)
	}
	if planner.calls != 1 {
		t.Fatalf("planner calls = %d, want fresh planning after retention", planner.calls)
	}
}

func TestRunPrunesConfiguredRetentionBeforeFreshAllocation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	old := fixture.allocateRetainedRunForPRKey(t, "old-live", "github_other_repo_1", ledger.PostModeLive, testNow().Add(-31*24*time.Hour))
	fresh := fixture.allocateRetainedRunForPRKey(t, "fresh-live", "github_other_repo_1", ledger.PostModeLive, testNow().Add(-29*24*time.Hour))
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	provider := &retentionProvider{
		Fake: fixture.fake,
		beforeLive: func() {
			if _, err := fixture.store.GetRun(ctx, old.RunID); !errors.Is(err, ledger.ErrNotFound) {
				t.Fatalf("expired live run before provider GetPR error = %v, want ErrNotFound", err)
			}
			if _, err := fixture.store.GetRun(ctx, fresh.RunID); err != nil {
				t.Fatalf("fresh live run before provider GetPR error = %v, want nil", err)
			}
		},
	}
	opts := fixture.opts(planner)
	opts.Provider = provider
	opts.NewRunID = sequence("fresh")
	opts.Retention = datalifecycle.RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour}

	if _, err := Run(ctx, opts, Request{Pipeline: fixture.req}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunManualOnlySkipsRetentionBeforeFreshAllocation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	oldLive := fixture.allocateRetainedRunForPRKey(t, "old-live", "github_other_repo_1", ledger.PostModeLive, testNow().Add(-365*24*time.Hour))
	oldDryRun := fixture.allocateRetainedRunForPRKey(t, "old-dry", "github_other_repo_1", ledger.PostModeDryRun, testNow().Add(-8*24*time.Hour))
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	provider := &retentionProvider{
		Fake: fixture.fake,
		beforeLive: func() {
			if _, err := fixture.store.GetRun(ctx, oldLive.RunID); err != nil {
				t.Fatalf("live run before provider GetPR error = %v, want nil", err)
			}
			if _, err := fixture.store.GetRun(ctx, oldDryRun.RunID); err != nil {
				t.Fatalf("dry-run before provider GetPR error = %v, want nil", err)
			}
		},
	}
	opts := fixture.opts(planner)
	opts.Provider = provider
	opts.NewRunID = sequence("fresh")
	opts.RetentionManualOnly = true

	if _, err := Run(ctx, opts, Request{Pipeline: fixture.req}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := fixture.store.GetRun(ctx, oldLive.RunID); err != nil {
		t.Fatalf("live run after Run error = %v, want nil", err)
	}
	if _, err := fixture.store.GetRun(ctx, oldDryRun.RunID); err != nil {
		t.Fatalf("dry-run after Run error = %v, want nil", err)
	}
}

func TestRunCommitsNamedSessionCandidateAfterPostSuccess(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	candidate := testNamedSessionCandidate("provider-new")
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment, namedCandidate: &candidate}
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("fresh")

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 || result.Outbox.Aborted {
		t.Fatalf("result = %#v, want successful post", result)
	}
	stored, err := fixture.store.GetNamedSession(ctx, candidate.Name)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if stored.ProviderSessionID != candidate.ProviderSessionID ||
		stored.CreatedAt != candidate.CreatedAt ||
		stored.LastUsedAt != candidate.LastUsedAt ||
		stored.Profile != candidate.Profile ||
		stored.Host != candidate.Host {
		t.Fatalf("stored named session = %#v, want %#v", stored, candidate)
	}
}

func TestRunDoesNotCommitNamedSessionCandidateAfterOutboxStaleSHAAbort(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.fake.SetError(gitprovider.OperationPostIssueComment, gitprovider.WrapError(gitprovider.ErrStaleSHA, gitprovider.OperationPostIssueComment, nil))
	candidate := testNamedSessionCandidate("provider-new")
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment, namedCandidate: &candidate}
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("fresh")

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != exitUpstream || !result.Outbox.Aborted || result.Outbox.Outcome != ledger.OutcomeAborted {
		t.Fatalf("result = %#v, want stale-SHA abort", result)
	}
	if _, err := fixture.store.GetNamedSession(ctx, candidate.Name); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want ErrNotFound", err)
	}
}

func TestRunDoesNotCommitNamedSessionCandidateAfterOutboxError(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	candidate := testNamedSessionCandidate("provider-new")
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment, namedCandidate: &candidate}
	limiterErr := errors.New("limiter failed")
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("fresh")
	opts.Limiter = failingLimiter{err: limiterErr}

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if !errors.Is(err, limiterErr) {
		t.Fatalf("Run error = %v, want limiter error", err)
	}
	if result.Outbox.Aborted {
		t.Fatalf("outbox = %#v, want non-aborted error", result.Outbox)
	}
	if _, err := fixture.store.GetNamedSession(ctx, candidate.Name); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want ErrNotFound", err)
	}
}

func TestRunResumeExistingActionsSkipsPlanner(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "resume-1", testBaseSHA)
	fixture.insertReviewActions(t, run.RunID, review.ReviewEventRequestChanges)
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	result, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want resume to skip LLM", planner.calls)
	}
	if result.Run.RunID != run.RunID || result.Outbox.Outcome != ledger.OutcomeRequestChanges {
		t.Fatalf("result = %#v, want resumed request_changes", result)
	}
	if result.Run.Outcome == nil || *result.Run.Outcome != ledger.OutcomeRequestChanges {
		t.Fatalf("result run outcome = %v, want request_changes", result.Run.Outcome)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 1 {
		t.Fatalf("issue comments = %d, want one rollup", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 1 {
		t.Fatalf("reviews = %d, want one review", len(reviews))
	}
}

func TestRunResumeInvalidStoredActionsFailsRunWithRerunGuidance(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "invalid-actions", testBaseSHA)
	if err := fixture.store.InsertPlannedAction(ctx, ledger.PlannedAction{
		ActionID:  "inline-1",
		RunID:     run.RunID,
		Kind:      ledger.PlannedActionInlineComment,
		PlannedAt: testNow(),
		PayloadJSON: payloadJSON(t, outbox.InlineCommentPayload{
			Body:        "inline",
			Path:        "main.go",
			Side:        review.DiffSideRight,
			Line:        1,
			SubjectType: review.AnchorKindLine,
		}),
		Status:   ledger.PlannedActionPending,
		Required: true,
	}); err != nil {
		t.Fatalf("InsertPlannedAction: %v", err)
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	_, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err == nil || !strings.Contains(err.Error(), "--rerun") {
		t.Fatalf("Run error = %v, want rerun guidance", err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want no LLM replay", planner.calls)
	}
	stored, err := fixture.store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("run outcome = %#v, want failed", stored.Outcome)
	}
}

func TestRunResumePartialPlanningStateFailsWithoutReplayingLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "partial-plan", testBaseSHA)
	if err := fixture.store.InsertSession(ctx, ledger.Session{
		SessionRowID:      "session-1",
		RunID:             run.RunID,
		ProviderSessionID: "provider-session",
		Role:              ledger.SessionRoleOrchestrator,
		Adapter:           "fake",
		Model:             "model",
		StartedAt:         testNow(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	_, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err == nil || !strings.Contains(err.Error(), "partial planning state") {
		t.Fatalf("Run error = %v, want partial planning guidance", err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want no LLM replay", planner.calls)
	}
	stored, err := fixture.store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("run outcome = %#v, want failed", stored.Outcome)
	}
}

func TestRunResumeEmptyPlanningStateReplansSameRun(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "empty-plan", testBaseSHA)
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("fresh")

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Run.RunID != run.RunID {
		t.Fatalf("result run = %q, want resume of %q", result.Run.RunID, run.RunID)
	}
	if planner.calls != 1 || len(planner.runs) != 1 || planner.runs[0].RunID != run.RunID {
		t.Fatalf("planner calls/runs = %d %#v, want one replan of existing run", planner.calls, planner.runs)
	}
	if result.Outbox.Outcome != ledger.OutcomeComment {
		t.Fatalf("outbox outcome = %q, want comment", result.Outbox.Outcome)
	}
	if _, err := fixture.store.GetRun(ctx, "fresh-1"); err == nil {
		t.Fatal("GetRun fresh-1 succeeded, want no fresh allocation for empty resume")
	}
}

func TestRunResumeFindingOnlyPartialPlanningStateFailsWithoutReplayingLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "finding-only-plan", testBaseSHA)
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	opts := fixture.opts(planner)
	opts.Store = findingOnlyStore{Store: fixture.store, runID: run.RunID}

	_, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err == nil || !strings.Contains(err.Error(), "partial planning state") {
		t.Fatalf("Run error = %v, want partial planning guidance", err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want no LLM replay", planner.calls)
	}
	stored, err := fixture.store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("run outcome = %#v, want failed", stored.Outcome)
	}
}

func TestRunAbortsWithoutPostingWhenHeadOrBaseMovesBeforePost(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	moved := fixture.pr
	moved.Head.SHA = "cccccccccccccccccccccccccccccccccccccccc"
	fixture.provider = &sequencedPRProvider{
		Fake: fixture.fake,
		prs:  []gitprovider.PR{fixture.pr, fixture.pr, moved},
	}
	candidate := testNamedSessionCandidate("provider-new")
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment, namedCandidate: &candidate}

	result, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != exitUpstream || !result.Outbox.Aborted {
		t.Fatalf("result = %#v, want aborted upstream exit", result)
	}
	if result.Run.Outcome == nil || *result.Run.Outcome != ledger.OutcomeAborted {
		t.Fatalf("result run outcome = %v, want aborted", result.Run.Outcome)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 0 {
		t.Fatalf("issue comments = %d, want no posts after movement", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 0 {
		t.Fatalf("reviews = %d, want no reviews after movement", len(reviews))
	}
	if _, err := fixture.store.GetNamedSession(ctx, candidate.Name); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want ErrNotFound", err)
	}
}

func TestRunFreshRunIDCollisionRetriesBeforePlanning(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	_, err := fixture.store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           "other_pr",
		PRURL:           "https://example.test/other",
		RunID:           "dupe",
		SHA:             strings.Repeat("d", 40),
		BaseSHA:         strings.Repeat("e", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		StartedAt:       testNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "dupe"),
	})
	if err != nil {
		t.Fatalf("AllocateRun collision seed: %v", err)
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	runIDs := []string{"dupe", "fresh-2"}
	opts := fixture.opts(planner)
	opts.NewRunID = func() string {
		next := runIDs[0]
		runIDs = runIDs[1:]
		return next
	}

	result, err := Run(ctx, opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Run.RunID != "fresh-2" || planner.calls != 1 || planner.runs[0].RunID != "fresh-2" {
		t.Fatalf("result/planner = %#v calls=%d runs=%#v, want fresh-2 after collision", result.Run, planner.calls, planner.runs)
	}
	if !strings.Contains(planner.runs[0].ArtifactPath, "fresh-2") || strings.Contains(planner.runs[0].ArtifactPath, "dupe") {
		t.Fatalf("planner artifact path = %q, want retried fresh run path", planner.runs[0].ArtifactPath)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 1 {
		t.Fatalf("issue comments = %d, want writes only after successful retry planning", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 1 {
		t.Fatalf("reviews = %d, want writes only after successful retry planning", len(reviews))
	}
}

func TestRunFreshRunIDCollisionFailsAfterBoundedAttemptsBeforePlanning(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	_, err := fixture.store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           "other_pr",
		PRURL:           "https://example.test/other",
		RunID:           "dupe",
		SHA:             strings.Repeat("d", 40),
		BaseSHA:         strings.Repeat("e", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		StartedAt:       testNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "dupe"),
	})
	if err != nil {
		t.Fatalf("AllocateRun collision seed: %v", err)
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}
	opts := fixture.opts(planner)
	opts.NewRunID = func() string { return "dupe" }

	_, err = Run(ctx, opts, Request{Pipeline: fixture.req})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("Run error = %v, want bounded collision failure", err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want no planning before successful fresh allocation", planner.calls)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 0 {
		t.Fatalf("issue comments = %d, want no writes after collision failure", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 0 {
		t.Fatalf("reviews = %d, want no writes after collision failure", len(reviews))
	}
}

func TestRunRepairAbortsWithExitUpstreamWhenHeadMoves(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeRequestChanges)
	moved := fixture.pr
	moved.Head.SHA = strings.Repeat("c", 40)
	fixture.provider = &sequencedPRProvider{
		Fake: fixture.fake,
		prs:  []gitprovider.PR{fixture.pr, moved},
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	result, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run repair moved head: %v", err)
	}
	if result.ExitCode != exitUpstream || result.Status != gateio.StatusBaseMovedAbort || !strings.Contains(result.Message, "head") {
		t.Fatalf("result = %#v, want moved-head upstream exit", result)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want repair path to skip planning", planner.calls)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 0 {
		t.Fatalf("issue comments = %d, want no writes after movement", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 0 {
		t.Fatalf("reviews = %d, want no writes after movement", len(reviews))
	}
}

func TestRunRetryPostsAbortsWithExitUpstreamWhenHeadMoves(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-retry", testBaseSHA)
	action := reviewActions(t, run.RunID, review.ReviewEventComment)[1]
	action.Status = ledger.PlannedActionFailedTerminal
	action.Error = strPtr("previous failure")
	failureClass := ledger.PlannedActionFailureClassTerminal
	action.FailureClass = &failureClass
	if err := fixture.store.InsertPlannedAction(ctx, action); err != nil {
		t.Fatalf("InsertPlannedAction: %v", err)
	}
	if err := fixture.store.CompleteRun(ctx, run.RunID, ledger.OutcomeFailed, testNow()); err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}
	moved := fixture.pr
	moved.Head.SHA = strings.Repeat("c", 40)
	fixture.provider = &sequencedPRProvider{
		Fake: fixture.fake,
		prs:  []gitprovider.PR{fixture.pr, moved},
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	result, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req, Flags: Flags{RetryPosts: true}})
	if err != nil {
		t.Fatalf("Run retry-posts moved head: %v", err)
	}
	if result.ExitCode != exitUpstream || result.Status != gateio.StatusBaseMovedAbort || !strings.Contains(result.Message, "head") {
		t.Fatalf("result = %#v, want moved-head upstream exit", result)
	}
	if !result.Outbox.Aborted || result.Outbox.Outcome != ledger.OutcomeAborted || result.Outbox.ExitCode != exitUpstream {
		t.Fatalf("outbox result = %#v, want aborted outcome metadata", result.Outbox)
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want retry-posts path to skip planning", planner.calls)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 0 {
		t.Fatalf("issue comments = %d, want no writes after movement", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 0 {
		t.Fatalf("reviews = %d, want no writes after movement", len(reviews))
	}
	storedAction := actionByID(t, fixture.store, run.RunID, action.ActionID)
	if storedAction.Status != ledger.PlannedActionFailedTerminal || storedAction.Error == nil || storedAction.FailureClass == nil {
		t.Fatalf("retry action after moved head = %#v, want unchanged failure", storedAction)
	}
	storedRun, err := fixture.store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun retry: %v", err)
	}
	if storedRun.Outcome == nil || *storedRun.Outcome != ledger.OutcomeAborted {
		t.Fatalf("retry run outcome = %v, want aborted", storedRun.Outcome)
	}
	if result.Run.Outcome == nil || *result.Run.Outcome != ledger.OutcomeAborted {
		t.Fatalf("result run outcome = %v, want aborted", result.Run.Outcome)
	}
}

type fixture struct {
	store    *ledger.Store
	provider gitprovider.GitProvider
	fake     *gitprovider.Fake
	ref      gitprovider.PRRef
	pr       gitprovider.PR
	req      pipeline.Request
	layout   statepaths.Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 30}
	pr := gitprovider.PR{
		Ref:    ref,
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/30",
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Head:   gitprovider.PRBranchRef{SHA: testHeadSHA},
		Base:   gitprovider.PRBranchRef{SHA: testBaseSHA},
	}
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	layout := statepaths.NewLayout(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "cache"))
	return &fixture{
		store:    store,
		provider: provider,
		fake:     provider,
		ref:      ref,
		pr:       pr,
		layout:   layout,
		req: pipeline.Request{
			PRRef:           ref,
			PRURL:           pr.URL,
			ProfileName:     "home",
			Profile:         config.Profile{},
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		},
	}
}

func (f *fixture) opts(planner Planner) Options {
	return Options{
		Store:                   f.store,
		Provider:                f.provider,
		Planner:                 planner,
		Limiter:                 noopLimiter{},
		Layout:                  f.layout,
		Now:                     testNow,
		StaleHeartbeatThreshold: time.Minute,
	}
}

func (f *fixture) allocateRun(t *testing.T, runID, baseSHA string) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(f.ref.Host, f.ref.Owner, f.ref.Repo, f.ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := f.store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           f.pr.URL,
		RunID:           runID,
		SHA:             f.pr.Head.SHA,
		BaseSHA:         baseSHA,
		Profile:         f.req.ProfileName,
		PostingIdentity: f.req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       testNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), runID),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func (f *fixture) allocateOldRetainedRun(t *testing.T, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(f.ref.Host, f.ref.Owner, f.ref.Repo, f.ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	return f.allocateRetainedRunForPRKey(t, runID, prKey, mode, started)
}

func (f *fixture) allocateRetainedRunForPRKey(t *testing.T, runID, prKey string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	artifactPath := filepath.Join(f.layout.DataRoot, "runs", prKey, f.pr.Head.SHA, f.pr.Base.SHA, "home__review-bot", runID)
	run, err := f.store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           f.pr.URL,
		RunID:           runID,
		SHA:             f.pr.Head.SHA,
		BaseSHA:         f.pr.Base.SHA,
		Profile:         f.req.ProfileName,
		PostingIdentity: f.req.PostingIdentity.Login,
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func (f *fixture) insertReviewActions(t *testing.T, runID string, event review.ReviewEvent) {
	t.Helper()
	for _, action := range reviewActions(t, runID, event) {
		if err := f.store.InsertPlannedAction(context.Background(), action); err != nil {
			t.Fatalf("InsertPlannedAction(%s): %v", action.ActionID, err)
		}
	}
}

func setPartialRollup(t *testing.T, f *fixture, runID string, outcome string) {
	t.Helper()
	body, err := marker.RenderAction(marker.ActionMarker{
		RunID:    runID,
		ActionID: "rollup-1",
		Kind:     marker.ActionKindRollupComment,
		SHA:      f.pr.Head.SHA,
		BaseSHA:  f.pr.Base.SHA,
		Outcome:  outcome,
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	if err := f.fake.SetIssueComments(f.ref, []gitprovider.IssueComment{{
		ID:        "issue-rollup",
		Author:    f.req.PostingIdentity,
		Body:      body,
		CreatedAt: testNow(),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
}

func actionByID(t *testing.T, store *ledger.Store, runID, actionID string) ledger.PlannedAction {
	t.Helper()
	actions, err := store.ListPlannedActions(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	for _, action := range actions {
		if action.ActionID == actionID {
			return action
		}
	}
	t.Fatalf("action %s/%s not found", runID, actionID)
	return ledger.PlannedAction{}
}

type findingOnlyStore struct {
	*ledger.Store
	runID string
}

func (s findingOnlyStore) ListFindings(ctx context.Context, runID string) ([]ledger.Finding, error) {
	if runID != s.runID {
		return s.Store.ListFindings(ctx, runID)
	}
	return []ledger.Finding{{
		FindingID:    review.FindingID("finding-only"),
		RunID:        runID,
		SessionRowID: "missing-session",
		Severity:     review.SeverityMajor,
		FilePath:     "main.go",
		Anchoring:    review.AnchoringInline,
		Body:         "finding body",
	}}, nil
}

type fakePlanner struct {
	store          *ledger.Store
	outcome        reviewplan.Outcome
	namedCandidate *ledger.NamedSession
	calls          int
	runs           []ledger.Run
}

type retentionProvider struct {
	*gitprovider.Fake
	beforeLive func()
}

func (p *retentionProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	if p.beforeLive != nil {
		p.beforeLive()
	}
	return p.Fake.GetPR(ctx, ref)
}

func (p *fakePlanner) Live(_ context.Context, _ pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	p.calls++
	p.runs = append(p.runs, run)
	event := review.ReviewEventComment
	if p.outcome == reviewplan.OutcomeRequestChanges {
		event = review.ReviewEventRequestChanges
	}
	for _, action := range reviewActions(nil, run.RunID, event) {
		if err := p.store.InsertPlannedAction(context.Background(), action); err != nil {
			return pipeline.Result{}, err
		}
	}
	return pipeline.Result{
		Run:                   run,
		PRKey:                 run.PRKey,
		Artifacts:             pipeline.ArtifactPathsFromDir(run.ArtifactPath),
		Plan:                  reviewplan.Plan{Outcome: p.outcome},
		NamedSessionCandidate: p.namedCandidate,
	}, nil
}

func reviewActions(t *testing.T, runID string, event review.ReviewEvent) []ledger.PlannedAction {
	now := testNow()
	return []ledger.PlannedAction{
		{
			ActionID:    "rollup-1",
			RunID:       runID,
			Kind:        ledger.PlannedActionRollupComment,
			PlannedAt:   now,
			PayloadJSON: payloadJSON(t, outbox.RollupCommentPayload{Body: "rollup body"}),
			Status:      ledger.PlannedActionPending,
			Required:    true,
		},
		{
			ActionID:    "submit-1",
			RunID:       runID,
			Kind:        ledger.PlannedActionSubmitReview,
			PlannedAt:   now,
			PayloadJSON: payloadJSON(t, outbox.SubmitReviewPayload{Body: "review body", Event: event}),
			Status:      ledger.PlannedActionPending,
			Required:    true,
		},
	}
}

func payloadJSON(t *testing.T, payload any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		if t != nil {
			t.Fatalf("Marshal: %v", err)
		}
		panic(err)
	}
	return string(data)
}

func strPtr(value string) *string {
	return &value
}

func testNamedSessionCandidate(providerSessionID string) ledger.NamedSession {
	return ledger.NamedSession{
		Name:              "daily",
		Profile:           "home",
		Provider:          "anthropic",
		Adapter:           "fake-llm",
		Model:             "sonnet",
		Host:              "github.com",
		ProviderSessionID: providerSessionID,
		CreatedAt:         testNow().Add(-time.Hour),
		LastUsedAt:        testNow(),
	}
}

type sequencedPRProvider struct {
	*gitprovider.Fake
	prs   []gitprovider.PR
	calls int
}

func (p *sequencedPRProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	if p.calls < len(p.prs) {
		pr := p.prs[p.calls]
		p.calls++
		return pr, nil
	}
	return p.Fake.GetPR(ctx, ref)
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

type failingLimiter struct {
	err error
}

func (l failingLimiter) Wait(context.Context, string) error { return l.err }

func sequence(prefix string) func() string {
	var counter int
	return func() string {
		counter++
		return fmt.Sprintf("%s-%d", prefix, counter)
	}
}

func testNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}
