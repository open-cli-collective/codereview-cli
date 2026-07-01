package agentscmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestAgentsListWithoutPRLoadsProfileAndFlagSources(t *testing.T) {
	profileDir := t.TempDir()
	flagDir := t.TempDir()
	overrideFlagDir := t.TempDir()
	writeAgent(t, profileDir, "shared", "reviewer", "profile desc", "profile prompt")
	writeAgent(t, flagDir, "shared", "reviewer", "first flag desc", "first flag prompt")
	writeAgent(t, overrideFlagDir, "shared", "reviewer", "second flag desc", "second flag prompt")
	cfg := testConfig(profileDir)
	cmd, out := newTestCommand(t, cfg, func(*cobra.Command, *root.Options, config.File, config.Profile) (gitprovider.GitProvider, func(), error) {
		t.Fatal("provider factory called without PR argument")
		return nil, nil, nil
	})

	if err := root.Execute(cmd, []string{"--profile", "home", "agents", "list", "--agents-dir", flagDir, "--agents-dir", overrideFlagDir}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "shared:reviewer") || !strings.Contains(text, "second flag desc") || !strings.Contains(text, "Provenance: flag:2") || !strings.Contains(text, "Source fingerprint: sha256:") {
		t.Fatalf("stdout = %q, want second repeatable flag override with provenance", text)
	}
	if strings.Contains(text, "first flag desc") {
		t.Fatalf("stdout = %q, want later --agents-dir to override earlier flag source", text)
	}
	if strings.Contains(text, "Note:") {
		t.Fatalf("stdout = %q, want no PR trust note", text)
	}
}

func TestAgentsListFailsFastForUnreadableProfileSource(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "agent-source-file")
	if err := os.WriteFile(notDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile notDir: %v", err)
	}
	cfg := testConfig(notDir)
	cmd, _ := newTestCommand(t, cfg, func(*cobra.Command, *root.Options, config.File, config.Profile) (gitprovider.GitProvider, func(), error) {
		t.Fatal("provider factory called without PR argument")
		return nil, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "agents", "list"})
	if err == nil || !strings.Contains(err.Error(), "agents: read source") {
		t.Fatalf("Execute error = %v, want read source failure", err)
	}
}

func TestAgentsListWithPRLoadsRepoBaseAndTrustNote(t *testing.T) {
	profileDir := t.TempDir()
	writeAgent(t, profileDir, "profile", "only", "profile desc", "profile prompt")
	trustCurrentTempFixtures(t)
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig(profileDir)
	cmd, out := newTestCommand(t, cfg, providerFactory(fake))

	if err := root.Execute(cmd, []string{"agents", "list", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.AgentsList
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Agents) != 2 {
		t.Fatalf("agents len = %d, want profile and repo agents: %#v", len(got.Agents), got.Agents)
	}
	if got.Repo == nil || got.Repo.Provenance != "repo@refs/heads/main:base-sh" {
		t.Fatalf("repo = %#v, want base provenance", got.Repo)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources len = %d, want profile and repo sources: %#v", len(got.Sources), got.Sources)
	}
	if got.Sources[0].Fingerprint == "" || got.Sources[0].Status != "available" {
		t.Fatalf("profile source = %#v, want available fingerprinted source", got.Sources[0])
	}
	if !strings.Contains(got.TrustNote, "PR-head .codereview/agents changes do not affect") {
		t.Fatalf("trust_note = %q, want PR-head note", got.TrustNote)
	}
}

func TestAgentsListWithPRUsesRepositoryProfileRoute(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repos:     []string{ref.Repo},
		},
	}}
	cmd, out := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
		if profile.Git.CredentialRef != "codereview/work" {
			t.Fatalf("provider profile credential ref = %q, want work route", profile.Git.CredentialRef)
		}
		return fake, nil, nil
	})

	if err := root.Execute(cmd, []string{"agents", "list", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.AgentsList
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if got.Repo == nil || got.Repo.Provenance == "" {
		t.Fatalf("repo = %#v, want repository source", got.Repo)
	}
}

func TestAgentsListWithPRRejectsAmbiguousRepositoryProfileRoute(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      ref.Host,
				Namespace: ref.Owner,
				Repos:     []string{ref.Repo},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      ref.Host,
				Namespace: ref.Owner,
				Repos:     []string{ref.Repo},
			},
		},
	}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile) (gitprovider.GitProvider, func(), error) {
		t.Fatal("provider factory should not be called for ambiguous repository routes")
		return fake, nil, nil
	})

	err := root.Execute(cmd, []string{"agents", "list", prURL(ref), "--json"})
	if !errors.Is(err, config.ErrRepositoryProfileAmbiguous) {
		t.Fatalf("Execute error = %v, want ErrRepositoryProfileAmbiguous", err)
	}
	if !strings.Contains(err.Error(), "pass --profile with one of: home, work") {
		t.Fatalf("error = %v, want profile suggestions", err)
	}
}

