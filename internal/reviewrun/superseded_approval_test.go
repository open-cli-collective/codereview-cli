package reviewrun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// Issue #569: approval at A, changes requested at B, then a clean review at C.
// Exercise the real gate, action planner, ledger, and outbox together; checking
// only the rollup outcome would miss an approval posted as a COMMENT.
func TestRunSupersededApprovalPostsFreshApproval(t *testing.T) {
	for _, tt := range []struct {
		name          string
		rerun         bool
		legacyComment bool
	}{
		{name: "plain review"},
		{name: "rerun", rerun: true},
		{name: "rerun after legacy comment", rerun: true, legacyComment: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			history := supersededApprovalHistory(t, fixture)
			if tt.legacyComment {
				history = append(history, markedHistoryReview(t, fixture, "legacy-comment", fixture.pr.Head.SHA,
					gitprovider.ReviewStateCommented, testNow().Add(-time.Minute)))
			}
			if err := fixture.fake.SetReviews(fixture.ref, history); err != nil {
				t.Fatalf("SetReviews: %v", err)
			}
			planner := &cleanReviewPlanner{store: fixture.store}
			opts := fixture.opts(planner)
			opts.NewRunID = sequence("reapprove")

			if tt.legacyComment {
				// A marked COMMENT at C is a completed verdict. Recovery requires
				// --rerun, even though the older approval is no longer effective.
				prior, err := Run(ctx, opts, Request{Pipeline: fixture.req})
				if err != nil {
					t.Fatalf("Run before rerun: %v", err)
				}
				if prior.Status != gateio.StatusEarlyExit || prior.Message != "review already complete" {
					t.Fatalf("Run before rerun = %#v, want completed comment verdict", prior)
				}
				if planner.calls != 0 || len(fixture.fake.RecordedReviews(fixture.ref)) != 0 {
					t.Fatal("completed comment verdict unexpectedly planned or posted a review")
				}
			}

			result, err := Run(ctx, opts, Request{Pipeline: fixture.req, Flags: Flags{Rerun: tt.rerun}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Status != gateio.StatusContinue || result.Decision.Kind != gate.DecisionFresh || planner.calls != 1 {
				t.Fatalf("Run = %#v, planner calls = %d, want one fresh review", result, planner.calls)
			}
			if result.Pipeline == nil || result.Pipeline.Plan.Outcome != reviewplan.OutcomeApproved {
				t.Fatalf("pipeline = %#v, want clean approval plan", result.Pipeline)
			}
			assertApprovalPostedAndNextRunSkipped(t, fixture, opts, history, result)
			if planner.calls != 1 {
				t.Fatalf("planner calls = %d, want no planning after successful approval", planner.calls)
			}
		})
	}
}

func TestRunSupersededApprovalAllowsAuthorOverride(t *testing.T) {
	fixture := newFixture(t)
	history := supersededApprovalHistory(t, fixture)
	if err := fixture.fake.SetReviews(fixture.ref, history); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	if err := fixture.fake.SetIssueComments(fixture.ref, []gitprovider.IssueComment{{
		ID: "override-request", Author: fixture.pr.Author,
		Body: "These findings are low-value, please approve the pull request.", CreatedAt: testNow().Add(-time.Minute),
	}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}
	planner := &cleanReviewPlanner{store: fixture.store}
	classifier := &supersededApprovalClassifier{}
	opts := fixture.opts(planner)
	opts.NewRunID = sequence("override")
	opts.ApprovalOverride = classifier

	result, err := Run(context.Background(), opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != gateio.StatusApprovalOverride || planner.calls != 0 || classifier.calls != 1 {
		t.Fatalf("Run = %#v, planner/classifier calls = %d/%d, want approval override without full review", result, planner.calls, classifier.calls)
	}
	if len(classifier.last.Candidates) != 1 || classifier.last.Candidates[0].ID != "override-request" ||
		!classifier.last.LatestMarkerAt.Equal(testNow().Add(-2*time.Minute)) {
		t.Fatalf("classifier request = %#v, want author's request after the superseding change request", classifier.last)
	}
	assertApprovalPostedAndNextRunSkipped(t, fixture, opts, history, result)
	if planner.calls != 0 || classifier.calls != 1 {
		t.Fatalf("planner/classifier calls = %d/%d, want no work after successful approval", planner.calls, classifier.calls)
	}
}

func supersededApprovalHistory(t *testing.T, fixture *fixture) []gitprovider.Review {
	t.Helper()
	return []gitprovider.Review{
		markedHistoryReview(t, fixture, "approval-at-a", strings.Repeat("d", 40),
			gitprovider.ReviewStateApproved, testNow().Add(-3*time.Minute)),
		markedHistoryReview(t, fixture, "changes-at-b", strings.Repeat("e", 40),
			gitprovider.ReviewStateChangesRequested, testNow().Add(-2*time.Minute)),
	}
}

func markedHistoryReview(t *testing.T, fixture *fixture, id, sha string, state gitprovider.ReviewState, submittedAt time.Time) gitprovider.Review {
	t.Helper()
	body, err := marker.RenderAction(marker.ActionMarker{
		RunID: id, ActionID: id + "-submit", Kind: marker.ActionKindSubmitReview,
		SHA: sha, BaseSHA: fixture.pr.Base.SHA,
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	return gitprovider.Review{
		ID: gitprovider.ReviewID(id), Author: fixture.req.PostingIdentity, Body: body,
		State: state, CommitSHA: sha, SubmittedAt: submittedAt,
	}
}

func assertApprovalPostedAndNextRunSkipped(t *testing.T, fixture *fixture, opts Options, history []gitprovider.Review, result Result) {
	t.Helper()
	if result.ExitCode != exitOK || result.Outbox.Outcome != ledger.OutcomeApproved || result.Outbox.Posted != 1 ||
		result.Run.Outcome == nil || *result.Run.Outcome != ledger.OutcomeApproved {
		t.Fatalf("Run = %#v, want one successful approval", result)
	}
	writes := fixture.fake.RecordedReviews(fixture.ref)
	if len(writes) != 1 || writes[0].Event != review.ReviewEventApprove || writes[0].CommitSHA != fixture.pr.Head.SHA {
		t.Fatalf("review writes = %#v, want one APPROVE at current head C", writes)
	}
	markers := marker.FindActions(writes[0].Body)
	if len(markers) != 1 || markers[0].Kind != marker.ActionKindSubmitReview || markers[0].RunID != result.Run.RunID ||
		markers[0].SHA != fixture.pr.Head.SHA || markers[0].BaseSHA != fixture.pr.Base.SHA {
		t.Fatalf("review markers = %#v, want current head/base submit marker", markers)
	}
	action := actionByID(t, fixture.store, result.Run.RunID, markers[0].ActionID)
	if action.Status != ledger.PlannedActionPosted || action.SubmitReview == nil || action.SubmitReview.Event != review.ReviewEventApprove {
		t.Fatalf("stored action = %#v, want posted approval", action)
	}

	// Fake records writes separately from reads. Expose the successful write as
	// host state, retaining both earlier verdicts without dismissing either one.
	history = append(history, gitprovider.Review{
		ID: "approval-at-c", Author: fixture.req.PostingIdentity, Body: writes[0].Body,
		State: gitprovider.ReviewStateApproved, Event: writes[0].Event,
		CommitSHA: writes[0].CommitSHA, SubmittedAt: testNow(),
	})
	if err := fixture.fake.SetReviews(fixture.ref, history); err != nil {
		t.Fatalf("SetReviews after approval: %v", err)
	}
	next, err := Run(context.Background(), opts, Request{Pipeline: fixture.req})
	if err != nil {
		t.Fatalf("Run after approval: %v", err)
	}
	if next.ExitCode != exitOK || next.Status != gateio.StatusEarlyExit || next.Message != "review already approved" {
		t.Fatalf("Run after approval = %#v, want active-approval early exit", next)
	}
	if writes := fixture.fake.RecordedReviews(fixture.ref); len(writes) != 1 {
		t.Fatalf("review writes = %#v, want no duplicate approval", writes)
	}
	runs, err := fixture.store.ListRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %#v, %v, want only the successful run", runs, err)
	}
}

// Keep LLM execution canned, but use production action planning so an outcome
// of approved cannot conceal a COMMENT payload in the test double.
type cleanReviewPlanner struct {
	store *ledger.Store
	calls int
}

func (p *cleanReviewPlanner) Live(ctx context.Context, req pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	p.calls++
	plan, err := reviewplan.Build(reviewplan.Request{
		PostMode: reviewplan.PostModeLive, Profile: req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login, HeadSHA: run.SHA,
		Rollup: review.Rollup{ReviewEvent: review.ReviewEventApprove, ReviewEventRationale: "No findings remain."},
		Now:    testNow,
		NewActionID: func(kind reviewplan.ActionKind) (string, error) {
			return run.RunID + "-" + string(kind), nil
		},
	})
	if err != nil {
		return pipeline.Result{}, err
	}
	for _, action := range plan.Actions {
		if err := p.store.InsertPlannedAction(ctx, ledger.PlannedAction{Action: action.Action, RunID: run.RunID}); err != nil {
			return pipeline.Result{}, err
		}
	}
	return pipeline.Result{
		Run: run, PRKey: run.PRKey, Plan: plan,
		Artifacts: pipeline.ArtifactPathsFromDir(run.ArtifactPath),
	}, nil
}

type supersededApprovalClassifier struct {
	calls int
	last  approvaloverride.Request
}

func (c *supersededApprovalClassifier) ClassifyApprovalOverride(_ context.Context, req approvaloverride.Request) (approvaloverride.Result, error) {
	c.calls++
	c.last = req
	return approvaloverride.Result{Approve: true}, nil
}
