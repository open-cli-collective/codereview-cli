package noleak

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/agentscmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/datacmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/initcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/mecmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/sessionscmd"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/identity"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestCommandSurfacesDoNotLeakSeededSecrets(t *testing.T) {
	type commandCase struct {
		name    string
		prepare func(*testing.T, *auditHarness)
		args    func(*auditHarness) []string
		env     func(*auditHarness) map[string]string
		wantErr bool
	}

	cases := []commandCase{
		{
			name: "init rejects ambient backend env credential ingress",
			args: func(h *auditHarness) []string {
				return []string{
					"--backend", string(credstore.BackendFile),
					"init",
					"--non-interactive",
					"--git-token-from-env", "CR_NOLEAK_GIT",
					"--reviewer-token-from-env", "CR_NOLEAK_REVIEWER",
					"--llm-auth", string(config.LLMAuthAPIKey),
					"--llm-adapter", string(config.LLMAdapterAnthropicAPI),
					"--llm-api-key-from-env", "CR_NOLEAK_LLM",
					"--agent-source", h.agentDir,
				}
			},
			env:     secretEnv,
			wantErr: true,
		},
		{
			name:    "set credential text",
			prepare: saveConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--store", auditCredentialStoreID, "--name", "codereview/default", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_GIT", "--overwrite"}
			},
			env: secretEnv,
		},
		{
			name:    "set credential json",
			prepare: saveConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--store", auditCredentialStoreID, "--name", "codereview/default-reviewer", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_REVIEWER", "--overwrite", "--json"}
			},
			env: secretEnv,
		},
		{
			name:    "set github app private key json",
			prepare: saveGitHubAppReviewerConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--store", auditCredentialStoreID, "--name", "codereview/default-app", "--key", credentials.GitHubAppPrivateKeyKey, "--from-env", "CR_NOLEAK_APP_PRIVATE_KEY", "--overwrite", "--json"}
			},
			env: secretEnv,
		},
		{
			name:    "set github app installation id text",
			prepare: saveGitHubAppReviewerConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--store", auditCredentialStoreID, "--name", "codereview/default-app", "--key", credentials.GitHubAppInstallationIDKey, "--from-env", "CR_NOLEAK_APP_INSTALLATION_ID", "--overwrite"}
			},
			env:     secretEnv,
			wantErr: true,
		},
		{
			name:    "set credential duplicate failure",
			prepare: seedConfiguredCredentials,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--store", auditCredentialStoreID, "--name", "codereview/default", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_GIT"}
			},
			env:     secretEnv,
			wantErr: true,
		},
		{
			name:    "config show text",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("config", "show"),
		},
		{
			name:    "config show json",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("config", "show", "--json"),
		},
		{
			name:    "config show github app json",
			prepare: seedGitHubAppConfigShowCredentials,
			args:    staticArgs("config", "show", "--json"),
		},
		{
			name:    "config clear json",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("config", "clear", "--json"),
		},
		{
			name:    "me text",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("me"),
		},
		{
			name:    "me json",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("me", "--json"),
		},
		{
			name:    "me github app json",
			prepare: seedGitHubAppGitCredentials,
			args:    staticArgs("me", "--json"),
		},
		{
			name:    "agents list json",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("agents", "list", "--json"),
		},
		{
			name:    "agents list pr json",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"agents", "list", "--json", h.prURL}
			},
		},
		{
			name:    "agents show text",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("agents", "show", "harness:reviewer"),
		},
		{
			name:    "agents usage failure",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("agents", "list", "not-a-url"),
			wantErr: true,
		},
		{
			name:    "review dry-run text",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", "--dry-run", h.prURL}
			},
		},
		{
			name:    "review dry-run json",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", "--dry-run", "--json", h.prURL}
			},
		},
		{
			name:    "review live text",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", h.prURL}
			},
		},
		{
			name:    "review live json",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", "--json", h.prURL}
			},
		},
		{
			name:    "review github app reviewer live",
			prepare: seedGitHubAppReviewerCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", h.prURL}
			},
		},
		{
			name:    "review github app git live",
			prepare: seedGitHubAppGitLookupCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", h.prURL}
			},
		},
		{
			name:    "review usage failure",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("review", "not-a-url"),
			wantErr: true,
		},
		{
			name:    "sessions list json",
			prepare: prepareNamedSession,
			args:    staticArgs("sessions", "list", "--json"),
		},
		{
			name:    "sessions show text",
			prepare: prepareNamedSession,
			args:    staticArgs("sessions", "show", "daily"),
		},
		{
			name:    "sessions delete json",
			prepare: prepareNamedSession,
			args:    staticArgs("sessions", "delete", "daily", "--json"),
		},
		{
			name:    "data show text",
			prepare: prepareRunData,
			args:    staticArgs("data", "show"),
		},
		{
			name:    "data prune json dry-run",
			prepare: prepareRunData,
			args:    staticArgs("data", "prune", "--dry-run", "--json", "--older-than", "1h"),
		},
		{
			name:    "data purge json dry-run",
			prepare: prepareRunData,
			args:    staticArgs("data", "purge", "--dry-run", "--json"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuditHarness(t)
			if tc.prepare != nil {
				tc.prepare(t, h)
			}
			if tc.env != nil {
				for key, value := range tc.env(h) {
					t.Setenv(key, value)
				}
			}

			stdout, stderr, err := h.run(tc.args(h))
			if tc.wantErr && err == nil {
				t.Fatal("command error = nil, want failure path")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("command error = %v; stdout = %q; stderr = %q", err, stdout, stderr)
			}

			h.assertNoLeaks(t, "stdout", []byte(stdout))
			h.assertNoLeaks(t, "stderr", []byte(stderr))
			if err != nil {
				h.assertNoLeaks(t, "returned error", []byte(err.Error()))
			}
			h.assertOwnedFilesDoNotLeak(t)
		})
	}
}

