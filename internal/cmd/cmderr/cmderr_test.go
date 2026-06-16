package cmderr

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestConfigErrorMapping(t *testing.T) {
	err := Config(config.ErrInvalid)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("mapped error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}

	err = Config(config.ErrUnsupported)
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}

	err = Config(config.ErrSecretsProfileNotFound)
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("missing secrets profile exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestCredentialErrorMapping(t *testing.T) {
	usageErrors := []error{
		credentials.ErrInvalidBackendSelection,
		credentials.ErrWrongService,
		credstore.ErrRefEmpty,
		credstore.ErrRefSegmentCount,
		credstore.ErrRefInvalidChar,
		credstore.ErrKeyNotAllowed,
		credstore.ErrExists,
	}
	for _, err := range usageErrors {
		if got := exitcode.FromError(Credential(err)); got != exitcode.UsageError {
			t.Fatalf("Credential(%v) exit code = %d, want %d", err, got, exitcode.UsageError)
		}
	}

	authErrors := []error{
		credstore.ErrFilePassphraseRequired,
		credstore.ErrSecretServiceFailClosed,
		credstore.ErrStoreClosed,
		credstore.ErrBackendNotImplemented,
	}
	for _, err := range authErrors {
		if got := exitcode.FromError(Credential(err)); got != exitcode.AuthConfigError {
			t.Fatalf("Credential(%v) exit code = %d, want %d", err, got, exitcode.AuthConfigError)
		}
	}

	err := Credential(errors.New("unexpected write failure"))
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("unexpected credential error exit code = %d, want %d", got, exitcode.Failure)
	}
}

func TestProviderErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		is   error
	}{
		{name: "auth", err: gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, errors.New("bad token")), want: exitcode.AuthConfigError, is: gitprovider.ErrAuth},
		{name: "permission", err: gitprovider.WrapError(gitprovider.ErrPermission, gitprovider.OperationWhoAmI, errors.New("forbidden")), want: exitcode.AuthConfigError, is: gitprovider.ErrPermission},
		{name: "retryable", err: gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationWhoAmI, errors.New("timeout")), want: exitcode.UpstreamError, is: gitprovider.ErrRetryable},
		{name: "not found", err: gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationWhoAmI, errors.New("missing")), want: exitcode.Failure, is: gitprovider.ErrNotFound},
		{name: "conflict", err: gitprovider.WrapError(gitprovider.ErrConflict, gitprovider.OperationWhoAmI, errors.New("conflict")), want: exitcode.Failure, is: gitprovider.ErrConflict},
		{name: "stale sha", err: gitprovider.WrapError(gitprovider.ErrStaleSHA, gitprovider.OperationWhoAmI, errors.New("stale")), want: exitcode.Failure, is: gitprovider.ErrStaleSHA},
		{name: "primary stale sha beats retryable cause", err: gitprovider.WrapError(gitprovider.ErrStaleSHA, gitprovider.OperationWhoAmI, gitprovider.ErrRetryable), want: exitcode.Failure, is: gitprovider.ErrStaleSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Provider(tt.err)
			if got := exitcode.FromError(err); got != tt.want {
				t.Fatalf("exit code = %d, want %d", got, tt.want)
			}
			if !errors.Is(err, tt.is) {
				t.Fatalf("mapped error = %v, want errors.Is %v", err, tt.is)
			}
		})
	}
}
