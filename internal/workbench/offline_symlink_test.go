package workbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/symlinkmetadata"
)

func TestOfflineReviewerRetryPreservesPinnedSymlinkIdentity(t *testing.T) {
	fixture, artifacts, deps := prepareReviewerFixture(t)
	metadata, err := symlinkmetadata.New(fixture.baseSHA, fixture.headSHA, nil)
	if err != nil {
		t.Fatalf("create pinned metadata: %v", err)
	}
	adapter := &llm.FakeAdapter{
		ReviewerWorkspaceModeSet:   true,
		ReviewerWorkspaceModeValue: llm.ReviewerWorkspacePermissionBounded,
	}
	req, cleanup, err := PrepareReviewerRequest(context.Background(), deps, adapter, artifacts, fixture.headSHA, "harness:offline-retry", []string{"main.go"}, "model", "medium", "prompt", filepath.Join(t.TempDir(), "review.jsonl"), ReviewerOptions{Offline: true, SymlinkMetadata: metadata})
	if err != nil {
		t.Fatalf("PrepareReviewerRequest: %v", err)
	}
	t.Cleanup(cleanupForTest(t, cleanup))
	for attempt := range 2 {
		workspace := req.ReviewerWorkspace
		if workspace == nil || !workspace.NoNetwork || workspace.SymlinkMetadataPath != artifacts.SymlinkMetadataJSON || workspace.SymlinkMetadataDigest != metadata.Digest || workspace.BaseSHA != fixture.baseSHA || workspace.HeadSHA != fixture.headSHA || workspace.DiffPath != artifacts.DiffPatch {
			t.Fatalf("attempt %d lost offline or pinned metadata identity: %#v", attempt, workspace)
		}
		if remotes := strings.TrimSpace(gitCommandOutput(t, workspace.RepoDir, "remote")); remotes != "" {
			t.Fatalf("attempt %d remotes = %q, want none", attempt, remotes)
		}
		if attempt == 0 {
			if err := os.WriteFile(filepath.Join(workspace.RepoDir, "untracked"), []byte("dirty"), 0o600); err != nil {
				t.Fatalf("dirty workspace: %v", err)
			}
			if err := req.OnValidationRetry(&req); err != nil {
				t.Fatalf("OnValidationRetry: %v", err)
			}
			if !req.FreshValidationRetrySession {
				t.Fatal("validation retry did not request a fresh session")
			}
			if status := strings.TrimSpace(gitCommandOutput(t, req.ReviewerWorkspace.RepoDir, "status", "--porcelain")); status != "" {
				t.Fatalf("retry workspace status = %q, want clean", status)
			}
		}
	}
}

func TestOfflineReviewerRejectsIncompleteSymlinkIdentity(t *testing.T) {
	for _, missing := range []string{"path", "digest", "base", "head"} {
		t.Run(missing, func(t *testing.T) {
			fixture, artifacts, deps := prepareReviewerFixture(t)
			metadata, err := symlinkmetadata.New(fixture.baseSHA, fixture.headSHA, nil)
			if err != nil {
				t.Fatal(err)
			}
			broken := runartifact.FromDir(artifacts.Dir)
			switch missing {
			case "path":
				broken.SymlinkMetadataJSON = ""
			case "digest":
				metadata.Digest = ""
			case "base":
				metadata.BaseSHA = ""
			case "head":
				metadata.HeadSHA = ""
			}
			workspace, cleanup, err := prepareReviewerWorkspace(context.Background(), deps, broken, fixture.headSHA, "harness:offline-invalid", []string{"main.go"}, 1024, ReviewerOptions{Offline: true, SymlinkMetadata: metadata})
			if cleanup != nil {
				t.Cleanup(cleanupForTest(t, cleanup))
			}
			if err == nil || !strings.Contains(err.Error(), "pinned symlink metadata identity is required") || cleanup != nil || workspace.RepoDir != "" {
				t.Fatalf("missing %s returned workspace %#v, cleanup present %t, error %v", missing, workspace, cleanup != nil, err)
			}
		})
	}
}
