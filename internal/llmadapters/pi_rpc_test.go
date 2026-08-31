package llmadapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestPiRPCLaunchSafetyAndSuccess(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "record.json")
	logPath := filepath.Join(t.TempDir(), "pi-rpc.jsonl")
	ctx := &trackedAfterFuncContext{done: make(chan struct{})}
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("success", recordPath),
		Timeout:           5 * time.Second,
	})

	stream, err := adapter.Start(ctx, Request{
		Model:   "opencode-go/kimi-k2.6",
		Effort:  "max",
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
	if ctx.active != 0 {
		t.Fatalf("active parent context registrations = %d, want 0", ctx.active)
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
	assertFlagValue(t, record.AdapterArgs, "--thinking", "max")
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

func TestPiRPCReviewerWorkspaceModeIsPermissionBounded(t *testing.T) {
	adapter := NewPiRPCAdapter(PiRPCOptions{})
	if got := AdapterReviewerWorkspaceMode(adapter); got != ReviewerWorkspacePermissionBounded {
		t.Fatalf("ReviewerWorkspaceMode = %q, want %q", got, ReviewerWorkspacePermissionBounded)
	}
	if got := AdapterReviewerWorkspaceMode(adapter); got == ReviewerWorkspaceWrite {
		t.Fatalf("ReviewerWorkspaceMode = %q, must not grant workspace_write", got)
	}
}

func TestPiRPCReviewerWorkspaceLaunchUsesOnlyCROwnedTools(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	recordPath := filepath.Join(tempDir, "record.json")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("reviewer-tools", recordPath),
		Timeout:           5 * time.Second,
	})

	stream, err := adapter.Start(context.Background(), Request{
		Prompt: "review assigned files",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir:            repoDir,
			ScratchDir:         scratchDir,
			DiffPath:           diffPath,
			AllowedFiles:       []string{"assigned.go"},
			MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	record := readPiRPCRecord(t, recordPath)
	if !samePath(t, record.Cwd, repoDir) {
		t.Fatalf("cwd = %q, want reviewer repo %q", record.Cwd, repoDir)
	}
	if containsFlag(record.AdapterArgs, "--no-tools") {
		t.Fatalf("args = %#v, reviewer extension tools must remain enabled", record.AdapterArgs)
	}
	assertFlagValue(t, record.AdapterArgs, "--tools", piRPCReviewerToolNames)
	reviewerPrompt := flagValue(record.AdapterArgs, "--system-prompt")
	for _, instruction := range []string{"Invoke cr_diff before cr_read, cr_search, or cr_list", "If cr_diff fails"} {
		if !strings.Contains(reviewerPrompt, instruction) {
			t.Fatalf("reviewer system prompt = %q, want instruction %q", reviewerPrompt, instruction)
		}
	}
	for _, flag := range []string{"--no-builtin-tools", "--no-context-files", "--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session"} {
		if !containsFlag(record.AdapterArgs, flag) {
			t.Fatalf("args = %#v, want %s", record.AdapterArgs, flag)
		}
	}
	extensionPath := flagValue(record.AdapterArgs, "--extension")
	if extensionPath == "" || !pathWithin(t, scratchDir, extensionPath) {
		t.Fatalf("--extension = %q, want generated extension under %q", extensionPath, scratchDir)
	}
	extension := []byte(record.Extension)
	for _, tool := range []string{"cr_read", "cr_search", "cr_list", "cr_diff"} {
		if !strings.Contains(string(extension), `name: "`+tool+`"`) {
			t.Fatalf("extension does not register %s:\n%s", tool, extension)
		}
	}
	for _, forbidden := range []string{"workspace_write", `name: "bash"`, `name: "edit"`, `name: "write"`} {
		if strings.Contains(strings.ToLower(string(extension)), forbidden) {
			t.Fatalf("extension contains forbidden capability %q:\n%s", forbidden, extension)
		}
	}
	for _, key := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE", "XDG_CACHE_HOME"} {
		value := record.Env[key]
		if value == "" || !pathWithin(t, scratchDir, value) {
			t.Fatalf("%s = %q, want scratch-rooted path under %q", key, value, scratchDir)
		}
	}
}

func TestPiRPCReviewerWorkspaceRejectsUnknownToolEvents(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("tool", filepath.Join(tempDir, "record.json")),
		Timeout:           5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{
		Prompt: "review",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); !errors.Is(err, ErrToolUse) {
		t.Fatalf("Wait error = %v, want ErrToolUse for native Read event", err)
	}
}

func TestPiRPCReviewerLogCapDoesNotBreakProtocolCompletion(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	logPath := filepath.Join(tempDir, "reviewer.jsonl")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("reviewer-log-flood", filepath.Join(tempDir, "record.json")),
		Timeout:           5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{
		Prompt:  "review",
		LogPath: logPath,
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want completed final response", response.StructuredOutput)
	}
	if response.ReviewerToolEvidence == nil || response.ReviewerToolEvidence.DiffStatus != llm.DiffToolStatusNotInvoked {
		t.Fatalf("reviewer tool evidence = %#v, want not-invoked cr_diff", response.ReviewerToolEvidence)
	}
	logged, err := os.ReadFile(logPath) // #nosec G304 -- logPath is rooted in t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if len(logged) > 2048 {
		t.Fatalf("reviewer log = %d bytes, want aggregate cap 2048", len(logged))
	}
	if !strings.Contains(string(logged), "reviewer RPC/stderr log cap reached") {
		t.Fatalf("reviewer log = %q, want cap marker", logged)
	}
	if !strings.Contains(string(logged), "codereview-pi-tool-evidence tool=cr_diff status=not_invoked started=0 completed=0 failed=0") {
		t.Fatalf("reviewer log = %q, want bounded no-invocation evidence", logged)
	}
}

func TestPiRPCReviewerReportsIncompleteDiffEvidence(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("reviewer-diff-incomplete", filepath.Join(tempDir, "record.json")),
		Timeout:           5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{
		Prompt: "review",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if response.ReviewerToolEvidence == nil || response.ReviewerToolEvidence.DiffStatus != llm.DiffToolStatusIncomplete {
		t.Fatalf("reviewer tool evidence = %#v, want incomplete cr_diff", response.ReviewerToolEvidence)
	}
}

func TestPiRPCReviewerLogCapPreservesDiffFailureEvidence(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	logPath := filepath.Join(tempDir, "reviewer.jsonl")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("reviewer-diff-failure-log-flood", filepath.Join(tempDir, "record.json")),
		Timeout:           5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{
		Prompt:  "review",
		LogPath: logPath,
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want completed final response", response.StructuredOutput)
	}
	if response.ReviewerToolEvidence == nil || response.ReviewerToolEvidence.DiffStatus != llm.DiffToolStatusFailed || response.ReviewerToolEvidence.DiffDiagnostic != "fixed diff unavailable" {
		t.Fatalf("reviewer tool evidence = %#v, want failed cr_diff with bounded diagnostic", response.ReviewerToolEvidence)
	}
	logged, err := os.ReadFile(logPath) // #nosec G304 -- logPath is rooted in t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if len(logged) > 2048 {
		t.Fatalf("reviewer log = %d bytes, want aggregate cap 2048", len(logged))
	}
	if !strings.Contains(string(logged), "reviewer RPC/stderr log cap reached") {
		t.Fatalf("reviewer log = %q, want cap marker", logged)
	}
	if !strings.Contains(string(logged), "codereview-pi-tool-evidence tool=cr_diff status=failed started=1 completed=1 failed=1") {
		t.Fatalf("reviewer log = %q, want bounded failed-invocation evidence", logged)
	}
	if !strings.Contains(string(logged), `error="fixed diff unavailable"`) {
		t.Fatalf("reviewer log = %q, want precise cr_diff failure", logged)
	}
}

func TestPiRPCReviewerExtensionLoadsInInstalledPi(t *testing.T) {
	piPath, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("Pi is not installed")
	}
	tempDir := t.TempDir()
	extensionPath := filepath.Join(tempDir, "cr-review-tools.mjs")
	extension := piRPCReviewerExtension(os.Args[0], filepath.Join(tempDir, "config.json"), tempDir, 2048, time.Second, piRPCPreflightRegistration)
	if err := os.WriteFile(extensionPath, []byte(extension), 0o600); err != nil { // #nosec G703 -- extensionPath is rooted in t.TempDir.
		t.Fatalf("WriteFile(extension): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, piPath,
		"--mode", "rpc",
		"--system-prompt", piRPCReviewerSystemPrompt,
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session",
		"--no-builtin-tools", "--no-context-files", "--no-approve",
		"--tools", piRPCReviewerToolNames,
		"--extension", extensionPath,
	) // #nosec G204 -- test launches the discovered Pi executable with fixed arguments.
	cmd.Dir = tempDir
	cmd.Stdin = strings.NewReader("{\"id\":\"state-1\",\"type\":\"get_state\"}\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi extension load: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"id":"state-1"`) || !strings.Contains(string(output), `"success":true`) {
		t.Fatalf("Pi get_state output = %s, want successful response", output)
	}
	if !piRPCPreflightReceivedRegistration(string(output)) {
		t.Fatalf("Pi extension output = %s, want registration completion marker", output)
	}
	if err := NewPiRPCAdapter(PiRPCOptions{Command: piPath}).preflightReviewerRuntime(context.Background()); err != nil {
		t.Fatalf("installed Pi reviewer preflight: %v", err)
	}
}

func TestPiRPCReviewerPreflightRejectsUnsupportedPi(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env: append(piRPCHelperEnv("success", filepath.Join(tempDir, "record.json")),
			"LLM_PI_RPC_HELP_UNSUPPORTED=1",
		),
		Timeout: 5 * time.Second,
	})
	stream, err := adapter.Start(context.Background(), Request{
		Prompt: "review",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
		},
	})
	if !errors.Is(err, ErrPiRPCIncompatible) {
		t.Fatalf("Start error = %v, want classified Pi compatibility error", err)
	}
	if stream != nil {
		t.Fatalf("stream = %#v, want nil before reviewer launch", stream)
	}
}

