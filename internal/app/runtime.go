// Package app assembles command-independent review runtime
// dependencies.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
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

// ResponseRunner executes response-only thread lifecycle runs.
type ResponseRunner interface {
	Respond(context.Context, threadrespond.Request) (threadrespond.Result, error)
}

// Runtime contains reusable runtime dependencies that need cleanup after a run.
type Runtime struct {
	Runner          Runner
	Responder       ResponseRunner
	PostingIdentity gitprovider.Identity
	Cleanup         func()
}

// OpenRequest carries command-resolved values needed to assemble a runtime.
type OpenRequest struct {
	Config                            config.File
	Profile                           config.Profile
	Backend                           string
	BackendFlagChanged                bool
	Command                           string
	Progress                          *progress.Logger
	Warnings                          io.Writer
	PRRef                             gitprovider.PRRef
	RequireOpinionatedReviewAuthority bool
	MaxAgents                         int
	MaxConcurrency                    int
	Retention                         datalifecycle.RetentionPolicy
	RetentionManualOnly               bool
	ResolveRepoRoot                   func(context.Context) (string, error)
	GitCommand                        func(context.Context, string, ...string) ([]byte, error)
	Dependencies                      Dependencies
}

// SelectionOpenRequest carries command-resolved values for selection-only
// runtime assembly.
type SelectionOpenRequest struct {
	Config             config.File
	Profile            config.Profile
	Backend            string
	BackendFlagChanged bool
	PRRef              gitprovider.PRRef
	Dependencies       Dependencies
}

// GitProviderFactory builds a provider and the credential used to authenticate
// it.
type GitProviderFactory func(config.GitConfig, credentials.Reader, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error)

// PostingIdentityResolver resolves the identity used for live review writes.
type PostingIdentityResolver func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error)

// AdapterFactory builds an LLM adapter from normalized config and credentials.
type AdapterFactory func(config.LLMConfig, credentials.Reader) (llm.Adapter, error)

// Dependencies contains fakeable assembly seams. Nil fields use production
// defaults.
type Dependencies struct {
	NewGitProvider         GitProviderFactory
	ResolvePostingIdentity PostingIdentityResolver
	NewAdapter             AdapterFactory
	RuntimeLayout          func() (statepaths.Layout, error)
	OpenLedger             func(context.Context, string) (*ledger.Store, error)
	NewLimiter             func() (outbox.Limiter, error)
}

func (d Dependencies) withDefaults() Dependencies {
	if d.NewGitProvider == nil {
		d.NewGitProvider = func(git config.GitConfig, store credentials.Reader, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return githubprovider.NewFromGitConfig(git, store, opts)
		}
	}
	if d.ResolvePostingIdentity == nil {
		d.ResolvePostingIdentity = resolvePostingIdentity
	}
	if d.NewAdapter == nil {
		d.NewAdapter = newAdapter
	}
	if d.RuntimeLayout == nil {
		d.RuntimeLayout = runtimeLayout
	}
	if d.OpenLedger == nil {
		d.OpenLedger = ledger.Open
	}
	if d.NewLimiter == nil {
		d.NewLimiter = func() (outbox.Limiter, error) {
			return outbox.NewTokenBucket(livePostLimiterInterval, livePostLimiterBurst)
		}
	}
	return d
}

