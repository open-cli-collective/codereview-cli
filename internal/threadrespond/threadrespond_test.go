package threadrespond

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadanalysis"
)

func TestRunDryRunFiltersAndPlansThreadResponses(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-clarify", threadanalysis.DecisionClarify, "Please clarify the intended behavior.", "", false))
	fixture.adapter.Queue(threadAnalysisResult("thread-summarize", threadanalysis.DecisionSummarize, "", "Human confirmed this is fixed.", true))
	fixture.provider.SetInlineThreads(fixture.ref, []gitprovider.InlineThread{
		markedThread(t, "thread-clarify", "main.go", 10, false, fixture.bot, fixture.human),
		markedThread(t, "thread-summarize", "main.go", 20, false, fixture.bot, fixture.human),
		markedThread(t, "thread-resolved", "main.go", 30, true, fixture.bot, fixture.human),
		humanOnlyThread("thread-human", "main.go", 40, fixture.human),
	})

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}
	if len(result.EligibleThreads) != 2 {
		t.Fatalf("eligible threads = %d, want 2", len(result.EligibleThreads))
	}
	if len(fixture.adapter.Requests()) != 2 {
		t.Fatalf("adapter requests = %d, want 2", len(fixture.adapter.Requests()))
	}
	if len(result.PlannedActions) != 3 {
		t.Fatalf("planned actions = %d, want reply, summary reply, resolve", len(result.PlannedActions))
	}
	for _, action := range result.PlannedActions {
		if action.Status != ledger.PlannedActionPlannedOnly {
			t.Fatalf("action %s status = %s, want planned_only", action.ActionID, action.Status)
		}
	}
	replyPayload := decodePayload[outbox.ThreadReplyPayload](t, result.PlannedActions[0])
	if replyPayload.Summary || replyPayload.Body != "Please clarify the intended behavior." {
		t.Fatalf("reply payload = %#v, want normal clarify reply", replyPayload)
	}
	summaryPayload := decodePayload[outbox.ThreadReplyPayload](t, result.PlannedActions[1])
	if !summaryPayload.Summary || summaryPayload.Body != "Human confirmed this is fixed." {
		t.Fatalf("summary payload = %#v, want summary reply", summaryPayload)
	}
	if got := fixture.provider.RecordedThreadReplies(fixture.ref); len(got) != 0 {
		t.Fatalf("thread replies = %#v, want no provider writes in dry-run", got)
	}
	stored, err := fixture.store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("stored outcome = %v, want dry_run", stored.Outcome)
	}
}

func TestRunLivePostsThroughOutbox(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	fixture.provider.SetInlineThreads(fixture.ref, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run live: %v", err)
	}
	if result.Outbox.Outcome != ledger.OutcomeComment || result.Outbox.Posted != 2 {
		t.Fatalf("outbox = %#v, want comment with reply+resolve posted", result.Outbox)
	}
	replies := fixture.provider.RecordedThreadReplies(fixture.ref)
	if len(replies) != 1 || replies[0].ThreadID != "thread-1" {
		t.Fatalf("thread replies = %#v, want thread-1", replies)
	}
	if summaries := marker.FindThreadSummaries(replies[0].Body); len(summaries) != 1 {
		t.Fatalf("reply body summaries = %#v, want one summary marker", summaries)
	}
	resolved := fixture.provider.RecordedResolvedThreads(fixture.ref)
	if !reflect.DeepEqual(resolved, []gitprovider.ThreadID{"thread-1"}) {
		t.Fatalf("resolved threads = %#v, want thread-1", resolved)
	}
	stored, err := fixture.store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeComment {
		t.Fatalf("stored outcome = %v, want comment", stored.Outcome)
	}
}

func TestRunNoMatchingThreadsCompletesWithoutLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.provider.SetInlineThreads(fixture.ref, []gitprovider.InlineThread{
		humanOnlyThread("thread-human", "main.go", 10, fixture.human),
	})

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run no-op: %v", err)
	}
	if len(fixture.adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want 0", len(fixture.adapter.Requests()))
	}
	if result.Outbox.Outcome != ledger.OutcomeNothingToReview {
		t.Fatalf("outbox outcome = %s, want nothing_to_review", result.Outbox.Outcome)
	}
	stored, err := fixture.store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeNothingToReview {
		t.Fatalf("stored outcome = %v, want nothing_to_review", stored.Outcome)
	}
}

func TestRetryPostsExistingActionsWithoutLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "retry-run", ledger.PostModeLive)
	action := responsePlannedAction(t, run.RunID, "reply-1", ledger.PlannedActionThreadReply, "thread-1", outbox.ThreadReplyPayload{Body: "Retry this"})
	action.Status = ledger.PlannedActionFailedTerminal
	errMessage := "validation failed"
	action.Error = &errMessage
	insertAction(t, fixture.store, action)

	opts := fixture.options()
	opts.Adapter = nil
	result, err := Run(ctx, opts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		RetryRunID:      run.RunID,
	})
	if err != nil {
		t.Fatalf("Run retry: %v", err)
	}
	if len(fixture.adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want retry to skip LLM", len(fixture.adapter.Requests()))
	}
	if result.Outbox.Posted != 1 {
		t.Fatalf("outbox = %#v, want one posted retry action", result.Outbox)
	}
	replies := fixture.provider.RecordedThreadReplies(fixture.ref)
	if len(replies) != 1 || replies[0].ThreadID != "thread-1" {
		t.Fatalf("thread replies = %#v, want retry post", replies)
	}
	storedActions, err := fixture.store.ListPlannedActions(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if storedActions[0].Status != ledger.PlannedActionPosted || storedActions[0].Error != nil {
		t.Fatalf("stored action = %#v, want posted with cleared error", storedActions[0])
	}
}

func TestRetryRejectsPendingNonResponseActions(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "retry-run", ledger.PostModeLive)
	insertAction(t, fixture.store, ledger.PlannedAction{
		ActionID:    "inline-1",
		RunID:       run.RunID,
		Kind:        ledger.PlannedActionInlineComment,
		PlannedAt:   fixedNow(),
		PayloadJSON: `{"body":"inline","path":"main.go","side":"RIGHT","line":1,"subject_type":"line"}`,
		Status:      ledger.PlannedActionPending,
		Required:    false,
	})

	_, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		RetryRunID:      run.RunID,
	})
	if err == nil || !strings.Contains(err.Error(), "non-response action") {
		t.Fatalf("Run retry mixed action error = %v, want non-response action", err)
	}
	if len(fixture.provider.RecordedInlineComments(fixture.ref)) != 0 {
		t.Fatalf("retry mixed action posted inline comments")
	}
}

func TestRetryRejectsProfileMismatch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "retry-run", ledger.PostModeLive)
	insertAction(t, fixture.store, responsePlannedAction(t, run.RunID, "reply-1", ledger.PlannedActionThreadReply, "thread-1", outbox.ThreadReplyPayload{Body: "Retry this"}))

	_, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "other-profile",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		RetryRunID:      run.RunID,
	})
	if err == nil || !strings.Contains(err.Error(), "profile mismatch") {
		t.Fatalf("Run retry mismatch error = %v, want profile mismatch", err)
	}
	if len(fixture.provider.RecordedThreadReplies(fixture.ref)) != 0 {
		t.Fatalf("retry mismatch posted replies")
	}
}

func TestRunFailedAnalysisCompletesRunFailed(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.provider.SetInlineThreads(fixture.ref, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err == nil {
		t.Fatal("Run analysis failure error = nil, want fake adapter error")
	}
	stored, getErr := fixture.store.GetRun(ctx, result.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun: %v", getErr)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("stored outcome = %v, want failed", stored.Outcome)
	}
}

func TestRunLiveLocksBeforeReadingThreads(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	events := &eventLog{}
	fixture.provider = &recordingProvider{Fake: fixture.provider.Fake, events: events}
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionReplyOnly, "Reply", "", false))
	fixture.provider.SetInlineThreads(fixture.ref, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	opts := fixture.options()
	opts.Acquire = func(string) (Lock, error) {
		events.add("lock")
		return noopLock{}, nil
	}

	_, err := Run(ctx, opts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run live: %v", err)
	}
	if got := events.all(); !orderedBefore(got, "lock", "list_threads") {
		t.Fatalf("events = %#v, want lock before list_threads", got)
	}
}

type fixture struct {
	store    *ledger.Store
	provider *recordingProvider
	adapter  *llm.FakeAdapter
	ref      gitprovider.PRRef
	pr       gitprovider.PR
	bot      gitprovider.Identity
	human    gitprovider.Identity
	layout   statepaths.Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	store, err := ledger.Open(context.Background(), layout.LedgerDB())
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	pr := gitprovider.PR{
		Ref:    ref,
		Title:  "Test PR",
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/29",
		State:  gitprovider.PRStateOpen,
		Author: gitprovider.Identity{Login: "author", ID: "author-id"},
		Head:   gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "feature", Ref: "refs/heads/feature", SHA: testHeadSHA},
		Base:   gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, Name: "main", Ref: "refs/heads/main", SHA: testBaseSHA},
	}
	fake := &gitprovider.Fake{}
	fake.SetCapabilities(gitprovider.ProviderCaps{ThreadResolution: true})
	if err := fake.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	return &fixture{
		store:    store,
		provider: &recordingProvider{Fake: fake, events: &eventLog{}},
		adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		ref:      ref,
		pr:       pr,
		bot:      gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		human:    gitprovider.Identity{Login: "human", ID: "human-id"},
		layout:   layout,
	}
}

func (f *fixture) options() Options {
	return Options{
		Store:       f.store,
		Provider:    f.provider,
		Adapter:     f.adapter,
		Limiter:     noopLimiter{},
		Layout:      f.layout,
		Acquire:     func(string) (Lock, error) { return noopLock{}, nil },
		Now:         fixedNow,
		NewRunID:    sequence("run"),
		NewActionID: actionSequence(),
		NewStepID:   sequence("step"),
	}
}

