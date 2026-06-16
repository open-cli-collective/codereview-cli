// Package credentials adapts cli-common/credstore to cr's command surface.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

const (
	// ServiceName is the credential-ref service segment owned by cr.
	ServiceName = "codereview"

	// BackendSourceSecretsProfile records that a named secrets-management
	// profile selected the active credential backend.
	BackendSourceSecretsProfile credstore.Source = "secrets_profile"

	// GitTokenKey stores the Git host access token for PAT auth.
	GitTokenKey = "git_token"
	// GitHubAppIDKey stores the GitHub App JWT issuer, usually the app ID.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	GitHubAppIDKey = "github_app_id"
	// GitHubAppPrivateKeyKey stores the GitHub App PEM private key.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	GitHubAppPrivateKeyKey = "github_app_private_key"
	// GitHubAppInstallationIDKey stores an optional explicit GitHub App installation ID.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	GitHubAppInstallationIDKey = "github_app_installation_id"
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
	return []string{GitTokenKey, GitHubAppIDKey, GitHubAppPrivateKeyKey, GitHubAppInstallationIDKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey}
}

// Ref is a parsed cr credential ref.
type Ref struct {
	Service string
	Profile string
	Full    string
}

// SecretsProfileSelectionSource identifies why a secrets-management profile was selected.
type SecretsProfileSelectionSource string

const (
	// SecretsProfileSelectionExplicit means the active profile selected a named
	// secrets-management profile directly.
	SecretsProfileSelectionExplicit SecretsProfileSelectionSource = "profile"
	// SecretsProfileSelectionDefault means the configured global default
	// secrets-management profile selected the store.
	SecretsProfileSelectionDefault SecretsProfileSelectionSource = "default_profile"
	// SecretsProfileSelectionLegacyDefault means no named secrets-management
	// profile applied, so legacy backend fallback rules selected the store.
	SecretsProfileSelectionLegacyDefault SecretsProfileSelectionSource = "legacy_fallback"
)

// ResolvedSecretsProfile is the typed runtime store-selection result.
type ResolvedSecretsProfile struct {
	ID              string
	Label           string
	Backend         string
	Source          config.EffectiveSecretsProfileSource
	SelectionSource SecretsProfileSelectionSource
}

// DisplayName returns the best user-facing label for the resolved store.
func (r ResolvedSecretsProfile) DisplayName() string {
	if strings.TrimSpace(r.Label) != "" {
		return strings.TrimSpace(r.Label)
	}
	return strings.TrimSpace(r.ID)
}

// Equal reports whether two resolved secrets-profile selections point at the
// same logical credential store.
func (r ResolvedSecretsProfile) Equal(other ResolvedSecretsProfile) bool {
	return r.ID == other.ID && r.Source == other.Source && r.Backend == other.Backend
}

