package configcmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func newPreflightCommand(opts *root.Options) *cobra.Command {
	preflight := &cobra.Command{Use: "preflight", Short: "Configure author preflight"}
	judge := &cobra.Command{Use: "judge", Short: "Manage the profile's optional prior-thread judge"}
	judge.AddCommand(&cobra.Command{
		Use:   "set <judge.yml>",
		Short: "Enable the judge for future preflights using this profile",
		Args:  exitcode.ExactArgs(1, "config preflight judge set requires <judge.yml>"),
		RunE: func(_ *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return exitcode.Usage(fmt.Errorf("judge config path is required"))
			}
			judgePath, err := filepath.Abs(args[0])
			if err != nil {
				return exitcode.Usage(fmt.Errorf("resolve judge config path: %w", err))
			}
			endpoint, model, err := app.InspectPriorJudgeConfig(judgePath)
			if err != nil {
				return cmderr.Config(fmt.Errorf("judge config %q: %w", judgePath, err))
			}
			profileName, err := savePreflightJudgePath(opts, judgePath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(opts.Stdout, "Enabled preflight judging for profile %q using %q. Future preflights using this profile may send eligible finding and thread text to %q (model %q).\n",
				profileName, judgePath, endpoint, model)
			return err
		},
	}, &cobra.Command{
		Use:   "unset",
		Short: "Remove the saved judge from this profile",
		Args:  exitcode.NoArgs("config preflight judge unset takes no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			profileName, err := savePreflightJudgePath(opts, "")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(opts.Stdout, "Profile %q has no saved preflight judge.\n", profileName)
			return err
		},
	})
	preflight.AddCommand(judge)
	return preflight
}

func savePreflightJudgePath(opts *root.Options, judgePath string) (string, error) {
	path, cfg, name, profile, err := loadActiveProfile(opts)
	if err != nil {
		return "", err
	}
	if profile.Preflight.JudgeConfigPath != judgePath {
		profile.Preflight = config.PreflightConfig{JudgeConfigPath: judgePath}
		cfg.Profiles[name] = profile
		if err := saveConfigFile(path, cfg); err != nil {
			return "", cmderr.Config(err)
		}
	}
	return name, nil
}
