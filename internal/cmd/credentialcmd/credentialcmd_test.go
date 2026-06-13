package credentialcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
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

func TestInitSetDefaultUpdatesExistingConfigWhenRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, config.File{
		DefaultProfile: "home",
		Profiles: map[string]config.Profile{
			"home": basicProfile("home"),
		},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	cmd, _, _ := newTestCommand(path, strings.NewReader(""))
	err := root.Execute(cmd, []string{
		"--profile", "work",
		"init",
		"--non-interactive",
		"--set-default",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.DefaultProfile != "work" {
		t.Fatalf("default_profile = %q, want work", got.DefaultProfile)
	}
}

func TestInitDurableKeyringBackendFlags(t *testing.T) {
	t.Run("set backend", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		cmd, _, _ := newTestCommand(path, strings.NewReader(""))

		err := root.Execute(cmd, []string{
			"init",
			"--non-interactive",
			"--keyring-backend", "file",
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}

		got, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load config: %v", err)
		}
		if got.Keyring.Backend != "file" {
			t.Fatalf("keyring.backend = %q, want file", got.Keyring.Backend)
		}
	})

	t.Run("reset backend", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		if err := config.Save(path, config.File{
			DefaultProfile: "work",
			Keyring:        config.KeyringConfig{Backend: "file"},
			Profiles: map[string]config.Profile{
				"work": basicProfile("work"),
			},
		}); err != nil {
			t.Fatalf("Save config: %v", err)
		}

		cmd, _, _ := newTestCommand(path, strings.NewReader(""))
		err := root.Execute(cmd, []string{
			"--profile", "work",
			"init",
			"--non-interactive",
			"--replace-profile",
			"--reset-keyring-backend",
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}

		got, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load config: %v", err)
		}
		if got.Keyring.Backend != "" {
			t.Fatalf("keyring.backend = %q, want empty after reset", got.Keyring.Backend)
		}
	})
}

func TestInitGitAuthModeFlagSupportsGitHubApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd, _, errOut := newTestCommand(path, strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"init",
		"--non-interactive",
		"--git-auth-mode", string(config.GitAuthModeGitHubApp),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["default"].Git.AuthMode != config.GitAuthModeGitHubApp {
		t.Fatalf("git.auth_mode = %q, want github_app", got.Profiles["default"].Git.AuthMode)
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "--key "+credentials.GitHubAppIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want github app id follow-up hint", stderr)
	}
	if !strings.Contains(stderr, "--key "+credentials.GitHubAppPrivateKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want github app private key follow-up hint", stderr)
	}
}

func TestInitDisableReviewerClearsReviewerCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("work")
	existing.Git.IdentityCache = "git-cache"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
		IdentityCache: "reviewer-cache",
	}
	existing.LLM.ModelMap = config.ModelMap{
		string(config.ModelTierSmall):  "claude-haiku-4",
		string(config.ModelTierMedium): "claude-sonnet-4-6",
	}
	existing.LLM.ReviewerModelTier = config.ModelTierMedium
	existing.AgentSources = []string{"/tmp/agents", "/tmp/more-agents"}
	existing.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	if err := config.Save(path, config.File{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": existing,
		},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	cmd, _, _ := newTestCommand(path, strings.NewReader(""))
	err := root.Execute(cmd, []string{
		"--profile", "work",
		"init",
		"--non-interactive",
		"--replace-profile",
		"--disable-reviewer",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	expected := existing
	expected.ReviewerCredentials = nil
	if !reflect.DeepEqual(got.Profiles["work"], expected) {
		t.Fatalf("saved profile = %#v, want %#v", got.Profiles["work"], expected)
	}
}

func TestInitLLMReviewerModelTierFlags(t *testing.T) {
	t.Run("set tier", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		cmd, _, _ := newTestCommand(path, strings.NewReader(""))

		err := root.Execute(cmd, []string{
			"init",
			"--non-interactive",
			"--llm-reviewer-model-tier", string(config.ModelTierLarge),
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}

		got, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load config: %v", err)
		}
		if got.Profiles["default"].LLM.ReviewerModelTier != config.ModelTierLarge {
			t.Fatalf("reviewer_model_tier = %q, want large", got.Profiles["default"].LLM.ReviewerModelTier)
		}
	})

	t.Run("clear tier", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		existing := basicProfile("work")
		existing.Git.IdentityCache = "git-cache"
		existing.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/work-reviewer",
			IdentityCache: "reviewer-cache",
		}
		existing.LLM.ModelMap = config.ModelMap{
			string(config.ModelTierSmall):  "claude-haiku-4",
			string(config.ModelTierMedium): "claude-sonnet-4-6",
		}
		existing.LLM.ReviewerModelTier = config.ModelTierMedium
		existing.AgentSources = []string{"/tmp/agents"}
		existing.ReviewPolicy = config.ReviewPolicy{
			MajorEvent:       config.ReviewMajorEventRequestChanges,
			AllowSelfApprove: true,
			ResolveThreads:   config.ResolveThreadsNever,
			ResolveAfter:     "24h",
		}
		if err := config.Save(path, config.File{
			DefaultProfile: "work",
			Profiles: map[string]config.Profile{
				"work": existing,
			},
		}); err != nil {
			t.Fatalf("Save config: %v", err)
		}

		cmd, _, _ := newTestCommand(path, strings.NewReader(""))
		err := root.Execute(cmd, []string{
			"--profile", "work",
			"init",
			"--non-interactive",
			"--replace-profile",
			"--clear-llm-reviewer-model-tier",
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}

		got, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load config: %v", err)
		}
		expected := existing
		expected.LLM.ReviewerModelTier = ""
		if !reflect.DeepEqual(got.Profiles["work"], expected) {
			t.Fatalf("saved profile = %#v, want %#v", got.Profiles["work"], expected)
		}
	})
}

func TestInitGitTokenIngressUnderGitHubAppRejectsBeforeReadingSecret(t *testing.T) {
	cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), failReader{})
	err := root.Execute(cmd, []string{
		"init",
		"--non-interactive",
		"--git-auth-mode", string(config.GitAuthModeGitHubApp),
		"--git-token-stdin",
	})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "git token ingress requires --git-auth-mode pat") {
		t.Fatalf("error = %v, want git auth mode rejection", err)
	}
	if strings.Contains(err.Error(), "secret ingress was read") {
		t.Fatalf("init read git-token stdin before rejecting github_app mode: %v", err)
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
		openStore: func(string, bool, config.File) (initStore, error) {
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
		gitAuth:        string(config.GitAuthModePAT),
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

func TestBuildInteractiveInitWorkspaceCancelLeavesConfigAndKeyringUntouched(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	wantErr := errors.New("canceled")
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{}, wantErr
		}),
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called after interactive cancel")
			return nil
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore called after interactive cancel")
			return nil, nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runInitWithDeps error = %v, want %v", err, wantErr)
	}
}

func TestInitNonInteractiveBypassesInteractiveWorkspacePrompter(t *testing.T) {
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			t.Fatal("interactive prompter called during non-interactive init")
			return initDraft{}, nil
		}),
		configPath: func(*root.Options) (string, error) { return filepath.Join(t.TempDir(), "config.yml"), nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("keyring opened during non-interactive init without secrets")
			return nil, nil
		},
		readSecret: func(io.Reader, bool, string, string, string) (string, bool, error) {
			t.Fatal("readSecret called during non-interactive init without secret ingress")
			return "", false, nil
		},
	}
	flags := initOptions{
		nonInteractive: true,
		gitHost:        "github.com",
		gitAuth:        string(config.GitAuthModePAT),
		reviewerAuth:   string(config.GitAuthModePAT),
		llmProvider:    string(config.LLMProviderAnthropic),
		llmAuth:        string(config.LLMAuthSubscription),
		llmAdapter:     string(config.LLMAdapterClaudeCLI),
		majorEvent:     string(config.ReviewMajorEventComment),
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, flags, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
}

func TestBuildInteractiveInitWorkspaceDoesNotMutateInputConfig(t *testing.T) {
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	cfg := config.File{Profiles: map[string]config.Profile{}}
	draft := initDraft{
		ProfileName: "default",
		MakeDefault: true,
		GitHost:     "github.com",
		GitAuth:     string(config.GitAuthModePAT),
		LLMProvider: string(config.LLMProviderAnthropic),
		LLMAuth:     string(config.LLMAuthSubscription),
		LLMAdapter:  string(config.LLMAdapterClaudeCLI),
	}

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, opts, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if _, ok := cfg.Profiles["default"]; ok {
		t.Fatalf("input config mutated during workspace build: %#v", cfg.Profiles)
	}
	if _, ok := workspace.cfg.Profiles["default"]; !ok {
		t.Fatalf("workspace config profiles = %#v, want draft default profile", workspace.cfg.Profiles)
	}
}

func TestInitGitScopeDraftRoundTripPreservesIdentityCacheFromPreviousProfile(t *testing.T) {
	git := config.GitConfig{
		Host:          "https://github.mycompany.com/",
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work",
		IdentityCache: "rianjs-work",
	}

	scope := initGitScopeDraftFromConfig(git)
	exported := scope.exportConfig(&git)

	if got := exported; !reflect.DeepEqual(got, git) {
		t.Fatalf("exportConfig = %#v, want %#v", got, git)
	}
}

func TestInitGitScopeDraftExportClearsIdentityCacheWhenShapeChanges(t *testing.T) {
	previous := config.GitConfig{
		Host:          "https://github.mycompany.com/",
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work",
		IdentityCache: "rianjs-work",
	}

	scope := initGitScopeDraft{
		Host:          "github.mycompany.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-2",
	}
	exported := scope.exportConfig(&previous)

	if exported.IdentityCache != "" {
		t.Fatalf("IdentityCache = %q, want cleared on scope change", exported.IdentityCache)
	}
	if exported.Host != "github.mycompany.com" {
		t.Fatalf("Host = %q, want draft host spelling on shape change", exported.Host)
	}
}

func TestInitReviewerEntityDraftRoundTripVariants(t *testing.T) {
	tests := []struct {
		name     string
		profile  config.Profile
		previous *config.ReviewerCredentials
		want     *config.ReviewerCredentials
		kind     initReviewerEntityKind
	}{
		{
			name:     "use git identity",
			profile:  basicProfile("work"),
			previous: nil,
			want:     nil,
			kind:     initReviewerEntityKindUseGitIdentity,
		},
		{
			name: "pat reviewer",
			profile: func() config.Profile {
				p := basicProfile("work")
				p.ReviewerCredentials = &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "review-bot",
				}
				return p
			}(),
			previous: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-bot",
			},
			want: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-bot",
			},
			kind: initReviewerEntityKindPAT,
		},
		{
			name: "github app reviewer",
			profile: func() config.Profile {
				p := basicProfile("work")
				p.ReviewerCredentials = &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "review-app",
				}
				return p
			}(),
			previous: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-app",
			},
			want: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-app",
			},
			kind: initReviewerEntityKindGitHubApp,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entity := initReviewerEntityDraftFromConfig(tt.profile)
			if entity.Kind != tt.kind {
				t.Fatalf("Kind = %q, want %q", entity.Kind, tt.kind)
			}
			if got := entity.exportConfig(tt.previous); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("exportConfig = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestInitReviewerEntityDraftExportClearsIdentityCacheWhenShapeChanges(t *testing.T) {
	previous := &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		IdentityCache: "review-app",
	}
	entity := initReviewerEntityDraft{
		Kind:          initReviewerEntityKindPAT,
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer-2",
	}

	exported := entity.exportConfig(previous)

	if exported == nil {
		t.Fatal("exportConfig = nil, want separate reviewer credentials")
	}
	if exported.IdentityCache != "" {
		t.Fatalf("IdentityCache = %q, want cleared on reviewer entity change", exported.IdentityCache)
	}
}

func TestBuildInitGitScopeInventoryDeduplicatesNormalizedGitHubEnterpriseHost(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.Git.Host = "https://github.mycompany.com/"
	work.Git.Host = "github.mycompany.com"
	home.Git.AuthMode = config.GitAuthModeGitHubApp
	work.Git.AuthMode = config.GitAuthModeGitHubApp
	home.Git.CredentialRef = "codereview/shared-ghe"
	work.Git.CredentialRef = "codereview/shared-ghe"
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}

	scopes, profileScopeNames := buildInitGitScopeInventory(cfg)

	if len(scopes) != 1 {
		t.Fatalf("len(scopes) = %d, want 1; scopes=%#v", len(scopes), scopes)
	}
	if profileScopeNames["home"] != profileScopeNames["work"] {
		t.Fatalf("profileScopeNames = %#v, want shared normalized GHE scope", profileScopeNames)
	}
}

func TestBuildInitGitScopeInventoryAssignsStableSuffixOnNameCollision(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.Git.Host = "github.com"
	work.Git.Host = "https://github.com/"
	home.Git.CredentialRef = "codereview/home"
	work.Git.CredentialRef = "codereview/work"
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}

	scopes, profileScopeNames := buildInitGitScopeInventory(cfg)

	if len(scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2; scopes=%#v", len(scopes), scopes)
	}
	if profileScopeNames["home"] == profileScopeNames["work"] {
		t.Fatalf("profileScopeNames = %#v, want distinct names for colliding scope bases", profileScopeNames)
	}
	if profileScopeNames["home"] != "github-com-pat" && profileScopeNames["work"] != "github-com-pat" {
		t.Fatalf("profileScopeNames = %#v, want one unsuffixed base name", profileScopeNames)
	}
	if initGitScopeLabel(scopes[profileScopeNames["home"]]) == initGitScopeLabel(scopes[profileScopeNames["work"]]) {
		t.Fatalf("git scope labels should be distinguishable: %#v", profileScopeNames)
	}
}

func TestBuildInitReviewerEntityInventoryVariantsAndDeduping(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	bot := basicProfile("bot")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/shared-reviewer",
	}
	bot.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/shared-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
			"bot":  bot,
		},
	}

	entities, profileEntityNames := buildInitReviewerEntityInventory(cfg)

	if len(entities) != 2 {
		t.Fatalf("len(entities) = %d, want 2; entities=%#v", len(entities), entities)
	}
	if profileEntityNames["home"] == profileEntityNames["work"] {
		t.Fatalf("profileEntityNames = %#v, want self entity distinct from PAT reviewer", profileEntityNames)
	}
	if profileEntityNames["work"] != profileEntityNames["bot"] {
		t.Fatalf("profileEntityNames = %#v, want deduped PAT reviewer entity", profileEntityNames)
	}
}

func TestBuildInitReviewerEntityInventoryAssignsStableSuffixOnNameCollision(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/home-reviewer",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}

	entities, profileEntityNames := buildInitReviewerEntityInventory(cfg)

	if len(entities) != 2 {
		t.Fatalf("len(entities) = %d, want 2; entities=%#v", len(entities), entities)
	}
	if profileEntityNames["home"] == profileEntityNames["work"] {
		t.Fatalf("profileEntityNames = %#v, want distinct names for colliding reviewer bases", profileEntityNames)
	}
	if profileEntityNames["home"] != "reviewer-pat" && profileEntityNames["work"] != "reviewer-pat" {
		t.Fatalf("profileEntityNames = %#v, want one unsuffixed base name", profileEntityNames)
	}
	if initReviewerEntityLabel(entities[profileEntityNames["home"]]) == initReviewerEntityLabel(entities[profileEntityNames["work"]]) {
		t.Fatalf("reviewer labels should be distinguishable: %#v", profileEntityNames)
	}
}

func TestSharedGitScopeAndReviewerEntityDoNotDriftIdentityCacheAcrossProfiles(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.Git.Host = "github.mycompany.com"
	work.Git.Host = "https://github.mycompany.com/"
	home.Git.AuthMode = config.GitAuthModeGitHubApp
	work.Git.AuthMode = config.GitAuthModeGitHubApp
	home.Git.CredentialRef = "codereview/shared-git"
	work.Git.CredentialRef = "codereview/shared-git"
	home.Git.IdentityCache = "home-cache"
	work.Git.IdentityCache = "work-cache"
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
		IdentityCache: "home-reviewer-cache",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
		IdentityCache: "work-reviewer-cache",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}

	scopes, scopeNames := buildInitGitScopeInventory(cfg)
	entities, entityNames := buildInitReviewerEntityInventory(cfg)

	homeGit := scopes[scopeNames["home"]].exportConfig(&home.Git)
	workGit := scopes[scopeNames["work"]].exportConfig(&work.Git)
	if homeGit.IdentityCache != "home-cache" || workGit.IdentityCache != "work-cache" {
		t.Fatalf("git identity caches drifted: home=%q work=%q", homeGit.IdentityCache, workGit.IdentityCache)
	}

	homeReviewer := entities[entityNames["home"]].exportConfig(home.ReviewerCredentials)
	workReviewer := entities[entityNames["work"]].exportConfig(work.ReviewerCredentials)
	if homeReviewer == nil || workReviewer == nil {
		t.Fatalf("reviewer export = (%#v,%#v), want distinct github_app reviewers", homeReviewer, workReviewer)
	}
	if homeReviewer.IdentityCache != "home-reviewer-cache" || workReviewer.IdentityCache != "work-reviewer-cache" {
		t.Fatalf("reviewer identity caches drifted: home=%q work=%q", homeReviewer.IdentityCache, workReviewer.IdentityCache)
	}
	if homeGit.Host != "github.mycompany.com" || workGit.Host != "https://github.mycompany.com/" {
		t.Fatalf("git host spellings drifted: home=%q work=%q", homeGit.Host, workGit.Host)
	}
}

