//go:build unix

package runlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func platformSupported() error {
	return nil
}

func lockFile(file *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("%w: %s", ErrHeld, file.Name())
		}
		return fmt.Errorf("runlock: acquire %s: %w", file.Name(), err)
	}
	return nil
}

func unlockFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("runlock: release %s: %w", file.Name(), err)
	}
	return nil
}
