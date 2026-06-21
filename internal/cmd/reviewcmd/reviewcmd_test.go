package reviewcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestReviewDryRunCallsRunnerAndRendersText(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	var gotRuntime RuntimeOptions
	var cleanupCalled bool
	cmd, out := newTestCommand(t, testConfig(), func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, opts RuntimeOptions) (Runtime, error) {
		gotRuntime = opts
		return Runtime{
			Runner:          runner,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
			Cleanup:         func() { cleanupCalled = true },
		}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--agents-dir", "/tmp/agents",
		"--fail-on", "minor",
		"--max-agents", "3",
		"--max-concurrency", "2",
		"--allow-self-review",
		"--allow-self-approve",
		"--no-resolve-threads",
		"--verbose",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.PRRef.Number != 29 || req.ProfileName != "home" || req.PostingIdentity.Login != "review-bot" {
		t.Fatalf("request identity/ref = %#v", req)
	}
	if req.FailOn == nil || *req.FailOn != review.SeverityMinor {
		t.Fatalf("FailOn = %#v, want minor", req.FailOn)
	}
	if len(req.AgentDirs) != 1 || req.AgentDirs[0] != "/tmp/agents" || !req.AllowSelfReview || !req.AllowSelfApprove || !req.NoResolveThreads || !req.MajorRequestChanges || !req.IncludeNits {
		t.Fatalf("request flags = %#v", req)
	}
	if req.SelectionModelOverride != "" || req.SelectionEffortOverride != "" ||
		req.SelectionPromptInstructions != "" ||
		req.ReviewerModelOverride != "" || req.ReviewerEffortOverride != "" {
		t.Fatalf("stage overrides = %#v, want empty when flags omitted", req)
	}
	if gotRuntime.MaxAgents != 3 || gotRuntime.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = %#v, want max agents/concurrency", gotRuntime)
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	if text := out.String(); !strings.Contains(text, "Post mode: dry_run") || !strings.Contains(text, "Planned actions:") {
		t.Fatalf("stdout = %q, want dry-run render", text)
	}
}

func TestReviewUsesRepositoryProfileRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar", "baz"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		if profile.Git.CredentialRef != "codereview/work" {
			t.Fatalf("runtime profile credential ref = %q, want work route", profile.Git.CredentialRef)
		}
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", "https://github.com/rianjs/bar/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].ProfileName != "work" {
		t.Fatalf("request profile = %#v, want work", runner.requests)
	}
}

func TestReviewExplicitProfileBypassesRepositoryRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		if profile.Git.CredentialRef != "codereview/home" {
			t.Fatalf("runtime profile credential ref = %q, want explicit home", profile.Git.CredentialRef)
		}
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"--profile", "home", "review", "https://github.com/rianjs/bar/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].ProfileName != "home" {
		t.Fatalf("request profile = %#v, want home", runner.requests)
	}
}

func TestReviewExplicitEmptyProfileFailsBeforeRepositoryRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for an empty explicit profile")
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "", "review", "https://github.com/rianjs/bar/pull/29", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("Execute error = %v, want empty profile failure", err)
	}
}

func TestReviewUnmatchedRepositoryRequiresProfileOrRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for an unmatched repository")
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	err := root.Execute(cmd, []string{"review", "https://github.com/example/missing/pull/29", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "no repository profile route matched") {
		t.Fatalf("Execute error = %v, want unmatched route failure", err)
	}
}

func TestReviewExplicitProfileHostMismatch(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.Git.Host = "gitlab.com"
	cfg.Profiles["home"] = home
	cfg.RepositoryProfiles = nil
	work := home
	work.Git.Host = "github.com"
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cmd, _ := newTestCommand(t, cfg, func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called when route profile host mismatches")
		return Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want host mismatch")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if !strings.Contains(err.Error(), `PR host "github.com" must match configured git host "gitlab.com"`) {
		t.Fatalf("error = %v, want host mismatch detail", err)
	}
}

func TestReviewHelpDocumentsApprovalFastPaths(t *testing.T) {
	cmd, out := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for help")
		return Runtime{}, nil
	})

	if err := root.Execute(cmd, []string{"review", "--help"}); err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"already approved the PR",
		"--rerun to bypass this",
		"approval override request newer than that marker",
		"--retry-posts is recovery-only",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help = %q, want substring %q", text, want)
		}
	}
}

