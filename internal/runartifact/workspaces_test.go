package runartifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// windowsGitDirLimit mirrors Git for Windows: PATH_MAX 260 minus the 40
// characters setup.c reserves when checking an explicit GIT_DIR.
const windowsGitDirLimit = 220

func workspaceTestRun(t *testing.T, dataRoot, owner, repo string, number int, runID string) Paths {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: owner, Repo: repo, Number: number}
	pr := gitprovider.PR{
		Ref:  ref,
		Head: gitprovider.PRBranchRef{SHA: "0123456789abcdef0123456789abcdef01234567"},
		Base: gitprovider.PRBranchRef{SHA: "fedcba9876543210fedcba9876543210fedcba98"},
	}
	paths, err := ForRun(statepaths.NewLayout(dataRoot, ""), ref, pr, "example-profile", "example-bot", runID)
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}
	return paths
}

// The budget that broke Windows reviews: every checkout's .git path must fit
// Git for Windows' GIT_DIR limit for the longest names GitHub allows, the
// longest reviewer IDs, and the coverage-repair suffix, with a data root longer
// than the Windows default.
func TestWorkspaceGitDirsFitWindowsLimitForWorstCaseNames(t *testing.T) {
	// Longer than C:\Users\<20-char user>\AppData\Local\cr\data.
	dataRoot := filepath.FromSlash("C:/Users/" + strings.Repeat("u", 40) + "/AppData/Local/cr/data")
	owner := strings.Repeat("o", 39) // GitHub's owner maximum.
	repo := strings.Repeat("r", 100) // GitHub's repository maximum.
	paths := workspaceTestRun(t, dataRoot, owner, repo, 999999, "123e4567-e89b-12d3-a456-426614174000")

	if paths.WorkspacesRoot != filepath.Join(dataRoot, statepaths.WorkspacesDirName) {
		t.Fatalf("WorkspacesRoot = %q, want %q", paths.WorkspacesRoot, filepath.Join(dataRoot, statepaths.WorkspacesDirName))
	}
	longAgent := "architecture:" + strings.Repeat("a", 120)
	repos := map[string]string{"workbench": paths.WorkbenchRepoDir}
	for _, agentID := range []string{"architecture:solid-reviewer-agnostic", longAgent, longAgent + agents.ReviewerCoverageRepairSuffix} {
		_, repoDir := paths.ReviewerWorkspace(agentID, "unused")
		repos[agentID] = repoDir
	}
	// The data root plus \ws\<12>\r\.git, whatever the PR or reviewer.
	wantLen := len(dataRoot) + len(filepath.Join("x", statepaths.WorkspacesDirName, strings.Repeat("k", workspaceKeyLength), workspaceRepoName, ".git")) - 1
	for name, repoDir := range repos {
		gitDir := filepath.Join(repoDir, ".git")
		if !strings.HasPrefix(gitDir, paths.WorkspacesRoot+string(filepath.Separator)) {
			t.Fatalf("%s .git path %q is outside the workspaces root %q", name, gitDir, paths.WorkspacesRoot)
		}
		if len(gitDir) != wantLen {
			t.Fatalf("%s .git path is %d characters, want data root plus fixed suffix = %d: %q", name, len(gitDir), wantLen, gitDir)
		}
		if len(gitDir) > windowsGitDirLimit {
			t.Fatalf("%s .git path is %d characters, over Git for Windows' %d: %q", name, len(gitDir), windowsGitDirLimit, gitDir)
		}
	}
}

func TestWorkspaceDirsAreStableAndDistinct(t *testing.T) {
	dataRoot := t.TempDir()
	run := workspaceTestRun(t, dataRoot, "example-org", "payments-service", 12, "run-a")
	other := workspaceTestRun(t, dataRoot, "example-org", "payments-service", 12, "run-b")

	if resumed := FromDir(run.Dir); resumed != run {
		t.Fatalf("FromDir(run.Dir) = %#v, want the ForRun paths %#v so a resumed run finds its workspaces", resumed, run)
	}
	alpha, _ := run.ReviewerWorkspace("harness:alpha", "unused")
	if again, _ := run.ReviewerWorkspace("harness:alpha", "unused"); again != alpha {
		t.Fatalf("reviewer workspace changed between calls: %q then %q", alpha, again)
	}
	seen := map[string]string{}
	for name, dir := range map[string]string{
		"workbench":           run.WorkbenchWorkspace(),
		"alpha":               alpha,
		"alpha repair":        first(run.ReviewerWorkspace("harness:alpha"+agents.ReviewerCoverageRepairSuffix, "unused")),
		"beta":                first(run.ReviewerWorkspace("harness:beta", "unused")),
		"other run workbench": other.WorkbenchWorkspace(),
		"other run alpha":     first(other.ReviewerWorkspace("harness:alpha", "unused")),
	} {
		if previous, ok := seen[dir]; ok {
			t.Fatalf("%s and %s share workspace %q", previous, name, dir)
		}
		seen[dir] = name
	}
}

func TestNonStandardRunDirKeepsWorkspacesInWorkbench(t *testing.T) {
	for _, dir := range []string{
		t.TempDir(),
		filepath.Join(t.TempDir(), "run-benchmark"),
		filepath.Join(t.TempDir(), "elsewhere", "a", "b", "c", "d", "run-x"),
	} {
		paths := FromDir(dir)
		if paths.WorkspacesRoot != "" || paths.WorkbenchWorkspace() != "" {
			t.Fatalf("FromDir(%q) workspaces root = %q, want none outside the data-root layout", dir, paths.WorkspacesRoot)
		}
		if want := filepath.Join(dir, "workbench", "repo"); paths.WorkbenchRepoDir != want {
			t.Fatalf("WorkbenchRepoDir = %q, want %q", paths.WorkbenchRepoDir, want)
		}
		root, repo := paths.ReviewerWorkspace("go:tests", "go-tests-1234")
		if want := filepath.Join(dir, "workbench", "reviewers", "go-tests-1234"); root != want || repo != filepath.Join(want, "repo") {
			t.Fatalf("ReviewerWorkspace = %q, %q; want %q and its repo child", root, repo, want)
		}
		if err := paths.WriteWorkspaceOwner(root, WorkspaceRoleReviewer, "go:tests"); err != nil {
			t.Fatalf("WriteWorkspaceOwner: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, WorkspaceOwnerFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owner record stat err = %v, want none written inside the run tree", err)
		}
	}
}

func TestWorkspaceOwnerRoundTrip(t *testing.T) {
	run := workspaceTestRun(t, t.TempDir(), "example-org", "web-client", 7, "run-owner")
	root, _ := run.ReviewerWorkspace("harness:alpha", "unused")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := run.WriteWorkspaceOwner(root, WorkspaceRoleReviewer, "harness:alpha"); err != nil {
		t.Fatalf("WriteWorkspaceOwner: %v", err)
	}
	owner, err := ReadWorkspaceOwner(root)
	if err != nil {
		t.Fatalf("ReadWorkspaceOwner: %v", err)
	}
	if owner.RunDir != filepath.Clean(run.Dir) || owner.Role != WorkspaceRoleReviewer || owner.AgentID != "harness:alpha" {
		t.Fatalf("owner = %#v, want run %q reviewer harness:alpha", owner, run.Dir)
	}
	if err := os.WriteFile(filepath.Join(root, WorkspaceOwnerFile), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadWorkspaceOwner(root); err == nil {
		t.Fatal("ReadWorkspaceOwner accepted an owner record without a run directory")
	}
}

func first(root, _ string) string { return root }
