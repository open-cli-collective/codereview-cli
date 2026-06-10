package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/statedirtest"
)

func TestPathUsesCodereviewConfigScope(t *testing.T) {
	root := statedirtest.Hermetic(t)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	want := filepath.Join(userConfigDir, "codereview", "config.yml")
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Path = %q, want absolute", got)
	}
	if rel, err := filepath.Rel(root, got); err != nil || rel == ".." || rel == "." || len(rel) >= 2 && rel[:2] == ".." {
		t.Fatalf("Path = %q, want under hermetic root %q", got, root)
	}
	if _, err := os.Stat(filepath.Dir(got)); !os.IsNotExist(err) {
		t.Fatalf("Path must not create dir; stat err = %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	want := validFile()

	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want.normalized()) {
		t.Fatalf("Load = %#v, want %#v", got, want.normalized())
	}
}

func TestSaveCreatesPrivateConfigFileAndDoesNotTruncateOnInvalidSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yml")
	if err := Save(path, validFile()); err != nil {
		t.Fatalf("Save valid: %v", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != dirPerm {
			t.Fatalf("dir perm = %o, want %o", perm, dirPerm)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat file: %v", err)
		}
		if perm := fileInfo.Mode().Perm(); perm != filePerm {
			t.Fatalf("file perm = %o, want %o", perm, filePerm)
		}
	}

	if err := Save(path, File{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Save invalid error = %v, want ErrInvalid", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed after failed save:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestLoadMissingConfig(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yml"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Load missing error = %v, want ErrNotConfigured", err)
	}
}

func TestLoadRejectsEmptyAndMultipleDocuments(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "multiple documents", body: `default_profile: home
profiles: {}
---
default_profile: other
profiles: {}
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			writeFile(t, path, tt.body)
			_, err := Load(path)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
    review_policy:
      major_events: comment
`)

	_, err := Load(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load unknown field error = %v, want ErrInvalid", err)
	}
}