type auditHarness struct {
	t                *testing.T
	configPath       string
	configRoot       string
	layout           statepaths.Layout
	agentDir         string
	githubURL        string
	graphQLURL       string
	llmURL           string
	prRef            gitprovider.PRRef
	prURL            string
	prKey            string
	headSHA          string
	baseSHA          string
	workbenchRepoDir string
	now              time.Time
	llmCalls         atomic.Int32

	gitSecret                     string
	reviewerSecret                string
	llmSecret                     string
	keyringSecret                 string
	githubAppIDSecret             string
	githubAppPrivateKey           string
	githubAppInstallationIDSecret string
	githubAppInstallationToken    string
	githubAppSlug                 string
	secretMu                      sync.Mutex
	secrets                       []string
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	rootDir := statedirtest.Hermetic(t)
	keyringSecret := "noleak-file-keyring-passphrase" // #nosec G101 -- distinctive test canary, not a real passphrase.
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", keyringSecret)
	appPrivateKey := noLeakPrivateKeyPEM(t)

	configPath, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	workbench := newNoLeakWorkbenchFixture(t)
	baseSHA, headSHA := workbench.baseSHA, workbench.headSHA
	h := &auditHarness{ // #nosec G101 -- these are distinctive test canaries, not real credentials.
		t:                             t,
		configPath:                    configPath,
		configRoot:                    filepath.Dir(configPath),
		layout:                        layout,
		agentDir:                      filepath.Join(rootDir, "agents"),
		headSHA:                       headSHA,
		baseSHA:                       baseSHA,
		workbenchRepoDir:              workbench.repoDir,
		now:                           time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
		gitSecret:                     "cr-noleak-git-token-0001",
		reviewerSecret:                "cr-noleak-reviewer-token-0002",
		llmSecret:                     "cr-noleak-llm-key-0003",
		keyringSecret:                 keyringSecret,
		githubAppIDSecret:             "1004005006",
		githubAppPrivateKey:           appPrivateKey,
		githubAppInstallationIDSecret: "42424242",
		githubAppInstallationToken:    "cr-noleak-installation-token-0004",
		githubAppSlug:                 "codereview-noleak",
	}
	githubServer := httptest.NewServer(http.HandlerFunc(h.handleGitHub))
	t.Cleanup(githubServer.Close)
	llmServer := httptest.NewServer(http.HandlerFunc(h.handleLLM))
	t.Cleanup(llmServer.Close)

	h.githubURL = githubServer.URL
	h.graphQLURL = githubServer.URL + "/graphql"
	h.llmURL = llmServer.URL
	host := strings.TrimPrefix(githubServer.URL, "http://")
	h.prRef = gitprovider.PRRef{Host: host, Owner: "open-cli-collective", Repo: "codereview-cli", Number: 68}
	h.prURL = fmt.Sprintf("https://%s/%s/%s/pull/%d", h.prRef.Host, h.prRef.Owner, h.prRef.Repo, h.prRef.Number)
	prKey, err := statepaths.PRKey(h.prRef.Host, h.prRef.Owner, h.prRef.Repo, h.prRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	h.prKey = prKey
	h.secrets = []string{
		h.gitSecret,
		h.reviewerSecret,
		h.llmSecret,
		h.keyringSecret,
		h.githubAppPrivateKey,
		h.githubAppInstallationIDSecret,
		h.githubAppInstallationToken,
	}
	writeAgent(t, h.agentDir, "harness", "reviewer", "No-leak harness reviewer.", "Review changed Go files without mentioning credentials.\n")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
	return h
}

type noLeakWorkbenchFixture struct {
	repoDir string
	baseSHA string
	headSHA string
}

func newNoLeakWorkbenchFixture(t *testing.T) noLeakWorkbenchFixture {
	t.Helper()
	repoDir := t.TempDir()
	noLeakGitMustSucceed(t, repoDir, "init", "-b", "main")
	noLeakGitMustSucceed(t, repoDir, "config", "user.name", "NoLeak Test")
	noLeakGitMustSucceed(t, repoDir, "config", "user.email", "noleak@example.com")
	noLeakGitMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.MkdirAll(filepath.Join(repoDir, ".codereview", "agents", "harness", "reviewer"), 0o700); err != nil {
		t.Fatalf("mkdir repo guidance: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".codereview", "agents", "harness", "index.yaml"), []byte("name: harness\ndescription: harness category\nowner: owner\n"), 0o600); err != nil {
		t.Fatalf("write repo category: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".codereview", "agents", "harness", "reviewer", "index.yaml"), []byte("name: reviewer\ndescription: noleak repo reviewer\nmodel_tier: medium\neffort: medium\n"), 0o600); err != nil {
		t.Fatalf("write repo agent index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".codereview", "agents", "harness", "reviewer", "prompt.md"), []byte("Review changed Go files without mentioning credentials.\n"), 0o600); err != nil {
		t.Fatalf("write repo agent prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	noLeakGitMustSucceed(t, repoDir, "add", ".codereview/agents", "main.go")
	noLeakGitMustSucceed(t, repoDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(noLeakGitOutput(t, repoDir, "rev-parse", "HEAD"))
	noLeakGitMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go: %v", err)
	}
	noLeakGitMustSucceed(t, repoDir, "commit", "-am", "head")
	headSHA := strings.TrimSpace(noLeakGitOutput(t, repoDir, "rev-parse", "HEAD"))
	return noLeakWorkbenchFixture{repoDir: repoDir, baseSHA: baseSHA, headSHA: headSHA}
}

func noLeakGitMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(noLeakGitOutput(t, dir, args...))
}

func noLeakGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func noLeakGitCommand(ref gitprovider.PRRef) func(context.Context, string, ...string) ([]byte, error) {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "remote" && args[1] == "get-url" && args[2] == "origin" {
			return []byte(fmt.Sprintf("https://%s/%s/%s.git\n", ref.Host, ref.Owner, ref.Repo)), nil
		}
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			message := strings.TrimSpace(string(out))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
		}
		return out, nil
	}
}

func (h *auditHarness) run(args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	args = auditArgsWithExplicitProfile(args)
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: h.configPath,
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	configcmd.Register(cmd, opts)
	credentialcmd.Register(cmd, opts)
	initcmd.Register(cmd, opts)
	datacmd.Register(cmd, opts)
	sessionscmd.Register(cmd, opts)
	mecmd.RegisterWithFactory(cmd, opts, h.identityFactory)
	agentscmd.RegisterWithFactory(cmd, opts, h.providerFactory)
	reviewcmd.RegisterWithFactory(cmd, opts, h.reviewRuntimeFactory)

	err := root.Execute(cmd, args)
	return stdout.String(), stderr.String(), err
}

func auditArgsWithExplicitProfile(args []string) []string {
	if len(args) == 0 || auditArgsIncludeProfile(args) {
		return args
	}
	command := auditRootCommand(args)
	switch command {
	case "config", "me", "agents", "review":
		out := []string{"--profile", "default"}
		out = append(out, args...)
		return out
	default:
		return args
	}
}

func auditArgsIncludeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "--profile" || strings.HasPrefix(arg, "--profile=") {
			return true
		}
	}
	return false
}

func auditRootCommand(args []string) string {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "" {
			continue
		}
		if arg[0] != '-' {
			return arg
		}
		if arg == "--backend" || arg == "--config" || arg == "--profile" {
			index++
		}
	}
	return ""
}

