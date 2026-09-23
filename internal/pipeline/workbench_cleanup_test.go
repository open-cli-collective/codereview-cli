package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const workbenchCleanupRunID = "run-workbench-cleanup"

// runWorkbenchCleanupDryRun runs a dry-run review against a real workbench and
// returns the run artifact paths whether or not the run succeeded.
func runWorkbenchCleanupDryRun(t *testing.T, adapter *llm.FakeAdapter, keepWorkbench bool) (ArtifactPaths, error) {
	t.Helper()
	ctx := context.Background()
	invocationDir := t.TempDir()
	gitCommandMustSucceed(t, invocationDir, "init")
	t.Chdir(invocationDir)
	store := openPipelineStore(t)
	t.Cleanup(func() { closeStore(t, store) })
	fixture := newWorkbenchGitFixture(t)
	provider, req := dryRunHarness(t)
	provider.pr = fixture.pr
	addRepoAgentFixture(provider)
	provider.diff = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.PRRef = fixture.pr.Ref
	req.PRURL = fixture.pr.URL
	req.Profile.AgentSources = nil
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	artifacts, err := ArtifactPathsForRun(layout, req.PRRef, fixture.pr, req.ProfileName, runlifecycle.PostingKey(req.PostingIdentity), workbenchCleanupRunID)
	if err != nil {
		t.Fatalf("ArtifactPathsForRun: %v", err)
	}
	_, runErr := dryRunForTest(ctx, Options{
		Provider:   provider,
		Adapter:    adapter,
		Store:      store,
		Layout:     layout,
		Now:        fixedNow,
		GitCommand: workbenchGitCommandForTest(req.PRRef, fixture.repoDir),
		ResolveRepoRoot: func(context.Context) (string, error) {
			return invocationDir, nil
		},
		KeepWorkbench:   keepWorkbench,
		NewRunID:        func() string { return workbenchCleanupRunID },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	return artifacts, runErr
}

func TestDryRunRemovesWorkbenchAfterSuccess(t *testing.T) {
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`, 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", nil), 30, 6))

	artifacts, err := runWorkbenchCleanupDryRun(t, adapter, false)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if _, err := os.Stat(artifacts.WorkbenchDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workbench stat err = %v, want removed after success", err)
	}
	for _, path := range []string{artifacts.FindingsJSON, artifacts.RollupMarkdown} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %s missing after workbench cleanup: %v", path, err)
		}
	}
}

func TestDryRunRetainsWorkbenchAfterFailure(t *testing.T) {
	providerErr := errors.New("selection provider failed")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{SessionID: "selection-failed", WaitErr: providerErr})

	artifacts, err := runWorkbenchCleanupDryRun(t, adapter, false)
	if !errors.Is(err, providerErr) {
		t.Fatalf("DryRun error = %v, want selection provider failure", err)
	}
	if _, err := os.Stat(filepath.Join(artifacts.WorkbenchDir, "repo")); err != nil {
		t.Fatalf("workbench missing after failed run: %v", err)
	}
}

func TestDryRunRetainsWorkbenchWhenKeepWorkbenchEnabled(t *testing.T) {
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`, 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", nil), 30, 6))

	artifacts, err := runWorkbenchCleanupDryRun(t, adapter, true)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifacts.WorkbenchDir, "repo")); err != nil {
		t.Fatalf("workbench missing with KeepWorkbench enabled: %v", err)
	}
}

// TestLiveRetainsWorkbenchUntilPostCompletes pins that a live run keeps its
// checkout, because posting happens after Live returns to the review runner.
func TestLiveRetainsWorkbenchUntilPostCompletes(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live-workbench",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live-workbench"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	if _, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run); err != nil {
		t.Fatalf("Live: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactPath, "workbench", "repo")); err != nil {
		t.Fatalf("workbench removed before the live post: %v", err)
	}
}
