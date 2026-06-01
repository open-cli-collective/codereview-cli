package gateio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/review"
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

func TestEvaluateLocalResumeSkipsExternalFailures(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-resume", testBaseSHA, ledger.PostModeLive)
	stale := fixture.allocateRun(t, "run-stale", testOldBase, ledger.PostModeLive)
	stalePath := fixture.lockPathForRun(t, stale)
	opts := fixture.opts()
	provider := &countingProvider{GitProvider: fixture.provider}
	opts.Provider = provider
	opts.Store = plannedActionErrorStore{
		Store: fixture.store,
		runID: stale.RunID,
		err:   errors.New("unexpected stale action lookup"),
	}
	opts.Acquire = func(path string) (Lock, error) {
		if path == stalePath {
			return nil, errors.New("unexpected stale lock probe")
		}
		return fixture.locks.acquire(path)
	}
	fixture.provider.SetError(gitprovider.OperationListIssueComments, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListIssueComments, errors.New("comments unavailable")))
	fixture.provider.SetError(gitprovider.OperationListReviews, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListReviews, errors.New("reviews unavailable")))

	result, err := Evaluate(context.Background(), opts, fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionResume || result.Run.RunID != run.RunID {
		t.Fatalf("Evaluate = %#v, want local resume of %s", result, run.RunID)
	}
	if provider.issueComments != 0 || provider.reviews != 0 {
		t.Fatalf("marker reads = issueComments:%d reviews:%d, want none", provider.issueComments, provider.reviews)
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

func TestEvaluateFreshUsesSuppliedRunID(t *testing.T) {
	fixture := newFixture(t)
	fixture.req.FreshRunID = "fresh-supplied"

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionFresh || result.Run.RunID != "fresh-supplied" {
		t.Fatalf("Evaluate = %#v, want fresh supplied run ID", result)
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
			name: "partial rollup repairs through outbox",
			seed: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-partial", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeRequestChanges})
				setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-1", Author: f.req.PostingIdentity, Body: body, CreatedAt: testNow}})
			},
			wantStatus: StatusRepairExecuted,
			wantKind:   gate.DecisionRepair,
			wantRunID:  "run-partial",
			wantOut:    gate.PROutcomeRequestChanges,
			wantRuns:   1,
		},
		{
			name: "paired rollup and submit review exits early",
			seed: func(t *testing.T, f *fixture) {
				rollup := mustRenderAction(t, marker.ActionMarker{RunID: "run-paired", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeRequestChanges})
				submit := mustRenderAction(t, marker.ActionMarker{RunID: "run-paired", ActionID: "submit-1", Kind: marker.ActionKindSubmitReview, SHA: testHeadSHA, BaseSHA: testBaseSHA})
				setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-1", Author: f.req.PostingIdentity, Body: rollup, CreatedAt: testNow}})
				setReviews(t, f, []gitprovider.Review{{ID: "review-1", Author: f.req.PostingIdentity, Body: submit, SubmittedAt: testNow.Add(time.Minute)}})
			},
			wantStatus: StatusEarlyExit,
			wantKind:   gate.DecisionEarlyExit,
			wantRunID:  "run-paired",
		},
		{
			name: "current complete marker beats stale marker",
			seed: func(t *testing.T, f *fixture) {
				current := mustRenderAction(t, marker.ActionMarker{RunID: "run-current", ActionID: "submit-1", Kind: marker.ActionKindSubmitReview, SHA: testHeadSHA, BaseSHA: testBaseSHA})
				stale := mustRenderAction(t, marker.ActionMarker{RunID: "run-stale-marker", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testOldBase, Outcome: marker.RollupOutcomeComment})
				setIssueComments(t, f, []gitprovider.IssueComment{
					{ID: "issue-current", Author: f.req.PostingIdentity, Body: current, CreatedAt: testNow},
					{ID: "issue-stale", Author: f.req.PostingIdentity, Body: stale, CreatedAt: testNow.Add(time.Minute)},
				})
			},
			wantStatus: StatusEarlyExit,
			wantKind:   gate.DecisionEarlyExit,
			wantRunID:  "run-current",
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

func TestEvaluatePartialRepairPostsSingleReview(t *testing.T) {
	fixture := newFixture(t)
	setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeApproved)

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate repair: %v", err)
	}
	if result.Status != StatusRepairExecuted || result.Run.RunID != "run-repair" {
		t.Fatalf("Evaluate repair = %#v, want repair execution for marker run", result)
	}
	if result.OutboxResult.Outcome != ledger.OutcomeApproved || result.OutboxResult.ExitCode != 0 || result.OutboxResult.Posted != 1 {
		t.Fatalf("OutboxResult = %#v, want one approved review post", result.OutboxResult)
	}
	if got := fixture.provider.RecordedIssueComments(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("issue comment writes = %d, want no duplicate rollup", len(got))
	}
	reviews := fixture.provider.RecordedReviews(fixture.req.PRRef)
	if len(reviews) != 1 {
		t.Fatalf("review writes = %d, want exactly one submit_review", len(reviews))
	}
	if reviews[0].Event != review.ReviewEventApprove {
		t.Fatalf("review event = %q, want approve", reviews[0].Event)
	}
	markers := marker.FindActions(reviews[0].Body)
	if len(markers) != 1 || markers[0].RunID != "run-repair" || markers[0].ActionID != repairSubmitReviewActionID || markers[0].Kind != marker.ActionKindSubmitReview {
		t.Fatalf("review markers = %#v, want repair submit marker", markers)
	}
	run, err := fixture.store.GetRun(context.Background(), "run-repair")
	if err != nil {
		t.Fatalf("GetRun repair: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeApproved {
		t.Fatalf("repair run outcome = %v, want approved", run.Outcome)
	}
	action := actionByID(t, fixture.store, "run-repair", repairSubmitReviewActionID)
	if action.Kind != ledger.PlannedActionSubmitReview || !action.Required || action.Status != ledger.PlannedActionPosted {
		t.Fatalf("repair action = %#v, want posted required submit_review", action)
	}
	var payload outbox.SubmitReviewPayload
	if err := json.Unmarshal([]byte(action.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode repair payload: %v", err)
	}
	if payload.Body != repairSubmitReviewBody || payload.Event != review.ReviewEventApprove {
		t.Fatalf("repair payload = %#v, want pinned body/event", payload)
	}
}

func TestEvaluatePartialRepairCleansUpRecoveryRunWhenActionInsertFails(t *testing.T) {
	fixture := newFixture(t)
	setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeApproved)
	opts := fixture.opts()
	opts.Store = insertActionErrorStore{Store: fixture.store, err: errors.New("insert failed")}

	if _, err := Evaluate(context.Background(), opts, fixture.req); err == nil {
		t.Fatal("Evaluate repair insert error = nil, want error")
	}
	if runs := fixture.listRuns(t); len(runs) != 0 {
		t.Fatalf("runs after failed repair action insert = %d, want cleanup", len(runs))
	}
	if _, err := fixture.store.GetRun(context.Background(), "run-repair"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRun repair after cleanup = %v, want ErrNotFound", err)
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("review writes = %d, want none", len(got))
	}
}

func TestEvaluatePartialRepairConcurrentAttemptsPostOneReview(t *testing.T) {
	fixture := newFixture(t)
	setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeComment)
	limiter := newBlockingLimiter()
	opts := fixture.opts()
	opts.Limiter = limiter

	type evalResult struct {
		result Result
		err    error
	}
	done := make(chan evalResult, 1)
	go func() {
		result, err := Evaluate(context.Background(), opts, fixture.req)
		done <- evalResult{result: result, err: err}
	}()
	<-limiter.entered

	if _, err := Evaluate(context.Background(), opts, fixture.req); !errors.Is(err, runlock.ErrHeld) {
		t.Fatalf("concurrent repair error = %v, want ErrHeld", err)
	}
	close(limiter.release)

	first := <-done
	if first.err != nil {
		t.Fatalf("first Evaluate repair: %v", first.err)
	}
	if first.result.Status != StatusRepairExecuted {
		t.Fatalf("first result = %#v, want repair executed", first.result)
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 1 {
		t.Fatalf("review writes = %d, want exactly one", len(got))
	}
}

func TestEvaluatePartialRunScopeMismatchErrors(t *testing.T) {
	fixture := newFixture(t)
	fixture.allocateRun(t, "run-partial", testOldBase, ledger.PostModeLive)
	body := mustRenderAction(t, marker.ActionMarker{RunID: "run-partial", ActionID: "rollup-1", Kind: marker.ActionKindRollupComment, SHA: testHeadSHA, BaseSHA: testBaseSHA, Outcome: marker.RollupOutcomeApproved})
	setIssueComments(t, fixture, []gitprovider.IssueComment{{ID: "issue-1", Author: fixture.req.PostingIdentity, Body: body, CreatedAt: testNow}})

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Status != StatusError || result.Decision.Kind != gate.DecisionError || result.Decision.ErrorReason != gate.ErrorInvalidInput {
		t.Fatalf("Evaluate = %#v, want invalid-input status for scoped mismatch", result)
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
			beforeRuns := fixture.listRuns(t)
			var warnings bytes.Buffer
			opts := fixture.opts()
			opts.Warnings = &warnings

			result, err := Evaluate(context.Background(), opts, fixture.req)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			defer releaseResultLock(t, result)
			if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionFresh || result.Run.RunID == "" {
				t.Fatalf("Evaluate = %#v, want fresh continuation after stale-base handling", result)
			}
			if result.Run.RunID == run.RunID || result.Run.BaseSHA != testBaseSHA {
				t.Fatalf("fresh run = %#v, want new current-base run", result.Run)
			}
			afterRuns := fixture.listRuns(t)
			if len(afterRuns) != len(beforeRuns)+1 {
				t.Fatalf("runs after Evaluate = %d, want %d", len(afterRuns), len(beforeRuns)+1)
			}
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
	stale := fixture.allocateRun(t, "run-stale", testOldBase, ledger.PostModeLive)
	stalePath := fixture.lockPathForRun(t, stale)
	fixture.req.Flags.Rerun = true
	opts := fixture.opts()
	provider := &countingProvider{GitProvider: fixture.provider}
	opts.Provider = provider
	opts.Acquire = func(path string) (Lock, error) {
		if path == stalePath {
			return nil, errors.New("unexpected stale lock probe")
		}
		return fixture.locks.acquire(path)
	}
	fixture.provider.SetError(gitprovider.OperationListIssueComments, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListIssueComments, errors.New("comments unavailable")))

	result, err := Evaluate(context.Background(), opts, fixture.req)
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
	if provider.issueComments != 0 || provider.reviews != 0 {
		t.Fatalf("marker reads = issueComments:%d reviews:%d, want none", provider.issueComments, provider.reviews)
	}
}

func TestEvaluateRerunDoesNotMutateWhenBaseRefetchFails(t *testing.T) {
	fixture := newFixture(t)
	old := fixture.allocateRun(t, "run-old", testBaseSHA, ledger.PostModeLive)
	fixture.req.Flags.Rerun = true
	fixture.provider.SetError(gitprovider.OperationGetPR, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationGetPR, errors.New("pr unavailable")))

	if _, err := Evaluate(context.Background(), fixture.opts(), fixture.req); err == nil {
		t.Fatal("Evaluate rerun GetPR error = nil, want error")
	}
	gotOld, err := fixture.store.GetRun(context.Background(), old.RunID)
	if err != nil {
		t.Fatalf("GetRun old: %v", err)
	}
	if gotOld.Outcome != nil {
		t.Fatalf("old outcome = %v, want unchanged running run", gotOld.Outcome)
	}
	if runs := fixture.listRuns(t); len(runs) != 1 {
		t.Fatalf("runs after failed rerun = %d, want no fresh allocation", len(runs))
	}
}

func TestEvaluateRetryPostsExecutesOnlyMissingReview(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
	rollup := plannedAction(run.RunID, "rollup-1", ledger.PlannedActionPosted, true, nil)
	rollup.PayloadJSON = payloadJSON(t, outbox.RollupCommentPayload{Body: "existing rollup"})
	rollup.PostedAt = timePtr(testNow.Add(-time.Minute))
	rollup.UpstreamID = strPtr("issue-rollup")
	insertAction(t, fixture.store, rollup)
	insertAction(t, fixture.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventRequestChanges))
	if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, testNow); err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}
	setPartialRollup(t, fixture, run.RunID, marker.RollupOutcomeRequestChanges)

	plain, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate failed partial: %v", err)
	}
	if plain.Status != StatusError || plain.Decision.ErrorReason != gate.ErrorPartialFailed {
		t.Fatalf("plain Evaluate = %#v, want failed partial guidance", plain)
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("plain review writes = %d, want no auto-repair", len(got))
	}

	stale := fixture.allocateRun(t, "run-stale", testOldBase, ledger.PostModeLive)
	stalePath := fixture.lockPathForRun(t, stale)
	fixture.req.Flags.RetryPosts = true
	opts := fixture.opts()
	opts.Acquire = func(path string) (Lock, error) {
		if path == stalePath {
			return nil, errors.New("unexpected stale lock probe")
		}
		return fixture.locks.acquire(path)
	}

	result, err := Evaluate(context.Background(), opts, fixture.req)
	if err != nil {
		t.Fatalf("Evaluate retry-posts: %v", err)
	}
	if result.Status != StatusRetryPostsExecuted || result.Decision.Kind != gate.DecisionRetryPosts || result.Decision.RunID != run.RunID {
		t.Fatalf("Evaluate = %#v, want retry-posts execution for %s", result, run.RunID)
	}
	if result.OutboxResult.Outcome != ledger.OutcomeRequestChanges || result.OutboxResult.ExitCode != 0 {
		t.Fatalf("OutboxResult = %#v, want request_changes success", result.OutboxResult)
	}
	if got := fixture.provider.RecordedIssueComments(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("issue comment writes = %d, want no duplicate rollup", len(got))
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 1 {
		t.Fatalf("review writes = %d, want exactly one submit_review", len(got))
	}
	submit := actionByID(t, fixture.store, run.RunID, "submit-1")
	if submit.Status != ledger.PlannedActionPosted || submit.Error != nil || submit.FailureClass != nil {
		t.Fatalf("submit after retry = %#v, want posted with cleared failure", submit)
	}
}

