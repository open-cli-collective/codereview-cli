package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestPrepareWorkbenchArtifactsCreatesCleanPinnedCheckoutAndMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.baseSHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchRepoDir, "main.go"), []byte("workspace"), 0o600); err != nil {
		t.Fatalf("write workbench file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchRepoDir, "new-file.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatalf("write new workbench file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchScratch, "note.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	var meta workbenchMetadataArtifact
	if err := readJSONFile(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.SchemaVersion != workbenchMetadataSchemaVersion || meta.CheckoutMode != workbenchCheckoutModeArtifactClone {
		t.Fatalf("metadata = %#v, want schema %d and checkout mode %q", meta, workbenchMetadataSchemaVersion, workbenchCheckoutModeArtifactClone)
	}
	if meta.Base.SHA != fixture.baseSHA || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("metadata refs = %#v/%#v, want base/head fixture SHAs", meta.Base, meta.Head)
	}
	if meta.PR != (workbenchPRIdentity{Host: fixture.pr.Ref.Host, Owner: fixture.pr.Ref.Owner, Repo: fixture.pr.Ref.Repo, Number: fixture.pr.Ref.Number}) {
		t.Fatalf("metadata PR = %#v, want fixture PR identity", meta.PR)
	}
	if meta.SourceRepoRoot != fixture.repoDir {
		t.Fatalf("metadata source repo root = %q, want %q", meta.SourceRepoRoot, fixture.repoDir)
	}
	if meta.RepoPath != artifacts.WorkbenchRepoDir || meta.ScratchPath != artifacts.WorkbenchScratch {
		t.Fatalf("metadata paths = repo %q scratch %q, want artifact workbench paths", meta.RepoPath, meta.ScratchPath)
	}
	if !reflect.DeepEqual(meta.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("metadata changed files = %#v, want main.go", meta.ChangedFiles)
	}
	if meta.FingerprintInputs.PR != meta.PR ||
		meta.FingerprintInputs.BaseSHA != fixture.baseSHA ||
		meta.FingerprintInputs.HeadSHA != fixture.headSHA ||
		meta.FingerprintInputs.CheckoutMode != workbenchCheckoutModeArtifactClone ||
		meta.FingerprintInputs.SourceRepoRoot != fixture.repoDir ||
		!reflect.DeepEqual(meta.FingerprintInputs.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("fingerprint inputs = %#v, want deterministic metadata inputs", meta.FingerprintInputs)
	}
}

func TestDeriveWorkbenchRemoteURLPreservesRemoteStyle(t *testing.T) {
	branch := gitprovider.PRBranchRef{Host: "github.com", Owner: "fork-owner", Repo: "codereview-cli"}

	scpURL, err := deriveWorkbenchRemoteURL("git@github.com:open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive scp remote: %v", err)
	}
	if scpURL != "git@github.com:fork-owner/codereview-cli.git" {
		t.Fatalf("scp remote = %q, want fork-style scp URL", scpURL)
	}

	httpsURL, err := deriveWorkbenchRemoteURL("https://github.com/open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive https remote: %v", err)
	}
	if httpsURL != "https://github.com/fork-owner/codereview-cli.git" {
		t.Fatalf("https remote = %q, want fork-style https URL", httpsURL)
	}
}

