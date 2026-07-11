// Package pipeline orchestrates review pipeline phases without owning command IO.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/dossier"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/pricing"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/sessionreuse"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadanalysis"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
	"github.com/open-cli-collective/codereview-cli/internal/workbench"
)

const (
	defaultMaxAgents      = 5
	defaultMaxConcurrency = 5
	defaultMaxPromptBytes = 512 * 1024
)

// ErrStructuredOutputInvalidAfterRetry marks a selector or rollup response that
// stayed invalid after the LLM retry path.
var ErrStructuredOutputInvalidAfterRetry = llm.ErrStructuredOutputInvalidAfterRetry

// ReadProvider is the PR read boundary needed by dry-run review.
type ReadProvider interface {
	GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error)
	GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error)
	GetFileAtRef(context.Context, gitprovider.PRRef, string, string) ([]byte, error)
	ListTreeAtRef(context.Context, gitprovider.PRRef, string, string) ([]gitprovider.TreeEntry, error)
	ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error)
	ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error)
	ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error)
	Capabilities() gitprovider.ProviderCaps
}

type rangeDiffProvider interface {
	GetDiffBetweenRefs(context.Context, gitprovider.PRRef, string, string) (gitprovider.UnifiedDiff, error)
}

// Store is the ledger behavior required by the dry-run pipeline.
type Store interface {
	ListRuns(context.Context) ([]ledger.Run, error)
	ListRunsForHeadScope(context.Context, ledger.ListRunsForHeadScopeParams) ([]ledger.Run, error)
	DeleteRun(context.Context, string) error
	AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error)
	InsertSession(context.Context, ledger.Session) error
	GetSession(context.Context, string) (ledger.Session, error)
	InsertPlanningResult(context.Context, []ledger.Finding, []ledger.PlannedAction) error
	ListFindings(context.Context, string) ([]ledger.Finding, error)
	ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error)
	CompleteRun(context.Context, string, ledger.Outcome, time.Time) error
}

// NamedSessionStore persists cross-run LLM sessions.
type NamedSessionStore interface {
	GetNamedSession(context.Context, string) (ledger.NamedSession, error)
	UpsertNamedSession(context.Context, ledger.NamedSession) error
}

// LLMTaskProgress records task-aware LLM pipeline breadcrumbs without owning
// command IO details.
type LLMTaskProgress = llmlifecycle.Progress

// LLMTaskProgressSpan is one active LLM task breadcrumb.
type LLMTaskProgressSpan = llmlifecycle.ProgressSpan

// LLMTaskProgressEvent describes one LLM task execution or reload.
type LLMTaskProgressEvent = llmlifecycle.ProgressEvent

// LLMTaskProgressResult describes the outcome of one task execution or reload.
type LLMTaskProgressResult = llmlifecycle.ProgressResult

// ContextBudget limits prompt size. A negative MaxPromptBytes disables checks.
type ContextBudget struct {
	MaxPromptBytes int
}

// Options contains dry-run pipeline dependencies.
type Options struct {
	Provider      ReadProvider
	Adapter       llm.Adapter
	Store         Store
	NamedSessions NamedSessionStore
	Layout        statepaths.Layout
	Warnings      io.Writer
	TaskProgress  LLMTaskProgress

	Now             func() time.Time
	NewRunID        func() string
	NewSessionRowID func() string
	NewFindingID    func() (review.FindingID, error)
	NewActionID     func(reviewplan.ActionKind) (string, error)

	Budget         ContextBudget
	MaxAgents      int
	MaxConcurrency int

	Retention           datalifecycle.RetentionPolicy
	RetentionManualOnly bool

	GitCommand      func(context.Context, string, ...string) ([]byte, error)
	ResolveRepoRoot func(context.Context) (string, error)
}

// Request identifies one dry-run review.
type Request struct {
	PRRef           gitprovider.PRRef
	PRURL           string
	ProfileName     string
	SessionName     string
	Profile         config.Profile
	PostingIdentity gitprovider.Identity
	AgentDirs       []string

	SelectionModelOverride      string
	SelectionEffortOverride     string
	SelectionPromptInstructions string
	ReviewerModelOverride       string
	ReviewerModelTierOverride   string
	ReviewerEffortOverride      string
	ReviewerFast                bool
	ReviewBaseSHA               string
	ReviewHeadSHA               string
	Rerun                       bool
	FreshSession                bool

	FailOn              *review.Severity
	AllowSelfReview     bool
	AllowSelfApprove    bool
	NoResolveThreads    bool
	MajorRequestChanges bool

	// ToolVersion is the raw cr version string rendered in the rollup
	// footer (e.g. "0.3.63").
	ToolVersion string
}

// SelectionRequest runs only the selection phase without review persistence or posting.
type SelectionRequest struct {
	PRRef           gitprovider.PRRef
	ProfileName     string
	Profile         config.Profile
	PostingIdentity gitprovider.Identity
	AgentDirs       []string
	ArtifactDir     string
	ReviewBaseSHA   string
	ReviewHeadSHA   string

	SelectionModelOverride      string
	SelectionEffortOverride     string
	SelectionPromptInstructions string
}

// ArtifactPaths contains per-run artifact paths.
type ArtifactPaths = runartifact.Paths

type llmTaskStatus = llmlifecycle.Status

const (
	llmTaskStatusSucceeded      llmTaskStatus = llmlifecycle.StatusSucceeded
	llmTaskStatusFailedIsolated llmTaskStatus = llmlifecycle.StatusFailedIsolated
	llmTaskStatusFailedBlocking llmTaskStatus = llmlifecycle.StatusFailedBlocking
)

var errLLMTaskFailedBlocking = errors.New("pipeline: blocking LLM task failed")

type llmTaskError struct {
	status llmTaskStatus
	err    error
}

func (e *llmTaskError) Error() string {
	if e == nil || e.err == nil {
		return errLLMTaskFailedBlocking.Error()
	}
	return e.err.Error()
}

func (e *llmTaskError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *llmTaskError) Is(target error) bool {
	return target == errLLMTaskFailedBlocking && e != nil && e.status == llmTaskStatusFailedBlocking
}

// Result is the completed dry-run pipeline output.
type Result struct {
	Run                   ledger.Run
	PR                    gitprovider.PR
	PRKey                 string
	Artifacts             ArtifactPaths
	Quota                 llm.Quota
	QuotaSupported        bool
	QuotaLow              bool
	Catalog               agents.Catalog
	Selection             llm.Selection
	Findings              []review.Finding
	Rollup                review.Rollup
	Plan                  reviewplan.Plan
	Sessions              []ledger.Session
	PlannedActions        []ledger.PlannedAction
	NamedSessionCandidate *ledger.NamedSession
	FailOnTriggered       bool
	EffectiveCaps         reviewplan.ProviderCaps
	AgentDefsChanged      bool
	CurrentBaseSHA        string
	CurrentHeadSHA        string
	ReviewBaseSHA         string
	ReviewHeadSHA         string
	ReviewerFailures      []ReviewerFailure
	ReviewerCoverage      []reviewplan.ReviewerCoverageSummary
	reviewerFastDelivered string
}

// ReviewerFailure records an isolated reviewer LLM task failure that should not
// abort the whole run.
type ReviewerFailure struct {
	TaskID  string `json:"task_id"`
	AgentID string `json:"agent_id"`
	Error   string `json:"error"`
}

const (
	reviewerCoverageCompleteBroad        = "complete_broad"
	reviewerCoverageCompleteConstrained  = "complete_constrained"
	reviewerCoverageIncompleteSkipped    = "incomplete_skipped"
	reviewerCoverageIncompleteFailed     = "incomplete_failed"
	reviewerCoverageIncompleteUnassigned = "incomplete_unassigned"
)

// SelectionSession describes the single LLM turn used for selection-only execution.
type SelectionSession struct {
	ProviderReportedSessionID string
	ProviderSessionID         string
	Adapter                   string
	Model                     string
	Effort                    string
	StartedAt                 time.Time
	CompletedAt               time.Time
	Response                  llm.Response
}

// SelectionResult is the selection-only pipeline output.
type SelectionResult struct {
	PR               gitprovider.PR
	PRKey            string
	Artifacts        ArtifactPaths
	Quota            llm.Quota
	QuotaSupported   bool
	QuotaLow         bool
	Catalog          agents.Catalog
	ParsedDiff       ParsedDiff
	ChangedFiles     []string
	Threads          []gitprovider.InlineThread
	Selection        llm.Selection
	SelectionSession SelectionSession
	EffectiveCaps    reviewplan.ProviderCaps
	AgentDefsChanged bool
	CurrentBaseSHA   string
	CurrentHeadSHA   string
	ReviewBaseSHA    string
	ReviewHeadSHA    string
}

type sessionDraft = llmlifecycle.SessionDraft

type executionMode struct {
	live         bool
	run          ledger.Run
	planPostMode reviewplan.PostMode
	completeAs   ledger.Outcome
}

type namedSessionState struct {
	enabled                  bool
	active                   sessionreuse.Scope
	stored                   *ledger.NamedSession
	supportsResume           bool
	currentProviderSessionID string
	createdAt                time.Time
}

type selectionSetupRequest struct {
	PRRef            gitprovider.PRRef
	Profile          config.Profile
	PostingIdentity  gitprovider.Identity
	AgentDirs        []string
	ReviewRequest    Request
	ReviewBaseSHA    string
	ReviewHeadSHA    string
	NoResolveThreads bool
	ResolvedPR       *reviewPRContext
	InvocationRoot   *string
	ResolveArtifacts func(gitprovider.PR) (ArtifactPaths, error)
}

type preparedSelectionContext struct {
	pr               gitprovider.PR
	reviewPR         gitprovider.PR
	prKey            string
	artifacts        ArtifactPaths
	rawDiff          string
	quota            llm.Quota
	quotaSupported   bool
	quotaLow         bool
	parsed           ParsedDiff
	changedFiles     []string
	threads          []gitprovider.InlineThread
	threadContext    []threadcontext.Thread
	reviews          []gitprovider.Review
	issueComments    []gitprovider.IssueComment
	catalog          agents.Catalog
	effectiveCaps    reviewplan.ProviderCaps
	agentDefsChanged bool
	currentBaseSHA   string
	currentHeadSHA   string
	reviewBaseSHA    string
	reviewHeadSHA    string
}

type selectionPhaseRequest struct {
	RunID                       string
	DurableSession              bool
	Profile                     config.Profile
	SelectionModelOverride      string
	SelectionEffortOverride     string
	SelectionPromptInstructions string
	ReviewPR                    gitprovider.PR
	Catalog                     agents.Catalog
	ParsedDiff                  ParsedDiff
	Threads                     []gitprovider.InlineThread
	ThreadContext               []threadcontext.Thread
	Artifacts                   ArtifactPaths
	ResumeSessionID             string
	MaxAgents                   int
}

type reviewPRContext struct {
	pr             gitprovider.PR
	reviewPR       gitprovider.PR
	pinnedReview   bool
	currentBaseSHA string
	currentHeadSHA string
}

// DryRun executes the dry-run review pipeline.
func DryRun(ctx context.Context, opts Options, req Request) (Result, error) {
	if err := validate(opts, req); err != nil {
		return Result{}, err
	}
	if err := pruneRetention(ctx, opts.Layout, opts.Store, opts.Now, opts.Warnings, opts.Retention, opts.RetentionManualOnly); err != nil {
		return Result{}, err
	}
	return execute(ctx, opts, req, executionMode{
		planPostMode: reviewplan.PostModeDryRun,
		completeAs:   ledger.OutcomeDryRun,
	})
}

