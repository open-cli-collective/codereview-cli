// Package configcmd wires the `cr config` command surface.
package configcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

var (
	// Test seams. Tests that override these package-level functions must not run
	// in parallel with other configcmd tests.
	saveConfigFile   = config.Save
	removeConfigFile = os.Remove
	removeEmptyDir   = os.Remove
	removeCacheRoot  = os.RemoveAll
	resolveCacheRoot = statepaths.CacheRoot
)

// Register attaches config commands to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect cr configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var jsonOutput bool
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show the resolved cr profile configuration",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config show takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			profileName, profile, err := config.ResolveProfile(cfg, opts.Profile)
			if err != nil {
				return cmderr.Config(err)
			}
			refs, err := config.CredentialRefs(profile)
			if err != nil {
				return cmderr.Config(err)
			}
			backendFlagSet := cmderr.BackendFlagChanged(cmd)
			store, err := credentials.OpenStore(opts.Backend, backendFlagSet, cfg)
			var storeErr error
			if err != nil {
				storeErr = err
			}
			if store != nil {
				defer store.Close()
			}
			backend, source, err := backendMetadata(store, opts.Backend, backendFlagSet, cfg)
			if err != nil {
				return cmderr.Credential(err)
			}
			statuses, err := credentialStatuses(store, refs, storeErr)
			if err != nil {
				return cmderr.Credential(err)
			}
			show := view.NewConfigShow(profileName, profile, cfg.Data, statuses)
			show.Backend = string(backend)
			show.BackendSource = string(source)
			show.AgentSources = agents.InspectProfileSources(profile.AgentSources)
			if jsonOutput {
				return view.RenderConfigJSON(opts.Stdout, show)
			}
			return view.RenderConfigText(opts.Stdout, show)
		},
	}
	showCmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON")

	var pathJSON bool
	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Show the resolved cr config path",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config path takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			result := view.ConfigPath{
				ConfigPath: path,
				ConfigDir:  filepath.Dir(path),
			}
			if pathJSON {
				return view.RenderConfigPathJSON(opts.Stdout, result)
			}
			return view.RenderConfigPathText(opts.Stdout, result)
		},
	}
	pathCmd.Flags().BoolVar(&pathJSON, "json", false, "Emit JSON")

	defaultCmd := &cobra.Command{
		Use:   "default",
		Short: "Inspect and update the default profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var defaultGetJSON bool
	defaultGetCmd := &cobra.Command{
		Use:   "get",
		Short: "Show the configured default profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config default get takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			result := view.ConfigDefault{DefaultProfile: cfg.DefaultProfile}
			if defaultGetJSON {
				return view.RenderConfigDefaultJSON(opts.Stdout, result)
			}
			return view.RenderConfigDefaultText(opts.Stdout, result)
		},
	}
	defaultGetCmd.Flags().BoolVar(&defaultGetJSON, "json", false, "Emit JSON")

	defaultSetCmd := &cobra.Command{
		Use:   "set <profile>",
		Short: "Set the configured default profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config default set requires <profile>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			profileName := strings.TrimSpace(args[0])
			if profileName == "" {
				return exitcode.Usage(fmt.Errorf("profile must be non-empty"))
			}
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			if _, ok := cfg.Profiles[profileName]; !ok {
				return cmderr.Config(fmt.Errorf("%w: %s", config.ErrProfileNotFound, profileName))
			}
			cfg.DefaultProfile = profileName
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			return view.RenderConfigDefaultText(opts.Stdout, view.ConfigDefault{DefaultProfile: profileName})
		},
	}
	defaultCmd.AddCommand(defaultGetCmd, defaultSetCmd)

	var clearAll bool
	var clearJSON bool
	var clearDryRun bool
	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear stored credentials declared by the active profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config clear takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			profileName, profile, err := config.ResolveProfile(cfg, opts.Profile)
			if err != nil {
				return cmderr.Config(err)
			}
			refs, err := config.CredentialRefs(profile)
			if err != nil {
				return cmderr.Config(err)
			}
			profiles, err := distinctCredentialProfiles(refs)
			if err != nil {
				return cmderr.Credential(err)
			}
			store, err := credentials.OpenStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg)
			if err != nil {
				return cmderr.Credential(err)
			}
			defer store.Close()
			backend, source := store.Backend()
			result := view.ConfigClear{
				Backend:       string(backend),
				BackendSource: string(source),
				DryRun:        clearDryRun,
			}
			for _, profile := range profiles {
				keys, err := clearCredentialBundle(store, profile.Profile, clearDryRun)
				if err != nil {
					return cmderr.Credential(err)
				}
				result.Cleared = append(result.Cleared, view.ClearedCredentialRef{
					Ref:  profile.Full,
					Keys: keys,
				})
			}
			if clearAll {
				change, err := clearProfileFromConfig(path, cfg, profileName, clearDryRun)
				if err != nil {
					return fmt.Errorf("config clear --all credentials already cleared for profile %q (%s), but config reset failed: %w", profileName, credentialRefList(profiles), err)
				}
				result.ConfigProfileRemoved = change.profileRemoved
				result.DefaultProfile = change.defaultProfile
				result.ConfigPathRemoved = change.configPathRemoved

				cache, err := clearCacheRoot(clearDryRun)
				result.Cache = &cache
				if err != nil {
					cachePath := cache.Path
					if cachePath == "" {
						cachePath = "cache root"
					}
					cacheErr := fmt.Errorf("config clear --all cleared profile %q but cache cleanup failed for %s: %w", profileName, cachePath, err)
					if clearDryRun {
						cacheErr = fmt.Errorf("config clear --all --dry-run inspected profile %q but cache preview failed for %s: %w", profileName, cachePath, err)
					}
					if clearJSON {
						if renderErr := view.RenderConfigClearJSON(opts.Stdout, result); renderErr != nil {
							return renderErr
						}
					} else if renderErr := view.RenderConfigClearText(opts.Stdout, result); renderErr != nil {
						return renderErr
					}
					return cacheErr
				}
			}
			if clearJSON {
				return view.RenderConfigClearJSON(opts.Stdout, result)
			}
			return view.RenderConfigClearText(opts.Stdout, result)
		},
	}
	clearCmd.Flags().BoolVar(&clearAll, "all", false, "Also remove the active profile from config and clear disposable cache")
	clearCmd.Flags().BoolVar(&clearJSON, "json", false, "Emit JSON")
	clearCmd.Flags().BoolVar(&clearDryRun, "dry-run", false, "Report what would be cleared without deleting credentials, config, or cache")

	configCmd.AddCommand(showCmd, pathCmd, defaultCmd, clearCmd, newLLMCommand(opts))
	rootCmd.AddCommand(configCmd)
}

