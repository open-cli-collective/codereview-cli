// Package pireviewtoolcmd wires the hidden Pi reviewer tool subprocess.
package pireviewtoolcmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/pireviewtool"
)

// Register adds the internal helper command used only by the generated Pi
// reviewer extension.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	var configPath string
	cmd := &cobra.Command{
		Use:    "__pi-review-tool",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if code := pireviewtool.Run(cmd.Context(), []string{"--config", configPath}, opts.Stdin, opts.Stdout, opts.Stderr); code != 0 {
				return errors.New("pi reviewer tool failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Internal reviewer tool configuration")
	rootCmd.AddCommand(cmd)
}
