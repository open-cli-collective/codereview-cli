// Package respondcmd wires the `cr respond` command surface.
package respondcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/threadreply"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const respondLong = `Respond to human replies on cr's own open review-comment threads.

cr lists the pull request's inline review threads, finds the ones it authored
(its own findings) that have a newer human reply, and asks a small-tier LLM
whether to acknowledge, answer, or skip. It posts a contextual in-thread reply
and resolves the thread when the finding has been addressed.

Use --dry-run (or --no-post) to plan replies without posting. Use
--no-resolve-threads to keep threads open even when the reply indicates the
finding is addressed.`

// Runtime contains the per-command dependencies for `cr respond`.
type Runtime struct {
	Provider        gitprovider.GitProvider
	PostingIdentity gitprovider.Identity
	Classifier      threadreply.Classifier
	Cleanup         func()
}

// RuntimeFactory builds the concrete runtime used by `cr respond`.
type RuntimeFactory func(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile, ref gitprovider.PRRef) (Runtime, error)

type commandFlags struct {
	dryRun           bool
	noPost           bool
	jsonOutput       bool
	noResolveThreads bool
}

// Register attaches the respond command to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, newRuntime)
}

// RegisterWithFactory attaches the respond command with an injected runtime factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory RuntimeFactory) {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "respond <PR>",
		Short: "Reply to human replies on cr's own review-comment threads",
		Long:  respondLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("respond requires one PR URL"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRespond(cmd.Context(), cmd, opts, factory, flags, args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Plan thread replies without posting")
	cmd.Flags().BoolVar(&flags.noPost, "no-post", false, "Alias for --dry-run")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	cmd.Flags().BoolVar(&flags.noResolveThreads, "no-resolve-threads", false, "Reply but never resolve threads")
	rootCmd.AddCommand(cmd)
}

func runRespond(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory RuntimeFactory, flags commandFlags, prArg string) error {
	if flags.noPost {
		flags.dryRun = true
	}
	path, err := configPath(opts)
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
	_, profile, err := config.ResolveProfileForRepository(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
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
	noResolve := flags.noResolveThreads || profile.ReviewPolicy.ResolveThreads == config.ResolveThreadsNever

	runtime, err := factory(cmd, opts, cfg, profile, ref)
	if err != nil {
		return err
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Provider == nil {
		return fmt.Errorf("respond: runtime provider is required")
	}
	if runtime.Classifier == nil {
		return fmt.Errorf("respond: runtime classifier is required")
	}

	result, err := respond(ctx, runtime, ref, prArg, flags.dryRun, noResolve)
	if err != nil {
		return mapRunError(err)
	}
	if flags.jsonOutput {
		return view.RenderRespondJSON(opts.Stdout, result)
	}
	return view.RenderRespondText(opts.Stdout, result)
}

func respond(ctx context.Context, runtime Runtime, ref gitprovider.PRRef, prURL string, dryRun, noResolve bool) (view.RespondResult, error) {
	pr, err := runtime.Provider.GetPR(ctx, ref)
	if err != nil {
		return view.RespondResult{}, err
	}
	threads, err := runtime.Provider.ListInlineThreads(ctx, ref)
	if err != nil {
		return view.RespondResult{}, err
	}
	candidates := threadreply.SelectCandidates(pr, runtime.PostingIdentity, threads)
	result := view.RespondResult{
		PRURL:      prURL,
		DryRun:     dryRun,
		Considered: len(candidates),
		Threads:    make([]view.RespondThread, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		decision, err := runtime.Classifier.ClassifyThreadReply(ctx, candidate.Request)
		if err != nil {
			return view.RespondResult{}, err
		}
		rendered, err := applyDecision(ctx, runtime, ref, candidate, decision, dryRun, noResolve)
		if err != nil {
			return view.RespondResult{}, err
		}
		switch {
		case rendered.Resolved:
			result.Resolved++
			result.Replied++
		case rendered.Decision == string(threadreply.DecisionSkip):
			result.Skipped++
		default:
			result.Replied++
		}
		result.Threads = append(result.Threads, rendered)
	}
	return result, nil
}

func applyDecision(ctx context.Context, runtime Runtime, ref gitprovider.PRRef, candidate threadreply.Candidate, decision threadreply.Result, dryRun, noResolve bool) (view.RespondThread, error) {
	thread := view.RespondThread{
		ThreadID: string(candidate.Thread.ID),
		Path:     candidate.Thread.Path,
		Line:     candidate.Thread.Line,
		Decision: string(decision.Decision),
		Reply:    decision.Reply,
	}
	if decision.Decision == threadreply.DecisionSkip {
		return thread, nil
	}
	willResolve := decision.Decision == threadreply.DecisionAcknowledgeAndResolve && !noResolve
	if dryRun {
		thread.Resolved = willResolve
		return thread, nil
	}
	if _, err := runtime.Provider.ReplyToThread(ctx, ref, candidate.Thread.ID, decision.Reply); err != nil {
		return view.RespondThread{}, err
	}
	thread.Posted = true
	if willResolve {
		if err := runtime.Provider.ResolveThread(ctx, ref, candidate.Thread.ID); err != nil {
			return view.RespondThread{}, err
		}
		thread.Resolved = true
	}
	return thread, nil
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
		errors.Is(err, config.ErrSecretsProfileNotFound),
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

var (
	newGitProvider = func(git config.GitConfig, store githubprovider.TokenStore, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
		return githubprovider.NewFromGitConfig(git, store, opts)
	}
	newAdapterForRuntime = newAdapter
)

func newRuntime(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile, ref gitprovider.PRRef) (Runtime, error) {
	resolvedSecretsProfile, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
	if err != nil {
		return Runtime{}, mapRunError(err)
	}
	store, err := credentials.OpenResolvedStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, resolvedSecretsProfile)
	if err != nil {
		return Runtime{}, mapRunError(err)
	}
	cleanup := func() { _ = store.Close() }
	providerGit := gitConfigForReviewerAuth(profile)
	provider, credential, err := newGitProvider(providerGit, store, gitProviderOptions(ref))
	if err != nil {
		cleanup()
		return Runtime{}, mapRunError(err)
	}
	postingIdentity, err := provider.WhoAmI(cmd.Context(), credential)
	if err != nil {
		cleanup()
		return Runtime{}, mapRunError(err)
	}
	classifier, err := buildClassifier(profile, store, opts.Stderr)
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	return Runtime{
		Provider:        provider,
		PostingIdentity: postingIdentity,
		Classifier:      classifier,
		Cleanup:         cleanup,
	}, nil
}

func buildClassifier(profile config.Profile, store *credstore.Store, warnings io.Writer) (threadreply.Classifier, error) {
	return &lazyClassifier{profile: profile, store: store, warnings: warnings}, nil
}

// lazyClassifier defers LLM adapter construction until the first classification
// so commands with no candidate threads never touch the LLM provider.
type lazyClassifier struct {
	mu       sync.Mutex
	profile  config.Profile
	store    *credstore.Store
	warnings io.Writer
	resolved threadreply.Classifier
	err      error
	loaded   bool
}

func (c *lazyClassifier) ClassifyThreadReply(ctx context.Context, req threadreply.Request) (threadreply.Result, error) {
	classifier, err := c.get()
	if err != nil {
		return threadreply.Result{}, err
	}
	return classifier.ClassifyThreadReply(ctx, req)
}

func (c *lazyClassifier) get() (threadreply.Classifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.resolved, c.err
	}
	c.loaded = true
	adapter, err := newAdapterForRuntime(c.profile.LLM, c.store)
	if err != nil {
		c.err = fmt.Errorf("respond: initialize LLM adapter: %w", err)
		return nil, c.err
	}
	model, effort, ok := resolveClassifierModel(c.profile, c.warnings)
	if !ok {
		c.err = fmt.Errorf("%w: respond requires a small or medium model tier", config.ErrUnsupported)
		return nil, c.err
	}
	c.resolved = threadreply.NewLLMClassifier(adapter, model, effort)
	return c.resolved, nil
}

func resolveClassifierModel(profile config.Profile, warnings io.Writer) (string, string, bool) {
	if resolved, ok := config.ResolveModelTier(profile.LLM, config.ModelTierSmall); ok {
		return resolved.Model, "low", true
	}
	if resolved, ok := config.ResolveModelTier(profile.LLM, config.ModelTierMedium); ok {
		if warnings != nil {
			_, _ = fmt.Fprintf(warnings, "warning: respond classifier small model is not configured; falling back to medium tier model %s\n", resolved.Model)
		}
		return resolved.Model, "low", true
	}
	return "", "", false
}

func gitConfigForReviewerAuth(profile config.Profile) config.GitConfig {
	if profile.ReviewerCredentials == nil {
		return profile.Git
	}
	return config.GitConfig{
		Host:          profile.Git.Host,
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		CredentialRef: profile.ReviewerCredentials.CredentialRef,
		IdentityCache: profile.ReviewerCredentials.IdentityCache,
	}
}

func gitProviderOptions(ref gitprovider.PRRef) githubprovider.Options {
	if ref.Owner == "" || ref.Repo == "" {
		return githubprovider.Options{}
	}
	return githubprovider.Options{InstallationLookup: &githubprovider.InstallationLookup{
		Owner: ref.Owner,
		Repo:  ref.Repo,
	}}
}

func newAdapter(llmConfig config.LLMConfig, store *credstore.Store) (llm.Adapter, error) {
	switch llmConfig.Adapter {
	case config.LLMAdapterClaudeCLI:
		return llm.NewClaudeCLIAdapter(llm.SubprocessOptions{}), nil
	case config.LLMAdapterCodexCLI:
		if llmConfig.Provider != config.LLMProviderOpenAI || llmConfig.Auth != config.LLMAuthSubscription {
			return nil, fmt.Errorf("%w: codex_cli requires provider openai with subscription auth", config.ErrUnsupported)
		}
		return llm.NewCodexCLIAdapter(llm.SubprocessOptions{AllowBestEffortNoTools: true}), nil
	case config.LLMAdapterPiRPC:
		if llmConfig.Provider != config.LLMProviderPi || llmConfig.Auth != config.LLMAuthSubscription {
			return nil, fmt.Errorf("%w: pi_rpc requires provider pi with subscription auth", config.ErrUnsupported)
		}
		return llm.NewPiRPCAdapter(llm.PiRPCOptions{}), nil
	case config.LLMAdapterAnthropicAPI, config.LLMAdapterOpenAIAPI:
		return llm.NewAPIAdapterFromConfig(llmConfig, store, llm.APIOptions{})
	default:
		return nil, fmt.Errorf("%w: unsupported LLM adapter %q", config.ErrUnsupported, llmConfig.Adapter)
	}
}
