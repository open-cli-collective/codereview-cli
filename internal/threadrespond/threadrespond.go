// Package threadrespond plans and posts responses to existing inline review
// threads through the shared LLM, reviewplan, ledger, and outbox boundaries.
package threadrespond

import (
	"context"
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
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
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
	Store        Store
	Provider     outbox.LiveProvider
	Adapter      llm.Adapter
	Limiter      outbox.Limiter
	Layout       statepaths.Layout
	Acquire      AcquireFunc
	TaskProgress llmlifecycle.Progress
	Now          func() time.Time

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
	Artifacts       runartifact.Paths
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
	eligible := threadcontext.PendingCRAuthoredFindingThreads(threads)

	mode := postMode(req)
	run, artifacts, err := allocateOrResumeRun(ctx, opts, req, pr, prKey, mode)
	if err != nil {
		return Result{PR: pr, PRKey: prKey, Artifacts: artifacts, Threads: threads, EligibleThreads: eligible, ExitCode: exitFailed}, err
	}
	if err := ensureArtifactDirs(artifacts); err != nil {
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
		if err := validateCachedResponseActionCapabilities(existingActions, effectiveCaps(opts.Provider.Capabilities(), req)); err != nil {
			return result, err
		}
		if err := validateCachedResponseActionThreads(opts, req, run, artifacts, eligible, existingActions); err != nil {
			return result, err
		}
		result.PlannedActions = existingActions
	} else {
		if len(eligible) > 0 {
			analysisOpts, err := buildThreadAnalysisOptions(opts, req, run, artifacts)
			if err != nil {
				return result, err
			}
			analyses, err := threadanalysis.AnalyzeThreads(ctx, analysisOpts, eligible, func(thread threadcontext.Thread) (string, error) {
				return threadLogPath(artifacts, thread.ID), nil
			})
			if err != nil {
				return result, err
			}
			result.Analyses = analyses
			result.Responses = threadanalysis.ResponseActions(analyses)
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
			plannedActions = append(plannedActions, ledger.PlannedAction{Action: action.Action, RunID: run.RunID})
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
		return completeAndRefresh(ctx, opts.Store, result, message, exitOK)
	}
	if opts.Limiter == nil {
		return result, fmt.Errorf("threadrespond: outbox limiter is required")
	}
	return postAndRefresh(ctx, opts, req, result, desiredOutcomeForRetry(result.PlannedActions))
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
	result := Result{Run: run, PR: pr, PRKey: prKey, Artifacts: runartifact.FromDir(run.ArtifactPath), ExitCode: exitOK}
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
	if err := ledger.ResetFailedTerminalActions(ctx, opts.Store, actions, func(action ledger.PlannedAction) bool {
		return action.Required && isResponseAction(action.Kind)
	}); err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	actions, err = opts.Store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		result.ExitCode = exitFailed
		return result, err
	}
	result.PlannedActions = actions
	if moved, message, err := abortIfMoved(ctx, opts, req, run); err != nil {
		result.ExitCode = exitUpstream
		return result, err
	} else if moved {
		return completeAndRefresh(ctx, opts.Store, result, message, exitFailed)
	}
	return postAndRefresh(ctx, opts, req, result, desiredOutcomeForRetry(actions))
}

func completeAndRefresh(ctx context.Context, store Store, result Result, message string, refreshErrorExitCode int) (Result, error) {
	run, err := store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		result.ExitCode = refreshErrorExitCode
		return result, err
	}
	result.Run = run
	result.Outbox = outbox.Result{Outcome: ledger.OutcomeAborted, ExitCode: exitUpstream, Aborted: true}
	result.ExitCode = exitUpstream
	result.Message = message
	return result, nil
}

