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

func TestReportIdentityReplayStaysOfflineAndDiagnostic(t *testing.T) {
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
		inExperiment := strings.HasPrefix(rel, "internal/identityexperiment/")
		inShadow := strings.HasPrefix(rel, "internal/identityshadow/")
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if importPath == identityImport && !strings.HasPrefix(rel, "internal/cmd/benchmarkcmd/") && !inExperiment && !inShadow {
				t.Fatalf("%s imports internal/reportidentity; replay may only support benchmark and isolated diagnostic behavior", rel)
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

func TestIdentityInferenceStaysBehindDiagnosticBoundaries(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	modulePath := "github.com/open-cli-collective/codereview-cli"
	systemOneImport := modulePath + "/internal/systemone"
	openAIDecisionsImport := modulePath + "/internal/openaidecisions"
	cloudflareClefImport := modulePath + "/internal/cloudflareclef"
	identityInferenceImport := modulePath + "/internal/reportidentityinference"
	identityImport := modulePath + "/internal/reportidentity"
	identityExperimentImport := modulePath + "/internal/identityexperiment"
	identityShadowImport := modulePath + "/internal/identityshadow"
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
		inExperiment := strings.HasPrefix(rel, "internal/identityexperiment/")
		inShadow := strings.HasPrefix(rel, "internal/identityshadow/")
		inApp := strings.HasPrefix(rel, "internal/app/")
		inSystemOneClient := strings.HasPrefix(rel, "internal/systemone/")
		inOpenAIDecisionsClient := strings.HasPrefix(rel, "internal/openaidecisions/")
		inCloudflareClefClient := strings.HasPrefix(rel, "internal/cloudflareclef/")
		inAdapter := strings.HasPrefix(rel, "internal/reportidentityinference/")
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if (importPath == systemOneImport || strings.HasPrefix(importPath, systemOneImport+"/")) && !inCommand && !inAdapter && !inCloudflareClefClient && !inExperiment {
				t.Fatalf("%s imports internal/systemone; inference transport may be invoked only by benchmarkcmd, Cloudflare Clef, its identity adapter, or identityexperiment", rel)
			}
			if (importPath == openAIDecisionsImport || strings.HasPrefix(importPath, openAIDecisionsImport+"/")) && !inCommand {
				t.Fatalf("%s imports internal/openaidecisions; Decisions transport may be invoked only by benchmarkcmd", rel)
			}
			if (importPath == cloudflareClefImport || strings.HasPrefix(importPath, cloudflareClefImport+"/")) && !inCommand {
				t.Fatalf("%s imports internal/cloudflareclef; Cloudflare Clef transport may be invoked only by benchmarkcmd", rel)
			}
			if (importPath == identityInferenceImport || strings.HasPrefix(importPath, identityInferenceImport+"/")) && !inCommand && !inExperiment {
				t.Fatalf("%s imports internal/reportidentityinference; identity inference may be invoked only by benchmarkcmd or identityexperiment", rel)
			}
			if (importPath == identityImport || strings.HasPrefix(importPath, identityImport+"/")) && !inCommand && !inExperiment && !inShadow {
				t.Fatalf("%s imports internal/reportidentity; replay may be invoked only by benchmarkcmd or identityexperiment", rel)
			}
			if (importPath == identityExperimentImport || strings.HasPrefix(importPath, identityExperimentImport+"/")) && !inCommand && !inExperiment {
				if !inShadow && !inApp {
					t.Fatalf("%s imports internal/identityexperiment; shared execution is limited to its adapters and isolated review diagnostics", rel)
				}
			}
			if (importPath == identityShadowImport || strings.HasPrefix(importPath, identityShadowImport+"/")) && !inApp {
				t.Fatalf("%s imports internal/identityshadow; the isolated observer may only be composed by app", rel)
			}
			if inExperiment {
				if _, isStdlib := stdlib[importPath]; isStdlib || importPath == modulePath+"/internal/review" || importPath == identityImport || importPath == identityInferenceImport || importPath == systemOneImport {
					continue
				}
				t.Fatalf("%s imports %q; identityexperiment may import only standard library packages, reportidentity, reportidentityinference, review, and System One", rel, importPath)
			}
			if inShadow {
				if _, isStdlib := stdlib[importPath]; isStdlib || importPath == modulePath+"/internal/review" || importPath == modulePath+"/internal/reportidentity" || importPath == modulePath+"/internal/reportidentitysnapshot" || importPath == identityExperimentImport || importPath == "go.yaml.in/yaml/v3" {
					continue
				}
				forbidden := []string{
					"/internal/app", "/internal/pipeline", "/internal/reviewrun", "/internal/reviewplan",
					"/internal/outbox", "/internal/gate", "/internal/gateio", "/internal/gitprovider",
					"/internal/gitproviders", "/internal/ledger", "/internal/hooks", "/internal/llm",
					"/internal/llmadapters", "/internal/credentials", "/internal/cmd",
				}
				for _, packageName := range forbidden {
					if strings.HasPrefix(importPath, modulePath+packageName) {
						t.Fatalf("%s imports %q; identityshadow must remain isolated from review authority and credential backends", rel, importPath)
					}
				}
				t.Fatalf("%s imports %q; identityshadow may import only standard library, identityexperiment, reportidentity, reportidentitysnapshot, and review", rel, importPath)
			}
			if !inSystemOneClient && !inOpenAIDecisionsClient && !inCloudflareClefClient && !inAdapter {
				continue
			}
			if _, isStdlib := stdlib[importPath]; isStdlib || ((inAdapter || inCloudflareClefClient) && importPath == systemOneImport) {
				continue
			}
			t.Fatalf("%s imports %q; inference transports are standard-library only, while Cloudflare Clef and the identity adapter may additionally import System One", rel, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", repoRoot, err)
	}
}