func TestAgentsListExplicitProfileBypassesRepositoryRoute(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repos:     []string{ref.Repo},
		},
	}}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
		if profile.Git.CredentialRef != "codereview/home" {
			t.Fatalf("provider profile credential ref = %q, want explicit home", profile.Git.CredentialRef)
		}
		return fake, nil, nil
	})

	if err := root.Execute(cmd, []string{"--profile", "home", "agents", "list", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestAgentsShowWithPRUsesRepositoryProfileRoute(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repos:     []string{ref.Repo},
		},
	}}
	cmd, out := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
		if profile.Git.CredentialRef != "codereview/work" {
			t.Fatalf("provider profile credential ref = %q, want work route", profile.Git.CredentialRef)
		}
		return fake, nil, nil
	})

	if err := root.Execute(cmd, []string{"agents", "show", "repo:reviewer", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.AgentsShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if got.Agent.ID != "repo:reviewer" {
		t.Fatalf("agent = %#v, want repo:reviewer", got.Agent)
	}
}

func TestAgentsListExplicitEmptyProfileFailsBeforeRepositoryRoute(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repos:     []string{ref.Repo},
		},
	}}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile) (gitprovider.GitProvider, func(), error) {
		t.Fatal("provider factory should not be called for an empty explicit profile")
		return fake, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "", "agents", "list", prURL(ref), "--json"})
	if err == nil || !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("Execute error = %v, want empty profile failure", err)
	}
}

func TestAgentsListExplicitProfileHostMismatch(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	home := cfg.Profiles["home"]
	home.Git.Host = "gitlab.com"
	cfg.Profiles["home"] = home
	cfg.RepositoryProfiles = nil
	work := home
	work.Git.Host = "github.com"
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
	cmd, _ := newTestCommand(t, cfg, func(*cobra.Command, *root.Options, config.File, config.Profile) (gitprovider.GitProvider, func(), error) {
		t.Fatal("provider factory should not be called when explicit profile host mismatches")
		return fake, nil, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "agents", "list", prURL(ref)})
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

func TestAgentsListWithPRRejectsUnsafeProfileSource(t *testing.T) {
	tests := []struct {
		name       string
		source     func(t *testing.T) string
		wantDetail string
	}{
		{name: "relative", source: relativeAgentSource, wantDetail: "relative"},
		{name: "temp", source: tempAgentSource, wantDetail: "OS temp"},
		{name: "same invocation worktree", source: gitWorktreeAgentSource, wantDetail: "current invocation worktree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
			cfg := testConfig(tt.source(t))
			cmd, _ := newTestCommand(t, cfg, providerFactory(fake))

			err := root.Execute(cmd, []string{"agents", "list", prURL(ref)})
			if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("Execute error = %v, want ErrUnsafeSource with %q", err, tt.wantDetail)
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
		})
	}
}

