package pireviewtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

func TestRunDecodesOneStrictRequest(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	configJSON := `{"repo_dir":` + quoteJSON(t, repo) + `,"diff_path":` + quoteJSON(t, diff) + `,"max_output_bytes":4096,"timeout_ms":1000}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--config", configPath}, strings.NewReader(`{"tool":"cr_read","path":"assigned.go"}`), &stdout, &stderr)
	if code != 0 || stdout.String() != "package root\n" || stderr.Len() != 0 {
		t.Fatalf("Run = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"--config", configPath}, strings.NewReader(`{"tool":"cr_read","path":"assigned.go","command":"sh"}`), &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "unknown field") {
		t.Fatalf("strict Run = %d, stderr %q, want unknown-field failure", code, stderr.String())
	}
}

func TestRunReadAndDiffRangesReachContentBeyondOutputCap(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	largeFile := strings.Repeat("a", 40*1024) + "READ_TARGET" + strings.Repeat("b", 40*1024)
	if err := os.WriteFile(filepath.Join(repo, "large.txt"), []byte(largeFile), 0o600); err != nil {
		t.Fatalf("WriteFile(large): %v", err)
	}
	largeDiff := strings.Repeat("x", 40*1024) + "DIFF_TARGET" + strings.Repeat("y", 40*1024)
	if err := os.WriteFile(diff, []byte(largeDiff), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	configPath := writeToolConfig(t, Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 128, TimeoutMS: 1000})

	for _, tt := range []struct {
		name    string
		request string
		want    string
	}{
		{name: "read", request: `{"tool":"cr_read","path":"large.txt","offset":40950,"limit":40}`, want: "READ_TARGET"},
		{name: "diff", request: `{"tool":"cr_diff","offset":40950,"limit":40}`, want: "DIFF_TARGET"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), []string{"--config", configPath}, strings.NewReader(tt.request), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("Run = %d, stderr %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.want) || !strings.Contains(stdout.String(), "offset=40950") || !strings.Contains(stdout.String(), "next_offset=40990") {
				t.Fatalf("range output = %q, want target and deterministic next offset", stdout.String())
			}
			if len(stdout.String()) > 128 {
				t.Fatalf("range output = %d bytes, want aggregate tool cap 128", len(stdout.String()))
			}
		})
	}
}

func TestExecuteReadRangesPreserveUTF8AcrossContinuationBoundaries(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	want := strings.Repeat("🙂", 100)
	if err := os.WriteFile(filepath.Join(repo, "unicode.txt"), []byte(want), 0o600); err != nil {
		t.Fatalf("WriteFile(unicode): %v", err)
	}
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 128}
	var reconstructed []byte
	offset := int64(0)
	for page := 0; page < 20; page++ {
		got, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "unicode.txt", Offset: offset})
		if err != nil {
			t.Fatalf("Execute(page %d): %v", page, err)
		}
		headerEnd := strings.IndexByte(got, '\n')
		if headerEnd < 0 {
			t.Fatalf("page %d = %q, want range header", page, got)
		}
		var start, end, total, next int64
		if _, err := fmt.Sscanf(got[:headerEnd], "[cr-range offset=%d end=%d total=%d next_offset=%d]", &start, &end, &total, &next); err != nil {
			t.Fatalf("parse page %d header %q: %v", page, got[:headerEnd], err)
		}
		payload := []byte(got[headerEnd+1:])
		if !utf8.Valid(payload) {
			t.Fatalf("page %d payload splits a UTF-8 sequence: %x", page, payload)
		}
		if start != offset || end != start+int64(len(payload)) || total != int64(len([]byte(want))) {
			t.Fatalf("page %d metadata = %d/%d/%d for %d payload bytes", page, start, end, total, len(payload))
		}
		reconstructed = append(reconstructed, payload...)
		if next < 0 {
			break
		}
		offset = next
	}
	if string(reconstructed) != want {
		t.Fatalf("reconstructed %d bytes, want all %d UTF-8 bytes", len(reconstructed), len([]byte(want)))
	}
}

func TestExecuteReadSearchListAndFixedDiff(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	config := Config{RepoDir: repo, DiffPath: diff, AllowedFiles: []string{"assigned.go"}, MaxOutputBytes: 4096}

	read, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "nested/context.go"})
	if err != nil || !strings.Contains(read, "package nested") {
		t.Fatalf("read = %q, err = %v", read, err)
	}
	// AllowedFiles is assignment metadata, not a filesystem sensitivity boundary.
	if !strings.Contains(read, "context") {
		t.Fatalf("read outside allowed_files = %q, want repository context", read)
	}
	search, err := Execute(context.Background(), config, Request{Tool: ToolSearch, Query: "needle"})
	if err != nil || !strings.Contains(search, "nested/context.go:3") {
		t.Fatalf("search = %q, err = %v", search, err)
	}
	list, err := Execute(context.Background(), config, Request{Tool: ToolList, Path: "nested"})
	if err != nil || !strings.Contains(list, "nested/context.go") {
		t.Fatalf("list = %q, err = %v", list, err)
	}
	fixedDiff, err := Execute(context.Background(), config, Request{Tool: ToolDiff, Path: "../../etc/passwd"})
	if err != nil || fixedDiff != "fixed pinned diff\n" {
		t.Fatalf("diff = %q, err = %v", fixedDiff, err)
	}
}

func TestExecuteRejectsPathEscapesLinksAndUnknownTools(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}
	link := filepath.Join(repo, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}

	for _, request := range []Request{
		{Tool: ToolRead, Path: outside},
		{Tool: ToolRead, Path: "../outside.txt"},
		{Tool: ToolRead, Path: "nested/../../outside.txt"},
		{Tool: ToolRead, Path: "outside-link"},
		{Tool: ToolList, Path: "outside-link"},
		{Tool: "bash", Path: "nested/context.go"},
	} {
		if _, err := Execute(context.Background(), config, request); !errors.Is(err, ErrDenied) {
			t.Errorf("Execute(%+v) error = %v, want ErrDenied", request, err)
		}
	}
}

func TestExecuteRejectsSymlinkComponentsAndDiffLinks(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(repo, "linked-dir")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}
	if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "linked-dir/secret.txt"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("symlink component error = %v, want ErrDenied", err)
	}
	diffLink := filepath.Join(t.TempDir(), "diff-link")
	if err := os.Symlink(diff, diffLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	config.DiffPath = diffLink
	if _, err := Execute(context.Background(), config, Request{Tool: ToolDiff}); !errors.Is(err, ErrDenied) {
		t.Fatalf("symlink diff error = %v, want ErrDenied", err)
	}
}

func TestExecuteListAndSearchExcludeCloneMetadata(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	source := filepath.Join(t.TempDir(), "source")
	clone := filepath.Join(t.TempDir(), "clone")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("MkdirAll(source): %v", err)
	}
	runGitForToolTest(t, gitPath, source, "init", "-b", "main")
	runGitForToolTest(t, gitPath, source, "config", "user.name", "Tool Test")
	runGitForToolTest(t, gitPath, source, "config", "user.email", "tool@example.com")
	if err := os.WriteFile(filepath.Join(source, "visible.txt"), []byte("visible needle\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(visible): %v", err)
	}
	runGitForToolTest(t, gitPath, source, "add", "visible.txt")
	runGitForToolTest(t, gitPath, source, "commit", "-m", "fixture")
	runGitForToolTest(t, gitPath, "", "clone", source, clone)
	if err := os.WriteFile(filepath.Join(clone, ".git", "metadata-secret"), []byte("metadata needle\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(metadata): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(clone, "submodule"), 0o700); err != nil {
		t.Fatalf("MkdirAll(submodule): %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "submodule", ".git"), []byte("gitdir: ../.git/modules/submodule\nmetadata needle\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(submodule metadata): %v", err)
	}
	diff := filepath.Join(t.TempDir(), "diff.patch")
	if err := os.WriteFile(diff, []byte("diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	config := Config{RepoDir: clone, DiffPath: diff, MaxOutputBytes: 4096}

	listed, err := Execute(context.Background(), config, Request{Tool: ToolList})
	if err != nil {
		t.Fatalf("Execute(list): %v", err)
	}
	if strings.Contains(listed, ".git") || !strings.Contains(listed, "visible.txt") {
		t.Fatalf("list = %q, want worktree files without VCS metadata", listed)
	}
	searched, err := Execute(context.Background(), config, Request{Tool: ToolSearch, Query: "needle"})
	if err != nil {
		t.Fatalf("Execute(search): %v", err)
	}
	if strings.Contains(searched, ".git") || strings.Contains(searched, "metadata needle") || !strings.Contains(searched, "visible needle") {
		t.Fatalf("search = %q, want worktree match without VCS metadata", searched)
	}
}

func TestExecuteSearchSkipsOversizedAndBinaryLinesWithoutAbortingRepository(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "a-oversized.txt"), []byte(strings.Repeat("x", 2*1024*1024)), 0o600); err != nil {
		t.Fatalf("WriteFile(oversized): %v", err)
	}
	binary := append([]byte{0, 1, 2, 3}, []byte("needle in binary")...)
	if err := os.WriteFile(filepath.Join(repo, "b-binary.bin"), binary, 0o600); err != nil {
		t.Fatalf("WriteFile(binary): %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "z-match.txt"), []byte("final needle\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(match): %v", err)
	}

	got, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolSearch, Query: "needle"})
	if err != nil {
		t.Fatalf("Execute(search): %v", err)
	}
	if !strings.Contains(got, "z-match.txt:1:final needle") {
		t.Fatalf("search = %q, want later text match after oversized file", got)
	}
	if strings.Contains(got, "b-binary.bin") {
		t.Fatalf("search = %q, want binary file omitted", got)
	}
}

func TestSearchFileRetainsOnlyBoundedMatchesBeforeEOF(t *testing.T) {
	const (
		maxBytes   = 256
		matchCount = 10_000
	)
	beforeEOF := errors.New("reader stopped before EOF")
	reader := io.MultiReader(
		strings.NewReader(strings.Repeat("needle repeated content\n", matchCount)),
		iotest.ErrReader(beforeEOF),
	)
	collector := newSearchMatchCollector(maxBytes)
	binary, err := searchFile(context.Background(), reader, "needle", collector)
	if !errors.Is(err, beforeEOF) {
		t.Fatalf("searchFile error = %v, want pre-EOF sentinel", err)
	}
	if binary {
		t.Fatal("searchFile reported text fixture as binary")
	}
	if collector.retainedBytes > maxBytes {
		t.Fatalf("retained match storage = %d bytes across %d matches, want <= %d", collector.retainedBytes, len(collector.matches), maxBytes)
	}
	if len(collector.matches) >= matchCount {
		t.Fatalf("retained %d matches, want collector to stop retaining before EOF", len(collector.matches))
	}
}

func TestExecuteBoundsOutputAndHonorsCancellation(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "nested", "context.go"), []byte(strings.Repeat("context ", 80)), 0o600); err != nil {
		t.Fatalf("WriteFile(context): %v", err)
	}
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 128}
	got, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "nested/context.go"})
	if err != nil {
		t.Fatalf("Execute(read): %v", err)
	}
	if len(got) > config.MaxOutputBytes || !strings.Contains(got, "next_offset=") {
		t.Fatalf("bounded output = %q (%d bytes), want <= %d with range metadata", got, len(got), config.MaxOutputBytes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Execute(ctx, Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolList}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Execute error = %v, want context.Canceled", err)
	}
}

func TestExecuteFixedDiffDoesNotInvokeGitHelpersOrConfig(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	marker := filepath.Join(t.TempDir(), "external-diff-ran")
	t.Setenv("GIT_EXTERNAL_DIFF", marker)
	t.Setenv("GIT_CONFIG_GLOBAL", marker)
	got, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolDiff})
	if err != nil || got != "fixed pinned diff\n" {
		t.Fatalf("Execute(diff) = %q, %v", got, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git helper marker exists or stat failed: %v", err)
	}
}

func TestResolveRepoPathRejectsCrossVolumeWhenRepresentable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cross-volume paths are represented only on Windows")
	}
	if _, err := resolveRepoPath(`C:\repo`, `D:\escape`); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-volume error = %v, want ErrDenied", err)
	}
}

func reviewerToolFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatalf("MkdirAll(repo): %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "assigned.go"), []byte("package root\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(assigned): %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "nested", "context.go"), []byte("package nested\n\n// needle context\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(context): %v", err)
	}
	diff := filepath.Join(t.TempDir(), "diff.patch")
	if err := os.WriteFile(diff, []byte("fixed pinned diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	return repo, diff
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}

func writeToolConfig(t *testing.T, config Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal(config): %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	return path
}

func runGitForToolTest(t *testing.T, gitPath, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitPath, args...) // #nosec G204 -- test launches discovered Git with fixed/test-owned arguments.
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
