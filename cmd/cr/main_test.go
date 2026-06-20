package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
		{name: "data command wired", args: []string{"data", "--help"}, wantCode: 0, wantStdout: "Manage local review data", wantStdoutSubstring: true},
		{name: "benchmark command wired", args: []string{"benchmark", "--help"}, wantCode: 0, wantStdout: "Validate, inspect, and run benchmark suites", wantStdoutSubstring: true},
		{name: "benchmark select command wired", args: []string{"benchmark", "select", "--help"}, wantCode: 0, wantStdout: "Run selector-only benchmark suites", wantStdoutSubstring: true},
		{name: "benchmark run command wired", args: []string{"benchmark", "run", "--help"}, wantCode: 0, wantStdout: "Run a benchmark suite", wantStdoutSubstring: true},
		{name: "benchmark compare command wired", args: []string{"benchmark", "compare", "--help"}, wantCode: 0, wantStdout: "Compare benchmark results", wantStdoutSubstring: true},
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

func TestRunBenchmarkCommands(t *testing.T) {
	statedirtest.Hermetic(t)
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config Path: %v", err)
	}
	if err := config.Save(path, mainTestConfig()); err != nil {
		t.Fatalf("config Save: %v", err)
	}
	suitePath := filepath.Join(t.TempDir(), "suite.yml")
	if err := os.WriteFile(suitePath, []byte(`
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: claude-sonnet-4-6
        effort: high
      reviewers:
        model: claude-sonnet-4-6
        effort: high
        agent_dirs: []
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`), 0o600); err != nil {
		t.Fatalf("WriteFile suite: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"benchmark", "validate", suitePath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Benchmark suite \"suite1\" is valid") {
		t.Fatalf("validate stdout = %q, want success summary", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"benchmark", "doctor", suitePath, "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal doctor JSON: %v\n%s", err, stdout.String())
	}
	if decoded["suite_id"] != "suite1" {
		t.Fatalf("doctor JSON = %#v, want suite1", decoded)
	}
}

func TestRunCredentialCommandsDoNotLeakSecrets(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config Path: %v", err)
	}
	if err := config.Save(path, mainCredentialCommandTestConfig()); err != nil {
		t.Fatalf("config Save: %v", err)
	}
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
			name: "set initial credential",
			args: []string{
				"set-credential",
				"--store", "test-file",
				"--name", "codereview/default",
				"--key", "git_token",
				"--from-env", "CR_TOKEN_ONE",
			},
		},
		{
			name:     "config show",
			args:     []string{"config", "show", "--json"},
			wantCode: 0,
		},
		{
			name: "existing credential failure",
			args: []string{
				"set-credential",
				"--store", "test-file",
				"--name", "codereview/default",
				"--key", "git_token",
				"--from-env", "CR_TOKEN_TWO",
			},
			wantCode: 2,
		},
		{
			name: "set overwrite",
			args: []string{
				"set-credential",
				"--store", "test-file",
				"--name", "codereview/default",
				"--key", "git_token",
				"--from-env", "CR_TOKEN_TWO",
				"--overwrite",
			},
			wantCode: 0,
		},
		{
			name:     "config clear",
			args:     []string{"config", "clear", "--json"},
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
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:     "github.com",
					AuthMode: config.GitAuthModePAT,
					Credential: config.CredentialLocation{
						Store: config.LocalOSCredentialStoreID,
						Name:  "codereview/home",
					},
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

func mainCredentialCommandTestConfig() config.File {
	cfg := mainTestConfig()
	cfg.DefaultProfile = "default"
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"test-file": {
				DisplayName: "Test file store",
				Backend: config.SecretsStoreBackend{
					Kind: config.SecretsBackendKind("file"),
				},
			},
		},
	}
	profile := cfg.Profiles["home"]
	profile.Git.Credential = config.CredentialLocation{Store: "test-file", Name: "codereview/default"}
	cfg.Profiles = map[string]config.Profile{"default": profile}
	return cfg
}

func assertNoSecretLeak(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("output leaked secret %q: %q", secret, output)
		}
	}
}
