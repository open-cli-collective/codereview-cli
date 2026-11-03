package pipeline

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestOutputContractsCarrySensitiveValueRule(t *testing.T) {
	relocated := relocationAssignment{
		ManifestDigest:        strings.Repeat("a", 64),
		AssignmentDigest:      strings.Repeat("b", 64),
		MoveCount:             1,
		SymlinkMetadataDigest: strings.Repeat("c", 64),
		SymlinkPaths:          []string{"links/current"},
		BaseOnlySymlinkPaths:  []string{"links/previous"},
	}
	cases := []struct {
		name     string
		contract outputContract
	}{
		{name: "selection", contract: selectionOutputContract(nil, []string{"main.go"}, nil, 0)},
		{name: "selection with max agents", contract: selectionOutputContract(nil, []string{"main.go"}, nil, 2)},
		{name: "findings", contract: findingsOutputContract("agent-1", []string{"main.go"})},
		{name: "findings with relocations", contract: findingsOutputContractWithRelocations("agent-1", []string{"main.go"}, relocated)},
		{name: "rollup", contract: rollupOutputContract([]review.Finding{{ID: "finding-1"}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.Contains(tc.contract.Instructions, llm.SensitiveValueRule) {
				t.Fatalf("%s output contract instructions omit the sensitive value rule:\n%s", tc.name, strings.Join(tc.contract.Instructions, "\n"))
			}
		})
	}
}

// Every model call in a review produces text cr posts or logs, so every prompt
// the adapter receives must carry the rule. Driving one run through each stage
// catches a stage whose prompt is assembled without it.
func TestEveryReviewPromptCarriesSensitiveValueRule(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	provider.threads = []gitprovider.InlineThread{
		markedReviewThread(t, "thread-1", "main.go", 2, req.PostingIdentity, human),
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("thread-analysis-session", `{
		"schema_version": 1,
		"thread_id": "thread-1",
		"decision": "reply_only",
		"reply_body": "Noted.",
		"summary": "",
		"resolve": false,
		"rationale": "reply"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", coverageOnlyJSON("harness:reviewer", nil, []string{"main.go"}, "could not inspect assigned file"), 1, 1))
	adapter.Queue(fakeLLMResult("coverage-repair-session", coverageOnlyJSON("harness:reviewer", nil, []string{"main.go"}, "still could not inspect assigned file"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", nil), 1, 1))

	if _, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-sensitive-value-rule" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	stages := map[string]bool{}
	for _, request := range adapter.Requests() {
		stage := promptStage(request.Prompt)
		stages[stage] = true
		if !promptCarriesSensitiveValueRule(request.Prompt) {
			t.Fatalf("%s prompt omits the sensitive value rule:\n%s", stage, request.Prompt)
		}
	}
	for _, want := range []string{"discussion_summary", "selection", "thread_analysis", "findings", "coverage_repair", "rollup"} {
		if !stages[want] {
			t.Fatalf("stages exercised = %v, want %s", stages, want)
		}
	}
}

func promptStage(prompt string) string {
	switch {
	case strings.HasPrefix(prompt, "Analyze this inline code-review discussion thread."):
		return "thread_analysis"
	case strings.Contains(prompt, `"coverage_repair"`):
		return "coverage_repair"
	}
	var payload struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil || payload.Schema == "" {
		return "unknown"
	}
	return payload.Schema
}

// JSON prompts carry the rule as an encoded string; plain-text prompts carry it
// verbatim.
func promptCarriesSensitiveValueRule(prompt string) bool {
	encoded, err := json.Marshal(llm.SensitiveValueRule)
	if err != nil {
		panic(err)
	}
	return strings.Contains(prompt, llm.SensitiveValueRule) || strings.Contains(prompt, strings.Trim(string(encoded), `"`))
}
