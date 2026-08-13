package reviewplan

import (
	"strings"
	"testing"
)

// A reviewer that never produced a result must not appear as "0".
//
// Zero findings and "did not run" are the same number and opposite meanings:
// one says the code is clean, the other says nothing was examined. Rendering
// both as 0 let a run where most reviewers failed to start read as a clean
// review, with the failure visible only in a coverage section further down.
func TestReviewerTableDistinguishesFailureFromZeroFindings(t *testing.T) {
	reviewers := []ReviewerSummary{
		{Name: "security:code-auditor", Findings: 0}, // failed to start
		{Name: "documentation:docs", Findings: 0},    // genuinely found nothing
	}
	coverage := []ReviewerCoverageSummary{
		{AgentID: "security:code-auditor", Status: "incomplete_failed"},
		{AgentID: "documentation:docs", Status: "complete_broad"},
	}

	var out strings.Builder
	writeReviewerTable(&out, reviewers, coverage)
	got := out.String()

	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "security:code-auditor") {
			continue
		}
		if strings.Contains(line, "| 0 |") {
			t.Fatalf("a reviewer that did not run is reported as zero findings: %q", line)
		}
		if !strings.Contains(line, "did not run") {
			t.Fatalf("failed reviewer row does not say it did not run: %q", line)
		}
	}

	// The reviewer that really did run must still show its honest zero.
	if !strings.Contains(got, "| documentation:docs | 0 |") {
		t.Fatalf("a completed reviewer lost its zero count:\n%s", got)
	}
}

// A reviewer absent from coverage keeps its count: unknown status must not be
// reported as a failure, or genuine zeros start reading as breakage.
func TestReviewerTableKeepsCountWhenCoverageIsUnknown(t *testing.T) {
	var out strings.Builder
	writeReviewerTable(&out,
		[]ReviewerSummary{{Name: "policies:conventions", Findings: 0}},
		nil,
	)
	if !strings.Contains(out.String(), "| policies:conventions | 0 |") {
		t.Fatalf("unknown coverage should leave the count alone:\n%s", out.String())
	}
}
