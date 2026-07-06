package reviewruntime

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

func TestOpenCanInstantiateWithoutCobra(t *testing.T) {
	cfg := testConfig()
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	runtime, err := Open(context.Background(), OpenRequest{
		Config:  cfg,
		Profile: cfg.Profiles["home"],
		PRRef:   testPRRef(),
		Dependencies: testDependencies(t,
			func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
				return identity, nil
			},
			func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Runner == nil || runtime.Responder == nil || runtime.PostingIdentity != identity {
		t.Fatalf("runtime = %#v, want runner, responder, and posting identity", runtime)
	}
}

func TestOpenUsesReviewerCredentialsAsRuntimeProvider(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/home-reviewer"},
		CredentialRef: "codereview/home-reviewer",
	}
	cfg.Profiles["home"] = profile

	var providerCalls []config.GitConfig
	repoProvider := &gitprovider.Fake{}
	reviewerProvider := &gitprovider.Fake{}
	repoProvider.SetCapabilities(gitprovider.ProviderCaps{ThreadResolution: true, BundleInlineOnSubmit: true})
	reviewerProvider.SetCapabilities(gitprovider.ProviderCaps{NativeFileLevelComments: true})
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}

	runtime, err := Open(context.Background(), OpenRequest{
		Config:  cfg,
		Profile: profile,
		PRRef:   testPRRef(),
		Dependencies: testDependencies(t,
			func(git config.GitConfig, _ githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				providerCalls = append(providerCalls, git)
				if git.CredentialRef == "codereview/home-reviewer" {
					return reviewerProvider, gitprovider.Credential{Type: "pat", Token: "reviewer-token"}, nil
				}
				return repoProvider, gitprovider.Credential{Type: "pat", Token: "repo-token"}, nil
			},
			func(_ context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, _ githubprovider.TokenStore, _ config.Profile) (gitprovider.Identity, error) {
				if provider != reviewerProvider || credential.Token != "reviewer-token" {
					t.Fatalf("identity resolver provider=%#v credential=%#v, want reviewer provider/token", provider, credential)
				}
				return identity, nil
			},
			func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if len(providerCalls) != 2 ||
		providerCalls[0].CredentialRef != "codereview/home" ||
		providerCalls[1].CredentialRef != "codereview/home-reviewer" {
		t.Fatalf("provider calls = %#v, want repository read then reviewer posting providers", providerCalls)
	}
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	liveProvider, ok := runner.live.Provider.(runtimeProvider)
	if !ok {
		t.Fatalf("live provider = %#v, want split runtime provider", runner.live.Provider)
	}
	if liveProvider.read != repoProvider || liveProvider.write != reviewerProvider {
		t.Fatalf("live provider = %#v, want read repo/write reviewer providers", liveProvider)
	}
	if got := liveProvider.Capabilities(); got.ThreadResolution || got.BundleInlineOnSubmit || !got.NativeFileLevelComments {
		t.Fatalf("live provider capabilities = %#v, want write-provider capabilities only", got)
	}
	ref := testPRRef()
	if _, err := liveProvider.PostIssueComment(context.Background(), ref, "rollup body"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if _, err := liveProvider.SubmitReview(context.Background(), ref, gitprovider.ReviewRequest{
		CommitSHA: "abc123",
		Event:     review.ReviewEventComment,
		Body:      "review body",
	}); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if got := repoProvider.RecordedIssueComments(ref); len(got) != 0 {
		t.Fatalf("repo provider issue comments = %#v, want none", got)
	}
	if got := reviewerProvider.RecordedIssueComments(ref); len(got) != 1 || got[0] != "rollup body" {
		t.Fatalf("reviewer provider issue comments = %#v, want rollup body", got)
	}
}

func TestOpenWarnsAndContinuesWhenOpinionatedReviewAuthorityIsIneligible(t *testing.T) {
	cfg := testConfig()
	ref := testPRRef()
	provider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	if err := provider.SetReviewAuthority(ref, identity.Login, gitprovider.ReviewAuthority{Eligible: false, Permission: "none"}); err != nil {
		t.Fatalf("SetReviewAuthority: %v", err)
	}
	var stderr bytes.Buffer
	runtime, err := Open(context.Background(), OpenRequest{
		Config:                            cfg,
		Profile:                           cfg.Profiles["home"],
		PRRef:                             ref,
		RequireOpinionatedReviewAuthority: true,
		Warnings:                          &stderr,
		Dependencies: testDependencies(t,
			func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
				return identity, nil
			},
			func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if !strings.Contains(stderr.String(), `warning: posting identity "review-bot" may not create GitHub reviews`) {
		t.Fatalf("stderr = %q, want advisory review authority warning", stderr.String())
	}
}

func TestOpenAbortsWhenOpinionatedReviewAuthorityProbeIsCanceled(t *testing.T) {
	cfg := testConfig()
	provider := &gitprovider.Fake{}
	provider.SetError(gitprovider.OperationReviewAuthority, context.Canceled)
	_, err := Open(context.Background(), OpenRequest{
		Config:                            cfg,
		Profile:                           cfg.Profiles["home"],
		PRRef:                             testPRRef(),
		RequireOpinionatedReviewAuthority: true,
		Dependencies: testDependencies(t,
			func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
				return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}, nil
			},
			func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
}

func TestOpenPassesGitHubAppInstallationLookupAndPinnedID(t *testing.T) {
	tests := []struct {
		name               string
		mutate             func(*config.File)
		wantLookup         *githubprovider.InstallationLookup
		wantInstallationID string
	}{
		{
			name: "repository lookup",
			mutate: func(cfg *config.File) {
				profile := cfg.Profiles["home"]
				profile.Git.AuthMode = config.GitAuthModeGitHubApp
				cfg.Profiles["home"] = profile
			},
			wantLookup: &githubprovider.InstallationLookup{Owner: "open-cli", Repo: "codereview-cli"},
		},
		{
			name: "pinned reviewer installation",
			mutate: func(cfg *config.File) {
				cfg.ReviewerEntities = map[string]config.ReviewerEntity{
					"cr-reviewer": {
						Host:     "github.com",
						AuthMode: config.GitAuthModeGitHubApp,
						Credential: config.CredentialLocation{
							Store: "test-memory",
							Name:  "codereview/cr-reviewer",
						},
					},
				}
				profile := cfg.Profiles["home"]
				profile.Reviewer = config.ProfileReviewer{
					Kind:   config.ProfileReviewerKindEntity,
					Entity: "cr-reviewer",
					GitHubAppInstallation: &config.ProfileReviewerGitHubAppInstallation{
						Mode:           config.ProfileReviewerGitHubAppInstallationPinned,
						InstallationID: "42",
					},
				}
				cfg.Profiles["home"] = profile
				*cfg = config.Normalize(*cfg)
			},
			wantInstallationID: "42",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			var gotLookup *githubprovider.InstallationLookup
			var gotInstallationID string
			runtime, err := Open(context.Background(), OpenRequest{
				Config:  cfg,
				Profile: cfg.Profiles["home"],
				PRRef:   testPRRef(),
				Dependencies: testDependencies(t,
					func(_ config.GitConfig, _ githubprovider.TokenStore, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
						gotLookup = opts.InstallationLookup
						gotInstallationID = opts.InstallationID
						return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token"}, nil
					},
					func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
						return gitprovider.Identity{Login: "cr-reviewer[bot]", ID: "12345"}, nil
					},
					func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
						return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
					},
				),
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if runtime.Cleanup != nil {
				runtime.Cleanup()
			}
			if tt.wantLookup == nil {
				if gotLookup != nil {
					t.Fatalf("InstallationLookup = %#v, want nil", gotLookup)
				}
			} else if gotLookup == nil || gotLookup.Owner != tt.wantLookup.Owner || gotLookup.Repo != tt.wantLookup.Repo {
				t.Fatalf("InstallationLookup = %#v, want %#v", gotLookup, tt.wantLookup)
			}
			if gotInstallationID != tt.wantInstallationID {
				t.Fatalf("InstallationID = %q, want %q", gotInstallationID, tt.wantInstallationID)
			}
		})
	}
}

func TestOpenRejectsBackendOverrideForNamedSecretsProfile(t *testing.T) {
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

	_, err := Open(context.Background(), OpenRequest{
		Config:             cfg,
		Profile:            cfg.Profiles["home"],
		Backend:            "memory",
		BackendFlagChanged: true,
		Dependencies:       testDependencies(t, nil, nil, nil),
	})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Open error = %v, want ErrInvalid", err)
	}
}

func TestOpenSelectionUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
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
	runtime, err := OpenSelection(context.Background(), SelectionOpenRequest{
		Config:  cfg,
		Profile: cfg.Profiles["home"],
		Dependencies: Dependencies{
			NewGitProvider: func(_ config.GitConfig, tokenStore githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				token, err := tokenStore.Get("home", credentials.GitTokenKey)
				if err != nil {
					t.Fatalf("tokenStore.Get(home, git_token): %v", err)
				}
				if token != "named-store-token" {
					t.Fatalf("token = %q, want named-store-token", token)
				}
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
			},
			NewAdapter: func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("OpenSelection: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
}

func TestOpenLiveApprovedFastPathDoesNotInitializeAdapter(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	ref := testPRRef()
	pr := testPR()
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
	runtime, err := Open(context.Background(), OpenRequest{
		Config:  cfg,
		Profile: profile,
		PRRef:   ref,
		Dependencies: testDependencies(t,
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
		),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after Open = %d, want 0", adapterCalls)
	}
	result, err := runtime.Runner.Live(context.Background(), pipelineRequest(ref, pr, profile, identity), reviewrun.Flags{})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.Status != "early_exit" || result.Message != "review already approved" {
		t.Fatalf("Live result = %#v, want approved early exit", result)
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after approved fast path = %d, want 0", adapterCalls)
	}
}

func TestNewAdapterCreatesSupportedCLIAdapters(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.LLMConfig
		want string
	}{
		{name: "codex", cfg: config.LLMConfig{Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI}, want: "codex_cli"},
		{name: "pi", cfg: config.LLMConfig{Provider: config.LLMProviderPi, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterPiRPC}, want: "pi_rpc"},
		{name: "claude", cfg: config.LLMConfig{Provider: config.LLMProviderAnthropic, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterClaudeCLI}, want: "claude_cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, err := newAdapter(tt.cfg, nil)
			if err != nil {
				t.Fatalf("newAdapter: %v", err)
			}
			if adapter.Name() != tt.want {
				t.Fatalf("adapter name = %q, want %q", adapter.Name(), tt.want)
			}
		})
	}
}

func testDependencies(t *testing.T, provider GitProviderFactory, identity PostingIdentityResolver, adapter AdapterFactory) Dependencies {
	t.Helper()
	if provider == nil {
		provider = func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		}
	}
	if identity == nil {
		identity = func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}, nil
		}
	}
	if adapter == nil {
		adapter = func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		}
	}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	return Dependencies{
		NewGitProvider:         provider,
		ResolvePostingIdentity: identity,
		NewAdapter:             adapter,
		RuntimeLayout: func() (statepaths.Layout, error) {
			return layout, nil
		},
		OpenLedger: func(ctx context.Context, _ string) (*ledger.Store, error) {
			return ledger.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
		},
		NewLimiter: func() (outbox.Limiter, error) {
			return noopLimiter{}, nil
		},
	}
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
			},
		},
	}
}

func testPRRef() gitprovider.PRRef {
	return gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 29}
}

func testPR() gitprovider.PR {
	ref := testPRRef()
	return gitprovider.PR{
		Ref:    ref,
		URL:    "https://github.com/open-cli/codereview-cli/pull/29",
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Base: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "main",
			Ref:   "refs/heads/main",
			SHA:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		Head: gitprovider.PRBranchRef{
			Host:  ref.Host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  "feature",
			Ref:   "refs/heads/feature",
			SHA:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
}

func pipelineRequest(ref gitprovider.PRRef, pr gitprovider.PR, profile config.Profile, identity gitprovider.Identity) pipeline.Request {
	return pipeline.Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         profile,
		PostingIdentity: identity,
	}
}
