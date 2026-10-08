package pipeline

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestDirectOpenAIAndCodexCannotUseLegacyAggregateEstimator(t *testing.T) {
	catalog := pipelineCatalogWithRevision(t, "synthetic-request-cost-catalog")
	in, out := 1_000_000, 1_000_000
	// The synthetic choice is deliberately priced by the legacy all-band
	// catalog: runtime provenance, rather than a model-prefix guess, must block it.
	for _, adapter := range []string{"openai_api", "codex_cli"} {
		draft := sessionDraft{Adapter: adapter, Model: "claude-sonnet-5", Response: llm.Response{Usage: llm.Usage{TokensIn: &in, TokensOut: &out, Speed: "standard"}}}
		got := workstreamUsageFromTotalsForCatalog("reviewer", draftWorkstreamTotals(draft), catalog)
		if got.CostUSD != nil || got.CostEstimated {
			t.Fatalf("%s gained legacy pricing: %#v", adapter, got)
		}
	}
}

func TestRequestCostPrimaryRepairHistoriesStayDetachedAndUnpriced(t *testing.T) {
	known := 0.01
	one := &llm.RequestCostEvidence{Version: 1, Source: "openai_responses", Scope: "task_artifact_history", TaskID: "primary", AttemptsObserved: 1, Attempts: []llm.RequestCostAttempt{{GenerationID: "one", Ordinal: 1}}}
	two := llm.CloneRequestCostEvidence(one)
	two.TaskID = "repair"
	two.Gaps = []llm.CostEvidenceGap{llm.CostGapInterrupted}
	drafts := []sessionDraft{
		{Adapter: "openai_api", Model: "first-model", Response: llm.Response{Usage: llm.Usage{CostUSD: &known}, RequestCostEvidence: one}},
		{Adapter: "openai_api", Model: "second-model", Response: llm.Response{RequestCostEvidence: two}},
	}
	totals := combineReviewerWorkstreamTotals(drafts)
	if len(totals.requestHistories) != 2 || totals.requestHistories[0].TaskID != "primary" || totals.requestHistories[1].TaskID != "repair" {
		t.Fatal("task identities merged")
	}
	one.Attempts[0].GenerationID = "mutated"
	if totals.requestHistories[0].Attempts[0].GenerationID != "one" {
		t.Fatal("workstream history aliases source")
	}
	got := workstreamUsageFromTotalsForCatalog("reviewer", totals, nil)
	if got.CostUSD != nil || got.CostEstimated {
		t.Fatal("known subset masked unknown task")
	}
	// A duplicate reference is never sent through a price selector in this slice.
	duplicate := combineReviewerWorkstreamTotals([]sessionDraft{drafts[0], drafts[0]})
	if workstreamUsageFromTotalsForCatalog("reviewer", duplicate, nil).CostUSD != nil {
		t.Fatal("duplicate snapshot charged")
	}
}