func TestPiRPCReviewerPreflightDoesNotCacheCanceledContext(t *testing.T) {
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "repo")
	scratchDir := filepath.Join(tempDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	diffPath := filepath.Join(tempDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("diff\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(diff): %v", err)
	}
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env:               piRPCHelperEnv("success", filepath.Join(tempDir, "record.json")),
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	req := Request{Prompt: "review", ReviewerWorkspace: &ReviewerWorkspaceRequest{
		RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
	}}
	if stream, err := adapter.Start(canceled, req); err == nil || stream != nil {
		t.Fatalf("canceled Start = (%#v, %v), want preflight failure", stream, err)
	}
	stream, err := adapter.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("healthy Start after cancellation: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("healthy Wait after cancellation: %v", err)
	}
}

func TestPiRPCReviewerPreflightUsesEmptyDiscoveryDisabledDirectory(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "preflight.json")
	mutationPath := filepath.Join(tempDir, "hostile-resource-loaded")
	adapter := NewPiRPCAdapter(PiRPCOptions{
		Command:           os.Args[0],
		commandArgsPrefix: piRPCHelperPrefix(),
		Env: []string{
			"LLM_PI_RPC_HELPER=1",
			"LLM_HELPER_RECORD=" + recordPath,
			"LLM_PI_RPC_HOSTILE_MUTATION=" + mutationPath,
		},
	})
	if err := adapter.preflightReviewerRuntime(context.Background()); err != nil {
		t.Fatalf("preflightReviewerRuntime: %v", err)
	}
	record := readPiRPCRecord(t, recordPath)
	if record.CwdEntries != 0 {
		t.Fatalf("preflight cwd = %q with %d entries, want empty isolated repository", record.Cwd, record.CwdEntries)
	}
	if samePath(t, record.Cwd, repoRootForTest(t)) {
		t.Fatalf("preflight cwd = repository root %q", record.Cwd)
	}
	assertFlagValue(t, record.AdapterArgs, "--mode", "rpc")
	assertFlagValue(t, record.AdapterArgs, "--tools", piRPCReviewerToolNames)
	for _, flag := range []string{"--no-builtin-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-approve", "--no-session"} {
		if !containsFlag(record.AdapterArgs, flag) {
			t.Fatalf("preflight args = %#v, want %s", record.AdapterArgs, flag)
		}
	}
	if extensionPath := flagValue(record.AdapterArgs, "--extension"); extensionPath == "" || filepath.Base(extensionPath) != "cr-review-tools.mjs" {
		t.Fatalf("preflight extension = %q, want generated reviewer extension", extensionPath)
	}
	if got := strings.Count(record.Extension, "pi.registerTool"); got != 4 {
		t.Fatalf("preflight extension registers %d tools, want 4", got)
	}
	if !strings.Contains(record.Extension, piRPCPreflightRegistration) {
		t.Fatalf("preflight extension = %q, want registration-completion marker", record.Extension)
	}
	if _, err := os.Stat(mutationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile resource mutation stat = %v, want resource undiscovered", err)
	}
}

