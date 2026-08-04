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
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestDataShowJSONEmptyStore(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustDefaultLayoutNoCreate(t)
	assertDataStateAbsent(t, layout)
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
	assertDataStateAbsent(t, layout)
}

func TestDataShowTextEmptyStoreDoesNotCreateState(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustDefaultLayoutNoCreate(t)
	assertDataStateAbsent(t, layout)
	var stdout, stderr bytes.Buffer

	err := runDataCommand(&stdout, &stderr, "data", "show")
	if err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "Run count: 0") || !strings.Contains(text, "Orphans: 0") {
		t.Fatalf("stdout = %q, want empty data summary", text)
	}
	assertDataStateAbsent(t, layout)
}

func TestDataPruneEmptyDoesNotCreateState(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantText   string
		wantDryRun bool
	}{
		{name: "dry-run json", args: []string{"data", "prune", "--dry-run", "--json"}, wantDryRun: true},
		{name: "real json", args: []string{"data", "prune", "--json"}},
		{name: "dry-run text", args: []string{"data", "prune", "--dry-run"}, wantText: "Would delete runs: 0", wantDryRun: true},
		{name: "real text", args: []string{"data", "prune"}, wantText: "Deleted runs: 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statedirtest.Hermetic(t)
			layout := mustDefaultLayoutNoCreate(t)
			assertDataStateAbsent(t, layout)
			var stdout, stderr bytes.Buffer

			if err := runDataCommand(&stdout, &stderr, tt.args...); err != nil {
				t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if tt.wantText != "" {
				if text := stdout.String(); !strings.Contains(text, tt.wantText) {
					t.Fatalf("stdout = %q, want %q", text, tt.wantText)
				}
			} else {
				var decoded view.DataPrune
				if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
					t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
				}
				if decoded.DryRun != tt.wantDryRun || len(decoded.SelectedRuns) != 0 || len(decoded.DeletedRuns) != 0 || len(decoded.OrphansRemoved) != 0 || len(decoded.Warnings) != 0 {
					t.Fatalf("decoded = %#v, want empty prune result without warnings", decoded)
				}
			}
			assertDataStateAbsent(t, layout)
		})
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

