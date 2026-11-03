package github

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const (
	defaultRetryMaxAttempts = 6
	defaultRetryBaseDelay   = time.Second
	defaultRetryMaxDelay    = 16 * time.Second
	// defaultRetryMaxWait is the longest host-requested wait worth sitting
	// through. A primary rate limit can ask for most of an hour; that fails now
	// with the reason instead of parking the run.
	defaultRetryMaxWait = 2 * time.Minute
	// defaultRetryMaxElapsed bounds one call across attempts, because each
	// attempt can itself spend defaultHTTPTimeout on an abandoned connection.
	defaultRetryMaxElapsed = 5 * time.Minute
	// defaultRateLimitWait is GitHub's documented minimum wait when a rate-limit
	// response names no wait of its own.
	defaultRateLimitWait = time.Minute
)

// RetryEvent describes one scheduled retry of a read.
type RetryEvent struct {
	Op          gitprovider.Operation
	Attempt     int // the attempt that just failed, starting at 1
	MaxAttempts int
	Delay       time.Duration
	Reason      string
}

// RetryPolicy bounds retries of idempotent GitHub requests. Zero fields take
// the defaults; MaxAttempts of 1 disables retrying.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxWait     time.Duration
	MaxElapsed  time.Duration
	// Sleep waits for d or until ctx ends. Tests inject it to avoid real waits.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter maps a backoff to the delay actually used.
	Jitter func(time.Duration) time.Duration
	// OnRetry observes each scheduled retry.
	OnRetry func(RetryEvent)
}

type retrier struct {
	policy RetryPolicy
	now    func() time.Time
}

func newRetrier(policy RetryPolicy, now func() time.Time) retrier {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = defaultRetryMaxAttempts
	}
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = defaultRetryBaseDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaultRetryMaxDelay
	}
	if policy.MaxWait <= 0 {
		policy.MaxWait = defaultRetryMaxWait
	}
	if policy.MaxElapsed <= 0 {
		policy.MaxElapsed = defaultRetryMaxElapsed
	}
	if policy.Sleep == nil {
		policy.Sleep = sleepContext
	}
	if policy.Jitter == nil {
		policy.Jitter = equalJitter
	}
	if now == nil {
		now = time.Now
	}
	return retrier{policy: policy, now: now}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func equalJitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1) // #nosec G404 -- backoff spacing, not a secret.
}

// retrySafeOperation lists what may be re-sent blindly. Anything else — writes
// and any operation added later — gets one attempt, because a write can land at
// GitHub and still return 5xx. Review writes are retried by internal/outbox,
// which re-reads the PR for its action marker before sending again.
func retrySafeOperation(op gitprovider.Operation) bool {
	switch op {
	case gitprovider.OperationWhoAmI,
		gitprovider.OperationReviewAuthority,
		gitprovider.OperationGetPR,
		gitprovider.OperationGetDiff,
		gitprovider.OperationGetDiffBetweenRefs,
		gitprovider.OperationGetFileAtRef,
		gitprovider.OperationListTreeAtRef,
		gitprovider.OperationListInlineThreads,
		gitprovider.OperationListReviews,
		gitprovider.OperationListIssueComments,
		gitprovider.OperationListRepositoryInvitations:
		return true
	case gitprovider.OperationPostInlineComment,
		gitprovider.OperationReplyToThread,
		gitprovider.OperationResolveThread,
		gitprovider.OperationPostIssueComment,
		gitprovider.OperationSubmitReview,
		gitprovider.OperationAcceptRepositoryInvitation:
		return false
	default:
		return false
	}
}

// do runs attempt under the policy when op is safe to repeat.
func (r retrier) do(ctx context.Context, op gitprovider.Operation, attempt func() error) error {
	if !retrySafeOperation(op) {
		return attempt()
	}
	return r.repeat(ctx, op, attempt)
}

