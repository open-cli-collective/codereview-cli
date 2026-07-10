// Package respondcmd wires the `cr respond` command surface.
package respondcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/appruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

type flags struct {
	dryRun           bool
	noPost           bool
	rerun            bool
	retryPosts       string
	jsonOutput       bool
	noResolveThreads bool
}

// Register attaches the respond command to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, app.Open)
}

// RuntimeFactory builds the concrete runtime used by response lifecycle commands.
type RuntimeFactory func(context.Context, app.OpenRequest) (app.Runtime, error)

// RegisterWithFactory attaches the respond command with an injected runtime factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory RuntimeFactory) {
	var flags flags
	cmd := &cobra.Command{
		Use:   "respond <PR>",
		Short: "Respond to unresolved codereview inline discussion threads",
		Args:  exitcode.ExactArgs(1, "respond requires one PR URL"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), cmd, opts, factory, flags, args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Plan thread responses without posting")
	cmd.Flags().BoolVar(&flags.noPost, "no-post", false, "Alias for --dry-run")
	cmd.Flags().BoolVar(&flags.rerun, "rerun", false, "Bypass incomplete response-run resume and start a fresh response attempt")
	cmd.Flags().StringVar(&flags.retryPosts, "retry-posts", "", "Retry missing or failed required posts for the given response run ID")
	root.AddJSONFlag(cmd, &flags.jsonOutput)
	cmd.Flags().BoolVar(&flags.noResolveThreads, "no-resolve-threads", false, "Do not plan thread-resolution actions")
	rootCmd.AddCommand(cmd)
}

func run(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory RuntimeFactory, flags flags, prArg string) error {
	if flags.noPost {
		flags.dryRun = true
	}
	retryRunID := strings.TrimSpace(flags.retryPosts)
	if cmd.Flags().Changed("retry-posts") && retryRunID == "" {
		return exitcode.Usage(fmt.Errorf("--retry-posts must be a non-empty run ID"))
	}
	if retryRunID != "" && flags.dryRun {
		return exitcode.Usage(fmt.Errorf("--retry-posts cannot be used with --dry-run or --no-post"))
	}
	if retryRunID != "" && flags.rerun {
		return exitcode.Usage(fmt.Errorf("--retry-posts cannot be used with --rerun"))
	}
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cmderr.Config(err)
	}
	ref, err := prref.ParseGitHubPullURL(prArg)
	if err != nil {
		return exitcode.Usage(err)
	}
	profileName, profile, err := config.ResolveProfileForRepository(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
		Host:      ref.Host,
		Namespace: ref.Owner,
		Repo:      ref.Repo,
	})
	if err != nil {
		return cmderr.Config(err)
	}
	if !prref.SameHost(ref.Host, profile.Git.Host) {
		return exitcode.Usage(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host))
	}
	runtime, err := factory(ctx, app.OpenRequest{
		Config:              cfg,
		Profile:             profile,
		Backend:             opts.Backend,
		BackendFlagChanged:  cmderr.BackendFlagChanged(cmd),
		Command:             "respond",
		Progress:            progress.New(opts.Stderr, opts.Quiet, nil),
		Warnings:            opts.Stderr,
		PRRef:               ref,
		Retention:           appruntime.RetentionPolicyFromConfig(cfg.Data.Retention),
		RetentionManualOnly: cfg.Data.Retention.Enforcement == config.RetentionManualOnly,
	})
	if err != nil {
		return cmdruntime.MapRunError(err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Responder == nil {
		return cmdruntime.MissingResponderError()
	}
	noResolve := flags.noResolveThreads || profile.ReviewPolicy.ResolveThreads == config.ResolveThreadsNever
	result, err := runtime.Responder.Respond(ctx, threadrespond.Request{
		PRRef:            ref,
		PRURL:            prArg,
		ProfileName:      profileName,
		Profile:          profile,
		PostingIdentity:  runtime.PostingIdentity,
		DryRun:           flags.dryRun,
		NoResolveThreads: noResolve,
		Rerun:            flags.rerun,
		RetryRunID:       retryRunID,
	})
	if err != nil {
		return cmdruntime.MapRunError(err)
	}
	rendered := newResult(result)
	if err := view.Render(opts.Stdout, flags.jsonOutput, rendered, func(w io.Writer) error {
		return renderText(w, rendered)
	}); err != nil {
		return err
	}
	if result.ExitCode != exitcode.Success {
		return exitcode.With(result.ExitCode, resultError(result))
	}
	return nil
}

type renderedResult struct {
	Run     view.ReviewRun    `json:"run"`
	Counts  counts            `json:"counts"`
	Outbox  view.ReviewOutbox `json:"outbox"`
	Message string            `json:"message,omitempty"`
}

type counts struct {
	Considered       int `json:"considered"`
	Responded        int `json:"responded"`
	Resolved         int `json:"resolved"`
	ResolvePlanned   int `json:"resolve_planned"`
	ResolveFailed    int `json:"resolve_failed"`
	ProviderResolved int `json:"provider_resolved"`
	Planned          int `json:"planned"`
}

func newResult(result threadrespond.Result) renderedResult {
	outcome := result.Outbox.Outcome.String()
	if outcome == "" && result.Run.Outcome != nil {
		outcome = result.Run.Outcome.String()
	}
	outboxExitCode := result.Outbox.ExitCode
	if outboxExitCode == 0 && result.ExitCode != 0 {
		outboxExitCode = result.ExitCode
	}
	responded := countThreadReplies(result.Plan.Actions)
	resolvePlanned := countThreadResolves(result.Plan.Actions)
	if responded == 0 && resolvePlanned == 0 && len(result.PlannedActions) > 0 {
		responded = countPlannedThreadReplies(result.PlannedActions)
		resolvePlanned = countPlannedThreadResolves(result.PlannedActions)
	}
	providerResolved := countPostedThreadResolves(result.PlannedActions)
	resolveFailed := countFailedThreadResolves(result.PlannedActions)
	return renderedResult{
		Run: view.ReviewRun{
			RunID:        result.Run.RunID,
			PRURL:        result.PR.URL,
			PRKey:        result.PRKey,
			PostMode:     result.Run.PostMode.String(),
			Outcome:      outcome,
			ArtifactPath: result.Run.ArtifactPath,
			BaseSHA:      result.Run.BaseSHA,
			HeadSHA:      result.Run.SHA,
		},
		Counts: counts{
			Considered:       len(result.EligibleThreads),
			Responded:        responded,
			Resolved:         providerResolved,
			ResolvePlanned:   resolvePlanned,
			ResolveFailed:    resolveFailed,
			ProviderResolved: providerResolved,
			Planned:          len(result.PlannedActions),
		},
		Outbox: view.ReviewOutbox{
			Outcome:        outcome,
			ExitCode:       outboxExitCode,
			Posted:         result.Outbox.Posted,
			Pending:        result.Outbox.Pending,
			FailedTerminal: result.Outbox.FailedTerminal,
			Aborted:        result.Outbox.Aborted,
		},
		Message: result.Message,
	}
}

func renderText(w io.Writer, rendered renderedResult) error {
	if _, err := fmt.Fprintf(w, "Run: %s\n", rendered.Run.RunID); err != nil {
		return err
	}
	if rendered.Run.PRURL != "" {
		if _, err := fmt.Fprintf(w, "PR: %s\n", rendered.Run.PRURL); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Outcome: %s\n", rendered.Run.Outcome); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Threads: considered %d, responded %d, provider resolved %d (resolve planned %d, failed %d)\n", rendered.Counts.Considered, rendered.Counts.Responded, rendered.Counts.ProviderResolved, rendered.Counts.ResolvePlanned, rendered.Counts.ResolveFailed); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Planned actions: %d\n", rendered.Counts.Planned); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Outbox: posted %d, pending %d, failed %d\n", rendered.Outbox.Posted, rendered.Outbox.Pending, rendered.Outbox.FailedTerminal); err != nil {
		return err
	}
	if rendered.Run.ArtifactPath != "" {
		if _, err := fmt.Fprintf(w, "Artifacts: %s\n", rendered.Run.ArtifactPath); err != nil {
			return err
		}
	}
	if strings.TrimSpace(rendered.Message) != "" {
		if _, err := fmt.Fprintf(w, "Message: %s\n", rendered.Message); err != nil {
			return err
		}
	}
	return nil
}

