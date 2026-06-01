// Package datalifecycle manages cr's durable local data retention.
package datalifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	// LiveRetention is the default retention window for live review runs.
	LiveRetention = 90 * 24 * time.Hour
	// DryRunRetention is the default retention window for dry-run review runs.
	DryRunRetention = 7 * 24 * time.Hour
)

// Store is the ledger behavior required by data lifecycle operations.
type Store interface {
	ListRuns(context.Context) ([]ledger.Run, error)
	DeleteRun(context.Context, string) error
}

// RemoveAllFunc removes a path recursively.
type RemoveAllFunc func(string) error

// Options contains lifecycle operation dependencies.
type Options struct {
	Layout    statepaths.Layout
	Store     Store
	Now       func() time.Time
	RemoveAll RemoveAllFunc
}

// PruneOptions selects runs to prune.
type PruneOptions struct {
	OlderThan time.Duration
	KeepLast  *int
	DryRun    bool
}

// Stats summarizes local durable data.
type Stats struct {
	DataRoot      string
	LedgerPath    string
	RunsRoot      string
	RunCount      int
	LiveRuns      int
	DryRunRuns    int
	OutcomeCounts map[ledger.Outcome]int
	OldestStarted *time.Time
	NewestStarted *time.Time
	ArtifactBytes int64
	OrphanCount   int
	OrphanBytes   int64
}

// PruneResult summarizes one prune operation.
type PruneResult struct {
	DryRun         bool
	SelectedRuns   []RunItem
	DeletedRuns    []RunItem
	OrphansRemoved []OrphanItem
	Warnings       []string
}

// PurgeResult summarizes a whole data-root purge.
type PurgeResult struct {
	DataRoot string
	DryRun   bool
	Removed  bool
}

// RunItem describes one selected or deleted ledger run.
type RunItem struct {
	RunID        string
	PostMode     ledger.PostMode
	StartedAt    time.Time
	ArtifactPath string
}

// OrphanItem describes one unreferenced artifact directory.
type OrphanItem struct {
	Path  string
	Bytes int64
}

// Show returns current data lifecycle stats.
func Show(ctx context.Context, opts Options) (Stats, error) {
	if err := validateOptions(opts, true); err != nil {
		return Stats{}, err
	}
	runs, err := opts.Store.ListRuns(ctx)
	if err != nil {
		return Stats{}, err
	}
	orphanItems, err := findOrphans(opts.Layout, runs)
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{
		DataRoot:      opts.Layout.DataRoot,
		LedgerPath:    opts.Layout.LedgerDB(),
		RunsRoot:      runsRoot(opts.Layout),
		RunCount:      len(runs),
		OutcomeCounts: make(map[ledger.Outcome]int),
	}
	for _, run := range runs {
		switch run.PostMode {
		case ledger.PostModeLive:
			stats.LiveRuns++
		case ledger.PostModeDryRun:
			stats.DryRunRuns++
		}
		if run.Outcome != nil {
			stats.OutcomeCounts[*run.Outcome]++
		}
		if stats.OldestStarted == nil || run.StartedAt.Before(*stats.OldestStarted) {
			started := run.StartedAt
			stats.OldestStarted = &started
		}
		if stats.NewestStarted == nil || run.StartedAt.After(*stats.NewestStarted) {
			started := run.StartedAt
			stats.NewestStarted = &started
		}
	}
	stats.ArtifactBytes, err = dirBytes(runsRoot(opts.Layout))
	if err != nil {
		return Stats{}, err
	}
	for _, orphan := range orphanItems {
		stats.OrphanCount++
		stats.OrphanBytes += orphan.Bytes
	}
	return stats, nil
}

