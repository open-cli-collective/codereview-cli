// Package threadrespond plans and posts responses to existing inline review
// threads through the shared LLM, reviewplan, ledger, and outbox boundaries.
package threadrespond

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/plannedactions"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadanalysis"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

const (
	exitOK       = 0
	exitFailed   = 1
	exitUpstream = 5

	freshRunIDAttempts = 3
	lockPRAttempts     = 3

	responseRunMarkerSchema = 1
)

// Store is the durable state required by response planning and posting.
type Store interface {
	llmlifecycle.Store
	GetRun(context.Context, string) (ledger.Run, error)
	ListRunsForHeadScope(context.Context, ledger.ListRunsForHeadScopeParams) ([]ledger.Run, error)
	AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error)
	InsertPlannedAction(context.Context, ledger.PlannedAction) error
	InsertPlannedActions(context.Context, []ledger.PlannedAction) error
	ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error)
	UpdatePlannedAction(context.Context, ledger.PlannedAction) error
	CompleteRun(context.Context, string, ledger.Outcome, time.Time) error
}

// Lock is a held live-run lock.
type Lock interface {
	Release() error
}

// AcquireFunc acquires one non-blocking live-run lock.
type AcquireFunc func(string) (Lock, error)

// Options contains response-run dependencies.
type Options struct {
	Store    Store
	Provider gitprovider.GitProvider
	Adapter  llm.Adapter
	Limiter  outbox.Limiter
	Layout   statepaths.Layout
	Acquire  AcquireFunc
	Now      func() time.Time

	NewRunID       func() string
	NewActionID    func(reviewplan.ActionKind) (string, error)
	NewStepID      func() string
	ModelTier      config.ModelTier
	ModelOverride  string
	EffortOverride string
}

// Request identifies one response run.
type Request struct {
	PRRef            gitprovider.PRRef
	PRURL            string
	ProfileName      string
	Profile          config.Profile
	PostingIdentity  gitprovider.Identity
	DryRun           bool
	NoResolveThreads bool
	Rerun            bool
	RetryRunID       string
}

// Result summarizes a response invocation.
type Result struct {
	Run             ledger.Run
	PR              gitprovider.PR
	PRKey           string
	Artifacts       pipeline.ArtifactPaths
	Threads         []threadcontext.Thread
	EligibleThreads []threadcontext.Thread
	Analyses        []threadanalysis.Result
	Responses       []review.ThreadResponseAction
	Plan            reviewplan.Plan
	PlannedActions  []ledger.PlannedAction
	Outbox          outbox.Result
	ExitCode        int
	Message         string
}

// Run executes a fresh response run or same-run retry.
func Run(ctx context.Context, opts Options, req Request) (Result, error) {
	if err := validateOptions(opts); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(req.RetryRunID) != "" {
		return retry(ctx, opts, req)
	}
	return fresh(ctx, opts, req)
}