func TestInitLLMRuntimeDraftFromConfigRecognizesKnownPresets(t *testing.T) {
	tests := []struct {
		name   string
		llm    config.LLMConfig
		preset initLLMRuntimePreset
	}{
		{
			name: "claude cli subscription",
			llm: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
			preset: initLLMRuntimePresetClaudeCLISubscription,
		},
		{
			name: "codex cli subscription",
			llm: config.LLMConfig{
				Provider: config.LLMProviderOpenAI,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterCodexCLI,
			},
			preset: initLLMRuntimePresetCodexCLISubscription,
		},
		{
			name: "pi local runtime",
			llm: config.LLMConfig{
				Provider: config.LLMProviderPi,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterPiRPC,
			},
			preset: initLLMRuntimePresetPiLocal,
		},
		{
			name: "anthropic api key",
			llm: config.LLMConfig{
				Provider:      config.LLMProviderAnthropic,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterAnthropicAPI,
				CredentialRef: "codereview/work-llm",
			},
			preset: initLLMRuntimePresetAnthropicAPIKey,
		},
		{
			name: "openai api key",
			llm: config.LLMConfig{
				Provider:      config.LLMProviderOpenAI,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterOpenAIAPI,
				CredentialRef: "codereview/work-llm",
			},
			preset: initLLMRuntimePresetOpenAIAPIKey,
		},
		{
			name: "custom supported combination",
			llm: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterAnthropicAPI,
			},
			preset: initLLMRuntimePresetCustom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := initLLMRuntimeDraftFromConfig(tt.llm)
			if runtime.Preset != tt.preset {
				t.Fatalf("Preset = %q, want %q", runtime.Preset, tt.preset)
			}
			if got := runtime.exportConfig(); !reflect.DeepEqual(got, tt.llm) {
				t.Fatalf("exportConfig = %#v, want %#v", got, tt.llm)
			}
		})
	}
}

func TestBuildInitLLMRuntimeInventoryDeduplicatesSharedAPIKeyRuntime(t *testing.T) {
	home := apiKeyProfile("home", config.LLMProviderOpenAI)
	work := apiKeyProfile("work", config.LLMProviderOpenAI)
	home.LLM.CredentialRef = "codereview/shared-llm"
	work.LLM.CredentialRef = "codereview/shared-llm"
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}

	runtimes, profileRuntimeNames := buildInitLLMRuntimeInventory(cfg)

	if len(runtimes) != 1 {
		t.Fatalf("len(runtimes) = %d, want 1; runtimes=%#v", len(runtimes), runtimes)
	}
	if profileRuntimeNames["home"] == "" || profileRuntimeNames["home"] != profileRuntimeNames["work"] {
		t.Fatalf("profileRuntimeNames = %#v, want shared runtime name for home/work", profileRuntimeNames)
	}
	runtime := runtimes[profileRuntimeNames["home"]]
	if runtime.Preset != initLLMRuntimePresetOpenAIAPIKey {
		t.Fatalf("runtime preset = %q, want %q", runtime.Preset, initLLMRuntimePresetOpenAIAPIKey)
	}
	if runtime.CredentialRef != "codereview/shared-llm" {
		t.Fatalf("runtime credential ref = %q, want codereview/shared-llm", runtime.CredentialRef)
	}
}

func TestInitLLMRuntimeLabelsDifferentiateSamePresetEntries(t *testing.T) {
	first := initLLMRuntimeDraft{
		Name:          "anthropic-api-key",
		Preset:        initLLMRuntimePresetAnthropicAPIKey,
		Provider:      config.LLMProviderAnthropic,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterAnthropicAPI,
		CredentialRef: "codereview/a",
	}
	second := first
	second.Name = "anthropic-api-key-2"
	second.CredentialRef = "codereview/b"

	if initLLMRuntimeLabel(first) == initLLMRuntimeLabel(second) {
		t.Fatalf("runtime labels should be distinguishable: %q", initLLMRuntimeLabel(first))
	}
}

func TestBuildInitLLMRuntimeInventoryKeepsCrossProviderSameRefDistinct(t *testing.T) {
	openAI := apiKeyProfile("home", config.LLMProviderOpenAI)
	anthropic := apiKeyProfile("work", config.LLMProviderAnthropic)
	openAI.LLM.CredentialRef = "codereview/shared-llm"
	anthropic.LLM.CredentialRef = "codereview/shared-llm"
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": openAI,
			"work": anthropic,
		},
	}

	runtimes, profileRuntimeNames := buildInitLLMRuntimeInventory(cfg)

	if len(runtimes) != 2 {
		t.Fatalf("len(runtimes) = %d, want 2; runtimes=%#v", len(runtimes), runtimes)
	}
	if profileRuntimeNames["home"] == profileRuntimeNames["work"] {
		t.Fatalf("profileRuntimeNames = %#v, want distinct runtime names for cross-provider shared ref", profileRuntimeNames)
	}
	if runtimes[profileRuntimeNames["home"]].CredentialRef != "codereview/shared-llm" || runtimes[profileRuntimeNames["work"]].CredentialRef != "codereview/shared-llm" {
		t.Fatalf("runtime refs = %#v, want shared credential ref preserved for both providers", runtimes)
	}
}

func TestBuildInteractiveInitWorkspaceImportsLLMRuntimeInventory(t *testing.T) {
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	home := apiKeyProfile("home", config.LLMProviderOpenAI)
	work := apiKeyProfile("work", config.LLMProviderOpenAI)
	home.LLM.CredentialRef = "codereview/shared-llm"
	work.LLM.CredentialRef = "codereview/shared-llm"
	cfg := config.File{
		DefaultProfile: "home",
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	draft := seedInteractiveInitDraft("home", "home", "home", &home)

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, opts, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if workspace.llmRuntimeName == "" {
		t.Fatal("workspace.llmRuntimeName = empty, want selected runtime")
	}
	if len(workspace.llmRuntimes) != 1 {
		t.Fatalf("len(workspace.llmRuntimes) = %d, want 1; runtimes=%#v", len(workspace.llmRuntimes), workspace.llmRuntimes)
	}
	runtime := workspace.llmRuntimes[workspace.llmRuntimeName]
	if got := runtime.exportConfig(); !reflect.DeepEqual(got, workspace.profile.LLM) {
		t.Fatalf("selected runtime export = %#v, want profile llm %#v", got, workspace.profile.LLM)
	}
}

func TestBuildInteractiveInitWorkspaceImportsGitScopeAndReviewerEntityInventory(t *testing.T) {
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	existing := basicProfile("work")
	existing.Git.Host = "github.mycompany.com"
	existing.Git.AuthMode = config.GitAuthModeGitHubApp
	existing.Git.CredentialRef = "codereview/office-git"
	existing.Git.IdentityCache = "git-cache"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/office-reviewer",
		IdentityCache: "reviewer-cache",
	}
	cfg := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": existing},
	}
	draft := seedInteractiveInitDraft("work", "work", "work", &existing)

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, opts, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if workspace.gitScopeName == "" || workspace.reviewerEntityName == "" {
		t.Fatalf("workspace names = git:%q reviewer:%q, want imported inventory selections", workspace.gitScopeName, workspace.reviewerEntityName)
	}
	gitScope := workspace.gitScopes[workspace.gitScopeName]
	if got := gitScope.exportConfig(&existing.Git); !reflect.DeepEqual(got, existing.Git) {
		t.Fatalf("git scope export = %#v, want %#v", got, existing.Git)
	}
	reviewerEntity := workspace.reviewerEntities[workspace.reviewerEntityName]
	if got := reviewerEntity.exportConfig(existing.ReviewerCredentials); !reflect.DeepEqual(got, existing.ReviewerCredentials) {
		t.Fatalf("reviewer entity export = %#v, want %#v", got, existing.ReviewerCredentials)
	}
}

func TestInitInteractivePromptDrivenFlowStillExportsReviewerNilAfterDraftInventoryImport(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	var savedCfg config.File
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:      "office",
				MakeDefault:      true,
				GitHost:          "github.mycompany.com",
				GitAuth:          string(config.GitAuthModePAT),
				GitCredentialRef: "codereview/office-git",
				ReviewerEnabled:  false,
				LLMProvider:      string(config.LLMProviderAnthropic),
				LLMAuth:          string(config.LLMAuthSubscription),
				LLMAdapter:       string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionKeep},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"office-git": {credentials.GitTokenKey: "existing-token"},
			}), nil
		},
	}

	deps.saveConfig = func(_ string, cfg config.File) error {
		savedCfg = cloneInitConfigFile(cfg)
		return nil
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	profile := savedCfg.Profiles["office"]
	if profile.Git.Host != "github.mycompany.com" || profile.Git.AuthMode != config.GitAuthModePAT || profile.Git.CredentialRef != "codereview/office-git" {
		t.Fatalf("git profile = %#v, want PAT office git scope", profile.Git)
	}
	if profile.ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials = %#v, want nil for use-git-identity path", profile.ReviewerCredentials)
	}
}

func TestInitInteractivePromptDrivenFlowStillExportsSeparateReviewerAfterDraftInventoryImport(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	var savedCfg config.File
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:           "office",
				MakeDefault:           true,
				GitHost:               "github.mycompany.com",
				GitAuth:               string(config.GitAuthModePAT),
				GitCredentialRef:      "codereview/office-git",
				ReviewerEnabled:       true,
				ReviewerAuth:          string(config.GitAuthModeGitHubApp),
				ReviewerCredentialRef: "codereview/office-reviewer",
				LLMProvider:           string(config.LLMProviderAnthropic),
				LLMAuth:               string(config.LLMAuthSubscription),
				LLMAdapter:            string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(_ string, cfg config.File) error {
			savedCfg = cloneInitConfigFile(cfg)
			return nil
		},
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"office-git": {
					credentials.GitTokenKey: "existing-git-token",
				},
				"office-reviewer": {
					credentials.GitHubAppIDKey:         "12345",
					credentials.GitHubAppPrivateKeyKey: "private-key",
				},
			}), nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	profile := savedCfg.Profiles["office"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want separate reviewer preserved")
	}
	if profile.ReviewerCredentials.AuthMode != config.GitAuthModeGitHubApp || profile.ReviewerCredentials.CredentialRef != "codereview/office-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want github_app office reviewer", profile.ReviewerCredentials)
	}
}

func TestCollectInteractiveInitSecretsRecordsDraftWritesBeforeApply(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle called during draft secret collection")
		return credstore.Result{}, nil
	}
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	cfg := config.File{Profiles: map[string]config.Profile{}}
	draft := initDraft{
		ProfileName: "default",
		MakeDefault: true,
		GitHost:     "github.com",
		GitAuth:     string(config.GitAuthModePAT),
		LLMProvider: string(config.LLMProviderAnthropic),
		LLMAuth:     string(config.LLMAuthSubscription),
		LLMAdapter:  string(config.LLMAdapterClaudeCLI),
	}
	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, opts, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	deps := initDeps{
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionSetNow,
				initCredentialSecretActionSetNow,
			},
			sources: []initSecretSource{initSecretSourcePaste},
			pastes:  []string{"new-token"},
		},
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read")
			return "", nil
		},
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	workspace, err = collectInteractiveInitSecrets(&cobra.Command{}, opts, deps, workspace)
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if got := workspace.writes["codereview/default"][credentials.GitTokenKey]; got != "new-token" {
		t.Fatalf("draft write = %q, want new-token", got)
	}
	if !workspace.satisfiedRefs["codereview/default"] {
		t.Fatalf("satisfiedRefs = %#v, want default ref marked satisfied", workspace.satisfiedRefs)
	}
	if workspace.overwriteRefs["codereview/default"] {
		t.Fatalf("overwriteRefs = %#v, want default ref not marked for overwrite", workspace.overwriteRefs)
	}
	if len(workspace.credentialPlan) != 1 || workspace.credentialPlan[0].State != initCredentialPlanStateWrite {
		t.Fatalf("credentialPlan = %#v, want single write entry", workspace.credentialPlan)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "" {
		t.Fatalf("stored git token = %q, want no keyring write before apply", got)
	}
}

func TestCollectInteractiveInitSecretsSourceBackReturnsToCredentialChoices(t *testing.T) {
	store := newFakeInitStore(nil)
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{
			initCredentialSecretActionSetNow,
			initCredentialSecretActionDefer,
		},
		sources: []initSecretSource{initSecretSourceBack},
	}
	workspace, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read")
			return "", nil
		},
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
	}, testInitSecretWorkspace())
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if len(prompter.actions) != 0 || len(prompter.sources) != 0 {
		t.Fatalf("prompter queues = actions %#v sources %#v, want consumed", prompter.actions, prompter.sources)
	}
	if got := workspace.writes["codereview/default"][credentials.GitTokenKey]; got != "" {
		t.Fatalf("draft write = %q, want none after source Back then defer", got)
	}
	if workspace.satisfiedRefs["codereview/default"] {
		t.Fatalf("satisfiedRefs = %#v, want deferred ref unsatisfied", workspace.satisfiedRefs)
	}
}

func TestCollectInteractiveInitSecretsPasteBackReturnsToSourceChoices(t *testing.T) {
	store := newFakeInitStore(nil)
	prompter := &fakeInitSecretPrompter{
		actions:     []initCredentialSecretAction{initCredentialSecretActionSetNow},
		sources:     []initSecretSource{initSecretSourcePaste, initSecretSourceClipboard},
		pasteErrors: []error{errInitSecretValueBack},
	}
	clipboardReads := 0
	workspace, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return true },
		clipboardRead: func() (string, error) {
			clipboardReads++
			return "clipboard-token", nil
		},
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
	}, testInitSecretWorkspace())
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if got := workspace.writes["codereview/default"][credentials.GitTokenKey]; got != "clipboard-token" {
		t.Fatalf("draft write = %q, want clipboard-token", got)
	}
	if !workspace.satisfiedRefs["codereview/default"] {
		t.Fatalf("satisfiedRefs = %#v, want ref satisfied after clipboard retry", workspace.satisfiedRefs)
	}
	if clipboardReads != 1 {
		t.Fatalf("clipboardReads = %d, want 1", clipboardReads)
	}
	if len(prompter.actions) != 0 || len(prompter.sources) != 0 || len(prompter.pasteErrors) != 0 {
		t.Fatalf("prompter queues = actions %#v sources %#v pasteErrors %#v, want consumed", prompter.actions, prompter.sources, prompter.pasteErrors)
	}
}

func TestCollectInteractiveInitSecretsBackAfterPartialMultiKeyWriteDiscardsScratch(t *testing.T) {
	store := newFakeInitStore(nil)
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{
			initCredentialSecretActionSetNow,
			initCredentialSecretActionDefer,
		},
		sources: []initSecretSource{
			initSecretSourcePaste,
			initSecretSourceBack,
		},
		pastes: []string{"12345"},
	}
	workspace, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read")
			return "", nil
		},
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
	}, testInitMultiKeySecretWorkspace())
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if got := workspace.writes["codereview/default"][credentials.GitHubAppIDKey]; got != "" {
		t.Fatalf("partial draft write = %q, want discarded after later source Back then defer", got)
	}
	if workspace.satisfiedRefs["codereview/default"] {
		t.Fatalf("satisfiedRefs = %#v, want deferred ref unsatisfied", workspace.satisfiedRefs)
	}
	if len(prompter.actions) != 0 || len(prompter.sources) != 0 || len(prompter.pastes) != 0 {
		t.Fatalf("prompter queues = actions %#v sources %#v pastes %#v, want consumed", prompter.actions, prompter.sources, prompter.pastes)
	}
}

func testInitSecretWorkspace() initWorkspaceDraft {
	return initWorkspaceDraft{
		cfg: config.File{Profiles: map[string]config.Profile{}},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "git",
				Ref:     "codereview/default",
				Mode:    string(config.GitAuthModePAT),
			},
			KeySpecs: []credentials.KeySpec{{
				Key:      credentials.GitTokenKey,
				Required: true,
			}},
			MissingRequiredKeys: []string{credentials.GitTokenKey},
			State:               initCredentialPlanStateMissingRequired,
		}},
	}
}