func newLLMCommand(opts *root.Options) *cobra.Command {
	llmCmd := &cobra.Command{
		Use:   "llm",
		Short: "Inspect and update LLM profile configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect and update LLM model tier mappings",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List effective model tier mappings",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config llm models list takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			result := modelMapResult(profileName, profile)
			if listJSON {
				return renderModelJSON(opts.Stdout, result)
			}
			return renderModelMapText(opts.Stdout, result)
		},
	}
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Emit JSON")

	setCmd := &cobra.Command{
		Use:   "set <tier> <model>",
		Short: "Set a model tier mapping on the active profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 {
				return exitcode.Usage(fmt.Errorf("config llm models set requires <tier> and <model>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			tier, err := parseModelTierArg(args[0])
			if err != nil {
				return err
			}
			model := strings.TrimSpace(args[1])
			if model == "" {
				return exitcode.Usage(fmt.Errorf("model must be non-empty"))
			}
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			if profile.LLM.ModelMap == nil {
				profile.LLM.ModelMap = config.ModelMap{}
			}
			profile.LLM.ModelMap[string(tier)] = model
			cfg.Profiles[profileName] = profile
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			_, err = fmt.Fprintf(opts.Stdout, "Set %s: %s\n", tier, model)
			return err
		},
	}

	unsetCmd := &cobra.Command{
		Use:   "unset <tier>",
		Short: "Remove a model tier mapping from the active profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config llm models unset requires <tier>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			tier, err := parseModelTierArg(args[0])
			if err != nil {
				return err
			}
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			if profile.LLM.ModelMap != nil {
				delete(profile.LLM.ModelMap, string(tier))
				if len(profile.LLM.ModelMap) == 0 {
					profile.LLM.ModelMap = nil
				}
			}
			cfg.Profiles[profileName] = profile
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			_, err = fmt.Fprintf(opts.Stdout, "Unset %s\n", tier)
			return err
		},
	}

	var resetProvider string
	resetCmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the active profile model map to built-in defaults",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config llm models reset takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			if guard := strings.TrimSpace(resetProvider); guard != "" {
				provider := config.LLMProvider(guard)
				if !provider.Valid() {
					return exitcode.Usage(fmt.Errorf("--provider %q is invalid", guard))
				}
				if provider != profile.LLM.Provider {
					return exitcode.Usage(fmt.Errorf("--provider %q does not match active profile provider %q", guard, profile.LLM.Provider))
				}
			}
			profile.LLM.ModelMap = nil
			cfg.Profiles[profileName] = profile
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			_, err = fmt.Fprintf(opts.Stdout, "Reset model map for profile %s\n", profileName)
			return err
		},
	}
	resetCmd.Flags().StringVar(&resetProvider, "provider", "", "Optional provider guard for the active profile")

	var resolveJSON bool
	resolveCmd := &cobra.Command{
		Use:   "resolve <tier>",
		Short: "Resolve a model tier under the active profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config llm models resolve requires <tier>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			tier, err := parseModelTierArg(args[0])
			if err != nil {
				return err
			}
			_, _, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			resolved, ok := config.ResolveModelTier(profile.LLM, tier)
			if !ok {
				return fmt.Errorf("model_tier %q is not mapped for provider %q adapter %q", tier, profile.LLM.Provider, profile.LLM.Adapter)
			}
			result := modelResolveResult{
				ActiveProfile: profileName,
				Provider:      string(profile.LLM.Provider),
				Adapter:       string(profile.LLM.Adapter),
				Tier:          string(tier),
				Model:         resolved.Model,
				Source:        string(resolved.Source),
			}
			if resolveJSON {
				return renderModelJSON(opts.Stdout, result)
			}
			return renderModelResolveText(opts.Stdout, result)
		},
	}
	resolveCmd.Flags().BoolVar(&resolveJSON, "json", false, "Emit JSON")

	modelsCmd.AddCommand(listCmd, setCmd, unsetCmd, resetCmd, resolveCmd)
	llmCmd.AddCommand(modelsCmd)
	return llmCmd
}

