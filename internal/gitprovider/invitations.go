package gitprovider

import (
	"context"
	"time"
)

// InvitationID identifies a pending repository invitation on one host.
type InvitationID string

// RepositoryInvitation is a pending invitation for the authenticated identity
// to collaborate on one repository.
type RepositoryInvitation struct {
	ID         InvitationID
	Owner      string
	Repo       string
	Inviter    Identity
	Invitee    Identity
	Permission string
	CreatedAt  time.Time
	Expired    bool
	URL        string
}

// FullName returns the owner/repo name of the invited repository.
func (i RepositoryInvitation) FullName() string {
	return i.Owner + "/" + i.Repo
}

// RepositoryInvitations lists and accepts the pending repository invitations
// addressed to the authenticated identity. It is separate from GitProvider
// because it acts on the account rather than on a pull request, so review
// runtime wrappers need not forward it.
//
// Every host adapter implements it, so gitproviders can construct an
// invitation client for any configured host without a capability check. A
// host or credential with no invitations to accept, such as GitLab or a
// GitHub App installation, returns an error wrapping config.ErrUnsupported
// from both methods; the adapter owns that decision.
type RepositoryInvitations interface {
	ListRepositoryInvitations(ctx context.Context) ([]RepositoryInvitation, error)
	AcceptRepositoryInvitation(ctx context.Context, id InvitationID) error
}
