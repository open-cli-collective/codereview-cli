package configedit_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

func TestParseRepositoryRouteSpecNormalizesDomainInputs(t *testing.T) {
	spec, err := configedit.ParseRepositoryRouteSpec("https://github.com/", " rianjs ", []string{" baz ", "bar", "bar"})
	if err != nil {
		t.Fatalf("ParseRepositoryRouteSpec: %v", err)
	}
	want := configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"bar", "baz"},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec = %#v, want %#v", spec, want)
	}
	if got := configedit.FormatRepositoryRouteSpec(spec); got != "github.com/rianjs [bar, baz]" {
		t.Fatalf("FormatRepositoryRouteSpec = %q, want formatted route", got)
	}
}

func TestParseRepositoryRouteSpecRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		host string
		ns   string
		repo []string
		want error
	}{
		{name: "host", host: " ", ns: "rianjs", want: configedit.ErrRouteHostRequired},
		{name: "namespace", host: "github.com", ns: " ", want: configedit.ErrRouteNamespaceRequired},
		{name: "repo", host: "github.com", ns: "rianjs", repo: []string{" "}, want: configedit.ErrRouteRepoRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := configedit.ParseRepositoryRouteSpec(tt.host, tt.ns, tt.repo)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ParseRepositoryRouteSpec error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRepositoryRouteHelpersSetMoveUnsetAndCanonicalize(t *testing.T) {
	routes := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "https://github.com/",
				Namespace: "rianjs",
				Repos:     []string{"baz", "bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}

	moved := configedit.SetRepositoryRoutes(routes, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"baz"},
	})
	wantMoved := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"baz"},
			},
		},
	}
	if !reflect.DeepEqual(moved, wantMoved) {
		t.Fatalf("SetRepositoryRoutes = %#v, want %#v", moved, wantMoved)
	}

	pruned, changed := configedit.UnsetRepositoryRoutes(moved, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"baz"},
	})
	if !changed {
		t.Fatal("UnsetRepositoryRoutes changed = false, want true")
	}
	wantPruned := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	if !reflect.DeepEqual(pruned, wantPruned) {
		t.Fatalf("UnsetRepositoryRoutes = %#v, want %#v", pruned, wantPruned)
	}

	again, changed := configedit.UnsetRepositoryRoutes(pruned, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"missing"},
	})
	if changed {
		t.Fatal("UnsetRepositoryRoutes changed = true, want false")
	}
	if !reflect.DeepEqual(again, pruned) {
		t.Fatalf("idempotent unset = %#v, want %#v", again, pruned)
	}
}

func TestAgentSourceHelpersNormalizePreserveAndReset(t *testing.T) {
	existing := []string{" ./agents/../agents/team/ "}
	got, changed, err := configedit.AddAgentSource(existing, "./agents/../agents/team")
	if err != nil {
		t.Fatalf("AddAgentSource duplicate: %v", err)
	}
	if changed {
		t.Fatal("AddAgentSource duplicate changed = true, want false")
	}
	if !reflect.DeepEqual(got, existing) {
		t.Fatalf("AddAgentSource duplicate = %#v, want preserved existing source", got)
	}

	got, changed, err = configedit.AddAgentSource(existing, " ./shared/../team/agents/ ")
	if err != nil {
		t.Fatalf("AddAgentSource new: %v", err)
	}
	if !changed {
		t.Fatal("AddAgentSource new changed = false, want true")
	}
	want := []string{" ./agents/../agents/team/ ", "team/agents"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AddAgentSource new = %#v, want %#v", got, want)
	}

	got, changed, err = configedit.RemoveAgentSource(got, " ./team/agents ")
	if err != nil {
		t.Fatalf("RemoveAgentSource: %v", err)
	}
	if !changed {
		t.Fatal("RemoveAgentSource changed = false, want true")
	}
	if !reflect.DeepEqual(got, existing) {
		t.Fatalf("RemoveAgentSource = %#v, want %#v", got, existing)
	}

	got, changed = configedit.ResetAgentSources(got)
	if !changed || got != nil {
		t.Fatalf("ResetAgentSources = (%#v,%t), want nil,true", got, changed)
	}
}

func TestAgentSourceHelpersRejectBlankPath(t *testing.T) {
	if _, _, err := configedit.AddAgentSource(nil, " "); !errors.Is(err, configedit.ErrAgentSourcePathRequired) {
		t.Fatalf("AddAgentSource error = %v, want ErrAgentSourcePathRequired", err)
	}
	if _, _, err := configedit.RemoveAgentSource([]string{"agents"}, " "); !errors.Is(err, configedit.ErrAgentSourcePathRequired) {
		t.Fatalf("RemoveAgentSource error = %v, want ErrAgentSourcePathRequired", err)
	}
}

