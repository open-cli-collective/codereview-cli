package gate

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestProductionImportsStayStdlibOnly(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(testFile)
	repoRoot, modulePath := repoRootAndModule(t, dir)
	stdlib := stdlibImports(t, repoRoot)
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(file) != ".go" || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(path, modulePath+"/") || path == modulePath {
				t.Fatalf("production import %q is from this repo, want stdlib only", path)
			}
			if _, ok := stdlib[path]; !ok {
				t.Fatalf("production import %q is not in the standard library", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
}

func repoRootAndModule(t *testing.T, dir string) (string, string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Path}}")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list module: %v", err)
	}
	parts := strings.Fields(string(output))
	if len(parts) != 2 {
		t.Fatalf("go list module output = %q, want dir and path", output)
	}
	return parts[0], parts[1]
}

func stdlibImports(t *testing.T, repoRoot string) map[string]struct{} {
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