const auditCredentialStoreID = "test-file"

func (h *auditHarness) config() config.File {
	maxAgeDays := 30
	return config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				auditCredentialStoreID: {
					DisplayName: "No-leak file store",
					Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"default": {
				Git: config.GitConfig{
					Host:          h.prRef.Host,
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: auditCredentialStoreID, Name: "codereview/default"},
					CredentialRef: "codereview/default",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: auditCredentialStoreID, Name: "codereview/default-reviewer"},
					CredentialRef: "codereview/default-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					Credential:    config.CredentialLocation{Store: auditCredentialStoreID, Name: "codereview/default-llm"},
					CredentialRef: "codereview/default-llm",
					ModelMap:      config.ModelMap{"medium": "claude-sonnet-4-6"},
				},
				AgentSources: []string{h.agentDir},
				ReviewPolicy: config.ReviewPolicy{
					MajorEvent:     config.ReviewMajorEventComment,
					ResolveThreads: config.ResolveThreadsNever,
				},
			},
		},
		Data: config.DataConfig{Retention: config.RetentionConfig{
			MaxAgeDays:  &maxAgeDays,
			Enforcement: config.RetentionAtWrite,
		}},
	}
}

func (h *auditHarness) githubAppGitConfig() config.File {
	cfg := h.config()
	profile := cfg.Profiles["default"]
	profile.Git.AuthMode = config.GitAuthModeGitHubApp
	profile.Git.GitHubApp = &config.GitHubAppConfig{AppID: h.githubAppIDSecret}
	profile.Git.Credential = config.CredentialLocation{Store: auditCredentialStoreID, Name: "codereview/default-app"}
	profile.Git.CredentialRef = "codereview/default-app"
	cfg.Profiles["default"] = profile
	return cfg
}