// Open builds the concrete runtime used by review lifecycle commands.
func Open(ctx context.Context, req OpenRequest) (Runtime, error) {
	deps := req.Dependencies.withDefaults()
	command := commandName(req.Command)
	profile := normalizeRuntimeProfile(req.Profile)
	stores := newRuntimeCredentialStores(req.Config, req.Backend, req.BackendFlagChanged, command, req.Progress)
	cleanup := stores.Close
	_, repoProviderStore, err := stores.Open(profile.Git.Credential)
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	repoProvider, _, err := deps.NewGitProvider(profile.Git, repoProviderStore, gitProviderOptions(profile, profile.Git, req.PRRef))
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	repoProvider = withProgressProvider(req.Progress, command, repoProvider)
	postingGit := gitConfigForReviewerAuth(profile)
	_, postingProviderStore, err := stores.Open(postingGit.Credential)
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	postingProvider, credential, err := deps.NewGitProvider(postingGit, postingProviderStore, gitProviderOptions(profile, postingGit, req.PRRef))
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	rawPostingProvider := postingProvider
	postingProvider = withProgressProvider(req.Progress, command, postingProvider)
	postingIdentity, err := deps.ResolvePostingIdentity(ctx, postingProvider, credential, postingProviderStore, profile)
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	if err := warnOpinionatedReviewAuthority(ctx, rawPostingProvider, req, postingIdentity, req.Warnings); err != nil {
		cleanup()
		return Runtime{}, err
	}
	layout, err := deps.RuntimeLayout()
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	ledgerStore, err := deps.OpenLedger(ctx, layout.LedgerDB())
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	cleanup = func() {
		_ = ledgerStore.Close()
		stores.Close()
	}
	limiter, err := deps.NewLimiter()
	if err != nil {
		cleanup()
		return Runtime{}, err
	}
	adapter := newLazyAdapter(func() (llm.Adapter, error) {
		adapterStore := repoProviderStore
		if profile.LLM.Auth == config.LLMAuthAPIKey {
			var err error
			_, adapterStore, err = stores.Open(profile.LLM.Credential)
			if err != nil {
				return nil, err
			}
		}
		adapter, err := deps.NewAdapter(profile.LLM, adapterStore)
		if err != nil {
			return nil, err
		}
		return withProgressAdapter(req.Progress, command, adapter, string(profile.LLM.Provider), string(profile.LLM.Adapter)), nil
	})
	runner := buildReviewRunner(ledgerStore, repoProvider, postingProvider, adapter, profile, limiter, layout, req.Warnings, req.Progress, req, command)
	return Runtime{
		Runner:          runner,
		Responder:       runner,
		PostingIdentity: postingIdentity,
		Cleanup:         cleanup,
	}, nil
}

func warnOpinionatedReviewAuthority(ctx context.Context, provider gitprovider.GitProvider, req OpenRequest, postingIdentity gitprovider.Identity, warnings io.Writer) error {
	if !req.RequireOpinionatedReviewAuthority {
		return nil
	}
	authority, err := provider.ReviewAuthority(ctx, req.PRRef, postingIdentity)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		writeReviewAuthorityWarning(warnings, postingIdentity, req.PRRef, "probe failed: "+err.Error())
		return nil
	}
	if authority.Eligible {
		return nil
	}
	detail := "permission unavailable"
	switch {
	case strings.TrimSpace(authority.Permission) != "":
		detail = "permission=" + authority.Permission
	case strings.TrimSpace(authority.RoleName) != "":
		detail = "role=" + authority.RoleName
	}
	writeReviewAuthorityWarning(warnings, postingIdentity, req.PRRef, detail)
	return nil
}

func writeReviewAuthorityWarning(warnings io.Writer, postingIdentity gitprovider.Identity, ref gitprovider.PRRef, detail string) {
	if warnings == nil {
		return
	}
	repo := fmt.Sprintf("%s/%s", ref.Owner, ref.Repo)
	_, _ = fmt.Fprintf(warnings, "warning: posting identity %q may not create GitHub reviews that count toward PR approval state for %s (%s); continuing because the review can still be posted\n", postingIdentity.Login, repo, detail)
}

func commandName(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "review"
	}
	return command
}