func postAndRefresh(ctx context.Context, opts Options, req Request, result Result, desiredOutcome ledger.Outcome) (Result, error) {
	run := result.Run
	postResult, err := outbox.Post(ctx, outbox.Options{
		Store:    opts.Store,
		Provider: opts.Provider,
		Limiter:  opts.Limiter,
		Now:      opts.now,
	}, outbox.Request{
		Run:             run,
		PRRef:           req.PRRef,
		PostingIdentity: req.PostingIdentity,
		DesiredOutcome:  desiredOutcome,
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
	actions, err := opts.Store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		return result, err
	}
	result.Run = run
	result.PlannedActions = actions
	return result, nil
}

func ensureArtifactDirs(artifacts runartifact.Paths) error {
	for _, dir := range []string{
		artifacts.AgentLogsDir,
		filepath.Join(artifacts.AgentLogsDir, "thread-analysis"),
		artifacts.LLMTasksDir,
	} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
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
	premises := runlifecycle.ComparePremises(run, pr.Head.SHA, pr.Base.SHA)
	if !premises.Moved {
		return false, "", nil
	}
	if err := opts.Store.CompleteRun(ctx, run.RunID, ledger.OutcomeAborted, opts.now()); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("threadrespond premises moved: head %s -> %s, base %s -> %s", premises.StoredHeadSHA, premises.CurrentHeadSHA, premises.StoredBaseSHA, premises.CurrentBaseSHA), nil
}

func buildThreadAnalysisOptions(opts Options, req Request, run ledger.Run, artifacts runartifact.Paths) (threadanalysis.Options, error) {
	if opts.Adapter == nil {
		return threadanalysis.Options{}, fmt.Errorf("threadrespond: adapter is required")
	}
	runtime, err := resolveRuntime(opts, req)
	if err != nil {
		return threadanalysis.Options{}, err
	}
	return threadanalysis.Options{
		Store:          opts.Store,
		RunID:          run.RunID,
		Adapter:        opts.Adapter,
		Model:          runtime.model,
		Effort:         runtime.effort,
		LifecyclePaths: llmlifecycle.Paths{LLMTasksDir: artifacts.LLMTasksDir},
		Progress:       opts.TaskProgress,
		Now:            opts.now,
		NewStepID:      opts.newStepID,
	}, nil
}

func validateCachedResponseActionThreads(opts Options, req Request, run ledger.Run, artifacts runartifact.Paths, eligible []threadcontext.Thread, actions []ledger.PlannedAction) error {
	threadIDs := responseActionThreadIDs(actions)
	if len(threadIDs) == 0 {
		return nil
	}
	analysisOpts, err := buildThreadAnalysisOptions(opts, req, run, artifacts)
	if err != nil {
		return err
	}
	threadsByID := make(map[string]threadcontext.Thread, len(eligible))
	for _, thread := range eligible {
		threadsByID[string(thread.ID)] = thread
	}
	for _, threadID := range threadIDs {
		thread, ok := threadsByID[threadID]
		if !ok {
			return fmt.Errorf("threadrespond: thread %s is no longer eligible for cached response actions; pass --rerun to start fresh", threadID)
		}
		analysisOpts.LogPath = threadLogPath(artifacts, thread.ID)
		if err := threadanalysis.ValidateCachedThread(analysisOpts, thread); err != nil {
			return err
		}
	}
	return nil
}

func responseActionThreadIDs(actions []ledger.PlannedAction) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Status == ledger.PlannedActionSuperseded || action.Status == ledger.PlannedActionPlannedOnly {
			continue
		}
		if !isResponseAction(action.Kind) {
			continue
		}
		threadID := strings.TrimSpace(action.ThreadID)
		if threadID == "" {
			continue
		}
		if _, ok := seen[threadID]; ok {
			continue
		}
		seen[threadID] = struct{}{}
		ids = append(ids, threadID)
	}
	return ids
}