func (h *auditHarness) saveConfig(t *testing.T) {
	t.Helper()
	h.saveConfigFile(t, h.config())
}

func (h *auditHarness) saveConfigFile(t *testing.T, cfg config.File) {
	t.Helper()
	if err := config.Save(h.configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
}

type credentialSeed struct {
	ref    string
	key    string
	secret string
}

type noLeakRuntimeProvider struct {
	read  gitprovider.GitProvider
	write gitprovider.GitProvider
}

func (p noLeakRuntimeProvider) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	return p.write.WhoAmI(ctx, creds)
}

func (p noLeakRuntimeProvider) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	return p.write.ReviewAuthority(ctx, ref, identity)
}

func (p noLeakRuntimeProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	return p.read.GetPR(ctx, ref)
}

func (p noLeakRuntimeProvider) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return p.read.GetDiff(ctx, ref)
}

func (p noLeakRuntimeProvider) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	return p.read.GetFileAtRef(ctx, ref, gitRef, path)
}

func (p noLeakRuntimeProvider) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error) {
	return p.read.ListTreeAtRef(ctx, ref, gitRef, path)
}

func (p noLeakRuntimeProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return p.read.ListInlineThreads(ctx, ref)
}

func (p noLeakRuntimeProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	return p.read.ListReviews(ctx, ref)
}

func (p noLeakRuntimeProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return p.read.ListIssueComments(ctx, ref)
}

func (p noLeakRuntimeProvider) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, c gitprovider.InlineComment) (gitprovider.CommentID, error) {
	return p.write.PostInlineComment(ctx, ref, c)
}

func (p noLeakRuntimeProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	return p.write.ReplyToThread(ctx, ref, threadID, body)
}

func (p noLeakRuntimeProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	return p.write.ResolveThread(ctx, ref, threadID)
}

func (p noLeakRuntimeProvider) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	return p.write.PostIssueComment(ctx, ref, body)
}

func (p noLeakRuntimeProvider) SubmitReview(ctx context.Context, ref gitprovider.PRRef, r gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	return p.write.SubmitReview(ctx, ref, r)
}

func (p noLeakRuntimeProvider) Capabilities() gitprovider.ProviderCaps {
	readCaps := p.read.Capabilities()
	writeCaps := p.write.Capabilities()
	return gitprovider.ProviderCaps{
		NativeFileLevelComments: readCaps.NativeFileLevelComments || writeCaps.NativeFileLevelComments,
		ThreadResolution:        readCaps.ThreadResolution || writeCaps.ThreadResolution,
		BundleInlineOnSubmit:    readCaps.BundleInlineOnSubmit || writeCaps.BundleInlineOnSubmit,
	}
}

func (h *auditHarness) seedCredentials(t *testing.T) {
	t.Helper()
	cfg := h.config()
	h.seedCredentialWrites(t, cfg, []credentialSeed{
		{ref: "codereview/default", key: credentials.GitTokenKey, secret: h.gitSecret},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	})
}

func (h *auditHarness) seedGitHubAppConfigShowCredentials(t *testing.T) {
	t.Helper()
	cfg := h.githubAppGitConfig()
	h.seedCredentialWrites(t, cfg, []credentialSeed{
		{ref: "codereview/default-app", key: credentials.GitHubAppPrivateKeyKey, secret: h.githubAppPrivateKey},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	})
}

func (h *auditHarness) seedGitHubAppGitCredentials(t *testing.T) {
	t.Helper()
	cfg := h.githubAppGitConfig()
	h.seedCredentialWrites(t, cfg, []credentialSeed{
		{ref: "codereview/default-app", key: credentials.GitHubAppPrivateKeyKey, secret: h.githubAppPrivateKey},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	})
}

func (h *auditHarness) seedGitHubAppGitLookupCredentials(t *testing.T) {
	t.Helper()
	cfg := h.githubAppGitConfig()
	h.seedCredentialWrites(t, cfg, []credentialSeed{
		{ref: "codereview/default-app", key: credentials.GitHubAppPrivateKeyKey, secret: h.githubAppPrivateKey},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	})
}

func (h *auditHarness) seedGitHubAppReviewerCredentials(t *testing.T) {
	t.Helper()
	cfg := h.githubAppGitConfig()
	h.seedCredentialWrites(t, cfg, []credentialSeed{
		{ref: "codereview/default-app", key: credentials.GitHubAppPrivateKeyKey, secret: h.githubAppPrivateKey},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	})
}

func (h *auditHarness) seedCredentialWrites(t *testing.T, cfg config.File, writes []credentialSeed) {
	t.Helper()
	h.saveConfigFile(t, cfg)
	store, err := h.openCredentialStore()
	if err != nil {
		t.Fatalf("open credential store: %v", err)
	}
	defer store.Close()
	for _, write := range writes {
		ref, err := credentials.ParseRef(write.ref)
		if err != nil {
			t.Fatalf("ParseRef(%s): %v", write.ref, err)
		}
		if err := store.Set(ref.Profile, write.key, write.secret, credstore.WithOverwrite()); err != nil {
			t.Fatalf("Set(%s,%s): %v", write.ref, write.key, err)
		}
	}
}

