package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClassifyHTTPStatusTransient(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{529, true},
		{200, false},
		{400, false},
		{401, false},
		{404, false},
	}
	for _, tc := range cases {
		if got := classifyHTTPStatusTransient(tc.status); got != tc.want {
			t.Errorf("classifyHTTPStatusTransient(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestIsTransientCLIDetail(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"Overloaded", true},
		{"model is overloaded_error", true},
		{"rate limit exceeded", true},
		{"rate_limit_error", true},
		{"got HTTP 529", true},
		{"upstream returned 503", true},
		{"request timed out", true},
		{"connection reset by peer", true},
		{"service unavailable", true},
		{"invalid prompt", false},
		{"tool use error", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTransientCLIDetail(tc.detail); got != tc.want {
			t.Errorf("isTransientCLIDetail(%q) = %v, want %v", tc.detail, got, tc.want)
		}
	}
}

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