func fresh(ctx context.Context, opts Options, req Request) (res Result, err error) {
	pr, lock, err := readPRWithOptionalLock(ctx, opts, req, !req.DryRun)
	if err != nil {
		return Result{ExitCode: exitUpstream}, err
	}
	if lock != nil {
		defer func() { _ = lock.Release() }()
	}
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return Result{ExitCode: exitFailed}, err
	}

	providerThreads, err := opts.Provider.ListInlineThreads(ctx, req.PRRef)
	if err != nil {
		return Result{PR: pr, PRKey: prKey, ExitCode: exitUpstream}, err
	}
	threads, err := threadcontext.Normalize(providerThreads, threadcontext.Options{PostingIdentity: req.PostingIdentity})
	if err != nil {
		return Result{PR: pr, PRKey: prKey, ExitCode: exitFailed}, err
	}
	eligible := eligibleThreads(threads)

	mode := postMode(req)
	run, artifacts, err := allocateOrResumeRun(ctx, opts, req, pr, prKey, mode)
	if err != nil {
		return Result{PR: pr, PRKey: prKey, Artifacts: artifacts, Threads: threads, EligibleThreads: eligible, ExitCode: exitFailed}, err
	}
	result := Result{
		Run:             run,
		PR:              pr,
		PRKey:           prKey,
		Artifacts:       artifacts,
		Threads:         threads,
		EligibleThreads: eligible,
		ExitCode:        exitOK,
	}
	defer completeFailedOnError(ctx, opts, &result, &err)

	existingActions, err := opts.Store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		return result, err
	}
	if len(existingActions) > 0 {
		if err := validateResponseActions(existingActions); err != nil {
			return result, err
		}
		result.PlannedActions = existingActions
	} else {
		if len(eligible) > 0 {
			if opts.Adapter == nil {
				return result, fmt.Errorf("threadrespond: adapter is required")
			}
			runtime, err := resolveRuntime(opts, req)
			if err != nil {
				return result, err
			}
			analyses, err := analyzeThreads(ctx, opts, run, artifacts, runtime, eligible)
			if err != nil {
				return result, err
			}
			result.Analyses = analyses
			result.Responses = responsesFromAnalyses(analyses)
		}

		plan, err := reviewplan.BuildThreadResponses(reviewplan.ThreadResponseRequest{
			PostMode:     planPostMode(req),
			ProviderCaps: effectiveCaps(opts.Provider.Capabilities(), req),
			Responses:    result.Responses,
			Now:          opts.now,
			NewActionID:  opts.newActionID,
		})
		if err != nil {
			return result, err
		}
		result.Plan = plan
		plannedActions := make([]ledger.PlannedAction, 0, len(plan.Actions))
		for _, action := range plan.Actions {
			planned, err := plannedactions.FromReviewPlan(run.RunID, action)
			if err != nil {
				return result, err
			}
			plannedActions = append(plannedActions, planned)
		}
		if err := opts.Store.InsertPlannedActions(ctx, plannedActions); err != nil {
			return result, err
		}
		result.PlannedActions = plannedActions
	}

	if req.DryRun {
		if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeDryRun, opts.now()); err != nil {
			return result, err
		}
		run, err := opts.Store.GetRun(ctx, run.RunID)
		if err != nil {
			return result, err
		}
		result.Run = run
		return result, nil
	}
	if len(result.PlannedActions) == 0 {
		if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeNothingToReview, opts.now()); err != nil {
			return result, err
		}
		run, err := opts.Store.GetRun(ctx, run.RunID)
		if err != nil {
			return result, err
		}
		result.Run = run
		result.Outbox = outbox.Result{Outcome: ledger.OutcomeNothingToReview, ExitCode: exitOK}
		return result, nil
	}
	if moved, message, err := abortIfMoved(ctx, opts, req, run); err != nil {
		result.ExitCode = exitUpstream
		return result, err
	} else if moved {
		run, err := opts.Store.GetRun(ctx, run.RunID)
		if err != nil {
			return result, err
		}
		result.Run = run
		result.Outbox = outbox.Result{Outcome: ledger.OutcomeAborted, ExitCode: exitUpstream, Aborted: true}
		result.ExitCode = exitUpstream
		result.Message = message
		return result, nil
	}
	if opts.Limiter == nil {
		return result, fmt.Errorf("threadrespond: outbox limiter is required")
	}

	postResult, err := outbox.Post(ctx, outbox.Options{
		Store:    opts.Store,
		Provider: opts.Provider,
		Limiter:  opts.Limiter,
		Now:      opts.now,
	}, outbox.Request{
		Run:             run,
		PRRef:           req.PRRef,
		PostingIdentity: req.PostingIdentity,
		DesiredOutcome:  desiredOutcomeForRetry(result.PlannedActions),
	})
	result.Outbox = postResult
	result.ExitCode = postResult.ExitCode
	if err != nil {
		return result, err
	}
	run, err = opts.Store.GetRun(ctx, run.RunID)
	if err != nil {
		return result, err
	}
	result.Run = run
	return result, nil
}

