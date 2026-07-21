// Package config loads and validates cr's non-secret configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// ErrRepositoryProfileAmbiguous means repository routing matched multiple profiles.
	ErrRepositoryProfileAmbiguous = errors.New("config: repository profile ambiguous")
	// ErrSecretsStoreNotFound means the requested credential store is absent.
	ErrSecretsStoreNotFound = errors.New("config: credential store not found")
	// ErrInvalid means the config file is malformed or violates the schema.
	ErrInvalid = errors.New("config: invalid")
	// ErrUnsupported means the config uses a known v2-only option.
	ErrUnsupported = errors.New("config: not supported in v1")
)

// IsConfigSelection reports whether err identifies an invalid or missing
// configuration selection rather than a credential-store operation failure.
func IsConfigSelection(err error) bool {
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrProfileNotFound) || errors.Is(err, ErrSecretsStoreNotFound)
}

// RepositoryProfileAmbiguityError describes an ambiguous repository route match.
type RepositoryProfileAmbiguityError struct {
	Target   RepositoryTarget
	Profiles []string
}

// Error returns an actionable ambiguity message with --profile choices.
func (e RepositoryProfileAmbiguityError) Error() string {
	return fmt.Sprintf("%v: multiple repository profile routes matched %s/%s/%s; pass --profile with one of: %s", ErrRepositoryProfileAmbiguous, e.Target.Host, e.Target.Namespace, e.Target.Repo, strings.Join(e.Profiles, ", "))
}

// Unwrap supports errors.Is(err, ErrRepositoryProfileAmbiguous).
func (e RepositoryProfileAmbiguityError) Unwrap() error {
	return ErrRepositoryProfileAmbiguous
}

// File is the root config.yml schema.
type File struct {
	Secrets            SecretsConfig                     `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	RepositoryAccess   map[string]RepositoryAccessConfig `yaml:"repository_access,omitempty" json:"repository_access,omitempty"`
	LLMRuntimes        map[string]LLMConfig              `yaml:"llm_runtimes,omitempty" json:"llm_runtimes,omitempty"`
	ReviewerEntities   map[string]ReviewerEntity         `yaml:"reviewer_entities,omitempty" json:"reviewer_entities,omitempty"`
	RepositoryProfiles []RepositoryProfile               `yaml:"repository_profiles,omitempty" json:"repository_profiles,omitempty"`
	Profiles           map[string]Profile                `yaml:"profiles,omitempty" json:"profiles,omitempty"`
	Data               DataConfig                        `yaml:"data,omitempty" json:"data"`
}

// SecretsConfig carries named credential store configuration.
type SecretsConfig struct {
	Stores map[string]SecretsStore `yaml:"stores,omitempty" json:"stores,omitempty"`
}

// SecretsStore is one named configured credential store.
type SecretsStore struct {
	DisplayName string              `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Backend     SecretsStoreBackend `yaml:"backend" json:"backend"`
}

// SecretsStoreBackend carries one typed backend choice.
type SecretsStoreBackend struct {
	Kind        SecretsBackendKind             `yaml:"kind" json:"kind"`
	OnePassword *SecretsStoreOnePasswordConfig `yaml:"onepassword,omitempty" json:"onepassword,omitempty"`
}

// SecretsStoreOnePasswordConfig carries non-secret 1Password backend settings.
type SecretsStoreOnePasswordConfig struct {
	Timeout         string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	AccountID       string `yaml:"account_id,omitempty" json:"account_id,omitempty"`
	AccountURL      string `yaml:"account_url,omitempty" json:"account_url,omitempty"`
	VaultID         string `yaml:"vault_id,omitempty" json:"vault_id,omitempty"`
	VaultName       string `yaml:"vault_name,omitempty" json:"vault_name,omitempty"`
	ConnectHost     string `yaml:"connect_host,omitempty" json:"connect_host,omitempty"`
	ConnectTokenEnv string `yaml:"connect_token_env,omitempty" json:"connect_token_env,omitempty"`
	ServiceTokenEnv string `yaml:"service_token_env,omitempty" json:"service_token_env,omitempty"`
}

// SecretsBackendKind is the durable non-secret backend selector for a named
// credential store.
type SecretsBackendKind string

// Credential store constants.
const (
	LocalOSCredentialStoreID  = "local-os"
	defaultOnePasswordTimeout = "5s"
)

// EffectiveSecretsStoreSource distinguishes configured stores from the read-only
// projected built-in OS store.
type EffectiveSecretsStoreSource string

// Effective credential-store inventory sources.
const (
	EffectiveSecretsStoreSourceBuiltIn    EffectiveSecretsStoreSource = "built_in"
	EffectiveSecretsStoreSourceConfigured EffectiveSecretsStoreSource = "configured"
)

const (
	// ProjectedOSCredentialStoreBackendKind is the presentation backend for the built-in OS store.
	ProjectedOSCredentialStoreBackendKind = "auto"
)

// EffectiveSecretsStore is the presentation-safe credential-store inventory
// shape used by callers that need to summarize the config.
type EffectiveSecretsStore struct {
	ID          string                      `json:"id"`
	DisplayName string                      `json:"display_name,omitempty"`
	Backend     string                      `json:"backend"`
	ReadOnly    bool                        `json:"read_only,omitempty"`
	Source      EffectiveSecretsStoreSource `json:"source"`
}

// Profile is one named review profile.
type Profile struct {
	RepositoryAccess string          `yaml:"repository_access,omitempty" json:"repository_access,omitempty"`
	Git              GitConfig       `yaml:"-" json:"-"`
	Reviewer         ProfileReviewer `yaml:"reviewer" json:"reviewer"`
	LLMRuntime       string          `yaml:"llm_runtime" json:"llm_runtime"`
	Fast             bool            `yaml:"fast,omitempty" json:"fast,omitempty"`
	AgentSources     []string        `yaml:"agent_sources,omitempty" json:"agent_sources,omitempty"`
	ReviewPolicy     ReviewPolicy    `yaml:"review_policy,omitempty" json:"review_policy"`
	Hooks            []Hook          `yaml:"hooks,omitempty" json:"hooks,omitempty"`

	// ReviewerCredentials is the resolved effective reviewer credential block
	// derived from ReviewerEntities for runtime callers. It is not config schema.
	ReviewerCredentials *ReviewerCredentials `yaml:"-" json:"-"`
	// LLM is the resolved effective runtime derived from LLMRuntime for runtime
	// callers. It is not config schema.
	LLM LLMConfig `yaml:"-" json:"-"`
}

type profileYAML struct {
	RepositoryAccess string          `yaml:"repository_access,omitempty" json:"repository_access,omitempty"`
	Git              GitConfig       `yaml:"git,omitempty" json:"git,omitempty"`
	Reviewer         ProfileReviewer `yaml:"reviewer" json:"reviewer"`
	LLMRuntime       string          `yaml:"llm_runtime" json:"llm_runtime"`
	Fast             bool            `yaml:"fast,omitempty" json:"fast,omitempty"`
	AgentSources     []string        `yaml:"agent_sources,omitempty" json:"agent_sources,omitempty"`
	ReviewPolicy     ReviewPolicy    `yaml:"review_policy,omitempty" json:"review_policy"`
	Hooks            []Hook          `yaml:"hooks,omitempty" json:"hooks,omitempty"`
}

// Hook configures one observe-only lifecycle command. Argv is executed
// directly without a shell; timeout defaults to 30s and on_dry_run defaults
// to false. Hook commands receive the documented JSON payload on stdin and
// matching CR_* environment variables for common fields.
type Hook struct {
	Event    string   `yaml:"event" json:"event"`
	Argv     []string `yaml:"argv" json:"argv"`
	Timeout  string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	OnDryRun bool     `yaml:"on_dry_run,omitempty" json:"on_dry_run,omitempty"`
}

const defaultHookTimeout = "30s"

var hookEvents = map[string]bool{
	"run.started":            true,
	"workspace.prepared":     true,
	"dossier.ready":          true,
	"selection.completed":    true,
	"reviewer.completed":     true,
	"plan.ready":             true,
	"posting.action":         true,
	"run.completed":          true,
	"run.failed":             true,
	"respond.run.started":    true,
	"respond.plan.ready":     true,
	"respond.posting.action": true,
	"respond.run.completed":  true,
	"respond.run.failed":     true,
}

