package pipeline

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"

	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// A run older than the live retention window is the discriminator: while
// the active-runs lock is held the prune must skip and the run survives;
// once the lock is free the same call deletes it.
func TestTryPruneRetentionSkipsWhileActiveRunsLockHeld(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	stale, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           "github.com_owner_repo_1",
		PRURL:           "https://github.com/owner/repo/pull/1",
		RunID:           "stale-live-run",
		SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow().Add(-100 * 24 * time.Hour),
		ArtifactPath:    filepath.Join(layout.DataRoot, "runs", "github.com_owner_repo_1", "a", "b", "home", "run-stale"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	opts := Options{Layout: layout, Store: store, Now: fixedNow}

	held, err := runlock.AcquireShared(layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}
	if err := tryPruneRetention(context.Background(), opts); err != nil {
		t.Fatalf("tryPruneRetention while lock held: %v, want skip", err)
	}
	if _, err := store.GetRun(ctx, stale.RunID); err != nil {
		t.Fatalf("stale run pruned while the lock was held: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := tryPruneRetention(context.Background(), opts); err != nil {
		t.Fatalf("tryPruneRetention with lock free: %v", err)
	}
	if _, err := store.GetRun(ctx, stale.RunID); err == nil {
		t.Fatalf("stale run survived an unguarded prune, want deletion")
	}
}

func TestTryPruneRetentionHonorsManualOnly(t *testing.T) {
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	opts := Options{Layout: layout, RetentionManualOnly: true}
	if err := tryPruneRetention(context.Background(), opts); err != nil {
		t.Fatalf("tryPruneRetention manual-only: %v, want nil without touching the store", err)
	}
}

// The run-duration half of the guard: while execute is in flight, an
// exclusive Acquire on the active-runs lock must be refused, and it must
// succeed again once the run returns. The probe rides the first GitCommand
// call, which fires inside execute after the guard acquisition.
func TestExecuteHoldsActiveRunsLockForRunDuration(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	probed := errors.New("probe never fired")
	probeFired := false
	_, runErr := DryRun(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   layout,
		GitCommand: func(gitCtx context.Context, dir string, args ...string) ([]byte, error) {
			// The workbench "init" is the first git call inside execute
			// after the guard; earlier calls (repo-root resolution, agent
			// catalogs) run against the real git binary.
			if !probeFired && len(args) > 0 && args[0] == "init" {
				probeFired = true
				_, probed = runlock.Acquire(layout.ActiveRunsLock())
				return nil, errors.New("probe abort")
			}
			cmd := exec.CommandContext(gitCtx, "git", args...)
			cmd.Dir = dir
			return cmd.CombinedOutput()
		},
	}, req)
	if !probeFired {
		t.Fatalf("git probe never fired; DryRun err = %v", runErr)
	}
	if !errors.Is(probed, runlock.ErrHeld) {
		t.Fatalf("Acquire during execute = %v, want ErrHeld", probed)
	}
	released, err := runlock.Acquire(layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("Acquire after execute returned: %v", err)
	}
	if err := released.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
