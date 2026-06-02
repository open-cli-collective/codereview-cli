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
	"sync"
	"time"
)

const piRPCPromptID = "prompt-1"

// PiRPCOptions configures the Pi RPC subprocess adapter.
type PiRPCOptions struct {
	Command           string
	Env               []string
	Timeout           time.Duration
	ScratchDirFactory ScratchDirFactory
	commandArgsPrefix []string
}

// PiRPCAdapter runs Pi in RPC mode as an LLM adapter.
type PiRPCAdapter struct {
	command           string
	commandArgsPrefix []string
	env               []string
	timeout           time.Duration
	scratchDirFactory ScratchDirFactory
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

	procCtx, cancel := context.WithCancel(ctx)
	if a.timeout > 0 {
		procCtx, cancel = context.WithTimeout(ctx, a.timeout)
	}
	execArgs := append(append([]string(nil), a.commandArgsPrefix...), args...)
	// #nosec G204 -- the Pi RPC adapter intentionally launches a configured CLI binary after validating adapter-owned safety args.
	cmd := exec.CommandContext(procCtx, a.command, execArgs...)
	cmd.Dir = scratch
	procGroup, err := newProcessGroup(cmd)
	if err != nil {
		cancel()
		_ = cleanup()
		return nil, err
	}
	cmd.Cancel = func() error {
		return procGroup.kill(cmd)
	}
	if len(a.env) > 0 {
		cmd.Env = append(os.Environ(), a.env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	logFile, err := openSubprocessLog(req.LogPath)
	if err != nil {
		cancel()
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		closeSubprocessLog(logFile)
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	if err := procGroup.afterStart(cmd); err != nil {
		cancel()
		_ = procGroup.kill(cmd)
		go func() { _, _ = io.Copy(io.Discard, stdout) }()
		go func() { _, _ = io.Copy(io.Discard, stderr) }()
		_ = cmd.Wait()
		closeSubprocessLog(logFile)
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}
	if err := writePiRPCPrompt(stdin, req.Prompt); err != nil {
		cancel()
		_ = procGroup.kill(cmd)
		go func() { _, _ = io.Copy(io.Discard, stdout) }()
		go func() { _, _ = io.Copy(io.Discard, stderr) }()
		_ = cmd.Wait()
		closeSubprocessLog(logFile)
		_ = procGroup.close()
		_ = cleanup()
		return nil, err
	}

	stream := &piRPCStream{
		cancel:       cancel,
		done:         make(chan struct{}),
		stdin:        stdin,
		logFile:      logFile,
		cleanup:      cleanup,
		processGroup: procGroup,
	}
	go stream.run(procCtx, cmd, stdout, stderr)
	return stream, nil
}

func (a *PiRPCAdapter) buildArgs(req Request, _ string) ([]string, error) {
	args := []string{
		"--mode", "rpc",
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
	if containsFlag(args, "-p") || containsFlag(args, "--print") {
		return fmt.Errorf("%w: pi_rpc must not use print mode", ErrUnsafeSubprocessConfig)
	}
	if flagValue(args, "--mode") != "rpc" {
		return fmt.Errorf("%w: pi_rpc must use rpc mode", ErrUnsafeSubprocessConfig)
	}
	if containsFlag(args, "--tools") || containsFlag(args, "-t") {
		return fmt.Errorf("%w: pi_rpc must disable tools", ErrUnsafeSubprocessConfig)
	}
	for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session"} {
		if !containsFlag(args, flag) {
			return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
		}
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
	mu        sync.Mutex
	sessionID string
	result    subprocessResult

	cancel       context.CancelFunc
	done         chan struct{}
	stdin        io.Closer
	logMu        sync.Mutex
	logFile      *os.File
	cleanup      func() error
	processGroup *processGroup
}

func (s *piRPCStream) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *piRPCStream) Wait(ctx context.Context) (Response, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		s.cancel()
		<-s.done
	}

	s.mu.Lock()
	result := s.result
	if result.err == nil && ctx.Err() != nil {
		result.err = ctx.Err()
	}
	s.mu.Unlock()
	return result.response, result.err
}

func (s *piRPCStream) run(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderr io.Reader) {
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if s.logFile != nil {
			_, _ = io.Copy(piRPCLogWriter{stream: s}, stderr)
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
		s.writeLog(append(line, '\n'))
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

type piRPCLogWriter struct {
	stream *piRPCStream
}

func (w piRPCLogWriter) Write(p []byte) (int, error) {
	w.stream.logMu.Lock()
	defer w.stream.logMu.Unlock()
	if w.stream.logFile == nil {
		return len(p), nil
	}
	return w.stream.logFile.Write(p)
}

func (s *piRPCStream) writeLog(p []byte) {
	if s.logFile == nil {
		return
	}
	_, _ = piRPCLogWriter{stream: s}.Write(p)
}

func (s *piRPCStream) closeLog() {
	if s.logFile == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	closeSubprocessLog(s.logFile)
}

func (s *piRPCStream) setSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == "" {
		s.sessionID = id
	}
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