// repeat runs attempt until it stops failing retryably or the policy runs out.
func (r retrier) repeat(ctx context.Context, op gitprovider.Operation, attempt func() error) error {
	started := r.now()
	for tries := 1; ; tries++ {
		err := attempt()
		if err == nil || !errors.Is(err, gitprovider.ErrRetryable) || isPermanentTransportError(err) || ctx.Err() != nil {
			return err
		}
		if tries >= r.policy.MaxAttempts {
			return fmt.Errorf("%w (gave up after %d attempts)", err, tries)
		}
		delay := r.delay(tries, err)
		if hint, ok := gitprovider.RetryAfterHint(err); ok && hint > r.policy.MaxWait {
			return fmt.Errorf("%w (not retrying: GitHub asked to wait %s)", err, hint.Round(time.Second))
		}
		if r.now().Sub(started)+delay > r.policy.MaxElapsed {
			return fmt.Errorf("%w (gave up after %d attempts over %s)", err, tries, r.now().Sub(started).Round(time.Second))
		}
		if deadline, ok := ctx.Deadline(); ok && r.now().Add(delay).After(deadline) {
			return err
		}
		if r.policy.OnRetry != nil {
			r.policy.OnRetry(RetryEvent{Op: op, Attempt: tries, MaxAttempts: r.policy.MaxAttempts, Delay: delay, Reason: retryReason(err)})
		}
		if sleepErr := r.policy.Sleep(ctx, delay); sleepErr != nil {
			if errors.Is(sleepErr, context.Canceled) {
				return context.Canceled
			}
			return err
		}
	}
}

func (r retrier) delay(tries int, err error) time.Duration {
	backoff := r.policy.BaseDelay << (tries - 1)
	if backoff > r.policy.MaxDelay || backoff <= 0 {
		backoff = r.policy.MaxDelay
	}
	delay := r.policy.Jitter(backoff)
	if hint, ok := gitprovider.RetryAfterHint(err); ok && hint > delay {
		return hint
	}
	return delay
}

// isPermanentTransportError spots transport failures another attempt cannot
// fix. They keep their ErrRetryable classification for exit codes.
func isPermanentTransportError(err error) bool {
	var certErr *tls.CertificateVerificationError
	return errors.As(err, &certErr)
}

func retryReason(err error) string {
	var providerErr *gitprovider.ProviderError
	if errors.As(err, &providerErr) && providerErr.Err != nil {
		return providerErr.Err.Error()
	}
	return err.Error()
}

// responseError classifies a non-2xx response, attaching any requested wait.
func (r retrier) responseError(op gitprovider.Operation, status int, header http.Header, body []byte, classify func(gitprovider.Operation, int, []byte) error) error {
	if !isRateLimitResponse(status, header, body) {
		return r.withRetryHint(classify(op, status, body), header)
	}
	err := gitprovider.WrapError(gitprovider.ErrRetryable, op, fmt.Errorf("github: status %d (rate limited)", status))
	hint, ok := requestedWait(header, r.now())
	if !ok {
		hint = defaultRateLimitWait
	}
	return &gitprovider.RetryAfterError{After: hint, Err: err}
}

// withRetryHint attaches the wait from header to a retryable err.
func (r retrier) withRetryHint(err error, header http.Header) error {
	if !errors.Is(err, gitprovider.ErrRetryable) {
		return err
	}
	if hint, ok := requestedWait(header, r.now()); ok {
		return &gitprovider.RetryAfterError{After: hint, Err: err}
	}
	return err
}

// isRateLimitResponse recognizes a rate limit GitHub reports as 403 rather than
// 429. mapHTTPStatus would call that a permission failure, and the outbox would
// then record a terminal auth failure for a request that only needed waiting.
func isRateLimitResponse(status int, header http.Header, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	if strings.TrimSpace(header.Get("Retry-After")) != "" || strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" {
		return true
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(payload.Message), "rate limit")
}

// requestedWait reads GitHub's documented order: Retry-After first, then
// X-RateLimit-Reset once the quota is exhausted.
func requestedWait(header http.Header, now time.Time) (time.Duration, bool) {
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			if seconds < 0 {
				return 0, false
			}
			return time.Duration(seconds) * time.Second, true
		}
		if at, err := http.ParseTime(value); err == nil {
			return max(at.Sub(now), 0), true
		}
		return 0, false
	}
	if strings.TrimSpace(header.Get("X-RateLimit-Remaining")) != "0" {
		return 0, false
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil {
		return 0, false
	}
	return max(time.Unix(reset, 0).Sub(now), 0), true
}
