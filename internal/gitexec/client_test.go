//go:build !windows

package gitexec

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

type rotatingTokens struct {
	values []string
	calls  int
	err    error
}

func (s *rotatingTokens) AccessToken(context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	value := s.values[s.calls]
	s.calls++
	return value, nil
}

func TestClientLocalCommandsDoNotReadToken(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tokens := &rotatingTokens{err: errors.New("refresh unavailable")}
	client, err := New("github.example.com", tokens)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, args := range [][]string{{"status", "--porcelain"}, {"clone", dir, filepath.Join(dir, "clone")}} {
		if _, err := client.Run(context.Background(), "", args...); err != nil {
			t.Fatalf("local Run %v: %v", args, err)
		}
	}
	if tokens.calls != 0 {
		t.Fatalf("token calls = %d, want none for local Git", tokens.calls)
	}
}

func TestClientPreservesFailingLocalCommandDiagnostic(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'local failure' >&2\nexit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client, err := New("github.example.com", &rotatingTokens{err: errors.New("refresh unavailable")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Run(context.Background(), dir, "status")
	if err == nil || !strings.Contains(err.Error(), "local failure") {
		t.Fatalf("Run error = %v, want intact local diagnostic", err)
	}
}

func TestClientUsesFreshHostScopedTokenWithoutLeakingArguments(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GITEXEC_RECORD.args\"\nenv > \"$GITEXEC_RECORD.env\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITEXEC_RECORD", record)

	tokens := &rotatingTokens{values: []string{"first-secret", "second-secret"}}
	client, err := New("github.example.com", tokens)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 2 {
		if _, err := client.Run(context.Background(), "", "fetch", "https://github.example.com/acme/repo.git"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	args, err := os.ReadFile(record + ".args") // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "secret") {
		t.Fatalf("arguments leaked token: %s", args)
	}
	env, err := os.ReadFile(record + ".env") // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatal(err)
	}
	got := string(env)
	if !strings.Contains(got, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(got, "GIT_CONFIG_KEY_0=http.https://github.example.com/.extraHeader") {
		t.Fatalf("git environment missing safety config: %s", got)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:second-secret"))
	if !strings.Contains(got, wantAuth) {
		t.Fatalf("git environment did not use refreshed token: %s", got)
	}
}

func TestClientRedactsTokenFromGitError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nenv | grep GIT_CONFIG_VALUE_0 >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const token = "distinct-secret-token"
	client, err := New("github.example.com", TokenSourceFunc(func(context.Context) (string, error) { return token, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Run(context.Background(), "", "fetch", "https://github.example.com/acme/repo.git")
	if err == nil {
		t.Fatal("Run succeeded, want git failure")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))) {
		t.Fatalf("Run error leaked token: %v", err)
	}
}

// fakeGit installs a git that prints the output of each attempt in turn and
// fails, then succeeds once they run out. It counts its invocations in a file.
func fakeGit(t *testing.T, outputs ...string) (count func() int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> '" + counter + "'\nn=$(wc -l < '" + counter + "' | tr -d ' ')\ncase \"$n\" in\n"
	for i, output := range outputs {
		script += strconv.Itoa(i+1) + ") echo '" + output + "' >&2; exit 128 ;;\n"
	}
	script += "esac\nexit 0\n"
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, err := os.ReadFile(counter) // #nosec G304 -- test-owned temp file.
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "x")
	}
}

func newRetryTestClient(t *testing.T, delays *[]time.Duration, events *[]RetryEvent) *Client {
	t.Helper()
	client, err := New("github.com", TokenSourceFunc(func(context.Context) (string, error) { return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	client.sleep = func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
	client.OnRetry = func(event RetryEvent) { *events = append(*events, event) }
	return client
}

const testFetchURL = "https://github.com/o/r.git"

func TestFetchRetriesTransientServerError(t *testing.T) {
	count := fakeGit(t, "fatal: unable to access '"+testFetchURL+"/': The requested URL returned error: 502")
	var delays []time.Duration
	var events []RetryEvent

	if _, err := newRetryTestClient(t, &delays, &events).Run(context.Background(), "", "fetch", "--no-tags", testFetchURL, "abc"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count() != 2 || len(delays) != 1 || len(events) != 1 || events[0].Attempt != 1 {
		t.Fatalf("runs = %d delays = %v events = %#v, want one retry", count(), delays, events)
	}
	if delays[0] < time.Second || delays[0] > 2*time.Second {
		t.Fatalf("delay = %s, want within the first backoff", delays[0])
	}
}

func TestFetchGivesUpAsRetryableUpstreamFailure(t *testing.T) {
	const failure = "error: RPC failed; curl 56 Recv failure: Connection reset by peer"
	count := fakeGit(t, failure, failure, failure, failure, failure)
	var delays []time.Duration
	var events []RetryEvent

	_, err := newRetryTestClient(t, &delays, &events).Run(context.Background(), "", "fetch", "--no-tags", testFetchURL, "abc")
	if !errors.Is(err, gitprovider.ErrRetryable) || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("error = %v, want retryable failure naming the attempts", err)
	}
	if count() != fetchMaxAttempts || len(delays) != fetchMaxAttempts-1 {
		t.Fatalf("runs = %d delays = %v", count(), delays)
	}
}

func TestPermanentFetchFailureAndOtherCommandsAreNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		output string
		args   []string
	}{
		{name: "unadvertised sha", output: "fatal: remote error: upload-pack: not our ref abc", args: []string{"fetch", "--no-tags", testFetchURL, "abc"}},
		{name: "missing repository", output: "fatal: unable to access: The requested URL returned error: 404", args: []string{"fetch", "--no-tags", testFetchURL, "abc"}},
		{name: "transient words only in a ref name", output: "fatal: couldn\x27t find remote ref refs/heads/gnutls_fix", args: []string{"fetch", "--no-tags", testFetchURL, "refs/heads/gnutls_fix"}},
		{name: "not a fetch", output: "fatal: The requested URL returned error: 502", args: []string{"rev-parse", "HEAD"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := fakeGit(t, tt.output, tt.output)
			var delays []time.Duration
			var events []RetryEvent

			_, err := newRetryTestClient(t, &delays, &events).Run(context.Background(), "", tt.args...)
			if err == nil || errors.Is(err, gitprovider.ErrRetryable) {
				t.Fatalf("error = %v, want a plain failure", err)
			}
			if count() != 1 || len(delays) != 0 {
				t.Fatalf("runs = %d delays = %v, want one run and no wait", count(), delays)
			}
		})
	}
}

func TestFetchRetryStopsWhenContextIsCanceled(t *testing.T) {
	const failure = "fatal: The requested URL returned error: 503"
	count := fakeGit(t, failure, failure)
	var delays []time.Duration
	var events []RetryEvent
	client := newRetryTestClient(t, &delays, &events)
	client.sleep = func(context.Context, time.Duration) error { return context.Canceled }

	if _, err := client.Run(context.Background(), "", "fetch", "--no-tags", testFetchURL, "abc"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if count() != 1 {
		t.Fatalf("runs = %d, want 1", count())
	}
}
