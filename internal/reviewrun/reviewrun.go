// Package reviewrun wires live review orchestration around gate, pipeline, and outbox.
package reviewrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	exitOK       = 0
	exitFailed   = 1
	exitAuth     = 3
	exitUpstream = 5

	freshRunIDAttempts = 3
)

// Store is the durable state required by live review orchestration.
type Store interface {
	gateio.Store
	ListRuns(context.Context) ([]ledger.Run, error)
	ListFindings(context.Context, string) ([]ledger.Finding, error)
	ListSessionsForRun(context.Context, string) ([]ledger.Session, error)
	UpsertNamedSession(context.Context, ledger.NamedSession) error
}

// Planner runs the CR-20 planning phases into a live run.
type Planner interface {
	Live(context.Context, pipeline.Request, ledger.Run) (pipeline.Result, error)
}

// Options contains live review dependencies.
type Options struct {
	Store                   Store
	Provider                gitprovider.GitProvider
	Planner                 Planner
	Limiter                 outbox.Limiter
	Layout                  statepaths.Layout
	Acquire                 gateio.AcquireFunc
	Now                     func() time.Time
	NewRunID                func() string
	StaleHeartbeatThreshold time.Duration
	Warnings                io.Writer
	ApprovalOverride        approvaloverride.Classifier
	Retention               datalifecycle.RetentionPolicy
	RetentionManualOnly     bool
}

// Flags contains live gate-affecting CLI flags.
type Flags struct {
	Rerun      bool
	RetryPosts bool
}

// Request identifies one live review.
type Request struct {
	Pipeline pipeline.Request
	Flags    Flags
}

// Result summarizes one live review invocation.
type Result struct {
	Status          gateio.Status
	Decision        gate.Decision
	Run             ledger.Run
	PR              gitprovider.PR
	PRKey           string
	Pipeline        *pipeline.Result
	Outbox          outbox.Result
	ExitCode        int
	Message         string
	FailOnTriggered bool
}

// Run executes one live review invocation.
func Run(ctx context.Context, opts Options, req Request) (Result, error) {
	if err := validateOptions(opts); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if req.Flags.Rerun && req.Flags.RetryPosts {
		return Result{}, fmt.Errorf("reviewrun: --rerun and --retry-posts are mutually exclusive")
	}
	if err := agents.RequireSafeProfileSources(req.Pipeline.Profile.AgentSources); err != nil {
		return Result{}, err
	}
	if err := pruneRetention(ctx, opts); err != nil {
		return Result{}, err
	}

	pr, err := opts.Provider.GetPR(ctx, req.Pipeline.PRRef)
	if err != nil {
		return Result{}, err
	}
	prKey, err := statepaths.PRKey(req.Pipeline.PRRef.Host, req.Pipeline.PRRef.Owner, req.Pipeline.PRRef.Repo, req.Pipeline.PRRef.Number)
	if err != nil {
		return Result{}, err
	}

	gateResult, err := evaluateGate(ctx, opts, req, pr, prKey)
	if err != nil {
		return Result{}, err
	}
	if gateResult.Lock != nil {
		defer func() { _ = gateResult.Lock.Release() }()
	}

	result := Result{
		Status:   gateResult.Status,
		Decision: gateResult.Decision,
		Run:      gateResult.Run,
		PR:       pr,
		PRKey:    prKey,
		ExitCode: exitOK,
	}
	switch gateResult.Status {
	case gateio.StatusContinue:
		return continueRun(ctx, opts, req, result)
	case gateio.StatusRepairExecuted, gateio.StatusRetryPostsExecuted, gateio.StatusApprovalOverride:
		result.Outbox = gateResult.OutboxResult
		result.ExitCode = gateResult.OutboxResult.ExitCode
		run, err := opts.Store.GetRun(ctx, gateResult.Run.RunID)
		if err != nil {
			return result, err
		}
		result.Run = run
		failOn, err := failOnFromStore(ctx, opts.Store, gateResult.Run.RunID, req.Pipeline.FailOn)
		if err != nil {
			return result, err
		}
		result.FailOnTriggered = failOn
		return result, nil
	case gateio.StatusEarlyExit:
		result.Message = gateResult.Decision.Message
		if strings.TrimSpace(result.Message) == "" {
			result.Message = "review already complete"
		}
		return result, nil
	case gateio.StatusBaseMovedAbort:
		result.ExitCode = exitUpstream
		result.Outbox = outbox.Result{Outcome: ledger.OutcomeAborted, ExitCode: exitUpstream, Aborted: true}
		if strings.TrimSpace(result.Run.RunID) != "" {
			run, err := opts.Store.GetRun(ctx, result.Run.RunID)
			if err != nil {
				return result, err
			}
			result.Run = run
		}
		result.Message = gateResult.Decision.Message
		return result, nil
	case gateio.StatusError:
		result.ExitCode = exitForDecision(gateResult.Decision)
		result.Message = gateResult.Decision.Message
		return result, nil
	case gateio.StatusDryRunFresh:
		return Result{}, fmt.Errorf("reviewrun: dry-run gate status is invalid for live review")
	default:
		return Result{}, fmt.Errorf("reviewrun: unsupported gate status %q", gateResult.Status)
	}
}

