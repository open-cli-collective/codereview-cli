package view

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// The JSON view must carry the same did-not-run distinction as the rendered
// rollup. Summary's contract is that both consumers agree; a markdown-only fix
// would leave a failed reviewer serializing as findings: 0, which is the exact
// ambiguity being removed.
func TestReviewSummaryJSONDistinguishesFailedReviewer(t *testing.T) {
	summary := reviewplan.Summary{
		Reviewers: []reviewplan.ReviewerSummary{
			{Name: "security:code-auditor", Findings: 0},
			{Name: "documentation:docs", Findings: 0},
			{Name: "policies:conventions", Findings: 0},
		},
		Run: reviewplan.RunSummary{
			ReviewerCoverage: []reviewplan.ReviewerCoverageSummary{
				{AgentID: "security:code-auditor", Status: "incomplete_failed"},
				{AgentID: "documentation:docs", Status: "complete_broad"},
				// policies:conventions absent: status unknown.
			},
		},
	}

	raw, err := json.Marshal(newReviewSummary(summary))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	if !strings.Contains(got, `"name":"security:code-auditor","findings":0,"ran":false`) {
		t.Fatalf("failed reviewer must serialize ran:false, got:\n%s", got)
	}
	if !strings.Contains(got, `"name":"documentation:docs","findings":0,"ran":true`) {
		t.Fatalf("completed reviewer must serialize ran:true, got:\n%s", got)
	}
	// Unknown coverage omits the field rather than guessing a failure.
	if !strings.Contains(got, `"name":"policies:conventions","findings":0}`) {
		t.Fatalf("unknown coverage must omit ran, got:\n%s", got)
	}
}
