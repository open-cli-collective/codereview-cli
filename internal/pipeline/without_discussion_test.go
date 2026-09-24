package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/dossier"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// Distinct text for each kind of existing discussion, so any leak into a
// prompt or dossier file is caught by a plain substring search.
const (
	discussionThreadMarker   = "DISCUSSION-THREAD-MARKER"
	discussionCommentMarker  = "DISCUSSION-ISSUE-COMMENT-MARKER"
	discussionReviewMarker   = "DISCUSSION-PRIOR-REVIEW-MARKER"
	discussionCRThreadMarker = "Original finding."
	priorOrchestratorSession = "prior-orchestrator-session"
	priorReviewerSession     = "prior-reviewer-session"
)

var discussionMarkers = []string{discussionThreadMarker, discussionCommentMarker, discussionReviewMarker}

func addExistingDiscussion(provider *readOnlyProvider, bot gitprovider.Identity) {
	human := gitprovider.Identity{Login: "maintainer", ID: "maintainer-id"}
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-human",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
		Comments: []gitprovider.ThreadComment{{
			ID:     "thread-human-1",
			Body:   discussionThreadMarker + " this looks wrong",
			Author: human,
		}},
	}}
	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-1",
		Body:   discussionCommentMarker + " please also handle the empty case",
		Author: human,
	}}
	provider.reviews = []gitprovider.Review{{
		ID:     "review-1",
		Body:   discussionReviewMarker + " earlier cr review summary",
		Author: bot,
		Event:  review.ReviewEventComment,
	}}
}

// storePriorSessions stores the PR's default orchestrator session and reviewer
// cohort as an earlier live review would have left them.
func storePriorSessions(t *testing.T, ctx context.Context, store *ledger.Store, provider *readOnlyProvider, req Request) (string, ledger.ReviewerCohortScope) {
	t.Helper()
	// The earlier live review that left these sessions behind.
	allocateLiveRun(t, store, provider, req, "run-prior-live")
	name, err := defaultSessionName(req)
	if err != nil {
		t.Fatalf("defaultSessionName: %v", err)
	}
	stored := namedSessionForRequest(req, priorOrchestratorSession)
	stored.Name = name
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	scope := ledger.ReviewerCohortScope{PRKey: prKey, Profile: req.ProfileName, PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity)}
	if err := store.ReplaceReviewerCohort(ctx, ledger.ReviewerCohort{
		Scope: scope, Adapter: "fake-llm", CreatedAt: fixedNow().Add(-time.Hour), UpdatedAt: fixedNow().Add(-time.Hour),
		Members: []ledger.ReviewerCohortMember{{
			AgentID: "harness:reviewer", AssignmentMode: ledger.ReviewerAssignmentScoped, Files: []string{"main.go"},
			Model: "claude-sonnet-5", Effort: "medium", ProviderSessionID: priorReviewerSession,
		}},
	}); err != nil {
		t.Fatalf("ReplaceReviewerCohort: %v", err)
	}
	return name, scope
}

func pinnedDiscussionHarness(t *testing.T) (*readOnlyProvider, Request) {
	t.Helper()
	provider, req := dryRunHarness(t)
	fixture, reviewBaseSHA, reviewHeadSHA := newPinnedReviewFixtureForRef(t, req.PRRef)
	provider.pr = fixture.pr
	provider.pr.Title = "Replay title"
	provider.pr.Body = "Replay description stays visible."
	addRepoAgentFixture(provider)
	provider.fixtureRepoDir = fixture.repoDir
	provider.diffBetween = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	req.ReviewBaseSHA = reviewBaseSHA
	req.ReviewHeadSHA = reviewHeadSHA
	addExistingDiscussion(provider, req.PostingIdentity)
	provider.threads = append(provider.threads, markedReviewThread(t, "thread-cr", "main.go", 2, req.PostingIdentity, gitprovider.Identity{Login: "maintainer", ID: "maintainer-id"}))
	return provider, req
}

func withoutDiscussionOptions(t *testing.T, provider *readOnlyProvider, adapter llm.Adapter, store *ledger.Store, runID string) Options {
	t.Helper()
	return Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return runID },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}
}

