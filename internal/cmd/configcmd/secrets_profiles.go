package configcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func newSecretsStoreCommand(opts *root.Options) *cobra.Command {
	secretsCmd := &cobra.Command{
		Use:   "credential-store",
		Short: "Inspect configured credential stores",
		Args:  exitcode.NoArgsf("unknown config credential-store command %q"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List effective credential stores",
		Args:  exitcode.NoArgs("config credential-store list takes no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			_, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			result := view.ConfigSecretsStores{Stores: configSecretsStoresView(config.EffectiveSecretsStores(cfg))}
			return view.Render(opts.Stdout, listJSON, result, func(w io.Writer) error {
				return view.RenderConfigSecretsStoresText(w, result)
			})
		},
	}
	root.AddJSONFlag(listCmd, &listJSON)

	var getJSON bool
	getCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show one effective credential store",
		Args:  exitcode.ExactArgs(1, "config credential-store get requires <id>"),
		RunE: func(_ *cobra.Command, args []string) error {
			_, cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			profile, err := effectiveSecretsStoreByID(cfg, args[0])
			if err != nil {
				return cmderr.Config(err)
			}
			result := configSecretsStoreView(cfg, profile)
			return view.Render(opts.Stdout, getJSON, result, func(w io.Writer) error {
				return view.RenderConfigSecretsStoreText(w, result)
			})
		},
	}
	root.AddJSONFlag(getCmd, &getJSON)

	secretsCmd.AddCommand(listCmd, getCmd)
	return secretsCmd
}

func configSecretsStoresView(profiles []config.EffectiveSecretsStore) []view.ConfigSecretsStore {
	items := make([]view.ConfigSecretsStore, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, configSecretsStoreView(config.File{}, profile))
	}
	return items
}

func configSecretsStoreView(cfg config.File, profile config.EffectiveSecretsStore) view.ConfigSecretsStore {
	result := view.ConfigSecretsStore{
		ID:       profile.ID,
		Label:    profile.DisplayName,
		Backend:  profile.Backend,
		ReadOnly: profile.ReadOnly,
		Source:   string(profile.Source),
	}
	if configured, ok := configuredOnePasswordSecretsStore(cfg, profile); ok {
		onePassword := &view.ConfigSecretsStoreOnePassword{
			Timeout: configured.Backend.OnePassword.Timeout,
			VaultID: configured.Backend.OnePassword.VaultID,
		}
		switch configured.Backend.Kind {
		case config.SecretsBackendKind(credstore.BackendOP):
			onePassword.ServiceAccountTokenEnv = configured.Backend.OnePassword.ServiceTokenEnv
		case config.SecretsBackendKind(credstore.BackendOPConnect):
			onePassword.ConnectHost = configured.Backend.OnePassword.ConnectHost
			onePassword.ConnectTokenEnv = configured.Backend.OnePassword.ConnectTokenEnv
		case config.SecretsBackendKind(credstore.BackendOPDesktop):
			onePassword.DesktopAccountID = configured.Backend.OnePassword.AccountID
			onePassword.DesktopAccountEnv = credstore.DefaultOnePasswordDesktopAccountEnv
		}
		result.BackendInfo = &view.ConfigSecretsStoreBackendDetails{OnePassword: onePassword}
	}
	return result
}

func configuredOnePasswordSecretsStore(cfg config.File, profile config.EffectiveSecretsStore) (config.SecretsStore, bool) {
	if profile.Source != config.EffectiveSecretsStoreSourceConfigured {
		return config.SecretsStore{}, false
	}
	configured, ok := cfg.Secrets.Stores[profile.ID]
	if !ok || configured.Backend.OnePassword == nil || !config.IsOnePasswordSecretsBackend(configured.Backend.Kind) {
		return config.SecretsStore{}, false
	}
	return configured, true
}

func effectiveSecretsStoreByID(cfg config.File, rawID string) (config.EffectiveSecretsStore, error) {
	id := strings.TrimSpace(rawID)
	if id == "" {
		return config.EffectiveSecretsStore{}, fmt.Errorf("%w: credential store id is required", config.ErrInvalid)
	}
	for _, profile := range config.EffectiveSecretsStores(cfg) {
		if profile.ID == id {
			return profile, nil
		}
	}
	return config.EffectiveSecretsStore{}, fmt.Errorf("%w: %s", config.ErrSecretsStoreNotFound, id)
}