func TestRuntimeLayoutMigratesLegacyDataAndCache(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLayout := statepaths.NewLayout(filepath.Join(layout.DataRoot, statepaths.AppDir), layout.CacheRoot)
	legacyStore, err := ledger.Open(context.Background(), legacyLayout.LedgerDB())
	if err != nil {
		t.Fatalf("legacy ledger.Open: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("legacy store Close: %v", err)
	}
	writeReviewFile(t, filepath.Join(layout.DataRoot, statepaths.AppDir, "runs", "sentinel.txt"), "run")
	writeReviewFile(t, filepath.Join(layout.CacheRoot, statepaths.AppDir, "http", "sentinel.txt"), "cache")

	got, err := runtimeLayout()
	if err != nil {
		t.Fatalf("runtimeLayout: %v", err)
	}
	if got != layout {
		t.Fatalf("runtime layout = %#v, want %#v", got, layout)
	}
	if _, err := os.Stat(layout.LedgerDB()); err != nil {
		t.Fatalf("new ledger stat: %v", err)
	}
	assertReviewTestFile(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "run")
	assertReviewTestFile(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy data root stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy cache root stat err = %v, want removed", err)
	}
}

func TestNewRuntimeMigratesLegacyDataAndCache(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLayout := statepaths.NewLayout(filepath.Join(layout.DataRoot, statepaths.AppDir), layout.CacheRoot)
	legacyStore, err := ledger.Open(context.Background(), legacyLayout.LedgerDB())
	if err != nil {
		t.Fatalf("legacy ledger.Open: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("legacy store Close: %v", err)
	}
	writeReviewFile(t, filepath.Join(layout.DataRoot, statepaths.AppDir, "runs", "sentinel.txt"), "run")
	writeReviewFile(t, filepath.Join(layout.CacheRoot, statepaths.AppDir, "http", "sentinel.txt"), "cache")

	provider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, testConfig(), testConfig().Profiles["home"], RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if runtime.Runner == nil || runtime.PostingIdentity != identity {
		t.Fatalf("runtime = %#v, want runner and posting identity", runtime)
	}
	if _, err := os.Stat(layout.LedgerDB()); err != nil {
		t.Fatalf("new ledger stat: %v", err)
	}
	assertReviewTestFile(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "run")
	assertReviewTestFile(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy data root stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy cache root stat err = %v, want removed", err)
	}
}

func TestNewRuntimeCreatesCodexCLIWithoutOpenAIAPIKey(t *testing.T) {
	statedirtest.Hermetic(t)
	previousAPIKey, hadAPIKey := os.LookupEnv("OPENAI_API_KEY")
	if err := os.Unsetenv("OPENAI_API_KEY"); err != nil {
		t.Fatalf("Unsetenv(OPENAI_API_KEY): %v", err)
	}
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	t.Cleanup(func() {
		if hadAPIKey {
			_ = os.Setenv("OPENAI_API_KEY", previousAPIKey)
			return
		}
		_ = os.Unsetenv("OPENAI_API_KEY")
	})

	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	profile := cfg.Profiles["home"]
	profile.LLM = config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}
	cfg.Profiles["home"] = profile

	provider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		newAdapter,
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.PostingIdentity != identity {
		t.Fatalf("PostingIdentity = %#v, want %#v", runtime.PostingIdentity, identity)
	}
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	if runner.pipeline.Adapter == nil || runner.pipeline.Adapter.Name() != "codex_cli" {
		t.Fatalf("pipeline adapter = %#v, want codex_cli", runner.pipeline.Adapter)
	}
}

func TestNewRuntimeUsesReviewerCredentialsAsRuntimeProvider(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	profile := cfg.Profiles["work"]
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		Credential:    config.CredentialLocation{Store: "test-memory"},
		CredentialRef: "codereview/work-reviewer",
	}
	cfg.Profiles["work"] = profile

	var providerCalls []config.GitConfig
	reviewerProvider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(git config.GitConfig, _ githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			providerCalls = append(providerCalls, git)
			return reviewerProvider, gitprovider.Credential{Type: "pat", Token: "reviewer-token"}, nil
		},
		func(_ context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, _ githubprovider.TokenStore, _ config.Profile) (gitprovider.Identity, error) {
			if provider != reviewerProvider || credential.Token != "reviewer-token" {
				t.Fatalf("identity resolver got provider=%p credential=%#v, want reviewer provider/token", provider, credential)
			}
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if len(providerCalls) != 1 || providerCalls[0].CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("provider calls = %#v, want reviewer credential ref only", providerCalls)
	}
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	if runner.pipeline.Provider != reviewerProvider || runner.live.Provider != reviewerProvider {
		t.Fatalf("runtime providers = pipeline:%p live:%p, want reviewer provider %p", runner.pipeline.Provider, runner.live.Provider, reviewerProvider)
	}
}

func TestNewRuntimePassesPRRefForGitHubAppInstallationLookup(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	profile := cfg.Profiles["home"]
	profile.Git.AuthMode = config.GitAuthModeGitHubApp
	cfg.Profiles["home"] = profile
	prRef := gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 76}

	var gotLookup *githubprovider.InstallationLookup
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, _ githubprovider.TokenStore, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			gotLookup = opts.InstallationLookup
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token", Login: "cr-reviewer[bot]"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{Login: "cr-reviewer[bot]", ID: "12345"}, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{PRRef: prRef})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if gotLookup == nil || gotLookup.Owner != "open-cli" || gotLookup.Repo != "codereview-cli" {
		t.Fatalf("InstallationLookup = %#v, want PR owner/repo", gotLookup)
	}
}

func TestNewRuntimeRejectsBackendOverrideForNamedSecretsProfile(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	profile := cfg.Profiles["home"]
	cmd := &cobra.Command{}
	cmd.Flags().String(credstore.BackendFlagName, "", "")
	if err := cmd.Flags().Set(credstore.BackendFlagName, "memory"); err != nil {
		t.Fatalf("Set backend flag: %v", err)
	}
	opts := &root.Options{Backend: "memory", Stderr: io.Discard}

	_, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("newRuntime error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestNewRuntimeUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if err := store.Set("home", credentials.GitTokenKey, "named-store-token", credstore.WithOverwrite()); err != nil {
		t.Fatalf("Set(home, git_token): %v", err)
	}

	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	profile := cfg.Profiles["home"]
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, tokenStore githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			token, err := tokenStore.Get("home", credentials.GitTokenKey)
			if err != nil {
				t.Fatalf("tokenStore.Get(home, git_token): %v", err)
			}
			if token != "named-store-token" {
				t.Fatalf("token = %q, want named-store-token", token)
			}
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
}

