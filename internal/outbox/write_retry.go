package outbox

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
)

const (
	defaultWriteMaxAttempts = 3
	// defaultWriteSettle spaces a resend from the failure. GitHub stops
	// processing an API request after 10 seconds, so a write that returned 5xx or
	// timed out has finished one way or the other before the marker check runs.
	defaultWriteSettle = 12 * time.Second
	// defaultWriteMaxWait matches the GitHub client's longest honored wait.
	defaultWriteMaxWait = 2 * time.Minute
)

// WriteRetry bounds in-run recovery of a write that failed retryably. A write
// can land at the host and still report failure, so nothing is resent until a
// fresh read of the PR shows the write's action marker is absent.
type WriteRetry struct {
	// MaxAttempts counts sends of one action, including the first.
	MaxAttempts int
	// Settle is the wait before the first marker check; later checks wait
	// proportionally longer.
	Settle time.Duration
	// MaxWait is the longest host-requested wait worth sitting through.
	MaxWait time.Duration
	// Sleep waits for d or until ctx ends. Tests inject it to avoid real waits.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter maps a delay to the delay actually used.
	Jitter func(time.Duration) time.Duration
	// OnEvent observes each recovery step.
	OnEvent func(WriteRetryEvent)
}

// WriteRetryPhase names one step of write recovery.
type WriteRetryPhase string

const (
	// WriteRetryWaiting means a write failed and the marker check is scheduled.
	WriteRetryWaiting WriteRetryPhase = "waiting"
	// WriteRetryReconciled means the failed write had landed; nothing is resent.
	WriteRetryReconciled WriteRetryPhase = "reconciled"
	// WriteRetryResending means the marker is absent and the write is resent.
	WriteRetryResending WriteRetryPhase = "resending"
	// WriteRetryAbandoned means recovery stopped with the action left pending.
	WriteRetryAbandoned WriteRetryPhase = "abandoned"
)

// WriteRetryEvent describes one recovery step for one action.
type WriteRetryEvent struct {
	Phase       WriteRetryPhase
	ActionID    string
	Kind        ledger.PlannedActionKind
	Attempt     int // sends so far
	MaxAttempts int
	Delay       time.Duration
	Reason      string
}

// DefaultWriteRetry is the production recovery policy.
func DefaultWriteRetry(onEvent func(WriteRetryEvent)) *WriteRetry {
	return &WriteRetry{OnEvent: onEvent}
}

func (w WriteRetry) normalized() WriteRetry {
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = defaultWriteMaxAttempts
	}
	if w.Settle <= 0 {
		w.Settle = defaultWriteSettle
	}
	if w.MaxWait <= 0 {
		w.MaxWait = defaultWriteMaxWait
	}
	if w.Sleep == nil {
		w.Sleep = sleepContext
	}
	if w.Jitter == nil {
		w.Jitter = addJitter
	}
	return w
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

// addJitter only lengthens the delay, so the settle time stays a lower bound.
func addJitter(d time.Duration) time.Duration {
	spread := d / 4
	if spread <= 0 {
		return d
	}
	return d + rand.N(spread) // #nosec G404 -- backoff spacing, not a secret.
}

type writeRecovery struct {
	upstreamID string
	err        error
}

// recoverWrite handles a failed dispatch of actions[i]. It returns the write's
// final result; a nil recovery.err means the action can be recorded as posted
// with recovery.upstreamID. The returned error is for failures of recovery
// itself: the ledger, or ctx ending during a wait.
func recoverWrite(ctx context.Context, opts Options, req Request, actions []ledger.PlannedAction, i int, plan actionPlan, sendErr error) (writeRecovery, error) {
	policy := opts.WriteRetry.normalized()
	action := &actions[i]
	emit := func(phase WriteRetryPhase, attempt int, delay time.Duration, reason string) {
		if policy.OnEvent != nil {
			policy.OnEvent(WriteRetryEvent{Phase: phase, ActionID: action.ActionID, Kind: action.Kind, Attempt: attempt, MaxAttempts: policy.MaxAttempts, Delay: delay, Reason: reason})
		}
	}
	err := sendErr
	for attempt := 1; attempt < policy.MaxAttempts && errors.Is(err, gitprovider.ErrRetryable); attempt++ {
		if ctx.Err() != nil {
			break
		}
		delay := policy.Jitter(policy.Settle * time.Duration(attempt))
		if hint, ok := gitprovider.RetryAfterHint(err); ok {
			if hint > policy.MaxWait {
				emit(WriteRetryAbandoned, attempt, 0, "host asked to wait "+hint.Round(time.Second).String())
				return writeRecovery{err: err}, nil
			}
			delay = max(delay, hint)
		}
		if updateErr := recordPendingError(ctx, opts.Store, action, err); updateErr != nil {
			return writeRecovery{}, updateErr
		}
		emit(WriteRetryWaiting, attempt, delay, err.Error())
		if sleepErr := policy.Sleep(ctx, delay); sleepErr != nil {
			return writeRecovery{}, sleepErr
		}

		state, readErr := readHostState(ctx, opts.Provider, req.PRRef)
		if readErr != nil {
			emit(WriteRetryAbandoned, attempt, 0, "could not check whether the write landed: "+readErr.Error())
			return writeRecovery{err: err}, nil
		}
		match, matchErr := reconcileAction(req, opts.Provider.Capabilities(), actions, *action, state)
		if matchErr != nil {
			emit(WriteRetryAbandoned, attempt, 0, "could not check whether the write landed: "+matchErr.Error())
			return writeRecovery{err: err}, nil
		}
		if match.ok {
			emit(WriteRetryReconciled, attempt, 0, "")
			return writeRecovery{upstreamID: match.upstreamID}, nil
		}

		if waitErr := opts.Limiter.Wait(ctx, req.PRRef.Host); waitErr != nil {
			if isContextError(waitErr) {
				return writeRecovery{}, waitErr
			}
			emit(WriteRetryAbandoned, attempt, 0, waitErr.Error())
			return writeRecovery{err: err}, nil
		}
		now := opts.Now().UTC()
		action.Attempts++
		action.AttemptedAt = &now
		if updateErr := opts.Store.UpdatePlannedAction(ctx, *action); updateErr != nil {
			return writeRecovery{}, updateErr
		}
		emit(WriteRetryResending, attempt+1, 0, "")
		var upstreamID string
		upstreamID, err = dispatch(ctx, opts.Provider, req.PRRef, plan)
		if err == nil {
			return writeRecovery{upstreamID: upstreamID}, nil
		}
	}
	return writeRecovery{err: err}, nil
}