func TestEvaluateAbortedPartialIsAuditOnly(t *testing.T) {
	fixture := newFixture(t)
	aborted := fixture.allocateRun(t, "run-aborted", testBaseSHA, ledger.PostModeLive)
	if err := fixture.store.CompleteRun(context.Background(), aborted.RunID, ledger.OutcomeAborted, testNow); err != nil {
		t.Fatalf("CompleteRun aborted: %v", err)
	}
	setPartialRollup(t, fixture, aborted.RunID, marker.RollupOutcomeComment)

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate aborted partial: %v", err)
	}
	defer releaseResultLock(t, result)
	if result.Status != StatusContinue || result.Decision.Kind != gate.DecisionFresh || result.Run.RunID == aborted.RunID {
		t.Fatalf("Evaluate = %#v, want fresh run for aborted partial", result)
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("review writes = %d, want no repair post", len(got))
	}
}

func TestEvaluateRetryPostsIneligibleDoesNotPost(t *testing.T) {
	fixture := newFixture(t)
	fixture.req.Flags.RetryPosts = true

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate retry-posts ineligible: %v", err)
	}
	if result.Status != StatusError || result.Decision.ErrorReason != gate.ErrorRetryPostsIneligible {
		t.Fatalf("Evaluate = %#v, want retry-posts eligibility error", result)
	}
	if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
		t.Fatalf("review writes = %d, want none", len(got))
	}
}

