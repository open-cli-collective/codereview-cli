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
		GitHubApp:     &config.GitHubAppConfig{AppID: "12345"},
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
		{key: credentials.GitHubAppPrivateKeyKey, value: "private-key-value"},
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
		credentials.GitHubAppPrivateKeyKey,
	})
}

func TestSetCredentialRejectsUnsupportedConfigBeforeIngress(t *testing.T) {
	hermeticFileBackend(t)
	t.Setenv("CR_FUTURE_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawCredentialTestConfig(t, path, `llm_runtimes:
  claude-cli:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
profiles:
  future:
    git:
      host: github.com
      auth_mode: oauth_device
      credential:
        store: local-os
        name: codereview/future
    reviewer:
      kind: git_identity
    llm_runtime: claude-cli
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

const testFileCredentialStoreID = "test-file"

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
