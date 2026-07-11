package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	piRPCPromptID     = "prompt-1"
	piRPCSystemPrompt = "You are a strict JSON API for code review structured output. Return exactly one JSON object that matches the requested schema. Do not include markdown fences, prose, explanations, or leading/trailing text. The first byte of your final answer must be { and the last byte must be }."
)

// PiRPCOptions configures the Pi RPC subprocess adapter.
type PiRPCOptions struct {
	Command           string
	Env               []string
	Timeout           time.Duration
	ScratchDirFactory ScratchDirFactory
	FastModeModels    []string
	commandArgsPrefix []string
}

// PiRPCAdapter runs Pi in RPC mode as an LLM adapter.
type PiRPCAdapter struct {
	command           string
	commandArgsPrefix []string
	env               []string
	timeout           time.Duration
	scratchDirFactory ScratchDirFactory
	fastModeModels    []string
}

// NewPiRPCAdapter returns a Pi RPC subprocess adapter.
func NewPiRPCAdapter(opts PiRPCOptions) *PiRPCAdapter {
	command := opts.Command
	if command == "" {
		command = "pi"
	}
	factory := opts.ScratchDirFactory
	if factory == nil {
		factory = defaultScratchDir
	}
	return &PiRPCAdapter{
		command:           command,
		commandArgsPrefix: append([]string(nil), opts.commandArgsPrefix...),
		env:               append([]string(nil), opts.Env...),
		timeout:           opts.Timeout,
		scratchDirFactory: factory,
		fastModeModels:    append([]string(nil), opts.FastModeModels...),
	}
}

// Name returns the adapter name.
func (a *PiRPCAdapter) Name() string { return "pi_rpc" }

// SupportsResume reports whether Pi RPC session resume is implemented.
func (a *PiRPCAdapter) SupportsResume() bool { return false }

// SupportsCacheAccounting reports whether provider usage can include cache fields.
func (a *PiRPCAdapter) SupportsCacheAccounting() bool { return true }

// SupportsCostReporting reports whether provider responses include cost.
func (a *PiRPCAdapter) SupportsCostReporting() bool { return true }

// Quota reports unsupported quota for Pi RPC.
func (a *PiRPCAdapter) Quota(context.Context) (Quota, bool, error) {
	return Quota{}, false, nil
}

// Resume is unsupported until Pi RPC session persistence is designed.
func (a *PiRPCAdapter) Resume(context.Context, string, Request) (Stream, error) {
	return nil, errors.New("llm pi rpc: resume unsupported")
}

// Start launches Pi in RPC mode in a fresh scratch directory.
func (a *PiRPCAdapter) Start(ctx context.Context, req Request) (Stream, error) {
	if err := validateFastMode(a.Name(), a.fastModeModels, req); err != nil {
		return nil, err
	}
	scratch, cleanup, err := a.scratchDirFactory()
	if err != nil {
		return nil, err
	}
	if cleanup == nil {
		cleanup = func() error { return nil }
	}
	scratch, err = validateScratchDir(scratch)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	args, err := a.buildArgs(req, scratch)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	if err := a.validateArgs(args); err != nil {
		_ = cleanup()
		return nil, err
	}

	execArgs := append(append([]string(nil), a.commandArgsPrefix...), args...)
	var env []string
	if len(a.env) > 0 {
		env = append(os.Environ(), a.env...)
	}
	process, err := launchProcess(ctx, a.command, execArgs, scratch, env, a.timeout, req.LogPath, cleanup, true)
	if err != nil {
		return nil, err
	}
	if err := writePiRPCPrompt(process.stdin, req.Prompt); err != nil {
		process.abort(cleanup)
		return nil, err
	}

	stream := &piRPCStream{
		baseStream: baseStream{
			cancel:       process.cancel,
			done:         make(chan struct{}),
			logFile:      process.logFile,
			cleanup:      cleanup,
			processGroup: process.processGroup,
		},
		stdin: process.stdin,
	}
	go stream.run(process.ctx, process.cmd, process.stdout, process.stderr)
	return stream, nil
}

