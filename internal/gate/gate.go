// Package gate decides whether a codereview run should resume, repair, exit,
// retry posts, abort stale state, or run fresh from summarized state.
package gate

import "fmt"

// DecisionKind identifies the next gate action.
type DecisionKind string

// Decision kind values.
const (
	DecisionResume     DecisionKind = "resume"
	DecisionEarlyExit  DecisionKind = "early_exit"
	DecisionRepair     DecisionKind = "repair"
	DecisionRetryPosts DecisionKind = "retry_posts"
	DecisionAbortStale DecisionKind = "abort_stale"
	DecisionFresh      DecisionKind = "fresh"
	DecisionError      DecisionKind = "error"
)

// Valid reports whether k is a known decision kind.
func (k DecisionKind) Valid() bool {
	switch k {
	case DecisionResume, DecisionEarlyExit, DecisionRepair, DecisionRetryPosts, DecisionAbortStale, DecisionFresh, DecisionError:
		return true
	default:
		return false
	}
}

// Flags contains gate-affecting CLI flags.
type Flags struct {
	Rerun      bool
	RetryPosts bool
	DryRun     bool
}

// PostMode records whether a summarized run is live or dry-run.
type PostMode string

// Post mode values.
const (
	PostModeLive   PostMode = "live"
	PostModeDryRun PostMode = "dry_run"
)

// Valid reports whether m is a known post mode.
func (m PostMode) Valid() bool {
	switch m {
	case PostModeLive, PostModeDryRun:
		return true
	default:
		return false
	}
}

// RunState records the summarized local run outcome state.
type RunState string

// Run state values.
const (
	RunStateRunning         RunState = "running"
	RunStateIncomplete      RunState = "incomplete"
	RunStateApproved        RunState = "approved"
	RunStateRequestChanges  RunState = "request_changes"
	RunStateComment         RunState = "comment"
	RunStateNothingToReview RunState = "nothing_to_review"
	RunStateDryRun          RunState = "dry_run"
	RunStateAborted         RunState = "aborted"
	RunStateFailed          RunState = "failed"
)

// Valid reports whether s is a known run state.
func (s RunState) Valid() bool {
	switch s {
	case RunStateRunning, RunStateIncomplete, RunStateApproved, RunStateRequestChanges, RunStateComment, RunStateNothingToReview, RunStateDryRun, RunStateAborted, RunStateFailed:
		return true
	default:
		return false
	}
}

// FailureClass records whether a failed action was auth-related or terminal.
type FailureClass string

// Failure class values.
const (
	FailureClassNone     FailureClass = ""
	FailureClassTerminal FailureClass = "terminal"
	FailureClassAuth     FailureClass = "auth"
)

// Valid reports whether c is a known failure class.
func (c FailureClass) Valid() bool {
	switch c {
	case FailureClassNone, FailureClassTerminal, FailureClassAuth:
		return true
	default:
		return false
	}
}

// LockState records the non-blocking stale-base lock probe result.
type LockState string

// Lock state values.
const (
	LockStateFree LockState = "free"
	LockStateHeld LockState = "held"
)

// Valid reports whether s is a known lock state.
func (s LockState) Valid() bool {
	switch s {
	case LockStateFree, LockStateHeld:
		return true
	default:
		return false
	}
}

// PRState records the summarized PR marker state.
type PRState string

// PR state values.
const (
	PRStateFresh          PRState = "fresh"
	PRStateCompleteReview PRState = "complete_review"
	PRStateCompleteNoDiff PRState = "complete_no_diff"
	PRStatePartial        PRState = "partial"
	PRStateStaleBase      PRState = "stale_base"
)

// Valid reports whether s is a known PR marker state.
func (s PRState) Valid() bool {
	switch s {
	case PRStateFresh, PRStateCompleteReview, PRStateCompleteNoDiff, PRStatePartial, PRStateStaleBase:
		return true
	default:
		return false
	}
}

// PROutcome records the verdict from a rollup marker.
type PROutcome string

