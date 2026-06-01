// Package pipeline orchestrates review pipeline phases without owning command IO.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	defaultMaxAgents      = 5
	defaultMaxConcurrency = 5
	defaultMaxPromptBytes = 512 * 1024
)

// ReadProvider is the PR read boundary needed by dry-run review.
type ReadProvider interface {
	GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error)
	GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error)
	GetFileAtRef(context.Context, gitprovider.PRRef, string, string) ([]byte, error)
	ListTreeAtRef(context.Context, gitprovider.PRRef, string, string) ([]gitprovider.TreeEntry, error)
	ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error)
	Capabilities() gitprovider.ProviderCaps
}

// Store is the ledger behavior required by the dry-run pipeline.
type Store interface {
	AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error)
	InsertSession(context.Context, ledger.Session) error
	InsertFinding(context.Context, ledger.Finding) error
	InsertPlannedAction(context.Context, ledger.PlannedAction) error
	CompleteRun(context.Context, string, ledger.Outcome, time.Time) error
}

// ContextBudget limits prompt size. A negative MaxPromptBytes disables checks.
type ContextBudget struct {
	MaxPromptBytes int
}

// Options contains dry-run pipeline dependencies.
type Options struct {
	Provider ReadProvider
	Adapter  llm.Adapter
	Store    Store
	Layout   statepaths.Layout

	Now             func() time.Time
	NewRunID        func() string
	NewSessionRowID func() string
	NewFindingID    func() (review.FindingID, error)
	NewActionID     func(reviewplan.ActionKind) (string, error)

	Budget         ContextBudget
	MaxAgents      int
	MaxConcurrency int
}

// Request identifies one dry-run review.
type Request struct {
	PRRef           gitprovider.PRRef
	PRURL           string
	ProfileName     string
	Profile         config.Profile
	PostingIdentity gitprovider.Identity
	AgentDirs       []string

	FailOn              *review.Severity
	IncludeNits         bool
	AllowSelfReview     bool
	AllowSelfApprove    bool
	NoResolveThreads    bool
	MajorRequestChanges bool
}

