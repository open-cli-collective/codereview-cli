package benchmark

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractRunMetricsAggregatesAgentLogs(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "orchestrator-selection.jsonl"), `{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"assistant","api":"openai-completions","provider":"opencode-go","model":"deepseek-v4-pro","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1,"totalTokens":18,"cost":{"input":0.1,"output":0.2,"cacheRead":0.01,"cacheWrite":0.03,"total":0.34}},"stopReason":"stop"}}
{"type":"message_end","message":{"role":"assistant","usage":{"tokens_in":7,"tokens_out":3,"cache_create":2,"cost_usd":0.25}}}
`)
	writeLog(t, filepath.Join(logDir, "frontend%3Afrontend-code-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"partial":{"role":"assistant","provider":"opencode-go","model":"deepseek-v4-pro","usage":{"input":1000,"output":500,"cacheRead":200,"cacheWrite":0,"totalTokens":1700,"cost":{"input":10,"output":20,"cacheRead":1,"cacheWrite":0,"total":31}},"stopReason":"stop"}}}
{"type":"message_end","message":{"role":"assistant","usage":{"tokensIn":4,"tokensOut":2,"cacheCreate":1,"totalCost":0.5}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if !metrics.HasData() {
		t.Fatal("metrics.HasData() = false")
	}
	if metrics.Turns != 2 || metrics.LLMCalls != 3 {
		t.Fatalf("metrics activity = turns %d, llm calls %d", metrics.Turns, metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 21 || metrics.Tokens.Output != 10 || metrics.Tokens.CacheRead != 2 || metrics.Tokens.CacheWrite != 4 || metrics.Tokens.TotalTokens != 37 {
		t.Fatalf("tokens = %#v", metrics.Tokens)
	}
	if !metrics.Tokens.Available || !metrics.Cost.Available {
		t.Fatalf("usage availability = tokens %v, cost %v; want both true", metrics.Tokens.Available, metrics.Cost.Available)
	}
	if metrics.Cost.Total != 1.09 {
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
	if metrics.Phases[0].LLMCalls != 1 || metrics.Phases[1].LLMCalls != 2 {
		t.Fatalf("phase llm calls = %d, %d; want 1, 2", metrics.Phases[0].LLMCalls, metrics.Phases[1].LLMCalls)
	}
	if metrics.Phases[0].Provider != "opencode-go" || metrics.Phases[0].Model != "deepseek-v4-pro" || metrics.Phases[0].StopReason != "stop" {
		t.Fatalf("phase[0] metadata = provider %q, model %q, stop %q", metrics.Phases[0].Provider, metrics.Phases[0].Model, metrics.Phases[0].StopReason)
	}
}

func TestExtractRunMetricsIgnoresStreamingPartials(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"a","partial":{"role":"assistant","usage":{"input":100,"output":50,"totalTokens":150,"cost":{"total":3}}}}}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"b","partial":{"role":"assistant","usage":{"input":100,"output":50,"totalTokens":150,"cost":{"total":3}}}}}
{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":5,"totalTokens":15,"cost":{"total":0.3}}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want final message only", metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 10 || metrics.Tokens.Output != 5 || metrics.Tokens.TotalTokens != 15 {
		t.Fatalf("tokens = %#v, want final message only", metrics.Tokens)
	}
	if metrics.Cost.Total != 0.3 {
		t.Fatalf("cost = %#v, want final message only", metrics.Cost)
	}
}

func TestExtractRunMetricsPreservesZeroValuedTelemetry(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "local-reviewer.jsonl"), `{"type":"message_end","message":{"role":"assistant","provider":"local","model":"free","usage":{"input":0,"output":0,"totalTokens":0,"cost_usd":0}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if !metrics.HasData() || !metrics.HasTokenUsage() || !metrics.HasCostUsage() {
		t.Fatalf("metrics availability = data %v, tokens %v, cost %v; want all true", metrics.HasData(), metrics.HasTokenUsage(), metrics.HasCostUsage())
	}
	if metrics.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want 1", metrics.LLMCalls)
	}
	if metrics.Tokens.TotalTokens != 0 || metrics.Cost.Total != 0 {
		t.Fatalf("metrics = %#v, want explicit zero telemetry", metrics)
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