// PR outcome values.
const (
	PROutcomeApproved        PROutcome = "approved"
	PROutcomeRequestChanges  PROutcome = "request_changes"
	PROutcomeComment         PROutcome = "comment"
	PROutcomeNothingToReview PROutcome = "nothing_to_review"
)

// Valid reports whether o is a known PR outcome.
func (o PROutcome) Valid() bool {
	switch o {
	case PROutcomeApproved, PROutcomeRequestChanges, PROutcomeComment, PROutcomeNothingToReview:
		return true
	default:
		return false
	}
}

// RealVerdict reports whether o is a review verdict that needs submit_review.
func (o PROutcome) RealVerdict() bool {
	switch o {
	case PROutcomeApproved, PROutcomeRequestChanges, PROutcomeComment:
		return true
	case PROutcomeNothingToReview:
		return false
	default:
		return false
	}
}

// ErrorReason identifies why the kernel returned DecisionError.
type ErrorReason string

// Error reason values.
const (
	ErrorInvalidInput                 ErrorReason = "invalid_input"
	ErrorMutuallyExclusiveFlags       ErrorReason = "mutually_exclusive_flags"
	ErrorRetryPostsIneligible         ErrorReason = "retry_posts_ineligible"
	ErrorPartialFailed                ErrorReason = "partial_failed"
	ErrorPartialResumableInconsistent ErrorReason = "partial_resumable_inconsistent"
)

// Valid reports whether r is a known error reason.
func (r ErrorReason) Valid() bool {
	switch r {
	case ErrorInvalidInput, ErrorMutuallyExclusiveFlags, ErrorRetryPostsIneligible, ErrorPartialFailed, ErrorPartialResumableInconsistent:
		return true
	default:
		return false
	}
}

// RunSummary is the local ledger state needed by the pure gate.
type RunSummary struct {
	RunID                  string
	Attempt                int
	PostMode               PostMode
	State                  RunState
	RequiredPending        int
	RequiredFailedTerminal int
	FailureClass           FailureClass
}

// StaleBaseCandidate is a different-base local row plus its lock probe state.
type StaleBaseCandidate struct {
	Run            RunSummary
	LockState      LockState
	HeartbeatStale bool
}

// PRSummary is the parsed PR marker state for the current head/base context.
type PRSummary struct {
	State   PRState
	RunID   string
	Outcome PROutcome
}

// Request contains all summarized state used by Decide.
type Request struct {
	Flags               Flags
	ExactRuns           []RunSummary
	StaleBaseCandidates []StaleBaseCandidate
	PR                  PRSummary
	PartialRun          *RunSummary
}

// Decision is the pure gate output consumed by later IO integration.
type Decision struct {
	Kind             DecisionKind
	RunID            string
	Outcome          PROutcome
	SupersedeRunIDs  []string
	AbortStaleRunIDs []string
	Warnings         []string
	ErrorReason      ErrorReason
	FailureClass     FailureClass
	Message          string
}

// Decide applies the codereview gate state machine to summarized inputs.
func Decide(req Request) Decision {
	if req.Flags.Rerun && req.Flags.RetryPosts {
		return errorDecision(ErrorMutuallyExclusiveFlags, "--rerun and --retry-posts are mutually exclusive")
	}
	if req.Flags.DryRun {
		return Decision{Kind: DecisionFresh, Message: "dry-run bypasses live gate"}
	}
	if req.Flags.Rerun {
		return decideRerun(req.ExactRuns)
	}
	if req.Flags.RetryPosts {
		return decideRetryPosts(req.ExactRuns)
	}

	if decision := validateRuns(req.ExactRuns); decision.Kind == DecisionError {
		return decision
	}

	if run, ok := latestMatchingRun(req.ExactRuns, resumable); ok {
		return Decision{Kind: DecisionResume, RunID: run.RunID}
	}

	warnings, staleDecision := decideStaleBase(req.StaleBaseCandidates)
	if staleDecision.Kind == DecisionError || staleDecision.Kind == DecisionAbortStale {
		staleDecision.Warnings = append(staleDecision.Warnings, warnings...)
		return staleDecision
	}

	if decision := validatePR(req.PR); decision.Kind == DecisionError {
		return decision
	}
	if req.PR.State == PRStatePartial && req.PartialRun != nil {
		if decision := validateRun(*req.PartialRun); decision.Kind == DecisionError {
			return decision
		}
		if req.PartialRun.RunID != req.PR.RunID {
			return invalidInput(fmt.Sprintf("partial marker run ID %q does not match local row run ID %q", req.PR.RunID, req.PartialRun.RunID))
		}
	}

	decision := decidePR(req.PR, req.PartialRun)
	decision.Warnings = append(decision.Warnings, warnings...)
	return decision
}