func (a *PiRPCAdapter) buildArgs(req Request, _ string) ([]string, error) {
	args := []string{
		"--mode", "rpc",
		"--system-prompt", piRPCSystemPrompt,
		"--no-tools",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-session",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--thinking", req.Effort)
	}
	return args, nil
}

func (a *PiRPCAdapter) validateArgs(args []string) error {
	if err := validateAllowedFlags("pi_rpc", args, map[string]bool{
		"--mode":                true,
		"--system-prompt":       true,
		"--no-tools":            false,
		"--no-extensions":       false,
		"--no-skills":           false,
		"--no-prompt-templates": false,
		"--no-themes":           false,
		"--no-session":          false,
		"--model":               true,
		"--thinking":            true,
	}); err != nil {
		return err
	}
	if flagValue(args, "--mode") != "rpc" {
		return fmt.Errorf("%w: pi_rpc must use rpc mode", ErrUnsafeSubprocessConfig)
	}
	if containsFlag(args, "--tools") || containsFlag(args, "-t") {
		return fmt.Errorf("%w: pi_rpc must disable tools", ErrUnsafeSubprocessConfig)
	}
	for _, flag := range []string{"--system-prompt", "--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session"} {
		if !containsFlag(args, flag) {
			return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
		}
	}
	if containsFlag(args, "--system-prompt") && flagValue(args, "--system-prompt") != piRPCSystemPrompt {
		return fmt.Errorf("%w: pi_rpc system prompt mismatch", ErrUnsafeSubprocessConfig)
	}
	return nil
}

func writePiRPCPrompt(stdin io.Writer, prompt string) error {
	command := map[string]string{
		"id":      piRPCPromptID,
		"type":    "prompt",
		"message": prompt,
	}
	data, err := json.Marshal(command)
	if err != nil {
		return err
	}
	_, err = stdin.Write(append(data, '\n'))
	if err != nil {
		return fmt.Errorf("llm pi rpc: send prompt: %w", err)
	}
	return nil
}

type piRPCStream struct {
	baseStream
	stdin io.Closer
}

func (s *piRPCStream) run(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderr io.Reader) {
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if s.logFile != nil {
			_, _ = io.Copy(&s.baseStream, stderr)
			return
		}
		_, _ = io.Copy(io.Discard, stderr)
	}()

	scanResult := s.scanStdout(stdout)
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	waitErr := cmd.Wait()
	<-stderrDone

	result := subprocessResult{response: scanResult.response}
	switch {
	case scanResult.err != nil:
		result.err = scanResult.err
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.err = ctx.Err()
	case scanResult.agentEnd && waitErr != nil:
		result.err = waitErr
	case scanResult.agentEnd && len(scanResult.response.StructuredOutput) == 0:
		result.err = errors.New("llm pi rpc: no structured output")
	case scanResult.agentEnd:
		result.err = nil
	case ctx.Err() != nil:
		result.err = ctx.Err()
	case waitErr != nil:
		result.err = waitErr
	case !scanResult.agentEnd:
		result.err = errors.New("llm pi rpc: missing agent_end")
	}
	s.cancel()
	if s.processGroup != nil {
		_ = s.processGroup.close()
	}
	s.closeLog()
	if s.cleanup != nil {
		_ = s.cleanup()
	}
	s.mu.Lock()
	s.result = result
	s.mu.Unlock()
	close(s.done)
}

type piRPCScanResult struct {
	response Response
	err      error
	agentEnd bool
}

