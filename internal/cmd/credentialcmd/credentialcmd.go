// Package credentialcmd wires credential ingress commands.
package credentialcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// Register attaches credential commands to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	rootCmd.AddCommand(newSetCredentialCommand(opts), newInitCommand(opts))
}

type setCredentialOptions struct {
	ref       string
	key       string
	stdin     bool
	fromEnv   string
	overwrite bool
	json      bool
}

func newSetCredentialCommand(opts *root.Options) *cobra.Command {
	var flags setCredentialOptions
	cmd := &cobra.Command{
		Use:   "set-credential",
		Short: "Write one secret value to cr's credential store",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("set-credential takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runSetCredential(cmd, opts, flags)
			if flags.json {
				if err != nil {
					result.Written = false
					result.Error = err.Error()
				}
				if renderErr := view.RenderCredentialWriteJSON(opts.Stdout, result); renderErr != nil && err == nil {
					return renderErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&flags.ref, "ref", "", "Credential ref (<service>/<profile>)")
	cmd.Flags().StringVar(&flags.key, "key", "", "Credential key")
	cmd.Flags().BoolVar(&flags.stdin, "stdin", false, "Read the secret value from stdin")
	cmd.Flags().StringVar(&flags.fromEnv, "from-env", "", "Read the secret value from this environment variable")
	cmd.Flags().BoolVar(&flags.overwrite, "overwrite", false, "Replace an existing credential")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Emit JSON")
	return cmd
}

func runSetCredential(cmd *cobra.Command, opts *root.Options, flags setCredentialOptions) (view.CredentialWrite, error) {
	result := view.CredentialWrite{Ref: flags.ref, Key: flags.key}
	if flags.ref == "" {
		return result, exitcode.Usage(fmt.Errorf("--ref is required"))
	}
	parsed, err := credentials.ParseRef(flags.ref)
	if err != nil {
		return result, exitcode.Usage(err)
	}
	if flags.key == "" {
		return result, exitcode.Usage(fmt.Errorf("--key is required"))
	}
	cfg, err := loadOptionalConfig(opts)
	if err != nil {
		return result, cmderr.Config(err)
	}
	if err := credentials.ValidateAllowedKeyForConfig(cfg, flags.ref, flags.key); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrUnsupported) {
			return result, cmderr.Config(err)
		}
		return result, exitcode.Usage(err)
	}
	secret, err := readSecretIngress(opts.Stdin, flags.stdin, flags.fromEnv, "--stdin", "--from-env")
	if err != nil {
		return result, exitcode.Usage(err)
	}
	store, err := credentials.OpenStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg)
	if err != nil {
		return result, cmderr.Credential(err)
	}
	defer store.Close()
	backend, source := store.Backend()
	result.Backend = string(backend)
	result.BackendSource = string(source)
	setOpts := []credstore.SetOpt{}
	if flags.overwrite {
		setOpts = append(setOpts, credstore.WithOverwrite())
	}
	if err := store.Set(parsed.Profile, flags.key, secret, setOpts...); err != nil {
		return result, cmderr.Credential(err)
	}
	result.Written = true
	if !flags.json {
		_, err = fmt.Fprintf(opts.Stderr, "wrote %s to %s via %s\n", flags.key, flags.ref, backend)
	}
	return result, err
}

type initOptions struct {
	nonInteractive     bool
	gitHost            string
	gitAuth            string
	gitRef             string
	reviewerRef        string
	reviewerAuth       string
	disableReviewer    bool
	llmProvider        string
	llmAuth            string
	llmAdapter         string
	llmRef             string
	llmReviewerTier    string
	clearLLMReviewer   bool
	agentSources       []string
	majorEvent         string
	selfApprove        bool
	resolveThreads     string
	resolveAfter       string
	gitTokenStdin      bool
	gitTokenEnv        string
	reviewerTokenStdin bool
	reviewerTokenEnv   string
	llmKeyStdin        bool
	llmKeyEnv          string
	overwrite          bool
	replaceProfile     bool
	setDefault         bool
	keyringBackend     string
	resetKeyring       bool
}

type initPrompter interface {
	Run(initPromptContext) (initDraft, error)
}

type initMenuPrompter interface {
	ChooseAction(initMenuPrompt) (initMenuAction, error)
}

type initModelMapPrompter interface {
	EditModelMap(initModelMapPrompt) (initModelMapEdit, error)
}

type initAgentSourcesPrompter interface {
	EditAgentSources(initAgentSourcesPrompt) (initAgentSourcesEdit, error)
}

type initReviewPolicyPrompter interface {
	EditReviewPolicy(initReviewPolicyPrompt) (initReviewPolicyEdit, error)
}

type initRoutesPrompter interface {
	EditRoutes(initRoutesPrompt) (initRoutesEdit, error)
}

type initRetentionPrompter interface {
	EditRetention(initRetentionPrompt) (initRetentionEdit, error)
}

type initKeyringBackendPrompter interface {
	EditKeyringBackend(initKeyringBackendPrompt) (initKeyringBackendEdit, error)
}

type initLLMRuntimePrompter interface {
	EditLLMRuntime(initLLMRuntimePrompt) (initDraft, error)
}

type initReviewerEntityPrompter interface {
	EditReviewerEntity(initReviewerEntityPrompt) (initDraft, error)
}

type initFinalizePrompter interface {
	ChooseFinalizeAction(initFinalizePrompt) (initFinalizeAction, error)
}

type initPromptContext struct {
	RequestedProfileName string
	ExistingProfileName  string
	ExistingProfile      *config.Profile
	ExistingProfileNames []string
	DefaultProfileName   string
	ExistingConfig       config.File
	GitScopes            map[string]initGitScopeDraft
	ProfileGitScopes     map[string]string
	ReviewerEntities     map[string]initReviewerEntityDraft
	ProfileReviewerEntities map[string]string
	LLMRuntimes          map[string]initLLMRuntimeDraft
	ProfileLLMRuntimes   map[string]string
	ProfileWarnings      map[string][]string
}

type initDraft struct {
	OriginalProfileName   string
	ProfileName           string
	MakeDefault           bool
	GitHost               string
	GitAuth               string
	GitCredentialRef      string
	ReviewerEnabled       bool
	ReviewerAuth          string
	ReviewerCredentialRef string
	LLMProvider           string
	LLMAuth               string
	LLMAdapter            string
	LLMReviewerModelTier  string
	LLMCredentialRef      string
	AdvancedStorageLabels bool
}

type initModelMapPrompt struct {
	LLM      config.LLMConfig
	ModelMap config.ModelMap
}

type initModelMapEdit struct {
	Apply    bool
	ModelMap config.ModelMap
}

type initAgentSourcesPrompt struct {
	Sources []string
}

type initAgentSourcesEdit struct {
	Apply   bool
	Sources []string
}

type initReviewPolicyPrompt struct {
	ReviewPolicy config.ReviewPolicy
}

type initReviewPolicyEdit struct {
	Apply        bool
	ReviewPolicy config.ReviewPolicy
}

type initRoutesPrompt struct {
	ProfileName  string
	ProfileHost  string
	PreviousHost string
	HostChanged  bool
	Routes       []configedit.RepositoryRouteSpec
}

type initRoutesEdit struct {
	Apply  bool
	Routes []configedit.RepositoryRouteSpec
}

type initRetentionPrompt struct {
	Retention config.RetentionConfig
}

type initRetentionEdit struct {
	Apply     bool
	Retention config.RetentionConfig
}

type initKeyringBackendPrompt struct {
	Backend string
}

type initKeyringBackendEdit struct {
	Apply   bool
	Backend string
}

type initFinalizeAction string

const (
	initFinalizeActionSave   initFinalizeAction = "save"
	initFinalizeActionCancel initFinalizeAction = "cancel"
)

type initProfileReadiness struct {
	ProfileName string
	Ready       bool
	Notes       []string
}

type initFinalizePrompt struct {
	Profiles []initProfileReadiness
}

type initMenuAction string

const (
	initMenuActionLLMRuntimes      initMenuAction = "llm_runtimes"
	initMenuActionReviewerEntities initMenuAction = "reviewer_entities"
	initMenuActionReviewProfiles   initMenuAction = "review_profiles"
	initMenuActionGlobalSettings   initMenuAction = "global_settings"
	initMenuActionSave             initMenuAction = "save"
	initMenuActionExit             initMenuAction = "exit"
)

type initMenuPrompt struct {
	HasWorkspace          bool
	LLMRuntimeCount       int
	ReviewerEntityCount   int
	ReviewProfileCount    int
	CanConfigureLLM       bool
	CanConfigureReviewer  bool
	CanSave               bool
	ActiveProfileName     string
}

type initLLMRuntimePrompt struct {
	Context initPromptContext
}

type initReviewerEntityPrompt struct {
	Context initPromptContext
}

type initDeps struct {
	prompter             initPrompter
	menuPrompter         initMenuPrompter
	llmRuntimePrompter   initLLMRuntimePrompter
	reviewerPrompter     initReviewerEntityPrompter
	finalizePrompter     initFinalizePrompter
	modelMapPrompter     initModelMapPrompter
	agentSourcesPrompter initAgentSourcesPrompter
	reviewPolicyPrompter initReviewPolicyPrompter
	routesPrompter       initRoutesPrompter
	retentionPrompter    initRetentionPrompter
	keyringPrompter      initKeyringBackendPrompter
	secretPrompter       initSecretPrompter
	clipboardSupported   func() bool
	clipboardRead        func() (string, error)
	configPath           func(*root.Options) (string, error)
	loadConfig           func(string) (config.File, bool, error)
	saveConfig           func(string, config.File) error
	openStore            func(string, bool, config.File) (initStore, error)
	readSecret           func(io.Reader, bool, string, string, string) (string, bool, error)
}

type initPlan struct {
	path              string
	cfg               config.File
	previousProfile   *config.Profile
	profileName       string
	profile           config.Profile
	writes            map[string]map[string]string
	credentialPlan    []initCredentialPlanEntry
	overwriteRefs     map[string]bool
	satisfiedRefs     map[string]bool
	backendFlagSet    bool
	backendArg        string
	llmSecretProvided bool
	allowDeferredLLM  bool
	writeLLMHint      bool
}

type initWorkspaceDraft struct {
	path               string
	cfg                config.File
	previousProfile    *config.Profile
	profileName        string
	profile            config.Profile
	gitScopeName       string
	gitScopes          map[string]initGitScopeDraft
	reviewerEntityName string
	reviewerEntities   map[string]initReviewerEntityDraft
	llmRuntimeName     string
	llmRuntimes        map[string]initLLMRuntimeDraft
	writes             map[string]map[string]string
	credentialPlan     []initCredentialPlanEntry
	overwriteRefs      map[string]bool
	satisfiedRefs      map[string]bool
	backendFlagSet     bool
	backendArg         string
	allowDeferredLLM   bool
	writeLLMHint       bool
}

type initSessionPlan struct {
	path           string
	cfg            config.File
	profileNames   []string
	profileRefs    map[string][]config.CredentialRef
	writes         map[string]map[string]string
	credentialPlan []initCredentialPlanEntry
	overwriteRefs  map[string]bool
	satisfiedRefs  map[string]bool
	backendFlagSet bool
	backendArg     string
}

type initSessionDraft struct {
	path                 string
	originalCfg          config.File
	cfg                  config.File
	requestedProfileName string
	backendFlagSet       bool
	workspace            *initWorkspaceDraft
	touchedProfiles      map[string]string
}

type initGitScopeDraft struct {
	Name          string
	Host          string
	AuthMode      config.GitAuthMode
	CredentialRef string
}

type initReviewerEntityKind string

const (
	initReviewerEntityKindUseGitIdentity initReviewerEntityKind = "use_git_identity"
	initReviewerEntityKindPAT            initReviewerEntityKind = "pat"
	initReviewerEntityKindGitHubApp      initReviewerEntityKind = "github_app"
)

type initReviewerEntityDraft struct {
	Name          string
	Kind          initReviewerEntityKind
	AuthMode      config.GitAuthMode
	CredentialRef string
}

type initLLMRuntimePreset string

const (
	initLLMRuntimePresetClaudeCLISubscription initLLMRuntimePreset = "claude_cli_subscription"
	initLLMRuntimePresetCodexCLISubscription  initLLMRuntimePreset = "codex_cli_subscription"
	initLLMRuntimePresetPiLocal               initLLMRuntimePreset = "pi_local"
	// #nosec G101 -- runtime preset identifiers, not secret material.
	initLLMRuntimePresetAnthropicAPIKey initLLMRuntimePreset = "anthropic_api_key"
	// #nosec G101 -- runtime preset identifiers, not secret material.
	initLLMRuntimePresetOpenAIAPIKey initLLMRuntimePreset = "openai_api_key"
	initLLMRuntimePresetCustom       initLLMRuntimePreset = "custom"
)

type initLLMRuntimeDraft struct {
	Name          string
	Preset        initLLMRuntimePreset
	Provider      config.LLMProvider
	Auth          config.LLMAuth
	Adapter       config.LLMAdapter
	CredentialRef string
}

type initStore interface {
	Exists(profile, key string) (bool, error)
	ListBundle(profile string) ([]string, error)
	SetBundle(profile string, kv map[string]string, opts ...credstore.SetOpt) (credstore.Result, error)
	Close() error
}

type initCredentialPlanState string

const (
	initCredentialPlanStateKeepExisting initCredentialPlanState = "keep_existing"
	initCredentialPlanStateDefer        initCredentialPlanState = "defer"
	// #nosec G101 -- init planner state label, not secret material.
	initCredentialPlanStateOverwriteRef    initCredentialPlanState = "overwrite_ref"
	initCredentialPlanStateWrite           initCredentialPlanState = "write"
	initCredentialPlanStateClearRef        initCredentialPlanState = "clear_ref"
	initCredentialPlanStateMissingRequired initCredentialPlanState = "missing_required"
	initCreateProfileSentinel                                      = "__create__"
)

type initCredentialPlanEntry struct {
	Ref                 config.CredentialRef
	PreviousRef         *config.CredentialRef
	KeySpecs            []credentials.KeySpec
	PlannedWriteKeys    []string
	MissingRequiredKeys []string
	State               initCredentialPlanState
}

type initSecretPrompter interface {
	ChooseCredentialAction(initCredentialSecretPrompt) (initCredentialSecretAction, error)
	ChooseSecretSource(initSecretValuePrompt) (initSecretSource, error)
	PasteSecret(initSecretValuePrompt) (string, error)
}

type initCredentialSecretAction string

const (
	initCredentialSecretActionKeep   initCredentialSecretAction = "keep"
	initCredentialSecretActionSetNow initCredentialSecretAction = "set_now"
	initCredentialSecretActionDefer  initCredentialSecretAction = "defer"
)

type initModelMapAction string

const (
	initModelMapActionPreserve initModelMapAction = "preserve"
	initModelMapActionEdit     initModelMapAction = "edit"
	initModelMapActionReset    initModelMapAction = "reset"
)

type initAgentSourcesAction string

const (
	initAgentSourcesActionPreserve initAgentSourcesAction = "preserve"
	initAgentSourcesActionEdit     initAgentSourcesAction = "edit"
	initAgentSourcesActionReset    initAgentSourcesAction = "reset"
)

type initReviewPolicyAction string

const (
	initReviewPolicyActionPreserve initReviewPolicyAction = "preserve"
	initReviewPolicyActionEdit     initReviewPolicyAction = "edit"
)

type initRoutesAction string

const (
	initRoutesActionPreserve initRoutesAction = "preserve"
	initRoutesActionEdit     initRoutesAction = "edit"
	initRoutesActionReset    initRoutesAction = "reset"
)

type initRetentionAction string

const (
	initRetentionActionPreserve initRetentionAction = "preserve"
	initRetentionActionEdit     initRetentionAction = "edit"
	initRetentionActionReset    initRetentionAction = "reset"
)

type initRetentionMaxAgeMode string

const (
	initRetentionMaxAgeDefault initRetentionMaxAgeMode = "default"
	initRetentionMaxAgeForever initRetentionMaxAgeMode = "forever"
	initRetentionMaxAgeCustom  initRetentionMaxAgeMode = "custom"
)

type initKeyringBackendAction string

const (
	initKeyringBackendActionPreserve initKeyringBackendAction = "preserve"
	initKeyringBackendActionEdit     initKeyringBackendAction = "edit"
	initKeyringBackendActionReset    initKeyringBackendAction = "reset"
)

type initSecretSource string

const (
	initSecretSourceKeepExisting initSecretSource = "keep_existing"
	initSecretSourcePaste        initSecretSource = "paste"
	initSecretSourceClipboard    initSecretSource = "clipboard"
	initSecretSourceSkip         initSecretSource = "skip"
)

type initCredentialSecretPrompt struct {
	Entry              initCredentialPlanEntry
	TargetHasRequired  bool
	TargetHasAnyKeys   bool
	ClipboardSupported bool
}

type initSecretValuePrompt struct {
	Entry              initCredentialPlanEntry
	Key                string
	Optional           bool
	TargetHasKey       bool
	ClipboardSupported bool
}

func defaultInitDeps() initDeps {
	return initDeps{
		menuPrompter:         nil,
		llmRuntimePrompter:   nil,
		reviewerPrompter:     nil,
		finalizePrompter:     nil,
		modelMapPrompter:     nil,
		agentSourcesPrompter: nil,
		reviewPolicyPrompter: nil,
		routesPrompter:       nil,
		retentionPrompter:    nil,
		keyringPrompter:      nil,
		clipboardSupported:   func() bool { return !clipboard.Unsupported },
		clipboardRead:        clipboard.ReadAll,
		configPath:           configPath,
		loadConfig:           loadConfigForInit,
		saveConfig:           config.Save,
		openStore: func(flagValue string, flagSet bool, cfg config.File) (initStore, error) {
			return credentials.OpenStore(flagValue, flagSet, cfg)
		},
		readSecret: readOptionalSecretIngress,
	}
}

func (deps initDeps) withDefaults() initDeps {
	defaults := defaultInitDeps()
	if deps.secretPrompter == nil {
		deps.secretPrompter = defaults.secretPrompter
	}
	if deps.menuPrompter == nil {
		deps.menuPrompter = defaults.menuPrompter
	}
	if deps.llmRuntimePrompter == nil {
		deps.llmRuntimePrompter = defaults.llmRuntimePrompter
	}
	if deps.reviewerPrompter == nil {
		deps.reviewerPrompter = defaults.reviewerPrompter
	}
	if deps.finalizePrompter == nil {
		deps.finalizePrompter = defaults.finalizePrompter
	}
	if deps.modelMapPrompter == nil {
		deps.modelMapPrompter = defaults.modelMapPrompter
	}
	if deps.agentSourcesPrompter == nil {
		deps.agentSourcesPrompter = defaults.agentSourcesPrompter
	}
	if deps.reviewPolicyPrompter == nil {
		deps.reviewPolicyPrompter = defaults.reviewPolicyPrompter
	}
	if deps.routesPrompter == nil {
		deps.routesPrompter = defaults.routesPrompter
	}
	if deps.retentionPrompter == nil {
		deps.retentionPrompter = defaults.retentionPrompter
	}
	if deps.keyringPrompter == nil {
		deps.keyringPrompter = defaults.keyringPrompter
	}
	if deps.clipboardSupported == nil {
		deps.clipboardSupported = defaults.clipboardSupported
	}
	if deps.clipboardRead == nil {
		deps.clipboardRead = defaults.clipboardRead
	}
	if deps.configPath == nil {
		deps.configPath = defaults.configPath
	}
	if deps.loadConfig == nil {
		deps.loadConfig = defaults.loadConfig
	}
	if deps.saveConfig == nil {
		deps.saveConfig = defaults.saveConfig
	}
	if deps.openStore == nil {
		deps.openStore = defaults.openStore
	}
	if deps.readSecret == nil {
		deps.readSecret = defaults.readSecret
	}
	return deps
}

