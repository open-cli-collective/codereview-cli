package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestPostEmbedsMarkersAndPostsInCanonicalOrder(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))
	insertAction(t, store, plannedAction(run.RunID, "resolve-1", ledger.PlannedActionResolveThread, false, "thread-1", ResolveThreadPayload{}))
	insertAction(t, store, plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, false, "thread-1", ThreadReplyPayload{Body: "reply body"}))

	result, err := Post(context.Background(), Options{
		Store:    store,
		Provider: provider,
		Limiter:  noopLimiter{},
		Now:      fixedClock(),
	}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeRequestChanges || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want request_changes exit 0", result)
	}
	wantOrder := []string{"ReplyToThread", "ResolveThread", "PostInlineComment", "PostIssueComment", "SubmitReview"}
	if !reflect.DeepEqual(provider.writes, wantOrder) {
		t.Fatalf("provider write order = %#v, want %#v", provider.writes, wantOrder)
	}

	reply := provider.RecordedThreadReplies(ref)[0]
	assertActionBody(t, reply.Body, run, "reply-1", marker.ActionKindThreadReply, "")
	inline := provider.RecordedInlineComments(ref)[0]
	assertActionBody(t, inline.Body, run, "inline-1", marker.ActionKindInlineComment, "")
	if inline.CommitSHA != run.SHA {
		t.Fatalf("inline CommitSHA = %q, want %q", inline.CommitSHA, run.SHA)
	}
	rollup := provider.RecordedIssueComments(ref)[0]
	assertActionBody(t, rollup, run, "rollup-1", marker.ActionKindRollupComment, string(ledger.OutcomeRequestChanges))
	submit := provider.RecordedReviews(ref)[0]
	assertActionBody(t, submit.Body, run, "submit-1", marker.ActionKindSubmitReview, "")
	if submit.Event != review.ReviewEventRequestChanges {
		t.Fatalf("submit event = %q, want request_changes", submit.Event)
	}

	for _, action := range listActions(t, store, run.RunID) {
		if action.Status != ledger.PlannedActionPosted {
			t.Fatalf("action %s status = %s, want posted", action.ActionID, action.Status)
		}
		if action.Attempts != 1 {
			t.Fatalf("action %s attempts = %d, want 1", action.ActionID, action.Attempts)
		}
	}
}

func TestPostTreatsGitHubAppResolveThreadLimitationAsAdvisory(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()

	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventComment,
	}))
	insertAction(t, store, plannedAction(run.RunID, "resolve-1", ledger.PlannedActionResolveThread, true, "thread-1", ResolveThreadPayload{}))
	provider.SetError(gitprovider.OperationResolveThread,
		gitprovider.WrapError(gitprovider.ErrThreadResolutionUnsupported, gitprovider.OperationResolveThread, errors.New("github graphql: GitHub App integrations cannot resolve review threads (resource not accessible by integration)")))

	result, err := Post(context.Background(), Options{
		Store:    store,
		Provider: provider,
		Limiter:  noopLimiter{},
		Now:      fixedClock(),
	}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeComment,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK || result.Outcome != ledger.OutcomeComment {
		t.Fatalf("Post result = %#v, want comment exit 0", result)
	}
	action := actionByID(t, store, run.RunID, "resolve-1")
	if action.Status != ledger.PlannedActionPending {
		t.Fatalf("resolve action status = %s, want pending", action.Status)
	}
	if action.FailureClass == nil || *action.FailureClass != ledger.PlannedActionFailureClassAdvisory {
		t.Fatalf("resolve action failure class = %#v, want advisory", action.FailureClass)
	}
}

func TestPostBundlesInlineCommentsIntoSubmitReviewWhenProviderSupportsIt(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{BundleInlineOnSubmit: true})
	ref := testPRRef()

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))

	result, err := Post(context.Background(), Options{
		Store:    store,
		Provider: provider,
		Limiter:  noopLimiter{},
		Now:      fixedClock(),
	}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeRequestChanges || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want request_changes exit 0", result)
	}
	if !reflect.DeepEqual(provider.writes, []string{"PostIssueComment", "SubmitReview"}) {
		t.Fatalf("provider write order = %#v, want rollup then submit only", provider.writes)
	}
	if got := provider.RecordedInlineComments(ref); len(got) != 0 {
		t.Fatalf("RecordedInlineComments = %#v, want none when bundled", got)
	}
	reviews := provider.RecordedReviews(ref)
	if len(reviews) != 1 {
		t.Fatalf("RecordedReviews len = %d, want 1", len(reviews))
	}
	if len(reviews[0].Comments) != 1 {
		t.Fatalf("bundled review comments = %#v, want one", reviews[0].Comments)
	}
	assertActionBody(t, reviews[0].Comments[0].Body, run, "inline-1", marker.ActionKindInlineComment, "")
	for _, id := range []string{"inline-1", "rollup-1", "submit-1"} {
		action := actionByID(t, store, run.RunID, id)
		if action.Status != ledger.PlannedActionPosted {
			t.Fatalf("action %s status = %s, want posted", action.ActionID, action.Status)
		}
	}
}

