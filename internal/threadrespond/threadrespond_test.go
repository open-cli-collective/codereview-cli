package threadrespond

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
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
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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
	postedActions, err := fixture.store.ListPlannedActions(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListPlannedActions posted: %v", err)
	}
	if len(postedActions) != 2 {
		t.Fatalf("posted actions = %#v, want reply and resolve", postedActions)
	}
	if !reflect.DeepEqual(result.PlannedActions, postedActions) {
		t.Fatalf("result planned actions = %#v, want refreshed posted actions %#v", result.PlannedActions, postedActions)
	}
	var sawPostedReply, sawPostedResolve bool
	for _, action := range postedActions {
		if action.Status != ledger.PlannedActionPosted || action.PostedAt == nil || action.UpstreamID == nil {
			t.Fatalf("posted action = %#v, want posted status/upstream metadata", action)
		}
		switch action.Kind {
		case ledger.PlannedActionThreadReply:
			sawPostedReply = true
			summaries := marker.FindThreadSummaries(replies[0].Body)
			if len(summaries) != 1 || summaries[0].ActionID != action.ActionID || summaries[0].RunID != result.Run.RunID {
				t.Fatalf("reply summary markers = %#v, want persisted action %s for run %s", summaries, action.ActionID, result.Run.RunID)
			}
		case ledger.PlannedActionResolveThread:
			sawPostedResolve = true
			if action.ThreadID == nil || *action.ThreadID != "thread-1" {
				t.Fatalf("resolve action = %#v, want thread-1", action)
			}
		case ledger.PlannedActionInlineComment, ledger.PlannedActionRollupComment, ledger.PlannedActionSubmitReview:
			t.Fatalf("posted action kind = %s, want thread reply/resolve", action.Kind)
		}
	}
	if !sawPostedReply || !sawPostedResolve {
		t.Fatalf("posted actions = %#v, want reply and resolve", postedActions)
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

func TestRunLiveAbortsWhenPRMovesBeforePosting(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	fixture.provider.beforeGetPR = func(provider *recordingProvider, call int) error {
		if call == 3 {
			if err := provider.SetPR(fixture.ref, movedPR(fixture.pr)); err != nil {
				return err
			}
		}
		return nil
	}

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run live moved: %v", err)
	}
	if !result.Outbox.Aborted || result.Outbox.Outcome != ledger.OutcomeAborted || result.ExitCode != exitUpstream {
		t.Fatalf("result = %#v, want aborted upstream result", result)
	}
	if !strings.Contains(result.Message, "premises moved") {
		t.Fatalf("message = %q, want premises moved", result.Message)
	}
	if replies := fixture.provider.RecordedThreadReplies(fixture.ref); len(replies) != 0 {
		t.Fatalf("thread replies = %#v, want no stale provider writes", replies)
	}
	if resolved := fixture.provider.RecordedResolvedThreads(fixture.ref); len(resolved) != 0 {
		t.Fatalf("resolved threads = %#v, want no stale provider writes", resolved)
	}
	stored, err := fixture.store.GetRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeAborted {
		t.Fatalf("stored outcome = %v, want aborted", stored.Outcome)
	}
}

func TestRunNoMatchingThreadsCompletesWithoutLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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

