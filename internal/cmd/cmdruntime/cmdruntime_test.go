package cmdruntime

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestMapRunError(t *testing.T) {
	plain := errors.New("plain failure")
	tests := []struct {
		name string
		err  error
		want int
		is   error
	}{
		{name: "invalid config", err: config.ErrInvalid, want: exitcode.UsageError, is: config.ErrInvalid},
		{name: "missing config", err: config.ErrNotConfigured, want: exitcode.AuthConfigError, is: config.ErrNotConfigured},
		{name: "unsafe agent source", err: agents.ErrUnsafeSource, want: exitcode.UsageError, is: agents.ErrUnsafeSource},
		{name: "provider auth", err: gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, errors.New("bad token")), want: exitcode.AuthConfigError, is: gitprovider.ErrAuth},
		{name: "provider retryable", err: gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationWhoAmI, errors.New("timeout")), want: exitcode.UpstreamError, is: gitprovider.ErrRetryable},
		{name: "credential usage", err: credentials.ErrInvalidBackendSelection, want: exitcode.UsageError, is: credentials.ErrInvalidBackendSelection},
		{name: "credential auth config", err: credstore.ErrStoreClosed, want: exitcode.AuthConfigError, is: credstore.ErrStoreClosed},
		{name: "passthrough", err: plain, want: exitcode.Failure, is: plain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MapRunError(tt.err)
			if got := exitcode.FromError(err); got != tt.want {
				t.Fatalf("exit code = %d, want %d", got, tt.want)
			}
			if !errors.Is(err, tt.is) {
				t.Fatalf("mapped error = %v, want errors.Is %v", err, tt.is)
			}
		})
	}
}
