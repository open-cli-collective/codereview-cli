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
			mustAvoid: "CR_FUTURE_TOKEN",
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

func TestInitRejectsInvalidSecretAndProfileInputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing non-interactive", args: []string{"init"}},
		{name: "two stdin secrets", args: []string{"init", "--non-interactive", "--git-token-stdin", "--llm-api-key-stdin"}},
		{name: "git and reviewer stdin secrets", args: []string{"init", "--non-interactive", "--git-token-stdin", "--reviewer-token-stdin"}},
		{name: "invalid profile segment", args: []string{"--profile", "bad.profile", "init", "--non-interactive"}},
		{name: "reviewer ref matches git ref", args: []string{"init", "--non-interactive", "--reviewer-credential-ref", "codereview/default"}},
		{name: "unsupported reviewer auth", args: []string{"init", "--non-interactive", "--reviewer-auth-mode", string(config.GitAuthModeOAuthDevice)}},
		{name: "empty reviewer env secret", args: []string{"init", "--non-interactive", "--reviewer-token-from-env", "CR_EMPTY_REVIEWER_TOKEN"}},
		{name: "llm ingress under subscription auth", args: []string{"init", "--non-interactive", "--llm-api-key-from-env", "CR_LLM_KEY"}},
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