func TestPrepareWorkbenchArtifactsFetchesForkHeadFromDerivedRemote(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	var fetchedRemotes []string
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			fetchedRemotes = append(fetchedRemotes, cmdArgs[2])
			switch cmdArgs[2] {
			case "git@github.com:open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			case "git@github.com:fork-owner/codereview-cli-fork.git":
				cmdArgs[2] = fixture.forkRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
		}
		return out, nil
	}

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.pr.Head.SHA {
		t.Fatalf("workbench HEAD = %q, want fork head %q", got, fixture.pr.Head.SHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.pr.Base.SHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	if !slices.Contains(fetchedRemotes, "git@github.com:fork-owner/codereview-cli-fork.git") {
		t.Fatalf("fetched remotes = %#v, want derived fork remote fetch", fetchedRemotes)
	}
}

func TestPrepareWorkbenchArtifactsRejectsMismatchedBaseHostEvenWhenCommitsExistLocally(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	pr := fixture.pr
	pr.Base.Host = "example.com"
	pr.Ref.Host = "example.com"

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, workbenchPreparationRequest{
		PRRef:        pr.Ref,
		ReviewPR:     pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err == nil {
		t.Fatal("prepareWorkbenchArtifacts unexpectedly succeeded for mismatched base host")
	}
	if !strings.Contains(err.Error(), `source repo origin "git@github.com:open-cli-collective/codereview-cli.git" does not match PR base repo open-cli-collective/codereview-cli on example.com`) {
		t.Fatalf("prepareWorkbenchArtifacts error = %v, want host mismatch", err)
	}
}

func TestPrepareWorkbenchArtifactsRejectsUnsafeFetchRef(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	pr := fixture.pr
	pr.Head.Ref = "--upload-pack=/tmp/pwn"
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			switch cmdArgs[2] {
			case "git@github.com:open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			case "git@github.com:fork-owner/codereview-cli-fork.git":
				cmdArgs[2] = fixture.forkRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
		}
		return out, nil
	}

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, workbenchPreparationRequest{
		PRRef:        pr.Ref,
		ReviewPR:     pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err == nil || !strings.Contains(err.Error(), `reject unsafe fetch ref "--upload-pack=/tmp/pwn"`) {
		t.Fatalf("prepareWorkbenchArtifacts error = %v, want unsafe ref rejection", err)
	}
}

func TestPrepareWorkbenchArtifactsRefreshesExistingArtifactRoot(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	req := workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	}
	opts := Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}

	if err := prepareWorkbenchArtifacts(ctx, opts, req); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts first run: %v", err)
	}
	stalePath := filepath.Join(artifacts.WorkbenchDir, "stale.txt")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale artifact: %v", err)
	}
	if err := os.WriteFile(artifacts.WorkbenchMetadataPath(), []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatalf("overwrite stale metadata: %v", err)
	}

	if err := prepareWorkbenchArtifacts(ctx, opts, req); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts second run: %v", err)
	}

	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale artifact stat error = %v, want not exist", err)
	}
	var meta workbenchMetadataArtifact
	if err := readJSONFile(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read refreshed metadata: %v", err)
	}
	if meta.SchemaVersion != workbenchMetadataSchemaVersion || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("refreshed metadata = %#v, want current workbench metadata", meta)
	}
}

