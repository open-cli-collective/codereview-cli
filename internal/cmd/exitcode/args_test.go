package exitcode

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestPositionalArgs(t *testing.T) {
	tests := []struct {
		name string
		args cobra.PositionalArgs
		good []string
		bad  []string
		want string
	}{
		{"no args", NoArgs("none"), nil, []string{"x"}, "none"},
		{"dynamic no args", NoArgsf("unknown %q"), nil, []string{"x"}, `unknown "x"`},
		{"exact args", ExactArgs(2, "two"), []string{"x", "y"}, []string{"x"}, "two"},
		{"non-empty arg", NonEmptyArg("non-empty"), []string{"x"}, []string{" "}, "non-empty"},
		{"maximum args", MaximumArgs(1, "at most one"), []string{"x"}, []string{"x", "y"}, "at most one"},
		{"range args", RangeArgs(1, 2, "one or two"), []string{"x"}, nil, "one or two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.args(nil, tt.good); err != nil {
				t.Fatalf("valid args: %v", err)
			}
			err := tt.args(nil, tt.bad)
			if err == nil || err.Error() != tt.want || FromError(err) != UsageError {
				t.Fatalf("invalid args = (%v, %d), want (%q, %d)", err, FromError(err), tt.want, UsageError)
			}
		})
	}
}
