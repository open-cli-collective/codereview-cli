package gateio

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	testHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testOldBase = "cccccccccccccccccccccccccccccccccccccccc"
)

var testNow = time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

func TestEvaluateResumesLocalRunBeforePRState(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-resume", testBaseSHA, ledger.PostModeLive)

	submit := mustRenderAction(t, marker.ActionMarker{
		RunID:    "run-complete",
		ActionID: "submit-1",
		Kind:     marker.ActionKindSubmitReview,
		SHA:      testHeadSHA,
		BaseSHA:  testBaseSHA,
	})
	if err := fixture.provider.SetReviews(fixture.req.PRRef, []gitprovider.Review{{
		ID:          gitprovider.ReviewID("review-1"),
		Author:      fixture.req.PostingIdentity,
		Body:        submit,
		SubmittedAt: testNow,
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}

	result, err := Evaluate(ctx, fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionResume || result.Run.RunID != run.RunID {
		t.Fatalf("Evaluate = %#v, want resume of %s", result, run.RunID)
	}
	if runs := fixture.listRuns(t); len(runs) != 1 {
		t.Fatalf("runs after resume = %d, want no fresh allocation", len(runs))
	}
}

func TestEvaluateIncompleteCompletedRunIsResumable(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-incomplete", testBaseSHA, ledger.PostModeLive)
	if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeIncomplete, testNow.Add(-time.Minute)); err != nil {
		t.Fatalf("CompleteRun incomplete: %v", err)
	}

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionResume || result.Run.RunID != run.RunID {
		t.Fatalf("Evaluate = %#v, want incomplete resume", result)
	}
}

func TestEvaluatePRMarkerDecisions(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(*testing.T, *fixture)
		wantStatus Status
		wantKind   gate.DecisionKind
		wantRunID  string
		wantOut    gate.PROutcome
		wantRuns   int
	}{
		{
			name: "complete submit review exits early",
			seed: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-submit", ActionID: "submit-1", Kind: marker.ActionKindSubmitReview, SHA: testHeadSHA, BaseSHA: testBaseSHA})
				setReviews(t, f, []gitprovider.Review{{ID: "review-1", Author: f.req.PostingIdentity, Body: body, SubmittedAt: testNow}})
			},
			wantStatus: StatusEarlyExit,
			wantKind:   gate.DecisionEarlyExit,
			wantRunID:  "run-submit",
		},
		{
			name: "complete no-diff rollup exits early",
			seed: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-nodiff", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeNothingToReview})
				setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-1", Author: f.req.PostingIdentity, Body: body, CreatedAt: testNow}})
			},
			wantStatus: StatusEarlyExit,
			wantKind:   gate.DecisionEarlyExit,
			wantRunID:  "run-nodiff",
			wantOut:    gate.PROutcomeNothingToReview,
		},
		{
			name: "partial rollup is repair unsupported",
			seed: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-partial", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeRequestChanges})
				setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-1", Author: f.req.PostingIdentity, Body: body, CreatedAt: testNow}})
			},
			wantStatus: StatusRepairUnsupported,
			wantKind:   gate.DecisionRepair,
			wantRunID:  "run-partial",
			wantOut:    gate.PROutcomeRequestChanges,
		},
		{
			name: "stale-base marker allocates fresh",
			seed: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-stale-marker", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testOldBase, Outcome: marker.RollupOutcomeComment})
				setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-1", Author: f.req.PostingIdentity, Body: body, CreatedAt: testNow}})
			},
			wantStatus: StatusContinue,
			wantKind:   gate.DecisionFresh,
			wantRunID:  "run-stale-marker",
			wantRuns:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			tt.seed(t, fixture)
			result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			defer releaseResultLock(t, result)
			if result.Status != tt.wantStatus || result.Decision.Kind != tt.wantKind || result.Decision.RunID != tt.wantRunID || result.Decision.Outcome != tt.wantOut {
				t.Fatalf("Evaluate = %#v, want status=%s kind=%s run=%q outcome=%q", result, tt.wantStatus, tt.wantKind, tt.wantRunID, tt.wantOut)
			}
			if tt.wantRuns > 0 && len(fixture.listRuns(t)) != tt.wantRuns {
				t.Fatalf("runs = %d, want %d", len(fixture.listRuns(t)), tt.wantRuns)
			}
		})
	}
}