type modelMapResultView struct {
	ActiveProfile string        `json:"active_profile"`
	Provider      string        `json:"provider"`
	Adapter       string        `json:"adapter"`
	Models        []modelMapRow `json:"models"`
}

type modelMapRow struct {
	Tier   string `json:"tier"`
	Model  string `json:"model,omitempty"`
	Source string `json:"source"`
}

type modelResolveResult struct {
	ActiveProfile string `json:"active_profile"`
	Provider      string `json:"provider"`
	Adapter       string `json:"adapter"`
	Tier          string `json:"tier"`
	Model         string `json:"model"`
	Source        string `json:"source"`
}

func loadActiveProfile(opts *root.Options) (string, config.File, string, config.Profile, error) {
	path, err := configPath(opts)
	if err != nil {
		return "", config.File{}, "", config.Profile{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", config.File{}, "", config.Profile{}, cmderr.Config(err)
	}
	profileName, profile, err := config.ResolveProfile(cfg, opts.Profile)
	if err != nil {
		return "", config.File{}, "", config.Profile{}, cmderr.Config(err)
	}
	return path, cfg, profileName, profile, nil
}

func parseModelTierArg(raw string) (config.ModelTier, error) {
	tier := config.ModelTier(strings.TrimSpace(raw))
	if !tier.Valid() {
		return "", exitcode.Usage(fmt.Errorf("model tier must be one of small, medium, large"))
	}
	return tier, nil
}

func modelMapResult(profileName string, profile config.Profile) modelMapResultView {
	effective := config.EffectiveModelMap(profile.LLM)
	result := modelMapResultView{
		ActiveProfile: profileName,
		Provider:      string(profile.LLM.Provider),
		Adapter:       string(profile.LLM.Adapter),
	}
	for _, tier := range config.ModelTiers() {
		row := modelMapRow{Tier: string(tier), Source: "unset"}
		if resolved, ok := effective[tier]; ok {
			row.Model = resolved.Model
			row.Source = string(resolved.Source)
		}
		result.Models = append(result.Models, row)
	}
	return result
}

func renderModelJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func renderModelMapText(w io.Writer, result modelMapResultView) error {
	if _, err := fmt.Fprintf(w, "Profile: %s\n", result.ActiveProfile); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Provider: %s\n", result.Provider); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Adapter: %s\n", result.Adapter); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Model map:"); err != nil {
		return err
	}
	for _, row := range result.Models {
		model := row.Model
		if model == "" {
			model = "<unset>"
		}
		if _, err := fmt.Fprintf(w, "  %s: %s (%s)\n", row.Tier, model, row.Source); err != nil {
			return err
		}
	}
	return nil
}

