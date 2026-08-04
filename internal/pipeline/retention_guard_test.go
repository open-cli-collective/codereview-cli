package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// tryPruneRetention with a nil Store is the discriminator here: the prune
// itself fails on a nil store, so a nil error proves the prune never ran.
func TestTryPruneRetentionSkipsWhileActiveRunsLockHeld(t *testing.T) {
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	opts := Options{Layout: layout}

	held, err := runlock.AcquireShared(layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}
	if err := tryPruneRetention(context.Background(), opts); err != nil {
		t.Fatalf("tryPruneRetention while lock held: %v, want skip", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	err = tryPruneRetention(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "store is required") {
		t.Fatalf("tryPruneRetention with lock free = %v, want prune to run and fail on the nil store", err)
	}
}

func TestTryPruneRetentionHonorsManualOnly(t *testing.T) {
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	opts := Options{Layout: layout, RetentionManualOnly: true}
	if err := tryPruneRetention(context.Background(), opts); err != nil {
		t.Fatalf("tryPruneRetention manual-only: %v, want nil without touching the store", err)
	}
}