func gitConfigForReviewerAuth(profile config.Profile) config.GitConfig {
	if profile.ReviewerCredentials == nil {
		return profile.Git
	}
	var githubApp *config.GitHubAppConfig
	if profile.ReviewerCredentials.GitHubApp != nil {
		app := *profile.ReviewerCredentials.GitHubApp
		githubApp = &app
	}
	return config.GitConfig{
		Host:          profile.Git.Host,
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		Credential:    profile.ReviewerCredentials.Credential,
		GitHubApp:     githubApp,
		CredentialRef: profile.ReviewerCredentials.CredentialRef,
		IdentityCache: profile.ReviewerCredentials.IdentityCache,
	}
}

type runtimeCredentialStores struct {
	cfg                config.File
	backend            string
	backendFlagChanged bool
	command            string
	logger             *progress.Logger
	stores             map[string]runtimeCredentialStore
}

type runtimeCredentialStore struct {
	store  *credstore.Store
	reader credentials.Reader
}

func newRuntimeCredentialStores(cfg config.File, backend string, backendFlagChanged bool, command string, logger *progress.Logger) *runtimeCredentialStores {
	return &runtimeCredentialStores{
		cfg:                cfg,
		backend:            backend,
		backendFlagChanged: backendFlagChanged,
		command:            command,
		logger:             logger,
		stores:             map[string]runtimeCredentialStore{},
	}
}

func (s *runtimeCredentialStores) Open(location config.CredentialLocation) (*credstore.Store, credentials.Reader, error) {
	resolved, err := credentials.ResolveCredentialStore(s.cfg, location.Store)
	if err != nil {
		return nil, nil, err
	}
	if opened, ok := s.stores[resolved.ID]; ok {
		return opened.store, opened.reader, nil
	}
	store, err := credentials.OpenResolvedStore(s.backend, s.backendFlagChanged, s.cfg, resolved)
	if err != nil {
		return nil, nil, err
	}
	baseReader := credentials.ProgressStoreReader(s.command, s.logger, resolved, store)
	reader := credentials.ProgressCachingReader(s.command, s.logger, resolved.ID, resolved, baseReader)
	s.stores[resolved.ID] = runtimeCredentialStore{store: store, reader: reader}
	return store, reader, nil
}

func (s *runtimeCredentialStores) Close() {
	for id, opened := range s.stores {
		if closer, ok := opened.reader.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		_ = opened.store.Close()
		delete(s.stores, id)
	}
}

func normalizeRuntimeProfile(profile config.Profile) config.Profile {
	return config.Normalize(config.File{
		Profiles: map[string]config.Profile{"runtime": profile},
	}).Profiles["runtime"]
}

func gitProviderOptions(profile config.Profile, git config.GitConfig, ref gitprovider.PRRef) githubprovider.Options {
	opts := githubprovider.Options{}
	if installationID := config.PinnedGitHubAppInstallationIDForGit(profile, git); installationID != "" {
		opts.InstallationID = installationID
		return opts
	}
	if strings.TrimSpace(ref.Owner) != "" && strings.TrimSpace(ref.Repo) != "" {
		opts.InstallationLookup = &githubprovider.InstallationLookup{
			Owner: ref.Owner,
			Repo:  ref.Repo,
		}
	}
	return opts
}

func runtimeLayout() (statepaths.Layout, error) {
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		return statepaths.Layout{}, err
	}
	if err := statepaths.MigrateLegacyDataRoot(layout); err != nil {
		return statepaths.Layout{}, err
	}
	if err := statepaths.MigrateLegacyCacheRoot(layout); err != nil {
		return statepaths.Layout{}, err
	}
	return layout, nil
}

