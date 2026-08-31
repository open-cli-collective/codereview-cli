package llmadapters

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

const (
	piRPCPromptID               = "prompt-1"
	piRPCSystemPrompt           = "You are a strict JSON API for code review structured output. Return exactly one JSON object that matches the requested schema. Do not include markdown fences, prose, explanations, or leading/trailing text. The first byte of your final answer must be { and the last byte must be }."
	piRPCReviewerSystemPrompt   = piRPCSystemPrompt + " Inspect the disposable repository only through the CR-owned cr_read, cr_search, cr_list, and cr_diff tools. Invoke cr_diff before cr_read, cr_search, or cr_list so the review starts from the pinned change. If cr_diff fails, record that exact tool failure as a constraint before inspecting allowed head files. These tools are read-only; do not request shell, write, edit, or any other tool."
	piRPCReviewerToolTimeout    = 15 * time.Second
	piRPCReviewerToolNames      = "cr_read,cr_search,cr_list,cr_diff"
	piRPCToolEvidenceReserve    = 256
	piRPCToolDiagnosticMaxRunes = 128
	piRPCPreflightTimeout       = 5 * time.Second
	piRPCPreflightOutputBytes   = 64 * 1024
	piRPCPreflightRegistration  = "codereview-pi-reviewer-tools-registered cr_read,cr_search,cr_list,cr_diff"
)

// ErrPiRPCIncompatible reports that the installed Pi runtime cannot enforce
// the bounded reviewer tool contract.
var ErrPiRPCIncompatible = errors.New("llm pi rpc: incompatible Pi runtime")

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
	preflightMu       sync.Mutex
	preflightReady    bool
}

var _ llm.Adapter = (*PiRPCAdapter)(nil)
var _ llm.ReviewerWorkspaceCapable = (*PiRPCAdapter)(nil)

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
	// Same defaulting as the subprocess adapters: an unbounded task can hang a
	// whole review on one stuck worker. Negative disables the bound explicitly.
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultLLMTaskTimeout
	}
	return &PiRPCAdapter{
		command:           command,
		commandArgsPrefix: append([]string(nil), opts.commandArgsPrefix...),
		env:               append([]string(nil), opts.Env...),
		timeout:           timeout,
		scratchDirFactory: factory,
		fastModeModels:    append([]string(nil), opts.FastModeModels...),
	}
}

// Name returns the adapter name.
func (a *PiRPCAdapter) Name() string { return "pi_rpc" }

// ReviewerWorkspaceMode reports Pi's CR-owned, read-only inspection boundary.
func (a *PiRPCAdapter) ReviewerWorkspaceMode() ReviewerWorkspaceMode {
	return ReviewerWorkspacePermissionBounded
}

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
	if req.ReviewerWorkspace != nil {
		if err := a.ensureReviewerRuntime(ctx); err != nil {
			return nil, err
		}
	}
	scratch, cleanup, workDir, extensionPath, err := a.prepareInvocation(req)
	if err != nil {
		return nil, err
	}
	if cleanup == nil {
		cleanup = func() error { return nil }
	}
	args, err := a.buildArgs(req, extensionPath)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	if err := a.validateArgs(args, req, extensionPath); err != nil {
		_ = cleanup()
		return nil, err
	}

	execArgs := append(append([]string(nil), a.commandArgsPrefix...), args...)
	env := append(os.Environ(), a.env...)
	if req.ReviewerWorkspace != nil {
		env = append(env, req.ReviewerWorkspace.Env...)
		env, err = reviewerInvocationEnv(env, scratch)
		if err != nil {
			_ = cleanup()
			return nil, err
		}
	}
	process, err := launchProcess(ctx, a.command, execArgs, workDir, env, a.timeout, req.LogPath, cleanup, true)
	if err != nil {
		return nil, err
	}
	if err := writePiRPCPrompt(process.Stdin(), req.Prompt); err != nil {
		process.Abort(cleanup)
		return nil, err
	}

	stream := &piRPCStream{
		baseStream:         llm.NewProcessStream(process, cleanup),
		stdin:              process.Stdin(),
		allowReviewerTools: req.ReviewerWorkspace != nil,
		logBytesLeft:       -1,
	}
	if req.ReviewerWorkspace != nil {
		stream.toolEvidenceBytesLeft = min(piRPCToolEvidenceReserve, req.ReviewerWorkspace.MaxToolOutputBytes)
		stream.logBytesLeft = req.ReviewerWorkspace.MaxToolOutputBytes - stream.toolEvidenceBytesLeft
	}
	go stream.run(process.Context(), process.Command(), process.Stdout(), process.Stderr())
	return stream, nil
}

