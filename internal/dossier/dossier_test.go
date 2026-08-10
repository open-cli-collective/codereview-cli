package dossier

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

const llmTaskStatusSucceeded = llmlifecycle.StatusSucceeded

type dossierInputs = Inputs
type dossierPreparationRequest = PreparationRequest
type dossierDiscussionSummaryArtifact = DiscussionSummary

type ContextBudget struct {
	MaxPromptBytes int
}

type Options struct {
	Adapter         llm.Adapter
	Store           llmlifecycle.Store
	TaskProgress    llmlifecycle.Progress
	Now             func() time.Time
	NewSessionRowID func() string
	Budget          ContextBudget
}

var (
	ArtifactPathsFromDir     = runartifact.FromDir
	writeRawDossierArtifacts = WriteRaw
)

func prepareDossierArtifacts(ctx context.Context, opts Options, req PreparationRequest) error {
	return Prepare(ctx, Env{
		Adapter:         opts.Adapter,
		Store:           opts.Store,
		TaskProgress:    opts.TaskProgress,
		Now:             opts.Now,
		NewSessionRowID: opts.NewSessionRowID,
		CheckPromptBudget: func(model, prompt string) error {
			limit := opts.Budget.MaxPromptBytes
			if limit == 0 {
				limit = 512 * 1024
			}
			if limit < 0 || len(prompt) <= limit {
				return nil
			}
			return fmt.Errorf("pipeline: context budget exceeded for dossier-summary model %s: %d bytes > %d", model, len(prompt), limit)
		},
	}, req)
}

func lifecyclePaths(paths runartifact.Paths) llmlifecycle.Paths {
	return llmlifecycle.Paths{LLMTasksDir: paths.LLMTasksDir}
}

type testProvider struct {
	pr   gitprovider.PR
	diff gitprovider.UnifiedDiff
}

type testRequest struct {
	Profile         config.Profile
	PostingIdentity gitprovider.Identity
}

func dryRunHarness(t *testing.T) (*testProvider, testRequest) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	pr := gitprovider.PR{
		Ref:    ref,
		Title:  "CR-20 dry-run",
		Body:   "Default PR body.",
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/29",
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Base:   gitprovider.PRBranchRef{Ref: "refs/heads/main", SHA: strings.Repeat("b", 40)},
		Head:   gitprovider.PRBranchRef{Ref: "refs/heads/feature", SHA: strings.Repeat("a", 40)},
	}
	return &testProvider{pr: pr, diff: gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}}, testRequest{
		Profile: config.Profile{LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		}},
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
	}
}

func parseDiffPatchesForTest(t *testing.T, raw string) []ChangedFile {
	t.Helper()
	return []ChangedFile{{OldPath: "main.go", Path: "main.go", Patch: raw, HunkCount: 1}}
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
		ArtifactPath:    filepath.Join(layout.DataRoot, "runs", runID),
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

type fakeTaskProgress struct {
	mu     sync.Mutex
	starts []llmlifecycle.ProgressEvent
	loads  []fakeTaskProgressLoad
}

type fakeTaskProgressSpan struct {
	parent *fakeTaskProgress
}

type fakeTaskProgressLoad struct {
	event  llmlifecycle.ProgressEvent
	result llmlifecycle.ProgressResult
}

func (f *fakeTaskProgress) StartLLMTask(event llmlifecycle.ProgressEvent) llmlifecycle.ProgressSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, event)
	return fakeTaskProgressSpan{parent: f}
}

func (f *fakeTaskProgress) LoadLLMTask(event llmlifecycle.ProgressEvent, result llmlifecycle.ProgressResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, fakeTaskProgressLoad{event: event, result: result})
}

