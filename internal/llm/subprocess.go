package llm

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ScratchDirFactory creates an empty working directory and returns its cleanup.
type ScratchDirFactory func() (string, func() error, error)

// LaunchedProcess is shared process-launch state for subprocess adapters.
type LaunchedProcess struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	logFile      *os.File
	processGroup *processGroup
}

// LaunchProcess starts an adapter subprocess with shared cancellation, logging,
// and platform-specific process-group handling.
func LaunchProcess(ctx context.Context, command string, args []string, dir string, env []string, timeout time.Duration, logPath string, cleanup func() error, withStdin bool) (*LaunchedProcess, error) {
	var procCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		procCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		procCtx, cancel = context.WithCancel(ctx)
	}
	// #nosec G204 -- adapters intentionally launch configured CLI binaries after validating adapter-owned safety args.
	cmd := exec.CommandContext(procCtx, command, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	procGroup, err := newProcessGroup(cmd)
	if err != nil {
		cancel()
		_ = cleanup()
		return nil, err
	}
	cmd.Cancel = func() error { return procGroup.kill(cmd) }

	var stdin io.WriteCloser
	if withStdin {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			cancel()
			_ = procGroup.close()
			_ = cleanup()
			return nil, err
		}
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
	logFile, err := openSubprocessLog(logPath)
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

	process := &LaunchedProcess{
		ctx:          procCtx,
		cancel:       cancel,
		cmd:          cmd,
		stdin:        stdin,
		stdout:       stdout,
		stderr:       stderr,
		logFile:      logFile,
		processGroup: procGroup,
	}
	if err := procGroup.afterStart(cmd); err != nil {
		process.Abort(cleanup)
		return nil, err
	}
	return process, nil
}

// Context returns the process context.
func (p *LaunchedProcess) Context() context.Context { return p.ctx }

// Command returns the started command.
func (p *LaunchedProcess) Command() *exec.Cmd { return p.cmd }

// Stdin returns the optional process stdin pipe.
func (p *LaunchedProcess) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the process stdout pipe.
func (p *LaunchedProcess) Stdout() io.ReadCloser { return p.stdout }

// Stderr returns the process stderr pipe.
func (p *LaunchedProcess) Stderr() io.ReadCloser { return p.stderr }

// Abort terminates a launched process and releases all launch resources.
func (p *LaunchedProcess) Abort(cleanup func() error) {
	p.cancel()
	_ = p.processGroup.kill(p.cmd)
	go func() { _, _ = io.Copy(io.Discard, p.stdout) }()
	go func() { _, _ = io.Copy(io.Discard, p.stderr) }()
	_ = p.cmd.Wait()
	closeSubprocessLog(p.logFile)
	_ = p.processGroup.close()
	_ = cleanup()
}

type streamResult struct {
	response Response
	err      error
}

// BaseStream provides shared cancellation, result, logging, and cleanup state.
type BaseStream struct {
	mu        sync.Mutex
	sessionID string
	result    streamResult

	cancel       context.CancelFunc
	done         chan struct{}
	logMu        sync.Mutex
	logFile      *os.File
	cleanup      func() error
	processGroup *processGroup
}

// NewProcessStream creates stream state owned by a launched subprocess.
func NewProcessStream(process *LaunchedProcess, cleanup func() error) BaseStream {
	return BaseStream{
		cancel:       process.cancel,
		done:         make(chan struct{}),
		logFile:      process.logFile,
		cleanup:      cleanup,
		processGroup: process.processGroup,
	}
}

// NewBaseStream creates stream state for a non-subprocess adapter.
func NewBaseStream(cancel context.CancelFunc) BaseStream {
	return BaseStream{cancel: cancel, done: make(chan struct{})}
}

// SessionID returns the first provider session ID reported by the stream.
func (s *BaseStream) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Wait waits for the adapter result or context cancellation.
func (s *BaseStream) Wait(ctx context.Context) (Response, error) {
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

func (s *BaseStream) Write(p []byte) (int, error) {
	if !s.HasLog() {
		return len(p), nil
	}
	return s.WithLogFile(func(file *os.File) (int, error) { return file.Write(p) })
}

// WithLogFile serializes a write operation against the optional adapter log.
func (s *BaseStream) WithLogFile(write func(*os.File) (int, error)) (int, error) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile == nil {
		return 0, nil
	}
	return write(s.logFile)
}

// HasLog reports whether the stream has an adapter log.
func (s *BaseStream) HasLog() bool { return s.logFile != nil }

// WriteLog appends bytes to the optional adapter log.
func (s *BaseStream) WriteLog(p []byte) {
	if s.HasLog() {
		_, _ = s.Write(p)
	}
}

// CloseLog closes the optional adapter log.
func (s *BaseStream) CloseLog() {
	if s.logFile == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	closeSubprocessLog(s.logFile)
}

// SetSessionID records the first non-empty provider session ID.
func (s *BaseStream) SetSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == "" {
		s.sessionID = id
	}
}

// Cancel cancels the adapter operation.
func (s *BaseStream) Cancel() { s.cancel() }

// CloseProcessGroup releases platform-specific process-group resources.
func (s *BaseStream) CloseProcessGroup() {
	if s.processGroup != nil {
		_ = s.processGroup.close()
	}
}

// Cleanup runs the stream cleanup once.
func (s *BaseStream) Cleanup() {
	if s.cleanup == nil {
		return
	}
	_ = s.cleanup()
	s.cleanup = nil
}

// Finish publishes a stream result and wakes waiters.
func (s *BaseStream) Finish(response Response, err error) {
	s.mu.Lock()
	s.result = streamResult{response: response, err: err}
	s.mu.Unlock()
	close(s.done)
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