func retry(ctx context.Context, opts Options, req Request) (Result, error) {
	if req.DryRun {
		return Result{}, fmt.Errorf("threadrespond: --retry-posts cannot be used with dry-run")
	}
	pr, lock, err := readPRWithOptionalLock(ctx, opts, req, true)
	if err != nil {
		return Result{ExitCode: exitUpstream}, err
	}
	defer func() { _ = lock.Release() }()
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return Result{PR: pr, ExitCode: exitFailed}, err
	}
	run, err := opts.Store.GetRun(ctx, strings.TrimSpace(req.RetryRunID))
	if err != nil {
		return Result{PR: pr, PRKey: prKey, ExitCode: exitFailed}, err
	}
	result := Result{Run: run, PR: pr, PRKey: prKey, Artifacts: pipeline.ArtifactPathsFromDir(run.ArtifactPath), ExitCode: exitOK}
	if err := validateRetryRun(req, pr, prKey, run); err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	actions, err := opts.Store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	if err := validateResponseActions(actions); err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	if opts.Limiter == nil {
		result.ExitCode = exitFailed
		return result, fmt.Errorf("threadrespond: outbox limiter is required")
	}
	if err := resetFailedTerminalResponseActions(ctx, opts.Store, actions); err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	result.PlannedActions = actions
	if moved, message, err := abortIfMoved(ctx, opts, req, run); err != nil {
		result.ExitCode = exitUpstream
		return result, err
	} else if moved {
		run, err := opts.Store.GetRun(ctx, run.RunID)
		if err != nil {
			result.ExitCode = exitFailed
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
		Now:      opts.now,
	}, outbox.Request{
		Run:             run,
		PRRef:           req.PRRef,
		PostingIdentity: req.PostingIdentity,
		DesiredOutcome:  desiredOutcomeForRetry(actions),
	})
	result.Outbox = postResult
	result.ExitCode = postResult.ExitCode
	if err != nil {
		return result, err
	}
	run, err = opts.Store.GetRun(ctx, run.RunID)
	if err != nil {
		return result, err
	}
	result.Run = run
	return result, nil
}

func readPRWithOptionalLock(ctx context.Context, opts Options, req Request, locked bool) (gitprovider.PR, Lock, error) {
	if !locked {
		pr, err := opts.Provider.GetPR(ctx, req.PRRef)
		return pr, nil, err
	}
	var lastErr error
	for attempt := 0; attempt < lockPRAttempts; attempt++ {
		pr, err := opts.Provider.GetPR(ctx, req.PRRef)
		if err != nil {
			return gitprovider.PR{}, nil, err
		}
		path, err := lockPath(opts.Layout, req, pr)
		if err != nil {
			return gitprovider.PR{}, nil, err
		}
		lock, err := opts.acquire(path)
		if err != nil {
			return gitprovider.PR{}, nil, err
		}
		lockedPR, err := opts.Provider.GetPR(ctx, req.PRRef)
		if err != nil {
			_ = lock.Release()
			return gitprovider.PR{}, nil, err
		}
		if lockedPR.Head.SHA == pr.Head.SHA && lockedPR.Base.SHA == pr.Base.SHA {
			return lockedPR, lock, nil
		}
		_ = lock.Release()
		lastErr = fmt.Errorf("threadrespond: PR moved before lock stabilized")
	}
	return gitprovider.PR{}, nil, lastErr
}

func abortIfMoved(ctx context.Context, opts Options, req Request, run ledger.Run) (bool, string, error) {
	pr, err := opts.Provider.GetPR(ctx, req.PRRef)
	if err != nil {
		return false, "", err
	}
	if pr.Head.SHA == run.SHA && pr.Base.SHA == run.BaseSHA {
		return false, "", nil
	}
	if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeAborted, opts.now()); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("threadrespond premises moved: head %s -> %s, base %s -> %s", run.SHA, pr.Head.SHA, run.BaseSHA, pr.Base.SHA), nil
}

