// Package reviewcmd wires the `cr review` command surface.
package reviewcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/reviewruntime"
	"github.com/open-cli-collective/codereview-cli/internal/version"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const reviewLong = `Run an automated pull-request review.

Live review checks local and host state before starting the reviewer loop. By
default, if the posting identity has already approved the PR, cr exits before
any LLM classifier or reviewer work, even if newer commits made that approval
stale. Use --rerun to bypass this and force a fresh live review.

If no existing approval is present and a prior codereview marker exists from
the posting identity, cr can fast-path approval when the PR author posts an
approval override request newer than that marker. Candidate comments are
filtered in Go; the small-tier classifier only decides whether the filtered
comments ask for override approval.

--retry-posts is recovery-only: it retries missing or failed required posts for
an existing run and does not check existing approvals or approval overrides.`

// RuntimeFactory builds the concrete runtime used by review lifecycle commands.
type RuntimeFactory func(context.Context, reviewruntime.OpenRequest) (reviewruntime.Runtime, error)

type commandFlags struct {
	dryRun            bool
	noPost            bool
	rerun             bool
	retryPosts        bool
	agentsDirs        []string
	failOn            string
	sessionName       string
	jsonOutput        bool
	selectionModel    string
	selectionEffort   string
	selectionPrompt   string
	reviewerModel     string
	reviewerModelTier string
	reviewerEffort    string
	reviewBaseSHA     string
	reviewHeadSHA     string
	maxAgents         int
	maxConcurrency    int
	allowSelfReview   bool
	allowSelfApprove  bool
	noResolveThreads  bool
}

// Register attaches the review command to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, reviewruntime.Open)
}

