package llmadapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type trackedAfterFuncContext struct {
	done   chan struct{}
	active int
}

func (c *trackedAfterFuncContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *trackedAfterFuncContext) Done() <-chan struct{}       { return c.done }
func (c *trackedAfterFuncContext) Err() error                  { return nil }
func (c *trackedAfterFuncContext) Value(any) any               { return nil }

func (c *trackedAfterFuncContext) AfterFunc(func()) func() bool {
	c.active++
	stopped := false
	return func() bool {
		if stopped {
			return false
		}
		stopped = true
		c.active--
		return true
	}
}

func TestLaunchProcessReleasesTimeoutContextOnStartError(t *testing.T) {
	ctx := &trackedAfterFuncContext{done: make(chan struct{})}
	cleanups := 0
	_, err := launchProcess(ctx, "cr-command-that-does-not-exist", nil, "", nil, time.Hour, "", func() error {
		cleanups++
		return nil
	}, false)
	if err == nil {
		t.Fatal("launchProcess error = nil, want command start failure")
	}
	if ctx.active != 0 {
		t.Fatalf("active parent context registrations = %d, want 0", ctx.active)
	}
	if cleanups != 1 {
		t.Fatalf("cleanups = %d, want 1", cleanups)
	}
}

func TestSubprocessClaudeBackgroundLaunchSafety(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	configDir := filepath.Join(tempDir, "claude")
	logPath := filepath.Join(tempDir, "events.log")
	adapter := newClaudeHelperAdapter("success", recordPath, configDir, 5*time.Second)

	stream, err := adapter.Start(context.Background(), Request{
		Model:   "claude-sonnet-4-6",
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
	if response.Usage != (Usage{}) {
		t.Fatalf("Usage = %#v, want empty for Claude bg result-file mode", response.Usage)
	}

	records := readHelperRecords(t, recordPath)
	record := records[0]
	if !containsFlag(record.AdapterArgs, "--bg") {
		t.Fatalf("args = %#v, want --bg", record.AdapterArgs)
	}
	assertFlagValue(t, record.AdapterArgs, "--tools", "Read,Write")
	assertFlagValue(t, record.AdapterArgs, "--permission-mode", "auto")
	assertFlagValue(t, record.AdapterArgs, "--model", "claude-sonnet-4-6")
	assertFlagValue(t, record.AdapterArgs, "--effort", "high")
	if containsFlag(record.AdapterArgs, "--settings") {
		t.Fatalf("args = %#v, want no --settings without fast request", record.AdapterArgs)
	}
	if addDir := flagValue(record.AdapterArgs, "--add-dir"); !samePath(t, addDir, record.Cwd) {
		if !samePath(t, addDir, record.AddDir) {
			t.Fatalf("--add-dir = %q, recorded add-dir = %q", addDir, record.AddDir)
		}
	} else {
		t.Fatalf("--add-dir = %q, cwd = %q, want separate result scratch", addDir, record.Cwd)
	}
	if record.Cwd == "" || record.Cwd == repoRootForTest(t) {
		t.Fatalf("cwd = %q, want adapter workdir outside repo", record.Cwd)
	}
	if wantWorkDir := filepath.Join(filepath.Dir(recordPath), "claude-bg-workdir"); !samePath(t, record.Cwd, wantWorkDir) {
		t.Fatalf("cwd = %q, want configured Claude bg workdir %q", record.Cwd, wantWorkDir)
	}
	if record.CwdEntries != 0 {
		t.Fatalf("cwd entries = %d, want empty adapter workdir before launch", record.CwdEntries)
	}
	if record.AddDirEntries != 1 {
		t.Fatalf("add-dir entries = %d, want prompt file before launch", record.AddDirEntries)
	}
	if record.StdinBytes != 0 {
		t.Fatalf("stdin bytes = %d, want empty stdin for Claude bg positional prompt", record.StdinBytes)
	}
	if record.PromptFileBytes == 0 || !strings.Contains(record.PromptFile, claudeBGResultFilename) || !strings.Contains(record.PromptFile, "prompt") {
		t.Fatalf("prompt file = %q, want prompt plus result-file contract", record.PromptFile)
	}
	checkedArgs := argsBeforePrompt(record.AdapterArgs)
	if len(checkedArgs)+2 != len(record.AdapterArgs) || record.AdapterArgs[len(checkedArgs)] != "--" {
		t.Fatalf("args = %#v, want -- separated positional prompt", record.AdapterArgs)
	}
	if promptArg := record.AdapterArgs[len(checkedArgs)+1]; !strings.Contains(promptArg, claudeBGPromptFilename) || !strings.Contains(promptArg, claudeBGResultFilename) {
		t.Fatalf("positional prompt = %q, want prompt/result file paths", promptArg)
	}
	assertClaudeCleanup(t, records, "job-1", false, configDir)
	if _, err := os.Stat(record.AddDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir exists after cleanup: stat err = %v", err)
	}

	// #nosec G304 -- test reads the log path it created with t.TempDir.
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if !strings.Contains(string(logged), "claude attach job-1") {
		t.Fatalf("log = %q, want launch output", logged)
	}
}

func TestSubprocessClaudeFastRequestUsesSessionSetting(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Command:           os.Args[0],
		commandArgsPrefix: helperPrefix(),
		Env:               helperClaudeEnv("success", recordPath, filepath.Join(tempDir, "claude")),
		Timeout:           5 * time.Second,
		FastModeModels:    []string{"claude-opus-4-8"},
	})
	stream, err := adapter.Start(context.Background(), Request{Model: "claude-opus-4-8", Prompt: "prompt", Fast: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	assertFlagValue(t, readHelperRecord(t, recordPath).AdapterArgs, "--settings", `{"fastMode":true}`)
}

func TestSubprocessClaudeBackgroundResume(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	configDir := filepath.Join(tempDir, "claude")
	adapter := newClaudeHelperAdapter("success", recordPath, configDir, 5*time.Second)

	stream, err := adapter.Resume(context.Background(), "prior-session", Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high",
		Prompt: "resume prompt",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if stream.SessionID() != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", stream.SessionID())
	}
	records := readHelperRecords(t, recordPath)
	assertClaudeCleanup(t, records, "job-1", false, configDir)
	record := readHelperRecord(t, recordPath)
	assertFlagValue(t, record.AdapterArgs, "--resume", "prior-session")
	assertFlagValue(t, record.AdapterArgs, "--model", "claude-sonnet-4-6")
	assertFlagValue(t, record.AdapterArgs, "--effort", "high")
	if record.StdinBytes != 0 {
		t.Fatalf("stdin bytes = %d, want empty stdin for resumed Claude bg prompt", record.StdinBytes)
	}
	if record.PromptFileBytes == 0 || !strings.Contains(record.PromptFile, "resume prompt") {
		t.Fatalf("prompt file = %q, want resumed prompt", record.PromptFile)
	}
}

func TestSubprocessReviewerWorkspaceModes(t *testing.T) {
	claude := NewClaudeCLIAdapter(SubprocessOptions{})
	if got := AdapterReviewerWorkspaceMode(claude); got != ReviewerWorkspacePermissionBounded {
		t.Fatalf("Claude reviewer workspace mode = %s, want %s", got, ReviewerWorkspacePermissionBounded)
	}
	if !SupportsReviewerWorkspace(claude) {
		t.Fatal("Claude SupportsReviewerWorkspace = false, want true")
	}

	codex := NewCodexCLIAdapter(SubprocessOptions{AllowBestEffortNoTools: true})
	if got := AdapterReviewerWorkspaceMode(codex); got != ReviewerWorkspaceWrite {
		t.Fatalf("Codex reviewer workspace mode = %s, want %s", got, ReviewerWorkspaceWrite)
	}
	if !SupportsReviewerWorkspace(codex) {
		t.Fatal("Codex SupportsReviewerWorkspace = false, want true")
	}
}

func TestSubprocessClaudeReviewerWorkspaceLaunch(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	configDir := filepath.Join(tempDir, "claude")
	scratchRoot := filepath.Join(tempDir, "workbench-scratch")
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(scratchRoot): %v", err)
	}
	repoRoot := filepath.Join(tempDir, "workbench-repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(repoRoot): %v", err)
	}
	adapter := newClaudeHelperAdapterWithEnv("success", recordPath, configDir, 5*time.Second, "CGO_CFLAGS=-O2", "CGO_CXXFLAGS=-stdlib=libc++")

	stream, err := adapter.Start(context.Background(), Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high",
		Prompt: "prompt",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir:            repoRoot,
			ScratchDir:         scratchRoot,
			MaxToolOutputBytes: 2048,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	record := readHelperRecord(t, recordPath)
	addDirs := flagValues(record.AdapterArgs, "--add-dir")
	if len(addDirs) != 2 {
		t.Fatalf("--add-dir values = %#v, want scratch invocation dir and repo root", addDirs)
	}
	if !containsSamePath(addDirs, record.AddDir) || !containsSamePath(addDirs, repoRoot) {
		t.Fatalf("--add-dir values = %#v, want scratch %q and repo %q", addDirs, record.AddDir, repoRoot)
	}
	if !samePath(t, record.Cwd, repoRoot) {
		t.Fatalf("cwd = %q, want reviewer workspace repo %q", record.Cwd, repoRoot)
	}
	if tools := flagValue(record.AdapterArgs, "--tools"); tools != "Read,Write,Bash" {
		t.Fatalf("--tools = %q, want reviewer workspace tools", tools)
	}
	cwd := strings.TrimPrefix(filepath.Clean(record.AddDir), "/private")
	wantRoot := strings.TrimPrefix(filepath.Clean(scratchRoot), "/private")
	if !strings.HasPrefix(cwd, wantRoot+string(filepath.Separator)) {
		t.Fatalf("scratch add-dir = %q, want child under %q", record.AddDir, scratchRoot)
	}
	if record.PromptFileBytes == 0 || !strings.Contains(record.PromptFile, "prompt") {
		t.Fatalf("prompt file = %q, want prompt in invocation scratch", record.PromptFile)
	}
	assertReviewerToolchainEnv(t, record, repoRoot, addDirs, "-O2", "-stdlib=libc++")
}

func TestReviewerInvocationEnvCreatesWritableToolchainDirs(t *testing.T) {
	scratch := t.TempDir()
	env, err := reviewerInvocationEnv([]string{"CGO_CFLAGS=-O2", "CGO_CXXFLAGS=-stdlib=libc++"}, scratch)
	if err != nil {
		t.Fatalf("reviewerInvocationEnv: %v", err)
	}
	clangCache := filepath.Join(scratch, "cache", "clang-module-cache")
	for _, dir := range []string{
		envValue(env, "TMPDIR"),
		envValue(env, "GOTMPDIR"),
		envValue(env, "GOCACHE"),
		envValue(env, "XDG_CACHE_HOME"),
		clangCache,
	} {
		if err := os.WriteFile(filepath.Join(dir, "writable"), []byte("ok"), 0o600); err != nil {
			t.Fatalf("toolchain dir %q is not writable: %v", dir, err)
		}
	}
	clangFlag := "-fmodules-cache-path=" + clangCache
	if got := envValue(env, "CGO_CFLAGS"); got != "-O2 "+clangFlag {
		t.Fatalf("CGO_CFLAGS = %q, want preserved user flag plus %q", got, clangFlag)
	}
	if got := envValue(env, "CGO_CXXFLAGS"); got != "-stdlib=libc++ "+clangFlag {
		t.Fatalf("CGO_CXXFLAGS = %q, want preserved user flag plus %q", got, clangFlag)
	}
}

func TestSubprocessClaudeReviewerWorkspaceValidationRejectsUnexpectedAddDir(t *testing.T) {
	tempDir := t.TempDir()
	scratch := filepath.Join(tempDir, "scratch")
	repoRoot := filepath.Join(tempDir, "repo")
	extra := filepath.Join(tempDir, "extra")
	for _, dir := range []string{scratch, repoRoot, extra} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	adapter := NewClaudeCLIAdapter(SubprocessOptions{})
	req := Request{
		Prompt: "prompt",
		ReviewerWorkspace: &ReviewerWorkspaceRequest{
			RepoDir:    repoRoot,
			ScratchDir: scratch,
		},
	}
	args, err := adapter.buildArgs(req, scratch)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	args = append(argsBeforePrompt(args), "--add-dir", extra, "--", "prompt")
	if err := adapter.validateArgs(args, scratch, req); !errors.Is(err, ErrUnsafeSubprocessConfig) || !strings.Contains(err.Error(), "add-dir") {
		t.Fatalf("validateArgs unexpected add-dir error = %v, want unsafe add-dir", err)
	}

	args, err = adapter.buildArgs(req, scratch)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	args = removeFlag(args, "--add-dir")
	if err := adapter.validateArgs(args, scratch, req); !errors.Is(err, ErrUnsafeSubprocessConfig) || !strings.Contains(err.Error(), "add-dir") {
		t.Fatalf("validateArgs missing add-dir error = %v, want unsafe add-dir", err)
	}
}

func TestSubprocessClaudeBackgroundStatesAndCleanup(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mode          string
		wantOutput    string
		wantSession   string
		wantErr       string
		wantErrIs     error
		wantStop      bool
		timeout       time.Duration
		wantRawResult bool
	}{
		{name: "idle result", mode: "bg-idle-result", wantOutput: `{"idle":true}`, wantSession: "session-idle"},
		{name: "invalid json is returned raw", mode: "bg-invalid-json", wantOutput: `not-json`, wantSession: "session-invalid", wantRawResult: true},
		{name: "blocked", mode: "bg-blocked", wantErr: "blocked: permission needed", wantStop: true},
		{name: "failed", mode: "bg-failed", wantErr: "failed: model failed", wantStop: true},
		{name: "waiting", mode: "bg-waiting", wantErr: "waiting: waiting for input", wantStop: true},
		{name: "stopped", mode: "bg-stopped", wantErr: "stopped: stopped by user", wantStop: true},
		{name: "stop fails still removes", mode: "bg-stop-fails", wantErr: "blocked: stop will fail", wantStop: true},
		{name: "missing result", mode: "bg-missing-result", wantErr: "completed without writing result file", wantSession: "session-missing", wantStop: true},
		{name: "empty result", mode: "bg-empty-result", wantErr: "empty result file", wantSession: "session-empty", wantStop: true},
		{name: "timeout", mode: "bg-running", wantErrIs: context.DeadlineExceeded, wantStop: true, timeout: 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			recordPath := filepath.Join(tempDir, "records.jsonl")
			configDir := filepath.Join(tempDir, "claude")
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			adapter := newClaudeHelperAdapter(tt.mode, recordPath, configDir, timeout)

			stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			response, err := stream.Wait(context.Background())
			if tt.wantErr != "" || tt.wantErrIs != nil {
				if tt.wantErrIs != nil {
					if !errors.Is(err, tt.wantErrIs) {
						t.Fatalf("Wait error = %v, want errors.Is %v", err, tt.wantErrIs)
					}
				} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Wait error = %v, want containing %q", err, tt.wantErr)
				}
				if tt.wantSession != "" && stream.SessionID() != tt.wantSession {
					t.Fatalf("SessionID = %q, want %q", stream.SessionID(), tt.wantSession)
				}
				assertClaudeCleanup(t, readHelperRecords(t, recordPath), "job-1", tt.wantStop, configDir)
				return
			}
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if string(response.StructuredOutput) != tt.wantOutput {
				t.Fatalf("StructuredOutput = %q, want %q", response.StructuredOutput, tt.wantOutput)
			}
			if !tt.wantRawResult {
				var decoded map[string]any
				if err := json.Unmarshal(response.StructuredOutput, &decoded); err != nil {
					t.Fatalf("StructuredOutput JSON: %v", err)
				}
			}
			if stream.SessionID() != tt.wantSession {
				t.Fatalf("SessionID = %q, want %q", stream.SessionID(), tt.wantSession)
			}
			assertClaudeCleanup(t, readHelperRecords(t, recordPath), "job-1", false, configDir)
		})
	}
}

