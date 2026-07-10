package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

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
	meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(artifacts), dossierSummaryTaskID)
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
	selectionMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(ArtifactPathsFromDir(result.Run.ArtifactPath)), orchestratorSelectionStage)
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