func (h *auditHarness) identityFactory(_ *cobra.Command, _ *root.Options, _ config.File) (identity.Resolver, func(), error) {
	store, err := h.openCredentialStore()
	if err != nil {
		return nil, nil, err
	}
	return realIdentityResolver{h: h, store: store}, func() { _ = store.Close() }, nil
}

func (h *auditHarness) providerFactory(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
	store, err := h.openCredentialStore()
	if err != nil {
		return nil, nil, err
	}
	provider, _, err := h.newGitHubProvider(profile.Git, store, nil)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return provider, func() { _ = store.Close() }, nil
}

func (h *auditHarness) reviewRuntimeFactory(ctx context.Context, runtimeOpts app.OpenRequest) (app.Runtime, error) {
	profile := runtimeOpts.Profile
	store, err := h.openCredentialStore()
	if err != nil {
		return app.Runtime{}, err
	}
	cleanup := func() { _ = store.Close() }
	readProvider, _, err := h.newGitHubProvider(profile.Git, store, installationLookup(runtimeOpts.PRRef))
	if err != nil {
		cleanup()
		return app.Runtime{}, err
	}
	postingGit := gitConfigForReviewerAuth(profile)
	postingProvider, credential, err := h.newGitHubProvider(postingGit, store, installationLookup(runtimeOpts.PRRef))
	if err != nil {
		cleanup()
		return app.Runtime{}, err
	}
	postingIdentity, err := postingProvider.WhoAmI(ctx, credential)
	if err != nil {
		cleanup()
		return app.Runtime{}, err
	}
	adapter, err := llm.NewAPIAdapterFromConfig(profile.LLM, store, llm.APIOptions{BaseURL: h.llmURL})
	if err != nil {
		cleanup()
		return app.Runtime{}, err
	}
	ledgerStore, err := ledger.Open(ctx, h.layout.LedgerDB())
	if err != nil {
		cleanup()
		return app.Runtime{}, err
	}
	cleanup = func() {
		_ = ledgerStore.Close()
		_ = store.Close()
	}
	pipelineOpts := pipeline.Options{
		Provider:            readProvider,
		Adapter:             adapter,
		Store:               ledgerStore,
		NamedSessions:       ledgerStore,
		Layout:              h.layout,
		Warnings:            runtimeOpts.Warnings,
		Now:                 func() time.Time { return h.now },
		Retention:           runtimeOpts.Retention,
		RetentionManualOnly: runtimeOpts.RetentionManualOnly,
		MaxAgents:           runtimeOpts.MaxAgents,
		MaxConcurrency:      runtimeOpts.MaxConcurrency,
		GitCommand:          noLeakGitCommand(h.prRef),
		ResolveRepoRoot:     func(context.Context) (string, error) { return h.workbenchRepoDir, nil },
	}
	liveProvider := noLeakRuntimeProvider{read: readProvider, write: postingProvider}
	runner := realReviewRunner{
		pipeline: pipelineOpts,
		live: reviewrun.Options{
			Store:                   ledgerStore,
			Provider:                liveProvider,
			Planner:                 livePlanner{opts: pipelineOpts},
			Limiter:                 noLeakLimiter{},
			Layout:                  h.layout,
			Now:                     func() time.Time { return h.now },
			StaleHeartbeatThreshold: 10 * time.Minute,
			Warnings:                runtimeOpts.Warnings,
			Retention:               runtimeOpts.Retention,
			RetentionManualOnly:     runtimeOpts.RetentionManualOnly,
		},
	}
	return app.Runtime{
		Runner:          runner,
		PostingIdentity: postingIdentity,
		Cleanup:         cleanup,
	}, nil
}

func (h *auditHarness) openCredentialStore() (*credstore.Store, error) {
	return credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
}

func (h *auditHarness) newGitHubProvider(git config.GitConfig, store credentials.Reader, lookup *githubprovider.InstallationLookup) (*githubprovider.Client, gitprovider.Credential, error) {
	opts := githubprovider.Options{
		BaseURL:            h.githubURL,
		GraphQLURL:         h.graphQLURL,
		InstallationLookup: lookup,
	}
	if lookup == nil {
		opts.InstallationID = h.githubAppInstallationIDSecret
	}
	return githubprovider.NewFromGitConfig(git, store, opts)
}

func gitConfigForReviewerAuth(profile config.Profile) config.GitConfig {
	if profile.ReviewerCredentials == nil {
		return profile.Git
	}
	return config.GitConfig{
		Host:          profile.Git.Host,
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		GitHubApp:     profile.ReviewerCredentials.GitHubApp,
		Credential:    profile.ReviewerCredentials.Credential,
		CredentialRef: profile.ReviewerCredentials.CredentialRef,
		IdentityCache: profile.ReviewerCredentials.IdentityCache,
	}
}

