package datacmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/statedirtest"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestDataShowJSONEmptyStore(t *testing.T) {
	statedirtest.Hermetic(t)
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "show", "--json")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	var decoded view.DataShow
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
	if decoded.RunCount != 0 || decoded.DataRoot == "" || decoded.LedgerPath == "" || decoded.RunsRoot == "" {
		t.Fatalf("decoded = %#v, want empty store paths and zero runs", decoded)
	}
}

func TestDataPruneDryRunDoesNotDelete(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := seedRun(t, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "prune", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	var decoded view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
	if !decoded.DryRun || len(decoded.SelectedRuns) != 1 || decoded.SelectedRuns[0].RunID != "old-live" || len(decoded.DeletedRuns) != 0 {
		t.Fatalf("decoded = %#v, want one selected dry-run deletion", decoded)
	}
	store := openLedgerForTest(t, layout)
	defer store.Close()
	if _, err := store.GetRun(context.Background(), "old-live"); err != nil {
		t.Fatalf("GetRun after dry-run: %v", err)
	}
}

func TestDataPruneDefaultIgnoresConfiguredRetention(t *testing.T) {
	statedirtest.Hermetic(t)
	maxAgeDays := 30
	configPath, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if err := config.Save(configPath, config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
		Data: config.DataConfig{
			Retention: config.RetentionConfig{
				MaxAgeDays:  &maxAgeDays,
				Enforcement: config.RetentionManualOnly,
			},
		},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	layout := mustLayout(t)
	store := openLedgerForTest(t, layout)
	allocateRun(t, store, layout, "live-31d", ledger.PostModeLive, testNow().Add(-31*24*time.Hour))
	allocateRun(t, store, layout, "live-91d", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	allocateRun(t, store, layout, "dry-8d", ledger.PostModeDryRun, testNow().Add(-8*24*time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout, stderr bytes.Buffer

	err = runDataCommand(&stdout, &stderr, "data", "prune", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	var decoded view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
	if got, want := dataPruneRunIDs(decoded.SelectedRuns), []string{"dry-8d", "live-91d"}; !equalStrings(got, want) {
		t.Fatalf("selected run IDs = %#v, want %#v", got, want)
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
	lines := strings.Split(stdout.String(), "\n")
	if len(lines) < 2 || lines[0] != "Deleted runs: 2" || lines[1] != "Orphans removed: 0" {
		t.Fatalf("stdout lines = %#v, want exact prune summary", lines)
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

	err := runDataCommand(&stdout, &stderr, "data", "purge", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	var decoded view.DataPurge
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
	if !decoded.DryRun || decoded.Removed || decoded.DataRoot != layout.DataRoot {
		t.Fatalf("decoded = %#v, want dry-run purge without removal", decoded)
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

func dataPruneRunIDs(runs []view.DataRunItem) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.RunID)
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func testNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}
