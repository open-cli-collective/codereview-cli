package credentials

import (
	"reflect"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

func TestPlan(t *testing.T) {
	t.Run("new git pat defaults to defer", func(t *testing.T) {
		desired := planningBasicProfile("work")
		entries, err := Plan(nil, desired, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("entries len = %d, want 1", len(entries))
		}
		entry := entries[0]
		if entry.Ref.Ref != "codereview/work" || entry.State != PlanStateDefer {
			t.Fatalf("entry = %#v, want git defer codereview/work", entry)
		}
		if !reflect.DeepEqual(entry.KeySpecs, []KeySpec{{Key: GitTokenKey, Required: true}}) {
			t.Fatalf("key specs = %#v, want git_token required", entry.KeySpecs)
		}
	})

	t.Run("reviewer github app default ref includes bundle keys", func(t *testing.T) {
		desired := planningBasicProfile("work")
		desired.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:   config.GitAuthModeGitHubApp,
			GitHubApp:  &config.GitHubAppConfig{AppID: "12345"},
			Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
		}
		entries, err := Plan(nil, desired, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("entries len = %d, want 2", len(entries))
		}
		entry := entries[1]
		if entry.Ref.Purpose != "reviewer_credentials" || entry.Ref.Ref != "codereview/work-reviewer" || entry.State != PlanStateDefer {
			t.Fatalf("reviewer entry = %#v, want deferred reviewer app ref", entry)
		}
		want := []KeySpec{{Key: GitHubAppPrivateKeyKey, Required: true}}
		if !reflect.DeepEqual(entry.KeySpecs, want) {
			t.Fatalf("key specs = %#v, want %#v", entry.KeySpecs, want)
		}
	})

	t.Run("llm providers use provider-specific api keys", func(t *testing.T) {
		for _, tt := range []struct {
			name     string
			provider config.LLMProvider
			adapter  config.LLMAdapter
			key      string
		}{
			{name: "anthropic", provider: config.LLMProviderAnthropic, adapter: config.LLMAdapterAnthropicAPI, key: AnthropicAPIKeyKey},
			{name: "openai", provider: config.LLMProviderOpenAI, adapter: config.LLMAdapterOpenAIAPI, key: OpenAIAPIKeyKey},
		} {
			t.Run(tt.name, func(t *testing.T) {
				desired := planningBasicProfile("work")
				desired.LLM = config.LLMConfig{
					Provider:   tt.provider,
					Auth:       config.LLMAuthAPIKey,
					Adapter:    tt.adapter,
					Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-llm"},
				}
				entries, err := Plan(nil, desired, nil)
				if err != nil {
					t.Fatalf("Plan: %v", err)
				}
				if len(entries) != 2 {
					t.Fatalf("entries len = %d, want 2", len(entries))
				}
				entry := entries[1]
				if entry.Ref.Purpose != "llm" || entry.State != PlanStateDefer {
					t.Fatalf("llm entry = %#v, want deferred llm credential", entry)
				}
				want := []KeySpec{{Key: tt.key, Required: true}}
				if !reflect.DeepEqual(entry.KeySpecs, want) {
					t.Fatalf("key specs = %#v, want %#v", entry.KeySpecs, want)
				}
			})
		}
	})

	t.Run("preserve custom refs across rename interactions", func(t *testing.T) {
		cfg := config.File{Profiles: map[string]config.Profile{"work": {
			Git: config.GitConfig{
				Host:       "github.com",
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-work"},
			},
			ReviewerCredentials: &config.ReviewerCredentials{
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-reviewer"},
			},
			LLM: config.LLMConfig{
				Provider:   config.LLMProviderAnthropic,
				Auth:       config.LLMAuthAPIKey,
				Adapter:    config.LLMAdapterAnthropicAPI,
				Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-llm"},
			},
		}}}
		renamed, changed, err := configedit.RenameProfile(cfg, "work", "office")
		if err != nil {
			t.Fatalf("RenameProfile: %v", err)
		}
		if !changed {
			t.Fatal("RenameProfile changed = false, want true")
		}
		previous := cfg.Profiles["work"]
		desired := renamed.Profiles["office"]
		entries, err := Plan(&previous, desired, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, entry := range entries {
			if entry.State != PlanStateKeepExisting {
				t.Fatalf("entry = %#v, want keep_existing across rename", entry)
			}
		}
	})

	t.Run("overwrite custom ref without writes is tracked separately", func(t *testing.T) {
		previous := planningBasicProfile("work")
		previous.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-old"}
		desired := previous
		desired.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-new"}
		entries, err := Plan(&previous, desired, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if entries[0].State != PlanStateOverwriteRef {
			t.Fatalf("state = %s, want overwrite_ref", entries[0].State)
		}
	})

	t.Run("optional refs can clear", func(t *testing.T) {
		previous := planningAPIKeyProfile("work", config.LLMProviderAnthropic)
		previous.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:   config.GitAuthModePAT,
			Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
		}
		desired := planningBasicProfile("work")
		entries, err := Plan(&previous, desired, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		states := map[string]PlanState{}
		for _, entry := range entries {
			states[entry.Ref.Purpose] = entry.State
		}
		if states["git"] != PlanStateKeepExisting {
			t.Fatalf("git state = %s, want keep_existing", states["git"])
		}
		if states["reviewer_credentials"] != PlanStateClearRef {
			t.Fatalf("reviewer state = %s, want clear_ref", states["reviewer_credentials"])
		}
		if states["llm"] != PlanStateClearRef {
			t.Fatalf("llm state = %s, want clear_ref", states["llm"])
		}
	})

	t.Run("github app private key not staged defers credential setup", func(t *testing.T) {
		desired := planningBasicProfile("work")
		desired.Git.AuthMode = config.GitAuthModeGitHubApp
		desired.Git.GitHubApp = &config.GitHubAppConfig{AppID: "12345"}
		entries, err := Plan(nil, desired, map[string][]string{"codereview/work": {}})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		entry := entries[0]
		if entry.State != PlanStateDefer {
			t.Fatalf("state = %s, want defer", entry.State)
		}
		if len(entry.PlannedWriteKeys) != 0 {
			t.Fatalf("planned write keys = %#v, want none", entry.PlannedWriteKeys)
		}
		if len(entry.MissingRequiredKeys) != 0 {
			t.Fatalf("missing required = %#v, want none without staged writes", entry.MissingRequiredKeys)
		}
	})
}

func TestPlanClearsOptionalRefsInStableOrder(t *testing.T) {
	previous := planningAPIKeyProfile("work", config.LLMProviderAnthropic)
	previous.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
	}
	desired := planningBasicProfile("work")

	entries, err := Plan(&previous, desired, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var cleared []string
	for _, entry := range entries {
		if entry.State == PlanStateClearRef {
			cleared = append(cleared, entry.Ref.Purpose)
		}
	}
	if !reflect.DeepEqual(cleared, []string{"reviewer_credentials", "llm"}) {
		t.Fatalf("cleared purposes = %#v, want reviewer then llm", cleared)
	}
}

func TestGroupWritesByStoreSeparatesStores(t *testing.T) {
	personal := ResolvedSecretsStore{ID: "personal-memory", Backend: string(credstore.BackendMemory), Source: config.EffectiveSecretsStoreSourceConfigured}
	work := ResolvedSecretsStore{ID: "work-file", Backend: string(credstore.BackendFile), Source: config.EffectiveSecretsStoreSourceConfigured}
	writes := map[string]map[string]string{
		"codereview/home": {GitTokenKey: "home-token"},
		"codereview/work": {GitTokenKey: "work-token"},
	}

	groups, err := GroupWritesByStore([]PlanEntry{
		{Ref: config.CredentialRef{Ref: "codereview/home"}, SecretsStore: personal},
		{Ref: config.CredentialRef{Ref: "codereview/work"}, SecretsStore: work},
	}, writes, map[string]bool{"codereview/work": true})
	if err != nil {
		t.Fatalf("GroupWritesByStore: %v", err)
	}
	if len(groups) != 2 || groups[0].Resolved != personal || groups[1].Resolved != work {
		t.Fatalf("groups = %#v, want deterministic personal then work groups", groups)
	}
	if !reflect.DeepEqual(groups[0].Writes, map[string]map[string]string{"codereview/home": writes["codereview/home"]}) {
		t.Fatalf("personal writes = %#v", groups[0].Writes)
	}
	if !groups[1].OverwriteRefs["codereview/work"] {
		t.Fatalf("work overwrite refs = %#v, want work", groups[1].OverwriteRefs)
	}
}

func TestGroupWritesByStoreKeepsSameCredentialNameInDifferentStoresSeparate(t *testing.T) {
	ref := "codereview/shared"
	writes := map[string]map[string]string{ref: {GitTokenKey: "shared-token"}}
	groups, err := GroupWritesByStore([]PlanEntry{
		{Ref: config.CredentialRef{Ref: ref}, SecretsStore: ResolvedSecretsStore{ID: "personal", Backend: "memory", Source: config.EffectiveSecretsStoreSourceConfigured}},
		{Ref: config.CredentialRef{Ref: ref}, SecretsStore: ResolvedSecretsStore{ID: "work", Backend: "file", Source: config.EffectiveSecretsStoreSourceConfigured}},
	}, writes, nil)
	if err != nil {
		t.Fatalf("GroupWritesByStore: %v", err)
	}
	if len(groups) != 2 || groups[0].Resolved.ID != "personal" || groups[1].Resolved.ID != "work" {
		t.Fatalf("groups = %#v, want same ref grouped once per store", groups)
	}
	for _, group := range groups {
		if !reflect.DeepEqual(group.Writes[ref], writes[ref]) {
			t.Fatalf("group %q writes = %#v, want shared bundle characterization", group.Resolved.ID, group.Writes)
		}
	}
}

func TestGroupStaleReviewerCredentialCleanupsByStoreProtectsSharedRefAcrossProfiles(t *testing.T) {
	appProfile := planningBasicProfile("app")
	appProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModeGitHubApp,
		GitHubApp:  &config.GitHubAppConfig{AppID: "12345"},
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-reviewer"},
	}
	patProfile := planningBasicProfile("pat")
	patProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-reviewer"},
	}
	cfg := config.File{Profiles: map[string]config.Profile{"app": appProfile, "pat": patProfile}}
	resolved, err := ResolveSecretsStoreForProfile(cfg, appProfile)
	if err != nil {
		t.Fatalf("ResolveSecretsStoreForProfile: %v", err)
	}
	entry := PlanEntry{
		Ref:          config.CredentialRef{Purpose: "reviewer_credentials", Ref: "codereview/shared-reviewer", Mode: string(config.GitAuthModeGitHubApp)},
		PreviousRef:  &config.CredentialRef{Purpose: "reviewer_credentials", Ref: "codereview/shared-reviewer", Mode: string(config.GitAuthModePAT)},
		SecretsStore: resolved,
		KeySpecs:     []KeySpec{{Key: GitHubAppPrivateKeyKey, Required: true}},
		State:        PlanStateKeepExisting,
	}

	groups, err := GroupStaleReviewerCredentialCleanupsByStore(cfg, []PlanEntry{entry})
	if err != nil {
		t.Fatalf("GroupStaleReviewerCredentialCleanupsByStore: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Entries["codereview/shared-reviewer"]) != 2 {
		t.Fatalf("groups = %#v, want both active shared-ref modes", groups)
	}
	if got, want := StaleReviewerCredentialKeys(groups[0].Entries["codereview/shared-reviewer"]), []string{GitHubAppIDKey, GitHubAppInstallationIDKey}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stale keys = %#v, want %#v", got, want)
	}
}