func TestEvaluateIgnoresForgedOtherAuthorMarkers(t *testing.T) {
	fixture := newFixture(t)
	body := mustRenderAction(t, marker.ActionMarker{RunID: "forged", ActionID: "submit-1", Kind: marker.ActionKindSubmitReview, SHA: testHeadSHA, BaseSHA: testBaseSHA})
	setReviews(t, fixture, []gitprovider.Review{{ID: "review-forged", Author: gitprovider.Identity{Login: "other", ID: "other-id"}, Body: body, SubmittedAt: testNow}})

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionFresh {
		t.Fatalf("Evaluate = %#v, want fresh allocation", result)
	}
}

func TestEvaluatePartialRunScopeMismatchRepairs(t *testing.T) {
	fixture := newFixture(t)
	fixture.allocateRun(t, "run-partial", testOldBase, ledger.PostModeLive)
	body := mustRenderAction(t, marker.ActionMarker{RunID: "run-partial", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeApproved})
	setIssueComments(t, fixture, []gitprovider.IssueComment{{ID: "issue-1", Author: fixture.req.PostingIdentity, Body: body, CreatedAt: testNow}})

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Status != StatusRepairUnsupported || result.Decision.Kind != gate.DecisionRepair {
		t.Fatalf("Evaluate = %#v, want repair unsupported for scoped mismatch", result)
	}
}

func TestEvaluateStaleBaseLockAuthority(t *testing.T) {
	tests := []struct {
		name        string
		holdOldLock bool
		heartbeat   *time.Time
		wantAborted bool
		wantWarning string
	}{
		{name: "held fresh heartbeat", holdOldLock: true, heartbeat: timePtr(testNow.Add(-time.Minute))},
		{name: "held stale heartbeat warns", holdOldLock: true, heartbeat: timePtr(testNow.Add(-time.Hour)), wantWarning: "stale-base run run-stale is locked and has a stale heartbeat"},
		{name: "held nil heartbeat uses started at", holdOldLock: true, wantWarning: "stale-base run run-stale is locked and has a stale heartbeat"},
		{name: "free lock aborts", wantAborted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			run := fixture.allocateRun(t, "run-stale", testOldBase, ledger.PostModeLive)
			if tt.heartbeat != nil {
				setHeartbeat(t, fixture.store, run.RunID, *tt.heartbeat)
			}
			if tt.holdOldLock {
				fixture.locks.hold(t, fixture.lockPathForRun(t, run))
			}
			var warnings bytes.Buffer
			opts := fixture.opts()
			opts.Warnings = &warnings

			result, err := Evaluate(context.Background(), opts, fixture.req)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			defer releaseResultLock(t, result)
			gotRun, err := fixture.store.GetRun(context.Background(), run.RunID)
			if err != nil {
				t.Fatalf("GetRun stale: %v", err)
			}
			gotAborted := gotRun.Outcome != nil && *gotRun.Outcome == ledger.OutcomeAborted
			if gotAborted != tt.wantAborted {
				t.Fatalf("stale aborted = %v, want %v", gotAborted, tt.wantAborted)
			}
			if tt.wantWarning != "" && !strings.Contains(warnings.String(), tt.wantWarning) {
				t.Fatalf("warnings = %q, want %q", warnings.String(), tt.wantWarning)
			}
		})
	}
}

func TestEvaluateRerunSupersedesResumable(t *testing.T) {
	fixture := newFixture(t)
	old := fixture.allocateRun(t, "run-old", testBaseSHA, ledger.PostModeLive)
	fixture.req.Flags.Rerun = true

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate rerun: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Decision.Kind != gate.DecisionFresh || result.Run.RunID == old.RunID {
		t.Fatalf("Evaluate = %#v, want fresh run after rerun", result)
	}
	gotOld, err := fixture.store.GetRun(context.Background(), old.RunID)
	if err != nil {
		t.Fatalf("GetRun old: %v", err)
	}
	if gotOld.Outcome == nil || *gotOld.Outcome != ledger.OutcomeAborted {
		t.Fatalf("old outcome = %v, want aborted", gotOld.Outcome)
	}
}

func TestEvaluateDryRunSurfacesKernelErrors(t *testing.T) {
	fixture := newFixture(t)
	fixture.req.Flags.DryRun = true
	fixture.req.Flags.Rerun = true
	fixture.req.Flags.RetryPosts = true

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate dry-run invalid flags: %v", err)
	}
	if result.Status != StatusError || result.Decision.Kind != gate.DecisionError || result.Decision.ErrorReason != gate.ErrorMutuallyExclusiveFlags {
		t.Fatalf("Evaluate = %#v, want status error for mutually exclusive flags", result)
	}
	if runs := fixture.listRuns(t); len(runs) != 0 {
		t.Fatalf("runs after dry-run error = %d, want no allocation", len(runs))
	}
}

