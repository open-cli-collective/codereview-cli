package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func dryRunForTest(ctx context.Context, opts Options, req Request) (Result, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	return DryRun(ctx, opts, req)
}

func selectionOnlyForTest(ctx context.Context, opts Options, req SelectionRequest) (SelectionResult, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	return SelectionOnly(ctx, opts, req)
}

func liveForTest(ctx context.Context, opts Options, req Request, run ledger.Run) (Result, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	return Live(ctx, opts, req, run)
}

func TestDryRunPlansAndPersistsWithoutProviderWrites(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.pr.Body = "Document the checkout-native review contract."
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
		Comments: []gitprovider.ThreadComment{{
			ID:     "comment-1",
			Body:   "Inline concern",
			Author: gitprovider.Identity{Login: "reviewer"},
		}},
	}}
	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-1",
		Body:   "Top-level concern",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	provider.reviews = []gitprovider.Review{{
		ID:     "review-1",
		Body:   "Review body",
		Author: gitprovider.Identity{Login: "architect"},
		Event:  review.ReviewEventComment,
	}, {
		ID:     "review-2",
		Body:   "Approved body should stay out of reviewer-facing discussion",
		Author: gitprovider.Identity{Login: "approver"},
		Event:  review.ReviewEventApprove,
	}}
	adapter := &llm.FakeAdapter{
		NameValue:      "fake-llm",
		QuotaValue:     llm.Quota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		QuotaSupported: true,
	}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Top-level concern", "Review body"}, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Inline concern"}}), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry-run", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired dry-run before provider GetPR error = %v, want ErrNotFound", err)
		}
	}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-1" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if result.Run.RunID != "run-1" || result.Run.PostMode != ledger.PostModeDryRun {
		t.Fatalf("run = %#v, want dry-run run-1", result.Run)
	}
	if !result.QuotaSupported || result.Quota.BlockRemainingPct != 87 || result.QuotaLow {
		t.Fatalf("quota result = supported %v quota %#v low %v", result.QuotaSupported, result.Quota, result.QuotaLow)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("findings = %#v", result.Findings)
	}
	if len(result.Sessions) != 3 {
		t.Fatalf("sessions len = %d, want selection/reviewer/rollup", len(result.Sessions))
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("requests len = %d, want dossier-summary/selection/reviewer/rollup", len(requests))
	}
	for _, request := range requests {
		if request.Model != "claude-sonnet-4-6" || request.Effort != "medium" {
			t.Fatalf("request = model:%q effort:%q, want claude-sonnet-4-6/medium from agent config", request.Model, request.Effort)
		}
	}
	for _, session := range result.Sessions {
		if session.Model != "claude-sonnet-4-6" || session.Effort == nil || *session.Effort != "medium" {
			t.Fatalf("session = model:%q effort:%v, want claude-sonnet-4-6/medium from agent config", session.Model, session.Effort)
		}
	}
	if got := result.Sessions[1].ProviderSessionID; got != "reviewer-session" {
		t.Fatalf("reviewer provider session = %q", got)
	}
	if len(result.PlannedActions) != 3 {
		t.Fatalf("planned actions len = %d, want inline/rollup/submit", len(result.PlannedActions))
	}
	for _, action := range result.PlannedActions {
		if action.Status != ledger.PlannedActionPlannedOnly {
			t.Fatalf("action status = %q, want planned_only for %#v", action.Status, action)
		}
		if strings.Contains(action.PayloadJSON, "<!-- codereview:") {
			t.Fatalf("dry-run payload contains real marker: %s", action.PayloadJSON)
		}
	}

	run, err := store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("stored outcome = %#v, want dry_run", run.Outcome)
	}
	if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("expired dry-run GetRun error = %v, want ErrNotFound", err)
	}
	storedFindings, err := store.ListFindings(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(storedFindings) != 1 || storedFindings[0].SessionRowID != "session-3" {
		t.Fatalf("stored findings = %#v, want reviewer session FK", storedFindings)
	}
	storedActions, err := store.ListPlannedActions(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if len(storedActions) != len(result.PlannedActions) {
		t.Fatalf("stored actions len = %d, want %d", len(storedActions), len(result.PlannedActions))
	}

	assertFileContains(t, result.Artifacts.DiffPatch, "diff --git a/main.go b/main.go")
	assertFileContains(t, result.Artifacts.FindingsJSON, `"severity": "major"`)
	assertFileContains(t, result.Artifacts.RollupMarkdown, "Automated PR Review")
	assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "pr-intent.md"), "Document the checkout-native review contract.")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "main.go:2")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Top-level concern")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Review body")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "Guidance provenance: repo@refs/heads/main:")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "Guidance source status: missing")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "PR-head .codereview/agents changes do not affect this listing.")
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "final/discussion.md")
	assertFileOmits(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "provider_session_id", "session_row_id", "mergeability", "approval", "CI status", "Approved body should stay out of reviewer-facing discussion")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "raw", "top-level-comments.json"), "Approved body should stay out of reviewer-facing discussion")
	slicePath, err := result.Artifacts.SlicePatch("harness:reviewer", "main.go")
	if err != nil {
		t.Fatalf("SlicePatch: %v", err)
	}
	assertFileContains(t, slicePath, "+var changed = true")
	for _, path := range []string{result.Artifacts.FindingsJSON, result.Artifacts.RollupMarkdown} {
		data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if strings.Contains(string(data), "<!-- codereview:") {
			t.Fatalf("artifact %s contains real marker: %s", path, data)
		}
	}
}

func TestDryRunResumesIncompleteRunAttempt(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForProvider(t, store, layout, provider, req, "run-resume", fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != resume.RunID || result.Artifacts.Dir != resume.ArtifactPath {
		t.Fatalf("result run = %q artifacts %q, want resumed %q artifacts %q", result.Run.RunID, result.Artifacts.Dir, resume.RunID, resume.ArtifactPath)
	}
	stored, err := store.GetRun(ctx, resume.RunID)
	if err != nil {
		t.Fatalf("GetRun resume: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("resumed run outcome = %v, want dry_run", stored.Outcome)
	}
}

func TestDryRunDoesNotResumeThreadResponseRun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	responseRun := allocateDryRunForProvider(t, store, layout, provider, req, "run-response", fixedNow().Add(-time.Minute))
	removeReviewRunMarkerForTest(t, responseRun.ArtifactPath)
	writeResponseRunMarkerForTest(t, responseRun.ArtifactPath, responseRun.RunID)

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-review" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != "run-review" {
		t.Fatalf("result run = %q, want fresh review run", result.Run.RunID)
	}
	if result.Artifacts.Dir == responseRun.ArtifactPath {
		t.Fatalf("result artifacts dir = %q, want not response artifact root", result.Artifacts.Dir)
	}
	storedResponse, err := store.GetRun(ctx, responseRun.RunID)
	if err != nil {
		t.Fatalf("GetRun response: %v", err)
	}
	if storedResponse.Outcome != nil {
		t.Fatalf("response run outcome = %#v, want still incomplete", storedResponse.Outcome)
	}
}

func TestDryRunResumesPinnedReviewRunByPinnedSHAs(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	fixture, reviewBaseSHA, reviewHeadSHA := newPinnedReviewFixtureForRef(t, req.PRRef)
	provider.pr = fixture.pr
	provider.fixtureRepoDir = fixture.repoDir
	provider.diffBetween = gitprovider.UnifiedDiff{}
	req.ReviewBaseSHA = reviewBaseSHA
	req.ReviewHeadSHA = reviewHeadSHA
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForSHAs(t, store, layout, req, "run-pinned-resume", reviewHeadSHA, reviewBaseSHA, fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable pinned dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != resume.RunID || result.Artifacts.Dir != resume.ArtifactPath {
		t.Fatalf("result run/artifacts = %q %q, want %q %q", result.Run.RunID, result.Artifacts.Dir, resume.RunID, resume.ArtifactPath)
	}
	if len(provider.diffBetweenCalls) != 1 || provider.diffBetweenCalls[0].baseSHA != reviewBaseSHA || provider.diffBetweenCalls[0].headSHA != reviewHeadSHA {
		t.Fatalf("diff between calls = %#v, want pinned base/head", provider.diffBetweenCalls)
	}
}

func TestDryRunResumeLoadsCompletedTasksAndRerunsFailedTaskOnly(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocateDryRunForProvider(t, store, layout, provider, req, "run-task-resume", fixedNow().Add(-time.Minute))

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	firstAdapter.Queue(fakeLLMResult("rollup-retry-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "unexpected-fresh-run" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("first DryRun error = %v, want invalid rollup after retry", err)
	}
	stored, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun first: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("first run outcome = %#v, want incomplete", stored.Outcome)
	}
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	for _, taskID := range []string{orchestratorSelectionStage, reviewerTaskID("harness:reviewer")} {
		meta, ok, err := readLLMTaskMetadata(artifacts, taskID)
		if err != nil || !ok || meta.Status != llmTaskStatusSucceeded {
			t.Fatalf("task %s metadata = %#v ok %v err %v, want succeeded", taskID, meta, ok, err)
		}
	}
	rollupMeta, ok, err := readLLMTaskMetadata(artifacts, orchestratorRollupStage)
	if err != nil || !ok || rollupMeta.Status != llmTaskStatusFailedBlocking {
		t.Fatalf("rollup metadata = %#v ok %v err %v, want failed_blocking", rollupMeta, ok, err)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	secondAdapter.Queue(fakeLLMResult("rollup-fixed-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  secondAdapter,
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("second DryRun: %v", err)
	}
	if result.Run.RunID != run.RunID || result.Artifacts.Dir != run.ArtifactPath {
		t.Fatalf("result run/artifacts = %q %q, want %q %q", result.Run.RunID, result.Artifacts.Dir, run.RunID, run.ArtifactPath)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want cached completed tasks and resumed rollup only", secondAdapter.Requests())
	}
	resumes := secondAdapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "rollup-retry-session" {
		t.Fatalf("second adapter resumes = %#v, want only failed rollup retry session", resumes)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("result findings = %#v, want cached reviewer finding", result.Findings)
	}
	stored, err = store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("second run outcome = %#v, want dry_run", stored.Outcome)
	}
}

func TestDryRunResumeReusesPersistedPlanningRows(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	failingComplete := &completeFailingStore{Store: store, err: errors.New("complete failed after planning")}
	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           failingComplete,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-planning-resume" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !strings.Contains(err.Error(), "complete failed after planning") {
		t.Fatalf("first DryRun error = %v, want complete failure", err)
	}
	actions, err := store.ListPlannedActions(ctx, "run-planning-resume")
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if len(actions) == 0 {
		t.Fatal("planned actions len = 0, want persisted planning rows")
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  secondAdapter,
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable post-planning dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("second DryRun: %v", err)
	}
	if result.Run.RunID != "run-planning-resume" {
		t.Fatalf("second result run = %#v, want resumed dry-run", result.Run)
	}
	stored, err := store.GetRun(ctx, "run-planning-resume")
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("stored second run outcome = %#v, want dry_run", stored.Outcome)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want cached LLM tasks", secondAdapter.Requests())
	}
	if len(result.PlannedActions) != len(actions) {
		t.Fatalf("result planned actions len = %d, want persisted %d", len(result.PlannedActions), len(actions))
	}
}

func TestDryRunRerunBypassesIncompleteRunAttempt(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.Rerun = true
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForProvider(t, store, layout, provider, req, "run-resume", fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-fresh" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != "run-fresh" {
		t.Fatalf("result run = %q, want fresh run", result.Run.RunID)
	}
	if result.Artifacts.Dir == resume.ArtifactPath {
		t.Fatalf("result artifacts dir = %q, want fresh artifact root", result.Artifacts.Dir)
	}
	storedResume, err := store.GetRun(ctx, resume.RunID)
	if err != nil {
		t.Fatalf("GetRun resume: %v", err)
	}
	if storedResume.Outcome != nil {
		t.Fatalf("bypassed run outcome = %v, want still incomplete", storedResume.Outcome)
	}
}

func TestDryRunIncompleteReviewerCoverageForcesCommentOutcome(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", coverageOnlyJSON("harness:reviewer", nil, []string{"main.go"}, "could not inspect assigned file"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("approve", nil), 1, 1))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-incomplete-coverage" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("outcome = %q, want comment despite approve rollup", result.Plan.Outcome)
	}
	coverage := result.Plan.Summary.Run.ReviewerCoverage
	if len(coverage) != 1 ||
		coverage[0].AgentID != "harness:reviewer" ||
		coverage[0].Status != reviewerCoverageIncompleteSkipped ||
		!reflect.DeepEqual(coverage[0].SkippedFiles, []string{"main.go"}) {
		t.Fatalf("coverage = %#v, want incomplete skipped reviewer coverage", coverage)
	}
	if !strings.Contains(result.Plan.RollupMarkdown, "### Reviewer Coverage") {
		t.Fatalf("rollup markdown missing reviewer coverage:\n%s", result.Plan.RollupMarkdown)
	}
}

func TestDryRunNormalizesReviewerFindingsFileAlias(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsFileAliasJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-file-alias" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].FilePath != "main.go" {
		t.Fatalf("findings = %#v, want canonical main.go path", result.Findings)
	}
	storedFindings, err := store.ListFindings(ctx, "run-file-alias")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(storedFindings) != 1 || storedFindings[0].FilePath != "main.go" {
		t.Fatalf("stored findings = %#v, want canonical main.go path", storedFindings)
	}
	data, err := os.ReadFile(result.Artifacts.FindingsJSON) // #nosec G304 -- test reads artifact path returned by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", result.Artifacts.FindingsJSON, err)
	}
	if !strings.Contains(string(data), `"file_path": "main.go"`) {
		t.Fatalf("findings artifact = %s, want canonical file_path", data)
	}
	if strings.Contains(string(data), `"file":`) {
		t.Fatalf("findings artifact leaked file alias: %s", data)
	}
}

func TestDryRunWithPinnedReviewSHAsUsesCompareDiffAndPinnedFileRefs(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentFullContent(t, req.Profile.AgentSources[0], "harness", "reviewer")
	fixture, reviewBaseSHA, reviewHeadSHA := newPinnedReviewFixtureForRef(t, req.PRRef)
	provider.pr = fixture.pr
	provider.fixtureRepoDir = fixture.repoDir
	req.ReviewBaseSHA = reviewBaseSHA
	req.ReviewHeadSHA = reviewHeadSHA
	provider.diffBetween = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	provider.files[fileKey{gitRef: reviewBaseSHA, path: "main.go"}] = []byte("package main\nvar changed = false\n")
	provider.files[fileKey{gitRef: reviewHeadSHA, path: "main.go"}] = []byte("package main\nvar changed = true\n")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-pinned" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if len(provider.diffBetweenCalls) != 1 || provider.diffBetweenCalls[0].baseSHA != reviewBaseSHA || provider.diffBetweenCalls[0].headSHA != reviewHeadSHA {
		t.Fatalf("diff between calls = %#v, want pinned base/head", provider.diffBetweenCalls)
	}
	if result.CurrentBaseSHA != provider.pr.Base.SHA || result.CurrentHeadSHA != provider.pr.Head.SHA ||
		result.ReviewBaseSHA != reviewBaseSHA || result.ReviewHeadSHA != reviewHeadSHA {
		t.Fatalf("result SHAs = current %s/%s review %s/%s", result.CurrentBaseSHA, result.CurrentHeadSHA, result.ReviewBaseSHA, result.ReviewHeadSHA)
	}
	if !strings.Contains(result.Artifacts.Dir, reviewHeadSHA) || !strings.Contains(result.Artifacts.Dir, reviewBaseSHA) {
		t.Fatalf("artifact dir = %s, want pinned head/base SHAs", result.Artifacts.Dir)
	}
	if len(provider.fileCalls) != 0 {
		t.Fatalf("file calls = %#v, want no stuffed file reads in reviewer workspace mode", provider.fileCalls)
	}
	requests := adapter.Requests()
	if len(requests) < 1 {
		t.Fatalf("adapter requests = %d, want selection request", len(requests))
	}
	selectionPrompt := requests[0].Prompt
	if !strings.Contains(selectionPrompt, reviewBaseSHA) || !strings.Contains(selectionPrompt, reviewHeadSHA) {
		t.Fatalf("selection prompt missing pinned review SHAs: %s", selectionPrompt)
	}
	if strings.Contains(selectionPrompt, provider.pr.Head.SHA) {
		t.Fatalf("selection prompt contains current PR SHAs: %s", selectionPrompt)
	}
	if requests[1].ReviewerWorkspace == nil {
		t.Fatalf("reviewer request = %#v, want reviewer workspace", requests[1])
	}
	workspace := requests[1].ReviewerWorkspace
	if !strings.Contains(workspace.RepoDir, filepath.Join("workbench", "reviewers")) ||
		!strings.HasPrefix(workspace.ScratchDir, result.Artifacts.WorkbenchScratch+string(filepath.Separator)) ||
		workspace.MaxToolOutputBytes != defaultReviewerWorkspaceToolOutputBytes {
		t.Fatalf("reviewer workspace request = %#v, want disposable repo, scratch, and default cap", workspace)
	}
	if provider.threadCalls != 0 {
		t.Fatalf("thread calls = %d, want no live thread reads for pinned review", provider.threadCalls)
	}
	if provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("review/comment calls = %d/%d, want no live discussion reads for pinned review", provider.reviewCalls, provider.issueCommentCalls)
	}
	if !containsFileCall(provider.treeCalls, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}) {
		t.Fatalf("tree calls = %#v, want repo agents loaded from current base SHA", provider.treeCalls)
	}
	if containsFileCall(provider.treeCalls, fileKey{gitRef: reviewBaseSHA, path: ".codereview/agents"}) {
		t.Fatalf("tree calls = %#v, want no repo agent load from pinned review base SHA", provider.treeCalls)
	}
	assertFileContains(t, result.Artifacts.DiffPatch, "diff --git a/main.go b/main.go")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Current PR discussion omitted because this review is pinned to explicit base/head SHAs.")
}

func TestDryRunWithPinnedReviewSHAsRejectsForkHeads(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ReviewBaseSHA = strings.Repeat("1", 40)
	req.ReviewHeadSHA = strings.Repeat("2", 40)
	provider.pr.Head.Owner = "fork-owner"
	provider.pr.Head.Repo = "codereview-cli-fork"

	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-pinned-fork" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !strings.Contains(err.Error(), "does not support fork PR heads") {
		t.Fatalf("DryRun error = %v, want clear fork-head rejection", err)
	}
	if len(provider.diffBetweenCalls) != 0 {
		t.Fatalf("diff between calls = %#v, want rejection before compare", provider.diffBetweenCalls)
	}
}

func TestDryRunSelectionPromptInstructionsStayInsideStructuredPayload(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SelectionPromptInstructions = "Prefer applies_when over prompt wording when routing."
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	if _, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-selection-instructions" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) == 0 {
		t.Fatal("adapter requests = 0, want selection request")
	}
	selectionPrompt := requests[0].Prompt
	if !strings.Contains(selectionPrompt, `"selection_instructions": "Prefer applies_when over prompt wording when routing."`) {
		t.Fatalf("selection prompt missing custom instruction field: %s", selectionPrompt)
	}
	if !strings.Contains(selectionPrompt, `"task": "`+defaultSelectionTask+`"`) {
		t.Fatalf("selection prompt missing stable task field: %s", selectionPrompt)
	}
	if !strings.Contains(selectionPrompt, `"output_contract"`) || !strings.Contains(selectionPrompt, `"schema": "selection"`) {
		t.Fatalf("selection prompt missing structured contract fields: %s", selectionPrompt)
	}
}

func TestSelectionOnlyRunsSingleSelectionPhaseWithoutReviewArtifacts(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
	}}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Open thread at main.go:2"}}), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	expectedArtifacts := ArtifactPathsFromDir(artifactDir)
	if !reflect.DeepEqual(result.Artifacts, expectedArtifacts) {
		t.Fatalf("artifacts = %#v, want %#v", result.Artifacts, expectedArtifacts)
	}
	if len(adapter.Requests()) != 2 {
		t.Fatalf("adapter requests = %d, want dossier summary + selection only", len(adapter.Requests()))
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter resumes = %#v, want none", adapter.Resumes())
	}
	expectedPRKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	if result.PRKey != expectedPRKey {
		t.Fatalf("PRKey = %q, want %q", result.PRKey, expectedPRKey)
	}
	if !reflect.DeepEqual(result.PR, provider.pr) {
		t.Fatalf("PR = %#v, want %#v", result.PR, provider.pr)
	}
	if len(result.Catalog.Agents) != 1 || result.Catalog.Agents[0].ID != "harness:reviewer" {
		t.Fatalf("catalog agents = %#v, want harness:reviewer", result.Catalog.Agents)
	}
	if len(result.Selection.SelectedAgents) != 1 || result.Selection.SelectedAgents[0].AgentID != "harness:reviewer" {
		t.Fatalf("selection = %#v, want harness:reviewer", result.Selection)
	}
	if len(result.ParsedDiff.Patches) != 1 || result.ParsedDiff.Patches[0].Path != "main.go" {
		t.Fatalf("parsed diff = %#v, want main.go patch", result.ParsedDiff.Patches)
	}
	if !reflect.DeepEqual(result.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("changed files = %#v, want main.go", result.ChangedFiles)
	}
	if len(result.Threads) != 1 || result.Threads[0].ID != "thread-1" {
		t.Fatalf("threads = %#v, want thread-1", result.Threads)
	}
	wantCaps := reviewplan.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true}
	if !reflect.DeepEqual(result.EffectiveCaps, wantCaps) {
		t.Fatalf("EffectiveCaps = %#v, want %#v", result.EffectiveCaps, wantCaps)
	}
	if result.AgentDefsChanged {
		t.Fatal("AgentDefsChanged = true, want false")
	}
	if result.CurrentBaseSHA != provider.pr.Base.SHA || result.CurrentHeadSHA != provider.pr.Head.SHA ||
		result.ReviewBaseSHA != provider.pr.Base.SHA || result.ReviewHeadSHA != provider.pr.Head.SHA {
		t.Fatalf("result SHAs = current %s/%s review %s/%s, want provider PR SHAs", result.CurrentBaseSHA, result.CurrentHeadSHA, result.ReviewBaseSHA, result.ReviewHeadSHA)
	}
	if result.SelectionSession.ProviderSessionID != "selection-session" || result.SelectionSession.Model != "claude-sonnet-4-6" || result.SelectionSession.Effort != "medium" {
		t.Fatalf("selection session = %#v, want selection-session claude-sonnet-4-6/medium", result.SelectionSession)
	}
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "final/change-map.md")
	expectedLog, err := expectedArtifacts.AgentLog("orchestrator-selection")
	if err != nil {
		t.Fatalf("AgentLog: %v", err)
	}
	request := adapter.Requests()[1]
	if request.LogPath != expectedLog {
		t.Fatalf("selection log path = %q, want %q", request.LogPath, expectedLog)
	}
	if info, err := os.Stat(expectedArtifacts.AgentLogsDir); err != nil || !info.IsDir() {
		t.Fatalf("agent logs dir stat = (%v, %v), want existing dir", info, err)
	}
	if _, err := os.Stat(result.Artifacts.FindingsJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("findings artifact stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(result.Artifacts.RollupMarkdown); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollup artifact stat error = %v, want not exist", err)
	}
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "main.go:2")
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "raw/pr-context.json")
}

