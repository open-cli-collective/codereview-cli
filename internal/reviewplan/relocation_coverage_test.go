package reviewplan

import (
	"strings"
	"testing"
)

func TestUninspectedFilesCountsRelocationImpactReview(t *testing.T) {
	coverage := []ReviewerCoverageSummary{
		{
			AgentID:                 "go:tests",
			Status:                  "incomplete_skipped",
			Scope:                   []string{"old/module.go", "src/router.go", "missing.go"},
			InspectedFiles:          []string{"src/router.go"},
			RelocationReviewedFiles: []string{"old/module.go"},
			SkippedFiles:            []string{"missing.go"},
			ContextFiles:            []string{"config/routes.yaml"},
		},
	}

	got := uninspectedFiles(coverage)
	if len(got) != 1 || got[0] != "missing.go" {
		t.Fatalf("uninspectedFiles() = %#v, want only missing.go", got)
	}
}

func TestApprovalWithheldDoesNotCallValidRelocationUnread(t *testing.T) {
	coverage := []ReviewerCoverageSummary{{
		AgentID:                 "go:tests",
		Status:                  "incomplete_skipped",
		Scope:                   []string{"old/module.go"},
		RelocationReviewedFiles: []string{"old/module.go"},
	}}
	var out strings.Builder
	writeApprovalWithheld(&out, nil, coverage, nil)
	text := out.String()
	if !strings.Contains(text, "Every changed file was either body-inspected or received relocation-impact review") {
		t.Fatalf("withheld text does not describe relocation coverage accurately: %s", text)
	}
	if strings.Contains(text, "old/module.go not body-inspected") || strings.Contains(text, "no body inspection or relocation-impact review") {
		t.Fatalf("valid relocation is reported as unread: %s", text)
	}
}