func installationLookup(ref gitprovider.PRRef) *githubprovider.InstallationLookup {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Repo) == "" {
		return nil
	}
	return &githubprovider.InstallationLookup{Owner: ref.Owner, Repo: ref.Repo}
}

func (h *auditHarness) seedNamedSession(t *testing.T) {
	t.Helper()
	store := h.openLedger(t)
	defer store.Close()
	err := store.UpsertNamedSession(context.Background(), ledger.NamedSession{
		Name:              "daily",
		Profile:           "default",
		Provider:          string(config.LLMProviderAnthropic),
		Adapter:           string(config.LLMAdapterAnthropicAPI),
		Model:             "claude-sonnet-4-6",
		Host:              "github.com",
		ProviderSessionID: "provider-session-safe-001",
		CreatedAt:         h.now.Add(-time.Hour),
		LastUsedAt:        h.now,
	})
	if err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
}

func (h *auditHarness) seedRun(t *testing.T) {
	t.Helper()
	store := h.openLedger(t)
	defer store.Close()
	artifactPath := filepath.Join(h.layout.DataRoot, "runs", h.prKey, h.headSHA, h.baseSHA, "default__reviewer-user", "seed-run")
	if err := os.MkdirAll(artifactPath, 0o700); err != nil {
		t.Fatalf("MkdirAll artifact path: %v", err)
	}
	writeFile(t, filepath.Join(artifactPath, "rollup.md"), "Seed run artifact.\n")
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           h.prKey,
		PRURL:           h.prURL,
		RunID:           "seed-run",
		SHA:             h.headSHA,
		BaseSHA:         h.baseSHA,
		Profile:         "default",
		PostingIdentity: "reviewer-user",
		PostMode:        ledger.PostModeDryRun,
		StartedAt:       h.now.Add(-2 * time.Hour),
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	if err := store.CompleteRun(context.Background(), run.RunID, ledger.OutcomeDryRun, h.now.Add(-time.Hour)); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
}

func (h *auditHarness) openLedger(t *testing.T) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), h.layout.LedgerDB())
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return store
}

func (h *auditHarness) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if r.URL.EscapedPath() == "/graphql" {
		h.handleGitHubGraphQL(w, r)
		return
	}
	pullPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", h.prRef.Owner, h.prRef.Repo, h.prRef.Number)
	appInstallationPath := "/app/installations/" + h.githubAppInstallationIDSecret
	repoInstallationPath := fmt.Sprintf("/repos/%s/%s/installation", h.prRef.Owner, h.prRef.Repo)
	switch {
	case r.Method == http.MethodGet && r.URL.EscapedPath() == appInstallationPath:
		if !h.requireGitHubAppJWT(w, r) {
			return
		}
		h.writeGitHubAppInstallation(w)
	case r.Method == http.MethodGet && r.URL.EscapedPath() == repoInstallationPath:
		if !h.requireGitHubAppJWT(w, r) {
			return
		}
		h.writeGitHubAppInstallation(w)
	case r.Method == http.MethodPost && r.URL.EscapedPath() == appInstallationPath+"/access_tokens":
		if !h.requireGitHubAppJWT(w, r) {
			return
		}
		writeHTTPJSON(h.t, w, map[string]any{
			"token":      h.githubAppInstallationToken,
			"expires_at": h.now.Add(time.Hour).Format(time.RFC3339),
		})
	case r.Method == http.MethodGet && r.URL.EscapedPath() == "/user":
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		login := "git-user"
		id := 1001
		if r.Header.Get("Authorization") == "Bearer "+h.reviewerSecret {
			login = "reviewer-user"
			id = 1002
		} else if r.Header.Get("Authorization") == "Bearer "+h.githubAppInstallationToken {
			login = h.githubAppSlug + "[bot]"
			id = 1005
		}
		writeHTTPJSON(h.t, w, map[string]any{"login": login, "id": id, "name": login})
	case r.Method == http.MethodGet && r.URL.EscapedPath() == pullPath && r.Header.Get("Accept") == "application/vnd.github.v3.diff":
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		_, _ = w.Write([]byte(h.diff()))
	case r.Method == http.MethodGet && r.URL.EscapedPath() == pullPath:
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		writeHTTPJSON(h.t, w, map[string]any{
			"title":    "Add no-leak harness",
			"html_url": h.prURL,
			"state":    "open",
			"merged":   false,
			"user":     map[string]any{"login": "author-user", "id": 1003, "name": "Author User"},
			"head": map[string]any{
				"ref": "secret-audit",
				"sha": h.headSHA,
				"repo": map[string]any{
					"name":  h.prRef.Repo,
					"owner": map[string]any{"login": h.prRef.Owner, "id": 1004, "name": h.prRef.Owner},
				},
			},
			"base": map[string]any{
				"ref": "main",
				"sha": h.baseSHA,
				"repo": map[string]any{
					"name":  h.prRef.Repo,
					"owner": map[string]any{"login": h.prRef.Owner, "id": 1004, "name": h.prRef.Owner},
				},
			},
		})
	case r.Method == http.MethodGet && r.URL.EscapedPath() == pullPath+"/reviews":
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		writeHTTPJSON(h.t, w, []map[string]any{})
	case r.Method == http.MethodGet && r.URL.EscapedPath() == fmt.Sprintf("/repos/%s/%s/issues/%d/comments", h.prRef.Owner, h.prRef.Repo, h.prRef.Number):
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		writeHTTPJSON(h.t, w, []map[string]any{})
	case r.Method == http.MethodPost && r.URL.EscapedPath() == fmt.Sprintf("/repos/%s/%s/issues/%d/comments", h.prRef.Owner, h.prRef.Repo, h.prRef.Number):
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		writeHTTPJSON(h.t, w, map[string]any{"id": 201})
	case r.Method == http.MethodPost && r.URL.EscapedPath() == pullPath+"/reviews":
		if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
			return
		}
		writeHTTPJSON(h.t, w, map[string]any{"id": 301})
	default:
		h.t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected GitHub request", http.StatusNotFound)
	}
}

