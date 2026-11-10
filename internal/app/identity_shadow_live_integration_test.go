package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/gittest"
	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/identityshadow"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
	"github.com/open-cli-collective/codereview-cli/internal/view"
	"github.com/open-cli-collective/codereview-cli/internal/workbench"
)

const (
	liveShadowFirstBody  = "Preserve the first independent obligation."
	liveShadowSecondBody = "Preserve the second independent obligation."
	liveShadowPriorBody  = "Original prior obligation."
)

func TestLiveIdentityShadowPreservesAuthoritativeReview(t *testing.T) {
	catalog, _ := runtimeCatalogFixture(t)
	repoDir, baseSHA, headSHA := runtimeGitFixture(t)
	systemTemp := filepath.Join(t.TempDir(), "system-temp")
	if err := os.Mkdir(systemTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", systemTemp)
	agentDir := runtimeAgentFixture(t, repoDir)
	t.Setenv("GO_WANT_APP_HOOK_HELPER", "1")
	for _, scenario := range []string{"success-same", "malformed-provider", "cancelled-in-observer", "invalid-config", "comparison-write-failure"} { //nolint:misspell // Stable diagnostic category.
		t.Run(scenario, func(t *testing.T) {
			off := runLiveShadowIntegration(t, catalog, repoDir, agentDir, baseSHA, headSHA, scenario, false)
			on := runLiveShadowIntegration(t, catalog, repoDir, agentDir, baseSHA, headSHA, scenario, true)
			if off.authority != on.authority {
				t.Fatalf("shadow changed actual authoritative state:\noff: %s\non: %s", off.authority, on.authority)
			}
			if off.presentation != on.presentation {
				t.Fatalf("shadow changed pipeline presentation JSON:\noff: %s\non: %s", off.presentation, on.presentation)
			}
		})
	}
}

type liveShadowObservation struct {
	authority    string
	presentation string
}

type liveShadowNativeState struct {
	Run      ledger.Run
	Findings []ledger.Finding
	Actions  []ledger.PlannedAction
	Inline   []gitprovider.InlineComment
	Replies  []gitprovider.ThreadReply
	Resolved []gitprovider.ThreadID
	Rollups  []string
	Reviews  []gitprovider.ReviewRequest
	Threads  []gitprovider.InlineThread
	Ops      []gitprovider.Operation
}

func runLiveShadowIntegration(t *testing.T, catalog *modelcatalog.Catalog, repoDir, agentDir, baseSHA, headSHA, scenario string, enabled bool) liveShadowObservation {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	layout := statepaths.NewLayout(filepath.Join(root, "data"), filepath.Join(root, "cache"))
	store, err := ledger.Open(ctx, filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ref, pr := testPRRef(), testPR()
	pr.Base.SHA, pr.Head.SHA = baseSHA, headSHA
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	profile := testConfig().Profiles["home"]
	profile.AgentSources = []string{agentDir}
	profile.LLM = config.LLMConfig{Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthAPIKey, Adapter: config.LLMAdapterOpenAIAPI, Credential: profile.Git.Credential}.WithCatalog(catalog)
	profile.Hooks = hookEntries([]string{"run.completed"})
	var warnings bytes.Buffer
	hookCapture := filepath.Join(root, "hooks.jsonl")
	t.Setenv("APP_HOOK_CAPTURE", hookCapture)
	dispatcher := newHookDispatcher(OpenRequest{Profile: profile, ProfileName: "home", Command: "review", PRURL: pr.URL, Warnings: &warnings}, store)
	t.Cleanup(dispatcher.drain)
	provider := &liveShadowGitProvider{Fake: &gitprovider.Fake{}, identity: identity}
	provider.SetCapabilities(gitprovider.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true})
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: runtimeGitOutput(t, repoDir, "diff", baseSHA, headSHA)}); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	provider.threads = liveShadowPriorThread(t, pr, identity, fixed)
	if err := provider.SetInlineThreads(ref, provider.threads); err != nil {
		t.Fatal(err)
	}
	lockPath, err := layout.LockFile(statepaths.LockSpec{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, PRNumber: ref.Number, HeadSHA: headSHA, BaseSHA: baseSHA, Profile: "home", PostingIdentity: runlifecycle.PostingKey(identity)})
	if err != nil {
		t.Fatal(err)
	}
	var operations []gitprovider.Operation
	lockHeldAtSubmit := false
	provider.SetCallHook(func(op gitprovider.Operation) {
		operations = append(operations, op)
		if op == gitprovider.OperationSubmitReview {
			lock, acquireErr := runlock.Acquire(lockPath)
			if lock != nil {
				_ = lock.Release()
			}
			if !errors.Is(acquireErr, runlock.ErrHeld) {
				t.Errorf("same live gate lock was not held during SubmitReview: %v", acquireErr)
			}
			lockHeldAtSubmit = errors.Is(acquireErr, runlock.ErrHeld)
		}
	})
	adapter := liveShadowNativeAdapter()
	nativeState := func() liveShadowNativeState {
		run, getErr := store.GetRun(ctx, "live-shadow-run")
		if getErr != nil {
			t.Fatal(getErr)
		}
		findings, listErr := store.ListFindings(ctx, run.RunID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		actions, listErr := store.ListPlannedActions(ctx, run.RunID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		return liveShadowNativeState{Run: run, Findings: findings, Actions: actions,
			Inline: provider.RecordedInlineComments(ref), Replies: provider.RecordedThreadReplies(ref), Resolved: provider.RecordedResolvedThreads(ref),
			Rollups: provider.RecordedIssueComments(ref), Reviews: provider.RecordedReviews(ref), Threads: provider.threads,
			Ops: append([]gitprovider.Operation{}, operations...)}
	}
	configPath := filepath.Join(root, "shadow.yml")
	if err := os.WriteFile(configPath, []byte(liveShadowConfig(scenario)), 0o600); err != nil {
		t.Fatal(err)
	}
	shadow := &liveShadowDecider{malformed: scenario == "malformed-provider"}
	observerCalls, keyCalls, clientCalls := 0, 0, 0
	attemptDir := ""
	var observer identityshadow.Observer
	if enabled {
		actual := identityshadow.New(identityshadow.Options{
			ConfigPath: configPath, Warnings: &warnings,
			ResolveSystemOneKey: func(_ context.Context, storeID, name string) (string, error) {
				keyCalls++
				if storeID != "local-os" || name != "codereview/identity" {
					t.Errorf("unexpected shadow credential selection: %q %q", storeID, name)
				}
				return "synthetic-shadow-key", nil
			},
			SystemOne: identityexperiment.SystemOneDependencies{NewClient: func(options systemone.Options) (identityexperiment.SystemOneDecider, error) {
				clientCalls++
				shadow.limits = options.Limits
				return shadow, nil
			}},
			Files: identityexperiment.FileDependencies{Rename: func(oldPath, newPath string) error {
				if scenario == "comparison-write-failure" && filepath.Base(newPath) == "comparison.json" {
					return errors.New("synthetic comparison publication fault")
				}
				return os.Rename(oldPath, newPath)
			}},
		})
		observer = identityShadowObserverFunc(func(entryCtx context.Context, snapshot reportidentitysnapshot.Snapshot, path string) {
			observerCalls++
			attemptDir = path
			if len(snapshot.Findings) != 2 || snapshot.Findings[0].Body != liveShadowFirstBody || snapshot.Findings[1].Body != liveShadowSecondBody || len(snapshot.PriorRoots) != 1 || snapshot.PriorRoots[0].Body != liveShadowPriorBody {
				t.Fatalf("observer did not receive complete original source: %#v", snapshot)
			}
			before := nativeState()
			assertLiveShadowNativePosts(t, before)
			if !lockHeldAtSubmit {
				t.Fatal("no real SubmitReview lock probe occurred")
			}
			lock, acquireErr := runlock.Acquire(lockPath)
			if acquireErr != nil {
				t.Fatalf("live gate lock still held at observer entry: %v", acquireErr)
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
			dispatcher.drain()
			payloads := readAppHookPayloads(t, hookCapture)
			if len(payloads) != 1 || payloads[0].Event != "run.completed" || payloads[0].RunID != "live-shadow-run" || payloads[0].Outcome != "comment" || payloads[0].DryRun {
				t.Fatalf("completion hook was not dispatched before observer: %#v", payloads)
			}
			beforeJSON := liveShadowCanonical(t, before, root)
			if scenario == "cancelled-in-observer" { //nolint:misspell // Stable diagnostic category.
				child, cancel := context.WithCancel(entryCtx)
				cancel()
				actual.Observe(child, snapshot, path)
			} else {
				actual.Observe(entryCtx, snapshot, path)
			}
			if after := liveShadowCanonical(t, nativeState(), root); beforeJSON != after {
				t.Fatalf("observer changed native state or fetched native source:\nbefore: %s\nafter: %s", beforeJSON, after)
			}
		})
	}
	gitCommand := func(commandCtx context.Context, dir string, args ...string) ([]byte, error) {
		copied := append([]string{}, args...)
		if len(copied) >= 3 && copied[0] == "fetch" {
			copied[2] = repoDir
		}
		cmd := exec.CommandContext(commandCtx, "git", copied...) // #nosec G204 -- local Git fixture and structured test arguments.
		cmd.Env, cmd.Dir = gittest.Env(), dir
		out, commandErr := cmd.CombinedOutput()
		if commandErr != nil {
			return nil, fmt.Errorf("local fixture git: %w: %s", commandErr, out)
		}
		return out, nil
	}
	resolveRoot := func(context.Context) (string, error) { return repoDir, nil }
	runner := buildReviewRunner(store, provider, withHookProvider(dispatcher, provider), adapter, profile, noopLimiter{}, layout, &warnings, nil, dispatcher,
		OpenRequest{IdentityShadow: enabled, MaxConcurrency: 1, MaxAgents: 1}, "review", gitCommand, resolveRoot, observer)
	runner.pipeline.Now = func() time.Time { return fixed }
	runner.live.Now = runner.pipeline.Now
	runner.live.NewRunID = func() string { return "live-shadow-run" }
	runner.pipeline.NewRunID = runner.live.NewRunID
	findingID, sessionID, actionID := 0, 0, 0
	runner.pipeline.NewFindingID = func() (review.FindingID, error) {
		findingID++
		return review.FindingID(fmt.Sprintf("finding-%d", findingID)), nil
	}
	runner.pipeline.NewSessionRowID = func() string { sessionID++; return fmt.Sprintf("session-%d", sessionID) }
	runner.pipeline.NewActionID = func(kind reviewplan.ActionKind) (string, error) {
		actionID++
		return fmt.Sprintf("action-%d-%s", actionID, kind), nil
	}
	runner.live.Planner = withProgressPlanner(nil, dispatcher, livePlanner{opts: runner.pipeline})
	req := pipelineRequest(ref, pr, profile, identity)
	major := review.SeverityMajor
	req.FailOn, req.ToolVersion = &major, "identity-shadow-live-test"
	result, runErr := runner.Live(ctx, req, reviewrun.Flags{})
	if runErr != nil {
		t.Fatalf("actual Live(enabled=%t, %s): %v; warnings: %s", enabled, scenario, runErr, &warnings)
	}
	if result.Pipeline == nil || result.Decision.Kind != gate.DecisionFresh || result.ExitCode != 0 || !result.FailOnTriggered || !result.Pipeline.FailOnTriggered {
		t.Fatalf("live fixture failed to produce fresh, successful, fail-on-triggering plan: %#v", result)
	}
	expectedArtifacts := runartifact.FromDir(result.Pipeline.Artifacts.Dir)
	if !reflect.DeepEqual(result.Pipeline.Artifacts, expectedArtifacts) {
		t.Fatalf("actual artifact paths differ from paths derived from run directory: got=%#v want=%#v", result.Pipeline.Artifacts, expectedArtifacts)
	}
	for _, target := range []struct {
		name string
		path string
	}{
		{name: "workbench workspace", path: expectedArtifacts.WorkbenchWorkspace()},
		{name: "workbench artifact directory", path: expectedArtifacts.WorkbenchDir},
	} {
		if target.path == "" {
			t.Fatalf("expected %s path is empty", target.name)
		}
		if _, statErr := os.Lstat(target.path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("successful run did not remove %s %q: %v", target.name, target.path, statErr)
		}
	}
	state := nativeState()
	assertLiveShadowNativePosts(t, state)
	threads, err := provider.ListInlineThreads(ctx, ref)
	if err != nil || len(threads) != 1 || !threads[0].Resolved || len(threads[0].Comments) != 3 {
		t.Fatalf("actual thread writes were not readable: %#v, %v", threads, err)
	}
	if len(adapter.Requests()) != 5 || len(adapter.Resumes()) != 0 || len(result.Pipeline.Findings) != 2 || len(result.Pipeline.Rollup.DedupeLog) != 0 {
		t.Fatalf("native review did not retain its complete two-finding result: requests=%d findings=%#v rollup=%#v", len(adapter.Requests()), result.Pipeline.Findings, result.Pipeline.Rollup)
	}
	if !enabled {
		if observerCalls != 0 || keyCalls != 0 || clientCalls != 0 || len(shadow.requests) != 0 || result.Pipeline.ReportIdentity != nil {
			t.Fatal("disabled shadow performed work")
		}
		if _, statErr := os.Stat(filepath.Join(result.Pipeline.Artifacts.Dir, "identity-shadow")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("disabled shadow created artifacts: %v", statErr)
		}
	} else {
		if observerCalls != 1 {
			t.Fatalf("observer calls = %d, want 1", observerCalls)
		}
		shadowRoot := filepath.Join(expectedArtifacts.Dir, "identity-shadow")
		relativeAttempt, relErr := filepath.Rel(shadowRoot, attemptDir)
		if relErr != nil || relativeAttempt == "." || relativeAttempt == ".." || strings.HasPrefix(relativeAttempt, ".."+string(filepath.Separator)) {
			t.Fatalf("identity-shadow attempt %q is outside %q: relative=%q err=%v", attemptDir, shadowRoot, relativeAttempt, relErr)
		}
		if info, statErr := os.Stat(attemptDir); statErr != nil || !info.IsDir() {
			t.Fatalf("identity-shadow diagnostic attempt was not retained as a directory at %q: %v", attemptDir, statErr)
		}
		assertLiveShadowDiagnostics(t, scenario, attemptDir, keyCalls, clientCalls, shadow.requests, warnings.String())
	}
	dispatcher.drain()
	hookPayloads := readAppHookPayloads(t, hookCapture)
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]map[string]any, 0, len(adapter.Requests()))
	for _, request := range adapter.Requests() {
		requests = append(requests, map[string]any{"Model": request.Model, "Effort": request.Effort, "Prompt": request.Prompt, "LogPath": request.LogPath, "Fast": request.Fast,
			"DurableSession": request.DurableSession, "ReviewerWorkspace": request.ReviewerWorkspace, "FreshValidationRetrySession": request.FreshValidationRetrySession})
	}
	state = nativeState()
	full := struct {
		Result   reviewrun.Result
		State    liveShadowNativeState
		Sessions []ledger.Session
		Requests []map[string]any
		Hooks    any
	}{result, state, sessions, requests, hookPayloads}
	presentation, err := view.NewReviewDryRun(*result.Pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := view.Render(&rendered, true, presentation, nil); err != nil {
		t.Fatal(err)
	}
	var presentationJSON any
	if err := json.Unmarshal(rendered.Bytes(), &presentationJSON); err != nil {
		t.Fatal(err)
	}
	workbenchRoot := expectedArtifacts.WorkbenchWorkspace()
	reviewerRoot, reviewerRepo := expectedArtifacts.ReviewerWorkspace("harness:reviewer", workbench.ReviewerWorkspaceSegment("harness:reviewer"))
	workspaceReplacements := []string{
		expectedArtifacts.WorkbenchRepoDir, "<workbench-repo>",
		workbenchRoot, "<workbench-root>",
		reviewerRepo, "<reviewer-repo>",
		reviewerRoot, "<reviewer-root>",
	}
	return liveShadowObservation{
		authority:    liveShadowCanonical(t, full, root, workspaceReplacements...),
		presentation: liveShadowCanonical(t, presentationJSON, root, workspaceReplacements...),
	}
}

func liveShadowNativeAdapter() *llm.FakeAdapter {
	adapter := &llm.FakeAdapter{NameValue: "fake-openai", ReviewerWorkspaceModeSet: true, ReviewerWorkspaceModeValue: llm.ReviewerWorkspaceWrite}
	outputs := []string{
		`{"schema_version":1,"top_level_comments":[],"inline_threads":[]}`,
		`{"schema_version":1,"selected_agents":[{"agent_id":"harness:reviewer","rationale":"review changed file","files":["main.go"]}],"thread_actions":[],"reasoning":"two independent obligations"}`,
		`{"schema_version":1,"thread_id":"prior-thread","decision":"summarize","reply_body":"","summary":"The prior obligation was fixed.","resolve":true,"rationale":"The supplied discussion confirms the fix."}`,
		`{"schema_version":1,"agent_id":"harness:reviewer","inspected_files":["main.go"],"skipped_files":[],"constraints":[],"findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"line","side":"RIGHT","line":3},"body":"` + liveShadowFirstBody + `"},{"severity":"minor","file_path":"main.go","anchor":{"kind":"file"},"body":"` + liveShadowSecondBody + `"}]}`,
		`{"schema_version":1,"review_event":"comment","review_event_rationale":"Two independent obligations remain.","dedupe_log":[],"ordered_findings":["finding-1","finding-2"]}`,
	}
	for i, output := range outputs {
		adapter.Queue(runtimeFakeResult(fmt.Sprintf("provider-session-%d", i+1), output, 10, 2, ""))
	}
	return adapter
}

func liveShadowPriorThread(t *testing.T, pr gitprovider.PR, identity gitprovider.Identity, now time.Time) []gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{RunID: "old-run", ActionID: "old-action", Kind: marker.ActionKindInlineComment, SHA: pr.Head.SHA, BaseSHA: pr.Base.SHA})
	if err != nil {
		t.Fatal(err)
	}
	return []gitprovider.InlineThread{{ID: "prior-thread", Path: "main.go", Side: review.DiffSideRight, Line: 3, SubjectType: review.AnchorKindLine, CommitSHA: pr.Head.SHA,
		Comments: []gitprovider.ThreadComment{
			{ID: "prior-root", ThreadID: "prior-thread", Body: action + "\n" + liveShadowPriorBody, Author: identity, Path: "main.go", Side: review.DiffSideRight, Line: 3, SubjectType: review.AnchorKindLine, CommitSHA: pr.Head.SHA, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute)},
			{ID: "prior-human", ThreadID: "prior-thread", Body: "The old obligation is now fixed.", Author: gitprovider.Identity{Login: "human", ID: "human-id"}, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		},
	}}
}