func TestOpenSelectionRuntimeRejectsBackendOverrideForNamedSecretsProfile(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home

	_, err := OpenSelectionRuntime(context.Background(), "memory", true, cfg, cfg.Profiles["home"])
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("OpenSelectionRuntime error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestOpenSelectionRuntimeUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if err := store.Set("home", credentials.GitTokenKey, "named-store-token", credstore.WithOverwrite()); err != nil {
		t.Fatalf("Set(home, git_token): %v", err)
	}

	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, tokenStore githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			token, err := tokenStore.Get("home", credentials.GitTokenKey)
			if err != nil {
				t.Fatalf("tokenStore.Get(home, git_token): %v", err)
			}
			if token != "named-store-token" {
				t.Fatalf("token = %q, want named-store-token", token)
			}
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{}, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)

	runtime, err := OpenSelectionRuntime(context.Background(), "", false, cfg, cfg.Profiles["home"])
	if err != nil {
		t.Fatalf("OpenSelectionRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
}

func TestNewRuntimeLiveApprovedFastPathDoesNotInitializeAdapter(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	ref, pr := reviewCommandPR()
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	if err := provider.SetReviews(ref, []gitprovider.Review{{
		ID:          "review-approved",
		Author:      identity,
		State:       gitprovider.ReviewStateApproved,
		SubmittedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	adapterCalls := 0
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			adapterCalls++
			return nil, errors.New("adapter should not initialize for approved fast path")
		},
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after newRuntime = %d, want 0", adapterCalls)
	}

	result, err := runtime.Runner.Live(context.Background(), pipeline.Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         profile,
		PostingIdentity: identity,
	}, reviewrun.Flags{})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.Status != gateio.StatusEarlyExit || result.Message != "review already approved" {
		t.Fatalf("Live result = %#v, want approved early exit", result)
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after approved fast path = %d, want 0", adapterCalls)
	}
}

func TestReviewNoPostIsDryRunAlias(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--no-post"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
}

func TestReviewDryRunPassesStageOverrides(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))
	promptPath := filepath.Join(t.TempDir(), "selection.md")
	writeReviewFile(t, promptPath, "Use applies_when as the routing contract.")

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--selection-model", " bench-selection-model ",
		"--selection-effort", " high ",
		"--selection-prompt", promptPath,
		"--reviewer-model", " bench-reviewer-model ",
		"--reviewer-effort", " low ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.SelectionModelOverride != "bench-selection-model" || req.SelectionEffortOverride != "high" {
		t.Fatalf("selection overrides = model:%q effort:%q, want bench-selection-model/high", req.SelectionModelOverride, req.SelectionEffortOverride)
	}
	if req.ReviewerModelOverride != "bench-reviewer-model" || req.ReviewerEffortOverride != "low" {
		t.Fatalf("reviewer overrides = model:%q effort:%q, want bench-reviewer-model/low", req.ReviewerModelOverride, req.ReviewerEffortOverride)
	}
	if req.SelectionPromptInstructions != "Use applies_when as the routing contract." {
		t.Fatalf("selection prompt override instructions = %q", req.SelectionPromptInstructions)
	}
}

func TestReviewDryRunPassesReviewerModelTierOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--reviewer-model-tier", " medium ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	if got := runner.requests[0].ReviewerModelTierOverride; got != "medium" {
		t.Fatalf("reviewer model tier override = %q, want medium", got)
	}
}

func TestReviewNoPostPassesReviewerEffortOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--no-post",
		"--reviewer-effort", "medium",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.SelectionModelOverride != "" || req.SelectionEffortOverride != "" || req.ReviewerModelOverride != "" || req.ReviewerModelTierOverride != "" || req.ReviewerEffortOverride != "medium" {
		t.Fatalf("stage overrides = %#v, want reviewer effort only", req)
	}
}

func TestReviewNoPostPassesReviewerModelTierOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--no-post",
		"--reviewer-model-tier", "large",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.ReviewerModelTierOverride != "large" || req.ReviewerModelOverride != "" {
		t.Fatalf("reviewer overrides = %#v, want reviewer model tier only", req)
	}
}

func TestReviewDryRunPassesReviewSHAOverrides(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--review-base-sha", " 1111111 ",
		"--review-head-sha", " 2222222 ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.ReviewBaseSHA != "1111111" || req.ReviewHeadSHA != "2222222" {
		t.Fatalf("review SHAs = base:%q head:%q, want 1111111/2222222", req.ReviewBaseSHA, req.ReviewHeadSHA)
	}
}

func TestReviewRejectsInvalidReviewSHAOverrides(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "base only", args: []string{"--dry-run", "--review-base-sha", "1111111"}},
		{name: "head only", args: []string{"--dry-run", "--review-head-sha", "2222222"}},
		{name: "blank base", args: []string{"--dry-run", "--review-base-sha", " ", "--review-head-sha", "2222222"}},
		{name: "invalid head", args: []string{"--dry-run", "--review-base-sha", "1111111", "--review-head-sha", "notsha"}},
		{name: "live", args: []string{"--review-base-sha", "1111111", "--review-head-sha", "2222222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid review SHA override")
			}
		})
	}
}

func TestReviewLiveRejectsStageOverridesBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection model", args: []string{"--selection-model", "bench-model"}},
		{name: "selection effort", args: []string{"--selection-effort", "high"}},
		{name: "selection prompt", args: []string{"--selection-prompt", "selection.md"}},
		{name: "reviewer model", args: []string{"--reviewer-model", "bench-model"}},
		{name: "reviewer model tier", args: []string{"--reviewer-model-tier", "medium"}},
		{name: "reviewer effort", args: []string{"--reviewer-effort", "high"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{liveResult: testLiveResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid live stage override")
			}
		})
	}
}

func TestReviewRejectsEmptyStageOverridesBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection model", args: []string{"--dry-run", "--selection-model", " \t "}},
		{name: "selection effort", args: []string{"--dry-run", "--selection-effort", " \t "}},
		{name: "selection prompt", args: []string{"--dry-run", "--selection-prompt", " \t "}},
		{name: "reviewer model", args: []string{"--dry-run", "--reviewer-model", " \t "}},
		{name: "reviewer model tier", args: []string{"--dry-run", "--reviewer-model-tier", " \t "}},
		{name: "reviewer effort", args: []string{"--dry-run", "--reviewer-effort", " \t "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for empty stage override")
			}
		})
	}
}

func TestReviewRejectsInvalidModelEffortBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection", args: []string{"--dry-run", "--selection-effort", "xhigh"}},
		{name: "reviewer", args: []string{"--dry-run", "--reviewer-effort", "xhigh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			err := root.Execute(cmd, append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...))
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid effort")
			}
		})
	}
}

func TestReviewRejectsInvalidReviewerModelTierBeforeRuntimeFactory(t *testing.T) {
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--reviewer-model-tier", "flagship"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for invalid reviewer model tier")
	}
}

func TestReviewRejectsReviewerModelAndReviewerModelTierTogether(t *testing.T) {
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--reviewer-model", "bench-model",
		"--reviewer-model-tier", "medium",
	})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for conflicting reviewer model flags")
	}
}

func TestReviewRejectsRemovedLLMFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--dry-run", "--llm-model", "bench-model"},
		{"--dry-run", "--llm-effort", "high"},
	} {
		cmd, _ := newTestCommand(t, testConfig(), fakeFactory(&fakeRunner{result: testPipelineResult(false)}))
		err := root.Execute(cmd, append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, args...))
		if err == nil {
			t.Fatal("Execute error = nil, want usage error")
		}
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want usage", got)
		}
	}
}

func TestReviewRejectsInvalidSelectionPromptFileBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.md")},
		{name: "directory", path: t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})
			err := root.Execute(cmd, []string{
				"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
				"--dry-run",
				"--selection-prompt", tt.path,
			})
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid selection prompt path")
			}
		})
	}
}

func TestReviewRejectsEmptySelectionPromptFileBeforeRuntimeFactory(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "selection.md")
	writeReviewFile(t, promptPath, "  \n\t  ")
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--selection-prompt", promptPath,
	})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for empty selection prompt file")
	}
}

func TestReviewProfileResolveThreadsNeverDisablesThreadResolution(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.ReviewPolicy.ResolveThreads = config.ResolveThreadsNever
	cfg.Profiles["home"] = profile
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || !runner.requests[0].NoResolveThreads {
		t.Fatalf("request NoResolveThreads = %#v, want true from profile", runner.requests)
	}
}

func TestReviewPassesRetentionConfigToRuntimeFactory(t *testing.T) {
	cfg := testConfig()
	maxAgeDays := 0
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionManualOnly,
	}
	runner := &fakeRunner{result: testPipelineResult(false)}
	var got RuntimeOptions
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, opts RuntimeOptions) (Runtime, error) {
		got = opts
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !got.Retention.LiveForever || !got.RetentionManualOnly {
		t.Fatalf("runtime retention = %#v manual %v, want keep-forever manual-only policy", got.Retention, got.RetentionManualOnly)
	}
}

func TestRetentionPolicyFromConfigDefaultsWhenMaxAgeOmitted(t *testing.T) {
	got := retentionPolicyFromConfig(config.RetentionConfig{})
	if got.LiveForever || got.LiveMaxAge != 0 || got.DryRunMaxAge != 0 {
		t.Fatalf("retention policy = %#v, want zero-value default policy", got)
	}
}

func TestReviewLiveCallsRunnerAndRendersText(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if len(runner.requests) != 0 {
		t.Fatalf("dry runner calls = %d, want 0", len(runner.requests))
	}
	if !runner.liveFlags[0].Rerun || runner.liveFlags[0].RetryPosts {
		t.Fatalf("live flags = %#v, want rerun only", runner.liveFlags[0])
	}
}

func TestReviewLiveSessionPassesNamedSession(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--session", " daily "}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if runner.liveRequests[0].SessionName != "daily" {
		t.Fatalf("SessionName = %q, want daily", runner.liveRequests[0].SessionName)
	}
}

func TestBuildReviewRunnerWiresNamedSessionDependencies(t *testing.T) {
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	}()
	provider := &gitprovider.Fake{}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	var warnings bytes.Buffer
	retention := datalifecycle.RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour}

	runner := buildReviewRunner(
		store,
		provider,
		adapter,
		testConfig().Profiles["home"],
		noopLimiter{},
		statepaths.NewLayout(t.TempDir(), t.TempDir()),
		&warnings,
		RuntimeOptions{MaxAgents: 3, MaxConcurrency: 2, Retention: retention, RetentionManualOnly: true},
	)

	if runner.pipeline.NamedSessions != store {
		t.Fatalf("pipeline NamedSessions = %#v, want ledger store", runner.pipeline.NamedSessions)
	}
	if runner.pipeline.Warnings != &warnings || runner.live.Warnings != &warnings {
		t.Fatalf("warnings not wired through pipeline/live")
	}
	planner, ok := runner.live.Planner.(livePlanner)
	if !ok {
		t.Fatalf("live planner = %T, want livePlanner", runner.live.Planner)
	}
	if planner.opts.NamedSessions != store || planner.opts.Warnings != &warnings {
		t.Fatalf("planner opts did not preserve named-session dependencies: %#v", planner.opts)
	}
	if runner.pipeline.MaxAgents != 3 || runner.pipeline.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = maxAgents:%d maxConcurrency:%d, want 3/2", runner.pipeline.MaxAgents, runner.pipeline.MaxConcurrency)
	}
	if runner.pipeline.Retention != retention || !runner.pipeline.RetentionManualOnly {
		t.Fatalf("pipeline retention = %#v manual %v, want configured manual policy", runner.pipeline.Retention, runner.pipeline.RetentionManualOnly)
	}
	if runner.live.Retention != retention || !runner.live.RetentionManualOnly {
		t.Fatalf("live retention = %#v manual %v, want configured manual policy", runner.live.Retention, runner.live.RetentionManualOnly)
	}
	if planner.opts.Retention != retention || !planner.opts.RetentionManualOnly {
		t.Fatalf("planner retention = %#v manual %v, want configured manual policy", planner.opts.Retention, planner.opts.RetentionManualOnly)
	}
	if warnings.Len() != 0 {
		t.Fatalf("warnings after buildReviewRunner = %q, want none before override classifier invocation", warnings.String())
	}
}