func decideRerun(runs []RunSummary) Decision {
	if decision := validateRuns(runs); decision.Kind == DecisionError {
		return decision
	}
	var supersede []string
	for _, run := range runs {
		if resumable(run) {
			supersede = append(supersede, run.RunID)
		}
	}
	return Decision{
		Kind:            DecisionFresh,
		SupersedeRunIDs: supersede,
		Message:         "--rerun bypasses gate",
	}
}

func decideRetryPosts(runs []RunSummary) Decision {
	if decision := validateRuns(runs); decision.Kind == DecisionError {
		return decision
	}
	run, ok := latestMatchingRun(runs, retryEligible)
	if !ok {
		return errorDecision(ErrorRetryPostsIneligible, "no live run has required pending or failed_terminal actions")
	}
	return Decision{Kind: DecisionRetryPosts, RunID: run.RunID}
}

func decideStaleBase(candidates []StaleBaseCandidate) ([]string, Decision) {
	var warnings []string
	var abortRunIDs []string
	for _, candidate := range candidates {
		run := candidate.Run
		if decision := validateRunShape(run); decision.Kind == DecisionError {
			return warnings, decision
		}
		if !resumable(run) {
			continue
		}
		if run.RunID == "" {
			return warnings, invalidInput("stale-base candidate is missing run ID")
		}
		if !candidate.LockState.Valid() {
			return warnings, invalidInput(fmt.Sprintf("stale-base candidate %q has invalid lock state %q", run.RunID, candidate.LockState))
		}
		switch candidate.LockState {
		case LockStateFree:
			abortRunIDs = append(abortRunIDs, run.RunID)
		case LockStateHeld:
			if candidate.HeartbeatStale {
				warnings = append(warnings, fmt.Sprintf("stale-base run %s is locked and has a stale heartbeat", run.RunID))
			}
		}
	}
	if len(abortRunIDs) > 0 {
		return warnings, Decision{Kind: DecisionAbortStale, AbortStaleRunIDs: abortRunIDs}
	}
	return warnings, Decision{}
}

func decidePR(pr PRSummary, partialRun *RunSummary) Decision {
	switch pr.State {
	case PRStateFresh:
		return Decision{Kind: DecisionFresh}
	case PRStateCompleteReview:
		return Decision{Kind: DecisionEarlyExit, RunID: pr.RunID}
	case PRStateCompleteNoDiff:
		return Decision{Kind: DecisionEarlyExit, RunID: pr.RunID, Outcome: pr.Outcome}
	case PRStateStaleBase:
		return Decision{Kind: DecisionFresh, RunID: pr.RunID, Message: "stale-base marker is audit only; run fresh"}
	case PRStatePartial:
		if partialRun == nil {
			return Decision{Kind: DecisionRepair, RunID: pr.RunID, Outcome: pr.Outcome}
		}
		if resumable(*partialRun) {
			return errorDecision(ErrorPartialResumableInconsistent, "partial marker local row is resumable; local resume should have won")
		}
		switch partialRun.State {
		case RunStateFailed:
			return Decision{
				Kind:         DecisionError,
				RunID:        partialRun.RunID,
				Outcome:      pr.Outcome,
				ErrorReason:  ErrorPartialFailed,
				FailureClass: partialRun.FailureClass,
				Message:      "partial marker run failed; use --retry-posts after fixing the cause or --rerun",
			}
		case RunStateAborted:
			return Decision{Kind: DecisionFresh, RunID: partialRun.RunID, Outcome: pr.Outcome, Message: "partial marker belongs to aborted run; run fresh"}
		case RunStateRunning, RunStateIncomplete, RunStateApproved, RunStateRequestChanges, RunStateComment, RunStateNothingToReview, RunStateDryRun:
			return invalidInput(fmt.Sprintf("partial marker local row %q has unsupported state %q", partialRun.RunID, partialRun.State))
		default:
			return invalidInput(fmt.Sprintf("partial marker local row %q has unsupported state %q", partialRun.RunID, partialRun.State))
		}
	default:
		return invalidInput(fmt.Sprintf("invalid PR state %q", pr.State))
	}
}

