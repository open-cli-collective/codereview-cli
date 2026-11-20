package cmdruntime

import (
	"context"
	"errors"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

// IdentityKeyResolver opens only the credential selected by the identity provider.
func IdentityKeyResolver(opts *root.Options, backendChanged bool, operation, kind string, location config.CredentialLocation) func(context.Context) (string, error) {
	logger := root.NewProgressLogger(opts)
	return func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cfg, err := LoadConfig(opts)
		if errors.Is(err, config.ErrNotConfigured) {
			cfg = config.File{}
		} else if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := credentials.IdentityAPIKeyForKind(kind)
		if err != nil {
			return "", err
		}
		if _, err := credentials.ParseRef(location.Name); err != nil {
			return "", err
		}
		if err := credentials.ValidateAllowedKeyForConfig(cfg, location.Name, key); err != nil {
			return "", err
		}
		resolved, err := credentials.ResolveCredentialStore(cfg, location.Store)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		store, err := credentials.OpenResolvedStore(opts.Backend, backendChanged, cfg, resolved)
		if err != nil {
			return "", err
		}
		defer func() { _ = store.Close() }()

		baseReader := credentials.ProgressStoreReader(operation, logger, resolved, store)
		reader := credentials.ProgressCachingReader(operation, logger, resolved.ID, resolved, baseReader)
		if closer, ok := reader.(interface{ Close() error }); ok {
			defer func() { _ = closer.Close() }()
		}
		return credentials.ReadIdentityAPIKey(ctx, cfg, reader, location, kind)
	}
}
