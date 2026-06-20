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
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("unknown config command %q", args[0]))
			}
			return nil
		},
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
			resolvedSecretsProfile, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
			if err != nil {
				return cmderr.Config(err)
			}
			refs, err := config.CredentialRefs(profile)
			if err != nil {
				return cmderr.Config(err)
			}
			backendFlagSet := cmderr.BackendFlagChanged(cmd)
			store, err := credentials.OpenResolvedStore(opts.Backend, backendFlagSet, cfg, resolvedSecretsProfile)
			var storeErr error
			if err != nil {
				if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, config.ErrSecretsProfileNotFound) {
					return cmderr.Config(err)
				}
				storeErr = err
			}
			if store != nil {
				defer store.Close()
			}
			backend, source, err := backendMetadata(store, opts.Backend, backendFlagSet, cfg, resolvedSecretsProfile)
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
			show.ActiveSecretsProfile = resolvedSecretsProfileViewPtr(resolvedSecretsProfile)
			show.SecretsProfiles = config.EffectiveSecretsProfiles(cfg)
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
			cfg, _, err = configedit.SetDefaultProfile(cfg, profileName)
			if err != nil {
				return cmderr.Config(err)
			}
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			return view.RenderConfigDefaultText(opts.Stdout, view.ConfigDefault{DefaultProfile: profileName})
		},
	}
	defaultCmd.AddCommand(defaultGetCmd, defaultSetCmd)

	routeCmd := &cobra.Command{
		Use:   "route",
		Short: "Inspect and update repository profile routes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var routeListJSON bool
	routeListCmd := &cobra.Command{
		Use:   "list",
		Short: "List repository profile routes",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config route list takes no arguments"))
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
			result := view.ConfigRoutes{Routes: configRoutesView(configedit.CanonicalRepositoryRoutes(cfg.RepositoryProfiles))}
			if routeListJSON {
				return view.RenderConfigRoutesJSON(opts.Stdout, result)
			}
			return view.RenderConfigRoutesText(opts.Stdout, result)
		},
	}
	routeListCmd.Flags().BoolVar(&routeListJSON, "json", false, "Emit JSON")

	var routeSetHost string
	var routeSetNamespace string
	var routeSetRepos []string
	routeSetCmd := &cobra.Command{
		Use:   "set",
		Short: "Set repository profile routes on the selected profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config route set takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			spec, err := parseConfigRouteSpec(routeSetHost, routeSetNamespace, routeSetRepos)
			if err != nil {
				return err
			}
			if spec.Host != config.NormalizeHost(profile.Git.Host) {
				return exitcode.Usage(fmt.Errorf("--host %q does not match selected profile host %q", spec.Host, profile.Git.Host))
			}
			cfg.RepositoryProfiles, err = configedit.SetRepositoryRoutes(cfg.RepositoryProfiles, profileName, spec)
			if err != nil {
				return usageRouteError(err)
			}
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			_, err = fmt.Fprintf(opts.Stdout, "Set route for profile %s: %s\n", profileName, configedit.FormatRepositoryRouteSpec(spec))
			return err
		},
	}
	routeSetCmd.Flags().StringVar(&routeSetHost, "host", "", "Repository host")
	routeSetCmd.Flags().StringVar(&routeSetNamespace, "namespace", "", "Repository namespace")
	routeSetCmd.Flags().StringArrayVar(&routeSetRepos, "repo", nil, "Repository name; repeat for multiple repos")
	mustMarkFlagRequired(routeSetCmd, "host")
	mustMarkFlagRequired(routeSetCmd, "namespace")

	var routeUnsetHost string
	var routeUnsetNamespace string
	var routeUnsetRepos []string
	routeUnsetCmd := &cobra.Command{
		Use:   "unset",
		Short: "Remove repository profile routes",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config route unset takes no arguments"))
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
			spec, err := parseConfigRouteSpec(routeUnsetHost, routeUnsetNamespace, routeUnsetRepos)
			if err != nil {
				return err
			}
			routes, changed, err := configedit.UnsetRepositoryRoutes(cfg.RepositoryProfiles, spec)
			if err != nil {
				return usageRouteError(err)
			}
			if !changed {
				_, err := fmt.Fprintf(opts.Stdout, "Route already absent: %s\n", configedit.FormatRepositoryRouteSpec(spec))
				return err
			}
			cfg.RepositoryProfiles = routes
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			_, err = fmt.Fprintf(opts.Stdout, "Removed route: %s\n", configedit.FormatRepositoryRouteSpec(spec))
			return err
		},
	}
	routeUnsetCmd.Flags().StringVar(&routeUnsetHost, "host", "", "Repository host")
	routeUnsetCmd.Flags().StringVar(&routeUnsetNamespace, "namespace", "", "Repository namespace")
	routeUnsetCmd.Flags().StringArrayVar(&routeUnsetRepos, "repo", nil, "Repository name; repeat for multiple repos")
	mustMarkFlagRequired(routeUnsetCmd, "host")
	mustMarkFlagRequired(routeUnsetCmd, "namespace")

	routeCmd.AddCommand(routeListCmd, routeSetCmd, routeUnsetCmd)

	var resolveProfileJSON bool
	resolveProfileCmd := &cobra.Command{
		Use:   "resolve-profile <PR_URL>",
		Short: "Preview which profile a PR URL would resolve to",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config resolve-profile requires <PR_URL>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := prref.ParseGitHubPullURL(args[0])
			if err != nil {
				return exitcode.Usage(err)
			}
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			resolution, err := config.ResolveProfileForRepositoryWithSource(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
				Host:      ref.Host,
				Namespace: ref.Owner,
				Repo:      ref.Repo,
			})
			if err != nil {
				return cmderr.Config(err)
			}
			if !prref.SameHost(ref.Host, resolution.Profile.Git.Host) {
				return exitcode.Usage(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, resolution.Profile.Git.Host))
			}
			result := configResolveProfileView(args[0], resolution)
			if resolveProfileJSON {
				return view.RenderConfigResolveProfileJSON(opts.Stdout, result)
			}
			return view.RenderConfigResolveProfileText(opts.Stdout, result)
		},
	}
	resolveProfileCmd.Flags().BoolVar(&resolveProfileJSON, "json", false, "Emit JSON")

	agentSourceCmd := &cobra.Command{
		Use:   "agent-source",
		Short: "Inspect and update profile agent sources",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var agentSourceListJSON bool
	agentSourceListCmd := &cobra.Command{
		Use:   "list",
		Short: "List agent sources on the selected profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config agent-source list takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			result := configAgentSourcesView(profileName, profile.AgentSources)
			if agentSourceListJSON {
				return view.RenderConfigAgentSourcesJSON(opts.Stdout, result)
			}
			return view.RenderConfigAgentSourcesText(opts.Stdout, result)
		},
	}
	agentSourceListCmd.Flags().BoolVar(&agentSourceListJSON, "json", false, "Emit JSON")

	agentSourceAddCmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Add an agent source to the selected profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config agent-source add requires <path>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			sources, changed, err := configedit.AddAgentSource(profile.AgentSources, args[0])
			if err != nil {
				return usageAgentSourceError(err)
			}
			if changed {
				profile.AgentSources = sources
				cfg.Profiles[profileName] = profile
				if err := saveConfigFile(path, cfg); err != nil {
					return cmderr.Config(err)
				}
			}
			return view.RenderConfigAgentSourcesText(opts.Stdout, configAgentSourcesView(profileName, sources))
		},
	}

	agentSourceRemoveCmd := &cobra.Command{
		Use:   "remove <path>",
		Short: "Remove an agent source from the selected profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config agent-source remove requires <path>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			path, cfg, profileName, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			sources, changed, err := configedit.RemoveAgentSource(profile.AgentSources, args[0])
			if err != nil {
				return usageAgentSourceError(err)
			}
			if changed {
				profile.AgentSources = sources
				cfg.Profiles[profileName] = profile
				if err := saveConfigFile(path, cfg); err != nil {
					return cmderr.Config(err)
				}
			}
			return view.RenderConfigAgentSourcesText(opts.Stdout, configAgentSourcesView(profileName, sources))
		},
	}

	agentSourceCmd.AddCommand(agentSourceListCmd, agentSourceAddCmd, agentSourceRemoveCmd)

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
			resolvedSecretsProfile, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
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
			store, err := credentials.OpenResolvedStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, resolvedSecretsProfile)
			if err != nil {
				if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrProfileNotFound) || errors.Is(err, config.ErrSecretsProfileNotFound) {
					return cmderr.Config(err)
				}
				return cmderr.Credential(err)
			}
			defer store.Close()
			backend, source, err := backendMetadata(store, opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, resolvedSecretsProfile)
			if err != nil {
				return cmderr.Credential(err)
			}
			result := view.ConfigClear{
				Backend:              string(backend),
				BackendSource:        string(source),
				ActiveSecretsProfile: resolvedSecretsProfileViewPtr(resolvedSecretsProfile),
				DryRun:               clearDryRun,
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

	configCmd.AddCommand(showCmd, pathCmd, defaultCmd, routeCmd, resolveProfileCmd, agentSourceCmd, newSecretsProfileCommand(opts), newRetentionCommand(opts), clearCmd, newLLMCommand(opts))
	rootCmd.AddCommand(configCmd)
}

