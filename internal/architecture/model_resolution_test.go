package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeModelResolutionGoesThroughStageResolver(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	allowedDirs := []string{
		"internal/config",
		"internal/stagemodel",
		"internal/cmd/configcmd",
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "dist", "vendor":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(mustRel(t, repoRoot, path))
		if pathInAllowedDirs(rel, allowedDirs) {
			return nil
		}

		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "ResolveModelTier" {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Name != "config" {
				return true
			}
			pos := fset.Position(selector.Pos())
			t.Fatalf("%s calls config.ResolveModelTier directly; runtime model selection must use internal/stagemodel", pos)
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs(.): %v", err)
	}
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("Rel(%s, %s): %v", base, target, err)
	}
	return rel
}

func pathInAllowedDirs(path string, allowedDirs []string) bool {
	for _, dir := range allowedDirs {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}
