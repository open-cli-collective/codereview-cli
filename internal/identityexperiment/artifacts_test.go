package identityexperiment

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestReserveOutputRefusesExistingDestinationsWithoutMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if publisher, err := ReserveOutput(existing, FileDependencies{}); publisher != nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination reserve = publisher:%#v err:%v", publisher, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" { // #nosec G304 -- marker is under t.TempDir.
		t.Fatalf("existing content changed: data=%q err=%v", data, err)
	}

	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if publisher, err := ReserveOutput(link, FileDependencies{}); publisher != nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink destination reserve = publisher:%#v err:%v", publisher, err)
	}
}

func TestReserveOutputFaultsAreClassifiedAndChmodCleansItsEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	inspectionPath := filepath.Join(root, "inspection")
	if publisher, err := ReserveOutput(inspectionPath, FileDependencies{
		Lstat: func(string) (os.FileInfo, error) { return nil, fs.ErrPermission },
	}); publisher != nil || !IsOutputInspectionError(err) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("inspection failure reserve = publisher:%#v err:%v", publisher, err)
	}

	mkdirPath := filepath.Join(root, "mkdir")
	if publisher, err := ReserveOutput(mkdirPath, FileDependencies{
		Mkdir: func(string, os.FileMode) error { return fs.ErrPermission },
	}); publisher != nil || err == nil || IsOutputInspectionError(err) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("mkdir failure reserve = publisher:%#v err:%v", publisher, err)
	}
	if _, err := os.Lstat(mkdirPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed mkdir left destination behind: %v", err)
	}

	chmodPath := filepath.Join(root, "chmod")
	if publisher, err := ReserveOutput(chmodPath, FileDependencies{
		Chmod: func(string, os.FileMode) error { return fs.ErrPermission },
	}); publisher != nil || err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("chmod failure reserve = publisher:%#v err:%v", publisher, err)
	}
	if _, err := os.Lstat(chmodPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("chmod failure did not clean its empty directory: %v", err)
	}
}

func TestPublisherWritesPrivateFilesAndManifestLastAndIsSingleUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only permissions differ on Windows")
	}
	root := t.TempDir()
	outputDir := filepath.Join(root, "artifacts")
	var published []string
	publisher, err := ReserveOutput(outputDir, FileDependencies{Rename: func(oldPath, newPath string) error {
		published = append(published, filepath.Base(newPath))
		return os.Rename(oldPath, newPath)
	}})
	if err != nil {
		t.Fatalf("ReserveOutput: %v", err)
	}
	result := executeOnePair(t)
	manifest, err := publisher.Publish(result)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"}; !reflect.DeepEqual(published, want) {
		t.Fatalf("publish order = %v, want %v", published, want)
	}
	assertMode(t, outputDir, 0o700)
	for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
		assertMode(t, filepath.Join(outputDir, name), 0o600)
	}
	fixture, err := os.ReadFile(filepath.Join(outputDir, "fixture.json")) // #nosec G304 -- outputDir is under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	replay, err := os.ReadFile(filepath.Join(outputDir, "replay.json")) // #nosec G304 -- outputDir is under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := os.ReadFile(filepath.Join(outputDir, "receipts.json")) // #nosec G304 -- outputDir is under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.Files.FixtureSHA256 != SHA256(fixture) ||
		manifest.Files.ReplaySHA256 != SHA256(replay) || manifest.Files.ReceiptsSHA256 != SHA256(receipts) {
		t.Fatalf("manifest hashes = %#v", manifest)
	}
	if _, err := publisher.Publish(result); err == nil {
		t.Fatal("reused publisher succeeded")
	}
	if len(published) != 4 {
		t.Fatalf("reused publisher wrote again: %v", published)
	}
}

func TestPostExecutionManifestRenameFailureRetainsAttemptAccounting(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "artifacts")
	publisher, err := ReserveOutput(outputDir, FileDependencies{Rename: func(oldPath, newPath string) error {
		if filepath.Base(newPath) == "manifest.json" {
			return errors.New("synthetic rename failure")
		}
		return os.Rename(oldPath, newPath)
	}})
	if err != nil {
		t.Fatalf("ReserveOutput: %v", err)
	}
	result := executeOnePair(t)
	if result.Receipts.AttemptedPairs != 1 || result.Receipts.ProviderCalls != 1 || result.Receipts.CompletedPairs != 1 {
		t.Fatalf("pre-publication accounting = %#v", result.Receipts)
	}
	if _, err := publisher.Publish(result); err == nil {
		t.Fatal("manifest rename failure was not returned")
	}
	if _, err := os.Stat(filepath.Join(outputDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists after failed rename or stat err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, "receipts.json")) // #nosec G304 -- outputDir is under t.TempDir.
	if err != nil {
		t.Fatalf("receipts not retained before manifest failure: %v", err)
	}
	var receipts Receipts
	if err := json.Unmarshal(data, &receipts); err != nil {
		t.Fatalf("decode retained receipts: %v", err)
	}
	if receipts.AttemptedPairs != 1 || receipts.ProviderCalls != 1 || receipts.CompletedPairs != 1 {
		t.Fatalf("retained accounting changed: %#v", receipts)
	}
}

func executeOnePair(t *testing.T) Result {
	t.Helper()
	decider := &testDecider{}
	backend := runnerTestBackend(decider)
	prepared, err := Prepare(fixtureBytes(t, 2), testConfiguration(), backend)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	result := Execute(context.Background(), prepared, testDependencies(), backend)
	if result.Err() != nil {
		t.Fatalf("Execute: %v", result.Err())
	}
	return result
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}
