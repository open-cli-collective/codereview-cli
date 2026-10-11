package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestLiveThreadAnalysisFailureContinuesAndPosts(t *testing.T) {
	for _, invalidOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid_output=%t", invalidOutput), func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			provider.caps.ThreadResolution = true
			for i := 1; i <= 3; i++ {
				provider.threads = append(provider.threads, markedReviewThread(t, fmt.Sprintf("thread-%d", i), "main.go", 2, req.PostingIdentity, gitprovider.Identity{Login: "human", ID: "human-id"}))
			}
			run := allocateLiveRun(t, store, provider, req, "run-partial-thread-analysis")
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			queuePartialThreadReview(t, adapter, invalidOutput)
			var warnings bytes.Buffer
			result, err := liveForTest(ctx, Options{
				Provider: provider, Adapter: adapter, Store: store,
				Layout: statepaths.NewLayout(t.TempDir(), t.TempDir()), Now: fixedNow,
				NewSessionRowID: sequence("session"), NewFindingID: findingSequence("finding"), NewActionID: actionSequence(),
				MaxConcurrency: 1, Warnings: &warnings,
			}, req, run)
			if err != nil {
				t.Fatalf("Live: %v", err)
			}
			if result.Plan.Outcome != reviewplan.OutcomeComment || len(result.ThreadAnalysisFailures) != 1 || result.ThreadAnalysisFailures[0].ThreadID != "thread-2" {
				t.Fatalf("plan/failures = %s/%#v, want completed non-approving partial review", result.Plan.Outcome, result.ThreadAnalysisFailures)
			}
			if len(result.ReviewerCoverage) != 1 || result.ReviewerCoverage[0].Status != reviewerCoverageCompleteBroad {
				t.Fatalf("reviewer coverage = %#v, want reviewer phase to run", result.ReviewerCoverage)
			}
			for _, want := range []string{"thread-2", "not analyzed", "cached on resume", "--rerun"} {
				if !strings.Contains(warnings.String(), want) || !strings.Contains(result.Plan.RollupMarkdown, want) {
					t.Fatalf("missing diagnostic %q: warnings=%s rollup=%s", want, warnings.String(), result.Plan.RollupMarkdown)
				}
			}
			requests := adapter.Requests()
			if !strings.Contains(requests[len(requests)-1].Prompt, `"thread_analysis_failures"`) || !strings.Contains(requests[len(requests)-1].Prompt, `"thread_id":"thread-2"`) {
				t.Fatalf("rollup prompt missing failure: %s", requests[len(requests)-1].Prompt)
			}
			for _, action := range result.PlannedActions {
				if action.ThreadID == "thread-2" {
					t.Fatalf("failed thread has planned action: %#v", action)
				}
			}
			for i := 1; i <= 3; i++ {
				meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), fmt.Sprintf("thread-analysis-thread-%d", i))
				want := llmlifecycle.StatusSucceeded
				if i == 2 {
					want = llmlifecycle.StatusFailedIsolated
				}
				if err != nil || !ok || meta.Status != want {
					t.Fatalf("thread %d metadata = %#v ok=%t err=%v", i, meta, ok, err)
				}
			}
			posting := &gitprovider.Fake{}
			posting.SetCapabilities(provider.caps)
			if err := posting.SetPR(req.PRRef, provider.pr); err != nil {
				t.Fatal(err)
			}
			if err := posting.SetInlineThreads(req.PRRef, provider.threads); err != nil {
				t.Fatal(err)
			}
			posted, err := outbox.Post(ctx, outbox.Options{Store: store, Provider: posting, Limiter: threadIsolationLimiter{}, Now: fixedNow}, outbox.Request{Run: run, PRRef: req.PRRef, PostingIdentity: req.PostingIdentity, DesiredOutcome: ledger.OutcomeComment})
			if err != nil || posted.ExitCode != 0 || posted.Outcome != ledger.OutcomeComment {
				t.Fatalf("Post = %#v err=%v, want completed review", posted, err)
			}
			if replies := posting.RecordedThreadReplies(req.PRRef); len(replies) != 2 {
				t.Fatalf("replies = %#v, want both successful siblings", replies)
			}
			resolved := posting.RecordedResolvedThreads(req.PRRef)
			if len(resolved) != 2 {
				t.Fatalf("resolutions = %#v, want successful siblings only", resolved)
			}
			for _, id := range resolved {
				if id == "thread-2" {
					t.Fatal("unanalyzed thread was resolved")
				}
			}
			if reviews := posting.RecordedReviews(req.PRRef); len(reviews) != 1 || reviews[0].Event != review.ReviewEventComment || !strings.Contains(reviews[0].Body, "Unanalyzed Threads") {
				t.Fatalf("posted reviews = %#v, want reported partial coverage", reviews)
			}
		})
	}
}

