package invitations

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestNewAllowlistValidatesAndDeduplicates(t *testing.T) {
	allow, err := NewAllowlist([]string{" example-org ", "Example-Org", "other"}, []string{"a/b", "A/B", " c/d "})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	if !reflect.DeepEqual(allow.Owners, []string{"example-org", "other"}) || !reflect.DeepEqual(allow.Repos, []string{"a/b", "c/d"}) {
		t.Fatalf("allowlist = %#v, want trimmed case-insensitive unique entries", allow)
	}
	if empty, err := NewAllowlist(nil, nil); err != nil || !empty.Empty() {
		t.Fatalf("NewAllowlist(nil, nil) = %#v, %v; want empty allowlist", empty, err)
	}
	for _, owner := range []string{"", " ", "org/repo", "two words"} {
		if _, err := NewAllowlist([]string{owner}, nil); !errors.Is(err, ErrInvalidAllowlist) {
			t.Fatalf("owner %q error = %v, want ErrInvalidAllowlist", owner, err)
		}
	}
	for _, repo := range []string{"", "org", "org/", "/repo", "org/repo/extra", "org /repo", "org/re po"} {
		if _, err := NewAllowlist(nil, []string{repo}); !errors.Is(err, ErrInvalidAllowlist) {
			t.Fatalf("repo %q error = %v, want ErrInvalidAllowlist", repo, err)
		}
	}
}

func TestAllowlistMatchesOwnerOrRepositoryCaseInsensitively(t *testing.T) {
	allow, err := NewAllowlist([]string{"Example-Org"}, []string{"other-org/Tooling"})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	tests := []struct {
		invitation gitprovider.RepositoryInvitation
		want       bool
	}{
		{gitprovider.RepositoryInvitation{Owner: "example-org", Repo: "anything"}, true},
		{gitprovider.RepositoryInvitation{Owner: "OTHER-ORG", Repo: "tooling"}, true},
		{gitprovider.RepositoryInvitation{Owner: "other-org", Repo: "tooling-extra"}, false},
		{gitprovider.RepositoryInvitation{Owner: "example-org-fork", Repo: "service"}, false},
	}
	for _, tt := range tests {
		if got := allow.Matches(tt.invitation); got != tt.want {
			t.Fatalf("Matches(%s) = %v, want %v", tt.invitation.FullName(), got, tt.want)
		}
	}
}