func (a *PiRPCAdapter) ensureReviewerRuntime(ctx context.Context) error {
	a.preflightMu.Lock()
	defer a.preflightMu.Unlock()
	if a.preflightReady {
		return nil
	}
	if err := a.preflightReviewerRuntime(ctx); err != nil {
		return err
	}
	a.preflightReady = true
	return nil
}

func (a *PiRPCAdapter) preflightReviewerRuntime(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, piRPCPreflightTimeout)
	defer cancel()
	preflightDir, err := os.MkdirTemp("", "codereview-pi-preflight-*")
	if err != nil {
		return fmt.Errorf("%w: create empty preflight directory: %w", ErrPiRPCIncompatible, err)
	}
	defer func() { _ = os.RemoveAll(preflightDir) }()
	repoDir := filepath.Join(preflightDir, "repo")
	scratchDir := filepath.Join(preflightDir, "scratch")
	for _, dir := range []string{repoDir, scratchDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("%w: create preflight %s directory: %w", ErrPiRPCIncompatible, filepath.Base(dir), err)
		}
	}
	diffPath := filepath.Join(preflightDir, "diff.patch")
	if err := os.WriteFile(diffPath, []byte("preflight diff\n"), 0o600); err != nil {
		return fmt.Errorf("%w: write preflight diff: %w", ErrPiRPCIncompatible, err)
	}
	configPath := filepath.Join(scratchDir, "review-tools.json")
	config, err := json.Marshal(map[string]any{
		"repo_dir":         repoDir,
		"diff_path":        diffPath,
		"allowed_files":    []string{},
		"max_output_bytes": piRPCPreflightOutputBytes,
		"timeout_ms":       piRPCReviewerToolTimeout.Milliseconds(),
	})
	if err != nil {
		return fmt.Errorf("%w: marshal preflight tool config: %w", ErrPiRPCIncompatible, err)
	}
	if err := os.WriteFile(configPath, append(config, '\n'), 0o600); err != nil {
		return fmt.Errorf("%w: write preflight tool config: %w", ErrPiRPCIncompatible, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: locate CR executable: %w", ErrPiRPCIncompatible, err)
	}
	extensionPath := filepath.Join(scratchDir, "cr-review-tools.mjs")
	extension := piRPCReviewerExtension(executable, configPath, repoDir, piRPCPreflightOutputBytes, piRPCReviewerToolTimeout, piRPCPreflightRegistration)
	if err := os.WriteFile(extensionPath, []byte(extension), 0o600); err != nil {
		return fmt.Errorf("%w: write preflight extension: %w", ErrPiRPCIncompatible, err)
	}
	args := append(append([]string(nil), a.commandArgsPrefix...),
		"--mode", "rpc",
		"--system-prompt", piRPCReviewerSystemPrompt,
		"--no-builtin-tools",
		"--tools", piRPCReviewerToolNames,
		"--extension", extensionPath,
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--no-approve",
		"--no-session",
	)
	cmd := exec.CommandContext(ctx, a.command, args...) // #nosec G204 -- adapter command and fixed help argument come from trusted runtime configuration.
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(), a.env...)
	cmd.Stdin = strings.NewReader(`{"id":"state-1","type":"get_state"}` + "\n")
	stdout := &boundedPiRPCPreflightCapture{remaining: piRPCPreflightOutputBytes / 2}
	stderr := &boundedPiRPCPreflightCapture{remaining: piRPCPreflightOutputBytes / 2}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: help preflight timed out: %w", ErrPiRPCIncompatible, ctx.Err())
		}
		return fmt.Errorf("%w: help preflight failed: %w", ErrPiRPCIncompatible, err)
	}
	if !piRPCPreflightReady(stdout.String(), stderr.String()) {
		return fmt.Errorf("%w: reviewer extension preflight did not return required state and registration evidence", ErrPiRPCIncompatible)
	}
	return nil
}

func piRPCPreflightReady(stdout, stderr string) bool {
	return piRPCPreflightReceivedState(stdout) && piRPCPreflightReceivedRegistration(stderr)
}

