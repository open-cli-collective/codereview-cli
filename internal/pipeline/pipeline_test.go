package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
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
	adapter := &llm.FakeAdapter{
		NameValue:      "fake-llm",
		QuotaValue:     llm.Quota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		QuotaSupported: true,
	}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := DryRun(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
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
}

func TestLiveMarksRunFailedAfterPlanningError(t *testing.T) {
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
	if storedRun.Outcome == nil || *storedRun.Outcome != ledger.OutcomeFailed {
		t.Fatalf("stored outcome = %#v, want failed", storedRun.Outcome)
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
	for _, request := range requests {
		if strings.Contains(request.Prompt, `"schema": "findings"`) {
			reviewerPrompts++
			if !strings.Contains(request.Prompt, `"agent"`) || !strings.Contains(request.Prompt, `"files"`) {
				t.Fatalf("reviewer prompt missing agent/files context: %s", request.Prompt)
			}
		}
	}
	if reviewerPrompts != 2 {
		t.Fatalf("reviewer prompts = %d, want 2", reviewerPrompts)
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

	_, err := DryRun(context.Background(), Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{},
		Store:    &noopStore{},
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, req)
	if err == nil {
		t.Fatal("DryRun error = nil, want self-review guard")
	}
	if !strings.Contains(err.Error(), "--allow-self-review") {
		t.Fatalf("DryRun error = %v, want allow-self-review guidance", err)
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
			name:   "selection",
			budget: 100,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
				req.Profile.AgentSources = []string{dir}
			},
			want:  "context budget exceeded for selection",
			runID: "run-budget-selection",
		},
		{
			name:   "reviewer diff",
			budget: 3000,
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
			name:   "full content",
			budget: 10000,
			mutate: func(t *testing.T, provider *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgentFullContent(t, dir, "harness", "reviewer")
				req.Profile.AgentSources = []string{dir}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: "main.go"}] = []byte(strings.Repeat("base\n", 3000))
				provider.files[fileKey{gitRef: provider.pr.Head.SHA, path: "main.go"}] = []byte("package main\n")
			},
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
			},
			want:  "context budget exceeded for full-content agent harness:reviewer",
			runID: "run-budget-full-content",
		},
		{
			name:   "rollup",
			budget: 3000,
			queue: func(adapter *llm.FakeAdapter) {
				adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
				adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, strings.Repeat("body ", 1000)), 1, 1))
			},
			want:  "context budget exceeded for rollup",
			runID: "run-budget-rollup",
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
	pr      gitprovider.PR
	diff    gitprovider.UnifiedDiff
	files   map[fileKey][]byte
	trees   map[fileKey][]gitprovider.TreeEntry
	threads []gitprovider.InlineThread
	caps    gitprovider.ProviderCaps
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
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", []string{"finding-1", "finding-2"})}, nil
	default:
		return nil, fmt.Errorf("unexpected prompt: %s", req.Prompt)
	}
}

func (a *promptAwareAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

type staticStream struct {
	sessionID string
	output    string
}

func (s staticStream) SessionID() string {
	return s.sessionID
}

func (s staticStream) Wait(context.Context) (llm.Response, error) {
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
	return p.pr, nil
}

func (p *readOnlyProvider) GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return p.diff, nil
}

func (p *readOnlyProvider) GetFileAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	data, ok := p.files[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (p *readOnlyProvider) ListTreeAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error) {
	entries, ok := p.trees[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]gitprovider.TreeEntry(nil), entries...), nil
}

func (p *readOnlyProvider) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return append([]gitprovider.InlineThread(nil), p.threads...), nil
}

func (p *readOnlyProvider) Capabilities() gitprovider.ProviderCaps {
	return p.caps
}

func dryRunHarness(t *testing.T) (*readOnlyProvider, Request) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("a", 40)
	pr := gitprovider.PR{
		Ref:    ref,
		Title:  "CR-20 dry-run",
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

func writeAgentFullContent(t *testing.T, rootDir, category, agent string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentYAML(agent, "full content reviewer", true))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), "Review full files.")
}

func agentYAML(name, description string, needsFullContent bool) string {
	return fmt.Sprintf("name: %s\ndescription: %s\nmodel: sonnet\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: %t\n", name, description, needsFullContent)
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

func (p *failingProvider) Capabilities() gitprovider.ProviderCaps {
	return gitprovider.ProviderCaps{}
}

type noopStore struct{}

func (noopStore) AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error) {
	return ledger.Run{}, nil
}

func (noopStore) InsertSession(context.Context, ledger.Session) error {
	return nil
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