// Live executes the review planning phases into a gate-allocated live run.
func Live(ctx context.Context, opts Options, req Request, run ledger.Run) (Result, error) {
	if hasDryRunStageOverrides(req) {
		return Result{}, fmt.Errorf("pipeline: selection and reviewer overrides require dry-run review")
	}
	if strings.TrimSpace(req.ReviewBaseSHA) != "" || strings.TrimSpace(req.ReviewHeadSHA) != "" {
		return Result{}, fmt.Errorf("pipeline: pinned review SHAs require dry-run review")
	}
	if strings.TrimSpace(run.RunID) == "" {
		return Result{}, fmt.Errorf("pipeline: live run ID is required")
	}
	if strings.TrimSpace(run.ArtifactPath) == "" {
		return Result{}, fmt.Errorf("pipeline: live artifact path is required")
	}
	if run.PostMode != ledger.PostModeLive {
		return Result{}, fmt.Errorf("pipeline: live run post mode is required")
	}
	return execute(ctx, opts, req, executionMode{
		live:         true,
		run:          run,
		planPostMode: reviewplan.PostModeLive,
	})
}

// SelectionOnly executes only the selection phase using caller-owned artifacts.
func SelectionOnly(ctx context.Context, opts Options, req SelectionRequest) (SelectionResult, error) {
	if err := validateSelectionOnly(opts, req); err != nil {
		return SelectionResult{}, err
	}
	invocationRoot, err := resolveInvocationRootForSafety(ctx, opts)
	if err != nil {
		return SelectionResult{}, err
	}
	if err := agents.RequireSafeProfileSources(req.Profile.AgentSources, invocationRoot); err != nil {
		return SelectionResult{}, err
	}

	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:            req.PRRef,
		Profile:          req.Profile,
		PostingIdentity:  req.PostingIdentity,
		AgentDirs:        req.AgentDirs,
		ReviewBaseSHA:    req.ReviewBaseSHA,
		ReviewHeadSHA:    req.ReviewHeadSHA,
		NoResolveThreads: false,
		InvocationRoot:   &invocationRoot,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(req.ArtifactDir), nil
		},
	})
	if err != nil {
		return SelectionResult{}, err
	}

	result := prepared.selectionResult()
	if err := workbench.Prepare(ctx, workbenchDeps(opts), workbench.Request{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		return SelectionResult{}, err
	}
	if err := dossier.Prepare(ctx, dossierEnv(opts), dossier.PreparationRequest{
		Profile:                 req.Profile,
		SelectionModelOverride:  req.SelectionModelOverride,
		SelectionEffortOverride: req.SelectionEffortOverride,
		Artifacts:               prepared.artifacts,
	}); err != nil {
		return SelectionResult{}, pipelineTaskError(err)
	}
	if len(prepared.parsed.Patches) == 0 {
		return result, nil
	}

	selection, session, _, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		Profile:                     req.Profile,
		SelectionModelOverride:      req.SelectionModelOverride,
		SelectionEffortOverride:     req.SelectionEffortOverride,
		SelectionPromptInstructions: req.SelectionPromptInstructions,
		ReviewPR:                    prepared.reviewPR,
		Catalog:                     prepared.catalog,
		ParsedDiff:                  prepared.parsed,
		Threads:                     prepared.threads,
		ThreadContext:               prepared.threadContext,
		Artifacts:                   prepared.artifacts,
		MaxAgents:                   opts.maxAgents(),
	})
	result.SelectionSession = selectionSessionFromDraft(session)
	if err != nil {
		return result, err
	}
	result.Selection = selection
	return result, nil
}

func execute(ctx context.Context, opts Options, req Request, mode executionMode) (out Result, err error) {
	if err := validate(opts, req); err != nil {
		return Result{}, err
	}
	invocationRoot, err := resolveInvocationRootForSafety(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	if err := agents.RequireSafeProfileSources(req.Profile.AgentSources, invocationRoot); err != nil {
		return Result{}, err
	}
	completed := false
	failureOutcome := ledger.OutcomeFailed
	if mode.live {
		defer func() {
			if !completed && !isContextError(err) {
				_ = opts.Store.CompleteRun(context.Background(), mode.run.RunID, failureOutcome, opts.now())
			}
		}()
	}
	now := opts.now()
	maxAgents := opts.maxAgents()
	maxConcurrency := opts.maxConcurrency(maxAgents)
	runID := ""
	if mode.live {
		runID = mode.run.RunID
	}
	reviewCtx, err := resolveReviewPRContext(ctx, opts.Provider, req.PRRef, req.ReviewBaseSHA, req.ReviewHeadSHA)
	if err != nil {
		return Result{}, err
	}
	var resumedDryRun *ledger.Run
	if !mode.live && !req.Rerun {
		run, ok, err := findIncompleteDryRun(ctx, opts.Store, req, reviewCtx.reviewPR)
		if err != nil {
			return Result{}, err
		}
		if ok {
			resumedDryRun = &run
			runID = run.RunID
		}
	}
	if !mode.live && strings.TrimSpace(runID) == "" {
		runID = opts.newRunID()
	}
	if sameIdentity(reviewCtx.pr.Author, req.PostingIdentity) && req.Profile.ReviewerCredentials != nil && !req.AllowSelfReview {
		return Result{}, fmt.Errorf("pipeline: reviewer credentials resolve to PR author %q; pass --allow-self-review to continue", req.PostingIdentity.Login)
	}
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:            req.PRRef,
		Profile:          req.Profile,
		PostingIdentity:  req.PostingIdentity,
		AgentDirs:        req.AgentDirs,
		ReviewRequest:    req,
		ReviewBaseSHA:    req.ReviewBaseSHA,
		ReviewHeadSHA:    req.ReviewHeadSHA,
		NoResolveThreads: req.NoResolveThreads,
		ResolvedPR:       &reviewCtx,
		InvocationRoot:   &invocationRoot,
		ResolveArtifacts: func(reviewPR gitprovider.PR) (ArtifactPaths, error) {
			if mode.live {
				return ArtifactPathsFromDir(mode.run.ArtifactPath), nil
			}
			if resumedDryRun != nil {
				return ArtifactPathsFromDir(resumedDryRun.ArtifactPath), nil
			}
			return ArtifactPathsForRun(opts.Layout, req.PRRef, reviewPR, req.ProfileName, postingKey(req.PostingIdentity), runID)
		},
	})
	if err != nil {
		return Result{}, err
	}

	result := prepared.reviewResult()
	run := mode.run
	if !mode.live {
		if resumedDryRun != nil {
			run = *resumedDryRun
		} else {
			run, err = opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
				PRKey:           prepared.prKey,
				PRURL:           req.PRURL,
				RunID:           runID,
				SHA:             prepared.reviewPR.Head.SHA,
				BaseSHA:         prepared.reviewPR.Base.SHA,
				Profile:         req.ProfileName,
				PostingIdentity: postingKey(req.PostingIdentity),
				PostMode:        ledger.PostModeDryRun,
				StartedAt:       now,
				ArtifactPath:    prepared.artifacts.Dir,
			})
			if err != nil {
				return Result{}, err
			}
			if err := runartifact.WriteMarker(prepared.artifacts.Dir, runartifact.KindReview, run.RunID); err != nil {
				return Result{}, err
			}
		}
		defer func() {
			if !completed {
				_ = opts.Store.CompleteRun(context.Background(), run.RunID, failureOutcome, opts.now())
			}
		}()
	}
	result.Run = run

	if err := workbench.Prepare(ctx, workbenchDeps(opts), workbench.Request{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		return Result{}, err
	}
	if err := dossier.Prepare(ctx, dossierEnv(opts), dossier.PreparationRequest{
		RunID:                   run.RunID,
		Profile:                 req.Profile,
		SelectionModelOverride:  req.SelectionModelOverride,
		SelectionEffortOverride: req.SelectionEffortOverride,
		Artifacts:               prepared.artifacts,
	}); err != nil {
		return Result{}, pipelineTaskError(err)
	}

	findingSessions, blockingFailure, err := executePlanPhases(ctx, opts, req, mode, run, prepared, now, maxAgents, maxConcurrency, &result)
	if err != nil {
		if blockingFailure {
			failureOutcome = ledger.OutcomeIncomplete
		}
		return Result{}, err
	}
	if err := persistExecutionResult(ctx, opts, req, run, prepared, findingSessions, &result); err != nil {
		return Result{}, err
	}
	if !mode.live {
		if result.NamedSessionCandidate != nil {
			if err := opts.NamedSessions.UpsertNamedSession(ctx, *result.NamedSessionCandidate); err != nil {
				return Result{}, err
			}
		}
		if err := opts.Store.CompleteRun(ctx, run.RunID, mode.completeAs, opts.now()); err != nil {
			return Result{}, err
		}
	}
	completed = true
	result.FailOnTriggered = failOnTriggered(result.Findings, req.FailOn)
	return result, nil
}

func executePlanPhases(ctx context.Context, opts Options, req Request, mode executionMode, run ledger.Run, prepared preparedSelectionContext, now time.Time, maxAgents, maxConcurrency int, result *Result) (map[review.FindingID]string, bool, error) {
	repoSources := append([]agents.SourceInfo(nil), prepared.catalog.Sources...)
	if len(prepared.parsed.Patches) == 0 {
		if sessionName := strings.TrimSpace(req.SessionName); sessionName != "" {
			if !mode.live {
				return nil, false, fmt.Errorf("pipeline: named session %q requires live review", sessionName)
			}
			opts.emitWarning(fmt.Sprintf("session %q was not updated because no orchestrator session was produced", sessionName))
		}
		plan, err := opts.buildPlan(req, prepared.pr, mode.planPostMode, result.EffectiveCaps, reviewplan.Diff{}, nil, review.Rollup{}, nil, true, result.AgentDefsChanged, planRunInputs{repoSources: repoSources})
		if err != nil {
			return nil, false, err
		}
		result.Plan = plan
		return nil, false, nil
	}
	if dossier.RepoGuidanceUnavailableReason(repoSources) != "" {
		plan, err := opts.buildPlan(req, prepared.reviewPR, mode.planPostMode, result.EffectiveCaps, prepared.parsed.PlanDiff, nil, review.Rollup{}, nil, false, result.AgentDefsChanged, planRunInputs{
			repoSources: repoSources,
			startedAt:   now,
		})
		if err != nil {
			return nil, false, err
		}
		result.Plan = plan
		return nil, false, nil
	}
	return executeLLMPhases(ctx, opts, req, mode, run, prepared, repoSources, now, maxAgents, maxConcurrency, result)
}

