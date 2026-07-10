// Package cmdtest provides shared command test setup.
package cmdtest

import (
	"bytes"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
)

// New builds a root command with captured stdout and stderr, then registers commands.
func New(opts *root.Options, register func(*cobra.Command, *root.Options)) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	if opts == nil {
		opts = &root.Options{}
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	opts.Stdout = stdout
	opts.Stderr = stderr
	cmd, rootOpts := root.NewCommandWithOptions(opts)
	register(cmd, rootOpts)
	return cmd, stdout, stderr
}
