package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPiRPCLaunchSafetyAndSuccess(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "record.json")
	logPath := filepath.Join(t.TempDir(), "pi-rpc.jsonl")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("success", recordPath),
		Timeout:           5 * time.Second,
	})

	stream, err := adapter.Start(context.Background(), Request{
		Model:   "opencode-go/kimi-k2.6",
		Effort:  "high",
		Prompt:  "review this diff",
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
		t.Fatalf("StructuredOutput = %s, want final assistant text only", response.StructuredOutput)
	}
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 3915 {
		t.Fatalf("Usage = %#v, want tokens_in", response.Usage)
	}
	if response.Usage.TokensOut == nil || *response.Usage.TokensOut != 50 {
		t.Fatalf("Usage = %#v, want tokens_out", response.Usage)
	}
	if response.Usage.CacheRead == nil || *response.Usage.CacheRead != 5 {
		t.Fatalf("Usage = %#v, want cache_read", response.Usage)
	}
	if response.Usage.CacheCreate == nil || *response.Usage.CacheCreate != 7 {
		t.Fatalf("Usage = %#v, want cache_create from cacheWrite", response.Usage)
	}
	if response.Usage.CostUSD == nil || *response.Usage.CostUSD != 0.00392005 {
		t.Fatalf("Usage = %#v, want cost_usd", response.Usage)
	}

	record := readPiRPCRecord(t, recordPath)
	assertFlagValue(t, record.AdapterArgs, "--mode", "rpc")
	assertFlagValue(t, record.AdapterArgs, "--model", "opencode-go/kimi-k2.6")
	assertFlagValue(t, record.AdapterArgs, "--system-prompt", piRPCSystemPrompt)
	for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session"} {
		if !containsFlag(record.AdapterArgs, flag) {
			t.Fatalf("args = %#v, want %s", record.AdapterArgs, flag)
		}
	}
	if len(record.Commands) != 1 {
		t.Fatalf("commands = %#v, want one prompt command", record.Commands)
	}
	if record.Commands[0]["type"] != "prompt" || record.Commands[0]["message"] != "review this diff" {
		t.Fatalf("prompt command = %#v, want prompt over stdin", record.Commands[0])
	}
	if record.Cwd == "" || record.Cwd == repoRootForTest(t) {
		t.Fatalf("cwd = %q, want scratch dir outside repo", record.Cwd)
	}
	if record.CwdEntries != 0 {
		t.Fatalf("cwd entries = %d, want empty scratch dir", record.CwdEntries)
	}
	// #nosec G304 -- test reads the log path it created with t.TempDir.
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if !strings.Contains(string(logged), `"agent_end"`) || !strings.Contains(string(logged), `"message_end"`) {
		t.Fatalf("log = %q, want raw RPC events", logged)
	}
}

func TestPiRPCLogStripsCumulativeStreamingPartials(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "record.json")
	logPath := filepath.Join(t.TempDir(), "pi-rpc.jsonl")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("streaming-partials", recordPath),
		Timeout:           5 * time.Second,
	})

	stream, err := adapter.Start(context.Background(), Request{
		Model:   "opencode-go/deepseek-v4-pro",
		Effort:  "high",
		Prompt:  "review this diff",
		LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want final assistant text", response.StructuredOutput)
	}

	logged, err := os.ReadFile(logPath) // #nosec G304 -- test reads the log path it created with t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	logText := string(logged)
	if !strings.Contains(logText, `"message_update"`) || !strings.Contains(logText, `"thinking_delta"`) || !strings.Contains(logText, `"delta"`) {
		t.Fatalf("log = %s, want normalized streaming update metadata", logText)
	}
	if !strings.Contains(logText, `"partial"`) || !strings.Contains(logText, `"provider"`) || !strings.Contains(logText, `"deepseek-v4-pro"`) {
		t.Fatalf("log = %s, want compact partial metadata preserved", logText)
	}
	if !strings.Contains(logText, `"message_end"`) || !strings.Contains(logText, `"agent_end"`) {
		t.Fatalf("log = %s, want final RPC events preserved", logText)
	}
	assertPiRPCMessageUpdatesCompacted(t, logged)
	if len(logged) > 10_000 {
		t.Fatalf("log size = %d bytes, want normalized bounded log", len(logged))
	}
}

