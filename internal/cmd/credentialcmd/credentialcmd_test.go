package credentialcmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
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
		{name: "invalid profile segment", args: []string{"--profile", "bad.profile", "init", "--non-interactive"}},
		{name: "llm ingress under subscription auth", args: []string{"init", "--non-interactive", "--llm-api-key-from-env", "CR_LLM_KEY"}},
	}
	t.Setenv("CR_LLM_KEY", "llm-key")
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

func newTestCommand(path string, stdin *strings.Reader) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
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
