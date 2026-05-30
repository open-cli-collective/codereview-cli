// Package credentialcmd wires credential ingress commands.
package credentialcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
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
	if err := credentials.ValidateAllowedKey(flags.key); err != nil {
		return result, exitcode.Usage(err)
	}
	secret, err := readSecretIngress(opts.Stdin, flags.stdin, flags.fromEnv, "--stdin", "--from-env")
	if err != nil {
		return result, exitcode.Usage(err)
	}
	cfg, err := loadOptionalConfig(opts)
	if err != nil {
		return result, cmderr.Config(err)
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
	nonInteractive bool
	gitHost        string
	gitRef         string
	llmProvider    string
	llmAuth        string
	llmAdapter     string
	llmRef         string
	agentSources   []string
	majorEvent     string
	selfApprove    bool
	resolveThreads string
	resolveAfter   string
	gitTokenStdin  bool
	gitTokenEnv    string
	llmKeyStdin    bool
	llmKeyEnv      string
	overwrite      bool
	replaceProfile bool
}

func newInitCommand(opts *root.Options) *cobra.Command {
	flags := initOptions{
		gitHost:     "github.com",
		llmProvider: string(config.LLMProviderAnthropic),
		llmAuth:     string(config.LLMAuthSubscription),
		llmAdapter:  string(config.LLMAdapterClaudeCLI),
		majorEvent:  string(config.ReviewMajorEventComment),
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
	cmd.Flags().BoolVar(&flags.llmKeyStdin, "llm-api-key-stdin", false, "Read the LLM API key from stdin")
	cmd.Flags().StringVar(&flags.llmKeyEnv, "llm-api-key-from-env", "", "Read the LLM API key from this environment variable")
	cmd.Flags().BoolVar(&flags.overwrite, "overwrite", false, "Replace existing keyring entries")
	cmd.Flags().BoolVar(&flags.replaceProfile, "replace-profile", false, "Replace an existing config profile")
	return cmd
}

func runInit(cmd *cobra.Command, opts *root.Options, flags initOptions) error {
	if !flags.nonInteractive {
		return exitcode.Usage(fmt.Errorf("init requires --non-interactive in v1"))
	}
	if flags.gitTokenStdin && flags.llmKeyStdin {
		return exitcode.Usage(fmt.Errorf("only one stdin secret ingress flag may be set"))
	}
	profileName := opts.Profile
	if profileName == "" {
		profileName = credstore.DefaultProfile
	}
	defaultGitRef, err := credentials.FormatRef(profileName)
	if err != nil {
		return exitcode.Usage(fmt.Errorf("profile %q cannot be used as a credential ref segment: %w", profileName, err))
	}
	gitRef := flags.gitRef
	if gitRef == "" {
		gitRef = defaultGitRef
	}
	if _, err := credentials.ParseRef(gitRef); err != nil {
		return exitcode.Usage(err)
	}
	llmRef := flags.llmRef
	if flags.llmAuth == string(config.LLMAuthAPIKey) && llmRef == "" {
		llmRef, err = credentials.FormatRef(profileName + "-llm")
		if err != nil {
			return exitcode.Usage(err)
		}
	}
	if llmRef != "" {
		if _, err := credentials.ParseRef(llmRef); err != nil {
			return exitcode.Usage(err)
		}
	}
	gitSecret, hasGitSecret, err := readOptionalSecretIngress(opts.Stdin, flags.gitTokenStdin, flags.gitTokenEnv, "--git-token-stdin", "--git-token-from-env")
	if err != nil {
		return exitcode.Usage(err)
	}
	llmSecret, hasLLMSecret, err := readOptionalSecretIngress(opts.Stdin, flags.llmKeyStdin, flags.llmKeyEnv, "--llm-api-key-stdin", "--llm-api-key-from-env")
	if err != nil {
		return exitcode.Usage(err)
	}
	if hasLLMSecret && flags.llmAuth != string(config.LLMAuthAPIKey) {
		return exitcode.Usage(fmt.Errorf("LLM API-key ingress requires --llm-auth %s", config.LLMAuthAPIKey))
	}

	path, err := configPath(opts)
	if err != nil {
		return exitcode.AuthConfig(err)
	}
	cfg, exists, err := loadConfigForInit(path)
	if err != nil {
		return cmderr.Config(err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	if _, ok := cfg.Profiles[profileName]; ok && !flags.replaceProfile {
		return exitcode.Usage(fmt.Errorf("profile %q already exists; use --replace-profile to replace config only", profileName))
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
	cfg.Profiles[profileName] = profile
	if !exists {
		cfg.DefaultProfile = profileName
	}

	writes := map[string]map[string]string{}
	if hasGitSecret {
		addWrite(writes, gitRef, credentials.GitTokenKey, gitSecret)
	}
	if hasLLMSecret {
		addWrite(writes, llmRef, credentials.LLMAPIKeyKey, llmSecret)
	}

	backendFlagSet := cmderr.BackendFlagChanged(cmd)
	if backendFlagSet {
		if _, err := credentials.StoreOptions(opts.Backend, true, cfg); err != nil {
			return cmderr.Credential(err)
		}
	}
	persistExplicitBackend := backendFlagSet && (len(writes) > 0 || profile.LLM.Auth == config.LLMAuthAPIKey)
	if persistExplicitBackend {
		if cfg.Keyring.Backend != "" && cfg.Keyring.Backend != opts.Backend {
			return exitcode.Usage(fmt.Errorf("--backend %q conflicts with existing keyring.backend %q", opts.Backend, cfg.Keyring.Backend))
		}
		cfg.Keyring.Backend = opts.Backend
	}

	if err := config.Validate(cfg); err != nil {
		return cmderr.Config(err)
	}

	var store *credstore.Store
	if len(writes) > 0 || profile.LLM.Auth == config.LLMAuthAPIKey {
		store, err = credentials.OpenStore(opts.Backend, backendFlagSet, cfg)
		if err != nil {
			return cmderr.Credential(err)
		}
		defer store.Close()
	}
	if profile.LLM.Auth == config.LLMAuthAPIKey && !hasLLMSecret {
		if flags.overwrite {
			return exitcode.Usage(fmt.Errorf("--overwrite with api_key LLM auth requires --llm-api-key-stdin or --llm-api-key-from-env"))
		}
		parsed, err := credentials.ParseRef(profile.LLM.CredentialRef)
		if err != nil {
			return exitcode.Usage(err)
		}
		present, err := store.Exists(parsed.Profile, credentials.LLMAPIKeyKey)
		if err != nil {
			return cmderr.Credential(err)
		}
		if !present {
			return exitcode.Usage(fmt.Errorf("api_key LLM auth requires --llm-api-key-stdin or --llm-api-key-from-env"))
		}
	}
	if store != nil && len(writes) > 0 && !flags.overwrite {
		if err := preflightNoOverwrite(store, writes); err != nil {
			return cmderr.Credential(err)
		}
	}
	if store != nil {
		setOpts := []credstore.SetOpt{}
		if flags.overwrite {
			setOpts = append(setOpts, credstore.WithOverwrite())
		}
		if _, err := writeBundles(store, writes, setOpts...); err != nil {
			return cmderr.Credential(err)
		}
	}
	if err := config.Save(path, cfg); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) {
			return cmderr.Config(err)
		}
		if len(writes) > 0 {
			return fmt.Errorf("init wrote credentials but failed to save config for profile %q; credential refs needing cleanup: %v: %w", profileName, sortedRefs(writes), err)
		}
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Initialized profile %s\n", profileName); err != nil {
		return err
	}
	if !hasGitSecret {
		_, err = fmt.Fprintf(opts.Stderr, "Next: cr set-credential --ref %s --key %s --stdin\n", gitRef, credentials.GitTokenKey)
	}
	return err
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

func preflightNoOverwrite(store *credstore.Store, writes map[string]map[string]string) error {
	for _, ref := range sortedRefs(writes) {
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

func writeBundles(store *credstore.Store, writes map[string]map[string]string, opts ...credstore.SetOpt) ([]string, error) {
	var writtenRefs []string
	for _, ref := range sortedRefs(writes) {
		parsed, err := credentials.ParseRef(ref)
		if err != nil {
			return writtenRefs, err
		}
		result, err := store.SetBundle(parsed.Profile, writes[ref], opts...)
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
