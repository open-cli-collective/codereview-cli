package respondcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewruntime"
	"github.com/open-cli-collective/codereview-cli/internal/threadanalysis"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
)

func TestRespondDryRunCallsResponderAndRendersText(t *testing.T) {
	responder := &fakeResponder{result: testThreadRespondResult(ledger.OutcomeDryRun)}
	var cleanupCalled bool
	var gotRuntime reviewruntime.OpenRequest
	cmd, out := newTestCommand(t, testConfig(), func(_ context.Context, req reviewruntime.OpenRequest) (reviewruntime.Runtime, error) {
		gotRuntime = req
		return reviewruntime.Runtime{
			Responder:       responder,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
			Cleanup:         func() { cleanupCalled = true },
		}, nil
	})

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--no-resolve-threads",
	})
	if err != nil {
		t.Fatalf("Execute respond: %v", err)
	}
	if len(responder.requests) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(responder.requests))
	}
	req := responder.requests[0]
	if req.PRRef.Number != 29 || req.ProfileName != "home" || req.PostingIdentity.Login != "review-bot" {
		t.Fatalf("respond request identity/ref = %#v", req)
	}
	if !req.DryRun || !req.NoResolveThreads || req.Rerun {
		t.Fatalf("respond request flags = %#v, want dry-run no-resolve", req)
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	if gotRuntime.Command != "respond" || gotRuntime.Progress == nil || gotRuntime.Warnings != out {
		t.Fatalf("runtime command/progress/warnings = %#v/%#v/%#v, want respond/progress/stdout-stderr", gotRuntime.Command, gotRuntime.Progress, gotRuntime.Warnings)
	}
	text := out.String()
	if !strings.Contains(text, "Threads: considered 1, responded 1, provider resolved 0 (resolve planned 1, failed 0)") || !strings.Contains(text, "Planned actions: 2") {
		t.Fatalf("stdout = %q, want respond summary", text)
	}
}

func TestRespondRejectsAmbiguousRepositoryProfileRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
	}
	cmd, _ := newTestCommand(t, cfg, func(context.Context, reviewruntime.OpenRequest) (reviewruntime.Runtime, error) {
		t.Fatal("runtime factory should not be called for ambiguous repository routes")
		return reviewruntime.Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if !errors.Is(err, config.ErrRepositoryProfileAmbiguous) {
		t.Fatalf("Execute error = %v, want ErrRepositoryProfileAmbiguous", err)
	}
	if !strings.Contains(err.Error(), "pass --profile with one of: home, work") {
		t.Fatalf("error = %v, want profile suggestions", err)
	}
}

func TestRespondRetryPostsFlagCallsResponder(t *testing.T) {
	result := testThreadRespondResult(ledger.OutcomeComment)
	result.Plan = reviewplan.Plan{}
	responder := &fakeResponder{result: result}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(responder))

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--retry-posts", "run-123",
	})
	if err != nil {
		t.Fatalf("Execute respond retry: %v", err)
	}
	if len(responder.requests) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(responder.requests))
	}
	if got := responder.requests[0].RetryRunID; got != "run-123" {
		t.Fatalf("RetryRunID = %q, want run-123", got)
	}
	if responder.requests[0].DryRun {
		t.Fatalf("respond retry request = %#v, want live retry", responder.requests[0])
	}
	if text := out.String(); !strings.Contains(text, "responded 1, provider resolved 0 (resolve planned 1, failed 0)") || !strings.Contains(text, "Planned actions: 2") {
		t.Fatalf("stdout = %q, want retry counts from planned actions", text)
	}
}

