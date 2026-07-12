package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/workbench"
)

func TestSelectionOnlyPreparesWorkbenchInCallerOwnedArtifacts(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	fixture := newWorkbenchGitFixture(t)
	provider, req := dryRunHarness(t)
	provider.pr = fixture.pr
	addRepoAgentFixture(provider)
	provider.diff = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.PRRef = fixture.pr.Ref
	req.PRURL = fixture.pr.URL
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Now:             fixedNow,
		GitCommand:      workbenchGitCommandForTest(req.PRRef, fixture.repoDir),
		ResolveRepoRoot: func(context.Context) (string, error) { return "", reporoot.ErrUnavailable },
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if _, err := os.Stat(result.Artifacts.WorkbenchMetadataPath()); err != nil {
		t.Fatalf("stat workbench metadata: %v", err)
	}
}

func TestDryRunPreparesWorkbenchInAllocatedRunArtifacts(t *testing.T) {
	ctx := context.Background()
	invocationDir := t.TempDir()
	gitCommandMustSucceed(t, invocationDir, "init")
	t.Chdir(invocationDir)
	store := openPipelineStore(t)
	defer closeStore(t, store)
	fixture := newWorkbenchGitFixture(t)
	provider, req := dryRunHarness(t)
	provider.pr = fixture.pr
	addRepoAgentFixture(provider)
	provider.diff = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.PRRef = fixture.pr.Ref
	req.PRURL = fixture.pr.URL
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`, 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", nil), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:   provider,
		Adapter:    adapter,
		Store:      store,
		Layout:     statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:        fixedNow,
		GitCommand: workbenchGitCommandForTest(req.PRRef, fixture.repoDir),
		ResolveRepoRoot: func(context.Context) (string, error) {
			return invocationDir, nil
		},
		NewRunID:        func() string { return "run-workbench" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.baseSHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	var meta workbenchMetadataArtifact
	if err := readJSONFile(result.Artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.RepoPath != result.Artifacts.WorkbenchRepoDir || meta.ScratchPath != result.Artifacts.WorkbenchScratch {
		t.Fatalf("metadata paths = %#v, want allocated run workbench paths", meta)
	}
}

func TestRunReviewerRejectsStaleWorkbenchMetadata(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-reviewer-stale-workbench", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	opts := Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
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
	selection, _, _, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	})
	if err != nil {
		t.Fatalf("runSelectionPhase first: %v", err)
	}
	agent, ok := prepared.catalog.Find(selection.SelectedAgents[0].AgentID)
	if !ok {
		t.Fatalf("selected agent %q missing from catalog", selection.SelectedAgents[0].AgentID)
	}
	if _, _, _, _, err := runReviewer(ctx, opts, req, run.RunID, prepared.reviewPR, prepared.parsed, prepared.artifacts, selection.SelectedAgents[0], agent, []string{orchestratorSelectionStage}); err != nil {
		t.Fatalf("runReviewer first: %v", err)
	}
	metaPath := prepared.artifacts.WorkbenchMetadataPath()
	var meta workbenchMetadataArtifact
	if err := readJSONFile(metaPath, &meta); err != nil {
		t.Fatalf("read workbench metadata: %v", err)
	}
	meta.FingerprintInputs.ChangedFiles = append(meta.FingerprintInputs.ChangedFiles, "extra.go")
	if err := writeJSONFile(metaPath, meta); err != nil {
		t.Fatalf("write workbench metadata: %v", err)
	}
	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := opts
	secondOpts.Adapter = secondAdapter
	_, _, _, _, err = runReviewer(ctx, secondOpts, req, run.RunID, prepared.reviewPR, prepared.parsed, prepared.artifacts, selection.SelectedAgents[0], agent, []string{orchestratorSelectionStage})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("runReviewer stale workbench error = %v, want input fingerprint changed", err)
	}
	if len(secondAdapter.Requests()) != 0 || len(secondAdapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite stale reviewer metadata: starts=%#v resumes=%#v", secondAdapter.Requests(), secondAdapter.Resumes())
	}
}

func TestDryRunReviewerWorkspaceAvoidsContextStuffingBudgetFailures(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		mutate func(t *testing.T, provider *readOnlyProvider, req *Request)
		runID  string
	}{
		{
			name:   "large reviewer diff",
			budget: 9500,
			mutate: func(t *testing.T, provider *readOnlyProvider, _ *Request) {
				t.Helper()
				provider.diff.Raw = largeDiff("main.go", strings.Repeat("+var x = true\n", 1600))
			},
			runID: "run-budget-reviewer-checkout",
		},
		{
			name:   "full-content agent stays within reviewer workspace budget",
			budget: 9500,
			mutate: func(t *testing.T, provider *readOnlyProvider, req *Request) {
				t.Helper()
				dir := t.TempDir()
				writeAgentFullContent(t, dir, "harness", "reviewer")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: "main.go"}] = []byte(strings.Repeat("base\n", 3000))
				provider.files[fileKey{gitRef: provider.pr.Head.SHA, path: "main.go"}] = []byte("package main\n")
			},
			runID: "run-budget-reviewer-workspace-agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			tt.mutate(t, provider, &req)
			adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
			adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))

			result, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return tt.runID },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				Budget:          ContextBudget{MaxPromptBytes: tt.budget},
				MaxConcurrency:  1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			requests := adapter.Requests()
			if len(requests) < 2 {
				t.Fatalf("adapter requests = %#v, want selection and reviewer calls", requests)
			}
			reviewerPrompt := requests[1].Prompt
			for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`} {
				if strings.Contains(reviewerPrompt, forbidden) {
					t.Fatalf("reviewer prompt leaked stuffed code field %q: %s", forbidden, reviewerPrompt)
				}
			}
			if requests[1].ReviewerWorkspace == nil {
				t.Fatalf("reviewer request = %#v, want reviewer workspace", requests[1])
			}
			workspace := requests[1].ReviewerWorkspace
			if !strings.Contains(workspace.RepoDir, filepath.Join("workbench", "reviewers")) ||
				!strings.HasPrefix(workspace.ScratchDir, result.Artifacts.WorkbenchScratch+string(filepath.Separator)) ||
				workspace.MaxToolOutputBytes != 32*1024 {
				t.Fatalf("reviewer workspace request = %#v, want disposable repo/scratch with default cap", workspace)
			}
			if len(result.Findings) != 1 {
				t.Fatalf("findings len = %d, want reviewer success under bounded prompt budget", len(result.Findings))
			}
		})
	}
}