func executeLLMPhases(ctx context.Context, opts Options, req Request, mode executionMode, run ledger.Run, prepared preparedSelectionContext, repoSources []agents.SourceInfo, now time.Time, maxAgents, maxConcurrency int, result *Result) (map[review.FindingID]string, bool, error) {
	runtimeConfig, err := resolveSelectionRuntimeConfig(req.Profile, req.SelectionModelOverride, req.SelectionEffortOverride)
	if err != nil {
		return nil, false, err
	}
	namedSession, err := prepareNamedSession(ctx, opts, req, mode.live, runtimeConfig.model, now)
	if err != nil {
		return nil, false, err
	}

	selection, selectionSession, selectionLedgerSession, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		RunID:                       run.RunID,
		DurableSession:              namedSession.enabled && namedSession.supportsResume,
		Profile:                     req.Profile,
		SelectionModelOverride:      req.SelectionModelOverride,
		SelectionEffortOverride:     req.SelectionEffortOverride,
		SelectionPromptInstructions: req.SelectionPromptInstructions,
		ReviewPR:                    prepared.reviewPR,
		Catalog:                     prepared.catalog,
		ParsedDiff:                  prepared.parsed,
		Threads:                     prepared.threads,
		ThreadContext:               prepared.threadContext,
		Artifacts:                   prepared.artifacts,
		ResumeSessionID:             namedSession.resumeID(),
		MaxAgents:                   maxAgents,
	})
	if err != nil {
		return executionPhaseFailure(err)
	}
	result.Selection = selection
	result.Sessions = appendSessionIfPresent(result.Sessions, selectionLedgerSession)
	namedSession.recordSessionID(selectionSession)

	selectionTaskIDs := []string{orchestratorSelectionStage}
	threadResponses, err := analyzeReviewThreads(ctx, opts, req, run, prepared.artifacts, prepared.threadContext)
	if err != nil {
		return executionPhaseFailure(err)
	}
	findings, reviewerResults, reviewerSessions, reviewerLedgerSessions, findingSessions, reviewerFailures, err := runReviewers(ctx, opts, req, run.RunID, prepared.reviewPR, prepared.catalog, prepared.parsed, prepared.artifacts, selection, selectionTaskIDs, maxConcurrency)
	if err != nil {
		return executionPhaseFailure(err)
	}
	result.Findings = findings
	result.ReviewerFailures = reviewerFailures
	result.reviewerFastDelivered = reviewerFastDelivery(req.ReviewerFast, reviewerSessions)
	reviewerCoverage := buildReviewerCoverage(selection.SelectedAgents, reviewerResults, reviewerFailures, prepared.changedFiles)
	result.ReviewerCoverage = reviewerCoverage
	result.Sessions = appendSessionsIfPresent(result.Sessions, reviewerLedgerSessions...)

	rollupRuntimeConfig, err := resolveSynthesisRuntimeConfig(req)
	if err != nil {
		return nil, false, err
	}
	rollupModel, rollupEffort := rollupRuntimeConfig.model, rollupRuntimeConfig.effort
	rollupPrompt, err := buildRollupPrompt(prepared.reviewPR, findings, reviewerFailures, reviewerCoverage)
	if err != nil {
		return nil, false, err
	}
	if err := opts.checkPromptBudget("rollup", "", rollupModel, "", rollupPrompt); err != nil {
		return nil, false, err
	}
	rollupLog, err := prepared.artifacts.AgentLog(orchestratorRollupStage)
	if err != nil {
		return nil, false, err
	}
	reviewerDeps := reviewerTaskIDs(selection.SelectedAgents)
	rollupDeps := append([]string(nil), selectionTaskIDs...)
	rollupDeps = append(rollupDeps, reviewerDeps...)
	rollup, rollupSession, rollupLedgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
		runID:             run.RunID,
		taskID:            orchestratorRollupStage,
		phase:             "rollup",
		dependencyTaskIDs: rollupDeps,
		inputFingerprint:  llmlifecycle.Fingerprint(opts.Adapter.Name(), orchestratorRollupStage, "rollup", rollupModel, rollupEffort, rollupPrompt, rollupDeps),
		artifacts:         prepared.artifacts,
		role:              ledger.SessionRoleOrchestrator,
		model:             rollupModel,
		effort:            rollupEffort,
		logPath:           rollupLog,
		prompt:            rollupPrompt,
		resumeSessionID:   namedSession.resumeID(),
	}, func(data []byte) (review.Rollup, error) {
		return llm.DecodeRollup(data, llm.RollupOptions{
			FindingSeverities:         findingSeverities(findings),
			MajorEventRequestsChanges: req.MajorRequestChanges,
		})
	})
	if err != nil {
		return executionPhaseFailure(err)
	}
	result.Rollup = rollup
	result.Sessions = appendSessionIfPresent(result.Sessions, rollupLedgerSession)
	result.NamedSessionCandidate = namedSession.buildCandidate(rollupSession, opts.now())
	if namedSession.enabled && result.NamedSessionCandidate == nil {
		opts.emitWarning(fmt.Sprintf("session %q was not updated because no orchestrator session was produced", namedSession.active.Name))
	}

	plan, err := opts.buildPlan(req, prepared.reviewPR, mode.planPostMode, result.EffectiveCaps, prepared.parsed.PlanDiff, findings, rollup, selection.ThreadActions, false, result.AgentDefsChanged, planRunInputs{
		threadResponses:  threadResponses,
		repoSources:      repoSources,
		hasRun:           true,
		selection:        selectionSession,
		reviewers:        reviewerSessions,
		rollup:           rollupSession,
		selectedAgents:   selection.SelectedAgents,
		findingSessions:  findingSessions,
		reviewerFailures: reviewerFailures,
		reviewerCoverage: reviewerCoverage,
		startedAt:        now,
	})
	if err != nil {
		return nil, false, err
	}
	result.Plan = plan
	return findingSessions, false, nil
}

func executionPhaseFailure(err error) (map[review.FindingID]string, bool, error) {
	return nil, errors.Is(err, errLLMTaskFailedBlocking), err
}

func persistExecutionResult(ctx context.Context, opts Options, req Request, run ledger.Run, prepared preparedSelectionContext, findingSessions map[review.FindingID]string, result *Result) error {
	ledgerFindings := make([]ledger.Finding, 0, len(result.Plan.AnchoredFindings))
	for _, finding := range result.Plan.AnchoredFindings {
		rowID, err := sessionRowIDForFinding(finding, findingSessions)
		if err != nil {
			return err
		}
		ledgerFindings = append(ledgerFindings, ledgerFinding(run.RunID, rowID, finding))
	}
	plannedActions := make([]ledger.PlannedAction, 0, len(result.Plan.Actions))
	for _, action := range result.Plan.Actions {
		plannedActions = append(plannedActions, ledger.PlannedAction{Action: action.Action, RunID: run.RunID})
	}
	_, existingActions, hasPersistedPlanning, err := persistedPlanning(ctx, opts.Store, run.RunID)
	if err != nil {
		return err
	}
	if hasPersistedPlanning {
		plannedActions = existingActions
	} else if err := opts.Store.InsertPlanningResult(ctx, ledgerFindings, plannedActions); err != nil {
		return err
	}
	result.PlannedActions = plannedActions
	return writeArtifacts(prepared.artifacts, prepared.rawDiff, prepared.parsed.Patches, result.Catalog, result.Selection, result.Findings, result.Plan.RollupMarkdown, reviewerRuntimeArtifact(req, prepared.catalog, result.Selection, result.reviewerFastDelivered))
}

func findIncompleteDryRun(ctx context.Context, store Store, req Request, pr gitprovider.PR) (ledger.Run, bool, error) {
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return ledger.Run{}, false, err
	}
	postingIdentity := postingKey(req.PostingIdentity)
	runs, err := store.ListRunsForHeadScope(ctx, ledger.ListRunsForHeadScopeParams{
		PRKey:           prKey,
		SHA:             pr.Head.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: postingIdentity,
	})
	if err != nil {
		return ledger.Run{}, false, err
	}
	var best ledger.Run
	found := false
	for _, run := range runs {
		if run.BaseSHA != pr.Base.SHA || run.PostMode != ledger.PostModeDryRun {
			continue
		}
		if run.Outcome != nil && *run.Outcome != ledger.OutcomeIncomplete {
			continue
		}
		if strings.TrimSpace(run.ArtifactPath) == "" {
			continue
		}
		if !runartifact.MarkerMatches(run.ArtifactPath, runartifact.KindReview, run.RunID) {
			continue
		}
		if !found || run.Attempt > best.Attempt || (run.Attempt == best.Attempt && run.StartedAt.After(best.StartedAt)) {
			best = run
			found = true
		}
	}
	return best, found, nil
}

func persistedPlanning(ctx context.Context, store Store, runID string) ([]ledger.Finding, []ledger.PlannedAction, bool, error) {
	findings, err := store.ListFindings(ctx, runID)
	if err != nil {
		return nil, nil, false, err
	}
	actions, err := store.ListPlannedActions(ctx, runID)
	if err != nil {
		return nil, nil, false, err
	}
	if len(findings) == 0 && len(actions) == 0 {
		return nil, nil, false, nil
	}
	if len(actions) == 0 {
		return nil, nil, false, fmt.Errorf("pipeline: persisted planning for run %s has findings but no planned actions", runID)
	}
	return findings, actions, true, nil
}

func (c preparedSelectionContext) reviewResult() Result {
	return Result{
		PR:               c.pr,
		PRKey:            c.prKey,
		Artifacts:        c.artifacts,
		Quota:            c.quota,
		QuotaSupported:   c.quotaSupported,
		QuotaLow:         c.quotaLow,
		Catalog:          c.catalog,
		EffectiveCaps:    c.effectiveCaps,
		AgentDefsChanged: c.agentDefsChanged,
		CurrentBaseSHA:   c.currentBaseSHA,
		CurrentHeadSHA:   c.currentHeadSHA,
		ReviewBaseSHA:    c.reviewBaseSHA,
		ReviewHeadSHA:    c.reviewHeadSHA,
	}
}

func (c preparedSelectionContext) selectionResult() SelectionResult {
	return SelectionResult{
		PR:               c.pr,
		PRKey:            c.prKey,
		Artifacts:        c.artifacts,
		Quota:            c.quota,
		QuotaSupported:   c.quotaSupported,
		QuotaLow:         c.quotaLow,
		Catalog:          c.catalog,
		ParsedDiff:       c.parsed,
		ChangedFiles:     append([]string(nil), c.changedFiles...),
		Threads:          append([]gitprovider.InlineThread(nil), c.threads...),
		EffectiveCaps:    c.effectiveCaps,
		AgentDefsChanged: c.agentDefsChanged,
		CurrentBaseSHA:   c.currentBaseSHA,
		CurrentHeadSHA:   c.currentHeadSHA,
		ReviewBaseSHA:    c.reviewBaseSHA,
		ReviewHeadSHA:    c.reviewHeadSHA,
	}
}

func selectionSessionFromDraft(draft sessionDraft) SelectionSession {
	return SelectionSession{
		ProviderReportedSessionID: draft.ProviderReportedSessionID,
		ProviderSessionID:         draft.ProviderSessionID,
		Adapter:                   draft.Adapter,
		Model:                     draft.Model,
		Effort:                    draft.Effort,
		StartedAt:                 draft.StartedAt,
		CompletedAt:               draft.CompletedAt,
		Response:                  draft.Response,
	}
}

