package reviewrun

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
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
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 1 {
		t.Fatalf("issue comments = %d, want one rollup", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 1 {
		t.Fatalf("reviews = %d, want one review", len(reviews))
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

func TestRunAbortsWithoutPostingWhenHeadOrBaseMovesBeforePost(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	moved := fixture.pr
	moved.Head.SHA = "cccccccccccccccccccccccccccccccccccccccc"
	fixture.provider = &sequencedPRProvider{
		Fake: fixture.fake,
		prs:  []gitprovider.PR{fixture.pr, fixture.pr, moved},
	}
	planner := &fakePlanner{store: fixture.store, outcome: reviewplan.OutcomeComment}

	result, err := Run(ctx, fixture.opts(planner), Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != exitUpstream || !result.Outbox.Aborted {
		t.Fatalf("result = %#v, want aborted upstream exit", result)
	}
	if comments := fixture.fake.RecordedIssueComments(fixture.ref); len(comments) != 0 {
		t.Fatalf("issue comments = %d, want no posts after movement", len(comments))
	}
	if reviews := fixture.fake.RecordedReviews(fixture.ref); len(reviews) != 0 {
		t.Fatalf("reviews = %d, want no reviews after movement", len(reviews))
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

type fakePlanner struct {
	store   *ledger.Store
	outcome reviewplan.Outcome
	calls   int
	runs    []ledger.Run
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
		Run:       run,
		PRKey:     run.PRKey,
		Artifacts: pipeline.ArtifactPathsFromDir(run.ArtifactPath),
		Plan:      reviewplan.Plan{Outcome: p.outcome},
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
