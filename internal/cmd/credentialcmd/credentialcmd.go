// Package credentialcmd wires credential ingress commands.
package credentialcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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
	gitRef             string
	reviewerRef        string
	reviewerAuth       string
	llmProvider        string
	llmAuth            string
	llmAdapter         string
	llmRef             string
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
}

type initPrompter interface {
	Run(initPromptContext) (initDraft, error)
}

type initModelMapPrompter interface {
	EditModelMap(initModelMapPrompt) (initModelMapEdit, error)
}

type initPromptContext struct {
	RequestedProfileName string
	ExistingProfileName  string
	ExistingProfile      *config.Profile
	ExistingProfileNames []string
	DefaultProfileName   string
	ExistingConfig       config.File
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
}

type initModelMapPrompt struct {
	LLM      config.LLMConfig
	ModelMap config.ModelMap
}

type initModelMapEdit struct {
	Apply    bool
	ModelMap config.ModelMap
}

type initDeps struct {
	prompter           initPrompter
	modelMapPrompter   initModelMapPrompter
	secretPrompter     initSecretPrompter
	clipboardSupported func() bool
	clipboardRead      func() (string, error)
	configPath         func(*root.Options) (string, error)
	loadConfig         func(string) (config.File, bool, error)
	saveConfig         func(string, config.File) error
	openStore          func(string, bool, config.File) (initStore, error)
	readSecret         func(io.Reader, bool, string, string, string) (string, bool, error)
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
		modelMapPrompter:   nil,
		clipboardSupported: func() bool { return !clipboard.Unsupported },
		clipboardRead:      clipboard.ReadAll,
		configPath:         configPath,
		loadConfig:         loadConfigForInit,
		saveConfig:         config.Save,
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
	if deps.modelMapPrompter == nil {
		deps.modelMapPrompter = defaults.modelMapPrompter
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
	cmd.Flags().StringVar(&flags.gitRef, "git-credential-ref", "", "Git credential ref")
	cmd.Flags().StringVar(&flags.reviewerRef, "reviewer-credential-ref", "", "Reviewer credential ref")
	cmd.Flags().StringVar(&flags.reviewerAuth, "reviewer-auth-mode", flags.reviewerAuth, "Reviewer credential auth mode")
	cmd.Flags().StringVar(&flags.llmProvider, "llm-provider", flags.llmProvider, "LLM provider")
	cmd.Flags().StringVar(&flags.llmAuth, "llm-auth", flags.llmAuth, "LLM auth mode")
	cmd.Flags().StringVar(&flags.llmAdapter, "llm-adapter", flags.llmAdapter, "LLM adapter")
	cmd.Flags().StringVar(&flags.llmRef, "llm-credential-ref", "", "LLM credential ref")
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
	if err := validateInteractiveInitFlags(flags); err != nil {
		return exitcode.Usage(err)
	}
	profileName := opts.Profile
	if profileName == "" {
		profileName = credstore.DefaultProfile
	}
	path, err := deps.configPath(opts)
	if err != nil {
		return exitcode.AuthConfig(err)
	}
	cfg, _, err := deps.loadConfig(path)
	if err != nil {
		return cmderr.Config(err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	existingProfileName := ""
	var existingProfile *config.Profile
	if profile, ok := cfg.Profiles[profileName]; ok {
		existingProfileName = profileName
		profileCopy := profile
		existingProfile = &profileCopy
	} else if opts.Profile == "" && cfg.DefaultProfile != "" {
		if profile, ok := cfg.Profiles[cfg.DefaultProfile]; ok {
			existingProfileName = cfg.DefaultProfile
			profileCopy := profile
			existingProfile = &profileCopy
			profileName = cfg.DefaultProfile
		}
	}
	prompter := deps.prompter
	if prompter == nil {
		prompter = newHuhInitPrompter(opts)
	}
	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: profileName,
		ExistingProfileName:  existingProfileName,
		ExistingProfile:      existingProfile,
		ExistingProfileNames: sortedProfileNames(cfg.Profiles),
		DefaultProfileName:   cfg.DefaultProfile,
		ExistingConfig:       cfg,
	})
	if err != nil {
		return err
	}
	plan, err := buildInteractiveInitPlan(cmd, opts, flags, deps, path, cfg, draft)
	if err != nil {
		return err
	}
	plan, err = collectInteractiveInitModelMap(opts, deps, plan)
	if err != nil {
		return err
	}
	plan, err = collectInteractiveInitSecrets(cmd, opts, deps, plan)
	if err != nil {
		return err
	}
	return applyInitPlan(opts, flags, deps, plan)
}

type huhInitPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

func newHuhInitPrompter(opts *root.Options) initPrompter {
	return huhInitPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

type huhInitSecretPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

type huhInitModelMapPrompter struct {
	stdin  io.Reader
	stderr io.Writer
}

func newHuhInitSecretPrompter(opts *root.Options) initSecretPrompter {
	return huhInitSecretPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func newHuhInitModelMapPrompter(opts *root.Options) initModelMapPrompter {
	return huhInitModelMapPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func (p huhInitPrompter) Run(ctx initPromptContext) (initDraft, error) {
	selectedProfileName := ctx.ExistingProfileName
	selectedExistingProfile := ctx.ExistingProfile
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
		} else {
			selectedProfileName = choice
			profile := ctx.ExistingConfig.Profiles[choice]
			profileCopy := profile
			selectedExistingProfile = &profileCopy
		}
	}

	draft := seedInteractiveInitDraft(ctx.RequestedProfileName, selectedProfileName, ctx.DefaultProfileName, selectedExistingProfile)
	reviewerGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Reviewer credential auth mode").
			Options(
				huh.NewOption("Personal access token", string(config.GitAuthModePAT)),
				huh.NewOption("GitHub App", string(config.GitAuthModeGitHubApp)),
			).
			Value(&draft.ReviewerAuth),
		huh.NewInput().
			Title("Reviewer credential ref").
			Description("Leave blank to use the standard profile-based ref.").
			Value(&draft.ReviewerCredentialRef).
			Validate(validateOptionalCredentialRef),
	).WithHideFunc(func() bool {
		return !draft.ReviewerEnabled
	})
	llmRefGroup := huh.NewGroup(
		huh.NewInput().
			Title("LLM credential ref").
			Description("Leave blank to use the standard profile-based ref. The secret itself is configured later.").
			Value(&draft.LLMCredentialRef).
			Validate(validateOptionalCredentialRef),
	).WithHideFunc(func() bool {
		return draft.LLMAuth != string(config.LLMAuthAPIKey)
	})
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
			huh.NewInput().
				Title("Git host").
				Value(&draft.GitHost).
				Validate(validateRequiredText("git host is required")),
			huh.NewSelect[string]().
				Title("Git auth mode").
				Options(
					huh.NewOption("Personal access token", string(config.GitAuthModePAT)),
					huh.NewOption("GitHub App", string(config.GitAuthModeGitHubApp)),
				).
				Value(&draft.GitAuth),
			huh.NewInput().
				Title("Git credential ref").
				Description("Leave blank to use the standard profile-based ref.").
				Value(&draft.GitCredentialRef).
				Validate(validateOptionalCredentialRef),
			huh.NewConfirm().
				Title("Configure separate reviewer credentials").
				Value(&draft.ReviewerEnabled),
		).Title("Git"),
		reviewerGroup.Title("Reviewer"),
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
			huh.NewSelect[string]().
				Title("Reviewer model tier").
				Options(
					huh.NewOption("Built-in default", ""),
					huh.NewOption("Small", string(config.ModelTierSmall)),
					huh.NewOption("Medium", string(config.ModelTierMedium)),
					huh.NewOption("Large", string(config.ModelTierLarge)),
				).
				Value(&draft.LLMReviewerModelTier),
		).Title("LLM"),
		llmRefGroup.Title("LLM Credential Ref"),
	).WithInput(p.stdin).WithOutput(p.stderr)
	if err := form.Run(); err != nil {
		return initDraft{}, err
	}
	return draft, nil
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

