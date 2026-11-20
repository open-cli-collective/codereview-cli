package benchmarkcmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
)

func resolveIdentityAPIKey(opts *root.Options, command *cobra.Command, provider identityProviderConfig) func(context.Context) (string, error) {
	return cmdruntime.IdentityKeyResolver(opts, cmderr.BackendFlagChanged(command), "benchmark", provider.Kind, provider.Credential)
}