func TestRunResumesIncompleteAnalysisWithoutRecomputing(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionClarify, "Please clarify the intended behavior.", "", false))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	firstOpts := fixture.options()
	firstOpts.NewActionID = func(reviewplan.ActionKind) (string, error) {
		return "", context.Canceled
	}

	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted error = %v, want context.Canceled", err)
	}
	if len(fixture.adapter.Requests()) != 1 {
		t.Fatalf("first adapter requests = %d, want one analysis call", len(fixture.adapter.Requests()))
	}
	storedFirst, getErr := fixture.store.GetRun(ctx, first.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun first: %v", getErr)
	}
	if storedFirst.Outcome != nil {
		t.Fatalf("first run outcome = %v, want incomplete after cancellation", storedFirst.Outcome)
	}
	if actions, listErr := fixture.store.ListPlannedActions(ctx, first.Run.RunID); listErr != nil || len(actions) != 0 {
		t.Fatalf("first planned actions = %#v err=%v, want none before planning", actions, listErr)
	}

	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := fixture.options()
	secondOpts.NewRunID = sequence("fresh")
	second, err := Run(ctx, secondOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("Run resumed: %v", err)
	}
	if second.Run.RunID != first.Run.RunID {
		t.Fatalf("resumed run = %q, want original %q", second.Run.RunID, first.Run.RunID)
	}
	if second.Artifacts.Dir != first.Artifacts.Dir {
		t.Fatalf("resumed artifact dir = %q, want %q", second.Artifacts.Dir, first.Artifacts.Dir)
	}
	if len(fixture.adapter.Requests()) != 0 {
		t.Fatalf("resumed adapter requests = %d, want cached analysis", len(fixture.adapter.Requests()))
	}
	if len(second.PlannedActions) != 1 {
		t.Fatalf("resumed planned actions = %d, want one reply", len(second.PlannedActions))
	}
	if _, err := fixture.store.GetRun(ctx, "fresh-1"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRun fresh-1 error = %v, want ErrNotFound", err)
	}
	storedSecond, err := fixture.store.GetRun(ctx, first.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if storedSecond.Outcome == nil || *storedSecond.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("resumed outcome = %v, want dry_run", storedSecond.Outcome)
	}
}

func TestRunResumeRejectsChangedThreadInputBeforeLLM(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionClarify, "Please clarify the intended behavior.", "", false))
	original := markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{original})
	firstOpts := fixture.options()
	firstOpts.NewActionID = func(reviewplan.ActionKind) (string, error) {
		return "", context.Canceled
	}

	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted error = %v, want context.Canceled", err)
	}

	changed := original
	changed.Comments = append([]gitprovider.ThreadComment(nil), original.Comments...)
	changed.Comments[1].Body = "Human reply changed after the interrupted run"
	setInlineThreads(t, fixture, []gitprovider.InlineThread{changed})
	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm"}
	_, err = Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("Run changed thread error = %v, want stale lifecycle input error", err)
	}
	if len(fixture.adapter.Requests()) != 0 || len(fixture.adapter.Resumes()) != 0 {
		t.Fatalf("adapter starts=%#v resumes=%#v, want no provider call for stale cached analysis", fixture.adapter.Requests(), fixture.adapter.Resumes())
	}
	stored, getErr := fixture.store.GetRun(ctx, first.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun first: %v", getErr)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("stale resume outcome = %v, want failed", stored.Outcome)
	}
}

func TestRunResumesExistingActionsWithoutReplanning(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	firstOpts := fixture.options()
	firstOpts.Limiter = cancelLimiter{}

	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted post error = %v, want context.Canceled", err)
	}
	if len(fixture.adapter.Requests()) != 1 {
		t.Fatalf("first adapter requests = %d, want one analysis call", len(fixture.adapter.Requests()))
	}
	storedFirst, getErr := fixture.store.GetRun(ctx, first.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun first: %v", getErr)
	}
	if storedFirst.Outcome != nil {
		t.Fatalf("first run outcome = %v, want incomplete after post cancellation", storedFirst.Outcome)
	}
	if len(first.PlannedActions) != 2 {
		t.Fatalf("first planned actions = %d, want reply and resolve", len(first.PlannedActions))
	}

	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm"}
	secondOpts := fixture.options()
	secondOpts.NewRunID = sequence("fresh")
	second, err := Run(ctx, secondOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err != nil {
		t.Fatalf("Run resumed post: %v", err)
	}
	if second.Run.RunID != first.Run.RunID {
		t.Fatalf("resumed run = %q, want original %q", second.Run.RunID, first.Run.RunID)
	}
	if len(fixture.adapter.Requests()) != 0 {
		t.Fatalf("resumed adapter requests = %d, want no replanning LLM call", len(fixture.adapter.Requests()))
	}
	if _, err := fixture.store.GetRun(ctx, "fresh-1"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRun fresh-1 error = %v, want ErrNotFound", err)
	}
	if second.Outbox.Outcome != ledger.OutcomeComment || second.Outbox.Posted != 2 {
		t.Fatalf("resumed outbox = %#v, want comment with two posted actions", second.Outbox)
	}
	if replies := fixture.provider.RecordedThreadReplies(fixture.ref); len(replies) != 1 || replies[0].ThreadID != "thread-1" {
		t.Fatalf("thread replies = %#v, want resumed thread reply", replies)
	}
	if resolved := fixture.provider.RecordedResolvedThreads(fixture.ref); !reflect.DeepEqual(resolved, []gitprovider.ThreadID{"thread-1"}) {
		t.Fatalf("resolved threads = %#v, want thread-1", resolved)
	}
}

