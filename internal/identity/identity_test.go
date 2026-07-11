package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/config/configtest"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestRefreshUsesGitCredentialsAndUpdatesCache(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "live-home", ID: "1", DisplayName: "Live Home"},
	}}

	updated, results, changed, err := Refresh(context.Background(), cfg, "home", resolver)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if got := updated.Profiles["home"].Git.IdentityCache; got != "live-home" {
		t.Fatalf("git identity cache = %q, want live-home", got)
	}
	if len(results) != 1 || results[0].CredentialSource != SourceGit || !results[0].IdentityCacheUpdated {
		t.Fatalf("results = %#v, want one git update", results)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].Credential.Name != "codereview/home" {
		t.Fatalf("resolver calls = %#v, want git ref", resolver.calls)
	}
}

func TestRefreshUsesReviewerCredentialsWhenPresent(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/work-reviewer": {Login: "bot", ID: "2", DisplayName: "Bot"},
	}}

	updated, results, changed, err := Refresh(context.Background(), cfg, "work", resolver)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	work := updated.Profiles["work"]
	if got := work.Git.IdentityCache; got != "work-user-cache" {
		t.Fatalf("git identity cache = %q, want unchanged", got)
	}
	if got := work.ReviewerCredentials.IdentityCache; got != "bot" {
		t.Fatalf("reviewer identity cache = %q, want bot", got)
	}
	if len(results) != 1 || results[0].CredentialSource != SourceReviewer || results[0].PreviousIdentityCache != "old-bot" {
		t.Fatalf("results = %#v, want reviewer result with old cache", results)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].Host != "github.com" || resolver.calls[0].Credential.Name != "codereview/work-reviewer" {
		t.Fatalf("resolver calls = %#v, want reviewer ref on profile host", resolver.calls)
	}
}

func TestRefreshAllSortedAndAtomicOnFailure(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{
		identities: map[string]gitprovider.Identity{
			"codereview/home": {Login: "new-home"},
			"codereview/work": {Login: "new-work-user"},
		},
		errs: map[string]error{
			"codereview/work-reviewer": errors.New("lookup failed"),
		},
	}

	updated, results, changed, err := RefreshAll(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("RefreshAll error = nil, want failure")
	}
	if changed || results != nil {
		t.Fatalf("changed=%v results=%#v, want no partial success", changed, results)
	}
	if got := updated.Profiles["home"].Git.IdentityCache; got != "old-home" {
		t.Fatalf("returned home cache = %q, want original", got)
	}
	if len(resolver.calls) != 3 ||
		resolver.calls[0].Credential.Name != "codereview/home" ||
		resolver.calls[1].Credential.Name != "codereview/work" ||
		resolver.calls[2].Credential.Name != "codereview/work-reviewer" {
		t.Fatalf("resolver calls = %#v, want sorted home then work git/reviewer", resolver.calls)
	}
}

func TestRefreshAllReviewerCredentialsRollbackOnFailure(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"reviewer": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/reviewer-git"},
					IdentityCache: "git-cache",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/reviewer"},
					IdentityCache: "old-bot",
				},
			},
			"z-failing": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/failing"},
					IdentityCache: "old-failing",
				},
			},
		},
	}
	resolver := &fakeResolver{
		identities: map[string]gitprovider.Identity{
			"codereview/reviewer-git": {Login: "new-git-user"},
			"codereview/reviewer":     {Login: "new-bot"},
		},
		errs: map[string]error{
			"codereview/failing": errors.New("lookup failed"),
		},
	}

	updated, results, changed, err := RefreshAll(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("RefreshAll error = nil, want failure")
	}
	if changed || results != nil {
		t.Fatalf("changed=%v results=%#v, want no partial success", changed, results)
	}
	if got := updated.Profiles["reviewer"].ReviewerCredentials.IdentityCache; got != "old-bot" {
		t.Fatalf("returned reviewer cache = %q, want original", got)
	}
	if got := updated.Profiles["reviewer"].Git.IdentityCache; got != "git-cache" {
		t.Fatalf("returned git cache = %q, want original", got)
	}
	if got := cfg.Profiles["reviewer"].ReviewerCredentials.IdentityCache; got != "old-bot" {
		t.Fatalf("input reviewer cache = %q, want original", got)
	}
	if len(resolver.calls) != 3 ||
		resolver.calls[0].Credential.Name != "codereview/reviewer-git" ||
		resolver.calls[1].Credential.Name != "codereview/reviewer" ||
		resolver.calls[2].Credential.Name != "codereview/failing" {
		t.Fatalf("resolver calls = %#v, want reviewer git/reviewer then failing", resolver.calls)
	}
}

