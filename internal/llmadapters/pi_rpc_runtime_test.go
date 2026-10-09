package llmadapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pireviewtool"
)

// piRuntimeVersionEnv opts this package into tests that drive the installed Pi
// runtime against a local mock provider. Its value is the exact version that
// `pi --version` must report: once it is set, a missing or different Pi fails
// instead of skipping. `make test-pi-runtime` sets it from the Makefile pin.
const piRuntimeVersionEnv = "CR_PI_RUNTIME_VERSION"

const (
	piRuntimeProviderName = "crruntime"
	piRuntimeModelID      = "local"
	piRuntimeModel        = piRuntimeProviderName + "/" + piRuntimeModelID
	piRuntimeAPIKey       = "cr-runtime-fixture-key" // #nosec G101 -- dummy key accepted only by the loopback mock provider.
	// piRuntimeHostileMarker is planted in global and project Pi resources the
	// fixture must never load. It must not reach the provider.
	piRuntimeHostileMarker = "CR_PI_RUNTIME_HOSTILE_RESOURCE"
	piRuntimeTestTimeout   = 60 * time.Second
	piRuntimeTaskTimeout   = 45 * time.Second
)

// TestMain lets the generated reviewer extension dispatch its tool helper to
// this test binary the way it dispatches to the cr binary in production, so
// runtime tests exercise the real pireviewtool.Run.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__pi-review-tool" {
		os.Exit(pireviewtool.Run(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestPiRPCRuntimeStructuredJSONWithoutTools(t *testing.T) {
	fixture := newPiRuntimeFixture(t,
		piRuntimeAnswer(`{"ok":true}`, piRuntimeUsage{prompt: 120, completion: 9, cached: 40, cacheWrite: 10}),
	)

	response, err := fixture.run(fixture.adapter(piRuntimeTaskTimeout), Request{Model: piRuntimeModel, Prompt: `Return {"ok":true}.`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %q, want the provider answer", response.StructuredOutput)
	}
	// input = 120 - 40 cached - 10 written; cost = 70*1 + 9*2 + 40*0.5 + 10*2.5.
	assertPiRuntimeUsage(t, response.Usage, 70, 9, 40, 10, 133)
	if response.ReviewerToolEvidence != nil {
		t.Fatalf("ReviewerToolEvidence = %#v, want none for a no-tools call", response.ReviewerToolEvidence)
	}
	requests := fixture.provider.requests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	if names := requests[0].toolNames(); len(names) != 0 {
		t.Fatalf("provider request tools = %v, want none", names)
	}
	if system := requests[0].systemText(); !strings.Contains(system, piRPCSystemPrompt) {
		t.Fatalf("provider system prompt = %q, want the CR structured-output prompt", system)
	}
}

func TestPiRPCRuntimeRecoversTransientProviderFailure(t *testing.T) {
	fixture := newPiRuntimeFixture(t,
		piRuntimeFailure(http.StatusServiceUnavailable, "fixture transient overload"),
		piRuntimeAnswer(`{"ok":true}`, piRuntimeUsage{prompt: 100, completion: 10}),
	)

	response, err := fixture.run(fixture.adapter(piRuntimeTaskTimeout), Request{Model: piRuntimeModel, Prompt: `Return {"ok":true}.`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %q, want the answer from the retried attempt", response.StructuredOutput)
	}
	// The failed attempt reports zero usage, so the total is the retry alone.
	assertPiRuntimeUsage(t, response.Usage, 100, 10, 0, 0, 120)
	if requests := fixture.provider.requests(); len(requests) != 2 {
		t.Fatalf("provider requests = %d, want the failed attempt and one retry", len(requests))
	}
}

func TestPiRPCRuntimePreservesPermanentProviderFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		steps    []piRuntimeStep
		requests int
		want     string
	}{
		{
			name:     "non-retryable rejection",
			steps:    []piRuntimeStep{piRuntimeFailure(http.StatusBadRequest, "fixture permanent rejection")},
			requests: 1,
			want:     "fixture permanent rejection",
		},
		{
			name: "retries exhausted",
			steps: []piRuntimeStep{
				piRuntimeFailure(http.StatusServiceUnavailable, "fixture overload one"),
				piRuntimeFailure(http.StatusServiceUnavailable, "fixture overload two"),
			},
			requests: 2,
			want:     "fixture overload two",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPiRuntimeFixture(t, tc.steps...)

			response, err := fixture.run(fixture.adapter(piRuntimeTaskTimeout), Request{Model: piRuntimeModel, Prompt: `Return {"ok":true}.`})
			if err == nil {
				t.Fatalf("run succeeded with %q, want the provider failure", response.StructuredOutput)
			}
			if !strings.Contains(err.Error(), "final assistant message error") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want final provider diagnostic %q", err, tc.want)
			}
			if len(response.StructuredOutput) != 0 {
				t.Fatalf("StructuredOutput = %q, want none on failure", response.StructuredOutput)
			}
			if requests := fixture.provider.requests(); len(requests) != tc.requests {
				t.Fatalf("provider requests = %d, want %d", len(requests), tc.requests)
			}
		})
	}
}

