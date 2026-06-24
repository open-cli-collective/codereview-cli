package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

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

	result, err := DryRun(ctx, Options{
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
	if len(requests) != 3 {
		t.Fatalf("requests len = %d, want selection/reviewer/rollup", len(requests))
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
	if len(storedFindings) != 1 || storedFindings[0].SessionRowID != "session-2" {
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
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "No dedicated repo review-guidance source is defined yet.")
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

func TestDryRunNormalizesReviewerFindingsFileAlias(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsFileAliasJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := DryRun(ctx, Options{
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
	currentBaseSHA := provider.pr.Base.SHA
	reviewBaseSHA := strings.Repeat("1", 40)
	reviewHeadSHA := strings.Repeat("2", 40)
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

	result, err := DryRun(ctx, Options{
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
	if result.CurrentBaseSHA != strings.Repeat("b", 40) || result.CurrentHeadSHA != strings.Repeat("a", 40) ||
		result.ReviewBaseSHA != reviewBaseSHA || result.ReviewHeadSHA != reviewHeadSHA {
		t.Fatalf("result SHAs = current %s/%s review %s/%s", result.CurrentBaseSHA, result.CurrentHeadSHA, result.ReviewBaseSHA, result.ReviewHeadSHA)
	}
	if !strings.Contains(result.Artifacts.Dir, reviewHeadSHA) || !strings.Contains(result.Artifacts.Dir, reviewBaseSHA) {
		t.Fatalf("artifact dir = %s, want pinned head/base SHAs", result.Artifacts.Dir)
	}
	if !containsFileCall(provider.fileCalls, fileKey{gitRef: reviewBaseSHA, path: "main.go"}) ||
		!containsFileCall(provider.fileCalls, fileKey{gitRef: reviewHeadSHA, path: "main.go"}) {
		t.Fatalf("file calls = %#v, want pinned base/head refs", provider.fileCalls)
	}
	requests := adapter.Requests()
	if len(requests) < 1 {
		t.Fatalf("adapter requests = %d, want selection request", len(requests))
	}
	selectionPrompt := requests[0].Prompt
	if !strings.Contains(selectionPrompt, reviewBaseSHA) || !strings.Contains(selectionPrompt, reviewHeadSHA) {
		t.Fatalf("selection prompt missing pinned review SHAs: %s", selectionPrompt)
	}
	if strings.Contains(selectionPrompt, currentBaseSHA) || strings.Contains(selectionPrompt, provider.pr.Head.SHA) {
		t.Fatalf("selection prompt contains current PR SHAs: %s", selectionPrompt)
	}
	if provider.threadCalls != 0 {
		t.Fatalf("thread calls = %d, want no live thread reads for pinned review", provider.threadCalls)
	}
	if provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("review/comment calls = %d/%d, want no live discussion reads for pinned review", provider.reviewCalls, provider.issueCommentCalls)
	}
	if !containsFileCall(provider.treeCalls, fileKey{gitRef: currentBaseSHA, path: ".codereview/agents"}) {
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

	_, err := DryRun(ctx, Options{
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

	if _, err := DryRun(ctx, Options{
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
	if !strings.Contains(selectionPrompt, `"task": "select reviewer agents and thread actions; return selection JSON only"`) {
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
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := SelectionOnly(ctx, Options{
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
	if len(adapter.Requests()) != 1 {
		t.Fatalf("adapter requests = %d, want selection only", len(adapter.Requests()))
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
	request := adapter.Requests()[0]
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
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")
	provider.pr.Body = "Selection prompt body should stay out of prompt payloads."
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:alpha", "main.go"), 10, 2))

	if _, err := SelectionOnly(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir())); err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want selection only", len(requests))
	}
	selectionPrompt := requests[0].Prompt
	var payload struct {
		Task                  string                     `json:"task"`
		Schema                string                     `json:"schema"`
		SelectionInstructions string                     `json:"selection_instructions"`
		OutputContract        map[string]any             `json:"output_contract"`
		Agents                []selectionAgentPrompt     `json:"agents"`
		ChangedFiles          []string                   `json:"changed_files"`
		Threads               []gitprovider.InlineThread `json:"threads"`
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
	if len(payload.Threads) != 1 || payload.Threads[0].ID != "thread-1" || payload.Threads[0].Path != "main.go" {
		t.Fatalf("threads = %#v, want thread-1 on main.go", payload.Threads)
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
	if strings.Contains(selectionPrompt, "Selection prompt body should stay out of prompt payloads.") {
		t.Fatalf("selection prompt leaked PR body: %s", selectionPrompt)
	}
}

func TestSelectionOnlyRejectsInvalidSelection(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("missing:agent", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("selection-session-retry", selectionJSON("missing:agent", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := SelectionOnly(ctx, Options{
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

	result, err := SelectionOnly(ctx, Options{
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

	_, err := SelectionOnly(ctx, Options{
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

	result, err := SelectionOnly(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

			result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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
	adapter := &reviewerIsolationAdapter{reviewerBarrier: newReviewerStartBarrier(3)}

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	_, err := Live(ctx, Options{
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
	result, err := Live(ctx, Options{
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

func TestLoadStructuredTaskRejectsValidatedOutputOutsideTaskDir(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	spec := llmTaskSpec{
		runID:            "run-output-path",
		taskID:           orchestratorSelectionStage,
		phase:            "selection",
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
		Adapter:           "fake-llm",
		Status:            llmTaskStatusSucceeded,
		SessionRowID:      "session-row",
		ProviderSessionID: "provider-session",
	}
	if err := writeLLMTaskSuccess(artifacts, &meta, []byte(`"ok"`)); err != nil {
		t.Fatalf("writeLLMTaskSuccess: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "validated-output.json")
	if err := os.WriteFile(outsidePath, []byte(`"outside"`), 0o600); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	meta.ValidatedOutputPath = outsidePath
	if err := writeLLMTaskMetadata(artifacts, meta); err != nil {
		t.Fatalf("writeLLMTaskMetadata: %v", err)
	}

	_, _, _, ok, err := loadStructuredTask[string](ctx, Options{Adapter: &llm.FakeAdapter{NameValue: "fake-llm"}}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if !ok {
		t.Fatal("loadStructuredTask ok = false, want metadata considered present")
	}
	if err == nil || !strings.Contains(err.Error(), "outside the task artifact directory") {
		t.Fatalf("loadStructuredTask error = %v, want outside task artifact directory", err)
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

func TestLoadStructuredTaskReportsCachedProgress(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	allocatePipelineRun(t, store, layout, "run-cached-progress", ledger.PostModeDryRun, fixedNow())
	spec := llmTaskSpec{
		runID:            "run-cached-progress",
		taskID:           orchestratorSelectionStage,
		phase:            "selection",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleOrchestrator,
		model:            "gpt-5.4-mini",
		effort:           "low",
		logPath:          filepath.Join(t.TempDir(), "selection.jsonl"),
		prompt:           "prompt",
	}
	session := ledger.Session{
		SessionRowID:      "session-row",
		RunID:             spec.runID,
		ProviderSessionID: "cached-session",
		Role:              ledger.SessionRoleOrchestrator,
		Adapter:           "fake-llm",
		Model:             spec.model,
		StartedAt:         fixedNow(),
	}
	if err := store.InsertSession(ctx, session); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	meta := llmTaskMetadata{
		SchemaVersion:       llmTaskSchemaVersion,
		TaskID:              spec.taskID,
		Phase:               spec.phase,
		InputFingerprint:    spec.inputFingerprint,
		Adapter:             "fake-llm",
		Status:              llmTaskStatusSucceeded,
		SessionRowID:        session.SessionRowID,
		ProviderSessionID:   session.ProviderSessionID,
		ValidatedOutputPath: "",
	}
	if err := writeLLMTaskSuccess(artifacts, &meta, []byte(`"cached"`)); err != nil {
		t.Fatalf("writeLLMTaskSuccess: %v", err)
	}
	progress := &fakeTaskProgress{}

	value, _, _, ok, err := loadStructuredTask[string](ctx, Options{
		Adapter:      &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:        store,
		TaskProgress: progress,
	}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err != nil {
		t.Fatalf("loadStructuredTask: %v", err)
	}
	if !ok || strings.TrimSpace(value) != `"cached"` {
		t.Fatalf("ok=%v value=%q, want cached task load", ok, value)
	}
	if len(progress.loads) != 1 {
		t.Fatalf("progress loads=%d, want 1", len(progress.loads))
	}
	if progress.loads[0].event.TaskID != orchestratorSelectionStage || progress.loads[0].event.Phase != "selection" || progress.loads[0].event.Source != "resume" {
		t.Fatalf("load event = %#v, want selection resume event", progress.loads[0].event)
	}
	if progress.loads[0].event.ResumeSessionID != "cached-session" || progress.loads[0].event.Model != spec.model || progress.loads[0].event.Effort != spec.effort {
		t.Fatalf("load event fields = %#v, want cached session/model/effort", progress.loads[0].event)
	}
	if !progress.loads[0].result.Cached || progress.loads[0].result.Status != string(llmTaskStatusSucceeded) || progress.loads[0].result.ProviderSessionID != "cached-session" {
		t.Fatalf("load = %#v, want cached succeeded selection task", progress.loads[0])
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

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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

	if _, err := DryRun(ctx, Options{
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

	if _, err := DryRun(ctx, Options{
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

	result, err := Live(ctx, Options{
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

			_, err := Live(ctx, Options{
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

	result, err := Live(ctx, Options{
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

	result, err := Live(ctx, Options{
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

			_, err := Live(ctx, Options{
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

	_, err := Live(ctx, Options{
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

	result, err := Live(ctx, Options{
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

	result, err := Live(ctx, Options{
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

	result, err := Live(ctx, Options{
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

	_, err = Live(ctx, Options{
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

	_, err = Live(ctx, Options{
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
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
	}}
	provider.caps.ThreadResolution = true
	req.NoResolveThreads = true
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [{
			"thread_id": "thread-1",
			"decision": "summarize_and_resolve",
			"summary": "Summary only",
			"safe_to_resolve_rationale": "safe"
		}],
		"reasoning": "thread cleanup"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("approve", nil), 1, 1))

	result, err := DryRun(ctx, Options{
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

	result, err := DryRun(ctx, Options{
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
			for _, forbidden := range []string{"Review alpha files.", "Review beta files.", `"prompt"`, `"owner"`, `"provenance"`, `"overridden"`} {
				if strings.Contains(request.Prompt, forbidden) {
					t.Fatalf("selection prompt leaked reviewer execution instructions %q: %s", forbidden, request.Prompt)
				}
			}
		}
		if strings.Contains(request.Prompt, `"schema": "findings"`) {
			reviewerPrompts++
			if !strings.Contains(request.Prompt, `"agent"`) || !strings.Contains(request.Prompt, `"files"`) {
				t.Fatalf("reviewer prompt missing agent/files context: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"file_path"`) ||
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

	result, err := DryRun(ctx, Options{
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

			_, err := DryRun(ctx, Options{
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

	_, err := DryRun(ctx, Options{
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
	contract := selectionOutputContract(nil, []FilePatch{{Path: "main.go"}}, nil, 3)
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
		gitprovider.PR{},
		agents.Catalog{Agents: []agents.Agent{{ID: "agent-1"}}},
		[]FilePatch{{Path: "main.go"}},
		nil,
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
	}, nil)
	if err != nil {
		t.Fatalf("buildRollupPrompt: %v", err)
	}

	var payload struct {
		Findings []rollupFindingPrompt `json:"findings"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("unmarshal rollup prompt: %v", err)
	}
	if len(payload.Findings) != 2 {
		t.Fatalf("rollup findings = %d, want 2", len(payload.Findings))
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
		{
			name:   "reviewer diff",
			budget: 5000,
			mutate: func(t *testing.T, provider *readOnlyProvider, _ *Request, _ *llm.FakeAdapter) {
				t.Helper()
				provider.diff.Raw = largeDiff("main.go", strings.Repeat("+var x = true\n", 400))
			},
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			},
			want:  "context budget exceeded for reviewer agent harness:reviewer",
			runID: "run-budget-reviewer",
		},
		{
			name:   "full content default model",
			budget: 10000,
			mutate: func(t *testing.T, provider *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgentFullContent(t, dir, "harness", "reviewer")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: "main.go"}] = []byte(strings.Repeat("base\n", 3000))
				provider.files[fileKey{gitRef: provider.pr.Head.SHA, path: "main.go"}] = []byte("package main\n")
			},
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			},
			want:  "context budget exceeded for full-content agent harness:reviewer file main.go model claude-sonnet-4-6",
			runID: "run-budget-full-content-default",
		},
		{
			name:   "full content override model",
			budget: 10000,
			mutate: func(t *testing.T, provider *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgentFullContent(t, dir, "harness", "reviewer")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				req.ReviewerModelOverride = "bench-model"
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: "main.go"}] = []byte(strings.Repeat("base\n", 3000))
				provider.files[fileKey{gitRef: provider.pr.Head.SHA, path: "main.go"}] = []byte("package main\n")
			},
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			},
			want:  "context budget exceeded for full-content agent harness:reviewer file main.go model bench-model",
			runID: "run-budget-full-content-override",
		},
		{
			name:   "rollup default model",
			budget: 5000,
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
				adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, strings.Repeat("body ", 1000)), 1, 1))
			},
			want:  "context budget exceeded for rollup model claude-sonnet-4-6",
			runID: "run-budget-rollup-default",
		},
		{
			name:   "rollup keeps default model under selection override",
			budget: 5000,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				req.SelectionModelOverride = "bench-model"
			},
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
				adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, strings.Repeat("body ", 1000)), 1, 1))
			},
			want:  "context budget exceeded for rollup model claude-sonnet-4-6",
			runID: "run-budget-rollup-override",
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
			_, err := DryRun(ctx, Options{
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
	case strings.Contains(req.Prompt, "harness:alpha"):
		return staticStream{sessionID: "alpha-session", output: findingsJSON("harness:alpha", "main.go", "major", 2, "Alpha finding")}, nil
	case strings.Contains(req.Prompt, "harness:beta"):
		return staticStream{sessionID: "beta-session", output: findingsJSON("harness:beta", "other.go", "major", 2, "Beta finding")}, nil
	case strings.Contains(req.Prompt, `"schema": "rollup"`):
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", findingIDsFromPrompt(req.Prompt))}, nil
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
	mu              sync.Mutex
	requests        []llm.Request
	betaAttempts    int
	betaProviderErr error
	reviewerBarrier *reviewerStartBarrier
}

func (a *reviewerIsolationAdapter) Name() string {
	return "reviewer-isolation"
}

func (a *reviewerIsolationAdapter) SupportsResume() bool {
	return false
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
	return nil, errors.New("resume unsupported")
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

func dryRunHarness(t *testing.T) (*readOnlyProvider, Request) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("a", 40)
	pr := gitprovider.PR{
		Ref:    ref,
		Title:  "CR-20 dry-run",
		Body:   "Default PR body.",
		URL:    prURL(ref),
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
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
	}
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "reviewer", "reviewer desc", "Review carefully.")
	trustCurrentTempFixtures(t)
	provider := &readOnlyProvider{
		pr:    pr,
		diff:  gitprovider.UnifiedDiff{Raw: smallDiff("main.go")},
		files: map[fileKey][]byte{},
		trees: map[fileKey][]gitprovider.TreeEntry{},
		caps:  gitprovider.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true},
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

func fixedNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func sequence(prefix string) func() string {
	var counter int
	return func() string {
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
		"schema_version": 1,
		"agent_id":       agentID,
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

func findingsFileAliasJSON(agentID, file, severity string, line int, body string) string {
	payload := map[string]any{
		"schema_version": 1,
		"agent_id":       agentID,
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
	var saw bool
	for _, file := range index.Files {
		if file.Path == wantPath {
			saw = true
		}
		if file.Path == "" || file.SHA256 == "" {
			t.Fatalf("index file = %#v, want non-empty path/hash", file)
		}
	}
	if !saw {
		t.Fatalf("dossier index files = %#v, want %q", index.Files, wantPath)
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
		"fingerprint",
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

func (noopStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	return nil
}
