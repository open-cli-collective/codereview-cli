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
	// LLMAPIKeyKey stores the key name for direct LLM provider adapters.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	LLMAPIKeyKey = "llm_api_key"
)

var (
	// ErrWrongService means a credential ref points at another CLI's keyring namespace.
	ErrWrongService = errors.New("credentials: credential ref uses wrong service")
	// ErrInvalidBackendSelection means a CLI/config backend selector was malformed.
	ErrInvalidBackendSelection = errors.New("credentials: invalid backend selection")
)

// AllowedKeys is cr's keyring write allowlist.
func AllowedKeys() []string {
	return []string{GitTokenKey, LLMAPIKeyKey}
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

// KeyForPurpose returns the keyring key expected for a config credential ref.
func KeyForPurpose(ref config.CredentialRef) (string, error) {
	switch ref.Purpose {
	case "git", "reviewer_credentials":
		return GitTokenKey, nil
	case "llm":
		return LLMAPIKeyKey, nil
	default:
		return "", fmt.Errorf("credentials: unsupported credential purpose %q", ref.Purpose)
	}
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
