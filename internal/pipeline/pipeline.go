// Package pipeline orchestrates review pipeline phases without owning command IO.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pricing"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/sessionreuse"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
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
	DeleteRun(context.Context, string) error
	AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error)
	InsertSession(context.Context, ledger.Session) error
	GetSession(context.Context, string) (ledger.Session, error)
	InsertFinding(context.Context, ledger.Finding) error
	InsertPlannedAction(context.Context, ledger.PlannedAction) error
	CompleteRun(context.Context, string, ledger.Outcome, time.Time) error
}

// NamedSessionStore reads cross-run named LLM sessions.
type NamedSessionStore interface {
	GetNamedSession(context.Context, string) (ledger.NamedSession, error)
}

// LLMTaskProgress records task-aware LLM pipeline breadcrumbs without owning
// command IO details.
type LLMTaskProgress interface {
	StartLLMTask(LLMTaskProgressEvent) LLMTaskProgressSpan
	LoadLLMTask(LLMTaskProgressEvent, LLMTaskProgressResult)
}

// LLMTaskProgressSpan is one active LLM task breadcrumb.
type LLMTaskProgressSpan interface {
	End(error, LLMTaskProgressResult)
}

// LLMTaskProgressEvent describes one LLM task execution or reload.
type LLMTaskProgressEvent struct {
	TaskID          string
	Phase           string
	AgentID         string
	Model           string
	Effort          string
	LogPath         string
	ResumeSessionID string
	Source          string
}

// LLMTaskProgressResult describes the outcome of one task execution or reload.
type LLMTaskProgressResult struct {
	ProviderSessionID  string
	Status             string
	ValidationAttempts int
	Cached             bool
}

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
	ReviewBaseSHA               string
	ReviewHeadSHA               string

	FailOn              *review.Severity
	IncludeNits         bool
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
	PRRef         gitprovider.PRRef
	ProfileName   string
	Profile       config.Profile
	AgentDirs     []string
	ArtifactDir   string
	ReviewBaseSHA string
	ReviewHeadSHA string

	SelectionModelOverride      string
	SelectionEffortOverride     string
	SelectionPromptInstructions string
}

// ArtifactPaths contains per-run artifact paths.
type ArtifactPaths struct {
	Dir              string `json:"dir"`
	DiffPatch        string `json:"diff_patch"`
	SlicesDir        string `json:"slices_dir"`
	FindingsJSON     string `json:"findings_json"`
	RollupMarkdown   string `json:"rollup_markdown"`
	AgentSourcesJSON string `json:"agent_sources_json"`
	AgentLogsDir     string `json:"agent_logs_dir"`
	LLMTasksDir      string `json:"llm_tasks_dir"`
	DossierDir       string `json:"dossier_dir"`
}

// SlicePatch returns the artifact path for an agent/file diff slice.
func (p ArtifactPaths) SlicePatch(agentID, filePath string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("pipeline: agent ID is required")
	}
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("pipeline: file path is required")
	}
	return filepath.Join(p.SlicesDir, statepaths.Encode(agentID), statepaths.Encode(filePath)+".patch"), nil
}

// AgentLog returns the tailable LLM log path for an agent.
func (p ArtifactPaths) AgentLog(agentID string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("pipeline: agent ID is required")
	}
	return filepath.Join(p.AgentLogsDir, statepaths.Encode(agentID)+".jsonl"), nil
}

// LLMTaskDir returns the artifact directory for one durable LLM task.
func (p ArtifactPaths) LLMTaskDir(taskID string) (string, error) {
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("pipeline: LLM task ID is required")
	}
	return filepath.Join(p.LLMTasksDir, statepaths.Encode(taskID)), nil
}

// LLMTaskMetadata returns the metadata artifact path for one durable LLM task.
func (p ArtifactPaths) LLMTaskMetadata(taskID string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "metadata.json"), nil
}

// LLMTaskValidatedOutput returns the validated structured output path for one task.
func (p ArtifactPaths) LLMTaskValidatedOutput(taskID string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "validated-output.json"), nil
}

// LLMTaskRawAttempt returns the raw structured output path for a failed attempt.
func (p ArtifactPaths) LLMTaskRawAttempt(taskID, attempt string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	attempt = strings.TrimSpace(attempt)
	if attempt == "" {
		return "", fmt.Errorf("pipeline: LLM task attempt is required")
	}
	return filepath.Join(dir, statepaths.Encode(attempt)+".json"), nil
}

// DossierRawPath returns a raw dossier artifact path by file name.
func (p ArtifactPaths) DossierRawPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "raw"), name)
}

// DossierSummaryPath returns a summary dossier artifact path by file name.
func (p ArtifactPaths) DossierSummaryPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "summary"), name)
}

// DossierFinalPath returns a reviewer-facing dossier artifact path by file name.
func (p ArtifactPaths) DossierFinalPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "final"), name)
}

// DossierIndexPath returns the dossier index artifact path.
func (p ArtifactPaths) DossierIndexPath() string {
	return filepath.Join(p.DossierDir, "index.json")
}

func dossierChildPath(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("pipeline: dossier artifact name is required")
	}
	if strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator)) {
		return "", fmt.Errorf("pipeline: dossier artifact name must be a file name")
	}
	return filepath.Join(dir, name), nil
}

const llmTaskSchemaVersion = 1

type llmTaskStatus string

const (
	llmTaskStatusSucceeded      llmTaskStatus = "succeeded"
	llmTaskStatusFailedIsolated llmTaskStatus = "failed_isolated"
	llmTaskStatusFailedBlocking llmTaskStatus = "failed_blocking"
)

type llmTaskMetadata struct {
	SchemaVersion       int                      `json:"schema_version"`
	TaskID              string                   `json:"task_id"`
	Phase               string                   `json:"phase"`
	DependencyTaskIDs   []string                 `json:"dependency_task_ids,omitempty"`
	InputFingerprint    string                   `json:"input_fingerprint"`
	AgentID             string                   `json:"agent_id,omitempty"`
	Status              llmTaskStatus            `json:"status"`
	SessionRowID        string                   `json:"session_row_id,omitempty"`
	ProviderSessionID   string                   `json:"provider_session_id,omitempty"`
	Adapter             string                   `json:"adapter,omitempty"`
	Model               string                   `json:"model,omitempty"`
	Effort              string                   `json:"effort,omitempty"`
	LogPath             string                   `json:"log_path,omitempty"`
	ValidatedOutputPath string                   `json:"validated_output_path,omitempty"`
	Error               string                   `json:"error,omitempty"`
	Attempts            []llmTaskAttemptMetadata `json:"attempts,omitempty"`
}