func newInitCommand(opts *root.Options) *cobra.Command {
	flags := initOptions{
		gitHost:      "github.com",
		gitAuth:      string(config.GitAuthModePAT),
		reviewerAuth: string(config.GitAuthModePAT),
		llmProvider:  string(config.LLMProviderAnthropic),
		llmAuth:      string(config.LLMAuthSubscription),
		llmAdapter:   string(config.LLMAdapterClaudeCLI),
		majorEvent:   string(config.ReviewMajorEventComment),
	}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create or update non-secret cr configuration",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("init takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, opts, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.nonInteractive, "non-interactive", false, "Run without prompts")
	cmd.Flags().StringVar(&flags.gitHost, "git-host", flags.gitHost, "Git host")
	cmd.Flags().StringVar(&flags.gitAuth, "git-auth-mode", flags.gitAuth, "Git credential auth mode")
	cmd.Flags().StringVar(&flags.gitRef, "git-credential-ref", "", "Git credential ref")
	cmd.Flags().StringVar(&flags.reviewerRef, "reviewer-credential-ref", "", "Reviewer credential ref")
	cmd.Flags().StringVar(&flags.reviewerAuth, "reviewer-auth-mode", flags.reviewerAuth, "Reviewer credential auth mode")
	cmd.Flags().BoolVar(&flags.disableReviewer, "disable-reviewer", false, "Disable separate reviewer credentials")
	cmd.Flags().StringVar(&flags.llmProvider, "llm-provider", flags.llmProvider, "LLM provider")
	cmd.Flags().StringVar(&flags.llmAuth, "llm-auth", flags.llmAuth, "LLM auth mode")
	cmd.Flags().StringVar(&flags.llmAdapter, "llm-adapter", flags.llmAdapter, "LLM adapter")
	cmd.Flags().StringVar(&flags.llmRef, "llm-credential-ref", "", "LLM credential ref")
	cmd.Flags().StringVar(&flags.llmReviewerTier, "llm-reviewer-model-tier", "", "Reviewer model tier override")
	cmd.Flags().BoolVar(&flags.clearLLMReviewer, "clear-llm-reviewer-model-tier", false, "Clear the configured reviewer model tier")
	cmd.Flags().StringArrayVar(&flags.agentSources, "agent-source", nil, "Agent source ref (repeatable)")
	cmd.Flags().StringVar(&flags.majorEvent, "major-event", flags.majorEvent, "Major finding event policy")
	cmd.Flags().BoolVar(&flags.selfApprove, "allow-self-approve", false, "Allow self approval")
	cmd.Flags().StringVar(&flags.resolveThreads, "resolve-threads", "", "Thread resolution policy")
	cmd.Flags().StringVar(&flags.resolveAfter, "resolve-after", "", "Duration before thread resolution")
	cmd.Flags().BoolVar(&flags.gitTokenStdin, "git-token-stdin", false, "Read the Git token from stdin")
	cmd.Flags().StringVar(&flags.gitTokenEnv, "git-token-from-env", "", "Read the Git token from this environment variable")
	cmd.Flags().BoolVar(&flags.reviewerTokenStdin, "reviewer-token-stdin", false, "Read the reviewer Git token from stdin")
	cmd.Flags().StringVar(&flags.reviewerTokenEnv, "reviewer-token-from-env", "", "Read the reviewer Git token from this environment variable")
	cmd.Flags().BoolVar(&flags.llmKeyStdin, "llm-api-key-stdin", false, "Read the LLM API key from stdin")
	cmd.Flags().StringVar(&flags.llmKeyEnv, "llm-api-key-from-env", "", "Read the LLM API key from this environment variable")
	cmd.Flags().BoolVar(&flags.overwrite, "overwrite", false, "Replace existing keyring entries")
	cmd.Flags().BoolVar(&flags.replaceProfile, "replace-profile", false, "Replace an existing config profile")
	cmd.Flags().BoolVar(&flags.setDefault, "set-default", false, "Set the target profile as the default profile")
	cmd.Flags().StringVar(&flags.keyringBackend, "keyring-backend", "", "Persist this keyring backend in config")
	cmd.Flags().BoolVar(&flags.resetKeyring, "reset-keyring-backend", false, "Clear any persisted keyring backend")
	return cmd
}

func runInit(cmd *cobra.Command, opts *root.Options, flags initOptions) error {
	return runInitWithDeps(cmd, opts, flags, defaultInitDeps())
}

func runInitWithDeps(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps) error {
	deps = deps.withDefaults()
	if !flags.nonInteractive {
		return runInteractiveInit(cmd, opts, flags, deps)
	}
	plan, err := buildNonInteractiveInitPlan(cmd, opts, flags, deps)
	if err != nil {
		return err
	}
	return applyInitPlan(opts, flags, deps, plan)
}

func runInteractiveInit(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps) error {
	if err := validateInteractiveInitFlags(cmd, flags); err != nil {
		return exitcode.Usage(err)
	}
	session, err := bootstrapInteractiveInitSession(cmd, opts, flags, deps)
	if err != nil {
		return err
	}
	useLegacyInitPath := deps.prompter != nil && deps.menuPrompter == nil
	if useLegacyInitPath {
		session, err = runInjectedInteractiveInit(cmd, opts, flags, deps, session)
	} else {
		session, err = runInteractiveInitMenuLoop(cmd, opts, flags, deps, session)
	}
	if err != nil {
		return err
	}
	if session.workspace == nil {
		return nil
	}
	if useLegacyInitPath {
		workspace, err := collectInteractiveInitSecrets(cmd, opts, deps, *session.workspace)
		if err != nil {
			return err
		}
		plan := finalizeInteractiveInitPlan(workspace)
		return applyInitPlan(opts, flags, deps, plan)
	}
	plan, err := buildInteractiveInitSessionPlan(opts, session)
	if err != nil {
		return err
	}
	plan, err = collectInteractiveInitSessionSecrets(opts, deps, plan)
	if err != nil {
		return err
	}
	action, err := chooseInteractiveInitFinalizeAction(opts, deps, plan)
	if err != nil {
		return err
	}
	if action == initFinalizeActionCancel {
		return nil
	}
	return applyInteractiveInitSessionPlan(opts, deps, plan)
}

func runInjectedInteractiveInit(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	var err error
	session, err = editInteractiveInitProfile(cmd, opts, flags, deps, session)
	if err != nil {
		return initSessionDraft{}, err
	}
	if deps.retentionPrompter != nil {
		session.cfg, err = collectInteractiveInitRetentionConfig(opts, deps, cloneInitConfigFile(session.cfg))
		if err != nil {
			return initSessionDraft{}, err
		}
		if session.workspace != nil {
			workspace := *session.workspace
			workspace.cfg = cloneInitConfigFile(session.cfg)
			session.workspace = &workspace
		}
	}
	if deps.keyringPrompter != nil {
		session.cfg, err = collectInteractiveInitKeyringBackendConfig(opts, deps, session.backendFlagSet, cloneInitConfigFile(session.cfg))
		if err != nil {
			return initSessionDraft{}, err
		}
		if session.workspace != nil {
			workspace := *session.workspace
			workspace.cfg = cloneInitConfigFile(session.cfg)
			workspace.backendArg = interactiveInitBackendArg(opts, workspace.backendFlagSet, session.cfg)
			session.workspace = &workspace
		}
	}
	return session, nil
}

type huhInitPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitMenuPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitLLMRuntimePrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitReviewerEntityPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitFinalizePrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