func TestPostReconcilesBundledInlineCommentsFromSubmitReview(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{BundleInlineOnSubmit: true})
	ref := testPRRef()

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))

	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{
		ID:     gitprovider.CommentID("issue-rollup"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "rollup-1",
			Kind:     marker.ActionKindRollupComment,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
			Outcome:  string(ledger.OutcomeRequestChanges),
		}),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
	if err := provider.SetReviews(ref, []gitprovider.Review{{
		ID:     gitprovider.ReviewID("review-submit"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "submit-1",
			Kind:     marker.ActionKindSubmitReview,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
		}),
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{inlineThreadWithAction(t, run, "inline-1")}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK {
		t.Fatalf("Post exit = %d, want 0", result.ExitCode)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want reconciliation only", provider.writes)
	}
	for _, tc := range []struct {
		id       string
		upstream string
	}{
		{id: "submit-1", upstream: "review-submit"},
		{id: "rollup-1", upstream: "issue-rollup"},
		{id: "inline-1", upstream: "comment-inline-1"},
	} {
		action := actionByID(t, store, run.RunID, tc.id)
		if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != tc.upstream {
			t.Fatalf("%s after reconciliation = %#v, want posted upstream %s", tc.id, action, tc.upstream)
		}
	}
}

func TestPostConflictReconcilesBundledInlineCommentsFromSubmitReview(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{BundleInlineOnSubmit: true})
	ref := testPRRef()

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))

	provider.SetError(gitprovider.OperationSubmitReview, gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationSubmitReview, nil))
	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{
		ID:     gitprovider.CommentID("issue-rollup"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "rollup-1",
			Kind:     marker.ActionKindRollupComment,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
			Outcome:  string(ledger.OutcomeRequestChanges),
		}),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
	if err := provider.SetReviews(ref, []gitprovider.Review{{
		ID:     gitprovider.ReviewID("review-submit"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "submit-1",
			Kind:     marker.ActionKindSubmitReview,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
		}),
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{inlineThreadWithAction(t, run, "inline-1")}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK {
		t.Fatalf("Post exit = %d, want 0", result.ExitCode)
	}
	for _, tc := range []struct {
		id       string
		upstream string
	}{
		{id: "submit-1", upstream: "review-submit"},
		{id: "rollup-1", upstream: "issue-rollup"},
		{id: "inline-1", upstream: "comment-inline-1"},
	} {
		action := actionByID(t, store, run.RunID, tc.id)
		if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != tc.upstream {
			t.Fatalf("%s after conflict reconciliation = %#v, want posted upstream %s", tc.id, action, tc.upstream)
		}
	}
}

func TestPostRepairsBundledInlineCommentsWhenSubmitReviewAlreadyPosted(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{BundleInlineOnSubmit: true})
	ref := testPRRef()

	submit := plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	})
	submit.Status = ledger.PlannedActionPosted
	submit.PostedAt = strPtrTime(testTime())
	submit.UpstreamID = strPtr("review-submit")
	insertAction(t, store, submit)
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{inlineThreadWithAction(t, run, "inline-1")}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK || result.Pending != 0 {
		t.Fatalf("Post result = %#v, want successful repaired completion", result)
	}
	action := actionByID(t, store, run.RunID, "inline-1")
	if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != "comment-inline-1" {
		t.Fatalf("inline after repair = %#v, want posted with comment-inline-1", action)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want no additional writes", provider.writes)
	}
}

func TestPostDoesNotRepairBundledInlineWithoutThreadComment(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{BundleInlineOnSubmit: true})
	ref := testPRRef()

	submit := plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventRequestChanges,
	})
	submit.Status = ledger.PlannedActionPosted
	submit.PostedAt = strPtrTime(testTime())
	submit.UpstreamID = strPtr("review-submit")
	insertAction(t, store, submit)
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeRequestChanges,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK || result.Pending != 1 {
		t.Fatalf("Post result = %#v, want one pending bundled inline action", result)
	}
	action := actionByID(t, store, run.RunID, "inline-1")
	if action.Status != ledger.PlannedActionPending || action.UpstreamID != nil {
		t.Fatalf("inline after repair = %#v, want still pending without thread comment", action)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want no additional writes", provider.writes)
	}
}

func TestPostThreadSummaryReplyUsesSummaryMarker(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	insertAction(t, store, plannedAction(run.RunID, "summary-1", ledger.PlannedActionThreadReply, false, "thread-1", ThreadReplyPayload{
		Body:    "summary <!-- codereview:skip --> body",
		Summary: true,
	}))

	_, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeComment,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	body := provider.RecordedThreadReplies(ref)[0].Body
	if strings.Contains(body, marker.RenderSkip()) {
		t.Fatalf("thread-summary body contains skip marker: %q", body)
	}
	if actions := marker.FindActions(body); len(actions) != 0 {
		t.Fatalf("thread-summary body action markers = %#v, want none", actions)
	}
	summaries := marker.FindThreadSummaries(body)
	if len(summaries) != 1 || summaries[0].RunID != run.RunID || summaries[0].ActionID != "summary-1" {
		t.Fatalf("thread-summary markers = %#v, want summary-1", summaries)
	}
	if strings.Contains(body, "<!-- codereview:skip -->") {
		t.Fatalf("thread-summary model content was not sanitized: %q", body)
	}
}

func TestPostReconcilesMarkersOnlyFromPostingIdentity(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	action := plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, true, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	})
	insertAction(t, store, action)

	markerText := mustRenderAction(t, marker.ActionMarker{
		RunID:    run.RunID,
		ActionID: action.ActionID,
		Kind:     marker.ActionKindInlineComment,
		SHA:      run.SHA,
		BaseSHA:  run.BaseSHA,
	})
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{{
		ID: gitprovider.ThreadID("thread-1"),
		Comments: []gitprovider.ThreadComment{
			{ID: gitprovider.CommentID("comment-other"), Author: gitprovider.Identity{Login: "other", ID: "other-id"}, Body: markerText},
			{ID: gitprovider.CommentID("comment-bot"), Author: botIdentity(), Body: markerText},
		},
	}}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, Request{
		Run:             run,
		PRRef:           ref,
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeComment,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK {
		t.Fatalf("Post exit = %d, want 0", result.ExitCode)
	}
	if len(provider.RecordedInlineComments(ref)) != 0 {
		t.Fatalf("Post wrote inline comment despite reconciliation")
	}
	got := actionByID(t, store, run.RunID, "inline-1")
	if got.Status != ledger.PlannedActionPosted || got.UpstreamID == nil || *got.UpstreamID != "comment-bot" {
		t.Fatalf("reconciled action = %#v, want posted with comment-bot", got)
	}
}