type llmTaskAttemptMetadata struct {
	Attempt           string `json:"attempt"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	RawOutputPath     string `json:"raw_output_path,omitempty"`
	DecodeError       string `json:"decode_error,omitempty"`
}

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
}

// ReviewerFailure records an isolated reviewer LLM task failure that should not
// abort the whole run.
type ReviewerFailure struct {
	TaskID  string `json:"task_id"`
	AgentID string `json:"agent_id"`
	Error   string `json:"error"`
}

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

type sessionDraft struct {
	rowID                     string
	providerReportedSessionID string
	providerSessionID         string
	role                      ledger.SessionRole
	agentID                   *string
	adapter                   string
	model                     string
	effort                    string
	startedAt                 time.Time
	completedAt               time.Time
	response                  llm.Response
}

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
	AgentDirs        []string
	ReviewBaseSHA    string
	ReviewHeadSHA    string
	NoResolveThreads bool
	ResolvedPR       *reviewPRContext
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
	Profile                     config.Profile
	SelectionModelOverride      string
	SelectionEffortOverride     string
	SelectionPromptInstructions string
	ReviewPR                    gitprovider.PR
	Catalog                     agents.Catalog
	ParsedDiff                  ParsedDiff
	Threads                     []gitprovider.InlineThread
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
	if err := agents.RequireSafeProfileSources(req.Profile.AgentSources); err != nil {
		return SelectionResult{}, err
	}

	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:            req.PRRef,
		Profile:          req.Profile,
		AgentDirs:        req.AgentDirs,
		ReviewBaseSHA:    req.ReviewBaseSHA,
		ReviewHeadSHA:    req.ReviewHeadSHA,
		NoResolveThreads: false,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(req.ArtifactDir), nil
		},
	})
	if err != nil {
		return SelectionResult{}, err
	}

	result := prepared.selectionResult()
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
	if err := agents.RequireSafeProfileSources(req.Profile.AgentSources); err != nil {
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
	runID := opts.newRunID()
	if mode.live {
		runID = mode.run.RunID
	}
	reviewCtx, err := resolveReviewPRContext(ctx, opts.Provider, req.PRRef, req.ReviewBaseSHA, req.ReviewHeadSHA)
	if err != nil {
		return Result{}, err
	}
	if sameIdentity(reviewCtx.pr.Author, req.PostingIdentity) && req.Profile.ReviewerCredentials != nil && !req.AllowSelfReview {
		return Result{}, fmt.Errorf("pipeline: reviewer credentials resolve to PR author %q; pass --allow-self-review to continue", req.PostingIdentity.Login)
	}
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:            req.PRRef,
		Profile:          req.Profile,
		AgentDirs:        req.AgentDirs,
		ReviewBaseSHA:    req.ReviewBaseSHA,
		ReviewHeadSHA:    req.ReviewHeadSHA,
		NoResolveThreads: req.NoResolveThreads,
		ResolvedPR:       &reviewCtx,
		ResolveArtifacts: func(reviewPR gitprovider.PR) (ArtifactPaths, error) {
			if mode.live {
				return ArtifactPathsFromDir(mode.run.ArtifactPath), nil
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
		defer func() {
			if !completed {
				_ = opts.Store.CompleteRun(context.Background(), run.RunID, failureOutcome, opts.now())
			}
		}()
	}
	result.Run = run

	findingSession := map[review.FindingID]string{}

	if len(prepared.parsed.Patches) == 0 {
		if sessionName := strings.TrimSpace(req.SessionName); sessionName != "" {
			if !mode.live {
				return Result{}, fmt.Errorf("pipeline: named session %q requires live review", sessionName)
			}
			opts.emitWarning(fmt.Sprintf("session %q was not updated because no orchestrator session was produced", sessionName))
		}
		plan, err := opts.buildPlan(req, prepared.pr, mode.planPostMode, result.EffectiveCaps, reviewplan.Diff{}, nil, review.Rollup{}, nil, true, result.AgentDefsChanged, planRunInputs{})
		if err != nil {
			return Result{}, err
		}
		result.Plan = plan
	} else {
		runtimeConfig, err := resolveSelectionRuntimeConfig(req.Profile, req.SelectionModelOverride, req.SelectionEffortOverride)
		if err != nil {
			return Result{}, err
		}
		model := runtimeConfig.model
		namedSession, err := prepareNamedSession(ctx, opts, req, mode.live, model, now)
		if err != nil {
			return Result{}, err
		}

		selection, selectionSession, selectionLedgerSession, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
			RunID:                       run.RunID,
			Profile:                     req.Profile,
			SelectionModelOverride:      req.SelectionModelOverride,
			SelectionEffortOverride:     req.SelectionEffortOverride,
			SelectionPromptInstructions: req.SelectionPromptInstructions,
			ReviewPR:                    prepared.reviewPR,
			Catalog:                     prepared.catalog,
			ParsedDiff:                  prepared.parsed,
			Threads:                     prepared.threads,
			Artifacts:                   prepared.artifacts,
			ResumeSessionID:             namedSession.resumeID(),
			MaxAgents:                   maxAgents,
		})
		if err != nil {
			if errors.Is(err, errLLMTaskFailedBlocking) {
				failureOutcome = ledger.OutcomeIncomplete
			}
			return Result{}, err
		}
		result.Selection = selection
		result.Sessions = appendSessionIfPresent(result.Sessions, selectionLedgerSession)
		namedSession.recordSessionID(selectionSession)

		selectionTaskIDs := []string{orchestratorSelectionStage}
		findings, reviewerSessions, reviewerLedgerSessions, reviewerFindingSessions, reviewerFailures, err := runReviewers(ctx, opts, req, run.RunID, prepared.reviewPR, prepared.catalog, prepared.parsed, prepared.artifacts, selection, selectionTaskIDs, maxConcurrency)
		if err != nil {
			if errors.Is(err, errLLMTaskFailedBlocking) {
				failureOutcome = ledger.OutcomeIncomplete
			}
			return Result{}, err
		}
		result.Findings = findings
		result.ReviewerFailures = reviewerFailures
		result.Sessions = appendSessionsIfPresent(result.Sessions, reviewerLedgerSessions...)
		for id, rowID := range reviewerFindingSessions {
			findingSession[id] = rowID
		}

		rollupRuntimeConfig, err := resolveSynthesisRuntimeConfig(req)
		if err != nil {
			return Result{}, err
		}
		rollupModel, rollupEffort := rollupRuntimeConfig.model, rollupRuntimeConfig.effort

		rollupPrompt, err := buildRollupPrompt(prepared.reviewPR, findings, reviewerFailures)
		if err != nil {
			return Result{}, err
		}
		if err := opts.checkPromptBudget("rollup", "", rollupModel, "", rollupPrompt); err != nil {
			return Result{}, err
		}
		rollupLog, err := prepared.artifacts.AgentLog(orchestratorRollupStage)
		if err != nil {
			return Result{}, err
		}
		reviewerDeps := reviewerTaskIDs(selection.SelectedAgents)
		rollupDeps := append([]string(nil), selectionTaskIDs...)
		rollupDeps = append(rollupDeps, reviewerDeps...)
		rollup, rollupSession, rollupLedgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
			runID:             run.RunID,
			taskID:            orchestratorRollupStage,
			phase:             "rollup",
			dependencyTaskIDs: rollupDeps,
			inputFingerprint:  llmTaskFingerprint(opts.Adapter.Name(), orchestratorRollupStage, "rollup", rollupModel, rollupEffort, rollupPrompt, rollupDeps),
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
			if errors.Is(err, errLLMTaskFailedBlocking) {
				failureOutcome = ledger.OutcomeIncomplete
			}
			return Result{}, err
		}
		result.Rollup = rollup
		result.Sessions = appendSessionIfPresent(result.Sessions, rollupLedgerSession)
		result.NamedSessionCandidate = namedSession.buildCandidate(rollupSession, opts.now())
		if namedSession.enabled && result.NamedSessionCandidate == nil {
			opts.emitWarning(fmt.Sprintf("session %q was not updated because no orchestrator session was produced", namedSession.active.Name))
		}

		plan, err := opts.buildPlan(req, prepared.reviewPR, mode.planPostMode, result.EffectiveCaps, prepared.parsed.PlanDiff, findings, rollup, selection.ThreadActions, false, result.AgentDefsChanged, planRunInputs{
			hasRun:           true,
			selection:        selectionSession,
			reviewers:        reviewerSessions,
			rollup:           rollupSession,
			selectedAgents:   selection.SelectedAgents,
			findingSessions:  findingSession,
			reviewerFailures: reviewerFailures,
			startedAt:        now,
		})
		if err != nil {
			return Result{}, err
		}
		result.Plan = plan
	}
	for _, finding := range result.Plan.AnchoredFindings {
		rowID, err := sessionRowIDForFinding(finding, findingSession)
		if err != nil {
			return Result{}, err
		}
		ledgerFinding := ledgerFinding(run.RunID, rowID, finding)
		if err := opts.Store.InsertFinding(ctx, ledgerFinding); err != nil {
			return Result{}, err
		}
	}
	for _, action := range result.Plan.Actions {
		planned, err := plannedAction(run.RunID, action)
		if err != nil {
			return Result{}, err
		}
		if err := opts.Store.InsertPlannedAction(ctx, planned); err != nil {
			return Result{}, err
		}
		result.PlannedActions = append(result.PlannedActions, planned)
	}
	if err := writeArtifacts(prepared.artifacts, prepared.rawDiff, prepared.parsed.Patches, result.Catalog, result.Selection, result.Findings, result.Plan.RollupMarkdown, reviewerRuntimeArtifact(req, prepared.catalog, result.Selection)); err != nil {
		return Result{}, err
	}
	if !mode.live {
		if err := opts.Store.CompleteRun(ctx, run.RunID, mode.completeAs, opts.now()); err != nil {
			return Result{}, err
		}
	}
	completed = true
	result.FailOnTriggered = failOnTriggered(result.Findings, req.FailOn)
	return result, nil
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
		ProviderReportedSessionID: draft.providerReportedSessionID,
		ProviderSessionID:         draft.providerSessionID,
		Adapter:                   draft.adapter,
		Model:                     draft.model,
		Effort:                    draft.effort,
		StartedAt:                 draft.startedAt,
		CompletedAt:               draft.completedAt,
		Response:                  draft.response,
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
	var reviews []gitprovider.Review
	var issueComments []gitprovider.IssueComment
	if !reviewCtx.pinnedReview {
		threads, err = opts.Provider.ListInlineThreads(ctx, req.PRRef)
		if err != nil {
			return preparedSelectionContext{}, err
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
	catalog, err := agents.Load(ctx, agents.LoadOptions{
		ProfileDirs:               append([]string(nil), req.Profile.AgentSources...),
		Repo:                      &agents.RepoSource{Reader: opts.Provider, Ref: req.PRRef, PR: pr},
		FlagDirs:                  append([]string(nil), req.AgentDirs...),
		RequireSafeProfileSources: true,
	})
	if err != nil {
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
	if err := writeDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:             pr,
		ReviewPR:              reviewPR,
		PinnedReview:          reviewCtx.pinnedReview,
		ChangedFiles:          parsed.Patches,
		Threads:               threads,
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

	selectionPrompt, err := buildSelectionPrompt(req.ReviewPR, req.Catalog, req.ParsedDiff.Patches, req.Threads, req.MaxAgents, req.SelectionPromptInstructions)
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
			KnownThreads: knownThreads(req.Threads),
		})
	}
	if strings.TrimSpace(req.RunID) != "" {
		selection, selectionSession, ledgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
			runID:            req.RunID,
			taskID:           orchestratorSelectionStage,
			phase:            "selection",
			inputFingerprint: llmTaskFingerprint(opts.Adapter.Name(), orchestratorSelectionStage, "selection", model, effort, selectionPrompt, nil),
			artifacts:        req.Artifacts,
			role:             ledger.SessionRoleOrchestrator,
			model:            model,
			effort:           effort,
			logPath:          selectionLog,
			prompt:           selectionPrompt,
			resumeSessionID:  req.ResumeSessionID,
		}, decode)
		if err != nil {
			return llm.Selection{}, selectionSession, ledgerSession, err
		}
		selection = opts.capSelectionAgents(selection, req.MaxAgents)
		return selection, selectionSession, ledgerSession, nil
	}
	selection, selectionSession, err := runStructuredResume(ctx, opts, ledger.SessionRoleOrchestrator, nil, model, effort, selectionLog, selectionPrompt, req.ResumeSessionID, decode)
	if err != nil {
		return llm.Selection{}, selectionSession, ledger.Session{}, err
	}
	selection = opts.capSelectionAgents(selection, req.MaxAgents)
	return selection, selectionSession, ledger.Session{}, nil
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

func runReviewers(ctx context.Context, opts Options, req Request, runID string, pr gitprovider.PR, catalog agents.Catalog, parsed ParsedDiff, artifacts ArtifactPaths, selection llm.Selection, dependencyTaskIDs []string, maxConcurrency int) ([]review.Finding, []sessionDraft, []ledger.Session, map[review.FindingID]string, []ReviewerFailure, error) {
	type job struct {
		selected llm.SelectedAgent
		agent    agents.Agent
	}
	var jobs []job
	for _, selected := range selection.SelectedAgents {
		agent, ok := catalog.Find(selected.AgentID)
		if !ok {
			return nil, nil, nil, nil, nil, fmt.Errorf("pipeline: selected agent %q not found", selected.AgentID)
		}
		jobs = append(jobs, job{selected: selected, agent: agent})
	}
	if len(jobs) == 0 {
		return nil, nil, nil, nil, nil, nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].agent.ID < jobs[j].agent.ID })

	reviewCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var allFindings []review.Finding
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
			findings, session, ledgerSession, failure, err := runReviewer(reviewCtx, opts, req, runID, pr, parsed, artifacts, current.selected, current.agent, dependencyTaskIDs)
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
			for _, finding := range findings {
				allFindings = append(allFindings, finding)
				findingSessions[finding.ID] = session.rowID
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, nil, nil, nil, firstErr
	}
	sort.Slice(allFindings, func(i, j int) bool { return allFindings[i].ID < allFindings[j].ID })
	sort.Slice(failures, func(i, j int) bool { return failures[i].AgentID < failures[j].AgentID })
	return allFindings, sessions, ledgerSessions, findingSessions, failures, nil
}

func runReviewer(ctx context.Context, opts Options, req Request, runID string, pr gitprovider.PR, parsed ParsedDiff, artifacts ArtifactPaths, selected llm.SelectedAgent, agent agents.Agent, dependencyTaskIDs []string) ([]review.Finding, sessionDraft, ledger.Session, *ReviewerFailure, error) {
	runtimeConfig, err := resolveReviewerRuntimeConfig(req, agent)
	if err != nil {
		return nil, sessionDraft{}, ledger.Session{}, nil, err
	}
	model, effort := runtimeConfig.model, runtimeConfig.effort
	prompt, err := buildReviewerPrompt(ctx, opts, req, pr, parsed, selected, agent, model)
	if err != nil {
		return nil, sessionDraft{}, ledger.Session{}, nil, err
	}
	if err := opts.checkPromptBudget("reviewer", agent.ID, model, strings.Join(selected.Files, ","), prompt); err != nil {
		return nil, sessionDraft{}, ledger.Session{}, nil, err
	}
	logPath, err := artifacts.AgentLog(agent.ID)
	if err != nil {
		return nil, sessionDraft{}, ledger.Session{}, nil, err
	}
	agentID := agent.ID
	taskID := reviewerTaskID(agent.ID)
	findings, session, ledgerSession, err := runStructuredTask(ctx, opts, llmTaskSpec{
		runID:             runID,
		taskID:            taskID,
		phase:             "reviewer",
		dependencyTaskIDs: dependencyTaskIDs,
		inputFingerprint:  llmTaskFingerprint(opts.Adapter.Name(), taskID, "reviewer", model, effort, prompt, dependencyTaskIDs),
		artifacts:         artifacts,
		role:              ledger.SessionRoleReviewer,
		agentID:           &agentID,
		model:             model,
		effort:            effort,
		logPath:           logPath,
		prompt:            prompt,
		llmFailureStatus:  llmTaskStatusFailedIsolated,
	}, func(data []byte) (llm.Findings, error) {
		return llm.DecodeFindings(data, llm.FindingsOptions{
			KnownAgents:  map[string]bool{agent.ID: true},
			ChangedFiles: changedFiles(parsed.Patches),
			NewFindingID: opts.newFindingID,
		})
	})
	if err != nil {
		var taskErr *llmTaskError
		if errors.As(err, &taskErr) && taskErr.status == llmTaskStatusFailedIsolated {
			return nil, session, ledgerSession, &ReviewerFailure{
				TaskID:  taskID,
				AgentID: agent.ID,
				Error:   sanitizeTaskErrorForMarkdown(err),
			}, nil
		}
		return nil, sessionDraft{}, ledger.Session{}, nil, err
	}
	return findings.Findings, session, ledgerSession, nil, nil
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

func runStructuredResume[T any](ctx context.Context, opts Options, role ledger.SessionRole, agentID *string, model, effort, logPath, prompt, resumeSessionID string, decode llm.Decoder[T]) (T, sessionDraft, error) {
	started := opts.now()
	result, err := llm.RunStructuredWithSessionResume(ctx, opts.Adapter, resumeSessionID, llm.Request{
		Model:   model,
		Effort:  effort,
		Prompt:  prompt,
		LogPath: logPath,
	}, decode)
	completed := opts.now()
	draft := sessionDraft{
		rowID:                     opts.newSessionRowID(),
		providerReportedSessionID: result.SessionID,
		providerSessionID:         result.SessionID,
		role:                      role,
		agentID:                   agentID,
		adapter:                   opts.Adapter.Name(),
		model:                     model,
		effort:                    effort,
		startedAt:                 started,
		completedAt:               completed,
		response:                  result.Response,
	}
	if strings.TrimSpace(draft.providerSessionID) == "" {
		draft.providerSessionID = draft.rowID
	}
	if strings.TrimSpace(draft.model) == "" {
		draft.model = "default"
	}
	return result.Value, draft, err
}

type llmTaskSpec struct {
	runID             string
	taskID            string
	phase             string
	dependencyTaskIDs []string
	inputFingerprint  string
	artifacts         ArtifactPaths
	role              ledger.SessionRole
	agentID           *string
	model             string
	effort            string
	logPath           string
	prompt            string
	resumeSessionID   string
	llmFailureStatus  llmTaskStatus
}

func runStructuredTask[T any](ctx context.Context, opts Options, spec llmTaskSpec, decode llm.Decoder[T]) (T, sessionDraft, ledger.Session, error) {
	if loaded, draft, session, ok, err := loadStructuredTask(ctx, opts, spec, decode); err != nil || ok {
		return loaded, draft, session, err
	}
	resumeSessionID := spec.resumeSessionID
	if meta, ok, err := readLLMTaskMetadata(spec.artifacts, spec.taskID); err != nil {
		var zero T
		return zero, sessionDraft{}, ledger.Session{}, err
	} else if ok && meta.Status == llmTaskStatusFailedBlocking {
		if taskSessionID := taskResumeSessionID(meta); strings.TrimSpace(taskSessionID) != "" {
			resumeSessionID = taskSessionID
		}
	}
	progressEvent := newLLMTaskProgressEvent(spec, resumeSessionID)
	progressSpan := startLLMTaskProgress(opts, progressEvent)

	started := opts.now()
	result, err := llm.RunStructuredWithSessionResume(ctx, opts.Adapter, resumeSessionID, llm.Request{
		Model:   spec.model,
		Effort:  spec.effort,
		Prompt:  spec.prompt,
		LogPath: spec.logPath,
	}, decode)
	completed := opts.now()
	draft := sessionDraft{
		rowID:                     opts.newSessionRowID(),
		providerReportedSessionID: result.SessionID,
		providerSessionID:         result.SessionID,
		role:                      spec.role,
		agentID:                   spec.agentID,
		adapter:                   opts.Adapter.Name(),
		model:                     spec.model,
		effort:                    spec.effort,
		startedAt:                 started,
		completedAt:               completed,
		response:                  result.Response,
	}
	if strings.TrimSpace(draft.providerSessionID) == "" && err == nil {
		draft.providerSessionID = draft.rowID
	}
	if strings.TrimSpace(draft.model) == "" {
		draft.model = "default"
	}

	meta := baseLLMTaskMetadata(opts, spec, draft)
	var session ledger.Session
	if strings.TrimSpace(draft.providerSessionID) != "" {
		session = draft.toLedger(spec.runID)
		if err := opts.Store.InsertSession(ctx, session); err != nil {
			meta.Status = llmTaskStatusFailedBlocking
			meta.ProviderSessionID = draft.providerSessionID
			var zero T
			endLLMTaskProgress(progressSpan, err, llmTaskProgressResult(meta, result, false))
			return zero, sessionDraft{}, ledger.Session{}, err
		}
	}

	if err == nil {
		meta.Status = llmTaskStatusSucceeded
		meta.SessionRowID = session.SessionRowID
		meta.ProviderSessionID = session.ProviderSessionID
		if writeErr := writeLLMTaskSuccess(spec.artifacts, &meta, result.Response.StructuredOutput); writeErr != nil {
			var zero T
			endLLMTaskProgress(progressSpan, writeErr, llmTaskProgressResult(meta, result, false))
			return zero, sessionDraft{}, ledger.Session{}, writeErr
		}
		endLLMTaskProgress(progressSpan, nil, llmTaskProgressResult(meta, result, false))
		return result.Value, draft, session, nil
	}

	meta.Error = sanitizeTaskErrorForMarkdown(err)
	meta.Status = llmTaskFailureStatus(ctx, spec, err, result.SessionID, len(result.ValidationAttempts) > 0)
	meta.SessionRowID = session.SessionRowID
	meta.ProviderSessionID = session.ProviderSessionID
	if writeErr := writeLLMTaskFailure(spec.artifacts, &meta, result.ValidationAttempts); writeErr != nil {
		var zero T
		endLLMTaskProgress(progressSpan, writeErr, llmTaskProgressResult(meta, result, false))
		return zero, draft, session, writeErr
	}
	var zero T
	endLLMTaskProgress(progressSpan, err, llmTaskProgressResult(meta, result, false))
	return zero, draft, session, &llmTaskError{status: meta.Status, err: err}
}

func llmTaskFailureStatus(ctx context.Context, spec llmTaskSpec, err error, providerSessionID string, hasValidationAttempt bool) llmTaskStatus {
	supportsIsolation := spec.llmFailureStatus != ""
	callerContextActive := ctx.Err() == nil && !isContextError(err)
	hasTaskExecutionEvidence := errors.Is(err, llm.ErrStructuredOutputInvalidAfterRetry) || strings.TrimSpace(providerSessionID) != "" || hasValidationAttempt
	if supportsIsolation && callerContextActive && hasTaskExecutionEvidence {
		return spec.llmFailureStatus
	}
	return llmTaskStatusFailedBlocking
}

func loadStructuredTask[T any](ctx context.Context, opts Options, spec llmTaskSpec, decode llm.Decoder[T]) (T, sessionDraft, ledger.Session, bool, error) {
	var zero T
	meta, ok, err := readLLMTaskMetadata(spec.artifacts, spec.taskID)
	if err != nil || !ok {
		return zero, sessionDraft{}, ledger.Session{}, ok, err
	}
	if err := validateLLMTaskMetadata(meta, spec, llmTaskAdapterName(opts.Adapter)); err != nil {
		return zero, sessionDraft{}, ledger.Session{}, true, err
	}
	switch meta.Status {
	case llmTaskStatusSucceeded:
		outputPath, err := spec.artifacts.LLMTaskValidatedOutput(spec.taskID)
		if err != nil {
			return zero, sessionDraft{}, ledger.Session{}, true, err
		}
		if strings.TrimSpace(meta.ValidatedOutputPath) != "" {
			outputPath, err = validateLLMTaskPayloadPath(spec.artifacts, spec.taskID, "validated_output_path", meta.ValidatedOutputPath)
			if err != nil {
				return zero, sessionDraft{}, ledger.Session{}, true, err
			}
		}
		data, err := os.ReadFile(outputPath) // #nosec G304 -- validated task output path is scoped to run artifacts.
		if err != nil {
			return zero, sessionDraft{}, ledger.Session{}, true, fmt.Errorf("pipeline: read LLM task %q output: %w", spec.taskID, err)
		}
		value, err := decode(data)
		if err != nil {
			return zero, sessionDraft{}, ledger.Session{}, true, fmt.Errorf("pipeline: decode stored LLM task %q output: %w", spec.taskID, err)
		}
		session, err := loadTaskSession(ctx, opts, spec.runID, meta)
		if err != nil {
			return zero, sessionDraft{}, ledger.Session{}, true, err
		}
		loadLLMTaskProgress(opts, newLLMTaskProgressEvent(spec, taskResumeSessionID(meta)), llmTaskProgressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true))
		return value, sessionDraftFromLedger(session), session, true, nil
	case llmTaskStatusFailedIsolated:
		if spec.llmFailureStatus != llmTaskStatusFailedIsolated {
			return zero, sessionDraft{}, ledger.Session{}, true, fmt.Errorf("pipeline: LLM task %q has isolated failure status outside reviewer phase", spec.taskID)
		}
		session, draft, err := loadOptionalTaskSession(ctx, opts, spec.runID, meta)
		if err != nil {
			return zero, sessionDraft{}, ledger.Session{}, true, err
		}
		loadLLMTaskProgress(opts, newLLMTaskProgressEvent(spec, taskResumeSessionID(meta)), llmTaskProgressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true))
		return zero, draft, session, true, &llmTaskError{status: llmTaskStatusFailedIsolated, err: errors.New(taskErrorText(meta))}
	case llmTaskStatusFailedBlocking:
		return zero, sessionDraft{}, ledger.Session{}, false, nil
	default:
		return zero, sessionDraft{}, ledger.Session{}, true, fmt.Errorf("pipeline: LLM task %q has unknown status %q", spec.taskID, meta.Status)
	}
}

func validateLLMTaskPayloadPath(paths ArtifactPaths, taskID, field, recordedPath string) (string, error) {
	recordedPath = strings.TrimSpace(recordedPath)
	if recordedPath == "" {
		return "", fmt.Errorf("pipeline: LLM task %q %s is empty", taskID, field)
	}
	taskDir, err := paths.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	absDir, err := filepath.Abs(taskDir)
	if err != nil {
		return "", fmt.Errorf("pipeline: resolve LLM task %q directory: %w", taskID, err)
	}
	absPath, err := filepath.Abs(recordedPath)
	if err != nil {
		return "", fmt.Errorf("pipeline: resolve LLM task %q %s: %w", taskID, field, err)
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return "", fmt.Errorf("pipeline: compare LLM task %q %s: %w", taskID, field, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("pipeline: LLM task %q %s points outside the task artifact directory; pass --rerun to start a fresh review", taskID, field)
	}
	return absPath, nil
}

func validateLLMTaskMetadata(meta llmTaskMetadata, spec llmTaskSpec, adapter string) error {
	if meta.SchemaVersion != llmTaskSchemaVersion {
		return fmt.Errorf("pipeline: LLM task %q schema version = %d, want %d", spec.taskID, meta.SchemaVersion, llmTaskSchemaVersion)
	}
	if meta.TaskID != spec.taskID {
		return fmt.Errorf("pipeline: LLM task metadata ID %q does not match %q", meta.TaskID, spec.taskID)
	}
	if meta.Phase != spec.phase {
		return fmt.Errorf("pipeline: LLM task %q phase = %q, want %q", spec.taskID, meta.Phase, spec.phase)
	}
	if strings.TrimSpace(adapter) != "" && meta.Adapter != adapter {
		return fmt.Errorf("pipeline: LLM task %q adapter = %q, want %q; pass --rerun to start a fresh review", spec.taskID, meta.Adapter, adapter)
	}
	fingerprint := strings.TrimSpace(spec.inputFingerprint)
	if fingerprint == "" {
		fingerprint = llmTaskFingerprint(adapter, spec.taskID, spec.phase, spec.model, spec.effort, spec.prompt, spec.dependencyTaskIDs)
	}
	if meta.InputFingerprint != fingerprint {
		return fmt.Errorf("pipeline: LLM task %q input fingerprint changed; pass --rerun to start a fresh review", spec.taskID)
	}
	return nil
}

func readLLMTaskMetadata(paths ArtifactPaths, taskID string) (llmTaskMetadata, bool, error) {
	path, err := paths.LLMTaskMetadata(taskID)
	if err != nil {
		return llmTaskMetadata{}, false, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- metadata path is derived from run artifact paths.
	if errors.Is(err, os.ErrNotExist) {
		return llmTaskMetadata{}, false, nil
	}
	if err != nil {
		return llmTaskMetadata{}, false, fmt.Errorf("pipeline: read LLM task %q metadata: %w", taskID, err)
	}
	var meta llmTaskMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return llmTaskMetadata{}, false, fmt.Errorf("pipeline: decode LLM task %q metadata: %w", taskID, err)
	}
	return meta, true, nil
}

func loadTaskSession(ctx context.Context, opts Options, runID string, meta llmTaskMetadata) (ledger.Session, error) {
	if strings.TrimSpace(meta.SessionRowID) == "" {
		return ledger.Session{}, fmt.Errorf("pipeline: LLM task %q is missing session row id", meta.TaskID)
	}
	session, err := opts.Store.GetSession(ctx, meta.SessionRowID)
	if err != nil {
		return ledger.Session{}, fmt.Errorf("pipeline: load LLM task %q session %q: %w", meta.TaskID, meta.SessionRowID, err)
	}
	if session.RunID != runID {
		return ledger.Session{}, fmt.Errorf("pipeline: LLM task %q session belongs to run %q, want %q", meta.TaskID, session.RunID, runID)
	}
	return session, nil
}

func loadOptionalTaskSession(ctx context.Context, opts Options, runID string, meta llmTaskMetadata) (ledger.Session, sessionDraft, error) {
	if strings.TrimSpace(meta.SessionRowID) == "" {
		return ledger.Session{}, sessionDraft{}, nil
	}
	session, err := loadTaskSession(ctx, opts, runID, meta)
	if err != nil {
		return ledger.Session{}, sessionDraft{}, err
	}
	return session, sessionDraftFromLedger(session), nil
}

func sessionDraftFromLedger(session ledger.Session) sessionDraft {
	completedAt := session.StartedAt
	if session.CompletedAt != nil {
		completedAt = *session.CompletedAt
	}
	draft := sessionDraft{
		rowID:                     session.SessionRowID,
		providerReportedSessionID: session.ProviderSessionID,
		providerSessionID:         session.ProviderSessionID,
		role:                      session.Role,
		agentID:                   session.AgentID,
		adapter:                   session.Adapter,
		model:                     session.Model,
		startedAt:                 session.StartedAt,
		completedAt:               completedAt,
		response: llm.Response{
			Usage: llm.Usage{
				TokensIn:    int64PtrToInt(session.TokensIn),
				TokensOut:   int64PtrToInt(session.TokensOut),
				CacheRead:   int64PtrToInt(session.CacheRead),
				CacheCreate: int64PtrToInt(session.CacheCreate),
				CostUSD:     session.CostUSD,
			},
		},
	}
	if session.DurationMS != nil {
		draft.response.DurationMS = *session.DurationMS
	}
	if session.Effort != nil {
		draft.effort = *session.Effort
	}
	return draft
}

func taskResumeSessionID(meta llmTaskMetadata) string {
	if strings.TrimSpace(meta.ProviderSessionID) != "" {
		return meta.ProviderSessionID
	}
	for i := len(meta.Attempts) - 1; i >= 0; i-- {
		if strings.TrimSpace(meta.Attempts[i].ProviderSessionID) != "" {
			return meta.Attempts[i].ProviderSessionID
		}
	}
	return ""
}

func taskErrorText(meta llmTaskMetadata) string {
	if strings.TrimSpace(meta.Error) != "" {
		return meta.Error
	}
	return fmt.Sprintf("LLM task %q failed", meta.TaskID)
}

func newLLMTaskProgressEvent(spec llmTaskSpec, resumeSessionID string) LLMTaskProgressEvent {
	agentID := ""
	if spec.agentID != nil {
		agentID = *spec.agentID
	}
	source := "execute"
	if strings.TrimSpace(resumeSessionID) != "" {
		source = "resume"
	}
	return LLMTaskProgressEvent{
		TaskID:          spec.taskID,
		Phase:           spec.phase,
		AgentID:         agentID,
		Model:           spec.model,
		Effort:          spec.effort,
		LogPath:         spec.logPath,
		ResumeSessionID: resumeSessionID,
		Source:          source,
	}
}

func llmTaskProgressResult(meta llmTaskMetadata, result any, cached bool) LLMTaskProgressResult {
	out := LLMTaskProgressResult{
		ProviderSessionID: meta.ProviderSessionID,
		Status:            string(meta.Status),
		Cached:            cached,
	}
	switch value := result.(type) {
	case llm.StructuredResult[llm.Selection]:
		out.ValidationAttempts = len(value.ValidationAttempts)
		if out.ProviderSessionID == "" {
			out.ProviderSessionID = value.SessionID
		}
	case llm.StructuredResult[llm.Findings]:
		out.ValidationAttempts = len(value.ValidationAttempts)
		if out.ProviderSessionID == "" {
			out.ProviderSessionID = value.SessionID
		}
	case llm.StructuredResult[review.Rollup]:
		out.ValidationAttempts = len(value.ValidationAttempts)
		if out.ProviderSessionID == "" {
			out.ProviderSessionID = value.SessionID
		}
	}
	return out
}

func startLLMTaskProgress(opts Options, event LLMTaskProgressEvent) LLMTaskProgressSpan {
	if opts.TaskProgress == nil {
		return nil
	}
	return opts.TaskProgress.StartLLMTask(event)
}

func endLLMTaskProgress(span LLMTaskProgressSpan, err error, result LLMTaskProgressResult) {
	if span != nil {
		span.End(err, result)
	}
}

func loadLLMTaskProgress(opts Options, event LLMTaskProgressEvent, result LLMTaskProgressResult) {
	if opts.TaskProgress == nil {
		return
	}
	opts.TaskProgress.LoadLLMTask(event, result)
}

func baseLLMTaskMetadata(opts Options, spec llmTaskSpec, draft sessionDraft) llmTaskMetadata {
	agentID := ""
	if spec.agentID != nil {
		agentID = *spec.agentID
	}
	fingerprint := strings.TrimSpace(spec.inputFingerprint)
	if fingerprint == "" {
		fingerprint = llmTaskFingerprint(llmTaskAdapterName(opts.Adapter), spec.taskID, spec.phase, spec.model, spec.effort, spec.prompt, spec.dependencyTaskIDs)
	}
	return llmTaskMetadata{
		SchemaVersion:     llmTaskSchemaVersion,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		DependencyTaskIDs: append([]string(nil), spec.dependencyTaskIDs...),
		InputFingerprint:  fingerprint,
		AgentID:           agentID,
		SessionRowID:      draft.rowID,
		ProviderSessionID: draft.providerSessionID,
		Adapter:           opts.Adapter.Name(),
		Model:             draft.model,
		Effort:            draft.effort,
		LogPath:           spec.logPath,
	}
}

func llmTaskAdapterName(adapter llm.Adapter) string {
	if adapter == nil {
		return ""
	}
	return adapter.Name()
}

func llmTaskFingerprint(adapter, taskID, phase, model, effort, prompt string, deps []string) string {
	hash := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("schema=%d", llmTaskSchemaVersion),
		"adapter=" + adapter,
		"task=" + taskID,
		"phase=" + phase,
		"model=" + model,
		"effort=" + effort,
		"prompt=" + prompt,
	} {
		_, _ = io.WriteString(hash, part)
		_, _ = io.WriteString(hash, "\n")
	}
	for _, dep := range deps {
		_, _ = io.WriteString(hash, "dep="+dep+"\n")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeLLMTaskSuccess(paths ArtifactPaths, meta *llmTaskMetadata, output []byte) error {
	outputPath, err := paths.LLMTaskValidatedOutput(meta.TaskID)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(outputPath, append(append([]byte(nil), output...), '\n')); err != nil {
		return err
	}
	meta.ValidatedOutputPath = outputPath
	return writeLLMTaskMetadata(paths, *meta)
}

func writeLLMTaskFailure(paths ArtifactPaths, meta *llmTaskMetadata, attempts []llm.StructuredValidationAttempt) error {
	for _, attempt := range attempts {
		attemptMeta := llmTaskAttemptMetadata{
			Attempt:           attempt.Label,
			ProviderSessionID: attempt.SessionID,
			DecodeError:       sanitizeTaskErrorForMarkdown(attempt.DecodeError),
		}
		if len(attempt.Response.StructuredOutput) > 0 {
			rawPath, err := paths.LLMTaskRawAttempt(meta.TaskID, attempt.Label)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(rawPath, append(append([]byte(nil), attempt.Response.StructuredOutput...), '\n')); err != nil {
				return err
			}
			attemptMeta.RawOutputPath = rawPath
		}
		meta.Attempts = append(meta.Attempts, attemptMeta)
	}
	return writeLLMTaskMetadata(paths, *meta)
}

func writeLLMTaskMetadata(paths ArtifactPaths, meta llmTaskMetadata) error {
	path, err := paths.LLMTaskMetadata(meta.TaskID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	name := strings.TrimSpace(req.SessionName)
	if name == "" {
		return namedSessionState{}, nil
	}
	if !live {
		return namedSessionState{}, fmt.Errorf("pipeline: named session %q requires live review", name)
	}
	if opts.NamedSessions == nil {
		return namedSessionState{}, fmt.Errorf("pipeline: named session store is required")
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
			return namedSessionState{}, err
		}
		if check.Warning != "" {
			opts.emitWarning(check.Warning)
		}
		state.stored = &stored
		state.createdAt = stored.CreatedAt
		if state.supportsResume {
			state.currentProviderSessionID = stored.ProviderSessionID
		}
	}

	if !state.supportsResume {
		opts.emitWarning(fmt.Sprintf("session %q adapter %q does not support resume; starting fresh", active.Name, opts.Adapter.Name()))
	}
	return state, nil
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
	if strings.TrimSpace(draft.providerReportedSessionID) != "" {
		s.currentProviderSessionID = draft.providerReportedSessionID
	}
}

func (s *namedSessionState) buildCandidate(draft sessionDraft, lastUsedAt time.Time) *ledger.NamedSession {
	if s == nil || !s.enabled {
		return nil
	}
	providerSessionID := strings.TrimSpace(draft.providerReportedSessionID)
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
		CreatedAt:         s.createdAt,
		LastUsedAt:        lastUsedAt,
	}
}

func (d sessionDraft) toLedger(runID string) ledger.Session {
	completed := d.completedAt
	duration := d.response.DurationMS
	session := ledger.Session{
		SessionRowID:      d.rowID,
		RunID:             runID,
		ProviderSessionID: d.providerSessionID,
		Role:              d.role,
		AgentID:           d.agentID,
		Adapter:           d.adapter,
		Model:             d.model,
		StartedAt:         d.startedAt,
		CompletedAt:       &completed,
		DurationMS:        &duration,
		TokensIn:          intPtrToInt64(d.response.Usage.TokensIn),
		TokensOut:         intPtrToInt64(d.response.Usage.TokensOut),
		CacheRead:         intPtrToInt64(d.response.Usage.CacheRead),
		CacheCreate:       intPtrToInt64(d.response.Usage.CacheCreate),
		CostUSD:           d.response.Usage.CostUSD,
	}
	if strings.TrimSpace(d.effort) != "" {
		session.Effort = &d.effort
	}
	return session
}

// planRunInputs carries the session telemetry buildPlan turns into the
// rollup's RunSummary and finding attribution.
type planRunInputs struct {
	hasRun           bool
	selection        sessionDraft
	reviewers        []sessionDraft
	rollup           sessionDraft
	selectedAgents   []llm.SelectedAgent
	findingSessions  map[review.FindingID]string
	reviewerFailures []ReviewerFailure
	startedAt        time.Time
}

func (opts Options) buildRunSummary(req Request, inputs planRunInputs) (reviewplan.RunSummary, map[review.FindingID]string) {
	if !inputs.hasRun {
		return reviewplan.RunSummary{}, nil
	}
	reviewerByAgent := map[string]sessionDraft{}
	agentByRow := map[string]string{}
	for _, draft := range inputs.reviewers {
		if draft.agentID == nil {
			continue
		}
		reviewerByAgent[*draft.agentID] = draft
		agentByRow[draft.rowID] = *draft.agentID
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
		Adapter:           inputs.selection.adapter,
		Model:             sharedWorkstreamModel(workstreams),
		PostingIdentity:   postingKey(req.PostingIdentity),
		SelectedReviewers: selectedIDs,
		ReviewerFailures:  reviewerFailureSummaries(inputs.reviewerFailures),
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
	usage := draft.response.Usage
	workstream := reviewplan.WorkstreamUsage{
		Name:        name,
		Model:       draft.model,
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
			draft.model, usage.TokensIn, usage.TokensOut, usage.CacheRead, usage.CacheCreate,
		); ok {
			workstream.CostUSD = &est
			workstream.CostEstimated = true
		}
	}
	// Zero means the adapter never reported a duration; fall back to the
	// pipeline's own start/complete clock for the workstream, and render
	// unavailable (not 0s) when neither source has data.
	switch {
	case draft.response.DurationMS > 0:
		duration := draft.response.DurationMS
		workstream.DurationMS = &duration
	case !draft.startedAt.IsZero() && draft.completedAt.After(draft.startedAt):
		duration := draft.completedAt.Sub(draft.startedAt).Milliseconds()
		workstream.DurationMS = &duration
	}
	return workstream
}

func (opts Options) buildPlan(req Request, pr gitprovider.PR, postMode reviewplan.PostMode, caps reviewplan.ProviderCaps, diff reviewplan.Diff, findings []review.Finding, rollup review.Rollup, threadActions []review.ThreadAction, noDiff bool, agentDefsChanged bool, runInputs planRunInputs) (reviewplan.Plan, error) {
	runSummary, findingReviewers := opts.buildRunSummary(req, runInputs)
	return reviewplan.Build(reviewplan.Request{
		PostMode:      postMode,
		ProviderCaps:  caps,
		Diff:          diff,
		Findings:      findings,
		Rollup:        rollup,
		ThreadActions: threadActions,
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
		IncludeNits:             req.IncludeNits,
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

func buildReviewerPrompt(ctx context.Context, opts Options, req Request, pr gitprovider.PR, parsed ParsedDiff, selected llm.SelectedAgent, agent agents.Agent, model string) (string, error) {
	patchesByPath := map[string]FilePatch{}
	for _, patch := range parsed.Patches {
		patchesByPath[patch.Path] = patch
		if patch.OldPath != "" {
			patchesByPath[patch.OldPath] = patch
		}
	}
	var files []fileContext
	for _, path := range selected.Files {
		patch, ok := patchesByPath[path]
		if !ok {
			return "", fmt.Errorf("pipeline: selected file %q missing patch", path)
		}
		fc := fileContext{Path: patch.Path, Diff: patch.Patch}
		if agent.NeedsFullFileContent {
			basePath := patch.OldPath
			if basePath == "" {
				basePath = patch.Path
			}
			baseBytes, err := fetchFileOptional(ctx, opts.Provider, req.PRRef, pr.Base.SHA, basePath)
			if err != nil {
				return "", err
			}
			if err := opts.checkPromptBudget("full-content", agent.ID, model, basePath, string(baseBytes)); err != nil {
				return "", err
			}
			headBytes, err := fetchFileOptional(ctx, opts.Provider, req.PRRef, pr.Head.SHA, patch.Path)
			if err != nil {
				return "", err
			}
			if err := opts.checkPromptBudget("full-content", agent.ID, model, patch.Path, string(headBytes)); err != nil {
				return "", err
			}
			fc.BaseContent = string(baseBytes)
			fc.HeadContent = string(headBytes)
		}
		files = append(files, fc)
	}
	payload := map[string]any{
		"task":            "review files and return findings JSON only",
		"output_contract": findingsOutputContract(agent.ID, patchPathsFromContexts(files)),
		"agent":           agentPromptFromAgent(agent),
		"files":           files,
		"schema":          "findings",
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

type agentPrompt struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Category             agents.Category `json:"category"`
	Description          string          `json:"description,omitempty"`
	ModelTier            string          `json:"model_tier,omitempty"`
	ModelID              string          `json:"model_id,omitempty"`
	Effort               string          `json:"effort,omitempty"`
	FileGlobs            []string        `json:"file_globs,omitempty"`
	AppliesWhen          []string        `json:"applies_when,omitempty"`
	NeedsFullFileContent bool            `json:"needs_full_file_content"`
	Prompt               string          `json:"prompt,omitempty"`
	Provenance           string          `json:"provenance"`
	Overridden           []string        `json:"overridden,omitempty"`
}

func agentPromptFromAgent(agent agents.Agent) agentPrompt {
	return agentPrompt{
		ID:                   agent.ID,
		Name:                 agent.Name,
		Category:             agent.Category,
		Description:          agent.Description,
		ModelTier:            agent.ModelTier,
		ModelID:              agent.ModelID,
		Effort:               agent.Effort,
		FileGlobs:            append([]string(nil), agent.FileGlobs...),
		AppliesWhen:          append([]string(nil), agent.AppliesWhen...),
		NeedsFullFileContent: agent.NeedsFullFileContent,
		Prompt:               agent.Prompt,
		Provenance:           agent.Provenance.String(),
		Overridden:           append([]string(nil), agent.Overridden...),
	}
}

type selectionAgentPrompt struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Category             string   `json:"category,omitempty"`
	FileGlobs            []string `json:"file_globs,omitempty"`
	AppliesWhen          []string `json:"applies_when,omitempty"`
	NeedsFullFileContent bool     `json:"needs_full_file_content"`
}

func selectionAgentPromptFromAgent(agent agents.Agent) selectionAgentPrompt {
	return selectionAgentPrompt{
		ID:                   agent.ID,
		Name:                 agent.Name,
		Category:             agent.Category.Name,
		FileGlobs:            append([]string(nil), agent.FileGlobs...),
		AppliesWhen:          append([]string(nil), agent.AppliesWhen...),
		NeedsFullFileContent: agent.NeedsFullFileContent,
	}
}

func selectionAgentPromptsFromCatalog(catalog agents.Catalog) []selectionAgentPrompt {
	out := make([]selectionAgentPrompt, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		out = append(out, selectionAgentPromptFromAgent(agent))
	}
	return out
}

type fileContext struct {
	Path        string `json:"path"`
	Diff        string `json:"diff"`
	BaseContent string `json:"base_content,omitempty"`
	HeadContent string `json:"head_content,omitempty"`
}

func fetchFileOptional(ctx context.Context, provider ReadProvider, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	data, err := provider.GetFileAtRef(ctx, ref, gitRef, path)
	if errors.Is(err, gitprovider.ErrNotFound) {
		return nil, nil
	}
	return data, err
}

const defaultSelectionTask = "select reviewer agents and thread actions; return selection JSON only"

func buildSelectionPrompt(pr gitprovider.PR, catalog agents.Catalog, patches []FilePatch, threads []gitprovider.InlineThread, maxAgents int, selectionInstructions string) (string, error) {
	payload := map[string]any{
		"task":                defaultSelectionTask,
		"output_contract":     selectionOutputContract(catalog.Agents, patches, threads, maxAgents),
		"schema":              "selection",
		"max_selected_agents": maxAgents,
		"pr":                  pr,
		"agents":              selectionAgentPromptsFromCatalog(catalog),
		"changed_files":       patchPaths(patches),
		"threads":             threads,
	}
	if instructions := strings.TrimSpace(selectionInstructions); instructions != "" {
		payload["selection_instructions"] = instructions
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build selection prompt: %w", err)
	}
	return string(body), nil
}

func buildRollupPrompt(pr gitprovider.PR, findings []review.Finding, reviewerFailures []ReviewerFailure) (string, error) {
	payload := map[string]any{
		"task":              "dedupe findings and return rollup JSON only",
		"output_contract":   rollupOutputContract(findings),
		"schema":            "rollup",
		"pr":                pr,
		"findings":          rollupFindingsPrompt(findings),
		"reviewer_failures": reviewerFailures,
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build rollup prompt: %w", err)
	}
	return string(body), nil
}

type rollupFindingPrompt struct {
	ID       string                      `json:"id"`
	Severity string                      `json:"severity"`
	FilePath string                      `json:"file_path"`
	Location rollupFindingLocationPrompt `json:"location"`
	Body     string                      `json:"body"`
}

type rollupFindingLocationPrompt struct {
	Kind string `json:"kind"`
	Side string `json:"side,omitempty"`
	Line int    `json:"line,omitempty"`
}

func rollupFindingsPrompt(findings []review.Finding) []rollupFindingPrompt {
	out := make([]rollupFindingPrompt, 0, len(findings))
	for _, finding := range findings {
		out = append(out, rollupFindingPrompt{
			ID:       finding.ID.String(),
			Severity: finding.Severity.String(),
			FilePath: finding.FilePath,
			Location: rollupFindingLocationPrompt{
				Kind: finding.Anchor.Kind.String(),
				Side: finding.Anchor.Side.String(),
				Line: finding.Anchor.Line,
			},
			Body: finding.Body,
		})
	}
	return out
}

type agentSourcesArtifact struct {
	Sources []agents.SourceInfo       `json:"sources"`
	Agents  []agentProvenanceArtifact `json:"agents"`
}

type agentProvenanceArtifact struct {
	ID              string                     `json:"id"`
	Provenance      string                     `json:"provenance"`
	Source          agents.SourceInfo          `json:"source"`
	ReviewerRuntime *reviewerRuntimeResolution `json:"reviewer_runtime,omitempty"`
}

type reviewerRuntimeResolution struct {
	Mode           string                `json:"mode"`
	FloorTier      string                `json:"floor_tier,omitempty"`
	BaselineTier   string                `json:"baseline_tier,omitempty"`
	EffectiveTier  string                `json:"effective_tier,omitempty"`
	ResolvedModel  string                `json:"resolved_model"`
	ModelMapSource config.ModelMapSource `json:"model_map_source,omitempty"`
}

type outputContract struct {
	Instructions   []string `json:"instructions"`
	ResponseSchema any      `json:"response_schema"`
	AllowedValues  any      `json:"allowed_values,omitempty"`
	Example        any      `json:"example"`
}

func selectionOutputContract(agents []agents.Agent, patches []FilePatch, threads []gitprovider.InlineThread, maxAgents int) outputContract {
	agentIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}
	changedFiles := patchPaths(patches)
	threadIDs := make([]string, 0, len(threads))
	for _, thread := range threads {
		threadIDs = append(threadIDs, string(thread.ID))
	}
	example := map[string]any{
		"schema_version":  1,
		"selected_agents": selectionExampleAgents(agentIDs, changedFiles),
		"thread_actions":  []map[string]any{},
		"reasoning":       "Selected the relevant reviewers for the changed files.",
	}
	if len(agentIDs) == 0 {
		example["reasoning"] = "No reviewer agents are available."
	}
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"selected_agents[].agent_id must be one of the allowed_agent_ids.",
			"selected_agents[].files must contain only paths from changed_files.",
			"selected_agents must contain at most max_selected_agents entries, ordered from highest to lowest review value.",
			"thread_actions must be an empty array when there are no threads.",
		},
		ResponseSchema: map[string]any{
			"schema_version":  "number, required, must be 1",
			"selected_agents": "array of {agent_id: string, rationale: string, files: string[]}",
			"thread_actions":  "array of {thread_id: string, decision: string, summary: string, safe_to_resolve_rationale: string}",
			"reasoning":       "string",
		},
		AllowedValues: map[string]any{
			"allowed_agent_ids":   agentIDs,
			"changed_files":       changedFiles,
			"known_thread_ids":    threadIDs,
			"max_selected_agents": maxAgents,
			"thread_decisions":    []string{"skip", "summarize_only", "summarize_and_resolve"},
		},
		Example: example,
	}
}

func selectionExampleAgents(agentIDs []string, changedFiles []string) []map[string]any {
	if len(agentIDs) == 0 {
		return []map[string]any{}
	}
	return []map[string]any{{
		"agent_id":  agentIDs[0],
		"rationale": "This agent applies to the changed files.",
		"files":     firstNOrPlaceholder(changedFiles, "path/to/changed-file.ext", 1),
	}}
}

func findingsOutputContract(agentID string, changedFiles []string) outputContract {
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"agent_id must match the provided agent id.",
			"findings must be an empty array when there are no actionable findings.",
			"file_path must be one of changed_files.",
			"Do not provide finding_id; the harness assigns IDs.",
		},
		ResponseSchema: map[string]any{
			"schema_version": "number, required, must be 1",
			"agent_id":       "string, required",
			"findings":       "array of {severity: string, file_path: string, anchor: {kind: 'file'} or {kind: 'line', side: 'RIGHT'|'LEFT', line: positive number}, body: string}",
		},
		AllowedValues: map[string]any{
			"severities":    []string{"blocking", "major", "minor", "nits"},
			"changed_files": changedFiles,
		},
		Example: map[string]any{
			"schema_version": 1,
			"agent_id":       agentID,
			"findings": []map[string]any{{
				"severity":  "major",
				"file_path": firstOrPlaceholder(changedFiles, "path/to/changed-file.ext"),
				"anchor": map[string]any{
					"kind": "file",
				},
				"body": "Explain the issue and the concrete impact. Include the suggested fix in the same body.",
			}},
		},
	}
}

func rollupOutputContract(findings []review.Finding) outputContract {
	findingIDs := make([]string, 0, len(findings))
	for _, finding := range findings {
		findingIDs = append(findingIDs, finding.ID.String())
	}
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"ordered_findings must contain finding ID strings only and include every kept finding exactly once.",
			"dedupe_log kept and dropped values must contain finding ID strings only, never finding objects.",
			"Use finding location only to distinguish findings during dedupe; do not include finding fields such as severity, file_path, location, body, anchor, or finding_id in the response.",
			"dedupe_log must be an empty array when no findings are duplicates.",
		},
		ResponseSchema: map[string]any{
			"schema_version":         "number, required, must be 1",
			"review_event":           "string: approve, comment, or request_changes",
			"review_event_rationale": "string",
			"dedupe_log":             "array of {kept: finding_id, dropped: finding_id[], reason: string}",
			"ordered_findings":       "array of finding ids after dedupe",
		},
		AllowedValues: map[string]any{
			"available_finding_ids": findingIDs,
		},
		Example: map[string]any{
			"schema_version":         1,
			"review_event":           "comment",
			"review_event_rationale": "Commenting because findings remain for human review.",
			"dedupe_log":             []map[string]any{},
			"ordered_findings":       findingIDs,
		},
	}
}

func patchPathsFromContexts(files []fileContext) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

func firstOrPlaceholder(values []string, placeholder string) string {
	if len(values) > 0 {
		return values[0]
	}
	return placeholder
}

func firstNOrPlaceholder(values []string, placeholder string, count int) []string {
	if len(values) == 0 {
		return []string{placeholder}
	}
	if count > len(values) {
		count = len(values)
	}
	return append([]string(nil), values[:count]...)
}

func writeArtifacts(paths ArtifactPaths, rawDiff string, patches []FilePatch, catalog agents.Catalog, selection llm.Selection, findings []review.Finding, rollup string, reviewerRuntime map[string]reviewerRuntimeResolution) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create slices dir: %w", err)
	}
	if err := os.WriteFile(paths.DiffPatch, []byte(rawDiff), 0o600); err != nil {
		return fmt.Errorf("pipeline: write diff: %w", err)
	}
	sourceJSON, err := json.MarshalIndent(agentSourcesArtifactFromCatalog(catalog, reviewerRuntime), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(paths.AgentSourcesJSON, append(sourceJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write agent source provenance: %w", err)
	}
	for _, selected := range selection.SelectedAgents {
		for _, file := range selected.Files {
			patch, ok := findPatch(patches, file)
			if !ok {
				continue
			}
			path, err := paths.SlicePatch(selected.AgentID, file)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("pipeline: create slice dir: %w", err)
			}
			if err := os.WriteFile(path, []byte(patch.Patch), 0o600); err != nil {
				return fmt.Errorf("pipeline: write slice: %w", err)
			}
		}
	}
	findingsJSON, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(paths.FindingsJSON, append(findingsJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write findings: %w", err)
	}
	if err := os.WriteFile(paths.RollupMarkdown, []byte(rollup+"\n"), 0o600); err != nil {
		return fmt.Errorf("pipeline: write rollup: %w", err)
	}
	return nil
}

type dossierInputs struct {
	CurrentPR             gitprovider.PR
	ReviewPR              gitprovider.PR
	PinnedReview          bool
	ChangedFiles          []FilePatch
	Threads               []gitprovider.InlineThread
	Reviews               []gitprovider.Review
	IssueComments         []gitprovider.IssueComment
	Catalog               agents.Catalog
	CurrentBaseSHA        string
	CurrentHeadSHA        string
	DiscussionOmittedNote string
}

type dossierChangedFileArtifact struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Binary    bool   `json:"binary,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	HunkCount int    `json:"hunk_count,omitempty"`
}

