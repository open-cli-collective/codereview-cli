package pipeline

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func TestDryRunSelectionPromptInstructionsStayInsideStructuredPayload(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SelectionPromptInstructions = "Prefer applies_when over prompt wording when routing."
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	if _, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-selection-instructions" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) == 0 {
		t.Fatal("adapter requests = 0, want selection request")
	}
	selectionPrompt := requests[0].Prompt
	if !strings.Contains(selectionPrompt, `"selection_instructions": "Prefer applies_when over prompt wording when routing."`) {
		t.Fatalf("selection prompt missing custom instruction field: %s", selectionPrompt)
	}
	if !strings.Contains(selectionPrompt, `"task": "`+defaultSelectionTask+`"`) {
		t.Fatalf("selection prompt missing stable task field: %s", selectionPrompt)
	}
	if !strings.Contains(selectionPrompt, `"output_contract"`) || !strings.Contains(selectionPrompt, `"schema": "selection"`) {
		t.Fatalf("selection prompt missing structured contract fields: %s", selectionPrompt)
	}
}

func TestSelectionOnlyPromptPreservesRoutingContractWithoutReviewerPromptBodies(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
	}}
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	req.SelectionPromptInstructions = "Prefer applies_when over prompt wording when routing."
	provider.diff.Raw = largeDiff("main.go", "+selection_prompt_should_not_embed_this_unique_hunk_line\n") + smallDiff("other.go")
	provider.pr.Body = "Selection prompt body should stay out of prompt payloads."
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Open thread at main.go:2"}}), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:alpha", "main.go"), 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 2 {
		t.Fatalf("adapter requests = %d, want dossier summary + selection only", len(requests))
	}
	selectionPrompt := requests[1].Prompt
	var payload struct {
		Task                  string                   `json:"task"`
		Schema                string                   `json:"schema"`
		SelectionInstructions string                   `json:"selection_instructions"`
		OutputContract        map[string]any           `json:"output_contract"`
		Agents                []selectionAgentPrompt   `json:"agents"`
		ChangedFiles          []string                 `json:"changed_files"`
		Dossier               selectionPromptDossier   `json:"dossier"`
		Workbench             selectionPromptWorkbench `json:"workbench"`
		Threads               []selectionThreadPrompt  `json:"threads"`
	}
	if err := json.Unmarshal([]byte(selectionPrompt), &payload); err != nil {
		t.Fatalf("unmarshal selection prompt: %v", err)
	}
	if payload.SelectionInstructions != "Prefer applies_when over prompt wording when routing." {
		t.Fatalf("selection instructions = %q, want custom instructions", payload.SelectionInstructions)
	}
	if payload.Task != defaultSelectionTask || payload.Schema != "selection" || payload.OutputContract == nil {
		t.Fatalf("selection prompt envelope = %#v, want task/schema/output contract", payload)
	}
	if !reflect.DeepEqual(payload.ChangedFiles, []string{"main.go", "other.go"}) {
		t.Fatalf("changed files = %#v, want main.go/other.go", payload.ChangedFiles)
	}
	if len(payload.Threads) != 1 || payload.Threads[0].ThreadID != "thread-1" || payload.Threads[0].Path != "main.go" || payload.Threads[0].Summary != "Open thread at main.go:2" {
		t.Fatalf("threads = %#v, want thread-1 on main.go", payload.Threads)
	}
	if !strings.Contains(payload.Dossier.PRIntent, provider.pr.Title) || !strings.Contains(payload.Dossier.PRIntent, provider.pr.Body) {
		t.Fatalf("pr intent = %q, want title and PR body", payload.Dossier.PRIntent)
	}
	if !strings.Contains(payload.Dossier.ChangeMap, "main.go") || !strings.Contains(payload.Dossier.Discussion, "Open thread at main.go:2") {
		t.Fatalf("dossier payload = %#v, want change map and summarized discussion", payload.Dossier)
	}
	if payload.Workbench.Head.SHA != provider.pr.Head.SHA || payload.Workbench.Base.SHA != provider.pr.Base.SHA {
		t.Fatalf("workbench payload = %#v, want review head/base SHAs", payload.Workbench)
	}
	if payload.Workbench.CheckoutMode == "" || payload.Workbench.PR.Number != provider.pr.Ref.Number {
		t.Fatalf("workbench payload = %#v, want checkout mode and PR identity", payload.Workbench)
	}
	if len(payload.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(payload.Agents))
	}
	wantAgents := map[string][]string{
		"harness:alpha": {"Go files changed"},
		"harness:beta":  {"Go files changed"},
	}
	for _, agent := range payload.Agents {
		wantAppliesWhen, ok := wantAgents[agent.ID]
		if !ok {
			t.Fatalf("unexpected selection agent %#v", agent)
		}
		if !reflect.DeepEqual(agent.AppliesWhen, wantAppliesWhen) {
			t.Fatalf("agent applies_when = %#v, want %#v", agent.AppliesWhen, wantAppliesWhen)
		}
		delete(wantAgents, agent.ID)
	}
	if len(wantAgents) != 0 {
		t.Fatalf("missing selection agents = %#v", wantAgents)
	}
	for _, forbidden := range []string{"Review alpha files.", "Review beta files.", `"prompt"`, `"provenance"`, `"overridden"`} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked reviewer execution detail %q: %s", forbidden, selectionPrompt)
		}
	}
	for _, forbidden := range []string{"diff --git", "@@ -", "@@ +"} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked diff hunk content %q: %s", forbidden, selectionPrompt)
		}
	}
	if strings.Contains(selectionPrompt, "selection_prompt_should_not_embed_this_unique_hunk_line") {
		t.Fatalf("selection prompt leaked raw patch content: %s", selectionPrompt)
	}
	for _, forbidden := range []string{result.Artifacts.WorkbenchRepoDir, result.Artifacts.WorkbenchScratch, result.Artifacts.DossierDir, `"source_repo_root"`, `"repo_path"`, `"scratch_path"`, `"metadata_path"`, `"metadata_sha256"`, `"fingerprint_inputs"`, `"root_dir"`, `"index_path"`, `"index_sha256"`} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked harness-only workbench detail %q: %s", forbidden, selectionPrompt)
		}
	}
}

