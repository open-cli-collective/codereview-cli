package workbench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func withGOOS(t *testing.T, value string) {
	t.Helper()
	previous := goos
	goos = value
	t.Cleanup(func() { goos = previous })
}

func TestCheckGitDirLengthEnforcesWindowsLimitOnly(t *testing.T) {
	// A repo dir whose .git path lands exactly on the limit, and one past it.
	atLimit := filepath.Join(string(filepath.Separator)+strings.Repeat("d", windowsGitDirLimit-len(filepath.Join("x", ".git"))), "")
	if got := len(filepath.Join(atLimit, ".git")); got != windowsGitDirLimit {
		t.Fatalf("fixture .git length = %d, want %d", got, windowsGitDirLimit)
	}
	overLimit := atLimit + "d"

	withGOOS(t, "windows")
	if err := checkGitDirLength(atLimit); err != nil {
		t.Fatalf("checkGitDirLength at the limit: %v", err)
	}
	err := checkGitDirLength(overLimit)
	if err == nil {
		t.Fatal("checkGitDirLength accepted a .git path over the Windows limit")
	}
	for _, want := range []string{"221 characters", "at most 220", `%LOCALAPPDATA%\cr\data`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	for _, platform := range []string{"linux", "darwin"} {
		withGOOS(t, platform)
		if err := checkGitDirLength(overLimit + strings.Repeat("d", 1000)); err != nil {
			t.Fatalf("checkGitDirLength on %s: %v, want no limit", platform, err)
		}
	}
}

// A reviewer workspace too long for Git for Windows fails before any git
// command runs, with the fix in the error, instead of mid-clone with
// "'$GIT_DIR' too big".
func TestReviewerWorkspaceRejectsOverlongGitDirBeforeCloning(t *testing.T) {
	withGOOS(t, "windows")
	dataRoot := filepath.Join(string(filepath.Separator)+strings.Repeat("d", 200), "cr", "data")
	runDir := filepath.Join(dataRoot, "runs", "github.com_o_r_1", "h", "b", "s", "run-x")
	artifacts := runartifact.FromDir(runDir)
	if artifacts.WorkspacesRoot == "" {
		t.Fatal("fixture run dir is not in the data-root layout")
	}
	var calls [][]string
	deps := Deps{GitCommand: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}}

	_, _, err := prepareReviewerWorkspace(context.Background(), deps, artifacts, "abc", "harness:alpha", nil, defaultReviewerWorkspaceToolOutputBytes)
	if err == nil || !strings.Contains(err.Error(), "Git for Windows accepts at most 220") {
		t.Fatalf("prepareReviewerWorkspace error = %v, want the Windows GIT_DIR limit", err)
	}
	if len(calls) != 0 {
		t.Fatalf("git calls = %q, want none before the length check", calls)
	}
}
