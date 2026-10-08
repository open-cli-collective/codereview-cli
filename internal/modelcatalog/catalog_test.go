package modelcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/statedirtest"
)

func TestBundledCatalogAccessorsAreDeepCopies(t *testing.T) {
	catalog, err := LoadBundled()
	if err != nil {
		t.Fatalf("LoadBundled: %v", err)
	}

	models := catalog.Models()
	if len(models) == 0 || len(models[0].SupportedEfforts) == 0 {
		t.Fatal("bundled catalog has no model effort data to copy")
	}
	models[0].SupportedEfforts[0] = "mutated"
	if catalog.Models()[0].SupportedEfforts[0] == "mutated" {
		t.Fatal("Models exposed the catalog's effort slice")
	}

	pricing := catalog.Pricing()
	var priceIndex int
	for priceIndex = range pricing {
		if pricing[priceIndex].Input != nil {
			break
		}
	}
	if priceIndex == len(pricing) {
		t.Fatal("bundled catalog has no input price to copy")
	}
	original := *pricing[priceIndex].Input
	*pricing[priceIndex].Input = original + 1
	if got := *catalog.Pricing()[priceIndex].Input; got != original {
		t.Fatal("Pricing exposed a mutable rate pointer")
	}

	manifest := catalog.Manifest()
	manifest.Files["models.csv"] = "mutated"
	if catalog.Manifest().Files["models.csv"] == "mutated" {
		t.Fatal("Manifest exposed its checksum map")
	}
}

func TestLoadPathPreservesCapabilityStates(t *testing.T) {
	tests := []struct {
		name                      string
		fast, ultrafast           string
		wantFast, wantFastKnown   bool
		wantUltra, wantUltraKnown bool
	}{
		{name: "yes and no", fast: "Y", ultrafast: "N", wantFast: true, wantFastKnown: true, wantUltra: false, wantUltraKnown: true},
		{name: "empty and null", fast: "", ultrafast: "null", wantFast: false, wantFastKnown: false, wantUltra: false, wantUltraKnown: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := copyBundledCatalog(t)
			modelID := "capability-" + strings.ReplaceAll(tt.name, " ", "-")
			appendModel(t, source, modelID, tt.fast, tt.ultrafast)
			catalog, err := LoadPath(source)
			if err != nil {
				t.Fatalf("LoadPath: %v", err)
			}
			var got Model
			for _, model := range catalog.Models() {
				if model.ModelID == modelID {
					got = model
					break
				}
			}
			if got.ModelID == "" {
				t.Fatalf("model %q was not loaded", modelID)
			}
			if got.Fast != tt.wantFast || got.FastKnown != tt.wantFastKnown || got.Ultrafast != tt.wantUltra || got.UltrafastKnown != tt.wantUltraKnown {
				t.Fatalf("capability = fast(%v,%v) ultrafast(%v,%v), want fast(%v,%v) ultrafast(%v,%v)", got.Fast, got.FastKnown, got.Ultrafast, got.UltrafastKnown, tt.wantFast, tt.wantFastKnown, tt.wantUltra, tt.wantUltraKnown)
			}
		})
	}

	invalid := copyBundledCatalog(t)
	appendModel(t, invalid, "capability-invalid", "TRUE", "")
	if _, err := LoadPath(invalid); err == nil || !strings.Contains(err.Error(), "must be Y, N, empty, or null") {
		t.Fatalf("invalid capability error = %v, want parser rejection", err)
	}
}

