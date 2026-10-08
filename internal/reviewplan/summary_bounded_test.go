package reviewplan

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestReviewerCoverageDiagnosticsBoundLargeCollections(t *testing.T) {
	scope := coverageTestPaths("scope", 2500)
	inspected := coverageTestPaths("inspected", 2500)
	skipped := coverageTestPaths("skipped", 2500)
	missing := coverageTestPaths("missing", 2500)
	contextFiles := coverageTestPaths("context", 2500)
	relocation := coverageTestPaths("relocation", 2500)
	diagnostic := strings.Repeat("界", 20) + string([]byte{0xff}) + strings.Repeat("🙂", 700)
	coverage := []ReviewerCoverageSummary{{
		AgentID:                 "reviewer",
		Status:                  "incomplete_skipped",
		Scope:                   scope,
		InspectedFiles:          inspected,
		SkippedFiles:            skipped,
		MissingFiles:            missing,
		ContextFiles:            contextFiles,
		RelocationReviewedFiles: relocation,
		Diagnostic:              diagnostic,
	}}
	original := cloneReviewerCoverage(coverage)

	req := summaryRequest()
	req.RunSummary.ReviewerCoverage = coverage
	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := plan.RollupMarkdown

	for _, want := range []string{
		"scope: 2500 files; examples: `scope-0000.go`, `scope-0001.go`, `scope-0002.go`, `scope-0003.go`, `scope-0004.go`; 2495 omitted",
		"body-inspected: 2500 files; examples: `inspected-0000.go`, `inspected-0001.go`, `inspected-0002.go`, `inspected-0003.go`, `inspected-0004.go`; 2495 omitted",
		"skipped: 2500 files; examples: `skipped-0000.go`, `skipped-0001.go`, `skipped-0002.go`, `skipped-0003.go`, `skipped-0004.go`; 2495 omitted",
		"missing: 2500 files; examples: `missing-0000.go`, `missing-0001.go`, `missing-0002.go`, `missing-0003.go`, `missing-0004.go`; 2495 omitted",
		"context (not coverage): 2500 files; examples: `context-0000.go`, `context-0001.go`, `context-0002.go`, `context-0003.go`, `context-0004.go`; 2495 omitted",
		"relocation-impact-reviewed (not body-inspected): 2500 files; examples: `relocation-0000.go`, `relocation-0001.go`, `relocation-0002.go`, `relocation-0003.go`, `relocation-0004.go`; 2495 omitted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rollup missing %q", want)
		}
	}
	coverageSection := sectionBetween(got, "### Reviewer Coverage", "### Reviewer Diagnostics")
	for _, prefix := range []string{"scope", "inspected", "skipped", "missing", "context", "relocation"} {
		if strings.Contains(coverageSection, prefix+"-0005.go") {
			t.Errorf("coverage summary rendered more than five %s examples", prefix)
		}
	}
	if !utf8.ValidString(got) {
		t.Fatalf("rollup contains invalid UTF-8")
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("invalid diagnostic byte was not replaced in public output")
	}
	if gotRunes := diagnosticSampleRunes(got); gotRunes != 500 {
		t.Fatalf("public diagnostic sample has %d runes, want 500", gotRunes)
	}
	if !strings.Contains(got, "finding body") {
		t.Fatalf("large coverage diagnostics dropped the real finding body:\n%s", got)
	}
	if len(plan.AnchoredFindings) != 2 || plan.AnchoredFindings[0].Line == nil || *plan.AnchoredFindings[0].Line != 12 ||
		plan.AnchoredFindings[1].Line == nil || *plan.AnchoredFindings[1].Line != 14 ||
		!strings.Contains(plan.AnchoredFindings[0].Body, "finding body") || !strings.Contains(plan.AnchoredFindings[1].Body, "finding body") {
		t.Fatalf("large coverage diagnostics changed finding anchors: %#v", plan.AnchoredFindings)
	}
	if plan.Outcome != OutcomeRequestChanges {
		t.Fatalf("large coverage diagnostics changed review outcome: %q", plan.Outcome)
	}
	if !reflect.DeepEqual(coverage, original) {
		t.Fatalf("rendering mutated typed coverage data")
	}
	if !reflect.DeepEqual(plan.Summary.Run.ReviewerCoverage, original) {
		t.Fatalf("summary did not retain complete typed coverage data")
	}
	var first, second strings.Builder
	writeReviewerCoverageDiagnostics(&first, coverage)
	reordered := cloneReviewerCoverage(coverage)
	for _, paths := range [][]string{
		reordered[0].Scope,
		reordered[0].InspectedFiles,
		reordered[0].SkippedFiles,
		reordered[0].MissingFiles,
		reordered[0].ContextFiles,
		reordered[0].RelocationReviewedFiles,
	} {
		reverseCoveragePaths(paths)
	}
	writeReviewerCoverageDiagnostics(&second, reordered)
	if first.String() != second.String() {
		t.Fatalf("coverage samples depend on source ordering")
	}
}

