package gitproviders

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	gitlabprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/gitlab"
)

// InvitationClient is a host adapter's identity lookup plus the repository
// invitations addressed to that identity.
type InvitationClient interface {
	WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error)
	gitprovider.RepositoryInvitations
}

// NewInvitationClient builds the adapter selected by the git config, typed for
// repository invitations, along with the credential used to authenticate it.
func NewInvitationClient(git config.GitConfig, store credentials.Reader, opts Options) (InvitationClient, gitprovider.Credential, error) {
	if git.ProviderKind() == config.GitProviderGitLab {
		client, credential, err := gitlabprovider.NewFromGitConfig(git, store, opts.GitLab)
		if err != nil {
			return nil, gitprovider.Credential{}, err
		}
		return client, credential, nil
	}
	client, credential, err := githubprovider.NewFromGitConfig(git, store, opts.GitHub)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	return client, credential, nil
}