func TestLiveResumeCachesIsolatedThreadAnalysisFailure(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.threads = []gitprovider.InlineThread{markedReviewThread(t, "thread-2", "main.go", 2, req.PostingIdentity, gitprovider.Identity{Login: "human", ID: "human-id"})}
	run := allocateLiveRun(t, store, provider, req, "run-resume-partial-thread-analysis")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(llm.FakeResult{SessionID: "failed-thread", WaitErr: errors.New("analysis provider failed")})
	adapter.Queue(fakeLLMResult("reviewer", findingsWithCoverageJSON("harness:reviewer", []string{"main.go"}, nil, nil, nil), 1, 1))
	adapter.Queue(llm.FakeResult{SessionID: "failed-rollup", WaitErr: errors.New("rollup provider failed")})
	opts := Options{Provider: provider, Adapter: adapter, Store: store, Layout: statepaths.NewLayout(t.TempDir(), t.TempDir()), Now: fixedNow, NewSessionRowID: sequence("session"), NewFindingID: findingSequence("finding"), NewActionID: actionSequence(), MaxConcurrency: 1}
	if _, err := liveForTest(ctx, opts, req, run); err == nil || ClassifyFailure(err) != FailureDurableBlocking {
		t.Fatalf("first Live error = %v, want blocking rollup failure", err)
	}
	resumed := &llm.FakeAdapter{NameValue: "fake-llm"}
	resumed.Queue(fakeLLMResult("recovered-rollup", rollupJSON("approve", nil), 1, 1))
	opts.Adapter = resumed
	opts.NewSessionRowID = sequence("resumed-session")
	result, err := liveForTest(ctx, opts, req, run)
	if err != nil {
		t.Fatalf("resumed Live: %v", err)
	}
	if len(resumed.Requests()) != 1 || !strings.Contains(resumed.Requests()[0].Prompt, `"schema":"rollup"`) {
		t.Fatalf("resume requests = %#v, want only blocking rollup retry", resumed.Requests())
	}
	if len(result.ThreadAnalysisFailures) != 1 || result.Plan.Outcome != reviewplan.OutcomeComment || !strings.Contains(result.Plan.RollupMarkdown, "thread-2") {
		t.Fatalf("resumed partial outcome = %#v/%s", result.ThreadAnalysisFailures, result.Plan.Outcome)
	}
}

func queuePartialThreadReview(t *testing.T, adapter *llm.FakeAdapter, invalidOutput bool) {
	t.Helper()
	adapter.Queue(fakeLLMResult("dossier", discussionSummaryJSON(nil, nil), 1, 1))
	selection := strings.Replace(selectionJSON("harness:reviewer", "main.go"), `"thread_actions": []`, `"thread_actions": [{"thread_id":"thread-2","decision":"summarize_and_resolve","summary":"selector proposed closure","safe_to_resolve_rationale":"settled"}]`, 1)
	adapter.Queue(fakeLLMResult("selection", selection, 1, 1))
	adapter.Queue(fakeLLMResult("thread-1", `{"thread_id":"thread-1","decision":"summarize","summary":"First thread settled","resolve":true,"rationale":"settled"}`, 1, 1))
	if invalidOutput {
		for i := 0; i < 2; i++ {
			adapter.Queue(fakeLLMResult(fmt.Sprintf("thread-2-attempt-%d", i), `{"thread_id":"thread-2","decision":"invalid","resolve":false}`, 1, 1))
		}
	} else {
		adapter.Queue(llm.FakeResult{SessionID: "thread-2-failed", WaitErr: errors.New("thread provider failed")})
	}
	adapter.Queue(fakeLLMResult("thread-3", `{"thread_id":"thread-3","decision":"summarize","summary":"Third thread settled","resolve":true,"rationale":"settled"}`, 1, 1))
	adapter.Queue(fakeLLMResult("reviewer", findingsWithCoverageJSON("harness:reviewer", []string{"main.go"}, nil, nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("rollup", rollupJSON("approve", nil), 1, 1))
}

type threadIsolationLimiter struct{}

func (threadIsolationLimiter) Wait(context.Context, string) error { return nil }
