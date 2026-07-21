package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/config/configtest"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitexec"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/gitproviders"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
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
			func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
				return identity, nil
			},
			func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/home-reviewer"},
	}
	cfg.Profiles["home"] = profile

	var providerCalls []config.GitConfig
	repoProvider := &gitprovider.Fake{}
	reviewerProvider := &gitprovider.Fake{}
	repoProvider.SetCapabilities(gitprovider.ProviderCaps{ThreadResolution: true, BundleInlineOnSubmit: true})
	reviewerProvider.SetCapabilities(gitprovider.ProviderCaps{NativeFileLevelComments: true})
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}

	deps := testDependencies(t,
		func(git config.GitConfig, _ credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			providerCalls = append(providerCalls, git)
			if git.Credential.Name == "codereview/home-reviewer" {
				return reviewerProvider, gitprovider.Credential{Type: "pat", Token: "reviewer-token"}, nil
			}
			return repoProvider, gitprovider.Credential{Type: "pat", Token: "repo-token"}, nil
		},
		func(_ context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, _ credentials.Reader, _ config.Profile) (gitprovider.Identity, error) {
			if provider != reviewerProvider || credential.Token != "reviewer-token" {
				t.Fatalf("identity resolver provider=%#v credential=%#v, want reviewer provider/token", provider, credential)
			}
			return identity, nil
		},
		func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	deps.NewGitCommand = func(host string, tokens gitexec.TokenSource, _ string) (func(context.Context, string, ...string) ([]byte, error), error) {
		if host != "github.com" {
			t.Fatalf("Git host = %q, want github.com", host)
		}
		token, err := tokens.AccessToken(context.Background())
		if err != nil {
			t.Fatalf("repository token source: %v", err)
		}
		if token != "repo-token" {
			t.Fatalf("checkout token = %q, want repository token and not reviewer token", token)
		}
		return func(context.Context, string, ...string) ([]byte, error) { return nil, nil }, nil
	}

	runtime, err := Open(context.Background(), OpenRequest{
		Config:       cfg,
		Profile:      profile,
		PRRef:        testPRRef(),
		Dependencies: deps,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if len(providerCalls) != 2 ||
		providerCalls[0].Credential.Name != "codereview/home" ||
		providerCalls[1].Credential.Name != "codereview/home-reviewer" {
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
			func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
				return identity, nil
			},
			func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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
	if !strings.Contains(stderr.String(), `warning: posting identity "review-bot" may not create reviews that count toward PR approval state`) {
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
			func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
				return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}, nil
			},
			func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
}

func TestOpenClosesCredentialStoresWhenRuntimeLayoutFails(t *testing.T) {
	cfg := testConfig()
	layoutErr := errors.New("layout failed")
	var readers []credentials.Reader
	deps := testDependencies(t,
		func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			readers = append(readers, tokenStore)
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		nil,
		nil,
	)
	deps.RuntimeLayout = func() (statepaths.Layout, error) {
		return statepaths.Layout{}, layoutErr
	}

	_, err := Open(context.Background(), OpenRequest{
		Config:       cfg,
		Profile:      cfg.Profiles["home"],
		PRRef:        testPRRef(),
		Dependencies: deps,
	})
	if !errors.Is(err, layoutErr) {
		t.Fatalf("Open error = %v, want layout failure", err)
	}
	assertCredentialReadersClosed(t, readers)
}

func TestOpenClosesCredentialStoresWhenLedgerOpenFails(t *testing.T) {
	cfg := testConfig()
	ledgerErr := errors.New("ledger open failed")
	var readers []credentials.Reader
	deps := testDependencies(t,
		func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			readers = append(readers, tokenStore)
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		nil,
		nil,
	)
	deps.OpenLedger = func(context.Context, string) (*ledger.Store, error) {
		return nil, ledgerErr
	}

	_, err := Open(context.Background(), OpenRequest{
		Config:       cfg,
		Profile:      cfg.Profiles["home"],
		PRRef:        testPRRef(),
		Dependencies: deps,
	})
	if !errors.Is(err, ledgerErr) {
		t.Fatalf("Open error = %v, want ledger failure", err)
	}
	assertCredentialReadersClosed(t, readers)
}

func TestOpenClosesCredentialStoresAndLedgerWhenLimiterCreationFails(t *testing.T) {
	cfg := testConfig()
	limiterErr := errors.New("limiter failed")
	var readers []credentials.Reader
	var ledgerStore *ledger.Store
	deps := testDependencies(t,
		func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			readers = append(readers, tokenStore)
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		nil,
		nil,
	)
	deps.OpenLedger = func(ctx context.Context, _ string) (*ledger.Store, error) {
		store, err := ledger.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
		if err != nil {
			return nil, err
		}
		ledgerStore = store
		return store, nil
	}
	deps.NewLimiter = func() (outbox.Limiter, error) {
		return nil, limiterErr
	}

	_, err := Open(context.Background(), OpenRequest{
		Config:       cfg,
		Profile:      cfg.Profiles["home"],
		PRRef:        testPRRef(),
		Dependencies: deps,
	})
	if !errors.Is(err, limiterErr) {
		t.Fatalf("Open error = %v, want limiter failure", err)
	}
	assertCredentialReadersClosed(t, readers)
	assertLedgerClosed(t, ledgerStore)
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
					func(_ config.GitConfig, _ credentials.Reader, opts gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
						gotLookup = opts.GitHub.InstallationLookup
						gotInstallationID = opts.GitHub.InstallationID
						return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token"}, nil
					},
					func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
						return gitprovider.Identity{Login: "cr-reviewer[bot]", ID: "12345"}, nil
					},
					func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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

func TestOpenSelectionPassesGitHubAppInstallationLookupAndPinnedID(t *testing.T) {
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
				credential := config.CredentialLocation{Store: "test-memory", Name: "codereview/cr-reviewer"}
				profile := cfg.Profiles["home"]
				profile.Git.AuthMode = config.GitAuthModeGitHubApp
				profile.Git.Credential = credential
				profile.ReviewerCredentials = &config.ReviewerCredentials{ // #nosec G101 -- test credential reference, not secret material.
					AuthMode:   config.GitAuthModeGitHubApp,
					Credential: credential,
				}
				profile.Reviewer = config.ProfileReviewer{
					Kind:   config.ProfileReviewerKindEntity,
					Entity: "cr-reviewer",
					GitHubAppInstallation: &config.ProfileReviewerGitHubAppInstallation{
						Mode:           config.ProfileReviewerGitHubAppInstallationPinned,
						InstallationID: "42",
					},
				}
				cfg.Profiles["home"] = profile
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
			runtime, err := OpenSelection(context.Background(), SelectionOpenRequest{
				Config:  cfg,
				Profile: cfg.Profiles["home"],
				PRRef:   testPRRef(),
				Dependencies: Dependencies{
					NewGitProvider: func(_ config.GitConfig, _ credentials.Reader, opts gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
						gotLookup = opts.GitHub.InstallationLookup
						gotInstallationID = opts.GitHub.InstallationID
						return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token"}, nil
					},
					NewAdapter: func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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

func TestOpenSelectionReturnsExecutableSelectionRuntime(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...) // #nosec G204 -- test uses structured Git arguments.
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repoDir, "init", "-b", "main")
	git(repoDir, "config", "user.name", "Selection Test")
	git(repoDir, "config", "user.email", "selection@example.com")
	git(repoDir, "commit", "--allow-empty", "-m", "base")
	baseSHA := git(repoDir, "rev-parse", "HEAD")
	git(repoDir, "commit", "--allow-empty", "-m", "head")
	headSHA := git(repoDir, "rev-parse", "HEAD")

	cfg := testConfig()
	profile := cfg.Profiles["home"]
	ref := testPRRef()
	pr := testPR()
	pr.Base.SHA = baseSHA
	pr.Head.SHA = headSHA
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	tokenCalls := 0
	runtime, err := OpenSelection(ctx, SelectionOpenRequest{
		Config: cfg, Profile: profile, PRRef: ref,
		Dependencies: Dependencies{
			NewGitProvider: func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "repo-token"}, nil
			},
			NewGitCommand: func(_ string, tokens gitexec.TokenSource, _ string) (func(context.Context, string, ...string) ([]byte, error), error) {
				return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
					cmdArgs := append([]string(nil), args...)
					if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
						token, err := tokens.AccessToken(ctx)
						if err != nil || token != "repo-token" {
							t.Fatalf("repository token = %q, %v", token, err)
						}
						tokenCalls++
						cmdArgs[2] = repoDir
					}
					cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- test uses structured Git arguments.
					cmd.Dir = dir
					return cmd.CombinedOutput()
				}, nil
			},
			NewAdapter: func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
			ResolveRepoRoot: func(context.Context) (string, error) { return "", reporoot.ErrUnavailable },
		},
	})
	if err != nil {
		t.Fatalf("OpenSelection: %v", err)
	}
	defer runtime.Cleanup()
	artifactDir := t.TempDir()
	result, err := runtime.Select(ctx, pipeline.SelectionRequest{
		PRRef: ref, ProfileName: "home", Profile: profile,
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		ArtifactDir:     artifactDir,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if tokenCalls == 0 {
		t.Fatal("selection runtime did not use repository token source for fetch")
	}
	if result.Artifacts.Dir != artifactDir {
		t.Fatalf("artifact dir = %q, want %q", result.Artifacts.Dir, artifactDir)
	}
	if _, err := os.Stat(result.Artifacts.WorkbenchMetadataPath()); err != nil {
		t.Fatalf("workbench metadata: %v", err)
	}
}

func TestOpenRejectsBackendOverrideForNamedSecretsStore(t *testing.T) {
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

func TestOpenSelectionUsesNamedSecretsStoreWithoutBackendOverride(t *testing.T) {
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
			NewGitProvider: func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				token, err := tokenStore.Get("home", credentials.GitTokenKey)
				if err != nil {
					t.Fatalf("tokenStore.Get(home, git_token): %v", err)
				}
				if token != "named-store-token" {
					t.Fatalf("token = %q, want named-store-token", token)
				}
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
			},
			NewAdapter: func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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

func TestOpenSelectionClosesCredentialStoresWhenProviderCreationFails(t *testing.T) {
	cfg := testConfig()
	providerErr := errors.New("provider creation failed")
	var readers []credentials.Reader

	_, err := OpenSelection(context.Background(), SelectionOpenRequest{
		Config:  cfg,
		Profile: cfg.Profiles["home"],
		Dependencies: Dependencies{
			NewGitProvider: func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				readers = append(readers, tokenStore)
				return nil, gitprovider.Credential{}, providerErr
			},
		},
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("OpenSelection error = %v, want provider failure", err)
	}
	assertCredentialReadersClosed(t, readers)
}

func TestOpenSelectionClosesCredentialStoresWhenAdapterCreationFails(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.LLM.Auth = config.LLMAuthAPIKey
	profile.LLM.Credential = config.CredentialLocation{Store: "test-memory", Name: "codereview/llm"}
	cfg.Profiles["home"] = profile
	adapterErr := errors.New("adapter creation failed")
	var readers []credentials.Reader

	_, err := OpenSelection(context.Background(), SelectionOpenRequest{
		Config:  cfg,
		Profile: profile,
		Dependencies: Dependencies{
			NewGitProvider: func(_ config.GitConfig, tokenStore credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				readers = append(readers, tokenStore)
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			NewAdapter: func(_ config.LLMConfig, store credentials.Reader) (llm.Adapter, error) {
				readers = append(readers, store)
				return nil, adapterErr
			},
		},
	})
	if !errors.Is(err, adapterErr) {
		t.Fatalf("OpenSelection error = %v, want adapter failure", err)
	}
	assertCredentialReadersClosed(t, readers)
}

func TestOpenInjectsCachedReaderIntoProviderAndAdapter(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.LLM.Provider = config.LLMProviderOpenAI
	profile.LLM.Auth = config.LLMAuthAPIKey
	profile.LLM.Adapter = config.LLMAdapterOpenAIAPI
	profile.LLM.Credential = config.CredentialLocation{Store: "test-memory", Name: "codereview/home"}
	cfg.Profiles["home"] = profile

	var providerReader credentials.Reader
	var postingReader credentials.Reader
	var adapterReader credentials.Reader
	var identityReader credentials.Reader
	runtime, err := Open(context.Background(), OpenRequest{
		Config:  cfg,
		Profile: profile,
		PRRef:   testPRRef(),
		Dependencies: testDependencies(t,
			func(git config.GitConfig, reader credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				if git.Credential.Name == "codereview/home" && providerReader == nil {
					providerReader = reader
				} else {
					postingReader = reader
				}
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(_ context.Context, _ gitprovider.GitProvider, _ gitprovider.Credential, reader credentials.Reader, _ config.Profile) (gitprovider.Identity, error) {
				identityReader = reader
				return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}, nil
			},
			func(_ config.LLMConfig, reader credentials.Reader) (llm.Adapter, error) {
				adapterReader = reader
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer runtime.Cleanup()
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	if got := runner.pipeline.Adapter.Name(); got != "fake-llm" {
		t.Fatalf("adapter name = %q, want fake-llm", got)
	}
	if providerReader != postingReader {
		t.Fatalf("provider reader = %p, posting reader = %p, want shared cached reader", providerReader, postingReader)
	}
	if postingReader != identityReader {
		t.Fatalf("posting reader = %p, identity reader = %p, want shared cached reader", postingReader, identityReader)
	}
	if providerReader != adapterReader {
		t.Fatalf("provider reader = %p, adapter reader = %p, want shared cached reader", providerReader, adapterReader)
	}
}

func TestOpenSelectionInjectsCachedReaderIntoProviderAndAdapter(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.LLM.Provider = config.LLMProviderOpenAI
	profile.LLM.Auth = config.LLMAuthAPIKey
	profile.LLM.Adapter = config.LLMAdapterOpenAIAPI
	profile.LLM.Credential = config.CredentialLocation{Store: "test-memory", Name: "codereview/home"}
	profile.LLM.Credential.Name = "codereview/home"
	cfg.Profiles["home"] = profile

	var providerReader credentials.Reader
	var adapterReader credentials.Reader
	runtime, err := OpenSelection(context.Background(), SelectionOpenRequest{
		Config:  cfg,
		Profile: profile,
		PRRef:   testPRRef(),
		Dependencies: Dependencies{
			NewGitProvider: func(_ config.GitConfig, reader credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				providerReader = reader
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			NewAdapter: func(_ config.LLMConfig, reader credentials.Reader) (llm.Adapter, error) {
				adapterReader = reader
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("OpenSelection: %v", err)
	}
	defer runtime.Cleanup()
	if providerReader != adapterReader {
		t.Fatalf("provider reader = %p, adapter reader = %p, want shared cached reader", providerReader, adapterReader)
	}
}

func TestOpenSelectionCachedReaderReturnsStoreClosedAfterCleanupForCachedKey(t *testing.T) {
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
	profile := cfg.Profiles["home"]
	profile.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = profile

	var providerReader credentials.Reader
	runtime, err := OpenSelection(context.Background(), SelectionOpenRequest{
		Config:  cfg,
		Profile: profile,
		PRRef:   testPRRef(),
		Dependencies: Dependencies{
			NewGitProvider: func(_ config.GitConfig, reader credentials.Reader, _ gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				providerReader = reader
				token, err := reader.Get("home", credentials.GitTokenKey)
				if err != nil {
					t.Fatalf("reader.Get(home, git_token): %v", err)
				}
				if token == "" {
					t.Fatal("reader.Get(home, git_token) returned empty token")
				}
				return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
			},
			NewAdapter: func(_ config.LLMConfig, _ credentials.Reader) (llm.Adapter, error) {
				return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("OpenSelection: %v", err)
	}
	if providerReader == nil {
		t.Fatal("provider reader was not captured")
	}
	runtime.Cleanup()
	if _, err := providerReader.Get("home", credentials.GitTokenKey); !errors.Is(err, credstore.ErrStoreClosed) {
		t.Fatalf("providerReader.Get after Cleanup = %v, want ErrStoreClosed", err)
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
			func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
				return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
			},
			func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
				return identity, nil
			},
			func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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

func TestAdapterConstructorsCoverLLMRuntimeSpecs(t *testing.T) {
	want := make(map[config.LLMAdapter]struct{}, len(config.LLMRuntimeSpecs()))
	wantFastModels := map[config.LLMAdapter][]string{
		config.LLMAdapterClaudeCLI:    {"claude-opus-4-8", "claude-opus-4-7"},
		config.LLMAdapterAnthropicAPI: {"claude-opus-4-8", "claude-opus-4-7"},
		config.LLMAdapterCodexCLI:     {"gpt-5.4", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
	}
	for _, spec := range config.LLMRuntimeSpecs() {
		if _, duplicate := want[spec.Adapter]; duplicate {
			t.Fatalf("duplicate LLM runtime spec for adapter %q", spec.Adapter)
		}
		want[spec.Adapter] = struct{}{}
		if !slices.Equal(spec.FastModeModels, wantFastModels[spec.Adapter]) {
			t.Errorf("runtime spec %q fast models = %#v, want %#v", spec.Adapter, spec.FastModeModels, wantFastModels[spec.Adapter])
		}
	}
	if len(adapterConstructors) != len(want) {
		t.Fatalf("adapter constructors = %d, runtime spec adapters = %d", len(adapterConstructors), len(want))
	}
	for adapter := range want {
		if adapterConstructors[adapter] == nil {
			t.Errorf("adapter constructor missing for runtime spec %q", adapter)
		}
	}
	for adapter := range adapterConstructors {
		if _, ok := want[adapter]; !ok {
			t.Errorf("adapter constructor %q has no runtime spec", adapter)
		}
	}
}

func testDependencies(t *testing.T, provider GitProviderFactory, identity PostingIdentityResolver, adapter AdapterFactory) Dependencies {
	t.Helper()
	if provider == nil {
		provider = func(config.GitConfig, credentials.Reader, gitproviders.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		}
	}
	if identity == nil {
		identity = func(context.Context, gitprovider.GitProvider, gitprovider.Credential, credentials.Reader, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}, nil
		}
	}
	if adapter == nil {
		adapter = func(config.LLMConfig, credentials.Reader) (llm.Adapter, error) {
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

func assertCredentialReadersClosed(t *testing.T, readers []credentials.Reader) {
	t.Helper()
	if len(readers) == 0 {
		t.Fatal("no credential readers captured")
	}
	seen := map[credentials.Reader]bool{}
	for _, reader := range readers {
		if reader == nil || seen[reader] {
			continue
		}
		seen[reader] = true
		if _, err := reader.Get("home", credentials.GitTokenKey); !errors.Is(err, credstore.ErrStoreClosed) {
			t.Fatalf("credential reader Get after Open failure = %v, want ErrStoreClosed", err)
		}
	}
}

func assertLedgerClosed(t *testing.T, store *ledger.Store) {
	t.Helper()
	if store == nil {
		t.Fatal("ledger store was not opened")
	}
	if err := store.DeleteNamedSession(context.Background(), "daily"); !errors.Is(err, ledger.ErrClosed) {
		t.Fatalf("ledger DeleteNamedSession after Open failure = %v, want ErrClosed", err)
	}
}

func testConfig() config.File {
	return configtest.File(
		configtest.WithoutRepositoryProfiles(),
		configtest.HomeProfile(config.Profile{
			Git: config.GitConfig{
				Host:       "github.com",
				AuthMode:   config.GitAuthModePAT,
				Credential: config.CredentialLocation{Store: "test-memory", Name: "codereview/home"},
			},
			LLM: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
		}),
	)
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
