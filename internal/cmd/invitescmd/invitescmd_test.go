package invitescmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdtest"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/config/configtest"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/gitproviders"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestAcceptRequiresAllowlistBeforeOpeningCredentials(t *testing.T) {
	h := newHarness(t, testConfig())
	for _, args := range [][]string{
		{"--profile", "home", "invites", "accept"},
		{"--profile", "home", "invites", "accept", "--dry-run"},
	} {
		_, err := h.run(args...)
		if exitcode.FromError(err) != exitcode.UsageError || !strings.Contains(err.Error(), "--owner or --repo") {
			t.Fatalf("%v error = %v (exit %d), want usage error naming --owner/--repo", args, err, exitcode.FromError(err))
		}
	}
	if h.factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want 0", h.factoryCalls)
	}
}

func TestMalformedAllowlistIsUsageError(t *testing.T) {
	h := newHarness(t, testConfig())
	for _, args := range [][]string{
		{"--profile", "home", "invites", "accept", "--repo", "no-slash"},
		{"--profile", "home", "invites", "accept", "--owner", "org/repo"},
		{"--profile", "home", "invites", "list", "--repo", "a/b/c"},
		{"--profile", "home", "invites", "accept", "--owner", "example-org", "extra"},
	} {
		if _, err := h.run(args...); exitcode.FromError(err) != exitcode.UsageError {
			t.Fatalf("%v error = %v (exit %d), want usage error", args, err, exitcode.FromError(err))
		}
	}
	if h.factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want 0", h.factoryCalls)
	}
}

func TestAcceptDryRunMakesNoWrites(t *testing.T) {
	h := newHarness(t, testConfig())
	var calls []gitprovider.Operation
	h.fake.SetCallHook(func(op gitprovider.Operation) { calls = append(calls, op) })

	out, err := h.run("--profile", "home", "invites", "accept", "--owner", "example-org", "--repo", "other-org/expired", "--dry-run")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if accepted := h.fake.RecordedAcceptedInvitations(); len(accepted) != 0 {
		t.Fatalf("accepted = %v, want none under --dry-run", accepted)
	}
	want := []gitprovider.Operation{gitprovider.OperationWhoAmI, gitprovider.OperationListRepositoryInvitations}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("provider calls = %v, want %v", calls, want)
	}
	for _, line := range []string{
		"Dry run: true",
		"  - example-org/docs: would_accept",
		"  - example-org/service: would_accept",
		"  - other-org/expired: skipped (invitation expired; ask the inviter to send a new one)",
		"  - other-org/tooling: skipped (not on the allowlist)",
		"Summary: accepted=0 would_accept=2 skipped=2 failed=0",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Fatalf("stdout missing %q:\n%s", line, out)
		}
	}
}

func TestAcceptPartialFailureReportsEveryResultThenExitsNonzero(t *testing.T) {
	h := newHarness(t, testConfig())
	h.fake.SetAcceptInvitationError("3", gitprovider.WrapError(gitprovider.ErrPermission, gitprovider.OperationAcceptRepositoryInvitation, errors.New("github: status 403")))

	out, err := h.run("--profile", "home", "invites", "accept", "--owner", "example-org", "--json")
	if err == nil {
		t.Fatalf("Execute error = nil, want failure\n%s", out)
	}
	if code := exitcode.FromError(err); code != exitcode.Failure {
		t.Fatalf("exit code = %d, want %d", code, exitcode.Failure)
	}
	if !strings.Contains(err.Error(), "1 of 2 accept attempts failed") {
		t.Fatalf("error = %v, want failure count", err)
	}
	var result view.InvitesAccept
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("stdout is not the JSON result: %v\n%s", err, out)
	}
	if result.Summary != (view.InvitesAcceptCounts{Accepted: 1, Skipped: 2, Failed: 1}) {
		t.Fatalf("summary = %#v", result.Summary)
	}
	if result.Results[0].Repository != "example-org/docs" || result.Results[0].Status != "failed" || result.Results[0].ReasonCode != "permission" || !strings.Contains(result.Results[0].Reason, "permission denied") {
		t.Fatalf("first result = %#v, want failed docs invitation with reason", result.Results[0])
	}
	if accepted := h.fake.RecordedAcceptedInvitations(); !reflect.DeepEqual(accepted, []gitprovider.InvitationID{"1"}) {
		t.Fatalf("accepted = %v, want the service invitation despite the docs failure", accepted)
	}
}

func TestAcceptJSONShape(t *testing.T) {
	h := newHarness(t, testConfig())

	out, err := h.run("--profile", "home", "invites", "accept", "--owner", "example-org", "--repo", "other-org/expired", "--json")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	assertGolden(t, "accept.golden.json", out)
}

