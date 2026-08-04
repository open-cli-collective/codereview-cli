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
	"github.com/open-cli-collective/codereview-cli/internal/gittest"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestPrepareCreatesCleanPinnedCheckoutAndMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())

	err := Prepare(ctx, Deps{
		GitCommand: testGitRunner(t, map[string]string{
			"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
		}),
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
	if meta.SourceRepoRoot != "" {
		t.Fatalf("metadata source repo root = %q, want omitted", meta.SourceRepoRoot)
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
		!reflect.DeepEqual(meta.FingerprintInputs.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("fingerprint inputs = %#v, want deterministic metadata inputs", meta.FingerprintInputs)
	}
}

func TestPrepareFetchesForkHeadThroughBasePullRef(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	var fetchedRemotes []string
	var fetchedRefs []string
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			fetchedRemotes = append(fetchedRemotes, cmdArgs[2])
			fetchedRefs = append(fetchedRefs, cmdArgs[len(cmdArgs)-1])
			switch cmdArgs[2] {
			case "https://github.com/open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		cmd.Env = gittest.Env()
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
		GitCommand: gitRunner,
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
	if slices.Contains(fetchedRemotes, "https://github.com/fork-owner/codereview-cli-fork.git") || !slices.Contains(fetchedRefs, "refs/pull/371/head") {
		t.Fatalf("fetches = remotes %#v refs %#v, want base remote PR-head ref", fetchedRemotes, fetchedRefs)
	}
}

func TestPrepareRejectsUnsafeFetchRef(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	pr := fixture.pr
	pr.Base.Ref = "--upload-pack=/tmp/pwn"
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			switch cmdArgs[2] {
			case "https://github.com/open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		cmd.Env = gittest.Env()
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
		GitCommand: gitRunner,
	}, Request{PRRef: pr.Ref, ReviewPR: pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts})
	if !errors.Is(err, ErrUnsafeFetchRef) {
		t.Fatalf("Prepare error = %v, want unsafe ref rejection", err)
	}
}

func TestPrepareRefreshesExistingArtifactRoot(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	req := Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts}
	deps := Deps{GitCommand: testGitRunner(t, map[string]string{
		"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
	})}

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

func TestPrepareReusesValidV1WorkbenchWithoutRewritingMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	req := Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts}
	remote := "https://github.com/open-cli-collective/codereview-cli.git"
	if err := Prepare(ctx, Deps{GitCommand: testGitRunner(t, map[string]string{remote: fixture.repoDir})}, req); err != nil {
		t.Fatalf("Prepare first run: %v", err)
	}
	metadataPath := artifacts.WorkbenchMetadataPath()
	data, err := os.ReadFile(metadataPath) // #nosec G304 -- test-controlled artifact path.
	if err != nil {
		t.Fatal(err)
	}
	v1 := strings.Replace(string(data), `"schema_version": 2,`, `"schema_version": 1,`+"\n  \"source_repo_root\": \"/old/unrelated/checkout\",", 1)
	if err := os.WriteFile(metadataPath, []byte(v1), 0o600); err != nil { // #nosec G703 -- test-controlled artifact path.
		t.Fatal(err)
	}
	baseRunner := testGitRunner(t, nil)
	reuseRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && (args[0] == "init" || args[0] == "fetch") {
			return nil, fmt.Errorf("unexpected rebuild command: git %s", strings.Join(args, " "))
		}
		return baseRunner(ctx, dir, args...)
	}
	if err := Prepare(ctx, Deps{GitCommand: reuseRunner}, req); err != nil {
		t.Fatalf("Prepare reuse: %v", err)
	}
	got, err := os.ReadFile(metadataPath) // #nosec G304 -- test-controlled artifact path.
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != v1 {
		t.Fatalf("v1 metadata was rewritten\ngot: %s\nwant: %s", got, v1)
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
	if requests[0].ReviewerWorkspace.DiffPath != artifacts.DiffPatch {
		t.Fatalf("fixed diff path = %q, want %q", requests[0].ReviewerWorkspace.DiffPath, artifacts.DiffPatch)
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

func TestReviewerWorkspaceAllowedFilesAcceptsDeletedPaths(t *testing.T) {
	fixture := newWorkbenchGitFixture(t)
	gitCommandMustSucceed(t, fixture.repoDir, "rm", "other.go")
	gitCommandMustSucceed(t, fixture.repoDir, "commit", "-m", "delete other.go")
	fixture.headSHA = strings.TrimSpace(gitCommandOutput(t, fixture.repoDir, "rev-parse", "HEAD"))
	fixture.pr.Head.SHA = fixture.headSHA
	artifacts := runartifact.FromDir(t.TempDir())
	deps := Deps{GitCommand: testGitRunner(t, map[string]string{
		"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
	})}
	if err := Prepare(context.Background(), deps, Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go", "other.go"}, Artifacts: artifacts}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:deleted", []string{"other.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	if _, err := os.Stat(filepath.Join(workspace.RepoDir, "main.go")); err != nil {
		t.Fatalf("Stat(main.go): %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.RepoDir, "other.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(other.go) error = %v, want file absent at head", err)
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

func TestReviewerWorkspaceAllowedFilesAcceptsSymlinkTargets(t *testing.T) {
	fixture := newWorkbenchGitFixture(t)
	if err := os.Remove(filepath.Join(fixture.repoDir, "other.go")); err != nil {
		t.Fatalf("Remove(other.go): %v", err)
	}
	if err := os.Symlink("main.go", filepath.Join(fixture.repoDir, "other.go")); err != nil {
		t.Fatalf("Symlink(other.go): %v", err)
	}
	gitCommandMustSucceed(t, fixture.repoDir, "add", "other.go")
	gitCommandMustSucceed(t, fixture.repoDir, "commit", "-m", "replace other.go with symlink")
	fixture.headSHA = strings.TrimSpace(gitCommandOutput(t, fixture.repoDir, "rev-parse", "HEAD"))
	fixture.pr.Head.SHA = fixture.headSHA
	artifacts := runartifact.FromDir(t.TempDir())
	deps := Deps{GitCommand: testGitRunner(t, map[string]string{
		"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
	})}
	if err := Prepare(context.Background(), deps, Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"other.go"}, Artifacts: artifacts}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:symlink", []string{"other.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	info, err := os.Lstat(filepath.Join(workspace.RepoDir, "other.go"))
	if err != nil {
		t.Fatalf("Lstat(other.go): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("other.go mode = %v, want symlink", info.Mode())
	}
}

func TestReviewerWorkspaceAllowedFilesAcceptsSubmoduleTargets(t *testing.T) {
	fixture := newWorkbenchGitFixture(t)
	gitCommandMustSucceed(t, fixture.repoDir, "update-index", "--add", "--cacheinfo", "160000,"+fixture.baseSHA+",vendor/shared")
	gitCommandMustSucceed(t, fixture.repoDir, "commit", "-m", "add submodule entry")
	fixture.headSHA = strings.TrimSpace(gitCommandOutput(t, fixture.repoDir, "rev-parse", "HEAD"))
	fixture.pr.Head.SHA = fixture.headSHA
	artifacts := runartifact.FromDir(t.TempDir())
	deps := Deps{GitCommand: testGitRunner(t, map[string]string{
		"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
	})}
	if err := Prepare(context.Background(), deps, Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"vendor/shared"}, Artifacts: artifacts}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, fixture.headSHA, "harness:submodule", []string{"vendor/shared"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	info, err := os.Stat(filepath.Join(workspace.RepoDir, "vendor", "shared"))
	if err != nil {
		t.Fatalf("Stat(vendor/shared): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("vendor/shared mode = %v, want submodule directory", info.Mode())
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
	if req.ReviewerWorkspace.DiffPath != artifacts.DiffPatch {
		t.Fatalf("fixed diff path = %q, want %q", req.ReviewerWorkspace.DiffPath, artifacts.DiffPatch)
	}
}

func TestPrepareReviewerRequestValidationRetryGetsFreshWorkspaceWithSameFixedDiff(t *testing.T) {
	fixture, artifacts, deps := prepareReviewerFixture(t)
	adapter := &llm.FakeAdapter{
		ReviewerWorkspaceModeSet:   true,
		ReviewerWorkspaceModeValue: llm.ReviewerWorkspacePermissionBounded,
	}
	req, cleanup, err := PrepareReviewerRequest(context.Background(), deps, adapter, artifacts, fixture.headSHA, "harness:retry", []string{"main.go"}, "model", "medium", "prompt", filepath.Join(t.TempDir(), "review.jsonl"))
	if err != nil {
		t.Fatalf("PrepareReviewerRequest: %v", err)
	}
	defer cleanupForTest(t, cleanup)
	firstRepo := req.ReviewerWorkspace.RepoDir
	if err := os.WriteFile(filepath.Join(firstRepo, "untracked"), []byte("dirty"), 0o600); err != nil {
		t.Fatalf("WriteFile(untracked): %v", err)
	}
	if err := req.OnValidationRetry(&req); err != nil {
		t.Fatalf("OnValidationRetry: %v", err)
	}
	if !req.FreshValidationRetrySession || req.ReviewerWorkspace == nil {
		t.Fatalf("retry request = %#v, want fresh reviewer session", req)
	}
	if _, err := os.Stat(filepath.Join(firstRepo, "untracked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first workspace stat error = %v, want cleaned", err)
	}
	if _, err := os.Stat(filepath.Join(req.ReviewerWorkspace.RepoDir, "untracked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry workspace stat error = %v, want clean", err)
	}
	if req.ReviewerWorkspace.DiffPath != artifacts.DiffPatch {
		t.Fatalf("retry fixed diff = %q, want %q", req.ReviewerWorkspace.DiffPath, artifacts.DiffPatch)
	}
}

type workbenchGitFixture struct {
	repoDir string
	baseSHA string
	headSHA string
	pr      gitprovider.PR
}

type forkWorkbenchFixture struct {
	baseRemotePath string
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
	gitCommandMustSucceed(t, forkRemotePath, "push", baseRemotePath, "HEAD:refs/pull/371/head")
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 371}
	return forkWorkbenchFixture{
		baseRemotePath: baseRemotePath,
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
	deps := Deps{GitCommand: testGitRunner(t, map[string]string{
		"https://github.com/open-cli-collective/codereview-cli.git": fixture.repoDir,
	})}
	if err := Prepare(context.Background(), deps, Request{PRRef: fixture.pr.Ref, ReviewPR: fixture.pr, ChangedFiles: []string{"main.go"}, Artifacts: artifacts}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return fixture, artifacts, deps
}

func testGitRunner(t *testing.T, remotes map[string]string) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			if local, ok := remotes[cmdArgs[2]]; ok {
				cmdArgs[2] = local
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed arguments.
		cmd.Env = gittest.Env()
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
		}
		return out, nil
	}
}

func gitCommandMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitCommandOutput(t, dir, args...))
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	cmd.Env = gittest.Env()
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