func analyzeThreads(ctx context.Context, opts Options, run ledger.Run, artifacts pipeline.ArtifactPaths, runtime llmRuntime, threads []threadcontext.Thread) ([]threadanalysis.Result, error) {
	results := make([]threadanalysis.Result, 0, len(threads))
	for _, thread := range threads {
		result, err := threadanalysis.AnalyzeThread(ctx, threadanalysis.Options{
			Store:          opts.Store,
			RunID:          run.RunID,
			Adapter:        opts.Adapter,
			Model:          runtime.model,
			Effort:         runtime.effort,
			LogPath:        threadLogPath(artifacts, thread.ID),
			LifecyclePaths: llmlifecycle.Paths{LLMTasksDir: artifacts.LLMTasksDir},
			Now:            opts.now,
			NewStepID:      opts.newStepID,
		}, thread)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func eligibleThreads(threads []threadcontext.Thread) []threadcontext.Thread {
	out := make([]threadcontext.Thread, 0, len(threads))
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		if !thread.Status.CRAuthoredFinding || !thread.Status.PendingHumanReply {
			continue
		}
		out = append(out, thread)
	}
	return out
}

func responsesFromAnalyses(analyses []threadanalysis.Result) []review.ThreadResponseAction {
	responses := make([]review.ThreadResponseAction, 0, len(analyses))
	for _, analysis := range analyses {
		response, ok := responseFromAnalysis(analysis)
		if ok {
			responses = append(responses, response)
		}
	}
	return responses
}

func responseFromAnalysis(result threadanalysis.Result) (review.ThreadResponseAction, bool) {
	threadID := strings.TrimSpace(result.ThreadID)
	switch result.Decision {
	case threadanalysis.DecisionSkip:
		return review.ThreadResponseAction{}, false
	case threadanalysis.DecisionReplyOnly, threadanalysis.DecisionClarify:
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseReply,
			ThreadID:  threadID,
			Body:      result.ReplyBody,
			Rationale: result.Rationale,
		}, true
	case threadanalysis.DecisionAcknowledge, threadanalysis.DecisionConcede:
		if strings.TrimSpace(result.Summary) == "" {
			return review.ThreadResponseAction{
				Kind:      review.ThreadResponseReply,
				ThreadID:  threadID,
				Body:      result.ReplyBody,
				Rationale: result.Rationale,
			}, true
		}
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseSummaryReply,
			ThreadID:  threadID,
			Body:      combineReplyAndSummary(result.ReplyBody, result.Summary),
			Resolve:   result.Resolve,
			Rationale: result.Rationale,
		}, true
	case threadanalysis.DecisionSummarize:
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseSummaryReply,
			ThreadID:  threadID,
			Body:      result.Summary,
			Resolve:   true,
			Rationale: result.Rationale,
		}, true
	default:
		return review.ThreadResponseAction{}, false
	}
}

func combineReplyAndSummary(reply, summary string) string {
	reply = threadcontext.SanitizeBody(reply)
	summary = threadcontext.SanitizeBody(summary)
	if reply == "" {
		return summary
	}
	if summary == "" {
		return reply
	}
	return reply + "\n\nSummary:\n" + summary
}

func allocateOrResumeRun(ctx context.Context, opts Options, req Request, pr gitprovider.PR, prKey string, mode ledger.PostMode) (ledger.Run, pipeline.ArtifactPaths, error) {
	if !req.Rerun {
		run, ok, err := findIncompleteRun(ctx, opts.Store, req, prKey, pr, mode)
		if err != nil {
			return ledger.Run{}, pipeline.ArtifactPaths{}, err
		}
		if ok {
			return run, pipeline.ArtifactPathsFromDir(run.ArtifactPath), nil
		}
	}

	var lastErr error
	for attempt := 0; attempt < freshRunIDAttempts; attempt++ {
		runID := opts.newRunID()
		artifacts, err := pipeline.ArtifactPathsForRun(opts.Layout, req.PRRef, pr, req.ProfileName, postingKey(req.PostingIdentity), runID)
		if err != nil {
			return ledger.Run{}, artifacts, err
		}
		run, err := allocateRun(ctx, opts, req, pr, prKey, runID, artifacts.Dir, mode)
		if err == nil {
			if err := writeResponseRunMarker(artifacts.Dir, run.RunID); err != nil {
				return ledger.Run{}, artifacts, err
			}
			return run, artifacts, nil
		}
		if !errors.Is(err, ledger.ErrRunExists) {
			return ledger.Run{}, artifacts, err
		}
		lastErr = err
	}
	return ledger.Run{}, pipeline.ArtifactPaths{}, fmt.Errorf("threadrespond: allocate fresh run ID after %d attempts: %w", freshRunIDAttempts, lastErr)
}

