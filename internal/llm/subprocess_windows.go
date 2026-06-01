//go:build windows

package llm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

type processGroup struct {
	mu  sync.Mutex
	job windows.Handle
}

func newProcessGroup(cmd *exec.Cmd) (*processGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("llm subprocess: create windows job object: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	return &processGroup{job: job}, nil
}

func (g *processGroup) afterStart(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("llm subprocess: open windows process for job assignment: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(g.job, process); err != nil {
		return fmt.Errorf("llm subprocess: assign process to windows job object: %w", err)
	}
	return nil
}

func (g *processGroup) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	var jobErr error
	if g != nil {
		g.mu.Lock()
		job := g.job
		if job != 0 {
			err := windows.TerminateJobObject(job, 1)
			g.mu.Unlock()
			if err == nil {
				return nil
			}
			if !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
				jobErr = err
			}
		} else {
			g.mu.Unlock()
		}
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if jobErr != nil {
			return fmt.Errorf("windows job termination failed: %w; direct process kill failed: %w", jobErr, err)
		}
		return err
	}
	if jobErr != nil {
		return jobErr
	}
	return nil
}

func (g *processGroup) close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.job)
	g.job = 0
	return err
}