func testInitMultiKeySecretWorkspace() initWorkspaceDraft {
	return initWorkspaceDraft{
		cfg: config.File{Profiles: map[string]config.Profile{}},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "git",
				Ref:     "codereview/default",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			},
			MissingRequiredKeys: []string{
				credentials.GitHubAppIDKey,
				credentials.GitHubAppPrivateKeyKey,
			},
			State: initCredentialPlanStateMissingRequired,
		}},
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
	existing.Git.AuthMode = config.GitAuthModeGitHubApp
	existing.Git.CredentialRef = "codereview/custom-git"
	existing.LLM.ReviewerModelTier = config.ModelTierMedium
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
			"",  // Git auth
			"",  // Git ref
			"",  // Configure reviewer creds
			"",  // Reviewer auth
			"",  // Reviewer ref
			"",  // LLM provider
			"",  // LLM auth
			"",  // LLM adapter
			"",  // Reviewer model tier
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
		GitScopes: map[string]initGitScopeDraft{
			"gitlab-scope": initGitScopeDraftFromConfig(existing.Git),
		},
		ProfileGitScopes: map[string]string{"work": "gitlab-scope"},
		ReviewerEntities: map[string]initReviewerEntityDraft{
			"reviewer-app": initReviewerEntityDraftFromConfig(existing),
		},
		ProfileReviewerEntities: map[string]string{"work": "reviewer-app"},
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"openai-runtime": initLLMRuntimeDraftFromConfig(existing.LLM),
		},
		ProfileLLMRuntimes: map[string]string{"work": "openai-runtime"},
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
	if draft.GitHost != "gitlab.com" || draft.GitAuth != string(config.GitAuthModeGitHubApp) || draft.GitCredentialRef != "codereview/custom-git" {
		t.Fatalf("git draft = %#v, want existing values", draft)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) || draft.ReviewerCredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer draft = %#v, want existing reviewer settings", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) || draft.LLMReviewerModelTier != string(config.ModelTierMedium) || draft.LLMCredentialRef != "codereview/work-llm" {
		t.Fatalf("llm draft = %#v, want existing api-key openai values", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Choose a profile to edit or create") || !strings.Contains(out, "Git scope host") || !strings.Contains(out, "Reviewer entity") || !strings.Contains(out, "LLM runtime") {
		t.Fatalf("wizard output missing expected prompts: %q", out)
	}
	if strings.Contains(out, "Git credential ref") || strings.Contains(out, "LLM credential ref") {
		t.Fatalf("wizard output unexpectedly exposed raw credential refs on the primary path: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "paste a secret") {
		t.Fatalf("wizard output unexpectedly requested secret ingress: %q", out)
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfileStartsFreshSeed(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := apiKeyProfile("work", config.LLMProviderOpenAI)
	existing.Git.Host = "gitlab.com"
	existing.Git.AuthMode = config.GitAuthModeGitHubApp
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Create new profile
			"",  // Profile name
			"",  // Make default
			"",  // Git host
			"",  // Git auth
			"",  // Reviewer entity
			"",  // LLM runtime
			"",  // Reviewer model tier
			"",  // Advanced storage labels
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
		GitScopes: map[string]initGitScopeDraft{
			"gitlab-work": initGitScopeDraftFromConfig(existing.Git),
		},
		ProfileGitScopes: map[string]string{"work": "gitlab-work"},
		ReviewerEntities: map[string]initReviewerEntityDraft{
			"work-reviewer": initReviewerEntityDraftFromConfig(existing),
		},
		ProfileReviewerEntities: map[string]string{"work": "work-reviewer"},
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"work-runtime": initLLMRuntimeDraftFromConfig(existing.LLM),
		},
		ProfileLLMRuntimes: map[string]string{"work": "work-runtime"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.OriginalProfileName != "" {
		t.Fatalf("draft.OriginalProfileName = %q, want blank for create-new seed", draft.OriginalProfileName)
	}
	if draft.ProfileName != credstore.DefaultProfile {
		t.Fatalf("draft.ProfileName = %q, want fresh default profile seed", draft.ProfileName)
	}
	if draft.GitHost != "github.com" || draft.GitAuth != string(config.GitAuthModePAT) {
		t.Fatalf("git draft = %#v, want fresh defaults for create-new seed", draft)
	}
	if draft.ReviewerEnabled {
		t.Fatalf("reviewer draft = %#v, want create-new seed to avoid inherited separate reviewer", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderAnthropic) || draft.LLMAuth != string(config.LLMAuthSubscription) || draft.LLMAdapter != string(config.LLMAdapterClaudeCLI) {
		t.Fatalf("llm draft = %#v, want fresh llm defaults for create-new seed", draft)
	}
}

func TestHuhInitPrompterAccessibleRequestedNewProfilePreservesExplicitName(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git host
			"", // Git auth
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Advanced storage labels
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "office",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.OriginalProfileName != "" {
		t.Fatalf("draft.OriginalProfileName = %q, want blank for new requested profile", draft.OriginalProfileName)
	}
	if draft.ProfileName != "office" {
		t.Fatalf("draft.ProfileName = %q, want explicit requested profile name preserved", draft.ProfileName)
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfilePreservesExplicitRequestedNameWhenNoProfileMatched(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Create new profile
			"",  // Profile name
			"",  // Make default
			"",  // Git host
			"",  // Git auth
			"",  // Reviewer entity
			"",  // LLM runtime
			"",  // Reviewer model tier
			"",  // Advanced storage labels
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "office",
		ExistingProfileNames: []string{"work"},
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.OriginalProfileName != "" {
		t.Fatalf("draft.OriginalProfileName = %q, want blank for unmatched requested profile", draft.OriginalProfileName)
	}
	if draft.ProfileName != "office" {
		t.Fatalf("draft.ProfileName = %q, want explicit requested profile name preserved for create-new fallback", draft.ProfileName)
	}
}

func TestRunBackableInitFormEscapeReturnsBack(t *testing.T) {
	t.Setenv("TERM", "xterm")
	var stderr bytes.Buffer
	choice := "stay"
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Backable").
				Options(
					huh.NewOption("Stay", "stay"),
					huh.NewOption("Leave", "leave"),
				).
				Value(&choice),
		),
	)

	back, err := runBackableInitForm(form, strings.NewReader("\x1b"), &stderr)
	if err != nil {
		t.Fatalf("runBackableInitForm: %v", err)
	}
	if !back {
		t.Fatal("back = false, want Escape to return local back")
	}
	if choice != "stay" {
		t.Fatalf("choice = %q, want unchanged default", choice)
	}
}

func TestRunBackableInitFormCtrlCStillAborts(t *testing.T) {
	t.Setenv("TERM", "xterm")
	var stderr bytes.Buffer
	choice := "stay"
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Backable").
				Options(
					huh.NewOption("Stay", "stay"),
					huh.NewOption("Leave", "leave"),
				).
				Value(&choice),
		),
	)

	back, err := runBackableInitForm(form, strings.NewReader("\x03"), &stderr)
	if !errors.Is(err, huh.ErrUserAborted) {
		t.Fatalf("runBackableInitForm error = %v, want ErrUserAborted", err)
	}
	if back {
		t.Fatal("back = true, want Ctrl+C to remain abort")
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleConfiguredRuntimeShowsDetails(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"1",
			"",
			"",
			"",
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		DefaultProfileName:   "work",
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderAnthropic) || draft.LLMAdapter != string(config.LLMAdapterClaudeCLI) {
		t.Fatalf("draft = %#v, want existing claude runtime", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Configured: Claude CLI subscription (claude-cli)") || !strings.Contains(out, "Custom compatible runtime") {
		t.Fatalf("stderr = %q, want mixed configured/custom runtime options", out)
	}
	if !strings.Contains(out, "LLM provider") || !strings.Contains(out, "LLM auth mode") || !strings.Contains(out, "LLM adapter") {
		t.Fatalf("stderr = %q, want configured runtime flow to show editable details", out)
	}
	if !strings.Contains(out, "Back to main menu") {
		t.Fatalf("stderr = %q, want focused runtime Back option", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleTemplateShowsDetails(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"3", // Template: Codex CLI subscription.
			"",  // Keep OpenAI provider default from the selected template.
			"",  // Keep subscription auth default.
			"",  // Keep Codex CLI adapter default.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		DefaultProfileName:   "work",
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthSubscription) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft = %#v, want codex subscription runtime", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Template: Codex CLI subscription") ||
		!strings.Contains(out, "LLM provider") ||
		!strings.Contains(out, "LLM auth mode") ||
		!strings.Contains(out, "LLM adapter") {
		t.Fatalf("stderr = %q, want template selection to show editable details", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleCustomRuntimeShowsCustomFields(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.LLM = config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterAnthropicAPI,
	}
	cfg := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": existing},
	}
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",
			"2",
			"2",
			"4",
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		DefaultProfileName:   "work",
		ExistingConfig:       cfg,
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"claude-cli": {
				Name:     "claude-cli",
				Preset:   initLLMRuntimePresetClaudeCLISubscription,
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
		},
		ProfileLLMRuntimes: map[string]string{"work": initCustomLLMRuntimeSelection},
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.ProfileName != "work" {
		t.Fatalf("draft.ProfileName = %q, want work", draft.ProfileName)
	}
	out := stderr.String()
	if !strings.Contains(out, "Configured: Claude CLI subscription (claude-cli)") || !strings.Contains(out, "Custom compatible runtime") {
		t.Fatalf("stderr = %q, want mixed configured/custom runtime options", out)
	}
	if !strings.Contains(out, "LLM provider") || !strings.Contains(out, "LLM auth mode") || !strings.Contains(out, "LLM adapter") {
		t.Fatalf("stderr = %q, want custom runtime fields", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"7", // Back to main menu.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	_, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		DefaultProfileName:   "work",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditLLMRuntime error = %v, want errInitNavigateBack", err)
	}
	if !strings.Contains(stderr.String(), "Back to main menu") {
		t.Fatalf("stderr = %q, want focused runtime Back option", stderr.String())
	}
}

func TestHuhInitLLMRuntimeDetailsBackDoesNotMutateDraft(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", "work", nil)
	draft.LLMProvider = string(config.LLMProviderOpenAI)
	draft.LLMAuth = string(config.LLMAuthSubscription)
	draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
	want := draft
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Back to runtime choices.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	back, err := prompter.editLLMRuntimeDetails(&draft)
	if err != nil {
		t.Fatalf("editLLMRuntimeDetails: %v", err)
	}
	if !back {
		t.Fatal("back = false, want details Back")
	}
	if !reflect.DeepEqual(draft, want) {
		t.Fatalf("draft mutated on details Back:\n got: %#v\nwant: %#v", draft, want)
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"4", // Back to main menu.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	_, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		DefaultProfileName:   "work",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditReviewerEntity error = %v, want errInitNavigateBack", err)
	}
	if !strings.Contains(stderr.String(), "Back to main menu") {
		t.Fatalf("stderr = %q, want focused reviewer Back option", stderr.String())
	}
}

func TestHuhInitReviewerEntityDetailsBackDoesNotMutateDraft(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", "work", nil)
	draft.ReviewerEnabled = true
	draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	draft.ReviewerCredentialRef = "codereview/work-reviewer"
	want := draft
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Back to reviewer choices.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	back, err := prompter.editReviewerEntityDetails(&draft)
	if err != nil {
		t.Fatalf("editReviewerEntityDetails: %v", err)
	}
	if !back {
		t.Fatal("back = false, want details Back")
	}
	if !reflect.DeepEqual(draft, want) {
		t.Fatalf("draft mutated on details Back:\n got: %#v\nwant: %#v", draft, want)
	}
}

func TestInitInventorySelectionsApplyToDraft(t *testing.T) {
	draft := seedInteractiveInitDraft("default", "", "", nil)
	gitScopes := map[string]initGitScopeDraft{
		"gitlab-work": {
			Name:          "gitlab-work",
			Host:          "gitlab.com",
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: "codereview/work-git",
		},
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"work-app": {
			Name:          "work-app",
			Kind:          initReviewerEntityKindGitHubApp,
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: "codereview/work-reviewer",
		},
	}
	llmRuntimes := map[string]initLLMRuntimeDraft{
		"openai-work": {
			Name:          "openai-work",
			Preset:        initLLMRuntimePresetOpenAIAPIKey,
			Provider:      config.LLMProviderOpenAI,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterOpenAIAPI,
			CredentialRef: "codereview/work-llm",
		},
	}

	applyGitScopeSelection(&draft, "gitlab-work", gitScopes)
	applyReviewerEntityInventorySelection(&draft, "work-app", reviewerEntities)
	applyLLMRuntimeInventorySelection(&draft, "openai-work", llmRuntimes)

	if draft.GitHost != "gitlab.com" || draft.GitAuth != string(config.GitAuthModeGitHubApp) || draft.GitCredentialRef != "codereview/work-git" {
		t.Fatalf("git draft = %#v, want selected gitlab scope values", draft)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) || draft.ReviewerCredentialRef != "codereview/work-reviewer" {
		t.Fatalf("reviewer draft = %#v, want selected github app reviewer", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) || draft.LLMCredentialRef != "codereview/work-llm" {
		t.Fatalf("llm draft = %#v, want selected openai api-key runtime", draft)
	}
}

func TestCustomLLMRuntimeSelectionUsesEditedProviderAuthAndAdapter(t *testing.T) {
	draft := seedInteractiveInitDraft("work", "work", "work", nil)
	draft.LLMProvider = string(config.LLMProviderOpenAI)
	draft.LLMAuth = string(config.LLMAuthAPIKey)
	draft.LLMAdapter = string(config.LLMAdapterOpenAIAPI)

	applyLLMRuntimeInventorySelection(&draft, initCustomLLMRuntimeSelection, map[string]initLLMRuntimeDraft{
		"claude-cli": {
			Name:     "claude-cli",
			Preset:   initLLMRuntimePresetClaudeCLISubscription,
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	})
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)

	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) {
		t.Fatalf("draft = %#v, want edited openai api runtime retained for custom selection", draft)
	}
}

func TestInitLLMRuntimeOptionsDistinguishConfiguredAndTemplateLabels(t *testing.T) {
	options := initLLMRuntimeOptions(map[string]initLLMRuntimeDraft{
		"claude-cli": {
			Name:     "claude-cli",
			Preset:   initLLMRuntimePresetClaudeCLISubscription,
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	})
	var configuredLabel string
	var templateLabel string
	for _, option := range options {
		switch option.Value {
		case "claude-cli":
			configuredLabel = option.Key
		case string(initLLMRuntimePresetClaudeCLISubscription):
			templateLabel = option.Key
		}
	}
	if configuredLabel == "" || templateLabel == "" {
		t.Fatalf("labels = %q / %q, want both configured and template options present", configuredLabel, templateLabel)
	}
	if configuredLabel == templateLabel {
		t.Fatalf("configured/template labels = %q / %q, want distinct labels", configuredLabel, templateLabel)
	}
}

func TestHuhInitPrompterAccessibleAdvancedStorageLabelsExposeRefInputs(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",  // Profile name
			"",  // Make default
			"",  // Git scope host
			"",  // Git scope auth mode
			"2", // Reviewer entity: PAT reviewer
			"4", // LLM runtime: Anthropic API key
			"",  // Reviewer model tier
			"y", // Advanced storage labels
			"",  // Git storage label
			"",  // Reviewer storage label
			"",  // LLM storage label
			"",
		}, "\n")),
		stderr: &stderr,
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		DefaultProfileName:   "",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Advanced storage labels") || !strings.Contains(out, "Git storage label") || !strings.Contains(out, "LLM storage label") {
		t.Fatalf("wizard output missing advanced storage label prompts: %q", out)
	}
	_ = draft
}

func TestHuhInitPrompterAccessibleShowsExistingProfileHealthWarnings(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Advanced storage labels
			"",
		}, "\n")),
		stderr: &stderr,
	}

	_, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		DefaultProfileName:   "work",
		ExistingConfig:       config.File{DefaultProfile: "work", Profiles: map[string]config.Profile{"work": existing}},
		ProfileWarnings: map[string][]string{
			"work": {"Git secret health: codereview/work is missing required keys (git_token)"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Existing profile secret health") || !strings.Contains(out, "missing required keys") {
		t.Fatalf("wizard output missing health warning banner: %q", out)
	}
}

func TestHuhInitPrompterAccessibleRendersComputedProfileHealthWarnings(t *testing.T) {
	t.Setenv("TERM", "dumb")
	opts := &root.Options{
		Backend: "file",
	}
	existing := basicProfile("work")
	ctx, err := buildInteractiveInitPromptContext(&cobra.Command{}, opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
	}, initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		DefaultProfileName:   "work",
		ExistingConfig:       config.File{DefaultProfile: "work", Profiles: map[string]config.Profile{"work": existing}},
	})
	if err != nil {
		t.Fatalf("buildInteractiveInitPromptContext: %v", err)
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Advanced storage labels
			"",
		}, "\n")),
		stderr: &stderr,
	}

	_, err = prompter.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Existing profile secret health") || !strings.Contains(out, "codereview/work is missing required keys (git_token)") {
		t.Fatalf("wizard output missing computed health warning banner: %q", out)
	}
}

