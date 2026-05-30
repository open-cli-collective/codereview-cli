package cmderr

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
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
}

func TestCredentialErrorMapping(t *testing.T) {
	usageErrors := []error{
		credentials.ErrInvalidBackendSelection,
		credentials.ErrWrongService,
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
		credstore.ErrBackendNotImplemented,
	}
	for _, err := range authErrors {
		if got := exitcode.FromError(Credential(err)); got != exitcode.AuthConfigError {
			t.Fatalf("Credential(%v) exit code = %d, want %d", err, got, exitcode.AuthConfigError)
		}
	}
}
