package credentials

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestParseRefEnforcesCodereviewService(t *testing.T) {
	ref, err := ParseRef("codereview/work")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Profile != "work" || ref.Full != "codereview/work" {
		t.Fatalf("ParseRef = %#v, want work ref", ref)
	}

	_, err = ParseRef("other/work")
	if !errors.Is(err, ErrWrongService) {
		t.Fatalf("wrong-service error = %v, want ErrWrongService", err)
	}
}

func TestStoreOptionsBackendPrecedenceMetadata(t *testing.T) {
	t.Setenv(BackendEnvVar(), "")

	cfg := config.File{Keyring: config.KeyringConfig{Backend: "memory"}}
	store, err := OpenStore("", false, cfg)
	if err != nil {
		t.Fatalf("OpenStore config backend: %v", err)
	}
	backend, source := store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceConfig {
		t.Fatalf("Backend = (%s,%s), want (memory,config)", backend, source)
	}

	store, err = OpenStore("memory", true, config.File{})
	if err != nil {
		t.Fatalf("OpenStore explicit backend: %v", err)
	}
	backend, source = store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceExplicit {
		t.Fatalf("Backend = (%s,%s), want (memory,explicit)", backend, source)
	}

	t.Setenv(BackendEnvVar(), "memory")
	store, err = OpenStore("", false, config.File{})
	if err != nil {
		t.Fatalf("OpenStore env backend: %v", err)
	}
	backend, source = store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceEnv {
		t.Fatalf("Backend = (%s,%s), want (memory,env)", backend, source)
	}
}

func TestStoreOptionsRejectsLegacyOnePasswordBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    string
		flagSet bool
		cfg     config.File
	}{
		{name: "flag", flag: "op", flagSet: true, cfg: config.File{}},
		{name: "config", cfg: config.File{Keyring: config.KeyringConfig{Backend: "op-connect"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := StoreOptions(tc.flag, tc.flagSet, tc.cfg)
			if !errors.Is(err, ErrInvalidBackendSelection) {
				t.Fatalf("StoreOptions error = %v, want ErrInvalidBackendSelection", err)
			}
		})
	}

	t.Run("env", func(t *testing.T) {
		t.Setenv(BackendEnvVar(), "op-desktop")
		_, err := StoreOptions("", false, config.File{})
		if !errors.Is(err, ErrInvalidBackendSelection) {
			t.Fatalf("StoreOptions env error = %v, want ErrInvalidBackendSelection", err)
		}
	})
}

func TestStoreOptionsInvalidBackendFlag(t *testing.T) {
	_, err := StoreOptions("bogus", true, config.File{})
	if !errors.Is(err, ErrInvalidBackendSelection) {
		t.Fatalf("StoreOptions error = %v, want ErrInvalidBackendSelection", err)
	}
}

func TestAllowedKeysExactCredentialMatrix(t *testing.T) {
	want := []string{GitTokenKey, GitHubAppIDKey, GitHubAppPrivateKeyKey, GitHubAppInstallationIDKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey}
	if got := AllowedKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedKeys = %#v, want %#v", got, want)
	}

	for _, key := range []string{
		LegacyLLMAPIKeyKey,
		"git_oauth_access_token",
		"git_oauth_refresh_token",
	} {
		if err := ValidateAllowedKey(key); !errors.Is(err, credstore.ErrKeyNotAllowed) {
			t.Fatalf("ValidateAllowedKey(%q) error = %v, want ErrKeyNotAllowed", key, err)
		}
	}
}

