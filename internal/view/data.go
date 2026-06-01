package view

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
)

// DataShow is the presentation model for `cr data show`.
type DataShow struct {
	DataRoot      string         `json:"data_root"`
	LedgerPath    string         `json:"ledger_path"`
	RunsRoot      string         `json:"runs_root"`
	RunCount      int            `json:"run_count"`
	LiveRuns      int            `json:"live_runs"`
	DryRunRuns    int            `json:"dry_run_runs"`
	OutcomeCounts map[string]int `json:"outcome_counts"`
	OldestStarted *time.Time     `json:"oldest_started,omitempty"`
	NewestStarted *time.Time     `json:"newest_started,omitempty"`
	ArtifactBytes int64          `json:"artifact_bytes"`
	OrphanCount   int            `json:"orphan_count"`
	OrphanBytes   int64          `json:"orphan_bytes"`
}

// DataPrune is the presentation model for `cr data prune`.
type DataPrune struct {
	DryRun         bool             `json:"dry_run"`
	SelectedRuns   []DataRunItem    `json:"selected_runs"`
	DeletedRuns    []DataRunItem    `json:"deleted_runs"`
	OrphansRemoved []DataOrphanItem `json:"orphans_removed"`
	Warnings       []string         `json:"warnings,omitempty"`
}

// DataPurge is the presentation model for `cr data purge`.
type DataPurge struct {
	DataRoot string `json:"data_root"`
	DryRun   bool   `json:"dry_run"`
	Removed  bool   `json:"removed"`
}

// DataRunItem describes one lifecycle-selected run.
type DataRunItem struct {
	RunID        string    `json:"run_id"`
	PostMode     string    `json:"post_mode"`
	StartedAt    time.Time `json:"started_at"`
	ArtifactPath string    `json:"artifact_path"`
}

// DataOrphanItem describes one lifecycle orphan artifact directory.
type DataOrphanItem struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// NewDataShow builds a data stats presentation model.
func NewDataShow(stats datalifecycle.Stats) DataShow {
	outcomes := make(map[string]int, len(stats.OutcomeCounts))
	for outcome, count := range stats.OutcomeCounts {
		outcomes[outcome.String()] = count
	}
	return DataShow{
		DataRoot:      stats.DataRoot,
		LedgerPath:    stats.LedgerPath,
		RunsRoot:      stats.RunsRoot,
		RunCount:      stats.RunCount,
		LiveRuns:      stats.LiveRuns,
		DryRunRuns:    stats.DryRunRuns,
		OutcomeCounts: outcomes,
		OldestStarted: stats.OldestStarted,
		NewestStarted: stats.NewestStarted,
		ArtifactBytes: stats.ArtifactBytes,
		OrphanCount:   stats.OrphanCount,
		OrphanBytes:   stats.OrphanBytes,
	}
}

// NewDataPrune builds a prune presentation model.
func NewDataPrune(result datalifecycle.PruneResult) DataPrune {
	return DataPrune{
		DryRun:         result.DryRun,
		SelectedRuns:   newDataRunItems(result.SelectedRuns),
		DeletedRuns:    newDataRunItems(result.DeletedRuns),
		OrphansRemoved: newDataOrphanItems(result.OrphansRemoved),
		Warnings:       append([]string(nil), result.Warnings...),
	}
}

// NewDataPurge builds a purge presentation model.
func NewDataPurge(result datalifecycle.PurgeResult) DataPurge {
	return DataPurge{DataRoot: result.DataRoot, DryRun: result.DryRun, Removed: result.Removed}
}

// RenderDataShowText writes stable human-readable data stats.
func RenderDataShowText(w io.Writer, result DataShow) error {
	if err := writeKV(w, "Data root", result.DataRoot); err != nil {
		return err
	}
	if err := writeKV(w, "Ledger", result.LedgerPath); err != nil {
		return err
	}
	if err := writeKV(w, "Runs", result.RunsRoot); err != nil {
		return err
	}
	if err := writeKV(w, "Run count", fmt.Sprint(result.RunCount)); err != nil {
		return err
	}
	if err := writeKV(w, "Live runs", fmt.Sprint(result.LiveRuns)); err != nil {
		return err
	}
	if err := writeKV(w, "Dry-run runs", fmt.Sprint(result.DryRunRuns)); err != nil {
		return err
	}
	if result.OldestStarted != nil {
		if err := writeKV(w, "Oldest started", result.OldestStarted.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if result.NewestStarted != nil {
		if err := writeKV(w, "Newest started", result.NewestStarted.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := writeKV(w, "Artifact bytes", fmt.Sprint(result.ArtifactBytes)); err != nil {
		return err
	}
	if err := writeKV(w, "Orphans", fmt.Sprint(result.OrphanCount)); err != nil {
		return err
	}
	return writeKV(w, "Orphan bytes", fmt.Sprint(result.OrphanBytes))
}

// RenderDataShowJSON writes data stats as indented JSON.
func RenderDataShowJSON(w io.Writer, result DataShow) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// RenderDataPruneText writes a prune summary.
func RenderDataPruneText(w io.Writer, result DataPrune) error {
	action := "Deleted runs"
	count := len(result.DeletedRuns)
	if result.DryRun {
		action = "Would delete runs"
		count = len(result.SelectedRuns)
	}
	if err := writeKV(w, action, fmt.Sprint(count)); err != nil {
		return err
	}
	if err := writeKV(w, "Orphans removed", fmt.Sprint(len(result.OrphansRemoved))); err != nil {
		return err
	}
	if len(result.Warnings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Warnings:"); err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(w, "  - %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// RenderDataPruneJSON writes a prune summary as indented JSON.
func RenderDataPruneJSON(w io.Writer, result DataPrune) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// RenderDataPurgeText writes a purge summary.
func RenderDataPurgeText(w io.Writer, result DataPurge) error {
	if result.DryRun {
		_, err := fmt.Fprintf(w, "Would purge data root: %s\n", result.DataRoot)
		return err
	}
	_, err := fmt.Fprintf(w, "Purged data root: %s\n", result.DataRoot)
	return err
}

// RenderDataPurgeJSON writes a purge summary as indented JSON.
func RenderDataPurgeJSON(w io.Writer, result DataPurge) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func newDataRunItems(items []datalifecycle.RunItem) []DataRunItem {
	out := make([]DataRunItem, 0, len(items))
	for _, item := range items {
		out = append(out, DataRunItem{
			RunID:        item.RunID,
			PostMode:     item.PostMode.String(),
			StartedAt:    item.StartedAt,
			ArtifactPath: item.ArtifactPath,
		})
	}
	return out
}

func newDataOrphanItems(items []datalifecycle.OrphanItem) []DataOrphanItem {
	out := make([]DataOrphanItem, 0, len(items))
	for _, item := range items {
		out = append(out, DataOrphanItem{Path: item.Path, Bytes: item.Bytes})
	}
	return out
}
