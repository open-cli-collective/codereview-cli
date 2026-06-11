package credentialcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestSetCredentialStdinJSONWritesFileBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	hermeticFileBackend(t)
	cmd, out, _ := newTestCommand(path, strings.NewReader("distinctive-token\n"))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"set-credential",
		"--ref", "codereview/work",
		"--key", credentials.GitTokenKey,
		"--stdin",
		"--json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "distinctive-token") {
		t.Fatalf("stdout leaked secret: %q", out.String())
	}
	var got view.CredentialWrite
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Ref != "codereview/work" || got.Key != credentials.GitTokenKey || !got.Written {
		t.Fatalf("credential write JSON = %#v, want written git token", got)
	}
	if got.Backend != "file" || got.BackendSource != "explicit" {
		t.Fatalf("backend JSON = (%q,%q), want (file,explicit)", got.Backend, got.BackendSource)
	}
	assertStored(t, "work", credentials.GitTokenKey, "distinctive-token")
}

func TestSetCredentialRejectsLiteralIngress(t *testing.T) {
	cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"set-credential",
		"--ref", "codereview/work",
		"--key", credentials.GitTokenKey,
		"--value=distinctive-token",
	})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
	if strings.Contains(err.Error(), "distinctive-token") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestSetCredentialRejectsDisallowedKeysBeforeIngress(t *testing.T) {
	for _, key := range []string{
		credentials.LegacyLLMAPIKeyKey,
		"git_app_private_key",
		"git_oauth_access_token",
		"git_oauth_refresh_token",
	} {
		t.Run(key, func(t *testing.T) {
			cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), failReader{})

			err := root.Execute(cmd, []string{
				"--backend", "memory",
				"set-credential",
				"--ref", "codereview/work",
				"--key", key,
				"--stdin",
			})
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
			}
			if strings.Contains(err.Error(), "secret ingress was read") {
				t.Fatalf("set-credential read secret ingress before rejecting key: %v", err)
			}
		})
	}
}

func TestSetCredentialUsesConfigCredentialMatrix(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Keyring:        config.KeyringConfig{Backend: "file"},
		Profiles: map[string]config.Profile{
			"work": apiKeyProfile("work", config.LLMProviderAnthropic),
		},
	})

	cmd, _, _ := newTestCommand(path, failReader{})
	err := root.Execute(cmd, []string{
		"--backend", "file",
		"set-credential",
		"--ref", "codereview/work-llm",
		"--key", credentials.OpenAIAPIKeyKey,
		"--stdin",
	})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("wrong provider exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if strings.Contains(err.Error(), "secret ingress was read") {
		t.Fatalf("set-credential read secret ingress before rejecting wrong provider key: %v", err)
	}
	assertFileBundleEmpty(t, "work-llm")

	cmd, _, _ = newTestCommand(path, strings.NewReader("openai-token"))
	err = root.Execute(cmd, []string{
		"--backend", "file",
		"set-credential",
		"--ref", "codereview/ad-hoc-openai",
		"--key", credentials.OpenAIAPIKeyKey,
		"--stdin",
	})
	if err != nil {
		t.Fatalf("unknown ref global key Execute: %v", err)
	}
	assertFileBundleKeys(t, "ad-hoc-openai", []string{credentials.OpenAIAPIKeyKey})

	cmd, _, _ = newTestCommand(path, strings.NewReader("anthropic-token"))
	err = root.Execute(cmd, []string{
		"--backend", "file",
		"set-credential",
		"--ref", "codereview/work-llm",
		"--key", credentials.AnthropicAPIKeyKey,
		"--stdin",
	})
	if err != nil {
		t.Fatalf("provider key Execute: %v", err)
	}
	assertFileBundleKeys(t, "work-llm", []string{credentials.AnthropicAPIKeyKey})
	assertStored(t, "work-llm", credentials.AnthropicAPIKeyKey, "anthropic-token")
}

func TestSetCredentialUsesGitHubAppCredentialMatrix(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		DefaultProfile: "work",
		Keyring:        config.KeyringConfig{Backend: "file"},
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	work := cfg.Profiles["work"]
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	cfg.Profiles["work"] = work
	saveCredentialTestConfig(t, path, cfg)

	cmd, _, _ := newTestCommand(path, failReader{})
	err := root.Execute(cmd, []string{
		"--backend", "file",
		"set-credential",
		"--ref", "codereview/work-reviewer",
		"--key", credentials.GitTokenKey,
		"--stdin",
	})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("git_token exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if strings.Contains(err.Error(), "secret ingress was read") {
		t.Fatalf("set-credential read secret ingress before rejecting wrong app key: %v", err)
	}

	for _, write := range []struct {
		key   string
		value string
	}{
		{key: credentials.GitHubAppIDKey, value: "12345"},
		{key: credentials.GitHubAppPrivateKeyKey, value: "private-key-value"},
		{key: credentials.GitHubAppInstallationIDKey, value: "67890"},
	} {
		cmd, _, _ = newTestCommand(path, strings.NewReader(write.value))
		err = root.Execute(cmd, []string{
			"--backend", "file",
			"set-credential",
			"--ref", "codereview/work-reviewer",
			"--key", write.key,
			"--stdin",
		})
		if err != nil {
			t.Fatalf("set %s Execute: %v", write.key, err)
		}
		assertStored(t, "work-reviewer", write.key, write.value)
	}
	assertFileBundleKeys(t, "work-reviewer", []string{
		credentials.GitHubAppIDKey,
		credentials.GitHubAppInstallationIDKey,
		credentials.GitHubAppPrivateKeyKey,
	})
}