func findIncompleteRun(ctx context.Context, store Store, req Request, prKey string, pr gitprovider.PR, mode ledger.PostMode) (ledger.Run, bool, error) {
	runs, err := store.ListRunsForHeadScope(ctx, ledger.ListRunsForHeadScopeParams{
		PRKey:           prKey,
		SHA:             pr.Head.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: postingKey(req.PostingIdentity),
	})
	if err != nil {
		return ledger.Run{}, false, err
	}
	var best ledger.Run
	found := false
	for _, run := range runs {
		if run.BaseSHA != pr.Base.SHA || run.PostMode != mode {
			continue
		}
		if run.Outcome != nil && *run.Outcome != ledger.OutcomeIncomplete {
			continue
		}
		if strings.TrimSpace(run.ArtifactPath) == "" {
			continue
		}
		if !hasResponseRunMarker(run.ArtifactPath) {
			continue
		}
		if !found || run.Attempt > best.Attempt || (run.Attempt == best.Attempt && run.StartedAt.After(best.StartedAt)) {
			best = run
			found = true
		}
	}
	return best, found, nil
}

type responseRunMarker struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	RunID         string `json:"run_id"`
}

func writeResponseRunMarker(artifactPath, runID string) error {
	data, err := json.MarshalIndent(responseRunMarker{
		SchemaVersion: responseRunMarkerSchema,
		Kind:          "thread_response",
		RunID:         runID,
	}, "", "  ")
	if err != nil {
		return err
	}
	return llmlifecycle.WriteFileAtomic(responseRunMarkerPath(artifactPath), append(data, '\n'))
}

func hasResponseRunMarker(artifactPath string) bool {
	info, err := os.Stat(responseRunMarkerPath(artifactPath))
	return err == nil && !info.IsDir()
}

func responseRunMarkerPath(artifactPath string) string {
	return filepath.Join(artifactPath, "thread-response-run.json")
}

func allocateRun(ctx context.Context, opts Options, req Request, pr gitprovider.PR, prKey, runID, artifactPath string, mode ledger.PostMode) (ledger.Run, error) {
	return opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             pr.Head.SHA,
		BaseSHA:         pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: postingKey(req.PostingIdentity),
		PostMode:        mode,
		StartedAt:       opts.now(),
		ArtifactPath:    artifactPath,
	})
}

func completeFailedOnError(ctx context.Context, opts Options, result *Result, errp *error) {
	if errp == nil || *errp == nil || result == nil || strings.TrimSpace(result.Run.RunID) == "" {
		return
	}
	if errors.Is(*errp, context.Canceled) || errors.Is(*errp, context.DeadlineExceeded) {
		return
	}
	outcome := ledger.OutcomeFailed
	var taskErr *llmlifecycle.TaskError
	if errors.As(*errp, &taskErr) && taskErr.Status() == llmlifecycle.StatusFailedBlocking {
		outcome = ledger.OutcomeIncomplete
	}
	_ = opts.Store.CompleteRun(ctx, result.Run.RunID, outcome, opts.now())
	if run, err := opts.Store.GetRun(ctx, result.Run.RunID); err == nil {
		result.Run = run
	}
	result.ExitCode = exitFailed
}