func newHuhInitPrompter(opts *root.Options) initPrompter {
	return huhInitPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitMenuPrompter(opts *root.Options) initMenuPrompter {
	return huhInitMenuPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitLLMRuntimePrompter(opts *root.Options) initLLMRuntimePrompter {
	return huhInitLLMRuntimePrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitReviewerEntityPrompter(opts *root.Options) initReviewerEntityPrompter {
	return huhInitReviewerEntityPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitFinalizePrompter(opts *root.Options) initFinalizePrompter {
	return huhInitFinalizePrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

type huhInitSecretPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitModelMapPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitAgentSourcesPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitReviewPolicyPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitRoutesPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitRetentionPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitKeyringBackendPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

const (
	initCustomGitScopeSelection       = "__custom_git_scope__"
	initCustomLLMRuntimeSelection     = "__custom_llm_runtime__"
)

func newHuhInitSecretPrompter(opts *root.Options) initSecretPrompter {
	return huhInitSecretPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitModelMapPrompter(opts *root.Options) initModelMapPrompter {
	return huhInitModelMapPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitAgentSourcesPrompter(opts *root.Options) initAgentSourcesPrompter {
	return huhInitAgentSourcesPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitReviewPolicyPrompter(opts *root.Options) initReviewPolicyPrompter {
	return huhInitReviewPolicyPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitRoutesPrompter(opts *root.Options) initRoutesPrompter {
	return huhInitRoutesPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitRetentionPrompter(opts *root.Options) initRetentionPrompter {
	return huhInitRetentionPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitKeyringBackendPrompter(opts *root.Options) initKeyringBackendPrompter {
	return huhInitKeyringBackendPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func buildInteractiveInitPromptContext(cmd *cobra.Command, opts *root.Options, deps initDeps, ctx initPromptContext) (initPromptContext, error) {
	ctx.GitScopes, ctx.ProfileGitScopes = buildInitGitScopeInventory(ctx.ExistingConfig)
	ctx.ReviewerEntities, ctx.ProfileReviewerEntities = buildInitReviewerEntityInventory(ctx.ExistingConfig)
	ctx.LLMRuntimes, ctx.ProfileLLMRuntimes = buildInitLLMRuntimeInventory(ctx.ExistingConfig)

	if len(ctx.ExistingConfig.Profiles) == 0 {
		return ctx, nil
	}
	backendFlagSet := cmderr.BackendFlagChanged(cmd)
	store, err := deps.openStore(opts.Backend, backendFlagSet, ctx.ExistingConfig)
	storeErr := err
	if initStorePresent(store) {
		defer func() { _ = store.Close() }()
	}
	ctx.ProfileWarnings = map[string][]string{}
	for name, profile := range ctx.ExistingConfig.Profiles {
		refs, err := config.CredentialRefs(profile)
		if err != nil {
			return initPromptContext{}, cmderr.Config(err)
		}
		statuses, err := credentials.CredentialStatuses(store, refs, storeErr)
		if err != nil {
			return initPromptContext{}, cmderr.Credential(err)
		}
		if warnings := initCredentialHealthWarnings(statuses); len(warnings) > 0 {
			ctx.ProfileWarnings[name] = warnings
		}
	}
	return ctx, nil
}

func initStorePresent(store initStore) bool {
	if store == nil {
		return false
	}
	// A typed-nil store can sit inside the interface after openStore returns, so store == nil is not sufficient here.
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array,
		reflect.String, reflect.Struct, reflect.UnsafePointer:
		return true
	}
	return true
}

func initCredentialHealthWarnings(statuses []credentials.CredentialStatus) []string {
	var warnings []string
	for _, status := range statuses {
		label := initCredentialPurposeLabel(status.Purpose)
		missing := credentials.MissingRequiredKeys(status)
		switch {
		case len(missing) > 0:
			warnings = append(warnings, fmt.Sprintf("%s secret health: %s is missing required keys (%s)", label, status.Ref, strings.Join(missing, ", ")))
		case !credentials.RequiredKeysSatisfied(status):
			warnings = append(warnings, fmt.Sprintf("%s secret health: cannot verify required keys for %s", label, status.Ref))
		}
	}
	return warnings
}

func bootstrapInteractiveInitSession(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps) (initSessionDraft, error) {
	profileName := opts.Profile
	if profileName == "" {
		profileName = credstore.DefaultProfile
	}
	path, err := deps.configPath(opts)
	if err != nil {
		return initSessionDraft{}, exitcode.AuthConfig(err)
	}
	cfg, _, err := deps.loadConfig(path)
	if err != nil {
		return initSessionDraft{}, cmderr.Config(err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	session := initSessionDraft{
		path:                 path,
		originalCfg:          cloneInitConfigFile(cfg),
		cfg:                  cloneInitConfigFile(cfg),
		requestedProfileName: profileName,
		backendFlagSet:       cmderr.BackendFlagChanged(cmd),
		touchedProfiles:      map[string]string{},
	}
	existingProfileName := ""
	var existingProfile *config.Profile
	if profile, ok := session.cfg.Profiles[profileName]; ok {
		existingProfileName = profileName
		profileCopy := profile
		existingProfile = &profileCopy
	} else if opts.Profile == "" && session.cfg.DefaultProfile != "" {
		if profile, ok := session.cfg.Profiles[session.cfg.DefaultProfile]; ok {
			existingProfileName = session.cfg.DefaultProfile
			profileCopy := profile
			existingProfile = &profileCopy
			session.requestedProfileName = session.cfg.DefaultProfile
		}
	}
	if existingProfile == nil {
		return session, nil
	}
	draft := seedInteractiveInitDraft(session.requestedProfileName, existingProfileName, session.cfg.DefaultProfile, existingProfile)
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, err
	}
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	return session, nil
}

func buildInteractiveInitMenuPrompt(session initSessionDraft) initMenuPrompt {
	llmRuntimes, _ := buildInitLLMRuntimeInventory(session.cfg)
	reviewerEntities, _ := buildInitReviewerEntityInventory(session.cfg)
	prompt := initMenuPrompt{
		HasWorkspace:         session.workspace != nil,
		LLMRuntimeCount:      len(llmRuntimes),
		ReviewerEntityCount:  len(reviewerEntities),
		ReviewProfileCount:   len(session.cfg.Profiles),
	}
	if session.workspace == nil {
		return prompt
	}
	prompt.ActiveProfileName = session.workspace.profileName
	prompt.CanConfigureLLM = true
	prompt.CanConfigureReviewer = true
	prompt.CanSave = true
	return prompt
}

func currentInteractiveInitPromptContext(cmd *cobra.Command, opts *root.Options, deps initDeps, session initSessionDraft) (initPromptContext, error) {
	existingProfileName := ""
	var existingProfile *config.Profile
	if session.workspace != nil {
		existingProfileName = session.workspace.profileName
		profileCopy := session.workspace.profile
		existingProfile = &profileCopy
	} else if profile, ok := session.cfg.Profiles[session.requestedProfileName]; ok {
		existingProfileName = session.requestedProfileName
		profileCopy := profile
		existingProfile = &profileCopy
	}
	return buildInteractiveInitPromptContext(cmd, opts, deps, initPromptContext{
		RequestedProfileName: session.requestedProfileName,
		ExistingProfileName:  existingProfileName,
		ExistingProfile:      existingProfile,
		ExistingProfileNames: sortedProfileNames(session.cfg.Profiles),
		DefaultProfileName:   session.cfg.DefaultProfile,
		ExistingConfig:       session.cfg,
	})
}

func runInteractiveInitMenuLoop(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	menuPrompter := deps.menuPrompter
	if menuPrompter == nil {
		menuPrompter = newHuhInitMenuPrompter(opts)
	}
	for {
		action, err := menuPrompter.ChooseAction(buildInteractiveInitMenuPrompt(session))
		if err != nil {
			return initSessionDraft{}, err
		}
		switch action {
		case initMenuActionLLMRuntimes:
			session, err = editInteractiveInitLLMRuntime(cmd, opts, flags, deps, session)
		case initMenuActionReviewerEntities:
			session, err = editInteractiveInitReviewerEntity(cmd, opts, flags, deps, session)
		case initMenuActionReviewProfiles:
			session, err = editInteractiveInitProfile(cmd, opts, flags, deps, session)
		case initMenuActionGlobalSettings:
			session, err = editInteractiveInitGlobalSettings(cmd, opts, deps, session)
		case initMenuActionSave:
			if session.workspace == nil {
				return initSessionDraft{}, exitcode.Usage(errors.New("save requires at least one configured profile"))
			}
			return session, nil
		case initMenuActionExit:
			session.workspace = nil
			return session, nil
		default:
			err = fmt.Errorf("unsupported init menu action %q", action)
		}
		if err != nil {
			return initSessionDraft{}, err
		}
	}
}

func editInteractiveInitProfile(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	prompter := deps.prompter
	if prompter == nil {
		prompter = newHuhInitPrompter(opts)
	}
	promptCtx, err := currentInteractiveInitPromptContext(cmd, opts, deps, session)
	if err != nil {
		return initSessionDraft{}, err
	}
	draft, err := prompter.Run(promptCtx)
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err = collectInteractiveInitRoutes(opts, deps, workspace)
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err = collectInteractiveInitModelMap(opts, deps, workspace)
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err = collectInteractiveInitAgentSources(opts, deps, workspace)
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err = collectInteractiveInitReviewPolicy(opts, deps, workspace)
	if err != nil {
		return initSessionDraft{}, err
	}
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.requestedProfileName = workspace.profileName
	session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
	return session, nil
}

func editInteractiveInitLLMRuntime(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	if session.workspace == nil {
		return initSessionDraft{}, exitcode.Usage(errors.New("configure a review profile before editing LLM runtimes"))
	}
	prompter := deps.llmRuntimePrompter
	if prompter == nil {
		prompter = newHuhInitLLMRuntimePrompter(opts)
	}
	promptCtx, err := currentInteractiveInitPromptContext(cmd, opts, deps, session)
	if err != nil {
		return initSessionDraft{}, err
	}
	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: promptCtx})
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, err
	}
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.requestedProfileName = workspace.profileName
	session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
	return session, nil
}

func editInteractiveInitReviewerEntity(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	if session.workspace == nil {
		return initSessionDraft{}, exitcode.Usage(errors.New("configure a review profile before editing reviewer entities"))
	}
	prompter := deps.reviewerPrompter
	if prompter == nil {
		prompter = newHuhInitReviewerEntityPrompter(opts)
	}
	promptCtx, err := currentInteractiveInitPromptContext(cmd, opts, deps, session)
	if err != nil {
		return initSessionDraft{}, err
	}
	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: promptCtx})
	if err != nil {
		return initSessionDraft{}, err
	}
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, err
	}
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.requestedProfileName = workspace.profileName
	session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
	return session, nil
}

func editInteractiveInitGlobalSettings(_ *cobra.Command, opts *root.Options, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	cfg, err := collectInteractiveInitRetentionConfig(opts, deps, cloneInitConfigFile(session.cfg))
	if err != nil {
		return initSessionDraft{}, err
	}
	cfg, err = collectInteractiveInitKeyringBackendConfig(opts, deps, session.backendFlagSet, cfg)
	if err != nil {
		return initSessionDraft{}, err
	}
	session.cfg = cfg
	if session.workspace != nil {
		workspace := *session.workspace
		workspace.cfg = cloneInitConfigFile(cfg)
		workspace.backendArg = interactiveInitBackendArg(opts, workspace.backendFlagSet, cfg)
		session.workspace = &workspace
	}
	return session, nil
}

func (p huhInitMenuPrompter) ChooseAction(prompt initMenuPrompt) (initMenuAction, error) {
	action := initMenuActionReviewProfiles
	if prompt.CanSave {
		action = initMenuActionSave
	}
	options := []huh.Option[initMenuAction]{
		huh.NewOption(fmt.Sprintf("Configure LLM runtimes (%d)", prompt.LLMRuntimeCount), initMenuActionLLMRuntimes),
		huh.NewOption(fmt.Sprintf("Configure reviewer entities (%d)", prompt.ReviewerEntityCount), initMenuActionReviewerEntities),
		huh.NewOption(fmt.Sprintf("Configure review profiles (%d)", prompt.ReviewProfileCount), initMenuActionReviewProfiles),
		huh.NewOption("Review global settings", initMenuActionGlobalSettings),
		huh.NewOption("Save and exit", initMenuActionSave),
		huh.NewOption("Exit without saving", initMenuActionExit),
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initMenuAction]().
				Title("cr init").
				Description(initMenuDescription(prompt)).
				Options(options...).
				Value(&action).
				Validate(func(value initMenuAction) error {
					switch value {
					case initMenuActionLLMRuntimes:
						if !prompt.CanConfigureLLM {
							return errors.New("configure a review profile before editing LLM runtimes")
						}
					case initMenuActionReviewerEntities:
						if !prompt.CanConfigureReviewer {
							return errors.New("configure a review profile before editing reviewer entities")
						}
					case initMenuActionSave:
						if !prompt.CanSave {
							return errors.New("configure a review profile before saving")
						}
					case initMenuActionReviewProfiles, initMenuActionGlobalSettings, initMenuActionExit:
					}
					return nil
				}),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return "", err
	}
	return action, nil
}

func initMenuDescription(prompt initMenuPrompt) string {
	if prompt.HasWorkspace && strings.TrimSpace(prompt.ActiveProfileName) != "" {
		return fmt.Sprintf("Active profile: %s", prompt.ActiveProfileName)
	}
	return "Configure the parts cr needs, then save when the active profile is ready."
}

func (p huhInitFinalizePrompter) ChooseFinalizeAction(prompt initFinalizePrompt) (initFinalizeAction, error) {
	action := initFinalizeActionSave
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Description(initFinalizeDescription(prompt)),
			huh.NewSelect[initFinalizeAction]().
				Title("Finalize init").
				Options(
					huh.NewOption("Save and write config/credentials", initFinalizeActionSave),
					huh.NewOption("Cancel without saving", initFinalizeActionCancel),
				).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return "", err
	}
	return action, nil
}

func initFinalizeDescription(prompt initFinalizePrompt) string {
	lines := []string{"Review readiness before writing config or credentials."}
	for _, profile := range prompt.Profiles {
		status := "ready"
		if !profile.Ready {
			status = "needs follow-up"
		}
		line := fmt.Sprintf("- %s: %s", profile.ProfileName, status)
		if len(profile.Notes) > 0 {
			line += " (" + strings.Join(profile.Notes, "; ") + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (p huhInitLLMRuntimePrompter) EditLLMRuntime(prompt initLLMRuntimePrompt) (initDraft, error) {
	draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
	selectedRuntime := prompt.Context.ProfileLLMRuntimes[prompt.Context.ExistingProfileName]
	seedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	if selectedRuntime == "" {
		if seedRuntimePreset == string(initLLMRuntimePresetCustom) {
			selectedRuntime = initCustomLLMRuntimeSelection
		} else {
			selectedRuntime = seedRuntimePreset
		}
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("LLM runtime").
				Description("Choose how reviewer agents run for this profile.").
				Options(initLLMRuntimeOptions(prompt.Context.LLMRuntimes)...).
				Value(&selectedRuntime),
			huh.NewSelect[string]().
				Title("LLM provider").
				Options(
					huh.NewOption("Anthropic", string(config.LLMProviderAnthropic)),
					huh.NewOption("OpenAI", string(config.LLMProviderOpenAI)),
					huh.NewOption("Pi", string(config.LLMProviderPi)),
				).
				Value(&draft.LLMProvider),
			huh.NewSelect[string]().
				Title("LLM auth mode").
				Options(
					huh.NewOption("Subscription", string(config.LLMAuthSubscription)),
					huh.NewOption("API key", string(config.LLMAuthAPIKey)),
				).
				Value(&draft.LLMAuth),
			huh.NewSelect[string]().
				Title("LLM adapter").
				Options(
					huh.NewOption("Claude CLI", string(config.LLMAdapterClaudeCLI)),
					huh.NewOption("Anthropic API", string(config.LLMAdapterAnthropicAPI)),
					huh.NewOption("Codex CLI", string(config.LLMAdapterCodexCLI)),
					huh.NewOption("OpenAI API", string(config.LLMAdapterOpenAIAPI)),
					huh.NewOption("Pi RPC", string(config.LLMAdapterPiRPC)),
				).
				Value(&draft.LLMAdapter),
		).WithHideFunc(func() bool {
			return selectedRuntime != initCustomLLMRuntimeSelection
		}).Title("LLM Runtime"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initDraft{}, err
	}
	applyLLMRuntimeInventorySelection(&draft, selectedRuntime, prompt.Context.LLMRuntimes)
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	return draft, nil
}

func (p huhInitReviewerEntityPrompter) EditReviewerEntity(prompt initReviewerEntityPrompt) (initDraft, error) {
	draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
	selectedReviewerEntity := prompt.Context.ProfileReviewerEntities[prompt.Context.ExistingProfileName]
	reviewerMode := string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	if selectedReviewerEntity == "" {
		selectedReviewerEntity = reviewerMode
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Reviewer entity").
				Description("Choose who posts COMMENT, APPROVE, or REQUEST_CHANGES for this profile.").
				Options(initReviewerEntityOptions(prompt.Context.ReviewerEntities)...).
				Value(&selectedReviewerEntity),
		).Title("Reviewer Entity"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initDraft{}, err
	}
	applyReviewerEntityInventorySelection(&draft, selectedReviewerEntity, prompt.Context.ReviewerEntities)
	reviewerMode = string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	applyReviewerEntitySelection(&draft, reviewerMode)
	return draft, nil
}

func (p huhInitPrompter) Run(ctx initPromptContext) (initDraft, error) {
	selectedProfileName := ctx.ExistingProfileName
	selectedExistingProfile := ctx.ExistingProfile
	selectedCreateNewProfile := false
	if len(ctx.ExistingProfileNames) > 0 {
		choice := initCreateProfileSentinel
		options := make([]huh.Option[string], 0, len(ctx.ExistingProfileNames)+1)
		for _, name := range ctx.ExistingProfileNames {
			options = append(options, huh.NewOption("Edit "+name, name))
			if name == ctx.ExistingProfileName {
				choice = name
			}
		}
		if ctx.ExistingProfile == nil {
			choice = initCreateProfileSentinel
		}
		options = append(options, huh.NewOption("Create new profile", initCreateProfileSentinel))
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Choose a profile to edit or create").
					Options(options...).
					Value(&choice),
			),
		).WithInput(p.stdin).WithOutput(p.stderr)
		if err := form.Run(); err != nil {
			return initDraft{}, err
		}
		if choice == initCreateProfileSentinel {
			selectedProfileName = ""
			selectedExistingProfile = nil
			selectedCreateNewProfile = true
		} else {
			selectedProfileName = choice
			profile := ctx.ExistingConfig.Profiles[choice]
			profileCopy := profile
			selectedExistingProfile = &profileCopy
		}
	}

	requestedProfileName := ctx.RequestedProfileName
	if selectedCreateNewProfile && ctx.ExistingProfile != nil {
		requestedProfileName = ""
	}
	draft := seedInteractiveInitDraft(requestedProfileName, selectedProfileName, ctx.DefaultProfileName, selectedExistingProfile)
	if warnings := ctx.ProfileWarnings[selectedProfileName]; len(warnings) > 0 {
		_, _ = fmt.Fprintln(p.stderr, "Existing profile secret health:")
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(p.stderr, "- %s\n", warning)
		}
		_, _ = fmt.Fprintln(p.stderr)
	}
	reviewerEntity := initReviewerEntityDraftFromSeedDraft(draft)
	llmRuntime := initLLMRuntimeDraftFromSeedDraft(draft)
	selectedRuntimePreset := string(llmRuntime.Preset)
	reviewerMode := string(reviewerEntity.Kind)
	selectedGitScope := ctx.ProfileGitScopes[selectedProfileName]
	if selectedGitScope == "" {
		selectedGitScope = initCustomGitScopeSelection
	}
	selectedReviewerEntity := ctx.ProfileReviewerEntities[selectedProfileName]
	if selectedReviewerEntity == "" {
		selectedReviewerEntity = reviewerMode
	}
	selectedLLMRuntime := ctx.ProfileLLMRuntimes[selectedProfileName]
	if selectedLLMRuntime == "" {
		if selectedRuntimePreset == string(initLLMRuntimePresetCustom) {
			selectedLLMRuntime = initCustomLLMRuntimeSelection
		} else {
			selectedLLMRuntime = selectedRuntimePreset
		}
	}
	gitScopeOptions := initGitScopeOptions(ctx.GitScopes)
	reviewerEntityOptions := initReviewerEntityOptions(ctx.ReviewerEntities)
	llmRuntimeOptions := initLLMRuntimeOptions(ctx.LLMRuntimes)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Profile name").
				Value(&draft.ProfileName).
				Validate(validateProfileName),
			huh.NewConfirm().
				Title("Make this the default profile").
				Value(&draft.MakeDefault),
		).Title("Profile"),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Git scope").
				Description("Choose an existing Git scope for this profile or configure a custom one.").
				Options(gitScopeOptions...).
				Value(&selectedGitScope),
			huh.NewInput().
				Title("Git scope host").
				Description("The Git host this review profile applies to, such as github.com or github.mycompany.com.").
				Value(&draft.GitHost).
				Validate(validateRequiredText("git host is required")),
			huh.NewSelect[string]().
				Title("Git scope auth mode").
				Options(
					huh.NewOption("Personal access token", string(config.GitAuthModePAT)),
					huh.NewOption("GitHub App", string(config.GitAuthModeGitHubApp)),
				).
				Value(&draft.GitAuth),
		).WithHideFunc(func() bool {
			return selectedGitScope != initCustomGitScopeSelection
		}).Title("Git Scope"),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Reviewer entity").
				Description("Choose who posts COMMENT, APPROVE, or REQUEST_CHANGES for this profile.").
				Options(reviewerEntityOptions...).
				Value(&selectedReviewerEntity),
			huh.NewSelect[string]().
				Title("LLM runtime").
				Description("Choose how reviewer agents run for this profile.").
				Options(llmRuntimeOptions...).
				Value(&selectedLLMRuntime),
			huh.NewSelect[string]().
				Title("Reviewer model tier").
				Options(
					huh.NewOption("Built-in default", ""),
					huh.NewOption("Small", string(config.ModelTierSmall)),
					huh.NewOption("Medium", string(config.ModelTierMedium)),
					huh.NewOption("Large", string(config.ModelTierLarge)),
				).
				Value(&draft.LLMReviewerModelTier),
			huh.NewConfirm().
				Title("Advanced storage labels").
				Description("Inspect or override non-secret credential-store labels for Git, reviewer, and LLM secrets.").
				Value(&draft.AdvancedStorageLabels),
		).Title("Review Profile"),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("LLM provider").
				Options(
					huh.NewOption("Anthropic", string(config.LLMProviderAnthropic)),
					huh.NewOption("OpenAI", string(config.LLMProviderOpenAI)),
					huh.NewOption("Pi", string(config.LLMProviderPi)),
				).
				Value(&draft.LLMProvider),
			huh.NewSelect[string]().
				Title("LLM auth mode").
				Options(
					huh.NewOption("Subscription", string(config.LLMAuthSubscription)),
					huh.NewOption("API key", string(config.LLMAuthAPIKey)),
				).
				Value(&draft.LLMAuth),
			huh.NewSelect[string]().
				Title("LLM adapter").
				Options(
					huh.NewOption("Claude CLI", string(config.LLMAdapterClaudeCLI)),
					huh.NewOption("Anthropic API", string(config.LLMAdapterAnthropicAPI)),
					huh.NewOption("Codex CLI", string(config.LLMAdapterCodexCLI)),
					huh.NewOption("OpenAI API", string(config.LLMAdapterOpenAIAPI)),
					huh.NewOption("Pi RPC", string(config.LLMAdapterPiRPC)),
				).
				Value(&draft.LLMAdapter),
		).WithHideFunc(func() bool {
			return selectedLLMRuntime != initCustomLLMRuntimeSelection
		}).Title("LLM Runtime Details"),
		huh.NewGroup(
			huh.NewInput().
				Title("Git storage label").
				Description("Leave blank to use the standard profile-based label.").
				Value(&draft.GitCredentialRef).
				Validate(validateOptionalCredentialRef),
			huh.NewInput().
				Title("Reviewer storage label").
				Description("Leave blank to use the standard profile-based label when using separate reviewer credentials.").
				Value(&draft.ReviewerCredentialRef).
				Validate(validateOptionalCredentialRef),
			huh.NewInput().
				Title("LLM storage label").
				Description("Leave blank to use the standard profile-based label when using an API-key runtime.").
				Value(&draft.LLMCredentialRef).
				Validate(validateOptionalCredentialRef),
		).WithHideFunc(func() bool {
			return !draft.AdvancedStorageLabels
		}).Title("Advanced Storage Labels"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initDraft{}, err
	}
	applyGitScopeSelection(&draft, selectedGitScope, ctx.GitScopes)
	applyReviewerEntityInventorySelection(&draft, selectedReviewerEntity, ctx.ReviewerEntities)
	applyLLMRuntimeInventorySelection(&draft, selectedLLMRuntime, ctx.LLMRuntimes)
	// Inventory selections fill provider/auth/ref fields; rerunning the mode finalizers applies side effects like clearing refs for self-reviewers or subscription runtimes.
	reviewerMode = string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	selectedRuntimePreset = string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyReviewerEntitySelection(&draft, reviewerMode)
	applyLLMRuntimeSelection(&draft, selectedRuntimePreset)
	return draft, nil
}

func initReviewerEntityDraftFromSeedDraft(draft initDraft) initReviewerEntityDraft {
	if !draft.ReviewerEnabled {
		return initReviewerEntityDraft{Kind: initReviewerEntityKindUseGitIdentity}
	}
	entity := initReviewerEntityDraft{
		AuthMode:      config.GitAuthMode(draft.ReviewerAuth),
		CredentialRef: strings.TrimSpace(draft.ReviewerCredentialRef),
	}
	switch entity.AuthMode {
	case config.GitAuthModeGitHubApp:
		entity.Kind = initReviewerEntityKindGitHubApp
	case config.GitAuthModePAT, config.GitAuthModeOAuthDevice:
		entity.Kind = initReviewerEntityKindPAT
	}
	return entity
}

func initGitScopeOptions(scopes map[string]initGitScopeDraft) []huh.Option[string] {
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+1)
	for _, name := range names {
		scope := scopes[name]
		options = append(options, huh.NewOption(initGitScopeLabel(scope), name))
	}
	options = append(options, huh.NewOption("Configure a custom Git scope", initCustomGitScopeSelection))
	return options
}

func initGitScopeLabel(scope initGitScopeDraft) string {
	label := fmt.Sprintf("%s via %s", scope.Host, initGitAuthModeLabel(scope.AuthMode))
	if strings.TrimSpace(scope.Name) != "" {
		label = fmt.Sprintf("%s (%s)", label, scope.Name)
	}
	return label
}

func initGitAuthModeLabel(mode config.GitAuthMode) string {
	switch mode {
	case config.GitAuthModeGitHubApp:
		return "GitHub App"
	case config.GitAuthModePAT, config.GitAuthModeOAuthDevice:
		return "personal access token"
	}
	return "personal access token"
}

func applyGitScopeSelection(draft *initDraft, selection string, scopes map[string]initGitScopeDraft) {
	if selection == initCustomGitScopeSelection {
		return
	}
	scope, ok := scopes[selection]
	if !ok {
		return
	}
	draft.GitHost = scope.Host
	draft.GitAuth = string(scope.AuthMode)
	if !draft.AdvancedStorageLabels {
		draft.GitCredentialRef = scope.CredentialRef
	}
}

func initReviewerEntityOptions(entities map[string]initReviewerEntityDraft) []huh.Option[string] {
	names := make([]string, 0, len(entities))
	for name := range entities {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+3)
	for _, name := range names {
		entity := entities[name]
		options = append(options, huh.NewOption(initReviewerEntityLabel(entity), name))
	}
	options = append(options,
		huh.NewOption("Use this profile's Git identity", string(initReviewerEntityKindUseGitIdentity)),
		huh.NewOption("Personal access token reviewer", string(initReviewerEntityKindPAT)),
		huh.NewOption("GitHub App reviewer", string(initReviewerEntityKindGitHubApp)),
	)
	return dedupeInitStringOptions(options)
}

func initReviewerEntityLabel(entity initReviewerEntityDraft) string {
	var label string
	switch entity.Kind {
	case initReviewerEntityKindUseGitIdentity:
		label = "Use this profile's Git identity"
	case initReviewerEntityKindGitHubApp:
		label = "GitHub App reviewer"
	case initReviewerEntityKindPAT:
		label = "Personal access token reviewer"
	}
	if strings.TrimSpace(entity.Name) != "" {
		label = fmt.Sprintf("%s (%s)", label, entity.Name)
	}
	return label
}

func applyReviewerEntityInventorySelection(draft *initDraft, selection string, entities map[string]initReviewerEntityDraft) {
	switch selection {
	case string(initReviewerEntityKindUseGitIdentity), string(initReviewerEntityKindPAT), string(initReviewerEntityKindGitHubApp):
		applyReviewerEntitySelection(draft, selection)
		return
	}
	entity, ok := entities[selection]
	if !ok {
		return
	}
	draft.ReviewerEnabled = entity.Kind != initReviewerEntityKindUseGitIdentity
	draft.ReviewerAuth = string(entity.AuthMode)
	if !draft.AdvancedStorageLabels {
		draft.ReviewerCredentialRef = entity.CredentialRef
	}
}

func applyReviewerEntitySelection(draft *initDraft, selection string) {
	switch initReviewerEntityKind(selection) {
	case initReviewerEntityKindUseGitIdentity:
		draft.ReviewerEnabled = false
		draft.ReviewerAuth = string(config.GitAuthModePAT)
		if !draft.AdvancedStorageLabels {
			draft.ReviewerCredentialRef = ""
		}
	case initReviewerEntityKindGitHubApp:
		draft.ReviewerEnabled = true
		draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	case initReviewerEntityKindPAT:
		draft.ReviewerEnabled = true
		draft.ReviewerAuth = string(config.GitAuthModePAT)
	}
}

func initLLMRuntimeDraftFromSeedDraft(draft initDraft) initLLMRuntimeDraft {
	return initLLMRuntimeDraftFromConfig(config.LLMConfig{
		Provider:      config.LLMProvider(draft.LLMProvider),
		Auth:          config.LLMAuth(draft.LLMAuth),
		Adapter:       config.LLMAdapter(draft.LLMAdapter),
		CredentialRef: strings.TrimSpace(draft.LLMCredentialRef),
	})
}

func initLLMRuntimeOptions(runtimes map[string]initLLMRuntimeDraft) []huh.Option[string] {
	names := make([]string, 0, len(runtimes))
	for name := range runtimes {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+6)
	for _, name := range names {
		runtime := runtimes[name]
		options = append(options, huh.NewOption(initLLMRuntimeLabel(runtime), name))
	}
	options = append(options,
		huh.NewOption("Claude CLI subscription", string(initLLMRuntimePresetClaudeCLISubscription)),
		huh.NewOption("Codex CLI subscription", string(initLLMRuntimePresetCodexCLISubscription)),
		huh.NewOption("Pi local runtime", string(initLLMRuntimePresetPiLocal)),
		huh.NewOption("Anthropic API key", string(initLLMRuntimePresetAnthropicAPIKey)),
		huh.NewOption("OpenAI API key", string(initLLMRuntimePresetOpenAIAPIKey)),
		huh.NewOption("Custom compatible runtime", initCustomLLMRuntimeSelection),
	)
	return dedupeInitStringOptions(options)
}