func TestEvaluateRetryPostsUnprovableOutcomeDoesNotMutate(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, *fixture, ledger.Run)
	}{
		{
			name: "conflicting submit events",
			seed: func(t *testing.T, f *fixture, run ledger.Run) {
				insertAction(t, f.store, submitReviewAction(t, run.RunID, "submit-approve", ledger.PlannedActionFailedTerminal, true, review.ReviewEventApprove))
				insertAction(t, f.store, submitReviewAction(t, run.RunID, "submit-comment", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
			},
		},
		{
			name: "required inline only",
			seed: func(t *testing.T, f *fixture, run ledger.Run) {
				insertAction(t, f.store, ledger.PlannedAction{
					ActionID:  "inline-1",
					RunID:     run.RunID,
					Kind:      ledger.PlannedActionInlineComment,
					PlannedAt: testNow,
					PayloadJSON: payloadJSON(t, outbox.InlineCommentPayload{
						Body:        "inline",
						Path:        "main.go",
						Side:        review.DiffSideRight,
						Line:        1,
						SubjectType: review.AnchorKindLine,
					}),
					Status:   ledger.PlannedActionPending,
					Required: true,
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
			tt.seed(t, fixture, run)
			if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, testNow); err != nil {
				t.Fatalf("CompleteRun failed: %v", err)
			}
			before, err := fixture.store.ListPlannedActions(context.Background(), run.RunID)
			if err != nil {
				t.Fatalf("ListPlannedActions before: %v", err)
			}
			fixture.req.Flags.RetryPosts = true

			result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
			if err != nil {
				t.Fatalf("Evaluate retry-posts: %v", err)
			}
			if result.Status != StatusError || result.Decision.ErrorReason != gate.ErrorInvalidInput {
				t.Fatalf("Evaluate = %#v, want invalid-input status", result)
			}
			after, err := fixture.store.ListPlannedActions(context.Background(), run.RunID)
			if err != nil {
				t.Fatalf("ListPlannedActions after: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("actions mutated before desired outcome proved:\nafter=%#v\nbefore=%#v", after, before)
			}
			if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
				t.Fatalf("review writes = %d, want none", len(got))
			}
		})
	}
}

func TestEvaluateBaseRefetchFailureBeforeRepairOrRetryDoesNotMutate(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		fixture := newFixture(t)
		setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeApproved)
		fixture.provider.SetError(gitprovider.OperationGetPR, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationGetPR, errors.New("pr unavailable")))

		if _, err := Evaluate(context.Background(), fixture.opts(), fixture.req); err == nil {
			t.Fatal("Evaluate repair GetPR error = nil, want error")
		}
		if runs := fixture.listRuns(t); len(runs) != 0 {
			t.Fatalf("runs after failed repair = %d, want no recovery row", len(runs))
		}
		if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
			t.Fatalf("review writes = %d, want none", len(got))
		}
	})

	t.Run("retry-posts", func(t *testing.T) {
		fixture := newFixture(t)
		run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
		insertAction(t, fixture.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
		if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, testNow); err != nil {
			t.Fatalf("CompleteRun failed: %v", err)
		}
		fixture.req.Flags.RetryPosts = true
		fixture.provider.SetError(gitprovider.OperationGetPR, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationGetPR, errors.New("pr unavailable")))

		if _, err := Evaluate(context.Background(), fixture.opts(), fixture.req); err == nil {
			t.Fatal("Evaluate retry-posts GetPR error = nil, want error")
		}
		action := actionByID(t, fixture.store, run.RunID, "submit-1")
		if action.Status != ledger.PlannedActionFailedTerminal || action.Error == nil || action.FailureClass == nil {
			t.Fatalf("submit after failed retry precheck = %#v, want unchanged failure", action)
		}
		if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
			t.Fatalf("review writes = %d, want none", len(got))
		}
	})
}

