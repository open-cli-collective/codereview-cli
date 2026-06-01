// Package reviewcmd wires the `cr review` command surface.
package reviewcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const (
	livePostLimiterInterval = 500 * time.Millisecond
	livePostLimiterBurst    = 2
)

// Runner executes the configured review pipeline.
type Runner interface {
	DryRun(context.Context, pipeline.Request) (pipeline.Result, error)
	Live(context.Context, pipeline.Request, reviewrun.Flags) (reviewrun.Result, error)
}

// Runtime contains per-command dependencies that need cleanup after a run.
type Runtime struct {
	Runner          Runner
	PostingIdentity gitprovider.Identity
	Cleanup         func()
}

// RuntimeOptions carries command flags that affect runtime construction.
type RuntimeOptions struct {
	MaxAgents      int
	MaxConcurrency int
}

// RuntimeFactory builds the concrete runtime used by `cr review`.
type RuntimeFactory func(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error)

type commandFlags struct {
	dryRun           bool
	noPost           bool
	rerun            bool
	retryPosts       bool
	agentsDirs       []string
	failOn           string
	jsonOutput       bool
	verbose          bool
	maxAgents        int
	maxConcurrency   int
	allowSelfReview  bool
	allowSelfApprove bool
	noResolveThreads bool
}

// Register attaches the review command to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, newRuntime)
}

// RegisterWithFactory attaches the review command with an injected runtime factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory RuntimeFactory) {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "review <PR>",
		Short: "Run an automated pull-request review",
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
	cmd.Flags().BoolVar(&flags.rerun, "rerun", false, "Bypass the gate and run a fresh live review")
	cmd.Flags().BoolVar(&flags.retryPosts, "retry-posts", false, "Retry missing or failed required posts without rerunning review")
	cmd.Flags().StringArrayVar(&flags.agentsDirs, "agents-dir", nil, "Additional trusted agents directory")
	cmd.Flags().StringVar(&flags.failOn, "fail-on", "", "Exit 1 when a finding at or above severity exists")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	cmd.Flags().BoolVar(&flags.verbose, "verbose", false, "Emit additional diagnostic details")
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
	if flags.rerun && flags.retryPosts {
		return exitcode.Usage(fmt.Errorf("--rerun and --retry-posts are mutually exclusive"))
	}
	if flags.maxAgents < 0 {
		return exitcode.Usage(fmt.Errorf("--max-agents must be non-negative"))
	}
	if flags.maxConcurrency < 0 {
		return exitcode.Usage(fmt.Errorf("--max-concurrency must be non-negative"))
	}
	var failOn *review.Severity
	if strings.TrimSpace(flags.failOn) != "" {
		threshold, err := review.ParseSeverity(flags.failOn)
		if err != nil {
			return exitcode.Usage(err)
		}
		failOn = &threshold
	}

	path, err := configPath(opts)
	if err != nil {
		return exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cmderr.Config(err)
	}
	profileName, profile, err := config.ResolveProfile(cfg, opts.Profile)
	if err != nil {
		return cmderr.Config(err)
	}
	ref, err := prref.ParseGitHubPullURL(prArg)
	if err != nil {
		return exitcode.Usage(err)
	}
	if !prref.SameHost(ref.Host, profile.Git.Host) {
		return exitcode.Usage(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host))
	}

	runtime, err := factory(cmd, opts, cfg, profile, RuntimeOptions{MaxAgents: flags.maxAgents, MaxConcurrency: flags.maxConcurrency})
	if err != nil {
		return err
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Runner == nil {
		return fmt.Errorf("review: runtime runner is required")
	}
	noResolve := flags.noResolveThreads || profile.ReviewPolicy.ResolveThreads == config.ResolveThreadsNever
	pipelineReq := pipeline.Request{
		PRRef:               ref,
		PRURL:               prArg,
		ProfileName:         profileName,
		Profile:             profile,
		PostingIdentity:     runtime.PostingIdentity,
		AgentDirs:           append([]string(nil), flags.agentsDirs...),
		FailOn:              failOn,
		AllowSelfReview:     flags.allowSelfReview,
		AllowSelfApprove:    flags.allowSelfApprove,
		NoResolveThreads:    noResolve,
		MajorRequestChanges: profile.ReviewPolicy.MajorEvent == config.ReviewMajorEventRequestChanges,
		IncludeNits:         flags.verbose,
	}
	if !flags.dryRun {
		return runLive(ctx, opts, flags, runtime.Runner, pipelineReq, failOn)
	}

	result, err := runtime.Runner.DryRun(ctx, pipelineReq)
	if err != nil {
		return mapRunError(err)
	}
	rendered, err := newReviewDryRun(result)
	if err != nil {
		return err
	}
	if flags.jsonOutput {
		err = view.RenderReviewDryRunJSON(opts.Stdout, rendered)
	} else {
		err = view.RenderReviewDryRunText(opts.Stdout, rendered)
	}
	if err != nil {
		return err
	}
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
}

