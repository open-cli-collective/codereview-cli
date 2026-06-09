package llm

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
	assertFlagValue(t, record.AdapterArgs, "--permission-mode", "acceptEdits")
	assertFlagValue(t, record.AdapterArgs, "--model", "claude-sonnet-4-6")
	assertFlagValue(t, record.AdapterArgs, "--effort", "high")
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
			if err := claude.validateArgs(tt.args, scratch); !errors.Is(err, ErrUnsafeSubprocessConfig) {
				t.Fatalf("validateArgs error = %v, want ErrUnsafeSubprocessConfig", err)
			}
		})
	}
	inlineClaudeArgs := replaceSubprocessFlagPairWithEquals(claudeArgs, "--tools")
	inlineClaudeArgs = replaceSubprocessFlagPairWithEquals(inlineClaudeArgs, "--permission-mode")
	inlineClaudeArgs = replaceSubprocessFlagPairWithEquals(inlineClaudeArgs, "--add-dir")
	if err := claude.validateArgs(inlineClaudeArgs, scratch); err != nil {
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
			if err := codex.validateArgs(tt.args, codexScratch); !errors.Is(err, ErrUnsafeSubprocessConfig) {
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
	if codex.SupportsResume() {
		t.Fatal("Codex SupportsResume = true, want false")
	}
	if _, err := codex.Resume(context.Background(), "session", Request{}); err == nil {
		t.Fatal("Codex Resume error = nil, want unsupported")
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
	switch os.Getenv("LLM_HELPER_MODE") {
	case "success":
		fmt.Println(`{"type":"thread.started","thread_id":"session-1"}`)
		fmt.Println(`{"type":"unknown.event","ignored":true}`)
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

func newCodexHelperAdapter(mode string, recordPath string, timeout time.Duration) *SubprocessAdapter {
	return NewCodexCLIAdapter(SubprocessOptions{
		Command:                os.Args[0],
		commandArgsPrefix:      helperPrefix(),
		Env:                    helperEnv(mode, recordPath),
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
