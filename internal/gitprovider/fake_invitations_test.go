package gitprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFakeRepositoryInvitationsAcceptRemovesPendingAndRecords(t *testing.T) {
	var fake Fake
	var calls []Operation
	fake.SetCallHook(func(op Operation) { calls = append(calls, op) })
	fake.SetRepositoryInvitations([]RepositoryInvitation{
		{ID: "1", Owner: "example-org", Repo: "service"},
		{ID: "2", Owner: "example-org", Repo: "docs"},
	})

	if err := fake.AcceptRepositoryInvitation(context.Background(), "1"); err != nil {
		t.Fatalf("AcceptRepositoryInvitation: %v", err)
	}
	pending, err := fake.ListRepositoryInvitations(context.Background())
	if err != nil {
		t.Fatalf("ListRepositoryInvitations: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "2" {
		t.Fatalf("pending = %#v, want only invitation 2", pending)
	}
	if got := fake.RecordedAcceptedInvitations(); !reflect.DeepEqual(got, []InvitationID{"1"}) {
		t.Fatalf("accepted = %#v, want [1]", got)
	}
	if err := fake.AcceptRepositoryInvitation(context.Background(), "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second accept error = %v, want ErrNotFound", err)
	}
	if err := fake.AcceptRepositoryInvitation(context.Background(), " "); err == nil {
		t.Fatal("blank invitation ID error = nil")
	}
	want := []Operation{OperationAcceptRepositoryInvitation, OperationListRepositoryInvitations, OperationAcceptRepositoryInvitation, OperationAcceptRepositoryInvitation}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestFakeRepositoryInvitationsInjectErrors(t *testing.T) {
	var fake Fake
	fake.SetRepositoryInvitations([]RepositoryInvitation{{ID: "1", Owner: "o", Repo: "a"}, {ID: "2", Owner: "o", Repo: "b"}})
	perID := errors.New("per-id failure")
	fake.SetAcceptInvitationError("1", perID)

	if err := fake.AcceptRepositoryInvitation(context.Background(), "1"); !errors.Is(err, perID) {
		t.Fatalf("accept 1 error = %v, want per-id failure", err)
	}
	if err := fake.AcceptRepositoryInvitation(context.Background(), "2"); err != nil {
		t.Fatalf("accept 2: %v", err)
	}
	fake.SetAcceptInvitationError("1", nil)
	opErr := errors.New("op failure")
	fake.SetError(OperationAcceptRepositoryInvitation, opErr)
	if err := fake.AcceptRepositoryInvitation(context.Background(), "1"); !errors.Is(err, opErr) {
		t.Fatalf("accept 1 with op error = %v, want op failure", err)
	}
	fake.SetError(OperationListRepositoryInvitations, opErr)
	if _, err := fake.ListRepositoryInvitations(context.Background()); !errors.Is(err, opErr) {
		t.Fatalf("list error = %v, want op failure", err)
	}
	if got := fake.RecordedAcceptedInvitations(); !reflect.DeepEqual(got, []InvitationID{"2"}) {
		t.Fatalf("accepted = %#v, want [2]", got)
	}
}
