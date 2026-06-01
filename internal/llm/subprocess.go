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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	// ErrToolUse reports a subprocess no-IO contract violation.
	ErrToolUse = errors.New("llm subprocess: tool use event")

	// ErrUnsafeSubprocessConfig reports a launch spec missing required safety flags.
	ErrUnsafeSubprocessConfig = errors.New("llm subprocess: unsafe configuration")
)

// ScratchDirFactory creates an empty working directory and returns its cleanup.
type ScratchDirFactory func() (string, func() error, error)

// SubprocessOptions configures subprocess LLM adapters.
type SubprocessOptions struct {
	Command                string
	Env                    []string
	Timeout                time.Duration
	ScratchDirFactory      ScratchDirFactory
	AllowBestEffortNoTools bool
	commandArgsPrefix      []string
}

type subprocessKind string

const (
	subprocessClaude subprocessKind = "claude_cli"
	subprocessCodex  subprocessKind = "codex_cli"
)

// SubprocessAdapter runs a subscription CLI as an LLM adapter.
type SubprocessAdapter struct {
	kind                   subprocessKind
	command                string
	commandArgsPrefix      []string
	env                    []string
	timeout                time.Duration
	scratchDirFactory      ScratchDirFactory
	allowBestEffortNoTools bool
}

// NewClaudeCLIAdapter returns a Claude Code subprocess adapter.
func NewClaudeCLIAdapter(opts SubprocessOptions) *SubprocessAdapter {
	return newSubprocessAdapter(subprocessClaude, "claude", opts)
}

// NewCodexCLIAdapter returns a Codex CLI subprocess adapter.
func NewCodexCLIAdapter(opts SubprocessOptions) *SubprocessAdapter {
	return newSubprocessAdapter(subprocessCodex, "codex", opts)
}

func newSubprocessAdapter(kind subprocessKind, defaultCommand string, opts SubprocessOptions) *SubprocessAdapter {
	command := opts.Command
	if command == "" {
		command = defaultCommand
	}
	factory := opts.ScratchDirFactory
	if factory == nil {
		factory = defaultScratchDir
	}
	return &SubprocessAdapter{
		kind:                   kind,
		command:                command,
		commandArgsPrefix:      append([]string(nil), opts.commandArgsPrefix...),
		env:                    append([]string(nil), opts.Env...),
		timeout:                opts.Timeout,
		scratchDirFactory:      factory,
		allowBestEffortNoTools: opts.AllowBestEffortNoTools,
	}
}

// Name returns the adapter name.
func (a *SubprocessAdapter) Name() string { return string(a.kind) }

// SupportsResume reports whether subprocess session resume is implemented.
func (a *SubprocessAdapter) SupportsResume() bool { return false }

// SupportsCacheAccounting reports whether cache usage metrics are guaranteed.
func (a *SubprocessAdapter) SupportsCacheAccounting() bool { return false }

// SupportsCostReporting reports whether cost metrics are guaranteed.
func (a *SubprocessAdapter) SupportsCostReporting() bool { return false }

// Quota reports unsupported quota for subscription CLI adapters.
func (a *SubprocessAdapter) Quota(context.Context) (Quota, bool, error) {
	return Quota{}, false, nil
}