// UnmarshalYAML accepts the legacy profile git block while the data model moves
// to profiles selecting reusable repository_access entries.
func (p *Profile) UnmarshalYAML(value *yaml.Node) error {
	var raw profileYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*p = Profile{
		RepositoryAccess: raw.RepositoryAccess,
		Git:              raw.Git,
		Reviewer:         raw.Reviewer,
		LLMRuntime:       raw.LLMRuntime,
		Fast:             raw.Fast,
		AgentSources:     raw.AgentSources,
		ReviewPolicy:     raw.ReviewPolicy,
		Hooks:            raw.Hooks,
	}
	return nil
}

// MarshalYAML writes only the durable profile composition fields. Git is
// resolved from repository_access at load/runtime boundaries.
func (p Profile) MarshalYAML() (any, error) {
	return profileYAML{
		RepositoryAccess: p.RepositoryAccess,
		Reviewer:         p.Reviewer,
		LLMRuntime:       p.LLMRuntime,
		Fast:             p.Fast,
		AgentSources:     p.AgentSources,
		ReviewPolicy:     p.ReviewPolicy,
		Hooks:            p.Hooks,
	}, nil
}

// GitConfig identifies the user's git-host credentials.
type GitConfig struct {
	Provider      GitProviderKind    `yaml:"provider,omitempty" json:"provider,omitempty"`
	Host          string             `yaml:"host" json:"host"`
	AuthMode      GitAuthMode        `yaml:"auth_mode" json:"auth_mode"`
	Credential    CredentialLocation `yaml:"credential" json:"credential"`
	GitHubApp     *GitHubAppConfig   `yaml:"github_app,omitempty" json:"github_app,omitempty"`
	IdentityCache string             `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
}

// ProviderKind returns the effective git provider, defaulting to GitHub when
// the config omits the field.
func (g GitConfig) ProviderKind() GitProviderKind {
	if g.Provider == "" {
		return GitProviderGitHub
	}
	return g.Provider
}

// GitHubAppConfig stores non-secret GitHub App identifiers.
type GitHubAppConfig struct {
	AppID string `yaml:"app_id" json:"app_id"`
}

// RepositoryAccessConfig is one reusable Git host/user credential definition.
type RepositoryAccessConfig struct {
	DisplayName string    `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Git         GitConfig `yaml:"git" json:"git"`
}

// ReviewerCredentials optionally identifies separate posting credentials.
type ReviewerCredentials struct {
	AuthMode      GitAuthMode        `yaml:"auth_mode" json:"auth_mode"`
	Credential    CredentialLocation `yaml:"credential" json:"credential"`
	GitHubApp     *GitHubAppConfig   `yaml:"github_app,omitempty" json:"github_app,omitempty"`
	DisplayName   string             `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	IdentityCache string             `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
}

// ProfileReviewer selects the identity used to post review comments.
type ProfileReviewer struct {
	Kind                  ProfileReviewerKind                   `yaml:"kind" json:"kind"`
	Entity                string                                `yaml:"entity,omitempty" json:"entity,omitempty"`
	GitHubAppInstallation *ProfileReviewerGitHubAppInstallation `yaml:"github_app_installation,omitempty" json:"github_app_installation,omitempty"`
}

// ProfileReviewerKind identifies how a profile chooses its posting identity.
type ProfileReviewerKind string

// Profile reviewer kinds.
const (
	ProfileReviewerKindGitIdentity ProfileReviewerKind = "git_identity"
	ProfileReviewerKindEntity      ProfileReviewerKind = "entity"
)

// Valid reports whether k is a known profile reviewer selector.
func (k ProfileReviewerKind) Valid() bool {
	switch k {
	case ProfileReviewerKindGitIdentity, ProfileReviewerKindEntity:
		return true
	default:
		return false
	}
}

// ProfileReviewerGitHubAppInstallation selects the GitHub App installation a
// profile uses when its reviewer entity is a GitHub App.
type ProfileReviewerGitHubAppInstallation struct {
	Mode           ProfileReviewerGitHubAppInstallationMode `yaml:"mode" json:"mode"`
	InstallationID string                                   `yaml:"installation_id,omitempty" json:"installation_id,omitempty"`
}

// ProfileReviewerGitHubAppInstallationMode identifies how a profile resolves a
// GitHub App installation.
type ProfileReviewerGitHubAppInstallationMode string

// GitHub App installation resolution modes.
const (
	ProfileReviewerGitHubAppInstallationDiscoverFromRepository ProfileReviewerGitHubAppInstallationMode = "discover_from_repository"
	ProfileReviewerGitHubAppInstallationPinned                 ProfileReviewerGitHubAppInstallationMode = "pinned"
)

// Valid reports whether m is a known GitHub App installation resolution mode.
func (m ProfileReviewerGitHubAppInstallationMode) Valid() bool {
	switch m {
	case ProfileReviewerGitHubAppInstallationDiscoverFromRepository, ProfileReviewerGitHubAppInstallationPinned:
		return true
	default:
		return false
	}
}

