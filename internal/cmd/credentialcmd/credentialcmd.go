// Package credentialcmd wires credential ingress commands.
package credentialcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
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
	RequestedProfileName         string
	ExistingProfileName          string
	ExistingProfile              *config.Profile
	ExistingProfileNames         []string
	DefaultProfileName           string
	ExistingConfig               config.File
	GitScopes                    map[string]initGitScopeDraft
	ProfileGitScopes             map[string]string
	ReviewerEntities             map[string]initReviewerEntityDraft
	ProfileReviewerEntities      map[string]string
	LLMRuntimes                  map[string]initLLMRuntimeDraft
	ProfileLLMRuntimes           map[string]string
	ProfileWarnings              map[string][]string
	PendingProfileDeletes        map[string]initPendingProfileDelete
	PendingReviewerEntityDeletes map[string]initPendingReviewerEntityDelete
	PendingLLMRuntimeDeletes     map[string]initPendingLLMRuntimeDelete
}

type initDraftAction string

const (
	initDraftActionNone                     initDraftAction = ""
	initDraftActionDeleteProfile            initDraftAction = "delete_profile"
	initDraftActionUndoDeleteProfile        initDraftAction = "undo_delete_profile"
	initDraftActionDeleteReviewerEntity     initDraftAction = "delete_reviewer_entity"
	initDraftActionUndoDeleteReviewerEntity initDraftAction = "undo_delete_reviewer_entity"
	initDraftActionDeleteLLMRuntime         initDraftAction = "delete_llm_runtime"
	initDraftActionUndoDeleteLLMRuntime     initDraftAction = "undo_delete_llm_runtime"
)

