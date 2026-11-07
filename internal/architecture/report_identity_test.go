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

func TestReportIdentityStaysOfflineAndOutsideReviewRuntime(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	identityImport := modulePath + "/internal/reportidentity"
	stdlib := standardLibraryImports(t, repoRoot)
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
			if importPath == identityImport && !strings.HasPrefix(rel, "internal/cmd/benchmarkcmd/") {
				t.Fatalf("%s imports internal/reportidentity; replay is offline tooling and cannot join production behavior", rel)
			}
			if !strings.HasPrefix(rel, "internal/reportidentity/") {
				continue
			}
			if _, isStdlib := stdlib[importPath]; isStdlib || importPath == modulePath+"/internal/review" {
				continue
			}
			t.Fatalf("%s imports %q; reportidentity may import only standard library packages and internal/review", rel, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}

func TestIdentityInferenceClientsStayBehindBenchmarkCommand(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	systemOneImport := modulePath + "/internal/systemone"
	openAIDecisionsImport := modulePath + "/internal/openaidecisions"
	identityInferenceImport := modulePath + "/internal/reportidentityinference"
	stdlib := standardLibraryImports(t, repoRoot)
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
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(mustRel(t, repoRoot, path))
		inCommand := strings.HasPrefix(rel, "internal/cmd/benchmarkcmd/")
		inSystemOneClient := strings.HasPrefix(rel, "internal/systemone/")
		inOpenAIDecisionsClient := strings.HasPrefix(rel, "internal/openaidecisions/")
		inAdapter := strings.HasPrefix(rel, "internal/reportidentityinference/")
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if (importPath == systemOneImport || strings.HasPrefix(importPath, systemOneImport+"/")) && !inCommand && !inAdapter {
				t.Fatalf("%s imports internal/systemone; inference transport may be invoked only by benchmarkcmd or its identity adapter", rel)
			}
			if (importPath == openAIDecisionsImport || strings.HasPrefix(importPath, openAIDecisionsImport+"/")) && !inCommand {
				t.Fatalf("%s imports internal/openaidecisions; Decisions transport may be invoked only by benchmarkcmd", rel)
			}
			if (importPath == identityInferenceImport || strings.HasPrefix(importPath, identityInferenceImport+"/")) && !inCommand {
				t.Fatalf("%s imports internal/reportidentityinference; identity inference may be invoked only by benchmarkcmd", rel)
			}
			if !inSystemOneClient && !inOpenAIDecisionsClient && !inAdapter {
				continue
			}
			if _, isStdlib := stdlib[importPath]; isStdlib || (inAdapter && importPath == systemOneImport) {
				continue
			}
			t.Fatalf("%s imports %q; inference transports are standard-library only and the identity adapter may additionally import System One", rel, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}
