package benchmark

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunMetrics summarizes usage and tool activity for one review run.
type RunMetrics struct {
	LLMCalls    int            `json:"llm_calls"`
	Turns       int            `json:"turns"`
	ToolCalls   int            `json:"tool_calls"`
	ToolResults int            `json:"tool_results"`
	Tokens      TokenMetrics   `json:"tokens"`
	Cost        CostMetrics    `json:"cost"`
	Phases      []PhaseMetrics `json:"phases,omitempty"`
}

// TokenMetrics records provider token usage.
type TokenMetrics struct {
	Available   bool  `json:"available"`
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheWrite  int64 `json:"cache_write"`
	TotalTokens int64 `json:"total_tokens"`
}

// CostMetrics records provider-reported cost estimates.
type CostMetrics struct {
	Available  bool    `json:"available"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Total      float64 `json:"total"`
}

// PhaseMetrics summarizes one agent log.
type PhaseMetrics struct {
	Name        string       `json:"name"`
	Role        string       `json:"role,omitempty"`
	LogPath     string       `json:"log_path"`
	Provider    string       `json:"provider,omitempty"`
	Model       string       `json:"model,omitempty"`
	StopReason  string       `json:"stop_reason,omitempty"`
	LLMCalls    int          `json:"llm_calls"`
	Turns       int          `json:"turns"`
	ToolCalls   int          `json:"tool_calls"`
	ToolResults int          `json:"tool_results"`
	Tokens      TokenMetrics `json:"tokens"`
	Cost        CostMetrics  `json:"cost"`
}

// HasData reports whether metrics contain provider usage or activity.
func (m RunMetrics) HasData() bool {
	return len(m.Phases) > 0 || m.Turns > 0 || m.LLMCalls > 0 || m.ToolCalls > 0 || m.ToolResults > 0 || m.Tokens.Available || m.Cost.Available || m.Tokens.TotalTokens > 0 || m.Cost.Total > 0
}

// HasTokenUsage reports whether provider token telemetry was captured.
func (m RunMetrics) HasTokenUsage() bool {
	return m.Tokens.Available
}

// HasCostUsage reports whether provider cost telemetry was captured.
func (m RunMetrics) HasCostUsage() bool {
	return m.Cost.Available
}

// ExtractRunMetrics reads agent JSONL logs from a review artifact directory.
func ExtractRunMetrics(artifactPath string) (RunMetrics, error) {
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return RunMetrics{}, nil
	}
	logs, err := filepath.Glob(filepath.Join(artifactPath, "agent-logs", "*.jsonl"))
	if err != nil {
		return RunMetrics{}, err
	}
	sort.Strings(logs)
	var metrics RunMetrics
	for _, logPath := range logs {
		phase, err := extractPhaseMetrics(logPath)
		if err != nil {
			return RunMetrics{}, err
		}
		if !phaseHasData(phase) {
			continue
		}
		metrics.Phases = append(metrics.Phases, phase)
		metrics.Turns += phase.Turns
		metrics.ToolCalls += phase.ToolCalls
		metrics.ToolResults += phase.ToolResults
		metrics.LLMCalls += phase.LLMCalls
		metrics.Tokens.add(phase.Tokens)
		metrics.Cost.add(phase.Cost)
	}
	return metrics, nil
}

func extractPhaseMetrics(logPath string) (PhaseMetrics, error) {
	// #nosec G304 -- logPath is discovered from the benchmark-owned review artifact's agent-logs directory.
	file, err := os.Open(logPath)
	if err != nil {
		return PhaseMetrics{}, err
	}
	defer file.Close()

	name := phaseName(logPath)
	phase := PhaseMetrics{Name: name, Role: phaseRole(name), LogPath: logPath}
	partialFallbacks := newPartialUsageFallbacks()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return PhaseMetrics{}, fmt.Errorf("%s: %w", logPath, err)
		}
		accumulateEvent(&phase, event, partialFallbacks)
	}
	if err := scanner.Err(); err != nil {
		return PhaseMetrics{}, err
	}
	partialFallbacks.flush(&phase)
	if phase.Tokens.TotalTokens == 0 {
		phase.Tokens.TotalTokens = phase.Tokens.Input + phase.Tokens.Output + phase.Tokens.CacheRead + phase.Tokens.CacheWrite
	}
	return phase, nil
}

func accumulateEvent(phase *PhaseMetrics, event map[string]any, partialFallbacks *partialUsageFallbacks) {
	eventType := stringValue(event["type"])
	switch eventType {
	case "turn_start":
		partialFallbacks.flushUnkeyed(phase)
		phase.Turns++
	case "tool_call", "tool_start":
		phase.ToolCalls++
	case "tool_result", "tool_end":
		phase.ToolResults++
	}
	if strings.Contains(eventType, "tool") && eventType != "tool_call" && eventType != "tool_start" && eventType != "tool_result" && eventType != "tool_end" {
		phase.ToolCalls++
	}
	if values, ok := event["toolCalls"].([]any); ok {
		phase.ToolCalls += len(values)
	}
	if values, ok := event["toolResults"].([]any); ok {
		phase.ToolResults += len(values)
	}

	if message, ok := event["message"].(map[string]any); ok {
		if accumulateMessage(phase, message) {
			partialFallbacks.discard(messageKey(event, message))
		} else if eventType == "message_end" {
			partialFallbacks.flushOne(phase, messageKey(event, message))
		}
	}
	if assistantEvent, ok := event["assistantMessageEvent"].(map[string]any); ok {
		if partial, ok := assistantEvent["partial"].(map[string]any); ok {
			accumulateMessageMetadata(phase, partial)
			if usage, ok := messageUsageFromMessage(partial); ok {
				partialFallbacks.record(messageKey(event, partial), usage)
			}
		}
	}
}

func accumulateMessageMetadata(phase *PhaseMetrics, message map[string]any) {
	provider := stringValue(message["provider"])
	model := stringValue(message["model"])
	if provider != "" {
		phase.Provider = provider
	}
	if model != "" {
		phase.Model = model
	}
	stopReason := stringValue(message["stopReason"])
	if stopReason != "" {
		phase.StopReason = stopReason
	}
}

func accumulateMessage(phase *PhaseMetrics, message map[string]any) bool {
	accumulateMessageMetadata(phase, message)
	usage, ok := messageUsageFromMessage(message)
	if !ok {
		return false
	}
	accumulateUsageMetrics(phase, usage)
	return true
}

type messageUsageMetrics struct {
	tokens TokenMetrics
	cost   CostMetrics
}

func messageUsageFromMessage(message map[string]any) (messageUsageMetrics, bool) {
	usage, ok := message["usage"].(map[string]any)
	if !ok {
		return messageUsageMetrics{}, false
	}
	tokens := tokenMetricsFromUsage(usage)
	cost := costMetricsFromUsage(usage)
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.Input + tokens.Output + tokens.CacheRead + tokens.CacheWrite
	}
	if !tokens.Available && !cost.Available {
		return messageUsageMetrics{}, false
	}
	return messageUsageMetrics{tokens: tokens, cost: cost}, true
}

func accumulateUsageMetrics(phase *PhaseMetrics, usage messageUsageMetrics) {
	phase.LLMCalls++
	phase.Tokens.add(usage.tokens)
	phase.Cost.add(usage.cost)
}

type partialUsageFallbacks struct {
	keyed  map[string]messageUsageMetrics
	order  []string
	active *messageUsageMetrics
}

func newPartialUsageFallbacks() *partialUsageFallbacks {
	return &partialUsageFallbacks{keyed: make(map[string]messageUsageMetrics)}
}

func (f *partialUsageFallbacks) record(key string, usage messageUsageMetrics) {
	if key == "" {
		copied := usage
		f.active = &copied
		return
	}
	if _, ok := f.keyed[key]; !ok {
		f.order = append(f.order, key)
	}
	f.keyed[key] = usage
}

func (f *partialUsageFallbacks) discard(key string) {
	if key != "" {
		delete(f.keyed, key)
		return
	}
	f.active = nil
}

func (f *partialUsageFallbacks) flushOne(phase *PhaseMetrics, key string) {
	if key != "" {
		if usage, ok := f.keyed[key]; ok {
			accumulateUsageMetrics(phase, usage)
			delete(f.keyed, key)
		}
		return
	}
	f.flushUnkeyed(phase)
}

func (f *partialUsageFallbacks) flushUnkeyed(phase *PhaseMetrics) {
	if f.active == nil {
		return
	}
	accumulateUsageMetrics(phase, *f.active)
	f.active = nil
}

func (f *partialUsageFallbacks) flush(phase *PhaseMetrics) {
	f.flushUnkeyed(phase)
	for _, key := range f.order {
		if usage, ok := f.keyed[key]; ok {
			accumulateUsageMetrics(phase, usage)
			delete(f.keyed, key)
		}
	}
}

func messageKey(event map[string]any, message map[string]any) string {
	if key := stringValue(message["id"]); key != "" {
		return key
	}
	if key := stringValue(message["messageId"]); key != "" {
		return key
	}
	if key := stringValue(message["message_id"]); key != "" {
		return key
	}
	if key := stringValue(event["messageId"]); key != "" {
		return key
	}
	if key := stringValue(event["message_id"]); key != "" {
		return key
	}
	if assistantEvent, ok := event["assistantMessageEvent"].(map[string]any); ok {
		if key := stringValue(assistantEvent["messageId"]); key != "" {
			return key
		}
		if key := stringValue(assistantEvent["message_id"]); key != "" {
			return key
		}
	}
	return ""
}

func tokenMetricsFromUsage(usage map[string]any) TokenMetrics {
	tokenKeys := []string{"input", "inputTokens", "input_tokens", "tokensIn", "tokens_in", "promptTokens", "prompt_tokens", "output", "outputTokens", "output_tokens", "tokensOut", "tokens_out", "completionTokens", "completion_tokens", "cacheRead", "cache_read", "cacheWrite", "cache_write", "cacheCreate", "cache_create", "totalTokens", "total_tokens", "tokens", "total"}
	return TokenMetrics{
		Available:   hasAnyKey(usage, tokenKeys...),
		Input:       int64ValueFromKeys(usage, "input", "inputTokens", "input_tokens", "tokensIn", "tokens_in", "promptTokens", "prompt_tokens"),
		Output:      int64ValueFromKeys(usage, "output", "outputTokens", "output_tokens", "tokensOut", "tokens_out", "completionTokens", "completion_tokens"),
		CacheRead:   int64ValueFromKeys(usage, "cacheRead", "cache_read"),
		CacheWrite:  int64ValueFromKeys(usage, "cacheWrite", "cache_write", "cacheCreate", "cache_create"),
		TotalTokens: int64ValueFromKeys(usage, "totalTokens", "total_tokens", "tokens", "total"),
	}
}

func costMetricsFromUsage(usage map[string]any) CostMetrics {
	costValue := usage["cost"]
	cost, ok := costValue.(map[string]any)
	if !ok {
		return CostMetrics{
			Available: hasAnyKey(usage, "cost", "cost_usd", "costUSD", "totalCost", "total_cost"),
			Total:     float64ValueFromKeys(usage, "cost", "cost_usd", "costUSD", "totalCost", "total_cost"),
		}
	}
	total := float64ValueFromKeys(cost, "total", "totalCost", "total_cost", "costUSD", "cost_usd")
	if total == 0 {
		total = float64Value(costValue)
	}
	metrics := CostMetrics{
		Available:  true,
		Input:      float64ValueFromKeys(cost, "input", "inputCost", "input_cost"),
		Output:     float64ValueFromKeys(cost, "output", "outputCost", "output_cost"),
		CacheRead:  float64ValueFromKeys(cost, "cacheRead", "cache_read", "cacheReadCost", "cache_read_cost"),
		CacheWrite: float64ValueFromKeys(cost, "cacheWrite", "cache_write", "cacheCreate", "cache_create", "cacheWriteCost", "cache_write_cost", "cacheCreateCost", "cache_create_cost"),
		Total:      total,
	}
	if metrics.Total == 0 {
		metrics.Total = metrics.Input + metrics.Output + metrics.CacheRead + metrics.CacheWrite
	}
	return metrics
}

func (m *TokenMetrics) add(other TokenMetrics) {
	m.Available = m.Available || other.Available
	m.Input += other.Input
	m.Output += other.Output
	m.CacheRead += other.CacheRead
	m.CacheWrite += other.CacheWrite
	m.TotalTokens += other.TotalTokens
}

func (m *CostMetrics) add(other CostMetrics) {
	m.Available = m.Available || other.Available
	m.Input += other.Input
	m.Output += other.Output
	m.CacheRead += other.CacheRead
	m.CacheWrite += other.CacheWrite
	m.Total += other.Total
}

func phaseHasData(phase PhaseMetrics) bool {
	return phase.LLMCalls > 0 || phase.Turns > 0 || phase.ToolCalls > 0 || phase.ToolResults > 0 || phase.Tokens.Available || phase.Cost.Available || phase.Tokens.TotalTokens > 0 || phase.Cost.Total > 0
}

func phaseName(logPath string) string {
	name := strings.TrimSuffix(filepath.Base(logPath), filepath.Ext(logPath))
	if decoded, err := url.QueryUnescape(name); err == nil {
		name = decoded
	}
	return name
}

func phaseRole(name string) string {
	switch name {
	case "orchestrator-selection":
		return "orchestrator_selection"
	case "orchestrator-rollup":
		return "orchestrator_rollup"
	default:
		if strings.HasPrefix(name, "orchestrator-") {
			return "orchestrator"
		}
		return "reviewer"
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func int64ValueFromKeys(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value := int64Value(values[key]); value != 0 {
			return value
		}
	}
	return 0
}

func hasAnyKey(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

func float64Value(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int64:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return 0
	}
}

func float64ValueFromKeys(values map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value := float64Value(values[key]); value != 0 {
			return value
		}
	}
	return 0
}

func writeJSONFile(path string, value any) error {
	// #nosec G304 -- path is a benchmark-owned output artifact path resolved by the caller.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return nil
}

// WriteMetricsFile writes metrics as indented JSON.
func WriteMetricsFile(path string, metrics RunMetrics) error {
	return writeJSONFile(path, metrics)
}

// IsMissingMetricsSource reports whether the artifact log source is absent.
func IsMissingMetricsSource(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, io.EOF)
}