func TestLoadInstalledFallsBackUntilPointerIsPublished(t *testing.T) {
	statedirtest.Hermetic(t)
	installed, err := InstalledDir()
	if err != nil {
		t.Fatalf("InstalledDir: %v", err)
	}

	for name, makeInstall := range map[string]func(t *testing.T, dir string){
		"fresh": func(*testing.T, string) {},
		"staged without pointer": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".catalog-install-pending"), 0o700); err != nil {
				t.Fatalf("MkdirAll pending: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(installed); err != nil {
				t.Fatalf("RemoveAll installed: %v", err)
			}
			makeInstall(t, installed)
			catalog, err := LoadInstalled()
			if err != nil {
				t.Fatalf("LoadInstalled: %v", err)
			}
			if catalog.Source().Kind != "bundled" {
				t.Fatalf("source kind = %q, want bundled", catalog.Source().Kind)
			}
		})
	}

	if err := os.MkdirAll(installed, 0o700); err != nil {
		t.Fatalf("MkdirAll installed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(installed, currentPointer), []byte("../outside\n"), 0o600); err != nil {
		t.Fatalf("write invalid pointer: %v", err)
	}
	if _, err := LoadInstalled(); err == nil || !strings.Contains(err.Error(), "pointer is invalid") {
		t.Fatalf("invalid pointer error = %v, want rejection", err)
	}

	source := copyBundledCatalog(t)
	revision := filepath.Join(installed, "r-corrupt")
	if err := os.MkdirAll(revision, 0o700); err != nil {
		t.Fatalf("MkdirAll revision: %v", err)
	}
	for _, name := range requiredFiles {
		body, err := os.ReadFile(filepath.Join(source, name)) // #nosec G304 -- source is a test catalog under t.TempDir.
		if err != nil {
			t.Fatalf("read source %s: %v", name, err)
		}
		if name == "models.csv" {
			body = append(body, '\n')
		}
		if err := os.WriteFile(filepath.Join(revision, name), body, 0o600); err != nil { // #nosec G703 -- revision is under the hermetic test directory.
			t.Fatalf("write revision %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(installed, currentPointer), []byte("r-corrupt\n"), 0o600); err != nil {
		t.Fatalf("write corrupt pointer: %v", err)
	}
	if _, err := LoadInstalled(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt active revision error = %v, want checksum failure", err)
	}
}

func TestUpdateLocalPublishesRevisionAtomically(t *testing.T) {
	source := copyBundledCatalog(t)
	setManifestRevision(t, source, "local-revision-one")
	target := t.TempDir()

	first, err := Update(context.Background(), UpdateOptions{SourceDir: source, InstallDir: target})
	if err != nil {
		t.Fatalf("first local Update: %v", err)
	}
	if first.Source().Kind != "installed" || first.Revision() != "local-revision-one" {
		t.Fatalf("first source = %#v revision %q, want installed/local-revision-one", first.Source(), first.Revision())
	}
	firstPointer := activeRevision(t, target)

	secondSource := copyBundledCatalog(t)
	setManifestRevision(t, secondSource, "local-revision-two")
	second, err := Update(context.Background(), UpdateOptions{SourceDir: secondSource, InstallDir: target})
	if err != nil {
		t.Fatalf("second local Update: %v", err)
	}
	if second.Revision() != "local-revision-two" {
		t.Fatalf("second revision = %q, want local-revision-two", second.Revision())
	}
	secondPointer := activeRevision(t, target)
	if secondPointer == firstPointer {
		t.Fatalf("active pointer = %q after revision update, want a new immutable revision", secondPointer)
	}

	badSource := copyBundledCatalog(t)
	badManifest := readManifest(t, badSource)
	badManifest.Revision = "invalid-duplicate"
	modelsPath := filepath.Join(badSource, "models.csv")
	models, err := os.ReadFile(modelsPath) // #nosec G304 -- test path is under t.TempDir.
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(models), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatal("bundled models file has no data row")
	}
	lines = append(lines, lines[1])
	models = []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(modelsPath, models, 0o600); err != nil { // #nosec G703 -- test path is under t.TempDir.
		t.Fatalf("write duplicate models: %v", err)
	}
	badManifest.Files["models.csv"] = digest(models)
	writeManifest(t, badSource, badManifest)
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: badSource, InstallDir: target}); err == nil {
		t.Fatal("duplicate model update succeeded, want validation failure")
	}
	if got := activeRevision(t, target); got != secondPointer {
		t.Fatalf("active pointer after failed duplicate update = %q, want %q", got, secondPointer)
	}
	active, err := LoadPath(filepath.Join(target, secondPointer))
	if err != nil {
		t.Fatalf("load previous active revision: %v", err)
	}
	if active.Revision() != "local-revision-two" {
		t.Fatalf("previous active revision = %q, want local-revision-two", active.Revision())
	}

	referenceSource := copyBundledCatalog(t)
	referenceManifest := readManifest(t, referenceSource)
	referenceManifest.Revision = "invalid-reference"
	defaultsPath := filepath.Join(referenceSource, "defaults.csv")
	defaults, err := os.ReadFile(defaultsPath) // #nosec G304 -- test path is under t.TempDir.
	if err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	defaults = append(defaults, []byte("missing-runtime,small,claude-sonnet-5-5,low,,https://example.invalid,2026-10-02\n")...)
	if err := os.WriteFile(defaultsPath, defaults, 0o600); err != nil { // #nosec G703 -- test path is under t.TempDir.
		t.Fatalf("write invalid defaults: %v", err)
	}
	referenceManifest.Files["defaults.csv"] = digest(defaults)
	writeManifest(t, referenceSource, referenceManifest)
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: referenceSource, InstallDir: target}); err == nil {
		t.Fatal("unknown runtime reference update succeeded, want validation failure")
	}
	if got := activeRevision(t, target); got != secondPointer {
		t.Fatalf("active pointer after failed reference update = %q, want %q", got, secondPointer)
	}

	schemaSource := copyBundledCatalog(t)
	schemaManifest := readManifest(t, schemaSource)
	schemaManifest.SchemaVersion++
	writeManifest(t, schemaSource, schemaManifest)
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: schemaSource, InstallDir: target}); err == nil {
		t.Fatal("incompatible schema update succeeded, want validation failure")
	}
	if got := activeRevision(t, target); got != secondPointer {
		t.Fatalf("active pointer after failed schema update = %q, want %q", got, secondPointer)
	}

	activePricingPath := filepath.Join(target, secondPointer, "pricing.csv")
	activePricing, err := os.ReadFile(activePricingPath) // #nosec G304 -- test path is under t.TempDir.
	if err != nil {
		t.Fatalf("read active pricing: %v", err)
	}
	if err := os.WriteFile(activePricingPath, append(activePricing, []byte("corrupt\n")...), 0o600); err != nil { // #nosec G703 -- test path is under t.TempDir.
		t.Fatalf("corrupt active pricing: %v", err)
	}
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: secondSource, InstallDir: target}); err == nil || !strings.Contains(err.Error(), "does not match snapshot") {
		t.Fatalf("update over corrupt existing revision error = %v, want content mismatch", err)
	}
	if got := activeRevision(t, target); got != secondPointer {
		t.Fatalf("active pointer after corrupt existing revision = %q, want %q", got, secondPointer)
	}
}

