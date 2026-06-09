// Package config loads and validates cr's non-secret configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// ErrInvalid means the config file is malformed or violates the schema.
	ErrInvalid = errors.New("config: invalid")
	// ErrUnsupported means the config uses a known v2-only option.
	ErrUnsupported = errors.New("config: not supported in v1")
)

// File is the root config.yml schema.
type File struct {
	DefaultProfile string             `yaml:"default_profile" json:"default_profile"`
	Keyring        KeyringConfig      `yaml:"keyring,omitempty" json:"keyring"`
	Profiles       map[string]Profile `yaml:"profiles" json:"profiles"`
	Data           DataConfig         `yaml:"data,omitempty" json:"data"`
}

// KeyringConfig carries non-secret keyring backend preferences.
type KeyringConfig struct {
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
}

// Profile is one named review profile.
type Profile struct {
	Git                 GitConfig            `yaml:"git" json:"git"`
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
	IdentityCache string      `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
}

// LLMConfig identifies the LLM provider and adapter.
type LLMConfig struct {
	Provider      LLMProvider `yaml:"provider" json:"provider"`
	Auth          LLMAuth     `yaml:"auth" json:"auth"`
	Adapter       LLMAdapter  `yaml:"adapter" json:"adapter"`
	CredentialRef string      `yaml:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	ModelMap      ModelMap    `yaml:"model_map,omitempty" json:"model_map,omitempty"`
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
	return m == GitAuthModePAT
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
			string(ModelTierMedium): "sonnet",
			string(ModelTierLarge):  "opus",
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
		if err := validateProfile(name, profile.normalized()); err != nil {
			return err
		}
	}
	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, cfg.DefaultProfile)
	}
	if err := validateKeyring(cfg.Keyring); err != nil {
		return err
	}
	if err := validateRetention(cfg.Data.Retention.normalized()); err != nil {
		return err
	}
	return nil
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

func validateKeyring(keyring KeyringConfig) error {
	backend := strings.TrimSpace(keyring.Backend)
	if backend == "" {
		return nil
	}
	if _, err := credstore.ParseBackend(backend); err != nil {
		return fmt.Errorf("%w: keyring.backend %q is invalid: %w", ErrInvalid, backend, err)
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

func validateRetention(retention RetentionConfig) error {
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
	cfg.Data.Retention = cfg.Data.Retention.normalized()
	return cfg
}

func (p Profile) normalized() Profile {
	p.LLM = p.LLM.normalized()
	p.ReviewPolicy = p.ReviewPolicy.normalized()
	return p
}

func (l LLMConfig) normalized() LLMConfig {
	l.CredentialRef = strings.TrimSpace(l.CredentialRef)
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