func TestSelectionOnlyAllowsThreadActionsWithThreadContext(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	provider.threads = []gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-1", "main.go", 2, req.PostingIdentity, human, "Cached settled summary"),
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [{
			"agent_id": "harness:reviewer",
			"rationale": "go file changed",
			"files": ["main.go"]
		}],
		"thread_actions": [{
			"thread_id": "thread-1",
			"decision": "summarize_only",
			"summary": "Thread remains settled"
		}],
		"reasoning": "select reviewer and keep cached thread context"
	}`, 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	if len(result.Selection.ThreadActions) != 1 {
		t.Fatalf("thread actions = %#v, want one", result.Selection.ThreadActions)
	}
	got := result.Selection.ThreadActions[0]
	if got.ThreadID != "thread-1" || got.Decision != review.ThreadDecisionSummarizeOnly || got.Summary != "Thread remains settled" {
		t.Fatalf("thread action = %#v, want decoded action for normalized thread context", got)
	}
}

func TestSelectionOnlyPromptPreservesRoutingContractWithoutReviewerPromptBodies(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
	}}
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	req.SelectionPromptInstructions = "Prefer applies_when over prompt wording when routing."
	provider.diff.Raw = largeDiff("main.go", "+selection_prompt_should_not_embed_this_unique_hunk_line\n") + smallDiff("other.go")
	provider.pr.Body = "Selection prompt body should stay out of prompt payloads."
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Open thread at main.go:2"}}), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:alpha", "main.go"), 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 2 {
		t.Fatalf("adapter requests = %d, want dossier summary + selection only", len(requests))
	}
	selectionPrompt := requests[1].Prompt
	var payload struct {
		Task                  string                   `json:"task"`
		Schema                string                   `json:"schema"`
		SelectionInstructions string                   `json:"selection_instructions"`
		OutputContract        map[string]any           `json:"output_contract"`
		Agents                []selectionAgentPrompt   `json:"agents"`
		ChangedFiles          []string                 `json:"changed_files"`
		Dossier               selectionPromptDossier   `json:"dossier"`
		Workbench             selectionPromptWorkbench `json:"workbench"`
		Threads               []selectionThreadPrompt  `json:"threads"`
	}
	if err := json.Unmarshal([]byte(selectionPrompt), &payload); err != nil {
		t.Fatalf("unmarshal selection prompt: %v", err)
	}
	if payload.SelectionInstructions != "Prefer applies_when over prompt wording when routing." {
		t.Fatalf("selection instructions = %q, want custom instructions", payload.SelectionInstructions)
	}
	if payload.Task != defaultSelectionTask || payload.Schema != "selection" || payload.OutputContract == nil {
		t.Fatalf("selection prompt envelope = %#v, want task/schema/output contract", payload)
	}
	if !reflect.DeepEqual(payload.ChangedFiles, []string{"main.go", "other.go"}) {
		t.Fatalf("changed files = %#v, want main.go/other.go", payload.ChangedFiles)
	}
	if len(payload.Threads) != 1 || payload.Threads[0].ThreadID != "thread-1" || payload.Threads[0].Path != "main.go" || payload.Threads[0].Summary != "Open thread at main.go:2" {
		t.Fatalf("threads = %#v, want thread-1 on main.go", payload.Threads)
	}
	if !strings.Contains(payload.Dossier.PRIntent, provider.pr.Title) || !strings.Contains(payload.Dossier.PRIntent, provider.pr.Body) {
		t.Fatalf("pr intent = %q, want title and PR body", payload.Dossier.PRIntent)
	}
	if !strings.Contains(payload.Dossier.ChangeMap, "main.go") || !strings.Contains(payload.Dossier.Discussion, "Open thread at main.go:2") {
		t.Fatalf("dossier payload = %#v, want change map and summarized discussion", payload.Dossier)
	}
	if payload.Workbench.Head.SHA != provider.pr.Head.SHA || payload.Workbench.Base.SHA != provider.pr.Base.SHA {
		t.Fatalf("workbench payload = %#v, want review head/base SHAs", payload.Workbench)
	}
	if payload.Workbench.CheckoutMode == "" || payload.Workbench.PR.Number != provider.pr.Ref.Number {
		t.Fatalf("workbench payload = %#v, want checkout mode and PR identity", payload.Workbench)
	}
	if len(payload.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(payload.Agents))
	}
	wantAgents := map[string][]string{
		"harness:alpha": {"Go files changed"},
		"harness:beta":  {"Go files changed"},
	}
	for _, agent := range payload.Agents {
		wantAppliesWhen, ok := wantAgents[agent.ID]
		if !ok {
			t.Fatalf("unexpected selection agent %#v", agent)
		}
		if !reflect.DeepEqual(agent.AppliesWhen, wantAppliesWhen) {
			t.Fatalf("agent applies_when = %#v, want %#v", agent.AppliesWhen, wantAppliesWhen)
		}
		delete(wantAgents, agent.ID)
	}
	if len(wantAgents) != 0 {
		t.Fatalf("missing selection agents = %#v", wantAgents)
	}
	for _, forbidden := range []string{"Review alpha files.", "Review beta files.", `"prompt"`, `"provenance"`, `"overridden"`} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked reviewer execution detail %q: %s", forbidden, selectionPrompt)
		}
	}
	for _, forbidden := range []string{"diff --git", "@@ -", "@@ +"} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked diff hunk content %q: %s", forbidden, selectionPrompt)
		}
	}
	if strings.Contains(selectionPrompt, "selection_prompt_should_not_embed_this_unique_hunk_line") {
		t.Fatalf("selection prompt leaked raw patch content: %s", selectionPrompt)
	}
	for _, forbidden := range []string{result.Artifacts.WorkbenchRepoDir, result.Artifacts.WorkbenchScratch, result.Artifacts.DossierDir, `"source_repo_root"`, `"repo_path"`, `"scratch_path"`, `"metadata_path"`, `"metadata_sha256"`, `"fingerprint_inputs"`, `"root_dir"`, `"index_path"`, `"index_sha256"`} {
		if strings.Contains(selectionPrompt, forbidden) {
			t.Fatalf("selection prompt leaked harness-only workbench detail %q: %s", forbidden, selectionPrompt)
		}
	}
}

func TestSelectionOnlyReusesCachedDossierSummaryTask(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-1",
		Body:   "Top-level concern",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	artifactDir := t.TempDir()

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Compact concern"}, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session-1", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		TaskProgress:    firstProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly first run: %v", err)
	}
	if len(firstProgress.starts) != 2 ||
		firstProgress.starts[0].TaskID != dossierSummaryTaskID || firstProgress.starts[0].Phase != "dossier" ||
		firstProgress.starts[1].TaskID != orchestratorSelectionStage || firstProgress.starts[1].Phase != "selection" {
		t.Fatalf("first progress starts = %#v, want dossier summary and selection execute", firstProgress.starts)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         secondAdapter,
		TaskProgress:    secondProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly second run: %v", err)
	}
	if len(secondProgress.loads) != 2 ||
		secondProgress.loads[0].event.TaskID != dossierSummaryTaskID || secondProgress.loads[0].event.Phase != "dossier" ||
		secondProgress.loads[1].event.TaskID != orchestratorSelectionStage || secondProgress.loads[1].event.Phase != "selection" {
		t.Fatalf("second progress loads = %#v, want cached dossier summary and selection loads", secondProgress.loads)
	}
	if secondProgress.loads[0].result.Usage.TokensIn == nil || *secondProgress.loads[0].result.Usage.TokensIn != 8 ||
		secondProgress.loads[0].result.Usage.TokensOut == nil || *secondProgress.loads[0].result.Usage.TokensOut != 2 {
		t.Fatalf("cached dossier summary usage = %#v, want metadata usage", secondProgress.loads[0].result.Usage)
	}
	if secondProgress.loads[1].result.Usage.TokensIn == nil || *secondProgress.loads[1].result.Usage.TokensIn != 10 ||
		secondProgress.loads[1].result.Usage.TokensOut == nil || *secondProgress.loads[1].result.Usage.TokensOut != 2 {
		t.Fatalf("cached selection usage = %#v, want metadata usage", secondProgress.loads[1].result.Usage)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter requests = %d, want cached dossier summary and selection loads", len(secondAdapter.Requests()))
	}

	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-2",
		Body:   "Changed concern should invalidate caller-owned cache",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	thirdAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	thirdAdapter.Queue(fakeLLMResult("dossier-summary-session-2", discussionSummaryJSON([]string{"Updated concern"}, nil), 8, 2))
	thirdAdapter.Queue(fakeLLMResult("selection-session-3", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	thirdProgress := &fakeTaskProgress{}
	if _, err := selectionOnlyForTest(ctx, Options{
		Provider:        provider,
		Adapter:         thirdAdapter,
		TaskProgress:    thirdProgress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, selectionRequestFromReview(req, artifactDir)); err != nil {
		t.Fatalf("SelectionOnly third run: %v", err)
	}
	if len(thirdProgress.starts) != 2 ||
		thirdProgress.starts[0].TaskID != dossierSummaryTaskID || thirdProgress.starts[0].Phase != "dossier" ||
		thirdProgress.starts[1].TaskID != orchestratorSelectionStage || thirdProgress.starts[1].Phase != "selection" {
		t.Fatalf("third progress starts = %#v, want dossier summary and selection rerun after caller-owned artifact change", thirdProgress.starts)
	}
	if len(thirdAdapter.Requests()) != 2 {
		t.Fatalf("third adapter requests = %d, want dossier summary rerun plus selection", len(thirdAdapter.Requests()))
	}
}

func TestPrepareDossierArtifactsUsesSummaryForFinalDiscussionAndInvalidatesOnDiscussionChange(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-summary", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	sessionIDs := sequence("session")

	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:    provider.pr,
		ReviewPR:     provider.pr,
		ChangedFiles: parseDiffPatchesForTest(t, provider.diff.Raw),
		IssueComments: []gitprovider.IssueComment{{
			ID:     "issue-1",
			Body:   "Very long raw concern that should not appear in the final summary output.",
			Author: gitprovider.Identity{Login: "maintainer"},
		}},
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Compact concern"}, nil), 8, 2))
	firstProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         firstAdapter,
		Store:           store,
		TaskProgress:    firstProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts first run: %v", err)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Compact concern")
	assertFileOmits(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Very long raw concern")
	meta, ok, err := readLLMTaskMetadata(artifacts, dossierSummaryTaskID)
	if err != nil || !ok {
		t.Fatalf("read dossier summary metadata = ok %v err %v", ok, err)
	}
	if meta.Status != llmTaskStatusSucceeded || meta.SessionRowID == "" || meta.ProviderSessionID != "dossier-summary-session" {
		t.Fatalf("dossier summary metadata = %#v, want succeeded run-backed task metadata", meta)
	}

	secondProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		TaskProgress:    secondProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts second run: %v", err)
	}
	if len(secondProgress.loads) != 1 || secondProgress.loads[0].event.TaskID != dossierSummaryTaskID {
		t.Fatalf("second progress loads = %#v, want cached dossier summary load", secondProgress.loads)
	}

	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:    provider.pr,
		ReviewPR:     provider.pr,
		ChangedFiles: parseDiffPatchesForTest(t, provider.diff.Raw),
		IssueComments: []gitprovider.IssueComment{{
			ID:     "issue-2",
			Body:   "A changed concern should invalidate the cached summary.",
			Author: gitprovider.Identity{Login: "maintainer"},
		}},
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts updated: %v", err)
	}
	thirdAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	thirdAdapter.Queue(fakeLLMResult("dossier-summary-session-2", discussionSummaryJSON([]string{"Updated concern"}, nil), 8, 2))
	thirdProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         thirdAdapter,
		Store:           store,
		TaskProgress:    thirdProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts third run: %v", err)
	}
	if len(thirdProgress.starts) != 1 || thirdProgress.starts[0].TaskID != dossierSummaryTaskID {
		t.Fatalf("third progress starts = %#v, want dossier summary rerun after raw discussion change", thirdProgress.starts)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Updated concern")
}

func TestPrepareDossierArtifactsInvalidatesSummaryWhenInlineThreadChangesAndPreservesAnchors(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-inline-thread", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	sessionIDs := sequence("session")

	writeThreads := func(threads []gitprovider.InlineThread) {
		if err := writeRawDossierArtifacts(artifacts, dossierInputs{
			CurrentPR:      provider.pr,
			ReviewPR:       provider.pr,
			ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
			Threads:        threads,
			Catalog:        agents.Catalog{},
			CurrentBaseSHA: provider.pr.Base.SHA,
			CurrentHeadSHA: provider.pr.Head.SHA,
		}); err != nil {
			t.Fatalf("writeRawDossierArtifacts: %v", err)
		}
	}

	writeThreads([]gitprovider.InlineThread{{
		ID:          "thread-1",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
		Resolved:    false,
		Comments: []gitprovider.ThreadComment{{
			Body:   "First thread body",
			Author: gitprovider.Identity{Login: "reviewer"},
		}},
	}})
	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{
		path:       "main.go",
		side:       string(review.DiffSideRight),
		line:       2,
		anchorKind: string(review.AnchorKindLine),
		resolved:   false,
		status:     "unresolved",
		summary:    "Thread summary",
	}}), 8, 2))
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         firstAdapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts first run: %v", err)
	}
	var summary dossierDiscussionSummaryArtifact
	if err := readJSONFile(filepath.Join(artifacts.DossierDir, "summary", "discussion.json"), &summary); err != nil {
		t.Fatalf("read summary discussion: %v", err)
	}
	if len(summary.InlineThreads) != 1 {
		t.Fatalf("summary inline threads = %#v, want one entry", summary.InlineThreads)
	}
	got := summary.InlineThreads[0]
	if got.Path != "main.go" || got.Side != string(review.DiffSideRight) || got.Line != 2 || got.AnchorKind != string(review.AnchorKindLine) || got.Resolved {
		t.Fatalf("summary inline thread = %#v, want preserved line anchor on main.go:2 unresolved", got)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "main.go:2 [RIGHT] {line} Unresolved: Thread summary")

	writeThreads([]gitprovider.InlineThread{{
		ID:          "thread-1",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        3,
		SubjectType: review.AnchorKindLine,
		Resolved:    true,
		Comments: []gitprovider.ThreadComment{{
			Body:   "Changed thread body",
			Author: gitprovider.Identity{Login: "reviewer"},
		}},
	}})
	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondAdapter.Queue(fakeLLMResult("dossier-summary-session-2", discussionSummaryJSON(nil, []threadSummary{{
		path:       "main.go",
		side:       string(review.DiffSideRight),
		line:       3,
		anchorKind: string(review.AnchorKindLine),
		resolved:   true,
		status:     "settled",
		summary:    "Updated thread summary",
	}}), 8, 2))
	secondProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         secondAdapter,
		Store:           store,
		TaskProgress:    secondProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts second run: %v", err)
	}
	if len(secondProgress.starts) != 1 || secondProgress.starts[0].TaskID != dossierSummaryTaskID {
		t.Fatalf("second progress starts = %#v, want dossier summary rerun after inline thread change", secondProgress.starts)
	}
	if err := readJSONFile(filepath.Join(artifacts.DossierDir, "summary", "discussion.json"), &summary); err != nil {
		t.Fatalf("read updated summary discussion: %v", err)
	}
	got = summary.InlineThreads[0]
	if got.Line != 3 || got.AnchorKind != string(review.AnchorKindLine) || !got.Resolved {
		t.Fatalf("updated summary inline thread = %#v, want preserved line 3 resolved anchor", got)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "main.go:3 [RIGHT] {line} Settled: Updated thread summary")
}

func TestPrepareDossierArtifactsInvalidatesSummaryWhenOmittedOrTruncatedDiscussionChanges(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-fingerprint", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	sessionIDs := sequence("session")

	writeComments := func(bodies []string) {
		comments := make([]gitprovider.IssueComment, 0, len(bodies))
		for i, body := range bodies {
			comments = append(comments, gitprovider.IssueComment{
				ID:     gitprovider.CommentID(fmt.Sprintf("issue-%d", i+1)),
				Body:   body,
				Author: gitprovider.Identity{Login: "maintainer"},
			})
		}
		if err := writeRawDossierArtifacts(artifacts, dossierInputs{
			CurrentPR:      provider.pr,
			ReviewPR:       provider.pr,
			ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
			IssueComments:  comments,
			Catalog:        agents.Catalog{},
			CurrentBaseSHA: provider.pr.Base.SHA,
			CurrentHeadSHA: provider.pr.Head.SHA,
		}); err != nil {
			t.Fatalf("writeRawDossierArtifacts: %v", err)
		}
	}

	bodies := make([]string, 0, dossierSummaryMaxTopLevel+1)
	for i := 0; i < dossierSummaryMaxTopLevel; i++ {
		bodies = append(bodies, fmt.Sprintf("Visible concern %02d", i))
	}
	bodies = append(bodies, "Omitted concern v1")
	writeComments(bodies)

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Initial summary"}, nil), 8, 2))
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         firstAdapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts first run: %v", err)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Additional top-level comments omitted: 1")

	bodies[len(bodies)-1] = "Omitted concern v2"
	writeComments(bodies)
	omittedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	omittedAdapter.Queue(fakeLLMResult("dossier-summary-session-2", discussionSummaryJSON([]string{"Summary after omitted change"}, nil), 8, 2))
	omittedProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         omittedAdapter,
		Store:           store,
		TaskProgress:    omittedProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts omitted-change run: %v", err)
	}
	if len(omittedProgress.starts) != 1 || omittedProgress.starts[0].TaskID != dossierSummaryTaskID {
		t.Fatalf("omitted-change progress starts = %#v, want dossier summary rerun", omittedProgress.starts)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Summary after omitted change")

	longBody := strings.Repeat("a", dossierSummaryExcerptRunes) + "tail-v1"
	writeComments([]string{longBody})
	truncatedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	truncatedAdapter.Queue(fakeLLMResult("dossier-summary-session-3", discussionSummaryJSON([]string{"Summary after truncated change"}, nil), 8, 2))
	truncatedProgress := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         truncatedAdapter,
		Store:           store,
		TaskProgress:    truncatedProgress,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts truncated baseline run: %v", err)
	}
	writeComments([]string{strings.Repeat("a", dossierSummaryExcerptRunes) + "tail-v2"})
	truncatedAdapter2 := &llm.FakeAdapter{NameValue: "fake-llm"}
	truncatedAdapter2.Queue(fakeLLMResult("dossier-summary-session-4", discussionSummaryJSON([]string{"Summary after truncated tail change"}, nil), 8, 2))
	truncatedProgress2 := &fakeTaskProgress{}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         truncatedAdapter2,
		Store:           store,
		TaskProgress:    truncatedProgress2,
		Now:             fixedNow,
		NewSessionRowID: sessionIDs,
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts truncated-tail run: %v", err)
	}
	if len(truncatedProgress2.starts) != 1 || truncatedProgress2.starts[0].TaskID != dossierSummaryTaskID {
		t.Fatalf("truncated-tail progress starts = %#v, want dossier summary rerun", truncatedProgress2.starts)
	}
	assertFileContains(t, filepath.Join(artifacts.DossierDir, "final", "discussion.md"), "Summary after truncated tail change")
}

func TestPrepareDossierArtifactsRendersInlineThreadOmittedCounts(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-thread-omits", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	threads := make([]gitprovider.InlineThread, 0, dossierSummaryMaxInlineThreads+1)
	for i := 0; i < dossierSummaryMaxInlineThreads+1; i++ {
		threads = append(threads, gitprovider.InlineThread{
			ID:          gitprovider.ThreadID(fmt.Sprintf("thread-%d", i+1)),
			Path:        "main.go",
			Side:        review.DiffSideRight,
			Line:        i + 1,
			SubjectType: review.AnchorKindLine,
			Comments: []gitprovider.ThreadComment{
				{Body: "first", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "second", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "third", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "fourth", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "fifth", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "sixth", Author: gitprovider.Identity{Login: "reviewer"}},
			},
		})
	}
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:      provider.pr,
		ReviewPR:       provider.pr,
		ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
		Threads:        threads,
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{
		path:     "main.go",
		line:     1,
		status:   "unresolved",
		summary:  "Thread summary",
		resolved: false,
	}}), 8, 2))
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         adapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	discussionPath := filepath.Join(artifacts.DossierDir, "final", "discussion.md")
	assertFileContains(t, discussionPath, "additional thread comments omitted: 1")
	assertFileContains(t, discussionPath, "Additional inline threads omitted: 1")
}

func TestPrepareDossierArtifactsSummaryPromptCarriesSplitDiscussionShape(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-prompt-shape", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:    provider.pr,
		ReviewPR:     provider.pr,
		ChangedFiles: parseDiffPatchesForTest(t, provider.diff.Raw),
		IssueComments: []gitprovider.IssueComment{{
			ID:     "issue-1",
			Body:   "Top-level concern body",
			Author: gitprovider.Identity{Login: "maintainer"},
		}},
		Threads: []gitprovider.InlineThread{{
			ID:          "thread-1",
			Path:        "main.go",
			Side:        review.DiffSideRight,
			Line:        2,
			SubjectType: review.AnchorKindLine,
			Resolved:    false,
			Comments: []gitprovider.ThreadComment{
				{Body: "one", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "two", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "three", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "four", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "five", Author: gitprovider.Identity{Login: "reviewer"}},
				{Body: "six", Author: gitprovider.Identity{Login: "reviewer"}},
			},
		}},
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Compact concern"}, []threadSummary{{
		path:       "main.go",
		side:       string(review.DiffSideRight),
		line:       2,
		anchorKind: string(review.AnchorKindLine),
		status:     "unresolved",
		summary:    "Thread summary",
	}}), 8, 2))
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         adapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %#v, want one dossier summary request", requests)
	}
	var payload struct {
		Task       string `json:"task"`
		Schema     string `json:"schema"`
		Provenance struct {
			SourceFingerprint string `json:"source_fingerprint"`
		} `json:"provenance"`
		Discussion dossierDiscussionPromptInput `json:"discussion"`
	}
	if err := json.Unmarshal([]byte(requests[0].Prompt), &payload); err != nil {
		t.Fatalf("unmarshal dossier summary prompt: %v", err)
	}
	if payload.Task == "" || payload.Schema != "discussion_summary" || payload.Provenance.SourceFingerprint == "" {
		t.Fatalf("prompt payload = %#v, want task/schema/provenance", payload)
	}
	if len(payload.Discussion.TopLevelComments) != 1 || payload.Discussion.TopLevelComments[0].UntrustedBody != "Top-level concern body" {
		t.Fatalf("top-level prompt payload = %#v, want raw top-level body", payload.Discussion.TopLevelComments)
	}
	if len(payload.Discussion.InlineThreads) != 1 {
		t.Fatalf("inline thread prompt payload = %#v, want one thread", payload.Discussion.InlineThreads)
	}
	thread := payload.Discussion.InlineThreads[0]
	if thread.Path != "main.go" || thread.Side != string(review.DiffSideRight) || thread.Line != 2 || thread.AnchorKind != string(review.AnchorKindLine) || thread.Resolved {
		t.Fatalf("thread prompt payload = %#v, want preserved inline anchor context", thread)
	}
	if len(thread.Comments) != dossierSummaryMaxThreadComments || thread.CommentsOmitted != 1 {
		t.Fatalf("thread prompt comments = %#v, want capped comments plus omitted count", thread)
	}
}

func TestPrepareDossierArtifactsReusesCRSettledThreadSummaryWithoutLLM(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-cr-settled", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	bot := req.PostingIdentity
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	threads := []gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-1", "main.go", 2, bot, human, "Cached settled summary"),
	}
	threadContext, err := threadcontext.Normalize(threads, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:      provider.pr,
		ReviewPR:       provider.pr,
		ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
		Threads:        threads,
		ThreadContext:  threadContext,
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         adapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want cached dossier summary without LLM", len(adapter.Requests()))
	}
	var summary dossierDiscussionSummaryArtifact
	if err := readJSONFile(filepath.Join(artifacts.DossierDir, "summary", "discussion.json"), &summary); err != nil {
		t.Fatalf("read summary discussion: %v", err)
	}
	if len(summary.InlineThreads) != 1 {
		t.Fatalf("summary inline threads = %#v, want one cached entry", summary.InlineThreads)
	}
	got := summary.InlineThreads[0]
	if got.ThreadID != "thread-1" || got.Resolved || got.Status != "settled" || got.Summary != "Cached settled summary" {
		t.Fatalf("cached summary = %#v, want thread-1 unresolved settled cached summary", got)
	}
	discussionPath := filepath.Join(artifacts.DossierDir, "final", "discussion.md")
	assertFileContains(t, discussionPath, "main.go:2 [RIGHT] {line} Settled: Cached settled summary")
	assertFileOmits(t, discussionPath, "Original finding")
}

func TestWriteRawDossierArtifactsMarksCachedSummarySource(t *testing.T) {
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-cached-summary-source", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	bot := req.PostingIdentity
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	providerResolved := crSettledReviewThread(t, "thread-provider", "main.go", 2, bot, human, "Provider summary")
	providerResolved.Resolved = true
	threads := []gitprovider.InlineThread{
		providerResolved,
		crSettledReviewThread(t, "thread-cr-settled", "main.go", 4, bot, human, "CR-settled summary"),
	}
	threadContext, err := threadcontext.Normalize(threads, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:      provider.pr,
		ReviewPR:       provider.pr,
		ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
		Threads:        threads,
		ThreadContext:  threadContext,
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	var rawThreads []dossierInlineThreadArtifact
	if err := readJSONFile(mustDossierRawPath(artifacts, "inline-threads.json"), &rawThreads); err != nil {
		t.Fatalf("read raw inline threads: %v", err)
	}
	got := map[string]string{}
	for _, thread := range rawThreads {
		if thread.CachedSummary != nil {
			got[thread.ID] = thread.CachedSummary.Source
		}
	}
	want := map[string]string{
		"thread-provider":   dossierCachedSummaryProviderSource,
		"thread-cr-settled": dossierCachedSummaryCRSource,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached summary sources = %#v, want %#v", got, want)
	}
}

func TestPrepareDossierArtifactsKeepsSameAnchorCachedSummariesByThreadID(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-cr-settled-same-anchor", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	bot := req.PostingIdentity
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	threads := []gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-a", "main.go", 2, bot, human, "First cached summary"),
		crSettledReviewThread(t, "thread-b", "main.go", 2, bot, human, "Second cached summary"),
	}
	threadContext, err := threadcontext.Normalize(threads, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:      provider.pr,
		ReviewPR:       provider.pr,
		ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
		Threads:        threads,
		ThreadContext:  threadContext,
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	var summary dossierDiscussionSummaryArtifact
	if err := readJSONFile(filepath.Join(artifacts.DossierDir, "summary", "discussion.json"), &summary); err != nil {
		t.Fatalf("read summary discussion: %v", err)
	}
	if len(summary.InlineThreads) != 2 {
		t.Fatalf("summary inline threads = %#v, want both same-anchor cached entries", summary.InlineThreads)
	}
	got := map[string]string{}
	for _, thread := range summary.InlineThreads {
		got[thread.ThreadID] = thread.Summary
	}
	want := map[string]string{"thread-a": "First cached summary", "thread-b": "Second cached summary"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached summaries = %#v, want %#v", got, want)
	}
}

func TestPrepareDossierArtifactsIncludesCachedSummaryBeyondInlineThreadCap(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-cr-settled-beyond-cap", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	bot := req.PostingIdentity
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	threads := make([]gitprovider.InlineThread, 0, dossierSummaryMaxInlineThreads+1)
	for i := 0; i < dossierSummaryMaxInlineThreads; i++ {
		threads = append(threads, gitprovider.InlineThread{
			ID:          gitprovider.ThreadID(fmt.Sprintf("thread-open-%02d", i)),
			Path:        "a.go",
			Side:        review.DiffSideRight,
			Line:        i + 1,
			SubjectType: review.AnchorKindLine,
			Comments: []gitprovider.ThreadComment{{
				Body:      fmt.Sprintf("open thread %02d", i),
				Author:    human,
				Path:      "a.go",
				Side:      review.DiffSideRight,
				Line:      i + 1,
				CreatedAt: fixedNow(),
				UpdatedAt: fixedNow(),
			}},
		})
	}
	threads = append(threads, crSettledReviewThread(t, "thread-cached", "z.go", 99, bot, human, "Cached beyond cap"))
	threadContext, err := threadcontext.Normalize(threads, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:      provider.pr,
		ReviewPR:       provider.pr,
		ChangedFiles:   parseDiffPatchesForTest(t, provider.diff.Raw),
		Threads:        threads,
		ThreadContext:  threadContext,
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{
		path:     "a.go",
		line:     1,
		status:   "unresolved",
		summary:  "Open thread summary",
		resolved: false,
	}}), 8, 2))
	if err := prepareDossierArtifacts(ctx, Options{
		Adapter:         adapter,
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	var summary dossierDiscussionSummaryArtifact
	if err := readJSONFile(filepath.Join(artifacts.DossierDir, "summary", "discussion.json"), &summary); err != nil {
		t.Fatalf("read summary discussion: %v", err)
	}
	var found bool
	for _, thread := range summary.InlineThreads {
		if thread.ThreadID == "thread-cached" && thread.Summary == "Cached beyond cap" && thread.Status == "settled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("summary inline threads = %#v, want cached thread beyond cap", summary.InlineThreads)
	}
}

func TestSelectionThreadPromptsFromContextUsesCRSettledSummary(t *testing.T) {
	bot := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	threads, err := threadcontext.Normalize([]gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-1", "main.go", 2, bot, human, "Cached settled summary"),
	}, threadcontext.Options{PostingIdentity: bot})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	prompts := selectionThreadPromptsFromContext(threads, dossierDiscussionSummaryArtifact{})
	if len(prompts) != 1 {
		t.Fatalf("selection thread prompts = %#v, want one prompt", prompts)
	}
	got := prompts[0]
	if got.ThreadID != "thread-1" || got.Resolved || got.Status != "settled" || got.Summary != "Cached settled summary" {
		t.Fatalf("selection thread prompt = %#v, want unresolved settled cached summary", got)
	}
}

func TestPrepareDossierArtifactsSummaryPromptBudgetFailure(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-dossier-budget", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	if err := writeRawDossierArtifacts(artifacts, dossierInputs{
		CurrentPR:    provider.pr,
		ReviewPR:     provider.pr,
		ChangedFiles: parseDiffPatchesForTest(t, provider.diff.Raw),
		IssueComments: []gitprovider.IssueComment{{
			ID:     "issue-1",
			Body:   "Top-level concern that should exceed an intentionally tiny prompt budget.",
			Author: gitprovider.Identity{Login: "maintainer"},
		}},
		Catalog:        agents.Catalog{},
		CurrentBaseSHA: provider.pr.Base.SHA,
		CurrentHeadSHA: provider.pr.Head.SHA,
	}); err != nil {
		t.Fatalf("writeRawDossierArtifacts: %v", err)
	}
	err := prepareDossierArtifacts(ctx, Options{
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		Budget:          ContextBudget{MaxPromptBytes: 10},
	}, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: artifacts,
	})
	if err == nil || !strings.Contains(err.Error(), "dossier discussion summary prompt budget") {
		t.Fatalf("prepareDossierArtifacts error = %v, want dossier summary prompt budget failure", err)
	}
}

func TestDecodeDossierDiscussionSummaryRejectsProcessState(t *testing.T) {
	promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{})
	if err != nil {
		t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
	}
	for _, text := range []string{"CI status is red", "Build failed in CI", "Approved by alice", "run_id=1234"} {
		_, err := decodeDossierDiscussionSummary([]byte(fmt.Sprintf(`{
			"schema_version": 1,
			"top_level_comments": [{"summary": %q}]
		}`, text)), promptData)
		if err == nil || !strings.Contains(err.Error(), "excluded reviewer-facing process state") {
			t.Fatalf("decodeDossierDiscussionSummary(%q) error = %v, want excluded process state", text, err)
		}
	}
}

func TestDecodeDossierDiscussionSummaryRejectsUnknownAnchor(t *testing.T) {
	promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{
		InlineThreads: []dossierInlineThreadArtifact{{
			Path:       "main.go",
			Side:       "RIGHT",
			Line:       2,
			AnchorKind: "line",
			Resolved:   true,
		}},
	})
	if err != nil {
		t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
	}
	_, err = decodeDossierDiscussionSummary([]byte(`{
		"schema_version": 1,
		"inline_threads": [{
			"path": "other.go",
			"side": "RIGHT",
			"line": 2,
			"anchor_kind": "line",
			"summary": "Moved thread"
		}]
	}`), promptData)
	if err == nil || !strings.Contains(err.Error(), "is not present in the source discussion") {
		t.Fatalf("decodeDossierDiscussionSummary error = %v, want source anchor rejection", err)
	}
}

func TestDecodeDossierDiscussionSummaryThreadIDWinsOverMismatchedAnchor(t *testing.T) {
	promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{
		InlineThreads: []dossierInlineThreadArtifact{{
			ID:         "thread-1",
			Path:       "main.go",
			Side:       "RIGHT",
			Line:       2,
			AnchorKind: "line",
			Resolved:   false,
			Comments: []dossierThreadCommentArtifact{{
				Author: "review-bot",
				Body:   "Original thread body",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
	}
	got, err := decodeDossierDiscussionSummary([]byte(`{
		"schema_version": 1,
		"inline_threads": [{
			"thread_id": "thread-1",
			"path": "other.go",
			"side": "LEFT",
			"line": 99,
			"anchor_kind": "file",
			"status": "unresolved",
			"summary": "Thread summary"
		}]
	}`), promptData)
	if err != nil {
		t.Fatalf("decodeDossierDiscussionSummary: %v", err)
	}
	if len(got.InlineThreads) != 1 {
		t.Fatalf("inline threads = %#v, want one", got.InlineThreads)
	}
	thread := got.InlineThreads[0]
	if thread.ThreadID != "thread-1" || thread.Path != "main.go" || thread.Side != "RIGHT" || thread.Line != 2 || thread.AnchorKind != "line" || thread.Resolved {
		t.Fatalf("decoded thread = %#v, want source thread fields selected by thread_id", thread)
	}
}

func TestSelectionOnlyRejectsInvalidSelection(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("missing:agent", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("selection-session-retry", selectionJSON("missing:agent", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err == nil || !strings.Contains(err.Error(), "structured output invalid after retry") || !strings.Contains(err.Error(), "unknown selected agent") {
		t.Fatalf("SelectionOnly error = %v, want retry-wrapped unknown selected agent", err)
	}
	if !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("SelectionOnly error = %v, want %v", err, ErrStructuredOutputInvalidAfterRetry)
	}
	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if result.SelectionSession.ProviderSessionID != "selection-session-retry" {
		t.Fatalf("selection session = %#v, want retry session id", result.SelectionSession)
	}
	if got := string(result.SelectionSession.Response.StructuredOutput); !strings.Contains(got, `"missing:agent"`) {
		t.Fatalf("selection response = %q, want raw invalid retry payload", got)
	}
	if !reflect.DeepEqual(result.Selection, llm.Selection{}) {
		t.Fatalf("selection = %#v, want zero value on invalid output", result.Selection)
	}
	requests := adapter.Requests()
	if len(requests) != 2 {
		t.Fatalf("adapter requests = %#v, want initial start plus retry", requests)
	}
	if !strings.Contains(requests[1].Prompt, "failed validation") || !strings.Contains(requests[1].Prompt, "unknown selected agent") {
		t.Fatalf("retry prompt = %q, want validation retry details", requests[1].Prompt)
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter resumes = %#v, want none", adapter.Resumes())
	}
}

func TestSelectionOnlyRequiresPostingIdentity(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	selectionReq := selectionRequestFromReview(req, t.TempDir())
	selectionReq.PostingIdentity = gitprovider.Identity{}

	_, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Now:      fixedNow,
	}, selectionReq)
	if err == nil || !strings.Contains(err.Error(), "selection posting identity is required") {
		t.Fatalf("SelectionOnly error = %v, want posting identity required", err)
	}
}

func TestSelectionOnlyCapsMaxAgents(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	var warnings bytes.Buffer
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [
			{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]},
			{"agent_id":"harness:beta","rationale":"main","files":["main.go"]}
		],
		"thread_actions": [],
		"reasoning": "too many"
	}`, 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider:  provider,
		Adapter:   adapter,
		Now:       fixedNow,
		MaxAgents: 1,
		Warnings:  &warnings,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	if len(result.Selection.SelectedAgents) != 1 || result.Selection.SelectedAgents[0].AgentID != "harness:alpha" {
		t.Fatalf("selected agents = %#v, want first selected agent only", result.Selection.SelectedAgents)
	}
	if got := warnings.String(); !strings.Contains(got, "orchestrator selected 2 agents; using first 1 due to max-agents") {
		t.Fatalf("warnings = %q, want max-agent cap warning", got)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %#v, want one selection request", requests)
	}
	if !strings.Contains(requests[0].Prompt, `"max_selected_agents": 1`) {
		t.Fatalf("selection prompt = %q, want max_selected_agents", requests[0].Prompt)
	}
}