func piRPCPreflightReceivedState(output string) bool {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4*1024), piRPCPreflightOutputBytes)
	for scanner.Scan() {
		var response struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err == nil && response.ID == "state-1" && response.Success {
			return true
		}
	}
	return false
}

func piRPCPreflightReceivedRegistration(output string) bool {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4*1024), piRPCPreflightOutputBytes)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == piRPCPreflightRegistration {
			return true
		}
	}
	return false
}

type boundedPiRPCPreflightCapture struct {
	mu        sync.Mutex
	remaining int
	data      strings.Builder
}

func (w *boundedPiRPCPreflightCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeBytes := len(p)
	if writeBytes > w.remaining {
		writeBytes = w.remaining
	}
	if writeBytes > 0 {
		_, _ = w.data.Write(p[:writeBytes])
		w.remaining -= writeBytes
	}
	return len(p), nil
}

func (w *boundedPiRPCPreflightCapture) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

func (a *PiRPCAdapter) prepareInvocation(req Request) (scratch string, cleanup func() error, workDir, extensionPath string, err error) {
	if req.ReviewerWorkspace == nil {
		scratch, cleanup, err = a.scratchDirFactory()
		if err != nil {
			return "", nil, "", "", err
		}
		scratch, err = validateScratchDir(scratch)
		if err != nil {
			_ = cleanup()
			return "", nil, "", "", err
		}
		return scratch, cleanup, scratch, "", nil
	}
	workspace := req.ReviewerWorkspace
	for label, dir := range map[string]string{"repo": workspace.RepoDir, "scratch": workspace.ScratchDir} {
		if strings.TrimSpace(dir) == "" || !filepath.IsAbs(dir) {
			return "", nil, "", "", fmt.Errorf("%w: reviewer %s dir must be absolute", ErrUnsafeSubprocessConfig, label)
		}
		info, statErr := os.Lstat(filepath.Clean(dir))
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", nil, "", "", fmt.Errorf("%w: reviewer %s dir is not a real directory", ErrUnsafeSubprocessConfig, label)
		}
	}
	if strings.TrimSpace(workspace.DiffPath) == "" || !filepath.IsAbs(workspace.DiffPath) || workspace.MaxToolOutputBytes <= 0 {
		return "", nil, "", "", fmt.Errorf("%w: reviewer fixed diff and positive output limit are required", ErrUnsafeSubprocessConfig)
	}
	scratch, err = os.MkdirTemp(workspace.ScratchDir, "pi-rpc-")
	if err != nil {
		return "", nil, "", "", fmt.Errorf("llm pi rpc: create reviewer invocation scratch: %w", err)
	}
	cleanup = func() error { return os.RemoveAll(scratch) }
	configPath := filepath.Join(scratch, "review-tools.json")
	config := map[string]any{
		"repo_dir":         workspace.RepoDir,
		"diff_path":        workspace.DiffPath,
		"allowed_files":    append([]string(nil), workspace.AllowedFiles...),
		"max_output_bytes": workspace.MaxToolOutputBytes,
		"timeout_ms":       piRPCReviewerToolTimeout.Milliseconds(),
	}
	data, marshalErr := json.Marshal(config)
	if marshalErr != nil {
		_ = cleanup()
		return "", nil, "", "", marshalErr
	}
	if writeErr := os.WriteFile(configPath, append(data, '\n'), 0o600); writeErr != nil {
		_ = cleanup()
		return "", nil, "", "", fmt.Errorf("llm pi rpc: write reviewer tool config: %w", writeErr)
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		_ = cleanup()
		return "", nil, "", "", fmt.Errorf("llm pi rpc: locate CR executable: %w", executableErr)
	}
	extensionPath = filepath.Join(scratch, "cr-review-tools.mjs")
	extension := piRPCReviewerExtension(executable, configPath, workspace.RepoDir, workspace.MaxToolOutputBytes, piRPCReviewerToolTimeout, "")
	if writeErr := os.WriteFile(extensionPath, []byte(extension), 0o600); writeErr != nil {
		_ = cleanup()
		return "", nil, "", "", fmt.Errorf("llm pi rpc: write reviewer extension: %w", writeErr)
	}
	return scratch, cleanup, workspace.RepoDir, extensionPath, nil
}

