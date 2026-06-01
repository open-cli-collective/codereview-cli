package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubprocessClaudeLaunchSafety(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "record.json")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Command:           os.Args[0],
		CommandArgsPrefix: helperPrefix(),
		Env:               helperEnv("success", recordPath),
		Timeout:           5 * time.Second,
	})

	stream, err := adapter.Start(context.Background(), Request{
		Model:   "sonnet",
		Effort:  "high",
		Prompt:  "prompt",
		LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if stream.SessionID() != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", stream.SessionID())
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s", response.StructuredOutput)
	}
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 7 || response.Usage.CostUSD != nil {
		t.Fatalf("Usage = %#v, want parsed nullable usage", response.Usage)
	}
	record := readHelperRecord(t, recordPath)
	assertFlagValue(t, record.AdapterArgs, "--tools", "")
	assertFlagValue(t, record.AdapterArgs, "--mcp-config", "{}")
	assertFlagValue(t, record.AdapterArgs, "--output-format", "stream-json")
	for _, flag := range []string{"--bare", "--print", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
		if !containsFlag(record.AdapterArgs, flag) {
			t.Fatalf("args = %#v, want %s", record.AdapterArgs, flag)
		}
	}
	if record.Cwd == "" || record.Cwd == repoRootForTest(t) {
		t.Fatalf("cwd = %q, want scratch dir outside repo", record.Cwd)
	}
	if record.CwdEntries != 0 {
		t.Fatalf("cwd entries = %d, want empty scratch dir", record.CwdEntries)
	}
	if record.StdinBytes != 0 {
		t.Fatalf("stdin bytes = %d, want closed stdin", record.StdinBytes)
	}
	// #nosec G304 -- test reads the log path it created with t.TempDir.
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if !strings.Contains(string(logged), `"structured_output"`) {
		t.Fatalf("log = %q, want stdout JSONL", logged)
	}
}

func TestSubprocessCodexSafetyModes(t *testing.T) {
	t.Run("default refuses before launch", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewCodexCLIAdapter(SubprocessOptions{
			Command:           os.Args[0],
			CommandArgsPrefix: helperPrefix(),
			Env:               helperEnv("success", recordPath),
		})
		_, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if !errors.Is(err, ErrUnsafeSubprocessConfig) {
			t.Fatalf("Start error = %v, want ErrUnsafeSubprocessConfig", err)
		}
		if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("helper record exists after refused launch: %v", statErr)
		}
	})

	t.Run("best effort asserts conservative flags", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewCodexCLIAdapter(SubprocessOptions{
			Command:                os.Args[0],
			CommandArgsPrefix:      helperPrefix(),
			Env:                    helperEnv("success", recordPath),
			Timeout:                5 * time.Second,
			AllowBestEffortNoTools: true,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "gpt-5.5", Effort: "high", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := stream.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		record := readHelperRecord(t, recordPath)
		if len(record.AdapterArgs) == 0 || record.AdapterArgs[0] != "exec" {
			t.Fatalf("args = %#v, want codex exec", record.AdapterArgs)
		}
		for _, flag := range []string{"--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"} {
			if !containsFlag(record.AdapterArgs, flag) {
				t.Fatalf("args = %#v, want %s", record.AdapterArgs, flag)
			}
		}
		assertFlagValue(t, record.AdapterArgs, "--sandbox", "read-only")
		if cd := flagValue(record.AdapterArgs, "--cd"); !samePath(t, cd, record.Cwd) {
			t.Fatalf("flagValue(--cd) = %q, helper cwd = %q, want same path", cd, record.Cwd)
		}
		for _, flag := range []string{"--search", "--add-dir"} {
			if containsFlag(record.AdapterArgs, flag) {
				t.Fatalf("args = %#v, do not want %s", record.AdapterArgs, flag)
			}
		}
	})
}

