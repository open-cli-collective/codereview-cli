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

func TestPayloadStructsAreOwnedByPlannedActions(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	want := map[string]bool{
		"InlineCommentPayload": false,
		"ThreadReplyPayload":   false,
		"ResolveThreadPayload": false,
		"RollupCommentPayload": false,
		"SubmitReviewPayload":  false,
	}
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
		for _, declaration := range parsed.Decls {
			generic, ok := declaration.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if _, ok := typeSpec.Type.(*ast.StructType); !ok {
					continue
				}
				if _, tracked := want[typeSpec.Name.Name]; !tracked {
					continue
				}
				rel := filepath.ToSlash(mustRel(t, repoRoot, path))
				if rel != "internal/plannedactions/plannedactions.go" {
					t.Fatalf("%s declares %s; payload structs belong in internal/plannedactions", rel, typeSpec.Name.Name)
				}
				if want[typeSpec.Name.Name] {
					t.Fatalf("%s declares %s more than once", rel, typeSpec.Name.Name)
				}
				want[typeSpec.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for name, found := range want {
		if !found {
			t.Errorf("canonical payload struct %s not found", name)
		}
	}
}

func TestPlannedActionsStaysLeaf(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	blocked := map[string]bool{
		"github.com/open-cli-collective/codereview-cli/internal/ledger":     true,
		"github.com/open-cli-collective/codereview-cli/internal/outbox":     true,
		"github.com/open-cli-collective/codereview-cli/internal/reviewplan": true,
	}
	dir := filepath.Join(repoRoot, "internal/plannedactions")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", path, err)
		}
		for _, spec := range parsed.Imports {
			if blocked[strings.Trim(spec.Path.Value, `"`)] {
				t.Fatalf("%s imports %s; plannedactions must remain a leaf", path, spec.Path.Value)
			}
		}
	}
}
