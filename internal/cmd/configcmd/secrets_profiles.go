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
			result := configSecretsProfileView(cfg, profile)
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
	var opTimeout string
	var opVaultID string
	var opItemTitlePrefix string
	var opItemTag string
	var opItemFieldTitle string
	var opConnectHost string
	var opConnectTokenEnv string
	var opServiceTokenEnv string
	var opDesktopAccountID string
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
			existing := cfg.Secrets.Profiles[strings.TrimSpace(args[0])]
			if backendChanged(cmd) {
				nextBackend, err := buildSecretsProfileBackendPatch(cmd, existing, setBackendSet, setBackend)
				if err != nil {
					return exitcode.Usage(err)
				}
				patch.Backend = nextBackend
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
			return view.RenderConfigSecretsProfileText(opts.Stdout, configSecretsProfileView(nextCfg, profile))
		},
	}
	setCmd.Flags().StringVar(&setBackend, "backend", "", fmt.Sprintf("Secrets backend kind (%s)", strings.Join(credstore.ValidBackendNames(), ", ")))
	setCmd.Flags().StringVar(&setLabel, "label", "", "Human-friendly label for the secrets-management profile")
	setCmd.Flags().BoolVar(&clearLabel, "clear-label", false, "Clear the configured label")
	setCmd.Flags().StringVar(&opTimeout, "op-timeout", "", "1Password timeout (for example 5s)")
	setCmd.Flags().StringVar(&opVaultID, "op-vault-id", "", "1Password vault id")
	setCmd.Flags().StringVar(&opItemTitlePrefix, "op-item-title-prefix", "", "1Password item title prefix")
	setCmd.Flags().StringVar(&opItemTag, "op-item-tag", "", "1Password item tag")
	setCmd.Flags().StringVar(&opItemFieldTitle, "op-item-field-title", "", "1Password item field title")
	setCmd.Flags().StringVar(&opConnectHost, "op-connect-host", "", "1Password Connect host")
	setCmd.Flags().StringVar(&opConnectTokenEnv, "op-connect-token-env", "", "Environment variable holding the 1Password Connect token")
	setCmd.Flags().StringVar(&opServiceTokenEnv, "op-service-token-env", "", "Environment variable holding the 1Password service account token")
	setCmd.Flags().StringVar(&opDesktopAccountID, "op-desktop-account-id", "", "1Password desktop account id")

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
			result := view.ConfigSecretsProfileDefault{DefaultProfile: configSecretsProfileViewPtr(cfg, profile)}
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
			return view.RenderConfigSecretsProfileText(opts.Stdout, configSecretsProfileView(nextCfg, profile))
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

