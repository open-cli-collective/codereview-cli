package architecture_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSelectedProductionPackagesStayStdlibOnly(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	stdlib := standardLibraryImports(t, repoRoot)
	for _, relDir := range []string{"internal/gate", "internal/fsatomic", "internal/marker"} {
		t.Run(relDir, func(t *testing.T) {
			checkStdlibOnlyPackage(t, repoRoot, relDir, stdlib)
		})
	}
}

func checkStdlibOnlyPackage(t *testing.T, repoRoot, relDir string, stdlib map[string]struct{}) {
	t.Helper()
	root := filepath.Join(repoRoot, filepath.FromSlash(relDir))
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(filePath) != ".go" || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, filePath, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			importedPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(mustRel(t, repoRoot, filePath))
			if _, ok := stdlib[importedPath]; !ok {
				t.Fatalf("%s imports %q, want standard library only", rel, importedPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", root, err)
	}
}

func standardLibraryImports(t *testing.T, repoRoot string) map[string]struct{} {
	t.Helper()
	cmd := exec.Command("go", "list", "std")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list std: %v", err)
	}
	imports := make(map[string]struct{})
	for _, path := range bytes.Fields(output) {
		imports[string(path)] = struct{}{}
	}
	return imports
}
