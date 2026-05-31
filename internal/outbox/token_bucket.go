package outbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TokenBucket is a goroutine-safe per-host posting limiter.
type TokenBucket struct {
	interval time.Duration
	burst    int

	mu      sync.Mutex
	buckets map[string]*bucketState
}

type bucketState struct {
	tokens int
	last   time.Time
}

// NewTokenBucket creates a per-host token bucket.
func NewTokenBucket(interval time.Duration, burst int) (*TokenBucket, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("outbox: interval must be positive")
	}
	if burst <= 0 {
		return nil, fmt.Errorf("outbox: burst must be positive")
	}
	return &TokenBucket{
		interval: interval,
		burst:    burst,
		buckets:  make(map[string]*bucketState),
	}, nil
}

// Wait blocks until a token is available for host or ctx is canceled.
func (b *TokenBucket) Wait(ctx context.Context, host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("outbox: host is required")
	}
	for {
		wait := b.takeOrWait(host)
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *TokenBucket) takeOrWait(host string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	state := b.buckets[host]
	if state == nil {
		state = &bucketState{tokens: b.burst, last: now}
		b.buckets[host] = state
	}

	if elapsed := now.Sub(state.last); elapsed >= b.interval {
		added := int(elapsed / b.interval)
		state.tokens += added
		if state.tokens > b.burst {
			state.tokens = b.burst
		}
		state.last = state.last.Add(time.Duration(added) * b.interval)
	}

	if state.tokens > 0 {
		state.tokens--
		return 0
	}
	wait := b.interval - now.Sub(state.last)
	if wait <= 0 {
		wait = b.interval
	}
	return wait
}
