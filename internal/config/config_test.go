package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
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

func TestRepositoryProfileRoutesRoundTripAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := validFile()
	cfg.RepositoryProfiles = []RepositoryProfile{
		{
			Profile: "work",
			Match: RepositoryProfileMatch{
				Host:      "https://GITHUB.com/",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
		{
			Profile: "home",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.RepositoryProfiles[0].Match.Host; got != "github.com" {
		t.Fatalf("route host = %q, want normalized github.com", got)
	}

	tests := []struct {
		name        string
		requested   string
		explicit    bool
		target      RepositoryTarget
		wantProfile string
		wantCredRef string
	}{
		{
			name:        "repo-specific route",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"},
			wantProfile: "work",
			wantCredRef: "codereview/work",
		},
		{
			name:        "target trims namespace and repo",
			target:      RepositoryTarget{Host: "github.com", Namespace: " rianjs ", Repo: " baz "},
			wantProfile: "work",
			wantCredRef: "codereview/work",
		},
		{
			name:        "namespace route fallback",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "zeta"},
			wantProfile: "home",
			wantCredRef: "codereview/home",
		},
		{
			name:        "default fallback",
			target:      RepositoryTarget{Host: "github.com", Namespace: "open-cli-collective", Repo: "codereview-cli"},
			wantProfile: "home",
			wantCredRef: "codereview/home",
		},
		{
			name:        "explicit profile bypasses route",
			requested:   "work",
			explicit:    true,
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "zeta"},
			wantProfile: "work",
			wantCredRef: "codereview/work",
		},
		{
			name:        "repo case-sensitive",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "Baz"},
			wantProfile: "home",
			wantCredRef: "codereview/home",
		},
		{
			name:        "namespace case-sensitive",
			target:      RepositoryTarget{Host: "github.com", Namespace: "Rianjs", Repo: "baz"},
			wantProfile: "home",
			wantCredRef: "codereview/home",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, profile, err := ResolveProfileForRepository(loaded, tt.requested, tt.explicit, tt.target)
			if err != nil {
				t.Fatalf("ResolveProfileForRepository: %v", err)
			}
			if name != tt.wantProfile || profile.Git.CredentialRef != tt.wantCredRef {
				t.Fatalf("resolved (%q,%q), want (%q,%q)", name, profile.Git.CredentialRef, tt.wantProfile, tt.wantCredRef)
			}
		})
	}

	cfg.RepositoryProfiles = []RepositoryProfile{
		{
			Profile: "home",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"baz"},
			},
		},
	}
	name, profile, err := ResolveProfileForRepository(cfg, "", false, RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"})
	if err != nil {
		t.Fatalf("ResolveProfileForRepository inverse order: %v", err)
	}
	if name != "work" || profile.Git.CredentialRef != "codereview/work" {
		t.Fatalf("inverse-order resolved (%q,%q), want work route", name, profile.Git.CredentialRef)
	}
}

func TestResolveProfileForRepositoryWithSource(t *testing.T) {
	cfg := validFile()
	cfg.RepositoryProfiles = []RepositoryProfile{
		{
			Profile: "work",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"baz"},
			},
		},
		{
			Profile: "home",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	}
	tests := []struct {
		name        string
		requested   string
		explicit    bool
		target      RepositoryTarget
		wantProfile string
		wantSource  RepositoryProfileResolutionSource
		wantRoute   *RepositoryProfile
	}{
		{
			name:        "explicit profile source",
			requested:   "work",
			explicit:    true,
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "zeta"},
			wantProfile: "work",
			wantSource:  RepositoryProfileResolutionSourceExplicit,
		},
		{
			name:        "repo route source",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"},
			wantProfile: "work",
			wantSource:  RepositoryProfileResolutionSourceRoute,
			wantRoute: &RepositoryProfile{
				Profile: "work",
				Match: RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "rianjs",
					Repos:     []string{"baz"},
				},
			},
		},
		{
			name:        "namespace route source",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "zeta"},
			wantProfile: "home",
			wantSource:  RepositoryProfileResolutionSourceRoute,
			wantRoute: &RepositoryProfile{
				Profile: "home",
				Match: RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "rianjs",
				},
			},
		},
		{
			name:        "default source",
			target:      RepositoryTarget{Host: "github.com", Namespace: "open-cli-collective", Repo: "codereview-cli"},
			wantProfile: "home",
			wantSource:  RepositoryProfileResolutionSourceDefault,
		},
		{
			name:        "explicit empty profile still bypasses route",
			requested:   "",
			explicit:    true,
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"},
			wantProfile: "home",
			wantSource:  RepositoryProfileResolutionSourceExplicit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveProfileForRepositoryWithSource(cfg, tt.requested, tt.explicit, tt.target)
			if err != nil {
				t.Fatalf("ResolveProfileForRepositoryWithSource: %v", err)
			}
			if got.ProfileName != tt.wantProfile {
				t.Fatalf("profile = %q, want %q", got.ProfileName, tt.wantProfile)
			}
			if got.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", got.Source, tt.wantSource)
			}
			if !reflect.DeepEqual(got.MatchedRoute, tt.wantRoute) {
				t.Fatalf("matched_route = %#v, want %#v", got.MatchedRoute, tt.wantRoute)
			}
		})
	}
}

func TestSaveOmitsEmptyRepositoryProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, validFile()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(body), "repository_profiles") {
		t.Fatalf("saved config contains empty repository_profiles:\n%s", string(body))
	}
}

func TestValidateRejectsInvalidRepositoryProfiles(t *testing.T) {
	validRoute := RepositoryProfile{
		Profile: "home",
		Match: RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
		},
	}
	tests := []struct {
		name   string
		routes []RepositoryProfile
		want   error
	}{
		{
			name:   "blank profile",
			routes: []RepositoryProfile{{Profile: " ", Match: validRoute.Match}},
			want:   ErrInvalid,
		},
		{
			name:   "missing profile",
			routes: []RepositoryProfile{{Profile: "missing", Match: validRoute.Match}},
			want:   ErrProfileNotFound,
		},
		{
			name: "blank host",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: " ", Namespace: "rianjs"},
			}},
			want: ErrInvalid,
		},
		{
			name: "blank namespace",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: "github.com", Namespace: " "},
			}},
			want: ErrInvalid,
		},
		{
			name: "empty repos",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{}},
			}},
			want: ErrInvalid,
		},
		{
			name: "blank repo",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"bar", " "}},
			}},
			want: ErrInvalid,
		},
		{
			name: "duplicate repo in route after trim",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"bar", " bar "}},
			}},
			want: ErrInvalid,
		},
		{
			name: "duplicate namespace route",
			routes: []RepositoryProfile{
				validRoute,
				validRoute,
			},
			want: ErrInvalid,
		},
		{
			name: "duplicate repo route",
			routes: []RepositoryProfile{
				{Profile: "home", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"bar"}}},
				{Profile: "home", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"bar"}}},
			},
			want: ErrInvalid,
		},
		{
			name: "route host must match profile host",
			routes: []RepositoryProfile{{
				Profile: "home",
				Match:   RepositoryProfileMatch{Host: "gitlab.com", Namespace: "rianjs"},
			}},
			want: ErrInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			cfg.RepositoryProfiles = tt.routes
			if err := Validate(cfg); !errors.Is(err, tt.want) {
				t.Fatalf("Validate error = %v, want %v", err, tt.want)
			}
		})
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

func TestCredentialRefsIncludesGitHubAppModes(t *testing.T) {
	profile := validFile().normalized().Profiles["work"]
	profile.Git.AuthMode = GitAuthModeGitHubApp
	profile.ReviewerCredentials.AuthMode = GitAuthModeGitHubApp

	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	want := []CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: "github_app"},
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "github_app"},
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

func TestValidateSecretsProfiles(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr error
		wantMsg string
	}{
		{
			name: "valid configured secrets profile",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					DefaultProfile: "personal",
					Profiles: map[string]SecretsProfile{
						"personal": {
							Label: "Personal Keychain",
							Backend: SecretsProfileBackend{
								Kind: SecretsBackendKind(credstore.BackendKeychain),
							},
						},
					},
				}
			},
		},
		{
			name: "missing configured default secrets profile",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{DefaultProfile: "missing"}
			},
			wantErr: ErrProfileNotFound,
			wantMsg: `secrets.default_profile "missing"`,
		},
		{
			name: "invalid secrets backend kind",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Profiles: map[string]SecretsProfile{
						"broken": {
							Backend: SecretsProfileBackend{Kind: "bogus"},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: `secrets.profiles.broken.backend.kind "bogus" is invalid`,
		},
		{
			name: "multiline label invalid",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Profiles: map[string]SecretsProfile{
						"broken": {
							Label:   "line one\nline two",
							Backend: SecretsProfileBackend{Kind: SecretsBackendKind(credstore.BackendMemory)},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: "secrets.profiles.broken.label must be a single line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			tt.mutate(&cfg)
			err := Validate(cfg)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Validate error = %v, want message containing %q", err, tt.wantMsg)
			}
		})
	}
}

