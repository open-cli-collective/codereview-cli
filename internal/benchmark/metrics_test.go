package benchmark

import (
	"math"
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

func TestExtractRunMetricsUsesFinalUsageOverStreamingPartials(t *testing.T) {
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

func TestExtractRunMetricsFallsBackToLastStreamingPartialUsage(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "partial-only-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"a","partial":{"role":"assistant","provider":"partial-provider","model":"partial-model","usage":{"input":50,"output":20,"totalTokens":70,"cost":{"total":1.2}}}}}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"b","partial":{"role":"assistant","provider":"partial-provider","model":"partial-model","usage":{"input":60,"output":25,"totalTokens":85,"cost":{"total":1.5}}}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want last partial counted once", metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 60 || metrics.Tokens.Output != 25 || metrics.Tokens.TotalTokens != 85 {
		t.Fatalf("tokens = %#v, want last partial usage", metrics.Tokens)
	}
	if metrics.Cost.Total != 1.5 {
		t.Fatalf("cost = %#v, want last partial usage", metrics.Cost)
	}
	if len(metrics.Phases) != 1 || metrics.Phases[0].Provider != "partial-provider" || metrics.Phases[0].Model != "partial-model" {
		t.Fatalf("phase metadata = %#v", metrics.Phases)
	}
}

func TestExtractRunMetricsKeepsPartialFallbackPerCompletion(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "mixed-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"a","partial":{"role":"assistant","usage":{"input":100,"output":50,"totalTokens":150,"cost":{"total":3}}}}}
{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"b","partial":{"role":"assistant","usage":{"input":80,"output":40,"totalTokens":120,"cost":{"total":2.4}}}}}
{"type":"message_end","message":{"role":"assistant","usage":{"input":8,"output":4,"totalTokens":12,"cost":{"total":0.24}}}}
{"type":"message_update","assistantMessageEvent":{"messageId":"msg-3","type":"thinking_delta","delta":"c","partial":{"id":"msg-3","role":"assistant","usage":{"input":30,"output":10,"totalTokens":40,"cost":{"total":0.8}}}}}
{"type":"message_update","assistantMessageEvent":{"messageId":"msg-4","type":"thinking_delta","delta":"d","partial":{"id":"msg-4","role":"assistant","usage":{"input":20,"output":10,"totalTokens":30,"cost":{"total":0.6}}}}}
{"type":"message_end","message":{"id":"msg-4","role":"assistant","usage":{"input":2,"output":1,"totalTokens":3,"cost":{"total":0.06}}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.LLMCalls != 4 {
		t.Fatalf("LLMCalls = %d, want two partial fallbacks and two final messages", metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 140 || metrics.Tokens.Output != 65 || metrics.Tokens.TotalTokens != 205 {
		t.Fatalf("tokens = %#v", metrics.Tokens)
	}
	if math.Abs(metrics.Cost.Total-4.1) > 0.000001 {
		t.Fatalf("cost = %#v", metrics.Cost)
	}
}

func TestExtractRunMetricsKeepsUnkeyedFallbackWhenKeyedCompletionFinishes(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "mixed-keyed-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"a","partial":{"role":"assistant","usage":{"input":100,"output":50,"totalTokens":150,"cost":{"total":3}}}}}
{"type":"message_update","assistantMessageEvent":{"messageId":"msg-2","type":"thinking_delta","delta":"b","partial":{"id":"msg-2","role":"assistant","usage":{"input":20,"output":10,"totalTokens":30,"cost":{"total":0.6}}}}}
{"type":"message_end","message":{"id":"msg-2","role":"assistant","usage":{"input":2,"output":1,"totalTokens":3,"cost":{"total":0.06}}}}
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.LLMCalls != 2 {
		t.Fatalf("LLMCalls = %d, want unkeyed fallback plus keyed final message", metrics.LLMCalls)
	}
	if metrics.Tokens.Input != 102 || metrics.Tokens.Output != 51 || metrics.Tokens.TotalTokens != 153 {
		t.Fatalf("tokens = %#v", metrics.Tokens)
	}
	if metrics.Cost.Total != 3.06 {
		t.Fatalf("cost = %#v", metrics.Cost)
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

func TestExtractRunMetricsAggregatesPiDiffEvidence(t *testing.T) {
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "reviewer-success.jsonl"), `{"type":"turn_start"}
codereview-pi-tool-evidence tool=cr_diff status=succeeded started=1 completed=1 failed=0
`)
	writeLog(t, filepath.Join(logDir, "reviewer-failed.jsonl"), `{"type":"turn_start"}
codereview-pi-tool-evidence tool=cr_diff status=failed started=1 completed=1 failed=1 error="fixed diff unavailable"
`)
	writeLog(t, filepath.Join(logDir, "reviewer-not-invoked.jsonl"), `{"type":"turn_start"}
codereview-pi-tool-evidence tool=cr_diff status=not_invoked started=0 completed=0 failed=0
`)

	metrics, err := ExtractRunMetrics(artifactPath)
	if err != nil {
		t.Fatalf("ExtractRunMetrics: %v", err)
	}
	if metrics.PiDiff == nil {
		t.Fatal("PiDiff = nil, want aggregated evidence")
	}
	if metrics.PiDiff.Succeeded != 1 || metrics.PiDiff.Failed != 1 || metrics.PiDiff.NotInvoked != 1 || metrics.PiDiff.Incomplete != 0 {
		t.Fatalf("PiDiff = %#v, want one success, failure, and non-invocation", metrics.PiDiff)
	}
	if len(metrics.Phases) != 3 {
		t.Fatalf("phases = %d, want evidence-only phases retained", len(metrics.Phases))
	}
}

func writeLog(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
