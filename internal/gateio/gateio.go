// Package gateio maps provider/ledger state into the pure gate kernel.
package gateio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// Status identifies how gate IO resolved the request.
type Status string

// Result status values.
const (
	StatusContinue           Status = "continue"
	StatusEarlyExit          Status = "early_exit"
	StatusDryRunFresh        Status = "dry_run_fresh"
	StatusRepairExecuted     Status = "repair_executed"
	StatusRetryPostsExecuted Status = "retry_posts_executed"
	StatusError              Status = "error"
	StatusBaseMovedAbort     Status = "base_moved_abort"
)

// Store is the ledger behavior required by gate IO.
type Store interface {
	ListRunsForHeadScope(context.Context, ledger.ListRunsForHeadScopeParams) ([]ledger.Run, error)
	GetRun(context.Context, string) (ledger.Run, error)
	ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error)
	AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error)
	InsertPlannedAction(context.Context, ledger.PlannedAction) error
	UpdatePlannedAction(context.Context, ledger.PlannedAction) error
	DeleteRun(context.Context, string) error
	CompleteRun(context.Context, string, ledger.Outcome, time.Time) error
}

// Lock is a held run lock.
type Lock interface {
	Release() error
}

// AcquireFunc acquires one non-blocking run lock.
type AcquireFunc func(string) (Lock, error)

// Options contains gate IO dependencies.
type Options struct {
	Store                   Store
	Provider                gitprovider.GitProvider
	Limiter                 outbox.Limiter
	Layout                  statepaths.Layout
	Acquire                 AcquireFunc
	Now                     func() time.Time
	StaleHeartbeatThreshold time.Duration
	Warnings                io.Writer
}

// Request identifies one gate evaluation.
type Request struct {
	PRRef              gitprovider.PRRef
	PR                 gitprovider.PR
	PRKey              string
	Profile            string
	PostingIdentity    gitprovider.Identity
	PostingIdentityKey string
	FreshRunID         string
	Flags              gate.Flags
	ArtifactPath       string
}

// Result is the outcome of one gate IO evaluation.
type Result struct {
	Status       Status
	Decision     gate.Decision
	Run          ledger.Run
	Lock         Lock
	OutboxResult outbox.Result
	Warnings     []string
}

type staleProbe struct {
	lock Lock
}

type markerRecord struct {
	marker marker.ActionMarker
	when   time.Time
	order  int
}

type staleRun struct {
	run ledger.Run
}

type repairExecution struct {
	postResult    outbox.Result
	run           ledger.Run
	baseDecision  gate.Decision
	baseMoved     bool
	outboxInvoked bool
}

type retryExecution struct {
	postResult     outbox.Result
	baseDecision   gate.Decision
	baseMoved      bool
	statusDecision gate.Decision
	outboxInvoked  bool
}

const (
	repairSubmitReviewActionID = "repair-submit-review"
	repairSubmitReviewBody     = "Completing previously posted codereview rollup."
)

// Evaluate acquires live gate state, calls the pure kernel, and executes
// non-repair decisions.
func Evaluate(ctx context.Context, opts Options, req Request) (Result, error) {
	if err := validateOptions(&opts); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if req.Flags.DryRun {
		decision := gate.Decide(gate.Request{
			Flags: req.Flags,
			PR:    gate.PRSummary{State: gate.PRStateFresh},
		})
		if decision.Kind == gate.DecisionError {
			return Result{Status: StatusError, Decision: decision}, nil
		}
		return Result{Status: StatusDryRunFresh, Decision: decision}, nil
	}

	lockPath, err := currentLockPath(opts.Layout, req)
	if err != nil {
		return Result{}, err
	}
	currentLock, err := opts.acquire(lockPath)
	if err != nil {
		return Result{}, err
	}
	releaseCurrent := true
	defer func() {
		if releaseCurrent {
			_ = currentLock.Release()
		}
	}()

	for attempts := 0; ; attempts++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		state, err := buildLocalState(ctx, opts, req)
		if err != nil {
			return Result{}, err
		}
		if attempts > len(state.staleRuns)+2 {
			return Result{}, fmt.Errorf("gateio: stale-base cleanup exceeded retry limit")
		}

		decision := gate.Decide(state.kernel)
		if !localDecisionApplies(req.Flags, decision) {
			state, err = attachExternalState(ctx, opts, req, state)
			if err != nil {
				return Result{}, err
			}
			decision = gate.Decide(state.kernel)
		}

		result, retry, err := executeDecision(ctx, opts, req, state, decision, currentLock, &releaseCurrent)
		if err != nil {
			return result, err
		}
		if retry {
			continue
		}
		return result, nil
	}
}