func TestSelectionThreadPromptsFromContextUsesCRSettledSummary(t *testing.T) {
	bot := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	threads, err := threadcontext.Normalize([]gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-1", "main.go", 2, bot, human, "Cached settled summary"),
	}, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	prompts := selectionThreadPromptsFromContext(threads, dossierDiscussionSummaryArtifact{})
	if len(prompts) != 1 {
		t.Fatalf("selection thread prompts = %#v, want one prompt", prompts)
	}
	got := prompts[0]
	if got.ThreadID != "thread-1" || got.Resolved || got.Status != "settled" || got.Summary != "Cached settled summary" {
		t.Fatalf("selection thread prompt = %#v, want unresolved settled cached summary", got)
	}
}

func TestSelectionOutputContractExampleHasNoAgentsWhenCatalogEmpty(t *testing.T) {
	contract := selectionOutputContract(nil, []string{"main.go"}, nil, 3)
	example, ok := contract.Example.(map[string]any)
	if !ok {
		t.Fatalf("Example = %#v, want map", contract.Example)
	}
	selected, ok := example["selected_agents"].([]map[string]any)
	if !ok {
		t.Fatalf("selected_agents = %#v, want []map[string]any", example["selected_agents"])
	}
	if len(selected) != 0 {
		t.Fatalf("selected_agents = %#v, want empty when no agents are allowed", selected)
	}
}