func TestBuildInteractiveInitPromptContextReportsCannotVerifyWarnings(t *testing.T) {
	opts := &root.Options{
		Backend: "file",
	}
	existing := basicProfile("work")
	ctx, err := buildInteractiveInitPromptContext(&cobra.Command{}, opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			return nil, fmt.Errorf("keyring unavailable")
		},
	}, initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		DefaultProfileName:   "work",
		ExistingConfig:       config.File{DefaultProfile: "work", Profiles: map[string]config.Profile{"work": existing}},
	})
	if err != nil {
		t.Fatalf("buildInteractiveInitPromptContext: %v", err)
	}
	warnings := ctx.ProfileWarnings["work"]
	if len(warnings) == 0 {
		t.Fatalf("ProfileWarnings = %#v, want cannot verify warning", ctx.ProfileWarnings)
	}
	if !strings.Contains(warnings[0], "cannot verify") {
		t.Fatalf("warning = %q, want cannot verify wording", warnings[0])
	}
}

func TestHuhInitModelMapPrompterAccessibleShowsTierInputs(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitModelMapPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2",          // Edit tier mappings
			"",           // Edit model tier details
			"",           // small stays blank
			"gpt-custom", // medium override
			"",           // large stays blank
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderOpenAI,
			Adapter:  config.LLMAdapterCodexCLI,
		},
		ModelMap: nil,
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	out := stderr.String()
	if !strings.Contains(out, "small model") || !strings.Contains(out, "medium model") || !strings.Contains(out, "large model") {
		t.Fatalf("stderr = %q, want tier input prompts", out)
	}
	if !strings.Contains(out, "Back to model-map choices") {
		t.Fatalf("stderr = %q, want model-map detail Back option", out)
	}
}

func TestHuhInitModelMapPrompterAccessibleKeepsExistingOverrideWhenLeftBlank(t *testing.T) {
	t.Setenv("TERM", "dumb")
	prompter := huhInitModelMapPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Edit tier mappings
			"",  // Edit model tier details
			"",  // small stays blank
			"",  // medium keeps existing prefilled value
			"",  // large stays blank
			"",
		}, "\n")),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ModelMap: config.ModelMap{"medium": "claude-custom"},
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if !reflect.DeepEqual(edit.ModelMap, config.ModelMap{"medium": "claude-custom"}) {
		t.Fatalf("edit.ModelMap = %#v, want preserved existing override", edit.ModelMap)
	}
}

func TestHuhInitModelMapPrompterAccessibleShowsResetOptionForExistingOverrides(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitModelMapPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ModelMap: config.ModelMap{"medium": "claude-custom"},
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if edit.Apply {
		t.Fatalf("edit.Apply = %v, want preserve path", edit.Apply)
	}
	if !strings.Contains(stderr.String(), "Reset all overrides to built-in defaults") {
		t.Fatalf("stderr = %q, want reset option when overrides exist", stderr.String())
	}
}

func TestHuhInitAgentSourcesPrompterAccessibleShowsEditablePaths(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitAgentSourcesPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Edit agent source paths
			"",  // Edit agent source details
			"",  // keep first prefilled path
			"",  // no additional paths
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditAgentSources(initAgentSourcesPrompt{
		Sources: []string{"/tmp/agents"},
	})
	if err != nil {
		t.Fatalf("EditAgentSources: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	if !reflect.DeepEqual(edit.Sources, []string{"/tmp/agents"}) {
		t.Fatalf("edit.Sources = %#v, want preserved source", edit.Sources)
	}
	out := stderr.String()
	if !strings.Contains(out, "Agent source 1") || !strings.Contains(out, "Add agent sources") {
		t.Fatalf("stderr = %q, want editable agent source inputs", out)
	}
	if !strings.Contains(out, "Back to agent-source choices") {
		t.Fatalf("stderr = %q, want agent-source detail Back option", out)
	}
}

func TestHuhInitReviewPolicyPrompterAccessibleShowsFields(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitReviewPolicyPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2",   // Edit review policy
			"",    // Edit review policy details
			"",    // keep comment
			"n",   // allow self-approve false
			"2",   // auto resolve
			"24h", // resolve after
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditReviewPolicy(initReviewPolicyPrompt{
		ReviewPolicy: config.ReviewPolicy{},
	})
	if err != nil {
		t.Fatalf("EditReviewPolicy: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	if edit.ReviewPolicy.MajorEvent != config.ReviewMajorEventComment {
		t.Fatalf("review policy = %#v, want default comment major_event", edit.ReviewPolicy)
	}
	out := stderr.String()
	if !strings.Contains(out, "Major findings event") || !strings.Contains(out, "Resolve-after duration") {
		t.Fatalf("stderr = %q, want review-policy fields", out)
	}
	if !strings.Contains(out, "Back to review-policy choices") {
		t.Fatalf("stderr = %q, want review-policy detail Back option", out)
	}
}

func TestHuhInitRoutesPrompterAccessibleShowsRouteEditor(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitRoutesPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Edit repository routes
			"",  // Edit repository route details
			"",  // keep existing route text
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName: "work",
		ProfileHost: "github.com",
		Routes: []configedit.RepositoryRouteSpec{{
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		}},
	})
	if err != nil {
		t.Fatalf("EditRoutes: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	if len(edit.Routes) != 1 || edit.Routes[0].Namespace != "open-cli-collective" {
		t.Fatalf("routes = %#v, want preserved route", edit.Routes)
	}
	out := stderr.String()
	if !strings.Contains(out, "Repository routes") || !strings.Contains(out, "Edit repository routes") {
		t.Fatalf("stderr = %q, want route editor prompt", out)
	}
	if !strings.Contains(out, "Back to repository-route choices") {
		t.Fatalf("stderr = %q, want repository-route detail Back option", out)
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
	existing.Git.AuthMode = config.GitAuthModeGitHubApp
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
				GitAuth:               string(config.GitAuthModeGitHubApp),
				GitCredentialRef:      "codereview/office-git",
				ReviewerEnabled:       true,
				ReviewerAuth:          string(config.GitAuthModeGitHubApp),
				ReviewerCredentialRef: "codereview/custom-office-reviewer",
				LLMProvider:           string(config.LLMProviderOpenAI),
				LLMAuth:               string(config.LLMAuthAPIKey),
				LLMAdapter:            string(config.LLMAdapterOpenAIAPI),
				LLMReviewerModelTier:  string(config.ModelTierSmall),
				LLMCredentialRef:      "codereview/custom-office-llm",
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read during deferred interactive init")
			return "", nil
		},
		configPath: func(*root.Options) (string, error) {
			return path, nil
		},
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
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
	if profile.Git.AuthMode != config.GitAuthModeGitHubApp {
		t.Fatalf("git auth_mode = %q, want github_app", profile.Git.AuthMode)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.CredentialRef != "codereview/custom-office-reviewer" {
		t.Fatalf("reviewer ref = %#v, want preserved custom-office-reviewer", profile.ReviewerCredentials)
	}
	if profile.LLM.CredentialRef != "codereview/custom-office-llm" {
		t.Fatalf("llm ref = %q, want custom-office-llm", profile.LLM.CredentialRef)
	}
	if profile.Git.IdentityCache != "" {
		t.Fatalf("git identity cache = %q, want cleared after git scope change", profile.Git.IdentityCache)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.AuthMode != config.GitAuthModeGitHubApp || profile.ReviewerCredentials.IdentityCache != "" {
		t.Fatalf("reviewer credentials = %#v, want github_app with cleared cache after entity change", profile.ReviewerCredentials)
	}
	if !reflect.DeepEqual(profile.AgentSources, []string{"/tmp/agents"}) {
		t.Fatalf("agent_sources = %#v, want preserved", profile.AgentSources)
	}
	if !reflect.DeepEqual(profile.LLM.ModelMap, config.ModelMap{"medium": "gpt-custom"}) {
		t.Fatalf("model_map = %#v, want preserved", profile.LLM.ModelMap)
	}
	if profile.LLM.ReviewerModelTier != config.ModelTierSmall {
		t.Fatalf("reviewer_model_tier = %q, want small", profile.LLM.ReviewerModelTier)
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/custom-office-llm --key "+credentials.OpenAIAPIKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred llm follow-up hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/office-git --key "+credentials.GitHubAppIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want github app git follow-up hint", stderr.String())
	}
	if route := cfg.RepositoryProfiles[0]; route.Profile != "office" {
		t.Fatalf("repository route profile = %q, want office", route.Profile)
	}
}

func TestInitInteractiveModelMapPreserveUsesEditedLLMContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("work")
	existing.LLM.ModelMap = config.ModelMap{"medium": "claude-custom"}
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": existing},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	var gotPrompt initModelMapPrompt
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				OriginalProfileName:  "work",
				ProfileName:          "work",
				MakeDefault:          true,
				GitHost:              "github.com",
				GitAuth:              string(config.GitAuthModePAT),
				GitCredentialRef:     "codereview/work",
				LLMProvider:          string(config.LLMProviderOpenAI),
				LLMAuth:              string(config.LLMAuthSubscription),
				LLMAdapter:           string(config.LLMAdapterCodexCLI),
				LLMReviewerModelTier: "",
			}, nil
		}),
		modelMapPrompter: initModelMapPrompterFunc(func(prompt initModelMapPrompt) (initModelMapEdit, error) {
			gotPrompt = prompt
			return initModelMapEdit{}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if gotPrompt.LLM.Provider != config.LLMProviderOpenAI || gotPrompt.LLM.Adapter != config.LLMAdapterCodexCLI {
		t.Fatalf("model-map prompt llm = %#v, want edited openai/codex_cli", gotPrompt.LLM)
	}
	if !reflect.DeepEqual(gotPrompt.ModelMap, config.ModelMap{"medium": "claude-custom"}) {
		t.Fatalf("model-map prompt map = %#v, want preserved existing override", gotPrompt.ModelMap)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if !reflect.DeepEqual(cfg.Profiles["work"].LLM.ModelMap, config.ModelMap{"medium": "claude-custom"}) {
		t.Fatalf("saved model_map = %#v, want preserved override", cfg.Profiles["work"].LLM.ModelMap)
	}
}

func TestInitInteractiveModelMapAddEditRemoveAndReset(t *testing.T) {
	tests := []struct {
		name     string
		existing config.ModelMap
		edit     initModelMapEdit
		want     config.ModelMap
	}{
		{
			name:     "add",
			existing: nil,
			edit:     initModelMapEdit{Apply: true, ModelMap: config.ModelMap{"medium": "claude-custom"}},
			want:     config.ModelMap{"medium": "claude-custom"},
		},
		{
			name:     "edit",
			existing: config.ModelMap{"medium": "claude-old"},
			edit:     initModelMapEdit{Apply: true, ModelMap: config.ModelMap{"medium": "claude-new"}},
			want:     config.ModelMap{"medium": "claude-new"},
		},
		{
			name:     "remove one tier",
			existing: config.ModelMap{"small": "small-model", "medium": "medium-model"},
			edit:     initModelMapEdit{Apply: true, ModelMap: config.ModelMap{"small": "small-model"}},
			want:     config.ModelMap{"small": "small-model"},
		},
		{
			name:     "reset all",
			existing: config.ModelMap{"large": "large-model"},
			edit:     initModelMapEdit{Apply: true, ModelMap: nil},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			existing := basicProfile("work")
			existing.LLM.ModelMap = copyModelMap(tt.existing)
			existing.AgentSources = []string{"/tmp/agents"}
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Profiles:       map[string]config.Profile{"work": existing},
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
						OriginalProfileName:  "work",
						ProfileName:          "work",
						MakeDefault:          true,
						GitHost:              "github.com",
						GitAuth:              string(config.GitAuthModePAT),
						GitCredentialRef:     "codereview/work",
						LLMProvider:          string(config.LLMProviderAnthropic),
						LLMAuth:              string(config.LLMAuthSubscription),
						LLMAdapter:           string(config.LLMAdapterClaudeCLI),
						LLMReviewerModelTier: "",
					}, nil
				}),
				modelMapPrompter: initModelMapPrompterFunc(func(initModelMapPrompt) (initModelMapEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			expected := existing
			expected.LLM.ModelMap = copyModelMap(tt.want)
			expected.ReviewPolicy.MajorEvent = config.ReviewMajorEventComment
			if !reflect.DeepEqual(cfg.Profiles["work"], expected) {
				t.Fatalf("saved profile = %#v, want %#v", cfg.Profiles["work"], expected)
			}
		})
	}
}

func TestInitInteractiveModelMapRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		edit initModelMapEdit
		want string
	}{
		{
			name: "blank model",
			edit: initModelMapEdit{Apply: true, ModelMap: config.ModelMap{"medium": " \t "}},
			want: "model_map.medium is required",
		},
		{
			name: "invalid tier",
			edit: initModelMapEdit{Apply: true, ModelMap: config.ModelMap{"flagship": "gpt"}},
			want: `tier "flagship" is invalid`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			existing := basicProfile("work")
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Profiles:       map[string]config.Profile{"work": existing},
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				modelMapPrompter: initModelMapPrompterFunc(func(initModelMapPrompt) (initModelMapEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err == nil {
				t.Fatal("runInitWithDeps error = nil, want invalid model-map rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			cfg, loadErr := config.Load(path)
			if loadErr != nil {
				t.Fatalf("Load config: %v", loadErr)
			}
			if !reflect.DeepEqual(cfg.Profiles["work"].LLM.ModelMap, existing.LLM.ModelMap) {
				t.Fatalf("saved model_map = %#v, want unchanged %#v", cfg.Profiles["work"].LLM.ModelMap, existing.LLM.ModelMap)
			}
		})
	}
}

func TestInitInteractiveAgentSourcesPreserveAddRemoveAndReset(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		edit     initAgentSourcesEdit
		want     []string
	}{
		{
			name:     "add",
			existing: nil,
			edit:     initAgentSourcesEdit{Apply: true, Sources: []string{" ./agents "}},
			want:     []string{"agents"},
		},
		{
			name:     "remove one and add one",
			existing: []string{"/tmp/alpha", "/tmp/beta"},
			edit:     initAgentSourcesEdit{Apply: true, Sources: []string{"/tmp/beta", "/tmp/gamma"}},
			want:     []string{"/tmp/beta", "/tmp/gamma"},
		},
		{
			name:     "dedupe normalized",
			existing: []string{"/tmp/alpha"},
			edit:     initAgentSourcesEdit{Apply: true, Sources: []string{"/tmp/alpha", "/tmp/alpha/../alpha"}},
			want:     []string{"/tmp/alpha"},
		},
		{
			name:     "reset all",
			existing: []string{"/tmp/alpha"},
			edit:     initAgentSourcesEdit{Apply: true, Sources: nil},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			existing := basicProfile("work")
			existing.AgentSources = append([]string(nil), tt.existing...)
			existing.ReviewPolicy = config.ReviewPolicy{
				MajorEvent:       config.ReviewMajorEventRequestChanges,
				AllowSelfApprove: true,
				ResolveThreads:   config.ResolveThreadsNever,
				ResolveAfter:     "24h",
			}
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Profiles:       map[string]config.Profile{"work": existing},
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				agentSourcesPrompter: initAgentSourcesPrompterFunc(func(initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			if !reflect.DeepEqual(cfg.Profiles["work"].AgentSources, tt.want) {
				t.Fatalf("agent_sources = %#v, want %#v", cfg.Profiles["work"].AgentSources, tt.want)
			}
			if !reflect.DeepEqual(cfg.Profiles["work"].ReviewPolicy, existing.ReviewPolicy) {
				t.Fatalf("review_policy = %#v, want preserved %#v", cfg.Profiles["work"].ReviewPolicy, existing.ReviewPolicy)
			}
		})
	}
}

func TestInitInteractiveAgentSourcesCanClearToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
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
				MakeDefault:         true,
				GitHost:             "github.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		agentSourcesPrompter: initAgentSourcesPrompterFunc(func(initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
			return initAgentSourcesEdit{Apply: true, Sources: []string{" \t ", ""}}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, loadErr := config.Load(path)
	if loadErr != nil {
		t.Fatalf("Load config: %v", loadErr)
	}
	if cfg.Profiles["work"].AgentSources != nil {
		t.Fatalf("agent_sources = %#v, want cleared empty slice semantics", cfg.Profiles["work"].AgentSources)
	}
}

func TestInitInteractiveReviewPolicyPreserveEditAndDefaults(t *testing.T) {
	tests := []struct {
		name     string
		existing config.ReviewPolicy
		edit     initReviewPolicyEdit
		want     config.ReviewPolicy
	}{
		{
			name: "set request changes policy",
			edit: initReviewPolicyEdit{Apply: true, ReviewPolicy: config.ReviewPolicy{
				MajorEvent:       config.ReviewMajorEventRequestChanges,
				AllowSelfApprove: true,
				ResolveThreads:   config.ResolveThreadsNever,
				ResolveAfter:     "24h",
			}},
			want: config.ReviewPolicy{
				MajorEvent:       config.ReviewMajorEventRequestChanges,
				AllowSelfApprove: true,
				ResolveThreads:   config.ResolveThreadsNever,
				ResolveAfter:     "24h",
			},
		},
		{
			name: "reset major event and self approve default",
			existing: config.ReviewPolicy{
				MajorEvent:       config.ReviewMajorEventRequestChanges,
				AllowSelfApprove: true,
				ResolveThreads:   config.ResolveThreadsAuto,
				ResolveAfter:     "1h",
			},
			edit: initReviewPolicyEdit{Apply: true, ReviewPolicy: config.ReviewPolicy{
				MajorEvent:       config.ReviewMajorEventComment,
				AllowSelfApprove: false,
			}},
			want: config.ReviewPolicy{
				MajorEvent: config.ReviewMajorEventComment,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			existing := basicProfile("work")
			existing.AgentSources = []string{"/tmp/agents"}
			existing.ReviewPolicy = tt.existing
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Profiles:       map[string]config.Profile{"work": existing},
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				reviewPolicyPrompter: initReviewPolicyPrompterFunc(func(initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			if !reflect.DeepEqual(cfg.Profiles["work"].ReviewPolicy, tt.want) {
				t.Fatalf("review_policy = %#v, want %#v", cfg.Profiles["work"].ReviewPolicy, tt.want)
			}
			if !reflect.DeepEqual(cfg.Profiles["work"].AgentSources, existing.AgentSources) {
				t.Fatalf("agent_sources = %#v, want preserved %#v", cfg.Profiles["work"].AgentSources, existing.AgentSources)
			}
		})
	}
}

func TestInitInteractiveReviewPolicyRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		edit initReviewPolicyEdit
		want string
	}{
		{
			name: "invalid major_event",
			edit: initReviewPolicyEdit{Apply: true, ReviewPolicy: config.ReviewPolicy{
				MajorEvent: "flag",
			}},
			want: `review_policy.major_event "flag" is invalid`,
		},
		{
			name: "invalid resolve_threads",
			edit: initReviewPolicyEdit{Apply: true, ReviewPolicy: config.ReviewPolicy{
				MajorEvent:     config.ReviewMajorEventComment,
				ResolveThreads: "always",
			}},
			want: `review_policy.resolve_threads "always" is invalid`,
		},
		{
			name: "invalid resolve_after",
			edit: initReviewPolicyEdit{Apply: true, ReviewPolicy: config.ReviewPolicy{
				MajorEvent:   config.ReviewMajorEventComment,
				ResolveAfter: "tomorrow",
			}},
			want: `review_policy.resolve_after "tomorrow" is invalid`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Profiles:       map[string]config.Profile{"work": basicProfile("work")},
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				reviewPolicyPrompter: initReviewPolicyPrompterFunc(func(initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err == nil {
				t.Fatal("runInitWithDeps error = nil, want invalid review-policy rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestHuhInitRetentionPrompterAccessibleShowsFields(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitRetentionPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Edit retention settings
			"",  // Edit retention details
			"",  // keep default 90 days
			"",  // custom days unused
			"2", // manual only
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditRetention(initRetentionPrompt{Retention: config.RetentionConfig{}})
	if err != nil {
		t.Fatalf("EditRetention: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	out := stderr.String()
	if !strings.Contains(out, "Maximum run-data age") || !strings.Contains(out, "Retention enforcement") {
		t.Fatalf("stderr = %q, want retention fields", out)
	}
	if !strings.Contains(out, "Back to retention choices") {
		t.Fatalf("stderr = %q, want retention detail Back option", out)
	}
}

func TestHuhInitKeyringBackendPrompterAccessibleShowsField(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2",    // Set keyring backend
			"",     // Set keyring backend details
			"file", // Persistent backend
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{})
	if err != nil {
		t.Fatalf("EditKeyringBackend: %v", err)
	}
	if !edit.Apply {
		t.Fatalf("edit = %#v, want apply edit path", edit)
	}
	if !strings.Contains(stderr.String(), "Persistent keyring backend") {
		t.Fatalf("stderr = %q, want backend input prompt", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Back to keyring choices") {
		t.Fatalf("stderr = %q, want keyring detail Back option", stderr.String())
	}
}

func TestInitInteractiveProfileSubflowBackPreservesBuiltWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionReviewProfiles,
			initMenuActionExit,
		},
	}
	deps := initDeps{
		menuPrompter: menu,
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				OriginalProfileName: "work",
				ProfileName:         "work",
				MakeDefault:         true,
				GitHost:             "github.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(initRoutesPrompt) (initRoutesEdit, error) {
			return initRoutesEdit{}, errInitNavigateBack
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig should not run after exiting without save")
			return nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(menu.prompts) < 2 {
		t.Fatalf("menu prompts = %#v, want prompt after profile subflow Back", menu.prompts)
	}
	got := menu.prompts[1]
	if got.ActiveProfileName != "work" || !got.CanSave || got.ReviewProfileCount != 1 {
		t.Fatalf("post-Back menu prompt = %#v, want active work workspace preserved", got)
	}
}

func TestInitInteractiveGlobalKeyringBackPreservesRetentionDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionGlobalSettings,
			initMenuActionSave,
		},
	}
	deps := initDeps{
		menuPrompter: menu,
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{
				Apply: true,
				Retention: config.RetentionConfig{
					MaxAgeDays:  intPtr(45),
					Enforcement: config.RetentionManualOnly,
				},
			}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{}, errInitNavigateBack
		}),
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Data.Retention.MaxAgeDaysValue() != 45 || cfg.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("retention = %#v, want retained 45/manual after keyring Back", cfg.Data.Retention)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatalf("profiles = %#v, want work profile preserved", cfg.Profiles)
	}
}

func TestInitInteractiveRetentionPreserveEditResetAndExplicitZero(t *testing.T) {
	keepForever := 0
	tests := []struct {
		name     string
		existing config.RetentionConfig
		edit     initRetentionEdit
		wantDays int
		wantMode config.RetentionEnforcement
	}{
		{
			name: "preserve explicit zero",
			existing: config.RetentionConfig{
				MaxAgeDays:  &keepForever,
				Enforcement: config.RetentionManualOnly,
			},
			edit:     initRetentionEdit{Apply: false},
			wantDays: 0,
			wantMode: config.RetentionManualOnly,
		},
		{
			name: "edit custom",
			edit: initRetentionEdit{Apply: true, Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(45),
				Enforcement: config.RetentionManualOnly,
			}},
			wantDays: 45,
			wantMode: config.RetentionManualOnly,
		},
		{
			name: "reset defaults",
			existing: config.RetentionConfig{
				MaxAgeDays:  &keepForever,
				Enforcement: config.RetentionManualOnly,
			},
			edit:     initRetentionEdit{Apply: true, Retention: config.DefaultRetentionConfig()},
			wantDays: 90,
			wantMode: config.RetentionAtWrite,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			existing := basicProfile("work")
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Keyring:        config.KeyringConfig{Backend: "file"},
				Profiles:       map[string]config.Profile{"work": existing},
				Data:           config.DataConfig{Retention: tt.existing},
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			if cfg.Data.Retention.MaxAgeDaysValue() != tt.wantDays || cfg.Data.Retention.Enforcement != tt.wantMode {
				t.Fatalf("retention = %#v, want %d/%s", cfg.Data.Retention, tt.wantDays, tt.wantMode)
			}
			if cfg.Keyring.Backend != "file" {
				t.Fatalf("keyring.backend = %q, want preserved file", cfg.Keyring.Backend)
			}
		})
	}
}

