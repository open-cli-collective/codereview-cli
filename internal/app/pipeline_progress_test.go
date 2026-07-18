package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

func TestPipelineReviewerProgressRendersResolutionAndAssignments(t *testing.T) {
	var out bytes.Buffer
	reporter := newPipelineReviewerProgress(progress.New(&out, false, time.Now), "review")
	reporter.ReviewersResolved(pipeline.ReviewerCatalogProgress{
		RepoStatus:   "available",
		TotalCount:   2,
		OfferedCount: 2,
		Reviewers: []pipeline.ReviewerProgressAgent{
			{AgentID: "repo:rules", Provenance: "repo@refs/heads/main:abc123", SourceKind: "repo", RequiredIfApplicable: true},
			{AgentID: "shared:dotnet", Provenance: "profile:/catalog", SourceKind: "profile"},
		},
	})
	reporter.ReviewersSelected(pipeline.ReviewerSelectionProgress{
		Reasoning: "Repository rules and shared .NET coverage apply.",
		Reviewers: []pipeline.ReviewerAssignmentProgress{{
			ReviewerProgressAgent: pipeline.ReviewerProgressAgent{
				AgentID: "repo:rules", Provenance: "repo@refs/heads/main:abc123", SourceKind: "repo", RequiredIfApplicable: true,
			},
			Rationale:    "Repository invariant applies.",
			Files:        []string{"main.go", "config.go"},
			AllowedFiles: []string{"main.go"},
		}},
	})

	got := out.String()
	for _, want := range []string{
		`op="resolve_reviewers" target="reviewers" offered_count="2"`,
		`offered_reviewers="repo:rules@repo@refs/heads/main:abc123,shared:dotnet@profile:/catalog"`,
		`repo_status="available" required_if_applicable="repo:rules" total_count="2"`,
		`op="select_reviewers" target="reviewers" reasoning="Repository rules and shared .NET coverage apply." selected_count="1" selected_ids="repo:rules"`,
		`op="assign_reviewer" target="reviewer" agent_id="repo:rules" allowed_files="main.go" files="main.go,config.go"`,
		`rationale="Repository invariant applies." required_if_applicable="true" source_kind="repo"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress output = %q, want %q", got, want)
		}
	}
}
