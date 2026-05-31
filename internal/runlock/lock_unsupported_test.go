//go:build !unix && !windows

package runlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformReturnsErrUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "run.lock")

	lock, err := Acquire(path)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Acquire unsupported error = %v, want ErrUnsupported", err)
	}
	if lock != nil {
		t.Fatalf("Acquire unsupported lock = %#v, want nil", lock)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported Acquire stat error = %v, want lock file absent", err)
	}
}