func initLLMRuntimeLabel(runtime initLLMRuntimeDraft) string {
	var label string
	switch runtime.Preset {
	case initLLMRuntimePresetClaudeCLISubscription:
		label = "Claude CLI subscription"
	case initLLMRuntimePresetCodexCLISubscription:
		label = "Codex CLI subscription"
	case initLLMRuntimePresetPiLocal:
		label = "Pi local runtime"
	case initLLMRuntimePresetAnthropicAPIKey:
		label = "Anthropic API key"
	case initLLMRuntimePresetOpenAIAPIKey:
		label = "OpenAI API key"
	case initLLMRuntimePresetCustom:
		label = fmt.Sprintf("Custom runtime (%s/%s/%s)", runtime.Provider, runtime.Auth, runtime.Adapter)
	}
	if strings.TrimSpace(runtime.Name) != "" {
		label = fmt.Sprintf("%s (%s)", label, runtime.Name)
	}
	return label
}

func applyLLMRuntimeInventorySelection(draft *initDraft, selection string, runtimes map[string]initLLMRuntimeDraft) {
	if selection == initCustomLLMRuntimeSelection {
		return
	}
	if runtime, ok := runtimes[selection]; ok {
		draft.LLMProvider = string(runtime.Provider)
		draft.LLMAuth = string(runtime.Auth)
		draft.LLMAdapter = string(runtime.Adapter)
		if !draft.AdvancedStorageLabels {
			draft.LLMCredentialRef = runtime.CredentialRef
		}
		return
	}
	applyLLMRuntimeSelection(draft, selection)
}

func dedupeInitStringOptions(options []huh.Option[string]) []huh.Option[string] {
	seen := map[string]struct{}{}
	deduped := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		key := option.Value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, option)
	}
	return deduped
}

func applyLLMRuntimeSelection(draft *initDraft, selection string) {
	runtime := initLLMRuntimeDraft{
		Preset: initLLMRuntimePreset(selection),
	}
	switch runtime.Preset {
	case initLLMRuntimePresetClaudeCLISubscription:
		runtime.Provider = config.LLMProviderAnthropic
		runtime.Auth = config.LLMAuthSubscription
		runtime.Adapter = config.LLMAdapterClaudeCLI
	case initLLMRuntimePresetCodexCLISubscription:
		runtime.Provider = config.LLMProviderOpenAI
		runtime.Auth = config.LLMAuthSubscription
		runtime.Adapter = config.LLMAdapterCodexCLI
	case initLLMRuntimePresetPiLocal:
		runtime.Provider = config.LLMProviderPi
		runtime.Auth = config.LLMAuthSubscription
		runtime.Adapter = config.LLMAdapterPiRPC
	case initLLMRuntimePresetAnthropicAPIKey:
		runtime.Provider = config.LLMProviderAnthropic
		runtime.Auth = config.LLMAuthAPIKey
		runtime.Adapter = config.LLMAdapterAnthropicAPI
	case initLLMRuntimePresetOpenAIAPIKey:
		runtime.Provider = config.LLMProviderOpenAI
		runtime.Auth = config.LLMAuthAPIKey
		runtime.Adapter = config.LLMAdapterOpenAIAPI
	case initLLMRuntimePresetCustom:
		return
	}
	draft.LLMProvider = string(runtime.Provider)
	draft.LLMAuth = string(runtime.Auth)
	draft.LLMAdapter = string(runtime.Adapter)
	if runtime.Auth != config.LLMAuthAPIKey && !draft.AdvancedStorageLabels {
		draft.LLMCredentialRef = ""
	}
}

func (p huhInitSecretPrompter) ChooseCredentialAction(prompt initCredentialSecretPrompt) (initCredentialSecretAction, error) {
	options := make([]huh.Option[initCredentialSecretAction], 0, 3)
	if prompt.TargetHasRequired {
		options = append(options, huh.NewOption("Keep existing secrets", initCredentialSecretActionKeep))
	}
	options = append(options,
		huh.NewOption("Set secrets now", initCredentialSecretActionSetNow),
		huh.NewOption("Defer and configure later", initCredentialSecretActionDefer),
	)
	choice := options[0].Value
	title := fmt.Sprintf("How should init handle %s credentials?", initCredentialPurposeLabel(prompt.Entry.Ref.Purpose))
	if prompt.Entry.Ref.Ref != "" {
		title = fmt.Sprintf("%s (%s)", title, prompt.Entry.Ref.Ref)
	}
	if prompt.TargetHasAnyKeys && !prompt.TargetHasRequired {
		title = fmt.Sprintf("%s Existing values were found; choose set-now to review them key by key.", title)
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initCredentialSecretAction]().
				Title(title).
				Options(options...).
				Value(&choice),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return "", err
	}
	return choice, nil
}

func (p huhInitSecretPrompter) ChooseSecretSource(prompt initSecretValuePrompt) (initSecretSource, error) {
	options := make([]huh.Option[initSecretSource], 0, 4)
	if prompt.TargetHasKey {
		options = append(options, huh.NewOption("Keep existing value", initSecretSourceKeepExisting))
	}
	if prompt.ClipboardSupported {
		options = append(options, huh.NewOption("Read from clipboard", initSecretSourceClipboard))
	}
	options = append(options, huh.NewOption("Paste in terminal", initSecretSourcePaste))
	if prompt.Optional {
		options = append(options, huh.NewOption("Skip this optional value", initSecretSourceSkip))
	}
	choice := options[0].Value
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initSecretSource]().
				Title(fmt.Sprintf("How should init get %s?", prompt.Key)).
				Options(options...).
				Value(&choice),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return "", err
	}
	return choice, nil
}

func (p huhInitSecretPrompter) PasteSecret(prompt initSecretValuePrompt) (string, error) {
	var value string
	field := huh.NewInput().
		Title(fmt.Sprintf("Paste %s", prompt.Key)).
		Description("Secret input is hidden.").
		Value(&value).
		EchoMode(huh.EchoModePassword).
		Validate(validateRequiredText("secret value is required"))
	if prompt.Key == credentials.GitHubAppPrivateKeyKey {
		field.Description("Secret input is hidden. Clipboard is recommended for multi-line private keys.")
	}
	form := huh.NewForm(huh.NewGroup(field)).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return "", err
	}
	return credentials.TrimSecretIngress(value), nil
}

func (p huhInitModelMapPrompter) EditModelMap(prompt initModelMapPrompt) (initModelMapEdit, error) {
	action := initModelMapActionPreserve
	options := []huh.Option[initModelMapAction]{
		huh.NewOption("Keep current model-map overrides", initModelMapActionPreserve),
		huh.NewOption("Edit tier mappings", initModelMapActionEdit),
	}
	if len(prompt.ModelMap) > 0 {
		options = append(options, huh.NewOption("Reset all overrides to built-in defaults", initModelMapActionReset))
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initModelMapAction]().
				Title("Model-map overrides").
				Options(options...).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initModelMapEdit{}, err
	}
	switch action {
	case initModelMapActionPreserve:
		return initModelMapEdit{Apply: false}, nil
	case initModelMapActionReset:
		return initModelMapEdit{Apply: true, ModelMap: nil}, nil
	case initModelMapActionEdit:
	default:
		return initModelMapEdit{}, fmt.Errorf("unsupported model-map action %q", action)
	}

	existing := copyModelMap(prompt.ModelMap)
	values := map[config.ModelTier]*string{}
	fields := make([]huh.Field, 0, len(config.ModelTiers()))
	builtIns := config.BuiltInModelMap(prompt.LLM.Provider, prompt.LLM.Adapter)
	for _, tier := range config.ModelTiers() {
		value := strings.TrimSpace(existing[string(tier)])
		values[tier] = &value
		description := initModelMapInputDescription(tier, builtIns[string(tier)])
		fields = append(fields,
			huh.NewInput().
				Title(fmt.Sprintf("%s model", tier)).
				Description(description).
				Value(values[tier]),
		)
	}
	form = huh.NewForm(huh.NewGroup(fields...).Title("Model tiers")).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initModelMapEdit{}, err
	}
	edited := config.ModelMap{}
	for _, tier := range config.ModelTiers() {
		value := strings.TrimSpace(*values[tier])
		if value == "" {
			continue
		}
		edited[string(tier)] = value
	}
	if len(edited) == 0 {
		edited = nil
	}
	return initModelMapEdit{Apply: true, ModelMap: edited}, nil
}

func (p huhInitAgentSourcesPrompter) EditAgentSources(prompt initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
	action := initAgentSourcesActionPreserve
	options := []huh.Option[initAgentSourcesAction]{
		huh.NewOption("Keep current agent source paths", initAgentSourcesActionPreserve),
		huh.NewOption("Edit agent source paths", initAgentSourcesActionEdit),
	}
	if len(prompt.Sources) > 0 {
		options = append(options, huh.NewOption("Reset all agent source paths", initAgentSourcesActionReset))
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initAgentSourcesAction]().
				Title("Agent source paths").
				Options(options...).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initAgentSourcesEdit{}, err
	}
	switch action {
	case initAgentSourcesActionPreserve:
		return initAgentSourcesEdit{Apply: false}, nil
	case initAgentSourcesActionReset:
		return initAgentSourcesEdit{Apply: true, Sources: nil}, nil
	case initAgentSourcesActionEdit:
	default:
		return initAgentSourcesEdit{}, fmt.Errorf("unsupported agent-sources action %q", action)
	}

	values := make([]string, len(prompt.Sources))
	fields := make([]huh.Field, 0, len(values)+1)
	for i := range prompt.Sources {
		values[i] = prompt.Sources[i]
		fields = append(fields,
			huh.NewInput().
				Title(fmt.Sprintf("Agent source %d", i+1)).
				Description("Leave blank to remove this path. Paths are normalized before save.").
				Value(&values[i]),
		)
	}
	var additions string
	fields = append(fields,
		huh.NewInput().
			Title("Add agent sources").
			Description("Optional. Enter comma-separated paths to append.").
			Value(&additions),
	)
	form = huh.NewForm(huh.NewGroup(fields...).Title("Agent sources")).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initAgentSourcesEdit{}, err
	}
	edited := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		edited = append(edited, value)
	}
	for _, value := range strings.Split(additions, ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		edited = append(edited, value)
	}
	if len(edited) == 0 {
		edited = nil
	}
	return initAgentSourcesEdit{Apply: true, Sources: edited}, nil
}

func (p huhInitReviewPolicyPrompter) EditReviewPolicy(prompt initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
	action := initReviewPolicyActionPreserve
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initReviewPolicyAction]().
				Title("Review policy").
				Options(
					huh.NewOption("Keep current review policy", initReviewPolicyActionPreserve),
					huh.NewOption("Edit review policy", initReviewPolicyActionEdit),
				).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initReviewPolicyEdit{}, err
	}
	if action == initReviewPolicyActionPreserve {
		return initReviewPolicyEdit{Apply: false}, nil
	}
	if action != initReviewPolicyActionEdit {
		return initReviewPolicyEdit{}, fmt.Errorf("unsupported review-policy action %q", action)
	}

	policy := prompt.ReviewPolicy
	if policy.MajorEvent == "" {
		policy.MajorEvent = config.ReviewMajorEventComment
	}
	majorEvent := policy.MajorEvent
	selfApprove := policy.AllowSelfApprove
	resolveThreads := string(policy.ResolveThreads)
	resolveAfter := policy.ResolveAfter
	form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[config.ReviewMajorEvent]().
				Title("Major findings event").
				Options(
					huh.NewOption("Comment", config.ReviewMajorEventComment),
					huh.NewOption("Request changes", config.ReviewMajorEventRequestChanges),
				).
				Value(&majorEvent),
			huh.NewConfirm().
				Title("Allow self-approve").
				Value(&selfApprove),
			huh.NewSelect[string]().
				Title("Resolve threads").
				Options(
					huh.NewOption("Use built-in default", ""),
					huh.NewOption("Auto-resolve", string(config.ResolveThreadsAuto)),
					huh.NewOption("Never resolve", string(config.ResolveThreadsNever)),
				).
				Value(&resolveThreads),
			huh.NewInput().
				Title("Resolve-after duration").
				Description("Optional. Leave blank to clear. Example: 24h or 30m.").
				Value(&resolveAfter).
				Validate(validateOptionalDuration),
		).Title("Review policy"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initReviewPolicyEdit{}, err
	}
	return initReviewPolicyEdit{
		Apply: true,
		ReviewPolicy: config.ReviewPolicy{
			MajorEvent:       majorEvent,
			AllowSelfApprove: selfApprove,
			ResolveThreads:   config.ResolveThreadsPolicy(strings.TrimSpace(resolveThreads)),
			ResolveAfter:     strings.TrimSpace(resolveAfter),
		},
	}, nil
}

func (p huhInitRoutesPrompter) EditRoutes(prompt initRoutesPrompt) (initRoutesEdit, error) {
	action := initRoutesActionPreserve
	options := []huh.Option[initRoutesAction]{
		huh.NewOption("Keep current repository routes", initRoutesActionPreserve),
		huh.NewOption("Edit repository routes", initRoutesActionEdit),
	}
	if len(prompt.Routes) > 0 {
		options = append(options, huh.NewOption("Remove all routes for this profile", initRoutesActionReset))
	}
	if prompt.HostChanged && len(prompt.Routes) > 0 {
		options = []huh.Option[initRoutesAction]{
			huh.NewOption("Reconcile repository routes", initRoutesActionEdit),
			huh.NewOption("Remove all routes for this profile", initRoutesActionReset),
		}
		action = initRoutesActionEdit
	}
	routeText := formatInitRouteSpecs(prompt.Routes)
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initRoutesAction]().
				Title("Repository routes").
				Options(options...).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initRoutesEdit{}, err
	}
	switch action {
	case initRoutesActionPreserve:
		return initRoutesEdit{Apply: false}, nil
	case initRoutesActionReset:
		return initRoutesEdit{Apply: true, Routes: nil}, nil
	case initRoutesActionEdit:
	default:
		return initRoutesEdit{}, fmt.Errorf("unsupported routes action %q", action)
	}

	fields := []huh.Field{}
	if prompt.HostChanged && len(prompt.Routes) > 0 {
		fields = append(fields, huh.NewNote().Description(fmt.Sprintf("The profile host changed from %s to %s. Update or remove the affected routes.", prompt.PreviousHost, prompt.ProfileHost)))
	}
	fields = append(fields,
		huh.NewText().
			Title("Repository routes").
			Description("One route per line. Use host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL.").
			Value(&routeText),
	)
	group := huh.NewGroup(fields...).Title("Routes")
	form = huh.NewForm(group).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initRoutesEdit{}, err
	}
	routes, err := parseInitRouteSpecs(routeText)
	if err != nil {
		return initRoutesEdit{}, err
	}
	return initRoutesEdit{Apply: true, Routes: routes}, nil
}

func (p huhInitRetentionPrompter) EditRetention(prompt initRetentionPrompt) (initRetentionEdit, error) {
	action := initRetentionActionPreserve
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initRetentionAction]().
				Title("Data retention").
				Options(
					huh.NewOption("Keep current retention settings", initRetentionActionPreserve),
					huh.NewOption("Edit retention settings", initRetentionActionEdit),
					huh.NewOption("Reset retention to defaults", initRetentionActionReset),
				).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initRetentionEdit{}, err
	}
	switch action {
	case initRetentionActionPreserve:
		return initRetentionEdit{Apply: false}, nil
	case initRetentionActionReset:
		return initRetentionEdit{Apply: true, Retention: config.DefaultRetentionConfig()}, nil
	case initRetentionActionEdit:
	default:
		return initRetentionEdit{}, fmt.Errorf("unsupported retention action %q", action)
	}

	retention := prompt.Retention
	mode := initRetentionMaxAgeDefault
	customDays := ""
	switch {
	case retention.MaxAgeDays != nil && *retention.MaxAgeDays == 0:
		mode = initRetentionMaxAgeForever
	case retention.MaxAgeDays != nil && *retention.MaxAgeDays != config.DefaultRetentionConfig().MaxAgeDaysValue():
		mode = initRetentionMaxAgeCustom
		customDays = fmt.Sprintf("%d", *retention.MaxAgeDays)
	}
	enforcement := string(retention.Enforcement)
	if enforcement == "" {
		enforcement = string(config.RetentionAtWrite)
	}
	form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initRetentionMaxAgeMode]().
				Title("Maximum run-data age").
				Options(
					huh.NewOption("Default 90 days", initRetentionMaxAgeDefault),
					huh.NewOption("Keep forever", initRetentionMaxAgeForever),
					huh.NewOption("Custom days", initRetentionMaxAgeCustom),
				).
				Value(&mode),
			huh.NewInput().
				Title("Custom max age in days").
				Description("Required only when using a custom max age. Use 0 from the mode selector for keep forever.").
				Value(&customDays).
				Validate(func(value string) error {
					if mode != initRetentionMaxAgeCustom {
						return nil
					}
					return validateRetentionMaxAgeDays(value)
				}),
			huh.NewSelect[string]().
				Title("Retention enforcement").
				Options(
					huh.NewOption("At write", string(config.RetentionAtWrite)),
					huh.NewOption("Manual only", string(config.RetentionManualOnly)),
				).
				Value(&enforcement),
		).Title("Retention"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initRetentionEdit{}, err
	}
	next := config.RetentionConfig{
		Enforcement: config.RetentionEnforcement(strings.TrimSpace(enforcement)),
	}
	switch mode {
	case initRetentionMaxAgeDefault:
		defaultDays := config.DefaultRetentionConfig().MaxAgeDaysValue()
		next.MaxAgeDays = &defaultDays
	case initRetentionMaxAgeForever:
		keepForever := 0
		next.MaxAgeDays = &keepForever
	case initRetentionMaxAgeCustom:
		days, err := parseInteractiveRetentionMaxAgeDays(customDays)
		if err != nil {
			return initRetentionEdit{}, err
		}
		next.MaxAgeDays = &days
	default:
		return initRetentionEdit{}, fmt.Errorf("unsupported retention max-age mode %q", mode)
	}
	return initRetentionEdit{Apply: true, Retention: next}, nil
}

func (p huhInitKeyringBackendPrompter) EditKeyringBackend(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	action := initKeyringBackendActionPreserve
	options := []huh.Option[initKeyringBackendAction]{
		huh.NewOption("Keep current keyring backend", initKeyringBackendActionPreserve),
		huh.NewOption("Set keyring backend", initKeyringBackendActionEdit),
	}
	if strings.TrimSpace(prompt.Backend) != "" {
		options = append(options, huh.NewOption("Reset backend to auto selection", initKeyringBackendActionReset))
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initKeyringBackendAction]().
				Title("Keyring backend").
				Options(options...).
				Value(&action),
		),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initKeyringBackendEdit{}, err
	}
	switch action {
	case initKeyringBackendActionPreserve:
		return initKeyringBackendEdit{Apply: false}, nil
	case initKeyringBackendActionReset:
		return initKeyringBackendEdit{Apply: true, Backend: ""}, nil
	case initKeyringBackendActionEdit:
	default:
		return initKeyringBackendEdit{}, fmt.Errorf("unsupported keyring-backend action %q", action)
	}

	backend := strings.TrimSpace(prompt.Backend)
	form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Persistent keyring backend").
				Description("Examples include file or memory. Leave runtime-only --backend choices out unless you want them saved.").
				Value(&backend).
				Validate(validateRequiredText("keyring backend is required")),
		).Title("Keyring"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initKeyringBackendEdit{}, err
	}
	return initKeyringBackendEdit{Apply: true, Backend: strings.TrimSpace(backend)}, nil
}

func initModelMapInputDescription(tier config.ModelTier, builtIn string) string {
	if builtIn = strings.TrimSpace(builtIn); builtIn != "" {
		return fmt.Sprintf("Leave blank to use the built-in %s model: %s", tier, builtIn)
	}
	return "Leave blank to remove the override for this tier."
}

func validateRequiredText(message string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return errors.New(message)
		}
		return nil
	}
}

func validateProfileName(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("profile name is required")
	}
	return nil
}

func validateOptionalCredentialRef(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	_, err := credentials.ParseRef(trimmed)
	return err
}

func validateOptionalDuration(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	_, err := time.ParseDuration(trimmed)
	return err
}

func validateRetentionMaxAgeDays(value string) error {
	_, err := parseInteractiveRetentionMaxAgeDays(value)
	return err
}

