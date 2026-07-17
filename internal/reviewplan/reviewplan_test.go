package reviewplan

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

var testTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func TestBuildUsesSameStructureForLiveAndDryRun(t *testing.T) {
	liveReq := baseRequest()
	dryReq := baseRequest()
	dryReq.PostMode = PostModeDryRun

	live, err := Build(liveReq)
	if err != nil {
		t.Fatalf("Build live: %v", err)
	}
	dry, err := Build(dryReq)
	if err != nil {
		t.Fatalf("Build dry: %v", err)
	}

	if live.Outcome != dry.Outcome || live.RollupMarkdown != dry.RollupMarkdown {
		t.Fatalf("live/dry plan diverged: live=%#v dry=%#v", live, dry)
	}
	if !strings.Contains(live.RollupMarkdown, "| Severity | Findings |") {
		t.Fatalf("rollup markdown missing summary table:\n%s", live.RollupMarkdown)
	}
	if !reflect.DeepEqual(live.AnchoredFindings, dry.AnchoredFindings) {
		t.Fatalf("anchored findings differ:\nlive=%#v\ndry=%#v", live.AnchoredFindings, dry.AnchoredFindings)
	}
	if len(live.Actions) != len(dry.Actions) {
		t.Fatalf("action count live=%d dry=%d", len(live.Actions), len(dry.Actions))
	}
	for i := range live.Actions {
		liveCopy := live.Actions[i]
		dryCopy := dry.Actions[i]
		if liveCopy.Status != ActionStatusPending {
			t.Fatalf("live action %d status = %q", i, liveCopy.Status)
		}
		if dryCopy.Status != ActionStatusPlannedOnly {
			t.Fatalf("dry action %d status = %q", i, dryCopy.Status)
		}
		liveCopy.Status = ""
		dryCopy.Status = ""
		if !reflect.DeepEqual(liveCopy, dryCopy) {
			t.Fatalf("action %d differs except status:\nlive=%#v\ndry=%#v", i, liveCopy, dryCopy)
		}
	}
	assertNoMarkerBodies(t, live)
	assertNoMarkerBodies(t, dry)
}

func TestBuildOrdersActionsAndMarkerMetadata(t *testing.T) {
	req := baseRequest()
	req.ThreadActions = []review.ThreadAction{
		{ThreadID: "thread-1", Decision: review.ThreadDecisionSummarizeAndResolve, Summary: "fixed"},
	}
	req.ProviderCaps.ThreadResolution = true

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gotKinds := actionKinds(plan.Actions)
	wantKinds := []ActionKind{
		ActionKindThreadReply,
		ActionKindResolveThread,
		ActionKindInlineComment,
		ActionKindSubmitReview,
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("action kinds = %#v, want %#v", gotKinds, wantKinds)
	}
	if !plan.Actions[0].Marker.ThreadSummary || plan.Actions[0].Marker.Skip {
		t.Fatalf("thread reply marker = %#v, want thread-summary without skip", plan.Actions[0].Marker)
	}
	if plan.Actions[1].Marker.BodyBearing {
		t.Fatalf("resolve marker = %#v, want non-body-bearing", plan.Actions[1].Marker)
	}
	if got := plan.Actions[3].Marker; !got.BodyBearing || !got.Skip || got.ActionKind != ActionKindSubmitReview || got.Outcome != "" {
		t.Fatalf("submit marker = %#v", got)
	}
	if got := plan.Actions[3].SubmitReview.Body; got != plan.RollupMarkdown {
		t.Fatalf("submit body = %q, want rollup markdown", got)
	}
}

