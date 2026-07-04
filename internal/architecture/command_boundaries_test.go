package architecture_test

import (
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
	knownDebt := map[string]string{
		"internal/cmd/benchmarkcmd/select.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd":   "#423 moves reusable app contracts out of reviewcmd",
		"internal/cmd/respondcmd/respondcmd.go -> github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd": "#422 extracts command-independent runtime assembly",
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(cmdRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
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
			if reason, ok := knownDebt[key]; ok {
				t.Logf("allowed known command coupling debt: %s (%s)", key, reason)
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