func buildReviewRunner(ledgerStore *ledger.Store, repoProvider gitprovider.GitProvider, postingProvider gitprovider.GitProvider, adapter llm.Adapter, profile config.Profile, limiter outbox.Limiter, layout statepaths.Layout, warnings io.Writer, logger *progress.Logger, req OpenRequest, command string) reviewRunner {
	taskProgress := newPipelineTaskProgress(logger, command)
	liveProvider := runtimeProvider{read: repoProvider, write: postingProvider}
	pipelineOpts := pipeline.Options{
		Provider:            repoProvider,
		Adapter:             adapter,
		Store:               ledgerStore,
		NamedSessions:       ledgerStore,
		Layout:              layout,
		Warnings:            warnings,
		TaskProgress:        taskProgress,
		MaxAgents:           req.MaxAgents,
		MaxConcurrency:      req.MaxConcurrency,
		Retention:           req.Retention,
		RetentionManualOnly: req.RetentionManualOnly,
		ResolveRepoRoot:     req.ResolveRepoRoot,
		GitCommand:          req.GitCommand,
	}
	return reviewRunner{
		pipeline: pipelineOpts,
		live: reviewrun.Options{
			Store:                   ledgerStore,
			Provider:                liveProvider,
			Planner:                 withProgressPlanner(logger, livePlanner{opts: pipelineOpts}),
			Limiter:                 limiter,
			Layout:                  layout,
			StaleHeartbeatThreshold: 10 * time.Minute,
			Warnings:                warnings,
			ApprovalOverride:        withProgressApprovalOverrideClassifier(logger, buildApprovalOverrideClassifier(profile, adapter, warnings)),
			Retention:               req.Retention,
			RetentionManualOnly:     req.RetentionManualOnly,
			ResolveRepoRoot:         req.ResolveRepoRoot,
		},
		respond: threadrespond.Options{
			Store:        ledgerStore,
			Provider:     liveProvider,
			Adapter:      adapter,
			Limiter:      limiter,
			Layout:       layout,
			TaskProgress: taskProgress,
			NewActionID:  pipelineOpts.NewActionID,
		},
	}
}

func buildApprovalOverrideClassifier(profile config.Profile, adapter llm.Adapter, warnings io.Writer) approvaloverride.Classifier {
	return &lazyApprovalOverrideClassifier{
		profile:  profile,
		adapter:  adapter,
		warnings: warnings,
	}
}

type lazyApprovalOverrideClassifier struct {
	mu         sync.Mutex
	profile    config.Profile
	adapter    llm.Adapter
	warnings   io.Writer
	classifier approvaloverride.Classifier
	disabled   bool
	loaded     bool
}

func (c *lazyApprovalOverrideClassifier) ClassifyApprovalOverride(ctx context.Context, req approvaloverride.Request) (approvaloverride.Result, error) {
	classifier, ok := c.get()
	if !ok {
		return approvaloverride.Result{}, nil
	}
	return classifier.ClassifyApprovalOverride(ctx, req)
}

func (c *lazyApprovalOverrideClassifier) get() (approvaloverride.Classifier, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.classifier, !c.disabled
	}
	c.loaded = true
	// Production passes a lazy adapter; keep direct callers/tests from
	// constructing an unusable classifier when no adapter is available.
	if c.adapter == nil {
		c.disabled = true
		return nil, false
	}
	resolved, ok := stagemodel.ResolveFirstAvailable(stagemodel.Request{
		Profile:       c.profile,
		Stage:         stagemodel.StageApprovalOverride,
		DefaultEffort: "low",
	}, config.ModelTierSmall, config.ModelTierMedium)
	if ok {
		if c.warnings != nil {
			if resolved.Tier == config.ModelTierMedium {
				_, _ = fmt.Fprintf(c.warnings, "warning: approval override classifier small model is not configured; falling back to medium tier model %s\n", resolved.Model)
			}
		}
		c.classifier = approvaloverride.NewLLMClassifier(c.adapter, resolved.Model, resolved.Effort)
		return c.classifier, true
	}
	if c.warnings != nil {
		_, _ = fmt.Fprintln(c.warnings, "warning: approval override classifier disabled because no small or medium model tier is configured")
	}
	c.disabled = true
	return nil, false
}

type lazyAdapter struct {
	mu      sync.Mutex
	factory func() (llm.Adapter, error)
	adapter llm.Adapter
	err     error
	loaded  bool
}

