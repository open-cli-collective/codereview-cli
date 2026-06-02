// Package configcmd wires the `cr config` command surface.
package configcmd

import (
	"errors"
	"fmt"
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

	var clearAll bool
	var clearJSON bool
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
			}
			for _, profile := range profiles {
				deleted, err := store.DeleteBundle(profile.Profile)
				if err != nil {
					return cmderr.Credential(err)
				}
				result.Cleared = append(result.Cleared, view.ClearedCredentialRef{
					Ref:  profile.Full,
					Keys: deleted,
				})
			}
			if clearAll {
				change, err := removeProfileFromConfig(path, cfg, profileName)
				if err != nil {
					return fmt.Errorf("config clear --all credentials already cleared for profile %q (%s), but config reset failed: %w", profileName, credentialRefList(profiles), err)
				}
				result.ConfigProfileRemoved = change.profileRemoved
				result.DefaultProfile = change.defaultProfile
				result.ConfigPathRemoved = change.configPathRemoved

				cache, err := clearCacheRoot()
				result.Cache = &cache
				if err != nil {
					cachePath := cache.Path
					if cachePath == "" {
						cachePath = "cache root"
					}
					cacheErr := fmt.Errorf("config clear --all cleared profile %q but cache cleanup failed for %s: %w", profileName, cachePath, err)
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

	configCmd.AddCommand(showCmd, clearCmd)
	rootCmd.AddCommand(configCmd)
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
		key, err := credentials.KeyForPurpose(ref)
		if err != nil {
			return nil, err
		}
		var present bool
		statusErr := storeErr
		if store != nil {
			present, statusErr = store.Exists(parsed.Profile, key)
		}
		statuses = append(statuses, view.CredentialStatus{
			Purpose: ref.Purpose,
			Ref:     ref.Ref,
			Mode:    ref.Mode,
			Keys:    []view.KeyStatus{keyStatus(key, present, statusErr)},
		})
	}
	return statuses, nil
}

func keyStatus(key string, present bool, err error) view.KeyStatus {
	if err != nil {
		return view.KeyStatus{Key: key, Status: "unknown", Error: err.Error()}
	}
	status := "missing"
	if present {
		status = "present"
	}
	return view.KeyStatus{Key: key, Present: &present, Status: status}
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
	if cfg.DefaultProfile == profileName {
		cfg.DefaultProfile = firstProfileName(cfg.Profiles)
		change.defaultProfile = cfg.DefaultProfile
	}
	if err := saveConfigFile(path, cfg); err != nil {
		return configClearChange{}, err
	}
	return change, nil
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

func clearCacheRoot() (view.CacheClear, error) {
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
