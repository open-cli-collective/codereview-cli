package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/dossier"
	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/workbench"
)

const (
	dossierSummaryTaskID        = dossier.SummaryTaskID
	dossierSummarySchemaVersion = 1
)

type dossierPreparationRequest = dossier.PreparationRequest
type dossierDiscussionSummaryArtifact = dossier.DiscussionSummary

type dossierIndexArtifact struct {
	HashAlgorithm string                     `json:"hash_algorithm"`
	Files         []dossierIndexFileArtifact `json:"files"`
}

type dossierIndexFileArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func prepareDossierArtifacts(ctx context.Context, opts Options, req dossier.PreparationRequest) error {
	return dossier.Prepare(ctx, dossierEnv(opts, req.Artifacts), req)
}

func readJSONFile(path string, out any) error {
	return fsatomic.ReadJSON(path, out)
}

func writeJSONFile(path string, payload any) error {
	return fsatomic.WriteJSON(path, payload)
}

func repoGuidanceSource(sources []agents.SourceInfo) (agents.SourceInfo, bool) {
	for _, source := range sources {
		if source.Kind == agents.SourceRepo {
			return source, true
		}
	}
	return agents.SourceInfo{}, false
}

func TestSelectionOnlyReusesCachedDossierSummaryTask(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-1",
		Body:   "Top-level concern",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	artifactDir := t.TempDir()

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Compact concern"}, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session-1", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		TaskProgress:    firstProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly first run: %v", err)
	}
	if len(firstProgress.starts) != 2 ||
		firstProgress.starts[0].TaskID != dossierSummaryTaskID || firstProgress.starts[0].Phase != "dossier" ||
		firstProgress.starts[1].TaskID != orchestratorSelectionStage || firstProgress.starts[1].Phase != "selection" {
		t.Fatalf("first progress starts = %#v, want dossier summary and selection execute", firstProgress.starts)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         secondAdapter,
		TaskProgress:    secondProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly second run: %v", err)
	}
	if len(secondProgress.loads) != 2 ||
		secondProgress.loads[0].event.TaskID != dossierSummaryTaskID || secondProgress.loads[0].event.Phase != "dossier" ||
		secondProgress.loads[1].event.TaskID != orchestratorSelectionStage || secondProgress.loads[1].event.Phase != "selection" {
		t.Fatalf("second progress loads = %#v, want cached dossier summary and selection loads", secondProgress.loads)
	}
	if secondProgress.loads[0].result.Usage.TokensIn == nil || *secondProgress.loads[0].result.Usage.TokensIn != 8 ||
		secondProgress.loads[0].result.Usage.TokensOut == nil || *secondProgress.loads[0].result.Usage.TokensOut != 2 {
		t.Fatalf("cached dossier summary usage = %#v, want metadata usage", secondProgress.loads[0].result.Usage)
	}
	if secondProgress.loads[1].result.Usage.TokensIn == nil || *secondProgress.loads[1].result.Usage.TokensIn != 10 ||
		secondProgress.loads[1].result.Usage.TokensOut == nil || *secondProgress.loads[1].result.Usage.TokensOut != 2 {
		t.Fatalf("cached selection usage = %#v, want metadata usage", secondProgress.loads[1].result.Usage)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter requests = %d, want cached dossier summary and selection loads", len(secondAdapter.Requests()))
	}

	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-2",
		Body:   "Changed concern should invalidate caller-owned cache",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	thirdAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	thirdAdapter.Queue(fakeLLMResult("dossier-summary-session-2", discussionSummaryJSON([]string{"Updated concern"}, nil), 8, 2))
	thirdAdapter.Queue(fakeLLMResult("selection-session-3", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	thirdProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         thirdAdapter,
		TaskProgress:    thirdProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly third run: %v", err)
	}
	if len(thirdProgress.starts) != 2 ||
		thirdProgress.starts[0].TaskID != dossierSummaryTaskID || thirdProgress.starts[0].Phase != "dossier" ||
		thirdProgress.starts[1].TaskID != orchestratorSelectionStage || thirdProgress.starts[1].Phase != "selection" {
		t.Fatalf("third progress starts = %#v, want dossier summary and selection rerun after caller-owned artifact change", thirdProgress.starts)
	}
	if len(thirdAdapter.Requests()) != 2 {
		t.Fatalf("third adapter requests = %d, want dossier summary rerun plus selection", len(thirdAdapter.Requests()))
	}
}