func TestRefreshAllRollsBackSameProfileReviewerFailure(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{
		identities: map[string]gitprovider.Identity{
			"codereview/home": {Login: "old-home"},
			"codereview/work": {Login: "new-work-user"},
		},
		errs: map[string]error{
			"codereview/work-reviewer": errors.New("lookup failed"),
		},
	}

	updated, results, changed, err := RefreshAll(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("RefreshAll error = nil, want reviewer lookup failure")
	}
	if changed || results != nil {
		t.Fatalf("changed=%v results=%#v, want no partial success", changed, results)
	}
	work := updated.Profiles["work"]
	if got := work.Git.IdentityCache; got != "work-user-cache" {
		t.Fatalf("returned work git cache = %q, want original", got)
	}
	if got := work.ReviewerCredentials.IdentityCache; got != "old-bot" {
		t.Fatalf("returned reviewer cache = %q, want original", got)
	}
	if got := cfg.Profiles["work"].Git.IdentityCache; got != "work-user-cache" {
		t.Fatalf("input work git cache = %q, want original", got)
	}
}

func TestRefreshEmptyLoginDoesNotClearCache(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: " "},
	}}

	updated, _, changed, err := Refresh(context.Background(), cfg, "home", resolver)
	if err == nil {
		t.Fatal("Refresh error = nil, want empty login error")
	}
	if changed {
		t.Fatal("changed = true, want false")
	}
	if got := updated.Profiles["home"].Git.IdentityCache; got != "old-home" {
		t.Fatalf("git identity cache = %q, want preserved", got)
	}
}

func TestRefreshUnchangedCacheReportsNoChange(t *testing.T) {
	cfg := testConfig()
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "old-home"},
	}}

	updated, results, changed, err := Refresh(context.Background(), cfg, "home", resolver)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want false")
	}
	if got := updated.Profiles["home"].Git.IdentityCache; got != "old-home" {
		t.Fatalf("git identity cache = %q, want unchanged", got)
	}
	if len(results) != 1 || results[0].IdentityCacheUpdated {
		t.Fatalf("results = %#v, want no cache update", results)
	}
}

type fakeResolver struct {
	identities map[string]gitprovider.Identity
	errs       map[string]error
	calls      []config.GitConfig
}

func (f *fakeResolver) ResolveIdentity(_ context.Context, _ string, git config.GitConfig) (gitprovider.Identity, error) {
	f.calls = append(f.calls, git)
	if err := f.errs[git.Credential.Name]; err != nil {
		return gitprovider.Identity{}, err
	}
	return f.identities[git.Credential.Name], nil
}

func testConfig() config.File {
	return configtest.File(
		configtest.WithoutSecrets(),
		configtest.WithoutRepositoryProfiles(),
		configtest.HomeProfile(config.Profile{
			Git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModePAT,
				Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/home"},
				IdentityCache: "old-home",
			},
			LLM: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
			ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
		}),
		configtest.Profile("work", config.Profile{
			Git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModePAT,
				Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/work"},
				IdentityCache: "work-user-cache",
			},
			ReviewerCredentials: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModePAT,
				Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/work-reviewer"},
				IdentityCache: "old-bot",
			},
			LLM: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
			ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
		}),
	)
}