// AbortIfBaseMoved aborts run when the PR base ref no longer matches run.BaseSHA.
func AbortIfBaseMoved(ctx context.Context, opts Options, req Request, run ledger.Run) (Result, error) {
	if err := validateAbortOptions(&opts); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	decision, moved, err := baseMovedDecision(ctx, opts, req, run.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	if !moved {
		return Result{Status: StatusContinue, Run: run}, nil
	}
	now := opts.now()
	if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeAborted, now); err != nil {
		return Result{}, err
	}
	return Result{
		Status:   StatusBaseMovedAbort,
		Run:      run,
		Decision: decision,
	}, nil
}

func baseMovedDecision(ctx context.Context, opts Options, req Request, baseSHA string) (gate.Decision, bool, error) {
	pr, err := opts.Provider.GetPR(ctx, req.PRRef)
	if err != nil {
		return gate.Decision{}, false, err
	}
	if pr.Base.SHA == baseSHA {
		return gate.Decision{}, false, nil
	}
	return gate.Decision{
		Kind:    gate.DecisionError,
		Message: fmt.Sprintf("base moved from %s to %s", baseSHA, pr.Base.SHA),
	}, true, nil
}

type gateState struct {
	kernel             gate.Request
	runByID            map[string]ledger.Run
	partialRunMismatch bool
	staleRuns          []staleRun
	staleLocks         map[string]staleProbe
}

func (s gateState) releaseStaleLocks() {
	for _, probe := range s.staleLocks {
		if probe.lock != nil {
			_ = probe.lock.Release()
		}
	}
}

func buildLocalState(ctx context.Context, opts Options, req Request) (gateState, error) {
	runs, err := opts.Store.ListRunsForHeadScope(ctx, ledger.ListRunsForHeadScopeParams{
		PRKey:           req.PRKey,
		SHA:             req.PR.Head.SHA,
		Profile:         req.Profile,
		PostingIdentity: req.postingKey(),
	})
	if err != nil {
		return gateState{}, err
	}

	state := gateState{
		kernel:     gate.Request{Flags: req.Flags, PR: gate.PRSummary{State: gate.PRStateFresh}},
		runByID:    make(map[string]ledger.Run, len(runs)),
		staleLocks: make(map[string]staleProbe),
	}
	for _, run := range runs {
		state.runByID[run.RunID] = run
		if run.BaseSHA != req.PR.Base.SHA {
			state.staleRuns = append(state.staleRuns, staleRun{run: run})
			continue
		}
		summary, err := summarizeRun(ctx, opts.Store, run)
		if err != nil {
			return gateState{}, err
		}
		state.kernel.ExactRuns = append(state.kernel.ExactRuns, summary)
	}
	return state, nil
}

func attachExternalState(ctx context.Context, opts Options, req Request, state gateState) (gateState, error) {
	for _, stale := range state.staleRuns {
		summary, err := summarizeRun(ctx, opts.Store, stale.run)
		if err != nil {
			state.releaseStaleLocks()
			return gateState{}, err
		}
		candidate, probe, err := summarizeStaleCandidate(opts, req, stale.run, summary)
		if err != nil {
			state.releaseStaleLocks()
			return gateState{}, err
		}
		state.kernel.StaleBaseCandidates = append(state.kernel.StaleBaseCandidates, candidate)
		if probe.lock != nil {
			state.staleLocks[stale.run.RunID] = probe
		}
	}

	prSummary, err := summarizePR(ctx, opts.Provider, req)
	if err != nil {
		state.releaseStaleLocks()
		return gateState{}, err
	}
	state.kernel.PR = prSummary
	if prSummary.State == gate.PRStatePartial {
		partialRun, mismatched, err := lookupScopedPartialRun(ctx, opts, req, prSummary.RunID)
		if err != nil {
			state.releaseStaleLocks()
			return gateState{}, err
		}
		state.partialRunMismatch = mismatched
		if partialRun != nil {
			state.kernel.PartialRun = partialRun
		}
	}
	return state, nil
}

func localDecisionApplies(flags gate.Flags, decision gate.Decision) bool {
	switch decision.Kind {
	case gate.DecisionResume, gate.DecisionRetryPosts, gate.DecisionError:
		return true
	case gate.DecisionFresh:
		return flags.Rerun
	case gate.DecisionEarlyExit, gate.DecisionRepair, gate.DecisionAbortStale:
		return false
	default:
		return false
	}
}

