package hooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestHookHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_HOOK_HELPER") != "1" {
		return
	}
	body, _ := io.ReadAll(os.Stdin)
	if delay, _ := time.ParseDuration(os.Getenv("HOOK_HELPER_SLEEP")); delay > 0 {
		time.Sleep(delay)
	}
	if size, _ := strconv.Atoi(os.Getenv("HOOK_HELPER_OUTPUT")); size > 0 {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), size))
	}
	if path := os.Getenv("HOOK_HELPER_CAPTURE"); path != "" {
		// #nosec G304,G703 -- the parent test supplies a t.TempDir capture path.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.Write(bytes.TrimSpace(body))
		_, _ = file.WriteString("\n")
		_ = file.Close()
	}
	if path := os.Getenv("HOOK_HELPER_ENV"); path != "" {
		// #nosec G304,G703 -- the parent test supplies a t.TempDir capture path.
		_ = os.WriteFile(path, []byte(strings.Join([]string{
			os.Getenv("CR_EVENT"), os.Getenv("CR_PR_URL"), os.Getenv("CR_RUN_ID"),
			os.Getenv("CR_OUTCOME"), os.Getenv("CR_PROFILE"), os.Getenv("CR_PASS_NUMBER"),
			os.Getenv("CR_ARTIFACT_DIR"), os.Getenv("CR_DRY_RUN"),
		}, "\n")), 0o600)
	}
	if code, _ := strconv.Atoi(os.Getenv("HOOK_HELPER_EXIT")); code != 0 {
		os.Exit(code)
	}
	os.Exit(0)
}

func TestDispatcherWritesPayloadAndCommonEnvironment(t *testing.T) {
	capture := filepathForTest(t, "payload")
	envCapture := filepathForTest(t, "env")
	t.Setenv("GO_WANT_HOOK_HELPER", "1")
	t.Setenv("HOOK_HELPER_CAPTURE", capture)
	t.Setenv("HOOK_HELPER_ENV", envCapture)
	dispatcher := New([]config.Hook{{Event: "run.completed", Argv: helperArgv(), Timeout: "1s"}}, io.Discard)
	want := Payload{
		Event: "run.completed", PRURL: "https://github.com/acme/repo/pull/7", RunID: "run-7",
		Outcome: "approved", Profile: "work", PassNumber: 2, ArtifactDir: "/tmp/run-7", DryRun: false,
	}
	dispatcher.Dispatch(want)
	dispatcher.Drain()

	lines := readPayloads(t, capture)
	if len(lines) != 1 || !reflect.DeepEqual(lines[0], want) {
		t.Fatalf("payloads = %#v, want %#v", lines, want)
	}
	// #nosec G304 -- envCapture is under t.TempDir.
	envBody, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	wantEnv := "run.completed\nhttps://github.com/acme/repo/pull/7\nrun-7\napproved\nwork\n2\n/tmp/run-7\nfalse"
	if string(envBody) != wantEnv {
		t.Fatalf("env = %q, want %q", envBody, wantEnv)
	}
}

func TestDispatcherIsNonBlockingAndDrainWaits(t *testing.T) {
	capture := filepathForTest(t, "slow")
	t.Setenv("GO_WANT_HOOK_HELPER", "1")
	t.Setenv("HOOK_HELPER_CAPTURE", capture)
	t.Setenv("HOOK_HELPER_SLEEP", "150ms")
	dispatcher := New([]config.Hook{{Event: "reviewer.completed", Argv: helperArgv(), Timeout: "1s"}}, io.Discard)
	started := time.Now()
	dispatcher.Dispatch(Payload{Event: "reviewer.completed"})
	if elapsed := time.Since(started); elapsed > 75*time.Millisecond {
		t.Fatalf("Dispatch blocked for %s", elapsed)
	}
	dispatcher.Drain()
	if elapsed := time.Since(started); elapsed < 125*time.Millisecond {
		t.Fatalf("Drain returned before slow hook completed after %s", elapsed)
	}
	if len(readPayloads(t, capture)) != 1 {
		t.Fatal("slow hook did not finish before Drain")
	}
}

func TestDispatcherDryRunFailureAndTimeoutIsolation(t *testing.T) {
	capture := filepathForTest(t, "dry")
	t.Setenv("GO_WANT_HOOK_HELPER", "1")
	t.Setenv("HOOK_HELPER_CAPTURE", capture)
	var warnings bytes.Buffer
	dispatcher := New([]config.Hook{
		{Event: "run.started", Argv: helperArgv(), Timeout: "1s"},
		{Event: "run.started", Argv: helperArgv(), Timeout: "1s", OnDryRun: true},
		{Event: "run.failed", Argv: []string{"definitely-not-a-real-cr-hook-command"}, Timeout: "1s"},
	}, &warnings)
	dispatcher.Dispatch(Payload{Event: "run.started", DryRun: true})
	dispatcher.Dispatch(Payload{Event: "run.failed"})
	dispatcher.Drain()
	if got := len(readPayloads(t, capture)); got != 1 {
		t.Fatalf("dry-run hook count = %d, want 1", got)
	}
	if !strings.Contains(warnings.String(), "definitely-not-a-real-cr-hook-command") {
		t.Fatalf("missing-command warning = %q", warnings.String())
	}

	warnings.Reset()
	t.Setenv("HOOK_HELPER_SLEEP", "500ms")
	t.Setenv("HOOK_HELPER_OUTPUT", "9000")
	timed := New([]config.Hook{{Event: "run.failed", Argv: helperArgv(), Timeout: "50ms"}}, &warnings)
	started := time.Now()
	timed.Dispatch(Payload{Event: "run.failed"})
	timed.Drain()
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("timed-out drain took %s", elapsed)
	}
	if !strings.Contains(warnings.String(), "timed out after 50ms") {
		t.Fatalf("timeout warning = %q", warnings.String())
	}

	warnings.Reset()
	t.Setenv("HOOK_HELPER_SLEEP", "")
	t.Setenv("HOOK_HELPER_EXIT", "7")
	failed := New([]config.Hook{{Event: "run.failed", Argv: helperArgv(), Timeout: "1s"}}, &warnings)
	failed.Dispatch(Payload{Event: "run.failed"})
	failed.Drain()
	if !strings.Contains(warnings.String(), "exit status 7") || len(warnings.String()) > maxOutput+256 {
		t.Fatalf("non-zero/capped warning length=%d body=%q", len(warnings.String()), warnings.String())
	}
}

func helperArgv() []string {
	return []string{os.Args[0], "-test.run=^TestHookHelperProcess$"}
}

func filepathForTest(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + string(os.PathSeparator) + name
}

func readPayloads(t *testing.T, path string) []Payload {
	t.Helper()
	// #nosec G304 -- callers pass paths under t.TempDir.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open payloads: %v", err)
	}
	defer func() { _ = file.Close() }()
	var payloads []Payload
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var payload Payload
		if err := json.Unmarshal(scanner.Bytes(), &payload); err != nil {
			t.Fatalf("decode payload %q: %v", scanner.Text(), err)
		}
		payloads = append(payloads, payload)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan payloads: %v", err)
	}
	return payloads
}

func ExamplePayload() {
	payload, _ := json.Marshal(Payload{Event: "run.started", PRURL: "https://github.com/acme/repo/pull/7", Profile: "work"})
	fmt.Println(string(payload))
	// Output: {"event":"run.started","pr_url":"https://github.com/acme/repo/pull/7","run_id":"","profile":"work","pass_number":0,"artifact_dir":"","dry_run":false}
}