func renderModelResolveText(w io.Writer, result modelResolveResult) error {
	if _, err := fmt.Fprintf(w, "Profile: %s\n", result.ActiveProfile); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Provider: %s\n", result.Provider); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Adapter: %s\n", result.Adapter); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Tier: %s\n", result.Tier); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Model: %s\n", result.Model); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "Source: %s\n", result.Source)
	return err
}

type configClearChange struct {
	profileRemoved    string
	defaultProfile    string
	configPathRemoved string
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func backendMetadata(store *credstore.Store, flagValue string, flagSet bool, cfg config.File) (credstore.Backend, credstore.Source, error) {
	if store != nil {
		backend, source := store.Backend()
		return backend, source, nil
	}
	return credentials.BackendMetadata(flagValue, flagSet, cfg)
}

func credentialStatuses(store *credstore.Store, refs []config.CredentialRef, storeErr error) ([]view.CredentialStatus, error) {
	statuses := make([]view.CredentialStatus, 0, len(refs))
	for _, ref := range refs {
		parsed, err := credentials.ParseRef(ref.Ref)
		if err != nil {
			return nil, err
		}
		specs, err := credentials.KeySpecsForPurpose(ref)
		if err != nil {
			return nil, err
		}
		keys := make([]view.KeyStatus, 0, len(specs))
		for _, spec := range specs {
			var present bool
			statusErr := storeErr
			if store != nil {
				present, statusErr = store.Exists(parsed.Profile, spec.Key)
			}
			keys = append(keys, keyStatus(spec.Key, spec.Required, present, statusErr))
		}
		statuses = append(statuses, view.CredentialStatus{
			Purpose: ref.Purpose,
			Ref:     ref.Ref,
			Mode:    ref.Mode,
			Keys:    keys,
		})
	}
	return statuses, nil
}

func keyStatus(key string, required bool, present bool, err error) view.KeyStatus {
	if err != nil {
		return view.KeyStatus{Key: key, Required: required, Status: "unknown", Error: err.Error()}
	}
	status := "missing"
	if present {
		status = "present"
	}
	return view.KeyStatus{Key: key, Required: required, Present: &present, Status: status}
}

func removeProfileFromConfig(path string, cfg config.File, profileName string) (configClearChange, error) {
	if _, ok := cfg.Profiles[profileName]; !ok {
		return configClearChange{}, fmt.Errorf("%w: %s", config.ErrProfileNotFound, profileName)
	}
	change := configClearChange{profileRemoved: profileName}
	delete(cfg.Profiles, profileName)
	if len(cfg.Profiles) == 0 {
		if err := removeConfigFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return configClearChange{}, err
		}
		removeEmptyConfigDir(filepath.Dir(path))
		change.configPathRemoved = path
		return change, nil
	}
	cfg.RepositoryProfiles = pruneRepositoryProfileRoutes(cfg.RepositoryProfiles, profileName)
	if cfg.DefaultProfile == profileName {
		cfg.DefaultProfile = firstProfileName(cfg.Profiles)
		change.defaultProfile = cfg.DefaultProfile
	}
	if err := saveConfigFile(path, cfg); err != nil {
		return configClearChange{}, err
	}
	return change, nil
}

