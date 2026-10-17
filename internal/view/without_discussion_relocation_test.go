package view

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func TestNewReviewDryRunJSONPreservesWithoutDiscussionAndRelocationCoverage(t *testing.T) {
	coverage := reviewplan.ReviewerCoverageSummary{
		AgentID: "harness:relocation", Status: "incomplete_skipped",
		InspectedFiles: []string{"workspace.yaml"}, MissingFiles: []string{"src/router.go"},
		ContextFiles: []string{"config/routes.yaml"}, RelocationReviewedFiles: []string{"apps/a.go", "apps/b.go"},
	}
	result := pipeline.Result{
		Run: ledger.Run{RunID: "offline-relocation", PostMode: ledger.PostModeDryRun}, WithoutDiscussion: true,
		Artifacts: runartifact.FromDir("/tmp/offline-relocation"),
		Plan:      reviewplan.Plan{Summary: reviewplan.Summary{Run: reviewplan.RunSummary{ReviewerCoverage: []reviewplan.ReviewerCoverageSummary{coverage}}}},
	}
	rendered, err := NewReviewDryRun(result)
	if err != nil {
		t.Fatalf("NewReviewDryRun: %v", err)
	}
	var out bytes.Buffer
	if err := RenderReviewDryRunJSON(&out, rendered); err != nil {
		t.Fatalf("RenderReviewDryRunJSON: %v", err)
	}
	var decoded ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if !decoded.Run.WithoutDiscussion || decoded.Artifacts.RelocationsJSON != result.Artifacts.RelocationsJSON || decoded.Artifacts.CoverageJSON != result.Artifacts.CoverageJSON {
		t.Fatalf("JSON lost discussion flag or relocation/coverage paths: %#v", decoded)
	}
	if len(decoded.Summary.Run.ReviewerCoverage) != 1 {
		t.Fatalf("JSON coverage = %#v", decoded.Summary.Run.ReviewerCoverage)
	}
	got := decoded.Summary.Run.ReviewerCoverage[0]
	if !slices.Equal(got.InspectedFiles, coverage.InspectedFiles) || !slices.Equal(got.MissingFiles, coverage.MissingFiles) || !slices.Equal(got.ContextFiles, coverage.ContextFiles) || !slices.Equal(got.RelocationReviewedFiles, coverage.RelocationReviewedFiles) {
		t.Fatalf("JSON lost combined coverage collections: %#v", got)
	}
}