func TestBuildApprovalOverrideClassifierModelResolution(t *testing.T) {
	tests := []struct {
		name         string
		llmConfig    config.LLMConfig
		wantModel    string
		wantWarning  string
		wantDisabled bool
	}{
		{
			name: "small tier preferred",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderOpenAI,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterCodexCLI,
			},
			wantModel: "gpt-5.4-mini",
		},
		{
			name: "medium fallback",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
			wantModel:   "claude-sonnet-4-6",
			wantWarning: "falling back to medium tier",
		},
		{
			name: "disabled without small or medium",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderPi,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterPiRPC,
			},
			wantDisabled: true,
			wantWarning:  "disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &llm.FakeAdapter{}
			adapter.Queue(llm.FakeResult{Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"approval_override_requested":false}`)}})
			var warnings bytes.Buffer

			classifier := buildApprovalOverrideClassifier(config.Profile{LLM: tt.llmConfig}, adapter, &warnings)
			if classifier == nil {
				t.Fatal("classifier = nil, want lazy classifier")
			}
			if warnings.Len() != 0 {
				t.Fatalf("warnings after build = %q, want deferred warnings", warnings.String())
			}
			result, err := classifier.ClassifyApprovalOverride(context.Background(), approvalOverrideClassifierTestRequest())
			if err != nil {
				t.Fatalf("ClassifyApprovalOverride: %v", err)
			}
			requests := adapter.Requests()
			if tt.wantDisabled {
				if result.Approve || len(requests) != 0 {
					t.Fatalf("disabled classifier result = %#v requests=%#v, want no approval or LLM request", result, requests)
				}
			} else if len(requests) != 1 || requests[0].Model != tt.wantModel || requests[0].Effort != "low" {
				t.Fatalf("requests = %#v, want model %q effort low", requests, tt.wantModel)
			}
			if tt.wantWarning != "" && !strings.Contains(warnings.String(), tt.wantWarning) {
				t.Fatalf("warnings = %q, want substring %q", warnings.String(), tt.wantWarning)
			}
			if tt.wantWarning == "" && warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none", warnings.String())
			}
		})
	}
}

func TestReviewLiveRealRunnerHonorsConfiguredRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 30
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR()
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRunForPRKey(t, store, layout, "old-live", "github_other_repo_1", ledger.PostModeLive, now.Add(-31*24*time.Hour))
	oldDryRun := allocateReviewCommandRunForPRKey(t, store, layout, "old-dry", "github_other_repo_1", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			runtimeOpts,
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old live GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
}

func TestReviewLiveSessionThroughRealRunnerPersistsNamedSession(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile

	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("a", 40)
	pr := gitprovider.PR{
		Ref:    ref,
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/29",
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Base: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "main",
			Ref:   "refs/heads/main",
			SHA:   baseSHA,
		},
		Head: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "feature",
			Ref:   "refs/heads/feature",
			SHA:   headSHA,
		},
	}
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("approve", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			statepaths.NewLayout(t.TempDir(), t.TempDir()),
			opts.Stderr,
			runtimeOpts,
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--session", "daily"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	session, err := store.GetNamedSession(context.Background(), "daily")
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if session.ProviderSessionID != "rollup-session" || session.Profile != "home" || session.Model != "claude-sonnet-4-6" {
		t.Fatalf("named session = %#v, want live runner persisted rollup scoped to profile/model", session)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "selection-session" {
		t.Fatalf("resumes = %#v, want rollup resumed from selection", resumes)
	}
}

func TestReviewDryRunRealRunnerHonorsConfiguredRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 30
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR()
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-31*24*time.Hour))
	newLive := allocateReviewCommandRun(t, store, layout, "new-live", ledger.PostModeLive, now.Add(-29*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			runtimeOpts,
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old live GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), newLive.RunID); err != nil {
		t.Fatalf("new live GetRun error = %v, want nil", err)
	}
}

func TestReviewDryRunRealRunnerHonorsConfiguredKeepLiveForever(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 0
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR()
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-365*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			runtimeOpts,
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); err != nil {
		t.Fatalf("old live GetRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
}

func TestReviewDryRunRealRunnerHonorsManualOnlyRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	cfg.Data.Retention = config.RetentionConfig{Enforcement: config.RetentionManualOnly}
	ref, pr := reviewCommandPR()
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-365*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			runtimeOpts,
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); err != nil {
		t.Fatalf("old live GetRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); err != nil {
		t.Fatalf("old dry-run GetRun error = %v, want nil", err)
	}
}

func TestReviewLiveRetryPostsCallsRunner(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if runner.liveFlags[0].Rerun || !runner.liveFlags[0].RetryPosts {
		t.Fatalf("live flags = %#v, want retry-posts only", runner.liveFlags[0])
	}
}

func TestReviewRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing pr", args: []string{"review", "--dry-run"}},
		{name: "bad url", args: []string{"review", "not-a-url", "--dry-run"}},
		{name: "wrong host", args: []string{"--profile", "home", "review", "https://gitlab.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}},
		{name: "bad fail on", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fail-on", "urgent"}},
		{name: "negative agents", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-agents", "-1"}},
		{name: "negative concurrency", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-concurrency", "-1"}},
		{name: "rerun retry conflict", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun", "--retry-posts"}},
		{name: "session dry run", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--session", "daily"}},
		{name: "session no post", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--no-post", "--session", "daily"}},
		{name: "session retry posts", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts", "--session", "daily"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{result: testPipelineResult(false)}
			cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))
			err := root.Execute(cmd, tt.args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage failure")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if len(runner.requests) != 0 {
				t.Fatalf("runner calls = %d, want 0", len(runner.requests))
			}
		})
	}
}

func TestReviewDryRunJSONHasNoTextQuotaPrefix(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "Quota:") {
		t.Fatalf("JSON output contains text quota prefix: %s", out.String())
	}
	var decoded view.ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Run.RunID != "run-1" || decoded.Quota == nil || len(decoded.Actions) != 1 {
		t.Fatalf("decoded = %#v", decoded)
	}
	if decoded.FailOnTriggered || decoded.Artifacts.FindingsJSON != "/tmp/run-1/findings.json" || decoded.Artifacts.RollupMarkdown != "/tmp/run-1/rollup.md" {
		t.Fatalf("decoded artifacts/fail-on = %#v", decoded)
	}
}

func TestReviewFailOnReturnsFailureAfterRendering(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(true)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fail-on", "major"})
	if err == nil {
		t.Fatal("Execute error = nil, want fail-on failure")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure", got)
	}
	if !strings.Contains(out.String(), "Automated PR Review") {
		t.Fatalf("stdout = %q, want rendered review before fail-on error", out.String())
	}
}

func TestReviewLiveFailOnReturnsFailureAfterRendering(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(true)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--fail-on", "major"})
	if err == nil {
		t.Fatal("Execute error = nil, want fail-on failure")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure", got)
	}
	if !strings.Contains(out.String(), "Status: continue") {
		t.Fatalf("stdout = %q, want live render before fail-on error", out.String())
	}
	if !strings.Contains(out.String(), "Fail-on: triggered") {
		t.Fatalf("stdout = %q, want live fail-on signal", out.String())
	}
}

func TestReviewLiveOutboxExitReturnsAfterRendering(t *testing.T) {
	live := testLiveResult(false)
	live.ExitCode = exitcode.UpstreamError
	live.Outbox.ExitCode = exitcode.UpstreamError
	live.Message = "review premises moved"
	runner := &fakeRunner{liveResult: live}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"})
	if err == nil {
		t.Fatal("Execute error = nil, want upstream failure")
	}
	if got := exitcode.FromError(err); got != exitcode.UpstreamError {
		t.Fatalf("exit code = %d, want upstream", got)
	}
	if !strings.Contains(out.String(), "Message: review premises moved") {
		t.Fatalf("stdout = %q, want live render before exit error", out.String())
	}
}

func TestReviewLiveNonUpstreamExitCodesReturnAfterRendering(t *testing.T) {
	tests := []struct {
		name string
		code int
	}{
		{name: "failure", code: exitcode.Failure},
		{name: "auth", code: exitcode.AuthConfigError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := testLiveResult(false)
			live.ExitCode = tt.code
			live.Outbox.ExitCode = tt.code
			runner := &fakeRunner{liveResult: live}
			cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

			err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"})
			if err == nil {
				t.Fatal("Execute error = nil, want live exit failure")
			}
			if got := exitcode.FromError(err); got != tt.code {
				t.Fatalf("exit code = %d, want %d", got, tt.code)
			}
			if !strings.Contains(out.String(), "Exit code: "+strconv.Itoa(tt.code)) {
				t.Fatalf("stdout = %q, want rendered exit code %d", out.String(), tt.code)
			}
		})
	}
}

func TestNewReviewDryRunRejectsInvalidPlannedPayload(t *testing.T) {
	result := testPipelineResult(false)
	result.PlannedActions[0].PayloadJSON = "{bad"

	_, err := newReviewDryRun(result)
	if err == nil {
		t.Fatal("newReviewDryRun error = nil, want invalid payload failure")
	}
	if !strings.Contains(err.Error(), "payload is invalid JSON") {
		t.Fatalf("newReviewDryRun error = %v, want payload JSON failure", err)
	}
}

func TestNewReviewDryRunMapsPlanSummary(t *testing.T) {
	tokensIn := 1200
	wall := int64(5000)
	result := testPipelineResult(false)
	result.Plan.Summary = reviewplan.Summary{
		Reviewers: []reviewplan.ReviewerSummary{{Name: "go:tests", Findings: 2}},
		Threads:   reviewplan.ThreadCounts{Considered: 3, Summarized: 2, Resolved: 1},
		Run: reviewplan.RunSummary{
			ToolVersion:       "0.0.0-test",
			Adapter:           "claude_cli",
			Model:             "sonnet",
			PostingIdentity:   "review-bot",
			SelectedReviewers: []string{"go:tests"},
			WallDurationMS:    &wall,
			Workstreams:       []reviewplan.WorkstreamUsage{{Name: "go:tests", Model: "sonnet", TokensIn: &tokensIn}},
		},
		Totals: reviewplan.AggregateUsage{TokensIn: &tokensIn},
	}

	rendered, err := newReviewDryRun(result)
	if err != nil {
		t.Fatalf("newReviewDryRun: %v", err)
	}
	summary := rendered.Summary
	if len(summary.Reviewers) != 1 || summary.Reviewers[0].Name != "go:tests" || summary.Reviewers[0].Findings != 2 {
		t.Fatalf("summary reviewers = %#v", summary.Reviewers)
	}
	if summary.Threads != (view.ReviewThreadCounts{Considered: 3, Summarized: 2, Resolved: 1}) {
		t.Fatalf("summary threads = %#v", summary.Threads)
	}
	run := summary.Run
	if run.ToolVersion != "0.0.0-test" || run.Adapter != "claude_cli" || run.Model != "sonnet" ||
		run.PostingIdentity != "review-bot" || len(run.SelectedReviewers) != 1 ||
		run.WallDurationMS == nil || *run.WallDurationMS != wall {
		t.Fatalf("summary run = %#v", run)
	}
	if len(run.Workstreams) != 1 || run.Workstreams[0].Name != "go:tests" ||
		run.Workstreams[0].TokensIn == nil || *run.Workstreams[0].TokensIn != tokensIn ||
		run.Workstreams[0].CostUSD != nil {
		t.Fatalf("summary workstreams = %#v", run.Workstreams)
	}
	if rendered.Summary.Totals.TokensIn == nil || *rendered.Summary.Totals.TokensIn != tokensIn || rendered.Summary.Totals.CostUSD != nil {
		t.Fatalf("summary totals = %#v", rendered.Summary.Totals)
	}
}

func TestReviewMapsRunnerError(t *testing.T) {
	runner := &fakeRunner{err: gitprovider.ErrRetryable}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want runner error")
	}
	if got := exitcode.FromError(err); got != exitcode.UpstreamError {
		t.Fatalf("exit code = %d, want upstream", got)
	}
}

func TestReviewMapsUnsafeAgentSourceError(t *testing.T) {
	runner := &fakeRunner{err: fmt.Errorf("%w: profile agent source agents is not trusted", agents.ErrUnsafeSource)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want runner error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
}

type fakeRunner struct {
	result       pipeline.Result
	err          error
	requests     []pipeline.Request
	liveResult   reviewrun.Result
	liveErr      error
	liveRequests []pipeline.Request
	liveFlags    []reviewrun.Flags
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

func (r *fakeRunner) DryRun(_ context.Context, req pipeline.Request) (pipeline.Result, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return pipeline.Result{}, r.err
	}
	return r.result, nil
}

func (r *fakeRunner) Live(_ context.Context, req pipeline.Request, flags reviewrun.Flags) (reviewrun.Result, error) {
	r.liveRequests = append(r.liveRequests, req)
	r.liveFlags = append(r.liveFlags, flags)
	if r.liveErr != nil {
		return reviewrun.Result{}, r.liveErr
	}
	return r.liveResult, nil
}

func fakeFactory(runner *fakeRunner) RuntimeFactory {
	return func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}
}

func withReviewRuntimeSeams(
	t *testing.T,
	providerFactory func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error),
	identityResolver func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error),
	adapterFactory func(config.LLMConfig, *credstore.Store) (llm.Adapter, error),
) {
	t.Helper()
	originalProviderFactory := newGitProvider
	originalIdentityResolver := resolvePostingIdentityForRuntime
	originalAdapterFactory := newAdapterForRuntime
	newGitProvider = providerFactory
	resolvePostingIdentityForRuntime = identityResolver
	newAdapterForRuntime = adapterFactory
	t.Cleanup(func() {
		newGitProvider = originalProviderFactory
		resolvePostingIdentityForRuntime = originalIdentityResolver
		newAdapterForRuntime = originalAdapterFactory
	})
}

func newTestCommand(t *testing.T, cfg config.File, factory RuntimeFactory) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
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

func testConfig() config.File {
	return config.File{
		Keyring: config.KeyringConfig{Backend: "memory"},
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"test-memory": {
					DisplayName: "Test Memory Store",
					Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)},
				},
			},
		},
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		}},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory"},
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{
					MajorEvent: config.ReviewMajorEventRequestChanges,
				},
			},
		},
	}
}

func testPipelineResult(failOnTriggered bool) pipeline.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	side := review.DiffSideRight
	line := 2
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return pipeline.Result{
		Run: ledger.Run{
			RunID:           "run-1",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			PostMode:        ledger.PostModeDryRun,
			PostingIdentity: "review-bot",
			ArtifactPath:    "/tmp/run-1",
		},
		PR: gitprovider.PR{
			Ref:   ref,
			Title: "CR-20",
			URL:   "https://github.com/open-cli-collective/codereview-cli/pull/29",
		},
		PRKey: "github.com_open-cli-collective_codereview-cli_29",
		Artifacts: pipeline.ArtifactPaths{
			Dir:            "/tmp/run-1",
			DiffPatch:      "/tmp/run-1/diff.patch",
			SlicesDir:      "/tmp/run-1/slices",
			FindingsJSON:   "/tmp/run-1/findings.json",
			RollupMarkdown: "/tmp/run-1/rollup.md",
			AgentLogsDir:   "/tmp/run-1/agent-logs",
		},
		QuotaSupported:  true,
		Quota:           llm.Quota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		FailOnTriggered: failOnTriggered,
		Plan: reviewplan.Plan{
			RollupMarkdown: "## Automated PR Review\n\nBody.",
			AnchoredFindings: []reviewplan.AnchoredFinding{{
				FindingID: "finding-1",
				Severity:  review.SeverityMajor,
				FilePath:  "main.go",
				Anchoring: review.AnchoringInline,
				Side:      &side,
				Line:      &line,
				Body:      "Fix this",
			}},
			Actions: []reviewplan.Action{{
				ActionID:  "inline_comment-1",
				Kind:      reviewplan.ActionKindInlineComment,
				FindingID: "finding-1",
				PlannedAt: now,
				Status:    reviewplan.ActionStatusPlannedOnly,
				Marker:    reviewplan.MarkerPlacement{BodyBearing: true},
				InlineComment: &reviewplan.InlineCommentPayload{
					Body:        "Fix this",
					Path:        "main.go",
					Side:        review.DiffSideRight,
					Line:        2,
					SubjectType: review.AnchorKindLine,
				},
			}},
		},
		PlannedActions: []ledger.PlannedAction{{
			ActionID:    "inline_comment-1",
			RunID:       "run-1",
			Kind:        ledger.PlannedActionInlineComment,
			FindingID:   stringPtr("finding-1"),
			PlannedAt:   now,
			PayloadJSON: `{"body":"Fix this","path":"main.go"}`,
			Status:      ledger.PlannedActionPlannedOnly,
		}},
	}
}

func testLiveResult(failOnTriggered bool) reviewrun.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	return reviewrun.Result{
		Status: gateio.StatusContinue,
		Run: ledger.Run{
			RunID:           "run-live",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			PostMode:        ledger.PostModeLive,
			PostingIdentity: "review-bot",
			ArtifactPath:    "/tmp/run-live",
		},
		PR: gitprovider.PR{
			Ref:   ref,
			Title: "CR-21",
			URL:   "https://github.com/open-cli-collective/codereview-cli/pull/29",
		},
		PRKey:           "github.com_open-cli-collective_codereview-cli_29",
		Outbox:          outbox.Result{Outcome: ledger.OutcomeComment, ExitCode: 0, Posted: 2},
		FailOnTriggered: failOnTriggered,
	}
}

func stringPtr(value string) *string {
	return &value
}

func writeReviewAgent(t *testing.T, rootDir string) {
	t.Helper()
	writeReviewFile(t, filepath.Join(rootDir, "harness", "index.yaml"), "name: harness\ndescription: harness category\nowner: owner\n")
	writeReviewFile(t, filepath.Join(rootDir, "harness", "reviewer", "index.yaml"), "name: reviewer\ndescription: reviewer\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n")
	writeReviewFile(t, filepath.Join(rootDir, "harness", "reviewer", "prompt.md"), "Review carefully.")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
}

func writeReviewFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func assertReviewTestFile(t *testing.T, path, want string) {
	t.Helper()
	// #nosec G304 -- test paths are controlled by statedirtest.Hermetic/t.TempDir.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func approvalOverrideClassifierTestRequest() approvaloverride.Request {
	return approvaloverride.Request{
		PR: gitprovider.PR{
			Title:  "Override",
			URL:    "https://example.test/pr/1",
			Author: gitprovider.Identity{Login: "author"},
		},
		PostingIdentity: gitprovider.Identity{Login: "review-bot"},
		LatestMarkerAt:  time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Candidates: []approvaloverride.Candidate{{
			ID:          "1",
			Source:      "issue_comment",
			Body:        "please approve",
			EffectiveAt: time.Date(2026, 6, 10, 12, 1, 0, 0, time.UTC),
		}},
	}
}

func reviewCommandPR() (gitprovider.PRRef, gitprovider.PR) {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("a", 40)
	return ref, gitprovider.PR{
		Ref:    ref,
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/29",
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Base: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "main",
			Ref:   "refs/heads/main",
			SHA:   baseSHA,
		},
		Head: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "feature",
			Ref:   "refs/heads/feature",
			SHA:   headSHA,
		},
	}
}

func allocateReviewCommandRun(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	prKey := "github_open-cli-collective_codereview-cli_29"
	return allocateReviewCommandRunForPRKey(t, store, layout, runID, prKey, mode, started)
}

func allocateReviewCommandRunForPRKey(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID, prKey string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/29",
		RunID:           runID,
		SHA:             strings.Repeat("a", 40),
		BaseSHA:         strings.Repeat("b", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    filepath.Join(layout.DataRoot, "runs", prKey, strings.Repeat("a", 40), strings.Repeat("b", 40), "home__review-bot", runID),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func reviewSmallDiff(path string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"index 1111111..2222222 100644",
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -1,2 +1,2 @@",
		" package main",
		"-var changed = false",
		"+var changed = true",
		"",
	}, "\n")
}

func fakeReviewLLMResult(sessionID, structured string) llm.FakeResult {
	return llm.FakeResult{
		SessionID: sessionID,
		Response:  llm.Response{StructuredOutput: []byte(structured), DurationMS: 1},
	}
}

func reviewRollupJSON(event string, ordered []string) string {
	payload := map[string]any{
		"schema_version":         1,
		"review_event":           event,
		"review_event_rationale": "policy",
		"dedupe_log":             []any{},
		"ordered_findings":       ordered,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal rollup: %v", err))
	}
	return string(data)
}

func TestFakeFactoryErrorIsReturned(t *testing.T) {
	factoryErr := errors.New("factory failed")
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		return Runtime{}, factoryErr
	})
	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Execute error = %v, want factory error", err)
	}
}

func TestNewAdapterCreatesCodexCLI(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "codex_cli" {
		t.Fatalf("adapter.Name = %q, want codex_cli", adapter.Name())
	}
}

func TestNewAdapterCreatesPiRPC(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderPi,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterPiRPC,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "pi_rpc" {
		t.Fatalf("adapter.Name = %q, want pi_rpc", adapter.Name())
	}
}

func TestNewAdapterCreatesClaudeCLIWithResume(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "claude_cli" || !adapter.SupportsResume() {
		t.Fatalf("adapter = %s resume=%v, want claude_cli with resume", adapter.Name(), adapter.SupportsResume())
	}
}

func TestNewAdapterRejectsCodexCLINonSubscription(t *testing.T) {
	_, err := newAdapter(config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterCodexCLI,
		CredentialRef: "codereview/openai",
	}, nil)
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("newAdapter error = %v, want config.ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "requires provider openai with subscription auth") {
		t.Fatalf("newAdapter error = %v, want Codex compatibility guidance", err)
	}
}

func TestNewAdapterRejectsPiRPCNonSubscription(t *testing.T) {
	_, err := newAdapter(config.LLMConfig{
		Provider:      config.LLMProviderPi,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterPiRPC,
		CredentialRef: "codereview/pi",
	}, nil)
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("newAdapter error = %v, want config.ErrUnsupported", err)
	}
}