type liveShadowGitProvider struct {
	*gitprovider.Fake
	identity gitprovider.Identity
	threads  []gitprovider.InlineThread
}

func (provider *liveShadowGitProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	id, err := provider.Fake.ReplyToThread(ctx, ref, threadID, body)
	if err != nil {
		return id, err
	}
	thread := &provider.threads[0]
	thread.Comments = append(thread.Comments, gitprovider.ThreadComment{ID: id, ThreadID: threadID, Body: body, Author: provider.identity, Path: thread.Path, CommitSHA: thread.CommitSHA, Side: thread.Side, Line: thread.Line, SubjectType: thread.SubjectType, CreatedAt: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)})
	return id, provider.SetInlineThreads(ref, provider.threads)
}

func (provider *liveShadowGitProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	if err := provider.Fake.ResolveThread(ctx, ref, threadID); err != nil {
		return err
	}
	provider.threads[0].Resolved = true
	return provider.SetInlineThreads(ref, provider.threads)
}

func assertLiveShadowNativePosts(t *testing.T, state liveShadowNativeState) {
	t.Helper()
	if state.Run.Outcome == nil || *state.Run.Outcome != ledger.OutcomeComment || state.Run.CompletedAt == nil || len(state.Findings) != 2 {
		t.Fatalf("actual ledger run was not completed with two findings: run=%#v findings=%#v", state.Run, state.Findings)
	}
	if state.Findings[0].Severity != review.SeverityMajor || state.Findings[0].Body != liveShadowFirstBody || state.Findings[1].Severity != review.SeverityMinor || state.Findings[1].Body != liveShadowSecondBody {
		t.Fatalf("actual ledger lost native finding severity/content: %#v", state.Findings)
	}
	if len(state.Inline) != 2 || len(state.Replies) != 1 || len(state.Resolved) != 1 || len(state.Rollups) != 0 || len(state.Reviews) != 1 || state.Reviews[0].Event != review.ReviewEventComment {
		t.Fatalf("real outbox write counts = inline:%d reply:%d resolve:%d issue:%d review:%d", len(state.Inline), len(state.Replies), len(state.Resolved), len(state.Rollups), len(state.Reviews))
	}
	if !strings.Contains(state.Reviews[0].Body, liveShadowFirstBody) || !strings.Contains(state.Reviews[0].Body, liveShadowSecondBody) {
		t.Fatal("submitted native review did not retain both findings in its rollup body")
	}
	if len(state.Actions) != 5 {
		t.Fatalf("native action count = %d, want reply/resolve/two findings/review with rollup", len(state.Actions))
	}
	for _, action := range state.Actions {
		if action.Status != ledger.PlannedActionPosted || action.PostedAt == nil || action.Attempts != 1 {
			t.Fatalf("actual outbox action was not posted once: %#v", action)
		}
	}
}