// allPrompts returns every prompt the adapter received, started or resumed.
func allPrompts(adapter *llm.FakeAdapter) []string {
	var prompts []string
	for _, request := range adapter.Requests() {
		prompts = append(prompts, request.Prompt)
	}
	for _, resume := range adapter.Resumes() {
		prompts = append(prompts, resume.Request.Prompt)
	}
	return prompts
}

// dossierText concatenates every file the run wrote under its dossier.
func dossierText(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- test reads its own temp artifacts.
		if err != nil {
			return err
		}
		out.WriteString(path)
		out.WriteString("\n")
		out.Write(data)
		out.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk dossier: %v", err)
	}
	return out.String()
}

func TestDryRunWithoutDiscussionReplaysAsFirstPass(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := pinnedDiscussionHarness(t)
	req.WithoutDiscussion = true
	sessionName, cohortScope := storePriorSessions(t, ctx, store, provider, req)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-replay", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-replay", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-replay", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, withoutDiscussionOptions(t, provider, adapter, store, "run-replay"), req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if provider.threadCalls != 0 || provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("discussion reads = threads %d reviews %d comments %d, want none", provider.threadCalls, provider.reviewCalls, provider.issueCommentCalls)
	}
	if resumes := adapter.Resumes(); len(resumes) != 0 {
		t.Fatalf("resumes = %#v, want no prior orchestrator or reviewer session resumed", resumes)
	}
	prompts := allPrompts(adapter)
	if len(prompts) != 3 {
		t.Fatalf("prompts = %d, want selection, reviewer, rollup", len(prompts))
	}
	for i, prompt := range prompts {
		for _, leaked := range append(discussionMarkers, discussionCRThreadMarker, "discussion_outcomes") {
			if strings.Contains(prompt, leaked) {
				t.Fatalf("prompt %d contains discussion %q:\n%s", i, leaked, prompt)
			}
		}
	}
	if !strings.Contains(prompts[0], "Replay title") || !strings.Contains(prompts[0], "Replay description stays visible.") {
		t.Fatalf("selection prompt lost PR title/description:\n%s", prompts[0])
	}
	dossierFiles := dossierText(t, result.Artifacts.DossierDir)
	for _, leaked := range append(discussionMarkers, discussionCRThreadMarker) {
		if strings.Contains(dossierFiles, leaked) {
			t.Fatalf("dossier contains discussion %q:\n%s", leaked, dossierFiles)
		}
	}
	summary, err := dossier.ReadDiscussionSummary(result.Artifacts)
	if err != nil {
		t.Fatalf("ReadDiscussionSummary: %v", err)
	}
	if !summary.WithoutDiscussion || len(summary.TopLevelComments) != 0 || len(summary.InlineThreads) != 0 {
		t.Fatalf("discussion summary = %#v, want empty and marked without discussion", summary)
	}

	marker, err := runartifact.ReadMarker(result.Artifacts.Dir, runartifact.KindReview)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if !marker.WithoutDiscussion || !result.WithoutDiscussion {
		t.Fatalf("marker/result without discussion = %v/%v, want recorded", marker.WithoutDiscussion, result.WithoutDiscussion)
	}
	if result.NamedSessionCandidate != nil {
		t.Fatalf("named session candidate = %#v, want none", result.NamedSessionCandidate)
	}
	storedSession, err := store.GetNamedSession(ctx, sessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if storedSession.ProviderSessionID != priorOrchestratorSession {
		t.Fatalf("orchestrator session = %q, want untouched %q", storedSession.ProviderSessionID, priorOrchestratorSession)
	}
	cohort, err := store.GetReviewerCohort(ctx, cohortScope)
	if err != nil {
		t.Fatalf("GetReviewerCohort: %v", err)
	}
	if len(cohort.Members) != 1 || cohort.Members[0].ProviderSessionID != priorReviewerSession {
		t.Fatalf("reviewer cohort = %#v, want untouched %q", cohort, priorReviewerSession)
	}
}

