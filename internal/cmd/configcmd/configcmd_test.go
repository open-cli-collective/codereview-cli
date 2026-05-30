package configcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
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
	if got.Backend != "memory" || got.BackendSource != "config" {
		t.Fatalf("backend = (%q,%q), want (memory,config)", got.Backend, got.BackendSource)
	}
	if got.CredentialRef != "codereview/work" {
		t.Fatalf("credential_ref = %q, want codereview/work", got.CredentialRef)
	}
	if len(got.CredentialRefs) != 3 {
		t.Fatalf("credential_refs len = %d, want 3", len(got.CredentialRefs))
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

func TestConfigClearDefaultClearsActiveProfileOnlyAndPreservesData(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	dataFile := filepath.Join(os.Getenv("XDG_DATA_HOME"), "codereview", "runs", "sentinel.txt")
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o700); err != nil {
		t.Fatalf("MkdirAll data sentinel: %v", err)
	}
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.WriteFile(dataFile, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile data sentinel: %v", err)
	}
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.LLMAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "clear", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if len(got.Cleared) != 1 || got.Cleared[0].Ref != "codereview/home" {
		t.Fatalf("cleared = %#v, want active home only", got.Cleared)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work-llm", credentials.LLMAPIKeyKey)
	// #nosec G304,G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearAllDeletesEveryDeclaredRef(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.LLMAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "config" {
		t.Fatalf("backend = (%q,%q), want (file,config)", got.Backend, got.BackendSource)
	}
	if len(got.Cleared) != 4 {
		t.Fatalf("cleared = %#v, want 4 refs including empty reviewer ref", got.Cleared)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	assertFileBackendMissing(t, "work", credentials.GitTokenKey)
	assertFileBackendMissing(t, "work-llm", credentials.LLMAPIKeyKey)
}

func TestConfigClearProfileAllRejectedBeforeDelete(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
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

func fileBackendConfig(t *testing.T) config.File {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	return cfg
}

func seedFileBackend(t *testing.T, profile string, values map[string]string) {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if _, err := store.SetBundle(profile, values, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle(%s): %v", profile, err)
	}
}

func assertFileBackendPresent(t *testing.T, profile, key string) {
	t.Helper()
	if !fileBackendExists(t, profile, key) {
		t.Fatalf("file backend %s/%s missing, want present", profile, key)
	}
}

func assertFileBackendMissing(t *testing.T, profile, key string) {
	t.Helper()
	if fileBackendExists(t, profile, key) {
		t.Fatalf("file backend %s/%s present, want missing", profile, key)
	}
}

func fileBackendExists(t *testing.T, profile, key string) bool {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	present, err := store.Exists(profile, key)
	if err != nil {
		t.Fatalf("Exists(%s,%s): %v", profile, key, err)
	}
	return present
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