// Prune deletes selected ledger rows first, then best-effort artifact dirs.
func Prune(ctx context.Context, opts Options, prune PruneOptions) (PruneResult, error) {
	if err := validateOptions(opts, true); err != nil {
		return PruneResult{}, err
	}
	if err := validatePruneOptions(prune); err != nil {
		return PruneResult{}, err
	}
	runs, err := opts.Store.ListRuns(ctx)
	if err != nil {
		return PruneResult{}, err
	}
	selected := selectRuns(runs, prune, opts.now())
	result := PruneResult{DryRun: prune.DryRun, SelectedRuns: runItems(selected)}
	if prune.DryRun {
		orphanItems, err := findOrphans(opts.Layout, runs)
		if err != nil {
			return result, err
		}
		result.OrphansRemoved = orphanItems
		return result, nil
	}

	for _, run := range selected {
		if err := opts.Store.DeleteRun(ctx, run.RunID); err != nil {
			return result, err
		}
		item := runItem(run)
		result.DeletedRuns = append(result.DeletedRuns, item)
		if err := validateArtifactPath(opts.Layout, run.ArtifactPath); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("skipped unsafe artifact path for run %s: %v", run.RunID, err))
			continue
		}
		if err := opts.removeAll()(run.ArtifactPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to remove artifacts for run %s at %s: %v", run.RunID, run.ArtifactPath, err))
		}
	}

	remaining, err := opts.Store.ListRuns(ctx)
	if err != nil {
		return result, err
	}
	orphans, err := findOrphans(opts.Layout, remaining)
	if err != nil {
		return result, err
	}
	for _, orphan := range orphans {
		if err := opts.removeAll()(orphan.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to remove orphan artifacts at %s: %v", orphan.Path, err))
			continue
		}
		result.OrphansRemoved = append(result.OrphansRemoved, orphan)
	}
	_ = removeEmptyParents(runsRoot(opts.Layout))
	return result, nil
}

// Purge removes the whole data root without opening the ledger database.
func Purge(layout statepaths.Layout, dryRun bool, yes bool, removeAll RemoveAllFunc) (PurgeResult, error) {
	if strings.TrimSpace(layout.DataRoot) == "" {
		return PurgeResult{}, fmt.Errorf("datalifecycle: data root is required")
	}
	result := PurgeResult{DataRoot: layout.DataRoot, DryRun: dryRun}
	if dryRun {
		return result, nil
	}
	if !yes {
		return PurgeResult{}, fmt.Errorf("datalifecycle: purge requires --yes")
	}
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	if err := removeAll(layout.DataRoot); err != nil {
		return result, err
	}
	result.Removed = true
	return result, nil
}

func validateOptions(opts Options, requireStore bool) error {
	if strings.TrimSpace(opts.Layout.DataRoot) == "" {
		return fmt.Errorf("datalifecycle: data root is required")
	}
	if requireStore && opts.Store == nil {
		return fmt.Errorf("datalifecycle: store is required")
	}
	return nil
}

func validatePruneOptions(opts PruneOptions) error {
	if opts.OlderThan < 0 {
		return fmt.Errorf("datalifecycle: --older-than must be positive")
	}
	if opts.OlderThan == 0 && opts.KeepLast == nil {
		return nil
	}
	if opts.OlderThan == 0 && opts.KeepLast == nil {
		return nil
	}
	if opts.OlderThan == 0 && opts.KeepLast != nil && *opts.KeepLast < 0 {
		return fmt.Errorf("datalifecycle: --keep-last must be non-negative")
	}
	if opts.OlderThan > 0 && opts.KeepLast != nil {
		return fmt.Errorf("datalifecycle: --older-than and --keep-last are mutually exclusive")
	}
	return nil
}

func selectRuns(runs []ledger.Run, opts PruneOptions, now time.Time) []ledger.Run {
	if opts.KeepLast != nil {
		return selectKeepLast(runs, *opts.KeepLast)
	}
	var selected []ledger.Run
	for _, run := range runs {
		cutoff := now.Add(-retentionFor(run.PostMode))
		if opts.OlderThan > 0 {
			cutoff = now.Add(-opts.OlderThan)
		}
		if run.StartedAt.Before(cutoff) {
			selected = append(selected, run)
		}
	}
	return selected
}

