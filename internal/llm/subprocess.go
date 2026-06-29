package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	subprocessClaude  subprocessKind = "claude_cli"
	subprocessCodex   subprocessKind = "codex_cli"
	logBytesUnlimited                = -1

	claudeBGPromptFilename  = "cr-prompt.txt"
	claudeBGResultFilename  = "cr-result.json"
	claudeBGPollInterval    = 500 * time.Millisecond
	claudeBGSessionIDGrace  = 10 * time.Second
	claudeBGControlTimeout  = 10 * time.Second
	claudeBGInspectMaxDepth = 8
	// Stale metadata GC only touches jobs whose state has been idle for at least a day.
	// Shorter windows risk racing manually resumed or slow-to-settle background sessions.
	claudeBGStaleJobAge = 24 * time.Hour
)

var (
	claudeBGJobIDDirectRE = regexp.MustCompile(`backgrounded\s+.\s+([A-Za-z0-9_-]+)`)
	claudeBGJobIDAttachRE = regexp.MustCompile(`\bclaude\s+attach\s+([A-Za-z0-9_-]+)\b`)
	claudeBGJobIDRE       = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	claudeBGUserCacheDir  = os.UserCacheDir
	claudeBGTerminalState = map[string]bool{
		"done":    true,
		"failed":  true,
		"waiting": true,
		"stopped": true,
		"blocked": true,
	}
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
func (a *SubprocessAdapter) SupportsResume() bool { return a.kind == subprocessClaude }

// ReviewerWorkspaceMode reports the subprocess adapter's reviewer workspace mode.
func (a *SubprocessAdapter) ReviewerWorkspaceMode() ReviewerWorkspaceMode {
	switch a.kind {
	case subprocessClaude:
		return ReviewerWorkspacePermissionBounded
	case subprocessCodex:
		return ReviewerWorkspaceWrite
	default:
		return ReviewerWorkspaceNone
	}
}

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
	if a.kind == subprocessClaude {
		return a.startClaudeBG(ctx, req, "")
	}
	if a.kind == subprocessCodex && !a.allowBestEffortNoTools {
		return nil, fmt.Errorf("%w: codex_cli requires AllowBestEffortNoTools until Codex exposes an all-tools-disabled flag", ErrUnsafeSubprocessConfig)
	}
	return a.startJSONLSubprocess(ctx, req)
}

func (a *SubprocessAdapter) startJSONLSubprocess(ctx context.Context, req Request) (Stream, error) {
	scratch, cleanup, err := a.invocationScratchDir(req)
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
	if err := a.validateArgs(args, scratch, req); err != nil {
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
	cmd.Env = a.processEnv(req)
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
		logBytesLeft: reviewerWorkspaceLogBytes(req),
		allowToolUse: req.ReviewerWorkspace != nil,
		cleanup:      cleanup,
		processGroup: procGroup,
	}
	go stream.run(procCtx, cmd, stdout, stderr)
	return stream, nil
}

func (a *SubprocessAdapter) startClaudeBG(ctx context.Context, req Request, resumeSessionID string) (Stream, error) {
	scratch, cleanup, err := a.invocationScratchDir(req)
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
	if err := writeClaudeBGPromptFile(req.Prompt, scratch); err != nil {
		_ = cleanup()
		return nil, err
	}
	args, err := a.buildArgsForSession(req, scratch, resumeSessionID)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	if err := a.validateArgs(args, scratch, req); err != nil {
		_ = cleanup()
		return nil, err
	}
	workDir, err := claudeBGWorkingDir(a.env)
	if err != nil {
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
	launchDir := workDir
	if req.ReviewerWorkspace != nil {
		launchDir = req.ReviewerWorkspace.RepoDir
	}
	cmd.Dir = launchDir
	procGroup, err := newProcessGroup(cmd)
	if err != nil {
		cancel()
		_ = cleanup()
		return nil, err
	}
	cmd.Cancel = func() error {
		return procGroup.kill(cmd)
	}
	cmd.Env = a.processEnv(req)
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
		logBytesLeft: reviewerWorkspaceLogBytes(req),
		cleanup:      cleanup,
		processGroup: procGroup,
	}
	go stream.runClaudeBG(procCtx, a, cmd, stdout, stderr, scratch, workDir)
	return stream, nil
}

func (a *SubprocessAdapter) processEnv(req Request) []string {
	env := append(os.Environ(), a.env...)
	if req.ReviewerWorkspace != nil {
		env = append(env, req.ReviewerWorkspace.Env...)
	}
	return env
}