func evaluateGate(ctx context.Context, opts Options, req Request, pr gitprovider.PR, prKey string) (gateio.Result, error) {
	var lastErr error
	for attempt := 0; attempt < freshRunIDAttempts; attempt++ {
		runID := opts.newRunID()
		artifacts, err := pipeline.ArtifactPathsForRun(opts.Layout, req.Pipeline.PRRef, pr, req.Pipeline.ProfileName, postingKey(req.Pipeline.PostingIdentity), runID)
		if err != nil {
			return gateio.Result{}, err
		}
		result, err := gateio.Evaluate(ctx, gateio.Options{
			Store:                   opts.Store,
			Provider:                opts.Provider,
			Limiter:                 opts.Limiter,
			Layout:                  opts.Layout,
			Acquire:                 opts.Acquire,
			Now:                     opts.Now,
			StaleHeartbeatThreshold: opts.staleHeartbeatThreshold(),
			Warnings:                opts.Warnings,
			ApprovalOverride:        opts.ApprovalOverride,
		}, gateio.Request{
			PRRef:              req.Pipeline.PRRef,
			PR:                 pr,
			PRKey:              prKey,
			Profile:            req.Pipeline.ProfileName,
			PostingIdentity:    req.Pipeline.PostingIdentity,
			PostingIdentityKey: postingKey(req.Pipeline.PostingIdentity),
			FreshRunID:         runID,
			Flags: gate.Flags{
				Rerun:      req.Flags.Rerun,
				RetryPosts: req.Flags.RetryPosts,
			},
			ArtifactPath: artifacts.Dir,
		})
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ledger.ErrRunExists) {
			return gateio.Result{}, err
		}
		lastErr = err
	}
	return gateio.Result{}, fmt.Errorf("reviewrun: allocate fresh run ID after %d attempts: %w", freshRunIDAttempts, lastErr)
}

func continueRun(ctx context.Context, opts Options, req Request, result Result) (Result, error) {
	if strings.TrimSpace(result.Run.RunID) == "" {
		return Result{}, fmt.Errorf("reviewrun: gate continue missing run")
	}

	desired, planResult, err := planOrResume(ctx, opts, req, result)
	if err != nil {
		return result, err
	}
	if planResult != nil {
		result.Pipeline = planResult
		result.FailOnTriggered = planResult.FailOnTriggered
	} else {
		failOn, err := failOnFromStore(ctx, opts.Store, result.Run.RunID, req.Pipeline.FailOn)
		if err != nil {
			return result, err
		}
		result.FailOnTriggered = failOn
	}

	if moved, message, err := abortIfMoved(ctx, opts, req, result.Run); err != nil {
		return result, err
	} else if moved {
		run, err := opts.Store.GetRun(ctx, result.Run.RunID)
		if err != nil {
			return result, err
		}
		result.Run = run
		result.Outbox = outbox.Result{Outcome: ledger.OutcomeAborted, ExitCode: exitUpstream, Aborted: true}
		result.ExitCode = exitUpstream
		result.Message = message
		return result, nil
	}

	postResult, err := outbox.Post(ctx, outbox.Options{
		Store:    opts.Store,
		Provider: opts.Provider,
		Limiter:  opts.Limiter,
		Now:      opts.Now,
	}, outbox.Request{
		Run:             result.Run,
		PRRef:           req.Pipeline.PRRef,
		PostingIdentity: req.Pipeline.PostingIdentity,
		DesiredOutcome:  desired,
	})
	result.Outbox = postResult
	result.ExitCode = postResult.ExitCode
	if err != nil {
		return result, err
	}
	if !postResult.Aborted && planResult != nil && planResult.NamedSessionCandidate != nil {
		if err := opts.Store.UpsertNamedSession(ctx, *planResult.NamedSessionCandidate); err != nil {
			return result, err
		}
	}
	run, err := opts.Store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		return result, err
	}
	result.Run = run
	return result, nil
}