func TestPiRPCPreflightReceivedStateRequiresOneSuccessfulStateResponse(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "valid response", output: `{"id":"state-1","success":true}` + "\n", want: true},
		{name: "substrings in diagnostic", output: `warning: {"id":"state-1"} {"success":true}` + "\n", want: false},
		{name: "wrong response id", output: `{"id":"prompt-1","success":true}` + "\n", want: false},
		{name: "failed response", output: `{"id":"state-1","success":false}` + "\n", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := piRPCPreflightReceivedState(tt.output); got != tt.want {
				t.Fatalf("piRPCPreflightReceivedState(%q) = %t, want %t", tt.output, got, tt.want)
			}
		})
	}
}

func TestPiRPCPreflightReceivedRegistrationRequiresExactMarkerLine(t *testing.T) {
	if piRPCPreflightReceivedRegistration("prefix " + piRPCPreflightRegistration + " suffix\n") {
		t.Fatal("registration marker embedded in diagnostic was accepted")
	}
	if !piRPCPreflightReceivedRegistration(piRPCPreflightRegistration + "\n") {
		t.Fatal("exact registration marker was not accepted")
	}
}

func TestPiRPCPreflightRequiresResponsesOnTheirExpectedStreams(t *testing.T) {
	state := `{"id":"state-1","success":true}` + "\n"
	marker := piRPCPreflightRegistration + "\n"
	if !piRPCPreflightReady(state, marker) {
		t.Fatal("preflight readiness = false, want valid separated streams")
	}
	if piRPCPreflightReady(marker+state, "") || piRPCPreflightReady("", state+marker) {
		t.Fatal("preflight accepted state or marker from the wrong stream")
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
	if stream.SessionID() != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", stream.SessionID())
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
	assertPiRPCFinalEventsPreserved(t, logged)
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
		if _, ok := event["message"]; ok {
			t.Fatalf("message_update = %#v, want redundant root message stripped", event)
		}
		if event["sessionId"] != "session-1" {
			t.Fatalf("message_update sessionId = %#v, want session-1", event["sessionId"])
		}
		assistantEvent, ok := event["assistantMessageEvent"].(map[string]any)
		if !ok {
			t.Fatalf("message_update = %#v, want assistantMessageEvent", event)
		}
		if assistantEvent["type"] != "thinking_delta" {
			t.Fatalf("assistantMessageEvent.type = %#v, want thinking_delta", assistantEvent["type"])
		}
		if assistantEvent["contentIndex"] != float64(0) {
			t.Fatalf("assistantMessageEvent.contentIndex = %#v, want 0", assistantEvent["contentIndex"])
		}
		if assistantEvent["delta"] != "x" {
			t.Fatalf("assistantMessageEvent.delta = %#v, want x", assistantEvent["delta"])
		}
		partial, ok := assistantEvent["partial"].(map[string]any)
		if !ok {
			t.Fatalf("message_update = %#v, want compact partial metadata", event)
		}
		wantPartial := map[string]any{
			"role":       "assistant",
			"provider":   "opencode-go",
			"model":      "deepseek-v4-pro",
			"api":        "openai-completions",
			"stopReason": "stop",
		}
		if !reflect.DeepEqual(partial, wantPartial) {
			t.Fatalf("partial = %#v, want exact compact metadata %#v", partial, wantPartial)
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

func assertPiRPCFinalEventsPreserved(t *testing.T, logged []byte) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(string(logged)))
	var messageEndFound, agentEndFound bool
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("Unmarshal(log line): %v", err)
		}
		switch event["type"] {
		case "message_end":
			message, ok := event["message"].(map[string]any)
			if !ok {
				t.Fatalf("message_end = %#v, want message", event)
			}
			content, ok := message["content"].([]any)
			if !ok || len(content) == 0 {
				t.Fatalf("message_end.message = %#v, want final content", message)
			}
			contentBlock, ok := content[0].(map[string]any)
			if !ok || contentBlock["text"] != `{"ok":true}` {
				t.Fatalf("message_end.message = %#v, want final structured content", message)
			}
			usage, ok := message["usage"].(map[string]any)
			if !ok || usage["input"] != float64(100) || usage["output"] != float64(50) {
				t.Fatalf("message_end.message = %#v, want final usage", message)
			}
			messageEndFound = true
		case "agent_end":
			messages, ok := event["messages"].([]any)
			if !ok || len(messages) == 0 {
				t.Fatalf("agent_end = %#v, want messages", event)
			}
			agentEndFound = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan(log): %v", err)
	}
	if !messageEndFound {
		t.Fatal("log missing message_end with final message content and usage")
	}
	if !agentEndFound {
		t.Fatal("log missing agent_end messages")
	}
}