func TestEffectiveSecretsProfiles(t *testing.T) {
	t.Run("configured secrets profiles win inventory", func(t *testing.T) {
		cfg := validFile()
		cfg.Keyring.Backend = string(credstore.BackendMemory)
		cfg.Secrets = SecretsConfig{
			DefaultProfile: "work-vault",
			Profiles: map[string]SecretsProfile{
				"personal": {
					Label:   "Personal Keychain",
					Backend: SecretsProfileBackend{Kind: SecretsBackendKind(credstore.BackendKeychain)},
				},
				"work-vault": {
					Label:   "Work File Store",
					Backend: SecretsProfileBackend{Kind: SecretsBackendKind(credstore.BackendFile)},
				},
			},
		}

		got := EffectiveSecretsProfiles(cfg)
		want := []EffectiveSecretsProfile{
			{
				ID:      "personal",
				Label:   "Personal Keychain",
				Backend: string(credstore.BackendKeychain),
				Source:  EffectiveSecretsProfileSourceConfigured,
			},
			{
				ID:        "work-vault",
				Label:     "Work File Store",
				Backend:   string(credstore.BackendFile),
				IsDefault: true,
				Source:    EffectiveSecretsProfileSourceConfigured,
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EffectiveSecretsProfiles = %#v, want %#v", got, want)
		}
	})

	t.Run("legacy keyring backend projects deterministic default", func(t *testing.T) {
		cfg := validFile()
		cfg.Keyring.Backend = string(credstore.BackendMemory)

		got := EffectiveSecretsProfiles(cfg)
		want := []EffectiveSecretsProfile{{
			ID:        LegacyProjectedSecretsProfileID,
			Label:     "Legacy default",
			Backend:   string(credstore.BackendMemory),
			IsDefault: true,
			Source:    EffectiveSecretsProfileSourceProjectedLegacy,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EffectiveSecretsProfiles = %#v, want %#v", got, want)
		}
	})

	t.Run("omitted legacy backend still projects auto default", func(t *testing.T) {
		cfg := validFile()
		cfg.Keyring.Backend = ""

		got := EffectiveSecretsProfiles(cfg)
		want := []EffectiveSecretsProfile{{
			ID:        LegacyProjectedSecretsProfileID,
			Label:     "Legacy default",
			Backend:   ProjectedLegacySecretsBackendKind,
			IsDefault: true,
			Source:    EffectiveSecretsProfileSourceProjectedLegacy,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EffectiveSecretsProfiles = %#v, want %#v", got, want)
		}
	})
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

func TestSaveLegacyConfigDoesNotPersistProjectedSecretsProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := validFile()
	cfg.Keyring.Backend = string(credstore.BackendMemory)

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Secrets.Profiles) != 0 {
		t.Fatalf("secrets.profiles = %#v, want omitted explicit profiles for legacy config", got.Secrets.Profiles)
	}
	if got.Secrets.DefaultProfile != "" {
		t.Fatalf("secrets.default_profile = %q, want empty for legacy config", got.Secrets.DefaultProfile)
	}
	effective := EffectiveSecretsProfiles(got)
	if len(effective) != 1 || effective[0].ID != LegacyProjectedSecretsProfileID || effective[0].Source != EffectiveSecretsProfileSourceProjectedLegacy {
		t.Fatalf("EffectiveSecretsProfiles = %#v, want projected legacy default", effective)
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
		{name: "reviewer oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
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

func TestValidateAcceptsGitHubAppAuthModes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
	}{
		{name: "git github_app", mutate: func(cfg *File) {
			profile := cfg.Profiles["home"]
			profile.Git.AuthMode = GitAuthModeGitHubApp
			cfg.Profiles["home"] = profile
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
			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate error = %v, want nil", err)
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
		{name: "reviewer oauth_device", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
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
		{name: "reviewer oauth_device", body: `default_profile: work
keyring: {}
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: oauth_device
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

func TestLoadAcceptsGitHubAppAuthMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: work
keyring: {}
profiles:
  work:
    git:
      host: github.com
      auth_mode: github_app
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: github_app
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profile := cfg.Profiles["work"]
	if profile.Git.AuthMode != GitAuthModeGitHubApp || profile.ReviewerCredentials.AuthMode != GitAuthModeGitHubApp {
		t.Fatalf("auth modes = git:%s reviewer:%s, want github_app", profile.Git.AuthMode, profile.ReviewerCredentials.AuthMode)
	}
}

func TestLoadRejectsMultilineReviewerDisplayName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `default_profile: work
keyring: {}
profiles:
  work:
    git:
      host: github.com
      auth_mode: github_app
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: github_app
      credential_ref: codereview/work-reviewer
      display_name: |
        line one
        line two
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	_, err := Load(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "reviewer_credentials.display_name") {
		t.Fatalf("Load error = %v, want display_name validation", err)
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
		{name: "reviewer oauth_device", profile: func() Profile {
			profile := validFile().normalized().Profiles["work"]
			profile.ReviewerCredentials.AuthMode = GitAuthModeOAuthDevice
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

func TestValidateRetentionAppliesDefaultsAndPreservesExplicitZero(t *testing.T) {
	if err := ValidateRetention(RetentionConfig{}); err != nil {
		t.Fatalf("ValidateRetention omitted defaults: %v", err)
	}
	zero := 0
	if err := ValidateRetention(RetentionConfig{MaxAgeDays: &zero}); err != nil {
		t.Fatalf("ValidateRetention explicit zero: %v", err)
	}
	negative := -1
	if err := ValidateRetention(RetentionConfig{MaxAgeDays: &negative}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ValidateRetention negative error = %v, want ErrInvalid", err)
	}
	if err := ValidateRetention(RetentionConfig{Enforcement: RetentionEnforcement("sometimes")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ValidateRetention bad enforcement error = %v, want ErrInvalid", err)
	}
	defaults := DefaultRetentionConfig()
	if defaults.MaxAgeDaysValue() != 90 || defaults.Enforcement != RetentionAtWrite {
		t.Fatalf("DefaultRetentionConfig = %#v, want 90 days at_write", defaults)
	}
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
					DisplayName:   "Work reviewer bot",
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
