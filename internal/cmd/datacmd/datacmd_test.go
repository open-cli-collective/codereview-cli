package datacmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/statedirtest"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestDataShowJSONEmptyStore(t *testing.T) {
	statedirtest.Hermetic(t)
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "show", "--json")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"run_count": 0`) {
		t.Fatalf("stdout = %q, want run_count 0", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"data_root":`) {
		t.Fatalf("stdout = %q, want data_root", stdout.String())
	}
}

func TestDataPruneDryRunDoesNotDelete(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := seedRun(t, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "prune", "--dry-run")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Would delete runs: 1") {
		t.Fatalf("stdout = %q, want dry-run selected count", stdout.String())
	}
	store := openLedgerForTest(t, layout)
	defer store.Close()
	if _, err := store.GetRun(context.Background(), "old-live"); err != nil {
		t.Fatalf("GetRun after dry-run: %v", err)
	}
}

func TestDataPruneKeepLastPerPostMode(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	store := openLedgerForTest(t, layout)
	allocateRun(t, store, layout, "live-new", ledger.PostModeLive, testNow())
	allocateRun(t, store, layout, "live-old", ledger.PostModeLive, testNow().Add(-time.Hour))
	allocateRun(t, store, layout, "dry-new", ledger.PostModeDryRun, testNow().Add(-2*time.Hour))
	allocateRun(t, store, layout, "dry-old", ledger.PostModeDryRun, testNow().Add(-3*time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "prune", "--keep-last", "1")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Deleted runs: 2") {
		t.Fatalf("stdout = %q, want two deleted runs", stdout.String())
	}
	store = openLedgerForTest(t, layout)
	defer store.Close()
	for _, runID := range []string{"live-new", "dry-new"} {
		if _, err := store.GetRun(context.Background(), runID); err != nil {
			t.Fatalf("GetRun(%s): %v", runID, err)
		}
	}
	for _, runID := range []string{"live-old", "dry-old"} {
		if _, err := store.GetRun(context.Background(), runID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("GetRun(%s) error = %v, want ErrNotFound", runID, err)
		}
	}
}

func TestDataPruneRejectsInvalidFlagCombinations(t *testing.T) {
	statedirtest.Hermetic(t)
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "mutually exclusive", args: []string{"data", "prune", "--older-than", "1h", "--keep-last", "1"}, wantErr: "mutually exclusive"},
		{name: "zero older-than", args: []string{"data", "prune", "--older-than", "0s"}, wantErr: "must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runDataCommand(&stdout, &stderr, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("runDataCommand error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDataPurgeWorksWithCorruptDB(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	if err := os.MkdirAll(layout.DataRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(layout.LedgerDB(), []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "purge", "--yes")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Purged data root:") {
		t.Fatalf("stdout = %q, want purge summary", stdout.String())
	}
	if _, err := os.Stat(layout.DataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data root stat error = %v, want not exist", err)
	}
}

func TestDataPurgeDryRunDoesNotRequireConfirmationOrDelete(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	if err := os.WriteFile(layout.LedgerDB(), []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "purge", "--dry-run")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Would purge data root:") {
		t.Fatalf("stdout = %q, want dry-run purge summary", stdout.String())
	}
	if _, err := os.Stat(layout.DataRoot); err != nil {
		t.Fatalf("data root stat: %v", err)
	}
	if _, err := os.Stat(layout.LedgerDB()); err != nil {
		t.Fatalf("ledger stat: %v", err)
	}
}

func TestDataPurgeRequiresConfirmation(t *testing.T) {
	statedirtest.Hermetic(t)
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "purge")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("runDataCommand error = %v, want --yes guidance", err)
	}
}

func runDataCommand(stdout, stderr *bytes.Buffer, args ...string) error {
	cmd, opts := root.NewCommandWithOptions(&root.Options{Stdout: stdout, Stderr: stderr})
	Register(cmd, opts)
	return root.Execute(cmd, args)
}

func seedRun(t *testing.T, runID string, mode ledger.PostMode, started time.Time) statepaths.Layout {
	t.Helper()
	layout := mustLayout(t)
	store := openLedgerForTest(t, layout)
	defer store.Close()
	allocateRun(t, store, layout, runID, mode, started)
	return layout
}

func mustLayout(t *testing.T) statepaths.Layout {
	t.Helper()
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	return layout
}

func openLedgerForTest(t *testing.T, layout statepaths.Layout) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), layout.LedgerDB())
	if err != nil {
		t.Fatalf("ledger Open: %v", err)
	}
	return store
}

func allocateRun(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	path := filepath.Join(layout.DataRoot, "runs", "github_owner_repo_1", strings.Repeat("a", 40), strings.Repeat("b", 40), "default__reviewer", runID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll artifact: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           "github_owner_repo_1",
		PRURL:           "https://github.com/owner/repo/pull/1",
		RunID:           runID,
		SHA:             strings.Repeat("a", 40),
		BaseSHA:         strings.Repeat("b", 40),
		Profile:         "default",
		PostingIdentity: "reviewer",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    path,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func testNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}