func TestSelectionPromptDependenciesTrackDossierAndWorkbenchDigests(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	_, deps1, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts first: %v", err)
	}
	if len(deps1) != 3 || !strings.HasPrefix(deps1[2], "changed_files_metadata=") {
		t.Fatalf("deps = %#v, want dossier index, raw changed-files, and workbench dependencies", deps1)
	}

	indexPath := result.Artifacts.DossierIndexPath()
	if err := os.WriteFile(indexPath, []byte(`{"hash_algorithm":"sha256","files":[{"path":"final/pr-intent.md","sha256":"changed"}]}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", indexPath, err)
	}
	_, deps2, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts dossier change: %v", err)
	}
	if reflect.DeepEqual(deps1, deps2) {
		t.Fatalf("deps after dossier index change = %#v, want changed from %#v", deps2, deps1)
	}

	metaPath := result.Artifacts.WorkbenchMetadataPath()
	var meta workbenchMetadataArtifact
	if err := readJSONFile(metaPath, &meta); err != nil {
		t.Fatalf("read workbench metadata: %v", err)
	}
	meta.FingerprintInputs.ChangedFiles = append(meta.FingerprintInputs.ChangedFiles, "extra.go")
	if err := writeJSONFile(metaPath, meta); err != nil {
		t.Fatalf("write workbench metadata: %v", err)
	}
	_, deps3, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts workbench change: %v", err)
	}
	if reflect.DeepEqual(deps2, deps3) {
		t.Fatalf("deps after workbench metadata change = %#v, want changed from %#v", deps3, deps2)
	}
	inputBefore, _, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads, []string{"main.go"})
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts before raw formatting change: %v", err)
	}
	promptBefore, err := buildSelectionPrompt(agents.Catalog{}, inputBefore, 0, "")
	if err != nil {
		t.Fatalf("buildSelectionPrompt before raw formatting change: %v", err)
	}
	changedFilesPath, err := result.Artifacts.DossierRawPath("changed-files.json")
	if err != nil {
		t.Fatalf("DossierRawPath changed-files: %v", err)
	}
	var rawMetadata any
	if err := readJSONFile(changedFilesPath, &rawMetadata); err != nil {
		t.Fatalf("read raw changed-files metadata: %v", err)
	}
	formattedRaw, err := json.MarshalIndent(rawMetadata, "", "  ")
	if err != nil {
		t.Fatalf("marshal raw changed-files metadata: %v", err)
	}
	if err := os.WriteFile(changedFilesPath, append(formattedRaw, '\n'), 0o600); err != nil {
		t.Fatalf("write formatted raw changed-files metadata: %v", err)
	}
	inputAfter, depsAfterRawChange, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads, []string{"main.go"})
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts after raw formatting change: %v", err)
	}
	promptAfter, err := buildSelectionPrompt(agents.Catalog{}, inputAfter, 0, "")
	if err != nil {
		t.Fatalf("buildSelectionPrompt after raw formatting change: %v", err)
	}
	if promptBefore != promptAfter {
		t.Fatalf("scoped prompt changed after whitespace-only raw metadata change")
	}
	if reflect.DeepEqual(deps3, depsAfterRawChange) {
		t.Fatalf("raw changed-files digest did not invalidate dependencies: %#v", depsAfterRawChange)
	}
	var changedRows []map[string]any
	if err := json.Unmarshal(formattedRaw, &changedRows); err != nil {
		t.Fatalf("decode raw changed-file rows: %v", err)
	}
	if len(changedRows) != 1 {
		t.Fatalf("raw changed-file rows = %#v, want one row", changedRows)
	}
	changedRows[0]["old_path"] = "legacy-main.go"
	changedRows[0]["status"] = "renamed"
	changedRows[0]["additions"] = 17
	changedRows[0]["deletions"] = 3
	changedRows[0]["hunk_count"] = 4
	semanticRaw, err := json.Marshal(changedRows)
	if err != nil {
		t.Fatalf("marshal changed raw file metadata: %v", err)
	}
	if err := os.WriteFile(changedFilesPath, append(semanticRaw, '\n'), 0o600); err != nil {
		t.Fatalf("write semantic raw metadata change: %v", err)
	}
	inputAfterSemanticChange, depsAfterSemanticChange, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads, []string{"main.go"})
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts after metadata change: %v", err)
	}
	promptAfterSemanticChange, err := buildSelectionPrompt(agents.Catalog{}, inputAfterSemanticChange, 0, "")
	if err != nil {
		t.Fatalf("buildSelectionPrompt after metadata change: %v", err)
	}
	if promptAfterSemanticChange == promptAfter {
		t.Fatalf("scoped prompt did not reflect changed status, rename, and file statistics")
	}
	if len(depsAfterSemanticChange) != 3 || depsAfterSemanticChange[2] == depsAfterRawChange[2] {
		t.Fatalf("raw metadata dependency after stats/status/rename change = %#v, want a new changed-files digest", depsAfterSemanticChange)
	}
}

func TestSelectionTaskMetadataDependsOnDossierSummaryTask(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "body"), 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 10, 2))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-selection-deps" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	selectionMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(ArtifactPathsFromDir(result.Run.ArtifactPath)), orchestratorSelectionStage)
	if err != nil || !ok {
		t.Fatalf("read selection metadata: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(selectionMeta.DependencyTaskIDs, []string{dossierSummaryTaskID}) {
		t.Fatalf("selection dependency_task_ids = %#v, want dossier summary dependency", selectionMeta.DependencyTaskIDs)
	}
}

func TestRunSelectionPhaseRejectsStaleSelectionMetadataWhenDossierDigestChanges(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-selection-stale-dossier", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	opts := Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(run.ArtifactPath), nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := workbench.Prepare(ctx, workbenchDeps(opts), workbench.Request{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		t.Fatalf("workbench.Prepare: %v", err)
	}
	if err := prepareDossierArtifacts(ctx, opts, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	if _, _, _, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	}); err != nil {
		t.Fatalf("runSelectionPhase first: %v", err)
	}
	indexPath := prepared.artifacts.DossierIndexPath()
	if err := os.WriteFile(indexPath, []byte(`{"hash_algorithm":"sha256","files":[{"path":"final/pr-intent.md","sha256":"changed"}]}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", indexPath, err)
	}
	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := opts
	secondOpts.Adapter = secondAdapter
	_, _, _, err = runSelectionPhase(ctx, secondOpts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("runSelectionPhase stale dossier error = %v, want input fingerprint changed", err)
	}
	if len(secondAdapter.Requests()) != 0 || len(secondAdapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite stale selection metadata: starts=%#v resumes=%#v", secondAdapter.Requests(), secondAdapter.Resumes())
	}
}