func TestSelectionPromptIncludesMaxSelectedAgentsContract(t *testing.T) {
	prompt, err := buildSelectionPrompt(
		agents.Catalog{Agents: []agents.Agent{{ID: "agent-1"}}},
		selectionPromptInput{ChangedFiles: []string{"main.go"}},
		3,
		"",
	)
	if err != nil {
		t.Fatalf("buildSelectionPrompt: %v", err)
	}
	var payload struct {
		MaxSelectedAgents int `json:"max_selected_agents"`
		OutputContract    struct {
			Instructions  []string       `json:"instructions"`
			AllowedValues map[string]any `json:"allowed_values"`
		} `json:"output_contract"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("Unmarshal prompt: %v", err)
	}
	if payload.MaxSelectedAgents != 3 {
		t.Fatalf("max_selected_agents = %d, want 3", payload.MaxSelectedAgents)
	}
	if got := payload.OutputContract.AllowedValues["max_selected_agents"]; got != float64(3) {
		t.Fatalf("allowed max_selected_agents = %#v, want 3", got)
	}
	if !strings.Contains(strings.Join(payload.OutputContract.Instructions, "\n"), "at most max_selected_agents") {
		t.Fatalf("instructions = %#v, want max-selected-agents guidance", payload.OutputContract.Instructions)
	}
	if !strings.Contains(prompt, "allowed_files") {
		t.Fatalf("selection prompt contract missing allowed_files: %s", prompt)
	}
}

func TestSelectionPromptMarksRepoAgentsRequiredAndUsesDefaultSharedBudget(t *testing.T) {
	catalog := agents.Catalog{Agents: []agents.Agent{
		{ID: "shared-1"},
		{ID: "shared-2"},
		{ID: "shared-3"},
		{ID: "shared-4"},
		{ID: "shared-5"},
		{ID: "shared-6"},
		{ID: "repo", Provenance: agents.Provenance{Kind: agents.SourceRepo}},
	}}
	prompt, err := buildSelectionPrompt(catalog, selectionPromptInput{ChangedFiles: []string{"main.go"}}, 0, "")
	if err != nil {
		t.Fatalf("buildSelectionPrompt: %v", err)
	}
	var payload struct {
		MaxSelectedAgents int                    `json:"max_selected_agents"`
		Agents            []selectionAgentPrompt `json:"agents"`
		OutputContract    struct {
			Instructions  []string       `json:"instructions"`
			AllowedValues map[string]any `json:"allowed_values"`
		} `json:"output_contract"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("Unmarshal prompt: %v", err)
	}
	if payload.MaxSelectedAgents != 6 {
		t.Fatalf("max_selected_agents = %d, want one repo agent plus five shared agents", payload.MaxSelectedAgents)
	}
	if got := payload.OutputContract.AllowedValues["max_shared_agents"]; got != float64(5) {
		t.Fatalf("max_shared_agents = %#v, want 5", got)
	}
	required := map[string]bool{}
	for _, agent := range payload.Agents {
		required[agent.ID] = agent.RequiredIfApplicable
	}
	if !required["repo"] || required["shared-1"] {
		t.Fatalf("required_if_applicable = %#v, want repo only", required)
	}
	instructions := strings.Join(payload.OutputContract.Instructions, "\n")
	if !strings.Contains(instructions, "Select every agent with required_if_applicable=true") || !strings.Contains(instructions, "at most max_shared_agents optional agents") {
		t.Fatalf("instructions = %#v, want required repo plus shared budget guidance", payload.OutputContract.Instructions)
	}
}

func TestRequiredOnMatchFilesMatchesRootAndNestedGoFilesOnly(t *testing.T) {
	agent := agents.Agent{RequiredOnMatch: true, FileGlobs: []string{"**/*.go"}}
	got := requiredOnMatchFiles(agent, []string{"main.go", "internal/app/main.go", "docs/readme.md"})
	want := []string{"main.go", "internal/app/main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required files = %#v, want %#v", got, want)
	}
	if got := requiredOnMatchFiles(agent, []string{"README.md", "docs/review.md"}); len(got) != 0 {
		t.Fatalf("docs-only required files = %#v, want none", got)
	}
}

func TestRequiredOnMatchFilesExcludesNegatedGlobs(t *testing.T) {
	agent := agents.Agent{
		RequiredOnMatch: true,
		FileGlobs: []string{
			"**/*.tsx",
			"!**/*.test.*",
			"!**/*.spec.*",
			"!**/*.stories.*",
		},
	}

	got := requiredOnMatchFiles(agent, []string{
		"Button.tsx",
		"components/Card.tsx",
		"Button.test.tsx",
		"components/Card.spec.tsx",
		"components/Card.stories.tsx",
	})
	want := []string{"Button.tsx", "components/Card.tsx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required files = %#v, want negated test, spec, and story globs excluded: %#v", got, want)
	}
}

