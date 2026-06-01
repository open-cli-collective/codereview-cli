package reviewcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestReviewDryRunCallsRunnerAndRendersText(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	var gotRuntime RuntimeOptions
	var cleanupCalled bool
	cmd, out := newTestCommand(t, testConfig(), func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, opts RuntimeOptions) (Runtime, error) {
		gotRuntime = opts
		return Runtime{
			Runner:          runner,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
			Cleanup:         func() { cleanupCalled = true },
		}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--agents-dir", "/tmp/agents",
		"--fail-on", "minor",
		"--max-agents", "3",
		"--max-concurrency", "2",
		"--allow-self-review",
		"--allow-self-approve",
		"--no-resolve-threads",
		"--verbose",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.PRRef.Number != 29 || req.ProfileName != "home" || req.PostingIdentity.Login != "review-bot" {
		t.Fatalf("request identity/ref = %#v", req)
	}
	if req.FailOn == nil || *req.FailOn != review.SeverityMinor {
		t.Fatalf("FailOn = %#v, want minor", req.FailOn)
	}
	if len(req.AgentDirs) != 1 || req.AgentDirs[0] != "/tmp/agents" || !req.AllowSelfReview || !req.AllowSelfApprove || !req.NoResolveThreads || !req.MajorRequestChanges || !req.IncludeNits {
		t.Fatalf("request flags = %#v", req)
	}
	if gotRuntime.MaxAgents != 3 || gotRuntime.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = %#v, want max agents/concurrency", gotRuntime)
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	if text := out.String(); !strings.Contains(text, "Post mode: dry_run") || !strings.Contains(text, "Planned actions:") {
		t.Fatalf("stdout = %q, want dry-run render", text)
	}
}

func TestReviewNoPostIsDryRunAlias(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--no-post"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
}

func TestReviewProfileResolveThreadsNeverDisablesThreadResolution(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.ReviewPolicy.ResolveThreads = config.ResolveThreadsNever
	cfg.Profiles["home"] = profile
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || !runner.requests[0].NoResolveThreads {
		t.Fatalf("request NoResolveThreads = %#v, want true from profile", runner.requests)
	}
}

func TestReviewLiveCallsRunnerAndRendersText(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if len(runner.requests) != 0 {
		t.Fatalf("dry runner calls = %d, want 0", len(runner.requests))
	}
	if !runner.liveFlags[0].Rerun || runner.liveFlags[0].RetryPosts {
		t.Fatalf("live flags = %#v, want rerun only", runner.liveFlags[0])
	}
}

func TestReviewLiveRetryPostsCallsRunner(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if runner.liveFlags[0].Rerun || !runner.liveFlags[0].RetryPosts {
		t.Fatalf("live flags = %#v, want retry-posts only", runner.liveFlags[0])
	}
}

func TestReviewRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing pr", args: []string{"review", "--dry-run"}},
		{name: "bad url", args: []string{"review", "not-a-url", "--dry-run"}},
		{name: "wrong host", args: []string{"review", "https://gitlab.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}},
		{name: "bad fail on", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fail-on", "urgent"}},
		{name: "negative agents", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-agents", "-1"}},
		{name: "negative concurrency", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-concurrency", "-1"}},
		{name: "rerun retry conflict", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun", "--retry-posts"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{result: testPipelineResult(false)}
			cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))
			err := root.Execute(cmd, tt.args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage failure")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if len(runner.requests) != 0 {
				t.Fatalf("runner calls = %d, want 0", len(runner.requests))
			}
		})
	}
}

func TestReviewDryRunJSONHasNoTextQuotaPrefix(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "Quota:") {
		t.Fatalf("JSON output contains text quota prefix: %s", out.String())
	}
	var decoded view.ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Run.RunID != "run-1" || decoded.Quota == nil || len(decoded.Actions) != 1 {
		t.Fatalf("decoded = %#v", decoded)
	}
	if decoded.FailOnTriggered || decoded.Artifacts.FindingsJSON != "/tmp/run-1/findings.json" || decoded.Artifacts.RollupMarkdown != "/tmp/run-1/rollup.md" {
		t.Fatalf("decoded artifacts/fail-on = %#v", decoded)
	}
}

