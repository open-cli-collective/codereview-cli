package reviewplan

import (
	"fmt"
	"reflect"
	"unicode/utf8"

	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestBuildUnanalyzedThreadsWithholdApprovalAndSuppressActions(t *testing.T) {
	for _, event := range []review.ReviewEvent{review.ReviewEventApprove, review.ReviewEventComment, review.ReviewEventRequestChanges} {
		t.Run(string(event), func(t *testing.T) {
			req := baseRequest()
			req.Findings = nil
			req.Rollup = review.Rollup{ReviewEvent: event}
			if event != review.ReviewEventApprove {
				f := finding("blocking", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12})
				if event == review.ReviewEventRequestChanges {
					f.Severity = review.SeverityBlocking
				}
				req.Findings = []review.Finding{f}
				req.Rollup.OrderedFindings = []review.FindingID{f.ID}
			}
			req.ProviderCaps.ThreadResolution = true
			req.RunSummary.ThreadAnalysisFailures = []ThreadAnalysisFailureSummary{{ThreadID: "failed-thread", Error: "invalid output"}}
			req.ThreadActions = []review.ThreadAction{{ThreadID: "failed-thread", Decision: review.ThreadDecisionSummarizeAndResolve, Summary: "selector proposed closure"}}
			req.ThreadResponses = []review.ThreadResponseAction{
				{ThreadID: "failed-thread", Kind: review.ThreadResponseSummaryReply, Body: "unsafe response", Resolve: true},
				{ThreadID: "successful-thread", Kind: review.ThreadResponseSummaryReply, Body: "settled", Resolve: true},
			}
			plan, err := Build(req)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range plan.Actions {
				if action.ThreadID == "failed-thread" {
					t.Fatalf("action for unanalyzed thread = %#v", action)
				}
			}
			if plan.Summary.Threads.Considered != 1 || plan.Summary.Threads.Resolved != 1 {
				t.Fatalf("thread counts = %#v, want successful response only", plan.Summary.Threads)
			}
			want := OutcomeComment
			if event == review.ReviewEventRequestChanges {
				want = OutcomeRequestChanges
			}
			if plan.Outcome != want {
				t.Fatalf("outcome = %q, want %q", plan.Outcome, want)
			}
			for _, want := range []string{"### Unanalyzed Threads", "failed-thread", "invalid output", "cached on resume", "--rerun"} {
				if !strings.Contains(plan.RollupMarkdown, want) {
					t.Fatalf("rollup missing %q: %s", want, plan.RollupMarkdown)
				}
			}
			if got := strings.Contains(plan.RollupMarkdown, "### Approval Withheld"); got != (event == review.ReviewEventApprove) {
				t.Fatalf("withheld section = %t for event %s", got, event)
			}
		})
	}
}

func TestUnanalyzedThreadDiagnosticsBoundLargeCollections(t *testing.T) {
	failures := make([]ThreadAnalysisFailureSummary, 2500)
	for i := range failures {
		failures[i] = ThreadAnalysisFailureSummary{ThreadID: fmt.Sprintf("thread-%04d", len(failures)-i-1), Error: strings.Repeat("界", 800)}
	}
	original := append([]ThreadAnalysisFailureSummary(nil), failures...)
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed_coverage=%t", mixed), func(t *testing.T) {
			req := baseRequest()
			req.Findings = nil
			req.Rollup = review.Rollup{ReviewEvent: review.ReviewEventApprove}
			req.RunSummary.ThreadAnalysisFailures = failures
			if mixed {
				req.RunSummary.ReviewerCoverage = []ReviewerCoverageSummary{{AgentID: "reviewer", Status: "incomplete_skipped", Scope: []string{"relocated.go", "missing.go"}, MissingFiles: []string{"missing.go"}, RelocationReviewedFiles: []string{"relocated.go"}}}
			}
			plan, err := Build(req)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Outcome != OutcomeComment {
				t.Fatalf("outcome = %s, want withheld approval", plan.Outcome)
			}
			for _, want := range []string{"2500 threads were not analyzed", "2495 omitted", "thread-0000", "thread-0004", "### Approval Withheld", "### Unanalyzed Threads"} {
				if !strings.Contains(plan.RollupMarkdown, want) {
					t.Errorf("rollup missing %q", want)
				}
			}
			if strings.Contains(plan.RollupMarkdown, "thread-0005") || strings.Contains(plan.RollupMarkdown, "thread-2499") {
				t.Fatal("public body rendered omitted thread IDs")
			}
			if len(plan.RollupMarkdown) > 15000 || !utf8.ValidString(plan.RollupMarkdown) {
				t.Fatalf("public body bytes=%d utf8=%t, want bounded valid body", len(plan.RollupMarkdown), utf8.ValidString(plan.RollupMarkdown))
			}
			if strings.Contains(plan.RollupMarkdown, strings.Repeat("界", 501)) {
				t.Fatal("public diagnostic exceeded 500-rune limit")
			}
			if !reflect.DeepEqual(failures, original) || !reflect.DeepEqual(plan.Summary.Run.ThreadAnalysisFailures, original) {
				t.Fatal("bounded rendering lost or mutated complete typed evidence")
			}
			if mixed {
				for _, want := range []string{"assigned review obligations", "no body inspection or relocation-impact review", "missing.go", "relocation-impact-reviewed (not body-inspected)"} {
					if !strings.Contains(plan.RollupMarkdown, want) {
						t.Errorf("mixed coverage rollup lost %q", want)
					}
				}
			}
			var forward, reversed strings.Builder
			writeThreadAnalysisDiagnostics(&forward, failures)
			reordered := append([]ThreadAnalysisFailureSummary(nil), failures...)
			for i, j := 0, len(reordered)-1; i < j; i, j = i+1, j-1 {
				reordered[i], reordered[j] = reordered[j], reordered[i]
			}
			writeThreadAnalysisDiagnostics(&reversed, reordered)
			if forward.String() != reversed.String() {
				t.Fatal("thread diagnostic examples depend on source order")
			}
		})
	}
}

func TestThreadFailureExamplesBoundLongIDsWithoutMutatingEvidence(t *testing.T) {
	failures := []ThreadAnalysisFailureSummary{{ThreadID: strings.Repeat("🙂", 800) + string([]byte{0xff}), Error: strings.Repeat("界", 800) + "<!-- codereview:marker -->"}}
	original := append([]ThreadAnalysisFailureSummary(nil), failures...)
	examples := ThreadAnalysisFailureExamples(failures)
	if len(examples) != 1 || utf8.RuneCountInString(examples[0].ThreadID) != 500 || utf8.RuneCountInString(examples[0].Error) != 500 {
		t.Fatalf("example sizes = %#v, want 500-rune samples", examples)
	}
	if !utf8.ValidString(examples[0].ThreadID+examples[0].Error) || strings.Contains(examples[0].Error, "<!-- codereview:") {
		t.Fatal("display sample contains invalid UTF-8 or live marker")
	}
	if !reflect.DeepEqual(failures, original) {
		t.Fatal("sampling changed complete failure evidence")
	}
}