func parseInteractiveRetentionMaxAgeDays(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("custom max age is required")
	}
	days, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("custom max age must be a whole number")
	}
	if days < 0 {
		return 0, fmt.Errorf("custom max age must be non-negative")
	}
	return days, nil
}

func validateInteractiveInitFlags(cmd *cobra.Command, flags initOptions) error {
	if flags.overwrite {
		return fmt.Errorf("--overwrite is only supported with --non-interactive")
	}
	if flags.gitTokenStdin || flags.gitTokenEnv != "" || flags.reviewerTokenStdin || flags.reviewerTokenEnv != "" || flags.llmKeyStdin || flags.llmKeyEnv != "" {
		return fmt.Errorf("secret ingress flags are only supported with --non-interactive")
	}
	anyNonInteractiveParityFlagSet := flags.disableReviewer ||
		flags.clearLLMReviewer ||
		flags.setDefault ||
		flags.resetKeyring ||
		cmd.Flags().Changed("git-auth-mode") ||
		cmd.Flags().Changed("llm-reviewer-model-tier") ||
		cmd.Flags().Changed("keyring-backend")
	if anyNonInteractiveParityFlagSet {
		return fmt.Errorf("non-interactive parity flags are only supported with --non-interactive")
	}
	return nil
}

func seedInteractiveInitDraft(requestedProfileName string, existingProfileName string, defaultProfileName string, existingProfile *config.Profile) initDraft {
	profileName := requestedProfileName
	if existingProfileName != "" {
		profileName = existingProfileName
	}
	if strings.TrimSpace(profileName) == "" {
		profileName = credstore.DefaultProfile
	}
	draft := initDraft{
		OriginalProfileName: existingProfileName,
		ProfileName:         profileName,
		MakeDefault:         existingProfileName == "" && defaultProfileName == "",
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		ReviewerAuth:        string(config.GitAuthModePAT),
		LLMProvider:         string(config.LLMProviderAnthropic),
		LLMAuth:             string(config.LLMAuthSubscription),
		LLMAdapter:          string(config.LLMAdapterClaudeCLI),
	}
	if existingProfile != nil {
		draft.MakeDefault = defaultProfileName == existingProfileName
		draft.GitHost = existingProfile.Git.Host
		draft.GitAuth = string(existingProfile.Git.AuthMode)
		draft.GitCredentialRef = existingProfile.Git.CredentialRef
		draft.LLMProvider = string(existingProfile.LLM.Provider)
		draft.LLMAuth = string(existingProfile.LLM.Auth)
		draft.LLMAdapter = string(existingProfile.LLM.Adapter)
		draft.LLMReviewerModelTier = string(existingProfile.LLM.ReviewerModelTier)
		draft.LLMCredentialRef = existingProfile.LLM.CredentialRef
		if existingProfile.ReviewerCredentials != nil {
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(existingProfile.ReviewerCredentials.AuthMode)
			draft.ReviewerCredentialRef = existingProfile.ReviewerCredentials.CredentialRef
		}
	}
	return draft
}

func sortedProfileNames(profiles map[string]config.Profile) []string {
	if len(profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func buildNonInteractiveInitPlan(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps) (initPlan, error) {
	stdinSecrets := 0
	for _, stdin := range []bool{flags.gitTokenStdin, flags.reviewerTokenStdin, flags.llmKeyStdin} {
		if stdin {
			stdinSecrets++
		}
	}
	if stdinSecrets > 1 {
		return initPlan{}, exitcode.Usage(fmt.Errorf("only one stdin secret ingress flag may be set"))
	}
	if flags.keyringBackend != "" && flags.resetKeyring {
		return initPlan{}, exitcode.Usage(fmt.Errorf("--keyring-backend and --reset-keyring-backend may not be used together; use one or the other"))
	}
	if flags.llmReviewerTier != "" && flags.clearLLMReviewer {
		return initPlan{}, exitcode.Usage(fmt.Errorf("--llm-reviewer-model-tier and --clear-llm-reviewer-model-tier may not be used together; use one or the other"))
	}
	reviewerCredentialsConfigured := flags.reviewerRef != "" ||
		flags.reviewerTokenStdin ||
		flags.reviewerTokenEnv != "" ||
		cmd.Flags().Changed("reviewer-auth-mode")
	if flags.disableReviewer && reviewerCredentialsConfigured {
		return initPlan{}, exitcode.Usage(fmt.Errorf("--disable-reviewer may not be combined with reviewer credential flags; use one or the other"))
	}
	profileName := opts.Profile
	if profileName == "" {
		profileName = credstore.DefaultProfile
	}
	path, err := deps.configPath(opts)
	if err != nil {
		return initPlan{}, exitcode.AuthConfig(err)
	}
	cfg, exists, err := deps.loadConfig(path)
	if err != nil {
		return initPlan{}, cmderr.Config(err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	var previousProfile *config.Profile
	if existing, ok := cfg.Profiles[profileName]; ok {
		existingCopy := existing
		previousProfile = &existingCopy
	}
	defaultGitRef, err := credentials.FormatRef(profileName)
	if err != nil {
		return initPlan{}, exitcode.Usage(fmt.Errorf("profile %q cannot be used as a credential ref segment: %w", profileName, err))
	}
	gitRef := flags.gitRef
	if gitRef == "" {
		if previousProfile != nil && previousProfile.Git.CredentialRef != "" {
			gitRef = previousProfile.Git.CredentialRef
		} else {
			gitRef = defaultGitRef
		}
	}
	if _, err := credentials.ParseRef(gitRef); err != nil {
		return initPlan{}, exitcode.Usage(err)
	}
	gitMode := config.GitAuthMode(flags.gitAuth)
	if !gitMode.Valid() {
		return initPlan{}, exitcode.Usage(fmt.Errorf("--git-auth-mode %q is invalid: valid values are pat, github_app", flags.gitAuth))
	}
	if !gitMode.Supported() {
		return initPlan{}, exitcode.Usage(fmt.Errorf("--git-auth-mode %s is not supported in v1", flags.gitAuth))
	}
	reviewerRequested := flags.reviewerRef != "" ||
		flags.reviewerTokenStdin ||
		flags.reviewerTokenEnv != "" ||
		cmd.Flags().Changed("reviewer-auth-mode")
	reviewerRef := flags.reviewerRef
	reviewerMode := config.GitAuthMode(flags.reviewerAuth)
	if reviewerRequested {
		if !reviewerMode.Valid() {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--reviewer-auth-mode %q is invalid", flags.reviewerAuth))
		}
		if !reviewerMode.Supported() {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--reviewer-auth-mode %s is not supported in v1", flags.reviewerAuth))
		}
		if reviewerMode != config.GitAuthModePAT && (flags.reviewerTokenStdin || flags.reviewerTokenEnv != "") {
			return initPlan{}, exitcode.Usage(fmt.Errorf("reviewer token ingress requires --reviewer-auth-mode %s", config.GitAuthModePAT))
		}
		if reviewerRef == "" {
			if previousProfile != nil && previousProfile.ReviewerCredentials != nil && previousProfile.ReviewerCredentials.CredentialRef != "" {
				reviewerRef = previousProfile.ReviewerCredentials.CredentialRef
			} else {
				reviewerRef, err = credentials.FormatRef(profileName + "-reviewer")
				if err != nil {
					return initPlan{}, exitcode.Usage(err)
				}
			}
		}
		if _, err := credentials.ParseRef(reviewerRef); err != nil {
			return initPlan{}, exitcode.Usage(err)
		}
		if reviewerRef == gitRef {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--reviewer-credential-ref %q must differ from --git-credential-ref", reviewerRef))
		}
	}
	llmRef := flags.llmRef
	if flags.llmAuth == string(config.LLMAuthAPIKey) && llmRef == "" {
		if previousProfile != nil && previousProfile.LLM.Auth == config.LLMAuthAPIKey && previousProfile.LLM.CredentialRef != "" {
			llmRef = previousProfile.LLM.CredentialRef
		} else {
			llmRef, err = credentials.FormatRef(profileName + "-llm")
			if err != nil {
				return initPlan{}, exitcode.Usage(err)
			}
		}
	}
	if llmRef != "" {
		if _, err := credentials.ParseRef(llmRef); err != nil {
			return initPlan{}, exitcode.Usage(err)
		}
	}
	if gitMode != config.GitAuthModePAT && (flags.gitTokenStdin || flags.gitTokenEnv != "") {
		return initPlan{}, exitcode.Usage(fmt.Errorf("git token ingress requires --git-auth-mode %s", config.GitAuthModePAT))
	}
	gitSecret, hasGitSecret, err := readInitSecret(deps, opts.Stdin, flags.gitTokenStdin, flags.gitTokenEnv, "--git-token-stdin", "--git-token-from-env")
	if err != nil {
		return initPlan{}, exitcode.Usage(err)
	}
	reviewerSecret, hasReviewerSecret, err := readInitSecret(deps, opts.Stdin, flags.reviewerTokenStdin, flags.reviewerTokenEnv, "--reviewer-token-stdin", "--reviewer-token-from-env")
	if err != nil {
		return initPlan{}, exitcode.Usage(err)
	}
	llmSecret, hasLLMSecret, err := readInitSecret(deps, opts.Stdin, flags.llmKeyStdin, flags.llmKeyEnv, "--llm-api-key-stdin", "--llm-api-key-from-env")
	if err != nil {
		return initPlan{}, exitcode.Usage(err)
	}
	if hasLLMSecret && flags.llmAuth != string(config.LLMAuthAPIKey) {
		return initPlan{}, exitcode.Usage(fmt.Errorf("LLM API-key ingress requires --llm-auth %s", config.LLMAuthAPIKey))
	}

	if previousProfile != nil && !flags.replaceProfile {
		return initPlan{}, exitcode.Usage(fmt.Errorf("profile %q already exists; use --replace-profile to replace config only", profileName))
	}
	profile := config.Profile{
		Git: config.GitConfig{
			Host:          flags.gitHost,
			AuthMode:      gitMode,
			CredentialRef: gitRef,
		},
		LLM: config.LLMConfig{
			Provider:      config.LLMProvider(flags.llmProvider),
			Auth:          config.LLMAuth(flags.llmAuth),
			Adapter:       config.LLMAdapter(flags.llmAdapter),
			CredentialRef: llmRef,
		},
		AgentSources: flags.agentSources,
		ReviewPolicy: config.ReviewPolicy{
			MajorEvent:       config.ReviewMajorEvent(flags.majorEvent),
			AllowSelfApprove: flags.selfApprove,
			ResolveThreads:   config.ResolveThreadsPolicy(flags.resolveThreads),
			ResolveAfter:     flags.resolveAfter,
		},
	}
	if flags.clearLLMReviewer {
		profile.LLM.ReviewerModelTier = ""
	} else {
		profile.LLM.ReviewerModelTier = config.ModelTier(strings.TrimSpace(flags.llmReviewerTier))
	}
	if reviewerRequested && !flags.disableReviewer {
		profile.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      reviewerMode,
			CredentialRef: reviewerRef,
		}
	}
	if previousProfile != nil {
		profile.Git.IdentityCache = previousProfile.Git.IdentityCache
		if previousProfile.LLM.ModelMap != nil {
			modelMap := make(config.ModelMap, len(previousProfile.LLM.ModelMap))
			for tier, model := range previousProfile.LLM.ModelMap {
				modelMap[tier] = model
			}
			profile.LLM.ModelMap = modelMap
		}
		if !cmd.Flags().Changed("agent-source") {
			profile.AgentSources = append([]string(nil), previousProfile.AgentSources...)
		}
		if !cmd.Flags().Changed("major-event") &&
			!cmd.Flags().Changed("allow-self-approve") &&
			!cmd.Flags().Changed("resolve-threads") &&
			!cmd.Flags().Changed("resolve-after") {
			profile.ReviewPolicy = previousProfile.ReviewPolicy
		}
		if !cmd.Flags().Changed("llm-reviewer-model-tier") && !flags.clearLLMReviewer {
			profile.LLM.ReviewerModelTier = previousProfile.LLM.ReviewerModelTier
		}
		if flags.disableReviewer {
			profile.ReviewerCredentials = nil
		} else if !reviewerRequested && previousProfile.ReviewerCredentials != nil {
			creds := *previousProfile.ReviewerCredentials
			profile.ReviewerCredentials = &creds
		} else if profile.ReviewerCredentials != nil && previousProfile.ReviewerCredentials != nil {
			profile.ReviewerCredentials.IdentityCache = previousProfile.ReviewerCredentials.IdentityCache
		}
	}
	cfg.Profiles[profileName] = profile
	if !exists {
		cfg.DefaultProfile = profileName
	}
	if flags.setDefault {
		cfg.DefaultProfile = profileName
	}

	writes := map[string]map[string]string{}
	if hasGitSecret {
		addWrite(writes, gitRef, credentials.GitTokenKey, gitSecret)
	}
	if hasReviewerSecret {
		addWrite(writes, reviewerRef, credentials.GitTokenKey, reviewerSecret)
	}
	if hasLLMSecret {
		llmKey, err := credentials.LLMAPIKeyForProvider(profile.LLM.Provider)
		if err != nil {
			return initPlan{}, cmderr.Config(err)
		}
		addWrite(writes, llmRef, llmKey, llmSecret)
	}
	plannedWriteKeys := projectInitPlannedWriteKeys(writes)
	credentialPlan, err := planInitCredentials(previousProfile, profile, plannedWriteKeys)
	if err != nil {
		return initPlan{}, cmderr.Config(err)
	}

	backendFlagSet := cmderr.BackendFlagChanged(cmd)
	if backendFlagSet {
		if _, err := credentials.StoreOptions(opts.Backend, true, cfg); err != nil {
			return initPlan{}, cmderr.Credential(err)
		}
	}
	explicitKeyringBackend := flags.keyringBackend != ""
	if explicitKeyringBackend {
		if _, err := credentials.StoreOptions(flags.keyringBackend, true, cfg); err != nil {
			return initPlan{}, cmderr.Credential(err)
		}
		if backendFlagSet && opts.Backend != flags.keyringBackend {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--backend %q conflicts with --keyring-backend %q", opts.Backend, flags.keyringBackend))
		}
		cfg.Keyring.Backend = flags.keyringBackend
	}
	if flags.resetKeyring {
		cfg.Keyring.Backend = ""
	}
	// Preserve the legacy init behavior where a write-backed runtime backend may
	// still become durable. New scripted flows should prefer --keyring-backend
	// and --reset-keyring-backend for readable, explicit ownership.
	persistRuntimeBackend := !explicitKeyringBackend && !flags.resetKeyring && backendFlagSet && (len(writes) > 0 || profile.LLM.Auth == config.LLMAuthAPIKey)
	if persistRuntimeBackend {
		if cfg.Keyring.Backend != "" && cfg.Keyring.Backend != opts.Backend {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--backend %q conflicts with existing keyring.backend %q", opts.Backend, cfg.Keyring.Backend))
		}
		cfg.Keyring.Backend = opts.Backend
	}

	if err := config.Validate(cfg); err != nil {
		return initPlan{}, cmderr.Config(err)
	}

	backendArg := interactiveInitBackendArg(opts, backendFlagSet, cfg)

	return initPlan{
		path:              path,
		cfg:               cfg,
		previousProfile:   previousProfile,
		profileName:       profileName,
		profile:           profile,
		writes:            writes,
		credentialPlan:    credentialPlan,
		overwriteRefs:     map[string]bool{},
		satisfiedRefs:     map[string]bool{},
		backendFlagSet:    backendFlagSet,
		backendArg:        backendArg,
		llmSecretProvided: hasLLMSecret,
	}, nil
}

func buildInteractiveInitWorkspace(cmd *cobra.Command, opts *root.Options, flags initOptions, _ initDeps, path string, cfg config.File, draft initDraft) (initWorkspaceDraft, error) {
	profileName := strings.TrimSpace(draft.ProfileName)
	if profileName == "" {
		return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("profile name is required"))
	}
	working := cloneInitConfigFile(cfg)
	if working.Profiles == nil {
		working.Profiles = map[string]config.Profile{}
	}
	originalName := strings.TrimSpace(draft.OriginalProfileName)
	var previousProfile *config.Profile
	if originalName != "" {
		profile, ok := working.Profiles[originalName]
		if !ok {
			return initWorkspaceDraft{}, cmderr.Config(fmt.Errorf("%w: %s", config.ErrProfileNotFound, originalName))
		}
		profileCopy := profile
		previousProfile = &profileCopy
	}
	if originalName == "" {
		if _, exists := working.Profiles[profileName]; exists {
			return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("profile %q already exists; select it to edit or choose a different name", profileName))
		}
	} else if profileName != originalName {
		renamed, _, err := configedit.RenameProfile(working, originalName, profileName)
		if err != nil {
			if errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, configedit.ErrProfileExists) || errors.Is(err, configedit.ErrProfileNameRequired) {
				return initWorkspaceDraft{}, exitcode.Usage(err)
			}
			return initWorkspaceDraft{}, cmderr.Config(err)
		}
		working = renamed
	}

	profile, err := synthesizeInteractiveProfile(flags, profileName, previousProfile, draft)
	if err != nil {
		return initWorkspaceDraft{}, err
	}
	working.Profiles[profileName] = profile
	if draft.MakeDefault || working.DefaultProfile == "" {
		var changed bool
		working, changed, err = configedit.SetDefaultProfile(working, profileName)
		_ = changed
		if err != nil {
			if errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, configedit.ErrProfileNameRequired) {
				return initWorkspaceDraft{}, exitcode.Usage(err)
			}
			return initWorkspaceDraft{}, cmderr.Config(err)
		}
	}
	if err := config.Validate(initValidationConfigForProfileHost(working, profileName, previousProfile, profile.Git.Host)); err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}

	credentialPlan, err := planInitCredentials(previousProfile, profile, nil)
	if err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	gitScopes, profileGitScopeNames := buildInitGitScopeInventory(working)
	gitScopeName := profileGitScopeNames[profileName]
	if gitScopeName == "" {
		return initWorkspaceDraft{}, cmderr.Config(fmt.Errorf("draft Git scope missing for profile %q", profileName))
	}
	reviewerEntities, profileReviewerEntityNames := buildInitReviewerEntityInventory(working)
	reviewerEntityName := profileReviewerEntityNames[profileName]
	if reviewerEntityName == "" {
		return initWorkspaceDraft{}, cmderr.Config(fmt.Errorf("draft reviewer entity missing for profile %q", profileName))
	}
	llmRuntimes, profileRuntimeNames := buildInitLLMRuntimeInventory(working)
	llmRuntimeName := profileRuntimeNames[profileName]
	if llmRuntimeName == "" {
		return initWorkspaceDraft{}, cmderr.Config(fmt.Errorf("draft LLM runtime missing for profile %q", profileName))
	}
	deferLLMSecret := profile.LLM.Auth == config.LLMAuthAPIKey
	backendFlagSet := cmderr.BackendFlagChanged(cmd)
	if backendFlagSet {
		if _, err := credentials.StoreOptions(opts.Backend, true, working); err != nil {
			return initWorkspaceDraft{}, cmderr.Credential(err)
		}
	}
	return initWorkspaceDraft{
		path:               path,
		cfg:                working,
		previousProfile:    previousProfile,
		profileName:        profileName,
		profile:            profile,
		gitScopeName:       gitScopeName,
		gitScopes:          gitScopes,
		reviewerEntityName: reviewerEntityName,
		reviewerEntities:   reviewerEntities,
		llmRuntimeName:     llmRuntimeName,
		llmRuntimes:        llmRuntimes,
		writes:             map[string]map[string]string{},
		credentialPlan:     credentialPlan,
		overwriteRefs:      map[string]bool{},
		satisfiedRefs:      map[string]bool{},
		backendFlagSet:     backendFlagSet,
		backendArg:         interactiveInitBackendArg(opts, backendFlagSet, working),
		allowDeferredLLM:   deferLLMSecret,
		writeLLMHint:       deferLLMSecret,
	}, nil
}

