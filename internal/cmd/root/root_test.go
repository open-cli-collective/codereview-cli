package root

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func TestNewCommandHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "no args", args: nil, wantOutput: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantOutput: "Usage:"},
		{name: "help command", args: []string{"help"}, wantOutput: "Usage:"},
		{name: "version flag", args: []string{"--version"}, wantOutput: "cr " + version.Info()},
		{name: "version command", args: []string{"version"}, wantOutput: "cr " + version.Info()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, opts := NewCommand()
			var out bytes.Buffer
			opts.Stdout = &out
			cmd.SetOut(&out)
			cmd.SetErr(&out)

			if err := Execute(cmd, tt.args); err != nil {
				t.Fatalf("Execute(%q): %v", tt.args, err)
			}
			if got := out.String(); !strings.Contains(got, tt.wantOutput) {
				t.Fatalf("Execute(%q) output = %q, want substring %q", tt.args, got, tt.wantOutput)
			}
		})
	}
}

func TestPersistentProfileFlagPopulatesOptions(t *testing.T) {
	cmd, opts := NewCommand()
	var out bytes.Buffer
	opts.Stdout = &out
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := Execute(cmd, []string{"--profile", "work", "version"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if opts.Profile != "work" {
		t.Fatalf("Profile = %q, want work", opts.Profile)
	}
}

func TestOutputShapeFlagsDeferred(t *testing.T) {
	tests := [][]string{
		{"--json"},
		{"--verbose"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd, _ := NewCommand()
			err := Execute(cmd, args)
			if err == nil {
				t.Fatalf("Execute(%q) error = nil, want usage error", args)
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
			}
		})
	}
}

func TestDashVIsNotVersion(t *testing.T) {
	cmd, _ := NewCommand()
	err := Execute(cmd, []string{"-v"})
	if err == nil {
		t.Fatal("Execute(-v) error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestRegisterAll(t *testing.T) {
	cmd, opts := NewCommand()
	var calls []string
	RegisterAll(cmd, opts,
		func(parent *cobra.Command, got *Options) {
			if parent != cmd || got != opts {
				t.Fatalf("registrar got (%p, %p), want (%p, %p)", parent, got, cmd, opts)
			}
			calls = append(calls, "one")
		},
		func(parent *cobra.Command, got *Options) {
			if parent != cmd || got != opts {
				t.Fatalf("registrar got (%p, %p), want (%p, %p)", parent, got, cmd, opts)
			}
			calls = append(calls, "two")
		},
	)

	if strings.Join(calls, ",") != "one,two" {
		t.Fatalf("calls = %v, want one,two", calls)
	}
}

func TestExecuteMapsCobraUsageErrors(t *testing.T) {
	tests := [][]string{
		{"bogus"},
		{"version", "extra"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd, _ := NewCommand()
			err := Execute(cmd, args)
			if err == nil {
				t.Fatalf("Execute(%q) error = nil, want error", args)
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
			}
		})
	}
}

func TestExecuteLeavesGenericCommandErrorsAsFailure(t *testing.T) {
	cmd, _ := NewCommand()
	cmd.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("handler failed")
		},
	})

	err := Execute(cmd, []string{"boom"})
	if err == nil {
		t.Fatal("Execute(boom) error = nil, want error")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want %d", got, exitcode.Failure)
	}
}