func TestRespondJSONRendersStableShape(t *testing.T) {
	responder := &fakeResponder{result: testThreadRespondResult(ledger.OutcomeComment)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(responder))

	if err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--json"}); err != nil {
		t.Fatalf("Execute respond json: %v", err)
	}
	var decoded struct {
		Run struct {
			RunID        string `json:"run_id"`
			PRURL        string `json:"pr_url"`
			PRKey        string `json:"pr_key"`
			PostMode     string `json:"post_mode"`
			Outcome      string `json:"outcome"`
			ArtifactPath string `json:"artifact_path"`
			BaseSHA      string `json:"base_sha"`
			HeadSHA      string `json:"head_sha"`
		} `json:"run"`
		Counts counts `json:"counts"`
		Outbox struct {
			Outcome        string `json:"outcome"`
			ExitCode       int    `json:"exit_code"`
			Posted         int    `json:"posted"`
			Pending        int    `json:"pending"`
			FailedTerminal int    `json:"failed_terminal"`
			Aborted        bool   `json:"aborted"`
		} `json:"outbox"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal stdout: %v\n%s", err, out.String())
	}
	if decoded.Run.RunID != "respond-run-1" ||
		decoded.Run.PRURL != "https://github.com/open-cli-collective/codereview-cli/pull/29" ||
		decoded.Run.PRKey != "github.com_open-cli-collective_codereview-cli_29" ||
		decoded.Run.PostMode != "live" ||
		decoded.Run.Outcome != "comment" ||
		decoded.Run.ArtifactPath != "/tmp/respond-run-1" ||
		decoded.Run.BaseSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		decoded.Run.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		decoded.Counts.Considered != 1 ||
		decoded.Counts.Responded != 1 ||
		decoded.Counts.Resolved != 0 ||
		decoded.Counts.ProviderResolved != 0 ||
		decoded.Counts.ResolvePlanned != 1 ||
		decoded.Counts.ResolveFailed != 0 ||
		decoded.Counts.Planned != 2 ||
		decoded.Outbox.Outcome != "comment" ||
		decoded.Outbox.ExitCode != 0 ||
		decoded.Outbox.Posted != 2 ||
		decoded.Outbox.Pending != 0 ||
		decoded.Outbox.FailedTerminal != 0 ||
		decoded.Outbox.Aborted {
		t.Fatalf("decoded json = %#v, want response summary", decoded)
	}
}

func TestRespondJSONRendersPostedProviderResolve(t *testing.T) {
	result := testThreadRespondResult(ledger.OutcomeComment)
	result.PlannedActions[1].Status = ledger.PlannedActionPosted
	responder := &fakeResponder{result: result}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(responder))

	if err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--json"}); err != nil {
		t.Fatalf("Execute respond json: %v", err)
	}
	var decoded struct {
		Counts counts `json:"counts"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal stdout: %v\n%s", err, out.String())
	}
	if decoded.Counts.Resolved != 1 ||
		decoded.Counts.ProviderResolved != 1 ||
		decoded.Counts.ResolvePlanned != 1 ||
		decoded.Counts.ResolveFailed != 0 {
		t.Fatalf("counts = %#v, want posted provider resolve plus planned resolve", decoded.Counts)
	}
}

func TestRespondTextRendersPostedProviderResolve(t *testing.T) {
	result := testThreadRespondResult(ledger.OutcomeComment)
	result.PlannedActions[1].Status = ledger.PlannedActionPosted
	responder := &fakeResponder{result: result}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(responder))

	if err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29"}); err != nil {
		t.Fatalf("Execute respond: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "provider resolved 1 (resolve planned 1, failed 0)") {
		t.Fatalf("stdout = %q, want posted provider resolve count", text)
	}
}

func TestRespondRendersFailedProviderResolveSeparately(t *testing.T) {
	result := testThreadRespondResult(ledger.OutcomeComment)
	result.PlannedActions[0].Status = ledger.PlannedActionPosted
	result.PlannedActions[1].Status = ledger.PlannedActionFailedTerminal
	result.Outbox = outbox.Result{Outcome: ledger.OutcomeComment, ExitCode: 1, Posted: 1, FailedTerminal: 1}
	result.ExitCode = 1
	result.Message = "resolveReviewThread permission denied"
	responder := &fakeResponder{result: result}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(responder))

	err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29"})
	if err == nil {
		t.Fatal("Execute respond error = nil, want non-zero result error")
	}
	text := out.String()
	if !strings.Contains(text, "provider resolved 0 (resolve planned 1, failed 1)") ||
		!strings.Contains(text, "Outbox: posted 1, pending 0, failed 1") ||
		!strings.Contains(text, "Message: resolveReviewThread permission denied") {
		t.Fatalf("stdout = %q, want provider resolve failure called out", text)
	}
}

