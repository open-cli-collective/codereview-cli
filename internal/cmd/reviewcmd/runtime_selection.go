package reviewcmd

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
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

// OpenSelectionRuntime resolves provider and adapter setup using the same
// semantics as the real review command.
func OpenSelectionRuntime(_ context.Context, backend string, backendFlagChanged bool, cfg config.File, profile config.Profile) (SelectionRuntime, error) {
	profile = normalizeRuntimeProfile(profile)
	stores := newRuntimeCredentialStores(cfg, backend, backendFlagChanged)
	cleanup := stores.Close
	gitStore, err := stores.Open(profile.Git.Credential)
	if err != nil {
		cleanup()
		return SelectionRuntime{}, cmdruntime.MapRunError(err)
	}
	// Selection-only paths read with the profile git credential. Reviewer
	// credentials are applied by the review runtime where posting identity and
	// PR-scoped GitHub App installation lookup are available.
	provider, _, err := newGitProvider(profile.Git, gitStore, githubprovider.Options{})
	if err != nil {
		cleanup()
		return SelectionRuntime{}, cmdruntime.MapRunError(err)
	}
	adapterStore := gitStore
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		adapterStore, err = stores.Open(profile.LLM.Credential)
		if err != nil {
			cleanup()
			return SelectionRuntime{}, cmdruntime.MapRunError(err)
		}
	}
	adapter, err := newAdapterForRuntime(profile.LLM, adapterStore)
	if err != nil {
		cleanup()
		return SelectionRuntime{}, cmdruntime.MapRunError(err)
	}
	return SelectionRuntime{
		Provider: provider,
		Adapter:  adapter,
		Cleanup:  cleanup,
	}, nil
}
