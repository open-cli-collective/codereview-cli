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
		{name: "multiple documents", body: `profiles: {}
---
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
	writeFile(t, path, `profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/home
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
	writeFile(t, path, `profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/home
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
	writeFile(t, path, `llm_runtimes:
  pi-local:
    provider: pi
    auth: subscription
    adapter: pi_rpc
profiles:
  pi:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/pi
    reviewer:
      kind: git_identity
    llm_runtime: pi-local
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
	writeFile(t, path, `llm_runtimes:
  codex-local:
    provider: openai
    auth: subscription
    adapter: codex_cli
profiles:
  codex:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/codex
    reviewer:
      kind: git_identity
    llm_runtime: codex-local
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
		mutate func(*LLMConfig)
	}{
		{name: "api key auth", mutate: func(llm *LLMConfig) {
			llm.Auth = LLMAuthAPIKey
			llm.CredentialRef = "codereview/pi-llm"
		}},
		{name: "claude cli adapter", mutate: func(llm *LLMConfig) {
			llm.Adapter = LLMAdapterClaudeCLI
		}},
		{name: "anthropic api adapter", mutate: func(llm *LLMConfig) {
			llm.Adapter = LLMAdapterAnthropicAPI
		}},
		{name: "pi rpc adapter with anthropic provider", mutate: func(llm *LLMConfig) {
			llm.Provider = LLMProviderAnthropic
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			llm := cfg.LLMRuntimes["home-llm"]
			llm.Provider = LLMProviderPi
			llm.Auth = LLMAuthSubscription
			llm.Adapter = LLMAdapterPiRPC
			llm.CredentialRef = ""
			llm.Credential = CredentialLocation{}
			tt.mutate(&llm)
			cfg.LLMRuntimes["home-llm"] = llm
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
		mutate func(*LLMConfig)
	}{
		{name: "anthropic provider", mutate: func(llm *LLMConfig) {
			llm.Provider = LLMProviderAnthropic
		}},
		{name: "api key auth", mutate: func(llm *LLMConfig) {
			llm.Auth = LLMAuthAPIKey
			llm.CredentialRef = "codereview/codex-llm"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			llm := cfg.LLMRuntimes["home-llm"]
			llm.Provider = LLMProviderOpenAI
			llm.Auth = LLMAuthSubscription
			llm.Adapter = LLMAdapterCodexCLI
			llm.CredentialRef = ""
			llm.Credential = CredentialLocation{}
			tt.mutate(&llm)
			cfg.LLMRuntimes["home-llm"] = llm
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

func TestValidateRejectsClaudeCLIAPIKeyAuth(t *testing.T) {
	cfg := validFile()
	llm := cfg.LLMRuntimes["home-llm"]
	llm.Auth = LLMAuthAPIKey
	llm.CredentialRef = "codereview/claude-llm"
	cfg.LLMRuntimes["home-llm"] = llm

	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
	const want = "config: invalid: llm_runtimes.home-llm adapter claude_cli requires provider anthropic and auth subscription"
	if err.Error() != want {
		t.Fatalf("Validate error = %q, want %q", err, want)
	}
}

func TestModelMapValidationAndResolution(t *testing.T) {
	cfg := validFile()
	llm := cfg.LLMRuntimes["home-llm"]
	llm.Provider = LLMProviderOpenAI
	llm.Auth = LLMAuthSubscription
	llm.Adapter = LLMAdapterCodexCLI
	llm.ModelMap = ModelMap{
		" medium ": " gpt-custom ",
	}
	cfg.LLMRuntimes["home-llm"] = llm

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
	llm := cfg.LLMRuntimes["home-llm"]
	llm.ReviewerModelTier = " medium "
	cfg.LLMRuntimes["home-llm"] = llm

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
	llm := cfg.LLMRuntimes["home-llm"]
	llm.ReviewerModelTier = "flagship"
	cfg.LLMRuntimes["home-llm"] = llm

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
			llm := cfg.LLMRuntimes["home-llm"]
			llm.ModelMap = tt.modelMap
			cfg.LLMRuntimes["home-llm"] = llm
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

	_, _, err := ResolveProfile(cfg, "")
	if !errors.Is(err, ErrProfileNotFound) || !strings.Contains(err.Error(), "pass --profile or configure a repository route") {
		t.Fatalf("ResolveProfile empty error = %v, want actionable ErrProfileNotFound", err)
	}

	name, profile, err := ResolveProfile(cfg, "work")
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
			name:        "repo case-sensitive falls back to namespace route",
			target:      RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "Baz"},
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

	_, _, err = ResolveProfileForRepository(loaded, "", false, RepositoryTarget{Host: "github.com", Namespace: "open-cli-collective", Repo: "codereview-cli"})
	if !errors.Is(err, ErrProfileNotFound) || !strings.Contains(err.Error(), "no repository profile route matched github.com/open-cli-collective/codereview-cli") {
		t.Fatalf("ResolveProfileForRepository unmatched error = %v, want actionable ErrProfileNotFound", err)
	}

	_, _, err = ResolveProfileForRepository(loaded, "", false, RepositoryTarget{Host: "github.com", Namespace: "Rianjs", Repo: "baz"})
	if !errors.Is(err, ErrProfileNotFound) || !strings.Contains(err.Error(), "no repository profile route matched github.com/Rianjs/baz") {
		t.Fatalf("ResolveProfileForRepository case-sensitive unmatched error = %v, want actionable ErrProfileNotFound", err)
	}
}

func TestValidateAcceptsOverlappingRepositoryProfilesAcrossProfiles(t *testing.T) {
	cfg := validFile()
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
			},
		},
		{
			Profile: "home",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
		{
			Profile: "work",
			Match: RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate overlapping repository profiles: %v", err)
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

	_, err := ResolveProfileForRepositoryWithSource(cfg, "", true, RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"})
	if !errors.Is(err, ErrProfileNotFound) || !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("ResolveProfileForRepositoryWithSource explicit empty error = %v, want actionable ErrProfileNotFound", err)
	}

	_, err = ResolveProfileForRepositoryWithSource(cfg, "", false, RepositoryTarget{Host: "github.com", Namespace: "open-cli-collective", Repo: "codereview-cli"})
	if !errors.Is(err, ErrProfileNotFound) || !strings.Contains(err.Error(), "no repository profile route matched github.com/open-cli-collective/codereview-cli") {
		t.Fatalf("ResolveProfileForRepositoryWithSource unmatched error = %v, want actionable ErrProfileNotFound", err)
	}
}

func TestResolveProfileForRepositoryRejectsAmbiguousRoutes(t *testing.T) {
	tests := []struct {
		name   string
		routes []RepositoryProfile
		target RepositoryTarget
	}{
		{
			name: "repo route",
			routes: []RepositoryProfile{
				{Profile: "work", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"baz"}}},
				{Profile: "home", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs", Repos: []string{"baz"}}},
			},
			target: RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"},
		},
		{
			name: "namespace route",
			routes: []RepositoryProfile{
				{Profile: "work", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs"}},
				{Profile: "home", Match: RepositoryProfileMatch{Host: "github.com", Namespace: "rianjs"}},
			},
			target: RepositoryTarget{Host: "github.com", Namespace: "rianjs", Repo: "baz"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			cfg.RepositoryProfiles = tt.routes
			_, _, err := ResolveProfileForRepository(cfg, "", false, tt.target)
			if !errors.Is(err, ErrRepositoryProfileAmbiguous) {
				t.Fatalf("ResolveProfileForRepository error = %v, want ErrRepositoryProfileAmbiguous", err)
			}
			var ambiguity RepositoryProfileAmbiguityError
			if !errors.As(err, &ambiguity) {
				t.Fatalf("ResolveProfileForRepository error = %T, want RepositoryProfileAmbiguityError", err)
			}
			if got, want := ambiguity.Profiles, []string{"home", "work"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("ambiguity profiles = %#v, want %#v", got, want)
			}
			if !strings.Contains(err.Error(), "pass --profile with one of: home, work") {
				t.Fatalf("ambiguity error = %q, want profile suggestions", err)
			}

			name, _, err := ResolveProfileForRepository(cfg, "work", true, tt.target)
			if err != nil {
				t.Fatalf("ResolveProfileForRepository explicit profile: %v", err)
			}
			if name != "work" {
				t.Fatalf("explicit profile = %q, want work", name)
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
	body, err := os.ReadFile(path) // #nosec G304 -- test reads the temp config file it just saved.
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
		{Purpose: "git", Store: LocalOSCredentialStoreID, Ref: "codereview/work", Mode: "pat"},
		{Purpose: "reviewer_credentials", Store: LocalOSCredentialStoreID, Ref: "codereview/work-reviewer", Mode: "pat"},
		{Purpose: "llm", Store: LocalOSCredentialStoreID, Ref: "codereview/work-llm", Mode: "api_key", Provider: "anthropic"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("CredentialRefs = %#v, want %#v", refs, want)
	}
}

func TestCredentialRefsIncludesGitHubAppModes(t *testing.T) {
	profile := validFile().normalized().Profiles["work"]
	profile.Git.AuthMode = GitAuthModeGitHubApp
	profile.Git.GitHubApp = &GitHubAppConfig{AppID: "12345"}
	profile.ReviewerCredentials.AuthMode = GitAuthModeGitHubApp
	profile.ReviewerCredentials.GitHubApp = &GitHubAppConfig{AppID: "12345"}

	refs, err := CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	want := []CredentialRef{
		{Purpose: "git", Store: LocalOSCredentialStoreID, Ref: "codereview/work", Mode: "github_app"},
		{Purpose: "reviewer_credentials", Store: LocalOSCredentialStoreID, Ref: "codereview/work-reviewer", Mode: "github_app"},
		{Purpose: "llm", Store: LocalOSCredentialStoreID, Ref: "codereview/work-llm", Mode: "api_key", Provider: "anthropic"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("CredentialRefs = %#v, want %#v", refs, want)
	}
}

func TestPinnedGitHubAppInstallationIDForGit(t *testing.T) {
	profile := validFile().normalized().Profiles["work"]
	profile.ReviewerCredentials = &ReviewerCredentials{
		AuthMode:      GitAuthModeGitHubApp,
		Credential:    CredentialLocation{Store: LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
		CredentialRef: "codereview/work-reviewer",
	}
	profile.Reviewer = ProfileReviewer{
		Kind:   ProfileReviewerKindEntity,
		Entity: "work-reviewer",
		GitHubAppInstallation: &ProfileReviewerGitHubAppInstallation{
			Mode:           ProfileReviewerGitHubAppInstallationPinned,
			InstallationID: "123456",
		},
	}
	git := GitConfig{
		Host:          "github.com",
		AuthMode:      GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	if got := PinnedGitHubAppInstallationIDForGit(profile, git); got != "123456" {
		t.Fatalf("PinnedGitHubAppInstallationIDForGit = %q, want 123456", got)
	}

	profile.Reviewer.GitHubAppInstallation.Mode = ProfileReviewerGitHubAppInstallationDiscoverFromRepository
	profile.Reviewer.GitHubAppInstallation.InstallationID = ""
	if got := PinnedGitHubAppInstallationIDForGit(profile, git); got != "" {
		t.Fatalf("discover installation id = %q, want empty", got)
	}

	git.CredentialRef = "codereview/work"
	if got := PinnedGitHubAppInstallationIDForGit(profile, git); got != "" {
		t.Fatalf("unmatched credential installation id = %q, want empty", got)
	}
}

func TestValidateAllowsCredentialStoresWithoutReviewProfiles(t *testing.T) {
	cfg := File{
		Secrets: SecretsConfig{
			Stores: map[string]SecretsStore{
				"personal-file": {
					DisplayName: "Personal file",
					Backend: SecretsStoreBackend{
						Kind: SecretsBackendKind(credstore.BackendFile),
					},
				},
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate store-only config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save store-only config: %v", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	legacyProfileKey := "default" + "_profile"
	if strings.Contains(string(body), legacyProfileKey) || strings.Contains(string(body), "profiles:") {
		t.Fatalf("saved store-only config contains review-profile fields:\n%s", string(body))
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load store-only config: %v", err)
	}
	if len(loaded.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", loaded.Profiles)
	}
	if _, ok := loaded.Secrets.Stores["personal-file"]; !ok {
		t.Fatalf("loaded stores = %#v, want personal-file", loaded.Secrets.Stores)
	}
}

func TestValidateAllowsEmptyConfig(t *testing.T) {
	cfg := File{}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate empty config: %v", err)
	}
	if err := Save(filepath.Join(t.TempDir(), "config.yml"), cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Save literal empty config error = %v, want ErrInvalid", err)
	}

	cfg = File{Profiles: map[string]Profile{}}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save explicit profileless config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load explicit profileless config: %v", err)
	}
	if len(loaded.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", loaded.Profiles)
	}
	if len(loaded.Secrets.Stores) != 0 {
		t.Fatalf("loaded stores = %#v, want none", loaded.Secrets.Stores)
	}
}

func TestValidateAllowsLLMRuntimesWithoutReviewProfiles(t *testing.T) {
	cfg := File{
		LLMRuntimes: map[string]LLMConfig{
			"codex-cli": {
				Provider: LLMProviderOpenAI,
				Auth:     LLMAuthSubscription,
				Adapter:  LLMAdapterCodexCLI,
			},
			"openai-api-key": {
				Provider: LLMProviderOpenAI,
				Auth:     LLMAuthAPIKey,
				Adapter:  LLMAdapterOpenAIAPI,
				Credential: CredentialLocation{
					Store: LocalOSCredentialStoreID,
					Name:  "codereview/openai-api-key",
				},
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate runtime-only config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save runtime-only config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load runtime-only config: %v", err)
	}
	if len(loaded.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", loaded.Profiles)
	}
	if loaded.LLMRuntimes["codex-cli"].Adapter != LLMAdapterCodexCLI {
		t.Fatalf("loaded runtimes = %#v, want codex-cli", loaded.LLMRuntimes)
	}
	if got := loaded.LLMRuntimes["openai-api-key"].Credential.Name; got != "codereview/openai-api-key" {
		t.Fatalf("openai runtime credential name = %q, want codereview/openai-api-key", got)
	}
}

func TestValidateAllowsRepositoryAccessWithoutReviewProfiles(t *testing.T) {
	cfg := File{
		RepositoryAccess: map[string]RepositoryAccessConfig{
			"personal-github": {
				DisplayName: "Personal GitHub",
				Git: GitConfig{
					Host:     " GitHub.com ",
					AuthMode: GitAuthModePAT,
					Credential: CredentialLocation{
						Store: LocalOSCredentialStoreID,
						Name:  "codereview/personal-github",
					},
				},
			},
			"work-app": {
				Git: GitConfig{
					Host:     "github.example.com",
					AuthMode: GitAuthModeGitHubApp,
					Credential: CredentialLocation{
						Store: LocalOSCredentialStoreID,
						Name:  "codereview/work-github-app",
					},
					GitHubApp: &GitHubAppConfig{AppID: "12345"},
				},
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate repository-access-only config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save repository-access-only config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load repository-access-only config: %v", err)
	}
	if len(loaded.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", loaded.Profiles)
	}
	if got := loaded.RepositoryAccess["personal-github"].Git.Host; got != "github.com" {
		t.Fatalf("repository access host = %q, want github.com", got)
	}
	if got := loaded.RepositoryAccess["work-app"].Git.GitHubApp.AppID; got != "12345" {
		t.Fatalf("repository access app id = %q, want 12345", got)
	}
}

func TestValidateRepositoryAccess(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr error
		wantMsg string
	}{
		{
			name: "blank name",
			mutate: func(cfg *File) {
				cfg.RepositoryAccess[" "] = cfg.RepositoryAccess["work"]
				delete(cfg.RepositoryAccess, "work")
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access name is required",
		},
		{
			name: "missing host",
			mutate: func(cfg *File) {
				access := cfg.RepositoryAccess["work"]
				access.Git.Host = ""
				cfg.RepositoryAccess["work"] = access
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.work.git.host is required",
		},
		{
			name: "unknown store",
			mutate: func(cfg *File) {
				access := cfg.RepositoryAccess["work"]
				access.Git.Credential.Store = "missing"
				cfg.RepositoryAccess["work"] = access
			},
			wantErr: ErrSecretsStoreNotFound,
			wantMsg: `repository_access.work.git.credential.store "missing"`,
		},
		{
			name: "github app requires app id",
			mutate: func(cfg *File) {
				access := cfg.RepositoryAccess["work"]
				access.Git.AuthMode = GitAuthModeGitHubApp
				access.Git.GitHubApp = nil
				cfg.RepositoryAccess["work"] = access
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.work.git.github_app.app_id is required",
		},
		{
			name: "pat rejects github app config",
			mutate: func(cfg *File) {
				access := cfg.RepositoryAccess["work"]
				access.Git.GitHubApp = &GitHubAppConfig{AppID: "12345"}
				cfg.RepositoryAccess["work"] = access
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.work.git.github_app must be empty unless auth_mode is github_app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := File{
				RepositoryAccess: map[string]RepositoryAccessConfig{
					"work": {
						Git: GitConfig{
							Host:     "github.com",
							AuthMode: GitAuthModePAT,
							Credential: CredentialLocation{
								Store: LocalOSCredentialStoreID,
								Name:  "codereview/work",
							},
						},
					},
				},
			}
			tt.mutate(&cfg)
			err := Validate(cfg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Validate error = %q, want containing %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestValidateSecretsStores(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr error
		wantMsg string
	}{
		{
			name: "valid configured credential store",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"personal": {
							DisplayName: "Personal Keychain",
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendKeychain),
							},
						},
					},
				}
			},
		},
		{
			name: "invalid secrets backend kind",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"broken": {
							Backend: SecretsStoreBackend{Kind: "bogus"},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: `secrets.stores.broken.backend.kind "bogus" is invalid`,
		},
		{
			name: "valid 1password service account store defaults token env and timeout",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"work-op": {
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendOP),
								OnePassword: &SecretsStoreOnePasswordConfig{
									VaultID: "vault-123",
								},
							},
						},
					},
				}
			},
		},
		{
			name: "valid 1password connect store requires host and defaults token env",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"work-connect": {
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendOPConnect),
								OnePassword: &SecretsStoreOnePasswordConfig{
									VaultID:     "vault-123",
									ConnectHost: "https://connect.example",
								},
							},
						},
					},
				}
			},
		},
		{
			name: "valid 1password desktop store permits env fallback account id",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"work-desktop": {
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendOPDesktop),
								OnePassword: &SecretsStoreOnePasswordConfig{
									VaultID: "vault-123",
								},
							},
						},
					},
				}
			},
		},
		{
			name: "1password service account missing vault id invalid",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"broken": {
							Backend: SecretsStoreBackend{
								Kind:        SecretsBackendKind(credstore.BackendOP),
								OnePassword: &SecretsStoreOnePasswordConfig{},
							},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: "secrets.stores.broken.backend.onepassword.vault_id is required",
		},
		{
			name: "1password connect missing host invalid",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"broken": {
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendOPConnect),
								OnePassword: &SecretsStoreOnePasswordConfig{
									VaultID: "vault-123",
								},
							},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: "secrets.stores.broken.backend.onepassword.connect_host is required",
		},
		{
			name: "1password timeout must parse",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"broken": {
							Backend: SecretsStoreBackend{
								Kind: SecretsBackendKind(credstore.BackendOP),
								OnePassword: &SecretsStoreOnePasswordConfig{
									VaultID: "vault-123",
									Timeout: "later",
								},
							},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: `secrets.stores.broken.backend.onepassword.timeout "later" is invalid`,
		},
		{
			name: "multiline display name invalid",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						"broken": {
							DisplayName: "line one\nline two",
							Backend:     SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendMemory)},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: "secrets.stores.broken.display_name must be a single line",
		},
		{
			name: "reserved built-in store id is rejected",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						LocalOSCredentialStoreID: {
							Backend: SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendMemory)},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: `secrets.stores.local-os is reserved`,
		},
		{
			name: "surrounding whitespace in id is rejected",
			mutate: func(cfg *File) {
				cfg.Secrets = SecretsConfig{
					Stores: map[string]SecretsStore{
						" work ": {
							Backend: SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendMemory)},
						},
					},
				}
			},
			wantErr: ErrInvalid,
			wantMsg: `secrets.stores. work  id must not contain surrounding whitespace`,
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

func TestValidateCredentialLocations(t *testing.T) {
	setProfileRepositoryAccessCredential := func(cfg *File, profileName string, location CredentialLocation) {
		profile := cfg.Profiles[profileName]
		access := cfg.RepositoryAccess[profile.RepositoryAccess]
		access.Git.Credential = location
		access.Git.CredentialRef = ""
		cfg.RepositoryAccess[profile.RepositoryAccess] = access
	}

	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr error
		wantMsg string
	}{
		{
			name: "configured store selection",
			mutate: func(cfg *File) {
				cfg.Secrets.Stores["work-file"] = SecretsStore{
					DisplayName: "Work File",
					Backend:     SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendFile)},
				}
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Store: "work-file", Name: "codereview/home"})
			},
		},
		{
			name: "missing store",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Name: "codereview/home"})
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.home-git.git.credential.store is required",
		},
		{
			name: "missing name",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Store: LocalOSCredentialStoreID})
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.home-git.git.credential.name is required",
		},
		{
			name: "unknown configured store",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Store: "missing", Name: "codereview/home"})
			},
			wantErr: ErrSecretsStoreNotFound,
			wantMsg: `repository_access.home-git.git.credential.store "missing"`,
		},
		{
			name: "credential name invalid grammar",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Store: LocalOSCredentialStoreID, Name: "codereview/bad.profile"})
			},
			wantErr: ErrInvalid,
			wantMsg: "repository_access.home-git.git.credential.name is invalid",
		},
		{
			name: "credential name wrong service",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "home", CredentialLocation{Store: LocalOSCredentialStoreID, Name: "other/home"})
			},
			wantErr: ErrInvalid,
			wantMsg: `repository_access.home-git.git.credential.name must use service "codereview"`,
		},
		{
			name: "same credential name in different stores",
			mutate: func(cfg *File) {
				cfg.Secrets.Stores["work-file"] = SecretsStore{
					DisplayName: "Work File",
					Backend:     SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendFile)},
				}
				setProfileRepositoryAccessCredential(cfg, "work", CredentialLocation{Store: LocalOSCredentialStoreID, Name: "codereview/shared"})
				entity := cfg.ReviewerEntities["work-reviewer"]
				entity.Credential = CredentialLocation{Store: "work-file", Name: "codereview/shared"}
				entity.CredentialRef = ""
				cfg.ReviewerEntities["work-reviewer"] = entity
			},
		},
		{
			name: "same credential name in same store",
			mutate: func(cfg *File) {
				setProfileRepositoryAccessCredential(cfg, "work", CredentialLocation{Store: LocalOSCredentialStoreID, Name: "codereview/shared"})
				entity := cfg.ReviewerEntities["work-reviewer"]
				entity.Credential = CredentialLocation{Store: LocalOSCredentialStoreID, Name: "codereview/shared"}
				entity.CredentialRef = ""
				cfg.ReviewerEntities["work-reviewer"] = entity
			},
			wantErr: ErrInvalid,
			wantMsg: "reviewer.entity \"work-reviewer\" credential must differ from git.credential",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile().normalized()
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

func TestSecretsStoreBackendNormalizedOnePasswordDefaults(t *testing.T) {
	service := SecretsStoreBackend{
		Kind: SecretsBackendKind(credstore.BackendOP),
		OnePassword: &SecretsStoreOnePasswordConfig{
			VaultID: "vault-123",
		},
	}.normalized()
	if service.OnePassword == nil {
		t.Fatal("service OnePassword = nil, want defaults")
	}
	if service.OnePassword.Timeout != defaultOnePasswordTimeout {
		t.Fatalf("service timeout = %q, want %q", service.OnePassword.Timeout, defaultOnePasswordTimeout)
	}
	if service.OnePassword.ServiceTokenEnv != credstore.DefaultOnePasswordServiceTokenEnv {
		t.Fatalf("service token env = %q, want %q", service.OnePassword.ServiceTokenEnv, credstore.DefaultOnePasswordServiceTokenEnv)
	}

	connect := SecretsStoreBackend{
		Kind: SecretsBackendKind(credstore.BackendOPConnect),
		OnePassword: &SecretsStoreOnePasswordConfig{
			VaultID:     "vault-123",
			ConnectHost: "https://connect.example",
		},
	}.normalized()
	if connect.OnePassword == nil {
		t.Fatal("connect OnePassword = nil, want defaults")
	}
	if connect.OnePassword.ConnectTokenEnv != credstore.DefaultOnePasswordConnectTokenEnv {
		t.Fatalf("connect token env = %q, want %q", connect.OnePassword.ConnectTokenEnv, credstore.DefaultOnePasswordConnectTokenEnv)
	}

	desktop := SecretsStoreBackend{
		Kind: SecretsBackendKind(credstore.BackendOPDesktop),
		OnePassword: &SecretsStoreOnePasswordConfig{
			VaultID: "vault-123",
		},
	}.normalized()
	if desktop.OnePassword == nil {
		t.Fatal("desktop OnePassword = nil, want defaults")
	}
	if desktop.OnePassword.Timeout != defaultOnePasswordTimeout {
		t.Fatalf("desktop timeout = %q, want %q", desktop.OnePassword.Timeout, defaultOnePasswordTimeout)
	}
}

func TestEffectiveSecretsStores(t *testing.T) {
	t.Run("built-in plus configured stores", func(t *testing.T) {
		cfg := validFile()
		cfg.Secrets = SecretsConfig{
			Stores: map[string]SecretsStore{
				"personal": {
					DisplayName: "Personal Keychain",
					Backend:     SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendKeychain)},
				},
				"work-vault": {
					DisplayName: "Work File Store",
					Backend:     SecretsStoreBackend{Kind: SecretsBackendKind(credstore.BackendFile)},
				},
			},
		}

		got := EffectiveSecretsStores(cfg)
		want := []EffectiveSecretsStore{
			{
				ID:          LocalOSCredentialStoreID,
				DisplayName: "OS credential store",
				Backend:     ProjectedOSCredentialStoreBackendKind,
				ReadOnly:    true,
				Source:      EffectiveSecretsStoreSourceBuiltIn,
			},
			{
				ID:          "personal",
				DisplayName: "Personal Keychain",
				Backend:     string(credstore.BackendKeychain),
				Source:      EffectiveSecretsStoreSourceConfigured,
			},
			{
				ID:          "work-vault",
				DisplayName: "Work File Store",
				Backend:     string(credstore.BackendFile),
				Source:      EffectiveSecretsStoreSourceConfigured,
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EffectiveSecretsStores = %#v, want %#v", got, want)
		}
	})

	t.Run("virgin config projects built-in store", func(t *testing.T) {
		cfg := validFile()

		got := EffectiveSecretsStores(cfg)
		want := []EffectiveSecretsStore{{
			ID:          LocalOSCredentialStoreID,
			DisplayName: "OS credential store",
			Backend:     ProjectedOSCredentialStoreBackendKind,
			ReadOnly:    true,
			Source:      EffectiveSecretsStoreSourceBuiltIn,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EffectiveSecretsStores = %#v, want %#v", got, want)
		}
	})
}

func TestLoadRejectsOldCredentialStoreSchema(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "keyring backend", body: `keyring:
  backend: memory
profiles: {}
`},
		{name: "secrets legacy selection", body: `secrets: work
profiles: {}
`},
		{name: "secrets profiles", body: `secrets:
  profiles:
    work:
      backend:
        kind: memory
profiles: {}
`},
		{name: "profile secrets profile", body: `profiles:
  home:
    secrets_profile: work
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`},
		{name: "credential ref", body: `profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
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
			if _, err := Load(path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
		})
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
			entity := cfg.ReviewerEntities["work-reviewer"]
			entity.AuthMode = GitAuthModeOAuthDevice
			cfg.ReviewerEntities["work-reviewer"] = entity
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
			profile.Git.GitHubApp = &GitHubAppConfig{AppID: "12345"}
			cfg.Profiles["home"] = profile
		}},
		{name: "reviewer github_app", mutate: func(cfg *File) {
			entity := cfg.ReviewerEntities["work-reviewer"]
			entity.AuthMode = GitAuthModeGitHubApp
			entity.GitHubApp = &GitHubAppConfig{AppID: "12345"}
			cfg.ReviewerEntities["work-reviewer"] = entity
			profile := cfg.Profiles["work"]
			profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
				Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
			}
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

func TestValidateProfileReviewerGitHubAppInstallation(t *testing.T) {
	githubAppReviewer := func(cfg *File) {
		entity := cfg.ReviewerEntities["work-reviewer"]
		entity.AuthMode = GitAuthModeGitHubApp
		entity.GitHubApp = &GitHubAppConfig{AppID: "12345"}
		cfg.ReviewerEntities["work-reviewer"] = entity
	}
	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr error
		wantMsg string
	}{
		{
			name: "discover from repository",
			mutate: func(cfg *File) {
				githubAppReviewer(cfg)
				profile := cfg.Profiles["work"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
				}
				cfg.Profiles["work"] = profile
			},
		},
		{
			name: "pinned installation",
			mutate: func(cfg *File) {
				githubAppReviewer(cfg)
				profile := cfg.Profiles["work"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode:           ProfileReviewerGitHubAppInstallationPinned,
					InstallationID: "123456",
				}
				cfg.Profiles["work"] = profile
			},
		},
		{
			name: "missing for github app reviewer",
			mutate: func(cfg *File) {
				githubAppReviewer(cfg)
			},
			wantErr: ErrInvalid,
			wantMsg: "profiles.work.reviewer.github_app_installation.mode is required",
		},
		{
			name: "pinned without id",
			mutate: func(cfg *File) {
				githubAppReviewer(cfg)
				profile := cfg.Profiles["work"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode: ProfileReviewerGitHubAppInstallationPinned,
				}
				cfg.Profiles["work"] = profile
			},
			wantErr: ErrInvalid,
			wantMsg: "installation_id is required",
		},
		{
			name: "discover with id",
			mutate: func(cfg *File) {
				githubAppReviewer(cfg)
				profile := cfg.Profiles["work"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode:           ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
					InstallationID: "123456",
				}
				cfg.Profiles["work"] = profile
			},
			wantErr: ErrInvalid,
			wantMsg: "installation_id must be empty",
		},
		{
			name: "pat reviewer with installation config",
			mutate: func(cfg *File) {
				profile := cfg.Profiles["work"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
				}
				cfg.Profiles["work"] = profile
			},
			wantErr: ErrInvalid,
			wantMsg: "must be empty unless reviewer.entity uses github_app auth",
		},
		{
			name: "git identity with installation config",
			mutate: func(cfg *File) {
				profile := cfg.Profiles["home"]
				profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
					Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
				}
				cfg.Profiles["home"] = profile
			},
			wantErr: ErrInvalid,
			wantMsg: "must be empty for git_identity reviewer",
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
			entity := cfg.ReviewerEntities["work-reviewer"]
			entity.AuthMode = GitAuthModeOAuthDevice
			cfg.ReviewerEntities["work-reviewer"] = entity
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
		{name: "git oauth_device", body: `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
profiles:
  home:
    git:
      host: github.com
      auth_mode: oauth_device
      credential:
        store: local-os
        name: codereview/home
    reviewer:
      kind: git_identity
    llm_runtime: claude-local
`},
		{name: "reviewer oauth_device", body: `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
reviewer_entities:
  work-reviewer:
    host: github.com
    auth_mode: oauth_device
    credential:
      store: local-os
      name: codereview/work-reviewer
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/work
    reviewer:
      kind: entity
      entity: work-reviewer
    llm_runtime: claude-local
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
	writeFile(t, path, `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
reviewer_entities:
  work-reviewer:
    host: github.com
    auth_mode: github_app
    credential:
      store: local-os
      name: codereview/work-reviewer
    github_app:
      app_id: "12345"
profiles:
  work:
    git:
      host: github.com
      auth_mode: github_app
      credential:
        store: local-os
        name: codereview/work
      github_app:
        app_id: "12345"
    reviewer:
      kind: entity
      entity: work-reviewer
      github_app_installation:
        mode: discover_from_repository
    llm_runtime: claude-local
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
	writeFile(t, path, `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
reviewer_entities:
  work-reviewer:
    host: github.com
    auth_mode: github_app
    credential:
      store: local-os
      name: codereview/work-reviewer
    github_app:
      app_id: "12345"
    display_name: |
      line one
      line two
profiles:
  work:
    git:
      host: github.com
      auth_mode: github_app
      credential:
        store: local-os
        name: codereview/work
      github_app:
        app_id: "12345"
    reviewer:
      kind: entity
      entity: work-reviewer
    llm_runtime: claude-local
`)
	_, err := Load(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "reviewer_entities.work-reviewer.display_name") {
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
	want := []CredentialRef{{Purpose: "git", Store: LocalOSCredentialStoreID, Ref: "codereview/home", Mode: "pat"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("CredentialRefs = %#v, want %#v", refs, want)
	}
}

func TestAPIKeyLLMRequiresCredentialRef(t *testing.T) {
	cfg := validFile()
	llm := cfg.LLMRuntimes["work-llm"]
	llm.CredentialRef = ""
	llm.Credential = CredentialLocation{}
	cfg.LLMRuntimes["work-llm"] = llm

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
			llm := cfg.LLMRuntimes["home-llm"]
			llm.CredentialRef = "codereview/home-llm"
			cfg.LLMRuntimes["home-llm"] = llm
		}},
		{name: "empty reviewer credential ref", mutate: func(cfg *File) {
			entity := cfg.ReviewerEntities["work-reviewer"]
			entity.CredentialRef = ""
			entity.Credential = CredentialLocation{}
			cfg.ReviewerEntities["work-reviewer"] = entity
		}},
		{name: "reviewer credential ref matches git credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			cfg.Profiles["work"] = profile
			entity := cfg.ReviewerEntities["work-reviewer"]
			entity.CredentialRef = profile.Git.CredentialRef
			cfg.ReviewerEntities["work-reviewer"] = entity
		}},
		{name: "llm credential ref matches git credential ref", mutate: func(cfg *File) {
			profile := cfg.Profiles["work"]
			llm := cfg.LLMRuntimes["work-llm"]
			llm.CredentialRef = profile.Git.CredentialRef
			cfg.LLMRuntimes["work-llm"] = llm
		}},
		{name: "llm credential ref matches reviewer credential ref", mutate: func(cfg *File) {
			entity := cfg.ReviewerEntities["work-reviewer"]
			llm := cfg.LLMRuntimes["work-llm"]
			llm.CredentialRef = entity.CredentialRef
			cfg.LLMRuntimes["work-llm"] = llm
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
			llm := cfg.LLMRuntimes["work-llm"]
			llm.CredentialRef = "codereview/work.llm"
			cfg.LLMRuntimes["work-llm"] = llm
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
	cfg.Data.Retention.MaxAgeDays = intPtr(-1)
	if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad retention Validate error = %v, want ErrInvalid", err)
	}
}

func TestRetentionMaxAgeDefaultAndExplicitZero(t *testing.T) {
	t.Run("omitted defaults to 90", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		writeFile(t, path, `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/home
    reviewer:
      kind: git_identity
    llm_runtime: claude-local
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
		writeFile(t, path, `llm_runtimes:
  claude-local:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/home
    reviewer:
      kind: git_identity
    llm_runtime: claude-local
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

func TestValidateHooks(t *testing.T) {
	for event := range hookEvents {
		cfg := validFile()
		profile := cfg.Profiles["home"]
		profile.Hooks = []Hook{{Event: event, Argv: []string{"notify"}}}
		cfg.Profiles["home"] = profile
		if err := Validate(cfg); err != nil {
			t.Fatalf("Validate event %q: %v", event, err)
		}
	}

	tests := []struct {
		name string
		hook Hook
	}{
		{name: "unknown event", hook: Hook{Event: "benchmark.run.started", Argv: []string{"notify"}}},
		{name: "empty argv", hook: Hook{Event: "run.started"}},
		{name: "blank command", hook: Hook{Event: "run.started", Argv: []string{" "}}},
		{name: "invalid timeout", hook: Hook{Event: "run.started", Argv: []string{"notify"}, Timeout: "later"}},
		{name: "zero timeout", hook: Hook{Event: "run.started", Argv: []string{"notify"}, Timeout: "0s"}},
		{name: "negative timeout", hook: Hook{Event: "run.started", Argv: []string{"notify"}, Timeout: "-1s"}},
		{name: "NUL command", hook: Hook{Event: "run.started", Argv: []string{"notify\x00bad"}}},
		{name: "NUL argument", hook: Hook{Event: "run.started", Argv: []string{"notify", "bad\x00arg"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFile()
			profile := cfg.Profiles["home"]
			profile.Hooks = []Hook{tt.hook}
			cfg.Profiles["home"] = profile
			if err := Validate(cfg); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
		})
	}
}

func validFile() File {
	return File{
		LLMRuntimes: map[string]LLMConfig{
			"home-llm": {
				Provider: LLMProviderAnthropic,
				Auth:     LLMAuthSubscription,
				Adapter:  LLMAdapterClaudeCLI,
			},
			"work-llm": {
				Provider:      LLMProviderAnthropic,
				Auth:          LLMAuthAPIKey,
				Adapter:       LLMAdapterAnthropicAPI,
				CredentialRef: "codereview/work-llm",
			},
		},
		ReviewerEntities: map[string]ReviewerEntity{
			"work-reviewer": {
				Host:          "github.com",
				AuthMode:      GitAuthModePAT,
				CredentialRef: "codereview/work-reviewer",
				DisplayName:   "Work reviewer bot",
				IdentityCache: "acme-review-bot",
			},
		},
		Profiles: map[string]Profile{
			"home": {
				Git: GitConfig{
					Host:          "github.com",
					AuthMode:      GitAuthModePAT,
					CredentialRef: "codereview/home",
					IdentityCache: "rianjs",
				},
				Reviewer:     ProfileReviewer{Kind: ProfileReviewerKindGitIdentity},
				LLMRuntime:   "home-llm",
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
				Reviewer:     ProfileReviewer{Kind: ProfileReviewerKindEntity, Entity: "work-reviewer"},
				LLMRuntime:   "work-llm",
				AgentSources: []string{"~/dev/work-reviewers"},
				ReviewPolicy: ReviewPolicy{
					MajorEvent:       ReviewMajorEventRequestChanges,
					AllowSelfApprove: true,
					ResolveThreads:   ResolveThreadsNever,
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
