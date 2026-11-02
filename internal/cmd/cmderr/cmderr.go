// Package cmderr maps domain errors onto cr's command exit taxonomy.
package cmderr

import (
	"errors"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// Sentinel groups shared by the mappers below and Classify, so each error
// class is listed once.
var (
	configUsageErrors = []error{
		config.ErrInvalid,
		config.ErrUnsupportedEffort,
	}
	configAuthErrors = []error{
		config.ErrNotConfigured,
		config.ErrProfileNotFound,
		config.ErrRepositoryProfileAmbiguous,
		config.ErrSecretsStoreNotFound,
		config.ErrUnsupported,
	}
	credentialUsageErrors = []error{
		credentials.ErrInvalidBackendSelection,
		credentials.ErrWrongService,
		credstore.ErrRefEmpty,
		credstore.ErrRefSegmentCount,
		credstore.ErrRefInvalidChar,
		credstore.ErrKeyNotAllowed,
		credstore.ErrExists,
	}
	credentialAuthErrors = []error{
		credstore.ErrFilePassphraseRequired,
		credstore.ErrSecretServiceFailClosed,
		credstore.ErrStoreClosed,
		credstore.ErrBackendNotImplemented,
	}
	providerAuthErrors = []error{
		gitprovider.ErrAuth,
		gitprovider.ErrPermission,
		gitprovider.ErrIneligibleReviewAuthority,
	}
	providerUpstreamErrors = []error{
		gitprovider.ErrRetryable,
	}
	providerFailureErrors = []error{
		gitprovider.ErrNotFound,
		gitprovider.ErrConflict,
		gitprovider.ErrStaleSHA,
	}
)

// Config maps config package errors to scriptable command errors.
func Config(err error) error {
	switch {
	case isAny(err, configUsageErrors):
		return exitcode.Usage(err)
	case isAny(err, configAuthErrors):
		return exitcode.AuthConfig(err)
	default:
		return err
	}
}

// Credential maps credstore-related errors to scriptable command errors.
func Credential(err error) error {
	switch {
	case isAny(err, credentialUsageErrors):
		return exitcode.Usage(err)
	case isAny(err, credentialAuthErrors):
		return exitcode.AuthConfig(err)
	default:
		return err
	}
}

// Provider maps git-provider errors to scriptable command errors.
func Provider(err error) error {
	var providerErr *gitprovider.ProviderError
	if errors.As(err, &providerErr) && providerErr.Kind != nil {
		return providerKind(err, providerErr.Kind)
	}
	return providerKind(err, err)
}

func providerKind(err error, kind error) error {
	switch {
	case isAny(kind, providerAuthErrors):
		return exitcode.AuthConfig(err)
	case isAny(kind, providerUpstreamErrors):
		return exitcode.Upstream(err)
	case isAny(kind, providerFailureErrors):
		return exitcode.With(exitcode.Failure, err)
	default:
		// Unknown provider errors intentionally fall through. exitcode.FromError
		// maps unwrapped errors to generic failure without inventing retry/auth
		// semantics for future provider sentinels.
		return err
	}
}

// Classify maps config, git-provider, and credential-store errors onto the
// exit taxonomy, checked in that order, and returns any other error
// unchanged. Commands that load a profile, open its credential store, and
// call the provider use it so their exit codes stay aligned.
func Classify(err error) error {
	switch {
	case isAny(err, configUsageErrors), isAny(err, configAuthErrors):
		return Config(err)
	case isAny(err, providerAuthErrors), isAny(err, providerUpstreamErrors), isAny(err, providerFailureErrors):
		return Provider(err)
	case isAny(err, credentialUsageErrors), isAny(err, credentialAuthErrors):
		return Credential(err)
	default:
		return err
	}
}

func isAny(err error, sentinels []error) bool {
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// BackendFlagChanged reports whether the inherited --backend flag was supplied.
func BackendFlagChanged(cmd *cobra.Command) bool {
	flag := cmd.Flag(credstore.BackendFlagName)
	return flag != nil && flag.Changed
}