func TestKeySpecsForPurposeCredentialMatrix(t *testing.T) {
	tests := []struct {
		name string
		ref  config.CredentialRef
		want []KeySpec
	}{
		{
			name: "user git pat",
			ref:  config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: "pat"},
			want: []KeySpec{{Key: GitTokenKey, Required: true}},
		},
		{
			name: "reviewer pat",
			ref:  config.CredentialRef{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "pat"},
			want: []KeySpec{{Key: GitTokenKey, Required: true}},
		},
		{
			name: "user git github app",
			ref:  config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: "github_app"},
			want: []KeySpec{
				{Key: GitHubAppIDKey, Required: true},
				{Key: GitHubAppPrivateKeyKey, Required: true},
				{Key: GitHubAppInstallationIDKey, Required: false},
			},
		},
		{
			name: "reviewer github app",
			ref:  config.CredentialRef{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "github_app"},
			want: []KeySpec{
				{Key: GitHubAppIDKey, Required: true},
				{Key: GitHubAppPrivateKeyKey, Required: true},
				{Key: GitHubAppInstallationIDKey, Required: false},
			},
		},
		{
			name: "anthropic api key",
			ref:  config.CredentialRef{Purpose: "llm", Ref: "codereview/work-llm", Mode: "api_key", Provider: "anthropic"},
			want: []KeySpec{{Key: AnthropicAPIKeyKey, Required: true}},
		},
		{
			name: "openai api key",
			ref:  config.CredentialRef{Purpose: "llm", Ref: "codereview/work-llm", Mode: "api_key", Provider: "openai"},
			want: []KeySpec{{Key: OpenAIAPIKeyKey, Required: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := KeySpecsForPurpose(tt.ref)
			if err != nil {
				t.Fatalf("KeySpecsForPurpose: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("KeySpecsForPurpose = %#v, want %#v", got, tt.want)
			}
		})
	}

	for _, ref := range []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: "oauth_device"},
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "oauth_device"},
	} {
		if _, err := KeySpecsForPurpose(ref); !errors.Is(err, config.ErrUnsupported) {
			t.Fatalf("KeySpecsForPurpose(%#v) error = %v, want ErrUnsupported", ref, err)
		}
	}
}

func TestValidateAllowedKeyForConfigNarrowsDeclaredRefs(t *testing.T) {
	cfg := config.File{
		DefaultProfile: "anthropic",
		Profiles: map[string]config.Profile{
			"anthropic": matrixProfile("codereview/git-a", "codereview/shared-llm", config.LLMProviderAnthropic),
			"openai":    matrixProfile("codereview/git-b", "codereview/shared-llm", config.LLMProviderOpenAI),
			"app":       githubAppMatrixProfile("codereview/app"),
		},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate config: %v", err)
	}

	wantLLMKeys := []string{AnthropicAPIKeyKey, OpenAIAPIKeyKey}
	gotLLMKeys, err := ExpectedKeysForConfigRef(cfg, "codereview/shared-llm")
	if err != nil {
		t.Fatalf("ExpectedKeysForConfigRef llm: %v", err)
	}
	if !reflect.DeepEqual(gotLLMKeys, wantLLMKeys) {
		t.Fatalf("LLM expected keys = %#v, want %#v", gotLLMKeys, wantLLMKeys)
	}
	for _, key := range wantLLMKeys {
		if err := ValidateAllowedKeyForConfig(cfg, "codereview/shared-llm", key); err != nil {
			t.Fatalf("ValidateAllowedKeyForConfig llm %s: %v", key, err)
		}
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/shared-llm", GitTokenKey); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("ValidateAllowedKeyForConfig llm git key error = %v, want ErrKeyNotAllowed", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/git-a", AnthropicAPIKeyKey); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("ValidateAllowedKeyForConfig git llm key error = %v, want ErrKeyNotAllowed", err)
	}
	wantAppKeys := []string{GitHubAppIDKey, GitHubAppInstallationIDKey, GitHubAppPrivateKeyKey}
	gotAppKeys, err := ExpectedKeysForConfigRef(cfg, "codereview/app")
	if err != nil {
		t.Fatalf("ExpectedKeysForConfigRef github_app: %v", err)
	}
	if !reflect.DeepEqual(gotAppKeys, wantAppKeys) {
		t.Fatalf("github_app expected keys = %#v, want %#v", gotAppKeys, wantAppKeys)
	}
	for _, key := range wantAppKeys {
		if err := ValidateAllowedKeyForConfig(cfg, "codereview/app", key); err != nil {
			t.Fatalf("ValidateAllowedKeyForConfig app %s: %v", key, err)
		}
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/app", GitTokenKey); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("ValidateAllowedKeyForConfig app git token error = %v, want ErrKeyNotAllowed", err)
	}

	if err := ValidateAllowedKeyForConfig(cfg, "codereview/undeclared", OpenAIAPIKeyKey); err != nil {
		t.Fatalf("ValidateAllowedKeyForConfig undeclared global key: %v", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/undeclared", LegacyLLMAPIKeyKey); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("ValidateAllowedKeyForConfig undeclared legacy key error = %v, want ErrKeyNotAllowed", err)
	}
}

