// Package configcmd wires the `cr config` command surface.
package configcmd

import (
	"fmt"
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
			store, err := credentials.OpenStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg)
			if err != nil {
				return cmderr.Credential(err)
			}
			defer store.Close()
			backend, source := store.Backend()
			statuses, err := credentialStatuses(store, refs)
			if err != nil {
				return cmderr.Credential(err)
			}
			show := view.NewConfigShow(profileName, profile, cfg.Data, statuses)
			show.Backend = string(backend)
			show.BackendSource = string(source)
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
		Short: "Clear stored credentials declared by cr configuration",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config clear takes no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if clearAll && opts.Profile != "" {
				return exitcode.Usage(fmt.Errorf("config clear --all cannot be combined with --profile"))
			}
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return cmderr.Config(err)
			}
			refs, err := refsToClear(cfg, opts.Profile, clearAll)
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
			if clearJSON {
				return view.RenderConfigClearJSON(opts.Stdout, result)
			}
			return view.RenderConfigClearText(opts.Stdout, result)
		},
	}
	clearCmd.Flags().BoolVar(&clearAll, "all", false, "Clear every credential ref declared by every profile")
	clearCmd.Flags().BoolVar(&clearJSON, "json", false, "Emit JSON")

	configCmd.AddCommand(showCmd, clearCmd)
	rootCmd.AddCommand(configCmd)
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func credentialStatuses(store *credstore.Store, refs []config.CredentialRef) ([]view.CredentialStatus, error) {
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
		present, err := store.Exists(parsed.Profile, key)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, view.CredentialStatus{
			Purpose: ref.Purpose,
			Ref:     ref.Ref,
			Mode:    ref.Mode,
			Keys: []view.KeyStatus{{
				Key:     key,
				Present: present,
			}},
		})
	}
	return statuses, nil
}

func refsToClear(cfg config.File, profileName string, all bool) ([]config.CredentialRef, error) {
	if !all {
		_, profile, err := config.ResolveProfile(cfg, profileName)
		if err != nil {
			return nil, err
		}
		return config.CredentialRefs(profile)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	var refs []config.CredentialRef
	for _, name := range names {
		profile := cfg.Profiles[name]
		profileRefs, err := config.CredentialRefs(profile)
		if err != nil {
			return nil, err
		}
		refs = append(refs, profileRefs...)
	}
	return refs, nil
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
