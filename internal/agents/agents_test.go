package agents

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestLoadFilesystemSourceParsesAgent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")

	catalog, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(catalog.Agents))
	}
	agent := catalog.Agents[0]
	if agent.ID != "harness:architecture" || agent.Name != "architecture" {
		t.Fatalf("agent identity = (%q,%q), want harness:architecture", agent.ID, agent.Name)
	}
	if agent.Category.Description != "harness category" || agent.Category.Owner != "owner" {
		t.Fatalf("category = %#v, want parsed metadata", agent.Category)
	}
	if agent.Description != "Reviews architecture." || agent.Model != "sonnet" || agent.Effort != "medium" {
		t.Fatalf("agent metadata = %#v, want parsed fields", agent)
	}
	if strings.TrimSpace(agent.Prompt) != "Prompt text." {
		t.Fatalf("prompt = %q, want Prompt text", agent.Prompt)
	}
	if got := agent.Provenance.String(); !strings.HasPrefix(got, "profile:") {
		t.Fatalf("provenance = %q, want profile source", got)
	}
	if len(catalog.Sources) != 1 || catalog.Sources[0].Fingerprint == "" || catalog.Sources[0].CanonicalPath == "" {
		t.Fatalf("sources = %#v, want source provenance with fingerprint and canonical path", catalog.Sources)
	}
}

func TestMissingFilesystemSourceFailsLoadAndInspectsMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-agents")

	_, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{missing}})
	if err == nil || !strings.Contains(err.Error(), "agents: read source") {
		t.Fatalf("Load error = %v, want read source failure", err)
	}

	sources := InspectProfileSources([]string{missing})
	if len(sources) != 1 || sources[0].Status != SourceStatusMissing || sources[0].Present || sources[0].Error == "" {
		t.Fatalf("sources = %#v, want non-fatal missing status", sources)
	}
}

func TestUnreadableFilesystemSourceFailsLoadAndInspectsUnreadable(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "agent-source-file")
	if err := os.WriteFile(notDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile notDir: %v", err)
	}

	_, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{notDir}})
	if err == nil || !strings.Contains(err.Error(), "agents: read source") {
		t.Fatalf("Load error = %v, want read source failure", err)
	}

	sources := InspectProfileSources([]string{notDir})
	if len(sources) != 1 || sources[0].Status != SourceStatusUnreadable || !sources[0].Present || sources[0].Error == "" {
		t.Fatalf("sources = %#v, want non-fatal unreadable status", sources)
	}

	blockedDir := filepath.Join(t.TempDir(), "blocked-agents")
	if err := os.Mkdir(blockedDir, 0o700); err != nil {
		t.Fatalf("Mkdir blockedDir: %v", err)
	}
	if err := os.Chmod(blockedDir, 0); err != nil {
		t.Skipf("Chmod unreadable unsupported: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(blockedDir, 0o700) // #nosec G302 -- test cleanup restores directory access.
	})
	if _, err := os.ReadDir(blockedDir); err == nil {
		t.Skip("directory permissions are not enforced in this environment")
	}
	_, err = Load(context.Background(), LoadOptions{ProfileDirs: []string{blockedDir}})
	if err == nil || !strings.Contains(err.Error(), "agents: read source") {
		t.Fatalf("Load chmod error = %v, want read source failure", err)
	}
	sources = InspectProfileSources([]string{blockedDir})
	if len(sources) != 1 || sources[0].Status != SourceStatusUnreadable || !sources[0].Present || sources[0].Error == "" {
		t.Fatalf("chmod sources = %#v, want non-fatal unreadable status", sources)
	}
}