func executeDecision(ctx context.Context, opts Options, req Request, state gateState, decision gate.Decision, currentLock Lock, releaseCurrent *bool) (Result, bool, error) {
	result := Result{Decision: decision, Warnings: append([]string(nil), decision.Warnings...)}

	switch decision.Kind {
	case gate.DecisionResume:
		state.releaseStaleLocks()
		run, ok := state.runByID[decision.RunID]
		if !ok {
			return Result{}, false, fmt.Errorf("gateio: resume run %q was not loaded", decision.RunID)
		}
		if baseResult, err := AbortIfBaseMoved(ctx, opts, req, run); err != nil {
			return Result{}, false, err
		} else if baseResult.Status == StatusBaseMovedAbort {
			return baseResult, false, nil
		}
		*releaseCurrent = false
		result.Status = StatusContinue
		result.Run = run
		result.Lock = currentLock
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	case gate.DecisionFresh:
		state.releaseStaleLocks()
		if baseDecision, moved, err := baseMovedDecision(ctx, opts, req, req.PR.Base.SHA); err != nil {
			return Result{}, false, err
		} else if moved {
			result.Status = StatusBaseMovedAbort
			result.Decision = baseDecision
			return result, false, nil
		}
		if err := abortRuns(ctx, opts, decision.SupersedeRunIDs); err != nil {
			return Result{}, false, err
		}
		run, err := allocateFresh(ctx, opts, req)
		if err != nil {
			return Result{}, false, err
		}
		*releaseCurrent = false
		result.Status = StatusContinue
		result.Run = run
		result.Lock = currentLock
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	case gate.DecisionEarlyExit:
		state.releaseStaleLocks()
		result.Status = StatusEarlyExit
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	case gate.DecisionAbortStale:
		if err := abortStaleRuns(ctx, opts, state, decision.AbortStaleRunIDs); err != nil {
			state.releaseStaleLocks()
			return Result{}, false, err
		}
		state.releaseStaleLocks()
		return Result{}, true, nil
	case gate.DecisionRepair:
		state.releaseStaleLocks()
		if state.partialRunMismatch {
			result.Status = StatusError
			result.Decision = gate.Decision{
				Kind:        gate.DecisionError,
				RunID:       decision.RunID,
				ErrorReason: gate.ErrorInvalidInput,
				Message:     fmt.Sprintf("partial marker run %q exists outside the current resume scope; not repairing", decision.RunID),
			}
			emitWarnings(opts.Warnings, result.Warnings)
			return result, false, nil
		}
		execution, err := executeRepair(ctx, opts, req, decision)
		if err != nil {
			if execution.outboxInvoked {
				result.Status = StatusRepairExecuted
				result.Run = execution.run
				result.OutboxResult = execution.postResult
				return result, false, err
			}
			return Result{}, false, err
		}
		if execution.baseMoved {
			result.Status = StatusBaseMovedAbort
			result.Decision = execution.baseDecision
			return result, false, nil
		}
		result.Status = StatusRepairExecuted
		result.Run = execution.run
		result.OutboxResult = execution.postResult
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	case gate.DecisionRetryPosts:
		state.releaseStaleLocks()
		run, ok := state.runByID[decision.RunID]
		if !ok {
			return Result{}, false, fmt.Errorf("gateio: retry-posts run %q was not loaded", decision.RunID)
		}
		execution, err := executeRetryPosts(ctx, opts, req, run)
		if err != nil {
			if execution.outboxInvoked {
				result.Status = StatusRetryPostsExecuted
				result.Run = run
				result.OutboxResult = execution.postResult
				return result, false, err
			}
			return Result{}, false, err
		}
		if execution.baseMoved {
			result.Status = StatusBaseMovedAbort
			result.Decision = execution.baseDecision
			return result, false, nil
		}
		if execution.statusDecision.Kind == gate.DecisionError {
			result.Status = StatusError
			result.Decision = execution.statusDecision
			emitWarnings(opts.Warnings, result.Warnings)
			return result, false, nil
		}
		result.Status = StatusRetryPostsExecuted
		result.Run = run
		result.OutboxResult = execution.postResult
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	case gate.DecisionError:
		state.releaseStaleLocks()
		result.Status = StatusError
		emitWarnings(opts.Warnings, result.Warnings)
		return result, false, nil
	default:
		state.releaseStaleLocks()
		return Result{}, false, fmt.Errorf("gateio: unsupported gate decision %q", decision.Kind)
	}
}