func TestRollupRenderingFallbackMetadataAndAgentNote(t *testing.T) {
	req := baseRequest()
	req.ProviderCaps = ProviderCaps{ThreadResolution: true}
	req.AgentDefinitionsChanged = true
	req.ThreadActions = []review.ThreadAction{
		{ThreadID: "thread-1", Decision: review.ThreadDecisionSummarizeOnly, Summary: "summarized one"},
		{ThreadID: "thread-2", Decision: review.ThreadDecisionSummarizeAndResolve, Summary: "summarized two"},
	}
	req.Findings = []review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
		finding("f-2", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
	}
	req.Findings[1].Severity = review.SeverityNits
	req.Findings[1].Body = "nits detail"
	req.Rollup.OrderedFindings = []review.FindingID{"f-1", "f-2"}

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, want := range []string{
		"## Automated PR Review",
		"**Reviewed commit:** `1234567890ab`",
		"**Profile:** `work` - **Posting as:** `review-bot`",
		"| major | 1 |",
		"*2 PR discussion threads considered. 2 summarized; 1 resolved.*",
		"Note: This PR modifies reviewer definitions under `.codereview/agents/`.",
	} {
		if !strings.Contains(plan.RollupMarkdown, want) {
			t.Fatalf("rollup markdown missing %q:\n%s", want, plan.RollupMarkdown)
		}
	}
	if !strings.Contains(plan.RollupMarkdown, "nits detail") {
		t.Fatalf("rollup missing nit finding body after nits are always included:\n%s", plan.RollupMarkdown)
	}

	inline := actionsOfKind(plan.Actions, ActionKindInlineComment)[0].InlineComment
	if inline.SubjectType != review.AnchorKindLine || inline.Side != review.DiffSideRight || inline.Line != 10 || inline.DiffPosition != 1 {
		t.Fatalf("fallback inline payload = %#v", inline)
	}
	if !strings.Contains(inline.Body, fileLevelFallbackPrefix+"main.go") || !strings.Contains(inline.Body, inlineFooter) {
		t.Fatalf("fallback body missing wrapper/footer: %q", inline.Body)
	}
}

func TestThreadDecisionResolutionDisabled(t *testing.T) {
	req := baseRequest()
	req.ProviderCaps.ThreadResolution = false
	req.ThreadActions = []review.ThreadAction{
		{ThreadID: "thread-1", Decision: review.ThreadDecisionSummarizeOnly, Summary: "summarized one"},
		{ThreadID: "thread-2", Decision: review.ThreadDecisionSummarizeAndResolve, Summary: "summarized two"},
	}

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := actionsOfKind(plan.Actions, ActionKindThreadReply); len(got) != 2 {
		t.Fatalf("thread replies = %d, want 2", len(got))
	}
	if got := actionsOfKind(plan.Actions, ActionKindResolveThread); len(got) != 0 {
		t.Fatalf("resolve actions = %#v, want none when capability disabled", got)
	}
}

func TestThreadDecisionRejectsInvalidValue(t *testing.T) {
	req := baseRequest()
	req.ThreadActions = []review.ThreadAction{
		{ThreadID: "thread-1", Decision: review.ThreadDecision("bogus"), Summary: "summary"},
	}

	_, err := Build(req)
	if err == nil || !strings.Contains(err.Error(), "invalid thread decision") {
		t.Fatalf("Build error = %v, want invalid thread decision", err)
	}
}

func TestBuildThreadResponsesEmitsOnlyRequiredThreadActions(t *testing.T) {
	plan, err := BuildThreadResponses(ThreadResponseRequest{
		PostMode:     PostModeLive,
		ProviderCaps: ProviderCaps{ThreadResolution: true},
		Responses: []review.ThreadResponseAction{
			{Kind: review.ThreadResponseReply, ThreadID: "thread-1", Body: "Please clarify."},
			{Kind: review.ThreadResponseSummaryReply, ThreadID: "thread-2", Body: "Resolved summary.", Resolve: true},
		},
		Now:         func() time.Time { return testTime },
		NewActionID: newIDGenerator(),
	})
	if err != nil {
		t.Fatalf("BuildThreadResponses: %v", err)
	}
	if plan.Outcome != OutcomeComment {
		t.Fatalf("Outcome = %q, want comment", plan.Outcome)
	}
	if got := actionKinds(plan.Actions); !reflect.DeepEqual(got, []ActionKind{ActionKindThreadReply, ActionKindThreadReply, ActionKindResolveThread}) {
		t.Fatalf("action kinds = %#v, want reply/reply/resolve only", got)
	}
	for _, action := range plan.Actions {
		if !action.Required {
			t.Fatalf("action %#v Required = false, want true", action)
		}
		if action.Kind == ActionKindRollupComment || action.Kind == ActionKindSubmitReview {
			t.Fatalf("response-only plan emitted %s", action.Kind)
		}
		if action.Status != ActionStatusPending {
			t.Fatalf("action status = %q, want pending", action.Status)
		}
	}
	if plan.Actions[0].ThreadReply == nil || plan.Actions[0].ThreadReply.Summary {
		t.Fatalf("normal reply payload = %#v, want Summary=false", plan.Actions[0].ThreadReply)
	}
	if plan.Actions[1].ThreadReply == nil || !plan.Actions[1].ThreadReply.Summary || !plan.Actions[1].Marker.ThreadSummary {
		t.Fatalf("summary reply = payload %#v marker %#v, want summary marker", plan.Actions[1].ThreadReply, plan.Actions[1].Marker)
	}
}