func TestSelectionOnlyPreparesWorkbenchInCallerOwnedArtifacts(t *testing.T) {
	ctx := context.Background()
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
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
		ResolveRepoRoot: func(context.Context) (string, error) {
			return fixture.repoDir, nil
		},
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
		Provider: provider,
		Adapter:  adapter,
		Store:    store,
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:      fixedNow,
		ResolveRepoRoot: func(context.Context) (string, error) {
			return fixture.repoDir, nil
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

func TestReviewerWorkspaceSmokeAllowsReadAndWorkspaceWrites(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	adapter := &reviewerWorkspaceSmokeAdapter{}
	opts := Options{Provider: provider, Adapter: adapter}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	llmReq, cleanup, err := buildReviewerWorkspaceRequest(ctx, Options{Adapter: adapter, GitCommand: opts.GitCommand}, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", []string{"main.go"}, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("buildReviewerWorkspaceRequest: %v", err)
	}
	defer func() {
		if cleanup != nil {
			if err := cleanup(); err != nil {
				t.Fatalf("cleanup reviewer workspace: %v", err)
			}
		}
	}()
	type smokeResult struct {
		ReadOK              bool `json:"read_ok"`
		MainContainsChanged bool `json:"main_contains_changed"`
		OutOfScopeReadable  bool `json:"out_of_scope_readable"`
		TrackedWriteOK      bool `json:"tracked_write_ok"`
		UntrackedWriteOK    bool `json:"untracked_write_ok"`
		ScratchWriteOK      bool `json:"scratch_write_ok"`
		MaxToolOutputBytes  int  `json:"max_tool_output_bytes"`
	}
	structured, err := llm.RunStructuredWithSessionResume(context.Background(), adapter, "", llmReq, func(data []byte) (smokeResult, error) {
		var out smokeResult
		return out, json.Unmarshal(data, &out)
	})
	if err != nil {
		t.Fatalf("RunStructured: %v", err)
	}
	got := structured.Value
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want one smoke invocation", len(requests))
	}
	if requests[0].ReviewerWorkspace == nil {
		t.Fatalf("reviewer workspace request = nil")
	}
	if requests[0].ReviewerWorkspace.RepoDir == artifacts.WorkbenchRepoDir {
		t.Fatalf("reviewer workspace repo = canonical workbench repo, want disposable workspace")
	}
	if requests[0].ReviewerWorkspace.MaxToolOutputBytes != defaultReviewerWorkspaceToolOutputBytes {
		t.Fatalf("max tool output bytes = %d, want default %d", requests[0].ReviewerWorkspace.MaxToolOutputBytes, defaultReviewerWorkspaceToolOutputBytes)
	}
	if !got.ReadOK || !got.MainContainsChanged || !got.OutOfScopeReadable || !got.TrackedWriteOK || !got.UntrackedWriteOK || !got.ScratchWriteOK {
		t.Fatalf("smoke result = %#v, want checkout read success plus workspace and scratch writes", got)
	}
	if canonicalMain, err := os.ReadFile(filepath.Join(artifacts.WorkbenchRepoDir, "main.go")); err != nil || strings.Contains(string(canonicalMain), "mutated") { // #nosec G304 -- test reads only caller-produced workbench path.
		t.Fatalf("canonical workbench main.go = %q err %v, want unchanged", canonicalMain, err)
	}
	if status := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "status", "--porcelain")); status != "" {
		t.Fatalf("canonical workbench status = %q, want clean after reviewer writes", status)
	}
}

func TestReviewerWorkspaceAllowedFilesPreservesRealCheckout(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup reviewer workspace: %v", err)
		}
	}()
	if workspace.RepoDir == artifacts.WorkbenchRepoDir {
		t.Fatalf("repo dir = %q, want disposable checkout distinct from workbench repo", workspace.RepoDir)
	}
	if len(workspace.AllowedFiles) != 1 || workspace.AllowedFiles[0] != "main.go" {
		t.Fatalf("allowed files = %#v, want main.go", workspace.AllowedFiles)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")); err != nil { // #nosec G304 -- test reads only caller-produced scope path.
		t.Fatalf("ReadFile(main.go): %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(artifacts.WorkbenchRepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced workbench path.
		t.Fatalf("ReadFile(workbench other.go): %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced workspace path.
		t.Fatalf("ReadFile(workspace other.go): %v", err)
	}
}

func TestReviewerWorkspaceAllowedFilesResetsWorkspace(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:scope-reset", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(first): %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup first reviewer workspace: %v", err)
	}
	workspace, cleanup, err = prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:scope-reset", []string{"other.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(second): %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup second reviewer workspace: %v", err)
		}
	}()
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced scope path.
		t.Fatalf("ReadFile(other.go): %v", err)
	}
	if len(workspace.AllowedFiles) != 1 || workspace.AllowedFiles[0] != "other.go" {
		t.Fatalf("allowed files = %#v, want other.go", workspace.AllowedFiles)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")); err != nil { // #nosec G304 -- test reads only caller-produced workspace path.
		t.Fatalf("ReadFile(main.go): %v", err)
	}
}

func TestReviewerWorkspaceAllowedFilesRejectsSymlinkTargets(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "real.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(real.go): %v", err)
	}
	if err := os.Symlink(filepath.Join(repo, "real.go"), filepath.Join(repo, "link.go")); err != nil {
		t.Fatalf("Symlink(link.go): %v", err)
	}
	err := validateReviewerWorkspaceFileTarget(filepath.Join(repo, "link.go"), "link.go")
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("validateReviewerWorkspaceFileTarget error = %v, want symlink rejection", err)
	}
}