func TestEvaluateBaseMovedPrecheckDoesNotRequireLimiter(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		fixture := newFixture(t)
		setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeApproved)
		moved := fixture.req.PR
		moved.Base.SHA = testOldBase
		if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
			t.Fatalf("SetPR moved: %v", err)
		}
		opts := fixture.opts()
		opts.Limiter = nil

		result, err := Evaluate(context.Background(), opts, fixture.req)
		if err != nil {
			t.Fatalf("Evaluate repair moved base: %v", err)
		}
		if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError {
			t.Fatalf("Evaluate = %#v, want base moved before limiter validation", result)
		}
		if runs := fixture.listRuns(t); len(runs) != 0 {
			t.Fatalf("runs after moved-base repair = %d, want no recovery row", len(runs))
		}
	})

	t.Run("retry-posts", func(t *testing.T) {
		fixture := newFixture(t)
		run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
		insertAction(t, fixture.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
		fixture.req.Flags.RetryPosts = true
		moved := fixture.req.PR
		moved.Base.SHA = testOldBase
		if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
			t.Fatalf("SetPR moved: %v", err)
		}
		opts := fixture.opts()
		opts.Limiter = nil

		result, err := Evaluate(context.Background(), opts, fixture.req)
		if err != nil {
			t.Fatalf("Evaluate retry moved base: %v", err)
		}
		if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError {
			t.Fatalf("Evaluate = %#v, want base moved before limiter validation", result)
		}
		action := actionByID(t, fixture.store, run.RunID, "submit-1")
		if action.Status != ledger.PlannedActionFailedTerminal || action.Error == nil {
			t.Fatalf("submit after moved-base retry = %#v, want unchanged failure", action)
		}
	})
}

