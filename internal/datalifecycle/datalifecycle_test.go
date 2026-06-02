package datalifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestPruneDefaultRetentionSelectsLiveAndDryRunWindows(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	store := &fakeStore{runs: []ledger.Run{
		testRun(layout, "live-old", ledger.PostModeLive, now.Add(-91*24*time.Hour)),
		testRun(layout, "live-new", ledger.PostModeLive, now.Add(-89*24*time.Hour)),
		testRun(layout, "dry-old", ledger.PostModeDryRun, now.Add(-8*24*time.Hour)),
		testRun(layout, "dry-new", ledger.PostModeDryRun, now.Add(-6*24*time.Hour)),
	}}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: func() time.Time { return now }}, PruneOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, want := runItemIDs(result.SelectedRuns), []string{"live-old", "dry-old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected runs = %#v, want %#v", got, want)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted = %#v, want none for dry-run", store.deleted)
	}
}

func TestPruneConfiguredRetentionSelectsLiveWindowAndDefaultDryRunWindow(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	store := &fakeStore{runs: []ledger.Run{
		testRun(layout, "live-old", ledger.PostModeLive, now.Add(-31*24*time.Hour)),
		testRun(layout, "live-new", ledger.PostModeLive, now.Add(-29*24*time.Hour)),
		testRun(layout, "dry-old", ledger.PostModeDryRun, now.Add(-8*24*time.Hour)),
		testRun(layout, "dry-new", ledger.PostModeDryRun, now.Add(-6*24*time.Hour)),
	}}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: func() time.Time { return now }}, PruneOptions{
		DryRun:    true,
		Retention: RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour},
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, want := runItemIDs(result.SelectedRuns), []string{"live-old", "dry-old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected runs = %#v, want %#v", got, want)
	}
}

func TestPruneConfiguredRetentionCanKeepLiveForever(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	store := &fakeStore{runs: []ledger.Run{
		testRun(layout, "live-old", ledger.PostModeLive, now.Add(-365*24*time.Hour)),
		testRun(layout, "dry-old", ledger.PostModeDryRun, now.Add(-8*24*time.Hour)),
		testRun(layout, "dry-new", ledger.PostModeDryRun, now.Add(-6*24*time.Hour)),
	}}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: func() time.Time { return now }}, PruneOptions{
		DryRun:    true,
		Retention: RetentionPolicy{LiveForever: true},
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, want := runItemIDs(result.SelectedRuns), []string{"dry-old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected runs = %#v, want %#v", got, want)
	}
}

func TestPruneExplicitOlderThanOverridesConfiguredRetentionPolicy(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	store := &fakeStore{runs: []ledger.Run{
		testRun(layout, "live-old", ledger.PostModeLive, now.Add(-31*24*time.Hour)),
		testRun(layout, "live-new", ledger.PostModeLive, now.Add(-29*24*time.Hour)),
		testRun(layout, "dry-old", ledger.PostModeDryRun, now.Add(-31*24*time.Hour)),
	}}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: func() time.Time { return now }}, PruneOptions{
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Retention: RetentionPolicy{LiveForever: true},
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, want := runItemIDs(result.SelectedRuns), []string{"live-old", "dry-old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected runs = %#v, want %#v", got, want)
	}
}

func TestPruneKeepLastPreservesNewestPerPostMode(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	store := &fakeStore{runs: []ledger.Run{
		testRun(layout, "live-3", ledger.PostModeLive, now.Add(-time.Minute)),
		testRun(layout, "dry-3", ledger.PostModeDryRun, now.Add(-2*time.Minute)),
		testRun(layout, "live-2", ledger.PostModeLive, now.Add(-3*time.Minute)),
		testRun(layout, "dry-2", ledger.PostModeDryRun, now.Add(-4*time.Minute)),
		testRun(layout, "live-1", ledger.PostModeLive, now.Add(-5*time.Minute)),
		testRun(layout, "dry-1", ledger.PostModeDryRun, now.Add(-6*time.Minute)),
	}}
	keep := 1

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: func() time.Time { return now }}, PruneOptions{KeepLast: &keep, DryRun: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, want := runItemIDs(result.SelectedRuns), []string{"live-2", "dry-2", "live-1", "dry-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected runs = %#v, want %#v", got, want)
	}
}