func TestInitInteractiveKeyringBackendPreserveSetResetAndConflict(t *testing.T) {
	tests := []struct {
		name        string
		existing    string
		edit        initKeyringBackendEdit
		runtime     string
		wantBackend string
		wantHint    string
		wantErr     string
	}{
		{
			name:        "preserve existing",
			existing:    "file",
			edit:        initKeyringBackendEdit{Apply: false},
			wantBackend: "file",
		},
		{
			name:        "set backend",
			edit:        initKeyringBackendEdit{Apply: true, Backend: "memory"},
			wantBackend: "memory",
		},
		{
			name:        "reset runtime only backend keeps hint",
			existing:    "file",
			edit:        initKeyringBackendEdit{Apply: true, Backend: ""},
			runtime:     "memory",
			wantBackend: "",
			wantHint:    "--backend memory",
		},
		{
			name:    "runtime conflict",
			edit:    initKeyringBackendEdit{Apply: true, Backend: "file"},
			runtime: "memory",
			wantErr: `--backend "memory" conflicts with selected keyring.backend "file"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile: "work",
				Keyring:        config.KeyringConfig{Backend: tt.existing},
				Profiles:       map[string]config.Profile{"work": basicProfile("work")},
			})
			cmd := &cobra.Command{}
			cmd.Flags().String(credstore.BackendFlagName, "", "")
			opts := &root.Options{
				Backend:    tt.runtime,
				Stdin:      strings.NewReader(""),
				Stdout:     &bytes.Buffer{},
				Stderr:     &bytes.Buffer{},
				ConfigPath: path,
			}
			if tt.runtime != "" {
				if err := cmd.Flags().Set(credstore.BackendFlagName, tt.runtime); err != nil {
					t.Fatalf("set backend flag: %v", err)
				}
			}
			var stderr bytes.Buffer
			opts.Stderr = &stderr
			deps := initDeps{
				prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
					return initDraft{
						OriginalProfileName: "work",
						ProfileName:         "work",
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderOpenAI),
						LLMAuth:             string(config.LLMAuthAPIKey),
						LLMAdapter:          string(config.LLMAdapterOpenAIAPI),
						LLMCredentialRef:    "codereview/work-llm",
					}, nil
				}),
				keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
					return tt.edit, nil
				}),
				secretPrompter: &fakeInitSecretPrompter{
					actions: []initCredentialSecretAction{initCredentialSecretActionDefer, initCredentialSecretActionDefer},
				},
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
				openStore: func(string, bool, config.File) (initStore, error) {
					return newFakeInitStore(nil), nil
				},
			}

			err := runInitWithDeps(cmd, opts, initOptions{}, deps)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			if cfg.Keyring.Backend != tt.wantBackend {
				t.Fatalf("keyring.backend = %q, want %q", cfg.Keyring.Backend, tt.wantBackend)
			}
			if tt.wantHint != "" && !strings.Contains(stderr.String(), tt.wantHint) {
				t.Fatalf("stderr = %q, want %q hint", stderr.String(), tt.wantHint)
			}
		})
	}
}

func TestBuildInteractiveInitMenuPromptNoWorkspaceDisablesProfileDependentActions(t *testing.T) {
	prompt := buildInteractiveInitMenuPrompt(initSessionDraft{
		cfg: config.File{Profiles: map[string]config.Profile{}},
	})
	if prompt.CanConfigureLLM || prompt.CanConfigureReviewer || prompt.CanSave {
		t.Fatalf("prompt = %#v, want LLM/reviewer/save disabled without a workspace", prompt)
	}
	if prompt.ReviewProfileCount != 0 || prompt.LLMRuntimeCount != 0 || prompt.ReviewerEntityCount != 0 {
		t.Fatalf("prompt counts = %#v, want zero counts without a workspace", prompt)
	}
}

func TestValidateInteractiveInitGlobalConfigWithoutProfilesStillValidatesKeyringBackend(t *testing.T) {
	err := validateInteractiveInitGlobalConfig(config.File{
		Keyring:  config.KeyringConfig{Backend: "definitely-not-a-backend"},
		Profiles: map[string]config.Profile{},
	})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid for bad keyring backend without profiles", err)
	}
}

func TestBuildInteractiveInitMenuPromptNoWorkspaceStillShowsExistingInventoryCounts(t *testing.T) {
	work := basicProfile("work")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	home := basicProfile("home")
	home.LLM = config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterOpenAIAPI,
		CredentialRef: "codereview/home-llm",
	}
	prompt := buildInteractiveInitMenuPrompt(initSessionDraft{
		cfg: config.File{
			Profiles: map[string]config.Profile{
				"home": home,
				"work": work,
			},
		},
	})
	if prompt.CanConfigureLLM || prompt.CanConfigureReviewer || prompt.CanSave {
		t.Fatalf("prompt = %#v, want actions disabled without active workspace", prompt)
	}
	if prompt.LLMRuntimeCount != 2 || prompt.ReviewerEntityCount != 2 || prompt.ReviewProfileCount != 2 {
		t.Fatalf("prompt counts = %#v, want existing inventory counts from session cfg", prompt)
	}
}

func TestHuhInitMenuPrompterAccessibleShowsMenuEntries(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin:  strings.NewReader("6\n"),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{
		HasWorkspace:        false,
		LLMRuntimeCount:     2,
		ReviewerEntityCount: 3,
		ReviewProfileCount:  1,
	})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action != initMenuActionExit {
		t.Fatalf("action = %q, want exit", action)
	}
	out := stderr.String()
	for _, want := range []string{
		"Configure LLM runtimes (2)",
		"Configure reviewer entities (3)",
		"Configure review profiles (1)",
		"Review global settings",
		"Save and exit",
		"Exit without saving",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr = %q, want menu item %q", out, want)
		}
	}
}

func TestHuhInitMenuPrompterAccessibleRejectsDisabledSaveUntilProfileExists(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"5", // Save and exit (disabled)
			"6", // Exit without saving
			"",
		}, "\n")),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action == initMenuActionSave {
		t.Fatalf("action = %q, want disabled save selection to be rejected", action)
	}
	if !strings.Contains(stderr.String(), "configure a review profile before saving") {
		t.Fatalf("stderr = %q, want disabled-save validation message", stderr.String())
	}
}

func TestHuhInitMenuPrompterAccessibleRejectsDisabledLLMUntilProfileExists(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"1", // Configure LLM runtimes (disabled)
			"6", // Exit without saving
			"",
		}, "\n")),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action == initMenuActionLLMRuntimes {
		t.Fatalf("action = %q, want disabled LLM selection to be rejected", action)
	}
	if !strings.Contains(stderr.String(), "configure a review profile before editing LLM runtimes") {
		t.Fatalf("stderr = %q, want disabled-llm validation message", stderr.String())
	}
}

func TestInitInteractiveMenuExitWithoutSaveLeavesConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	original := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	}
	saveCredentialTestConfig(t, path, original)
	wantCfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load initial config: %v", err)
	}
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	openStoreCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{initMenuActionExit},
		},
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			openStoreCalls++
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if !reflect.DeepEqual(cfg, wantCfg) {
		t.Fatalf("config after exit-without-save = %#v, want %#v", cfg, wantCfg)
	}
	if openStoreCalls != 0 {
		t.Fatalf("openStoreCalls = %d, want 0 on exit-without-save", openStoreCalls)
	}
}

func TestInitInteractiveMenuCarriesGlobalSettingsIntoFirstProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	retention := config.RetentionConfig{
		MaxAgeDays:  intPtr(45),
		Enforcement: config.RetentionManualOnly,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:          "default",
				MakeDefault:          true,
				GitHost:              "github.com",
				GitAuth:              string(config.GitAuthModePAT),
				LLMProvider:          string(config.LLMProviderAnthropic),
				LLMAuth:              string(config.LLMAuthSubscription),
				LLMAdapter:           string(config.LLMAdapterClaudeCLI),
				ReviewerEnabled:      false,
				ReviewerAuth:         string(config.GitAuthModePAT),
				LLMReviewerModelTier: "",
			}, nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{Apply: true, Retention: retention}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true, Backend: "file"}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionKeep},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"default": {credentials.GitTokenKey: "existing-token"},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Fatalf("default profile = %q, want default", cfg.DefaultProfile)
	}
	if cfg.Keyring.Backend != "file" {
		t.Fatalf("keyring.backend = %q, want file", cfg.Keyring.Backend)
	}
	if cfg.Data.Retention.MaxAgeDaysValue() != 45 || cfg.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("retention = %#v, want 45/manual_only", cfg.Data.Retention)
	}
}

func TestInitInteractiveMenuCanCreateMultipleProfilesBeforeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	prompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				if len(ctx.ExistingProfileNames) != 0 {
					t.Fatalf("first prompt ExistingProfileNames = %#v, want empty", ctx.ExistingProfileNames)
				}
				return initDraft{
					ProfileName: "home",
					MakeDefault: true,
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home"}) {
					t.Fatalf("second prompt ExistingProfileNames = %#v, want [home]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "home" || ctx.ExistingProfileName != "home" {
					t.Fatalf("second prompt active profile = (%q, %q), want home/home", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				if _, ok := ctx.ExistingConfig.Profiles["home"]; !ok {
					t.Fatalf("second prompt ExistingConfig = %#v, want persisted unsaved home profile", ctx.ExistingConfig.Profiles)
				}
				return initDraft{
					ProfileName: "work",
					MakeDefault: false,
					GitHost:     "github.company.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got := sortedProfileNames(cfg.Profiles); !reflect.DeepEqual(got, []string{"home", "work"}) {
		t.Fatalf("profiles = %#v, want [home work]", got)
	}
	if cfg.DefaultProfile != "home" {
		t.Fatalf("default profile = %q, want home", cfg.DefaultProfile)
	}
	if cfg.Profiles["work"].Git.Host != "github.company.com" {
		t.Fatalf("work git.host = %q, want github.company.com", cfg.Profiles["work"].Git.Host)
	}
}

func TestInitInteractiveMenuResumesUnsavedProfileAfterSwitchingProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	prompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionReviewProfiles,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				return initDraft{
					ProfileName: "work",
					MakeDefault: true,
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"work"}) {
					t.Fatalf("second prompt ExistingProfileNames = %#v, want [work]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "work" || ctx.ExistingProfileName != "work" {
					t.Fatalf("second prompt active profile = (%q, %q), want work/work", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				if ctx.DefaultProfileName != "work" {
					t.Fatalf("second prompt DefaultProfileName = %q, want work", ctx.DefaultProfileName)
				}
				if ctx.ExistingConfig.Profiles["work"].Git.Host != "github.com" {
					t.Fatalf("second prompt work profile = %#v, want first unsaved work draft in session cfg", ctx.ExistingConfig.Profiles["work"])
				}
				return initDraft{
					ProfileName: "home",
					MakeDefault: false,
					GitHost:     "gitlab.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 3:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home", "work"}) {
					t.Fatalf("third prompt ExistingProfileNames = %#v, want [home work]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "home" || ctx.ExistingProfileName != "home" {
					t.Fatalf("third prompt active profile = (%q, %q), want home/home after switching profiles", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				work := ctx.ExistingConfig.Profiles["work"]
				home := ctx.ExistingConfig.Profiles["home"]
				if work.Git.Host != "github.com" || home.Git.Host != "gitlab.com" {
					t.Fatalf("third prompt ExistingConfig = %#v, want both unsaved profiles available for resume", ctx.ExistingConfig.Profiles)
				}
				return initDraft{
					OriginalProfileName: "work",
					ProfileName:         "work",
					MakeDefault:         true,
					GitHost:             "github.enterprise.local",
					GitAuth:             string(config.GitAuthModePAT),
					LLMProvider:         string(config.LLMProviderAnthropic),
					LLMAuth:             string(config.LLMAuthSubscription),
					LLMAdapter:          string(config.LLMAdapterClaudeCLI),
				}, nil
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "work" {
		t.Fatalf("default profile = %q, want work", cfg.DefaultProfile)
	}
	if cfg.Profiles["work"].Git.Host != "github.enterprise.local" {
		t.Fatalf("work git.host = %q, want resumed update", cfg.Profiles["work"].Git.Host)
	}
	if cfg.Profiles["home"].Git.Host != "gitlab.com" {
		t.Fatalf("home git.host = %q, want persisted second profile", cfg.Profiles["home"].Git.Host)
	}
}

func TestInitInteractiveMenuFallbackDefaultPreservedWhenCreatingAnotherProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
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
	prompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls != 1 {
				t.Fatalf("unexpected prompter call %d", prompterCalls)
			}
			if ctx.RequestedProfileName != "work" || ctx.ExistingProfileName != "work" {
				t.Fatalf("prompt context = %#v, want fallback bootstrap from default work profile", ctx)
			}
			if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"work"}) {
				t.Fatalf("prompt ExistingProfileNames = %#v, want [work] from fallback bootstrap", ctx.ExistingProfileNames)
			}
			return initDraft{
				ProfileName: "home",
				MakeDefault: false,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "work" {
		t.Fatalf("default profile = %q, want existing fallback default preserved", cfg.DefaultProfile)
	}
	if got := sortedProfileNames(cfg.Profiles); !reflect.DeepEqual(got, []string{"home", "work"}) {
		t.Fatalf("profiles = %#v, want [home work]", got)
	}
}

func TestInitInteractiveMenuRenameDefaultProfileReconcilesRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		RepositoryProfiles: []config.RepositoryProfile{
			{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "open-cli-collective",
				},
			},
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
			"work": basicProfile("work"),
			"home": basicProfile("home"),
		},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			if ctx.ExistingProfileName != "work" {
				t.Fatalf("prompt context = %#v, want default work profile selected for rename", ctx)
			}
			return initDraft{
				OriginalProfileName: "work",
				ProfileName:         "office",
				MakeDefault:         true,
				GitHost:             "gitlab.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/custom-office-git",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if prompt.ProfileName != "office" || !prompt.HostChanged || prompt.PreviousHost != "github.com" || prompt.ProfileHost != "gitlab.com" {
				t.Fatalf("prompt = %#v, want renamed default profile reconciliation context", prompt)
			}
			return initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "gitlab.com",
				Namespace: "open-cli-collective",
			}}}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Fatalf("old profile still exists after rename: %#v", cfg.Profiles)
	}
	if cfg.DefaultProfile != "office" {
		t.Fatalf("default profile = %q, want renamed office default", cfg.DefaultProfile)
	}
	if len(cfg.RepositoryProfiles) != 2 {
		t.Fatalf("RepositoryProfiles = %#v, want renamed route plus unrelated preserved route", cfg.RepositoryProfiles)
	}
	routesByProfile := map[string]config.RepositoryProfile{}
	for _, route := range cfg.RepositoryProfiles {
		routesByProfile[route.Profile] = route
	}
	if routesByProfile["home"].Match.Host != "github.com" {
		t.Fatalf("home route = %#v, want unrelated home route preserved", routesByProfile["home"])
	}
	if routesByProfile["office"].Match.Host != "gitlab.com" {
		t.Fatalf("office route = %#v, want renamed reconciled route", routesByProfile["office"])
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimeRebuildsSecretPlanning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionReviewProfiles,
				initMenuActionLLMRuntimes,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
			draft.LLMProvider = string(config.LLMProviderOpenAI)
			draft.LLMAuth = string(config.LLMAuthAPIKey)
			draft.LLMAdapter = string(config.LLMAdapterOpenAIAPI)
			return draft, nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{Apply: true, Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(30),
				Enforcement: config.RetentionManualOnly,
			}}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true, Backend: "file"}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"default":     {credentials.GitTokenKey: "existing-token"},
				"default-llm": {credentials.OpenAIAPIKeyKey: "existing-openai-key"},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.LLM.Auth != config.LLMAuthAPIKey || profile.LLM.Adapter != config.LLMAdapterOpenAIAPI {
		t.Fatalf("llm profile = %#v, want openai api-key runtime", profile.LLM)
	}
	if profile.LLM.CredentialRef == "" {
		t.Fatalf("llm credential ref = %q, want generated ref after runtime rebuild", profile.LLM.CredentialRef)
	}
	if cfg.Keyring.Backend != "file" || cfg.Data.Retention.MaxAgeDaysValue() != 30 || cfg.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("global settings after runtime rebuild = %#v / %#v, want file + 30/manual_only", cfg.Keyring, cfg.Data.Retention)
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimePreservesUnrelatedProfileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	profile := basicProfile("work")
	profile.Git.Host = "gitlab.example.com"
	profile.Git.CredentialRef = "codereview/work-git"
	profile.Git.IdentityCache = "git-cache"
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		IdentityCache: "reviewer-cache",
	}
	profile.LLM.ModelMap = config.ModelMap{"medium": "claude-custom"}
	profile.LLM.ReviewerModelTier = config.ModelTierSmall
	profile.AgentSources = []string{"/tmp/agents"}
	profile.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	wantRoutes := []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "gitlab.example.com",
			Namespace: "team",
			Repos:     []string{"repo"},
		},
	}}
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile:     "work",
		RepositoryProfiles: wantRoutes,
		Profiles:           map[string]config.Profile{"work": profile},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionLLMRuntimes,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
			draft.LLMProvider = string(config.LLMProviderOpenAI)
			draft.LLMAuth = string(config.LLMAuthSubscription)
			draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"work-git":      {credentials.GitTokenKey: "existing-token"},
				"work-reviewer": {credentials.GitHubAppIDKey: "12345", credentials.GitHubAppPrivateKeyKey: "private-key"},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	got := cfg.Profiles["work"]
	if got.LLM.Provider != config.LLMProviderOpenAI || got.LLM.Adapter != config.LLMAdapterCodexCLI || got.LLM.Auth != config.LLMAuthSubscription {
		t.Fatalf("llm = %#v, want codex subscription runtime", got.LLM)
	}
	if got.LLM.CredentialRef != "" {
		t.Fatalf("llm credential ref = %q, want cleared for subscription runtime", got.LLM.CredentialRef)
	}
	if got.Git != profile.Git {
		t.Fatalf("git = %#v, want preserved %#v", got.Git, profile.Git)
	}
	if !reflect.DeepEqual(got.ReviewerCredentials, profile.ReviewerCredentials) {
		t.Fatalf("reviewer credentials = %#v, want preserved %#v", got.ReviewerCredentials, profile.ReviewerCredentials)
	}
	if !reflect.DeepEqual(got.AgentSources, profile.AgentSources) {
		t.Fatalf("agent_sources = %#v, want preserved %#v", got.AgentSources, profile.AgentSources)
	}
	if !reflect.DeepEqual(got.LLM.ModelMap, profile.LLM.ModelMap) {
		t.Fatalf("model_map = %#v, want preserved %#v", got.LLM.ModelMap, profile.LLM.ModelMap)
	}
	if got.LLM.ReviewerModelTier != profile.LLM.ReviewerModelTier {
		t.Fatalf("reviewer_model_tier = %q, want %q", got.LLM.ReviewerModelTier, profile.LLM.ReviewerModelTier)
	}
	if !reflect.DeepEqual(got.ReviewPolicy, profile.ReviewPolicy) {
		t.Fatalf("review_policy = %#v, want preserved %#v", got.ReviewPolicy, profile.ReviewPolicy)
	}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, wantRoutes) {
		t.Fatalf("repository_profiles = %#v, want preserved route %#v", cfg.RepositoryProfiles, wantRoutes)
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimeNoOpSkipsStoreOnSaveAndPersistsGlobalSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	wantProfile := basicProfile("work")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": wantProfile},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	openStoreCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionLLMRuntimes,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile), nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{Apply: true, Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(14),
				Enforcement: config.RetentionAtWrite,
			}}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true, Backend: "memory"}, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			openStoreCalls++
			return newFakeInitStore(nil), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if openStoreCalls != 0 {
		t.Fatalf("openStoreCalls = %d, want 0 for no-op focused runtime edit", openStoreCalls)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "memory" {
		t.Fatalf("keyring backend = %q, want memory", cfg.Keyring.Backend)
	}
	if cfg.Data.Retention.MaxAgeDaysValue() != 14 || cfg.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("retention = %#v, want 14/at_write", cfg.Data.Retention)
	}
	wantProfile.ReviewPolicy.MajorEvent = config.ReviewMajorEventComment
	if !reflect.DeepEqual(cfg.Profiles["work"], wantProfile) {
		t.Fatalf("profile = %#v, want unchanged %#v", cfg.Profiles["work"], wantProfile)
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimeDoesNotOpenStoreForPromptContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionLLMRuntimes,
				initMenuActionExit,
			},
		},
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile), nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run for focused llm prompt context")
			return nil, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
}

func TestInitInteractiveMenuFocusedBackKeepsSessionUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	original := config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	}
	saveCredentialTestConfig(t, path, original)
	wantCfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load original config: %v", err)
	}
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionLLMRuntimes,
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{}, errInitNavigateBack
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(initReviewerEntityPrompt) (initDraft, error) {
			return initDraft{}, errInitNavigateBack
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run for focused Back navigation")
			return nil, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig should not run after exiting without save")
			return nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if !reflect.DeepEqual(got, wantCfg) {
		t.Fatalf("config changed after focused Back navigation:\n got: %#v\nwant: %#v", got, wantCfg)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntityRebuildsSecretPlanning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionReviewProfiles,
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			return draft, nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{Apply: true, Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(14),
				Enforcement: config.RetentionAtWrite,
			}}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true, Backend: "memory"}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"default": {
					credentials.GitTokenKey: "existing-token",
				},
				"default-reviewer": {
					credentials.GitHubAppIDKey:         "12345",
					credentials.GitHubAppPrivateKeyKey: "private-key",
				},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want focused reviewer edit to enable separate reviewer")
	}
	if profile.ReviewerCredentials.AuthMode != config.GitAuthModeGitHubApp {
		t.Fatalf("reviewer credentials = %#v, want github app reviewer", profile.ReviewerCredentials)
	}
	if profile.ReviewerCredentials.CredentialRef == "" {
		t.Fatalf("reviewer credential ref = %q, want generated ref after reviewer rebuild", profile.ReviewerCredentials.CredentialRef)
	}
	if cfg.Keyring.Backend != "memory" || cfg.Data.Retention.MaxAgeDaysValue() != 14 || cfg.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("global settings after reviewer rebuild = %#v / %#v, want memory + 14/at_write", cfg.Keyring, cfg.Data.Retention)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntityDoesNotOpenStoreForPromptContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Profiles:       map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile), nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run for focused reviewer prompt context")
			return nil, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
}

func TestInitInteractiveMenuFinalSaveSummarizesDeferredNonActiveProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	prompterCalls := 0
	var finalizePrompt initFinalizePrompt
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			finalizePrompt = prompt
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls == 1 {
				return initDraft{
					ProfileName: "home",
					MakeDefault: true,
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			}
			return initDraft{
				ProfileName: "work",
				MakeDefault: false,
				GitHost:     "gitlab.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(finalizePrompt.Profiles) != 2 {
		t.Fatalf("finalize prompt = %#v, want readiness for both touched profiles", finalizePrompt)
	}
	readinessByProfile := map[string]initProfileReadiness{}
	for _, profile := range finalizePrompt.Profiles {
		readinessByProfile[profile.ProfileName] = profile
	}
	for _, name := range []string{"home", "work"} {
		profile := readinessByProfile[name]
		if profile.Ready {
			t.Fatalf("profile readiness = %#v, want deferred git credentials to mark %s not ready", profile, name)
		}
		if !strings.Contains(strings.Join(profile.Notes, "\n"), "Git deferred") {
			t.Fatalf("profile notes = %#v, want deferred git note for %s", profile.Notes, name)
		}
	}
	if !strings.Contains(stdout.String(), "Initialized 2 profile(s)") || !strings.Contains(stdout.String(), "- home: needs follow-up") || !strings.Contains(stdout.String(), "- work: needs follow-up") {
		t.Fatalf("stdout = %q, want readiness summary for both profiles", stdout.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/home --key "+credentials.GitTokenKey) || !strings.Contains(stderr.String(), "set-credential --ref codereview/work --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want follow-up hints for both deferred profiles", stderr.String())
	}
}

func TestInitInteractiveMenuFinalSaveSetNowWritesCredentialsAndMarksProfileReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	store := newFakeInitStore(map[string]map[string]string{})
	var finalizePrompt initFinalizePrompt
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			finalizePrompt = prompt
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
			sources: []initSecretSource{initSecretSourcePaste},
			pastes:  []string{"git-token"},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(finalizePrompt.Profiles) != 1 {
		t.Fatalf("finalize prompt = %#v, want one touched profile", finalizePrompt)
	}
	profileReadiness := finalizePrompt.Profiles[0]
	if profileReadiness.ProfileName != "default" || !profileReadiness.Ready || len(profileReadiness.Notes) != 0 {
		t.Fatalf("profile readiness = %#v, want ready default profile with no notes", profileReadiness)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "git-token" {
		t.Fatalf("stored git token = %q, want git-token from interactive SetNow flow", got)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Fatalf("default profile = %q, want default", cfg.DefaultProfile)
	}
	if cfg.Profiles["default"].Git.CredentialRef != "codereview/default" {
		t.Fatalf("default profile git ref = %q, want codereview/default", cfg.Profiles["default"].Git.CredentialRef)
	}
	if !strings.Contains(stdout.String(), "Initialized 1 profile(s)") || !strings.Contains(stdout.String(), "- default: ready") {
		t.Fatalf("stdout = %q, want ready summary for default profile", stdout.String())
	}
	if strings.Contains(stderr.String(), "set-credential --ref") {
		t.Fatalf("stderr = %q, want no follow-up credential hints after SetNow flow", stderr.String())
	}
}

func TestInitInteractiveMenuFinalSaveMixedReadinessSummarizesPerProfileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, "profiles: {}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	store := newFakeInitStore(map[string]map[string]string{})
	var finalizePrompt initFinalizePrompt
	prompterCalls := 0
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			finalizePrompt = prompt
			return initFinalizeActionSave, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls == 1 {
				return initDraft{
					ProfileName: "home",
					MakeDefault: true,
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			}
			return initDraft{
				ProfileName: "work",
				MakeDefault: false,
				GitHost:     "gitlab.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionSetNow,
				initCredentialSecretActionDefer,
			},
			sources: []initSecretSource{initSecretSourcePaste},
			pastes:  []string{"home-token"},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(finalizePrompt.Profiles) != 2 {
		t.Fatalf("finalize prompt = %#v, want readiness for both touched profiles", finalizePrompt)
	}
	readinessByProfile := map[string]initProfileReadiness{}
	for _, profile := range finalizePrompt.Profiles {
		readinessByProfile[profile.ProfileName] = profile
	}
	home := readinessByProfile["home"]
	if !home.Ready || len(home.Notes) != 0 {
		t.Fatalf("home readiness = %#v, want ready with no notes after SetNow", home)
	}
	work := readinessByProfile["work"]
	if work.Ready {
		t.Fatalf("work readiness = %#v, want deferred work profile to need follow-up", work)
	}
	if !strings.Contains(strings.Join(work.Notes, "\n"), "Git deferred") {
		t.Fatalf("work notes = %#v, want deferred git note", work.Notes)
	}
	if got := store.bundles["home"][credentials.GitTokenKey]; got != "home-token" {
		t.Fatalf("stored home git token = %q, want home-token", got)
	}
	if _, ok := store.bundles["work"][credentials.GitTokenKey]; ok {
		t.Fatalf("work store bundle = %#v, want deferred work profile to skip keyring write", store.bundles["work"])
	}
	if !strings.Contains(stdout.String(), "- home: ready") || !strings.Contains(stdout.String(), "- work: needs follow-up") {
		t.Fatalf("stdout = %q, want mixed readiness summary", stdout.String())
	}
	if strings.Contains(stderr.String(), "set-credential --ref codereview/home --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want no follow-up hint for ready home profile", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/work --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want follow-up hint for deferred work profile", stderr.String())
	}
}

func TestInitInteractiveMenuGlobalSettingsOnlySaveDoesNotFinalizeBootstrappedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile: "work",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Data: config.DataConfig{
			Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(14),
				Enforcement: config.RetentionManualOnly,
			},
		},
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
	finalizeCalls := 0
	openStoreCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionSave,
			},
		},
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{
				Apply: true,
				Retention: config.RetentionConfig{
					MaxAgeDays:  intPtr(30),
					Enforcement: config.RetentionAtWrite,
				},
			}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{
				Apply:   true,
				Backend: "file",
			}, nil
		}),
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			finalizeCalls++
			if len(prompt.Profiles) != 0 {
				t.Fatalf("finalize prompt = %#v, want no profile readiness for untouched bootstrap profile", prompt)
			}
			return initFinalizeActionSave, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			openStoreCalls++
			return newFakeInitStore(nil), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if finalizeCalls != 1 {
		t.Fatalf("finalizeCalls = %d, want 1", finalizeCalls)
	}
	if openStoreCalls != 0 {
		t.Fatalf("openStoreCalls = %d, want 0 when no touched profiles require credential handling", openStoreCalls)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "file" {
		t.Fatalf("keyring.backend = %q, want file", cfg.Keyring.Backend)
	}
	if cfg.Data.Retention.MaxAgeDaysValue() != 30 || cfg.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("retention = %#v, want 30/at_write", cfg.Data.Retention)
	}
	if got := sortedProfileNames(cfg.Profiles); !reflect.DeepEqual(got, []string{"work"}) {
		t.Fatalf("profiles = %#v, want only existing work profile", got)
	}
	work := cfg.Profiles["work"]
	if work.Git.Host != "github.com" || work.Git.AuthMode != config.GitAuthModePAT || work.Git.CredentialRef != "codereview/work" {
		t.Fatalf("work git = %#v, want untouched bootstrap profile git config", work.Git)
	}
	if work.ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials = %#v, want nil for untouched profile", work.ReviewerCredentials)
	}
	if work.LLM.Provider != config.LLMProviderAnthropic || work.LLM.Auth != config.LLMAuthSubscription || work.LLM.Adapter != config.LLMAdapterClaudeCLI {
		t.Fatalf("work llm = %#v, want untouched bootstrap profile llm config", work.LLM)
	}
}

func TestInitInteractiveMenuCancelAfterSecretEntryBeforeFinalSaveWritesNothing(t *testing.T) {
	store := newFakeInitStore(nil)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionCancel, nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
			sources: []initSecretSource{initSecretSourcePaste},
			pastes:  []string{"new-token"},
		},
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite cancel-before-save")
			return nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if _, exists := store.bundles["default"][credentials.GitTokenKey]; exists {
		t.Fatalf("store bundles = %#v, want no keyring writes after cancel", store.bundles)
	}
}

func TestInitInteractiveFinalizationKeyringOpenFailure(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			t.Fatal("finalize prompt should not run after keyring open failure")
			return "", nil
		}),
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			return nil, errors.New("open failed")
		},
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite keyring open failure")
			return nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err == nil || !strings.Contains(err.Error(), "open failed") {
		t.Fatalf("error = %v, want keyring open failure", err)
	}
}

func TestApplyInteractiveInitSessionPlanRejectsInvalidConfigBeforeKeyringWrite(t *testing.T) {
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	storeOpened := false
	plan := initSessionPlan{
		cfg: config.File{
			Profiles: map[string]config.Profile{
				"default": basicProfile("default"),
			},
		},
		profileNames: []string{"default"},
		writes: map[string]map[string]string{
			"codereview/default": {credentials.GitTokenKey: "git-token"},
		},
	}
	err := applyInteractiveInitSessionPlan(opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			storeOpened = true
			return newFakeInitStore(nil), nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite invalid config")
			return nil
		},
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "default_profile") {
		t.Fatalf("error = %v, want config validation failure", err)
	}
	if storeOpened {
		t.Fatal("store opened despite config validation failure")
	}
}

func TestApplyInteractiveInitSessionPlanConfigSaveFailureAfterKeyringWritesReportsCleanup(t *testing.T) {
	store := newFakeInitStore(nil)
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	profile := basicProfile("default")
	refs, err := config.CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          config.File{DefaultProfile: "default", Profiles: map[string]config.Profile{"default": profile}},
		profileNames: []string{"default"},
		profileRefs:  map[string][]config.CredentialRef{"default": refs},
		writes: map[string]map[string]string{
			"codereview/default": {credentials.GitTokenKey: "git-token"},
		},
	}
	err = applyInteractiveInitSessionPlan(opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error {
			return errors.New("disk full")
		},
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "credential refs needing cleanup: [codereview/default]") {
		t.Fatalf("error = %v, want cleanup hint after config save failure", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "git-token" {
		t.Fatalf("stored git token = %q, want git-token before config save failure", got)
	}
}

func TestApplyInteractiveInitSessionPlanPartialKeyringWriteFailureReportsCleanup(t *testing.T) {
	store := newFakeInitStore(nil)
	store.setBundleFunc = func(profile string, kv map[string]string, _ ...credstore.SetOpt) (credstore.Result, error) {
		if profile == "a" {
			if store.bundles[profile] == nil {
				store.bundles[profile] = map[string]string{}
			}
			for key, value := range kv {
				store.bundles[profile][key] = value
			}
			return credstore.Result{Written: []string{credentials.GitTokenKey}}, nil
		}
		return credstore.Result{RollbackFailed: []string{credentials.GitTokenKey}}, errors.New("write failed")
	}
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	plan := initSessionPlan{
		cfg: config.File{
			DefaultProfile: "a",
			Profiles: map[string]config.Profile{
				"a": basicProfile("a"),
				"b": basicProfile("b"),
			},
		},
		profileNames: []string{"a", "b"},
		writes: map[string]map[string]string{
			"codereview/a": {credentials.GitTokenKey: "token-a"},
			"codereview/b": {credentials.GitTokenKey: "token-b"},
		},
	}
	err := applyInteractiveInitSessionPlan(opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite partial keyring write failure")
			return nil
		},
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "credential refs needing cleanup: [codereview/a codereview/b]") {
		t.Fatalf("error = %v, want cleanup refs after partial keyring write failure", err)
	}
}

func TestApplyInteractiveInitSessionPlanOverwriteConflictFailsBeforeWrite(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"default": {credentials.GitTokenKey: "existing-token"},
	})
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	plan := initSessionPlan{
		cfg: config.File{
			DefaultProfile: "default",
			Profiles: map[string]config.Profile{
				"default": basicProfile("default"),
			},
		},
		profileNames: []string{"default"},
		writes: map[string]map[string]string{
			"codereview/default": {credentials.GitTokenKey: "new-token"},
		},
		overwriteRefs: map[string]bool{},
	}
	err := applyInteractiveInitSessionPlan(opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite overwrite conflict")
			return nil
		},
	}, plan)
	if err == nil || !strings.Contains(err.Error(), credstore.ErrExists.Error()) {
		t.Fatalf("error = %v, want overwrite conflict", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "existing-token" {
		t.Fatalf("stored git token = %q, want existing-token preserved on conflict", got)
	}
}

func TestInitNonInteractiveBypassesInteractiveMenuPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		menuPrompter: initMenuPrompterFunc(func(initMenuPrompt) (initMenuAction, error) {
			t.Fatal("interactive menu should not run for --non-interactive")
			return "", nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			t.Fatal("global settings should not run for --non-interactive")
			return initRetentionEdit{}, nil
		}),
	}
	flags := initOptions{
		nonInteractive: true,
		gitHost:        "github.com",
		gitAuth:        string(config.GitAuthModePAT),
		llmProvider:    string(config.LLMProviderAnthropic),
		llmAuth:        string(config.LLMAuthSubscription),
		llmAdapter:     string(config.LLMAdapterClaudeCLI),
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, flags, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.DefaultProfile == "" || len(cfg.Profiles) != 1 {
		t.Fatalf("config = %#v, want non-interactive profile saved", cfg)
	}
}

func TestInitInteractiveRoutesCreateEditRemoveAndDeriveFromPRURL(t *testing.T) {
	tests := []struct {
		name           string
		existingRoutes []config.RepositoryProfile
		edit           initRoutesEdit
		want           []config.RepositoryProfile
	}{
		{
			name: "create from pr url",
			edit: initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			}}},
			want: []config.RepositoryProfile{{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "open-cli-collective",
					Repos:     []string{"codereview-cli"},
				},
			}},
		},
		{
			name: "edit and preserve unrelated",
			existingRoutes: []config.RepositoryProfile{
				{
					Profile: "work",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "open-cli-collective",
					},
				},
				{
					Profile: "home",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "rianjs",
					},
				},
			},
			edit: initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli", "cli-common"},
			}}},
			want: []config.RepositoryProfile{
				{
					Profile: "home",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "rianjs",
					},
				},
				{
					Profile: "work",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "open-cli-collective",
						Repos:     []string{"cli-common", "codereview-cli"},
					},
				},
			},
		},
		{
			name: "remove all",
			existingRoutes: []config.RepositoryProfile{{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "open-cli-collective",
				},
			}},
			edit: initRoutesEdit{Apply: true, Routes: nil},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			saveCredentialTestConfig(t, path, config.File{
				DefaultProfile:     "work",
				RepositoryProfiles: tt.existingRoutes,
				Profiles: map[string]config.Profile{
					"work": basicProfile("work"),
					"home": basicProfile("home"),
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
						MakeDefault:         true,
						GitHost:             "github.com",
						GitAuth:             string(config.GitAuthModePAT),
						GitCredentialRef:    "codereview/work",
						LLMProvider:         string(config.LLMProviderAnthropic),
						LLMAuth:             string(config.LLMAuthSubscription),
						LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					}, nil
				}),
				routesPrompter: initRoutesPrompterFunc(func(initRoutesPrompt) (initRoutesEdit, error) {
					return tt.edit, nil
				}),
				configPath: func(*root.Options) (string, error) { return path, nil },
				loadConfig: loadConfigForInit,
				saveConfig: config.Save,
			}

			err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
			if err != nil {
				t.Fatalf("runInitWithDeps: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			if !reflect.DeepEqual(cfg.RepositoryProfiles, tt.want) {
				t.Fatalf("RepositoryProfiles = %#v, want %#v", cfg.RepositoryProfiles, tt.want)
			}
		})
	}
}

func TestInitInteractiveRouteParsersAcceptPRURLAndManualSpecs(t *testing.T) {
	urlSpec, err := parseInitRouteSpec("https://github.com/open-cli-collective/codereview-cli/pull/185")
	if err != nil {
		t.Fatalf("parseInitRouteSpec PR URL: %v", err)
	}
	if !reflect.DeepEqual(urlSpec, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "open-cli-collective",
		Repos:     []string{"codereview-cli"},
	}) {
		t.Fatalf("PR URL spec = %#v", urlSpec)
	}

	manualSpec, err := parseInitRouteSpec("github.com/open-cli-collective [codereview-cli, cli-common]")
	if err != nil {
		t.Fatalf("parseInitRouteSpec manual: %v", err)
	}
	if !reflect.DeepEqual(manualSpec, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "open-cli-collective",
		Repos:     []string{"cli-common", "codereview-cli"},
	}) {
		t.Fatalf("manual spec = %#v", manualSpec)
	}
}

func TestInitInteractiveReconcilesRouteHostChangeBeforeSave(t *testing.T) {
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
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if !prompt.HostChanged || prompt.PreviousHost != "github.com" || prompt.ProfileHost != "gitlab.com" {
				t.Fatalf("prompt = %#v, want host reconciliation context", prompt)
			}
			return initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "gitlab.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			}}}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Profiles["work"].Git.Host != "gitlab.com" {
		t.Fatalf("git.host = %q, want gitlab.com", cfg.Profiles["work"].Git.Host)
	}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "gitlab.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
	}}) {
		t.Fatalf("RepositoryProfiles = %#v, want reconciled gitlab route", cfg.RepositoryProfiles)
	}
}

func TestInitInteractiveReconcilesRouteHostChangeFromSelectedGitScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	work := basicProfile("work")
	office := basicProfile("office")
	office.Git.Host = "gitlab.com"
	office.Git.CredentialRef = "codereview/office"
	cfg := config.File{
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
			"office": office,
			"work":   work,
		},
	}
	saveCredentialTestConfig(t, path, config.File{
		DefaultProfile:     cfg.DefaultProfile,
		RepositoryProfiles: cfg.RepositoryProfiles,
		Profiles:           cfg.Profiles,
	})
	scopes, profileScopeNames := buildInitGitScopeInventory(cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			draft := seedInteractiveInitDraft("work", "work", "work", &work)
			applyGitScopeSelection(&draft, profileScopeNames["office"], scopes)
			return draft, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if !prompt.HostChanged || prompt.PreviousHost != "github.com" || prompt.ProfileHost != "gitlab.com" {
				t.Fatalf("prompt = %#v, want selected git scope reconciliation context", prompt)
			}
			return initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "gitlab.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			}}}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionKeep},
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"office": {credentials.GitTokenKey: "existing-token"},
			}), nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Profiles["work"].Git.Host != "gitlab.com" || cfg.Profiles["work"].Git.CredentialRef != "codereview/office" {
		t.Fatalf("git profile = %#v, want selected gitlab scope persisted onto work", cfg.Profiles["work"].Git)
	}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "gitlab.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
	}}) {
		t.Fatalf("RepositoryProfiles = %#v, want reconciled gitlab route", cfg.RepositoryProfiles)
	}
}

func TestInitInteractiveReconcilesRouteHostChangeDuringRename(t *testing.T) {
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
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/custom-office-git",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if prompt.ProfileName != "office" || !prompt.HostChanged {
				t.Fatalf("prompt = %#v, want renamed profile reconciliation context", prompt)
			}
			return initRoutesEdit{Apply: true, Routes: []configedit.RepositoryRouteSpec{{
				Host:      "gitlab.com",
				Namespace: "open-cli-collective",
			}}}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Fatalf("old profile still exists after rename: %#v", cfg.Profiles)
	}
	if cfg.RepositoryProfiles[0].Profile != "office" || cfg.RepositoryProfiles[0].Match.Host != "gitlab.com" {
		t.Fatalf("RepositoryProfiles = %#v, want renamed reconciled route", cfg.RepositoryProfiles)
	}
}

func TestInitInteractiveRejectsPreservedMismatchedRoutesAfterHostChange(t *testing.T) {
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
				ProfileName:         "work",
				GitHost:             "gitlab.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(initRoutesPrompt) (initRoutesEdit, error) {
			return initRoutesEdit{Apply: false}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "edit routes to reconcile the host change") {
		t.Fatalf("error = %v, want host reconciliation rejection", err)
	}
}

func TestInitInteractiveRejectsSecretIngressFlagsBeforePrompt(t *testing.T) {
	opts := &root.Options{
		Stdin:      failReader{},
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			t.Fatal("prompter called despite interactive secret-flag rejection")
			return initDraft{}, nil
		}),
		configPath: func(*root.Options) (string, error) {
			t.Fatal("configPath called despite interactive secret-flag rejection")
			return "", nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{
		// #nosec G101 -- test-only env var name; no credential value is embedded.
		gitTokenEnv: "CR_GIT_TOKEN",
	}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "only supported with --non-interactive") {
		t.Fatalf("error = %v, want interactive secret flag rejection", err)
	}
}

func TestInitInteractiveRejectsNonInteractiveParityFlagsBeforePrompt(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		flags initOptions
	}{
		{
			name: "string git auth",
			args: []string{"--git-auth-mode", "github_app"},
			flags: initOptions{
				gitAuth: string(config.GitAuthModeGitHubApp),
			},
		},
		{
			name: "bool disable reviewer",
			args: []string{"--disable-reviewer"},
			flags: initOptions{
				disableReviewer: true,
			},
		},
		{
			name: "string keyring backend",
			args: []string{"--keyring-backend", "file"},
			flags: initOptions{
				keyringBackend: "file",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &root.Options{
				Stdin:      failReader{},
				Stdout:     &bytes.Buffer{},
				Stderr:     &bytes.Buffer{},
				ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
			}
			deps := initDeps{
				prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
					t.Fatal("prompter called despite interactive parity-flag rejection")
					return initDraft{}, nil
				}),
				configPath: func(*root.Options) (string, error) {
					t.Fatal("configPath called despite interactive parity-flag rejection")
					return "", nil
				},
			}
			cmd := &cobra.Command{}
			cmd.Flags().String("git-auth-mode", "", "")
			cmd.Flags().Bool("disable-reviewer", false, "")
			cmd.Flags().String("llm-reviewer-model-tier", "", "")
			cmd.Flags().String("keyring-backend", "", "")
			if err := cmd.Flags().Parse(tt.args); err != nil {
				t.Fatalf("Parse flags: %v", err)
			}

			err := runInitWithDeps(cmd, opts, tt.flags, deps)
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
			}
			if !strings.Contains(err.Error(), "only supported with --non-interactive") {
				t.Fatalf("error = %v, want interactive parity-flag rejection", err)
			}
		})
	}
}

func TestInitInteractivePersistsExplicitBackendForDeferredLLM(t *testing.T) {
	hermeticFileBackend(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd := &cobra.Command{}
	cmd.Flags().String(credstore.BackendFlagName, "", "")
	if err := cmd.Flags().Set(credstore.BackendFlagName, "file"); err != nil {
		t.Fatalf("set backend flag: %v", err)
	}
	opts := &root.Options{
		Backend:    "file",
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:          "default",
				MakeDefault:          true,
				GitHost:              "github.com",
				GitAuth:              string(config.GitAuthModePAT),
				GitCredentialRef:     "codereview/default",
				LLMProvider:          string(config.LLMProviderOpenAI),
				LLMAuth:              string(config.LLMAuthAPIKey),
				LLMAdapter:           string(config.LLMAdapterOpenAIAPI),
				LLMReviewerModelTier: string(config.ModelTierMedium),
				LLMCredentialRef:     "codereview/default-llm",
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read during deferred llm init")
			return "", nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true, Backend: "file"}, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
		readSecret: func(io.Reader, bool, string, string, string) (string, bool, error) {
			t.Fatal("interactive deferred llm init should not read secret ingress")
			return "", false, nil
		},
	}

	err := runInitWithDeps(cmd, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "file" {
		t.Fatalf("keyring.backend = %q, want file", cfg.Keyring.Backend)
	}
	if strings.Contains(stderr.String(), "cr --backend file set-credential") {
		t.Fatalf("stderr = %q, want persisted backend to drop explicit backend hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cr set-credential --ref codereview/default-llm --key "+credentials.OpenAIAPIKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred llm follow-up hint without backend flag", stderr.String())
	}
}

func TestInitInteractiveCollectsClipboardGitSecretWithoutHint(t *testing.T) {
	store := newFakeInitStore(nil)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var savedCfg config.File
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
			sources: []initSecretSource{initSecretSourceClipboard},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead:      func() (string, error) { return "clipboard-token", nil },
		configPath:         func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(_ string, cfg config.File) error {
			savedCfg = cfg
			return nil
		},
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "clipboard-token" {
		t.Fatalf("stored git token = %q, want clipboard-token", got)
	}
	if strings.Contains(stdout.String()+stderr.String(), "clipboard-token") {
		t.Fatalf("interactive init leaked secret: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "set-credential --ref codereview/default --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want no stale follow-up hint after collected secret", stderr.String())
	}
	if savedCfg.DefaultProfile != "default" {
		t.Fatalf("default_profile = %q, want default", savedCfg.DefaultProfile)
	}
	if savedCfg.Profiles["default"].Git.CredentialRef != "codereview/default" {
		t.Fatalf("git ref = %q, want codereview/default", savedCfg.Profiles["default"].Git.CredentialRef)
	}
}

func TestInitInteractiveDeferDoesNotRequireKeyringAccess(t *testing.T) {
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &stderr,
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read on defer")
			return "", nil
		},
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not be called when interactive init defers")
			return nil, nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if !strings.Contains(stderr.String(), "set-credential --ref codereview/default --key "+credentials.GitTokenKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred git follow-up hint", stderr.String())
	}
}

func TestInitInteractiveSetNowOverwritesExistingTargetRef(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"default": {credentials.GitTokenKey: "old-token"},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionSetNow,
				initCredentialSecretActionSetNow,
			},
			sources: []initSecretSource{initSecretSourcePaste},
			pastes:  []string{"new-token"},
		},
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read")
			return "", nil
		},
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "new-token" {
		t.Fatalf("stored git token = %q, want new-token", got)
	}
}

func TestInitInteractiveCanKeepExistingSecretsAfterInspectingTargetRef(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"default": {credentials.GitTokenKey: "existing-token"},
	})
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &stderr,
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionSetNow,
				initCredentialSecretActionKeep,
			},
		},
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read when keeping existing secrets")
			return "", nil
		},
		configPath: func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "existing-token" {
		t.Fatalf("stored git token = %q, want existing-token", got)
	}
	if strings.Contains(stderr.String(), "set-credential --ref codereview/default --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want no follow-up hint after keeping existing secret", stderr.String())
	}
}

func TestInitInteractiveCollectsGitHubAppBundle(t *testing.T) {
	store := newFakeInitStore(nil)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModeGitHubApp),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
			sources: []initSecretSource{
				initSecretSourcePaste,
				initSecretSourceClipboard,
				initSecretSourceSkip,
			},
			pastes: []string{"12345"},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead:      func() (string, error) { return "private-key", nil },
		configPath:         func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	bundle := store.bundles["default"]
	if bundle[credentials.GitHubAppIDKey] != "12345" {
		t.Fatalf("github_app_id = %q, want 12345", bundle[credentials.GitHubAppIDKey])
	}
	if bundle[credentials.GitHubAppPrivateKeyKey] != "private-key" {
		t.Fatalf("github_app_private_key = %q, want private-key", bundle[credentials.GitHubAppPrivateKeyKey])
	}
	if _, ok := bundle[credentials.GitHubAppInstallationIDKey]; ok {
		t.Fatalf("github_app_installation_id present, want skipped optional key")
	}
}

func TestInitInteractiveCollectsProviderSpecificLLMKey(t *testing.T) {
	store := newFakeInitStore(nil)
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &stderr,
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:      "default",
				MakeDefault:      true,
				GitHost:          "github.com",
				GitAuth:          string(config.GitAuthModePAT),
				LLMProvider:      string(config.LLMProviderOpenAI),
				LLMAuth:          string(config.LLMAuthAPIKey),
				LLMAdapter:       string(config.LLMAdapterOpenAIAPI),
				LLMCredentialRef: "codereview/default-llm",
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionSetNow,
			},
			sources: []initSecretSource{initSecretSourceClipboard},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead:      func() (string, error) { return "openai-key", nil },
		configPath:         func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if got := store.bundles["default-llm"][credentials.OpenAIAPIKeyKey]; got != "openai-key" {
		t.Fatalf("openai api key = %q, want openai-key", got)
	}
	if strings.Contains(stderr.String(), "set-credential --ref codereview/default-llm --key "+credentials.OpenAIAPIKeyKey) {
		t.Fatalf("stderr = %q, want no stale llm follow-up hint after collected key", stderr.String())
	}
}

func TestInitInteractiveEmptyClipboardSecretDoesNotLeak(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				MakeDefault: true,
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
			sources: []initSecretSource{initSecretSourceClipboard},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead:      func() (string, error) { return "", nil },
		configPath:         func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{Profiles: map[string]config.Profile{}}, false, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite empty clipboard secret")
			return nil
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if strings.Contains(err.Error(), "clipboard-token") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "empty secret") {
		t.Fatalf("error = %v, want empty secret rejection", err)
	}
}

func TestInitInteractiveRejectsKeepForChangedRefWithoutTargetBundle(t *testing.T) {
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				OriginalProfileName: "work",
				ProfileName:         "work",
				MakeDefault:         true,
				GitHost:             "github.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work-new",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionKeep},
		},
		clipboardSupported: func() bool { return true },
		clipboardRead:      func() (string, error) { return "", nil },
		configPath:         func(*root.Options) (string, error) { return opts.ConfigPath, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{
				DefaultProfile: "work",
				Profiles: map[string]config.Profile{
					"work": basicProfile("work"),
				},
			}, true, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite invalid keep-existing secret choice")
			return nil
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "does not have all required keys") {
		t.Fatalf("error = %v, want missing target bundle rejection", err)
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
		{name: "unsupported git auth", args: []string{"init", "--non-interactive", "--git-auth-mode", string(config.GitAuthModeOAuthDevice)}},
		{name: "git token ingress under github app", args: []string{"init", "--non-interactive", "--git-auth-mode", string(config.GitAuthModeGitHubApp), "--git-token-from-env", "CR_GIT_TOKEN"}},
		{name: "unsupported reviewer auth", args: []string{"init", "--non-interactive", "--reviewer-auth-mode", string(config.GitAuthModeOAuthDevice)}},
		{name: "disable reviewer with reviewer auth", args: []string{"init", "--non-interactive", "--disable-reviewer", "--reviewer-auth-mode", string(config.GitAuthModePAT)}},
		{name: "disable reviewer with reviewer secret", args: []string{"init", "--non-interactive", "--disable-reviewer", "--reviewer-token-from-env", "CR_REVIEWER_TOKEN"}},
		{name: "empty reviewer env secret", args: []string{"init", "--non-interactive", "--reviewer-token-from-env", "CR_EMPTY_REVIEWER_TOKEN"}},
		{name: "llm ingress under subscription auth", args: []string{"init", "--non-interactive", "--llm-api-key-from-env", "CR_LLM_KEY"}},
		{name: "invalid reviewer model tier", args: []string{"init", "--non-interactive", "--llm-reviewer-model-tier", "flagship"}},
		{name: "set and clear reviewer model tier", args: []string{"init", "--non-interactive", "--llm-reviewer-model-tier", string(config.ModelTierMedium), "--clear-llm-reviewer-model-tier"}},
		{name: "set and reset keyring backend", args: []string{"init", "--non-interactive", "--keyring-backend", "file", "--reset-keyring-backend"}},
		{name: "runtime and durable backend conflict", args: []string{"--backend", "memory", "init", "--non-interactive", "--keyring-backend", "file"}},
		{name: "pi rpc adapter without pi provider", args: []string{"init", "--non-interactive", "--llm-adapter", string(config.LLMAdapterPiRPC)}},
		{name: "codex cli adapter without openai provider", args: []string{"init", "--non-interactive", "--llm-adapter", string(config.LLMAdapterCodexCLI)}},
	}
	t.Setenv("CR_LLM_KEY", "llm-key")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	t.Setenv("CR_EMPTY_REVIEWER_TOKEN", "")
	t.Setenv("CR_REVIEWER_TOKEN", "reviewer-token")
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
	}, false, nil)
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

type fakeInitSecretPrompter struct {
	actions     []initCredentialSecretAction
	sources     []initSecretSource
	pastes      []string
	pasteErrors []error
}

func (f *fakeInitSecretPrompter) ChooseCredentialAction(initCredentialSecretPrompt) (initCredentialSecretAction, error) {
	if len(f.actions) == 0 {
		return "", errors.New("unexpected credential action prompt")
	}
	action := f.actions[0]
	f.actions = f.actions[1:]
	return action, nil
}

func (f *fakeInitSecretPrompter) ChooseSecretSource(initSecretValuePrompt) (initSecretSource, error) {
	if len(f.sources) == 0 {
		return "", errors.New("unexpected secret source prompt")
	}
	source := f.sources[0]
	f.sources = f.sources[1:]
	return source, nil
}

func (f *fakeInitSecretPrompter) PasteSecret(initSecretValuePrompt) (string, error) {
	if len(f.pasteErrors) > 0 {
		err := f.pasteErrors[0]
		f.pasteErrors = f.pasteErrors[1:]
		if err != nil {
			return "", err
		}
	}
	if len(f.pastes) == 0 {
		return "", errors.New("unexpected paste prompt")
	}
	value := f.pastes[0]
	f.pastes = f.pastes[1:]
	return value, nil
}

type fakeInitStore struct {
	bundles       map[string]map[string]string
	setBundleFunc func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error)
}

func newFakeInitStore(bundles map[string]map[string]string) *fakeInitStore {
	copied := map[string]map[string]string{}
	for profile, bundle := range bundles {
		copied[profile] = map[string]string{}
		for key, value := range bundle {
			copied[profile][key] = value
		}
	}
	return &fakeInitStore{bundles: copied}
}

func (s *fakeInitStore) Exists(profile, key string) (bool, error) {
	_, ok := s.bundles[profile][key]
	return ok, nil
}

func (s *fakeInitStore) ListBundle(profile string) ([]string, error) {
	bundle := s.bundles[profile]
	keys := make([]string, 0, len(bundle))
	for key := range bundle {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *fakeInitStore) SetBundle(profile string, kv map[string]string, opts ...credstore.SetOpt) (credstore.Result, error) {
	if s.setBundleFunc != nil {
		return s.setBundleFunc(profile, kv, opts...)
	}
	if s.bundles[profile] == nil {
		s.bundles[profile] = map[string]string{}
	}
	// Tests only pass credstore.WithOverwrite when they intend replacement.
	overwrite := len(opts) > 0
	for key := range kv {
		if _, ok := s.bundles[profile][key]; ok && !overwrite {
			return credstore.Result{}, credstore.ErrExists
		}
	}
	written := make([]string, 0, len(kv))
	for key, value := range kv {
		s.bundles[profile][key] = value
		written = append(written, key)
	}
	sort.Strings(written)
	return credstore.Result{Written: written}, nil
}

func (s *fakeInitStore) Close() error { return nil }

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

type initMenuPrompterFunc func(initMenuPrompt) (initMenuAction, error)

func (f initMenuPrompterFunc) ChooseAction(prompt initMenuPrompt) (initMenuAction, error) {
	return f(prompt)
}

type initLLMRuntimePrompterFunc func(initLLMRuntimePrompt) (initDraft, error)

func (f initLLMRuntimePrompterFunc) EditLLMRuntime(prompt initLLMRuntimePrompt) (initDraft, error) {
	return f(prompt)
}

type initReviewerEntityPrompterFunc func(initReviewerEntityPrompt) (initDraft, error)

func (f initReviewerEntityPrompterFunc) EditReviewerEntity(prompt initReviewerEntityPrompt) (initDraft, error) {
	return f(prompt)
}

type initFinalizePrompterFunc func(initFinalizePrompt) (initFinalizeAction, error)

func (f initFinalizePrompterFunc) ChooseFinalizeAction(prompt initFinalizePrompt) (initFinalizeAction, error) {
	return f(prompt)
}

type initModelMapPrompterFunc func(initModelMapPrompt) (initModelMapEdit, error)

func (f initModelMapPrompterFunc) EditModelMap(prompt initModelMapPrompt) (initModelMapEdit, error) {
	return f(prompt)
}

type initAgentSourcesPrompterFunc func(initAgentSourcesPrompt) (initAgentSourcesEdit, error)

func (f initAgentSourcesPrompterFunc) EditAgentSources(prompt initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
	return f(prompt)
}

type initReviewPolicyPrompterFunc func(initReviewPolicyPrompt) (initReviewPolicyEdit, error)

func (f initReviewPolicyPrompterFunc) EditReviewPolicy(prompt initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
	return f(prompt)
}

type initRoutesPrompterFunc func(initRoutesPrompt) (initRoutesEdit, error)

func (f initRoutesPrompterFunc) EditRoutes(prompt initRoutesPrompt) (initRoutesEdit, error) {
	return f(prompt)
}

type initRetentionPrompterFunc func(initRetentionPrompt) (initRetentionEdit, error)

func (f initRetentionPrompterFunc) EditRetention(prompt initRetentionPrompt) (initRetentionEdit, error) {
	return f(prompt)
}

type initKeyringBackendPrompterFunc func(initKeyringBackendPrompt) (initKeyringBackendEdit, error)

func (f initKeyringBackendPrompterFunc) EditKeyringBackend(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	return f(prompt)
}

type fakeInitMenuPrompter struct {
	actions []initMenuAction
	prompts []initMenuPrompt
}

func (f *fakeInitMenuPrompter) ChooseAction(prompt initMenuPrompt) (initMenuAction, error) {
	f.prompts = append(f.prompts, prompt)
	if len(f.actions) == 0 {
		return "", errors.New("unexpected menu prompt")
	}
	action := f.actions[0]
	f.actions = f.actions[1:]
	return action, nil
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

func intPtr(v int) *int { return &v }

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
