package reporoot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gittest"
)

func TestResolveReturnsCanonicalGitRoot(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll repo root: %v", err)
	}
	initGitRepoForReporootTest(t, repoRoot)

	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repoRoot, link); err != nil {
		t.Fatalf("Symlink repo root: %v", err)
	}
	want, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks repo root: %v", err)
	}
	want, err = filepath.Abs(want)
	if err != nil {
		t.Fatalf("Abs repo root: %v", err)
	}

	got, err := Resolve(context.Background(), link, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("Resolve root = %q, want %q", got, want)
	}
}

func TestResolveReturnsUnavailableOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	got, err := Resolve(context.Background(), dir, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Resolve error = %v, want ErrUnavailable", err)
	}
	if got != "" {
		t.Fatalf("Resolve root = %q, want empty", got)
	}
}

func initGitRepoForReporootTest(t *testing.T, dir string) {
	t.Helper()
	initCmd := exec.Command("git", "init", dir) // #nosec G204 -- tests invoke git with fixed arguments.
	initCmd.Env = gittest.Env()
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}
