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

func TestStructuredLLMCallsGoThroughLifecycle(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	allowedDirs := []string{
		"internal/llm",
		"internal/llmlifecycle",
	}
	directCalls := map[string]bool{
		"RunStructured":                  true,
		"RunStructuredWithSession":       true,
		"RunStructuredWithSessionResume": true,
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
		llmAliases := importedAliases(parsed, "github.com/open-cli-collective/codereview-cli/internal/llm")
		if len(llmAliases) == 0 {
			return nil
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !directCalls[selector.Sel.Name] {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || !llmAliases[ident.Name] {
				return true
			}
			pos := fset.Position(selector.Pos())
			t.Fatalf("%s calls llm.%s directly; structured LLM actions must go through internal/llmlifecycle", pos, selector.Sel.Name)
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}

func importedAliases(file *ast.File, importPath string) map[string]bool {
	aliases := map[string]bool{}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path != importPath {
			continue
		}
		if spec.Name != nil {
			aliases[spec.Name.Name] = true
			continue
		}
		parts := strings.Split(path, "/")
		aliases[parts[len(parts)-1]] = true
	}
	return aliases
}