func TestSetCredentialRejectsUnsupportedConfigBeforeIngress(t *testing.T) {
	hermeticFileBackend(t)
	t.Setenv("CR_FUTURE_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, `default_profile: future
keyring:
  backend: file
profiles:
  future:
    git:
      host: github.com
      auth_mode: oauth_device
      credential_ref: codereview/future
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)

	tests := []struct {
		name      string
		args      []string
		stdin     io.Reader
		mustAvoid string
	}{
		{
			name: "stdin",
			args: []string{
				"--backend", "file",
				"set-credential",
				"--ref", "codereview/future",
				"--key", credentials.GitTokenKey,
				"--stdin",
			},
			stdin:     failReader{},
			mustAvoid: "secret ingress was read",
		},
		{
			name: "from-env",
			args: []string{
				"--backend", "file",
				"set-credential",
				"--ref", "codereview/future",
				"--key", credentials.GitTokenKey,
				"--from-env", "CR_FUTURE_TOKEN",
			},
			stdin:     strings.NewReader(""),
			mustAvoid: "empty secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _, _ := newTestCommand(path, tt.stdin)
			err := root.Execute(cmd, tt.args)
			if !errors.Is(err, config.ErrUnsupported) {
				t.Fatalf("future auth error = %v, want ErrUnsupported", err)
			}
			if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
				t.Fatalf("future auth exit code = %d, want %d; err=%v", got, exitcode.AuthConfigError, err)
			}
			if strings.Contains(err.Error(), tt.mustAvoid) {
				t.Fatalf("set-credential read secret ingress before rejecting future auth ref: %v", err)
			}
		})
	}
}

func TestSetCredentialExitCodeClasses(t *testing.T) {
	t.Run("invalid backend flag", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "bogus",
			"set-credential",
			"--ref", "codereview/work",
			"--key", credentials.GitTokenKey,
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
	})

	t.Run("disallowed key", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "memory",
			"set-credential",
			"--ref", "codereview/work",
			"--key", "bad_key",
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
	})

	t.Run("json error envelope", func(t *testing.T) {
		cmd, out, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "memory",
			"set-credential",
			"--ref", "codereview/work",
			"--key", "bad_key",
			"--stdin",
			"--json",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
		var got view.CredentialWrite
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
		}
		if got.Written || got.Error == "" || got.Ref != "codereview/work" || got.Key != "bad_key" {
			t.Fatalf("credential write JSON = %#v, want written=false error envelope", got)
		}
	})

	t.Run("wrong service ref", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "memory",
			"set-credential",
			"--ref", "other/work",
			"--key", credentials.GitTokenKey,
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
	})

	t.Run("file backend without passphrase", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "")
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "file",
			"set-credential",
			"--ref", "codereview/work",
			"--key", credentials.GitTokenKey,
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.AuthConfigError, err)
		}
	})

	t.Run("existing without overwrite", func(t *testing.T) {
		hermeticFileBackend(t)
		seedFileBackend(t, "work", credentials.GitTokenKey, "first")
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("second"))
		err := root.Execute(cmd, []string{
			"--backend", "file",
			"set-credential",
			"--ref", "codereview/work",
			"--key", credentials.GitTokenKey,
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
		assertStored(t, "work", credentials.GitTokenKey, "first")
	})
}

func TestInitNonInteractiveWritesConfigAndSecret(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "init-token")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "init-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Fatalf("default_profile = %q, want default", cfg.DefaultProfile)
	}
	if cfg.Keyring.Backend != "file" {
		t.Fatalf("keyring.backend = %q, want file", cfg.Keyring.Backend)
	}
	assertStored(t, "default", credentials.GitTokenKey, "init-token")
}

func TestInitNonInteractiveWritesReviewerCredential(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	t.Setenv("CR_REVIEWER_TOKEN", "reviewer-token")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
		"--reviewer-token-from-env", "CR_REVIEWER_TOKEN",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "git-token") || strings.Contains(out.String()+errOut.String(), "reviewer-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	reviewer := cfg.Profiles["default"].ReviewerCredentials
	if reviewer == nil {
		t.Fatal("reviewer credentials missing")
	}
	if reviewer.AuthMode != config.GitAuthModePAT || reviewer.CredentialRef != "codereview/default-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want pat codereview/default-reviewer", reviewer)
	}
	assertStored(t, "default", credentials.GitTokenKey, "git-token")
	assertStored(t, "default-reviewer", credentials.GitTokenKey, "reviewer-token")
}

func TestInitNonInteractiveWritesGitHubAppReviewerConfigOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "memory",
		"init",
		"--non-interactive",
		"--reviewer-auth-mode", string(config.GitAuthModeGitHubApp),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	reviewer := cfg.Profiles["default"].ReviewerCredentials
	if reviewer == nil || reviewer.AuthMode != config.GitAuthModeGitHubApp || reviewer.CredentialRef != "codereview/default-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want github_app codereview/default-reviewer", reviewer)
	}
	for _, key := range []string{credentials.GitHubAppIDKey, credentials.GitHubAppPrivateKeyKey, credentials.GitHubAppInstallationIDKey} {
		if !strings.Contains(errOut.String(), "--key "+key+" --stdin") {
			t.Fatalf("stderr = %q, want setup hint for %s", errOut.String(), key)
		}
	}
	if strings.Contains(out.String()+errOut.String(), "private-key-value") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestInitGitHubAppReviewerRejectsTokenIngressBeforeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	for _, tt := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{
			name: "stdin",
			args: []string{"init", "--non-interactive", "--reviewer-auth-mode", string(config.GitAuthModeGitHubApp), "--reviewer-token-stdin"},
		},
		{
			name: "env",
			args: []string{"init", "--non-interactive", "--reviewer-auth-mode", string(config.GitAuthModeGitHubApp), "--reviewer-token-from-env", "CR_REVIEWER_TOKEN"},
			env:  map[string]string{"CR_REVIEWER_TOKEN": "reviewer-token"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			cmd, _, _ := newTestCommand(path, failReader{})
			err := root.Execute(cmd, tt.args)
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
			}
			if strings.Contains(err.Error(), "secret ingress was read") || strings.Contains(err.Error(), "reviewer-token") {
				t.Fatalf("init read or leaked reviewer token before rejecting app token ingress: %v", err)
			}
		})
	}
}