func resetFailedTerminalResponseActions(ctx context.Context, store Store, actions []ledger.PlannedAction) error {
	for _, action := range actions {
		if !action.Required || action.Status != ledger.PlannedActionFailedTerminal {
			continue
		}
		if !isResponseAction(action.Kind) {
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

func validateResponseActions(actions []ledger.PlannedAction) error {
	for _, action := range actions {
		if action.Status == ledger.PlannedActionSuperseded || action.Status == ledger.PlannedActionPlannedOnly {
			continue
		}
		if !isResponseAction(action.Kind) {
			return fmt.Errorf("threadrespond: retry run contains non-response action %q", action.Kind)
		}
	}
	return nil
}

func isResponseAction(kind ledger.PlannedActionKind) bool {
	return kind == ledger.PlannedActionThreadReply || kind == ledger.PlannedActionResolveThread
}

func desiredOutcomeForRetry(actions []ledger.PlannedAction) ledger.Outcome {
	if len(actions) == 0 {
		return ledger.OutcomeNothingToReview
	}
	return ledger.OutcomeComment
}

func validateRetryRun(req Request, pr gitprovider.PR, prKey string, run ledger.Run) error {
	if run.PRKey != prKey {
		return fmt.Errorf("threadrespond: retry run PR mismatch")
	}
	if run.Profile != req.ProfileName {
		return fmt.Errorf("threadrespond: retry run profile mismatch")
	}
	if run.PostingIdentity != postingKey(req.PostingIdentity) {
		return fmt.Errorf("threadrespond: retry run posting identity mismatch")
	}
	if run.PostMode != ledger.PostModeLive {
		return fmt.Errorf("threadrespond: retry run must be live")
	}
	if run.SHA != pr.Head.SHA || run.BaseSHA != pr.Base.SHA {
		return fmt.Errorf("threadrespond: retry run premises moved: head %s -> %s, base %s -> %s", run.SHA, pr.Head.SHA, run.BaseSHA, pr.Base.SHA)
	}
	return nil
}

type llmRuntime struct {
	model  string
	effort string
}

func resolveRuntime(opts Options, req Request) (llmRuntime, error) {
	tier := opts.ModelTier
	if tier == "" {
		tier = config.ModelTierMedium
	}
	resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
		Profile:        req.Profile,
		Stage:          stagemodel.StageThreadAnalysis,
		Tier:           tier,
		ModelOverride:  opts.ModelOverride,
		EffortOverride: opts.EffortOverride,
		DefaultEffort:  string(modelprefs.EffortMedium),
	})
	if err != nil {
		return llmRuntime{}, err
	}
	return llmRuntime{model: resolved.Model, effort: resolved.Effort}, nil
}

func effectiveCaps(caps gitprovider.ProviderCaps, req Request) reviewplan.ProviderCaps {
	return reviewplan.ProviderCaps{
		NativeFileLevelComments: caps.NativeFileLevelComments,
		ThreadResolution:        caps.ThreadResolution && !req.NoResolveThreads,
	}
}

func lockPath(layout statepaths.Layout, req Request, pr gitprovider.PR) (string, error) {
	return layout.LockFile(statepaths.LockSpec{
		Host:            req.PRRef.Host,
		Owner:           req.PRRef.Owner,
		Repo:            req.PRRef.Repo,
		PRNumber:        req.PRRef.Number,
		HeadSHA:         pr.Head.SHA,
		BaseSHA:         pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: postingKey(req.PostingIdentity),
	})
}

func threadLogPath(artifacts pipeline.ArtifactPaths, threadID gitprovider.ThreadID) string {
	if strings.TrimSpace(artifacts.AgentLogsDir) == "" {
		return ""
	}
	return filepath.Join(artifacts.AgentLogsDir, "thread-analysis", statepaths.Encode(string(threadID))+".jsonl")
}

func postMode(req Request) ledger.PostMode {
	if req.DryRun {
		return ledger.PostModeDryRun
	}
	return ledger.PostModeLive
}

func planPostMode(req Request) reviewplan.PostMode {
	if req.DryRun {
		return reviewplan.PostModeDryRun
	}
	return reviewplan.PostModeLive
}

func validateOptions(opts Options) error {
	if opts.Store == nil {
		return fmt.Errorf("threadrespond: store is required")
	}
	if opts.Provider == nil {
		return fmt.Errorf("threadrespond: provider is required")
	}
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("threadrespond: layout data root is required")
	}
	return nil
}

func validateRequest(req Request) error {
	if err := req.PRRef.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(req.PRURL) == "" {
		return fmt.Errorf("threadrespond: PR URL is required")
	}
	if strings.TrimSpace(req.ProfileName) == "" {
		return fmt.Errorf("threadrespond: profile is required")
	}
	if strings.TrimSpace(postingKey(req.PostingIdentity)) == "" {
		return fmt.Errorf("threadrespond: posting identity is required")
	}
	return nil
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

func (opts Options) newActionID(kind reviewplan.ActionKind) (string, error) {
	if opts.NewActionID != nil {
		return opts.NewActionID(kind)
	}
	return uuid.NewString(), nil
}

func (opts Options) newStepID() string {
	if opts.NewStepID != nil {
		if stepID := strings.TrimSpace(opts.NewStepID()); stepID != "" {
			return stepID
		}
	}
	return uuid.NewString()
}

func (opts Options) acquire(path string) (Lock, error) {
	if opts.Acquire != nil {
		return opts.Acquire(path)
	}
	return runlock.Acquire(path)
}

func postingKey(identity gitprovider.Identity) string {
	if strings.TrimSpace(identity.Login) != "" {
		return identity.Login
	}
	return identity.ID
}