func (s *piRPCStream) scanStdout(stdout io.Reader) piRPCScanResult {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var result piRPCScanResult
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		s.writeLog(normalizePiRPCLogLine(line))
		event, err := parsePiRPCEvent(line)
		if err != nil {
			s.cancel()
			result.err = err
			return result
		}
		if event.sessionID != "" {
			s.setSessionID(event.sessionID)
		}
		if event.toolUse {
			s.cancel()
			result.err = ErrToolUse
			return result
		}
		if event.responseFailure != "" {
			s.cancel()
			result.err = fmt.Errorf("llm pi rpc: prompt failed: %s", event.responseFailure)
			return result
		}
		if len(event.structuredOutput) > 0 {
			result.response.StructuredOutput = event.structuredOutput
		}
		result.response.Usage = mergeUsage(result.response.Usage, event.usage)
		if event.agentEnd {
			result.agentEnd = true
			return result
		}
	}
	if err := scanner.Err(); err != nil && result.err == nil {
		result.err = err
	}
	return result
}

func normalizePiRPCLogLine(line []byte) []byte {
	logLine := append([]byte(nil), line...)
	if len(logLine) == 0 {
		return []byte{'\n'}
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(line, &event); err != nil {
		return append(logLine, '\n')
	}
	if rawString(event, "type") != "message_update" {
		return append(logLine, '\n')
	}
	assistantEventRaw, ok := event["assistantMessageEvent"]
	if !ok {
		return append(logLine, '\n')
	}
	var assistantEvent map[string]json.RawMessage
	if err := json.Unmarshal(assistantEventRaw, &assistantEvent); err != nil {
		return append(logLine, '\n')
	}
	partialRaw, ok := assistantEvent["partial"]
	if !ok {
		return append(logLine, '\n')
	}
	if compactPartial := compactPiRPCPartialForLog(partialRaw); len(compactPartial) > 0 {
		assistantEvent["partial"] = compactPartial
	} else {
		delete(assistantEvent, "partial")
	}
	normalizedAssistantEvent, err := json.Marshal(assistantEvent)
	if err != nil {
		return append(logLine, '\n')
	}
	event["assistantMessageEvent"] = normalizedAssistantEvent
	normalized, err := json.Marshal(event)
	if err != nil {
		return append(logLine, '\n')
	}
	return append(normalized, '\n')
}

func compactPiRPCPartialForLog(partialRaw json.RawMessage) json.RawMessage {
	var partial map[string]json.RawMessage
	if err := json.Unmarshal(partialRaw, &partial); err != nil {
		return nil
	}
	compact := make(map[string]json.RawMessage)
	for _, key := range []string{"role", "provider", "model", "api", "stopReason"} {
		if value, ok := partial[key]; ok {
			compact[key] = value
		}
	}
	if len(compact) == 0 {
		return nil
	}
	encoded, err := json.Marshal(compact)
	if err != nil {
		return nil
	}
	return encoded
}

type piRPCEvent struct {
	sessionID        string
	structuredOutput []byte
	usage            Usage
	toolUse          bool
	responseFailure  string
	agentEnd         bool
}

func parsePiRPCEvent(line []byte) (piRPCEvent, error) {
	var decoded any
	if err := json.Unmarshal(line, &decoded); err != nil {
		return piRPCEvent{}, fmt.Errorf("llm pi rpc: malformed JSONL event: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return piRPCEvent{}, fmt.Errorf("llm pi rpc: malformed JSONL event: %w", err)
	}
	eventType := rawString(raw, "type")
	event := piRPCEvent{
		toolUse: piRPCEventIndicatesToolUse(eventType) || valueIndicatesToolUse(decoded),
		usage:   parsePiRPCUsage(raw),
	}
	if id := firstRawString(raw, "sessionId", "session_id"); id != "" {
		event.sessionID = id
	}
	if eventType == "response" && rawString(raw, "command") == "prompt" && !rawBool(raw, "success") {
		event.responseFailure = rawString(raw, "error")
		if event.responseFailure == "" {
			event.responseFailure = "unknown error"
		}
		return event, nil
	}
	if eventType == "message_end" {
		if output, usage := parsePiRPCMessageEnd(raw["message"]); len(output) > 0 {
			event.structuredOutput = output
			event.usage = mergeUsage(event.usage, usage)
		} else {
			event.usage = mergeUsage(event.usage, usage)
		}
	}
	if eventType == "agent_end" {
		event.agentEnd = true
		if output := parsePiRPCAgentEnd(raw["messages"]); len(output) > 0 {
			event.structuredOutput = output
		}
	}
	return event, nil
}

func piRPCEventIndicatesToolUse(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return eventIndicatesToolUse(value) ||
		strings.HasPrefix(normalized, "tool_execution_") ||
		normalized == "bash"
}

func parsePiRPCMessageEnd(value json.RawMessage) ([]byte, Usage) {
	var raw map[string]json.RawMessage
	if len(value) == 0 {
		return nil, Usage{}
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return nil, Usage{}
	}
	usage := parsePiRPCUsage(raw)
	if rawString(raw, "role") != "assistant" {
		return nil, usage
	}
	return extractPiRPCText(raw["content"]), usage
}

func parsePiRPCAgentEnd(value json.RawMessage) []byte {
	var messages []map[string]json.RawMessage
	if len(value) == 0 {
		return nil
	}
	if err := json.Unmarshal(value, &messages); err != nil {
		return nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if rawString(messages[i], "role") != "assistant" {
			continue
		}
		if output := extractPiRPCText(messages[i]["content"]); len(output) > 0 {
			return output
		}
	}
	return nil
}

func extractPiRPCText(value json.RawMessage) []byte {
	var asString string
	if err := json.Unmarshal(value, &asString); err == nil {
		return []byte(strings.TrimSpace(asString))
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(value, &blocks); err != nil {
		return nil
	}
	var out strings.Builder
	for _, block := range blocks {
		if rawString(block, "type") != "text" {
			continue
		}
		out.WriteString(firstRawString(block, "text", "content"))
	}
	return []byte(strings.TrimSpace(out.String()))
}

func parsePiRPCUsage(raw map[string]json.RawMessage) Usage {
	value, ok := raw["usage"]
	if !ok {
		return Usage{}
	}
	var usageRaw map[string]json.RawMessage
	if err := json.Unmarshal(value, &usageRaw); err != nil {
		return Usage{}
	}
	usage := Usage{
		TokensIn:    firstRawIntPtr(usageRaw, "tokens_in", "tokensIn", "input", "inputTokens", "promptTokens"),
		TokensOut:   firstRawIntPtr(usageRaw, "tokens_out", "tokensOut", "output", "outputTokens", "completionTokens"),
		CacheRead:   firstRawIntPtr(usageRaw, "cache_read", "cacheRead"),
		CacheCreate: firstRawIntPtr(usageRaw, "cache_create", "cacheCreate", "cache_write", "cacheWrite"),
		CostUSD:     firstRawFloatPtr(usageRaw, "cost_usd", "costUSD", "totalCost", "totalCostUSD"),
	}
	if usage.CostUSD == nil {
		usage.CostUSD = nestedRawFloatPtr(usageRaw, "cost", "total")
	}
	return usage
}

func firstRawString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := rawString(raw, key); value != "" {
			return value
		}
	}
	return ""
}

func rawBool(raw map[string]json.RawMessage, key string) bool {
	value, ok := raw[key]
	if !ok {
		return false
	}
	var out bool
	if err := json.Unmarshal(value, &out); err != nil {
		return false
	}
	return out
}

func firstRawIntPtr(raw map[string]json.RawMessage, keys ...string) *int {
	for _, key := range keys {
		if value := rawIntPtr(raw, key); value != nil {
			return value
		}
	}
	return nil
}

func firstRawFloatPtr(raw map[string]json.RawMessage, keys ...string) *float64 {
	for _, key := range keys {
		if value := rawFloatPtr(raw, key); value != nil {
			return value
		}
	}
	return nil
}

func nestedRawFloatPtr(raw map[string]json.RawMessage, objectKey string, valueKey string) *float64 {
	value, ok := raw[objectKey]
	if !ok {
		return nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(value, &nested); err != nil {
		return nil
	}
	return rawFloatPtr(nested, valueKey)
}
