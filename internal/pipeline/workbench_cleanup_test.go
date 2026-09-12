package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
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
