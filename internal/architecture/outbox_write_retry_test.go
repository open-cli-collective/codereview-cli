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

// A write path that omits WriteRetry compiles and silently loses in-run recovery,
// so every outbox.Post and every options literal that feeds one must name the field.
func TestOutboxPostPathsCarryWriteRetry(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	feedsPost := map[string]bool{"reviewrun": true, "gateio": true, "threadrespond": true}
	postCalls := 0

	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, entry fs.DirEntry, walkErr error) error {
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
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				if selectorIs(n.Fun, "outbox", "Post") {
					postCalls++
					literal, ok := postOptionsLiteral(n)
					if !ok {
						t.Errorf("%s: outbox.Post options must be a literal so WriteRetry can be checked", fset.Position(n.Pos()))
					} else if !namesField(literal, "WriteRetry") {
						t.Errorf("%s: outbox.Post options omit WriteRetry", fset.Position(n.Pos()))
					}
				}
			case *ast.CompositeLit:
				if pkg, ok := optionsPackage(n.Type); ok && feedsPost[pkg] && !namesField(n, "WriteRetry") {
					t.Errorf("%s: %s.Options omits WriteRetry", fset.Position(n.Pos()), pkg)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
	if postCalls == 0 {
		t.Fatal("found no outbox.Post calls, so this test checked nothing")
	}
}

func selectorIs(expr ast.Expr, pkg, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func postOptionsLiteral(call *ast.CallExpr) (*ast.CompositeLit, bool) {
	if len(call.Args) < 2 {
		return nil, false
	}
	literal, ok := call.Args[1].(*ast.CompositeLit)
	return literal, ok
}

func optionsPackage(expr ast.Expr) (string, bool) {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Options" {
		return "", false
	}
	ident, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}

func namesField(literal *ast.CompositeLit, field string) bool {
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyValue.Key.(*ast.Ident); ok && key.Name == field {
			return true
		}
	}
	return false
}