func TestInitNonInteractiveWritesCustomReviewerCredentialFromStdin(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	cmd, out, errOut := newTestCommand(path, strings.NewReader("reviewer-token\n"))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"--profile", "work",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
		"--reviewer-credential-ref", "codereview/review-bot",
		"--reviewer-token-stdin",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "git-token") || strings.Contains(out.String()+errOut.String(), "reviewer-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "work" {
		t.Fatalf("default_profile = %q, want work", cfg.DefaultProfile)
	}
	reviewer := cfg.Profiles["work"].ReviewerCredentials
	if reviewer == nil || reviewer.CredentialRef != "codereview/review-bot" {
		t.Fatalf("reviewer credentials = %#v, want custom codereview/review-bot", reviewer)
	}
	assertStored(t, "work", credentials.GitTokenKey, "git-token")
	assertStored(t, "review-bot", credentials.GitTokenKey, "reviewer-token")
}

func TestInitNonInteractiveDerivesReviewerRefFromStdinForProfile(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	cmd, out, errOut := newTestCommand(path, strings.NewReader("reviewer-token\n"))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"--profile", "work",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
		"--reviewer-token-stdin",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "git-token") || strings.Contains(out.String()+errOut.String(), "reviewer-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	reviewer := cfg.Profiles["work"].ReviewerCredentials
	if reviewer == nil || reviewer.CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want derived codereview/work-reviewer", reviewer)
	}
	assertStored(t, "work", credentials.GitTokenKey, "git-token")
	assertStored(t, "work-reviewer", credentials.GitTokenKey, "reviewer-token")
}

