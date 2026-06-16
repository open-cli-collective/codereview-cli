package configcmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func newSecretsProfileCommand(opts *root.Options) *cobra.Command {
	secretsCmd := &cobra.Command{
		Use:   "secrets-profile",
		Short: "Inspect and update secrets-management profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List effective secrets-management profiles",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile list takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			_, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			result := view.ConfigSecretsProfiles{Profiles: configSecretsProfilesView(config.EffectiveSecretsProfiles(cfg))}
			if listJSON {
				return view.RenderConfigSecretsProfilesJSON(opts.Stdout, result)
			}
			return view.RenderConfigSecretsProfilesText(opts.Stdout, result)
		},
	}
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Emit JSON")

	var getJSON bool
	getCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show one effective secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile get requires <id>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			_, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			profile, err := effectiveSecretsProfileByID(cfg, args[0])
			if err != nil {
				return cmderr.Config(err)
			}
			result := configSecretsProfileView(profile)
			if getJSON {
				return view.RenderConfigSecretsProfileJSON(opts.Stdout, result)
			}
			return view.RenderConfigSecretsProfileText(opts.Stdout, result)
		},
	}
	getCmd.Flags().BoolVar(&getJSON, "json", false, "Emit JSON")

	var setBackend string
	var setLabel string
	var setBackendSet bool
	var setLabelSet bool
	var clearLabel bool
	setCmd := &cobra.Command{
		Use:   "set <id>",
		Short: "Create or update a secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile set requires <id>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			setBackendSet = cmd.Flags().Changed("backend")
			setLabelSet = cmd.Flags().Changed("label")
			path, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			var patch configedit.SecretsProfilePatch
			if setBackendSet {
				backend, err := credstore.ParseBackend(strings.TrimSpace(setBackend))
				if err != nil {
					return exitcode.Usage(err)
				}
				kind := config.SecretsBackendKind(backend)
				patch.Backend = &kind
			}
			if setLabelSet {
				value := setLabel
				patch.Label = &value
			}
			patch.ClearLabel = clearLabel
			nextCfg, changed, _, err := configedit.SetSecretsProfile(cfg, args[0], patch)
			if err != nil {
				return mapSecretsProfileMutationError(err)
			}
			if changed {
				if err := saveConfigFile(path, nextCfg); err != nil {
					return cmderr.Config(err)
				}
			}
			profile, err := effectiveSecretsProfileByID(nextCfg, args[0])
			if err != nil {
				return cmderr.Config(err)
			}
			return view.RenderConfigSecretsProfileText(opts.Stdout, configSecretsProfileView(profile))
		},
	}
	setCmd.Flags().StringVar(&setBackend, "backend", "", fmt.Sprintf("Secrets backend kind (%s)", strings.Join(credstore.ValidBackendNames(), ", ")))
	setCmd.Flags().StringVar(&setLabel, "label", "", "Human-friendly label for the secrets-management profile")
	setCmd.Flags().BoolVar(&clearLabel, "clear-label", false, "Clear the configured label")

	removeCmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove one configured secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile remove requires <id>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			path, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			nextCfg, changed, err := configedit.RemoveSecretsProfile(cfg, args[0])
			if err != nil {
				return mapSecretsProfileMutationError(err)
			}
			if changed {
				if err := saveConfigFile(path, nextCfg); err != nil {
					return cmderr.Config(err)
				}
			}
			return view.RenderConfigSecretsProfilesText(opts.Stdout, view.ConfigSecretsProfiles{Profiles: configSecretsProfilesView(config.EffectiveSecretsProfiles(nextCfg))})
		},
	}

	defaultCmd := &cobra.Command{
		Use:   "default",
		Short: "Inspect and update the default secrets-management profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var defaultJSON bool
	defaultGetCmd := &cobra.Command{
		Use:   "get",
		Short: "Show the effective default secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile default get takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			_, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			profile, ok := config.EffectiveDefaultSecretsProfile(cfg)
			if !ok {
				result := view.ConfigSecretsProfileDefault{}
				if defaultJSON {
					return view.RenderConfigSecretsProfileDefaultJSON(opts.Stdout, result)
				}
				return view.RenderConfigSecretsProfileDefaultText(opts.Stdout, result)
			}
			result := view.ConfigSecretsProfileDefault{DefaultProfile: configSecretsProfileViewPtr(profile)}
			if defaultJSON {
				return view.RenderConfigSecretsProfileDefaultJSON(opts.Stdout, result)
			}
			return view.RenderConfigSecretsProfileDefaultText(opts.Stdout, result)
		},
	}
	defaultGetCmd.Flags().BoolVar(&defaultJSON, "json", false, "Emit JSON")

	defaultSetCmd := &cobra.Command{
		Use:   "set <id>",
		Short: "Set the configured default secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile default set requires <id>"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			path, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			nextCfg, changed, err := configedit.SetDefaultSecretsProfile(cfg, args[0])
			if err != nil {
				return mapSecretsProfileMutationError(err)
			}
			if changed {
				if err := saveConfigFile(path, nextCfg); err != nil {
					return cmderr.Config(err)
				}
			}
			profile, ok := config.EffectiveDefaultSecretsProfile(nextCfg)
			if !ok {
				return cmderr.Config(fmt.Errorf("%w", config.ErrSecretsProfileNotFound))
			}
			return view.RenderConfigSecretsProfileText(opts.Stdout, configSecretsProfileView(profile))
		},
	}

	defaultUnsetCmd := &cobra.Command{
		Use:   "unset",
		Short: "Clear the configured default secrets-management profile",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config secrets-profile default unset takes no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			path, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			nextCfg, changed, err := configedit.UnsetDefaultSecretsProfile(cfg)
			if err != nil {
				return mapSecretsProfileMutationError(err)
			}
			if changed {
				if err := saveConfigFile(path, nextCfg); err != nil {
					return cmderr.Config(err)
				}
			}
			return view.RenderConfigSecretsProfilesText(opts.Stdout, view.ConfigSecretsProfiles{Profiles: configSecretsProfilesView(config.EffectiveSecretsProfiles(nextCfg))})
		},
	}

	defaultCmd.AddCommand(defaultGetCmd, defaultSetCmd, defaultUnsetCmd)
	secretsCmd.AddCommand(listCmd, getCmd, setCmd, removeCmd, defaultCmd)
	return secretsCmd
}