func TestPostReconcilesPreexistingActionsAndPostsOnlyMissingAction(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	limiter := &spyLimiter{}

	insertAction(t, store, plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, true, "thread-1", ThreadReplyPayload{Body: "reply body"}))
	insertAction(t, store, plannedAction(run.RunID, "summary-1", ledger.PlannedActionThreadReply, true, "thread-2", ThreadReplyPayload{Body: "summary body", Summary: true}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventComment,
	}))
	postedReply := plannedAction(run.RunID, "reply-resolved", ledger.PlannedActionThreadReply, false, "thread-resolved", ThreadReplyPayload{Body: "posted reply"})
	postedReply.Status = ledger.PlannedActionPosted
	postedReply.PostedAt = strPtrTime(testTime())
	postedReply.UpstreamID = strPtr("comment-resolved-reply")
	insertAction(t, store, postedReply)
	insertAction(t, store, plannedAction(run.RunID, "resolve-1", ledger.PlannedActionResolveThread, true, "thread-resolved", ResolveThreadPayload{}))
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, true, "", InlineCommentPayload{
		Body:        "inline body",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}))

	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{
		{
			ID: gitprovider.ThreadID("thread-1"),
			Comments: []gitprovider.ThreadComment{{
				ID:     gitprovider.CommentID("comment-reply"),
				Author: botIdentity(),
				Body: mustRenderAction(t, marker.ActionMarker{
					RunID:    run.RunID,
					ActionID: "reply-1",
					Kind:     marker.ActionKindThreadReply,
					SHA:      run.SHA,
					BaseSHA:  run.BaseSHA,
				}),
			}},
		},
		{
			ID: gitprovider.ThreadID("thread-2"),
			Comments: []gitprovider.ThreadComment{{
				ID:     gitprovider.CommentID("comment-summary"),
				Author: botIdentity(),
				Body:   mustRenderThreadSummary(t, marker.ThreadSummaryMarker{RunID: run.RunID, ActionID: "summary-1"}),
			}},
		},
		{ID: gitprovider.ThreadID("thread-resolved"), Resolved: true},
	}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{
		ID:     gitprovider.CommentID("issue-rollup"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "rollup-1",
			Kind:     marker.ActionKindRollupComment,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
			Outcome:  string(ledger.OutcomeComment),
		}),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
	if err := provider.SetReviews(ref, []gitprovider.Review{{
		ID:     gitprovider.ReviewID("review-submit"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "submit-1",
			Kind:     marker.ActionKindSubmitReview,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
		}),
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want comment exit 0", result)
	}
	if !reflect.DeepEqual(provider.writes, []string{"PostInlineComment"}) {
		t.Fatalf("provider writes = %#v, want only missing inline write", provider.writes)
	}
	if limiter.calls != 1 {
		t.Fatalf("limiter calls = %d, want one for missing inline", limiter.calls)
	}

	wantUpstreams := map[string]string{
		"reply-1":   "comment-reply",
		"summary-1": "comment-summary",
		"rollup-1":  "issue-rollup",
		"submit-1":  "review-submit",
		"resolve-1": "thread-resolved",
	}
	for actionID, upstreamID := range wantUpstreams {
		action := actionByID(t, store, run.RunID, actionID)
		if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != upstreamID {
			t.Fatalf("%s after reconciliation = %#v, want posted upstream %s", actionID, action, upstreamID)
		}
	}
	if got := actionByID(t, store, run.RunID, "inline-1"); got.Status != ledger.PlannedActionPosted || got.Attempts != 1 {
		t.Fatalf("inline after missing write = %#v, want posted with one attempt", got)
	}
}

func TestPostReconcilesNonInlineMarkersOnlyFromPostingIdentity(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	limiter := &spyLimiter{}

	insertAction(t, store, plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, true, "thread-1", ThreadReplyPayload{Body: "reply body"}))
	insertAction(t, store, plannedAction(run.RunID, "summary-1", ledger.PlannedActionThreadReply, true, "thread-2", ThreadReplyPayload{Body: "summary body", Summary: true}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup body"}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventComment,
	}))

	replyMarker := mustRenderAction(t, marker.ActionMarker{
		RunID:    run.RunID,
		ActionID: "reply-1",
		Kind:     marker.ActionKindThreadReply,
		SHA:      run.SHA,
		BaseSHA:  run.BaseSHA,
	})
	summaryMarker := mustRenderThreadSummary(t, marker.ThreadSummaryMarker{RunID: run.RunID, ActionID: "summary-1"})
	rollupMarker := mustRenderAction(t, marker.ActionMarker{
		RunID:    run.RunID,
		ActionID: "rollup-1",
		Kind:     marker.ActionKindRollupComment,
		SHA:      run.SHA,
		BaseSHA:  run.BaseSHA,
		Outcome:  string(ledger.OutcomeComment),
	})
	submitMarker := mustRenderAction(t, marker.ActionMarker{
		RunID:    run.RunID,
		ActionID: "submit-1",
		Kind:     marker.ActionKindSubmitReview,
		SHA:      run.SHA,
		BaseSHA:  run.BaseSHA,
	})
	other := gitprovider.Identity{Login: "other", ID: "other-id"}

	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{
		{
			ID: gitprovider.ThreadID("thread-1"),
			Comments: []gitprovider.ThreadComment{
				{ID: gitprovider.CommentID("comment-other-reply"), Author: other, Body: replyMarker},
				{ID: gitprovider.CommentID("comment-bot-reply"), Author: botIdentity(), Body: replyMarker},
			},
		},
		{
			ID: gitprovider.ThreadID("thread-2"),
			Comments: []gitprovider.ThreadComment{
				{ID: gitprovider.CommentID("comment-other-summary"), Author: other, Body: summaryMarker},
				{ID: gitprovider.CommentID("comment-bot-summary"), Author: botIdentity(), Body: summaryMarker},
			},
		},
	}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{
		{ID: gitprovider.CommentID("issue-other-rollup"), Author: other, Body: rollupMarker},
		{ID: gitprovider.CommentID("issue-bot-rollup"), Author: botIdentity(), Body: rollupMarker},
	}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
	if err := provider.SetReviews(ref, []gitprovider.Review{
		{ID: gitprovider.ReviewID("review-other-submit"), Author: other, Body: submitMarker},
		{ID: gitprovider.ReviewID("review-bot-submit"), Author: botIdentity(), Body: submitMarker},
	}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want comment exit 0", result)
	}
	if len(provider.writes) != 0 || limiter.calls != 0 {
		t.Fatalf("writes = %#v limiter calls = %d, want no writes", provider.writes, limiter.calls)
	}

	wantUpstreams := map[string]string{
		"reply-1":   "comment-bot-reply",
		"summary-1": "comment-bot-summary",
		"rollup-1":  "issue-bot-rollup",
		"submit-1":  "review-bot-submit",
	}
	for actionID, upstreamID := range wantUpstreams {
		action := actionByID(t, store, run.RunID, actionID)
		if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != upstreamID {
			t.Fatalf("%s after reconciliation = %#v, want posted upstream %s", actionID, action, upstreamID)
		}
	}
}