func TestPruneDeletesRowBeforeBestEffortArtifactRemoval(t *testing.T) {
	layout := testLayout(t)
	run := testRun(layout, "live-old", ledger.PostModeLive, testNow().Add(-91*24*time.Hour))
	if err := os.MkdirAll(run.ArtifactPath, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	store := &fakeStore{runs: []ledger.Run{run}}
	removeErr := errors.New("remove failed")

	result, err := Prune(context.Background(), Options{
		Layout: layout,
		Store:  store,
		Now:    testNow,
		RemoveAll: func(_ string) error {
			if _, exists := store.deleted[run.RunID]; !exists {
				t.Fatalf("RemoveAll called before DeleteRun")
			}
			return removeErr
		},
	}, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := store.Get(run.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("store Get after prune error = %v, want ErrNotFound", err)
	}
	if len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, "\n"), "remove failed") {
		t.Fatalf("warnings = %#v, want remove failure", result.Warnings)
	}
}

func TestPruneSkipsUnsafeArtifactPathsAfterDeletingRows(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	runs := []ledger.Run{
		{RunID: "outside", PostMode: ledger.PostModeLive, StartedAt: now.Add(-91 * 24 * time.Hour), ArtifactPath: filepath.Join(t.TempDir(), "outside")},
		{RunID: "data-root", PostMode: ledger.PostModeLive, StartedAt: now.Add(-91 * 24 * time.Hour), ArtifactPath: layout.DataRoot},
		{RunID: "runs-root", PostMode: ledger.PostModeLive, StartedAt: now.Add(-91 * 24 * time.Hour), ArtifactPath: filepath.Join(layout.DataRoot, "runs")},
		{RunID: "parent", PostMode: ledger.PostModeLive, StartedAt: now.Add(-91 * 24 * time.Hour), ArtifactPath: filepath.Join(layout.DataRoot, "runs", "github_owner_repo_1")},
	}
	store := &fakeStore{runs: runs}
	var removed []string

	result, err := Prune(context.Background(), Options{
		Layout: layout,
		Store:  store,
		Now:    func() time.Time { return now },
		RemoveAll: func(path string) error {
			removed = append(removed, path)
			return nil
		},
	}, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(store.deleted) != len(runs) {
		t.Fatalf("deleted = %#v, want all rows deleted", store.deleted)
	}
	if len(removed) != 0 {
		t.Fatalf("removed paths = %#v, want none", removed)
	}
	if len(result.Warnings) != len(runs) {
		t.Fatalf("warnings = %#v, want one per unsafe path", result.Warnings)
	}
}

func TestPruneSkipsArtifactPathWithSymlinkedAncestor(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll outside: %v", err)
	}
	link := filepath.Join(layout.DataRoot, "runs", "link-pr")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	run := ledger.Run{
		RunID:        "symlinked",
		PostMode:     ledger.PostModeLive,
		StartedAt:    now.Add(-91 * 24 * time.Hour),
		ArtifactPath: filepath.Join(link, strings.Repeat("a", 40), strings.Repeat("b", 40), "default__reviewer", "001"),
	}
	store := &fakeStore{runs: []ledger.Run{run}}
	var removed []string

	result, err := Prune(context.Background(), Options{
		Layout: layout,
		Store:  store,
		Now:    func() time.Time { return now },
		RemoveAll: func(path string) error {
			removed = append(removed, path)
			return nil
		},
	}, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := store.Get(run.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("Get after prune error = %v, want ErrNotFound", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed paths = %#v, want none for symlinked ancestor", removed)
	}
	if len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, "\n"), "symlink") {
		t.Fatalf("warnings = %#v, want symlink warning", result.Warnings)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside stat: %v", err)
	}
}