type dossierTopLevelCommentArtifact struct {
	Kind      string    `json:"kind"`
	URL       string    `json:"url,omitempty"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body"`
	Event     string    `json:"event,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type dossierThreadCommentArtifact struct {
	URL       string    `json:"url,omitempty"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body"`
	CommitSHA string    `json:"commit_sha,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type dossierInlineThreadArtifact struct {
	ID         string                         `json:"id"`
	Path       string                         `json:"path,omitempty"`
	Side       string                         `json:"side,omitempty"`
	Line       int                            `json:"line,omitempty"`
	AnchorKind string                         `json:"anchor_kind,omitempty"`
	Resolved   bool                           `json:"resolved"`
	CommitSHA  string                         `json:"commit_sha,omitempty"`
	Comments   []dossierThreadCommentArtifact `json:"comments,omitempty"`
}

type dossierRepoContextArtifact struct {
	RepoInfo                     *agents.RepoInfo    `json:"repo_info,omitempty"`
	Sources                      []agents.SourceInfo `json:"sources,omitempty"`
	ExplicitReviewGuidance       bool                `json:"explicit_review_guidance"`
	ExplicitReviewGuidanceSource string              `json:"explicit_review_guidance_source,omitempty"`
}

type dossierPRContextArtifact struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Author        string `json:"author,omitempty"`
	BaseRef       string `json:"base_ref,omitempty"`
	BaseSHA       string `json:"base_sha,omitempty"`
	HeadRef       string `json:"head_ref,omitempty"`
	HeadSHA       string `json:"head_sha,omitempty"`
	ReviewBaseSHA string `json:"review_base_sha,omitempty"`
	ReviewHeadSHA string `json:"review_head_sha,omitempty"`
	PinnedReview  bool   `json:"pinned_review"`
	Body          string `json:"body,omitempty"`
}

type dossierDiscussionArtifact struct {
	PinnedReview          bool                             `json:"pinned_review"`
	DiscussionOmittedNote string                           `json:"discussion_omitted_note,omitempty"`
	TopLevelComments      []dossierTopLevelCommentArtifact `json:"top_level_comments,omitempty"`
	InlineThreads         []dossierInlineThreadArtifact    `json:"inline_threads,omitempty"`
}

type dossierIndexArtifact struct {
	HashAlgorithm string                     `json:"hash_algorithm"`
	Files         []dossierIndexFileArtifact `json:"files"`
}

type dossierIndexFileArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func writeDossierArtifacts(paths ArtifactPaths, in dossierInputs) error {
	rawDir := filepath.Join(paths.DossierDir, "raw")
	summaryDir := filepath.Join(paths.DossierDir, "summary")
	finalDir := filepath.Join(paths.DossierDir, "final")
	for _, dir := range []string{paths.DossierDir, rawDir, summaryDir, finalDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pipeline: create dossier dir: %w", err)
		}
	}

	prContext := dossierPRContextArtifact{
		Title:         in.CurrentPR.Title,
		URL:           in.CurrentPR.URL,
		Author:        in.CurrentPR.Author.Login,
		BaseRef:       in.CurrentPR.Base.Ref,
		BaseSHA:       in.CurrentBaseSHA,
		HeadRef:       in.CurrentPR.Head.Ref,
		HeadSHA:       in.CurrentHeadSHA,
		ReviewBaseSHA: in.ReviewPR.Base.SHA,
		ReviewHeadSHA: in.ReviewPR.Head.SHA,
		PinnedReview:  in.PinnedReview,
		Body:          strings.TrimSpace(in.CurrentPR.Body),
	}

	changedFiles := dossierChangedFiles(in.ChangedFiles)
	topLevelComments := dossierTopLevelComments(in.IssueComments, in.Reviews)
	inlineThreads := dossierInlineThreads(in.Threads)
	repoContext := dossierRepoContextArtifact{
		RepoInfo:               in.Catalog.Repo,
		Sources:                append([]agents.SourceInfo(nil), in.Catalog.Sources...),
		ExplicitReviewGuidance: false,
	}
	discussion := dossierDiscussionArtifact{
		PinnedReview:          in.PinnedReview,
		DiscussionOmittedNote: strings.TrimSpace(in.DiscussionOmittedNote),
	}
	if !in.PinnedReview {
		discussion.TopLevelComments = topLevelComments
		discussion.InlineThreads = inlineThreads
	}

	rawFiles := map[string]any{
		"pr-context.json":         prContext,
		"changed-files.json":      changedFiles,
		"top-level-comments.json": topLevelComments,
		"inline-threads.json":     inlineThreads,
		"repo-context.json":       repoContext,
		"discussion.json":         discussion,
	}
	for name, payload := range rawFiles {
		path, err := paths.DossierRawPath(name)
		if err != nil {
			return err
		}
		if err := writeJSONFile(path, payload); err != nil {
			return err
		}
	}

	finalArtifacts := map[string]string{
		"pr-intent.md":     renderDossierPRIntent(prContext),
		"change-map.md":    renderDossierChangeMap(changedFiles),
		"repo-guidance.md": renderDossierRepoGuidance(repoContext),
		"discussion.md":    renderDossierDiscussion(paths, discussion),
	}
	for name, body := range finalArtifacts {
		path, err := paths.DossierFinalPath(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fmt.Errorf("pipeline: write dossier artifact %s: %w", name, err)
		}
	}

	index, err := buildDossierIndex(paths.DossierDir)
	if err != nil {
		return err
	}
	return writeJSONFile(paths.DossierIndexPath(), index)
}

func dossierChangedFiles(patches []FilePatch) []dossierChangedFileArtifact {
	out := make([]dossierChangedFileArtifact, 0, len(patches))
	for _, patch := range patches {
		additions, deletions := diffStats(patch.Patch)
		out = append(out, dossierChangedFileArtifact{
			Path:      patch.Path,
			OldPath:   oldPathIfDifferent(patch.OldPath, patch.Path),
			Status:    filePatchStatus(patch),
			Binary:    patch.Binary,
			Deleted:   patch.Deleted,
			Additions: additions,
			Deletions: deletions,
			HunkCount: len(patch.Hunks),
		})
	}
	return out
}

func dossierTopLevelComments(issueComments []gitprovider.IssueComment, reviews []gitprovider.Review) []dossierTopLevelCommentArtifact {
	out := make([]dossierTopLevelCommentArtifact, 0, len(issueComments)+len(reviews))
	for _, comment := range issueComments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		out = append(out, dossierTopLevelCommentArtifact{
			Kind:      "issue_comment",
			URL:       comment.URL,
			Author:    comment.Author.Login,
			Body:      body,
			CreatedAt: comment.CreatedAt,
			UpdatedAt: comment.UpdatedAt,
		})
	}
	for _, reviewRecord := range reviews {
		body := strings.TrimSpace(reviewRecord.Body)
		if body == "" {
			continue
		}
		out = append(out, dossierTopLevelCommentArtifact{
			Kind:      "review",
			URL:       reviewRecord.URL,
			Author:    reviewRecord.Author.Login,
			Body:      body,
			Event:     string(reviewRecord.Event),
			CreatedAt: reviewRecord.SubmittedAt,
			UpdatedAt: reviewRecord.SubmittedAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].URL < out[j].URL
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func dossierInlineThreads(threads []gitprovider.InlineThread) []dossierInlineThreadArtifact {
	out := make([]dossierInlineThreadArtifact, 0, len(threads))
	for _, thread := range threads {
		comments := make([]dossierThreadCommentArtifact, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			comments = append(comments, dossierThreadCommentArtifact{
				URL:       comment.URL,
				Author:    comment.Author.Login,
				Body:      strings.TrimSpace(comment.Body),
				CommitSHA: comment.CommitSHA,
				CreatedAt: comment.CreatedAt,
				UpdatedAt: comment.UpdatedAt,
			})
		}
		out = append(out, dossierInlineThreadArtifact{
			ID:         string(thread.ID),
			Path:       thread.Path,
			Side:       string(thread.Side),
			Line:       thread.Line,
			AnchorKind: string(thread.SubjectType),
			Resolved:   thread.Resolved,
			CommitSHA:  thread.CommitSHA,
			Comments:   comments,
		})
	}
	return out
}

func renderDossierPRIntent(pr dossierPRContextArtifact) string {
	var out strings.Builder
	out.WriteString("# PR Intent\n\n")
	if title := strings.TrimSpace(pr.Title); title != "" {
		out.WriteString("Title: ")
		out.WriteString(title)
		out.WriteString("\n\n")
	}
	if body := strings.TrimSpace(pr.Body); body != "" {
		out.WriteString(body)
		out.WriteString("\n\n")
	} else {
		out.WriteString("No PR body provided.\n\n")
	}
	if pr.URL != "" {
		out.WriteString("URL: ")
		out.WriteString(pr.URL)
		out.WriteString("\n")
	}
	if pr.Author != "" {
		out.WriteString("Author: ")
		out.WriteString(pr.Author)
		out.WriteString("\n")
	}
	out.WriteString("Review SHAs: ")
	out.WriteString(shortSHAOrValue(pr.ReviewBaseSHA))
	out.WriteString(" -> ")
	out.WriteString(shortSHAOrValue(pr.ReviewHeadSHA))
	out.WriteString("\n")
	if pr.PinnedReview {
		out.WriteString("Pinned review: true\n")
	}
	return out.String()
}

func renderDossierChangeMap(files []dossierChangedFileArtifact) string {
	var out strings.Builder
	out.WriteString("# Change Map\n\n")
	if len(files) == 0 {
		out.WriteString("No changed files.\n")
		return out.String()
	}
	for _, file := range files {
		out.WriteString("- ")
		out.WriteString(file.Status)
		out.WriteString(": ")
		out.WriteString(file.Path)
		if file.OldPath != "" {
			out.WriteString(" (from ")
			out.WriteString(file.OldPath)
			out.WriteString(")")
		}
		out.WriteString(fmt.Sprintf(" [+%d -%d]", file.Additions, file.Deletions))
		if file.Binary {
			out.WriteString(" binary")
		}
		out.WriteString("\n")
	}
	return out.String()
}

func renderDossierRepoGuidance(repo dossierRepoContextArtifact) string {
	var out strings.Builder
	out.WriteString("# Repo Guidance\n\n")
	out.WriteString("No dedicated repo review-guidance source is defined yet.\n\n")
	if repo.RepoInfo != nil {
		out.WriteString("Repo-local agent provenance: ")
		out.WriteString(repo.RepoInfo.Provenance)
		out.WriteString("\n\n")
		if note := strings.TrimSpace(repo.RepoInfo.TrustNote()); note != "" {
			out.WriteString(note)
			out.WriteString("\n")
		}
	}
	return out.String()
}

func renderDossierDiscussion(paths ArtifactPaths, discussion dossierDiscussionArtifact) string {
	if summary, ok := readSummaryArtifact(paths, "discussion.md"); ok {
		return summary
	}
	var out strings.Builder
	out.WriteString("# Discussion\n\n")
	if discussion.PinnedReview {
		note := strings.TrimSpace(discussion.DiscussionOmittedNote)
		if note == "" {
			note = "Current PR discussion omitted for pinned review."
		}
		out.WriteString(note)
		out.WriteString("\n")
		return out.String()
	}
	out.WriteString("## Top-level comments\n\n")
	if len(discussion.TopLevelComments) == 0 {
		out.WriteString("None.\n\n")
	} else {
		for _, comment := range discussion.TopLevelComments {
			out.WriteString("- ")
			if comment.Kind != "" {
				out.WriteString(comment.Kind)
				out.WriteString(" ")
			}
			if comment.Author != "" {
				out.WriteString("by ")
				out.WriteString(comment.Author)
			}
			if comment.Event != "" {
				out.WriteString(" (")
				out.WriteString(comment.Event)
				out.WriteString(")")
			}
			out.WriteString(": ")
			out.WriteString(singleLine(comment.Body))
			out.WriteString("\n")
		}
		out.WriteString("\n")
	}
	out.WriteString("## Inline threads\n\n")
	if len(discussion.InlineThreads) == 0 {
		out.WriteString("None.\n")
		return out.String()
	}
	for _, thread := range discussion.InlineThreads {
		out.WriteString("- ")
		out.WriteString(thread.Path)
		if thread.Line > 0 {
			out.WriteString(fmt.Sprintf(":%d", thread.Line))
		}
		if thread.Side != "" {
			out.WriteString(" [")
			out.WriteString(thread.Side)
			out.WriteString("]")
		}
		if thread.AnchorKind != "" {
			out.WriteString(" {")
			out.WriteString(thread.AnchorKind)
			out.WriteString("}")
		}
		if thread.Resolved {
			out.WriteString(" resolved")
		}
		out.WriteString("\n")
		for _, comment := range thread.Comments {
			out.WriteString("  ")
			if comment.Author != "" {
				out.WriteString(comment.Author)
				out.WriteString(": ")
			}
			out.WriteString(singleLine(comment.Body))
			out.WriteString("\n")
		}
	}
	return out.String()
}

func readSummaryArtifact(paths ArtifactPaths, name string) (string, bool) {
	path, err := paths.DossierSummaryPath(name)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func buildDossierIndex(dir string) (dossierIndexArtifact, error) {
	var files []dossierIndexFileArtifact
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Base(path) == "index.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, dossierIndexFileArtifact{
			Path:   filepath.ToSlash(rel),
			SHA256: sha256Hex(data),
		})
		return nil
	})
	if err != nil {
		return dossierIndexArtifact{}, fmt.Errorf("pipeline: build dossier index: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return dossierIndexArtifact{HashAlgorithm: "sha256", Files: files}, nil
}

func writeJSONFile(path string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write dossier artifact %s: %w", filepath.Base(path), err)
	}
	return nil
}

func diffStats(patch string) (int, int) {
	additions := 0
	deletions := 0
	for _, line := range splitLines(patch) {
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

func filePatchStatus(patch FilePatch) string {
	switch {
	case patch.Deleted:
		return "deleted"
	case strings.Contains(patch.Patch, "new file mode") || strings.Contains(patch.Patch, "--- /dev/null"):
		return "added"
	case patch.OldPath != "" && patch.Path != "" && patch.OldPath != patch.Path:
		return "renamed"
	default:
		return "modified"
	}
}

func oldPathIfDifferent(oldPath, path string) string {
	oldPath = strings.TrimSpace(oldPath)
	path = strings.TrimSpace(path)
	if oldPath == "" || oldPath == path {
		return ""
	}
	return oldPath
}

func singleLine(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "(empty)"
	}
	return value
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortSHAOrValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 12 {
		return value[:12]
	}
	if value == "" {
		return "unknown"
	}
	return value
}

func agentSourcesArtifactFromCatalog(catalog agents.Catalog, reviewerRuntime map[string]reviewerRuntimeResolution) agentSourcesArtifact {
	artifact := agentSourcesArtifact{
		Sources: append([]agents.SourceInfo(nil), catalog.Sources...),
		Agents:  make([]agentProvenanceArtifact, 0, len(catalog.Agents)),
	}
	for i := range artifact.Sources {
		artifact.Sources[i].Warnings = append([]string(nil), catalog.Sources[i].Warnings...)
	}
	for _, agent := range catalog.Agents {
		runtime, ok := reviewerRuntime[agent.ID]
		var runtimePtr *reviewerRuntimeResolution
		if ok {
			runtimeCopy := runtime
			runtimePtr = &runtimeCopy
		}
		artifact.Agents = append(artifact.Agents, agentProvenanceArtifact{
			ID:              agent.ID,
			Provenance:      agent.Provenance.String(),
			Source:          agent.Provenance.SourceInfo(),
			ReviewerRuntime: runtimePtr,
		})
	}
	return artifact
}

func reviewerRuntimeArtifact(req Request, catalog agents.Catalog, selection llm.Selection) map[string]reviewerRuntimeResolution {
	if strings.TrimSpace(req.ReviewerModelOverride) != "" {
		return nil
	}
	if len(selection.SelectedAgents) == 0 {
		return nil
	}
	agentsByID := make(map[string]agents.Agent, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		agentsByID[agent.ID] = agent
	}
	out := make(map[string]reviewerRuntimeResolution, len(selection.SelectedAgents))
	for _, selected := range selection.SelectedAgents {
		agent, ok := agentsByID[selected.AgentID]
		if !ok {
			continue
		}
		resolution, err := resolveAgentModel(req.Profile, req.ReviewerModelTierOverride, agent)
		if err != nil {
			continue
		}
		out[selected.AgentID] = resolution
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func plannedAction(runID string, action reviewplan.Action) (ledger.PlannedAction, error) {
	payload, err := actionPayload(action)
	if err != nil {
		return ledger.PlannedAction{}, err
	}
	planned := ledger.PlannedAction{
		ActionID:    action.ActionID,
		RunID:       runID,
		Kind:        ledgerKind(action.Kind),
		PlannedAt:   action.PlannedAt,
		PayloadJSON: string(payload),
		Status:      ledgerStatus(action.Status),
		Required:    action.Required,
	}
	if action.FindingID.Assigned() {
		id := action.FindingID.String()
		planned.FindingID = &id
	}
	if strings.TrimSpace(action.ThreadID) != "" {
		planned.ThreadID = &action.ThreadID
	}
	return planned, nil
}

func actionPayload(action reviewplan.Action) ([]byte, error) {
	switch action.Kind {
	case reviewplan.ActionKindInlineComment:
		if action.InlineComment == nil {
			return nil, fmt.Errorf("pipeline: inline payload missing")
		}
		return json.Marshal(outbox.InlineCommentPayload{
			Body:         action.InlineComment.Body,
			Path:         action.InlineComment.Path,
			Side:         action.InlineComment.Side,
			Line:         action.InlineComment.Line,
			SubjectType:  action.InlineComment.SubjectType,
			DiffPosition: action.InlineComment.DiffPosition,
		})
	case reviewplan.ActionKindThreadReply:
		if action.ThreadReply == nil {
			return nil, fmt.Errorf("pipeline: thread reply payload missing")
		}
		return json.Marshal(outbox.ThreadReplyPayload{
			Body:    action.ThreadReply.Body,
			Summary: action.ThreadReply.Summary,
		})
	case reviewplan.ActionKindResolveThread:
		return json.Marshal(outbox.ResolveThreadPayload{})
	case reviewplan.ActionKindRollupComment:
		if action.RollupComment == nil {
			return nil, fmt.Errorf("pipeline: rollup payload missing")
		}
		return json.Marshal(outbox.RollupCommentPayload{Body: action.RollupComment.Body})
	case reviewplan.ActionKindSubmitReview:
		if action.SubmitReview == nil {
			return nil, fmt.Errorf("pipeline: submit review payload missing")
		}
		return json.Marshal(outbox.SubmitReviewPayload{
			Body:  action.SubmitReview.Body,
			Event: action.SubmitReview.Event,
		})
	default:
		return nil, fmt.Errorf("pipeline: unknown action kind %q", action.Kind)
	}
}

func ledgerKind(kind reviewplan.ActionKind) ledger.PlannedActionKind {
	switch kind {
	case reviewplan.ActionKindInlineComment:
		return ledger.PlannedActionInlineComment
	case reviewplan.ActionKindThreadReply:
		return ledger.PlannedActionThreadReply
	case reviewplan.ActionKindResolveThread:
		return ledger.PlannedActionResolveThread
	case reviewplan.ActionKindRollupComment:
		return ledger.PlannedActionRollupComment
	case reviewplan.ActionKindSubmitReview:
		return ledger.PlannedActionSubmitReview
	default:
		return ledger.PlannedActionKind(kind)
	}
}

func ledgerStatus(status reviewplan.ActionStatus) ledger.PlannedActionStatus {
	switch status {
	case reviewplan.ActionStatusPending:
		return ledger.PlannedActionPending
	case reviewplan.ActionStatusPlannedOnly:
		return ledger.PlannedActionPlannedOnly
	default:
		return ledger.PlannedActionStatus(status)
	}
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
	prKey, err := statepaths.PRKey(ref.Host, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return ArtifactPaths{}, err
	}
	scope, err := statepaths.ResumeScope(profile, postingIdentity)
	if err != nil {
		return ArtifactPaths{}, err
	}
	dir := filepath.Join(layout.DataRoot, "runs", prKey, pr.Head.SHA, pr.Base.SHA, scope, "run-"+statepaths.Encode(runID))
	return ArtifactPaths{
		Dir:              dir,
		DiffPatch:        filepath.Join(dir, "diff.patch"),
		SlicesDir:        filepath.Join(dir, "slices"),
		FindingsJSON:     filepath.Join(dir, "findings.json"),
		RollupMarkdown:   filepath.Join(dir, "rollup.md"),
		AgentSourcesJSON: filepath.Join(dir, "agent-sources.json"),
		AgentLogsDir:     filepath.Join(dir, "agent-logs"),
		LLMTasksDir:      filepath.Join(dir, "llm-tasks"),
		DossierDir:       filepath.Join(dir, "dossier"),
	}, nil
}

// ArtifactPathsFromDir returns the artifact path set rooted at dir.
func ArtifactPathsFromDir(dir string) ArtifactPaths {
	return ArtifactPaths{
		Dir:              dir,
		DiffPatch:        filepath.Join(dir, "diff.patch"),
		SlicesDir:        filepath.Join(dir, "slices"),
		FindingsJSON:     filepath.Join(dir, "findings.json"),
		RollupMarkdown:   filepath.Join(dir, "rollup.md"),
		AgentSourcesJSON: filepath.Join(dir, "agent-sources.json"),
		AgentLogsDir:     filepath.Join(dir, "agent-logs"),
		LLMTasksDir:      filepath.Join(dir, "llm-tasks"),
		DossierDir:       filepath.Join(dir, "dossier"),
	}
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

func knownAgents(catalog agents.Catalog) map[string]bool {
	out := make(map[string]bool, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		out[agent.ID] = true
	}
	return out
}

func changedFiles(patches []FilePatch) map[string]bool {
	out := make(map[string]bool, len(patches))
	for _, patch := range patches {
		if patch.Path != "" {
			out[patch.Path] = true
		}
		if patch.OldPath != "" {
			out[patch.OldPath] = true
		}
	}
	return out
}

func knownThreads(threads []gitprovider.InlineThread) map[string]bool {
	out := make(map[string]bool, len(threads))
	for _, thread := range threads {
		out[string(thread.ID)] = true
	}
	return out
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
	if strings.TrimSpace(modelOverride) != "" {
		return applyStageRuntimeOverrides(modelOverride, effortOverride, "", string(modelprefs.EffortMedium)), nil
	}
	model, err := resolveModelTier(profile, config.ModelTierMedium)
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return applyStageRuntimeOverrides(modelOverride, effortOverride, model, string(modelprefs.EffortMedium)), nil
}

func resolveSynthesisRuntimeConfig(req Request) (llmRuntimeConfig, error) {
	model, err := resolveModelTier(req.Profile, config.ModelTierMedium)
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return llmRuntimeConfig{model: model, effort: string(modelprefs.EffortMedium)}, nil
}

func resolveReviewerRuntimeConfig(req Request, agent agents.Agent) (llmRuntimeConfig, error) {
	if strings.TrimSpace(req.ReviewerModelOverride) != "" {
		return applyStageRuntimeOverrides(req.ReviewerModelOverride, req.ReviewerEffortOverride, "", agent.Effort), nil
	}
	resolved, err := resolveAgentModel(req.Profile, req.ReviewerModelTierOverride, agent)
	if err != nil {
		return llmRuntimeConfig{}, err
	}
	return applyStageRuntimeOverrides(req.ReviewerModelOverride, req.ReviewerEffortOverride, resolved.ResolvedModel, agent.Effort), nil
}

func resolveAgentModel(profile config.Profile, baselineOverride string, agent agents.Agent) (reviewerRuntimeResolution, error) {
	if modelID := strings.TrimSpace(agent.ModelID); modelID != "" {
		return reviewerRuntimeResolution{
			Mode:          "exact_model",
			ResolvedModel: modelID,
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
	effectiveTier := maxModelTier(baselineTier, floorTier)
	resolved, err := resolveModelTierResolution(profile, effectiveTier)
	if err != nil {
		return reviewerRuntimeResolution{}, fmt.Errorf("pipeline: agent %s: %w", agent.ID, err)
	}
	return reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      string(floorTier),
		BaselineTier:   string(baselineTier),
		EffectiveTier:  string(effectiveTier),
		ResolvedModel:  resolved.Model,
		ModelMapSource: resolved.Source,
	}, nil
}

func resolveModelTier(profile config.Profile, tier config.ModelTier) (string, error) {
	resolved, err := resolveModelTierResolution(profile, tier)
	if err != nil {
		return "", err
	}
	return resolved.Model, nil
}

func resolveModelTierResolution(profile config.Profile, tier config.ModelTier) (config.ModelMapResolution, error) {
	resolved, ok := config.ResolveModelTier(profile.LLM, tier)
	if !ok {
		llmConfig := profile.LLM
		return config.ModelMapResolution{}, fmt.Errorf("model_tier %q is not mapped for provider %q adapter %q", tier, llmConfig.Provider, llmConfig.Adapter)
	}
	return resolved, nil
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

func maxModelTier(left, right config.ModelTier) config.ModelTier {
	if modelTierRank(left) >= modelTierRank(right) {
		return left
	}
	return right
}

func modelTierRank(tier config.ModelTier) int {
	switch tier {
	case config.ModelTierSmall:
		return 1
	case config.ModelTierMedium:
		return 2
	case config.ModelTierLarge:
		return 3
	default:
		return 0
	}
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

func intPtrToInt64(value *int) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

func int64PtrToInt(value *int64) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}