func backendChanged(cmd *cobra.Command) bool {
	for _, name := range []string{
		"backend",
		"op-timeout",
		"op-vault-id",
		"op-item-title-prefix",
		"op-item-tag",
		"op-item-field-title",
		"op-connect-host",
		"op-connect-token-env",
		"op-service-token-env",
		"op-desktop-account-id",
	} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func buildSecretsProfileBackendPatch(cmd *cobra.Command, existing config.SecretsProfile, backendSet bool, backendValue string) (*config.SecretsProfileBackend, error) {
	next := existing.Backend
	if backendSet {
		backend, err := credstore.ParseBackend(strings.TrimSpace(backendValue))
		if err != nil {
			return nil, err
		}
		next.Kind = config.SecretsBackendKind(backend)
	}
	if strings.TrimSpace(string(next.Kind)) == "" {
		return nil, configedit.ErrSecretsProfileBackendRequired
	}
	if err := validateSecretsProfileBackendFlags(cmd, next.Kind); err != nil {
		return nil, err
	}
	if !config.IsOnePasswordSecretsBackend(next.Kind) {
		next.OnePassword = nil
		return &next, nil
	}
	if next.OnePassword != nil {
		copyValue := *next.OnePassword
		next.OnePassword = &copyValue
	} else {
		next.OnePassword = &config.SecretsProfileOnePasswordConfig{}
	}
	if cmd.Flags().Changed("op-timeout") {
		next.OnePassword.Timeout, _ = cmd.Flags().GetString("op-timeout")
	}
	if cmd.Flags().Changed("op-vault-id") {
		next.OnePassword.VaultID, _ = cmd.Flags().GetString("op-vault-id")
	}
	if cmd.Flags().Changed("op-item-title-prefix") {
		next.OnePassword.ItemTitlePrefix, _ = cmd.Flags().GetString("op-item-title-prefix")
	}
	if cmd.Flags().Changed("op-item-tag") {
		next.OnePassword.ItemTag, _ = cmd.Flags().GetString("op-item-tag")
	}
	if cmd.Flags().Changed("op-item-field-title") {
		next.OnePassword.ItemFieldTitle, _ = cmd.Flags().GetString("op-item-field-title")
	}
	if cmd.Flags().Changed("op-connect-host") {
		next.OnePassword.ConnectHost, _ = cmd.Flags().GetString("op-connect-host")
	}
	if cmd.Flags().Changed("op-connect-token-env") {
		next.OnePassword.ConnectTokenEnv, _ = cmd.Flags().GetString("op-connect-token-env")
	}
	if cmd.Flags().Changed("op-service-token-env") {
		next.OnePassword.ServiceTokenEnv, _ = cmd.Flags().GetString("op-service-token-env")
	}
	if cmd.Flags().Changed("op-desktop-account-id") {
		next.OnePassword.DesktopAccountID, _ = cmd.Flags().GetString("op-desktop-account-id")
	}
	return &next, nil
}

func validateSecretsProfileBackendFlags(cmd *cobra.Command, kind config.SecretsBackendKind) error {
	changed := func(name string) bool { return cmd.Flags().Changed(name) }
	if !config.IsOnePasswordSecretsBackend(kind) {
		for _, name := range []string{
			"op-timeout",
			"op-vault-id",
			"op-item-title-prefix",
			"op-item-tag",
			"op-item-field-title",
			"op-connect-host",
			"op-connect-token-env",
			"op-service-token-env",
			"op-desktop-account-id",
		} {
			if changed(name) {
				return fmt.Errorf("--%s requires a 1Password backend", name)
			}
		}
		return nil
	}
	switch kind {
	case config.SecretsBackendKind(credstore.BackendOP):
		if changed("op-connect-host") || changed("op-connect-token-env") || changed("op-desktop-account-id") {
			return fmt.Errorf("op backend does not accept connect or desktop-specific flags")
		}
	case config.SecretsBackendKind(credstore.BackendOPConnect):
		if changed("op-service-token-env") || changed("op-desktop-account-id") {
			return fmt.Errorf("op-connect backend does not accept service-account or desktop-specific flags")
		}
	case config.SecretsBackendKind(credstore.BackendOPDesktop):
		if changed("op-connect-host") || changed("op-connect-token-env") || changed("op-service-token-env") {
			return fmt.Errorf("op-desktop backend does not accept service-account or connect-specific flags")
		}
	}
	return nil
}

func configSecretsProfilesView(profiles []config.EffectiveSecretsProfile) []view.ConfigSecretsProfile {
	items := make([]view.ConfigSecretsProfile, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, configSecretsProfileView(config.File{}, profile))
	}
	return items
}

func configSecretsProfileView(cfg config.File, profile config.EffectiveSecretsProfile) view.ConfigSecretsProfile {
	result := view.ConfigSecretsProfile{
		ID:        profile.ID,
		Label:     profile.Label,
		Backend:   profile.Backend,
		IsDefault: profile.IsDefault,
		Source:    string(profile.Source),
	}
	if profile.Source == config.EffectiveSecretsProfileSourceConfigured {
		if configured, ok := cfg.Secrets.Profiles[profile.ID]; ok && configured.Backend.OnePassword != nil && config.IsOnePasswordSecretsBackend(configured.Backend.Kind) {
			onePassword := &view.ConfigSecretsProfileOnePassword{
				Timeout:         configured.Backend.OnePassword.Timeout,
				VaultID:         configured.Backend.OnePassword.VaultID,
				ItemTitlePrefix: configured.Backend.OnePassword.ItemTitlePrefix,
				ItemTag:         configured.Backend.OnePassword.ItemTag,
				ItemFieldTitle:  configured.Backend.OnePassword.ItemFieldTitle,
			}
			switch configured.Backend.Kind {
			case config.SecretsBackendKind(credstore.BackendOP):
				onePassword.ServiceAccountTokenEnv = configured.Backend.OnePassword.ServiceTokenEnv
			case config.SecretsBackendKind(credstore.BackendOPConnect):
				onePassword.ConnectHost = configured.Backend.OnePassword.ConnectHost
				onePassword.ConnectTokenEnv = configured.Backend.OnePassword.ConnectTokenEnv
			case config.SecretsBackendKind(credstore.BackendOPDesktop):
				onePassword.DesktopAccountID = configured.Backend.OnePassword.DesktopAccountID
				onePassword.DesktopAccountEnv = credstore.DefaultOnePasswordDesktopAccountEnv
			}
			result.BackendInfo = &view.ConfigSecretsProfileBackendDetails{OnePassword: onePassword}
		}
	}
	return result
}

func configSecretsProfileViewPtr(cfg config.File, profile config.EffectiveSecretsProfile) *view.ConfigSecretsProfile {
	result := configSecretsProfileView(cfg, profile)
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