func (f *fixture) allocateRun(t *testing.T, runID string, mode ledger.PostMode) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(f.ref.Host, f.ref.Owner, f.ref.Repo, f.ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := f.store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           f.pr.URL,
		RunID:           runID,
		SHA:             testHeadSHA,
		BaseSHA:         testBaseSHA,
		Profile:         "default",
		PostingIdentity: "review-bot",
		PostMode:        mode,
		StartedAt:       fixedNow(),
		ArtifactPath:    f.layout.DataRoot + "/runs/retry-run",
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

type recordingProvider struct {
	*gitprovider.Fake
	events *eventLog
}

func (p *recordingProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	p.events.add("get_pr")
	return p.Fake.GetPR(ctx, ref)
}

func (p *recordingProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	p.events.add("list_threads")
	return p.Fake.ListInlineThreads(ctx, ref)
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

type noopLock struct{}

func (noopLock) Release() error { return nil }

func testProfile() config.Profile {
	return config.Profile{
		LLM: config.LLMConfig{
			Provider: config.LLMProviderOpenAI,
			Adapter:  config.LLMAdapterOpenAIAPI,
		},
	}
}

func markedThread(t *testing.T, id, path string, line int, resolved bool, bot, human gitprovider.Identity) gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "old-run",
		ActionID: "old-action",
		Kind:     marker.ActionKindInlineComment,
		SHA:      testHeadSHA,
		BaseSHA:  testBaseSHA,
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	threadID := gitprovider.ThreadID(id)
	created := fixedNow()
	return gitprovider.InlineThread{
		ID:          threadID,
		Resolved:    resolved,
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   testHeadSHA,
		Comments: []gitprovider.ThreadComment{
			{
				ID:          gitprovider.CommentID(id + "-cr"),
				ThreadID:    threadID,
				Body:        action + "\nOriginal finding.",
				Author:      bot,
				CommitSHA:   testHeadSHA,
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
				CommitSHA:   testHeadSHA,
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

func humanOnlyThread(id, path string, line int, human gitprovider.Identity) gitprovider.InlineThread {
	threadID := gitprovider.ThreadID(id)
	created := fixedNow()
	return gitprovider.InlineThread{
		ID:          threadID,
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   testHeadSHA,
		Comments: []gitprovider.ThreadComment{{
			ID:          gitprovider.CommentID(id + "-human"),
			ThreadID:    threadID,
			Body:        "Human-only thread",
			Author:      human,
			CommitSHA:   testHeadSHA,
			Path:        path,
			Side:        review.DiffSideRight,
			Line:        line,
			SubjectType: review.AnchorKindLine,
			CreatedAt:   created,
			UpdatedAt:   created,
		}},
	}
}

func threadAnalysisResult(threadID string, decision threadanalysis.Decision, reply, summary string, resolve bool) llm.FakeResult {
	body := fmt.Sprintf(`{"schema_version":1,"thread_id":%q,"decision":%q,"reply_body":%q,"summary":%q,"resolve":%t,"rationale":"because"}`,
		threadID, decision, reply, summary, resolve)
	return llm.FakeResult{
		SessionID: "session-" + threadID,
		Response:  llm.Response{StructuredOutput: []byte(body)},
	}
}

func responsePlannedAction(t *testing.T, runID, actionID string, kind ledger.PlannedActionKind, threadID string, payload any) ledger.PlannedAction {
	t.Helper()
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	return ledger.PlannedAction{
		ActionID:    actionID,
		RunID:       runID,
		Kind:        kind,
		ThreadID:    &threadID,
		PlannedAt:   fixedNow(),
		PayloadJSON: string(payloadJSON),
		Status:      ledger.PlannedActionPending,
		Required:    true,
	}
}

func insertAction(t *testing.T, store *ledger.Store, action ledger.PlannedAction) {
	t.Helper()
	if err := store.InsertPlannedAction(context.Background(), action); err != nil {
		t.Fatalf("InsertPlannedAction: %v", err)
	}
}

func decodePayload[T any](t *testing.T, action ledger.PlannedAction) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal([]byte(action.PayloadJSON), &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	return payload
}

func sequence(prefix string) func() string {
	var mu sync.Mutex
	var counter int
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%s-%d", prefix, counter)
	}
}

func actionSequence() func(reviewplan.ActionKind) (string, error) {
	var mu sync.Mutex
	counters := map[reviewplan.ActionKind]int{}
	return func(kind reviewplan.ActionKind) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		counters[kind]++
		return fmt.Sprintf("%s-%d", kind, counters[kind]), nil
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func orderedBefore(events []string, first, second string) bool {
	firstIndex, secondIndex := -1, -1
	for i, event := range events {
		if event == first && firstIndex == -1 {
			firstIndex = i
		}
		if event == second && secondIndex == -1 {
			secondIndex = i
		}
	}
	return firstIndex != -1 && secondIndex != -1 && firstIndex < secondIndex
}

const (
	testHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
