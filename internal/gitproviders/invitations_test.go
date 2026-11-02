package gitproviders

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	gitlabprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/gitlab"
)

func TestNewInvitationClientDispatchesOnProviderKind(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	client, credential, err := NewInvitationClient(testGitConfig("", "github.example.com"), store, Options{})
	if err != nil {
		t.Fatalf("NewInvitationClient github: %v", err)
	}
	if _, ok := client.(*githubprovider.Client); !ok {
		t.Fatalf("client = %T, want github client for empty provider kind", client)
	}
	if credential.Type != "pat" || credential.Token != "token" {
		t.Fatalf("credential = %#v, want PAT", credential)
	}
	client, _, err = NewInvitationClient(testGitConfig(config.GitProviderGitLab, "gitlab.example.com"), store, Options{})
	if err != nil {
		t.Fatalf("NewInvitationClient gitlab: %v", err)
	}
	if _, ok := client.(*gitlabprovider.Client); !ok {
		t.Fatalf("client = %T, want gitlab client", client)
	}
}

func TestNewInvitationClientReturnsNilClientOnError(t *testing.T) {
	for _, provider := range []config.GitProviderKind{config.GitProviderGitHub, config.GitProviderGitLab} {
		client, _, err := NewInvitationClient(testGitConfig(provider, "example.com"), tokenStore{}, Options{})
		if !errors.Is(err, errTokenNotFound) {
			t.Fatalf("%s error = %v, want missing token", provider, err)
		}
		if client != nil {
			t.Fatalf("%s client = %#v, want a nil interface on error", provider, client)
		}
	}
}