func prepareSelectionContext(ctx context.Context, opts Options, req selectionSetupRequest) (preparedSelectionContext, error) {
	reviewCtx := req.ResolvedPR
	if reviewCtx == nil {
		resolved, err := resolveReviewPRContext(ctx, opts.Provider, req.PRRef, req.ReviewBaseSHA, req.ReviewHeadSHA)
		if err != nil {
			return preparedSelectionContext{}, err
		}
		reviewCtx = &resolved
	}
	pr := reviewCtx.pr
	reviewPR := reviewCtx.reviewPR
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return preparedSelectionContext{}, err
	}
	if req.ResolveArtifacts == nil {
		return preparedSelectionContext{}, fmt.Errorf("pipeline: artifact resolver is required")
	}
	artifacts, err := req.ResolveArtifacts(reviewPR)
	if err != nil {
		return preparedSelectionContext{}, err
	}
	if err := os.MkdirAll(artifacts.AgentLogsDir, 0o700); err != nil {
		return preparedSelectionContext{}, fmt.Errorf("pipeline: create agent log dir: %w", err)
	}
	if err := os.MkdirAll(artifacts.LLMTasksDir, 0o700); err != nil {
		return preparedSelectionContext{}, fmt.Errorf("pipeline: create LLM task dir: %w", err)
	}
	if err := os.MkdirAll(artifacts.DossierDir, 0o700); err != nil {
		return preparedSelectionContext{}, fmt.Errorf("pipeline: create dossier dir: %w", err)
	}

	quota, quotaSupported, err := opts.Adapter.Quota(ctx)
	if err != nil {
		return preparedSelectionContext{}, err
	}
	diff, err := getReviewDiff(ctx, opts.Provider, req.PRRef, reviewPR.Base.SHA, reviewPR.Head.SHA, reviewCtx.pinnedReview)
	if err != nil {
		return preparedSelectionContext{}, err
	}
	parsed, err := parseUnifiedDiff(diff.Raw)
	if err != nil {
		return preparedSelectionContext{}, err
	}
	var threads []gitprovider.InlineThread
	var threadContext []threadcontext.Thread
	var reviews []gitprovider.Review
	var issueComments []gitprovider.IssueComment
	if !reviewCtx.pinnedReview {
		threads, err = opts.Provider.ListInlineThreads(ctx, req.PRRef)
		if err != nil {
			return preparedSelectionContext{}, err
		}
		if strings.TrimSpace(req.PostingIdentity.ID) != "" || strings.TrimSpace(req.PostingIdentity.Login) != "" {
			threadContext, err = threadcontext.Normalize(threads, threadcontext.Options{PostingIdentity: req.PostingIdentity})
			if err != nil {
				return preparedSelectionContext{}, err
			}
		}
		reviews, err = opts.Provider.ListReviews(ctx, req.PRRef)
		if err != nil {
			return preparedSelectionContext{}, err
		}
		issueComments, err = opts.Provider.ListIssueComments(ctx, req.PRRef)
		if err != nil {
			return preparedSelectionContext{}, err
		}
	}
	invocationRoot := ""
	if req.InvocationRoot != nil {
		invocationRoot = *req.InvocationRoot
	} else {
		invocationRoot, err = resolveInvocationRootForSafety(ctx, opts)
		if err != nil {
			return preparedSelectionContext{}, err
		}
	}
	catalog, err := agents.Load(ctx, agents.LoadOptions{
		ProfileDirs:               append([]string(nil), req.Profile.AgentSources...),
		Repo:                      &agents.RepoSource{Reader: opts.Provider, Ref: req.PRRef, PR: pr},
		FlagDirs:                  append([]string(nil), req.AgentDirs...),
		RequireSafeProfileSources: true,
		SafeProfileSourceRoot:     invocationRoot,
		AllowSoftRepoFailures:     true,
	})
	if err != nil {
		return preparedSelectionContext{}, err
	}
	if err := validateReviewerFastMode(req.ReviewRequest, catalog); err != nil {
		return preparedSelectionContext{}, err
	}

	out := preparedSelectionContext{
		pr:               pr,
		reviewPR:         reviewPR,
		prKey:            prKey,
		artifacts:        artifacts,
		rawDiff:          diff.Raw,
		quota:            quota,
		quotaSupported:   quotaSupported,
		quotaLow:         quotaSupported && (quota.BlockRemainingPct >= 0 && quota.BlockRemainingPct < 5 || quota.WeeklyRemainingPct >= 0 && quota.WeeklyRemainingPct < 5),
		parsed:           parsed,
		changedFiles:     patchPaths(parsed.Patches),
		threads:          threads,
		threadContext:    threadContext,
		reviews:          reviews,
		issueComments:    issueComments,
		catalog:          catalog,
		effectiveCaps:    effectiveCaps(opts.Provider.Capabilities(), req.NoResolveThreads),
		agentDefsChanged: agentDefinitionsChanged(parsed.Patches),
		currentBaseSHA:   reviewCtx.currentBaseSHA,
		currentHeadSHA:   reviewCtx.currentHeadSHA,
		reviewBaseSHA:    reviewPR.Base.SHA,
		reviewHeadSHA:    reviewPR.Head.SHA,
	}
	if err := dossier.WriteRaw(artifacts, dossier.Inputs{
		CurrentPR:             pr,
		ReviewPR:              reviewPR,
		PinnedReview:          reviewCtx.pinnedReview,
		ChangedFiles:          dossierChangedFiles(parsed.Patches),
		Threads:               threads,
		ThreadContext:         threadContext,
		Reviews:               reviews,
		IssueComments:         issueComments,
		Catalog:               catalog,
		CurrentBaseSHA:        reviewCtx.currentBaseSHA,
		CurrentHeadSHA:        reviewCtx.currentHeadSHA,
		DiscussionOmittedNote: "Current PR discussion omitted because this review is pinned to explicit base/head SHAs.",
	}); err != nil {
		return preparedSelectionContext{}, err
	}
	return out, nil
}

func resolveReviewPRContext(ctx context.Context, provider ReadProvider, ref gitprovider.PRRef, reviewBaseSHA, reviewHeadSHA string) (reviewPRContext, error) {
	pr, err := provider.GetPR(ctx, ref)
	if err != nil {
		return reviewPRContext{}, err
	}
	currentBaseSHA := pr.Base.SHA
	currentHeadSHA := pr.Head.SHA
	reviewBaseSHA = strings.TrimSpace(reviewBaseSHA)
	reviewHeadSHA = strings.TrimSpace(reviewHeadSHA)
	pinnedReview := reviewBaseSHA != "" || reviewHeadSHA != ""
	reviewPR := pr
	if pinnedReview {
		if err := validatePinnedReviewPR(ref, pr); err != nil {
			return reviewPRContext{}, err
		}
		reviewPR.Base.SHA = reviewBaseSHA
		reviewPR.Head.SHA = reviewHeadSHA
	}
	return reviewPRContext{
		pr:             pr,
		reviewPR:       reviewPR,
		pinnedReview:   pinnedReview,
		currentBaseSHA: currentBaseSHA,
		currentHeadSHA: currentHeadSHA,
	}, nil
}

func runSelectionPhase(ctx context.Context, opts Options, req selectionPhaseRequest) (llm.Selection, sessionDraft, ledger.Session, error) {
	runtimeConfig, err := resolveSelectionRuntimeConfig(req.Profile, req.SelectionModelOverride, req.SelectionEffortOverride)
	if err != nil {
		return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
	}
	model, effort := runtimeConfig.model, runtimeConfig.effort

	promptInput, promptDeps, err := selectionPromptInputFromArtifacts(req.Artifacts, req.Threads)
	knownThreadIDs := knownThreads(req.Threads)
	if len(req.ThreadContext) > 0 {
		promptInput, promptDeps, err = selectionPromptInputFromThreadContext(req.Artifacts, req.ThreadContext)
		knownThreadIDs = knownThreadContext(req.ThreadContext)
	}
	if err != nil {
		return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
	}
	dependencyTaskIDs := []string{dossier.SummaryTaskID}
	fingerprintDeps := append(append([]string(nil), dependencyTaskIDs...), promptDeps...)
	selectionPrompt, err := buildSelectionPrompt(req.Catalog, promptInput, req.MaxAgents, req.SelectionPromptInstructions)
	if err != nil {
		return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
	}
	if err := opts.checkPromptBudget("selection", "", model, "", selectionPrompt); err != nil {
		return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
	}
	selectionLog, err := req.Artifacts.AgentLog(orchestratorSelectionStage)
	if err != nil {
		return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
	}
	decode := func(data []byte) (llm.Selection, error) {
		return llm.DecodeSelection(data, llm.SelectionOptions{
			KnownAgents:  knownAgents(req.Catalog),
			ChangedFiles: changedFiles(req.ParsedDiff.Patches),
			KnownThreads: knownThreadIDs,
		})
	}
	selectionFingerprint := llmlifecycle.Fingerprint(opts.Adapter.Name(), orchestratorSelectionStage, "selection", model, effort, selectionPrompt, fingerprintDeps)
	hasRun := strings.TrimSpace(req.RunID) != ""
	if !hasRun {
		if err := llmlifecycle.ResetIfInputFingerprintChanged(lifecyclePaths(req.Artifacts), orchestratorSelectionStage, selectionFingerprint); err != nil {
			return llm.Selection{}, sessionDraft{}, ledger.Session{}, err
		}
	}
	selection, selectionSession, ledgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
		runID:             req.RunID,
		taskID:            orchestratorSelectionStage,
		phase:             "selection",
		allowNoRunCache:   !hasRun,
		dependencyTaskIDs: dependencyTaskIDs,
		inputFingerprint:  selectionFingerprint,
		artifacts:         req.Artifacts,
		role:              ledger.SessionRoleOrchestrator,
		model:             model,
		effort:            effort,
		logPath:           selectionLog,
		prompt:            selectionPrompt,
		baseRequest:       llm.Request{DurableSession: req.DurableSession},
		resumeSessionID:   req.ResumeSessionID,
	}, decode)
	if !hasRun {
		ledgerSession = ledger.Session{}
	}
	if err != nil {
		return llm.Selection{}, selectionSession, ledgerSession, err
	}
	selection = opts.capSelectionAgents(selection, req.MaxAgents)
	return selection, selectionSession, ledgerSession, nil
}

func analyzeReviewThreads(ctx context.Context, opts Options, req Request, run ledger.Run, artifacts ArtifactPaths, threads []threadcontext.Thread) ([]review.ThreadResponseAction, error) {
	eligible := threadcontext.PendingCRAuthoredFindingThreads(threads)
	if len(eligible) == 0 {
		return nil, nil
	}
	runtimeConfig, err := resolveThreadAnalysisRuntimeConfig(req.Profile)
	if err != nil {
		return nil, err
	}
	results := make([]threadanalysis.Result, 0, len(eligible))
	for _, thread := range eligible {
		logPath, err := artifacts.AgentLog("thread-analysis-" + string(thread.ID))
		if err != nil {
			return nil, err
		}
		result, err := threadanalysis.AnalyzeThread(ctx, threadanalysis.Options{
			Store:          opts.Store,
			RunID:          run.RunID,
			Adapter:        opts.Adapter,
			Model:          runtimeConfig.model,
			Effort:         runtimeConfig.effort,
			LogPath:        logPath,
			LifecyclePaths: lifecyclePaths(artifacts),
			Progress:       opts.TaskProgress,
			Now:            opts.now,
			NewStepID:      opts.newSessionRowID,
		}, thread)
		if err != nil {
			return nil, pipelineTaskError(err)
		}
		results = append(results, result)
	}
	return threadanalysis.ResponseActions(results), nil
}

func (opts Options) capSelectionAgents(selection llm.Selection, maxAgents int) llm.Selection {
	if maxAgents <= 0 || len(selection.SelectedAgents) <= maxAgents {
		return selection
	}
	opts.emitWarning(fmt.Sprintf("orchestrator selected %d agents; using first %d due to max-agents", len(selection.SelectedAgents), maxAgents))
	selection.SelectedAgents = append([]llm.SelectedAgent(nil), selection.SelectedAgents[:maxAgents]...)
	return selection
}