func TestPostValidationRejectsBadWiringBeforeSideEffects(t *testing.T) {
	run := testRun(ledger.PostModeLive)
	tests := []struct {
		name string
		opts Options
		req  Request
	}{
		{name: "nil store", opts: Options{Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: testRequest(run)},
		{name: "nil provider", opts: Options{Store: &countingStore{}, Limiter: noopLimiter{}}, req: testRequest(run)},
		{name: "nil limiter", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}}, req: testRequest(run)},
		{name: "invalid pr ref", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.PRRef.Host = "" })},
		{name: "missing run id", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.Run.RunID = "" })},
		{name: "missing sha", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.Run.SHA = "" })},
		{name: "missing base sha", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.Run.BaseSHA = "" })},
		{name: "dry run", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.Run.PostMode = ledger.PostModeDryRun })},
		{name: "empty identity", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.PostingIdentity = gitprovider.Identity{} })},
		{name: "bad desired outcome", opts: Options{Store: &countingStore{}, Provider: &gitprovider.Fake{}, Limiter: noopLimiter{}}, req: requestWith(testRequest(run), func(r *Request) { r.DesiredOutcome = ledger.OutcomeFailed })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Post(context.Background(), tt.opts, tt.req)
			if err == nil {
				t.Fatal("Post error = nil, want validation error")
			}
			if store, ok := tt.opts.Store.(*countingStore); ok && store.listCalls != 0 {
				t.Fatalf("ListPlannedActions calls = %d, want 0", store.listCalls)
			}
		})
	}
}

func TestPostSkipsPlannedOnlyRowsDuringIOAndFinalization(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	action := plannedAction(run.RunID, "planned-only-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"})
	action.Status = ledger.PlannedActionPlannedOnly
	insertAction(t, store, action)

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK || result.Pending != 0 || result.Posted != 0 || result.FailedTerminal != 0 {
		t.Fatalf("Post result = %#v, want success with no counted actions", result)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want none", provider.writes)
	}
	if len(provider.reads) != 0 {
		t.Fatalf("provider reads = %#v, want none", provider.reads)
	}
}

func TestPostFinalizationIgnoresOptionalFailuresAndPlannedOnlyRows(t *testing.T) {
	t.Run("optional terminal failure is non-fatal", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		provider := newRecordingProvider()
		limiter := &spyLimiter{}
		insertAction(t, store, ledger.PlannedAction{
			ActionID:    "optional-bad-json",
			RunID:       run.RunID,
			Kind:        ledger.PlannedActionRollupComment,
			PlannedAt:   testTime(),
			PayloadJSON: `{bad-json`,
			Status:      ledger.PlannedActionPending,
			Required:    false,
		})

		result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK || result.FailedTerminal != 1 {
			t.Fatalf("Post result = %#v, want comment exit 0 with optional terminal failure", result)
		}
		if limiter.calls != 0 || len(provider.writes) != 0 {
			t.Fatalf("limiter calls = %d writes = %#v, want none", limiter.calls, provider.writes)
		}
		assertRunOutcome(t, store, run.RunID, ptrOutcome(ledger.OutcomeComment))
	})

	t.Run("required malformed planned-only row is excluded", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		provider := newRecordingProvider()
		action := ledger.PlannedAction{
			ActionID:    "planned-only-bad-json",
			RunID:       run.RunID,
			Kind:        ledger.PlannedActionRollupComment,
			PlannedAt:   testTime(),
			PayloadJSON: `{bad-json`,
			Status:      ledger.PlannedActionPlannedOnly,
			Required:    true,
		}
		insertAction(t, store, action)

		result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK || result.FailedTerminal != 0 {
			t.Fatalf("Post result = %#v, want planned-only excluded from finalization", result)
		}
		if len(provider.reads) != 0 || len(provider.writes) != 0 {
			t.Fatalf("provider reads/writes = %#v/%#v, want none", provider.reads, provider.writes)
		}
		assertRunOutcome(t, store, run.RunID, ptrOutcome(ledger.OutcomeComment))
	})
}

func TestPostReadFailureDoesNotWriteOrFinalize(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetError(gitprovider.OperationListIssueComments, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListIssueComments, nil))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err == nil {
		t.Fatal("Post error = nil, want read error")
	}
	if result.ExitCode != exitUpstream {
		t.Fatalf("Post exit = %d, want %d", result.ExitCode, exitUpstream)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want none", provider.writes)
	}
	assertRunOutcome(t, store, run.RunID, nil)
	action := actionByID(t, store, run.RunID, "rollup-1")
	if action.Status != ledger.PlannedActionPending || action.Error != nil {
		t.Fatalf("action after read failure = %#v, want untouched pending", action)
	}
}

func TestPostLimiterErrorStopsBeforeLaterActionsAndDoesNotFinalize(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	limiterErr := errors.New("limiter unavailable")
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, false, "", InlineCommentPayload{
		Body:        "inline",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        1,
		SubjectType: review.AnchorKindLine,
	}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: errorLimiter{err: limiterErr}, Now: fixedClock()}, testRequest(run))
	if !errors.Is(err, limiterErr) {
		t.Fatalf("Post error = %v, want limiter error", err)
	}
	if result.ExitCode != exitUpstream || result.Pending != 2 {
		t.Fatalf("Post result = %#v, want exit 5 with two pending", result)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want none", provider.writes)
	}
	assertRunOutcome(t, store, run.RunID, nil)
	first := actionByID(t, store, run.RunID, "inline-1")
	if first.Attempts != 0 || first.Error == nil {
		t.Fatalf("first action after limiter error = %#v, want pending error without attempt", first)
	}
	later := actionByID(t, store, run.RunID, "rollup-1")
	if later.Attempts != 0 || later.Error != nil {
		t.Fatalf("later action after limiter error = %#v, want untouched pending", later)
	}
}

