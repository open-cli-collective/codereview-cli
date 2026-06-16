package llm

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"
)

// ErrTransient marks a provider failure that is likely temporary (an
// overloaded model, a 5xx, a rate limit, or a transport timeout) and therefore
// safe to retry. Concrete adapters wrap their transport/model errors with this
// sentinel so callers can classify retryable outcomes without knowing wire
// details, mirroring the gitprovider ErrRetryable taxonomy.
var ErrTransient = errors.New("llm: transient provider error")

// transientCLIDetailSubstrings are lowercase substrings that mark a CLI
// adapter failure detail as a transient provider error worth retrying.
var transientCLIDetailSubstrings = []string{
	"overloaded_error",
	"overloaded",
	"rate limit",
	"rate_limit",
	"429",
	"500",
	"502",
	"503",
	"504",
	"529",
	"timed out",
	"timeout",
	"temporarily unavailable",
	"service unavailable",
	"connection reset",
}

// classifyHTTPStatusTransient reports whether an HTTP status code indicates a
// transient provider failure that is safe to retry.
func classifyHTTPStatusTransient(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504, 529:
		return true
	default:
		return false
	}
}

// isTransientCLIDetail reports whether a CLI adapter failure detail string
// matches a known transient provider error signature. The match is a
// case-insensitive substring scan.
func isTransientCLIDetail(detail string) bool {
	lowered := strings.ToLower(detail)
	for _, needle := range transientCLIDetailSubstrings {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// retryPolicy bounds exponential backoff for transient retries.
type retryPolicy struct {
	MaxRetries int
	Base       time.Duration
	Multiplier float64
	Cap        time.Duration
}

// defaultRetryPolicy retries transient failures up to 3 times with 1s, 2s, 4s
// full-jittered backoff, capped at 30s.
var defaultRetryPolicy = retryPolicy{
	MaxRetries: 3,
	Base:       1 * time.Second,
	Multiplier: 2,
	Cap:        30 * time.Second,
}

// activeRetryPolicy is the policy the retry loop consults. It is a package var
// so tests can override the backoff timing for fast, deterministic runs.
var activeRetryPolicy = defaultRetryPolicy

// sleepBackoff waits for the jittered backoff delay for the given attempt
// (0-based) or returns ctx.Err() if the context is canceled first. A
// non-positive Base disables sleeping entirely so tests can run without delay.
func sleepBackoff(ctx context.Context, p retryPolicy, attempt int) error {
	if p.Base <= 0 {
		return nil
	}
	delay := float64(p.Base)
	for i := 0; i < attempt; i++ {
		delay *= p.Multiplier
		if p.Cap > 0 && delay >= float64(p.Cap) {
			delay = float64(p.Cap)
			break
		}
	}
	if p.Cap > 0 && delay > float64(p.Cap) {
		delay = float64(p.Cap)
	}
	// Full jitter: sleep a uniformly random duration in [0, delay]. The jitter
	// only spreads retry timing to avoid thundering herds; it is not security
	// sensitive, so a non-cryptographic PRNG is appropriate.
	// #nosec G404 -- backoff jitter does not require a cryptographic RNG.
	jittered := time.Duration(rand.Float64() * delay)
	timer := time.NewTimer(jittered)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