func TestRenameProfileUpdatesReferencesAndPreservesProfileValues(t *testing.T) {
	cfg := testConfig()

	got, changed, err := configedit.RenameProfile(cfg, "work", "office")
	if err != nil {
		t.Fatalf("RenameProfile: %v", err)
	}
	if !changed {
		t.Fatal("RenameProfile changed = false, want true")
	}
	if _, ok := got.Profiles["work"]; ok {
		t.Fatal("old profile still exists after rename")
	}
	office := got.Profiles["office"]
	if got.DefaultProfile != "office" {
		t.Fatalf("DefaultProfile = %q, want office", got.DefaultProfile)
	}
	if office.Git.CredentialRef != "codereview/custom-work" || office.Git.IdentityCache != "work-user" {
		t.Fatalf("git fields = %#v, want preserved credential ref and identity cache", office.Git)
	}
	if office.ReviewerCredentials == nil ||
		office.ReviewerCredentials.CredentialRef != "codereview/custom-reviewer" ||
		office.ReviewerCredentials.IdentityCache != "review-bot" {
		t.Fatalf("reviewer credentials = %#v, want preserved refs/cache", office.ReviewerCredentials)
	}
	if office.LLM.CredentialRef != "codereview/custom-llm" {
		t.Fatalf("llm credential ref = %q, want preserved custom ref", office.LLM.CredentialRef)
	}
	wantRoutes := []config.RepositoryProfile{
		{
			Profile: "office",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	if !reflect.DeepEqual(got.RepositoryProfiles, wantRoutes) {
		t.Fatalf("RepositoryProfiles = %#v, want %#v", got.RepositoryProfiles, wantRoutes)
	}
	if cfg.DefaultProfile != "work" {
		t.Fatalf("original config default mutated to %q", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatal("original config profile map was mutated")
	}
}

func TestProfileHelpersRejectInvalidDefaultAndRename(t *testing.T) {
	cfg := testConfig()

	if _, _, err := configedit.SetDefaultProfile(cfg, "missing"); !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("SetDefaultProfile missing error = %v, want ErrProfileNotFound", err)
	}
	if _, _, err := configedit.SetDefaultProfile(cfg, " "); !errors.Is(err, configedit.ErrProfileNameRequired) {
		t.Fatalf("SetDefaultProfile blank error = %v, want ErrProfileNameRequired", err)
	}
	if _, _, err := configedit.RenameProfile(cfg, "missing", "office"); !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("RenameProfile missing error = %v, want ErrProfileNotFound", err)
	}
	if _, _, err := configedit.RenameProfile(cfg, "work", "home"); !errors.Is(err, configedit.ErrProfileExists) {
		t.Fatalf("RenameProfile conflict error = %v, want ErrProfileExists", err)
	}
}

func TestFirstProfileAndPruneHelpers(t *testing.T) {
	cfg := testConfig()
	if got := configedit.FirstProfileName(cfg.Profiles); got != "home" {
		t.Fatalf("FirstProfileName = %q, want home", got)
	}
	pruned := configedit.PruneRepositoryProfileRoutes(cfg.RepositoryProfiles, "work")
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	if !reflect.DeepEqual(pruned, want) {
		t.Fatalf("PruneRepositoryProfileRoutes = %#v, want %#v", pruned, want)
	}
}

func TestCredentialRefAndOptionalResetHelpers(t *testing.T) {
	profile := testConfig().Profiles["work"]
	updated, changed := configedit.SetGitCredentialRef(profile, " codereview/new-work ")
	if !changed || updated.Git.CredentialRef != "codereview/new-work" {
		t.Fatalf("SetGitCredentialRef = (%#v,%t), want trimmed update", updated.Git, changed)
	}

	updated, changed, err := configedit.SetReviewerCredentialRef(profile, " codereview/new-reviewer ")
	if err != nil {
		t.Fatalf("SetReviewerCredentialRef: %v", err)
	}
	if !changed || updated.ReviewerCredentials.CredentialRef != "codereview/new-reviewer" {
		t.Fatalf("SetReviewerCredentialRef = (%#v,%t), want trimmed update", updated.ReviewerCredentials, changed)
	}
	if profile.ReviewerCredentials.CredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("original reviewer credentials mutated to %q", profile.ReviewerCredentials.CredentialRef)
	}

	cleared, changed := configedit.ClearReviewerCredentials(updated)
	if !changed || cleared.ReviewerCredentials != nil {
		t.Fatalf("ClearReviewerCredentials = (%#v,%t), want nil,true", cleared.ReviewerCredentials, changed)
	}
	if _, _, err := configedit.SetReviewerCredentialRef(cleared, "codereview/reviewer"); !errors.Is(err, configedit.ErrReviewerCredentialsNotConfigured) {
		t.Fatalf("SetReviewerCredentialRef without section error = %v, want ErrReviewerCredentialsNotConfigured", err)
	}

	updated, changed = configedit.SetLLMCredentialRef(profile, " codereview/new-llm ")
	if !changed || updated.LLM.CredentialRef != "codereview/new-llm" {
		t.Fatalf("SetLLMCredentialRef = (%#v,%t), want trimmed update", updated.LLM, changed)
	}
	cleared, changed = configedit.ClearLLMCredentialRef(updated)
	if !changed || cleared.LLM.CredentialRef != "" {
		t.Fatalf("ClearLLMCredentialRef = (%#v,%t), want empty,true", cleared.LLM, changed)
	}

	profile.LLM.ModelMap = config.ModelMap{"small": "model-a"}
	cleared, changed = configedit.ResetModelMap(profile)
	if !changed || cleared.LLM.ModelMap != nil {
		t.Fatalf("ResetModelMap = (%#v,%t), want nil,true", cleared.LLM.ModelMap, changed)
	}
}

func testConfig() config.File {
	return config.File{
		DefaultProfile: "work",
		RepositoryProfiles: []config.RepositoryProfile{
			{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "rianjs",
				},
			},
			{
				Profile: "home",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "open-cli-collective",
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/custom-work",
					IdentityCache: "work-user",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/custom-reviewer",
					IdentityCache: "review-bot",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					CredentialRef: "codereview/custom-llm",
				},
				AgentSources: []string{"~/agents"},
			},
		},
	}
}
