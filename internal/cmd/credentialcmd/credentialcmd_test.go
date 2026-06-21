package credentialcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/creack/pty"
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
	saveCredentialTestConfig(t, path, testFileCredentialStoreConfig("work"))
	cmd, out, _ := newTestCommand(path, strings.NewReader("distinctive-token\n"))

	err := root.Execute(cmd, []string{
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/work",
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
	if got.Store != testFileCredentialStoreID || got.Name != "codereview/work" || got.Key != credentials.GitTokenKey || !got.Written {
		t.Fatalf("credential write JSON = %#v, want written git token", got)
	}
	if got.Backend != "file" || got.BackendSource != string(credentials.BackendSourceCredentialStore) {
		t.Fatalf("backend JSON = (%q,%q), want (file,credential_store)", got.Backend, got.BackendSource)
	}
	assertStored(t, "work", credentials.GitTokenKey, "distinctive-token")
}

func TestSetCredentialUsesExplicitConfiguredStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	hermeticFileBackend(t)
	saveCredentialTestConfig(t, path, config.File{
		Secrets: testFileSecretsConfig(),
		Profiles: map[string]config.Profile{
			"work": profileWithCredentialStore(basicProfile("work"), testFileCredentialStoreID),
		},
	})
	cmd, out, _ := newTestCommand(path, strings.NewReader("distinctive-token\n"))

	err := root.Execute(cmd, []string{
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/work",
		"--key", credentials.GitTokenKey,
		"--stdin",
		"--json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.CredentialWrite
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != string(credentials.BackendSourceCredentialStore) {
		t.Fatalf("backend JSON = (%q,%q), want (file,credential_store)", got.Backend, got.BackendSource)
	}
	assertStored(t, "work", credentials.GitTokenKey, "distinctive-token")
}

func TestSetCredentialRejectsLiteralIngress(t *testing.T) {
	cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader(""))

	err := root.Execute(cmd, []string{
		"set-credential",
		"--store", config.LocalOSCredentialStoreID,
		"--name", "codereview/work",
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
				"set-credential",
				"--store", config.LocalOSCredentialStoreID,
				"--name", "codereview/work",
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
		Secrets: testFileSecretsConfig(),
		Profiles: map[string]config.Profile{
			"work": profileWithCredentialStore(apiKeyProfile("work", config.LLMProviderAnthropic), testFileCredentialStoreID),
		},
	})

	cmd, _, _ := newTestCommand(path, failReader{})
	err := root.Execute(cmd, []string{
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/work-llm",
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
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/ad-hoc-openai",
		"--key", credentials.OpenAIAPIKeyKey,
		"--stdin",
	})
	if err != nil {
		t.Fatalf("unknown ref global key Execute: %v", err)
	}
	assertFileBundleKeys(t, "ad-hoc-openai", []string{credentials.OpenAIAPIKeyKey})

	cmd, _, _ = newTestCommand(path, strings.NewReader("anthropic-token"))
	err = root.Execute(cmd, []string{
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/work-llm",
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
		Secrets: testFileSecretsConfig(),
		Profiles: map[string]config.Profile{
			"work": profileWithCredentialStore(basicProfile("work"), testFileCredentialStoreID),
		},
	}
	work := cfg.Profiles["work"]
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode: config.GitAuthModeGitHubApp,
		Credential: config.CredentialLocation{
			Store: testFileCredentialStoreID,
			Name:  "codereview/work-reviewer",
		},
		CredentialRef: "codereview/work-reviewer",
	}
	cfg.Profiles["work"] = work
	saveCredentialTestConfig(t, path, cfg)

	cmd, _, _ := newTestCommand(path, failReader{})
	err := root.Execute(cmd, []string{
		"set-credential",
		"--store", testFileCredentialStoreID,
		"--name", "codereview/work-reviewer",
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
			"set-credential",
			"--store", testFileCredentialStoreID,
			"--name", "codereview/work-reviewer",
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
	writeRawCredentialTestConfig(t, path, `profiles:
  future:
    git:
      host: github.com
      auth_mode: oauth_device
      credential:
        store: local-os
        name: codereview/future
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
				"set-credential",
				"--store", config.LocalOSCredentialStoreID,
				"--name", "codereview/future",
				"--key", credentials.GitTokenKey,
				"--stdin",
			},
			stdin:     failReader{},
			mustAvoid: "secret ingress was read",
		},
		{
			name: "from-env",
			args: []string{
				"set-credential",
				"--store", config.LocalOSCredentialStoreID,
				"--name", "codereview/future",
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
	t.Run("missing explicit destination", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			args []string
			want string
		}{
			{
				name: "store",
				args: []string{"set-credential", "--name", "codereview/work", "--key", credentials.GitTokenKey, "--stdin"},
				want: "--store is required",
			},
			{
				name: "name",
				args: []string{"set-credential", "--store", config.LocalOSCredentialStoreID, "--key", credentials.GitTokenKey, "--stdin"},
				want: "--name is required",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), failReader{})
				err := root.Execute(cmd, tt.args)
				if got := exitcode.FromError(err); got != exitcode.UsageError {
					t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
				}
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				if strings.Contains(err.Error(), "secret ingress was read") {
					t.Fatalf("set-credential read secret ingress before rejecting missing destination: %v", err)
				}
			})
		}
	})

	t.Run("unknown store", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), failReader{})
		err := root.Execute(cmd, []string{
			"set-credential",
			"--store", "missing-store",
			"--name", "codereview/work",
			"--key", credentials.GitTokenKey,
			"--stdin",
		})
		if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.AuthConfigError, err)
		}
		if !errors.Is(err, config.ErrSecretsStoreNotFound) {
			t.Fatalf("error = %v, want ErrSecretsStoreNotFound", err)
		}
		if strings.Contains(err.Error(), "secret ingress was read") {
			t.Fatalf("set-credential read secret ingress before rejecting unknown store: %v", err)
		}
	})

	t.Run("backend flag conflicts with credential store", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"--backend", "bogus",
			"set-credential",
			"--store", config.LocalOSCredentialStoreID,
			"--name", "codereview/work",
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
			"set-credential",
			"--store", config.LocalOSCredentialStoreID,
			"--name", "codereview/work",
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
			"set-credential",
			"--store", config.LocalOSCredentialStoreID,
			"--name", "codereview/work",
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
		if got.Written || got.Error == "" || got.Store != config.LocalOSCredentialStoreID || got.Name != "codereview/work" || got.Key != "bad_key" {
			t.Fatalf("credential write JSON = %#v, want written=false error envelope", got)
		}
	})

	t.Run("wrong service ref", func(t *testing.T) {
		cmd, _, _ := newTestCommand(filepath.Join(t.TempDir(), "config.yml"), strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"set-credential",
			"--store", config.LocalOSCredentialStoreID,
			"--name", "other/work",
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
		path := filepath.Join(t.TempDir(), "config.yml")
		saveCredentialTestConfig(t, path, testFileCredentialStoreConfig("work"))
		cmd, _, _ := newTestCommand(path, strings.NewReader("token"))
		err := root.Execute(cmd, []string{
			"set-credential",
			"--store", testFileCredentialStoreID,
			"--name", "codereview/work",
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
		path := filepath.Join(t.TempDir(), "config.yml")
		saveCredentialTestConfig(t, path, testFileCredentialStoreConfig("work"))
		cmd, _, _ := newTestCommand(path, strings.NewReader("second"))
		err := root.Execute(cmd, []string{
			"set-credential",
			"--store", testFileCredentialStoreID,
			"--name", "codereview/work",
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
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "init-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
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
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
	}
	profile := cfg.Profiles["default"]
	if profile.Git.Credential.Store != config.LocalOSCredentialStoreID || profile.Git.Credential.Name != "codereview/default" {
		t.Fatalf("git credential = %#v, want local-os codereview/default", profile.Git.Credential)
	}
	assertFakeStored(t, store, "default", credentials.GitTokenKey, "init-token")
}

func TestInitNonInteractiveWritesReviewerCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	t.Setenv("CR_REVIEWER_TOKEN", "reviewer-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	flags.reviewerTokenEnv = "CR_REVIEWER_TOKEN"
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
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
		return
	}
	if reviewer.AuthMode != config.GitAuthModePAT || reviewer.CredentialRef != "codereview/default-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want pat codereview/default-reviewer", reviewer)
	}
	if reviewer.Credential.Store != config.LocalOSCredentialStoreID || reviewer.Credential.Name != "codereview/default-reviewer" {
		t.Fatalf("reviewer credential = %#v, want local-os codereview/default-reviewer", reviewer.Credential)
	}
	assertFakeStored(t, store, "default", credentials.GitTokenKey, "git-token")
	assertFakeStored(t, store, "default-reviewer", credentials.GitTokenKey, "reviewer-token")
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
	for _, key := range []string{credentials.GitHubAppIDKey, credentials.GitHubAppPrivateKeyKey} {
		if !strings.Contains(errOut.String(), "--key "+key+" --stdin") {
			t.Fatalf("stderr = %q, want setup hint for %s", errOut.String(), key)
		}
	}
	if strings.Contains(errOut.String(), "--key "+credentials.GitHubAppInstallationIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want optional installation id omitted from required setup hints", errOut.String())
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
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	flags.reviewerRef = "codereview/review-bot"
	flags.reviewerTokenStdin = true
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "work", strings.NewReader("reviewer-token\n"), flags, store)
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
	if reviewer == nil || reviewer.CredentialRef != "codereview/review-bot" {
		t.Fatalf("reviewer credentials = %#v, want custom codereview/review-bot", reviewer)
	}
	assertFakeStored(t, store, "work", credentials.GitTokenKey, "git-token")
	assertFakeStored(t, store, "review-bot", credentials.GitTokenKey, "reviewer-token")
}

func TestInitNonInteractiveDerivesReviewerRefFromStdinForProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	flags.reviewerTokenStdin = true
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "work", strings.NewReader("reviewer-token\n"), flags, store)
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
	assertFakeStored(t, store, "work", credentials.GitTokenKey, "git-token")
	assertFakeStored(t, store, "work-reviewer", credentials.GitTokenKey, "reviewer-token")
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
			path := filepath.Join(t.TempDir(), "config.yml")
			t.Setenv("CR_GIT_TOKEN", "git-token")
			t.Setenv("CR_LLM_KEY", "llm-token")
			store := newFakeInitStore(nil)
			flags := defaultNonInteractiveInitOptionsForTest()
			flags.gitTokenEnv = "CR_GIT_TOKEN"
			flags.llmProvider = string(tt.provider)
			flags.llmAuth = string(config.LLMAuthAPIKey)
			flags.llmAdapter = string(tt.adapter)
			flags.llmKeyEnv = "CR_LLM_KEY"
			out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
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
			assertFakeBundleKeys(t, store, "default", []string{credentials.GitTokenKey})
			assertFakeBundleKeys(t, store, "default-llm", []string{tt.key})
			assertFakeStored(t, store, "default-llm", tt.key, "llm-token")
		})
	}
}

func TestInitNonInteractiveWritesPiRPCProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("CR_GIT_TOKEN", "git-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	flags.llmProvider = string(config.LLMProviderPi)
	flags.llmAuth = string(config.LLMAuthSubscription)
	flags.llmAdapter = string(config.LLMAdapterPiRPC)
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
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
	assertFakeBundleKeys(t, store, "default", []string{credentials.GitTokenKey})
	assertFakeStored(t, store, "default", credentials.GitTokenKey, "git-token")
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
		previous.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-old"}
		desired := previous
		desired.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-new"}
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

func TestLoadInteractiveCredentialPlanStateSkipsStoreForSettledEntries(t *testing.T) {
	previous := basicProfile("work")
	keepEntries, err := planInitCredentials(&previous, previous, nil)
	if err != nil {
		t.Fatalf("planInitCredentials keep: %v", err)
	}
	writeEntries, err := planInitCredentials(nil, basicProfile("office"), map[string][]string{
		"codereview/office": {credentials.GitTokenKey},
	})
	if err != nil {
		t.Fatalf("planInitCredentials write: %v", err)
	}
	entries := []initCredentialPlanEntry{
		keepEntries[0],
		{
			Ref: config.CredentialRef{
				Purpose:  "reviewer_credentials",
				Ref:      "codereview/work-reviewer",
				Mode:     string(config.GitAuthModeGitHubApp),
				Provider: "github",
			},
			State: initCredentialPlanStateClearRef,
		},
		writeEntries[0],
	}

	got, err := loadInteractiveCredentialPlanState(entries, func() (initStore, error) {
		t.Fatal("openStore should not run for settled keep_existing/clear_ref/write entries")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("loadInteractiveCredentialPlanState: %v", err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("entries = %#v, want unchanged %#v", got, entries)
	}
}

func TestLoadInteractiveCredentialPlanStatePreservesSettledEntriesWhenMixedPlanNeedsStore(t *testing.T) {
	keepEntries, err := planInitCredentials(nil, basicProfile("work"), nil)
	if err != nil {
		t.Fatalf("planInitCredentials keep seed: %v", err)
	}
	keepEntries[0].State = initCredentialPlanStateKeepExisting

	writeEntries, err := planInitCredentials(nil, basicProfile("office"), map[string][]string{
		"codereview/office": {credentials.GitTokenKey},
	})
	if err != nil {
		t.Fatalf("planInitCredentials write: %v", err)
	}

	deferredProfile := apiKeyProfile("lab", config.LLMProviderAnthropic)
	deferredEntries, err := planInitCredentials(nil, deferredProfile, nil)
	if err != nil {
		t.Fatalf("planInitCredentials deferred: %v", err)
	}
	var llmEntry initCredentialPlanEntry
	for _, entry := range deferredEntries {
		if entry.Ref.Purpose == "llm" {
			llmEntry = entry
			break
		}
	}
	if llmEntry.Ref.Ref == "" {
		t.Fatal("llm entry missing from deferred profile")
	}

	entries := []initCredentialPlanEntry{
		keepEntries[0],
		{
			Ref: config.CredentialRef{
				Purpose:  "reviewer_credentials",
				Ref:      "codereview/work-reviewer",
				Mode:     string(config.GitAuthModeGitHubApp),
				Provider: "github",
			},
			State: initCredentialPlanStateClearRef,
		},
		writeEntries[0],
		llmEntry,
	}

	openStoreCalls := 0
	got, err := loadInteractiveCredentialPlanState(entries, func() (initStore, error) {
		openStoreCalls++
		return newFakeInitStore(map[string]map[string]string{
			"lab-llm": {
				credentials.AnthropicAPIKeyKey: "existing-llm-token",
			},
		}), nil
	})
	if err != nil {
		t.Fatalf("loadInteractiveCredentialPlanState: %v", err)
	}
	if openStoreCalls != 1 {
		t.Fatalf("openStoreCalls = %d, want 1 for mixed plan with deferred entry", openStoreCalls)
	}
	if got[0].State != initCredentialPlanStateKeepExisting {
		t.Fatalf("keep entry state = %s, want keep_existing", got[0].State)
	}
	if got[1].State != initCredentialPlanStateClearRef {
		t.Fatalf("clear entry state = %s, want clear_ref", got[1].State)
	}
	if got[2].State != initCredentialPlanStateWrite {
		t.Fatalf("write entry state = %s, want write", got[2].State)
	}
	if got[3].State != initCredentialPlanStateKeepExisting {
		t.Fatalf("deferred llm state = %s, want keep_existing after store inspection", got[3].State)
	}
}

func TestBuildInteractiveInitSessionPlanUsesOriginalProfileForRenamedTouchedProfile(t *testing.T) {
	original := config.File{
		Profiles: map[string]config.Profile{
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/work-reviewer",
					DisplayName:   "Old label",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}
	renamed, changed, err := configedit.RenameProfile(original, "work", "office")
	if err != nil {
		t.Fatalf("RenameProfile: %v", err)
	}
	if !changed {
		t.Fatal("RenameProfile changed = false, want true")
	}
	profile := renamed.Profiles["office"]
	profile.ReviewerCredentials.DisplayName = "OC Collective bot"
	renamed.Profiles["office"] = profile

	plan, err := buildInteractiveInitSessionPlan(&root.Options{}, initSessionDraft{
		originalCfg:     original,
		cfg:             renamed,
		touchedProfiles: map[string]string{"office": "work"},
	})
	if err != nil {
		t.Fatalf("buildInteractiveInitSessionPlan: %v", err)
	}

	var reviewerEntry *initCredentialPlanEntry
	for i := range plan.credentialPlan {
		if plan.credentialPlan[i].Ref.Purpose == "reviewer_credentials" {
			reviewerEntry = &plan.credentialPlan[i]
			break
		}
	}
	if reviewerEntry == nil {
		t.Fatal("reviewer credential entry missing from session plan")
		return
	}
	if reviewerEntry.State != initCredentialPlanStateKeepExisting {
		t.Fatalf("reviewer entry state = %s, want keep_existing for label-only renamed profile edit", reviewerEntry.State)
	}
	if reviewerEntry.PreviousRef == nil || reviewerEntry.PreviousRef.Ref != "codereview/work-reviewer" {
		t.Fatalf("reviewer previous ref = %#v, want codereview/work-reviewer", reviewerEntry.PreviousRef)
	}
	if got := plan.profileRefs["office"]; len(got) == 0 {
		t.Fatalf("profileRefs[office] = %#v, want populated refs for renamed profile", got)
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
	if !strings.Contains(errOut.String(), "cr set-credential --store local-os --name codereview/default --key git_token --stdin") {
		t.Fatalf("stderr = %q, want explicit local-os set-credential hint", errOut.String())
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
	if got := errOut.String(); !strings.Contains(got, "cr set-credential --store local-os --name codereview/default-reviewer --key git_token --stdin") {
		t.Fatalf("stderr = %q, want explicit reviewer set-credential hint", got)
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
	path := filepath.Join(t.TempDir(), "config.yml")
	store := newFakeInitStore(map[string]map[string]string{
		"default-llm": {credentials.AnthropicAPIKeyKey: "llm-token"},
	})
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.llmAuth = string(config.LLMAuthAPIKey)
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
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
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
	}
	if cfg.Profiles["default"].LLM.Credential.Store != config.LocalOSCredentialStoreID {
		t.Fatalf("llm credential store = %q, want local-os", cfg.Profiles["default"].LLM.Credential.Store)
	}
}

func TestInitAPIKeyAuthWithExistingKeyDoesNotPrintLLMFollowUpHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	store := newFakeInitStore(map[string]map[string]string{
		"default-llm": {credentials.AnthropicAPIKeyKey: "llm-token"},
	})
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.llmAuth = string(config.LLMAuthAPIKey)
	out, errOut, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, store)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), "llm-token") {
		t.Fatalf("command output leaked secret: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), "--name codereview/default-llm") {
		t.Fatalf("stderr = %q, want no llm follow-up hint when key already exists", errOut.String())
	}
}

func TestInitReplaceProfilePreservesExistingCredentialRefsByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Secrets: testFileSecretsConfig(),
		Profiles: map[string]config.Profile{
			"work": profileWithCredentialStore(config.Profile{
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-git"},
					CredentialRef: "codereview/custom-git",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-reviewer"},
					CredentialRef: "codereview/custom-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-llm"},
					CredentialRef: "codereview/custom-llm",
				},
			}, testFileCredentialStoreID),
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	store := newFakeInitStore(map[string]map[string]string{
		"custom-llm": {credentials.AnthropicAPIKeyKey: "llm-token"},
	})
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.replaceProfile = true
	flags.llmAuth = string(config.LLMAuthAPIKey)
	_, _, err := runNonInteractiveInitWithFakeStore(t, path, "work", strings.NewReader(""), flags, store)
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
	if profile.Git.Credential.Store != testFileCredentialStoreID || profile.Git.Credential.Name != "codereview/custom-git" {
		t.Fatalf("git credential = %#v, want preserved test-file custom-git", profile.Git.Credential)
	}
	if profile.ReviewerCredentials == nil || profile.ReviewerCredentials.CredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer = %#v, want preserved custom-reviewer", profile.ReviewerCredentials)
	}
	if profile.ReviewerCredentials.Credential.Store != testFileCredentialStoreID || profile.ReviewerCredentials.Credential.Name != "codereview/custom-reviewer" {
		t.Fatalf("reviewer credential = %#v, want preserved test-file custom-reviewer", profile.ReviewerCredentials.Credential)
	}
	if profile.LLM.CredentialRef != "codereview/custom-llm" {
		t.Fatalf("llm ref = %q, want preserved custom-llm", profile.LLM.CredentialRef)
	}
	if profile.LLM.Credential.Store != testFileCredentialStoreID || profile.LLM.Credential.Name != "codereview/custom-llm" {
		t.Fatalf("llm credential = %#v, want preserved test-file custom-llm", profile.LLM.Credential)
	}
}

func TestInitReplaceProfileRefOverwriteEmitsFollowUpHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
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
	if !strings.Contains(errOut.String(), "set-credential --store local-os --name codereview/rotated-git --key git_token --stdin") {
		t.Fatalf("stderr = %q, want overwrite-ref follow-up hint", errOut.String())
	}
}

func TestInitAPIKeyAuthRejectsMissingSecretWithoutWritingDanglingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.llmAuth = string(config.LLMAuthAPIKey)
	_, _, err := runNonInteractiveInitWithFakeStore(t, path, "", strings.NewReader(""), flags, newFakeInitStore(nil))
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if _, err := config.Load(path); !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("Load after failed init error = %v, want ErrNotConfigured", err)
	}
}

func TestInitMergeReplaceAndBackendConflictSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, config.File{
		Secrets: testFileSecretsConfig(),
		Profiles: map[string]config.Profile{
			"home": profileWithCredentialStore(basicProfile("home"), testFileCredentialStoreID),
		},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	t.Setenv("CR_GIT_TOKEN", "work-token")
	store := newFakeInitStore(nil)
	flags := defaultNonInteractiveInitOptionsForTest()
	flags.gitTokenEnv = "CR_GIT_TOKEN"
	_, _, err := runNonInteractiveInitWithFakeStore(t, path, "work", strings.NewReader(""), flags, store)
	if err != nil {
		t.Fatalf("merge absent profile Execute: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatalf("work profile missing after merge: %#v", cfg.Profiles)
	}
	if cfg.Profiles["work"].Git.Credential.Store != config.LocalOSCredentialStoreID {
		t.Fatalf("work git credential store = %q, want local-os for new profile", cfg.Profiles["work"].Git.Credential.Store)
	}
	assertFakeStored(t, store, "work", credentials.GitTokenKey, "work-token")

	cmd, _, _ := newTestCommand(path, strings.NewReader(""))
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
		t.Fatalf("runtime backend conflict exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
}

func TestInitSetDefaultFlagRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, config.File{
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
		"--set-" + "default",
	})
	removedFlag := "--set-" + "default"
	if err == nil || !strings.Contains(err.Error(), "unknown flag: "+removedFlag) {
		t.Fatalf("Execute error = %v, want unknown %s flag", err, removedFlag)
	}
}

func TestInitDurableKeyringBackendFlagsRemoved(t *testing.T) {
	t.Run("set backend flag is unavailable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		cmd, _, _ := newTestCommand(path, strings.NewReader(""))

		err := root.Execute(cmd, []string{
			"init",
			"--non-interactive",
			"--keyring-backend", "file",
		})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
		}
	})

	t.Run("reset backend flag is unavailable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		if err := config.Save(path, config.File{
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
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
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
	expected = normalizeTestProfile(expected)
	if !reflect.DeepEqual(got.Profiles["work"], expected) {
		t.Fatalf("saved profile = %#v, want %#v", got.Profiles["work"], expected)
	}
}

func TestInitReplaceProfilePreservesReviewerDisplayNameWhenReviewerIdentityUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "Work reviewer bot",
		IdentityCache: "reviewer-cache",
	}
	if err := config.Save(path, config.File{
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
		"--reviewer-auth-mode", string(config.GitAuthModePAT),
		"--reviewer-credential-ref", "codereview/work-reviewer",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["work"].ReviewerCredentials == nil {
		t.Fatal("reviewer credentials cleared unexpectedly")
	}
	if got.Profiles["work"].ReviewerCredentials.DisplayName != "Work reviewer bot" {
		t.Fatalf("display_name = %q, want preserved reviewer display name", got.Profiles["work"].ReviewerCredentials.DisplayName)
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
		expected = normalizeTestProfile(expected)
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
		Keyring: config.KeyringConfig{Backend: "file"},
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
		t.Fatalf("plan config profiles = %#v, want suggested profile", plan.cfg.Profiles)
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
		t.Fatalf("workspace config profiles = %#v, want draft profile", workspace.cfg.Profiles)
	}
}

func TestBuildInteractiveInitWorkspaceOnlyCreatesNamedProfile(t *testing.T) {
	opts := &root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	cfg := config.File{Profiles: map[string]config.Profile{}}
	draft := initDraft{
		ProfileName: "default",
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
	if _, ok := workspace.cfg.Profiles["default"]; !ok {
		t.Fatalf("workspace config profiles = %#v, want draft profile", workspace.cfg.Profiles)
	}
}

func TestInitGitScopeDraftRoundTripPreservesIdentityCacheFromPreviousProfile(t *testing.T) {
	git := config.GitConfig{
		Host:          "https://github.mycompany.com/",
		AuthMode:      config.GitAuthModeGitHubApp,
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
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
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
		CredentialRef: "codereview/work",
		IdentityCache: "rianjs-work",
	}

	scope := initGitScopeDraft{
		Host:            "github.mycompany.com",
		AuthMode:        config.GitAuthModePAT,
		CredentialStore: config.LocalOSCredentialStoreID,
		CredentialRef:   "codereview/work-2",
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
					Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "review-bot",
				}
				return p
			}(),
			previous: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModePAT,
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-bot",
			},
			want: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModePAT,
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
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
					Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "review-app",
				}
				return p
			}(),
			previous: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModeGitHubApp,
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
				CredentialRef: "codereview/work-reviewer",
				IdentityCache: "review-app",
			},
			want: &config.ReviewerCredentials{
				AuthMode:      config.GitAuthModeGitHubApp,
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
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
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
		CredentialRef: "codereview/work-reviewer",
		IdentityCache: "review-app",
	}
	entity := initReviewerEntityDraft{
		Kind:            initReviewerEntityKindPAT,
		AuthMode:        config.GitAuthModePAT,
		CredentialStore: config.LocalOSCredentialStoreID,
		CredentialRef:   "codereview/work-reviewer-2",
	}

	exported := entity.exportConfig(previous)

	if exported == nil {
		t.Fatal("exportConfig = nil, want separate reviewer credentials")
		return
	}
	if exported.IdentityCache != "" {
		t.Fatalf("IdentityCache = %q, want cleared on reviewer entity change", exported.IdentityCache)
	}
}

func TestBuildInteractiveInitWorkspaceClearsReviewerDisplayNameWhenDraftLeavesItBlank(t *testing.T) {
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "Old label",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": existing,
		},
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)
	draft.ReviewerEnabled = true
	draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	draft.ReviewerCredentialStore = config.LocalOSCredentialStoreID
	draft.ReviewerCredentialRef = "codereview/work-reviewer"
	draft.ReviewerDisplayName = ""

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{}, "", cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if workspace.profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials cleared unexpectedly")
	}
	if got := workspace.profile.ReviewerCredentials.DisplayName; got != "" {
		t.Fatalf("display name = %q, want cleared blank value", got)
	}
}

func TestBuildInitGitScopeInventoryDeduplicatesNormalizedGitHubEnterpriseHost(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.Git.Host = "https://github.mycompany.com/"
	work.Git.Host = "github.mycompany.com"
	home.Git.AuthMode = config.GitAuthModeGitHubApp
	work.Git.AuthMode = config.GitAuthModeGitHubApp
	home.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-ghe"}
	work.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-ghe"}
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

func TestInitReviewerEntityOptionsExcludeConfiguredGitIdentityFallback(t *testing.T) {
	options := initReviewerEntityOptions(map[string]initReviewerEntityDraft{
		"use-git-identity": {
			Name: "use-git-identity",
			Kind: initReviewerEntityKindUseGitIdentity,
		},
		"reviewer-pat": {
			Name:          "reviewer-pat",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/reviewer-pat",
		},
	}, reviewerEntityTemplateFallbackLabel())
	var fallbackCount int
	var configuredFallbackLabel string
	var configuredPATLabel string
	for _, option := range options {
		switch option.Value {
		case string(initReviewerEntityKindUseGitIdentity):
			fallbackCount++
		case "use-git-identity":
			configuredFallbackLabel = option.Key
		case "reviewer-pat":
			configuredPATLabel = option.Key
		}
	}
	if fallbackCount != 1 {
		t.Fatalf("fallbackCount = %d, want exactly one generic git-identity fallback option", fallbackCount)
	}
	if configuredFallbackLabel != "" {
		t.Fatalf("configuredFallbackLabel = %q, want no configured pseudo-entity fallback option", configuredFallbackLabel)
	}
	if configuredPATLabel == "" {
		t.Fatal("configured PAT reviewer option missing")
	}
	if got, want := configuredPATLabel, "reviewer-pat (PAT reviewer)"; got != want {
		t.Fatalf("configuredPATLabel = %q, want %q", got, want)
	}
}

func TestInitReviewerEntityOptionsExposeLiteralCreateLabels(t *testing.T) {
	options := initReviewerEntityOptions(
		map[string]initReviewerEntityDraft{},
		"Use a profile's Git account (no separate reviewer entity)",
	)

	var got []string
	for _, option := range options {
		got = append(got, option.Key)
	}

	want := []string{
		"Use a profile's Git account (no separate reviewer entity)",
		"Configure new personal access token (PAT) reviewer",
		"Configure new GitHub App reviewer",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("option labels = %#v, want %#v", got, want)
	}
}

func TestInitReviewerEntitySelectionOptionsOmitCreateActions(t *testing.T) {
	options := initReviewerEntitySelectionOptions(
		map[string]initReviewerEntityDraft{
			"reviewer-pat": {
				Name:          "reviewer-pat",
				Kind:          initReviewerEntityKindPAT,
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/reviewer-pat",
			},
		},
		"Post using this profile's Git account (GitHub PAT)",
	)

	got := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		got = append(got, huh.NewOption(option.Key, option.Value))
	}
	want := []huh.Option[string]{
		huh.NewOption("reviewer-pat (PAT reviewer)", "reviewer-pat"),
		huh.NewOption("Post using this profile's Git account (GitHub PAT)", string(initReviewerEntityKindUseGitIdentity)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
}

func TestInitReviewerEntityLabelUsesExplicitDisplayName(t *testing.T) {
	label := initReviewerEntityLabel(initReviewerEntityDraft{
		Name:          "reviewer-github-app",
		Kind:          initReviewerEntityKindGitHubApp,
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "OC Collective bot",
	})
	if got, want := label, "OC Collective bot (GitHub App reviewer)"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestBuildInitReviewerEntityInventoryConflictingSharedDisplayNamesFallBackToRefLabel(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "OC Collective bot",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "Work reviewer bot",
	}

	entities, profileEntityNames := buildInitReviewerEntityInventory(config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	})

	entity := entities[profileEntityNames["home"]]
	if entity.DisplayName != "" {
		t.Fatalf("entity.DisplayName = %q, want cleared when shared profiles disagree", entity.DisplayName)
	}
	if got, want := initReviewerEntityLabel(entity), "open-cli-collective-rianjs-bot (GitHub App reviewer)"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestInitReviewerEntityInventoryRowsUseNameFirstConfiguredLabels(t *testing.T) {
	rows := initReviewerEntityInventoryRows(initPromptContext{
		ExistingProfileName: "home",
		ReviewerEntities: map[string]initReviewerEntityDraft{
			"reviewer-app": {
				Name:          "reviewer-app",
				Kind:          initReviewerEntityKindGitHubApp,
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/open-cli-collective-rianjs-bot",
			},
			"reviewer-pat": {
				Name:          "reviewer-pat",
				Kind:          initReviewerEntityKindPAT,
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/default-reviewer",
			},
		},
	})

	var configuredTitles []string
	var commandTitles []string
	var commandIDs []string
	for _, row := range rows {
		switch row.Kind {
		case initInventoryRowKindActive:
			configuredTitles = append(configuredTitles, row.Title)
		case initInventoryRowKindPending:
			// No staged-delete rows are expected in this direct inventory rendering case.
		case initInventoryRowKindCommand:
			commandIDs = append(commandIDs, row.ID)
			commandTitles = append(commandTitles, row.Title)
		}
	}
	if got, want := configuredTitles, []string{
		"open-cli-collective-rianjs-bot (GitHub App reviewer)",
		"default-reviewer (PAT reviewer)",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredTitles = %#v, want %#v", got, want)
	}
	if got, want := commandIDs, []string{
		string(initReviewerEntityKindUseGitIdentity),
		string(initReviewerEntityKindPAT),
		string(initReviewerEntityKindGitHubApp),
		initBackSelection,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commandIDs = %#v, want %#v", got, want)
	}
	if got, want := commandTitles[1:], []string{
		reviewerEntityTemplatePATLabel(),
		reviewerEntityTemplateGitHubAppLabel(),
		"Back to main menu",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commandTitles[1:] = %#v, want %#v", got, want)
	}
}

func TestBuildInitReviewerEntityInventorySharedDisplayNameWinsWhenOnlyOneProfileNamesEntity(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "OC Collective bot",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
	}

	entities, profileEntityNames := buildInitReviewerEntityInventory(config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	})

	entity := entities[profileEntityNames["home"]]
	if got, want := entity.DisplayName, "OC Collective bot"; got != want {
		t.Fatalf("entity.DisplayName = %q, want %q", got, want)
	}
	if got, want := initReviewerEntityLabel(entity), "OC Collective bot (GitHub App reviewer)"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestEditInteractiveInitReviewerEntityStepPropagatesSharedDisplayName(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "Old home label",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "Old work label",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	session := initSessionDraft{
		cfg:                  cloneInitConfigFile(cfg),
		originalCfg:          cloneInitConfigFile(cfg),
		requestedProfileName: "home",
		touchedProfiles:      map[string]string{},
		writes:               map[string]map[string]string{},
		overwriteRefs:        map[string]bool{},
		satisfiedRefs:        map[string]bool{},
	}
	session = rebuildInteractiveInitWorkspace(session, "home")

	draft := seedInteractiveInitDraft("home", "home", &home)
	draft.ReviewerEnabled = true
	draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	draft.ReviewerCredentialRef = "codereview/open-cli-collective-rianjs-bot-renamed"
	draft.ReviewerDisplayName = "OC Collective bot"

	next, stayInCategory, err := editInteractiveInitReviewerEntityStep(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{
		reviewerPrompter: initReviewerEntityPrompterFunc(func(_ initReviewerEntityPrompt) (initDraft, error) {
			draft.ActionTarget = "reviewer-github-app"
			return draft, nil
		}),
		prompter: initPrompterFunc(func(_ initPromptContext) (initDraft, error) {
			t.Fatal("unexpected secret collection prompt")
			return initDraft{}, nil
		}),
	}, session)
	if err != nil {
		t.Fatalf("editInteractiveInitReviewerEntityStep: %v", err)
	}
	if stayInCategory {
		t.Fatal("stayInCategory = true, want focused reviewer flow to return to main menu after stage")
	}
	for _, profileName := range []string{"home", "work"} {
		profile := next.cfg.Profiles[profileName]
		if profile.ReviewerCredentials == nil {
			t.Fatalf("%s reviewer credentials cleared unexpectedly", profileName)
		}
		if got, want := profile.ReviewerCredentials.AuthMode, config.GitAuthModeGitHubApp; got != want {
			t.Fatalf("%s auth mode = %q, want %q", profileName, got, want)
		}
		if got, want := profile.ReviewerCredentials.CredentialRef, "codereview/open-cli-collective-rianjs-bot-renamed"; got != want {
			t.Fatalf("%s credential ref = %q, want %q", profileName, got, want)
		}
		if got, want := profile.ReviewerCredentials.DisplayName, "OC Collective bot"; got != want {
			t.Fatalf("%s display name = %q, want %q", profileName, got, want)
		}
	}
}

func TestEditInteractiveInitReviewerEntityStepPropagatesConcreteSharedReviewerRefAfterStandardNoOpEdit(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/home-reviewer",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/home-reviewer",
		DisplayName:   "Old work label",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	session := initSessionDraft{
		cfg:                  cloneInitConfigFile(cfg),
		originalCfg:          cloneInitConfigFile(cfg),
		requestedProfileName: "home",
		touchedProfiles:      map[string]string{},
		writes:               map[string]map[string]string{},
		overwriteRefs:        map[string]bool{},
		satisfiedRefs:        map[string]bool{},
	}
	session = rebuildInteractiveInitWorkspace(session, "home")

	draft := seedInteractiveInitDraft("home", "home", &home)
	draft.ReviewerEnabled = true
	draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	draft.ReviewerCredentialRef = ""
	draft.ReviewerDisplayName = "OC Collective bot"

	next, stayInCategory, err := editInteractiveInitReviewerEntityStep(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{
		reviewerPrompter: initReviewerEntityPrompterFunc(func(_ initReviewerEntityPrompt) (initDraft, error) {
			draft.ActionTarget = "reviewer-github-app"
			return draft, nil
		}),
		prompter: initPrompterFunc(func(_ initPromptContext) (initDraft, error) {
			t.Fatal("unexpected secret collection prompt")
			return initDraft{}, nil
		}),
	}, session)
	if err != nil {
		t.Fatalf("editInteractiveInitReviewerEntityStep: %v", err)
	}
	if stayInCategory {
		t.Fatal("stayInCategory = true, want focused reviewer flow to return to main menu after stage")
	}
	for _, profileName := range []string{"home", "work"} {
		profile := next.cfg.Profiles[profileName]
		if profile.ReviewerCredentials == nil {
			t.Fatalf("%s reviewer credentials cleared unexpectedly", profileName)
		}
		if got, want := profile.ReviewerCredentials.CredentialRef, "codereview/home-reviewer"; got != want {
			t.Fatalf("%s credential ref = %q, want %q", profileName, got, want)
		}
		if got, want := profile.ReviewerCredentials.DisplayName, "OC Collective bot"; got != want {
			t.Fatalf("%s display name = %q, want %q", profileName, got, want)
		}
	}
}

func TestEditInteractiveInitReviewerEntityStepSelectingFallbackDoesNotPropagateSharedEntity(t *testing.T) {
	home := basicProfile("home")
	work := basicProfile("work")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "Old home label",
	}
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
		DisplayName:   "Old work label",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	session := initSessionDraft{
		cfg:                  cloneInitConfigFile(cfg),
		originalCfg:          cloneInitConfigFile(cfg),
		requestedProfileName: "home",
		touchedProfiles:      map[string]string{},
		writes:               map[string]map[string]string{},
		overwriteRefs:        map[string]bool{},
		satisfiedRefs:        map[string]bool{},
	}
	session = rebuildInteractiveInitWorkspace(session, "home")

	draft := seedInteractiveInitDraft("home", "home", &home)
	draft.ReviewerEnabled = false
	draft.ReviewerAuth = ""
	draft.ReviewerCredentialRef = ""
	draft.ReviewerDisplayName = ""

	next, stayInCategory, err := editInteractiveInitReviewerEntityStep(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{
		reviewerPrompter: initReviewerEntityPrompterFunc(func(_ initReviewerEntityPrompt) (initDraft, error) {
			return draft, nil
		}),
		prompter: initPrompterFunc(func(_ initPromptContext) (initDraft, error) {
			t.Fatal("unexpected secret collection prompt")
			return initDraft{}, nil
		}),
	}, session)
	if err != nil {
		t.Fatalf("editInteractiveInitReviewerEntityStep: %v", err)
	}
	if stayInCategory {
		t.Fatal("stayInCategory = true, want focused reviewer flow to return to main menu after stage")
	}
	if got := next.cfg.Profiles["home"].ReviewerCredentials; got != nil {
		t.Fatalf("home reviewer credentials = %#v, want cleared fallback reviewer", got)
	}
	workProfile := next.cfg.Profiles["work"]
	if workProfile.ReviewerCredentials == nil {
		t.Fatal("work reviewer credentials cleared unexpectedly")
	}
	if got, want := workProfile.ReviewerCredentials.DisplayName, "Old work label"; got != want {
		t.Fatalf("work display name = %q, want %q", got, want)
	}
	if got, want := workProfile.ReviewerCredentials.CredentialRef, "codereview/open-cli-collective-rianjs-bot"; got != want {
		t.Fatalf("work credential ref = %q, want %q", got, want)
	}
}

func TestInitReviewerEntityOptionsUseProfileAwareFallbackLabelWhenProfileKnown(t *testing.T) {
	profile := basicProfile("home")
	options := initReviewerEntityOptions(map[string]initReviewerEntityDraft{}, focusedReviewerEntityFallbackLabel(&profile))
	if len(options) == 0 {
		t.Fatal("options = empty, want fallback option")
	}
	if got := options[0].Key; got != "Post using this profile's Git account (GitHub PAT)" {
		t.Fatalf("fallback label = %q, want focused profile-specific fallback label", got)
	}
}

func TestReviewerEntityGitAccountFallbackLabelUsesKnownIdentityPAT(t *testing.T) {
	if got, want := reviewerEntityGitAccountFallbackLabel(config.GitAuthModePAT, "rianjs"), "Post as rianjs (GitHub PAT)"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
}

func TestReviewerEntityGitAccountFallbackLabelUsesKnownIdentityGitHubApp(t *testing.T) {
	if got, want := reviewerEntityGitAccountFallbackLabel(config.GitAuthModeGitHubApp, "review-bot"), "Post as review-bot (GitHub App)"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
}

func TestReviewerEntityGitAccountFallbackLabelUsesUnknownIdentityPAT(t *testing.T) {
	if got, want := reviewerEntityGitAccountFallbackLabel(config.GitAuthModePAT, ""), "Post using this profile's Git account (GitHub PAT)"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
}

func TestReviewerEntityGitAccountFallbackLabelUsesUnknownIdentityGitHubApp(t *testing.T) {
	if got, want := reviewerEntityGitAccountFallbackLabel(config.GitAuthModeGitHubApp, ""), "Post using this profile's Git account (GitHub App)"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
}

func TestInitReviewerEntityInventoryRowsUseExplicitGitAccountFallbackLabels(t *testing.T) {
	cases := []struct {
		name     string
		git      config.GitConfig
		wantText string
	}{
		{
			name: "known PAT identity",
			git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/home",
				IdentityCache: "rianjs",
			},
			wantText: "Post as rianjs (GitHub PAT)",
		},
		{
			name: "known GitHub App identity",
			git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/home-app",
				IdentityCache: "review-bot",
			},
			wantText: "Post as review-bot (GitHub App)",
		},
		{
			name: "unknown PAT identity",
			git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/home",
			},
			wantText: "Post using this profile's Git account (GitHub PAT)",
		},
		{
			name: "unknown GitHub App identity",
			git: config.GitConfig{
				Host:          "github.com",
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/home-app",
			},
			wantText: "Post using this profile's Git account (GitHub App)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := basicProfile("home")
			profile.Git = tc.git
			rows := initReviewerEntityInventoryRows(initPromptContext{
				ExistingProfile: &profile,
			})
			if len(rows) == 0 {
				t.Fatal("rows = empty, want fallback command row")
			}
			if got := rows[0].Title; got != tc.wantText {
				t.Fatalf("fallback row = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestProfileEditorReviewerEntityFallbackLabelUsesExplicitGitAccountFallbackLabels(t *testing.T) {
	cases := []struct {
		name     string
		git      initGitScopeDraft
		existing *config.Profile
		wantText string
	}{
		{
			name: "known PAT identity",
			git: initGitScopeDraft{
				Host:          "github.com",
				AuthMode:      config.GitAuthModePAT,
				CredentialRef: "codereview/home",
			},
			existing: &config.Profile{
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
					IdentityCache: "rianjs",
				},
			},
			wantText: "Post as rianjs (GitHub PAT)",
		},
		{
			name: "unknown GitHub App identity",
			git: initGitScopeDraft{
				Host:          "github.com",
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/home-app",
			},
			existing: &config.Profile{
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/home-app",
				},
			},
			wantText: "Post using this profile's Git account (GitHub App)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := initReviewerEntitySelectionOptions(map[string]initReviewerEntityDraft{}, profileEditorReviewerEntityFallbackLabel(tc.git, tc.existing))
			if len(options) == 0 {
				t.Fatal("options = empty, want fallback option")
			}
			if got := options[0].Key; got != tc.wantText {
				t.Fatalf("fallback option = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestProfileEditorReviewerEntityFallbackLabelIgnoresStaleIdentityCacheWhenGitScopeChanges(t *testing.T) {
	existing := basicProfile("work")
	existing.Git.IdentityCache = "rianjs"
	label := profileEditorReviewerEntityFallbackLabel(initGitScopeDraft{
		Host:          "github.mycompany.com",
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-app",
	}, &existing)
	if got, want := label, "Post using this profile's Git account (GitHub App)"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
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
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-llm"},
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
				Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-llm"},
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
	home.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
	work.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
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

func TestBuildInitLLMRuntimeInventoryIncludesStandaloneRuntimes(t *testing.T) {
	home := basicProfile("home")
	home.LLM = config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}
	cfg := config.File{
		LLMRuntimes: map[string]config.LLMConfig{
			"codex-cli": home.LLM,
			"openai-api-key": {
				Provider: config.LLMProviderOpenAI,
				Auth:     config.LLMAuthAPIKey,
				Adapter:  config.LLMAdapterOpenAIAPI,
				Credential: config.CredentialLocation{
					Store: config.LocalOSCredentialStoreID,
					Name:  "codereview/openai-api-key",
				},
			},
		},
		Profiles: map[string]config.Profile{"home": home},
	}

	runtimes, profileRuntimeNames := buildInitLLMRuntimeInventory(cfg)

	if len(runtimes) != 2 {
		t.Fatalf("len(runtimes) = %d, want 2; runtimes=%#v", len(runtimes), runtimes)
	}
	if profileRuntimeNames["home"] != "codex-cli" {
		t.Fatalf("profileRuntimeNames = %#v, want home to reuse standalone codex-cli", profileRuntimeNames)
	}
	if runtimes["openai-api-key"].CredentialRef != "codereview/openai-api-key" {
		t.Fatalf("openai runtime = %#v, want configured credential ref", runtimes["openai-api-key"])
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
	openAI.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
	anthropic.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
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

func TestDeleteInteractiveInitProfilePrunesRoutesAndUndoRestores(t *testing.T) {
	work := basicProfile("work")
	alpha := basicProfile("alpha")
	home := basicProfile("home")
	session := initSessionDraft{
		cfg: config.File{
			RepositoryProfiles: []config.RepositoryProfile{
				{
					Profile: "work",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "rianjs",
					},
				},
				{
					Profile: "home",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "open-cli-collective",
					},
				},
			},
			Profiles: map[string]config.Profile{
				"alpha": alpha,
				"home":  home,
				"work":  work,
			},
		},
		touchedProfiles:              map[string]string{"work": "work"},
		pendingProfileDeletes:        map[string]initPendingProfileDelete{},
		pendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{},
		pendingLLMRuntimeDeletes:     map[string]initPendingLLMRuntimeDelete{},
	}
	session = rebuildInteractiveInitWorkspace(session, "work")

	next, err := deleteInteractiveInitProfile(session, "work")
	if err != nil {
		t.Fatalf("deleteInteractiveInitProfile: %v", err)
	}
	if _, ok := next.cfg.Profiles["work"]; ok {
		t.Fatalf("profiles after delete = %#v, want work removed", next.cfg.Profiles)
	}
	if len(next.cfg.RepositoryProfiles) != 1 || next.cfg.RepositoryProfiles[0].Profile != "home" {
		t.Fatalf("repository_profiles after delete = %#v, want only home route", next.cfg.RepositoryProfiles)
	}
	if next.workspace == nil || next.workspace.profileName != "alpha" {
		t.Fatalf("workspace after delete = %#v, want active alpha workspace", next.workspace)
	}
	if _, ok := next.pendingProfileDeletes["work"]; !ok {
		t.Fatalf("pendingProfileDeletes = %#v, want work pending delete", next.pendingProfileDeletes)
	}
	if _, ok := next.touchedProfiles["work"]; ok {
		t.Fatalf("touchedProfiles after delete = %#v, want work removed", next.touchedProfiles)
	}

	restored, err := undoInteractiveInitProfileDelete(next, "work")
	if err != nil {
		t.Fatalf("undoInteractiveInitProfileDelete: %v", err)
	}
	if !reflect.DeepEqual(restored.cfg.Profiles["work"], work) {
		t.Fatalf("restored work profile = %#v, want %#v", restored.cfg.Profiles["work"], work)
	}
	if len(restored.cfg.RepositoryProfiles) != 2 {
		t.Fatalf("repository_profiles after undo = %#v, want restored routes", restored.cfg.RepositoryProfiles)
	}
	if got := restored.touchedProfiles["work"]; got != "work" {
		t.Fatalf("touchedProfiles[work] = %q, want restored original marker", got)
	}
	if _, ok := restored.pendingProfileDeletes["work"]; ok {
		t.Fatalf("pendingProfileDeletes after undo = %#v, want cleared work entry", restored.pendingProfileDeletes)
	}
}

func TestDeleteInteractiveInitReviewerEntityClearsAffectedProfilesAndUndoSkipsReeditedProfiles(t *testing.T) {
	work := basicProfile("work")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	home := basicProfile("home")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	bot := basicProfile("bot")
	session := initSessionDraft{
		cfg: config.File{
			Profiles: map[string]config.Profile{
				"bot":  bot,
				"home": home,
				"work": work,
			},
		},
		pendingProfileDeletes:        map[string]initPendingProfileDelete{},
		pendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{},
		pendingLLMRuntimeDeletes:     map[string]initPendingLLMRuntimeDelete{},
	}
	session = rebuildInteractiveInitWorkspace(session, "work")
	_, profileEntityNames := buildInitReviewerEntityInventory(session.cfg)
	entityName := profileEntityNames["work"]

	next, err := deleteInteractiveInitReviewerEntity(session, entityName)
	if err != nil {
		t.Fatalf("deleteInteractiveInitReviewerEntity: %v", err)
	}
	if next.cfg.Profiles["work"].ReviewerCredentials != nil || next.cfg.Profiles["home"].ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials after delete = work:%#v home:%#v, want cleared", next.cfg.Profiles["work"].ReviewerCredentials, next.cfg.Profiles["home"].ReviewerCredentials)
	}
	if next.cfg.Profiles["bot"].ReviewerCredentials != nil {
		t.Fatalf("bot reviewer credentials = %#v, want untouched nil", next.cfg.Profiles["bot"].ReviewerCredentials)
	}

	editedHome := next.cfg.Profiles["home"]
	editedHome.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/home-reviewer",
	}
	next.cfg.Profiles["home"] = editedHome

	restored, err := undoInteractiveInitReviewerEntityDelete(next, entityName)
	if err != nil {
		t.Fatalf("undoInteractiveInitReviewerEntityDelete: %v", err)
	}
	if restored.cfg.Profiles["work"].ReviewerCredentials == nil {
		t.Fatal("work reviewer credentials = nil, want restored shared reviewer")
	}
	if got := restored.cfg.Profiles["home"].ReviewerCredentials; got == nil || got.CredentialRef != "codereview/home-reviewer" {
		t.Fatalf("home reviewer credentials after undo = %#v, want preserved re-edit", got)
	}
}

func TestDeleteInteractiveInitLLMRuntimeRebindsAffectedProfilesAndUndoSkipsReeditedProfiles(t *testing.T) {
	work := basicProfile("work")
	home := basicProfile("home")
	bot := basicProfile("bot")
	bot.LLM = config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}
	session := initSessionDraft{
		cfg: config.File{
			Profiles: map[string]config.Profile{
				"bot":  bot,
				"home": home,
				"work": work,
			},
		},
		pendingProfileDeletes:        map[string]initPendingProfileDelete{},
		pendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{},
		pendingLLMRuntimeDeletes:     map[string]initPendingLLMRuntimeDelete{},
	}
	session = rebuildInteractiveInitWorkspace(session, "work")
	_, profileRuntimeNames := buildInitLLMRuntimeInventory(session.cfg)
	runtimeName := profileRuntimeNames["work"]
	replacement := initLLMRuntimeDraft{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterOpenAIAPI,
		CredentialRef: "codereview/shared-llm",
	}

	next, err := deleteInteractiveInitLLMRuntime(session, runtimeName, replacement)
	if err != nil {
		t.Fatalf("deleteInteractiveInitLLMRuntime: %v", err)
	}
	for _, profileName := range []string{"work", "home"} {
		llm := next.cfg.Profiles[profileName].LLM
		if llm.Provider != config.LLMProviderOpenAI || llm.Auth != config.LLMAuthAPIKey || llm.Adapter != config.LLMAdapterOpenAIAPI || llm.CredentialRef != "codereview/shared-llm" {
			t.Fatalf("%s llm after delete = %#v, want shared openai api-key replacement", profileName, llm)
		}
	}
	if !reflect.DeepEqual(next.cfg.Profiles["bot"].LLM, bot.LLM) {
		t.Fatalf("bot llm after delete = %#v, want untouched %#v", next.cfg.Profiles["bot"].LLM, bot.LLM)
	}

	editedHome := next.cfg.Profiles["home"]
	editedHome.LLM = config.LLMConfig{
		Provider: config.LLMProviderPi,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterPiRPC,
	}
	next.cfg.Profiles["home"] = editedHome

	restored, err := undoInteractiveInitLLMRuntimeDelete(next, runtimeName)
	if err != nil {
		t.Fatalf("undoInteractiveInitLLMRuntimeDelete: %v", err)
	}
	if !reflect.DeepEqual(restored.cfg.Profiles["work"].LLM, work.LLM) {
		t.Fatalf("work llm after undo = %#v, want %#v", restored.cfg.Profiles["work"].LLM, work.LLM)
	}
	if !reflect.DeepEqual(restored.cfg.Profiles["home"].LLM, editedHome.LLM) {
		t.Fatalf("home llm after undo = %#v, want preserved re-edit %#v", restored.cfg.Profiles["home"].LLM, editedHome.LLM)
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
	home.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
	work.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	draft := seedInteractiveInitDraft("home", "home", &home)

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
	existing.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/office-git"}
	existing.Git.CredentialRef = "codereview/office-git"
	existing.Git.IdentityCache = "git-cache"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/office-reviewer"},
		CredentialRef: "codereview/office-reviewer",
		IdentityCache: "reviewer-cache",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)

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
				ProfileName:        "office",
				GitHost:            "github.mycompany.com",
				GitAuth:            string(config.GitAuthModePAT),
				GitCredentialStore: config.LocalOSCredentialStoreID,
				GitCredentialRef:   "codereview/office-git",
				ReviewerEnabled:    false,
				LLMProvider:        string(config.LLMProviderAnthropic),
				LLMAuth:            string(config.LLMAuthSubscription),
				LLMAdapter:         string(config.LLMAdapterClaudeCLI),
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
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
				ProfileName:             "office",
				GitHost:                 "github.mycompany.com",
				GitAuth:                 string(config.GitAuthModePAT),
				GitCredentialStore:      config.LocalOSCredentialStoreID,
				GitCredentialRef:        "codereview/office-git",
				ReviewerEnabled:         true,
				ReviewerAuth:            string(config.GitAuthModeGitHubApp),
				ReviewerCredentialStore: config.LocalOSCredentialStoreID,
				ReviewerCredentialRef:   "codereview/office-reviewer",
				LLMProvider:             string(config.LLMProviderAnthropic),
				LLMAuth:                 string(config.LLMAuthSubscription),
				LLMAdapter:              string(config.LLMAdapterClaudeCLI),
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
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

func TestCollectInteractiveInitSecretsPassesDestinationToSharedCredentialPrompts(t *testing.T) {
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"team-vault": {
					Label:   "Team Vault",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, config.Profile{SecretsProfile: "team-vault"})
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	refs := []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: string(config.GitAuthModePAT)},
		{Purpose: "llm", Ref: "codereview/work-llm", Mode: string(config.LLMAuthAPIKey), Provider: string(config.LLMProviderAnthropic)},
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: string(config.GitAuthModePAT)},
	}
	entries := make([]initCredentialPlanEntry, 0, len(refs))
	for _, ref := range refs {
		specs, err := credentials.KeySpecsForPurpose(ref)
		if err != nil {
			t.Fatalf("KeySpecsForPurpose(%#v): %v", ref, err)
		}
		entries = append(entries, initCredentialPlanEntry{
			Ref:                 ref,
			SecretsProfile:      resolved,
			KeySpecs:            specs,
			MissingRequiredKeys: initCredentialRequiredKeys(initCredentialPlanEntry{KeySpecs: specs}),
			State:               initCredentialPlanStateMissingRequired,
		})
	}
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{
			initCredentialSecretActionSetNow,
			initCredentialSecretActionSetNow,
			initCredentialSecretActionSetNow,
		},
		sources: []initSecretSource{
			initSecretSourcePaste,
			initSecretSourcePaste,
			initSecretSourcePaste,
		},
		pastes: []string{"git-token", "llm-key", "reviewer-token"},
	}
	workspace, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}, initWorkspaceDraft{
		cfg:            cfg,
		credentialPlan: entries,
	})
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if len(workspace.writes) != 3 {
		t.Fatalf("writes = %#v, want three staged credential refs", workspace.writes)
	}
	for _, prompt := range prompter.actionPrompts {
		if !strings.Contains(prompt.Destination, "Destination: Team Vault / "+prompt.Entry.Ref.Ref) ||
			!strings.Contains(prompt.Destination, "Credential store: team-vault") ||
			!strings.Contains(prompt.Destination, "Backend kind: file") {
			t.Fatalf("action prompt destination = %q for %#v", prompt.Destination, prompt.Entry.Ref)
		}
	}
	for _, prompt := range prompter.sourcePrompts {
		if !strings.Contains(prompt.Destination, "Destination: Team Vault / "+prompt.Entry.Ref.Ref) ||
			!strings.Contains(prompt.Destination, "Credential store: team-vault") ||
			!strings.Contains(prompt.Destination, "Backend kind: file") {
			t.Fatalf("source prompt destination = %q for %#v", prompt.Destination, prompt.Entry.Ref)
		}
	}
	for _, prompt := range prompter.pastePrompts {
		if !strings.Contains(prompt.Destination, "Destination: Team Vault / "+prompt.Entry.Ref.Ref) ||
			!strings.Contains(prompt.Destination, "Credential store: team-vault") ||
			!strings.Contains(prompt.Destination, "Backend kind: file") {
			t.Fatalf("paste prompt destination = %q for %#v", prompt.Destination, prompt.Entry.Ref)
		}
	}
}

func TestCollectInteractiveInitSecretsDestinationUsesRawRuntimeBackend(t *testing.T) {
	t.Setenv(credentials.BackendEnvVar(), "")
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
	}
	_, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{Backend: string(credstore.BackendMemory)}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
	}, initWorkspaceDraft{
		cfg: config.File{},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: string(config.GitAuthModePAT)},
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:              config.LocalOSCredentialStoreID,
				Label:           "OS credential store",
				Backend:         config.ProjectedOSCredentialStoreBackendKind,
				Source:          config.EffectiveSecretsProfileSourceProjectedLegacy,
				SelectionSource: credentials.SecretsProfileSelectionBuiltInOS,
			},
			KeySpecs:            []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}},
			MissingRequiredKeys: []string{credentials.GitTokenKey},
			State:               initCredentialPlanStateMissingRequired,
		}},
		backendFlagSet: true,
		backendArg:     " --backend memory",
	})
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if len(prompter.actionPrompts) != 1 {
		t.Fatalf("action prompts = %d, want 1", len(prompter.actionPrompts))
	}
	destination := prompter.actionPrompts[0].Destination
	if !strings.Contains(destination, "Destination: "+initBuiltInOSCredentialStoreTitle()+" / codereview/work") ||
		!strings.Contains(destination, "Backend kind: memory") {
		t.Fatalf("destination = %q, want raw runtime backend metadata", destination)
	}
	if strings.Contains(destination, "credential destination unavailable") {
		t.Fatalf("destination = %q, want available runtime backend summary", destination)
	}
}

func TestCollectInteractiveInitSecretsDestinationUsesInferredDefaultBackend(t *testing.T) {
	t.Setenv(credentials.BackendEnvVar(), "")
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
	}
	_, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
	}, initWorkspaceDraft{
		cfg: config.File{},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: string(config.GitAuthModePAT)},
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:              config.LocalOSCredentialStoreID,
				Label:           "OS credential store",
				Backend:         config.ProjectedOSCredentialStoreBackendKind,
				Source:          config.EffectiveSecretsProfileSourceProjectedLegacy,
				SelectionSource: credentials.SecretsProfileSelectionBuiltInOS,
			},
			KeySpecs:            []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}},
			MissingRequiredKeys: []string{credentials.GitTokenKey},
			State:               initCredentialPlanStateMissingRequired,
		}},
	})
	if err != nil {
		t.Fatalf("collectInteractiveInitSecrets: %v", err)
	}
	if len(prompter.actionPrompts) != 1 {
		t.Fatalf("action prompts = %d, want 1", len(prompter.actionPrompts))
	}
	destination := prompter.actionPrompts[0].Destination
	if !strings.Contains(destination, "Destination: "+initBuiltInOSCredentialStoreTitle()+" / codereview/work") ||
		!strings.Contains(destination, "Backend kind: auto") {
		t.Fatalf("destination = %q, want inferred built-in credential-store copy", destination)
	}
	if strings.Contains(destination, "credential destination unavailable") {
		t.Fatalf("destination = %q, want available inferred backend summary", destination)
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
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
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
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

func TestCollectInteractiveInitSecretsMapsNamedSecretsProfileOpenConflictAsConfigError(t *testing.T) {
	workspace := testInitSecretWorkspace()
	workspace.credentialPlan[0].SecretsProfile = credentials.ResolvedSecretsProfile{
		ID:      "work-file",
		Label:   "Work File Store",
		Backend: string(credstore.BackendFile),
		Source:  config.EffectiveSecretsProfileSourceConfigured,
	}
	workspace.backendFlagSet = true
	prompter := &fakeInitSecretPrompter{
		actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
	}

	_, err := collectInteractiveInitSecrets(&cobra.Command{}, &root.Options{Backend: "memory"}, initDeps{
		secretPrompter:     prompter,
		clipboardSupported: func() bool { return false },
		clipboardRead: func() (string, error) {
			t.Fatal("clipboard should not be read")
			return "", nil
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore should not be used for configured credential stores")
			return nil, nil
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return nil, fmt.Errorf("%w: configured credential store conflict", config.ErrInvalid)
		},
	}, workspace)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("collectInteractiveInitSecrets error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestCollectInteractiveInitSessionSecretsMapsNamedSecretsProfileLoadConflictAsConfigError(t *testing.T) {
	plan := initSessionPlan{
		cfg: config.File{},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "git",
				Ref:     "codereview/default",
				Mode:    string(config.GitAuthModePAT),
			},
			State: initCredentialPlanStateMissingRequired,
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:      "work-file",
				Label:   "Work File Store",
				Backend: string(credstore.BackendFile),
				Source:  config.EffectiveSecretsProfileSourceConfigured,
			},
		}},
	}

	_, err := collectInteractiveInitSessionSecrets(&root.Options{Backend: "memory"}, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore should not be used for configured credential stores")
			return nil, nil
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return nil, fmt.Errorf("%w: configured credential store conflict", config.ErrInvalid)
		},
	}, plan)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("collectInteractiveInitSessionSecrets error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestMergeInteractiveInitSessionPlanEntryRejectsSameRefAcrossDifferentSecretsProfiles(t *testing.T) {
	entriesByKey := map[string]initCredentialPlanEntry{}
	first := initCredentialPlanEntry{
		Ref:   config.CredentialRef{Purpose: "git", Ref: "codereview/shared", Mode: string(config.GitAuthModePAT)},
		State: initCredentialPlanStateMissingRequired,
		SecretsProfile: credentials.ResolvedSecretsProfile{
			ID:      "personal-memory",
			Backend: string(credstore.BackendMemory),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
	}
	if err := mergeInteractiveInitSessionPlanEntry(entriesByKey, first); err != nil {
		t.Fatalf("merge first entry: %v", err)
	}

	err := mergeInteractiveInitSessionPlanEntry(entriesByKey, initCredentialPlanEntry{
		Ref:   config.CredentialRef{Purpose: "git", Ref: "codereview/shared", Mode: string(config.GitAuthModePAT)},
		State: initCredentialPlanStateMissingRequired,
		SecretsProfile: credentials.ResolvedSecretsProfile{
			ID:      "work-file",
			Backend: string(credstore.BackendFile),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
	})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("merge conflicting entry error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "multiple credential stores") {
		t.Fatalf("merge conflicting entry error = %v, want conflict detail", err)
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

func TestWriteInitCredentialPlanHintsForDeferredGitHubAppUsesRequiredKeysOnly(t *testing.T) {
	var stderr bytes.Buffer
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
		State: initCredentialPlanStateDefer,
	}

	if err := writeInitCredentialPlanHints(&stderr, "", entry); err != nil {
		t.Fatalf("writeInitCredentialPlanHints: %v", err)
	}
	got := stderr.String()
	for _, key := range []string{credentials.GitHubAppIDKey, credentials.GitHubAppPrivateKeyKey} {
		if !strings.Contains(got, "cr set-credential --store local-os --name codereview/rianjs-bot --key "+key+" --stdin") {
			t.Fatalf("stderr = %q, want required setup hint for %s", got, key)
		}
	}
	if strings.Contains(got, "--key "+credentials.GitHubAppInstallationIDKey+" --stdin") {
		t.Fatalf("stderr = %q, want optional installation id omitted from required setup hints", got)
	}
}

func TestHuhInitPrompterAccessiblePrefillsExistingProfile(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := apiKeyProfile("work", config.LLMProviderOpenAI)
	existing.Git.Host = "gitlab.com"
	existing.Git.AuthMode = config.GitAuthModeGitHubApp
	existing.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-git"}
	existing.LLM.ReviewerModelTier = config.ModelTierMedium
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModeGitHubApp,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-reviewer"},
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git storage label
			"", // Reviewer storage label
			"", // LLM ref
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		ExistingConfig: config.File{
			RepositoryProfiles: []config.RepositoryProfile{{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "gitlab.com",
					Namespace: "open-cli-collective",
					Repos:     []string{"codereview-cli"},
				},
			}},
			Profiles: map[string]config.Profile{"work": existing},
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
	if draft.GitHost != "gitlab.com" || draft.GitAuth != string(config.GitAuthModeGitHubApp) || draft.GitCredentialRef != "codereview/custom-git" {
		t.Fatalf("git draft = %#v, want existing values", draft)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) || draft.ReviewerCredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer draft = %#v, want existing reviewer settings", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) || draft.LLMReviewerModelTier != string(config.ModelTierMedium) || draft.LLMCredentialRef != "codereview/work-llm" {
		t.Fatalf("llm draft = %#v, want existing api-key openai values", draft)
	}
	if !draft.RoutesSet {
		t.Fatalf("draft.RoutesSet = false, want profile form to own route edits")
	}
	if !reflect.DeepEqual(draft.Routes, []configedit.RepositoryRouteSpec{{
		Host:      "gitlab.com",
		Namespace: "open-cli-collective",
		Repos:     []string{"codereview-cli"},
	}}) {
		t.Fatalf("draft.Routes = %#v, want prefilled route from inline editor", draft.Routes)
	}
	out := stderr.String()
	if !strings.Contains(out, "Choose a profile to edit or create") || !strings.Contains(out, "Git scope host") || !strings.Contains(out, "Reviewer entity") || !strings.Contains(out, "LLM runtime") {
		t.Fatalf("wizard output missing expected prompts: %q", out)
	}
	routeIndex := strings.Index(out, "Routes tell cr when to use this profile automatically.")
	reviewerIndex := strings.Index(out, "Reviewer entity")
	if routeIndex < 0 || reviewerIndex < 0 || routeIndex > reviewerIndex {
		t.Fatalf("wizard output order = %q, want automatic profile selection before reviewer entity", out)
	}
	if strings.Contains(out, "Secrets management") {
		t.Fatalf("wizard output unexpectedly showed inert secrets management selector: %q", out)
	}
	if !strings.Contains(out, "Minimum reviewer model tier") ||
		!strings.Contains(out, "Built-in baseline (small)") ||
		!strings.Contains(out, "Small baseline") ||
		!strings.Contains(out, "Medium baseline") ||
		!strings.Contains(out, "Large baseline") {
		t.Fatalf("wizard output missing reviewer model-tier baseline guidance: %q", out)
	}
	if strings.Contains(out, "Reviewer model tier") || strings.Contains(out, "Built-in default") {
		t.Fatalf("wizard output still contains legacy reviewer model-tier copy: %q", out)
	}
	if strings.Contains(out, "Git credential ref") || strings.Contains(out, "LLM credential ref") {
		t.Fatalf("wizard output unexpectedly exposed raw credential refs on the primary path: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "paste a secret") {
		t.Fatalf("wizard output unexpectedly requested secret ingress: %q", out)
	}
	staleProfileHeading := "Default " + "profile"
	staleAlreadySelected := "This profile is already the " + "default " + "profile."
	if strings.Contains(out, staleProfileHeading) || strings.Contains(out, staleAlreadySelected) {
		t.Fatalf("wizard output still shows removed profile-selection prompt in profile editor: %q", out)
	}
	staleMakeSelected := "Make this the " + "default " + "profile"
	staleKeepSelected := "No, keep the current " + "default " + "profile"
	if strings.Contains(out, staleMakeSelected) || strings.Contains(out, staleKeepSelected) {
		t.Fatalf("wizard output contains removed profile-selection copy: %q", out)
	}
}

func TestHuhInitPrompterAccessibleOmitsSecretsManagementFromProfileEditor(t *testing.T) {
	t.Setenv("TERM", "dumb")
	work := basicProfile("work")
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"team-vault": {
					Label: "Team Vault",
					Backend: config.SecretsProfileBackend{
						Kind: config.SecretsBackendKind(credstore.BackendFile),
					},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"work": work,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	secretsProfiles, profileSecretsProfiles, brokenProfileSecretsProfiles := buildInitSecretsProfileInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Route entries
			"", // Reviewer entity
			"", // Secrets management
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName:         "work",
		ExistingProfileName:          "work",
		ExistingProfile:              &work,
		ExistingProfileNames:         []string{"work"},
		ExistingConfig:               cfg,
		GitScopes:                    gitScopes,
		ProfileGitScopes:             profileGitScopes,
		ReviewerEntities:             reviewerEntities,
		ProfileReviewerEntities:      profileReviewerEntities,
		SecretsProfiles:              secretsProfiles,
		ProfileSecretsProfiles:       profileSecretsProfiles,
		BrokenProfileSecretsProfiles: brokenProfileSecretsProfiles,
		LLMRuntimes:                  llmRuntimes,
		ProfileLLMRuntimes:           profileLLMRuntimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if strings.Contains(out, "Secrets management") || strings.Contains(out, "Team Vault") {
		t.Fatalf("stderr = %q, want profile editor to omit secrets-management selector", out)
	}
	if draft.SecretsProfile != "" {
		t.Fatalf("draft.SecretsProfile = %q, want unchanged default secrets profile", draft.SecretsProfile)
	}
}

func TestHuhInitPrompterAccessibleKeepsFallbackReviewerSelectedInMixedInventory(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := basicProfile("home")
	home.Git.IdentityCache = "rianjs"
	work := basicProfile("work")
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
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "home\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "home",
					Title: "home",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName:    "home",
		ExistingProfileName:     "home",
		ExistingProfile:         &home,
		ExistingProfileNames:    []string{"home"},
		ExistingConfig:          cfg,
		GitScopes:               gitScopes,
		ProfileGitScopes:        profileGitScopes,
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: profileReviewerEntities,
		LLMRuntimes:             llmRuntimes,
		ProfileLLMRuntimes:      profileLLMRuntimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.ReviewerEnabled {
		t.Fatalf("draft reviewer = %#v, want fallback profile to remain on git identity", draft)
	}
	if !strings.Contains(stderr.String(), "Post as rianjs (GitHub PAT)") {
		t.Fatalf("stderr = %q, want profile-editor fallback label", stderr.String())
	}
}

func TestHuhInitPrompterAccessibleConfiguredReviewerHidesInlineReviewerEntityEditing(t *testing.T) {
	t.Setenv("TERM", "dumb")
	work := basicProfile("work")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "Work reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": work,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git storage label
			"", // Reviewer storage label
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &work,
		ExistingProfileNames:    []string{"work"},
		ExistingConfig:          cfg,
		GitScopes:               gitScopes,
		ProfileGitScopes:        profileGitScopes,
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: profileReviewerEntities,
		LLMRuntimes:             llmRuntimes,
		ProfileLLMRuntimes:      profileLLMRuntimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !draft.ReviewerEnabled {
		t.Fatalf("draft reviewer = %#v, want configured reviewer entity to stay selected", draft)
	}
	if got, want := draft.ReviewerDisplayName, "Work reviewer"; got != want {
		t.Fatalf("draft.ReviewerDisplayName = %q, want %q", got, want)
	}
	out := stderr.String()
	if !strings.Contains(out, "Work reviewer (PAT reviewer)") {
		t.Fatalf("stderr = %q, want configured reviewer label in profile editor", out)
	}
	if strings.Contains(out, "Reviewer entity label") || strings.Contains(out, "Configure new personal access token (PAT) reviewer") || strings.Contains(out, "Configure new GitHub App reviewer") {
		t.Fatalf("stderr = %q, want profile editor to select existing reviewers without inline create/edit controls", out)
	}
}

func TestHuhInitPrompterAccessibleConfiguredLLMRuntimeHidesInlineRuntimeEditing(t *testing.T) {
	t.Setenv("TERM", "dumb")
	work := apiKeyProfile("work", config.LLMProviderOpenAI)
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": work,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &work,
		ExistingProfileNames:    []string{"work"},
		ExistingConfig:          cfg,
		GitScopes:               gitScopes,
		ProfileGitScopes:        profileGitScopes,
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: profileReviewerEntities,
		LLMRuntimes:             llmRuntimes,
		ProfileLLMRuntimes:      profileLLMRuntimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) {
		t.Fatalf("draft llm = %#v, want existing runtime retained", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Configured: OpenAI API key") {
		t.Fatalf("stderr = %q, want configured runtime label in profile editor", out)
	}
	if strings.Contains(out, "Template: Claude CLI subscription") ||
		strings.Contains(out, "Template: Codex CLI subscription") ||
		strings.Contains(out, "Custom compatible runtime") ||
		strings.Contains(out, "LLM provider") ||
		strings.Contains(out, "LLM auth mode") ||
		strings.Contains(out, "LLM adapter") {
		t.Fatalf("stderr = %q, want profile editor to select existing runtimes without inline runtime setup controls", out)
	}
}

func TestHuhInitPrompterAccessibleProfileEditorBootstrapsNewLLMRuntimeWhenNoneConfigured(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	llmPrompterCalled := false
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime bootstrap action
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git storage label
			"", // Profile name (rerender after runtime setup)
			"", // Make default
			"", // Git scope
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime (new staged runtime selected)
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git storage label
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmPrompterCalled = true
			if len(prompt.Context.LLMRuntimes) != 0 {
				t.Fatalf("prompt.Context.LLMRuntimes = %#v, want empty first-run bootstrap inventory", prompt.Context.LLMRuntimes)
			}
			draft := seedInteractiveInitDraft("default", "", nil)
			draft.LLMProvider = string(config.LLMProviderOpenAI)
			draft.LLMAuth = string(config.LLMAuthSubscription)
			draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
			return draft, nil
		}),
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !llmPrompterCalled {
		t.Fatal("llmPrompterCalled = false, want profile editor bootstrap action to enter runtime setup")
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthSubscription) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft llm = %#v, want configured bootstrap runtime applied", draft)
	}
	if !strings.Contains(stderr.String(), "Configure a new LLM runtime first") {
		t.Fatalf("stderr = %q, want explicit first-run runtime bootstrap action", stderr.String())
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfileFallsBackToFirstConfiguredRuntimeWithoutProfileMapping(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime: default to first configured runtime
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git storage label
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"alpha-runtime": {
				Name:          "alpha-runtime",
				Preset:        initLLMRuntimePresetAnthropicAPIKey,
				Provider:      config.LLMProviderAnthropic,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterAnthropicAPI,
				CredentialRef: "codereview/alpha-llm",
			},
			"zeta-runtime": {
				Name:          "zeta-runtime",
				Preset:        initLLMRuntimePresetOpenAIAPIKey,
				Provider:      config.LLMProviderOpenAI,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterOpenAIAPI,
				CredentialRef: "codereview/zeta-llm",
			},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderAnthropic) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterAnthropicAPI) || draft.LLMCredentialRef != "codereview/alpha-llm" {
		t.Fatalf("draft llm = %#v, want create-new profile fallback to select the first configured runtime when no profile mapping exists", draft)
	}
	if !strings.Contains(stderr.String(), "Configured: Anthropic API key") || !strings.Contains(stderr.String(), "Configured: OpenAI API key") {
		t.Fatalf("stderr = %q, want both configured runtimes shown in create-new fallback flow", stderr.String())
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
			"", // Profile name
			"", // Make default
			"", // Git host
			"", // Git auth
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		ExistingConfig: config.File{
			Profiles: map[string]config.Profile{"work": existing},
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
	if draft.ProfileName != "new-profile" {
		t.Fatalf("draft.ProfileName = %q, want non-conflicting create-new profile seed", draft.ProfileName)
	}
	if draft.GitHost != "github.com" || draft.GitAuth != string(config.GitAuthModePAT) {
		t.Fatalf("git draft = %#v, want fresh defaults for create-new seed", draft)
	}
	if draft.ReviewerEnabled {
		t.Fatalf("reviewer draft = %#v, want create-new seed to avoid inherited separate reviewer", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) {
		t.Fatalf("llm draft = %#v, want create-new profile to select the existing runtime inventory by default", draft)
	}
	if draft.LLMReviewerModelTier != "" {
		t.Fatalf("draft.LLMReviewerModelTier = %q, want built-in baseline selection to serialize as empty", draft.LLMReviewerModelTier)
	}
	out := stderr.String()
	if !strings.Contains(out, "Post using this profile's Git account (GitHub PAT)") {
		t.Fatalf("stderr = %q, want explicit fallback label for create-new profile flow", out)
	}
	if !strings.Contains(out, "Reviewer entity") {
		t.Fatalf("stderr = %q, want reviewer entity prompt in create-new profile flow", out)
	}
	if !strings.Contains(out, "Configured: OpenAI API key") {
		t.Fatalf("stderr = %q, want create-new profile flow to show existing runtime inventory", out)
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfileOmitsSelectionPrompt(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.ProfileName != "default" {
		t.Fatalf("draft.ProfileName = %q, want requested profile name", draft.ProfileName)
	}
	staleMakeSelected := "Yes, make this profile the " + "default"
	staleProfileHeading := "Default " + "profile"
	if strings.Contains(stderr.String(), staleMakeSelected) || strings.Contains(stderr.String(), staleProfileHeading) {
		t.Fatalf("stderr = %q, want profile editor to omit removed selection copy", stderr.String())
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfileAvoidsExistingProfileName(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("default")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Git host
			"", // Git auth
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		ExistingProfileName:  "default",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"default"},
		ExistingConfig: config.File{
			Profiles: map[string]config.Profile{"default": existing},
		},
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"default-runtime": initLLMRuntimeDraftFromConfig(existing.LLM),
		},
		ProfileLLMRuntimes: map[string]string{"default": "default-runtime"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.OriginalProfileName != "" {
		t.Fatalf("draft.OriginalProfileName = %q, want blank for create-new seed", draft.OriginalProfileName)
	}
	if draft.ProfileName != "new-profile" {
		t.Fatalf("draft.ProfileName = %q, want create-new seed to avoid existing suggested-name profile", draft.ProfileName)
	}
}

func TestInitCreateProfileSeedNameUsesNextAvailableGeneratedName(t *testing.T) {
	got := initCreateProfileSeedName(initPromptContext{
		RequestedProfileName: "default",
		ExistingProfileNames: []string{"default", "new-profile"},
		ExistingConfig: config.File{
			Profiles: map[string]config.Profile{
				"default":     {},
				"new-profile": {},
			},
		},
		PendingProfileDeletes: map[string]initPendingProfileDelete{
			"new-profile-2": {ProfileName: "new-profile-2"},
		},
	})
	if got != "new-profile-3" {
		t.Fatalf("initCreateProfileSeedName = %q, want next available generated name", got)
	}
}

func TestProfileEditorSelectionPreservesSelectedReviewerEntityLabel(t *testing.T) {
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "Old label",
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)

	reviewerEntities := map[string]initReviewerEntityDraft{
		"work-reviewer": initReviewerEntityDraftFromConfig(existing),
	}
	selectedReviewerEntity := "work-reviewer"
	applyReviewerEntityInventorySelection(&draft, selectedReviewerEntity, reviewerEntities)
	reviewerMode := string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	applyReviewerEntitySelection(&draft, reviewerMode)

	if got, want := draft.ReviewerDisplayName, "Old label"; got != want {
		t.Fatalf("draft.ReviewerDisplayName = %q, want %q", got, want)
	}
}

func TestHuhInitPrompterAccessibleCanMarkExistingProfileForDeletion(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionStageDelete,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig: config.File{
			Profiles: map[string]config.Profile{"work": existing},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.Action != initDraftActionDeleteProfile || draft.ActionTarget != "work" {
		t.Fatalf("draft delete action = %#v, want delete work", draft)
	}
	if !strings.Contains(stderr.String(), "work") {
		t.Fatalf("stderr = %q, want profile inventory label", stderr.String())
	}
}

func TestHuhInitPrompterAccessibleNewProfileFlowShowsBackOption(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\nBack to main menu\n")
			return initInventoryResult{
				Action: initInventoryActionBack,
				Row: initInventoryRow{
					ID:            initBackSelection,
					Title:         "Back to main menu",
					PrimaryAction: initInventoryActionBack,
				},
			}, nil
		},
	}

	_, err := prompter.Run(initPromptContext{ExistingConfig: config.File{Profiles: map[string]config.Profile{}}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("Run error = %v, want errInitNavigateBack", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Create new profile") || !strings.Contains(out, "Back to main menu") {
		t.Fatalf("stderr = %q, want visible back option for new-profile flow", out)
	}
}

func TestHuhInitPrompterAccessibleCanRestorePendingDeletedProfile(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := basicProfile("home")
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "work (Staged for deletion)\n")
			return initInventoryResult{
				Action: initInventoryActionRestore,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work (Staged for deletion)",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "home",
		ExistingProfileName:  "home",
		ExistingProfile:      &home,
		ExistingProfileNames: []string{"home"},
		ExistingConfig: config.File{
			Profiles: map[string]config.Profile{"home": home},
		},
		PendingProfileDeletes: map[string]initPendingProfileDelete{
			"work": {ProfileName: "work"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.Action != initDraftActionUndoDeleteProfile || draft.ActionTarget != "work" {
		t.Fatalf("draft undo action = %#v, want restore work", draft)
	}
	if !strings.Contains(stderr.String(), "work (Staged for deletion)") {
		t.Fatalf("stderr = %q, want restore label", stderr.String())
	}
}

func TestHuhInitReviewerEntityDetailsAccessibleCanMarkConfiguredEntityForDeletion(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "reviewer-github-app (GitHub App reviewer)\n")
			return initInventoryResult{
				Action: initInventoryActionStageDelete,
				Row: initInventoryRow{
					ID:    "reviewer-github-app",
					Title: "reviewer-github-app (GitHub App reviewer)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{
		Context: initPromptContext{
			RequestedProfileName: "work",
			ExistingProfileName:  "work",
			ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": basicProfile("work")}},
			ReviewerEntities: map[string]initReviewerEntityDraft{
				"reviewer-github-app": {
					Name:          "reviewer-github-app",
					Kind:          initReviewerEntityKindGitHubApp,
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/shared-reviewer",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if draft.Action != initDraftActionDeleteReviewerEntity || draft.ActionTarget != "reviewer-github-app" {
		t.Fatalf("draft delete action = %#v, want reviewer-github-app delete", draft)
	}
	if !strings.Contains(stderr.String(), "reviewer-github-app (GitHub App reviewer)") {
		t.Fatalf("stderr = %q, want reviewer inventory label", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleCanRestorePendingDeletedEntity(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "reviewer-github-app (Staged for deletion)\n")
			return initInventoryResult{
				Action: initInventoryActionRestore,
				Row: initInventoryRow{
					ID:    "reviewer-github-app",
					Title: "reviewer-github-app (Staged for deletion)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{
		Context: initPromptContext{
			RequestedProfileName:    "work",
			ExistingProfileName:     "work",
			ExistingProfile:         &existing,
			ExistingConfig:          config.File{Profiles: map[string]config.Profile{"work": existing}},
			ReviewerEntities:        map[string]initReviewerEntityDraft{},
			ProfileReviewerEntities: map[string]string{"work": string(initReviewerEntityKindUseGitIdentity)},
			PendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{
				"reviewer-github-app": {EntityName: "reviewer-github-app"},
			},
		},
	})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if draft.Action != initDraftActionUndoDeleteReviewerEntity || draft.ActionTarget != "reviewer-github-app" {
		t.Fatalf("draft undo reviewer action = %#v, want reviewer-github-app restore", draft)
	}
	if !strings.Contains(stderr.String(), "reviewer-github-app (Staged for deletion)") {
		t.Fatalf("stderr = %q, want reviewer restore label", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleKeepsFallbackSelectedInMixedInventory(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := basicProfile("home")
	home.Git.IdentityCache = "rianjs"
	work := basicProfile("work")
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
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Post as rianjs (GitHub PAT)\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            string(initReviewerEntityKindUseGitIdentity),
					Title:         "Post as rianjs (GitHub PAT)",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{
		Context: initPromptContext{
			RequestedProfileName:    "home",
			ExistingProfileName:     "home",
			ExistingProfile:         &home,
			ExistingConfig:          cfg,
			ReviewerEntities:        reviewerEntities,
			ProfileReviewerEntities: profileReviewerEntities,
		},
	})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if draft.ReviewerEnabled {
		t.Fatalf("draft reviewer = %#v, want focused reviewer flow to preserve git-identity fallback", draft)
	}
	if !strings.Contains(stderr.String(), "Post as rianjs (GitHub PAT)") {
		t.Fatalf("stderr = %q, want focused fallback label", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleConfiguredReviewerRoundTripsInMixedInventory(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := basicProfile("home")
	work := basicProfile("work")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/custom-work-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldGitHubAppID:         "12345",
			initReviewerEntityFieldGitHubAppPrivateKey: testReviewerGitHubAppPrivateKey(),
		}, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "custom-work-reviewer (PAT reviewer)\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "reviewer-pat",
					Title: "custom-work-reviewer (PAT reviewer)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{
		Context: initPromptContext{
			RequestedProfileName:    "home",
			ExistingProfileName:     "home",
			ExistingProfile:         &home,
			ExistingConfig:          cfg,
			ReviewerEntities:        reviewerEntities,
			ProfileReviewerEntities: profileReviewerEntities,
		},
	})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModePAT) || draft.ReviewerCredentialRef != "codereview/custom-work-reviewer" {
		t.Fatalf("draft reviewer = %#v, want configured PAT reviewer to round-trip intact", draft)
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
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\nBack to main menu\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
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
	if !strings.Contains(stderr.String(), "Back to main menu") {
		t.Fatalf("stderr = %q, want visible back option for new-profile flow", stderr.String())
	}
}

func TestHuhInitPrompterAccessibleCreateNewProfilePreservesExplicitRequestedNameWhenNoProfileMatched(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
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
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
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
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr:       &stderr,
		editorRunner: stageLLMRuntimeEditorRunner(t, nil, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Configured: Claude CLI subscription (claude-cli)\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "claude-cli",
					Title: "Configured: Claude CLI subscription (claude-cli)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
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
	if !strings.Contains(out, "LLM provider") || !strings.Contains(out, "LLM auth mode") || !strings.Contains(out, "LLM adapter") || !strings.Contains(out, "Runtime detail action") || !strings.Contains(out, "Stage these runtime details") {
		t.Fatalf("stderr = %q, want configured runtime flow to show editable details", out)
	}
	if strings.Contains(out, "Edit runtime details") || strings.Contains(out, "Mark runtime for deletion") || strings.Contains(out, "Back to runtime choices") {
		t.Fatalf("stderr = %q, want configured runtime flow without intermediate runtime action menu", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleTemplateShowsDetails(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr:       &stderr,
		editorRunner: stageLLMRuntimeEditorRunner(t, nil, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Template: Codex CLI subscription\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            string(initLLMRuntimePresetCodexCLISubscription),
					Title:         "Template: Codex CLI subscription",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
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
		!strings.Contains(out, "Runtime detail action") ||
		!strings.Contains(out, "Stage these runtime details") {
		t.Fatalf("stderr = %q, want template selection to land on flattened runtime details", out)
	}
	if strings.Contains(out, "Stage this runtime") || strings.Contains(out, "Customize runtime details") || strings.Contains(out, "Back to runtime choices") {
		t.Fatalf("stderr = %q, want template selection without intermediate runtime action menu", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleTemplateShowsAvailabilityNote(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	checkerCalled := false
	prompter := huhInitLLMRuntimePrompter{
		stderr:       &stderr,
		editorRunner: stageLLMRuntimeEditorRunner(t, nil, ""),
		checker: func(preset initLLMRuntimePreset) string {
			if preset == initLLMRuntimePresetCodexCLISubscription {
				checkerCalled = true
			}
			return "Codex CLI check: codex-cli 0.139.0 installed."
		},
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Template: Codex CLI subscription\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            string(initLLMRuntimePresetCodexCLISubscription),
					Title:         "Template: Codex CLI subscription",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	_, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if !checkerCalled {
		t.Fatal("checkerCalled = false, want template selection to consult runtime availability checker")
	}
	if !strings.Contains(stderr.String(), "Runtime detail action") ||
		!strings.Contains(stderr.String(), "Codex CLI") ||
		!strings.Contains(stderr.String(), "codex-cli 0.139.0 installed.") {
		t.Fatalf("stderr = %q, want flattened runtime detail screen with runtime availability note", stderr.String())
	}
}

func TestInitLLMRuntimeSelectionDescriptionCodexSubscriptionExplainsAdapterManagedAuth(t *testing.T) {
	description := initLLMRuntimeSelectionDescription(initLLMRuntimeDraft{
		Preset:   initLLMRuntimePresetCodexCLISubscription,
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}, "Codex CLI check: codex-cli 0.139.0 installed.")
	if !strings.Contains(description, "Uses your existing Codex CLI login.") {
		t.Fatalf("description = %q, want existing Codex CLI login guidance", description)
	}
	if !strings.Contains(description, "does not store a Codex subscription secret") {
		t.Fatalf("description = %q, want no-secret guidance", description)
	}
	if !strings.Contains(description, "Codex CLI check: codex-cli 0.139.0 installed.") {
		t.Fatalf("description = %q, want CLI availability note", description)
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
		Profiles: map[string]config.Profile{"work": existing},
	}
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr: &stderr,
		editorRunner: stageLLMRuntimeEditorRunner(t, map[initLinearFieldID]string{
			initLLMRuntimeFieldProvider: string(config.LLMProviderOpenAI),
			initLLMRuntimeFieldAuth:     string(config.LLMAuthAPIKey),
			initLLMRuntimeFieldAdapter:  string(config.LLMAdapterOpenAIAPI),
		}, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Custom compatible runtime\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCustomLLMRuntimeSelection,
					Title:         "Custom compatible runtime",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
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
	if !strings.Contains(out, "LLM provider") || !strings.Contains(out, "LLM auth mode") || !strings.Contains(out, "LLM adapter") {
		t.Fatalf("stderr = %q, want custom runtime fields", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Back to main menu\n")
			return initInventoryResult{
				Action: initInventoryActionBack,
				Row: initInventoryRow{
					ID:            initBackSelection,
					Title:         "Back to main menu",
					PrimaryAction: initInventoryActionBack,
				},
			}, nil
		},
	}

	_, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
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
	draft := seedInteractiveInitDraft("work", "work", nil)
	draft.LLMProvider = string(config.LLMProviderOpenAI)
	draft.LLMAuth = string(config.LLMAuthSubscription)
	draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
	want := draft
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr:       &stderr,
		editorRunner: stageLLMRuntimeEditorRunner(t, nil, initDetailActionBack),
	}

	_, back, err := prompter.editLLMRuntimeDetails(draft)
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

func TestHuhInitLLMRuntimePrompterAccessibleCanMarkConfiguredRuntimeForDeletion(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"1", // Replacement: Template Codex CLI subscription
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Configured: Claude CLI subscription (claude-cli)\n")
			return initInventoryResult{
				Action: initInventoryActionStageDelete,
				Row: initInventoryRow{
					ID:    "claude-cli",
					Title: "Configured: Claude CLI subscription (claude-cli)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.Action != initDraftActionDeleteLLMRuntime || draft.ActionTarget != "claude-cli" {
		t.Fatalf("draft delete action = %#v, want claude-cli delete", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthSubscription) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft replacement = %#v, want codex subscription replacement", draft)
	}
	if !strings.Contains(stderr.String(), "Replacement LLM runtime") {
		t.Fatalf("stderr = %q, want runtime replacement prompt", stderr.String())
	}
}

func TestHuhInitLLMRuntimePrompterReplacementChoosesConfiguredTemplate(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"1", // Replacement: Template Codex CLI subscription
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.chooseLLMRuntimeDeleteReplacement(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}}, "claude-cli", seedInteractiveInitDraft("work", "work", &existing))
	if err != nil {
		t.Fatalf("chooseLLMRuntimeDeleteReplacement: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthSubscription) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft replacement = %#v, want codex subscription replacement", draft)
	}
	if !strings.Contains(stderr.String(), "Replacement LLM runtime") {
		t.Fatalf("stderr = %q, want replacement runtime prompt", stderr.String())
	}
}

func TestHuhInitLLMRuntimePrompterReplacementBackExcludesDeletedRuntime(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"6", // Back to runtime details
			"",
		}, "\n")),
		stderr: &stderr,
	}

	_, err := prompter.chooseLLMRuntimeDeleteReplacement(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}}, "claude-cli", seedInteractiveInitDraft("work", "work", &existing))
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("chooseLLMRuntimeDeleteReplacement error = %v, want errInitNavigateBack", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Replacement LLM runtime") || !strings.Contains(out, "Template: Codex CLI subscription") {
		t.Fatalf("stderr = %q, want replacement runtime prompt with replacement options", out)
	}
	if strings.Contains(out, "Configured: Claude CLI subscription (claude-cli)") {
		t.Fatalf("stderr = %q, want deleted runtime excluded from replacement choices", out)
	}
	if strings.Contains(out, "Template: Claude CLI subscription") {
		t.Fatalf("stderr = %q, want deleted runtime equivalent template excluded from replacement choices", out)
	}
}

func TestHuhInitLLMRuntimePrompterAccessibleCanRestorePendingDeletedRuntime(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "claude-cli (Staged for deletion)\n")
			return initInventoryResult{
				Action: initInventoryActionRestore,
				Row: initInventoryRow{
					ID:    "claude-cli",
					Title: "claude-cli (Staged for deletion)",
				},
			}, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
		LLMRuntimes:          map[string]initLLMRuntimeDraft{},
		PendingLLMRuntimeDeletes: map[string]initPendingLLMRuntimeDelete{
			"claude-cli": {RuntimeName: "claude-cli"},
		},
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.Action != initDraftActionUndoDeleteLLMRuntime || draft.ActionTarget != "claude-cli" {
		t.Fatalf("draft undo runtime action = %#v, want claude-cli restore", draft)
	}
	if !strings.Contains(stderr.String(), "claude-cli (Staged for deletion)") {
		t.Fatalf("stderr = %q, want runtime restore label", stderr.String())
	}
}

func TestHuhInitLLMRuntimePrompterDefaultUsesLinearRuntimeFlow(t *testing.T) {
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr: &stderr,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initLLMRuntimeFieldAction)
			model = selectInitLinearFieldValue(t, model, initLLMRuntimeFieldAction, initDetailActionEdit)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.LLMProvider != string(config.LLMProviderAnthropic) || draft.LLMAdapter != string(config.LLMAdapterClaudeCLI) {
		t.Fatalf("draft = %#v, want default claude runtime", draft)
	}
	out := stderr.String()
	for _, want := range []string{
		"LLM runtime",
		"Runtime",
		"Configured: Claude CLI subscription (claude-cli)",
		"Runtime details",
		"LLM provider",
		"[x] Anthropic",
		"LLM auth mode",
		"[x] Subscription",
		"LLM adapter",
		"[x] Claude CLI",
		"Runtime action",
		"Stage these runtime details",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	assertContentOrder(t, out, "Runtime", "Runtime details", "LLM provider", "LLM auth mode", "LLM adapter", "Runtime action")
	if strings.Contains(out, "Back to main menu") {
		t.Fatalf("stderr = %q, want action-local Back without staging instead of inventory Back", out)
	}
}

func TestHuhInitLLMRuntimePrompterDefaultCanDeleteWithInlineReplacement(t *testing.T) {
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitLLMRuntimePrompter{
		stderr: &stderr,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 60)
			model = focusInitLinearField(t, model, initLLMRuntimeFieldSelection)
			initial := model.View()
			if strings.Contains(initial, "Stage runtime deletion") {
				t.Fatalf("initial view exposes delete staging action before delete shortcut:\n%s", initial)
			}
			_, _ = io.WriteString(out, initial)
			updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			if cmd != nil {
				t.Fatal("delete shortcut returned quit command before choosing replacement")
			}
			if next.resultAction != "" {
				t.Fatalf("resultAction after delete shortcut = %q, want empty until replacement is staged", next.resultAction)
			}
			if next.document.fieldHidden(initLLMRuntimeFieldReplacement) {
				t.Fatal("replacement field hidden after delete shortcut")
			}
			if next.document[next.focused].ID != initLLMRuntimeFieldReplacement {
				t.Fatalf("focused field after delete shortcut = %q, want replacement field", next.document[next.focused].ID)
			}
			replacementIndex := next.document.fieldIndexByID(initLLMRuntimeFieldReplacement)
			for _, option := range next.document[replacementIndex].Options {
				if strings.Contains(option.Label, "Claude CLI subscription") {
					t.Fatalf("replacement options include deleted runtime equivalent: %#v", option)
				}
			}
			next = selectInitLinearFieldValue(t, next, initLLMRuntimeFieldReplacement, string(initLLMRuntimePresetCodexCLISubscription))
			next = focusInitLinearField(t, next, initLLMRuntimeFieldAction)
			_, _ = io.WriteString(out, "\n\nAfter delete shortcut:\n"+next.View())
			updated, _ = next.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok = updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T after staging, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.Action != initDraftActionDeleteLLMRuntime || draft.ActionTarget != "claude-cli" {
		t.Fatalf("draft action = %#v, want delete claude-cli", draft)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft replacement = %#v, want codex-cli replacement", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "d delete") {
		t.Fatalf("stderr = %q, want delete shortcut help", out)
	}
	if !strings.Contains(out, "Replacement LLM runtime") || !strings.Contains(out, "Stage runtime deletion") {
		t.Fatalf("stderr = %q, want inline replacement and delete staging action", out)
	}
}

func TestInitLLMRuntimeLinearEditorRejectsCustomDeleteReplacementMatchingDeleted(t *testing.T) {
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}
	seed := seedInteractiveInitDraft(ctx.RequestedProfileName, ctx.ExistingProfileName, ctx.ExistingProfile)
	model := newInitLinearEditorModel(initLLMRuntimeLinearEditor(ctx, seed, func(initLLMRuntimePreset) string { return "" }), 160, 60)
	model = focusInitLinearField(t, model, initLLMRuntimeFieldSelection)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if cmd != nil {
		t.Fatal("delete shortcut returned quit command before choosing replacement")
	}
	for range 4 {
		updated, cmd = next.Update(tea.KeyMsg{Type: tea.KeyDown})
		if cmd != nil {
			t.Fatal("replacement selection returned quit command")
		}
		next, ok = updated.(initLinearEditorModel)
		if !ok {
			t.Fatalf("Update returned %T while choosing custom replacement", updated)
		}
	}
	if got := next.document.selectedValue(initLLMRuntimeFieldReplacement); got != initCustomLLMRuntimeSelection {
		t.Fatalf("replacement = %q, want custom compatible runtime", got)
	}
	next = focusInitLinearField(t, next, initLLMRuntimeFieldAction)
	updated, cmd = next.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok = updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T after staging attempt", updated)
	}
	if cmd != nil {
		t.Fatal("unchanged custom replacement staged deletion, want inline validation error")
	}
	if next.resultAction != "" {
		t.Fatalf("resultAction = %q, want no staged action", next.resultAction)
	}
	actionIndex := next.document.fieldIndexByID(initLLMRuntimeFieldAction)
	if actionIndex < 0 || !strings.Contains(next.document[actionIndex].Error, "choose a replacement LLM runtime") {
		t.Fatalf("action error = %q, want replacement validation error", next.document[actionIndex].Error)
	}
}

func TestInitLLMRuntimeLinearEditorChangingSelectionResetsDeleteMode(t *testing.T) {
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}
	seed := seedInteractiveInitDraft(ctx.RequestedProfileName, ctx.ExistingProfileName, ctx.ExistingProfile)
	model := newInitLinearEditorModel(initLLMRuntimeLinearEditor(ctx, seed, func(initLLMRuntimePreset) string { return "" }), 160, 60)
	model = focusInitLinearField(t, model, initLLMRuntimeFieldSelection)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if cmd != nil {
		t.Fatal("delete shortcut returned quit command before choosing replacement")
	}
	if got := next.document.selectedValue(initLLMRuntimeFieldAction); got != initLLMRuntimeActionDelete {
		t.Fatalf("action after delete shortcut = %q, want delete", got)
	}
	next = focusInitLinearField(t, next, initLLMRuntimeFieldSelection)
	updated, cmd = next.Update(tea.KeyMsg{Type: tea.KeyDown})
	next, ok = updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T after changing runtime selection", updated)
	}
	if cmd != nil {
		t.Fatal("changing runtime selection returned quit command")
	}
	if got := next.document.selectedValue(initLLMRuntimeFieldAction); got != initDetailActionEdit {
		t.Fatalf("action after changing runtime selection = %q, want edit", got)
	}
	if !next.document.fieldHidden(initLLMRuntimeFieldReplacement) {
		t.Fatal("replacement field visible after changing runtime selection, want delete mode reset")
	}
	actionIndex := next.document.fieldIndexByID(initLLMRuntimeFieldAction)
	for _, option := range next.document[actionIndex].Options {
		if option.Value == initLLMRuntimeActionDelete {
			t.Fatalf("delete action still available after runtime selection changed: %#v", option)
		}
	}
}

func TestHuhInitLLMRuntimePrompterDefaultPreservesConfiguredAPIKeyRuntimeRef(t *testing.T) {
	existing := basicProfile("work")
	existing.LLM = config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterOpenAIAPI,
		CredentialRef: "codereview/custom-openai",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": existing},
	}
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	prompter := huhInitLLMRuntimePrompter{
		stderr:       &bytes.Buffer{},
		editorRunner: stageLLMRuntimeEditorRunner(t, nil, ""),
	}

	draft, err := prompter.EditLLMRuntime(initLLMRuntimePrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       cfg,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	}})
	if err != nil {
		t.Fatalf("EditLLMRuntime: %v", err)
	}
	if draft.LLMCredentialRef != "codereview/custom-openai" {
		t.Fatalf("LLMCredentialRef = %q, want configured runtime ref", draft.LLMCredentialRef)
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Back to main menu\n")
			return initInventoryResult{
				Action: initInventoryActionBack,
				Row: initInventoryRow{
					ID:            initBackSelection,
					Title:         "Back to main menu",
					PrimaryAction: initInventoryActionBack,
				},
			}, nil
		},
	}

	_, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditReviewerEntity error = %v, want errInitNavigateBack", err)
	}
	if !strings.Contains(stderr.String(), "Back to main menu") {
		t.Fatalf("stderr = %q, want focused reviewer Back option", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleChoiceShowsDetails(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldGitToken: "reviewer-pat",
		}, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Configure new personal access token (PAT) reviewer\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            string(initReviewerEntityKindPAT),
					Title:         reviewerEntityTemplatePATLabel(),
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModePAT) {
		t.Fatalf("draft = %#v, want PAT reviewer", draft)
	}
	out := stderr.String()
	if !strings.Contains(out, "Reviewer detail action") || !strings.Contains(out, "Stage reviewer settings") || !strings.Contains(out, "Back without staging") || !strings.Contains(out, "Entity label") || !strings.Contains(out, "Reviewer secret location") {
		t.Fatalf("stderr = %q, want reviewer details screen", out)
	}
	if strings.Contains(out, "Reviewer label action") || strings.Contains(out, "Use this reviewer label") || strings.Contains(out, "Reviewer secret location action") || strings.Contains(out, "Use this reviewer secret location") || strings.Contains(out, "Custom reviewer secret location") || strings.Contains(out, "Use the standard reviewer secret location (recommended)") || strings.Contains(out, "Use a custom reviewer secret location (advanced)") {
		t.Fatalf("stderr = %q, want flattened reviewer editor", out)
	}
}

func TestHuhInitReviewerEntityPrompterNewTemplateDoesNotInheritCustomSecretLocation(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/custom-work-reviewer",
		DisplayName:   "Old reviewer",
	}
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldGitHubAppID:         "12345",
			initReviewerEntityFieldGitHubAppPrivateKey: testReviewerGitHubAppPrivateKey(),
		}, ""),
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, "Configure new GitHub App reviewer\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            string(initReviewerEntityKindGitHubApp),
					Title:         reviewerEntityTemplateGitHubAppLabel(),
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) {
		t.Fatalf("draft = %#v, want GitHub App reviewer", draft)
	}
	if draft.AdvancedStorageLabels {
		t.Fatalf("draft.AdvancedStorageLabels = true, want standard reviewer secret location")
	}
	if got := draft.ReviewerCredentialRef; got != "" {
		t.Fatalf("draft.ReviewerCredentialRef = %q, want empty standard location for new template", got)
	}
	if strings.Contains(stderr.String(), "Custom reviewer secret location") {
		t.Fatalf("stderr = %q, want custom secret location hidden on standard path", stderr.String())
	}
}

func TestFinalizeReviewerEntityEditorDraftPersistsCustomSecretLocation(t *testing.T) {
	draft := seedInteractiveInitDraft("work", "work", nil)
	finalizeReviewerEntityEditorDraft(
		&draft,
		"",
		"",
		"",
		"codereview/open-cli-collective-rianjs-bot",
		"codereview/work-reviewer",
		false,
	)
	if !draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = false, want true for custom reviewer secret location")
	}
	if got, want := draft.ReviewerCredentialRef, "codereview/open-cli-collective-rianjs-bot"; got != want {
		t.Fatalf("draft.ReviewerCredentialRef = %q, want %q", got, want)
	}
	if got := draft.ReviewerDisplayName; got != "" {
		t.Fatalf("draft.ReviewerDisplayName = %q, want empty", got)
	}
}

func TestReviewerEntityEditorLabelSeedUsesExplicitDisplayNameWhenPresent(t *testing.T) {
	labelInput, explicitDisplayName, fallbackLabelSeed := reviewerEntityEditorLabelSeed(initReviewerEntityDraft{
		Kind:          initReviewerEntityKindGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "OC Collective bot",
	})
	if got, want := labelInput, "OC Collective bot"; got != want {
		t.Fatalf("labelInput = %q, want %q", got, want)
	}
	if got, want := explicitDisplayName, "OC Collective bot"; got != want {
		t.Fatalf("explicitDisplayName = %q, want %q", got, want)
	}
	if got := fallbackLabelSeed; got != "" {
		t.Fatalf("fallbackLabelSeed = %q, want empty", got)
	}
}

func TestReviewerEntityEditorLabelSeedUsesFallbackIdentityWhenDisplayNameMissing(t *testing.T) {
	labelInput, explicitDisplayName, fallbackLabelSeed := reviewerEntityEditorLabelSeed(initReviewerEntityDraft{
		Kind:          initReviewerEntityKindGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
	})
	if got, want := labelInput, "open-cli-collective-rianjs-bot"; got != want {
		t.Fatalf("labelInput = %q, want %q", got, want)
	}
	if got := explicitDisplayName; got != "" {
		t.Fatalf("explicitDisplayName = %q, want empty", got)
	}
	if got, want := fallbackLabelSeed, "open-cli-collective-rianjs-bot"; got != want {
		t.Fatalf("fallbackLabelSeed = %q, want %q", got, want)
	}
}

func TestHuhInitReviewerEntityPrompterExistingReviewerCustomSecretLocationPersists(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/custom-work-reviewer",
		DisplayName:   "Old reviewer",
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, ""),
	}

	nextDraft, back, err := prompter.editExistingReviewerEntity(initPromptContext{}, initReviewerEntityDraftFromConfig(existing), draft)
	if err != nil {
		t.Fatalf("editExistingReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want staged custom reviewer secret location")
	}
	if !nextDraft.ReviewerEnabled || nextDraft.ReviewerAuth != string(config.GitAuthModePAT) {
		t.Fatalf("draft = %#v, want PAT reviewer", nextDraft)
	}
	if !nextDraft.AdvancedStorageLabels {
		t.Fatalf("draft.AdvancedStorageLabels = false, want true for custom reviewer secret location; stderr=%q", stderr.String())
	}
	if got, want := nextDraft.ReviewerCredentialRef, "codereview/custom-work-reviewer"; got != want {
		t.Fatalf("draft.ReviewerCredentialRef = %q, want %q; stderr=%q", got, want, stderr.String())
	}
	if strings.Contains(stderr.String(), "Custom reviewer secret location") {
		t.Fatalf("stderr = %q, want flattened reviewer editor without nested custom secret prompt", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterExistingReviewerFallbackSeedDoesNotPersistDisplayName(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/open-cli-collective-rianjs-bot",
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, ""),
	}

	nextDraft, back, err := prompter.editExistingReviewerEntity(initPromptContext{}, initReviewerEntityDraftFromConfig(existing), draft)
	if err != nil {
		t.Fatalf("editExistingReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want edited reviewer details")
	}
	if got := nextDraft.ReviewerDisplayName; got != "" {
		t.Fatalf("draft.ReviewerDisplayName = %q, want empty when unchanged fallback seed is staged; stderr=%q", got, stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterAccessibleShowsSeededDisplayNamePrompt(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "Old label",
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)
	draft.ReviewerEnabled = true
	draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
	draft.ReviewerCredentialRef = "codereview/work-reviewer"
	draft.ReviewerDisplayName = "Old label"
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, ""),
	}

	nextDraft, back, err := prompter.editExistingReviewerEntity(initPromptContext{}, initReviewerEntityDraftFromConfig(existing), draft)
	if err != nil {
		t.Fatalf("editExistingReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want edited reviewer details")
	}
	if got, want := nextDraft.ReviewerDisplayName, "Old label"; got != want {
		t.Fatalf("draft.ReviewerDisplayName = %q, want %q; stderr=%q", got, want, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Entity label") {
		t.Fatalf("stderr = %q, want reviewer entity label prompt", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterExistingReviewerCanEditLabel(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	draft := seedInteractiveInitDraft("work", "work", &existing)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldLabel: "OC Collective bot",
		}, ""),
	}

	nextDraft, back, err := prompter.editExistingReviewerEntity(initPromptContext{}, initReviewerEntityDraftFromConfig(existing), draft)
	if err != nil {
		t.Fatalf("editExistingReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want edited reviewer details")
	}
	if got, want := nextDraft.ReviewerDisplayName, "OC Collective bot"; got != want {
		t.Fatalf("draft.ReviewerDisplayName = %q, want %q; stderr=%q", got, want, stderr.String())
	}
	if got, want := nextDraft.ReviewerCredentialRef, ""; got != want {
		t.Fatalf("draft.ReviewerCredentialRef = %q, want %q standard reviewer location semantics; stderr=%q", got, want, stderr.String())
	}
	if nextDraft.AdvancedStorageLabels {
		t.Fatalf("draft.AdvancedStorageLabels = true, want standard reviewer location semantics after keeping current location; stderr=%q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Custom reviewer secret location") {
		t.Fatalf("stderr = %q, want single-screen reviewer editor", stderr.String())
	}
}

func TestHuhInitReviewerEntityPrompterDefaultUsesLinearReviewerFlow(t *testing.T) {
	existing := basicProfile("work")
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 60)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindPAT))
			model.setFieldValue(initReviewerEntityFieldGitToken, "reviewer-pat")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitToken))
			model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)
			_, _ = io.WriteString(out, model.View())
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModePAT) {
		t.Fatalf("draft = %#v, want PAT reviewer", draft)
	}
	if got := draft.ReviewerCredentialWrites[credentials.GitTokenKey]; got != "reviewer-pat" {
		t.Fatalf("staged reviewer PAT = %q, want inline token", got)
	}
	out := stderr.String()
	for _, want := range []string{
		"Reviewer entity",
		"Configure new personal access token (PAT) reviewer",
		"Reviewer details",
		"Personal access token (PAT) reviewer",
		"Entity label",
		"Reviewer secret location",
		"Reviewer credential status",
		"This is a credential name, not a PAT",
		credentials.GitTokenKey,
		"staged",
		"Reviewer credential values",
		"GitHub PAT",
		"Reviewer action",
		"Stage reviewer settings",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	assertContentOrder(t, out, "Reviewer entity", "Reviewer details", "Entity label", "Reviewer secret location", "Reviewer credential status", "Reviewer action")
	if strings.Contains(out, "reviewer-pat") {
		t.Fatalf("stderr leaked reviewer PAT value:\n%s", out)
	}
	if strings.Contains(out, "Back to main menu") {
		t.Fatalf("stderr = %q, want action-local Back without staging instead of inventory Back", out)
	}
}

func TestHuhInitReviewerEntityPrompterGitHubAppLinearFlowShowsCredentialBundleCopy(t *testing.T) {
	existing := basicProfile("work")
	var stderr bytes.Buffer
	privateKey := testReviewerGitHubAppPrivateKey()
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 60)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
			model.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
			model.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, privateKey)
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))
			model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)
			_, _ = io.WriteString(out, model.View())
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	draft, err := prompter.EditReviewerEntity(initReviewerEntityPrompt{Context: initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}})
	if err != nil {
		t.Fatalf("EditReviewerEntity: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) {
		t.Fatalf("draft = %#v, want GitHub App reviewer", draft)
	}
	if got := draft.ReviewerCredentialWrites[credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("staged app id = %q, want inline app id", got)
	}
	if got := draft.ReviewerCredentialWrites[credentials.GitHubAppPrivateKeyKey]; got != privateKey {
		t.Fatalf("staged private key = %q, want inline private key", got)
	}
	out := stderr.String()
	for _, want := range []string{
		"GitHub App reviewer. Required credential keys",
		"Reviewer secret location",
		"Credential name for this reviewer",
		"This is a credential name, not a PAT",
		"Reviewer credential status",
		credentials.GitHubAppIDKey,
		credentials.GitHubAppPrivateKeyKey,
		credentials.GitHubAppInstallationIDKey,
		"staged",
		credentials.GitHubAppIDKey,
		credentials.GitHubAppPrivateKeyKey,
		"Reviewer credential values",
		"GitHub App ID",
		"GitHub App private key",
		"Optional credential key: " + credentials.GitHubAppInstallationIDKey,
		"optional",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	assertContentOrder(t, out, "Reviewer secret location", "Reviewer credential status", "Reviewer action")
	for _, leaked := range []string{"12345", privateKey} {
		if strings.Contains(out, leaked) {
			t.Fatalf("stderr leaked GitHub App secret value %q:\n%s", leaked, out)
		}
	}
}

func TestReviewerEntityLinearEditorNewLabelDerivesDefaultSecretLocation(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))

	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	labelIndex := model.document.fieldIndexByID(initReviewerEntityFieldLabel)
	if labelIndex < 0 {
		t.Fatal("label field missing")
	}
	model.afterFieldChange(labelIndex)

	if got, want := model.document.fieldValue(initReviewerEntityFieldSecretLocation), "codereview/rianjs-bot-reviewer"; got != want {
		t.Fatalf("secret location = %q, want label-derived %q", got, want)
	}
	status := model.document[model.document.fieldIndexByID(initReviewerEntityFieldCredentialStatus)].Description
	if !strings.Contains(status, "Destination: "+initBuiltInOSCredentialStoreTitle()+" / codereview/rianjs-bot-reviewer") {
		t.Fatalf("status = %q, want label-derived destination", status)
	}
	if !strings.Contains(status, credentials.GitHubAppIDKey) || !strings.Contains(status, string(initReviewerCredentialKeyMissing)) || strings.Contains(status, string(initReviewerCredentialKeyUnavailable)) {
		t.Fatalf("status = %q, want label-derived ref to show missing required keys, not unavailable", status)
	}
}

func TestReviewerEntityLinearEditorManualSecretLocationStopsLabelDerivedUpdates(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindPAT))

	model.setFieldValue(initReviewerEntityFieldSecretLocation, "codereview/custom-reviewer")
	locationIndex := model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation)
	if locationIndex < 0 {
		t.Fatal("secret location field missing")
	}
	model.afterFieldChange(locationIndex)

	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	labelIndex := model.document.fieldIndexByID(initReviewerEntityFieldLabel)
	if labelIndex < 0 {
		t.Fatal("label field missing")
	}
	model.afterFieldChange(labelIndex)

	if got, want := model.document.fieldValue(initReviewerEntityFieldSecretLocation), "codereview/custom-reviewer"; got != want {
		t.Fatalf("secret location = %q, want manual override %q", got, want)
	}
}

func TestReviewerEntityLinearEditorExistingReviewerLabelDoesNotMigrateRef(t *testing.T) {
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/existing-reviewer",
		DisplayName:   "Old label",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": profile},
	}
	entities, profileEntities := buildInitReviewerEntityInventory(cfg)
	profileCopy := profile
	ctx := initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &profileCopy,
		ExistingConfig:          cfg,
		ReviewerEntities:        entities,
		ProfileReviewerEntities: profileEntities,
	}
	seed := seedInteractiveInitDraft("work", "work", &profile)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)

	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	labelIndex := model.document.fieldIndexByID(initReviewerEntityFieldLabel)
	if labelIndex < 0 {
		t.Fatal("label field missing")
	}
	model.afterFieldChange(labelIndex)

	if got, want := model.document.fieldValue(initReviewerEntityFieldSecretLocation), "codereview/existing-reviewer"; got != want {
		t.Fatalf("secret location = %q, want existing ref %q", got, want)
	}
}

func TestReviewerEntityExistingReviewerChangedRefRequiresInlineSecrets(t *testing.T) {
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/existing-reviewer",
		DisplayName:   "Existing reviewer",
	}
	seed := seedInteractiveInitDraft("work", "work", &profile)
	state, err := newReviewerEntityEditorState(initReviewerEntityDraft{
		Kind:          initReviewerEntityKindPAT,
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/existing-reviewer",
		DisplayName:   "Existing reviewer",
	}, seed, true)
	if err != nil {
		t.Fatalf("newReviewerEntityEditorState: %v", err)
	}
	model := newInitLinearEditorModel(state.editor(initPromptContext{}), 120, 40)
	model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	unchanged, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if unchanged.resultAction != initDetailActionEdit {
		t.Fatalf("unchanged existing ref resultAction = %q, want staged without re-entering secrets", unchanged.resultAction)
	}

	model = newInitLinearEditorModel(state.editor(initPromptContext{}), 120, 40)
	model.setFieldValue(initReviewerEntityFieldSecretLocation, "codereview/new-reviewer")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation))
	model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	changed, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if changed.resultAction != "" {
		t.Fatalf("changed ref resultAction = %q, want validation to block staging", changed.resultAction)
	}
	actionIndex := changed.document.fieldIndexByID(initReviewerEntityFieldAction)
	if actionIndex < 0 {
		t.Fatal("action field missing")
	}
	if got := changed.document[actionIndex].Error; !strings.Contains(got, credentials.GitTokenKey) {
		t.Fatalf("changed ref action error = %q, want missing PAT credential", got)
	}
}

func TestReviewerEntityExistingReviewerChangedRefRequiresInlineSecretsWhenStatusUnavailable(t *testing.T) {
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/existing-reviewer",
		DisplayName:   "Existing reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": profile},
	}
	entities, profileEntities := buildInitReviewerEntityInventory(cfg)
	profileCopy := profile
	ctx := initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &profileCopy,
		ExistingConfig:          cfg,
		ReviewerEntities:        entities,
		ProfileReviewerEntities: profileEntities,
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/existing-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			Unavailable: "credential backend status unavailable",
			Keys: []initReviewerCredentialKeyStatus{{
				Key:      credentials.GitTokenKey,
				Required: true,
				State:    initReviewerCredentialKeyUnavailable,
			}},
		}},
	}
	seed := seedInteractiveInitDraft("work", "work", &profile)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model.setFieldValue(initReviewerEntityFieldSecretLocation, "codereview/new-reviewer")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation))
	model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	changed, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if changed.resultAction != "" {
		t.Fatalf("changed ref resultAction = %q, want validation to block staging", changed.resultAction)
	}
	actionIndex := changed.document.fieldIndexByID(initReviewerEntityFieldAction)
	if actionIndex < 0 {
		t.Fatal("action field missing")
	}
	if got := changed.document[actionIndex].Error; !strings.Contains(got, credentials.GitTokenKey) {
		t.Fatalf("changed ref action error = %q, want missing PAT credential", got)
	}
}

func TestReviewerEntityLinearEditorGitHubAppRequiresInlineSecretsBeforeStaging(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldLabel))
	model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	blocked, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if blocked.resultAction != "" {
		t.Fatalf("resultAction = %q, want validation to keep editor open", blocked.resultAction)
	}
	actionIndex := blocked.document.fieldIndexByID(initReviewerEntityFieldAction)
	if actionIndex < 0 {
		t.Fatal("action field missing")
	}
	if got := blocked.document[actionIndex].Error; !strings.Contains(got, credentials.GitHubAppIDKey) || !strings.Contains(got, credentials.GitHubAppPrivateKeyKey) {
		t.Fatalf("action error = %q, want missing GitHub App required keys", got)
	}

	blocked.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
	blocked.afterFieldChange(blocked.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
	privateKey := strings.Join([]string{
		"-----BEGIN PRIVATE KEY-----",
		"abc123",
		"-----END PRIVATE KEY-----",
	}, "\n")
	blocked.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, privateKey)
	blocked.afterFieldChange(blocked.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))
	blocked = focusInitLinearField(t, blocked, initReviewerEntityFieldAction)
	blocked = selectInitLinearFieldValue(t, blocked, initReviewerEntityFieldAction, initDetailActionEdit)

	updated, _ = blocked.Update(tea.KeyMsg{Type: tea.KeyEnter})
	staged, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if staged.resultAction != initDetailActionEdit {
		t.Fatalf("resultAction = %q, want staged reviewer settings", staged.resultAction)
	}
	draft, err := initReviewerEntityDraftFromDocument(ctx, seed, staged.document)
	if err != nil {
		t.Fatalf("initReviewerEntityDraftFromDocument: %v", err)
	}
	if got, want := draft.ReviewerCredentialWriteRef, "codereview/rianjs-bot-reviewer"; got != want {
		t.Fatalf("ReviewerCredentialWriteRef = %q, want %q", got, want)
	}
	if got := draft.ReviewerCredentialWrites[credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("staged github_app_id = %q, want 12345", got)
	}
	if got := draft.ReviewerCredentialWrites[credentials.GitHubAppPrivateKeyKey]; got != privateKey {
		t.Fatalf("staged private key = %q, want multiline key", got)
	}
	if _, ok := draft.ReviewerCredentialWrites[credentials.GitHubAppInstallationIDKey]; ok {
		t.Fatalf("optional installation id staged unexpectedly: %#v", draft.ReviewerCredentialWrites)
	}
}

func TestReviewerEntityLinearEditorLabelDerivedRefDoesNotMarkMissingStatusAsOverwrite(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			Keys: []initReviewerCredentialKeyStatus{
				{Key: credentials.GitHubAppIDKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false, State: initReviewerCredentialKeyOptional},
			},
		}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldLabel))
	model.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
	model.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, testReviewerGitHubAppPrivateKey())
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))

	draft, err := initReviewerEntityDraftFromDocument(ctx, seed, model.document)
	if err != nil {
		t.Fatalf("initReviewerEntityDraftFromDocument: %v", err)
	}
	if got, want := draft.ReviewerCredentialWriteRef, "codereview/rianjs-bot-reviewer"; got != want {
		t.Fatalf("ReviewerCredentialWriteRef = %q, want %q", got, want)
	}
	if draft.ReviewerCredentialOverwrite {
		t.Fatalf("ReviewerCredentialOverwrite = true, want missing label-derived ref to preserve no-overwrite preflight")
	}
}

func TestReviewerEntityLinearEditorManualRefDoesNotMarkUnavailableStatusAsOverwrite(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			Keys: []initReviewerCredentialKeyStatus{
				{Key: credentials.GitHubAppIDKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false, State: initReviewerCredentialKeyOptional},
			},
		}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
	model.setFieldValue(initReviewerEntityFieldSecretLocation, "codereview/manual-reviewer")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation))
	model.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
	model.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, testReviewerGitHubAppPrivateKey())
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))

	draft, err := initReviewerEntityDraftFromDocument(ctx, seed, model.document)
	if err != nil {
		t.Fatalf("initReviewerEntityDraftFromDocument: %v", err)
	}
	if got, want := draft.ReviewerCredentialWriteRef, "codereview/manual-reviewer"; got != want {
		t.Fatalf("ReviewerCredentialWriteRef = %q, want %q", got, want)
	}
	if draft.ReviewerCredentialOverwrite {
		t.Fatalf("ReviewerCredentialOverwrite = true, want unavailable manual ref to preserve no-overwrite preflight")
	}
}

func TestReviewerEntityLinearEditorLabelDerivedRefPreservesBackendUnavailableStatus(t *testing.T) {
	existing := basicProfile("work")
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			Unavailable: "credential backend status unavailable",
			Keys: []initReviewerCredentialKeyStatus{
				{Key: credentials.GitHubAppIDKey, Required: true, State: initReviewerCredentialKeyUnavailable},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true, State: initReviewerCredentialKeyUnavailable},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false, State: initReviewerCredentialKeyUnavailable},
			},
		}},
	}
	seed := seedInteractiveInitDraft("work", "work", &existing)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 120, 40)
	model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
	model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
	model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldLabel))

	status := model.document[model.document.fieldIndexByID(initReviewerEntityFieldCredentialStatus)].Description
	if !strings.Contains(status, "Destination: "+initBuiltInOSCredentialStoreTitle()+" / codereview/rianjs-bot-reviewer") ||
		!strings.Contains(status, "credential backend status unavailable") ||
		!strings.Contains(status, string(initReviewerCredentialKeyUnavailable)) {
		t.Fatalf("status = %q, want label-derived ref to preserve backend-unavailable state", status)
	}
	if strings.Contains(status, string(initReviewerCredentialKeyMissing)) {
		t.Fatalf("status = %q, want unavailable instead of missing when backend status pass failed", status)
	}
}

func TestHuhInitReviewerEntityStatusUpdatesWhenSecretLocationChanges(t *testing.T) {
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/old-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": profile},
	}
	entities, profileEntities := buildInitReviewerEntityInventory(cfg)
	profileCopy := profile
	ctx := initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &profileCopy,
		ExistingConfig:          cfg,
		ReviewerEntities:        entities,
		ProfileReviewerEntities: profileEntities,
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/old-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			Keys: []initReviewerCredentialKeyStatus{{
				Key:      credentials.GitTokenKey,
				Required: true,
				State:    initReviewerCredentialKeyExisting,
			}},
		}},
	}
	seed := seedInteractiveInitDraft("work", "work", &profile)
	model := newInitLinearEditorModel(initReviewerEntityLinearEditor(ctx, seed), 180, 60)
	statusIndex := model.document.fieldIndexByID(initReviewerEntityFieldCredentialStatus)
	if statusIndex < 0 {
		t.Fatal("reviewer credential status field missing")
	}
	initial := model.document[statusIndex].Description
	if !strings.Contains(initial, "codereview/old-reviewer") || !strings.Contains(initial, credentials.GitTokenKey) || !strings.Contains(initial, string(initReviewerCredentialKeyExisting)) {
		t.Fatalf("initial status = %q, want old reviewer existing token", initial)
	}

	locationIndex := model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation)
	if locationIndex < 0 {
		t.Fatal("reviewer secret location field missing")
	}
	model.document[locationIndex].Value = "codereview/new-reviewer"
	model.afterFieldChange(locationIndex)

	updated := model.document[statusIndex].Description
	for _, want := range []string{"Destination: " + initBuiltInOSCredentialStoreTitle() + " / codereview/new-reviewer", "credential backend status unavailable", credentials.GitTokenKey, string(initReviewerCredentialKeyUnavailable)} {
		if !strings.Contains(updated, want) {
			t.Fatalf("updated status = %q, want %q", updated, want)
		}
	}
	if strings.Contains(updated, "codereview/old-reviewer") || strings.Contains(updated, string(initReviewerCredentialKeyExisting)) {
		t.Fatalf("updated status = %q, want old ref existing state cleared", updated)
	}
}

func TestHuhInitReviewerEntityDetailsBackDoesNotMutateDraft(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", nil)
	want := draft
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, initDetailActionBack),
	}

	nextDraft, back, err := prompter.editNewReviewerEntity(initPromptContext{}, initReviewerEntityKindUseGitIdentity, draft)
	if err != nil {
		t.Fatalf("editNewReviewerEntity: %v", err)
	}
	if !back {
		t.Fatal("back = false, want details Back")
	}
	if !reflect.DeepEqual(nextDraft, initDraft{}) {
		t.Fatalf("nextDraft = %#v, want zero draft on Back", nextDraft)
	}
	if !reflect.DeepEqual(draft, want) {
		t.Fatalf("draft mutated on details Back:\n got: %#v\nwant: %#v", draft, want)
	}
}

func TestHuhInitReviewerEntityDetailsGitHubAppShowsCredentialBundleCopy(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", nil)
	var stderr bytes.Buffer
	privateKey := testReviewerGitHubAppPrivateKey()
	prompter := huhInitReviewerEntityPrompter{
		stderr: &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldGitHubAppID:         "12345",
			initReviewerEntityFieldGitHubAppPrivateKey: privateKey,
		}, ""),
	}

	nextDraft, back, err := prompter.editNewReviewerEntity(initPromptContext{}, initReviewerEntityKindGitHubApp, draft)
	if err != nil {
		t.Fatalf("editNewReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want staged GitHub App reviewer details")
	}
	if !nextDraft.ReviewerEnabled || nextDraft.ReviewerAuth != string(config.GitAuthModeGitHubApp) {
		t.Fatalf("draft = %#v, want GitHub App reviewer", nextDraft)
	}
	if got := nextDraft.ReviewerCredentialWrites[credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("staged app id = %q, want inline app id", got)
	}
	if got := nextDraft.ReviewerCredentialWrites[credentials.GitHubAppPrivateKeyKey]; got != privateKey {
		t.Fatalf("staged private key = %q, want inline private key", got)
	}
	out := stderr.String()
	for _, want := range []string{
		"GitHub App reviewer. Required credential keys",
		"Reviewer secret location",
		"Credential name for this reviewer",
		"This is a credential name, not a PAT",
		"Reviewer credential values",
		"GitHub App ID",
		"GitHub App private key",
		credentials.GitHubAppIDKey,
		credentials.GitHubAppPrivateKeyKey,
		"Optional credential key: " + credentials.GitHubAppInstallationIDKey,
		"optional",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	for _, leaked := range []string{"12345", privateKey} {
		if strings.Contains(out, leaked) {
			t.Fatalf("stderr leaked GitHub App secret value %q:\n%s", leaked, out)
		}
	}
}

func TestHuhInitReviewerEntityDetailsPreservesPromptContextCredentialStore(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", nil)
	draft.ReviewerCredentialStore = "work-file"
	resolved := credentials.ResolvedSecretsProfile{
		ID:      "work-file",
		Label:   "Work file",
		Source:  config.EffectiveSecretsProfileSourceConfigured,
		Backend: string(credstore.BackendFile),
	}
	ctx := initPromptContext{
		ExistingConfig: config.File{
			Secrets: config.SecretsConfig{
				Stores: map[string]config.SecretsStore{
					"work-file": {
						DisplayName: "Work file",
						Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
					},
				},
			},
		},
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Store:   "work-file",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			SecretsProfile: resolved,
			Keys: []initReviewerCredentialKeyStatus{
				{Key: credentials.GitHubAppIDKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true, State: initReviewerCredentialKeyMissing},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false, State: initReviewerCredentialKeyOptional},
			},
		}},
	}
	privateKey := testReviewerGitHubAppPrivateKey()
	prompter := huhInitReviewerEntityPrompter{
		stderr: &bytes.Buffer{},
		editorRunner: stageReviewerEntityEditorRunner(t, map[initLinearFieldID]string{
			initReviewerEntityFieldGitHubAppID:         "12345",
			initReviewerEntityFieldGitHubAppPrivateKey: privateKey,
		}, ""),
	}

	nextDraft, back, err := prompter.editNewReviewerEntity(ctx, initReviewerEntityKindGitHubApp, draft)
	if err != nil {
		t.Fatalf("editNewReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want staged GitHub App reviewer details")
	}
	if !nextDraft.ReviewerCredentialWriteStore.Equal(resolved) {
		t.Fatalf("ReviewerCredentialWriteStore = %#v, want %#v", nextDraft.ReviewerCredentialWriteStore, resolved)
	}
}

func TestHuhInitReviewerEntityDetailsAccessibleHidesSecretLocationForGitIdentity(t *testing.T) {
	t.Setenv("TERM", "dumb")
	draft := seedInteractiveInitDraft("work", "work", nil)
	var stderr bytes.Buffer
	prompter := huhInitReviewerEntityPrompter{
		stderr:       &stderr,
		editorRunner: stageReviewerEntityEditorRunner(t, nil, ""),
	}

	nextDraft, back, err := prompter.editNewReviewerEntity(initPromptContext{}, initReviewerEntityKindUseGitIdentity, draft)
	if err != nil {
		t.Fatalf("editNewReviewerEntity: %v", err)
	}
	if back {
		t.Fatal("back = true, want staged git-account reviewer details")
	}
	if !reflect.DeepEqual(nextDraft.ReviewerCredentialRef, "") {
		t.Fatalf("nextDraft.ReviewerCredentialRef = %q, want empty", nextDraft.ReviewerCredentialRef)
	}
	out := stderr.String()
	if strings.Contains(out, "Use a custom reviewer secret location") || strings.Contains(out, "Reviewer secret location") || strings.Contains(out, "Entity label") {
		t.Fatalf("stderr = %q, want git-account reviewer flow to hide reviewer secret-location controls", out)
	}
}

func TestInitInventorySelectionsApplyToDraft(t *testing.T) {
	draft := seedInteractiveInitDraft("default", "", nil)
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
	draft := seedInteractiveInitDraft("work", "work", nil)
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

func TestInitLLMRuntimeSelectionOptionsOmitTemplateActions(t *testing.T) {
	options := initLLMRuntimeSelectionOptions(map[string]initLLMRuntimeDraft{
		"claude-cli": {
			Name:     "claude-cli",
			Preset:   initLLMRuntimePresetClaudeCLISubscription,
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	})
	if len(options) != 1 {
		t.Fatalf("len(options) = %d, want 1 configured-only option", len(options))
	}
	if got, want := options[0].Value, "claude-cli"; got != want {
		t.Fatalf("options[0].Value = %q, want %q", got, want)
	}
	if strings.Contains(options[0].Key, "Template:") || strings.Contains(options[0].Key, "Custom compatible runtime") {
		t.Fatalf("options[0].Key = %q, want configured runtime label only", options[0].Key)
	}
}

func TestInitProfileEditorLLMRuntimeSelectionPrefersMatchingDraftRuntime(t *testing.T) {
	runtimes := map[string]initLLMRuntimeDraft{
		"alpha-runtime": {
			Name:          "alpha-runtime",
			Preset:        initLLMRuntimePresetAnthropicAPIKey,
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/alpha-llm",
		},
		"codex-cli": {
			Name:     "codex-cli",
			Preset:   initLLMRuntimePresetCodexCLISubscription,
			Provider: config.LLMProviderOpenAI,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterCodexCLI,
		},
	}
	draft := seedInteractiveInitDraft("default", "", nil)
	draft.LLMProvider = string(config.LLMProviderOpenAI)
	draft.LLMAuth = string(config.LLMAuthSubscription)
	draft.LLMAdapter = string(config.LLMAdapterCodexCLI)

	options, selected := initProfileEditorLLMRuntimeSelection(runtimes, "", draft)
	if got, want := selected, "codex-cli"; got != want {
		t.Fatalf("selected = %q, want %q from matching staged runtime identity", got, want)
	}
	if len(options) != 2 {
		t.Fatalf("len(options) = %d, want 2 configured runtime options", len(options))
	}
}

func TestInitProfileEditorLLMRuntimeSelectionFallsBackToFirstConfiguredRuntimeWithoutProfileMapping(t *testing.T) {
	runtimes := map[string]initLLMRuntimeDraft{
		"alpha-runtime": {
			Name:          "alpha-runtime",
			Preset:        initLLMRuntimePresetAnthropicAPIKey,
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/alpha-llm",
		},
		"zeta-runtime": {
			Name:          "zeta-runtime",
			Preset:        initLLMRuntimePresetOpenAIAPIKey,
			Provider:      config.LLMProviderOpenAI,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterOpenAIAPI,
			CredentialRef: "codereview/zeta-llm",
		},
	}
	draft := seedInteractiveInitDraft("default", "", nil)

	options, selected := initProfileEditorLLMRuntimeSelection(runtimes, "", draft)
	if got, want := selected, "alpha-runtime"; got != want {
		t.Fatalf("selected = %q, want deterministic first configured runtime fallback %q", got, want)
	}
	if len(options) != 2 {
		t.Fatalf("len(options) = %d, want 2 configured runtime options", len(options))
	}
}

func TestInitSecretsProfileSelectionOptionsShowConfiguredProfilesAndBuiltInDefault(t *testing.T) {
	profiles := []config.EffectiveSecretsProfile{
		{
			ID:      "personal-keychain",
			Label:   "Personal macOS Keychain",
			Backend: string(credstore.BackendKeychain),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
		{
			ID:      "work-1password",
			Label:   "Work 1Password",
			Backend: string(credstore.BackendOPDesktop),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
	}
	options := initSecretsProfileSelectionOptions(profiles, "", "Use built-in default (Legacy default (In-memory store))")
	if len(options) != 3 {
		t.Fatalf("len(options) = %d, want built-in default plus two configured profiles", len(options))
	}
	if got := options[0].Key; !strings.Contains(got, "Use built-in default") {
		t.Fatalf("options[0].Key = %q, want built-in default label first", got)
	}
	if got, want := options[1].Value, "personal-keychain"; got != want {
		t.Fatalf("options[1].Value = %q, want %q", got, want)
	}
	if got := options[2].Key; !strings.Contains(got, "Work 1Password") || !strings.Contains(got, "1Password desktop app") {
		t.Fatalf("options[2].Key = %q, want configured secrets profile label with backend", got)
	}
}

func TestInitProfileEditorSecretsProfileSelectionKeepsBrokenReferenceSelectable(t *testing.T) {
	profiles := []config.EffectiveSecretsProfile{
		{
			ID:      "team-vault",
			Label:   "Team Vault",
			Backend: string(credstore.BackendFile),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
	}
	existing := basicProfile("work")
	options, selected := initProfileEditorSecretsProfileSelection(profiles, "missing-vault", "missing-vault", "Use built-in default (Legacy default)", seedInteractiveInitDraft("work", "work", &existing))
	if got, want := selected, initMissingSecretsProfileSelection("missing-vault"); got != want {
		t.Fatalf("selected = %q, want missing-selection sentinel %q", got, want)
	}
	if len(options) != 3 {
		t.Fatalf("len(options) = %d, want missing + default + configured", len(options))
	}
	if got := options[0].Key; !strings.Contains(got, "Missing configured profile: missing-vault") {
		t.Fatalf("options[0].Key = %q, want missing profile recovery label", got)
	}
}

func TestInitProfileEditorSecretsProfileSelectionVisibleOnlyWhenMeaningful(t *testing.T) {
	if initProfileEditorSecretsProfileSelectionVisible(nil, "") {
		t.Fatal("visible = true, want inert built-in default selector hidden")
	}
	if !initProfileEditorSecretsProfileSelectionVisible([]config.EffectiveSecretsProfile{{
		ID:      "team-vault",
		Backend: string(credstore.BackendFile),
		Source:  config.EffectiveSecretsProfileSourceConfigured,
	}}, "") {
		t.Fatal("visible = false, want configured secrets profile selector shown")
	}
	if !initProfileEditorSecretsProfileSelectionVisible(nil, "missing-vault") {
		t.Fatal("visible = false, want broken secrets profile selector shown for repair")
	}
}

func TestApplySecretsProfileSelection(t *testing.T) {
	draft := initDraft{}
	applySecretsProfileSelection(&draft, initSecretsProfileDefaultSelection)
	if draft.SecretsProfile != "" {
		t.Fatalf("default selection set draft.SecretsProfile = %q, want cleared value", draft.SecretsProfile)
	}
	applySecretsProfileSelection(&draft, "team-vault")
	if draft.SecretsProfile != "team-vault" {
		t.Fatalf("configured selection set draft.SecretsProfile = %q, want team-vault", draft.SecretsProfile)
	}
	applySecretsProfileSelection(&draft, initMissingSecretsProfileSelection("missing-vault"))
	if draft.SecretsProfile != "missing-vault" {
		t.Fatalf("missing selection set draft.SecretsProfile = %q, want missing-vault", draft.SecretsProfile)
	}
}

func TestValidateInteractiveInitConfigDoesNotMaskUnrelatedInvalidState(t *testing.T) {
	cfg := config.Normalize(config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"broken": {
					Backend: config.SecretsStoreBackend{Kind: "bogus"},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	})
	err := validateInteractiveInitConfig(cfg)
	if err == nil {
		t.Fatal("validateInteractiveInitConfig error = nil, want invalid store backend")
	}
	if !strings.Contains(err.Error(), `secrets.stores.broken.backend.kind "bogus"`) {
		t.Fatalf("validateInteractiveInitConfig error = %v, want unrelated invalid state preserved", err)
	}
}

func TestHuhInitSecretPrompterAccessibleNamesSelectedSecretsProfile(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitSecretPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}
	_, err := prompter.ChooseCredentialAction(initCredentialSecretPrompt{
		Entry: initCredentialPlanEntry{
			Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work"},
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:      "team-vault",
				Label:   "Team Vault",
				Backend: string(credstore.BackendFile),
				Source:  config.EffectiveSecretsProfileSourceConfigured,
			},
		},
	})
	if err != nil {
		t.Fatalf("ChooseCredentialAction: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "via Team Vault") {
		t.Fatalf("stderr = %q, want selected credential-store context in prompt title", got)
	}
}

func TestWriteInitCredentialPlanHintsNamesSelectedSecretsProfile(t *testing.T) {
	var out bytes.Buffer
	err := writeInitCredentialPlanHints(&out, "", initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "git",
			Ref:     "codereview/work",
		},
		SecretsProfile: credentials.ResolvedSecretsProfile{
			ID:      "team-vault",
			Label:   "Team Vault",
			Backend: string(credstore.BackendFile),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
		KeySpecs: []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}},
	})
	if err != nil {
		t.Fatalf("writeInitCredentialPlanHints: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Next via Team Vault:") {
		t.Fatalf("hint output = %q, want selected credential-store context", got)
	}
}

func TestInitCredentialReadinessNoteNamesSelectedSecretsProfile(t *testing.T) {
	note := initCredentialReadinessNote(initCredentialPlanEntry{
		Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work"},
		SecretsProfile: credentials.ResolvedSecretsProfile{
			ID:      "team-vault",
			Label:   "Team Vault",
			Backend: string(credstore.BackendFile),
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
		State: initCredentialPlanStateDefer,
	})
	if !strings.Contains(note, "Git via Team Vault deferred") {
		t.Fatalf("note = %q, want named selected credential-store context", note)
	}
	if strings.Contains(note, "required:") {
		t.Fatalf("note = %q, want single-key deferred credentials to keep concise readiness copy", note)
	}
}

func TestInitCredentialReadinessNoteLabelsGitHubAppRequiredAndOptionalKeys(t *testing.T) {
	note := initCredentialReadinessNote(initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
		State: initCredentialPlanStateDefer,
	})
	for _, want := range []string{
		"reviewer deferred",
		"required: " + credentials.GitHubAppIDKey + ", " + credentials.GitHubAppPrivateKeyKey,
		"optional: " + credentials.GitHubAppInstallationIDKey,
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want %q", note, want)
		}
	}
	if strings.Contains(note, "missing "+credentials.GitHubAppInstallationIDKey) || strings.Contains(note, "required: "+credentials.GitHubAppInstallationIDKey) {
		t.Fatalf("note = %q, want installation id labeled optional only", note)
	}
}

func TestInitCredentialReadinessNoteLabelsPartialGitHubAppOptionalKey(t *testing.T) {
	note := initCredentialReadinessNote(initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
		MissingRequiredKeys: []string{credentials.GitHubAppPrivateKeyKey},
		State:               initCredentialPlanStateMissingRequired,
	})
	for _, want := range []string{
		"reviewer missing required " + credentials.GitHubAppPrivateKeyKey,
		"optional: " + credentials.GitHubAppInstallationIDKey,
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want %q", note, want)
		}
	}
	if strings.Contains(note, "missing "+credentials.GitHubAppInstallationIDKey) || strings.Contains(note, "required "+credentials.GitHubAppInstallationIDKey) {
		t.Fatalf("note = %q, want optional installation id not treated as required", note)
	}
}

func TestInitProfileReadinessLineIncludesNotes(t *testing.T) {
	line := initProfileReadinessLine(initProfileReadiness{
		ProfileName: "work",
		Ready:       false,
		Notes:       []string{"reviewer deferred (required: github_app_id, github_app_private_key; optional: github_app_installation_id)"},
	})
	for _, want := range []string{
		"- work: needs follow-up",
		"required: github_app_id, github_app_private_key",
		"optional: github_app_installation_id",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line = %q, want %q", line, want)
		}
	}
}

func TestBuildInteractiveInitWorkspaceRepairsBrokenSecretsProfileSelection(t *testing.T) {
	cfg := config.Normalize(config.File{
		Profiles: map[string]config.Profile{
			"work": func() config.Profile {
				profile := basicProfile("work")
				profile.SecretsProfile = "missing-vault"
				return profile
			}(),
		},
	})
	profile := cfg.Profiles["work"]
	draft := seedInteractiveInitDraft("work", "work", &profile)
	applySecretsProfileSelection(&draft, initSecretsProfileDefaultSelection)

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if got := workspace.profile.SecretsProfile; got != "" {
		t.Fatalf("workspace.profile.SecretsProfile = %q, want cleared explicit selection", got)
	}
}

func TestBuildInteractiveInitWorkspaceRepairsBrokenSecretsProfileToConfiguredProfile(t *testing.T) {
	cfg := config.Normalize(config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"team-vault": {
					Label:   "Team Vault",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"work": func() config.Profile {
				profile := basicProfile("work")
				profile.SecretsProfile = "missing-vault"
				return profile
			}(),
		},
	})
	profile := cfg.Profiles["work"]
	draft := seedInteractiveInitDraft("work", "work", &profile)
	applySecretsProfileSelection(&draft, "team-vault")

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if got := workspace.profile.SecretsProfile; got != "team-vault" {
		t.Fatalf("workspace.profile.SecretsProfile = %q, want team-vault", got)
	}
	if got := workspace.cfg.Profiles["work"].SecretsProfile; got != "team-vault" {
		t.Fatalf("workspace cfg secrets_profile = %q, want team-vault", got)
	}
}

func TestRunInitWithDepsDeferredHintsUseSelectedSecretsProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName:        "work",
				GitHost:            "github.com",
				GitAuth:            string(config.GitAuthModePAT),
				GitCredentialStore: "team-vault",
				GitCredentialRef:   "codereview/work",
				LLMProvider:        string(config.LLMProviderAnthropic),
				LLMAuth:            string(config.LLMAuthSubscription),
				LLMAdapter:         string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: func(string) (config.File, bool, error) {
			return config.File{
				Profiles: map[string]config.Profile{},
				Secrets: config.SecretsConfig{
					Stores: map[string]config.SecretsStore{
						"team-vault": {
							DisplayName: "Team Vault",
							Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
						},
					},
				},
			}, false, nil
		},
		saveConfig: func(string, config.File) error { return nil },
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "Next via Team Vault: cr set-credential --store team-vault --name codereview/work --key "+credentials.GitTokenKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred hint naming the selected credential store", got)
	}
}

func TestBuildInteractiveInitWorkspaceAllowsRepairWhileAnotherProfileStillHasBrokenSecretsProfile(t *testing.T) {
	cfg := config.Normalize(config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"team-vault": {
					Label:   "Team Vault",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": basicProfile("home"),
			"work": func() config.Profile {
				profile := basicProfile("work")
				profile.SecretsProfile = "missing-vault"
				return profile
			}(),
		},
	})
	home := cfg.Profiles["home"]
	draft := seedInteractiveInitDraft("home", "home", &home)
	applySecretsProfileSelection(&draft, "team-vault")

	workspace, err := buildInteractiveInitWorkspace(&cobra.Command{}, &root.Options{}, initOptions{}, initDeps{}, filepath.Join(t.TempDir(), "config.yml"), cfg, draft)
	if err != nil {
		t.Fatalf("buildInteractiveInitWorkspace: %v", err)
	}
	if got := workspace.profile.SecretsProfile; got != "team-vault" {
		t.Fatalf("workspace.profile.SecretsProfile = %q, want team-vault", got)
	}
}

func TestHuhInitPrompterAccessibleAdvancedStorageLabelsExposeRefInputs(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.Git.CredentialRef = "codereview/work"
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-reviewer"},
	}
	existing.LLM.Provider = config.LLMProviderAnthropic
	existing.LLM.Auth = config.LLMAuthAPIKey
	existing.LLM.Adapter = config.LLMAdapterAnthropicAPI
	existing.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/shared-llm"}
	runtimes := map[string]initLLMRuntimeDraft{
		"anthropic-runtime": {
			Name:          "anthropic-runtime",
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/shared-llm",
		},
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"pat-reviewer": {
			Name:          "pat-reviewer",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/shared-reviewer",
		},
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope: custom
			"", // Git scope host
			"", // Git scope auth mode
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Repository routes
			"", // LLM credential store
			"", // LLM credential name
			"", // Git credential store
			"", // Git credential name
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
		ReviewerEntities:     reviewerEntities,
		ProfileReviewerEntities: map[string]string{
			"work": "pat-reviewer",
		},
		LLMRuntimes: runtimes,
		ProfileLLMRuntimes: map[string]string{
			"work": "anthropic-runtime",
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if strings.Contains(out, "Storage label handling") || strings.Contains(out, "Customize storage labels (advanced)") {
		t.Fatalf("wizard output still shows legacy storage label mode prompt: %q", out)
	}
	if !strings.Contains(out, "Git credentials") || !strings.Contains(out, "Git credential store") || !strings.Contains(out, "Git credential name") {
		t.Fatalf("wizard output missing inline Git credential prompts: %q", out)
	}
	if !strings.Contains(out, "LLM API key credentials") || !strings.Contains(out, "LLM credential store") || !strings.Contains(out, "LLM credential name") {
		t.Fatalf("wizard output missing inline LLM credential prompts for API-key runtime: %q", out)
	}
	if strings.Contains(out, "Reviewer storage label") || strings.Contains(out, "LLM storage label") || strings.Contains(out, "Git secrets storage label") {
		t.Fatalf("wizard output exposed legacy storage-label copy: %q", out)
	}
	if !strings.Contains(out, "credential name is the full codereview/... path written to the selected store") {
		t.Fatalf("wizard output missing explicit credential-name guidance: %q", out)
	}
	if draft.AdvancedStorageLabels {
		t.Fatalf("draft.AdvancedStorageLabels = true, want false when the flattened fields keep their selected defaults")
	}
}

func TestHuhInitPrompterAccessibleStorageLabelsDefaultSkipPath(t *testing.T) {
	t.Setenv("TERM", "dumb")
	work := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": work,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // Profile name
			"", // Make default
			"", // Git scope
			"", // Reviewer entity
			"", // LLM runtime
			"", // Reviewer model tier
			"", // Repository routes
			"", // Git credential store
			"", // Git credential name
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &work,
		ExistingConfig:       cfg,
		GitScopes:            gitScopes,
		ProfileGitScopes:     profileGitScopes,
		LLMRuntimes:          llmRuntimes,
		ProfileLLMRuntimes:   profileLLMRuntimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if strings.Contains(out, "Storage label handling") {
		t.Fatalf("wizard output still shows legacy storage label mode prompt: %q", out)
	}
	if !strings.Contains(out, "Git credentials") || !strings.Contains(out, "Git credential store") || !strings.Contains(out, "Git credential name") {
		t.Fatalf("wizard output missing inline Git credential prompts: %q", out)
	}
	if !strings.Contains(out, "Profile action") || !strings.Contains(out, "Stage profile settings") {
		t.Fatalf("wizard output missing profile-level staging action: %q", out)
	}
	if strings.Contains(out, "Review policy action") || strings.Contains(out, "Stage review-policy settings") {
		t.Fatalf("wizard output still shows review-policy-only staging action: %q", out)
	}
	if draft.AdvancedStorageLabels {
		t.Fatalf("draft.AdvancedStorageLabels = true, want false on the default storage-label path")
	}
}

func TestHuhInitPrompterAccessibleStorageLabelsOnlyExposeGitLabel(t *testing.T) {
	t.Setenv("TERM", "dumb")
	work := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": work,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	runtimes := map[string]initLLMRuntimeDraft{
		"a-claude-runtime": {
			Name:     "a-claude-runtime",
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		"z-api-runtime": {
			Name:            "z-api-runtime",
			Provider:        config.LLMProviderAnthropic,
			Auth:            config.LLMAuthAPIKey,
			Adapter:         config.LLMAdapterAnthropicAPI,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/shared-llm",
		},
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"pat-reviewer": {
			Name:            "pat-reviewer",
			Kind:            initReviewerEntityKindPAT,
			AuthMode:        config.GitAuthModePAT,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/shared-reviewer",
		},
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",  // Profile name
			"",  // Make default
			"",  // Git scope
			"1", // Reviewer entity: pat-reviewer
			"2", // LLM runtime: z-api-runtime
			"",  // Reviewer model tier
			"",  // Repository routes
			"",  // LLM credential store
			"",  // LLM credential name
			"",  // Git credential store
			"",  // Git credential name
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &work,
		ExistingConfig:       cfg,
		GitScopes:            gitScopes,
		ProfileGitScopes:     profileGitScopes,
		ReviewerEntities:     reviewerEntities,
		LLMRuntimes:          runtimes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Git credentials") || !strings.Contains(out, "Git credential store") || !strings.Contains(out, "Git credential name") {
		t.Fatalf("stderr = %q, want inline Git credential prompts", out)
	}
	if !strings.Contains(out, "LLM API key credentials") || !strings.Contains(out, "LLM credential store") || !strings.Contains(out, "LLM credential name") {
		t.Fatalf("stderr = %q, want inline LLM credential prompts for API-key runtime", out)
	}
	if strings.Contains(out, "Reviewer storage label") || strings.Contains(out, "LLM storage label") || strings.Contains(out, "Git secrets storage label") {
		t.Fatalf("stderr = %q, want profile editor to omit legacy storage-label prompts", out)
	}
	_ = draft
}

func TestInitProfileStorageLabelSelectionTransitionFollowsChangedDefaults(t *testing.T) {
	draft := initDraft{
		ProfileName:           "work",
		GitAuth:               string(config.GitAuthModePAT),
		GitCredentialRef:      "codereview/old-git",
		ReviewerEnabled:       true,
		ReviewerAuth:          string(config.GitAuthModePAT),
		ReviewerCredentialRef: "codereview/old-reviewer",
		LLMProvider:           string(config.LLMProviderAnthropic),
		LLMAuth:               string(config.LLMAuthAPIKey),
		LLMAdapter:            string(config.LLMAdapterAnthropicAPI),
		LLMCredentialRef:      "codereview/old-llm",
	}
	gitScopes := map[string]initGitScopeDraft{
		"a-old-git": {
			Name:          "a-old-git",
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/old-git",
		},
		"z-new-git": {
			Name:          "z-new-git",
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/new-git",
		},
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"a-old-reviewer": {
			Name:          "a-old-reviewer",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/old-reviewer",
		},
		"z-new-reviewer": {
			Name:          "z-new-reviewer",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/new-reviewer",
		},
	}
	runtimes := map[string]initLLMRuntimeDraft{
		"a-old-runtime": {
			Name:          "a-old-runtime",
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/old-llm",
		},
		"z-new-runtime": {
			Name:          "z-new-runtime",
			Provider:      config.LLMProviderOpenAI,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterOpenAIAPI,
			CredentialRef: "codereview/new-llm",
		},
	}

	standardGitRef, err := initStandardGitCredentialRef(draft.ProfileName, "a-old-git", gitScopes)
	if err != nil {
		t.Fatalf("initStandardGitCredentialRef: %v", err)
	}
	standardReviewerRef, err := initStandardReviewerCredentialRef(draft.ProfileName, "a-old-reviewer", reviewerEntities)
	if err != nil {
		t.Fatalf("initStandardReviewerCredentialRef: %v", err)
	}
	standardLLMRef, err := initStandardLLMCredentialRef(draft.ProfileName, "a-old-runtime", runtimes)
	if err != nil {
		t.Fatalf("initStandardLLMCredentialRef: %v", err)
	}
	gitValue := initEffectiveStorageLabelValue(draft.GitCredentialRef, standardGitRef)
	reviewerValue := initEffectiveStorageLabelValue(draft.ReviewerCredentialRef, standardReviewerRef)
	llmValue := initEffectiveStorageLabelValue(draft.LLMCredentialRef, standardLLMRef)
	gitUsesDefaultBeforeSelection := initStorageLabelUsesDefault(gitValue, standardGitRef)
	reviewerUsesDefaultBeforeSelection := initStorageLabelUsesDefault(reviewerValue, standardReviewerRef)
	llmUsesDefaultBeforeSelection := initStorageLabelUsesDefault(llmValue, standardLLMRef)

	draft.AdvancedStorageLabels = true
	applyGitScopeSelection(&draft, "z-new-git", gitScopes)
	applyReviewerEntityInventorySelection(&draft, "z-new-reviewer", reviewerEntities)
	applyLLMRuntimeInventorySelection(&draft, "z-new-runtime", runtimes)
	reviewerMode := string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
	selectedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyReviewerEntitySelection(&draft, reviewerMode)
	applyLLMRuntimeSelection(&draft, selectedRuntimePreset)
	err = normalizeInitProfileStorageLabels(&draft, "z-new-git", "z-new-reviewer", "z-new-runtime", gitScopes, reviewerEntities, runtimes, initStorageLabelNormalizationInput{
		Git: initStorageLabelFieldState{
			Value:       gitValue,
			UsesDefault: gitUsesDefaultBeforeSelection,
		},
		Reviewer: initStorageLabelFieldState{
			Value:       reviewerValue,
			UsesDefault: reviewerUsesDefaultBeforeSelection,
		},
		LLM: initStorageLabelFieldState{
			Value:       llmValue,
			UsesDefault: llmUsesDefaultBeforeSelection,
		},
	})
	if err != nil {
		t.Fatalf("normalizeInitProfileStorageLabels: %v", err)
	}
	if draft.GitCredentialRef != "codereview/new-git" {
		t.Fatalf("git ref = %q, want changed selection default", draft.GitCredentialRef)
	}
	if draft.ReviewerCredentialRef != "codereview/new-reviewer" {
		t.Fatalf("reviewer ref = %q, want changed selection default", draft.ReviewerCredentialRef)
	}
	if draft.LLMCredentialRef != "codereview/new-llm" {
		t.Fatalf("llm ref = %q, want changed selection default", draft.LLMCredentialRef)
	}
	if draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = true, want false when inline values keep following changed defaults")
	}
}

func TestHuhInitPrompterAccessibleStorageLabelsKeepCustomOverridesAcrossSelections(t *testing.T) {
	t.Setenv("TERM", "dumb")
	existing := basicProfile("work")
	existing.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-git"}
	existing.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-reviewer"},
	}
	existing.LLM.Provider = config.LLMProviderAnthropic
	existing.LLM.Auth = config.LLMAuthAPIKey
	existing.LLM.Adapter = config.LLMAdapterAnthropicAPI
	existing.LLM.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-llm"}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": existing,
		},
	}
	gitScopes := map[string]initGitScopeDraft{
		"a-old-git": {
			Name:          "a-old-git",
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/old-git",
		},
		"z-new-git": {
			Name:          "z-new-git",
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/new-git",
		},
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"a-old-reviewer": {
			Name:          "a-old-reviewer",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/old-reviewer",
		},
		"z-new-reviewer": {
			Name:          "z-new-reviewer",
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/new-reviewer",
		},
	}
	runtimes := map[string]initLLMRuntimeDraft{
		"a-old-runtime": {
			Name:          "a-old-runtime",
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/old-llm",
		},
		"z-new-runtime": {
			Name:          "z-new-runtime",
			Provider:      config.LLMProviderOpenAI,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterOpenAIAPI,
			CredentialRef: "codereview/new-llm",
		},
	}
	var stderr bytes.Buffer
	prompter := huhInitPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",  // Profile name
			"1", // Make default: keep current default
			"2", // Git scope: z-new-git
			"",  // Git scope host
			"1", // Git scope auth mode: personal access token
			"2", // Reviewer entity: z-new-reviewer
			"2", // LLM runtime: z-new-runtime
			"1", // Reviewer model tier: built-in baseline
			"1", // Repository routes: keep current
			"",  // Git storage label
			"",  // Reviewer storage label
			"",  // LLM storage label
			"",
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName:    "work",
		ExistingProfileName:     "work",
		ExistingProfile:         &existing,
		ExistingProfileNames:    []string{"work"},
		ExistingConfig:          cfg,
		GitScopes:               gitScopes,
		ProfileGitScopes:        map[string]string{"work": "a-old-git"},
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: map[string]string{"work": "a-old-reviewer"},
		LLMRuntimes:             runtimes,
		ProfileLLMRuntimes:      map[string]string{"work": "a-old-runtime"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.GitCredentialRef != "codereview/custom-git" {
		t.Fatalf("git ref = %q, want preserved custom override", draft.GitCredentialRef)
	}
	if draft.ReviewerCredentialRef != "codereview/custom-reviewer" {
		t.Fatalf("reviewer ref = %q, want preserved custom override", draft.ReviewerCredentialRef)
	}
	if draft.LLMCredentialRef != "codereview/custom-llm" {
		t.Fatalf("llm ref = %q, want preserved custom override", draft.LLMCredentialRef)
	}
	if !draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = false, want true when custom inline overrides remain in place")
	}
}

func TestNormalizeInitProfileStorageLabelsUsesSelectedDefaultsForBlankInputs(t *testing.T) {
	draft := initDraft{
		ProfileName:           "work",
		GitAuth:               string(config.GitAuthModePAT),
		ReviewerEnabled:       true,
		ReviewerAuth:          string(config.GitAuthModePAT),
		LLMAuth:               string(config.LLMAuthAPIKey),
		LLMProvider:           string(config.LLMProviderAnthropic),
		LLMAdapter:            string(config.LLMAdapterAnthropicAPI),
		GitCredentialRef:      "codereview/old-git",
		ReviewerCredentialRef: "codereview/old-reviewer",
		LLMCredentialRef:      "codereview/old-llm",
	}
	reviewerEntities := map[string]initReviewerEntityDraft{
		"pat-reviewer": {
			Kind:          initReviewerEntityKindPAT,
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/shared-reviewer",
		},
	}
	runtimes := map[string]initLLMRuntimeDraft{
		"anthropic-runtime": {
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/shared-llm",
		},
	}

	err := normalizeInitProfileStorageLabels(&draft, initCustomGitScopeSelection, "pat-reviewer", "anthropic-runtime", nil, reviewerEntities, runtimes, initStorageLabelNormalizationInput{
		Git:      initStorageLabelFieldState{UsesDefault: true},
		Reviewer: initStorageLabelFieldState{UsesDefault: true},
		LLM:      initStorageLabelFieldState{UsesDefault: true},
	})
	if err != nil {
		t.Fatalf("normalizeInitProfileStorageLabels: %v", err)
	}
	if draft.GitCredentialRef != "codereview/work" {
		t.Fatalf("git ref = %q, want standard profile git ref", draft.GitCredentialRef)
	}
	if draft.ReviewerCredentialRef != "codereview/shared-reviewer" {
		t.Fatalf("reviewer ref = %q, want selected reviewer default ref", draft.ReviewerCredentialRef)
	}
	if draft.LLMCredentialRef != "codereview/shared-llm" {
		t.Fatalf("llm ref = %q, want selected runtime default ref", draft.LLMCredentialRef)
	}
	if draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = true, want false when blank inputs follow selected defaults")
	}
}

func TestNormalizeInitProfileStorageLabelsFollowsChangedGitScopeDefault(t *testing.T) {
	draft := initDraft{
		ProfileName:      "work",
		GitAuth:          string(config.GitAuthModePAT),
		GitCredentialRef: "codereview/old-git",
	}
	scopes := map[string]initGitScopeDraft{
		"old-git": {
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/old-git",
		},
		"new-git": {
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/new-git",
		},
	}

	err := normalizeInitProfileStorageLabels(&draft, "new-git", string(initReviewerEntityKindUseGitIdentity), "", scopes, nil, nil, initStorageLabelNormalizationInput{
		Git:      initStorageLabelFieldState{Value: "codereview/old-git", UsesDefault: true},
		Reviewer: initStorageLabelFieldState{UsesDefault: true},
		LLM:      initStorageLabelFieldState{UsesDefault: true},
	})
	if err != nil {
		t.Fatalf("normalizeInitProfileStorageLabels: %v", err)
	}
	if draft.GitCredentialRef != "codereview/new-git" {
		t.Fatalf("git ref = %q, want changed git-scope default", draft.GitCredentialRef)
	}
	if draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = true, want false when the git label keeps following the changed scope default")
	}
}

func TestNormalizeInitProfileStorageLabelsClearsHiddenReviewerAndLLMOverrides(t *testing.T) {
	draft := initDraft{
		ProfileName:           "work",
		GitAuth:               string(config.GitAuthModePAT),
		ReviewerEnabled:       false,
		ReviewerAuth:          string(config.GitAuthModePAT),
		LLMAuth:               string(config.LLMAuthSubscription),
		LLMProvider:           string(config.LLMProviderAnthropic),
		LLMAdapter:            string(config.LLMAdapterClaudeCLI),
		GitCredentialRef:      "codereview/work",
		ReviewerCredentialRef: "codereview/custom-reviewer",
		LLMCredentialRef:      "codereview/custom-llm",
	}
	runtimes := map[string]initLLMRuntimeDraft{
		"claude-runtime": {
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	}

	err := normalizeInitProfileStorageLabels(&draft, initCustomGitScopeSelection, string(initReviewerEntityKindUseGitIdentity), "claude-runtime", nil, nil, runtimes, initStorageLabelNormalizationInput{
		Git:      initStorageLabelFieldState{Value: "codereview/work", UsesDefault: true},
		Reviewer: initStorageLabelFieldState{Value: "codereview/custom-reviewer", UsesDefault: false},
		LLM:      initStorageLabelFieldState{Value: "codereview/custom-llm", UsesDefault: false},
	})
	if err != nil {
		t.Fatalf("normalizeInitProfileStorageLabels: %v", err)
	}
	if draft.ReviewerCredentialRef != "" {
		t.Fatalf("reviewer ref = %q, want cleared when reviewer label input is hidden", draft.ReviewerCredentialRef)
	}
	if draft.LLMCredentialRef != "" {
		t.Fatalf("llm ref = %q, want cleared when llm label input is hidden", draft.LLMCredentialRef)
	}
	if draft.AdvancedStorageLabels {
		t.Fatal("draft.AdvancedStorageLabels = true, want false after hidden reviewer/llm overrides are cleared")
	}
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
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "work\n")
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "work",
					Title: "work",
				},
			}, nil
		},
	}

	_, err := prompter.Run(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": existing}},
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

func TestHuhInitPrompterAccessibleHidesReviewerEntityLabelForProfileGitAccount(t *testing.T) {
	t.Setenv("TERM", "dumb")
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
			"", // Git storage label
			"", // Repository routes
			"",
		}, "\n")),
		stderr: &stderr,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(initLLMRuntimePrompt) (initDraft, error) {
			return initDraft{
				LLMProvider: string(config.LLMProviderOpenAI),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		inventoryRunner: func(prompt initInventoryPrompt, _ io.Reader, out io.Writer) (initInventoryResult, error) {
			_, _ = io.WriteString(out, prompt.Description+"\n")
			_, _ = io.WriteString(out, "Create new profile\n")
			return initInventoryResult{
				Action: initInventoryActionCommand,
				Row: initInventoryRow{
					ID:            initCreateProfileSentinel,
					Title:         "Create new profile",
					PrimaryAction: initInventoryActionCommand,
				},
			}, nil
		},
	}

	_, err := prompter.Run(initPromptContext{
		RequestedProfileName: "default",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(stderr.String(), "Reviewer entity label") {
		t.Fatalf("stderr = %q, want reviewer entity label hidden when using the profile Git account", stderr.String())
	}
}

func TestHuhInitModelMapPrompterAccessibleShowsTierInputs(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitModelMapPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",           // small stays blank
			"gpt-custom", // medium override
			"",           // large stays blank
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderPi,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterPiRPC,
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
	if strings.Contains(out, "Model-map action") || strings.Contains(out, "Stage model-map settings") || strings.Contains(out, "Back without staging") {
		t.Fatalf("stderr = %q, want flattened model-map editor without action preflight", out)
	}
}

func TestHuhInitModelMapPrompterAccessibleLeavesBuiltInsOutOfOverrides(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitModelMapPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // keep built-in small model
			"", // keep built-in medium model
			"", // keep built-in large model
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderOpenAI,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterCodexCLI,
		},
		ModelMap: nil,
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if edit.Apply != true {
		t.Fatal("edit.Apply = false, want true")
	}
	if edit.ModelMap != nil {
		t.Fatalf("edit.ModelMap = %#v, want built-in effective values omitted from overrides", edit.ModelMap)
	}
}

func TestHuhInitModelMapPrompterAccessibleEscapeBackNavigatesOut(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitModelMapPrompter{
		stdin:  strings.NewReader("\x1b"),
		stderr: &bytes.Buffer{},
	}

	_, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ModelMap: config.ModelMap{"medium": "claude-custom"},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditModelMap error = %v, want errInitNavigateBack", err)
	}
}

func TestHuhInitModelMapPrompterXtermKeepsPrefilledOverrideWithBuiltIns(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitModelMapPrompter{
		stdin:  strings.NewReader("\r\r\r\r"),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderOpenAI,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterCodexCLI,
		},
		ModelMap: config.ModelMap{"medium": "gpt-custom"},
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if !reflect.DeepEqual(edit.ModelMap, config.ModelMap{"medium": "gpt-custom"}) {
		t.Fatalf("edit.ModelMap = %#v, want preserved configured override with built-ins present", edit.ModelMap)
	}
}

func TestHuhInitModelMapPrompterXtermClearsPrefilledOverrideBackToBuiltIn(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitModelMapPrompter{
		stdin:  strings.NewReader("\t" + strings.Repeat("\x7f", 20) + "\r\t\r\r"),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditModelMap(initModelMapPrompt{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ModelMap: config.ModelMap{"medium": "claude-custom"},
	})
	if err != nil {
		t.Fatalf("EditModelMap: %v", err)
	}
	if edit.ModelMap != nil {
		t.Fatalf("edit.ModelMap = %#v, want cleared override to fall back to built-in mappings only", edit.ModelMap)
	}
}

func TestInitEffectiveModelMapInputValuePrefersConfiguredOverride(t *testing.T) {
	llm := config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
		ModelMap: config.ModelMap{"medium": "claude-custom"},
	}
	got := initEffectiveModelMapInputValue(config.EffectiveModelMap(llm), config.ModelTierMedium)
	if got != "claude-custom" {
		t.Fatalf("initEffectiveModelMapInputValue = %q, want configured override", got)
	}
}

func TestApplyModelMapToLLMUsesPromptModelMapOverrides(t *testing.T) {
	llm := config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
	}
	effective := config.EffectiveModelMap(applyModelMapToLLM(llm, config.ModelMap{"medium": "claude-custom"}))
	got := initEffectiveModelMapInputValue(effective, config.ModelTierMedium)
	if got != "claude-custom" {
		t.Fatalf("effective input value = %q, want prompt override applied even when llm.ModelMap is nil", got)
	}
}

func TestInitModelMapInputDescriptionReflectsMappingSource(t *testing.T) {
	if got := initModelMapInputDescription(config.ModelTierMedium, "", "gpt-5.4"); !strings.Contains(got, "Built-in medium model for this runtime: gpt-5.4.") {
		t.Fatalf("built-in description = %q", got)
	}
	if got := initModelMapInputDescription(config.ModelTierMedium, "claude-custom", "claude-sonnet-4-6"); !strings.Contains(got, "Configured override for the medium tier.") {
		t.Fatalf("override description = %q", got)
	}
	if got := initModelMapInputDescription(config.ModelTierSmall, "", ""); !strings.Contains(got, "No built-in small model for this runtime.") {
		t.Fatalf("unmapped description = %q", got)
	}
}

func TestHuhInitAgentSourcesPrompterAccessibleShowsEditablePaths(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitAgentSourcesPrompter{
		stdin: strings.NewReader(strings.Join([]string{
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
	if !strings.Contains(out, "Add local directories that contain custom reviewer agent definitions for this profile.") {
		t.Fatalf("stderr = %q, want local-directory explanation", out)
	}
	if !strings.Contains(out, "<repo>/.codereview/agents") || !strings.Contains(out, "any per-run --agents-dir sources") {
		t.Fatalf("stderr = %q, want agent-source resolution context", out)
	}
	if !strings.Contains(out, "Additional trusted reviewer-agent directories") {
		t.Fatalf("stderr = %q, want trusted reviewer-agent label", out)
	}
	if !strings.Contains(out, "Paths are deduplicated and normalized before save.") {
		t.Fatalf("stderr = %q, want normalization guidance", out)
	}
	if strings.Contains(out, "Most users should leave this empty") || strings.Contains(out, "Trust requirement") || strings.Contains(out, "Only configure directories you trust") {
		t.Fatalf("stderr = %q, want simplified agent-source copy", out)
	}
}

func TestHuhInitAgentSourcesPrompterXtermAcceptsMultilinePaths(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitAgentSourcesPrompter{
		stdin:  strings.NewReader("/tmp/agents-alpha\x0a/tmp/agents-beta\r"),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditAgentSources(initAgentSourcesPrompt{})
	if err != nil {
		t.Fatalf("EditAgentSources: %v", err)
	}
	if !reflect.DeepEqual(edit.Sources, []string{"/tmp/agents-alpha", "/tmp/agents-beta"}) {
		t.Fatalf("edit.Sources = %#v, want newline-separated trusted directories", edit.Sources)
	}
}

func TestHuhInitAgentSourcesPrompterXtermEditsPrefilledMultilinePaths(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitAgentSourcesPrompter{
		stdin:  strings.NewReader("\x0a/tmp/agents-beta\r"),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditAgentSources(initAgentSourcesPrompt{
		Sources: []string{"/tmp/agents-alpha"},
	})
	if err != nil {
		t.Fatalf("EditAgentSources: %v", err)
	}
	if !reflect.DeepEqual(edit.Sources, []string{"/tmp/agents-alpha", "/tmp/agents-beta"}) {
		t.Fatalf("edit.Sources = %#v, want preserved prefilled path plus appended multiline entry", edit.Sources)
	}
}

func TestHuhInitAgentSourcesPrompterXtermClearsPrefilledPaths(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitAgentSourcesPrompter{
		stdin:  strings.NewReader("\x15\r"),
		stderr: &bytes.Buffer{},
	}

	edit, err := prompter.EditAgentSources(initAgentSourcesPrompt{
		Sources: []string{"/tmp/agents-alpha"},
	})
	if err != nil {
		t.Fatalf("EditAgentSources: %v", err)
	}
	if edit.Sources != nil {
		t.Fatalf("edit.Sources = %#v, want nil after clearing prefilled trusted directories", edit.Sources)
	}
}

func TestHuhInitAgentSourcesPrompterBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitAgentSourcesPrompter{
		stdin:  strings.NewReader("\x1b"),
		stderr: &bytes.Buffer{},
	}

	_, err := prompter.EditAgentSources(initAgentSourcesPrompt{
		Sources: []string{"/tmp/agents"},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditAgentSources error = %v, want errInitNavigateBack", err)
	}
}

func TestHuhInitReviewPolicyPrompterAccessibleShowsFields(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitReviewPolicyPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"",    // Stage review-policy settings
			"",    // keep comment
			"",    // keep recommended self-approve false
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
	if edit.ReviewPolicy.AllowSelfApprove {
		t.Fatalf("review policy = %#v, want self-approve false on default path", edit.ReviewPolicy)
	}
	out := stderr.String()
	if !strings.Contains(out, "Major findings event") || !strings.Contains(out, "Resolve-after duration") {
		t.Fatalf("stderr = %q, want review-policy fields", out)
	}
	if !strings.Contains(out, "Do not allow self-approve (recommended)") || !strings.Contains(out, "Enable self-approve") {
		t.Fatalf("stderr = %q, want explicit self-approve choices", out)
	}
	if !strings.Contains(out, "Back without staging") {
		t.Fatalf("stderr = %q, want review-policy Back option", out)
	}
}

func TestInitReviewPolicySelfApproveChoiceRoundTrip(t *testing.T) {
	if got := initReviewPolicySelfApproveChoice(false); got != initSelfApproveDisallow {
		t.Fatalf("choice(false) = %q, want %q", got, initSelfApproveDisallow)
	}
	if got := initReviewPolicySelfApproveChoice(true); got != initSelfApproveEnable {
		t.Fatalf("choice(true) = %q, want %q", got, initSelfApproveEnable)
	}
	if initReviewPolicyAllowSelfApprove(initSelfApproveDisallow) {
		t.Fatal("allow(disallow) = true, want false")
	}
	if !initReviewPolicyAllowSelfApprove(initSelfApproveEnable) {
		t.Fatal("allow(enable) = false, want true")
	}
	options := initReviewPolicySelfApproveOptions()
	if len(options) != 2 {
		t.Fatalf("len(options) = %d, want 2", len(options))
	}
	if options[0].Key != "Do not allow self-approve (recommended)" {
		t.Fatalf("first label = %q", options[0].Key)
	}
	if options[1].Key != "Enable self-approve" {
		t.Fatalf("second label = %q", options[1].Key)
	}
}

func TestHuhInitRoutesPrompterAccessibleShowsRouteEditor(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitRoutesPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // keep existing route text
			"",
		}, "\n")),
		stderr: &stderr,
	}

	edit, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName: "work",
		ProfileHost: "github.com",
		Routes: []configedit.RepositoryRouteSpec{
			{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
			{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
	})
	if err != nil {
		t.Fatalf("EditRoutes: %v", err)
	}
	if len(edit.Routes) != 2 || edit.Routes[0].Namespace != "open-cli-collective" || edit.Routes[1].Namespace != "rianjs" {
		t.Fatalf("routes = %#v, want preserved route", edit.Routes)
	}
	out := stderr.String()
	if strings.Contains(out, "Repository route action") || strings.Contains(out, "Stage repository-route settings") {
		t.Fatalf("stderr = %q, want flattened route editor without action preflight", out)
	}
	if !strings.Contains(out, "Automatic profile selection") || !strings.Contains(out, "Routes tell cr when to use this profile automatically.") {
		t.Fatalf("stderr = %q, want route editor explanation", out)
	}
	if !strings.Contains(out, "Accepted route formats") || !strings.Contains(out, "host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL.") {
		t.Fatalf("stderr = %q, want route format instructions", out)
	}
	for _, want := range []string{
		"Leave blank to remove all routes for this profile. Examples:",
		"github.com/YourOrg",
		"github.com/YourUsername [RepoA, RepoB] (will not match on RepoC)",
		"github.com/YourOrg/org-repo/pull/123",
		"Separate multiple entries with ;.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr = %q, want route editor copy %q", out, want)
		}
	}
	if strings.Contains(out, "Newline-separated pastes") {
		t.Fatalf("stderr = %q, want route UI copy to avoid newline-paste guidance", out)
	}
	if count := strings.Count(out, "Separate multiple entries with ;."); count != 1 {
		t.Fatalf("stderr = %q, want semicolon guidance once, got %d", out, count)
	}
	if !strings.Contains(out, "Route entries") {
		t.Fatalf("stderr = %q, want route entry instructions", out)
	}
}

func TestHuhInitRoutesPrompterAccessibleBlankingPrefilledRoutesRemovesAll(t *testing.T) {
	t.Setenv("TERM", "xterm")
	var stderr bytes.Buffer
	initial := []configedit.RepositoryRouteSpec{{
		Host:      "github.com",
		Namespace: "open-cli-collective",
		Repos:     []string{"codereview-cli"},
	}}
	prompter := huhInitRoutesPrompter{
		stdin:  strings.NewReader("\x15\r\r"),
		stderr: &stderr,
	}

	edit, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName: "work",
		ProfileHost: "github.com",
		Routes:      initial,
	})
	if err != nil {
		t.Fatalf("EditRoutes: %v", err)
	}
	if edit.Routes != nil {
		t.Fatalf("routes = %#v, want nil after clearing prefilled route text", edit.Routes)
	}
}

func TestHuhInitRoutesPrompterAccessibleEscapeBackNavigatesOut(t *testing.T) {
	t.Setenv("TERM", "xterm")
	var stderr bytes.Buffer
	prompter := huhInitRoutesPrompter{
		stdin:  strings.NewReader("\x1b"),
		stderr: &stderr,
	}

	_, err := prompter.EditRoutes(initRoutesPrompt{
		ProfileName: "work",
		ProfileHost: "github.com",
		Routes: []configedit.RepositoryRouteSpec{{
			Host:      "github.com",
			Namespace: "open-cli-collective",
		}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditRoutes error = %v, want errInitNavigateBack", err)
	}
}

func TestInitReviewerModelTierCopy(t *testing.T) {
	if initReviewerModelTierTitle != "Minimum reviewer model tier" {
		t.Fatalf("title = %q", initReviewerModelTierTitle)
	}
	if initReviewerModelTierDescription != "Sets the minimum model tier for reviewer agents. Agents that require a higher tier still use their higher configured tier." {
		t.Fatalf("description = %q", initReviewerModelTierDescription)
	}

	got := initReviewerModelTierOptions()
	if len(got) != 4 {
		t.Fatalf("len(options) = %d, want 4", len(got))
	}
	if got[0].Key != "Built-in baseline (small)" || got[0].Value != "" {
		t.Fatalf("option[0] = %#v, want built-in small baseline", got[0])
	}
	if got[1].Key != "Small baseline" || got[1].Value != string(config.ModelTierSmall) {
		t.Fatalf("option[1] = %#v, want small baseline", got[1])
	}
	if got[2].Key != "Medium baseline" || got[2].Value != string(config.ModelTierMedium) {
		t.Fatalf("option[2] = %#v, want medium baseline", got[2])
	}
	if got[3].Key != "Large baseline" || got[3].Value != string(config.ModelTierLarge) {
		t.Fatalf("option[3] = %#v, want large baseline", got[3])
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
	if gotCtx.ExistingProfileName != "work" {
		t.Fatalf("prompt context = %#v, want existing work", gotCtx)
	}
	if gotCtx.ExistingProfile == nil || gotCtx.ExistingProfile.Git.CredentialRef != "codereview/work" {
		t.Fatalf("prompt existing profile = %#v, want work profile", gotCtx.ExistingProfile)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
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
	if !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/custom-office-llm --key "+credentials.OpenAIAPIKeyKey+" --stdin") {
		t.Fatalf("stderr = %q, want deferred llm follow-up hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/office-git --key "+credentials.GitHubAppIDKey+" --stdin") {
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
		Profiles: map[string]config.Profile{"work": existing},
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

func TestEditInteractiveInitProfileSkipsInlineProfileDetailCollectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	work := cfg.Profiles["work"]
	draft := seedInteractiveInitDraft("work", "work", &work)
	draft.RoutesSet = true
	draft.ModelMapSet = true
	draft.ModelMap = config.ModelMap{"large": "claude-custom"}
	draft.AgentSourcesSet = true
	draft.AgentSources = []string{"/tmp/agents", "/tmp/agents"}
	draft.ReviewPolicySet = true
	draft.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	deps := initDeps{
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return draft, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatal("routes prompter should not run when RoutesSet is true")
			return initRoutesEdit{}, nil
		}),
		modelMapPrompter: initModelMapPrompterFunc(func(initModelMapPrompt) (initModelMapEdit, error) {
			t.Fatal("model-map prompter should not run when ModelMapSet is true")
			return initModelMapEdit{}, nil
		}),
		agentSourcesPrompter: initAgentSourcesPrompterFunc(func(initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
			t.Fatal("agent-sources prompter should not run when AgentSourcesSet is true")
			return initAgentSourcesEdit{}, nil
		}),
		reviewPolicyPrompter: initReviewPolicyPrompterFunc(func(initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
			t.Fatal("review-policy prompter should not run when ReviewPolicySet is true")
			return initReviewPolicyEdit{}, nil
		}),
	}
	session := initSessionDraft{
		path:                 path,
		originalCfg:          cloneInitConfigFile(cfg),
		cfg:                  cloneInitConfigFile(cfg),
		requestedProfileName: "work",
	}

	next, stay, err := editInteractiveInitProfileStep(&cobra.Command{}, opts, initOptions{}, deps, session)
	if err != nil {
		t.Fatalf("editInteractiveInitProfileStep: %v", err)
	}
	if !stay {
		t.Fatal("stay = false, want profile category to remain active after staging profile")
	}
	profile := next.cfg.Profiles["work"]
	if !reflect.DeepEqual(profile.LLM.ModelMap, config.ModelMap{"large": "claude-custom"}) {
		t.Fatalf("model_map = %#v, want inline model-map edit", profile.LLM.ModelMap)
	}
	if !reflect.DeepEqual(profile.AgentSources, []string{"/tmp/agents"}) {
		t.Fatalf("agent_sources = %#v, want normalized inline agent-source edit", profile.AgentSources)
	}
	if profile.ReviewPolicy != draft.ReviewPolicy {
		t.Fatalf("review_policy = %#v, want %#v", profile.ReviewPolicy, draft.ReviewPolicy)
	}
}

func TestLoopInteractiveInitProfileV2StagesDraftIntoSessionBeforeReentry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("open-cli-collective")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"open-cli-collective": existing,
		},
	}
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	session := initSessionDraft{
		path:                 path,
		originalCfg:          cloneInitConfigFile(cfg),
		cfg:                  cloneInitConfigFile(cfg),
		requestedProfileName: "open-cli-collective",
	}
	calls := 0
	var reentryCtx initPromptContext
	deps := initDeps{
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			calls++
			if calls == 2 {
				reentryCtx = ctx
				return initDraft{}, errInitNavigateBack
			}
			work := ctx.ExistingConfig.Profiles["open-cli-collective"]
			draft := seedInteractiveInitDraft("open-cli-collective", "open-cli-collective", &work)
			draft.ProfileName = "open-cli-collective-lkjlkj"
			draft.GitCredentialRef = "codereview/open-cli-collective12365"
			draft.RoutesSet = true
			draft.ModelMapSet = true
			draft.AgentSourcesSet = true
			draft.ReviewPolicySet = true
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		clipboardSupported: func() bool { return false },
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	next, err := loopInteractiveInitProfileV2(&cobra.Command{}, opts, initOptions{}, deps, session)
	if err != nil {
		t.Fatalf("loopInteractiveInitProfileV2: %v", err)
	}
	if calls != 2 {
		t.Fatalf("profile v2 calls = %d, want stage then reentry", calls)
	}
	if _, ok := next.cfg.Profiles["open-cli-collective"]; ok {
		t.Fatalf("old profile still present after staged rename: %#v", next.cfg.Profiles)
	}
	profile := next.cfg.Profiles["open-cli-collective-lkjlkj"]
	if profile.Git.CredentialRef != "codereview/open-cli-collective12365" {
		t.Fatalf("staged git ref = %q, want edited v2 Git label", profile.Git.CredentialRef)
	}
	if reentryCtx.ExistingConfig.Profiles["open-cli-collective-lkjlkj"].Git.CredentialRef != "codereview/open-cli-collective12365" {
		t.Fatalf("reentry context profile = %#v, want staged v2 edits", reentryCtx.ExistingConfig.Profiles)
	}
}

func TestLoopInteractiveInitProfileV2AppliesInlineDetailDraftParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": existing,
		},
	}
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	session := initSessionDraft{
		path:                 path,
		originalCfg:          cloneInitConfigFile(cfg),
		cfg:                  cloneInitConfigFile(cfg),
		requestedProfileName: "work",
	}
	calls := 0
	var reentryCtx initPromptContext
	policy := config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
		ResolveAfter:     "24h",
	}
	deps := initDeps{
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			calls++
			if calls == 2 {
				reentryCtx = ctx
				return initDraft{}, errInitNavigateBack
			}
			work := ctx.ExistingConfig.Profiles["work"]
			draft := seedInteractiveInitDraft("work", "work", &work)
			draft.GitCredentialRef = "codereview/custom-work-git"
			draft.LLMProvider = string(config.LLMProviderOpenAI)
			draft.LLMAuth = string(config.LLMAuthSubscription)
			draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
			draft.LLMReviewerModelTier = string(config.ModelTierMedium)
			draft.RoutesSet = true
			draft.Routes = []configedit.RepositoryRouteSpec{{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			}}
			draft.ModelMapSet = true
			draft.ModelMap = config.ModelMap{"medium": "gpt-custom"}
			draft.AgentSourcesSet = true
			draft.AgentSources = []string{"/tmp/agents", "/tmp/agents/../agents"}
			draft.ReviewPolicySet = true
			draft.ReviewPolicy = policy
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		clipboardSupported: func() bool { return false },
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	next, err := loopInteractiveInitProfileV2(&cobra.Command{}, opts, initOptions{}, deps, session)
	if err != nil {
		t.Fatalf("loopInteractiveInitProfileV2: %v", err)
	}
	profile := next.cfg.Profiles["work"]
	if profile.Git.CredentialRef != "codereview/custom-work-git" {
		t.Fatalf("git ref = %q, want custom v2 git label", profile.Git.CredentialRef)
	}
	if profile.LLM.Provider != config.LLMProviderOpenAI || profile.LLM.Adapter != config.LLMAdapterCodexCLI || profile.LLM.ReviewerModelTier != config.ModelTierMedium {
		t.Fatalf("llm = %#v, want v2 runtime/model-tier edits", profile.LLM)
	}
	if !reflect.DeepEqual(profile.LLM.ModelMap, config.ModelMap{"medium": "gpt-custom"}) {
		t.Fatalf("model_map = %#v, want v2 model-map edit", profile.LLM.ModelMap)
	}
	if !reflect.DeepEqual(profile.AgentSources, []string{"/tmp/agents"}) {
		t.Fatalf("agent_sources = %#v, want normalized v2 agent sources", profile.AgentSources)
	}
	if profile.ReviewPolicy != policy {
		t.Fatalf("review_policy = %#v, want %#v", profile.ReviewPolicy, policy)
	}
	if len(next.cfg.RepositoryProfiles) != 1 || next.cfg.RepositoryProfiles[0].Profile != "work" || next.cfg.RepositoryProfiles[0].Match.Repos[0] != "codereview-cli" {
		t.Fatalf("repository_profiles = %#v, want v2 route edit", next.cfg.RepositoryProfiles)
	}
	reentryProfile := reentryCtx.ExistingConfig.Profiles["work"]
	if reentryProfile.Git.CredentialRef != "codereview/custom-work-git" || !reflect.DeepEqual(reentryProfile.AgentSources, []string{"/tmp/agents"}) {
		t.Fatalf("reentry profile = %#v, want staged v2 detail edits", reentryProfile)
	}
}

func TestCompleteInteractiveInitProfileV2DraftUsesProfileNamePriorityForRoutes(t *testing.T) {
	ctx := initPromptContext{
		ExistingProfileName: "existing",
		ExistingConfig: config.File{
			RepositoryProfiles: []config.RepositoryProfile{
				{
					Profile: "original",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "OriginalOrg",
					},
				},
				{
					Profile: "existing",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "ExistingOrg",
					},
				},
				{
					Profile: "fallback",
					Match: config.RepositoryProfileMatch{
						Host:      "github.com",
						Namespace: "FallbackOrg",
					},
				},
			},
		},
	}

	tests := []struct {
		name               string
		originalProfile    string
		existingProfile    string
		fallbackProfile    string
		wantRouteNamespace string
	}{
		{
			name:               "original profile wins",
			originalProfile:    "original",
			existingProfile:    "existing",
			fallbackProfile:    "fallback",
			wantRouteNamespace: "OriginalOrg",
		},
		{
			name:               "existing profile wins over fallback",
			existingProfile:    "existing",
			fallbackProfile:    "fallback",
			wantRouteNamespace: "ExistingOrg",
		},
		{
			name:               "fallback profile used last",
			fallbackProfile:    "fallback",
			wantRouteNamespace: "FallbackOrg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextCtx := ctx
			nextCtx.ExistingProfileName = tt.existingProfile
			draft := completeInteractiveInitProfileV2Draft(nextCtx, initDraft{
				OriginalProfileName: tt.originalProfile,
				ProfileName:         tt.fallbackProfile,
			})

			want := []configedit.RepositoryRouteSpec{{
				Host:      "github.com",
				Namespace: tt.wantRouteNamespace,
			}}
			if !reflect.DeepEqual(draft.Routes, want) {
				t.Fatalf("Routes = %#v, want %#v", draft.Routes, want)
			}
			if !draft.RoutesSet {
				t.Fatalf("RoutesSet = false, want true")
			}
		})
	}
}

func TestCompleteInteractiveInitProfileV2DraftPreservesExplicitSetFields(t *testing.T) {
	existing := basicProfile("work")
	existing.LLM.ModelMap = config.ModelMap{"medium": "existing-medium"}
	existing.AgentSources = []string{"/existing/agents"}
	existing.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
	}
	ctx := initPromptContext{
		ExistingProfileName: "work",
		ExistingProfile:     &existing,
		ExistingConfig: config.File{
			RepositoryProfiles: []config.RepositoryProfile{{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "ExistingOrg",
				},
			}},
		},
	}
	wantRoutes := []configedit.RepositoryRouteSpec{{
		Host:      "github.com",
		Namespace: "ExplicitOrg",
	}}
	wantModelMap := config.ModelMap{"large": "explicit-large"}
	wantAgentSources := []string{"/explicit/agents"}
	wantReviewPolicy := config.ReviewPolicy{
		MajorEvent:     config.ReviewMajorEventComment,
		ResolveThreads: config.ResolveThreadsNever,
	}

	draft := completeInteractiveInitProfileV2Draft(ctx, initDraft{
		RoutesSet:       true,
		Routes:          wantRoutes,
		ModelMapSet:     true,
		ModelMap:        wantModelMap,
		AgentSourcesSet: true,
		AgentSources:    wantAgentSources,
		ReviewPolicySet: true,
		ReviewPolicy:    wantReviewPolicy,
	})

	if !reflect.DeepEqual(draft.Routes, wantRoutes) {
		t.Fatalf("Routes = %#v, want explicit %#v", draft.Routes, wantRoutes)
	}
	if !reflect.DeepEqual(draft.ModelMap, wantModelMap) {
		t.Fatalf("ModelMap = %#v, want explicit %#v", draft.ModelMap, wantModelMap)
	}
	if !reflect.DeepEqual(draft.AgentSources, wantAgentSources) {
		t.Fatalf("AgentSources = %#v, want explicit %#v", draft.AgentSources, wantAgentSources)
	}
	if draft.ReviewPolicy != wantReviewPolicy {
		t.Fatalf("ReviewPolicy = %#v, want explicit %#v", draft.ReviewPolicy, wantReviewPolicy)
	}
}

func TestCompleteInteractiveInitProfileV2DraftFillsUnsetFieldsAndDefaultsReviewPolicy(t *testing.T) {
	existing := basicProfile("work")
	existing.LLM.ModelMap = config.ModelMap{"medium": "existing-medium"}
	existing.AgentSources = []string{"/existing/agents"}
	existing.ReviewPolicy = config.ReviewPolicy{
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsNever,
	}
	ctx := initPromptContext{
		ExistingProfileName: "work",
		ExistingProfile:     &existing,
		ExistingConfig: config.File{
			RepositoryProfiles: []config.RepositoryProfile{{
				Profile: "work",
				Match: config.RepositoryProfileMatch{
					Host:      "github.com",
					Namespace: "ExistingOrg",
				},
			}},
		},
	}

	draft := completeInteractiveInitProfileV2Draft(ctx, initDraft{})

	wantRoutes := []configedit.RepositoryRouteSpec{{
		Host:      "github.com",
		Namespace: "ExistingOrg",
	}}
	if !reflect.DeepEqual(draft.Routes, wantRoutes) || !draft.RoutesSet {
		t.Fatalf("Routes = %#v set=%v, want %#v set=true", draft.Routes, draft.RoutesSet, wantRoutes)
	}
	if !reflect.DeepEqual(draft.ModelMap, existing.LLM.ModelMap) || !draft.ModelMapSet {
		t.Fatalf("ModelMap = %#v set=%v, want existing set=true", draft.ModelMap, draft.ModelMapSet)
	}
	if !reflect.DeepEqual(draft.AgentSources, existing.AgentSources) || !draft.AgentSourcesSet {
		t.Fatalf("AgentSources = %#v set=%v, want existing set=true", draft.AgentSources, draft.AgentSourcesSet)
	}
	if !draft.ReviewPolicySet {
		t.Fatalf("ReviewPolicySet = false, want true")
	}
	if draft.ReviewPolicy.MajorEvent != config.ReviewMajorEventComment {
		t.Fatalf("ReviewPolicy.MajorEvent = %q, want default comment", draft.ReviewPolicy.MajorEvent)
	}
	if !draft.ReviewPolicy.AllowSelfApprove || draft.ReviewPolicy.ResolveThreads != config.ResolveThreadsNever {
		t.Fatalf("ReviewPolicy = %#v, want existing fields preserved", draft.ReviewPolicy)
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
				Profiles: map[string]config.Profile{"work": existing},
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
			expected = normalizeTestProfile(expected)
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
				Profiles: map[string]config.Profile{"work": existing},
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

func TestNormalizeInitModelMapDropsBuiltInsAndBlanks(t *testing.T) {
	llm := config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}
	got := normalizeInitModelMap(llm, config.ModelMap{
		"small":  "gpt-5.4-mini",
		"medium": " custom-medium ",
		"large":  " \t ",
	})
	if !reflect.DeepEqual(got, config.ModelMap{"medium": "custom-medium"}) {
		t.Fatalf("normalizeInitModelMap = %#v, want only explicit non-built-in overrides", got)
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
				Profiles: map[string]config.Profile{"work": existing},
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
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
				Profiles: map[string]config.Profile{"work": existing},
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
				Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	if edit.Retention.MaxAgeDaysValue() != config.DefaultRetentionConfig().MaxAgeDaysValue() {
		t.Fatalf("MaxAgeDaysValue = %d, want default %d for omitted retention config", edit.Retention.MaxAgeDaysValue(), config.DefaultRetentionConfig().MaxAgeDaysValue())
	}
	out := stderr.String()
	if !strings.Contains(out, "Maximum run-data age in days") || !strings.Contains(out, "Run data") {
		t.Fatalf("stderr = %q, want retention fields", out)
	}
	if !strings.Contains(out, "local record of review runs and related artifacts/logs") {
		t.Fatalf("stderr = %q, want explanatory run-data note", out)
	}
	if strings.Contains(out, "Stage retention settings") || strings.Contains(out, "Default 90 days") || strings.Contains(out, "Keep forever") || strings.Contains(out, "Custom days") || strings.Contains(out, "Custom max age in days") || strings.Contains(out, "Retention enforcement") {
		t.Fatalf("stderr = %q, want removed retention mode-selector copy absent", out)
	}
}

func TestHuhInitRetentionPrompterXtermBlankResetsToDefault(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitRetentionPrompter{
		stdin:  strings.NewReader("\x15\r"),
		stderr: &bytes.Buffer{},
	}

	thirty := 30
	edit, err := prompter.EditRetention(initRetentionPrompt{
		Retention: config.RetentionConfig{
			MaxAgeDays:  &thirty,
			Enforcement: config.RetentionManualOnly,
		},
	})
	if err != nil {
		t.Fatalf("EditRetention: %v", err)
	}
	if edit.Retention.MaxAgeDaysValue() != config.DefaultRetentionConfig().MaxAgeDaysValue() {
		t.Fatalf("MaxAgeDaysValue = %d, want default %d after blank reset", edit.Retention.MaxAgeDaysValue(), config.DefaultRetentionConfig().MaxAgeDaysValue())
	}
	if edit.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("Enforcement = %q, want preserved manual_only", edit.Retention.Enforcement)
	}
}

func TestHuhInitRetentionPrompterXtermKeepsForeverPrefill(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitRetentionPrompter{
		stdin:  strings.NewReader("\r"),
		stderr: &bytes.Buffer{},
	}

	forever := 0
	edit, err := prompter.EditRetention(initRetentionPrompt{
		Retention: config.RetentionConfig{
			MaxAgeDays: &forever,
		},
	})
	if err != nil {
		t.Fatalf("EditRetention: %v", err)
	}
	if edit.Retention.MaxAgeDaysValue() != 0 {
		t.Fatalf("MaxAgeDaysValue = %d, want preserved keep-forever 0", edit.Retention.MaxAgeDaysValue())
	}
}

func TestHuhInitRetentionPrompterBackReturnsNavigateBack(t *testing.T) {
	t.Setenv("TERM", "xterm")
	prompter := huhInitRetentionPrompter{
		stdin:  strings.NewReader("\x1b"),
		stderr: &bytes.Buffer{},
	}

	_, err := prompter.EditRetention(initRetentionPrompt{Retention: config.RetentionConfig{}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditRetention error = %v, want errInitNavigateBack", err)
	}
}

func TestBubbleTeaInitRetentionEditorShowsLinearGlobalSettingsFlow(t *testing.T) {
	editor := initRetentionEditor(config.RetentionConfig{})
	model := newInitLinearEditorModel(editor, 160, 24)
	view := model.View()

	for _, want := range []string{
		"Global settings",
		"Configure behavior that applies across review profiles.",
		"Run data",
		"Maximum run-data age in days",
		"> 90",
		"Global settings action",
		"Stage global settings",
		"Back without staging",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	assertContentOrder := func(parts ...string) {
		t.Helper()
		previous := -1
		for _, part := range parts {
			index := strings.Index(view, part)
			if index < 0 {
				t.Fatalf("view missing %q:\n%s", part, view)
			}
			if index <= previous {
				t.Fatalf("view order wrong for %q:\n%s", part, view)
			}
			previous = index
		}
	}
	assertContentOrder("Global settings", "Run data", "Maximum run-data age in days", "Global settings action")
}

func TestBubbleTeaInitRetentionPrompterStagesEditedValue(t *testing.T) {
	thirty := 30
	prompter := bubbleTeaInitRetentionPrompter{
		editorRunner: func(editor initLinearEditor, _ io.Reader, _ io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 24)
			model = focusInitLinearField(t, model, initRetentionFieldMaxAge)
			model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
			model = typeInitLinearText(t, model, "45")
			model = focusInitLinearField(t, model, initRetentionFieldAction)
			updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd == nil {
				t.Fatal("Update returned nil command, want quit command after staging")
			}
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	edit, err := prompter.EditRetention(initRetentionPrompt{
		Retention: config.RetentionConfig{
			MaxAgeDays:  &thirty,
			Enforcement: config.RetentionManualOnly,
		},
	})
	if err != nil {
		t.Fatalf("EditRetention: %v", err)
	}
	if !edit.Apply {
		t.Fatal("edit.Apply = false, want true")
	}
	if edit.Retention.MaxAgeDaysValue() != 45 {
		t.Fatalf("MaxAgeDaysValue = %d, want 45", edit.Retention.MaxAgeDaysValue())
	}
	if edit.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("Enforcement = %q, want preserved manual_only", edit.Retention.Enforcement)
	}
}

func TestBubbleTeaInitRetentionPrompterBlankResetsToDefault(t *testing.T) {
	thirty := 30
	prompter := bubbleTeaInitRetentionPrompter{
		editorRunner: func(editor initLinearEditor, _ io.Reader, _ io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 24)
			model = focusInitLinearField(t, model, initRetentionFieldMaxAge)
			model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
			model = focusInitLinearField(t, model, initRetentionFieldAction)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	edit, err := prompter.EditRetention(initRetentionPrompt{
		Retention: config.RetentionConfig{MaxAgeDays: &thirty},
	})
	if err != nil {
		t.Fatalf("EditRetention: %v", err)
	}
	if edit.Retention.MaxAgeDaysValue() != config.DefaultRetentionConfig().MaxAgeDaysValue() {
		t.Fatalf("MaxAgeDaysValue = %d, want default %d", edit.Retention.MaxAgeDaysValue(), config.DefaultRetentionConfig().MaxAgeDaysValue())
	}
}

func TestBubbleTeaInitRetentionPrompterBackReturnsNavigateBack(t *testing.T) {
	prompter := bubbleTeaInitRetentionPrompter{
		editorRunner: func(editor initLinearEditor, _ io.Reader, _ io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 160, 24)
			model = focusInitLinearField(t, model, initRetentionFieldAction)
			model = selectInitLinearFieldValue(t, model, initRetentionFieldAction, initDetailActionBack)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	_, err := prompter.EditRetention(initRetentionPrompt{Retention: config.RetentionConfig{}})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditRetention error = %v, want errInitNavigateBack", err)
	}
}

func TestBubbleTeaInitRetentionActionKeepsEditorOpenOnValidationError(t *testing.T) {
	editor := initRetentionEditor(config.RetentionConfig{})
	model := newInitLinearEditorModel(editor, 160, 24)
	model = focusInitLinearField(t, model, initRetentionFieldMaxAge)
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitLinearText(t, model, "abc")
	model = focusInitLinearField(t, model, initRetentionFieldAction)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if cmd != nil {
		t.Fatal("Update returned quit command, want editor to stay open on validation error")
	}
	if next.resultAction != "" {
		t.Fatalf("resultAction = %q, want empty", next.resultAction)
	}
	actionIndex := next.document.fieldIndexByID(initRetentionFieldAction)
	if actionIndex < 0 || !strings.Contains(next.document[actionIndex].Error, "whole number") {
		t.Fatalf("action error = %q, want whole-number validation", next.document[actionIndex].Error)
	}
}

func TestInitLinearEditorActionQuitClearsFinalView(t *testing.T) {
	editor := initRetentionEditor(config.RetentionConfig{})
	model := newInitLinearEditorModel(editor, 160, 24)
	model = focusInitLinearField(t, model, initRetentionFieldAction)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if cmd == nil {
		t.Fatal("Update returned nil command, want quit command after staging")
	}
	if !next.quitting {
		t.Fatal("quitting = false, want action quit to clear final rendered frame")
	}
	if got := next.View(); got != "" {
		t.Fatalf("View after action quit = %q, want empty", got)
	}
}

func TestInitLinearEditorDefaultDeleteShortcutStillQuits(t *testing.T) {
	const choiceField initLinearFieldID = "choice"
	var document initLinearDocument
	document.addEditableSelect(choiceField, "Choice", "", []huh.Option[string]{
		huh.NewOption("Configured item", "configured"),
	}, "configured")
	editor := initLinearEditor{Document: document}
	model := newInitLinearEditorModel(editor, 120, 12)
	index := model.document.fieldIndexByID(choiceField)
	model.document[index].Options[0].Deletable = true

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if cmd == nil {
		t.Fatal("delete shortcut returned nil command, want default quit behavior")
	}
	if next.resultAction != initLinearResultActionDelete {
		t.Fatalf("resultAction = %q, want delete", next.resultAction)
	}
	if !next.quitting {
		t.Fatal("quitting = false, want default delete shortcut to quit")
	}
}

func TestInitLinearEditorArrowKeysChangeSelectNotFocus(t *testing.T) {
	const choiceField initLinearFieldID = "choice"
	const inputField initLinearFieldID = "input"
	var document initLinearDocument
	document.addEditableSelect(choiceField, "Choice", "", []huh.Option[string]{
		huh.NewOption("Alpha", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")
	document.addEditableInput(inputField, "Input", "", "value", nil)
	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 120, 12)
	choiceIndex := model.document.fieldIndexByID(choiceField)

	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if got := model.document.selectedValue(choiceField); got != "beta" {
		t.Fatalf("selected value after down = %q, want beta", got)
	}
	if model.focused != choiceIndex {
		t.Fatalf("focused after select down = %d, want unchanged choice index %d", model.focused, choiceIndex)
	}

	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyUp})
	if got := model.document.selectedValue(choiceField); got != "alpha" {
		t.Fatalf("selected value after up = %q, want alpha", got)
	}
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
	inputIndex := model.document.fieldIndexByID(inputField)
	if model.focused != inputIndex {
		t.Fatalf("focused after tab = %d, want input index %d", model.focused, inputIndex)
	}
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if model.focused != inputIndex {
		t.Fatalf("focused after input down = %d, want unchanged input index %d", model.focused, inputIndex)
	}
}

func TestInitLinearEditorArrowKeysDoNotScrollFocusedInput(t *testing.T) {
	const inputField initLinearFieldID = "input"
	var document initLinearDocument
	document.addEditableInput(inputField, "Input", "", "value", nil)
	for i := 0; i < 20; i++ {
		document.addSection(fmt.Sprintf("Section %02d", i), "Context line")
	}
	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 120, 5)
	model.setYOffset(1)

	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if got := model.viewport.YOffset; got != 1 {
		t.Fatalf("viewport YOffset after input down = %d, want unchanged 1", got)
	}
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyUp})
	if got := model.viewport.YOffset; got != 1 {
		t.Fatalf("viewport YOffset after input up = %d, want unchanged 1", got)
	}
}

func TestInitLinearEditorOnlyFocusedSelectedFieldShowsCaret(t *testing.T) {
	const firstField initLinearFieldID = "first"
	const secondField initLinearFieldID = "second"
	var document initLinearDocument
	document.addEditableSelect(firstField, "First", "", []huh.Option[string]{
		huh.NewOption("Alpha", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")
	document.addEditableSelect(secondField, "Second", "", []huh.Option[string]{
		huh.NewOption("Gamma", "gamma"),
		huh.NewOption("Delta", "delta"),
	}, "delta")

	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 120, 12)
	if got := strings.Count(model.layout.Content, "> "); got != 1 {
		t.Fatalf("initial caret count = %d, want 1:\n%s", got, model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "> [x] Alpha") {
		t.Fatalf("initial content missing focused selected option:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [x] Delta") {
		t.Fatalf("initial content missing unfocused selected option marker:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [ ] Beta") {
		t.Fatalf("initial content missing unselected option marker:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "> [x] Delta") {
		t.Fatalf("initial content shows caret on unfocused selected option:\n%s", model.layout.Content)
	}

	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
	if got := strings.Count(model.layout.Content, "> "); got != 1 {
		t.Fatalf("caret count after tab = %d, want 1:\n%s", got, model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "> [x] Delta") {
		t.Fatalf("content after tab missing focused selected option:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [x] Alpha") {
		t.Fatalf("content after tab missing unfocused selected option marker:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "> [x] Alpha") {
		t.Fatalf("content after tab shows caret on unfocused selected option:\n%s", model.layout.Content)
	}
}

func TestInitLinearEditorSelectMarkerWrapsAndMarksSelectedLines(t *testing.T) {
	const choiceField initLinearFieldID = "choice"
	const width = 24
	var document initLinearDocument
	document.addEditableSelect(choiceField, "Choice", "", []huh.Option[string]{
		huh.NewOption("Alpha selected option wraps cleanly", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")

	model := newInitLinearEditorModel(initLinearEditor{Document: document}, width, 12)
	want := "> [x] Alpha selected\n      option wraps\n      cleanly"
	if !strings.Contains(model.layout.Content, want) {
		t.Fatalf("wrapped selected option missing marker or aligned continuations:\n%s", model.layout.Content)
	}
	lines := strings.Split(model.layout.Content, "\n")
	start := -1
	for index, line := range lines {
		if line == "> [x] Alpha selected" {
			start = index
			break
		}
	}
	if start < 0 {
		t.Fatalf("wrapped selected option start missing:\n%s", model.layout.Content)
	}
	for _, index := range []int{start, start + 1, start + 2} {
		if !model.layout.SelectedLines[index] {
			t.Fatalf("wrapped selected line %d is not marked selected; selected lines = %#v\n%s", index, model.layout.SelectedLines, model.layout.Content)
		}
	}
	for _, line := range lines {
		if len(line) > width {
			t.Fatalf("wrapped line length = %d, want <= %d for %q\n%s", len(line), width, line, model.layout.Content)
		}
	}
}

func TestValidateRetentionMaxAgeDaysUsesCurrentFieldCopy(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "non-number",
			value: "abc",
			want:  "maximum run-data age in days must be a whole number",
		},
		{
			name:  "negative",
			value: "-1",
			want:  "maximum run-data age in days must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRetentionMaxAgeDays(tt.value)
			if err == nil {
				t.Fatalf("validateRetentionMaxAgeDays(%q) error = nil, want %q", tt.value, tt.want)
			}
			if err.Error() != tt.want {
				t.Fatalf("validateRetentionMaxAgeDays(%q) error = %q, want %q", tt.value, err.Error(), tt.want)
			}
		})
	}
}

func TestHuhInitKeyringBackendPrompterAccessibleShowsField(t *testing.T) {
	rows := initSecretsManagementInventoryRows(config.File{})
	if len(rows) == 0 {
		t.Fatal("initSecretsManagementInventoryRows returned no rows")
	}
	if rows[0].ID != config.LocalOSCredentialStoreID {
		t.Fatalf("first row id = %q, want built-in OS credential store first", rows[0].ID)
	}
	if rows[0].Title != initBuiltInOSCredentialStoreTitle() {
		t.Fatalf("first row title = %q, want built-in OS credential-store title", rows[0].Title)
	}
	if rows[0].Description != initBuiltInOSCredentialStoreDescription() {
		t.Fatalf("first row description = %q, want built-in OS credential-store description", rows[0].Description)
	}
	if rows[0].Deletable {
		t.Fatalf("built-in row is deletable: %#v", rows[0])
	}
	var foundConfigure bool
	for _, row := range rows {
		if row.Title == "Configure new encrypted file profile" {
			foundConfigure = true
			break
		}
	}
	if !foundConfigure {
		t.Fatalf("rows = %#v, want configure-new backend command copy", rows)
	}
}

func TestInitAutomaticOSDefaultSecretsBackendLabelForGOOS(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{goos: "darwin", want: "macOS Login Keychain"},
		{goos: "windows", want: "Windows Credential Manager"},
		{goos: "linux", want: "Linux Secret Service"},
		{goos: "plan9", want: "OS credential store"},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			if got := initAutomaticOSDefaultSecretsBackendLabelForGOOS(tt.goos); got != tt.want {
				t.Fatalf("initAutomaticOSDefaultSecretsBackendLabelForGOOS(%q) = %q, want %q", tt.goos, got, tt.want)
			}
		})
	}
}

func TestInitSecretsManagementInventoryRowsUsePreferredBackendOrder(t *testing.T) {
	rows := initSecretsManagementInventoryRows(config.File{})
	titles := make([]string, 0, len(rows))
	for _, row := range rows {
		titles = append(titles, row.Title)
	}
	assertContentOrder(t, strings.Join(titles, "\n"),
		initBuiltInOSCredentialStoreTitle(),
		"Configure new 1Password desktop app profile",
		"Configure new 1Password service account profile",
		"Configure new 1Password Connect profile",
		"Configure new pass password store profile",
		"Configure new encrypted file profile",
		"Configure new in-memory store profile",
	)
}

func TestInitInteractiveProfileSubflowBackPreservesBuiltWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		}},
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	profileCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		profileV2Prompter: initPrompterFunc(func(prompt initPromptContext) (initDraft, error) {
			profileCalls++
			switch profileCalls {
			case 1:
				return initDraft{
					OriginalProfileName: "work",
					ProfileName:         "work",
					GitHost:             "gitlab.com",
					GitAuth:             string(config.GitAuthModePAT),
					GitCredentialRef:    "codereview/work",
					LLMProvider:         string(config.LLMProviderAnthropic),
					LLMAuth:             string(config.LLMAuthSubscription),
					LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					RoutesSet:           true,
					Routes: []configedit.RepositoryRouteSpec{{
						Host:      "gitlab.com",
						Namespace: "open-cli-collective",
					}},
				}, nil
			case 2:
				if prompt.ExistingProfile == nil {
					t.Fatal("ExistingProfile = nil, want staged work profile after route Back")
				}
				if got := prompt.ExistingProfile.Git.Host; got != "gitlab.com" {
					t.Fatalf("ExistingProfile.Git.Host = %q, want staged gitlab.com host after route Back", got)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected profile prompt #%d", profileCalls)
				return initDraft{}, nil
			}
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatalf("routes prompter should not run from v2 profile editor path: %#v", prompt)
			return initRoutesEdit{}, nil
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
	if profileCalls != 2 {
		t.Fatalf("profileCalls = %d, want staged profile pass then explicit Back", profileCalls)
	}
	got := menu.prompts[1]
	if got.ActiveProfileName != "work" || !got.CanSave || got.ReviewProfileCount != 1 {
		t.Fatalf("post-Back menu prompt = %#v, want active work workspace preserved", got)
	}
}

func TestInitInteractiveSecretsManagementBackPreservesEarlierRetentionDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
			initMenuActionSecretsManagement,
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
		t.Fatalf("retention = %#v, want retained 45/manual after secrets-management Back", cfg.Data.Retention)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatalf("profiles = %#v, want work profile preserved", cfg.Profiles)
	}
}

func TestHuhInitFinalizePrompterAccessibleBackReturnsBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitFinalizePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Back to main menu.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	action, err := prompter.ChooseFinalizeAction(initFinalizePrompt{
		Profiles: []initProfileReadiness{{ProfileName: "work", Ready: true}},
	})
	if err != nil {
		t.Fatalf("ChooseFinalizeAction: %v", err)
	}
	if action != initFinalizeActionBack {
		t.Fatalf("action = %q, want Back", action)
	}
	if !strings.Contains(stderr.String(), "Back to main menu") {
		t.Fatalf("stderr = %q, want finalize Back option", stderr.String())
	}
}

func TestHuhInitFinalizePrompterAccessibleShowsCommitAndDiscardOptions(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitFinalizePrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"2", // Back to main menu.
			"",
		}, "\n")),
		stderr: &stderr,
	}

	action, err := prompter.ChooseFinalizeAction(initFinalizePrompt{
		Profiles: []initProfileReadiness{{ProfileName: "work", Ready: true}},
	})
	if err != nil {
		t.Fatalf("ChooseFinalizeAction: %v", err)
	}
	if action != initFinalizeActionBack {
		t.Fatalf("action = %q, want Back", action)
	}
	out := stderr.String()
	for _, want := range []string{
		"Commit staged changes and exit",
		"Discard staged changes and exit",
		"Back to main menu",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr = %q, want finalize option %q", out, want)
		}
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
				Keyring:  config.KeyringConfig{Backend: "file"},
				Profiles: map[string]config.Profile{"work": existing},
				Data:     config.DataConfig{Retention: tt.existing},
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
			if cfg.Keyring.Backend != "" {
				t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
			}
		})
	}
}

func TestInitSecretsProfileIDFromLabelDeconflictsDeterministically(t *testing.T) {
	existing := map[string]config.SecretsProfile{
		"work-vault":   {},
		"work-vault-2": {},
	}
	got := initSecretsProfileIDFromLabel("Work Vault", config.SecretsBackendKind(credstore.BackendFile), existing)
	if got != "work-vault-3" {
		t.Fatalf("initSecretsProfileIDFromLabel = %q, want work-vault-3", got)
	}
}

func TestInitSecretsManagementInventoryRowsOmitBuiltInBackendsFromConfigureActions(t *testing.T) {
	rows := initSecretsManagementInventoryRows(config.File{})
	availability := map[string]bool{}
	for _, row := range rows {
		if strings.HasPrefix(row.ID, initConfigureSecretsProfileSelectionPrefix) {
			availability[row.ID] = row.Selectable
		}
	}
	for _, backend := range []credstore.Backend{
		credstore.BackendKeychain,
		credstore.BackendWinCred,
		credstore.BackendSecretService,
	} {
		if _, ok := availability[initConfigureSecretsProfileSelectionPrefix+string(backend)]; ok {
			t.Fatalf("%s should not be a configure-new action; OS credential stores are projected as local-os", backend)
		}
	}
	if _, ok := availability[initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendFile)]; !ok {
		t.Fatalf("encrypted file backend should be configurable: %#v", availability)
	}
}

func TestValidateInitSecretsRequiredSingleLine(t *testing.T) {
	if err := validateInitSecretsRequiredSingleLine("", true, "1Password vault name or id"); err == nil || err.Error() != "1Password vault name or id is required" {
		t.Fatalf("required validator error = %v, want required field failure", err)
	}
	if err := validateInitSecretsRequiredSingleLine("https://connect.example", true, "1Password Connect host"); err != nil {
		t.Fatalf("required validator valid input error = %v, want nil", err)
	}
}

func TestInitSecretsProfileBackendOptionsExcludeUnavailableChoicesUnlessCurrent(t *testing.T) {
	options := initSecretsProfileBackendOptions(config.SecretsBackendKind(credstore.BackendFile))
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	switch runtime.GOOS {
	case "darwin":
		for _, value := range values {
			if value == string(credstore.BackendWinCred) {
				t.Fatalf("wincred should be excluded from selectable backend options on darwin: %v", values)
			}
		}
	}
	if !initOnePasswordBackendsAvailable() {
		for _, backend := range []credstore.Backend{
			credstore.BackendOPDesktop,
			credstore.BackendOP,
			credstore.BackendOPConnect,
		} {
			if slices.Contains(values, string(backend)) {
				t.Fatalf("%s should be excluded from selectable backend options when 1Password is compiled out: %v", backend, values)
			}
		}
	}
}

func TestInitSecretsProfileBackendOptionsUsePreferredOrder(t *testing.T) {
	options := initSecretsProfileBackendOptions(config.SecretsBackendKind(credstore.BackendFile))
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	joined := "\n" + strings.Join(values, "\n") + "\n"
	want := []string{}
	if initOnePasswordBackendsAvailable() {
		want = append(want,
			"\n"+string(credstore.BackendOPDesktop)+"\n",
			"\n"+string(credstore.BackendOP)+"\n",
			"\n"+string(credstore.BackendOPConnect)+"\n",
		)
	}
	want = append(want,
		"\n"+string(credstore.BackendPass)+"\n",
		"\n"+string(credstore.BackendFile)+"\n",
	)
	want = append(want, "\n"+string(credstore.BackendMemory)+"\n")
	assertContentOrder(t, joined, want...)
}

func TestInitSecretsManagementLinearEditorHidesOnePasswordCreateTargetsWhenUnavailable(t *testing.T) {
	if initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are hidden only in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	targetIndex := model.document.fieldIndexByID(initSecretsManagementFieldTarget)
	if targetIndex < 0 {
		t.Fatal("target field missing")
	}
	for _, backend := range []credstore.Backend{
		credstore.BackendOPDesktop,
		credstore.BackendOP,
		credstore.BackendOPConnect,
	} {
		targetValue := initConfigureSecretsProfileSelectionPrefix + string(backend)
		for _, option := range model.document[targetIndex].Options {
			if option.Value == targetValue {
				t.Fatalf("target options include %q in keyring_no1password build: %#v", targetValue, model.document[targetIndex].Options)
			}
		}
	}
}

func TestHuhInitKeyringBackendPrompterStagesNewSecretsProfileEndToEnd(t *testing.T) {
	t.Setenv("TERM", "dumb")
	callCount := 0
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"", // keep backend-derived label
			"", // keep selected backend
			"", // stage settings
		}, "\n")),
		stderr: &stderr,
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, _ io.Writer) (initInventoryResult, error) {
			callCount++
			switch callCount {
			case 1:
				return initInventoryResult{
					Action: initInventoryActionCommand,
					Row:    initInventoryRow{ID: initConfigureSecretsProfileSelectionPrefix + string(credstore.BackendFile)},
				}, nil
			case 2:
				return initInventoryResult{
					Action: initInventoryActionBack,
					Row:    initInventoryRow{ID: initBackSelection},
				}, nil
			default:
				t.Fatalf("unexpected inventory call %d", callCount)
				return initInventoryResult{}, nil
			}
		},
	}

	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if err != nil {
		t.Fatalf("EditKeyringBackend: %v\n%s", err, stderr.String())
	}
	if !edit.Apply {
		t.Fatalf("edit = %#v, want apply=true", edit)
	}
	profile, ok := edit.Config.Secrets.Profiles["encrypted-file"]
	if !ok {
		t.Fatalf("secrets profiles = %#v, want generated encrypted-file profile", edit.Config.Secrets.Profiles)
	}
	if profile.Backend.Kind != config.SecretsBackendKind(credstore.BackendFile) {
		t.Fatalf("backend kind = %q, want file", profile.Backend.Kind)
	}
	if profile.Label != "Encrypted file" {
		t.Fatalf("profile label = %q, want backend-derived label", profile.Label)
	}
}

func TestHuhInitKeyringBackendPrompterDefaultUsesLinearSecretsManagementFlow(t *testing.T) {
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stderr:        &stderr,
		discoveryMode: initSecretsBackendDiscoveryModeOff,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendFile))
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionEdit)
			model.resultAction = initDetailActionEdit
			return model, nil
		},
	}

	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if err != nil {
		t.Fatalf("EditKeyringBackend: %v", err)
	}
	if !edit.Apply || !edit.HasConfigEdit {
		t.Fatalf("edit = %#v, want config edit", edit)
	}
	profile, ok := edit.Config.Secrets.Profiles["encrypted-file"]
	if !ok {
		t.Fatalf("secrets profiles = %#v, want generated encrypted-file profile", edit.Config.Secrets.Profiles)
	}
	if profile.Backend.Kind != config.SecretsBackendKind(credstore.BackendFile) {
		t.Fatalf("backend kind = %q, want file", profile.Backend.Kind)
	}
	out := stderr.String()
	for _, want := range []string{
		"Secrets storage",
		"Credential store",
		"Configure new encrypted file profile",
		"Credential store name",
		"Secrets-storage action",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	assertContentOrder(t, out, "Credential store", "Credential store name", "Secrets-storage action")
	if strings.Contains(out, "Back to main menu") {
		t.Fatalf("stderr = %q, want action-local Back without staging instead of inventory Back", out)
	}
	if strings.Contains(out, "1Password vault name or id") {
		t.Fatalf("stderr = %q, want file backend to hide 1Password-specific fields", out)
	}
}

func TestHuhInitKeyringBackendPrompterWritesDiscoveryNoticeBeforeOnePasswordProbe(t *testing.T) {
	var stderr bytes.Buffer
	probeSawNotice := false
	prompter := huhInitKeyringBackendPrompter{
		stderr: &stderr,
		executableLookPath: func(name string) (string, error) {
			if name == "pass" {
				return "", exec.ErrNotFound
			}
			return "", fmt.Errorf("unexpected executable lookup %q", name)
		},
		onePasswordCmdRunner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "account list --format=json" {
				out := stderr.String()
				probeSawNotice = strings.Contains(out, "Checking available secrets storage backends.") &&
					strings.Contains(out, "You may see permission prompts")
			}
			return nil, os.ErrNotExist
		},
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionBack)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	_, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditKeyringBackend error = %v, want navigate back", err)
	}
	if !probeSawNotice {
		t.Fatalf("1Password probe ran before discovery notice; stderr = %q", stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{
		initBuiltInOSCredentialStoreTitle() + ": available",
		"1Password desktop app: not found",
		"pass password store: not found",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing probe result %q:\n%s", want, out)
		}
	}
}

func TestHuhInitKeyringBackendPrompterWritesDiscoveryProbeResults(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password probe result uses 1Password-enabled backend labels")
	}
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stderr: &stderr,
		executableLookPath: func(name string) (string, error) {
			if name == "pass" {
				return "/usr/bin/pass", nil
			}
			return "", fmt.Errorf("unexpected executable lookup %q", name)
		},
		onePasswordCmdRunner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "account list --format=json":
				return []byte(`[{"account_uuid":"acct-1","account_name":"SignalFT","url":"signalft.1password.com"}]`), nil
			case "vault list --account acct-1 --format=json":
				return []byte(`[{"id":"vault-emp","name":"Employee"}]`), nil
			default:
				return nil, fmt.Errorf("unexpected op command %q", strings.Join(args, " "))
			}
		},
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionBack)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	_, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditKeyringBackend error = %v, want navigate back", err)
	}
	out := stderr.String()
	for _, want := range []string{
		initBuiltInOSCredentialStoreTitle() + ": available",
		"1Password desktop app: available (1 account, 1 vault)",
		"pass password store: available",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing probe result %q:\n%s", want, out)
		}
	}
	assertContentOrder(t, out, "Checking available secrets storage backends.", "1Password desktop app: available (1 account, 1 vault)", "pass password store: available")
}

func TestHuhInitKeyringBackendPrompterSafeDiscoverySkipsActiveInventory(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stderr:        &stderr,
		discoveryMode: initSecretsBackendDiscoveryModeSafe,
		executableLookPath: func(name string) (string, error) {
			if name == "pass" {
				return "", exec.ErrNotFound
			}
			return "", fmt.Errorf("unexpected executable lookup %q", name)
		},
		onePasswordCmdRunner: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("1Password command runner called despite safe discovery mode")
			return nil, nil
		},
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionBack)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	_, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditKeyringBackend error = %v, want navigate back", err)
	}
	out := stderr.String()
	for _, want := range []string{
		"Only passive discovery is enabled; inventory probes are skipped.",
		"1Password desktop app: skipped",
		"pass password store: not found",
		"1Password account URL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing safe discovery output %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "You may see permission prompts") {
		t.Fatalf("safe discovery output still mentions permission prompts:\n%s", out)
	}
}

func TestHuhInitKeyringBackendPrompterOffDiscoverySkipsAllBackendProbes(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	var stderr bytes.Buffer
	prompter := huhInitKeyringBackendPrompter{
		stderr:        &stderr,
		discoveryMode: initSecretsBackendDiscoveryModeOff,
		executableLookPath: func(name string) (string, error) {
			t.Fatalf("executable lookup %q called despite off discovery mode", name)
			return "", nil
		},
		onePasswordCmdRunner: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("1Password command runner called despite off discovery mode")
			return nil, nil
		},
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			model := newInitLinearEditorModel(editor, 180, 32)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
			_, _ = io.WriteString(out, model.layout.Content)
			model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionBack)
			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			return next, nil
		},
	}

	_, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("EditKeyringBackend error = %v, want navigate back", err)
	}
	out := stderr.String()
	for _, want := range []string{
		"Secrets backend discovery is disabled.",
		"1Password desktop app: skipped",
		"pass password store: skipped",
		"1Password account URL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing off discovery output %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Checking available secrets storage backends.") {
		t.Fatalf("off discovery output still says it is checking backends:\n%s", out)
	}
}

func TestResolveInitSecretsBackendDiscoveryModeUsesEnvUnlessFlagSet(t *testing.T) {
	t.Setenv(initSecretsBackendDiscoveryEnv, "safe")
	cmd := newInitCommand(&root.Options{Stdin: failReader{}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	mode, err := resolveInitSecretsBackendDiscoveryMode(cmd, initOptions{secretsDiscovery: string(initSecretsBackendDiscoveryModeFull)})
	if err != nil {
		t.Fatalf("resolveInitSecretsBackendDiscoveryMode env: %v", err)
	}
	if mode != initSecretsBackendDiscoveryModeSafe {
		t.Fatalf("mode = %q, want safe from env", mode)
	}
	if err := cmd.Flags().Set("secret-backend-discovery", "off"); err != nil {
		t.Fatalf("set secret-backend-discovery: %v", err)
	}
	mode, err = resolveInitSecretsBackendDiscoveryMode(cmd, initOptions{secretsDiscovery: string(initSecretsBackendDiscoveryModeOff)})
	if err != nil {
		t.Fatalf("resolveInitSecretsBackendDiscoveryMode flag: %v", err)
	}
	if mode != initSecretsBackendDiscoveryModeOff {
		t.Fatalf("mode = %q, want off from flag", mode)
	}
}

func TestResolveInitSecretsBackendDiscoveryModeRejectsInvalidValue(t *testing.T) {
	_, err := resolveInitSecretsBackendDiscoveryMode(&cobra.Command{}, initOptions{secretsDiscovery: "loud"})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d; err=%v", got, exitcode.UsageError, err)
	}
	if !strings.Contains(err.Error(), "valid values are full, safe, off") {
		t.Fatalf("error = %v, want valid-values copy", err)
	}
}

func TestInitSecretsManagementLinearEditorShowsBuiltInOSStoreReadOnly(t *testing.T) {
	cfg := config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	out := model.layout.Content
	for _, want := range []string{
		"Secrets storage",
		"Built in",
		"These built-in credential stores are always available and cannot be deleted:",
		initBuiltInOSCredentialStoreTitle(),
		initBuiltInOSCredentialStoreDescription(),
		"Actions",
		"Configure new encrypted file profile",
		"Back without staging",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("initial linear editor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[x] "+initBuiltInOSCredentialStoreTitle()) || strings.Contains(out, "[ ] "+initBuiltInOSCredentialStoreTitle()) {
		t.Fatalf("built-in OS store rendered as selectable checkbox row:\n%s", out)
	}
	for _, stale := range []string{
		"Fallback credential store",
		"Default credential store",
		"Default secrets-storage profile",
		"secrets-storage profile",
	} {
		if strings.Contains(out, stale) {
			t.Fatalf("initial linear editor output contains stale copy %q:\n%s", stale, out)
		}
	}
}

func TestHuhInitKeyringBackendPrompterLinearCanDeleteConfiguredSecretsProfile(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"personal": {
					Label: "1Password",
					Backend: config.SecretsProfileBackend{
						Kind:        config.SecretsBackendKind(credstore.BackendOPDesktop),
						OnePassword: &config.SecretsProfileOnePasswordConfig{VaultID: "Personal"},
					},
				},
				"onepasswordfoo": {
					Label: "1PasswordFoo",
					Backend: config.SecretsProfileBackend{
						Kind:        config.SecretsBackendKind(credstore.BackendOPDesktop),
						OnePassword: &config.SecretsProfileOnePasswordConfig{VaultID: "Personal"},
					},
				},
			},
		},
	}
	var stderr bytes.Buffer
	editorCalls := 0
	prompter := huhInitKeyringBackendPrompter{
		stderr:        &stderr,
		discoveryMode: initSecretsBackendDiscoveryModeOff,
		editorRunner: func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
			editorCalls++
			model := newInitLinearEditorModel(editor, 180, 32)
			switch editorCalls {
			case 1:
				model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, "onepasswordfoo")
				model = focusInitLinearField(t, model, initSecretsManagementFieldTarget)
				updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
				next, ok := updated.(initLinearEditorModel)
				if !ok {
					t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
				}
				return next, nil
			case 2:
				_, _ = io.WriteString(out, model.View())
				if !strings.Contains(model.layout.Content, "1PasswordFoo (1Password desktop app) (Staged for deletion)") {
					t.Fatalf("second editor content missing pending row:\n%s", model.layout.Content)
				}
				targetIndex := model.document.fieldIndexByID(initSecretsManagementFieldTarget)
				targetOptions := model.document[targetIndex].Options
				if got := targetOptions[len(targetOptions)-1].Value; got != initSecretsManagementRestoreSelectionPrefix+"onepasswordfoo" {
					t.Fatalf("last target option = %q, want staged deletion restore option last; options = %#v", got, targetOptions)
				}
				model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initSecretsManagementRestoreSelectionPrefix+"onepasswordfoo")
				model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
				model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionEdit)
				updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
				next, ok := updated.(initLinearEditorModel)
				if !ok {
					t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
				}
				return next, nil
			default:
				t.Fatalf("unexpected editor call %d", editorCalls)
				return initLinearEditorModel{}, nil
			}
		},
	}

	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{Config: cfg})
	if err != nil {
		t.Fatalf("EditKeyringBackend: %v", err)
	}
	if !edit.Apply || !edit.HasConfigEdit {
		t.Fatalf("edit = %#v, want config edit", edit)
	}
	if _, ok := edit.Config.Secrets.Profiles["onepasswordfoo"]; ok {
		t.Fatalf("secrets profiles = %#v, want onepasswordfoo removed", edit.Config.Secrets.Profiles)
	}
	if _, ok := edit.Config.Secrets.Profiles["personal"]; !ok {
		t.Fatalf("secrets profiles = %#v, want personal retained", edit.Config.Secrets.Profiles)
	}
	if !strings.Contains(stderr.String(), "1PasswordFoo (1Password desktop app) (Staged for deletion)") {
		t.Fatalf("stderr = %q, want staged deletion suffix", stderr.String())
	}
}

func TestInitSecretsManagementTargetOptionsMovesPendingDeletesToBottomInDeletionOrder(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"personal": {
					Label:   "Personal",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
	}
	pendingDeletes := map[string]initPendingSecretsManagementDelete{
		"alpha": {ID: "alpha", Profile: config.SecretsProfile{
			Label:   "Alpha",
			Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
		}},
		"beta": {ID: "beta", Profile: config.SecretsProfile{
			Label:   "Beta",
			Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
		}},
	}

	options := initSecretsManagementTargetOptions(cfg, pendingDeletes, []string{"alpha", "beta"})
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	wantSuffix := []string{
		initSecretsManagementRestoreSelectionPrefix + "alpha",
		initSecretsManagementRestoreSelectionPrefix + "beta",
	}
	if len(values) < len(wantSuffix) || !reflect.DeepEqual(values[len(values)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("target option values = %#v, want pending deletes last in staging order %#v", values, wantSuffix)
	}
}

func TestInitSecretsManagementTargetOptionsExcludeBuiltInOSStore(t *testing.T) {
	options := initSecretsManagementTargetOptions(config.File{}, nil, nil)
	for _, option := range options {
		if option.Value == config.LocalOSCredentialStoreID {
			t.Fatalf("target options include built-in OS store as selectable row: %#v", options)
		}
	}
}

func TestInitSecretsManagementLinearEditorDeleteActionOnlyAppliesToConfiguredProfiles(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"personal": {
					Label:   "1Password",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
				"unused": {
					Label:   "Unused",
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendFile))
	actionIndex := model.document.fieldIndexByID(initSecretsManagementFieldAction)
	if actionIndex < 0 {
		t.Fatal("action field missing")
	}
	for _, option := range model.document[actionIndex].Options {
		if option.Value == initSecretsManagementActionDelete {
			t.Fatalf("create-new target exposes delete action: %#v", model.document[actionIndex].Options)
		}
	}

	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, "personal")
	targetIndex := model.document.fieldIndexByID(initSecretsManagementFieldTarget)
	foundPersonalDeletable := false
	for _, option := range model.document[targetIndex].Options {
		if option.Value == "personal" {
			foundPersonalDeletable = option.Deletable
		}
		if option.Value == config.LocalOSCredentialStoreID && option.Deletable {
			t.Fatalf("built-in OS credential store option is deletable: %#v", option)
		}
	}
	if !foundPersonalDeletable {
		t.Fatalf("target options = %#v, want configured profile to be deletable", model.document[targetIndex].Options)
	}
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, "unused")
	foundDeletable := false
	for _, option := range model.document[targetIndex].Options {
		if option.Value == "unused" {
			foundDeletable = option.Deletable
		}
	}
	if !foundDeletable {
		t.Fatalf("target options = %#v, want configured profile to be deletable", model.document[targetIndex].Options)
	}
}

func TestInitLinearEditorCtrlWDeletesPreviousWord(t *testing.T) {
	const inputField initLinearFieldID = "input"
	var document initLinearDocument
	document.addEditableInput(inputField, "Input", "", "hello brave world", nil)
	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 120, 12)
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlW})

	if got := model.document.fieldValue(inputField); got != "hello brave " {
		t.Fatalf("input after ctrl+w = %q, want previous word deleted", got)
	}
	if got := model.document[model.document.fieldIndexByID(inputField)].Cursor; got != len("hello brave ") {
		t.Fatalf("cursor after ctrl+w = %d, want end of remaining text", got)
	}
}

func TestInitLinearEditorAlignsFieldDescriptionsWithFieldTitles(t *testing.T) {
	var document initLinearDocument
	document.addSection("Section", "Section description")
	document.addEditableInput("input", "Input", "Input description wraps with field title alignment.", "value", nil)
	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 80, 12)

	if !strings.Contains(model.layout.Content, "\nInput\nInput description") {
		t.Fatalf("layout content does not align input description with title:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "\n  Input\n") || strings.Contains(model.layout.Content, "\n  Input description") {
		t.Fatalf("layout content has stale field-title indentation:\n%s", model.layout.Content)
	}
}

func TestInitLinearEditorPreservesExplicitDescriptionNewlines(t *testing.T) {
	var document initLinearDocument
	document.addSection("Status", "first status line\nsecond_key  missing\nthird_key   optional")
	model := newInitLinearEditorModel(initLinearEditor{Document: document}, 80, 12)

	for _, want := range []string{
		"first status line",
		"second_key  missing",
		"third_key   optional",
	} {
		if !strings.Contains(model.layout.Content, want) {
			t.Fatalf("layout content missing %q:\n%s", want, model.layout.Content)
		}
	}
	if strings.Contains(model.layout.Content, "first status line second_key") ||
		strings.Contains(model.layout.Content, "second_key\nmissing") {
		t.Fatalf("layout content collapsed or split explicit description lines:\n%s", model.layout.Content)
	}
}

func TestInitSecretsManagementLinearEditorOnlyFocusedSelectChangesAndShowsCaret(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"work-secrets": {
					Label: "Work secrets",
					Backend: config.SecretsProfileBackend{
						Kind: config.SecretsBackendKind(credstore.BackendFile),
					},
				},
			},
		},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, "work-secrets")
	model = focusInitLinearField(t, model, initSecretsManagementFieldBackend)

	targetBefore := model.document.selectedValue(initSecretsManagementFieldTarget)
	actionBefore := model.document.selectedValue(initSecretsManagementFieldAction)
	model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyDown})

	if got := model.document.selectedValue(initSecretsManagementFieldTarget); got != targetBefore {
		t.Fatalf("target selection = %q, want unchanged %q", got, targetBefore)
	}
	if got := model.document.selectedValue(initSecretsManagementFieldAction); got != actionBefore {
		t.Fatalf("action selection = %q, want unchanged %q", got, actionBefore)
	}
	if got := strings.Count(model.layout.Content, "> "); got != 1 {
		t.Fatalf("caret count = %d, want 1:\n%s", got, model.layout.Content)
	}
	targetLine := -1
	for index, line := range strings.Split(model.layout.Content, "\n") {
		if strings.TrimSpace(line) == "[x] Work secrets (Encrypted file)" {
			targetLine = index
			break
		}
	}
	if targetLine < 0 {
		t.Fatalf("target selected line missing:\n%s", model.layout.Content)
	}
	if !model.layout.SelectedLines[targetLine] {
		t.Fatalf("target selected line is not marked selected; selected lines = %#v\n%s", model.layout.SelectedLines, model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [x] Work secrets (Encrypted file)") {
		t.Fatalf("content missing selected marker on unfocused target row:\n%s", model.layout.Content)
	}
	for _, unfocusedSelected := range []string{
		"> [x] Work secrets (Encrypted file)",
		"> [x] Stage secrets-storage settings",
	} {
		if strings.Contains(model.layout.Content, unfocusedSelected) {
			t.Fatalf("content shows active caret on unfocused selected row %q:\n%s", unfocusedSelected, model.layout.Content)
		}
	}
}

func TestInitSecretsManagementLinearEditorCreateBackendTargetLocksBackend(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOP))

	backendIndex := model.document.fieldIndexByID(initSecretsManagementFieldBackend)
	if backendIndex < 0 {
		t.Fatal("backend field missing")
	}
	backend := model.document[backendIndex]
	if got := model.document.selectedValue(initSecretsManagementFieldBackend); got != string(credstore.BackendOP) {
		t.Fatalf("selected backend = %q, want op", got)
	}
	if !backend.Hidden {
		t.Fatalf("backend field hidden = false, want create-new target to hide redundant backend selector")
	}
	sectionIndex := model.document.fieldIndexByID(initSecretsManagementSectionProfile)
	if sectionIndex < 0 {
		t.Fatal("profile section missing")
	}
	if !strings.Contains(model.document[sectionIndex].Description, "Selected target: Configure new 1Password service account profile") {
		t.Fatalf("profile section description = %q, want selected-target context", model.document[sectionIndex].Description)
	}
}

func TestInitSecretsManagementLinearEditorDesktopTargetSeedsFriendlyLabel(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))

	if got := model.document.fieldValue(initSecretsManagementFieldLabel); got != "1Password" {
		t.Fatalf("profile label = %q, want friendly 1Password default", got)
	}
	if got := model.document.selectedValue(initSecretsManagementFieldBackend); got != string(credstore.BackendOPDesktop) {
		t.Fatalf("selected backend = %q, want op-desktop", got)
	}
	for _, hidden := range []initLinearFieldID{
		initSecretsManagementFieldBackend,
	} {
		index := model.document.fieldIndexByID(hidden)
		if index < 0 {
			t.Fatalf("field %q missing", hidden)
		}
		if !model.document[index].Hidden {
			t.Fatalf("field %q hidden = false, want hidden for create-new desktop profile", hidden)
		}
	}
	out := model.layout.Content
	for _, want := range []string{
		"1Password account URL",
		"1Password account id (advanced)",
		"1Password vault name or id",
		"1Password request timeout",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("desktop content missing %q:\n%s", want, out)
		}
	}
	for _, hiddenText := range []string{
		"Credential store backend",
		"1Password details",
		"Desktop app account and vault routing",
		"Tokens are referenced by environment variable name",
		"1Password item title prefix",
		"1Password secret name",
		"1Password item tag",
	} {
		if strings.Contains(out, hiddenText) {
			t.Fatalf("desktop content includes hidden advanced field %q:\n%s", hiddenText, out)
		}
	}
	if strings.Contains(out, "\n1Password desktop\n") {
		t.Fatalf("desktop content includes redundant section heading:\n%s", out)
	}
	assertContentOrder(t, out, "1Password account URL", "1Password account id (advanced)", "1Password vault name or id", "1Password request timeout")
}

func TestInitOnePasswordDesktopDiscoveryListsAccountsAndVaults(t *testing.T) {
	var calls []string
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch strings.Join(args, " ") {
		case "account list --format=json":
			return []byte(`[
				{"account_uuid":"acct-1","account_name":"SignalFT","url":"signalft.1password.com"},
				{"account_uuid":"acct-2","account_name":"Personal","url":"my.1password.com"}
			]`), nil
		case "vault list --account acct-1 --format=json":
			return []byte(`[{"id":"vault-emp","name":"Employee"}]`), nil
		case "vault list --account acct-2 --format=json":
			return []byte(`[{"id":"vault-personal","name":"Personal"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected op command %q", strings.Join(args, " "))
		}
	}

	discovery := newInitOnePasswordDiscovery(runner).DiscoverDesktop(context.Background())
	if discovery.Err != nil {
		t.Fatalf("DiscoverDesktop error = %v", discovery.Err)
	}
	if !discovery.HasVaultChoices() {
		t.Fatalf("discovery = %#v, want vault choices", discovery)
	}
	selection, ok := discovery.Selection("0:0")
	if !ok {
		t.Fatalf("selection missing from discovery: %#v", discovery)
	}
	if selection.AccountID != "acct-1" || selection.AccountURL != "signalft.1password.com" || selection.VaultID != "vault-emp" || selection.VaultName != "Employee" {
		t.Fatalf("selection = %#v, want account/vault metadata", selection)
	}
	if got, want := calls, []string{
		"op account list --format=json",
		"op vault list --account acct-1 --format=json",
		"op vault list --account acct-2 --format=json",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("op calls = %#v, want %#v", got, want)
	}
}

func TestParseInitOnePasswordVaultsPrioritizesOwnedVaultsThenAlphabetizes(t *testing.T) {
	vaults, err := parseInitOnePasswordVaults([]byte(`[
		{"id":"vault-shared","name":"Shared"},
		{"id":"vault-zeta","name":"Zeta"},
		{"id":"vault-private","name":"Private"},
		{"id":"vault-alpha","name":"Alpha"},
		{"id":"vault-employee","name":"Employee"},
		{"id":"vault-beta","name":"beta"}
	]`))
	if err != nil {
		t.Fatalf("parseInitOnePasswordVaults: %v", err)
	}
	got := make([]string, 0, len(vaults))
	for _, vault := range vaults {
		got = append(got, vault.Name)
	}
	want := []string{"Employee", "Private", "Alpha", "beta", "Shared", "Zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vault names = %#v, want %#v", got, want)
	}
}

func TestInitOnePasswordDesktopDiscoveryUsesFreshTimeoutPerCommand(t *testing.T) {
	var calls []string
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		select {
		case <-time.After(60 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		switch strings.Join(args, " ") {
		case "account list --format=json":
			return []byte(`[
				{"account_uuid":"acct-1","url":"my.1password.com"},
				{"account_uuid":"acct-2","url":"signalft.1password.com"}
			]`), nil
		case "vault list --account acct-1 --format=json":
			return []byte(`[{"id":"vault-personal","name":"Personal"}]`), nil
		case "vault list --account acct-2 --format=json":
			return []byte(`[{"id":"vault-emp","name":"Employee"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected op command %q", strings.Join(args, " "))
		}
	}

	discovery := initOnePasswordDiscovery{run: runner, timeout: 100 * time.Millisecond}.DiscoverDesktop(context.Background())
	if discovery.Err != nil {
		t.Fatalf("DiscoverDesktop error = %v", discovery.Err)
	}
	accounts, vaults := discovery.Counts()
	if accounts != 2 || vaults != 2 {
		t.Fatalf("counts = %d accounts, %d vaults; want 2 accounts, 2 vaults; discovery=%#v", accounts, vaults, discovery)
	}
	selection, ok := discovery.AccountVaultSelection(initOnePasswordDesktopAccountSelectionValue(1), initOnePasswordDesktopVaultSelectionValue(0))
	if !ok {
		t.Fatalf("second account vault selection missing; discovery=%#v", discovery)
	}
	if selection.AccountURL != "signalft.1password.com" || selection.VaultName != "Employee" {
		t.Fatalf("second account selection = %#v, want SignalFT Employee vault", selection)
	}
	if got, want := calls, []string{
		"op account list --format=json",
		"op vault list --account acct-1 --format=json",
		"op vault list --account acct-2 --format=json",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("op calls = %#v, want %#v", got, want)
	}
}

func TestInitOnePasswordDesktopDiscoveryFailureFallsBackToManual(t *testing.T) {
	runner := func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	discovery := newInitOnePasswordDiscovery(runner).DiscoverDesktop(context.Background())
	if discovery.Err == nil {
		t.Fatal("DiscoverDesktop error = nil, want missing-op error")
	}
	if discovery.HasVaultChoices() {
		t.Fatalf("discovery = %#v, want no vault choices after op failure", discovery)
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoverySelectsAccountVault(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{{
		ID:  "acct-1",
		URL: "signalft.1password.com",
		Vaults: []initOnePasswordDiscoveredVault{{
			ID:   "vault-emp",
			Name: "Employee",
		}},
	}}}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	out := model.layout.Content
	for _, want := range []string{
		"1Password account",
		"signalft.1password.com",
		"Account: signalft.1password.com. Choose a vault below:",
		"1Password vault",
		"Employee",
		"Credential store name",
		"1Password-signalft",
		"1Password request timeout",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("desktop discovery content missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "only discovered account") {
		t.Fatalf("desktop discovery content still hides the account selector behind stale copy:\n%s", out)
	}
	for _, hidden := range []string{
		"Desktop app account and vault routing",
		"1Password account URL",
		"1Password account id (advanced)",
		"1Password vault name or id",
	} {
		if strings.Contains(out, hidden) {
			t.Fatalf("desktop discovery content includes manual field %q:\n%s", hidden, out)
		}
	}
	if strings.Contains(out, "\n1Password desktop\n") {
		t.Fatalf("desktop discovery content includes redundant section heading:\n%s", out)
	}

	edit, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, discovery)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocumentWithDiscovery: %v", err)
	}
	profile := edit.Config.Secrets.Stores["1password-signalft"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	got := profile.Backend.OnePassword
	if got.AccountID != "acct-1" || got.AccountURL != "signalft.1password.com" || got.VaultID != "vault-emp" || got.VaultName != "Employee" {
		t.Fatalf("saved onepassword config = %#v, want discovered account/vault metadata", got)
	}
}

func TestInitSecretsManagementLinearEditorCanCreateStoreBeforeReviewProfile(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{{
		ID:  "acct-1",
		URL: "my.1password.com",
		Vaults: []initOnePasswordDiscoveredVault{{
			ID:   "vault-private",
			Name: "Private",
		}},
	}}}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(config.File{}, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 40)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	model = focusInitLinearField(t, model, initSecretsManagementFieldAction)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldAction, initDetailActionEdit)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	if next.document[next.focused].Error != "" {
		t.Fatalf("stage action error = %q", next.document[next.focused].Error)
	}
	if next.resultAction != initDetailActionEdit {
		t.Fatalf("result action = %q, want edit", next.resultAction)
	}
	edit, err := initSecretsManagementEditFromDocumentWithDiscovery(config.File{}, next.document, discovery)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocumentWithDiscovery: %v", err)
	}
	if len(edit.Config.Profiles) != 0 {
		t.Fatalf("edit config unexpectedly added review profile data: %#v", edit.Config)
	}
	profile := edit.Config.Secrets.Stores["1password-personal"]
	if profile.Backend.OnePassword == nil || profile.Backend.OnePassword.VaultName != "Private" {
		t.Fatalf("created credential store = %#v, want private 1Password store", profile)
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoverySelectsAccountThenVault(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{
		{
			ID:   "acct-1",
			Name: "SignalFT",
			URL:  "signalft.1password.com",
			Vaults: []initOnePasswordDiscoveredVault{{
				ID:   "vault-emp",
				Name: "Employee",
			}},
		},
		{
			ID:   "acct-2",
			Name: "Personal",
			URL:  "my.1password.com",
			Vaults: []initOnePasswordDiscoveredVault{{
				ID:   "vault-personal",
				Name: "Personal",
			}},
		},
	}}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 40)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))

	out := model.layout.Content
	for _, want := range []string{
		"1Password account",
		"signalft.1password.com",
		"my.1password.com",
		"1Password vault",
		"Employee",
		"Credential store name",
		"1Password-signalft",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("desktop discovery content missing %q:\n%s", want, out)
		}
	}

	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldDesktopAccount, initOnePasswordDesktopAccountSelectionValue(1))
	out = model.layout.Content
	if !strings.Contains(out, "[x] Personal") {
		t.Fatalf("vault list did not switch to selected account's vaults:\n%s", out)
	}
	if strings.Contains(out, "[x] Employee") {
		t.Fatalf("vault list still selected first account vault after switching accounts:\n%s", out)
	}
	if !strings.Contains(out, "Account: my.1password.com. Choose a vault below:") {
		t.Fatalf("vault description did not switch to selected account:\n%s", out)
	}
	if !strings.Contains(out, "1Password-Personal") {
		t.Fatalf("credential store name did not update for selected account:\n%s", out)
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoveryIncludesAccountWithoutVaultChoices(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{
		{
			ID:   "acct-1",
			Name: "Personal",
			URL:  "my.1password.com",
			Vaults: []initOnePasswordDiscoveredVault{{
				ID:   "vault-personal",
				Name: "Personal",
			}},
		},
		{
			ID:   "acct-2",
			Name: "SignalFT",
			URL:  "signalft.1password.com",
		},
	}}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 40)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))

	out := model.layout.Content
	for _, want := range []string{
		"my.1password.com",
		"signalft.1password.com",
		"Personal",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("desktop discovery content missing account or vault %q:\n%s", want, out)
		}
	}

	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldDesktopAccount, initOnePasswordDesktopAccountSelectionValue(1))
	out = model.layout.Content
	for _, want := range []string{
		"[x] signalft.1password.com",
		"Account: signalft.1password.com. No vaults were discovered; enter a vault manually.",
		"[x] Enter vault manually",
		"1Password vault name or id",
		"1Password-signalft",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("desktop discovery content missing no-vault account behavior %q:\n%s", want, out)
		}
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoveryAllowsManualVaultInSelectedAccount(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{{
		ID:   "acct-1",
		Name: "SignalFT",
		URL:  "signalft.1password.com",
		Vaults: []initOnePasswordDiscoveredVault{{
			ID:   "vault-emp",
			Name: "Employee",
		}},
	}}}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 40)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldDesktopVault, initOnePasswordManualSelection)
	model.setFieldValue(initSecretsManagementFieldVaultID, "ExternalConfidential")

	edit, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, discovery)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocumentWithDiscovery: %v", err)
	}
	profile := edit.Config.Secrets.Stores["1password-signalft"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	got := profile.Backend.OnePassword
	if got.AccountID != "acct-1" || got.AccountURL != "signalft.1password.com" || got.VaultID != "ExternalConfidential" || got.VaultName != "" {
		t.Fatalf("saved onepassword config = %#v, want selected account and manual vault", got)
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoveryAllowsManualAccount(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	discovery := initOnePasswordDesktopDiscovery{Accounts: []initOnePasswordDiscoveredAccount{
		{
			ID:   "acct-1",
			Name: "SignalFT",
			URL:  "signalft.1password.com",
			Vaults: []initOnePasswordDiscoveredVault{{
				ID:   "vault-emp",
				Name: "Employee",
			}},
		},
		{
			ID:   "acct-2",
			Name: "Personal",
			URL:  "my.1password.com",
			Vaults: []initOnePasswordDiscoveredVault{{
				ID:   "vault-personal",
				Name: "Personal",
			}},
		},
	}}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 40)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldDesktopAccount, initOnePasswordManualSelection)
	out := model.layout.Content
	for _, want := range []string{
		"[x] Enter account and vault manually",
		"1Password account URL",
		"1Password account id (advanced)",
		"1Password vault name or id",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manual account content missing %q:\n%s", want, out)
		}
	}
	model.setFieldValue(initSecretsManagementFieldDesktopAccountURL, "example.1password.com")
	model.setFieldValue(initSecretsManagementFieldDesktopAccountID, "acct-manual")
	model.setFieldValue(initSecretsManagementFieldVaultID, "Engineering")

	edit, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, discovery)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocumentWithDiscovery: %v", err)
	}
	profile := edit.Config.Secrets.Stores["1password-signalft"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	got := profile.Backend.OnePassword
	if got.AccountID != "acct-manual" || got.AccountURL != "example.1password.com" || got.VaultID != "Engineering" || got.VaultName != "" {
		t.Fatalf("saved onepassword config = %#v, want manual account and vault", got)
	}
}

func TestHuhInitKeyringBackendPrompterDesktopDiscoveryCreatesProfile(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	t.Setenv("TERM", "dumb")
	callCount := 0
	prompter := huhInitKeyringBackendPrompter{
		stdin:  strings.NewReader(strings.Repeat("\n", 8)),
		stderr: &bytes.Buffer{},
		onePasswordCmdRunner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "account list --format=json":
				return []byte(`[{"account_uuid":"acct-1","account_name":"SignalFT","url":"signalft.1password.com"}]`), nil
			case "vault list --account acct-1 --format=json":
				return []byte(`[{"id":"vault-emp","name":"Employee"}]`), nil
			default:
				return nil, fmt.Errorf("unexpected op command %q", strings.Join(args, " "))
			}
		},
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, _ io.Writer) (initInventoryResult, error) {
			callCount++
			switch callCount {
			case 1:
				return initInventoryResult{
					Action: initInventoryActionCommand,
					Row:    initInventoryRow{ID: initConfigureSecretsProfileSelectionPrefix + string(credstore.BackendOPDesktop)},
				}, nil
			case 2:
				return initInventoryResult{
					Action: initInventoryActionBack,
					Row:    initInventoryRow{ID: initBackSelection},
				}, nil
			default:
				t.Fatalf("unexpected inventory call %d", callCount)
				return initInventoryResult{}, nil
			}
		},
	}

	edit, err := prompter.EditKeyringBackend(initKeyringBackendPrompt{
		Config: config.File{Profiles: map[string]config.Profile{"default": basicProfile("default")}},
	})
	if err != nil {
		t.Fatalf("EditKeyringBackend: %v", err)
	}
	profile := edit.Config.Secrets.Stores["1password"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	got := profile.Backend.OnePassword
	if got.AccountID != "acct-1" || got.AccountURL != "signalft.1password.com" || got.VaultID != "vault-emp" || got.VaultName != "Employee" {
		t.Fatalf("saved onepassword config = %#v, want discovered account/vault metadata", got)
	}
}

func TestInitSecretsManagementLinearEditorDesktopDiscoveryFailureAllowsManualProfile(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	discovery := initOnePasswordDesktopDiscovery{Err: os.ErrNotExist}
	editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, nil, nil, discovery)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	out := model.layout.Content
	for _, want := range []string{
		"1Password account URL",
		"1Password account id (advanced)",
		"1Password vault name or id",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manual desktop content missing %q:\n%s", want, out)
		}
	}
	model.setFieldValue(initSecretsManagementFieldDesktopAccountURL, "signalft.1password.com")
	model.setFieldValue(initSecretsManagementFieldDesktopAccountID, "acct-manual")
	model.setFieldValue(initSecretsManagementFieldVaultID, "Employee")

	edit, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, discovery)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocumentWithDiscovery: %v", err)
	}
	profile := edit.Config.Secrets.Stores["1password"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	got := profile.Backend.OnePassword
	if got.AccountID != "acct-manual" || got.AccountURL != "signalft.1password.com" || got.VaultID != "Employee" || got.VaultName != "" {
		t.Fatalf("saved onepassword config = %#v, want manual account/vault metadata", got)
	}
}

func TestInitSecretsManagementLinearEditorShowsOnePasswordBackendRolloverDescriptions(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	tests := []struct {
		name string
		kind string
		want string
	}{
		{name: "service account", kind: string(credstore.BackendOP), want: "CI or server environments"},
		{name: "connect", kind: string(credstore.BackendOPConnect), want: "Connect API endpoint"},
		{name: "desktop", kind: string(credstore.BackendOPDesktop), want: "Most common for local use"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			editor := initSecretsManagementLinearEditor(cfg)
			model := newInitLinearEditorModel(editor, 180, 32)
			model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(tc.kind))
			index := model.document.fieldIndexByID(initSecretsManagementFieldBackend)
			if index < 0 {
				t.Fatal("backend field missing")
			}
			if !strings.Contains(model.document[index].Description, tc.want) {
				t.Fatalf("backend description = %q, want %q", model.document[index].Description, tc.want)
			}
		})
	}
}

func TestInitSecretsManagementEditStoresOnePasswordVaultNameOrIDAsEntered(t *testing.T) {
	if !initOnePasswordBackendsAvailable() {
		t.Skip("1Password create targets are not selectable in keyring_no1password builds")
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, initConfigureSecretsProfileSelectionPrefix+string(credstore.BackendOPDesktop))
	model.setFieldValue(initSecretsManagementFieldVaultID, "Employee")
	edit, err := initSecretsManagementEditFromDocument(cfg, model.document)
	if err != nil {
		t.Fatalf("initSecretsManagementEditFromDocument: %v", err)
	}
	profile := edit.Config.Secrets.Profiles["1password"]
	if profile.Backend.OnePassword == nil {
		t.Fatal("saved onepassword config = nil")
	}
	if got := profile.Backend.OnePassword.VaultID; got != "Employee" {
		t.Fatalf("saved vault reference = %q, want entered vault name", got)
	}
}

func TestInitSecretsManagementLinearEditorConfiguredProfileKeepsBackendEditable(t *testing.T) {
	cfg := config.File{
		Profiles: map[string]config.Profile{"default": basicProfile("default")},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"work-secrets": {
					Label: "Work secrets",
					Backend: config.SecretsProfileBackend{
						Kind: config.SecretsBackendKind(credstore.BackendFile),
					},
				},
			},
		},
	}
	editor := initSecretsManagementLinearEditor(cfg)
	model := newInitLinearEditorModel(editor, 180, 32)
	model = selectInitLinearFieldValue(t, model, initSecretsManagementFieldTarget, "work-secrets")

	index := model.document.fieldIndexByID(initSecretsManagementFieldBackend)
	if index < 0 {
		t.Fatal("backend field missing")
	}
	if len(model.document[index].Options) <= 1 {
		t.Fatalf("configured profile backend options = %#v, want editable backend choices", model.document[index].Options)
	}
	if strings.Contains(model.document[index].Description, "fixed by the selected create-new target") {
		t.Fatalf("configured profile backend description says locked: %q", model.document[index].Description)
	}
}

func TestInitSecretsProfileBackendFromInputsSwitchingAwayFromOnePasswordClearsBackendFields(t *testing.T) {
	backend := initSecretsProfileBackendFromInputs(initSecretsProfileBackendInput{ // #nosec G101 -- fixture values are account/env-var names, not secret values.
		KindValue:       string(credstore.BackendFile),
		Timeout:         "5s",
		AccountID:       "desktop-account",
		AccountURL:      "signalft.1password.com",
		VaultID:         "vault-123",
		VaultName:       "Employee",
		ConnectHost:     "https://connect.example",
		ConnectTokenEnv: "OP_CONNECT_TOKEN",
		ServiceTokenEnv: "OP_SERVICE_ACCOUNT_TOKEN",
	})
	if backend.Kind != config.SecretsBackendKind(credstore.BackendFile) {
		t.Fatalf("backend kind = %q, want file", backend.Kind)
	}
	if backend.OnePassword != nil {
		t.Fatalf("onepassword config = %#v, want cleared after switching to file backend", backend.OnePassword)
	}
}

func TestBuildInteractiveInitMenuPromptNoWorkspaceDisablesProfileDependentActions(t *testing.T) {
	prompt := buildInteractiveInitMenuPrompt(initSessionDraft{
		cfg: config.File{Profiles: map[string]config.Profile{}},
	})
	if !prompt.CanConfigureLLM || prompt.CanConfigureReviewer || prompt.CanSave {
		t.Fatalf("prompt = %#v, want LLM enabled and reviewer/save disabled without a workspace", prompt)
	}
	if prompt.ReviewProfileCount != 0 || prompt.LLMRuntimeCount != 0 || prompt.ReviewerEntityCount != 0 {
		t.Fatalf("prompt counts = %#v, want zero counts without a workspace", prompt)
	}
}

func TestBuildInteractiveInitMenuPromptNoWorkspaceCanSaveStagedCredentialStore(t *testing.T) {
	original := config.File{Profiles: map[string]config.Profile{}}
	next := cloneInitConfigFile(original)
	next.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"personal-file": {
				DisplayName: "Personal file",
				Backend: config.SecretsStoreBackend{
					Kind: config.SecretsBackendKind(credstore.BackendFile),
				},
			},
		},
	}
	prompt := buildInteractiveInitMenuPrompt(initSessionDraft{
		originalCfg: original,
		cfg:         next,
	})
	if !prompt.CanConfigureLLM || prompt.CanConfigureReviewer {
		t.Fatalf("prompt = %#v, want LLM enabled and reviewer disabled without a workspace", prompt)
	}
	if !prompt.CanSave {
		t.Fatalf("prompt = %#v, want store-only staged changes to enable commit", prompt)
	}
}

func TestBuildInteractiveInitMenuPromptAfterDeletingLastProfileDisablesSaveAndFocusedEditors(t *testing.T) {
	session := initSessionDraft{
		cfg:                          config.File{Profiles: map[string]config.Profile{}},
		pendingProfileDeletes:        map[string]initPendingProfileDelete{"work": {ProfileName: "work"}},
		pendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{},
		pendingLLMRuntimeDeletes:     map[string]initPendingLLMRuntimeDelete{},
	}

	prompt := buildInteractiveInitMenuPrompt(session)
	if !prompt.CanConfigureLLM || prompt.CanConfigureReviewer || prompt.CanSave {
		t.Fatalf("prompt = %#v, want LLM enabled and save/reviewer disabled with zero-profile draft", prompt)
	}
	if prompt.ReviewProfileCount != 0 || prompt.LLMRuntimeCount != 0 || prompt.ReviewerEntityCount != 0 {
		t.Fatalf("prompt counts = %#v, want effective inventory counts at zero after deleting last profile", prompt)
	}
}

func TestValidateInteractiveInitGlobalConfigWithoutProfilesStillValidatesSecretsStores(t *testing.T) {
	err := validateInteractiveInitGlobalConfig(config.File{
		Secrets: config.SecretsConfig{Stores: map[string]config.SecretsStore{
			"broken": {Backend: config.SecretsStoreBackend{Kind: "definitely-not-a-backend"}},
		}},
		Profiles: map[string]config.Profile{},
	})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid for bad credential store without profiles", err)
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
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	}
	prompt := buildInteractiveInitMenuPrompt(initSessionDraft{
		originalCfg: cloneInitConfigFile(cfg),
		cfg:         cfg,
	})
	if !prompt.CanConfigureLLM || prompt.CanConfigureReviewer || prompt.CanSave {
		t.Fatalf("prompt = %#v, want LLM enabled and reviewer/save disabled without active workspace", prompt)
	}
	if prompt.LLMRuntimeCount != 2 || prompt.ReviewerEntityCount != 1 || prompt.ReviewProfileCount != 2 {
		t.Fatalf("prompt counts = %#v, want existing inventory counts from session cfg", prompt)
	}
}

func TestInitMenuItemsOrdersRootMenuAndMovesCountsToDescriptions(t *testing.T) {
	items := initMenuItems(initMenuPrompt{
		HasWorkspace:         true,
		LLMRuntimeCount:      2,
		ReviewerEntityCount:  3,
		ReviewProfileCount:   1,
		CanConfigureLLM:      true,
		CanConfigureReviewer: true,
		CanSave:              true,
	})
	var actions []initMenuAction
	var titles []string
	var descriptions []string
	for _, item := range items {
		actions = append(actions, item.Action)
		titles = append(titles, item.Title)
		descriptions = append(descriptions, item.Description)
		if strings.Contains(item.Title, "(") || strings.Contains(item.Title, ")") {
			t.Fatalf("menu title %q contains old inline count suffix", item.Title)
		}
	}
	wantActions := []initMenuAction{
		initMenuActionSecretsManagement,
		initMenuActionLLMRuntimes,
		initMenuActionReviewerEntities,
		initMenuActionReviewProfiles,
		initMenuActionGlobalSettings,
		initMenuActionSave,
		initMenuActionExit,
	}
	if !reflect.DeepEqual(actions, wantActions) {
		t.Fatalf("actions = %#v, want %#v", actions, wantActions)
	}
	assertContentOrder(t, strings.Join(titles, "\n"),
		"Configure secrets storage",
		"Configure LLM runtimes",
		"Configure reviewer entities",
		"Configure review profiles",
		"Configure global settings",
		"Commit staged changes and exit",
		"Discard staged changes and exit",
	)
	joinedDescriptions := strings.Join(descriptions, "\n")
	for _, want := range []string{
		"2 runtimes configured",
		"3 reviewer entities configured",
		"1 profile configured",
	} {
		if !strings.Contains(joinedDescriptions, want) {
			t.Fatalf("descriptions = %#v, want %q", descriptions, want)
		}
	}
}

func TestInitMenuItemsUseInfrastructureDescriptionsBeforeConfiguredCounts(t *testing.T) {
	items := initMenuItems(initMenuPrompt{
		CanConfigureLLM:      true,
		CanConfigureReviewer: true,
		CanSave:              true,
	})
	descriptions := map[initMenuAction]string{}
	for _, item := range items {
		descriptions[item.Action] = item.Description
	}
	tests := map[initMenuAction]string{
		initMenuActionLLMRuntimes:      "Model providers and runtime credentials",
		initMenuActionReviewerEntities: "Reviewer identities and posting credentials",
		initMenuActionReviewProfiles:   "Repository routing and review composition",
	}
	for action, want := range tests {
		if descriptions[action] != want {
			t.Fatalf("%s description = %q, want %q", action, descriptions[action], want)
		}
	}
}

func TestInitMenuStyledViewShowsRootMenuOrder(t *testing.T) {
	model := newInitMenuModel(initMenuPrompt{
		HasWorkspace:         true,
		ActiveProfileName:    "default",
		LLMRuntimeCount:      2,
		ReviewerEntityCount:  3,
		ReviewProfileCount:   1,
		CanConfigureLLM:      true,
		CanConfigureReviewer: true,
		CanSave:              true,
	})
	out := model.View()
	assertContentOrder(t, out,
		"cr init",
		"Active profile: default",
		"Configure secrets storage",
		"Credential stores for tokens and keys",
		"Configure LLM runtimes",
		"2 runtimes configured",
		"Configure reviewer entities",
		"3 reviewer entities configured",
		"Configure review profiles",
		"1 profile configured",
		"Configure global settings",
		"Data retention and global defaults",
		"Commit staged changes and exit",
		"Write staged config and credential changes",
		"Discard staged changes and exit",
		"Leave without writing staged changes",
	)
	if !strings.Contains(out, ">") {
		t.Fatalf("view missing selected-row caret:\n%s", out)
	}
	if strings.Contains(out, "Configure LLM runtimes (2)") || strings.Contains(out, "Configure review profiles (1)") {
		t.Fatalf("view contains old inline count suffix:\n%s", out)
	}
}

func TestInitMenuQDiscardsAndExits(t *testing.T) {
	model := newInitMenuModel(initMenuPrompt{HasWorkspace: true})
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	result := next.(initMenuModel)
	if cmd == nil {
		t.Fatal("cmd = nil, want quit command")
	}
	if result.result != initMenuActionExit || !result.quitting {
		t.Fatalf("result = %#v, want discard exit", result)
	}
}

func TestRunInitMenuExecutesBubbleTeaPath(t *testing.T) {
	var stderr bytes.Buffer
	action, err := runInitMenu(initMenuPrompt{HasWorkspace: true}, strings.NewReader("q"), &stderr)
	if err != nil {
		t.Fatalf("runInitMenu: %v", err)
	}
	if action != initMenuActionExit {
		t.Fatalf("action = %q, want discard exit", action)
	}
	if strings.Contains(stderr.String(), "Configure LLM runtimes (") {
		t.Fatalf("stderr contains old inline count suffix:\n%s", stderr.String())
	}
}

func TestInitMenuDisabledRowsShowErrorWithoutQuitting(t *testing.T) {
	tests := []struct {
		action initMenuAction
		reason string
	}{
		{
			action: initMenuActionReviewerEntities,
			reason: "configure a review profile before editing reviewer entities",
		},
		{
			action: initMenuActionSave,
			reason: "stage changes before committing",
		},
	}
	for _, tt := range tests {
		model := newInitMenuModel(initMenuPrompt{})
		model.selected = initMenuSelectedIndex(model.items, tt.action)

		next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		result := next.(initMenuModel)

		if cmd != nil {
			t.Fatalf("%s: cmd = %#v, want no quit command", tt.action, cmd)
		}
		if result.quitting || result.result != "" {
			t.Fatalf("%s: result = %#v, want stay in menu without action", tt.action, result)
		}
		if !strings.Contains(result.err, tt.reason) {
			t.Fatalf("%s: err = %q, want %q", tt.action, result.err, tt.reason)
		}
		if !strings.Contains(result.View(), "! "+tt.reason) {
			t.Fatalf("%s: view missing rendered error:\n%s", tt.action, result.View())
		}
		if !strings.Contains(result.View(), "Prerequisite: "+tt.reason) {
			t.Fatalf("%s: view missing prerequisite description:\n%s", tt.action, result.View())
		}
		if strings.Contains(result.View(), "Unavailable:") {
			t.Fatalf("%s: view contains old unavailable wording:\n%s", tt.action, result.View())
		}
	}
}

func TestInitMenuNavigationSkipsDisabledRows(t *testing.T) {
	model := newInitMenuModel(initMenuPrompt{})
	if got := model.items[model.selected].Action; got != initMenuActionReviewProfiles {
		t.Fatalf("initial action = %q, want review profiles", got)
	}

	model.selected = initMenuSelectedIndex(model.items, initMenuActionSecretsManagement)
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	result := next.(initMenuModel)
	if got := result.items[result.selected].Action; got != initMenuActionLLMRuntimes {
		t.Fatalf("after down selected action = %q, want LLM runtimes", got)
	}

	result.selected = initMenuSelectedIndex(result.items, initMenuActionReviewerEntities)
	out := result.View()
	if strings.Contains(out, "\x1b[38;5;42mConfigure reviewer entities") {
		t.Fatalf("disabled selected row rendered with active selected color:\n%s", out)
	}
}

func TestInitMenuUseAccessibleFallback(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if !initMenuUseAccessibleFallback(os.Stdin, os.Stderr) {
		t.Fatal("TERM=dumb should use accessible fallback")
	}
	t.Setenv("TERM", "xterm")
	if !initMenuUseAccessibleFallback(strings.NewReader(""), &bytes.Buffer{}) {
		t.Fatal("non-file test streams should use accessible fallback")
	}
}

func TestInitMenuUseAccessibleFallbackRecognizesTerminalFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test is Unix-only")
	}
	t.Setenv("TERM", "xterm")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	if initMenuUseAccessibleFallback(slave, slave) {
		t.Fatal("PTY-backed terminal files should use Bubble Tea menu")
	}
}

func TestHuhInitMenuPrompterAccessibleShowsMenuEntries(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin:  strings.NewReader("7\n"),
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
		"Configure secrets storage",
		"Configure LLM runtimes",
		"Configure reviewer entities",
		"Configure review profiles",
		"Configure global settings",
		"Commit staged changes and exit",
		"Discard staged changes and exit",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr = %q, want menu item %q", out, want)
		}
	}
	if strings.Count(out, "Configure review profiles") != 1 {
		t.Fatalf("stderr = %q, want exactly one review-profile menu item", out)
	}
	if strings.Contains(out, "Configure review profiles v2") {
		t.Fatalf("stderr = %q, want temporary v2 menu item removed", out)
	}
	if strings.Contains(out, "Configure LLM runtimes (2)") || strings.Contains(out, "Configure reviewer entities (3)") || strings.Contains(out, "Configure review profiles (1)") {
		t.Fatalf("stderr = %q, want fallback labels without old inline count suffixes", out)
	}
	assertContentOrder(t, out,
		"Configure secrets storage",
		"Configure LLM runtimes",
		"Configure reviewer entities",
		"Configure review profiles",
		"Configure global settings",
		"Commit staged changes and exit",
		"Discard staged changes and exit",
	)
}

func TestHuhInitMenuPrompterAccessibleNumericOrder(t *testing.T) {
	t.Setenv("TERM", "dumb")
	tests := []struct {
		input string
		want  initMenuAction
	}{
		{input: "1\n", want: initMenuActionSecretsManagement},
		{input: "2\n", want: initMenuActionLLMRuntimes},
		{input: "3\n", want: initMenuActionReviewerEntities},
		{input: "4\n", want: initMenuActionReviewProfiles},
		{input: "5\n", want: initMenuActionGlobalSettings},
		{input: "6\n", want: initMenuActionSave},
		{input: "7\n", want: initMenuActionExit},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		prompter := huhInitMenuPrompter{
			stdin:  strings.NewReader(tt.input),
			stderr: &stderr,
		}
		action, err := prompter.ChooseAction(initMenuPrompt{
			HasWorkspace:         true,
			LLMRuntimeCount:      2,
			ReviewerEntityCount:  3,
			ReviewProfileCount:   1,
			CanConfigureLLM:      true,
			CanConfigureReviewer: true,
			CanSave:              true,
		})
		if err != nil {
			t.Fatalf("ChooseAction(%q): %v", tt.input, err)
		}
		if action != tt.want {
			t.Fatalf("ChooseAction(%q) = %q, want %q", tt.input, action, tt.want)
		}
	}
}

func TestHuhInitMenuPrompterDefaultStartsAtTopWhenProfileIsActive(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{
		HasWorkspace:         true,
		ActiveProfileName:    "default",
		LLMRuntimeCount:      2,
		ReviewerEntityCount:  3,
		ReviewProfileCount:   1,
		CanConfigureLLM:      true,
		CanConfigureReviewer: true,
		CanSave:              true,
	})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action != initMenuActionSecretsManagement {
		t.Fatalf("action = %q, want secrets management as first active-workspace configuration item", action)
	}
}

func TestInitMenuInitialActionStartsAtSecretsManagementWhenWorkspaceIsActive(t *testing.T) {
	action := initMenuInitialAction(initMenuPrompt{
		HasWorkspace:         true,
		CanConfigureReviewer: true,
	})
	if action != initMenuActionSecretsManagement {
		t.Fatalf("action = %q, want secrets management when workspace is active", action)
	}
}

func TestInitMenuInitialActionStartsAtReviewProfilesBeforeWorkspace(t *testing.T) {
	action := initMenuInitialAction(initMenuPrompt{})
	if action != initMenuActionReviewProfiles {
		t.Fatalf("action = %q, want review profiles before workspace-dependent actions are enabled", action)
	}
}

func TestHuhInitMenuPrompterDefaultStartsAtProfileSetupBeforeWorkspace(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{
		ReviewProfileCount: 1,
	})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action != initMenuActionReviewProfiles {
		t.Fatalf("action = %q, want review profile setup before dependent workflows are enabled", action)
	}
}

func TestHuhInitMenuPrompterAccessibleRejectsDisabledSaveUntilChangesStaged(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"6", // Commit staged changes and exit (disabled)
			"7", // Discard staged changes and exit
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
	if !strings.Contains(stderr.String(), "stage changes before committing") {
		t.Fatalf("stderr = %q, want disabled-save validation message", stderr.String())
	}
}

func TestHuhInitMenuPrompterAccessibleAllowsLLMBeforeProfileExists(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin:  strings.NewReader("2\n"),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action != initMenuActionLLMRuntimes {
		t.Fatalf("action = %q, want LLM runtimes", action)
	}
	if strings.Contains(stderr.String(), "configure a review profile before editing LLM runtimes") {
		t.Fatalf("stderr = %q, want no disabled-LLM validation message", stderr.String())
	}
}

func TestHuhInitMenuPrompterAccessibleRejectsDisabledReviewerUntilProfileExists(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitMenuPrompter{
		stdin: strings.NewReader(strings.Join([]string{
			"3", // Configure reviewer entities (disabled)
			"7", // Discard staged changes and exit
			"",
		}, "\n")),
		stderr: &stderr,
	}
	action, err := prompter.ChooseAction(initMenuPrompt{})
	if err != nil {
		t.Fatalf("ChooseAction: %v", err)
	}
	if action == initMenuActionReviewerEntities {
		t.Fatalf("action = %q, want disabled reviewer selection to be rejected", action)
	}
	if !strings.Contains(stderr.String(), "configure a review profile before editing reviewer entities") {
		t.Fatalf("stderr = %q, want disabled-reviewer validation message", stderr.String())
	}
}

func TestInitProfileV2ReadOnlyContentRendersTargetOrderWithRealData(t *testing.T) {
	profile := basicProfile("open-cli-collective")
	reviewerRef, err := credentials.FormatRef("occ-reviewer")
	if err != nil {
		t.Fatalf("FormatRef: %v", err)
	}
	profile.Git.Host = "github.enterprise"
	profile.Git.AuthMode = config.GitAuthModeGitHubApp
	profile.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/custom-git"}
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:    config.GitAuthModeGitHubApp,
		Credential:  config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: reviewerRef},
		DisplayName: "OCC reviewer",
	}
	profile.LLM.ModelMap = config.ModelMap{
		string(config.ModelTierLarge): "claude-opus-4-7",
	}
	profile.AgentSources = []string{"/opt/codereview/agents"}
	profile.ReviewPolicy = config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsAuto,
		ResolveAfter:     "24h",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"open-cli-collective": profile,
		},
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "open-cli-collective",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		}},
	}
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)

	content, err := initProfileV2ReadOnlyContent(initPromptContext{
		RequestedProfileName:    "open-cli-collective",
		ExistingProfileName:     "open-cli-collective",
		ExistingProfile:         &profile,
		ExistingProfileNames:    []string{"open-cli-collective"},
		ExistingConfig:          cfg,
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: profileReviewerEntities,
		LLMRuntimes:             llmRuntimes,
		ProfileLLMRuntimes:      profileLLMRuntimes,
	}, "open-cli-collective")
	if err != nil {
		t.Fatalf("initProfileV2ReadOnlyContent: %v", err)
	}

	for _, want := range []string{
		"Profile name",
		"> open-cli-collective",
		"Automatic profile selection",
		"github.com/open-cli-collective",
		"Git scope",
		"Git scope host",
		"github.enterprise",
		"Reviewer entity",
		"OCC reviewer (GitHub App reviewer)",
		"LLM runtime",
		"Configured: Claude CLI subscription",
		"Minimum reviewer model tier",
		"Model tier mapping",
		"large model",
		"claude-opus-4-7",
		"Additional reviewer-agent directories (optional)",
		"/opt/codereview/agents",
		"Review Policy",
		"Request changes",
		"Enable self-approve",
		"Auto-resolve",
		"24h",
		"Git credentials",
		"Git credential store",
		initBuiltInOSCredentialStoreTitle(),
		"Git credential name",
		"codereview/custom-git",
		"Profile action",
		"Stage profile settings",
		"Back without staging",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q:\n%s", want, content)
		}
	}

	assertContentOrder := func(parts ...string) {
		t.Helper()
		previous := -1
		for _, part := range parts {
			index := strings.Index(content, part)
			if index < 0 {
				t.Fatalf("content missing %q:\n%s", part, content)
			}
			if index <= previous {
				t.Fatalf("content order wrong for %q:\n%s", part, content)
			}
			previous = index
		}
	}
	assertContentOrder(
		"Profile name",
		"Automatic profile selection",
		"Route entries",
		"Git scope",
		"Reviewer entity",
		"LLM runtime",
		"Minimum reviewer model tier",
		"Model tier mapping",
		"Additional reviewer-agent directories (optional)",
		"Review Policy",
		"Git credentials",
		"Profile action",
	)
}

func TestInitProfileV2ReadOnlyModelFocusNavigationPreservesRouteGuidance(t *testing.T) {
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addInput("Profile name", "", "open-cli-collective")
	initProfileV2AppendRouteSection(&document, "github.com/open-cli-collective")
	initProfileV2AddSelect(&document, "Reviewer entity", "Choose who posts review events.", []huh.Option[string]{
		huh.NewOption("Post using this profile's Git account (GitHub PAT)", "profile"),
	}, "profile")

	routeIndex := document.fieldIndexByTitle("Route entries")
	if routeIndex < 0 {
		t.Fatal("Route entries field missing")
	}

	for _, tc := range []struct {
		name string
		key  tea.KeyType
	}{
		{name: "enter", key: tea.KeyEnter},
		{name: "tab", key: tea.KeyTab},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, 240, 24)
			model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tc.key})

			if model.focused != routeIndex {
				t.Fatalf("focused index = %d, want route entries index %d", model.focused, routeIndex)
			}
			if model.viewport.YOffset != 0 {
				t.Fatalf("viewport YOffset = %d, want unchanged top while route entries are already visible", model.viewport.YOffset)
			}
			for _, want := range []string{
				"Automatic profile selection",
				"Accepted route formats",
				"Route entries",
				"github.com/open-cli-collective",
			} {
				if !strings.Contains(model.View(), want) {
					t.Fatalf("view missing %q after focusing route entries:\n%s", want, model.View())
				}
			}
			if strings.Contains(model.View(), "> Route entries") {
				t.Fatalf("view still uses caret on focused title instead of active rail:\n%s", model.View())
			}
		})
	}
}

func TestInitProfileV2ArrowKeysChangeSelectNotFocus(t *testing.T) {
	const choiceField initProfileV2FieldID = "choice"
	const inputField initProfileV2FieldID = "input"
	var document initProfileV2Document
	document.addEditableSelect(choiceField, "Choice", "", []huh.Option[string]{
		huh.NewOption("Alpha", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")
	document.addEditableInput(inputField, "Input", "", "value", nil)
	model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, 120, 12)
	choiceIndex := model.document.fieldIndexByID(choiceField)

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if got := model.document.selectedValue(choiceField); got != "beta" {
		t.Fatalf("selected value after down = %q, want beta", got)
	}
	if model.focused != choiceIndex {
		t.Fatalf("focused after select down = %d, want unchanged choice index %d", model.focused, choiceIndex)
	}

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyUp})
	if got := model.document.selectedValue(choiceField); got != "alpha" {
		t.Fatalf("selected value after up = %q, want alpha", got)
	}
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
	inputIndex := model.document.fieldIndexByID(inputField)
	if model.focused != inputIndex {
		t.Fatalf("focused after tab = %d, want input index %d", model.focused, inputIndex)
	}
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if model.focused != inputIndex {
		t.Fatalf("focused after input down = %d, want unchanged input index %d", model.focused, inputIndex)
	}
}

func TestInitProfileV2ArrowKeysDoNotScrollFocusedInput(t *testing.T) {
	const inputField initProfileV2FieldID = "input"
	var document initProfileV2Document
	document.addEditableInput(inputField, "Input", "", "value", nil)
	for i := 0; i < 20; i++ {
		document.addSection(fmt.Sprintf("Section %02d", i), "Context line")
	}
	model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, 120, 5)
	model.viewport.SetYOffset(1)

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if got := model.viewport.YOffset; got != 1 {
		t.Fatalf("viewport YOffset after input down = %d, want unchanged 1", got)
	}
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyUp})
	if got := model.viewport.YOffset; got != 1 {
		t.Fatalf("viewport YOffset after input up = %d, want unchanged 1", got)
	}
}

func TestInitProfileV2OnlyFocusedSelectedFieldShowsCaret(t *testing.T) {
	const firstField initProfileV2FieldID = "first"
	const secondField initProfileV2FieldID = "second"
	var document initProfileV2Document
	document.addEditableSelect(firstField, "First", "", []huh.Option[string]{
		huh.NewOption("Alpha", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")
	document.addEditableSelect(secondField, "Second", "", []huh.Option[string]{
		huh.NewOption("Gamma", "gamma"),
		huh.NewOption("Delta", "delta"),
	}, "delta")

	model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, 120, 12)
	if got := strings.Count(model.layout.Content, "> "); got != 1 {
		t.Fatalf("initial caret count = %d, want 1:\n%s", got, model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "> [x] Alpha") {
		t.Fatalf("initial content missing focused selected option:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [x] Delta") {
		t.Fatalf("initial content missing unfocused selected option marker:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [ ] Beta") {
		t.Fatalf("initial content missing unselected option marker:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "> [x] Delta") {
		t.Fatalf("initial content shows caret on unfocused selected option:\n%s", model.layout.Content)
	}

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
	if got := strings.Count(model.layout.Content, "> "); got != 1 {
		t.Fatalf("caret count after tab = %d, want 1:\n%s", got, model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "> [x] Delta") {
		t.Fatalf("content after tab missing focused selected option:\n%s", model.layout.Content)
	}
	if !strings.Contains(model.layout.Content, "  [x] Alpha") {
		t.Fatalf("content after tab missing unfocused selected option marker:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "> [x] Alpha") {
		t.Fatalf("content after tab shows caret on unfocused selected option:\n%s", model.layout.Content)
	}
}

func TestInitProfileV2SelectMarkerWrapsUnfocusedSelectedOption(t *testing.T) {
	const inputField initProfileV2FieldID = "input"
	const choiceField initProfileV2FieldID = "choice"
	const width = 24
	var document initProfileV2Document
	document.addEditableInput(inputField, "Input", "", "value", nil)
	document.addEditableSelect(choiceField, "Choice", "", []huh.Option[string]{
		huh.NewOption("Alpha selected option wraps cleanly", "alpha"),
		huh.NewOption("Beta", "beta"),
	}, "alpha")

	model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, width, 12)
	want := "  [x] Alpha selected\n      option wraps\n      cleanly"
	if !strings.Contains(model.layout.Content, want) {
		t.Fatalf("wrapped unfocused selected option missing marker or aligned continuations:\n%s", model.layout.Content)
	}
	if strings.Contains(model.layout.Content, "> [x] Alpha selected") {
		t.Fatalf("wrapped unfocused selected option shows caret:\n%s", model.layout.Content)
	}
	for _, line := range strings.Split(model.layout.Content, "\n") {
		if len(line) > width {
			t.Fatalf("wrapped line length = %d, want <= %d for %q\n%s", len(line), width, line, model.layout.Content)
		}
	}
}

func TestInitProfileV2LayoutWrapsAndMeasuresSmallViewport(t *testing.T) {
	var document initProfileV2Document
	document.addSection("Profile", "This section has enough words to wrap across multiple lines in a narrow terminal.")
	document.addInput("Profile name", "Short field that should remain measurable.", "monit")
	document.addInput("Route entries", "Routes tell cr when to use this profile automatically in a narrow viewport.", "github.com/SignalFT")
	document.addInput("Git credential name", "Full credential name under the selected store.", "codereview/monit")

	layout := initProfileV2LayoutDocument(document, 32, document.firstFocusableField())
	if len(layout.Bounds) != len(document) {
		t.Fatalf("bounds count = %d, want %d", len(layout.Bounds), len(document))
	}
	if layout.Lines <= len(document) {
		t.Fatalf("layout lines = %d, want wrapped content larger than document length %d", layout.Lines, len(document))
	}
	for _, line := range strings.Split(layout.Content, "\n") {
		if len(line) > 32 {
			t.Fatalf("line length = %d, want <= 32 for %q\n%s", len(line), line, layout.Content)
		}
	}
	for index, bounds := range layout.Bounds {
		if bounds.Start < 0 || bounds.End <= bounds.Start || bounds.End > layout.Lines {
			t.Fatalf("bounds[%d] = %#v outside layout with %d lines", index, bounds, layout.Lines)
		}
	}

	model := newInitProfileV2ReadOnlyModel(initProfileV2Editor{Document: document}, 32, 6)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyEnd})
	bounds := model.layout.Bounds[model.focused]
	if bounds.Start < model.viewport.YOffset || bounds.Start >= model.viewport.YOffset+model.viewport.Height {
		t.Fatalf("focused field start line %d not visible in viewport [%d,%d)", bounds.Start, model.viewport.YOffset, model.viewport.YOffset+model.viewport.Height)
	}
}

func TestInitProfileV2TextInputsDraftProfileNameAndRoutes(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2Editor("monit", "github.com/SignalFT; github.com/OtherMonitOrg"), 160, 24)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "monit-next")

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if draft.ProfileName != "monit-next" {
		t.Fatalf("ProfileName = %q, want monit-next", draft.ProfileName)
	}
	if !draft.RoutesSet {
		t.Fatal("RoutesSet = false, want true")
	}
	wantRoutes := []configedit.RepositoryRouteSpec{
		{Host: "github.com", Namespace: "SignalFT"},
		{Host: "github.com", Namespace: "OtherMonitOrg"},
	}
	if !reflect.DeepEqual(draft.Routes, wantRoutes) {
		t.Fatalf("Routes = %#v, want %#v", draft.Routes, wantRoutes)
	}
}

func TestInitProfileV2TextInputsClearRoutes(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2Editor("monit", "github.com/SignalFT"), 160, 24)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if !draft.RoutesSet {
		t.Fatal("RoutesSet = false, want true")
	}
	if len(draft.Routes) != 0 {
		t.Fatalf("Routes = %#v, want cleared routes", draft.Routes)
	}
}

func TestInitProfileV2TextInputsShowLocalErrors(t *testing.T) {
	t.Run("profile name", func(t *testing.T) {
		model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2Editor("monit", "github.com/SignalFT"), 160, 24)
		model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})

		if !strings.Contains(model.View(), "profile name is required") {
			t.Fatalf("view missing profile name validation error:\n%s", model.View())
		}
		if _, err := model.validatedDraft(); err == nil || !strings.Contains(err.Error(), "profile name is required") {
			t.Fatalf("validatedDraft error = %v, want profile name validation", err)
		}
	})

	t.Run("routes", func(t *testing.T) {
		model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2Editor("monit", "github.com/SignalFT"), 160, 24)
		model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyTab})
		model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
		model = typeInitProfileV2Text(t, model, "not-a-route")

		if !strings.Contains(model.View(), "must be host/namespace") {
			t.Fatalf("view missing route validation error:\n%s", model.View())
		}
		if _, err := model.validatedDraft(); err == nil || !strings.Contains(err.Error(), "must be host/namespace") {
			t.Fatalf("validatedDraft error = %v, want route validation", err)
		}
	})
}

func TestInitProfileV2GitScopeDefaultPathHidesCustomControls(t *testing.T) {
	profile := basicProfile("monit")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"monit": profile,
		},
	}
	gitScopes, profileGitScopes := buildInitGitScopeInventory(cfg)
	reviewerEntities, profileReviewerEntities := buildInitReviewerEntityInventory(cfg)
	llmRuntimes, profileLLMRuntimes := buildInitLLMRuntimeInventory(cfg)

	content, err := initProfileV2ReadOnlyContent(initPromptContext{
		RequestedProfileName:    "monit",
		ExistingProfileName:     "monit",
		ExistingProfile:         &profile,
		ExistingProfileNames:    []string{"monit"},
		ExistingConfig:          cfg,
		GitScopes:               gitScopes,
		ProfileGitScopes:        profileGitScopes,
		ReviewerEntities:        reviewerEntities,
		ProfileReviewerEntities: profileReviewerEntities,
		LLMRuntimes:             llmRuntimes,
		ProfileLLMRuntimes:      profileLLMRuntimes,
	}, "monit")
	if err != nil {
		t.Fatalf("initProfileV2ReadOnlyContent: %v", err)
	}
	for _, absent := range []string{
		"Choose an existing Git scope",
		"Git scope host",
		"Git scope auth mode",
	} {
		if strings.Contains(content, absent) {
			t.Fatalf("content contains %q, want ordinary single-scope profile to keep Git scope controls hidden:\n%s", absent, content)
		}
	}
}

func TestInitProfileV2GitScopePreservesSelectedScopeInDraft(t *testing.T) {
	gitScopes := map[string]initGitScopeDraft{
		"gitlab-work": {
			Host:          "gitlab.com",
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: "codereview/gitlab-work",
		},
		"github-work": {
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/github-work",
		},
	}
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithGitScope(
		"monit",
		"gitlab.com/SignalFT",
		"gitlab-work",
		gitScopes,
		initDraft{
			OriginalProfileName: "monit",
			ProfileName:         "monit",
			GitHost:             "github.com",
			GitAuth:             string(config.GitAuthModePAT),
		},
	), 160, 24)

	if strings.Contains(model.View(), "Git scope host") {
		t.Fatalf("view contains custom Git host controls for selected shared scope:\n%s", model.View())
	}
	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if draft.GitHost != "gitlab.com" || draft.GitAuth != string(config.GitAuthModeGitHubApp) || draft.GitCredentialRef != "codereview/gitlab-work" {
		t.Fatalf("draft Git = (%q,%q,%q), want selected gitlab scope", draft.GitHost, draft.GitAuth, draft.GitCredentialRef)
	}
}

func TestInitProfileV2GitScopeCustomEditsDraft(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithGitScope(
		"monit",
		"gitlab.com/SignalFT",
		initCustomGitScopeSelection,
		nil,
		initDraft{
			OriginalProfileName: "monit",
			ProfileName:         "monit",
			GitHost:             "github.enterprise",
			GitAuth:             string(config.GitAuthModeGitHubApp),
		},
	), 160, 24)

	model = focusInitProfileV2Field(t, model, initProfileV2FieldGitHost)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "gitlab.com")
	model = focusInitProfileV2Field(t, model, initProfileV2FieldGitAuth)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyUp})

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if draft.GitHost != "gitlab.com" || draft.GitAuth != string(config.GitAuthModePAT) {
		t.Fatalf("draft Git = (%q,%q), want edited custom Git host/auth", draft.GitHost, draft.GitAuth)
	}
}

func TestInitProfileV2GitScopeRejectsRoutesForDifferentHost(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithGitScope(
		"monit",
		"github.com/SignalFT",
		initCustomGitScopeSelection,
		nil,
		initDraft{
			OriginalProfileName: "monit",
			ProfileName:         "monit",
			GitHost:             "gitlab.com",
			GitAuth:             string(config.GitAuthModePAT),
		},
	), 160, 24)

	_, err := model.validatedDraft()
	if err == nil || !strings.Contains(err.Error(), `route host "github.com" does not match selected profile host "gitlab.com"`) {
		t.Fatalf("validatedDraft error = %v, want route host mismatch", err)
	}
}

func TestInitProfileV2SelectsDraftReviewerRuntimeAndModelTier(t *testing.T) {
	// #nosec G101 -- test fixture credential reference, not a secret.
	reviewerEntities := map[string]initReviewerEntityDraft{
		"app-reviewer": {
			Kind:            initReviewerEntityKindGitHubApp,
			AuthMode:        config.GitAuthModeGitHubApp,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/app-reviewer",
			DisplayName:     "enterprise/reviewer-bot",
		},
	}
	llmRuntimes := map[string]initLLMRuntimeDraft{
		"claude-work": {
			Preset:   initLLMRuntimePresetClaudeCLISubscription,
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		"openai-work": {
			Preset:          initLLMRuntimePresetOpenAIAPIKey,
			Provider:        config.LLMProviderOpenAI,
			Auth:            config.LLMAuthAPIKey,
			Adapter:         config.LLMAdapterOpenAIAPI,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/openai-work",
		},
	}
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithSelections("monit", "github.com/SignalFT", reviewerEntities, llmRuntimes), 160, 24)
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldReviewerEntity, "app-reviewer")
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldLLMRuntime, "openai-work")
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldReviewerModelTier, string(config.ModelTierMedium))

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if !draft.ReviewerEnabled || draft.ReviewerAuth != string(config.GitAuthModeGitHubApp) || draft.ReviewerCredentialRef != "codereview/app-reviewer" || draft.ReviewerDisplayName != "enterprise/reviewer-bot" {
		t.Fatalf("draft reviewer = (enabled:%t auth:%q ref:%q name:%q), want selected GitHub App reviewer", draft.ReviewerEnabled, draft.ReviewerAuth, draft.ReviewerCredentialRef, draft.ReviewerDisplayName)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAuth != string(config.LLMAuthAPIKey) || draft.LLMAdapter != string(config.LLMAdapterOpenAIAPI) || draft.LLMCredentialRef != "codereview/openai-work" {
		t.Fatalf("draft LLM = (%q,%q,%q,%q), want selected OpenAI API runtime", draft.LLMProvider, draft.LLMAuth, draft.LLMAdapter, draft.LLMCredentialRef)
	}
	if draft.LLMReviewerModelTier != string(config.ModelTierMedium) {
		t.Fatalf("draft.LLMReviewerModelTier = %q, want medium", draft.LLMReviewerModelTier)
	}
}

func TestInitProfileV2NoRuntimeBootstrapRequestsExistingFlow(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithSelections("monit", "github.com/SignalFT", nil, nil), 160, 40)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldLLMRuntime)
	if !strings.Contains(model.View(), "Configure a new LLM runtime first") {
		t.Fatalf("view missing no-runtime bootstrap option:\n%s", model.View())
	}

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initProfileV2ReadOnlyModel)
	if !ok {
		t.Fatalf("Update returned %T, want initProfileV2ReadOnlyModel", updated)
	}
	if !next.requestLLMRuntimeBootstrap {
		t.Fatal("requestLLMRuntimeBootstrap = false, want existing runtime flow request")
	}
	if cmd == nil {
		t.Fatal("Update returned nil command, want quit command for runtime bootstrap handoff")
	}
	if _, err := next.validatedDraft(); err == nil || !strings.Contains(err.Error(), "configure a new LLM runtime first") {
		t.Fatalf("validatedDraft error = %v, want no-runtime guard", err)
	}
}

func TestInitProfileV2ModelMapInputsDraftOverridesAndClears(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithModelMap(
		"monit",
		"github.com/SignalFT",
		config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		config.ModelMap{
			string(config.ModelTierLarge): "claude-opus-4-7",
		},
	), 160, 40)

	model = focusInitProfileV2Field(t, model, initProfileV2FieldModelMap(config.ModelTierSmall))
	if !strings.Contains(model.View(), "> |") {
		t.Fatalf("view missing editable cursor for empty small model field:\n%s", model.View())
	}
	model = typeInitProfileV2Text(t, model, "claude-haiku-custom")
	model = focusInitProfileV2Field(t, model, initProfileV2FieldModelMap(config.ModelTierMedium))
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = focusInitProfileV2Field(t, model, initProfileV2FieldModelMap(config.ModelTierLarge))
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if !draft.ModelMapSet {
		t.Fatal("draft.ModelMapSet = false, want model map edits staged")
	}
	want := config.ModelMap{
		string(config.ModelTierSmall): "claude-haiku-custom",
	}
	if !reflect.DeepEqual(draft.ModelMap, want) {
		t.Fatalf("draft.ModelMap = %#v, want %#v", draft.ModelMap, want)
	}
}

func TestInitProfileV2LLMRuntimeSelectionRefreshesModelMapFields(t *testing.T) {
	llmRuntimes := map[string]initLLMRuntimeDraft{
		"claude-work": {
			Preset:   initLLMRuntimePresetClaudeCLISubscription,
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		"openai-work": {
			Preset:   initLLMRuntimePresetOpenAIAPIKey,
			Provider: config.LLMProviderOpenAI,
			Auth:     config.LLMAuthAPIKey,
			Adapter:  config.LLMAdapterOpenAIAPI,
		},
	}
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithRuntimeAndModelMap("monit", "github.com/SignalFT", llmRuntimes, "claude-work"), 160, 24)
	if got := model.document.fieldValue(initProfileV2FieldModelMap(config.ModelTierSmall)); got != "" {
		t.Fatalf("initial small model = %q, want unmapped Claude small model", got)
	}
	if got := model.document.fieldValue(initProfileV2FieldModelMap(config.ModelTierMedium)); got != "claude-sonnet-4-6" {
		t.Fatalf("initial medium model = %q, want Claude built-in", got)
	}

	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldLLMRuntime, "openai-work")

	if got := model.document.fieldValue(initProfileV2FieldModelMap(config.ModelTierSmall)); got != "gpt-5.4-mini" {
		t.Fatalf("small model after runtime change = %q, want OpenAI built-in", got)
	}
	if got := model.document.fieldValue(initProfileV2FieldModelMap(config.ModelTierMedium)); got != "gpt-5.4" {
		t.Fatalf("medium model after runtime change = %q, want OpenAI built-in", got)
	}
	smallIndex := model.document.fieldIndexByID(initProfileV2FieldModelMap(config.ModelTierSmall))
	if smallIndex < 0 || !strings.Contains(model.document[smallIndex].Description, "Built-in small model for this runtime: gpt-5.4-mini.") {
		t.Fatalf("small model description after runtime change = %q", model.document[smallIndex].Description)
	}
}

func TestInitProfileV2AgentSourcesTextareaDraftsNormalizedSources(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithAgentSources("monit", "github.com/SignalFT", []string{"/tmp/agents-old"}), 160, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldAgentSources)
	if !strings.Contains(model.View(), "ctrl+j newline") {
		t.Fatalf("view missing textarea newline help:\n%s", model.View())
	}

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "/tmp/agents-alpha")
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlJ})
	model = typeInitProfileV2Text(t, model, "/tmp/agents-alpha/../agents-alpha")
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlJ})
	model = typeInitProfileV2Text(t, model, " ./agents-beta ")

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if !draft.AgentSourcesSet {
		t.Fatal("draft.AgentSourcesSet = false, want agent-source edits staged")
	}
	want := []string{"/tmp/agents-alpha", "agents-beta"}
	if !reflect.DeepEqual(draft.AgentSources, want) {
		t.Fatalf("draft.AgentSources = %#v, want %#v", draft.AgentSources, want)
	}
}

func TestInitProfileV2AgentSourcesEnterMovesFocusWithoutDestroyingNavigation(t *testing.T) {
	editor := newTestInitProfileV2EditorWithAgentSources("monit", "github.com/SignalFT", nil)
	editor.Document.addEditableInput(initProfileV2FieldID("after_agent_sources"), "After agent sources", "", "next", nil)
	model := newInitProfileV2ReadOnlyModel(editor, 48, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldAgentSources)
	model = typeInitProfileV2Text(t, model, strings.Repeat("/tmp/very-long-agent-source-path/", 5))

	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyEnter})

	if got := model.document[model.focused].Title; got != "After agent sources" {
		t.Fatalf("focused field = %q, want next field after textarea", got)
	}
	if !strings.Contains(model.layout.Content, "After agent sources") {
		t.Fatalf("layout missing next field after leaving long textarea:\n%s", model.layout.Content)
	}
}

func TestInitProfileV2ReviewPolicyDraftsSelections(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(
		"monit",
		"github.com/SignalFT",
		config.ReviewPolicy{},
		"codereview/monit",
		true,
		nil,
		initCustomGitScopeSelection,
	), 160, 40)
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldReviewMajorEvent, string(config.ReviewMajorEventRequestChanges))
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldSelfApprove, initSelfApproveEnable)
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldResolveThreads, string(config.ResolveThreadsAuto))
	model = focusInitProfileV2Field(t, model, initProfileV2FieldResolveAfter)
	model = typeInitProfileV2Text(t, model, "24h")

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if !draft.ReviewPolicySet {
		t.Fatal("draft.ReviewPolicySet = false, want review-policy edits staged")
	}
	want := config.ReviewPolicy{
		MajorEvent:       config.ReviewMajorEventRequestChanges,
		AllowSelfApprove: true,
		ResolveThreads:   config.ResolveThreadsAuto,
		ResolveAfter:     "24h",
	}
	if !reflect.DeepEqual(draft.ReviewPolicy, want) {
		t.Fatalf("draft.ReviewPolicy = %#v, want %#v", draft.ReviewPolicy, want)
	}
}

func TestInitProfileV2ReviewPolicyRejectsInvalidDuration(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(
		"monit",
		"github.com/SignalFT",
		config.ReviewPolicy{},
		"codereview/monit",
		true,
		nil,
		initCustomGitScopeSelection,
	), 160, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldResolveAfter)
	model = typeInitProfileV2Text(t, model, "tomorrow")

	if !strings.Contains(model.View(), "invalid duration") {
		index := model.document.fieldIndexByID(initProfileV2FieldResolveAfter)
		if index < 0 || !strings.Contains(model.document[index].Error, "invalid duration") {
			t.Fatalf("duration field error = %q, want invalid duration", model.document[index].Error)
		}
	}
	if _, err := model.validatedDraft(); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("validatedDraft error = %v, want duration validation", err)
	}
}

func TestInitProfileV2GitCredentialNameDraftsCustomName(t *testing.T) {
	gitScopes := map[string]initGitScopeDraft{
		"github-work": {
			Host:            "github.com",
			AuthMode:        config.GitAuthModePAT,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/monit",
		},
	}
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(
		"monit",
		"github.com/SignalFT",
		config.ReviewPolicy{},
		"codereview/monit",
		true,
		gitScopes,
		"github-work",
	), 160, 40)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldGitCredentialName)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "codereview/custom-monit-git")

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if draft.GitCredentialRef != "codereview/custom-monit-git" {
		t.Fatalf("draft.GitCredentialRef = %q, want custom credential name", draft.GitCredentialRef)
	}
	if draft.GitCredentialStore != config.LocalOSCredentialStoreID {
		t.Fatalf("draft.GitCredentialStore = %q, want local-os", draft.GitCredentialStore)
	}
}

func TestInitProfileV2GitCredentialNameRejectsInvalidCredentialRef(t *testing.T) {
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(
		"monit",
		"github.com/SignalFT",
		config.ReviewPolicy{},
		"codereview/monit",
		true,
		nil,
		initCustomGitScopeSelection,
	), 160, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldGitCredentialName)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "not-a-ref")

	if !strings.Contains(model.View(), "credential ref") {
		index := model.document.fieldIndexByID(initProfileV2FieldGitCredentialName)
		if index < 0 || !strings.Contains(model.document[index].Error, "credential ref") {
			t.Fatalf("git credential name field error = %q, want credential-ref validation", model.document[index].Error)
		}
	}
	if _, err := model.validatedDraft(); err == nil {
		t.Fatal("validatedDraft error = nil, want credential-ref validation")
	}
}

func TestInitProfileV2GitCredentialNameFollowsChangedScopeDefaultWhenUnedited(t *testing.T) {
	gitScopes := map[string]initGitScopeDraft{
		"old-git": {
			Host:            "github.com",
			AuthMode:        config.GitAuthModePAT,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/old-git",
		},
		"new-git": {
			Host:            "github.com",
			AuthMode:        config.GitAuthModePAT,
			CredentialStore: config.LocalOSCredentialStoreID,
			CredentialRef:   "codereview/new-git",
		},
	}
	model := newInitProfileV2ReadOnlyModel(newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(
		"monit",
		"github.com/SignalFT",
		config.ReviewPolicy{},
		"codereview/old-git",
		true,
		gitScopes,
		"old-git",
	), 160, 24)
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldGitScope, "new-git")

	draft, err := model.validatedDraft()
	if err != nil {
		t.Fatalf("validatedDraft: %v", err)
	}
	if draft.GitCredentialRef != "codereview/new-git" {
		t.Fatalf("draft.GitCredentialRef = %q, want changed Git scope default", draft.GitCredentialRef)
	}
}

func TestInitProfileV2ProfileActionStagesValidatedDraft(t *testing.T) {
	editor := newTestInitProfileV2Editor("monit", "github.com/SignalFT")
	initProfileV2AppendProfileActionSection(&editor.Document)
	model := newInitProfileV2ReadOnlyModel(editor, 160, 24)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "monit-next")
	model = focusInitProfileV2Field(t, model, initProfileV2FieldProfileAction)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initProfileV2ReadOnlyModel)
	if !ok {
		t.Fatalf("Update returned %T, want initProfileV2ReadOnlyModel", updated)
	}
	if cmd == nil {
		t.Fatal("Update returned nil command, want quit command after staging")
	}
	if !next.result.StageProfile {
		t.Fatalf("StageProfile = false, result = %#v", next.result)
	}
	if next.result.Draft.ProfileName != "monit-next" {
		t.Fatalf("staged ProfileName = %q, want monit-next", next.result.Draft.ProfileName)
	}
	if !next.result.Draft.RoutesSet || len(next.result.Draft.Routes) != 1 {
		t.Fatalf("staged routes = (%t,%#v), want one staged route", next.result.Draft.RoutesSet, next.result.Draft.Routes)
	}
}

func TestInitProfileV2ProfileActionKeepsEditorOpenOnValidationError(t *testing.T) {
	editor := newTestInitProfileV2Editor("monit", "github.com/SignalFT")
	initProfileV2AppendProfileActionSection(&editor.Document)
	model := newInitProfileV2ReadOnlyModel(editor, 160, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldRoutes)
	model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyCtrlU})
	model = typeInitProfileV2Text(t, model, "not-a-route")
	model = focusInitProfileV2Field(t, model, initProfileV2FieldProfileAction)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initProfileV2ReadOnlyModel)
	if !ok {
		t.Fatalf("Update returned %T, want initProfileV2ReadOnlyModel", updated)
	}
	if cmd != nil {
		t.Fatal("Update returned quit command, want editor to stay open on validation error")
	}
	if next.result.StageProfile {
		t.Fatal("StageProfile = true, want validation to block staging")
	}
	actionIndex := next.document.fieldIndexByID(initProfileV2FieldProfileAction)
	if actionIndex < 0 || !strings.Contains(next.document[actionIndex].Error, "must be host/namespace") {
		t.Fatalf("profile action error = %q, want route validation", next.document[actionIndex].Error)
	}
}

func TestInitProfileV2ProfileActionBackReturnsWithoutStaging(t *testing.T) {
	editor := newTestInitProfileV2Editor("monit", "github.com/SignalFT")
	initProfileV2AppendProfileActionSection(&editor.Document)
	model := newInitProfileV2ReadOnlyModel(editor, 160, 24)
	model = focusInitProfileV2Field(t, model, initProfileV2FieldProfileAction)
	model = selectInitProfileV2FieldValue(t, model, initProfileV2FieldProfileAction, initDetailActionBack)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := updated.(initProfileV2ReadOnlyModel)
	if !ok {
		t.Fatalf("Update returned %T, want initProfileV2ReadOnlyModel", updated)
	}
	if cmd == nil {
		t.Fatal("Update returned nil command, want quit command for back without staging")
	}
	if next.result.StageProfile {
		t.Fatal("StageProfile = true, want back without staging to discard edits")
	}
}

func TestBubbleTeaInitProfileV2PrompterReturnsStagedDraft(t *testing.T) {
	profile := basicProfile("monit")
	prompter := bubbleTeaInitProfileV2Prompter{
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, _ io.Writer) (initInventoryResult, error) {
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "monit",
					Title: "monit",
				},
			}, nil
		},
		profileEditorRunner: func(editor initProfileV2Editor) (initProfileV2EditorResult, error) {
			draft := editor.Draft
			draft.ProfileName = "monit-next"
			return initProfileV2EditorResult{StageProfile: true, Draft: draft}, nil
		},
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "monit",
		ExistingProfileName:  "monit",
		ExistingProfile:      &profile,
		ExistingProfileNames: []string{"monit"},
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"monit": profile}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if draft.ProfileName != "monit-next" {
		t.Fatalf("draft.ProfileName = %q, want staged draft from editor", draft.ProfileName)
	}
}

func TestBubbleTeaInitProfileV2PrompterBootstrapsRuntimeBeforeStaging(t *testing.T) {
	profile := basicProfile("monit")
	editorCalls := 0
	llmPromptCalled := false
	prompter := bubbleTeaInitProfileV2Prompter{
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, _ io.Writer) (initInventoryResult, error) {
			return initInventoryResult{
				Action: initInventoryActionEdit,
				Row: initInventoryRow{
					ID:    "monit",
					Title: "monit",
				},
			}, nil
		},
		profileEditorRunner: func(editor initProfileV2Editor) (initProfileV2EditorResult, error) {
			editorCalls++
			switch editorCalls {
			case 1:
				if len(editor.LLMRuntimes) != 0 {
					t.Fatalf("initial editor runtimes = %#v, want first-run no-runtime state", editor.LLMRuntimes)
				}
				return initProfileV2EditorResult{BootstrapLLMRuntime: true}, nil
			case 2:
				if got := editor.Document.selectedValue(initProfileV2FieldLLMRuntime); got != "codex-cli" {
					t.Fatalf("selected LLM runtime after bootstrap = %q, want codex-cli", got)
				}
				runtime, ok := editor.LLMRuntimes["codex-cli"]
				if !ok {
					t.Fatalf("editor runtimes = %#v, want staged codex-cli runtime", editor.LLMRuntimes)
				}
				if runtime.Provider != config.LLMProviderOpenAI || runtime.Adapter != config.LLMAdapterCodexCLI {
					t.Fatalf("runtime = %#v, want Codex CLI subscription runtime", runtime)
				}
				draft, err := newInitProfileV2ReadOnlyModel(editor, 160, 24).validatedDraft()
				if err != nil {
					t.Fatalf("validatedDraft after bootstrap: %v", err)
				}
				return initProfileV2EditorResult{StageProfile: true, Draft: draft}, nil
			default:
				t.Fatalf("unexpected editor call %d", editorCalls)
				return initProfileV2EditorResult{}, nil
			}
		},
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmPromptCalled = true
			if len(prompt.Context.LLMRuntimes) != 0 {
				t.Fatalf("bootstrap prompt runtimes = %#v, want empty first-run inventory", prompt.Context.LLMRuntimes)
			}
			return initDraft{
				LLMProvider:      string(config.LLMProviderOpenAI),
				LLMAuth:          string(config.LLMAuthSubscription),
				LLMAdapter:       string(config.LLMAdapterCodexCLI),
				LLMCredentialRef: "",
			}, nil
		}),
	}

	draft, err := prompter.Run(initPromptContext{
		RequestedProfileName: "monit",
		ExistingProfileName:  "monit",
		ExistingProfile:      &profile,
		ExistingProfileNames: []string{"monit"},
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"monit": profile}},
		LLMRuntimes:          map[string]initLLMRuntimeDraft{},
		ProfileLLMRuntimes:   map[string]string{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !llmPromptCalled {
		t.Fatal("llmRuntimePrompter was not called")
	}
	if editorCalls != 2 {
		t.Fatalf("editorCalls = %d, want bootstrap editor then staged editor", editorCalls)
	}
	if draft.LLMProvider != string(config.LLMProviderOpenAI) || draft.LLMAdapter != string(config.LLMAdapterCodexCLI) {
		t.Fatalf("draft LLM = (%q,%q), want staged bootstrap runtime", draft.LLMProvider, draft.LLMAdapter)
	}
}

func TestBubbleTeaInitProfileV2PrompterBackWithoutStagingReturnsToChooser(t *testing.T) {
	profile := basicProfile("monit")
	inventoryCalls := 0
	editorCalls := 0
	prompter := bubbleTeaInitProfileV2Prompter{
		inventoryRunner: func(_ initInventoryPrompt, _ io.Reader, _ io.Writer) (initInventoryResult, error) {
			inventoryCalls++
			if inventoryCalls == 1 {
				return initInventoryResult{
					Action: initInventoryActionEdit,
					Row: initInventoryRow{
						ID:    "monit",
						Title: "monit",
					},
				}, nil
			}
			return initInventoryResult{Action: initInventoryActionBack}, nil
		},
		profileEditorRunner: func(initProfileV2Editor) (initProfileV2EditorResult, error) {
			editorCalls++
			return initProfileV2EditorResult{}, nil
		},
	}

	_, err := prompter.Run(initPromptContext{
		RequestedProfileName: "monit",
		ExistingProfileName:  "monit",
		ExistingProfile:      &profile,
		ExistingProfileNames: []string{"monit"},
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"monit": profile}},
	})
	if !errors.Is(err, errInitNavigateBack) {
		t.Fatalf("Run error = %v, want chooser back after unstaged editor exit", err)
	}
	if inventoryCalls != 2 || editorCalls != 1 {
		t.Fatalf("calls = inventory:%d editor:%d, want chooser re-entry after one unstaged editor exit", inventoryCalls, editorCalls)
	}
}

func updateInitLinearEditorModel(t *testing.T, model initLinearEditorModel, msg tea.Msg) initLinearEditorModel {
	t.Helper()
	updated, _ := model.Update(msg)
	next, ok := updated.(initLinearEditorModel)
	if !ok {
		t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
	}
	return next
}

func typeInitLinearText(t *testing.T, model initLinearEditorModel, text string) initLinearEditorModel {
	t.Helper()
	for _, r := range text {
		model = updateInitLinearEditorModel(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return model
}

func focusInitLinearField(t *testing.T, model initLinearEditorModel, id initLinearFieldID) initLinearEditorModel {
	t.Helper()
	index := model.document.fieldIndexByID(id)
	if index < 0 {
		t.Fatalf("field %q missing", id)
	}
	model.focused = index
	model.relayout()
	model.ensureFocusedVisible()
	return model
}

func selectInitLinearFieldValue(t *testing.T, model initLinearEditorModel, id initLinearFieldID, value string) initLinearEditorModel {
	t.Helper()
	index := model.document.fieldIndexByID(id)
	if index < 0 {
		t.Fatalf("field %q missing", id)
	}
	model.selectFieldValue(id, value)
	model.afterFieldChange(index)
	model.relayout()
	model.ensureFocusedVisible()
	if got := model.document.selectedValue(id); got != value {
		t.Fatalf("field %q selected value = %q, want %q", id, got, value)
	}
	return model
}

func stageReviewerEntityEditorRunner(t *testing.T, edits map[initLinearFieldID]string, action string) initReviewerEntityEditorRunner {
	t.Helper()
	if action == "" {
		action = initDetailActionEdit
	}
	return func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
		model := newInitLinearEditorModel(editor, 160, 60)
		for id, value := range edits {
			model.setFieldValue(id, value)
			index := model.document.fieldIndexByID(id)
			if index < 0 {
				t.Fatalf("field %q missing", id)
			}
			model.afterFieldChange(index)
		}
		model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
		model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, action)
		_, _ = io.WriteString(out, model.View())
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		next, ok := updated.(initLinearEditorModel)
		if !ok {
			t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
		}
		if action == initDetailActionEdit && next.resultAction == "" {
			actionIndex := next.document.fieldIndexByID(initReviewerEntityFieldAction)
			if actionIndex >= 0 && strings.TrimSpace(next.document[actionIndex].Error) != "" {
				t.Fatalf("reviewer editor validation blocked staging: %s", next.document[actionIndex].Error)
			}
		}
		return next, nil
	}
}

func stageLLMRuntimeEditorRunner(t *testing.T, selections map[initLinearFieldID]string, action string) initLLMRuntimeEditorRunner {
	t.Helper()
	if action == "" {
		action = initDetailActionEdit
	}
	return func(editor initLinearEditor, _ io.Reader, out io.Writer) (initLinearEditorModel, error) {
		model := newInitLinearEditorModel(editor, 160, 60)
		for id, value := range selections {
			model = selectInitLinearFieldValue(t, model, id, value)
		}
		model = focusInitLinearField(t, model, initLLMRuntimeFieldAction)
		model = selectInitLinearFieldValue(t, model, initLLMRuntimeFieldAction, action)
		_, _ = io.WriteString(out, model.View())
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		next, ok := updated.(initLinearEditorModel)
		if !ok {
			t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
		}
		return next, nil
	}
}

func updateInitProfileV2ReadOnlyModel(t *testing.T, model initProfileV2ReadOnlyModel, msg tea.Msg) initProfileV2ReadOnlyModel {
	t.Helper()
	updated, _ := model.Update(msg)
	next, ok := updated.(initProfileV2ReadOnlyModel)
	if !ok {
		t.Fatalf("Update returned %T, want initProfileV2ReadOnlyModel", updated)
	}
	return next
}

func typeInitProfileV2Text(t *testing.T, model initProfileV2ReadOnlyModel, text string) initProfileV2ReadOnlyModel {
	t.Helper()
	for _, r := range text {
		model = updateInitProfileV2ReadOnlyModel(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return model
}

func focusInitProfileV2Field(t *testing.T, model initProfileV2ReadOnlyModel, id initProfileV2FieldID) initProfileV2ReadOnlyModel {
	t.Helper()
	index := model.document.fieldIndexByID(id)
	if index < 0 {
		t.Fatalf("field %q missing", id)
	}
	model.focused = index
	model.relayout()
	model.ensureFocusedVisible()
	return model
}

func selectInitProfileV2FieldValue(t *testing.T, model initProfileV2ReadOnlyModel, id initProfileV2FieldID, value string) initProfileV2ReadOnlyModel {
	t.Helper()
	index := model.document.fieldIndexByID(id)
	if index < 0 {
		t.Fatalf("field %q missing", id)
	}
	model.selectFieldValue(id, value)
	model.afterFieldChange(index)
	model.relayout()
	model.ensureFocusedVisible()
	if got := model.document.selectedValue(id); got != value {
		t.Fatalf("field %q selected value = %q, want %q", id, got, value)
	}
	return model
}

func newTestInitProfileV2Editor(profileName string, routeText string) initProfileV2Editor {
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	return initProfileV2Editor{
		Draft: initDraft{
			OriginalProfileName: profileName,
			ProfileName:         profileName,
			GitHost:             "github.com",
			GitAuth:             string(config.GitAuthModePAT),
		},
		Document: document,
	}
}

func newTestInitProfileV2EditorWithSelections(profileName string, routeText string, reviewerEntities map[string]initReviewerEntityDraft, llmRuntimes map[string]initLLMRuntimeDraft) initProfileV2Editor {
	draft := initDraft{
		OriginalProfileName: profileName,
		ProfileName:         profileName,
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		LLMProvider:         string(config.LLMProviderAnthropic),
		LLMAuth:             string(config.LLMAuthSubscription),
		LLMAdapter:          string(config.LLMAdapterClaudeCLI),
	}
	llmRuntimeOptions, selectedLLMRuntime := initProfileEditorLLMRuntimeSelection(llmRuntimes, "", draft)
	storeOptions := []huh.Option[string]{
		huh.NewOption(initBuiltInOSCredentialStoreTitle()+" - "+initBuiltInOSCredentialStoreDescription(), config.LocalOSCredentialStoreID),
	}
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	document.addEditableSelect(
		initProfileV2FieldReviewerEntity,
		"Reviewer entity",
		reviewerEntitySelectionDescription(),
		initReviewerEntitySelectionOptions(reviewerEntities, reviewerEntityGitAccountFallbackLabel(config.GitAuthModePAT, "")),
		string(initReviewerEntityKindUseGitIdentity),
	)
	document.addEditableSelect(initProfileV2FieldLLMRuntime, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, selectedLLMRuntime)
	initProfileV2AppendLLMStorageSection(&document, storeOptions, draft.LLMCredentialStore, draft.LLMCredentialRef, !initLLMStorageLabelRelevant(selectedLLMRuntime, llmRuntimes))
	document.addEditableSelect(initProfileV2FieldReviewerModelTier, initReviewerModelTierTitle, initReviewerModelTierDescription, initReviewerModelTierOptions(), draft.LLMReviewerModelTier)
	return initProfileV2Editor{
		Draft:                  draft,
		ReviewerEntities:       reviewerEntities,
		LLMRuntimes:            llmRuntimes,
		CredentialStoreOptions: storeOptions,
		Document:               document,
	}
}

func newTestInitProfileV2EditorWithModelMap(profileName string, routeText string, llm config.LLMConfig, modelMap config.ModelMap) initProfileV2Editor {
	draft := initDraft{
		OriginalProfileName: profileName,
		ProfileName:         profileName,
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		LLMProvider:         string(llm.Provider),
		LLMAuth:             string(llm.Auth),
		LLMAdapter:          string(llm.Adapter),
		ModelMap:            copyModelMap(modelMap),
	}
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendModelMapSection(&document, llm, modelMap)
	return initProfileV2Editor{
		Draft:    draft,
		Document: document,
	}
}

func newTestInitProfileV2EditorWithRuntimeAndModelMap(profileName string, routeText string, llmRuntimes map[string]initLLMRuntimeDraft, selectedRuntime string) initProfileV2Editor {
	draft := initDraft{
		OriginalProfileName: profileName,
		ProfileName:         profileName,
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		LLMProvider:         string(config.LLMProviderAnthropic),
		LLMAuth:             string(config.LLMAuthSubscription),
		LLMAdapter:          string(config.LLMAdapterClaudeCLI),
	}
	if runtime, ok := llmRuntimes[selectedRuntime]; ok {
		draft.LLMProvider = string(runtime.Provider)
		draft.LLMAuth = string(runtime.Auth)
		draft.LLMAdapter = string(runtime.Adapter)
		draft.LLMCredentialStore = initCredentialStoreDraftValue(runtime.CredentialStore)
		draft.LLMCredentialRef = runtime.CredentialRef
	}
	storeOptions := []huh.Option[string]{
		huh.NewOption(initBuiltInOSCredentialStoreTitle()+" - "+initBuiltInOSCredentialStoreDescription(), config.LocalOSCredentialStoreID),
	}
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	llmRuntimeOptions, normalizedRuntime := initProfileEditorLLMRuntimeSelection(llmRuntimes, selectedRuntime, draft)
	document.addEditableSelect(initProfileV2FieldLLMRuntime, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, normalizedRuntime)
	initProfileV2AppendLLMStorageSection(&document, storeOptions, draft.LLMCredentialStore, draft.LLMCredentialRef, !initLLMStorageLabelRelevant(normalizedRuntime, llmRuntimes))
	initProfileV2AppendModelMapSection(&document, initProfileEditorModelMapLLM(draft, normalizedRuntime, llmRuntimes), draft.ModelMap)
	return initProfileV2Editor{
		Draft:                  draft,
		LLMRuntimes:            llmRuntimes,
		CredentialStoreOptions: storeOptions,
		Document:               document,
	}
}

func newTestInitProfileV2EditorWithAgentSources(profileName string, routeText string, agentSources []string) initProfileV2Editor {
	draft := initDraft{
		OriginalProfileName: profileName,
		ProfileName:         profileName,
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		LLMProvider:         string(config.LLMProviderAnthropic),
		LLMAuth:             string(config.LLMAuthSubscription),
		LLMAdapter:          string(config.LLMAdapterClaudeCLI),
		AgentSources:        append([]string(nil), agentSources...),
	}
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendAgentSourcesSection(&document, agentSources)
	return initProfileV2Editor{
		Draft:    draft,
		Document: document,
	}
}

func newTestInitProfileV2EditorWithReviewPolicyAndGitStorage(profileName string, routeText string, policy config.ReviewPolicy, gitStorageLabel string, gitLabelUsesDefault bool, gitScopes map[string]initGitScopeDraft, selectedGitScope string) initProfileV2Editor {
	draft := initDraft{
		OriginalProfileName: profileName,
		ProfileName:         profileName,
		GitHost:             "github.com",
		GitAuth:             string(config.GitAuthModePAT),
		GitCredentialStore:  config.LocalOSCredentialStoreID,
		GitCredentialRef:    strings.TrimSpace(gitStorageLabel),
		LLMProvider:         string(config.LLMProviderAnthropic),
		LLMAuth:             string(config.LLMAuthSubscription),
		LLMAdapter:          string(config.LLMAdapterClaudeCLI),
		ReviewPolicy:        policy,
	}
	storeOptions := []huh.Option[string]{
		huh.NewOption(initBuiltInOSCredentialStoreTitle()+" - "+initBuiltInOSCredentialStoreDescription(), config.LocalOSCredentialStoreID),
	}
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	if selectedGitScope != "" && selectedGitScope != initCustomGitScopeSelection {
		initProfileV2AppendGitScopeSection(&document, selectedGitScope, initGitScopeOptions(gitScopes), draft, true)
	}
	initProfileV2AppendReviewPolicySection(&document, policy)
	initProfileV2AppendGitStorageSection(&document, storeOptions, config.LocalOSCredentialStoreID, gitStorageLabel)
	return initProfileV2Editor{
		Draft:                      draft,
		GitScopes:                  gitScopes,
		CredentialStoreOptions:     storeOptions,
		SelectedGitScope:           selectedGitScope,
		InitialGitStorageLabel:     gitStorageLabel,
		GitStorageLabelUsesDefault: gitLabelUsesDefault,
		Document:                   document,
	}
}

func newTestInitProfileV2EditorWithGitScope(profileName string, routeText string, selectedGitScope string, gitScopes map[string]initGitScopeDraft, draft initDraft) initProfileV2Editor {
	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", profileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendGitScopeSection(&document, selectedGitScope, initGitScopeOptions(gitScopes), draft, true)
	return initProfileV2Editor{
		Draft:            draft,
		GitScopes:        gitScopes,
		SelectedGitScope: selectedGitScope,
		Document:         document,
	}
}

func TestInitInteractiveMenuExitWithoutSaveLeavesConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	original := config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	prompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionSecretsManagement,
				initMenuActionReviewProfiles,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return initDraft{
				ProfileName:          "default",
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
			return initKeyringBackendEdit{Apply: true}, nil
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
	if _, ok := cfg.Profiles["default"]; !ok {
		t.Fatalf("profiles = %#v, want suggested profile", cfg.Profiles)
	}
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
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
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				if len(ctx.ExistingProfileNames) != 0 {
					t.Fatalf("first prompt ExistingProfileNames = %#v, want empty", ctx.ExistingProfileNames)
				}
				return initDraft{
					ProfileName: "home",
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home"}) {
					t.Fatalf("second prompt ExistingProfileNames = %#v, want [home] before focused Back", ctx.ExistingProfileNames)
				}
				return initDraft{}, errInitNavigateBack
			case 3:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home"}) {
					t.Fatalf("third prompt ExistingProfileNames = %#v, want [home]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "home" || ctx.ExistingProfileName != "home" {
					t.Fatalf("third prompt active profile = (%q, %q), want home/home", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				if _, ok := ctx.ExistingConfig.Profiles["home"]; !ok {
					t.Fatalf("third prompt ExistingConfig = %#v, want persisted unsaved home profile", ctx.ExistingConfig.Profiles)
				}
				return initDraft{
					ProfileName: "work",
					GitHost:     "github.company.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 4:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
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
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				return initDraft{
					ProfileName: "work",
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				return initDraft{}, errInitNavigateBack
			case 3:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"work"}) {
					t.Fatalf("third prompt ExistingProfileNames = %#v, want [work]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "work" || ctx.ExistingProfileName != "work" {
					t.Fatalf("third prompt active profile = (%q, %q), want work/work", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				if ctx.ExistingConfig.Profiles["work"].Git.Host != "github.com" {
					t.Fatalf("third prompt work profile = %#v, want first unsaved work draft in session cfg", ctx.ExistingConfig.Profiles["work"])
				}
				return initDraft{
					ProfileName: "home",
					GitHost:     "gitlab.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 4:
				return initDraft{}, errInitNavigateBack
			case 5:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home", "work"}) {
					t.Fatalf("fifth prompt ExistingProfileNames = %#v, want [home work]", ctx.ExistingProfileNames)
				}
				if ctx.RequestedProfileName != "home" || ctx.ExistingProfileName != "home" {
					t.Fatalf("fifth prompt active profile = (%q, %q), want home/home after switching profiles", ctx.RequestedProfileName, ctx.ExistingProfileName)
				}
				work := ctx.ExistingConfig.Profiles["work"]
				home := ctx.ExistingConfig.Profiles["home"]
				if work.Git.Host != "github.com" || home.Git.Host != "gitlab.com" {
					t.Fatalf("fifth prompt ExistingConfig = %#v, want both unsaved profiles available for resume", ctx.ExistingConfig.Profiles)
				}
				return initDraft{
					OriginalProfileName: "work",
					ProfileName:         "work",
					GitHost:             "github.enterprise.local",
					GitAuth:             string(config.GitAuthModePAT),
					LLMProvider:         string(config.LLMProviderAnthropic),
					LLMAuth:             string(config.LLMAuthSubscription),
					LLMAdapter:          string(config.LLMAdapterClaudeCLI),
				}, nil
			case 6:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
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
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				if ctx.RequestedProfileName != "work" || ctx.ExistingProfileName != "work" {
					t.Fatalf("prompt context = %#v, want fallback bootstrap from default work profile", ctx)
				}
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"work"}) {
					t.Fatalf("prompt ExistingProfileNames = %#v, want [work] from fallback bootstrap", ctx.ExistingProfileNames)
				}
				return initDraft{
					ProfileName: "home",
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				if !reflect.DeepEqual(ctx.ExistingProfileNames, []string{"home", "work"}) {
					t.Fatalf("second prompt ExistingProfileNames = %#v, want [home work] before focused Back", ctx.ExistingProfileNames)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
			}
			return initDraft{}, nil
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
	if got := sortedProfileNames(cfg.Profiles); !reflect.DeepEqual(got, []string{"home", "work"}) {
		t.Fatalf("profiles = %#v, want [home work]", got)
	}
}

func TestInitInteractiveMenuRenameProfileReconcilesRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
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
		Profile:    "work",
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
		profileV2Prompter: initPrompterFunc(func(ctx initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				if ctx.ExistingProfileName != "work" {
					t.Fatalf("prompt context = %#v, want selected work profile for rename", ctx)
				}
				return initDraft{
					OriginalProfileName: "work",
					ProfileName:         "office",
					GitHost:             "gitlab.com",
					GitAuth:             string(config.GitAuthModePAT),
					GitCredentialRef:    "codereview/custom-office-git",
					LLMProvider:         string(config.LLMProviderAnthropic),
					LLMAuth:             string(config.LLMAuthSubscription),
					LLMAdapter:          string(config.LLMAdapterClaudeCLI),
					RoutesSet:           true,
					Routes: []configedit.RepositoryRouteSpec{{
						Host:      "gitlab.com",
						Namespace: "open-cli-collective",
					}},
				}, nil
			case 2:
				if ctx.ExistingProfileName != "office" {
					t.Fatalf("second prompt ExistingProfileName = %q, want office after staged rename", ctx.ExistingProfileName)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
			}
			return initDraft{}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatalf("routes prompter should not run from v2 profile editor path: %#v", prompt)
			return initRoutesEdit{}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
			},
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
	prompterCalls := 0
	llmPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionSecretsManagement,
				initMenuActionReviewProfiles,
				initMenuActionLLMRuntimes,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return initDraft{
				ProfileName: "default",
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmPrompterCalls++
			if llmPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
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
			return initKeyringBackendEdit{Apply: true}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
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
	if cfg.Keyring.Backend != "" || cfg.Data.Retention.MaxAgeDaysValue() != 30 || cfg.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("global settings after runtime rebuild = %#v / %#v, want empty backend + 30/manual_only", cfg.Keyring, cfg.Data.Retention)
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimePreservesUnrelatedProfileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	profile := basicProfile("work")
	profile.Git.Host = "gitlab.example.com"
	profile.Git.Credential = config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-git"}
	profile.Git.IdentityCache = "git-cache"
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
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
		RepositoryProfiles: wantRoutes,
		Profiles:           map[string]config.Profile{"work": profile},
	})
	profile = normalizeTestProfile(profile)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	llmPrompterCalls := 0
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
			llmPrompterCalls++
			if llmPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
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
		Profiles: map[string]config.Profile{"work": wantProfile},
	})
	wantProfile = normalizeTestProfile(wantProfile)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	openStoreCalls := 0
	llmPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionSecretsManagement,
				initMenuActionLLMRuntimes,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmPrompterCalls++
			if llmPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile), nil
		}),
		retentionPrompter: initRetentionPrompterFunc(func(initRetentionPrompt) (initRetentionEdit, error) {
			return initRetentionEdit{Apply: true, Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(14),
				Enforcement: config.RetentionAtWrite,
			}}, nil
		}),
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{Apply: true}, nil
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
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring backend = %q, want empty", cfg.Keyring.Backend)
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
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	llmPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionLLMRuntimes,
				initMenuActionExit,
			},
		},
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmPrompterCalls++
			if llmPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile), nil
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
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	store := newFakeInitStore(nil)
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle should not run after focused Back navigation")
		return credstore.Result{}, nil
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
			return store, nil
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
	profilePrompterCalls := 0
	reviewerPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionGlobalSettings,
				initMenuActionSecretsManagement,
				initMenuActionReviewProfiles,
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			profilePrompterCalls++
			if profilePrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return initDraft{
				ProfileName: "default",
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
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
			return initKeyringBackendEdit{Apply: true}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
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
	if cfg.Keyring.Backend != "" || cfg.Data.Retention.MaxAgeDaysValue() != 14 || cfg.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("global settings after reviewer rebuild = %#v / %#v, want empty backend + 14/at_write", cfg.Keyring, cfg.Data.Retention)
	}
}

func TestInitCredentialSecretPromptTitleReviewerKeys(t *testing.T) {
	githubAppEntry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
	}
	patEntry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/work-reviewer",
			Mode:    string(config.GitAuthModePAT),
		},
		KeySpecs: []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}},
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "github app action title",
			got: initCredentialSecretPromptTitle(initCredentialSecretPrompt{
				Entry: githubAppEntry,
			}),
			want: "How should init handle GitHub App reviewer secrets? (codereview/rianjs-bot) (required: github_app_id, github_app_private_key; optional: github_app_installation_id)",
		},
		{
			name: "pat action title",
			got: initCredentialSecretPromptTitle(initCredentialSecretPrompt{
				Entry: patEntry,
			}),
			want: "How should init handle PAT reviewer secret? (codereview/work-reviewer)",
		},
		{
			name: "pat key source title",
			got: initSecretSourcePromptTitle(initSecretValuePrompt{
				Entry: patEntry,
				Key:   credentials.GitTokenKey,
			}),
			want: "How should init get git_token?",
		},
		{
			name: "github app private key source title",
			got: initSecretSourcePromptTitle(initSecretValuePrompt{
				Entry: githubAppEntry,
				Key:   credentials.GitHubAppPrivateKeyKey,
			}),
			want: "How should init get github_app_private_key?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("title = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestInitCredentialDestinationDescriptionLegacyAutoUsesPlatformCopy(t *testing.T) {
	t.Setenv(credentials.BackendEnvVar(), "")
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work"},
		SecretsProfile: credentials.ResolvedSecretsProfile{
			ID:              config.LocalOSCredentialStoreID,
			Label:           "OS credential store",
			Backend:         config.ProjectedOSCredentialStoreBackendKind,
			Source:          config.EffectiveSecretsProfileSourceProjectedLegacy,
			SelectionSource: credentials.SecretsProfileSelectionBuiltInOS,
		},
	}

	description := initCredentialDestinationDescription(initCredentialDestinationContext{
		Entry: entry,
		Config: config.File{
			Profiles: map[string]config.Profile{"work": basicProfile("work")},
		},
	})

	for _, want := range []string{
		"Destination: " + initBuiltInOSCredentialStoreTitle() + " / codereview/work",
		"Credential store: " + config.LocalOSCredentialStoreID,
		"Backend kind: auto",
		"Secret values are collected separately.",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
}

func TestInitCredentialDestinationDescriptionNamedOnePasswordShowsRoutingWithoutTokenValues(t *testing.T) {
	serviceTokenEnv := strings.Join([]string{"OP_SERVICE_ACCOUNT", "TOKEN"}, "_")
	connectTokenEnv := strings.Join([]string{"OP_CONNECT", "TOKEN"}, "_")
	t.Setenv(serviceTokenEnv, "sentinel-service-token-value")
	t.Setenv(connectTokenEnv, "sentinel-connect-token-value")
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"team-vault": {
					DisplayName: "Team Vault",
					Backend: config.SecretsStoreBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOP),
						OnePassword: &config.SecretsStoreOnePasswordConfig{
							VaultID:         "Engineering",
							ItemTitlePrefix: "cr-",
							ItemTag:         "code-review",
							ItemFieldTitle:  "credential",
							ServiceTokenEnv: serviceTokenEnv,
						},
					},
				},
			},
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, config.Profile{SecretsProfile: "team-vault"})
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}

	description := initCredentialDestinationDescription(initCredentialDestinationContext{
		Entry: initCredentialPlanEntry{
			Ref:            config.CredentialRef{Purpose: "reviewer_credentials", Ref: "codereview/rianjs-bot", Mode: string(config.GitAuthModePAT)},
			SecretsProfile: resolved,
		},
		Config: cfg,
	})

	for _, want := range []string{
		"Destination: Team Vault / Engineering / codereview/rianjs-bot",
		"Credential store: team-vault",
		"Backend kind: op",
		"1Password vault: Engineering",
		"1Password service account token env var: " + serviceTokenEnv,
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
	for _, removed := range []string{
		"1Password item title prefix",
		"1Password item tag",
		"1Password item field title",
	} {
		if strings.Contains(description, removed) {
			t.Fatalf("description contains removed item-control copy %q: %s", removed, description)
		}
	}
	for _, leaked := range []string{"sentinel-service-token-value", "sentinel-connect-token-value"} {
		if strings.Contains(description, leaked) {
			t.Fatalf("description leaked token value %q: %s", leaked, description)
		}
	}
}

func TestInitCredentialDestinationDescriptionOnePasswordConnectDoesNotReadTokenValue(t *testing.T) {
	connectTokenEnv := strings.Join([]string{"CUSTOM_CONNECT", "TOKEN"}, "_")
	t.Setenv(connectTokenEnv, "sentinel-connect-token-value")
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"connect-vault": {
					DisplayName: "Connect Vault",
					Backend: config.SecretsStoreBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOPConnect),
						OnePassword: &config.SecretsStoreOnePasswordConfig{
							VaultID:         "Engineering",
							ConnectHost:     "https://connect.example",
							ConnectTokenEnv: connectTokenEnv,
						},
					},
				},
			},
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, config.Profile{SecretsProfile: "connect-vault"})
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}

	description := initCredentialDestinationDescription(initCredentialDestinationContext{
		Entry: initCredentialPlanEntry{
			Ref:            config.CredentialRef{Purpose: "llm", Ref: "codereview/work-llm", Mode: string(config.LLMAuthAPIKey), Provider: string(config.LLMProviderAnthropic)},
			SecretsProfile: resolved,
		},
		Config: cfg,
	})

	for _, want := range []string{
		"Destination: Connect Vault / Engineering / codereview/work-llm",
		"Backend kind: op-connect",
		"1Password Connect host: https://connect.example",
		"1Password Connect token env var: " + connectTokenEnv,
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
	if strings.Contains(description, "sentinel-connect-token-value") {
		t.Fatalf("description leaked Connect token value: %s", description)
	}
}

func TestInitCredentialDestinationDescriptionOnePasswordDesktopShowsAccountID(t *testing.T) {
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"desktop-vault": {
					DisplayName: "Desktop Vault",
					Backend: config.SecretsStoreBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOPDesktop),
						OnePassword: &config.SecretsStoreOnePasswordConfig{
							VaultID:          "Engineering",
							DesktopAccountID: "account-123",
						},
					},
				},
			},
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, config.Profile{SecretsProfile: "desktop-vault"})
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}

	description := initCredentialDestinationDescription(initCredentialDestinationContext{
		Entry: initCredentialPlanEntry{
			Ref:            config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: string(config.GitAuthModePAT)},
			SecretsProfile: resolved,
		},
		Config: cfg,
	})

	for _, want := range []string{
		"Destination: Desktop Vault / Engineering / codereview/work",
		"Backend kind: op-desktop",
		"1Password vault: Engineering",
		"1Password desktop account id: account-123",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
}

func TestInitCredentialDestinationDescriptionUnavailableIsNonFatal(t *testing.T) {
	description := initCredentialDestinationDescription(initCredentialDestinationContext{
		Entry: initCredentialPlanEntry{
			Ref: config.CredentialRef{Purpose: "llm", Ref: "codereview/work-llm"},
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:      "missing-profile",
				Label:   "Missing Profile",
				Backend: string(credstore.BackendOPConnect),
				Source:  config.EffectiveSecretsProfileSourceConfigured,
			},
		},
		Config: config.File{},
	})

	for _, want := range []string{
		"Destination: Missing Profile / codereview/work-llm",
		"Credential store: missing-profile",
		"Backend kind: op-connect",
		"credential destination unavailable",
		"Secret values are collected separately.",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
}

func TestHuhInitSecretPrompterAccessibleShowsCredentialDestination(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitSecretPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}
	_, err := prompter.ChooseCredentialAction(initCredentialSecretPrompt{
		Entry: initCredentialPlanEntry{
			Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work"},
			SecretsProfile: credentials.ResolvedSecretsProfile{
				ID:      "team-vault",
				Label:   "Team Vault",
				Backend: string(credstore.BackendFile),
				Source:  config.EffectiveSecretsProfileSourceConfigured,
			},
		},
		Destination: "Destination: Team Vault / codereview/work",
	})
	if err != nil {
		t.Fatalf("ChooseCredentialAction: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "Destination: Team Vault / codereview/work") {
		t.Fatalf("stderr = %q, want credential destination note", got)
	}
}

func TestHuhInitSecretPrompterAccessibleSecretSourceShowsCredentialDestination(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var stderr bytes.Buffer
	prompter := huhInitSecretPrompter{
		stdin:  strings.NewReader("\n"),
		stderr: &stderr,
	}
	_, err := prompter.ChooseSecretSource(initSecretValuePrompt{
		Entry: initCredentialPlanEntry{
			Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work"},
		},
		Key:         credentials.GitTokenKey,
		Destination: "Destination: Team Vault / codereview/work",
	})
	if err != nil {
		t.Fatalf("ChooseSecretSource: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "Destination: Team Vault / codereview/work") {
		t.Fatalf("stderr = %q, want credential destination note", got)
	}
}

func TestInitSecretPasteDescriptionKeepsDestinationForGitHubAppPrivateKey(t *testing.T) {
	description := initSecretPasteDescription(initSecretValuePrompt{
		Key:         credentials.GitHubAppPrivateKeyKey,
		Destination: "Destination: Team Vault / codereview/rianjs-bot",
	})

	for _, want := range []string{
		"Destination: Team Vault / codereview/rianjs-bot",
		"Clipboard is recommended for multi-line private keys",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
}

func TestInitReviewerCredentialStatusStates(t *testing.T) {
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
	}
	decisions := map[initCredentialDecisionKey]initCredentialDecisionKind{}
	decisions[initCredentialDecisionMapKey(entry, credentials.GitHubAppInstallationIDKey)] = initCredentialDecisionSkipOptional

	status := initReviewerCredentialStatusFromEntry(
		entry,
		map[string]string{credentials.GitHubAppPrivateKeyKey: "sentinel-private-key"},
		decisions,
		map[string]bool{credentials.GitHubAppIDKey: true},
		"",
	)

	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppIDKey, initReviewerCredentialKeyExisting)
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppPrivateKeyKey, initReviewerCredentialKeyStaged)
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppInstallationIDKey, initReviewerCredentialKeySkippedOptional)
	if strings.Contains(initReviewerCredentialStatusDescription(status), "sentinel-private-key") {
		t.Fatalf("status description leaked secret value: %s", initReviewerCredentialStatusDescription(status))
	}
}

func TestInitReviewerCredentialStatusDeferPreservesPartialExistingKeys(t *testing.T) {
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/rianjs-bot",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
	}
	decisions := map[initCredentialDecisionKey]initCredentialDecisionKind{}
	recordInitCredentialEntryDecision(decisions, entry, initCredentialDecisionDefer)

	status := initReviewerCredentialStatusFromEntry(
		entry,
		nil,
		decisions,
		map[string]bool{credentials.GitHubAppIDKey: true},
		"",
	)

	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppIDKey, initReviewerCredentialKeyExisting)
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppPrivateKeyKey, initReviewerCredentialKeyDeferred)
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppInstallationIDKey, initReviewerCredentialKeyOptional)
}

func TestInitReviewerCredentialStatusSelectionFiltersByAuthMode(t *testing.T) {
	ctx := initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingConfig:       config.File{Profiles: map[string]config.Profile{"work": basicProfile("work")}},
		ReviewerCredentialStatuses: []initReviewerCredentialStatus{
			{
				Ref: config.CredentialRef{
					Purpose: "reviewer_credentials",
					Ref:     "codereview/work-reviewer",
					Mode:    string(config.GitAuthModeGitHubApp),
				},
				Keys: []initReviewerCredentialKeyStatus{
					{Key: credentials.GitHubAppIDKey, Required: true, State: initReviewerCredentialKeyStaged},
					{Key: credentials.GitHubAppPrivateKeyKey, Required: true, State: initReviewerCredentialKeyStaged},
				},
			},
			{
				Ref: config.CredentialRef{
					Purpose: "reviewer_credentials",
					Ref:     "codereview/work-reviewer",
					Mode:    string(config.GitAuthModePAT),
				},
				Keys: []initReviewerCredentialKeyStatus{
					{Key: credentials.GitTokenKey, Required: true, State: initReviewerCredentialKeyMissing},
				},
			},
		},
	}
	seed := seedInteractiveInitDraft("work", "work", nil)

	status, ok := initReviewerCredentialStatusForSelectionRef(ctx, seed, string(initReviewerEntityKindPAT), config.LocalOSCredentialStoreID, "codereview/work-reviewer")
	if !ok {
		t.Fatal("PAT status missing")
	}
	assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyMissing)
	assertReviewerCredentialKeyAbsent(t, status, credentials.GitHubAppIDKey)
	assertReviewerCredentialKeyAbsent(t, status, credentials.GitHubAppPrivateKeyKey)
}

func TestInitReviewerCredentialStatusBackendUnavailableDoesNotLeakOrBlock(t *testing.T) {
	entry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/work-reviewer",
			Mode:    string(config.GitAuthModePAT),
		},
		KeySpecs: []credentials.KeySpec{{Key: credentials.GitTokenKey, Required: true}},
	}
	session := initSessionDraft{
		cfg: config.File{
			Profiles: map[string]config.Profile{"work": basicProfile("work")},
		},
		workspace: &initWorkspaceDraft{
			profileName:    "work",
			profile:        basicProfile("work"),
			credentialPlan: []initCredentialPlanEntry{entry},
		},
		writes:              map[string]map[string]string{},
		credentialDecisions: map[initCredentialDecisionKey]initCredentialDecisionKind{},
	}
	deps := initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			return nil, errors.New("sentinel backend unavailable")
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return nil, errors.New("sentinel backend unavailable")
		},
	}

	statuses := buildInteractiveInitReviewerCredentialStatuses(&root.Options{}, deps, session)
	status := findReviewerCredentialStatusForTest(t, statuses, "codereview/work-reviewer", string(config.GitAuthModePAT))
	assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyUnavailable)
	description := initReviewerCredentialStatusDescription(status)
	if !strings.Contains(description, "credential backend status unavailable") {
		t.Fatalf("description = %q, want unavailable note", description)
	}
	if strings.Contains(description, "sentinel backend unavailable") {
		t.Fatalf("description leaked backend error detail: %s", description)
	}
}

func TestInitReviewerCredentialStatusShowsExistingPATAndSecretsProfileDestination(t *testing.T) {
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: "work-1password", Name: "codereview/work-reviewer"},
	}
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"work-1password": {
					DisplayName: "Work Vault",
					Backend: config.SecretsStoreBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOPDesktop),
						OnePassword: &config.SecretsStoreOnePasswordConfig{
							VaultID:          "Engineering",
							ItemTitlePrefix:  "cr-",
							ItemTag:          "code-review",
							ItemFieldTitle:   "credential",
							DesktopAccountID: "account-123",
						},
					},
				},
			},
		},
		Profiles: map[string]config.Profile{"work": profile},
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {credentials.GitTokenKey: "existing-token"},
	})
	session := initSessionDraft{
		cfg: cfg,
		workspace: &initWorkspaceDraft{
			profileName: "work",
			profile:     profile,
		},
		writes:              map[string]map[string]string{},
		credentialDecisions: map[initCredentialDecisionKey]initCredentialDecisionKind{},
	}
	var opened []string
	deps := initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore called for named secrets profile")
			return nil, nil
		},
		openResolvedStore: func(resolved credentials.ResolvedSecretsProfile, _ string, _ bool, _ config.File) (initStore, error) {
			opened = append(opened, resolved.ID)
			return store, nil
		},
	}

	statuses := buildInteractiveInitReviewerCredentialStatuses(&root.Options{}, deps, session)
	status := findReviewerCredentialStatusForTest(t, statuses, "codereview/work-reviewer", string(config.GitAuthModePAT))
	assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyExisting)
	if !reflect.DeepEqual(opened, []string{"work-1password"}) {
		t.Fatalf("opened secrets profiles = %#v, want work-1password", opened)
	}
	description := initReviewerCredentialStatusDescription(status)
	if strings.TrimSpace(status.Destination) == "" {
		t.Fatalf("status.Destination is empty; description fell back to legacy formatter: %q", description)
	}
	for _, want := range []string{
		"Destination: Work Vault / Engineering / codereview/work-reviewer",
		"Credential store: work-1password",
		"Backend kind: op-desktop",
		"1Password vault: Engineering",
		"1Password desktop account id: account-123",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
	for _, removed := range []string{
		"1Password item title prefix",
		"1Password item tag",
		"1Password item field title",
	} {
		if strings.Contains(description, removed) {
			t.Fatalf("description contains removed item-control copy %q: %s", removed, description)
		}
	}
	if strings.Contains(description, "existing-token") {
		t.Fatalf("description leaked secret value: %s", description)
	}
}

func TestInitReviewerCredentialStatusDropsStagedWritesWhenSecretsStoreChanges(t *testing.T) {
	originalProfile := basicProfile("work")
	originalProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:   config.GitAuthModeGitHubApp,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work-reviewer"},
	}
	originalCfg := config.File{Profiles: map[string]config.Profile{"work": originalProfile}}
	originalResolved, err := credentials.ResolveSecretsProfileForProfile(originalCfg, originalProfile)
	if err != nil {
		t.Fatalf("Resolve original secrets profile: %v", err)
	}
	profile := originalProfile
	profile.ReviewerCredentials.Credential.Store = "work-file"
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"work-file": {
					Backend: config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{"work": profile},
	}
	session := initSessionDraft{
		cfg: cfg,
		workspace: &initWorkspaceDraft{
			profileName:     "work",
			profile:         profile,
			previousProfile: &originalProfile,
		},
		writes: map[string]map[string]string{
			"codereview/work-reviewer": {
				credentials.GitHubAppIDKey:         "12345",
				credentials.GitHubAppPrivateKeyKey: "private-key",
			},
		},
		credentialWriteStores: map[string]credentials.ResolvedSecretsProfile{
			"codereview/work-reviewer": originalResolved,
		},
		credentialDecisions: map[initCredentialDecisionKey]initCredentialDecisionKind{},
		satisfiedRefs:       map[string]bool{"codereview/work-reviewer": true},
	}
	deps := initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore called after profile moved to named secrets profile")
			return nil, nil
		},
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
	}

	statuses := buildInteractiveInitReviewerCredentialStatuses(&root.Options{}, deps, session)
	status := findReviewerCredentialStatusForTest(t, statuses, "codereview/work-reviewer", string(config.GitAuthModeGitHubApp))
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppIDKey, initReviewerCredentialKeyMissing)
	assertReviewerCredentialKeyState(t, status, credentials.GitHubAppPrivateKeyKey, initReviewerCredentialKeyMissing)
}

func TestInitReviewerCredentialStatusIncludesSelectableReviewerEntities(t *testing.T) {
	work := basicProfile("work")
	bot := basicProfile("bot")
	bot.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/shared-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": work,
			"bot":  bot,
		},
	}
	store := newFakeInitStore(map[string]map[string]string{
		"shared-reviewer": {credentials.GitTokenKey: "existing-token"},
	})
	session := initSessionDraft{
		cfg: cfg,
		workspace: &initWorkspaceDraft{
			profileName: "work",
			profile:     work,
		},
		writes:              map[string]map[string]string{},
		credentialDecisions: map[initCredentialDecisionKey]initCredentialDecisionKind{},
	}
	deps := initDeps{
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
	}

	statuses := buildInteractiveInitReviewerCredentialStatuses(&root.Options{}, deps, session)
	status := findReviewerCredentialStatusForTest(t, statuses, "codereview/shared-reviewer", string(config.GitAuthModePAT))
	assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyExisting)
	if strings.Contains(initReviewerCredentialStatusDescription(status), "existing-token") {
		t.Fatalf("status description leaked secret value: %s", initReviewerCredentialStatusDescription(status))
	}
}

func TestInitReviewerCredentialStatusIncludesStandardTemplateRefs(t *testing.T) {
	work := basicProfile("work")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": work},
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {credentials.GitTokenKey: "existing-token"},
	})
	session := initSessionDraft{
		cfg: cfg,
		workspace: &initWorkspaceDraft{
			profileName: "work",
			profile:     work,
		},
		writes:              map[string]map[string]string{},
		credentialDecisions: map[initCredentialDecisionKey]initCredentialDecisionKind{},
	}
	deps := initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
	}

	ctx := currentInteractiveInitReviewerEntityPromptContext(&root.Options{}, deps, session)
	status, ok := initReviewerCredentialStatusForSelectionRef(ctx, seedInteractiveInitDraft("work", "work", &work), string(initReviewerEntityKindPAT), config.LocalOSCredentialStoreID, "")
	if !ok {
		t.Fatal("PAT template status missing")
	}
	assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyExisting)
	if strings.Contains(initReviewerCredentialStatusDescription(status), "existing-token") {
		t.Fatalf("status description leaked secret value: %s", initReviewerCredentialStatusDescription(status))
	}
}

func assertReviewerCredentialKeyState(t *testing.T, status initReviewerCredentialStatus, key string, want initReviewerCredentialKeyState) {
	t.Helper()
	for _, row := range status.Keys {
		if row.Key == key {
			if row.State != want {
				t.Fatalf("%s state = %q, want %q in %#v", key, row.State, want, status.Keys)
			}
			return
		}
	}
	t.Fatalf("missing key %q in %#v", key, status.Keys)
}

func assertReviewerCredentialKeyAbsent(t *testing.T, status initReviewerCredentialStatus, key string) {
	t.Helper()
	for _, row := range status.Keys {
		if row.Key == key {
			t.Fatalf("unexpected key %q in %#v", key, status.Keys)
		}
	}
}

func testReviewerGitHubAppPrivateKey() string {
	return strings.Join([]string{
		"-----BEGIN PRIVATE KEY-----",
		"abc123",
		"-----END PRIVATE KEY-----",
	}, "\n")
}

func testLocalOSResolvedCredentialStore() credentials.ResolvedSecretsProfile {
	resolved, err := credentials.ResolveCredentialStore(config.File{}, config.LocalOSCredentialStoreID)
	if err != nil {
		panic(err)
	}
	return resolved
}

func stageDraftInlineGitHubAppReviewerCredentials(draft *initDraft, ref string, appID string, privateKey string) {
	if strings.TrimSpace(draft.ReviewerCredentialStore) == "" {
		draft.ReviewerCredentialStore = config.LocalOSCredentialStoreID
	}
	draft.ReviewerCredentialWriteRef = ref
	draft.ReviewerCredentialWriteStore = testLocalOSResolvedCredentialStore()
	draft.ReviewerCredentialWrites = map[string]string{
		credentials.GitHubAppIDKey:         appID,
		credentials.GitHubAppPrivateKeyKey: privateKey,
	}
	draft.ReviewerCredentialSatisfied = true
}

func stageDraftInlinePATReviewerCredential(draft *initDraft, ref string, token string) {
	if strings.TrimSpace(draft.ReviewerCredentialStore) == "" {
		draft.ReviewerCredentialStore = config.LocalOSCredentialStoreID
	}
	draft.ReviewerCredentialWriteRef = ref
	draft.ReviewerCredentialWriteStore = testLocalOSResolvedCredentialStore()
	draft.ReviewerCredentialWrites = map[string]string{
		credentials.GitTokenKey: token,
	}
	draft.ReviewerCredentialSatisfied = true
}

func TestMergeReviewerCredentialDraftWritesDropsStaleKeysWhenAuthModeChanges(t *testing.T) {
	ref := "codereview/shared-reviewer"
	privateKey := testReviewerGitHubAppPrivateKey()

	t.Run("PAT to GitHub App", func(t *testing.T) {
		session := initSessionDraft{}
		patDraft := initDraft{ReviewerAuth: string(config.GitAuthModePAT)}
		stageDraftInlinePATReviewerCredential(&patDraft, ref, "old-token")
		patDraft.ReviewerCredentialOverwrite = true
		session = mergeReviewerCredentialDraftWrites(session, patDraft)

		appDraft := initDraft{ReviewerAuth: string(config.GitAuthModeGitHubApp)}
		stageDraftInlineGitHubAppReviewerCredentials(&appDraft, ref, "12345", privateKey)
		session = mergeReviewerCredentialDraftWrites(session, appDraft)

		if _, ok := session.writes[ref][credentials.GitTokenKey]; ok {
			t.Fatalf("writes[%q] kept stale PAT key: %#v", ref, session.writes[ref])
		}
		if got := session.writes[ref][credentials.GitHubAppIDKey]; got != "12345" {
			t.Fatalf("github_app_id = %q, want current GitHub App staged value", got)
		}
		if got := session.writes[ref][credentials.GitHubAppPrivateKeyKey]; got != privateKey {
			t.Fatalf("github_app_private_key = %q, want current GitHub App staged value", got)
		}
		if session.overwriteRefs[ref] {
			t.Fatalf("overwriteRefs[%q] = true, want stale overwrite flag cleared", ref)
		}

		profile := basicProfile("work")
		profile.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: ref,
		}
		if _, err := planInitCredentialsWithConfig(config.File{Profiles: map[string]config.Profile{"work": profile}}, nil, profile, projectInitPlannedWriteKeys(session.writes)); err != nil {
			t.Fatalf("planInitCredentialsWithConfig: %v", err)
		}
	})

	t.Run("GitHub App to PAT", func(t *testing.T) {
		session := initSessionDraft{}
		appDraft := initDraft{ReviewerAuth: string(config.GitAuthModeGitHubApp)}
		stageDraftInlineGitHubAppReviewerCredentials(&appDraft, ref, "12345", privateKey)
		session = mergeReviewerCredentialDraftWrites(session, appDraft)

		patDraft := initDraft{ReviewerAuth: string(config.GitAuthModePAT)}
		stageDraftInlinePATReviewerCredential(&patDraft, ref, "new-token")
		session = mergeReviewerCredentialDraftWrites(session, patDraft)

		if _, ok := session.writes[ref][credentials.GitHubAppIDKey]; ok {
			t.Fatalf("writes[%q] kept stale GitHub App key: %#v", ref, session.writes[ref])
		}
		if _, ok := session.writes[ref][credentials.GitHubAppPrivateKeyKey]; ok {
			t.Fatalf("writes[%q] kept stale GitHub App private key: %#v", ref, session.writes[ref])
		}
		if got := session.writes[ref][credentials.GitTokenKey]; got != "new-token" {
			t.Fatalf("git_token = %q, want current PAT staged value", got)
		}

		profile := basicProfile("work")
		profile.ReviewerCredentials = &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: ref,
		}
		if _, err := planInitCredentialsWithConfig(config.File{Profiles: map[string]config.Profile{"work": profile}}, nil, profile, projectInitPlannedWriteKeys(session.writes)); err != nil {
			t.Fatalf("planInitCredentialsWithConfig: %v", err)
		}
	})
}

func TestMergeReviewerCredentialDraftWritesPreservesOverwriteFlagOnNoSecretReentry(t *testing.T) {
	ref := "codereview/work-reviewer"
	session := initSessionDraft{}
	firstDraft := initDraft{ReviewerAuth: string(config.GitAuthModePAT)}
	stageDraftInlinePATReviewerCredential(&firstDraft, ref, "replacement-token")
	firstDraft.ReviewerCredentialOverwrite = true
	session = mergeReviewerCredentialDraftWrites(session, firstDraft)

	secondDraft := initDraft{
		ReviewerAuth:                string(config.GitAuthModePAT),
		ReviewerCredentialWriteRef:  ref,
		ReviewerCredentialSatisfied: true,
	}
	session = mergeReviewerCredentialDraftWrites(session, secondDraft)

	if got := session.writes[ref][credentials.GitTokenKey]; got != "replacement-token" {
		t.Fatalf("staged git_token = %q, want replacement retained", got)
	}
	if !session.overwriteRefs[ref] {
		t.Fatalf("overwriteRefs[%q] = false, want retained for existing staged replacement", ref)
	}
}

func TestInitInteractiveMenuFocusedGitHubAppReviewerInlineWritesReadyWithoutHints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	privateKey := testReviewerGitHubAppPrivateKey()
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	secretPrompter := &fakeInitSecretPrompter{}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {
			credentials.GitTokenKey: "existing-token",
		},
	})
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			draft.ReviewerCredentialRef = "codereview/rianjs-bot"
			draft.ReviewerDisplayName = "rianjs-bot"
			stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "12345", privateKey)
			return draft, nil
		}),
		secretPrompter: secretPrompter,
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := got.Profiles["work"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want deferred GitHub App reviewer saved")
	}
	if profile.ReviewerCredentials.AuthMode != config.GitAuthModeGitHubApp ||
		profile.ReviewerCredentials.CredentialRef != "codereview/rianjs-bot" ||
		profile.ReviewerCredentials.DisplayName != "rianjs-bot" {
		t.Fatalf("reviewer credentials = %#v, want rianjs-bot GitHub App reviewer", profile.ReviewerCredentials)
	}
	if len(secretPrompter.actionPrompts) != 0 || len(secretPrompter.sourcePrompts) != 0 {
		t.Fatalf("secret prompts = actions:%d sources:%d, want inline reviewer credential collection only", len(secretPrompter.actionPrompts), len(secretPrompter.sourcePrompts))
	}
	if got := store.bundles["rianjs-bot"][credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("stored app id = %q, want staged inline value", got)
	}
	if got := store.bundles["rianjs-bot"][credentials.GitHubAppPrivateKeyKey]; got != privateKey {
		t.Fatalf("stored private key = %q, want staged inline private key", got)
	}
	if _, ok := store.bundles["rianjs-bot"][credentials.GitHubAppInstallationIDKey]; ok {
		t.Fatalf("installation id stored unexpectedly: %#v", store.bundles["rianjs-bot"])
	}
	if !strings.Contains(stdout.String(), "- work: ready") {
		t.Fatalf("stdout = %q, want ready profile after inline reviewer credentials", stdout.String())
	}
	if strings.Contains(stdout.String(), "reviewer deferred") || strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/rianjs-bot") {
		t.Fatalf("stdout/stderr kept follow-up hint:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
}

func TestInitInteractiveMenuFocusedGitHubAppReviewerInlineWritesBeforeCommitAndDoesNotRepeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionReviewerEntities,
			initMenuActionSave,
		},
	}
	privateKey := testReviewerGitHubAppPrivateKey()
	secretPrompter := &fakeInitSecretPrompter{}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	reviewerPrompterCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			draft.ReviewerCredentialRef = "codereview/rianjs-bot"
			draft.ReviewerDisplayName = "rianjs-bot"
			stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "12345", privateKey)
			return draft, nil
		}),
		secretPrompter: secretPrompter,
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %d, want reviewer menu then save", len(menu.prompts))
	}
	if len(secretPrompter.actionPrompts) != 0 || len(secretPrompter.sourcePrompts) != 0 {
		t.Fatalf("secret prompts = actions:%d sources:%d, want no separate reviewer credential prompt", len(secretPrompter.actionPrompts), len(secretPrompter.sourcePrompts))
	}
	if got := store.bundles["rianjs-bot"][credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("stored app id = %q, want staged value written at commit", got)
	}
	if got := store.bundles["rianjs-bot"][credentials.GitHubAppPrivateKeyKey]; got != privateKey {
		t.Fatalf("stored private key = %q, want multi-line private key", got)
	}
	if _, ok := store.bundles["rianjs-bot"][credentials.GitHubAppInstallationIDKey]; ok {
		t.Fatalf("installation id stored unexpectedly: %#v", store.bundles["rianjs-bot"])
	}
	if strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/rianjs-bot") {
		t.Fatalf("stderr = %q, want no follow-up hints after set-now reviewer flow", stderr.String())
	}
	if !strings.Contains(stdout.String(), "- work: ready") {
		t.Fatalf("stdout = %q, want ready profile after set-now reviewer flow", stdout.String())
	}
}

func TestInitInteractiveMenuLabelDerivedReviewerRefDoesNotOverwriteUnconfiguredExistingBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
		"rianjs-bot-reviewer": {
			credentials.GitHubAppIDKey:         "existing-app-id",
			credentials.GitHubAppPrivateKeyKey: "existing-private-key",
		},
	})
	privateKey := testReviewerGitHubAppPrivateKey()
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			model := newInitLinearEditorModel(initReviewerEntityLinearEditor(prompt.Context, seed), 160, 60)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
			model.setFieldValue(initReviewerEntityFieldLabel, "rianjs-bot")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldLabel))
			model.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
			model.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, privateKey)
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))
			model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			if next.resultAction != initDetailActionEdit {
				t.Fatalf("resultAction = %q, want staged reviewer settings; action error = %q", next.resultAction, next.document[next.document.fieldIndexByID(initReviewerEntityFieldAction)].Error)
			}
			return initReviewerEntityDraftFromDocument(prompt.Context, seed, next.document)
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite existing label-derived reviewer bundle conflict")
			return nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err == nil || !strings.Contains(err.Error(), credstore.ErrExists.Error()) {
		t.Fatalf("runInitWithDeps error = %v, want no-overwrite conflict", err)
	}
	if got := store.bundles["rianjs-bot-reviewer"][credentials.GitHubAppIDKey]; got != "existing-app-id" {
		t.Fatalf("stored github_app_id = %q, want existing value preserved after conflict", got)
	}
	if got := store.bundles["rianjs-bot-reviewer"][credentials.GitHubAppPrivateKeyKey]; got != "existing-private-key" {
		t.Fatalf("stored private key = %q, want existing value preserved after conflict", got)
	}
	if strings.Contains(stdout.String(), "Saved staged init changes") {
		t.Fatalf("stdout = %q, want save blocked by no-overwrite conflict", stdout.String())
	}
}

func TestInitInteractiveMenuManualReviewerRefDoesNotOverwriteUnconfiguredExistingBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
		"manual-reviewer": {
			credentials.GitHubAppIDKey:         "existing-app-id",
			credentials.GitHubAppPrivateKeyKey: "existing-private-key",
		},
	})
	privateKey := testReviewerGitHubAppPrivateKey()
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			model := newInitLinearEditorModel(initReviewerEntityLinearEditor(prompt.Context, seed), 160, 60)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldSelection, string(initReviewerEntityKindGitHubApp))
			model.setFieldValue(initReviewerEntityFieldSecretLocation, "codereview/manual-reviewer")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation))
			model.setFieldValue(initReviewerEntityFieldGitHubAppID, "12345")
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppID))
			model.setFieldValue(initReviewerEntityFieldGitHubAppPrivateKey, privateKey)
			model.afterFieldChange(model.document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey))
			model = focusInitLinearField(t, model, initReviewerEntityFieldAction)
			model = selectInitLinearFieldValue(t, model, initReviewerEntityFieldAction, initDetailActionEdit)

			updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			next, ok := updated.(initLinearEditorModel)
			if !ok {
				t.Fatalf("Update returned %T, want initLinearEditorModel", updated)
			}
			if next.resultAction != initDetailActionEdit {
				t.Fatalf("resultAction = %q, want staged reviewer settings; action error = %q", next.resultAction, next.document[next.document.fieldIndexByID(initReviewerEntityFieldAction)].Error)
			}
			return initReviewerEntityDraftFromDocument(prompt.Context, seed, next.document)
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite existing manual reviewer bundle conflict")
			return nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err == nil || !strings.Contains(err.Error(), credstore.ErrExists.Error()) {
		t.Fatalf("runInitWithDeps error = %v, want no-overwrite conflict", err)
	}
	if got := store.bundles["manual-reviewer"][credentials.GitHubAppIDKey]; got != "existing-app-id" {
		t.Fatalf("stored github_app_id = %q, want existing value preserved after conflict", got)
	}
	if got := store.bundles["manual-reviewer"][credentials.GitHubAppPrivateKeyKey]; got != "existing-private-key" {
		t.Fatalf("stored private key = %q, want existing value preserved after conflict", got)
	}
	if strings.Contains(stdout.String(), "Saved staged init changes") {
		t.Fatalf("stdout = %q, want save blocked by no-overwrite conflict", stdout.String())
	}
}

func TestInitInteractiveMenuFocusedPATReviewerPromptsForGitTokenOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	secretPrompter := &fakeInitSecretPrompter{}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModePAT)
			draft.ReviewerCredentialRef = "codereview/work-reviewer"
			stageDraftInlinePATReviewerCredential(&draft, draft.ReviewerCredentialRef, "reviewer-pat")
			return draft, nil
		}),
		secretPrompter: secretPrompter,
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(secretPrompter.actionPrompts) != 0 || len(secretPrompter.sourcePrompts) != 0 {
		t.Fatalf("secret prompts = actions:%d sources:%d, want no separate reviewer PAT prompt", len(secretPrompter.actionPrompts), len(secretPrompter.sourcePrompts))
	}
	if got := store.bundles["work-reviewer"][credentials.GitTokenKey]; got != "reviewer-pat" {
		t.Fatalf("stored reviewer PAT = %q, want staged value written at commit", got)
	}
	if strings.Contains(stderr.String(), credentials.GitHubAppIDKey) || strings.Contains(stderr.String(), credentials.GitHubAppPrivateKeyKey) {
		t.Fatalf("stderr = %q, want PAT reviewer flow not GitHub App keys", stderr.String())
	}
}

func TestInitInteractiveMenuFocusedUseGitReviewerSkipsReviewerCredentialPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = false
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"work": {credentials.GitTokenKey: "existing-token"},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
}

func TestInitInteractiveMenuReviewerCredentialDecisionDropsAfterReviewerCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	secretPrompter := &fakeInitSecretPrompter{}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	store.setBundleFunc = func(profile string, kv map[string]string, _ ...credstore.SetOpt) (credstore.Result, error) {
		if profile == "rianjs-bot" {
			t.Fatalf("SetBundle called for stale reviewer ref with %#v", kv)
		}
		return credstore.Result{}, nil
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			switch reviewerPrompterCalls {
			case 1:
				draft.ReviewerEnabled = true
				draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
				draft.ReviewerCredentialRef = "codereview/rianjs-bot"
				stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "12345", testReviewerGitHubAppPrivateKey())
			case 2:
				draft.ReviewerEnabled = false
			default:
				return initDraft{}, errInitNavigateBack
			}
			return draft, nil
		}),
		secretPrompter: secretPrompter,
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(secretPrompter.actionPrompts) != 0 || len(secretPrompter.sourcePrompts) != 0 {
		t.Fatalf("secret prompts = actions:%d sources:%d, want no separate reviewer credential prompt", len(secretPrompter.actionPrompts), len(secretPrompter.sourcePrompts))
	}
	if strings.Contains(stdout.String(), "reviewer deferred") || strings.Contains(stderr.String(), "rianjs-bot") {
		t.Fatalf("stdout/stderr kept stale reviewer decision:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["work"].ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials = %#v, want cleared reviewer", got.Profiles["work"].ReviewerCredentials)
	}
}

func TestInitInteractiveMenuReviewerCredentialDecisionDropsAfterReviewerRefChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	secretPrompter := &fakeInitSecretPrompter{}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			switch reviewerPrompterCalls {
			case 1:
				draft.ReviewerCredentialRef = "codereview/old-reviewer"
				stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "old-app-id", testReviewerGitHubAppPrivateKey())
			case 2:
				draft.ReviewerCredentialRef = "codereview/new-reviewer"
				stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "new-app-id", testReviewerGitHubAppPrivateKey())
			default:
				return initDraft{}, errInitNavigateBack
			}
			return draft, nil
		}),
		secretPrompter: secretPrompter,
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(secretPrompter.actionPrompts) != 0 || len(secretPrompter.sourcePrompts) != 0 {
		t.Fatalf("secret prompts = actions:%d sources:%d, want no separate reviewer credential prompt", len(secretPrompter.actionPrompts), len(secretPrompter.sourcePrompts))
	}
	out := stdout.String() + "\n" + stderr.String()
	if strings.Contains(out, "old-reviewer") {
		t.Fatalf("output kept stale reviewer ref:\n%s", out)
	}
	if strings.Contains(out, "set-credential --store local-os --name codereview/new-reviewer") {
		t.Fatalf("output kept follow-up hint for inline reviewer ref:\n%s", out)
	}
	if _, ok := store.bundles["old-reviewer"]; ok {
		t.Fatalf("old reviewer bundle = %#v, want stale inline write filtered", store.bundles["old-reviewer"])
	}
	if got := store.bundles["new-reviewer"][credentials.GitHubAppIDKey]; got != "new-app-id" {
		t.Fatalf("new reviewer app id = %q, want active inline write", got)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["work"].ReviewerCredentials == nil || got.Profiles["work"].ReviewerCredentials.CredentialRef != "codereview/new-reviewer" {
		t.Fatalf("reviewer credentials = %#v, want new reviewer ref", got.Profiles["work"].ReviewerCredentials)
	}
}

func TestInitInteractiveMenuReviewerSetNowDiscardDoesNotWriteConfigOrCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	}
	cfg = config.Normalize(cfg)
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle called before commit/discard")
		return credstore.Result{}, nil
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			draft.ReviewerCredentialRef = "codereview/rianjs-bot"
			stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "sentinel-app-id", "sentinel-private-key")
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called on discard")
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
	if got.Profiles["work"].ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials after discard = %#v, want original nil reviewer", got.Profiles["work"].ReviewerCredentials)
	}
	// #nosec G304 -- path is a test-controlled temporary config file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}
	if strings.Contains(string(raw), "rianjs-bot") || strings.Contains(string(raw), "sentinel") {
		t.Fatalf("config after discard contains staged reviewer data:\n%s", string(raw))
	}
	if _, ok := store.bundles["rianjs-bot"]; ok {
		t.Fatalf("reviewer bundle = %#v, want no credential write on discard", store.bundles["rianjs-bot"])
	}
}

func TestInitInteractiveMenuReviewerBackWithoutStagingDiscardsInlineSecretWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	}
	cfg = config.Normalize(cfg)
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle called after backing out of reviewer secret capture")
		return credstore.Result{}, nil
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		reviewerPrompter: initReviewerEntityPrompterFunc(func(_ initReviewerEntityPrompt) (initDraft, error) {
			return initDraft{}, errInitNavigateBack
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got.Profiles["work"].ReviewerCredentials != nil {
		t.Fatalf("reviewer credentials after back = %#v, want original nil reviewer", got.Profiles["work"].ReviewerCredentials)
	}
	// #nosec G304 -- path is a test-controlled temporary config file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}
	if strings.Contains(string(raw), "sentinel") {
		t.Fatalf("config after secret-step back contains partial secret values:\n%s", string(raw))
	}
	if _, ok := store.bundles["rianjs-bot"]; ok {
		t.Fatalf("reviewer bundle = %#v, want no credential write after back without staging", store.bundles["rianjs-bot"])
	}
}

func TestInitInteractiveMenuReviewerCredentialStatusShowsStagedOnReentry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work": {credentials.GitTokenKey: "existing-token"},
	})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle called before commit")
		return credstore.Result{}, nil
	}
	reviewerPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			switch reviewerPrompterCalls {
			case 1:
				draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
				draft.ReviewerEnabled = true
				draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
				draft.ReviewerCredentialRef = "codereview/rianjs-bot"
				stageDraftInlineGitHubAppReviewerCredentials(&draft, draft.ReviewerCredentialRef, "sentinel-app-id", "sentinel-private-key")
				return draft, nil
			case 2:
				if len(prompt.Context.ReviewerCredentialStatuses) == 0 {
					t.Fatalf("second prompt reviewer statuses empty; existing profile name = %q; staged profile = %#v", prompt.Context.ExistingProfileName, prompt.Context.ExistingConfig.Profiles["work"].ReviewerCredentials)
				}
				status := findReviewerCredentialStatusForTest(t, prompt.Context.ReviewerCredentialStatuses, "codereview/rianjs-bot", string(config.GitAuthModeGitHubApp))
				assertReviewerCredentialKeyState(t, status, credentials.GitHubAppIDKey, initReviewerCredentialKeyStaged)
				assertReviewerCredentialKeyState(t, status, credentials.GitHubAppPrivateKeyKey, initReviewerCredentialKeyStaged)
				assertReviewerCredentialKeyState(t, status, credentials.GitHubAppInstallationIDKey, initReviewerCredentialKeyOptional)
				description := initReviewerCredentialStatusDescription(status)
				if strings.Contains(description, "sentinel") {
					t.Fatalf("status leaked secret value: %s", description)
				}
				return initDraft{}, errInitNavigateBack
			default:
				return initDraft{}, errInitNavigateBack
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{},
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called on discard")
			return nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if reviewerPrompterCalls != 2 {
		t.Fatalf("reviewerPrompterCalls = %d, want staged reviewer and reentry", reviewerPrompterCalls)
	}
	if _, ok := store.bundles["rianjs-bot"]; ok {
		t.Fatalf("reviewer bundle = %#v, want no credential write before commit", store.bundles["rianjs-bot"])
	}
}

func findReviewerCredentialStatusForTest(t *testing.T, statuses []initReviewerCredentialStatus, ref string, mode string) initReviewerCredentialStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Ref.Ref == ref && status.Ref.Mode == mode {
			return status
		}
	}
	available := make([]string, 0, len(statuses))
	for _, status := range statuses {
		available = append(available, fmt.Sprintf("%s/%s keys=%d", status.Ref.Ref, status.Ref.Mode, len(status.Keys)))
	}
	t.Fatalf("missing reviewer credential status ref=%q mode=%q; available: %s", ref, mode, strings.Join(available, ", "))
	return initReviewerCredentialStatus{}
}

func TestInitInteractiveMenuFocusedReviewerEntitySavePreservesCustomCredentialRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": basicProfile("work"),
		},
	}
	cfg.Profiles["work"] = config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/work",
		},
		ReviewerCredentials: &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModeGitHubApp,
			CredentialRef: "codereview/custom-work-reviewer",
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile), nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"work": {
					credentials.GitTokenKey: "existing-token",
				},
				"custom-work-reviewer": {
					credentials.GitHubAppIDKey:         "12345",
					credentials.GitHubAppPrivateKeyKey: "private-key",
				},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := got.Profiles["work"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want custom reviewer ref preserved after save")
	}
	if profile.ReviewerCredentials.CredentialRef != "codereview/custom-work-reviewer" {
		t.Fatalf("reviewer credential ref = %q, want custom reviewer ref preserved after save", profile.ReviewerCredentials.CredentialRef)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntityLabelOnlySaveSkipsCredentialWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/work-reviewer",
					DisplayName:   "Old label",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	store := newFakeInitStore(map[string]map[string]string{
		"work": {
			credentials.GitTokenKey: "existing-token",
		},
		"work-reviewer": {
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle called for reviewer label-only save")
		return credstore.Result{}, nil
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			draft.ReviewerDisplayName = "OC Collective bot"
			return draft, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := got.Profiles["work"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want reviewer still configured")
	}
	if got, want := profile.ReviewerCredentials.DisplayName, "OC Collective bot"; got != want {
		t.Fatalf("reviewer display name = %q, want %q", got, want)
	}
	if got, want := profile.ReviewerCredentials.CredentialRef, "codereview/work-reviewer"; got != want {
		t.Fatalf("reviewer credential ref = %q, want %q", got, want)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntitySaveRestoresDefaultCredentialRefWhenDraftClearsCustomRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/custom-work-reviewer",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	reviewerPrompterCalls := 0
	openStoreCalls := 0
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerPrompterCalls++
			if reviewerPrompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.ReviewerEnabled = true
			draft.ReviewerAuth = string(config.GitAuthModeGitHubApp)
			draft.ReviewerCredentialRef = ""
			return draft, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionKeep,
				initCredentialSecretActionKeep,
			},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			openStoreCalls++
			return newFakeInitStore(map[string]map[string]string{
				"work": {
					credentials.GitTokenKey: "existing-token",
				},
				"work-reviewer": {
					credentials.GitHubAppIDKey:         "12345",
					credentials.GitHubAppPrivateKeyKey: "private-key",
				},
				"custom-work-reviewer": {
					credentials.GitHubAppIDKey:         "legacy-id",
					credentials.GitHubAppPrivateKeyKey: "legacy-private-key",
				},
			}), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if openStoreCalls == 0 {
		t.Fatal("openStoreCalls = 0, want credential-shape change to inspect the store")
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	profile := got.Profiles["work"]
	if profile.ReviewerCredentials == nil {
		t.Fatal("reviewer credentials = nil, want default reviewer ref restored after save")
	}
	if profile.ReviewerCredentials.CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("reviewer credential ref = %q, want generated default reviewer ref after clearing custom ref", profile.ReviewerCredentials.CredentialRef)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntityPromptContextIncludesCredentialStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {credentials.GitTokenKey: "existing-token"},
	})
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		t.Fatal("SetBundle should not run while building focused reviewer prompt context")
		return credstore.Result{}, nil
	}
	deps := initDeps{
		menuPrompter: &fakeInitMenuPrompter{
			actions: []initMenuAction{
				initMenuActionReviewerEntities,
				initMenuActionExit,
			},
		},
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			status := findReviewerCredentialStatusForTest(t, prompt.Context.ReviewerCredentialStatuses, "codereview/work-reviewer", string(config.GitAuthModePAT))
			assertReviewerCredentialKeyState(t, status, credentials.GitTokenKey, initReviewerCredentialKeyExisting)
			return initDraft{}, errInitNavigateBack
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			return store, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
}

func TestInitInteractiveMenuFocusedReviewProfilesDoesNotOpenStoreForPromptContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	}
	cfg = config.Normalize(cfg)
	saveCredentialTestConfig(t, path, cfg)
	existing := cfg.Profiles["work"]
	expectedPrompt := buildInteractiveInitInventoryPromptContext(initPromptContext{
		RequestedProfileName: "work",
		ExistingProfileName:  "work",
		ExistingProfile:      &existing,
		ExistingProfileNames: []string{"work"},
		ExistingConfig:       cfg,
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
				initMenuActionExit,
			},
		},
		profileV2Prompter: initPrompterFunc(func(prompt initPromptContext) (initDraft, error) {
			if prompt.RequestedProfileName != expectedPrompt.RequestedProfileName ||
				prompt.ExistingProfileName != expectedPrompt.ExistingProfileName {
				t.Fatalf("prompt identity = %#v, want %#v", prompt, expectedPrompt)
			}
			if prompt.ExistingProfile == nil {
				t.Fatal("ExistingProfile = nil, want existing work profile")
			}
			if !reflect.DeepEqual(prompt.ExistingProfile.Git, expectedPrompt.ExistingProfile.Git) {
				t.Fatalf("ExistingProfile.Git = %#v, want %#v", prompt.ExistingProfile.Git, expectedPrompt.ExistingProfile.Git)
			}
			if !reflect.DeepEqual(prompt.ExistingProfile.LLM, expectedPrompt.ExistingProfile.LLM) {
				t.Fatalf("ExistingProfile.LLM = %#v, want %#v", prompt.ExistingProfile.LLM, expectedPrompt.ExistingProfile.LLM)
			}
			if !reflect.DeepEqual(prompt.ExistingProfileNames, expectedPrompt.ExistingProfileNames) {
				t.Fatalf("ExistingProfileNames = %#v, want %#v", prompt.ExistingProfileNames, expectedPrompt.ExistingProfileNames)
			}
			if !reflect.DeepEqual(prompt.ProfileGitScopes, expectedPrompt.ProfileGitScopes) {
				t.Fatalf("ProfileGitScopes = %#v, want %#v", prompt.ProfileGitScopes, expectedPrompt.ProfileGitScopes)
			}
			if !reflect.DeepEqual(prompt.ProfileReviewerEntities, expectedPrompt.ProfileReviewerEntities) {
				t.Fatalf("ProfileReviewerEntities = %#v, want %#v", prompt.ProfileReviewerEntities, expectedPrompt.ProfileReviewerEntities)
			}
			if !reflect.DeepEqual(prompt.ProfileLLMRuntimes, expectedPrompt.ProfileLLMRuntimes) {
				t.Fatalf("ProfileLLMRuntimes = %#v, want %#v", prompt.ProfileLLMRuntimes, expectedPrompt.ProfileLLMRuntimes)
			}
			return initDraft{}, errInitNavigateBack
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run for focused review profile prompt context")
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

func TestInitInteractiveMenuFocusedReviewerEntityDeleteUndoReturnsToMenu(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"work": func() config.Profile {
				profile := basicProfile("work")
				profile.ReviewerCredentials = &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModeGitHubApp,
					CredentialRef: "codereview/shared-reviewer",
				}
				return profile
			}(),
		},
	}
	saveCredentialTestConfig(t, path, cfg)
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionReviewerEntities,
			initMenuActionExit,
		},
	}
	reviewerCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerCalls++
			switch reviewerCalls {
			case 1:
				if _, ok := prompt.Context.ReviewerEntities["reviewer-github-app"]; !ok {
					t.Fatalf("ReviewerEntities = %#v, want configured reviewer-github-app before delete", prompt.Context.ReviewerEntities)
				}
				return initDraft{
					Action:       initDraftActionDeleteReviewerEntity,
					ActionTarget: "reviewer-github-app",
				}, nil
			case 2:
				if _, ok := prompt.Context.PendingReviewerEntityDeletes["reviewer-github-app"]; !ok {
					t.Fatalf("PendingReviewerEntityDeletes = %#v, want reviewer-github-app pending delete before undo", prompt.Context.PendingReviewerEntityDeletes)
				}
				return initDraft{
					Action:       initDraftActionUndoDeleteReviewerEntity,
					ActionTarget: "reviewer-github-app",
				}, nil
			case 3:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected reviewer prompt #%d", reviewerCalls)
				return initDraft{}, nil
			}
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if reviewerCalls != 3 {
		t.Fatalf("reviewerCalls = %d, want delete, undo, then back inside reviewer category", reviewerCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu before reviewer category and after backing out", menu.prompts)
	}
}

func TestInitInteractiveMenuFocusedReviewerEntityStageReturnsToMenu(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionReviewerEntities,
			initMenuActionExit,
		},
	}
	reviewerCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerCalls++
			switch reviewerCalls {
			case 1:
				draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
				draft.ReviewerEnabled = true
				draft.ReviewerAuth = string(config.GitAuthModePAT)
				return draft, nil
			default:
				t.Fatalf("unexpected reviewer prompt #%d", reviewerCalls)
				return initDraft{}, nil
			}
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
	if reviewerCalls != 1 {
		t.Fatalf("reviewerCalls = %d, want one staged reviewer edit", reviewerCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu before category entry and after stage", menu.prompts)
	}
	if !menu.prompts[1].CanSave {
		t.Fatalf("post-stage menu prompt = %#v, want staged reviewer edit to be saveable", menu.prompts[1])
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimeStageReturnsToMenu(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionLLMRuntimes,
			initMenuActionExit,
		},
	}
	llmCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmCalls++
			switch llmCalls {
			case 1:
				draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
				draft.LLMProvider = string(config.LLMProviderOpenAI)
				draft.LLMAuth = string(config.LLMAuthSubscription)
				draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
				return draft, nil
			default:
				t.Fatalf("unexpected LLM prompt #%d", llmCalls)
				return initDraft{}, nil
			}
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if llmCalls != 1 {
		t.Fatalf("llmCalls = %d, want one staged runtime edit", llmCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu before category entry and after stage", menu.prompts)
	}
	if !menu.prompts[1].CanSave {
		t.Fatalf("post-stage menu prompt = %#v, want staged runtime edit to be saveable", menu.prompts[1])
	}
}

func TestInitInteractiveMenuCanCommitLLMRuntimeBeforeReviewProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionLLMRuntimes,
			initMenuActionSave,
		},
	}
	llmCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmCalls++
			if len(prompt.Context.ExistingConfig.Profiles) != 0 {
				t.Fatalf("prompt profiles = %#v, want no review profiles", prompt.Context.ExistingConfig.Profiles)
			}
			draft := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
			draft.LLMProvider = string(config.LLMProviderOpenAI)
			draft.LLMAuth = string(config.LLMAuthSubscription)
			draft.LLMAdapter = string(config.LLMAdapterCodexCLI)
			return draft, nil
		}),
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			t.Fatalf("finalize prompter should not run for profileless LLM-runtime commit: %#v", prompt)
			return initFinalizeActionCancel, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if llmCalls != 1 {
		t.Fatalf("llmCalls = %d, want one staged runtime edit", llmCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu before category entry and after stage", menu.prompts)
	}
	if !menu.prompts[0].CanConfigureLLM || menu.prompts[0].CanConfigureReviewer || menu.prompts[0].CanSave {
		t.Fatalf("initial prompt = %#v, want LLM enabled and reviewer/save disabled", menu.prompts[0])
	}
	if !menu.prompts[1].CanSave {
		t.Fatalf("post-stage menu prompt = %#v, want staged runtime edit to be saveable", menu.prompts[1])
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("profiles = %#v, want none", cfg.Profiles)
	}
	runtime, ok := cfg.LLMRuntimes["codex-cli"]
	if !ok {
		t.Fatalf("llm_runtimes = %#v, want codex-cli", cfg.LLMRuntimes)
	}
	if runtime.Provider != config.LLMProviderOpenAI || runtime.Auth != config.LLMAuthSubscription || runtime.Adapter != config.LLMAdapterCodexCLI {
		t.Fatalf("codex-cli runtime = %#v, want OpenAI subscription Codex CLI", runtime)
	}
}

func TestInitInteractiveMenuFocusedLLMRuntimeDeleteUndoReturnsToMenu(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionLLMRuntimes,
			initMenuActionExit,
		},
	}
	llmCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmCalls++
			switch llmCalls {
			case 1:
				if _, ok := prompt.Context.LLMRuntimes["claude-cli"]; !ok {
					t.Fatalf("LLMRuntimes = %#v, want configured claude-cli before delete", prompt.Context.LLMRuntimes)
				}
				return initDraft{
					Action:       initDraftActionDeleteLLMRuntime,
					ActionTarget: "claude-cli",
					LLMProvider:  string(config.LLMProviderOpenAI),
					LLMAuth:      string(config.LLMAuthSubscription),
					LLMAdapter:   string(config.LLMAdapterCodexCLI),
				}, nil
			case 2:
				if _, ok := prompt.Context.PendingLLMRuntimeDeletes["claude-cli"]; !ok {
					t.Fatalf("PendingLLMRuntimeDeletes = %#v, want claude-cli pending delete before undo", prompt.Context.PendingLLMRuntimeDeletes)
				}
				return initDraft{
					Action:       initDraftActionUndoDeleteLLMRuntime,
					ActionTarget: "claude-cli",
				}, nil
			case 3:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected LLM prompt #%d", llmCalls)
				return initDraft{}, nil
			}
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if llmCalls != 3 {
		t.Fatalf("llmCalls = %d, want delete, undo, then back inside LLM category", llmCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu before LLM category and after backing out", menu.prompts)
	}
}

func TestInitInteractiveLLMRuntimeDeletePersistsReplacementAndReloadsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionLLMRuntimes,
			initMenuActionSave,
		},
	}
	llmCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmCalls++
			if llmCalls > 2 {
				t.Fatalf("unexpected LLM prompt #%d", llmCalls)
			}
			if llmCalls == 2 {
				return initDraft{}, errInitNavigateBack
			}
			if _, ok := prompt.Context.LLMRuntimes["claude-cli"]; !ok {
				t.Fatalf("LLMRuntimes = %#v, want configured claude-cli before delete", prompt.Context.LLMRuntimes)
			}
			return initDraft{
				Action:       initDraftActionDeleteLLMRuntime,
				ActionTarget: "claude-cli",
				LLMProvider:  string(config.LLMProviderOpenAI),
				LLMAuth:      string(config.LLMAuthSubscription),
				LLMAdapter:   string(config.LLMAdapterCodexCLI),
			}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps save: %v", err)
	}
	if llmCalls != 2 {
		t.Fatalf("llmCalls = %d, want staged runtime delete then category back", llmCalls)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	work := saved.Profiles["work"]
	if work.LLM.Provider != config.LLMProviderOpenAI || work.LLM.Auth != config.LLMAuthSubscription || work.LLM.Adapter != config.LLMAdapterCodexCLI {
		t.Fatalf("saved LLM = %#v, want Codex CLI subscription replacement", work.LLM)
	}
	runtimes, profileRuntimes := buildInitLLMRuntimeInventory(saved)
	if _, ok := runtimes["claude-cli"]; ok {
		t.Fatalf("saved runtime inventory = %#v, want deleted claude-cli absent", runtimes)
	}
	if _, ok := runtimes["codex-cli"]; !ok {
		t.Fatalf("saved runtime inventory = %#v, want replacement codex-cli present", runtimes)
	}
	if got := profileRuntimes["work"]; got != "codex-cli" {
		t.Fatalf("profile runtime = %q, want codex-cli after save", got)
	}

	reloadMenu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionLLMRuntimes,
			initMenuActionExit,
		},
	}
	reloadCalls := 0
	reloadDeps := initDeps{
		menuPrompter: reloadMenu,
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			reloadCalls++
			if len(prompt.Context.PendingLLMRuntimeDeletes) != 0 {
				t.Fatalf("PendingLLMRuntimeDeletes = %#v, want no stale staged deletion on reload", prompt.Context.PendingLLMRuntimeDeletes)
			}
			if _, ok := prompt.Context.LLMRuntimes["claude-cli"]; ok {
				t.Fatalf("reload LLMRuntimes = %#v, want deleted claude-cli absent", prompt.Context.LLMRuntimes)
			}
			if _, ok := prompt.Context.LLMRuntimes["codex-cli"]; !ok {
				t.Fatalf("reload LLMRuntimes = %#v, want codex-cli replacement present", prompt.Context.LLMRuntimes)
			}
			return initDraft{}, errInitNavigateBack
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called during reload-only verification")
			return nil
		},
	}
	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, reloadDeps); err != nil {
		t.Fatalf("runInitWithDeps reload: %v", err)
	}
	if reloadCalls != 1 {
		t.Fatalf("reloadCalls = %d, want one reload inventory prompt", reloadCalls)
	}
}

func TestInitInteractiveMenuFocusedReviewProfileStageStaysInCategoryUntilBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	profileCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		profileV2Prompter: initPrompterFunc(func(prompt initPromptContext) (initDraft, error) {
			profileCalls++
			switch profileCalls {
			case 1:
				draft := seedInteractiveInitDraft(prompt.RequestedProfileName, prompt.ExistingProfileName, prompt.ExistingProfile)
				draft.GitHost = "gitlab.com"
				draft.RoutesSet = true
				draft.Routes = []configedit.RepositoryRouteSpec{{
					Host:      "gitlab.com",
					Namespace: "open-cli-collective",
				}}
				return draft, nil
			case 2:
				if prompt.ExistingProfile == nil {
					t.Fatal("ExistingProfile = nil, want updated work profile on second focused pass")
				}
				if got := prompt.ExistingProfile.Git.Host; got != "gitlab.com" {
					t.Fatalf("ExistingProfile.Git.Host = %q, want staged gitlab.com host on second focused pass", got)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected profile prompt #%d", profileCalls)
				return initDraft{}, nil
			}
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatalf("routes prompter should not run from v2 profile editor path: %#v", prompt)
			return initRoutesEdit{}, nil
		}),
		modelMapPrompter: initModelMapPrompterFunc(func(initModelMapPrompt) (initModelMapEdit, error) {
			return initModelMapEdit{Apply: false}, nil
		}),
		agentSourcesPrompter: initAgentSourcesPrompterFunc(func(initAgentSourcesPrompt) (initAgentSourcesEdit, error) {
			return initAgentSourcesEdit{Apply: false}, nil
		}),
		reviewPolicyPrompter: initReviewPolicyPrompterFunc(func(initReviewPolicyPrompt) (initReviewPolicyEdit, error) {
			return initReviewPolicyEdit{Apply: false}, nil
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
	if profileCalls != 2 {
		t.Fatalf("profileCalls = %d, want staged profile edit then Back in-category", profileCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu only before category entry and after explicit Back", menu.prompts)
	}
}

func TestInitInteractiveMenuFocusedReviewProfileDoesNotRunRouteSubprompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
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
	profileCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		profileV2Prompter: initPrompterFunc(func(prompt initPromptContext) (initDraft, error) {
			profileCalls++
			switch profileCalls {
			case 1:
				draft := seedInteractiveInitDraft(prompt.RequestedProfileName, prompt.ExistingProfileName, prompt.ExistingProfile)
				draft.GitHost = "gitlab.com"
				draft.RoutesSet = true
				draft.Routes = []configedit.RepositoryRouteSpec{{
					Host:      "gitlab.com",
					Namespace: "open-cli-collective",
				}}
				return draft, nil
			case 2:
				if prompt.ExistingProfile == nil {
					t.Fatal("ExistingProfile = nil, want staged work profile on second focused pass")
				}
				if got := prompt.ExistingProfile.Git.Host; got != "gitlab.com" {
					t.Fatalf("ExistingProfile.Git.Host = %q, want staged gitlab.com host after route Back", got)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected profile prompt #%d", profileCalls)
				return initDraft{}, nil
			}
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatalf("routes prompter should not run from v2 profile editor path: %#v", prompt)
			return initRoutesEdit{}, nil
		}),
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if profileCalls != 2 {
		t.Fatalf("profileCalls = %d, want v2 stage to reopen review-profile flow before explicit Back", profileCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want main menu only before category entry and after explicit Back", menu.prompts)
	}
}

func TestInitInteractiveLegacyInjectedPathProfileEditRemainsOneShot(t *testing.T) {
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
		prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			return initDraft{
				ProfileName: "default",
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
			return newFakeInitStore(nil), nil
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
	if prompterCalls != 1 {
		t.Fatalf("prompterCalls = %d, want legacy injected path to remain one-shot", prompterCalls)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if _, ok := cfg.Profiles["default"]; !ok {
		t.Fatalf("profiles = %#v, want suggested profile", cfg.Profiles)
	}
}

func TestInitInteractiveMenuDeleteUndoAndSaveFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	work := basicProfile("work")
	work.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	home := basicProfile("home")
	home.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{
			"home": home,
			"work": work,
		},
	})
	var stdout bytes.Buffer
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
		Profile:    "work",
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionReviewProfiles,
			initMenuActionReviewerEntities,
			initMenuActionLLMRuntimes,
			initMenuActionSave,
		},
	}
	profileEdits := 0
	reviewerEdits := 0
	llmEdits := 0
	deps := initDeps{
		menuPrompter: menu,
		profileV2Prompter: initPrompterFunc(func(prompt initPromptContext) (initDraft, error) {
			profileEdits++
			switch profileEdits {
			case 1:
				if got := prompt.ExistingProfileName; got != "work" {
					t.Fatalf("first profile prompt ExistingProfileName = %q, want work", got)
				}
				return initDraft{
					Action:       initDraftActionDeleteProfile,
					ActionTarget: "work",
				}, nil
			case 2:
				if got := prompt.ExistingProfileName; got != "home" {
					t.Fatalf("second profile prompt ExistingProfileName = %q, want home after deleting active work profile", got)
				}
				if _, ok := prompt.PendingProfileDeletes["work"]; !ok {
					t.Fatalf("PendingProfileDeletes = %#v, want work pending delete before undo", prompt.PendingProfileDeletes)
				}
				return initDraft{
					Action:       initDraftActionUndoDeleteProfile,
					ActionTarget: "work",
				}, nil
			case 3:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected extra profile prompt #%d", profileEdits)
				return initDraft{}, nil
			}
		}),
		reviewerPrompter: initReviewerEntityPrompterFunc(func(prompt initReviewerEntityPrompt) (initDraft, error) {
			reviewerEdits++
			switch reviewerEdits {
			case 1:
				if _, ok := prompt.Context.ReviewerEntities["reviewer-github-app"]; !ok {
					t.Fatalf("ReviewerEntities = %#v, want configured reviewer-github-app inventory entry", prompt.Context.ReviewerEntities)
				}
				return initDraft{
					Action:       initDraftActionDeleteReviewerEntity,
					ActionTarget: "reviewer-github-app",
				}, nil
			case 2:
				if _, ok := prompt.Context.PendingReviewerEntityDeletes["reviewer-github-app"]; !ok {
					t.Fatalf("PendingReviewerEntityDeletes = %#v, want reviewer-github-app pending delete before leaving category", prompt.Context.PendingReviewerEntityDeletes)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected reviewer prompt #%d", reviewerEdits)
				return initDraft{}, nil
			}
		}),
		llmRuntimePrompter: initLLMRuntimePrompterFunc(func(prompt initLLMRuntimePrompt) (initDraft, error) {
			llmEdits++
			switch llmEdits {
			case 1:
				if _, ok := prompt.Context.LLMRuntimes["claude-cli"]; !ok {
					t.Fatalf("LLMRuntimes = %#v, want configured claude-cli runtime", prompt.Context.LLMRuntimes)
				}
				return initDraft{
					Action:       initDraftActionDeleteLLMRuntime,
					ActionTarget: "claude-cli",
					LLMProvider:  string(config.LLMProviderOpenAI),
					LLMAuth:      string(config.LLMAuthSubscription),
					LLMAdapter:   string(config.LLMAdapterCodexCLI),
				}, nil
			case 2:
				if _, ok := prompt.Context.PendingLLMRuntimeDeletes["claude-cli"]; !ok {
					t.Fatalf("PendingLLMRuntimeDeletes = %#v, want claude-cli pending delete before leaving category", prompt.Context.PendingLLMRuntimeDeletes)
				}
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected LLM prompt #%d", llmEdits)
				return initDraft{}, nil
			}
		}),
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionSave, nil
		}),
		configPath:         func(*root.Options) (string, error) { return path, nil },
		loadConfig:         loadConfigForInit,
		saveConfig:         config.Save,
		clipboardSupported: func() bool { return false },
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(map[string]map[string]string{
				"home": {credentials.GitTokenKey: "home-token"},
				"work": {credentials.GitTokenKey: "work-token"},
			}), nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}

	if len(menu.prompts) != 4 {
		t.Fatalf("menu prompts = %#v, want one menu pass after each focused category", menu.prompts)
	}
	if got := menu.prompts[1].ReviewProfileCount; got != 2 {
		t.Fatalf("review profile count after delete+undo category = %d, want both profiles restored", got)
	}
	if got := menu.prompts[2].ReviewerEntityCount; got != 0 {
		t.Fatalf("reviewer entity count after delete = %d, want zero configured separate reviewers after fallback", got)
	}
	if profileEdits != 3 {
		t.Fatalf("profileEdits = %d, want delete, undo, then Back sequence", profileEdits)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	for _, name := range []string{"home", "work"} {
		profile := saved.Profiles[name]
		if profile.ReviewerCredentials != nil {
			t.Fatalf("%s reviewer credentials = %#v, want deleted reviewer entity to fall back to git identity", name, profile.ReviewerCredentials)
		}
		if profile.LLM.Provider != config.LLMProviderOpenAI || profile.LLM.Auth != config.LLMAuthSubscription || profile.LLM.Adapter != config.LLMAdapterCodexCLI {
			t.Fatalf("%s llm = %#v, want codex subscription replacement", name, profile.LLM)
		}
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
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				return initDraft{
					ProfileName: "home",
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				return initDraft{}, errInitNavigateBack
			case 3:
				return initDraft{
					ProfileName: "work",
					GitHost:     "gitlab.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 4:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionDefer,
				initCredentialSecretActionDefer,
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
	if !strings.Contains(stdout.String(), "Saved staged init changes") || !strings.Contains(stdout.String(), "- review profiles: 2") || !strings.Contains(stdout.String(), "- home: needs follow-up") || !strings.Contains(stdout.String(), "- work: needs follow-up") {
		t.Fatalf("stdout = %q, want readiness summary for both profiles", stdout.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/home --key "+credentials.GitTokenKey) || !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/work --key "+credentials.GitTokenKey) {
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
				initMenuActionSave,
			},
		},
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			finalizePrompt = prompt
			return initFinalizeActionSave, nil
		}),
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return initDraft{
				ProfileName: "default",
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
		t.Fatalf("profile readiness = %#v, want ready suggested profile with no notes", profileReadiness)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "git-token" {
		t.Fatalf("stored git token = %q, want git-token from interactive SetNow flow", got)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Profiles["default"].Git.CredentialRef != "codereview/default" {
		t.Fatalf("suggested profile git ref = %q, want codereview/default", cfg.Profiles["default"].Git.CredentialRef)
	}
	if !strings.Contains(stdout.String(), "Saved staged init changes") || !strings.Contains(stdout.String(), "- review profiles: 1") || !strings.Contains(stdout.String(), "- credential secrets: 1 name") || !strings.Contains(stdout.String(), "- default: ready") {
		t.Fatalf("stdout = %q, want ready summary for suggested profile", stdout.String())
	}
	if strings.Contains(stderr.String(), "set-credential --store") {
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
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			switch prompterCalls {
			case 1:
				return initDraft{
					ProfileName: "home",
					GitHost:     "github.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 2:
				return initDraft{}, errInitNavigateBack
			case 3:
				return initDraft{
					ProfileName: "work",
					GitHost:     "gitlab.com",
					GitAuth:     string(config.GitAuthModePAT),
					LLMProvider: string(config.LLMProviderAnthropic),
					LLMAuth:     string(config.LLMAuthSubscription),
					LLMAdapter:  string(config.LLMAdapterClaudeCLI),
				}, nil
			case 4:
				return initDraft{}, errInitNavigateBack
			default:
				t.Fatalf("unexpected prompter call %d", prompterCalls)
				return initDraft{}, nil
			}
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{
				initCredentialSecretActionSetNow,
				initCredentialSecretActionDefer,
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
	if strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/home --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want no follow-up hint for ready home profile", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/work --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want follow-up hint for deferred work profile", stderr.String())
	}
}

func TestInitInteractiveMenuGlobalSettingsOnlySaveDoesNotFinalizeBootstrappedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Keyring: config.KeyringConfig{Backend: "memory"},
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
				Apply: true,
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
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
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

func TestInitInteractiveMenuSecretsManagementOnlySaveDoesNotFinalizeBootstrappedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Keyring: config.KeyringConfig{Backend: "memory"},
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
				initMenuActionSecretsManagement,
				initMenuActionSave,
			},
		},
		keyringPrompter: initKeyringBackendPrompterFunc(func(initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			return initKeyringBackendEdit{
				Apply: true,
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
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
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

func TestInitInteractiveMenuFinalizeBackReturnsToMenu(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Profiles: map[string]config.Profile{"work": basicProfile("work")},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionSave,
			initMenuActionExit,
		},
	}
	deps := initDeps{
		menuPrompter: menu,
		finalizePrompter: initFinalizePrompterFunc(func(initFinalizePrompt) (initFinalizeAction, error) {
			return initFinalizeActionBack, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig should not run after finalize Back then exit")
			return nil
		},
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if len(menu.prompts) < 2 {
		t.Fatalf("menu prompts = %#v, want main menu shown again after finalize Back", menu.prompts)
	}
}

func TestInitInteractiveMenuCancelAfterSecretEntryBeforeFinalSaveWritesNothing(t *testing.T) {
	store := newFakeInitStore(nil)
	prompterCalls := 0
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
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			prompterCalls++
			if prompterCalls > 1 {
				return initDraft{}, errInitNavigateBack
			}
			return initDraft{
				ProfileName: "default",
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
		profileV2Prompter: initPrompterFunc(func(initPromptContext) (initDraft, error) {
			return initDraft{
				ProfileName: "default",
				GitHost:     "github.com",
				GitAuth:     string(config.GitAuthModePAT),
				LLMProvider: string(config.LLMProviderAnthropic),
				LLMAuth:     string(config.LLMAuthSubscription),
				LLMAdapter:  string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionSetNow},
		},
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

func TestInitInteractiveMenuCanCommitSecretsStorageBeforeReviewProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionSecretsManagement,
			initMenuActionSave,
		},
	}
	keyringCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		keyringPrompter: initKeyringBackendPrompterFunc(func(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			keyringCalls++
			next := cloneInitConfigFile(prompt.Config)
			next.Secrets = config.SecretsConfig{
				Stores: map[string]config.SecretsStore{
					"personal-file": {
						DisplayName: "Personal file",
						Backend: config.SecretsStoreBackend{
							Kind: config.SecretsBackendKind(credstore.BackendFile),
						},
					},
				},
			}
			return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: next}, nil
		}),
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			t.Fatalf("finalize prompter should not run for profileless secrets-storage commit: %#v", prompt)
			return initFinalizeActionCancel, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run when only credential-store config changed")
			return nil, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if keyringCalls != 1 {
		t.Fatalf("keyringCalls = %d, want 1", keyringCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want before and after secrets-storage edit", menu.prompts)
	}
	if menu.prompts[0].CanSave {
		t.Fatalf("initial prompt = %#v, want commit disabled before staged changes", menu.prompts[0])
	}
	if !menu.prompts[1].CanSave || menu.prompts[1].CanConfigureLLM || menu.prompts[1].CanConfigureReviewer {
		t.Fatalf("post-edit prompt = %#v, want only commit enabled without workspace", menu.prompts[1])
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", cfg.Profiles)
	}
	store, ok := cfg.Secrets.Stores["personal-file"]
	if !ok {
		t.Fatalf("stores = %#v, want personal-file", cfg.Secrets.Stores)
	}
	if store.DisplayName != "Personal file" || store.Backend.Kind != config.SecretsBackendKind(credstore.BackendFile) {
		t.Fatalf("store = %#v, want named file backend", store)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	legacyProfileKey := "default" + "_profile"
	if strings.Contains(string(body), legacyProfileKey) || strings.Contains(string(body), "profiles:") {
		t.Fatalf("saved profileless secrets-storage config contains profile fields:\n%s", string(body))
	}
}

func TestInitInteractiveMenuCanCommitDeletingLastSecretsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"personal-file": {
					DisplayName: "Personal file",
					Backend: config.SecretsStoreBackend{
						Kind: config.SecretsBackendKind(credstore.BackendFile),
					},
				},
			},
		},
		Profiles: map[string]config.Profile{},
	})
	opts := &root.Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		ConfigPath: path,
	}
	menu := &fakeInitMenuPrompter{
		actions: []initMenuAction{
			initMenuActionSecretsManagement,
			initMenuActionSave,
		},
	}
	keyringCalls := 0
	deps := initDeps{
		menuPrompter: menu,
		keyringPrompter: initKeyringBackendPrompterFunc(func(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
			keyringCalls++
			next := cloneInitConfigFile(prompt.Config)
			next.Secrets = config.SecretsConfig{}
			return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: next}, nil
		}),
		finalizePrompter: initFinalizePrompterFunc(func(prompt initFinalizePrompt) (initFinalizeAction, error) {
			t.Fatalf("finalize prompter should not run for profileless secrets-storage deletion: %#v", prompt)
			return initFinalizeActionCancel, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("openStore should not run when only credential-store config changed")
			return nil, nil
		},
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
	}

	if err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps); err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	if keyringCalls != 1 {
		t.Fatalf("keyringCalls = %d, want 1", keyringCalls)
	}
	if len(menu.prompts) != 2 {
		t.Fatalf("menu prompts = %#v, want before and after secrets-storage delete", menu.prompts)
	}
	if menu.prompts[0].CanSave {
		t.Fatalf("initial prompt = %#v, want commit disabled before staged changes", menu.prompts[0])
	}
	if !menu.prompts[1].CanSave {
		t.Fatalf("post-delete prompt = %#v, want commit enabled after deleting credential store", menu.prompts[1])
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("loaded profiles = %#v, want none", cfg.Profiles)
	}
	if len(cfg.Secrets.Stores) != 0 {
		t.Fatalf("loaded stores = %#v, want deleted store removed", cfg.Secrets.Stores)
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
				"broken": {},
			},
		},
		profileNames: []string{"broken"},
		writes: map[string]map[string]string{
			"codereview/broken": {credentials.GitTokenKey: "git-token"},
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
	if err == nil || !strings.Contains(err.Error(), "profiles.broken.git.host is required") {
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
		cfg:          config.File{Profiles: map[string]config.Profile{"default": profile}},
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
	if err == nil || !strings.Contains(err.Error(), "credential names needing cleanup: [codereview/default]") {
		t.Fatalf("error = %v, want cleanup hint after config save failure", err)
	}
	if got := store.bundles["default"][credentials.GitTokenKey]; got != "git-token" {
		t.Fatalf("stored git token = %q, want git-token before config save failure", got)
	}
}

func TestApplyInteractiveInitSessionPlanSummarizesSecretsStorageOnlyChanges(t *testing.T) {
	var stdout bytes.Buffer
	opts := &root.Options{
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
	}
	profile := basicProfile("default")
	original := config.File{
		Profiles: map[string]config.Profile{"default": profile},
	}
	next := original
	next.Secrets = config.SecretsConfig{
		Profiles: map[string]config.SecretsProfile{
			"one-password": {
				Label: "1Password",
				Backend: config.SecretsProfileBackend{
					Kind: config.SecretsBackendKind(credstore.BackendOPDesktop),
					OnePassword: &config.SecretsProfileOnePasswordConfig{
						VaultID: "Personal",
					},
				},
			},
		},
	}
	plan := initSessionPlan{
		path:        filepath.Join(t.TempDir(), "config.yml"),
		originalCfg: original,
		cfg:         next,
	}
	err := applyInteractiveInitSessionPlan(opts, initDeps{
		saveConfig: func(string, config.File) error { return nil },
	}, plan)
	if err != nil {
		t.Fatalf("applyInteractiveInitSessionPlan: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Saved staged init changes") || !strings.Contains(out, "- secrets storage: 1 store") {
		t.Fatalf("stdout = %q, want secrets-storage summary", out)
	}
	if strings.Contains(out, "Initialized 0 profile(s)") {
		t.Fatalf("stdout = %q, want no profile-only initialization summary", out)
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
	if err == nil || !strings.Contains(err.Error(), "credential names needing cleanup: [codereview/a codereview/b]") {
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

func TestApplyInteractiveInitSessionPlanWritesSeparateSecretsProfilesIndependently(t *testing.T) {
	homeStore := newFakeInitStore(nil)
	workStore := newFakeInitStore(nil)
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"personal-memory": {
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)},
				},
				"work-file": {
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": func() config.Profile {
				p := basicProfile("home")
				p.SecretsProfile = "personal-memory"
				return p
			}(),
			"work": func() config.Profile {
				p := basicProfile("work")
				p.SecretsProfile = "work-file"
				return p
			}(),
		},
	}
	homeResolved, err := credentials.ResolveSecretsProfileForProfile(cfg, cfg.Profiles["home"])
	if err != nil {
		t.Fatalf("Resolve home secrets profile: %v", err)
	}
	workResolved, err := credentials.ResolveSecretsProfileForProfile(cfg, cfg.Profiles["work"])
	if err != nil {
		t.Fatalf("Resolve work secrets profile: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"home", "work"},
		writes: map[string]map[string]string{
			"codereview/home": {credentials.GitTokenKey: "home-token"},
			"codereview/work": {credentials.GitTokenKey: "work-token"},
		},
		credentialPlan: []initCredentialPlanEntry{
			{Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/home", Mode: string(config.GitAuthModePAT)}, SecretsProfile: homeResolved},
			{Ref: config.CredentialRef{Purpose: "git", Ref: "codereview/work", Mode: string(config.GitAuthModePAT)}, SecretsProfile: workResolved},
		},
	}
	var opened []string
	err = applyInteractiveInitSessionPlan(opts, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore called for named secrets-profile writes")
			return nil, nil
		},
		openResolvedStore: func(resolved credentials.ResolvedSecretsProfile, _ string, _ bool, _ config.File) (initStore, error) {
			opened = append(opened, resolved.ID)
			switch resolved.ID {
			case "personal-memory":
				return homeStore, nil
			case "work-file":
				return workStore, nil
			default:
				t.Fatalf("unexpected resolved secrets profile %q", resolved.ID)
				return nil, nil
			}
		},
		saveConfig: func(string, config.File) error { return nil },
	}, plan)
	if err != nil {
		t.Fatalf("applyInteractiveInitSessionPlan: %v", err)
	}
	if !reflect.DeepEqual(opened, []string{"personal-memory", "work-file"}) {
		t.Fatalf("opened secrets profiles = %#v, want home then work", opened)
	}
	if got := homeStore.bundles["home"][credentials.GitTokenKey]; got != "home-token" {
		t.Fatalf("home store token = %q, want home-token", got)
	}
	if got := workStore.bundles["work"][credentials.GitTokenKey]; got != "work-token" {
		t.Fatalf("work store token = %q, want work-token", got)
	}
}

func TestApplyInteractiveInitSessionPlanWritesReviewerSecretsToResolvedStore(t *testing.T) {
	workStore := newFakeInitStore(nil)
	profile := basicProfile("work")
	profile.SecretsProfile = "work-file"
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"work-file": {
					Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{"work": profile},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
	if err != nil {
		t.Fatalf("Resolve work secrets profile: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"work"},
		writes: map[string]map[string]string{
			"codereview/work-reviewer": {
				credentials.GitHubAppIDKey:         "12345",
				credentials.GitHubAppPrivateKeyKey: "private-key",
			},
		},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			SecretsProfile: resolved,
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false},
			},
			State: initCredentialPlanStateWrite,
		}},
	}

	err = applyInteractiveInitSessionPlan(&root.Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore called for named reviewer secrets")
			return nil, nil
		},
		openResolvedStore: func(resolved credentials.ResolvedSecretsProfile, _ string, _ bool, _ config.File) (initStore, error) {
			if resolved.ID != "work-file" {
				t.Fatalf("opened secrets profile %q, want work-file", resolved.ID)
			}
			return workStore, nil
		},
		saveConfig: func(string, config.File) error { return nil },
	}, plan)
	if err != nil {
		t.Fatalf("applyInteractiveInitSessionPlan: %v", err)
	}
	if got := workStore.bundles["work-reviewer"][credentials.GitHubAppIDKey]; got != "12345" {
		t.Fatalf("stored reviewer app id = %q, want named-store value", got)
	}
	if got := workStore.bundles["work-reviewer"][credentials.GitHubAppPrivateKeyKey]; got != "private-key" {
		t.Fatalf("stored reviewer private key = %q, want named-store value", got)
	}
}

func TestApplyInteractiveInitSessionPlanNamedSecretsProfileWriteFailureStopsConfigSave(t *testing.T) {
	store := newFakeInitStore(nil)
	store.setBundleFunc = func(string, map[string]string, ...credstore.SetOpt) (credstore.Result, error) {
		return credstore.Result{}, errors.New("backend vault unreachable")
	}
	opts := &root.Options{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	cfg := config.File{
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"work-1password": {
					Label: "Work 1Password",
					Backend: config.SecretsProfileBackend{
						Kind: config.SecretsBackendKind(credstore.BackendOPDesktop),
						OnePassword: &config.SecretsProfileOnePasswordConfig{
							VaultID: "Not A Real Vault",
						},
					},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"work": func() config.Profile {
				p := basicProfile("work")
				p.SecretsProfile = "work-1password"
				return p
			}(),
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, cfg.Profiles["work"])
	if err != nil {
		t.Fatalf("Resolve work secrets profile: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"work"},
		writes: map[string]map[string]string{
			"codereview/work": {credentials.GitTokenKey: "work-token"},
		},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "git",
				Ref:     "codereview/work",
				Mode:    string(config.GitAuthModePAT),
			},
			SecretsProfile: resolved,
		}},
	}
	err = applyInteractiveInitSessionPlan(opts, initDeps{
		openResolvedStore: func(resolved credentials.ResolvedSecretsProfile, _ string, _ bool, _ config.File) (initStore, error) {
			if resolved.ID != "work-1password" {
				t.Fatalf("opened secrets profile %q, want work-1password", resolved.ID)
			}
			return store, nil
		},
		openStore: func(string, bool, config.File) (initStore, error) {
			t.Fatal("legacy openStore called for named secrets-profile write")
			return nil, nil
		},
		saveConfig: func(string, config.File) error {
			t.Fatal("saveConfig called despite backend write failure")
			return nil
		},
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "backend vault unreachable") {
		t.Fatalf("applyInteractiveInitSessionPlan error = %v, want backend write failure", err)
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
	if len(cfg.Profiles) != 1 {
		t.Fatalf("config = %#v, want non-interactive profile saved", cfg)
	}
	if _, ok := cfg.Profiles["default"]; !ok {
		t.Fatalf("profiles = %#v, want suggested profile saved", cfg.Profiles)
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
			edit: initRoutesEdit{Routes: []configedit.RepositoryRouteSpec{{
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
			edit: initRoutesEdit{Routes: []configedit.RepositoryRouteSpec{{
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
			edit: initRoutesEdit{Routes: nil},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			saveCredentialTestConfig(t, path, config.File{
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

func TestInitInteractiveProfileDraftRoutesSkipRouteSubprompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
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
			draft := seedInteractiveInitDraft("work", "work", nil)
			draft.GitHost = "github.com"
			draft.GitAuth = string(config.GitAuthModePAT)
			draft.GitCredentialRef = "codereview/work"
			draft.LLMProvider = string(config.LLMProviderAnthropic)
			draft.LLMAuth = string(config.LLMAuthSubscription)
			draft.LLMAdapter = string(config.LLMAdapterClaudeCLI)
			draft.RoutesSet = true
			draft.Routes = []configedit.RepositoryRouteSpec{{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			}}
			return draft, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(initRoutesPrompt) (initRoutesEdit, error) {
			t.Fatal("routesPrompter called despite inline profile routes")
			return initRoutesEdit{}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
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
	wantRoutes := []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
	}}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, wantRoutes) {
		t.Fatalf("RepositoryProfiles = %#v, want inline draft routes applied", cfg.RepositoryProfiles)
	}
}

func TestInitInteractiveRouteEditorPreservesExistingRoutesWhenLeftUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	wantRoutes := []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "open-cli-collective",
		},
	}}
	saveCredentialTestConfig(t, path, config.File{
		RepositoryProfiles: wantRoutes,
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
				GitHost:             "github.com",
				GitAuth:             string(config.GitAuthModePAT),
				GitCredentialRef:    "codereview/work",
				LLMProvider:         string(config.LLMProviderAnthropic),
				LLMAuth:             string(config.LLMAuthSubscription),
				LLMAdapter:          string(config.LLMAdapterClaudeCLI),
			}, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if !reflect.DeepEqual(prompt.Routes, currentProfileRouteSpecs(wantRoutes, "work")) {
				t.Fatalf("prompt.Routes = %#v, want prefilled current routes", prompt.Routes)
			}
			return initRoutesEdit{Routes: prompt.Routes}, nil
		}),
		secretPrompter: &fakeInitSecretPrompter{
			actions: []initCredentialSecretAction{initCredentialSecretActionDefer},
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
	if !reflect.DeepEqual(cfg.RepositoryProfiles, wantRoutes) {
		t.Fatalf("RepositoryProfiles = %#v, want %#v", cfg.RepositoryProfiles, wantRoutes)
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

	shorthandURLSpec, err := parseInitRouteSpec("github.com/YourOrg/org-repo/pull/123")
	if err != nil {
		t.Fatalf("parseInitRouteSpec shorthand PR URL: %v", err)
	}
	if !reflect.DeepEqual(shorthandURLSpec, configedit.RepositoryRouteSpec{
		Host:      "github.com",
		Namespace: "YourOrg",
		Repos:     []string{"org-repo"},
	}) {
		t.Fatalf("shorthand PR URL spec = %#v", shorthandURLSpec)
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

func TestInitInteractiveRouteSpecsUseSemicolonDisplay(t *testing.T) {
	got := formatInitRouteSpecs([]configedit.RepositoryRouteSpec{
		{
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
		{
			Host:      "github.com",
			Namespace: "rianjs",
		},
	})
	want := "github.com/open-cli-collective [codereview-cli]; github.com/rianjs"
	if got != want {
		t.Fatalf("formatInitRouteSpecs = %q, want %q", got, want)
	}
}

func TestInitInteractiveRouteParsersAcceptSemicolonsNewlinesAndDeduplicate(t *testing.T) {
	specs, err := parseInitRouteSpecs(strings.Join([]string{
		"https://github.com/open-cli-collective/codereview-cli/pull/185; GitHub.com/rianjs [RepoB, RepoA, RepoA]",
		"github.com/open-cli-collective/codereview-cli",
	}, "\n"))
	if err != nil {
		t.Fatalf("parseInitRouteSpecs: %v", err)
	}
	want := []configedit.RepositoryRouteSpec{
		{
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
		{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"RepoA", "RepoB"},
		},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("parseInitRouteSpecs = %#v, want %#v", specs, want)
	}
}

func TestInitInteractiveReconcilesRouteHostChangeBeforeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
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
			return initRoutesEdit{Routes: []configedit.RepositoryRouteSpec{{
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
			draft := seedInteractiveInitDraft("work", "work", &work)
			applyGitScopeSelection(&draft, profileScopeNames["office"], scopes)
			return draft, nil
		}),
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if !prompt.HostChanged || prompt.PreviousHost != "github.com" || prompt.ProfileHost != "gitlab.com" {
				t.Fatalf("prompt = %#v, want selected git scope reconciliation context", prompt)
			}
			return initRoutesEdit{Routes: []configedit.RepositoryRouteSpec{{
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
			return initRoutesEdit{Routes: []configedit.RepositoryRouteSpec{{
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

func TestInitInteractiveRejectsUnchangedStaleRoutesAfterHostChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	saveCredentialTestConfig(t, path, config.File{
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
		routesPrompter: initRoutesPrompterFunc(func(prompt initRoutesPrompt) (initRoutesEdit, error) {
			if !prompt.HostChanged || prompt.PreviousHost != "github.com" || prompt.ProfileHost != "gitlab.com" {
				t.Fatalf("prompt = %#v, want host reconciliation context", prompt)
			}
			return initRoutesEdit{Routes: prompt.Routes}, nil
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
	if !strings.Contains(err.Error(), `route host "github.com" does not match selected profile host "gitlab.com"`) {
		t.Fatalf("error = %v, want mismatched route host rejection", err)
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

func TestInitInteractiveDeferredLLMHintUsesExplicitLocalOSStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	opts := &root.Options{
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
			return initKeyringBackendEdit{Apply: true}, nil
		}),
		openStore: func(string, bool, config.File) (initStore, error) {
			return newFakeInitStore(nil), nil
		},
		readSecret: func(io.Reader, bool, string, string, string) (string, bool, error) {
			t.Fatal("interactive deferred llm init should not read secret ingress")
			return "", false, nil
		},
	}

	err := runInitWithDeps(&cobra.Command{}, opts, initOptions{}, deps)
	if err != nil {
		t.Fatalf("runInitWithDeps: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Keyring.Backend != "" {
		t.Fatalf("keyring.backend = %q, want empty", cfg.Keyring.Backend)
	}
	if strings.Contains(stderr.String(), "--backend") {
		t.Fatalf("stderr = %q, want no backend flag in deferred credential hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cr set-credential --store local-os --name codereview/default-llm --key "+credentials.OpenAIAPIKeyKey+" --stdin") {
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
	if strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/default --key "+credentials.GitTokenKey) {
		t.Fatalf("stderr = %q, want no stale follow-up hint after collected secret", stderr.String())
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
	if !strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/default --key "+credentials.GitTokenKey+" --stdin") {
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
	if strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/default --key "+credentials.GitTokenKey) {
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
	if strings.Contains(stderr.String(), "set-credential --store local-os --name codereview/default-llm --key "+credentials.OpenAIAPIKeyKey) {
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
		{name: "obsolete keyring backend flags", args: []string{"init", "--non-interactive", "--keyring-backend", "file", "--reset-keyring-backend"}},
		{name: "runtime backend with obsolete durable backend flag", args: []string{"--backend", "memory", "init", "--non-interactive", "--keyring-backend", "file"}},
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
	if !strings.Contains(err.Error(), "credential names needing cleanup: [codereview/a]") {
		t.Fatalf("error = %v, want cleanup ref", err)
	}
}

func TestWriteBundlesLeavesStaleReviewerKeysForPostSaveCleanup(t *testing.T) {
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendMemory,
	})
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer store.Close()
	if _, err := store.SetBundle("work-reviewer", map[string]string{credentials.GitTokenKey: "old-reviewer-pat"}); err != nil {
		t.Fatalf("seed reviewer PAT bundle: %v", err)
	}
	written, err := writeBundles(store, map[string]map[string]string{
		"codereview/work-reviewer": {
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	}, false, nil)
	if err != nil {
		t.Fatalf("writeBundles: %v", err)
	}
	if !reflect.DeepEqual(written, []string{"codereview/work-reviewer"}) {
		t.Fatalf("written refs = %#v, want work reviewer", written)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitTokenKey); err != nil || !ok {
		t.Fatalf("stale git_token exists = %t, err = %v; want retained until config save succeeds", ok, err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitHubAppIDKey); err != nil || !ok {
		t.Fatalf("github_app_id exists = %t, err = %v; want written", ok, err)
	}
}

func TestWriteBundlesKeepsStaleReviewerKeysRequiredByAnotherActiveMode(t *testing.T) {
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendMemory,
	})
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer store.Close()
	if _, err := store.SetBundle("shared-reviewer", map[string]string{credentials.GitTokenKey: "existing-pat"}); err != nil {
		t.Fatalf("seed shared reviewer PAT bundle: %v", err)
	}
	if _, err := writeBundles(store, map[string]map[string]string{
		"codereview/shared-reviewer": {
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	}, false, nil); err != nil {
		t.Fatalf("writeBundles: %v", err)
	}
	if ok, err := store.Exists("shared-reviewer", credentials.GitTokenKey); err != nil || !ok {
		t.Fatalf("git_token exists = %t, err = %v; want retained for active PAT entry", ok, err)
	}
}

func TestApplyInteractiveInitSessionPlanDeletesStaleReviewerKeyWithoutWrites(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {
			credentials.GitTokenKey:                "old-reviewer-pat",
			credentials.GitHubAppIDKey:             "12345",
			credentials.GitHubAppPrivateKeyKey:     "private-key",
			credentials.GitHubAppInstallationIDKey: "67890",
		},
	})
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "work bot",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{"work": profile},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	refs, err := config.CredentialRefs(profile)
	if err != nil {
		t.Fatalf("CredentialRefs: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"work"},
		profileRefs:  map[string][]config.CredentialRef{"work": refs},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			PreviousRef: &config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			SecretsProfile: resolved,
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false},
			},
			State: initCredentialPlanStateKeepExisting,
		}},
	}
	var saved bool
	err = applyInteractiveInitSessionPlan(&root.Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error {
			saved = true
			return nil
		},
	}, plan)
	if err != nil {
		t.Fatalf("applyInteractiveInitSessionPlan: %v", err)
	}
	if !saved {
		t.Fatal("saveConfig was not called")
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitTokenKey); err != nil || ok {
		t.Fatalf("stale git_token exists = %t, err = %v; want deleted", ok, err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitHubAppIDKey); err != nil || !ok {
		t.Fatalf("github_app_id exists = %t, err = %v; want retained", ok, err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitHubAppPrivateKeyKey); err != nil || !ok {
		t.Fatalf("github_app_private_key exists = %t, err = %v; want retained", ok, err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitHubAppInstallationIDKey); err != nil || !ok {
		t.Fatalf("github_app_installation_id exists = %t, err = %v; want retained", ok, err)
	}
}

func TestApplyInteractiveInitSessionPlanSaveFailureDoesNotDeleteStaleReviewerKey(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {
			credentials.GitTokenKey:            "old-reviewer-pat",
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	})
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
	}
	cfg := config.File{Profiles: map[string]config.Profile{"work": profile}}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"work"},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			PreviousRef: &config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			SecretsProfile: resolved,
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false},
			},
			State: initCredentialPlanStateKeepExisting,
		}},
	}

	err = applyInteractiveInitSessionPlan(&root.Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, initDeps{
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error { return errors.New("disk full") },
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("applyInteractiveInitSessionPlan error = %v, want save failure", err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitTokenKey); err != nil || !ok {
		t.Fatalf("git_token exists = %t, err = %v; want retained because config save failed", ok, err)
	}
}

func TestApplyInteractiveInitSessionPlanKeepsSharedRefPATKeyAfterStagedAuthSwitchWrites(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"shared-reviewer": {credentials.GitTokenKey: "existing-pat"},
	})
	appProfile := basicProfile("app")
	appProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	patProfile := basicProfile("pat")
	patProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/shared-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"app": appProfile,
			"pat": patProfile,
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, appProfile)
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	plan := initSessionPlan{
		path:         filepath.Join(t.TempDir(), "config.yml"),
		cfg:          cfg,
		profileNames: []string{"app"},
		writes: map[string]map[string]string{
			"codereview/shared-reviewer": {
				credentials.GitHubAppIDKey:         "12345",
				credentials.GitHubAppPrivateKeyKey: "private-key",
			},
		},
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/shared-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			PreviousRef: &config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/shared-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			SecretsProfile: resolved,
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false},
			},
			State: initCredentialPlanStateWrite,
		}},
	}

	err = applyInteractiveInitSessionPlan(&root.Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, initDeps{
		openStore:  func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error { return nil },
	}, plan)
	if err != nil {
		t.Fatalf("applyInteractiveInitSessionPlan: %v", err)
	}
	if ok, err := store.Exists("shared-reviewer", credentials.GitTokenKey); err != nil || !ok {
		t.Fatalf("git_token exists = %t, err = %v; want retained for untouched PAT profile", ok, err)
	}
	if ok, err := store.Exists("shared-reviewer", credentials.GitHubAppIDKey); err != nil || !ok {
		t.Fatalf("github_app_id exists = %t, err = %v; want staged app key written", ok, err)
	}
}

func TestApplyInitPlanDeletesStaleReviewerKeyWithoutWrites(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"work-reviewer": {
			credentials.GitTokenKey:            "old-reviewer-pat",
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	})
	profile := basicProfile("work")
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/work-reviewer",
		DisplayName:   "work bot",
	}
	cfg := config.File{Profiles: map[string]config.Profile{"work": profile}}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, profile)
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	plan := initPlan{
		path:        filepath.Join(t.TempDir(), "config.yml"),
		cfg:         cfg,
		profileName: "work",
		profile:     profile,
		credentialPlan: []initCredentialPlanEntry{{
			Ref: config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModeGitHubApp),
			},
			PreviousRef: &config.CredentialRef{
				Purpose: "reviewer_credentials",
				Ref:     "codereview/work-reviewer",
				Mode:    string(config.GitAuthModePAT),
			},
			SecretsProfile: resolved,
			KeySpecs: []credentials.KeySpec{
				{Key: credentials.GitHubAppIDKey, Required: true},
				{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
				{Key: credentials.GitHubAppInstallationIDKey, Required: false},
			},
			State: initCredentialPlanStateKeepExisting,
		}},
	}

	err = applyInitPlan(&root.Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, initOptions{}, initDeps{
		openStore: func(string, bool, config.File) (initStore, error) { return store, nil },
		saveConfig: func(string, config.File) error {
			return nil
		},
	}, plan)
	if err != nil {
		t.Fatalf("applyInitPlan: %v", err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitTokenKey); err != nil || ok {
		t.Fatalf("stale git_token exists = %t, err = %v; want deleted", ok, err)
	}
	if ok, err := store.Exists("work-reviewer", credentials.GitHubAppIDKey); err != nil || !ok {
		t.Fatalf("github_app_id exists = %t, err = %v; want retained", ok, err)
	}
}

func TestStaleReviewerCleanupKeepsKeysRequiredByAnotherActiveModeWithoutWrites(t *testing.T) {
	store := newFakeInitStore(map[string]map[string]string{
		"shared-reviewer": {
			credentials.GitTokenKey:            "existing-pat",
			credentials.GitHubAppIDKey:         "12345",
			credentials.GitHubAppPrivateKeyKey: "private-key",
		},
	})
	appEntry := initCredentialPlanEntry{
		Ref: config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/shared-reviewer",
			Mode:    string(config.GitAuthModeGitHubApp),
		},
		PreviousRef: &config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     "codereview/shared-reviewer",
			Mode:    string(config.GitAuthModePAT),
		},
		KeySpecs: []credentials.KeySpec{
			{Key: credentials.GitHubAppIDKey, Required: true},
			{Key: credentials.GitHubAppPrivateKeyKey, Required: true},
			{Key: credentials.GitHubAppInstallationIDKey, Required: false},
		},
		State: initCredentialPlanStateKeepExisting,
	}
	appProfile := basicProfile("app")
	appProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModeGitHubApp,
		CredentialRef: "codereview/shared-reviewer",
	}
	patProfile := basicProfile("pat")
	patProfile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/shared-reviewer",
	}
	cfg := config.File{
		Profiles: map[string]config.Profile{
			"app": appProfile,
			"pat": patProfile,
		},
	}
	resolved, err := credentials.ResolveSecretsProfileForProfile(cfg, appProfile)
	if err != nil {
		t.Fatalf("ResolveSecretsProfileForProfile: %v", err)
	}
	appEntry.SecretsProfile = resolved
	groups, err := groupStaleReviewerCredentialCleanupsByStore(cfg, []initCredentialPlanEntry{appEntry})
	if err != nil {
		t.Fatalf("groupStaleReviewerCredentialCleanupsByStore: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("cleanup groups = %#v, want one group", groups)
	}
	cleanedRefs, err := deleteStaleReviewerCredentialKeysForRefs(store, groups[0].Entries)
	if err != nil {
		t.Fatalf("deleteStaleReviewerCredentialKeysForRefs: %v", err)
	}
	if len(cleanedRefs) != 0 {
		t.Fatalf("cleaned refs = %#v, want none because both modes are active", cleanedRefs)
	}
	if ok, err := store.Exists("shared-reviewer", credentials.GitTokenKey); err != nil || !ok {
		t.Fatalf("git_token exists = %t, err = %v; want retained for active PAT entry", ok, err)
	}
}

type fakeInitSecretPrompter struct {
	actions       []initCredentialSecretAction
	sources       []initSecretSource
	pastes        []string
	pasteErrors   []error
	onAction      func(initCredentialSecretPrompt)
	actionPrompts []initCredentialSecretPrompt
	sourcePrompts []initSecretValuePrompt
	pastePrompts  []initSecretValuePrompt
}

func (f *fakeInitSecretPrompter) ChooseCredentialAction(prompt initCredentialSecretPrompt) (initCredentialSecretAction, error) {
	f.actionPrompts = append(f.actionPrompts, prompt)
	if f.onAction != nil {
		f.onAction(prompt)
	}
	if len(f.actions) == 0 {
		return "", errors.New("unexpected credential action prompt")
	}
	action := f.actions[0]
	f.actions = f.actions[1:]
	return action, nil
}

func (f *fakeInitSecretPrompter) ChooseSecretSource(prompt initSecretValuePrompt) (initSecretSource, error) {
	f.sourcePrompts = append(f.sourcePrompts, prompt)
	if len(f.sources) == 0 {
		return "", errors.New("unexpected secret source prompt")
	}
	source := f.sources[0]
	f.sources = f.sources[1:]
	return source, nil
}

func (f *fakeInitSecretPrompter) PasteSecret(prompt initSecretValuePrompt) (string, error) {
	f.pastePrompts = append(f.pastePrompts, prompt)
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

func (s *fakeInitStore) Delete(profile, key string) error {
	if _, ok := s.bundles[profile][key]; !ok {
		return credstore.ErrNotFound
	}
	delete(s.bundles[profile], key)
	return nil
}

func (s *fakeInitStore) Close() error { return nil }

func assertFakeStored(t *testing.T, store *fakeInitStore, profile, key, want string) {
	t.Helper()
	if got := store.bundles[profile][key]; got != want {
		t.Fatalf("fake store %s/%s = %q, want %q", profile, key, got, want)
	}
}

func assertFakeBundleKeys(t *testing.T, store *fakeInitStore, profile string, want []string) {
	t.Helper()
	got := make([]string, 0, len(store.bundles[profile]))
	for key := range store.bundles[profile] {
		got = append(got, key)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fake store %s keys = %#v, want %#v", profile, got, want)
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

func assertContentOrder(t *testing.T, content string, parts ...string) {
	t.Helper()
	previous := -1
	for _, part := range parts {
		index := strings.Index(content, part)
		if index < 0 {
			t.Fatalf("content missing %q:\n%s", part, content)
		}
		if index <= previous {
			t.Fatalf("content order wrong for %q:\n%s", part, content)
		}
		previous = index
	}
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

const testFileCredentialStoreID = "test-file"

type initFlagChange struct {
	name  string
	value string
}

func defaultNonInteractiveInitOptionsForTest() initOptions {
	return initOptions{
		nonInteractive: true,
		gitHost:        "github.com",
		gitAuth:        string(config.GitAuthModePAT),
		reviewerAuth:   string(config.GitAuthModePAT),
		llmProvider:    string(config.LLMProviderAnthropic),
		llmAuth:        string(config.LLMAuthSubscription),
		llmAdapter:     string(config.LLMAdapterClaudeCLI),
		majorEvent:     string(config.ReviewMajorEventComment),
	}
}

func runNonInteractiveInitWithFakeStore(t *testing.T, path string, profile string, stdin io.Reader, flags initOptions, store *fakeInitStore, changes ...initFlagChange) (*bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	opts := &root.Options{
		Profile:    profile,
		Stdin:      stdin,
		Stdout:     out,
		Stderr:     errOut,
		ConfigPath: path,
	}
	cmd := newInitCommand(opts)
	for _, change := range changes {
		if err := cmd.Flags().Set(change.name, change.value); err != nil {
			t.Fatalf("set %s flag: %v", change.name, err)
		}
	}
	deps := initDeps{
		configPath: func(*root.Options) (string, error) { return path, nil },
		loadConfig: loadConfigForInit,
		saveConfig: config.Save,
		openResolvedStore: func(credentials.ResolvedSecretsProfile, string, bool, config.File) (initStore, error) {
			return store, nil
		},
	}
	return out, errOut, runInitWithDeps(cmd, opts, flags, deps)
}

func testFileSecretsConfig() config.SecretsConfig {
	return config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			testFileCredentialStoreID: {
				DisplayName: "Test File Store",
				Backend: config.SecretsStoreBackend{
					Kind: config.SecretsBackendKind(credstore.BackendFile),
				},
			},
		},
	}
}

func testFileCredentialStoreConfig(profileName string) config.File {
	profile := profileWithCredentialStore(basicProfile(profileName), testFileCredentialStoreID)
	return config.File{
		Secrets:  testFileSecretsConfig(),
		Profiles: map[string]config.Profile{profileName: profile},
	}
}

func profileWithCredentialStore(profile config.Profile, storeID string) config.Profile {
	profile.Git.Credential.Store = storeID
	if profile.ReviewerCredentials != nil {
		profile.ReviewerCredentials.Credential.Store = storeID
	}
	if profile.LLM.Credential.Name != "" {
		profile.LLM.Credential.Store = storeID
	}
	return profile
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
			Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: ref},
			CredentialRef: ref,
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
	}
}

func normalizeTestProfile(profile config.Profile) config.Profile {
	cfg := config.Normalize(config.File{
		Profiles: map[string]config.Profile{
			"test": profile,
		},
	})
	return cfg.Profiles["test"]
}

func apiKeyProfile(profile string, provider config.LLMProvider) config.Profile {
	p := basicProfile(profile)
	ref := "codereview/" + profile + "-llm"
	p.LLM = config.LLMConfig{
		Provider:      provider,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterAnthropicAPI,
		Credential:    config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: ref},
		CredentialRef: ref,
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