func planOrResume(ctx context.Context, opts Options, req Request, result Result) (ledger.Outcome, *pipeline.Result, error) {
	actions, err := opts.Store.ListPlannedActions(ctx, result.Run.RunID)
	if err != nil {
		return "", nil, err
	}
	if result.Decision.Kind == gate.DecisionResume && len(actions) > 0 {
		desired, err := desiredOutcomeFromActions(actions)
		if err != nil {
			if completeErr := opts.Store.CompleteRun(ctx, result.Run.RunID, ledger.OutcomeFailed, opts.now()); completeErr != nil {
				return "", nil, completeErr
			}
			return "", nil, fmt.Errorf("reviewrun: cannot derive desired outcome from stored actions for run %s; pass --rerun to start a fresh review: %w", result.Run.RunID, err)
		}
		return desired, nil, nil
	}
	if result.Decision.Kind == gate.DecisionResume {
		empty, err := planningStateEmpty(ctx, opts.Store, result.Run.RunID)
		if err != nil {
			return "", nil, err
		}
		if !empty {
			hasTasks, err := pipeline.HasLLMTaskMetadata(result.Run.ArtifactPath)
			if err != nil {
				return "", nil, err
			}
			if !hasTasks {
				if completeErr := opts.Store.CompleteRun(ctx, result.Run.RunID, ledger.OutcomeFailed, opts.now()); completeErr != nil {
					return "", nil, completeErr
				}
				return "", nil, fmt.Errorf("reviewrun: run %s has partial planning state without planned actions; pass --rerun to start a fresh review", result.Run.RunID)
			}
		}
	}

	planned, err := opts.Planner.Live(ctx, req.Pipeline, result.Run)
	if err != nil {
		return "", nil, err
	}
	desired, err := desiredOutcomeFromPlan(planned.Plan.Outcome)
	if err != nil {
		return "", nil, err
	}
	return desired, &planned, nil
}

func planningStateEmpty(ctx context.Context, store Store, runID string) (bool, error) {
	sessions, err := store.ListSessionsForRun(ctx, runID)
	if err != nil {
		return false, err
	}
	if len(sessions) > 0 {
		return false, nil
	}
	findings, err := store.ListFindings(ctx, runID)
	if err != nil {
		return false, err
	}
	return len(findings) == 0, nil
}

func abortIfMoved(ctx context.Context, opts Options, req Request, run ledger.Run) (bool, string, error) {
	pr, err := opts.Provider.GetPR(ctx, req.Pipeline.PRRef)
	if err != nil {
		return false, "", err
	}
	if pr.Head.SHA == run.SHA && pr.Base.SHA == run.BaseSHA {
		return false, "", nil
	}
	if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeAborted, opts.now()); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("review premises moved: head %s -> %s, base %s -> %s", run.SHA, pr.Head.SHA, run.BaseSHA, pr.Base.SHA), nil
}