func TestPostLocalValidationFailsTerminalWithoutLimiterOrProvider(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	limiter := &spyLimiter{}
	insertAction(t, store, ledger.PlannedAction{
		ActionID:    "bad-json",
		RunID:       run.RunID,
		Kind:        ledger.PlannedActionRollupComment,
		PlannedAt:   testTime(),
		PayloadJSON: `{bad-json`,
		Status:      ledger.PlannedActionPending,
		Required:    true,
	})

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeFailed || result.ExitCode != exitFailed {
		t.Fatalf("Post result = %#v, want failed exit 1", result)
	}
	if limiter.calls != 0 || len(provider.writes) != 0 {
		t.Fatalf("limiter calls = %d writes = %#v, want none", limiter.calls, provider.writes)
	}
	action := actionByID(t, store, run.RunID, "bad-json")
	if action.Status != ledger.PlannedActionFailedTerminal || action.Attempts != 0 || action.Error == nil {
		t.Fatalf("bad action = %#v, want failed terminal without attempts", action)
	}
}

func TestPostLocalValidationFailsTerminalWithoutLimiterOrProviderByActionKind(t *testing.T) {
	tests := []struct {
		name   string
		action ledger.PlannedAction
	}{
		{
			name: "inline invalid provider request",
			action: plannedAction("run-1", "inline-1", ledger.PlannedActionInlineComment, true, "", InlineCommentPayload{
				Body:        "inline",
				Side:        review.DiffSideRight,
				Line:        1,
				SubjectType: review.AnchorKindLine,
			}),
		},
		{
			name:   "thread reply missing target",
			action: plannedAction("run-1", "reply-1", ledger.PlannedActionThreadReply, true, "", ThreadReplyPayload{Body: "reply"}),
		},
		{
			name: "resolve malformed json",
			action: ledger.PlannedAction{
				ActionID:    "resolve-1",
				RunID:       "run-1",
				Kind:        ledger.PlannedActionResolveThread,
				ThreadID:    strPtr("thread-1"),
				PlannedAt:   testTime(),
				PayloadJSON: `{bad-json`,
				Status:      ledger.PlannedActionPending,
				Required:    true,
			},
		},
		{
			name:   "rollup missing body",
			action: plannedAction("run-1", "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "   "}),
		},
		{
			name: "submit invalid provider request",
			action: plannedAction("run-1", "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
				Body:  "review",
				Event: review.ReviewEvent("invalid"),
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			run := allocateRun(t, store, ledger.PostModeLive)
			provider := newRecordingProvider()
			limiter := &spyLimiter{}
			tt.action.RunID = run.RunID
			insertAction(t, store, tt.action)

			result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if result.Outcome != ledger.OutcomeFailed || result.ExitCode != exitFailed {
				t.Fatalf("Post result = %#v, want failed exit 1", result)
			}
			if limiter.calls != 0 || len(provider.writes) != 0 {
				t.Fatalf("limiter calls = %d writes = %#v, want none", limiter.calls, provider.writes)
			}
			action := actionByID(t, store, run.RunID, tt.action.ActionID)
			if action.Status != ledger.PlannedActionFailedTerminal || action.Attempts != 0 || action.Error == nil {
				t.Fatalf("bad action = %#v, want failed terminal without attempts", action)
			}
		})
	}
}

func TestPostTypedProviderFailuresFinalizeByRequiredActionState(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome ledger.Outcome
		wantExit    int
		wantStatus  ledger.PlannedActionStatus
		wantClass   *string
	}{
		{
			name:        "retryable pending incomplete",
			err:         gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationPostIssueComment, nil),
			wantOutcome: ledger.OutcomeIncomplete,
			wantExit:    exitUpstream,
			wantStatus:  ledger.PlannedActionPending,
			wantClass:   nil,
		},
		{
			name:        "auth terminal exit auth",
			err:         gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationPostIssueComment, nil),
			wantOutcome: ledger.OutcomeFailed,
			wantExit:    exitAuth,
			wantStatus:  ledger.PlannedActionFailedTerminal,
			wantClass:   strPtr(ledger.PlannedActionFailureClassAuth),
		},
		{
			name:        "auth chain terminal exit auth",
			err:         terseAuthError{},
			wantOutcome: ledger.OutcomeFailed,
			wantExit:    exitAuth,
			wantStatus:  ledger.PlannedActionFailedTerminal,
			wantClass:   strPtr(ledger.PlannedActionFailureClassAuth),
		},
		{
			name:        "untyped terminal exit failed",
			err:         errors.New("validation rejected"),
			wantOutcome: ledger.OutcomeFailed,
			wantExit:    exitFailed,
			wantStatus:  ledger.PlannedActionFailedTerminal,
			wantClass:   strPtr(ledger.PlannedActionFailureClassTerminal),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			run := allocateRun(t, store, ledger.PostModeLive)
			provider := newRecordingProvider()
			provider.SetError(gitprovider.OperationPostIssueComment, tt.err)
			insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

			result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if result.Outcome != tt.wantOutcome || result.ExitCode != tt.wantExit {
				t.Fatalf("Post result = %#v, want %s exit %d", result, tt.wantOutcome, tt.wantExit)
			}
			action := actionByID(t, store, run.RunID, "rollup-1")
			if action.Status != tt.wantStatus || action.Attempts != 1 || action.Error == nil || !sameStringPtr(action.FailureClass, tt.wantClass) {
				t.Fatalf("action = %#v, want %s with one attempt and error", action, tt.wantStatus)
			}
		})
	}
}

func TestPostPersistedAuthFailureClassDrivesRerunExitAuth(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	action := plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"})
	action.Status = ledger.PlannedActionFailedTerminal
	action.Error = strPtr("login expired")
	action.FailureClass = strPtr(ledger.PlannedActionFailureClassAuth)
	insertAction(t, store, action)

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeFailed || result.ExitCode != exitAuth {
		t.Fatalf("Post result = %#v, want failed exit auth", result)
	}
	if len(provider.reads) != 0 || len(provider.writes) != 0 {
		t.Fatalf("provider reads/writes = %#v/%#v, want none", provider.reads, provider.writes)
	}
}