func TestRunResumeRejectsChangedThreadInputAfterActionsPersisted(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	original := markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{original})
	firstOpts := fixture.options()
	firstOpts.Limiter = cancelLimiter{}

	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted post error = %v, want context.Canceled", err)
	}
	if len(first.PlannedActions) != 2 {
		t.Fatalf("first planned actions = %d, want reply and resolve", len(first.PlannedActions))
	}

	changed := original
	changed.Comments = append([]gitprovider.ThreadComment(nil), original.Comments...)
	changed.Comments[1].Body = "Human reply changed after response actions were planned"
	setInlineThreads(t, fixture, []gitprovider.InlineThread{changed})
	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm"}
	_, err = Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("Run changed thread error = %v, want stale lifecycle input error", err)
	}
	if len(fixture.adapter.Requests()) != 0 || len(fixture.adapter.Resumes()) != 0 {
		t.Fatalf("adapter starts=%#v resumes=%#v, want no provider call for stale cached actions", fixture.adapter.Requests(), fixture.adapter.Resumes())
	}
	if replies := fixture.provider.RecordedThreadReplies(fixture.ref); len(replies) != 0 {
		t.Fatalf("thread replies = %#v, want no stale provider writes", replies)
	}
	if resolved := fixture.provider.RecordedResolvedThreads(fixture.ref); len(resolved) != 0 {
		t.Fatalf("resolved threads = %#v, want no stale provider writes", resolved)
	}
	stored, getErr := fixture.store.GetRun(ctx, first.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun first: %v", getErr)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeFailed {
		t.Fatalf("stale resume outcome = %v, want failed", stored.Outcome)
	}
}

func TestRunResumeRejectsCachedResolveWhenResolutionDisabled(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	firstOpts := fixture.options()
	firstOpts.Limiter = cancelLimiter{}

	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted post error = %v, want context.Canceled", err)
	}
	if len(first.PlannedActions) != 2 {
		t.Fatalf("first planned actions = %d, want reply and resolve", len(first.PlannedActions))
	}

	_, err = Run(ctx, fixture.options(), Request{
		PRRef:            fixture.ref,
		PRURL:            fixture.pr.URL,
		ProfileName:      "default",
		Profile:          testProfile(),
		PostingIdentity:  fixture.bot,
		NoResolveThreads: true,
	})
	if err == nil || !strings.Contains(err.Error(), "current options do not allow thread resolution") {
		t.Fatalf("Run no-resolve cached action error = %v, want resolution capability error", err)
	}
	if replies := fixture.provider.RecordedThreadReplies(fixture.ref); len(replies) != 0 {
		t.Fatalf("thread replies = %#v, want no stale provider writes", replies)
	}
	if resolved := fixture.provider.RecordedResolvedThreads(fixture.ref); len(resolved) != 0 {
		t.Fatalf("resolved threads = %#v, want no stale provider writes", resolved)
	}
}

func TestRunRerunBypassesIncompleteResponseRun(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionClarify, "Please clarify.", "", false))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})
	firstOpts := fixture.options()
	firstOpts.NewActionID = func(reviewplan.ActionKind) (string, error) {
		return "", context.Canceled
	}
	first, err := Run(ctx, firstOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run interrupted error = %v, want context.Canceled", err)
	}

	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionReplyOnly, "Fresh reply.", "", false))
	secondOpts := fixture.options()
	secondOpts.NewStepID = sequence("rerun-step")
	second, err := Run(ctx, secondOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
		Rerun:           true,
	})
	if err != nil {
		t.Fatalf("Run rerun: %v", err)
	}
	if second.Run.RunID == first.Run.RunID {
		t.Fatalf("rerun reused %q, want fresh run", second.Run.RunID)
	}
	if second.Artifacts.Dir == first.Artifacts.Dir {
		t.Fatalf("rerun artifacts dir = %q, want fresh artifact root", second.Artifacts.Dir)
	}
	storedFirst, err := fixture.store.GetRun(ctx, first.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun first: %v", err)
	}
	if storedFirst.Outcome != nil {
		t.Fatalf("bypassed run outcome = %v, want still incomplete", storedFirst.Outcome)
	}
	if len(fixture.adapter.Requests()) != 2 {
		t.Fatalf("adapter requests = %d, want original plus rerun analysis", len(fixture.adapter.Requests()))
	}
}

