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
		{name: "me command wired", args: []string{"me", "--help"}, wantCode: 0, wantStdout: "Resolve and cache", wantStdoutSubstring: true},
		{name: "agents command wired", args: []string{"agents", "--help"}, wantCode: 0, wantStdout: "Inspect trusted review agents", wantStdoutSubstring: true},
		{name: "review command wired", args: []string{"review", "--help"}, wantCode: 0, wantStdout: "Run an automated pull-request review", wantStdoutSubstring: true},
		{name: "sessions command wired", args: []string{"sessions", "--help"}, wantCode: 0, wantStdout: "Manage named LLM sessions", wantStdoutSubstring: true},
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

func TestRunCredentialCommandsDoNotLeakSecrets(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	const (
		firstSecret  = "distinctive-secret-one"
		secondSecret = "distinctive-secret-two"
	)
	t.Setenv("CR_TOKEN_ONE", firstSecret)
	t.Setenv("CR_TOKEN_TWO", secondSecret)

	runs := []struct {
		name     string
		args     []string
		stdin    string
		wantCode int
	}{
		{
			name: "init",
			args: []string{
				"--backend", "file",
				"init",
				"--non-interactive",
				"--git-token-from-env", "CR_TOKEN_ONE",
			},
		},
		{
			name:     "config show",
			args:     []string{"--backend", "file", "config", "show", "--json"},
			wantCode: 0,
		},
		{
			name: "existing credential failure",
			args: []string{
				"--backend", "file",
				"set-credential",
				"--ref", "codereview/default",
				"--key", "git_token",
				"--from-env", "CR_TOKEN_TWO",
			},
			wantCode: 2,
		},
		{
			name: "set overwrite",
			args: []string{
				"--backend", "file",
				"set-credential",
				"--ref", "codereview/default",
				"--key", "git_token",
				"--from-env", "CR_TOKEN_TWO",
				"--overwrite",
			},
			wantCode: 0,
		},
		{
			name:     "config clear",
			args:     []string{"--backend", "file", "config", "clear", "--json"},
			wantCode: 0,
		},
	}

	for _, runCase := range runs {
		t.Run(runCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(runCase.args, strings.NewReader(runCase.stdin), &stdout, &stderr)
			if code != runCase.wantCode {
				t.Fatalf("run exit code = %d, want %d; stderr=%q", code, runCase.wantCode, stderr.String())
			}
			assertNoSecretLeak(t, stdout.String(), firstSecret, secondSecret)
			assertNoSecretLeak(t, stderr.String(), firstSecret, secondSecret)
		})
	}
}

func mainTestConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
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

func assertNoSecretLeak(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("output leaked secret %q: %q", secret, output)
		}
	}
}
