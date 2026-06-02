package agentscmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

	if err := root.Execute(cmd, []string{"agents", "list", "--agents-dir", flagDir, "--agents-dir", overrideFlagDir}); err != nil {
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

func TestAgentsListWithPRLoadsRepoBaseAndTrustNote(t *testing.T) {
	profileDir := t.TempDir()
	writeAgent(t, profileDir, "profile", "only", "profile desc", "profile prompt")
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

func TestAgentsShowRendersAgentAndMissingFailure(t *testing.T) {
	profileDir := t.TempDir()
	writeAgent(t, profileDir, "harness", "architecture", "architecture desc", "Read carefully.")
	cfg := testConfig(profileDir)
	cmd, out := newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))

	if err := root.Execute(cmd, []string{"agents", "show", "harness:architecture"}); err != nil {
		t.Fatalf("Execute show: %v", err)
	}
	if text := out.String(); !strings.Contains(text, "Agent: harness:architecture") || !strings.Contains(text, "Read carefully.") || !strings.Contains(text, "Source canonical path:") {
		t.Fatalf("stdout = %q, want agent detail and prompt", text)
	}

	cmd, _ = newTestCommand(t, cfg, providerFactory(&gitprovider.Fake{}))
	err := root.Execute(cmd, []string{"agents", "show", "missing:agent"})
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
	err = root.Execute(cmd, []string{"agents", "list", "https://gitlab.com/open-cli-collective/codereview-cli/pull/28"})
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
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles:       map[string]config.Profile{"home": profile},
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

func agentIndexYAML(name, description string) string {
	return "name: " + name + "\ndescription: " + description + "\nmodel: sonnet\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n"
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
