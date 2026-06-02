package noleak

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/agentscmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/datacmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/mecmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/sessionscmd"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/identity"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
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
			name: "init with env credential ingress",
			args: func(h *auditHarness) []string {
				return []string{
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
			env: secretEnv,
		},
		{
			name:    "set credential text",
			prepare: saveConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--ref", "codereview/default", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_GIT", "--overwrite"}
			},
			env: secretEnv,
		},
		{
			name:    "set credential json",
			prepare: saveConfigOnly,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--ref", "codereview/default-reviewer", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_REVIEWER", "--overwrite", "--json"}
			},
			env: secretEnv,
		},
		{
			name:    "set credential duplicate failure",
			prepare: seedConfiguredCredentials,
			args: func(*auditHarness) []string {
				return []string{"set-credential", "--ref", "codereview/default", "--key", credentials.GitTokenKey, "--from-env", "CR_NOLEAK_GIT"}
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
			name:    "agents list json",
			prepare: seedConfiguredCredentials,
			args:    staticArgs("agents", "list", "--json"),
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
			name:    "review dry-run json verbose",
			prepare: seedConfiguredCredentials,
			args: func(h *auditHarness) []string {
				return []string{"review", "--dry-run", "--json", "--verbose", h.prURL}
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
	t          *testing.T
	configPath string
	configRoot string
	layout     statepaths.Layout
	agentDir   string
	prRef      gitprovider.PRRef
	prURL      string
	prKey      string
	headSHA    string
	baseSHA    string
	now        time.Time

	gitSecret      string
	reviewerSecret string
	llmSecret      string
	secrets        []string
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	rootDir := statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "noleak-file-keyring-passphrase")

	configPath, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	prRef := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 68}
	prKey, err := statepaths.PRKey(prRef.Host, prRef.Owner, prRef.Repo, prRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	h := &auditHarness{ // #nosec G101 -- these are distinctive test canaries, not real credentials.
		t:              t,
		configPath:     configPath,
		configRoot:     filepath.Dir(configPath),
		layout:         layout,
		agentDir:       filepath.Join(rootDir, "agents"),
		prRef:          prRef,
		prURL:          fmt.Sprintf("https://%s/%s/%s/pull/%d", prRef.Host, prRef.Owner, prRef.Repo, prRef.Number),
		prKey:          prKey,
		headSHA:        strings.Repeat("a", 40),
		baseSHA:        strings.Repeat("b", 40),
		now:            time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
		gitSecret:      "cr-noleak-git-token-0001",
		reviewerSecret: "cr-noleak-reviewer-token-0002",
		llmSecret:      "cr-noleak-llm-key-0003",
	}
	h.secrets = []string{h.gitSecret, h.reviewerSecret, h.llmSecret}
	writeAgent(t, h.agentDir, "harness", "reviewer", "No-leak harness reviewer.", "Review changed Go files without mentioning credentials.\n")
	return h
}

func (h *auditHarness) run(args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: h.configPath,
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	configcmd.Register(cmd, opts)
	credentialcmd.Register(cmd, opts)
	datacmd.Register(cmd, opts)
	sessionscmd.Register(cmd, opts)
	mecmd.RegisterWithFactory(cmd, opts, h.identityFactory)
	agentscmd.RegisterWithFactory(cmd, opts, h.providerFactory)
	reviewcmd.RegisterWithFactory(cmd, opts, h.reviewRuntimeFactory)

	args = append([]string{"--backend", string(credstore.BackendFile)}, args...)
	err := root.Execute(cmd, args)
	return stdout.String(), stderr.String(), err
}

func (h *auditHarness) config() config.File {
	maxAgeDays := 30
	return config.File{
		DefaultProfile: "default",
		Keyring:        config.KeyringConfig{Backend: string(credstore.BackendFile)},
		Profiles: map[string]config.Profile{
			"default": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/default",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/default-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					CredentialRef: "codereview/default-llm",
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

func (h *auditHarness) saveConfig(t *testing.T) {
	t.Helper()
	if err := config.Save(h.configPath, h.config()); err != nil {
		t.Fatalf("Save config: %v", err)
	}
}

func (h *auditHarness) seedCredentials(t *testing.T) {
	t.Helper()
	cfg := h.config()
	store, err := credentials.OpenStore(string(credstore.BackendFile), true, cfg)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	writes := []struct {
		ref    string
		key    string
		secret string
	}{
		{ref: "codereview/default", key: credentials.GitTokenKey, secret: h.gitSecret},
		{ref: "codereview/default-reviewer", key: credentials.GitTokenKey, secret: h.reviewerSecret},
		{ref: "codereview/default-llm", key: credentials.AnthropicAPIKeyKey, secret: h.llmSecret},
	}
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

func (h *auditHarness) identityFactory(*cobra.Command, *root.Options, config.File) (identity.Resolver, func(), error) {
	return fixedIdentityResolver{identity: gitprovider.Identity{Login: "reviewer-user", ID: "1002", DisplayName: "Reviewer User"}}, nil, nil
}

func (h *auditHarness) providerFactory(*cobra.Command, *root.Options, config.File, config.Profile) (gitprovider.GitProvider, func(), error) {
	fake := &gitprovider.Fake{}
	_ = fake.SetPR(h.prRef, h.pr())
	return fake, nil, nil
}

func (h *auditHarness) reviewRuntimeFactory(*cobra.Command, *root.Options, config.File, config.Profile, reviewcmd.RuntimeOptions) (reviewcmd.Runtime, error) {
	return reviewcmd.Runtime{
		Runner:          fakeReviewRunner{h: h},
		PostingIdentity: gitprovider.Identity{Login: "reviewer-user", ID: "1002", DisplayName: "Reviewer User"},
	}, nil
}

func (h *auditHarness) pr() gitprovider.PR {
	return gitprovider.PR{
		Ref:   h.prRef,
		Title: "Add no-leak harness",
		URL:   h.prURL,
		State: gitprovider.PRStateOpen,
		Author: gitprovider.Identity{
			Login:       "author-user",
			ID:          "1001",
			DisplayName: "Author User",
		},
		Head: gitprovider.PRBranchRef{
			Host:  h.prRef.Host,
			Owner: h.prRef.Owner,
			Repo:  h.prRef.Repo,
			Name:  "secret-audit",
			Ref:   "refs/heads/secret-audit",
			SHA:   h.headSHA,
		},
		Base: gitprovider.PRBranchRef{
			Host:  h.prRef.Host,
			Owner: h.prRef.Owner,
			Repo:  h.prRef.Repo,
			Name:  "main",
			Ref:   "refs/heads/main",
			SHA:   h.baseSHA,
		},
	}
}

func (h *auditHarness) reviewResult(t *testing.T, runID string, mode ledger.PostMode, outcome ledger.Outcome) (ledger.Run, pipeline.ArtifactPaths) {
	t.Helper()
	artifacts := h.reviewArtifacts(t, runID)
	return ledger.Run{
		RunID:           runID,
		PRKey:           h.prKey,
		SHA:             h.headSHA,
		BaseSHA:         h.baseSHA,
		Attempt:         1,
		Profile:         "default",
		PostingIdentity: "reviewer-user",
		PostMode:        mode,
		StartedAt:       h.now,
		Outcome:         &outcome,
		ArtifactPath:    artifacts.Dir,
	}, artifacts
}

func (h *auditHarness) reviewArtifacts(t *testing.T, runID string) pipeline.ArtifactPaths {
	t.Helper()
	dir := filepath.Join(h.layout.DataRoot, "runs", h.prKey, h.headSHA, h.baseSHA, "default__reviewer-user", runID)
	paths := pipeline.ArtifactPaths{
		Dir:            dir,
		DiffPatch:      filepath.Join(dir, "diff.patch"),
		SlicesDir:      filepath.Join(dir, "slices"),
		FindingsJSON:   filepath.Join(dir, "findings.json"),
		RollupMarkdown: filepath.Join(dir, "rollup.md"),
		AgentLogsDir:   filepath.Join(dir, "agent-logs"),
	}
	writeFile(t, paths.DiffPatch, "diff --git a/main.go b/main.go\n")
	writeFile(t, paths.FindingsJSON, "[]\n")
	writeFile(t, paths.RollupMarkdown, "No findings from no-leak harness.\n")
	writeFile(t, filepath.Join(paths.AgentLogsDir, "harness-reviewer.jsonl"), `{"event":"completed","agent":"harness:reviewer"}`+"\n")
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		t.Fatalf("MkdirAll slices: %v", err)
	}
	return paths
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
		Model:             "claude-3-5-sonnet",
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

func (h *auditHarness) assertNoLeaks(t *testing.T, label string, data []byte) {
	t.Helper()
	if err := credstore.NoLeakAssertion(data, h.secrets...); err != nil {
		t.Fatalf("%s leaked a seeded secret: %v", label, err)
	}
}

func (h *auditHarness) assertOwnedFilesDoNotLeak(t *testing.T) {
	t.Helper()
	for _, rootDir := range []string{h.configRoot, h.layout.DataRoot} {
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
			if entry.IsDir() || !isScannedTextArtifact(path) {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 1<<20 {
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
	base := filepath.Base(path)
	if base == "keyring" {
		return true
	}
	if strings.Contains(filepath.Clean(path), string(filepath.Separator)+"keyring"+string(filepath.Separator)) {
		return true
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return true
	}
	switch {
	case base == "ledger.db",
		strings.HasPrefix(base, "ledger.db-"),
		strings.HasSuffix(base, ".db"),
		strings.Contains(base, ".db-"),
		strings.HasSuffix(base, ".lock"):
		return true
	default:
		return false
	}
}

func isScannedTextArtifact(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonl", ".md", ".patch", ".txt", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

type fixedIdentityResolver struct {
	identity gitprovider.Identity
}

func (r fixedIdentityResolver) ResolveIdentity(context.Context, config.GitConfig) (gitprovider.Identity, error) {
	return r.identity, nil
}

var _ identity.Resolver = fixedIdentityResolver{}

type fakeReviewRunner struct {
	h *auditHarness
}

func (r fakeReviewRunner) DryRun(_ context.Context, _ pipeline.Request) (pipeline.Result, error) {
	outcome := ledger.OutcomeDryRun
	run, artifacts := r.h.reviewResult(r.h.t, "dry-run-001", ledger.PostModeDryRun, outcome)
	return pipeline.Result{
		Run:       run,
		PR:        r.h.pr(),
		PRKey:     r.h.prKey,
		Artifacts: artifacts,
		Rollup: review.Rollup{
			ReviewEvent:          review.ReviewEventComment,
			ReviewEventRationale: "No issues found.",
		},
		Plan: reviewplan.Plan{
			Outcome:        reviewplan.OutcomeComment,
			RollupMarkdown: "No findings from no-leak harness.",
		},
	}, nil
}

func (r fakeReviewRunner) Live(_ context.Context, _ pipeline.Request, _ reviewrun.Flags) (reviewrun.Result, error) {
	outcome := ledger.OutcomeComment
	run, artifacts := r.h.reviewResult(r.h.t, "live-run-001", ledger.PostModeLive, outcome)
	pipelineResult := pipeline.Result{
		Run:       run,
		PR:        r.h.pr(),
		PRKey:     r.h.prKey,
		Artifacts: artifacts,
		Plan: reviewplan.Plan{
			Outcome:        reviewplan.OutcomeComment,
			RollupMarkdown: "Posted no-leak harness review.",
		},
	}
	return reviewrun.Result{
		Status:   gateio.StatusContinue,
		Decision: gate.Decision{Kind: gate.DecisionFresh, Message: "fresh review"},
		Run:      run,
		PR:       r.h.pr(),
		PRKey:    r.h.prKey,
		Pipeline: &pipelineResult,
		Outbox: outbox.Result{
			Outcome:  ledger.OutcomeComment,
			ExitCode: 0,
			Posted:   1,
		},
		ExitCode: 0,
		Message:  "review posted",
	}, nil
}

var _ reviewcmd.Runner = fakeReviewRunner{}

func saveConfigOnly(t *testing.T, h *auditHarness) {
	t.Helper()
	h.saveConfig(t)
}

func seedConfiguredCredentials(t *testing.T, h *auditHarness) {
	t.Helper()
	h.saveConfig(t)
	h.seedCredentials(t)
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
		"CR_NOLEAK_GIT":      h.gitSecret,
		"CR_NOLEAK_REVIEWER": h.reviewerSecret,
		"CR_NOLEAK_LLM":      h.llmSecret,
	}
}

func writeAgent(t *testing.T, rootDir, category, agent, description, prompt string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), "name: "+agent+"\ndescription: "+description+"\nmodel: sonnet\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n")
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
