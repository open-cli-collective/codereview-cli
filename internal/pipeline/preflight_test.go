package pipeline

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func TestPreflightKeepsDiscussionAndLeavesFormalReviewStateAlone(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	addExistingDiscussion(provider, req.PostingIdentity)
	sessionName, scope := storePriorSessions(ctx, t, store, provider, req)
	beforeSession, err := store.GetNamedSession(ctx, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	beforeCohort, err := store.GetReviewerCohort(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("dossier", discussionSummaryJSON([]string{discussionCommentMarker}, nil), 8, 2))
	adapter.Queue(fakeLLMResult("preflight-selection", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("preflight-reviewer", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("preflight-rollup", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	opts := withoutDiscussionOptions(t, provider, adapter, store, "run-preflight")
	head, base := provider.pr.Head.SHA, provider.pr.Base.SHA
	ordinary := allocateDryRunForSHAs(t, store, opts.Layout, req, "run-ordinary", head, base, fixedNow().Add(-2*time.Minute))
	req.Preflight = true

	result, err := dryRunForTest(ctx, opts, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.RunID != "run-preflight" || result.Run.PostMode != ledger.PostModeDryRun || result.Plan.Incomplete || len(result.Findings) != 1 {
		t.Fatalf("preflight did not produce a fresh dry run with a finding: run=%#v findings=%#v", result.Run, result.Findings)
	}
	if !strings.Contains(result.Plan.RollupMarkdown, "PR Preflight") || strings.Contains(result.Plan.RollupMarkdown, "Posting as") {
		t.Fatalf("preflight rollup presents itself as a posting review: %s", result.Plan.RollupMarkdown)
	}
	if result.NamedSessionCandidate != nil || len(adapter.Resumes()) != 0 {
		t.Fatal("preflight used persistent review conversations")
	}
	if !strings.Contains(dossierText(t, result.Artifacts.DossierDir), discussionThreadMarker) {
		t.Fatal("preflight discarded current PR discussion")
	}
	marker, err := runartifact.ReadMarker(result.Artifacts.Dir, runartifact.KindPreflight)
	if err != nil || marker.RunID != result.Run.RunID || runartifact.MarkerMatches(result.Artifacts.Dir, runartifact.KindReview, result.Run.RunID) {
		t.Fatalf("preflight marker = %#v, error %v", marker, err)
	}
	afterSession, err := store.GetNamedSession(ctx, sessionName)
	if err != nil || !reflect.DeepEqual(afterSession, beforeSession) {
		t.Fatalf("formal orchestrator session changed: %#v, error %v", afterSession, err)
	}
	afterCohort, err := store.GetReviewerCohort(ctx, scope)
	if err != nil || !reflect.DeepEqual(afterCohort, beforeCohort) {
		t.Fatalf("formal reviewer cohort changed: %#v, error %v", afterCohort, err)
	}
	interrupted := allocateDryRunForSHAs(t, store, opts.Layout, req, "run-interrupted-preflight", head, base, fixedNow().Add(-time.Minute))
	removeReviewRunMarkerForTest(t, interrupted.ArtifactPath)
	if err := runartifact.WriteMarker(interrupted.ArtifactPath, runartifact.KindPreflight, interrupted.RunID); err != nil {
		t.Fatal(err)
	}
	req.Preflight = false
	resumed, found, err := findIncompleteDryRun(ctx, store, req, provider.pr)
	if err != nil || !found || resumed.RunID != ordinary.RunID {
		t.Fatalf("ordinary dry-run recovery selected %q, want %q (found=%v, error=%v)", resumed.RunID, ordinary.RunID, found, err)
	}
}