func TestAgentsListWithPRAllowsSiblingGitCatalog(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig(siblingGitCatalogSource(t))
	cmd, out := newTestCommand(t, cfg, providerFactory(fake))

	if err := root.Execute(cmd, []string{"agents", "list", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.AgentsList
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(got.Sources) == 0 || got.Sources[0].Fingerprint == "" {
		t.Fatalf("sources = %#v, want fingerprinted profile source", got.Sources)
	}
}

func TestAgentsShowWithPRRejectsUnsafeProfileSource(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig(tempAgentSource(t))
	cmd, _ := newTestCommand(t, cfg, providerFactory(fake))

	err := root.Execute(cmd, []string{"agents", "show", "repo:reviewer", prURL(ref)})
	if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), "OS temp") {
		t.Fatalf("Execute error = %v, want ErrUnsafeSource with temp detail", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
}

func TestAgentsShowRendersAgentAndMissingFailure(t *testing.T) {
	profileDir := t.TempDir()
	writeAgent(t, profileDir, "harness", "architecture", "architecture desc", "Read carefully.")
	cfg := testConfig(profileDir)
	cmd, out := newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))

	if err := root.Execute(cmd, []string{"--profile", "home", "agents", "show", "harness:architecture"}); err != nil {
		t.Fatalf("Execute show: %v", err)
	}
	if text := out.String(); !strings.Contains(text, "Agent: harness:architecture") || !strings.Contains(text, "Read carefully.") || !strings.Contains(text, "Source canonical path:") {
		t.Fatalf("stdout = %q, want agent detail and prompt", text)
	}

	cmd, _ = newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))
	err := root.Execute(cmd, []string{"--profile", "home", "agents", "show", "missing:agent"})
	if err == nil {
		t.Fatal("Execute missing error = nil, want failure")
	}
	if exitcode.FromError(err) != exitcode.Failure {
		t.Fatalf("missing exit code = %d, want failure", exitcode.FromError(err))
	}
}

func TestAgentsShowJSONWithPR(t *testing.T) {
	fake, ref := fakeProviderWithRepoAgent(t, "repo", "reviewer", "repo desc")
	cfg := testConfig("")
	cmd, out := newTestCommand(t, cfg, providerFactory(fake))

	if err := root.Execute(cmd, []string{"agents", "show", "repo:reviewer", prURL(ref), "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.AgentsShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if got.Agent.ID != "repo:reviewer" || got.Agent.Provenance != "repo@refs/heads/main:base-sh" {
		t.Fatalf("agent = %#v, want repo agent", got.Agent)
	}
	if got.Agent.Source.Kind != "repo" || got.Agent.Source.SHA == "" {
		t.Fatalf("agent source = %#v, want structured repo source", got.Agent.Source)
	}
	if !strings.Contains(got.TrustNote, "PR-head .codereview/agents changes do not affect") {
		t.Fatalf("trust_note = %q, want PR-head note", got.TrustNote)
	}
}

func TestAgentsListRejectsInvalidPRArg(t *testing.T) {
	cfg := testConfig("")
	cmd, _ := newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))

	err := root.Execute(cmd, []string{"agents", "list", "not-a-url"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}

	cmd, _ = newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))
	err = root.Execute(cmd, []string{"agents", "list", "http://github.com/open-cli-collective/codereview-cli/pull/28"})
	if err == nil {
		t.Fatal("Execute http URL error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("http URL exit code = %d, want usage", got)
	}

	cmd, _ = newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))
	err = root.Execute(cmd, []string{"--profile", "home", "agents", "list", "https://gitlab.com/open-cli-collective/codereview-cli/pull/28"})
	if err == nil {
		t.Fatal("Execute wrong host error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("wrong host exit code = %d, want usage", got)
	}
}

func newTestCommand(t *testing.T, cfg config.File, factory ProviderFactory) (*cobra.Command, *bytes.Buffer) {
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

func providerFactory(provider gitprovider.GitProvider) ProviderFactory {
	return func(*cobra.Command, *root.Options, config.File, config.Profile) (gitprovider.GitProvider, func(), error) {
		return provider, nil, nil
	}
}

func testConfig(agentSource string) config.File {
	profile := config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/home",
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
	}
	if agentSource != "" {
		profile.AgentSources = []string{agentSource}
	}
	return config.File{
		Keyring: config.KeyringConfig{Backend: "memory"},
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		}},
		Profiles: map[string]config.Profile{"home": profile},
	}
}

func fakeProviderWithRepoAgent(t *testing.T, category, agent, description string) (*gitprovider.Fake, gitprovider.PRRef) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 28}
	pr := gitprovider.PR{
		Ref:   ref,
		Title: "CR-19",
		Base:  gitprovider.PRBranchRef{Name: "main", Ref: "refs/heads/main", SHA: "base-sha-123456"},
		Head:  gitprovider.PRBranchRef{Name: "feature", Ref: "refs/heads/feature", SHA: "head-sha-123456"},
	}
	var fake gitprovider.Fake
	if err := fake.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	addRepoAgent(t, &fake, ref, pr.Base.SHA, category, agent, description)
	addRepoAgent(t, &fake, ref, pr.Head.SHA, category, agent, "head desc")
	return &fake, ref
}