type liveShadowDecider struct {
	limits    systemone.Limits
	malformed bool
	requests  []systemone.Request
}

func (decider *liveShadowDecider) Decide(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return systemone.Receipt{}, err
	}
	plan, err := systemone.Prepare(request, decider.limits)
	if err != nil {
		return systemone.Receipt{}, err
	}
	decider.requests = append(decider.requests, request)
	answers := map[string]systemone.ChoiceAnswer{"identity": {Choice: "a", Confidence: .98, Probabilities: map[string]float64{"a": .98, "b": .01, "c": .01}}}
	if decider.malformed {
		answers = map[string]systemone.ChoiceAnswer{}
	}
	return systemone.Receipt{RequestedModel: request.Model, ReturnedModel: request.ExpectedModel, RequestSHA256: plan.RequestSHA256, ResponseSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic response"))), RequestID: "synthetic-request", Attempted: true, HTTPStatus: 200, Usage: &systemone.Usage{InputTokens: 8, OutputTokens: 2}, Answers: answers}, nil
}

func liveShadowConfig(scenario string) string {
	config := `schema_version: 1
provider:
  kind: system_one
  endpoint: https://example.invalid/system-one
  credential:
    store: local-os
    name: codereview/identity
  auth_scheme: bearer
model: shadow-test-model
expected_model: shadow-test-model
limits:
  max_state_bytes: 6144
  max_request_bytes: 8192
budget:
  max_calls: 40
  max_total_request_bytes: 327680
timeout: 10s
inference_timeout: 30s
`
	if scenario == "invalid-config" {
		config += "api_key_env: REJECTED_UNSHIPPED_FIELD\n"
	}
	return config
}

