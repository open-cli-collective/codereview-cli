// Package credentials adapts cli-common/credstore to cr's command surface.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

const (
	// ServiceName is the credential-ref service segment owned by cr.
	ServiceName = "codereview"

	// BackendSourceSecretsProfile is retained for old callers that still use
	// the previous secrets-profile naming.
	BackendSourceSecretsProfile credstore.Source = "secrets_profile"
	// BackendSourceCredentialStore records that an explicit credential-store
	// destination selected the active credential backend.
	// #nosec G101 -- this is a backend source label, not a secret value.
	BackendSourceCredentialStore credstore.Source = "credential_store"

	// GitTokenKey stores the Git host access token for PAT auth.
	GitTokenKey = "git_token"
	// GitHubAppIDKey is a removed legacy key name. App IDs now belong to
	// non-secret config, not credential bundles.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	GitHubAppIDKey = "github_app_id"
	// GitHubAppPrivateKeyKey stores the GitHub App PEM private key.
	// #nosec G101 -- this is a keyring item name, not a secret value.
	GitHubAppPrivateKeyKey = "github_app_private_key"
	// GitHubAppInstallationIDKey is a removed legacy key name. Installation
	// routing now belongs to review profile config, not credential bundles.
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
	// ErrWrongService means a credential name points at another CLI's keyring namespace.
	ErrWrongService = errors.New("credentials: credential name uses wrong service")
	// ErrInvalidBackendSelection means a CLI/config backend selector was malformed.
	ErrInvalidBackendSelection = errors.New("credentials: invalid backend selection")
)

// Reader is the minimal read-only credential surface used by runtime consumers.
type Reader interface {
	Get(profile, key string) (string, error)
}

type storeReader struct {
	store *credstore.Store
}

// NewStoreReader adapts a credstore-backed implementation to the read-only seam.
func NewStoreReader(store *credstore.Store) Reader {
	if store == nil {
		return nil
	}
	return storeReader{store: store}
}

func (r storeReader) Get(profile, key string) (string, error) {
	return r.store.Get(profile, key)
}

// CachedReader marks readers that scope reads through one cache-backed store reader.
type CachedReader interface {
	Reader
	CacheStoreID() string
}

type readCacheKey struct {
	profile string
	key     string
}

type inflightRead struct {
	ready chan struct{}
	value string
	err   error
}

type cachingReader struct {
	storeID string
	base    Reader
	logger  *progress.Logger
	command string
	backend string
	closed  bool

	mu       sync.Mutex
	cached   map[readCacheKey]string
	inflight map[readCacheKey]*inflightRead
}

// CachingReader wraps one underlying reader with a process-local read-through cache.
// Runtime assembly creates one wrapper per resolved store, so cache entries are
// isolated by reader instance and keyed by profile/key within that instance.
func CachingReader(storeID string, base Reader) CachedReader {
	return newCachingReader(storeID, base, nil, "", "")
}

func newCachingReader(storeID string, base Reader, logger *progress.Logger, command, backend string) CachedReader {
	if base == nil {
		return nil
	}
	reader := &cachingReader{
		storeID:  strings.TrimSpace(storeID),
		base:     base,
		logger:   logger,
		command:  strings.TrimSpace(command),
		backend:  strings.TrimSpace(backend),
		cached:   map[readCacheKey]string{},
		inflight: map[readCacheKey]*inflightRead{},
	}
	return reader
}

func (r *cachingReader) CacheStoreID() string {
	return r.storeID
}

func (r *cachingReader) Get(profile, key string) (value string, err error) {
	cacheKey := readCacheKey{
		profile: profile,
		key:     key,
	}
	target := secretTarget(r.backend, profile, key)
	cacheHit := false

	if r.logger != nil {
		span := r.logger.Start(r.command, "read_secret_cache", target)
		defer func() {
			span.EndFields(err, progress.Field{Key: "cache_hit", Value: fmt.Sprintf("%t", cacheHit)})
		}()
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		err = credstore.ErrStoreClosed
		return "", err
	}
	if cachedValue, ok := r.cached[cacheKey]; ok {
		r.mu.Unlock()
		cacheHit = true
		return cachedValue, nil
	}
	if read, ok := r.inflight[cacheKey]; ok {
		r.mu.Unlock()
		<-read.ready
		return read.value, read.err
	}
	read := &inflightRead{ready: make(chan struct{})}
	r.inflight[cacheKey] = read
	r.mu.Unlock()

	value, err = r.base.Get(profile, key)

	r.mu.Lock()
	delete(r.inflight, cacheKey)
	if err == nil {
		r.cached[cacheKey] = value
	}
	read.value = value
	read.err = err
	close(read.ready)
	r.mu.Unlock()

	return value, err
}

