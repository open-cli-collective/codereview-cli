package exitcode

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func NoArgs(msg string) cobra.PositionalArgs {
	return ExactArgs(0, msg)
}

func NoArgsf(format string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 0 {
			return Usage(fmt.Errorf(format, args[0]))
		}
		return nil
	}
}

func ExactArgs(n int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

func NonEmptyArg(msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

func MaximumArgs(n int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > n {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}

func RangeArgs(minimum, maximum int, msg string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < minimum || len(args) > maximum {
			return Usage(fmt.Errorf("%s", msg))
		}
		return nil
	}
}