func finalizeInteractiveInitPlan(workspace initWorkspaceDraft) initPlan {
	return initPlan{
		path:             workspace.path,
		cfg:              workspace.cfg,
		previousProfile:  workspace.previousProfile,
		profileName:      workspace.profileName,
		profile:          workspace.profile,
		writes:           workspace.writes,
		credentialPlan:   workspace.credentialPlan,
		overwriteRefs:    workspace.overwriteRefs,
		satisfiedRefs:    workspace.satisfiedRefs,
		backendFlagSet:   workspace.backendFlagSet,
		backendArg:       workspace.backendArg,
		allowDeferredLLM: workspace.allowDeferredLLM,
		writeLLMHint:     workspace.writeLLMHint,
	}
}

func recordTouchedProfile(session initSessionDraft, currentName string, draftOriginalName string) initSessionDraft {
	currentName = strings.TrimSpace(currentName)
	if currentName == "" {
		return session
	}
	if session.touchedProfiles == nil {
		session.touchedProfiles = map[string]string{}
	}
	originalName := resolveHistoricProfileName(session, currentName, draftOriginalName)
	if draftOriginalName != "" && strings.TrimSpace(draftOriginalName) != currentName {
		delete(session.touchedProfiles, strings.TrimSpace(draftOriginalName))
	}
	session.touchedProfiles[currentName] = originalName
	return session
}

func resolveHistoricProfileName(session initSessionDraft, currentName string, draftOriginalName string) string {
	originalName := strings.TrimSpace(draftOriginalName)
	if preservedOriginal, ok := session.touchedProfiles[originalName]; ok {
		originalName = preservedOriginal
	}
	if originalName == "" {
		if _, exists := session.originalCfg.Profiles[currentName]; exists {
			originalName = currentName
		}
	}
	return originalName
}

func buildInteractiveInitSessionPlan(opts *root.Options, session initSessionDraft) (initSessionPlan, error) {
	profileNames := sortedTouchedProfileNames(session)
	profileRefs := make(map[string][]config.CredentialRef, len(profileNames))
	entriesByKey := map[string]initCredentialPlanEntry{}
	for _, profileName := range profileNames {
		profile, ok := session.cfg.Profiles[profileName]
		if !ok {
			return initSessionPlan{}, cmderr.Config(fmt.Errorf("%w: %s", config.ErrProfileNotFound, profileName))
		}
		refs, err := config.CredentialRefs(profile)
		if err != nil {
			return initSessionPlan{}, cmderr.Config(err)
		}
		profileRefs[profileName] = append([]config.CredentialRef(nil), refs...)
		for _, ref := range refs {
			key := initCredentialEntryKey(ref)
			if _, exists := entriesByKey[key]; exists {
				continue
			}
			specs, err := credentials.KeySpecsForPurpose(ref)
			if err != nil {
				return initSessionPlan{}, cmderr.Config(err)
			}
			entriesByKey[key] = initCredentialPlanEntry{
				Ref:      ref,
				KeySpecs: append([]credentials.KeySpec(nil), specs...),
				State:    initCredentialPlanStateDefer,
			}
		}
	}
	entryKeys := make([]string, 0, len(entriesByKey))
	for key := range entriesByKey {
		entryKeys = append(entryKeys, key)
	}
	sort.Strings(entryKeys)
	entries := make([]initCredentialPlanEntry, 0, len(entryKeys))
	for _, key := range entryKeys {
		entries = append(entries, entriesByKey[key])
	}
	return initSessionPlan{
		path:           session.path,
		cfg:            cloneInitConfigFile(session.cfg),
		profileNames:   profileNames,
		profileRefs:    profileRefs,
		writes:         map[string]map[string]string{},
		credentialPlan: entries,
		overwriteRefs:  map[string]bool{},
		satisfiedRefs:  map[string]bool{},
		backendFlagSet: session.backendFlagSet,
		backendArg:     interactiveInitBackendArg(opts, session.backendFlagSet, session.cfg),
	}, nil
}

func sortedTouchedProfileNames(session initSessionDraft) []string {
	names := map[string]struct{}{}
	for name := range session.touchedProfiles {
		names[name] = struct{}{}
	}
	profileNames := make([]string, 0, len(names))
	for name := range names {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	return profileNames
}

func initCredentialEntryKey(ref config.CredentialRef) string {
	return strings.Join([]string{ref.Purpose, ref.Ref, ref.Mode, ref.Provider}, "\x00")
}

func initGitScopeDraftFromConfig(git config.GitConfig) initGitScopeDraft {
	return initGitScopeDraft{
		Host:          strings.TrimSpace(git.Host),
		AuthMode:      git.AuthMode,
		CredentialRef: strings.TrimSpace(git.CredentialRef),
	}
}

func (scope initGitScopeDraft) exportConfig(previous *config.GitConfig) config.GitConfig {
	git := config.GitConfig{
		Host:          strings.TrimSpace(scope.Host),
		AuthMode:      scope.AuthMode,
		CredentialRef: strings.TrimSpace(scope.CredentialRef),
	}
	if previous != nil && scope.matchesConfig(*previous) {
		git.Host = previous.Host
		git.IdentityCache = previous.IdentityCache
	}
	return git
}

func (scope initGitScopeDraft) identityKey() string {
	return strings.Join([]string{
		config.NormalizeHost(scope.Host),
		string(scope.AuthMode),
		strings.TrimSpace(scope.CredentialRef),
	}, "\x00")
}

func (scope initGitScopeDraft) suggestedName() string {
	host := strings.ReplaceAll(config.NormalizeHost(scope.Host), ".", "-")
	host = strings.ReplaceAll(host, "/", "-")
	host = strings.Trim(host, "-")
	if host == "" {
		host = "git-scope"
	}
	return fmt.Sprintf("%s-%s", host, scope.AuthMode)
}

func (scope initGitScopeDraft) matchesConfig(previous config.GitConfig) bool {
	return config.NormalizeHost(previous.Host) == config.NormalizeHost(scope.Host) &&
		previous.AuthMode == scope.AuthMode &&
		strings.TrimSpace(previous.CredentialRef) == strings.TrimSpace(scope.CredentialRef)
}

func buildInitGitScopeInventory(cfg config.File) (map[string]initGitScopeDraft, map[string]string) {
	scopes := map[string]initGitScopeDraft{}
	profileScopeNames := map[string]string{}
	scopeNamesByKey := map[string]string{}
	for _, profileName := range sortedProfileNames(cfg.Profiles) {
		profile := cfg.Profiles[profileName]
		scope := initGitScopeDraftFromConfig(profile.Git)
		key := scope.identityKey()
		if existingName, ok := scopeNamesByKey[key]; ok {
			profileScopeNames[profileName] = existingName
			continue
		}
		scope.Name = uniqueInitGitScopeName(scopes, scope.suggestedName())
		scopes[scope.Name] = scope
		scopeNamesByKey[key] = scope.Name
		profileScopeNames[profileName] = scope.Name
	}
	return scopes, profileScopeNames
}

func uniqueInitGitScopeName(existing map[string]initGitScopeDraft, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "git-scope"
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

func initReviewerEntityDraftFromConfig(profile config.Profile) initReviewerEntityDraft {
	if profile.ReviewerCredentials == nil {
		return initReviewerEntityDraft{
			Kind: initReviewerEntityKindUseGitIdentity,
		}
	}
	entity := initReviewerEntityDraft{
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		CredentialRef: strings.TrimSpace(profile.ReviewerCredentials.CredentialRef),
	}
	switch profile.ReviewerCredentials.AuthMode {
	case config.GitAuthModeGitHubApp:
		entity.Kind = initReviewerEntityKindGitHubApp
	case config.GitAuthModePAT, config.GitAuthModeOAuthDevice:
		entity.Kind = initReviewerEntityKindPAT
	}
	return entity
}

func (entity initReviewerEntityDraft) exportConfig(previous *config.ReviewerCredentials) *config.ReviewerCredentials {
	if entity.Kind == initReviewerEntityKindUseGitIdentity {
		return nil
	}
	reviewer := &config.ReviewerCredentials{
		AuthMode:      entity.AuthMode,
		CredentialRef: strings.TrimSpace(entity.CredentialRef),
	}
	if previous != nil && entity.matchesConfig(*previous) {
		reviewer.IdentityCache = previous.IdentityCache
	}
	return reviewer
}

func (entity initReviewerEntityDraft) identityKey() string {
	if entity.Kind == initReviewerEntityKindUseGitIdentity {
		return string(entity.Kind)
	}
	return strings.Join([]string{
		string(entity.Kind),
		string(entity.AuthMode),
		strings.TrimSpace(entity.CredentialRef),
	}, "\x00")
}

func (entity initReviewerEntityDraft) suggestedName() string {
	switch entity.Kind {
	case initReviewerEntityKindUseGitIdentity:
		return "use-git-identity"
	case initReviewerEntityKindGitHubApp:
		return "reviewer-github-app"
	case initReviewerEntityKindPAT:
		return "reviewer-pat"
	}
	return "reviewer-entity"
}

func (entity initReviewerEntityDraft) matchesConfig(previous config.ReviewerCredentials) bool {
	return entity.Kind == initReviewerEntityDraftFromConfig(config.Profile{ReviewerCredentials: &previous}).Kind &&
		previous.AuthMode == entity.AuthMode &&
		strings.TrimSpace(previous.CredentialRef) == strings.TrimSpace(entity.CredentialRef)
}

func buildInitReviewerEntityInventory(cfg config.File) (map[string]initReviewerEntityDraft, map[string]string) {
	entities := map[string]initReviewerEntityDraft{}
	profileEntityNames := map[string]string{}
	entityNamesByKey := map[string]string{}
	for _, profileName := range sortedProfileNames(cfg.Profiles) {
		profile := cfg.Profiles[profileName]
		entity := initReviewerEntityDraftFromConfig(profile)
		key := entity.identityKey()
		if existingName, ok := entityNamesByKey[key]; ok {
			profileEntityNames[profileName] = existingName
			continue
		}
		entity.Name = uniqueInitReviewerEntityName(entities, entity.suggestedName())
		entities[entity.Name] = entity
		entityNamesByKey[key] = entity.Name
		profileEntityNames[profileName] = entity.Name
	}
	return entities, profileEntityNames
}

func uniqueInitReviewerEntityName(existing map[string]initReviewerEntityDraft, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "reviewer-entity"
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

func initLLMRuntimeDraftFromConfig(llm config.LLMConfig) initLLMRuntimeDraft {
	runtime := initLLMRuntimeDraft{
		Preset:        initLLMRuntimePresetCustom,
		Provider:      llm.Provider,
		Auth:          llm.Auth,
		Adapter:       llm.Adapter,
		CredentialRef: strings.TrimSpace(llm.CredentialRef),
	}
	switch {
	case runtime.Provider == config.LLMProviderAnthropic && runtime.Auth == config.LLMAuthSubscription && runtime.Adapter == config.LLMAdapterClaudeCLI:
		runtime.Preset = initLLMRuntimePresetClaudeCLISubscription
	case runtime.Provider == config.LLMProviderOpenAI && runtime.Auth == config.LLMAuthSubscription && runtime.Adapter == config.LLMAdapterCodexCLI:
		runtime.Preset = initLLMRuntimePresetCodexCLISubscription
	case runtime.Provider == config.LLMProviderPi && runtime.Auth == config.LLMAuthSubscription && runtime.Adapter == config.LLMAdapterPiRPC:
		runtime.Preset = initLLMRuntimePresetPiLocal
	case runtime.Provider == config.LLMProviderAnthropic && runtime.Auth == config.LLMAuthAPIKey && runtime.Adapter == config.LLMAdapterAnthropicAPI:
		runtime.Preset = initLLMRuntimePresetAnthropicAPIKey
	case runtime.Provider == config.LLMProviderOpenAI && runtime.Auth == config.LLMAuthAPIKey && runtime.Adapter == config.LLMAdapterOpenAIAPI:
		runtime.Preset = initLLMRuntimePresetOpenAIAPIKey
	}
	if runtime.Auth == config.LLMAuthSubscription {
		runtime.CredentialRef = ""
	}
	return runtime
}

func (runtime initLLMRuntimeDraft) exportConfig() config.LLMConfig {
	llm := config.LLMConfig{
		Provider: runtime.Provider,
		Auth:     runtime.Auth,
		Adapter:  runtime.Adapter,
	}
	if runtime.Auth == config.LLMAuthAPIKey {
		llm.CredentialRef = strings.TrimSpace(runtime.CredentialRef)
	}
	return llm
}

func (runtime initLLMRuntimeDraft) identityKey() string {
	return strings.Join([]string{
		string(runtime.Provider),
		string(runtime.Auth),
		string(runtime.Adapter),
		strings.TrimSpace(runtime.CredentialRef),
	}, "\x00")
}

func (runtime initLLMRuntimeDraft) suggestedName() string {
	switch runtime.Preset {
	case initLLMRuntimePresetClaudeCLISubscription:
		return "claude-cli"
	case initLLMRuntimePresetCodexCLISubscription:
		return "codex-cli"
	case initLLMRuntimePresetPiLocal:
		return "pi-local"
	case initLLMRuntimePresetAnthropicAPIKey:
		return "anthropic-api-key"
	case initLLMRuntimePresetOpenAIAPIKey:
		return "openai-api-key"
	case initLLMRuntimePresetCustom:
		return fmt.Sprintf("%s-%s-%s", runtime.Provider, runtime.Auth, runtime.Adapter)
	}
	return "llm-runtime"
}

func buildInitLLMRuntimeInventory(cfg config.File) (map[string]initLLMRuntimeDraft, map[string]string) {
	runtimes := map[string]initLLMRuntimeDraft{}
	profileRuntimeNames := map[string]string{}
	runtimeNamesByKey := map[string]string{}
	for _, profileName := range sortedProfileNames(cfg.Profiles) {
		profile := cfg.Profiles[profileName]
		runtime := initLLMRuntimeDraftFromConfig(profile.LLM)
		key := runtime.identityKey()
		if existingName, ok := runtimeNamesByKey[key]; ok {
			profileRuntimeNames[profileName] = existingName
			continue
		}
		runtime.Name = uniqueInitLLMRuntimeName(runtimes, runtime.suggestedName())
		runtimes[runtime.Name] = runtime
		runtimeNamesByKey[key] = runtime.Name
		profileRuntimeNames[profileName] = runtime.Name
	}
	return runtimes, profileRuntimeNames
}

func uniqueInitLLMRuntimeName(existing map[string]initLLMRuntimeDraft, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "llm-runtime"
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

func cloneInitConfigFile(cfg config.File) config.File {
	cloned := cfg
	if cfg.Profiles != nil {
		cloned.Profiles = make(map[string]config.Profile, len(cfg.Profiles))
		for name, profile := range cfg.Profiles {
			cloned.Profiles[name] = cloneInitProfile(profile)
		}
	}
	if len(cfg.RepositoryProfiles) > 0 {
		cloned.RepositoryProfiles = make([]config.RepositoryProfile, len(cfg.RepositoryProfiles))
		for i, route := range cfg.RepositoryProfiles {
			clonedRoute := route
			if len(route.Match.Repos) > 0 {
				clonedRoute.Match.Repos = append([]string(nil), route.Match.Repos...)
			}
			cloned.RepositoryProfiles[i] = clonedRoute
		}
	}
	if cfg.Data.Retention.MaxAgeDays != nil {
		value := *cfg.Data.Retention.MaxAgeDays
		cloned.Data.Retention.MaxAgeDays = &value
	}
	return cloned
}

func cloneInitProfile(profile config.Profile) config.Profile {
	cloned := profile
	if profile.ReviewerCredentials != nil {
		reviewer := *profile.ReviewerCredentials
		cloned.ReviewerCredentials = &reviewer
	}
	cloned.LLM = cloneInitLLMConfig(profile.LLM)
	if len(profile.AgentSources) > 0 {
		cloned.AgentSources = append([]string(nil), profile.AgentSources...)
	}
	return cloned
}

func cloneInitLLMConfig(llm config.LLMConfig) config.LLMConfig {
	cloned := llm
	if len(llm.ModelMap) > 0 {
		cloned.ModelMap = make(config.ModelMap, len(llm.ModelMap))
		for tier, model := range llm.ModelMap {
			cloned.ModelMap[tier] = model
		}
	}
	return cloned
}

func initValidationConfigForProfileHost(cfg config.File, profileName string, previousProfile *config.Profile, profileHost string) config.File {
	if previousProfile == nil {
		return cfg
	}
	if config.NormalizeHost(previousProfile.Git.Host) == config.NormalizeHost(profileHost) {
		return cfg
	}
	copyCfg := cfg
	if len(cfg.RepositoryProfiles) == 0 {
		return copyCfg
	}
	copyCfg.RepositoryProfiles = append([]config.RepositoryProfile(nil), cfg.RepositoryProfiles...)
	for i, route := range copyCfg.RepositoryProfiles {
		if route.Profile != profileName {
			continue
		}
		route.Match.Host = profileHost
		copyCfg.RepositoryProfiles[i] = route
	}
	return copyCfg
}

func synthesizeInteractiveProfile(flags initOptions, profileName string, previousProfile *config.Profile, draft initDraft) (config.Profile, error) {
	profile := config.Profile{}
	if previousProfile != nil {
		profile = *previousProfile
		if previousProfile.ReviewerCredentials != nil {
			creds := *previousProfile.ReviewerCredentials
			profile.ReviewerCredentials = &creds
		}
	} else {
		profile.Git.Host = flags.gitHost
		profile.Git.AuthMode = config.GitAuthModePAT
		profile.LLM.Provider = config.LLMProvider(flags.llmProvider)
		profile.LLM.Auth = config.LLMAuth(flags.llmAuth)
		profile.LLM.Adapter = config.LLMAdapter(flags.llmAdapter)
	}
	defaultGitRef, err := credentials.FormatRef(profileName)
	if err != nil {
		return config.Profile{}, exitcode.Usage(fmt.Errorf("profile %q cannot be used as a credential ref segment: %w", profileName, err))
	}
	profile.Git.Host = strings.TrimSpace(draft.GitHost)
	profile.Git.AuthMode = config.GitAuthMode(draft.GitAuth)
	profile.Git.CredentialRef = strings.TrimSpace(draft.GitCredentialRef)
	if profile.Git.CredentialRef == "" {
		profile.Git.CredentialRef = defaultGitRef
	}
	// Changing the selected Git scope means any cached resolved identity may belong to the wrong host/auth pair.
	if previousProfile != nil && !initGitScopeDraftFromConfig(profile.Git).matchesConfig(previousProfile.Git) {
		profile.Git.IdentityCache = ""
	}
	if draft.ReviewerEnabled {
		reviewerRef := strings.TrimSpace(draft.ReviewerCredentialRef)
		if reviewerRef == "" {
			reviewerRef, err = credentials.FormatRef(profileName + "-reviewer")
			if err != nil {
				return config.Profile{}, exitcode.Usage(err)
			}
		}
		reviewer := config.ReviewerCredentials{
			AuthMode:      config.GitAuthMode(draft.ReviewerAuth),
			CredentialRef: reviewerRef,
		}
		// Changing reviewer credentials can invalidate the cached reviewer identity even when the profile name stays the same.
		if previousProfile != nil && previousProfile.ReviewerCredentials != nil && initReviewerEntityDraftFromConfig(config.Profile{ReviewerCredentials: &reviewer}).matchesConfig(*previousProfile.ReviewerCredentials) {
			reviewer.IdentityCache = previousProfile.ReviewerCredentials.IdentityCache
		}
		profile.ReviewerCredentials = &reviewer
	} else {
		profile.ReviewerCredentials = nil
	}
	profile.LLM.Provider = config.LLMProvider(draft.LLMProvider)
	profile.LLM.Auth = config.LLMAuth(draft.LLMAuth)
	profile.LLM.Adapter = config.LLMAdapter(draft.LLMAdapter)
	profile.LLM.ReviewerModelTier = config.ModelTier(strings.TrimSpace(draft.LLMReviewerModelTier))
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		llmRef := strings.TrimSpace(draft.LLMCredentialRef)
		if llmRef == "" {
			llmRef, err = credentials.FormatRef(profileName + "-llm")
			if err != nil {
				return config.Profile{}, exitcode.Usage(err)
			}
		}
		profile.LLM.CredentialRef = llmRef
	} else {
		profile.LLM.CredentialRef = ""
	}
	return profile, nil
}

func collectInteractiveInitModelMap(opts *root.Options, deps initDeps, plan initWorkspaceDraft) (initWorkspaceDraft, error) {
	prompter := deps.modelMapPrompter
	if prompter == nil {
		if deps.prompter != nil {
			return plan, nil
		}
		prompter = newHuhInitModelMapPrompter(opts)
	}
	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM:      plan.profile.LLM,
		ModelMap: copyModelMap(plan.profile.LLM.ModelMap),
	})
	if err != nil {
		return initWorkspaceDraft{}, err
	}
	if !edit.Apply {
		return plan, nil
	}
	nextProfile := plan.profile
	nextProfile.LLM.ModelMap = normalizeInitModelMap(edit.ModelMap)
	nextCfg := plan.cfg
	nextCfg.Profiles[plan.profileName] = nextProfile
	if err := config.Validate(nextCfg); err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	plan.profile = nextProfile
	plan.cfg = nextCfg
	return plan, nil
}

func collectInteractiveInitAgentSources(opts *root.Options, deps initDeps, plan initWorkspaceDraft) (initWorkspaceDraft, error) {
	prompter := deps.agentSourcesPrompter
	if prompter == nil {
		if deps.prompter != nil {
			return plan, nil
		}
		prompter = newHuhInitAgentSourcesPrompter(opts)
	}
	edit, err := prompter.EditAgentSources(initAgentSourcesPrompt{
		Sources: append([]string(nil), plan.profile.AgentSources...),
	})
	if err != nil {
		return initWorkspaceDraft{}, err
	}
	if !edit.Apply {
		return plan, nil
	}
	nextSources, err := normalizeInitAgentSources(edit.Sources)
	if err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	nextProfile := plan.profile
	nextProfile.AgentSources = nextSources
	nextCfg := plan.cfg
	nextCfg.Profiles[plan.profileName] = nextProfile
	if err := config.Validate(nextCfg); err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	plan.profile = nextProfile
	plan.cfg = nextCfg
	return plan, nil
}

func collectInteractiveInitReviewPolicy(opts *root.Options, deps initDeps, plan initWorkspaceDraft) (initWorkspaceDraft, error) {
	prompter := deps.reviewPolicyPrompter
	if prompter == nil {
		if deps.prompter != nil {
			return plan, nil
		}
		prompter = newHuhInitReviewPolicyPrompter(opts)
	}
	edit, err := prompter.EditReviewPolicy(initReviewPolicyPrompt{
		ReviewPolicy: plan.profile.ReviewPolicy,
	})
	if err != nil {
		return initWorkspaceDraft{}, err
	}
	if !edit.Apply {
		return plan, nil
	}
	nextProfile := plan.profile
	nextProfile.ReviewPolicy = edit.ReviewPolicy
	nextCfg := plan.cfg
	nextCfg.Profiles[plan.profileName] = nextProfile
	if err := config.Validate(nextCfg); err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	plan.profile = nextProfile
	plan.cfg = nextCfg
	return plan, nil
}

func normalizeInitAgentSources(sources []string) ([]string, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	var normalized []string
	for _, source := range sources {
		if strings.TrimSpace(source) == "" {
			continue
		}
		var err error
		normalized, _, err = configedit.AddAgentSource(normalized, source)
		if err != nil {
			return nil, err
		}
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func collectInteractiveInitRoutes(opts *root.Options, deps initDeps, plan initWorkspaceDraft) (initWorkspaceDraft, error) {
	prompter := deps.routesPrompter
	if prompter == nil {
		if deps.prompter != nil {
			return plan, nil
		}
		prompter = newHuhInitRoutesPrompter(opts)
	}
	previousHost := ""
	hostChanged := false
	if plan.previousProfile != nil {
		previousHost = plan.previousProfile.Git.Host
		hostChanged = config.NormalizeHost(previousHost) != config.NormalizeHost(plan.profile.Git.Host)
	}
	edit, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName:  plan.profileName,
		ProfileHost:  plan.profile.Git.Host,
		PreviousHost: previousHost,
		HostChanged:  hostChanged,
		Routes:       currentProfileRouteSpecs(plan.cfg.RepositoryProfiles, plan.profileName),
	})
	if err != nil {
		return initWorkspaceDraft{}, err
	}
	if !edit.Apply {
		if err := validateInitRouteHosts(plan.cfg.RepositoryProfiles, plan.profileName, plan.profile.Git.Host); err != nil {
			return initWorkspaceDraft{}, exitcode.Usage(err)
		}
		return plan, nil
	}
	nextRoutes, err := applyInitProfileRoutes(plan.cfg.RepositoryProfiles, plan.profileName, plan.profile.Git.Host, edit.Routes)
	if err != nil {
		return initWorkspaceDraft{}, exitcode.Usage(err)
	}
	nextCfg := plan.cfg
	nextCfg.RepositoryProfiles = nextRoutes
	if err := config.Validate(nextCfg); err != nil {
		return initWorkspaceDraft{}, cmderr.Config(err)
	}
	plan.cfg = nextCfg
	return plan, nil
}

func currentProfileRouteSpecs(routes []config.RepositoryProfile, profileName string) []configedit.RepositoryRouteSpec {
	if len(routes) == 0 {
		return nil
	}
	canonical := configedit.CanonicalRepositoryRoutes(routes)
	specs := make([]configedit.RepositoryRouteSpec, 0, len(canonical))
	for _, route := range canonical {
		if route.Profile != profileName {
			continue
		}
		specs = append(specs, configedit.RepositoryRouteSpec{
			Host:      route.Match.Host,
			Namespace: route.Match.Namespace,
			Repos:     append([]string(nil), route.Match.Repos...),
		})
	}
	if len(specs) == 0 {
		return nil
	}
	return specs
}

func validateInitRouteHosts(routes []config.RepositoryProfile, profileName string, profileHost string) error {
	wantHost := config.NormalizeHost(profileHost)
	for _, spec := range currentProfileRouteSpecs(routes, profileName) {
		if spec.Host != wantHost {
			return fmt.Errorf("profile %q has repository routes on host %q but git.host is %q; edit routes to reconcile the host change", profileName, spec.Host, profileHost)
		}
	}
	return nil
}

func applyInitProfileRoutes(routes []config.RepositoryProfile, profileName string, profileHost string, specs []configedit.RepositoryRouteSpec) ([]config.RepositoryProfile, error) {
	pruned := configedit.PruneRepositoryProfileRoutes(routes, profileName)
	normalizedHost := config.NormalizeHost(profileHost)
	var err error
	for _, spec := range specs {
		spec, err = configedit.NormalizeRepositoryRouteSpec(spec)
		if err != nil {
			return nil, err
		}
		if spec.Host != normalizedHost {
			return nil, fmt.Errorf("route host %q does not match selected profile host %q", spec.Host, profileHost)
		}
		pruned, err = configedit.SetRepositoryRoutes(pruned, profileName, spec)
		if err != nil {
			return nil, err
		}
	}
	return pruned, nil
}

func formatInitRouteSpecs(specs []configedit.RepositoryRouteSpec) string {
	if len(specs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(specs))
	for _, spec := range specs {
		lines = append(lines, configedit.FormatRepositoryRouteSpec(spec))
	}
	return strings.Join(lines, "\n")
}

func parseInitRouteSpecs(raw string) ([]configedit.RepositoryRouteSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	lines := strings.Split(raw, "\n")
	specs := make([]configedit.RepositoryRouteSpec, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		spec, err := parseInitRouteSpec(line)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, nil
	}
	return specs, nil
}

func parseInitRouteSpec(raw string) (configedit.RepositoryRouteSpec, error) {
	if ref, err := prref.ParseGitHubPullURL(raw); err == nil {
		return configedit.NormalizeRepositoryRouteSpec(configedit.RepositoryRouteSpec{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repos:     []string{ref.Repo},
		})
	}
	trimmed := strings.TrimSpace(raw)
	repos := []string(nil)
	if open := strings.Index(trimmed, "["); open >= 0 {
		closeIndex := strings.LastIndex(trimmed, "]")
		if closeIndex <= open {
			return configedit.RepositoryRouteSpec{}, fmt.Errorf("route %q has an invalid repo list", raw)
		}
		repoList := strings.TrimSpace(trimmed[open+1 : closeIndex])
		if repoList != "" {
			repos = strings.Split(repoList, ",")
		}
		trimmed = strings.TrimSpace(trimmed[:open])
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	switch len(parts) {
	case 2:
	case 3:
		repos = append(repos, parts[2])
	default:
		return configedit.RepositoryRouteSpec{}, fmt.Errorf("route %q must be host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL", raw)
	}
	return configedit.NormalizeRepositoryRouteSpec(configedit.RepositoryRouteSpec{
		Host:      parts[0],
		Namespace: parts[1],
		Repos:     repos,
	})
}

func collectInteractiveInitRetentionConfig(opts *root.Options, deps initDeps, cfg config.File) (config.File, error) {
	prompter := deps.retentionPrompter
	if prompter == nil {
		prompter = newHuhInitRetentionPrompter(opts)
	}
	edit, err := prompter.EditRetention(initRetentionPrompt{
		Retention: cfg.Data.Retention,
	})
	if err != nil {
		return config.File{}, err
	}
	if !edit.Apply {
		return cfg, nil
	}
	nextCfg := cfg
	nextCfg.Data.Retention = edit.Retention
	if err := validateInteractiveInitGlobalConfig(nextCfg); err != nil {
		return config.File{}, cmderr.Config(err)
	}
	return nextCfg, nil
}

func collectInteractiveInitKeyringBackendConfig(opts *root.Options, deps initDeps, backendFlagSet bool, cfg config.File) (config.File, error) {
	prompter := deps.keyringPrompter
	if prompter == nil {
		prompter = newHuhInitKeyringBackendPrompter(opts)
	}
	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Backend: cfg.Keyring.Backend,
	})
	if err != nil {
		return config.File{}, err
	}
	if !edit.Apply {
		return cfg, nil
	}
	nextCfg := cfg
	nextCfg.Keyring.Backend = strings.TrimSpace(edit.Backend)
	if err := validateInteractiveInitGlobalConfig(nextCfg); err != nil {
		return config.File{}, cmderr.Config(err)
	}
	if backendFlagSet && nextCfg.Keyring.Backend != "" && nextCfg.Keyring.Backend != opts.Backend {
		return config.File{}, exitcode.Usage(fmt.Errorf("--backend %q conflicts with selected keyring.backend %q", opts.Backend, nextCfg.Keyring.Backend))
	}
	return nextCfg, nil
}