func (r *cachingReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cached = map[readCacheKey]string{}
	return nil
}

// AllowedKeys is cr's keyring write allowlist.
func AllowedKeys() []string {
	return []string{GitTokenKey, GitHubAppPrivateKeyKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey}
}

// Ref is a parsed cr credential name.
type Ref struct {
	Service string
	Profile string
	Full    string
}

// SecretsProfileSelectionSource identifies why a credential store was selected.
type SecretsProfileSelectionSource string

const (
	// SecretsProfileSelectionExplicit means the credential location selected a
	// configured store directly.
	SecretsProfileSelectionExplicit SecretsProfileSelectionSource = "profile"
	// SecretsProfileSelectionBuiltInOS means the credential location selected
	// the built-in OS credential store.
	SecretsProfileSelectionBuiltInOS SecretsProfileSelectionSource = "local_os"
)

// ResolvedSecretsProfile is the typed runtime store-selection result.
type ResolvedSecretsProfile struct {
	ID              string
	Label           string
	Backend         string
	Source          config.EffectiveSecretsProfileSource
	SelectionSource SecretsProfileSelectionSource
}

// ResolvedCredentialStore is the current name for ResolvedSecretsProfile.
type ResolvedCredentialStore = ResolvedSecretsProfile

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

// PlatformOSBackend returns the non-configurable OS credential backend for goos.
func PlatformOSBackend(goos string) (credstore.Backend, error) {
	switch goos {
	case "darwin":
		return credstore.BackendKeychain, nil
	case "windows":
		return credstore.BackendWinCred, nil
	case "linux":
		return credstore.BackendSecretService, nil
	default:
		return "", credstore.ErrBackendNotImplemented
	}
}

// ResolveCredentialStore resolves one explicit credential-store ID.
func ResolveCredentialStore(cfg config.File, storeID string) (ResolvedCredentialStore, error) {
	cfg = config.Normalize(cfg)
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return ResolvedCredentialStore{}, fmt.Errorf("%w: credential store is required", config.ErrInvalid)
	}
	if storeID == config.LocalOSCredentialStoreID {
		backend, err := PlatformOSBackend(runtime.GOOS)
		if err != nil {
			return ResolvedCredentialStore{}, fmt.Errorf("%w: no OS credential store backend for GOOS %q", err, runtime.GOOS)
		}
		return ResolvedCredentialStore{
			ID:              config.LocalOSCredentialStoreID,
			Label:           "OS credential store",
			Backend:         string(backend),
			Source:          config.EffectiveSecretsStoreSourceBuiltIn,
			SelectionSource: SecretsProfileSelectionBuiltInOS,
		}, nil
	}
	store, ok := cfg.Secrets.Stores[storeID]
	if !ok {
		return ResolvedCredentialStore{}, fmt.Errorf("%w: %s", config.ErrSecretsStoreNotFound, storeID)
	}
	backend, err := credstore.ParseBackend(string(store.Backend.Kind))
	if err != nil {
		return ResolvedCredentialStore{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
	}
	label := strings.TrimSpace(store.DisplayName)
	if label == "" {
		label = storeID
	}
	return ResolvedCredentialStore{
		ID:              storeID,
		Label:           label,
		Backend:         string(backend),
		Source:          config.EffectiveSecretsStoreSourceConfigured,
		SelectionSource: SecretsProfileSelectionExplicit,
	}, nil
}

// ResolveCredentialStoreForLocation resolves the credential store referenced
// by one configured credential location.
func ResolveCredentialStoreForLocation(cfg config.File, location config.CredentialLocation) (ResolvedCredentialStore, error) {
	return ResolveCredentialStore(cfg, location.Store)
}

// ResolveSecretsProfileForProfile resolves the effective credential store for
// one review profile.
func ResolveSecretsProfileForProfile(cfg config.File, profile config.Profile) (ResolvedSecretsProfile, error) {
	if selection := strings.TrimSpace(profile.SecretsProfile); selection != "" {
		return ResolveCredentialStore(cfg, selection)
	}
	profile = config.Normalize(config.File{
		Profiles: map[string]config.Profile{"profile": profile},
	}).Profiles["profile"]
	return ResolveCredentialStoreForLocation(cfg, profile.Git.Credential)
}