func TestInitNonInteractiveWritesProviderSpecificAPIKeySecret(t *testing.T) {
	tests := []struct {
		name     string
		provider config.LLMProvider
		adapter  config.LLMAdapter
		key      string
	}{
		{
			name:     "anthropic",
			provider: config.LLMProviderAnthropic,
			adapter:  config.LLMAdapterAnthropicAPI,
			key:      credentials.AnthropicAPIKeyKey,
		},
		{
			name:     "openai",
			provider: config.LLMProviderOpenAI,
			adapter:  config.LLMAdapterOpenAIAPI,
			key:      credentials.OpenAIAPIKeyKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hermeticFileBackend(t)
			path := filepath.Join(t.TempDir(), "config.yml")
			t.Setenv("CR_GIT_TOKEN", "git-token")
			t.Setenv("CR_LLM_KEY", "llm-token")
			cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

			err := root.Execute(cmd, []string{
				"--backend", "file",
				"init",
				"--non-interactive",
				"--git-token-from-env", "CR_GIT_TOKEN",
				"--llm-provider", string(tt.provider),
				"--llm-auth", string(config.LLMAuthAPIKey),
				"--llm-adapter", string(tt.adapter),
				"--llm-api-key-from-env", "CR_LLM_KEY",
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if strings.Contains(out.String()+errOut.String(), "git-token") || strings.Contains(out.String()+errOut.String(), "llm-token") {
				t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			profile := cfg.Profiles["default"]
			if profile.LLM.Provider != tt.provider || profile.LLM.CredentialRef != "codereview/default-llm" {
				t.Fatalf("LLM config = %#v, want provider %s default LLM ref", profile.LLM, tt.provider)
			}
			assertFileBundleKeys(t, "default", []string{credentials.GitTokenKey})
			assertFileBundleKeys(t, "default-llm", []string{tt.key})
			assertStored(t, "default-llm", tt.key, "llm-token")
		})
	}
}

func TestInitNonInteractiveWritesPiRPCProfile(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
		"--llm-provider", string(config.LLMProviderPi),
		"--llm-auth", string(config.LLMAuthSubscription),
		"--llm-adapter", string(config.LLMAdapterPiRPC),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "git-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.LLM.Provider != config.LLMProviderPi ||
		profile.LLM.Auth != config.LLMAuthSubscription ||
		profile.LLM.Adapter != config.LLMAdapterPiRPC ||
		profile.LLM.CredentialRef != "" {
		t.Fatalf("LLM config = %#v, want pi subscription pi_rpc without credential ref", profile.LLM)
	}
	assertFileBundleKeys(t, "default", []string{credentials.GitTokenKey})
	assertStored(t, "default", credentials.GitTokenKey, "git-token")
}

func TestPlanInitCredentials(t *testing.T) {
	t.Run("new git pat defaults to defer", func(t *testing.T) {
		desired := basicProfile("work")
		entries, err := planInitCredentials(nil, desired, nil)
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("entries len = %d, want 1", len(entries))
		}
		entry := entries[0]
		if entry.Ref.Ref != "codereview/work" || entry.State != initCredentialPlanStateDefer {
			t.Fatalf("entry = %#v, want git defer codereview/work", entry)
		}
		if !reflect.DeepEqual(entry.KeySpecs, []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}}) {
			t.Fatalf("key specs = %#v, want git_token required", entry.KeySpecs)
		}
	})

	t.Run("reviewer github app default ref includes bundle keys", func(t *testing.T) {
		desired := basicProfile("work")
		desired.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: "codereview/work-reviewer",
		}
		entries, err := planInitCredentials(nil, desired, nil)
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("entries len = %d, want 2", len(entries))
		}
		entry := entries[1]
		if entry.Ref.Purpose != "reviewer_credentials" || entry.Ref.Ref != "codereview/work-reviewer" || entry.State != initCredentialPlanStateDefer {
			t.Fatalf("reviewer entry = %#v, want deferred reviewer app ref", entry)
		}
		want := []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		}
		if !reflect.DeepEqual(entry.KeySpecs, want) {
			t.Fatalf("key specs = %#v, want %#v", entry.KeySpecs, want)
		}
	})

	t.Run("llm providers use provider-specific api keys", func(t *testing.T) {
		for _, tt := range []struct {
			name     string
			provider config.LLMProvider
			adapter  config.LLMAdapter
			key      string
		}{
			{name: "anthropic", provider: config.LLMProviderAnthropic, adapter: config.LLMAdapterAnthropicAPI, key: credentials.AnthropicAPIKeyKey},
			{name: "openai", provider: config.LLMProviderOpenAI, adapter: config.LLMAdapterOpenAIAPI, key: credentials.OpenAIAPIKeyKey},
		} {
			t.Run(tt.name, func(t *testing.T) {
				desired := basicProfile("work")
				desired.LLM = config.LLMConfig{
					Provider:      tt.provider,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       tt.adapter,
					CredentialRef: "codereview/work-llm",
				}
				entries, err := planInitCredentials(nil, desired, nil)
				if err != nil {
					t.Fatalf("planInitCredentials: %v", err)
				}
				if len(entries) != 2 {
					t.Fatalf("entries len = %d, want 2", len(entries))
				}
				entry := entries[1]
				if entry.Ref.Purpose != "llm" || entry.State != initCredentialPlanStateDefer {
					t.Fatalf("llm entry = %#v, want deferred llm credential", entry)
				}
				want := []credentials.KeySpec{{Key: tt.key, Required: true}}
				if !reflect.DeepEqual(entry.KeySpecs, want) {
					t.Fatalf("key specs = %#v, want %#v", entry.KeySpecs, want)
				}
			})
		}
	})

	t.Run("preserve custom refs across rename interactions", func(t *testing.T) {
		cfg := config.File{
			DefaultProfile: "work",
			Profiles: map[string]config.Profile{
				"work": {
					Git: config.GitConfig{
						Host:          "github.com",
						AuthMode:      config.GitAuthModePAT,
						CredentialRef: "codereview/custom-work",
					},
					ReviewerCredentials: &config.ReviewerCredentials{
						AuthMode:      config.GitAuthModePAT,
						CredentialRef: "codereview/custom-reviewer",
					},
					LLM: config.LLMConfig{
						Provider:      config.LLMProviderAnthropic,
						Auth:          config.LLMAuthAPIKey,
						Adapter:       config.LLMAdapterAnthropicAPI,
						CredentialRef: "codereview/custom-llm",
					},
				},
			},
		}
		renamed, changed, err := configedit.RenameProfile(cfg, "work", "office")
		if err != nil {
			t.Fatalf("RenameProfile: %v", err)
		}
		if !changed {
			t.Fatal("RenameProfile changed = false, want true")
		}
		previous := cfg.Profiles["work"]
		desired := renamed.Profiles["office"]
		entries, err := planInitCredentials(&previous, desired, nil)
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		for _, entry := range entries {
			if entry.State != initCredentialPlanStateKeepExisting {
				t.Fatalf("entry = %#v, want keep_existing across rename", entry)
			}
		}
	})

	t.Run("overwrite custom ref without writes is tracked separately", func(t *testing.T) {
		previous := basicProfile("work")
		previous.Git.CredentialRef = "codereview/custom-old"
		desired := previous
		desired.Git.CredentialRef = "codereview/custom-new"
		entries, err := planInitCredentials(&previous, desired, nil)
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		if entries[0].State != initCredentialPlanStateOverwriteRef {
			t.Fatalf("state = %s, want overwrite_ref", entries[0].State)
		}
	})

	t.Run("optional refs can clear", func(t *testing.T) {
		previous := apiKeyProfile("work", config.LLMProviderAnthropic)
		previous.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/work-reviewer",
		}
		desired := basicProfile("work")
		entries, err := planInitCredentials(&previous, desired, nil)
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		states := map[string]initCredentialPlanState{}
		for _, entry := range entries {
			states[entry.Ref.Purpose] = entry.State
		}
		if states["git"] != initCredentialPlanStateKeepExisting {
			t.Fatalf("git state = %s, want keep_existing", states["git"])
		}
		if states["reviewer_credentials"] != initCredentialPlanStateClearRef {
			t.Fatalf("reviewer state = %s, want clear_ref", states["reviewer_credentials"])
		}
		if states["llm"] != initCredentialPlanStateClearRef {
			t.Fatalf("llm state = %s, want clear_ref", states["llm"])
		}
	})

	t.Run("partial github app bundle reports missing required keys", func(t *testing.T) {
		desired := basicProfile("work")
		desired.Git.AuthMode = config.GitAuthModeGitHubApp
		entries, err := planInitCredentials(nil, desired, map[string][]string{
			"codereview/work": []string{credentials.GitHubAppIDKey},
		})
		if err != nil {
			t.Fatalf("planInitCredentials: %v", err)
		}
		entry := entries[0]
		if entry.State != initCredentialPlanStateMissingRequired {
			t.Fatalf("state = %s, want missing_required", entry.State)
		}
		if !reflect.DeepEqual(entry.PlannedWriteKeys, []string{credentials.GitHubAppIDKey}) {
			t.Fatalf("planned write keys = %#v, want github_app_id only", entry.PlannedWriteKeys)
		}
		if !reflect.DeepEqual(entry.MissingRequiredKeys, []string{credentials.GitHubAppPrivateKeyKey}) {
			t.Fatalf("missing required = %#v, want github_app_private_key", entry.MissingRequiredKeys)
		}
	})
}

