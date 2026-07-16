// Package gitproviders selects the concrete git-provider adapter for a git
// config. It is the single construction seam commands and the app runtime use
// so provider dispatch stays out of call sites.
package gitproviders

import (
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	gitlabprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/gitlab"
)

// Options carries provider-specific construction options; only the block
// matching the config's provider kind is consulted.
type Options struct {
	GitHub githubprovider.Options
	GitLab gitlabprovider.Options
}

// New builds the provider adapter selected by the git config along with the
// credential used to authenticate it.
func New(git config.GitConfig, store credentials.Reader, opts Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
	switch git.ProviderKind() {
	case config.GitProviderGitLab:
		return gitlabprovider.NewFromGitConfig(git, store, opts.GitLab)
	case config.GitProviderGitHub:
		return githubprovider.NewFromGitConfig(git, store, opts.GitHub)
	default:
		return githubprovider.NewFromGitConfig(git, store, opts.GitHub)
	}
}

// GitBasicAuthUsername returns the username the git HTTP transport pairs with
// the provider token when fetching over HTTPS.
func GitBasicAuthUsername(kind config.GitProviderKind) string {
	if kind == config.GitProviderGitLab {
		return "oauth2"
	}
	return "x-access-token"
}