func TestBuildThreadResponsesDryRunAndResolutionDisabled(t *testing.T) {
	plan, err := BuildThreadResponses(ThreadResponseRequest{
		PostMode:     PostModeDryRun,
		ProviderCaps: ProviderCaps{ThreadResolution: false},
		Responses: []review.ThreadResponseAction{
			{Kind: review.ThreadResponseSummaryReply, ThreadID: "thread-1", Body: "Summary.", Resolve: true},
		},
		Now:         func() time.Time { return testTime },
		NewActionID: newIDGenerator(),
	})
	if err != nil {
		t.Fatalf("BuildThreadResponses: %v", err)
	}
	if got := actionKinds(plan.Actions); !reflect.DeepEqual(got, []ActionKind{ActionKindThreadReply}) {
		t.Fatalf("action kinds = %#v, want reply only when resolution disabled", got)
	}
	if plan.Actions[0].Status != ActionStatusPlannedOnly {
		t.Fatalf("status = %q, want planned_only", plan.Actions[0].Status)
	}
}

func TestBuildThreadResponsesNoActions(t *testing.T) {
	plan, err := BuildThreadResponses(ThreadResponseRequest{
		PostMode:    PostModeLive,
		Now:         func() time.Time { return testTime },
		NewActionID: newIDGenerator(),
	})
	if err != nil {
		t.Fatalf("BuildThreadResponses: %v", err)
	}
	if plan.Outcome != OutcomeNothingToReview || len(plan.Actions) != 0 {
		t.Fatalf("plan = %#v, want nothing_to_review with no actions", plan)
	}
}

func TestBuildThreadResponsesRejectsInvalidResponse(t *testing.T) {
	_, err := BuildThreadResponses(ThreadResponseRequest{
		PostMode: PostModeLive,
		Responses: []review.ThreadResponseAction{
			{Kind: review.ThreadResponseReply, ThreadID: "thread-1", Resolve: true},
		},
		Now:         func() time.Time { return testTime },
		NewActionID: newIDGenerator(),
	})
	if err == nil || !strings.Contains(err.Error(), "thread response") {
		t.Fatalf("BuildThreadResponses error = %v, want invalid response", err)
	}
}

func TestMultipleInlineActionsFollowRollupOrder(t *testing.T) {
	req := baseRequest()
	req.Findings = []review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12}),
		finding("f-2", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 13}),
	}
	req.Rollup.OrderedFindings = []review.FindingID{"f-2", "f-1"}

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	inline := actionsOfKind(plan.Actions, ActionKindInlineComment)
	if got := []review.FindingID{inline[0].FindingID, inline[1].FindingID}; !reflect.DeepEqual(got, []review.FindingID{"f-2", "f-1"}) {
		t.Fatalf("inline finding order = %#v", got)
	}
}