func TestInitRuntimeOnlyBackendIsCarriedIntoCredentialHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, _, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "memory",
		"init",
		"--non-interactive",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty runtime-only backend", cfg.Keyring.Backend)
	}
	if !strings.Contains(errOut.String(), "cr --backend memory set-credential") {
		t.Fatalf("stderr = %q, want backend-preserving set-credential hint", errOut.String())
	}
}

func TestInitReviewerConfigOnlyCarriesBackendIntoCredentialHint(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, _, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--reviewer-credential-ref", "codereview/default-reviewer",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	reviewer := cfg.Profiles["default"].ReviewerCredentials
	if reviewer == nil || reviewer.CredentialRef != "codereview/default-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want codereview/default-reviewer", reviewer)
	}
	if got := errOut.String(); !strings.Contains(got, "cr --backend file set-credential --ref codereview/default-reviewer --key git_token --stdin") {
		t.Fatalf("stderr = %q, want backend-preserving reviewer set-credential hint", got)
	}
	store := openFileStore(t)
	defer store.Close()
	present, err := store.Exists("default-reviewer", credentials.GitTokenKey)
	if err != nil {
		t.Fatalf("Exists(default-reviewer, git_token): %v", err)
	}
	if present {
		t.Fatal("reviewer token present, want config-only init to avoid writing credentials")
	}
}

func TestInitPersistsExplicitBackendWhenExistingAPIKeySatisfiesConfig(t *testing.T) {
	hermeticFileBackend(t)
	seedFileBackend(t, "default-llm", credentials.AnthropicAPIKeyKey, "llm-token")
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--llm-auth", string(config.LLMAuthAPIKey),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "llm-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "file" {
		t.Fatalf("keyring.backend = %q, want file", cfg.Keyring.Backend)
	}
}

func TestInitAPIKeyAuthWithExistingKeyDoesNotPrintLLMFollowUpHint(t *testing.T) {
	hermeticFileBackend(t)
	seedFileBackend(t, "default-llm", credentials.AnthropicAPIKeyKey, "llm-token")
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, out, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--llm-auth", string(config.LLMAuthAPIKey),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "llm-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), "--ref codereview/default-llm") {
		t.Fatalf("stderr = %q, want no llm follow-up hint when key already exists", errOut.String())
	}
}

func TestInitReplaceProfilePreservesExistingCredentialRefsByDefault(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		DefaultProfile: "work",
		Keyring:        config.KeyringConfig{Backend: "file"},
		Profiles: map[string]config.Profile{
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/custom-git",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/custom-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					CredentialRef: "codereview/custom-llm",
				},
			},
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	seedFileBackend(t, "custom-llm", credentials.AnthropicAPIKeyKey, "llm-token")
	cmd, _, _ := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"--profile", "work",
		"init",
		"--non-interactive",
		"--replace-profile",
		"--reviewer-auth-mode", string(config.GitAuthModePAT),
		"--llm-auth", string(config.LLMAuthAPIKey),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := got.Profiles["work"]
	if profile.Git.CredentialRef != "codereview/custom-git" {
		t.Fatalf("git ref = %q, want preserved custom-git", profile.Git.CredentialRef)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.CredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer = %#v, want preserved custom-reviewer", profile.ReviewerCredentials)
	}
	if profile.LLM.CredentialRef != "codereview/custom-llm" {
		t.Fatalf("llm ref = %q, want preserved custom-llm", profile.LLM.CredentialRef)
	}
}

func TestInitReplaceProfileRefOverwriteEmitsFollowUpHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	})
	cmd, _, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--profile", "work",
		"init",
		"--non-interactive",
		"--replace-profile",
		"--git-credential-ref", "codereview/rotated-git",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["work"].Git.CredentialRef != "codereview/rotated-git" {
		t.Fatalf("git ref = %q, want rotated-git", got.Profiles["work"].Git.CredentialRef)
	}
	if !strings.Contains(errOut.String(), "set-credential --ref codereview/rotated-git --key git_token --stdin") {
		t.Fatalf("stderr = %q, want overwrite-ref follow-up hint", errOut.String())
	}
}

func TestInitAPIKeyAuthRejectsMissingSecretWithoutWritingDanglingConfig(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, _, _ := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"--backend", "file",
		"init",
		"--non-interactive",
		"--llm-auth", string(config.LLMAuthAPIKey),
	})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if _, err := config.Load(path); !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("Load after failed init error = %v, want ErrNotConfigured", err)
	}
}

