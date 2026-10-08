package llmadapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

// The evidence collector and settlement protocol arrived on separate branches.
// Keep execution evidence across retries without treating agent_end as final.
func TestPiRPCReviewerTraceRequiresSettledRun(t *testing.T) {
	final := piRPCTestTurn{text: `{"ok":true}`, stopReason: "stop"}
	failure := piRPCTestTurn{stopReason: "error", errorMessage: "fixture retry failure"}
	firstRead := []string{
		piRPCTestPromptStarted,
		`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"cr_read","args":{"path":"one.go"}}`,
		`{"type":"tool_execution_end","toolCallId":"read-1","toolName":"cr_read","isError":false}`,
	}
	for _, tt := range []struct {
		name            string
		tail            []string
		wantComplete    bool
		wantCalls       int
		wantErr         string
		cancelAfterCall bool
	}{
		{
			name: "retry retains calls until successful settlement",
			tail: []string{
				failure.end(), piRPCTestAgentEnd(true, failure),
				`{"type":"auto_retry_start","attempt":1}`,
				`#pause`,
				`{"type":"tool_execution_start","toolCallId":"read-2","toolName":"cr_read","args":{"path":"two.go","view":"symlink"}}`,
				`{"type":"tool_execution_end","toolCallId":"read-2","toolName":"cr_read","isError":false}`,
				final.end(), piRPCTestAgentEnd(false, final), `{"type":"agent_settled"}`,
			},
			wantComplete: true,
			wantCalls:    2,
		},
		{
			name:      "agent end without settlement is incomplete",
			tail:      []string{final.end(), piRPCTestAgentEnd(false, final), `#exit`},
			wantCalls: 1,
			wantErr:   "agent_settled",
		},
		{
			name:            "caller cancellation preserves partial trace",
			tail:            strings.Split(strings.Repeat("#pause\n", 100), "\n"),
			wantCalls:       1,
			wantErr:         "context canceled",
			cancelAfterCall: true,
		},
		{
			name:      "malformed settlement is incomplete",
			tail:      []string{final.end(), piRPCTestAgentEnd(false, final), `{"type":"agent_settled"`},
			wantCalls: 1,
			wantErr:   "malformed JSONL",
		},
		{
			name:      "settlement without final answer is incomplete",
			tail:      []string{`{"type":"agent_settled"}`},
			wantCalls: 1,
			wantErr:   "no structured output",
		},
		{
			name:      "settled provider failure is incomplete",
			tail:      []string{failure.end(), piRPCTestAgentEnd(false, failure), `{"type":"agent_settled"}`},
			wantCalls: 1,
			wantErr:   "final assistant message error",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			repoDir, scratchDir := filepath.Join(root, "repo"), filepath.Join(root, "scratch")
			for _, dir := range []string{repoDir, scratchDir} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			diffPath := filepath.Join(root, "diff.patch")
			if err := os.WriteFile(diffPath, []byte("fixed diff\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := append(append([]string(nil), firstRead...), tt.tail...)
			adapter := piRPCScriptAdapter(t, 5*time.Second, script...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logPath := filepath.Join(root, "events.jsonl")
			stream, err := adapter.Start(ctx, Request{
				Prompt:  "review",
				LogPath: logPath,
				ReviewerWorkspace: &ReviewerWorkspaceRequest{
					RepoDir: repoDir, ScratchDir: scratchDir, DiffPath: diffPath, MaxToolOutputBytes: 2048,
				},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if tt.cancelAfterCall {
				deadline := time.Now().Add(3 * time.Second)
				for {
					logged, _ := os.ReadFile(logPath) // #nosec G304 -- fixture-owned path.
					if strings.Contains(string(logged), `"tool_execution_end"`) {
						cancel()
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("tool execution checkpoint was not observed before cancellation")
					}
					time.Sleep(time.Millisecond)
				}
			}
			response, err := stream.Wait(context.Background())
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Wait error = %v, want %q", err, tt.wantErr)
			}
			evidence := response.ReviewerToolEvidence
			if evidence == nil || evidence.Trace == nil || evidence.Trace.StreamComplete != tt.wantComplete || len(evidence.Trace.Calls) != tt.wantCalls {
				t.Fatalf("evidence = %#v, want complete=%t and %d calls", evidence, tt.wantComplete, tt.wantCalls)
			}
			if evidence.DiffStatus != llm.DiffToolStatusNotInvoked {
				t.Fatalf("diff status = %s, file reads must not satisfy the diff gate", evidence.DiffStatus)
			}
			for _, call := range evidence.Trace.Calls {
				if call.Status != llm.ReviewerToolCallSucceeded {
					t.Fatalf("call = %#v, want retained successful execution", call)
				}
			}
			if tt.wantComplete && (string(response.StructuredOutput) != `{"ok":true}` || evidence.Trace.Calls[1].ReadView != llm.ReviewerToolReadSymlink) {
				t.Fatalf("settled output = %s, calls = %#v", response.StructuredOutput, evidence.Trace.Calls)
			}
			if !tt.wantComplete && len(response.StructuredOutput) != 0 {
				t.Fatalf("failed stream returned structured output: %s", response.StructuredOutput)
			}
		})
	}
}
