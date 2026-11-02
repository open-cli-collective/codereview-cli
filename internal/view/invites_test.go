package view

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/invitations"
)

func TestInvitesTextWithNoPendingInvitations(t *testing.T) {
	identity := InvitesIdentity{Profile: "reviewer", CredentialSource: "git", Host: "github.com", Login: "review-bot"}

	var list bytes.Buffer
	if err := RenderInvitesListText(&list, NewInvitesList(identity, nil)); err != nil {
		t.Fatalf("RenderInvitesListText: %v", err)
	}
	wantList := "Profile: reviewer\nCredential source: git\nHost: github.com\nLogin: review-bot\nInvitations: none\n"
	if list.String() != wantList {
		t.Fatalf("list text = %q, want %q", list.String(), wantList)
	}

	allow, err := invitations.NewAllowlist([]string{"example-org"}, nil)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	var accept bytes.Buffer
	if err := RenderInvitesAcceptText(&accept, NewInvitesAccept(identity, allow, true, nil)); err != nil {
		t.Fatalf("RenderInvitesAcceptText: %v", err)
	}
	if !strings.HasSuffix(accept.String(), "Dry run: true\nInvitations: none\nSummary: accepted=0 would_accept=0 skipped=0 failed=0\n") {
		t.Fatalf("accept text = %q", accept.String())
	}
}

func TestInvitesAcceptJSONKeepsEmptyAllowlistArrays(t *testing.T) {
	allow, err := invitations.NewAllowlist(nil, []string{"example-org/service"})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	var out bytes.Buffer
	if err := RenderJSON(&out, NewInvitesAccept(InvitesIdentity{}, allow, false, nil)); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	for _, want := range []string{`"owners": []`, `"repos": [`, `"results": []`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("JSON missing %s:\n%s", want, out.String())
		}
	}
}