func TestPiRPCRuntimeReviewerToolsAndUsage(t *testing.T) {
	fixture := newPiRuntimeFixture(t,
		piRuntimeToolCalls(piRuntimeUsage{prompt: 100, completion: 5},
			piRuntimeToolCall{id: "read-early", name: "cr_read", args: `{"path":"main.go"}`},
		),
		piRuntimeToolCalls(piRuntimeUsage{prompt: 150, completion: 7, cached: 60, cacheWrite: 20},
			piRuntimeToolCall{id: "diff", name: "cr_diff", args: `{}`},
		),
		piRuntimeToolCalls(piRuntimeUsage{prompt: 200, completion: 11, cached: 120},
			piRuntimeToolCall{id: "read", name: "cr_read", args: `{"path":"main.go"}`},
			piRuntimeToolCall{id: "search", name: "cr_search", args: `{"query":"func Hello"}`},
			piRuntimeToolCall{id: "list", name: "cr_list", args: `{}`},
			piRuntimeToolCall{id: "escape", name: "cr_read", args: `{"path":"../outside.txt"}`},
		),
		piRuntimeAnswer(`{"findings":[]}`, piRuntimeUsage{prompt: 260, completion: 13, cached: 180, cacheWrite: 30}),
	)
	workspace := fixture.reviewerWorkspace()

	response, err := fixture.run(fixture.adapter(piRuntimeTaskTimeout), Request{
		Model:             piRuntimeModel,
		Prompt:            "Review the pinned change.",
		ReviewerWorkspace: workspace,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(response.StructuredOutput) != `{"findings":[]}` {
		t.Fatalf("StructuredOutput = %q, want the final answer after every tool turn", response.StructuredOutput)
	}
	if response.ReviewerToolEvidence == nil || response.ReviewerToolEvidence.DiffStatus != llm.DiffToolStatusSucceeded {
		t.Fatalf("ReviewerToolEvidence = %#v, want a succeeded cr_diff", response.ReviewerToolEvidence)
	}
	// Every assistant turn counts once: input 100+70+80+50, output 5+7+11+13,
	// cache read 0+60+120+180, cache write 0+20+0+30, cost 300 + 36*2 +
	// 360*0.5 + 50*2.5.
	assertPiRuntimeUsage(t, response.Usage, 300, 36, 360, 50, 677)

	requests := fixture.provider.requests()
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d, want one per assistant turn", len(requests))
	}
	wantTools := []string{"cr_diff", "cr_list", "cr_read", "cr_search"}
	for i, request := range requests {
		if got := request.toolNames(); !reflect.DeepEqual(got, wantTools) {
			t.Fatalf("request %d tools = %v, want only %v", i, got, wantTools)
		}
		if system := request.systemText(); !strings.Contains(system, piRPCReviewerSystemPrompt) {
			t.Fatalf("request %d system prompt = %q, want the CR reviewer prompt", i, system)
		}
	}
	assertPiRuntimeToolResult(t, requests[1], "read-early", "cr_diff must be invoked before inspecting repository files")
	assertPiRuntimeToolResult(t, requests[2], "diff", "+func Hello() string")
	assertPiRuntimeToolResult(t, requests[3], "read", `return "hello from the head revision"`)
	assertPiRuntimeToolResult(t, requests[3], "search", "main.go")
	assertPiRuntimeToolResult(t, requests[3], "list", "main.go\n")
	assertPiRuntimeToolResult(t, requests[3], "escape", "path traversal")
	if strings.Contains(string(requests[3].raw), "outside the repository") {
		t.Fatal("cr_read returned content from outside the repository root")
	}
	assertPiRuntimeDirEmpty(t, workspace.ScratchDir)
}

func TestPiRPCRuntimeReviewerRejectsUnlistedTool(t *testing.T) {
	shellMarker := filepath.Join(t.TempDir(), "shell-ran")
	fixture := newPiRuntimeFixture(t,
		piRuntimeToolCalls(piRuntimeUsage{prompt: 100, completion: 5},
			piRuntimeToolCall{id: "diff", name: "cr_diff", args: `{}`},
		),
		piRuntimeToolCalls(piRuntimeUsage{prompt: 150, completion: 5},
			piRuntimeToolCall{id: "shell", name: "bash", args: mustPiRuntimeJSON(t, map[string]string{"command": "touch " + shellMarker})},
		),
		piRuntimeAnswer(`{"findings":[]}`, piRuntimeUsage{prompt: 200, completion: 5}),
	)
	workspace := fixture.reviewerWorkspace()

	response, err := fixture.run(fixture.adapter(piRuntimeTaskTimeout), Request{
		Model:             piRuntimeModel,
		Prompt:            "Review the pinned change.",
		ReviewerWorkspace: workspace,
	})
	if !errors.Is(err, ErrToolUse) {
		t.Fatalf("run error = %v (output %q), want ErrToolUse for an unlisted tool", err, response.StructuredOutput)
	}
	if len(response.StructuredOutput) != 0 {
		t.Fatalf("StructuredOutput = %q, want none after a denied tool", response.StructuredOutput)
	}
	if _, statErr := os.Stat(shellMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("shell marker stat = %v, want the bash request never executed", statErr)
	}
	if requests := fixture.provider.requests(); len(requests) != 2 {
		t.Fatalf("provider requests = %d, want the run stopped at the denied tool call", len(requests))
	}
	assertPiRuntimeDirEmpty(t, workspace.ScratchDir)
}

// requirePiRuntime returns the installed Pi path when runtime tests are
// enabled. It skips only when the opt-in is unset; a set but blank version
// fails so a broken pin cannot pass by skipping.
func requirePiRuntime(t *testing.T) string {
	t.Helper()
	want, set := os.LookupEnv(piRuntimeVersionEnv)
	if !set {
		t.Skipf("set %s to the required Pi version (make test-pi-runtime) to run installed Pi runtime tests", piRuntimeVersionEnv)
	}
	if strings.TrimSpace(want) == "" {
		t.Fatalf("%s is set but blank; it must name the exact required Pi version", piRuntimeVersionEnv)
	}
	install := installedPiRuntime()
	if install.err != nil {
		t.Fatalf("%s=%s requires the installed Pi runtime: %v", piRuntimeVersionEnv, want, install.err)
	}
	if install.version != want {
		t.Fatalf("installed Pi %s reports version %q, want exactly %q", install.path, install.version, want)
	}
	return install.path
}

type piRuntimeInstall struct {
	path    string
	version string
	err     error
}

var installedPiRuntime = sync.OnceValue(func() piRuntimeInstall {
	path, err := exec.LookPath("pi")
	if err != nil {
		return piRuntimeInstall{err: err}
	}
	agentDir, err := os.MkdirTemp("", "cr-pi-runtime-version-*")
	if err != nil {
		return piRuntimeInstall{path: path, err: err}
	}
	defer func() { _ = os.RemoveAll(agentDir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version") // #nosec G204 -- test runs the discovered Pi executable with a fixed argument.
	cmd.Env = append(os.Environ(), piRuntimeIsolationEnv(agentDir, agentDir)...)
	output, err := cmd.Output()
	if err != nil {
		return piRuntimeInstall{path: path, err: fmt.Errorf("pi --version: %w", err)}
	}
	return piRuntimeInstall{path: path, version: strings.TrimSpace(string(output))}
})

// piRuntimeGoroutines counts live goroutines, leaving out LaunchProcess's
// pipe-close grace, which outlives a finished run by a bounded delay by design.
func piRuntimeGoroutines() (int, string) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var kept []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "internal/llm.LaunchProcess.func") && strings.Contains(stack, "time.Sleep") {
			continue
		}
		kept = append(kept, stack)
	}
	return len(kept), strings.Join(kept, "\n\n")
}

