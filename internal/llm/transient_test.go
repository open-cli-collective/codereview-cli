package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepBackoffZeroBaseReturnsImmediately(t *testing.T) {
	p := retryPolicy{MaxRetries: 3, Base: 0, Multiplier: 2, Cap: 30 * time.Second}
	start := time.Now()
	if err := sleepBackoff(context.Background(), p, 2); err != nil {
		t.Fatalf("sleepBackoff: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("sleepBackoff with zero base slept %v, want immediate", elapsed)
	}
}

func TestSleepBackoffCancelledContextReturnsErr(t *testing.T) {
	p := retryPolicy{MaxRetries: 3, Base: 10 * time.Second, Multiplier: 2, Cap: 30 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sleepBackoff(ctx, p, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepBackoff canceled = %v, want context.Canceled", err)
	}
}