func TestSortActionsPreservesEqualKeyOrder(t *testing.T) {
	actions := []ledger.PlannedAction{
		{RunID: "first", ActionID: "same", Kind: ledger.PlannedActionRollupComment},
		{RunID: "second", ActionID: "same", Kind: ledger.PlannedActionRollupComment},
	}

	sorted := sortActions(actions)
	if sorted[0].RunID != "first" || sorted[1].RunID != "second" {
		t.Fatalf("equal-key order = %q, %q; want first, second", sorted[0].RunID, sorted[1].RunID)
	}
}

func TestPostStaleSHAAbortsRunAndLeavesLaterActions(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetError(gitprovider.OperationPostIssueComment, gitprovider.WrapError(gitprovider.ErrStaleSHA, gitprovider.OperationPostIssueComment, nil))
	insertAction(t, store, plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, false, "thread-1", ThreadReplyPayload{Body: "reply"}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !result.Aborted || result.Outcome != ledger.OutcomeAborted || result.ExitCode != exitUpstream {
		t.Fatalf("Post result = %#v, want aborted exit 5", result)
	}
	assertRunOutcome(t, store, run.RunID, ptrOutcome(ledger.OutcomeAborted))
	if got := actionByID(t, store, run.RunID, "reply-1"); got.Status != ledger.PlannedActionPosted {
		t.Fatalf("reply status = %s, want posted", got.Status)
	}
	if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionPending || got.Attempts != 1 || got.Error == nil {
		t.Fatalf("rollup after stale SHA = %#v, want pending with error", got)
	}
	if got := actionByID(t, store, run.RunID, "submit-1"); got.Status != ledger.PlannedActionPending || got.Attempts != 0 {
		t.Fatalf("submit after stale SHA = %#v, want untouched pending", got)
	}
}

func TestPostConflictRefetchFailureStopsBeforeLaterActions(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.postIssueComment = func(context.Context, string) (gitprovider.CommentID, error) {
		provider.SetError(gitprovider.OperationListIssueComments, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListIssueComments, nil))
		return "", gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationPostIssueComment, nil)
	}
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err == nil {
		t.Fatal("Post error = nil, want refetch error")
	}
	if result.ExitCode != exitUpstream {
		t.Fatalf("Post result = %#v, want exit 5", result)
	}
	assertRunOutcome(t, store, run.RunID, nil)
	if !reflect.DeepEqual(provider.writes, []string{"PostIssueComment"}) {
		t.Fatalf("provider writes = %#v, want only rollup attempt", provider.writes)
	}
	if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionPending || got.Attempts != 1 || got.Error == nil {
		t.Fatalf("rollup after refetch failure = %#v, want pending with error", got)
	}
	if got := actionByID(t, store, run.RunID, "submit-1"); got.Status != ledger.PlannedActionPending || got.Attempts != 0 || got.Error != nil {
		t.Fatalf("submit after refetch failure = %#v, want untouched pending", got)
	}
}

func TestPostConflictFallback(t *testing.T) {
	t.Run("fresh predicate satisfied", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		provider := newRecordingProvider()
		ref := testPRRef()
		action := plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"})
		insertAction(t, store, action)
		provider.postIssueComment = func(_ context.Context, body string) (gitprovider.CommentID, error) {
			if !strings.Contains(body, "action=rollup-1") {
				t.Fatalf("posted body = %q, want rollup marker", body)
			}
			markerText := mustRenderAction(t, marker.ActionMarker{
				RunID:    run.RunID,
				ActionID: action.ActionID,
				Kind:     marker.ActionKindRollupComment,
				SHA:      run.SHA,
				BaseSHA:  run.BaseSHA,
				Outcome:  string(ledger.OutcomeComment),
			})
			if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{ID: gitprovider.CommentID("issue-1"), Author: botIdentity(), Body: markerText}}); err != nil {
				t.Fatalf("SetIssueComments: %v", err)
			}
			return "", gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationPostIssueComment, nil)
		}

		result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if result.ExitCode != exitOK {
			t.Fatalf("Post result = %#v, want success", result)
		}
		if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionPosted || got.UpstreamID == nil || *got.UpstreamID != "issue-1" {
			t.Fatalf("rollup after conflict reconcile = %#v, want posted issue-1", got)
		}
	})

	t.Run("wrapped retryable remains pending", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		provider := newRecordingProvider()
		provider.SetError(gitprovider.OperationPostIssueComment, gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationPostIssueComment, gitprovider.ErrRetryable))
		insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

		result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if result.Outcome != ledger.OutcomeIncomplete || result.ExitCode != exitUpstream {
			t.Fatalf("Post result = %#v, want incomplete exit 5", result)
		}
		if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionPending {
			t.Fatalf("rollup status = %s, want pending", got.Status)
		}
	})

	t.Run("bare conflict fails terminal", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		provider := newRecordingProvider()
		provider.SetError(gitprovider.OperationPostIssueComment, gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationPostIssueComment, nil))
		insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

		result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if result.Outcome != ledger.OutcomeFailed || result.ExitCode != exitFailed {
			t.Fatalf("Post result = %#v, want failed exit 1", result)
		}
		if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionFailedTerminal {
			t.Fatalf("rollup status = %s, want failed_terminal", got.Status)
		}
	})

	for _, tt := range []struct {
		name     string
		cause    error
		wantExit int
	}{
		{name: "wrapped auth fails terminal with auth exit", cause: gitprovider.ErrAuth, wantExit: exitAuth},
		{name: "wrapped permission fails terminal with auth exit", cause: gitprovider.ErrPermission, wantExit: exitAuth},
		{name: "wrapped not found fails terminal", cause: gitprovider.ErrNotFound, wantExit: exitFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			run := allocateRun(t, store, ledger.PostModeLive)
			provider := newRecordingProvider()
			provider.SetError(gitprovider.OperationPostIssueComment, gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationPostIssueComment, tt.cause))
			insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

			result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if result.Outcome != ledger.OutcomeFailed || result.ExitCode != tt.wantExit {
				t.Fatalf("Post result = %#v, want failed exit %d", result, tt.wantExit)
			}
			if !reflect.DeepEqual(provider.writes, []string{"PostIssueComment"}) {
				t.Fatalf("provider writes = %#v, want one rollup attempt", provider.writes)
			}
			if got := actionByID(t, store, run.RunID, "rollup-1"); got.Status != ledger.PlannedActionFailedTerminal || got.Attempts != 1 || got.Error == nil {
				t.Fatalf("rollup status = %#v, want failed_terminal with one attempt", got)
			}
		})
	}
}