func TestFilesystemSourceSymlinkRecordsCanonicalPathAndFingerprint(t *testing.T) {
	realRoot := t.TempDir()
	writeAgent(t, realRoot, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	linkRoot := filepath.Join(t.TempDir(), "agents-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("Symlink unsupported: %v", err)
	}
	wantCanonical, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(realRoot): %v", err)
	}
	wantCanonical, err = filepath.Abs(wantCanonical)
	if err != nil {
		t.Fatalf("Abs(realRoot): %v", err)
	}

	catalog, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{linkRoot}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(catalog.Sources))
	}
	source := catalog.Sources[0]
	if source.ConfiguredPath != linkRoot || source.CanonicalPath != wantCanonical || source.Fingerprint == "" {
		t.Fatalf("source = %#v, want configured symlink, canonical real path, fingerprint", source)
	}
	if len(catalog.Agents) != 1 || catalog.Agents[0].Provenance.CanonicalPath != wantCanonical || catalog.Agents[0].Provenance.Fingerprint != source.Fingerprint {
		t.Fatalf("agent provenance = %#v, source %#v; want canonical/fingerprint copied", catalog.Agents, source)
	}
}

func TestFilesystemSourceFingerprintChangesWithPromptContent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")

	first, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
	if err != nil {
		t.Fatalf("Load first: %v", err)
	}
	promptPath := filepath.Join(root, "harness", "architecture", "prompt.md")
	if err := os.WriteFile(promptPath, []byte("Changed prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile prompt: %v", err)
	}
	second, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
	if err != nil {
		t.Fatalf("Load second: %v", err)
	}
	if first.Sources[0].Fingerprint == "" || first.Sources[0].Fingerprint == second.Sources[0].Fingerprint {
		t.Fatalf("fingerprints = %q then %q, want non-empty change", first.Sources[0].Fingerprint, second.Sources[0].Fingerprint)
	}
}

func TestFilesystemSourceFingerprintIgnoresNonLoadedNestedFiles(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")

	first, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
	if err != nil {
		t.Fatalf("Load first: %v", err)
	}
	exampleDir := filepath.Join(root, "harness", "architecture", "examples")
	if err := os.MkdirAll(exampleDir, 0o700); err != nil {
		t.Fatalf("MkdirAll examples: %v", err)
	}
	if err := os.WriteFile(filepath.Join(exampleDir, "prompt.md"), []byte("Example prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile example prompt: %v", err)
	}
	second, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
	if err != nil {
		t.Fatalf("Load second: %v", err)
	}
	if first.Sources[0].Fingerprint != second.Sources[0].Fingerprint {
		t.Fatalf("fingerprints = %q then %q, want unchanged for non-loaded nested files", first.Sources[0].Fingerprint, second.Sources[0].Fingerprint)
	}
}

func TestFilesystemSourceWarningsForRelativeTempAndGitWorktreePaths(t *testing.T) {
	cwd := t.TempDir()
	relativeRoot := filepath.Join(cwd, "agents")
	writeAgent(t, relativeRoot, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	t.Chdir(cwd)

	relativeCatalog, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{"agents"}})
	if err != nil {
		t.Fatalf("Load relative: %v", err)
	}
	relativeWarnings := relativeCatalog.Sources[0].Warnings
	if !hasWarning(relativeWarnings, "relative") || !hasWarning(relativeWarnings, "OS temp") {
		t.Fatalf("relative warnings = %#v, want relative and temp warnings", relativeWarnings)
	}

	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o700); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	gitSource := filepath.Join(repoRoot, "agents")
	writeAgent(t, gitSource, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	gitCatalog, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{gitSource}})
	if err != nil {
		t.Fatalf("Load git source: %v", err)
	}
	if !hasWarning(gitCatalog.Sources[0].Warnings, "Git worktree") {
		t.Fatalf("git warnings = %#v, want Git worktree warning", gitCatalog.Sources[0].Warnings)
	}
}