func pruneRepositoryProfileRoutes(routes []config.RepositoryProfile, profileName string) []config.RepositoryProfile {
	if len(routes) == 0 {
		return routes
	}
	pruned := routes[:0]
	for _, route := range routes {
		if route.Profile != profileName {
			pruned = append(pruned, route)
		}
	}
	return pruned
}

func firstProfileName(profiles map[string]config.Profile) string {
	if len(profiles) == 0 {
		return ""
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
}

func removeEmptyConfigDir(dir string) {
	if filepath.Base(dir) != statepaths.AppDir {
		return
	}
	if err := removeEmptyDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

func clearCredentialBundle(store *credstore.Store, profile string, dryRun bool) ([]string, error) {
	if dryRun {
		return store.ListBundle(profile)
	}
	return store.DeleteBundle(profile)
}

func clearProfileFromConfig(path string, cfg config.File, profileName string, dryRun bool) (configClearChange, error) {
	if dryRun {
		return previewProfileFromConfig(path, cfg, profileName)
	}
	return removeProfileFromConfig(path, cfg, profileName)
}

func previewProfileFromConfig(path string, cfg config.File, profileName string) (configClearChange, error) {
	if _, ok := cfg.Profiles[profileName]; !ok {
		return configClearChange{}, fmt.Errorf("%w: %s", config.ErrProfileNotFound, profileName)
	}
	// Keep this preview in lockstep with removeProfileFromConfig so dry-run
	// reports exactly what the mutating path would change.
	change := configClearChange{profileRemoved: profileName}
	if len(cfg.Profiles) == 1 {
		change.configPathRemoved = path
		return change, nil
	}
	if cfg.DefaultProfile == profileName {
		remaining := make(map[string]config.Profile, len(cfg.Profiles)-1)
		for name, profile := range cfg.Profiles {
			if name != profileName {
				remaining[name] = profile
			}
		}
		change.defaultProfile = firstProfileName(remaining)
	}
	return change, nil
}

func clearCacheRoot(dryRun bool) (view.CacheClear, error) {
	path, err := resolveCacheRoot()
	if err != nil {
		return view.CacheClear{Status: "error", Error: err.Error()}, err
	}
	cache := view.CacheClear{Path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		cache.Status = "missing"
		return cache, nil
	} else if err != nil {
		cache.Status = "error"
		cache.Error = err.Error()
		return cache, err
	}
	if dryRun {
		cache.Status = "would_remove"
		return cache, nil
	}
	if err := removeCacheRoot(path); err != nil {
		cache.Status = "error"
		cache.Error = err.Error()
		return cache, err
	}
	cache.Status = "removed"
	return cache, nil
}

func distinctCredentialProfiles(refs []config.CredentialRef) ([]credentials.Ref, error) {
	seen := map[string]credentials.Ref{}
	for _, ref := range refs {
		parsed, err := credentials.ParseRef(ref.Ref)
		if err != nil {
			return nil, err
		}
		seen[parsed.Full] = parsed
	}
	names := make([]string, 0, len(seen))
	for ref := range seen {
		names = append(names, ref)
	}
	sort.Strings(names)
	out := make([]credentials.Ref, 0, len(names))
	for _, ref := range names {
		out = append(out, seen[ref])
	}
	return out, nil
}

func credentialRefList(refs []credentials.Ref) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Full)
	}
	return strings.Join(names, ", ")
}
