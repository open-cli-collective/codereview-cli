package exitcode

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// NoArgs rejects any positional arguments with the given usage message.
func NoArgs(msg string) cobra.PositionalArgs {
	return ExactArgs(0, msg)
}

// NoArgsf rejects any positional arguments, formatting the message with the first argument.
func NoArgsf(format string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 0 {
			return Usage(fmt.Errorf(format, args[0]))
		}
		return nil
	}
}

// ExactArgs requires exactly n positional arguments.
func ExactArgs(n int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

// NonEmptyArg requires a single non-empty positional argument.
func NonEmptyArg(msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

// MaximumArgs allows at most n positional arguments.
func MaximumArgs(n int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > n {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

// RangeArgs requires between minimum and maximum positional arguments inclusive.
func RangeArgs(minimum, maximum int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < minimum || len(args) > maximum {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}
