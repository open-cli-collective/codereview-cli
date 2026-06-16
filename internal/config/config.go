// Package config loads and validates cr's non-secret configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedir"
	"gopkg.in/yaml.v3"
)

const (
	serviceName                = "codereview"
	fileName                   = "config.yml"
	dirPerm                    = 0o700
	filePerm                   = 0o600
	defaultRetentionMaxAgeDays = 90
)

var (
	// ErrNotConfigured means config.yml does not exist.
	ErrNotConfigured = errors.New("config: not configured")
	// ErrProfileNotFound means the requested profile is absent.
	ErrProfileNotFound = errors.New("config: profile not found")
	// ErrSecretsProfileNotFound means the requested secrets-management profile is absent.
	ErrSecretsProfileNotFound = errors.New("config: secrets-management profile not found")
	// ErrInvalid means the config file is malformed or violates the schema.
	ErrInvalid = errors.New("config: invalid")
	// ErrUnsupported means the config uses a known v2-only option.
	ErrUnsupported = errors.New("config: not supported in v1")
)

// File is the root config.yml schema.
type File struct {
	DefaultProfile     string              `yaml:"default_profile" json:"default_profile"`
	Keyring            KeyringConfig       `yaml:"keyring,omitempty" json:"keyring"`
	Secrets            SecretsConfig       `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	RepositoryProfiles []RepositoryProfile `yaml:"repository_profiles,omitempty" json:"repository_profiles,omitempty"`
	Profiles           map[string]Profile  `yaml:"profiles" json:"profiles"`
	Data               DataConfig          `yaml:"data,omitempty" json:"data"`
}

// KeyringConfig carries non-secret keyring backend preferences.
type KeyringConfig struct {
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
}

// SecretsConfig carries named secrets-management profile configuration.
type SecretsConfig struct {
	DefaultProfile string                    `yaml:"default_profile,omitempty" json:"default_profile,omitempty"`
	Profiles       map[string]SecretsProfile `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// SecretsProfile is one named secrets-management profile.
type SecretsProfile struct {
	Label   string                `yaml:"label,omitempty" json:"label,omitempty"`
	Backend SecretsProfileBackend `yaml:"backend" json:"backend"`
}

// SecretsProfileBackend carries one typed backend choice.
type SecretsProfileBackend struct {
	Kind        SecretsBackendKind               `yaml:"kind" json:"kind"`
	OnePassword *SecretsProfileOnePasswordConfig `yaml:"onepassword,omitempty" json:"onepassword,omitempty"`
}

// SecretsProfileOnePasswordConfig carries non-secret 1Password backend settings.
type SecretsProfileOnePasswordConfig struct {
	Timeout          string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	VaultID          string `yaml:"vault_id,omitempty" json:"vault_id,omitempty"`
	ItemTitlePrefix  string `yaml:"item_title_prefix,omitempty" json:"item_title_prefix,omitempty"`
	ItemTag          string `yaml:"item_tag,omitempty" json:"item_tag,omitempty"`
	ItemFieldTitle   string `yaml:"item_field_title,omitempty" json:"item_field_title,omitempty"`
	ConnectHost      string `yaml:"connect_host,omitempty" json:"connect_host,omitempty"`
	ConnectTokenEnv  string `yaml:"connect_token_env,omitempty" json:"connect_token_env,omitempty"`
	ServiceTokenEnv  string `yaml:"service_token_env,omitempty" json:"service_token_env,omitempty"`
	DesktopAccountID string `yaml:"desktop_account_id,omitempty" json:"desktop_account_id,omitempty"`
}

// SecretsBackendKind is the durable non-secret backend selector for a
// named secrets-management profile.
type SecretsBackendKind string

// EffectiveSecretsProfileSource distinguishes configured profiles from the
// read-only projected legacy compatibility entry.
type EffectiveSecretsProfileSource string

const (
	// LegacyProjectedSecretsProfileID is the reserved effective id used when an
	// old config still relies on legacy keyring.backend behavior.
	LegacyProjectedSecretsProfileID = "legacy-default"
	// ProjectedLegacySecretsBackendKind is the effective backend summary when the
	// old config omits keyring.backend and still relies on auto/env resolution.
	ProjectedLegacySecretsBackendKind = "auto"
	defaultOnePasswordTimeout         = "5s"
)

// Effective secrets-profile inventory sources.
const (
	EffectiveSecretsProfileSourceConfigured      EffectiveSecretsProfileSource = "configured"
	EffectiveSecretsProfileSourceProjectedLegacy EffectiveSecretsProfileSource = "projected_legacy"
)

// EffectiveSecretsProfile is the presentation-safe effective secrets-management
// inventory shape used by callers that need to summarize the config.
type EffectiveSecretsProfile struct {
	ID        string                        `json:"id"`
	Label     string                        `json:"label,omitempty"`
	Backend   string                        `json:"backend"`
	IsDefault bool                          `json:"is_default,omitempty"`
	Source    EffectiveSecretsProfileSource `json:"source"`
}

// Profile is one named review profile.
type Profile struct {
	Git                 GitConfig            `yaml:"git" json:"git"`
	SecretsProfile      string               `yaml:"secrets_profile,omitempty" json:"secrets_profile,omitempty"`
	ReviewerCredentials *ReviewerCredentials `yaml:"reviewer_credentials,omitempty" json:"reviewer_credentials,omitempty"`
	LLM                 LLMConfig            `yaml:"llm" json:"llm"`
	AgentSources        []string             `yaml:"agent_sources,omitempty" json:"agent_sources,omitempty"`
	ReviewPolicy        ReviewPolicy         `yaml:"review_policy,omitempty" json:"review_policy"`
}

// GitConfig identifies the user's git-host credentials.
type GitConfig struct {
	Host          string      `yaml:"host" json:"host"`
	AuthMode      GitAuthMode `yaml:"auth_mode" json:"auth_mode"`
	CredentialRef string      `yaml:"credential_ref" json:"credential_ref"`
	IdentityCache string      `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
}

// ReviewerCredentials optionally identifies separate posting credentials.
type ReviewerCredentials struct {
	AuthMode      GitAuthMode `yaml:"auth_mode" json:"auth_mode"`
	CredentialRef string      `yaml:"credential_ref" json:"credential_ref"`
	DisplayName   string      `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	IdentityCache string      `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
}

// RepositoryProfile routes repositories to profiles when --profile is omitted.
type RepositoryProfile struct {
	Profile string                 `yaml:"profile" json:"profile"`
	Match   RepositoryProfileMatch `yaml:"match" json:"match"`
}

// RepositoryProfileMatch identifies a provider namespace and optional repos.
type RepositoryProfileMatch struct {
	Host      string   `yaml:"host" json:"host"`
	Namespace string   `yaml:"namespace" json:"namespace"`
	Repos     []string `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// LLMConfig identifies the LLM provider and adapter.
type LLMConfig struct {
	Provider          LLMProvider `yaml:"provider" json:"provider"`
	Auth              LLMAuth     `yaml:"auth" json:"auth"`
	Adapter           LLMAdapter  `yaml:"adapter" json:"adapter"`
	CredentialRef     string      `yaml:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	ModelMap          ModelMap    `yaml:"model_map,omitempty" json:"model_map,omitempty"`
	ReviewerModelTier ModelTier   `yaml:"reviewer_model_tier,omitempty" json:"reviewer_model_tier,omitempty"`
}

// ModelMap maps portable model tiers to provider-specific model identifiers.
type ModelMap map[string]string

// ModelTier is a provider-neutral model slot.
type ModelTier string

// Model tiers.
const (
	ModelTierSmall  ModelTier = "small"
	ModelTierMedium ModelTier = "medium"
	ModelTierLarge  ModelTier = "large"
)

// ModelMapSource identifies where a resolved model map value came from.
type ModelMapSource string

// Model map sources.
const (
	ModelMapSourceConfig  ModelMapSource = "config"
	ModelMapSourceBuiltIn ModelMapSource = "built_in"
)

// ModelMapResolution is one effective tier mapping.
type ModelMapResolution struct {
	Tier   ModelTier      `json:"tier"`
	Model  string         `json:"model"`
	Source ModelMapSource `json:"source"`
}

// Valid reports whether t is a known model tier.
func (t ModelTier) Valid() bool {
	switch t {
	case ModelTierSmall, ModelTierMedium, ModelTierLarge:
		return true
	default:
		return false
	}
}

// ModelTiers returns model tiers in stable display order.
func ModelTiers() []ModelTier {
	return []ModelTier{ModelTierSmall, ModelTierMedium, ModelTierLarge}
}

// ReviewPolicy carries profile-level review policy toggles.
type ReviewPolicy struct {
	MajorEvent       ReviewMajorEvent     `yaml:"major_event,omitempty" json:"major_event"`
	AllowSelfApprove bool                 `yaml:"allow_self_approve,omitempty" json:"allow_self_approve"`
	ResolveThreads   ResolveThreadsPolicy `yaml:"resolve_threads,omitempty" json:"resolve_threads,omitempty"`
	ResolveAfter     string               `yaml:"resolve_after,omitempty" json:"resolve_after,omitempty"`
}

// DataConfig carries non-secret durable data policy.
type DataConfig struct {
	Retention RetentionConfig `yaml:"retention,omitempty" json:"retention"`
}

// RetentionConfig controls run-data lifecycle behavior.
type RetentionConfig struct {
	MaxAgeDays  *int                 `yaml:"max_age_days,omitempty" json:"max_age_days"`
	Enforcement RetentionEnforcement `yaml:"enforcement,omitempty" json:"enforcement"`
}

// GitAuthMode identifies how git-host credentials are obtained.
type GitAuthMode string

// Git auth modes.
const (
	GitAuthModePAT         GitAuthMode = "pat"
	GitAuthModeOAuthDevice GitAuthMode = "oauth_device"
	GitAuthModeGitHubApp   GitAuthMode = "github_app"
)

// Valid reports whether m is a known git auth mode.
func (m GitAuthMode) Valid() bool {
	switch m {
	case GitAuthModePAT, GitAuthModeOAuthDevice, GitAuthModeGitHubApp:
		return true
	default:
		return false
	}
}

// Supported reports whether m is implemented in v1.
func (m GitAuthMode) Supported() bool {
	return m == GitAuthModePAT || m == GitAuthModeGitHubApp
}

// LLMProvider identifies the model provider family.
type LLMProvider string

// LLM providers.
const (
	LLMProviderAnthropic LLMProvider = "anthropic"
	LLMProviderOpenAI    LLMProvider = "openai"
	LLMProviderPi        LLMProvider = "pi"
)

// Valid reports whether p is a known LLM provider.
func (p LLMProvider) Valid() bool {
	switch p {
	case LLMProviderAnthropic, LLMProviderOpenAI, LLMProviderPi:
		return true
	default:
		return false
	}
}

// LLMAuth identifies how the LLM adapter authenticates.
type LLMAuth string

// LLM auth modes.
const (
	LLMAuthSubscription LLMAuth = "subscription"
	LLMAuthAPIKey       LLMAuth = "api_key"
)

// Valid reports whether a is a known LLM auth mode.
func (a LLMAuth) Valid() bool {
	switch a {
	case LLMAuthSubscription, LLMAuthAPIKey:
		return true
	default:
		return false
	}
}

// LLMAdapter identifies the concrete LLM adapter.
type LLMAdapter string

// LLM adapters.
const (
	LLMAdapterClaudeCLI    LLMAdapter = "claude_cli"
	LLMAdapterAnthropicAPI LLMAdapter = "anthropic_api"
	LLMAdapterCodexCLI     LLMAdapter = "codex_cli"
	LLMAdapterOpenAIAPI    LLMAdapter = "openai_api"
	LLMAdapterPiRPC        LLMAdapter = "pi_rpc"
)

// Valid reports whether a is a known LLM adapter.
func (a LLMAdapter) Valid() bool {
	switch a {
	case LLMAdapterClaudeCLI, LLMAdapterAnthropicAPI, LLMAdapterCodexCLI, LLMAdapterOpenAIAPI, LLMAdapterPiRPC:
		return true
	default:
		return false
	}
}

// BuiltInModelMap returns this CLI's provider+adapter model-tier defaults.
func BuiltInModelMap(provider LLMProvider, adapter LLMAdapter) ModelMap {
	switch {
	case provider == LLMProviderOpenAI && adapter == LLMAdapterCodexCLI,
		provider == LLMProviderOpenAI && adapter == LLMAdapterOpenAIAPI:
		return ModelMap{
			string(ModelTierSmall):  "gpt-5.4-mini",
			string(ModelTierMedium): "gpt-5.4",
			string(ModelTierLarge):  "gpt-5.5",
		}
	case provider == LLMProviderAnthropic && adapter == LLMAdapterClaudeCLI:
		return ModelMap{
			string(ModelTierMedium): "claude-sonnet-4-6",
			string(ModelTierLarge):  "claude-opus-4-8",
		}
	default:
		return ModelMap{}
	}
}

// EffectiveModelMap merges built-in defaults with profile model_map overrides.
func EffectiveModelMap(llm LLMConfig) map[ModelTier]ModelMapResolution {
	llm = llm.normalized()
	out := map[ModelTier]ModelMapResolution{}
	for tier, model := range BuiltInModelMap(llm.Provider, llm.Adapter) {
		model = strings.TrimSpace(model)
		parsed := ModelTier(strings.TrimSpace(tier))
		if parsed.Valid() && model != "" {
			out[parsed] = ModelMapResolution{Tier: parsed, Model: model, Source: ModelMapSourceBuiltIn}
		}
	}
	for tier, model := range llm.ModelMap {
		model = strings.TrimSpace(model)
		parsed := ModelTier(strings.TrimSpace(tier))
		if parsed.Valid() && model != "" {
			out[parsed] = ModelMapResolution{Tier: parsed, Model: model, Source: ModelMapSourceConfig}
		}
	}
	return out
}

// ResolveModelTier resolves one portable tier under the active LLM config.
func ResolveModelTier(llm LLMConfig, tier ModelTier) (ModelMapResolution, bool) {
	tier = ModelTier(strings.TrimSpace(string(tier)))
	if !tier.Valid() {
		return ModelMapResolution{}, false
	}
	resolved, ok := EffectiveModelMap(llm)[tier]
	return resolved, ok
}

// ReviewMajorEvent identifies how major findings affect the review event.
type ReviewMajorEvent string

// Review major-event policies.
const (
	ReviewMajorEventComment        ReviewMajorEvent = "comment"
	ReviewMajorEventRequestChanges ReviewMajorEvent = "request_changes"
)

// Valid reports whether e is a known major-event policy.
func (e ReviewMajorEvent) Valid() bool {
	switch e {
	case ReviewMajorEventComment, ReviewMajorEventRequestChanges:
		return true
	default:
		return false
	}
}

// ResolveThreadsPolicy identifies profile-level thread-resolution behavior.
type ResolveThreadsPolicy string

// Thread-resolution policies.
const (
	ResolveThreadsAuto  ResolveThreadsPolicy = "auto"
	ResolveThreadsNever ResolveThreadsPolicy = "never"
)

// Valid reports whether p is a known thread-resolution policy.
func (p ResolveThreadsPolicy) Valid() bool {
	switch p {
	case ResolveThreadsAuto, ResolveThreadsNever:
		return true
	default:
		return false
	}
}

// RetentionEnforcement identifies when retention is applied.
type RetentionEnforcement string

// Retention enforcement policies.
const (
	RetentionAtWrite    RetentionEnforcement = "at_write"
	RetentionManualOnly RetentionEnforcement = "manual_only"
)

// Valid reports whether e is a known retention enforcement policy.
func (e RetentionEnforcement) Valid() bool {
	switch e {
	case RetentionAtWrite, RetentionManualOnly:
		return true
	default:
		return false
	}
}

// CredentialRef is one declared non-secret pointer into the credential store.
type CredentialRef struct {
	Purpose  string `json:"purpose"`
	Ref      string `json:"ref"`
	Mode     string `json:"mode"`
	Provider string `json:"provider,omitempty"`
}

// RepositoryTarget identifies the pull-request repository used for route lookup.
type RepositoryTarget struct {
	Host      string
	Namespace string
	Repo      string
}

// RepositoryProfileResolutionSource identifies why a profile was selected for
// a repository target.
type RepositoryProfileResolutionSource string

const (
	// RepositoryProfileResolutionSourceExplicit means the inherited global
	// --profile flag explicitly bypassed repository routing.
	RepositoryProfileResolutionSourceExplicit RepositoryProfileResolutionSource = "explicit_profile"
	// RepositoryProfileResolutionSourceRoute means a repository_profiles route
	// selected the profile.
	RepositoryProfileResolutionSourceRoute RepositoryProfileResolutionSource = "repository_route"
	// RepositoryProfileResolutionSourceDefault means routing did not match and
	// the default profile was used.
	RepositoryProfileResolutionSourceDefault RepositoryProfileResolutionSource = "default_profile"
)

// RepositoryProfileResolution describes the resolved profile plus the source of
// that decision.
type RepositoryProfileResolution struct {
	ProfileName  string
	Profile      Profile
	Source       RepositoryProfileResolutionSource
	MatchedRoute *RepositoryProfile
}

// Path resolves the default cr config.yml path without creating it.
func Path() (string, error) {
	dir, err := (statedir.Scope{Name: serviceName}).ConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve config dir: %w", err)
	}
	return filepath.Join(dir, fileName), nil
}

// Load reads and validates config.yml.
func Load(path string) (File, error) {
	// #nosec G304 -- path is the resolved cr config path or an injected test path.
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return File{}, fmt.Errorf("%w: %s", ErrNotConfigured, path)
	}
	if err != nil {
		return File{}, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var cfg File
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return File{}, invalid("empty config")
		}
		return File{}, invalid("parse %s: %v", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return File{}, invalid("parse %s: %v", path, err)
	} else if err == nil {
		return File{}, invalid("parse %s: multiple YAML documents are not supported", path)
	}

	cfg = cfg.normalized()
	if err := Validate(cfg); err != nil {
		return File{}, err
	}
	return cfg, nil
}

// Save validates and atomically writes config.yml.
func Save(path string, cfg File) error {
	if strings.TrimSpace(path) == "" {
		return invalid("path is required")
	}
	cfg = cfg.normalized()
	if err := Validate(cfg); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("config: create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp config: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: chmod temp config: %w", err)
	}
	encoder := yaml.NewEncoder(tmp)
	encoder.SetIndent(2)
	if err := encoder.Encode(cfg); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: finish config encode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: replace config: %w", err)
	}
	renamed = true
	return nil
}

// Validate checks a config file after defaults have been applied.
func Validate(cfg File) error {
	cfg = cfg.normalized()
	if strings.TrimSpace(cfg.DefaultProfile) == "" {
		return invalid("default_profile is required")
	}
	if len(cfg.Profiles) == 0 {
		return invalid("profiles is required")
	}
	for name, profile := range cfg.Profiles {
		if strings.TrimSpace(name) == "" {
			return invalid("profile name is required")
		}
		profile = profile.normalized()
		if err := validateProfile(name, profile); err != nil {
			return err
		}
		if err := validateProfileSecretsProfileSelection(cfg.Secrets, name, profile); err != nil {
			return err
		}
	}
	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, cfg.DefaultProfile)
	}
	if err := validateRepositoryProfiles(cfg); err != nil {
		return err
	}
	if err := ValidateKeyring(cfg.Keyring); err != nil {
		return err
	}
	if err := ValidateSecrets(cfg.Secrets); err != nil {
		return err
	}
	if err := ValidateRetention(cfg.Data.Retention); err != nil {
		return err
	}
	return nil
}

// EffectiveSecretsProfiles returns the configured secrets-management profiles or
// a projected read-only legacy profile when config still relies on the old
// keyring.backend model.
func EffectiveSecretsProfiles(cfg File) []EffectiveSecretsProfile {
	cfg = cfg.normalized()
	if len(cfg.Secrets.Profiles) == 0 {
		backend := strings.TrimSpace(cfg.Keyring.Backend)
		if backend == "" {
			backend = ProjectedLegacySecretsBackendKind
		}
		return []EffectiveSecretsProfile{{
			ID:        LegacyProjectedSecretsProfileID,
			Label:     "Legacy default",
			Backend:   backend,
			IsDefault: true,
			Source:    EffectiveSecretsProfileSourceProjectedLegacy,
		}}
	}

	ids := make([]string, 0, len(cfg.Secrets.Profiles))
	for id := range cfg.Secrets.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	profiles := make([]EffectiveSecretsProfile, 0, len(ids))
	for _, id := range ids {
		profile := cfg.Secrets.Profiles[id]
		label := strings.TrimSpace(profile.Label)
		profiles = append(profiles, EffectiveSecretsProfile{
			ID:        id,
			Label:     label,
			Backend:   string(profile.Backend.Kind),
			IsDefault: strings.TrimSpace(cfg.Secrets.DefaultProfile) == id,
			Source:    EffectiveSecretsProfileSourceConfigured,
		})
	}
	return profiles
}

// EffectiveDefaultSecretsProfile returns the effective default secrets profile, if any.
func EffectiveDefaultSecretsProfile(cfg File) (EffectiveSecretsProfile, bool) {
	for _, profile := range EffectiveSecretsProfiles(cfg) {
		if profile.IsDefault {
			return profile, true
		}
	}
	return EffectiveSecretsProfile{}, false
}

// ResolveProfile returns the requested profile, or the default profile when
// requestedName is empty.
func ResolveProfile(cfg File, requestedName string) (string, Profile, error) {
	cfg = cfg.normalized()
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name = cfg.DefaultProfile
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return "", Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, name)
	}
	return name, profile.normalized(), nil
}

// ResolveProfileForRepository resolves the active profile for a repository.
// Explicit profile selection bypasses repository routing.
func ResolveProfileForRepository(cfg File, requestedName string, explicitProfile bool, target RepositoryTarget) (string, Profile, error) {
	resolution, err := ResolveProfileForRepositoryWithSource(cfg, requestedName, explicitProfile, target)
	if err != nil {
		return "", Profile{}, err
	}
	return resolution.ProfileName, resolution.Profile, nil
}

// ResolveProfileForRepositoryWithSource resolves the active profile for a
// repository and reports why that profile was selected.
func ResolveProfileForRepositoryWithSource(cfg File, requestedName string, explicitProfile bool, target RepositoryTarget) (RepositoryProfileResolution, error) {
	if explicitProfile {
		name, profile, err := ResolveProfile(cfg, requestedName)
		if err != nil {
			return RepositoryProfileResolution{}, err
		}
		return RepositoryProfileResolution{
			ProfileName: name,
			Profile:     profile,
			Source:      RepositoryProfileResolutionSourceExplicit,
		}, nil
	}
	cfg = cfg.normalized()
	targetHost := normalizeConfigHost(target.Host)
	targetNamespace := strings.TrimSpace(target.Namespace)
	targetRepo := strings.TrimSpace(target.Repo)
	if targetHost == "" {
		return RepositoryProfileResolution{}, invalid("repository host is required")
	}
	if targetNamespace == "" {
		return RepositoryProfileResolution{}, invalid("repository namespace is required")
	}
	if targetRepo == "" {
		return RepositoryProfileResolution{}, invalid("repository repo is required")
	}

	var namespaceRoute *RepositoryProfile
	for _, route := range cfg.RepositoryProfiles {
		if route.Match.Host != targetHost || route.Match.Namespace != targetNamespace {
			continue
		}
		if route.Match.Repos == nil {
			routeCopy := route
			namespaceRoute = &routeCopy
			continue
		}
		for _, repo := range route.Match.Repos {
			if repo == targetRepo {
				name, profile, err := ResolveProfile(cfg, route.Profile)
				if err != nil {
					return RepositoryProfileResolution{}, err
				}
				routeCopy := route
				return RepositoryProfileResolution{
					ProfileName:  name,
					Profile:      profile,
					Source:       RepositoryProfileResolutionSourceRoute,
					MatchedRoute: &routeCopy,
				}, nil
			}
		}
	}
	if namespaceRoute != nil {
		name, profile, err := ResolveProfile(cfg, namespaceRoute.Profile)
		if err != nil {
			return RepositoryProfileResolution{}, err
		}
		return RepositoryProfileResolution{
			ProfileName:  name,
			Profile:      profile,
			Source:       RepositoryProfileResolutionSourceRoute,
			MatchedRoute: namespaceRoute,
		}, nil
	}
	name, profile, err := ResolveProfile(cfg, "")
	if err != nil {
		return RepositoryProfileResolution{}, err
	}
	return RepositoryProfileResolution{
		ProfileName: name,
		Profile:     profile,
		Source:      RepositoryProfileResolutionSourceDefault,
	}, nil
}

// CredentialRefs returns all credential-store refs declared by profile.
func CredentialRefs(profile Profile) ([]CredentialRef, error) {
	profile = profile.normalized()
	if err := validateProfile("profile", profile); err != nil {
		return nil, err
	}

	refs := []CredentialRef{}
	gitRef, err := gitCredentialRef("git", profile.Git.AuthMode, profile.Git.CredentialRef)
	if err != nil {
		return nil, err
	}
	refs = append(refs, gitRef)

	if profile.ReviewerCredentials != nil {
		reviewerRef, err := gitCredentialRef("reviewer_credentials", profile.ReviewerCredentials.AuthMode, profile.ReviewerCredentials.CredentialRef)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reviewerRef)
	}

	if profile.LLM.Auth == LLMAuthAPIKey {
		refs = append(refs, CredentialRef{
			Purpose:  "llm",
			Ref:      profile.LLM.CredentialRef,
			Mode:     string(LLMAuthAPIKey),
			Provider: string(profile.LLM.Provider),
		})
	}
	return refs, nil
}

func gitCredentialRef(purpose string, mode GitAuthMode, ref string) (CredentialRef, error) {
	if !mode.Supported() {
		return CredentialRef{}, fmt.Errorf("%w: %s auth_mode %q", ErrUnsupported, purpose, mode)
	}
	return CredentialRef{Purpose: purpose, Ref: ref, Mode: string(mode)}, nil
}

func validateProfile(name string, profile Profile) error {
	if strings.TrimSpace(profile.Git.Host) == "" {
		return invalid("profiles.%s.git.host is required", name)
	}
	if !profile.Git.AuthMode.Valid() {
		return invalid("profiles.%s.git.auth_mode %q is invalid", name, profile.Git.AuthMode)
	}
	if !profile.Git.AuthMode.Supported() {
		return fmt.Errorf("%w: profiles.%s.git.auth_mode %q", ErrUnsupported, name, profile.Git.AuthMode)
	}
	if strings.TrimSpace(profile.Git.CredentialRef) == "" {
		return invalid("profiles.%s.git.credential_ref is required", name)
	}
	if err := validateCredentialRef(fmt.Sprintf("profiles.%s.git.credential_ref", name), profile.Git.CredentialRef); err != nil {
		return err
	}
	if profile.ReviewerCredentials != nil {
		if !profile.ReviewerCredentials.AuthMode.Valid() {
			return invalid("profiles.%s.reviewer_credentials.auth_mode %q is invalid", name, profile.ReviewerCredentials.AuthMode)
		}
		if !profile.ReviewerCredentials.AuthMode.Supported() {
			return fmt.Errorf("%w: profiles.%s.reviewer_credentials.auth_mode %q", ErrUnsupported, name, profile.ReviewerCredentials.AuthMode)
		}
		if strings.TrimSpace(profile.ReviewerCredentials.CredentialRef) == "" {
			return invalid("profiles.%s.reviewer_credentials.credential_ref is required", name)
		}
		if err := validateCredentialRef(fmt.Sprintf("profiles.%s.reviewer_credentials.credential_ref", name), profile.ReviewerCredentials.CredentialRef); err != nil {
			return err
		}
		if profile.ReviewerCredentials.CredentialRef == profile.Git.CredentialRef {
			return invalid("profiles.%s.reviewer_credentials.credential_ref must differ from git.credential_ref", name)
		}
		if err := validateOptionalSingleLine(fmt.Sprintf("profiles.%s.reviewer_credentials.display_name", name), profile.ReviewerCredentials.DisplayName); err != nil {
			return err
		}
	}
	if !profile.LLM.Provider.Valid() {
		return invalid("profiles.%s.llm.provider %q is invalid", name, profile.LLM.Provider)
	}
	if !profile.LLM.Auth.Valid() {
		return invalid("profiles.%s.llm.auth %q is invalid", name, profile.LLM.Auth)
	}
	if !profile.LLM.Adapter.Valid() {
		return invalid("profiles.%s.llm.adapter %q is invalid", name, profile.LLM.Adapter)
	}
	if profile.LLM.Provider == LLMProviderPi {
		if profile.LLM.Auth != LLMAuthSubscription || profile.LLM.Adapter != LLMAdapterPiRPC {
			return invalid("profiles.%s.llm provider pi requires auth subscription and adapter pi_rpc", name)
		}
	}
	if profile.LLM.Adapter == LLMAdapterPiRPC {
		if profile.LLM.Provider != LLMProviderPi || profile.LLM.Auth != LLMAuthSubscription {
			return invalid("profiles.%s.llm adapter pi_rpc requires provider pi and auth subscription", name)
		}
	}
	if profile.LLM.Adapter == LLMAdapterCodexCLI {
		if profile.LLM.Provider != LLMProviderOpenAI || profile.LLM.Auth != LLMAuthSubscription {
			return invalid("profiles.%s.llm adapter codex_cli requires provider openai and auth subscription", name)
		}
	}
	if profile.LLM.Auth == LLMAuthAPIKey && strings.TrimSpace(profile.LLM.CredentialRef) == "" {
		return invalid("profiles.%s.llm.credential_ref is required for api_key auth", name)
	}
	if profile.LLM.Auth == LLMAuthSubscription && strings.TrimSpace(profile.LLM.CredentialRef) != "" {
		return invalid("profiles.%s.llm.credential_ref must be empty for subscription auth", name)
	}
	if profile.LLM.Auth == LLMAuthAPIKey {
		if err := validateCredentialRef(fmt.Sprintf("profiles.%s.llm.credential_ref", name), profile.LLM.CredentialRef); err != nil {
			return err
		}
		if profile.LLM.CredentialRef == profile.Git.CredentialRef {
			return invalid("profiles.%s.llm.credential_ref must differ from git.credential_ref", name)
		}
		if profile.ReviewerCredentials != nil && profile.LLM.CredentialRef == profile.ReviewerCredentials.CredentialRef {
			return invalid("profiles.%s.llm.credential_ref must differ from reviewer_credentials.credential_ref", name)
		}
	}
	for tier, model := range profile.LLM.ModelMap {
		modelTier := ModelTier(tier)
		if !modelTier.Valid() {
			return invalid("profiles.%s.llm.model_map tier %q is invalid", name, tier)
		}
		if strings.TrimSpace(model) == "" {
			return invalid("profiles.%s.llm.model_map.%s is required", name, tier)
		}
	}
	if profile.LLM.ReviewerModelTier != "" && !profile.LLM.ReviewerModelTier.Valid() {
		return invalid("profiles.%s.llm.reviewer_model_tier %q is invalid; must be one of small, medium, large", name, profile.LLM.ReviewerModelTier)
	}
	for index, source := range profile.AgentSources {
		if strings.TrimSpace(source) == "" {
			return invalid("profiles.%s.agent_sources[%d] is required", name, index)
		}
	}
	if !profile.ReviewPolicy.MajorEvent.Valid() {
		return invalid("profiles.%s.review_policy.major_event %q is invalid", name, profile.ReviewPolicy.MajorEvent)
	}
	if profile.ReviewPolicy.ResolveThreads != "" && !profile.ReviewPolicy.ResolveThreads.Valid() {
		return invalid("profiles.%s.review_policy.resolve_threads %q is invalid", name, profile.ReviewPolicy.ResolveThreads)
	}
	if profile.ReviewPolicy.ResolveAfter != "" {
		if _, err := time.ParseDuration(profile.ReviewPolicy.ResolveAfter); err != nil {
			return invalid("profiles.%s.review_policy.resolve_after %q is invalid: %v", name, profile.ReviewPolicy.ResolveAfter, err)
		}
	}
	return nil
}

func validateProfileSecretsProfileSelection(secrets SecretsConfig, name string, profile Profile) error {
	selection := strings.TrimSpace(profile.SecretsProfile)
	if selection == "" {
		return nil
	}
	if selection == LegacyProjectedSecretsProfileID {
		return invalid("profiles.%s.secrets_profile %q is reserved", name, LegacyProjectedSecretsProfileID)
	}
	if _, ok := secrets.Profiles[selection]; !ok {
		return fmt.Errorf("%w: profiles.%s.secrets_profile %q", ErrSecretsProfileNotFound, name, selection)
	}
	return nil
}

func validateRepositoryProfiles(cfg File) error {
	namespaceRoutes := map[string]int{}
	repoRoutes := map[string]int{}
	for index, route := range cfg.RepositoryProfiles {
		field := fmt.Sprintf("repository_profiles[%d]", index)
		if strings.TrimSpace(route.Profile) == "" {
			return invalid("%s.profile is required", field)
		}
		profile, ok := cfg.Profiles[route.Profile]
		if !ok {
			return fmt.Errorf("%w: %s.profile %q", ErrProfileNotFound, field, route.Profile)
		}
		if strings.TrimSpace(route.Match.Host) == "" {
			return invalid("%s.match.host is required", field)
		}
		host := normalizeConfigHost(route.Match.Host)
		if host == "" {
			return invalid("%s.match.host is required", field)
		}
		if host != normalizeConfigHost(profile.Git.Host) {
			return invalid("%s.match.host %q must match profile %q git.host %q", field, route.Match.Host, route.Profile, profile.Git.Host)
		}
		namespace := strings.TrimSpace(route.Match.Namespace)
		if namespace == "" {
			return invalid("%s.match.namespace is required", field)
		}
		if route.Match.Repos != nil && len(route.Match.Repos) == 0 {
			return invalid("%s.match.repos must be omitted or contain at least one repo", field)
		}
		namespaceKey := routeKey(host, namespace, "")
		if route.Match.Repos == nil {
			if previous, ok := namespaceRoutes[namespaceKey]; ok {
				return invalid("%s duplicates repository_profiles[%d] namespace route for %s/%s", field, previous, host, namespace)
			}
			namespaceRoutes[namespaceKey] = index
			continue
		}
		seenRepos := map[string]struct{}{}
		for repoIndex, repo := range route.Match.Repos {
			repo = strings.TrimSpace(repo)
			if repo == "" {
				return invalid("%s.match.repos[%d] is required", field, repoIndex)
			}
			if _, ok := seenRepos[repo]; ok {
				return invalid("%s.match.repos[%d] duplicates repo %q", field, repoIndex, repo)
			}
			seenRepos[repo] = struct{}{}
			repoKey := routeKey(host, namespace, repo)
			if previous, ok := repoRoutes[repoKey]; ok {
				return invalid("%s duplicates repository_profiles[%d] repo route for %s/%s/%s", field, previous, host, namespace, repo)
			}
			repoRoutes[repoKey] = index
		}
	}
	return nil
}

// ValidateKeyring checks non-secret keyring backend preferences.
func ValidateKeyring(keyring KeyringConfig) error {
	backend := strings.TrimSpace(keyring.Backend)
	if backend == "" {
		return nil
	}
	if _, err := credstore.ParseBackend(backend); err != nil {
		return fmt.Errorf("%w: keyring.backend %q is invalid: %w", ErrInvalid, backend, err)
	}
	return nil
}

// ValidateSecrets checks non-secret named secrets-management profile config.
func ValidateSecrets(secrets SecretsConfig) error {
	secrets = secrets.normalized()
	if strings.TrimSpace(secrets.DefaultProfile) != "" {
		if secrets.DefaultProfile == LegacyProjectedSecretsProfileID {
			return invalid("secrets.default_profile %q is reserved", LegacyProjectedSecretsProfileID)
		}
		if _, ok := secrets.Profiles[secrets.DefaultProfile]; !ok {
			return fmt.Errorf("%w: secrets.default_profile %q", ErrProfileNotFound, secrets.DefaultProfile)
		}
	}
	for id, profile := range secrets.Profiles {
		trimmedID := strings.TrimSpace(id)
		if trimmedID == "" {
			return invalid("secrets.profiles key is required")
		}
		if trimmedID != id {
			return invalid("secrets.profiles.%s id must not contain surrounding whitespace", id)
		}
		if id == LegacyProjectedSecretsProfileID {
			return invalid("secrets.profiles.%s is reserved", LegacyProjectedSecretsProfileID)
		}
		if err := validateSecretsProfile(id, profile); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretsProfile(id string, profile SecretsProfile) error {
	if err := validateOptionalSingleLine(fmt.Sprintf("secrets.profiles.%s.label", id), profile.Label); err != nil {
		return err
	}
	if strings.TrimSpace(string(profile.Backend.Kind)) == "" {
		return invalid("secrets.profiles.%s.backend.kind is required", id)
	}
	if _, err := credstore.ParseBackend(string(profile.Backend.Kind)); err != nil {
		return fmt.Errorf("%w: secrets.profiles.%s.backend.kind %q is invalid: %w", ErrInvalid, id, profile.Backend.Kind, err)
	}
	if err := validateSecretsProfileBackend(id, profile.Backend); err != nil {
		return err
	}
	return nil
}

func validateSecretsProfileBackend(id string, backend SecretsProfileBackend) error {
	if !IsOnePasswordSecretsBackend(backend.Kind) {
		return nil
	}
	onePassword := backend.OnePassword
	if onePassword == nil {
		onePassword = &SecretsProfileOnePasswordConfig{}
	}
	field := func(suffix string) string {
		return fmt.Sprintf("secrets.profiles.%s.backend.onepassword.%s", id, suffix)
	}
	for suffix, value := range map[string]string{
		"vault_id":           onePassword.VaultID,
		"item_title_prefix":  onePassword.ItemTitlePrefix,
		"item_tag":           onePassword.ItemTag,
		"item_field_title":   onePassword.ItemFieldTitle,
		"connect_host":       onePassword.ConnectHost,
		"connect_token_env":  onePassword.ConnectTokenEnv,
		"service_token_env":  onePassword.ServiceTokenEnv,
		"desktop_account_id": onePassword.DesktopAccountID,
	} {
		if err := validateOptionalSingleLine(field(suffix), value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(onePassword.VaultID) == "" {
		return invalid("%s is required", field("vault_id"))
	}
	if strings.TrimSpace(onePassword.Timeout) != "" {
		if _, err := time.ParseDuration(onePassword.Timeout); err != nil {
			return invalid("%s %q is invalid", field("timeout"), onePassword.Timeout)
		}
	}
	switch backend.Kind {
	case SecretsBackendKind(credstore.BackendOP):
		if strings.TrimSpace(onePassword.ServiceTokenEnv) == "" {
			return invalid("%s is required", field("service_token_env"))
		}
	case SecretsBackendKind(credstore.BackendOPConnect):
		if strings.TrimSpace(onePassword.ConnectHost) == "" {
			return invalid("%s is required", field("connect_host"))
		}
		if strings.TrimSpace(onePassword.ConnectTokenEnv) == "" {
			return invalid("%s is required", field("connect_token_env"))
		}
	case SecretsBackendKind(credstore.BackendOPDesktop):
		// DesktopAccountID may be omitted so ByteNess can fall back to
		// OP_DESKTOP_ACCOUNT_ID at runtime.
	}
	return nil
}

func validateCredentialRef(field, ref string) error {
	service, _, err := credstore.ParseRef(ref)
	if err != nil {
		return fmt.Errorf("%w: %s is invalid: %w", ErrInvalid, field, err)
	}
	if service != serviceName {
		return invalid("%s must use service %q", field, serviceName)
	}
	return nil
}

func validateOptionalSingleLine(field, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return invalid("%s must be a single line", field)
	}
	return nil
}

// DefaultRetentionConfig returns the normalized durable retention defaults.
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{}.normalized()
}

// ValidateRetention checks retention after applying omitted-field defaults.
func ValidateRetention(retention RetentionConfig) error {
	retention = retention.normalized()
	if retention.MaxAgeDaysValue() < 0 {
		return invalid("data.retention.max_age_days %d is invalid", retention.MaxAgeDaysValue())
	}
	if !retention.Enforcement.Valid() {
		return invalid("data.retention.enforcement %q is invalid", retention.Enforcement)
	}
	return nil
}

func (cfg File) normalized() File {
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profiles[name] = profile.normalized()
	}
	cfg.Profiles = profiles
	if len(cfg.RepositoryProfiles) > 0 {
		routes := make([]RepositoryProfile, len(cfg.RepositoryProfiles))
		for index, route := range cfg.RepositoryProfiles {
			route.Profile = strings.TrimSpace(route.Profile)
			route.Match.Host = normalizeConfigHost(route.Match.Host)
			route.Match.Namespace = strings.TrimSpace(route.Match.Namespace)
			if len(route.Match.Repos) > 0 {
				repos := make([]string, len(route.Match.Repos))
				for repoIndex, repo := range route.Match.Repos {
					repos[repoIndex] = strings.TrimSpace(repo)
				}
				route.Match.Repos = repos
			}
			routes[index] = route
		}
		cfg.RepositoryProfiles = routes
	}
	cfg.Data.Retention = cfg.Data.Retention.normalized()
	cfg.Secrets = cfg.Secrets.normalized()
	return cfg
}

func (s SecretsConfig) normalized() SecretsConfig {
	s.DefaultProfile = strings.TrimSpace(s.DefaultProfile)
	if s.Profiles == nil {
		s.Profiles = map[string]SecretsProfile{}
	}
	profiles := make(map[string]SecretsProfile, len(s.Profiles))
	for id, profile := range s.Profiles {
		profiles[id] = profile.normalized()
	}
	s.Profiles = profiles
	return s
}

func (p SecretsProfile) normalized() SecretsProfile {
	p.Label = strings.TrimSpace(p.Label)
	p.Backend = p.Backend.normalized()
	return p
}

func (b SecretsProfileBackend) normalized() SecretsProfileBackend {
	b.Kind = SecretsBackendKind(strings.TrimSpace(string(b.Kind)))
	if !IsOnePasswordSecretsBackend(b.Kind) {
		b.OnePassword = nil
		return b
	}
	onePassword := SecretsProfileOnePasswordConfig{}
	if b.OnePassword != nil {
		onePassword = b.OnePassword.normalized()
	}
	if onePassword.Timeout == "" && (b.Kind == SecretsBackendKind(credstore.BackendOP) || b.Kind == SecretsBackendKind(credstore.BackendOPDesktop)) {
		onePassword.Timeout = defaultOnePasswordTimeout
	}
	switch b.Kind {
	case SecretsBackendKind(credstore.BackendOP):
		if onePassword.ServiceTokenEnv == "" {
			onePassword.ServiceTokenEnv = credstore.DefaultOnePasswordServiceTokenEnv
		}
		onePassword.ConnectHost = ""
		onePassword.ConnectTokenEnv = ""
		onePassword.DesktopAccountID = ""
	case SecretsBackendKind(credstore.BackendOPConnect):
		if onePassword.ConnectTokenEnv == "" {
			onePassword.ConnectTokenEnv = credstore.DefaultOnePasswordConnectTokenEnv
		}
		onePassword.ServiceTokenEnv = ""
		onePassword.DesktopAccountID = ""
	case SecretsBackendKind(credstore.BackendOPDesktop):
		onePassword.ServiceTokenEnv = ""
		onePassword.ConnectHost = ""
		onePassword.ConnectTokenEnv = ""
	}
	b.OnePassword = &onePassword
	return b
}

func (c SecretsProfileOnePasswordConfig) normalized() SecretsProfileOnePasswordConfig {
	c.Timeout = strings.TrimSpace(c.Timeout)
	c.VaultID = strings.TrimSpace(c.VaultID)
	c.ItemTitlePrefix = strings.TrimSpace(c.ItemTitlePrefix)
	c.ItemTag = strings.TrimSpace(c.ItemTag)
	c.ItemFieldTitle = strings.TrimSpace(c.ItemFieldTitle)
	c.ConnectHost = strings.TrimSpace(c.ConnectHost)
	c.ConnectTokenEnv = strings.TrimSpace(c.ConnectTokenEnv)
	c.ServiceTokenEnv = strings.TrimSpace(c.ServiceTokenEnv)
	c.DesktopAccountID = strings.TrimSpace(c.DesktopAccountID)
	return c
}

// IsOnePasswordSecretsBackend reports whether kind is one of cr's supported
// 1Password-backed secrets-profile variants.
func IsOnePasswordSecretsBackend(kind SecretsBackendKind) bool {
	switch kind {
	case SecretsBackendKind(credstore.BackendOP), SecretsBackendKind(credstore.BackendOPConnect), SecretsBackendKind(credstore.BackendOPDesktop):
		return true
	default:
		return false
	}
}

func (p Profile) normalized() Profile {
	p.SecretsProfile = strings.TrimSpace(p.SecretsProfile)
	p.LLM = p.LLM.normalized()
	p.ReviewPolicy = p.ReviewPolicy.normalized()
	return p
}

func (l LLMConfig) normalized() LLMConfig {
	l.CredentialRef = strings.TrimSpace(l.CredentialRef)
	l.ReviewerModelTier = ModelTier(strings.TrimSpace(string(l.ReviewerModelTier)))
	if len(l.ModelMap) > 0 {
		modelMap := make(ModelMap, len(l.ModelMap))
		for tier, model := range l.ModelMap {
			modelMap[strings.TrimSpace(tier)] = strings.TrimSpace(model)
		}
		l.ModelMap = modelMap
	}
	return l
}

func (p ReviewPolicy) normalized() ReviewPolicy {
	if p.MajorEvent == "" {
		p.MajorEvent = ReviewMajorEventComment
	}
	return p
}

func (r RetentionConfig) normalized() RetentionConfig {
	if r.MaxAgeDays == nil {
		maxAgeDays := defaultRetentionMaxAgeDays
		r.MaxAgeDays = &maxAgeDays
	}
	if r.Enforcement == "" {
		r.Enforcement = RetentionAtWrite
	}
	return r
}

// MaxAgeDaysValue returns the configured max age, applying the default when
// max_age_days was omitted. A configured zero still means keep forever.
func (r RetentionConfig) MaxAgeDaysValue() int {
	if r.MaxAgeDays == nil {
		return defaultRetentionMaxAgeDays
	}
	return *r.MaxAgeDays
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// NormalizeHost applies the same host normalization used by config validation
// and repository route resolution.
func NormalizeHost(raw string) string {
	return normalizeConfigHost(raw)
}

func normalizeConfigHost(raw string) string {
	host := strings.TrimSpace(raw)
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return strings.ToLower(strings.TrimSuffix(host, "/"))
}

func routeKey(host, namespace, repo string) string {
	return host + "\x00" + namespace + "\x00" + repo
}
