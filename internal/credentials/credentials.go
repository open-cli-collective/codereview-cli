// Package credentials adapts cli-common/credstore to cr's command surface.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

const (
	// ServiceName is the credential-ref service segment owned by cr.
	ServiceName = "codereview"

	// GitTokenKey stores the Git host access token for PAT auth.
	GitTokenKey = "git_token"
	// AnthropicAPIKeyKey stores the key name for Anthropic direct API adapters.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	AnthropicAPIKeyKey = "anthropic_api_key"
	// OpenAIAPIKeyKey stores the key name for OpenAI direct API adapters.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	OpenAIAPIKeyKey = "openai_api_key"
	// LegacyLLMAPIKeyKey is the pre-matrix generic key. It is intentionally
	// not in the v1 allowlist.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	LegacyLLMAPIKeyKey = "llm_api_key"
)

var (
	// ErrWrongService means a credential ref points at another CLI's keyring namespace.
	ErrWrongService = errors.New("credentials: credential ref uses wrong service")
	// ErrInvalidBackendSelection means a CLI/config backend selector was malformed.
	ErrInvalidBackendSelection = errors.New("credentials: invalid backend selection")
)

// AllowedKeys is cr's keyring write allowlist.
func AllowedKeys() []string {
	return []string{GitTokenKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey}
}

// Ref is a parsed cr credential ref.
type Ref struct {
	Service string
	Profile string
	Full    string
}

// FormatRef returns the canonical ref for a cr profile segment.
func FormatRef(profile string) (string, error) {
	return credstore.FormatRef(ServiceName, profile)
}

// ParseRef validates ref with the shared grammar and cr's service segment.
func ParseRef(ref string) (Ref, error) {
	service, profile, err := credstore.ParseRef(ref)
	if err != nil {
		return Ref{}, err
	}
	if service != ServiceName {
		return Ref{}, fmt.Errorf("%w: got %q, want %q", ErrWrongService, service, ServiceName)
	}
	return Ref{Service: service, Profile: profile, Full: ref}, nil
}

// StoreOptions validates backend selectors and returns credstore options.
func StoreOptions(flagValue string, flagSet bool, cfg config.File) (credstore.Options, error) {
	opts := credstore.Options{AllowedKeys: AllowedKeys()}
	if err := credstore.BindBackendFlag(&opts, flagValue, flagSet, cfg.Keyring.Backend); err != nil {
		return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
	}
	return opts, nil
}

