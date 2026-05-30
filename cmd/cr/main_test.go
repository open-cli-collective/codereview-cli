package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name                string
		args                []string
		wantCode            int
		wantStdout          string
		wantStdoutSubstring bool
		wantStderr          string
		wantEmptyStdout     bool
	}{
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "version subcommand", args: []string{"version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "no args shows usage", args: nil, wantCode: 0, wantStdout: "Usage:", wantStdoutSubstring: true},
		{name: "profile flag accepted", args: []string{"--profile", "work", "version"}, wantCode: 0, wantStdout: "cr " + version.Info() + "\n"},
		{name: "json deferred", args: []string{"--json"}, wantCode: 2, wantStderr: "unknown flag", wantEmptyStdout: true},
		{name: "verbose deferred", args: []string{"--verbose"}, wantCode: 2, wantStderr: "unknown flag", wantEmptyStdout: true},
		{name: "dash-v is not version", args: []string{"-v"}, wantCode: 2, wantStderr: "unknown shorthand flag", wantEmptyStdout: true},
		{name: "help flag shows usage", args: []string{"--help"}, wantCode: 0, wantStdout: "Usage:", wantStdoutSubstring: true},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 2, wantStderr: "unknown command", wantEmptyStdout: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if tt.wantStdout != "" && tt.wantStdoutSubstring && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("run(%q) stdout = %q, want substring %q", tt.args, stdout.String(), tt.wantStdout)
			}
			if tt.wantStdout != "" && !tt.wantStdoutSubstring && stdout.String() != tt.wantStdout {
				t.Errorf("run(%q) stdout = %q, want %q", tt.args, stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("run(%q) stderr = %q, want substring %q", tt.args, stderr.String(), tt.wantStderr)
			}
			if tt.wantEmptyStdout && stdout.String() != "" {
				t.Errorf("run(%q) stdout = %q, want empty on failure", tt.args, stdout.String())
			}
		})
	}
}

func TestRunConfigShowJSON(t *testing.T) {
	statedirtest.Hermetic(t)
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config Path: %v", err)
	}
	if err := config.Save(path, mainTestConfig()); err != nil {
		t.Fatalf("config Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"active_profile": "home"`) {
		t.Fatalf("stdout = %q, want active profile JSON", stdout.String())
	}
}

func mainTestConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
			},
		},
	}
}
