package configedit_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

func TestNormalizeRepositoryRouteSpecNormalizesDomainInputs(t *testing.T) {
	spec, err := configedit.NormalizeRepositoryRouteSpec(configedit.RepositoryRouteSpec{
		Host:      "https://github.com/",
		Namespace: " rianjs ",
		Repos:     []string{" baz ", "bar", "bar"},
	})
	if err != nil {
		t.Fatalf("NormalizeRepositoryRouteSpec: %v", err)
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

func TestNormalizeRepositoryRouteSpecRejectsMissingFields(t *testing.T) {
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
			_, err := configedit.NormalizeRepositoryRouteSpec(configedit.RepositoryRouteSpec{
				Host:      tt.host,
				Namespace: tt.ns,
				Repos:     tt.repo,
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("NormalizeRepositoryRouteSpec error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRepositoryRouteHelpersSetShareUnsetAndCanonicalize(t *testing.T) {
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

	moved, err := configedit.SetRepositoryRoutes(routes, "work", configedit.RepositoryRouteSpec{
		Host:      "https://github.com/",
		Namespace: " rianjs ",
		Repos:     []string{" baz "},
	})
	if err != nil {
		t.Fatalf("SetRepositoryRoutes: %v", err)
	}
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
				Repos:     []string{"bar", "baz"},
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

	pruned, changed, err := configedit.UnsetRepositoryRoutes(moved, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"baz"},
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutes: %v", err)
	}
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

	again, changed, err := configedit.UnsetRepositoryRoutes(pruned, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"missing"},
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutes idempotent: %v", err)
	}
	if changed {
		t.Fatal("UnsetRepositoryRoutes changed = true, want false")
	}
	if !reflect.DeepEqual(again, pruned) {
		t.Fatalf("idempotent unset = %#v, want %#v", again, pruned)
	}
}

func TestRepositoryRouteHelpersUnsetForProfile(t *testing.T) {
	routes := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
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

	got, changed, err := configedit.UnsetRepositoryRoutesForProfile(routes, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"baz"},
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutesForProfile: %v", err)
	}
	if !changed {
		t.Fatal("UnsetRepositoryRoutesForProfile changed = false, want true")
	}
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnsetRepositoryRoutesForProfile = %#v, want %#v", got, want)
	}

	again, changed, err := configedit.UnsetRepositoryRoutesForProfile(got, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{"baz"},
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutesForProfile idempotent: %v", err)
	}
	if changed {
		t.Fatal("UnsetRepositoryRoutesForProfile changed = true, want false")
	}
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("idempotent scoped unset = %#v, want %#v", again, got)
	}
}

func TestRepositoryRouteHelpersShareNamespaceRoutes(t *testing.T) {
	routes := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	}

	shared, err := configedit.SetRepositoryRoutes(routes, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
	})
	if err != nil {
		t.Fatalf("SetRepositoryRoutes namespace: %v", err)
	}
	wantShared := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	}
	if !reflect.DeepEqual(shared, wantShared) {
		t.Fatalf("SetRepositoryRoutes namespace = %#v, want %#v", shared, wantShared)
	}

	homeOnly, changed, err := configedit.UnsetRepositoryRoutesForProfile(shared, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutesForProfile namespace: %v", err)
	}
	if !changed {
		t.Fatal("UnsetRepositoryRoutesForProfile namespace changed = false, want true")
	}
	if !reflect.DeepEqual(homeOnly, routes) {
		t.Fatalf("UnsetRepositoryRoutesForProfile namespace = %#v, want %#v", homeOnly, routes)
	}
}

func TestRepositoryRouteHelpersScopedUnsetPrunesLastOwnerAndPreservesAbsentSiblings(t *testing.T) {
	routes := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	}

	unchanged, changed, err := configedit.UnsetRepositoryRoutesForProfile(routes, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutesForProfile absent owner: %v", err)
	}
	if changed {
		t.Fatal("UnsetRepositoryRoutesForProfile absent owner changed = true, want false")
	}
	if !reflect.DeepEqual(unchanged, routes) {
		t.Fatalf("absent owner unset = %#v, want %#v", unchanged, routes)
	}

	pruned, changed, err := configedit.UnsetRepositoryRoutesForProfile(routes, "home", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
	})
	if err != nil {
		t.Fatalf("UnsetRepositoryRoutesForProfile last owner: %v", err)
	}
	if !changed {
		t.Fatal("UnsetRepositoryRoutesForProfile last owner changed = false, want true")
	}
	if len(pruned) != 0 {
		t.Fatalf("last owner unset = %#v, want empty routes", pruned)
	}
}

