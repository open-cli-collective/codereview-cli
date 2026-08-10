// Package reviewcmd wires the `cr review` command surface.
package reviewcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/appruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/version"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const reviewLong = `Run an automated pull-request review.

Live review checks local and host state before starting the reviewer loop. By
default, if the posting identity has already approved the PR, cr exits before
any LLM classifier or reviewer work, even if newer commits made that approval
stale. Use --rerun to bypass these local gates and force a new live review.

Session reuse is independent of local review gates. Plain follow-up reviews and
--rerun reuse the PR's original reviewer cohort and each reviewer's provider
session. --session scopes only the orchestrator conversation. --fresh-session
reselects the reviewer cohort and starts fresh orchestrator and reviewer
conversations without changing local review gates.

--fast requests fast execution for reviewer agents only; --no-fast disables it.
CLI flags override the profile default. Unsupported runtimes or reviewer models
emit a warning and continue at normal speed.

If no existing approval is present and a prior codereview marker exists from
the posting identity, cr can fast-path approval when the PR author posts an
approval override request newer than that marker. Candidate comments are
filtered in Go; the small-tier classifier only decides whether the filtered
comments ask for override approval.

--retry-posts is recovery-only: it retries missing or failed required posts for
an existing run and does not check existing approvals, approval overrides, or
run LLM planning.`

// RuntimeFactory builds the concrete runtime used by review lifecycle commands.
type RuntimeFactory func(context.Context, app.OpenRequest) (app.Runtime, error)

type commandFlags struct {
	dryRun            bool
	noPost            bool
	rerun             bool
	retryPosts        bool
	freshSession      bool
	fast              bool
	noFast            bool
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
	RegisterWithFactory(rootCmd, opts, app.Open)
}

// RegisterWithFactory attaches the review command with an injected runtime factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory RuntimeFactory) {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "review <PR>",
		Short: "Run an automated pull-request review",
		Long:  reviewLong,
		Args:  exitcode.ExactArgs(1, "review requires one PR URL"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReview(cmd.Context(), cmd, opts, factory, flags, args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Plan review actions without posting")
	cmd.Flags().BoolVar(&flags.noPost, "no-post", false, "Alias for --dry-run")
	cmd.Flags().BoolVar(&flags.rerun, "rerun", false, "Bypass local approval/override, resume, and marker gates; reuse reviewer and orchestrator sessions")
	cmd.Flags().BoolVar(&flags.retryPosts, "retry-posts", false, "Retry missing or failed required posts without rerunning review or checking approval overrides")
	cmd.Flags().BoolVar(&flags.freshSession, "fresh-session", false, "Reselect reviewers and start fresh reviewer/orchestrator conversations without changing local review gates")
	cmd.Flags().BoolVar(&flags.fast, "fast", false, "Request fast execution for supported reviewer runtimes and models")
	cmd.Flags().BoolVar(&flags.noFast, "no-fast", false, "Disable fast execution for reviewer agents")
	cmd.Flags().StringArrayVar(&flags.agentsDirs, "agents-dir", nil, "Additional trusted agents directory")
	cmd.Flags().StringVar(&flags.failOn, "fail-on", "", "Exit 1 when a finding at or above severity exists")
	cmd.Flags().StringVar(&flags.sessionName, "session", "", "Override the PR's default orchestrator session with a named live-review session")
	root.AddJSONFlag(cmd, &flags.jsonOutput)
	cmd.Flags().StringVar(&flags.selectionModel, "selection-model", "", "Override selection model for dry-run review")
	cmd.Flags().StringVar(&flags.selectionEffort, "selection-effort", "", "Override selection effort for dry-run review")
	cmd.Flags().StringVar(&flags.selectionPrompt, "selection-prompt", "", "Override selection instructions from a file for dry-run review")
	cmd.Flags().StringVar(&flags.reviewerModel, "reviewer-model", "", "Override reviewer model for this review")
	cmd.Flags().StringVar(&flags.reviewerModelTier, "reviewer-model-tier", "", "Override reviewer baseline model tier for dry-run review")
	cmd.Flags().StringVar(&flags.reviewerEffort, "reviewer-effort", "", "Override reviewer effort for this review")
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
	if cmd.Flags().Changed("fast") && cmd.Flags().Changed("no-fast") {
		return exitcode.Usage(fmt.Errorf("--fast and --no-fast are mutually exclusive"))
	}
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
	if err := validateNonEmptyChangedFlags(cmd,
		[2]string{"selection-model", flags.selectionModel},
		[2]string{"selection-effort", flags.selectionEffort},
	); err != nil {
		return exitcode.Usage(err)
	}
	if selectionEffortChanged && !modelprefs.Effort(selectionEffort).Valid() {
		return exitcode.Usage(fmt.Errorf("--selection-effort must be one of low, medium, high"))
	}
	if err := validateNonEmptyChangedFlags(cmd,
		[2]string{"selection-prompt", flags.selectionPrompt},
		[2]string{"reviewer-model", flags.reviewerModel},
		[2]string{"reviewer-model-tier", flags.reviewerModelTier},
	); err != nil {
		return exitcode.Usage(err)
	}
	if reviewerModelChanged && reviewerModelTierChanged {
		return exitcode.Usage(fmt.Errorf("--reviewer-model and --reviewer-model-tier cannot be used together"))
	}
	if reviewerModelTierChanged && !config.ModelTier(reviewerModelTier).Valid() {
		return exitcode.Usage(fmt.Errorf("--reviewer-model-tier must be one of small, medium, large"))
	}
	if err := validateNonEmptyChangedFlags(cmd, [2]string{"reviewer-effort", flags.reviewerEffort}); err != nil {
		return exitcode.Usage(err)
	}
	if reviewerEffortChanged && !modelprefs.Effort(reviewerEffort).Valid() {
		return exitcode.Usage(fmt.Errorf("--reviewer-effort must be one of low, medium, high"))
	}
	dryRunOnlyOverrideChanged := selectionModelChanged || selectionEffortChanged || selectionPromptChanged || reviewerModelTierChanged
	if dryRunOnlyOverrideChanged && !flags.dryRun {
		return exitcode.Usage(fmt.Errorf("--selection-model, --selection-effort, --selection-prompt, and --reviewer-model-tier require --dry-run or --no-post"))
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
	if flags.freshSession && flags.retryPosts {
		return exitcode.Usage(fmt.Errorf("--fresh-session cannot be used with --retry-posts"))
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

	logger := root.NewProgressLogger(opts)
	configSpan := logger.Start("review", "load_config", "config")
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return exitcode.AuthConfig(configSpan.End(err))
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cmderr.Config(configSpan.End(err))
	}
	_ = configSpan.End(nil)
	parseSpan := logger.Start("review", "parse_pr", "pr")
	ref, urlProvider, err := prref.ParsePullURL(prArg)
	if err != nil {
		return exitcode.Usage(parseSpan.End(err))
	}
	_ = parseSpan.End(nil)
	profileSpan := logger.Start("review", "resolve_profile", "profile")
	profileName, profile, err := config.ResolveProfileForRepository(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
		Host:      ref.Host,
		Namespace: ref.Owner,
		Repo:      ref.Repo,
	})
	if err != nil {
		return cmderr.Config(profileSpan.End(err))
	}
	if !prref.SameHost(ref.Host, profile.Git.Host) {
		return exitcode.Usage(profileSpan.End(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host)))
	}
	if err := prref.MatchProvider(urlProvider, string(profile.Git.ProviderKind())); err != nil {
		return exitcode.Usage(profileSpan.End(err))
	}
	_ = profileSpan.End(nil)
	reviewerFast := profile.Fast
	if cmd.Flags().Changed("fast") {
		reviewerFast = flags.fast
	} else if cmd.Flags().Changed("no-fast") {
		reviewerFast = !flags.noFast
	}
	if reviewerFast && flags.retryPosts {
		return exitcode.Usage(fmt.Errorf("fast mode cannot be used with --retry-posts"))
	}

	runtimeReq := app.OpenRequest{
		Config:                            cfg,
		Profile:                           profile,
		ProfileName:                       profileName,
		Backend:                           opts.Backend,
		BackendFlagChanged:                cmderr.BackendFlagChanged(cmd),
		Command:                           commandName(cmd),
		Progress:                          logger,
		Warnings:                          opts.Stderr,
		MaxAgents:                         flags.maxAgents,
		MaxConcurrency:                    flags.maxConcurrency,
		PRRef:                             ref,
		PRURL:                             prArg,
		RequireOpinionatedReviewAuthority: !flags.dryRun,
		Retention:                         appruntime.RetentionPolicyFromConfig(cfg.Data.Retention),
		RetentionManualOnly:               cfg.Data.Retention.Enforcement == config.RetentionManualOnly,
	}
	runtimeSpan := logger.Start("review", "build_runtime", "runtime")
	runtime, err := factory(ctx, runtimeReq)
	if err != nil {
		return runtimeSpan.End(cmdruntime.MapRunError(err))
	}
	_ = runtimeSpan.End(nil)
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
		ReviewerFast:                reviewerFast,
		ReviewBaseSHA:               reviewBaseSHA,
		ReviewHeadSHA:               reviewHeadSHA,
		Rerun:                       flags.rerun,
		FreshSession:                flags.freshSession,
		ToolVersion:                 version.Version,
	}
	if !flags.dryRun {
		return runLive(ctx, logger, opts, flags, runtime.Runner, pipelineReq, failOn)
	}

	execSpan := logger.Start("review", "execute_dry_run", "pr")
	result, err := runtime.Runner.DryRun(ctx, pipelineReq)
	if err != nil {
		return execSpan.End(cmdruntime.MapRunError(err))
	}
	_ = execSpan.End(nil)
	renderSpan := logger.Start("review", "render_result", "stdout")
	rendered, err := view.NewReviewDryRun(result)
	if err != nil {
		return renderSpan.End(err)
	}
	err = view.Render(opts.Stdout, flags.jsonOutput, rendered, func(w io.Writer) error {
		return view.RenderReviewDryRunText(w, rendered)
	})
	if err != nil {
		return renderSpan.End(err)
	}
	_ = renderSpan.End(nil)
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
}

func validateNonEmptyChangedFlags(cmd *cobra.Command, flags ...[2]string) error {
	for _, flag := range flags {
		if cmd.Flags().Changed(flag[0]) && strings.TrimSpace(flag[1]) == "" {
			return fmt.Errorf("--%s must be non-empty", flag[0])
		}
	}
	return nil
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

func runLive(ctx context.Context, logger *progress.Logger, opts *root.Options, flags commandFlags, runner app.Runner, req pipeline.Request, failOn *review.Severity) error {
	execSpan := logger.Start("review", "execute_live", "pr")
	result, err := runner.Live(ctx, req, reviewrun.Flags{Rerun: flags.rerun, RetryPosts: flags.retryPosts})
	if err != nil {
		return execSpan.End(cmdruntime.MapRunError(err))
	}
	_ = execSpan.End(nil)
	renderSpan := logger.Start("review", "render_result", "stdout")
	rendered := newReviewLive(result)
	err = view.Render(opts.Stdout, flags.jsonOutput, rendered, func(w io.Writer) error {
		return view.RenderReviewLiveText(w, rendered)
	})
	if err != nil {
		return renderSpan.End(err)
	}
	_ = renderSpan.End(nil)
	if result.ExitCode != exitcode.Success {
		return exitcode.With(result.ExitCode, liveResultError(result))
	}
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
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
