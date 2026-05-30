// Package configcmd wires the `cr config` command surface.
package configcmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
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
		RunE: func(*cobra.Command, []string) error {
			path, err := configPath(opts)
			if err != nil {
				return exitcode.AuthConfig(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return mapConfigError(err)
			}
			profileName, profile, err := config.ResolveProfile(cfg, opts.Profile)
			if err != nil {
				return mapConfigError(err)
			}
			refs, err := config.CredentialRefs(profile)
			if err != nil {
				return mapConfigError(err)
			}
			show := view.NewConfigShow(profileName, profile, cfg.Data, refs)
			if jsonOutput {
				return view.RenderConfigJSON(opts.Stdout, show)
			}
			return view.RenderConfigText(opts.Stdout, show)
		},
	}
	showCmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON")

	configCmd.AddCommand(showCmd)
	rootCmd.AddCommand(configCmd)
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func mapConfigError(err error) error {
	switch {
	case errors.Is(err, config.ErrInvalid):
		return exitcode.Usage(err)
	case errors.Is(err, config.ErrNotConfigured), errors.Is(err, config.ErrProfileNotFound), errors.Is(err, config.ErrUnsupported):
		return exitcode.AuthConfig(err)
	default:
		return err
	}
}