func TestRunDoesNotResumeIncompleteUnmarkedRun(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	unmarked := fixture.allocateRun(t, "unmarked-review-run", ledger.PostModeDryRun)
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionReplyOnly, "Fresh reply.", "", false))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Run.RunID == unmarked.RunID {
		t.Fatalf("result run = %q, want fresh response run instead of unmarked run", result.Run.RunID)
	}
	if len(fixture.adapter.Requests()) != 1 {
		t.Fatalf("adapter requests = %d, want fresh analysis for response run", len(fixture.adapter.Requests()))
	}
	storedUnmarked, err := fixture.store.GetRun(ctx, unmarked.RunID)
	if err != nil {
		t.Fatalf("GetRun unmarked: %v", err)
	}
	if storedUnmarked.Outcome != nil {
		t.Fatalf("unmarked run outcome = %v, want untouched incomplete", storedUnmarked.Outcome)
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
	if !reflect.DeepEqual(result.PlannedActions, storedActions) {
		t.Fatalf("result planned actions = %#v, want refreshed posted actions %#v", result.PlannedActions, storedActions)
	}
}

func TestRetryAbortsWhenPRMovesBeforePosting(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	run := fixture.allocateRun(t, "retry-run", ledger.PostModeLive)
	action := responsePlannedAction(t, run.RunID, "reply-1", ledger.PlannedActionThreadReply, "thread-1", outbox.ThreadReplyPayload{Body: "Retry this"})
	action.Status = ledger.PlannedActionFailedTerminal
	insertAction(t, fixture.store, action)
	fixture.provider.beforeGetPR = func(provider *recordingProvider, call int) error {
		if call == 3 {
			if err := provider.SetPR(fixture.ref, movedPR(fixture.pr)); err != nil {
				return err
			}
		}
		return nil
	}

	result, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		RetryRunID:      run.RunID,
	})
	if err != nil {
		t.Fatalf("Run retry moved: %v", err)
	}
	if !result.Outbox.Aborted || result.Outbox.Outcome != ledger.OutcomeAborted || result.ExitCode != exitUpstream {
		t.Fatalf("result = %#v, want aborted upstream result", result)
	}
	if replies := fixture.provider.RecordedThreadReplies(fixture.ref); len(replies) != 0 {
		t.Fatalf("thread replies = %#v, want no stale provider writes", replies)
	}
	stored, err := fixture.store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeAborted {
		t.Fatalf("stored outcome = %v, want aborted", stored.Outcome)
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

func TestRunFailedAnalysisCompletesRunIncomplete(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("stored outcome = %v, want incomplete", stored.Outcome)
	}
}

