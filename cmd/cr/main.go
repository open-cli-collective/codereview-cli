// Command cr is the Open CLI Collective code-review CLI.
//
// This is currently a scaffold entry point: it reports its build version so the
// release machinery (goreleaser, the reusable release workflow) has a real
// binary to build and ship. The actual review command surface lands separately.
package main

import (
	"fmt"
	"os"

	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func main() {
	switch arg := firstArg(); arg {
	case "--version", "-v", "version":
		fmt.Printf("cr %s (%s, %s)\n", version.Version, version.Commit, version.Date)
	case "", "--help", "-h", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "cr: unknown command %q\n\n", arg)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func firstArg() string {
	if len(os.Args) < 2 {
		return ""
	}
	return os.Args[1]
}

func usage(w *os.File) {
	fmt.Fprint(w, `cr - Open CLI Collective code-review CLI (scaffold)

Usage:
  cr version    Print the build version
  cr help       Show this help

The review command surface is not yet implemented.
`)
}
