package outbox

import (
	"context"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestPostReconcilesSubmitReviewFromIssueCommentsWhenReviewSummaryAsComment(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	provider.SetCapabilities(gitprovider.ProviderCaps{ReviewSummaryAsComment: true})
	ref := testPRRef()
	limiter := &spyLimiter{}

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventComment,
	}))

	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{
		ID:     gitprovider.CommentID("note-77"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "submit-1",
			Kind:     marker.ActionKindSubmitReview,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
		}),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want comment exit 0", result)
	}
	if len(provider.writes) != 0 {
		t.Fatalf("provider writes = %#v, want none after reconciliation", provider.writes)
	}
	action := actionByID(t, store, run.RunID, "submit-1")
	if action.Status != ledger.PlannedActionPosted || action.UpstreamID == nil || *action.UpstreamID != "note-77" {
		t.Fatalf("submit-1 = %#v, want posted upstream note-77", action)
	}
}

func TestPostDoesNotReconcileSubmitReviewFromIssueCommentsWithoutCap(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	provider := newRecordingProvider()
	ref := testPRRef()
	limiter := &spyLimiter{}

	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{
		Body:  "review body",
		Event: review.ReviewEventComment,
	}))

	if err := provider.SetIssueComments(ref, []gitprovider.IssueComment{{
		ID:     gitprovider.CommentID("note-77"),
		Author: botIdentity(),
		Body: mustRenderAction(t, marker.ActionMarker{
			RunID:    run.RunID,
			ActionID: "submit-1",
			Kind:     marker.ActionKindSubmitReview,
			SHA:      run.SHA,
			BaseSHA:  run.BaseSHA,
		}),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}

	result, err := Post(context.Background(), Options{Store: store, Provider: provider, Limiter: limiter, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.Outcome != ledger.OutcomeComment || result.ExitCode != exitOK {
		t.Fatalf("Post result = %#v, want comment exit 0", result)
	}
	if !reflect.DeepEqual(provider.writes, []string{"SubmitReview"}) {
		t.Fatalf("provider writes = %#v, want fresh SubmitReview without the capability", provider.writes)
	}
}