func TestRespondRejectsRetryPostsWithDryRun(t *testing.T) {
	responder := &fakeResponder{result: testThreadRespondResult(ledger.OutcomeComment)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(responder))

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--retry-posts", "run-123",
		"--dry-run",
	})
	if err == nil || !strings.Contains(err.Error(), "--retry-posts cannot be used") {
		t.Fatalf("Execute error = %v, want retry/dry-run usage", err)
	}
	if len(responder.requests) != 0 {
		t.Fatalf("respond calls = %d, want none", len(responder.requests))
	}
}

type fakeResponder struct {
	result   threadrespond.Result
	err      error
	requests []threadrespond.Request
}

func (r *fakeResponder) Respond(_ context.Context, req threadrespond.Request) (threadrespond.Result, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return threadrespond.Result{}, r.err
	}
	return r.result, nil
}

func fakeFactory(responder *fakeResponder) RuntimeFactory {
	return func(context.Context, reviewruntime.OpenRequest) (reviewruntime.Runtime, error) {
		return reviewruntime.Runtime{
			Responder:       responder,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		}, nil
	}
}

func newTestCommand(t *testing.T, cfg config.File, factory RuntimeFactory) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	path := t.TempDir() + "/config.yml"
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Quiet:      true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out
}

func testConfig() config.File {
	return config.File{
		Keyring: config.KeyringConfig{Backend: "memory"},
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"test-memory": {
					DisplayName: "Test Memory Store",
					Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)},
				},
			},
		},
		RepositoryProfiles: []config.RepositoryProfile{{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		}},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory"},
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{
					MajorEvent: config.ReviewMajorEventRequestChanges,
				},
			},
		},
	}
}

func testThreadRespondResult(outcome ledger.Outcome) threadrespond.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	return threadrespond.Result{
		Run: ledger.Run{
			RunID:           "respond-run-1",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			PostMode:        ledger.PostModeLive,
			PostingIdentity: "review-bot",
			SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ArtifactPath:    "/tmp/respond-run-1",
			Outcome:         &outcome,
		},
		PR: gitprovider.PR{
			Ref:   ref,
			Title: "CR respond",
			URL:   "https://github.com/open-cli-collective/codereview-cli/pull/29",
		},
		PRKey: "github.com_open-cli-collective_codereview-cli_29",
		EligibleThreads: []threadcontext.Thread{{
			ID: "thread-1",
			Status: threadcontext.Status{
				PendingHumanReply: true,
			},
		}},
		Analyses: []threadanalysis.Result{{
			ThreadID: "thread-1",
			Decision: threadanalysis.DecisionAcknowledge,
			Resolve:  true,
		}},
		Plan: reviewplan.Plan{
			Actions: []reviewplan.Action{
				{ActionID: "thread_reply-1", Kind: reviewplan.ActionKindThreadReply, ThreadID: "thread-1", ThreadReply: &reviewplan.ThreadReplyPayload{Body: "Ack"}},
				{ActionID: "resolve_thread-1", Kind: reviewplan.ActionKindResolveThread, ThreadID: "thread-1", ResolveThread: &reviewplan.ResolveThreadPayload{}},
			},
		},
		PlannedActions: []ledger.PlannedAction{
			{ActionID: "thread_reply-1", RunID: "respond-run-1", Kind: ledger.PlannedActionThreadReply, Status: ledger.PlannedActionPending, Required: true},
			{ActionID: "resolve_thread-1", RunID: "respond-run-1", Kind: ledger.PlannedActionResolveThread, Status: ledger.PlannedActionPending, Required: true},
		},
		Outbox:   outbox.Result{Outcome: outcome, ExitCode: 0, Posted: 2},
		ExitCode: 0,
	}
}

var _ reviewruntime.ResponseRunner = (*fakeResponder)(nil)
