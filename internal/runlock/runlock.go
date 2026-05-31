// Package runlock provides fail-fast advisory file locks for live review runs.
package runlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

var (
	// ErrHeld means another process already holds the requested lock.
	ErrHeld = errors.New("runlock: lock held")
	// ErrUnsupported means the current platform has no runlock implementation.
	ErrUnsupported = errors.New("runlock: unsupported platform")
)

// Lock is a held advisory file lock.
type Lock struct {
	mu       sync.Mutex
	file     *os.File
	released bool
}

// Acquire creates path's parent directory and takes a non-blocking exclusive
// advisory lock on path.
func Acquire(path string) (*Lock, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("runlock: path is required")
	}
	if err := platformSupported(); err != nil {
		return nil, err
	}
	// #nosec G703 -- runlock intentionally creates the caller-selected lock directory.
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("runlock: create lock dir: %w", err)
	}
	// #nosec G304 G703 -- runlock intentionally opens the caller-selected lock file.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("runlock: open lock file: %w", err)
	}
	if err := lockFile(file); err != nil {
		closeErr := file.Close()
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("runlock: close unheld lock file: %w", closeErr))
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release unlocks and closes the lock file. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	file := l.file
	l.file = nil
	if file == nil {
		return nil
	}
	return errors.Join(unlockFile(file), file.Close())
}