func getReviewDiff(ctx context.Context, provider ReadProvider, ref gitprovider.PRRef, baseSHA, headSHA string, pinned bool) (gitprovider.UnifiedDiff, error) {
	if !pinned {
		return provider.GetDiff(ctx, ref)
	}
	rangeProvider, ok := provider.(rangeDiffProvider)
	if !ok {
		return gitprovider.UnifiedDiff{}, fmt.Errorf("pipeline: provider does not support pinned base/head diff review")
	}
	return rangeProvider.GetDiffBetweenRefs(ctx, ref, baseSHA, headSHA)
}

func validatePinnedReviewPR(ref gitprovider.PRRef, pr gitprovider.PR) error {
	if pr.Head.Host != "" && pr.Head.Host != ref.Host || pr.Head.Owner != "" && pr.Head.Owner != ref.Owner || pr.Head.Repo != "" && pr.Head.Repo != ref.Repo {
		return fmt.Errorf("pipeline: pinned base/head review does not support fork PR heads; head repository %s/%s differs from base repository %s/%s", pr.Head.Owner, pr.Head.Repo, ref.Owner, ref.Repo)
	}
	return nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func sessionRowIDForFinding(finding reviewplan.AnchoredFinding, findingSession map[review.FindingID]string) (string, error) {
	rowID := strings.TrimSpace(findingSession[finding.FindingID])
	if rowID == "" {
		return "", fmt.Errorf("pipeline: missing reviewer session row for finding %q", finding.FindingID)
	}
	return rowID, nil
}

func appendSessionIfPresent(sessions []ledger.Session, session ledger.Session) []ledger.Session {
	if strings.TrimSpace(session.SessionRowID) == "" {
		return sessions
	}
	return append(sessions, session)
}

func appendSessionsIfPresent(sessions []ledger.Session, more ...ledger.Session) []ledger.Session {
	for _, session := range more {
		sessions = appendSessionIfPresent(sessions, session)
	}
	return sessions
}

func runReviewers(ctx context.Context, opts Options, req Request, runID string, pr gitprovider.PR, catalog agents.Catalog, parsed ParsedDiff, artifacts ArtifactPaths, selection llm.Selection, dependencyTaskIDs []string, maxConcurrency int) ([]review.Finding, []llm.Findings, []sessionDraft, []ledger.Session, map[review.FindingID]string, []ReviewerFailure, error) {
	type job struct {
		selected llm.SelectedAgent
		agent    agents.Agent
	}
	var jobs []job
	for _, selected := range selection.SelectedAgents {
		agent, ok := catalog.Find(selected.AgentID)
		if !ok {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("pipeline: selected agent %q not found", selected.AgentID)
		}
		jobs = append(jobs, job{selected: selected, agent: agent})
	}
	if len(jobs) == 0 {
		return nil, nil, nil, nil, nil, nil, nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].agent.ID < jobs[j].agent.ID })

	reviewCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var allFindings []review.Finding
	var reviewerResults []llm.Findings
	var sessions []sessionDraft
	var ledgerSessions []ledger.Session
	findingSessions := map[review.FindingID]string{}
	var failures []ReviewerFailure
	var firstErr error
	var wg sync.WaitGroup
	for _, current := range jobs {
		current := current
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-reviewCtx.Done():
				return
			}
			defer func() { <-sem }()
			result, session, ledgerSession, failure, err := runReviewer(reviewCtx, opts, req, runID, pr, parsed, artifacts, current.selected, current.agent, dependencyTaskIDs)
			mu.Lock()
			defer mu.Unlock()
			if failure != nil {
				failures = append(failures, *failure)
				sessions = append(sessions, session)
				ledgerSessions = appendSessionIfPresent(ledgerSessions, ledgerSession)
				return
			}
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			sessions = append(sessions, session)
			ledgerSessions = appendSessionIfPresent(ledgerSessions, ledgerSession)
			reviewerResults = append(reviewerResults, result)
			for _, finding := range result.Findings {
				allFindings = append(allFindings, finding)
				findingSessions[finding.ID] = session.RowID
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, nil, nil, nil, nil, firstErr
	}
	sort.Slice(allFindings, func(i, j int) bool { return allFindings[i].ID < allFindings[j].ID })
	sort.Slice(reviewerResults, func(i, j int) bool { return reviewerResults[i].AgentID < reviewerResults[j].AgentID })
	sort.Slice(failures, func(i, j int) bool { return failures[i].AgentID < failures[j].AgentID })
	return allFindings, reviewerResults, sessions, ledgerSessions, findingSessions, failures, nil
}

