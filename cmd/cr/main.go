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

	"github.com/open-cli-collective/codereview-cli/internal/cmd/agentscmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/mecmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/sessionscmd"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches on the first argument and returns the process exit code. It
// takes its args and stdio as parameters so it is testable without spawning a
// process.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd, _ := buildRootCommand(stdin, stdout, stderr)

	err := root.Execute(cmd, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitcode.FromError(err)
	}
	return exitcode.Success
}

func buildRootCommand(stdin io.Reader, stdout, stderr io.Writer) (*cobra.Command, *root.Options) {
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	root.RegisterAll(cmd, opts, configcmd.Register, credentialcmd.Register, mecmd.Register, agentscmd.Register, reviewcmd.Register, sessionscmd.Register)
	return cmd, opts
}