func TestInitMergeReplaceAndBackendConflictSemantics(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "file"},
		Profiles: map[string]config.Profile{
			"home": basicProfile("home"),
		},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	t.Setenv("CR_GIT_TOKEN", "work-token")
	cmd, _, _ := newTestCommand(path, strings.NewReader(""))
	err := root.Execute(cmd, []string{
		"--profile", "work",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
	})
	if err != nil {
		t.Fatalf("merge absent profile Execute: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "home" {
		t.Fatalf("default_profile = %q, want existing home", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatalf("work profile missing after merge: %#v", cfg.Profiles)
	}

	cmd, _, _ = newTestCommand(path, strings.NewReader(""))
	err = root.Execute(cmd, []string{"--profile", "work", "init", "--non-interactive"})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("existing profile exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}

	cmd, _, _ = newTestCommand(path, strings.NewReader(""))
	err = root.Execute(cmd, []string{
		"--backend", "memory",
		"--profile", "new",
		"init",
		"--non-interactive",
		"--git-token-from-env", "CR_GIT_TOKEN",
	})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("backend conflict exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
}

func TestInitPlanApplyPreservesUnrelatedExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	keepForever := 0
	home := basicProfile("home")
	home.AgentSources = []string{"/tmp/home-agents"}
	home.LLM.ModelMap = config.ModelMap{"medium": "custom-medium-model"}
	home.LLM.ReviewerModelTier = config.ModelTierMedium
	home.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	existing := config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "file"},
		RepositoryProfiles: []config.RepositoryProfile{
			{
				Profile: "home",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "open-cli-collective",
					Repos:     []string{"codereview-cli"},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": home,
		},
		Data: config.DataConfig{
			Retention: config.RetentionConfig{
				MaxAgeDays:  &keepForever,
				Enforcement: config.RetentionManualOnly,
			},
		},
	}
	if err := config.Save(path, existing); err != nil {
		t.Fatalf("Save existing config: %v", err)
	}
	before, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load existing config: %v", err)
	}
	cmd, _, _ := newTestCommand(path, strings.NewReader(""))

	err = root.Execute(cmd, []string{
		"--profile", "work",
		"init",
		"--non-interactive",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config after init: %v", err)
	}
	if _, ok := after.Profiles["work"]; !ok {
		t.Fatalf("work profile missing after init: %#v", after.Profiles)
	}
	want := before
	want.Profiles = make(map[string]config.Profile, len(before.Profiles)+1)
	for name, profile := range before.Profiles {
		want.Profiles[name] = profile
	}
	want.Profiles["work"] = after.Profiles["work"]
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("config after init = %#v, want only work profile added to %#v", after, before)
	}
}

func TestBuildNonInteractiveInitPlanDoesNotApplySideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	configPathCalled := false
	loadConfigCalled := false
	readSecretCalls := 0
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	deps := initDeps{
		configPath: func(*root.Options) (string, error) {
			configPathCalled = true
			return path, nil
		},
		loadConfig: func(gotPath string) (config.File, bool, error) {
			loadConfigCalled = true
			if gotPath != path {
				t.Fatalf("load path = %q, want %q", gotPath, path)
			}
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("config save called during plan build")
			return nil
		},
		openStore: func(string, bool, config.File) (*credstore.Store, error) {
			t.Fatal("keyring opened during plan build")
			return nil, nil
		},
		readSecret: func(io.Reader, bool, string, string, string) (string, bool, error) {
			readSecretCalls++
			return "", false, nil
		},
	}
	flags := initOptions{
		nonInteractive: true,
		gitHost:        "github.com",
		reviewerAuth:   string(config.GitAuthModePAT),
		llmProvider:    string(config.LLMProviderAnthropic),
		llmAuth:        string(config.LLMAuthSubscription),
		llmAdapter:     string(config.LLMAdapterClaudeCLI),
		majorEvent:     string(config.ReviewMajorEventComment),
	}

	plan, err := buildNonInteractiveInitPlan(&cobra.Command{}, opts, flags, deps)
	if err != nil {
		t.Fatalf("buildNonInteractiveInitPlan: %v", err)
	}
	if !configPathCalled || !loadConfigCalled {
		t.Fatalf("plan build called configPath=%v loadConfig=%v, want both", configPathCalled, loadConfigCalled)
	}
	if readSecretCalls != 0 {
		t.Fatalf("readSecret calls = %d, want 0 without secret ingress flags", readSecretCalls)
	}
	if plan.path != path {
		t.Fatalf("plan path = %q, want %q", plan.path, path)
	}
	if _, ok := plan.cfg.Profiles["default"]; !ok {
		t.Fatalf("plan config profiles = %#v, want default profile", plan.cfg.Profiles)
	}
}

func TestPlanInitCredentialsClearsOptionalRefsInStableOrder(t *testing.T) {
	previous := apiKeyProfile("work", config.LLMProviderAnthropic)
	previous.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
	}
	desired := basicProfile("work")

	entries, err := planInitCredentials(&previous, desired, nil)
	if err != nil {
		t.Fatalf("planInitCredentials: %v", err)
	}

	var cleared []string
	for _, entry := range entries {
		if entry.State == initCredentialPlanStateClearRef {
			cleared = append(cleared, entry.Ref.Purpose)
		}
	}
	if !reflect.DeepEqual(cleared, []string{"reviewer_credentials", "llm"}) {
		t.Fatalf("cleared purposes = %#v, want reviewer then llm", cleared)
	}
}

func TestWriteInitCredentialPlanHintsUsesMissingRequiredKeysOnly(t *testing.T) {
	var stderr bytes.Buffer
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "git",
			Ref:     "codereview/work",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
		PlannedWriteKeys:    []string{credentials.GitHubAppIDKey},
		MissingRequiredKeys: []string{credentials.GitHubAppPrivateKeyKey},
		State:               initCredentialPlanStateMissingRequired,
	}

	if err := writeInitCredentialPlanHints(&stderr, "", entry); err != nil {
		t.Fatalf("writeInitCredentialPlanHints: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "--key "+credentials.GitHubAppPrivateKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want missing required private key hint", got)
	}
	if strings.Contains(got, "--key "+credentials.GitHubAppIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want no already-present app id hint", got)
	}
	if strings.Contains(got, "--key "+credentials.GitHubAppInstallationIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want no optional installation id hint", got)
	}
}