func TestSubprocessClaudeStaleJobGC(t *testing.T) {
	t.Run("owned stale job is removed", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		configDir := filepath.Join(tempDir, "claude")
		workDir := helperClaudeWorkDir(recordPath)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatalf("MkdirAll(workDir): %v", err)
		}
		adapter := newClaudeHelperAdapter("success", recordPath, configDir, 5*time.Second)
		writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-stale", "state.json"), map[string]any{"cwd": workDir}, time.Now().Add(-25*time.Hour))

		if err := adapter.cleanupClaudeBGJob(context.Background(), "job-1", nil, workDir); err != nil {
			t.Fatalf("cleanupClaudeBGJob: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-stale")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("job-stale exists after cleanup: stat err = %v", err)
		}
		if !hasClaudeControlRecord(readHelperRecords(t, recordPath), "rm", "job-stale") {
			t.Fatal("missing rm for stale owned job")
		}
	})

	t.Run("owned fresh job is left alone", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		configDir := filepath.Join(tempDir, "claude")
		workDir := helperClaudeWorkDir(recordPath)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatalf("MkdirAll(workDir): %v", err)
		}
		adapter := newClaudeHelperAdapter("success", recordPath, configDir, 5*time.Second)
		writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-fresh", "state.json"), map[string]any{"cwd": workDir}, time.Now().Add(-(claudeBGStaleJobAge - time.Minute)))

		if err := adapter.cleanupClaudeBGJob(context.Background(), "job-1", nil, workDir); err != nil {
			t.Fatalf("cleanupClaudeBGJob: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-fresh")); err != nil {
			t.Fatalf("job-fresh missing after cleanup: %v", err)
		}
		if hasClaudeControlRecord(readHelperRecords(t, recordPath), "rm", "job-fresh") {
			t.Fatal("unexpected rm for fresh owned job")
		}
	})

	t.Run("unrelated stale job is left alone", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		configDir := filepath.Join(tempDir, "claude")
		workDir := helperClaudeWorkDir(recordPath)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatalf("MkdirAll(workDir): %v", err)
		}
		adapter := newClaudeHelperAdapter("success", recordPath, configDir, 5*time.Second)
		writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-unrelated", "state.json"), map[string]any{
			"cwd":          filepath.Join(tempDir, "elsewhere"),
			"intent":       "plain prompt",
			"respawnFlags": []any{"--model", "claude-sonnet-4-6"},
		}, time.Now().Add(-25*time.Hour))

		if err := adapter.cleanupClaudeBGJob(context.Background(), "job-1", nil, workDir); err != nil {
			t.Fatalf("cleanupClaudeBGJob: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-unrelated")); err != nil {
			t.Fatalf("job-unrelated missing after cleanup: %v", err)
		}
		if hasClaudeControlRecord(readHelperRecords(t, recordPath), "rm", "job-unrelated") {
			t.Fatal("unexpected rm for unrelated stale job")
		}
	})

	t.Run("active stale job is skipped", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		configDir := filepath.Join(tempDir, "claude")
		workDir := helperClaudeWorkDir(recordPath)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatalf("MkdirAll(workDir): %v", err)
		}
		adapter := newClaudeHelperAdapterWithEnv("success", recordPath, configDir, 5*time.Second,
			`LLM_HELPER_CLAUDE_AGENTS_JSON=[{"id":"job-active"}]`,
		)
		writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-active", "state.json"), map[string]any{"cwd": workDir}, time.Now().Add(-25*time.Hour))

		if err := adapter.cleanupClaudeBGJob(context.Background(), "job-1", nil, workDir); err != nil {
			t.Fatalf("cleanupClaudeBGJob: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-active")); err != nil {
			t.Fatalf("job-active missing after cleanup: %v", err)
		}
		if hasClaudeControlRecord(readHelperRecords(t, recordPath), "rm", "job-active") {
			t.Fatal("unexpected rm for active stale job")
		}
	})

	t.Run("rm refusal falls back to direct deletion", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		configDir := filepath.Join(tempDir, "claude")
		workDir := helperClaudeWorkDir(recordPath)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatalf("MkdirAll(workDir): %v", err)
		}
		adapter := newClaudeHelperAdapterWithEnv("success", recordPath, configDir, 5*time.Second,
			"LLM_HELPER_CLAUDE_FAIL_RM_IDS=job-stale",
		)
		writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-stale", "state.json"), map[string]any{
			"respawnFlags": []any{"--add-dir", filepath.Join(tempDir, "codereview-llm-stale")},
		}, time.Now().Add(-25*time.Hour))

		if err := adapter.cleanupClaudeBGJob(context.Background(), "job-1", nil, workDir); err != nil {
			t.Fatalf("cleanupClaudeBGJob: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-stale")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("job-stale exists after fallback cleanup: stat err = %v", err)
		}
		if !hasClaudeControlRecord(readHelperRecords(t, recordPath), "rm", "job-stale") {
			t.Fatal("missing rm attempt before stale fallback delete")
		}
	})
}