func validateInteractiveInitFlags(flags initOptions) error {
	if flags.overwrite {
		return fmt.Errorf("--overwrite is only supported with --non-interactive")
	}
	if flags.gitTokenStdin || flags.gitTokenEnv != "" || flags.reviewerTokenStdin || flags.reviewerTokenEnv != "" || flags.llmKeyStdin || flags.llmKeyEnv != "" {
		return fmt.Errorf("secret ingress flags are only supported with --non-interactive")
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
			AuthMode:      config.GitAuthModePAT,
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
	if reviewerRequested {
		profile.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      reviewerMode,
			CredentialRef: reviewerRef,
		}
	}
	cfg.Profiles[profileName] = profile
	if !exists {
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
	persistExplicitBackend := backendFlagSet && (len(writes) > 0 || profile.LLM.Auth == config.LLMAuthAPIKey)
	if persistExplicitBackend {
		if cfg.Keyring.Backend != "" && cfg.Keyring.Backend != opts.Backend {
			return initPlan{}, exitcode.Usage(fmt.Errorf("--backend %q conflicts with existing keyring.backend %q", opts.Backend, cfg.Keyring.Backend))
		}
		cfg.Keyring.Backend = opts.Backend
	}

	if err := config.Validate(cfg); err != nil {
		return initPlan{}, cmderr.Config(err)
	}

	backendArg := ""
	if backendFlagSet && !persistExplicitBackend {
		backendArg = fmt.Sprintf(" --backend %s", opts.Backend)
	}

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

func buildInteractiveInitPlan(cmd *cobra.Command, opts *root.Options, flags initOptions, _ initDeps, path string, cfg config.File, draft initDraft) (initPlan, error) {
	profileName := strings.TrimSpace(draft.ProfileName)
	if profileName == "" {
		return initPlan{}, exitcode.Usage(fmt.Errorf("profile name is required"))
	}
	working := cfg
	if working.Profiles == nil {
		working.Profiles = map[string]config.Profile{}
	}
	originalName := strings.TrimSpace(draft.OriginalProfileName)
	var previousProfile *config.Profile
	if originalName != "" {
		profile, ok := working.Profiles[originalName]
		if !ok {
			return initPlan{}, cmderr.Config(fmt.Errorf("%w: %s", config.ErrProfileNotFound, originalName))
		}
		profileCopy := profile
		previousProfile = &profileCopy
	}
	if originalName == "" {
		if _, exists := working.Profiles[profileName]; exists {
			return initPlan{}, exitcode.Usage(fmt.Errorf("profile %q already exists; select it to edit or choose a different name", profileName))
		}
	} else if profileName != originalName {
		if err := validateInteractiveRouteHostChange(working, originalName, previousProfile.Git.Host, draft.GitHost); err != nil {
			return initPlan{}, exitcode.Usage(err)
		}
		renamed, _, err := configedit.RenameProfile(working, originalName, profileName)
		if err != nil {
			if errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, configedit.ErrProfileExists) || errors.Is(err, configedit.ErrProfileNameRequired) {
				return initPlan{}, exitcode.Usage(err)
			}
			return initPlan{}, cmderr.Config(err)
		}
		working = renamed
	} else if previousProfile != nil {
		if err := validateInteractiveRouteHostChange(working, originalName, previousProfile.Git.Host, draft.GitHost); err != nil {
			return initPlan{}, exitcode.Usage(err)
		}
	}

	profile, err := synthesizeInteractiveProfile(flags, profileName, previousProfile, draft)
	if err != nil {
		return initPlan{}, err
	}
	working.Profiles[profileName] = profile
	if draft.MakeDefault || working.DefaultProfile == "" {
		var changed bool
		working, changed, err = configedit.SetDefaultProfile(working, profileName)
		_ = changed
		if err != nil {
			if errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, configedit.ErrProfileNameRequired) {
				return initPlan{}, exitcode.Usage(err)
			}
			return initPlan{}, cmderr.Config(err)
		}
	}
	if err := config.Validate(working); err != nil {
		return initPlan{}, cmderr.Config(err)
	}

	credentialPlan, err := planInitCredentials(previousProfile, profile, nil)
	if err != nil {
		return initPlan{}, cmderr.Config(err)
	}
	deferLLMSecret := profile.LLM.Auth == config.LLMAuthAPIKey
	backendFlagSet := cmderr.BackendFlagChanged(cmd)
	backendArg := ""
	if backendFlagSet {
		if _, err := credentials.StoreOptions(opts.Backend, true, working); err != nil {
			return initPlan{}, cmderr.Credential(err)
		}
		persistExplicitBackend := deferLLMSecret
		if persistExplicitBackend {
			if working.Keyring.Backend != "" && working.Keyring.Backend != opts.Backend {
				return initPlan{}, exitcode.Usage(fmt.Errorf("--backend %q conflicts with existing keyring.backend %q", opts.Backend, working.Keyring.Backend))
			}
			working.Keyring.Backend = opts.Backend
		} else {
			backendArg = fmt.Sprintf(" --backend %s", opts.Backend)
		}
	}
	return initPlan{
		path:             path,
		cfg:              working,
		previousProfile:  previousProfile,
		profileName:      profileName,
		profile:          profile,
		writes:           map[string]map[string]string{},
		credentialPlan:   credentialPlan,
		overwriteRefs:    map[string]bool{},
		satisfiedRefs:    map[string]bool{},
		backendFlagSet:   backendFlagSet,
		backendArg:       backendArg,
		allowDeferredLLM: deferLLMSecret,
		writeLLMHint:     deferLLMSecret,
	}, nil
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
		if previousProfile != nil && previousProfile.ReviewerCredentials != nil {
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

func validateInteractiveRouteHostChange(cfg config.File, profileName string, previousHost string, nextHost string) error {
	if config.NormalizeHost(previousHost) == config.NormalizeHost(nextHost) {
		return nil
	}
	for _, route := range cfg.RepositoryProfiles {
		if route.Profile != profileName {
			continue
		}
		return fmt.Errorf("profile %q has repository routes; changing git.host from %q to %q requires route reconciliation that init does not support yet", profileName, previousHost, nextHost)
	}
	return nil
}

func collectInteractiveInitModelMap(opts *root.Options, deps initDeps, plan initPlan) (initPlan, error) {
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
		return initPlan{}, err
	}
	if !edit.Apply {
		return plan, nil
	}
	nextProfile := plan.profile
	nextProfile.LLM.ModelMap = normalizeInitModelMap(edit.ModelMap)
	nextCfg := plan.cfg
	nextCfg.Profiles[plan.profileName] = nextProfile
	if err := config.Validate(nextCfg); err != nil {
		return initPlan{}, cmderr.Config(err)
	}
	plan.profile = nextProfile
	plan.cfg = nextCfg
	return plan, nil
}

func collectInteractiveInitSecrets(_ *cobra.Command, opts *root.Options, deps initDeps, plan initPlan) (initPlan, error) {
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
			return initPlan{}, err
		}
		if action == initCredentialSecretActionDefer {
			continue
		}
		activeStore, err := openStore()
		if err != nil {
			return initPlan{}, cmderr.Credential(err)
		}
		targetKeys, err := existingInitCredentialKeys(activeStore, entry.Ref.Ref)
		if err != nil {
			return initPlan{}, cmderr.Credential(err)
		}
		targetHasRequired := initCredentialKeysSatisfySpecs(targetKeys, entry.KeySpecs)
		targetHasAnyKeys := len(targetKeys) > 0
		if action == initCredentialSecretActionSetNow && targetHasAnyKeys {
			action, err = prompter.ChooseCredentialAction(initCredentialSecretPrompt{
				Entry:              entry,
				TargetHasRequired:  targetHasRequired,
				TargetHasAnyKeys:   targetHasAnyKeys,
				ClipboardSupported: deps.clipboardSupported(),
			})
			if err != nil {
				return initPlan{}, err
			}
		}
		if action == initCredentialSecretActionKeep {
			if !targetHasRequired {
				return initPlan{}, exitcode.Usage(fmt.Errorf("%s credential ref %q does not have all required keys", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
			}
			plan.satisfiedRefs[entry.Ref.Ref] = true
			continue
		}
		if action == initCredentialSecretActionDefer {
			continue
		}
		if action != initCredentialSecretActionSetNow {
			return initPlan{}, fmt.Errorf("unsupported interactive secret action %q", action)
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
				return initPlan{}, err
			}
			switch source {
			case initSecretSourceKeepExisting:
				if !targetHasKey {
					return initPlan{}, exitcode.Usage(fmt.Errorf("%s credential key %q does not exist for ref %q", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key, entry.Ref.Ref))
				}
				continue
			case initSecretSourceSkip:
				if spec.Required {
					return initPlan{}, exitcode.Usage(fmt.Errorf("%s credential key %q is required", initCredentialPurposeLabel(entry.Ref.Purpose), spec.Key))
				}
				continue
			case initSecretSourceClipboard:
				value, err := deps.clipboardRead()
				if err != nil {
					return initPlan{}, exitcode.Usage(fmt.Errorf("read clipboard: %w", err))
				}
				value = credentials.TrimSecretIngress(value)
				if value == "" {
					return initPlan{}, exitcode.Usage(fmt.Errorf("clipboard supplied an empty secret"))
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
					return initPlan{}, err
				}
				value = credentials.TrimSecretIngress(value)
				if value == "" {
					return initPlan{}, exitcode.Usage(fmt.Errorf("pasted secret for %q is empty", spec.Key))
				}
				addWrite(plan.writes, entry.Ref.Ref, spec.Key, value)
			default:
				return initPlan{}, fmt.Errorf("unsupported interactive secret source %q", source)
			}
			if targetHasKey {
				overwriteRef = true
			}
		}
		planned := plan.writes[entry.Ref.Ref]
		if !initCredentialWritePlanSatisfiesEntry(entry, targetKeys, planned) {
			return initPlan{}, exitcode.Usage(fmt.Errorf("%s credential ref %q still needs required keys; keep existing values or defer instead", initCredentialPurposeLabel(entry.Ref.Purpose), entry.Ref.Ref))
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

func initCredentialKeysSatisfySpecs(keys map[string]bool, specs []credentials.KeySpec) bool {
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		if !keys[spec.Key] {
			return false
		}
	}
	return true
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
		next.MissingRequiredKeys = missingRequiredInitCredentialKeys(next.KeySpecs, next.PlannedWriteKeys)
		next.State = classifyInitCredentialPlanEntry(next)
		refreshed = append(refreshed, next)
	}
	return refreshed
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