func TestSelectionOnlyContextBudgetFailure(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	_, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
		Budget:   ContextBudget{MaxPromptBytes: 100},
	}, selectionRequestFromReview(req, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for selection model claude-sonnet-4-6") {
		t.Fatalf("SelectionOnly error = %v, want selection budget failure", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want no LLM call after budget failure", adapter.Requests())
	}
}

func TestSelectionOnlyNoDiffSkipsLLMAndReturnsPreparedContext(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if len(result.ParsedDiff.Patches) != 0 || len(result.ChangedFiles) != 0 {
		t.Fatalf("parsed diff = %#v changed files = %#v, want empty", result.ParsedDiff.Patches, result.ChangedFiles)
	}
	if !reflect.DeepEqual(result.Selection, llm.Selection{}) || !reflect.DeepEqual(result.SelectionSession, SelectionSession{}) {
		t.Fatalf("selection result = %#v session = %#v, want zero values", result.Selection, result.SelectionSession)
	}
	if len(result.Catalog.Agents) != 1 || result.Catalog.Agents[0].ID != "harness:reviewer" {
		t.Fatalf("catalog agents = %#v, want harness:reviewer", result.Catalog.Agents)
	}
}

func TestPrepareWorkbenchArtifactsCreatesCleanPinnedCheckoutAndMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.baseSHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchRepoDir, "main.go"), []byte("workspace"), 0o600); err != nil {
		t.Fatalf("write workbench file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchRepoDir, "new-file.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatalf("write new workbench file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifacts.WorkbenchScratch, "note.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	var meta workbenchMetadataArtifact
	if err := readJSONFile(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.SchemaVersion != workbenchMetadataSchemaVersion || meta.CheckoutMode != workbenchCheckoutModeArtifactClone {
		t.Fatalf("metadata = %#v, want schema %d and checkout mode %q", meta, workbenchMetadataSchemaVersion, workbenchCheckoutModeArtifactClone)
	}
	if meta.Base.SHA != fixture.baseSHA || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("metadata refs = %#v/%#v, want base/head fixture SHAs", meta.Base, meta.Head)
	}
	if meta.PR != (workbenchPRIdentity{Host: fixture.pr.Ref.Host, Owner: fixture.pr.Ref.Owner, Repo: fixture.pr.Ref.Repo, Number: fixture.pr.Ref.Number}) {
		t.Fatalf("metadata PR = %#v, want fixture PR identity", meta.PR)
	}
	if meta.SourceRepoRoot != fixture.repoDir {
		t.Fatalf("metadata source repo root = %q, want %q", meta.SourceRepoRoot, fixture.repoDir)
	}
	if meta.RepoPath != artifacts.WorkbenchRepoDir || meta.ScratchPath != artifacts.WorkbenchScratch {
		t.Fatalf("metadata paths = repo %q scratch %q, want artifact workbench paths", meta.RepoPath, meta.ScratchPath)
	}
	if !reflect.DeepEqual(meta.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("metadata changed files = %#v, want main.go", meta.ChangedFiles)
	}
	if meta.FingerprintInputs.PR != meta.PR ||
		meta.FingerprintInputs.BaseSHA != fixture.baseSHA ||
		meta.FingerprintInputs.HeadSHA != fixture.headSHA ||
		meta.FingerprintInputs.CheckoutMode != workbenchCheckoutModeArtifactClone ||
		meta.FingerprintInputs.SourceRepoRoot != fixture.repoDir ||
		!reflect.DeepEqual(meta.FingerprintInputs.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("fingerprint inputs = %#v, want deterministic metadata inputs", meta.FingerprintInputs)
	}
}

func TestDeriveWorkbenchRemoteURLPreservesRemoteStyle(t *testing.T) {
	branch := gitprovider.PRBranchRef{Host: "github.com", Owner: "fork-owner", Repo: "codereview-cli"}

	scpURL, err := deriveWorkbenchRemoteURL("git@github.com:open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive scp remote: %v", err)
	}
	if scpURL != "git@github.com:fork-owner/codereview-cli.git" {
		t.Fatalf("scp remote = %q, want fork-style scp URL", scpURL)
	}

	httpsURL, err := deriveWorkbenchRemoteURL("https://github.com/open-cli-collective/codereview-cli.git", branch)
	if err != nil {
		t.Fatalf("derive https remote: %v", err)
	}
	if httpsURL != "https://github.com/fork-owner/codereview-cli.git" {
		t.Fatalf("https remote = %q, want fork-style https URL", httpsURL)
	}
}

func TestPrepareWorkbenchArtifactsFetchesForkHeadFromDerivedRemote(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	var fetchedRemotes []string
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			fetchedRemotes = append(fetchedRemotes, cmdArgs[2])
			switch cmdArgs[2] {
			case "git@github.com:open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			case "git@github.com:fork-owner/codereview-cli-fork.git":
				cmdArgs[2] = fixture.forkRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
		}
		return out, nil
	}

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.pr.Head.SHA {
		t.Fatalf("workbench HEAD = %q, want fork head %q", got, fixture.pr.Head.SHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.pr.Base.SHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	if !slices.Contains(fetchedRemotes, "git@github.com:fork-owner/codereview-cli-fork.git") {
		t.Fatalf("fetched remotes = %#v, want derived fork remote fetch", fetchedRemotes)
	}
}

func TestPrepareWorkbenchArtifactsRejectsMismatchedBaseHostEvenWhenCommitsExistLocally(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	pr := fixture.pr
	pr.Base.Host = "example.com"
	pr.Ref.Host = "example.com"

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}, workbenchPreparationRequest{
		PRRef:        pr.Ref,
		ReviewPR:     pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err == nil {
		t.Fatal("prepareWorkbenchArtifacts unexpectedly succeeded for mismatched base host")
	}
	if !strings.Contains(err.Error(), `source repo origin "git@github.com:open-cli-collective/codereview-cli.git" does not match PR base repo open-cli-collective/codereview-cli on example.com`) {
		t.Fatalf("prepareWorkbenchArtifacts error = %v, want host mismatch", err)
	}
}

func TestPrepareWorkbenchArtifactsRejectsUnsafeFetchRef(t *testing.T) {
	ctx := context.Background()
	fixture := newForkWorkbenchFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	pr := fixture.pr
	pr.Head.Ref = "--upload-pack=/tmp/pwn"
	gitRunner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" {
			switch cmdArgs[2] {
			case "git@github.com:open-cli-collective/codereview-cli.git":
				cmdArgs[2] = fixture.baseRemotePath
			case "git@github.com:fork-owner/codereview-cli-fork.git":
				cmdArgs[2] = fixture.forkRemotePath
			}
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
		}
		return out, nil
	}

	err := prepareWorkbenchArtifacts(ctx, Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.sourceRepoDir, nil },
		GitCommand:      gitRunner,
	}, workbenchPreparationRequest{
		PRRef:        pr.Ref,
		ReviewPR:     pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	})
	if err == nil || !strings.Contains(err.Error(), `reject unsafe fetch ref "--upload-pack=/tmp/pwn"`) {
		t.Fatalf("prepareWorkbenchArtifacts error = %v, want unsafe ref rejection", err)
	}
}

func TestPrepareWorkbenchArtifactsRefreshesExistingArtifactRoot(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	req := workbenchPreparationRequest{
		PRRef:        fixture.pr.Ref,
		ReviewPR:     fixture.pr,
		ChangedFiles: []string{"main.go"},
		Artifacts:    artifacts,
	}
	opts := Options{
		ResolveRepoRoot: func(context.Context) (string, error) { return fixture.repoDir, nil },
	}

	if err := prepareWorkbenchArtifacts(ctx, opts, req); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts first run: %v", err)
	}
	stalePath := filepath.Join(artifacts.WorkbenchDir, "stale.txt")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale artifact: %v", err)
	}
	if err := os.WriteFile(artifacts.WorkbenchMetadataPath(), []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatalf("overwrite stale metadata: %v", err)
	}

	if err := prepareWorkbenchArtifacts(ctx, opts, req); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts second run: %v", err)
	}

	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale artifact stat error = %v, want not exist", err)
	}
	var meta workbenchMetadataArtifact
	if err := readJSONFile(artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read refreshed metadata: %v", err)
	}
	if meta.SchemaVersion != workbenchMetadataSchemaVersion || meta.Head.SHA != fixture.headSHA {
		t.Fatalf("refreshed metadata = %#v, want current workbench metadata", meta)
	}
}

func TestSelectionOnlyPreparesWorkbenchInCallerOwnedArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newWorkbenchGitFixture(t)
	provider, req := dryRunHarness(t)
	provider.pr = fixture.pr
	provider.diff = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.PRRef = fixture.pr.Ref
	req.PRURL = fixture.pr.URL
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
		ResolveRepoRoot: func(context.Context) (string, error) {
			return fixture.repoDir, nil
		},
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if _, err := os.Stat(result.Artifacts.WorkbenchMetadataPath()); err != nil {
		t.Fatalf("stat workbench metadata: %v", err)
	}
}

