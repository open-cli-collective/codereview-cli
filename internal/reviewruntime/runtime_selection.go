package reviewruntime

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

// SelectionRuntime contains the dependencies needed for selection-only
// execution paths that must match review-command runtime semantics.
type SelectionRuntime struct {
	Provider gitprovider.GitProvider
	Adapter  llm.Adapter
	Cleanup  func()
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
	stores := newRuntimeCredentialStores(cfg, backend, backendFlagChanged)
	cleanup := stores.Close
	gitStore, err := stores.Open(profile.Git.Credential)
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	// Selection-only paths read with the profile git credential. Reviewer
	// credentials are applied by the review runtime where posting identity and
	// PR-scoped GitHub App installation lookup are available.
	provider, _, err := deps.NewGitProvider(profile.Git, gitStore, githubprovider.Options{})
	if err != nil {
		cleanup()
		return SelectionRuntime{}, err
	}
	adapterStore := gitStore
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		adapterStore, err = stores.Open(profile.LLM.Credential)
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
		Provider: provider,
		Adapter:  adapter,
		Cleanup:  cleanup,
	}, nil
}