func newLazyAdapter(factory func() (llm.Adapter, error)) *lazyAdapter {
	return &lazyAdapter{factory: factory}
}

func (a *lazyAdapter) get() (llm.Adapter, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return a.adapter, a.err
	}
	a.loaded = true
	if a.factory == nil {
		a.err = fmt.Errorf("review: LLM adapter factory is required")
		return nil, a.err
	}
	a.adapter, a.err = a.factory()
	if a.err != nil {
		a.err = fmt.Errorf("review: initialize LLM adapter: %w", a.err)
	}
	return a.adapter, a.err
}

func (a *lazyAdapter) Name() string {
	adapter, err := a.get()
	if err != nil {
		return "unavailable"
	}
	return adapter.Name()
}

func (a *lazyAdapter) SupportsResume() bool {
	adapter, err := a.get()
	if err != nil {
		return false
	}
	return adapter.SupportsResume()
}

func (a *lazyAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	adapter, err := a.get()
	if err != nil {
		return llm.ReviewerWorkspaceNone
	}
	return llm.AdapterReviewerWorkspaceMode(adapter)
}

func (a *lazyAdapter) SupportsCacheAccounting() bool {
	adapter, err := a.get()
	if err != nil {
		return false
	}
	return adapter.SupportsCacheAccounting()
}

func (a *lazyAdapter) SupportsCostReporting() bool {
	adapter, err := a.get()
	if err != nil {
		return false
	}
	return adapter.SupportsCostReporting()
}

func (a *lazyAdapter) Quota(ctx context.Context) (llm.Quota, bool, error) {
	adapter, err := a.get()
	if err != nil {
		return llm.Quota{}, false, err
	}
	return adapter.Quota(ctx)
}

func (a *lazyAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	adapter, err := a.get()
	if err != nil {
		return nil, err
	}
	return adapter.Start(ctx, req)
}

func (a *lazyAdapter) Resume(ctx context.Context, sessionID string, req llm.Request) (llm.Stream, error) {
	adapter, err := a.get()
	if err != nil {
		return nil, err
	}
	return adapter.Resume(ctx, sessionID, req)
}

type reviewRunner struct {
	pipeline pipeline.Options
	live     reviewrun.Options
	respond  threadrespond.Options
}

func (r reviewRunner) DryRun(ctx context.Context, req pipeline.Request) (pipeline.Result, error) {
	return pipeline.DryRun(ctx, r.pipeline, req)
}

func (r reviewRunner) Live(ctx context.Context, req pipeline.Request, flags reviewrun.Flags) (reviewrun.Result, error) {
	return reviewrun.Run(ctx, r.live, reviewrun.Request{Pipeline: req, Flags: flags})
}

func (r reviewRunner) Respond(ctx context.Context, req threadrespond.Request) (threadrespond.Result, error) {
	opts := r.respond
	if opts.Acquire == nil && r.live.Acquire != nil {
		opts.Acquire = func(path string) (threadrespond.Lock, error) {
			return r.live.Acquire(path)
		}
	}
	if opts.Now == nil {
		opts.Now = r.live.Now
	}
	if opts.NewRunID == nil {
		opts.NewRunID = r.live.NewRunID
	}
	return threadrespond.Run(ctx, opts, req)
}

type livePlanner struct {
	opts pipeline.Options
}

func (p livePlanner) Live(ctx context.Context, req pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	return pipeline.Live(ctx, p.opts, req, run)
}

func resolvePostingIdentity(ctx context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, _ credentials.Reader, _ config.Profile) (gitprovider.Identity, error) {
	return provider.WhoAmI(ctx, credential)
}

func newAdapter(llmConfig config.LLMConfig, store credentials.Reader) (llm.Adapter, error) {
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

var _ Runner = reviewRunner{}
var _ ResponseRunner = reviewRunner{}