// Start launches the configured subprocess in a fresh scratch directory.
func (a *SubprocessAdapter) Start(ctx context.Context, req Request) (Stream, error) {
	if a.kind == subprocessCodex && !a.allowBestEffortNoTools {
		return nil, fmt.Errorf("%w: codex_cli requires AllowBestEffortNoTools until Codex exposes an all-tools-disabled flag", ErrUnsafeSubprocessConfig)
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
	if err := a.validateArgs(args, scratch); err != nil {
		_ = cleanup()
		return nil, err
	}

	procCtx, cancel := context.WithCancel(ctx)
	if a.timeout > 0 {
		procCtx, cancel = context.WithTimeout(ctx, a.timeout)
	}
	execArgs := append(append([]string(nil), a.commandArgsPrefix...), args...)
	// #nosec G204 -- subprocess adapters intentionally launch configured CLI binaries after validating adapter-owned safety args.
	cmd := exec.CommandContext(procCtx, a.command, execArgs...)
	cmd.Dir = scratch
	cmd.Stdin = nil
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

	stream := &subprocessStream{
		cancel:       cancel,
		done:         make(chan struct{}),
		logFile:      logFile,
		cleanup:      cleanup,
		processGroup: procGroup,
	}
	go stream.run(procCtx, cmd, stdout, stderr)
	return stream, nil
}

// Resume is unsupported until subprocess session persistence is designed.
func (a *SubprocessAdapter) Resume(context.Context, string, Request) (Stream, error) {
	return nil, fmt.Errorf("llm subprocess: resume unsupported for %s", a.kind)
}

func (a *SubprocessAdapter) buildArgs(req Request, scratch string) ([]string, error) {
	switch a.kind {
	case subprocessClaude:
		args := []string{
			"--bare",
			"--print",
			"--output-format", "stream-json",
			"--tools", "",
			"--mcp-config", "{}",
			"--strict-mcp-config",
			"--disable-slash-commands",
			"--no-session-persistence",
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.Effort != "" {
			args = append(args, "--effort", req.Effort)
		}
		return append(args, "--", req.Prompt), nil
	case subprocessCodex:
		args := []string{
			"exec",
			"--json",
			"--ephemeral",
			"--skip-git-repo-check",
			"--ignore-user-config",
			"--ignore-rules",
			"--sandbox", "read-only",
			"--cd", scratch,
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.Effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+req.Effort)
		}
		return append(args, "--", req.Prompt), nil
	default:
		return nil, fmt.Errorf("%w: unknown subprocess adapter %q", ErrUnsafeSubprocessConfig, a.kind)
	}
}

func (a *SubprocessAdapter) validateArgs(args []string, scratch string) error {
	checkedArgs := argsBeforePrompt(args)
	if containsFlag(checkedArgs, "--add-dir") || containsFlag(checkedArgs, "--search") {
		return fmt.Errorf("%w: unsafe tool-enabling flag present", ErrUnsafeSubprocessConfig)
	}
	if flagValue(checkedArgs, "--sandbox") == "danger-full-access" {
		return fmt.Errorf("%w: unsafe sandbox mode", ErrUnsafeSubprocessConfig)
	}
	switch a.kind {
	case subprocessClaude:
		required := map[string]string{
			"--output-format": "stream-json",
			"--tools":         "",
			"--mcp-config":    "{}",
		}
		for flag, value := range required {
			got, ok := flagValueOK(checkedArgs, flag)
			if !ok || got != value {
				return fmt.Errorf("%w: missing %s %q", ErrUnsafeSubprocessConfig, flag, value)
			}
		}
		for _, flag := range []string{"--bare", "--print", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
			if !containsFlag(checkedArgs, flag) {
				return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
			}
		}
	case subprocessCodex:
		if len(checkedArgs) == 0 || checkedArgs[0] != "exec" {
			return fmt.Errorf("%w: codex_cli must use exec", ErrUnsafeSubprocessConfig)
		}
		if flagValue(checkedArgs, "--sandbox") != "read-only" {
			return fmt.Errorf("%w: codex_cli must use read-only sandbox", ErrUnsafeSubprocessConfig)
		}
		if flagValue(checkedArgs, "--cd") != scratch {
			return fmt.Errorf("%w: codex_cli must use scratch cwd", ErrUnsafeSubprocessConfig)
		}
		for _, flag := range []string{"--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"} {
			if !containsFlag(checkedArgs, flag) {
				return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
			}
		}
	}
	return nil
}

type subprocessStream struct {
	mu        sync.Mutex
	sessionID string
	result    subprocessResult

	cancel       context.CancelFunc
	done         chan struct{}
	logMu        sync.Mutex
	logFile      *os.File
	cleanup      func() error
	processGroup *processGroup
}

type subprocessResult struct {
	response Response
	err      error
}

func (s *subprocessStream) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *subprocessStream) Wait(ctx context.Context) (Response, error) {
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

func (s *subprocessStream) run(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderr io.Reader) {
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if s.logFile != nil {
			_, _ = io.Copy(subprocessLogWriter{stream: s}, stderr)
			return
		}
		_, _ = io.Copy(io.Discard, stderr)
	}()

	scanResult := s.scanStdout(stdout)
	waitErr := cmd.Wait()
	<-stderrDone

	result := subprocessResult{response: scanResult.response}
	switch {
	case scanResult.err != nil:
		result.err = scanResult.err
	case ctx.Err() != nil:
		result.err = ctx.Err()
	case waitErr != nil:
		result.err = waitErr
	case len(scanResult.response.StructuredOutput) == 0:
		result.err = errors.New("llm subprocess: no structured output")
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

type scanResult struct {
	response Response
	err      error
}

func (s *subprocessStream) scanStdout(stdout io.Reader) scanResult {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var result scanResult
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		s.writeLog(append(line, '\n'))
		event, err := parseSubprocessEvent(line)
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
		if len(event.structuredOutput) > 0 {
			result.response.StructuredOutput = event.structuredOutput
		}
		result.response.Usage = mergeUsage(result.response.Usage, event.usage)
	}
	if err := scanner.Err(); err != nil && result.err == nil {
		result.err = err
	}
	return result
}

type subprocessLogWriter struct {
	stream *subprocessStream
}

func (w subprocessLogWriter) Write(p []byte) (int, error) {
	w.stream.logMu.Lock()
	defer w.stream.logMu.Unlock()
	if w.stream.logFile == nil {
		return len(p), nil
	}
	return w.stream.logFile.Write(p)
}

func (s *subprocessStream) writeLog(p []byte) {
	if s.logFile == nil {
		return
	}
	_, _ = subprocessLogWriter{stream: s}.Write(p)
}

func (s *subprocessStream) closeLog() {
	if s.logFile == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	closeSubprocessLog(s.logFile)
}

func (s *subprocessStream) setSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == "" {
		s.sessionID = id
	}
}