func TestEvaluateHeadMovedPrecheckDoesNotRequireLimiter(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		fixture := newFixture(t)
		setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeApproved)
		moved := fixture.req.PR
		moved.Head.SHA = strings.Repeat("d", 40)
		if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
			t.Fatalf("SetPR moved: %v", err)
		}
		opts := fixture.opts()
		opts.Limiter = nil

		result, err := Evaluate(context.Background(), opts, fixture.req)
		if err != nil {
			t.Fatalf("Evaluate repair moved head: %v", err)
		}
		if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError || !strings.Contains(result.Decision.Message, "head") {
			t.Fatalf("Evaluate = %#v, want head moved before limiter validation", result)
		}
		if runs := fixture.listRuns(t); len(runs) != 0 {
			t.Fatalf("runs after moved-head repair = %d, want no recovery row", len(runs))
		}
		if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
			t.Fatalf("review writes = %d, want none", len(got))
		}
	})

	t.Run("retry-posts", func(t *testing.T) {
		fixture := newFixture(t)
		run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
		insertAction(t, fixture.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
		fixture.req.Flags.RetryPosts = true
		moved := fixture.req.PR
		moved.Head.SHA = strings.Repeat("d", 40)
		if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
			t.Fatalf("SetPR moved: %v", err)
		}
		opts := fixture.opts()
		opts.Limiter = nil

		result, err := Evaluate(context.Background(), opts, fixture.req)
		if err != nil {
			t.Fatalf("Evaluate retry moved head: %v", err)
		}
		if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError || !strings.Contains(result.Decision.Message, "head") {
			t.Fatalf("Evaluate = %#v, want head moved before limiter validation", result)
		}
		action := actionByID(t, fixture.store, run.RunID, "submit-1")
		if action.Status != ledger.PlannedActionFailedTerminal || action.Error == nil {
			t.Fatalf("submit after moved-head retry = %#v, want unchanged failure", action)
		}
		if got := fixture.provider.RecordedReviews(fixture.req.PRRef); len(got) != 0 {
			t.Fatalf("review writes = %d, want none", len(got))
		}
	})
}

func TestEvaluateOutboxErrorsReturnExecutionResultAndDurableState(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		fixture := newFixture(t)
		setPartialRollup(t, fixture, "run-repair", marker.RollupOutcomeComment)
		fixture.provider.SetError(gitprovider.OperationListInlineThreads, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListInlineThreads, errors.New("threads unavailable")))

		result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
		if err == nil {
			t.Fatal("Evaluate repair outbox read error = nil, want error")
		}
		if result.Status != StatusRepairExecuted || result.Run.RunID != "run-repair" || result.OutboxResult.ExitCode != 5 {
			t.Fatalf("Evaluate result = %#v, want repair execution result with upstream exit", result)
		}
		run, getErr := fixture.store.GetRun(context.Background(), "run-repair")
		if getErr != nil {
			t.Fatalf("GetRun repair: %v", getErr)
		}
		if run.Outcome != nil {
			t.Fatalf("repair outcome = %v, want nil after outbox read failure", run.Outcome)
		}
		action := actionByID(t, fixture.store, "run-repair", repairSubmitReviewActionID)
		if action.Status != ledger.PlannedActionPending || action.Error != nil || action.Attempts != 0 {
			t.Fatalf("repair action after outbox read failure = %#v, want untouched pending", action)
		}
	})

	t.Run("retry-posts", func(t *testing.T) {
		fixture := newFixture(t)
		run := fixture.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
		insertAction(t, fixture.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
		if err := fixture.store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, testNow); err != nil {
			t.Fatalf("CompleteRun failed: %v", err)
		}
		fixture.req.Flags.RetryPosts = true
		fixture.provider.SetError(gitprovider.OperationListInlineThreads, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListInlineThreads, errors.New("threads unavailable")))

		result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
		if err == nil {
			t.Fatal("Evaluate retry-posts outbox read error = nil, want error")
		}
		if result.Status != StatusRetryPostsExecuted || result.Run.RunID != run.RunID || result.OutboxResult.ExitCode != 5 {
			t.Fatalf("Evaluate result = %#v, want retry execution result with upstream exit", result)
		}
		action := actionByID(t, fixture.store, run.RunID, "submit-1")
		if action.Status != ledger.PlannedActionPending || action.Error != nil || action.FailureClass != nil || action.Attempts != 1 {
			t.Fatalf("submit after retry outbox read failure = %#v, want reset pending without new attempt", action)
		}
		gotRun, getErr := fixture.store.GetRun(context.Background(), run.RunID)
		if getErr != nil {
			t.Fatalf("GetRun retry: %v", getErr)
		}
		if gotRun.Outcome == nil || *gotRun.Outcome != ledger.OutcomeFailed {
			t.Fatalf("retry run outcome = %v, want previous failed outcome", gotRun.Outcome)
		}
	})
}