func TestUpdateRejectsDefaultAboveRuntimeMaximumAndPreservesActiveRevision(t *testing.T) {
	source := copyBundledCatalog(t)
	setManifestRevision(t, source, "runtime-ceiling-baseline")
	target := t.TempDir()
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: source, InstallDir: target}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}
	wantPointer := activeRevision(t, target)

	invalid := copyBundledCatalog(t)
	setManifestRevision(t, invalid, "runtime-ceiling-invalid")
	runtimesPath := filepath.Join(invalid, "runtimes.csv")
	runtimes, err := os.ReadFile(runtimesPath) // #nosec G304 -- runtimesPath is under t.TempDir.
	if err != nil {
		t.Fatalf("read runtimes: %v", err)
	}
	old := "openai-api-key,openai,api_key,openai_api,openai_api,openai-api-key,OpenAI API,max,Y"
	updated := strings.Replace(string(runtimes), old, "openai-api-key,openai,api_key,openai_api,openai_api,openai-api-key,OpenAI API,high,Y", 1)
	if updated == string(runtimes) {
		t.Fatal("runtime fixture did not change maximum effort")
	}
	runtimes = []byte(updated)
	if err := os.WriteFile(runtimesPath, runtimes, 0o600); err != nil { // #nosec G703 -- runtimesPath is under t.TempDir.
		t.Fatalf("write runtimes: %v", err)
	}
	manifest := readManifest(t, invalid)
	manifest.Files["runtimes.csv"] = digest(runtimes)
	writeManifest(t, invalid, manifest)

	if _, err := Update(context.Background(), UpdateOptions{SourceDir: invalid, InstallDir: target}); err == nil || !strings.Contains(err.Error(), "exceeds runtime maximum") {
		t.Fatalf("inconsistent Update error = %v, want runtime ceiling rejection", err)
	}
	if got := activeRevision(t, target); got != wantPointer {
		t.Fatalf("active pointer after inconsistent update = %q, want %q", got, wantPointer)
	}
	active, err := LoadPath(filepath.Join(target, wantPointer))
	if err != nil {
		t.Fatalf("load previous active revision: %v", err)
	}
	if active.Revision() != "runtime-ceiling-baseline" {
		t.Fatalf("previous active revision = %q, want runtime-ceiling-baseline", active.Revision())
	}
}

