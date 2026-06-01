package pipeline

import (
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

func TestProductionImportsStayOutOfCommandAndConcreteRuntimeLayers(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(testFile)
	modulePath := modulePath(t, dir)
	forbidden := []string{
		modulePath + "/internal/cmd",
		modulePath + "/internal/credentials",
		modulePath + "/internal/gitprovider/github",
		modulePath + "/internal/view",
		"github.com/spf13/cobra",
	}

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
			for _, prefix := range forbidden {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					t.Fatalf("%s imports %q, which crosses the dry-run pipeline boundary", filepath.Base(file), path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
}

func modulePath(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Path}}")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list module: %v", err)
	}
	return strings.TrimSpace(string(output))
}