func githubAppMatrixProfile(ref string) config.Profile {
	p := matrixProfile(ref, "codereview/app-llm", config.LLMProviderAnthropic)
	p.Git.AuthMode = config.GitAuthModeGitHubApp
	p.LLM.Auth = config.LLMAuthSubscription
	p.LLM.Adapter = config.LLMAdapterClaudeCLI
	p.LLM.CredentialRef = ""
	return p
}

func TestExpectedKeysForConfigRefIgnoresUnrelatedUnsupportedProfiles(t *testing.T) {
	cfg := config.File{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work":       matrixProfile("codereview/work", "codereview/work-llm", config.LLMProviderAnthropic),
			"shared-pat": matrixProfile("codereview/shared-git", "codereview/shared-llm", config.LLMProviderOpenAI),
			"future": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModeOAuthDevice,
					CredentialRef: "codereview/future", // #nosec G101 -- keyring ref, not a secret value.
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
			"shared-future": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModeOAuthDevice,
					CredentialRef: "codereview/shared-git",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}

	if err := ValidateAllowedKeyForConfig(cfg, "codereview/work-llm", AnthropicAPIKeyKey); err != nil {
		t.Fatalf("ValidateAllowedKeyForConfig matching supported ref: %v", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/undeclared", OpenAIAPIKeyKey); err != nil {
		t.Fatalf("ValidateAllowedKeyForConfig undeclared ref: %v", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/shared-git", GitTokenKey); err != nil {
		t.Fatalf("ValidateAllowedKeyForConfig mixed shared ref: %v", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/future", GitTokenKey); !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("ValidateAllowedKeyForConfig matching unsupported ref error = %v, want ErrUnsupported", err)
	}
}

func TestResolveSecretsProfileForProfileAndRef(t *testing.T) {
	cfg := config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Secrets: config.SecretsConfig{
			DefaultProfile: "work-file",
			Profiles: map[string]config.SecretsProfile{
				"personal-keychain": {
					Label:   "Personal Keychain",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)},
				},
				"work-file": {
					Label:   "Work File Store",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": func() config.Profile {
				p := matrixProfile("codereview/shared-git", "codereview/home-llm", config.LLMProviderAnthropic)
				p.SecretsProfile = "personal-keychain"
				return p
			}(),
			"work": func() config.Profile {
				p := matrixProfile("codereview/shared-git", "codereview/work-llm", config.LLMProviderOpenAI)
				return p
			}(),
		},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate config: %v", err)
	}

	homeResolved, err := ResolveSecretsProfileForProfile(cfg, cfg.Profiles["home"])
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile(home): %v", err)
	}
	wantHome := ResolvedSecretsProfile{
		ID:              "personal-keychain",
		Label:           "Personal Keychain",
		Backend:         "keychain",
		Source:          config.EffectiveSecretsProfileSourceConfigured,
		SelectionSource: SecretsProfileSelectionExplicit,
	}
	if !reflect.DeepEqual(homeResolved, wantHome) {
		t.Fatalf("home resolved secrets profile = %#v, want %#v", homeResolved, wantHome)
	}

	workResolved, err := ResolveSecretsProfileForProfile(cfg, cfg.Profiles["work"])
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile(work): %v", err)
	}
	wantWork := ResolvedSecretsProfile{
		ID:              "work-file",
		Label:           "Work File Store",
		Backend:         "file",
		Source:          config.EffectiveSecretsProfileSourceConfigured,
		SelectionSource: SecretsProfileSelectionDefault,
	}
	if !reflect.DeepEqual(workResolved, wantWork) {
		t.Fatalf("work resolved secrets profile = %#v, want %#v", workResolved, wantWork)
	}

	if _, err := ResolveSecretsProfileForRef(cfg, "codereview/shared-git", ""); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("ResolveSecretsProfileForRef(shared-git) error = %v, want ErrInvalid ambiguity", err)
	}
	selectedResolved, err := ResolveSecretsProfileForRef(cfg, "codereview/shared-git", "home")
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForRef(shared-git, home): %v", err)
	}
	if !reflect.DeepEqual(selectedResolved, wantHome) {
		t.Fatalf("selected resolved secrets profile = %#v, want %#v", selectedResolved, wantHome)
	}
	undeclaredResolved, err := ResolveSecretsProfileForRef(cfg, "codereview/custom-ref", "work")
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForRef(custom-ref, work): %v", err)
	}
	if !reflect.DeepEqual(undeclaredResolved, wantWork) {
		t.Fatalf("undeclared resolved secrets profile = %#v, want %#v", undeclaredResolved, wantWork)
	}

	cfg.Secrets.DefaultProfile = ""
	legacyResolved, err := ResolveSecretsProfileForProfile(cfg, cfg.Profiles["work"])
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile(work legacy): %v", err)
	}
	wantLegacy := ResolvedSecretsProfile{
		ID:              config.LegacyProjectedSecretsProfileID,
		Label:           "Legacy default",
		Backend:         "memory",
		Source:          config.EffectiveSecretsProfileSourceProjectedLegacy,
		SelectionSource: SecretsProfileSelectionLegacyDefault,
	}
	if !reflect.DeepEqual(legacyResolved, wantLegacy) {
		t.Fatalf("legacy resolved secrets profile = %#v, want %#v", legacyResolved, wantLegacy)
	}
}