func TestNormalizePiRPCLogLinePreservesRootMessageForUnrecognizedPartial(t *testing.T) {
	line := []byte(`{"type":"message_update","message":{"role":"assistant","content":[{"type":"thinking","thinking":"cumulative"}]},"assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"x","partial":{"content":[{"type":"unknown","value":"unrecognized"}],"usage":{"tokensIn":100,"tokensOut":50}}}}`)

	normalized := normalizePiRPCLogLine(line)
	var event map[string]any
	if err := json.Unmarshal(normalized, &event); err != nil {
		t.Fatalf("Unmarshal(normalized): %v", err)
	}
	message, ok := event["message"].(map[string]any)
	if !ok {
		t.Fatalf("normalized message_update = %#v, want root message preserved", event)
	}
	if message["role"] != "assistant" {
		t.Fatalf("root message = %#v, want assistant message preserved", message)
	}
	assistantEvent, ok := event["assistantMessageEvent"].(map[string]any)
	if !ok {
		t.Fatalf("normalized message_update = %#v, want assistantMessageEvent", event)
	}
	if _, ok := assistantEvent["partial"]; ok {
		t.Fatalf("assistantMessageEvent = %#v, want unrecognized partial removed", assistantEvent)
	}
}

func TestNormalizePiRPCLogLinePreservesRootMessageForInvalidAssistantEvent(t *testing.T) {
	for _, tt := range []struct {
		name      string
		eventType string
		role      string
	}{
		{name: "unknown event type", eventType: "future_or_bad_event", role: "assistant"},
		{name: "invalid partial role", eventType: "thinking_delta", role: "user"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			partial := map[string]any{
				"role":       tt.role,
				"provider":   "opencode-go",
				"model":      "deepseek-v4-pro",
				"api":        "openai-completions",
				"stopReason": "stop",
				"timestamp":  1700000000000,
				"content":    []map[string]any{{"type": "thinking", "thinking": "cumulative"}},
				"usage": map[string]any{
					"input":       100,
					"output":      50,
					"cacheRead":   0,
					"cacheWrite":  0,
					"totalTokens": 150,
					"cost": map[string]any{
						"input":      0,
						"output":     0,
						"cacheRead":  0,
						"cacheWrite": 0,
						"total":      0,
					},
				},
			}
			event := map[string]any{
				"type":    "message_update",
				"message": partial,
				"assistantMessageEvent": map[string]any{
					"type":         tt.eventType,
					"contentIndex": 0,
					"delta":        "x",
					"partial":      partial,
				},
			}
			line, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal(event): %v", err)
			}
			normalized := normalizePiRPCLogLine(line)
			var decoded map[string]any
			if err := json.Unmarshal(normalized, &decoded); err != nil {
				t.Fatalf("Unmarshal(normalized): %v", err)
			}
			if _, ok := decoded["message"]; !ok {
				t.Fatalf("normalized message_update = %#v, want root message preserved", decoded)
			}
		})
	}
}