type initDraft struct {
	Action                initDraftAction
	ActionTarget          string
	OriginalProfileName   string
	ProfileName           string
	MakeDefault           bool
	GitHost               string
	GitAuth               string
	GitCredentialRef      string
	ReviewerEnabled       bool
	ReviewerAuth          string
	ReviewerCredentialRef string
	ReviewerDisplayName   string
	LLMProvider           string
	LLMAuth               string
	LLMAdapter            string
	LLMReviewerModelTier  string
	LLMCredentialRef      string
	AdvancedStorageLabels bool
	RepositoryRoutesAction string
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
	Action       initRoutesAction
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
	initFinalizeActionBack   initFinalizeAction = "back"
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

var (
	errInitNavigateBack    = errors.New("init: navigate back")
	errInitSecretValueBack = errors.New("init: secret value back")
)

type initMenuPrompt struct {
	HasWorkspace         bool
	LLMRuntimeCount      int
	ReviewerEntityCount  int
	ReviewProfileCount   int
	CanConfigureLLM      bool
	CanConfigureReviewer bool
	CanSave              bool
	ActiveProfileName    string
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
	path                         string
	originalCfg                  config.File
	cfg                          config.File
	requestedProfileName         string
	backendFlagSet               bool
	workspace                    *initWorkspaceDraft
	touchedProfiles              map[string]string
	writes                       map[string]map[string]string
	overwriteRefs                map[string]bool
	satisfiedRefs                map[string]bool
	pendingProfileDeletes        map[string]initPendingProfileDelete
	pendingReviewerEntityDeletes map[string]initPendingReviewerEntityDelete
	pendingLLMRuntimeDeletes     map[string]initPendingLLMRuntimeDelete
}

type initPendingProfileDelete struct {
	ProfileName       string
	Profile           config.Profile
	OriginalDefault   string
	FallbackDefault   string
	PrunedRoutes      []config.RepositoryProfile
	WasActive         bool
	HadTouchedProfile bool
	TouchedOriginal   string
}

type initPendingReviewerEntityDelete struct {
	EntityName string
	Affected   map[string]config.ReviewerCredentials
}

type initPendingLLMRuntimeDelete struct {
	RuntimeName string
	Replacement initLLMRuntimeDraft
	Applied     map[string]config.LLMConfig
	Original    map[string]config.LLMConfig
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
	DisplayName   string
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
	initCredentialSecretActionBack   initCredentialSecretAction = "back"
)

type initModelMapAction string

const (
	initModelMapActionPreserve initModelMapAction = "preserve"
	initModelMapActionEdit     initModelMapAction = "edit"
	initModelMapActionReset    initModelMapAction = "reset"
	initModelMapActionBack     initModelMapAction = "back"
)

type initAgentSourcesAction string

const (
	initAgentSourcesActionPreserve initAgentSourcesAction = "preserve"
	initAgentSourcesActionEdit     initAgentSourcesAction = "edit"
	initAgentSourcesActionReset    initAgentSourcesAction = "reset"
	initAgentSourcesActionBack     initAgentSourcesAction = "back"
)

type initReviewPolicyAction string

const (
	initReviewPolicyActionPreserve initReviewPolicyAction = "preserve"
	initReviewPolicyActionEdit     initReviewPolicyAction = "edit"
	initReviewPolicyActionBack     initReviewPolicyAction = "back"
)

type initRoutesAction string

const (
	initRoutesActionPreserve initRoutesAction = "preserve"
	initRoutesActionEdit     initRoutesAction = "edit"
	initRoutesActionReset    initRoutesAction = "reset"
	initRoutesActionBack     initRoutesAction = "back"
)

type initRetentionAction string

const (
	initRetentionActionPreserve initRetentionAction = "preserve"
	initRetentionActionEdit     initRetentionAction = "edit"
	initRetentionActionReset    initRetentionAction = "reset"
	initRetentionActionBack     initRetentionAction = "back"
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
	initKeyringBackendActionBack     initKeyringBackendAction = "back"
)

type initSecretSource string

const (
	initSecretSourceKeepExisting initSecretSource = "keep_existing"
	initSecretSourcePaste        initSecretSource = "paste"
	initSecretSourceClipboard    initSecretSource = "clipboard"
	initSecretSourceSkip         initSecretSource = "skip"
	initSecretSourceBack         initSecretSource = "back"
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
		if err != nil {
			return err
		}
		if session.workspace == nil {
			return nil
		}
		workspace, err := collectInteractiveInitSecrets(cmd, opts, deps, *session.workspace)
		if err != nil {
			return err
		}
		plan := finalizeInteractiveInitPlan(workspace)
		return applyInitPlan(opts, flags, deps, plan)
	}

	for {
		session, err = runInteractiveInitMenuLoop(cmd, opts, flags, deps, session)
		if err != nil {
			return err
		}
		if session.workspace == nil {
			return nil
		}
		plan, err := buildInteractiveInitSessionPlan(opts, session)
		if err != nil {
			return err
		}
		plan, err = collectInteractiveInitSessionSecrets(opts, deps, plan)
		if errors.Is(err, errInitNavigateBack) {
			continue
		}
		if err != nil {
			return err
		}
		action, err := chooseInteractiveInitFinalizeAction(opts, deps, plan)
		if errors.Is(err, errInitNavigateBack) {
			continue
		}
		if err != nil {
			return err
		}
		switch action {
		case initFinalizeActionBack:
			continue
		case initFinalizeActionCancel:
			return nil
		case initFinalizeActionSave:
			return applyInteractiveInitSessionPlan(opts, deps, plan)
		default:
			return fmt.Errorf("unsupported init finalize action %q", action)
		}
	}
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
	stdin   io.Reader
	stderr  io.Writer
	checker func(initLLMRuntimePreset) string
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
	return huhInitLLMRuntimePrompter{
		stdin:   opts.Stdin,
		stderr:  opts.Stderr,
		checker: defaultInitLLMRuntimeAvailabilityNote,
	}
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
	initCustomGitScopeSelection           = "__custom_git_scope__"
	initCustomLLMRuntimeSelection         = "__custom_llm_runtime__"
	initBackSelection                     = "__back__"
	initUndoProfileDeletePrefix           = "__undo_profile_delete__:"
	initUndoReviewerEntityDeletePrefix    = "__undo_reviewer_entity_delete__:"
	initUndoLLMRuntimeDeletePrefix        = "__undo_llm_runtime_delete__:"
	initLLMRuntimeTemplateActionUse       = "use"
	initLLMRuntimeTemplateActionCustomize = "customize"
	initStorageLabelsStandard             = "standard"
	initStorageLabelsCustom               = "custom"
	initSelfApproveDisallow               = "disallow"
	initSelfApproveEnable                 = "enable"
	initDetailActionEdit                  = "edit"
	initDetailActionBack                  = "back"
	initDetailActionDelete                = "delete"
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

func buildInteractiveInitInventoryPromptContext(ctx initPromptContext) initPromptContext {
	ctx.GitScopes, ctx.ProfileGitScopes = buildInitGitScopeInventory(ctx.ExistingConfig)
	ctx.ReviewerEntities, ctx.ProfileReviewerEntities = buildInitReviewerEntityInventory(ctx.ExistingConfig)
	ctx.LLMRuntimes, ctx.ProfileLLMRuntimes = buildInitLLMRuntimeInventory(ctx.ExistingConfig)
	return ctx
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
		path:                         path,
		originalCfg:                  cloneInitConfigFile(cfg),
		cfg:                          cloneInitConfigFile(cfg),
		requestedProfileName:         profileName,
		backendFlagSet:               cmderr.BackendFlagChanged(cmd),
		touchedProfiles:              map[string]string{},
		writes:                       map[string]map[string]string{},
		overwriteRefs:                map[string]bool{},
		satisfiedRefs:                map[string]bool{},
		pendingProfileDeletes:        map[string]initPendingProfileDelete{},
		pendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{},
		pendingLLMRuntimeDeletes:     map[string]initPendingLLMRuntimeDelete{},
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
		HasWorkspace:        session.workspace != nil,
		LLMRuntimeCount:     len(llmRuntimes),
		ReviewerEntityCount: countConfiguredInitReviewerEntities(reviewerEntities),
		ReviewProfileCount:  len(session.cfg.Profiles),
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

func currentInteractiveInitInventoryPromptContext(session initSessionDraft) initPromptContext {
	return buildInteractiveInitInventoryPromptContext(currentInteractiveInitPromptContextBase(session))
}

func currentInteractiveInitPromptContextBase(session initSessionDraft) initPromptContext {
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
	return initPromptContext{
		RequestedProfileName:         session.requestedProfileName,
		ExistingProfileName:          existingProfileName,
		ExistingProfile:              existingProfile,
		ExistingProfileNames:         sortedProfileNames(session.cfg.Profiles),
		DefaultProfileName:           session.cfg.DefaultProfile,
		ExistingConfig:               session.cfg,
		PendingProfileDeletes:        session.pendingProfileDeletes,
		PendingReviewerEntityDeletes: session.pendingReviewerEntityDeletes,
		PendingLLMRuntimeDeletes:     session.pendingLLMRuntimeDeletes,
	}
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
			session, err = loopInteractiveInitLLMRuntime(cmd, opts, flags, deps, session)
		case initMenuActionReviewerEntities:
			session, err = loopInteractiveInitReviewerEntity(cmd, opts, flags, deps, session)
		case initMenuActionReviewProfiles:
			session, err = loopInteractiveInitProfile(cmd, opts, flags, deps, session)
		case initMenuActionGlobalSettings:
			session, err = editInteractiveInitGlobalSettings(cmd, opts, deps, session)
		case initMenuActionSave:
			if session.workspace == nil {
				return initSessionDraft{}, exitcode.Usage(errors.New("commit requires at least one configured profile"))
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
	session, _, err := editInteractiveInitProfileStep(cmd, opts, flags, deps, session)
	return session, err
}

func loopInteractiveInitProfile(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	for {
		var stayInCategory bool
		var err error
		session, stayInCategory, err = editInteractiveInitProfileStep(cmd, opts, flags, deps, session)
		if err != nil {
			return initSessionDraft{}, err
		}
		if !stayInCategory {
			return session, nil
		}
	}
}

func editInteractiveInitProfileStep(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, bool, error) {
	prompter := deps.prompter
	if prompter == nil {
		prompter = newHuhInitPrompter(opts)
	}
	promptCtx := currentInteractiveInitInventoryPromptContext(session)
	draft, err := prompter.Run(promptCtx)
	if errors.Is(err, errInitNavigateBack) {
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	switch draft.Action {
	case initDraftActionNone:
	case initDraftActionDeleteProfile:
		session, err := deleteInteractiveInitProfile(session, draft.ActionTarget)
		return session, err == nil, err
	case initDraftActionUndoDeleteProfile:
		session, err := undoInteractiveInitProfileDelete(session, draft.ActionTarget)
		return session, err == nil, err
	case initDraftActionDeleteReviewerEntity, initDraftActionUndoDeleteReviewerEntity, initDraftActionDeleteLLMRuntime, initDraftActionUndoDeleteLLMRuntime:
		return initSessionDraft{}, false, fmt.Errorf("unsupported review-profile draft action %q", draft.Action)
	}
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, false, err
	}
	nextWorkspace, err := collectInteractiveInitRoutes(opts, deps, workspace, initRoutesAction(strings.TrimSpace(draft.RepositoryRoutesAction)))
	if errors.Is(err, errInitNavigateBack) {
		session.workspace = &workspace
		session.cfg = cloneInitConfigFile(workspace.cfg)
		session.requestedProfileName = workspace.profileName
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		return session, strings.TrimSpace(draft.RepositoryRoutesAction) != "", nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	workspace = nextWorkspace
	nextWorkspace, err = collectInteractiveInitModelMap(opts, deps, workspace)
	if errors.Is(err, errInitNavigateBack) {
		session.workspace = &workspace
		session.cfg = cloneInitConfigFile(workspace.cfg)
		session.requestedProfileName = workspace.profileName
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	workspace = nextWorkspace
	nextWorkspace, err = collectInteractiveInitAgentSources(opts, deps, workspace)
	if errors.Is(err, errInitNavigateBack) {
		session.workspace = &workspace
		session.cfg = cloneInitConfigFile(workspace.cfg)
		session.requestedProfileName = workspace.profileName
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	workspace = nextWorkspace
	nextWorkspace, err = collectInteractiveInitReviewPolicy(opts, deps, workspace)
	if errors.Is(err, errInitNavigateBack) {
		session.workspace = &workspace
		session.cfg = cloneInitConfigFile(workspace.cfg)
		session.requestedProfileName = workspace.profileName
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	workspace = nextWorkspace
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.requestedProfileName = workspace.profileName
	session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
	if deps.menuPrompter != nil || deps.prompter == nil {
		session, err = collectInteractiveInitSessionWorkspaceSecrets(opts, deps, session, []string{"git", "reviewer_credentials", "llm"})
		if errors.Is(err, errInitNavigateBack) {
			return session, false, nil
		}
		if err != nil {
			return initSessionDraft{}, false, err
		}
	}
	return session, true, nil
}

func loopInteractiveInitLLMRuntime(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	for {
		var stayInCategory bool
		var err error
		session, stayInCategory, err = editInteractiveInitLLMRuntimeStep(cmd, opts, flags, deps, session)
		if err != nil {
			return initSessionDraft{}, err
		}
		if !stayInCategory {
			return session, nil
		}
	}
}

func editInteractiveInitLLMRuntimeStep(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, bool, error) {
	if session.workspace == nil {
		return initSessionDraft{}, false, exitcode.Usage(errors.New("configure a review profile before editing LLM runtimes"))
	}
	prompter := deps.llmRuntimePrompter
	if prompter == nil {
		prompter = newHuhInitLLMRuntimePrompter(opts)
	}
	promptCtx := currentInteractiveInitInventoryPromptContext(session)
	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: promptCtx})
	if errors.Is(err, errInitNavigateBack) {
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	switch draft.Action {
	case initDraftActionNone:
	case initDraftActionDeleteLLMRuntime:
		session, err := deleteInteractiveInitLLMRuntime(session, draft.ActionTarget, initLLMRuntimeDraftFromSeedDraft(draft))
		return session, err == nil, err
	case initDraftActionUndoDeleteLLMRuntime:
		session, err := undoInteractiveInitLLMRuntimeDelete(session, draft.ActionTarget)
		return session, err == nil, err
	case initDraftActionDeleteProfile, initDraftActionUndoDeleteProfile, initDraftActionDeleteReviewerEntity, initDraftActionUndoDeleteReviewerEntity:
		return initSessionDraft{}, false, fmt.Errorf("unsupported LLM-runtime draft action %q", draft.Action)
	}
	previousProfileName := session.workspace.profileName
	previousProfile := session.workspace.profile
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, false, err
	}
	session.workspace = &workspace
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.requestedProfileName = workspace.profileName
	if previousProfileName != workspace.profileName || !reflect.DeepEqual(previousProfile, workspace.profile) {
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		if deps.menuPrompter != nil || deps.prompter == nil {
			session, err = collectInteractiveInitSessionWorkspaceSecrets(opts, deps, session, []string{"llm"})
			if errors.Is(err, errInitNavigateBack) {
				return session, false, nil
			}
			if err != nil {
				return initSessionDraft{}, false, err
			}
		}
	}
	return session, true, nil
}

func loopInteractiveInitReviewerEntity(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	for {
		var stayInCategory bool
		var err error
		session, stayInCategory, err = editInteractiveInitReviewerEntityStep(cmd, opts, flags, deps, session)
		if err != nil {
			return initSessionDraft{}, err
		}
		if !stayInCategory {
			return session, nil
		}
	}
}

func editInteractiveInitReviewerEntityStep(cmd *cobra.Command, opts *root.Options, flags initOptions, deps initDeps, session initSessionDraft) (initSessionDraft, bool, error) {
	if session.workspace == nil {
		return initSessionDraft{}, false, exitcode.Usage(errors.New("configure a review profile before editing reviewer entities"))
	}
	prompter := deps.reviewerPrompter
	if prompter == nil {
		prompter = newHuhInitReviewerEntityPrompter(opts)
	}
	promptCtx := currentInteractiveInitInventoryPromptContext(session)
	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: promptCtx})
	if errors.Is(err, errInitNavigateBack) {
		return session, false, nil
	}
	if err != nil {
		return initSessionDraft{}, false, err
	}
	switch draft.Action {
	case initDraftActionNone:
	case initDraftActionDeleteReviewerEntity:
		session, err := deleteInteractiveInitReviewerEntity(session, draft.ActionTarget)
		return session, err == nil, err
	case initDraftActionUndoDeleteReviewerEntity:
		session, err := undoInteractiveInitReviewerEntityDelete(session, draft.ActionTarget)
		return session, err == nil, err
	case initDraftActionDeleteProfile, initDraftActionUndoDeleteProfile, initDraftActionDeleteLLMRuntime, initDraftActionUndoDeleteLLMRuntime:
		return initSessionDraft{}, false, fmt.Errorf("unsupported reviewer-entity draft action %q", draft.Action)
	}
	previousProfileName := session.workspace.profileName
	previousProfile := session.workspace.profile
	previousCfg := cloneInitConfigFile(session.cfg)
	workspace, err := buildInteractiveInitWorkspace(cmd, opts, flags, deps, session.path, session.cfg, draft)
	if err != nil {
		return initSessionDraft{}, false, err
	}
	session.cfg = cloneInitConfigFile(workspace.cfg)
	session.cfg = propagateSharedReviewerEntityDisplayName(previousCfg, session.cfg, workspace.profileName, initReviewerEntityDraftFromSeedDraft(draft))
	session = rebuildInteractiveInitWorkspace(session, workspace.profileName)
	session.requestedProfileName = workspace.profileName
	if previousProfileName != workspace.profileName || !reflect.DeepEqual(previousProfile, session.workspace.profile) {
		session = recordTouchedProfile(session, workspace.profileName, draft.OriginalProfileName)
		if deps.menuPrompter != nil || deps.prompter == nil {
			session, err = collectInteractiveInitSessionWorkspaceSecrets(opts, deps, session, []string{"reviewer_credentials"})
			if errors.Is(err, errInitNavigateBack) {
				return session, false, nil
			}
			if err != nil {
				return initSessionDraft{}, false, err
			}
		}
	}
	return session, true, nil
}

func propagateSharedReviewerEntityDisplayName(priorCfg config.File, updatedCfg config.File, activeProfileName string, entity initReviewerEntityDraft) config.File {
	if entity.Kind == initReviewerEntityKindUseGitIdentity {
		return updatedCfg
	}
	identityKey := entity.identityKey()
	displayName := normalizeOptionalDisplayName(entity.DisplayName)
	for profileName, previousProfile := range priorCfg.Profiles {
		if profileName == activeProfileName {
			continue
		}
		if initReviewerEntityDraftFromConfig(previousProfile).identityKey() != identityKey {
			continue
		}
		profile, ok := updatedCfg.Profiles[profileName]
		if !ok || profile.ReviewerCredentials == nil {
			continue
		}
		if initReviewerEntityDraftFromConfig(profile).identityKey() != identityKey {
			continue
		}
		profile.ReviewerCredentials.DisplayName = displayName
		updatedCfg.Profiles[profileName] = profile
	}
	return updatedCfg
}

func editInteractiveInitGlobalSettings(_ *cobra.Command, opts *root.Options, deps initDeps, session initSessionDraft) (initSessionDraft, error) {
	cfg, err := collectInteractiveInitRetentionConfig(opts, deps, cloneInitConfigFile(session.cfg))
	if errors.Is(err, errInitNavigateBack) {
		return session, nil
	}
	if err != nil {
		return initSessionDraft{}, err
	}
	nextCfg, err := collectInteractiveInitKeyringBackendConfig(opts, deps, session.backendFlagSet, cfg)
	if errors.Is(err, errInitNavigateBack) {
		session.cfg = cfg
		if session.workspace != nil {
			workspace := *session.workspace
			workspace.cfg = cloneInitConfigFile(cfg)
			workspace.backendArg = interactiveInitBackendArg(opts, workspace.backendFlagSet, cfg)
			session.workspace = &workspace
		}
		return session, nil
	}
	if err != nil {
		return initSessionDraft{}, err
	}
	cfg = nextCfg
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
		huh.NewOption("Commit staged changes and exit", initMenuActionSave),
		huh.NewOption("Discard staged changes and exit", initMenuActionExit),
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
							return errors.New("configure a review profile before committing changes")
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
	return "Configure the parts cr needs, stage changes as you go, then commit when the active profile is ready."
}

func runBackableInitForm(form *huh.Form, stdin io.Reader, stderr io.Writer) (bool, error) {
	escaped := false
	filter := func(_ tea.Model, msg tea.Msg) tea.Msg {
		keyMsg, ok := msg.(tea.KeyMsg)
		if ok && keyMsg.Type == tea.KeyEsc {
			escaped = true
			return tea.Quit()
		}
		return msg
	}
	err := form.
		WithProgramOptions(tea.WithFilter(filter)).
		WithInput(stdin).
		WithOutput(stderr).
		Run()
	if err != nil {
		return false, err
	}
	return escaped, nil
}

func (p huhInitFinalizePrompter) ChooseFinalizeAction(prompt initFinalizePrompt) (initFinalizeAction, error) {
	action := initFinalizeActionSave
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Description(initFinalizeDescription(prompt)),
			huh.NewSelect[initFinalizeAction]().
				Title("Finalize init").
				Options(
					huh.NewOption("Commit staged changes and exit", initFinalizeActionSave),
					huh.NewOption("Back to main menu", initFinalizeActionBack),
					huh.NewOption("Discard staged changes and exit", initFinalizeActionCancel),
				).
				Value(&action),
		),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return "", err
	}
	if back {
		return initFinalizeActionBack, nil
	}
	return action, nil
}

func initFinalizeDescription(prompt initFinalizePrompt) string {
	lines := []string{"Review readiness before committing staged config and credential changes."}
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
	for {
		choice := selectedRuntime
		options := initLLMRuntimeOptions(prompt.Context.LLMRuntimes)
		pendingDeleteNames := make([]string, 0, len(prompt.Context.PendingLLMRuntimeDeletes))
		for name := range prompt.Context.PendingLLMRuntimeDeletes {
			pendingDeleteNames = append(pendingDeleteNames, name)
		}
		sort.Strings(pendingDeleteNames)
		for _, name := range pendingDeleteNames {
			options = append(options, huh.NewOption(llmRuntimeDeletePendingLabel(name), initUndoLLMRuntimeDeletePrefix+name))
		}
		options = append(options, huh.NewOption("Back to main menu", initBackSelection))
		selectForm := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("LLM runtime").
					Description("Choose how reviewer agents run for this profile.").
					Options(options...).
					Value(&choice),
			).Title("LLM Runtime"),
		)
		back, err := runBackableInitForm(selectForm, p.stdin, p.stderr)
		if err != nil {
			return initDraft{}, err
		}
		if back || choice == initBackSelection {
			return initDraft{}, errInitNavigateBack
		}
		if strings.HasPrefix(choice, initUndoLLMRuntimeDeletePrefix) {
			return initDraft{
				Action:       initDraftActionUndoDeleteLLMRuntime,
				ActionTarget: strings.TrimPrefix(choice, initUndoLLMRuntimeDeletePrefix),
			}, nil
		}

		candidateDraft := draft
		if choice != initCustomLLMRuntimeSelection {
			applyLLMRuntimeInventorySelection(&candidateDraft, choice, prompt.Context.LLMRuntimes)
			resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(candidateDraft).Preset)
			applyLLMRuntimeSelection(&candidateDraft, resolvedRuntimePreset)
		}
		detailDraft := candidateDraft
		usedSelection, back, deleted, err := p.editLLMRuntimeDetails(choice, &detailDraft, prompt.Context.LLMRuntimes)
		if err != nil {
			return initDraft{}, err
		}
		if usedSelection {
			return detailDraft, nil
		}
		if back {
			selectedRuntime = choice
			continue
		}
		if deleted {
			replacementDraft, err := p.chooseLLMRuntimeDeleteReplacement(prompt, choice, draft)
			if err != nil {
				if errors.Is(err, errInitNavigateBack) {
					selectedRuntime = choice
					continue
				}
				return initDraft{}, err
			}
			replacementDraft.Action = initDraftActionDeleteLLMRuntime
			replacementDraft.ActionTarget = choice
			return replacementDraft, nil
		}
		resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(detailDraft).Preset)
		applyLLMRuntimeSelection(&detailDraft, resolvedRuntimePreset)
		return detailDraft, nil
	}
}

