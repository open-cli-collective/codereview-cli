// Package root builds the cr root command and owns root-level command wiring.
package root

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/version"
)

// RegisterFunc attaches a command subtree to the root command.
type RegisterFunc func(rootCmd *cobra.Command, opts *Options)

// Options carries root-level command dependencies and persistent options.
type Options struct {
	Profile string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// DefaultOptions returns root options wired to the process stdio streams.
func DefaultOptions() *Options {
	return &Options{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// NewCommand builds a fresh cr root command and its injectable options.
func NewCommand() (*cobra.Command, *Options) {
	opts := DefaultOptions()
	var showVersion bool

	cmd := &cobra.Command{
		Use:           "cr",
		Short:         "Open CLI Collective code-review CLI",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				_, err := fmt.Fprintf(opts.Stdout, "cr %s\n", version.Info())
				return err
			}
			return cmd.Help()
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitcode.Usage(err)
	})
	cmd.Flags().BoolVar(&showVersion, "version", false, "Print the build version")
	cmd.PersistentFlags().StringVar(&opts.Profile, "profile", "", "Profile name")
	cmd.AddCommand(newVersionCommand(opts))

	return cmd, opts
}

const rootLong = `cr is the Open CLI Collective code-review CLI.

The review command surface is not yet implemented.`

func newVersionCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Usage(fmt.Errorf("version takes no arguments"))
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintf(opts.Stdout, "cr %s\n", version.Info())
			return err
		},
	}
}

// Execute runs cmd with args and maps cobra parse errors into cr usage errors.
func Execute(cmd *cobra.Command, args []string) error {
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return nil
	}
	if exitcode.FromError(err) != exitcode.Failure {
		return err
	}
	if isCobraUnknownCommand(err) {
		return exitcode.Usage(err)
	}
	return err
}

func isCobraUnknownCommand(err error) bool {
	return strings.HasPrefix(err.Error(), "unknown command ")
}

// RegisterAll applies child-command registrars to a root command tree.
func RegisterAll(cmd *cobra.Command, opts *Options, fns ...RegisterFunc) {
	for _, fn := range fns {
		fn(cmd, opts)
	}
}
