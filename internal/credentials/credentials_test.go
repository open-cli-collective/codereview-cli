package credentials

import (
	"errors"
	"reflect"
	"testing"

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

func TestStoreOptionsInvalidBackendFlag(t *testing.T) {
	_, err := StoreOptions("bogus", true, config.File{})
	if !errors.Is(err, ErrInvalidBackendSelection) {
		t.Fatalf("StoreOptions error = %v, want ErrInvalidBackendSelection", err)
	}
}

func TestAllowedKeysExactCredentialMatrix(t *testing.T) {
	want := []string{GitTokenKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey}
	if got := AllowedKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedKeys = %#v, want %#v", got, want)
	}

	for _, key := range []string{
		LegacyLLMAPIKeyKey,
		"git_app_private_key",
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
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "github_app"},
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

	if err := ValidateAllowedKeyForConfig(cfg, "codereview/undeclared", OpenAIAPIKeyKey); err != nil {
		t.Fatalf("ValidateAllowedKeyForConfig undeclared global key: %v", err)
	}
	if err := ValidateAllowedKeyForConfig(cfg, "codereview/undeclared", LegacyLLMAPIKeyKey); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("ValidateAllowedKeyForConfig undeclared legacy key error = %v, want ErrKeyNotAllowed", err)
	}
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
