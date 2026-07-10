// Package root builds the cr root command and owns root-level command wiring.
package root

import (
	"fmt"
	"io"
	"os"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/version"
)

// RegisterFunc attaches a command subtree to the root command.
type RegisterFunc func(rootCmd *cobra.Command, opts *Options)

// Options carries root-level command dependencies and persistent options.
type Options struct {
	Profile    string
	Backend    string
	ConfigPath string
	Quiet      bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

const profileFlagName = "profile"

// DefaultOptions returns root options wired to the process stdio streams.
func DefaultOptions() *Options {
	return &Options{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// NewCommandWithOptions builds a fresh cr root command using caller-provided
// options and wires the command output streams from those options.
func NewCommandWithOptions(opts *Options) (*cobra.Command, *Options) {
	if opts == nil {
		opts = DefaultOptions()
	}
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
	cmd.SetIn(opts.Stdin)
	cmd.SetOut(opts.Stdout)
	cmd.SetErr(opts.Stderr)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitcode.Usage(err)
	})
	cmd.Flags().BoolVar(&showVersion, "version", false, "Print the build version")
	cmd.PersistentFlags().StringVar(&opts.Profile, profileFlagName, "", "Profile name")
	cmd.PersistentFlags().StringVar(&opts.Backend, credstore.BackendFlagName, "", credstore.BackendFlagUsage())
	cmd.PersistentFlags().BoolVar(&opts.Quiet, "quiet", opts.Quiet, "Suppress progress logs")
	cmd.AddCommand(newVersionCommand(opts))

	return cmd, opts
}

// ProfileFlagChanged reports whether the inherited --profile flag was supplied.
func ProfileFlagChanged(cmd *cobra.Command) bool {
	flag := cmd.Flag(profileFlagName)
	return flag != nil && flag.Changed
}

// AddJSONFlag adds the shared --json output flag to cmd.
func AddJSONFlag(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVar(target, "json", false, "Emit JSON")
}

// NewProgressLogger returns a progress logger configured from root options.
func NewProgressLogger(opts *Options) *progress.Logger {
	if opts == nil {
		return progress.New(nil, true, nil)
	}
	return progress.New(opts.Stderr, opts.Quiet, nil)
}

const rootLong = `cr is the Open CLI Collective code-review CLI.

Use cr review --dry-run to plan pull-request review actions without posting.`

func newVersionCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  exitcode.NoArgs("version takes no arguments"),
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintf(opts.Stdout, "cr %s\n", version.Info())
			return err
		},
	}
}

// Execute runs cmd with args and maps unknown commands into cr usage errors.
func Execute(cmd *cobra.Command, args []string) error {
	cmd.SetArgs(args)
	if isHelpCommand(args) && len(args) > 1 {
		if _, _, err := cmd.Find(args[1:]); err != nil {
			return exitcode.Usage(err)
		}
	} else if !isHelpCommand(args) {
		if _, _, err := cmd.Find(args); err != nil {
			return exitcode.Usage(err)
		}
	}
	err := cmd.Execute()
	if err == nil {
		return nil
	}
	return err
}

func isHelpCommand(args []string) bool {
	return len(args) > 0 && args[0] == "help"
}

// RegisterAll applies child-command registrars to a root command tree.
func RegisterAll(cmd *cobra.Command, opts *Options, fns ...RegisterFunc) {
	for _, fn := range fns {
		fn(cmd, opts)
	}
}