func TestNormalizePiRPCLogLinePreservesNonidenticalRootMessage(t *testing.T) {
	partial := map[string]any{
		"role":       "assistant",
		"provider":   "opencode-go",
		"model":      "deepseek-v4-pro",
		"api":        "openai-completions",
		"stopReason": "stop",
		"timestamp":  1700000000000,
		"content":    []map[string]any{{"type": "thinking", "thinking": "partial"}},
		"usage": map[string]any{
			"input":       100,
			"output":      50,
			"cacheRead":   0,
			"cacheWrite":  0,
			"totalTokens": 150,
			"cost": map[string]any{
				"input":      0,
				"output":     0,
				"cacheRead":  0,
				"cacheWrite": 0,
				"total":      0,
			},
		},
	}
	event := map[string]any{
		"type": "message_update",
		"message": map[string]any{
			"role":       "assistant",
			"provider":   "opencode-go",
			"model":      "deepseek-v4-pro",
			"api":        "openai-completions",
			"stopReason": "stop",
			"timestamp":  1700000000000,
			"content":    []map[string]any{{"type": "thinking", "thinking": "root"}},
			"usage": map[string]any{
				"input":       100,
				"output":      50,
				"cacheRead":   0,
				"cacheWrite":  0,
				"totalTokens": 150,
				"cost": map[string]any{
					"input":      0,
					"output":     0,
					"cacheRead":  0,
					"cacheWrite": 0,
					"total":      0,
				},
			},
		},
		"assistantMessageEvent": map[string]any{
			"type":         "thinking_delta",
			"contentIndex": 0,
			"delta":        "x",
			"partial":      partial,
		},
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal(event): %v", err)
	}
	normalized := normalizePiRPCLogLine(line)
	var decoded map[string]any
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("Unmarshal(normalized): %v", err)
	}
	message, ok := decoded["message"].(map[string]any)
	if !ok {
		t.Fatalf("normalized message_update = %#v, want nonidentical root message preserved", decoded)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("root message = %#v, want root content preserved", message)
	}
	contentBlock, ok := content[0].(map[string]any)
	if !ok || contentBlock["thinking"] != "root" {
		t.Fatalf("root message = %#v, want root snapshot preserved", message)
	}
	want := append(append([]byte(nil), line...), '\n')
	if !bytes.Equal(normalized, want) {
		t.Fatalf("normalized = %s, want original line preserved", normalized)
	}
}