func executeRepair(ctx context.Context, opts Options, req Request, decision gate.Decision) (repairExecution, error) {
	if baseDecision, moved, err := baseMovedDecision(ctx, opts, req, req.PR.Base.SHA); err != nil {
		return repairExecution{}, err
	} else if moved {
		return repairExecution{baseDecision: baseDecision, baseMoved: true}, nil
	}
	if err := requireOutboxLimiter(opts); err != nil {
		return repairExecution{}, err
	}
	if _, err := opts.Store.GetRun(ctx, decision.RunID); err == nil {
		return repairExecution{}, fmt.Errorf("gateio: repair run %q already exists", decision.RunID)
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return repairExecution{}, err
	}

	desired, event, err := repairOutcomeEvent(decision.Outcome)
	if err != nil {
		return repairExecution{}, err
	}
	run, err := allocateRepair(ctx, opts, req, decision.RunID)
	if err != nil {
		return repairExecution{}, err
	}
	if err := insertRepairSubmitReview(ctx, opts, run.RunID, event); err != nil {
		if deleteErr := opts.Store.DeleteRun(ctx, run.RunID); deleteErr != nil && !errors.Is(deleteErr, ledger.ErrNotFound) {
			return repairExecution{}, fmt.Errorf("gateio: insert repair submit_review: %w; cleanup recovery run: %w", err, deleteErr)
		}
		return repairExecution{}, err
	}
	postResult, err := outbox.Post(ctx, outbox.Options{
		Store:    opts.Store,
		Provider: opts.Provider,
		Limiter:  opts.Limiter,
		Now:      opts.Now,
	}, outbox.Request{
		Run:             run,
		PRRef:           req.PRRef,
		PostingIdentity: req.PostingIdentity,
		DesiredOutcome:  desired,
	})
	if err != nil {
		return repairExecution{postResult: postResult, run: run, outboxInvoked: true}, err
	}
	return repairExecution{postResult: postResult, run: run, outboxInvoked: true}, nil
}

func executeRetryPosts(ctx context.Context, opts Options, req Request, run ledger.Run) (retryExecution, error) {
	if baseDecision, moved, err := baseMovedDecision(ctx, opts, req, run.BaseSHA); err != nil {
		return retryExecution{}, err
	} else if moved {
		return retryExecution{baseDecision: baseDecision, baseMoved: true}, nil
	}
	if err := requireOutboxLimiter(opts); err != nil {
		return retryExecution{}, err
	}

	actions, err := opts.Store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		return retryExecution{}, err
	}
	if !hasRequiredRetryEligibleAction(actions) {
		return retryExecution{statusDecision: retryPostsIneligible()}, nil
	}
	desired, statusDecision := retryDesiredOutcome(actions)
	if statusDecision.Kind == gate.DecisionError {
		return retryExecution{statusDecision: statusDecision}, nil
	}
	if err := resetRequiredFailedTerminalActionsForRetry(ctx, opts.Store, actions); err != nil {
		return retryExecution{}, err
	}
	postResult, err := outbox.Post(ctx, outbox.Options{
		Store:    opts.Store,
		Provider: opts.Provider,
		Limiter:  opts.Limiter,
		Now:      opts.Now,
	}, outbox.Request{
		Run:             run,
		PRRef:           req.PRRef,
		PostingIdentity: req.PostingIdentity,
		DesiredOutcome:  desired,
	})
	if err != nil {
		return retryExecution{postResult: postResult, outboxInvoked: true}, err
	}
	return retryExecution{postResult: postResult, outboxInvoked: true}, nil
}

func requireOutboxLimiter(opts Options) error {
	if opts.Limiter == nil {
		return fmt.Errorf("gateio: outbox limiter is required")
	}
	return nil
}

func allocateRepair(ctx context.Context, opts Options, req Request, runID string) (ledger.Run, error) {
	return opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           req.PRKey,
		PRURL:           req.PR.URL,
		RunID:           runID,
		SHA:             req.PR.Head.SHA,
		BaseSHA:         req.PR.Base.SHA,
		Profile:         req.Profile,
		PostingIdentity: req.postingKey(),
		PostMode:        ledger.PostModeLive,
		StartedAt:       opts.now(),
		ArtifactPath:    req.ArtifactPath,
	})
}

func insertRepairSubmitReview(ctx context.Context, opts Options, runID string, event review.ReviewEvent) error {
	payload, err := json.Marshal(outbox.SubmitReviewPayload{
		Body:  repairSubmitReviewBody,
		Event: event,
	})
	if err != nil {
		return err
	}
	return opts.Store.InsertPlannedAction(ctx, ledger.PlannedAction{
		ActionID:    repairSubmitReviewActionID,
		RunID:       runID,
		Kind:        ledger.PlannedActionSubmitReview,
		PlannedAt:   opts.now(),
		PayloadJSON: string(payload),
		Status:      ledger.PlannedActionPending,
		Required:    true,
	})
}