func assertPiRPCMessageUpdatesCompacted(t *testing.T, logged []byte) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(string(logged)))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("Unmarshal(log line): %v", err)
		}
		if event["type"] != "message_update" {
			continue
		}
		assistantEvent, ok := event["assistantMessageEvent"].(map[string]any)
		if !ok {
			t.Fatalf("message_update = %#v, want assistantMessageEvent", event)
		}
		partial, ok := assistantEvent["partial"].(map[string]any)
		if !ok {
			t.Fatalf("message_update = %#v, want compact partial metadata", event)
		}
		if _, ok := partial["content"]; ok {
			t.Fatalf("partial = %#v, want content stripped", partial)
		}
		if _, ok := partial["usage"]; ok {
			t.Fatalf("partial = %#v, want usage stripped", partial)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan(log): %v", err)
	}
}

func TestPiRPCProtocolFailures(t *testing.T) {
	t.Run("prompt response failure", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("prompt-failure", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "No API key found") {
			t.Fatalf("Wait error = %v, want prompt failure", err)
		}
	})

	t.Run("tool execution event kills and fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("tool", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
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
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("sleep", recordPath),
			Timeout:           20 * time.Millisecond,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait error = %v, want deadline exceeded", err)
		}
	})

	t.Run("caller cancellation propagates before agent_end", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		ctx, cancel := context.WithCancel(context.Background())
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("sleep", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(ctx, Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		cancel()
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait error = %v, want context canceled", err)
		}
	})

	t.Run("malformed stdout JSONL fails immediately", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("malformed", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
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

	t.Run("missing agent_end fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("no-agent-end", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "agent_end") {
			t.Fatalf("Wait error = %v, want missing agent_end", err)
		}
	})

	t.Run("non-zero exit after agent_end fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("agent-end-exit-failure", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exit status 42") {
			t.Fatalf("Wait error = %v, want non-zero exit status", err)
		}
	})

	t.Run("non-zero exit before agent_end fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "record.json")
		adapter := NewPiRPCAdapter(PiRPCOptions{
			Command:           os.Args[0],
			commandArgsPrefix: piRPCHelperPrefix(),
			Env:               piRPCHelperEnv("exit-before-agent-end", recordPath),
			Timeout:           5 * time.Second,
		})
		stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exit status 43") {
			t.Fatalf("Wait error = %v, want non-zero exit status", err)
		}
	})
}

func TestPiRPCRejectsUnsafeSpecs(t *testing.T) {
	adapter := NewPiRPCAdapter(PiRPCOptions{})
	args, err := adapter.buildArgs(Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"}, t.TempDir())
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "tools allowlist", args: append([]string{"--tools", "read"}, args...)},
		{name: "missing no tools", args: removeFlag(args, "--no-tools")},
		{name: "missing no extensions", args: removeFlag(args, "--no-extensions")},
		{name: "unexpected flag", args: append([]string{"--unexpected"}, args...)},
		{name: "text mode", args: replaceFlagValue(args, "--mode", "text")},
		{name: "wrong system prompt", args: replaceFlagValue(args, "--system-prompt", "be loose")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := adapter.validateArgs(tt.args); !errors.Is(err, ErrUnsafeSubprocessConfig) {
				t.Fatalf("validateArgs error = %v, want ErrUnsafeSubprocessConfig", err)
			}
		})
	}
}

func TestPiRPCResumeUnsupported(t *testing.T) {
	adapter := NewPiRPCAdapter(PiRPCOptions{})
	if _, err := adapter.Resume(context.Background(), "session", Request{}); err == nil {
		t.Fatal("Resume error = nil, want unsupported")
	}
}

