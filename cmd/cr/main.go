// Command cr is the Open CLI Collective code-review CLI.
//
// This entry point stays thin: command construction and exit-code mapping live
// under internal/cmd so tests can exercise the same tree main uses.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on the first argument and returns the process exit code. It
// takes its args and writers as parameters (rather than reading os.Args /
// os.Stdout directly) so it is testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	cmd, opts := buildRootCommand()
	opts.Stdout = stdout
	opts.Stderr = stderr
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	err := root.Execute(cmd, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitcode.FromError(err)
	}
	return exitcode.Success
}

func buildRootCommand() (*cobra.Command, *root.Options) {
	return root.NewCommand()
}