func TestReviewerWorkspaceAllowedFilesRejectsEscapePathsAndCleansUp(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}

	for _, path := range []string{"../main.go", filepath.Join(t.TempDir(), "main.go")} {
		agentID := "harness:escape-" + statepaths.Encode(path)
		workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, agentID, []string{path}, 1024)
		if err == nil {
			if cleanup != nil {
				_ = cleanup()
			}
			t.Fatalf("prepareReviewerWorkspace(%q) = %#v, nil error; want rejection", path, workspace)
		}
		if cleanup != nil {
			t.Fatalf("prepareReviewerWorkspace(%q) cleanup = non-nil, want nil on setup failure", path)
		}
		encoded := statepaths.Encode(agentID)
		if _, statErr := os.Stat(filepath.Join(artifacts.WorkbenchDir, "reviewers", encoded)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reviewer workspace for %q stat err = %v, want cleaned", path, statErr)
		}
		if _, statErr := os.Stat(filepath.Join(artifacts.WorkbenchScratch, encoded)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reviewer scratch for %q stat err = %v, want cleaned", path, statErr)
		}
	}
}

func TestBuildReviewerWorkspaceRequestUnsupportedAdapterFails(t *testing.T) {
	artifacts := ArtifactPathsFromDir(t.TempDir())
	req, cleanup, err := buildReviewerWorkspaceRequest(context.Background(), Options{Adapter: &llm.FakeAdapter{NameValue: "fake-unsupported", ReviewerWorkspaceModeSet: true}}, artifacts, strings.Repeat("1", 40), "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "reviewer workspace capability") {
		t.Fatalf("buildReviewerWorkspaceRequest error = %v, want missing reviewer workspace capability", err)
	}
	if cleanup != nil {
		t.Fatalf("cleanup is non-nil, want nil")
	}
	if req.Model != "" || req.Effort != "" || req.Prompt != "" || req.LogPath != "" || req.ReviewerWorkspace != nil || req.OnValidationRetry != nil {
		t.Fatalf("request = %#v, want zero request on unsupported adapter", req)
	}
}

func TestBuildReviewerWorkspaceRequestAcceptsPermissionBoundedAdapter(t *testing.T) {
	ctx := context.Background()
	provider, reviewReq := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	adapter := &llm.FakeAdapter{
		NameValue:                  "fake-bounded",
		ReviewerWorkspaceModeSet:   true,
		ReviewerWorkspaceModeValue: llm.ReviewerWorkspacePermissionBounded,
	}
	opts := Options{Provider: provider, Adapter: adapter}
	configureWorkbenchFixtureForTest(ctx, &opts, reviewReq.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         reviewReq.PRRef,
		Profile:       reviewReq.Profile,
		AgentDirs:     reviewReq.AgentDirs,
		ReviewBaseSHA: reviewReq.ReviewBaseSHA,
		ReviewHeadSHA: reviewReq.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        reviewReq.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	req, cleanup, err := buildReviewerWorkspaceRequest(ctx, Options{Adapter: adapter, GitCommand: opts.GitCommand}, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("buildReviewerWorkspaceRequest: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup reviewer workspace: %v", err)
		}
	}()
	if req.ReviewerWorkspace == nil {
		t.Fatalf("ReviewerWorkspace = nil")
	}
	if req.ReviewerWorkspace.RepoDir == artifacts.WorkbenchRepoDir || !strings.HasPrefix(req.ReviewerWorkspace.ScratchDir, artifacts.WorkbenchScratch+string(filepath.Separator)) {
		t.Fatalf("ReviewerWorkspace = %#v, want disposable repo and scratch", req.ReviewerWorkspace)
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
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
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
				workspace.MaxToolOutputBytes != defaultReviewerWorkspaceToolOutputBytes {
				t.Fatalf("reviewer workspace request = %#v, want disposable repo/scratch with default cap", workspace)
			}
			if len(result.Findings) != 1 {
				t.Fatalf("findings len = %d, want reviewer success under bounded prompt budget", len(result.Findings))
			}
		})
	}
}