func TestAbortIfBaseMoved(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-current", testBaseSHA, ledger.PostModeLive)
	moved := fixture.req.PR
	moved.Base.SHA = testOldBase
	if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
		t.Fatalf("SetPR moved: %v", err)
	}

	result, err := AbortIfBaseMoved(context.Background(), fixture.opts(), fixture.req, run)
	if err != nil {
		t.Fatalf("AbortIfBaseMoved: %v", err)
	}
	if result.Status != StatusBaseMovedAbort {
		t.Fatalf("AbortIfBaseMoved = %#v, want base moved abort", result)
	}
	got, err := fixture.store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Outcome == nil || *got.Outcome != ledger.OutcomeAborted {
		t.Fatalf("outcome = %v, want aborted", got.Outcome)
	}
}

func TestSummarizeRunCountsRequiredActions(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-failed", testBaseSHA, ledger.PostModeLive)
	insertAction(t, fixture.store, plannedAction(run.RunID, "pending-required", ledger.PlannedActionPending, true, nil))
	insertAction(t, fixture.store, plannedAction(run.RunID, "pending-optional", ledger.PlannedActionPending, false, nil))
	insertAction(t, fixture.store, plannedAction(run.RunID, "failed-auth", ledger.PlannedActionFailedTerminal, true, strPtr(ledger.PlannedActionFailureClassAuth)))
	insertAction(t, fixture.store, plannedAction(run.RunID, "failed-optional", ledger.PlannedActionFailedTerminal, false, strPtr(ledger.PlannedActionFailureClassTerminal)))
	insertAction(t, fixture.store, plannedAction(run.RunID, "planned-only", ledger.PlannedActionPlannedOnly, true, nil))
	insertAction(t, fixture.store, plannedAction(run.RunID, "superseded", ledger.PlannedActionSuperseded, true, nil))
	if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, testNow); err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}
	run, err := fixture.store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	got, err := summarizeRun(context.Background(), fixture.store, run)
	if err != nil {
		t.Fatalf("summarizeRun: %v", err)
	}
	want := gate.RunSummary{
		RunID:                  run.RunID,
		Attempt:                run.Attempt,
		PostMode:               gate.PostModeLive,
		State:                  gate.RunStateFailed,
		RequiredPending:        1,
		RequiredFailedTerminal: 1,
		FailureClass:           gate.FailureClassAuth,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summarizeRun = %#v, want %#v", got, want)
	}
}

func TestEvaluateRejectsNonPositiveStaleThreshold(t *testing.T) {
	fixture := newFixture(t)
	opts := fixture.opts()
	opts.StaleHeartbeatThreshold = 0
	if _, err := Evaluate(context.Background(), opts, fixture.req); err == nil {
		t.Fatal("Evaluate threshold 0 error = nil, want error")
	}
}

type fixture struct {
	store    *ledger.Store
	provider *gitprovider.Fake
	layout   statepaths.Layout
	locks    *memoryLocks
	req      Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close ledger: %v", err)
		}
	})
	layout := statepaths.NewLayout(filepath.Join(t.TempDir(), "data", statepaths.AppDir), filepath.Join(t.TempDir(), "cache", statepaths.AppDir))
	ref := gitprovider.PRRef{Host: "github", Owner: "open-cli", Repo: "codereview-cli", Number: 22}
	prKey, err := statepaths.PRKey(ref.Host, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	bot := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	pr := gitprovider.PR{
		Ref:   ref,
		URL:   "https://example.test/pr/22",
		State: gitprovider.PRStateOpen,
		Head:  gitprovider.PRBranchRef{SHA: testHeadSHA},
		Base:  gitprovider.PRBranchRef{SHA: testBaseSHA},
	}
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	return &fixture{
		store:    store,
		provider: provider,
		layout:   layout,
		locks:    newMemoryLocks(),
		req: Request{
			PRRef:              ref,
			PR:                 pr,
			PRKey:              prKey,
			Profile:            "default",
			PostingIdentity:    bot,
			PostingIdentityKey: bot.Login,
			ArtifactPath:       "/tmp/fresh-run",
		},
	}
}