func TestLoadRejectsInvalidEnums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: bogus
`)
	_, err := Load(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load invalid enum error = %v, want ErrInvalid", err)
	}
}

func TestLoadAcceptsPiRPCSubscriptionProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: pi
profiles:
  pi:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/pi
    llm:
      provider: pi
      auth: subscription
      adapter: pi_rpc
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profile := cfg.Profiles["pi"]
	if profile.LLM.Provider != LLMProviderPi || profile.LLM.Adapter != LLMAdapterPiRPC {
		t.Fatalf("LLM = %#v, want pi/pi_rpc", profile.LLM)
	}
	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("CredentialRefs = %#v, want only git credential for subscription auth", refs)
	}
}

func TestLoadAcceptsCodexCLISubscriptionProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: codex
profiles:
  codex:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/codex
    llm:
      provider: openai
      auth: subscription
      adapter: codex_cli
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profile := cfg.Profiles["codex"]
	if profile.LLM.Provider != LLMProviderOpenAI || profile.LLM.Adapter != LLMAdapterCodexCLI {
		t.Fatalf("LLM = %#v, want openai/codex_cli", profile.LLM)
	}
	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("CredentialRefs = %#v, want only git credential for subscription auth", refs)
	}
}

func TestValidateRejectsInvalidPiCombinations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{name: "api key auth", mutate: func(profile *Profile) {
			profile.LLM.Auth = LLMAuthAPIKey
			profile.LLM.CredentialRef = "codereview/pi-llm"
		}},
		{name: "claude cli adapter", mutate: func(profile *Profile) {
			profile.LLM.Adapter = LLMAdapterClaudeCLI
		}},
		{name: "anthropic api adapter", mutate: func(profile *Profile) {
			profile.LLM.Adapter = LLMAdapterAnthropicAPI
		}},
		{name: "pi rpc adapter with anthropic provider", mutate: func(profile *Profile) {
			profile.LLM.Provider = LLMProviderAnthropic
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			profile := cfg.Profiles["home"]
			profile.LLM.Provider = LLMProviderPi
			profile.LLM.Auth = LLMAuthSubscription
			profile.LLM.Adapter = LLMAdapterPiRPC
			profile.LLM.CredentialRef = ""
			tt.mutate(&profile)
			cfg.Profiles["home"] = profile
			err := Validate(cfg)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "requires") {
				t.Fatalf("Validate error = %v, want Pi compatibility guidance", err)
			}
		})
	}
}

func TestValidateRejectsInvalidCodexCLICombinations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{name: "anthropic provider", mutate: func(profile *Profile) {
			profile.LLM.Provider = LLMProviderAnthropic
		}},
		{name: "api key auth", mutate: func(profile *Profile) {
			profile.LLM.Auth = LLMAuthAPIKey
			profile.LLM.CredentialRef = "codereview/codex-llm"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			profile := cfg.Profiles["home"]
			profile.LLM.Provider = LLMProviderOpenAI
			profile.LLM.Auth = LLMAuthSubscription
			profile.LLM.Adapter = LLMAdapterCodexCLI
			profile.LLM.CredentialRef = ""
			tt.mutate(&profile)
			cfg.Profiles["home"] = profile
			err := Validate(cfg)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "codex_cli requires provider openai and auth subscription") {
				t.Fatalf("Validate error = %v, want Codex compatibility guidance", err)
			}
		})
	}
}

func TestModelMapValidationAndResolution(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.LLM.Provider = LLMProviderOpenAI
	profile.LLM.Auth = LLMAuthSubscription
	profile.LLM.Adapter = LLMAdapterCodexCLI
	profile.LLM.ModelMap = ModelMap{
		" medium ": " gpt-custom ",
	}
	cfg.Profiles["home"] = profile

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, resolved, err := ResolveProfile(cfg, "home")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	effective := EffectiveModelMap(resolved.LLM)
	if effective[ModelTierSmall].Model != "gpt-5.4-mini" || effective[ModelTierSmall].Source != ModelMapSourceBuiltIn {
		t.Fatalf("small resolution = %#v, want built-in gpt-5.4-mini", effective[ModelTierSmall])
	}
	if effective[ModelTierMedium].Model != "gpt-custom" || effective[ModelTierMedium].Source != ModelMapSourceConfig {
		t.Fatalf("medium resolution = %#v, want config override", effective[ModelTierMedium])
	}
	if got, ok := ResolveModelTier(resolved.LLM, ModelTierLarge); !ok || got.Model != "gpt-5.5" || got.Source != ModelMapSourceBuiltIn {
		t.Fatalf("ResolveModelTier large = %#v ok=%t, want built-in gpt-5.5", got, ok)
	}
	if resolved.LLM.ReviewerModelTier != "" {
		t.Fatalf("ReviewerModelTier = %q, want empty by default", resolved.LLM.ReviewerModelTier)
	}
}

func TestReviewerModelTierValidationAndNormalization(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.LLM.ReviewerModelTier = " medium "
	cfg.Profiles["home"] = profile

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, resolved, err := ResolveProfile(cfg, "home")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if resolved.LLM.ReviewerModelTier != ModelTierMedium {
		t.Fatalf("ReviewerModelTier = %q, want %q", resolved.LLM.ReviewerModelTier, ModelTierMedium)
	}
}

func TestValidateRejectsInvalidReviewerModelTier(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.LLM.ReviewerModelTier = "flagship"
	cfg.Profiles["home"] = profile

	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "reviewer_model_tier") {
		t.Fatalf("Validate error = %v, want reviewer_model_tier guidance", err)
	}
}

func TestBuiltInModelMapIsProviderAdapterSpecific(t *testing.T) {
	tests := []struct {
		name     string
		provider LLMProvider
		adapter  LLMAdapter
		want     ModelMap
	}{
		{
			name:     "codex cli",
			provider: LLMProviderOpenAI,
			adapter:  LLMAdapterCodexCLI,
			want: ModelMap{
				"small":  "gpt-5.4-mini",
				"medium": "gpt-5.4",
				"large":  "gpt-5.5",
			},
		},
		{
			name:     "openai api",
			provider: LLMProviderOpenAI,
			adapter:  LLMAdapterOpenAIAPI,
			want: ModelMap{
				"small":  "gpt-5.4-mini",
				"medium": "gpt-5.4",
				"large":  "gpt-5.5",
			},
		},
		{
			name:     "claude cli",
			provider: LLMProviderAnthropic,
			adapter:  LLMAdapterClaudeCLI,
			want: ModelMap{
				"medium": "claude-sonnet-4-6",
				"large":  "claude-opus-4-8",
			},
		},
		{name: "anthropic api", provider: LLMProviderAnthropic, adapter: LLMAdapterAnthropicAPI, want: ModelMap{}},
		{name: "pi rpc", provider: LLMProviderPi, adapter: LLMAdapterPiRPC, want: ModelMap{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuiltInModelMap(tt.provider, tt.adapter); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuiltInModelMap = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestValidateRejectsInvalidModelMap(t *testing.T) {
	tests := []struct {
		name     string
		modelMap ModelMap
		want     string
	}{
		{name: "invalid tier", modelMap: ModelMap{"flagship": "gpt"}, want: `tier "flagship" is invalid`},
		{name: "blank model", modelMap: ModelMap{"medium": " \t "}, want: "model_map.medium is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			profile := cfg.Profiles["home"]
			profile.LLM.ModelMap = tt.modelMap
			cfg.Profiles["home"] = profile
			err := Validate(cfg)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveProfile(t *testing.T) {
	cfg := validFile().normalized()

	name, profile, err := ResolveProfile(cfg, "")
	if err != nil {
		t.Fatalf("ResolveProfile default: %v", err)
	}
	if name != "home" {
		t.Fatalf("default profile name = %q, want home", name)
	}
	if profile.Git.CredentialRef != "codereview/home" {
		t.Fatalf("default profile ref = %q, want codereview/home", profile.Git.CredentialRef)
	}

	name, profile, err = ResolveProfile(cfg, "work")
	if err != nil {
		t.Fatalf("ResolveProfile work: %v", err)
	}
	if name != "work" {
		t.Fatalf("explicit profile name = %q, want work", name)
	}
	if profile.LLM.Auth != LLMAuthAPIKey {
		t.Fatalf("work LLM auth = %q, want api_key", profile.LLM.Auth)
	}

	_, _, err = ResolveProfile(cfg, "missing")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("ResolveProfile missing error = %v, want ErrProfileNotFound", err)
	}
}

func TestCredentialRefs(t *testing.T) {
	cfg := validFile().normalized()
	_, profile, err := ResolveProfile(cfg, "work")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}

	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	want := []CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: "pat"},
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "pat"},
		{Purpose: "llm", Ref: "codereview/work-llm", Mode: "api_key", Provider: "anthropic"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("CredentialRefs = %#v, want %#v", refs, want)
	}
}

func TestValidateRejectsMissingDefaultProfile(t *testing.T) {
	cfg := validFile()
	cfg.DefaultProfile = "missing"

	if err := Validate(cfg); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("Validate error = %v, want ErrProfileNotFound", err)
	}
}

func TestKeyringBackendRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := validFile()
	cfg.Keyring.Backend = "memory"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Keyring.Backend != "memory" {
		t.Fatalf("keyring.backend = %q, want memory", got.Keyring.Backend)
	}

	cfg.Keyring.Backend = "bogus"
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate invalid backend error = %v, want ErrInvalid", err)
	}
}

func TestValidateRejectsReservedGitAuthModes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
	}{
		{name: "git oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.AuthMode = GitAuthModeOAuthDevice
			cfg.Profiles["home"] = profile
		}},
		{name: "git github_app", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.AuthMode = GitAuthModeGitHubApp
			cfg.Profiles["home"] = profile
		}},
		{name: "reviewer oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
			cfg.Profiles["work"] = profile
		}},
		{name: "reviewer github_app", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeGitHubApp
			cfg.Profiles["work"] = profile
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			tt.mutate(&cfg)
			if err := Validate(cfg); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Validate error = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestSaveRejectsReservedGitAuthModes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
	}{
		{name: "git oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.AuthMode = GitAuthModeOAuthDevice
			cfg.Profiles["home"] = profile
		}},
		{name: "git github_app", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.AuthMode = GitAuthModeGitHubApp
			cfg.Profiles["home"] = profile
		}},
		{name: "reviewer oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
			cfg.Profiles["work"] = profile
		}},
		{name: "reviewer github_app", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeGitHubApp
			cfg.Profiles["work"] = profile
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			tt.mutate(&cfg)
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := Save(path, cfg); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Save error = %v, want ErrUnsupported", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("saved unsupported config, stat err = %v", err)
			}
		})
	}
}

func TestLoadRejectsReservedGitAuthModes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "git oauth_device", body: `default_profile: home
keyring: {}
profiles:
  home:
    git:
      host: github.com
      auth_mode: oauth_device
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`},
		{name: "reviewer github_app", body: `default_profile: work