func TestRepositoryRouteMutatorsValidateDirectSpecs(t *testing.T) {
	if _, err := configedit.SetRepositoryRoutes(nil, "work", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
		Repos:     []string{" "},
	}); !errors.Is(err, configedit.ErrRouteRepoRequired) {
		t.Fatalf("SetRepositoryRoutes error = %v, want ErrRouteRepoRequired", err)
	}
	if _, _, err := configedit.UnsetRepositoryRoutes(nil, configedit.RepositoryRouteSpec{
		Host:      " ",
		Namespace: "rianjs",
	}); !errors.Is(err, configedit.ErrRouteHostRequired) {
		t.Fatalf("UnsetRepositoryRoutes error = %v, want ErrRouteHostRequired", err)
	}
	if _, _, err := configedit.UnsetRepositoryRoutesForProfile(nil, " ", configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "rianjs",
	}); !errors.Is(err, configedit.ErrProfileNameRequired) {
		t.Fatalf("UnsetRepositoryRoutesForProfile error = %v, want ErrProfileNameRequired", err)
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

}

func TestAgentSourceHelpersRejectBlankPath(t *testing.T) {
	if _, _, err := configedit.AddAgentSource(nil, " "); !errors.Is(err, configedit.ErrAgentSourcePathRequired) {
		t.Fatalf("AddAgentSource error = %v, want ErrAgentSourcePathRequired", err)
	}
	if _, _, err := configedit.RemoveAgentSource([]string{"agents"}, " "); !errors.Is(err, configedit.ErrAgentSourcePathRequired) {
		t.Fatalf("RemoveAgentSource error = %v, want ErrAgentSourcePathRequired", err)
	}
}

func TestRenameProfileUpdatesRoutesAndPreservesProfileValues(t *testing.T) {
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
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatal("original config profile map was mutated")
	}
}