func (a *PiRPCAdapter) buildArgs(req Request, extensionPath string) ([]string, error) {
	systemPrompt := piRPCSystemPrompt
	args := []string{
		"--mode", "rpc",
		"--system-prompt", systemPrompt,
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-session",
	}
	if req.ReviewerWorkspace == nil {
		args = append(args, "--no-tools")
	} else {
		args[3] = piRPCReviewerSystemPrompt
		args = append(args, "--no-builtin-tools", "--no-context-files", "--no-approve", "--tools", piRPCReviewerToolNames, "--extension", extensionPath)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--thinking", req.Effort)
	}
	return args, nil
}

func (a *PiRPCAdapter) validateArgs(args []string, req Request, extensionPath string) error {
	allowedFlags := map[string]bool{
		"--mode":                true,
		"--system-prompt":       true,
		"--no-tools":            false,
		"--no-builtin-tools":    false,
		"--no-context-files":    false,
		"--no-approve":          false,
		"--tools":               true,
		"--no-extensions":       false,
		"--extension":           true,
		"--no-skills":           false,
		"--no-prompt-templates": false,
		"--no-themes":           false,
		"--no-session":          false,
		"--model":               true,
		"--thinking":            true,
	}
	if err := validateAllowedFlags("pi_rpc", args, allowedFlags); err != nil {
		return err
	}
	for flag := range allowedFlags {
		if countPiRPCFlag(args, flag) > 1 {
			return fmt.Errorf("%w: duplicate %s", ErrUnsafeSubprocessConfig, flag)
		}
	}
	if flagValue(args, "--mode") != "rpc" {
		return fmt.Errorf("%w: pi_rpc must use rpc mode", ErrUnsafeSubprocessConfig)
	}
	for _, flag := range []string{"--system-prompt", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-session"} {
		if !containsFlag(args, flag) {
			return fmt.Errorf("%w: missing %s", ErrUnsafeSubprocessConfig, flag)
		}
	}
	wantSystemPrompt := piRPCSystemPrompt
	if req.ReviewerWorkspace == nil {
		if !containsFlag(args, "--no-tools") || containsFlag(args, "--no-builtin-tools") || containsFlag(args, "--tools") || containsFlag(args, "-t") || containsFlag(args, "--extension") {
			return fmt.Errorf("%w: non-reviewer pi_rpc must disable all tools", ErrUnsafeSubprocessConfig)
		}
	} else {
		wantSystemPrompt = piRPCReviewerSystemPrompt
		if containsFlag(args, "--no-tools") || !containsFlag(args, "--no-builtin-tools") || !containsFlag(args, "--no-context-files") || !containsFlag(args, "--no-approve") || flagValue(args, "--tools") != piRPCReviewerToolNames || flagValue(args, "--extension") != extensionPath {
			return fmt.Errorf("%w: reviewer pi_rpc must load only the CR-owned extension", ErrUnsafeSubprocessConfig)
		}
	}
	if containsFlag(args, "--system-prompt") && flagValue(args, "--system-prompt") != wantSystemPrompt {
		return fmt.Errorf("%w: pi_rpc system prompt mismatch", ErrUnsafeSubprocessConfig)
	}
	return nil
}

func countPiRPCFlag(args []string, flag string) int {
	count := 0
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			count++
		}
	}
	return count
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
	stdin                 io.Closer
	allowReviewerTools    bool
	logLimitMu            sync.Mutex
	logBytesLeft          int
	logCapped             bool
	toolEvidenceBytesLeft int
	diffToolStarted       int
	diffToolCompleted     int
	diffToolFailed        int
	diffToolError         string
}

