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

// Accepting a repository invitation is the one account-level provider write.
// It stays inside internal/invitations, which enforces the allowlist and
// dry-run, so no other caller can accept invitations without them. Any
// selector naming the method counts, so a method value or method expression
// cannot slip past where a direct call would be caught. Adapters only declare
// the method, and declarations are not selectors.
func TestRepositoryInvitationAcceptsStayInInvitationsPackage(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	allowedDirs := []string{"internal/invitations"}

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
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "AcceptRepositoryInvitation" {
				return true
			}
			t.Fatalf("%s references AcceptRepositoryInvitation outside internal/invitations", fset.Position(selector.Pos()))
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}
