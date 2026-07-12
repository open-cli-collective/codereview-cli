package architecture_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGitTransportConstructionAndWorkbenchDependenciesStayLayered(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	internalRoot := filepath.Join(repoRoot, "internal")
	gitexecImport := "github.com/open-cli-collective/codereview-cli/internal/gitexec"
	reporootImport := "github.com/open-cli-collective/codereview-cli/internal/reporoot"
	fset := token.NewFileSet()
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
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
			if importPath == gitexecImport && !strings.HasPrefix(rel, "internal/app/") {
				t.Fatalf("%s imports gitexec; production Git transport construction belongs in internal/app", rel)
			}
			if importPath == reporootImport && strings.HasPrefix(rel, "internal/workbench/") {
				t.Fatalf("%s imports reporoot; workbench preparation must not depend on the invocation checkout", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", internalRoot, err)
	}
}