func TestAnchoringModes(t *testing.T) {
	tests := []struct {
		name        string
		caps        ProviderCaps
		diff        Diff
		finding     review.Finding
		wantAnchor  review.Anchoring
		wantSubject review.AnchorKind
		wantBody    string
		wantAction  bool
	}{
		{
			name: "inline",
			caps: ProviderCaps{NativeFileLevelComments: true},
			diff: oneFileDiff("main.go"),
			finding: finding("f-1", "main.go", review.Anchor{
				Kind: review.AnchorKindLine,
				Side: review.DiffSideRight,
				Line: 12,
			}),
			wantAnchor:  review.AnchoringInline,
			wantSubject: review.AnchorKindLine,
			wantBody:    inlineFooter,
			wantAction:  true,
		},
		{
			name: "native file level",
			caps: ProviderCaps{NativeFileLevelComments: true},
			diff: oneFileDiff("main.go"),
			finding: finding("f-1", "main.go", review.Anchor{
				Kind: review.AnchorKindFile,
			}),
			wantAnchor:  review.AnchoringFileLevelNative,
			wantSubject: review.AnchorKindFile,
			wantBody:    inlineFooter,
			wantAction:  true,
		},
		{
			name: "first hunk fallback",
			diff: oneFileDiff("main.go"),
			finding: finding("f-1", "main.go", review.Anchor{
				Kind: review.AnchorKindFile,
			}),
			wantAnchor:  review.AnchoringFileLevelFallback,
			wantSubject: review.AnchorKindLine,
			wantBody:    fileLevelFallbackPrefix + "main.go",
			wantAction:  true,
		},
		{
			name: "rollup only missing file",
			diff: oneFileDiff("main.go"),
			finding: finding("f-1", "other.go", review.Anchor{
				Kind: review.AnchorKindFile,
			}),
			wantAnchor: review.AnchoringRollupOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseRequest()
			req.ProviderCaps = tt.caps
			req.Diff = tt.diff
			req.Findings = []review.Finding{tt.finding}
			req.Rollup.OrderedFindings = []review.FindingID{tt.finding.ID}

			plan, err := Build(req)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			got := plan.AnchoredFindings[0]
			if got.Anchoring != tt.wantAnchor {
				t.Fatalf("anchoring = %q, want %q", got.Anchoring, tt.wantAnchor)
			}
			inlineActions := actionsOfKind(plan.Actions, ActionKindInlineComment)
			if !tt.wantAction {
				if len(inlineActions) != 0 {
					t.Fatalf("inline actions = %#v, want none", inlineActions)
				}
				return
			}
			if len(inlineActions) != 1 {
				t.Fatalf("inline actions = %d, want 1", len(inlineActions))
			}
			payload := inlineActions[0].InlineComment
			if payload.SubjectType != tt.wantSubject {
				t.Fatalf("subject type = %q, want %q", payload.SubjectType, tt.wantSubject)
			}
			if !strings.Contains(payload.Body, tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", payload.Body, tt.wantBody)
			}
			if strings.Contains(got.Body, inlineFooter) || strings.Contains(got.Body, fileLevelFallbackPrefix) {
				t.Fatalf("durable body contains posting wrapper: %q", got.Body)
			}
		})
	}
}

func TestInlineCapDemotesToRollupOnly(t *testing.T) {
	req := baseRequest()
	req.MaxInlineComments = 1
	req.Findings = []review.Finding{
		finding("f-1", "main.go", review.Anchor{
			Kind: review.AnchorKindLine,
			Side: review.DiffSideRight,
			Line: 12,
		}),
		finding("f-2", "main.go", review.Anchor{
			Kind: review.AnchorKindLine,
			Side: review.DiffSideRight,
			Line: 13,
		}),
	}
	req.Rollup.OrderedFindings = []review.FindingID{"f-1", "f-2"}

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plan.AnchoredFindings[1].Anchoring; got != review.AnchoringRollupOnly {
		t.Fatalf("anchoring = %q, want rollup-only", got)
	}
	if got := actionsOfKind(plan.Actions, ActionKindInlineComment); len(got) != 1 {
		t.Fatalf("inline actions = %#v, want one", got)
	}
}

