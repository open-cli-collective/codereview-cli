package benchmarkcmd

import (
	"errors"
	"fmt"

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
)

// identityClefEndpoint resolves the only supported Clef route for a selected
// account and model. The suite runner also uses this value in resolved
// configuration so retained provenance records the exact Cloudflare route.
func identityClefEndpoint(provider identityProviderConfig, model string) (string, error) {
	if provider.Kind != "cloudflare_clef" {
		return "", fmt.Errorf("provider %q is not a Cloudflare Clef provider", provider.Name)
	}
	if provider.Endpoint != "" || provider.AuthScheme != "" || provider.ServiceTier != "" {
		return "", fmt.Errorf("provider %q must not configure endpoint, auth_scheme, or service_tier for Cloudflare Clef", provider.Name)
	}
	if !validAPIKeyEnvName(provider.APIKeyEnv) {
		return "", fmt.Errorf("provider %q api_key_env must be an environment variable name", provider.Name)
	}
	endpoint, err := cloudflareclef.Endpoint(provider.AccountID, model)
	if err != nil {
		return "", errors.New("cloudflare Clef requires an ASCII account ID and model clef or clef-flash")
	}
	return endpoint, nil
}

func newIdentityClefProviderRuntime(provider identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
	return identityClefProviderRuntime(provider, flags, defaultInferClefDependencies())
}

func identityClefProviderRuntime(provider identityProviderConfig, flags identityInferenceFlags, dependencies inferClefDependencies) (identityProviderRuntime, error) {
	endpoint, err := identityClefEndpoint(provider, flags.model)
	if err != nil {
		return identityProviderRuntime{}, err
	}
	flags.endpoint = endpoint
	flags.apiKeyEnv = provider.APIKeyEnv
	flags.authScheme = "bearer"
	flags.serviceTier = ""
	clefFlags := inferClefFlags{identityInferenceFlags: flags, accountID: provider.AccountID}
	return identityProviderRuntime{
		dependencies: identityInferenceDependencies{
			lookupAPIKey: dependencies.lookupAPIKey,
			now:          dependencies.now,
			rename:       dependencies.rename,
		},
		backend: inferClefBackend(clefFlags, dependencies),
	}, nil
}