func TestEvaluateDryRunFreshDoesNotAllocate(t *testing.T) {
	fixture := newFixture(t)
	fixture.req.Flags.DryRun = true

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate dry-run: %v", err)
	}
	if result.Status != StatusDryRunFresh || result.Decision.Kind != gate.DecisionFresh {
		t.Fatalf("Evaluate = %#v, want dry-run fresh", result)
	}
	if runs := fixture.listRuns(t); len(runs) != 0 {
		t.Fatalf("runs after dry-run = %d, want no allocation", len(runs))
	}
}

func TestEvaluateFreshDoesNotAllocateWhenBaseRefetchFails(t *testing.T) {
	fixture := newFixture(t)
	fixture.provider.SetError(gitprovider.OperationGetPR, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationGetPR, errors.New("pr unavailable")))

	if _, err := Evaluate(context.Background(), fixture.opts(), fixture.req); err == nil {
		t.Fatal("Evaluate fresh GetPR error = nil, want error")
	}
	if runs := fixture.listRuns(t); len(runs) != 0 {
		t.Fatalf("runs after failed fresh = %d, want no allocation", len(runs))
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

func TestEvaluateRejectsMismatchedRequestIdentity(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{
			name: "pr snapshot ref",
			mutate: func(req *Request) {
				req.PR.Ref.Number++
			},
			wantErr: "PR snapshot ref",
		},
		{
			name: "pr key",
			mutate: func(req *Request) {
				req.PRKey = "github_other_repo_22"
			},
			wantErr: "pr key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			tt.mutate(&fixture.req)
			_, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
			if err == nil {
				t.Fatal("Evaluate error = nil, want mismatch error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Evaluate error = %q, want substring %q", err, tt.wantErr)
			}
		})
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

func TestEvaluateAbortsIfBaseMoved(t *testing.T) {
	fixture := newFixture(t)
	moved := fixture.req.PR
	moved.Base.SHA = testOldBase
	if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
		t.Fatalf("SetPR moved: %v", err)
	}

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError {
		t.Fatalf("Evaluate = %#v, want base moved abort", result)
	}
	runs := fixture.listRuns(t)
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want no allocation after base moved", len(runs))
	}
}

func TestEvaluateAbortsResumedRunIfBaseMoved(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-current", testBaseSHA, ledger.PostModeLive)
	moved := fixture.req.PR
	moved.Base.SHA = testOldBase
	if err := fixture.provider.SetPR(fixture.req.PRRef, moved); err != nil {
		t.Fatalf("SetPR moved: %v", err)
	}

	result, err := Evaluate(context.Background(), fixture.opts(), fixture.req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Status != StatusBaseMovedAbort || result.Decision.Kind != gate.DecisionError {
		t.Fatalf("Evaluate = %#v, want base moved abort", result)
	}
	got, err := fixture.store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Outcome == nil || *got.Outcome != ledger.OutcomeAborted {
		t.Fatalf("resumed outcome = %v, want aborted", got.Outcome)
	}
}

func TestAbortIfBaseMovedDoesNotRequireStaleThreshold(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-current", testBaseSHA, ledger.PostModeLive)
	opts := fixture.opts()
	opts.StaleHeartbeatThreshold = 0

	result, err := AbortIfBaseMoved(context.Background(), opts, fixture.req, run)
	if err != nil {
		t.Fatalf("AbortIfBaseMoved: %v", err)
	}
	if result.Status != StatusContinue {
		t.Fatalf("AbortIfBaseMoved = %#v, want continue", result)
	}
}

func TestGateRunStateMapsLedgerOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome *ledger.Outcome
		want    gate.RunState
	}{
		{name: "running", want: gate.RunStateRunning},
		{name: "incomplete", outcome: outcomePtr(ledger.OutcomeIncomplete), want: gate.RunStateIncomplete},
		{name: "approved", outcome: outcomePtr(ledger.OutcomeApproved), want: gate.RunStateApproved},
		{name: "request changes", outcome: outcomePtr(ledger.OutcomeRequestChanges), want: gate.RunStateRequestChanges},
		{name: "comment", outcome: outcomePtr(ledger.OutcomeComment), want: gate.RunStateComment},
		{name: "nothing to review", outcome: outcomePtr(ledger.OutcomeNothingToReview), want: gate.RunStateNothingToReview},
		{name: "dry run", outcome: outcomePtr(ledger.OutcomeDryRun), want: gate.RunStateDryRun},
		{name: "aborted", outcome: outcomePtr(ledger.OutcomeAborted), want: gate.RunStateAborted},
		{name: "failed", outcome: outcomePtr(ledger.OutcomeFailed), want: gate.RunStateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gateRunState(tt.outcome)
			if err != nil {
				t.Fatalf("gateRunState: %v", err)
			}
			if got != tt.want {
				t.Fatalf("gateRunState = %q, want %q", got, tt.want)
			}
		})
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