func TestDryRunPreparesWorkbenchInAllocatedRunArtifacts(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	fixture := newWorkbenchGitFixture(t)
	provider, req := dryRunHarness(t)
	provider.pr = fixture.pr
	provider.diff = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.PRRef = fixture.pr.Ref
	req.PRURL = fixture.pr.URL
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`, 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", nil), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Store:    store,
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:      fixedNow,
		ResolveRepoRoot: func(context.Context) (string, error) {
			return fixture.repoDir, nil
		},
		NewRunID:        func() string { return "run-workbench" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "rev-parse", "HEAD")); got != fixture.headSHA {
		t.Fatalf("workbench HEAD = %q, want %q", got, fixture.headSHA)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, result.Artifacts.WorkbenchRepoDir, "diff", "--name-only", fixture.baseSHA+"...HEAD")); got != "main.go" {
		t.Fatalf("workbench diff names = %q, want main.go", got)
	}
	var meta workbenchMetadataArtifact
	if err := readJSONFile(result.Artifacts.WorkbenchMetadataPath(), &meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.RepoPath != result.Artifacts.WorkbenchRepoDir || meta.ScratchPath != result.Artifacts.WorkbenchScratch {
		t.Fatalf("metadata paths = %#v, want allocated run workbench paths", meta)
	}
}

func TestReviewerWorkspaceSmokeAllowsReadAndWorkspaceWrites(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	adapter := &reviewerWorkspaceSmokeAdapter{}
	opts := Options{Provider: provider, Adapter: adapter}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	llmReq, cleanup, err := buildReviewerWorkspaceRequest(ctx, Options{Adapter: adapter, GitCommand: opts.GitCommand}, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", []string{"main.go"}, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("buildReviewerWorkspaceRequest: %v", err)
	}
	defer func() {
		if cleanup != nil {
			if err := cleanup(); err != nil {
				t.Fatalf("cleanup reviewer workspace: %v", err)
			}
		}
	}()
	type smokeResult struct {
		ReadOK              bool `json:"read_ok"`
		MainContainsChanged bool `json:"main_contains_changed"`
		OutOfScopeReadable  bool `json:"out_of_scope_readable"`
		TrackedWriteOK      bool `json:"tracked_write_ok"`
		UntrackedWriteOK    bool `json:"untracked_write_ok"`
		ScratchWriteOK      bool `json:"scratch_write_ok"`
		MaxToolOutputBytes  int  `json:"max_tool_output_bytes"`
	}
	got, _, err := llm.RunStructured(context.Background(), adapter, llmReq, func(data []byte) (smokeResult, error) {
		var out smokeResult
		return out, json.Unmarshal(data, &out)
	})
	if err != nil {
		t.Fatalf("RunStructured: %v", err)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want one smoke invocation", len(requests))
	}
	if requests[0].ReviewerWorkspace == nil {
		t.Fatalf("reviewer workspace request = nil")
	}
	if requests[0].ReviewerWorkspace.RepoDir == artifacts.WorkbenchRepoDir {
		t.Fatalf("reviewer workspace repo = canonical workbench repo, want disposable workspace")
	}
	if requests[0].ReviewerWorkspace.MaxToolOutputBytes != defaultReviewerWorkspaceToolOutputBytes {
		t.Fatalf("max tool output bytes = %d, want default %d", requests[0].ReviewerWorkspace.MaxToolOutputBytes, defaultReviewerWorkspaceToolOutputBytes)
	}
	if !got.ReadOK || !got.MainContainsChanged || !got.OutOfScopeReadable || !got.TrackedWriteOK || !got.UntrackedWriteOK || !got.ScratchWriteOK {
		t.Fatalf("smoke result = %#v, want checkout read success plus workspace and scratch writes", got)
	}
	if canonicalMain, err := os.ReadFile(filepath.Join(artifacts.WorkbenchRepoDir, "main.go")); err != nil || strings.Contains(string(canonicalMain), "mutated") { // #nosec G304 -- test reads only caller-produced workbench path.
		t.Fatalf("canonical workbench main.go = %q err %v, want unchanged", canonicalMain, err)
	}
	if status := strings.TrimSpace(gitCommandOutput(t, artifacts.WorkbenchRepoDir, "status", "--porcelain")); status != "" {
		t.Fatalf("canonical workbench status = %q, want clean after reviewer writes", status)
	}
}

func TestReviewerWorkspaceAllowedFilesPreservesRealCheckout(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup reviewer workspace: %v", err)
		}
	}()
	if workspace.RepoDir == artifacts.WorkbenchRepoDir {
		t.Fatalf("repo dir = %q, want disposable checkout distinct from workbench repo", workspace.RepoDir)
	}
	if len(workspace.AllowedFiles) != 1 || workspace.AllowedFiles[0] != "main.go" {
		t.Fatalf("allowed files = %#v, want main.go", workspace.AllowedFiles)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")); err != nil { // #nosec G304 -- test reads only caller-produced scope path.
		t.Fatalf("ReadFile(main.go): %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(artifacts.WorkbenchRepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced workbench path.
		t.Fatalf("ReadFile(workbench other.go): %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced workspace path.
		t.Fatalf("ReadFile(workspace other.go): %v", err)
	}
}

func TestReviewerWorkspaceAllowedFilesResetsWorkspace(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:scope-reset", []string{"main.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(first): %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup first reviewer workspace: %v", err)
	}
	workspace, cleanup, err = prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, "harness:scope-reset", []string{"other.go"}, 1024)
	if err != nil {
		t.Fatalf("prepareReviewerWorkspace(second): %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup second reviewer workspace: %v", err)
		}
	}()
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go")); err != nil { // #nosec G304 -- test reads only caller-produced scope path.
		t.Fatalf("ReadFile(other.go): %v", err)
	}
	if len(workspace.AllowedFiles) != 1 || workspace.AllowedFiles[0] != "other.go" {
		t.Fatalf("allowed files = %#v, want other.go", workspace.AllowedFiles)
	}
	if _, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")); err != nil { // #nosec G304 -- test reads only caller-produced workspace path.
		t.Fatalf("ReadFile(main.go): %v", err)
	}
}

func TestReviewerWorkspaceAllowedFilesRejectsSymlinkTargets(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "real.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(real.go): %v", err)
	}
	if err := os.Symlink(filepath.Join(repo, "real.go"), filepath.Join(repo, "link.go")); err != nil {
		t.Fatalf("Symlink(link.go): %v", err)
	}
	err := validateReviewerWorkspaceFileTarget(filepath.Join(repo, "link.go"), "link.go")
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("validateReviewerWorkspaceFileTarget error = %v, want symlink rejection", err)
	}
}

func TestReviewerWorkspaceAllowedFilesRejectsEscapePathsAndCleansUp(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	opts := Options{Provider: provider, Adapter: &reviewerWorkspaceSmokeAdapter{}}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}

	for _, path := range []string{"../main.go", filepath.Join(t.TempDir(), "main.go")} {
		agentID := "harness:escape-" + statepaths.Encode(path)
		workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, prepared.reviewPR.Head.SHA, agentID, []string{path}, 1024)
		if err == nil {
			if cleanup != nil {
				_ = cleanup()
			}
			t.Fatalf("prepareReviewerWorkspace(%q) = %#v, nil error; want rejection", path, workspace)
		}
		if cleanup != nil {
			t.Fatalf("prepareReviewerWorkspace(%q) cleanup = non-nil, want nil on setup failure", path)
		}
		encoded := statepaths.Encode(agentID)
		if _, statErr := os.Stat(filepath.Join(artifacts.WorkbenchDir, "reviewers", encoded)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reviewer workspace for %q stat err = %v, want cleaned", path, statErr)
		}
		if _, statErr := os.Stat(filepath.Join(artifacts.WorkbenchScratch, encoded)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reviewer scratch for %q stat err = %v, want cleaned", path, statErr)
		}
	}
}

func TestBuildReviewerWorkspaceRequestUnsupportedAdapterFails(t *testing.T) {
	artifacts := ArtifactPathsFromDir(t.TempDir())
	req, cleanup, err := buildReviewerWorkspaceRequest(context.Background(), Options{Adapter: &llm.FakeAdapter{NameValue: "fake-unsupported", ReviewerWorkspaceModeSet: true}}, artifacts, strings.Repeat("1", 40), "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "reviewer workspace capability") {
		t.Fatalf("buildReviewerWorkspaceRequest error = %v, want missing reviewer workspace capability", err)
	}
	if cleanup != nil {
		t.Fatalf("cleanup is non-nil, want nil")
	}
	if req.Model != "" || req.Effort != "" || req.Prompt != "" || req.LogPath != "" || req.ReviewerWorkspace != nil || req.OnValidationRetry != nil {
		t.Fatalf("request = %#v, want zero request on unsupported adapter", req)
	}
}

func TestBuildReviewerWorkspaceRequestAcceptsPermissionBoundedAdapter(t *testing.T) {
	ctx := context.Background()
	provider, reviewReq := dryRunHarness(t)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	adapter := &llm.FakeAdapter{
		NameValue:                  "fake-bounded",
		ReviewerWorkspaceModeSet:   true,
		ReviewerWorkspaceModeValue: llm.ReviewerWorkspacePermissionBounded,
	}
	opts := Options{Provider: provider, Adapter: adapter}
	configureWorkbenchFixtureForTest(ctx, &opts, reviewReq.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         reviewReq.PRRef,
		Profile:       reviewReq.Profile,
		AgentDirs:     reviewReq.AgentDirs,
		ReviewBaseSHA: reviewReq.ReviewBaseSHA,
		ReviewHeadSHA: reviewReq.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return artifacts, nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        reviewReq.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	req, cleanup, err := buildReviewerWorkspaceRequest(ctx, Options{Adapter: adapter, GitCommand: opts.GitCommand}, artifacts, prepared.reviewPR.Head.SHA, "harness:smoke", nil, "gpt-5.5", "medium", "smoke", filepath.Join(t.TempDir(), "smoke.jsonl"))
	if err != nil {
		t.Fatalf("buildReviewerWorkspaceRequest: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup reviewer workspace: %v", err)
		}
	}()
	if req.ReviewerWorkspace == nil {
		t.Fatalf("ReviewerWorkspace = nil")
	}
	if req.ReviewerWorkspace.RepoDir == artifacts.WorkbenchRepoDir || !strings.HasPrefix(req.ReviewerWorkspace.ScratchDir, artifacts.WorkbenchScratch+string(filepath.Separator)) {
		t.Fatalf("ReviewerWorkspace = %#v, want disposable repo and scratch", req.ReviewerWorkspace)
	}
}

func TestDryRunNoDiffDoesNotResolveUnmappedModelTier(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.Profile.LLM = config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthAPIKey,
		Adapter:  config.LLMAdapterAnthropicAPI,
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-no-diff-unmapped" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	if result.Plan.Outcome != reviewplan.OutcomeNothingToReview {
		t.Fatalf("Plan.Outcome = %q, want %q", result.Plan.Outcome, reviewplan.OutcomeNothingToReview)
	}
}

func TestDryRunAgentModelTierUsesProfileModelMapOverride(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"medium": "profile-medium-model"}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-map-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	for _, request := range adapter.Requests() {
		if request.Model != "profile-medium-model" || request.Effort != "medium" {
			t.Fatalf("request = model:%q effort:%q, want profile-medium-model/medium", request.Model, request.Effort)
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	for _, session := range sessions {
		if session.Model != "profile-medium-model" {
			t.Fatalf("session.Model = %q, want profile-medium-model", session.Model)
		}
	}
}

func TestDryRunReviewerBaselineTierRaisesReviewerModelFloor(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	req.Profile.LLM.ReviewerModelTier = config.ModelTierLarge
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-baseline-large" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "profile-large-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "large",
		EffectiveTier:  "large",
		ResolvedModel:  "profile-large-model",
		ModelMapSource: config.ModelMapSourceConfig,
	})
}

func TestDryRunSelectionOverridesApplyOnlyToSelection(t *testing.T) {
	tests := []struct {
		name           string
		modelOverride  string
		effortOverride string
		wantModels     []string
		wantEfforts    []string
	}{
		{
			name:           "model and effort",
			modelOverride:  "bench-model",
			effortOverride: "high",
			wantModels:     []string{"bench-model", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:    []string{"high", "medium", "medium"},
		},
		{
			name:          "model only",
			modelOverride: "bench-model",
			wantModels:    []string{"bench-model", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:   []string{"medium", "medium", "medium"},
		},
		{
			name:           "effort only",
			effortOverride: "high",
			wantModels:     []string{"claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:    []string{"high", "medium", "medium"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.SelectionModelOverride = tt.modelOverride
			req.SelectionEffortOverride = tt.effortOverride
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
			adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
			adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

			result, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return "run-override-" + strings.ReplaceAll(tt.name, " ", "-") },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}

			requests := adapter.Requests()
			if len(requests) != 3 {
				t.Fatalf("requests len = %d, want selection/reviewer/rollup", len(requests))
			}
			for i, request := range requests {
				if request.Model != tt.wantModels[i] || request.Effort != tt.wantEfforts[i] {
					t.Fatalf("request[%d] = model:%q effort:%q, want %s/%s", i, request.Model, request.Effort, tt.wantModels[i], tt.wantEfforts[i])
				}
			}
			sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
			if err != nil {
				t.Fatalf("ListSessionsForRun: %v", err)
			}
			if len(sessions) != 3 {
				t.Fatalf("sessions len = %d, want selection/reviewer/rollup", len(sessions))
			}
			for i, session := range sessions {
				if session.Model != tt.wantModels[i] || session.Effort == nil || *session.Effort != tt.wantEfforts[i] {
					t.Fatalf("session[%d] = model:%q effort:%v, want %s/%s", i, session.Model, session.Effort, tt.wantModels[i], tt.wantEfforts[i])
				}
			}
			data, err := os.ReadFile(result.Artifacts.AgentSourcesJSON) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", result.Artifacts.AgentSourcesJSON, err)
			}
			if tt.modelOverride != "" && strings.Contains(string(data), tt.modelOverride) {
				t.Fatalf("agent source artifact contains runtime override model: %s", data)
			}
			assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
		})
	}
}

func TestDryRunReviewerOverridesApplyOnlyToReviewers(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ReviewerModelOverride = "bench-reviewer-model"
	req.ReviewerEffortOverride = "low"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "bench-reviewer-model", "claude-sonnet-4-6"}
	wantEfforts := []string{"medium", "low", "medium"}
	requests := adapter.Requests()
	for i, request := range requests {
		if request.Model != wantModels[i] || request.Effort != wantEfforts[i] {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/%s", i, request.Model, request.Effort, wantModels[i], wantEfforts[i])
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	for i, session := range sessions {
		if session.Model != wantModels[i] || session.Effort == nil || *session.Effort != wantEfforts[i] {
			t.Fatalf("session[%d] = model:%q effort:%v, want %s/%s", i, session.Model, session.Effort, wantModels[i], wantEfforts[i])
		}
	}
}

func TestDryRunReviewerFailureIsolation(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgent(t, req.Profile.AgentSources[0], "harness", "alpha", "alpha desc", "Review alpha.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "beta", "beta desc", "Review beta.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "gamma", "gamma desc", "Review gamma.")
	adapter := &reviewerIsolationAdapter{supportsResume: true, reviewerBarrier: newReviewerStartBarrier(3)}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-isolation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  3,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2 successful reviewer findings", len(result.Findings))
	}
	if len(result.ReviewerFailures) != 1 || result.ReviewerFailures[0].AgentID != "harness:beta" {
		t.Fatalf("reviewer failures = %#v findings = %#v, want isolated beta failure", result.ReviewerFailures, result.Findings)
	}
	if len(result.Sessions) != 5 {
		t.Fatalf("sessions len = %d, want selection, alpha, beta retry, gamma, rollup", len(result.Sessions))
	}
	betaMeta, ok, err := readLLMTaskMetadata(result.Artifacts, reviewerTaskID("harness:beta"))
	if err != nil || !ok {
		t.Fatalf("read beta task metadata = ok %v err %v", ok, err)
	}
	if betaMeta.Status != llmTaskStatusFailedIsolated || len(betaMeta.Attempts) != 2 {
		t.Fatalf("beta metadata = %#v, want failed_isolated with initial and retry attempts", betaMeta)
	}
	if !adapter.BetaRetrySawCleanWorkspace() {
		t.Fatalf("beta retry reused dirty reviewer workspace; want clean workspace for validation retry")
	}
	for _, attempt := range betaMeta.Attempts {
		if attempt.DecodeError == "" {
			t.Fatalf("beta attempt %#v missing decode error", attempt)
		}
		assertTaskPayloadContains(t, attempt.RawOutputPath, `"agent_id": "harness:beta"`)
	}
	for _, agentID := range []string{"harness:alpha", "harness:gamma"} {
		meta, ok, err := readLLMTaskMetadata(result.Artifacts, reviewerTaskID(agentID))
		if err != nil || !ok {
			t.Fatalf("read %s task metadata = ok %v err %v", agentID, ok, err)
		}
		if meta.Status != llmTaskStatusSucceeded {
			t.Fatalf("%s metadata status = %q, want succeeded", agentID, meta.Status)
		}
		assertTaskPayloadContains(t, meta.ValidatedOutputPath, agentID)
	}
	if got := adapter.ReviewerStartedCount(); got != 3 {
		t.Fatalf("reviewer starts = %d, want all three reviewers to start before release", got)
	}
	for _, agentID := range []string{"harness:alpha", "harness:beta", "harness:gamma"} {
		encoded := statepaths.Encode(agentID)
		if _, err := os.Stat(filepath.Join(result.Artifacts.WorkbenchDir, "reviewers", encoded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reviewer workspace %s stat err = %v, want cleaned", agentID, err)
		}
		if _, err := os.Stat(filepath.Join(result.Artifacts.WorkbenchScratch, encoded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reviewer scratch %s stat err = %v, want cleaned", agentID, err)
		}
	}
	requests := adapter.Requests()
	if len(requests) != 6 {
		t.Fatalf("requests len = %d, want selection, three reviewers, beta retry, rollup", len(requests))
	}
	rollupPrompt := requests[len(requests)-1].Prompt
	if !strings.Contains(rollupPrompt, `"reviewer_failures"`) || !strings.Contains(rollupPrompt, `"agent_id": "harness:beta"`) {
		t.Fatalf("rollup prompt missing isolated reviewer failure context: %s", rollupPrompt)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("plan outcome = %q, want comment with isolated reviewer failure", result.Plan.Outcome)
	}
	if !strings.Contains(result.Plan.RollupMarkdown, "### Reviewer Diagnostics") || !strings.Contains(result.Plan.RollupMarkdown, "harness:beta") {
		t.Fatalf("rollup markdown missing reviewer diagnostic:\n%s", result.Plan.RollupMarkdown)
	}
}

func TestDryRunReviewerProviderFailureIsolation(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgent(t, req.Profile.AgentSources[0], "harness", "alpha", "alpha desc", "Review alpha.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "beta", "beta desc", "Review beta.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "gamma", "gamma desc", "Review gamma.")
	betaErr := errors.New("provider wait failed")
	adapter := &reviewerIsolationAdapter{betaProviderErr: betaErr, reviewerBarrier: newReviewerStartBarrier(3)}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-provider-isolation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  3,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2 successful reviewer findings", len(result.Findings))
	}
	if len(result.ReviewerFailures) != 1 || result.ReviewerFailures[0].AgentID != "harness:beta" {
		t.Fatalf("reviewer failures = %#v, want isolated beta failure", result.ReviewerFailures)
	}
	betaMeta, ok, err := readLLMTaskMetadata(result.Artifacts, reviewerTaskID("harness:beta"))
	if err != nil || !ok {
		t.Fatalf("read beta task metadata = ok %v err %v", ok, err)
	}
	if betaMeta.Status != llmTaskStatusFailedIsolated || betaMeta.ProviderSessionID != "beta-provider-session" {
		t.Fatalf("beta metadata = %#v, want isolated failure with provider session", betaMeta)
	}
	if len(betaMeta.Attempts) != 0 {
		t.Fatalf("beta attempts = %#v, want none for provider wait failure", betaMeta.Attempts)
	}
	if !strings.Contains(betaMeta.Error, "provider wait failed") {
		t.Fatalf("beta error = %q, want provider diagnostic", betaMeta.Error)
	}
	for _, agentID := range []string{"harness:alpha", "harness:gamma"} {
		meta, ok, err := readLLMTaskMetadata(result.Artifacts, reviewerTaskID(agentID))
		if err != nil || !ok {
			t.Fatalf("read %s task metadata = ok %v err %v", agentID, ok, err)
		}
		assertTaskPayloadContains(t, meta.ValidatedOutputPath, agentID)
	}
	if got := adapter.ReviewerStartedCount(); got != 3 {
		t.Fatalf("reviewer starts = %d, want all three reviewers to start before release", got)
	}
	requests := adapter.Requests()
	if len(requests) != 5 {
		t.Fatalf("requests len = %d, want selection, three reviewers, rollup", len(requests))
	}
	rollupPrompt := requests[len(requests)-1].Prompt
	if !strings.Contains(rollupPrompt, `"reviewer_failures"`) || !strings.Contains(rollupPrompt, `"agent_id": "harness:beta"`) {
		t.Fatalf("rollup prompt missing isolated reviewer failure context: %s", rollupPrompt)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("plan outcome = %q, want comment with isolated reviewer failure", result.Plan.Outcome)
	}
}

func TestLiveResumeCompletedSelectionAndReviewersRerunsOnlyRollup(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	if err := store.UpsertNamedSession(ctx, namedSessionForRequest(req, "stored-session")); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	run := allocateLiveRun(t, store, provider, req, "run-rollup-resume")
	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	firstAdapter.Queue(fakeLLMResult("rollup-retry-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	findingID := func() (review.FindingID, error) { return review.FindingID("finding-1"), nil }

	_, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingID,
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err == nil || !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("first Live error = %v, want invalid rollup after retry", err)
	}
	stored, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("run outcome = %#v, want incomplete after rollup task failure", stored.Outcome)
	}
	selectionMeta, ok, err := readLLMTaskMetadata(ArtifactPathsFromDir(run.ArtifactPath), orchestratorSelectionStage)
	if err != nil || !ok || selectionMeta.Status != llmTaskStatusSucceeded {
		t.Fatalf("selection metadata = %#v ok %v err %v, want succeeded", selectionMeta, ok, err)
	}
	reviewerMeta, ok, err := readLLMTaskMetadata(ArtifactPathsFromDir(run.ArtifactPath), reviewerTaskID("harness:reviewer"))
	if err != nil || !ok || reviewerMeta.Status != llmTaskStatusSucceeded {
		t.Fatalf("reviewer metadata = %#v ok %v err %v, want succeeded", reviewerMeta, ok, err)
	}
	rollupMeta, ok, err := readLLMTaskMetadata(ArtifactPathsFromDir(run.ArtifactPath), orchestratorRollupStage)
	if err != nil || !ok || rollupMeta.Status != llmTaskStatusFailedBlocking {
		t.Fatalf("rollup metadata = %#v ok %v err %v, want failed_blocking", rollupMeta, ok, err)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	secondAdapter.Queue(fakeLLMResult("rollup-fixed-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         secondAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingID,
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("second Live: %v", err)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want no fresh selection/reviewer/rollup start", secondAdapter.Requests())
	}
	resumes := secondAdapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "rollup-retry-session" {
		t.Fatalf("second adapter resumes = %#v, want only failed rollup retry session", resumes)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("result findings = %#v, want loaded reviewer finding", result.Findings)
	}
	if result.Rollup.ReviewEvent != review.ReviewEventComment {
		t.Fatalf("rollup = %#v, want rerun rollup result", result.Rollup)
	}
	if len(result.PlannedActions) == 0 {
		t.Fatal("planned actions len = 0, want resumed pipeline to synthesize actions")
	}
}

func TestRunStructuredTaskRejectsAdapterMismatchBeforeRetry(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	spec := llmTaskSpec{
		runID:            "run-adapter-mismatch",
		taskID:           orchestratorRollupStage,
		phase:            "rollup",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleOrchestrator,
		model:            "model",
		effort:           "medium",
		prompt:           "prompt",
	}
	meta := llmTaskMetadata{
		SchemaVersion:     llmTaskSchemaVersion,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		InputFingerprint:  spec.inputFingerprint,
		Adapter:           "old-llm",
		Status:            llmTaskStatusFailedBlocking,
		SessionRowID:      "session-row",
		ProviderSessionID: "old-provider-session",
	}
	if err := writeLLMTaskMetadata(artifacts, meta); err != nil {
		t.Fatalf("writeLLMTaskMetadata: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "new-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("new-session", `"ok"`, 1, 1))

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err == nil || !strings.Contains(err.Error(), `adapter = "old-llm", want "new-llm"`) {
		t.Fatalf("runStructuredTask error = %v, want adapter mismatch", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite mismatch: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestRunStructuredTaskRejectsDependencyTaskIDMismatchBeforeRetry(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	spec := llmTaskSpec{
		runID:             "run-dependency-mismatch",
		taskID:            orchestratorSelectionStage,
		phase:             "selection",
		dependencyTaskIDs: []string{dossierSummaryTaskID},
		inputFingerprint:  "fingerprint",
		artifacts:         artifacts,
		role:              ledger.SessionRoleOrchestrator,
		model:             "model",
		effort:            "medium",
		prompt:            "prompt",
	}
	meta := llmTaskMetadata{
		SchemaVersion:     llmTaskSchemaVersion,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		DependencyTaskIDs: nil,
		InputFingerprint:  spec.inputFingerprint,
		Adapter:           "fake-llm",
		Status:            llmTaskStatusFailedBlocking,
		SessionRowID:      "session-row",
		ProviderSessionID: "provider-session",
	}
	if err := writeLLMTaskMetadata(artifacts, meta); err != nil {
		t.Fatalf("writeLLMTaskMetadata: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("new-session", `"ok"`, 1, 1))

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err == nil || !strings.Contains(err.Error(), "dependency task ids") {
		t.Fatalf("runStructuredTask error = %v, want dependency task id mismatch", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite dependency mismatch: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestRunStructuredTaskReviewerStartFailureIsBlocking(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	startErr := errors.New("auth failed")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{StartErr: startErr})
	spec := llmTaskSpec{
		runID:            "run-reviewer-start-failure",
		taskID:           reviewerTaskID("harness:beta"),
		phase:            "reviewer",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleReviewer,
		model:            "model",
		effort:           "medium",
		prompt:           "prompt",
		llmFailureStatus: llmTaskStatusFailedIsolated,
	}

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if !errors.Is(err, startErr) || !errors.Is(err, errLLMTaskFailedBlocking) {
		t.Fatalf("runStructuredTask error = %v, want blocking start error wrapping %v", err, startErr)
	}
	meta, ok, readErr := readLLMTaskMetadata(artifacts, spec.taskID)
	if readErr != nil || !ok {
		t.Fatalf("read task metadata = ok %v err %v", ok, readErr)
	}
	if meta.Status != llmTaskStatusFailedBlocking || meta.ProviderSessionID != "" {
		t.Fatalf("metadata = %#v, want failed_blocking without provider session", meta)
	}
}

func TestRunStructuredTaskReportsProgressOnExecution(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	allocatePipelineRun(t, store, layout, "run-progress", ledger.PostModeDryRun, fixedNow())
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("task-session", `"ok"`, 5, 3))
	progress := &fakeTaskProgress{}
	spec := llmTaskSpec{
		runID:            "run-progress",
		taskID:           orchestratorRollupStage,
		phase:            "rollup",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleOrchestrator,
		model:            "gpt-5.5",
		effort:           "high",
		logPath:          filepath.Join(t.TempDir(), "rollup.jsonl"),
		prompt:           "prompt",
	}

	value, _, _, err := runStructuredTask[string](ctx, Options{
		Adapter:         adapter,
		Store:           store,
		TaskProgress:    progress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err != nil {
		t.Fatalf("runStructuredTask: %v", err)
	}
	if value != `"ok"` {
		t.Fatalf("value = %q, want ok payload", value)
	}
	if len(progress.starts) != 1 || len(progress.ends) != 1 {
		t.Fatalf("progress starts=%d ends=%d, want 1/1", len(progress.starts), len(progress.ends))
	}
	if progress.starts[0].TaskID != orchestratorRollupStage || progress.starts[0].Phase != "rollup" || progress.starts[0].Source != "execute" {
		t.Fatalf("start = %#v, want rollup execute event", progress.starts[0])
	}
	if progress.starts[0].Model != spec.model || progress.starts[0].Effort != spec.effort || progress.starts[0].LogPath != spec.logPath {
		t.Fatalf("start fields = %#v, want model/effort/logPath from spec", progress.starts[0])
	}
	if progress.ends[0].result.Status != string(llmTaskStatusSucceeded) || progress.ends[0].result.ProviderSessionID != "task-session" || progress.ends[0].result.Cached {
		t.Fatalf("end result = %#v, want succeeded uncached task-session", progress.ends[0].result)
	}
}

func assertTaskPayloadContains(t *testing.T, path, want string) {
	t.Helper()
	if strings.TrimSpace(path) == "" {
		t.Fatalf("task payload path is empty, want file containing %q", want)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact paths produced by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("task payload %s = %s, want %q", path, data, want)
	}
}

type fakeTaskProgress struct {
	mu     sync.Mutex
	starts []LLMTaskProgressEvent
	ends   []fakeTaskProgressEnd
	loads  []fakeTaskProgressLoad
}

type fakeTaskProgressSpan struct {
	parent *fakeTaskProgress
	event  LLMTaskProgressEvent
}

type fakeTaskProgressEnd struct {
	event  LLMTaskProgressEvent
	err    error
	result LLMTaskProgressResult
}

type fakeTaskProgressLoad struct {
	event  LLMTaskProgressEvent
	result LLMTaskProgressResult
}

func (f *fakeTaskProgress) StartLLMTask(event LLMTaskProgressEvent) LLMTaskProgressSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, event)
	return fakeTaskProgressSpan{parent: f, event: event}
}

func (f *fakeTaskProgress) LoadLLMTask(event LLMTaskProgressEvent, result LLMTaskProgressResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, fakeTaskProgressLoad{event: event, result: result})
}

func (s fakeTaskProgressSpan) End(err error, result LLMTaskProgressResult) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.parent.ends = append(s.parent.ends, fakeTaskProgressEnd{event: s.event, err: err, result: result})
}

func TestDryRunReviewerModelTierOverrideAppliesOnlyToReviewers(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	req.ReviewerModelTierOverride = "large"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-tier-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "profile-large-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "large",
		EffectiveTier:  "large",
		ResolvedModel:  "profile-large-model",
		ModelMapSource: config.ModelMapSourceConfig,
	})
}

func TestDryRunAgentModelIDBypassesModelMapForReviewer(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests len = %d, want selection/reviewer/rollup", len(requests))
	}
	wantModels := []string{"claude-sonnet-4-6", "agent-provider-model", "claude-sonnet-4-6"}
	for i, request := range requests {
		if request.Model != wantModels[i] || request.Effort != "medium" {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/medium", i, request.Model, request.Effort, wantModels[i])
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	for i, session := range sessions {
		if session.Model != wantModels[i] {
			t.Fatalf("session[%d].Model = %q, want %q", i, session.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:          "exact_model",
		ResolvedModel: "agent-provider-model",
	})
}

func TestDryRunReviewerBaselineDoesNotAffectAgentModelID(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	req.Profile.LLM.ReviewerModelTier = config.ModelTierLarge
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id-baseline" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "agent-provider-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:          "exact_model",
		ResolvedModel: "agent-provider-model",
	})
}

func TestDryRunReviewerFloorsResolveIndependentlyPerAgent(t *testing.T) {
	provider, req := dryRunHarness(t)
	_ = provider
	writeAgentWithModelTier(t, req.Profile.AgentSources[0], "harness", "senior", "large")
	catalog, err := agents.Load(context.Background(), agents.LoadOptions{
		ProfileDirs: req.Profile.AgentSources,
	})
	if err != nil {
		t.Fatalf("agents.Load: %v", err)
	}
	got := reviewerRuntimeArtifact(req, catalog, llm.Selection{
		SelectedAgents: []llm.SelectedAgent{
			{AgentID: "harness:reviewer", Files: []string{"main.go"}},
			{AgentID: "harness:senior", Files: []string{"main.go"}},
		},
	})
	if got == nil {
		t.Fatal("reviewerRuntimeArtifact = nil, want selected reviewer runtime metadata")
	}
	if runtime := got["harness:reviewer"]; runtime != (reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "small",
		EffectiveTier:  "medium",
		ResolvedModel:  "claude-sonnet-4-6",
		ModelMapSource: config.ModelMapSourceBuiltIn,
	}) {
		t.Fatalf("reviewer runtime = %#v", runtime)
	}
	if runtime := got["harness:senior"]; runtime != (reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "large",
		BaselineTier:   "small",
		EffectiveTier:  "large",
		ResolvedModel:  "claude-opus-4-8",
		ModelMapSource: config.ModelMapSourceBuiltIn,
	}) {
		t.Fatalf("senior runtime = %#v", runtime)
	}
}

func TestDryRunReviewerModelOverrideBypassesAgentModelID(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	req.ReviewerModelOverride = "override-model"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "override-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] || request.Effort != "medium" {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/medium", i, request.Model, request.Effort, wantModels[i])
		}
	}
	data, err := os.ReadFile(result.Artifacts.AgentSourcesJSON) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", result.Artifacts.AgentSourcesJSON, err)
	}
	if strings.Contains(string(data), "override-model") {
		t.Fatalf("agent source artifact contains runtime override model: %s", data)
	}
}

func TestDryRunPrunesConfiguredRetentionBeforeFetch(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldLive := allocatePipelineRun(t, store, layout, "old-live", ledger.PostModeLive, fixedNow().Add(-31*24*time.Hour))
	newLive := allocatePipelineRun(t, store, layout, "new-live", ledger.PostModeLive, fixedNow().Add(-29*24*time.Hour))
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired live run before provider GetPR error = %v, want ErrNotFound", err)
		}
		if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired dry-run before provider GetPR error = %v, want ErrNotFound", err)
		}
		if _, err := store.GetRun(ctx, newLive.RunID); err != nil {
			t.Fatalf("fresh live run before provider GetPR error = %v, want nil", err)
		}
	}

	if _, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-1" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
		Retention:       datalifecycle.RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour},
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
}

func TestDryRunManualOnlySkipsRetentionBeforeFetch(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldLive := allocatePipelineRun(t, store, layout, "old-live", ledger.PostModeLive, fixedNow().Add(-365*24*time.Hour))
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldLive.RunID); err != nil {
			t.Fatalf("live run before provider GetPR error = %v, want nil", err)
		}
		if _, err := store.GetRun(ctx, oldDryRun.RunID); err != nil {
			t.Fatalf("dry-run before provider GetPR error = %v, want nil", err)
		}
	}

	if _, err := dryRunForTest(ctx, Options{
		Provider:            provider,
		Adapter:             adapter,
		Store:               store,
		Layout:              layout,
		Now:                 fixedNow,
		NewRunID:            func() string { return "run-1" },
		NewSessionRowID:     sequence("session"),
		NewFindingID:        findingSequence("finding"),
		NewActionID:         actionSequence(),
		MaxConcurrency:      1,
		RetentionManualOnly: true,
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if _, err := store.GetRun(ctx, oldLive.RunID); err != nil {
		t.Fatalf("live run after DryRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(ctx, oldDryRun.RunID); err != nil {
		t.Fatalf("dry-run after DryRun error = %v, want nil", err)
	}
}

func TestLivePlansPendingActionsWithoutCompletingRun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.Run.RunID != run.RunID || result.Run.PostMode != ledger.PostModeLive {
		t.Fatalf("run = %#v, want supplied live run", result.Run)
	}
	storedRun, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome != nil {
		t.Fatalf("stored outcome = %#v, want incomplete until outbox", storedRun.Outcome)
	}
	if len(result.PlannedActions) != 3 {
		t.Fatalf("planned actions len = %d, want inline/rollup/submit", len(result.PlannedActions))
	}
	for _, action := range result.PlannedActions {
		if action.Status != ledger.PlannedActionPending {
			t.Fatalf("action status = %q, want pending for %#v", action.Status, action)
		}
		if strings.Contains(action.PayloadJSON, "<!-- codereview:") {
			t.Fatalf("live payload contains marker before outbox: %s", action.PayloadJSON)
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions len = %d, want selection/reviewer/rollup", len(sessions))
	}
	assertFileContains(t, result.Artifacts.RollupMarkdown, "Automated PR Review")
	assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
}

func TestLiveRejectsStageRuntimeOverrides(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "selection model", mutate: func(req *Request) { req.SelectionModelOverride = "bench-model" }},
		{name: "selection effort", mutate: func(req *Request) { req.SelectionEffortOverride = "high" }},
		{name: "selection prompt", mutate: func(req *Request) { req.SelectionPromptInstructions = "Use applies_when." }},
		{name: "reviewer model", mutate: func(req *Request) { req.ReviewerModelOverride = "bench-model" }},
		{name: "reviewer effort", mutate: func(req *Request) { req.ReviewerEffortOverride = "high" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, req := dryRunHarness(t)
			tt.mutate(&req)
			run := allocateLiveRun(t, store, provider, req, "run-live-override-"+strings.ReplaceAll(tt.name, " ", "-"))
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

			_, err := liveForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req, run)
			if err == nil {
				t.Fatal("Live error = nil, want stage override rejection")
			}
			if !strings.Contains(err.Error(), "selection and reviewer overrides require dry-run review") {
				t.Fatalf("Live error = %v, want stage override rejection", err)
			}
			if len(adapter.Requests()) != 0 {
				t.Fatalf("adapter requests = %#v, want none", adapter.Requests())
			}
		})
	}
}

func TestLiveNamedSessionResumesOrchestratorOnlyAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named")
	stored := namedSessionForRequest(req, "stored-session")
	stored.CreatedAt = fixedNow().Add(-time.Hour)
	stored.LastUsedAt = fixedNow().Add(-time.Minute)
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	resumes := adapter.Resumes()
	if len(resumes) != 2 {
		t.Fatalf("resumes = %#v, want selection and rollup resumes", resumes)
	}
	if resumes[0].SessionID != "stored-session" || resumes[1].SessionID != "selection-new" {
		t.Fatalf("resume session ids = %#v, want stored-session then selection-new", resumes)
	}
	requests := adapter.Requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Prompt, `"schema": "findings"`) {
		t.Fatalf("fresh starts = %#v, want reviewer only", requests)
	}
	if result.NamedSessionCandidate == nil {
		t.Fatal("NamedSessionCandidate = nil, want rollup candidate")
	}
	wantCandidate := stored
	wantCandidate.ProviderSessionID = "rollup-new"
	wantCandidate.LastUsedAt = fixedNow()
	if !reflect.DeepEqual(*result.NamedSessionCandidate, wantCandidate) {
		t.Fatalf("candidate = %#v, want %#v", *result.NamedSessionCandidate, wantCandidate)
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if gotStored.ProviderSessionID != "stored-session" {
		t.Fatalf("stored provider session = %q, want unchanged stored-session", gotStored.ProviderSessionID)
	}
}

func TestLiveNamedSessionMissingRowStartsFreshAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-first")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want rollup resume from fresh selection", resumes)
	}
	if result.NamedSessionCandidate == nil {
		t.Fatal("NamedSessionCandidate = nil, want first-run candidate")
	}
	wantCandidate := namedSessionForRequest(req, "rollup-new")
	if !reflect.DeepEqual(*result.NamedSessionCandidate, wantCandidate) {
		t.Fatalf("candidate = %#v, want %#v", *result.NamedSessionCandidate, wantCandidate)
	}
	if _, err := store.GetNamedSession(ctx, req.SessionName); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want pipeline not to persist candidate", err)
	}
}

func TestLiveNamedSessionScopeMismatchRefusesBeforeLLM(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ledger.NamedSession)
		wantErr string
	}{
		{name: "profile", mutate: func(s *ledger.NamedSession) { s.Profile = "other" }, wantErr: "profile mismatch"},
		{name: "provider", mutate: func(s *ledger.NamedSession) { s.Provider = "openai" }, wantErr: "provider mismatch"},
		{name: "adapter", mutate: func(s *ledger.NamedSession) { s.Adapter = "other-adapter" }, wantErr: "adapter mismatch"},
		{name: "model", mutate: func(s *ledger.NamedSession) { s.Model = "claude-opus-4-8" }, wantErr: "model mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.SessionName = "daily"
			run := allocateLiveRun(t, store, provider, req, "run-live-named-mismatch-"+tt.name)
			stored := namedSessionForRequest(req, "stored-session")
			tt.mutate(&stored)
			if err := store.UpsertNamedSession(ctx, stored); err != nil {
				t.Fatalf("UpsertNamedSession: %v", err)
			}
			adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}

			_, err := liveForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				NamedSessions:   store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req, run)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Live error = %v, want %q", err, tt.wantErr)
			}
			if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
				t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
			}
		})
	}
}

func TestLiveNamedSessionResumeFailureLeavesStoredSessionUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-resume-failure")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	resumeErr := errors.New("resume failed")
	adapter.Queue(llm.FakeResult{StartErr: resumeErr})

	_, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if !errors.Is(err, resumeErr) {
		t.Fatalf("Live error = %v, want resume failure", err)
	}
	if resumes := adapter.Resumes(); len(resumes) != 1 || resumes[0].SessionID != "stored-session" {
		t.Fatalf("resumes = %#v, want one stored-session resume", resumes)
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if !reflect.DeepEqual(gotStored, stored) {
		t.Fatalf("stored named session = %#v, want unchanged %#v", gotStored, stored)
	}
}

func TestLiveNamedSessionCrossHostWarnsAndContinues(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-host")
	stored := namedSessionForRequest(req, "stored-session")
	stored.Host = "github.enterprise.example"
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.Host != req.PRRef.Host {
		t.Fatalf("candidate = %#v, want active host", result.NamedSessionCandidate)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 2 || resumes[0].SessionID != "stored-session" || resumes[1].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want stored-session then selection-new", resumes)
	}
	if !strings.Contains(warnings.String(), "host mismatch") || !strings.Contains(warnings.String(), "continuing") {
		t.Fatalf("warnings = %q, want host mismatch warning", warnings.String())
	}
}

func TestLiveNamedSessionUnsupportedResumeStartsFreshAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-unsupported")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-fresh", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-fresh", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("resumes = %#v, want none for unsupported adapter", adapter.Resumes())
	}
	if len(adapter.Requests()) != 3 {
		t.Fatalf("starts = %d, want selection/reviewer/rollup", len(adapter.Requests()))
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.ProviderSessionID != "rollup-fresh" {
		t.Fatalf("candidate = %#v, want rollup-fresh", result.NamedSessionCandidate)
	}
	if !strings.Contains(warnings.String(), "does not support resume") {
		t.Fatalf("warnings = %q, want unsupported resume warning", warnings.String())
	}
}

func TestLiveNamedSessionNoDiffLeavesCandidateEmpty(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-nodiff")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.NamedSessionCandidate != nil {
		t.Fatalf("NamedSessionCandidate = %#v, want nil", result.NamedSessionCandidate)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if !reflect.DeepEqual(gotStored, stored) {
		t.Fatalf("stored named session = %#v, want unchanged %#v", gotStored, stored)
	}
	if !strings.Contains(warnings.String(), "no orchestrator session was produced") {
		t.Fatalf("warnings = %q, want no-orchestrator warning", warnings.String())
	}
}

func TestLiveMarksRunIncompleteAfterBlockingLLMTaskError(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live-failed",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live-failed"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}

	_, err = liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err == nil || !strings.Contains(err.Error(), "no queued result") {
		t.Fatalf("Live error = %v, want fake LLM planning error", err)
	}
	storedRun, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome == nil || *storedRun.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("stored outcome = %#v, want incomplete", storedRun.Outcome)
	}
}

func TestLiveLeavesRunIncompleteAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live-canceled",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live-canceled"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}

	_, err = liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Live error = %v, want context.Canceled", err)
	}
	storedRun, err := store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome != nil {
		t.Fatalf("stored outcome = %#v, want incomplete after cancellation", storedRun.Outcome)
	}
}

func TestDryRunNoResolveThreadsKeepsSummaryReplyOnly(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	provider.threads = []gitprovider.InlineThread{
		markedReviewThread(t, "thread-1", "main.go", 2, req.PostingIdentity, human),
	}
	provider.caps.ThreadResolution = true
	req.NoResolveThreads = true
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "thread cleanup"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("thread-analysis-session", `{
		"schema_version": 1,
		"thread_id": "thread-1",
		"decision": "summarize",
		"reply_body": "",
		"summary": "Summary only",
		"resolve": true,
		"rationale": "safe"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("approve", nil), 1, 1))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-threads" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.EffectiveCaps.ThreadResolution {
		t.Fatal("EffectiveCaps.ThreadResolution = true, want disabled by request")
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("adapter requests = %d, want dossier/selection/thread-analysis/rollup", len(requests))
	}
	if !strings.Contains(requests[1].Prompt, `"status": "pending_human_reply"`) || strings.Contains(requests[1].Prompt, "<!-- codereview:") {
		t.Fatalf("selection prompt did not use sanitized normalized thread context:\n%s", requests[1].Prompt)
	}
	meta, ok, err := readLLMTaskMetadata(result.Artifacts, "thread-analysis-thread-1")
	if err != nil || !ok {
		t.Fatalf("read thread analysis metadata ok=%t err=%v", ok, err)
	}
	if meta.Status != llmTaskStatusSucceeded || meta.Phase != string(stagemodel.StageThreadAnalysis) {
		t.Fatalf("thread analysis metadata = %#v, want succeeded thread_analysis", meta)
	}
	var sawReply, sawResolve bool
	for _, action := range result.Plan.Actions {
		switch action.Kind {
		case reviewplan.ActionKindThreadReply:
			sawReply = true
		case reviewplan.ActionKindResolveThread:
			sawResolve = true
		case reviewplan.ActionKindInlineComment, reviewplan.ActionKindRollupComment, reviewplan.ActionKindSubmitReview:
		}
	}
	if !sawReply || sawResolve {
		t.Fatalf("actions = %#v, want thread reply and no resolve action", result.Plan.Actions)
	}
}