func resetRequiredFailedTerminalActionsForRetry(ctx context.Context, store Store, actions []ledger.PlannedAction) error {
	for _, action := range actions {
		if !action.Required || action.Status != ledger.PlannedActionFailedTerminal {
			continue
		}
		action.Status = ledger.PlannedActionPending
		action.PostedAt = nil
		action.UpstreamID = nil
		action.Error = nil
		action.FailureClass = nil
		if err := store.UpdatePlannedAction(ctx, action); err != nil {
			return err
		}
	}
	return nil
}

func hasRequiredRetryEligibleAction(actions []ledger.PlannedAction) bool {
	for _, action := range actions {
		if !action.Required {
			continue
		}
		switch action.Status {
		case ledger.PlannedActionPending, ledger.PlannedActionFailedTerminal:
			return true
		case ledger.PlannedActionPosted, ledger.PlannedActionSuperseded, ledger.PlannedActionPlannedOnly:
		}
	}
	return false
}

func retryDesiredOutcome(actions []ledger.PlannedAction) (ledger.Outcome, gate.Decision) {
	var (
		submitOutcome      ledger.Outcome
		haveSubmitOutcome  bool
		haveRequiredRollup bool
		haveOtherRequired  bool
	)
	for _, action := range actions {
		if !action.Required || action.Status == ledger.PlannedActionSuperseded || action.Status == ledger.PlannedActionPlannedOnly {
			continue
		}
		switch action.Kind {
		case ledger.PlannedActionSubmitReview:
			payload, err := decodeSubmitReviewPayload(action)
			if err != nil {
				return "", invalidInputDecision(err.Error())
			}
			outcome, err := outcomeFromReviewEvent(payload.Event)
			if err != nil {
				return "", invalidInputDecision(err.Error())
			}
			if haveSubmitOutcome && submitOutcome != outcome {
				return "", invalidInputDecision("conflicting required submit_review outcomes")
			}
			submitOutcome = outcome
			haveSubmitOutcome = true
		case ledger.PlannedActionRollupComment:
			haveRequiredRollup = true
		case ledger.PlannedActionInlineComment, ledger.PlannedActionThreadReply, ledger.PlannedActionResolveThread:
			haveOtherRequired = true
		default:
			haveOtherRequired = true
		}
	}
	if haveSubmitOutcome {
		return submitOutcome, gate.Decision{}
	}
	if haveRequiredRollup && !haveOtherRequired {
		return ledger.OutcomeNothingToReview, gate.Decision{}
	}
	return "", invalidInputDecision("retry-posts desired outcome cannot be derived from planned actions")
}

func decodeSubmitReviewPayload(action ledger.PlannedAction) (outbox.SubmitReviewPayload, error) {
	var payload outbox.SubmitReviewPayload
	if err := json.Unmarshal([]byte(action.PayloadJSON), &payload); err != nil {
		return payload, fmt.Errorf("gateio: decode submit_review payload %q: %w", action.ActionID, err)
	}
	return payload, nil
}

func repairOutcomeEvent(outcome gate.PROutcome) (ledger.Outcome, review.ReviewEvent, error) {
	switch outcome {
	case gate.PROutcomeApproved:
		return ledger.OutcomeApproved, review.ReviewEventApprove, nil
	case gate.PROutcomeRequestChanges:
		return ledger.OutcomeRequestChanges, review.ReviewEventRequestChanges, nil
	case gate.PROutcomeComment:
		return ledger.OutcomeComment, review.ReviewEventComment, nil
	case gate.PROutcomeNothingToReview:
		return "", "", fmt.Errorf("gateio: unsupported repair outcome %q", outcome)
	default:
		return "", "", fmt.Errorf("gateio: unsupported repair outcome %q", outcome)
	}
}

func outcomeFromReviewEvent(event review.ReviewEvent) (ledger.Outcome, error) {
	switch event {
	case review.ReviewEventApprove:
		return ledger.OutcomeApproved, nil
	case review.ReviewEventRequestChanges:
		return ledger.OutcomeRequestChanges, nil
	case review.ReviewEventComment:
		return ledger.OutcomeComment, nil
	default:
		return "", fmt.Errorf("gateio: unsupported submit_review event %q", event)
	}
}

func retryPostsIneligible() gate.Decision {
	return gate.Decision{
		Kind:        gate.DecisionError,
		ErrorReason: gate.ErrorRetryPostsIneligible,
		Message:     "no live run has required pending or failed_terminal actions",
	}
}