func assertLiveShadowDiagnostics(t *testing.T, scenario, dir string, keys, clients int, requests []systemone.Request, warnings string) {
	t.Helper()
	var receipt struct {
		Status           string `json:"status"`
		ProviderCalls    int    `json:"provider_calls"`
		ComparisonStatus string `json:"comparison_status"`
	}
	liveShadowReadJSON(t, filepath.Join(dir, "receipt.json"), &receipt)
	wantStatus, wantCalls := "complete", 3
	switch scenario {
	case "malformed-provider":
		wantStatus, wantCalls = "malformed_response", 1
	case "cancelled-in-observer": //nolint:misspell // Stable diagnostic category.
		wantStatus, wantCalls = expectedIdentityShadowCanceledStatus, 0
	case "invalid-config":
		wantStatus, wantCalls = "invalid_config", 0
	case "comparison-write-failure":
		wantStatus = "artifact_failure"
	}
	if receipt.Status != wantStatus || receipt.ProviderCalls != wantCalls || len(requests) != wantCalls {
		t.Fatalf("actual observer diagnostic = %#v, requests=%d; want status=%s calls=%d", receipt, len(requests), wantStatus, wantCalls)
	}
	wantKeys, wantClients := 1, 2
	switch wantCalls {
	case 0:
		wantKeys, wantClients = 0, 0
	case 1:
		wantClients = 1
	}
	if keys != wantKeys || clients != wantClients {
		t.Fatalf("shadow key/client calls = %d/%d, want %d/%d", keys, clients, wantKeys, wantClients)
	}
	if strings.Contains(warnings, "synthetic-shadow-key") || strings.Contains(warnings, "synthetic comparison publication fault") {
		t.Fatalf("shadow warning exposed credential or raw backend error: %s", warnings)
	}
	if wantStatus == "complete" {
		if strings.Contains(warnings, "cr identity-shadow") {
			t.Fatalf("successful diagnostic warned: %s", warnings)
		}
	} else if strings.Count(warnings, "cr identity-shadow status="+wantStatus) != 1 {
		t.Fatalf("missing single sanitized shadow warning for %s: %s", scenario, warnings)
	}
	if wantCalls > 0 {
		for _, task := range []string{"cohort", "prior_threads"} {
			if _, err := os.Stat(filepath.Join(dir, task, "manifest.json")); err != nil {
				t.Fatalf("actual nested %s artifacts were not retained: %v", task, err)
			}
		}
	}
	_, manifestErr := os.Stat(filepath.Join(dir, "manifest.json"))
	if scenario == "comparison-write-failure" {
		if !errors.Is(manifestErr, os.ErrNotExist) || receipt.ComparisonStatus != "unavailable" {
			t.Fatalf("failed comparison published misleading aggregate completion: %#v %v", receipt, manifestErr)
		}
	} else if manifestErr != nil {
		t.Fatalf("actual diagnostic aggregate manifest was not retained: %v", manifestErr)
	}
	if scenario == "success-same" || scenario == "malformed-provider" {
		var comparison identityshadow.Comparison
		liveShadowReadJSON(t, filepath.Join(dir, "comparison.json"), &comparison)
		if comparison.Cohort.ExpectedPairs != 1 || comparison.PriorExact.ExpectedPairs != 2 {
			t.Fatalf("observer lost full three-pair universe: %#v", comparison)
		}
		if scenario == "success-same" && (comparison.Cohort.Disagreements != 1 || comparison.PriorExact.Disagreements != 2 || comparison.Cohort.UnresolvedPairs+comparison.PriorExact.UnresolvedPairs != 0) {
			t.Fatalf("same predictions were not measured against unchanged distinct baselines: %#v", comparison)
		}
		if scenario == "malformed-provider" && comparison.Cohort.UnresolvedPairs+comparison.PriorExact.UnresolvedPairs != 3 {
			t.Fatalf("malformed first response lost unresolved denominators: %#v", comparison)
		}
	}
	if wantCalls > 0 {
		assertLiveShadowRequestRecords(t, requests)
	}
	for _, name := range []string{"source.json", "baseline.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("actual observer did not retain %s: %v", name, err)
		}
	}
}