// RegisterWithFactory attaches the review command with an injected runtime factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory RuntimeFactory) {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "review <PR>",
		Short: "Run an automated pull-request review",
		Long:  reviewLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("review requires one PR URL"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReview(cmd.Context(), cmd, opts, factory, flags, args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Plan review actions without posting")
	cmd.Flags().BoolVar(&flags.noPost, "no-post", false, "Alias for --dry-run")
	cmd.Flags().BoolVar(&flags.rerun, "rerun", false, "Bypass approval/override, resume, and marker gates and run a fresh live review")
	cmd.Flags().BoolVar(&flags.retryPosts, "retry-posts", false, "Retry missing or failed required posts without rerunning review or checking approval overrides")
	cmd.Flags().StringArrayVar(&flags.agentsDirs, "agents-dir", nil, "Additional trusted agents directory")
	cmd.Flags().StringVar(&flags.failOn, "fail-on", "", "Exit 1 when a finding at or above severity exists")
	cmd.Flags().StringVar(&flags.sessionName, "session", "", "Named LLM session to reuse for live reviews")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	cmd.Flags().StringVar(&flags.selectionModel, "selection-model", "", "Override selection model for dry-run review")
	cmd.Flags().StringVar(&flags.selectionEffort, "selection-effort", "", "Override selection effort for dry-run review")
	cmd.Flags().StringVar(&flags.selectionPrompt, "selection-prompt", "", "Override selection instructions from a file for dry-run review")
	cmd.Flags().StringVar(&flags.reviewerModel, "reviewer-model", "", "Override reviewer models for dry-run review")
	cmd.Flags().StringVar(&flags.reviewerModelTier, "reviewer-model-tier", "", "Override reviewer baseline model tier for dry-run review")
	cmd.Flags().StringVar(&flags.reviewerEffort, "reviewer-effort", "", "Override reviewer effort for dry-run review")
	cmd.Flags().StringVar(&flags.reviewBaseSHA, "review-base-sha", "", "Review this base commit SHA instead of the PR's current base SHA; requires --dry-run and --review-head-sha")
	cmd.Flags().StringVar(&flags.reviewHeadSHA, "review-head-sha", "", "Review this head commit SHA instead of the PR's current head SHA; requires --dry-run and --review-base-sha")
	cmd.Flags().IntVar(&flags.maxAgents, "max-agents", 0, "Maximum selected reviewer agents")
	cmd.Flags().IntVar(&flags.maxConcurrency, "max-concurrency", 0, "Maximum concurrent reviewer agents")
	cmd.Flags().BoolVar(&flags.allowSelfReview, "allow-self-review", false, "Allow reviewer credentials matching the PR author")
	cmd.Flags().BoolVar(&flags.allowSelfApprove, "allow-self-approve", false, "Allow approval when posting identity is the PR author")
	cmd.Flags().BoolVar(&flags.noResolveThreads, "no-resolve-threads", false, "Do not plan thread-resolution actions")
	rootCmd.AddCommand(cmd)
}

func runReview(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory RuntimeFactory, flags commandFlags, prArg string) error {
	if flags.noPost {
		flags.dryRun = true
	}
	selectionModelChanged := cmd.Flags().Changed("selection-model")
	selectionEffortChanged := cmd.Flags().Changed("selection-effort")
	selectionPromptChanged := cmd.Flags().Changed("selection-prompt")
	reviewerModelChanged := cmd.Flags().Changed("reviewer-model")
	reviewerModelTierChanged := cmd.Flags().Changed("reviewer-model-tier")
	reviewerEffortChanged := cmd.Flags().Changed("reviewer-effort")
	reviewBaseChanged := cmd.Flags().Changed("review-base-sha")
	reviewHeadChanged := cmd.Flags().Changed("review-head-sha")
	selectionModel := strings.TrimSpace(flags.selectionModel)
	selectionEffort := strings.TrimSpace(flags.selectionEffort)
	selectionPromptPath := strings.TrimSpace(flags.selectionPrompt)
	reviewerModel := strings.TrimSpace(flags.reviewerModel)
	reviewerModelTier := strings.TrimSpace(flags.reviewerModelTier)
	reviewerEffort := strings.TrimSpace(flags.reviewerEffort)
	reviewBaseSHA := strings.TrimSpace(flags.reviewBaseSHA)
	reviewHeadSHA := strings.TrimSpace(flags.reviewHeadSHA)
	if selectionModelChanged && selectionModel == "" {
		return exitcode.Usage(fmt.Errorf("--selection-model must be non-empty"))
	}
	if selectionEffortChanged && selectionEffort == "" {
		return exitcode.Usage(fmt.Errorf("--selection-effort must be non-empty"))
	}
	if selectionEffortChanged && !validModelEffort(selectionEffort) {
		return exitcode.Usage(fmt.Errorf("--selection-effort must be one of low, medium, high"))
	}
	if selectionPromptChanged && selectionPromptPath == "" {
		return exitcode.Usage(fmt.Errorf("--selection-prompt must be non-empty"))
	}
	if reviewerModelChanged && reviewerModel == "" {
		return exitcode.Usage(fmt.Errorf("--reviewer-model must be non-empty"))
	}
	if reviewerModelTierChanged && reviewerModelTier == "" {
		return exitcode.Usage(fmt.Errorf("--reviewer-model-tier must be non-empty"))
	}
	if reviewerModelChanged && reviewerModelTierChanged {
		return exitcode.Usage(fmt.Errorf("--reviewer-model and --reviewer-model-tier cannot be used together"))
	}
	if reviewerModelTierChanged && !config.ModelTier(reviewerModelTier).Valid() {
		return exitcode.Usage(fmt.Errorf("--reviewer-model-tier must be one of small, medium, large"))
	}
	if reviewerEffortChanged && reviewerEffort == "" {
		return exitcode.Usage(fmt.Errorf("--reviewer-effort must be non-empty"))
	}
	if reviewerEffortChanged && !validModelEffort(reviewerEffort) {
		return exitcode.Usage(fmt.Errorf("--reviewer-effort must be one of low, medium, high"))
	}
	stageOverrideChanged := selectionModelChanged || selectionEffortChanged || selectionPromptChanged || reviewerModelChanged || reviewerModelTierChanged || reviewerEffortChanged
	if stageOverrideChanged && !flags.dryRun {
		return exitcode.Usage(fmt.Errorf("--selection-model, --selection-effort, --selection-prompt, --reviewer-model, --reviewer-model-tier, and --reviewer-effort require --dry-run or --no-post"))
	}
	if reviewBaseChanged != reviewHeadChanged {
		return exitcode.Usage(fmt.Errorf("--review-base-sha and --review-head-sha must be set together"))
	}
	if reviewBaseChanged {
		if err := validateReviewSHAFlag("--review-base-sha", reviewBaseSHA); err != nil {
			return exitcode.Usage(err)
		}
		if err := validateReviewSHAFlag("--review-head-sha", reviewHeadSHA); err != nil {
			return exitcode.Usage(err)
		}
		if !flags.dryRun {
			return exitcode.Usage(fmt.Errorf("--review-base-sha and --review-head-sha require --dry-run or --no-post"))
		}
	}
	if flags.rerun && flags.retryPosts {
		return exitcode.Usage(fmt.Errorf("--rerun and --retry-posts are mutually exclusive"))
	}
	sessionName := strings.TrimSpace(flags.sessionName)
	if sessionName != "" && (flags.dryRun || flags.noPost) {
		return exitcode.Usage(fmt.Errorf("--session requires live review and cannot be used with --dry-run or --no-post"))
	}
	if sessionName != "" && flags.retryPosts {
		return exitcode.Usage(fmt.Errorf("--session cannot be used with --retry-posts"))
	}
	if flags.maxAgents < 0 {
		return exitcode.Usage(fmt.Errorf("--max-agents must be non-negative"))
	}
	if flags.maxConcurrency < 0 {
		return exitcode.Usage(fmt.Errorf("--max-concurrency must be non-negative"))
	}
	selectionPromptInstructions := ""
	if selectionPromptChanged {
		var err error
		selectionPromptInstructions, err = loadSelectionPromptOverride(selectionPromptPath)
		if err != nil {
			return exitcode.Usage(err)
		}
	}
	var failOn *review.Severity
	if strings.TrimSpace(flags.failOn) != "" {
		threshold, err := review.ParseSeverity(flags.failOn)
		if err != nil {
			return exitcode.Usage(err)
		}
		failOn = &threshold
	}

	logger := newProgressLogger(opts)
	configSpan := logger.Start("review", "load_config", "config")
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return exitcode.AuthConfig(endProgressSpan(configSpan, err))
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cmderr.Config(endProgressSpan(configSpan, err))
	}
	configSpan.End(nil)
	parseSpan := logger.Start("review", "parse_pr", "pr")
	ref, err := prref.ParseGitHubPullURL(prArg)
	if err != nil {
		return exitcode.Usage(endProgressSpan(parseSpan, err))
	}
	parseSpan.End(nil)
	profileSpan := logger.Start("review", "resolve_profile", "profile")
	profileName, profile, err := config.ResolveProfileForRepository(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
		Host:      ref.Host,
		Namespace: ref.Owner,
		Repo:      ref.Repo,
	})
	if err != nil {
		return cmderr.Config(endProgressSpan(profileSpan, err))
	}
	if !prref.SameHost(ref.Host, profile.Git.Host) {
		return exitcode.Usage(endProgressSpan(profileSpan, fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host)))
	}
	profileSpan.End(nil)

	runtimeReq := reviewruntime.OpenRequest{
		Config:                            cfg,
		Profile:                           profile,
		Backend:                           opts.Backend,
		BackendFlagChanged:                cmderr.BackendFlagChanged(cmd),
		Command:                           commandName(cmd),
		Progress:                          logger,
		Warnings:                          opts.Stderr,
		MaxAgents:                         flags.maxAgents,
		MaxConcurrency:                    flags.maxConcurrency,
		PRRef:                             ref,
		RequireOpinionatedReviewAuthority: !flags.dryRun,
		Retention:                         cmdruntime.RetentionPolicyFromConfig(cfg.Data.Retention),
		RetentionManualOnly:               cfg.Data.Retention.Enforcement == config.RetentionManualOnly,
		ResolveRepoRoot:                   cmdruntime.ResolveRepoRoot,
	}
	runtimeSpan := logger.Start("review", "build_runtime", "runtime")
	runtime, err := factory(ctx, runtimeReq)
	if err != nil {
		return endProgressSpan(runtimeSpan, cmdruntime.MapRunError(err))
	}
	runtimeSpan.End(nil)
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Runner == nil {
		return fmt.Errorf("review: runtime runner is required")
	}
	noResolve := flags.noResolveThreads || profile.ReviewPolicy.ResolveThreads == config.ResolveThreadsNever
	pipelineReq := pipeline.Request{
		PRRef:                       ref,
		PRURL:                       prArg,
		ProfileName:                 profileName,
		SessionName:                 sessionName,
		Profile:                     profile,
		PostingIdentity:             runtime.PostingIdentity,
		AgentDirs:                   append([]string(nil), flags.agentsDirs...),
		FailOn:                      failOn,
		AllowSelfReview:             flags.allowSelfReview,
		AllowSelfApprove:            flags.allowSelfApprove,
		NoResolveThreads:            noResolve,
		MajorRequestChanges:         profile.ReviewPolicy.MajorEvent == config.ReviewMajorEventRequestChanges,
		SelectionModelOverride:      selectionModel,
		SelectionEffortOverride:     selectionEffort,
		SelectionPromptInstructions: selectionPromptInstructions,
		ReviewerModelOverride:       reviewerModel,
		ReviewerModelTierOverride:   reviewerModelTier,
		ReviewerEffortOverride:      reviewerEffort,
		ReviewBaseSHA:               reviewBaseSHA,
		ReviewHeadSHA:               reviewHeadSHA,
		Rerun:                       flags.rerun,
		ToolVersion:                 version.Version,
	}
	if !flags.dryRun {
		return runLive(ctx, logger, opts, flags, runtime.Runner, pipelineReq, failOn)
	}

	execSpan := logger.Start("review", "execute_dry_run", "pr")
	result, err := runtime.Runner.DryRun(ctx, pipelineReq)
	if err != nil {
		return endProgressSpan(execSpan, cmdruntime.MapRunError(err))
	}
	execSpan.End(nil)
	renderSpan := logger.Start("review", "render_result", "stdout")
	rendered, err := newReviewDryRun(result)
	if err != nil {
		return endProgressSpan(renderSpan, err)
	}
	if flags.jsonOutput {
		err = view.RenderReviewDryRunJSON(opts.Stdout, rendered)
	} else {
		err = view.RenderReviewDryRunText(opts.Stdout, rendered)
	}
	if err != nil {
		return endProgressSpan(renderSpan, err)
	}
	renderSpan.End(nil)
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
}