func TestListSortsAndFilters(t *testing.T) {
	fake := fakeWithInvitations()
	all, err := List(context.Background(), fake, Allowlist{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := fullNames(all); !reflect.DeepEqual(got, []string{"example-org/docs", "example-org/service", "other-org/expired", "other-org/tooling"}) {
		t.Fatalf("List = %v, want sorted pending invitations", got)
	}
	allow, _ := NewAllowlist(nil, []string{"other-org/tooling"})
	filtered, err := List(context.Background(), fake, allow)
	if err != nil {
		t.Fatalf("List filtered: %v", err)
	}
	if got := fullNames(filtered); !reflect.DeepEqual(got, []string{"other-org/tooling"}) {
		t.Fatalf("filtered = %v, want only other-org/tooling", got)
	}
}

func TestAcceptRequiresAllowlistBeforeAnyProviderCall(t *testing.T) {
	fake := fakeWithInvitations()
	var calls []gitprovider.Operation
	fake.SetCallHook(func(op gitprovider.Operation) { calls = append(calls, op) })

	_, err := Accept(context.Background(), fake, Allowlist{}, false)
	if !errors.Is(err, ErrAllowlistRequired) {
		t.Fatalf("Accept error = %v, want ErrAllowlistRequired", err)
	}
	if len(calls) != 0 {
		t.Fatalf("provider calls = %v, want none", calls)
	}
}

func TestAcceptReportsEveryInvitationAndAcceptsOnlyAllowlisted(t *testing.T) {
	fake := fakeWithInvitations()
	allow, _ := NewAllowlist([]string{"example-org", "other-org"}, nil)

	outcomes, err := Accept(context.Background(), fake, allow, false)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	got := statuses(outcomes)
	want := map[string]Status{
		"example-org/docs":    StatusAccepted,
		"example-org/service": StatusAccepted,
		"other-org/expired":   StatusSkipped,
		"other-org/tooling":   StatusAccepted,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if outcomes[2].Reason != ReasonExpired {
		t.Fatalf("expired reason = %q, want %q", outcomes[2].Reason, ReasonExpired)
	}
	if accepted := fake.RecordedAcceptedInvitations(); !reflect.DeepEqual(accepted, []gitprovider.InvitationID{"3", "1", "2"}) {
		t.Fatalf("accepted = %v, want sorted allowlisted unexpired invitations", accepted)
	}

	narrow := fakeWithInvitations()
	repoOnly, _ := NewAllowlist(nil, []string{"example-org/service"})
	outcomes, err = Accept(context.Background(), narrow, repoOnly, false)
	if err != nil {
		t.Fatalf("Accept narrow: %v", err)
	}
	for _, outcome := range outcomes {
		if outcome.Invitation.FullName() != "example-org/service" && (outcome.Status != StatusSkipped || outcome.Reason != ReasonNotAllowlisted) {
			t.Fatalf("outcome %s = %s (%s), want skipped off the allowlist", outcome.Invitation.FullName(), outcome.Status, outcome.Reason)
		}
	}
	if accepted := narrow.RecordedAcceptedInvitations(); !reflect.DeepEqual(accepted, []gitprovider.InvitationID{"1"}) {
		t.Fatalf("accepted = %v, want only invitation 1", accepted)
	}
}

func TestAcceptDryRunMakesNoWrites(t *testing.T) {
	fake := fakeWithInvitations()
	var calls []gitprovider.Operation
	fake.SetCallHook(func(op gitprovider.Operation) { calls = append(calls, op) })
	allow, _ := NewAllowlist([]string{"example-org"}, nil)

	outcomes, err := Accept(context.Background(), fake, allow, true)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !reflect.DeepEqual(calls, []gitprovider.Operation{gitprovider.OperationListRepositoryInvitations}) {
		t.Fatalf("provider calls = %v, want only the listing", calls)
	}
	if summary := Summarize(outcomes); summary != (Summary{WouldAccept: 2, Skipped: 2}) {
		t.Fatalf("summary = %#v, want two would_accept and two skipped", summary)
	}
}

func TestAcceptContinuesPastFailures(t *testing.T) {
	fake := fakeWithInvitations()
	failure := gitprovider.WrapError(gitprovider.ErrPermission, gitprovider.OperationAcceptRepositoryInvitation, errors.New("github: status 403"))
	fake.SetAcceptInvitationError("3", failure)
	allow, _ := NewAllowlist([]string{"example-org"}, nil)

	outcomes, err := Accept(context.Background(), fake, allow, false)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcomes[0].Status != StatusFailed || !errors.Is(outcomes[0].Err, gitprovider.ErrPermission) || outcomes[0].Reason != ReasonPermission {
		t.Fatalf("first outcome = %#v, want failed with the provider error", outcomes[0])
	}
	if outcomes[1].Status != StatusAccepted {
		t.Fatalf("second outcome = %#v, want accepted after the failure", outcomes[1])
	}
	if summary := Summarize(outcomes); summary != (Summary{Accepted: 1, Skipped: 2, Failed: 1}) {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestAcceptReturnsListingFailure(t *testing.T) {
	fake := fakeWithInvitations()
	fake.SetError(gitprovider.OperationListRepositoryInvitations, gitprovider.ErrAuth)
	allow, _ := NewAllowlist([]string{"example-org"}, nil)

	if _, err := Accept(context.Background(), fake, allow, false); !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("Accept error = %v, want ErrAuth", err)
	}
	if accepted := fake.RecordedAcceptedInvitations(); len(accepted) != 0 {
		t.Fatalf("accepted = %v, want none", accepted)
	}
}

func TestFailureReasonClassifiesProviderErrors(t *testing.T) {
	tests := []struct {
		err  error
		want ReasonCode
	}{
		{gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationAcceptRepositoryInvitation, nil), ReasonAuth},
		{gitprovider.WrapError(gitprovider.ErrPermission, gitprovider.OperationAcceptRepositoryInvitation, nil), ReasonPermission},
		{gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationAcceptRepositoryInvitation, nil), ReasonNotFound},
		{gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationAcceptRepositoryInvitation, nil), ReasonConflict},
		{gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationAcceptRepositoryInvitation, nil), ReasonRetryable},
		{gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationAcceptRepositoryInvitation, fmt.Errorf("client timeout: %w", context.DeadlineExceeded)), ReasonRetryable},
		{context.DeadlineExceeded, ReasonRetryable},
		{context.Canceled, ReasonCanceled},
		{errors.New("github: validation error"), ReasonError},
	}
	for _, tt := range tests {
		if got := failureReason(tt.err); got != tt.want {
			t.Fatalf("failureReason(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func fakeWithInvitations() *gitprovider.Fake {
	fake := &gitprovider.Fake{}
	fake.SetRepositoryInvitations([]gitprovider.RepositoryInvitation{
		{ID: "1", Owner: "example-org", Repo: "service", Permission: "write"},
		{ID: "2", Owner: "other-org", Repo: "tooling", Permission: "read"},
		{ID: "3", Owner: "example-org", Repo: "docs", Permission: "write"},
		{ID: "4", Owner: "other-org", Repo: "expired", Permission: "write", Expired: true},
	})
	return fake
}

func fullNames(invitations []gitprovider.RepositoryInvitation) []string {
	names := make([]string, 0, len(invitations))
	for _, invitation := range invitations {
		names = append(names, invitation.FullName())
	}
	return names
}

func statuses(outcomes []Outcome) map[string]Status {
	out := make(map[string]Status, len(outcomes))
	for _, outcome := range outcomes {
		out[outcome.Invitation.FullName()] = outcome.Status
	}
	return out
}