func (f *fixture) opts() Options {
	return Options{
		Store:                   f.store,
		Provider:                f.provider,
		Layout:                  f.layout,
		Acquire:                 f.locks.acquire,
		Now:                     func() time.Time { return testNow },
		StaleHeartbeatThreshold: 10 * time.Minute,
	}
}

func (f *fixture) allocateRun(t *testing.T, runID, baseSHA string, mode ledger.PostMode) ledger.Run {
	t.Helper()
	run, err := f.store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           f.req.PRKey,
		PRURL:           f.req.PR.URL,
		RunID:           runID,
		SHA:             f.req.PR.Head.SHA,
		BaseSHA:         baseSHA,
		Profile:         f.req.Profile,
		PostingIdentity: f.req.postingKey(),
		PostMode:        mode,
		StartedAt:       testNow.Add(-time.Hour),
		ArtifactPath:    "/tmp/" + runID,
	})
	if err != nil {
		t.Fatalf("AllocateRun(%s): %v", runID, err)
	}
	return run
}

func (f *fixture) listRuns(t *testing.T) []ledger.Run {
	t.Helper()
	runs, err := f.store.ListRunsForHeadScope(context.Background(), ledger.ListRunsForHeadScopeParams{
		PRKey:           f.req.PRKey,
		SHA:             f.req.PR.Head.SHA,
		Profile:         f.req.Profile,
		PostingIdentity: f.req.postingKey(),
	})
	if err != nil {
		t.Fatalf("ListRunsForHeadScope: %v", err)
	}
	return runs
}

func (f *fixture) lockPathForRun(t *testing.T, run ledger.Run) string {
	t.Helper()
	path, err := lockPathForRun(f.layout, f.req.PRRef, run)
	if err != nil {
		t.Fatalf("lockPathForRun: %v", err)
	}
	return path
}

type memoryLocks struct {
	held map[string]bool
}

type memoryLock struct {
	path  string
	locks *memoryLocks
}

func newMemoryLocks() *memoryLocks {
	return &memoryLocks{held: map[string]bool{}}
}

func (m *memoryLocks) acquire(path string) (Lock, error) {
	if m.held[path] {
		return nil, runlock.ErrHeld
	}
	m.held[path] = true
	return memoryLock{path: path, locks: m}, nil
}

func (m *memoryLocks) hold(t *testing.T, path string) {
	t.Helper()
	lock, err := m.acquire(path)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	t.Cleanup(func() {
		_ = lock.Release()
	})
}

func (l memoryLock) Release() error {
	if !l.locks.held[l.path] {
		return nil
	}
	delete(l.locks.held, l.path)
	return nil
}

func setReviews(t *testing.T, f *fixture, reviews []gitprovider.Review) {
	t.Helper()
	if err := f.provider.SetReviews(f.req.PRRef, reviews); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
}

func setIssueComments(t *testing.T, f *fixture, comments []gitprovider.IssueComment) {
	t.Helper()
	if err := f.provider.SetIssueComments(f.req.PRRef, comments); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
}

func mustRenderAction(t *testing.T, action marker.ActionMarker) string {
	t.Helper()
	body, err := marker.RenderAction(action)
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	return body
}

func releaseResultLock(t *testing.T, result Result) {
	t.Helper()
	if result.Lock != nil {
		if err := result.Lock.Release(); err != nil {
			t.Fatalf("Release result lock: %v", err)
		}
	}
}

func setHeartbeat(t *testing.T, store *ledger.Store, runID string, heartbeat time.Time) {
	t.Helper()
	if err := store.UpdateHeartbeat(context.Background(), runID, heartbeat); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
}

func insertAction(t *testing.T, store *ledger.Store, action ledger.PlannedAction) {
	t.Helper()
	if err := store.InsertPlannedAction(context.Background(), action); err != nil {
		t.Fatalf("InsertPlannedAction: %v", err)
	}
}

func plannedAction(runID, actionID string, status ledger.PlannedActionStatus, required bool, failureClass *string) ledger.PlannedAction {
	return ledger.PlannedAction{
		ActionID:     actionID,
		RunID:        runID,
		Kind:         ledger.PlannedActionRollupComment,
		PlannedAt:    testNow,
		PayloadJSON:  "{}",
		Status:       status,
		Required:     required,
		FailureClass: failureClass,
	}
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func strPtr(value string) *string {
	return &value
}