func TestNormalizePiRPCLogLinePreservesOriginalForInvalidPiUpdateShape(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(partial, assistantEvent map[string]any)
	}{
		{
			name: "missing assistant timestamp",
			mutate: func(partial, _ map[string]any) {
				delete(partial, "timestamp")
			},
		},
		{
			name: "missing assistant usage",
			mutate: func(partial, _ map[string]any) {
				delete(partial, "usage")
			},
		},
		{
			name: "missing event content index",
			mutate: func(_, assistantEvent map[string]any) {
				delete(assistantEvent, "contentIndex")
			},
		},
		{
			name: "wrong event delta type",
			mutate: func(_, assistantEvent map[string]any) {
				assistantEvent["delta"] = 1
			},
		},
		{
			name: "unknown content block",
			mutate: func(partial, _ map[string]any) {
				partial["content"] = []map[string]any{{"type": "unknown", "value": "unrecognized"}}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			partial := map[string]any{
				"role":       "assistant",
				"provider":   "opencode-go",
				"model":      "deepseek-v4-pro",
				"api":        "openai-completions",
				"stopReason": "stop",
				"timestamp":  float64(1700000000000),
				"content":    []map[string]any{{"type": "thinking", "thinking": "cumulative"}},
				"usage": map[string]any{
					"input":       100,
					"output":      50,
					"cacheRead":   0,
					"cacheWrite":  0,
					"totalTokens": 150,
					"cost": map[string]any{
						"input":      0,
						"output":     0,
						"cacheRead":  0,
						"cacheWrite": 0,
						"total":      0,
					},
				},
			}
			assistantEvent := map[string]any{
				"type":         "thinking_delta",
				"contentIndex": 0,
				"delta":        "x",
				"partial":      partial,
			}
			event := map[string]any{
				"type":                  "message_update",
				"message":               partial,
				"assistantMessageEvent": assistantEvent,
			}
			tt.mutate(partial, assistantEvent)
			line, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal(event): %v", err)
			}
			normalized := normalizePiRPCLogLine(line)
			want := append(append([]byte(nil), line...), '\n')
			if !bytes.Equal(normalized, want) {
				t.Fatalf("normalized = %s, want original line preserved", normalized)
			}
		})
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
	req := Request{Model: "opencode-go/kimi-k2.6", Prompt: "prompt"}
	args, err := adapter.buildArgs(req, "")
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
			if err := adapter.validateArgs(tt.args, req, ""); !errors.Is(err, ErrUnsafeSubprocessConfig) {
				t.Fatalf("validateArgs error = %v, want ErrUnsafeSubprocessConfig", err)
			}
		})
	}
}