type subprocessEvent struct {
	sessionID        string
	structuredOutput []byte
	usage            Usage
	toolUse          bool
}

const maxToolUseScanDepth = 64

func parseSubprocessEvent(line []byte) (subprocessEvent, error) {
	var decoded any
	if err := json.Unmarshal(line, &decoded); err != nil {
		return subprocessEvent{}, fmt.Errorf("llm subprocess: malformed JSONL event: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return subprocessEvent{}, fmt.Errorf("llm subprocess: malformed JSONL event: %w", err)
	}
	eventType := rawString(raw, "type")
	eventName := rawString(raw, "name")
	event := subprocessEvent{
		toolUse: eventIndicatesToolUse(eventType) || eventIndicatesToolUse(eventName) || valueIndicatesToolUse(decoded),
		usage:   parseUsage(raw),
	}
	if id := rawString(raw, "session_id"); id != "" {
		event.sessionID = id
	} else if id := rawString(raw, "thread_id"); id != "" {
		event.sessionID = id
	} else if strings.Contains(strings.ToLower(eventType), "session") {
		event.sessionID = rawString(raw, "id")
	}
	if output := rawStructuredOutput(raw); len(output) > 0 {
		event.structuredOutput = output
	}
	return event, nil
}

func rawString(raw map[string]json.RawMessage, key string) string {
	value, ok := raw[key]
	if !ok {
		return ""
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil {
		return ""
	}
	return out
}

func rawStructuredOutput(raw map[string]json.RawMessage) []byte {
	if value, ok := raw["structured_output"]; ok {
		return rawJSONValue(value)
	}
	if value, ok := raw["structuredOutput"]; ok {
		return rawJSONValue(value)
	}
	if value, ok := raw["response"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err == nil {
			if output, ok := nested["structured_output"]; ok {
				return rawJSONValue(output)
			}
			if output, ok := nested["structuredOutput"]; ok {
				return rawJSONValue(output)
			}
		}
	}
	return nil
}

func rawJSONValue(value json.RawMessage) []byte {
	var asString string
	if err := json.Unmarshal(value, &asString); err == nil {
		return []byte(asString)
	}
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return append([]byte(nil), value...)
}

func eventIndicatesToolUse(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "tool_use") ||
		strings.Contains(normalized, "tool_call") ||
		strings.Contains(normalized, "function_call")
}

func valueIndicatesToolUse(value any) bool {
	return valueIndicatesToolUseAtDepth(value, 0)
}

func valueIndicatesToolUseAtDepth(value any, depth int) bool {
	if depth > maxToolUseScanDepth {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		return mapIndicatesToolUse(typed, depth)
	case []any:
		for _, item := range typed {
			if valueIndicatesToolUseAtDepth(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func mapIndicatesToolUse(raw map[string]any, depth int) bool {
	for key, value := range raw {
		normalized := strings.ToLower(key)
		if normalized == "tool_use" || normalized == "tool_call" || normalized == "function_call" {
			return toolUseFieldIndicatesUse(value)
		}
		if (normalized == "type" || normalized == "name") && eventIndicatesToolUse(fmt.Sprint(value)) {
			return true
		}
		if valueIndicatesToolUseAtDepth(value, depth+1) {
			return true
		}
	}
	return false
}

func toolUseFieldIndicatesUse(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return false
	}
}

func parseUsage(raw map[string]json.RawMessage) Usage {
	value, ok := raw["usage"]
	if !ok {
		return Usage{}
	}
	var usageRaw map[string]json.RawMessage
	if err := json.Unmarshal(value, &usageRaw); err != nil {
		return Usage{}
	}
	return Usage{
		TokensIn:    rawIntPtr(usageRaw, "tokens_in"),
		TokensOut:   rawIntPtr(usageRaw, "tokens_out"),
		CacheRead:   rawIntPtr(usageRaw, "cache_read"),
		CacheCreate: rawIntPtr(usageRaw, "cache_create"),
		CostUSD:     rawFloatPtr(usageRaw, "cost_usd"),
	}
}

func rawIntPtr(raw map[string]json.RawMessage, key string) *int {
	value, ok := raw[key]
	if !ok || string(value) == "null" {
		return nil
	}
	var out int
	if err := json.Unmarshal(value, &out); err != nil {
		return nil
	}
	return &out
}

func rawFloatPtr(raw map[string]json.RawMessage, key string) *float64 {
	value, ok := raw[key]
	if !ok || string(value) == "null" {
		return nil
	}
	var out float64
	if err := json.Unmarshal(value, &out); err != nil {
		return nil
	}
	return &out
}

func mergeUsage(current Usage, next Usage) Usage {
	if next.TokensIn != nil {
		current.TokensIn = next.TokensIn
	}
	if next.TokensOut != nil {
		current.TokensOut = next.TokensOut
	}
	if next.CacheRead != nil {
		current.CacheRead = next.CacheRead
	}
	if next.CacheCreate != nil {
		current.CacheCreate = next.CacheCreate
	}
	if next.CostUSD != nil {
		current.CostUSD = next.CostUSD
	}
	return current
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) string {
	value, _ := flagValueOK(args, flag)
	return value
}

func flagValueOK(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func argsBeforePrompt(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[:i]
		}
	}
	return args
}

func validateScratchDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("%w: scratch dir is not empty", ErrUnsafeSubprocessConfig)
	}
	return abs, nil
}

func defaultScratchDir() (string, func() error, error) {
	dir, err := os.MkdirTemp("", "codereview-llm-*")
	if err != nil {
		return "", nil, err
	}
	return dir, func() error { return os.RemoveAll(dir) }, nil
}

func openSubprocessLog(path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	// #nosec G304 -- LogPath is an explicit caller-selected artifact path for tailable adapter logs.
	return os.Create(path)
}

func closeSubprocessLog(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