func TestHuhInitPrompterAccessiblePrefillsExistingProfile(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := apiKeyProfile("work", config.LLMProviderOpenAI)
	existing.Git.Host = "gitlab.com"
	existing.Git.CredentialRef = "codereview/custom-git"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/custom-reviewer",
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"1", // Edit work
			"",  // Profile name
			"",  // Make default
			"",  // Git host
			"",  // Git ref
			"",  // Configure reviewer creds
			"",  // Reviewer auth
			"",  // Reviewer ref
			"",  // LLM provider
			"",  // LLM auth
			"",  // LLM adapter
			"",  // LLM ref
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		DefaultProfileName:   "work",
		ExistingConfig: config.File{
			DefaultProfile: "work",
			Profiles:       map[string]config.Profile{"work": existing},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.ProfileName != "work" || draft.OriginalProfileName != "work" {
		t.Fatalf("draft profile = %#v, want existing work prefill", draft)
	}
	if !draft.MakeDefault {
		t.Fatal("draft.MakeDefault = false, want existing default true")
	}
	if draft.GitHost != "gitlab.com" || draft.GitCredentialRef != "codereview/custom-git" {
		t.Fatalf("git draft = (%q,%q), want existing values", draft.GitHost, draft.GitCredentialRef)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) || draft.ReviewerCredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer draft = %#v, want existing reviewer settings", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) || draft.LLMCredentialRef != "codereview/work-llm" {
		t.Fatalf("llm draft = %#v, want existing api-key openai values", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Choose a profile to edit or create") || !strings.Contains(out, "Git credential ref") || !strings.Contains(out, "LLM credential ref") {
		t.Fatalf("wizard output missing expected prompts: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "paste a secret") {
		t.Fatalf("wizard output unexpectedly requested secret ingress: %q", out)
	}
}

func TestInitInteractivePromptBuildsPlanAndPreservesOutOfScopeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := apiKeyProfile("work", config.LLMProviderOpenAI)
	existing.AgentSources = []string{"/tmp/agents"}
	existing.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	existing.LLM.ModelMap = config.ModelMap{"medium": "gpt-custom"}
	existing.LLM.ReviewerModelTier = config.ModelTierLarge
	existing.Git.IdentityCache = "git-cache"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
		IdentityCache: "reviewer-cache",
	}
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		}},
		Profiles: map[string]config.Profile{"work": existing},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	var gotCtx initPromptContext
	deps := initDeps{
		prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			gotCtx = ctx
			return initDraft{
				OriginalProfileName:   "work",
				ProfileName:           "office",
				MakeDefault:           true,
				GitHost:               "github.com",
				GitCredentialRef:      "codereview/office-git",
				ReviewerEnabled:       true,
				ReviewerAuth:          string(config.GitAuthModeGitHubApp),
				ReviewerCredentialRef: "codereview/custom-office-reviewer",
				LLMProvider:           string(config.LLMProviderOpenAI),
				LLMAuth:               string(config.LLMAuthAPIKey),
				LLMAdapter:            string(config.LLMAdapterOpenAIAPI),
				LLMCredentialRef:      "codereview/custom-office-llm",
			}, nil
		}),
		configPath: func(*root.Options) (string, error) {
			return path, nil
		},
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		openStore: func(string, bool, config.File) (*credstore.Store, error) {
			t.Fatal("interactive non-secret init should not open the keyring")
			return nil, nil
		},
		readSecret: func(io.Reader, bool, string, string, string) (string, bool, error) {
			t.Fatal("interactive non-secret init should not read secret ingress")
			return "", false, nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if gotCtx.ExistingProfileName != "work" || gotCtx.DefaultProfileName != "work" {
		t.Fatalf("prompt context = %#v, want existing/default work", gotCtx)
	}
	if gotCtx.ExistingProfile == nil || gotCtx.ExistingProfile.Git.CredentialRef != "codereview/work" {
		t.Fatalf("prompt existing profile = %#v, want work profile", gotCtx.ExistingProfile)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "office" {
		t.Fatalf("default_profile = %q, want office", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Fatalf("old profile still present after rename: %#v", cfg.Profiles)
	}
	profile := cfg.Profiles["office"]
	if profile.Git.CredentialRef != "codereview/office-git" {
		t.Fatalf("git ref = %q, want office-git", profile.Git.CredentialRef)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.CredentialRef != "codereview/custom-office-reviewer" {
		t.Fatalf("reviewer ref = %#v, want preserved custom-office-reviewer", profile.ReviewerCredentials)
	}
	if profile.LLM.CredentialRef != "codereview/custom-office-llm" {
		t.Fatalf("llm ref = %q, want custom-office-llm", profile.LLM.CredentialRef)
	}
	if profile.Git.IdentityCache != "git-cache" {
		t.Fatalf("git identity cache = %q, want preserved git-cache", profile.Git.IdentityCache)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.AuthMode != config.GitAuthModeGitHubApp || profile.ReviewerCredentials.IdentityCache != "" {
		t.Fatalf("reviewer credentials = %#v, want github_app with reset cache", profile.ReviewerCredentials)
	}
	if !reflect.DeepEqual(profile.AgentSources, []string{"/tmp/agents"}) {
		t.Fatalf("agent_sources = %#v, want preserved", profile.AgentSources)
	}
	if !reflect.DeepEqual(profile.LLM.ModelMap, config.ModelMap{"medium": "gpt-custom"}) {
		t.Fatalf("model_map = %#v, want preserved", profile.LLM.ModelMap)
	}
	if profile.LLM.ReviewerModelTier != config.ModelTierLarge {
		t.Fatalf("reviewer_model_tier = %q, want large", profile.LLM.ReviewerModelTier)
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/custom-office-llm --key "+credentials.OpenAIAPIKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred llm follow-up hint", stderr.String())
	}
	if route := cfg.RepositoryProfiles[0]; route.Profile != "office" {
		t.Fatalf("repository route profile = %q, want office", route.Profile)
	}
}

func TestInitInteractiveBlocksRouteHostChangeBeforeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		}},
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				OriginalProfileName: "work",
				ProfileName:         "work",
				GitHost:             "gitlab.com",
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite route-host block")
			return nil
		},
		openStore: func(string, bool, config.File) (*credstore.Store, error) {
			t.Fatal("openStore called despite route-host block")
			return nil, nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "route reconciliation") {
		t.Fatalf("error = %v, want route reconciliation block", err)
	}
}