func TestProfileHelpersRejectInvalidRename(t *testing.T) {
	cfg := testConfig()

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

func TestSecretsProfileHelpers(t *testing.T) {
	t.Run("set creates, updates, clears label, and preserves omitted fields", func(t *testing.T) {
		cfg := testConfig()
		backend := config.SecretsProfileBackend{Kind: config.SecretsBackendKind("keychain")}
		label := " Personal Keychain "
		updated, changed, created, err := configedit.SetSecretsProfile(cfg, " personal ", configedit.SecretsProfilePatch{
			Backend: &backend,
			Label:   &label,
		})
		if err != nil {
			t.Fatalf("SetSecretsProfile create: %v", err)
		}
		if !changed || !created {
			t.Fatalf("SetSecretsProfile create = changed:%t created:%t, want true,true", changed, created)
		}
		got := updated.Secrets.Profiles["personal"]
		if got.Label != "Personal Keychain" || got.Backend.Kind != backend.Kind {
			t.Fatalf("created profile = %#v, want trimmed label + backend", got)
		}
		if len(cfg.Secrets.Profiles) != 0 {
			t.Fatalf("original cfg mutated on create: %#v", cfg.Secrets.Profiles)
		}

		nextBackend := config.SecretsProfileBackend{Kind: config.SecretsBackendKind("file")}
		updated2, changed, created, err := configedit.SetSecretsProfile(updated, "personal", configedit.SecretsProfilePatch{
			Backend: &nextBackend,
		})
		if err != nil {
			t.Fatalf("SetSecretsProfile backend update: %v", err)
		}
		if !changed || created {
			t.Fatalf("SetSecretsProfile backend update = changed:%t created:%t, want true,false", changed, created)
		}
		got = updated2.Secrets.Profiles["personal"]
		if got.Label != "Personal Keychain" || got.Backend.Kind != nextBackend.Kind {
			t.Fatalf("updated profile = %#v, want preserved label + new backend", got)
		}

		updated3, changed, created, err := configedit.SetSecretsProfile(updated2, "personal", configedit.SecretsProfilePatch{
			ClearLabel: true,
		})
		if err != nil {
			t.Fatalf("SetSecretsProfile clear label: %v", err)
		}
		if !changed || created {
			t.Fatalf("SetSecretsProfile clear label = changed:%t created:%t, want true,false", changed, created)
		}
		if got := updated3.Secrets.Profiles["personal"]; got.Label != "" || got.Backend.Kind != nextBackend.Kind {
			t.Fatalf("cleared label profile = %#v, want empty label + preserved backend", got)
		}
	})

	t.Run("set replaces 1password backend payloads", func(t *testing.T) {
		cfg := testConfig()
		backend := config.SecretsProfileBackend{
			Kind: config.SecretsBackendKind("op-connect"),
			OnePassword: &config.SecretsProfileOnePasswordConfig{
				VaultID:         "vault-123",
				ConnectHost:     "https://connect.example",
				ConnectTokenEnv: "CUSTOM_CONNECT_TOKEN",
			},
		}
		updated, changed, created, err := configedit.SetSecretsProfile(cfg, "work-op", configedit.SecretsProfilePatch{Backend: &backend})
		if err != nil {
			t.Fatalf("SetSecretsProfile 1password create: %v", err)
		}
		if !changed || !created {
			t.Fatalf("SetSecretsProfile 1password create = changed:%t created:%t, want true,true", changed, created)
		}
		got := updated.Secrets.Profiles["work-op"]
		if got.Backend.OnePassword == nil || got.Backend.OnePassword.ConnectHost != "https://connect.example" {
			t.Fatalf("created 1password profile = %#v, want connect host payload", got)
		}

		replacement := config.SecretsProfileBackend{
			Kind: config.SecretsBackendKind("op"),
			OnePassword: &config.SecretsProfileOnePasswordConfig{
				VaultID:         "vault-123",
				ServiceTokenEnv: "CUSTOM_SERVICE_TOKEN",
			},
		}
		updated, changed, created, err = configedit.SetSecretsProfile(updated, "work-op", configedit.SecretsProfilePatch{Backend: &replacement})
		if err != nil {
			t.Fatalf("SetSecretsProfile 1password replace: %v", err)
		}
		if !changed || created {
			t.Fatalf("SetSecretsProfile 1password replace = changed:%t created:%t, want true,false", changed, created)
		}
		got = updated.Secrets.Profiles["work-op"]
		if got.Backend.OnePassword == nil || got.Backend.OnePassword.ServiceTokenEnv != "CUSTOM_SERVICE_TOKEN" {
			t.Fatalf("replaced 1password profile = %#v, want service token env payload", got)
		}

		downgraded := config.SecretsProfileBackend{Kind: config.SecretsBackendKind("file")}
		updated, changed, created, err = configedit.SetSecretsProfile(updated, "work-op", configedit.SecretsProfilePatch{Backend: &downgraded})
		if err != nil {
			t.Fatalf("SetSecretsProfile 1password downgrade: %v", err)
		}
		if !changed || created {
			t.Fatalf("SetSecretsProfile 1password downgrade = changed:%t created:%t, want true,false", changed, created)
		}
		got = updated.Secrets.Profiles["work-op"]
		if got.Backend.Kind != "file" || got.Backend.OnePassword != nil {
			t.Fatalf("downgraded profile = %#v, want file backend without 1password payload", got)
		}
	})

	t.Run("set allows stores before review profiles exist", func(t *testing.T) {
		backend := config.SecretsProfileBackend{
			Kind: config.SecretsBackendKind("op-desktop"),
			OnePassword: &config.SecretsProfileOnePasswordConfig{
				AccountURL: "my.1password.com",
				VaultID:    "vault-private",
				VaultName:  "Private",
			},
		}
		label := "1Password-Personal"
		updated, changed, created, err := configedit.SetSecretsProfile(config.File{}, "1password-personal", configedit.SecretsProfilePatch{
			Backend: &backend,
			Label:   &label,
		})
		if err != nil {
			t.Fatalf("SetSecretsProfile store-only create: %v", err)
		}
		if !changed || !created {
			t.Fatalf("SetSecretsProfile store-only create = changed:%t created:%t, want true,true", changed, created)
		}
		if len(updated.Profiles) != 0 {
			t.Fatalf("updated config unexpectedly added review profile data: %#v", updated)
		}
		got := updated.Secrets.Profiles["1password-personal"]
		if got.Label != "1Password-Personal" || got.Backend.OnePassword == nil || got.Backend.OnePassword.VaultName != "Private" {
			t.Fatalf("created store = %#v, want named 1Password store", got)
		}
	})

	t.Run("set validates edge cases", func(t *testing.T) {
		cfg := testConfig()
		backend := config.SecretsProfileBackend{Kind: config.SecretsBackendKind("keychain")}
		label := "name"
		spaceLabel := "   "
		tests := []struct {
			name  string
			id    string
			patch configedit.SecretsProfilePatch
			want  error
		}{
			{name: "missing id", id: " ", patch: configedit.SecretsProfilePatch{Backend: &backend}, want: configedit.ErrSecretsProfileIDRequired},
			{name: "reserved id", id: config.LocalOSCredentialStoreID, patch: configedit.SecretsProfilePatch{Backend: &backend}, want: configedit.ErrSecretsProfileReserved},
			{name: "create missing backend", id: "personal", patch: configedit.SecretsProfilePatch{}, want: configedit.ErrSecretsProfileBackendRequired},
			{name: "conflicting label flags", id: "personal", patch: configedit.SecretsProfilePatch{Backend: &backend, Label: &label, ClearLabel: true}, want: configedit.ErrSecretsProfileLabelConflict},
			{name: "blank label", id: "personal", patch: configedit.SecretsProfilePatch{Backend: &backend, Label: &spaceLabel}, want: configedit.ErrSecretsProfileLabelRequired},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, _, _, err := configedit.SetSecretsProfile(cfg, tt.id, tt.patch)
				if !errors.Is(err, tt.want) {
					t.Fatalf("SetSecretsProfile error = %v, want %v", err, tt.want)
				}
			})
		}

		cfg.Secrets.Profiles = map[string]config.SecretsProfile{
			"personal": {Backend: config.SecretsProfileBackend{Kind: backend.Kind}},
		}
		_, _, _, err := configedit.SetSecretsProfile(cfg, "personal", configedit.SecretsProfilePatch{})
		if !errors.Is(err, configedit.ErrSecretsProfileMutationRequired) {
			t.Fatalf("SetSecretsProfile update without flags error = %v, want ErrSecretsProfileMutationRequired", err)
		}

		cfg = testConfig()
		cfg.Secrets.Profiles = map[string]config.SecretsProfile{
			"personal": {Backend: config.SecretsProfileBackend{Kind: backend.Kind}},
		}
		invalidBackend := config.SecretsProfileBackend{Kind: config.SecretsBackendKind("bogus")}
		before := cfg.Secrets.Profiles["personal"]
		_, _, _, err = configedit.SetSecretsProfile(cfg, "personal", configedit.SecretsProfilePatch{Backend: &invalidBackend})
		if !errors.Is(err, config.ErrInvalid) {
			t.Fatalf("SetSecretsProfile invalid backend error = %v, want ErrInvalid", err)
		}
		if after := cfg.Secrets.Profiles["personal"]; !reflect.DeepEqual(after, before) {
			t.Fatalf("original cfg mutated on failed update: got %#v want %#v", after, before)
		}
	})

	t.Run("remove is idempotent but blocks reserved id", func(t *testing.T) {
		cfg := testConfig()
		cfg.Secrets.Profiles = map[string]config.SecretsProfile{
			"work": {Backend: config.SecretsProfileBackend{Kind: "file"}},
		}
		_, _, err := configedit.RemoveSecretsProfile(cfg, config.LocalOSCredentialStoreID)
		if !errors.Is(err, configedit.ErrSecretsProfileReserved) {
			t.Fatalf("RemoveSecretsProfile reserved error = %v, want ErrSecretsProfileReserved", err)
		}

		updated, changed, err := configedit.RemoveSecretsProfile(cfg, "work")
		if err != nil {
			t.Fatalf("RemoveSecretsProfile existing: %v", err)
		}
		if !changed || len(updated.Secrets.Profiles) != 0 {
			t.Fatalf("RemoveSecretsProfile existing = changed:%t profiles:%#v, want true/empty", changed, updated.Secrets.Profiles)
		}
		if len(cfg.Secrets.Profiles) != 1 {
			t.Fatalf("original cfg mutated on remove: %#v", cfg.Secrets.Profiles)
		}
		updated, changed, err = configedit.RemoveSecretsProfile(updated, "work")
		if err != nil {
			t.Fatalf("RemoveSecretsProfile idempotent: %v", err)
		}
		if changed {
			t.Fatalf("RemoveSecretsProfile idempotent changed = %t, want false", changed)
		}
	})
}

func testConfig() config.File {
	return config.File{
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