func TestEventMappingAndNothingToReview(t *testing.T) {
	if got := ReviewEventForFindings([]review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
	}, EventOptions{}); got != review.ReviewEventComment {
		t.Fatalf("major event = %q, want comment", got)
	}
	blocking := finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindFile})
	blocking.Severity = review.SeverityBlocking
	if got := ReviewEventForFindings([]review.Finding{blocking}, EventOptions{}); got != review.ReviewEventRequestChanges {
		t.Fatalf("blocking event = %q, want request_changes", got)
	}
	if got := ReviewEventForFindings([]review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
	}, EventOptions{MajorEventRequestsChanges: true}); got != review.ReviewEventRequestChanges {
		t.Fatalf("major policy event = %q, want request_changes", got)
	}
	if got := ReviewEventForFindings(nil, EventOptions{PostingIdentityIsPRAuthor: true}); got != review.ReviewEventComment {
		t.Fatalf("self-review event = %q, want comment", got)
	}
	if got, err := OutcomeFromReviewEvent(review.ReviewEventApprove); err != nil || got != OutcomeApproved {
		t.Fatalf("OutcomeFromReviewEvent approve = %q, %v", got, err)
	}

	req := baseRequest()
	req.Rollup.ReviewEvent = review.ReviewEventApprove
	req.Findings[0].Severity = review.SeverityMinor
	req.EventOptions.PostingIdentityIsPRAuthor = true
	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build explicit self approve: %v", err)
	}
	if plan.Outcome != OutcomeComment {
		t.Fatalf("self-approve outcome = %q, want comment", plan.Outcome)
	}
	submit := actionsOfKind(plan.Actions, ActionKindSubmitReview)[0]
	if submit.SubmitReview.Event != review.ReviewEventComment {
		t.Fatalf("self-approve submit event = %q, want comment", submit.SubmitReview.Event)
	}
	req.EventOptions.AllowSelfApprove = true
	plan, err = Build(req)
	if err != nil {
		t.Fatalf("Build explicit self approve allowed: %v", err)
	}
	submit = actionsOfKind(plan.Actions, ActionKindSubmitReview)[0]
	if plan.Outcome != OutcomeApproved || submit.SubmitReview.Event != review.ReviewEventApprove {
		t.Fatalf("allowed self-approve = outcome %q event %q, want approve", plan.Outcome, submit.SubmitReview.Event)
	}

	req = baseRequest()
	req.NoDiff = true
	req.Findings = nil
	req.Rollup.OrderedFindings = nil
	plan, err = Build(req)
	if err != nil {
		t.Fatalf("Build no diff: %v", err)
	}
	if plan.Outcome != OutcomeNothingToReview {
		t.Fatalf("outcome = %q", plan.Outcome)
	}
	gotKinds := actionKinds(plan.Actions)
	if !reflect.DeepEqual(gotKinds, []ActionKind{ActionKindRollupComment}) {
		t.Fatalf("no-diff action kinds = %#v", gotKinds)
	}
	if !plan.Actions[0].Required {
		t.Fatal("no-diff rollup required = false")
	}
}

func TestBuildForcesRequestChangesWhenRepoGuidanceUnavailable(t *testing.T) {
	req := baseRequest()
	req.Findings = nil
	req.Rollup = review.Rollup{}
	req.ThreadActions = []review.ThreadAction{{
		ThreadID: "thread-1",
		Decision: review.ThreadDecisionSummarizeAndResolve,
		Summary:  "will be ignored",
	}}
	req.RepoGuidanceUnavailable = true
	req.RepoGuidanceUnavailableReason = "Base branch `.codereview/agents/` could not be read as trusted review guidance."

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Outcome != OutcomeRequestChanges {
		t.Fatalf("outcome = %q, want request_changes", plan.Outcome)
	}
	if got := actionKinds(plan.Actions); !reflect.DeepEqual(got, []ActionKind{ActionKindSubmitReview}) {
		t.Fatalf("action kinds = %#v, want submit only", got)
	}
	submit := actionsOfKind(plan.Actions, ActionKindSubmitReview)[0]
	if submit.SubmitReview.Event != review.ReviewEventRequestChanges {
		t.Fatalf("submit event = %q, want request_changes", submit.SubmitReview.Event)
	}
	if submit.SubmitReview.Body != plan.RollupMarkdown {
		t.Fatalf("submit body = %q, want rollup markdown", submit.SubmitReview.Body)
	}
	if strings.Contains(plan.RollupMarkdown, "will be ignored") {
		t.Fatalf("rollup = %q, want no thread-action content", plan.RollupMarkdown)
	}
	if !strings.Contains(plan.RollupMarkdown, "trusted repo-local review guidance") || !strings.Contains(plan.RollupMarkdown, req.RepoGuidanceUnavailableReason) {
		t.Fatalf("rollup = %q, want repo guidance explanation", plan.RollupMarkdown)
	}
}