func TestRunFailedStructuredAnalysisPersistsAttemptsAndResumes(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	fixture.adapter.Queue(llm.FakeResult{
		SessionID: "failed-initial",
		Response:  llm.Response{StructuredOutput: []byte(`{"schema_version":1,"thread_id":"thread-1","decision":"bogus","resolve":false}`)},
	})
	fixture.adapter.Queue(llm.FakeResult{
		SessionID: "failed-retry",
		Response:  llm.Response{StructuredOutput: []byte(`{"schema_version":1,"thread_id":"thread-1","decision":"still_bogus","resolve":false}`)},
	})
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
		markedThread(t, "thread-1", "main.go", 10, false, fixture.bot, fixture.human),
	})

	first, err := Run(ctx, fixture.options(), Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err == nil {
		t.Fatal("Run invalid structured output error = nil, want validation failure")
	}
	storedFirst, getErr := fixture.store.GetRun(ctx, first.Run.RunID)
	if getErr != nil {
		t.Fatalf("GetRun first: %v", getErr)
	}
	if storedFirst.Outcome == nil || *storedFirst.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("first outcome = %v, want incomplete", storedFirst.Outcome)
	}
	paths := llmlifecycle.Paths{LLMTasksDir: first.Artifacts.LLMTasksDir}
	meta, ok, err := llmlifecycle.ReadMetadata(paths, "thread-analysis-thread-1")
	if err != nil || !ok {
		t.Fatalf("ReadMetadata failed task = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.Status != llmlifecycle.StatusFailedBlocking || meta.ProviderSessionID != "failed-retry" || len(meta.Attempts) != 2 {
		t.Fatalf("failed metadata = %#v, want blocking failure with attempts", meta)
	}
	for _, attempt := range meta.Attempts {
		if attempt.RawOutputPath == "" {
			t.Fatalf("attempt = %#v, want raw output path", attempt)
		}
		if _, err := os.Stat(attempt.RawOutputPath); err != nil {
			t.Fatalf("raw attempt %s stat: %v", attempt.RawOutputPath, err)
		}
	}

	fixture.adapter = &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionSummarize, "", "Summary for future context.", true))
	secondOpts := fixture.options()
	secondOpts.NewStepID = sequence("resume-step")
	second, err := Run(ctx, secondOpts, Request{
		PRRef:           fixture.ref,
		PRURL:           fixture.pr.URL,
		ProfileName:     "default",
		Profile:         testProfile(),
		PostingIdentity: fixture.bot,
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("Run resumed analysis: %v", err)
	}
	if second.Run.RunID != first.Run.RunID || second.Artifacts.Dir != first.Artifacts.Dir {
		t.Fatalf("resumed run/artifacts = %q %q, want %q %q", second.Run.RunID, second.Artifacts.Dir, first.Run.RunID, first.Artifacts.Dir)
	}
	if len(fixture.adapter.Requests()) != 0 {
		t.Fatalf("resumed adapter starts = %d, want resume call only", len(fixture.adapter.Requests()))
	}
	resumes := fixture.adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "failed-retry" {
		t.Fatalf("resumes = %#v, want failed-retry", resumes)
	}
	storedSecond, err := fixture.store.GetRun(ctx, first.Run.RunID)
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if storedSecond.Outcome == nil || *storedSecond.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("second outcome = %v, want dry_run", storedSecond.Outcome)
	}
	meta, ok, err = llmlifecycle.ReadMetadata(paths, "thread-analysis-thread-1")
	if err != nil || !ok {
		t.Fatalf("ReadMetadata resumed task = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.Status != llmlifecycle.StatusSucceeded || meta.ProviderSessionID != "session-thread-1" || meta.ValidatedOutputPath == "" {
		t.Fatalf("resumed metadata = %#v, want succeeded resumed task", meta)
	}
}

func TestRunLiveLocksBeforeReadingThreads(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	events := &eventLog{}
	fixture.provider = &recordingProvider{Fake: fixture.provider.Fake, events: events}
	fixture.adapter.Queue(threadAnalysisResult("thread-1", threadanalysis.DecisionReplyOnly, "Reply", "", false))
	setInlineThreads(t, fixture, []gitprovider.InlineThread{
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
	events      *eventLog
	mu          sync.Mutex
	getPRCalls  int
	beforeGetPR func(*recordingProvider, int) error
}

func (p *recordingProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	p.mu.Lock()
	p.getPRCalls++
	call := p.getPRCalls
	beforeGetPR := p.beforeGetPR
	p.mu.Unlock()
	if beforeGetPR != nil {
		if err := beforeGetPR(p, call); err != nil {
			return gitprovider.PR{}, err
		}
	}
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

type cancelLimiter struct{}

func (cancelLimiter) Wait(context.Context, string) error { return context.Canceled }

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

func setInlineThreads(t *testing.T, fixture *fixture, threads []gitprovider.InlineThread) {
	t.Helper()
	if err := fixture.provider.SetInlineThreads(fixture.ref, threads); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
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

func movedPR(pr gitprovider.PR) gitprovider.PR {
	pr.Head.SHA = testMovedHeadSHA
	return pr
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
	testHeadSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testMovedHeadSHA = "cccccccccccccccccccccccccccccccccccccccc"
	testBaseSHA      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