func TestDryRunMultiAgentSessionsMapFindingsToReviewerSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")
	adapter := &promptAwareAdapter{}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-multi-agent" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  2,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("adapter requests = %d, want selection, two reviewers, rollup", len(requests))
	}
	var reviewerPrompts int
	var selectionPrompts int
	for _, request := range requests {
		assertPromptOmitsLocalAgentSourceProvenance(t, request.Prompt, result.Catalog.Sources)
		if !strings.Contains(request.Prompt, `"output_contract"`) {
			t.Fatalf("prompt missing output contract: %s", request.Prompt)
		}
		if strings.Contains(request.Prompt, `"schema": "selection"`) {
			selectionPrompts++
			if !strings.Contains(request.Prompt, `"agent_id"`) ||
				!strings.Contains(request.Prompt, `"thread_actions"`) ||
				!strings.Contains(request.Prompt, `"schema_version"`) {
				t.Fatalf("selection prompt missing output schema fields: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"applies_when"`) {
				t.Fatalf("selection prompt missing applies_when routing metadata: %s", request.Prompt)
			}
			for _, forbidden := range []string{"Review alpha files.", "Review beta files.", `"prompt"`, `"provenance"`, `"overridden"`} {
				if strings.Contains(request.Prompt, forbidden) {
					t.Fatalf("selection prompt leaked reviewer execution instructions %q: %s", forbidden, request.Prompt)
				}
			}
		}
		if strings.Contains(request.Prompt, `"schema": "findings"`) {
			reviewerPrompts++
			if !strings.Contains(request.Prompt, `"agent"`) || !strings.Contains(request.Prompt, `"assignment"`) || !strings.Contains(request.Prompt, `"dossier"`) || !strings.Contains(request.Prompt, `"workbench"`) {
				t.Fatalf("reviewer prompt missing checkout-native context: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"file_path"`) ||
				!strings.Contains(request.Prompt, `"inspected_files"`) ||
				!strings.Contains(request.Prompt, `"skipped_files"`) ||
				!strings.Contains(request.Prompt, `"anchor"`) ||
				!strings.Contains(request.Prompt, `"Do not provide finding_id`) {
				t.Fatalf("reviewer prompt missing output schema fields: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"prompt"`) {
				t.Fatalf("reviewer prompt missing agent prompt field: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, "Review alpha files.") && !strings.Contains(request.Prompt, "Review beta files.") {
				t.Fatalf("reviewer prompt missing prompt.md body text: %s", request.Prompt)
			}
			for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`, `"needs_full_file_content"`, `"provenance"`, `"overridden"`, `"model_tier"`, `"model_id"`, `"effort"`} {
				if strings.Contains(request.Prompt, forbidden) {
					t.Fatalf("reviewer prompt leaked unsupported or stuffed field %q: %s", forbidden, request.Prompt)
				}
			}
		}
		if strings.Contains(request.Prompt, `"schema": "rollup"`) &&
			(!strings.Contains(request.Prompt, `"review_event"`) ||
				!strings.Contains(request.Prompt, `"dedupe_log"`) ||
				!strings.Contains(request.Prompt, `"ordered_findings"`)) {
			t.Fatalf("rollup prompt missing output schema fields: %s", request.Prompt)
		}
		if strings.Contains(request.Prompt, `"schema": "rollup"`) {
			if strings.Contains(request.Prompt, `"anchor"`) {
				t.Fatalf("rollup prompt leaked finding anchors: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"location"`) {
				t.Fatalf("rollup prompt missing finding location context: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"Use finding location only to distinguish findings during dedupe; do not include finding fields such as severity, file_path, location, body, anchor, or finding_id in the response."`) {
				t.Fatalf("rollup prompt missing explicit finding-object rejection: %s", request.Prompt)
			}
		}
	}
	if reviewerPrompts != 2 {
		t.Fatalf("reviewer prompts = %d, want 2", reviewerPrompts)
	}
	if selectionPrompts != 1 {
		t.Fatalf("selection prompts = %d, want 1", selectionPrompts)
	}

	storedFindings, err := store.ListFindings(ctx, "run-multi-agent")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	sessionAgents := map[string]string{}
	for _, session := range result.Sessions {
		if session.AgentID != nil {
			sessionAgents[session.SessionRowID] = *session.AgentID
		}
	}
	got := map[string]string{}
	for _, finding := range storedFindings {
		got[finding.FilePath] = sessionAgents[finding.SessionRowID]
	}
	want := map[string]string{
		"main.go":  "harness:alpha",
		"other.go": "harness:beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finding session agents = %#v, want %#v", got, want)
	}
}

func TestDryRunPlanSummaryNamesWorkstreamsInSelectionOrder(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	req.ToolVersion = "0.0.0-test"
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &promptAwareAdapter{},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-summary" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  2,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	summary := result.Plan.Summary
	var workstreamNames []string
	for _, workstream := range summary.Run.Workstreams {
		workstreamNames = append(workstreamNames, workstream.Name)
	}
	wantNames := []string{"orchestrator-selection", "harness:alpha", "harness:beta", "orchestrator-rollup"}
	if !reflect.DeepEqual(workstreamNames, wantNames) {
		t.Fatalf("workstream names = %#v, want %#v", workstreamNames, wantNames)
	}
	if !reflect.DeepEqual(summary.Run.SelectedReviewers, []string{"harness:alpha", "harness:beta"}) {
		t.Fatalf("selected reviewers = %#v", summary.Run.SelectedReviewers)
	}
	if summary.Run.ToolVersion != "0.0.0-test" || summary.Run.PostingIdentity == "" {
		t.Fatalf("run summary identity = %#v", summary.Run)
	}
	if summary.Run.WallDurationMS == nil {
		t.Fatalf("wall duration missing: %#v", summary.Run)
	}
	reviewerCounts := map[string]int{}
	for _, reviewer := range summary.Reviewers {
		reviewerCounts[reviewer.Name] = reviewer.Findings
	}
	if reviewerCounts["harness:alpha"] != 1 || reviewerCounts["harness:beta"] != 1 {
		t.Fatalf("reviewer counts = %#v, want one finding each", summary.Reviewers)
	}
	for _, want := range []string{"| Reviewer | Findings |", "Per-workstream usage", "| orchestrator-selection |"} {
		if !strings.Contains(result.Plan.RollupMarkdown, want) {
			t.Fatalf("rollup markdown missing %q:\n%s", want, result.Plan.RollupMarkdown)
		}
	}
}

func TestDryRunUsagePopulatesRollupAndLedgerSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ToolVersion = "0.0.0-test"
	systemTemp := filepath.Join(t.TempDir(), "system-temp")
	if err := os.MkdirAll(systemTemp, 0o700); err != nil {
		t.Fatalf("mkdir system temp: %v", err)
	}
	t.Setenv("TMPDIR", systemTemp)
	adapter := &providerOriginUsageAdapter{name: "codex_cli"}
	adapter.Queue(newCodexUsageScriptAdapter(t, "selection-session", selectionJSON("harness:reviewer", "main.go"), llm.Usage{
		TokensIn:  intPtr(25475),
		TokensOut: intPtr(812),
		CacheRead: intPtr(19712),
	}))
	adapter.Queue(newClaudeTranscriptScriptAdapter(t, "reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Finding"), llm.Usage{
		TokensIn:    intPtr(13),
		TokensOut:   intPtr(4069),
		CacheRead:   intPtr(861774),
		CacheCreate: intPtr(180377),
	}))
	adapter.Queue(newCodexUsageScriptAdapter(t, "rollup-session", rollupJSON("comment", []string{"finding-1"}), llm.Usage{
		TokensIn:  intPtr(11324),
		TokensOut: intPtr(129),
		CacheRead: intPtr(4480),
	}))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-usage" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	workstreamByName := map[string]reviewplan.WorkstreamUsage{}
	for _, workstream := range result.Plan.Summary.Run.Workstreams {
		workstreamByName[workstream.Name] = workstream
	}
	selection := workstreamByName[orchestratorSelectionStage]
	if selection.TokensIn == nil || *selection.TokensIn != 25475 ||
		selection.TokensOut == nil || *selection.TokensOut != 812 ||
		selection.CacheRead == nil || *selection.CacheRead != 19712 {
		t.Fatalf("selection workstream = %#v, want Codex usage values", selection)
	}
	reviewer := workstreamByName["harness:reviewer"]
	if reviewer.TokensIn == nil || *reviewer.TokensIn != 13 ||
		reviewer.TokensOut == nil || *reviewer.TokensOut != 4069 ||
		reviewer.CacheRead == nil || *reviewer.CacheRead != 861774 ||
		reviewer.CacheCreate == nil || *reviewer.CacheCreate != 180377 {
		t.Fatalf("reviewer workstream = %#v, want Claude-style cache values", reviewer)
	}
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, orchestratorSelectionStage, false)
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, "harness:reviewer", true)
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, orchestratorRollupStage, false)
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	sessionByProviderID := map[string]ledger.Session{}
	for _, session := range sessions {
		sessionByProviderID[session.ProviderSessionID] = session
	}
	if got := sessionByProviderID["selection-session"]; got.TokensIn == nil || *got.TokensIn != 25475 ||
		got.TokensOut == nil || *got.TokensOut != 812 ||
		got.CacheRead == nil || *got.CacheRead != 19712 {
		t.Fatalf("persisted selection session = %#v, want parsed Codex usage", got)
	}
	if got := sessionByProviderID["reviewer-session"]; got.TokensIn == nil || *got.TokensIn != 13 ||
		got.TokensOut == nil || *got.TokensOut != 4069 ||
		got.CacheRead == nil || *got.CacheRead != 861774 ||
		got.CacheCreate == nil || *got.CacheCreate != 180377 {
		t.Fatalf("persisted reviewer session = %#v, want transcript-derived Claude usage", got)
	}
	if got := sessionByProviderID["rollup-session"]; got.TokensIn == nil || *got.TokensIn != 11324 ||
		got.TokensOut == nil || *got.TokensOut != 129 ||
		got.CacheRead == nil || *got.CacheRead != 4480 {
		t.Fatalf("persisted rollup session = %#v, want parsed Codex rollup usage", got)
	}
}