func desiredOutcomeFromPlan(outcome reviewplan.Outcome) (ledger.Outcome, error) {
	switch outcome {
	case reviewplan.OutcomeApproved:
		return ledger.OutcomeApproved, nil
	case reviewplan.OutcomeRequestChanges:
		return ledger.OutcomeRequestChanges, nil
	case reviewplan.OutcomeComment:
		return ledger.OutcomeComment, nil
	case reviewplan.OutcomeNothingToReview:
		return ledger.OutcomeNothingToReview, nil
	default:
		return "", fmt.Errorf("reviewrun: unsupported plan outcome %q", outcome)
	}
}

func desiredOutcomeFromActions(actions []ledger.PlannedAction) (ledger.Outcome, error) {
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
			var payload outbox.SubmitReviewPayload
			if err := json.Unmarshal([]byte(action.PayloadJSON), &payload); err != nil {
				return "", fmt.Errorf("reviewrun: decode submit_review payload %q: %w", action.ActionID, err)
			}
			outcome, err := outcomeFromReviewEvent(payload.Event)
			if err != nil {
				return "", err
			}
			if haveSubmitOutcome && submitOutcome != outcome {
				return "", fmt.Errorf("reviewrun: conflicting required submit_review outcomes")
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
		return submitOutcome, nil
	}
	if haveRequiredRollup && !haveOtherRequired {
		return ledger.OutcomeNothingToReview, nil
	}
	return "", fmt.Errorf("reviewrun: desired outcome cannot be derived from planned actions")
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
		return "", fmt.Errorf("reviewrun: unsupported submit_review event %q", event)
	}
}

func failOnFromStore(ctx context.Context, store Store, runID string, threshold *review.Severity) (bool, error) {
	if threshold == nil || strings.TrimSpace(runID) == "" {
		return false, nil
	}
	findings, err := store.ListFindings(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, finding := range findings {
		if finding.Severity.AtLeast(*threshold) {
			return true, nil
		}
	}
	return false, nil
}

func pruneRetention(ctx context.Context, opts Options) error {
	if opts.RetentionManualOnly {
		return nil
	}
	result, err := datalifecycle.Prune(ctx, datalifecycle.Options{
		Layout: opts.Layout,
		Store:  opts.Store,
		Now:    opts.Now,
	}, datalifecycle.PruneOptions{Retention: opts.Retention})
	if err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if opts.Warnings != nil {
			_, _ = fmt.Fprintf(opts.Warnings, "warning: %s\n", warning)
		}
	}
	return nil
}

func exitForDecision(decision gate.Decision) int {
	if decision.FailureClass == gate.FailureClassAuth {
		return exitAuth
	}
	return exitFailed
}

func validateOptions(opts Options) error {
	if opts.Store == nil {
		return fmt.Errorf("reviewrun: store is required")
	}
	if opts.Provider == nil {
		return fmt.Errorf("reviewrun: provider is required")
	}
	if opts.Planner == nil {
		return fmt.Errorf("reviewrun: planner is required")
	}
	if opts.Limiter == nil {
		return fmt.Errorf("reviewrun: outbox limiter is required")
	}
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("reviewrun: layout data root is required")
	}
	return nil
}

func validateRequest(req Request) error {
	if err := req.Pipeline.PRRef.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(req.Pipeline.ProfileName) == "" {
		return fmt.Errorf("reviewrun: profile is required")
	}
	if strings.TrimSpace(postingKey(req.Pipeline.PostingIdentity)) == "" {
		return fmt.Errorf("reviewrun: posting identity is required")
	}
	return nil
}

func (opts Options) staleHeartbeatThreshold() time.Duration {
	if opts.StaleHeartbeatThreshold > 0 {
		return opts.StaleHeartbeatThreshold
	}
	return 10 * time.Minute
}

func (opts Options) now() time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}
	return time.Now().UTC()
}

func (opts Options) newRunID() string {
	if opts.NewRunID != nil {
		if runID := strings.TrimSpace(opts.NewRunID()); runID != "" {
			return runID
		}
	}
	return uuid.NewString()
}

func postingKey(identity gitprovider.Identity) string {
	if strings.TrimSpace(identity.Login) != "" {
		return identity.Login
	}
	if strings.TrimSpace(identity.ID) != "" {
		return identity.ID
	}
	return identity.DisplayName
}