func selectKeepLast(runs []ledger.Run, keep int) []ledger.Run {
	seen := map[ledger.PostMode]int{}
	var selected []ledger.Run
	for _, run := range runs {
		seen[run.PostMode]++
		if seen[run.PostMode] > keep {
			selected = append(selected, run)
		}
	}
	return selected
}

func retentionFor(mode ledger.PostMode) time.Duration {
	if mode == ledger.PostModeDryRun {
		return DryRunRetention
	}
	return LiveRetention
}

func findOrphans(layout statepaths.Layout, runs []ledger.Run) ([]OrphanItem, error) {
	return orphanItems(layout, runs, false, nil)
}

func orphanItems(layout statepaths.Layout, runs []ledger.Run, remove bool, removeAll RemoveAllFunc) ([]OrphanItem, error) {
	root := runsRoot(layout)
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	referenced, err := referencedArtifacts(layout, runs)
	if err != nil {
		return nil, err
	}
	var items []OrphanItem
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		isAttempt, err := isAttemptDir(root, path)
		if err != nil || !isAttempt {
			return err
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if referenced[filepath.Clean(abs)] {
			return filepath.SkipDir
		}
		bytes, err := dirBytes(path)
		if err != nil {
			return err
		}
		items = append(items, OrphanItem{Path: path, Bytes: bytes})
		if remove {
			if err := removeAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return filepath.SkipDir
		}
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	if remove {
		_ = removeEmptyParents(root)
	}
	return items, nil
}

func referencedArtifacts(layout statepaths.Layout, runs []ledger.Run) (map[string]bool, error) {
	referenced := map[string]bool{}
	for _, run := range runs {
		if err := validateArtifactPath(layout, run.ArtifactPath); err != nil {
			continue
		}
		abs, err := filepath.Abs(run.ArtifactPath)
		if err != nil {
			return nil, err
		}
		referenced[filepath.Clean(abs)] = true
	}
	return referenced, nil
}

func validateArtifactPath(layout statepaths.Layout, artifactPath string) error {
	if strings.TrimSpace(artifactPath) == "" {
		return fmt.Errorf("empty artifact path")
	}
	root, err := filepath.Abs(runsRoot(layout))
	if err != nil {
		return err
	}
	dataRoot, err := filepath.Abs(layout.DataRoot)
	if err != nil {
		return err
	}
	artifact, err := filepath.Abs(artifactPath)
	if err != nil {
		return err
	}
	artifact = filepath.Clean(artifact)
	if artifact == filepath.Clean(dataRoot) {
		return fmt.Errorf("%s is the data root", artifactPath)
	}
	if artifact == filepath.Clean(root) {
		return fmt.Errorf("%s is the runs root", artifactPath)
	}
	rel, err := filepath.Rel(root, artifact)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is outside %s", artifactPath, root)
	}
	isAttempt, err := isAttemptDir(root, artifact)
	if err != nil {
		return err
	}
	if !isAttempt {
		return fmt.Errorf("%s is not a run artifact directory", artifactPath)
	}
	return nil
}

func isAttemptDir(root, path string) (bool, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return false, nil
	}
	return len(strings.Split(rel, string(filepath.Separator))) == 5, nil
}

func dirBytes(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

func removeEmptyParents(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return nil
}

func runsRoot(layout statepaths.Layout) string {
	return filepath.Join(layout.DataRoot, "runs")
}

func runItems(runs []ledger.Run) []RunItem {
	items := make([]RunItem, 0, len(runs))
	for _, run := range runs {
		items = append(items, runItem(run))
	}
	return items
}

func runItem(run ledger.Run) RunItem {
	return RunItem{
		RunID:        run.RunID,
		PostMode:     run.PostMode,
		StartedAt:    run.StartedAt,
		ArtifactPath: run.ArtifactPath,
	}
}

func (opts Options) now() time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}
	return time.Now().UTC()
}

func (opts Options) removeAll() RemoveAllFunc {
	if opts.RemoveAll != nil {
		return opts.RemoveAll
	}
	return os.RemoveAll
}