func validModelEffort(value string) bool {
	return modelprefs.Effort(value).Valid()
}

func loadSelectionPromptOverride(rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", fmt.Errorf("--selection-prompt must be non-empty")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--selection-prompt must resolve to a readable file: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("--selection-prompt must reference a readable file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("--selection-prompt must reference a file, not a directory")
	}
	data, err := os.ReadFile(absPath) // #nosec G304 -- user-selected prompt override path is explicit CLI input.
	if err != nil {
		return "", fmt.Errorf("--selection-prompt must reference a readable file: %w", err)
	}
	instructions := strings.TrimSpace(string(data))
	if instructions == "" {
		return "", fmt.Errorf("--selection-prompt file must contain non-empty prompt text")
	}
	return instructions, nil
}

var reviewSHAFlagPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func validateReviewSHAFlag(name, sha string) error {
	if sha == "" {
		return fmt.Errorf("%s must be non-empty", name)
	}
	if !reviewSHAFlagPattern.MatchString(sha) {
		return fmt.Errorf("%s must be a 7-64 character hex SHA", name)
	}
	return nil
}

func commandName(cmd *cobra.Command) string {
	if cmd == nil {
		return "review"
	}
	name := strings.TrimSpace(cmd.Name())
	if name == "" {
		return "review"
	}
	return name
}