// ReviewerEntity is one reusable configured posting identity.
type ReviewerEntity struct {
	Host          string             `yaml:"host" json:"host"`
	AuthMode      GitAuthMode        `yaml:"auth_mode" json:"auth_mode"`
	Credential    CredentialLocation `yaml:"credential" json:"credential"`
	GitHubApp     *GitHubAppConfig   `yaml:"github_app,omitempty" json:"github_app,omitempty"`
	DisplayName   string             `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	IdentityCache string             `yaml:"identity_cache,omitempty" json:"identity_cache,omitempty"`
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
	Provider          LLMProvider        `yaml:"provider" json:"provider"`
	Auth              LLMAuth            `yaml:"auth" json:"auth"`
	Adapter           LLMAdapter         `yaml:"adapter" json:"adapter"`
	Credential        CredentialLocation `yaml:"credential,omitempty" json:"credential,omitempty"`
	ModelMap          ModelMap           `yaml:"model_map,omitempty" json:"model_map,omitempty"`
	ReviewerModelTier ModelTier          `yaml:"reviewer_model_tier,omitempty" json:"reviewer_model_tier,omitempty"`
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

// GitProviderKind identifies which git-host API family a git config targets.
type GitProviderKind string

// Git provider kinds. An empty value means GitHub for backward compatibility
// with configs written before the field existed.
const (
	GitProviderGitHub GitProviderKind = "github"
	GitProviderGitLab GitProviderKind = "gitlab"
)

// Valid reports whether k is a known git provider kind. The empty value is
// valid and means GitHub.
func (k GitProviderKind) Valid() bool {
	switch k {
	case "", GitProviderGitHub, GitProviderGitLab:
		return true
	default:
		return false
	}
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

// LLMRuntimeSpec describes one built-in LLM runtime shape. An empty Auth keeps
// the runtime's existing auth validation permissive.
type LLMRuntimeSpec struct {
	Provider              LLMProvider
	Auth                  LLMAuth
	Adapter               LLMAdapter
	SuggestedName         string
	DisplayName           string
	BuiltInModelMap       ModelMap
	FastModeModels        []string
	RequiresCredentialRef bool
}

var llmRuntimeSpecs = []LLMRuntimeSpec{
	{
		Provider:      LLMProviderAnthropic,
		Auth:          LLMAuthSubscription,
		Adapter:       LLMAdapterClaudeCLI,
		SuggestedName: "claude-cli",
		DisplayName:   "Claude CLI",
		BuiltInModelMap: ModelMap{
			string(ModelTierMedium): "claude-sonnet-4-6",
			string(ModelTierLarge):  "claude-opus-4-8",
		},
		FastModeModels: []string{"claude-opus-4-8", "claude-opus-4-7"},
	},
	{
		Provider:              LLMProviderAnthropic,
		Auth:                  LLMAuthAPIKey,
		Adapter:               LLMAdapterAnthropicAPI,
		SuggestedName:         "anthropic-api-key",
		DisplayName:           "Anthropic API",
		BuiltInModelMap:       ModelMap{},
		FastModeModels:        []string{"claude-opus-4-8", "claude-opus-4-7"},
		RequiresCredentialRef: true,
	},
	{
		Provider:      LLMProviderOpenAI,
		Auth:          LLMAuthSubscription,
		Adapter:       LLMAdapterCodexCLI,
		SuggestedName: "codex-cli",
		DisplayName:   "Codex CLI",
		BuiltInModelMap: ModelMap{
			string(ModelTierSmall):  "gpt-5.4-mini",
			string(ModelTierMedium): "gpt-5.4",
			string(ModelTierLarge):  "gpt-5.5",
		},
		FastModeModels: []string{"gpt-5.4", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
	},
	{
		Provider:      LLMProviderOpenAI,
		Auth:          LLMAuthAPIKey,
		Adapter:       LLMAdapterOpenAIAPI,
		SuggestedName: "openai-api-key",
		DisplayName:   "OpenAI API",
		BuiltInModelMap: ModelMap{
			string(ModelTierSmall):  "gpt-5.4-mini",
			string(ModelTierMedium): "gpt-5.4",
			string(ModelTierLarge):  "gpt-5.5",
		},
		RequiresCredentialRef: true,
	},
	{
		Provider:        LLMProviderPi,
		Auth:            LLMAuthSubscription,
		Adapter:         LLMAdapterPiRPC,
		SuggestedName:   "pi-local",
		DisplayName:     "Pi RPC",
		BuiltInModelMap: ModelMap{},
	},
}

// LLMRuntimeSpecs returns the built-in LLM runtime specifications.
func LLMRuntimeSpecs() []LLMRuntimeSpec {
	specs := make([]LLMRuntimeSpec, len(llmRuntimeSpecs))
	for i, spec := range llmRuntimeSpecs {
		spec.BuiltInModelMap = maps.Clone(spec.BuiltInModelMap)
		spec.FastModeModels = append([]string(nil), spec.FastModeModels...)
		specs[i] = spec
	}
	return specs
}

// FindLLMRuntimeSpec finds a built-in runtime matching provider, auth, and
// adapter. A spec with empty Auth matches either supported auth mode.
func FindLLMRuntimeSpec(provider LLMProvider, auth LLMAuth, adapter LLMAdapter) (LLMRuntimeSpec, bool) {
	for _, spec := range llmRuntimeSpecs {
		if spec.Provider == provider && (spec.Auth == "" || spec.Auth == auth) && spec.Adapter == adapter {
			spec.BuiltInModelMap = maps.Clone(spec.BuiltInModelMap)
			spec.FastModeModels = append([]string(nil), spec.FastModeModels...)
			return spec, true
		}
	}
	return LLMRuntimeSpec{}, false
}

// SupportsFastMode reports whether this runtime can honor fast mode for model.
func (s LLMRuntimeSpec) SupportsFastMode(model string) bool {
	model = strings.TrimSpace(model)
	for _, supported := range s.FastModeModels {
		if model == supported {
			return true
		}
	}
	return false
}

func findLLMRuntimeSpecByProviderAdapter(provider LLMProvider, adapter LLMAdapter) (LLMRuntimeSpec, bool) {
	for _, spec := range llmRuntimeSpecs {
		if spec.Provider == provider && spec.Adapter == adapter {
			return spec, true
		}
	}
	return LLMRuntimeSpec{}, false
}

func findLLMRuntimeSpecByAdapter(adapter LLMAdapter) (LLMRuntimeSpec, bool) {
	for _, spec := range llmRuntimeSpecs {
		if spec.Adapter == adapter {
			return spec, true
		}
	}
	return LLMRuntimeSpec{}, false
}

// Valid reports whether a is a known LLM adapter.
func (a LLMAdapter) Valid() bool {
	_, ok := findLLMRuntimeSpecByAdapter(a)
	return ok
}

// BuiltInModelMap returns this CLI's provider+adapter model-tier defaults.
func BuiltInModelMap(provider LLMProvider, adapter LLMAdapter) ModelMap {
	if spec, ok := findLLMRuntimeSpecByProviderAdapter(provider, adapter); ok {
		return maps.Clone(spec.BuiltInModelMap)
	}
	return ModelMap{}
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

// CredentialLocation names where one credential bundle is stored.
type CredentialLocation struct {
	Store string `yaml:"store" json:"store"`
	Name  string `yaml:"name" json:"name"`
}

func (c CredentialLocation) normalized() CredentialLocation {
	c.Store = strings.TrimSpace(c.Store)
	c.Name = strings.TrimSpace(c.Name)
	return c
}

func (c CredentialLocation) empty() bool {
	return strings.TrimSpace(c.Store) == "" && strings.TrimSpace(c.Name) == ""
}

// CredentialRef is one declared non-secret pointer into a credential store.
// It is derived from CredentialLocation for readiness/status helpers.
type CredentialRef struct {
	Purpose  string `json:"purpose"`
	Store    string `json:"store,omitempty"`
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
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, fmt.Errorf("%w: %s", ErrNotConfigured, path)
	}
	if err != nil {
		return File{}, fmt.Errorf("config: open %s: %w", path, err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return File{}, invalid("empty config")
	}
	if yamlDocumentHasMappingPath(body, "secrets", "profiles") {
		return File{}, invalid("secrets.profiles is no longer supported; use secrets.stores")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(body))
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

	if err := Validate(cfg); err != nil {
		return File{}, err
	}
	cfg = cfg.normalized()
	return cfg, nil
}

// Save validates and atomically writes config.yml.
func Save(path string, cfg File) error {
	if strings.TrimSpace(path) == "" {
		return invalid("path is required")
	}
	if fileHasNoExplicitContent(cfg) {
		return invalid("config is empty")
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	cfg = cfg.normalized()

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

func fileHasNoExplicitContent(cfg File) bool {
	return cfg.Secrets.Stores == nil &&
		cfg.RepositoryAccess == nil &&
		cfg.LLMRuntimes == nil &&
		cfg.ReviewerEntities == nil &&
		cfg.RepositoryProfiles == nil &&
		cfg.Profiles == nil &&
		cfg.Data.Retention.MaxAgeDays == nil &&
		cfg.Data.Retention.Enforcement == ""
}

func yamlDocumentHasMappingPath(body []byte, path ...string) bool {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return false
	}
	current := &node
	if current.Kind == yaml.DocumentNode && len(current.Content) > 0 {
		current = current.Content[0]
	}
	for _, key := range path {
		current = yamlMappingValue(current, key)
		if current == nil {
			return false
		}
	}
	return true
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// Normalize returns cfg with the same normalization pass Validate and Save apply.
func Normalize(cfg File) File {
	return cfg.normalized()
}

// Validate checks a config file after defaults have been applied.
func Validate(cfg File) error {
	cfg = cfg.normalized()
	if err := ValidateSecrets(cfg.Secrets); err != nil {
		return err
	}
	if err := ValidateRetention(cfg.Data.Retention); err != nil {
		return err
	}
	for name, access := range cfg.RepositoryAccess {
		if strings.TrimSpace(name) == "" {
			return invalid("repository_access name is required")
		}
		if err := validateRepositoryAccess(cfg.Secrets, name, access); err != nil {
			return err
		}
	}
	for name, runtime := range cfg.LLMRuntimes {
		if strings.TrimSpace(name) == "" {
			return invalid("llm_runtimes name is required")
		}
		if err := validateLLMConfig(fmt.Sprintf("llm_runtimes.%s", name), runtime); err != nil {
			return err
		}
		if runtime.Auth == LLMAuthAPIKey {
			if err := validateCredentialStoreSelection(cfg.Secrets, fmt.Sprintf("llm_runtimes.%s.credential.store", name), runtime.Credential.Store); err != nil {
				return err
			}
		}
	}
	for name, entity := range cfg.ReviewerEntities {
		if strings.TrimSpace(name) == "" {
			return invalid("reviewer_entities name is required")
		}
		if err := validateReviewerEntity(cfg.Secrets, name, entity); err != nil {
			return err
		}
	}
	if len(cfg.Profiles) == 0 {
		if len(cfg.RepositoryProfiles) > 0 {
			return invalid("profiles is required")
		}
		return nil
	}
	for name, profile := range cfg.Profiles {
		if strings.TrimSpace(name) == "" {
			return invalid("profile name is required")
		}
		if err := validateProfile(cfg, name, profile); err != nil {
			return err
		}
		if err := validateProfileCredentialStoreSelections(cfg, name, profile); err != nil {
			return err
		}
	}
	if err := validateRepositoryProfiles(cfg); err != nil {
		return err
	}
	return nil
}

// EffectiveSecretsStores returns the read-only built-in OS credential store
// followed by configured credential stores in stable order.
func EffectiveSecretsStores(cfg File) []EffectiveSecretsStore {
	cfg = cfg.normalized()
	out := []EffectiveSecretsStore{{
		ID:          LocalOSCredentialStoreID,
		DisplayName: "OS credential store",
		Backend:     ProjectedOSCredentialStoreBackendKind,
		ReadOnly:    true,
		Source:      EffectiveSecretsStoreSourceBuiltIn,
	}}

	ids := make([]string, 0, len(cfg.Secrets.Stores))
	for id := range cfg.Secrets.Stores {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		store := cfg.Secrets.Stores[id]
		displayName := strings.TrimSpace(store.DisplayName)
		out = append(out, EffectiveSecretsStore{
			ID:          id,
			DisplayName: displayName,
			Backend:     string(store.Backend.Kind),
			Source:      EffectiveSecretsStoreSourceConfigured,
		})
	}
	return out
}

// ResolveProfile returns the requested profile.
func ResolveProfile(cfg File, requestedName string) (string, Profile, error) {
	cfg = cfg.normalized()
	name := strings.TrimSpace(requestedName)
	if name == "" {
		return "", Profile{}, fmt.Errorf("%w: no profile selected; pass --profile or configure a repository route", ErrProfileNotFound)
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

	repoRoutes := []RepositoryProfile{}
	namespaceRoutes := []RepositoryProfile{}
	for _, route := range cfg.RepositoryProfiles {
		if route.Match.Host != targetHost || route.Match.Namespace != targetNamespace {
			continue
		}
		if route.Match.Repos == nil {
			namespaceRoutes = append(namespaceRoutes, route)
			continue
		}
		for _, repo := range route.Match.Repos {
			if repo == targetRepo {
				repoRoutes = append(repoRoutes, route)
				break
			}
		}
	}
	if len(repoRoutes) > 0 {
		return resolveRepositoryRouteMatches(cfg, RepositoryTarget{Host: targetHost, Namespace: targetNamespace, Repo: targetRepo}, repoRoutes)
	}
	if len(namespaceRoutes) > 0 {
		return resolveRepositoryRouteMatches(cfg, RepositoryTarget{Host: targetHost, Namespace: targetNamespace, Repo: targetRepo}, namespaceRoutes)
	}
	return RepositoryProfileResolution{}, fmt.Errorf("%w: no repository profile route matched %s/%s/%s; pass --profile or configure a repository route", ErrProfileNotFound, targetHost, targetNamespace, targetRepo)
}

func resolveRepositoryRouteMatches(cfg File, target RepositoryTarget, routes []RepositoryProfile) (RepositoryProfileResolution, error) {
	profileNames := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		profileNames[route.Profile] = struct{}{}
	}
	names := make([]string, 0, len(profileNames))
	for name := range profileNames {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 1 {
		return RepositoryProfileResolution{}, RepositoryProfileAmbiguityError{
			Target:   target,
			Profiles: names,
		}
	}
	name, profile, err := ResolveProfile(cfg, names[0])
	if err != nil {
		return RepositoryProfileResolution{}, err
	}
	routeCopy := routes[0]
	return RepositoryProfileResolution{
		ProfileName:  name,
		Profile:      profile,
		Source:       RepositoryProfileResolutionSourceRoute,
		MatchedRoute: &routeCopy,
	}, nil
}

// CredentialRefs returns all credential-store refs declared by profile.
func CredentialRefs(profile Profile) ([]CredentialRef, error) {
	profile = profile.normalized()
	if err := validateEffectiveProfile("profile", profile); err != nil {
		return nil, err
	}

	refs := []CredentialRef{}
	gitRef, err := gitCredentialRef("git", profile.Git.AuthMode, profile.Git.Credential)
	if err != nil {
		return nil, err
	}
	refs = append(refs, gitRef)

	if profile.ReviewerCredentials != nil {
		reviewerRef, err := gitCredentialRef("reviewer_credentials", profile.ReviewerCredentials.AuthMode, profile.ReviewerCredentials.Credential)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reviewerRef)
	}

	if profile.LLM.Auth == LLMAuthAPIKey {
		refs = append(refs, CredentialRef{
			Purpose:  "llm",
			Store:    profile.LLM.Credential.Store,
			Ref:      profile.LLM.Credential.Name,
			Mode:     string(LLMAuthAPIKey),
			Provider: string(profile.LLM.Provider),
		})
	}
	return refs, nil
}

// PinnedGitHubAppInstallationIDForGit returns the profile-level pinned
// installation id that applies to git when git is the selected GitHub App
// reviewer entity for profile.
func PinnedGitHubAppInstallationIDForGit(profile Profile, git GitConfig) string {
	profile = profile.normalized()
	git = git.normalized()
	if git.AuthMode != GitAuthModeGitHubApp || profile.ReviewerCredentials == nil {
		return ""
	}
	if profile.Reviewer.Kind != ProfileReviewerKindEntity || profile.Reviewer.GitHubAppInstallation == nil {
		return ""
	}
	installation := profile.Reviewer.GitHubAppInstallation.normalized()
	if installation.Mode != ProfileReviewerGitHubAppInstallationPinned {
		return ""
	}
	reviewerCredentials := profile.ReviewerCredentials.normalized()
	if reviewerCredentials.AuthMode != GitAuthModeGitHubApp || !sameCredentialLocation(reviewerCredentials.Credential, git.Credential) {
		return ""
	}
	return installation.InstallationID
}

func gitCredentialRef(purpose string, mode GitAuthMode, credential CredentialLocation) (CredentialRef, error) {
	if !mode.Supported() {
		return CredentialRef{}, fmt.Errorf("%w: %s auth_mode %q", ErrUnsupported, purpose, mode)
	}
	return CredentialRef{Purpose: purpose, Store: credential.Store, Ref: credential.Name, Mode: string(mode)}, nil
}

func validateProfile(cfg File, name string, profile Profile) error {
	if strings.TrimSpace(profile.RepositoryAccess) == "" {
		return invalid("profiles.%s.repository_access is required", name)
	}
	if _, ok := cfg.RepositoryAccess[profile.RepositoryAccess]; !ok {
		return fmt.Errorf("%w: profiles.%s.repository_access %q", ErrProfileNotFound, name, profile.RepositoryAccess)
	}
	if err := validateGitConfig(fmt.Sprintf("profiles.%s.git", name), profile.Git); err != nil {
		return err
	}
	if !profile.Reviewer.Kind.Valid() {
		return invalid("profiles.%s.reviewer.kind %q is invalid", name, profile.Reviewer.Kind)
	}
	var reviewerEntity *ReviewerEntity
	switch profile.Reviewer.Kind {
	case ProfileReviewerKindGitIdentity:
		if strings.TrimSpace(profile.Reviewer.Entity) != "" {
			return invalid("profiles.%s.reviewer.entity must be empty for git_identity reviewer", name)
		}
		if profile.Reviewer.GitHubAppInstallation != nil {
			return invalid("profiles.%s.reviewer.github_app_installation must be empty for git_identity reviewer", name)
		}
	case ProfileReviewerKindEntity:
		if strings.TrimSpace(profile.Reviewer.Entity) == "" {
			return invalid("profiles.%s.reviewer.entity is required", name)
		}
		entity, ok := cfg.ReviewerEntities[profile.Reviewer.Entity]
		if !ok {
			return fmt.Errorf("%w: profiles.%s.reviewer.entity %q", ErrProfileNotFound, name, profile.Reviewer.Entity)
		}
		reviewerEntity = &entity
		if normalizeConfigHost(entity.Host) != normalizeConfigHost(profile.Git.Host) {
			return invalid("profiles.%s.reviewer.entity %q host %q must match git.host %q", name, profile.Reviewer.Entity, entity.Host, profile.Git.Host)
		}
		if sameCredentialLocation(entity.Credential, profile.Git.Credential) {
			return invalid("profiles.%s.reviewer.entity %q credential must differ from git.credential when store and name match", name, profile.Reviewer.Entity)
		}
		if profile.Git.ProviderKind() == GitProviderGitLab && entity.AuthMode != GitAuthModePAT {
			return fmt.Errorf("%w: profiles.%s.reviewer.entity %q auth_mode %q is not supported for provider gitlab; use pat", ErrUnsupported, name, profile.Reviewer.Entity, entity.AuthMode)
		}
		if err := validateProfileReviewerInstallation(name, profile.Reviewer, entity); err != nil {
			return err
		}
	}
	if strings.TrimSpace(profile.LLMRuntime) == "" {
		return invalid("profiles.%s.llm_runtime is required", name)
	}
	runtime, ok := cfg.LLMRuntimes[profile.LLMRuntime]
	if !ok {
		return fmt.Errorf("%w: profiles.%s.llm_runtime %q", ErrProfileNotFound, name, profile.LLMRuntime)
	}
	if runtime.Auth == LLMAuthAPIKey {
		if sameCredentialLocation(runtime.Credential, profile.Git.Credential) {
			return invalid("profiles.%s.llm_runtime %q credential must differ from git.credential when store and name match", name, profile.LLMRuntime)
		}
		if reviewerEntity != nil && sameCredentialLocation(runtime.Credential, reviewerEntity.Credential) {
			return invalid("profiles.%s.llm_runtime %q credential must differ from reviewer entity credential when store and name match", name, profile.LLMRuntime)
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
	if !profile.ReviewPolicy.ResolveThreads.Valid() {
		return invalid("profiles.%s.review_policy.resolve_threads %q is invalid", name, profile.ReviewPolicy.ResolveThreads)
	}
	for index, hook := range profile.Hooks {
		if err := validateHook(fmt.Sprintf("profiles.%s.hooks[%d]", name, index), hook); err != nil {
			return err
		}
	}
	return nil
}

func validateHook(field string, hook Hook) error {
	if !hookEvents[hook.Event] {
		return invalid("%s.event %q is invalid", field, hook.Event)
	}
	if len(hook.Argv) == 0 {
		return invalid("%s.argv must contain at least one argument", field)
	}
	for index, arg := range hook.Argv {
		if strings.ContainsRune(arg, '\x00') {
			return invalid("%s.argv[%d] must not contain NUL", field, index)
		}
	}
	if strings.TrimSpace(hook.Argv[0]) == "" {
		return invalid("%s.argv[0] is required", field)
	}
	timeout, err := time.ParseDuration(hook.Timeout)
	if err != nil || timeout <= 0 {
		return invalid("%s.timeout %q is invalid", field, hook.Timeout)
	}
	return nil
}

func validateProfileReviewerInstallation(profileName string, reviewer ProfileReviewer, entity ReviewerEntity) error {
	path := fmt.Sprintf("profiles.%s.reviewer.github_app_installation", profileName)
	if entity.AuthMode != GitAuthModeGitHubApp {
		if reviewer.GitHubAppInstallation != nil {
			return invalid("%s must be empty unless reviewer.entity uses github_app auth", path)
		}
		return nil
	}
	if reviewer.GitHubAppInstallation == nil {
		return invalid("%s.mode is required for github_app reviewer entities", path)
	}
	installation := reviewer.GitHubAppInstallation.normalized()
	if !installation.Mode.Valid() {
		return invalid("%s.mode %q is invalid", path, installation.Mode)
	}
	switch installation.Mode {
	case ProfileReviewerGitHubAppInstallationDiscoverFromRepository:
		if strings.TrimSpace(installation.InstallationID) != "" {
			return invalid("%s.installation_id must be empty when mode is discover_from_repository", path)
		}
	case ProfileReviewerGitHubAppInstallationPinned:
		if strings.TrimSpace(installation.InstallationID) == "" {
			return invalid("%s.installation_id is required when mode is pinned", path)
		}
		if _, err := strconv.ParseInt(installation.InstallationID, 10, 64); err != nil {
			return invalid("%s.installation_id %q must be a decimal GitHub App installation ID", path, installation.InstallationID)
		}
	}
	return nil
}

func validateEffectiveProfile(name string, profile Profile) error {
	if err := validateGitConfig(name+".git", profile.Git); err != nil {
		return err
	}
	if profile.ReviewerCredentials != nil {
		if !profile.ReviewerCredentials.AuthMode.Valid() {
			return invalid("%s.reviewer_credentials.auth_mode %q is invalid", name, profile.ReviewerCredentials.AuthMode)
		}
		if !profile.ReviewerCredentials.AuthMode.Supported() {
			return fmt.Errorf("%w: %s.reviewer_credentials.auth_mode %q", ErrUnsupported, name, profile.ReviewerCredentials.AuthMode)
		}
		if profile.Git.ProviderKind() == GitProviderGitLab && profile.ReviewerCredentials.AuthMode != GitAuthModePAT {
			return fmt.Errorf("%w: %s.reviewer_credentials.auth_mode %q is not supported for provider gitlab; use pat", ErrUnsupported, name, profile.ReviewerCredentials.AuthMode)
		}
		if err := validateCredentialLocation(name+".reviewer_credentials.credential", profile.ReviewerCredentials.Credential); err != nil {
			return err
		}
		if err := validateGitHubAppConfig(name+".reviewer_credentials.github_app", profile.ReviewerCredentials.AuthMode, profile.ReviewerCredentials.GitHubApp); err != nil {
			return err
		}
	}
	if err := validateLLMConfig(name+".llm", profile.LLM); err != nil {
		return err
	}
	return nil
}

func validateLLMConfig(field string, llm LLMConfig) error {
	if !llm.Provider.Valid() {
		return invalid("%s.provider %q is invalid", field, llm.Provider)
	}
	if !llm.Auth.Valid() {
		return invalid("%s.auth %q is invalid", field, llm.Auth)
	}
	if !llm.Adapter.Valid() {
		return invalid("%s.adapter %q is invalid", field, llm.Adapter)
	}
	runtimeSpec, runtimeKnown := FindLLMRuntimeSpec(llm.Provider, llm.Auth, llm.Adapter)
	if llm.Provider == LLMProviderPi {
		if !runtimeKnown || runtimeSpec.Adapter != LLMAdapterPiRPC {
			return invalid("%s provider pi requires auth subscription and adapter pi_rpc", field)
		}
	}
	if llm.Adapter == LLMAdapterPiRPC {
		if !runtimeKnown {
			return invalid("%s adapter pi_rpc requires provider pi and auth subscription", field)
		}
	}
	adapterSpec, _ := findLLMRuntimeSpecByAdapter(llm.Adapter)
	if !adapterSpec.RequiresCredentialRef && adapterSpec.Auth != "" && !runtimeKnown {
		return invalid("%s adapter %s requires provider %s and auth %s", field, llm.Adapter, adapterSpec.Provider, adapterSpec.Auth)
	}
	if llm.Auth == LLMAuthAPIKey && llm.Credential.empty() {
		return invalid("%s.credential is required for api_key auth", field)
	}
	if llm.Auth == LLMAuthSubscription && !llm.Credential.empty() {
		return invalid("%s.credential must be empty for subscription auth", field)
	}
	if llm.Auth == LLMAuthAPIKey {
		if err := validateCredentialLocation(field+".credential", llm.Credential); err != nil {
			return err
		}
	}
	for tier, model := range llm.ModelMap {
		modelTier := ModelTier(tier)
		if !modelTier.Valid() {
			return invalid("%s.model_map tier %q is invalid", field, tier)
		}
		if strings.TrimSpace(model) == "" {
			return invalid("%s.model_map.%s is required", field, tier)
		}
	}
	if llm.ReviewerModelTier != "" && !llm.ReviewerModelTier.Valid() {
		return invalid("%s.reviewer_model_tier %q is invalid; must be one of small, medium, large", field, llm.ReviewerModelTier)
	}
	return nil
}

func validateReviewerEntity(secrets SecretsConfig, name string, entity ReviewerEntity) error {
	if strings.TrimSpace(entity.Host) == "" {
		return invalid("reviewer_entities.%s.host is required", name)
	}
	if !entity.AuthMode.Valid() {
		return invalid("reviewer_entities.%s.auth_mode %q is invalid", name, entity.AuthMode)
	}
	if !entity.AuthMode.Supported() {
		return fmt.Errorf("%w: reviewer_entities.%s.auth_mode %q", ErrUnsupported, name, entity.AuthMode)
	}
	if err := validateCredentialLocation(fmt.Sprintf("reviewer_entities.%s.credential", name), entity.Credential); err != nil {
		return err
	}
	if err := validateCredentialStoreSelection(secrets, fmt.Sprintf("reviewer_entities.%s.credential.store", name), entity.Credential.Store); err != nil {
		return err
	}
	if err := validateGitHubAppConfig(fmt.Sprintf("reviewer_entities.%s.github_app", name), entity.AuthMode, entity.GitHubApp); err != nil {
		return err
	}
	if err := validateOptionalSingleLine(fmt.Sprintf("reviewer_entities.%s.display_name", name), entity.DisplayName); err != nil {
		return err
	}
	return nil
}

func validateRepositoryAccess(secrets SecretsConfig, name string, access RepositoryAccessConfig) error {
	if err := validateOptionalSingleLine(fmt.Sprintf("repository_access.%s.display_name", name), access.DisplayName); err != nil {
		return err
	}
	if err := validateGitConfig(fmt.Sprintf("repository_access.%s.git", name), access.Git); err != nil {
		return err
	}
	if err := validateCredentialStoreSelection(secrets, fmt.Sprintf("repository_access.%s.git.credential.store", name), access.Git.Credential.Store); err != nil {
		return err
	}
	return nil
}

func validateGitConfig(path string, git GitConfig) error {
	if !git.Provider.Valid() {
		return invalid("%s.provider %q is invalid: valid values are github, gitlab", path, git.Provider)
	}
	if strings.TrimSpace(git.Host) == "" {
		return invalid("%s.host is required", path)
	}
	if !git.AuthMode.Valid() {
		return invalid("%s.auth_mode %q is invalid", path, git.AuthMode)
	}
	if !git.AuthMode.Supported() {
		return fmt.Errorf("%w: %s.auth_mode %q", ErrUnsupported, path, git.AuthMode)
	}
	if git.ProviderKind() == GitProviderGitLab && git.AuthMode != GitAuthModePAT {
		return fmt.Errorf("%w: %s.auth_mode %q is not supported for provider gitlab; use pat", ErrUnsupported, path, git.AuthMode)
	}
	if err := validateCredentialLocation(path+".credential", git.Credential); err != nil {
		return err
	}
	if err := validateGitHubAppConfig(path+".github_app", git.AuthMode, git.GitHubApp); err != nil {
		return err
	}
	return nil
}

func validateGitHubAppConfig(path string, authMode GitAuthMode, app *GitHubAppConfig) error {
	if authMode != GitAuthModeGitHubApp {
		if app != nil {
			return invalid("%s must be empty unless auth_mode is github_app", path)
		}
		return nil
	}
	if app == nil {
		return invalid("%s.app_id is required for github_app auth", path)
	}
	if strings.TrimSpace(app.AppID) == "" {
		return invalid("%s.app_id is required for github_app auth", path)
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(app.AppID), 10, 64); err != nil {
		return invalid("%s.app_id %q must be a decimal GitHub App ID", path, app.AppID)
	}
	return nil
}

func validateProfileCredentialStoreSelections(cfg File, name string, profile Profile) error {
	for _, credential := range profileCredentialLocations(cfg, profile) {
		if err := validateCredentialStoreSelection(cfg.Secrets, fmt.Sprintf("profiles.%s.%s.credential.store", name, credential.path), credential.location.Store); err != nil {
			return err
		}
	}
	return nil
}

type profileCredentialLocation struct {
	path     string
	location CredentialLocation
}

func profileCredentialLocations(cfg File, profile Profile) []profileCredentialLocation {
	locations := []profileCredentialLocation{{
		path:     "git",
		location: profile.Git.Credential,
	}}
	if profile.Reviewer.Kind == ProfileReviewerKindEntity {
		entity := cfg.ReviewerEntities[profile.Reviewer.Entity]
		locations = append(locations, profileCredentialLocation{
			path:     "reviewer.entity",
			location: entity.Credential,
		})
	}
	if runtime := cfg.LLMRuntimes[profile.LLMRuntime]; runtime.Auth == LLMAuthAPIKey {
		locations = append(locations, profileCredentialLocation{
			path:     "llm_runtime",
			location: runtime.Credential,
		})
	}
	return locations
}

func validateCredentialStoreSelection(secrets SecretsConfig, field, store string) error {
	store = strings.TrimSpace(store)
	if store == "" {
		return invalid("%s is required", field)
	}
	if store == LocalOSCredentialStoreID {
		return nil
	}
	if _, ok := secrets.Stores[store]; !ok {
		return fmt.Errorf("%w: %s %q", ErrSecretsStoreNotFound, field, store)
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
		namespaceKey := routeProfileKey(host, namespace, "", route.Profile)
		if route.Match.Repos == nil {
			if previous, ok := namespaceRoutes[namespaceKey]; ok {
				return invalid("%s duplicates repository_profiles[%d] namespace route for profile %s at %s/%s", field, previous, route.Profile, host, namespace)
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
			repoKey := routeProfileKey(host, namespace, repo, route.Profile)
			if previous, ok := repoRoutes[repoKey]; ok {
				return invalid("%s duplicates repository_profiles[%d] repo route for profile %s at %s/%s/%s", field, previous, route.Profile, host, namespace, repo)
			}
			repoRoutes[repoKey] = index
		}
	}
	return nil
}

// ValidateSecrets checks non-secret named credential store config.
func ValidateSecrets(secrets SecretsConfig) error {
	secrets = secrets.normalized()
	for id, store := range secrets.Stores {
		trimmedID := strings.TrimSpace(id)
		if trimmedID == "" {
			return invalid("secrets.stores key is required")
		}
		if trimmedID != id {
			return invalid("secrets.stores.%s id must not contain surrounding whitespace", id)
		}
		if id == LocalOSCredentialStoreID {
			return invalid("secrets.stores.%s is reserved", LocalOSCredentialStoreID)
		}
		if err := validateSecretsStore(id, store); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretsStore(id string, store SecretsStore) error {
	if err := validateOptionalSingleLine(fmt.Sprintf("secrets.stores.%s.display_name", id), store.DisplayName); err != nil {
		return err
	}
	if strings.TrimSpace(string(store.Backend.Kind)) == "" {
		return invalid("secrets.stores.%s.backend.kind is required", id)
	}
	if _, err := credstore.ParseBackend(string(store.Backend.Kind)); err != nil {
		return fmt.Errorf("%w: secrets.stores.%s.backend.kind %q is invalid: %w", ErrInvalid, id, store.Backend.Kind, err)
	}
	if err := validateSecretsStoreBackend(id, store.Backend); err != nil {
		return err
	}
	return nil
}

func validateSecretsStoreBackend(id string, backend SecretsStoreBackend) error {
	if !IsOnePasswordSecretsBackend(backend.Kind) {
		return nil
	}
	onePassword := backend.OnePassword
	if onePassword == nil {
		onePassword = &SecretsStoreOnePasswordConfig{}
	}
	field := func(suffix string) string {
		return fmt.Sprintf("secrets.stores.%s.backend.onepassword.%s", id, suffix)
	}
	for suffix, value := range map[string]string{
		"account_id":        onePassword.AccountID,
		"account_url":       onePassword.AccountURL,
		"vault_id":          onePassword.VaultID,
		"vault_name":        onePassword.VaultName,
		"connect_host":      onePassword.ConnectHost,
		"connect_token_env": onePassword.ConnectTokenEnv,
		"service_token_env": onePassword.ServiceTokenEnv,
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
		// AccountID may be omitted so ByteNess can fall back to
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

func validateCredentialLocation(field string, credential CredentialLocation) error {
	credential = credential.normalized()
	if credential.Store == "" {
		return invalid("%s.store is required", field)
	}
	if credential.Name == "" {
		return invalid("%s.name is required", field)
	}
	return validateCredentialRef(field+".name", credential.Name)
}

func sameCredentialLocation(left, right CredentialLocation) bool {
	left = left.normalized()
	right = right.normalized()
	return left.Store == right.Store && left.Name == right.Name
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
	if cfg.RepositoryAccess != nil {
		accesses := make(map[string]RepositoryAccessConfig, len(cfg.RepositoryAccess))
		for name, access := range cfg.RepositoryAccess {
			accesses[strings.TrimSpace(name)] = access.normalized()
		}
		cfg.RepositoryAccess = accesses
	}
	if cfg.LLMRuntimes != nil {
		runtimes := make(map[string]LLMConfig, len(cfg.LLMRuntimes))
		for name, runtime := range cfg.LLMRuntimes {
			runtimes[strings.TrimSpace(name)] = runtime.normalized()
		}
		cfg.LLMRuntimes = runtimes
	}
	if cfg.ReviewerEntities != nil {
		entities := make(map[string]ReviewerEntity, len(cfg.ReviewerEntities))
		for name, entity := range cfg.ReviewerEntities {
			entities[strings.TrimSpace(name)] = entity.normalized()
		}
		cfg.ReviewerEntities = entities
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	cfg = cfg.projectProfileComponentReferences()
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profiles[name] = cfg.resolveProfileRuntimeFields(profile.normalized())
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

func (cfg File) projectProfileComponentReferences() File {
	repositoryAccessNamesByKey := map[string]string{}
	for name, access := range cfg.RepositoryAccess {
		repositoryAccessNamesByKey[repositoryAccessIdentityKey(access)] = name
	}
	runtimeNamesByKey := map[string]string{}
	for name, runtime := range cfg.LLMRuntimes {
		runtimeNamesByKey[llmRuntimeIdentityKey(runtime)] = name
	}
	reviewerNamesByKey := map[string]string{}
	for name, entity := range cfg.ReviewerEntities {
		reviewerNamesByKey[reviewerEntityIdentityKey(entity)] = name
	}

	profileNames := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	reviewerDisplayNameConflicts := map[string]bool{}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for _, name := range profileNames {
		profile := cfg.Profiles[name]
		profile = profile.normalized()
		if strings.TrimSpace(profile.RepositoryAccess) == "" && !profile.Git.empty() {
			access := repositoryAccessFromProfile(profile)
			key := repositoryAccessIdentityKey(access)
			accessName, ok := repositoryAccessNamesByKey[key]
			if !ok {
				if cfg.RepositoryAccess == nil {
					cfg.RepositoryAccess = map[string]RepositoryAccessConfig{}
				}
				accessName = uniqueConfigComponentName(cfg.RepositoryAccess, suggestedRepositoryAccessName(name, access))
				cfg.RepositoryAccess[accessName] = access
				repositoryAccessNamesByKey[key] = accessName
			}
			profile.RepositoryAccess = accessName
		}
		if strings.TrimSpace(profile.LLMRuntime) == "" && !profile.LLM.empty() {
			runtime := profile.LLM.normalized()
			key := llmRuntimeIdentityKey(runtime)
			runtimeName, ok := runtimeNamesByKey[key]
			if !ok {
				if cfg.LLMRuntimes == nil {
					cfg.LLMRuntimes = map[string]LLMConfig{}
				}
				runtimeName = uniqueConfigComponentName(cfg.LLMRuntimes, suggestedLLMRuntimeName(runtime))
				cfg.LLMRuntimes[runtimeName] = runtime
				runtimeNamesByKey[key] = runtimeName
			}
			profile.LLMRuntime = runtimeName
		}
		if !profile.Reviewer.Kind.Valid() {
			if profile.ReviewerCredentials == nil {
				profile.Reviewer.Kind = ProfileReviewerKindGitIdentity
			} else {
				entity := reviewerEntityFromProfile(profile)
				key := reviewerEntityIdentityKey(entity)
				entityName, ok := reviewerNamesByKey[key]
				if !ok {
					if cfg.ReviewerEntities == nil {
						cfg.ReviewerEntities = map[string]ReviewerEntity{}
					}
					entityName = uniqueConfigComponentName(cfg.ReviewerEntities, suggestedReviewerEntityName(name, entity))
					cfg.ReviewerEntities[entityName] = entity
					reviewerNamesByKey[key] = entityName
				} else if existing := cfg.ReviewerEntities[entityName]; !reviewerDisplayNameConflicts[entityName] {
					displayName := mergeProjectedReviewerEntityDisplayName(existing.DisplayName, entity.DisplayName)
					if displayName == "" && strings.TrimSpace(existing.DisplayName) != "" && strings.TrimSpace(entity.DisplayName) != "" && strings.TrimSpace(existing.DisplayName) != strings.TrimSpace(entity.DisplayName) {
						reviewerDisplayNameConflicts[entityName] = true
					}
					existing.DisplayName = displayName
					cfg.ReviewerEntities[entityName] = existing
				}
				profile.Reviewer = projectedProfileReviewerForEntity(entityName, entity.AuthMode)
			}
		}
		profiles[name] = profile
	}
	cfg.Profiles = profiles
	return cfg
}

func projectedProfileReviewerForEntity(entityName string, authMode GitAuthMode) ProfileReviewer {
	reviewer := ProfileReviewer{Kind: ProfileReviewerKindEntity, Entity: strings.TrimSpace(entityName)}
	if authMode == GitAuthModeGitHubApp {
		reviewer.GitHubAppInstallation = &ProfileReviewerGitHubAppInstallation{
			Mode: ProfileReviewerGitHubAppInstallationDiscoverFromRepository,
		}
	}
	return reviewer
}

func mergeProjectedReviewerEntityDisplayName(existing, next string) string {
	existing = strings.TrimSpace(existing)
	next = strings.TrimSpace(next)
	switch {
	case existing == "":
		return next
	case next == "":
		return existing
	case existing == next:
		return existing
	default:
		return ""
	}
}

func uniqueConfigComponentName[T any](existing map[string]T, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "component"
	}
	if _, ok := existing[base]; !ok {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
	}
}

func llmRuntimeIdentityKey(llm LLMConfig) string {
	llm = llm.normalized()
	modelKeys := make([]string, 0, len(llm.ModelMap))
	for tier := range llm.ModelMap {
		modelKeys = append(modelKeys, tier)
	}
	sort.Strings(modelKeys)
	models := make([]string, 0, len(modelKeys))
	for _, tier := range modelKeys {
		models = append(models, tier+"="+strings.TrimSpace(llm.ModelMap[tier]))
	}
	return strings.Join([]string{
		string(llm.Provider),
		string(llm.Auth),
		string(llm.Adapter),
		llm.Credential.Store,
		llm.Credential.Name,
		strings.Join(models, "\x1f"),
		string(llm.ReviewerModelTier),
	}, "\x00")
}

func suggestedLLMRuntimeName(llm LLMConfig) string {
	if spec, ok := FindLLMRuntimeSpec(llm.Provider, llm.Auth, llm.Adapter); ok &&
		(spec.Auth == llm.Auth || spec.Auth == "" && llm.Auth == LLMAuthSubscription) {
		return spec.SuggestedName
	}
	name := strings.Trim(strings.Join([]string{string(llm.Provider), string(llm.Auth), string(llm.Adapter)}, "-"), "-")
	if name == "" {
		return "llm-runtime"
	}
	return name
}

func reviewerEntityFromProfile(profile Profile) ReviewerEntity {
	reviewer := profile.ReviewerCredentials.normalized()
	return ReviewerEntity{
		Host:          profile.Git.Host,
		AuthMode:      reviewer.AuthMode,
		Credential:    reviewer.Credential,
		GitHubApp:     cloneGitHubAppConfig(reviewer.GitHubApp),
		DisplayName:   reviewer.DisplayName,
		IdentityCache: reviewer.IdentityCache,
	}.normalized()
}

func reviewerEntityIdentityKey(entity ReviewerEntity) string {
	entity = entity.normalized()
	return strings.Join([]string{
		entity.Host,
		string(entity.AuthMode),
		entity.Credential.Store,
		entity.Credential.Name,
		gitHubAppID(entity.GitHubApp),
	}, "\x00")
}

func gitHubAppID(app *GitHubAppConfig) string {
	if app == nil {
		return ""
	}
	return strings.TrimSpace(app.AppID)
}

func suggestedReviewerEntityName(profileName string, entity ReviewerEntity) string {
	prefix := strings.TrimSpace(profileName)
	if prefix != "" {
		return prefix + "-reviewer"
	}
	switch entity.AuthMode {
	case GitAuthModeGitHubApp:
		return "reviewer-github-app"
	case GitAuthModePAT:
		return "reviewer-pat"
	case GitAuthModeOAuthDevice:
		return "reviewer-oauth-device"
	}
	return "reviewer-entity"
}

func repositoryAccessFromProfile(profile Profile) RepositoryAccessConfig {
	return RepositoryAccessConfig{
		Git: profile.Git,
	}.normalized()
}

func repositoryAccessIdentityKey(access RepositoryAccessConfig) string {
	access = access.normalized()
	return strings.Join([]string{
		access.Git.Host,
		string(access.Git.AuthMode),
		access.Git.Credential.Store,
		access.Git.Credential.Name,
		gitHubAppID(access.Git.GitHubApp),
	}, "\x00")
}

func suggestedRepositoryAccessName(profileName string, access RepositoryAccessConfig) string {
	prefix := strings.TrimSpace(profileName)
	if prefix != "" {
		return prefix + "-git"
	}
	host := strings.ReplaceAll(access.normalized().Git.Host, ".", "-")
	if strings.TrimSpace(host) != "" {
		return host + "-git"
	}
	return "repository-access"
}

func (cfg File) resolveProfileRuntimeFields(profile Profile) Profile {
	if access, ok := cfg.RepositoryAccess[profile.RepositoryAccess]; ok {
		profile.Git = access.Git.normalized()
	}
	if runtime, ok := cfg.LLMRuntimes[profile.LLMRuntime]; ok {
		profile.LLM = runtime.normalized()
	}
	switch profile.Reviewer.Kind {
	case ProfileReviewerKindGitIdentity:
		profile.ReviewerCredentials = nil
	case ProfileReviewerKindEntity:
		if entity, ok := cfg.ReviewerEntities[profile.Reviewer.Entity]; ok {
			profile.ReviewerCredentials = entity.reviewerCredentials()
		}
	}
	return profile
}

func (s SecretsConfig) normalized() SecretsConfig {
	if s.Stores == nil {
		s.Stores = map[string]SecretsStore{}
	}
	stores := make(map[string]SecretsStore, len(s.Stores))
	for id, store := range s.Stores {
		stores[id] = store.normalized()
	}
	s.Stores = stores
	return s
}

func (s SecretsStore) normalized() SecretsStore {
	s.DisplayName = strings.TrimSpace(s.DisplayName)
	s.Backend = s.Backend.normalized()
	return s
}

func (b SecretsStoreBackend) normalized() SecretsStoreBackend {
	b.Kind = SecretsBackendKind(strings.TrimSpace(string(b.Kind)))
	if !IsOnePasswordSecretsBackend(b.Kind) {
		b.OnePassword = nil
		return b
	}
	onePassword := SecretsStoreOnePasswordConfig{}
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
		onePassword.AccountID = ""
	case SecretsBackendKind(credstore.BackendOPConnect):
		if onePassword.ConnectTokenEnv == "" {
			onePassword.ConnectTokenEnv = credstore.DefaultOnePasswordConnectTokenEnv
		}
		onePassword.ServiceTokenEnv = ""
		onePassword.AccountID = ""
	case SecretsBackendKind(credstore.BackendOPDesktop):
		onePassword.ServiceTokenEnv = ""
		onePassword.ConnectHost = ""
		onePassword.ConnectTokenEnv = ""
	}
	b.OnePassword = &onePassword
	return b
}

func (c SecretsStoreOnePasswordConfig) normalized() SecretsStoreOnePasswordConfig {
	c.Timeout = strings.TrimSpace(c.Timeout)
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.AccountURL = strings.TrimSpace(c.AccountURL)
	c.VaultID = strings.TrimSpace(c.VaultID)
	c.VaultName = strings.TrimSpace(c.VaultName)
	c.ConnectHost = strings.TrimSpace(c.ConnectHost)
	c.ConnectTokenEnv = strings.TrimSpace(c.ConnectTokenEnv)
	c.ServiceTokenEnv = strings.TrimSpace(c.ServiceTokenEnv)
	return c
}

// IsOnePasswordSecretsBackend reports whether kind is one of cr's supported
// 1Password-backed credential-store variants.
func IsOnePasswordSecretsBackend(kind SecretsBackendKind) bool {
	switch kind {
	case SecretsBackendKind(credstore.BackendOP), SecretsBackendKind(credstore.BackendOPConnect), SecretsBackendKind(credstore.BackendOPDesktop):
		return true
	default:
		return false
	}
}

func (p Profile) normalized() Profile {
	p.RepositoryAccess = strings.TrimSpace(p.RepositoryAccess)
	p.Git = p.Git.normalized()
	p.Reviewer = p.Reviewer.normalized()
	p.LLMRuntime = strings.TrimSpace(p.LLMRuntime)
	if p.ReviewerCredentials != nil {
		reviewer := p.ReviewerCredentials.normalized()
		p.ReviewerCredentials = &reviewer
	}
	p.LLM = p.LLM.normalized()
	p.ReviewPolicy = p.ReviewPolicy.normalized()
	p.Hooks = append([]Hook(nil), p.Hooks...)
	for index := range p.Hooks {
		p.Hooks[index].Event = strings.TrimSpace(p.Hooks[index].Event)
		p.Hooks[index].Argv = append([]string(nil), p.Hooks[index].Argv...)
		p.Hooks[index].Timeout = strings.TrimSpace(p.Hooks[index].Timeout)
		if p.Hooks[index].Timeout == "" {
			p.Hooks[index].Timeout = defaultHookTimeout
		}
	}
	return p
}

func (r ProfileReviewer) normalized() ProfileReviewer {
	r.Kind = ProfileReviewerKind(strings.TrimSpace(string(r.Kind)))
	r.Entity = strings.TrimSpace(r.Entity)
	if r.GitHubAppInstallation != nil {
		installation := r.GitHubAppInstallation.normalized()
		r.GitHubAppInstallation = &installation
	}
	return r
}

func (i ProfileReviewerGitHubAppInstallation) normalized() ProfileReviewerGitHubAppInstallation {
	i.Mode = ProfileReviewerGitHubAppInstallationMode(strings.TrimSpace(string(i.Mode)))
	i.InstallationID = strings.TrimSpace(i.InstallationID)
	return i
}

func (g GitConfig) normalized() GitConfig {
	g.Provider = GitProviderKind(strings.ToLower(strings.TrimSpace(string(g.Provider))))
	g.Host = normalizeConfigHost(g.Host)
	g.AuthMode = GitAuthMode(strings.TrimSpace(string(g.AuthMode)))
	g.Credential = g.Credential.normalized()
	g.GitHubApp = cloneGitHubAppConfig(g.GitHubApp)
	return g
}

func (g GitConfig) empty() bool {
	g = g.normalized()
	return g.Provider == "" &&
		g.Host == "" &&
		g.AuthMode == "" &&
		g.Credential.empty() &&
		g.GitHubApp == nil &&
		g.IdentityCache == ""
}

func (r RepositoryAccessConfig) normalized() RepositoryAccessConfig {
	r.DisplayName = strings.TrimSpace(r.DisplayName)
	r.Git = r.Git.normalized()
	return r
}

func (r ReviewerCredentials) normalized() ReviewerCredentials {
	r.Credential = r.Credential.normalized()
	r.GitHubApp = cloneGitHubAppConfig(r.GitHubApp)
	return r
}

func (r ReviewerEntity) normalized() ReviewerEntity {
	r.Host = normalizeConfigHost(r.Host)
	r.AuthMode = GitAuthMode(strings.TrimSpace(string(r.AuthMode)))
	r.Credential = r.Credential.normalized()
	r.GitHubApp = cloneGitHubAppConfig(r.GitHubApp)
	r.DisplayName = strings.TrimSpace(r.DisplayName)
	r.IdentityCache = strings.TrimSpace(r.IdentityCache)
	return r
}

func (r ReviewerEntity) reviewerCredentials() *ReviewerCredentials {
	r = r.normalized()
	return &ReviewerCredentials{
		AuthMode:      r.AuthMode,
		Credential:    r.Credential,
		GitHubApp:     cloneGitHubAppConfig(r.GitHubApp),
		DisplayName:   r.DisplayName,
		IdentityCache: r.IdentityCache,
	}
}

func cloneGitHubAppConfig(app *GitHubAppConfig) *GitHubAppConfig {
	if app == nil {
		return nil
	}
	cloned := *app
	cloned.AppID = strings.TrimSpace(cloned.AppID)
	return &cloned
}

func (l LLMConfig) normalized() LLMConfig {
	l.Credential = l.Credential.normalized()
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

func (l LLMConfig) empty() bool {
	return strings.TrimSpace(string(l.Provider)) == "" &&
		strings.TrimSpace(string(l.Auth)) == "" &&
		strings.TrimSpace(string(l.Adapter)) == "" &&
		l.Credential.empty() &&
		len(l.ModelMap) == 0 &&
		strings.TrimSpace(string(l.ReviewerModelTier)) == ""
}

func (p ReviewPolicy) normalized() ReviewPolicy {
	if p.MajorEvent == "" {
		p.MajorEvent = ReviewMajorEventRequestChanges
	}
	if p.ResolveThreads == "" {
		p.ResolveThreads = ResolveThreadsAuto
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

func routeProfileKey(host, namespace, repo, profile string) string {
	return routeKey(host, namespace, repo) + "\x00" + profile
}