keyring: {}
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: github_app
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			writeFile(t, path, tt.body)
			_, err := Load(path)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Load error = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestCredentialRefsRejectReservedGitAuthModes(t *testing.T) {
	tests := []struct {
		name    string
		profile func() Profile
	}{
		{name: "git oauth_device", profile: func() Profile {
			profile := validFile().normalized().Profiles["home"]
			profile.Git.AuthMode = GitAuthModeOAuthDevice
			return profile
		}},
		{name: "git github_app", profile: func() Profile {
			profile := validFile().normalized().Profiles["home"]
			profile.Git.AuthMode = GitAuthModeGitHubApp
			return profile
		}},
		{name: "reviewer oauth_device", profile: func() Profile {
			profile := validFile().normalized().Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
			return profile
		}},
		{name: "reviewer github_app", profile: func() Profile {
			profile := validFile().normalized().Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeGitHubApp
			return profile
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := CredentialRefs(tt.profile())
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("CredentialRefs error = %v, want ErrUnsupported", err)
			}
			if refs != nil {
				t.Fatalf("CredentialRefs = %#v, want nil", refs)
			}
		})
	}
}

func TestSubscriptionLLMCredentialsAreAdapterManaged(t *testing.T) {
	cfg := validFile().normalized()
	_, profile, err := ResolveProfile(cfg, "home")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}

	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	want := []CredentialRef{{Purpose: "git", Ref: "codereview/home", Mode: "pat"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("CredentialRefs = %#v, want %#v", refs, want)
	}
}