// piRuntimeIsolationEnv points Pi at a fixture-owned agent directory and home
// and turns off automatic network activity other than the model request.
func piRuntimeIsolationEnv(agentDir, home string) []string {
	return []string{
		"PI_CODING_AGENT_DIR=" + agentDir,
		"PI_CODING_AGENT_SESSION_DIR=" + filepath.Join(agentDir, "sessions"),
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		"HOME=" + home,
		"USERPROFILE=" + home,
	}
}

type piRuntimeFixture struct {
	t      *testing.T
	piPath string
	// command launches Pi; tests may point it at a wrapper of piPath.
	command  string
	root     string
	agentDir string
	env      []string
	provider *piRuntimeProvider
	// sideEffects are files that hostile global or project resources would
	// create if Pi loaded them.
	sideEffects []string
}

func newPiRuntimeFixture(t *testing.T, steps ...piRuntimeStep) *piRuntimeFixture {
	t.Helper()
	piPath := requirePiRuntime(t)
	goroutines, _ := piRuntimeGoroutines()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, stacks := piRuntimeGoroutines()
			if got <= goroutines {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutines = %d after the runtime test, want at most %d:\n%s", got, goroutines, stacks)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	root := t.TempDir()
	fixture := &piRuntimeFixture{
		t:        t,
		piPath:   piPath,
		command:  piPath,
		root:     root,
		agentDir: filepath.Join(root, "agent"),
		provider: newPiRuntimeProvider(t, steps),
	}
	home := filepath.Join(root, "home")
	fixture.env = piRuntimeIsolationEnv(fixture.agentDir, home)
	fixture.writeJSON(filepath.Join(fixture.agentDir, "models.json"), map[string]any{
		"providers": map[string]any{
			piRuntimeProviderName: map[string]any{
				"baseUrl": fixture.provider.server.URL + "/v1",
				"api":     "openai-completions",
				"apiKey":  piRuntimeAPIKey,
				"models": []any{map[string]any{
					"id":            piRuntimeModelID,
					"name":          "CR runtime fixture",
					"reasoning":     false,
					"input":         []string{"text"},
					"contextWindow": 100000,
					"maxTokens":     1000,
					// Rates per million tokens that make each token cost an
					// exactly representable amount: 1, 2, 0.5, and 2.5.
					"cost": map[string]any{"input": 1000000, "output": 2000000, "cacheRead": 500000, "cacheWrite": 2500000},
				}},
			},
		},
	})
	fixture.writeJSON(filepath.Join(fixture.agentDir, "settings.json"), map[string]any{
		"retry":                  map[string]any{"enabled": true, "maxRetries": 1, "baseDelayMs": 10},
		"enableInstallTelemetry": false,
	})
	// Resources at Pi's default global location. The agent directory override
	// must keep every one of them out of the run.
	fixture.plantHostileResources(filepath.Join(home, ".pi", "agent"), "global")
	fixture.writeFile(filepath.Join(home, ".pi", "agent", "settings.json"), `{"retry":{"enabled":false},"defaultProjectTrust":"always"}`)
	fixture.writeFile(filepath.Join(home, ".agents", "skills", "hostile", "SKILL.md"), "---\nname: hostile\ndescription: "+piRuntimeHostileMarker+"\n---\n"+piRuntimeHostileMarker+"\n")

	t.Cleanup(fixture.assertIsolated)
	return fixture
}

// plantHostileResources writes instructions, an extension, and an MCP server
// under dir. Loading any of them leaves the marker in a provider request or
// creates a side-effect file.
func (f *piRuntimeFixture) plantHostileResources(dir, label string) {
	f.t.Helper()
	extensionMarker := filepath.Join(f.root, label+"-extension-loaded")
	mcpMarker := filepath.Join(f.root, label+"-mcp-started")
	f.sideEffects = append(f.sideEffects, extensionMarker, mcpMarker)
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "SYSTEM.md", "APPEND_SYSTEM.md"} {
		f.writeFile(filepath.Join(dir, name), piRuntimeHostileMarker+" "+label+" "+name+"\n")
	}
	f.writeFile(filepath.Join(dir, "extensions", "hostile.js"),
		"import fs from \"node:fs\";\nexport default function () { fs.writeFileSync("+mustPiRuntimeJSON(f.t, extensionMarker)+", \"loaded\"); }\n")
	f.writeJSON(filepath.Join(dir, "mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"hostile": map[string]any{
				"command":     "node",
				"args":        []string{"-e", "require('node:fs').writeFileSync(" + mustPiRuntimeJSON(f.t, mcpMarker) + ", 'started')"},
				"description": piRuntimeHostileMarker,
			},
		},
	})
}

