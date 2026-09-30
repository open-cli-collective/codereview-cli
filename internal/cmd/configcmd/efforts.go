package configcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func newEffortsCommand(opts *root.Options) *cobra.Command {
	cmd := &cobra.Command{Use: "efforts", Short: "Inspect and update reasoning effort tier mappings"}
	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List configured reasoning efforts and ceilings",
		Args: exitcode.NoArgs("config llm efforts list takes no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _, name, profile, err := loadActiveProfile(opts)
			if err != nil {
				return err
			}
			result := struct {
				ActiveProfile string           `json:"active_profile"`
				EffortMap     config.EffortMap `json:"effort_map"`
				MaxEffort     config.EffortMap `json:"max_effort"`
			}{name, profile.LLM.EffortMap, profile.LLM.MaxEffort}
			return view.Render(opts.Stdout, asJSON, result, func(w io.Writer) error {
				for _, tier := range config.ModelTiers() {
					effort := profile.LLM.EffortMap[string(tier)]
					if effort == "" {
						effort = "agent/stage default"
					}
					ceiling := profile.LLM.MaxEffort[string(tier)]
					if ceiling == "" {
						ceiling = "uncapped"
					}
					if _, err := fmt.Fprintf(w, "%s: %s (max: %s)\n", tier, effort, ceiling); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	root.AddJSONFlag(list, &asJSON)
	set := &cobra.Command{
		Use: "set <tier> <effort>", Short: "Set reasoning effort independently of the model",
		Args: exitcode.ExactArgs(2, "config llm efforts set requires <tier> and <effort>"),
		RunE: func(_ *cobra.Command, args []string) error {
			tier, err := parseModelTierArg(args[0])
			if err != nil {
				return err
			}
			effort := strings.TrimSpace(args[1])
			_, err = mutateActiveLLM(opts, func(_ config.Profile, runtime *config.LLMConfig) error {
				if err := config.ValidateEffortForRuntime(*runtime, effort); err != nil {
					return exitcode.Usage(err)
				}
				if effort == "" {
					return exitcode.Usage(fmt.Errorf("effort must be non-empty"))
				}
				if runtime.EffortMap == nil {
					runtime.EffortMap = config.EffortMap{}
				}
				runtime.EffortMap[string(tier)] = effort
				return nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(opts.Stdout, "Set %s effort: %s\n", tier, effort)
			return err
		},
	}
	unset := &cobra.Command{
		Use: "unset <tier>", Short: "Restore agent or stage reasoning effort for a tier",
		Args: exitcode.ExactArgs(1, "config llm efforts unset requires <tier>"),
		RunE: func(_ *cobra.Command, args []string) error {
			tier, err := parseModelTierArg(args[0])
			if err != nil {
				return err
			}
			_, err = mutateActiveLLM(opts, func(_ config.Profile, runtime *config.LLMConfig) error {
				delete(runtime.EffortMap, string(tier))
				return nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(opts.Stdout, "Unset %s effort\n", tier)
			return err
		},
	}
	cmd.AddCommand(list, set, unset)
	return cmd
}