func newRetentionCommand(opts *root.Options) *cobra.Command {
	retentionCmd := &cobra.Command{
		Use:   "retention",
		Short: "Inspect and update data retention configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var getJSON bool
	getCmd := &cobra.Command{
		Use:   "get",
		Short: "Show data retention configuration",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config retention get takes no arguments"))
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
			result := view.NewConfigRetention(cfg.Data.Retention)
			if getJSON {
				return view.RenderConfigRetentionJSON(opts.Stdout, result)
			}
			return view.RenderConfigRetentionText(opts.Stdout, result)
		},
	}
	getCmd.Flags().BoolVar(&getJSON, "json", false, "Emit JSON")

	var setMaxAgeDays int
	var setEnforcement string
	setCmd := &cobra.Command{
		Use:   "set",
		Short: "Set data retention configuration",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config retention set takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			maxAgeChanged := cmd.Flags().Changed("max-age-days")
			enforcementChanged := cmd.Flags().Changed("enforcement")
			if !maxAgeChanged && !enforcementChanged {
				return exitcode.Usage(fmt.Errorf("config retention set requires --max-age-days or --enforcement"))
			}
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			retention := cfg.Data.Retention
			if maxAgeChanged {
				maxAgeDays, err := parseRetentionMaxAgeDays(setMaxAgeDays)
				if err != nil {
					return err
				}
				retention.MaxAgeDays = &maxAgeDays
			}
			if enforcementChanged {
				enforcement, err := parseRetentionEnforcement(setEnforcement)
				if err != nil {
					return err
				}
				retention.Enforcement = enforcement
			}
			if err := config.ValidateRetention(retention); err != nil {
				return cmderr.Config(err)
			}
			cfg.Data.Retention = retention
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			return view.RenderConfigRetentionText(opts.Stdout, view.NewConfigRetention(retention))
		},
	}
	setCmd.Flags().IntVar(&setMaxAgeDays, "max-age-days", 0, "Maximum run-data age in days; 0 keeps data forever")
	setCmd.Flags().StringVar(&setEnforcement, "enforcement", "", "Retention enforcement policy: at_write or manual_only")

	resetCmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset data retention to defaults",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config retention reset takes no arguments"))
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
			retention := config.DefaultRetentionConfig()
			cfg.Data.Retention = retention
			if err := saveConfigFile(path, cfg); err != nil {
				return cmderr.Config(err)
			}
			return view.RenderConfigRetentionText(opts.Stdout, view.NewConfigRetention(retention))
		},
	}

	retentionCmd.AddCommand(getCmd, setCmd, resetCmd)
	return retentionCmd
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

