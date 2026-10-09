package reviewplan

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPublicFailureAndConstraintProseIsBoundedAndCoverageNoteIsLocal(t *testing.T) {
	failureError := string([]byte{0xff}) + strings.Repeat("界", 700) + " failure tail"
	constraints := []string{string([]byte{0xfe}) + strings.Repeat("🙂", 700) + " constraint tail"}
	failures := []ReviewerFailureSummary{{AgentID: "failed", Error: failureError}}
	coverage := []ReviewerCoverageSummary{{
		AgentID:     "reviewer",
		Status:      "complete_broad",
		Constraints: constraints,
	}}
	failuresBefore := append([]ReviewerFailureSummary(nil), failures...)
	coverageBefore := []ReviewerCoverageSummary{{
		AgentID:     coverage[0].AgentID,
		Status:      coverage[0].Status,
		Constraints: append([]string(nil), constraints...),
	}}

	req := summaryRequest()
	req.RunSummary.ReviewerFailures = failures
	req.RunSummary.ReviewerCoverage = coverage
	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := plan.RollupMarkdown
	if !utf8.ValidString(got) {
		t.Fatalf("rollup contains invalid UTF-8")
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("invalid diagnostic bytes were not replaced in public output")
	}
	for _, marker := range []string{"failed: ", "constraints: "} {
		if gotRunes := sampledTextRunes(got, marker); gotRunes != 500 {
			t.Errorf("public %q sample has %d runes, want 500", marker, gotRunes)
		}
	}
	if strings.Contains(got, "failure tail") || strings.Contains(got, "constraint tail") {
		t.Fatalf("public summary rendered prose beyond its 500-rune sample")
	}
	if !reflect.DeepEqual(plan.Summary.Run.ReviewerFailures, failuresBefore) {
		t.Fatalf("failure summary did not retain its complete typed error")
	}
	if !reflect.DeepEqual(plan.Summary.Run.ReviewerCoverage, coverageBefore) {
		t.Fatalf("coverage constraints were truncated in typed data")
	}
	const note = "Complete coverage details are retained in the local `coverage.json` artifact."
	if !strings.Contains(got, "### Reviewer Coverage\n\n"+note+"\n\n") {
		t.Fatalf("coverage section is missing the local-artifact note:\n%s", got)
	}
	if strings.Contains(note, "/") || strings.Contains(note, "/Users/") {
		t.Fatalf("coverage note contains an absolute path: %q", note)
	}
}

func sampledTextRunes(markdown, marker string) int {
	for _, line := range strings.Split(markdown, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return utf8.RuneCountInString(line[i+len(marker):])
		}
	}
	return 0
}