func countThreadReplies(actions []reviewplan.Action) int {
	var count int
	for _, action := range actions {
		if action.Kind == reviewplan.ActionKindThreadReply {
			count++
		}
	}
	return count
}

func countThreadResolves(actions []reviewplan.Action) int {
	var count int
	for _, action := range actions {
		if action.Kind == reviewplan.ActionKindResolveThread {
			count++
		}
	}
	return count
}

func countPlannedThreadReplies(actions []ledger.PlannedAction) int {
	var count int
	for _, action := range actions {
		if action.Kind == ledger.PlannedActionThreadReply {
			count++
		}
	}
	return count
}

func countPlannedThreadResolves(actions []ledger.PlannedAction) int {
	var count int
	for _, action := range actions {
		if action.Kind == ledger.PlannedActionResolveThread {
			count++
		}
	}
	return count
}

func countPostedThreadResolves(actions []ledger.PlannedAction) int {
	var count int
	for _, action := range actions {
		if action.Kind == ledger.PlannedActionResolveThread && action.Status == ledger.PlannedActionPosted {
			count++
		}
	}
	return count
}

func countFailedThreadResolves(actions []ledger.PlannedAction) int {
	var count int
	for _, action := range actions {
		if action.Kind == ledger.PlannedActionResolveThread && action.Status == ledger.PlannedActionFailedTerminal {
			count++
		}
	}
	return count
}

func resultError(result threadrespond.Result) error {
	if strings.TrimSpace(result.Message) != "" {
		return errors.New(result.Message)
	}
	if result.Outbox.Outcome != "" {
		return fmt.Errorf("respond completed with outcome %s", result.Outbox.Outcome)
	}
	return fmt.Errorf("respond did not complete")
}