func configResolveProfileView(prURL string, resolution config.RepositoryProfileResolution) view.ConfigResolveProfile {
	result := view.ConfigResolveProfile{
		PRURL:           prURL,
		ResolvedProfile: resolution.ProfileName,
		Source:          string(resolution.Source),
		GitHost:         resolution.Profile.Git.Host,
	}
	if resolution.MatchedRoute != nil {
		route := &view.ConfigRoute{
			Profile:   resolution.MatchedRoute.Profile,
			Host:      resolution.MatchedRoute.Match.Host,
			Namespace: resolution.MatchedRoute.Match.Namespace,
		}
		if len(resolution.MatchedRoute.Match.Repos) > 0 {
			route.Repos = append([]string(nil), resolution.MatchedRoute.Match.Repos...)
		}
		result.MatchedRoute = route
	}
	return result
}

func configAgentSourcesView(profileName string, sources []string) view.ConfigAgentSources {
	result := view.ConfigAgentSources{
		ActiveProfile: profileName,
		AgentSources:  []string{},
	}
	if len(sources) > 0 {
		result.AgentSources = append([]string(nil), sources...)
	}
	return result
}

func loadConfig(opts *root.Options) (string, config.File, error) {
	path, err := configPath(opts)
	if err != nil {
		return "", config.File{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", config.File{}, cmderr.Config(err)
	}
	return path, cfg, nil
}

func loadActiveProfile(opts *root.Options) (string, config.File, string, config.Profile, error) {
	path, cfg, err := loadConfig(opts)
	if err != nil {
		return "", config.File{}, "", config.Profile{}, err
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

func parseRetentionMaxAgeDays(days int) (int, error) {
	if days < 0 {
		return 0, exitcode.Usage(fmt.Errorf("--max-age-days must be non-negative"))
	}
	return days, nil
}

func parseRetentionEnforcement(raw string) (config.RetentionEnforcement, error) {
	enforcement := config.RetentionEnforcement(strings.TrimSpace(raw))
	if !enforcement.Valid() {
		return "", exitcode.Usage(fmt.Errorf("--enforcement must be one of at_write, manual_only"))
	}
	return enforcement, nil
}

func parseConfigRouteSpec(rawHost string, rawNamespace string, rawRepos []string) (configedit.RepositoryRouteSpec, error) {
	spec, err := configedit.NormalizeRepositoryRouteSpec(configedit.RepositoryRouteSpec{
		Host:      rawHost,
		Namespace: rawNamespace,
		Repos:     rawRepos,
	})
	if err != nil {
		return configedit.RepositoryRouteSpec{}, usageRouteError(err)
	}
	return spec, nil
}

func usageRouteError(err error) error {
	switch {
	case errors.Is(err, configedit.ErrRouteHostRequired):
		return exitcode.Usage(fmt.Errorf("--host is required"))
	case errors.Is(err, configedit.ErrRouteNamespaceRequired):
		return exitcode.Usage(fmt.Errorf("--namespace is required"))
	case errors.Is(err, configedit.ErrRouteRepoRequired):
		return exitcode.Usage(fmt.Errorf("--repo must be non-empty"))
	default:
		return exitcode.Usage(err)
	}
}

func usageAgentSourceError(err error) error {
	if errors.Is(err, configedit.ErrAgentSourcePathRequired) {
		return exitcode.Usage(fmt.Errorf("path must be non-empty"))
	}
	return exitcode.Usage(err)
}

func configRoutesView(routes []config.RepositoryProfile) []view.ConfigRoute {
	out := make([]view.ConfigRoute, 0, len(routes))
	for _, route := range routes {
		item := view.ConfigRoute{
			Profile:   route.Profile,
			Host:      route.Match.Host,
			Namespace: route.Match.Namespace,
		}
		if len(route.Match.Repos) > 0 {
			item.Repos = append([]string(nil), route.Match.Repos...)
		}
		out = append(out, item)
	}
	return out
}

func mustMarkFlagRequired(cmd *cobra.Command, name string) {
	if err := cmd.MarkFlagRequired(name); err != nil {
		panic(err)
	}
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

func backendMetadata(store *credstore.Store, flagValue string, flagSet bool, cfg config.File, resolvedSecretsProfile credentials.ResolvedSecretsProfile) (credstore.Backend, credstore.Source, error) {
	if store != nil {
		backend, _ := store.Backend()
		return backend, credentials.BackendSourceCredentialStore, nil
	}
	if resolvedSecretsProfile.IsNamed() {
		backend, err := credstore.ParseBackend(resolvedSecretsProfile.Backend)
		if err != nil {
			return "", "", fmt.Errorf("%w: %w", credentials.ErrInvalidBackendSelection, err)
		}
		return backend, credentials.BackendSourceCredentialStore, nil
	}
	return credentials.BackendMetadata(flagValue, flagSet, cfg)
}

func credentialStatuses(store *credstore.Store, refs []config.CredentialRef, storeErr error) ([]view.CredentialStatus, error) {
	statuses, err := credentials.CredentialStatuses(store, refs, storeErr)
	if err != nil {
		return nil, err
	}
	viewStatuses := make([]view.CredentialStatus, 0, len(statuses))
	for _, status := range statuses {
		keys := make([]view.KeyStatus, 0, len(status.Keys))
		for _, key := range status.Keys {
			keys = append(keys, view.KeyStatus{
				Key:      key.Key,
				Required: key.Required,
				Present:  key.Present,
				Status:   key.Status,
				Error:    key.Error,
			})
		}
		viewStatuses = append(viewStatuses, view.CredentialStatus{
			Purpose: status.Purpose,
			Ref:     status.Ref,
			Mode:    status.Mode,
			Keys:    keys,
		})
	}
	return viewStatuses, nil
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
	cfg.RepositoryProfiles = configedit.PruneRepositoryProfileRoutes(cfg.RepositoryProfiles, profileName)
	if cfg.DefaultProfile == profileName {
		cfg.DefaultProfile = configedit.FirstProfileName(cfg.Profiles)
		change.defaultProfile = cfg.DefaultProfile
	}
	if err := saveConfigFile(path, cfg); err != nil {
		return configClearChange{}, err
	}
	return change, nil
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

func resolvedSecretsProfileViewPtr(resolved credentials.ResolvedSecretsProfile) *view.ConfigSecretsProfile {
	return &view.ConfigSecretsProfile{
		ID:       resolved.ID,
		Label:    resolved.Label,
		Backend:  resolved.Backend,
		ReadOnly: resolved.Source == config.EffectiveSecretsStoreSourceBuiltIn,
		Source:   string(resolved.Source),
	}
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
		change.defaultProfile = configedit.FirstProfileName(remaining)
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