func TestSubprocessClaudeCleanupWarningsDoNotFailSuccess(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	configDir := filepath.Join(tempDir, "claude")
	logPath := filepath.Join(tempDir, "events.log")
	workDir := helperClaudeWorkDir(recordPath)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(workDir): %v", err)
	}
	writeClaudeHelperStateAt(t, filepath.Join(configDir, "jobs", "job-stale", "state.json"), map[string]any{"cwd": workDir}, time.Now().Add(-25*time.Hour))
	adapter := newClaudeHelperAdapterWithEnv("success", recordPath, configDir, 5*time.Second,
		"LLM_HELPER_CLAUDE_AGENTS_JSON={bad json",
	)

	stream, err := adapter.Start(context.Background(), Request{
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
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %q, want success response", response.StructuredOutput)
	}
	record := readHelperRecord(t, recordPath)
	if _, err := os.Stat(filepath.Join(configDir, "jobs", "job-stale")); err != nil {
		t.Fatalf("job-stale missing after fail-closed agents discovery: %v", err)
	}
	if _, err := os.Stat(record.AddDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir exists after warning-path cleanup: stat err = %v", err)
	}
	logged, err := os.ReadFile(logPath) // #nosec G304 -- test reads the log path it created with t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	logText := string(logged)
	if !strings.Contains(logText, "warning: llm subprocess: Claude bg active-agent discovery failed") {
		t.Fatalf("log = %q, want cleanup warning", logText)
	}
	if !strings.Contains(logText, "malformed JSON") {
		t.Fatalf("log = %q, want malformed JSON detail", logText)
	}
}

func TestParseClaudeBGActiveJobs(t *testing.T) {
	t.Run("recurses through unknown wrapper keys", func(t *testing.T) {
		activeJobs, err := parseClaudeBGActiveJobs([]byte(`{"wrapper":{"sessions":[{"id":"job-active"}]}}`))
		if err != nil {
			t.Fatalf("parseClaudeBGActiveJobs: %v", err)
		}
		if !activeJobs["job-active"] {
			t.Fatalf("activeJobs = %#v, want job-active", activeJobs)
		}
	})

	t.Run("fails closed on non-empty shape without ids", func(t *testing.T) {
		if _, err := parseClaudeBGActiveJobs([]byte(`{"wrapper":{"sessions":[{"state":"running"}]}}`)); err == nil || !strings.Contains(err.Error(), "unrecognized non-empty JSON shape") {
			t.Fatalf("parseClaudeBGActiveJobs error = %v, want unrecognized non-empty JSON shape", err)
		}
	})

	t.Run("fails closed beyond traversal depth", func(t *testing.T) {
		var nested any = map[string]any{"id": "job-too-deep"}
		for i := 0; i < claudeBGInspectMaxDepth+2; i++ {
			nested = map[string]any{"wrapper": nested}
		}
		payload, err := json.Marshal(nested)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if _, err := parseClaudeBGActiveJobs(payload); err == nil || !strings.Contains(err.Error(), "unrecognized non-empty JSON shape") {
			t.Fatalf("parseClaudeBGActiveJobs error = %v, want unrecognized non-empty JSON shape", err)
		}
	})

	t.Run("allows empty arrays", func(t *testing.T) {
		activeJobs, err := parseClaudeBGActiveJobs([]byte(`[]`))
		if err != nil {
			t.Fatalf("parseClaudeBGActiveJobs: %v", err)
		}
		if len(activeJobs) != 0 {
			t.Fatalf("activeJobs = %#v, want empty", activeJobs)
		}
	})
}

func TestSubprocessClaudeWaitsForDelayedIdleSessionID(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "claude")
	scratch := filepath.Join(tempDir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatalf("Mkdir(scratch): %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, claudeBGResultFilename), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile(result): %v", err)
	}
	jobID := "job-delayed"
	statePath := filepath.Join(configDir, "jobs", jobID, "state.json")
	writeClaudeHelperState(t, statePath, map[string]any{
		"state":    "working",
		"tempo":    "idle",
		"inFlight": map[string]any{"tasks": 0, "queued": 0},
	})
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeClaudeHelperState(t, statePath, map[string]any{
			"state":     "working",
			"tempo":     "idle",
			"sessionId": "session-delayed",
			"inFlight":  map[string]any{"tasks": 0, "queued": 0},
		})
	}()

	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Env:     []string{"CLAUDE_CONFIG_DIR=" + configDir},
		Timeout: 5 * time.Second,
	})
	response, sessionID, err := adapter.waitForClaudeBGResult(context.Background(), jobID, scratch)
	if err != nil {
		t.Fatalf("waitForClaudeBGResult: %v", err)
	}
	if sessionID != "session-delayed" || string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("result = %q session = %q, want delayed session", response.StructuredOutput, sessionID)
	}
}

func TestSubprocessClaudePollsUntilTerminalState(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "claude")
	scratch := filepath.Join(tempDir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatalf("Mkdir(scratch): %v", err)
	}
	jobID := "job-transition"
	statePath := filepath.Join(configDir, "jobs", jobID, "state.json")
	writeClaudeHelperState(t, statePath, map[string]any{
		"state":    "working",
		"tempo":    "busy",
		"inFlight": map[string]any{"tasks": 1, "queued": 0},
	})
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(scratch, claudeBGResultFilename), []byte(`{"done":true}`), 0o600); err != nil {
			return
		}
		if err := writeClaudeHelperStateFile(statePath, map[string]any{
			"state":     "done",
			"sessionId": "session-transition",
		}); err != nil {
			return
		}
	}()

	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Env:     []string{"CLAUDE_CONFIG_DIR=" + configDir},
		Timeout: 5 * time.Second,
	})
	response, sessionID, err := adapter.waitForClaudeBGResult(context.Background(), jobID, scratch)
	if err != nil {
		t.Fatalf("waitForClaudeBGResult: %v", err)
	}
	if sessionID != "session-transition" || string(response.StructuredOutput) != `{"done":true}` {
		t.Fatalf("result = %q session = %q, want terminal transition result", response.StructuredOutput, sessionID)
	}
}

