//go:build !windows

package llm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type processGroup struct{}

func newProcessGroup(cmd *exec.Cmd) (*processGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processGroup{}, nil
}

func (g *processGroup) afterStart(*exec.Cmd) error {
	return nil
}

func (g *processGroup) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("%w; direct process kill failed: %w", err, killErr)
		}
		return err
	}
	return nil
}

func (g *processGroup) close() error {
	return nil
}