func TestSubprocessToolUseAndProtocolFailures(t *testing.T) {
	t.Run("tool use kills and fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewClaudeCLIAdapter(SubprocessOptions{
			Command:           os.Args[0],
			CommandArgsPrefix: helperPrefix(),
			Env:               helperEnv("tool", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		start := time.Now()
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, ErrToolUse) {
			t.Fatalf("Wait error = %v, want ErrToolUse", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("tool-use process was not killed promptly")
		}
	})

	t.Run("timeout cancels process", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewClaudeCLIAdapter(SubprocessOptions{
			Command:           os.Args[0],
			CommandArgsPrefix: helperPrefix(),
			Env:               helperEnv("sleep", recordPath),
			Timeout:           20 * time.Millisecond,
		})
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait error = %v, want deadline exceeded", err)
		}
	})

	t.Run("malformed stdout JSONL fails immediately", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewClaudeCLIAdapter(SubprocessOptions{
			Command:           os.Args[0],
			CommandArgsPrefix: helperPrefix(),
			Env:               helperEnv("malformed", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		response, err := stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "malformed JSONL") {
			t.Fatalf("Wait error = %v, want malformed JSONL", err)
		}
		if len(response.StructuredOutput) != 0 {
			t.Fatalf("StructuredOutput = %s, want none after malformed event", response.StructuredOutput)
		}
	})
}

func TestSubprocessResumeUnsupported(t *testing.T) {
	adapter := NewClaudeCLIAdapter(SubprocessOptions{})
	if _, err := adapter.Resume(context.Background(), "session", Request{}); err == nil {
		t.Fatal("Resume error = nil, want unsupported")
	}
}

func TestSubprocessHelperProcess(_ *testing.T) {
	if os.Getenv("LLM_SUBPROCESS_HELPER") != "1" {
		return
	}
	recordPath := os.Getenv("LLM_HELPER_RECORD")
	cwd, _ := os.Getwd()
	entries, _ := os.ReadDir(cwd)
	stdin, _ := io.ReadAll(os.Stdin)
	record := helperRecord{
		AdapterArgs: adapterArgsFromHelper(),
		Cwd:         cwd,
		CwdEntries:  len(entries),
		StdinBytes:  len(stdin),
	}
	if recordPath != "" {
		data, _ := json.Marshal(record)
		// #nosec G703 -- helper writes only to a t.TempDir path supplied by the parent test.
		_ = os.WriteFile(recordPath, data, 0o600)
	}
	switch os.Getenv("LLM_HELPER_MODE") {
	case "success":
		fmt.Println(`{"type":"session.started","session_id":"session-1"}`)
		fmt.Println(`{"type":"unknown.event","ignored":true}`)
		fmt.Println(`{"type":"response","usage":{"tokens_in":7,"tokens_out":11,"cache_read":null,"cost_usd":null},"structured_output":{"ok":true}}`)
	case "tool":
		fmt.Println(`{"type":"session.started","session_id":"session-1"}`)
		fmt.Println(`{"type":"tool_use","name":"Read"}`)
		time.Sleep(10 * time.Second)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "malformed":
		fmt.Println(`{"type":`)
		fmt.Println(`{"type":"response","structured_output":{"ok":true}}`)
	default:
		fmt.Println(`{"type":"response","structured_output":{"ok":true}}`)
	}
	os.Exit(0)
}

type helperRecord struct {
	AdapterArgs []string `json:"adapter_args"`
	Cwd         string   `json:"cwd"`
	CwdEntries  int      `json:"cwd_entries"`
	StdinBytes  int      `json:"stdin_bytes"`
}

func helperPrefix() []string {
	return []string{"-test.run=TestSubprocessHelperProcess", "--"}
}

func helperEnv(mode string, recordPath string) []string {
	return []string{
		"LLM_SUBPROCESS_HELPER=1",
		"LLM_HELPER_MODE=" + mode,
		"LLM_HELPER_RECORD=" + recordPath,
	}
}

func adapterArgsFromHelper() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return append([]string(nil), os.Args[i+1:]...)
		}
	}
	return nil
}

func readHelperRecord(t *testing.T, path string) helperRecord {
	t.Helper()
	// #nosec G304 -- test reads helper output from a t.TempDir path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(record): %v", err)
	}
	var record helperRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("Unmarshal(record): %v", err)
	}
	return record
}

func assertFlagValue(t *testing.T, args []string, flag string, want string) {
	t.Helper()
	if got := flagValue(args, flag); got != want {
		t.Fatalf("flagValue(%s) = %q in %#v, want %q", flag, got, args, want)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root not found")
		}
		dir = parent
	}
}

func samePath(t *testing.T, left string, right string) bool {
	t.Helper()
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	return left == right || "/private"+left == right || left == "/private"+right
}