func addRepoAgent(t *testing.T, fake *gitprovider.Fake, ref gitprovider.PRRef, gitRef, category, agent, description string) {
	t.Helper()
	rootPath := ".codereview/agents"
	categoryPath := rootPath + "/" + category
	agentPath := categoryPath + "/" + agent
	if err := fake.SetTreeAtRef(ref, gitRef, rootPath, []gitprovider.TreeEntry{{Path: category, Type: "tree"}}); err != nil {
		t.Fatalf("SetTreeAtRef root: %v", err)
	}
	if err := fake.SetFileAtRef(ref, gitRef, categoryPath+"/index.yaml", []byte("name: "+category+"\ndescription: "+category+" category\nowner: owner\n")); err != nil {
		t.Fatalf("SetFileAtRef category: %v", err)
	}
	if err := fake.SetTreeAtRef(ref, gitRef, categoryPath, []gitprovider.TreeEntry{{Path: agentPath, Type: "tree"}}); err != nil {
		t.Fatalf("SetTreeAtRef category: %v", err)
	}
	if err := fake.SetFileAtRef(ref, gitRef, agentPath+"/index.yaml", []byte(agentIndexYAML(agent, description))); err != nil {
		t.Fatalf("SetFileAtRef index: %v", err)
	}
	if err := fake.SetFileAtRef(ref, gitRef, agentPath+"/prompt.md", []byte("Repo prompt\n")); err != nil {
		t.Fatalf("SetFileAtRef prompt: %v", err)
	}
}

func prURL(ref gitprovider.PRRef) string {
	return "https://" + ref.Host + "/" + ref.Owner + "/" + ref.Repo + "/pull/" + strconv.Itoa(ref.Number)
}

func writeAgent(t *testing.T, rootDir, category, agent, description, prompt string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentIndexYAML(agent, description))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), prompt)
}

func relativeAgentSource(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	source := filepath.Join(cwd, "agents")
	writeAgent(t, source, "profile", "reviewer", "profile desc", "profile prompt")
	t.Chdir(cwd)
	return "agents"
}

func tempAgentSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	writeAgent(t, source, "profile", "reviewer", "profile desc", "profile prompt")
	return source
}

func gitWorktreeAgentSource(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	trustCurrentTempFixtures(t)
	repoRoot := filepath.Join(workspace, "review-repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll review repo: %v", err)
	}
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil { // #nosec G204 -- tests invoke git with fixed arguments.
		t.Fatalf("git init review repo: %v\n%s", err, out)
	}
	source := filepath.Join(repoRoot, "nested", "agents")
	writeAgent(t, source, "profile", "reviewer", "profile desc", "profile prompt")
	t.Chdir(repoRoot)
	return source
}

func siblingGitCatalogSource(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	trustCurrentTempFixtures(t)
	reviewRoot := filepath.Join(workspace, "review-repo")
	if err := os.MkdirAll(reviewRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll review repo: %v", err)
	}
	if out, err := exec.Command("git", "init", reviewRoot).CombinedOutput(); err != nil { // #nosec G204 -- tests invoke git with fixed arguments.
		t.Fatalf("git init review repo: %v\n%s", err, out)
	}
	catalogRoot := filepath.Join(workspace, "catalog-repo")
	if err := os.MkdirAll(catalogRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll catalog repo: %v", err)
	}
	if out, err := exec.Command("git", "init", catalogRoot).CombinedOutput(); err != nil { // #nosec G204 -- tests invoke git with fixed arguments.
		t.Fatalf("git init catalog repo: %v\n%s", err, out)
	}
	source := filepath.Join(catalogRoot, "agents")
	writeAgent(t, source, "profile", "reviewer", "profile desc", "profile prompt")
	t.Chdir(reviewRoot)
	return source
}

func trustCurrentTempFixtures(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
}

func agentIndexYAML(name, description string) string {
	return "name: " + name + "\ndescription: " + description + "\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n"
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