func TestInitInteractiveBlocksRouteHostChangeDuringRenameBeforeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		}},
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				OriginalProfileName: "work",
				ProfileName:         "office",
				GitHost:             "gitlab.com",
				GitCredentialRef:    "codereview/custom-office-git",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite rename route-host block")
			return nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "route reconciliation") {
		t.Fatalf("error = %v, want route reconciliation block", err)
	}
}

func TestInitRejectsInvalidSecretAndProfileInputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "two stdin secrets", args: []string{"init", "--non-interactive", "--git-token-stdin", "--llm-api-key-stdin"}},
		{name: "git and reviewer stdin secrets", args: []string{"init", "--non-interactive", "--git-token-stdin", "--reviewer-token-stdin"}},
		{name: "invalid profile segment", args: []string{"--profile", "bad.profile", "init", "--non-interactive"}},
		{name: "reviewer ref matches git ref", args: []string{"init", "--non-interactive", "--reviewer-credential-ref", "codereview/default"}},
		{name: "unsupported reviewer auth", args: []string{"init", "--non-interactive", "--reviewer-auth-mode", string(config.GitAuthModeOAuthDevice)}},
		{name: "empty reviewer env secret", args: []string{"init", "--non-interactive", "--reviewer-token-from-env", "CR_EMPTY_REVIEWER_TOKEN"}},
		{name: "llm ingress under subscription auth", args: []string{"init", "--non-interactive", "--llm-api-key-from-env", "CR_LLM_KEY"}},
		{name: "pi rpc adapter without pi provider", args: []string{"init", "--non-interactive", "--llm-adapter", string(config.LLMAdapterPiRPC)}},
		{name: "codex cli adapter without openai provider", args: []string{"init", "--non-interactive", "--llm-adapter", string(config.LLMAdapterCodexCLI)}},
	}
	t.Setenv("CR_LLM_KEY", "llm-key")
	t.Setenv("CR_EMPTY_REVIEWER_TOKEN", "")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("secret"))
			err := root.Execute(cmd, tt.args)
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
			}
		})
	}
}

func TestWriteBundlesReportsPreviouslyWrittenRefsForCleanup(t *testing.T) {
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendMemory,
	})
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer store.Close()
	if _, err := store.SetBundle("b", map[string]string{credentials.GitTokenKey: "existing"}); err != nil {
		t.Fatalf("seed b bundle: %v", err)
	}

	written, err := writeBundles(store, map[string]map[string]string{
		"codereview/a": {credentials.GitTokenKey: "new"},
		"codereview/b": {credentials.GitTokenKey: "conflict"},
	})
	if err == nil {
		t.Fatal("writeBundles error = nil, want conflict")
	}
	if len(written) != 1 || written[0] != "codereview/a" {
		t.Fatalf("written refs = %#v, want codereview/a", written)
	}
	if !strings.Contains(err.Error(), "credential refs needing cleanup: [codereview/a]") {
		t.Fatalf("error = %v, want cleanup ref", err)
	}
}

func newTestCommand(path string, stdin io.Reader) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      stdin,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	Register(cmd, opts)
	return cmd, &stdout, &stderr
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) {
	return 0, errors.New("secret ingress was read")
}

type initPrompterFunc func(initPromptContext) (initDraft, error)

func (f initPrompterFunc) Run(ctx initPromptContext) (initDraft, error) {
	return f(ctx)
}

func hermeticFileBackend(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
}

func seedFileBackend(t *testing.T, profile, key, value string) {
	t.Helper()
	store := openFileStore(t)
	defer store.Close()
	if err := store.Set(profile, key, value, credstore.WithOverwrite()); err != nil {
		t.Fatalf("Set(%s,%s): %v", profile, key, err)
	}
}

func assertStored(t *testing.T, profile, key, want string) {
	t.Helper()
	store := openFileStore(t)
	defer store.Close()
	got, err := store.Get(profile, key)
	if err != nil {
		t.Fatalf("Get(%s,%s): %v", profile, key, err)
	}
	if got != want {
		t.Fatalf("Get(%s,%s) = %q, want %q", profile, key, got, want)
	}
}

func assertFileBundleKeys(t *testing.T, profile string, want []string) {
	t.Helper()
	store := openFileStore(t)
	defer store.Close()
	got, err := store.ListBundle(profile)
	if err != nil {
		t.Fatalf("ListBundle(%s): %v", profile, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListBundle(%s) = %#v, want %#v", profile, got, want)
	}
}

func assertFileBundleEmpty(t *testing.T, profile string) {
	t.Helper()
	store := openFileStore(t)
	defer store.Close()
	got, err := store.ListBundle(profile)
	if err != nil {
		t.Fatalf("ListBundle(%s): %v", profile, err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBundle(%s) = %#v, want empty", profile, got)
	}
}

func openFileStore(t *testing.T) *credstore.Store {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	return store
}

func basicProfile(profile string) config.Profile {
	ref, err := credentials.FormatRef(profile)
	if err != nil {
		panic(err)
	}
	return config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: ref,
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	}
}

func apiKeyProfile(profile string, provider config.LLMProvider) config.Profile {
	p := basicProfile(profile)
	p.LLM = config.LLMConfig{
		Provider:      provider,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterAnthropicAPI,
		CredentialRef: "codereview/" + profile + "-llm",
	}
	if provider == config.LLMProviderOpenAI {
		p.LLM.Adapter = config.LLMAdapterOpenAIAPI
	}
	return p
}

func saveCredentialTestConfig(t *testing.T, path string, cfg config.File) {
	t.Helper()
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
}

func writeRawCredentialTestConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
}
