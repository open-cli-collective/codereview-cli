package runlifecycle

import (
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func TestNewestCompatibleIncompleteRun(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	incomplete := ledger.OutcomeIncomplete
	complete := ledger.OutcomeDryRun
	runs := []ledger.Run{
		{RunID: "wrong-base", BaseSHA: "old", Attempt: 9, PostMode: ledger.PostModeDryRun, StartedAt: t0, ArtifactPath: dir + "/wrong-base"},
		{RunID: "wrong-mode", BaseSHA: "base", Attempt: 9, PostMode: ledger.PostModeLive, StartedAt: t0, ArtifactPath: dir + "/wrong-mode"},
		{RunID: "complete", BaseSHA: "base", Attempt: 9, PostMode: ledger.PostModeDryRun, StartedAt: t0, Outcome: &complete, ArtifactPath: dir + "/complete"},
		{RunID: "unmarked", BaseSHA: "base", Attempt: 9, PostMode: ledger.PostModeDryRun, StartedAt: t0, Outcome: &incomplete, ArtifactPath: dir + "/unmarked"},
		{RunID: "older-attempt", BaseSHA: "base", Attempt: 1, PostMode: ledger.PostModeDryRun, StartedAt: t0.Add(time.Hour), ArtifactPath: dir + "/older-attempt"},
		{RunID: "older-time", BaseSHA: "base", Attempt: 2, PostMode: ledger.PostModeDryRun, StartedAt: t0, ArtifactPath: dir + "/older-time"},
		{RunID: "best", BaseSHA: "base", Attempt: 2, PostMode: ledger.PostModeDryRun, StartedAt: t0.Add(time.Minute), ArtifactPath: dir + "/best"},
	}
	for _, run := range runs {
		if run.RunID == "unmarked" {
			continue
		}
		if err := runartifact.WriteMarker(run.ArtifactPath, runartifact.KindReview, run.RunID); err != nil {
			t.Fatalf("WriteMarker(%s): %v", run.RunID, err)
		}
	}

	got, ok := NewestCompatibleIncompleteRun(runs, "base", ledger.PostModeDryRun, runartifact.KindReview, runartifact.MarkerMatches)
	if !ok || got.RunID != "best" {
		t.Fatalf("NewestCompatibleIncompleteRun = (%q, %v), want (best, true)", got.RunID, ok)
	}
	if _, ok := NewestCompatibleIncompleteRun(runs, "base", ledger.PostModeDryRun, runartifact.KindThreadResponse, runartifact.MarkerMatches); ok {
		t.Fatal("NewestCompatibleIncompleteRun with response marker = found, want none")
	}
}

func TestComparePremises(t *testing.T) {
	run := ledger.Run{SHA: "head-old", BaseSHA: "base-old"}
	stable := ComparePremises(run, "head-old", "base-old")
	if stable.Moved {
		t.Fatalf("stable comparison = %#v, want not moved", stable)
	}
	moved := ComparePremises(run, "head-new", "base-new")
	if !moved.Moved || moved.StoredHeadSHA != "head-old" || moved.CurrentHeadSHA != "head-new" || moved.StoredBaseSHA != "base-old" || moved.CurrentBaseSHA != "base-new" {
		t.Fatalf("moved comparison = %#v", moved)
	}
}

func TestPostingKey(t *testing.T) {
	if got := PostingKey(gitprovider.Identity{Login: "login", ID: "id", DisplayName: "name"}); got != "login" {
		t.Fatalf("PostingKey with login = %q, want login", got)
	}
	if got := PostingKey(gitprovider.Identity{ID: "id", DisplayName: "name"}); got != "id" {
		t.Fatalf("PostingKey with ID = %q, want id", got)
	}
	if got := PostingKey(gitprovider.Identity{DisplayName: "name"}); got != "" {
		t.Fatalf("PostingKey with display name only = %q, want empty", got)
	}
}
