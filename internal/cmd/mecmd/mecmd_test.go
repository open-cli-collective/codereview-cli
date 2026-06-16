package mecmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/identity"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestMeDefaultProfileUpdatesGitCacheAndRendersText(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "live-home", ID: "1", DisplayName: "Live Home"},
	}}
	cmd, out := newTestCommand(path, resolver)

	if err := root.Execute(cmd, []string{"me"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Profile: home") || !strings.Contains(got, "Login: live-home") || !strings.Contains(got, "Identity cache updated: true") {
		t.Fatalf("stdout = %q, want home live identity update", got)
	}
	cfg := loadTestConfig(t, path)
	if got := cfg.Profiles["home"].Git.IdentityCache; got != "live-home" {
		t.Fatalf("git identity cache = %q, want live-home", got)
	}
}

func TestMeJSONDoesNotLeakTokenMaterial(t *testing.T) {
	const token = "distinctive-secret-token"
	store := openFileStore(t)
	defer store.Close()
	if _, err := store.SetBundle("home", map[string]string{credentials.GitTokenKey: token}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle home: %v", err)
	}
	path := saveTestConfig(t, testConfig())
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{"login": "live-home", "id": 1})
	}))
	defer server.Close()
	cmd, out := newTestCommandWithFactory(path, func(_ *cobra.Command, _ *root.Options, cfg config.File) (identity.Resolver, func(), error) {
		return &githubResolver{
			cfg:                cfg,
			backend:            string(credstore.BackendFile),
			backendFlagChanged: true,
			options: githubprovider.Options{
				BaseURL:    server.URL,
				GraphQLURL: server.URL + "/graphql",
			},
		}, nil, nil
	})

	if err := root.Execute(cmd, []string{"me", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(auths) != 1 || auths[0] != "Bearer "+token {
		t.Fatalf("auths = %#v, want token used for provider request", auths)
	}
	if strings.Contains(out.String(), token) {
		t.Fatalf("stdout leaked token: %q", out.String())
	}
	var got view.MeResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Profiles) != 1 || got.Profiles[0].CredentialSource != "git" || got.Profiles[0].Login != "live-home" {
		t.Fatalf("JSON = %#v, want one git identity", got)
	}
}

func TestMeUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
	const token = "named-secrets-profile-token"
	store := openFileStore(t)
	defer store.Close()
	if _, err := store.SetBundle("home", map[string]string{credentials.GitTokenKey: token}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle home: %v", err)
	}
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	cfg.Secrets = config.SecretsConfig{
		DefaultProfile: "work-file",
		Profiles: map[string]config.SecretsProfile{
			"work-file": {
				Label:   "Work File Store",
				Backend: config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{"login": "live-home", "id": 1})
	}))
	defer server.Close()
	cmd, out := newTestCommandWithFactory(path, func(_ *cobra.Command, _ *root.Options, cfg config.File) (identity.Resolver, func(), error) {
		return &githubResolver{
			cfg: cfg,
			options: githubprovider.Options{
				BaseURL:    server.URL,
				GraphQLURL: server.URL + "/graphql",
			},
		}, nil, nil
	})

	if err := root.Execute(cmd, []string{"me", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(auths) != 1 || auths[0] != "Bearer "+token {
		t.Fatalf("auths = %#v, want file-backed named secrets profile token", auths)
	}
	var got view.MeResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Login != "live-home" {
		t.Fatalf("JSON = %#v, want resolved named-store identity", got)
	}
}

func TestMeProfileUsesReviewerCredentials(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/work-reviewer": {Login: "bot"},
	}}
	cmd, _ := newTestCommand(path, resolver)

	if err := root.Execute(cmd, []string{"--profile", "work", "me"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfg := loadTestConfig(t, path)
	work := cfg.Profiles["work"]
	if got := work.Git.IdentityCache; got != "work-user-cache" {
		t.Fatalf("git identity cache = %q, want unchanged", got)
	}
	if got := work.ReviewerCredentials.IdentityCache; got != "bot" {
		t.Fatalf("reviewer identity cache = %q, want bot", got)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("resolver calls = %#v, want reviewer ref", resolver.calls)
	}
}

func TestMeReviewerGitHubAppAuthJSONUsesReviewerCredentialFlow(t *testing.T) {
	const installationToken = "me-reviewer-github-app-installation-token" // #nosec G101 -- distinctive test canary, not a real token.
	store := openFileStore(t)
	defer store.Close()
	privateKey := testPrivateKeyPEM(t)
	if _, err := store.SetBundle("work-reviewer", map[string]string{
		credentials.GitHubAppIDKey:             "12345",
		credentials.GitHubAppPrivateKeyKey:     privateKey,
		credentials.GitHubAppInstallationIDKey: "42",
	}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle reviewer: %v", err)
	}
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	work := cfg.Profiles["work"]
	work.ReviewerCredentials.AuthMode = config.GitAuthModeGitHubApp
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)

	var appJWTs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			appJWTs = append(appJWTs, r.Header.Get("Authorization"))
			writeJSON(t, w, map[string]any{"id": 42, "app_id": 12345, "app_slug": "reviewer-app"})
		case "/app/installations/42/access_tokens":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			appJWTs = append(appJWTs, r.Header.Get("Authorization"))
			writeJSON(t, w, map[string]any{
				"token":      installationToken,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	cmd, out := newTestCommandWithFactory(path, func(_ *cobra.Command, _ *root.Options, cfg config.File) (identity.Resolver, func(), error) {
		return &githubResolver{
			cfg:                cfg,
			backend:            string(credstore.BackendFile),
			backendFlagChanged: true,
			options: githubprovider.Options{
				BaseURL:    server.URL,
				GraphQLURL: server.URL + "/graphql",
			},
		}, nil, nil
	})

	if err := root.Execute(cmd, []string{"--profile", "work", "me", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(appJWTs) != 2 || !strings.HasPrefix(appJWTs[0], "Bearer ") || !strings.HasPrefix(appJWTs[1], "Bearer ") {
		t.Fatalf("app JWT auths = %#v, want app JWTs for installation and token requests", appJWTs)
	}
	for _, secret := range []string{privateKey, installationToken, "12345", "42"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("stdout leaked %q: %q", secret, out.String())
		}
	}
	var got view.MeResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Profiles) != 1 || got.Profiles[0].CredentialSource != "reviewer_credentials" || got.Profiles[0].Login != "reviewer-app[bot]" {
		t.Fatalf("JSON = %#v, want reviewer app identity", got)
	}
}

func TestMeAllProcessesProfilesSortedAndSavesOnce(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home":          {Login: "new-home"},
		"codereview/work":          {Login: "new-work-user"},
		"codereview/work-reviewer": {Login: "new-bot"},
	}}
	cmd, out := newTestCommand(path, resolver)

	if err := root.Execute(cmd, []string{"me", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.MeResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Profiles) != 3 ||
		got.Profiles[0].Profile != "home" ||
		got.Profiles[0].CredentialSource != "git" ||
		got.Profiles[1].Profile != "work" ||
		got.Profiles[1].CredentialSource != "git" ||
		got.Profiles[2].Profile != "work" ||
		got.Profiles[2].CredentialSource != "reviewer_credentials" {
		t.Fatalf("profiles = %#v, want sorted home git/work git/work reviewer", got.Profiles)
	}
	cfg := loadTestConfig(t, path)
	if cfg.Profiles["home"].Git.IdentityCache != "new-home" ||
		cfg.Profiles["work"].Git.IdentityCache != "new-work-user" ||
		cfg.Profiles["work"].ReviewerCredentials.IdentityCache != "new-bot" {
		t.Fatalf("updated caches = %#v", cfg.Profiles)
	}
}

func TestMeAllProfileRejected(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path, &fakeResolver{})

	err := root.Execute(cmd, []string{"--profile", "work", "me", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestMeAllFailureDoesNotSavePartialCache(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{
		identities: map[string]gitprovider.Identity{
			"codereview/home": {Login: "new-home"},
			"codereview/work": {Login: "new-work-user"},
		},
		errs: map[string]error{"codereview/work-reviewer": errors.New("lookup failed")},
	}
	cmd, _ := newTestCommand(path, resolver)

	err := root.Execute(cmd, []string{"me", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want lookup failure")
	}
	cfg := loadTestConfig(t, path)
	if got := cfg.Profiles["home"].Git.IdentityCache; got != "old-home" {
		t.Fatalf("home cache = %q, want unchanged", got)
	}
	if got := cfg.Profiles["work"].Git.IdentityCache; got != "work-user-cache" {
		t.Fatalf("work git cache = %q, want unchanged", got)
	}
}

func TestMeEmptyLoginDoesNotClearCache(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: ""},
	}}
	cmd, _ := newTestCommand(path, resolver)

	err := root.Execute(cmd, []string{"me"})
	if err == nil {
		t.Fatal("Execute error = nil, want empty login failure")
	}
	cfg := loadTestConfig(t, path)
	if got := cfg.Profiles["home"].Git.IdentityCache; got != "old-home" {
		t.Fatalf("home cache = %q, want unchanged", got)
	}
}

func TestMeUnchangedCacheDoesNotRewriteConfig(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	// #nosec G304 -- test path is controlled by t.TempDir.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "old-home"},
	}}
	saveCalled := false

	result, err := runMeWithSaver(context.Background(), &cobra.Command{}, &root.Options{ConfigPath: path}, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		return resolver, nil, nil
	}, false, func(string, config.File) error {
		saveCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("runMeWithSaver: %v", err)
	}
	if saveCalled {
		t.Fatal("save called for unchanged identity cache")
	}
	if len(result.Profiles) != 1 || result.Profiles[0].IdentityCacheUpdated {
		t.Fatalf("result = %#v, want unchanged cache", result)
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("config bytes changed\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMeMissingProfileExitCode(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path, &fakeResolver{})

	err := root.Execute(cmd, []string{"--profile", "missing", "me"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestMeMissingProfileDoesNotOpenResolverFactory(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	factoryOpened := false
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		factoryOpened = true
		return &fakeResolver{}, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "missing", "me"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if factoryOpened {
		t.Fatal("resolver factory opened for missing profile")
	}
}

func TestMeMissingConfigExitCode(t *testing.T) {
	cmd, _ := newTestCommand(filepath.Join(t.TempDir(), "missing.yml"), &fakeResolver{})

	err := root.Execute(cmd, []string{"me"})
	if !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("Execute error = %v, want ErrNotConfigured", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestMeProductionMissingGitCredentialExitCode(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	Register(cmd, opts)

	err := root.Execute(cmd, []string{"me"})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("Execute error = %v, want ErrAuth", err)
	}
	if !errors.Is(err, credstore.ErrNotFound) {
		t.Fatalf("Execute error = %v, want missing credential cause", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestMeProductionMissingReviewerCredentialUsesReviewerRef(t *testing.T) {
	store := openFileStore(t)
	defer store.Close()
	if _, err := store.SetBundle("work", map[string]string{credentials.GitTokenKey: "git-token"}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle work: %v", err)
	}
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	work := cfg.Profiles["work"]
	work.Git.Host = "localhost:1"
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	Register(cmd, opts)

	err := root.Execute(cmd, []string{"--profile", "work", "me"})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("Execute error = %v, want ErrAuth", err)
	}
	if !errors.Is(err, credstore.ErrNotFound) {
		t.Fatalf("Execute error = %v, want missing credential cause", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestMeGitHubAppRequiresInstallationIDWithoutRepositoryContext(t *testing.T) {
	store := openFileStore(t)
	defer store.Close()
	privateKey := testPrivateKeyPEM(t)
	if _, err := store.SetBundle("home", map[string]string{
		credentials.GitHubAppIDKey:         "12345",
		credentials.GitHubAppPrivateKeyKey: privateKey,
	}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle home: %v", err)
	}
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	home := cfg.Profiles["home"]
	home.Git.AuthMode = config.GitAuthModeGitHubApp
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	Register(cmd, opts)

	err := root.Execute(cmd, []string{"--profile", "home", "me"})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("Execute error = %v, want ErrAuth", err)
	}
	if !strings.Contains(err.Error(), credentials.GitHubAppInstallationIDKey) {
		t.Fatalf("Execute error = %v, want missing installation id detail", err)
	}
	if strings.Contains(err.Error()+out.String(), privateKey) {
		t.Fatalf("output leaked private key: err=%v out=%q", err, out.String())
	}
}

func TestMeGitHubAppGitAuthJSONWithoutReviewerCredentials(t *testing.T) {
	const installationToken = "me-github-app-installation-token" // #nosec G101 -- distinctive test canary, not a real token.
	store := openFileStore(t)
	defer store.Close()
	privateKey := testPrivateKeyPEM(t)
	if _, err := store.SetBundle("home", map[string]string{
		credentials.GitHubAppIDKey:             "12345",
		credentials.GitHubAppPrivateKeyKey:     privateKey,
		credentials.GitHubAppInstallationIDKey: "42",
	}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle home: %v", err)
	}
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	home := cfg.Profiles["home"]
	home.Git.AuthMode = config.GitAuthModeGitHubApp
	home.ReviewerCredentials = nil
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)

	var appJWTs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			appJWTs = append(appJWTs, r.Header.Get("Authorization"))
			writeJSON(t, w, map[string]any{"id": 42, "app_id": 12345, "app_slug": "cr-reviewer"})
		case "/app/installations/42/access_tokens":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			appJWTs = append(appJWTs, r.Header.Get("Authorization"))
			writeJSON(t, w, map[string]any{
				"token":      installationToken,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	cmd, out := newTestCommandWithFactory(path, func(_ *cobra.Command, _ *root.Options, cfg config.File) (identity.Resolver, func(), error) {
		return &githubResolver{
			cfg:                cfg,
			backend:            string(credstore.BackendFile),
			backendFlagChanged: true,
			options: githubprovider.Options{
				BaseURL:    server.URL,
				GraphQLURL: server.URL + "/graphql",
			},
		}, nil, nil
	})

	if err := root.Execute(cmd, []string{"me", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(appJWTs) != 2 || !strings.HasPrefix(appJWTs[0], "Bearer ") || !strings.HasPrefix(appJWTs[1], "Bearer ") {
		t.Fatalf("app JWT auths = %#v, want app JWTs for installation and token requests", appJWTs)
	}
	for _, secret := range []string{privateKey, installationToken, "12345", "42"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("stdout leaked %q: %q", secret, out.String())
		}
	}
	var got view.MeResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Profiles) != 1 || got.Profiles[0].CredentialSource != "git" || got.Profiles[0].Login != "cr-reviewer[bot]" {
		t.Fatalf("JSON = %#v, want app-backed git identity", got)
	}
}

func TestMeReservedAuthModeExitCode(t *testing.T) {
	path := writeRawTestConfig(t, `default_profile: home
keyring:
  backend: memory
profiles:
  home:
    git:
      host: github.com
      auth_mode: oauth_device
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	factoryOpened := false
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		factoryOpened = true
		return &fakeResolver{}, nil, nil
	})

	err := root.Execute(cmd, []string{"me"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
	if factoryOpened {
		t.Fatal("resolver factory opened for unsupported auth mode")
	}
}

func TestMeReviewerGitHubAppAuthModeUsesResolver(t *testing.T) {
	path := writeRawTestConfig(t, `default_profile: work
keyring:
  backend: memory
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: github_app
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	resolver := &fakeResolver{
		identities: map[string]gitprovider.Identity{"codereview/work-reviewer": {Login: "cr-reviewer[bot]", ID: "12345"}},
		errs:       map[string]error{},
	}
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		return resolver, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "work", "me"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resolver.calls) != 1 ||
		resolver.calls[0].AuthMode != config.GitAuthModeGitHubApp ||
		resolver.calls[0].CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("resolver calls = %#v, want reviewer github_app config", resolver.calls)
	}
}

func TestMeAllReservedAuthModeDoesNotOpenResolverFactory(t *testing.T) {
	path := writeRawTestConfig(t, `default_profile: home
keyring:
  backend: memory
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
  work:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: oauth_device
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	factoryOpened := false
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		factoryOpened = true
		return &fakeResolver{}, nil, nil
	})

	err := root.Execute(cmd, []string{"me", "--all"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if factoryOpened {
		t.Fatal("resolver factory opened for unsupported auth mode in --all")
	}
}

func TestMeAllReservedGitAuthModeWithReviewerDoesNotOpenResolverFactory(t *testing.T) {
	path := writeRawTestConfig(t, `default_profile: home
keyring:
  backend: memory
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
  work:
    git:
      host: github.com
      auth_mode: oauth_device
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: pat
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
`)
	factoryOpened := false
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		factoryOpened = true
		return &fakeResolver{}, nil, nil
	})

	err := root.Execute(cmd, []string{"me", "--all"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if factoryOpened {
		t.Fatal("resolver factory opened for unsupported git auth mode in --all")
	}
}

func TestPrevalidateAllIdentityProfileRejectsInvalidAuthModes(t *testing.T) {
	home := testConfig().Profiles["home"]
	home.Git.AuthMode = config.GitAuthMode("bogus")
	if err := prevalidateAllIdentityProfile("home", home); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("git invalid auth error = %v, want ErrInvalid", err)
	}

	work := testConfig().Profiles["work"]
	work.ReviewerCredentials.AuthMode = config.GitAuthMode("bogus")
	if err := prevalidateAllIdentityProfile("work", work); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("reviewer invalid auth error = %v, want ErrInvalid", err)
	}
}

func TestMeProviderErrorExitCode(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{errs: map[string]error{
		"codereview/home": gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationWhoAmI, errors.New("timeout")),
	}}
	cmd, _ := newTestCommand(path, resolver)

	err := root.Execute(cmd, []string{"me"})
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("Execute error = %v, want ErrRetryable", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UpstreamError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UpstreamError)
	}
}

func TestGitHubResolverUsesCR08FactoryAndCredentialStore(t *testing.T) {
	store := openFileStore(t)
	defer store.Close()
	if _, err := store.SetBundle("work", map[string]string{credentials.GitTokenKey: "git-token"}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle work: %v", err)
	}
	if _, err := store.SetBundle("work-reviewer", map[string]string{credentials.GitTokenKey: "reviewer-token"}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle reviewer: %v", err)
	}

	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Fatalf("path = %q, want /user", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		switch auth {
		case "Bearer git-token":
			writeJSON(t, w, map[string]any{"login": "git-user", "id": 1, "name": "Git User"})
		case "Bearer reviewer-token":
			writeJSON(t, w, map[string]any{"login": "review-bot", "id": 2, "name": "Review Bot"})
		default:
			t.Fatalf("Authorization = %q", auth)
		}
	}))
	defer server.Close()

	resolver := &githubResolver{
		cfg: config.File{
			DefaultProfile: "work",
			Keyring:        config.KeyringConfig{Backend: "file"},
			Profiles: map[string]config.Profile{
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
						Provider: config.LLMProviderAnthropic,
						Auth:     config.LLMAuthSubscription,
						Adapter:  config.LLMAdapterClaudeCLI,
					},
				},
			},
		},
		backend:            string(credstore.BackendFile),
		backendFlagChanged: true,
		options: githubprovider.Options{
			BaseURL:    server.URL,
			GraphQLURL: server.URL + "/graphql",
		},
	}
	gitIdentity, err := resolver.ResolveIdentity(context.Background(), "work", config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work",
	})
	if err != nil {
		t.Fatalf("ResolveIdentity git: %v", err)
	}
	reviewerIdentity, err := resolver.ResolveIdentity(context.Background(), "work", config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work-reviewer",
	})
	if err != nil {
		t.Fatalf("ResolveIdentity reviewer: %v", err)
	}
	if gitIdentity.Login != "git-user" || reviewerIdentity.Login != "review-bot" {
		t.Fatalf("identities = %#v %#v", gitIdentity, reviewerIdentity)
	}
	if len(auths) != 2 || auths[0] != "Bearer git-token" || auths[1] != "Bearer reviewer-token" {
		t.Fatalf("auths = %#v", auths)
	}
}

func TestOrgDeploymentPrestagedMultiRefCredentialsHealthChecks(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	path := saveTestConfig(t, config.File{
		DefaultProfile: "work",
		Keyring:        config.KeyringConfig{Backend: "file"},
		Profiles: map[string]config.Profile{
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
				AgentSources: []string{"~/.config/codereview/agents"},
				ReviewPolicy: config.ReviewPolicy{
					MajorEvent: config.ReviewMajorEventComment,
				},
			},
		},
	})

	runOrgDeploymentCommand(t, path, strings.NewReader("user-token"), nil, []string{
		"set-credential",
		"--ref", "codereview/work",
		"--key", credentials.GitTokenKey,
		"--stdin",
		"--overwrite",
	})
	runOrgDeploymentCommand(t, path, strings.NewReader("reviewer-token"), nil, []string{
		"set-credential",
		"--ref", "codereview/work-reviewer",
		"--key", credentials.GitTokenKey,
		"--stdin",
		"--overwrite",
	})
	runOrgDeploymentCommand(t, path, strings.NewReader("llm-token"), nil, []string{
		"set-credential",
		"--ref", "codereview/work-llm",
		"--key", credentials.AnthropicAPIKeyKey,
		"--stdin",
		"--overwrite",
	})

	configOut := runOrgDeploymentCommand(t, path, strings.NewReader(""), nil, []string{"config", "show", "--json"})
	for _, secret := range []string{"user-token", "reviewer-token", "llm-token"} {
		if strings.Contains(configOut.String(), secret) {
			t.Fatalf("config show leaked %s: %q", secret, configOut.String())
		}
	}
	var show view.ConfigShow
	if err := json.Unmarshal(configOut.Bytes(), &show); err != nil {
		t.Fatalf("Unmarshal config show: %v\n%s", err, configOut.String())
	}
	if len(show.CredentialRefs) != 3 {
		t.Fatalf("credential refs = %#v, want git/reviewer/llm", show.CredentialRefs)
	}
	wantPresent := map[string]string{
		"git":                  credentials.GitTokenKey,
		"reviewer_credentials": credentials.GitTokenKey,
		"llm":                  credentials.AnthropicAPIKeyKey,
	}
	for _, ref := range show.CredentialRefs {
		wantKey, ok := wantPresent[ref.Purpose]
		if !ok {
			t.Fatalf("unexpected credential purpose %q in %#v", ref.Purpose, show.CredentialRefs)
		}
		if len(ref.Keys) != 1 || ref.Keys[0].Key != wantKey || ref.Keys[0].Present == nil || !*ref.Keys[0].Present {
			t.Fatalf("credential status for %s = %#v, want present %s", ref.Purpose, ref.Keys, wantKey)
		}
		delete(wantPresent, ref.Purpose)
	}
	if len(wantPresent) != 0 {
		t.Fatalf("missing credential purposes: %#v", wantPresent)
	}

	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("path = %q, want /user", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		switch auth {
		case "Bearer user-token":
			writeJSON(t, w, map[string]any{"login": "work-user", "id": 1, "name": "Work User"})
		case "Bearer reviewer-token":
			writeJSON(t, w, map[string]any{"login": "review-bot", "id": 2, "name": "Review Bot"})
		default:
			t.Errorf("Authorization = %q", auth)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	meOut := runOrgDeploymentCommand(t, path, strings.NewReader(""), orgDeploymentFactory(server.URL), []string{"me", "--all", "--json"})
	for _, secret := range []string{"user-token", "reviewer-token", "llm-token"} {
		if strings.Contains(meOut.String(), secret) {
			t.Fatalf("me leaked %s: %q", secret, meOut.String())
		}
	}
	var meResult view.MeResult
	if err := json.Unmarshal(meOut.Bytes(), &meResult); err != nil {
		t.Fatalf("Unmarshal me: %v\n%s", err, meOut.String())
	}
	if len(meResult.Profiles) != 2 ||
		meResult.Profiles[0].Profile != "work" ||
		meResult.Profiles[0].CredentialSource != "git" ||
		meResult.Profiles[0].Login != "work-user" ||
		meResult.Profiles[1].Profile != "work" ||
		meResult.Profiles[1].CredentialSource != "reviewer_credentials" ||
		meResult.Profiles[1].Login != "review-bot" {
		t.Fatalf("me profiles = %#v, want work git and reviewer identities", meResult.Profiles)
	}
	if len(auths) != 2 || auths[0] != "Bearer user-token" || auths[1] != "Bearer reviewer-token" {
		t.Fatalf("auths = %#v, want user then reviewer tokens", auths)
	}
}

type fakeResolver struct {
	identities map[string]gitprovider.Identity
	errs       map[string]error
	calls      []config.GitConfig
}

func (f *fakeResolver) ResolveIdentity(_ context.Context, _ string, git config.GitConfig) (gitprovider.Identity, error) {
	f.calls = append(f.calls, git)
	if err := f.errs[git.CredentialRef]; err != nil {
		return gitprovider.Identity{}, err
	}
	return f.identities[git.CredentialRef], nil
}

func newTestCommand(path string, resolver identity.Resolver) (*cobra.Command, *bytes.Buffer) {
	return newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		return resolver, nil, nil
	})
}

func newTestCommandWithFactory(path string, factory IdentityResolverFactory) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out
}

func runOrgDeploymentCommand(t *testing.T, path string, stdin *strings.Reader, factory IdentityResolverFactory, args []string) *bytes.Buffer {
	t.Helper()
	if factory == nil {
		factory = func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
			return &fakeResolver{}, nil, nil
		}
	}
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      stdin,
		Stdout:     &out,
		Stderr:     &out,
	})
	credentialcmd.Register(cmd, opts)
	configcmd.Register(cmd, opts)
	RegisterWithFactory(cmd, opts, factory)
	if err := root.Execute(cmd, args); err != nil {
		t.Fatalf("Execute %v: %v\noutput:\n%s", args, err, out.String())
	}
	return &out
}

func orgDeploymentFactory(serverURL string) IdentityResolverFactory {
	return func(_ *cobra.Command, opts *root.Options, cfg config.File) (identity.Resolver, func(), error) {
		return &githubResolver{
			cfg:     cfg,
			backend: opts.Backend,
			options: githubprovider.Options{
				BaseURL:    serverURL,
				GraphQLURL: serverURL + "/graphql",
			},
		}, nil, nil
	}
}

func saveTestConfig(t *testing.T, cfg config.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

func writeRawTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func loadTestConfig(t *testing.T, path string) config.File {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func openFileStore(t *testing.T) *credstore.Store {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
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
					IdentityCache: "old-home",
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
					IdentityCache: "work-user-cache",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work-reviewer",
					IdentityCache: "old-bot",
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

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode: %v", err)
	}
}
