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
	if got := shared.Overridden; len(got) != 2 || got[0] == got[1] {
		t.Fatalf("overridden provenance = %#v, want profile and repo provenance", got)
	}
	repoOnly, ok := catalog.Find("repo:only")
	if !ok {
		t.Fatal("repo:only missing")
	}
	if repoOnly.Provenance.String() != "repo@main:base-sh" {
		t.Fatalf("repo provenance = %q, want repo@main:base-sh", repoOnly.Provenance.String())
	}
	if catalog.Repo == nil || catalog.Repo.Provenance != "repo@main:base-sh" || !strings.Contains(catalog.Repo.TrustNote(), "PR-head .codereview/agents changes do not affect") {
		t.Fatalf("repo info = %#v, want provenance and trust note", catalog.Repo)
	}
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