func runReviewer(ctx context.Context, opts Options, req Request, runID string, pr gitprovider.PR, parsed ParsedDiff, artifacts ArtifactPaths, selected llm.SelectedAgent, agent agents.Agent, dependencyTaskIDs []string) (llm.Findings, sessionDraft, ledger.Session, *ReviewerFailure, error) {
	runtimeConfig, err := resolveReviewerRuntimeConfig(req, agent)
	if err != nil {
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	model, effort := runtimeConfig.model, runtimeConfig.effort
	changedFilePaths := patchPaths(parsed.Patches)
	assignmentScope := reviewerAssignmentScope(selected, changedFilePaths)
	prompt, promptDeps, err := buildReviewerPrompt(artifacts, pr, selected, agent, changedFilePaths)
	if err != nil {
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	if err := opts.checkPromptBudget("reviewer", agent.ID, model, strings.Join(selected.Files, ","), prompt); err != nil {
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	logPath, err := artifacts.AgentLog(agent.ID)
	if err != nil {
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	agentID := agent.ID
	taskID := reviewerTaskID(agent.ID)
	request, cleanupWorkspace, err := workbench.PrepareReviewerRequest(ctx, workbenchDeps(opts), opts.Adapter, artifacts, pr.Head.SHA, agent.ID, selected.AllowedFiles, model, effort, prompt, logPath)
	if err != nil {
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	if req.ReviewerFast {
		request.Fast = true
	}
	defer func() {
		if cleanupWorkspace != nil {
			if cleanupErr := cleanupWorkspace(); cleanupErr != nil {
				opts.emitWarning(fmt.Sprintf("cleanup reviewer workspace for %s: %v", agent.ID, cleanupErr))
			}
		}
	}()
	fingerprintDeps := append(append([]string(nil), dependencyTaskIDs...), promptDeps...)
	if req.ReviewerFast {
		fingerprintDeps = append(fingerprintDeps, "fast=true")
	}
	findings, session, ledgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
		runID:             runID,
		taskID:            taskID,
		phase:             "reviewer",
		dependencyTaskIDs: dependencyTaskIDs,
		inputFingerprint:  llmlifecycle.Fingerprint(opts.Adapter.Name(), taskID, "reviewer", model, effort, prompt, fingerprintDeps),
		artifacts:         artifacts,
		role:              ledger.SessionRoleReviewer,
		agentID:           &agentID,
		model:             model,
		effort:            effort,
		logPath:           logPath,
		prompt:            prompt,
		baseRequest:       request,
		llmFailureStatus:  llmTaskStatusFailedIsolated,
	}, func(data []byte) (llm.Findings, error) {
		return llm.DecodeFindings(data, llm.FindingsOptions{
			KnownAgents:  map[string]bool{agent.ID: true},
			ChangedFiles: stringSet(assignmentScope),
			NewFindingID: opts.newFindingID,
		})
	})
	if err != nil {
		var taskErr *llmTaskError
		if errors.As(err, &taskErr) && taskErr.status == llmTaskStatusFailedIsolated {
			return llm.Findings{}, session, ledgerSession, &ReviewerFailure{
				TaskID:  taskID,
				AgentID: agent.ID,
				Error:   sanitizeTaskErrorForMarkdown(err),
			}, nil
		}
		return llm.Findings{}, sessionDraft{}, ledger.Session{}, nil, err
	}
	return findings, session, ledgerSession, nil, nil
}

func reviewerTaskID(agentID string) string {
	return "reviewer-" + statepaths.Encode(agentID)
}

func reviewerTaskIDs(selected []llm.SelectedAgent) []string {
	out := make([]string, 0, len(selected))
	for _, agent := range selected {
		out = append(out, reviewerTaskID(agent.AgentID))
	}
	sort.Strings(out)
	return out
}

type llmTaskSpec struct {
	runID             string
	taskID            string
	phase             string
	dependencyTaskIDs []string
	allowNoRunCache   bool
	inputFingerprint  string
	artifacts         ArtifactPaths
	role              ledger.SessionRole
	agentID           *string
	model             string
	effort            string
	logPath           string
	prompt            string
	baseRequest       llm.Request
	resumeSessionID   string
	llmFailureStatus  llmTaskStatus
}

func lifecyclePaths(paths ArtifactPaths) llmlifecycle.Paths {
	return llmlifecycle.Paths{LLMTasksDir: paths.LLMTasksDir}
}

func pipelineTaskError(err error) error {
	var taskErr *llmlifecycle.TaskError
	if !errors.As(err, &taskErr) {
		return err
	}
	return &llmTaskError{status: llmTaskStatus(taskErr.Status()), err: errors.Unwrap(taskErr)}
}

func runStructuredTask[T any](ctx context.Context, opts Options, spec llmTaskSpec, decode llm.Decoder[T]) (T, sessionDraft, ledger.Session, error) {
	var zero T
	result, err := llmlifecycle.RunStructured(ctx, llmlifecycle.Request{
		Store:             opts.Store,
		Adapter:           opts.Adapter,
		RunID:             spec.runID,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		DependencyTaskIDs: spec.dependencyTaskIDs,
		AllowNoRunCache:   spec.allowNoRunCache,
		InputFingerprint:  spec.inputFingerprint,
		Paths:             lifecyclePaths(spec.artifacts),
		Role:              spec.role,
		AgentID:           spec.agentID,
		Model:             spec.model,
		Effort:            spec.effort,
		LogPath:           spec.logPath,
		Prompt:            spec.prompt,
		BaseRequest:       spec.baseRequest,
		ResumeSessionID:   spec.resumeSessionID,
		FailureStatus:     llmlifecycle.Status(spec.llmFailureStatus),
		Progress:          opts.TaskProgress,
		Now:               opts.now,
		NewSessionRowID:   opts.newSessionRowID,
	}, decode)
	draft := result.Draft
	if err != nil {
		return zero, draft, result.Session, pipelineTaskError(err)
	}
	return result.Value, draft, result.Session, nil
}

func sanitizeTaskErrorForMarkdown(err error) string {
	if err == nil {
		return ""
	}
	value := strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
	value = strings.ReplaceAll(value, "<!-- codereview:", "&lt;!-- codereview:")
	if utf8.RuneCountInString(value) > 1000 {
		runes := []rune(value)
		value = string(runes[:1000]) + "..."
	}
	return value
}

func prepareNamedSession(ctx context.Context, opts Options, req Request, live bool, model string, now time.Time) (namedSessionState, error) {
	explicitName := strings.TrimSpace(req.SessionName)
	if explicitName != "" && !live {
		return namedSessionState{}, fmt.Errorf("pipeline: named session %q requires live review", explicitName)
	}
	if opts.NamedSessions == nil {
		if explicitName != "" {
			return namedSessionState{}, fmt.Errorf("pipeline: named session store is required")
		}
		return namedSessionState{}, nil
	}
	name := explicitName
	if name == "" {
		var err error
		name, err = defaultSessionName(req)
		if err != nil {
			return namedSessionState{}, err
		}
	}

	active := sessionreuse.Normalize(sessionreuse.Scope{
		Name:     name,
		Profile:  req.ProfileName,
		Provider: string(req.Profile.LLM.Provider),
		Adapter:  opts.Adapter.Name(),
		Model:    model,
		Host:     req.PRRef.Host,
	})
	if err := sessionreuse.Validate(active); err != nil {
		return namedSessionState{}, err
	}

	state := namedSessionState{
		enabled:        true,
		active:         active,
		supportsResume: opts.Adapter.SupportsResume(),
		createdAt:      now,
	}
	if req.FreshSession {
		return state, nil
	}
	stored, err := opts.NamedSessions.GetNamedSession(ctx, active.Name)
	if errors.Is(err, ledger.ErrNotFound) {
		stored = ledger.NamedSession{}
	} else if err != nil {
		return namedSessionState{}, fmt.Errorf("pipeline: get named session %q: %w", active.Name, err)
	} else {
		storedScope := sessionreuse.Normalize(sessionreuse.Scope{
			Name:     stored.Name,
			Profile:  stored.Profile,
			Provider: stored.Provider,
			Adapter:  stored.Adapter,
			Model:    stored.Model,
			Host:     stored.Host,
		})
		check, err := sessionreuse.Check(storedScope, active)
		if err != nil {
			if explicitName != "" {
				return namedSessionState{}, err
			}
			opts.emitWarning(fmt.Sprintf("%v; starting fresh", err))
			return state, nil
		}
		if check.Warning != "" {
			opts.emitWarning(check.Warning)
		}
		state.stored = &stored
		state.createdAt = stored.CreatedAt
		if state.supportsResume {
			if stored.DurableSession || !requiresDurableSessionMarker(active.Adapter) {
				state.currentProviderSessionID = stored.ProviderSessionID
			} else {
				opts.emitWarning(fmt.Sprintf("session %q stored adapter session predates durable resume support; starting fresh", active.Name))
			}
		}
	}

	if !state.supportsResume {
		opts.emitWarning(fmt.Sprintf("session %q adapter %q does not support resume; starting fresh", active.Name, opts.Adapter.Name()))
	}
	return state, nil
}

func defaultSessionName(req Request) (string, error) {
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return "", err
	}
	scope, err := statepaths.ResumeScope(req.ProfileName, postingKey(req.PostingIdentity))
	if err != nil {
		return "", err
	}
	return "default:" + prKey + "__" + scope, nil
}

func requiresDurableSessionMarker(adapter string) bool {
	return strings.TrimSpace(adapter) == "codex_cli"
}

func (s *namedSessionState) resumeID() string {
	if s == nil || !s.enabled || !s.supportsResume {
		return ""
	}
	return s.currentProviderSessionID
}

func (s *namedSessionState) recordSessionID(draft sessionDraft) {
	if s == nil || !s.enabled || !s.supportsResume {
		return
	}
	if strings.TrimSpace(draft.ProviderReportedSessionID) != "" {
		s.currentProviderSessionID = draft.ProviderReportedSessionID
	}
}

func (s *namedSessionState) buildCandidate(draft sessionDraft, lastUsedAt time.Time) *ledger.NamedSession {
	if s == nil || !s.enabled {
		return nil
	}
	providerSessionID := strings.TrimSpace(draft.ProviderReportedSessionID)
	if providerSessionID == "" {
		return nil
	}
	return &ledger.NamedSession{
		Name:              s.active.Name,
		Profile:           s.active.Profile,
		Provider:          s.active.Provider,
		Adapter:           s.active.Adapter,
		Model:             s.active.Model,
		Host:              s.active.Host,
		ProviderSessionID: providerSessionID,
		DurableSession:    s.supportsResume,
		CreatedAt:         s.createdAt,
		LastUsedAt:        lastUsedAt,
	}
}

// planRunInputs carries the session telemetry buildPlan turns into the
// rollup's RunSummary and finding attribution.
type planRunInputs struct {
	hasRun           bool
	selection        sessionDraft
	reviewers        []sessionDraft
	rollup           sessionDraft
	selectedAgents   []llm.SelectedAgent
	threadResponses  []review.ThreadResponseAction
	repoSources      []agents.SourceInfo
	findingSessions  map[review.FindingID]string
	reviewerFailures []ReviewerFailure
	reviewerCoverage []reviewplan.ReviewerCoverageSummary
	startedAt        time.Time
}

func (opts Options) buildRunSummary(req Request, inputs planRunInputs) (reviewplan.RunSummary, map[review.FindingID]string) {
	if !inputs.hasRun {
		return reviewplan.RunSummary{}, nil
	}
	reviewerByAgent := map[string]sessionDraft{}
	agentByRow := map[string]string{}
	for _, draft := range inputs.reviewers {
		if draft.AgentID == nil {
			continue
		}
		reviewerByAgent[*draft.AgentID] = draft
		agentByRow[draft.RowID] = *draft.AgentID
	}

	workstreams := []reviewplan.WorkstreamUsage{workstreamUsage(orchestratorSelectionStage, inputs.selection)}
	selectedIDs := make([]string, 0, len(inputs.selectedAgents))
	for _, selected := range inputs.selectedAgents {
		selectedIDs = append(selectedIDs, selected.AgentID)
		if draft, ok := reviewerByAgent[selected.AgentID]; ok {
			workstreams = append(workstreams, workstreamUsage(selected.AgentID, draft))
		}
	}
	workstreams = append(workstreams, workstreamUsage(orchestratorRollupStage, inputs.rollup))

	wallMS := opts.now().Sub(inputs.startedAt).Milliseconds()
	summary := reviewplan.RunSummary{
		ToolVersion:       req.ToolVersion,
		Adapter:           inputs.selection.Adapter,
		Model:             sharedWorkstreamModel(workstreams),
		PostingIdentity:   postingKey(req.PostingIdentity),
		SelectedReviewers: selectedIDs,
		ReviewerFailures:  reviewerFailureSummaries(inputs.reviewerFailures),
		ReviewerCoverage:  inputs.reviewerCoverage,
		WallDurationMS:    &wallMS,
		Workstreams:       workstreams,
	}

	findingReviewers := make(map[review.FindingID]string, len(inputs.findingSessions))
	for id, rowID := range inputs.findingSessions {
		if agentID, ok := agentByRow[rowID]; ok {
			findingReviewers[id] = agentID
		}
	}
	return summary, findingReviewers
}

func reviewerFailureSummaries(failures []ReviewerFailure) []reviewplan.ReviewerFailureSummary {
	out := make([]reviewplan.ReviewerFailureSummary, 0, len(failures))
	for _, failure := range failures {
		out = append(out, reviewplan.ReviewerFailureSummary{
			AgentID: failure.AgentID,
			Error:   failure.Error,
		})
	}
	return out
}

func buildReviewerCoverage(selected []llm.SelectedAgent, results []llm.Findings, failures []ReviewerFailure, changedFiles []string) []reviewplan.ReviewerCoverageSummary {
	if len(selected) == 0 {
		return nil
	}
	resultByAgent := make(map[string]llm.Findings, len(results))
	for _, result := range results {
		resultByAgent[result.AgentID] = result
	}
	failureByAgent := make(map[string]ReviewerFailure, len(failures))
	for _, failure := range failures {
		failureByAgent[failure.AgentID] = failure
	}
	assigned := map[string]bool{}
	out := make([]reviewplan.ReviewerCoverageSummary, 0, len(selected)+1)
	for _, agent := range selected {
		scope := reviewerAssignmentScope(agent, changedFiles)
		for _, file := range scope {
			assigned[file] = true
		}
		entry := reviewplan.ReviewerCoverageSummary{
			AgentID: agent.AgentID,
			Scope:   scope,
		}
		if failure, ok := failureByAgent[agent.AgentID]; ok {
			entry.Status = reviewerCoverageIncompleteFailed
			entry.Diagnostic = failure.Error
			out = append(out, entry)
			continue
		}
		result, ok := resultByAgent[agent.AgentID]
		if !ok {
			entry.Status = reviewerCoverageIncompleteFailed
			entry.Diagnostic = "reviewer result was not recorded"
			out = append(out, entry)
			continue
		}
		entry.InspectedFiles = copySortedStrings(result.InspectedFiles)
		entry.SkippedFiles = sortedIntersection(result.SkippedFiles, scope)
		entry.Constraints = copySortedStrings(result.Constraints)
		missing := coverageMissingFiles(scope, entry.InspectedFiles, entry.SkippedFiles)
		switch {
		case len(entry.SkippedFiles) > 0 || len(missing) > 0:
			entry.Status = reviewerCoverageIncompleteSkipped
			if len(missing) > 0 {
				entry.Diagnostic = "assigned files were neither inspected nor skipped: " + strings.Join(missing, ", ")
			}
		case len(agent.AllowedFiles) > 0:
			entry.Status = reviewerCoverageCompleteConstrained
		default:
			entry.Status = reviewerCoverageCompleteBroad
		}
		out = append(out, entry)
	}
	var unassigned []string
	for _, file := range changedFiles {
		if !assigned[file] {
			unassigned = append(unassigned, file)
		}
	}
	if len(unassigned) > 0 {
		out = append(out, reviewplan.ReviewerCoverageSummary{
			AgentID:      "unassigned",
			Status:       reviewerCoverageIncompleteUnassigned,
			SkippedFiles: copySortedStrings(unassigned),
			Diagnostic:   "changed files were not assigned to a selected reviewer",
		})
	}
	return out
}

// reviewerAssignmentScope is the coverage expectation: selected or allowed files
// define the work the reviewer was asked to cover.
func reviewerAssignmentScope(agent llm.SelectedAgent, changedFiles []string) []string {
	if len(agent.AllowedFiles) > 0 {
		return copySortedStrings(agent.AllowedFiles)
	}
	if len(agent.Files) > 0 {
		return copySortedStrings(agent.Files)
	}
	return copySortedStrings(changedFiles)
}

func coverageMissingFiles(scope, inspected, skipped []string) []string {
	covered := stringSet(inspected)
	for _, file := range skipped {
		covered[file] = true
	}
	var missing []string
	for _, file := range scope {
		if !covered[file] {
			missing = append(missing, file)
		}
	}
	return copySortedStrings(missing)
}

func sortedIntersection(values, scope []string) []string {
	inScope := stringSet(scope)
	var out []string
	for _, value := range values {
		if inScope[value] {
			out = append(out, value)
		}
	}
	return copySortedStrings(out)
}

func stringSet(values []string) map[string]bool {
	return setBy(values, func(value string) string { return value })
}

func setBy[T any](values []T, key func(T) string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[key(value)] = true
	}
	return out
}

func copySortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// Orchestrator stage names share a stable prefix so the headline model filter
// can exclude them; deriving the stage names from the prefix keeps the filter
// and the names from diverging.
const (
	orchestratorWorkstreamPrefix = "orchestrator-"
	orchestratorSelectionStage   = orchestratorWorkstreamPrefix + "selection"
	orchestratorRollupStage      = orchestratorWorkstreamPrefix + "rollup"
)

// sharedWorkstreamModel reports the run's headline model. It reflects the model
// the reviewer agents ran on, excluding the orchestrator selection/rollup stages
// — those run on a cheaper baseline tier, so including them would blank the
// headline on every mixed-tier run (e.g. Sonnet orchestrators + Opus reviewers).
// Distinct reviewer models are comma-joined in first-seen order; if no reviewer
// reported a model, it falls back to any reported model, else "".
func sharedWorkstreamModel(workstreams []reviewplan.WorkstreamUsage) string {
	reviewer := distinctWorkstreamModels(workstreams, true)
	if len(reviewer) > 0 {
		return strings.Join(reviewer, ", ")
	}
	return strings.Join(distinctWorkstreamModels(workstreams, false), ", ")
}

// distinctWorkstreamModels returns the distinct non-empty models in first-seen
// order. When reviewersOnly is set, orchestrator stages are excluded.
func distinctWorkstreamModels(workstreams []reviewplan.WorkstreamUsage, reviewersOnly bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, workstream := range workstreams {
		if workstream.Model == "" {
			continue
		}
		if reviewersOnly && strings.HasPrefix(workstream.Name, orchestratorWorkstreamPrefix) {
			continue
		}
		if seen[workstream.Model] {
			continue
		}
		seen[workstream.Model] = true
		out = append(out, workstream.Model)
	}
	return out
}

func workstreamUsage(name string, draft sessionDraft) reviewplan.WorkstreamUsage {
	usage := draft.Response.Usage
	workstream := reviewplan.WorkstreamUsage{
		Name:        name,
		Model:       draft.Model,
		TokensIn:    usage.TokensIn,
		TokensOut:   usage.TokensOut,
		CacheRead:   usage.CacheRead,
		CacheCreate: usage.CacheCreate,
		CostUSD:     usage.CostUSD,
	}
	// When the adapter reports no cost (e.g. subscription auth), estimate it from
	// tokens at public list prices — only for models the price table knows, so an
	// agent's unpriced model leaves cost unavailable rather than wrong.
	if workstream.CostUSD == nil {
		if est, ok := pricing.EstimateUSD(
			draft.Model, usage.TokensIn, usage.TokensOut, usage.CacheRead, usage.CacheCreate,
		); ok {
			workstream.CostUSD = &est
			workstream.CostEstimated = true
		}
	}
	// Zero means the adapter never reported a duration; fall back to the
	// pipeline's own start/complete clock for the workstream, and render
	// unavailable (not 0s) when neither source has data.
	switch {
	case draft.Response.DurationMS > 0:
		duration := draft.Response.DurationMS
		workstream.DurationMS = &duration
	case !draft.StartedAt.IsZero() && draft.CompletedAt.After(draft.StartedAt):
		duration := draft.CompletedAt.Sub(draft.StartedAt).Milliseconds()
		workstream.DurationMS = &duration
	}
	return workstream
}

func (opts Options) buildPlan(req Request, pr gitprovider.PR, postMode reviewplan.PostMode, caps reviewplan.ProviderCaps, diff reviewplan.Diff, findings []review.Finding, rollup review.Rollup, threadActions []review.ThreadAction, noDiff bool, agentDefsChanged bool, runInputs planRunInputs) (reviewplan.Plan, error) {
	runSummary, findingReviewers := opts.buildRunSummary(req, runInputs)
	return reviewplan.Build(reviewplan.Request{
		PostMode:                      postMode,
		ProviderCaps:                  caps,
		Diff:                          diff,
		Findings:                      findings,
		Rollup:                        rollup,
		ThreadActions:                 threadActions,
		ThreadResponses:               append([]review.ThreadResponseAction(nil), runInputs.threadResponses...),
		RepoGuidanceUnavailable:       dossier.RepoGuidanceUnavailableReason(runInputs.repoSources) != "",
		RepoGuidanceUnavailableReason: dossier.RepoGuidanceUnavailableReason(runInputs.repoSources),
		EventOptions: reviewplan.EventOptions{
			MajorEventRequestsChanges: req.MajorRequestChanges,
			PostingIdentityIsPRAuthor: sameIdentity(pr.Author, req.PostingIdentity),
			AllowSelfApprove:          req.AllowSelfApprove || req.Profile.ReviewPolicy.AllowSelfApprove,
		},
		NoDiff:                  noDiff,
		Profile:                 req.ProfileName,
		PostingIdentity:         postingKey(req.PostingIdentity),
		HeadSHA:                 pr.Head.SHA,
		AgentDefinitionsChanged: agentDefsChanged,
		RunSummary:              runSummary,
		FindingReviewers:        findingReviewers,
		Now:                     opts.now,
		NewActionID:             opts.newActionID,
	})
}

func (opts Options) emitWarning(warning string) {
	if opts.Warnings == nil || strings.TrimSpace(warning) == "" {
		return
	}
	_, _ = fmt.Fprintln(opts.Warnings, warning)
}

func pruneRetention(ctx context.Context, layout statepaths.Layout, store datalifecycle.Store, now func() time.Time, warnings io.Writer, retention datalifecycle.RetentionPolicy, manualOnly bool) error {
	if manualOnly {
		return nil
	}
	result, err := datalifecycle.Prune(ctx, datalifecycle.Options{
		Layout: layout,
		Store:  store,
		Now:    now,
	}, datalifecycle.PruneOptions{Retention: retention})
	if err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if warnings != nil {
			_, _ = fmt.Fprintf(warnings, "warning: %s\n", warning)
		}
	}
	return nil
}

