package reviewplan

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
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
		ActionKindRollupComment,
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
	if got := plan.Actions[3].Marker; !got.BodyBearing || !got.Skip || got.Outcome != OutcomeRequestChanges {
		t.Fatalf("rollup marker = %#v", got)
	}
	if got := plan.Actions[4].Marker; !got.BodyBearing || !got.Skip || got.ActionKind != ActionKindSubmitReview || got.Outcome != "" {
		t.Fatalf("submit marker = %#v", got)
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
	if strings.Contains(plan.RollupMarkdown, "nits detail") {
		t.Fatalf("rollup included nits detail while IncludeNits=false:\n%s", plan.RollupMarkdown)
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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	allowed := map[string]bool{
		"github.com/open-cli-collective/codereview-cli/internal/review": true,
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if deniedImport(path) {
				t.Fatalf("%s imports denied package %q", name, path)
			}
			if allowed[path] || isStdlibImport(path) {
				continue
			}
			t.Fatalf("%s imports %q; reviewplan production code may only import stdlib plus internal/review", name, path)
		}
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
		return string(kind) + "-" + string(rune('a'+next-1)), nil
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
