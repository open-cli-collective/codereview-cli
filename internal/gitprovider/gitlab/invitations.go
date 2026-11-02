package gitlab

import (
	"context"
	"fmt"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

var _ gitprovider.RepositoryInvitations = (*Client)(nil)

// ListRepositoryInvitations is unsupported: GitLab adds an existing user to a
// project as a member directly, so there is no pending invitation to list.
func (c *Client) ListRepositoryInvitations(context.Context) ([]gitprovider.RepositoryInvitation, error) {
	return nil, errInvitationsUnsupported(gitprovider.OperationListRepositoryInvitations)
}

// AcceptRepositoryInvitation is unsupported for the same reason as
// ListRepositoryInvitations.
func (c *Client) AcceptRepositoryInvitation(context.Context, gitprovider.InvitationID) error {
	return errInvitationsUnsupported(gitprovider.OperationAcceptRepositoryInvitation)
}

func errInvitationsUnsupported(op gitprovider.Operation) error {
	return fmt.Errorf("%w: %s for provider gitlab; GitLab adds existing users as project members without an invitation to accept", config.ErrUnsupported, op)
}