func TestDataReadOnlyDoesNotBlockLegacyMigration(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	legacyRoot := filepath.Join(layout.DataRoot, statepaths.AppDir)
	legacyLayout := statepaths.NewLayout(legacyRoot, layout.CacheRoot)
	store := openLedgerForTest(t, legacyLayout)
	allocateRun(t, store, legacyLayout, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("Close legacy store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runDataCommand(&stdout, &stderr, "data", "show", "--json"); err != nil {
		t.Fatalf("data show: %v; stderr = %q", err, stderr.String())
	}
	var shown view.DataShow
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("Unmarshal show: %v; stdout = %q", err, stdout.String())
	}
	if shown.RunCount != 0 {
		t.Fatalf("show run count = %d, want empty before mutating migration", shown.RunCount)
	}
	if _, err := os.Stat(layout.LedgerDB()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new ledger stat err = %v, want missing after read-only show", err)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "ledger.db")); err != nil {
		t.Fatalf("legacy ledger stat err = %v, want still present after read-only show", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runDataCommand(&stdout, &stderr, "data", "prune", "--dry-run", "--json"); err != nil {
		t.Fatalf("data prune dry-run: %v; stderr = %q", err, stderr.String())
	}
	var dryRun view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &dryRun); err != nil {
		t.Fatalf("Unmarshal prune dry-run: %v; stdout = %q", err, stdout.String())
	}
	if len(dryRun.SelectedRuns) != 0 || len(dryRun.Warnings) != 1 || !strings.Contains(dryRun.Warnings[0], "dry-run does not migrate") {
		t.Fatalf("dry-run prune = %#v, want empty preview with legacy migration warning", dryRun)
	}
	if _, err := os.Stat(layout.LedgerDB()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new ledger stat err = %v, want still missing after dry-run prune", err)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "ledger.db")); err != nil {
		t.Fatalf("legacy ledger stat err = %v, want still present after dry-run prune", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runDataCommand(&stdout, &stderr, "data", "prune", "--older-than", "1h", "--json"); err != nil {
		t.Fatalf("data prune: %v; stderr = %q", err, stderr.String())
	}
	var pruned view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &pruned); err != nil {
		t.Fatalf("Unmarshal prune: %v; stdout = %q", err, stdout.String())
	}
	if len(pruned.DeletedRuns) != 1 || pruned.DeletedRuns[0].RunID != "old-live" {
		t.Fatalf("deleted runs = %#v, want migrated old-live deletion", pruned.DeletedRuns)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "ledger.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy ledger stat err = %v, want migrated away", err)
	}
}

func TestDataPruneDryRunWarnsForStagedLegacyMigration(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	writeDataCommandFile(t, filepath.Join(statepaths.LegacyDataRoot(layout)+".migrating", "ledger.db"), "staged")
	var stdout, stderr bytes.Buffer

	if err := runDataCommand(&stdout, &stderr, "data", "prune", "--dry-run", "--json"); err != nil {
		t.Fatalf("data prune dry-run: %v; stderr = %q", err, stderr.String())
	}
	var dryRun view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &dryRun); err != nil {
		t.Fatalf("Unmarshal prune dry-run: %v; stdout = %q", err, stdout.String())
	}
	if len(dryRun.Warnings) != 1 || !strings.Contains(dryRun.Warnings[0], "dry-run does not migrate") {
		t.Fatalf("warnings = %#v, want staged legacy migration warning", dryRun.Warnings)
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
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:       "github.com",
					AuthMode:   config.GitAuthModePAT,
					Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/home"},
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
	now := time.Now().UTC()
	allocateRun(t, store, layout, "live-31d", ledger.PostModeLive, now.Add(-31*24*time.Hour))
	allocateRun(t, store, layout, "live-91d", ledger.PostModeLive, now.Add(-91*24*time.Hour))
	allocateRun(t, store, layout, "dry-8d", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
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

func TestDataPruneRemovesDossierAndWorkbenchArtifacts(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	store := openLedgerForTest(t, layout)
	run := allocateRun(t, store, layout, "live-old", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "dossier", "final", "repo-guidance.md"), "guidance")
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "workbench", "repo", "main.go"), "package main\n")
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "workbench", "scratch", "notes.txt"), "scratch")

	var stdout, stderr bytes.Buffer
	if err := runDataCommand(&stdout, &stderr, "data", "prune", "--older-than", "1h", "--json"); err != nil {
		t.Fatalf("data prune: %v; stderr = %q", err, stderr.String())
	}
	var pruned view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &pruned); err != nil {
		t.Fatalf("Unmarshal prune: %v; stdout = %q", err, stdout.String())
	}
	if len(pruned.DeletedRuns) != 1 || pruned.DeletedRuns[0].RunID != "live-old" {
		t.Fatalf("deleted runs = %#v, want live-old deletion", pruned.DeletedRuns)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactPath, "dossier")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dossier stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactPath, "workbench")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workbench stat err = %v, want removed", err)
	}
	if _, err := os.Stat(run.ArtifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact root stat err = %v, want removed", err)
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

func TestDataPurgeRemovesDossierAndWorkbenchArtifacts(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := mustLayout(t)
	store := openLedgerForTest(t, layout)
	run := allocateRun(t, store, layout, "live-old", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "dossier", "final", "repo-guidance.md"), "guidance")
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "workbench", "repo", "main.go"), "package main\n")
	writeDataCommandFile(t, filepath.Join(run.ArtifactPath, "workbench", "scratch", "notes.txt"), "scratch")

	var stdout, stderr bytes.Buffer
	if err := runDataCommand(&stdout, &stderr, "data", "purge", "--yes"); err != nil {
		t.Fatalf("data purge: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Purged data root:") {
		t.Fatalf("stdout = %q, want purge summary", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactPath, "dossier")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dossier stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactPath, "workbench")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workbench stat err = %v, want removed", err)
	}
	if _, err := os.Stat(layout.DataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data root stat err = %v, want removed", err)
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

func TestDataPruneProgressWritesToStderr(t *testing.T) {
	statedirtest.Hermetic(t)
	seedRun(t, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	var stdout, stderr bytes.Buffer

	if err := runDataCommandWithQuiet(&stdout, &stderr, false, "data", "prune", "--dry-run", "--json"); err != nil {
		t.Fatalf("runDataCommand: %v; stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), `command="data.prune" op="resolve_layout"`) {
		t.Fatalf("stderr = %q, want resolve_layout progress", stderr.String())
	}
	if !strings.Contains(stderr.String(), `command="data.prune" op="find_orphans"`) {
		t.Fatalf("stderr = %q, want find_orphans progress", stderr.String())
	}
	var decoded view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
}

func runDataCommand(stdout, stderr *bytes.Buffer, args ...string) error {
	return runDataCommandWithQuiet(stdout, stderr, true, args...)
}

func runDataCommandWithQuiet(stdout, stderr *bytes.Buffer, quiet bool, args ...string) error {
	cmd, opts := root.NewCommandWithOptions(&root.Options{Stdout: stdout, Stderr: stderr, Quiet: quiet})
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

func mustDefaultLayoutNoCreate(t *testing.T) statepaths.Layout {
	t.Helper()
	layout, err := statepaths.DefaultLayout()
	if err != nil {
		t.Fatalf("DefaultLayout: %v", err)
	}
	return layout
}

func assertDataStateAbsent(t *testing.T, layout statepaths.Layout) {
	t.Helper()
	for _, path := range []string{layout.DataRoot, layout.CacheRoot, layout.LedgerDB()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state path %s stat err = %v, want missing", path, err)
		}
	}
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

func writeDataCommandFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
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

// The command-level enforcement: a live prune or purge refuses while any run
// holds the active-runs lock, deletes nothing, and succeeds normally once
// the lock is released.
func TestDataPruneAndPurgeRefuseWhileActiveRunsLockHeld(t *testing.T) {
	statedirtest.Hermetic(t)
	layout := seedRun(t, "old-live", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	held, err := runlock.AcquireShared(layout.ActiveRunsLock())
	if err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = runDataCommand(&stdout, &stderr, "data", "prune", "--older-than", "24h")
	if err == nil || !strings.Contains(err.Error(), "another cr instance appears to be running") {
		t.Fatalf("prune while lock held: err = %v, want refusal", err)
	}
	err = runDataCommand(&stdout, &stderr, "data", "purge", "--yes")
	if err == nil || !strings.Contains(err.Error(), "another cr instance appears to be running") {
		t.Fatalf("purge while lock held: err = %v, want refusal", err)
	}
	store := openLedgerForTest(t, layout)
	if _, err := store.GetRun(context.Background(), "old-live"); err != nil {
		t.Fatalf("run deleted by refused command: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := runDataCommand(&stdout, &stderr, "data", "prune", "--older-than", "24h", "--json"); err != nil {
		t.Fatalf("prune after release: %v; stderr = %q", err, stderr.String())
	}
	var decoded view.DataPrune
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v; stdout = %q", err, stdout.String())
	}
	if len(decoded.DeletedRuns) != 1 || decoded.DeletedRuns[0].RunID != "old-live" {
		t.Fatalf("decoded = %#v, want old-live deleted after lock release", decoded)
	}
}
