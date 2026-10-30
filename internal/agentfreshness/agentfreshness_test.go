package agentfreshness

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gittest"
)

func TestCheckReportsCheckoutBehindItsUpstream(t *testing.T) {
	upstream, clone := catalogClone(t)
	pushCommits(t, upstream, 2)

	notices := Check(context.Background(), []string{agentsDir(t, clone)}, testGit)

	if len(notices) != 1 {
		t.Fatalf("notices = %+v, want one", notices)
	}
	got := notices[0]
	if got.Kind != KindBehind || got.Behind != 2 || got.Upstream != "origin/main" || got.FetchError != "" {
		t.Fatalf("notice = %+v, want behind origin/main by 2 after a successful fetch", got)
	}
	line := got.String()
	if !strings.Contains(line, "2 commits behind origin/main") || !strings.Contains(line, "merge --ff-only origin/main") {
		t.Fatalf("line = %q, want behind count and fast-forward command", line)
	}
}

func TestCheckIsQuietForCurrentCheckout(t *testing.T) {
	_, clone := catalogClone(t)

	if notices := Check(context.Background(), []string{agentsDir(t, clone)}, testGit); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none", notices)
	}
}

func TestCheckFallsBackToLastFetchWhenUpstreamIsUnreachable(t *testing.T) {
	upstream, clone := catalogClone(t)
	pushCommits(t, upstream, 1)
	git(t, clone, "fetch", "--quiet")
	git(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	notices := Check(context.Background(), []string{agentsDir(t, clone)}, testGit)

	if len(notices) != 1 || notices[0].Kind != KindBehind || notices[0].Behind != 1 || notices[0].FetchError == "" {
		t.Fatalf("notices = %+v, want behind by 1 with a fetch error", notices)
	}
	if line := notices[0].String(); strings.Contains(line, "\n") || !strings.Contains(line, "1 commit behind") {
		t.Fatalf("line = %q, want a single line naming 1 commit", line)
	}
}

func TestCheckIsQuietWhenUpstreamIsUnreachableAndLastFetchIsCurrent(t *testing.T) {
	_, clone := catalogClone(t)
	git(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	if notices := Check(context.Background(), []string{agentsDir(t, clone)}, testGit); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none", notices)
	}
}

func TestCheckNotesUnversionedAndUntrackedSources(t *testing.T) {
	plain := t.TempDir()
	local := t.TempDir()
	git(t, local, "init", "--quiet", "--initial-branch=main")
	commitFile(t, local, "agents/a.md")

	notices := Check(context.Background(), []string{plain, filepath.Join(local, "agents")}, testGit)

	if len(notices) != 2 || notices[0].Kind != KindUnversioned || notices[1].Kind != KindNoUpstream {
		t.Fatalf("notices = %+v, want unversioned then no-upstream", notices)
	}
	if line := notices[0].String(); !strings.Contains(line, "not in a Git repository") || !strings.HasPrefix(line, "note: ") {
		t.Fatalf("line = %q, want an unversioned note", line)
	}
	if line := notices[1].String(); !strings.Contains(line, "tracks no upstream") || !strings.HasPrefix(line, "note: ") {
		t.Fatalf("line = %q, want a no-upstream note", line)
	}
}

func TestCheckSkipsMissingSourcesAndChecksEachCheckoutOnce(t *testing.T) {
	upstream, clone := catalogClone(t)
	pushCommits(t, upstream, 1)
	dir := agentsDir(t, clone)
	other := filepath.Join(clone, "more-agents")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetches int
	run := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if args[0] == "fetch" {
			fetches++
		}
		return testGit(ctx, dir, args...)
	}

	notices := Check(context.Background(), []string{filepath.Join(t.TempDir(), "missing"), dir, dir, other}, run)

	if len(notices) != 1 || notices[0].Kind != KindBehind || fetches != 1 {
		t.Fatalf("notices = %+v, fetches = %d; want one behind notice from one fetch", notices, fetches)
	}
}

func TestWarnSkipsCheckWithoutWriter(t *testing.T) {
	Warn(context.Background(), []string{t.TempDir()}, nil)

	var out bytes.Buffer
	Warn(context.Background(), []string{t.TempDir()}, &out)
	if !strings.Contains(out.String(), "not in a Git repository") {
		t.Fatalf("output = %q, want unversioned note", out.String())
	}
}

func testGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- test git fixture.
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(), "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

// catalogClone returns a bare upstream with one commit and a clone tracking it.
func catalogClone(t *testing.T) (upstream, clone string) {
	t.Helper()
	upstream = filepath.Join(t.TempDir(), "catalog.git")
	git(t, filepath.Dir(upstream), "init", "--quiet", "--bare", "--initial-branch=main", upstream)
	seed := t.TempDir()
	git(t, seed, "clone", "--quiet", upstream, ".")
	commitFile(t, seed, "agents/a.md")
	git(t, seed, "push", "--quiet", "origin", "HEAD:main")
	clone = t.TempDir()
	git(t, clone, "clone", "--quiet", upstream, ".")
	return upstream, clone
}

func pushCommits(t *testing.T, upstream string, n int) {
	t.Helper()
	work := t.TempDir()
	git(t, work, "clone", "--quiet", upstream, ".")
	for i := range n {
		commitFile(t, work, filepath.Join("agents", "new-"+string(rune('a'+i))+".md"))
	}
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
}

func commitFile(t *testing.T, repo, rel string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", rel)
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", rel)
}

func agentsDir(t *testing.T, clone string) string {
	t.Helper()
	return filepath.Join(clone, "agents")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- test git fixture.
	cmd.Dir = dir
	cmd.Env = gittest.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