func TestSharedWorkstreamModel(t *testing.T) {
	ws := func(name, model string) reviewplan.WorkstreamUsage {
		return reviewplan.WorkstreamUsage{Name: name, Model: model}
	}
	cases := []struct {
		name        string
		workstreams []reviewplan.WorkstreamUsage
		want        string
	}{
		{"all same", []reviewplan.WorkstreamUsage{ws("a:x", "sonnet"), ws("b:y", "sonnet")}, "sonnet"},
		{
			"orchestrators excluded so reviewer model is the headline",
			[]reviewplan.WorkstreamUsage{
				ws("orchestrator-selection", "sonnet"),
				ws("policies:conventions", "opus"),
				ws("structure:repo-health", "opus"),
				ws("orchestrator-rollup", "sonnet"),
			},
			"opus",
		},
		{
			"mixed reviewer models are joined in first-seen order",
			[]reviewplan.WorkstreamUsage{ws("a:x", "opus"), ws("b:y", "sonnet")},
			"opus, sonnet",
		},
		{"empty reviewer model is skipped", []reviewplan.WorkstreamUsage{ws("a:x", ""), ws("b:y", "sonnet")}, "sonnet"},
		{
			"falls back to orchestrator model when there are no reviewers",
			[]reviewplan.WorkstreamUsage{ws("orchestrator-selection", "sonnet")},
			"sonnet",
		},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedWorkstreamModel(tc.workstreams); got != tc.want {
				t.Fatalf("sharedWorkstreamModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkstreamUsageEstimatesCostWhenAdapterReportsNone(t *testing.T) {
	in, out := 1_000_000, 1_000_000

	// Known model, adapter reported no cost → estimate is filled and marked.
	draft := sessionDraft{
		model:    "claude-sonnet-4-6",
		response: llm.Response{Usage: llm.Usage{TokensIn: &in, TokensOut: &out}},
	}
	w := workstreamUsage("policies:conventions", draft)
	if w.CostUSD == nil || !w.CostEstimated {
		t.Fatalf("expected estimated cost; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}
	if want := 18.0; *w.CostUSD < want-1e-6 || *w.CostUSD > want+1e-6 { // 1M*$3 + 1M*$15
		t.Fatalf("cost = %v, want %v", *w.CostUSD, want)
	}

	// Unknown model (any agent's model) → no estimate, cost stays unavailable.
	draft.model = "vendor/unknown-model"
	w = workstreamUsage("x:y", draft)
	if w.CostUSD != nil || w.CostEstimated {
		t.Fatalf("unknown model should not be estimated; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}

	// Adapter reported a real cost → passes through, not marked estimated.
	realCost := 9.99
	draft.model = "claude-sonnet-4-6"
	draft.response.Usage.CostUSD = &realCost
	w = workstreamUsage("z:w", draft)
	if w.CostUSD == nil || *w.CostUSD != realCost || w.CostEstimated {
		t.Fatalf("real cost should pass through unmarked; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}
}

func TestBuildRunSummaryWorkstreamBoundaries(t *testing.T) {
	agentID := "harness:alpha"
	inputs := planRunInputs{
		hasRun:    true,
		selection: sessionDraft{adapter: "fake", model: "sonnet", response: llm.Response{DurationMS: 0}},
		reviewers: []sessionDraft{{rowID: "row-1", agentID: &agentID, model: "sonnet", response: llm.Response{DurationMS: 25}}},
		rollup:    sessionDraft{model: "sonnet", startedAt: fixedNow(), completedAt: fixedNow().Add(2 * time.Second)},
		selectedAgents: []llm.SelectedAgent{
			{AgentID: agentID},
			{AgentID: "harness:missing-draft"},
		},
		findingSessions: map[review.FindingID]string{"f-1": "row-1", "f-2": "row-unknown"},
		startedAt:       fixedNow(),
	}
	summary, findingReviewers := Options{Now: fixedNow}.buildRunSummary(Request{ToolVersion: "t"}, inputs)

	var names []string
	for _, workstream := range summary.Workstreams {
		names = append(names, workstream.Name)
	}
	want := []string{"orchestrator-selection", agentID, "orchestrator-rollup"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("workstreams = %#v, want missing-draft agent skipped: %#v", names, want)
	}
	if !reflect.DeepEqual(summary.SelectedReviewers, []string{agentID, "harness:missing-draft"}) {
		t.Fatalf("selected reviewers = %#v, must keep the draft-less agent", summary.SelectedReviewers)
	}
	if summary.Workstreams[0].DurationMS != nil {
		t.Fatalf("zero duration must render unavailable, got %v", *summary.Workstreams[0].DurationMS)
	}
	if summary.Workstreams[1].DurationMS == nil || *summary.Workstreams[1].DurationMS != 25 {
		t.Fatalf("reported duration lost: %#v", summary.Workstreams[1])
	}
	if summary.Workstreams[2].DurationMS == nil || *summary.Workstreams[2].DurationMS != 2000 {
		t.Fatalf("start/complete fallback duration missing: %#v", summary.Workstreams[2])
	}
	if !reflect.DeepEqual(findingReviewers, map[review.FindingID]string{"f-1": agentID}) {
		t.Fatalf("finding reviewers = %#v, want unknown session unattributed", findingReviewers)
	}
}

func TestDryRunRejectsUnsafeProfileAgentSourcesBeforeRunAllocation(t *testing.T) {
	tests := []struct {
		name       string
		source     func(t *testing.T) string
		wantDetail string
	}{
		{name: "relative", source: relativeAgentSource, wantDetail: "relative"},
		{name: "temp", source: tempAgentSource, wantDetail: "OS temp"},
		{name: "git worktree", source: gitWorktreeAgentSource, wantDetail: "Git worktree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.Profile.AgentSources = []string{tt.source(t)}

			_, err := dryRunForTest(ctx, Options{
				Provider: provider,
				Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
				Store:    store,
				Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:      fixedNow,
				NewRunID: func() string {
					t.Fatal("NewRunID called before unsafe source rejection")
					return ""
				},
			}, req)
			if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("DryRun error = %v, want ErrUnsafeSource with %q", err, tt.wantDetail)
			}
		})
	}
}

func TestDryRunMarksRunFailedAfterPostAllocationError(t *testing.T) {
	ctx := context.Background()
	inner := openPipelineStore(t)
	defer closeStore(t, inner)
	storeErr := errors.New("insert planned action failed")
	store := &failingStore{Store: inner, insertPlannedActionErr: storeErr}
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))

	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-failed-after-allocation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if !errors.Is(err, storeErr) {
		t.Fatalf("DryRun error = %v, want planned-action failure", err)
	}
	run, err := inner.GetRun(ctx, "run-failed-after-allocation")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeFailed {
		t.Fatalf("run outcome = %#v, want failed", run.Outcome)
	}
}

func TestDryRunRejectsSelfReviewWhenReviewerCredentialsMatchAuthor(t *testing.T) {
	provider, req := dryRunHarness(t)
	req.Profile.ReviewerCredentials = &config.ReviewerCredentials{AuthMode: config.GitAuthModePAT, CredentialRef: "codereview/reviewer"}
	req.PostingIdentity = provider.pr.Author
	adapter := &llm.FakeAdapter{QuotaErr: errors.New("quota should not be called")}

	_, err := DryRun(context.Background(), Options{
		Provider: provider,
		Adapter:  adapter,
		Store:    &noopStore{},
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, req)
	if err == nil {
		t.Fatal("DryRun error = nil, want self-review guard")
	}
	if !strings.Contains(err.Error(), "--allow-self-review") {
		t.Fatalf("DryRun error = %v, want allow-self-review guidance", err)
	}
	if provider.diffCalls != 0 || provider.threadCalls != 0 || len(provider.treeCalls) != 0 {
		t.Fatalf("provider side effects = diff:%d threads:%d tree:%#v, want early self-review rejection before diff/thread/catalog work", provider.diffCalls, provider.threadCalls, provider.treeCalls)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestSelectionOutputContractExampleHasNoAgentsWhenCatalogEmpty(t *testing.T) {
	contract := selectionOutputContract(nil, []string{"main.go"}, nil, 3)
	example, ok := contract.Example.(map[string]any)
	if !ok {
		t.Fatalf("Example = %#v, want map", contract.Example)
	}
	selected, ok := example["selected_agents"].([]map[string]any)
	if !ok {
		t.Fatalf("selected_agents = %#v, want []map[string]any", example["selected_agents"])
	}
	if len(selected) != 0 {
		t.Fatalf("selected_agents = %#v, want empty when no agents are allowed", selected)
	}
}

func TestSelectionPromptIncludesMaxSelectedAgentsContract(t *testing.T) {
	prompt, err := buildSelectionPrompt(
		agents.Catalog{Agents: []agents.Agent{{ID: "agent-1"}}},
		selectionPromptInput{ChangedFiles: []string{"main.go"}},
		3,
		"",
	)
	if err != nil {
		t.Fatalf("buildSelectionPrompt: %v", err)
	}
	var payload struct {
		MaxSelectedAgents int `json:"max_selected_agents"`
		OutputContract    struct {
			Instructions  []string       `json:"instructions"`
			AllowedValues map[string]any `json:"allowed_values"`
		} `json:"output_contract"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("Unmarshal prompt: %v", err)
	}
	if payload.MaxSelectedAgents != 3 {
		t.Fatalf("max_selected_agents = %d, want 3", payload.MaxSelectedAgents)
	}
	if got := payload.OutputContract.AllowedValues["max_selected_agents"]; got != float64(3) {
		t.Fatalf("allowed max_selected_agents = %#v, want 3", got)
	}
	if !strings.Contains(strings.Join(payload.OutputContract.Instructions, "\n"), "at most max_selected_agents") {
		t.Fatalf("instructions = %#v, want max-selected-agents guidance", payload.OutputContract.Instructions)
	}
	if !strings.Contains(prompt, "allowed_files") {
		t.Fatalf("selection prompt contract missing allowed_files: %s", prompt)
	}
}

func TestSelectionOutputContractExampleOmitsAllowedFilesForBroadReviewer(t *testing.T) {
	contract := selectionOutputContract([]agents.Agent{{ID: "agent-1"}}, []string{"main.go"}, nil, 3)
	example, ok := contract.Example.(map[string]any)
	if !ok {
		t.Fatalf("Example = %#v, want map", contract.Example)
	}
	selected, ok := example["selected_agents"].([]map[string]any)
	if !ok || len(selected) != 1 {
		t.Fatalf("selected_agents = %#v, want one example reviewer", example["selected_agents"])
	}
	if _, ok := selected[0]["allowed_files"]; ok {
		t.Fatalf("selection example unexpectedly set allowed_files for broad reviewer: %#v", selected[0])
	}
}

func TestSelectionPromptDependenciesTrackDossierAndWorkbenchDigests(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	_, deps1, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts first: %v", err)
	}
	if len(deps1) != 2 {
		t.Fatalf("deps len = %d, want dossier/workbench deps", len(deps1))
	}

	indexPath := result.Artifacts.DossierIndexPath()
	if err := os.WriteFile(indexPath, []byte(`{"hash_algorithm":"sha256","files":[{"path":"final/pr-intent.md","sha256":"changed"}]}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", indexPath, err)
	}
	_, deps2, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts dossier change: %v", err)
	}
	if reflect.DeepEqual(deps1, deps2) {
		t.Fatalf("deps after dossier index change = %#v, want changed from %#v", deps2, deps1)
	}

	metaPath := result.Artifacts.WorkbenchMetadataPath()
	var meta workbenchMetadataArtifact
	if err := readJSONFile(metaPath, &meta); err != nil {
		t.Fatalf("read workbench metadata: %v", err)
	}
	meta.FingerprintInputs.ChangedFiles = append(meta.FingerprintInputs.ChangedFiles, "extra.go")
	if err := writeJSONFile(metaPath, meta); err != nil {
		t.Fatalf("write workbench metadata: %v", err)
	}
	_, deps3, err := selectionPromptInputFromArtifacts(result.Artifacts, result.Threads)
	if err != nil {
		t.Fatalf("selectionPromptInputFromArtifacts workbench change: %v", err)
	}
	if reflect.DeepEqual(deps2, deps3) {
		t.Fatalf("deps after workbench metadata change = %#v, want changed from %#v", deps3, deps2)
	}
}

func TestSelectionTaskMetadataDependsOnDossierSummaryTask(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "body"), 10, 2))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 10, 2))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-selection-deps" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	selectionMeta, ok, err := readLLMTaskMetadata(ArtifactPathsFromDir(result.Run.ArtifactPath), orchestratorSelectionStage)
	if err != nil || !ok {
		t.Fatalf("read selection metadata: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(selectionMeta.DependencyTaskIDs, []string{dossierSummaryTaskID}) {
		t.Fatalf("selection dependency_task_ids = %#v, want dossier summary dependency", selectionMeta.DependencyTaskIDs)
	}
}

func TestRunSelectionPhaseRejectsStaleSelectionMetadataWhenDossierDigestChanges(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-selection-stale-dossier", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	opts := Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(run.ArtifactPath), nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if err := prepareDossierArtifacts(ctx, opts, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	if _, _, _, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	}); err != nil {
		t.Fatalf("runSelectionPhase first: %v", err)
	}
	indexPath := prepared.artifacts.DossierIndexPath()
	if err := os.WriteFile(indexPath, []byte(`{"hash_algorithm":"sha256","files":[{"path":"final/pr-intent.md","sha256":"changed"}]}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", indexPath, err)
	}
	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := opts
	secondOpts.Adapter = secondAdapter
	_, _, _, err = runSelectionPhase(ctx, secondOpts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("runSelectionPhase stale dossier error = %v, want input fingerprint changed", err)
	}
	if len(secondAdapter.Requests()) != 0 || len(secondAdapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite stale selection metadata: starts=%#v resumes=%#v", secondAdapter.Requests(), secondAdapter.Resumes())
	}
}

func TestFindingsOutputContractScopesAnchorToFindingItems(t *testing.T) {
	contract := findingsOutputContract("agent-1", []string{"main.go"})
	schema, ok := contract.ResponseSchema.(map[string]any)
	if !ok {
		t.Fatalf("response schema type = %T, want map", contract.ResponseSchema)
	}
	if _, ok := schema["anchor"]; ok {
		t.Fatalf("response schema exposes anchor as a top-level field: %#v", schema)
	}
	findingsSchema, ok := schema["findings"].(string)
	if !ok {
		t.Fatalf("findings schema type = %T, want string", schema["findings"])
	}
	if !strings.Contains(findingsSchema, "anchor") {
		t.Fatalf("findings schema does not describe item anchors: %q", findingsSchema)
	}
}

func TestRunReviewerRejectsStaleWorkbenchMetadata(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocatePipelineRun(t, store, layout, "run-reviewer-stale-workbench", ledger.PostModeDryRun, fixedNow())
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	opts := Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
	}
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	prepared, err := prepareSelectionContext(ctx, opts, selectionSetupRequest{
		PRRef:         req.PRRef,
		Profile:       req.Profile,
		AgentDirs:     req.AgentDirs,
		ReviewBaseSHA: req.ReviewBaseSHA,
		ReviewHeadSHA: req.ReviewHeadSHA,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(run.ArtifactPath), nil
		},
	})
	if err != nil {
		t.Fatalf("prepareSelectionContext: %v", err)
	}
	if err := prepareWorkbenchArtifacts(ctx, opts, workbenchPreparationRequest{
		PRRef:        req.PRRef,
		ReviewPR:     prepared.reviewPR,
		ChangedFiles: prepared.changedFiles,
		Artifacts:    prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareWorkbenchArtifacts: %v", err)
	}
	if err := prepareDossierArtifacts(ctx, opts, dossierPreparationRequest{
		RunID:     run.RunID,
		Profile:   req.Profile,
		Artifacts: prepared.artifacts,
	}); err != nil {
		t.Fatalf("prepareDossierArtifacts: %v", err)
	}
	selection, _, _, err := runSelectionPhase(ctx, opts, selectionPhaseRequest{
		RunID:      run.RunID,
		Profile:    req.Profile,
		ReviewPR:   prepared.reviewPR,
		Catalog:    prepared.catalog,
		ParsedDiff: prepared.parsed,
		Threads:    prepared.threads,
		Artifacts:  prepared.artifacts,
		MaxAgents:  1,
	})
	if err != nil {
		t.Fatalf("runSelectionPhase first: %v", err)
	}
	agent, ok := prepared.catalog.Find(selection.SelectedAgents[0].AgentID)
	if !ok {
		t.Fatalf("selected agent %q missing from catalog", selection.SelectedAgents[0].AgentID)
	}
	if _, _, _, _, err := runReviewer(ctx, opts, req, run.RunID, prepared.reviewPR, prepared.parsed, prepared.artifacts, selection.SelectedAgents[0], agent, []string{orchestratorSelectionStage}); err != nil {
		t.Fatalf("runReviewer first: %v", err)
	}
	metaPath := prepared.artifacts.WorkbenchMetadataPath()
	var meta workbenchMetadataArtifact
	if err := readJSONFile(metaPath, &meta); err != nil {
		t.Fatalf("read workbench metadata: %v", err)
	}
	meta.FingerprintInputs.ChangedFiles = append(meta.FingerprintInputs.ChangedFiles, "extra.go")
	if err := writeJSONFile(metaPath, meta); err != nil {
		t.Fatalf("write workbench metadata: %v", err)
	}
	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := opts
	secondOpts.Adapter = secondAdapter
	_, _, _, _, err = runReviewer(ctx, secondOpts, req, run.RunID, prepared.reviewPR, prepared.parsed, prepared.artifacts, selection.SelectedAgents[0], agent, []string{orchestratorSelectionStage})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("runReviewer stale workbench error = %v, want input fingerprint changed", err)
	}
	if len(secondAdapter.Requests()) != 0 || len(secondAdapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite stale reviewer metadata: starts=%#v resumes=%#v", secondAdapter.Requests(), secondAdapter.Resumes())
	}
}

func TestRollupPromptPreservesLocationForDedupeWithoutRawAnchors(t *testing.T) {
	prompt, err := buildRollupPrompt(gitprovider.PR{Body: "Rollup prompt body should stay out of prompt payloads."}, []review.Finding{
		{
			ID:       "finding-1",
			Severity: review.SeverityMajor,
			FilePath: "main.go",
			Anchor:   review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 10},
			Body:     "same issue text",
		},
		{
			ID:       "finding-2",
			Severity: review.SeverityMajor,
			FilePath: "main.go",
			Anchor:   review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 20},
			Body:     "same issue text",
		},
	}, nil, []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageCompleteBroad,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
	}})
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}

	var payload struct {
		Findings         []rollupFindingPrompt       `json:"findings"`
		ReviewerCoverage []rollupCoveragePromptEntry `json:"reviewer_coverage"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("unmarshal rollup prompt: %v", err)
	}
	if len(payload.Findings) != 2 {
		t.Fatalf("rollup findings = %d, want 2", len(payload.Findings))
	}
	if len(payload.ReviewerCoverage) != 1 ||
		payload.ReviewerCoverage[0].Status != reviewerCoverageCompleteBroad ||
		!reflect.DeepEqual(payload.ReviewerCoverage[0].InspectedFiles, []string{"main.go"}) {
		t.Fatalf("reviewer coverage = %#v, want compact broad coverage", payload.ReviewerCoverage)
	}
	if payload.Findings[0].Location.Line != 10 || payload.Findings[1].Location.Line != 20 {
		t.Fatalf("rollup finding locations = %#v", payload.Findings)
	}
	if strings.Contains(prompt, `"anchor"`) {
		t.Fatalf("rollup prompt leaked raw anchor key: %s", prompt)
	}
	if strings.Contains(prompt, "Rollup prompt body should stay out of prompt payloads.") {
		t.Fatalf("rollup prompt leaked PR body: %s", prompt)
	}
	for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`, "@@", "+changed implementation body"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("rollup prompt leaked stuffed code context %q: %s", forbidden, prompt)
		}
	}
}

func TestBuildReviewerCoverageStatuses(t *testing.T) {
	broad := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:broad", Files: []string{"main.go", "other.go"}}},
		[]llm.Findings{{AgentID: "harness:broad", InspectedFiles: []string{"main.go", "other.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(broad) != 1 || broad[0].Status != reviewerCoverageCompleteBroad {
		t.Fatalf("broad coverage = %#v, want complete broad", broad)
	}

	selected := []llm.SelectedAgent{
		{AgentID: "harness:scoped", Files: []string{"db.sql"}, AllowedFiles: []string{"db.sql"}},
		{AgentID: "harness:skipped", Files: []string{"api.go"}, AllowedFiles: []string{"api.go"}},
		{AgentID: "harness:failed", Files: []string{"worker.go"}, AllowedFiles: []string{"worker.go"}},
	}
	results := []llm.Findings{
		{AgentID: "harness:scoped", InspectedFiles: []string{"db.sql"}, Constraints: []string{"SQL-only review"}},
		{AgentID: "harness:skipped", InspectedFiles: []string{"api.go"}, SkippedFiles: []string{"api.go"}},
	}
	failures := []ReviewerFailure{{AgentID: "harness:failed", Error: "model failed"}}

	got := buildReviewerCoverage(selected, results, failures, []string{"api.go", "db.sql", "unassigned.go", "worker.go"})
	byAgent := map[string]reviewplan.ReviewerCoverageSummary{}
	for _, entry := range got {
		byAgent[entry.AgentID] = entry
	}

	if byAgent["harness:scoped"].Status != reviewerCoverageCompleteConstrained {
		t.Fatalf("scoped coverage = %#v", byAgent["harness:scoped"])
	}
	if byAgent["harness:skipped"].Status != reviewerCoverageIncompleteSkipped {
		t.Fatalf("skipped coverage = %#v", byAgent["harness:skipped"])
	}
	if byAgent["harness:failed"].Status != reviewerCoverageIncompleteFailed || byAgent["harness:failed"].Diagnostic != "model failed" {
		t.Fatalf("failed coverage = %#v", byAgent["harness:failed"])
	}
	if byAgent["unassigned"].Status != reviewerCoverageIncompleteUnassigned ||
		!reflect.DeepEqual(byAgent["unassigned"].SkippedFiles, []string{"unassigned.go"}) {
		t.Fatalf("unassigned coverage = %#v", byAgent["unassigned"])
	}
}

func TestBuildReviewerCoverageMarksAssignedScopeMissing(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:reviewer", AllowedFiles: []string{"main.go", "other.go"}}},
		[]llm.Findings{{AgentID: "harness:reviewer", InspectedFiles: []string{"main.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 1 {
		t.Fatalf("coverage entries = %#v", got)
	}
	if got[0].Status != reviewerCoverageIncompleteSkipped ||
		!strings.Contains(got[0].Diagnostic, "other.go") {
		t.Fatalf("coverage = %#v, want incomplete missing other.go", got[0])
	}
}

func TestReviewerScopesSeparateReadAccessFromExpectedCoverage(t *testing.T) {
	changed := []string{"api.go", "main.go", "schema.sql"}
	if got := reviewerReadableFiles(changed); !reflect.DeepEqual(got, []string{"api.go", "main.go", "schema.sql"}) {
		t.Fatalf("reviewable files = %#v, want all changed files", got)
	}
	if got := reviewerAssignmentScope(llm.SelectedAgent{
		Files:        []string{"main.go"},
		AllowedFiles: []string{"schema.sql"},
	}, changed); !reflect.DeepEqual(got, []string{"schema.sql"}) {
		t.Fatalf("allowed-files scope = %#v, want allowed files", got)
	}
	if got := reviewerAssignmentScope(llm.SelectedAgent{Files: []string{"main.go"}}, changed); !reflect.DeepEqual(got, []string{"main.go"}) {
		t.Fatalf("broad coverage scope = %#v, want selected files", got)
	}
	_, err := llm.DecodeFindings([]byte(`{
		"schema_version": 1,
		"agent_id": "agent-1",
		"inspected_files": ["schema.sql"],
		"findings": [{
			"severity": "major",
			"file_path": "api.go",
			"anchor": {"kind": "file"},
			"body": "outside assignment"
		}]
	}`), llm.FindingsOptions{
		KnownAgents:  map[string]bool{"agent-1": true},
		ChangedFiles: stringSet(reviewerAssignmentScope(llm.SelectedAgent{AllowedFiles: []string{"schema.sql"}}, changed)),
		NewFindingID: findingSequence("scope"),
	})
	if err == nil || !strings.Contains(err.Error(), "not in changed files") {
		t.Fatalf("DecodeFindings outside assignment error = %v, want not in changed files", err)
	}
}

func TestBuildReviewerCoverageAllowsBroadReviewerSplitAssignments(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{
			{AgentID: "harness:alpha", Files: []string{"main.go"}},
			{AgentID: "harness:beta", Files: []string{"other.go"}},
		},
		[]llm.Findings{
			{AgentID: "harness:alpha", InspectedFiles: []string{"main.go"}},
			{AgentID: "harness:beta", InspectedFiles: []string{"other.go"}},
		},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 2 {
		t.Fatalf("coverage entries = %#v, want two reviewer entries", got)
	}
	for _, entry := range got {
		if entry.Status != reviewerCoverageCompleteBroad {
			t.Fatalf("coverage entry = %#v, want complete broad", entry)
		}
	}
}

func TestBuildReviewerCoverageIgnoresSkippedFilesOutsideAssignmentScope(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:reviewer", Files: []string{"main.go"}}},
		[]llm.Findings{{AgentID: "harness:reviewer", InspectedFiles: []string{"main.go"}, SkippedFiles: []string{"other.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 2 {
		t.Fatalf("coverage entries = %#v, want reviewer plus unassigned other.go", got)
	}
	if got[0].AgentID != "harness:reviewer" || got[0].Status != reviewerCoverageCompleteBroad {
		t.Fatalf("reviewer coverage = %#v, want complete broad for assigned scope", got[0])
	}
	if len(got[0].SkippedFiles) != 0 {
		t.Fatalf("reviewer skipped files = %#v, want outside-assignment skipped file filtered", got[0].SkippedFiles)
	}
	if got[1].AgentID != "unassigned" || got[1].Status != reviewerCoverageIncompleteUnassigned {
		t.Fatalf("unassigned coverage = %#v, want other.go unassigned", got[1])
	}
}

func TestRollupPromptAndFingerprintChangeWhenReviewerCoverageChanges(t *testing.T) {
	findings := largeRollupFindings(1, "main.go", "body")
	baseCoverage := []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageCompleteBroad,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
	}}
	skippedCoverage := []reviewplan.ReviewerCoverageSummary{{
		AgentID:        "harness:reviewer",
		Status:         reviewerCoverageIncompleteSkipped,
		Scope:          []string{"main.go"},
		InspectedFiles: []string{"main.go"},
		SkippedFiles:   []string{"main.go"},
	}}
	basePrompt, err := buildRollupPrompt(gitprovider.PR{}, findings, nil, baseCoverage)
	if err != nil {
		t.Fatalf("buildRollupPrompt base: %v", err)
	}
	skippedPrompt, err := buildRollupPrompt(gitprovider.PR{}, findings, nil, skippedCoverage)
	if err != nil {
		t.Fatalf("buildRollupPrompt skipped: %v", err)
	}
	if basePrompt == skippedPrompt {
		t.Fatal("rollup prompts are equal, want coverage changes to affect prompt")
	}
	deps := []string{orchestratorSelectionStage, reviewerTaskID("harness:reviewer")}
	baseFingerprint := llmTaskFingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", basePrompt, deps)
	skippedFingerprint := llmTaskFingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", skippedPrompt, deps)
	if baseFingerprint == skippedFingerprint {
		t.Fatal("rollup fingerprints are equal, want coverage changes to invalidate cached rollup input")
	}
}

func TestDryRunContextBudgetFailures(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		mutate func(t *testing.T, provider *readOnlyProvider, req *Request, adapter *llm.FakeAdapter)
		want   string
		runID  string
		queue  func(adapter *llm.FakeAdapter)
	}{
		{
			name:   "selection default model",
			budget: 100,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
			},
			want:  "context budget exceeded for selection model claude-sonnet-4-6",
			runID: "run-budget-selection-default",
		},
		{
			name:   "selection override model",
			budget: 100,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				req.SelectionModelOverride = "bench-model"
			},
			want:  "context budget exceeded for selection model bench-model",
			runID: "run-budget-selection-override",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			if tt.mutate != nil {
				tt.mutate(t, provider, &req, adapter)
			}
			if tt.queue != nil {
				tt.queue(adapter)
			}
			_, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return tt.runID },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				Budget:          ContextBudget{MaxPromptBytes: tt.budget},
				MaxConcurrency:  1,
			}, req)
			if err == nil {
				t.Fatal("DryRun error = nil, want budget failure")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DryRun error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDryRunReviewerWorkspaceAvoidsContextStuffingBudgetFailures(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		mutate func(t *testing.T, provider *readOnlyProvider, req *Request)
		runID  string
	}{
		{
			name:   "large reviewer diff",
			budget: 9500,
			mutate: func(t *testing.T, provider *readOnlyProvider, _ *Request) {
				t.Helper()
				provider.diff.Raw = largeDiff("main.go", strings.Repeat("+var x = true\n", 1600))
			},
			runID: "run-budget-reviewer-checkout",
		},
		{
			name:   "full-content agent stays within reviewer workspace budget",
			budget: 9500,
			mutate: func(t *testing.T, provider *readOnlyProvider, req *Request) {
				t.Helper()
				dir := t.TempDir()
				writeAgentFullContent(t, dir, "harness", "reviewer")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: "main.go"}] = []byte(strings.Repeat("base\n", 3000))
				provider.files[fileKey{gitRef: provider.pr.Head.SHA, path: "main.go"}] = []byte("package main\n")
			},
			runID: "run-budget-reviewer-workspace-agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			tt.mutate(t, provider, &req)
			adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
			adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))

			result, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return tt.runID },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				Budget:          ContextBudget{MaxPromptBytes: tt.budget},
				MaxConcurrency:  1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			requests := adapter.Requests()
			if len(requests) < 2 {
				t.Fatalf("adapter requests = %#v, want selection and reviewer calls", requests)
			}
			reviewerPrompt := requests[1].Prompt
			for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`} {
				if strings.Contains(reviewerPrompt, forbidden) {
					t.Fatalf("reviewer prompt leaked stuffed code field %q: %s", forbidden, reviewerPrompt)
				}
			}
			if requests[1].ReviewerWorkspace == nil {
				t.Fatalf("reviewer request = %#v, want reviewer workspace", requests[1])
			}
			workspace := requests[1].ReviewerWorkspace
			if !strings.Contains(workspace.RepoDir, filepath.Join("workbench", "reviewers")) ||
				!strings.HasPrefix(workspace.ScratchDir, result.Artifacts.WorkbenchScratch+string(filepath.Separator)) ||
				workspace.MaxToolOutputBytes != defaultReviewerWorkspaceToolOutputBytes {
				t.Fatalf("reviewer workspace request = %#v, want disposable repo/scratch with default cap", workspace)
			}
			if len(result.Findings) != 1 {
				t.Fatalf("findings len = %d, want reviewer success under bounded prompt budget", len(result.Findings))
			}
		})
	}
}

