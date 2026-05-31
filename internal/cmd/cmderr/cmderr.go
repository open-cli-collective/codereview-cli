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

// Config maps config package errors to scriptable command errors.
func Config(err error) error {
	switch {
	case errors.Is(err, config.ErrInvalid):
		return exitcode.Usage(err)
	case errors.Is(err, config.ErrNotConfigured), errors.Is(err, config.ErrProfileNotFound), errors.Is(err, config.ErrUnsupported):
		return exitcode.AuthConfig(err)
	default:
		return err
	}
}

// Credential maps credstore-related errors to scriptable command errors.
func Credential(err error) error {
	switch {
	case errors.Is(err, credentials.ErrInvalidBackendSelection),
		errors.Is(err, credentials.ErrWrongService),
		errors.Is(err, credstore.ErrRefEmpty),
		errors.Is(err, credstore.ErrRefSegmentCount),
		errors.Is(err, credstore.ErrRefInvalidChar),
		errors.Is(err, credstore.ErrKeyNotAllowed),
		errors.Is(err, credstore.ErrExists):
		return exitcode.Usage(err)
	case errors.Is(err, credstore.ErrFilePassphraseRequired),
		errors.Is(err, credstore.ErrSecretServiceFailClosed),
		errors.Is(err, credstore.ErrStoreClosed),
		errors.Is(err, credstore.ErrBackendNotImplemented):
		return exitcode.AuthConfig(err)
	default:
		return err
	}
}

// Provider maps git-provider errors to scriptable command errors.
func Provider(err error) error {
	switch {
	case errors.Is(err, gitprovider.ErrAuth), errors.Is(err, gitprovider.ErrPermission):
		return exitcode.AuthConfig(err)
	case errors.Is(err, gitprovider.ErrRetryable):
		return exitcode.Upstream(err)
	case errors.Is(err, gitprovider.ErrNotFound),
		errors.Is(err, gitprovider.ErrConflict),
		errors.Is(err, gitprovider.ErrStaleSHA):
		return exitcode.With(exitcode.Failure, err)
	default:
		return err
	}
}

// BackendFlagChanged reports whether the inherited --backend flag was supplied.
func BackendFlagChanged(cmd *cobra.Command) bool {
	flag := cmd.Flag(credstore.BackendFlagName)
	return flag != nil && flag.Changed
}
