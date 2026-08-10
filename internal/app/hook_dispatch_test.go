package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/hooks"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

func TestAppHookHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_APP_HOOK_HELPER") != "1" {
		return
	}
	body, _ := io.ReadAll(os.Stdin)
	if os.Getenv("APP_HOOK_SLEEP_EVENT") == os.Getenv("CR_EVENT") {
		delay, _ := time.ParseDuration(os.Getenv("APP_HOOK_SLEEP"))
		time.Sleep(delay)
	}
	// #nosec G304,G703 -- the parent test supplies a t.TempDir capture path.
	file, err := os.OpenFile(os.Getenv("APP_HOOK_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = file.Write(body)
	_ = file.Close()
	os.Exit(0)
}

func TestReviewHooksFanOutFromExistingProgressSeams(t *testing.T) {
	capture := t.TempDir() + "/events.jsonl"
	t.Setenv("GO_WANT_APP_HOOK_HELPER", "1")
	t.Setenv("APP_HOOK_CAPTURE", capture)
	t.Setenv("APP_HOOK_SLEEP_EVENT", "reviewer.completed")
	t.Setenv("APP_HOOK_SLEEP", "150ms")
	entries := hookEntries([]string{
		"run.started", "workspace.prepared", "dossier.ready", "selection.completed",
		"reviewer.completed", "plan.ready", "posting.action", "run.completed", "run.failed",
	})
	run := ledger.Run{RunID: "run-7", Attempt: 2, Profile: "work", ArtifactPath: t.TempDir()}
	if err := runartifact.WriteMarker(run.ArtifactPath, runartifact.KindReview, run.RunID); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	store := hookStore{run.RunID: run}
	dispatcher := newHookDispatcher(OpenRequest{
		Profile: config.Profile{Hooks: entries}, ProfileName: "work", Command: "review",
		PRURL: "https://github.com/acme/repo/pull/7",
	}, store)
	dispatcher.begin(false)
	ref := gitprovider.PRRef{Host: "github.com", Owner: "acme", Repo: "repo", Number: 7}
	fake := &gitprovider.Fake{}
	if err := fake.SetPR(ref, gitprovider.PR{Ref: ref, State: gitprovider.PRStateOpen, Author: gitprovider.Identity{Login: "piekstra", ID: "1795"}}); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if _, err := withProgressProvider(nil, dispatcher, "review", fake).GetPR(context.Background(), ref); err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	progress := newPipelineTaskProgress(nil, "review", dispatcher)
	logPath := run.ArtifactPath + "/agent-logs/task.jsonl"

	dossier := progress.StartLLMTask(pipeline.LLMTaskProgressEvent{TaskID: "dossier", Phase: "dossier", LogPath: logPath})
	dossier.End(nil, pipeline.LLMTaskProgressResult{Status: "succeeded"})
	selection := progress.StartLLMTask(pipeline.LLMTaskProgressEvent{TaskID: "selection", Phase: "selection", LogPath: logPath})
	selection.End(nil, pipeline.LLMTaskProgressResult{Status: "succeeded"})
	reviewer := progress.StartLLMTask(pipeline.LLMTaskProgressEvent{TaskID: "reviewer-a", Phase: "reviewer", AgentID: "agent-a", Model: "model-a", LogPath: logPath})
	started := time.Now()
	reviewer.End(nil, pipeline.LLMTaskProgressResult{Status: "succeeded"})
	if elapsed := time.Since(started); elapsed > 75*time.Millisecond {
		t.Fatalf("reviewer progress waited on hook for %s", elapsed)
	}

	agentID := "agent-a"
	dispatcher.completeReview(pipeline.Result{
		Run:       run,
		Selection: llm.Selection{SelectedAgents: []llm.SelectedAgent{{AgentID: agentID}}},
		Sessions:  []ledger.Session{{AgentID: &agentID, Model: "model-a"}},
	})
	action := marker.ActionMarker{RunID: run.RunID, ActionID: "action-1", Kind: marker.ActionKindRollupComment, SHA: "abc1234", BaseSHA: "def5678", Outcome: marker.RollupOutcomeComment}
	rendered, err := marker.RenderAction(action)
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	provider := withHookProvider(dispatcher, &gitprovider.Fake{})
	if _, err := provider.PostIssueComment(context.Background(), ref, rendered+"\n\nreview"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	dispatcher.emit("run.completed", hooks.Payload{Outcome: "comment"}, run, true)
	dispatcher.drain()

	payloads := readAppHookPayloads(t, capture)
	byEvent := map[string][]hooks.Payload{}
	for _, payload := range payloads {
		byEvent[payload.Event] = append(byEvent[payload.Event], payload)
	}
	for _, event := range []string{"run.started", "workspace.prepared", "dossier.ready", "selection.completed", "reviewer.completed", "plan.ready", "posting.action", "run.completed"} {
		if len(byEvent[event]) != 1 {
			t.Fatalf("event %s count = %d, payloads %#v", event, len(byEvent[event]), payloads)
		}
	}
	if len(byEvent["run.failed"]) != 0 {
		t.Fatalf("run.failed unexpectedly fired: %#v", byEvent["run.failed"])
	}
	if got := byEvent["reviewer.completed"][0]; got.ReviewerID != agentID || got.ReviewerStatus != "succeeded" || got.RunID != run.RunID {
		t.Fatalf("reviewer payload = %#v", got)
	}
	if got := byEvent["selection.completed"][0]; len(got.Agents) != 1 || got.Agents[0] != agentID || got.Models[agentID] != "model-a" {
		t.Fatalf("selection payload = %#v", got)
	}
	if got := byEvent["posting.action"][0]; got.ActionKind != marker.ActionKindRollupComment || got.ActionMarker != rendered {
		t.Fatalf("posting payload = %#v", got)
	}
	if got := byEvent["run.started"][0]; got.Author != "" {
		t.Fatalf("run.started carried an author before the pull request was read: %#v", got)
	}
	for _, event := range []string{"workspace.prepared", "dossier.ready", "selection.completed", "reviewer.completed", "plan.ready", "posting.action", "run.completed"} {
		if got := byEvent[event][0]; got.Author != "piekstra" {
			t.Fatalf("event %s author = %q, want piekstra", event, got.Author)
		}
	}
}

func TestHookAuthorKeepsTheFirstIdentityReadFromTheProvider(t *testing.T) {
	capture := t.TempDir() + "/events.jsonl"
	t.Setenv("GO_WANT_APP_HOOK_HELPER", "1")
	t.Setenv("APP_HOOK_CAPTURE", capture)
	dispatcher := newHookDispatcher(OpenRequest{
		Profile: config.Profile{Hooks: hookEntries([]string{"run.completed"})}, ProfileName: "work", Command: "review",
	}, hookStore{})
	ref := gitprovider.PRRef{Host: "github.com", Owner: "acme", Repo: "repo", Number: 7}
	fake := &gitprovider.Fake{}
	provider := withProgressProvider(nil, dispatcher, "review", fake)
	for _, pr := range []gitprovider.PR{
		{Ref: ref, State: gitprovider.PRStateOpen, Author: gitprovider.Identity{Login: "piekstra"}},
		{Ref: ref, State: gitprovider.PRStateOpen},
	} {
		if err := fake.SetPR(ref, pr); err != nil {
			t.Fatalf("SetPR: %v", err)
		}
		if _, err := provider.GetPR(context.Background(), ref); err != nil {
			t.Fatalf("GetPR: %v", err)
		}
	}
	dispatcher.emit("run.completed", hooks.Payload{Outcome: "approved"}, ledger.Run{RunID: "run-1"}, true)
	dispatcher.drain()

	payloads := readAppHookPayloads(t, capture)
	if len(payloads) != 1 || payloads[0].Author != "piekstra" {
		t.Fatalf("payloads = %#v, want one carrying author piekstra", payloads)
	}
}

func TestHookProviderWrapperIsSkippedWhenNoHooksAreConfigured(t *testing.T) {
	dispatcher := newHookDispatcher(OpenRequest{ProfileName: "work", Command: "review"}, hookStore{})
	fake := &gitprovider.Fake{}
	if got := withProgressProvider(nil, dispatcher, "review", fake); got != gitprovider.GitProvider(fake) {
		t.Fatalf("provider = %#v, want the unwrapped provider", got)
	}
}

func TestRespondHookNamespaceDoesNotFireReviewEvents(t *testing.T) {
	capture := t.TempDir() + "/events.jsonl"
	t.Setenv("GO_WANT_APP_HOOK_HELPER", "1")
	t.Setenv("APP_HOOK_CAPTURE", capture)
	entries := hookEntries([]string{"run.started", "posting.action", "run.completed", "respond.run.started", "respond.plan.ready", "respond.posting.action", "respond.run.completed"})
	run := ledger.Run{RunID: "respond-1", Attempt: 1, ArtifactPath: "/tmp/respond-1"}
	dispatcher := newHookDispatcher(OpenRequest{Profile: config.Profile{Hooks: entries}, ProfileName: "work", Command: "respond"}, hookStore{run.RunID: run})
	dispatcher.begin(false)
	action := marker.ActionMarker{RunID: run.RunID, ActionID: "respond-action", Kind: marker.ActionKindThreadReply, SHA: "abc1234", BaseSHA: "def5678"}
	body, err := marker.RenderAction(action)
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	provider := withHookProvider(dispatcher, &gitprovider.Fake{})
	if _, err := provider.ReplyToThread(context.Background(), gitprovider.PRRef{Host: "github.com", Owner: "acme", Repo: "repo", Number: 7}, "thread-1", body); err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}
	dispatcher.emit("respond.run.completed", hooks.Payload{Outcome: "nothing_to_review"}, run, true)
	dispatcher.drain()
	payloads := readAppHookPayloads(t, capture)
	if len(payloads) != 4 {
		t.Fatalf("respond payloads = %#v, want four", payloads)
	}
	for _, payload := range payloads {
		if payload.Event[:len("respond.")] != "respond." {
			t.Fatalf("review hook leaked into respond: %#v", payload)
		}
	}
}

func TestRunFailedHookCarriesOutcome(t *testing.T) {
	capture := t.TempDir() + "/events.jsonl"
	t.Setenv("GO_WANT_APP_HOOK_HELPER", "1")
	t.Setenv("APP_HOOK_CAPTURE", capture)
	dispatcher := newHookDispatcher(OpenRequest{
		Profile: config.Profile{Hooks: hookEntries([]string{"run.failed"})}, ProfileName: "work", Command: "review",
	}, hookStore{})
	run := ledger.Run{RunID: "failed-1", Attempt: 3, ArtifactPath: "/tmp/failed-1"}
	dispatcher.emit("run.failed", hooks.Payload{Outcome: "failed"}, run, true)
	dispatcher.drain()
	payloads := readAppHookPayloads(t, capture)
	if len(payloads) != 1 || payloads[0].Outcome != "failed" || payloads[0].PassNumber != 3 {
		t.Fatalf("failed payloads = %#v", payloads)
	}
}

type hookStore map[string]ledger.Run

func (s hookStore) GetRun(_ context.Context, runID string) (ledger.Run, error) {
	return s[runID], nil
}

func hookEntries(events []string) []config.Hook {
	entries := make([]config.Hook, 0, len(events))
	for _, event := range events {
		entries = append(entries, config.Hook{Event: event, Argv: []string{os.Args[0], "-test.run=^TestAppHookHelperProcess$"}, Timeout: "1s", OnDryRun: true})
	}
	return entries
}

func readAppHookPayloads(t *testing.T, path string) []hooks.Payload {
	t.Helper()
	file, err := os.Open(path) // #nosec G304 -- test path is controlled by t.TempDir.
	if err != nil {
		t.Fatalf("open hook capture: %v", err)
	}
	defer func() { _ = file.Close() }()
	var payloads []hooks.Payload
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var payload hooks.Payload
		if err := json.Unmarshal(scanner.Bytes(), &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		payloads = append(payloads, payload)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan payloads: %v", err)
	}
	return payloads
}