func TestSubprocessClaudeDefaultWorkingDirUsesCacheRoot(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("CR_CLAUDE_BG_WORK_DIR", "")
	oldUserCacheDir := claudeBGUserCacheDir
	claudeBGUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { claudeBGUserCacheDir = oldUserCacheDir })

	workDir, err := claudeBGWorkingDir(nil)
	if err != nil {
		t.Fatalf("claudeBGWorkingDir: %v", err)
	}
	if !strings.Contains(workDir, "codereview-cli") || !strings.Contains(workDir, "claude-bg-workdir") {
		t.Fatalf("workDir = %q, want codereview-cli claude bg cache path", workDir)
	}
	info, err := os.Stat(workDir)
	if err != nil {
		t.Fatalf("Stat(workDir): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("workDir is not a directory: %q", workDir)
	}
}

func TestSubprocessCodexSafetyModes(t *testing.T) {
	t.Run("default refuses before launch", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := NewCodexCLIAdapter(SubprocessOptions{
			Command:           os.Args[0],
			commandArgsPrefix: helperPrefix(),
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
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("success", recordPath, 5*time.Second)
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

	t.Run("durable session start omits ephemeral", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("success", recordPath, 5*time.Second)
		stream, err := adapter.Start(context.Background(), Request{
			Model:          "gpt-5.5",
			Effort:         "high",
			Prompt:         "prompt",
			DurableSession: true,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := stream.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		record := readHelperRecord(t, recordPath)
		if containsFlag(record.AdapterArgs, "--ephemeral") {
			t.Fatalf("args = %#v, do not want --ephemeral for durable session", record.AdapterArgs)
		}
	})

	t.Run("resume uses exec resume and keeps prompt delivery", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("success", recordPath, 5*time.Second)
		stream, err := adapter.Resume(context.Background(), "prior-session", Request{
			Model:  "gpt-5.5",
			Effort: "high",
			Prompt: "resume prompt",
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if _, err := stream.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if stream.SessionID() != "session-1" {
			t.Fatalf("SessionID = %q, want session-1", stream.SessionID())
		}
		record := readHelperRecord(t, recordPath)
		if len(record.AdapterArgs) < 2 || record.AdapterArgs[0] != "exec" || record.AdapterArgs[1] != "resume" {
			t.Fatalf("args = %#v, want codex exec resume", record.AdapterArgs)
		}
		for _, flag := range []string{"--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"} {
			if !containsFlag(record.AdapterArgs, flag) {
				t.Fatalf("args = %#v, want %s", record.AdapterArgs, flag)
			}
		}
		for _, flag := range []string{"--sandbox", "--cd", "--add-dir"} {
			if containsFlag(record.AdapterArgs, flag) {
				t.Fatalf("args = %#v, do not want unsupported resume flag %s", record.AdapterArgs, flag)
			}
		}
		assertFlagValue(t, record.AdapterArgs, "--model", "gpt-5.5")
		assertFlagValue(t, record.AdapterArgs, "-c", "model_reasoning_effort=high")
		promptIndex := len(argsBeforePrompt(record.AdapterArgs))
		if promptIndex+2 != len(record.AdapterArgs) || record.AdapterArgs[promptIndex] != "--" || record.AdapterArgs[promptIndex+1] != "resume prompt" {
			t.Fatalf("args = %#v, want -- separated resumed prompt", record.AdapterArgs)
		}
		if got := record.AdapterArgs[promptIndex-1]; got != "prior-session" {
			t.Fatalf("resume session arg = %q in %#v, want prior-session immediately before prompt separator", got, record.AdapterArgs)
		}
	})

	t.Run("resume reviewer workspace is rejected", func(t *testing.T) {
		tempDir := t.TempDir()
		scratchRoot := filepath.Join(tempDir, "workbench-scratch")
		if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(scratchRoot): %v", err)
		}
		repoRoot := filepath.Join(tempDir, "workbench-repo")
		if err := os.MkdirAll(repoRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(repoRoot): %v", err)
		}
		adapter := NewCodexCLIAdapter(SubprocessOptions{AllowBestEffortNoTools: true})
		_, err := adapter.Resume(context.Background(), "prior-session", Request{
			Model:  "gpt-5.5",
			Effort: "high",
			Prompt: "resume prompt",
			ReviewerWorkspace: &ReviewerWorkspaceRequest{
				RepoDir:    repoRoot,
				ScratchDir: scratchRoot,
			},
		})
		if !errors.Is(err, ErrUnsafeSubprocessConfig) {
			t.Fatalf("Resume error = %v, want ErrUnsafeSubprocessConfig", err)
		}
	})

	t.Run("reviewer workspace toolchain env stays under scratch add-dir", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		scratchRoot := filepath.Join(tempDir, "workbench-scratch")
		if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(scratchRoot): %v", err)
		}
		repoRoot := filepath.Join(tempDir, "workbench-repo")
		if err := os.MkdirAll(repoRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(repoRoot): %v", err)
		}
		tempRoot := filepath.Join(scratchRoot, "tmp")
		cacheRoot := filepath.Join(scratchRoot, "cache")
		goCache := filepath.Join(cacheRoot, "go-build")
		goTmp := filepath.Join(tempRoot, "go")
		xdgCache := filepath.Join(cacheRoot, "xdg")
		adapter := newCodexHelperAdapter("success", recordPath, 5*time.Second, "CGO_CFLAGS=-O2", "CGO_CXXFLAGS=-stdlib=libc++")
		stream, err := adapter.Start(context.Background(), Request{
			Model:  "gpt-5.5",
			Effort: "high",
			Prompt: "prompt",
			ReviewerWorkspace: &ReviewerWorkspaceRequest{
				RepoDir:            repoRoot,
				ScratchDir:         scratchRoot,
				Env:                []string{"TMPDIR=" + tempRoot, "TMP=" + tempRoot, "TEMP=" + tempRoot, "GOCACHE=" + goCache, "GOTMPDIR=" + goTmp, "XDG_CACHE_HOME=" + xdgCache},
				MaxToolOutputBytes: 2048,
			},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := stream.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		record := readHelperRecord(t, recordPath)
		assertFlagValue(t, record.AdapterArgs, "--sandbox", "workspace-write")
		assertFlagValue(t, record.AdapterArgs, "--add-dir", record.AddDir)
		if cd := flagValue(record.AdapterArgs, "--cd"); !samePath(t, cd, repoRoot) {
			t.Fatalf("--cd = %q, want repo %q", cd, repoRoot)
		}
		scratch := strings.TrimPrefix(filepath.Clean(record.AddDir), "/private")
		wantRoot := strings.TrimPrefix(filepath.Clean(scratchRoot), "/private")
		if !strings.HasPrefix(scratch, wantRoot+string(filepath.Separator)) {
			t.Fatalf("--add-dir = %q, want invocation scratch under %q", record.AddDir, scratchRoot)
		}
		assertReviewerToolchainEnv(t, record, repoRoot, []string{record.AddDir}, "-O2", "-stdlib=libc++")
	})

	t.Run("reviewer workspace caps subprocess logs", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		logPath := filepath.Join(tempDir, "events.log")
		scratchRoot := filepath.Join(tempDir, "workbench-scratch")
		if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(scratchRoot): %v", err)
		}
		repoRoot := filepath.Join(tempDir, "workbench-repo")
		if err := os.MkdirAll(repoRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(repoRoot): %v", err)
		}
		adapter := newCodexHelperAdapter("noisy-success", recordPath, 5*time.Second)
		stream, err := adapter.Start(context.Background(), Request{
			Prompt:  "prompt",
			LogPath: logPath,
			ReviewerWorkspace: &ReviewerWorkspaceRequest{
				RepoDir:            repoRoot,
				ScratchDir:         scratchRoot,
				MaxToolOutputBytes: 64,
			},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := stream.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		logged, err := os.ReadFile(logPath) // #nosec G304 -- test reads the log path it created with t.TempDir.
		if err != nil {
			t.Fatalf("ReadFile(log): %v", err)
		}
		logText := string(logged)
		warningText := "warning: reviewer workspace tool output cap reached; further subprocess logs truncated\n"
		if !strings.Contains(logText, warningText) {
			t.Fatalf("log = %q, want truncation warning", logText)
		}
		if len(logged) > 64+len(warningText) {
			t.Fatalf("log bytes = %d, want capped output at %d plus warning", len(logged), 64+len(warningText))
		}
	})
}

func TestSubprocessCodexParsesObservedUsageFields(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "records.jsonl")
	adapter := newCodexHelperAdapter("codex-usage", recordPath, 5*time.Second)
	stream, err := adapter.Start(context.Background(), Request{Model: "gpt-5.5", Effort: "high", Prompt: "prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want ok JSON", response.StructuredOutput)
	}
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 25475 ||
		response.Usage.TokensOut == nil || *response.Usage.TokensOut != 812 ||
		response.Usage.CacheRead == nil || *response.Usage.CacheRead != 19712 {
		t.Fatalf("Usage = %#v, want observed Codex usage fields", response.Usage)
	}
	if response.Usage.CacheCreate != nil {
		t.Fatalf("CacheCreate = %v, want nil when Codex event does not report cache creation", *response.Usage.CacheCreate)
	}
}

func TestSubprocessParseUsageAliases(t *testing.T) {
	raw := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"tokensIn":1,"tokensOut":2,"cacheRead":3,"cacheWrite":4}`),
	}
	usage := parseUsage(raw)
	if usage.TokensIn == nil || *usage.TokensIn != 1 ||
		usage.TokensOut == nil || *usage.TokensOut != 2 ||
		usage.CacheRead == nil || *usage.CacheRead != 3 ||
		usage.CacheCreate == nil || *usage.CacheCreate != 4 {
		t.Fatalf("Usage = %#v, want camelCase aliases parsed", usage)
	}

	raw = map[string]json.RawMessage{
		"usage": json.RawMessage(`{"prompt_tokens":5,"completion_tokens":6,"cachedInputTokens":7,"cache_create":8}`),
	}
	usage = parseUsage(raw)
	if usage.TokensIn == nil || *usage.TokensIn != 5 ||
		usage.TokensOut == nil || *usage.TokensOut != 6 ||
		usage.CacheRead == nil || *usage.CacheRead != 7 ||
		usage.CacheCreate == nil || *usage.CacheCreate != 8 {
		t.Fatalf("Usage = %#v, want prompt/completion/cache aliases parsed", usage)
	}
}

func TestSubprocessCodexToolUseAndProtocolFailures(t *testing.T) {
	t.Run("tool use kills and fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("tool", recordPath, 5*time.Second)
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
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("sleep", recordPath, 20*time.Millisecond)
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
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("malformed", recordPath, 5*time.Second)
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

	t.Run("nested tool use fails stream", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "records.jsonl")
		adapter := newCodexHelperAdapter("nested-tool", recordPath, 5*time.Second)
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, ErrToolUse) {
			t.Fatalf("Wait error = %v, want ErrToolUse", err)
		}
	})
}

func TestSubprocessRejectsUnsafeSpecs(t *testing.T) {
	claude := NewClaudeCLIAdapter(SubprocessOptions{})
	scratch := t.TempDir()
	claudeArgs, err := claude.buildArgs(Request{Prompt: "prompt"}, scratch)
	if err != nil {
		t.Fatalf("buildArgs(claude): %v", err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "search", args: append([]string{"--search"}, claudeArgs...)},
		{name: "bad add dir", args: replaceSubprocessFlagValue(claudeArgs, "--add-dir", t.TempDir())},
		{name: "danger sandbox", args: append([]string{"--sandbox", "danger-full-access"}, claudeArgs...)},
		{name: "missing write tool", args: removeFlagPair(claudeArgs, "--tools")},
		{name: "unexpected flag", args: append([]string{"--unexpected"}, claudeArgs...)},
		{name: "prompt argv", args: append(append([]string(nil), claudeArgs...), "--", "prompt")},
	} {
		t.Run("claude "+tt.name, func(t *testing.T) {
			if err := claude.validateArgs(tt.args, scratch, Request{Prompt: "prompt"}); !errors.Is(err, ErrUnsafeSubprocessConfig) {
				t.Fatalf("validateArgs error = %v, want ErrUnsafeSubprocessConfig", err)
			}
		})
	}
	inlineClaudeArgs := replaceSubprocessFlagPairWithEquals(claudeArgs, "--tools")
	inlineClaudeArgs = replaceSubprocessFlagPairWithEquals(inlineClaudeArgs, "--permission-mode")
	inlineClaudeArgs = replaceSubprocessFlagPairWithEquals(inlineClaudeArgs, "--add-dir")
	if err := claude.validateArgs(inlineClaudeArgs, scratch, Request{Prompt: "prompt"}); err != nil {
		t.Fatalf("validateArgs(claude inline flags): %v", err)
	}

	codex := NewCodexCLIAdapter(SubprocessOptions{AllowBestEffortNoTools: true})
	codexScratch := t.TempDir()
	codexArgs, err := codex.buildArgs(Request{Prompt: "prompt"}, codexScratch)
	if err != nil {
		t.Fatalf("buildArgs(codex): %v", err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "search", args: append([]string{"--search"}, codexArgs...)},
		{name: "add dir", args: append([]string{"--add-dir", t.TempDir()}, codexArgs...)},
	} {
		t.Run("codex "+tt.name, func(t *testing.T) {
			if err := codex.validateArgs(tt.args, codexScratch, Request{Prompt: "prompt"}); !errors.Is(err, ErrUnsafeSubprocessConfig) {
				t.Fatalf("validateArgs error = %v, want ErrUnsafeSubprocessConfig", err)
			}
		})
	}

	recordPath := filepath.Join(t.TempDir(), "records.jsonl")
	nonEmptyScratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(nonEmptyScratch, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyScratch, "file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Command:           os.Args[0],
		commandArgsPrefix: helperPrefix(),
		Env:               helperClaudeEnv("success", recordPath, filepath.Join(t.TempDir(), "claude")),
		ScratchDirFactory: func() (string, func() error, error) {
			return nonEmptyScratch, func() error { return nil }, nil
		},
	})
	_, err = adapter.Start(context.Background(), Request{Prompt: "prompt"})
	if !errors.Is(err, ErrUnsafeSubprocessConfig) {
		t.Fatalf("Start error = %v, want ErrUnsafeSubprocessConfig", err)
	}
	if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("helper record exists after refused non-empty scratch launch: %v", statErr)
	}
}

func TestSubprocessProcessGroupCleanup(t *testing.T) {
	t.Run("tool use kills process group", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		childPIDPath := filepath.Join(tempDir, "child.pid")
		adapter := NewCodexCLIAdapter(SubprocessOptions{
			Command:                os.Args[0],
			commandArgsPrefix:      helperPrefix(),
			Env:                    append(helperEnv("spawn-child-tool", recordPath), "LLM_HELPER_CHILD_PID="+childPIDPath),
			Timeout:                5 * time.Second,
			AllowBestEffortNoTools: true,
		})
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, ErrToolUse) {
			t.Fatalf("Wait error = %v, want ErrToolUse", err)
		}
		pid := readPID(t, childPIDPath)
		eventually(t, 2*time.Second, func() bool {
			return !processExists(pid)
		})
	})

	t.Run("timeout kills process group", func(t *testing.T) {
		tempDir := t.TempDir()
		recordPath := filepath.Join(tempDir, "records.jsonl")
		childPIDPath := filepath.Join(tempDir, "child.pid")
		adapter := NewCodexCLIAdapter(SubprocessOptions{
			Command:                os.Args[0],
			commandArgsPrefix:      helperPrefix(),
			Env:                    append(helperEnv("spawn-child-sleep", recordPath), "LLM_HELPER_CHILD_PID="+childPIDPath),
			Timeout:                200 * time.Millisecond,
			AllowBestEffortNoTools: true,
		})
		stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait error = %v, want deadline exceeded", err)
		}
		pid := readPID(t, childPIDPath)
		eventually(t, 2*time.Second, func() bool {
			return !processExists(pid)
		})
	})
}

func TestSubprocessResumeSupport(t *testing.T) {
	claude := NewClaudeCLIAdapter(SubprocessOptions{})
	if !claude.SupportsResume() {
		t.Fatal("Claude SupportsResume = false, want true")
	}
	codex := NewCodexCLIAdapter(SubprocessOptions{AllowBestEffortNoTools: true})
	if !codex.SupportsResume() {
		t.Fatal("Codex SupportsResume = false, want true")
	}
}

func TestSubprocessCodexResumeRetainsSafetyGuard(t *testing.T) {
	adapter := NewCodexCLIAdapter(SubprocessOptions{})
	if _, err := adapter.Resume(context.Background(), "prior-session", Request{Prompt: "prompt"}); !errors.Is(err, ErrUnsafeSubprocessConfig) {
		t.Fatalf("Resume error = %v, want ErrUnsafeSubprocessConfig", err)
	}
}

func TestSubprocessToolUseDetectionIsPreciseAndBounded(t *testing.T) {
	event, err := parseSubprocessEvent([]byte(`{"type":"message","delta":{"tool_use":0,"tool_call":false,"function_call":""},"structured_output":{"ok":true}}`))
	if err != nil {
		t.Fatalf("parseSubprocessEvent(false values): %v", err)
	}
	if event.toolUse {
		t.Fatal("toolUse = true for falsey tool-use fields")
	}

	deepPayload := `{"type":"message","payload":` +
		strings.Repeat(`{"payload":`, maxToolUseScanDepth+8) +
		`{"type":"tool_use"}` +
		strings.Repeat(`}`, maxToolUseScanDepth+8) +
		`}`
	event, err = parseSubprocessEvent([]byte(deepPayload))
	if err != nil {
		t.Fatalf("parseSubprocessEvent(deep payload): %v", err)
	}
	if event.toolUse {
		t.Fatal("toolUse = true beyond max scan depth")
	}
}

func TestParseSubprocessEventExtractsAgentMessageText(t *testing.T) {
	event, err := parseSubprocessEvent([]byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`))
	if err != nil {
		t.Fatalf("parseSubprocessEvent(agent_message): %v", err)
	}
	if string(event.structuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %q, want JSON text from agent_message", event.structuredOutput)
	}
}