func runLive(ctx context.Context, logger *progress.Logger, opts *root.Options, flags commandFlags, runner reviewruntime.Runner, req pipeline.Request, failOn *review.Severity) error {
	execSpan := logger.Start("review", "execute_live", "pr")
	result, err := runner.Live(ctx, req, reviewrun.Flags{Rerun: flags.rerun, RetryPosts: flags.retryPosts})
	if err != nil {
		return endProgressSpan(execSpan, cmdruntime.MapRunError(err))
	}
	execSpan.End(nil)
	renderSpan := logger.Start("review", "render_result", "stdout")
	rendered := newReviewLive(result)
	if flags.jsonOutput {
		err = view.RenderReviewLiveJSON(opts.Stdout, rendered)
	} else {
		err = view.RenderReviewLiveText(opts.Stdout, rendered)
	}
	if err != nil {
		return endProgressSpan(renderSpan, err)
	}
	renderSpan.End(nil)
	if result.ExitCode != exitcode.Success {
		return exitcode.With(result.ExitCode, liveResultError(result))
	}
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
}

func newReviewSummary(summary reviewplan.Summary) view.ReviewSummary {
	// Arrays serialize as [], never null, so JSON consumers see one shape.
	out := view.ReviewSummary{
		Reviewers: []view.ReviewReviewerSummary{},
		Threads: view.ReviewThreadCounts{
			Considered: summary.Threads.Considered,
			Summarized: summary.Threads.Summarized,
			Resolved:   summary.Threads.Resolved,
		},
		Run: view.ReviewRunSummary{
			ToolVersion:       summary.Run.ToolVersion,
			Adapter:           summary.Run.Adapter,
			Model:             summary.Run.Model,
			PostingIdentity:   summary.Run.PostingIdentity,
			SelectedReviewers: summary.Run.SelectedReviewers,
			ReviewerCoverage:  []view.ReviewReviewerCoverageSummary{},
			WallDurationMS:    summary.Run.WallDurationMS,
			Workstreams:       []view.ReviewWorkstream{},
		},
		Totals: view.ReviewWorkstreamTotals{
			TokensIn:          summary.Totals.TokensIn,
			TokensOut:         summary.Totals.TokensOut,
			CacheRead:         summary.Totals.CacheRead,
			CacheCreate:       summary.Totals.CacheCreate,
			CostUSD:           summary.Totals.CostUSD,
			ComputeDurationMS: summary.Totals.ComputeDurationMS,
		},
	}
	for _, reviewer := range summary.Reviewers {
		out.Reviewers = append(out.Reviewers, view.ReviewReviewerSummary{Name: reviewer.Name, Findings: reviewer.Findings})
	}
	for _, coverage := range summary.Run.ReviewerCoverage {
		out.Run.ReviewerCoverage = append(out.Run.ReviewerCoverage, view.ReviewReviewerCoverageSummary{
			AgentID:        coverage.AgentID,
			Status:         coverage.Status,
			Scope:          coverage.Scope,
			InspectedFiles: coverage.InspectedFiles,
			SkippedFiles:   coverage.SkippedFiles,
			Constraints:    coverage.Constraints,
			Diagnostic:     coverage.Diagnostic,
		})
	}
	for _, workstream := range summary.Run.Workstreams {
		out.Run.Workstreams = append(out.Run.Workstreams, view.ReviewWorkstream{
			Name:        workstream.Name,
			Model:       workstream.Model,
			TokensIn:    workstream.TokensIn,
			TokensOut:   workstream.TokensOut,
			CacheRead:   workstream.CacheRead,
			CacheCreate: workstream.CacheCreate,
			CostUSD:     workstream.CostUSD,
			DurationMS:  workstream.DurationMS,
		})
	}
	return out
}