// IsNamed reports whether the selection came from an explicit named secrets profile.
func (r ResolvedSecretsProfile) IsNamed() bool {
	return r.Source == config.EffectiveSecretsProfileSourceConfigured
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

// ResolveSecretsProfileForProfile resolves the effective secrets-management
// profile for one review profile.
func ResolveSecretsProfileForProfile(cfg config.File, profile config.Profile) (ResolvedSecretsProfile, error) {
	selection := strings.TrimSpace(profile.SecretsProfile)
	if selection != "" {
		return resolveConfiguredSecretsProfile(cfg, selection, SecretsProfileSelectionExplicit)
	}
	if effectiveDefault, ok := config.EffectiveDefaultSecretsProfile(cfg); ok && effectiveDefault.Source == config.EffectiveSecretsProfileSourceConfigured {
		return resolvedSecretsProfileFromEffective(effectiveDefault, SecretsProfileSelectionDefault), nil
	}
	return resolveLegacySecretsProfile(cfg), nil
}

// ResolveSecretsProfileForRef resolves the effective secrets-management profile
// for a low-level credential ref write/read, optionally narrowed by the global
// --profile selection.
func ResolveSecretsProfileForRef(cfg config.File, ref string, selectedProfile string) (ResolvedSecretsProfile, error) {
	selectedProfile = strings.TrimSpace(selectedProfile)
	if selectedProfile != "" {
		profile, ok := cfg.Profiles[selectedProfile]
		if !ok {
			return ResolvedSecretsProfile{}, fmt.Errorf("%w: %s", config.ErrProfileNotFound, selectedProfile)
		}
		if len(matchingCredentialRefs(profile, ref)) > 0 {
			return ResolveSecretsProfileForProfile(cfg, profile)
		}
		owners := profilesDeclaringCredentialRef(cfg, ref)
		if len(owners) == 0 {
			return ResolveSecretsProfileForProfile(cfg, profile)
		}
		return ResolvedSecretsProfile{}, fmt.Errorf("%w: credential ref %q is not declared by selected profile %q; declared by %s", config.ErrInvalid, ref, selectedProfile, strings.Join(ownerNames(owners), ", "))
	}

	owners := profilesDeclaringCredentialRef(cfg, ref)
	if len(owners) == 0 {
		return resolveLegacySecretsProfile(cfg), nil
	}
	resolved, err := ResolveSecretsProfileForProfile(cfg, owners[0].Profile)
	if err != nil {
		return ResolvedSecretsProfile{}, err
	}
	for _, owner := range owners[1:] {
		next, err := ResolveSecretsProfileForProfile(cfg, owner.Profile)
		if err != nil {
			return ResolvedSecretsProfile{}, err
		}
		if resolved.Equal(next) {
			continue
		}
		return ResolvedSecretsProfile{}, ambiguousSecretsProfileError(ref, cfg, owners)
	}
	return resolved, nil
}

// StoreOptionsForResolvedProfile builds credstore options for one resolved
// secrets-management profile.
func StoreOptionsForResolvedProfile(flagValue string, flagSet bool, cfg config.File, resolved ResolvedSecretsProfile) (credstore.Options, error) {
	if resolved.IsNamed() {
		if flagSet {
			return credstore.Options{}, fmt.Errorf("%w: --backend conflicts with named secrets-management profile %q", config.ErrInvalid, resolved.DisplayName())
		}
		profile, ok := cfg.Secrets.Profiles[resolved.ID]
		if !ok {
			return credstore.Options{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, resolved.ID)
		}
		backend, err := credstore.ParseBackend(resolved.Backend)
		if err != nil {
			return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
		}
		options := credstore.Options{
			AllowedKeys: AllowedKeys(),
			Backend:     backend,
		}
		if config.IsOnePasswordSecretsBackend(profile.Backend.Kind) {
			options.OnePassword, err = onePasswordOptionsFromConfig(profile.Backend)
			if err != nil {
				return credstore.Options{}, fmt.Errorf("%w: %w", config.ErrInvalid, err)
			}
		}
		return options, nil
	}
	return StoreOptions(flagValue, flagSet, cfg)
}

// OpenResolvedStore opens the resolved service-scoped keyring store.
func OpenResolvedStore(flagValue string, flagSet bool, cfg config.File, resolved ResolvedSecretsProfile) (*credstore.Store, error) {
	opts, err := StoreOptionsForResolvedProfile(flagValue, flagSet, cfg, resolved)
	if err != nil {
		return nil, err
	}
	store, err := credstore.Open(ServiceName, &opts)
	if err != nil {
		return nil, fmt.Errorf("credentials: opening secrets-management profile %q (%s): %w", resolved.DisplayName(), resolved.Backend, err)
	}
	return store, nil
}

// StoreOptions validates backend selectors and returns credstore options.
func StoreOptions(flagValue string, flagSet bool, cfg config.File) (credstore.Options, error) {
	opts := credstore.Options{AllowedKeys: AllowedKeys()}
	if err := credstore.BindBackendFlag(&opts, flagValue, flagSet, cfg.Keyring.Backend); err != nil {
		return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
	}
	if err := rejectLegacyOnePasswordBackend(opts.Backend); err != nil {
		return credstore.Options{}, err
	}
	if err := rejectLegacyOnePasswordBackend(opts.ConfigBackend); err != nil {
		return credstore.Options{}, err
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

func onePasswordOptionsFromConfig(backend config.SecretsProfileBackend) (*credstore.OnePasswordOptions, error) {
	if !config.IsOnePasswordSecretsBackend(backend.Kind) {
		return nil, nil
	}
	cfg := backend.OnePassword
	if cfg == nil {
		cfg = &config.SecretsProfileOnePasswordConfig{}
	}
	options := &credstore.OnePasswordOptions{
		VaultID:          strings.TrimSpace(cfg.VaultID),
		ItemTitlePrefix:  strings.TrimSpace(cfg.ItemTitlePrefix),
		ItemTag:          strings.TrimSpace(cfg.ItemTag),
		ItemFieldTitle:   strings.TrimSpace(cfg.ItemFieldTitle),
		ConnectHost:      strings.TrimSpace(cfg.ConnectHost),
		ConnectTokenEnv:  strings.TrimSpace(cfg.ConnectTokenEnv),
		ServiceTokenEnv:  strings.TrimSpace(cfg.ServiceTokenEnv),
		DesktopAccountID: strings.TrimSpace(cfg.DesktopAccountID),
	}
	if strings.TrimSpace(cfg.Timeout) != "" {
		timeout, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid 1Password timeout %q: %w", cfg.Timeout, err)
		}
		options.Timeout = timeout
	}
	return options, nil
}

func resolvedSecretsProfileFromEffective(profile config.EffectiveSecretsProfile, selectionSource SecretsProfileSelectionSource) ResolvedSecretsProfile {
	return ResolvedSecretsProfile{
		ID:              profile.ID,
		Label:           strings.TrimSpace(profile.Label),
		Backend:         profile.Backend,
		Source:          profile.Source,
		SelectionSource: selectionSource,
	}
}

func resolveConfiguredSecretsProfile(cfg config.File, id string, selectionSource SecretsProfileSelectionSource) (ResolvedSecretsProfile, error) {
	id = strings.TrimSpace(id)
	for _, profile := range config.EffectiveSecretsProfiles(cfg) {
		if profile.ID != id {
			continue
		}
		if profile.Source != config.EffectiveSecretsProfileSourceConfigured {
			break
		}
		return resolvedSecretsProfileFromEffective(profile, selectionSource), nil
	}
	return ResolvedSecretsProfile{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, id)
}

func resolveLegacySecretsProfile(cfg config.File) ResolvedSecretsProfile {
	backend := strings.TrimSpace(cfg.Keyring.Backend)
	if backend == "" {
		backend = config.ProjectedLegacySecretsBackendKind
	}
	return ResolvedSecretsProfile{
		ID:              config.LegacyProjectedSecretsProfileID,
		Label:           "Legacy default",
		Backend:         backend,
		Source:          config.EffectiveSecretsProfileSourceProjectedLegacy,
		SelectionSource: SecretsProfileSelectionLegacyDefault,
	}
}

type credentialRefOwner struct {
	Name    string
	Profile config.Profile
}

func profilesDeclaringCredentialRef(cfg config.File, ref string) []credentialRefOwner {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	owners := make([]credentialRefOwner, 0, len(names))
	for _, name := range names {
		profile := cfg.Profiles[name]
		if len(matchingCredentialRefs(profile, ref)) == 0 {
			continue
		}
		owners = append(owners, credentialRefOwner{Name: name, Profile: profile})
	}
	return owners
}

func ownerNames(owners []credentialRefOwner) []string {
	names := make([]string, 0, len(owners))
	for _, owner := range owners {
		names = append(names, owner.Name)
	}
	return names
}

func ambiguousSecretsProfileError(ref string, cfg config.File, owners []credentialRefOwner) error {
	details := make([]string, 0, len(owners))
	for _, owner := range owners {
		resolved, err := ResolveSecretsProfileForProfile(cfg, owner.Profile)
		if err != nil {
			return err
		}
		details = append(details, fmt.Sprintf("%s -> %s (%s)", owner.Name, resolved.DisplayName(), resolved.ID))
	}
	return fmt.Errorf("%w: credential ref %q is declared by profiles using different secrets-management profiles; pass --profile to disambiguate: %s", config.ErrInvalid, ref, strings.Join(details, "; "))
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
		if err := rejectLegacyOnePasswordBackend(backend); err != nil {
			return "", "", err
		}
		return backend, credstore.SourceEnv, nil
	}
	if opts.ConfigBackend != "" {
		backend, err := credstore.ParseBackend(string(opts.ConfigBackend))
		if err != nil {
			return "", "", fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
		}
		if err := rejectLegacyOnePasswordBackend(backend); err != nil {
			return "", "", err
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

func rejectLegacyOnePasswordBackend(kind credstore.Backend) error {
	if !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kind)) {
		return nil
	}
	return fmt.Errorf("%w: backend %q is only supported through named secrets-management profiles", ErrInvalidBackendSelection, kind)
}

// KeyStatus reports the presence state for one declared keyring key.
type KeyStatus struct {
	Key      string
	Required bool
	Present  *bool
	Status   string
	Error    string
}

// CredentialStatus reports declared ref context and per-key presence state.
type CredentialStatus struct {
	Purpose  string
	Ref      string
	Mode     string
	Provider string
	Keys     []KeyStatus
}

// KeyStatusStore is the read-only store surface needed for credential health inspection.
type KeyStatusStore interface {
	Exists(profile, key string) (bool, error)
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
		switch mode {
		case config.GitAuthModePAT:
			return []KeySpec{{Key: GitTokenKey, Required: true}}, nil
		case config.GitAuthModeGitHubApp:
			return []KeySpec{
				{Key: GitHubAppIDKey, Required: true},
				{Key: GitHubAppPrivateKeyKey, Required: true},
				{Key: GitHubAppInstallationIDKey, Required: false},
			}, nil
		case config.GitAuthModeOAuthDevice:
			return nil, fmt.Errorf("%w: %s auth_mode %q", config.ErrUnsupported, ref.Purpose, mode)
		default:
			return nil, fmt.Errorf("%w: %s auth_mode %q", config.ErrUnsupported, ref.Purpose, mode)
		}
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

// CredentialStatuses returns per-ref, per-key presence state for declared refs.
func CredentialStatuses(store KeyStatusStore, refs []config.CredentialRef, storeErr error) ([]CredentialStatus, error) {
	statuses := make([]CredentialStatus, 0, len(refs))
	for _, ref := range refs {
		status, err := CredentialRefStatus(store, ref, storeErr)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// CredentialRefStatus returns per-key presence state for one declared ref.
func CredentialRefStatus(store KeyStatusStore, ref config.CredentialRef, storeErr error) (CredentialStatus, error) {
	parsed, err := ParseRef(ref.Ref)
	if err != nil {
		return CredentialStatus{}, err
	}
	specs, err := KeySpecsForPurpose(ref)
	if err != nil {
		return CredentialStatus{}, err
	}
	keys := make([]KeyStatus, 0, len(specs))
	for _, spec := range specs {
		var present bool
		statusErr := storeErr
		if statusErr == nil && store != nil {
			present, statusErr = store.Exists(parsed.Profile, spec.Key)
		}
		keys = append(keys, buildKeyStatus(spec.Key, spec.Required, present, statusErr))
	}
	return CredentialStatus{
		Purpose:  ref.Purpose,
		Ref:      ref.Ref,
		Mode:     ref.Mode,
		Provider: ref.Provider,
		Keys:     keys,
	}, nil
}

// RequiredKeysSatisfied reports whether every required key is known-present.
func RequiredKeysSatisfied(status CredentialStatus) bool {
	for _, key := range status.Keys {
		if !key.Required {
			continue
		}
		if key.Present == nil || !*key.Present {
			return false
		}
	}
	return true
}

// MissingRequiredKeys reports required keys that are known-missing.
func MissingRequiredKeys(status CredentialStatus) []string {
	var missing []string
	for _, key := range status.Keys {
		if !key.Required || key.Status != "missing" {
			continue
		}
		missing = append(missing, key.Key)
	}
	return missing
}

func buildKeyStatus(key string, required bool, present bool, err error) KeyStatus {
	if err != nil {
		return KeyStatus{Key: key, Required: required, Status: "unknown", Error: err.Error()}
	}
	status := "missing"
	if present {
		status = "present"
	}
	return KeyStatus{Key: key, Required: required, Present: &present, Status: status}
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