// reviewerWorkspace builds a disposable checkout that also carries hostile
// project-level Pi resources, as an untrusted pull request could.
func (f *piRuntimeFixture) reviewerWorkspace() *ReviewerWorkspaceRequest {
	f.t.Helper()
	workspaceRoot := filepath.Join(f.root, "workspace")
	repoDir := filepath.Join(workspaceRoot, "repo")
	scratchDir := filepath.Join(workspaceRoot, "scratch")
	f.writeFile(filepath.Join(repoDir, "main.go"), "package main\n\nfunc Hello() string {\n\treturn \"hello from the head revision\"\n}\n")
	f.writeFile(filepath.Join(workspaceRoot, "outside.txt"), "outside the repository\n")
	f.plantHostileResources(filepath.Join(repoDir, ".pi"), "project")
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		f.writeFile(filepath.Join(repoDir, name), piRuntimeHostileMarker+" project root "+name+"\n")
	}
	f.writeFile(filepath.Join(repoDir, ".agents", "skills", "hostile", "SKILL.md"), "---\nname: hostile\ndescription: "+piRuntimeHostileMarker+"\n---\n"+piRuntimeHostileMarker+"\n")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		f.t.Fatalf("MkdirAll(scratch): %v", err)
	}
	diffPath := filepath.Join(workspaceRoot, "diff.patch")
	f.writeFile(diffPath, "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,5 @@\n package main\n+\n+func Hello() string {\n+\treturn \"hello from the head revision\"\n+}\n")
	return &ReviewerWorkspaceRequest{
		RepoDir:            repoDir,
		ScratchDir:         scratchDir,
		DiffPath:           diffPath,
		MaxToolOutputBytes: 64 * 1024,
	}
}

