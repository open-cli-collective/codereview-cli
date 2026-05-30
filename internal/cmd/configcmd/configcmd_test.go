package configcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestConfigShowText(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: home") {
		t.Fatalf("stdout = %q, want home profile", out.String())
	}
	if !strings.Contains(out.String(), "adapter-managed; not stored by cr") {
		t.Fatalf("stdout = %q, want adapter-managed LLM note", out.String())
	}
}

func TestConfigShowProfileFlag(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: work") {
		t.Fatalf("stdout = %q, want work profile", out.String())
	}
	if !strings.Contains(out.String(), "Credential ref: codereview/work-llm") {
		t.Fatalf("stdout = %q, want work LLM ref", out.String())
	}
}

func TestConfigShowProfileFlagLastValueWins(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--profile", "work"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: work") {
		t.Fatalf("stdout = %q, want command-position profile to win", out.String())
	}
}

func TestConfigShowJSON(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ActiveProfile != "work" {
		t.Fatalf("active_profile = %q, want work", got.ActiveProfile)
	}
	if got.LLMCredential.Ref != "codereview/work-llm" {
		t.Fatalf("llm credential = %#v, want work LLM ref", got.LLMCredential)
	}
}

func TestRootJSONFlagStillDeferred(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--json", "config", "show"})
	if err == nil {
		t.Fatal("Execute root --json error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowMissingConfigExitCode(t *testing.T) {
	cmd, _ := newTestCommand(filepath.Join(t.TempDir(), "missing.yml"))

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("Execute error = %v, want ErrNotConfigured", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestConfigShowMissingProfileExitCode(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show", "--profile", "missing"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestConfigShowInvalidEnumExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawConfig(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: nope
`)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Execute error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowReservedAuthModeExitCode(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.Git.AuthMode = config.GitAuthModeOAuthDevice
	cfg.Profiles["home"] = profile
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func newTestCommand(path string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	Register(cmd, opts)
	return cmd, &out
}

func saveTestConfig(t *testing.T, cfg config.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

func writeRawConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func testConfig() config.File {
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
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					CredentialRef: "codereview/work-llm",
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventRequestChanges},
			},
		},
		Data: config.DataConfig{
			Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(90),
				Enforcement: config.RetentionAtWrite,
			},
		},
	}
}

func intPtr(value int) *int {
	return &value
}
