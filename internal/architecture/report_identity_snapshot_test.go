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

func TestReportIdentitySnapshotRemainsValueOnly(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	stdlib := standardLibraryImports(t, repoRoot)
	fset := token.NewFileSet()

	err := filepath.WalkDir(filepath.Join(repoRoot, "internal", "reportidentitysnapshot"), func(path string, entry fs.DirEntry, walkErr error) error {
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
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if _, isStdlib := stdlib[importPath]; isStdlib || importPath == modulePath+"/internal/review" {
				continue
			}
			t.Fatalf("%s imports %q; reportidentitysnapshot may import only standard library packages and internal/review", path, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(reportidentitysnapshot): %v", err)
	}
}