func TestAPIKeyLLMRequiresCredentialRef(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["work"]
	profile.LLM.CredentialRef = ""
	cfg.Profiles["work"] = profile

	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
}

func TestValidateRejectsInvalidCredentialRefs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
	}{
		{name: "subscription LLM stored ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.LLM.CredentialRef = "codereview/home-llm"
			cfg.Profiles["home"] = profile
		}},
		{name: "empty reviewer credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.CredentialRef = ""
			cfg.Profiles["work"] = profile
		}},
		{name: "reviewer credential ref matches git credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.CredentialRef = profile.Git.CredentialRef
			cfg.Profiles["work"] = profile
		}},
		{name: "llm credential ref matches git credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.LLM.CredentialRef = profile.Git.CredentialRef
			cfg.Profiles["work"] = profile
		}},
		{name: "llm credential ref matches reviewer credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.LLM.CredentialRef = profile.ReviewerCredentials.CredentialRef
			cfg.Profiles["work"] = profile
		}},
		{name: "git ref invalid chars", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.CredentialRef = "codereview/bad.profile"
			cfg.Profiles["home"] = profile
		}},
		{name: "git ref wrong service", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.CredentialRef = "other/home"
			cfg.Profiles["home"] = profile
		}},
		{name: "llm ref invalid chars", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.LLM.CredentialRef = "codereview/work.llm"
			cfg.Profiles["work"] = profile
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			tt.mutate(&cfg)
			if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidationCoversAgentSourcesReviewPolicyAndRetention(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["work"]
	profile.AgentSources = append(profile.AgentSources, "")
	cfg.Profiles["work"] = profile
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty agent source Validate error = %v, want ErrInvalid", err)
	}

	cfg = validFile()
	profile = cfg.Profiles["work"]
	profile.ReviewPolicy.ResolveThreads = ResolveThreadsPolicy("sometimes")
	cfg.Profiles["work"] = profile
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad resolve_threads Validate error = %v, want ErrInvalid", err)
	}

	cfg = validFile()
	profile = cfg.Profiles["work"]
	profile.ReviewPolicy.ResolveAfter = "two days"
	cfg.Profiles["work"] = profile
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad resolve_after Validate error = %v, want ErrInvalid", err)
	}

	cfg = validFile()
	cfg.Data.Retention.MaxAgeDays = intPtr(-1)
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad retention Validate error = %v, want ErrInvalid", err)
	}
}