func TestAllowedKeyMemoryRoundTrip(t *testing.T) {
	store, err := OpenStore("memory", true, config.File{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if err := store.Set("work", GitTokenKey, "token"); err != nil {
		t.Fatalf("Set allowed key: %v", err)
	}
	got, err := store.Get("work", GitTokenKey)
	if err != nil {
		t.Fatalf("Get allowed key: %v", err)
	}
	if got != "token" {
		t.Fatalf("Get = %q, want token", got)
	}
	if err := store.Set("work", "bad_key", "token"); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("Set disallowed key error = %v, want ErrKeyNotAllowed", err)
	}
}

func TestStoreOptionsForResolvedProfile_OnePasswordBackend(t *testing.T) {
	cfg := config.File{
		DefaultProfile: "home",
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"work-op": {
					Label: "Work 1Password",
					Backend: config.SecretsProfileBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOPConnect),
						OnePassword: &config.SecretsProfileOnePasswordConfig{
							Timeout:         "7s",
							VaultID:         "vault-123",
							ItemTitlePrefix: "cr",
							ItemTag:         "codereview",
							ItemFieldTitle:  "credential",
							ConnectHost:     "https://connect.example",
							ConnectTokenEnv: "CUSTOM_CONNECT_TOKEN",
						},
					},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": matrixProfile("codereview/shared-git", "codereview/home-llm", config.LLMProviderAnthropic),
		},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	resolved := ResolvedSecretsProfile{
		ID:      "work-op",
		Label:   "Work 1Password",
		Backend: string(credstore.BackendOPConnect),
		Source:  config.EffectiveSecretsProfileSourceConfigured,
	}
	got, err := StoreOptionsForResolvedProfile("", false, cfg, resolved)
	if err != nil {
		t.Fatalf("StoreOptionsForResolvedProfile: %v", err)
	}
	if got.Backend != credstore.BackendOPConnect {
		t.Fatalf("Backend = %q, want %q", got.Backend, credstore.BackendOPConnect)
	}
	if got.OnePassword == nil {
		t.Fatal("OnePassword = nil, want populated options")
	}
	if got.OnePassword.Timeout != 7*time.Second {
		t.Fatalf("Timeout = %v, want 7s", got.OnePassword.Timeout)
	}
	if got.OnePassword.VaultID != "vault-123" || got.OnePassword.ConnectHost != "https://connect.example" || got.OnePassword.ConnectTokenEnv != "CUSTOM_CONNECT_TOKEN" {
		t.Fatalf("OnePassword = %#v, want mapped non-secret fields", got.OnePassword)
	}
}

