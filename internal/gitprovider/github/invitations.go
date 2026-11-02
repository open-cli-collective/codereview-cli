package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

type repositoryInvitationResponse struct {
	ID          int64         `json:"id"`
	Repository  repoResponse  `json:"repository"`
	Invitee     *userResponse `json:"invitee"`
	Inviter     *userResponse `json:"inviter"`
	Permissions string        `json:"permissions"`
	CreatedAt   time.Time     `json:"created_at"`
	Expired     bool          `json:"expired"`
	HTMLURL     string        `json:"html_url"`
}

var _ gitprovider.RepositoryInvitations = (*Client)(nil)

// ListRepositoryInvitations returns the pending repository invitations
// addressed to the authenticated user.
func (c *Client) ListRepositoryInvitations(ctx context.Context) ([]gitprovider.RepositoryInvitation, error) {
	const op = gitprovider.OperationListRepositoryInvitations
	if err := c.requireUserToken(op); err != nil {
		return nil, err
	}
	endpoint := restURL(c.baseURL, "user", "repository_invitations") + "?per_page=100"
	payloads, err := doRESTPages[repositoryInvitationResponse](ctx, c, op, endpoint, acceptJSON)
	if err != nil {
		return nil, err
	}
	invitations := make([]gitprovider.RepositoryInvitation, 0, len(payloads))
	for _, payload := range payloads {
		invitation, err := repositoryInvitationFromResponse(payload)
		if err != nil {
			return nil, err
		}
		invitations = append(invitations, invitation)
	}
	return invitations, nil
}

// AcceptRepositoryInvitation accepts one pending repository invitation for
// the authenticated user.
func (c *Client) AcceptRepositoryInvitation(ctx context.Context, id gitprovider.InvitationID) error {
	const op = gitprovider.OperationAcceptRepositoryInvitation
	if err := c.requireUserToken(op); err != nil {
		return err
	}
	numericID, err := strconv.ParseInt(strings.TrimSpace(string(id)), 10, 64)
	if err != nil || numericID <= 0 {
		return fmt.Errorf("%w: invitation ID %q must be a positive integer", ErrValidation, id)
	}
	endpoint := restURL(c.baseURL, "user", "repository_invitations", strconv.FormatInt(numericID, 10))
	_, _, err = c.doREST(ctx, op, http.MethodPatch, endpoint, acceptJSON, nil)
	return err
}

// requireUserToken rejects GitHub App clients: installation tokens act for an
// app installation, which has no repository invitations of its own.
func (c *Client) requireUserToken(op gitprovider.Operation) error {
	if c == nil {
		return fmt.Errorf("%w: nil client", ErrValidation)
	}
	if c.appAuth != nil {
		return fmt.Errorf("%w: %s requires a personal access token; GitHub App installations are granted repository access by installation, not invitation", config.ErrUnsupported, op)
	}
	return nil
}

func repositoryInvitationFromResponse(payload repositoryInvitationResponse) (gitprovider.RepositoryInvitation, error) {
	if payload.ID <= 0 {
		return gitprovider.RepositoryInvitation{}, fmt.Errorf("%w: repository invitation without an ID", ErrValidation)
	}
	owner := strings.TrimSpace(payload.Repository.Owner.Login)
	repo := strings.TrimSpace(payload.Repository.Name)
	if owner == "" || repo == "" {
		return gitprovider.RepositoryInvitation{}, fmt.Errorf("%w: repository invitation %d without a repository owner and name", ErrValidation, payload.ID)
	}
	invitation := gitprovider.RepositoryInvitation{
		ID:         gitprovider.InvitationID(strconv.FormatInt(payload.ID, 10)),
		Owner:      owner,
		Repo:       repo,
		Permission: strings.TrimSpace(payload.Permissions),
		CreatedAt:  payload.CreatedAt,
		Expired:    payload.Expired,
		URL:        payload.HTMLURL,
	}
	if payload.Inviter != nil {
		invitation.Inviter = identityFromUser(*payload.Inviter)
	}
	if payload.Invitee != nil {
		invitation.Invitee = identityFromUser(*payload.Invitee)
	}
	return invitation, nil
}
