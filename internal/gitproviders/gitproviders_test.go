package gitproviders

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	gitlabprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/gitlab"
)

var errTokenNotFound = errors.New("token not found")

type tokenStore map[string]map[string]string

func (s tokenStore) Get(profile, key string) (string, error) {
	keys, ok := s[profile]
	if !ok {
		return "", errTokenNotFound
	}
	value, ok := keys[key]
	if !ok {
		return "", errTokenNotFound
	}
	return value, nil
}

func testGitConfig(provider config.GitProviderKind, host string) config.GitConfig {
	return config.GitConfig{
		Provider:   provider,
		Host:       host,
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
	}
}

func TestNewDispatchesOnProviderKind(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}

	provider, credential, err := New(testGitConfig("", "github.example.com"), store, Options{})
	if err != nil {
		t.Fatalf("New github: %v", err)
	}
	if _, ok := provider.(*githubprovider.Client); !ok {
		t.Fatalf("provider = %T, want github client for empty provider kind", provider)
	}
	if credential.Type != "pat" || credential.Token != "token" {
		t.Fatalf("credential = %#v, want PAT", credential)
	}

	provider, _, err = New(testGitConfig(config.GitProviderGitLab, "gitlab.example.com"), store, Options{})
	if err != nil {
		t.Fatalf("New gitlab: %v", err)
	}
	if _, ok := provider.(*gitlabprovider.Client); !ok {
		t.Fatalf("provider = %T, want gitlab client", provider)
	}
}

func TestGitBasicAuthUsername(t *testing.T) {
	if got := GitBasicAuthUsername(config.GitProviderGitHub); got != "x-access-token" {
		t.Fatalf("github username = %q, want x-access-token", got)
	}
	if got := GitBasicAuthUsername(""); got != "x-access-token" {
		t.Fatalf("default username = %q, want x-access-token", got)
	}
	if got := GitBasicAuthUsername(config.GitProviderGitLab); got != "oauth2" {
		t.Fatalf("gitlab username = %q, want oauth2", got)
	}
}
