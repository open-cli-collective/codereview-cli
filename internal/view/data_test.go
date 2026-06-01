package view

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
)

func TestRenderDataShowTextAndJSON(t *testing.T) {
	started := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stats := datalifecycle.Stats{
		DataRoot:      "/data/codereview",
		LedgerPath:    "/data/codereview/ledger.db",
		RunsRoot:      "/data/codereview/runs",
		RunCount:      2,
		LiveRuns:      1,
		DryRunRuns:    1,
		OutcomeCounts: map[ledger.Outcome]int{ledger.OutcomeComment: 1},
		OldestStarted: &started,
		NewestStarted: &started,
		ArtifactBytes: 42,
		OrphanCount:   1,
		OrphanBytes:   7,
	}
	result := NewDataShow(stats)
	var text bytes.Buffer

	if err := RenderDataShowText(&text, result); err != nil {
		t.Fatalf("RenderDataShowText: %v", err)
	}
	for _, want := range []string{"Data root: /data/codereview", "Run count: 2", "Orphan bytes: 7"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text = %q, want %q", text.String(), want)
		}
	}

	var jsonBuf bytes.Buffer
	if err := RenderDataShowJSON(&jsonBuf, result); err != nil {
		t.Fatalf("RenderDataShowJSON: %v", err)
	}
	var decoded DataShow
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.DataRoot != stats.DataRoot || decoded.OutcomeCounts[ledger.OutcomeComment.String()] != 1 || decoded.ArtifactBytes != 42 {
		t.Fatalf("decoded JSON = %#v", decoded)
	}
}

func TestRenderDataPruneTextAndJSON(t *testing.T) {
	started := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	result := NewDataPrune(datalifecycle.PruneResult{
		DryRun:       true,
		SelectedRuns: []datalifecycle.RunItem{{RunID: "run-1", PostMode: ledger.PostModeDryRun, StartedAt: started, ArtifactPath: "/data/runs/run-1"}},
		Warnings:     []string{"kept orphan"},
	})
	var text bytes.Buffer

	if err := RenderDataPruneText(&text, result); err != nil {
		t.Fatalf("RenderDataPruneText: %v", err)
	}
	if !strings.Contains(text.String(), "Would delete runs: 1") || !strings.Contains(text.String(), "Warnings:") {
		t.Fatalf("text = %q, want dry-run count and warnings", text.String())
	}

	var jsonBuf bytes.Buffer
	if err := RenderDataPruneJSON(&jsonBuf, result); err != nil {
		t.Fatalf("RenderDataPruneJSON: %v", err)
	}
	var decoded DataPrune
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.DryRun || len(decoded.SelectedRuns) != 1 || decoded.SelectedRuns[0].PostMode != ledger.PostModeDryRun.String() || decoded.Warnings[0] != "kept orphan" {
		t.Fatalf("decoded JSON = %#v", decoded)
	}
}

func TestRenderDataPurgeTextAndJSON(t *testing.T) {
	result := NewDataPurge(datalifecycle.PurgeResult{DataRoot: "/data/codereview", DryRun: true})
	var text bytes.Buffer

	if err := RenderDataPurgeText(&text, result); err != nil {
		t.Fatalf("RenderDataPurgeText: %v", err)
	}
	if got := text.String(); got != "Would purge data root: /data/codereview\n" {
		t.Fatalf("text = %q", got)
	}

	var jsonBuf bytes.Buffer
	if err := RenderDataPurgeJSON(&jsonBuf, result); err != nil {
		t.Fatalf("RenderDataPurgeJSON: %v", err)
	}
	var decoded DataPurge
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.DataRoot != "/data/codereview" || !decoded.DryRun || decoded.Removed {
		t.Fatalf("decoded JSON = %#v", decoded)
	}
}
