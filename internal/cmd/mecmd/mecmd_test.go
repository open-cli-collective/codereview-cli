package mecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "live-home", ID: "1"},
	}}
	cmd, out := newTestCommand(path, resolver)

	if err := root.Execute(cmd, []string{"me", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
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

func TestMeAllProcessesProfilesSortedAndSavesOnce(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home":          {Login: "new-home"},
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
	if len(got.Profiles) != 2 || got.Profiles[0].Profile != "home" || got.Profiles[1].Profile != "work" {
		t.Fatalf("profiles = %#v, want sorted home/work", got.Profiles)
	}
	cfg := loadTestConfig(t, path)
	if cfg.Profiles["home"].Git.IdentityCache != "new-home" || cfg.Profiles["work"].ReviewerCredentials.IdentityCache != "new-bot" {
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
		identities: map[string]gitprovider.Identity{"codereview/home": {Login: "new-home"}},
		errs:       map[string]error{"codereview/work-reviewer": errors.New("lookup failed")},
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
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat before: %v", err)
	}
	resolver := &fakeResolver{identities: map[string]gitprovider.Identity{
		"codereview/home": {Login: "old-home"},
	}}
	cmd, out := newTestCommand(path, resolver)

	if err := root.Execute(cmd, []string{"me"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("config modtime changed from %s to %s", infoBefore.ModTime(), infoAfter.ModTime())
	}
	if !strings.Contains(out.String(), "Identity cache updated: false") {
		t.Fatalf("stdout = %q, want unchanged cache", out.String())
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

func TestMeReservedAuthModeExitCode(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.Git.AuthMode = config.GitAuthModeOAuthDevice
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
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

func TestMeReviewerReservedAuthModeDoesNotOpenResolverFactory(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["work"]
	work.ReviewerCredentials.AuthMode = config.GitAuthModeGitHubApp
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	factoryOpened := false
	cmd, _ := newTestCommandWithFactory(path, func(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
		factoryOpened = true
		return &fakeResolver{}, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "work", "me"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if factoryOpened {
		t.Fatal("resolver factory opened for unsupported reviewer auth mode")
	}
}

func TestMeAllReservedAuthModeDoesNotOpenResolverFactory(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["work"]
	work.ReviewerCredentials.AuthMode = config.GitAuthModeOAuthDevice
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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

	resolver := &GitHubResolver{
		Store: store,
		Options: githubprovider.Options{
			BaseURL:    server.URL,
			GraphQLURL: server.URL + "/graphql",
		},
	}
	gitIdentity, err := resolver.ResolveIdentity(context.Background(), config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work",
	})
	if err != nil {
		t.Fatalf("ResolveIdentity git: %v", err)
	}
	reviewerIdentity, err := resolver.ResolveIdentity(context.Background(), config.GitConfig{
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

type fakeResolver struct {
	identities map[string]gitprovider.Identity
	errs       map[string]error
	calls      []config.GitConfig
}

func (f *fakeResolver) ResolveIdentity(_ context.Context, git config.GitConfig) (gitprovider.Identity, error) {
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

func saveTestConfig(t *testing.T, cfg config.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
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