func TestPruneSweepsOrphansAndPreservesReferencedArtifacts(t *testing.T) {
	layout := testLayout(t)
	ref := testRun(layout, "referenced", ledger.PostModeLive, testNow())
	orphan := testRun(layout, "orphan", ledger.PostModeLive, testNow())
	writeFile(t, filepath.Join(ref.ArtifactPath, "rollup.md"), "keep")
	writeFile(t, filepath.Join(orphan.ArtifactPath, "rollup.md"), "remove")
	store := &fakeStore{runs: []ledger.Run{ref}}
	keep := 10

	result, err := Prune(context.Background(), Options{Layout: layout, Store: store, Now: testNow}, PruneOptions{KeepLast: &keep})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(result.DeletedRuns) != 0 {
		t.Fatalf("DeletedRuns = %#v, want none", result.DeletedRuns)
	}
	if got, want := orphanPaths(result.OrphansRemoved), []string{orphan.ArtifactPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans removed = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(ref.ArtifactPath); err != nil {
		t.Fatalf("referenced artifact stat: %v", err)
	}
	if _, err := os.Stat(orphan.ArtifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan artifact stat error = %v, want not exist", err)
	}
}

func TestShowSummarizesRunsAndOrphans(t *testing.T) {
	layout := testLayout(t)
	now := testNow()
	live := testRun(layout, "live", ledger.PostModeLive, now.Add(-time.Hour))
	outcome := ledger.OutcomeComment
	live.Outcome = &outcome
	dry := testRun(layout, "dry", ledger.PostModeDryRun, now)
	orphan := testRun(layout, "orphan", ledger.PostModeDryRun, now)
	writeFile(t, filepath.Join(live.ArtifactPath, "rollup.md"), "12345")
	writeFile(t, filepath.Join(orphan.ArtifactPath, "rollup.md"), "123")
	store := &fakeStore{runs: []ledger.Run{dry, live}}

	stats, err := Show(context.Background(), Options{Layout: layout, Store: store})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if stats.RunCount != 2 || stats.LiveRuns != 1 || stats.DryRunRuns != 1 {
		t.Fatalf("stats counts = runs:%d live:%d dry:%d", stats.RunCount, stats.LiveRuns, stats.DryRunRuns)
	}
	if stats.OutcomeCounts[ledger.OutcomeComment] != 1 {
		t.Fatalf("OutcomeCounts = %#v, want comment count", stats.OutcomeCounts)
	}
	if stats.OrphanCount != 1 || stats.OrphanBytes != 3 || stats.ArtifactBytes != 8 {
		t.Fatalf("artifact stats = orphan count %d orphan bytes %d artifact bytes %d", stats.OrphanCount, stats.OrphanBytes, stats.ArtifactBytes)
	}
}

func TestPurgeDoesNotOpenCorruptDB(t *testing.T) {
	layout := testLayout(t)
	writeFile(t, layout.LedgerDB(), "not sqlite")

	result, err := Purge(layout, false, true, os.RemoveAll)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if !result.Removed {
		t.Fatalf("Removed = false, want true")
	}
	if _, err := os.Stat(layout.DataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data root stat error = %v, want not exist", err)
	}
}

func TestPurgeRequiresYesUnlessDryRun(t *testing.T) {
	layout := testLayout(t)
	if _, err := Purge(layout, false, false, os.RemoveAll); err == nil {
		t.Fatal("Purge without --yes error = nil, want error")
	}
	if _, err := Purge(layout, true, false, os.RemoveAll); err != nil {
		t.Fatalf("dry-run Purge without --yes: %v", err)
	}
}

type fakeStore struct {
	runs    []ledger.Run
	deleted map[string]ledger.Run
}

func (s *fakeStore) ListRuns(context.Context) ([]ledger.Run, error) {
	runs := make([]ledger.Run, 0, len(s.runs))
	for _, run := range s.runs {
		if _, deleted := s.deleted[run.RunID]; !deleted {
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func (s *fakeStore) DeleteRun(_ context.Context, runID string) error {
	for _, run := range s.runs {
		if run.RunID == runID {
			if s.deleted == nil {
				s.deleted = map[string]ledger.Run{}
			}
			s.deleted[runID] = run
			return nil
		}
	}
	return ledger.ErrNotFound
}

func (s *fakeStore) Get(runID string) (ledger.Run, error) {
	for _, run := range s.runs {
		if run.RunID == runID {
			if _, deleted := s.deleted[runID]; deleted {
				return ledger.Run{}, ledger.ErrNotFound
			}
			return run, nil
		}
	}
	return ledger.Run{}, ledger.ErrNotFound
}

func testLayout(t *testing.T) statepaths.Layout {
	t.Helper()
	root := t.TempDir()
	layout := statepaths.NewLayout(filepath.Join(root, "data"), filepath.Join(root, "cache"))
	if err := os.MkdirAll(filepath.Join(layout.DataRoot, "runs"), 0o700); err != nil {
		t.Fatalf("MkdirAll runs: %v", err)
	}
	return layout
}

func testRun(layout statepaths.Layout, id string, mode ledger.PostMode, started time.Time) ledger.Run {
	return ledger.Run{
		RunID:           id,
		PRKey:           "github_owner_repo_1",
		SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Attempt:         1,
		Profile:         "default",
		PostingIdentity: "reviewer",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath: filepath.Join(
			layout.DataRoot,
			"runs",
			"github_owner_repo_1",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"default__reviewer",
			id,
		),
	}
}

func testNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func runItemIDs(items []RunItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.RunID)
	}
	return ids
}

func orphanPaths(items []OrphanItem) []string {
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	return paths
}