func (h *auditHarness) handleGitHubGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireBearer(w, r, h.gitSecret, h.reviewerSecret, h.githubAppInstallationToken) {
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.t.Errorf("decode GraphQL request: %v", err)
		http.Error(w, "bad GraphQL request", http.StatusBadRequest)
		return
	}
	switch {
	case strings.Contains(req.Query, "reviewThreads"):
		writeHTTPJSON(h.t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    []map[string]any{},
		}}}}})
	case strings.Contains(req.Query, "object(expression"):
		writeHTTPJSON(h.t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"object": nil}}})
	default:
		h.t.Errorf("unexpected GraphQL query: %s", req.Query)
		http.Error(w, "unexpected GraphQL query", http.StatusNotFound)
	}
}

func (h *auditHarness) handleLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/messages" {
		h.t.Errorf("unexpected LLM request: %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected LLM request", http.StatusNotFound)
		return
	}
	if got := r.Header.Get("x-api-key"); got != h.llmSecret {
		h.t.Errorf("LLM x-api-key header did not use seeded credential")
		http.Error(w, "bad LLM credential", http.StatusUnauthorized)
		return
	}
	call := h.llmCalls.Add(1)
	var structured string
	switch call {
	case 1:
		structured = `{"schema_version":1,"selected_agents":[{"agent_id":"harness:reviewer","rationale":"review changed Go file","files":["main.go"]}],"thread_actions":[],"reasoning":"exercise real LLM selection"}`
	case 2:
		structured = `{"schema_version":1,"agent_id":"harness:reviewer","findings":[]}`
	default:
		structured = `{"schema_version":1,"review_event":"comment","review_event_rationale":"no findings","dedupe_log":[],"ordered_findings":[]}`
	}
	writeHTTPJSON(h.t, w, map[string]any{
		"id":      fmt.Sprintf("msg_%d", call),
		"content": []map[string]any{{"type": "text", "text": structured}},
		"usage":   map[string]any{"input_tokens": 7, "output_tokens": 11},
	})
}

func (h *auditHarness) writeGitHubAppInstallation(w http.ResponseWriter) {
	writeHTTPJSON(h.t, w, map[string]any{
		"id":       42424242,
		"app_id":   1004005006,
		"app_slug": h.githubAppSlug,
	})
}

func (h *auditHarness) requireGitHubAppJWT(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") {
		h.t.Errorf("request %s %s did not use GitHub App JWT authorization", r.Method, r.URL.String())
		http.Error(w, "bad github app jwt", http.StatusUnauthorized)
		return false
	}
	token := strings.TrimPrefix(got, "Bearer ")
	if len(strings.Split(token, ".")) != 3 {
		h.t.Errorf("request %s %s did not use a JWT-shaped GitHub App token", r.Method, r.URL.String())
		http.Error(w, "bad github app jwt", http.StatusUnauthorized)
		return false
	}
	h.noteSecret(token)
	return true
}

func (h *auditHarness) requireBearer(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	got := r.Header.Get("Authorization")
	for _, secret := range allowed {
		if got == "Bearer "+secret {
			return true
		}
	}
	h.t.Errorf("request %s %s did not use an expected bearer credential", r.Method, r.URL.String())
	http.Error(w, "bad bearer credential", http.StatusUnauthorized)
	return false
}

func (h *auditHarness) noteSecret(secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	h.secretMu.Lock()
	defer h.secretMu.Unlock()
	for _, existing := range h.secrets {
		if existing == secret {
			return
		}
	}
	h.secrets = append(h.secrets, secret)
}

func (h *auditHarness) diff() string {
	return strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 1111111..2222222 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,2 +1,2 @@",
		" package main",
		"-var changed = false",
		"+var changed = true",
		"",
	}, "\n")
}

func writeHTTPJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode HTTP JSON: %v", err)
	}
}

func (h *auditHarness) assertNoLeaks(t *testing.T, label string, data []byte) {
	t.Helper()
	h.secretMu.Lock()
	secrets := append([]string(nil), h.secrets...)
	h.secretMu.Unlock()
	if err := credstore.NoLeakAssertion(data, secrets...); err != nil {
		t.Fatalf("%s leaked a seeded secret: %v", label, err)
	}
}

func (h *auditHarness) assertOwnedFilesDoNotLeak(t *testing.T) {
	t.Helper()
	for _, rootDir := range []string{h.configRoot, h.layout.DataRoot, h.layout.CacheRoot} {
		if _, err := os.Stat(rootDir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatalf("Stat(%s): %v", rootDir, err)
		}
		err := filepath.WalkDir(rootDir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if shouldSkipOwnedPath(path, entry) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path) // #nosec G304,G122 -- paths are under hermetic test-owned roots and symlink entries are skipped.
			if err != nil {
				return err
			}
			h.assertNoLeaks(t, path, data)
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir(%s): %v", rootDir, err)
		}
	}
}

func shouldSkipOwnedPath(path string, entry os.DirEntry) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(path)
	if base == "keyring" {
		return true
	}
	if strings.Contains(clean, string(filepath.Separator)+"keyring"+string(filepath.Separator)) {
		return true
	}
	if strings.Contains(clean, string(filepath.Separator)+"workbench"+string(filepath.Separator)+"repo") {
		return true
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return true
	}
	if strings.HasSuffix(base, ".lock") {
		return true
	}
	return false
}

type realIdentityResolver struct {
	h     *auditHarness
	store *credstore.Store
}

func (r realIdentityResolver) ResolveIdentity(ctx context.Context, _ string, git config.GitConfig) (gitprovider.Identity, error) {
	provider, credential, err := r.h.newGitHubProvider(git, r.store, nil)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	return provider.WhoAmI(ctx, credential)
}

var _ identity.Resolver = realIdentityResolver{}

type realReviewRunner struct {
	pipeline pipeline.Options
	live     reviewrun.Options
}

func (r realReviewRunner) DryRun(ctx context.Context, req pipeline.Request) (pipeline.Result, error) {
	return pipeline.DryRun(ctx, r.pipeline, req)
}

func (r realReviewRunner) Live(ctx context.Context, req pipeline.Request, flags reviewrun.Flags) (reviewrun.Result, error) {
	return reviewrun.Run(ctx, r.live, reviewrun.Request{Pipeline: req, Flags: flags})
}

type livePlanner struct {
	opts pipeline.Options
}

func (p livePlanner) Live(ctx context.Context, req pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	return pipeline.Live(ctx, p.opts, req, run)
}

type noLeakLimiter struct{}

func (noLeakLimiter) Wait(context.Context, string) error { return nil }

var _ app.Runner = realReviewRunner{}

func saveConfigOnly(t *testing.T, h *auditHarness) {
	t.Helper()
	h.saveConfig(t)
}

func saveGitHubAppReviewerConfigOnly(t *testing.T, h *auditHarness) {
	t.Helper()
	h.saveConfigFile(t, h.githubAppGitConfig())
}

func seedConfiguredCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.saveConfig(t)
	h.seedCredentials(t)
}

func seedGitHubAppConfigShowCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.seedGitHubAppConfigShowCredentials(t)
}

func seedGitHubAppGitCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.seedGitHubAppGitCredentials(t)
}

func seedGitHubAppGitLookupCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.seedGitHubAppGitLookupCredentials(t)
}

func seedGitHubAppReviewerCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.seedGitHubAppReviewerCredentials(t)
}

func prepareNamedSession(t *testing.T, h *auditHarness) {
	t.Helper()
	seedConfiguredCredentials(t, h)
	h.seedNamedSession(t)
}

func prepareRunData(t *testing.T, h *auditHarness) {
	t.Helper()
	seedConfiguredCredentials(t, h)
	h.seedRun(t)
}

func staticArgs(args ...string) func(*auditHarness) []string {
	return func(*auditHarness) []string {
		return append([]string(nil), args...)
	}
}

func secretEnv(h *auditHarness) map[string]string {
	return map[string]string{
		"CR_NOLEAK_GIT":                 h.gitSecret,
		"CR_NOLEAK_REVIEWER":            h.reviewerSecret,
		"CR_NOLEAK_LLM":                 h.llmSecret,
		"CR_NOLEAK_APP_ID":              h.githubAppIDSecret,
		"CR_NOLEAK_APP_PRIVATE_KEY":     h.githubAppPrivateKey,
		"CR_NOLEAK_APP_INSTALLATION_ID": h.githubAppInstallationIDSecret,
	}
}

func writeAgent(t *testing.T, rootDir, category, agent, description, prompt string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), "name: "+agent+"\ndescription: "+description+"\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), prompt)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func noLeakPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}