func TestReviewFailOnReturnsFailureAfterRendering(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(true)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fail-on", "major"})
	if err == nil {
		t.Fatal("Execute error = nil, want fail-on failure")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure", got)
	}
	if !strings.Contains(out.String(), "Automated PR Review") {
		t.Fatalf("stdout = %q, want rendered review before fail-on error", out.String())
	}
}

func TestReviewLiveFailOnReturnsFailureAfterRendering(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(true)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--fail-on", "major"})
	if err == nil {
		t.Fatal("Execute error = nil, want fail-on failure")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure", got)
	}
	if !strings.Contains(out.String(), "Status: continue") {
		t.Fatalf("stdout = %q, want live render before fail-on error", out.String())
	}
	if !strings.Contains(out.String(), "Fail-on: triggered") {
		t.Fatalf("stdout = %q, want live fail-on signal", out.String())
	}
}

func TestReviewLiveOutboxExitReturnsAfterRendering(t *testing.T) {
	live := testLiveResult(false)
	live.ExitCode = exitcode.UpstreamError
	live.Outbox.ExitCode = exitcode.UpstreamError
	live.Message = "review premises moved"
	runner := &fakeRunner{liveResult: live}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"})
	if err == nil {
		t.Fatal("Execute error = nil, want upstream failure")
	}
	if got := exitcode.FromError(err); got != exitcode.UpstreamError {
		t.Fatalf("exit code = %d, want upstream", got)
	}
	if !strings.Contains(out.String(), "Message: review premises moved") {
		t.Fatalf("stdout = %q, want live render before exit error", out.String())
	}
}

func TestReviewLiveNonUpstreamExitCodesReturnAfterRendering(t *testing.T) {
	tests := []struct {
		name string
		code int
	}{
		{name: "failure", code: exitcode.Failure},
		{name: "auth", code: exitcode.AuthConfigError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := testLiveResult(false)
			live.ExitCode = tt.code
			live.Outbox.ExitCode = tt.code
			runner := &fakeRunner{liveResult: live}
			cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

			err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"})
			if err == nil {
				t.Fatal("Execute error = nil, want live exit failure")
			}
			if got := exitcode.FromError(err); got != tt.code {
				t.Fatalf("exit code = %d, want %d", got, tt.code)
			}
			if !strings.Contains(out.String(), "Exit code: "+strconv.Itoa(tt.code)) {
				t.Fatalf("stdout = %q, want rendered exit code %d", out.String(), tt.code)
			}
		})
	}
}

func TestNewReviewDryRunRejectsInvalidPlannedPayload(t *testing.T) {
	result := testPipelineResult(false)
	result.PlannedActions[0].PayloadJSON = "{bad"

	_, err := newReviewDryRun(result)
	if err == nil {
		t.Fatal("newReviewDryRun error = nil, want invalid payload failure")
	}
	if !strings.Contains(err.Error(), "payload is invalid JSON") {
		t.Fatalf("newReviewDryRun error = %v, want payload JSON failure", err)
	}
}

func TestReviewMapsRunnerError(t *testing.T) {
	runner := &fakeRunner{err: gitprovider.ErrRetryable}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want runner error")
	}
	if got := exitcode.FromError(err); got != exitcode.UpstreamError {
		t.Fatalf("exit code = %d, want upstream", got)
	}
}

type fakeRunner struct {
	result       pipeline.Result
	err          error
	requests     []pipeline.Request
	liveResult   reviewrun.Result
	liveErr      error
	liveRequests []pipeline.Request
	liveFlags    []reviewrun.Flags
}

func (r *fakeRunner) DryRun(_ context.Context, req pipeline.Request) (pipeline.Result, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return pipeline.Result{}, r.err
	}
	return r.result, nil
}

func (r *fakeRunner) Live(_ context.Context, req pipeline.Request, flags reviewrun.Flags) (reviewrun.Result, error) {
	r.liveRequests = append(r.liveRequests, req)
	r.liveFlags = append(r.liveFlags, flags)
	if r.liveErr != nil {
		return reviewrun.Result{}, r.liveErr
	}
	return r.liveResult, nil
}

func fakeFactory(runner *fakeRunner) RuntimeFactory {
	return func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}
}