func (p huhInitLLMRuntimePrompter) editLLMRuntimeDetails(selection string, draft *initDraft, runtimes map[string]initLLMRuntimeDraft) (bool, bool, bool, error) {
	runtime := initLLMRuntimeDraftFromSeedDraft(*draft)
	detailTitle := "LLM Runtime Details"
	detailDescription := initLLMRuntimeSelectionDescription(runtime, p.runtimeAvailabilityNote(runtime.Preset))
	if _, ok := runtimes[selection]; !ok && selection != initCustomLLMRuntimeSelection {
		action := initLLMRuntimeTemplateActionUse
		actionForm := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("LLM runtime details").
					Description(detailDescription).
					Options(
						huh.NewOption("Stage this runtime", initLLMRuntimeTemplateActionUse),
						huh.NewOption("Customize runtime details", initLLMRuntimeTemplateActionCustomize),
						huh.NewOption("Back to runtime choices", initDetailActionBack),
					).
					Value(&action),
			).Title(detailTitle),
		)
		back, err := runBackableInitForm(actionForm, p.stdin, p.stderr)
		if err != nil {
			return false, false, false, err
		}
		if back || action == initDetailActionBack {
			return false, true, false, nil
		}
		if action == initLLMRuntimeTemplateActionUse {
			return true, false, false, nil
		}
	}

	action := initDetailActionEdit
	detailOptions := []huh.Option[string]{
		huh.NewOption("Edit runtime details", initDetailActionEdit),
	}
	if _, ok := runtimes[selection]; ok {
		detailOptions = append(detailOptions, huh.NewOption("Mark runtime for deletion", initDetailActionDelete))
	}
	detailOptions = append(detailOptions, huh.NewOption("Back to runtime choices", initDetailActionBack))
	actionForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("LLM runtime details").
				Description(detailDescription).
				Options(detailOptions...).
				Value(&action),
		).Title(detailTitle),
	)
	back, err := runBackableInitForm(actionForm, p.stdin, p.stderr)
	if err != nil {
		return false, false, false, err
	}
	if back || action == initDetailActionBack {
		return false, true, false, nil
	}
	if action == initDetailActionDelete {
		return false, false, true, nil
	}
	editAction := initDetailActionEdit
	editForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Runtime detail action").
				Options(
					huh.NewOption("Stage these runtime details", initDetailActionEdit),
					huh.NewOption("Back to runtime actions", initDetailActionBack),
				).
				Value(&editAction),
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
		).Title(detailTitle),
	)
	back, err = runBackableInitForm(editForm, p.stdin, p.stderr)
	if err != nil {
		return false, false, false, err
	}
	if back || editAction == initDetailActionBack {
		return false, true, false, nil
	}
	return false, false, false, nil
}

func (p huhInitLLMRuntimePrompter) chooseLLMRuntimeDeleteReplacement(prompt initLLMRuntimePrompt, deletedRuntimeName string, seed initDraft) (initDraft, error) {
	choice := ""
	options := make([]huh.Option[string], 0, len(prompt.Context.LLMRuntimes)+6)
	for _, option := range initLLMRuntimeOptions(prompt.Context.LLMRuntimes) {
		if option.Value == deletedRuntimeName {
			continue
		}
		options = append(options, option)
	}
	options = append(options, huh.NewOption("Back to runtime details", initBackSelection))
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Replacement LLM runtime").
				Description("Choose the runtime that should replace this deleted runtime for every affected profile.").
				Options(options...).
				Value(&choice),
		).Title("LLM Runtime Replacement"),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return initDraft{}, err
	}
	if back || choice == initBackSelection {
		return initDraft{}, errInitNavigateBack
	}
	replacementDraft := seed
	if choice != initCustomLLMRuntimeSelection {
		applyLLMRuntimeInventorySelection(&replacementDraft, choice, prompt.Context.LLMRuntimes)
		resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(replacementDraft).Preset)
		applyLLMRuntimeSelection(&replacementDraft, resolvedRuntimePreset)
		return replacementDraft, nil
	}
	customDraft := replacementDraft
	usedSelection, back, deleted, err := p.editLLMRuntimeDetails(choice, &customDraft, prompt.Context.LLMRuntimes)
	if err != nil {
		return initDraft{}, err
	}
	if usedSelection || back || deleted {
		return initDraft{}, errInitNavigateBack
	}
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(customDraft).Preset)
	applyLLMRuntimeSelection(&customDraft, resolvedRuntimePreset)
	return customDraft, nil
}

func (p huhInitLLMRuntimePrompter) runtimeAvailabilityNote(preset initLLMRuntimePreset) string {
	if p.checker == nil {
		return ""
	}
	return strings.TrimSpace(p.checker(preset))
}

func (p huhInitReviewerEntityPrompter) EditReviewerEntity(prompt initReviewerEntityPrompt) (initDraft, error) {
	draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
	selectedReviewerEntity := prompt.Context.ProfileReviewerEntities[prompt.Context.ExistingProfileName]
	reviewerMode := string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	if selectedReviewerEntity == "" {
		selectedReviewerEntity = reviewerMode
	} else {
		selectedReviewerEntity = normalizeReviewerEntitySelectionValue(selectedReviewerEntity, prompt.Context.ReviewerEntities)
	}
	for {
		choice := selectedReviewerEntity
		options := initReviewerEntityOptions(prompt.Context.ReviewerEntities, focusedReviewerEntityFallbackLabel(prompt.Context.ExistingProfileName))
		pendingDeleteNames := make([]string, 0, len(prompt.Context.PendingReviewerEntityDeletes))
		for name := range prompt.Context.PendingReviewerEntityDeletes {
			pendingDeleteNames = append(pendingDeleteNames, name)
		}
		sort.Strings(pendingDeleteNames)
		for _, name := range pendingDeleteNames {
			options = append(options, huh.NewOption(reviewerEntityDeletePendingLabel(name), initUndoReviewerEntityDeletePrefix+name))
		}
		options = append(options, huh.NewOption("Back to main menu", initBackSelection))
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Reviewer entity").
					Description(reviewerEntitySelectionDescription()).
					Options(options...).
					Value(&choice),
			).Title("Reviewer Entity"),
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initDraft{}, err
		}
		if back || choice == initBackSelection {
			return initDraft{}, errInitNavigateBack
		}
		if strings.HasPrefix(choice, initUndoReviewerEntityDeletePrefix) {
			return initDraft{
				Action:       initDraftActionUndoDeleteReviewerEntity,
				ActionTarget: strings.TrimPrefix(choice, initUndoReviewerEntityDeletePrefix),
			}, nil
		}
		candidateDraft := draft
		applyReviewerEntityInventorySelection(&candidateDraft, choice, prompt.Context.ReviewerEntities)
		reviewerMode = string(initReviewerEntityDraftFromSeedDraft(candidateDraft).Kind)
		applyReviewerEntitySelection(&candidateDraft, reviewerMode)
		detailDraft := candidateDraft
		back, deleted, err := p.editReviewerEntityDetails(choice, &detailDraft, prompt.Context.ReviewerEntities)
		if err != nil {
			return initDraft{}, err
		}
		if back {
			selectedReviewerEntity = choice
			continue
		}
		if deleted {
			return initDraft{
				Action:       initDraftActionDeleteReviewerEntity,
				ActionTarget: choice,
			}, nil
		}
		reviewerMode = string(initReviewerEntityDraftFromSeedDraft(detailDraft).Kind)
		applyReviewerEntitySelection(&detailDraft, reviewerMode)
		return detailDraft, nil
	}
}