func TestSelectionPromptMarksMatchingProfileAgentRequired(t *testing.T) {
	catalog := agents.Catalog{Agents: []agents.Agent{{
		ID:              "shared:go",
		FileGlobs:       []string{"**/*.go"},
		RequiredOnMatch: true,
		Provenance:      agents.Provenance{Kind: agents.SourceProfile},
	}}}
	prompt, err := buildSelectionPrompt(catalog, selectionPromptInput{ChangedFiles: []string{"main.go"}}, 0, "")
	if err != nil {
		t.Fatalf("buildSelectionPrompt: %v", err)
	}
	var payload struct {
		Agents []selectionAgentPrompt `json:"agents"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("Unmarshal prompt: %v", err)
	}
	if len(payload.Agents) != 1 || !payload.Agents[0].RequiredIfApplicable || !reflect.DeepEqual(payload.Agents[0].RequiredFiles, []string{"main.go"}) {
		t.Fatalf("agents = %#v, want matching profile agent required for main.go", payload.Agents)
	}
}

func TestSelectionOutputContractExampleOmitsAllowedFilesForBroadReviewer(t *testing.T) {
	contract := selectionOutputContract([]agents.Agent{{ID: "agent-1"}}, []string{"main.go"}, nil, 3)
	example, ok := contract.Example.(map[string]any)
	if !ok {
		t.Fatalf("Example = %#v, want map", contract.Example)
	}
	selected, ok := example["selected_agents"].([]map[string]any)
	if !ok || len(selected) != 1 {
		t.Fatalf("selected_agents = %#v, want one example reviewer", example["selected_agents"])
	}
	if _, ok := selected[0]["allowed_files"]; ok {
		t.Fatalf("selection example unexpectedly set allowed_files for broad reviewer: %#v", selected[0])
	}
}

func TestFindingsOutputContractScopesAnchorToFindingItems(t *testing.T) {
	contract := findingsOutputContract("agent-1", []string{"main.go"})
	schema, ok := contract.ResponseSchema.(map[string]any)
	if !ok {
		t.Fatalf("response schema type = %T, want map", contract.ResponseSchema)
	}
	if _, ok := schema["anchor"]; ok {
		t.Fatalf("response schema exposes anchor as a top-level field: %#v", schema)
	}
	findingsSchema, ok := schema["findings"].(string)
	if !ok {
		t.Fatalf("findings schema type = %T, want string", schema["findings"])
	}
	if !strings.Contains(findingsSchema, "anchor") {
		t.Fatalf("findings schema does not describe item anchors: %q", findingsSchema)
	}
}

func TestFindingsOutputContractStatesValidatorConstraintLimits(t *testing.T) {
	contract := findingsOutputContract("agent-1", []string{"main.go"})
	instructions := strings.Join(contract.Instructions, "\n")
	for _, want := range []string{
		"constraints must contain at most 10 entries.",
		"Each constraints entry must contain at most 300 Unicode runes.",
	} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("constraints instructions = %q, want %q", instructions, want)
		}
	}
}

func TestRollupPromptPreservesLocationForDedupeWithoutRawAnchors(t *testing.T) {
	prompt, err := buildRollupPrompt(gitprovider.PR{Body: "Rollup prompt body should stay out of prompt payloads."}, []review.Finding{
		{
			ID:       "finding-1",
			Severity: review.SeverityMajor,
			FilePath: "main.go",
			Anchor:   review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 10},
			Body:     "same issue text",
		},
		{
			ID:       "finding-2",
			Severity: review.SeverityMajor,
			FilePath: "main.go",
			Anchor:   review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 20},
			Body:     "same issue text",
		},
	}, nil, []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageCompleteBroad,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
	}})
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}

	var payload struct {
		Findings         []rollupFindingPrompt       `json:"findings"`
		ReviewerCoverage []rollupCoveragePromptEntry `json:"reviewer_coverage"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("unmarshal rollup prompt: %v", err)
	}
	if len(payload.Findings) != 2 {
		t.Fatalf("rollup findings = %d, want 2", len(payload.Findings))
	}
	if len(payload.ReviewerCoverage) != 1 ||
		payload.ReviewerCoverage[0].Status != reviewerCoverageCompleteBroad ||
		!reflect.DeepEqual(payload.ReviewerCoverage[0].InspectedFiles, []string{"main.go"}) {
		t.Fatalf("reviewer coverage = %#v, want compact broad coverage", payload.ReviewerCoverage)
	}
	if payload.Findings[0].Location.Line != 10 || payload.Findings[1].Location.Line != 20 {
		t.Fatalf("rollup finding locations = %#v", payload.Findings)
	}
	if strings.Contains(prompt, `"anchor"`) {
		t.Fatalf("rollup prompt leaked raw anchor key: %s", prompt)
	}
	if strings.Contains(prompt, "Rollup prompt body should stay out of prompt payloads.") {
		t.Fatalf("rollup prompt leaked PR body: %s", prompt)
	}
	for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`, "@@", "+changed implementation body"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("rollup prompt leaked stuffed code context %q: %s", forbidden, prompt)
		}
	}
}

func TestRollupPromptAndFingerprintChangeWhenReviewerCoverageChanges(t *testing.T) {
	findings := largeRollupFindings(1, "main.go", "body")
	baseCoverage := []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageCompleteBroad,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
	}}
	skippedCoverage := []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageIncompleteSkipped,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
		SkippedFiles:   []string{"main.go"},
	}}
	basePrompt, err := buildRollupPrompt(gitprovider.PR{}, findings, nil, baseCoverage)
	if err != nil {
		t.Fatalf("buildRollupPrompt base: %v", err)
	}
	skippedPrompt, err := buildRollupPrompt(gitprovider.PR{}, findings, nil, skippedCoverage)
	if err != nil {
		t.Fatalf("buildRollupPrompt skipped: %v", err)
	}
	if basePrompt == skippedPrompt {
		t.Fatal("rollup prompts are equal, want coverage changes to affect prompt")
	}
	deps := []string{orchestratorSelectionStage, reviewerTaskID("harness:reviewer")}
	baseFingerprint := llmlifecycle.Fingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", basePrompt, deps)
	skippedFingerprint := llmlifecycle.Fingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", skippedPrompt, deps)
	if baseFingerprint == skippedFingerprint {
		t.Fatal("rollup fingerprints are equal, want coverage changes to invalidate cached rollup input")
	}
}

func TestRollupPromptBudgetUsesSynthesisModel(t *testing.T) {
	provider, req := dryRunHarness(t)
	rollupRuntime, err := resolveSynthesisRuntimeConfig(req)
	if err != nil {
		t.Fatalf("resolveSynthesisRuntimeConfig: %v", err)
	}
	prompt, err := buildRollupPrompt(provider.pr, largeRollupFindings(4, "main.go", strings.Repeat("body ", 4000)), nil, nil)
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}
	err = (Options{Budget: ContextBudget{MaxPromptBytes: 10000}}).checkPromptBudget("rollup", "", rollupRuntime.model, "", prompt)
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for rollup model claude-sonnet-5") {
		t.Fatalf("rollup budget error = %v, want synthesis-model budget failure", err)
	}
}

func TestRollupPromptBudgetIgnoresSelectionModelOverride(t *testing.T) {
	provider, req := dryRunHarness(t)
	req.SelectionModelOverride = "bench-model"
	rollupRuntime, err := resolveSynthesisRuntimeConfig(req)
	if err != nil {
		t.Fatalf("resolveSynthesisRuntimeConfig: %v", err)
	}
	prompt, err := buildRollupPrompt(provider.pr, largeRollupFindings(4, "main.go", strings.Repeat("body ", 4000)), nil, nil)
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}
	err = (Options{Budget: ContextBudget{MaxPromptBytes: 10000}}).checkPromptBudget("rollup", "", rollupRuntime.model, "", prompt)
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for rollup model claude-sonnet-5") {
		t.Fatalf("rollup budget error = %v, want default synthesis model despite selection override", err)
	}
}