func TestCredentialStatuses(t *testing.T) {
	store := fakeKeyStatusStore{
		present: map[string]map[string]bool{
			"git": {
				GitTokenKey: true,
			},
			"app": {
				GitHubAppIDKey:         true,
				GitHubAppPrivateKeyKey: true,
			},
			"llm": {
				OpenAIAPIKeyKey: true,
			},
		},
	}
	refs := []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/git", Mode: "pat"},
		{Purpose: "reviewer_credentials", Ref: "codereview/app", Mode: "github_app"},
		{Purpose: "llm", Ref: "codereview/llm", Mode: "api_key", Provider: "openai"},
	}

	got, err := CredentialStatuses(store, refs, nil)
	if err != nil {
		t.Fatalf("CredentialStatuses: %v", err)
	}

	want := []CredentialStatus{
		{
			Purpose: "git",
			Ref:     "codereview/git",
			Mode:    "pat",
			Keys: []KeyStatus{
				presentKeyStatus(GitTokenKey, true),
			},
		},
		{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/app",
			Mode:    "github_app",
			Keys: []KeyStatus{
				presentKeyStatus(GitHubAppIDKey, true),
				presentKeyStatus(GitHubAppPrivateKeyKey, true),
				missingKeyStatus(GitHubAppInstallationIDKey, false),
			},
		},
		{
			Purpose:  "llm",
			Ref:      "codereview/llm",
			Mode:     "api_key",
			Provider: "openai",
			Keys: []KeyStatus{
				presentKeyStatus(OpenAIAPIKeyKey, true),
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialStatuses = %#v, want %#v", got, want)
	}
	if !RequiredKeysSatisfied(got[0]) {
		t.Fatalf("RequiredKeysSatisfied git = false, want true")
	}
	if !RequiredKeysSatisfied(got[1]) {
		t.Fatalf("RequiredKeysSatisfied github_app = false, want true")
	}
	if missing := MissingRequiredKeys(got[1]); len(missing) != 0 {
		t.Fatalf("MissingRequiredKeys github_app = %#v, want empty because optional key is missing", missing)
	}
}

func TestCredentialStatusesUnknown(t *testing.T) {
	refs := []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/git", Mode: "pat"},
	}

	t.Run("store open error", func(t *testing.T) {
		got, err := CredentialStatuses(nil, refs, errors.New("open failed"))
		if err != nil {
			t.Fatalf("CredentialStatuses: %v", err)
		}
		want := []CredentialStatus{
			{
				Purpose: "git",
				Ref:     "codereview/git",
				Mode:    "pat",
				Keys: []KeyStatus{
					unknownKeyStatus(GitTokenKey, true, "open failed"),
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CredentialStatuses store error = %#v, want %#v", got, want)
		}
		if RequiredKeysSatisfied(got[0]) {
			t.Fatalf("RequiredKeysSatisfied unknown = true, want false")
		}
		if missing := MissingRequiredKeys(got[0]); len(missing) != 0 {
			t.Fatalf("MissingRequiredKeys unknown = %#v, want empty", missing)
		}
	})

	t.Run("per-key exists error", func(t *testing.T) {
		store := fakeKeyStatusStore{
			errs: map[string]error{
				"git/" + GitTokenKey: errors.New("exists failed"),
			},
		}
		got, err := CredentialStatuses(store, refs, nil)
		if err != nil {
			t.Fatalf("CredentialStatuses: %v", err)
		}
		want := []CredentialStatus{
			{
				Purpose: "git",
				Ref:     "codereview/git",
				Mode:    "pat",
				Keys: []KeyStatus{
					unknownKeyStatus(GitTokenKey, true, "exists failed"),
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CredentialStatuses exists error = %#v, want %#v", got, want)
		}
	})
}

func TestCredentialStatusesPartialRequiredBundle(t *testing.T) {
	store := fakeKeyStatusStore{
		present: map[string]map[string]bool{
			"app": {
				GitHubAppIDKey: true,
			},
		},
	}
	ref := config.CredentialRef{
		Purpose: "reviewer_credentials",
		Ref:     "codereview/app",
		Mode:    "github_app",
	}

	got, err := CredentialRefStatus(store, ref, nil)
	if err != nil {
		t.Fatalf("CredentialRefStatus: %v", err)
	}
	want := CredentialStatus{
		Purpose: "reviewer_credentials",
		Ref:     "codereview/app",
		Mode:    "github_app",
		Keys: []KeyStatus{
			presentKeyStatus(GitHubAppIDKey, true),
			missingKeyStatus(GitHubAppPrivateKeyKey, true),
			missingKeyStatus(GitHubAppInstallationIDKey, false),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialRefStatus = %#v, want %#v", got, want)
	}
	if RequiredKeysSatisfied(got) {
		t.Fatalf("RequiredKeysSatisfied partial github_app = true, want false")
	}
	wantMissing := []string{GitHubAppPrivateKeyKey}
	if missing := MissingRequiredKeys(got); !reflect.DeepEqual(missing, wantMissing) {
		t.Fatalf("MissingRequiredKeys partial github_app = %#v, want %#v", missing, wantMissing)
	}
}

func TestMissingRequiredKeys(t *testing.T) {
	status := CredentialStatus{
		Purpose: "reviewer_credentials",
		Ref:     "codereview/app",
		Mode:    "github_app",
		Keys: []KeyStatus{
			missingKeyStatus(GitHubAppIDKey, true),
			unknownKeyStatus(GitHubAppPrivateKeyKey, true, "boom"),
			missingKeyStatus(GitHubAppInstallationIDKey, false),
		},
	}
	want := []string{GitHubAppIDKey}
	if got := MissingRequiredKeys(status); !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingRequiredKeys = %#v, want %#v", got, want)
	}
}

func matrixProfile(gitRef, llmRef string, provider config.LLMProvider) config.Profile {
	adapter := config.LLMAdapterAnthropicAPI
	if provider == config.LLMProviderOpenAI {
		adapter = config.LLMAdapterOpenAIAPI
	}
	return config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: gitRef,
		},
		LLM: config.LLMConfig{
			Provider:      provider,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       adapter,
			CredentialRef: llmRef,
		},
	}
}

type fakeKeyStatusStore struct {
	present map[string]map[string]bool
	errs    map[string]error
}

func (s fakeKeyStatusStore) Exists(profile, key string) (bool, error) {
	if err := s.errs[profile+"/"+key]; err != nil {
		return false, err
	}
	return s.present[profile][key], nil
}

func presentKeyStatus(key string, required bool) KeyStatus {
	present := true
	return KeyStatus{Key: key, Required: required, Present: &present, Status: "present"}
}

func missingKeyStatus(key string, required bool) KeyStatus {
	present := false
	return KeyStatus{Key: key, Required: required, Present: &present, Status: "missing"}
}

func unknownKeyStatus(key string, required bool, message string) KeyStatus {
	return KeyStatus{Key: key, Required: required, Status: "unknown", Error: message}
}