func (p huhInitReviewerEntityPrompter) editReviewerEntityDetails(selection string, draft *initDraft, entities map[string]initReviewerEntityDraft) (bool, bool, error) {
	action := initDetailActionEdit
	detailOptions := []huh.Option[string]{
		huh.NewOption("Edit reviewer details", initDetailActionEdit),
	}
	if _, ok := entities[selection]; ok {
		detailOptions = append(detailOptions, huh.NewOption("Mark reviewer entity for deletion", initDetailActionDelete))
	}
	detailOptions = append(detailOptions, huh.NewOption("Back to reviewer choices", initDetailActionBack))
	actionForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Reviewer entity details").
				Options(detailOptions...).
				Value(&action),
		).Title("Reviewer Entity Details"),
	)
	back, err := runBackableInitForm(actionForm, p.stdin, p.stderr)
	if err != nil {
		return false, false, err
	}
	if back || action == initDetailActionBack {
		return true, false, nil
	}
	if action == initDetailActionDelete {
		return false, true, nil
	}
	editDraft := *draft
	reviewerMode := string(initReviewerEntityDraftFromSeedDraft(editDraft).Kind)
	reviewerTypeForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Reviewer entity type").
				Options(
					huh.NewOption(reviewerEntityTemplateFallbackLabel(), string(initReviewerEntityKindUseGitIdentity)),
					huh.NewOption(reviewerEntityTemplatePATLabel(), string(initReviewerEntityKindPAT)),
					huh.NewOption(reviewerEntityTemplateGitHubAppLabel(), string(initReviewerEntityKindGitHubApp)),
				).
				Value(&reviewerMode),
		).Title("Reviewer Entity Details"),
	)
	back, err = runBackableInitForm(reviewerTypeForm, p.stdin, p.stderr)
	if err != nil {
		return false, false, err
	}
	if back {
		return true, false, nil
	}
	if initReviewerEntityKind(reviewerMode) != initReviewerEntityKindUseGitIdentity {
		labelAction := initDetailActionEdit
		labelActionForm := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Reviewer label action").
					Options(
						huh.NewOption("Use this reviewer label", initDetailActionEdit),
						huh.NewOption("Back without staging", initDetailActionBack),
					).
					Value(&labelAction),
			).Title("Reviewer Entity Details"),
		)
		back, err = runBackableInitForm(labelActionForm, p.stdin, p.stderr)
		if err != nil {
			return false, false, err
		}
		if back || labelAction == initDetailActionBack {
			return true, false, nil
		}
		labelInputForm := huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Reviewer entity label").
					Description("Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.").
					Value(&editDraft.ReviewerDisplayName).
					Validate(validateOptionalDisplayName),
			).Title("Reviewer Entity Details"),
		)
		back, err = runBackableInitForm(labelInputForm, p.stdin, p.stderr)
		if err != nil {
			return false, false, err
		}
		if back {
			return true, false, nil
		}
		editDraft.ReviewerDisplayName = normalizeOptionalDisplayName(editDraft.ReviewerDisplayName)

		secretLocationAction := initReviewerSecretLocationActionStandard
		if currentReviewerRefUsesCustomLocation(editDraft) {
			secretLocationAction = initReviewerSecretLocationActionCustom
		}
		secretLocationForm := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Reviewer secret location").
					Description("Most users should leave the standard secret location in place.").
					Options(
						huh.NewOption("Use the standard reviewer secret location", initReviewerSecretLocationActionStandard),
						huh.NewOption("Use a custom reviewer secret location", initReviewerSecretLocationActionCustom),
						huh.NewOption("Back without staging", initDetailActionBack),
					).
					Value(&secretLocationAction),
			).Title("Reviewer Entity Details"),
		)
		back, err = runBackableInitForm(secretLocationForm, p.stdin, p.stderr)
		if err != nil {
			return false, false, err
		}
		if back || secretLocationAction == initDetailActionBack {
			return true, false, nil
		}
		editDraft.AdvancedStorageLabels = secretLocationAction == initReviewerSecretLocationActionCustom
		if !editDraft.AdvancedStorageLabels {
			editDraft.ReviewerCredentialRef = ""
		}
		if editDraft.AdvancedStorageLabels {
			customLocationAction := initDetailActionEdit
			customLocationForm := huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Reviewer secret location action").
						Options(
							huh.NewOption("Use this reviewer secret location", initDetailActionEdit),
							huh.NewOption("Back without staging", initDetailActionBack),
						).
						Value(&customLocationAction),
					huh.NewInput().
						Title("Reviewer secret location").
						Description("Leave blank to use the standard profile-based secret location for this separate reviewer credential.").
						Value(&editDraft.ReviewerCredentialRef).
						Validate(validateOptionalCredentialRef),
				).Title("Reviewer Entity Details"),
			)
			back, err = runBackableInitForm(customLocationForm, p.stdin, p.stderr)
			if err != nil {
				return false, false, err
			}
			if back || customLocationAction == initDetailActionBack {
				return true, false, nil
			}
		}
	}
	editAction := initDetailActionEdit
	editActionForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Reviewer detail action").
				Options(
					huh.NewOption("Stage these reviewer settings", initDetailActionEdit),
					huh.NewOption("Back without staging", initDetailActionBack),
				).
				Value(&editAction),
		).Title("Reviewer Entity Details"),
	)
	back, err = runBackableInitForm(editActionForm, p.stdin, p.stderr)
	if err != nil {
		return false, false, err
	}
	if back || editAction == initDetailActionBack {
		return true, false, nil
	}
	applyReviewerEntitySelection(&editDraft, reviewerMode)
	*draft = editDraft
	return false, false, nil
}

const (
	initReviewerSecretLocationActionStandard = "reviewer_secret_location_standard"
	initReviewerSecretLocationActionCustom   = "reviewer_secret_location_custom"
)

func currentReviewerRefUsesCustomLocation(draft initDraft) bool {
	if !draft.ReviewerEnabled {
		return false
	}
	reviewerRef := strings.TrimSpace(draft.ReviewerCredentialRef)
	if reviewerRef == "" {
		return false
	}
	defaultRef, err := credentials.FormatRef(draft.ProfileName + "-reviewer")
	if err != nil {
		return false
	}
	return reviewerRef != defaultRef
}

func (p huhInitPrompter) Run(ctx initPromptContext) (initDraft, error) {
	for {
		selectedProfileName := ctx.ExistingProfileName
		selectedExistingProfile := ctx.ExistingProfile
		selectedCreateNewProfile := false
		hasProfileChooser := len(ctx.ExistingProfileNames) > 0
		if len(ctx.ExistingProfileNames) > 0 {
			choice := initCreateProfileSentinel
			options := make([]huh.Option[string], 0, len(ctx.ExistingProfileNames)+len(ctx.PendingProfileDeletes)+2)
			for _, name := range ctx.ExistingProfileNames {
				options = append(options, huh.NewOption("Edit "+name, name))
				if name == ctx.ExistingProfileName {
					choice = name
				}
			}
			pendingDeleteNames := make([]string, 0, len(ctx.PendingProfileDeletes))
			for name := range ctx.PendingProfileDeletes {
				pendingDeleteNames = append(pendingDeleteNames, name)
			}
			sort.Strings(pendingDeleteNames)
			for _, name := range pendingDeleteNames {
				options = append(options, huh.NewOption(profileDeletePendingLabel(name), initUndoProfileDeletePrefix+name))
			}
			if ctx.ExistingProfile == nil {
				choice = initCreateProfileSentinel
			}
			options = append(options, huh.NewOption("Create new profile", initCreateProfileSentinel))
			options = append(options, huh.NewOption("Back to main menu", initBackSelection))
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Choose a profile to edit or create").
						Options(options...).
						Value(&choice),
				),
			)
			back, err := runBackableInitForm(form, p.stdin, p.stderr)
			if err != nil {
				return initDraft{}, err
			}
			if back || choice == initBackSelection {
				return initDraft{}, errInitNavigateBack
			}
			if strings.HasPrefix(choice, initUndoProfileDeletePrefix) {
				return initDraft{
					Action:       initDraftActionUndoDeleteProfile,
					ActionTarget: strings.TrimPrefix(choice, initUndoProfileDeletePrefix),
				}, nil
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
		} else {
			selectedReviewerEntity = normalizeReviewerEntitySelectionValue(selectedReviewerEntity, ctx.ReviewerEntities)
		}
		selectedLLMRuntime := ctx.ProfileLLMRuntimes[selectedProfileName]
		if selectedLLMRuntime == "" {
			if selectedRuntimePreset == string(initLLMRuntimePresetCustom) {
				selectedLLMRuntime = initCustomLLMRuntimeSelection
			} else {
				selectedLLMRuntime = selectedRuntimePreset
			}
		}
		selectedStorageLabelsMode := initStorageLabelsStandard
		if draft.AdvancedStorageLabels {
			selectedStorageLabelsMode = initStorageLabelsCustom
		}
		currentRoutes := currentProfileRouteSpecs(ctx.ExistingConfig.RepositoryProfiles, selectedProfileName)
		selectedRepositoryRoutesAction := string(initRoutesActionPreserve)
		gitScopeOptions := initGitScopeOptions(ctx.GitScopes)
		reviewerEntityOptions := initReviewerEntityOptions(ctx.ReviewerEntities, profileEditorReviewerEntityFallbackLabel(selectedProfileName))
		llmRuntimeOptions := initLLMRuntimeOptions(ctx.LLMRuntimes)
		detailAction := initDetailActionEdit
		detailBackLabel := "Back to main menu"
		if hasProfileChooser {
			detailBackLabel = "Back to profile choices"
		}

		detailOptions := []huh.Option[string]{
			huh.NewOption("Edit profile details", initDetailActionEdit),
		}
		if selectedExistingProfile != nil {
			detailOptions = append(detailOptions, huh.NewOption("Mark profile for deletion", initDetailActionDelete))
		}
		detailOptions = append(detailOptions, huh.NewOption(detailBackLabel, initDetailActionBack))

		actionForm := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Review profile details").
					Options(detailOptions...).
					Value(&detailAction),
			).Title("Review Profile"),
		)
		back, err := runBackableInitForm(actionForm, p.stdin, p.stderr)
		if err != nil {
			return initDraft{}, err
		}
		if back || detailAction == initDetailActionBack {
			if hasProfileChooser {
				continue
			}
			return initDraft{}, errInitNavigateBack
		}
		if detailAction == initDetailActionDelete {
			return initDraft{
				Action:       initDraftActionDeleteProfile,
				ActionTarget: selectedProfileName,
			}, nil
		}

		reviewerProfileFields := []huh.Field{
			huh.NewSelect[string]().
				Title("Reviewer entity").
				Description(reviewerEntitySelectionDescription()).
				Options(reviewerEntityOptions...).
				Value(&selectedReviewerEntity),
		}
		if selectedReviewerEntity != string(initReviewerEntityKindUseGitIdentity) {
			reviewerProfileFields = append(reviewerProfileFields,
				huh.NewInput().
					Title("Reviewer entity label").
					Description("Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.").
					Value(&draft.ReviewerDisplayName).
					Validate(validateOptionalDisplayName),
			)
		}
		reviewerProfileFields = append(reviewerProfileFields,
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
			huh.NewSelect[string]().
				Title("Storage label handling").
				Description("Choose whether to use the standard credential-store labels or override them for this profile.").
				Options(
					huh.NewOption("Use standard storage labels (recommended)", initStorageLabelsStandard),
					huh.NewOption("Customize storage labels (advanced)", initStorageLabelsCustom),
				).
				Value(&selectedStorageLabelsMode),
			huh.NewSelect[string]().
				Title("Repository routes").
				Description("Choose how cr should select this profile automatically when --profile is omitted.").
				Options(profileRouteActionOptions(len(currentRoutes) > 0)...).
				Value(&selectedRepositoryRoutesAction),
		)

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
			huh.NewGroup(reviewerProfileFields...).Title("Review Profile"),
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
				return selectedStorageLabelsMode != initStorageLabelsCustom
			}).Title("Advanced Storage Labels"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initDraft{}, err
		}
		if back {
			if hasProfileChooser {
				continue
			}
			return initDraft{}, errInitNavigateBack
		}
		typedReviewerDisplayName := normalizeOptionalDisplayName(draft.ReviewerDisplayName)
		draft.AdvancedStorageLabels = selectedStorageLabelsMode == initStorageLabelsCustom
		applyGitScopeSelection(&draft, selectedGitScope, ctx.GitScopes)
		applyReviewerEntityInventorySelection(&draft, selectedReviewerEntity, ctx.ReviewerEntities)
		applyLLMRuntimeInventorySelection(&draft, selectedLLMRuntime, ctx.LLMRuntimes)
		// Inventory selections fill provider/auth/ref fields; rerunning the mode finalizers applies side effects like clearing refs for self-reviewers or subscription runtimes.
		reviewerMode = string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
		selectedRuntimePreset = string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyReviewerEntitySelection(&draft, reviewerMode)
		applyLLMRuntimeSelection(&draft, selectedRuntimePreset)
		if reviewerMode == string(initReviewerEntityKindUseGitIdentity) {
			draft.ReviewerDisplayName = ""
		} else {
			draft.ReviewerDisplayName = typedReviewerDisplayName
		}
		draft.RepositoryRoutesAction = selectedRepositoryRoutesAction
		return draft, nil
	}
}