func newReviewDryRun(result pipeline.Result) (view.ReviewDryRun, error) {
	outcome := ledger.OutcomeDryRun.String()
	if result.Run.Outcome != nil {
		outcome = result.Run.Outcome.String()
	}
	rendered := view.ReviewDryRun{
		Run: view.ReviewRun{
			RunID:        result.Run.RunID,
			PRURL:        result.PR.URL,
			PRKey:        result.PRKey,
			PostMode:     result.Run.PostMode.String(),
			Outcome:      outcome,
			ArtifactPath: result.Run.ArtifactPath,
			BaseSHA:      result.ReviewBaseSHA,
			HeadSHA:      result.ReviewHeadSHA,
		},
		RollupMarkdown:  result.Plan.RollupMarkdown,
		Summary:         newReviewSummary(result.Plan.Summary),
		FailOnTriggered: result.FailOnTriggered,
		Artifacts: view.ReviewArtifacts{
			Dir:            result.Artifacts.Dir,
			DiffPatch:      result.Artifacts.DiffPatch,
			SlicesDir:      result.Artifacts.SlicesDir,
			FindingsJSON:   result.Artifacts.FindingsJSON,
			RollupMarkdown: result.Artifacts.RollupMarkdown,
			AgentLogsDir:   result.Artifacts.AgentLogsDir,
		},
	}
	if result.QuotaSupported {
		rendered.Quota = &view.ReviewQuota{
			BlockRemainingPct:  result.Quota.BlockRemainingPct,
			WeeklyRemainingPct: result.Quota.WeeklyRemainingPct,
			Low:                result.QuotaLow,
		}
	}
	if result.CurrentBaseSHA != "" && result.CurrentBaseSHA != result.ReviewBaseSHA {
		rendered.Run.CurrentBaseSHA = result.CurrentBaseSHA
	}
	if result.CurrentHeadSHA != "" && result.CurrentHeadSHA != result.ReviewHeadSHA {
		rendered.Run.CurrentHeadSHA = result.CurrentHeadSHA
	}
	for _, finding := range result.Plan.AnchoredFindings {
		rendered.Findings = append(rendered.Findings, viewFinding(finding))
	}
	planned := map[string]ledger.PlannedAction{}
	for _, action := range result.PlannedActions {
		planned[action.ActionID] = action
	}
	for _, action := range result.Plan.Actions {
		renderedAction, err := viewAction(action, planned[action.ActionID])
		if err != nil {
			return view.ReviewDryRun{}, err
		}
		rendered.Actions = append(rendered.Actions, renderedAction)
	}
	return rendered, nil
}

