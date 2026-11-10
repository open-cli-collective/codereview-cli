package benchmarkcmd

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

func resolveIdentityAPIKey(opts *root.Options, command *cobra.Command, provider identityProviderConfig) func(context.Context) (string, error) {
	logger := root.NewProgressLogger(opts)
	return func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cfg, err := cmdruntime.LoadConfig(opts)
		if errors.Is(err, config.ErrNotConfigured) {
			cfg = config.File{}
		} else if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := credentials.IdentityAPIKeyForKind(provider.Kind)
		if err != nil {
			return "", err
		}
		if _, err := credentials.ParseRef(provider.Credential.Name); err != nil {
			return "", err
		}
		if err := credentials.ValidateAllowedKeyForConfig(cfg, provider.Credential.Name, key); err != nil {
			return "", err
		}
		resolved, err := credentials.ResolveCredentialStore(cfg, provider.Credential.Store)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		store, err := credentials.OpenResolvedStore(opts.Backend, cmderr.BackendFlagChanged(command), cfg, resolved)
		if err != nil {
			return "", err
		}
		defer func() { _ = store.Close() }()

		baseReader := credentials.ProgressStoreReader("benchmark", logger, resolved, store)
		reader := credentials.ProgressCachingReader("benchmark", logger, resolved.ID, resolved, baseReader)
		if closer, ok := reader.(interface{ Close() error }); ok {
			defer func() { _ = closer.Close() }()
		}
		return credentials.ReadIdentityAPIKey(ctx, cfg, reader, provider.Credential, provider.Kind)
	}
}
