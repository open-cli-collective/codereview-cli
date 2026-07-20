package config

import (
	"errors"
	"strings"
	"testing"
)

func TestGitProviderKindValid(t *testing.T) {
	tests := []struct {
		kind GitProviderKind
		want bool
	}{
		{kind: "", want: true},
		{kind: GitProviderGitHub, want: true},
		{kind: GitProviderGitLab, want: true},
		{kind: "bitbucket", want: false},
		{kind: "GitHub", want: false},
	}
	for _, tt := range tests {
		if got := tt.kind.Valid(); got != tt.want {
			t.Errorf("GitProviderKind(%q).Valid() = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

func TestGitConfigProviderKindDefaultsToGitHub(t *testing.T) {
	if got := (GitConfig{}).ProviderKind(); got != GitProviderGitHub {
		t.Fatalf("ProviderKind() = %q, want %q", got, GitProviderGitHub)
	}
	if got := (GitConfig{Provider: GitProviderGitLab}).ProviderKind(); got != GitProviderGitLab {
		t.Fatalf("ProviderKind() = %q, want %q", got, GitProviderGitLab)
	}
}

func TestValidateAcceptsGitLabProviderWithPAT(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.Git.Provider = GitProviderGitLab
	profile.Git.Host = "gitlab.example.com"
	cfg.Profiles["home"] = profile
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}
}

func TestValidateRejectsUnknownGitProvider(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.Git.Provider = "bitbucket"
	cfg.Profiles["home"] = profile
	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Fatalf("Validate error = %v, want provider mention", err)
	}
}

func TestValidateRejectsGitLabProviderWithGitHubAppAuth(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.Git.Provider = GitProviderGitLab
	profile.Git.AuthMode = GitAuthModeGitHubApp
	profile.Git.GitHubApp = &GitHubAppConfig{AppID: "12345"}
	cfg.Profiles["home"] = profile
	if err := Validate(cfg); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Validate error = %v, want ErrUnsupported", err)
	}
}

func TestNormalizeLowercasesGitProvider(t *testing.T) {
	cfg := validFile()
	profile := cfg.Profiles["home"]
	profile.Git.Provider = " GitLab "
	cfg.Profiles["home"] = profile
	normalized := Normalize(cfg)
	if got := normalized.Profiles["home"].Git.Provider; got != GitProviderGitLab {
		t.Fatalf("normalized provider = %q, want %q", got, GitProviderGitLab)
	}
}

func TestValidateRejectsGitLabProviderWithGitHubAppReviewerEntity(t *testing.T) {
	cfg := validFile()
	entity := cfg.ReviewerEntities["work-reviewer"]
	entity.AuthMode = GitAuthModeGitHubApp
	entity.GitHubApp = &GitHubAppConfig{AppID: "12345"}
	cfg.ReviewerEntities["work-reviewer"] = entity
	profile := cfg.Profiles["work"]
	profile.Git.Provider = GitProviderGitLab
	profile.Reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
		Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
	}
	cfg.Profiles["work"] = profile
	if err := Validate(cfg); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Validate error = %v, want ErrUnsupported", err)
	}
}