func TestRollupPromptBudgetUsesSynthesisModel(t *testing.T) {
	provider, req := dryRunHarness(t)
	rollupRuntime, err := resolveSynthesisRuntimeConfig(req)
	if err != nil {
		t.Fatalf("resolveSynthesisRuntimeConfig: %v", err)
	}
	prompt, err := buildRollupPrompt(provider.pr, largeRollupFindings(4, "main.go", strings.Repeat("body ", 4000)), nil, nil)
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}
	err = (Options{Budget: ContextBudget{MaxPromptBytes: 10000}}).checkPromptBudget("rollup", "", rollupRuntime.model, "", prompt)
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for rollup model claude-sonnet-4-6") {
		t.Fatalf("rollup budget error = %v, want synthesis-model budget failure", err)
	}
}

func TestRollupPromptBudgetIgnoresSelectionModelOverride(t *testing.T) {
	provider, req := dryRunHarness(t)
	req.SelectionModelOverride = "bench-model"
	rollupRuntime, err := resolveSynthesisRuntimeConfig(req)
	if err != nil {
		t.Fatalf("resolveSynthesisRuntimeConfig: %v", err)
	}
	prompt, err := buildRollupPrompt(provider.pr, largeRollupFindings(4, "main.go", strings.Repeat("body ", 4000)), nil, nil)
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}
	err = (Options{Budget: ContextBudget{MaxPromptBytes: 10000}}).checkPromptBudget("rollup", "", rollupRuntime.model, "", prompt)
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for rollup model claude-sonnet-4-6") {
		t.Fatalf("rollup budget error = %v, want default synthesis model despite selection override", err)
	}
}

func TestSessionRowIDForFindingRequiresReviewerSession(t *testing.T) {
	finding := reviewplan.AnchoredFinding{FindingID: "finding-1"}
	if got, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"finding-1": "session-1"}); err != nil || got != "session-1" {
		t.Fatalf("sessionRowIDForFinding = %q, %v; want session-1 nil", got, err)
	}
	if _, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"other": "session-1"}); err == nil {
		t.Fatal("sessionRowIDForFinding missing error = nil, want invariant failure")
	}
	if _, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"finding-1": "  "}); err == nil {
		t.Fatal("sessionRowIDForFinding blank error = nil, want invariant failure")
	}
}

type readOnlyProvider struct {
	pr                gitprovider.PR
	diff              gitprovider.UnifiedDiff
	diffCalls         int
	diffBetween       gitprovider.UnifiedDiff
	diffBetweenCalls  []shaPair
	files             map[fileKey][]byte
	fileCalls         []fileKey
	trees             map[fileKey][]gitprovider.TreeEntry
	treeCalls         []fileKey
	threads           []gitprovider.InlineThread
	reviews           []gitprovider.Review
	issueComments     []gitprovider.IssueComment
	threadCalls       int
	reviewCalls       int
	issueCommentCalls int
	caps              gitprovider.ProviderCaps
	onGetPR           func()
	fixtureRepoDir    string
}

type shaPair struct {
	baseSHA string
	headSHA string
}

type promptAwareAdapter struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (a *promptAwareAdapter) Name() string {
	return "prompt-aware"
}

func (a *promptAwareAdapter) SupportsResume() bool {
	return false
}

func (a *promptAwareAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}

func (a *promptAwareAdapter) SupportsCacheAccounting() bool {
	return false
}

func (a *promptAwareAdapter) SupportsCostReporting() bool {
	return false
}

func (a *promptAwareAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *promptAwareAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("resume unsupported")
}

func (a *promptAwareAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	switch {
	case strings.Contains(req.Prompt, `"schema": "selection"`):
		return staticStream{sessionID: "selection-session", output: `{
			"schema_version": 1,
			"selected_agents": [
				{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]},
				{"agent_id":"harness:beta","rationale":"other","files":["other.go"]}
			],
			"thread_actions": [],
			"reasoning": "two agents"
		}`}, nil
	case strings.Contains(req.Prompt, `"schema": "rollup"`):
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", findingIDsFromPrompt(req.Prompt))}, nil
	case strings.Contains(req.Prompt, "harness:alpha"):
		return staticStream{sessionID: "alpha-session", output: findingsJSON("harness:alpha", "main.go", "major", 2, "Alpha finding")}, nil
	case strings.Contains(req.Prompt, "harness:beta"):
		return staticStream{sessionID: "beta-session", output: findingsJSON("harness:beta", "other.go", "major", 2, "Beta finding")}, nil
	default:
		return nil, fmt.Errorf("unexpected prompt: %s", req.Prompt)
	}
}

func findingIDsFromPrompt(prompt string) []string {
	matches := regexp.MustCompile(`finding-\d+`).FindAllString(prompt, -1)
	seen := map[string]bool{}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if seen[match] {
			continue
		}
		seen[match] = true
		ids = append(ids, match)
	}
	return ids
}

func (a *promptAwareAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

type reviewerIsolationAdapter struct {
	mu                         sync.Mutex
	requests                   []llm.Request
	supportsResume             bool
	betaAttempts               int
	betaProviderErr            error
	betaRetrySawCleanWorkspace bool
	reviewerBarrier            *reviewerStartBarrier
}

func (a *reviewerIsolationAdapter) Name() string {
	return "reviewer-isolation"
}

func (a *reviewerIsolationAdapter) SupportsResume() bool {
	return a.supportsResume
}

func (a *reviewerIsolationAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}

func (a *reviewerIsolationAdapter) SupportsCacheAccounting() bool {
	return false
}

func (a *reviewerIsolationAdapter) SupportsCostReporting() bool {
	return false
}

func (a *reviewerIsolationAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *reviewerIsolationAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unexpected reviewer resume")
}

func (a *reviewerIsolationAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	switch {
	case strings.Contains(req.Prompt, `"schema": "selection"`):
		return staticStream{sessionID: "selection-session", output: selectionJSONForAgents("main.go", "harness:alpha", "harness:beta", "harness:gamma")}, nil
	case strings.Contains(req.Prompt, `"schema": "rollup"`):
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", findingIDsFromPrompt(req.Prompt))}, nil
	case strings.Contains(req.Prompt, `"id": "harness:alpha"`):
		a.waitReviewerStart("harness:alpha")
		return staticStream{sessionID: "alpha-session", output: findingsJSON("harness:alpha", "main.go", "major", 2, "alpha finding")}, nil
	case strings.Contains(req.Prompt, `"id": "harness:beta"`):
		a.waitReviewerStart("harness:beta")
		if a.betaProviderErr != nil {
			return staticStream{sessionID: "beta-provider-session", err: a.betaProviderErr}, nil
		}
		a.mu.Lock()
		a.betaAttempts++
		attempt := a.betaAttempts
		a.mu.Unlock()
		if req.ReviewerWorkspace != nil {
			markerPath := filepath.Join(req.ReviewerWorkspace.RepoDir, "beta-attempt-marker")
			if attempt == 1 {
				if err := os.WriteFile(markerPath, []byte("dirty"), 0o600); err != nil {
					return nil, err
				}
			} else {
				_, err := os.Stat(markerPath)
				a.mu.Lock()
				a.betaRetrySawCleanWorkspace = errors.Is(err, os.ErrNotExist)
				a.mu.Unlock()
			}
		}
		sessionID := "beta-session"
		if attempt > 1 {
			sessionID = "beta-retry-session"
		}
		return staticStream{sessionID: sessionID, output: `{"schema_version": 1, "agent_id": "harness:beta", "findings": [`}, nil
	case strings.Contains(req.Prompt, `"id": "harness:gamma"`):
		a.waitReviewerStart("harness:gamma")
		return staticStream{sessionID: "gamma-session", output: findingsJSON("harness:gamma", "main.go", "minor", 2, "gamma finding")}, nil
	default:
		return nil, fmt.Errorf("unexpected prompt: %s", req.Prompt)
	}
}

type reviewerWorkspaceSmokeAdapter struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (a *reviewerWorkspaceSmokeAdapter) Name() string         { return "reviewer-workspace-smoke" }
func (a *reviewerWorkspaceSmokeAdapter) SupportsResume() bool { return false }
func (a *reviewerWorkspaceSmokeAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}
func (a *reviewerWorkspaceSmokeAdapter) SupportsCacheAccounting() bool { return false }
func (a *reviewerWorkspaceSmokeAdapter) SupportsCostReporting() bool   { return false }
func (a *reviewerWorkspaceSmokeAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}
func (a *reviewerWorkspaceSmokeAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("resume unsupported")
}
func (a *reviewerWorkspaceSmokeAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	workspace := req.ReviewerWorkspace
	if workspace == nil {
		return nil, errors.New("missing reviewer workspace request")
	}
	mainBytes, err := os.ReadFile(filepath.Join(workspace.RepoDir, "main.go")) // #nosec G304 -- test adapter reads only caller-provided test workspace roots.
	if err != nil {
		return nil, err
	}
	_, otherReadErr := os.ReadFile(filepath.Join(workspace.RepoDir, "other.go"))                                   // #nosec G304 -- test adapter probes only caller-provided test workspace roots.
	trackedWriteErr := os.WriteFile(filepath.Join(workspace.RepoDir, "main.go"), []byte("mutated"), 0o600)         // #nosec G304,G306 -- test adapter intentionally probes disposable workspace writes.
	untrackedWriteErr := os.WriteFile(filepath.Join(workspace.RepoDir, "untracked.txt"), []byte("mutated"), 0o600) // #nosec G304,G306 -- test adapter intentionally probes disposable workspace writes.
	scratchPath := filepath.Join(workspace.ScratchDir, "smoke-output.txt")
	scratchWriteErr := os.WriteFile(scratchPath, []byte("scratch-ok"), 0o600) // #nosec G306 -- test adapter writes only to caller-owned scratch.
	output := fmt.Sprintf(`{"read_ok":%t,"main_contains_changed":%t,"out_of_scope_readable":%t,"tracked_write_ok":%t,"untracked_write_ok":%t,"scratch_write_ok":%t,"max_tool_output_bytes":%d}`,
		true,
		strings.Contains(string(mainBytes), "var changed = true"),
		otherReadErr == nil,
		trackedWriteErr == nil,
		untrackedWriteErr == nil,
		scratchWriteErr == nil,
		workspace.MaxToolOutputBytes,
	)
	return staticStream{sessionID: "workspace-smoke-session", output: output}, nil
}
func (a *reviewerWorkspaceSmokeAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

func (a *reviewerIsolationAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

func (a *reviewerIsolationAdapter) waitReviewerStart(agentID string) {
	if a.reviewerBarrier != nil {
		a.reviewerBarrier.wait(agentID)
	}
}

func (a *reviewerIsolationAdapter) ReviewerStartedCount() int {
	if a.reviewerBarrier == nil {
		return 0
	}
	return a.reviewerBarrier.startedCount()
}

func (a *reviewerIsolationAdapter) BetaRetrySawCleanWorkspace() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.betaRetrySawCleanWorkspace
}

type reviewerStartBarrier struct {
	mu      sync.Mutex
	want    int
	started map[string]bool
	release chan struct{}
	closed  bool
}

func newReviewerStartBarrier(want int) *reviewerStartBarrier {
	return &reviewerStartBarrier{
		want:    want,
		started: map[string]bool{},
		release: make(chan struct{}),
	}
}

func (b *reviewerStartBarrier) wait(agentID string) {
	b.mu.Lock()
	if !b.started[agentID] {
		b.started[agentID] = true
	}
	if !b.closed && len(b.started) >= b.want {
		close(b.release)
		b.closed = true
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

func (b *reviewerStartBarrier) startedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.started)
}

type staticStream struct {
	sessionID string
	output    string
	err       error
}

func (s staticStream) SessionID() string {
	return s.sessionID
}

func (s staticStream) Wait(context.Context) (llm.Response, error) {
	if s.err != nil {
		return llm.Response{DurationMS: 1}, s.err
	}
	return llm.Response{StructuredOutput: []byte(s.output), DurationMS: 1}, nil
}

type failingStore struct {
	*ledger.Store
	insertPlannedActionErr error
}

func (s *failingStore) InsertPlannedAction(ctx context.Context, action ledger.PlannedAction) error {
	if s.insertPlannedActionErr != nil {
		return s.insertPlannedActionErr
	}
	return s.Store.InsertPlannedAction(ctx, action)
}

func (s *failingStore) InsertPlanningResult(ctx context.Context, findings []ledger.Finding, actions []ledger.PlannedAction) error {
	if s.insertPlannedActionErr != nil {
		return s.insertPlannedActionErr
	}
	return s.Store.InsertPlanningResult(ctx, findings, actions)
}

type completeFailingStore struct {
	*ledger.Store
	err error
}

func (s *completeFailingStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	return s.err
}

type fileKey struct {
	gitRef string
	path   string
}

func (p *readOnlyProvider) GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error) {
	if p.onGetPR != nil {
		p.onGetPR()
	}
	return p.pr, nil
}

func (p *readOnlyProvider) GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	p.diffCalls++
	return p.diff, nil
}

func (p *readOnlyProvider) GetDiffBetweenRefs(_ context.Context, _ gitprovider.PRRef, baseSHA, headSHA string) (gitprovider.UnifiedDiff, error) {
	p.diffBetweenCalls = append(p.diffBetweenCalls, shaPair{baseSHA: baseSHA, headSHA: headSHA})
	return p.diffBetween, nil
}

func (p *readOnlyProvider) GetFileAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	p.fileCalls = append(p.fileCalls, fileKey{gitRef: gitRef, path: path})
	data, ok := p.files[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (p *readOnlyProvider) ListTreeAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error) {
	p.treeCalls = append(p.treeCalls, fileKey{gitRef: gitRef, path: path})
	entries, ok := p.trees[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]gitprovider.TreeEntry(nil), entries...), nil
}

func (p *readOnlyProvider) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	p.threadCalls++
	return append([]gitprovider.InlineThread(nil), p.threads...), nil
}

func (p *readOnlyProvider) ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error) {
	p.reviewCalls++
	return append([]gitprovider.Review(nil), p.reviews...), nil
}

func (p *readOnlyProvider) ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	p.issueCommentCalls++
	return append([]gitprovider.IssueComment(nil), p.issueComments...), nil
}

func (p *readOnlyProvider) Capabilities() gitprovider.ProviderCaps {
	return p.caps
}

func containsFileCall(calls []fileKey, want fileKey) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

type workbenchGitFixture struct {
	repoDir string
	baseSHA string
	headSHA string
	pr      gitprovider.PR
}

type forkWorkbenchFixture struct {
	sourceRepoDir  string
	baseRemotePath string
	forkRemotePath string
	pr             gitprovider.PR
}

func newWorkbenchGitFixture(t *testing.T) workbenchGitFixture {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 370}
	return newWorkbenchGitFixtureForRef(t, ref)
}

func newWorkbenchGitFixtureForRef(t *testing.T, ref gitprovider.PRRef) workbenchGitFixture {
	t.Helper()
	repoDir := t.TempDir()
	gitCommandMustSucceed(t, repoDir, "init", "-b", "main")
	gitCommandMustSucceed(t, repoDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, repoDir, "config", "user.email", "workbench@example.com")
	gitCommandMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\n\nvar helper = true\n"), 0o600); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "add", "main.go", "other.go")
	gitCommandMustSucceed(t, repoDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	gitCommandMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))

	return workbenchGitFixture{
		repoDir: repoDir,
		baseSHA: baseSHA,
		headSHA: headSHA,
		pr: gitprovider.PR{
			Ref:   ref,
			Title: "Workbench fixture",
			URL:   prURL(ref),
			State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "main",
				Ref:   "refs/heads/main",
				SHA:   baseSHA,
			},
			Head: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "feature",
				Ref:   "refs/heads/feature",
				SHA:   headSHA,
			},
		},
	}
}

func newPinnedReviewFixtureForRef(t *testing.T, ref gitprovider.PRRef) (workbenchGitFixture, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	gitCommandMustSucceed(t, repoDir, "init", "-b", "main")
	gitCommandMustSucceed(t, repoDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, repoDir, "config", "user.email", "workbench@example.com")
	gitCommandMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "add", "main.go")
	gitCommandMustSucceed(t, repoDir, "commit", "-m", "base")
	reviewBaseSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = maybe\n"), 0o600); err != nil {
		t.Fatalf("update main.go for review head: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "review head")
	reviewHeadSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	gitCommandMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go for current head: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "current head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))

	return workbenchGitFixture{
		repoDir: repoDir,
		baseSHA: reviewHeadSHA,
		headSHA: headSHA,
		pr: gitprovider.PR{
			Ref:   ref,
			Title: "Pinned review fixture",
			URL:   prURL(ref),
			State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "main",
				Ref:   "refs/heads/main",
				SHA:   reviewHeadSHA,
			},
			Head: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "feature",
				Ref:   "refs/heads/feature",
				SHA:   headSHA,
			},
		},
	}, reviewBaseSHA, reviewHeadSHA
}

func newForkWorkbenchFixture(t *testing.T) forkWorkbenchFixture {
	t.Helper()
	baseSeedDir := t.TempDir()
	gitCommandMustSucceed(t, baseSeedDir, "init", "-b", "main")
	gitCommandMustSucceed(t, baseSeedDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, baseSeedDir, "config", "user.email", "workbench@example.com")
	if err := os.WriteFile(filepath.Join(baseSeedDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	gitCommandMustSucceed(t, baseSeedDir, "add", "main.go")
	gitCommandMustSucceed(t, baseSeedDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(gitCommandOutput(t, baseSeedDir, "rev-parse", "HEAD"))

	baseRemotePath := filepath.Join(t.TempDir(), "base-remote.git")
	gitCommandMustSucceed(t, "", "clone", "--bare", baseSeedDir, baseRemotePath)

	sourceRepoDir := filepath.Join(t.TempDir(), "source")
	gitCommandMustSucceed(t, "", "clone", baseRemotePath, sourceRepoDir)
	gitCommandMustSucceed(t, sourceRepoDir, "remote", "set-url", "origin", "git@github.com:open-cli-collective/codereview-cli.git")

	forkRemotePath := filepath.Join(t.TempDir(), "fork-remote.git")
	gitCommandMustSucceed(t, "", "clone", baseRemotePath, forkRemotePath)
	gitCommandMustSucceed(t, forkRemotePath, "checkout", "-b", "feature")
	gitCommandMustSucceed(t, forkRemotePath, "config", "user.name", "Fork Workbench Test")
	gitCommandMustSucceed(t, forkRemotePath, "config", "user.email", "fork@example.com")
	if err := os.WriteFile(filepath.Join(forkRemotePath, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update fork main.go: %v", err)
	}
	gitCommandMustSucceed(t, forkRemotePath, "commit", "-am", "fork head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, forkRemotePath, "rev-parse", "HEAD"))

	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 371}
	return forkWorkbenchFixture{
		sourceRepoDir:  sourceRepoDir,
		baseRemotePath: baseRemotePath,
		forkRemotePath: forkRemotePath,
		pr: gitprovider.PR{
			Ref:   ref,
			Title: "Fork workbench fixture",
			URL:   prURL(ref),
			State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "main",
				Ref:   "refs/heads/main",
				SHA:   baseSHA,
			},
			Head: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: "fork-owner",
				Repo:  "codereview-cli-fork",
				Name:  "feature",
				Ref:   "refs/heads/feature",
				SHA:   headSHA,
			},
		},
	}
}

func gitCommandMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitCommandOutput(t, dir, args...))
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func configureWorkbenchFixtureForTest(_ context.Context, opts *Options, ref gitprovider.PRRef) {
	if opts.ResolveRepoRoot != nil && opts.GitCommand != nil {
		return
	}
	provider, ok := opts.Provider.(*readOnlyProvider)
	if !ok || strings.TrimSpace(provider.fixtureRepoDir) == "" {
		return
	}
	repoDir := provider.fixtureRepoDir
	if opts.ResolveRepoRoot == nil {
		opts.ResolveRepoRoot = func(context.Context) (string, error) {
			return repoDir, nil
		}
	}
	if opts.GitCommand == nil {
		opts.GitCommand = workbenchGitCommandForTest(ref)
	}
}

func workbenchGitCommandForTest(ref gitprovider.PRRef) func(context.Context, string, ...string) ([]byte, error) {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "remote" && args[1] == "get-url" && args[2] == "origin" {
			return []byte(fmt.Sprintf("https://%s/%s/%s.git\n", ref.Host, ref.Owner, ref.Repo)), nil
		}
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			message := strings.TrimSpace(string(out))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
		}
		return out, nil
	}
}

func dryRunHarness(t *testing.T) (*readOnlyProvider, Request) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	fixture := newWorkbenchGitFixtureForRef(t, ref)
	pr := fixture.pr
	pr.Title = "CR-20 dry-run"
	pr.Body = "Default PR body."
	pr.Author = gitprovider.Identity{Login: "author", ID: "author-id"}
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "reviewer", "reviewer desc", "Review carefully.")
	trustCurrentTempFixtures(t)
	provider := &readOnlyProvider{
		pr:             pr,
		diff:           gitprovider.UnifiedDiff{Raw: smallDiff("main.go")},
		files:          map[fileKey][]byte{},
		trees:          map[fileKey][]gitprovider.TreeEntry{},
		caps:           gitprovider.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true},
		fixtureRepoDir: fixture.repoDir,
	}
	req := Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         testProfile(dir),
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
	}
	return provider, req
}

func selectionRequestFromReview(req Request, artifactDir string) SelectionRequest {
	return SelectionRequest{
		PRRef:                       req.PRRef,
		ProfileName:                 req.ProfileName,
		Profile:                     req.Profile,
		PostingIdentity:             req.PostingIdentity,
		AgentDirs:                   append([]string(nil), req.AgentDirs...),
		ArtifactDir:                 artifactDir,
		ReviewBaseSHA:               req.ReviewBaseSHA,
		ReviewHeadSHA:               req.ReviewHeadSHA,
		SelectionModelOverride:      req.SelectionModelOverride,
		SelectionEffortOverride:     req.SelectionEffortOverride,
		SelectionPromptInstructions: req.SelectionPromptInstructions,
	}
}

func trustCurrentTempFixtures(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
}

func allocateLiveRun(t *testing.T, store *ledger.Store, provider *readOnlyProvider, req Request, runID string) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), runID),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func namedSessionForRequest(req Request, providerSessionID string) ledger.NamedSession {
	return ledger.NamedSession{
		Name:              req.SessionName,
		Profile:           req.ProfileName,
		Provider:          string(req.Profile.LLM.Provider),
		Adapter:           "fake-llm",
		Model:             "claude-sonnet-4-6",
		Host:              req.PRRef.Host,
		ProviderSessionID: providerSessionID,
		CreatedAt:         fixedNow(),
		LastUsedAt:        fixedNow(),
	}
}

func testProfile(agentSource string) config.Profile {
	profile := config.Profile{
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
		ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
	}
	if agentSource != "" {
		profile.AgentSources = []string{agentSource}
	}
	return profile
}

func openPipelineStore(t *testing.T) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return store
}

func closeStore(t *testing.T, store *ledger.Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
}