// OpenStore opens the service-scoped keyring store.
func OpenStore(flagValue string, flagSet bool, cfg config.File) (*credstore.Store, error) {
	opts, err := StoreOptions(flagValue, flagSet, cfg)
	if err != nil {
		return nil, err
	}
	store, err := credstore.Open(ServiceName, &opts)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// BackendMetadata reports the selected backend/source without opening the store.
func BackendMetadata(flagValue string, flagSet bool, cfg config.File) (credstore.Backend, credstore.Source, error) {
	opts, err := StoreOptions(flagValue, flagSet, cfg)
	if err != nil {
		return "", "", err
	}
	if opts.Backend != "" {
		return opts.Backend, credstore.SourceExplicit, nil
	}
	if value := os.Getenv(BackendEnvVar()); value != "" {
		backend, err := credstore.ParseBackend(value)
		if err != nil {
			return "", "", fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
		}
		return backend, credstore.SourceEnv, nil
	}
	if opts.ConfigBackend != "" {
		backend, err := credstore.ParseBackend(string(opts.ConfigBackend))
		if err != nil {
			return "", "", fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
		}
		return backend, credstore.SourceConfig, nil
	}
	switch runtime.GOOS {
	case "darwin":
		return credstore.BackendKeychain, credstore.SourceAuto, nil
	case "windows":
		return credstore.BackendWinCred, credstore.SourceAuto, nil
	case "linux":
		return credstore.BackendSecretService, credstore.SourceAuto, nil
	default:
		return "", "", credstore.ErrBackendNotImplemented
	}
}

// KeySpec describes one key in a declared credential bundle.
type KeySpec struct {
	Key      string
	Required bool
}

// KeySpecsForPurpose returns the exact keyring keys expected for a config credential ref.
func KeySpecsForPurpose(ref config.CredentialRef) ([]KeySpec, error) {
	switch ref.Purpose {
	case "git", "reviewer_credentials":
		mode := config.GitAuthMode(ref.Mode)
		if !mode.Valid() {
			return nil, fmt.Errorf("%w: %s auth_mode %q", config.ErrInvalid, ref.Purpose, mode)
		}
		if !mode.Supported() {
			return nil, fmt.Errorf("%w: %s auth_mode %q", config.ErrUnsupported, ref.Purpose, mode)
		}
		return []KeySpec{{Key: GitTokenKey, Required: true}}, nil
	case "llm":
		if config.LLMAuth(ref.Mode) != config.LLMAuthAPIKey {
			return nil, fmt.Errorf("%w: llm auth %q", config.ErrUnsupported, ref.Mode)
		}
		key, err := LLMAPIKeyForProvider(config.LLMProvider(ref.Provider))
		if err != nil {
			return nil, err
		}
		return []KeySpec{{Key: key, Required: true}}, nil
	default:
		return nil, fmt.Errorf("credentials: unsupported credential purpose %q", ref.Purpose)
	}
}

// KeyForPurpose returns the single required keyring key expected for a config credential ref.
func KeyForPurpose(ref config.CredentialRef) (string, error) {
	specs, err := KeySpecsForPurpose(ref)
	if err != nil {
		return "", err
	}
	if len(specs) != 1 {
		return "", fmt.Errorf("credentials: credential purpose %q has %d keys, want one", ref.Purpose, len(specs))
	}
	return specs[0].Key, nil
}

// LLMAPIKeyForProvider returns the provider-specific key for API-key LLM auth.
func LLMAPIKeyForProvider(provider config.LLMProvider) (string, error) {
	switch provider {
	case config.LLMProviderAnthropic:
		return AnthropicAPIKeyKey, nil
	case config.LLMProviderOpenAI:
		return OpenAIAPIKeyKey, nil
	case config.LLMProviderPi:
		return "", fmt.Errorf("%w: llm provider %q does not support api-key credentials", config.ErrUnsupported, provider)
	default:
		return "", fmt.Errorf("%w: llm provider %q", config.ErrInvalid, provider)
	}
}

// ValidateAllowedKeyForConfig validates key globally and, when ref is declared
// in cfg, against the exact key set for that ref.
func ValidateAllowedKeyForConfig(cfg config.File, ref, key string) error {
	if err := ValidateAllowedKey(key); err != nil {
		return err
	}
	expected, err := ExpectedKeysForConfigRef(cfg, ref)
	if err != nil {
		return err
	}
	if len(expected) == 0 {
		return nil
	}
	for _, candidate := range expected {
		if key == candidate {
			return nil
		}
	}
	return &credstore.KeyError{Key: key, Allowed: expected}
}

// ExpectedKeysForConfigRef returns the sorted union of expected keys for
// supported declarations of ref across profiles. Unsupported matching
// declarations fail only when no supported declaration contributes keys. An
// undeclared ref returns nil.
func ExpectedKeysForConfigRef(cfg config.File, ref string) ([]string, error) {
	keys := map[string]struct{}{}
	var unsupportedErr error
	for _, profile := range cfg.Profiles {
		for _, candidate := range matchingCredentialRefs(profile, ref) {
			specs, err := KeySpecsForPurpose(candidate)
			if err != nil {
				if errors.Is(err, config.ErrUnsupported) && unsupportedErr == nil {
					unsupportedErr = err
					continue
				}
				return nil, err
			}
			for _, spec := range specs {
				keys[spec.Key] = struct{}{}
			}
		}
	}
	if len(keys) == 0 {
		if unsupportedErr != nil {
			return nil, unsupportedErr
		}
		return nil, nil
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func matchingCredentialRefs(profile config.Profile, ref string) []config.CredentialRef {
	refs := []config.CredentialRef{}
	if profile.Git.CredentialRef == ref {
		refs = append(refs, config.CredentialRef{
			Purpose: "git",
			Ref:     profile.Git.CredentialRef,
			Mode:    string(profile.Git.AuthMode),
		})
	}
	if profile.ReviewerCredentials != nil && profile.ReviewerCredentials.CredentialRef == ref {
		refs = append(refs, config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     profile.ReviewerCredentials.CredentialRef,
			Mode:    string(profile.ReviewerCredentials.AuthMode),
		})
	}
	if profile.LLM.Auth == config.LLMAuthAPIKey && profile.LLM.CredentialRef == ref {
		refs = append(refs, config.CredentialRef{
			Purpose:  "llm",
			Ref:      profile.LLM.CredentialRef,
			Mode:     string(profile.LLM.Auth),
			Provider: string(profile.LLM.Provider),
		})
	}
	return refs
}

// ValidateAllowedKey fails when key is not in cr's write allowlist.
func ValidateAllowedKey(key string) error {
	allowed := AllowedKeys()
	for _, candidate := range allowed {
		if key == candidate {
			return nil
		}
	}
	sort.Strings(allowed)
	return &credstore.KeyError{Key: key, Allowed: allowed}
}

// BackendEnvVar returns cr's backend selector environment variable name.
func BackendEnvVar() string {
	return credstore.BackendEnvVar(ServiceName)
}

// TrimSecretIngress removes the terminal newline produced by echo/heredocs
// without treating other whitespace as disposable.
func TrimSecretIngress(value string) string {
	return strings.TrimRight(value, "\r\n")
}