func TestPostResolveThreadDoesNotReconcileWithoutPostedSameRunReply(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	provider.SetError(gitprovider.OperationResolveThread, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationResolveThread, nil))
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{{ID: gitprovider.ThreadID("thread-1"), Resolved: true}}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	insertAction(t, store, plannedAction(run.RunID, "resolve-1", ledger.PlannedActionResolveThread, true, "thread-1", ResolveThreadPayload{}))

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeIncomplete || result.ExitCode != exitUpstream {
		t.Fatalf("Post result = %#v, want incomplete exit 5", result)
	}
	if !reflect.DeepEqual(provider.writes, []string{"ResolveThread"}) {
		t.Fatalf("provider writes = %#v, want resolve attempt", provider.writes)
	}
	action := actionByID(t, store, run.RunID, "resolve-1")
	if action.Status != ledger.PlannedActionPending || action.Attempts != 1 || action.Error == nil {
		t.Fatalf("resolve action = %#v, want pending after retryable resolve attempt", action)
	}
}

func TestPostResolveMalformedPayloadFailsBeforeReconciliation(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	reply := plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, false, "thread-1", ThreadReplyPayload{Body: "posted reply"})
	reply.Status = ledger.PlannedActionPosted
	reply.PostedAt = strPtrTime(testTime())
	reply.UpstreamID = strPtr("comment-reply")
	insertAction(t, store, reply)
	insertAction(t, store, ledger.PlannedAction{
		ActionID:    "resolve-1",
		RunID:       run.RunID,
		Kind:        ledger.PlannedActionResolveThread,
		ThreadID:    strPtr("thread-1"),
		PlannedAt:   testTime(),
		PayloadJSON: `{bad-json`,
		Status:      ledger.PlannedActionPending,
		Required:    true,
	})
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{{ID: gitprovider.ThreadID("thread-1"), Resolved: true}}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeFailed || result.ExitCode != exitFailed {
		t.Fatalf("Post result = %#v, want failed exit 1", result)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want none", provider.writes)
	}
	action := actionByID(t, store, run.RunID, "resolve-1")
	if action.Status != ledger.PlannedActionFailedTerminal || action.Attempts != 0 || action.Error == nil || !sameStringPtr(action.FailureClass, strPtr(ledger.PlannedActionFailureClassTerminal)) {
		t.Fatalf("resolve action = %#v, want failed terminal without attempts", action)
	}
}

func TestTokenBucketValidationAndCancellation(t *testing.T) {
	if _, err := NewTokenBucket(0, 1); err == nil {
		t.Fatal("NewTokenBucket zero interval error = nil")
	}
	if _, err := NewTokenBucket(time.Millisecond, 0); err == nil {
		t.Fatal("NewTokenBucket zero burst error = nil")
	}
	bucket, err := NewTokenBucket(50*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	if err := bucket.Wait(context.Background(), "github.com"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bucket.Wait(ctx, "github.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Wait error = %v, want context.Canceled", err)
	}
}

func TestTokenBucketIsHostKeyedAndSpacesSameHost(t *testing.T) {
	interval := 40 * time.Millisecond
	bucket, err := NewTokenBucket(interval, 1)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := bucket.Wait(ctx, "github.com"); err != nil {
		t.Fatalf("first github Wait: %v", err)
	}
	otherCtx, otherCancel := context.WithTimeout(ctx, interval/4)
	defer otherCancel()
	if err := bucket.Wait(otherCtx, "gitlab.com"); err != nil {
		t.Fatalf("gitlab Wait: %v", err)
	}
	sameStart := time.Now()
	if err := bucket.Wait(ctx, "github.com"); err != nil {
		t.Fatalf("second github Wait: %v", err)
	}
	if elapsed := time.Since(sameStart); elapsed < interval/2 {
		t.Fatalf("same host Wait took %v, want at least %v", elapsed, interval/2)
	}
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

type errorLimiter struct {
	err error
}

func (l errorLimiter) Wait(context.Context, string) error { return l.err }

type spyLimiter struct {
	calls int
}

func (l *spyLimiter) Wait(context.Context, string) error {
	l.calls++
	return nil
}

type countingStore struct {
	listCalls int
}

type terseAuthError struct{}

func (terseAuthError) Error() string { return "login expired" }

func (terseAuthError) Unwrap() error { return gitprovider.ErrAuth }

func (s *countingStore) ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error) {
	s.listCalls++
	return nil, nil
}

func (*countingStore) UpdatePlannedAction(context.Context, ledger.PlannedAction) error {
	panic("UpdatePlannedAction should not be called")
}

func (*countingStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	panic("CompleteRun should not be called")
}

type recordingProvider struct {
	*gitprovider.Fake
	reads            []string
	writes           []string
	postIssueComment func(context.Context, string) (gitprovider.CommentID, error)
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{Fake: &gitprovider.Fake{}}
}

func (p *recordingProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	p.reads = append(p.reads, "ListInlineThreads")
	return p.Fake.ListInlineThreads(ctx, ref)
}

func (p *recordingProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	p.reads = append(p.reads, "ListIssueComments")
	return p.Fake.ListIssueComments(ctx, ref)
}

func (p *recordingProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	p.reads = append(p.reads, "ListReviews")
	return p.Fake.ListReviews(ctx, ref)
}

func (p *recordingProvider) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, c gitprovider.InlineComment) (gitprovider.CommentID, error) {
	p.writes = append(p.writes, "PostInlineComment")
	return p.Fake.PostInlineComment(ctx, ref, c)
}

func (p *recordingProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	p.writes = append(p.writes, "ReplyToThread")
	return p.Fake.ReplyToThread(ctx, ref, threadID, body)
}

func (p *recordingProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	p.writes = append(p.writes, "ResolveThread")
	return p.Fake.ResolveThread(ctx, ref, threadID)
}

func (p *recordingProvider) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	p.writes = append(p.writes, "PostIssueComment")
	if p.postIssueComment != nil {
		return p.postIssueComment(ctx, body)
	}
	return p.Fake.PostIssueComment(ctx, ref, body)
}