func validateRuns(runs []RunSummary) Decision {
	for _, run := range runs {
		if decision := validateRun(run); decision.Kind == DecisionError {
			return decision
		}
	}
	return Decision{}
}

func validateRun(run RunSummary) Decision {
	if decision := validateRunShape(run); decision.Kind == DecisionError {
		return decision
	}
	if run.RunID == "" {
		return invalidInput("run summary is missing run ID")
	}
	return Decision{}
}

func validateRunShape(run RunSummary) Decision {
	if !run.PostMode.Valid() {
		return invalidInput(fmt.Sprintf("run %q has invalid post mode %q", run.RunID, run.PostMode))
	}
	if !run.State.Valid() {
		return invalidInput(fmt.Sprintf("run %q has invalid state %q", run.RunID, run.State))
	}
	if run.Attempt <= 0 {
		return invalidInput(fmt.Sprintf("run %q has non-positive attempt %d", run.RunID, run.Attempt))
	}
	if run.RequiredPending < 0 || run.RequiredFailedTerminal < 0 {
		return invalidInput(fmt.Sprintf("run %q has negative required action counts", run.RunID))
	}
	if !run.FailureClass.Valid() {
		return invalidInput(fmt.Sprintf("run %q has invalid failure class %q", run.RunID, run.FailureClass))
	}
	return Decision{}
}

func validatePR(pr PRSummary) Decision {
	switch pr.State {
	case PRStateFresh, PRStateStaleBase:
		return Decision{}
	case PRStateCompleteReview:
		if pr.RunID == "" {
			return invalidInput("complete review marker is missing run ID")
		}
		if pr.Outcome != "" && !pr.Outcome.Valid() {
			return invalidInput(fmt.Sprintf("complete review marker has invalid outcome %q", pr.Outcome))
		}
		return Decision{}
	case PRStateCompleteNoDiff:
		if pr.RunID == "" {
			return invalidInput("complete no-diff marker is missing run ID")
		}
		if pr.Outcome != PROutcomeNothingToReview {
			return invalidInput("complete no-diff marker must carry outcome nothing_to_review")
		}
		return Decision{}
	case PRStatePartial:
		if pr.RunID == "" {
			return invalidInput("partial marker is missing run ID")
		}
		if !pr.Outcome.RealVerdict() {
			return invalidInput("partial marker must carry a real verdict outcome")
		}
		return Decision{}
	default:
		return invalidInput(fmt.Sprintf("invalid PR state %q", pr.State))
	}
}

func latestMatchingRun(runs []RunSummary, match func(RunSummary) bool) (RunSummary, bool) {
	var selected RunSummary
	found := false
	for _, run := range runs {
		if !match(run) {
			continue
		}
		if !found || run.Attempt > selected.Attempt {
			selected = run
			found = true
		}
	}
	return selected, found
}

func resumable(run RunSummary) bool {
	if run.PostMode != PostModeLive {
		return false
	}
	switch run.State {
	case RunStateRunning, RunStateIncomplete:
		return true
	case RunStateApproved, RunStateRequestChanges, RunStateComment, RunStateNothingToReview, RunStateDryRun, RunStateAborted, RunStateFailed:
		return false
	default:
		return false
	}
}

func retryEligible(run RunSummary) bool {
	return run.PostMode == PostModeLive && run.RequiredPending+run.RequiredFailedTerminal > 0
}

func invalidInput(message string) Decision {
	return errorDecision(ErrorInvalidInput, message)
}

func errorDecision(reason ErrorReason, message string) Decision {
	return Decision{Kind: DecisionError, ErrorReason: reason, Message: message}
}