func (s fakeTaskProgressSpan) End(error, llmlifecycle.ProgressResult) {}

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
		topLevelPayload = append(topLevelPayload, map[string]any{"kind": "issue_comment", "author": "reviewer", "summary": summary})
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
			"path": thread.path, "side": side, "line": thread.line, "anchor_kind": anchorKind,
			"resolved": thread.resolved, "status": thread.status, "summary": thread.summary,
		})
	}
	data, err := json.Marshal(map[string]any{
		"schema_version": dossierSummarySchemaVersion, "top_level_comments": topLevelPayload, "inline_threads": threadPayload,
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func intPtr(value int) *int { return &value }

func crSettledReviewThread(t *testing.T, id, path string, line int, bot, human gitprovider.Identity, summary string) gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{
		RunID: "old-run", ActionID: "old-action", Kind: marker.ActionKindInlineComment,
		SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	summaryMarker, err := marker.RenderThreadSummary(marker.ThreadSummaryMarker{RunID: "response-run", ActionID: "summary-" + id})
	if err != nil {
		t.Fatalf("RenderThreadSummary: %v", err)
	}
	threadID := gitprovider.ThreadID(id)
	created := fixedNow()
	comment := func(commentID, body string, author gitprovider.Identity, at time.Time) gitprovider.ThreadComment {
		return gitprovider.ThreadComment{
			ID: gitprovider.CommentID(commentID), ThreadID: threadID, Body: body, Author: author,
			CommitSHA: strings.Repeat("a", 40), Path: path, Side: review.DiffSideRight, Line: line,
			SubjectType: review.AnchorKindLine, CreatedAt: at, UpdatedAt: at,
		}
	}
	return gitprovider.InlineThread{
		ID: threadID, Path: path, Side: review.DiffSideRight, Line: line,
		SubjectType: review.AnchorKindLine, CommitSHA: strings.Repeat("a", 40),
		Comments: []gitprovider.ThreadComment{
			comment(id+"-cr", action+"\nOriginal finding.", bot, created),
			comment(id+"-human", "Human reply", human, created.Add(time.Minute)),
			comment(id+"-summary", summaryMarker+"\n\n"+summary, bot, created.Add(2*time.Minute)),
		},
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifacts under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("file %s = %q, want substring %q", path, data, want)
	}
}

func assertFileOmits(t *testing.T, path, unwanted string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifacts under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if strings.Contains(string(data), unwanted) {
		t.Fatalf("file %s = %q, want to omit %q", path, data, unwanted)
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
	for _, text := range []string{"CI status is red", "Build failed in CI", "The pull request was approved by alice", "run_id=1234"} {
		_, err := decodeDossierDiscussionSummary([]byte(fmt.Sprintf(`{
			"schema_version": 1,
			"top_level_comments": [{"summary": %q}]
		}`, text)), promptData)
		if err == nil || !strings.Contains(err.Error(), "excluded reviewer-facing process state") {
			t.Fatalf("decodeDossierDiscussionSummary(%q) error = %v, want excluded process state", text, err)
		}
	}
}

func TestDossierDiscussionSummaryProcessVocabularyMatrix(t *testing.T) {
	accepted := []string{
		"Existing matching approval records remain retrievable after a run reaches a terminal state.",
		"The approval request was approved by an administrator.",
		"Draft invoices are stored in the ledger.",
		"The session_id and run_id columns are indexed.",
		"Cache hits update retry state transitions.",
	}
	rejected := []string{
		"The PR is approved.",
		"The PR is approved by alice.",
		"The PR was approved by alice.",
		"The pull request is approved by alice.",
		"The pull request was approved by alice.",
		"The pull request has been approved.",
		"The pull request is a draft.",
		"PR mergeability status is clean.",
		"The PR is mergeable.",
		"The PR was mergeable.",
		"The pull request is mergeable.",
		"The pull request was mergeable.",
		"Requested reviewers are listed.",
		"Requested review is pending.",
		"CI is failing.",
		"CI was failing.",
		"CI has been failing.",
		"Checks are failing.",
		"Checks were failing.",
		"Checks have been failing.",
		"Build is failing.",
		"Build was failing.",
		"Build has been failing.",
		"Builds are failing.",
		"Builds were failing.",
		"Builds have been failing.",
		"CI failed.",
		"CI has failed.",
		"CI had failed.",
		"Build has failed.",
		"Checks have failed.",
		"Builds had failed.",
		"CI status is red.",
		"Build failed in CI.",
		"Checks failed in CI.",
		"session_id=019fe123.",
		"The session ID is 019fe123.",
		"The run_id was abc.",
		"retry_state: exhausted.",
		"cache_state=hit.",
	}
	for i, text := range accepted {
		t.Run(fmt.Sprintf("accept_%d", i), func(t *testing.T) {
			promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{
				TopLevelComments: []dossierTopLevelCommentArtifact{{Body: text}},
			})
			if err != nil {
				t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
			}
			if len(promptData.Input.TopLevelComments) != 1 {
				t.Fatalf("prompt comments = %#v, want accepted vocabulary", promptData.Input.TopLevelComments)
			}
			_, err = decodeDossierDiscussionSummary([]byte(fmt.Sprintf(`{
				"schema_version": 1,
				"top_level_comments": [{"summary": %q}]
			}`, text)), promptData)
			if err != nil {
				t.Fatalf("decodeDossierDiscussionSummary(%q): %v, want accepted vocabulary", text, err)
			}
		})
	}
	for i, text := range rejected {
		t.Run(fmt.Sprintf("reject_%d", i), func(t *testing.T) {
			promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{
				TopLevelComments: []dossierTopLevelCommentArtifact{{Body: text}},
			})
			if err != nil {
				t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
			}
			if len(promptData.Input.TopLevelComments) != 0 {
				t.Fatalf("prompt comments = %#v, want process state omitted", promptData.Input.TopLevelComments)
			}
			_, err = decodeDossierDiscussionSummary([]byte(fmt.Sprintf(`{
				"schema_version": 1,
				"top_level_comments": [{"summary": %q}]
			}`, text)), promptData)
			if err == nil || !strings.Contains(err.Error(), "excluded reviewer-facing process state") {
				t.Fatalf("decodeDossierDiscussionSummary(%q) error = %v, want excluded process state", text, err)
			}
		})
	}
}

func TestDossierDiscussionSummaryFiltersProcessStateFromInlineContent(t *testing.T) {
	promptData, err := dossierDiscussionPromptInputFromDiscussion(dossierDiscussionArtifact{
		InlineThreads: []dossierInlineThreadArtifact{
			{
				ID: "inline-comment", Path: "main.go", Side: "RIGHT", Line: 2, AnchorKind: "line",
				Comments: []dossierThreadCommentArtifact{{Body: "Checks are failing."}},
			},
			{
				ID: "cached-summary", Path: "other.go", Side: "RIGHT", Line: 4, AnchorKind: "line",
				CachedSummary: &dossierCachedThreadSummaryArtifact{
					ThreadID: "cached-summary", Body: "The pull request has been approved.",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dossierDiscussionPromptInputFromDiscussion: %v", err)
	}
	if len(promptData.CachedInlineSummaries) != 0 {
		t.Fatalf("cached inline summaries = %#v, want process state excluded", promptData.CachedInlineSummaries)
	}
	if len(promptData.Input.InlineThreads) != 2 {
		t.Fatalf("inline threads = %#v, want both threads without excluded text", promptData.Input.InlineThreads)
	}
	for _, thread := range promptData.Input.InlineThreads {
		if len(thread.Comments) != 0 {
			t.Fatalf("inline thread %q comments = %#v, want process state excluded", thread.ThreadID, thread.Comments)
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
