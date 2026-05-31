//go:build windows

package runlock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const (
	windowsLockOffsetLow  uint32 = 0
	windowsLockOffsetHigh uint32 = 0
	windowsLockLengthLow  uint32 = 1
	windowsLockLengthHigh uint32 = 0
)

func platformSupported() error {
	return nil
}

func windowsOverlapped() windows.Overlapped {
	return windows.Overlapped{Offset: windowsLockOffsetLow, OffsetHigh: windowsLockOffsetHigh}
}

func lockFile(file *os.File) error {
	overlapped := windowsOverlapped()
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		windowsLockLengthLow,
		windowsLockLengthHigh,
		&overlapped,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%w: %s", ErrHeld, file.Name())
	}
	return fmt.Errorf("runlock: acquire %s: %w", file.Name(), err)
}

func unlockFile(file *os.File) error {
	overlapped := windowsOverlapped()
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, windowsLockLengthLow, windowsLockLengthHigh, &overlapped); err != nil {
		return fmt.Errorf("runlock: release %s: %w", file.Name(), err)
	}
	return nil
}