func configSecretsProfilesView(profiles []config.EffectiveSecretsProfile) []view.ConfigSecretsProfile {
	items := make([]view.ConfigSecretsProfile, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, configSecretsProfileView(profile))
	}
	return items
}

func configSecretsProfileView(profile config.EffectiveSecretsProfile) view.ConfigSecretsProfile {
	return view.ConfigSecretsProfile{
		ID:        profile.ID,
		Label:     profile.Label,
		Backend:   profile.Backend,
		IsDefault: profile.IsDefault,
		Source:    string(profile.Source),
	}
}

func configSecretsProfileViewPtr(profile config.EffectiveSecretsProfile) *view.ConfigSecretsProfile {
	result := configSecretsProfileView(profile)
	return &result
}

func effectiveSecretsProfileByID(cfg config.File, rawID string) (config.EffectiveSecretsProfile, error) {
	id := strings.TrimSpace(rawID)
	if id == "" {
		return config.EffectiveSecretsProfile{}, fmt.Errorf("%w", configedit.ErrSecretsProfileIDRequired)
	}
	for _, profile := range config.EffectiveSecretsProfiles(cfg) {
		if profile.ID == id {
			return profile, nil
		}
	}
	return config.EffectiveSecretsProfile{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, id)
}

func mapSecretsProfileMutationError(err error) error {
	switch {
	case errors.Is(err, configedit.ErrSecretsProfileIDRequired),
		errors.Is(err, configedit.ErrSecretsProfileReserved),
		errors.Is(err, configedit.ErrSecretsProfileBackendRequired),
		errors.Is(err, configedit.ErrSecretsProfileMutationRequired),
		errors.Is(err, configedit.ErrSecretsProfileLabelConflict),
		errors.Is(err, configedit.ErrSecretsProfileLabelRequired):
		return exitcode.Usage(err)
	case errors.Is(err, configedit.ErrSecretsProfileDefaultConfigured),
		errors.Is(err, config.ErrSecretsProfileNotFound),
		errors.Is(err, config.ErrInvalid):
		return cmderr.Config(err)
	default:
		return cmderr.Config(err)
	}
}
