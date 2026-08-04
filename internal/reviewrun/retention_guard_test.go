package reviewrun

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// The live path's automatic retention must honor the active-runs lock the
// same way the dry-run pipeline does: a run older than the live retention
// window survives while the lock is held and is deleted once it is free.
func TestPruneRetentionSkipsWhileActiveRunsLockHeld(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	stale, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           "github.com_owner_repo_1",
		PRURL:           "https://github.com/owner/repo/pull/1",
		RunID:           "stale-live-run",
		SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		StartedAt:       now().Add(-100 * 24 * time.Hour),
		ArtifactPath:    filepath.Join(layout.DataRoot, "runs", "github.com_owner_repo_1", "a", "b", "home", "run-stale"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	opts := Options{Layout: layout, Store: store, Now: now}

	held, err := runlock.AcquireShared(layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}
	if err := pruneRetention(ctx, opts); err != nil {
		t.Fatalf("pruneRetention while lock held: %v, want skip", err)
	}
	if _, err := store.GetRun(ctx, stale.RunID); err != nil {
		t.Fatalf("stale run pruned while the lock was held: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := pruneRetention(ctx, opts); err != nil {
		t.Fatalf("pruneRetention with lock free: %v", err)
	}
	if _, err := store.GetRun(ctx, stale.RunID); err == nil {
		t.Fatalf("stale run survived an unguarded prune, want deletion")
	}
}
