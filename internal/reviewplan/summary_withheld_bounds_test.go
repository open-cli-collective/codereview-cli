package reviewplan

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestApprovalWithheldFailureDiagnosticsAreBounded(t *testing.T) {
	largeError := string([]byte{0xff}) + strings.Repeat("界", 700) + " unique approval-withheld failure tail"
	failures := []ReviewerFailureSummary{
		{AgentID: "go:implementation-tests", Error: largeError},
		{AgentID: "policies:conventions", Error: "failed to read /Users/aaron/private/secret.txt"},
	}
	failuresBefore := append([]ReviewerFailureSummary(nil), failures...)

	req := summaryRequest()
	req.Findings = nil
	req.Rollup = review.Rollup{
		ReviewEvent:          review.ReviewEventApprove,
		ReviewEventRationale: "no findings",
	}
	req.RunSummary.ReviewerFailures = failures
	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Outcome != OutcomeComment {
		t.Fatalf("outcome = %q, want comment when a reviewer failed", plan.Outcome)
	}
	got := plan.RollupMarkdown
	if !utf8.ValidString(got) {
		t.Fatalf("rollup contains invalid UTF-8")
	}
	if strings.Contains(got, "unique approval-withheld failure tail") {
		t.Fatalf("public rollup rendered failure prose beyond its 500-rune sample:\n%s", got)
	}
	for _, marker := range []string{"did not produce a result: ", "failed: "} {
		if gotRunes := sampledTextRunes(got, marker); gotRunes != 500 {
			t.Errorf("public %q sample has %d runes, want 500", marker, gotRunes)
		}
	}
	if strings.Contains(got, "/Users/aaron/private/secret.txt") || !strings.Contains(got, "<path>") {
		t.Fatalf("Build did not redact the absolute path in public output:\n%s", got)
	}
	if !reflect.DeepEqual(plan.Summary.Run.ReviewerFailures, failuresBefore) {
		t.Fatalf("summary did not retain the complete typed failure prose")
	}
}