func (s *piRPCStream) run(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderr io.Reader) {
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if s.HasLog() {
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
	evidence := s.reviewerToolEvidence()
	s.writeReviewerToolEvidence(evidence)
	scanResult.response.ReviewerToolEvidence = evidence

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
	s.Cancel()
	s.CloseProcessGroup()
	s.CloseLog()
	s.Cleanup()
	s.Finish(result.response, result.err)
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
			s.Cancel()
			result.err = err
			return result
		}
		if event.sessionID != "" {
			s.SetSessionID(event.sessionID)
		}
		if event.toolUse && (!s.allowReviewerTools || !isAllowedPiRPCReviewerTool(event.toolName)) {
			s.Cancel()
			result.err = ErrToolUse
			return result
		}
		s.observeReviewerToolEvent(event)
		if event.responseFailure != "" {
			s.Cancel()
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

type piRPCLogWriter struct{ stream *piRPCStream }

func (w piRPCLogWriter) Write(p []byte) (int, error) {
	w.stream.writeLog(p)
	return len(p), nil
}

func (s *piRPCStream) writeLog(p []byte) {
	s.logLimitMu.Lock()
	defer s.logLimitMu.Unlock()
	if s.logBytesLeft < 0 {
		s.WriteLog(p)
		return
	}
	if s.logCapped || s.logBytesLeft == 0 {
		return
	}
	const marker = "warning: reviewer RPC/stderr log cap reached; further logs truncated\n"
	if len(p) < s.logBytesLeft {
		s.WriteLog(p)
		s.logBytesLeft -= len(p)
		return
	}
	bodyBytes := s.logBytesLeft - len(marker)
	if bodyBytes > len(p) {
		bodyBytes = len(p)
	}
	if bodyBytes > 0 {
		s.WriteLog(p[:bodyBytes])
	}
	markerBytes := s.logBytesLeft - max(bodyBytes, 0)
	if markerBytes > len(marker) {
		markerBytes = len(marker)
	}
	if markerBytes > 0 {
		s.WriteLog([]byte(marker[:markerBytes]))
	}
	s.logBytesLeft = 0
	s.logCapped = true
}

func (s *piRPCStream) observeReviewerToolEvent(event piRPCEvent) {
	if !s.allowReviewerTools || event.toolName != "cr_diff" {
		return
	}
	if event.toolStarted {
		s.diffToolStarted++
	}
	if event.toolCompleted {
		s.diffToolCompleted++
	}
	if event.toolFailed {
		s.diffToolFailed++
	}
	if event.toolError != "" && s.diffToolError == "" {
		s.diffToolError = event.toolError
	}
}

func (s *piRPCStream) reviewerToolEvidence() *llm.ReviewerToolEvidence {
	if !s.allowReviewerTools {
		return nil
	}
	status := llm.DiffToolStatusNotInvoked
	switch {
	case s.diffToolFailed > 0:
		status = llm.DiffToolStatusFailed
	case s.diffToolCompleted > 0:
		status = llm.DiffToolStatusSucceeded
	case s.diffToolStarted > 0:
		status = llm.DiffToolStatusIncomplete
	}
	evidence := &llm.ReviewerToolEvidence{DiffStatus: status}
	if status == llm.DiffToolStatusFailed {
		evidence.DiffDiagnostic = boundPiRPCToolError(s.diffToolError)
	}
	return evidence
}

func (s *piRPCStream) writeReviewerToolEvidence(evidence *llm.ReviewerToolEvidence) {
	if evidence == nil || s.toolEvidenceBytesLeft <= 0 {
		return
	}
	line := fmt.Sprintf("codereview-pi-tool-evidence tool=cr_diff status=%s started=%d completed=%d failed=%d", evidence.DiffStatus, s.diffToolStarted, s.diffToolCompleted, s.diffToolFailed)
	if evidence.DiffStatus == llm.DiffToolStatusFailed {
		line += fmt.Sprintf(" error=%q", evidence.DiffDiagnostic)
	}
	evidenceBytes := []byte(line + "\n")
	s.logLimitMu.Lock()
	defer s.logLimitMu.Unlock()
	writeBytes := min(len(evidenceBytes), s.toolEvidenceBytesLeft)
	if writeBytes > 0 {
		s.WriteLog(evidenceBytes[:writeBytes])
		s.toolEvidenceBytesLeft -= writeBytes
	}
}

func boundPiRPCToolError(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) <= piRPCToolDiagnosticMaxRunes {
		return value
	}
	return string(runes[:piRPCToolDiagnosticMaxRunes-3]) + "..."
}

func piRPCToolError(raw json.RawMessage) string {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return ""
	}
	if message := firstRawString(result, "error", "message"); message != "" {
		return message
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(result["content"], &content); err != nil {
		return ""
	}
	for _, block := range content {
		if message := firstRawString(block, "text", "content"); message != "" {
			return message
		}
	}
	return ""
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
	delete(event, "message")
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
	toolName         string
	toolStarted      bool
	toolCompleted    bool
	toolFailed       bool
	toolError        string
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
	if event.toolUse {
		event.toolName = firstRawString(raw, "toolName", "tool_name", "name")
		switch eventType {
		case "tool_execution_start":
			event.toolStarted = true
		case "tool_execution_end":
			event.toolCompleted = true
			event.toolFailed = rawBool(raw, "isError")
			var resultRaw map[string]json.RawMessage
			if err := json.Unmarshal(raw["result"], &resultRaw); err == nil && rawBool(resultRaw, "isError") {
				event.toolFailed = true
			}
			if event.toolFailed {
				event.toolError = piRPCToolError(raw["result"])
				if event.toolError == "" {
					event.toolError = "tool execution failed"
				}
			}
		}
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

func isAllowedPiRPCReviewerTool(name string) bool {
	switch name {
	case "cr_read", "cr_search", "cr_list", "cr_diff":
		return true
	default:
		return false
	}
}

func piRPCReviewerExtension(executable, configPath, repoDir string, maxOutputBytes int, timeout time.Duration, registrationMarker string) string {
	quoted := func(value string) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	markerStatement := ""
	if registrationMarker != "" {
		markerStatement = "  process.stderr.write(" + quoted(registrationMarker+"\n") + ");\n"
	}
	return `import { spawn } from "node:child_process";

const executable = ` + quoted(executable) + `;
const configPath = ` + quoted(configPath) + `;
const repoDir = ` + quoted(repoDir) + `;
const maxOutputBytes = ` + strconv.Itoa(maxOutputBytes) + `;
const timeoutMs = ` + strconv.FormatInt(timeout.Milliseconds(), 10) + `;

function runTool(tool, params, signal) {
  return new Promise((resolve) => {
    const child = spawn(executable, ["__pi-review-tool", "--config", configPath], {
      cwd: repoDir,
      env: process.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = Buffer.alloc(0);
    let stderr = Buffer.alloc(0);
    const append = (current, chunk) => Buffer.concat([current, chunk]).subarray(0, maxOutputBytes);
    child.stdout.on("data", (chunk) => { stdout = append(stdout, chunk); });
    child.stderr.on("data", (chunk) => { stderr = append(stderr, chunk); });
    const kill = () => {
      try {
        child.kill("SIGKILL");
      } catch {}
    };
    const timer = setTimeout(kill, timeoutMs);
    signal?.addEventListener("abort", kill, { once: true });
    child.on("error", (error) => {
      clearTimeout(timer);
      resolve({ content: [{ type: "text", text: String(error) }], details: {}, isError: true });
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", kill);
      const text = code === 0 ? stdout.toString("utf8") : (stderr.toString("utf8") || ("tool exited " + code));
      resolve({ content: [{ type: "text", text }], details: {}, isError: code !== 0 });
    });
    child.stdin.end(JSON.stringify({ ...params, tool }));
  });
}

export default function (pi) {
  let diffAttempted = false;
  const headTool = (tool) => (_id, params, signal) => {
    if (!diffAttempted) return Promise.resolve({ content: [{ type: "text", text: "cr_diff must be invoked before inspecting repository files" }], details: {}, isError: true });
    return runTool(tool, params, signal);
  };
  pi.registerTool({ name: "cr_read", label: "CR Read", description: "Read one repository file. Use offset and limit with next_offset from ranged responses to continue.", parameters: { type: "object", properties: { path: { type: "string" }, offset: { type: "integer", minimum: 0 }, limit: { type: "integer", minimum: 0 } }, required: ["path"], additionalProperties: false }, execute: headTool("cr_read") });
  pi.registerTool({ name: "cr_search", label: "CR Search", description: "Search repository text literally.", parameters: { type: "object", properties: { query: { type: "string" }, path: { type: "string" } }, required: ["query"], additionalProperties: false }, execute: headTool("cr_search") });
  pi.registerTool({ name: "cr_list", label: "CR List", description: "List repository files.", parameters: { type: "object", properties: { path: { type: "string" } }, additionalProperties: false }, execute: headTool("cr_list") });
  pi.registerTool({ name: "cr_diff", label: "CR Diff", description: "Read the fixed pinned review diff. Use offset and limit with next_offset from ranged responses to continue.", parameters: { type: "object", properties: { offset: { type: "integer", minimum: 0 }, limit: { type: "integer", minimum: 0 } }, additionalProperties: false }, execute: (_id, params, signal) => { diffAttempted = true; return runTool("cr_diff", params, signal); } });
` + markerStatement + `
}
`
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
