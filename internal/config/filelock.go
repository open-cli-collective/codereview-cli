package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/runlock"
)

// ErrChanged means another writer changed a loaded draft before it was saved.
var ErrChanged = errors.New("config: changed since loading; retry the command")

// lockFile serializes config writes across processes. Loaded drafts are also
// checked by Save so waiting for a writer cannot silently overwrite its edits.
func lockFile(ctx context.Context, path string) (*runlock.Lock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	// Keep persistent advisory locks outside the removable config directory.
	lockPath := filepath.Join(cache, "codereview", "config-locks", fmt.Sprintf("%x.lock", sha256.Sum256([]byte(abs))))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		lock, err := runlock.Acquire(lockPath)
		if !errors.Is(err, runlock.ErrHeld) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