func runLive(ctx context.Context, opts *root.Options, flags commandFlags, runner Runner, req pipeline.Request, failOn *review.Severity) error {
	result, err := runner.Live(ctx, req, reviewrun.Flags{Rerun: flags.rerun, RetryPosts: flags.retryPosts})
	if err != nil {
		return mapRunError(err)
	}
	rendered := newReviewLive(result)
	if flags.jsonOutput {
		err = view.RenderReviewLiveJSON(opts.Stdout, rendered)
	} else {
		err = view.RenderReviewLiveText(opts.Stdout, rendered)
	}
	if err != nil {
		return err
	}
	if result.ExitCode != exitcode.Success {
		return exitcode.With(result.ExitCode, liveResultError(result))
	}
	if result.FailOnTriggered && failOn != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("findings at or above --fail-on %s", failOn.String()))
	}
	return nil
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
		},
		RollupMarkdown:  result.Plan.RollupMarkdown,
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

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func mapRunError(err error) error {
	switch {
	case errors.Is(err, config.ErrInvalid),
		errors.Is(err, config.ErrNotConfigured),
		errors.Is(err, config.ErrProfileNotFound),
		errors.Is(err, config.ErrUnsupported):
		return cmderr.Config(err)
	case errors.Is(err, gitprovider.ErrAuth),
		errors.Is(err, gitprovider.ErrPermission),
		errors.Is(err, gitprovider.ErrRetryable),
		errors.Is(err, gitprovider.ErrNotFound),
		errors.Is(err, gitprovider.ErrConflict),
		errors.Is(err, gitprovider.ErrStaleSHA):
		return cmderr.Provider(err)
	case errors.Is(err, credentials.ErrInvalidBackendSelection),
		errors.Is(err, credentials.ErrWrongService),
		errors.Is(err, credstore.ErrRefEmpty),
		errors.Is(err, credstore.ErrRefSegmentCount),
		errors.Is(err, credstore.ErrRefInvalidChar),
		errors.Is(err, credstore.ErrKeyNotAllowed),
		errors.Is(err, credstore.ErrExists),
		errors.Is(err, credstore.ErrFilePassphraseRequired),
		errors.Is(err, credstore.ErrSecretServiceFailClosed),
		errors.Is(err, credstore.ErrStoreClosed),
		errors.Is(err, credstore.ErrBackendNotImplemented):
		return cmderr.Credential(err)
	default:
		return err
	}
}

