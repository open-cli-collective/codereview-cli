package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommandPackagesDoNotAddFeatureCommandImports(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	cmdRoot := filepath.Join(repoRoot, "internal", "cmd")
	sharedCommandImports := map[string]bool{
		modulePath + "/internal/cmd/cmderr":     true,
		modulePath + "/internal/cmd/cmdruntime": true,
		modulePath + "/internal/cmd/exitcode":   true,
		modulePath + "/internal/cmd/root":       true,
	}

	checkCommandFeatureImports(t, repoRoot, cmdRoot, modulePath, sharedCommandImports, false, nil)
}

func TestCommandPackageTestsKeepFeatureCommandImportsAtCommandTreeBoundaries(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	cmdRoot := filepath.Join(repoRoot, "internal", "cmd")
	sharedCommandImports := map[string]bool{
		modulePath + "/internal/cmd/cmderr":     true,
		modulePath + "/internal/cmd/cmdruntime": true,
		modulePath + "/internal/cmd/exitcode":   true,
		modulePath + "/internal/cmd/root":       true,
	}
	allowedTestImports := map[string]string{
		"internal/cmd/mecmd/mecmd_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd":       "me command integration registers support commands",
		"internal/cmd/mecmd/mecmd_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd":   "me command integration registers support commands",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/agentscmd":     "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/configcmd":     "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/credentialcmd": "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/datacmd":       "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/mecmd":         "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd":     "command-surface noleak harness registers the command tree",
		"internal/cmd/noleak/noleak_test.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/sessionscmd":   "command-surface noleak harness registers the command tree",
	}

	checkCommandFeatureImports(t, repoRoot, cmdRoot, modulePath, sharedCommandImports, true, allowedTestImports)
}

func checkCommandFeatureImports(t *testing.T, repoRoot, cmdRoot, modulePath string, sharedCommandImports map[string]bool, testFiles bool, allowedImports map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(cmdRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") != testFiles {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(mustRel(t, repoRoot, path))
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(importPath, modulePath+"/internal/cmd/") {
				continue
			}
			if sharedCommandImports[importPath] {
				continue
			}
			key := rel + " -> " + importPath
			if reason, ok := allowedImports[key]; ok {
				t.Logf("allowed command-tree integration import: %s (%s)", key, reason)
				continue
			}
			pos := fset.Position(spec.Pos())
			t.Fatalf("%s imports feature command package %s; reusable command behavior must move through shared app/runtime seams", pos, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", cmdRoot, err)
	}
}

func TestApplicationPackagesStayOutOfCommandAndViewLayers(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	appRoots := applicationPackageRoots(t, repoRoot)
	forbidden := []string{
		modulePath + "/internal/cmd",
		modulePath + "/internal/view",
		"github.com/charmbracelet/bubbles",
		"github.com/charmbracelet/bubbletea",
		"github.com/charmbracelet/huh",
		"github.com/charmbracelet/lipgloss",
		"github.com/spf13/cobra",
	}

	for _, root := range appRoots {
		checkApplicationRootImports(t, repoRoot, root, forbidden)
	}
}

func TestCommandRuntimeDoesNotOwnApplicationRuntimeContracts(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	cmdRuntimeDir := filepath.Join(repoRoot, "internal", "cmd", "cmdruntime")
	forbiddenImports := map[string]string{
		modulePath + "/internal/datalifecycle": "retention policy mapping belongs in internal/appruntime",
		modulePath + "/internal/reporoot":      "repo-root resolution belongs in internal/appruntime",
	}
	forbiddenFuncs := map[string]string{
		"RetentionPolicyFromConfig": "retention policy mapping belongs in internal/appruntime",
		"ResolveRepoRoot":           "repo-root resolution belongs in internal/appruntime",
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(cmdRuntimeDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if reason, forbidden := forbiddenImports[importPath]; forbidden {
				pos := fset.Position(spec.Pos())
				t.Fatalf("%s imports %s; %s", pos, importPath, reason)
			}
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			if reason, forbidden := forbiddenFuncs[fn.Name.Name]; forbidden {
				pos := fset.Position(fn.Pos())
				t.Fatalf("%s defines %s; %s", pos, fn.Name.Name, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", cmdRuntimeDir, err)
	}
}

func applicationPackageRoots(t *testing.T, repoRoot string) []string {
	t.Helper()
	internalRoot := filepath.Join(repoRoot, "internal")
	entries, err := os.ReadDir(internalRoot)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", internalRoot, err)
	}
	excluded := map[string]bool{
		"cmd":  true,
		"view": true,
	}
	var roots []string
	for _, entry := range entries {
		if !entry.IsDir() || excluded[entry.Name()] {
			continue
		}
		roots = append(roots, filepath.ToSlash(filepath.Join("internal", entry.Name())))
	}
	if len(roots) == 0 {
		t.Fatal("no application package roots discovered under internal")
	}
	return roots
}

func checkApplicationRootImports(t *testing.T, repoRoot, root string, forbidden []string) {
	t.Helper()
	dir := filepath.Join(repoRoot, filepath.FromSlash(root))
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, prefix := range forbidden {
				if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
					pos := fset.Position(spec.Pos())
					t.Fatalf("%s imports %s; application packages must not depend on command/UI layers", pos, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
}
