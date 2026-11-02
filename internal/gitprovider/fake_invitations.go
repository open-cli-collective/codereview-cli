package gitprovider

import "context"

type fakeInvitations struct {
	pending      []RepositoryInvitation
	acceptErrors map[InvitationID]error
	accepted     []InvitationID
}

var _ RepositoryInvitations = (*Fake)(nil)

// SetRepositoryInvitations configures the pending invitations returned by
// ListRepositoryInvitations.
func (f *Fake) SetRepositoryInvitations(invitations []RepositoryInvitation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invitations.pending = append([]RepositoryInvitation(nil), invitations...)
}

// SetAcceptInvitationError injects err for accepting one invitation. Passing
// nil clears it. Use SetError with OperationAcceptRepositoryInvitation to fail
// every accept.
func (f *Fake) SetAcceptInvitationError(id InvitationID, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invitations.acceptErrors == nil {
		f.invitations.acceptErrors = make(map[InvitationID]error)
	}
	if err == nil {
		delete(f.invitations.acceptErrors, id)
		return
	}
	f.invitations.acceptErrors[id] = err
}

// ListRepositoryInvitations returns the configured pending invitations.
func (f *Fake) ListRepositoryInvitations(_ context.Context) ([]RepositoryInvitation, error) {
	f.call(OperationListRepositoryInvitations)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationListRepositoryInvitations); err != nil {
		return nil, err
	}
	return append([]RepositoryInvitation(nil), f.invitations.pending...), nil
}

// AcceptRepositoryInvitation accepts one pending invitation, removing it from
// the pending list and recording the write.
func (f *Fake) AcceptRepositoryInvitation(_ context.Context, id InvitationID) error {
	f.call(OperationAcceptRepositoryInvitation)
	if err := requireNonEmpty("invitation ID", string(id)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationAcceptRepositoryInvitation); err != nil {
		return err
	}
	if err := f.invitations.acceptErrors[id]; err != nil {
		return err
	}
	for index, invitation := range f.invitations.pending {
		if invitation.ID != id {
			continue
		}
		f.invitations.pending = append(f.invitations.pending[:index:index], f.invitations.pending[index+1:]...)
		f.invitations.accepted = append(f.invitations.accepted, id)
		return nil
	}
	return WrapError(ErrNotFound, OperationAcceptRepositoryInvitation, nil)
}

// RecordedAcceptedInvitations returns successful invitation accepts in order.
func (f *Fake) RecordedAcceptedInvitations() []InvitationID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InvitationID(nil), f.invitations.accepted...)
}