func ledgerFinding(runID, sessionRowID string, finding reviewplan.AnchoredFinding) ledger.Finding {
	out := ledger.Finding{
		FindingID:    finding.FindingID,
		RunID:        runID,
		SessionRowID: sessionRowID,
		Severity:     finding.Severity,
		FilePath:     finding.FilePath,
		Anchoring:    finding.Anchoring,
		Body:         finding.Body,
	}
	if finding.Side != nil {
		side := *finding.Side
		out.Side = &side
	}
	if finding.Line != nil {
		line := int64(*finding.Line)
		out.Line = &line
	}
	if finding.DiffPosition != nil {
		diffPosition := int64(*finding.DiffPosition)
		out.DiffPosition = &diffPosition
	}
	return out
}

func validate(opts Options, req Request) error {
	if err := validateCommonOptions(opts); err != nil {
		return err
	}
	if opts.Store == nil {
		return fmt.Errorf("pipeline: store is required")
	}
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("pipeline: data root is required")
	}
	if err := validateRequestCore(req.PRRef, req.ProfileName, req.ReviewBaseSHA, req.ReviewHeadSHA); err != nil {
		return err
	}
	if strings.TrimSpace(req.PRURL) == "" {
		return fmt.Errorf("pipeline: PR URL is required")
	}
	if strings.TrimSpace(postingKey(req.PostingIdentity)) == "" {
		return fmt.Errorf("pipeline: posting identity is required")
	}
	return nil
}

func validateSelectionOnly(opts Options, req SelectionRequest) error {
	if err := validateCommonOptions(opts); err != nil {
		return err
	}
	if err := validateRequestCore(req.PRRef, req.ProfileName, req.ReviewBaseSHA, req.ReviewHeadSHA); err != nil {
		return err
	}
	if strings.TrimSpace(req.ArtifactDir) == "" {
		return fmt.Errorf("pipeline: selection artifact dir is required")
	}
	if strings.TrimSpace(postingKey(req.PostingIdentity)) == "" {
		return fmt.Errorf("pipeline: selection posting identity is required")
	}
	return nil
}

func validateCommonOptions(opts Options) error {
	if opts.Provider == nil {
		return fmt.Errorf("pipeline: provider is required")
	}
	if opts.Adapter == nil {
		return fmt.Errorf("pipeline: adapter is required")
	}
	return nil
}

func validateRequestCore(ref gitprovider.PRRef, profileName, reviewBaseSHA, reviewHeadSHA string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(profileName) == "" {
		return fmt.Errorf("pipeline: profile is required")
	}
	if err := validateReviewSHAs(reviewBaseSHA, reviewHeadSHA); err != nil {
		return err
	}
	return nil
}

var reviewSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func validateReviewSHAs(baseSHA, headSHA string) error {
	baseSHA = strings.TrimSpace(baseSHA)
	headSHA = strings.TrimSpace(headSHA)
	if baseSHA == "" && headSHA == "" {
		return nil
	}
	if baseSHA == "" || headSHA == "" {
		return fmt.Errorf("pipeline: review base and head SHAs must be set together")
	}
	if !reviewSHAPattern.MatchString(baseSHA) {
		return fmt.Errorf("pipeline: review base SHA must be a 7-64 character hex SHA")
	}
	if !reviewSHAPattern.MatchString(headSHA) {
		return fmt.Errorf("pipeline: review head SHA must be a 7-64 character hex SHA")
	}
	return nil
}

// ArtifactPathsForRun returns the artifact paths for a generated run ID.
func ArtifactPathsForRun(layout statepaths.Layout, ref gitprovider.PRRef, pr gitprovider.PR, profile, postingIdentity, runID string) (ArtifactPaths, error) {
	return runartifact.ForRun(layout, ref, pr, profile, postingIdentity, runID)
}

// ArtifactPathsFromDir returns the artifact path set rooted at dir.
func ArtifactPathsFromDir(dir string) ArtifactPaths {
	return runartifact.FromDir(dir)
}

// HasLLMTaskMetadata reports whether an artifact directory contains at least
// one completed task metadata file. Metadata is written last, so its presence is
// the durable boundary for safe task resume.
func HasLLMTaskMetadata(artifactDir string) (bool, error) {
	paths := ArtifactPathsFromDir(artifactDir)
	entries, err := os.ReadDir(paths.LLMTasksDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pipeline: read LLM task dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(paths.LLMTasksDir, entry.Name(), "metadata.json")
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("pipeline: stat LLM task metadata: %w", err)
		}
	}
	return false, nil
}

func effectiveCaps(caps gitprovider.ProviderCaps, noResolve bool) reviewplan.ProviderCaps {
	return reviewplan.ProviderCaps{
		NativeFileLevelComments: caps.NativeFileLevelComments,
		ThreadResolution:        caps.ThreadResolution && !noResolve,
	}
}

func (opts Options) checkPromptBudget(phase, agentID, model, filePath, prompt string) error {
	limit := opts.Budget.MaxPromptBytes
	if limit == 0 {
		limit = defaultMaxPromptBytes
	}
	if limit < 0 || len(prompt) <= limit {
		return nil
	}
	target := phase
	if agentID != "" {
		target += " agent " + agentID
	}
	if filePath != "" {
		target += " file " + filePath
	}
	if model != "" {
		target += " model " + model
	}
	return fmt.Errorf("pipeline: context budget exceeded for %s: %d bytes > %d", target, len(prompt), limit)
}

func (opts Options) now() time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}
	return time.Now().UTC()
}

func (opts Options) newRunID() string {
	if opts.NewRunID != nil {
		return opts.NewRunID()
	}
	return uuid.NewString()
}

func (opts Options) newSessionRowID() string {
	if opts.NewSessionRowID != nil {
		return opts.NewSessionRowID()
	}
	return uuid.NewString()
}

func (opts Options) newFindingID() (review.FindingID, error) {
	if opts.NewFindingID != nil {
		return opts.NewFindingID()
	}
	return review.FindingID(uuid.NewString()), nil
}

func (opts Options) newActionID(kind reviewplan.ActionKind) (string, error) {
	if opts.NewActionID != nil {
		return opts.NewActionID(kind)
	}
	return string(kind) + "-" + uuid.NewString(), nil
}

