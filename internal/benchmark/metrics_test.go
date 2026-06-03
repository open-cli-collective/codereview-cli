package benchmark

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractRunMetricsAggregatesAgentLogs(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "orchestrator-selection.jsonl"), `{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"assistant","api":"openai-completions","provider":"opencode-go","model":"deepseek-v4-pro","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1,"totalTokens":18,"cost":{"input":0.1,"output":0.2,"cacheRead":0.01,"cacheWrite":0.03,"total":0.34}},"stopReason":"stop"}}
`)
	writeLog(t, filepath.Join(logDir, "frontend%3Afrontend-code-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"partial":{"role":"assistant","provider":"opencode-go","model":"deepseek-v4-pro","usage":{"input":100,"output":50,"cacheRead":20,"cacheWrite":0,"totalTokens":170,"cost":{"input":1,"output":2,"cacheRead":0.1,"cacheWrite":0,"total":3.1}},"stopReason":"stop"}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if !metrics.HasData() {
		t.Fatal("metrics.HasData() = false")
	}
	if metrics.Turns != 2 || metrics.LLMCalls != 2 {
		t.Fatalf("metrics activity = turns %d, llm calls %d", metrics.Turns, metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 110 || metrics.Tokens.Output != 55 || metrics.Tokens.CacheRead != 22 || metrics.Tokens.CacheWrite != 1 || metrics.Tokens.TotalTokens != 188 {
		t.Fatalf("tokens = %#v", metrics.Tokens)
	}
	if metrics.Cost.Total != 3.44 {
		t.Fatalf("cost total = %v", metrics.Cost.Total)
	}
	if len(metrics.Phases) != 2 {
		t.Fatalf("phases = %d, want 2", len(metrics.Phases))
	}
	if metrics.Phases[0].Name != "frontend:frontend-code-reviewer" || metrics.Phases[0].Role != "reviewer" {
		t.Fatalf("phase[0] = %#v", metrics.Phases[0])
	}
	if metrics.Phases[1].Name != "orchestrator-selection" || metrics.Phases[1].Role != "orchestrator_selection" {
		t.Fatalf("phase[1] = %#v", metrics.Phases[1])
	}
}

func TestExtractRunMetricsReturnsEmptyWhenLogsAreAbsent(t *testing.T) {
	metrics, err := ExtractRunMetrics(t.TempDir())
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.HasData() {
		t.Fatalf("metrics = %#v, want empty", metrics)
	}
}

func writeLog(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