func assertRollupUsageRow(t *testing.T, path string, workstream string, wantCacheCreate bool) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads an artifact path produced under t.TempDir.
	if err != nil {
		t.Fatalf("read rollup markdown: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		cells := markdownTableCells(line)
		if len(cells) < 8 || cells[0] != workstream {
			continue
		}
		for _, idx := range []int{2, 3, 4} {
			if cells[idx] == "" || cells[idx] == "unavailable" {
				t.Fatalf("rollup usage row %q cell %d = %q, want populated token/cache value in line %q", workstream, idx, cells[idx], line)
			}
		}
		if wantCacheCreate && (cells[5] == "" || cells[5] == "unavailable") {
			t.Fatalf("rollup usage row %q cache create = %q, want populated value in line %q", workstream, cells[5], line)
		}
		if !wantCacheCreate && cells[5] != "unavailable" {
			t.Fatalf("rollup usage row %q cache create = %q, want unavailable when provider omitted it in line %q", workstream, cells[5], line)
		}
		return
	}
	t.Fatalf("rollup markdown %s missing usage row for %q:\n%s", path, workstream, data)
}

func markdownTableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

func allocatePipelineRun(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	artifactPath := filepath.Join(layout.DataRoot, "runs", "github_open-cli_codereview-cli_29", strings.Repeat("a", 40), strings.Repeat("b", 40), "home__review-bot", runID)
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           "github_open-cli_codereview-cli_29",
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/29",
		RunID:           runID,
		SHA:             strings.Repeat("a", 40),
		BaseSHA:         strings.Repeat("b", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func allocateDryRunForProvider(t *testing.T, store *ledger.Store, layout statepaths.Layout, provider *readOnlyProvider, req Request, runID string, started time.Time) ledger.Run {
	t.Helper()
	return allocateDryRunForSHAs(t, store, layout, req, runID, provider.pr.Head.SHA, provider.pr.Base.SHA, started)
}

func allocateDryRunForSHAs(t *testing.T, store *ledger.Store, layout statepaths.Layout, req Request, runID, headSHA, baseSHA string, started time.Time) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	artifactPath := filepath.Join(layout.DataRoot, "runs", prKey, headSHA, baseSHA, statepaths.Encode(req.ProfileName)+"__"+statepaths.Encode(postingKey(req.PostingIdentity)), runID)
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             headSHA,
		BaseSHA:         baseSHA,
		Profile:         req.ProfileName,
		PostingIdentity: postingKey(req.PostingIdentity),
		PostMode:        ledger.PostModeDryRun,
		StartedAt:       started,
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	if err := runartifact.WriteMarker(run.ArtifactPath, runartifact.KindReview, run.RunID); err != nil {
		t.Fatalf("WriteMarker review: %v", err)
	}
	return run
}

func removeReviewRunMarkerForTest(t *testing.T, artifactPath string) {
	t.Helper()
	if err := os.Remove(runartifact.MarkerPath(artifactPath, runartifact.KindReview)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove review marker: %v", err)
	}
}

func writeResponseRunMarkerForTest(t *testing.T, artifactPath, runID string) {
	t.Helper()
	if err := runartifact.WriteMarker(artifactPath, runartifact.KindThreadResponse, runID); err != nil {
		t.Fatalf("WriteMarker response: %v", err)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func sequence(prefix string) func() string {
	var counter int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%s-%d", prefix, counter)
	}
}

func findingSequence(prefix string) func() (review.FindingID, error) {
	next := sequence(prefix)
	return func() (review.FindingID, error) {
		return review.FindingID(next()), nil
	}
}

func actionSequence() func(reviewplan.ActionKind) (string, error) {
	counters := map[reviewplan.ActionKind]int{}
	return func(kind reviewplan.ActionKind) (string, error) {
		counters[kind]++
		return fmt.Sprintf("%s-%d", kind, counters[kind]), nil
	}
}

func markedReviewThread(t *testing.T, id, path string, line int, bot, human gitprovider.Identity) gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "old-run",
		ActionID: "old-action",
		Kind:     marker.ActionKindInlineComment,
		SHA:      strings.Repeat("a", 40),
		BaseSHA:  strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	threadID := gitprovider.ThreadID(id)
	created := fixedNow()
	return gitprovider.InlineThread{
		ID:          threadID,
		Resolved:    false,
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   strings.Repeat("a", 40),
		Comments: []gitprovider.ThreadComment{
			{
				ID:          gitprovider.CommentID(id + "-cr"),
				ThreadID:    threadID,
				Body:        action + "\nOriginal finding.",
				Author:      bot,
				CommitSHA:   strings.Repeat("a", 40),
				Path:        path,
				Side:        review.DiffSideRight,
				Line:        line,
				SubjectType: review.AnchorKindLine,
				CreatedAt:   created,
				UpdatedAt:   created,
			},
			{
				ID:          gitprovider.CommentID(id + "-human"),
				ThreadID:    threadID,
				Body:        "Human reply",
				Author:      human,
				CommitSHA:   strings.Repeat("a", 40),
				Path:        path,
				Side:        review.DiffSideRight,
				Line:        line,
				SubjectType: review.AnchorKindLine,
				CreatedAt:   created.Add(time.Minute),
				UpdatedAt:   created.Add(time.Minute),
			},
		},
	}
}

func crSettledReviewThread(t *testing.T, id, path string, line int, bot, human gitprovider.Identity, summary string) gitprovider.InlineThread {
	t.Helper()
	thread := markedReviewThread(t, id, path, line, bot, human)
	summaryMarker, err := marker.RenderThreadSummary(marker.ThreadSummaryMarker{
		RunID:    "response-run",
		ActionID: "summary-" + id,
	})
	if err != nil {
		t.Fatalf("RenderThreadSummary: %v", err)
	}
	created := fixedNow().Add(2 * time.Minute)
	thread.Comments = append(thread.Comments, gitprovider.ThreadComment{
		ID:          gitprovider.CommentID(id + "-summary"),
		ThreadID:    thread.ID,
		Body:        summaryMarker + "\n\n" + summary,
		Author:      bot,
		CommitSHA:   strings.Repeat("a", 40),
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CreatedAt:   created,
		UpdatedAt:   created,
	})
	return thread
}

func fakeLLMResult(sessionID, structured string, tokensIn, tokensOut int) llm.FakeResult {
	return llm.FakeResult{
		SessionID: sessionID,
		Response: llm.Response{
			StructuredOutput: []byte(structured),
			Usage:            llm.Usage{TokensIn: intPtr(tokensIn), TokensOut: intPtr(tokensOut)},
			DurationMS:       123,
		},
	}
}

type providerOriginUsageAdapter struct {
	mu       sync.Mutex
	name     string
	adapters []llm.Adapter
}

func (a *providerOriginUsageAdapter) Queue(adapter llm.Adapter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adapters = append(a.adapters, adapter)
}

func (a *providerOriginUsageAdapter) Name() string {
	if strings.TrimSpace(a.name) != "" {
		return a.name
	}
	return "provider_origin_usage"
}

func (a *providerOriginUsageAdapter) SupportsResume() bool { return false }

func (a *providerOriginUsageAdapter) SupportsCacheAccounting() bool { return false }

func (a *providerOriginUsageAdapter) SupportsCostReporting() bool { return false }

func (a *providerOriginUsageAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspacePermissionBounded
}

func (a *providerOriginUsageAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *providerOriginUsageAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	if len(a.adapters) == 0 {
		a.mu.Unlock()
		return nil, fmt.Errorf("provider origin usage adapter: no queued adapter")
	}
	adapter := a.adapters[0]
	a.adapters = a.adapters[1:]
	a.mu.Unlock()
	return adapter.Start(ctx, req)
}

func (a *providerOriginUsageAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, fmt.Errorf("provider origin usage adapter: resume unsupported")
}

func newCodexUsageScriptAdapter(t *testing.T, sessionID string, structured string, usage llm.Usage) llm.Adapter {
	t.Helper()
	script := writeExecutableScript(t, "codex-usage", codexUsageScript(t, sessionID, structured, usage))
	return llm.NewCodexCLIAdapter(llm.SubprocessOptions{
		Command:                script,
		Timeout:                5 * time.Second,
		AllowBestEffortNoTools: true,
	})
}

func newClaudeTranscriptScriptAdapter(t *testing.T, sessionID string, structured string, usage llm.Usage) llm.Adapter {
	t.Helper()
	configDir := t.TempDir()
	workDir := t.TempDir()
	transcriptPath := writeClaudeUsageTranscript(t, usage)
	state := map[string]any{
		"state":        "done",
		"sessionId":    sessionID,
		"linkScanPath": transcriptPath,
		"createdAt":    "2026-06-09T20:00:00Z",
	}
	stateJSON := mustMarshalJSON(t, state)
	script := writeExecutableScript(t, "claude-transcript", claudeTranscriptScript(sessionID, structured, stateJSON))
	return llm.NewClaudeCLIAdapter(llm.SubprocessOptions{
		Command: script,
		Env: []string{
			"CLAUDE_CONFIG_DIR=" + configDir,
			"CR_CLAUDE_BG_WORK_DIR=" + workDir,
		},
		Timeout: 5 * time.Second,
	})
}

func codexUsageScript(t *testing.T, sessionID string, structured string, usage llm.Usage) string {
	t.Helper()
	usageFields := []string{
		fmt.Sprintf(`"input_tokens":%d`, mustInt(t, usage.TokensIn, "TokensIn")),
		fmt.Sprintf(`"output_tokens":%d`, mustInt(t, usage.TokensOut, "TokensOut")),
	}
	if usage.CacheRead != nil {
		usageFields = append(usageFields, fmt.Sprintf(`"cached_input_tokens":%d`, *usage.CacheRead))
	}
	if usage.CacheCreate != nil {
		usageFields = append(usageFields, fmt.Sprintf(`"cache_create":%d`, *usage.CacheCreate))
	}
	return fmt.Sprintf(`#!/bin/sh
cat <<'JSONL'
{"type":"thread.started","thread_id":%s}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":%s}}
{"type":"turn.completed","usage":{%s,"reasoning_output_tokens":271}}
JSONL
`, mustMarshalJSON(t, sessionID), mustMarshalJSON(t, structured), strings.Join(usageFields, ","))
}

func claudeTranscriptScript(sessionID string, structured string, stateJSON string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in
  stop|rm) exit 0 ;;
  agents) printf '[]'; exit 0 ;;
esac
add_dir=""
want_add_dir=0
for arg in "$@"; do
  if [ "$want_add_dir" = "1" ]; then
    if [ -z "$add_dir" ]; then add_dir="$arg"; fi
    want_add_dir=0
    continue
  fi
  if [ "$arg" = "--add-dir" ]; then want_add_dir=1; fi
done
job_id="job-%s"
mkdir -p "$CLAUDE_CONFIG_DIR/jobs/$job_id" "$add_dir"
cat > "$CLAUDE_CONFIG_DIR/jobs/$job_id/state.json" <<'STATE'
%s
STATE
cat > "$add_dir/cr-result.json" <<'RESULT'
%s
RESULT
printf 'backgrounded * %%s\n  claude attach %%s\n' "$job_id" "$job_id"
`, sessionID, stateJSON, structured)
}

func writeClaudeUsageTranscript(t *testing.T, usage llm.Usage) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-transcript.jsonl")
	line := map[string]any{
		"type":      "assistant",
		"timestamp": "2026-06-09T20:00:02Z",
		"message": map[string]any{
			"id": "message-1",
			"usage": map[string]any{
				"input_tokens":                mustInt(t, usage.TokensIn, "TokensIn"),
				"output_tokens":               mustInt(t, usage.TokensOut, "TokensOut"),
				"cache_read_input_tokens":     mustInt(t, usage.CacheRead, "CacheRead"),
				"cache_creation_input_tokens": mustInt(t, usage.CacheCreate, "CacheCreate"),
			},
		},
	}
	data := mustMarshalJSON(t, line) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write Claude usage transcript: %v", err)
	}
	return path
}

func writeExecutableScript(t *testing.T, name string, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s script: %v", name, err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- test helper script must be executable and lives under t.TempDir.
		t.Fatalf("chmod %s script: %v", name, err)
	}
	return path
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}

func mustInt(t *testing.T, value *int, name string) int {
	t.Helper()
	if value == nil {
		t.Fatalf("%s must be set", name)
	}
	return *value
}

func selectionJSON(agentID, file string) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"selected_agents": [{
			"agent_id": %q,
			"rationale": "go file changed",
			"files": [%q]
		}],
		"thread_actions": [],
		"reasoning": "select reviewer"
	}`, agentID, file)
}

func selectionJSONForAgents(file string, agentIDs ...string) string {
	selected := make([]map[string]any, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		selected = append(selected, map[string]any{
			"agent_id":  agentID,
			"rationale": "go file changed",
			"files":     []string{file},
		})
	}
	payload := map[string]any{
		"schema_version":  1,
		"selected_agents": selected,
		"thread_actions":  []any{},
		"reasoning":       "select reviewers",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func findingsJSON(agentID, file, severity string, line int, body string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": []string{file},
		"skipped_files":   []string{},
		"constraints":     []string{},
		"findings": []map[string]any{{
			"severity":  severity,
			"file_path": file,
			"anchor": map[string]any{
				"kind": "line",
				"side": "RIGHT",
				"line": line,
			},
			"body": body,
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func coverageOnlyJSON(agentID string, inspected, skipped []string, constraints ...string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": inspected,
		"skipped_files":   skipped,
		"constraints":     constraints,
		"findings":        []map[string]any{},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func largeRollupFindings(count int, filePath, body string) []review.Finding {
	out := make([]review.Finding, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, review.Finding{
			ID:       review.FindingID(fmt.Sprintf("finding-%d", i+1)),
			Severity: review.SeverityMajor,
			FilePath: filePath,
			Anchor: review.Anchor{
				Kind: review.AnchorKindLine,
				Side: review.DiffSideRight,
				Line: i + 2,
			},
			Body: body,
		})
	}
	return out
}

func findingsFileAliasJSON(agentID, file, severity string, line int, body string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": []string{file},
		"skipped_files":   []string{},
		"constraints":     []string{},
		"findings": []map[string]any{{
			"severity": severity,
			"file":     file,
			"anchor": map[string]any{
				"kind": "line",
				"side": "RIGHT",
				"line": line,
			},
			"body": body,
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func rollupJSON(event string, ordered []string) string {
	payload := map[string]any{
		"schema_version":         1,
		"review_event":           event,
		"review_event_rationale": "policy",
		"dedupe_log":             []any{},
		"ordered_findings":       ordered,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

type threadSummary struct {
	path       string
	side       string
	line       int
	anchorKind string
	resolved   bool
	status     string
	summary    string
}

func discussionSummaryJSON(topLevel []string, threads []threadSummary) string {
	topLevelPayload := make([]map[string]any, 0, len(topLevel))
	for _, summary := range topLevel {
		topLevelPayload = append(topLevelPayload, map[string]any{
			"kind":    "issue_comment",
			"author":  "reviewer",
			"summary": summary,
		})
	}
	threadPayload := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		side := thread.side
		if side == "" {
			side = string(review.DiffSideRight)
		}
		anchorKind := thread.anchorKind
		if anchorKind == "" {
			anchorKind = string(review.AnchorKindLine)
		}
		threadPayload = append(threadPayload, map[string]any{
			"path":        thread.path,
			"side":        side,
			"line":        thread.line,
			"anchor_kind": anchorKind,
			"resolved":    thread.resolved,
			"status":      thread.status,
			"summary":     thread.summary,
		})
	}
	payload := map[string]any{
		"schema_version":     dossierSummarySchemaVersion,
		"top_level_comments": topLevelPayload,
		"inline_threads":     threadPayload,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func parseDiffPatchesForTest(t *testing.T, raw string) []FilePatch {
	t.Helper()
	parsed, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	return parsed.Patches
}

func smallDiff(path string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"index 1111111..2222222 100644",
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -1,2 +1,2 @@",
		" package main",
		"-var changed = false",
		"+var changed = true",
		"",
	}, "\n")
}

func largeDiff(path, body string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"index 1111111..2222222 100644",
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -1,1 +1,400 @@",
		"-package main",
		body,
		"",
	}, "\n")
}

func writeAgent(t *testing.T, rootDir, category, agent, description, prompt string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentYAML(agent, description, false))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), prompt)
}

func relativeAgentSource(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	source := filepath.Join(cwd, "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	t.Chdir(cwd)
	return "agents"
}

func tempAgentSource(t *testing.T) string {
	t.Helper()
	tempRoot := os.TempDir()
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll temp root: %v", err)
	}
	source, err := os.MkdirTemp(tempRoot, "codereview-agents-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(source); err != nil {
			t.Fatalf("RemoveAll temp agent source: %v", err)
		}
	})
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	return source
}

func gitWorktreeAgentSource(t *testing.T) string {
	t.Helper()
	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o700); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	source := filepath.Join(repoRoot, "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	return source
}

func writeAgentFullContent(t *testing.T, rootDir, category, agent string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentYAML(agent, "full content reviewer", true))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), "Review full files.")
}

func writeAgentModelID(t *testing.T, rootDir, category, agent, modelID string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), fmt.Sprintf("name: %s\ndescription: %s desc\nmodel_id: %s\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n", agent, agent, modelID))
}

func writeAgentWithModelTier(t *testing.T, rootDir, category, agent, modelTier string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), fmt.Sprintf("name: %s\ndescription: %s desc\nmodel_tier: %s\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n", agent, agent, modelTier))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), "Review carefully.")
}

func agentYAML(name, description string, needsFullContent bool) string {
	return fmt.Sprintf("name: %s\ndescription: %s\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: %t\n", name, description, needsFullContent)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("file %s = %q, want substring %q", path, data, want)
	}
}

func assertAgentSourcesArtifact(t *testing.T, path, wantAgent string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided artifact paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var artifact agentSourcesArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("Unmarshal agent sources artifact: %v\n%s", err, data)
	}
	profileSource, ok := findArtifactSource(artifact.Sources, agents.SourceProfile)
	if !ok {
		t.Fatalf("artifact sources = %#v, want profile source", artifact.Sources)
	}
	if profileSource.Status != agents.SourceStatusAvailable || profileSource.Fingerprint == "" || profileSource.CanonicalPath == "" || len(profileSource.Warnings) != 0 {
		t.Fatalf("profile source = %#v, want trusted available source with fingerprint and canonical path", profileSource)
	}
	repoSource, ok := findArtifactSource(artifact.Sources, agents.SourceRepo)
	if !ok {
		t.Fatalf("artifact sources = %#v, want repo source", artifact.Sources)
	}
	if repoSource.Status != agents.SourceStatusMissing || repoSource.Present || repoSource.SHA == "" {
		t.Fatalf("repo source = %#v, want missing repo source anchored to base SHA", repoSource)
	}
	for _, agent := range artifact.Agents {
		if agent.ID == wantAgent &&
			agent.Source.Fingerprint == profileSource.Fingerprint &&
			agent.Source.CanonicalPath == profileSource.CanonicalPath &&
			agent.Source.Status == agents.SourceStatusAvailable {
			return
		}
	}
	t.Fatalf("artifact agents = %#v, want %s with exact profile source provenance", artifact.Agents, wantAgent)
}

func assertReviewerRuntimeArtifact(t *testing.T, path, wantAgent string, want reviewerRuntimeResolution) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided artifact paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var artifact agentSourcesArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("Unmarshal agent sources artifact: %v\n%s", err, data)
	}
	for _, agent := range artifact.Agents {
		if agent.ID != wantAgent {
			continue
		}
		if agent.ReviewerRuntime == nil {
			t.Fatalf("agent %s reviewer runtime = nil, want %#v", wantAgent, want)
		}
		if *agent.ReviewerRuntime != want {
			t.Fatalf("agent %s reviewer runtime = %#v, want %#v", wantAgent, *agent.ReviewerRuntime, want)
		}
		return
	}
	t.Fatalf("artifact agents = %#v, want reviewer runtime for %s", artifact.Agents, wantAgent)
}

func assertDossierIndexArtifact(t *testing.T, dir, wantPath string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json")) // #nosec G304 -- test reads artifact path under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(dossier index): %v", err)
	}
	var index dossierIndexArtifact
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("Unmarshal dossier index: %v\n%s", err, data)
	}
	if index.HashAlgorithm != "sha256" {
		t.Fatalf("hash algorithm = %q, want sha256", index.HashAlgorithm)
	}
	if len(index.Files) == 0 {
		t.Fatal("dossier index files = 0, want artifacts")
	}
	wantHashes := map[string]string{}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Base(path) == "index.json" {
			return nil
		}
		fileData, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		wantHashes[filepath.ToSlash(path)] = sha256Hex(fileData)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(dossier): %v", err)
	}
	var saw bool
	for _, file := range index.Files {
		if file.Path == wantPath {
			saw = true
		}
		if file.Path == "" || file.SHA256 == "" {
			t.Fatalf("index file = %#v, want non-empty path/hash", file)
		}
		wantHash, ok := wantHashes[file.Path]
		if !ok {
			t.Fatalf("index file = %#v, want tracked dossier artifact", file)
		}
		if file.SHA256 != wantHash {
			t.Fatalf("index hash for %s = %q, want %q", file.Path, file.SHA256, wantHash)
		}
		delete(wantHashes, file.Path)
	}
	if !saw {
		t.Fatalf("dossier index files = %#v, want %q", index.Files, wantPath)
	}
	if len(wantHashes) != 0 {
		t.Fatalf("dossier index missing files = %#v", wantHashes)
	}
}

func assertFileOmits(t *testing.T, path string, forbidden ...string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact path returned by pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	text := string(data)
	for _, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Fatalf("artifact %s contains forbidden substring %q:\n%s", path, needle, text)
		}
	}
}

func findArtifactSource(sources []agents.SourceInfo, kind agents.SourceKind) (agents.SourceInfo, bool) {
	for _, source := range sources {
		if source.Kind == kind {
			return source, true
		}
	}
	return agents.SourceInfo{}, false
}

func assertPromptOmitsLocalAgentSourceProvenance(t *testing.T, prompt string, sources []agents.SourceInfo) {
	t.Helper()
	for _, forbidden := range []string{
		"configured_path",
		"canonical_path",
		"Source warning",
		"OS temp directory",
		"Git worktree",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains local source provenance %q:\n%s", forbidden, prompt)
		}
	}
	for _, source := range sources {
		for _, forbidden := range []string{source.ConfiguredPath, source.CanonicalPath, source.Fingerprint} {
			if forbidden != "" && strings.Contains(prompt, forbidden) {
				t.Fatalf("prompt contains local source value %q:\n%s", forbidden, prompt)
			}
		}
		for _, warning := range source.Warnings {
			if warning != "" && strings.Contains(prompt, warning) {
				t.Fatalf("prompt contains local source warning %q:\n%s", warning, prompt)
			}
		}
	}
}

func prURL(ref gitprovider.PRRef) string {
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", ref.Host, ref.Owner, ref.Repo, ref.Number)
}

func intPtr(value int) *int {
	return &value
}

func TestReadOnlyProviderDoesNotSatisfyGitProvider(t *testing.T) {
	var provider any = &readOnlyProvider{}
	if _, ok := provider.(gitprovider.GitProvider); ok {
		t.Fatal("readOnlyProvider unexpectedly satisfies gitprovider.GitProvider")
	}
	if _, ok := provider.(ReadProvider); !ok {
		t.Fatal("readOnlyProvider does not satisfy pipeline.ReadProvider")
	}
}

func TestDryRunFailsOnProviderReadError(t *testing.T) {
	errProvider := errors.New("provider failed")
	provider := &failingProvider{err: errProvider}
	_, err := DryRun(context.Background(), Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{},
		Store:    &noopStore{},
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, Request{
		PRRef:           gitprovider.PRRef{Host: "github.com", Owner: "o", Repo: "r", Number: 1},
		PRURL:           "https://github.com/o/r/pull/1",
		ProfileName:     "home",
		PostingIdentity: gitprovider.Identity{Login: "bot"},
	})
	if !errors.Is(err, errProvider) {
		t.Fatalf("DryRun error = %v, want provider read error", err)
	}
}

type failingProvider struct {
	err error
}

func (p *failingProvider) GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error) {
	return gitprovider.PR{}, p.err
}

func (p *failingProvider) GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return gitprovider.UnifiedDiff{}, p.err
}

func (p *failingProvider) GetFileAtRef(context.Context, gitprovider.PRRef, string, string) ([]byte, error) {
	return nil, p.err
}

func (p *failingProvider) ListTreeAtRef(context.Context, gitprovider.PRRef, string, string) ([]gitprovider.TreeEntry, error) {
	return nil, p.err
}

func (p *failingProvider) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return nil, p.err
}

func (p *failingProvider) ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error) {
	return nil, p.err
}

func (p *failingProvider) ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return nil, p.err
}

func (p *failingProvider) Capabilities() gitprovider.ProviderCaps {
	return gitprovider.ProviderCaps{}
}

type noopStore struct{}

func (noopStore) ListRuns(context.Context) ([]ledger.Run, error) {
	return nil, nil
}

func (noopStore) ListRunsForHeadScope(context.Context, ledger.ListRunsForHeadScopeParams) ([]ledger.Run, error) {
	return nil, nil
}

func (noopStore) DeleteRun(context.Context, string) error {
	return nil
}

func (noopStore) AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error) {
	return ledger.Run{}, nil
}

func (noopStore) InsertSession(context.Context, ledger.Session) error {
	return nil
}

func (noopStore) GetSession(context.Context, string) (ledger.Session, error) {
	return ledger.Session{}, ledger.ErrNotFound
}

func (noopStore) InsertFinding(context.Context, ledger.Finding) error {
	return nil
}

func (noopStore) InsertPlannedAction(context.Context, ledger.PlannedAction) error {
	return nil
}

func (noopStore) InsertPlanningResult(context.Context, []ledger.Finding, []ledger.PlannedAction) error {
	return nil
}

func (noopStore) ListFindings(context.Context, string) ([]ledger.Finding, error) {
	return nil, nil
}

func (noopStore) ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error) {
	return nil, nil
}

func (noopStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	return nil
}
