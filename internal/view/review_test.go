package view

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderReviewDryRunText(t *testing.T) {
	result := testReviewDryRun()

	var out bytes.Buffer
	if err := RenderReviewDryRunText(&out, result); err != nil {
		t.Fatalf("RenderReviewDryRunText: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Quota: block 87%, weekly 64%",
		"Run: run-1",
		"Post mode: dry_run",
		"Outcome: dry_run",
		"## Automated PR Review",
		"Planned actions:",
		"inline_comment-1 inline_comment [planned_only] marker: omitted in dry-run",
		"Artifacts:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want substring %q", text, want)
		}
	}
	if strings.Contains(text, "<!-- codereview:") {
		t.Fatalf("text contains real marker: %q", text)
	}
}

func TestRenderReviewDryRunJSON(t *testing.T) {
	result := testReviewDryRun()

	var out bytes.Buffer
	if err := RenderReviewDryRunJSON(&out, result); err != nil {
		t.Fatalf("RenderReviewDryRunJSON: %v", err)
	}
	var decoded ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Run.RunID != "run-1" || len(decoded.Actions) != 1 || decoded.Actions[0].Kind != "inline_comment" {
		t.Fatalf("decoded = %#v", decoded)
	}
	if bytes.Contains(out.Bytes(), []byte("Quota:")) {
		t.Fatalf("JSON output contains text quota prefix: %s", out.String())
	}
}

func TestRenderReviewDryRunJSONSummaryPreservesNulls(t *testing.T) {
	tokensIn := 1200
	result := testReviewDryRun()
	result.Summary = ReviewSummary{
		Reviewers: []ReviewReviewerSummary{{Name: "go:tests", Findings: 2}},
		Threads:   ReviewThreadCounts{Considered: 1, Summarized: 1},
		Run: ReviewRunSummary{
			ToolVersion:       "0.3.63",
			Adapter:           "claude_cli",
			Model:             "sonnet",
			PostingIdentity:   "review-bot",
			SelectedReviewers: []string{"go:tests"},
			Workstreams: []ReviewWorkstream{{
				Name:     "go:tests",
				Model:    "sonnet",
				TokensIn: &tokensIn,
			}},
		},
	}

	var out bytes.Buffer
	if err := RenderReviewDryRunJSON(&out, result); err != nil {
		t.Fatalf("RenderReviewDryRunJSON: %v", err)
	}
	var decoded struct {
		Summary struct {
			Reviewers []ReviewReviewerSummary `json:"reviewers"`
			Run       struct {
				Workstreams []map[string]json.RawMessage `json:"workstreams"`
				Totals      map[string]json.RawMessage   `json:"totals"`
			} `json:"run"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(decoded.Summary.Reviewers) != 1 || decoded.Summary.Reviewers[0].Findings != 2 {
		t.Fatalf("summary reviewers = %#v", decoded.Summary.Reviewers)
	}
	workstream := decoded.Summary.Run.Workstreams[0]
	if string(workstream["tokens_in"]) != "1200" {
		t.Fatalf("tokens_in = %s, want 1200", workstream["tokens_in"])
	}
	for _, field := range []string{"tokens_out", "cost_usd", "duration_ms"} {
		if string(workstream[field]) != "null" {
			t.Fatalf("workstream %s = %s, want null (never zero)", field, workstream[field])
		}
	}
	if string(decoded.Summary.Run.Totals["cost_usd"]) != "null" {
		t.Fatalf("totals cost_usd = %s, want null", decoded.Summary.Run.Totals["cost_usd"])
	}
}

func testReviewDryRun() ReviewDryRun {
	return ReviewDryRun{
		Run: ReviewRun{
			RunID:        "run-1",
			PRURL:        "https://github.com/open-cli-collective/codereview-cli/pull/29",
			PRKey:        "github.com_open-cli-collective_codereview-cli_29",
			PostMode:     "dry_run",
			Outcome:      "dry_run",
			ArtifactPath: "/tmp/run-1",
		},
		Quota:          &ReviewQuota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		RollupMarkdown: "## Automated PR Review\n\nLooks good.",
		Findings: []ReviewFinding{{
			ID:        "finding-1",
			Severity:  "major",
			FilePath:  "main.go",
			Anchoring: "inline",
			Line:      intPtr(2),
			Body:      "Fix this",
		}},
		Actions: []ReviewAction{{
			ID:            "inline_comment-1",
			Kind:          "inline_comment",
			Status:        "planned_only",
			MarkerOmitted: true,
			Payload:       json.RawMessage(`{"body":"Fix this"}`),
		}},
		Artifacts: ReviewArtifacts{
			Dir:            "/tmp/run-1",
			DiffPatch:      "/tmp/run-1/diff.patch",
			SlicesDir:      "/tmp/run-1/slices",
			FindingsJSON:   "/tmp/run-1/findings.json",
			RollupMarkdown: "/tmp/run-1/rollup.md",
			AgentLogsDir:   "/tmp/run-1/agent-logs",
		},
	}
}