// ResolveSecretsProfileForRef resolves the effective credential store for a
// low-level credential-name write/read, optionally narrowed by the global
// --profile selection.
func ResolveSecretsProfileForRef(cfg config.File, ref string, selectedProfile string) (ResolvedSecretsProfile, error) {
	selectedProfile = strings.TrimSpace(selectedProfile)
	if selectedProfile != "" {
		profile, ok := cfg.Profiles[selectedProfile]
		if !ok {
			return ResolvedSecretsProfile{}, fmt.Errorf("%w: %s", config.ErrProfileNotFound, selectedProfile)
		}
		matches := matchingCredentialRefs(profile, ref)
		if len(matches) == 0 {
			owners := profilesDeclaringCredentialRef(cfg, ref)
			if len(owners) == 0 {
				return ResolvedSecretsProfile{}, fmt.Errorf("%w: credential name %q is not declared by selected profile %q", config.ErrInvalid, ref, selectedProfile)
			}
			return ResolvedSecretsProfile{}, fmt.Errorf("%w: credential name %q is not declared by selected profile %q; declared by %s", config.ErrInvalid, ref, selectedProfile, strings.Join(ownerNames(owners), ", "))
		}
		return ResolveCredentialStore(cfg, matches[0].Store)
	}

	owners := profilesDeclaringCredentialRef(cfg, ref)
	if len(owners) == 0 {
		return ResolvedSecretsProfile{}, fmt.Errorf("%w: credential name %q is not declared by any profile", config.ErrInvalid, ref)
	}
	matches := matchingCredentialRefs(owners[0].Profile, ref)
	resolved, err := ResolveCredentialStore(cfg, matches[0].Store)
	if err != nil {
		return ResolvedSecretsProfile{}, err
	}
	for _, owner := range owners[1:] {
		nextMatches := matchingCredentialRefs(owner.Profile, ref)
		next, err := ResolveCredentialStore(cfg, nextMatches[0].Store)
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
// credential store.
func StoreOptionsForResolvedProfile(flagValue string, flagSet bool, cfg config.File, resolved ResolvedSecretsProfile) (credstore.Options, error) {
	return StoreOptionsForResolvedStore(flagValue, flagSet, cfg, resolved)
}

// StoreOptionsForResolvedStore builds credstore options for one resolved
// credential store.
func StoreOptionsForResolvedStore(_ string, flagSet bool, cfg config.File, resolved ResolvedCredentialStore) (credstore.Options, error) {
	if flagSet {
		return credstore.Options{}, fmt.Errorf("%w: --backend conflicts with credential store %q", config.ErrInvalid, resolved.DisplayName())
	}
	backend, err := credstore.ParseBackend(resolved.Backend)
	if err != nil {
		return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
	}
	options := credstore.Options{
		AllowedKeys: AllowedKeys(),
		Backend:     backend,
	}
	if resolved.Source != config.EffectiveSecretsStoreSourceConfigured {
		return options, nil
	}
	cfg = config.Normalize(cfg)
	store, ok := cfg.Secrets.Stores[resolved.ID]
	if !ok {
		return credstore.Options{}, fmt.Errorf("%w: %s", config.ErrSecretsStoreNotFound, resolved.ID)
	}
	if config.IsOnePasswordSecretsBackend(store.Backend.Kind) {
		options.OnePassword, err = onePasswordOptionsFromConfig(store.Backend)
		if err != nil {
			return credstore.Options{}, fmt.Errorf("%w: %w", config.ErrInvalid, err)
		}
	}
	return options, nil
}

// OpenResolvedStore opens the resolved service-scoped keyring store.
func OpenResolvedStore(flagValue string, flagSet bool, cfg config.File, resolved ResolvedSecretsProfile) (*credstore.Store, error) {
	opts, err := StoreOptionsForResolvedProfile(flagValue, flagSet, cfg, resolved)
	if err != nil {
		return nil, err
	}
	store, err := credstore.Open(ServiceName, &opts)
	if err != nil {
		return nil, fmt.Errorf("credentials: opening credential store %q (%s): %w", resolved.DisplayName(), resolved.Backend, err)
	}
	return store, nil
}

// StoreOptions validates backend selectors and returns credstore options.
func StoreOptions(flagValue string, flagSet bool, _ config.File) (credstore.Options, error) {
	opts := credstore.Options{AllowedKeys: AllowedKeys()}
	if err := credstore.BindBackendFlag(&opts, flagValue, flagSet, ""); err != nil {
		return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
	}
	if err := rejectLegacyOnePasswordBackend(opts.Backend); err != nil {
		return credstore.Options{}, err
	}
	if err := rejectLegacyOnePasswordBackend(opts.ConfigBackend); err != nil {
		return credstore.Options{}, err
	}
	if opts.Backend == "" {
		if value := os.Getenv(BackendEnvVar()); value != "" {
			backend, err := credstore.ParseBackend(value)
			if err != nil {
				return credstore.Options{}, fmt.Errorf("%w: %w", ErrInvalidBackendSelection, err)
			}
			if err := rejectLegacyOnePasswordBackend(backend); err != nil {
				return credstore.Options{}, err
			}
		}
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
	onePasswordCfg := backend.OnePassword
	if onePasswordCfg == nil {
		onePasswordCfg = &config.SecretsProfileOnePasswordConfig{}
	}
	options := &credstore.OnePasswordOptions{
		VaultID:          strings.TrimSpace(onePasswordCfg.VaultID),
		ItemTitlePrefix:  strings.TrimSpace(onePasswordCfg.ItemTitlePrefix),
		ItemTag:          strings.TrimSpace(onePasswordCfg.ItemTag),
		ItemFieldTitle:   strings.TrimSpace(onePasswordCfg.ItemFieldTitle),
		ConnectHost:      strings.TrimSpace(onePasswordCfg.ConnectHost),
		ConnectTokenEnv:  strings.TrimSpace(onePasswordCfg.ConnectTokenEnv),
		ServiceTokenEnv:  strings.TrimSpace(onePasswordCfg.ServiceTokenEnv),
		DesktopAccountID: strings.TrimSpace(onePasswordCfg.DesktopAccountID),
	}
	if strings.TrimSpace(onePasswordCfg.Timeout) != "" {
		timeout, err := time.ParseDuration(onePasswordCfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid 1Password timeout %q: %w", onePasswordCfg.Timeout, err)
		}
		options.Timeout = timeout
	}
	return options, nil
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
	return fmt.Errorf("%w: credential name %q is declared by profiles using different credential stores; pass --profile to disambiguate: %s", config.ErrInvalid, ref, strings.Join(details, "; "))
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
	return fmt.Errorf("%w: backend %q is only supported through configured credential stores", ErrInvalidBackendSelection, kind)
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

// KeySpecsForPurpose returns the exact keyring keys expected for a configured credential location.
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
				{Key: GitHubAppPrivateKeyKey, Required: true},
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

// KeyForPurpose returns the single required keyring key expected for a configured credential location.
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
	gitCredential := normalizedCredentialLocation(profile.Git.Credential, profile.Git.CredentialRef)
	if gitCredential.Name == ref {
		refs = append(refs, config.CredentialRef{
			Purpose: "git",
			Store:   gitCredential.Store,
			Ref:     gitCredential.Name,
			Mode:    string(profile.Git.AuthMode),
		})
	}
	if profile.ReviewerCredentials != nil {
		reviewerCredential := normalizedCredentialLocation(profile.ReviewerCredentials.Credential, profile.ReviewerCredentials.CredentialRef)
		if reviewerCredential.Name == ref {
			refs = append(refs, config.CredentialRef{
				Purpose: "reviewer_credentials",
				Store:   reviewerCredential.Store,
				Ref:     reviewerCredential.Name,
				Mode:    string(profile.ReviewerCredentials.AuthMode),
			})
		}
	}
	llmCredential := normalizedCredentialLocation(profile.LLM.Credential, profile.LLM.CredentialRef)
	if profile.LLM.Auth == config.LLMAuthAPIKey && llmCredential.Name == ref {
		refs = append(refs, config.CredentialRef{
			Purpose:  "llm",
			Store:    llmCredential.Store,
			Ref:      llmCredential.Name,
			Mode:     string(profile.LLM.Auth),
			Provider: string(profile.LLM.Provider),
		})
	}
	return refs
}

func normalizedCredentialLocation(location config.CredentialLocation, legacyRef string) config.CredentialLocation {
	location.Store = strings.TrimSpace(location.Store)
	location.Name = strings.TrimSpace(location.Name)
	legacyRef = strings.TrimSpace(legacyRef)
	if legacyRef != "" {
		if location.Store == "" {
			location.Store = config.LocalOSCredentialStoreID
		}
		location.Name = legacyRef
	}
	return location
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