func TestListJSONShapeAndFilter(t *testing.T) {
	h := newHarness(t, testConfig())

	out, err := h.run("--profile", "home", "invites", "list", "--json")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	assertGolden(t, "list.golden.json", out)

	out, err = h.run("--profile", "home", "invites", "list", "--repo", "Other-Org/Tooling")
	if err != nil {
		t.Fatalf("Execute filtered: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"Profile: home",
		"Credential source: git",
		"Host: github.com",
		"Login: review-bot",
		"Invitations:",
		"  - other-org/tooling",
		"    Permission: read",
		"    Inviter: other-owner",
		"    Created: 2026-10-29T08:00:00Z",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("filtered list =\n%s\nwant\n%s", out, want)
	}
	if accepted := h.fake.RecordedAcceptedInvitations(); len(accepted) != 0 {
		t.Fatalf("list accepted %v, want no writes", accepted)
	}
}

func TestReviewerCredentialsAreThePostingIdentity(t *testing.T) {
	h := newHarness(t, testConfig())

	out, err := h.run("--profile", "work", "invites", "list", "--json")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if h.lastGit.Credential.Name != "codereview/work-reviewer" || h.lastProfile != "work" {
		t.Fatalf("factory got profile %q credential %q, want work reviewer credential", h.lastProfile, h.lastGit.Credential.Name)
	}
	var result view.InvitesList
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if result.CredentialSource != "reviewer_credentials" {
		t.Fatalf("credential_source = %q, want reviewer_credentials", result.CredentialSource)
	}
}

func TestUnsupportedPostingCredentialsFailBeforeOpeningCredentials(t *testing.T) {
	cfg := testConfig()
	app := cfg.Profiles["work"]
	app.ReviewerCredentials.AuthMode = config.GitAuthModeGitHubApp
	app.ReviewerCredentials.GitHubApp = &config.GitHubAppConfig{AppID: "12345"}
	cfg.Profiles["app"] = app
	gitlab := cfg.Profiles["home"]
	gitlab.Git.Provider = config.GitProviderGitLab
	gitlab.Git.Host = "gitlab.com"
	cfg.Profiles["gitlab"] = gitlab
	h := newHarness(t, cfg)

	for _, profile := range []string{"app", "gitlab"} {
		_, err := h.run("--profile", profile, "invites", "list")
		if exitcode.FromError(err) != exitcode.AuthConfigError || !errors.Is(err, config.ErrUnsupported) {
			t.Fatalf("%s error = %v (exit %d), want unsupported config error", profile, err, exitcode.FromError(err))
		}
	}
	if h.factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want 0", h.factoryCalls)
	}
}

func TestProviderFailuresMapToExitCodes(t *testing.T) {
	tests := []struct {
		name string
		op   gitprovider.Operation
		err  error
		want int
	}{
		{name: "identity auth", op: gitprovider.OperationWhoAmI, err: gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, nil), want: exitcode.AuthConfigError},
		{name: "list retryable", op: gitprovider.OperationListRepositoryInvitations, err: gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationListRepositoryInvitations, nil), want: exitcode.UpstreamError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testConfig())
			h.fake.SetError(tt.op, tt.err)
			out, err := h.run("--profile", "home", "invites", "accept", "--owner", "example-org")
			if code := exitcode.FromError(err); code != tt.want {
				t.Fatalf("exit code = %d (%v), want %d", code, err, tt.want)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want nothing before the identity and listing succeed", out)
			}
			if !h.closed {
				t.Fatal("credential store cleanup did not run")
			}
		})
	}
}

func TestMissingProfileIsConfigError(t *testing.T) {
	h := newHarness(t, testConfig())
	if _, err := h.run("invites", "list"); exitcode.FromError(err) != exitcode.AuthConfigError || !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("error = %v, want profile-not-found config error", err)
	}
}