func profileRouteActionOptions(hasRoutes bool) []huh.Option[string] {
	if !hasRoutes {
		return []huh.Option[string]{
			huh.NewOption("Skip automatic profile-selection routes for now", string(initRoutesActionPreserve)),
			huh.NewOption("Add automatic profile-selection routes", string(initRoutesActionEdit)),
		}
	}
	options := []huh.Option[string]{
		huh.NewOption("Keep current automatic profile-selection routes", string(initRoutesActionPreserve)),
		huh.NewOption("Edit automatic profile-selection routes", string(initRoutesActionEdit)),
	}
	if hasRoutes {
		options = append(options, huh.NewOption("Remove all automatic profile-selection routes for this profile", string(initRoutesActionReset)))
	}
	return options
}

func initReviewerEntityDraftFromSeedDraft(draft initDraft) initReviewerEntityDraft {
	if !draft.ReviewerEnabled {
		return initReviewerEntityDraft{Kind: initReviewerEntityKindUseGitIdentity}
	}
	entity := initReviewerEntityDraft{
		AuthMode:      config.GitAuthMode(draft.ReviewerAuth),
		CredentialRef: strings.TrimSpace(draft.ReviewerCredentialRef),
		DisplayName:   normalizeOptionalDisplayName(draft.ReviewerDisplayName),
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

func initReviewerEntityOptions(entities map[string]initReviewerEntityDraft, fallbackLabel string) []huh.Option[string] {
	names := configuredInitReviewerEntityNames(entities)
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+3)
	for _, name := range names {
		entity := entities[name]
		options = append(options, huh.NewOption(initReviewerEntityLabel(entity), name))
	}
	options = append(options,
		huh.NewOption(fallbackLabel, string(initReviewerEntityKindUseGitIdentity)),
		huh.NewOption(reviewerEntityTemplatePATLabel(), string(initReviewerEntityKindPAT)),
		huh.NewOption(reviewerEntityTemplateGitHubAppLabel(), string(initReviewerEntityKindGitHubApp)),
	)
	return dedupeInitStringOptions(options)
}

func configuredInitReviewerEntityNames(entities map[string]initReviewerEntityDraft) []string {
	names := make([]string, 0, len(entities))
	for name, entity := range entities {
		if entity.Kind == initReviewerEntityKindUseGitIdentity {
			continue
		}
		names = append(names, name)
	}
	return names
}

func countConfiguredInitReviewerEntities(entities map[string]initReviewerEntityDraft) int {
	return len(configuredInitReviewerEntityNames(entities))
}

func normalizeReviewerEntitySelectionValue(selection string, entities map[string]initReviewerEntityDraft) string {
	entity, ok := entities[selection]
	if !ok {
		return selection
	}
	if entity.Kind == initReviewerEntityKindUseGitIdentity {
		return string(initReviewerEntityKindUseGitIdentity)
	}
	return selection
}

func reviewerEntitySelectionDescription() string {
	return "Choose who posts COMMENT, APPROVE, or REQUEST_CHANGES for this profile. Profiles with no separate reviewer entity post using their profile Git account."
}

func reviewerEntityTemplateFallbackLabel() string {
	return "Use a profile's Git account (no separate reviewer entity)"
}

func reviewerEntityTemplatePATLabel() string {
	return "Use a personal access token (PAT) reviewer"
}

func reviewerEntityTemplateGitHubAppLabel() string {
	return "Use a GitHub App reviewer"
}

func profileEditorReviewerEntityFallbackLabel(profileName string) string {
	if strings.TrimSpace(profileName) == "" {
		return reviewerEntityTemplateFallbackLabel()
	}
	return "None (uses this profile's Git account; no separate reviewer entity)"
}

func focusedReviewerEntityFallbackLabel(profileName string) string {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return reviewerEntityTemplateFallbackLabel()
	}
	return fmt.Sprintf("None (uses the %s profile's Git account)", profileName)
}

func initReviewerEntityLabel(entity initReviewerEntityDraft) string {
	displayName := normalizeOptionalDisplayName(entity.DisplayName)
	switch entity.Kind {
	case initReviewerEntityKindUseGitIdentity:
		return "None (uses each profile's Git account)"
	case initReviewerEntityKindGitHubApp:
		if displayName != "" {
			return fmt.Sprintf("%s (GitHub App reviewer)", displayName)
		}
		return fmt.Sprintf("GitHub App reviewer: %s", reviewerEntityFallbackIdentityLabel(entity))
	case initReviewerEntityKindPAT:
		if displayName != "" {
			return fmt.Sprintf("%s (PAT reviewer)", displayName)
		}
		return fmt.Sprintf("PAT reviewer: %s", reviewerEntityFallbackIdentityLabel(entity))
	default:
		if displayName != "" {
			return displayName
		}
	}
	return entity.Name
}

func reviewerEntityFallbackIdentityLabel(entity initReviewerEntityDraft) string {
	ref := strings.TrimSpace(entity.CredentialRef)
	if ref == "" {
		return strings.TrimSpace(entity.Name)
	}
	parsed, err := credentials.ParseRef(ref)
	if err == nil {
		profileSegment := strings.TrimSpace(parsed.Profile)
		if profileSegment != "" {
			return profileSegment
		}
	}
	pathSegment := ref
	if slash := strings.LastIndex(ref, "/"); slash >= 0 && slash < len(ref)-1 {
		pathSegment = ref[slash+1:]
	}
	return pathSegment
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
	draft.ReviewerDisplayName = normalizeOptionalDisplayName(entity.DisplayName)
	if !draft.AdvancedStorageLabels {
		draft.ReviewerCredentialRef = entity.CredentialRef
	}
}

func applyReviewerEntitySelection(draft *initDraft, selection string) {
	switch initReviewerEntityKind(selection) {
	case initReviewerEntityKindUseGitIdentity:
		draft.ReviewerEnabled = false
		draft.ReviewerAuth = string(config.GitAuthModePAT)
		draft.ReviewerDisplayName = ""
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
		options = append(options, huh.NewOption("Configured: "+initLLMRuntimeLabel(runtime), name))
	}
	options = append(options,
		huh.NewOption("Template: Claude CLI subscription", string(initLLMRuntimePresetClaudeCLISubscription)),
		huh.NewOption("Template: Codex CLI subscription", string(initLLMRuntimePresetCodexCLISubscription)),
		huh.NewOption("Template: Pi local runtime", string(initLLMRuntimePresetPiLocal)),
		huh.NewOption("Template: Anthropic API key", string(initLLMRuntimePresetAnthropicAPIKey)),
		huh.NewOption("Template: OpenAI API key", string(initLLMRuntimePresetOpenAIAPIKey)),
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

func initLLMRuntimeSelectionDescription(runtime initLLMRuntimeDraft, availability string) string {
	lines := []string{
		fmt.Sprintf("Selected runtime: %s.", initLLMRuntimeLabel(runtime)),
	}
	switch runtime.Preset {
	case initLLMRuntimePresetClaudeCLISubscription:
		lines = append(lines, "Uses your existing Claude CLI login. cr does not store a Claude subscription secret or launch browser sign-in.")
	case initLLMRuntimePresetCodexCLISubscription:
		lines = append(lines, "Uses your existing Codex CLI login. cr does not store a Codex subscription secret or launch browser sign-in.")
	case initLLMRuntimePresetPiLocal:
		lines = append(lines, "Uses the local Pi runtime without storing an additional cr-managed secret.")
	case initLLMRuntimePresetAnthropicAPIKey:
		lines = append(lines, "This runtime needs an Anthropic API key. init will gather it in this runtime flow and stage the write until Commit staged changes and exit.")
	case initLLMRuntimePresetOpenAIAPIKey:
		lines = append(lines, "This runtime needs an OpenAI API key. init will gather it in this runtime flow and stage the write until Commit staged changes and exit.")
	case initLLMRuntimePresetCustom:
		lines = append(lines, "Customize provider, auth mode, and adapter before staging this runtime for the active profile.")
	}
	if availability != "" {
		lines = append(lines, availability)
	}
	return strings.Join(lines, "\n")
}

func defaultInitLLMRuntimeAvailabilityNote(preset initLLMRuntimePreset) string {
	command := ""
	label := ""
	switch preset {
	case initLLMRuntimePresetClaudeCLISubscription:
		command = "claude"
		label = "Claude CLI"
	case initLLMRuntimePresetCodexCLISubscription:
		command = "codex"
		label = "Codex CLI"
	case initLLMRuntimePresetPiLocal, initLLMRuntimePresetAnthropicAPIKey, initLLMRuntimePresetOpenAIAPIKey, initLLMRuntimePresetCustom:
		return ""
	default:
		return ""
	}
	output, err := exec.Command(command, "--version").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s check: `%s --version` not available.", label, command)
	}
	version := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if version == "" {
		return fmt.Sprintf("%s check: `%s --version` succeeded.", label, command)
	}
	return fmt.Sprintf("%s check: %s installed.", label, version)
}

func profileDeletePendingLabel(name string) string {
	return fmt.Sprintf("Restore %s (staged for deletion)", name)
}

func reviewerEntityDeletePendingLabel(name string) string {
	return fmt.Sprintf("Restore reviewer entity %s (staged for deletion)", name)
}

func llmRuntimeDeletePendingLabel(name string) string {
	return fmt.Sprintf("Restore LLM runtime %s (staged for deletion)", name)
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
		huh.NewOption("Back to main menu", initCredentialSecretActionBack),
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
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return "", err
	}
	if back {
		return initCredentialSecretActionBack, nil
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
	options = append(options, huh.NewOption("Back to credential choices", initSecretSourceBack))
	choice := options[0].Value
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[initSecretSource]().
				Title(fmt.Sprintf("How should init get %s?", prompt.Key)).
				Options(options...).
				Value(&choice),
		),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return "", err
	}
	if back {
		return initSecretSourceBack, nil
	}
	return choice, nil
}

func (p huhInitSecretPrompter) PasteSecret(prompt initSecretValuePrompt) (string, error) {
	var value string
	action := initDetailActionEdit
	field := huh.NewInput().
		Title(fmt.Sprintf("Paste %s", prompt.Key)).
		Description("Secret input is hidden.").
		Value(&value).
		EchoMode(huh.EchoModePassword).
		Validate(validateRequiredText("secret value is required"))
	if prompt.Key == credentials.GitHubAppPrivateKeyKey {
		field.Description("Secret input is hidden. Clipboard is recommended for multi-line private keys.")
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Secret value").
				Options(
					huh.NewOption("Paste secret", initDetailActionEdit),
					huh.NewOption("Back to secret source choices", initDetailActionBack),
				).
				Value(&action),
		).Title("Secret Value"),
		huh.NewGroup(field).WithHideFunc(func() bool {
			return action == initDetailActionBack
		}).Title("Secret Value"),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return "", err
	}
	if back || action == initDetailActionBack {
		return "", errInitSecretValueBack
	}
	return credentials.TrimSecretIngress(value), nil
}