func TestEvaluateRejectsMissingLayoutDataRoot(t *testing.T) {
	fixture := newFixture(t)
	opts := fixture.opts()
	opts.Layout.DataRoot = ""
	if _, err := Evaluate(context.Background(), opts, fixture.req); err == nil {
		t.Fatal("Evaluate missing data root error = nil, want error")
	}
}

func TestEvaluateOutboxLimiterRequiredOnlyForPostExecution(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*testing.T, *fixture)
		wantStatus Status
		wantErr    string
	}{
		{
			name:       "fresh does not require limiter",
			wantStatus: StatusContinue,
		},
		{
			name: "resume does not require limiter",
			setup: func(t *testing.T, f *fixture) {
				f.allocateRun(t, "run-resume", testBaseSHA, ledger.PostModeLive)
			},
			wantStatus: StatusContinue,
		},
		{
			name: "early exit does not require limiter",
			setup: func(t *testing.T, f *fixture) {
				body := mustRenderAction(t, marker.ActionMarker{RunID: "run-submit", ActionID: "submit-1", Kind: marker.ActionKindSubmitReview, SHA: testHeadSHA, BaseSHA: testBaseSHA})
				setReviews(t, f, []gitprovider.Review{{ID: "review-1", Author: f.req.PostingIdentity, Body: body, SubmittedAt: testNow}})
			},
			wantStatus: StatusEarlyExit,
		},
		{
			name: "dry-run does not require limiter",
			setup: func(_ *testing.T, f *fixture) {
				f.req.Flags.DryRun = true
			},
			wantStatus: StatusDryRunFresh,
		},
		{
			name: "repair requires limiter",
			setup: func(t *testing.T, f *fixture) {
				setPartialRollup(t, f, "run-repair", marker.RollupOutcomeApproved)
			},
			wantErr: "outbox limiter is required",
		},
		{
			name: "retry-posts requires limiter",
			setup: func(t *testing.T, f *fixture) {
				run := f.allocateRun(t, "run-retry", testBaseSHA, ledger.PostModeLive)
				insertAction(t, f.store, submitReviewAction(t, run.RunID, "submit-1", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment))
				f.req.Flags.RetryPosts = true
			},
			wantErr: "outbox limiter is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			if tt.setup != nil {
				tt.setup(t, fixture)
			}
			opts := fixture.opts()
			opts.Limiter = nil
			result, err := Evaluate(context.Background(), opts, fixture.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Evaluate error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			defer releaseResultLock(t, result)
			if result.Status != tt.wantStatus {
				t.Fatalf("Evaluate status = %s, want %s", result.Status, tt.wantStatus)
			}
		})
	}
}

func TestAbortStaleRunsRequiresLockedTarget(t *testing.T) {
	fixture := newFixture(t)
	err := abortStaleRuns(context.Background(), fixture.opts(), gateState{staleLocks: map[string]staleProbe{}}, []string{"missing-run"})
	if err == nil {
		t.Fatal("abortStaleRuns missing lock error = nil, want error")
	}
}