func (p *recordingProvider) SubmitReview(ctx context.Context, ref gitprovider.PRRef, r gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	p.writes = append(p.writes, "SubmitReview")
	return p.Fake.SubmitReview(ctx, ref, r)
}

func openStore(t *testing.T) *ledger.Store {
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
	return store
}

func allocateRun(t *testing.T, store *ledger.Store, mode ledger.PostMode) ledger.Run {
	t.Helper()
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           "github_open-cli_codereview-cli_19",
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/19",
		RunID:           "run-1",
		SHA:             testSHA(),
		BaseSHA:         testBaseSHA(),
		Profile:         "default",
		PostingIdentity: "bot-id",
		PostMode:        mode,
		StartedAt:       testTime(),
		ArtifactPath:    "/tmp/run-1",
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func testRun(mode ledger.PostMode) ledger.Run {
	return ledger.Run{
		RunID:           "run-1",
		PRKey:           "github_open-cli_codereview-cli_19",
		SHA:             testSHA(),
		BaseSHA:         testBaseSHA(),
		Profile:         "default",
		PostingIdentity: "bot-id",
		PostMode:        mode,
		StartedAt:       testTime(),
	}
}

func testRequest(run ledger.Run) Request {
	return Request{
		Run:             run,
		PRRef:           testPRRef(),
		PostingIdentity: botIdentity(),
		DesiredOutcome:  ledger.OutcomeComment,
	}
}

func requestWith(req Request, mutate func(*Request)) Request {
	mutate(&req)
	return req
}

func testPRRef() gitprovider.PRRef {
	return gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 19}
}

func botIdentity() gitprovider.Identity {
	return gitprovider.Identity{Login: "codereview-bot", ID: "bot-id"}
}

func inlineThreadWithAction(t *testing.T, run ledger.Run, actionID string) gitprovider.InlineThread {
	t.Helper()
	return gitprovider.InlineThread{
		ID:          gitprovider.ThreadID("thread-" + actionID),
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   run.SHA,
		Comments: []gitprovider.ThreadComment{{
			ID:          gitprovider.CommentID("comment-" + actionID),
			ThreadID:    gitprovider.ThreadID("thread-" + actionID),
			Author:      botIdentity(),
			CommitSHA:   run.SHA,
			Path:        "main.go",
			Side:        review.DiffSideRight,
			Line:        12,
			SubjectType: review.AnchorKindLine,
			Body: mustRenderAction(t, marker.ActionMarker{
				RunID:    run.RunID,
				ActionID: actionID,
				Kind:     marker.ActionKindInlineComment,
				SHA:      run.SHA,
				BaseSHA:  run.BaseSHA,
			}),
		}},
	}
}

func testTime() time.Time {
	return time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
}

func fixedClock() func() time.Time {
	now := testTime()
	return func() time.Time { return now }
}

func testSHA() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

func testBaseSHA() string {
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}

func plannedAction(runID, actionID string, kind ledger.PlannedActionKind, required bool, threadID string, payload any) ledger.PlannedAction {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	action := ledger.PlannedAction{
		ActionID:    actionID,
		RunID:       runID,
		Kind:        kind,
		PlannedAt:   testTime(),
		PayloadJSON: string(body),
		Status:      ledger.PlannedActionPending,
		Required:    required,
	}
	if threadID != "" {
		action.ThreadID = &threadID
	}
	return action
}

func insertAction(t *testing.T, store *ledger.Store, action ledger.PlannedAction) {
	t.Helper()
	if err := store.InsertPlannedAction(context.Background(), action); err != nil {
		t.Fatalf("InsertPlannedAction(%s): %v", action.ActionID, err)
	}
}

func listActions(t *testing.T, store *ledger.Store, runID string) []ledger.PlannedAction {
	t.Helper()
	actions, err := store.ListPlannedActions(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	return actions
}

func actionByID(t *testing.T, store *ledger.Store, runID, actionID string) ledger.PlannedAction {
	t.Helper()
	for _, action := range listActions(t, store, runID) {
		if action.ActionID == actionID {
			return action
		}
	}
	t.Fatalf("action %q not found", actionID)
	return ledger.PlannedAction{}
}

func assertRunOutcome(t *testing.T, store *ledger.Store, runID string, want *ledger.Outcome) {
	t.Helper()
	run, err := store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if want == nil {
		if run.Outcome != nil || run.CompletedAt != nil {
			t.Fatalf("run outcome/completed = %v/%v, want nil/nil", run.Outcome, run.CompletedAt)
		}
		return
	}
	if run.Outcome == nil || *run.Outcome != *want || run.CompletedAt == nil {
		t.Fatalf("run outcome/completed = %v/%v, want %s with completed_at", run.Outcome, run.CompletedAt, *want)
	}
}

func ptrOutcome(outcome ledger.Outcome) *ledger.Outcome {
	return &outcome
}

func strPtrTime(value time.Time) *time.Time {
	return &value
}

func sameStringPtr(got *string, want *string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func assertActionBody(t *testing.T, body string, run ledger.Run, actionID string, kind string, outcome string) {
	t.Helper()
	if !strings.Contains(body, marker.RenderSkip()) {
		t.Fatalf("body missing skip marker: %q", body)
	}
	actions := marker.FindActions(body)
	if len(actions) != 1 {
		t.Fatalf("body action markers = %#v, want one", actions)
	}
	want := marker.ActionMarker{
		RunID:    run.RunID,
		ActionID: actionID,
		Kind:     kind,
		SHA:      run.SHA,
		BaseSHA:  run.BaseSHA,
		Outcome:  outcome,
	}
	if actions[0] != want {
		t.Fatalf("action marker = %#v, want %#v", actions[0], want)
	}
}

func mustRenderAction(t *testing.T, action marker.ActionMarker) string {
	t.Helper()
	text, err := marker.RenderAction(action)
	if err != nil {
		t.Fatalf("RenderAction(%#v): %v", action, err)
	}
	return text
}

func mustRenderThreadSummary(t *testing.T, summary marker.ThreadSummaryMarker) string {
	t.Helper()
	text, err := marker.RenderThreadSummary(summary)
	if err != nil {
		t.Fatalf("RenderThreadSummary(%#v): %v", summary, err)
	}
	return text
}