func (p huhInitModelMapPrompter) EditModelMap(prompt initModelMapPrompt) (initModelMapEdit, error) {
	for {
		action := initModelMapActionPreserve
		options := []huh.Option[initModelMapAction]{
			huh.NewOption("Keep current model-map overrides", initModelMapActionPreserve),
			huh.NewOption("Edit tier mappings", initModelMapActionEdit),
			huh.NewOption("Back to main menu", initModelMapActionBack),
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
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initModelMapEdit{}, err
		}
		if back || action == initModelMapActionBack {
			return initModelMapEdit{}, errInitNavigateBack
		}
		switch action {
		case initModelMapActionPreserve:
			return initModelMapEdit{Apply: false}, nil
		case initModelMapActionReset:
			return initModelMapEdit{Apply: true, ModelMap: nil}, nil
		case initModelMapActionEdit:
		case initModelMapActionBack:
			return initModelMapEdit{}, errInitNavigateBack
		default:
			return initModelMapEdit{}, fmt.Errorf("unsupported model-map action %q", action)
		}

		detailAction := initDetailActionEdit
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
		form = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Model tier details").
					Options(
						huh.NewOption("Edit tier mappings", initDetailActionEdit),
						huh.NewOption("Back to model-map choices", initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Model Tiers"),
			huh.NewGroup(fields...).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Model Tiers"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initModelMapEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			continue
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
}

func (p huhInitAgentSourcesPrompter) EditAgentSources(prompt initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
	for {
		action := initAgentSourcesActionPreserve
		options := []huh.Option[initAgentSourcesAction]{
			huh.NewOption("Keep current agent source paths", initAgentSourcesActionPreserve),
			huh.NewOption("Edit agent source paths", initAgentSourcesActionEdit),
			huh.NewOption("Back to main menu", initAgentSourcesActionBack),
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
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initAgentSourcesEdit{}, err
		}
		if back || action == initAgentSourcesActionBack {
			return initAgentSourcesEdit{}, errInitNavigateBack
		}
		switch action {
		case initAgentSourcesActionPreserve:
			return initAgentSourcesEdit{Apply: false}, nil
		case initAgentSourcesActionReset:
			return initAgentSourcesEdit{Apply: true, Sources: nil}, nil
		case initAgentSourcesActionEdit:
		case initAgentSourcesActionBack:
			return initAgentSourcesEdit{}, errInitNavigateBack
		default:
			return initAgentSourcesEdit{}, fmt.Errorf("unsupported agent-sources action %q", action)
		}

		detailAction := initDetailActionEdit
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
		form = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Agent source details").
					Options(
						huh.NewOption("Edit agent source paths", initDetailActionEdit),
						huh.NewOption("Back to agent-source choices", initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Agent Sources"),
			huh.NewGroup(fields...).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Agent Sources"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initAgentSourcesEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			continue
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
}

func (p huhInitReviewPolicyPrompter) EditReviewPolicy(prompt initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
	for {
		action := initReviewPolicyActionPreserve
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[initReviewPolicyAction]().
					Title("Review policy").
					Options(
						huh.NewOption("Keep current review policy", initReviewPolicyActionPreserve),
						huh.NewOption("Edit review policy", initReviewPolicyActionEdit),
						huh.NewOption("Back to main menu", initReviewPolicyActionBack),
					).
					Value(&action),
			),
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initReviewPolicyEdit{}, err
		}
		if back || action == initReviewPolicyActionBack {
			return initReviewPolicyEdit{}, errInitNavigateBack
		}
		if action == initReviewPolicyActionPreserve {
			return initReviewPolicyEdit{Apply: false}, nil
		}
		if action != initReviewPolicyActionEdit {
			return initReviewPolicyEdit{}, fmt.Errorf("unsupported review-policy action %q", action)
		}

		detailAction := initDetailActionEdit
		policy := prompt.ReviewPolicy
		if policy.MajorEvent == "" {
			policy.MajorEvent = config.ReviewMajorEventComment
		}
		majorEvent := policy.MajorEvent
		selfApprove := policy.AllowSelfApprove
		selfApproveChoice := initReviewPolicySelfApproveChoice(selfApprove)
		resolveThreads := string(policy.ResolveThreads)
		resolveAfter := policy.ResolveAfter
		form = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Review policy details").
					Options(
						huh.NewOption("Edit review policy", initDetailActionEdit),
						huh.NewOption("Back to review-policy choices", initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Review Policy"),
			huh.NewGroup(
				huh.NewSelect[config.ReviewMajorEvent]().
					Title("Major findings event").
					Options(
						huh.NewOption("Comment", config.ReviewMajorEventComment),
						huh.NewOption("Request changes", config.ReviewMajorEventRequestChanges),
					).
					Value(&majorEvent),
				huh.NewSelect[string]().
					Title("Allow self-approve").
					Options(initReviewPolicySelfApproveOptions()...).
					Value(&selfApproveChoice),
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
			).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Review Policy"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initReviewPolicyEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			continue
		}
		selfApprove = initReviewPolicyAllowSelfApprove(selfApproveChoice)
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
}

func (p huhInitRoutesPrompter) EditRoutes(prompt initRoutesPrompt) (initRoutesEdit, error) {
	for {
		action := prompt.Action
		if action == "" {
			action = initRoutesActionPreserve
			options := []huh.Option[initRoutesAction]{
				huh.NewOption("Keep current repository routes", initRoutesActionPreserve),
				huh.NewOption("Edit repository routes", initRoutesActionEdit),
				huh.NewOption("Back to main menu", initRoutesActionBack),
			}
			if len(prompt.Routes) > 0 {
				options = append(options, huh.NewOption("Remove all routes for this profile", initRoutesActionReset))
			}
			if prompt.HostChanged && len(prompt.Routes) > 0 {
				options = []huh.Option[initRoutesAction]{
					huh.NewOption("Reconcile repository routes", initRoutesActionEdit),
					huh.NewOption("Remove all routes for this profile", initRoutesActionReset),
					huh.NewOption("Back to main menu", initRoutesActionBack),
				}
				action = initRoutesActionEdit
			}
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[initRoutesAction]().
						Title("Repository routes").
						Options(options...).
						Value(&action),
				),
			)
			back, err := runBackableInitForm(form, p.stdin, p.stderr)
			if err != nil {
				return initRoutesEdit{}, err
			}
			if back || action == initRoutesActionBack {
				return initRoutesEdit{}, errInitNavigateBack
			}
		}
		routeText := formatInitRouteSpecs(prompt.Routes)
		switch action {
		case initRoutesActionPreserve:
			return initRoutesEdit{Apply: false}, nil
		case initRoutesActionReset:
			return initRoutesEdit{Apply: true, Routes: nil}, nil
		case initRoutesActionEdit:
		case initRoutesActionBack:
			return initRoutesEdit{}, errInitNavigateBack
		default:
			return initRoutesEdit{}, fmt.Errorf("unsupported routes action %q", action)
		}

		detailAction := initDetailActionEdit
		fields := []huh.Field{}
		detailBackLabel := "Back to repository-route choices"
		if prompt.Action != "" {
			detailBackLabel = "Back to review profile"
		}
		fields = append(fields,
			huh.NewNote().
				Title("Automatic profile selection").
				Description("Routes tell cr when to use this profile automatically. Explicit --profile still wins; otherwise matching routes beat the default profile."),
			huh.NewNote().
				Title("Accepted route formats").
				Description("One per line: host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL."),
		)
		if prompt.HostChanged && len(prompt.Routes) > 0 {
			fields = append(fields, huh.NewNote().Description(fmt.Sprintf("The profile host changed from %s to %s. Update or remove the affected routes.", prompt.PreviousHost, prompt.ProfileHost)))
		}
		fields = append(fields,
			huh.NewText().
				Title("Route entries").
				Value(&routeText),
		)
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Repository route details").
					Options(
						huh.NewOption("Edit repository routes", initDetailActionEdit),
						huh.NewOption(detailBackLabel, initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Repository Routes"),
			huh.NewGroup(fields...).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Repository Routes"),
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initRoutesEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			if prompt.Action != "" {
				return initRoutesEdit{}, errInitNavigateBack
			}
			continue
		}
		routes, err := parseInitRouteSpecs(routeText)
		if err != nil {
			return initRoutesEdit{}, err
		}
		return initRoutesEdit{Apply: true, Routes: routes}, nil
	}
}

func (p huhInitRetentionPrompter) EditRetention(prompt initRetentionPrompt) (initRetentionEdit, error) {
	for {
		action := initRetentionActionPreserve
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[initRetentionAction]().
					Title("Data retention").
					Options(
						huh.NewOption("Keep current retention settings", initRetentionActionPreserve),
						huh.NewOption("Edit retention settings", initRetentionActionEdit),
						huh.NewOption("Reset retention to defaults", initRetentionActionReset),
						huh.NewOption("Back to main menu", initRetentionActionBack),
					).
					Value(&action),
			),
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initRetentionEdit{}, err
		}
		if back || action == initRetentionActionBack {
			return initRetentionEdit{}, errInitNavigateBack
		}
		switch action {
		case initRetentionActionPreserve:
			return initRetentionEdit{Apply: false}, nil
		case initRetentionActionReset:
			return initRetentionEdit{Apply: true, Retention: config.DefaultRetentionConfig()}, nil
		case initRetentionActionEdit:
		case initRetentionActionBack:
			return initRetentionEdit{}, errInitNavigateBack
		default:
			return initRetentionEdit{}, fmt.Errorf("unsupported retention action %q", action)
		}

		detailAction := initDetailActionEdit
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
				huh.NewSelect[string]().
					Title("Retention details").
					Options(
						huh.NewOption("Edit retention settings", initDetailActionEdit),
						huh.NewOption("Back to retention choices", initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Retention"),
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
			).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Retention"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initRetentionEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			continue
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
}

func (p huhInitKeyringBackendPrompter) EditKeyringBackend(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	for {
		action := initKeyringBackendActionPreserve
		options := []huh.Option[initKeyringBackendAction]{
			huh.NewOption("Keep current keyring backend", initKeyringBackendActionPreserve),
			huh.NewOption("Set keyring backend", initKeyringBackendActionEdit),
			huh.NewOption("Back to main menu", initKeyringBackendActionBack),
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
		)
		back, err := runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		if back || action == initKeyringBackendActionBack {
			return initKeyringBackendEdit{}, errInitNavigateBack
		}
		switch action {
		case initKeyringBackendActionPreserve:
			return initKeyringBackendEdit{Apply: false}, nil
		case initKeyringBackendActionReset:
			return initKeyringBackendEdit{Apply: true, Backend: ""}, nil
		case initKeyringBackendActionEdit:
		case initKeyringBackendActionBack:
			return initKeyringBackendEdit{}, errInitNavigateBack
		default:
			return initKeyringBackendEdit{}, fmt.Errorf("unsupported keyring-backend action %q", action)
		}

		detailAction := initDetailActionEdit
		backend := strings.TrimSpace(prompt.Backend)
		form = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Keyring backend details").
					Options(
						huh.NewOption("Set keyring backend", initDetailActionEdit),
						huh.NewOption("Back to keyring choices", initDetailActionBack),
					).
					Value(&detailAction),
			).Title("Keyring"),
			huh.NewGroup(
				huh.NewInput().
					Title("Persistent keyring backend").
					Description("Examples include file or memory. Leave runtime-only --backend choices out unless you want them saved.").
					Value(&backend).
					Validate(validateRequiredText("keyring backend is required")),
			).WithHideFunc(func() bool {
				return detailAction == initDetailActionBack
			}).Title("Keyring"),
		)
		back, err = runBackableInitForm(form, p.stdin, p.stderr)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		if back || detailAction == initDetailActionBack {
			continue
		}
		return initKeyringBackendEdit{Apply: true, Backend: strings.TrimSpace(backend)}, nil
	}
}

func initModelMapInputDescription(tier config.ModelTier, builtIn string) string {
	if builtIn = strings.TrimSpace(builtIn); builtIn != "" {
		return fmt.Sprintf("Leave blank to use the built-in %s model: %s", tier, builtIn)
	}
	return "Leave blank to remove the override for this tier."
}

func initReviewPolicySelfApproveOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("Do not allow self-approve (recommended)", initSelfApproveDisallow),
		huh.NewOption("Enable self-approve", initSelfApproveEnable),
	}
}

func initReviewPolicySelfApproveChoice(enabled bool) string {
	if enabled {
		return initSelfApproveEnable
	}
	return initSelfApproveDisallow
}

func initReviewPolicyAllowSelfApprove(choice string) bool {
	return choice == initSelfApproveEnable
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

func normalizeOptionalDisplayName(value string) string {
	return strings.TrimSpace(value)
}

func validateOptionalDisplayName(value string) error {
	trimmed := normalizeOptionalDisplayName(value)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return fmt.Errorf("display name must be a single line")
	}
	return nil
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
			draft.ReviewerDisplayName = normalizeOptionalDisplayName(existingProfile.ReviewerCredentials.DisplayName)
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
			if initReviewerEntityDraftFromConfig(config.Profile{ReviewerCredentials: profile.ReviewerCredentials}).matchesConfig(*previousProfile.ReviewerCredentials) {
				profile.ReviewerCredentials.DisplayName = previousProfile.ReviewerCredentials.DisplayName
			}
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
	activeRefs := map[string]bool{}
	for _, key := range entryKeys {
		entry := entriesByKey[key]
		activeRefs[entry.Ref.Ref] = true
		entries = append(entries, entry)
	}
	writes := filterInitWritesByRefs(session.writes, activeRefs)
	overwriteRefs := filterInitBoolMapByRefs(session.overwriteRefs, activeRefs)
	satisfiedRefs := filterInitBoolMapByRefs(session.satisfiedRefs, activeRefs)
	entries = refreshInteractiveCredentialPlan(entries, projectInitPlannedWriteKeys(writes), satisfiedRefs)
	return initSessionPlan{
		path:           session.path,
		cfg:            cloneInitConfigFile(session.cfg),
		profileNames:   profileNames,
		profileRefs:    profileRefs,
		writes:         writes,
		credentialPlan: entries,
		overwriteRefs:  overwriteRefs,
		satisfiedRefs:  satisfiedRefs,
		backendFlagSet: session.backendFlagSet,
		backendArg:     interactiveInitBackendArg(opts, session.backendFlagSet, session.cfg),
	}, nil
}

func collectInteractiveInitSessionWorkspaceSecrets(opts *root.Options, deps initDeps, session initSessionDraft, purposes []string) (initSessionDraft, error) {
	if session.workspace == nil {
		return session, nil
	}
	purposeSet := map[string]struct{}{}
	for _, purpose := range purposes {
		purposeSet[purpose] = struct{}{}
	}
	entries := make([]initCredentialPlanEntry, 0, len(session.workspace.credentialPlan))
	for _, entry := range session.workspace.credentialPlan {
		if _, ok := purposeSet[entry.Ref.Purpose]; !ok {
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return session, nil
	}
	workspace := *session.workspace
	workspace.writes = cloneInitWrites(session.writes)
	workspace.overwriteRefs = cloneInitBoolMap(session.overwriteRefs)
	workspace.satisfiedRefs = cloneInitBoolMap(session.satisfiedRefs)
	workspace.credentialPlan = refreshInteractiveCredentialPlan(entries, projectInitPlannedWriteKeys(workspace.writes), workspace.satisfiedRefs)
	nextWorkspace, err := collectInteractiveInitSecrets(nil, opts, deps, workspace)
	if err != nil {
		return session, err
	}
	session.workspace = &nextWorkspace
	session.cfg = cloneInitConfigFile(nextWorkspace.cfg)
	session.writes = cloneInitWrites(nextWorkspace.writes)
	session.overwriteRefs = cloneInitBoolMap(nextWorkspace.overwriteRefs)
	session.satisfiedRefs = cloneInitBoolMap(nextWorkspace.satisfiedRefs)
	return session, nil
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

func ensureInitDeleteState(session *initSessionDraft) {
	if session.pendingProfileDeletes == nil {
		session.pendingProfileDeletes = map[string]initPendingProfileDelete{}
	}
	if session.pendingReviewerEntityDeletes == nil {
		session.pendingReviewerEntityDeletes = map[string]initPendingReviewerEntityDelete{}
	}
	if session.pendingLLMRuntimeDeletes == nil {
		session.pendingLLMRuntimeDeletes = map[string]initPendingLLMRuntimeDelete{}
	}
}

func rebuildInteractiveInitWorkspace(session initSessionDraft, profileName string) initSessionDraft {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		session.workspace = nil
		session.requestedProfileName = ""
		return session
	}
	profile, ok := session.cfg.Profiles[profileName]
	if !ok {
		session.workspace = nil
		session.requestedProfileName = ""
		return session
	}
	gitScopes, profileGitScopeNames := buildInitGitScopeInventory(session.cfg)
	reviewerEntities, profileReviewerEntityNames := buildInitReviewerEntityInventory(session.cfg)
	llmRuntimes, profileRuntimeNames := buildInitLLMRuntimeInventory(session.cfg)
	workspace := initWorkspaceDraft{
		path:               session.path,
		cfg:                cloneInitConfigFile(session.cfg),
		profileName:        profileName,
		profile:            profile,
		gitScopeName:       profileGitScopeNames[profileName],
		gitScopes:          gitScopes,
		reviewerEntityName: profileReviewerEntityNames[profileName],
		reviewerEntities:   reviewerEntities,
		llmRuntimeName:     profileRuntimeNames[profileName],
		llmRuntimes:        llmRuntimes,
		writes:             map[string]map[string]string{},
		overwriteRefs:      map[string]bool{},
		satisfiedRefs:      map[string]bool{},
		backendFlagSet:     session.backendFlagSet,
		backendArg:         interactiveInitBackendArg(&root.Options{}, session.backendFlagSet, session.cfg),
		allowDeferredLLM:   profile.LLM.Auth == config.LLMAuthAPIKey,
		writeLLMHint:       profile.LLM.Auth == config.LLMAuthAPIKey,
	}
	session.workspace = &workspace
	session.requestedProfileName = profileName
	return session
}

func preferredInteractiveInitProfile(session initSessionDraft) string {
	if session.workspace != nil {
		if _, ok := session.cfg.Profiles[session.workspace.profileName]; ok {
			return session.workspace.profileName
		}
	}
	if session.requestedProfileName != "" {
		if _, ok := session.cfg.Profiles[session.requestedProfileName]; ok {
			return session.requestedProfileName
		}
	}
	return configedit.FirstProfileName(session.cfg.Profiles)
}

func deleteInteractiveInitProfile(session initSessionDraft, profileName string) (initSessionDraft, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return initSessionDraft{}, exitcode.Usage(configedit.ErrProfileNameRequired)
	}
	profile, ok := session.cfg.Profiles[profileName]
	if !ok {
		return initSessionDraft{}, cmderr.Config(fmt.Errorf("%w: %s", config.ErrProfileNotFound, profileName))
	}
	ensureInitDeleteState(&session)
	if _, exists := session.pendingProfileDeletes[profileName]; exists {
		return session, nil
	}
	prunedRoutes := make([]config.RepositoryProfile, 0, len(session.cfg.RepositoryProfiles))
	for _, route := range session.cfg.RepositoryProfiles {
		if route.Profile == profileName {
			prunedRoutes = append(prunedRoutes, route)
		}
	}
	fallbackDefault := configedit.FirstProfileName(withoutProfile(session.cfg.Profiles, profileName))
	touchedOriginal, hadTouchedProfile := session.touchedProfiles[profileName]
	pending := initPendingProfileDelete{
		ProfileName:       profileName,
		Profile:           cloneInitProfile(profile),
		OriginalDefault:   session.cfg.DefaultProfile,
		FallbackDefault:   fallbackDefault,
		PrunedRoutes:      append([]config.RepositoryProfile(nil), prunedRoutes...),
		WasActive:         session.workspace != nil && session.workspace.profileName == profileName,
		HadTouchedProfile: hadTouchedProfile,
		TouchedOriginal:   touchedOriginal,
	}
	delete(session.cfg.Profiles, profileName)
	session.cfg.RepositoryProfiles = configedit.PruneRepositoryProfileRoutes(session.cfg.RepositoryProfiles, profileName)
	if session.cfg.DefaultProfile == profileName {
		session.cfg.DefaultProfile = fallbackDefault
	}
	delete(session.touchedProfiles, profileName)
	session.pendingProfileDeletes[profileName] = pending
	return rebuildInteractiveInitWorkspace(session, preferredInteractiveInitProfile(session)), nil
}

func undoInteractiveInitProfileDelete(session initSessionDraft, profileName string) (initSessionDraft, error) {
	profileName = strings.TrimSpace(profileName)
	ensureInitDeleteState(&session)
	pending, ok := session.pendingProfileDeletes[profileName]
	if !ok {
		return session, nil
	}
	if session.cfg.Profiles == nil {
		session.cfg.Profiles = map[string]config.Profile{}
	}
	session.cfg.Profiles[profileName] = cloneInitProfile(pending.Profile)
	session.cfg.RepositoryProfiles = configedit.CanonicalRepositoryRoutes(append(append([]config.RepositoryProfile(nil), session.cfg.RepositoryProfiles...), pending.PrunedRoutes...))
	if pending.OriginalDefault == profileName && session.cfg.DefaultProfile == pending.FallbackDefault {
		session.cfg.DefaultProfile = profileName
	}
	if pending.HadTouchedProfile {
		if session.touchedProfiles == nil {
			session.touchedProfiles = map[string]string{}
		}
		session.touchedProfiles[profileName] = pending.TouchedOriginal
	}
	delete(session.pendingProfileDeletes, profileName)
	if session.workspace == nil {
		return rebuildInteractiveInitWorkspace(session, profileName), nil
	}
	return session, nil
}

func deleteInteractiveInitReviewerEntity(session initSessionDraft, entityName string) (initSessionDraft, error) {
	entityName = strings.TrimSpace(entityName)
	if entityName == "" {
		return initSessionDraft{}, exitcode.Usage(errors.New("reviewer entity name is required"))
	}
	ensureInitDeleteState(&session)
	if _, exists := session.pendingReviewerEntityDeletes[entityName]; exists {
		return session, nil
	}
	_, profileEntityNames := buildInitReviewerEntityInventory(session.cfg)
	affected := map[string]config.ReviewerCredentials{}
	for profileName, selected := range profileEntityNames {
		if selected != entityName {
			continue
		}
		profile := session.cfg.Profiles[profileName]
		if profile.ReviewerCredentials == nil {
			continue
		}
		affected[profileName] = *profile.ReviewerCredentials
		cleared, _ := configedit.ClearReviewerCredentials(profile)
		session.cfg.Profiles[profileName] = cleared
	}
	if len(affected) == 0 {
		return initSessionDraft{}, exitcode.Usage(fmt.Errorf("reviewer entity %q is not deletable", entityName))
	}
	session.pendingReviewerEntityDeletes[entityName] = initPendingReviewerEntityDelete{
		EntityName: entityName,
		Affected:   affected,
	}
	return rebuildInteractiveInitWorkspace(session, preferredInteractiveInitProfile(session)), nil
}

func undoInteractiveInitReviewerEntityDelete(session initSessionDraft, entityName string) (initSessionDraft, error) {
	entityName = strings.TrimSpace(entityName)
	ensureInitDeleteState(&session)
	pending, ok := session.pendingReviewerEntityDeletes[entityName]
	if !ok {
		return session, nil
	}
	for profileName, previous := range pending.Affected {
		profile, ok := session.cfg.Profiles[profileName]
		if !ok {
			continue
		}
		if profile.ReviewerCredentials != nil {
			continue
		}
		restored := previous
		profile.ReviewerCredentials = &restored
		session.cfg.Profiles[profileName] = profile
	}
	delete(session.pendingReviewerEntityDeletes, entityName)
	return rebuildInteractiveInitWorkspace(session, preferredInteractiveInitProfile(session)), nil
}

func deleteInteractiveInitLLMRuntime(session initSessionDraft, runtimeName string, replacement initLLMRuntimeDraft) (initSessionDraft, error) {
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return initSessionDraft{}, exitcode.Usage(errors.New("LLM runtime name is required"))
	}
	if replacement.Provider == "" || replacement.Auth == "" || replacement.Adapter == "" {
		return initSessionDraft{}, exitcode.Usage(errors.New("LLM runtime deletion requires a replacement runtime"))
	}
	ensureInitDeleteState(&session)
	if _, exists := session.pendingLLMRuntimeDeletes[runtimeName]; exists {
		return session, nil
	}
	_, profileRuntimeNames := buildInitLLMRuntimeInventory(session.cfg)
	original := map[string]config.LLMConfig{}
	applied := map[string]config.LLMConfig{}
	for profileName, selected := range profileRuntimeNames {
		if selected != runtimeName {
			continue
		}
		profile := session.cfg.Profiles[profileName]
		original[profileName] = cloneInitLLMConfig(profile.LLM)
		nextLLM, err := initLLMConfigForReplacement(profileName, replacement)
		if err != nil {
			return initSessionDraft{}, err
		}
		applied[profileName] = cloneInitLLMConfig(nextLLM)
		profile.LLM = nextLLM
		session.cfg.Profiles[profileName] = profile
	}
	if len(original) == 0 {
		return initSessionDraft{}, exitcode.Usage(fmt.Errorf("LLM runtime %q is not deletable", runtimeName))
	}
	session.pendingLLMRuntimeDeletes[runtimeName] = initPendingLLMRuntimeDelete{
		RuntimeName: runtimeName,
		Replacement: replacement,
		Applied:     applied,
		Original:    original,
	}
	return rebuildInteractiveInitWorkspace(session, preferredInteractiveInitProfile(session)), nil
}