func (f *piRuntimeFixture) adapter(timeout time.Duration) *PiRPCAdapter {
	return NewPiRPCAdapter(PiRPCOptions{
		Command: f.command,
		Env:     f.env,
		Timeout: timeout,
		ScratchDirFactory: func() (string, func() error, error) {
			dir, err := os.MkdirTemp(f.root, "scratch-*")
			if err != nil {
				return "", nil, err
			}
			return dir, func() error { return os.RemoveAll(dir) }, nil
		},
	})
}

func (f *piRuntimeFixture) run(adapter *PiRPCAdapter, req Request) (Response, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), piRuntimeTestTimeout)
	defer cancel()
	stream, err := adapter.Start(ctx, req)
	if err != nil {
		return Response{}, err
	}
	return stream.Wait(ctx)
}

func (f *piRuntimeFixture) assertIsolated() {
	t := f.t
	t.Helper()
	for i, request := range f.provider.requests() {
		if request.authorization != "Bearer "+piRuntimeAPIKey {
			t.Errorf("request %d authorization is not the fixture key", i)
		}
		if bytes.Contains(request.raw, []byte(piRuntimeHostileMarker)) {
			t.Errorf("request %d carries a global or project resource the fixture must not load", i)
		}
	}
	for _, unexpected := range f.provider.unexpectedRequests() {
		t.Errorf("unexpected provider request: %s", unexpected)
	}
	for _, path := range f.sideEffects {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("hostile resource side effect %s exists (stat err %v)", filepath.Base(path), err)
		}
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Errorf("ReadDir(fixture root): %v", err)
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "scratch-") {
			t.Errorf("adapter scratch %s was not removed", entry.Name())
		}
	}
}