func newTestCommand(t *testing.T, cfg config.File, factory RuntimeFactory) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out
}

func testConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles: map[string]config.Profile{
			"home": {
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
				ReviewPolicy: config.ReviewPolicy{
					MajorEvent: config.ReviewMajorEventRequestChanges,
				},
			},
		},
	}
}

func testPipelineResult(failOnTriggered bool) pipeline.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	side := review.DiffSideRight
	line := 2
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return pipeline.Result{
		Run: ledger.Run{
			RunID:           "run-1",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			PostMode:        ledger.PostModeDryRun,
			PostingIdentity: "review-bot",
			ArtifactPath:    "/tmp/run-1",
		},
		PR: gitprovider.PR{
			Ref:   ref,
			Title: "CR-20",
			URL:   "https://github.com/open-cli-collective/codereview-cli/pull/29",
		},
		PRKey: "github.com_open-cli-collective_codereview-cli_29",
		Artifacts: pipeline.ArtifactPaths{
			Dir:            "/tmp/run-1",
			DiffPatch:      "/tmp/run-1/diff.patch",
			SlicesDir:      "/tmp/run-1/slices",
			FindingsJSON:   "/tmp/run-1/findings.json",
			RollupMarkdown: "/tmp/run-1/rollup.md",
			AgentLogsDir:   "/tmp/run-1/agent-logs",
		},
		QuotaSupported:  true,
		Quota:           llm.Quota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		FailOnTriggered: failOnTriggered,
		Plan: reviewplan.Plan{
			RollupMarkdown: "## Automated PR Review\n\nBody.",
			AnchoredFindings: []reviewplan.AnchoredFinding{{
				FindingID: "finding-1",
				Severity:  review.SeverityMajor,
				FilePath:  "main.go",
				Anchoring: review.AnchoringInline,
				Side:      &side,
				Line:      &line,
				Body:      "Fix this",
			}},
			Actions: []reviewplan.Action{{
				ActionID:  "inline_comment-1",
				Kind:      reviewplan.ActionKindInlineComment,
				FindingID: "finding-1",
				PlannedAt: now,
				Status:    reviewplan.ActionStatusPlannedOnly,
				Marker:    reviewplan.MarkerPlacement{BodyBearing: true},
				InlineComment: &reviewplan.InlineCommentPayload{
					Body:        "Fix this",
					Path:        "main.go",
					Side:        review.DiffSideRight,
					Line:        2,
					SubjectType: review.AnchorKindLine,
				},
			}},
		},
		PlannedActions: []ledger.PlannedAction{{
			ActionID:    "inline_comment-1",
			RunID:       "run-1",
			Kind:        ledger.PlannedActionInlineComment,
			FindingID:   stringPtr("finding-1"),
			PlannedAt:   now,
			PayloadJSON: `{"body":"Fix this","path":"main.go"}`,
			Status:      ledger.PlannedActionPlannedOnly,
		}},
	}
}

func testLiveResult(failOnTriggered bool) reviewrun.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	return reviewrun.Result{
		Status: gateio.StatusContinue,
		Run: ledger.Run{
			RunID:           "run-live",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			PostMode:        ledger.PostModeLive,
			PostingIdentity: "review-bot",
			ArtifactPath:    "/tmp/run-live",
		},
		PR: gitprovider.PR{
			Ref:   ref,
			Title: "CR-21",
			URL:   "https://github.com/open-cli-collective/codereview-cli/pull/29",
		},
		PRKey:           "github.com_open-cli-collective_codereview-cli_29",
		Outbox:          outbox.Result{Outcome: ledger.OutcomeComment, ExitCode: 0, Posted: 2},
		FailOnTriggered: failOnTriggered,
	}
}

func stringPtr(value string) *string {
	return &value
}

func TestFakeFactoryErrorIsReturned(t *testing.T) {
	factoryErr := errors.New("factory failed")
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		return Runtime{}, factoryErr
	})
	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Execute error = %v, want factory error", err)
	}
}

func TestNewAdapterRejectsCodexCLIBestEffortByDefault(t *testing.T) {
	_, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}, nil)
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("newAdapter error = %v, want config.ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "no-tools mode is explicit") {
		t.Fatalf("newAdapter error = %v, want explicit no-tools explanation", err)
	}
}