func TestRetentionMaxAgeDefaultAndExplicitZero(t *testing.T) {
	t.Run("omitted defaults to 90", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		writeFile(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
data:
  retention:
    enforcement: at_write
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Data.Retention.MaxAgeDaysValue(); got != 90 {
			t.Fatalf("MaxAgeDaysValue = %d, want 90", got)
		}
	})

	t.Run("explicit zero means keep forever", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		writeFile(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
data:
  retention:
    max_age_days: 0
    enforcement: at_write
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Data.Retention.MaxAgeDaysValue(); got != 0 {
			t.Fatalf("MaxAgeDaysValue = %d, want 0", got)
		}
	})
}

func validFile() File {
	return File{
		DefaultProfile: "home",
		Profiles: map[string]Profile{
			"home": {
				Git: GitConfig{
					Host:          "github.com",
					AuthMode:      GitAuthModePAT,
					CredentialRef: "codereview/home",
					IdentityCache: "rianjs",
				},
				LLM: LLMConfig{
					Provider: LLMProviderAnthropic,
					Auth:     LLMAuthSubscription,
					Adapter:  LLMAdapterClaudeCLI,
				},
				AgentSources: []string{"~/dev/my-reviewers"},
				ReviewPolicy: ReviewPolicy{
					MajorEvent:       ReviewMajorEventComment,
					AllowSelfApprove: false,
				},
			},
			"work": {
				Git: GitConfig{
					Host:          "github.com",
					AuthMode:      GitAuthModePAT,
					CredentialRef: "codereview/work",
					IdentityCache: "rianjs",
				},
				ReviewerCredentials: &ReviewerCredentials{
					AuthMode:      GitAuthModePAT,
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "acme-review-bot",
				},
				LLM: LLMConfig{
					Provider:      LLMProviderAnthropic,
					Auth:          LLMAuthAPIKey,
					Adapter:       LLMAdapterAnthropicAPI,
					CredentialRef: "codereview/work-llm",
				},
				AgentSources: []string{"~/dev/work-reviewers"},
				ReviewPolicy: ReviewPolicy{
					MajorEvent:       ReviewMajorEventRequestChanges,
					AllowSelfApprove: true,
					ResolveThreads:   ResolveThreadsNever,
					ResolveAfter:     "48h",
				},
			},
		},
		Data: DataConfig{
			Retention: RetentionConfig{
				MaxAgeDays:  intPtr(90),
				Enforcement: RetentionAtWrite,
			},
		},
	}
}

func intPtr(value int) *int {
	return &value
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