func TestUpdateHTTPRequiresChecksumsAndPreservesActiveRevision(t *testing.T) {
	source := copyBundledCatalog(t)
	setManifestRevision(t, source, "http-revision-one")
	files := catalogFiles(t, source)
	target := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	first, err := Update(context.Background(), UpdateOptions{URL: server.URL, InstallDir: target})
	server.Close()
	if err != nil {
		t.Fatalf("HTTP Update: %v", err)
	}
	if first.Revision() != "http-revision-one" {
		t.Fatalf("HTTP revision = %q, want http-revision-one", first.Revision())
	}
	pointer := activeRevision(t, target)

	missingChecksum := copyBytes(files)
	manifest := readManifestBytes(t, missingChecksum["manifest.json"])
	delete(manifest.Files, "models.csv")
	missingChecksum["manifest.json"] = marshalManifest(t, manifest)
	missingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := missingChecksum[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	_, err = Update(context.Background(), UpdateOptions{URL: missingServer.URL, InstallDir: target})
	missingServer.Close()
	if err == nil || !strings.Contains(err.Error(), "must checksum models.csv") {
		t.Fatalf("missing checksum Update error = %v, want checksum rejection", err)
	}
	if got := activeRevision(t, target); got != pointer {
		t.Fatalf("active pointer after missing checksum = %q, want %q", got, pointer)
	}

	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == "pricing.csv" {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		body := files[filepath.Base(r.URL.Path)]
		_, _ = w.Write(body)
	}))
	_, err = Update(context.Background(), UpdateOptions{URL: failingServer.URL, InstallDir: target})
	failingServer.Close()
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("download failure Update error = %v, want HTTP failure", err)
	}
	if got := activeRevision(t, target); got != pointer {
		t.Fatalf("active pointer after download failure = %q, want %q", got, pointer)
	}
}

func copyBundledCatalog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range requiredFiles {
		body, err := bundledFS.ReadFile(filepath.Join("data", name))
		if err != nil {
			t.Fatalf("read bundled %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil { // #nosec G703 -- test path is under t.TempDir.
			t.Fatalf("write catalog %s: %v", name, err)
		}
	}
	return dir
}

func catalogFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(requiredFiles))
	for _, name := range requiredFiles {
		body, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- dir is a test catalog under t.TempDir.
		if err != nil {
			t.Fatalf("read catalog %s: %v", name, err)
		}
		files[name] = body
	}
	return files
}

func copyBytes(input map[string][]byte) map[string][]byte {
	output := make(map[string][]byte, len(input))
	for name, body := range input {
		output[name] = append([]byte(nil), body...)
	}
	return output
}

func readManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json")) // #nosec G304 -- test path is under t.TempDir.
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return readManifestBytes(t, body)
}

func readManifestBytes(t *testing.T, body []byte) Manifest {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

func writeManifest(t *testing.T, dir string, manifest Manifest) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), marshalManifest(t, manifest), 0o600); err != nil { // #nosec G703 -- test path is under t.TempDir.
		t.Fatalf("write manifest: %v", err)
	}
}

func marshalManifest(t *testing.T, manifest Manifest) []byte {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return append(body, '\n')
}

func setManifestRevision(t *testing.T, dir, revision string) {
	t.Helper()
	manifest := readManifest(t, dir)
	manifest.Revision = revision
	writeManifest(t, dir, manifest)
}

func appendModel(t *testing.T, dir, modelID, fast, ultrafast string) {
	t.Helper()
	path := filepath.Join(dir, "models.csv")
	body, err := os.ReadFile(path) // #nosec G304 -- path is a test catalog under t.TempDir.
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	body = append(body, []byte(fmt.Sprintf("openai-api-key,%s,low,%s,%s,,https://example.invalid,2026-10-03\n", modelID, fast, ultrafast))...)
	if err := os.WriteFile(path, body, 0o600); err != nil { // #nosec G703 -- path is a test catalog under t.TempDir.
		t.Fatalf("write models: %v", err)
	}
	manifest := readManifest(t, dir)
	manifest.Files["models.csv"] = digest(body)
	writeManifest(t, dir, manifest)
}

func activeRevision(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, currentPointer)) // #nosec G304 -- test path is under t.TempDir.
	if err != nil {
		t.Fatalf("read active pointer: %v", err)
	}
	return strings.TrimSpace(string(body))
}

func digest(body []byte) string {
	return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%x", sha256.Sum256(body))))
}