func undoInteractiveInitLLMRuntimeDelete(session initSessionDraft, runtimeName string) (initSessionDraft, error) {
	runtimeName = strings.TrimSpace(runtimeName)
	ensureInitDeleteState(&session)
	pending, ok := session.pendingLLMRuntimeDeletes[runtimeName]
	if !ok {
		return session, nil
	}
	for profileName, previous := range pending.Original {
		profile, ok := session.cfg.Profiles[profileName]
		if !ok {
			continue
		}
		applied := pending.Applied[profileName]
		if !reflect.DeepEqual(profile.LLM, applied) {
			continue
		}
		profile.LLM = cloneInitLLMConfig(previous)
		session.cfg.Profiles[profileName] = profile
	}
	delete(session.pendingLLMRuntimeDeletes, runtimeName)
	return rebuildInteractiveInitWorkspace(session, preferredInteractiveInitProfile(session)), nil
}

func initLLMConfigForReplacement(profileName string, runtime initLLMRuntimeDraft) (config.LLMConfig, error) {
	llm := runtime.exportConfig()
	if llm.Auth != config.LLMAuthAPIKey {
		return llm, nil
	}
	if strings.TrimSpace(llm.CredentialRef) != "" {
		return llm, nil
	}
	ref, err := credentials.FormatRef(profileName + "-llm")
	if err != nil {
		return config.LLMConfig{}, exitcode.Usage(err)
	}
	llm.CredentialRef = ref
	return llm, nil
}