// ArtifactPaths contains per-run artifact paths.
type ArtifactPaths struct {
	Dir            string `json:"dir"`
	DiffPatch      string `json:"diff_patch"`
	SlicesDir      string `json:"slices_dir"`
	FindingsJSON   string `json:"findings_json"`
	RollupMarkdown string `json:"rollup_markdown"`
	AgentLogsDir   string `json:"agent_logs_dir"`
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

// Result is the completed dry-run pipeline output.
type Result struct {
	Run              ledger.Run
	PR               gitprovider.PR
	PRKey            string
	Artifacts        ArtifactPaths
	Quota            llm.Quota
	QuotaSupported   bool
	QuotaLow         bool
	Catalog          agents.Catalog
	Selection        llm.Selection
	Findings         []review.Finding
	Rollup           review.Rollup
	Plan             reviewplan.Plan
	Sessions         []ledger.Session
	PlannedActions   []ledger.PlannedAction
	FailOnTriggered  bool
	EffectiveCaps    reviewplan.ProviderCaps
	AgentDefsChanged bool
}

type sessionDraft struct {
	rowID             string
	providerSessionID string
	role              ledger.SessionRole
	agentID           *string
	adapter           string
	model             string
	effort            string
	startedAt         time.Time
	completedAt       time.Time
	response          llm.Response
}

type executionMode struct {
	live         bool
	run          ledger.Run
	planPostMode reviewplan.PostMode
	completeAs   ledger.Outcome
}

// DryRun executes the dry-run review pipeline.
func DryRun(ctx context.Context, opts Options, req Request) (Result, error) {
	return execute(ctx, opts, req, executionMode{
		planPostMode: reviewplan.PostModeDryRun,
		completeAs:   ledger.OutcomeDryRun,
	})
}

// Live executes the review planning phases into a gate-allocated live run.
func Live(ctx context.Context, opts Options, req Request, run ledger.Run) (Result, error) {
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

func execute(ctx context.Context, opts Options, req Request, mode executionMode) (Result, error) {
	if err := validate(opts, req); err != nil {
		return Result{}, err
	}
	completed := false
	if mode.live {
		defer func() {
			if !completed {
				_ = opts.Store.CompleteRun(context.Background(), mode.run.RunID, ledger.OutcomeFailed, opts.now())
			}
		}()
	}
	now := opts.now()
	maxAgents := opts.maxAgents()
	maxConcurrency := opts.maxConcurrency(maxAgents)

	pr, err := opts.Provider.GetPR(ctx, req.PRRef)
	if err != nil {
		return Result{}, err
	}
	if sameIdentity(pr.Author, req.PostingIdentity) && req.Profile.ReviewerCredentials != nil && !req.AllowSelfReview {
		return Result{}, fmt.Errorf("pipeline: reviewer credentials resolve to PR author %q; pass --allow-self-review to continue", req.PostingIdentity.Login)
	}
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		return Result{}, err
	}
	runID := opts.newRunID()
	var artifacts ArtifactPaths
	if mode.live {
		runID = mode.run.RunID
		artifacts = ArtifactPathsFromDir(mode.run.ArtifactPath)
	} else {
		artifacts, err = ArtifactPathsForRun(opts.Layout, req.PRRef, pr, req.ProfileName, postingKey(req.PostingIdentity), runID)
		if err != nil {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(artifacts.AgentLogsDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("pipeline: create agent log dir: %w", err)
	}

	quota, quotaSupported, err := opts.Adapter.Quota(ctx)
	if err != nil {
		return Result{}, err
	}
	diff, err := opts.Provider.GetDiff(ctx, req.PRRef)
	if err != nil {
		return Result{}, err
	}
	parsed, err := parseUnifiedDiff(diff.Raw)
	if err != nil {
		return Result{}, err
	}
	threads, err := opts.Provider.ListInlineThreads(ctx, req.PRRef)
	if err != nil {
		return Result{}, err
	}
	catalog, err := agents.Load(ctx, agents.LoadOptions{
		ProfileDirs: append([]string(nil), req.Profile.AgentSources...),
		Repo:        &agents.RepoSource{Reader: opts.Provider, Ref: req.PRRef, PR: pr},
		FlagDirs:    append([]string(nil), req.AgentDirs...),
	})
	if err != nil {
		return Result{}, err
	}

	result := Result{
		PR:               pr,
		PRKey:            prKey,
		Artifacts:        artifacts,
		Quota:            quota,
		QuotaSupported:   quotaSupported,
		QuotaLow:         quotaSupported && (quota.BlockRemainingPct >= 0 && quota.BlockRemainingPct < 5 || quota.WeeklyRemainingPct >= 0 && quota.WeeklyRemainingPct < 5),
		Catalog:          catalog,
		EffectiveCaps:    effectiveCaps(opts.Provider.Capabilities(), req.NoResolveThreads),
		AgentDefsChanged: agentDefinitionsChanged(parsed.Patches),
	}

	var sessionDrafts []sessionDraft
	findingSession := map[review.FindingID]string{}
	model, effort := orchestratorModel(catalog)

	if len(parsed.Patches) == 0 {
		plan, err := opts.buildPlan(req, pr, mode.planPostMode, result.EffectiveCaps, reviewplan.Diff{}, nil, review.Rollup{}, nil, true, result.AgentDefsChanged)
		if err != nil {
			return Result{}, err
		}
		result.Plan = plan
	} else {
		selectionPrompt, err := buildSelectionPrompt(pr, catalog, parsed.Patches, threads)
		if err != nil {
			return Result{}, err
		}
		if err := opts.checkPromptBudget("selection", "", model, "", selectionPrompt); err != nil {
			return Result{}, err
		}
		selectionLog, err := artifacts.AgentLog("orchestrator-selection")
		if err != nil {
			return Result{}, err
		}
		selection, selectionSession, err := runStructured(ctx, opts, ledger.SessionRoleOrchestrator, nil, model, effort, selectionLog, selectionPrompt, func(data []byte) (llm.Selection, error) {
			return llm.DecodeSelection(data, llm.SelectionOptions{
				KnownAgents:  knownAgents(catalog),
				ChangedFiles: changedFiles(parsed.Patches),
				KnownThreads: knownThreads(threads),
			})
		})
		if err != nil {
			return Result{}, err
		}
		if len(selection.SelectedAgents) > maxAgents {
			return Result{}, fmt.Errorf("pipeline: selected agents %d exceeds max %d", len(selection.SelectedAgents), maxAgents)
		}
		result.Selection = selection
		sessionDrafts = append(sessionDrafts, selectionSession)

		findings, reviewerSessions, reviewerFindingSessions, err := runReviewers(ctx, opts, req, pr, catalog, parsed, artifacts, selection, maxConcurrency)
		if err != nil {
			return Result{}, err
		}
		result.Findings = findings
		sessionDrafts = append(sessionDrafts, reviewerSessions...)
		for id, rowID := range reviewerFindingSessions {
			findingSession[id] = rowID
		}

		rollupPrompt, err := buildRollupPrompt(pr, findings)
		if err != nil {
			return Result{}, err
		}
		if err := opts.checkPromptBudget("rollup", "", model, "", rollupPrompt); err != nil {
			return Result{}, err
		}
		rollupLog, err := artifacts.AgentLog("orchestrator-rollup")
		if err != nil {
			return Result{}, err
		}
		rollup, rollupSession, err := runStructured(ctx, opts, ledger.SessionRoleOrchestrator, nil, model, effort, rollupLog, rollupPrompt, func(data []byte) (review.Rollup, error) {
			return llm.DecodeRollup(data, llm.RollupOptions{
				FindingSeverities:         findingSeverities(findings),
				MajorEventRequestsChanges: req.MajorRequestChanges,
			})
		})
		if err != nil {
			return Result{}, err
		}
		result.Rollup = rollup
		sessionDrafts = append(sessionDrafts, rollupSession)

		plan, err := opts.buildPlan(req, pr, mode.planPostMode, result.EffectiveCaps, parsed.PlanDiff, findings, rollup, selection.ThreadActions, false, result.AgentDefsChanged)
		if err != nil {
			return Result{}, err
		}
		result.Plan = plan
	}

	run := mode.run
	if !mode.live {
		run, err = opts.Store.AllocateRun(ctx, ledger.AllocateRunParams{
			PRKey:           prKey,
			PRURL:           req.PRURL,
			RunID:           runID,
			SHA:             pr.Head.SHA,
			BaseSHA:         pr.Base.SHA,
			Profile:         req.ProfileName,
			PostingIdentity: postingKey(req.PostingIdentity),
			PostMode:        ledger.PostModeDryRun,
			StartedAt:       now,
			ArtifactPath:    artifacts.Dir,
		})
		if err != nil {
			return Result{}, err
		}
	}
	result.Run = run
	if !mode.live {
		defer func() {
			if !completed {
				_ = opts.Store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeFailed, opts.now())
			}
		}()
	}

	for _, draft := range sessionDrafts {
		session := draft.toLedger(run.RunID)
		if err := opts.Store.InsertSession(ctx, session); err != nil {
			return Result{}, err
		}
		result.Sessions = append(result.Sessions, session)
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
	if err := writeArtifacts(artifacts, diff.Raw, parsed.Patches, result.Selection, result.Findings, result.Plan.RollupMarkdown); err != nil {
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

func sessionRowIDForFinding(finding reviewplan.AnchoredFinding, findingSession map[review.FindingID]string) (string, error) {
	rowID := strings.TrimSpace(findingSession[finding.FindingID])
	if rowID == "" {
		return "", fmt.Errorf("pipeline: missing reviewer session row for finding %q", finding.FindingID)
	}
	return rowID, nil
}

func runReviewers(ctx context.Context, opts Options, req Request, pr gitprovider.PR, catalog agents.Catalog, parsed ParsedDiff, artifacts ArtifactPaths, selection llm.Selection, maxConcurrency int) ([]review.Finding, []sessionDraft, map[review.FindingID]string, error) {
	type job struct {
		selected llm.SelectedAgent
		agent    agents.Agent
	}
	var jobs []job
	for _, selected := range selection.SelectedAgents {
		agent, ok := catalog.Find(selected.AgentID)
		if !ok {
			return nil, nil, nil, fmt.Errorf("pipeline: selected agent %q not found", selected.AgentID)
		}
		jobs = append(jobs, job{selected: selected, agent: agent})
	}
	if len(jobs) == 0 {
		return nil, nil, nil, nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].agent.ID < jobs[j].agent.ID })

	reviewCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var allFindings []review.Finding
	var sessions []sessionDraft
	findingSessions := map[review.FindingID]string{}
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
			findings, session, err := runReviewer(reviewCtx, opts, req, pr, parsed, artifacts, current.selected, current.agent)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			sessions = append(sessions, session)
			for _, finding := range findings {
				allFindings = append(allFindings, finding)
				findingSessions[finding.ID] = session.rowID
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, nil, firstErr
	}
	sort.Slice(allFindings, func(i, j int) bool { return allFindings[i].ID < allFindings[j].ID })
	return allFindings, sessions, findingSessions, nil
}

func runReviewer(ctx context.Context, opts Options, req Request, pr gitprovider.PR, parsed ParsedDiff, artifacts ArtifactPaths, selected llm.SelectedAgent, agent agents.Agent) ([]review.Finding, sessionDraft, error) {
	prompt, err := buildReviewerPrompt(ctx, opts, req, pr, parsed, selected, agent)
	if err != nil {
		return nil, sessionDraft{}, err
	}
	if err := opts.checkPromptBudget("reviewer", agent.ID, agent.Model, strings.Join(selected.Files, ","), prompt); err != nil {
		return nil, sessionDraft{}, err
	}
	logPath, err := artifacts.AgentLog(agent.ID)
	if err != nil {
		return nil, sessionDraft{}, err
	}
	agentID := agent.ID
	findings, session, err := runStructured(ctx, opts, ledger.SessionRoleReviewer, &agentID, agent.Model, agent.Effort, logPath, prompt, func(data []byte) (llm.Findings, error) {
		return llm.DecodeFindings(data, llm.FindingsOptions{
			KnownAgents:  map[string]bool{agent.ID: true},
			ChangedFiles: changedFiles(parsed.Patches),
			NewFindingID: opts.newFindingID,
		})
	})
	if err != nil {
		return nil, sessionDraft{}, err
	}
	return findings.Findings, session, nil
}

func runStructured[T any](ctx context.Context, opts Options, role ledger.SessionRole, agentID *string, model, effort, logPath, prompt string, decode llm.Decoder[T]) (T, sessionDraft, error) {
	started := opts.now()
	result, err := llm.RunStructuredWithSession(ctx, opts.Adapter, llm.Request{
		Model:   model,
		Effort:  effort,
		Prompt:  prompt,
		LogPath: logPath,
	}, decode)
	completed := opts.now()
	draft := sessionDraft{
		rowID:             opts.newSessionRowID(),
		providerSessionID: result.SessionID,
		role:              role,
		agentID:           agentID,
		adapter:           opts.Adapter.Name(),
		model:             model,
		effort:            effort,
		startedAt:         started,
		completedAt:       completed,
		response:          result.Response,
	}
	if strings.TrimSpace(draft.providerSessionID) == "" {
		draft.providerSessionID = draft.rowID
	}
	if strings.TrimSpace(draft.model) == "" {
		draft.model = "default"
	}
	return result.Value, draft, err
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

func (opts Options) buildPlan(req Request, pr gitprovider.PR, postMode reviewplan.PostMode, caps reviewplan.ProviderCaps, diff reviewplan.Diff, findings []review.Finding, rollup review.Rollup, threadActions []review.ThreadAction, noDiff bool, agentDefsChanged bool) (reviewplan.Plan, error) {
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
		Now:                     opts.now,
		NewActionID:             opts.newActionID,
	})
}

func buildReviewerPrompt(ctx context.Context, opts Options, req Request, pr gitprovider.PR, parsed ParsedDiff, selected llm.SelectedAgent, agent agents.Agent) (string, error) {
	patchesByPath := map[string]FilePatch{}
	for _, patch := range parsed.Patches {
		patchesByPath[patch.Path] = patch
		if patch.OldPath != "" {
			patchesByPath[patch.OldPath] = patch
		}
	}
	type fileContext struct {
		Path        string `json:"path"`
		Diff        string `json:"diff"`
		BaseContent string `json:"base_content,omitempty"`
		HeadContent string `json:"head_content,omitempty"`
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
			if err := opts.checkPromptBudget("full-content", agent.ID, agent.Model, basePath, string(baseBytes)); err != nil {
				return "", err
			}
			headBytes, err := fetchFileOptional(ctx, opts.Provider, req.PRRef, pr.Head.SHA, patch.Path)
			if err != nil {
				return "", err
			}
			if err := opts.checkPromptBudget("full-content", agent.ID, agent.Model, patch.Path, string(headBytes)); err != nil {
				return "", err
			}
			fc.BaseContent = string(baseBytes)
			fc.HeadContent = string(headBytes)
		}
		files = append(files, fc)
	}
	payload := map[string]any{
		"task":   "review files and return findings JSON only",
		"agent":  agent,
		"files":  files,
		"schema": "findings",
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func fetchFileOptional(ctx context.Context, provider ReadProvider, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	data, err := provider.GetFileAtRef(ctx, ref, gitRef, path)
	if errors.Is(err, gitprovider.ErrNotFound) {
		return nil, nil
	}
	return data, err
}

func buildSelectionPrompt(pr gitprovider.PR, catalog agents.Catalog, patches []FilePatch, threads []gitprovider.InlineThread) (string, error) {
	payload := map[string]any{
		"task":          "select reviewer agents and thread actions; return selection JSON only",
		"schema":        "selection",
		"pr":            pr,
		"agents":        catalog.Agents,
		"changed_files": patchPaths(patches),
		"threads":       threads,
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build selection prompt: %w", err)
	}
	return string(body), nil
}

func buildRollupPrompt(pr gitprovider.PR, findings []review.Finding) (string, error) {
	payload := map[string]any{
		"task":     "dedupe findings and return rollup JSON only",
		"schema":   "rollup",
		"pr":       pr,
		"findings": findings,
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build rollup prompt: %w", err)
	}
	return string(body), nil
}

func writeArtifacts(paths ArtifactPaths, rawDiff string, patches []FilePatch, selection llm.Selection, findings []review.Finding, rollup string) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create slices dir: %w", err)
	}
	if err := os.WriteFile(paths.DiffPatch, []byte(rawDiff), 0o600); err != nil {
		return fmt.Errorf("pipeline: write diff: %w", err)
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
	if opts.Provider == nil {
		return fmt.Errorf("pipeline: provider is required")
	}
	if opts.Adapter == nil {
		return fmt.Errorf("pipeline: adapter is required")
	}
	if opts.Store == nil {
		return fmt.Errorf("pipeline: store is required")
	}
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("pipeline: data root is required")
	}
	if err := req.PRRef.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(req.PRURL) == "" {
		return fmt.Errorf("pipeline: PR URL is required")
	}
	if strings.TrimSpace(req.ProfileName) == "" {
		return fmt.Errorf("pipeline: profile is required")
	}
	if strings.TrimSpace(postingKey(req.PostingIdentity)) == "" {
		return fmt.Errorf("pipeline: posting identity is required")
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
		Dir:            dir,
		DiffPatch:      filepath.Join(dir, "diff.patch"),
		SlicesDir:      filepath.Join(dir, "slices"),
		FindingsJSON:   filepath.Join(dir, "findings.json"),
		RollupMarkdown: filepath.Join(dir, "rollup.md"),
		AgentLogsDir:   filepath.Join(dir, "agent-logs"),
	}, nil
}

// ArtifactPathsFromDir returns the artifact path set rooted at dir.
func ArtifactPathsFromDir(dir string) ArtifactPaths {
	return ArtifactPaths{
		Dir:            dir,
		DiffPatch:      filepath.Join(dir, "diff.patch"),
		SlicesDir:      filepath.Join(dir, "slices"),
		FindingsJSON:   filepath.Join(dir, "findings.json"),
		RollupMarkdown: filepath.Join(dir, "rollup.md"),
		AgentLogsDir:   filepath.Join(dir, "agent-logs"),
	}
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

func orchestratorModel(catalog agents.Catalog) (string, string) {
	for _, agent := range catalog.Agents {
		if strings.TrimSpace(agent.Model) != "" || strings.TrimSpace(agent.Effort) != "" {
			return agent.Model, agent.Effort
		}
	}
	return "", ""
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
