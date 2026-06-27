package respondcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/threadanalysis"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
)

func TestRespondDryRunCallsResponderAndRendersText(t *testing.T) {
	responder := &fakeResponder{result: testThreadRespondResult(ledger.OutcomeDryRun)}
	var cleanupCalled bool
	cmd, out := newTestCommand(t, testConfig(), func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ cmdruntime.Options) (cmdruntime.Runtime, error) {
		return cmdruntime.Runtime{
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
	text := out.String()
	if !strings.Contains(text, "Threads: considered 1, responded 1, resolved 1") || !strings.Contains(text, "Planned actions: 2") {
		t.Fatalf("stdout = %q, want respond summary", text)
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
	if text := out.String(); !strings.Contains(text, "responded 1, resolved 1") || !strings.Contains(text, "Planned actions: 2") {
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
		decoded.Counts.Resolved != 1 ||
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

func fakeFactory(responder *fakeResponder) cmdruntime.Factory {
	return func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ cmdruntime.Options) (cmdruntime.Runtime, error) {
		return cmdruntime.Runtime{
			Responder:       responder,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		}, nil
	}
}

func newTestCommand(t *testing.T, cfg config.File, factory cmdruntime.Factory) (*cobra.Command, *bytes.Buffer) {
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

var _ cmdruntime.ResponseRunner = (*fakeResponder)(nil)
