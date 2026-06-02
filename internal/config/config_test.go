package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
		{Purpose: "llm", Ref: "codereview/work-llm", Mode: "api_key"},
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