// Without the flag, a pinned replay still resumes the PR's prior sessions,
// which is the leak the flag closes.
func TestDryRunPinnedWithDiscussionStillResumesPriorSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := pinnedDiscussionHarness(t)
	storePriorSessions(t, ctx, store, provider, req)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("reviewer-resumed", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-resumed", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, withoutDiscussionOptions(t, provider, adapter, store, "run-pinned"), req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	resumed := map[string]bool{}
	for _, resume := range adapter.Resumes() {
		resumed[resume.SessionID] = true
	}
	if !resumed[priorReviewerSession] || !resumed[priorOrchestratorSession] {
		t.Fatalf("resumes = %#v, want prior reviewer and orchestrator sessions", adapter.Resumes())
	}
	if result.WithoutDiscussion {
		t.Fatal("result.WithoutDiscussion = true, want false")
	}
	marker, err := runartifact.ReadMarker(result.Artifacts.Dir, runartifact.KindReview)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if marker.WithoutDiscussion {
		t.Fatal("marker records without_discussion for a run with discussion")
	}
}

// Without the flag and without pinning, the same discussion fixture reaches
// the dossier and the selection prompt, so the assertions above would see a leak.
func TestDryRunWithDiscussionFeedsExistingDiscussion(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	addExistingDiscussion(provider, req.PostingIdentity)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{discussionCommentMarker}, nil), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, withoutDiscussionOptions(t, provider, adapter, store, "run-with-discussion"), req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if provider.threadCalls == 0 || provider.reviewCalls == 0 || provider.issueCommentCalls == 0 {
		t.Fatalf("discussion reads = threads %d reviews %d comments %d, want all read", provider.threadCalls, provider.reviewCalls, provider.issueCommentCalls)
	}
	dossierFiles := dossierText(t, result.Artifacts.DossierDir)
	for _, want := range discussionMarkers {
		if !strings.Contains(dossierFiles, want) {
			t.Fatalf("dossier missing discussion %q", want)
		}
	}
	prompts := allPrompts(adapter)
	selectionPrompt := prompts[1]
	if !strings.Contains(selectionPrompt, discussionThreadMarker) || !strings.Contains(selectionPrompt, discussionCommentMarker) {
		t.Fatalf("selection prompt missing existing discussion:\n%s", selectionPrompt)
	}
}

// An incomplete run made with discussion is never resumed by a replay without
// it, because its tasks and sessions may have seen that discussion.
func TestDryRunWithoutDiscussionDoesNotResumeRunWithDiscussion(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := pinnedDiscussionHarness(t)
	opts := withoutDiscussionOptions(t, provider, &llm.FakeAdapter{NameValue: "fake-llm"}, store, "run-fresh-replay")
	provider.diffBetween = gitprovider.UnifiedDiff{}
	withDiscussion := allocateDryRunForSHAs(t, store, opts.Layout, req, "run-with-discussion", req.ReviewHeadSHA, req.ReviewBaseSHA, fixedNow().Add(-time.Minute))
	req.WithoutDiscussion = true

	result, err := dryRunForTest(ctx, opts, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID == withDiscussion.RunID || result.Run.RunID != "run-fresh-replay" {
		t.Fatalf("run = %q, want a fresh run instead of resuming %q", result.Run.RunID, withDiscussion.RunID)
	}
	marker, err := runartifact.ReadMarker(result.Artifacts.Dir, runartifact.KindReview)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if !marker.WithoutDiscussion {
		t.Fatal("fresh replay marker does not record without_discussion")
	}
}

func TestPipelineRejectsWithoutDiscussionOutsidePinnedDryRun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.WithoutDiscussion = true
	opts := withoutDiscussionOptions(t, provider, &llm.FakeAdapter{NameValue: "fake-llm"}, store, "run-unpinned")
	if _, err := dryRunForTest(ctx, opts, req); err == nil || !strings.Contains(err.Error(), "requires pinned review base and head SHAs") {
		t.Fatalf("DryRun error = %v, want pinned SHA requirement", err)
	}
	run := allocateLiveRun(t, store, provider, Request{PRRef: req.PRRef, PRURL: req.PRURL, ProfileName: req.ProfileName, Profile: req.Profile, PostingIdentity: req.PostingIdentity}, "run-live")
	if _, err := liveForTest(ctx, opts, req, run); err == nil || !strings.Contains(err.Error(), "requires dry-run review") {
		t.Fatalf("Live error = %v, want dry-run requirement", err)
	}
	if provider.threadCalls != 0 || provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("discussion reads = %d/%d/%d, want none before rejection", provider.threadCalls, provider.reviewCalls, provider.issueCommentCalls)
	}
}