func invalidInputDecision(message string) gate.Decision {
	return gate.Decision{
		Kind:        gate.DecisionError,
		ErrorReason: gate.ErrorInvalidInput,
		Message:     message,
	}
}

func summarizeRun(ctx context.Context, store Store, run ledger.Run) (gate.RunSummary, error) {
	mode, err := gatePostMode(run.PostMode)
	if err != nil {
		return gate.RunSummary{}, err
	}
	state, err := gateRunState(run.Outcome)
	if err != nil {
		return gate.RunSummary{}, err
	}
	actions, err := store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		return gate.RunSummary{}, err
	}
	summary := gate.RunSummary{
		RunID:    run.RunID,
		Attempt:  run.Attempt,
		PostMode: mode,
		State:    state,
	}
	for _, action := range actions {
		if !action.Required {
			continue
		}
		switch action.Status {
		case ledger.PlannedActionPending:
			summary.RequiredPending++
		case ledger.PlannedActionFailedTerminal:
			summary.RequiredFailedTerminal++
			if action.FailureClass != nil && *action.FailureClass == ledger.PlannedActionFailureClassAuth {
				summary.FailureClass = gate.FailureClassAuth
			} else if summary.FailureClass == gate.FailureClassNone {
				summary.FailureClass = gate.FailureClassTerminal
			}
		case ledger.PlannedActionPosted, ledger.PlannedActionSuperseded, ledger.PlannedActionPlannedOnly:
		}
	}
	return summary, nil
}

func summarizeStaleCandidate(opts Options, req Request, run ledger.Run, summary gate.RunSummary) (gate.StaleBaseCandidate, staleProbe, error) {
	lockPath, err := lockPathForRun(opts.Layout, req.PRRef, run)
	if err != nil {
		return gate.StaleBaseCandidate{}, staleProbe{}, err
	}
	lock, err := opts.acquire(lockPath)
	if err == nil {
		return gate.StaleBaseCandidate{Run: summary, LockState: gate.LockStateFree}, staleProbe{lock: lock}, nil
	}
	if !errors.Is(err, runlock.ErrHeld) {
		return gate.StaleBaseCandidate{}, staleProbe{}, err
	}
	return gate.StaleBaseCandidate{
		Run:            summary,
		LockState:      gate.LockStateHeld,
		HeartbeatStale: heartbeatStale(run, opts.now(), opts.StaleHeartbeatThreshold),
	}, staleProbe{}, nil
}

func summarizePR(ctx context.Context, provider gitprovider.GitProvider, req Request) (gate.PRSummary, error) {
	comments, err := provider.ListIssueComments(ctx, req.PRRef)
	if err != nil {
		return gate.PRSummary{}, err
	}
	reviews, err := provider.ListReviews(ctx, req.PRRef)
	if err != nil {
		return gate.PRSummary{}, err
	}

	records := make([]markerRecord, 0, len(comments)+len(reviews))
	order := 0
	for _, comment := range comments {
		if !sameIdentity(comment.Author, req.PostingIdentity) {
			continue
		}
		for _, found := range marker.FindActions(comment.Body) {
			records = append(records, markerRecord{marker: found, when: comment.CreatedAt, order: order})
			order++
		}
	}
	for _, review := range reviews {
		if !sameIdentity(review.Author, req.PostingIdentity) {
			continue
		}
		for _, found := range marker.FindActions(review.Body) {
			records = append(records, markerRecord{marker: found, when: review.SubmittedAt, order: order})
			order++
		}
	}
	return classifyMarkers(records, req.PR.Head.SHA, req.PR.Base.SHA), nil
}