func validateInteractiveInitGlobalConfig(cfg config.File) error {
	if len(cfg.Profiles) == 0 || strings.TrimSpace(cfg.DefaultProfile) == "" {
		if err := config.ValidateKeyring(cfg.Keyring); err != nil {
			return err
		}
		return config.ValidateRetention(cfg.Data.Retention)
	}
	return config.Validate(cfg)
}

func interactiveInitBackendArg(opts *root.Options, backendFlagSet bool, cfg config.File) string {
	if !backendFlagSet {
		return ""
	}
	if strings.TrimSpace(cfg.Keyring.Backend) == strings.TrimSpace(opts.Backend) {
		return ""
	}
	return fmt.Sprintf(" --backend %s", opts.Backend)
}

func collectInteractiveInitSecrets(_ *cobra.Command, opts *root.Options, deps initDeps, plan initWorkspaceDraft) (initWorkspaceDraft, error) {
	if len(plan.credentialPlan) == 0 {
		return plan, nil
	}
	needsSecrets := false
	for _, entry := range plan.credentialPlan {
		switch entry.State {
		case initCredentialPlanStateDefer, initCredentialPlanStateMissingRequired, initCredentialPlanStateOverwriteRef:
			needsSecrets = true
		case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateClearRef:
		}
	}
	if !needsSecrets {
		return plan, nil
	}

	prompter := deps.secretPrompter
	if prompter == nil {
		prompter = newHuhInitSecretPrompter(opts)
	}
	var store initStore
	openStore := func() (initStore, error) {
		if store != nil {
			return store, nil
		}
		opened, err := deps.openStore(opts.Backend, plan.backendFlagSet, plan.cfg)
		if err != nil {
			return nil, err
		}
		store = opened
		return store, nil
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()

	if plan.writes == nil {
		plan.writes = map[string]map[string]string{}
	}
	if plan.overwriteRefs == nil {
		plan.overwriteRefs = map[string]bool{}
	}
	if plan.satisfiedRefs == nil {
		plan.satisfiedRefs = map[string]bool{}
	}

	for _, entry := range plan.credentialPlan {
		switch entry.State {
		case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateClearRef:
			continue
		case initCredentialPlanStateDefer, initCredentialPlanStateMissingRequired, initCredentialPlanStateOverwriteRef:
		}
		action, err := prompter.ChooseCredentialAction(initCredentialSecretPrompt{
			Entry:              entry,
			ClipboardSupported: deps.clipboardSupported(),
		})
		if err != nil {
			return initWorkspaceDraft{}, err
		}
		if action == initCredentialSecretActionDefer {
			continue
		}
		activeStore, err := openStore()
		if err != nil {
			return initWorkspaceDraft{}, cmderr.Credential(err)
		}
		targetKeys, err := existingInitCredentialKeys(activeStore, entry.Ref.Ref)
		if err != nil {
			return initWorkspaceDraft{}, cmderr.Credential(err)
		}
		targetStatus, err := credentials.CredentialRefStatus(activeStore, entry.Ref, nil)
		if err != nil {
			return initWorkspaceDraft{}, cmderr.Credential(err)
		}
		targetHasRequired := credentials.RequiredKeysSatisfied(targetStatus)
		targetHasAnyKeys := len(targetKeys) > 0
		if action == initCredentialSecretActionSetNow && targetHasAnyKeys {
			action, err = prompter.ChooseCredentialAction(initCredentialSecretPrompt{
				Entry:              entry,
				TargetHasRequired:  targetHasRequired,
				TargetHasAnyKeys:   targetHasAnyKeys,
				ClipboardSupported: deps.clipboardSupported(),
			})
			if err != nil {
				return initWorkspaceDraft{}, err
			}
		}
		if action == initCredentialSecretActionKeep {
			if !targetHasRequired {
				return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential ref %q does not have all required keys", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
			}
			plan.satisfiedRefs[entry.Ref.Ref] = true
			continue
		}
		if action == initCredentialSecretActionDefer {
			continue
		}
		if action != initCredentialSecretActionSetNow {
			return initWorkspaceDraft{}, fmt.Errorf("unsupported interactive secret action %q", action)
		}

		overwriteRef := false
		for _, spec := range entry.KeySpecs {
			targetHasKey := targetKeys[spec.Key]
			source, err := prompter.ChooseSecretSource(initSecretValuePrompt{
				Entry:              entry,
				Key:                spec.Key,
				Optional:           !spec.Required,
				TargetHasKey:       targetHasKey,
				ClipboardSupported: deps.clipboardSupported(),
			})
			if err != nil {
				return initWorkspaceDraft{}, err
			}
			switch source {
			case initSecretSourceKeepExisting:
				if !targetHasKey {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential key %q does not exist for ref %q", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key, entry.Ref.Ref))
				}
				continue
			case initSecretSourceSkip:
				if spec.Required {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential key %q is required", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key))
				}
				continue
			case initSecretSourceClipboard:
				value, err := deps.clipboardRead()
				if err != nil {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("read clipboard: %w", err))
				}
				value = credentials.TrimSecretIngress(value)
				if value == "" {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("clipboard supplied an empty secret"))
				}
				addWrite(plan.writes, entry.Ref.Ref, spec.Key, value)
			case initSecretSourcePaste:
				value, err := prompter.PasteSecret(initSecretValuePrompt{
					Entry:              entry,
					Key:                spec.Key,
					Optional:           !spec.Required,
					TargetHasKey:       targetHasKey,
					ClipboardSupported: deps.clipboardSupported(),
				})
				if err != nil {
					return initWorkspaceDraft{}, err
				}
				value = credentials.TrimSecretIngress(value)
				if value == "" {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("pasted secret for %q is empty", spec.Key))
				}
				addWrite(plan.writes, entry.Ref.Ref, spec.Key, value)
			default:
				return initWorkspaceDraft{}, fmt.Errorf("unsupported interactive secret source %q", source)
			}
			if targetHasKey {
				overwriteRef = true
			}
		}
		planned := plan.writes[entry.Ref.Ref]
		if !initCredentialWritePlanSatisfiesEntry(entry, targetKeys, planned) {
			return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential ref %q still needs required keys; keep existing values or defer instead", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
		}
		if overwriteRef {
			plan.overwriteRefs[entry.Ref.Ref] = true
		}
		plan.satisfiedRefs[entry.Ref.Ref] = true
	}
	plan.credentialPlan = refreshInteractiveCredentialPlan(plan.credentialPlan, projectInitPlannedWriteKeys(plan.writes), plan.satisfiedRefs)
	if hasDeferredLLMCredential(plan.credentialPlan) {
		plan.writeLLMHint = true
	}
	return plan, nil
}

func collectInteractiveInitSessionSecrets(opts *root.Options, deps initDeps, plan initSessionPlan) (initSessionPlan, error) {
	if len(plan.credentialPlan) == 0 {
		return plan, nil
	}
	var err error
	plan.credentialPlan, err = loadInteractiveCredentialPlanState(plan.credentialPlan, func() (initStore, error) {
		return deps.openStore(opts.Backend, plan.backendFlagSet, plan.cfg)
	})
	if err != nil {
		return initSessionPlan{}, cmderr.Credential(err)
	}
	workspacePlan := initWorkspaceDraft{
		cfg:             plan.cfg,
		writes:          plan.writes,
		credentialPlan:  plan.credentialPlan,
		overwriteRefs:   plan.overwriteRefs,
		satisfiedRefs:   plan.satisfiedRefs,
		backendFlagSet:  plan.backendFlagSet,
		backendArg:      plan.backendArg,
		allowDeferredLLM: true,
	}
	workspacePlan, err = collectInteractiveInitSecrets(nil, opts, deps, workspacePlan)
	if err != nil {
		return initSessionPlan{}, err
	}
	plan.writes = workspacePlan.writes
	plan.credentialPlan = workspacePlan.credentialPlan
	plan.overwriteRefs = workspacePlan.overwriteRefs
	plan.satisfiedRefs = workspacePlan.satisfiedRefs
	return plan, nil
}

