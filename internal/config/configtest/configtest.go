// Package configtest provides shared configuration fixtures.
package configtest

import (
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// Option customizes File.
type Option func(*config.File)

// File returns the common command-test configuration with opts applied.
func File(opts ...Option) config.File {
	cfg := config.File{
		Secrets: config.SecretsConfig{Stores: map[string]config.SecretsStore{
			"test-memory": {
				DisplayName: "Test Memory Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)},
			},
		}},
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		}},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:       "github.com",
					AuthMode:   config.GitAuthModePAT,
					Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/home"},
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventRequestChanges},
			},
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithoutSecrets clears configured secret stores.
func WithoutSecrets() Option {
	return func(cfg *config.File) { cfg.Secrets = config.SecretsConfig{} }
}

// WithoutRepositoryProfiles clears repository routing.
func WithoutRepositoryProfiles() Option {
	return func(cfg *config.File) { cfg.RepositoryProfiles = nil }
}

// RepositoryProfiles replaces repository routing.
func RepositoryProfiles(profiles ...config.RepositoryProfile) Option {
	return func(cfg *config.File) { cfg.RepositoryProfiles = profiles }
}

// HomeProfile replaces the home profile.
func HomeProfile(profile config.Profile) Option {
	return func(cfg *config.File) { cfg.Profiles["home"] = profile }
}

// Profile adds or replaces a named profile.
func Profile(name string, profile config.Profile) Option {
	return func(cfg *config.File) { cfg.Profiles[name] = profile }
}

// Data replaces data lifecycle configuration.
func Data(data config.DataConfig) Option {
	return func(cfg *config.File) { cfg.Data = data }
}