func allocateOrResumeRun(ctx context.Context, opts Options, req Request, pr gitprovider.PR, prKey string, mode ledger.PostMode) (ledger.Run, runartifact.Paths, error) {
	if !req.Rerun {
		run, ok, err := findIncompleteRun(ctx, opts.Store, req, prKey, pr, mode)
		if err != nil {
			return ledger.Run{}, runartifact.Paths{}, err
		}
		if ok {
			return run, runartifact.FromDir(run.ArtifactPath), nil
		}
	}

	var lastErr error
	for attempt := 0; attempt < freshRunIDAttempts; attempt++ {
		runID := opts.newRunID()
		artifacts, err := runartifact.ForRun(opts.Layout, req.PRRef, pr, req.ProfileName, runlifecycle.PostingKey(req.PostingIdentity), runID)
		if err != nil {
			return ledger.Run{}, artifacts, err
		}
		run, err := allocateRun(ctx, opts, req, pr, prKey, runID, artifacts.Dir, mode)
		if err == nil {
			if err := runartifact.WriteMarker(artifacts.Dir, runartifact.KindThreadResponse, run.RunID); err != nil {
				return ledger.Run{}, artifacts, err
			}
			return run, artifacts, nil
		}
		if !errors.Is(err, ledger.ErrRunExists) {
			return ledger.Run{}, artifacts, err
		}
		lastErr = err
	}
	return ledger.Run{}, runartifact.Paths{}, fmt.Errorf("threadrespond: allocate fresh run ID after %d attempts: %w", freshRunIDAttempts, lastErr)
}

func findIncompleteRun(ctx context.Context, store Store, req Request, prKey string, pr gitprovider.PR, mode ledger.PostMode) (ledger.Run, bool, error) {
	runs, err := store.ListRunsForHeadScope(ctx, ledger.ListRunsForHeadScopeParams{
		PRKey:           prKey,
		SHA:             pr.Head.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity),
	})
	if err != nil {
		return ledger.Run{}, false, err
	}
	// Legacy read only: older runs could store DisplayName as their key.
	if legacy := strings.TrimSpace(req.PostingIdentity.DisplayName); legacy != "" && legacy != runlifecycle.PostingKey(req.PostingIdentity) {
		legacyRuns, err := store.ListRunsForHeadScope(ctx, ledger.ListRunsForHeadScopeParams{
			PRKey:           prKey,
			SHA:             pr.Head.SHA,
			Profile:         req.ProfileName,
			PostingIdentity: legacy,
		})
		if err != nil {
			return ledger.Run{}, false, err
		}
		runs = append(runs, legacyRuns...)
	}
	best, found := runlifecycle.NewestCompatibleIncompleteRun(runs, pr.Base.SHA, mode, runartifact.KindThreadResponse, runartifact.MarkerMatches)
	return best, found, nil
}

func allocateRun(ctx context.Context, opts Options, req Request, pr gitprovider.PR, prKey, runID, artifactPath string, mode ledger.PostMode) (ledger.Run, error) {
	return opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             pr.Head.SHA,
		BaseSHA:         pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity),
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

func validateCachedResponseActionCapabilities(actions []ledger.PlannedAction, caps reviewplan.ProviderCaps) error {
	for _, action := range actions {
		if action.Status == ledger.PlannedActionSuperseded {
			continue
		}
		if action.Kind == ledger.PlannedActionResolveThread && !caps.ThreadResolution {
			return fmt.Errorf("threadrespond: cached response action %s resolves a thread, but current options do not allow thread resolution; pass --rerun to start fresh", action.ActionID)
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
	if run.PostingIdentity != runlifecycle.PostingKey(req.PostingIdentity) {
		return fmt.Errorf("threadrespond: retry run posting identity mismatch")
	}
	if run.PostMode != ledger.PostModeLive {
		return fmt.Errorf("threadrespond: retry run must be live")
	}
	premises := runlifecycle.ComparePremises(run, pr.Head.SHA, pr.Base.SHA)
	if premises.Moved {
		return fmt.Errorf("threadrespond: retry run premises moved: head %s -> %s, base %s -> %s", premises.StoredHeadSHA, premises.CurrentHeadSHA, premises.StoredBaseSHA, premises.CurrentBaseSHA)
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
		PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity),
	})
}

func threadLogPath(artifacts runartifact.Paths, threadID gitprovider.ThreadID) string {
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
	if strings.TrimSpace(runlifecycle.PostingKey(req.PostingIdentity)) == "" {
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
