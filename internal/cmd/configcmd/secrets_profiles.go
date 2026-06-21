package configcmd

import (
	"fmt"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func newSecretsProfileCommand(opts *root.Options) *cobra.Command {
	secretsCmd := &cobra.Command{
		Use:   "credential-store",
		Short: "Inspect configured credential stores",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("unknown config credential-store command %q", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List effective credential stores",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("config credential-store list takes no arguments"))
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
		Short: "Show one effective credential store",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("config credential-store get requires <id>"))
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

	secretsCmd.AddCommand(listCmd, getCmd)
	return secretsCmd
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
		ID:       profile.ID,
		Label:    profile.Label,
		Backend:  profile.Backend,
		ReadOnly: profile.ReadOnly,
		Source:   string(profile.Source),
	}
	if configured, ok := configuredOnePasswordSecretsProfile(cfg, profile); ok {
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
	return result
}

func configuredOnePasswordSecretsProfile(cfg config.File, profile config.EffectiveSecretsProfile) (config.SecretsProfile, bool) {
	if profile.Source != config.EffectiveSecretsProfileSourceConfigured {
		return config.SecretsProfile{}, false
	}
	configured, ok := cfg.Secrets.Profiles[profile.ID]
	if !ok || configured.Backend.OnePassword == nil || !config.IsOnePasswordSecretsBackend(configured.Backend.Kind) {
		return config.SecretsProfile{}, false
	}
	return configured, true
}

func effectiveSecretsProfileByID(cfg config.File, rawID string) (config.EffectiveSecretsProfile, error) {
	id := strings.TrimSpace(rawID)
	if id == "" {
		return config.EffectiveSecretsProfile{}, fmt.Errorf("%w: credential store id is required", config.ErrInvalid)
	}
	for _, profile := range config.EffectiveSecretsProfiles(cfg) {
		if profile.ID == id {
			return profile, nil
		}
	}
	return config.EffectiveSecretsProfile{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, id)
}
