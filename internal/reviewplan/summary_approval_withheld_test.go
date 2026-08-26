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
				Scope:          []string{"main.go", "schema.sql"},
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"schema.sql"},
			},
			{
				AgentID:        "database:schema",
				Status:         "complete_constrained",
				Scope:          []string{"schema.sql"},
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
				Scope:          []string{"main.go", "vendor_dump.sql"},
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"vendor_dump.sql"},
			},
			{
				AgentID:        "architecture:solid",
				Status:         "complete_constrained",
				Scope:          []string{"main.go"},
				InspectedFiles: []string{"main.go"},
			},
		})
		if len(got) != 1 || got[0] != "vendor_dump.sql" {
			t.Fatalf("uninspectedFiles = %v, want [vendor_dump.sql]", got)
		}
	})

	// buildReviewerCoverage sets incomplete_skipped when assigned files were
	// neither inspected nor skipped; the paths live in the scope and the
	// diagnostic, and the skip list stays empty.
	t.Run("assigned files reported in neither list are uninspected", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{{
			AgentID:        "structure:repo-health",
			Status:         "incomplete_skipped",
			Scope:          []string{"main.go", "forgotten.py"},
			InspectedFiles: []string{"main.go"},
			Diagnostic:     "assigned files were neither inspected nor skipped: forgotten.py",
		}})
		if len(got) != 1 || got[0] != "forgotten.py" {
			t.Fatalf("uninspectedFiles = %v, want [forgotten.py]", got)
		}
	})

	// A reviewer that crashed gets a coverage entry carrying its scope and no
	// file lists at all, so its uniquely assigned files are unread.
	t.Run("a failed reviewer leaves its whole scope uninspected", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{
			{
				AgentID:    "structure:repo-health",
				Status:     "incomplete_failed",
				Scope:      []string{"a.sh", "shared.go"},
				Diagnostic: "adapter exited 1",
			},
			{
				AgentID:        "go:implementation-tests",
				Status:         "complete_constrained",
				Scope:          []string{"shared.go"},
				InspectedFiles: []string{"shared.go"},
			},
		})
		if len(got) != 1 || got[0] != "a.sh" {
			t.Fatalf("uninspectedFiles = %v, want [a.sh]; shared.go was read by the other reviewer", got)
		}
	})

	t.Run("unassigned files count and duplicates collapse", func(t *testing.T) {
		got := uninspectedFiles([]ReviewerCoverageSummary{
			{
				AgentID:      "structure:repo-health",
				Status:       "incomplete_skipped",
				Scope:        []string{"b.sh", "a.sh"},
				SkippedFiles: []string{"b.sh", "a.sh"},
			},
			{
				AgentID:      "policies:conventions",
				Status:       "incomplete_skipped",
				Scope:        []string{"a.sh"},
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

	mustContain := func(t *testing.T, md string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
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
		mustContain(t, plan.RollupMarkdown,
			"### Approval Withheld",
			"No blocking or major findings were reported.",
			"1 file inspected by no reviewer:",
			"`big_test.py`",
			"Re-running the same review reproduces this",
		)
	})

	// The gate coerces on coverage status, so every status it coerces on has to
	// produce an explanation. These are the statuses that carry no skip list.
	t.Run("explains a status whose evidence is only a diagnostic", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "structure:repo-health",
				Status:         "incomplete_tool",
				Scope:          []string{"main.go"},
				InspectedFiles: []string{"main.go"},
				Diagnostic:     "diff tool did not succeed",
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeComment {
			t.Fatalf("outcome = %q, want comment", plan.Outcome)
		}
		mustContain(t, plan.RollupMarkdown,
			"### Approval Withheld",
			"`structure:repo-health` — ⚠️ incomplete (tool failure): diff tool did not succeed",
		)
	})

	t.Run("a failed reviewer's assigned files are named as unread", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health", "go:implementation-tests"},
			ReviewerFailures: []ReviewerFailureSummary{{
				AgentID: "structure:repo-health",
				Error:   "adapter exited 1",
			}},
			ReviewerCoverage: []ReviewerCoverageSummary{
				{
					AgentID:    "structure:repo-health",
					Status:     "incomplete_failed",
					Scope:      []string{"deploy.sh"},
					Diagnostic: "adapter exited 1",
				},
				{
					AgentID:        "go:implementation-tests",
					Status:         "complete_constrained",
					Scope:          []string{"main.go"},
					InspectedFiles: []string{"main.go"},
				},
			},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		mustContain(t, md,
			"### Approval Withheld",
			"`structure:repo-health` did not produce a result: adapter exited 1",
			"1 file inspected by no reviewer:",
			"`deploy.sh`",
		)
		// Within this section the crash is one event: it is reported as a
		// failure, not also as a coverage diagnostic. The rollup's separate
		// Reviewer Diagnostics section names it again, which is its job.
		section := md[strings.Index(md, "### Approval Withheld"):]
		if end := strings.Index(section[1:], "\n### "); end >= 0 {
			section = section[:end+1]
		}
		if strings.Count(section, "adapter exited 1") != 1 {
			t.Fatalf("failed reviewer reported twice inside the section:\n%s", section)
		}
	})

	// summary.Reviewers is derived from SelectedReviewers, while the coercion
	// reads ReviewerCoverage, so a withheld run can reach the rollup's
	// no-reviewer-table branch.
	t.Run("renders in the rollup shape that has no reviewer table", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:      "unassigned",
				Status:       "incomplete_unassigned",
				SkippedFiles: []string{"orphan.sh"},
				Diagnostic:   "changed files were not assigned to a selected reviewer",
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
		if strings.Contains(md, "| Reviewer | Findings |") {
			t.Fatalf("test did not reach the no-reviewer-table branch:\n%s", md)
		}
		mustContain(t, md, "### Approval Withheld", "`orphan.sh`")
	})

	t.Run("a clean approving review says nothing about withholding", func(t *testing.T) {
		req := cleanApproveRequest()
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"structure:repo-health"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "structure:repo-health",
				Status:         "complete_broad",
				Scope:          []string{"main.go"},
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
				Scope:          []string{"main.go", "big_test.py"},
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
