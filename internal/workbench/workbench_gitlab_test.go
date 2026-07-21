package workbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func TestPullHeadRef(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		want      string
		wantErr   bool
	}{
		{name: "default pull namespace", namespace: "", want: "refs/pull/371/head"},
		{name: "merge requests namespace", namespace: "merge-requests", want: "refs/merge-requests/371/head"},
		{name: "unsafe namespace", namespace: "../evil", wantErr: true},
		{name: "namespace with space", namespace: "pull head", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pullHeadRef(tt.namespace, 371)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsafeFetchRef) {
					t.Fatalf("pullHeadRef error = %v, want ErrUnsafeFetchRef", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("pullHeadRef error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("pullHeadRef = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBranchRemoteURLAllowsNestedNamespaces(t *testing.T) {
	got, err := branchRemoteURL(gitprovider.PRBranchRef{Host: "gitlab.example.com", Owner: "group/subgroup", Repo: "project"})
	if err != nil {
		t.Fatalf("branchRemoteURL error = %v", err)
	}
	if got != "https://gitlab.example.com/group/subgroup/project.git" {
		t.Fatalf("branchRemoteURL = %q, want nested namespace URL", got)
	}
	for _, owner := range []string{"group//sub", "group/../sub", "group/-", "."} {
		if _, err := branchRemoteURL(gitprovider.PRBranchRef{Host: "gitlab.example.com", Owner: owner, Repo: "project"}); !errors.Is(err, ErrInvalidRepositoryIdentity) {
			t.Fatalf("branchRemoteURL(%q) error = %v, want ErrInvalidRepositoryIdentity", owner, err)
		}
	}
}

func TestPrepareFetchesForkHeadThroughMergeRequestRef(t *testing.T) {
	ctx := context.Background()
	fixture := newGitLabForkWorkbenchFixture(t)
	artifacts := runartifact.FromDir(t.TempDir())
	var fetchedRefs []string
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			fetchedRefs = append(fetchedRefs, cmdArgs[len(cmdArgs)-1])
			if cmdArgs[2] == "https://gitlab.example.com/group/subgroup/project.git" {
				cmdArgs[2] = fixture.baseRemotePath
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

	err := Prepare(ctx, Deps{GitCommand: gitRunner}, Request{
		PRRef:            fixture.pr.Ref,
		ReviewPR:         fixture.pr,
		ChangedFiles:     []string{"main.go"},
		Artifacts:        artifacts,
		HeadRefNamespace: "merge-requests",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.pr.Head.SHA {
		t.Fatalf("workbench HEAD = %q, want fork head %q", got, fixture.pr.Head.SHA)
	}
	if !slices.Contains(fetchedRefs, "refs/merge-requests/371/head") {
		t.Fatalf("fetched refs = %#v, want refs/merge-requests/371/head", fetchedRefs)
	}
}

func newGitLabForkWorkbenchFixture(t *testing.T) forkWorkbenchFixture {
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
	gitCommandMustSucceed(t, forkRemotePath, "push", baseRemotePath, "HEAD:refs/merge-requests/371/head")
	ref := gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group/subgroup", Repo: "project", Number: 371}
	return forkWorkbenchFixture{
		baseRemotePath: baseRemotePath,
		pr: gitprovider.PR{
			Ref: ref, Title: "GitLab fork workbench fixture", URL: "https://gitlab.example.com/group/subgroup/project/-/merge_requests/371", State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "main", Ref: "refs/heads/main", SHA: baseSHA},
			Head: gitprovider.PRBranchRef{Host: ref.Host, Owner: "fork-owner", Repo: "project-fork", Name: "feature", Ref: "refs/heads/feature", SHA: headSHA},
		},
	}
}