func classifyMarkers(records []markerRecord, headSHA, baseSHA string) gate.PRSummary {
	submits := map[string]markerRecord{}
	var currentNoDiff *markerRecord
	var currentPartial *markerRecord
	var stale *markerRecord

	for i := range records {
		record := records[i]
		found := record.marker
		if found.SHA != headSHA {
			continue
		}
		if found.BaseSHA != baseSHA {
			stale = newest(stale, &record)
			continue
		}
		if found.Kind == marker.ActionKindSubmitReview {
			key := markerKey(found)
			submits[key] = record
		}
	}
	for i := range records {
		record := records[i]
		found := record.marker
		if found.SHA != headSHA || found.BaseSHA != baseSHA || found.Kind != marker.ActionKindRollupComment {
			continue
		}
		if found.Outcome == marker.RollupOutcomeNothingToReview {
			currentNoDiff = newest(currentNoDiff, &record)
			continue
		}
		if _, ok := submits[markerKey(found)]; ok {
			continue
		}
		currentPartial = newest(currentPartial, &record)
	}
	if len(submits) > 0 {
		if record, ok := newestSubmit(submits); ok {
			return gate.PRSummary{State: gate.PRStateCompleteReview, RunID: record.marker.RunID}
		}
	}
	if currentNoDiff != nil {
		return gate.PRSummary{State: gate.PRStateCompleteNoDiff, RunID: currentNoDiff.marker.RunID, Outcome: gate.PROutcomeNothingToReview}
	}
	if currentPartial != nil {
		outcome := gate.PROutcome(currentPartial.marker.Outcome)
		return gate.PRSummary{State: gate.PRStatePartial, RunID: currentPartial.marker.RunID, Outcome: outcome}
	}
	if stale != nil {
		return gate.PRSummary{State: gate.PRStateStaleBase, RunID: stale.marker.RunID}
	}
	return gate.PRSummary{State: gate.PRStateFresh}
}

func lookupScopedPartialRun(ctx context.Context, opts Options, req Request, runID string) (*gate.RunSummary, bool, error) {
	run, err := opts.Store.GetRun(ctx, runID)
	if errors.Is(err, ledger.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if run.PRKey != req.PRKey || run.SHA != req.PR.Head.SHA || run.BaseSHA != req.PR.Base.SHA ||
		run.Profile != req.Profile || run.PostingIdentity != req.postingKey() {
		return nil, true, nil
	}
	summary, err := summarizeRun(ctx, opts.Store, run)
	if err != nil {
		return nil, false, err
	}
	return &summary, false, nil
}

func abortStaleRuns(ctx context.Context, opts Options, state gateState, runIDs []string) error {
	for _, runID := range runIDs {
		probe, ok := state.staleLocks[runID]
		if !ok || probe.lock == nil {
			return fmt.Errorf("gateio: stale abort target %q was not locked", runID)
		}
		if err := opts.Store.CompleteRun(ctx, runID, ledger.OutcomeAborted, opts.now()); err != nil {
			return err
		}
	}
	return nil
}

func abortRuns(ctx context.Context, opts Options, runIDs []string) error {
	for _, runID := range runIDs {
		if err := opts.Store.CompleteRun(ctx, runID, ledger.OutcomeAborted, opts.now()); err != nil {
			return err
		}
	}
	return nil
}

func allocateFresh(ctx context.Context, opts Options, req Request) (ledger.Run, error) {
	return opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           req.PRKey,
		PRURL:           req.PR.URL,
		RunID:           strings.TrimSpace(req.FreshRunID),
		SHA:             req.PR.Head.SHA,
		BaseSHA:         req.PR.Base.SHA,
		Profile:         req.Profile,
		PostingIdentity: req.postingKey(),
		PostMode:        ledger.PostModeLive,
		StartedAt:       opts.now(),
		ArtifactPath:    req.ArtifactPath,
	})
}

func currentLockPath(layout statepaths.Layout, req Request) (string, error) {
	return layout.LockFile(statepaths.LockSpec{
		Host:            req.PRRef.Host,
		Owner:           req.PRRef.Owner,
		Repo:            req.PRRef.Repo,
		PRNumber:        req.PRRef.Number,
		HeadSHA:         req.PR.Head.SHA,
		BaseSHA:         req.PR.Base.SHA,
		Profile:         req.Profile,
		PostingIdentity: req.postingKey(),
	})
}

func lockPathForRun(layout statepaths.Layout, ref gitprovider.PRRef, run ledger.Run) (string, error) {
	return layout.LockFile(statepaths.LockSpec{
		Host:            ref.Host,
		Owner:           ref.Owner,
		Repo:            ref.Repo,
		PRNumber:        ref.Number,
		HeadSHA:         run.SHA,
		BaseSHA:         run.BaseSHA,
		Profile:         run.Profile,
		PostingIdentity: run.PostingIdentity,
	})
}

func (o *Options) acquire(path string) (Lock, error) {
	acquire := o.Acquire
	if acquire == nil {
		acquire = func(path string) (Lock, error) { return runlock.Acquire(path) }
	}
	return acquire(path)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (r Request) postingKey() string {
	if strings.TrimSpace(r.PostingIdentityKey) != "" {
		return r.PostingIdentityKey
	}
	if strings.TrimSpace(r.PostingIdentity.Login) != "" {
		return r.PostingIdentity.Login
	}
	return r.PostingIdentity.ID
}

func validateOptions(opts *Options) error {
	if err := validateAbortOptions(opts); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("gateio: layout data root is required")
	}
	if opts.StaleHeartbeatThreshold <= 0 {
		return fmt.Errorf("gateio: stale heartbeat threshold must be positive")
	}
	return nil
}

