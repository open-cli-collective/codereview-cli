package llmadapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This covers adapter teardown ordering only. It does not establish process
// ownership or claim that the background service cleans up detached children.
func TestSubprocessClaudeJobCleanupPrecedesScratchRemoval(t *testing.T) {
	for _, tt := range []struct {
		name        string
		mode        string
		extraEnv    []string
		wantErr     string
		wantStop    bool
		wantWarning string
		wantJobDir  bool
	}{
		{name: "success", mode: "success"},
		{
			name:     "task failure",
			mode:     "bg-failed",
			wantErr:  ErrClaudeBGTransport.Error() + ": job failed: model failed",
			wantStop: true,
		},
		{
			name:        "stop failure preserves task error",
			mode:        "bg-stop-fails",
			wantErr:     ErrClaudeBGTransport.Error() + ": job blocked: stop will fail",
			wantStop:    true,
			wantWarning: "warning: llm subprocess: Claude bg cleanup stop failed: exit status 44: stop refused",
		},
		{
			name:        "remove failure preserves success",
			mode:        "success",
			extraEnv:    []string{"LLM_HELPER_CLAUDE_FAIL_RM_IDS=job-1"},
			wantWarning: "warning: llm subprocess: Claude bg cleanup rm failed: exit status 45: rm refused",
			wantJobDir:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			root, err := os.OpenRoot(tempDir)
			if err != nil {
				t.Fatalf("OpenRoot: %v", err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Errorf("Close root: %v", err)
				}
			})
			recordPath := filepath.Join(tempDir, "records.jsonl")
			logPath := filepath.Join(tempDir, "events.log")
			configDir := filepath.Join(tempDir, "claude")
			scratch := filepath.Join(tempDir, "scratch")
			if err := root.Mkdir("scratch", 0o700); err != nil {
				t.Fatal(err)
			}
			var cleanupRecords []byte
			var cleanupRecordErr error
			var cleanupLog []byte
			var cleanupLogErr error
			var cleanupJobInfo os.FileInfo
			var cleanupJobErr error
			var removalErr error
			cleanupCalls := 0
			adapter := NewClaudeCLIAdapter(SubprocessOptions{
				Command:           os.Args[0],
				commandArgsPrefix: helperPrefix(),
				Env:               append(helperClaudeEnv(tt.mode, recordPath, configDir), tt.extraEnv...),
				Timeout:           5 * time.Second,
				ScratchDirFactory: func() (string, func() error, error) {
					return scratch, func() error {
						cleanupCalls++
						// Records identify invocations; log and file snapshots assert
						// their visible effects at cleanup. Synchronous command-return
						// ordering is a separate source contract. Read these snapshots
						// only after Wait synchronizes with stream completion.
						cleanupRecords, cleanupRecordErr = root.ReadFile("records.jsonl")
						cleanupLog, cleanupLogErr = root.ReadFile("events.log")
						cleanupJobInfo, cleanupJobErr = root.Stat(filepath.Join("claude", "jobs", "job-1"))
						removalErr = root.RemoveAll("scratch")
						return removalErr
					}, nil
				},
			})
			// Exercise one background invocation, independently of the existing
			// fallback tests and the caller's foreground environment preference.
			stream, err := adapter.startClaudeBG(context.Background(), Request{Prompt: "prompt", LogPath: logPath}, "")
			if err != nil {
				t.Fatalf("startClaudeBG: %v", err)
			}
			response, err := stream.Wait(context.Background())
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Wait error = %v, want exactly %q", err, tt.wantErr)
				}
				if !errors.Is(err, ErrClaudeBGTransport) {
					t.Fatalf("Wait error = %v, want ErrClaudeBGTransport", err)
				}
			} else if err != nil || string(response.StructuredOutput) != `{"ok":true}` {
				t.Fatalf("Wait = (%s, %v), want success", response.StructuredOutput, err)
			}
			if cleanupCalls != 1 || cleanupRecordErr != nil || removalErr != nil {
				t.Fatalf("cleanup calls=%d record error=%v removal error=%v", cleanupCalls, cleanupRecordErr, removalErr)
			}
			if cleanupLogErr != nil {
				t.Fatalf("read log at scratch removal: %v", cleanupLogErr)
			}
			var warnings []string
			for _, line := range strings.Split(string(cleanupLog), "\n") {
				if strings.HasPrefix(line, "warning: ") {
					warnings = append(warnings, line)
				}
			}
			if got := strings.Join(warnings, "\n"); got != tt.wantWarning {
				t.Fatalf("warnings at scratch removal = %q, want %q", got, tt.wantWarning)
			}
			if tt.wantJobDir {
				if cleanupJobErr != nil || cleanupJobInfo == nil || !cleanupJobInfo.IsDir() {
					t.Fatalf("refused rm did not preserve job directory at scratch removal: %v", cleanupJobErr)
				}
			} else if !errors.Is(cleanupJobErr, os.ErrNotExist) {
				t.Fatalf("successful rm left job directory at scratch removal: %v", cleanupJobErr)
			}
			var records []helperRecord
			decoder := json.NewDecoder(strings.NewReader(string(cleanupRecords)))
			for {
				var record helperRecord
				if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("decode records at scratch removal: %v", err)
				}
				records = append(records, record)
			}
			assertClaudeCleanup(t, records, "job-1", tt.wantStop, configDir)
			if _, err := root.Stat("scratch"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch remains after Wait: %v", err)
			}
		})
	}
}