func TestSubprocessClaudeRealBackgroundJob(t *testing.T) {
	if os.Getenv("CR_CLAUDE_BG_INTEGRATION") != "1" {
		t.Skip("set CR_CLAUDE_BG_INTEGRATION=1 to run the real Claude background-job contract check")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatalf("claude CLI not found for integration check: %v", err)
	}
	env := []string(nil)
	if configDir := strings.TrimSpace(os.Getenv("CR_CLAUDE_BG_INTEGRATION_CONFIG_DIR")); configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	adapter := NewClaudeCLIAdapter(SubprocessOptions{
		Env:     env,
		Timeout: 5 * time.Minute,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := adapter.Start(ctx, Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high",
		Prompt: "Return this exact structured JSON object and nothing else: {\"ok\":true}",
	})
	if err != nil {
		t.Fatalf("Start real Claude bg integration: %v", err)
	}
	response, err := stream.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait real Claude bg integration: %v", err)
	}
	if stream.SessionID() == "" {
		t.Fatal("real Claude bg integration returned empty session id")
	}
	var decoded map[string]bool
	if err := json.Unmarshal(response.StructuredOutput, &decoded); err != nil || !decoded["ok"] {
		t.Fatalf("real Claude bg output = %q err = %v, want {\"ok\":true}", response.StructuredOutput, err)
	}

	resumeStream, err := adapter.Resume(ctx, stream.SessionID(), Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high",
		Prompt: "Return this exact structured JSON object and nothing else: {\"resumed\":true}",
	})
	if err != nil {
		t.Fatalf("Resume real Claude bg integration: %v", err)
	}
	resumeResponse, err := resumeStream.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait resumed real Claude bg integration: %v", err)
	}
	decoded = map[string]bool{}
	if err := json.Unmarshal(resumeResponse.StructuredOutput, &decoded); err != nil || !decoded["resumed"] {
		t.Fatalf("real Claude bg resume output = %q err = %v, want {\"resumed\":true}", resumeResponse.StructuredOutput, err)
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
	args := adapterArgsFromHelper()
	record := helperRecord{
		AdapterArgs:     args,
		Cwd:             cwd,
		CwdEntries:      len(entries),
		StdinBytes:      len(stdin),
		Stdin:           string(stdin),
		ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		TMPDir:          os.Getenv("TMPDIR"),
		GoCache:         os.Getenv("GOCACHE"),
		GoTmpDir:        os.Getenv("GOTMPDIR"),
		XDGCacheHome:    os.Getenv("XDG_CACHE_HOME"),
		TMP:             os.Getenv("TMP"),
		TEMP:            os.Getenv("TEMP"),
		CGOCFlags:       os.Getenv("CGO_CFLAGS"),
		CGOCXXFlags:     os.Getenv("CGO_CXXFLAGS"),
	}
	record.AddDir = flagValue(args, "--add-dir")
	if record.AddDir != "" {
		addDirEntries, _ := os.ReadDir(record.AddDir)
		record.AddDirEntries = len(addDirEntries)
		if promptData, err := os.ReadFile(filepath.Join(record.AddDir, claudeBGPromptFilename)); err == nil { // #nosec G304 -- helper reads adapter-owned prompt scratch file in tests.
			record.PromptFile = string(promptData)
			record.PromptFileBytes = len(promptData)
		}
	}
	if recordPath != "" {
		appendHelperRecord(recordPath, record)
	}
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			if os.Getenv("LLM_HELPER_MODE") == "bg-stop-fails" || helperControlShouldFail("LLM_HELPER_CLAUDE_FAIL_STOP_IDS", args) {
				fmt.Fprintln(os.Stderr, "stop refused")
				os.Exit(44)
			}
			os.Exit(0)
		case "rm":
			if helperControlShouldFail("LLM_HELPER_CLAUDE_FAIL_RM_IDS", args) {
				fmt.Fprintln(os.Stderr, "rm refused")
				os.Exit(45)
			}
			// #nosec G703 -- helper removes only a test CLAUDE_CONFIG_DIR job path under t.TempDir.
			if err := os.RemoveAll(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "jobs", args[1])); err != nil {
				fmt.Fprintf(os.Stderr, "rm cleanup failed: %v\n", err)
				os.Exit(47)
			}
			os.Exit(0)
		case "agents":
			if os.Getenv("LLM_HELPER_CLAUDE_AGENTS_FAIL") == "1" {
				fmt.Fprintln(os.Stderr, "agents refused")
				os.Exit(46)
			}
			if payload := os.Getenv("LLM_HELPER_CLAUDE_AGENTS_JSON"); payload != "" {
				fmt.Print(payload)
				os.Exit(0)
			}
			fmt.Print("[]")
			os.Exit(0)
		}
	}
	if containsFlag(args, "--bg") {
		runClaudeBGHelper(os.Getenv("LLM_HELPER_MODE"), args)
		os.Exit(0)
	}
	if containsFlag(args, "-p") && flagValue(args, "--output-format") == "json" {
		runClaudeForegroundHelper(os.Getenv("LLM_HELPER_MODE"), args)
		os.Exit(0)
	}
	switch os.Getenv("LLM_HELPER_MODE") {
	case "success":
		fmt.Println(`{"type":"thread.started","thread_id":"session-1"}`)
		fmt.Println(`{"type":"unknown.event","ignored":true}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`)
	case "codex-usage":
		fmt.Println(`{"type":"thread.started","thread_id":"session-usage"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":25475,"cached_input_tokens":19712,"output_tokens":812,"reasoning_output_tokens":271}}`)
	case "noisy-success":
		fmt.Fprintln(os.Stderr, strings.Repeat("stderr-noise-", 32))
		fmt.Println(`{"type":"thread.started","thread_id":"session-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`)
	case "tool":
		fmt.Println(`{"type":"thread.started","thread_id":"session-1"}`)
		fmt.Println(`{"type":"tool_use","name":"Read"}`)
		time.Sleep(10 * time.Second)
	case "nested-tool":
		fmt.Println(`{"type":"message","delta":{"tool_use":{"name":"Read"}}}`)
		time.Sleep(10 * time.Second)
	case "spawn-child-tool":
		child := exec.Command(os.Args[0], "-test.run=TestSubprocessChildProcess", "--") // #nosec G204,G702 -- helper launches the current test binary.
		child.Env = append(os.Environ(), "LLM_SUBPROCESS_CHILD=1")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(os.Getenv("LLM_HELPER_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600) // #nosec G703 -- helper writes to t.TempDir path.
		}
		fmt.Println(`{"type":"tool_use","name":"Read"}`)
		time.Sleep(10 * time.Second)
	case "spawn-child-sleep":
		child := exec.Command(os.Args[0], "-test.run=TestSubprocessChildProcess", "--") // #nosec G204,G702 -- helper launches the current test binary.
		child.Env = append(os.Environ(), "LLM_SUBPROCESS_CHILD=1")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(os.Getenv("LLM_HELPER_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600) // #nosec G703 -- helper writes to t.TempDir path.
		}
		time.Sleep(10 * time.Second)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "malformed":
		fmt.Println(`{"type":`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`)
	default:
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"ok\":true}"}}`)
	}
	os.Exit(0)
}

func TestSubprocessChildProcess(_ *testing.T) {
	if os.Getenv("LLM_SUBPROCESS_CHILD") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

type helperRecord struct {
	AdapterArgs     []string `json:"adapter_args"`
	Cwd             string   `json:"cwd"`
	CwdEntries      int      `json:"cwd_entries"`
	StdinBytes      int      `json:"stdin_bytes"`
	Stdin           string   `json:"stdin"`
	ClaudeConfigDir string   `json:"claude_config_dir"`
	AddDir          string   `json:"add_dir"`
	AddDirEntries   int      `json:"add_dir_entries"`
	PromptFile      string   `json:"prompt_file"`
	PromptFileBytes int      `json:"prompt_file_bytes"`
	TMPDir          string   `json:"tmpdir"`
	GoCache         string   `json:"gocache"`
	GoTmpDir        string   `json:"gotmpdir"`
	XDGCacheHome    string   `json:"xdg_cache_home"`
	TMP             string   `json:"tmp"`
	TEMP            string   `json:"temp"`
	CGOCFlags       string   `json:"cgo_cflags"`
	CGOCXXFlags     string   `json:"cgo_cxxflags"`
}

func newClaudeHelperAdapter(mode string, recordPath string, configDir string, timeout time.Duration) *SubprocessAdapter {
	return newClaudeHelperAdapterWithEnv(mode, recordPath, configDir, timeout)
}

func newClaudeHelperAdapterWithEnv(mode string, recordPath string, configDir string, timeout time.Duration, extraEnv ...string) *SubprocessAdapter {
	return NewClaudeCLIAdapter(SubprocessOptions{
		Command:           os.Args[0],
		commandArgsPrefix: helperPrefix(),
		Env:               append(helperClaudeEnv(mode, recordPath, configDir), extraEnv...),
		Timeout:           timeout,
	})
}

func newCodexHelperAdapter(mode string, recordPath string, timeout time.Duration, extraEnv ...string) *SubprocessAdapter {
	return NewCodexCLIAdapter(SubprocessOptions{
		Command:                os.Args[0],
		commandArgsPrefix:      helperPrefix(),
		Env:                    append(helperEnv(mode, recordPath), extraEnv...),
		Timeout:                timeout,
		AllowBestEffortNoTools: true,
	})
}

func helperPrefix() []string {
	return []string{"-test.run=TestSubprocessHelperProcess", "--"}
}

func helperClaudeEnv(mode string, recordPath string, configDir string) []string {
	return append(helperEnv(mode, recordPath), "CLAUDE_CONFIG_DIR="+configDir, "CR_CLAUDE_BG_WORK_DIR="+helperClaudeWorkDir(recordPath))
}

func helperClaudeWorkDir(recordPath string) string {
	return filepath.Join(filepath.Dir(recordPath), "claude-bg-workdir")
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

func helperControlShouldFail(envKey string, args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, jobID := range strings.Split(os.Getenv(envKey), ",") {
		if strings.TrimSpace(jobID) == args[1] {
			return true
		}
	}
	return false
}

func runClaudeBGHelper(mode string, args []string) {
	jobID := "job-1"
	resultDir := flagValue(args, "--add-dir")
	state := map[string]any{
		"state":     "done",
		"sessionId": "session-1",
	}
	result := `{"ok":true}`
	writeResult := true
	switch mode {
	case "bg-idle-result":
		state = map[string]any{
			"state":     "working",
			"tempo":     "idle",
			"sessionId": "session-idle",
			"inFlight":  map[string]any{"tasks": 0, "queued": 0},
		}
		result = `{"idle":true}`
	case "bg-invalid-json":
		state["sessionId"] = "session-invalid"
		result = `not-json`
	case "bg-blocked":
		state = map[string]any{"state": "blocked", "detail": "permission needed"}
		writeResult = false
	case "bg-failed":
		state = map[string]any{"state": "failed", "error": "model failed"}
		writeResult = false
	case "bg-waiting":
		state = map[string]any{"state": "waiting", "needs": "waiting for input"}
		writeResult = false
	case "bg-stopped":
		state = map[string]any{"state": "stopped", "message": "stopped by user"}
		writeResult = false
	case "bg-stop-fails":
		state = map[string]any{"state": "blocked", "detail": "stop will fail"}
		writeResult = false
	case "bg-missing-result":
		state = map[string]any{"state": "done", "sessionId": "session-missing"}
		writeResult = false
	case "bg-empty-result":
		state = map[string]any{"state": "done", "sessionId": "session-empty"}
		result = ""
	case "bg-running":
		state = map[string]any{
			"state":     "working",
			"tempo":     "busy",
			"sessionId": "session-running",
			"inFlight":  map[string]any{"tasks": 1, "queued": 0},
			"detail":    "still running",
		}
		writeResult = false
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := writeClaudeHelperStateFile(filepath.Join(configDir, "jobs", jobID, "state.json"), state); err != nil {
		fmt.Fprintf(os.Stderr, "write state: %v\n", err)
		os.Exit(43)
	}
	if writeResult {
		// #nosec G703 -- helper writes only inside the test scratch dir.
		_ = os.WriteFile(filepath.Join(resultDir, claudeBGResultFilename), []byte(result), 0o600)
	}
	fmt.Printf("backgrounded * %s\n  claude attach %s\n", jobID, jobID)
}

func writeClaudeHelperState(t *testing.T, path string, state map[string]any) {
	t.Helper()
	if err := writeClaudeHelperStateFile(path, state); err != nil {
		t.Fatalf("writeClaudeHelperStateFile: %v", err)
	}
}

func writeClaudeHelperStateFile(path string, state map[string]any) error {
	// #nosec G703 -- helper state path is derived from t.TempDir config dir and fixed job id.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// #nosec G306,G703 -- helper state is written under t.TempDir.
	return os.WriteFile(path, data, 0o600)
}

func writeClaudeHelperStateAt(t *testing.T, path string, state map[string]any, modTime time.Time) {
	t.Helper()
	if err := writeClaudeHelperStateFile(path, state); err != nil {
		t.Fatalf("writeClaudeHelperStateFile: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(state): %v", err)
	}
}

func appendHelperRecord(path string, record helperRecord) {
	data, _ := json.Marshal(record)
	// #nosec G304,G306,G703 -- helper writes only to a t.TempDir path supplied by the parent test.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.Write(append(data, '\n'))
}

func readHelperRecord(t *testing.T, path string) helperRecord {
	t.Helper()
	records := readHelperRecords(t, path)
	if len(records) == 0 {
		t.Fatalf("no helper records in %s", path)
	}
	return records[0]
}

func readHelperRecords(t *testing.T, path string) []helperRecord {
	t.Helper()
	// #nosec G304 -- test reads helper output from a t.TempDir path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(record): %v", err)
	}
	var records []helperRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record helperRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("Unmarshal(record line %q): %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func assertClaudeCleanup(t *testing.T, records []helperRecord, jobID string, wantStop bool, configDir string) {
	t.Helper()
	if len(records) == 0 {
		t.Fatalf("assertClaudeCleanup: no records, cannot verify cleanup")
	}
	stopSeen := false
	rmSeen := false
	for _, record := range records[1:] {
		if len(record.AdapterArgs) != 2 || record.AdapterArgs[1] != jobID {
			continue
		}
		if record.ClaudeConfigDir != configDir {
			t.Fatalf("cleanup CLAUDE_CONFIG_DIR = %q, want %q", record.ClaudeConfigDir, configDir)
		}
		switch record.AdapterArgs[0] {
		case "stop":
			stopSeen = true
		case "rm":
			rmSeen = true
		}
	}
	if stopSeen != wantStop {
		t.Fatalf("stopSeen = %v, want %v in records %#v", stopSeen, wantStop, records)
	}
	if !rmSeen {
		t.Fatalf("rmSeen = false, want true in records %#v", records)
	}
}

func containsSamePath(paths []string, want string) bool {
	for _, path := range paths {
		if sameCleanPath(path, want) {
			return true
		}
	}
	return false
}

func assertReviewerToolchainEnv(t *testing.T, record helperRecord, repoRoot string, addDirs []string, wantCFlags, wantCXXFlags string) {
	t.Helper()
	roots := append([]string{repoRoot}, addDirs...)
	for name, path := range map[string]string{
		"TMPDIR":         record.TMPDir,
		"TMP":            record.TMP,
		"TEMP":           record.TEMP,
		"GOTMPDIR":       record.GoTmpDir,
		"GOCACHE":        record.GoCache,
		"XDG_CACHE_HOME": record.XDGCacheHome,
	} {
		if path == "" || !pathUnderAnyRoot(path, roots) {
			t.Fatalf("%s = %q, want path under repo or explicit --add-dir roots %#v", name, path, roots)
		}
	}
	clangFlag := "-fmodules-cache-path=" + filepath.Join(record.AddDir, "cache", "clang-module-cache")
	if record.CGOCFlags != wantCFlags+" "+clangFlag {
		t.Fatalf("CGO_CFLAGS = %q, want preserved %q plus %q", record.CGOCFlags, wantCFlags, clangFlag)
	}
	if record.CGOCXXFlags != wantCXXFlags+" "+clangFlag {
		t.Fatalf("CGO_CXXFLAGS = %q, want preserved %q plus %q", record.CGOCXXFlags, wantCXXFlags, clangFlag)
	}
}

func pathUnderAnyRoot(path string, roots []string) bool {
	path = strings.TrimPrefix(filepath.Clean(path), "/private")
	for _, root := range roots {
		root = strings.TrimPrefix(filepath.Clean(root), "/private")
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func hasClaudeControlRecord(records []helperRecord, verb string, jobID string) bool {
	for _, record := range records {
		if len(record.AdapterArgs) == 2 && record.AdapterArgs[0] == verb && record.AdapterArgs[1] == jobID {
			return true
		}
	}
	return false
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	pidData, err := os.ReadFile(path) // #nosec G304 -- test reads helper pid from t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(child pid): %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("Atoi(child pid): %v", err)
	}
	return pid
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

func removeFlagPair(args []string, flag string) []string {
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

func replaceSubprocessFlagValue(args []string, flag string, value string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flag {
			out[i+1] = value
			return out
		}
	}
	return out
}

func replaceSubprocessFlagPairWithEquals(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			out = append(out, flag+"="+args[i+1])
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", timeout)
}

func TestSubprocessAdapterDefaultsTaskTimeout(t *testing.T) {
	// Without a bound, one hung CLI worker or never-terminal background job
	// hangs the entire review indefinitely — the caller in app/runtime.go sets
	// no Timeout, so the constructor must default it.
	a := NewClaudeCLIAdapter(SubprocessOptions{})
	if a.timeout != defaultLLMTaskTimeout {
		t.Fatalf("timeout = %v, want default %v", a.timeout, defaultLLMTaskTimeout)
	}

	explicit := NewCodexCLIAdapter(SubprocessOptions{Timeout: 3 * time.Minute})
	if explicit.timeout != 3*time.Minute {
		t.Fatalf("explicit timeout = %v, want 3m", explicit.timeout)
	}

	// Negative opts out: LaunchProcess only applies a deadline when timeout > 0.
	unbounded := NewClaudeCLIAdapter(SubprocessOptions{Timeout: -1})
	if unbounded.timeout >= 0 {
		t.Fatalf("negative timeout should be preserved as opt-out, got %v", unbounded.timeout)
	}

	pi := NewPiRPCAdapter(PiRPCOptions{})
	if pi.timeout != defaultLLMTaskTimeout {
		t.Fatalf("pi timeout = %v, want default %v", pi.timeout, defaultLLMTaskTimeout)
	}
}

func runClaudeForegroundHelper(mode string, args []string) {
	scratch := flagValue(args, "--add-dir")
	switch mode {
	case "foreground-success":
		if scratch != "" {
			_ = os.WriteFile(filepath.Join(scratch, claudeBGResultFilename), []byte(`{"ok":true}`), 0o600)
		}
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"wrote the result file","session_id":"fg-session-1"}`)
	case "foreground-no-result":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"forgot the file","session_id":"fg-session-2"}`)
	case "foreground-fail":
		fmt.Fprintln(os.Stderr, "model is overloaded_error")
		os.Exit(1)
	case "foreground-preamble":
		fmt.Println("Warning: some banner text before the payload")
		fmt.Println(`{"unrelated": true} trailing junk that must not parse`)
		if scratch != "" {
			_ = os.WriteFile(filepath.Join(scratch, claudeBGResultFilename), []byte(`{"ok":true}`), 0o600)
		}
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"fg-session-3"}`)
	}
}

func TestSubprocessClaudeForegroundMode(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	configDir := filepath.Join(tempDir, "claude")
	adapter := newClaudeHelperAdapterWithEnv(
		"foreground-success", recordPath, configDir, 5*time.Second, "CR_CLAUDE_FOREGROUND=1")

	stream, err := adapter.Start(context.Background(), Request{
		Model:   "claude-sonnet-5",
		Effort:  "high",
		Prompt:  "prompt",
		LogPath: filepath.Join(tempDir, "events.log"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s", response.StructuredOutput)
	}
	if stream.SessionID() != "fg-session-1" {
		t.Fatalf("SessionID = %q, want fg-session-1", stream.SessionID())
	}

	records := readHelperRecords(t, recordPath)
	record := records[0]
	if containsFlag(record.AdapterArgs, "--bg") {
		t.Fatalf("args = %#v, foreground mode must not pass --bg", record.AdapterArgs)
	}
	if !containsFlag(record.AdapterArgs, "-p") || flagValue(record.AdapterArgs, "--output-format") != "json" {
		t.Fatalf("args = %#v, want -p --output-format json", record.AdapterArgs)
	}
	assertFlagValue(t, record.AdapterArgs, "--model", "claude-sonnet-5")
	assertFlagValue(t, record.AdapterArgs, "--permission-mode", "acceptEdits")
}

func TestSubprocessClaudeForegroundNoResultFileErrors(t *testing.T) {
	tempDir := t.TempDir()
	adapter := newClaudeHelperAdapterWithEnv(
		"foreground-no-result", filepath.Join(tempDir, "records.jsonl"),
		filepath.Join(tempDir, "claude"), 5*time.Second, "CR_CLAUDE_FOREGROUND=1")
	stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err == nil {
		t.Fatal("Wait should fail when no result file was written")
	}
}

func TestSubprocessClaudeForegroundTransientFailure(t *testing.T) {
	tempDir := t.TempDir()
	adapter := newClaudeHelperAdapterWithEnv(
		"foreground-fail", filepath.Join(tempDir, "records.jsonl"),
		filepath.Join(tempDir, "claude"), 5*time.Second, "CR_CLAUDE_FOREGROUND=1")
	stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = stream.Wait(context.Background())
	if err == nil || !errors.Is(err, ErrTransient) {
		t.Fatalf("Wait err = %v, want ErrTransient (overloaded stderr detail)", err)
	}
}

func TestClaudeForegroundDisabledByDefault(t *testing.T) {
	if claudeForegroundEnabled(nil) && os.Getenv("CR_CLAUDE_FOREGROUND") == "" {
		t.Fatal("foreground must be opt-in")
	}
	if !claudeForegroundEnabled([]string{"CR_CLAUDE_FOREGROUND=1"}) {
		t.Fatal("env slice should enable foreground")
	}
	if claudeForegroundEnabled([]string{"CR_CLAUDE_FOREGROUND=0"}) {
		t.Fatal("explicit 0 should disable")
	}
}

func TestSubprocessClaudeForegroundResume(t *testing.T) {
	tempDir := t.TempDir()
	recordPath := filepath.Join(tempDir, "records.jsonl")
	adapter := newClaudeHelperAdapterWithEnv(
		"foreground-success", recordPath, filepath.Join(tempDir, "claude"),
		5*time.Second, "CR_CLAUDE_FOREGROUND=1")

	stream, err := adapter.Resume(context.Background(), "prior-session", Request{
		Model:  "claude-sonnet-5",
		Prompt: "resume prompt",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	record := readHelperRecords(t, recordPath)[0]
	if containsFlag(record.AdapterArgs, "--bg") {
		t.Fatalf("args = %#v, foreground resume must not fall back to --bg", record.AdapterArgs)
	}
	if !containsFlag(record.AdapterArgs, "-p") {
		t.Fatalf("args = %#v, want -p for foreground resume", record.AdapterArgs)
	}
	assertFlagValue(t, record.AdapterArgs, "--resume", "prior-session")
}

func TestSubprocessClaudeForegroundPreambleParsing(t *testing.T) {
	tempDir := t.TempDir()
	adapter := newClaudeHelperAdapterWithEnv(
		"foreground-preamble", filepath.Join(tempDir, "records.jsonl"),
		filepath.Join(tempDir, "claude"), 5*time.Second, "CR_CLAUDE_FOREGROUND=1")
	stream, err := adapter.Start(context.Background(), Request{Prompt: "prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s", response.StructuredOutput)
	}
	if stream.SessionID() != "fg-session-3" {
		t.Fatalf("SessionID = %q, want fg-session-3 (parsed from trailing JSON, not preamble)", stream.SessionID())
	}
}

func TestClaudeTransportXORValidation(t *testing.T) {
	scratch := t.TempDir()
	adapter := NewClaudeCLIAdapter(SubprocessOptions{})
	base := []string{
		"--tools", "Read,Write",
		"--add-dir", scratch,
		"--", "positional prompt",
	}
	cases := []struct {
		name           string
		prefix         []string
		permissionMode string
		wantErr        bool
	}{
		{"bg only", []string{"--bg"}, "auto", false},
		{"foreground only", []string{"-p", "--output-format", "json"}, "acceptEdits", false},
		{"acceptEdits with bg", []string{"--bg"}, "acceptEdits", true},
		{"auto with foreground", []string{"-p", "--output-format", "json"}, "auto", true},
		{"both transports", []string{"--bg", "-p", "--output-format", "json"}, "auto", true},
		{"neither transport", nil, "acceptEdits", true},
	}
	for _, tc := range cases {
		args := append(append([]string(nil), tc.prefix...), "--permission-mode", tc.permissionMode)
		args = append(args, base...)
		err := adapter.validateArgs(args, scratch, Request{})
		if tc.wantErr && !errors.Is(err, ErrUnsafeSubprocessConfig) {
			t.Fatalf("%s: err = %v, want ErrUnsafeSubprocessConfig", tc.name, err)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: err = %v, want nil", tc.name, err)
		}
	}
}