// Resume starts a subprocess request from an existing provider session when the
// adapter supports provider-side session reuse.
func (a *SubprocessAdapter) Resume(ctx context.Context, sessionID string, req Request) (Stream, error) {
	sessionID = strings.TrimSpace(sessionID)
	if a.kind != subprocessClaude {
		return nil, fmt.Errorf("llm subprocess: resume unsupported for %s", a.kind)
	}
	if sessionID == "" {
		// A blank session id is treated as a fresh start so callers can pass
		// through optional resume state without special-casing the first run.
		return a.Start(ctx, req)
	}
	return a.startClaudeBG(ctx, req, sessionID)
}

func (a *SubprocessAdapter) buildArgs(req Request, scratch string) ([]string, error) {
	return a.buildArgsForSession(req, scratch, "")
}

func (a *SubprocessAdapter) buildArgsForSession(req Request, scratch string, resumeSessionID string) ([]string, error) {
	switch a.kind {
	case subprocessClaude:
		tools := "Read,Write"
		if req.ReviewerWorkspace != nil {
			tools = "Read,Write,Bash"
		}
		args := []string{
			"--bg",
			"--tools", tools,
			"--permission-mode", "acceptEdits",
			"--add-dir", scratch,
		}
		if workspace := req.ReviewerWorkspace; workspace != nil {
			args = append(args, "--add-dir", workspace.RepoDir)
		}
		if resumeSessionID != "" {
			args = append(args, "--resume", resumeSessionID)
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.Effort != "" {
			args = append(args, "--effort", req.Effort)
		}
		return append(args, "--", claudeBGPositionalPrompt(scratch)), nil
	case subprocessCodex:
		workspace := req.ReviewerWorkspace
		args := subprocessCodexBaseArgs("read-only", scratch)
		if workspace != nil {
			args = subprocessCodexBaseArgs("workspace-write", workspace.RepoDir)
			args = append(args, "--add-dir", scratch)
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

func subprocessCodexBaseArgs(sandbox, cwd string) []string {
	return []string{
		"exec",
		"--json",
		"--ephemeral",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"--ignore-rules",
		"--sandbox", sandbox,
		"--cd", cwd,
	}
}

func (a *SubprocessAdapter) validateArgs(args []string, scratch string, req Request) error {
	checkedArgs := argsBeforePrompt(args)
	if containsFlag(checkedArgs, "--search") {
		return fmt.Errorf("%w: unsafe tool-enabling flag present", ErrUnsafeSubprocessConfig)
	}
	if flagValue(checkedArgs, "--sandbox") == "danger-full-access" {
		return fmt.Errorf("%w: unsafe sandbox mode", ErrUnsafeSubprocessConfig)
	}
	switch a.kind {
	case subprocessClaude:
		if len(checkedArgs)+2 != len(args) || args[len(checkedArgs)] != "--" || strings.TrimSpace(args[len(checkedArgs)+1]) == "" {
			return fmt.Errorf("%w: claude_cli must receive a positional background prompt", ErrUnsafeSubprocessConfig)
		}
		if err := validateAllowedFlags("claude_cli", checkedArgs, map[string]bool{
			"--bg":              false,
			"--tools":           true,
			"--permission-mode": true,
			"--add-dir":         true,
			"--resume":          true,
			"--model":           true,
			"--effort":          true,
		}); err != nil {
			return err
		}
		if !containsFlag(checkedArgs, "--bg") {
			return fmt.Errorf("%w: claude_cli must use background jobs", ErrUnsafeSubprocessConfig)
		}
		requiredTools := "Read,Write"
		if req.ReviewerWorkspace != nil {
			requiredTools = "Read,Write,Bash"
		}
		required := map[string]string{
			"--tools":           requiredTools,
			"--permission-mode": "acceptEdits",
		}
		for flag, value := range required {
			got, ok := flagValueOK(checkedArgs, flag)
			if !ok || got != value {
				return fmt.Errorf("%w: missing %s %q", ErrUnsafeSubprocessConfig, flag, value)
			}
		}
		if err := validateClaudeAddDirs(checkedArgs, scratch, req.ReviewerWorkspace); err != nil {
			return err
		}
	case subprocessCodex:
		workspace := req.ReviewerWorkspace
		if workspace == nil && containsFlag(checkedArgs, "--add-dir") {
			return fmt.Errorf("%w: unsafe tool-enabling flag present", ErrUnsafeSubprocessConfig)
		}
		if len(checkedArgs) == 0 || checkedArgs[0] != "exec" {
			return fmt.Errorf("%w: codex_cli must use exec", ErrUnsafeSubprocessConfig)
		}
		wantSandbox := "read-only"
		wantCWD := scratch
		if workspace != nil {
			wantSandbox = "workspace-write"
			wantCWD = workspace.RepoDir
		}
		if flagValue(checkedArgs, "--sandbox") != wantSandbox {
			return fmt.Errorf("%w: codex_cli must use %s sandbox", ErrUnsafeSubprocessConfig, wantSandbox)
		}
		if flagValue(checkedArgs, "--cd") != wantCWD {
			return fmt.Errorf("%w: codex_cli must use configured cwd", ErrUnsafeSubprocessConfig)
		}
		if workspace != nil {
			addDir, ok := flagValueOK(checkedArgs, "--add-dir")
			if !ok || !sameCleanPath(addDir, scratch) {
				return fmt.Errorf("%w: codex_cli must add only the invocation scratch dir", ErrUnsafeSubprocessConfig)
			}
		}
		for _, flag := range []string{"--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"} {
			if !containsFlag(checkedArgs, flag) {
				return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
			}
		}
	}
	return nil
}

func validateClaudeAddDirs(args []string, scratch string, workspace *ReviewerWorkspaceRequest) error {
	addDirs := flagValues(args, "--add-dir")
	if workspace == nil {
		if len(addDirs) != 1 || !sameCleanPath(addDirs[0], scratch) {
			return fmt.Errorf("%w: claude_cli must restrict add-dir to scratch dir", ErrUnsafeSubprocessConfig)
		}
		return nil
	}
	if strings.TrimSpace(workspace.RepoDir) == "" {
		return fmt.Errorf("%w: reviewer workspace repo dir is required", ErrUnsafeSubprocessConfig)
	}
	if len(addDirs) != 2 {
		return fmt.Errorf("%w: claude_cli reviewer workspace requires exactly scratch and repo add-dir roots", ErrUnsafeSubprocessConfig)
	}
	want := []string{scratch, workspace.RepoDir}
	seen := make([]bool, len(want))
	for _, addDir := range addDirs {
		matched := -1
		for i, wantDir := range want {
			if sameCleanPath(addDir, wantDir) {
				matched = i
				break
			}
		}
		if matched == -1 {
			return fmt.Errorf("%w: claude_cli reviewer workspace add-dir %q is outside scratch and repo roots", ErrUnsafeSubprocessConfig, addDir)
		}
		if seen[matched] {
			return fmt.Errorf("%w: claude_cli reviewer workspace duplicate add-dir %q", ErrUnsafeSubprocessConfig, addDir)
		}
		seen[matched] = true
	}
	for i, ok := range seen {
		if !ok {
			return fmt.Errorf("%w: claude_cli reviewer workspace missing add-dir %q", ErrUnsafeSubprocessConfig, want[i])
		}
	}
	return nil
}

func (a *SubprocessAdapter) invocationScratchDir(req Request) (string, func() error, error) {
	workspace := req.ReviewerWorkspace
	if workspace == nil {
		return a.scratchDirFactory()
	}
	if err := RequireReviewerWorkspace(a); err != nil {
		return "", nil, err
	}
	root := strings.TrimSpace(workspace.ScratchDir)
	if root == "" {
		return "", nil, fmt.Errorf("%w: reviewer workspace scratch dir is required", ErrUnsafeSubprocessConfig)
	}
	if strings.TrimSpace(workspace.RepoDir) == "" {
		return "", nil, fmt.Errorf("%w: reviewer workspace repo dir is required", ErrUnsafeSubprocessConfig)
	}
	for _, dir := range []string{workspace.ScratchDir, workspace.TempDir, workspace.CacheDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, fmt.Errorf("llm subprocess: create reviewer workspace dir: %w", err)
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("llm subprocess: create reviewer workspace scratch root: %w", err)
	}
	scratch, err := os.MkdirTemp(root, "llm-subprocess-*")
	if err != nil {
		return "", nil, fmt.Errorf("llm subprocess: create reviewer workspace scratch dir: %w", err)
	}
	return scratch, func() error { return os.RemoveAll(scratch) }, nil
}

type subprocessStream struct {
	mu        sync.Mutex
	sessionID string
	result    subprocessResult

	cancel       context.CancelFunc
	done         chan struct{}
	logMu        sync.Mutex
	logFile      *os.File
	logBytesLeft int
	logCapped    bool
	allowToolUse bool
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

func (s *subprocessStream) runClaudeBG(ctx context.Context, adapter *SubprocessAdapter, cmd *exec.Cmd, stdout io.Reader, stderr io.Reader, scratch string, workDir string) {
	stderrDone := make(chan bytes.Buffer, 1)
	go func() {
		var stderrBuf bytes.Buffer
		if s.logFile != nil {
			_, _ = io.Copy(io.MultiWriter(subprocessLogWriter{stream: s}, &stderrBuf), stderr)
		} else {
			_, _ = io.Copy(&stderrBuf, stderr)
		}
		stderrDone <- stderrBuf
	}()

	launchStdout, readErr := io.ReadAll(stdout)
	if len(launchStdout) > 0 {
		s.writeLog(launchStdout)
	}
	waitErr := cmd.Wait()
	stderrBuf := <-stderrDone

	result := subprocessResult{}
	jobID := ""
	if readErr != nil {
		result.err = readErr
	} else if ctx.Err() != nil {
		result.err = ctx.Err()
	} else if waitErr != nil {
		detail := strings.TrimSpace(stderrBuf.String())
		if detail == "" {
			detail = strings.TrimSpace(string(launchStdout))
		}
		if detail != "" {
			launchErr := fmt.Errorf("llm subprocess: Claude bg launch failed: %w: %s", waitErr, detail)
			if isTransientCLIDetail(detail) {
				result.err = fmt.Errorf("%w: %w", ErrTransient, launchErr)
			} else {
				result.err = launchErr
			}
		} else {
			result.err = waitErr
		}
	} else {
		jobID = extractClaudeBGJobID(string(launchStdout))
		if jobID == "" {
			result.err = errors.New("llm subprocess: could not parse Claude background job id")
		}
	}

	if s.processGroup != nil {
		_ = s.processGroup.close()
	}

	if result.err == nil {
		var sessionID string
		result.response, sessionID, result.err = adapter.waitForClaudeBGResult(ctx, jobID, scratch)
		if sessionID != "" {
			s.setSessionID(sessionID)
		}
	}
	s.runCleanup()
	if jobID != "" {
		if err := adapter.cleanupClaudeBGJob(context.WithoutCancel(ctx), jobID, result.err, workDir); err != nil {
			s.writeCleanupWarning(err)
		}
	}

	s.cancel()
	s.closeLog()
	s.runCleanup()
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
		if event.toolUse && !s.allowToolUse {
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
	if w.stream.logBytesLeft == 0 {
		if !w.stream.logCapped {
			if _, err := w.stream.logFile.Write([]byte("warning: reviewer workspace tool output cap reached; further subprocess logs truncated\n")); err != nil {
				return 0, err
			}
			w.stream.logCapped = true
		}
		return len(p), nil
	}
	writeBytes := p
	if w.stream.logBytesLeft > 0 && len(writeBytes) > w.stream.logBytesLeft {
		writeBytes = writeBytes[:w.stream.logBytesLeft]
	}
	n, err := w.stream.logFile.Write(writeBytes)
	if w.stream.logBytesLeft > 0 {
		w.stream.logBytesLeft -= n
	}
	if w.stream.logBytesLeft == 0 && !w.stream.logCapped {
		if _, writeErr := w.stream.logFile.Write([]byte("warning: reviewer workspace tool output cap reached; further subprocess logs truncated\n")); writeErr != nil && err == nil {
			err = writeErr
		}
		w.stream.logCapped = true
	}
	if err != nil {
		return n, err
	}
	return len(p), nil
}

func (s *subprocessStream) writeLog(p []byte) {
	if s.logFile == nil {
		return
	}
	_, _ = subprocessLogWriter{stream: s}.Write(p)
}

func (s *subprocessStream) writeCleanupWarning(err error) {
	if err == nil {
		return
	}
	s.writeLog([]byte("warning: " + err.Error() + "\n"))
}

func (s *subprocessStream) runCleanup() {
	if s.cleanup == nil {
		return
	}
	_ = s.cleanup()
	s.cleanup = nil
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

func reviewerWorkspaceLogBytes(req Request) int {
	workspace := req.ReviewerWorkspace
	if workspace == nil {
		return logBytesUnlimited
	}
	if workspace.MaxToolOutputBytes == logBytesUnlimited {
		return logBytesUnlimited
	}
	if workspace.MaxToolOutputBytes < 0 {
		return 0
	}
	return workspace.MaxToolOutputBytes
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
	if value, ok := raw["item"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err == nil {
			if rawString(nested, "type") == "agent_message" {
				if text := rawString(nested, "text"); strings.TrimSpace(text) != "" {
					return []byte(text)
				}
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
		TokensIn:    firstRawIntPtr(usageRaw, "tokens_in", "tokensIn", "input_tokens", "inputTokens", "input", "promptTokens", "prompt_tokens"),
		TokensOut:   firstRawIntPtr(usageRaw, "tokens_out", "tokensOut", "output_tokens", "outputTokens", "output", "completionTokens", "completion_tokens"),
		CacheRead:   firstRawIntPtr(usageRaw, "cache_read", "cacheRead", "cached_input_tokens", "cachedInputTokens"),
		CacheCreate: firstRawIntPtr(usageRaw, "cache_create", "cacheCreate", "cache_write", "cacheWrite"),
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

func (a *SubprocessAdapter) waitForClaudeBGResult(ctx context.Context, jobID string, scratch string) (Response, string, error) {
	resultPaths := []string{
		filepath.Join(scratch, claudeBGResultFilename),
		filepath.Join(claudeConfigDirFromEnv(a.env), "jobs", jobID, "tmp", claudeBGResultFilename),
	}
	state, err := a.waitForClaudeBGState(ctx, jobID, resultPaths)
	if err != nil {
		return Response{}, extractClaudeBGSessionID(state), err
	}
	sessionID := extractClaudeBGSessionID(state)
	if sessionID == "" {
		sessionID, state = a.waitForClaudeBGSessionID(ctx, jobID, state)
	}
	if sessionID == "" {
		return Response{}, "", fmt.Errorf("llm subprocess: Claude background job completed without session id: %s", claudeBGStateDetail(state))
	}
	output, err := readFirstNonEmptyFile(resultPaths)
	if err != nil {
		return Response{}, sessionID, err
	}
	return Response{StructuredOutput: output, Usage: claudeBGTranscriptUsage(state)}, sessionID, nil
}

func (a *SubprocessAdapter) waitForClaudeBGState(ctx context.Context, jobID string, resultPaths []string) (map[string]any, error) {
	statePath := filepath.Join(claudeConfigDirFromEnv(a.env), "jobs", jobID, "state.json")
	lastState := map[string]any{"state": "starting", "job_id": jobID}
	ticker := time.NewTicker(claudeBGPollInterval)
	defer ticker.Stop()

	for {
		state, ok := readClaudeBGState(statePath)
		if ok {
			state["job_id"] = jobID
			lastState = state
			stateName := strings.TrimSpace(fmt.Sprint(state["state"]))
			if claudeBGIdleWithResultFile(state, resultPaths) {
				state["state"] = "done"
				state["result_file_ready_state"] = stateName
				return state, nil
			}
			if claudeBGTerminalState[stateName] {
				if stateName == "done" {
					return state, nil
				}
				detail := claudeBGStateDetail(state)
				jobErr := fmt.Errorf("llm subprocess: Claude background job %s: %s", stateName, detail)
				if isTransientCLIDetail(detail) {
					return state, fmt.Errorf("%w: %w", ErrTransient, jobErr)
				}
				return state, jobErr
			}
		}

		select {
		case <-ctx.Done():
			lastState["state"] = "timeout"
			if detail := claudeBGStateDetail(lastState); detail != "" && detail != "no detail" {
				return lastState, fmt.Errorf("llm subprocess: Claude background job timed out: %s: %w", detail, ctx.Err())
			}
			return lastState, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *SubprocessAdapter) waitForClaudeBGSessionID(ctx context.Context, jobID string, lastState map[string]any) (string, map[string]any) {
	statePath := filepath.Join(claudeConfigDirFromEnv(a.env), "jobs", jobID, "state.json")
	deadline := time.NewTimer(claudeBGSessionIDGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(claudeBGPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", lastState
		case <-deadline.C:
			return "", lastState
		case <-ticker.C:
			state, ok := readClaudeBGState(statePath)
			if !ok {
				continue
			}
			state["job_id"] = jobID
			lastState = state
			if sessionID := extractClaudeBGSessionID(state); sessionID != "" {
				return sessionID, state
			}
		}
	}
}

func readClaudeBGState(path string) (map[string]any, bool) {
	// #nosec G304 -- Claude bg state path is derived from adapter env and job id.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, false
	}
	return state, true
}

func claudeBGIdleWithResultFile(state map[string]any, resultPaths []string) bool {
	stateName := strings.TrimSpace(fmt.Sprint(state["state"]))
	if stateName != "working" {
		return false
	}
	tempo, hasTempo := state["tempo"]
	if !hasTempo || strings.TrimSpace(fmt.Sprint(tempo)) != "idle" {
		return false
	}
	if inFlight, ok := state["inFlight"].(map[string]any); ok {
		if numericStateValue(inFlight["tasks"]) != 0 || numericStateValue(inFlight["queued"]) != 0 {
			return false
		}
	}
	return anyNonEmptyFile(resultPaths)
}

func numericStateValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func anyNonEmptyFile(paths []string) bool {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && info.Size() > 0 {
			return true
		}
	}
	return false
}

func readFirstNonEmptyFile(paths []string) ([]byte, error) {
	for _, path := range paths {
		// #nosec G304 -- result paths are adapter-owned scratch/job tmp paths.
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, errors.New("llm subprocess: Claude background job wrote an empty result file")
		}
		return data, nil
	}
	return nil, errors.New("llm subprocess: Claude background job completed without writing result file")
}

func claudeBGStateDetail(state map[string]any) string {
	for _, key := range []string{"detail", "error", "message", "needs"} {
		if value := strings.TrimSpace(fmt.Sprint(state[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	if output, ok := state["output"].(map[string]any); ok {
		for _, key := range []string{"error", "message"} {
			if value := strings.TrimSpace(fmt.Sprint(output[key])); value != "" && value != "<nil>" {
				return value
			}
		}
	}
	return "no detail"
}

func extractClaudeBGSessionID(state map[string]any) string {
	for _, key := range []string{"sessionId", "session_id", "resumeSessionId", "resume_session_id"} {
		if value := strings.TrimSpace(fmt.Sprint(state[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	if output, ok := state["output"].(map[string]any); ok {
		for _, key := range []string{"sessionId", "session_id"} {
			if value := strings.TrimSpace(fmt.Sprint(output[key])); value != "" && value != "<nil>" {
				return value
			}
		}
	}
	return ""
}

func (a *SubprocessAdapter) cleanupClaudeBGJob(ctx context.Context, jobID string, resultErr error, workDir string) error {
	if !claudeBGJobIDRE.MatchString(jobID) {
		return fmt.Errorf("llm subprocess: invalid Claude background job id %q", jobID)
	}
	var cleanupErr error
	if resultErr != nil {
		if err := a.runClaudeBGControl(ctx, workDir, "stop", jobID); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if err := a.runClaudeBGControl(ctx, workDir, "rm", jobID); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := a.gcClaudeBGJobs(ctx, workDir); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func (a *SubprocessAdapter) runClaudeBGControl(ctx context.Context, workDir string, verb string, jobID string) error {
	stdout, stderr, err := a.runClaudeBGCommand(ctx, workDir, verb, jobID)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail != "" {
			return fmt.Errorf("llm subprocess: Claude bg cleanup %s failed: %w: %s", verb, err, detail)
		}
		return fmt.Errorf("llm subprocess: Claude bg cleanup %s failed: %w", verb, err)
	}
	return nil
}

func (a *SubprocessAdapter) runClaudeBGCommand(ctx context.Context, workDir string, args ...string) ([]byte, []byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, claudeBGControlTimeout)
	defer cancel()
	execArgs := append(append([]string(nil), a.commandArgsPrefix...), args...)
	// #nosec G204 -- control verbs target the adapter-owned Claude CLI binary and validated job id.
	cmd := exec.CommandContext(cmdCtx, a.command, execArgs...)
	cmd.Dir = workDir
	if len(a.env) > 0 {
		cmd.Env = append(os.Environ(), a.env...)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (a *SubprocessAdapter) gcClaudeBGJobs(ctx context.Context, workDir string) error {
	jobsDir := filepath.Join(claudeConfigDirFromEnv(a.env), "jobs")
	entries, err := os.ReadDir(jobsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("llm subprocess: Claude bg stale-job scan failed: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	now := time.Now()
	staleJobIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobID := strings.TrimSpace(entry.Name())
		if !claudeBGJobIDRE.MatchString(jobID) {
			continue
		}
		statePath := filepath.Join(jobsDir, jobID, "state.json")
		stateInfo, err := os.Stat(statePath)
		if err != nil || stateInfo.IsDir() || now.Sub(stateInfo.ModTime()) < claudeBGStaleJobAge {
			continue
		}
		staleJobIDs = append(staleJobIDs, jobID)
	}
	if len(staleJobIDs) == 0 {
		return nil
	}
	activeJobs, err := a.listActiveClaudeBGJobs(ctx, workDir)
	if err != nil {
		// Fail closed when the active job set is unavailable so stale GC cannot
		// remove a job that might still be running.
		return err
	}
	var cleanupErr error
	for _, jobID := range staleJobIDs {
		if activeJobs[jobID] {
			continue
		}
		jobDir := filepath.Join(jobsDir, jobID)
		statePath := filepath.Join(jobDir, "state.json")
		state, ok := readClaudeBGState(statePath)
		if !ok || !claudeBGOwnsJob(state, workDir) {
			continue
		}
		if err := a.removeStaleClaudeBGJobDir(ctx, workDir, jobID, jobDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func (a *SubprocessAdapter) removeStaleClaudeBGJobDir(ctx context.Context, workDir string, jobID string, jobDir string) error {
	if err := a.runClaudeBGControl(ctx, workDir, "rm", jobID); err != nil {
		if removeErr := os.RemoveAll(jobDir); removeErr != nil {
			return fmt.Errorf("llm subprocess: Claude bg stale metadata cleanup failed for %s: %w", jobID, errors.Join(err, removeErr))
		}
	}
	return nil
}

func (a *SubprocessAdapter) listActiveClaudeBGJobs(ctx context.Context, workDir string) (map[string]bool, error) {
	stdout, stderr, err := a.runClaudeBGCommand(ctx, workDir, "agents", "--json")
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail != "" {
			return nil, fmt.Errorf("llm subprocess: Claude bg active-agent discovery failed: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("llm subprocess: Claude bg active-agent discovery failed: %w", err)
	}
	activeJobs, err := parseClaudeBGActiveJobs(stdout)
	if err != nil {
		return nil, fmt.Errorf("llm subprocess: Claude bg active-agent discovery failed: %w", err)
	}
	return activeJobs, nil
}

func parseClaudeBGActiveJobs(data []byte) (map[string]bool, error) {
	var jsonRoot any
	if err := json.Unmarshal(data, &jsonRoot); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}
	activeJobs := map[string]bool{}
	collectClaudeBGActiveJobs(jsonRoot, 0, activeJobs)
	if len(activeJobs) == 0 && claudeBGJSONShapeNonEmpty(jsonRoot) {
		return nil, errors.New("unrecognized non-empty JSON shape")
	}
	return activeJobs, nil
}

func collectClaudeBGActiveJobs(value any, depth int, activeJobs map[string]bool) {
	if depth > claudeBGInspectMaxDepth {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectClaudeBGActiveJobs(item, depth+1, activeJobs)
		}
	case map[string]any:
		if jobID := claudeBGStateString(typed, "id", "jobId", "job_id"); jobID != "" && claudeBGJobIDRE.MatchString(jobID) {
			activeJobs[jobID] = true
		}
		for _, nested := range typed {
			collectClaudeBGActiveJobs(nested, depth+1, activeJobs)
		}
	}
}

func claudeBGJSONShapeNonEmpty(value any) bool {
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return claudeBGSprintedValue(typed) != ""
	}
}

func claudeBGOwnsJob(state map[string]any, workDir string) bool {
	if cwd := claudeBGStateString(state, "cwd"); cwd != "" && sameCleanPath(cwd, workDir) {
		return true
	}
	if claudeBGIntentOwnsJob(state["intent"]) {
		return true
	}
	if claudeBGRespawnFlagsOwnJob(state["respawnFlags"]) {
		return true
	}
	return false
}

func claudeBGIntentOwnsJob(value any) bool {
	for _, candidate := range flattenStateStrings(value, 0) {
		if strings.Contains(candidate, claudeBGPromptFilename) || strings.Contains(candidate, claudeBGResultFilename) || strings.Contains(candidate, "codereview-llm-") {
			return true
		}
	}
	return false
}

func claudeBGRespawnFlagsOwnJob(value any) bool {
	tokens := flattenCommandTokens(value)
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		// #nosec G101 -- this compares a known CLI flag name, not a secret token.
		if token == "--add-dir" && i+1 < len(tokens) && isCodereviewScratchDir(tokens[i+1]) {
			return true
		}
		if addDirPath, ok := strings.CutPrefix(token, "--add-dir="); ok && isCodereviewScratchDir(addDirPath) {
			return true
		}
	}
	return false
}

func flattenStateStrings(value any, depth int) []string {
	if depth > claudeBGInspectMaxDepth {
		return nil
	}
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, flattenStateStrings(item, depth+1)...)
		}
		return out
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenStateStrings(item, depth+1)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenStateStrings(item, depth+1)...)
		}
		return out
	default:
		return nil
	}
}

func flattenCommandTokens(value any) []string {
	stringsOnly := flattenStateStrings(value, 0)
	tokens := make([]string, 0, len(stringsOnly))
	for _, item := range stringsOnly {
		tokens = append(tokens, strings.Fields(item)...)
	}
	return tokens
}

func isCodereviewScratchDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	return strings.HasPrefix(filepath.Base(filepath.Clean(path)), "codereview-llm-")
}

func claudeBGStateString(state map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := claudeBGSprintedValue(state[key]); value != "" {
			return value
		}
	}
	return ""
}

func claudeBGSprintedValue(value any) string {
	stringValue := strings.TrimSpace(fmt.Sprint(value))
	if stringValue == "" || stringValue == "<nil>" {
		return ""
	}
	return stringValue
}

func writeClaudeBGPromptFile(prompt string, scratch string) error {
	promptPath := filepath.Join(scratch, claudeBGPromptFilename)
	resultPath := filepath.Join(scratch, claudeBGResultFilename)
	if err := os.WriteFile(promptPath, []byte(wrapClaudeBGPrompt(prompt, resultPath)), 0o600); err != nil {
		return fmt.Errorf("llm subprocess: writing Claude bg prompt file: %w", err)
	}
	return nil
}

func claudeBGPositionalPrompt(scratch string) string {
	promptPath := filepath.Join(scratch, claudeBGPromptFilename)
	resultPath := filepath.Join(scratch, claudeBGResultFilename)
	return fmt.Sprintf("Read %s and follow its instructions exactly. Use the Write tool to write the final structured result to %s. Do not answer in chat.", promptPath, resultPath)
}

func wrapClaudeBGPrompt(prompt string, resultPath string) string {
	return strings.TrimSpace(prompt) + "\n\n" + strings.TrimSpace(fmt.Sprintf(`codereview-cli completion contract:
- When the review is complete, use the Write tool to write the final structured JSON result to this exact file path: %s
- The file contents must be only the final JSON value requested by the review instructions.
- Do not wrap the JSON in markdown fences.
- Do not write any explanatory text before or after the JSON in that file.`, resultPath)) + "\n"
}

func extractClaudeBGJobID(output string) string {
	for _, re := range []*regexp.Regexp{claudeBGJobIDDirectRE, claudeBGJobIDAttachRE} {
		match := re.FindStringSubmatch(output)
		if len(match) == 2 && claudeBGJobIDRE.MatchString(match[1]) {
			return match[1]
		}
	}
	return ""
}

func claudeConfigDirFromEnv(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], "CLAUDE_CONFIG_DIR=") {
			if value := strings.TrimSpace(strings.TrimPrefix(env[i], "CLAUDE_CONFIG_DIR=")); value != "" {
				return value
			}
		}
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); value != "" {
		return value
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, ".claude")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".claude")
	}
	return ".claude"
}

func claudeBGWorkingDir(env []string) (string, error) {
	if value := envValue(env, "CR_CLAUDE_BG_WORK_DIR"); value != "" {
		return ensureClaudeBGWorkingDir(value)
	}
	if value := strings.TrimSpace(os.Getenv("CR_CLAUDE_BG_WORK_DIR")); value != "" {
		return ensureClaudeBGWorkingDir(value)
	}
	base, err := claudeBGUserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	return ensureClaudeBGWorkingDir(filepath.Join(base, "codereview-cli", "claude-bg-workdir"))
}

func ensureClaudeBGWorkingDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimSpace(strings.TrimPrefix(env[i], prefix))
		}
	}
	return ""
}

func sameCleanPath(left string, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil {
		left = leftAbs
	}
	if rightErr == nil {
		right = rightAbs
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func validateAllowedFlags(adapterName string, args []string, allowed map[string]bool) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flag := arg
		inlineValue := false
		if name, _, ok := strings.Cut(arg, "="); ok {
			flag = name
			inlineValue = true
		}
		hasValue, ok := allowed[flag]
		if !ok {
			return fmt.Errorf("%w: unexpected %s flag %s", ErrUnsafeSubprocessConfig, adapterName, arg)
		}
		if inlineValue {
			if !hasValue {
				return fmt.Errorf("%w: %s flag %s does not accept a value", ErrUnsafeSubprocessConfig, adapterName, flag)
			}
			continue
		}
		if hasValue {
			i++
		}
	}
	return nil
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

func flagValues(args []string, flag string) []string {
	var values []string
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			values = append(values, args[i+1])
			continue
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			values = append(values, value)
		}
	}
	return values
}

func flagValueOK(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			return value, true
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
