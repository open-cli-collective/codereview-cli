// Command cr is the Open CLI Collective code-review CLI.
//
// This is currently a scaffold entry point: it reports its build version so the
// release machinery (goreleaser, the reusable release workflow) has a real
// binary to build and ship. The actual review command surface lands separately.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on the first argument and returns the process exit code. It
// takes its args and writers as parameters (rather than reading os.Args /
// os.Stdout directly) so it is testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	var arg string
	if len(args) > 0 {
		arg = args[0]
	}
	switch arg {
	// no "-v" alias: it conventionally means --verbose, so reserve it for the
	// real command surface rather than binding it to version now.
	case "--version", "version":
		fmt.Fprintf(stdout, "cr %s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return 0
	case "", "--help", "-h", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "cr: unknown command %q\n\n", arg)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `cr - Open CLI Collective code-review CLI (scaffold)

Usage:
  cr version    Print the build version
  cr help       Show this help

The review command surface is not yet implemented.
`)
}