func TestBuildNoDiffTakesPrecedenceOverRepoGuidanceUnavailable(t *testing.T) {
	req := baseRequest()
	req.NoDiff = true
	req.Findings = nil
	req.Rollup = review.Rollup{}
	req.RepoGuidanceUnavailable = true
	req.RepoGuidanceUnavailableReason = "Base branch `.codereview/agents/` was invalid and could not be used as trusted review guidance."

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Outcome != OutcomeNothingToReview {
		t.Fatalf("outcome = %q, want nothing_to_review", plan.Outcome)
	}
	if got := actionKinds(plan.Actions); !reflect.DeepEqual(got, []ActionKind{ActionKindRollupComment}) {
		t.Fatalf("action kinds = %#v, want rollup only", got)
	}
	if strings.Contains(plan.RollupMarkdown, req.RepoGuidanceUnavailableReason) {
		t.Fatalf("rollup = %q, want no repo guidance override in no-diff path", plan.RollupMarkdown)
	}
}

func TestOrderedFindingsMustCoverNonDroppedFindings(t *testing.T) {
	req := baseRequest()
	req.Findings = []review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
		finding("f-2", "main.go", review.Anchor{Kind: review.AnchorKindFile}),
	}
	req.Findings[1].Body = "dropped finding body"
	req.Rollup.OrderedFindings = []review.FindingID{"f-1"}

	_, err := Build(req)
	if err == nil || !strings.Contains(err.Error(), "neither ordered nor dropped") {
		t.Fatalf("Build omitted finding error = %v", err)
	}

	req.Rollup.DedupeLog = []review.DedupeEntry{{Kept: "f-1", Dropped: []review.FindingID{"f-2"}, Reason: "same issue"}}
	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build with dropped finding: %v", err)
	}
	if len(plan.AnchoredFindings) != 2 {
		t.Fatalf("anchored findings = %d, want 2", len(plan.AnchoredFindings))
	}
	if got := actionsOfKind(plan.Actions, ActionKindInlineComment); len(got) != 1 {
		t.Fatalf("inline actions = %d, want 1", len(got))
	}
	if strings.Contains(plan.RollupMarkdown, "dropped finding body") {
		t.Fatalf("rollup unexpectedly includes dropped finding body: %q", plan.RollupMarkdown)
	}

	req.Rollup.OrderedFindings = []review.FindingID{"f-1", "f-2"}
	_, err = Build(req)
	if err == nil || !strings.Contains(err.Error(), "dropped finding") {
		t.Fatalf("Build ordered dropped finding error = %v", err)
	}

	req.Rollup.OrderedFindings = nil
	_, err = Build(req)
	if err == nil || !strings.Contains(err.Error(), "ordered findings are required") {
		t.Fatalf("Build empty ordered findings with dedupe error = %v", err)
	}
}

func TestActionIDGeneratorFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  ActionIDGenerator
		want string
	}{
		{
			name: "generator error",
			gen: func(ActionKind) (string, error) {
				return "", errors.New("boom")
			},
			want: "boom",
		},
		{
			name: "empty ID",
			gen: func(ActionKind) (string, error) {
				return "   ", nil
			},
			want: "empty",
		},
		{
			name: "duplicate ID",
			gen: func(ActionKind) (string, error) {
				return "same", nil
			},
			want: "duplicate action ID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			req.NewActionID = tc.gen
			_, err := Build(req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestProductionImportBoundary(t *testing.T) {
	allowed := map[string]bool{
		"github.com/open-cli-collective/codereview-cli/internal/plannedactions": true,
		"github.com/open-cli-collective/codereview-cli/internal/prref":          true,
		"github.com/open-cli-collective/codereview-cli/internal/review":         true,
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("ParseFile(%s): %w", path, err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if deniedImport(path) {
				return fmt.Errorf("%s imports denied package %q", name, path)
			}
			if allowed[path] || isStdlibImport(path) {
				continue
			}
			return fmt.Errorf("%s imports %q; reviewplan production code may only import stdlib plus internal/plannedactions, internal/prref, and internal/review", name, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func baseRequest() Request {
	return Request{
		PostMode:        PostModeLive,
		ProviderCaps:    ProviderCaps{NativeFileLevelComments: true},
		Diff:            oneFileDiff("main.go"),
		Findings:        []review.Finding{finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12})},
		Rollup:          review.Rollup{ReviewEvent: review.ReviewEventRequestChanges, OrderedFindings: []review.FindingID{"f-1"}},
		Profile:         "work",
		PostingIdentity: "review-bot",
		HeadSHA:         "1234567890abcdef",
		Now:             func() time.Time { return testTime },
		NewActionID:     newIDGenerator(),
	}
}

func newIDGenerator() ActionIDGenerator {
	next := 0
	return func(kind ActionKind) (string, error) {
		next++
		return fmt.Sprintf("%s-%03d", kind, next), nil
	}
}

func finding(id string, path string, anchor review.Anchor) review.Finding {
	return review.Finding{
		ID:       review.FindingID(id),
		Severity: review.SeverityMajor,
		FilePath: path,
		Anchor:   anchor,
		Body:     "finding body <!-- codereview:run-id=fake -->",
	}
}

func oneFileDiff(path string) Diff {
	return Diff{Files: []DiffFile{{
		Path: path,
		Hunks: []DiffHunk{{
			OldStart:     10,
			OldEnd:       20,
			NewStart:     10,
			NewEnd:       20,
			FallbackSide: review.DiffSideRight,
			FallbackLine: 10,
			DiffPosition: 1,
		}},
	}}}
}

func actionKinds(actions []Action) []ActionKind {
	kinds := make([]ActionKind, 0, len(actions))
	for _, action := range actions {
		kinds = append(kinds, action.Kind)
	}
	return kinds
}

func actionsOfKind(actions []Action, kind ActionKind) []Action {
	var filtered []Action
	for _, action := range actions {
		if action.Kind == kind {
			filtered = append(filtered, action)
		}
	}
	return filtered
}

func assertNoMarkerBodies(t *testing.T, plan Plan) {
	t.Helper()
	for _, body := range allBodies(plan) {
		if strings.Contains(body, "<!-- codereview:") {
			t.Fatalf("body contains marker prefix: %q", body)
		}
	}
}

func allBodies(plan Plan) []string {
	bodies := []string{plan.RollupMarkdown}
	for _, finding := range plan.AnchoredFindings {
		bodies = append(bodies, finding.Body)
	}
	for _, action := range plan.Actions {
		if action.InlineComment != nil {
			bodies = append(bodies, action.InlineComment.Body)
		}
		if action.ThreadReply != nil {
			bodies = append(bodies, action.ThreadReply.Body)
		}
		if action.RollupComment != nil {
			bodies = append(bodies, action.RollupComment.Body)
		}
		if action.SubmitReview != nil {
			bodies = append(bodies, action.SubmitReview.Body)
		}
	}
	return bodies
}

func isStdlibImport(path string) bool {
	return !strings.Contains(path, ".")
}

func deniedImport(path string) bool {
	for _, root := range []string{"database/sql", "io/fs", "io/ioutil", "net", "os", "os/exec", "syscall"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
