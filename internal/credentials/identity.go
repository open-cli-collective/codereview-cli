package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

var errIdentityAPIKeyUnavailable = errors.New("credentials: identity API key is unavailable")

// IdentityAPIKeyForKind maps one fixed identity provider kind to its allowed
// credential key. User configuration cannot select an arbitrary key name.
func IdentityAPIKeyForKind(kind string) (string, error) {
	switch kind {
	case "system_one":
		return SystemOneAPIKeyKey, nil
	case "openai_decisions":
		return OpenAIAPIKeyKey, nil
	case "cloudflare_clef":
		return CloudflareAPIKeyKey, nil
	default:
		return "", fmt.Errorf("%w: unsupported identity provider kind %q", config.ErrInvalid, kind)
	}
}

// ReadIdentityAPIKey reads one provider-fixed API key from an already selected
// credential reader. It never opens a store or writes credential data.
func ReadIdentityAPIKey(ctx context.Context, cfg config.File, reader Reader, location config.CredentialLocation, kind string) (string, error) {
	if ctx == nil {
		return "", errIdentityAPIKeyUnavailable
	}
	key, err := IdentityAPIKeyForKind(kind)
	if err != nil {
		return "", err
	}
	parsed, err := ParseRef(location.Name)
	if err != nil {
		return "", err
	}
	if _, err := ResolveCredentialStore(cfg, location.Store); err != nil {
		return "", err
	}
	if err := ValidateAllowedKeyForConfig(cfg, location.Name, key); err != nil {
		return "", err
	}
	if reader == nil {
		return "", errIdentityAPIKeyUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := reader.Get(parsed.Profile, key)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil || value == "" {
		return "", errIdentityAPIKeyUnavailable
	}
	return value, nil
}