func TestConfiguredProviderAcceptsWithStoredTokenWithoutPrintingIt(t *testing.T) {
	const token = "cr-invites-distinctive-token-0001" // #nosec G101 -- distinctive test canary, not a real credential.
	store := openFileStore(t)
	if _, err := store.SetBundle("home", map[string]string{credentials.GitTokenKey: token}, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle home: %v", err)
	}
	var patched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want the stored token", got)
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/user":
			writeJSON(t, w, map[string]any{"login": "review-bot", "id": 9100})
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/user/repository_invitations":
			writeJSON(t, w, []map[string]any{
				{
					"id":          301001,
					"repository":  map[string]any{"name": "service", "owner": map[string]any{"login": "example-org"}},
					"inviter":     map[string]any{"login": "example-owner"},
					"permissions": "write",
					"created_at":  "2026-10-30T14:52:50Z",
				},
				{
					"id":          301002,
					"repository":  map[string]any{"name": "docs", "owner": map[string]any{"login": "example-org"}},
					"inviter":     map[string]any{"login": "example-owner"},
					"permissions": "write",
					"created_at":  "2026-10-30T14:52:50Z",
				},
			})
		case r.Method == http.MethodPatch && r.URL.EscapedPath() == "/user/repository_invitations/301001":
			patched = append(patched, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.EscapedPath() == "/user/repository_invitations/301002":
			patched = append(patched, r.URL.EscapedPath())
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"token ` + token + ` lacks scope"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := saveConfig(t, fileBackendConfig())
	factory := ConfiguredProviderFactory(gitproviders.Options{GitHub: githubprovider.Options{
		BaseURL:    server.URL,
		GraphQLURL: server.URL + "/graphql",
	}})

	for _, args := range [][]string{
		{"--profile", "home", "invites", "list"},
		{"--profile", "home", "invites", "list", "--json"},
		{"--profile", "home", "invites", "accept", "--owner", "example-org"},
		{"--profile", "home", "invites", "accept", "--owner", "example-org", "--json"},
	} {
		stdout, stderr, err := runWithFactory(path, factory, args...)
		for label, data := range map[string]string{"stdout": stdout, "stderr": stderr} {
			if err := credstore.NoLeakAssertion([]byte(data), token); err != nil {
				t.Fatalf("%v %s leaked the token: %v", args, label, err)
			}
		}
		if err != nil {
			if leakErr := credstore.NoLeakAssertion([]byte(err.Error()), token); leakErr != nil {
				t.Fatalf("%v error leaked the token: %v", args, leakErr)
			}
		}
		if args[3] == "accept" && exitcode.FromError(err) != exitcode.Failure {
			t.Fatalf("%v error = %v, want the 403 to fail the run", args, err)
		}
		if args[3] == "list" && err != nil {
			t.Fatalf("%v error = %v", args, err)
		}
	}
	if len(patched) != 4 {
		t.Fatalf("PATCH requests = %v, want both invitations accepted on each of the two runs", patched)
	}
}

type harness struct {
	t            *testing.T
	path         string
	fake         *gitprovider.Fake
	factoryCalls int
	lastProfile  string
	lastGit      config.GitConfig
	closed       bool
}

func newHarness(t *testing.T, cfg config.File) *harness {
	t.Helper()
	fake := &gitprovider.Fake{}
	fake.SetIdentity(gitprovider.Identity{Login: "review-bot", ID: "9100"})
	fake.SetRepositoryInvitations([]gitprovider.RepositoryInvitation{
		testInvitation("1", "example-org", "service", "write", time.Date(2026, 10, 30, 14, 52, 50, 0, time.UTC), false),
		testInvitation("2", "other-org", "tooling", "read", time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC), false),
		testInvitation("3", "example-org", "docs", "maintain", time.Date(2026, 10, 31, 9, 15, 0, 0, time.UTC), false),
		testInvitation("4", "other-org", "expired", "write", time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC), true),
	})
	return &harness{t: t, path: saveConfig(t, cfg), fake: fake}
}

func testInvitation(id, owner, repo, permission string, created time.Time, expired bool) gitprovider.RepositoryInvitation {
	inviter := "example-owner"
	if owner == "other-org" {
		inviter = "other-owner"
	}
	return gitprovider.RepositoryInvitation{
		ID:         gitprovider.InvitationID(id),
		Owner:      owner,
		Repo:       repo,
		Inviter:    gitprovider.Identity{Login: inviter},
		Invitee:    gitprovider.Identity{Login: "review-bot"},
		Permission: permission,
		CreatedAt:  created,
		Expired:    expired,
		URL:        "https://github.com/" + owner + "/" + repo + "/invitations",
	}
}

func (h *harness) factory(_ *cobra.Command, _ *root.Options, _ config.File, profileName string, git config.GitConfig) (Provider, gitprovider.Credential, func(), error) {
	h.factoryCalls++
	h.lastProfile = profileName
	h.lastGit = git
	return h.fake, gitprovider.Credential{Type: "pat", Token: "fake-token"}, func() { h.closed = true }, nil
}

func (h *harness) run(args ...string) (string, error) {
	h.t.Helper()
	stdout, _, err := runWithFactory(h.path, h.factory, args...)
	return stdout, err
}

func runWithFactory(path string, factory ProviderFactory, args ...string) (string, string, error) {
	opts := &root.Options{ConfigPath: path, Stdin: strings.NewReader("")}
	cmd, stdout, stderr := cmdtest.New(opts, func(cmd *cobra.Command, opts *root.Options) {
		RegisterWithFactory(cmd, opts, factory)
	})
	err := root.Execute(cmd, args)
	return stdout.String(), stderr.String(), err
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path) // #nosec G304 -- fixed golden names under testdata.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("%s differs\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func saveConfig(t *testing.T, cfg config.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
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

func fileBackendConfig() config.File {
	cfg := testConfig()
	cfg.Secrets.Stores["test-file"] = config.SecretsStore{
		DisplayName: "Test File Store",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "test-file"
	cfg.Profiles["home"] = home
	return cfg
}

func testConfig() config.File {
	llm := config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
	}
	return configtest.File(
		configtest.WithoutRepositoryProfiles(),
		configtest.HomeProfile(config.Profile{
			Git: config.GitConfig{
				Host:       "github.com",
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/home"},
			},
			LLM:          llm,
			ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
		}),
		configtest.Profile("work", config.Profile{
			Git: config.GitConfig{
				Host:       "github.com",
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/work"},
			},
			ReviewerCredentials: &config.ReviewerCredentials{
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/work-reviewer"},
			},
			LLM:          llm,
			ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
		}),
	)
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(value); err != nil {
		t.Errorf("Encode: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}