func newReviewLive(result reviewrun.Result) view.ReviewLive {
	outcome := result.Outbox.Outcome.String()
	if outcome == "" && result.Run.Outcome != nil {
		outcome = result.Run.Outcome.String()
	}
	outboxExitCode := result.Outbox.ExitCode
	if outboxExitCode == 0 && result.ExitCode != 0 {
		outboxExitCode = result.ExitCode
	}
	rendered := view.ReviewLive{
		Run: view.ReviewRun{
			RunID:        result.Run.RunID,
			PRURL:        result.PR.URL,
			PRKey:        result.PRKey,
			PostMode:     result.Run.PostMode.String(),
			Outcome:      outcome,
			ArtifactPath: result.Run.ArtifactPath,
		},
		Status:          string(result.Status),
		Decision:        string(result.Decision.Kind),
		Message:         result.Message,
		FailOnTriggered: result.FailOnTriggered,
		Outbox: view.ReviewOutbox{
			Outcome:        outcome,
			ExitCode:       outboxExitCode,
			Posted:         result.Outbox.Posted,
			Pending:        result.Outbox.Pending,
			FailedTerminal: result.Outbox.FailedTerminal,
			Aborted:        result.Outbox.Aborted,
		},
	}
	if result.Pipeline != nil {
		rendered.Run.BaseSHA = result.Pipeline.ReviewBaseSHA
		rendered.Run.HeadSHA = result.Pipeline.ReviewHeadSHA
		if result.Pipeline.CurrentBaseSHA != "" && result.Pipeline.CurrentBaseSHA != result.Pipeline.ReviewBaseSHA {
			rendered.Run.CurrentBaseSHA = result.Pipeline.CurrentBaseSHA
		}
		if result.Pipeline.CurrentHeadSHA != "" && result.Pipeline.CurrentHeadSHA != result.Pipeline.ReviewHeadSHA {
			rendered.Run.CurrentHeadSHA = result.Pipeline.CurrentHeadSHA
		}
		rendered.Artifacts = view.ReviewArtifacts{
			Dir:            result.Pipeline.Artifacts.Dir,
			DiffPatch:      result.Pipeline.Artifacts.DiffPatch,
			SlicesDir:      result.Pipeline.Artifacts.SlicesDir,
			FindingsJSON:   result.Pipeline.Artifacts.FindingsJSON,
			RollupMarkdown: result.Pipeline.Artifacts.RollupMarkdown,
			AgentLogsDir:   result.Pipeline.Artifacts.AgentLogsDir,
		}
	}
	return rendered
}

func liveResultError(result reviewrun.Result) error {
	if strings.TrimSpace(result.Message) != "" {
		return errors.New(result.Message)
	}
	if result.Outbox.Outcome != "" {
		return fmt.Errorf("live review completed with outcome %s", result.Outbox.Outcome)
	}
	return fmt.Errorf("live review did not complete")
}

func viewFinding(finding reviewplan.AnchoredFinding) view.ReviewFinding {
	out := view.ReviewFinding{
		ID:        finding.FindingID.String(),
		Severity:  finding.Severity.String(),
		FilePath:  finding.FilePath,
		Anchoring: finding.Anchoring.String(),
		Body:      finding.Body,
	}
	if finding.Side != nil {
		out.Side = finding.Side.String()
	}
	if finding.Line != nil {
		line := *finding.Line
		out.Line = &line
	}
	return out
}

func viewAction(action reviewplan.Action, planned ledger.PlannedAction) (view.ReviewAction, error) {
	status := action.Status
	payload := json.RawMessage(`{}`)
	if planned.ActionID != "" {
		if parsed := json.RawMessage(planned.PayloadJSON); json.Valid(parsed) {
			payload = parsed
		} else {
			return view.ReviewAction{}, fmt.Errorf("review: planned action %q payload is invalid JSON", planned.ActionID)
		}
		if planned.Status != "" {
			status = reviewplan.ActionStatus(planned.Status.String())
		}
	}
	out := view.ReviewAction{
		ID:            action.ActionID,
		Kind:          string(action.Kind),
		Status:        string(status),
		Required:      action.Required,
		MarkerOmitted: action.Marker.BodyBearing,
		Payload:       payload,
	}
	if action.FindingID.Assigned() {
		out.FindingID = action.FindingID.String()
	}
	if strings.TrimSpace(action.ThreadID) != "" {
		out.ThreadID = action.ThreadID
	}
	return out, nil
}