func TestResetRequiredFailedTerminalActions(t *testing.T) {
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "run-reset", testBaseSHA, ledger.PostModeLive)
	attemptedAt := testNow.Add(-time.Hour)
	postedAt := testNow.Add(-time.Minute)
	requiredFailed := submitReviewAction(t, run.RunID, "required-failed", ledger.PlannedActionFailedTerminal, true, review.ReviewEventComment)
	requiredFailed.Attempts = 7
	requiredFailed.AttemptedAt = &attemptedAt
	requiredFailed.PostedAt = &postedAt
	requiredFailed.UpstreamID = strPtr("stale-upstream")
	insertAction(t, fixture.store, requiredFailed)
	optionalFailed := submitReviewAction(t, run.RunID, "optional-failed", ledger.PlannedActionFailedTerminal, false, review.ReviewEventComment)
	insertAction(t, fixture.store, optionalFailed)
	posted := submitReviewAction(t, run.RunID, "posted", ledger.PlannedActionPosted, true, review.ReviewEventComment)
	posted.PostedAt = &postedAt
	posted.UpstreamID = strPtr("review-posted")
	insertAction(t, fixture.store, posted)
	superseded := submitReviewAction(t, run.RunID, "superseded", ledger.PlannedActionSuperseded, true, review.ReviewEventComment)
	insertAction(t, fixture.store, superseded)
	plannedOnly := submitReviewAction(t, run.RunID, "planned-only", ledger.PlannedActionPlannedOnly, true, review.ReviewEventComment)
	insertAction(t, fixture.store, plannedOnly)

	actions, err := fixture.store.ListPlannedActions(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if err := resetRequiredFailedTerminalActionsForRetry(context.Background(), fixture.store, actions); err != nil {
		t.Fatalf("resetRequiredFailedTerminalActionsForRetry: %v", err)
	}

	got := actionByID(t, fixture.store, run.RunID, "required-failed")
	if got.Status != ledger.PlannedActionPending || got.Error != nil || got.FailureClass != nil || got.PostedAt != nil || got.UpstreamID != nil {
		t.Fatalf("required failed reset = %#v, want pending with failure/post fields cleared", got)
	}
	if got.Attempts != 7 || got.AttemptedAt == nil || !got.AttemptedAt.Equal(attemptedAt) {
		t.Fatalf("required failed attempts = %d attempted_at = %v, want preserved", got.Attempts, got.AttemptedAt)
	}
	for _, actionID := range []string{"optional-failed", "posted", "superseded", "planned-only"} {
		before := map[string]ledger.PlannedAction{
			"optional-failed": optionalFailed,
			"posted":          posted,
			"superseded":      superseded,
			"planned-only":    plannedOnly,
		}[actionID]
		after := actionByID(t, fixture.store, run.RunID, actionID)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s changed after reset: got %#v want %#v", actionID, after, before)
		}
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
		Limiter:                 noopLimiter{},
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
	mu   sync.Mutex
	held map[string]bool
}

type memoryLock struct {
	path  string
	locks *memoryLocks
}

type countingProvider struct {
	gitprovider.GitProvider
	issueComments int
	reviews       int
}

type plannedActionErrorStore struct {
	Store
	runID string
	err   error
}

type insertActionErrorStore struct {
	Store
	err error
}

type noopLimiter struct{}

type blockingLimiter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s plannedActionErrorStore) ListPlannedActions(ctx context.Context, runID string) ([]ledger.PlannedAction, error) {
	if runID == s.runID {
		return nil, s.err
	}
	return s.Store.ListPlannedActions(ctx, runID)
}

func (s insertActionErrorStore) InsertPlannedAction(context.Context, ledger.PlannedAction) error {
	return s.err
}

func (p *countingProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	p.issueComments++
	return p.GitProvider.ListIssueComments(ctx, ref)
}

func (p *countingProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	p.reviews++
	return p.GitProvider.ListReviews(ctx, ref)
}

func (noopLimiter) Wait(context.Context, string) error {
	return nil
}

func newBlockingLimiter() *blockingLimiter {
	return &blockingLimiter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (l *blockingLimiter) Wait(ctx context.Context, _ string) error {
	l.once.Do(func() {
		close(l.entered)
	})
	select {
	case <-l.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newMemoryLocks() *memoryLocks {
	return &memoryLocks{held: map[string]bool{}}
}

func (m *memoryLocks) acquire(path string) (Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	l.locks.mu.Lock()
	defer l.locks.mu.Unlock()
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

func setPartialRollup(t *testing.T, f *fixture, runID string, outcome string) {
	t.Helper()
	body := mustRenderAction(t, marker.ActionMarker{
		RunID:    runID,
		ActionID: "rollup-1",
		Kind:     marker.ActionKindRollupComment,
		SHA:      testHeadSHA,
		BaseSHA:  testBaseSHA,
		Outcome:  outcome,
	})
	setIssueComments(t, f, []gitprovider.IssueComment{{ID: "issue-rollup", Author: f.req.PostingIdentity, Body: body, CreatedAt: testNow}})
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
	t.Fatalf("action %s not found for run %s", actionID, runID)
	return ledger.PlannedAction{}
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

func submitReviewAction(t *testing.T, runID, actionID string, status ledger.PlannedActionStatus, required bool, event review.ReviewEvent) ledger.PlannedAction {
	t.Helper()
	action := ledger.PlannedAction{
		ActionID:     actionID,
		RunID:        runID,
		Kind:         ledger.PlannedActionSubmitReview,
		PlannedAt:    testNow,
		PayloadJSON:  payloadJSON(t, outbox.SubmitReviewPayload{Body: "review body", Event: event}),
		Status:       status,
		Required:     required,
		FailureClass: nil,
	}
	if status == ledger.PlannedActionFailedTerminal {
		action.Error = strPtr("permission denied")
		action.FailureClass = strPtr(ledger.PlannedActionFailureClassAuth)
		action.Attempts = 1
		action.AttemptedAt = timePtr(testNow.Add(-time.Minute))
	}
	return action
}

func payloadJSON(t *testing.T, payload any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	return string(data)
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func outcomePtr(value ledger.Outcome) *ledger.Outcome {
	return &value
}

func strPtr(value string) *string {
	return &value
}
