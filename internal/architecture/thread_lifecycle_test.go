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

func TestProviderWritesGoThroughPostingBoundary(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	allowedDirs := []string{
		"internal/outbox",
		"internal/gitprovider",
	}
	allowedFiles := map[string]bool{
		"internal/app/provider_progress.go": true,
		"internal/app/runtime_provider.go":  true,
	}
	writeMethods := map[string]bool{
		"PostInlineComment": true,
		"ReplyToThread":     true,
		"ResolveThread":     true,
		"PostIssueComment":  true,
		"SubmitReview":      true,
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
		if pathInAllowedDirs(rel, allowedDirs) || allowedFiles[rel] {
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
			if !ok || !writeMethods[selector.Sel.Name] {
				return true
			}
			pos := fset.Position(selector.Pos())
			t.Fatalf("%s calls provider write method %s outside the posting boundary", pos, selector.Sel.Name)
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}

func TestPackagesStayOnLayeredSeams(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	blockedImports := map[string]map[string]bool{
		"internal/threadcontext": {
			"github.com/open-cli-collective/codereview-cli/internal/ledger":         true,
			"github.com/open-cli-collective/codereview-cli/internal/llm":            true,
			"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle":   true,
			"github.com/open-cli-collective/codereview-cli/internal/outbox":         true,
			"github.com/open-cli-collective/codereview-cli/internal/pipeline":       true,
			"github.com/open-cli-collective/codereview-cli/internal/reviewplan":     true,
			"github.com/open-cli-collective/codereview-cli/internal/threadanalysis": true,
			"github.com/open-cli-collective/codereview-cli/internal/threadrespond":  true,
		},
		"internal/threadanalysis": {
			"github.com/open-cli-collective/codereview-cli/internal/ledger":        true,
			"github.com/open-cli-collective/codereview-cli/internal/outbox":        true,
			"github.com/open-cli-collective/codereview-cli/internal/pipeline":      true,
			"github.com/open-cli-collective/codereview-cli/internal/reviewplan":    true,
			"github.com/open-cli-collective/codereview-cli/internal/threadrespond": true,
		},
		"internal/threadrespond": {
			"github.com/open-cli-collective/codereview-cli/internal/pipeline": true,
		},
		"internal/workbench": {
			"github.com/open-cli-collective/codereview-cli/internal/pipeline": true,
		},
	}

	for dir, blocked := range blockedImports {
		checkPackageImports(t, repoRoot, dir, blocked)
	}
}

func checkPackageImports(t *testing.T, repoRoot, dir string, blocked map[string]bool) {
	t.Helper()
	root := filepath.Join(repoRoot, filepath.FromSlash(dir))
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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
			importPath := strings.Trim(spec.Path.Value, `"`)
			if !blocked[importPath] {
				continue
			}
			pos := fset.Position(spec.Pos())
			t.Fatalf("%s imports %s; package must use the documented layered seams", pos, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", root, err)
	}
}
