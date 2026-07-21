package app

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitproviders"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
)

// SelectionRuntime contains the dependencies needed for selection-only
// execution paths that must match review-command runtime semantics.
type SelectionRuntime struct {
	Select  func(context.Context, pipeline.SelectionRequest) (pipeline.SelectionResult, error)
	Cleanup func()
}

// OpenSelection resolves provider and adapter setup using the same
// semantics as the real review command.
func OpenSelection(ctx context.Context, req SelectionOpenRequest) (SelectionRuntime, error) {
	_ = ctx
	deps := req.Dependencies.withDefaults()
	backend := req.Backend
	backendFlagChanged := req.BackendFlagChanged
	cfg := req.Config
	profile := req.Profile
	profile = normalizeRuntimeProfile(profile)
	stores := newRuntimeCredentialStores(cfg, backend, backendFlagChanged, "", nil)
	cleanup := stores.Close
	_, gitStore, err := stores.Open(profile.Git.Credential)
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	// Selection-only paths read with the profile git credential while keeping
	// PR-scoped GitHub App installation lookup aligned with review runs.
	provider, credential, err := deps.NewGitProvider(profile.Git, gitStore, gitProviderOptions(profile, profile.Git, req.PRRef))
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	gitCommand, err := deps.NewGitCommand(profile.Git.Host, repositoryTokenSource(provider, credential), gitproviders.GitBasicAuthUsername(profile.Git.ProviderKind()))
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	adapterStore := gitStore
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		_, adapterStore, err = stores.Open(profile.LLM.Credential)
		if err != nil {
			cleanup()
			return SelectionRuntime{}, err
		}
	}
	adapter, err := deps.NewAdapter(profile.LLM, adapterStore)
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	return SelectionRuntime{
		Select: func(ctx context.Context, selection pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			return pipeline.SelectionOnly(ctx, pipeline.Options{
				Provider:        provider,
				Adapter:         adapter,
				GitCommand:      gitCommand,
				ResolveRepoRoot: deps.ResolveRepoRoot,
				MaxAgents:       selection.MaxAgents,
			}, selection)
		},
		Cleanup: cleanup,
	}, nil
}