func validateAbortOptions(opts *Options) error {
	if opts.Store == nil {
		return fmt.Errorf("gateio: store is required")
	}
	if opts.Provider == nil {
		return fmt.Errorf("gateio: provider is required")
	}
	return nil
}

func validateRequest(req Request) error {
	if err := req.PRRef.Validate(); err != nil {
		return err
	}
	if req.PR.Ref != req.PRRef {
		return fmt.Errorf("gateio: PR snapshot ref %+v does not match request ref %+v", req.PR.Ref, req.PRRef)
	}
	if strings.TrimSpace(req.PRKey) == "" {
		return fmt.Errorf("gateio: pr key is required")
	}
	expectedPRKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return err
	}
	if req.PRKey != expectedPRKey {
		return fmt.Errorf("gateio: pr key %q does not match request ref %q", req.PRKey, expectedPRKey)
	}
	if strings.TrimSpace(req.Profile) == "" {
		return fmt.Errorf("gateio: profile is required")
	}
	if strings.TrimSpace(req.postingKey()) == "" {
		return fmt.Errorf("gateio: posting identity is required")
	}
	if strings.TrimSpace(req.PR.Head.SHA) == "" || strings.TrimSpace(req.PR.Base.SHA) == "" {
		return fmt.Errorf("gateio: PR head and base SHA are required")
	}
	if !req.Flags.DryRun && strings.TrimSpace(req.ArtifactPath) == "" {
		return fmt.Errorf("gateio: artifact path is required")
	}
	return nil
}

func gatePostMode(mode ledger.PostMode) (gate.PostMode, error) {
	switch mode {
	case ledger.PostModeLive:
		return gate.PostModeLive, nil
	case ledger.PostModeDryRun:
		return gate.PostModeDryRun, nil
	default:
		return "", fmt.Errorf("gateio: unsupported post mode %q", mode)
	}
}

func gateRunState(outcome *ledger.Outcome) (gate.RunState, error) {
	if outcome == nil {
		return gate.RunStateRunning, nil
	}
	switch *outcome {
	case ledger.OutcomeIncomplete:
		return gate.RunStateIncomplete, nil
	case ledger.OutcomeApproved:
		return gate.RunStateApproved, nil
	case ledger.OutcomeRequestChanges:
		return gate.RunStateRequestChanges, nil
	case ledger.OutcomeComment:
		return gate.RunStateComment, nil
	case ledger.OutcomeNothingToReview:
		return gate.RunStateNothingToReview, nil
	case ledger.OutcomeDryRun:
		return gate.RunStateDryRun, nil
	case ledger.OutcomeAborted:
		return gate.RunStateAborted, nil
	case ledger.OutcomeFailed:
		return gate.RunStateFailed, nil
	default:
		return "", fmt.Errorf("gateio: unsupported outcome %q", *outcome)
	}
}

func heartbeatStale(run ledger.Run, now time.Time, threshold time.Duration) bool {
	last := run.StartedAt
	if run.HeartbeatAt != nil {
		last = *run.HeartbeatAt
	}
	return now.Sub(last) > threshold
}

func sameIdentity(author gitprovider.Identity, target gitprovider.Identity) bool {
	if strings.TrimSpace(author.ID) != "" && strings.TrimSpace(target.ID) != "" {
		return author.ID == target.ID
	}
	return strings.TrimSpace(author.Login) != "" && author.Login == target.Login
}

func markerKey(found marker.ActionMarker) string {
	return found.RunID + "\x00" + found.SHA + "\x00" + found.BaseSHA
}

func newest(current *markerRecord, candidate *markerRecord) *markerRecord {
	if current == nil {
		selected := *candidate
		return &selected
	}
	if candidate.when.After(current.when) || (candidate.when.Equal(current.when) && candidate.order > current.order) {
		selected := *candidate
		return &selected
	}
	return current
}

func newestSubmit(submits map[string]markerRecord) (markerRecord, bool) {
	var selected *markerRecord
	for _, record := range submits {
		selected = newest(selected, &record)
	}
	if selected == nil {
		return markerRecord{}, false
	}
	return *selected, true
}

func emitWarnings(w io.Writer, warnings []string) {
	if w == nil {
		return
	}
	for _, warning := range warnings {
		fmt.Fprintln(w, warning)
	}
}