func TestRequireSafeProfileSourcesRejectsRelativeTempAndGitWorktreePaths(t *testing.T) {
	cwd := t.TempDir()
	relativeRoot := filepath.Join(cwd, "agents")
	writeAgent(t, relativeRoot, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	t.Chdir(cwd)
	err := RequireSafeProfileSources([]string{"agents"})
	if !errors.Is(err, ErrUnsafeSource) || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("relative error = %v, want ErrUnsafeSource with relative warning", err)
	}

	tempRoot := t.TempDir()
	writeAgent(t, tempRoot, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	err = RequireSafeProfileSources([]string{tempRoot})
	if !errors.Is(err, ErrUnsafeSource) || !strings.Contains(err.Error(), "OS temp") {
		t.Fatalf("temp error = %v, want ErrUnsafeSource with temp warning", err)
	}

	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o700); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	gitSource := filepath.Join(repoRoot, "agents")
	writeAgent(t, gitSource, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")
	err = RequireSafeProfileSources([]string{gitSource})
	if !errors.Is(err, ErrUnsafeSource) || !strings.Contains(err.Error(), "Git worktree") {
		t.Fatalf("git error = %v, want ErrUnsafeSource with Git worktree warning", err)
	}
}

func TestLoadRequireSafeProfileSourcesRejectsUnsafeSource(t *testing.T) {
	tempRoot := t.TempDir()
	writeAgent(t, tempRoot, "harness", "architecture", "Reviews architecture.", "sonnet", "medium", "Prompt text.\n")

	_, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{tempRoot}, RequireSafeProfileSources: true})
	if !errors.Is(err, ErrUnsafeSource) || !strings.Contains(err.Error(), "OS temp") {
		t.Fatalf("Load error = %v, want ErrUnsafeSource with temp warning", err)
	}
}