func (f *piRuntimeFixture) writeJSON(path string, value any) {
	f.t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		f.t.Fatalf("Marshal(%s): %v", filepath.Base(path), err)
	}
	f.writeFile(path, string(data)+"\n")
}

func (f *piRuntimeFixture) writeFile(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- path is rooted in the fixture's t.TempDir.
		f.t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// piRuntimeStep answers one provider request.
type piRuntimeStep func(w http.ResponseWriter, r *http.Request, stop <-chan struct{})

type piRuntimeUsage struct {
	prompt     int
	completion int
	cached     int
	cacheWrite int
}

func (u piRuntimeUsage) payload() map[string]any {
	return map[string]any{
		"prompt_tokens":     u.prompt,
		"completion_tokens": u.completion,
		"total_tokens":      u.prompt + u.completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      u.cached,
			"cache_write_tokens": u.cacheWrite,
		},
	}
}

type piRuntimeToolCall struct {
	id   string
	name string
	args string
}

func piRuntimeAnswer(text string, usage piRuntimeUsage) piRuntimeStep {
	return func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		writePiRuntimeSSE(w,
			piRuntimeChunk(map[string]any{"role": "assistant", "content": text}, nil, nil),
			piRuntimeChunk(map[string]any{}, "stop", &usage),
		)
	}
}

func piRuntimeToolCalls(usage piRuntimeUsage, calls ...piRuntimeToolCall) piRuntimeStep {
	toolCalls := make([]any, 0, len(calls))
	for i, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"index":    i,
			"id":       call.id,
			"type":     "function",
			"function": map[string]any{"name": call.name, "arguments": call.args},
		})
	}
	return func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		writePiRuntimeSSE(w,
			piRuntimeChunk(map[string]any{"role": "assistant", "tool_calls": toolCalls}, nil, nil),
			piRuntimeChunk(map[string]any{}, "tool_calls", &usage),
		)
	}
}

// piRuntimeFailure returns an OpenAI-style error. Pi retries by matching the
// error text, so only 5xx responses carry the retryable server_error type.
func piRuntimeFailure(status int, message string) piRuntimeStep {
	errorType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		errorType = "server_error"
	}
	return func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": errorType}})
	}
}

// piRuntimeStall starts a stream and then holds the request open until the
// client disconnects or the fixture shuts down. started is closed once the
// stream is open; disconnected is closed if the client goes away first.
func piRuntimeStall(started, disconnected chan<- struct{}) piRuntimeStep {
	return func(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(piRuntimeChunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-stop:
		}
	}
}

