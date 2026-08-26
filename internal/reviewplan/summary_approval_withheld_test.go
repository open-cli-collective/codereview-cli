package reviewplan

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestUninspectedFiles(t *testing.T) {
	t.Run("a file another reviewer inspected is covered", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{
			{
				AgentID:        "structure:repo-health",
				Status:         "incomplete_skipped",
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"schema.sql"},
			},
			{
				AgentID:        "database:schema",
				Status:         "complete_constrained",
				InspectedFiles: []string{"schema.sql"},
			},
		})
		if len(got) != 0 {
			t.Fatalf("uninspectedFiles = %v, want none; the second reviewer read schema.sql", got)
		}
	})

	t.Run("a file every reviewer skipped is uninspected", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{
			{
				AgentID:        "structure:repo-health",
				Status:         "incomplete_skipped",
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"vendor_dump.sql"},
			},
			{
				AgentID:        "architecture:solid",
				Status:         "complete_constrained",
				InspectedFiles: []string{"main.go"},
			},
		})
		if len(got) != 1 || got[0] != "vendor_dump.sql" {
			t.Fatalf("uninspectedFiles = %v, want [vendor_dump.sql]", got)
		}
	})

	t.Run("unassigned files count and duplicates collapse", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{
			{
				AgentID:      "structure:repo-health",
				Status:       "incomplete_skipped",
				SkippedFiles: []string{"b.sh", "a.sh"},
			},
			{
				AgentID:      "policies:conventions",
				Status:       "incomplete_skipped",
				SkippedFiles: []string{"a.sh"},
			},
			{
				AgentID:      "unassigned",
				Status:       "incomplete_unassigned",
				SkippedFiles: []string{"c.sh"},
			},
		})
		want := []string{"a.sh", "b.sh", "c.sh"}
		if len(got) != len(want) {
			t.Fatalf("uninspectedFiles = %v, want %v", got, want)
		}
		for i, file := range want {
			if got[i] != file {
				t.Fatalf("uninspectedFiles = %v, want %v (sorted, deduped)", got, want)
			}
		}
	})
}

func TestRollupApprovalWithheld(t *testing.T) {
	cleanApproveRequest := func() Request {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		return req
	}

	t.Run("names the unread files when coverage withholds approval", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "structure:repo-health",
				Status:         "incomplete_skipped",
				Scope:          []string{"main.go", "big_test.py"},
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"big_test.py"},
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeComment {
			t.Fatalf("outcome = %q, want comment", plan.Outcome)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"### Approval Withheld",
			"No blocking or major findings were reported.",
			"1 file inspected by no reviewer:",
			"`big_test.py`",
			"Re-running the same review reproduces this",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
	})

	t.Run("a reviewer failure is named even with no unread files", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerFailures: []ReviewerFailureSummary{{
				AgentID: "structure:repo-health",
				Error:   "adapter exited 1",
			}},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "architecture:solid",
				Status:         "complete_broad",
				InspectedFiles: []string{"main.go"},
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"### Approval Withheld",
			"`structure:repo-health` did not produce a result: adapter exited 1",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
		if strings.Contains(md, "inspected by no reviewer") {
			t.Fatalf("rollup claims unread files when there are none:\n%s", md)
		}
	})

	t.Run("a clean approving review says nothing about withholding", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "structure:repo-health",
				Status:         "complete_broad",
				InspectedFiles: []string{"main.go"},
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeApproved {
			t.Fatalf("outcome = %q, want approved", plan.Outcome)
		}
		if strings.Contains(plan.RollupMarkdown, "Approval Withheld") {
			t.Fatalf("approved rollup carries a withheld section:\n%s", plan.RollupMarkdown)
		}
	})

	t.Run("a request-changes review says nothing about withholding", func(t *testing.T) {
		req := baseRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "structure:repo-health",
				Status:         "incomplete_skipped",
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"big_test.py"},
			}},
		}
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventRequestChanges,
			ReviewEventRationale: "blocking finding",
			OrderedFindings:      req.Rollup.OrderedFindings,
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if strings.Contains(plan.RollupMarkdown, "Approval Withheld") {
			t.Fatalf("a review that was never going to approve carries a withheld section:\n%s", plan.RollupMarkdown)
		}
	})
}