func TestPiRPCHelperProcess(_ *testing.T) {
	if os.Getenv("LLM_PI_RPC_HELPER") != "1" {
		return
	}
	recordPath := os.Getenv("LLM_HELPER_RECORD")
	cwd, _ := os.Getwd()
	entries, _ := os.ReadDir(cwd)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var rawCommand string
	if scanner.Scan() {
		rawCommand = scanner.Text()
	}
	var command map[string]string
	_ = json.Unmarshal([]byte(rawCommand), &command)
	record := piRPCRecord{
		AdapterArgs: adapterArgsFromHelper(),
		Cwd:         cwd,
		CwdEntries:  len(entries),
		Commands:    []map[string]string{command},
	}
	if recordPath != "" {
		data, _ := json.Marshal(record)
		// #nosec G703 -- helper writes only to a t.TempDir path supplied by the parent test.
		_ = os.WriteFile(recordPath, data, 0o600)
	}

	switch os.Getenv("LLM_HELPER_MODE") {
	case "success":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"agent_start","sessionId":"session-1"}`)
		fmt.Println(`{"type":"message_end","message":{"role":"user","content":"review this diff"}}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","text":"ignored"},{"type":"text","text":"{\"ok\":true}"}],"usage":{"tokensIn":3915,"tokensOut":50,"cacheRead":5,"cacheWrite":7,"cost":{"total":0.00392005}}}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "streaming-partials":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"agent_start","sessionId":"session-1"}`)
		thinking := ""
		for i := 0; i < 25; i++ {
			thinking += strings.Repeat("x", 200)
			event := map[string]any{
				"type": "message_update",
				"assistantMessageEvent": map[string]any{
					"type":         "thinking_delta",
					"contentIndex": 0,
					"delta":        "x",
					"partial": map[string]any{
						"role":       "assistant",
						"provider":   "opencode-go",
						"model":      "deepseek-v4-pro",
						"stopReason": "stop",
						"content":    []map[string]any{{"type": "thinking", "thinking": thinking}},
						"usage":      map[string]any{"tokensIn": 100, "tokensOut": 50, "totalTokens": 150},
					},
				},
			}
			data, _ := json.Marshal(event)
			fmt.Println(string(data))
		}
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"tokensIn":100,"tokensOut":50}}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "prompt-failure":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":false,"error":"No API key found for opencode-go"}`)
	case "tool":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"Read","args":{"path":"x"}}`)
		time.Sleep(10 * time.Second)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "malformed":
		fmt.Println(`{"type":`)
	case "no-agent-end":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
	case "agent-end-exit-failure":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
		os.Exit(42)
	case "exit-before-agent-end":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		os.Exit(43)
	case "spawn-child-tool":
		child := exec.Command(os.Args[0], "-test.run=TestPiRPCProcessGroupCleanup", "--") // #nosec G204,G702 -- helper launches the current test binary.
		child.Env = append(os.Environ(), "LLM_PI_RPC_CHILD=1")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(os.Getenv("LLM_HELPER_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600) // #nosec G703 -- helper writes to t.TempDir path.
		}
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"Read","args":{"path":"x"}}`)
		time.Sleep(10 * time.Second)
	default:
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"agent_end","messages":[]}`)
	}
	os.Exit(0)
}

type piRPCRecord struct {
	AdapterArgs []string            `json:"adapter_args"`
	Cwd         string              `json:"cwd"`
	CwdEntries  int                 `json:"cwd_entries"`
	Commands    []map[string]string `json:"commands"`
}

func piRPCHelperPrefix() []string {
	return []string{"-test.run=TestPiRPCHelperProcess", "--"}
}

func piRPCHelperEnv(mode string, recordPath string) []string {
	return []string{
		"LLM_PI_RPC_HELPER=1",
		"LLM_HELPER_MODE=" + mode,
		"LLM_HELPER_RECORD=" + recordPath,
	}
}

func readPiRPCRecord(t *testing.T, path string) piRPCRecord {
	t.Helper()
	// #nosec G304 -- test reads helper output from a t.TempDir path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(record): %v", err)
	}
	var record piRPCRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("Unmarshal(record): %v", err)
	}
	return record
}

func removeFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == flag {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func replaceFlagValue(args []string, flag string, value string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flag {
			out[i+1] = value
			return out
		}
	}
	return out
}

func TestPiRPCProcessGroupCleanup(_ *testing.T) {
	if os.Getenv("LLM_PI_RPC_CHILD") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func TestPiRPCToolUseKillsProcessGroup(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "record.json")
	childPIDPath := filepath.Join(tempDir, "child.pid")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env: append(piRPCHelperEnv("spawn-child-tool", recordPath),
			"LLM_HELPER_CHILD_PID="+childPIDPath,
		),
		Timeout: 5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = stream.Wait(context.Background())
	if !errors.Is(err, ErrToolUse) {
		t.Fatalf("Wait error = %v, want ErrToolUse", err)
	}
	pidData, err := os.ReadFile(childPIDPath) // #nosec G304 -- test reads helper pid from t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(child pid): %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("Atoi(child pid): %v", err)
	}
	eventually(t, 2*time.Second, func() bool {
		return !processExists(pid)
	})
}