func assertLiveShadowRequestRecords(t *testing.T, requests []systemone.Request) {
	t.Helper()
	first := map[string]any{"kind": "finding", "finding": map[string]any{
		"finding_id": "finding-1", "severity": "major", "file_path": "main.go", "anchor": map[string]any{"kind": "line", "side": "RIGHT", "line": float64(3)}, "body": liveShadowFirstBody,
	}}
	second := map[string]any{"kind": "finding", "finding": map[string]any{
		"finding_id": "finding-2", "severity": "minor", "file_path": "main.go", "anchor": map[string]any{"kind": "file"}, "body": liveShadowSecondBody,
	}}
	prior := map[string]any{"kind": "thread", "thread": map[string]any{
		"thread_id": "prior-thread", "eligible": true, "file_path": "main.go", "body": liveShadowPriorBody,
	}}
	expected := []map[string]any{
		{"protocol_version": float64(1), "task": "cohort", "left": first, "right": second},
		{"protocol_version": float64(1), "task": "prior_threads", "left": first, "right": prior},
		{"protocol_version": float64(1), "task": "prior_threads", "left": second, "right": prior},
	}
	for index, request := range requests {
		var state map[string]any
		if err := json.Unmarshal(request.State, &state); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(state, expected[index]) || request.Model != "shadow-test-model" || request.ExpectedModel != "shadow-test-model" {
			t.Fatalf("actual request %d changed full report values or leaked baseline data: %s", index, request.State)
		}
	}
}

func liveShadowReadJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- path is a test-owned diagnostic artifact.
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func liveShadowCanonical(t *testing.T, value any, root string, pathReplacements ...string) string {
	t.Helper()
	if len(pathReplacements)%2 != 0 {
		t.Fatalf("workspace path replacements must be source/replacement pairs, got %d values", len(pathReplacements))
	}
	for index := 0; index < len(pathReplacements); index += 2 {
		if pathReplacements[index] == "" || pathReplacements[index+1] == "" {
			t.Fatalf("workspace path replacement %d contains an empty source or value", index/2)
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var normalize func(any) any
	normalize = func(value any) any {
		switch typed := value.(type) {
		case map[string]any:
			for key, item := range typed {
				switch key {
				case "StartedAt", "CompletedAt", "AttemptedAt", "PostedAt", "PlannedAt":
					if item != nil {
						typed[key] = "<timestamp>"
					}
				default:
					typed[key] = normalize(item)
				}
			}
		case []any:
			for index, item := range typed {
				typed[index] = normalize(item)
			}
		case string:
			normalized := typed
			for index := 0; index < len(pathReplacements); index += 2 {
				normalized = strings.ReplaceAll(normalized, pathReplacements[index], pathReplacements[index+1])
			}
			return strings.ReplaceAll(normalized, root, "<run-root>")
		}
		return value
	}
	encoded, err := json.Marshal(normalize(decoded))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestLiveShadowCanonicalNormalizesOnlyExpectedWorkspacePaths(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "run")
	workbenchRoot := filepath.Join(t.TempDir(), "data", "ws", "workbench-hash")
	workbenchRepo := filepath.Join(workbenchRoot, "r")
	unknownHash := "deadbeef123456"
	unknownWorkspace := filepath.Join(t.TempDir(), "data", "ws", unknownHash, "r")
	value := map[string]string{
		"workbench_repo":    workbenchRepo,
		"workbench_root":    workbenchRoot,
		"unknown_workspace": unknownWorkspace,
		"unrelated_hash":    unknownHash,
		"run_artifact_path": filepath.Join(runRoot, "findings.json"),
	}
	canonical := liveShadowCanonical(t, value, runRoot, workbenchRepo, "<workbench-repo>", workbenchRoot, "<workbench-root>")
	var got map[string]string
	if err := json.Unmarshal([]byte(canonical), &got); err != nil {
		t.Fatalf("Decode normalized workspace fixture: %v", err)
	}
	if got["workbench_repo"] != "<workbench-repo>" || got["workbench_root"] != "<workbench-root>" ||
		got["run_artifact_path"] != filepath.Join("<run-root>", "findings.json") {
		t.Fatalf("known workspace/run paths were not normalized: %#v", got)
	}
	if got["unknown_workspace"] != unknownWorkspace || got["unrelated_hash"] != unknownHash {
		t.Fatalf("unrelated hash or unknown workspace path was normalized: %#v", got)
	}
}
