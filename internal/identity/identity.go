// Package identity resolves live git-host identities and refreshes config caches.
package identity

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const (
	// SourceGit means the profile's git credentials provide the posting identity.
	SourceGit = "git"
	// SourceReviewer means reviewer_credentials provide the posting identity.
	SourceReviewer = "reviewer_credentials"
)

// CredentialSource identifies which configured credential was used.
type CredentialSource string

// Resolver resolves one live git-host identity.
type Resolver interface {
	ResolveIdentity(ctx context.Context, git config.GitConfig) (gitprovider.Identity, error)
}

// ProfileResult describes one refreshed profile identity.
type ProfileResult struct {
	Profile               string
	CredentialSource      CredentialSource
	Host                  string
	Identity              gitprovider.Identity
	PreviousIdentityCache string
	IdentityCacheUpdated  bool
}

// Refresh refreshes the selected profile identity cache.
func Refresh(ctx context.Context, cfg config.File, profileName string, resolver Resolver) (config.File, []ProfileResult, bool, error) {
	cfg = normalizeForUpdate(cfg)
	name, profile, err := config.ResolveProfile(cfg, profileName)
	if err != nil {
		return cfg, nil, false, err
	}
	updatedProfile, result, changed, err := refreshProfile(ctx, name, profile, resolver)
	if err != nil {
		return cfg, nil, false, err
	}
	cfg.Profiles[name] = updatedProfile
	return cfg, []ProfileResult{result}, changed, nil
}

// RefreshAll refreshes every configured profile identity cache.
func RefreshAll(ctx context.Context, cfg config.File, resolver Resolver) (config.File, []ProfileResult, bool, error) {
	original := normalizeForUpdate(cfg)
	names := make([]string, 0, len(original.Profiles))
	for name := range original.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	updated := normalizeForUpdate(original)
	results := make([]ProfileResult, 0, len(names))
	changed := false
	for _, name := range names {
		profile := updated.Profiles[name]
		updatedProfile, result, profileChanged, err := refreshProfile(ctx, name, profile, resolver)
		if err != nil {
			return original, nil, false, err
		}
		updated.Profiles[name] = updatedProfile
		results = append(results, result)
		changed = changed || profileChanged
	}
	return updated, results, changed, nil
}

func refreshProfile(ctx context.Context, name string, profile config.Profile, resolver Resolver) (config.Profile, ProfileResult, bool, error) {
	if resolver == nil {
		return config.Profile{}, ProfileResult{}, false, fmt.Errorf("identity: resolver is required")
	}
	source, gitConfig, previous := identitySource(profile)
	live, err := resolver.ResolveIdentity(ctx, gitConfig)
	if err != nil {
		return config.Profile{}, ProfileResult{}, false, err
	}
	if strings.TrimSpace(live.Login) == "" {
		return config.Profile{}, ProfileResult{}, false, fmt.Errorf("identity: provider returned empty login for profile %q", name)
	}

	changed := live.Login != previous
	if changed {
		switch source {
		case SourceReviewer:
			if profile.ReviewerCredentials == nil {
				return config.Profile{}, ProfileResult{}, false, fmt.Errorf("identity: reviewer credentials missing for profile %q", name)
			}
			profile.ReviewerCredentials.IdentityCache = live.Login
		default:
			profile.Git.IdentityCache = live.Login
		}
	}
	return profile, ProfileResult{
		Profile:               name,
		CredentialSource:      source,
		Host:                  gitConfig.Host,
		Identity:              live,
		PreviousIdentityCache: previous,
		IdentityCacheUpdated:  changed,
	}, changed, nil
}

func identitySource(profile config.Profile) (CredentialSource, config.GitConfig, string) {
	if profile.ReviewerCredentials != nil {
		return SourceReviewer, config.GitConfig{
			Host:          profile.Git.Host,
			AuthMode:      profile.ReviewerCredentials.AuthMode,
			CredentialRef: profile.ReviewerCredentials.CredentialRef,
			IdentityCache: profile.ReviewerCredentials.IdentityCache,
		}, profile.ReviewerCredentials.IdentityCache
	}
	return SourceGit, profile.Git, profile.Git.IdentityCache
}

func normalizeForUpdate(cfg config.File) config.File {
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
		return cfg
	}
	profiles := make(map[string]config.Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profiles[name] = profile
	}
	cfg.Profiles = profiles
	return cfg
}