func TestGroupStaleReviewerCredentialCleanupsByStoreSeparatesConfiguredStores(t *testing.T) {
	cfg := config.File{Secrets: config.SecretsConfig{Stores: map[string]config.SecretsStore{
		"personal": {Backend: config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)}},
		"work":     {Backend: config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)}},
	}}}
	entries := make([]PlanEntry, 0, 2)
	for _, storeID := range []string{"personal", "work"} {
		resolved, err := ResolveCredentialStore(cfg, storeID)
		if err != nil {
			t.Fatalf("ResolveCredentialStore(%s): %v", storeID, err)
		}
		entries = append(entries, PlanEntry{
			Ref:          config.CredentialRef{Purpose: "reviewer_credentials", Store: storeID, Ref: "codereview/shared", Mode: string(config.GitAuthModeGitHubApp)},
			PreviousRef:  &config.CredentialRef{Purpose: "reviewer_credentials", Store: storeID, Ref: "codereview/shared", Mode: string(config.GitAuthModePAT)},
			SecretsStore: resolved,
			KeySpecs:     []KeySpec{{Key: GitHubAppPrivateKeyKey, Required: true}},
			State:        PlanStateKeepExisting,
		})
	}

	groups, err := GroupStaleReviewerCredentialCleanupsByStore(config.File{}, entries)
	if err != nil {
		t.Fatalf("GroupStaleReviewerCredentialCleanupsByStore: %v", err)
	}
	if len(groups) != 2 || groups[0].Resolved.ID != "personal" || groups[1].Resolved.ID != "work" {
		t.Fatalf("groups = %#v, want one cleanup group per configured store", groups)
	}
}

func TestResolvedSecretsStoreIdentityIsComparableWithoutStringEncoding(t *testing.T) {
	a := ResolvedSecretsStore{ID: "a|b", Backend: "c"}.Identity()
	b := ResolvedSecretsStore{ID: "a", Backend: "b|c"}.Identity()
	stores := map[StoreIdentity]bool{a: true, b: true}
	if len(stores) != 2 {
		t.Fatalf("store identities collided: %#v", stores)
	}
}

func planningBasicProfile(profile string) config.Profile {
	ref, err := FormatRef(profile)
	if err != nil {
		panic(err)
	}
	return config.Profile{
		Git: config.GitConfig{
			Host:       "github.com",
			AuthMode:   config.GitAuthModePAT,
			Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: ref},
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	}
}

func planningAPIKeyProfile(profile string, provider config.LLMProvider) config.Profile {
	p := planningBasicProfile(profile)
	p.LLM = config.LLMConfig{
		Provider:   provider,
		Auth:       config.LLMAuthAPIKey,
		Adapter:    config.LLMAdapterAnthropicAPI,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/" + profile + "-llm"},
	}
	if provider == config.LLMProviderOpenAI {
		p.LLM.Adapter = config.LLMAdapterOpenAIAPI
	}
	return p
}