func withoutProfile(profiles map[string]config.Profile, removed string) map[string]config.Profile {
	if len(profiles) == 0 {
		return nil
	}
	next := make(map[string]config.Profile, max(len(profiles)-1, 0))
	for name, profile := range profiles {
		if name == removed {
			continue
		}
		next[name] = profile
	}
	return next
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
		DisplayName:   normalizeOptionalDisplayName(profile.ReviewerCredentials.DisplayName),
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
		DisplayName:   normalizeOptionalDisplayName(entity.DisplayName),
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
	displayNamesByKey := map[string]map[string]struct{}{}
	for _, profileName := range sortedProfileNames(cfg.Profiles) {
		profile := cfg.Profiles[profileName]
		entity := initReviewerEntityDraftFromConfig(profile)
		key := entity.identityKey()
		if entity.DisplayName != "" {
			if _, ok := displayNamesByKey[key]; !ok {
				displayNamesByKey[key] = map[string]struct{}{}
			}
			displayNamesByKey[key][entity.DisplayName] = struct{}{}
		}
		if existingName, ok := entityNamesByKey[key]; ok {
			profileEntityNames[profileName] = existingName
			continue
		}
		entity.Name = uniqueInitReviewerEntityName(entities, entity.suggestedName())
		entities[entity.Name] = entity
		entityNamesByKey[key] = entity.Name
		profileEntityNames[profileName] = entity.Name
	}
	for name, entity := range entities {
		displayNames := displayNamesByKey[entity.identityKey()]
		noDisplayName := len(displayNames) == 0
		conflictingDisplayNames := len(displayNames) > 1
		if noDisplayName || conflictingDisplayNames {
			entity.DisplayName = ""
			entities[name] = entity
			continue
		}
		for displayName := range displayNames {
			entity.DisplayName = displayName
		}
		entities[name] = entity
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
			DisplayName:   normalizeOptionalDisplayName(draft.ReviewerDisplayName),
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

func collectInteractiveInitRoutes(opts *root.Options, deps initDeps, plan initWorkspaceDraft, action initRoutesAction) (initWorkspaceDraft, error) {
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
	if action == "" {
		action = initRoutesActionPreserve
	}
	if action == initRoutesActionPreserve {
		if err := validateInitRouteHosts(plan.cfg.RepositoryProfiles, plan.profileName, plan.profile.Git.Host); err == nil {
			return plan, nil
		}
		// A preserved route set became invalid after the profile edit, usually
		// because git.host changed. Fall back to the route action chooser so the
		// user can explicitly reconcile, remove, or back out instead of silently
		// converting "preserve" into "edit".
		action = ""
	}
	if action == initRoutesActionReset {
		nextRoutes, err := applyInitProfileRoutes(plan.cfg.RepositoryProfiles, plan.profileName, plan.profile.Git.Host, nil)
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
	edit, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName:  plan.profileName,
		ProfileHost:  plan.profile.Git.Host,
		PreviousHost: previousHost,
		HostChanged:  hostChanged,
		Routes:       currentProfileRouteSpecs(plan.cfg.RepositoryProfiles, plan.profileName),
		Action:       action,
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
	credentialChoices:
		for {
			action, err := prompter.ChooseCredentialAction(initCredentialSecretPrompt{
				Entry:              entry,
				ClipboardSupported: deps.clipboardSupported(),
			})
			if err != nil {
				return initWorkspaceDraft{}, err
			}
			if action == initCredentialSecretActionBack {
				return initWorkspaceDraft{}, errInitNavigateBack
			}
			if action == initCredentialSecretActionDefer {
				break
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
				if action == initCredentialSecretActionBack {
					return initWorkspaceDraft{}, errInitNavigateBack
				}
			}
			if action == initCredentialSecretActionKeep {
				if !targetHasRequired {
					return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential ref %q does not have all required keys", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
				}
				plan.satisfiedRefs[entry.Ref.Ref] = true
				break
			}
			if action == initCredentialSecretActionDefer {
				break
			}
			if action != initCredentialSecretActionSetNow {
				return initWorkspaceDraft{}, fmt.Errorf("unsupported interactive secret action %q", action)
			}

			overwriteRef := false
			entryWrites := map[string]map[string]string{
				entry.Ref.Ref: copyStringMap(plan.writes[entry.Ref.Ref]),
			}
			for _, spec := range entry.KeySpecs {
			sourceChoices:
				for {
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
					if source == initSecretSourceBack {
						continue credentialChoices
					}
					switch source {
					case initSecretSourceKeepExisting:
						if !targetHasKey {
							return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential key %q does not exist for ref %q", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key, entry.Ref.Ref))
						}
					case initSecretSourceSkip:
						if spec.Required {
							return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential key %q is required", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key))
						}
					case initSecretSourceClipboard:
						value, err := deps.clipboardRead()
						if err != nil {
							return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("read clipboard: %w", err))
						}
						value = credentials.TrimSecretIngress(value)
						if value == "" {
							return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("clipboard supplied an empty secret"))
						}
						addWrite(entryWrites, entry.Ref.Ref, spec.Key, value)
					case initSecretSourcePaste:
						value, err := prompter.PasteSecret(initSecretValuePrompt{
							Entry:              entry,
							Key:                spec.Key,
							Optional:           !spec.Required,
							TargetHasKey:       targetHasKey,
							ClipboardSupported: deps.clipboardSupported(),
						})
						if errors.Is(err, errInitSecretValueBack) {
							continue sourceChoices
						}
						if err != nil {
							return initWorkspaceDraft{}, err
						}
						value = credentials.TrimSecretIngress(value)
						if value == "" {
							return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("pasted secret for %q is empty", spec.Key))
						}
						addWrite(entryWrites, entry.Ref.Ref, spec.Key, value)
					case initSecretSourceBack:
						continue credentialChoices
					default:
						return initWorkspaceDraft{}, fmt.Errorf("unsupported interactive secret source %q", source)
					}
					if targetHasKey {
						overwriteRef = true
					}
					break
				}
			}
			planned := entryWrites[entry.Ref.Ref]
			if !initCredentialWritePlanSatisfiesEntry(entry, targetKeys, planned) {
				return initWorkspaceDraft{}, exitcode.Usage(fmt.Errorf("%s credential ref %q still needs required keys; keep existing values or defer instead", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
			}
			if len(planned) == 0 {
				delete(plan.writes, entry.Ref.Ref)
			} else {
				plan.writes[entry.Ref.Ref] = planned
			}
			if overwriteRef {
				plan.overwriteRefs[entry.Ref.Ref] = true
			}
			plan.satisfiedRefs[entry.Ref.Ref] = true
			break
		}
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
		cfg:              plan.cfg,
		writes:           plan.writes,
		credentialPlan:   plan.credentialPlan,
		overwriteRefs:    plan.overwriteRefs,
		satisfiedRefs:    plan.satisfiedRefs,
		backendFlagSet:   plan.backendFlagSet,
		backendArg:       plan.backendArg,
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
		if entry.State == initCredentialPlanStateKeepExisting || len(entry.PlannedWriteKeys) > 0 {
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

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func cloneInitWrites(writes map[string]map[string]string) map[string]map[string]string {
	if len(writes) == 0 {
		return map[string]map[string]string{}
	}
	cloned := make(map[string]map[string]string, len(writes))
	for ref, values := range writes {
		cloned[ref] = copyStringMap(values)
	}
	return cloned
}

func cloneInitBoolMap(values map[string]bool) map[string]bool {
	if len(values) == 0 {
		return map[string]bool{}
	}
	cloned := make(map[string]bool, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func filterInitWritesByRefs(writes map[string]map[string]string, activeRefs map[string]bool) map[string]map[string]string {
	filtered := map[string]map[string]string{}
	for ref, values := range writes {
		if !activeRefs[ref] {
			continue
		}
		filtered[ref] = copyStringMap(values)
	}
	return filtered
}

func filterInitBoolMapByRefs(values map[string]bool, activeRefs map[string]bool) map[string]bool {
	filtered := map[string]bool{}
	for key, value := range values {
		if !activeRefs[key] {
			continue
		}
		filtered[key] = value
	}
	return filtered
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
