package threadrespond

import (
	"context"
	"errors"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
)

// Mirrors pipeline's TestExecuteHoldsActiveRunsLockForRunDuration: while a
// respond run is in flight an exclusive Acquire on the active-runs lock must
// be refused, and it must succeed again once Run returns. The probe rides
// the provider call hook, which fires inside the run after the acquisition.
func TestRunHoldsActiveRunsLockForRunDuration(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		humanOnlyThread("thread-human", "main.go", 10, fixture.human),
	})
	probed := errors.New("probe never fired")
	probeFired := false
	fixture.provider.SetCallHook(func(gitprovider.Operation) {
		if !probeFired {
			probeFired = true
			_, probed = runlock.Acquire(fixture.layout.ActiveRunsLock())
		}
	})

	_, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !probeFired {
		t.Fatal("provider probe never fired")
	}
	if !errors.Is(probed, runlock.ErrHeld) {
		t.Fatalf("Acquire during Run = %v, want ErrHeld", probed)
	}
	released, err := runlock.Acquire(fixture.layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("Acquire after Run returned: %v", err)
	}
	if err := released.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// The guard is fail-open through the injected seam: a composition root (or
// platform) whose shared acquisition fails still gets a working respond run.
func TestRunProceedsWhenSharedLockUnavailable(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		humanOnlyThread("thread-human", "main.go", 10, fixture.human),
	})
	opts := fixture.options()
	acquireCalls := 0
	opts.AcquireShared = func(string) (Lock, error) {
		acquireCalls++
		return nil, errors.New("shared locks unavailable on this platform")
	}

	result, err := Run(ctx, opts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run with unavailable shared lock: %v", err)
	}
	if acquireCalls != 1 {
		t.Fatalf("AcquireShared calls = %d, want 1 (seam not used)", acquireCalls)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
}
