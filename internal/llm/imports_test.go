package llm

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

func TestStructuredContractsStayNoIO(t *testing.T) {
	for _, file := range []string{"adapter.go", "contracts.go", "fake.go"} {
		assertNoIOImports(t, file)
	}
}

func assertNoIOImports(t *testing.T, file string) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("ParseFile(%s): %v", file, err)
	}
	for _, imported := range parsed.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("Unquote import in %s: %v", file, err)
		}
		switch path {
		case "io/fs", "net", "net/http", "os", "os/exec", "syscall":
			t.Fatalf("%s imports %q, want structured contracts without IO/process dependencies", file, path)
		}
	}
}