func loadInteractiveCredentialPlanState(entries []initCredentialPlanEntry, openStore func() (initStore, error)) ([]initCredentialPlanEntry, error) {
	var needsStore bool
	for _, entry := range entries {
		if entry.State != initCredentialPlanStateClearRef {
			needsStore = true
			break
		}
	}
	if !needsStore {
		return entries, nil
	}
	store, err := openStore()
	if err != nil {
		return nil, err
	}
	defer store.Close()
	normalized := make([]initCredentialPlanEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.State == initCredentialPlanStateClearRef {
			normalized = append(normalized, entry)
			continue
		}
		status, err := credentials.CredentialRefStatus(store, entry.Ref, nil)
		if err != nil {
			return nil, err
		}
		next := entry
		missingRequired := credentials.MissingRequiredKeys(status)
		if credentials.RequiredKeysSatisfied(status) {
			next.State = initCredentialPlanStateKeepExisting
			next.MissingRequiredKeys = nil
			normalized = append(normalized, next)
			continue
		}
		if next.State == initCredentialPlanStateOverwriteRef {
			next.MissingRequiredKeys = missingRequired
			normalized = append(normalized, next)
			continue
		}
		if credentialStatusHasAnyKeys(status) {
			next.State = initCredentialPlanStateMissingRequired
			next.MissingRequiredKeys = missingRequired
			normalized = append(normalized, next)
			continue
		}
		next.State = initCredentialPlanStateDefer
		next.MissingRequiredKeys = nil
		normalized = append(normalized, next)
	}
	return normalized, nil
}

func credentialStatusHasAnyKeys(status credentials.CredentialStatus) bool {
	for _, key := range status.Keys {
		if key.Present != nil && *key.Present {
			return true
		}
	}
	return false
}

func existingInitCredentialKeys(store initStore, ref string) (map[string]bool, error) {
	parsed, err := credentials.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	keys, err := store.ListBundle(parsed.Profile)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		present[key] = true
	}
	return present, nil
}

func initCredentialWritePlanSatisfiesEntry(entry initCredentialPlanEntry, targetKeys map[string]bool, planned map[string]string) bool {
	for _, spec := range entry.KeySpecs {
		if !spec.Required {
			continue
		}
		if _, ok := planned[spec.Key]; ok {
			continue
		}
		if targetKeys[spec.Key] {
			continue
		}
		return false
	}
	return true
}

func copyModelMap(modelMap config.ModelMap) config.ModelMap {
	if len(modelMap) == 0 {
		return nil
	}
	copied := make(config.ModelMap, len(modelMap))
	for tier, model := range modelMap {
		copied[tier] = model
	}
	return copied
}

func normalizeInitModelMap(modelMap config.ModelMap) config.ModelMap {
	if len(modelMap) == 0 {
		return nil
	}
	normalized := config.ModelMap{}
	for _, tier := range config.ModelTiers() {
		model, ok := modelMap[string(tier)]
		if !ok {
			continue
		}
		normalized[string(tier)] = strings.TrimSpace(model)
	}
	for tier, model := range modelMap {
		if _, seen := normalized[tier]; seen {
			continue
		}
		normalized[tier] = strings.TrimSpace(model)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func refreshInteractiveCredentialPlan(entries []initCredentialPlanEntry, plannedWriteKeys map[string][]string, satisfiedRefs map[string]bool) []initCredentialPlanEntry {
	refreshed := make([]initCredentialPlanEntry, 0, len(entries))
	for _, entry := range entries {
		next := entry
		switch entry.State {
		case initCredentialPlanStateClearRef:
			refreshed = append(refreshed, next)
			continue
		case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateDefer, initCredentialPlanStateOverwriteRef, initCredentialPlanStateMissingRequired:
		}
		next.PlannedWriteKeys = append([]string(nil), plannedWriteKeys[entry.Ref.Ref]...)
		keptWithoutNewWrites := satisfiedRefs[entry.Ref.Ref] && len(next.PlannedWriteKeys) == 0
		if keptWithoutNewWrites {
			next.MissingRequiredKeys = nil
			next.State = initCredentialPlanStateKeepExisting
			refreshed = append(refreshed, next)
			continue
		}
		if len(next.PlannedWriteKeys) == 0 && statePreservesWithoutPlannedWrites(entry.State) {
			refreshed = append(refreshed, next)
			continue
		}
		next.MissingRequiredKeys = missingRequiredInitCredentialKeys(next.KeySpecs, next.PlannedWriteKeys)
		next.State = classifyInitCredentialPlanEntry(next)
		refreshed = append(refreshed, next)
	}
	return refreshed
}

func statePreservesWithoutPlannedWrites(state initCredentialPlanState) bool {
	switch state {
	case initCredentialPlanStateDefer, initCredentialPlanStateOverwriteRef, initCredentialPlanStateMissingRequired:
		return true
	case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateClearRef:
		return false
	default:
		return false
	}
}

func chooseInteractiveInitFinalizeAction(opts *root.Options, deps initDeps, plan initSessionPlan) (initFinalizeAction, error) {
	prompter := deps.finalizePrompter
	if prompter == nil {
		prompter = newHuhInitFinalizePrompter(opts)
	}
	return prompter.ChooseFinalizeAction(buildInteractiveInitFinalizePrompt(plan))
}

func buildInteractiveInitFinalizePrompt(plan initSessionPlan) initFinalizePrompt {
	return initFinalizePrompt{Profiles: buildInteractiveInitProfileReadiness(plan)}
}

func buildInteractiveInitProfileReadiness(plan initSessionPlan) []initProfileReadiness {
	entryByKey := map[string]initCredentialPlanEntry{}
	for _, entry := range plan.credentialPlan {
		entryByKey[initCredentialEntryKey(entry.Ref)] = entry
	}
	readiness := make([]initProfileReadiness, 0, len(plan.profileNames))
	for _, profileName := range plan.profileNames {
		profileReadiness := initProfileReadiness{ProfileName: profileName, Ready: true}
		for _, ref := range plan.profileRefs[profileName] {
			entry, ok := entryByKey[initCredentialEntryKey(ref)]
			if !ok {
				continue
			}
			note := initCredentialReadinessNote(entry)
			if note == "" {
				continue
			}
			profileReadiness.Ready = false
			profileReadiness.Notes = append(profileReadiness.Notes, note)
		}
		readiness = append(readiness, profileReadiness)
	}
	return readiness
}

func initCredentialReadinessNote(entry initCredentialPlanEntry) string {
	label := initCredentialPurposeLabel(entry.Ref.Purpose)
	switch entry.State {
	case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateClearRef:
		return ""
	case initCredentialPlanStateDefer:
		return label + " deferred"
	case initCredentialPlanStateOverwriteRef, initCredentialPlanStateMissingRequired:
		if len(entry.MissingRequiredKeys) == 0 {
			return label + " needs setup"
		}
		return fmt.Sprintf("%s missing %s", label, strings.Join(entry.MissingRequiredKeys, ", "))
	}
	return ""
}

func applyInteractiveInitSessionPlan(opts *root.Options, deps initDeps, plan initSessionPlan) error {
	if err := config.Validate(plan.cfg); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) {
			return cmderr.Config(err)
		}
		return err
	}
	var store initStore
	if len(plan.writes) > 0 {
		var err error
		store, err = deps.openStore(opts.Backend, plan.backendFlagSet, plan.cfg)
		if err != nil {
			return cmderr.Credential(err)
		}
		defer store.Close()
		if err := preflightNoOverwrite(store, plan.writes, plan.overwriteRefs); err != nil {
			return cmderr.Credential(err)
		}
		if _, err := writeBundles(store, plan.writes, false, plan.overwriteRefs); err != nil {
			return cmderr.Credential(err)
		}
	}
	if err := deps.saveConfig(plan.path, plan.cfg); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) {
			return cmderr.Config(err)
		}
		if len(plan.writes) > 0 {
			return fmt.Errorf("init wrote credentials but failed to save config; credential refs needing cleanup: %v: %w", sortedRefs(plan.writes), err)
		}
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Initialized %d profile(s)\n", len(plan.profileNames)); err != nil {
		return err
	}
	for _, readiness := range buildInteractiveInitProfileReadiness(plan) {
		status := "ready"
		if !readiness.Ready {
			status = "needs follow-up"
		}
		if _, err := fmt.Fprintf(opts.Stdout, "- %s: %s\n", readiness.ProfileName, status); err != nil {
			return err
		}
	}
	var writeErr error
	for _, entry := range plan.credentialPlan {
		if !shouldWriteInitCredentialHint(entry, true) {
			continue
		}
		hintErr := writeInitCredentialPlanHints(opts.Stderr, plan.backendArg, entry)
		if writeErr == nil {
			writeErr = hintErr
		}
	}
	return writeErr
}

func hasDeferredLLMCredential(entries []initCredentialPlanEntry) bool {
	for _, entry := range entries {
		if entry.Ref.Purpose != "llm" {
			continue
		}
		return shouldWriteInitCredentialHint(entry, true)
	}
	return false
}

func initCredentialPurposeLabel(purpose string) string {
	switch purpose {
	case "git":
		return "Git"
	case "reviewer_credentials":
		return "reviewer"
	case "llm":
		return "LLM"
	default:
		return purpose
	}
}

func applyInitPlan(opts *root.Options, flags initOptions, deps initDeps, plan initPlan) error {
	var store initStore
	if len(plan.writes) > 0 || (plan.profile.LLM.Auth == config.LLMAuthAPIKey && !plan.allowDeferredLLM) {
		var err error
		store, err = deps.openStore(opts.Backend, plan.backendFlagSet, plan.cfg)
		if err != nil {
			return cmderr.Credential(err)
		}
		defer store.Close()
	}
	if plan.profile.LLM.Auth == config.LLMAuthAPIKey && !plan.llmSecretProvided && !plan.allowDeferredLLM {
		if flags.overwrite {
			return exitcode.Usage(fmt.Errorf("--overwrite with api_key LLM auth requires --llm-api-key-stdin or --llm-api-key-from-env"))
		}
		parsed, err := credentials.ParseRef(plan.profile.LLM.CredentialRef)
		if err != nil {
			return exitcode.Usage(err)
		}
		llmKey, err := credentials.LLMAPIKeyForProvider(plan.profile.LLM.Provider)
		if err != nil {
			return cmderr.Config(err)
		}
		present, err := store.Exists(parsed.Profile, llmKey)
		if err != nil {
			return cmderr.Credential(err)
		}
		if !present {
			return exitcode.Usage(fmt.Errorf("api_key LLM auth requires --llm-api-key-stdin or --llm-api-key-from-env"))
		}
	}
	if store != nil && len(plan.writes) > 0 && !flags.overwrite {
		if err := preflightNoOverwrite(store, plan.writes, plan.overwriteRefs); err != nil {
			return cmderr.Credential(err)
		}
	}
	if store != nil {
		if _, err := writeBundles(store, plan.writes, flags.overwrite, plan.overwriteRefs); err != nil {
			return cmderr.Credential(err)
		}
	}
	if err := deps.saveConfig(plan.path, plan.cfg); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) {
			return cmderr.Config(err)
		}
		if len(plan.writes) > 0 {
			return fmt.Errorf("init wrote credentials but failed to save config for profile %q; credential refs needing cleanup: %v: %w", plan.profileName, sortedRefs(plan.writes), err)
		}
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Initialized profile %s\n", plan.profileName); err != nil {
		return err
	}
	var err error
	for _, entry := range plan.credentialPlan {
		if !shouldWriteInitCredentialHint(entry, plan.writeLLMHint) {
			continue
		}
		hintErr := writeInitCredentialPlanHints(opts.Stderr, plan.backendArg, entry)
		if err == nil {
			err = hintErr
		}
	}
	return err
}

func writeInitCredentialPlanHints(w io.Writer, backendArg string, entry initCredentialPlanEntry) error {
	keys := entry.MissingRequiredKeys
	if len(keys) == 0 {
		keys = make([]string, 0, len(entry.KeySpecs))
		for _, spec := range entry.KeySpecs {
			keys = append(keys, spec.Key)
		}
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "Next: cr%s set-credential --ref %s --key %s --stdin\n", backendArg, entry.Ref.Ref, key); err != nil {
			return err
		}
	}
	return nil
}

func projectInitPlannedWriteKeys(writes map[string]map[string]string) map[string][]string {
	projected := make(map[string][]string, len(writes))
	for ref, bundle := range writes {
		keys := sortedKeys(bundle)
		projected[ref] = append([]string(nil), keys...)
	}
	return projected
}

func planInitCredentials(previousProfile *config.Profile, desiredProfile config.Profile, plannedWriteKeys map[string][]string) ([]initCredentialPlanEntry, error) {
	desiredRefs, err := config.CredentialRefs(desiredProfile)
	if err != nil {
		return nil, err
	}
	var previousRefs []config.CredentialRef
	if previousProfile != nil {
		previousRefs, err = config.CredentialRefs(*previousProfile)
		if err != nil {
			return nil, err
		}
	}

	previousByPurpose := make(map[string]config.CredentialRef, len(previousRefs))
	for _, ref := range previousRefs {
		previousByPurpose[ref.Purpose] = ref
	}

	entries := make([]initCredentialPlanEntry, 0, len(desiredRefs)+len(previousRefs))
	for _, ref := range desiredRefs {
		specs, err := credentials.KeySpecsForPurpose(ref)
		if err != nil {
			return nil, err
		}
		writeKeys := append([]string(nil), plannedWriteKeys[ref.Ref]...)
		if err := validateInitPlannedWriteKeys(ref, specs, writeKeys); err != nil {
			return nil, err
		}
		entry := initCredentialPlanEntry{
			Ref:              ref,
			KeySpecs:         append([]credentials.KeySpec(nil), specs...),
			PlannedWriteKeys: writeKeys,
		}
		if previousRef, ok := previousByPurpose[ref.Purpose]; ok {
			previousCopy := previousRef
			entry.PreviousRef = &previousCopy
			delete(previousByPurpose, ref.Purpose)
		}
		entry.MissingRequiredKeys = missingRequiredInitCredentialKeys(entry.KeySpecs, entry.PlannedWriteKeys)
		entry.State = classifyInitCredentialPlanEntry(entry)
		entries = append(entries, entry)
	}
	for _, ref := range previousRefs {
		if _, ok := previousByPurpose[ref.Purpose]; !ok {
			continue
		}
		entries = append(entries, initCredentialPlanEntry{
			Ref:   ref,
			State: initCredentialPlanStateClearRef,
		})
	}
	return entries, nil
}

func validateInitPlannedWriteKeys(ref config.CredentialRef, specs []credentials.KeySpec, plannedWriteKeys []string) error {
	if len(plannedWriteKeys) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		allowed[spec.Key] = struct{}{}
	}
	for _, key := range plannedWriteKeys {
		if _, ok := allowed[key]; ok {
			continue
		}
		return fmt.Errorf("init credential planner: unexpected planned write key %q for %s ref %q", key, ref.Purpose, ref.Ref)
	}
	return nil
}

func missingRequiredInitCredentialKeys(specs []credentials.KeySpec, plannedWriteKeys []string) []string {
	if len(plannedWriteKeys) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(plannedWriteKeys))
	for _, key := range plannedWriteKeys {
		present[key] = struct{}{}
	}
	var missing []string
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		if _, ok := present[spec.Key]; ok {
			continue
		}
		missing = append(missing, spec.Key)
	}
	return missing
}

func classifyInitCredentialPlanEntry(entry initCredentialPlanEntry) initCredentialPlanState {
	if len(entry.MissingRequiredKeys) > 0 {
		return initCredentialPlanStateMissingRequired
	}
	if len(entry.PlannedWriteKeys) > 0 {
		return initCredentialPlanStateWrite
	}
	if entry.PreviousRef == nil {
		return initCredentialPlanStateDefer
	}
	if entry.PreviousRef.Purpose == entry.Ref.Purpose &&
		entry.PreviousRef.Ref == entry.Ref.Ref &&
		entry.PreviousRef.Mode == entry.Ref.Mode &&
		entry.PreviousRef.Provider == entry.Ref.Provider {
		return initCredentialPlanStateKeepExisting
	}
	return initCredentialPlanStateOverwriteRef
}

func shouldWriteInitCredentialHint(entry initCredentialPlanEntry, includeLLM bool) bool {
	if entry.Ref.Purpose == "llm" && !includeLLM {
		return false
	}
	switch entry.State {
	case initCredentialPlanStateDefer, initCredentialPlanStateOverwriteRef, initCredentialPlanStateMissingRequired:
		return true
	case initCredentialPlanStateKeepExisting, initCredentialPlanStateWrite, initCredentialPlanStateClearRef:
		return false
	}
	return false
}

func readInitSecret(deps initDeps, stdin io.Reader, useStdin bool, envVar string, stdinFlag string, envFlag string) (string, bool, error) {
	if !useStdin && envVar == "" {
		return "", false, nil
	}
	return deps.readSecret(stdin, useStdin, envVar, stdinFlag, envFlag)
}

func readSecretIngress(r io.Reader, stdin bool, envVar, stdinFlag, envFlag string) (string, error) {
	value, ok, err := readOptionalSecretIngress(r, stdin, envVar, stdinFlag, envFlag)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("exactly one of %s or %s is required", stdinFlag, envFlag)
	}
	return value, nil
}

func readOptionalSecretIngress(r io.Reader, stdin bool, envVar, stdinFlag, envFlag string) (string, bool, error) {
	if stdin && envVar != "" {
		return "", false, fmt.Errorf("only one of %s or %s may be set", stdinFlag, envFlag)
	}
	if !stdin && envVar == "" {
		return "", false, nil
	}
	var value string
	if stdin {
		bytes, err := io.ReadAll(r)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", stdinFlag, err)
		}
		value = credentials.TrimSecretIngress(string(bytes))
	} else {
		value = os.Getenv(envVar)
	}
	if value == "" {
		return "", false, fmt.Errorf("%s supplied an empty secret", ingressName(stdin, envVar, stdinFlag, envFlag))
	}
	return value, true, nil
}

func ingressName(stdin bool, envVar, stdinFlag, envFlag string) string {
	if stdin {
		return stdinFlag
	}
	return fmt.Sprintf("%s %s", envFlag, envVar)
}

func loadOptionalConfig(opts *root.Options) (config.File, error) {
	path, err := configPath(opts)
	if err != nil {
		return config.File{}, err
	}
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotConfigured) {
		return config.File{}, nil
	}
	return cfg, err
}

func loadConfigForInit(path string) (config.File, bool, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotConfigured) {
		return config.File{Profiles: map[string]config.Profile{}}, false, nil
	}
	return cfg, true, err
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func addWrite(writes map[string]map[string]string, ref, key, value string) {
	if writes[ref] == nil {
		writes[ref] = map[string]string{}
	}
	writes[ref][key] = value
}

func preflightNoOverwrite(store initStore, writes map[string]map[string]string, overwriteRefs map[string]bool) error {
	for _, ref := range sortedRefs(writes) {
		if overwriteRefs[ref] {
			continue
		}
		parsed, err := credentials.ParseRef(ref)
		if err != nil {
			return err
		}
		for _, key := range sortedKeys(writes[ref]) {
			present, err := store.Exists(parsed.Profile, key)
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("%w: %s/%s", credstore.ErrExists, ref, key)
			}
		}
	}
	return nil
}

func writeBundles(store initStore, writes map[string]map[string]string, overwriteAll bool, overwriteRefs map[string]bool) ([]string, error) {
	var writtenRefs []string
	for _, ref := range sortedRefs(writes) {
		parsed, err := credentials.ParseRef(ref)
		if err != nil {
			return writtenRefs, err
		}
		setOpts := []credstore.SetOpt{}
		if overwriteAll || overwriteRefs[ref] {
			setOpts = append(setOpts, credstore.WithOverwrite())
		}
		result, err := store.SetBundle(parsed.Profile, writes[ref], setOpts...)
		if err != nil {
			cleanupRefs := append([]string(nil), writtenRefs...)
			if len(result.RollbackFailed) > 0 {
				cleanupRefs = append(cleanupRefs, ref)
			}
			if len(cleanupRefs) > 0 {
				return writtenRefs, fmt.Errorf("init wrote credentials before failing on %s; credential refs needing cleanup: %v: %w", ref, cleanupRefs, err)
			}
			return writtenRefs, err
		}
		writtenRefs = append(writtenRefs, ref)
	}
	return writtenRefs, nil
}

func sortedRefs(writes map[string]map[string]string) []string {
	refs := make([]string, 0, len(writes))
	for ref := range writes {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
