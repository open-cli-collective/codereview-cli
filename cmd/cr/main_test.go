package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring expected on stdout
		wantStderr string // substring expected on stderr
	}{
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "version subcommand", args: []string{"version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "no args shows usage", args: nil, wantCode: 0, wantStdout: "Usage:"},
		{name: "profile flag accepted", args: []string{"--profile", "work", "version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "json deferred", args: []string{"--json"}, wantCode: 2, wantStderr: "unknown flag"},
		{name: "verbose deferred", args: []string{"--verbose"}, wantCode: 2, wantStderr: "unknown flag"},
		{name: "dash-v is not version", args: []string{"-v"}, wantCode: 2, wantStderr: "unknown shorthand flag"},
		{name: "help flag shows usage", args: []string{"--help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 2, wantStderr: "unknown command"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if tt.wantStdout != "" && tt.wantStdout == "Usage:" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("run(%q) stdout = %q, want substring %q", tt.args, stdout.String(), tt.wantStdout)
			}
			if tt.wantStdout != "" && tt.wantStdout != "Usage:" && stdout.String() != tt.wantStdout {
				t.Errorf("run(%q) stdout = %q, want %q", tt.args, stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("run(%q) stderr = %q, want substring %q", tt.args, stderr.String(), tt.wantStderr)
			}
			if tt.wantCode != 0 && stdout.String() != "" {
				t.Errorf("run(%q) stdout = %q, want empty on failure", tt.args, stdout.String())
			}
		})
	}
}