func piRuntimeChunk(delta map[string]any, finishReason any, usage *piRuntimeUsage) map[string]any {
	chunk := map[string]any{
		"id":      "cr-runtime-completion",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   piRuntimeModelID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
	if usage != nil {
		chunk["usage"] = usage.payload()
	}
	return chunk
}

func writePiRuntimeSSE(w http.ResponseWriter, chunks ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// piRuntimeProvider is a scripted OpenAI-compatible chat completions endpoint
// bound to loopback. Request n gets step n; any extra request is rejected with
// a non-retryable error and reported by the fixture.
type piRuntimeProvider struct {
	server     *httptest.Server
	stop       chan struct{}
	mu         sync.Mutex
	steps      []piRuntimeStep
	recorded   []piRuntimeRequest
	unexpected []string
}

func newPiRuntimeProvider(t *testing.T, steps []piRuntimeStep) *piRuntimeProvider {
	t.Helper()
	provider := &piRuntimeProvider{stop: make(chan struct{}), steps: steps}
	provider.server = httptest.NewServer(provider)
	t.Cleanup(provider.server.Close)
	// Release stalled handlers before Close waits for them.
	t.Cleanup(func() { close(provider.stop) })
	host, _, err := net.SplitHostPort(provider.server.Listener.Addr().String())
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("mock provider address %s is not loopback (%v)", provider.server.Listener.Addr(), err)
	}
	return provider
}

func (p *piRuntimeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		p.recordUnexpected(r.Method + " " + r.URL.Path)
		http.Error(w, "unexpected endpoint", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		p.recordUnexpected("unreadable request body: " + err.Error())
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	request := piRuntimeRequest{authorization: r.Header.Get("Authorization"), raw: body}
	if err := json.Unmarshal(body, &request.body); err != nil {
		p.recordUnexpected("malformed request body: " + err.Error())
	}
	p.mu.Lock()
	index := len(p.recorded)
	p.recorded = append(p.recorded, request)
	var step piRuntimeStep
	if index < len(p.steps) {
		step = p.steps[index]
	}
	p.mu.Unlock()
	if step == nil {
		p.recordUnexpected(fmt.Sprintf("request %d beyond the %d scripted steps", index, len(p.steps)))
		piRuntimeFailure(http.StatusBadRequest, "unexpected extra provider request")(w, r, p.stop)
		return
	}
	step(w, r, p.stop)
}

func (p *piRuntimeProvider) recordUnexpected(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unexpected = append(p.unexpected, message)
}

func (p *piRuntimeProvider) requests() []piRuntimeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]piRuntimeRequest(nil), p.recorded...)
}

func (p *piRuntimeProvider) unexpectedRequests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.unexpected...)
}

type piRuntimeRequest struct {
	authorization string
	raw           []byte
	body          struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
}

func (r piRuntimeRequest) toolNames() []string {
	names := make([]string, 0, len(r.body.Tools))
	for _, tool := range r.body.Tools {
		names = append(names, tool.Function.Name)
	}
	slices.Sort(names)
	return names
}

func (r piRuntimeRequest) systemText() string {
	var out strings.Builder
	for _, message := range r.body.Messages {
		if message.Role == "system" || message.Role == "developer" {
			out.WriteString(piRuntimeContentText(message.Content))
		}
	}
	return out.String()
}

func (r piRuntimeRequest) toolResult(id string) (string, bool) {
	for _, message := range r.body.Messages {
		if message.Role == "tool" && message.ToolCallID == id {
			return piRuntimeContentText(message.Content), true
		}
	}
	return "", false
}

func piRuntimeContentText(content json.RawMessage) string {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

func assertPiRuntimeToolResult(t *testing.T, request piRuntimeRequest, id, want string) {
	t.Helper()
	got, ok := request.toolResult(id)
	if !ok {
		t.Fatalf("provider request has no tool result for %s", id)
	}
	if !strings.Contains(got, want) {
		t.Fatalf("tool result %s = %q, want it to contain %q", id, got, want)
	}
}

func assertPiRuntimeUsage(t *testing.T, usage Usage, tokensIn, tokensOut, cacheRead, cacheCreate int, costUSD float64) {
	t.Helper()
	got := fmt.Sprintf("in=%s out=%s cacheRead=%s cacheCreate=%s cacheCreate1h=%s cost=%s",
		piRuntimeIntField(usage.TokensIn), piRuntimeIntField(usage.TokensOut), piRuntimeIntField(usage.CacheRead),
		piRuntimeIntField(usage.CacheCreate), piRuntimeIntField(usage.CacheCreate1h), piRuntimeFloatField(usage.CostUSD))
	want := fmt.Sprintf("in=%d out=%d cacheRead=%d cacheCreate=%d cacheCreate1h=nil cost=%g",
		tokensIn, tokensOut, cacheRead, cacheCreate, costUSD)
	if got != want {
		t.Fatalf("usage = %s, want %s", got, want)
	}
}

func piRuntimeIntField(value *int) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprint(*value)
}

func piRuntimeFloatField(value *float64) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprintf("%g", *value)
}

func assertPiRuntimeDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s still holds %d entries after the run, want adapter cleanup", dir, len(entries))
	}
}

func mustPiRuntimeJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}