func TestApprovalWithheldBoundsMissingPathExamples(t *testing.T) {
	missing := coverageTestPaths("missing", 2500)
	req := baseRequest()
	req.Findings = nil
	req.Rollup = review.Rollup{
		ReviewEvent:          review.ReviewEventApprove,
		ReviewEventRationale: "no findings",
	}
	req.RunSummary = RunSummary{
		ReviewerCoverage: []ReviewerCoverageSummary{{
			AgentID:                 "unassigned",
			Status:                  "incomplete_unassigned",
			Scope:                   missing,
			MissingFiles:            append([]string(nil), missing...),
			ContextFiles:            append([]string(nil), missing...),
			RelocationReviewedFiles: append([]string(nil), missing...),
		}},
	}

	plan, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Outcome != OutcomeComment {
		t.Fatalf("outcome = %q, want comment when coverage is incomplete", plan.Outcome)
	}
	section := sectionBetween(plan.RollupMarkdown, "### Approval Withheld", "### Reviewer Coverage")
	for _, want := range []string{
		"2500 files not body-inspected by any reviewer",
		"examples: `missing-0000.go`, `missing-0001.go`, `missing-0002.go`, `missing-0003.go`, `missing-0004.go`",
		"2495 omitted",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("withheld section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "missing-0005.go") {
		t.Fatalf("withheld section rendered more than five missing examples")
	}
}

func coverageTestPaths(prefix string, count int) []string {
	paths := make([]string, count)
	for i := range paths {
		paths[i] = fmt.Sprintf("%s-%04d.go", prefix, count-i-1)
	}
	return paths
}

func cloneReviewerCoverage(in []ReviewerCoverageSummary) []ReviewerCoverageSummary {
	out := make([]ReviewerCoverageSummary, len(in))
	for i, entry := range in {
		out[i] = entry
		out[i].Scope = append([]string(nil), entry.Scope...)
		out[i].InspectedFiles = append([]string(nil), entry.InspectedFiles...)
		out[i].SkippedFiles = append([]string(nil), entry.SkippedFiles...)
		out[i].MissingFiles = append([]string(nil), entry.MissingFiles...)
		out[i].ContextFiles = append([]string(nil), entry.ContextFiles...)
		out[i].RelocationReviewedFiles = append([]string(nil), entry.RelocationReviewedFiles...)
		out[i].Constraints = append([]string(nil), entry.Constraints...)
	}
	return out
}

func reverseCoveragePaths(paths []string) {
	for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
		paths[i], paths[j] = paths[j], paths[i]
	}
}

func sectionBetween(text, startMarker, endMarker string) string {
	start := strings.Index(text, startMarker)
	if start < 0 {
		return ""
	}
	text = text[start:]
	if end := strings.Index(text, endMarker); end >= 0 {
		return text[:end]
	}
	return text
}

func diagnosticSampleRunes(markdown string) int {
	const prefix = "diagnostic: "
	for _, line := range strings.Split(markdown, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			return utf8.RuneCountInString(line[i+len(prefix):])
		}
	}
	return 0
}
