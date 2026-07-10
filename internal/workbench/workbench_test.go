package workbench

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
	"sync"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestPrepareCreatesCleanPinnedCheckoutAndMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())

	err := Prepare(ctx, Deps{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, Request{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
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

	var meta metadataArtifact
	if err := readJSON(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.SchemaVersion != metadataSchemaVersion || meta.CheckoutMode != checkoutModeArtifactClone {
		t.Fatalf("metadata = %#v, want schema %d and checkout mode %q", meta, metadataSchemaVersion, checkoutModeArtifactClone)
	}
	if meta.Base.SHA != fixture.baseSHA || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("metadata refs = %#v/%#v, want base/head fixture SHAs", meta.Base, meta.Head)
	}
	if meta.PR != (prIdentity{Host: fixture.pr.Ref.Host, Owner: fixture.pr.Ref.Owner, Repo: fixture.pr.Ref.Repo, Number: fixture.pr.Ref.Number}) {
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
		meta.FingerprintInputs.CheckoutMode != checkoutModeArtifactClone ||
		meta.FingerprintInputs.SourceRepoRoot != fixture.repoDir ||
		!reflect.DeepEqual(meta.FingerprintInputs.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("fingerprint inputs = %#v, want deterministic metadata inputs", meta.FingerprintInputs)
	}
}

func TestDeriveRemoteURLPreservesRemoteStyle(t *testing.T) {
	branch := gitprovider.PRBranchRef{Host: "github.com", Owner: "fork-owner", Repo: "codereview-cli"}

	scpURL, err := deriveRemoteURL("git@github.com:open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive scp remote: %v", err)
	}
	if scpURL != "git@github.com:fork-owner/codereview-cli.git" {
		t.Fatalf("scp remote = %q, want fork-style scp URL", scpURL)
	}

	httpsURL, err := deriveRemoteURL("https://github.com/open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive https remote: %v", err)
	}
	if httpsURL != "https://github.com/fork-owner/codereview-cli.git" {
		t.Fatalf("https remote = %q, want fork-style https URL", httpsURL)
	}
}

func TestPrepareFetchesForkHeadFromDerivedRemote(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
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

	err := Prepare(ctx, Deps{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, Request{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
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

func TestPrepareRejectsMismatchedBaseHostEvenWhenCommitsExistLocally(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	pr := fixture.pr
	pr.Base.Host = "example.com"
	pr.Ref.Host = "example.com"

	err := Prepare(ctx, Deps{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, Request{PRRef: pr.Ref, ReviewPR: pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts})
	if err == nil {
		t.Fatal("Prepare unexpectedly succeeded for mismatched base host")
	}
	if !strings.Contains(err.Error(), `source repo origin "git@github.com:open-cli-collective/codereview-cli.git" does not match PR base repo open-cli-collective/codereview-cli on example.com`) {
		t.Fatalf("Prepare error = %v, want host mismatch", err)
	}
}

func TestPrepareRejectsUnsafeFetchRef(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
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

	err := Prepare(ctx, Deps{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, Request{PRRef: pr.Ref, ReviewPR: pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts})
	if err == nil || !strings.Contains(err.Error(), `reject unsafe fetch ref "--upload-pack=/tmp/pwn"`) {
		t.Fatalf("Prepare error = %v, want unsafe ref rejection", err)
	}
}

func TestPrepareRefreshesExistingArtifactRoot(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	req := Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts}
	deps := Deps{ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil }}

	if err := Prepare(ctx, deps, req); err != nil {
		t.Fatalf("Prepare first run: %v", err)
	}
	stalePath := filepath.Join(artifacts.WorkbenchDir, "stale.txt")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale artifact: %v", err)
	}
	if err := os.WriteFile(artifacts.WorkbenchMetadataPath(), []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatalf("overwrite stale metadata: %v", err)
	}

	if err := Prepare(ctx, deps, req); err != nil {
		t.Fatalf("Prepare second run: %v", err)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale artifact stat error = %v, want not exist", err)
	}
	var meta metadataArtifact
	if err := readJSON(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read refreshed metadata: %v", err)
	}
	if meta.SchemaVersion != metadataSchemaVersion || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("refreshed metadata = %#v, want current workbench metadata", meta)
	}
}

func TestReviewerWorkspaceSmokeAllowsReadAndWorkspaceWrites(t *testing.T) {
	ctx := context.Background()
	fixture, artifacts, deps := prepareReviewerFixture(t)
	adapter := &reviewerWorkspaceSmokeAdapter{}
	llmReq, cleanup, err := PrepareReviewerRequest(ctx, deps, adapter, artifacts, fixture.headSHA, "harness:smoke", []string{"main.go"}, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("PrepareReviewerRequest: %v", err)
	}
	defer cleanupForTest(t, cleanup)

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
	if len(requests) != 1 || requests[0].ReviewerWorkspace == nil {
		t.Fatalf("adapter requests = %#v, want one workspace invocation", requests)
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
	fixture, artifacts, deps := prepareReviewerFixture(t)
	workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:smoke", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	if workspace.RepoDir == artifacts.WorkbenchRepoDir {
		t.Fatalf("repo dir = %q, want disposable checkout distinct from workbench repo", workspace.RepoDir)
	}
	if !reflect.DeepEqual(workspace.AllowedFiles, []string{"main.go"}) {
		t.Fatalf("allowed files = %#v, want main.go", workspace.AllowedFiles)
	}
	for _, path := range []string{filepath.Join(workspace.RepoDir, "main.go"), filepath.Join(workspace.RepoDir, "other.go"), filepath.Join(artifacts.WorkbenchRepoDir, "other.go")} {
		if _, err := os.ReadFile(path); err != nil { // #nosec G304 -- test reads only fixture paths.
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
	}
}

func TestReviewerWorkspaceAllowedFilesResetsWorkspace(t *testing.T) {
	fixture, artifacts, deps := prepareReviewerFixture(t)
	workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:scope-reset", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(first): %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup first reviewer workspace: %v", err)
	}
	workspace, cleanup, err = prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:scope-reset", []string{"other.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(second): %v", err)
	}
	defer cleanupForTest(t, cleanup)
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only fixture paths.
		t.Fatalf("ReadFile(other.go): %v", err)
	}
	if !reflect.DeepEqual(workspace.AllowedFiles, []string{"other.go"}) {
		t.Fatalf("allowed files = %#v, want other.go", workspace.AllowedFiles)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")); err != nil { // #nosec G304 -- test reads only fixture paths.
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
	fixture, artifacts, deps := prepareReviewerFixture(t)
	for _, path := range []string{"../main.go", filepath.Join(t.TempDir(), "main.go")} {
		agentID := "harness:escape-" + statepaths.Encode(path)
		workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, agentID, []string{path}, 1024)
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

func TestPrepareReviewerRequestUnsupportedAdapterFails(t *testing.T) {
	artifacts := runartifact.FromDir(t.TempDir())
	req, cleanup, err := PrepareReviewerRequest(context.Background(), Deps{}, &llm.FakeAdapter{NameValue: "fake-unsupported", ReviewerWorkspaceModeSet: true}, artifacts, strings.Repeat("1", 40), "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "reviewer workspace capability") {
		t.Fatalf("PrepareReviewerRequest error = %v, want missing reviewer workspace capability", err)
	}
	if cleanup != nil {
		t.Fatalf("cleanup is non-nil, want nil")
	}
	if req.Model != "" || req.Effort != "" || req.Prompt != "" || req.LogPath != "" || req.ReviewerWorkspace != nil || req.OnValidationRetry != nil {
		t.Fatalf("request = %#v, want zero request on unsupported adapter", req)
	}
}

func TestPrepareReviewerRequestAcceptsPermissionBoundedAdapter(t *testing.T) {
	fixture, artifacts, deps := prepareReviewerFixture(t)
	adapter := &llm.FakeAdapter{
		NameValue:                  "fake-bounded",
		ReviewerWorkspaceModeSet:   true,
		ReviewerWorkspaceModeValue: llm.ReviewerWorkspacePermissionBounded,
	}
	req, cleanup, err := PrepareReviewerRequest(context.Background(), deps, adapter, artifacts, fixture.headSHA, "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("PrepareReviewerRequest: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	if req.ReviewerWorkspace == nil {
		t.Fatalf("ReviewerWorkspace = nil")
	}
	if req.ReviewerWorkspace.RepoDir == artifacts.WorkbenchRepoDir || !strings.HasPrefix(req.ReviewerWorkspace.ScratchDir, artifacts.WorkbenchScratch+string(filepath.Separator)) {
		t.Fatalf("ReviewerWorkspace = %#v, want disposable repo and scratch", req.ReviewerWorkspace)
	}
}

type workbenchGitFixture struct {
	repoDir string
	baseSHA string
	headSHA string
	pr      gitprovider.PR
}

type forkWorkbenchFixture struct {
	sourceRepoDir  string
	baseRemotePath string
	forkRemotePath string
	pr             gitprovider.PR
}

func newWorkbenchGitFixture(t *testing.T) workbenchGitFixture {
	t.Helper()
	repoDir := t.TempDir()
	gitCommandMustSucceed(t, repoDir, "init", "-b", "main")
	gitCommandMustSucceed(t, repoDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, repoDir, "config", "user.email", "workbench@example.com")
	gitCommandMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\n\nvar helper = true\n"), 0o600); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "add", "main.go", "other.go")
	gitCommandMustSucceed(t, repoDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	gitCommandMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 370}
	return workbenchGitFixture{
		repoDir: repoDir,
		baseSHA: baseSHA,
		headSHA: headSHA,
		pr: gitprovider.PR{
			Ref: ref, Title: "Workbench fixture", URL: "https://github.com/open-cli-collective/codereview-cli/pull/370", State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "main", Ref: "refs/heads/main", SHA: baseSHA},
			Head: gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "feature", Ref: "refs/heads/feature", SHA: headSHA},
		},
	}
}

func newForkWorkbenchFixture(t *testing.T) forkWorkbenchFixture {
	t.Helper()
	baseSeedDir := t.TempDir()
	gitCommandMustSucceed(t, baseSeedDir, "init", "-b", "main")
	gitCommandMustSucceed(t, baseSeedDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, baseSeedDir, "config", "user.email", "workbench@example.com")
	if err := os.WriteFile(filepath.Join(baseSeedDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	gitCommandMustSucceed(t, baseSeedDir, "add", "main.go")
	gitCommandMustSucceed(t, baseSeedDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(gitCommandOutput(t, baseSeedDir, "rev-parse", "HEAD"))
	baseRemotePath := filepath.Join(t.TempDir(), "base-remote.git")
	gitCommandMustSucceed(t, "", "clone", "--bare", baseSeedDir, baseRemotePath)
	sourceRepoDir := filepath.Join(t.TempDir(), "source")
	gitCommandMustSucceed(t, "", "clone", baseRemotePath, sourceRepoDir)
	gitCommandMustSucceed(t, sourceRepoDir, "remote", "set-url", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	forkRemotePath := filepath.Join(t.TempDir(), "fork-remote.git")
	gitCommandMustSucceed(t, "", "clone", baseRemotePath, forkRemotePath)
	gitCommandMustSucceed(t, forkRemotePath, "checkout", "-b", "feature")
	gitCommandMustSucceed(t, forkRemotePath, "config", "user.name", "Fork Workbench Test")
	gitCommandMustSucceed(t, forkRemotePath, "config", "user.email", "fork@example.com")
	if err := os.WriteFile(filepath.Join(forkRemotePath, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update fork main.go: %v", err)
	}
	gitCommandMustSucceed(t, forkRemotePath, "commit", "-am", "fork head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, forkRemotePath, "rev-parse", "HEAD"))
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 371}
	return forkWorkbenchFixture{
		sourceRepoDir: sourceRepoDir, baseRemotePath: baseRemotePath, forkRemotePath: forkRemotePath,
		pr: gitprovider.PR{
			Ref: ref, Title: "Fork workbench fixture", URL: "https://github.com/open-cli-collective/codereview-cli/pull/371", State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "main", Ref: "refs/heads/main", SHA: baseSHA},
			Head: gitprovider.PRBranchRef{Host: ref.Host, Owner: "fork-owner", Repo: "codereview-cli-fork", Name: "feature", Ref: "refs/heads/feature", SHA: headSHA},
		},
	}
}

func prepareReviewerFixture(t *testing.T) (workbenchGitFixture, runartifact.Paths, Deps) {
	t.Helper()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	deps := Deps{ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil }}
	if err := Prepare(context.Background(), deps, Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return fixture, artifacts, deps
}

func gitCommandMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitCommandOutput(t, dir, args...))
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path) // #nosec G304 -- test reads only caller-owned artifacts.
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func cleanupForTest(t *testing.T, cleanup func() error) func() {
	t.Helper()
	return func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup reviewer workspace: %v", err)
		}
	}
}

type reviewerWorkspaceSmokeAdapter struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (a *reviewerWorkspaceSmokeAdapter) Name() string         { return "reviewer-workspace-smoke" }
func (a *reviewerWorkspaceSmokeAdapter) SupportsResume() bool { return false }
func (a *reviewerWorkspaceSmokeAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}
func (a *reviewerWorkspaceSmokeAdapter) SupportsCacheAccounting() bool { return false }
func (a *reviewerWorkspaceSmokeAdapter) SupportsCostReporting() bool   { return false }
func (a *reviewerWorkspaceSmokeAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}
func (a *reviewerWorkspaceSmokeAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("resume unsupported")
}
func (a *reviewerWorkspaceSmokeAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	workspace := req.ReviewerWorkspace
	if workspace == nil {
		return nil, errors.New("missing reviewer workspace request")
	}
	mainBytes, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")) // #nosec G304 -- test adapter reads only caller-provided test workspace roots.
	if err != nil {
		return nil, err
	}
	_, otherReadErr := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go"))                                          // #nosec G304 -- test adapter probes only caller-provided test workspace roots.
	trackedWriteErr := os.WriteFile(filepath.Join(workspace.RepoDir, "main.go"), []byte("mutated"), 0o600)                // #nosec G304,G306 -- test adapter intentionally probes disposable workspace writes.
	untrackedWriteErr := os.WriteFile(filepath.Join(workspace.RepoDir, "untracked.txt"), []byte("mutated"), 0o600)        // #nosec G304,G306 -- test adapter intentionally probes disposable workspace writes.
	scratchWriteErr := os.WriteFile(filepath.Join(workspace.ScratchDir, "smoke-output.txt"), []byte("scratch-ok"), 0o600) // #nosec G306 -- test adapter writes only to caller-owned scratch.
	output := fmt.Sprintf(`{"read_ok":true,"main_contains_changed":%t,"out_of_scope_readable":%t,"tracked_write_ok":%t,"untracked_write_ok":%t,"scratch_write_ok":%t,"max_tool_output_bytes":%d}`,
		strings.Contains(string(mainBytes), "var changed = true"), otherReadErr == nil, trackedWriteErr == nil, untrackedWriteErr == nil, scratchWriteErr == nil, workspace.MaxToolOutputBytes)
	return smokeStream{output: output}, nil
}
func (a *reviewerWorkspaceSmokeAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

type smokeStream struct{ output string }

func (smokeStream) SessionID() string { return "workspace-smoke-session" }
func (s smokeStream) Wait(context.Context) (llm.Response, error) {
	return llm.Response{StructuredOutput: []byte(s.output)}, nil
}