func (opts Options) maxAgents() int {
	if opts.MaxAgents <= 0 {
		return defaultMaxAgents
	}
	return opts.MaxAgents
}

func (opts Options) maxConcurrency(maxAgents int) int {
	if opts.MaxConcurrency <= 0 {
		if maxAgents > 0 {
			return maxAgents
		}
		return defaultMaxConcurrency
	}
	return opts.MaxConcurrency
}

func (opts Options) resolveRepoRoot(ctx context.Context) (string, error) {
	if opts.ResolveRepoRoot != nil {
		return opts.ResolveRepoRoot(ctx)
	}
	return reporoot.Resolve(ctx, "", opts.GitCommand)
}

func workbenchDeps(opts Options) workbench.Deps {
	return workbench.Deps{GitCommand: opts.GitCommand, ResolveRepoRoot: opts.ResolveRepoRoot}
}

func dossierEnv(opts Options) dossier.Env {
	return dossier.Env{
		Adapter:         opts.Adapter,
		Store:           opts.Store,
		TaskProgress:    opts.TaskProgress,
		Now:             opts.now,
		NewSessionRowID: opts.newSessionRowID,
		CheckPromptBudget: func(model, prompt string) error {
			return opts.checkPromptBudget("dossier-summary", "", model, "", prompt)
		},
	}
}

func dossierChangedFiles(patches []FilePatch) []dossier.ChangedFile {
	files := make([]dossier.ChangedFile, len(patches))
	for i, patch := range patches {
		files[i] = dossier.ChangedFile{
			OldPath:   patch.OldPath,
			Path:      patch.Path,
			Patch:     patch.Patch,
			Binary:    patch.Binary,
			Deleted:   patch.Deleted,
			HunkCount: len(patch.Hunks),
		}
	}
	return files
}

func resolveInvocationRootForSafety(ctx context.Context, opts Options) (string, error) {
	root, err := opts.resolveRepoRoot(ctx)
	if errors.Is(err, reporoot.ErrUnavailable) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return root, nil
}

func knownAgents(catalog agents.Catalog) map[string]bool {
	return setBy(catalog.Agents, func(agent agents.Agent) string { return agent.ID })
}

func changedFiles(patches []FilePatch) map[string]bool {
	paths := make([]string, 0, len(patches)*2)
	for _, patch := range patches {
		if patch.Path != "" {
			paths = append(paths, patch.Path)
		}
		if patch.OldPath != "" {
			paths = append(paths, patch.OldPath)
		}
	}
	return stringSet(paths)
}

func knownThreads(threads []gitprovider.InlineThread) map[string]bool {
	return threadIDSet(threads, func(thread gitprovider.InlineThread) gitprovider.ThreadID { return thread.ID })
}

func knownThreadContext(threads []threadcontext.Thread) map[string]bool {
	return threadIDSet(threads, func(thread threadcontext.Thread) gitprovider.ThreadID { return thread.ID })
}

func threadIDSet[T any](threads []T, id func(T) gitprovider.ThreadID) map[string]bool {
	return setBy(threads, func(thread T) string { return string(id(thread)) })
}

func patchPaths(patches []FilePatch) []string {
	paths := make([]string, 0, len(patches))
	for _, patch := range patches {
		paths = append(paths, patch.Path)
	}
	sort.Strings(paths)
	return paths
}

func findPatch(patches []FilePatch, path string) (FilePatch, bool) {
	for _, patch := range patches {
		if patch.Path == path || patch.OldPath == path {
			return patch, true
		}
	}
	return FilePatch{}, false
}

func findingSeverities(findings []review.Finding) map[review.FindingID]review.Severity {
	out := make(map[review.FindingID]review.Severity, len(findings))
	for _, finding := range findings {
		out[finding.ID] = finding.Severity
	}
	return out
}

func failOnTriggered(findings []review.Finding, threshold *review.Severity) bool {
	if threshold == nil {
		return false
	}
	for _, finding := range findings {
		if finding.Severity.AtLeast(*threshold) {
			return true
		}
	}
	return false
}

func agentDefinitionsChanged(patches []FilePatch) bool {
	for _, patch := range patches {
		if strings.HasPrefix(patch.Path, ".codereview/agents/") || strings.HasPrefix(patch.OldPath, ".codereview/agents/") {
			return true
		}
	}
	return false
}

type llmRuntimeConfig struct {
	model  string
	effort string
}

func hasDryRunStageOverrides(req Request) bool {
	return strings.TrimSpace(req.SelectionModelOverride) != "" ||
		strings.TrimSpace(req.SelectionEffortOverride) != "" ||
		strings.TrimSpace(req.SelectionPromptInstructions) != "" ||
		strings.TrimSpace(req.ReviewerModelOverride) != "" ||
		strings.TrimSpace(req.ReviewerModelTierOverride) != "" ||
		strings.TrimSpace(req.ReviewerEffortOverride) != ""
}

func resolveSelectionRuntimeConfig(profile config.Profile, modelOverride, effortOverride string) (llmRuntimeConfig, error) {
	resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
		Profile:        profile,
		Stage:          stagemodel.StageSelection,
		Tier:           config.ModelTierMedium,
		ModelOverride:  modelOverride,
		EffortOverride: effortOverride,
		DefaultEffort:  string(modelprefs.EffortMedium),
	})
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return llmRuntimeConfig{model: resolved.Model, effort: resolved.Effort}, nil
}

func resolveThreadAnalysisRuntimeConfig(profile config.Profile) (llmRuntimeConfig, error) {
	resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
		Profile:       profile,
		Stage:         stagemodel.StageThreadAnalysis,
		Tier:          config.ModelTierMedium,
		DefaultEffort: string(modelprefs.EffortMedium),
	})
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return llmRuntimeConfig{model: resolved.Model, effort: resolved.Effort}, nil
}

func resolveSynthesisRuntimeConfig(req Request) (llmRuntimeConfig, error) {
	resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
		Profile:       req.Profile,
		Stage:         stagemodel.StageSynthesis,
		Tier:          config.ModelTierMedium,
		DefaultEffort: string(modelprefs.EffortMedium),
	})
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return llmRuntimeConfig{model: resolved.Model, effort: resolved.Effort}, nil
}

func resolveReviewerRuntimeConfig(req Request, agent agents.Agent) (llmRuntimeConfig, error) {
	if strings.TrimSpace(req.ReviewerModelOverride) != "" {
		resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
			Profile:        req.Profile,
			Stage:          stagemodel.StageReviewer,
			ModelOverride:  req.ReviewerModelOverride,
			EffortOverride: req.ReviewerEffortOverride,
			DefaultEffort:  agent.Effort,
		})
		if err != nil {
			return llmRuntimeConfig{}, err
		}
		return llmRuntimeConfig{model: resolved.Model, effort: resolved.Effort}, nil
	}
	resolved, err := resolveAgentModel(req.Profile, req.ReviewerModelTierOverride, agent)
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return applyStageRuntimeOverrides(req.ReviewerModelOverride, req.ReviewerEffortOverride, resolved.ResolvedModel, agent.Effort), nil
}

func validateReviewerFastMode(req Request, catalog agents.Catalog) error {
	if !req.ReviewerFast {
		return nil
	}
	spec, ok := config.FindLLMRuntimeSpec(req.Profile.LLM.Provider, req.Profile.LLM.Auth, req.Profile.LLM.Adapter)
	runtimeName := fmt.Sprintf("%s/%s/%s", req.Profile.LLM.Provider, req.Profile.LLM.Auth, req.Profile.LLM.Adapter)
	if !ok {
		return fmt.Errorf("pipeline: --fast is unsupported for runtime %s: runtime is not registered", runtimeName)
	}
	if len(spec.FastModeModels) == 0 {
		return fmt.Errorf("pipeline: --fast is unsupported for runtime %s: adapter has no fast-mode mechanism", runtimeName)
	}
	for _, agent := range catalog.Agents {
		runtimeConfig, err := resolveReviewerRuntimeConfig(req, agent)
		if err != nil {
			return err
		}
		if !spec.SupportsFastMode(runtimeConfig.model) {
			return fmt.Errorf("pipeline: --fast is unsupported for runtime %s: reviewer %q resolves to model %q; supported models: %s", runtimeName, agent.ID, runtimeConfig.model, strings.Join(spec.FastModeModels, ", "))
		}
	}
	return nil
}

func resolveAgentModel(profile config.Profile, baselineOverride string, agent agents.Agent) (reviewerRuntimeResolution, error) {
	if modelID := strings.TrimSpace(agent.ModelID); modelID != "" {
		resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
			Profile:       profile,
			Stage:         stagemodel.StageReviewer,
			ModelOverride: modelID,
			DefaultEffort: agent.Effort,
		})
		if err != nil {
			return reviewerRuntimeResolution{}, fmt.Errorf("pipeline: agent %s: %w", agent.ID, err)
		}
		return reviewerRuntimeResolution{
			Mode:          "exact_model",
			ResolvedModel: resolved.Model,
		}, nil
	}
	floorTier := config.ModelTier(strings.TrimSpace(agent.ModelTier))
	if !floorTier.Valid() {
		return reviewerRuntimeResolution{}, fmt.Errorf("pipeline: agent %s model_tier %q is invalid", agent.ID, agent.ModelTier)
	}
	baselineTier, err := resolveReviewerBaselineTier(profile, baselineOverride)
	if err != nil {
		return reviewerRuntimeResolution{}, fmt.Errorf("pipeline: agent %s: %w", agent.ID, err)
	}
	resolved, err := stagemodel.ResolveStageModel(stagemodel.Request{
		Profile:       profile,
		Stage:         stagemodel.StageReviewer,
		Tier:          baselineTier,
		FloorTier:     floorTier,
		DefaultEffort: agent.Effort,
	})
	if err != nil {
		return reviewerRuntimeResolution{}, fmt.Errorf("pipeline: agent %s: %w", agent.ID, err)
	}
	return reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      string(floorTier),
		BaselineTier:   string(baselineTier),
		EffectiveTier:  string(resolved.Tier),
		ResolvedModel:  resolved.Model,
		ModelMapSource: resolved.Source,
	}, nil
}

func resolveReviewerBaselineTier(profile config.Profile, override string) (config.ModelTier, error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		tier := config.ModelTier(trimmed)
		if !tier.Valid() {
			return "", fmt.Errorf("reviewer baseline model_tier %q is invalid; must be one of small, medium, large", override)
		}
		return tier, nil
	}
	tier := profile.LLM.ReviewerModelTier
	if tier == "" {
		return config.ModelTierSmall, nil
	}
	if !tier.Valid() {
		return "", fmt.Errorf("reviewer baseline model_tier %q is invalid; must be one of small, medium, large", tier)
	}
	return tier, nil
}

func applyStageRuntimeOverrides(modelOverride, effortOverride, model, effort string) llmRuntimeConfig {
	if override := strings.TrimSpace(modelOverride); override != "" {
		model = override
	}
	if override := strings.TrimSpace(effortOverride); override != "" {
		effort = override
	}
	return llmRuntimeConfig{model: model, effort: effort}
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

func sameIdentity(left, right gitprovider.Identity) bool {
	if strings.TrimSpace(left.ID) != "" && strings.TrimSpace(right.ID) != "" {
		return left.ID == right.ID
	}
	if strings.TrimSpace(left.Login) != "" && strings.TrimSpace(right.Login) != "" {
		return strings.EqualFold(left.Login, right.Login)
	}
	return false
}