func TestLoadMergesSourcesByPrecedenceAndProvenance(t *testing.T) {
	ctx := context.Background()
	profileDir := t.TempDir()
	flagDir := t.TempDir()
	writeAgent(t, profileDir, "shared", "reviewer", "profile desc", "sonnet", "low", "profile prompt")
	writeAgent(t, flagDir, "shared", "reviewer", "flag desc", "opus", "high", "flag prompt")

	ref := testPRRef()
	reader := newRepoReader()
	pr := testPR("base-sha-123456789", "head-sha-987654321")
	reader.addAgent(t, ref, pr.Base.SHA, "shared", "reviewer", "repo desc", "repo prompt")
	reader.addAgent(t, ref, pr.Base.SHA, "repo", "only", "repo only desc", "repo only prompt")

	catalog, err := Load(ctx, LoadOptions{
		ProfileDirs: []string{profileDir},
		Repo:        &RepoSource{Reader: reader, Ref: ref, PR: pr},
		FlagDirs:    []string{flagDir},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2: %#v", len(catalog.Agents), catalog.Agents)
	}
	shared, ok := catalog.Find("shared:reviewer")
	if !ok {
		t.Fatal("shared:reviewer missing")
	}
	if shared.Description != "flag desc" || shared.Provenance.String() != "flag:1" {
		t.Fatalf("shared winner = (%q,%q), want flag override", shared.Description, shared.Provenance.String())
	}
	wantOverridden := []string{"profile:" + filepath.Base(filepath.Clean(profileDir)), "repo@refs/heads/main:base-sh"}
	if got := shared.Overridden; strings.Join(got, ",") != strings.Join(wantOverridden, ",") {
		t.Fatalf("overridden provenance = %#v, want %#v", got, wantOverridden)
	}
	repoOnly, ok := catalog.Find("repo:only")
	if !ok {
		t.Fatal("repo:only missing")
	}
	if repoOnly.Provenance.String() != "repo@refs/heads/main:base-sh" {
		t.Fatalf("repo provenance = %q, want repo@refs/heads/main:base-sh", repoOnly.Provenance.String())
	}
	if catalog.Repo == nil || catalog.Repo.Provenance != "repo@refs/heads/main:base-sh" || !strings.Contains(catalog.Repo.TrustNote(), "PR-head .codereview/agents changes do not affect") {
		t.Fatalf("repo info = %#v, want provenance and trust note", catalog.Repo)
	}
}

func hasWarning(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

func TestRepoLoaderUsesBaseSHAAndNeverHeadDefinitions(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	reader := newRepoReader()
	pr := testPR("base-sha-1111111", "head-sha-2222222")
	reader.addAgent(t, ref, pr.Base.SHA, "trusted", "reviewer", "base desc", "base prompt")
	reader.addAgent(t, ref, pr.Head.SHA, "trusted", "reviewer", "head desc", "head prompt")

	catalog, err := Load(ctx, LoadOptions{Repo: &RepoSource{Reader: reader, Ref: ref, PR: pr}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, ok := catalog.Find("trusted:reviewer")
	if !ok {
		t.Fatal("trusted:reviewer missing")
	}
	if agent.Description != "base desc" || strings.Contains(agent.Prompt, "head") {
		t.Fatalf("agent = %#v, want base definition only", agent)
	}
	for _, call := range reader.calls {
		if call.gitRef == pr.Head.SHA {
			t.Fatalf("repo loader used head SHA in call %#v", call)
		}
	}
}

func TestMissingRepoAgentsTreeIsEmptySource(t *testing.T) {
	ref := testPRRef()
	reader := newRepoReader()

	catalog, err := Load(context.Background(), LoadOptions{Repo: &RepoSource{Reader: reader, Ref: ref, PR: testPR("base-sha", "head-sha")}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Agents) != 0 {
		t.Fatalf("agents = %#v, want empty", catalog.Agents)
	}
	if catalog.Repo == nil {
		t.Fatal("repo info nil, want base trust metadata even when tree is absent")
	}
}

func TestLoadRejectsEmptyFilesystemSource(t *testing.T) {
	_, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{""}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
}

func TestLoadRejectsUnsafeAndMismatchedNames(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{
			name: "unsafe category path",
			mutate: func(t *testing.T, root string) {
				writeAgent(t, root, "bad:name", "reviewer", "desc", "sonnet", "low", "prompt")
			},
		},
		{
			name: "category yaml mismatch",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "wrong", "agent", "agent")
			},
		},
		{
			name: "empty category yaml name",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "", "agent", "agent")
			},
		},
		{
			name: "category yaml slash",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "bad/name", "agent", "agent")
			},
		},
		{
			name: "agent yaml mismatch",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "cat", "agent", "wrong")
			},
		},
		{
			name: "empty agent yaml name",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "cat", "agent", "")
			},
		},
		{
			name: "agent yaml backslash",
			mutate: func(t *testing.T, root string) {
				writeAgentWithNames(t, root, "cat", "cat", "agent", `bad\name`)
			},
		},
		{
			name: "dotdot category path",
			mutate: func(t *testing.T, root string) {
				writeAgent(t, root, "bad..category", "agent", "desc", "sonnet", "low", "prompt")
			},
		},
		{
			name: "unknown yaml field",
			mutate: func(t *testing.T, root string) {
				writeAgent(t, root, "cat", "agent", "desc", "sonnet", "low", "prompt")
				indexPath := filepath.Join(root, "cat", "agent", "index.yaml")
				appendFile(t, indexPath, "unexpected: true\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.mutate(t, root)
			_, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{root}})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRepoLoadRejectsUnsafeTreeAndYAMLNames(t *testing.T) {
	tests := []struct {
		name  string
		setup func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR)
	}{
		{
			name: "dot category tree name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: ".", Type: "tree"})
			},
		},
		{
			name: "unsafe category tree name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "bad..category", Type: "tree"})
			},
		},
		{
			name: "colon category tree name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "bad:category", Type: "tree"})
			},
		},
		{
			name: "empty category yaml name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: \"\"\ndescription: cat category\nowner: owner\n"))
			},
		},
		{
			name: "category yaml mismatch",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: other\ndescription: cat category\nowner: owner\n"))
			},
		},
		{
			name: "unsafe agent tree name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: cat\ndescription: cat category\nowner: owner\n"))
				reader.addTree(ref, pr.Base.SHA, categoryPath, gitprovider.TreeEntry{Path: categoryPath + `/bad\agent`, Type: "tree"})
			},
		},
		{
			name: "unsafe agent yaml name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				agentPath := categoryPath + "/agent"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: cat\ndescription: cat category\nowner: owner\n"))
				reader.addTree(ref, pr.Base.SHA, categoryPath, gitprovider.TreeEntry{Path: agentPath, Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, agentPath+"/index.yaml", []byte(agentIndexYAML("bad/name", "desc", "sonnet", "medium")))
				reader.addFile(ref, pr.Base.SHA, agentPath+"/prompt.md", []byte("prompt"))
			},
		},
		{
			name: "empty agent yaml name",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				agentPath := categoryPath + "/agent"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: cat\ndescription: cat category\nowner: owner\n"))
				reader.addTree(ref, pr.Base.SHA, categoryPath, gitprovider.TreeEntry{Path: agentPath, Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, agentPath+"/index.yaml", []byte(agentIndexYAML("", "desc", "sonnet", "medium")))
				reader.addFile(ref, pr.Base.SHA, agentPath+"/prompt.md", []byte("prompt"))
			},
		},
		{
			name: "agent yaml backslash",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				agentPath := categoryPath + "/agent"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: cat\ndescription: cat category\nowner: owner\n"))
				reader.addTree(ref, pr.Base.SHA, categoryPath, gitprovider.TreeEntry{Path: agentPath, Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, agentPath+"/index.yaml", []byte(agentIndexYAML(`bad\name`, "desc", "sonnet", "medium")))
				reader.addFile(ref, pr.Base.SHA, agentPath+"/prompt.md", []byte("prompt"))
			},
		},
		{
			name: "unknown yaml field",
			setup: func(reader *repoReader, ref gitprovider.PRRef, pr gitprovider.PR) {
				categoryPath := repoAgentsRoot + "/cat"
				agentPath := categoryPath + "/agent"
				reader.addTree(ref, pr.Base.SHA, repoAgentsRoot, gitprovider.TreeEntry{Path: "cat", Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, categoryPath+"/index.yaml", []byte("name: cat\ndescription: cat category\nowner: owner\n"))
				reader.addTree(ref, pr.Base.SHA, categoryPath, gitprovider.TreeEntry{Path: agentPath, Type: "tree"})
				reader.addFile(ref, pr.Base.SHA, agentPath+"/index.yaml", []byte(agentIndexYAML("agent", "desc", "sonnet", "medium")+"unexpected: true\n"))
				reader.addFile(ref, pr.Base.SHA, agentPath+"/prompt.md", []byte("prompt"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := testPRRef()
			pr := testPR("base-sha", "head-sha")
			reader := newRepoReader()
			tt.setup(reader, ref, pr)
			_, err := Load(context.Background(), LoadOptions{Repo: &RepoSource{Reader: reader, Ref: ref, PR: pr}})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRepoLoadRejectsMismatchedSourceRef(t *testing.T) {
	ref := testPRRef()
	pr := testPR("base-sha", "head-sha")
	otherRef := ref
	otherRef.Number = 99
	reader := newRepoReader()
	reader.addAgent(t, ref, pr.Base.SHA, "cat", "agent", "desc", "prompt")

	_, err := Load(context.Background(), LoadOptions{Repo: &RepoSource{Reader: reader, Ref: otherRef, PR: pr}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
}

func TestImportBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", file, err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			for _, forbidden := range []string{
				"github.com/open-cli-collective/codereview-cli/internal/cmd",
				"github.com/open-cli-collective/codereview-cli/internal/config",
				"github.com/open-cli-collective/codereview-cli/internal/credentials",
				"github.com/open-cli-collective/codereview-cli/internal/view",
			} {
				if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
					t.Fatalf("%s imports forbidden package %s", file, path)
				}
			}
		}
	}
}

func writeAgent(t *testing.T, root, category, agent, description, model, effort, prompt string) {
	t.Helper()
	writeAgentWithNames(t, root, category, category, agent, agent)
	indexPath := filepath.Join(root, category, agent, "index.yaml")
	writeFile(t, indexPath, agentIndexYAML(agent, description, model, effort))
	writeFile(t, filepath.Join(root, category, agent, "prompt.md"), prompt)
}

func writeAgentWithNames(t *testing.T, root, categoryPath, categoryName, agentPath, agentName string) {
	t.Helper()
	writeFile(t, filepath.Join(root, categoryPath, "index.yaml"), "name: "+categoryName+"\ndescription: "+categoryPath+" category\nowner: owner\n")
	writeFile(t, filepath.Join(root, categoryPath, agentPath, "index.yaml"), agentIndexYAML(agentName, "desc", "sonnet", "low"))
	writeFile(t, filepath.Join(root, categoryPath, agentPath, "prompt.md"), "prompt")
}

func agentIndexYAML(name, description, model, effort string) string {
	return "name: " + name + "\ndescription: " + description + "\nmodel: " + model + "\neffort: " + effort + "\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n"
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

func appendFile(t *testing.T, path, body string) {
	t.Helper()
	// #nosec G304 -- test paths are controlled by t.TempDir.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer file.Close()
	if _, err := file.WriteString(body); err != nil {
		t.Fatalf("WriteString(%s): %v", path, err)
	}
}

type repoCall struct {
	op     string
	gitRef string
	path   string
}

type repoFileSelector struct {
	ref    gitprovider.PRRef
	gitRef string
	path   string
}

type repoReader struct {
	trees map[repoFileSelector][]gitprovider.TreeEntry
	files map[repoFileSelector][]byte
	calls []repoCall
}

func newRepoReader() *repoReader {
	return &repoReader{
		trees: make(map[repoFileSelector][]gitprovider.TreeEntry),
		files: make(map[repoFileSelector][]byte),
	}
}

func (r *repoReader) addAgent(t *testing.T, ref gitprovider.PRRef, gitRef, category, agent, description, prompt string) {
	t.Helper()
	r.addTree(ref, gitRef, repoAgentsRoot, gitprovider.TreeEntry{Path: category, Type: "tree"})
	categoryPath := repoAgentsRoot + "/" + category
	r.addFile(ref, gitRef, categoryPath+"/index.yaml", []byte("name: "+category+"\ndescription: "+category+" category\nowner: repo-owner\n"))
	r.addTree(ref, gitRef, categoryPath, gitprovider.TreeEntry{Path: categoryPath + "/" + agent, Type: "tree"})
	agentPath := categoryPath + "/" + agent
	r.addFile(ref, gitRef, agentPath+"/index.yaml", []byte(agentIndexYAML(agent, description, "sonnet", "medium")))
	r.addFile(ref, gitRef, agentPath+"/prompt.md", []byte(prompt))
}

func (r *repoReader) addTree(ref gitprovider.PRRef, gitRef, treePath string, entry gitprovider.TreeEntry) {
	key := repoFileSelector{ref: ref, gitRef: gitRef, path: treePath}
	r.trees[key] = append(r.trees[key], entry)
	sort.Slice(r.trees[key], func(i, j int) bool { return r.trees[key][i].Path < r.trees[key][j].Path })
}

func (r *repoReader) addFile(ref gitprovider.PRRef, gitRef, path string, contents []byte) {
	r.files[repoFileSelector{ref: ref, gitRef: gitRef, path: path}] = append([]byte(nil), contents...)
}

func (r *repoReader) ListTreeAtRef(_ context.Context, ref gitprovider.PRRef, gitRef string, treePath string) ([]gitprovider.TreeEntry, error) {
	r.calls = append(r.calls, repoCall{op: "tree", gitRef: gitRef, path: treePath})
	entries, ok := r.trees[repoFileSelector{ref: ref, gitRef: gitRef, path: treePath}]
	if !ok {
		return nil, gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationListTreeAtRef, nil)
	}
	return append([]gitprovider.TreeEntry(nil), entries...), nil
}

func (r *repoReader) GetFileAtRef(_ context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	r.calls = append(r.calls, repoCall{op: "file", gitRef: gitRef, path: path})
	contents, ok := r.files[repoFileSelector{ref: ref, gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationGetFileAtRef, nil)
	}
	return append([]byte(nil), contents...), nil
}

func testPRRef() gitprovider.PRRef {
	return gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 28}
}

func testPR(baseSHA, headSHA string) gitprovider.PR {
	ref := testPRRef()
	return gitprovider.PR{
		Ref:   ref,
		Title: "CR-19",
		Base:  gitprovider.PRBranchRef{Name: "main", Ref: "refs/heads/main", SHA: baseSHA},
		Head:  gitprovider.PRBranchRef{Name: "feature", Ref: "refs/heads/feature", SHA: headSHA},
	}
}