func newRuntime(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
	store, err := credentials.OpenStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg)
	if err != nil {
		return Runtime{}, cmderr.Credential(err)
	}
	cleanup := func() { _ = store.Close() }
	provider, credential, err := githubprovider.NewFromGitConfig(profile.Git, store, githubprovider.Options{})
	if err != nil {
		cleanup()
		return Runtime{}, mapRunError(err)
	}
	postingIdentity, err := resolvePostingIdentity(cmd.Context(), provider, credential, store, profile)
	if err != nil {
		cleanup()
		return Runtime{}, mapRunError(err)
	}
	adapter, err := newAdapter(profile.LLM, store)
	if err != nil {
		cleanup()
		return Runtime{}, mapRunError(err)
	}
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	ledgerStore, err := ledger.Open(cmd.Context(), layout.LedgerDB())
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	cleanup = func() {
		_ = ledgerStore.Close()
		_ = store.Close()
	}
	pipelineOpts := pipeline.Options{
		Provider:       provider,
		Adapter:        adapter,
		Store:          ledgerStore,
		Layout:         layout,
		MaxAgents:      runtimeOpts.MaxAgents,
		MaxConcurrency: runtimeOpts.MaxConcurrency,
	}
	limiter, err := outbox.NewTokenBucket(livePostLimiterInterval, livePostLimiterBurst)
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	return Runtime{
		Runner: reviewRunner{
			pipeline: pipelineOpts,
			live: reviewrun.Options{
				Store:                   ledgerStore,
				Provider:                provider,
				Planner:                 livePlanner{opts: pipelineOpts},
				Limiter:                 limiter,
				Layout:                  layout,
				StaleHeartbeatThreshold: 10 * time.Minute,
				Warnings:                opts.Stderr,
			},
		},
		PostingIdentity: postingIdentity,
		Cleanup:         cleanup,
	}, nil
}

type reviewRunner struct {
	pipeline pipeline.Options
	live     reviewrun.Options
}

func (r reviewRunner) DryRun(ctx context.Context, req pipeline.Request) (pipeline.Result, error) {
	return pipeline.DryRun(ctx, r.pipeline, req)
}

func (r reviewRunner) Live(ctx context.Context, req pipeline.Request, flags reviewrun.Flags) (reviewrun.Result, error) {
	return reviewrun.Run(ctx, r.live, reviewrun.Request{Pipeline: req, Flags: flags})
}

type livePlanner struct {
	opts pipeline.Options
}

func (p livePlanner) Live(ctx context.Context, req pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	return pipeline.Live(ctx, p.opts, req, run)
}

func resolvePostingIdentity(ctx context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, store githubprovider.TokenStore, profile config.Profile) (gitprovider.Identity, error) {
	if profile.ReviewerCredentials == nil {
		return provider.WhoAmI(ctx, credential)
	}
	reviewerGit := config.GitConfig{
		Host:          profile.Git.Host,
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		CredentialRef: profile.ReviewerCredentials.CredentialRef,
		IdentityCache: profile.ReviewerCredentials.IdentityCache,
	}
	reviewerProvider, reviewerCredential, err := githubprovider.NewFromGitConfig(reviewerGit, store, githubprovider.Options{})
	if err != nil {
		return gitprovider.Identity{}, err
	}
	return reviewerProvider.WhoAmI(ctx, reviewerCredential)
}

func newAdapter(llmConfig config.LLMConfig, store *credstore.Store) (llm.Adapter, error) {
	switch llmConfig.Adapter {
	case config.LLMAdapterClaudeCLI:
		return llm.NewClaudeCLIAdapter(llm.SubprocessOptions{}), nil
	case config.LLMAdapterCodexCLI:
		return nil, fmt.Errorf("%w: codex_cli is not supported for cr review until no-tools mode is explicit", config.ErrUnsupported)
	case config.LLMAdapterAnthropicAPI, config.LLMAdapterOpenAIAPI:
		return llm.NewAPIAdapterFromConfig(llmConfig, store, llm.APIOptions{})
	default:
		return nil, fmt.Errorf("%w: unsupported LLM adapter %q", config.ErrUnsupported, llmConfig.Adapter)
	}
}

var _ Runner = reviewRunner{}