func TestPiRPCRejectsUnsafeReviewerSpecs(t *testing.T) {
	adapter := NewPiRPCAdapter(PiRPCOptions{})
	extensionPath := filepath.Join(t.TempDir(), "extension.mjs")
	req := Request{Prompt: "prompt", ReviewerWorkspace: &ReviewerWorkspaceRequest{}}
	args, err := adapter.buildArgs(req, extensionPath)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "missing builtin disable", args: removeFlag(args, "--no-builtin-tools")},
		{name: "missing context disable", args: removeFlag(args, "--no-context-files")},
		{name: "missing project approval disable", args: removeFlag(args, "--no-approve")},
		{name: "all tools disabled", args: append(removeFlagWithValue(args, "--tools"), "--no-tools")},
		{name: "native bash added", args: replaceFlagValue(args, "--tools", piRPCReviewerToolNames+",bash")},
		{name: "wrong extension", args: replaceFlagValue(args, "--extension", filepath.Join(t.TempDir(), "other.mjs"))},
		{name: "extra extension", args: append(args, "--extension", filepath.Join(t.TempDir(), "extra.mjs"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := adapter.validateArgs(tt.args, req, extensionPath); !errors.Is(err, ErrUnsafeSubprocessConfig) {
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
	if containsFlag(adapterArgsFromHelper(), "--help") {
		cwd, _ := os.Getwd()
		entries, _ := os.ReadDir(cwd)
		record := piRPCRecord{AdapterArgs: adapterArgsFromHelper(), Cwd: cwd, CwdEntries: len(entries)}
		if recordPath := os.Getenv("LLM_HELPER_RECORD"); recordPath != "" {
			data, _ := json.Marshal(record)
			_ = os.WriteFile(recordPath, data, 0o600) // #nosec G703 -- helper writes only to a test-owned path.
		}
		safe := len(entries) == 0
		for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-approve", "--no-session"} {
			safe = safe && containsFlag(record.AdapterArgs, flag)
		}
		if !safe && os.Getenv("LLM_PI_RPC_HOSTILE_MUTATION") != "" {
			_ = os.WriteFile(os.Getenv("LLM_PI_RPC_HOSTILE_MUTATION"), []byte("loaded"), 0o600) // #nosec G703 -- test helper writes to a test-owned marker.
		}
		if os.Getenv("LLM_PI_RPC_HELP_UNSUPPORTED") == "1" {
			fmt.Println("--mode rpc --system-prompt --no-tools")
		} else {
			fmt.Println("--mode rpc --system-prompt --no-tools --no-builtin-tools --tools --extension --no-extensions --no-skills --no-prompt-templates --no-themes --no-session --no-context-files --no-approve explicit -e paths still work")
		}
		os.Exit(0)
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
		Env: map[string]string{
			"TMPDIR":         os.Getenv("TMPDIR"),
			"GOTMPDIR":       os.Getenv("GOTMPDIR"),
			"GOCACHE":        os.Getenv("GOCACHE"),
			"XDG_CACHE_HOME": os.Getenv("XDG_CACHE_HOME"),
		},
	}
	if extensionPath := flagValue(record.AdapterArgs, "--extension"); extensionPath != "" {
		record.Extension = string(mustReadHelperFile(extensionPath))
		if strings.Contains(record.Extension, piRPCPreflightRegistration) {
			fmt.Fprintln(os.Stderr, piRPCPreflightRegistration)
		}
	}
	if recordPath != "" {
		data, _ := json.Marshal(record)
		// #nosec G703 -- helper writes only to a t.TempDir path supplied by the parent test.
		_ = os.WriteFile(recordPath, data, 0o600)
	}
	if command["type"] == "get_state" {
		if os.Getenv("LLM_PI_RPC_HELP_UNSUPPORTED") == "1" {
			fmt.Println(`{"id":"state-1","success":false,"error":"unsupported"}`)
		} else {
			fmt.Println(`{"id":"state-1","success":true}`)
		}
		os.Exit(0)
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
			partial := map[string]any{
				"role":       "assistant",
				"provider":   "opencode-go",
				"model":      "deepseek-v4-pro",
				"api":        "openai-completions",
				"stopReason": "stop",
				"timestamp":  1700000000000,
				"content":    []map[string]any{{"type": "thinking", "thinking": thinking}},
				"usage": map[string]any{
					"input":       100,
					"output":      50,
					"cacheRead":   0,
					"cacheWrite":  0,
					"totalTokens": 150,
					"cost": map[string]any{
						"input":      0,
						"output":     0,
						"cacheRead":  0,
						"cacheWrite": 0,
						"total":      0,
					},
				},
			}
			event := map[string]any{
				"type":      "message_update",
				"message":   partial,
				"sessionId": "session-1",
				"assistantMessageEvent": map[string]any{
					"type":         "thinking_delta",
					"contentIndex": 0,
					"delta":        "x",
					"partial":      partial,
				},
			}
			data, _ := json.Marshal(event)
			fmt.Println(string(data))
		}
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}],"api":"openai-completions","provider":"opencode-go","model":"deepseek-v4-pro","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0,"totalTokens":150,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1700000000000}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "prompt-failure":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":false,"error":"No API key found for opencode-go"}`)
	case "tool":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"Read","args":{"path":"x"}}`)
		time.Sleep(10 * time.Second)
	case "reviewer-tools":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		for _, tool := range []string{"cr_read", "cr_search", "cr_list", "cr_diff"} {
			fmt.Printf("{\"type\":\"tool_execution_start\",\"toolCallId\":\"%s\",\"toolName\":%q,\"args\":{}}\n", tool, tool)
			fmt.Printf("{\"type\":\"tool_execution_end\",\"toolCallId\":\"%s\",\"toolName\":%q,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n", tool, tool)
		}
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "reviewer-log-flood":
		fmt.Fprintln(os.Stderr, strings.Repeat("stderr flood\n", 1000))
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		for i := 0; i < 20; i++ {
			fmt.Printf("{\"type\":\"tool_execution_end\",\"toolCallId\":\"tool-%d\",\"toolName\":\"cr_read\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", i, strings.Repeat("tool output ", 500))
		}
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "reviewer-diff-failure-log-flood":
		fmt.Fprintln(os.Stderr, strings.Repeat("stderr flood\n", 1000))
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"tool_execution_start","toolCallId":"diff-1","toolName":"cr_diff","args":{}}`)
		fmt.Println(`{"type":"tool_execution_end","toolCallId":"diff-1","toolName":"cr_diff","result":{"content":[{"type":"text","text":"fixed diff unavailable"}],"isError":true}}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
	case "reviewer-diff-incomplete":
		fmt.Println(`{"id":"prompt-1","type":"response","command":"prompt","success":true}`)
		fmt.Println(`{"type":"tool_execution_start","toolCallId":"diff-1","toolName":"cr_diff","args":{}}`)
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		fmt.Println(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`)
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
	Env         map[string]string   `json:"env"`
	Extension   string              `json:"extension"`
}

func mustReadHelperFile(path string) []byte {
	data, _ := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- helper reads the CR-generated extension path from its own argv.
	return data
}

func pathWithin(t *testing.T, root, candidate string) bool {
	t.Helper()
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
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

func removeFlagWithValue(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
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
