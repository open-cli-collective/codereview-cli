package reviewcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
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
	if req.SelectionModelOverride != "" || req.SelectionEffortOverride != "" ||
		req.SelectionPromptInstructions != "" ||
		req.ReviewerModelOverride != "" || req.ReviewerEffortOverride != "" {
		t.Fatalf("stage overrides = %#v, want empty when flags omitted", req)
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

func TestRespondDryRunCallsResponderAndRendersText(t *testing.T) {
	runner := &fakeRunner{respondResult: testThreadRespondResult(ledger.OutcomeDryRun)}
	var cleanupCalled bool
	cmd, out := newTestCommand(t, testConfig(), func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ RuntimeOptions) (Runtime, error) {
		return Runtime{
			Runner:          runner,
			Responder:       runner,
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
	if len(runner.respondRequests) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(runner.respondRequests))
	}
	req := runner.respondRequests[0]
	if req.PRRef.Number != 29 || req.ProfileName != "home" || req.PostingIdentity.Login != "review-bot" {
		t.Fatalf("respond request identity/ref = %#v", req)
	}
	if !req.DryRun || !req.NoResolveThreads || req.Rerun {
		t.Fatalf("respond request flags = %#v, want dry-run no-resolve", req)
	}
	if req.RetryRunID != "" {
		t.Fatalf("RetryRunID = %q, want empty", req.RetryRunID)
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	text := out.String()
	if !strings.Contains(text, "Threads: considered 1, responded 1, resolved 1") || !strings.Contains(text, "Planned actions: 2") {
		t.Fatalf("stdout = %q, want respond summary", text)
	}
}

func TestRespondProfileResolveThreadsNeverDisablesThreadResolution(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.ReviewPolicy.ResolveThreads = config.ResolveThreadsNever
	cfg.Profiles["home"] = profile
	runner := &fakeRunner{respondResult: testThreadRespondResult(ledger.OutcomeDryRun)}
	cmd, _ := newTestCommand(t, cfg, fakeFactory(runner))

	if err := root.Execute(cmd, []string{"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.respondRequests) != 1 || !runner.respondRequests[0].NoResolveThreads {
		t.Fatalf("respond request NoResolveThreads = %#v, want true from profile", runner.respondRequests)
	}
}

func TestRespondRerunFlagCallsResponder(t *testing.T) {
	runner := &fakeRunner{respondResult: testThreadRespondResult(ledger.OutcomeDryRun)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--rerun",
	})
	if err != nil {
		t.Fatalf("Execute respond rerun: %v", err)
	}
	if len(runner.respondRequests) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(runner.respondRequests))
	}
	if !runner.respondRequests[0].Rerun || !runner.respondRequests[0].DryRun {
		t.Fatalf("respond request = %#v, want rerun dry-run", runner.respondRequests[0])
	}
}

func TestRespondRetryPostsFlagCallsResponder(t *testing.T) {
	result := testThreadRespondResult(ledger.OutcomeComment)
	result.Plan = reviewplan.Plan{}
	runner := &fakeRunner{respondResult: result}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--retry-posts", "run-123",
	})
	if err != nil {
		t.Fatalf("Execute respond retry: %v", err)
	}
	if len(runner.respondRequests) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(runner.respondRequests))
	}
	if got := runner.respondRequests[0].RetryRunID; got != "run-123" {
		t.Fatalf("RetryRunID = %q, want run-123", got)
	}
	if runner.respondRequests[0].DryRun {
		t.Fatalf("respond retry request = %#v, want live retry", runner.respondRequests[0])
	}
	if text := out.String(); !strings.Contains(text, "responded 1, resolved 1") || !strings.Contains(text, "Planned actions: 2") {
		t.Fatalf("stdout = %q, want retry counts from planned actions", text)
	}
}

func TestRespondRealRunnerResumesInterruptedRunThroughCLI(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	provider.SetCapabilities(gitprovider.ProviderCaps{ThreadResolution: true})
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetInlineThreads(ref, []gitprovider.InlineThread{
		reviewCommandMarkedThread(t, pr, "thread-1", "main.go", 2),
	}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	store, err := ledger.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(reviewCommandThreadAnalysisResult("thread-1", "thread-session-1"))

	factory := func(limiter outbox.Limiter) RuntimeFactory {
		return func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
			runner := buildReviewRunner(
				store,
				provider,
				adapter,
				profile,
				limiter,
				layout,
				opts.Stderr,
				nil,
				runtimeOptsWithWorkbench(t, runtimeOpts),
			)
			return Runtime{Responder: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
		}
	}

	firstCmd, _ := newTestCommand(t, cfg, factory(cancelLimiter{}))
	firstErr := root.Execute(firstCmd, []string{"respond", pr.URL})
	if !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("first Execute error = %v, want context.Canceled", firstErr)
	}
	if len(adapter.Requests()) != 1 {
		t.Fatalf("first adapter requests = %d, want one thread-analysis call", len(adapter.Requests()))
	}
	runs, err := store.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns first: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != nil {
		t.Fatalf("first runs = %#v, want one incomplete response run", runs)
	}
	firstRunID := runs[0].RunID
	firstArtifactPath := runs[0].ArtifactPath
	actions, err := store.ListPlannedActions(ctx, runs[0].RunID)
	if err != nil {
		t.Fatalf("ListPlannedActions first: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("first planned actions = %d, want reply and resolve", len(actions))
	}

	secondCmd, out := newTestCommand(t, cfg, factory(noopLimiter{}))
	if err := root.Execute(secondCmd, []string{"respond", pr.URL}); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if len(adapter.Requests()) != 1 {
		t.Fatalf("second adapter requests = %d, want persisted analysis reused", len(adapter.Requests()))
	}
	runs, err = store.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns second: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID == "" || runs[0].Outcome == nil || *runs[0].Outcome != ledger.OutcomeComment {
		t.Fatalf("second runs = %#v, want same run completed as comment", runs)
	}
	if runs[0].RunID != firstRunID || runs[0].ArtifactPath != firstArtifactPath {
		t.Fatalf("second run = %q artifact %q, want resumed %q artifact %q", runs[0].RunID, runs[0].ArtifactPath, firstRunID, firstArtifactPath)
	}
	if replies := provider.RecordedThreadReplies(ref); len(replies) != 1 || replies[0].ThreadID != "thread-1" {
		t.Fatalf("thread replies = %#v, want resumed reply post", replies)
	}
	if resolved := provider.RecordedResolvedThreads(ref); len(resolved) != 1 || resolved[0] != "thread-1" {
		t.Fatalf("resolved threads = %#v, want resumed resolve post", resolved)
	}
	if text := out.String(); !strings.Contains(text, "responded 1, resolved 1") {
		t.Fatalf("stdout = %q, want resumed response summary", text)
	}
}

func TestRespondJSONRendersStableShape(t *testing.T) {
	runner := &fakeRunner{respondResult: testThreadRespondResult(ledger.OutcomeComment)}
	cmd, out := newTestCommand(t, testConfig(), fakeFactory(runner))

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
		Counts respondCounts `json:"counts"`
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
	runner := &fakeRunner{respondResult: testThreadRespondResult(ledger.OutcomeComment)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"respond", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--retry-posts", "run-123",
		"--dry-run",
	})
	if err == nil || !strings.Contains(err.Error(), "--retry-posts cannot be used") {
		t.Fatalf("Execute error = %v, want retry/dry-run usage", err)
	}
	if len(runner.respondRequests) != 0 {
		t.Fatalf("respond calls = %d, want none", len(runner.respondRequests))
	}
}

func TestReviewUsesRepositoryProfileRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar", "baz"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		if profile.Git.CredentialRef != "codereview/work" {
			t.Fatalf("runtime profile credential ref = %q, want work route", profile.Git.CredentialRef)
		}
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", "https://github.com/rianjs/bar/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].ProfileName != "work" {
		t.Fatalf("request profile = %#v, want work", runner.requests)
	}
}

func TestReviewExplicitProfileBypassesRepositoryRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, profile config.Profile, _ RuntimeOptions) (Runtime, error) {
		if profile.Git.CredentialRef != "codereview/home" {
			t.Fatalf("runtime profile credential ref = %q, want explicit home", profile.Git.CredentialRef)
		}
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"--profile", "home", "review", "https://github.com/rianjs/bar/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].ProfileName != "home" {
		t.Fatalf("request profile = %#v, want home", runner.requests)
	}
}

func TestReviewExplicitEmptyProfileFailsBeforeRepositoryRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for an empty explicit profile")
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "", "review", "https://github.com/rianjs/bar/pull/29", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("Execute error = %v, want empty profile failure", err)
	}
}

func TestReviewUnmatchedRepositoryRequiresProfileOrRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "rianjs",
			Repos:     []string{"bar"},
		},
	}}
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for an unmatched repository")
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	err := root.Execute(cmd, []string{"review", "https://github.com/example/missing/pull/29", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "no repository profile route matched") {
		t.Fatalf("Execute error = %v, want unmatched route failure", err)
	}
}

func TestReviewExplicitProfileHostMismatch(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.Git.Host = "gitlab.com"
	cfg.Profiles["home"] = home
	cfg.RepositoryProfiles = nil
	work := home
	work.Git.Host = "github.com"
	work.Git.CredentialRef = "codereview/work"
	cfg.Profiles["work"] = work
	cmd, _ := newTestCommand(t, cfg, func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called when route profile host mismatches")
		return Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want host mismatch")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if !strings.Contains(err.Error(), `PR host "github.com" must match configured git host "gitlab.com"`) {
		t.Fatalf("error = %v, want host mismatch detail", err)
	}
}

func TestReviewHelpDocumentsApprovalFastPaths(t *testing.T) {
	cmd, out := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		t.Fatal("runtime factory should not be called for help")
		return Runtime{}, nil
	})

	if err := root.Execute(cmd, []string{"review", "--help"}); err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"already approved the PR",
		"--rerun to bypass this",
		"approval override request newer than that marker",
		"--retry-posts is recovery-only",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help = %q, want substring %q", text, want)
		}
	}
}

func TestRuntimeLayoutMigratesLegacyDataAndCache(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLayout := statepaths.NewLayout(filepath.Join(layout.DataRoot, statepaths.AppDir), layout.CacheRoot)
	legacyStore, err := ledger.Open(context.Background(), legacyLayout.LedgerDB())
	if err != nil {
		t.Fatalf("legacy ledger.Open: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("legacy store Close: %v", err)
	}
	writeReviewFile(t, filepath.Join(layout.DataRoot, statepaths.AppDir, "runs", "sentinel.txt"), "run")
	writeReviewFile(t, filepath.Join(layout.CacheRoot, statepaths.AppDir, "http", "sentinel.txt"), "cache")

	got, err := runtimeLayout()
	if err != nil {
		t.Fatalf("runtimeLayout: %v", err)
	}
	if got != layout {
		t.Fatalf("runtime layout = %#v, want %#v", got, layout)
	}
	if _, err := os.Stat(layout.LedgerDB()); err != nil {
		t.Fatalf("new ledger stat: %v", err)
	}
	assertReviewTestFile(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "run")
	assertReviewTestFile(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy data root stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy cache root stat err = %v, want removed", err)
	}
}

func TestNewRuntimeMigratesLegacyDataAndCache(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLayout := statepaths.NewLayout(filepath.Join(layout.DataRoot, statepaths.AppDir), layout.CacheRoot)
	legacyStore, err := ledger.Open(context.Background(), legacyLayout.LedgerDB())
	if err != nil {
		t.Fatalf("legacy ledger.Open: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("legacy store Close: %v", err)
	}
	writeReviewFile(t, filepath.Join(layout.DataRoot, statepaths.AppDir, "runs", "sentinel.txt"), "run")
	writeReviewFile(t, filepath.Join(layout.CacheRoot, statepaths.AppDir, "http", "sentinel.txt"), "cache")

	provider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, testConfig(), testConfig().Profiles["home"], RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if runtime.Runner == nil || runtime.PostingIdentity != identity {
		t.Fatalf("runtime = %#v, want runner and posting identity", runtime)
	}
	if _, err := os.Stat(layout.LedgerDB()); err != nil {
		t.Fatalf("new ledger stat: %v", err)
	}
	assertReviewTestFile(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "run")
	assertReviewTestFile(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy data root stat err = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, statepaths.AppDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy cache root stat err = %v, want removed", err)
	}
}

func TestNewRuntimeCreatesCodexCLIWithoutOpenAIAPIKey(t *testing.T) {
	statedirtest.Hermetic(t)
	previousAPIKey, hadAPIKey := os.LookupEnv("OPENAI_API_KEY")
	if err := os.Unsetenv("OPENAI_API_KEY"); err != nil {
		t.Fatalf("Unsetenv(OPENAI_API_KEY): %v", err)
	}
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	t.Cleanup(func() {
		if hadAPIKey {
			_ = os.Setenv("OPENAI_API_KEY", previousAPIKey)
			return
		}
		_ = os.Unsetenv("OPENAI_API_KEY")
	})

	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	profile := cfg.Profiles["home"]
	profile.LLM = config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}
	cfg.Profiles["home"] = profile

	provider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		newAdapter,
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.PostingIdentity != identity {
		t.Fatalf("PostingIdentity = %#v, want %#v", runtime.PostingIdentity, identity)
	}
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	if runner.pipeline.Adapter == nil || runner.pipeline.Adapter.Name() != "codex_cli" {
		t.Fatalf("pipeline adapter = %#v, want codex_cli", runner.pipeline.Adapter)
	}
	loadedAdapter, err := runner.pipeline.Adapter.(*lazyAdapter).get()
	if err != nil {
		t.Fatalf("lazy adapter get: %v", err)
	}
	progressAdapter, ok := loadedAdapter.(progressAdapter)
	if !ok || progressAdapter.adapter == nil || progressAdapter.adapter.Name() != "codex_cli" {
		t.Fatalf("loaded pipeline adapter = %#v, want wrapped codex_cli adapter", loadedAdapter)
	}
	if runner.pipeline.TaskProgress == nil {
		t.Fatal("pipeline TaskProgress = nil, want review progress wiring")
	}
}

func TestNewRuntimeUsesReviewerCredentialsAsRuntimeProvider(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	profile := cfg.Profiles["work"]
	profile.ReviewerCredentials = &config.ReviewerCredentials{
		AuthMode:      config.GitAuthModePAT,
		Credential:    config.CredentialLocation{Store: "test-memory"},
		CredentialRef: "codereview/work-reviewer",
	}
	cfg.Profiles["work"] = profile

	var providerCalls []config.GitConfig
	reviewerProvider := &gitprovider.Fake{}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(git config.GitConfig, _ githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			providerCalls = append(providerCalls, git)
			return reviewerProvider, gitprovider.Credential{Type: "pat", Token: "reviewer-token"}, nil
		},
		func(_ context.Context, provider gitprovider.GitProvider, credential gitprovider.Credential, _ githubprovider.TokenStore, _ config.Profile) (gitprovider.Identity, error) {
			wrapped, ok := provider.(progressProvider)
			if !ok || wrapped.provider != reviewerProvider || credential.Token != "reviewer-token" {
				t.Fatalf("identity resolver got provider=%T %#v credential=%#v, want wrapped reviewer provider/token", provider, provider, credential)
			}
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if len(providerCalls) != 1 || providerCalls[0].CredentialRef != "codereview/work-reviewer" {
		t.Fatalf("provider calls = %#v, want reviewer credential ref only", providerCalls)
	}
	runner, ok := runtime.Runner.(reviewRunner)
	if !ok {
		t.Fatalf("Runner type = %T, want reviewRunner", runtime.Runner)
	}
	pipelineProvider, ok := runner.pipeline.Provider.(progressProvider)
	if !ok || pipelineProvider.provider != reviewerProvider {
		t.Fatalf("pipeline provider = %#v, want wrapped reviewer provider", runner.pipeline.Provider)
	}
	liveProvider, ok := runner.live.Provider.(progressProvider)
	if !ok || liveProvider.provider != reviewerProvider {
		t.Fatalf("live provider = %#v, want wrapped reviewer provider", runner.live.Provider)
	}
}

func TestNewRuntimePassesPRRefForGitHubAppInstallationLookup(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	profile := cfg.Profiles["home"]
	profile.Git.AuthMode = config.GitAuthModeGitHubApp
	cfg.Profiles["home"] = profile
	prRef := gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 76}

	var gotLookup *githubprovider.InstallationLookup
	var gotInstallationID string
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, _ githubprovider.TokenStore, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			gotLookup = opts.InstallationLookup
			gotInstallationID = opts.InstallationID
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token", Login: "cr-reviewer[bot]"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{Login: "cr-reviewer[bot]", ID: "12345"}, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{PRRef: prRef})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if gotLookup == nil || gotLookup.Owner != "open-cli" || gotLookup.Repo != "codereview-cli" {
		t.Fatalf("InstallationLookup = %#v, want PR owner/repo", gotLookup)
	}
	if gotInstallationID != "" {
		t.Fatalf("InstallationID = %q, want empty for repository lookup", gotInstallationID)
	}
}

func TestNewRuntimePassesPinnedGitHubAppReviewerInstallationID(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	cfg.Keyring.Backend = "memory"
	cfg.ReviewerEntities = map[string]config.ReviewerEntity{
		"cr-reviewer": {
			Host:     "github.com",
			AuthMode: config.GitAuthModeGitHubApp,
			Credential: config.CredentialLocation{
				Store: "test-memory",
				Name:  "codereview/cr-reviewer",
			},
		},
	}
	profile := cfg.Profiles["home"]
	profile.Reviewer = config.ProfileReviewer{
		Kind:   config.ProfileReviewerKindEntity,
		Entity: "cr-reviewer",
		GitHubAppInstallation: &config.ProfileReviewerGitHubAppInstallation{
			Mode:           config.ProfileReviewerGitHubAppInstallationPinned,
			InstallationID: "42",
		},
	}
	cfg.Profiles["home"] = profile
	cfg = config.Normalize(cfg)
	profile = cfg.Profiles["home"]
	prRef := gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 76}

	var gotLookup *githubprovider.InstallationLookup
	var gotInstallationID string
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, _ githubprovider.TokenStore, opts githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			gotLookup = opts.InstallationLookup
			gotInstallationID = opts.InstallationID
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "github_app", Token: "installation-token", Login: "cr-reviewer[bot]"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{Login: "cr-reviewer[bot]", ID: "12345"}, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{PRRef: prRef})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
	if gotInstallationID != "42" {
		t.Fatalf("InstallationID = %q, want pinned id", gotInstallationID)
	}
	if gotLookup != nil {
		t.Fatalf("InstallationLookup = %#v, want nil when pinned", gotLookup)
	}
}

func TestNewRuntimeRejectsBackendOverrideForNamedSecretsProfile(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	profile := cfg.Profiles["home"]
	cmd := &cobra.Command{}
	cmd.Flags().String(credstore.BackendFlagName, "", "")
	if err := cmd.Flags().Set(credstore.BackendFlagName, "memory"); err != nil {
		t.Fatalf("Set backend flag: %v", err)
	}
	opts := &root.Options{Backend: "memory", Stderr: io.Discard}

	_, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("newRuntime error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestNewRuntimeUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if err := store.Set("home", credentials.GitTokenKey, "named-store-token", credstore.WithOverwrite()); err != nil {
		t.Fatalf("Set(home, git_token): %v", err)
	}

	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	profile := cfg.Profiles["home"]
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, tokenStore githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			token, err := tokenStore.Get("home", credentials.GitTokenKey)
			if err != nil {
				t.Fatalf("tokenStore.Get(home, git_token): %v", err)
			}
			if token != "named-store-token" {
				t.Fatalf("token = %q, want named-store-token", token)
			}
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runtime, err := newRuntime(cmd, &root.Options{Stderr: io.Discard}, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
}

func TestOpenSelectionRuntimeRejectsBackendOverrideForNamedSecretsProfile(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home

	_, err := OpenSelectionRuntime(context.Background(), "memory", true, cfg, cfg.Profiles["home"])
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("OpenSelectionRuntime error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestOpenSelectionRuntimeUsesNamedSecretsProfileStoreWithoutBackendOverride(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if err := store.Set("home", credentials.GitTokenKey, "named-store-token", credstore.WithOverwrite()); err != nil {
		t.Fatalf("Set(home, git_token): %v", err)
	}

	cfg := testConfig()
	cfg.Secrets = config.SecretsConfig{
		Stores: map[string]config.SecretsStore{
			"work-file": {
				DisplayName: "Work File Store",
				Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
			},
		},
	}
	home := cfg.Profiles["home"]
	home.Git.Credential.Store = "work-file"
	cfg.Profiles["home"] = home
	withReviewRuntimeSeams(t,
		func(_ config.GitConfig, tokenStore githubprovider.TokenStore, _ githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			token, err := tokenStore.Get("home", credentials.GitTokenKey)
			if err != nil {
				t.Fatalf("tokenStore.Get(home, git_token): %v", err)
			}
			if token != "named-store-token" {
				t.Fatalf("token = %q, want named-store-token", token)
			}
			return &gitprovider.Fake{}, gitprovider.Credential{Type: "pat", Token: token}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return gitprovider.Identity{}, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			return &llm.FakeAdapter{NameValue: "fake-llm"}, nil
		},
	)

	runtime, err := OpenSelectionRuntime(context.Background(), "", false, cfg, cfg.Profiles["home"])
	if err != nil {
		t.Fatalf("OpenSelectionRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		runtime.Cleanup()
	}
}

func TestNewRuntimeLiveApprovedFastPathDoesNotInitializeAdapter(t *testing.T) {
	statedirtest.Hermetic(t)
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	identity := gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
	if err := provider.SetReviews(ref, []gitprovider.Review{{
		ID:          "review-approved",
		Author:      identity,
		State:       gitprovider.ReviewStateApproved,
		SubmittedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	adapterCalls := 0
	withReviewRuntimeSeams(t,
		func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error) {
			return provider, gitprovider.Credential{Type: "pat", Token: "token"}, nil
		},
		func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error) {
			return identity, nil
		},
		func(config.LLMConfig, *credstore.Store) (llm.Adapter, error) {
			adapterCalls++
			return nil, errors.New("adapter should not initialize for approved fast path")
		},
	)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	opts := &root.Options{Stderr: io.Discard}
	runtime, err := newRuntime(cmd, opts, cfg, profile, RuntimeOptions{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after newRuntime = %d, want 0", adapterCalls)
	}

	result, err := runtime.Runner.Live(context.Background(), pipeline.Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         profile,
		PostingIdentity: identity,
	}, reviewrun.Flags{})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.Status != gateio.StatusEarlyExit || result.Message != "review already approved" {
		t.Fatalf("Live result = %#v, want approved early exit", result)
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls after approved fast path = %d, want 0", adapterCalls)
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

func TestReviewDryRunRerunFlagCallsDryRunner(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--rerun"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("dry runner calls = %d, want 1", len(runner.requests))
	}
	if len(runner.liveRequests) != 0 {
		t.Fatalf("live runner calls = %d, want 0", len(runner.liveRequests))
	}
	if !runner.requests[0].Rerun {
		t.Fatalf("dry-run request = %#v, want rerun propagated", runner.requests[0])
	}
}

func TestReviewDryRunPassesStageOverrides(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))
	promptPath := filepath.Join(t.TempDir(), "selection.md")
	writeReviewFile(t, promptPath, "Use applies_when as the routing contract.")

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--selection-model", " bench-selection-model ",
		"--selection-effort", " high ",
		"--selection-prompt", promptPath,
		"--reviewer-model", " bench-reviewer-model ",
		"--reviewer-effort", " low ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.SelectionModelOverride != "bench-selection-model" || req.SelectionEffortOverride != "high" {
		t.Fatalf("selection overrides = model:%q effort:%q, want bench-selection-model/high", req.SelectionModelOverride, req.SelectionEffortOverride)
	}
	if req.ReviewerModelOverride != "bench-reviewer-model" || req.ReviewerEffortOverride != "low" {
		t.Fatalf("reviewer overrides = model:%q effort:%q, want bench-reviewer-model/low", req.ReviewerModelOverride, req.ReviewerEffortOverride)
	}
	if req.SelectionPromptInstructions != "Use applies_when as the routing contract." {
		t.Fatalf("selection prompt override instructions = %q", req.SelectionPromptInstructions)
	}
}

func TestReviewDryRunPassesReviewerModelTierOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--reviewer-model-tier", " medium ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	if got := runner.requests[0].ReviewerModelTierOverride; got != "medium" {
		t.Fatalf("reviewer model tier override = %q, want medium", got)
	}
}

func TestReviewNoPostPassesReviewerEffortOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--no-post",
		"--reviewer-effort", "medium",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.SelectionModelOverride != "" || req.SelectionEffortOverride != "" || req.ReviewerModelOverride != "" || req.ReviewerModelTierOverride != "" || req.ReviewerEffortOverride != "medium" {
		t.Fatalf("stage overrides = %#v, want reviewer effort only", req)
	}
}

func TestReviewNoPostPassesReviewerModelTierOverride(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--no-post",
		"--reviewer-model-tier", "large",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.ReviewerModelTierOverride != "large" || req.ReviewerModelOverride != "" {
		t.Fatalf("reviewer overrides = %#v, want reviewer model tier only", req)
	}
}

func TestReviewDryRunPassesReviewSHAOverrides(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--review-base-sha", " 1111111 ",
		"--review-head-sha", " 2222222 ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	req := runner.requests[0]
	if req.ReviewBaseSHA != "1111111" || req.ReviewHeadSHA != "2222222" {
		t.Fatalf("review SHAs = base:%q head:%q, want 1111111/2222222", req.ReviewBaseSHA, req.ReviewHeadSHA)
	}
}

func TestReviewRejectsInvalidReviewSHAOverrides(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "base only", args: []string{"--dry-run", "--review-base-sha", "1111111"}},
		{name: "head only", args: []string{"--dry-run", "--review-head-sha", "2222222"}},
		{name: "blank base", args: []string{"--dry-run", "--review-base-sha", " ", "--review-head-sha", "2222222"}},
		{name: "invalid head", args: []string{"--dry-run", "--review-base-sha", "1111111", "--review-head-sha", "notsha"}},
		{name: "live", args: []string{"--review-base-sha", "1111111", "--review-head-sha", "2222222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid review SHA override")
			}
		})
	}
}

func TestReviewLiveRejectsStageOverridesBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection model", args: []string{"--selection-model", "bench-model"}},
		{name: "selection effort", args: []string{"--selection-effort", "high"}},
		{name: "selection prompt", args: []string{"--selection-prompt", "selection.md"}},
		{name: "reviewer model", args: []string{"--reviewer-model", "bench-model"}},
		{name: "reviewer model tier", args: []string{"--reviewer-model-tier", "medium"}},
		{name: "reviewer effort", args: []string{"--reviewer-effort", "high"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{liveResult: testLiveResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid live stage override")
			}
		})
	}
}

func TestReviewRejectsEmptyStageOverridesBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection model", args: []string{"--dry-run", "--selection-model", " \t "}},
		{name: "selection effort", args: []string{"--dry-run", "--selection-effort", " \t "}},
		{name: "selection prompt", args: []string{"--dry-run", "--selection-prompt", " \t "}},
		{name: "reviewer model", args: []string{"--dry-run", "--reviewer-model", " \t "}},
		{name: "reviewer model tier", args: []string{"--dry-run", "--reviewer-model-tier", " \t "}},
		{name: "reviewer effort", args: []string{"--dry-run", "--reviewer-effort", " \t "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for empty stage override")
			}
		})
	}
}

func TestReviewRejectsInvalidModelEffortBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "selection", args: []string{"--dry-run", "--selection-effort", "xhigh"}},
		{name: "reviewer", args: []string{"--dry-run", "--reviewer-effort", "xhigh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})

			err := root.Execute(cmd, append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, tt.args...))
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid effort")
			}
		})
	}
}

func TestReviewRejectsInvalidReviewerModelTierBeforeRuntimeFactory(t *testing.T) {
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--reviewer-model-tier", "flagship"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for invalid reviewer model tier")
	}
}

func TestReviewRejectsReviewerModelAndReviewerModelTierTogether(t *testing.T) {
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--reviewer-model", "bench-model",
		"--reviewer-model-tier", "medium",
	})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for conflicting reviewer model flags")
	}
}

func TestReviewRejectsRemovedLLMFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--dry-run", "--llm-model", "bench-model"},
		{"--dry-run", "--llm-effort", "high"},
	} {
		cmd, _ := newTestCommand(t, testConfig(), fakeFactory(&fakeRunner{result: testPipelineResult(false)}))
		err := root.Execute(cmd, append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, args...))
		if err == nil {
			t.Fatal("Execute error = nil, want usage error")
		}
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("exit code = %d, want usage", got)
		}
	}
}

func TestReviewRejectsInvalidSelectionPromptFileBeforeRuntimeFactory(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.md")},
		{name: "directory", path: t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var factoryCalled bool
			cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
				factoryCalled = true
				return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
			})
			err := root.Execute(cmd, []string{
				"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
				"--dry-run",
				"--selection-prompt", tt.path,
			})
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if factoryCalled {
				t.Fatal("runtime factory was called for invalid selection prompt path")
			}
		})
	}
}

func TestReviewRejectsEmptySelectionPromptFileBeforeRuntimeFactory(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "selection.md")
	writeReviewFile(t, promptPath, "  \n\t  ")
	var factoryCalled bool
	cmd, _ := newTestCommand(t, testConfig(), func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		factoryCalled = true
		return Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--selection-prompt", promptPath,
	})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if factoryCalled {
		t.Fatal("runtime factory was called for empty selection prompt file")
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

func TestReviewPassesRetentionConfigToRuntimeFactory(t *testing.T) {
	cfg := testConfig()
	maxAgeDays := 0
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionManualOnly,
	}
	runner := &fakeRunner{result: testPipelineResult(false)}
	var got RuntimeOptions
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, opts RuntimeOptions) (Runtime, error) {
		got = opts
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !got.Retention.LiveForever || !got.RetentionManualOnly {
		t.Fatalf("runtime retention = %#v manual %v, want keep-forever manual-only policy", got.Retention, got.RetentionManualOnly)
	}
}

func TestRetentionPolicyFromConfigDefaultsWhenMaxAgeOmitted(t *testing.T) {
	got := retentionPolicyFromConfig(config.RetentionConfig{})
	if got.LiveForever || got.LiveMaxAge != 0 || got.DryRunMaxAge != 0 {
		t.Fatalf("retention policy = %#v, want zero-value default policy", got)
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

func TestReviewLiveSessionPassesNamedSession(t *testing.T) {
	runner := &fakeRunner{liveResult: testLiveResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--session", " daily "}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.liveRequests) != 1 {
		t.Fatalf("live runner calls = %d, want 1", len(runner.liveRequests))
	}
	if runner.liveRequests[0].SessionName != "daily" {
		t.Fatalf("SessionName = %q, want daily", runner.liveRequests[0].SessionName)
	}
}

func TestBuildReviewRunnerWiresNamedSessionDependencies(t *testing.T) {
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	}()
	provider := &gitprovider.Fake{}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	var warnings bytes.Buffer
	retention := datalifecycle.RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour}

	runner := buildReviewRunner(
		store,
		provider,
		adapter,
		testConfig().Profiles["home"],
		noopLimiter{},
		statepaths.NewLayout(t.TempDir(), t.TempDir()),
		&warnings,
		nil,
		runtimeOptsWithWorkbench(t, RuntimeOptions{MaxAgents: 3, MaxConcurrency: 2, Retention: retention, RetentionManualOnly: true}),
	)

	if runner.pipeline.NamedSessions != store {
		t.Fatalf("pipeline NamedSessions = %#v, want ledger store", runner.pipeline.NamedSessions)
	}
	if runner.pipeline.Warnings != &warnings || runner.live.Warnings != &warnings {
		t.Fatalf("warnings not wired through pipeline/live")
	}
	planner, ok := runner.live.Planner.(livePlanner)
	if !ok {
		t.Fatalf("live planner = %T, want livePlanner", runner.live.Planner)
	}
	if planner.opts.NamedSessions != store || planner.opts.Warnings != &warnings {
		t.Fatalf("planner opts did not preserve named-session dependencies: %#v", planner.opts)
	}
	if runner.pipeline.MaxAgents != 3 || runner.pipeline.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = maxAgents:%d maxConcurrency:%d, want 3/2", runner.pipeline.MaxAgents, runner.pipeline.MaxConcurrency)
	}
	if runner.pipeline.Retention != retention || !runner.pipeline.RetentionManualOnly {
		t.Fatalf("pipeline retention = %#v manual %v, want configured manual policy", runner.pipeline.Retention, runner.pipeline.RetentionManualOnly)
	}
	if runner.live.Retention != retention || !runner.live.RetentionManualOnly {
		t.Fatalf("live retention = %#v manual %v, want configured manual policy", runner.live.Retention, runner.live.RetentionManualOnly)
	}
	if planner.opts.Retention != retention || !planner.opts.RetentionManualOnly {
		t.Fatalf("planner retention = %#v manual %v, want configured manual policy", planner.opts.Retention, planner.opts.RetentionManualOnly)
	}
	if warnings.Len() != 0 {
		t.Fatalf("warnings after buildReviewRunner = %q, want none before override classifier invocation", warnings.String())
	}
}

func TestBuildApprovalOverrideClassifierModelResolution(t *testing.T) {
	tests := []struct {
		name         string
		llmConfig    config.LLMConfig
		wantModel    string
		wantWarning  string
		wantDisabled bool
	}{
		{
			name: "small tier preferred",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderOpenAI,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterCodexCLI,
			},
			wantModel: "gpt-5.4-mini",
		},
		{
			name: "medium fallback",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
			wantModel:   "claude-sonnet-4-6",
			wantWarning: "falling back to medium tier",
		},
		{
			name: "disabled without small or medium",
			llmConfig: config.LLMConfig{
				Provider: config.LLMProviderPi,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterPiRPC,
			},
			wantDisabled: true,
			wantWarning:  "disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &llm.FakeAdapter{}
			adapter.Queue(llm.FakeResult{Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"approval_override_requested":false}`)}})
			var warnings bytes.Buffer

			classifier := buildApprovalOverrideClassifier(config.Profile{LLM: tt.llmConfig}, adapter, &warnings)
			if classifier == nil {
				t.Fatal("classifier = nil, want lazy classifier")
			}
			if warnings.Len() != 0 {
				t.Fatalf("warnings after build = %q, want deferred warnings", warnings.String())
			}
			result, err := classifier.ClassifyApprovalOverride(context.Background(), approvalOverrideClassifierTestRequest(t))
			if err != nil {
				t.Fatalf("ClassifyApprovalOverride: %v", err)
			}
			requests := adapter.Requests()
			if tt.wantDisabled {
				if result.Approve || len(requests) != 0 {
					t.Fatalf("disabled classifier result = %#v requests=%#v, want no approval or LLM request", result, requests)
				}
			} else if len(requests) != 1 || requests[0].Model != tt.wantModel || requests[0].Effort != "low" {
				t.Fatalf("requests = %#v, want model %q effort low", requests, tt.wantModel)
			}
			if tt.wantWarning != "" && !strings.Contains(warnings.String(), tt.wantWarning) {
				t.Fatalf("warnings = %q, want substring %q", warnings.String(), tt.wantWarning)
			}
			if tt.wantWarning == "" && warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none", warnings.String())
			}
		})
	}
}

func TestReviewLiveRealRunnerHonorsConfiguredRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 30
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRunForPRKey(t, store, layout, "old-live", "github_other_repo_1", ledger.PostModeLive, now.Add(-31*24*time.Hour))
	oldDryRun := allocateReviewCommandRunForPRKey(t, store, layout, "old-dry", "github_other_repo_1", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old live GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
}

func TestReviewLiveSessionThroughRealRunnerPersistsNamedSession(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile

	fixture := newReviewCommandWorkbenchFixture(t)
	reviewCommandFixtures.Store(reviewCommandFixtureKey(fixture.ref), fixture)
	ref, pr := fixture.ref, fixture.pr
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("approve", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			statepaths.NewLayout(t.TempDir(), t.TempDir()),
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--session", "daily"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	session, err := store.GetNamedSession(context.Background(), "daily")
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if session.ProviderSessionID != "rollup-session" || session.Profile != "home" || session.Model != "claude-sonnet-4-6" {
		t.Fatalf("named session = %#v, want live runner persisted rollup scoped to profile/model", session)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "selection-session" {
		t.Fatalf("resumes = %#v, want rollup resumed from selection", resumes)
	}
}

func TestReviewRealRunnerResumesIncompleteRunThroughCLI(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile

	fixture := newReviewCommandWorkbenchFixture(t)
	reviewCommandFixtures.Store(reviewCommandFixtureKey(fixture.ref), fixture)
	ref, pr := fixture.ref, fixture.pr
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	artifacts, err := pipeline.ArtifactPathsForRun(layout, ref, pr, "home", "review-bot", "resume-live")
	if err != nil {
		t.Fatalf("ArtifactPathsForRun: %v", err)
	}
	prKey, err := statepaths.PRKey(ref.Host, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           pr.URL,
		RunID:           "resume-live",
		SHA:             pr.Head.SHA,
		BaseSHA:         pr.Base.SHA,
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		StartedAt:       time.Now().Add(-time.Minute),
		ArtifactPath:    artifacts.Dir,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("approve", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runs, err := store.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != run.RunID || runs[0].ArtifactPath != run.ArtifactPath {
		t.Fatalf("runs = %#v, want resumed run %q artifact %q", runs, run.RunID, run.ArtifactPath)
	}
	if runs[0].Outcome == nil || *runs[0].Outcome != ledger.OutcomeApproved {
		t.Fatalf("resumed run outcome = %v, want approved", runs[0].Outcome)
	}
	if len(adapter.Requests()) != 2 {
		t.Fatalf("adapter requests = %d, want selection/rollup planning", len(adapter.Requests()))
	}
}

func TestReviewDryRunRealRunnerHonorsConfiguredRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 30
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-31*24*time.Hour))
	newLive := allocateReviewCommandRun(t, store, layout, "new-live", ledger.PostModeLive, now.Add(-29*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old live GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(context.Background(), newLive.RunID); err != nil {
		t.Fatalf("new live GetRun error = %v, want nil", err)
	}
}

func TestReviewDryRunRealRunnerHonorsConfiguredKeepLiveForever(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	maxAgeDays := 0
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  &maxAgeDays,
		Enforcement: config.RetentionAtWrite,
	}
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-365*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); err != nil {
		t.Fatalf("old live GetRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("old dry-run GetRun error = %v, want ErrNotFound", err)
	}
}

func TestReviewDryRunRealRunnerHonorsManualOnlyRetention(t *testing.T) {
	cfg := testConfig()
	agentDir := t.TempDir()
	writeReviewAgent(t, agentDir)
	profile := cfg.Profiles["home"]
	profile.AgentSources = []string{agentDir}
	cfg.Profiles["home"] = profile
	cfg.Data.Retention = config.RetentionConfig{Enforcement: config.RetentionManualOnly}
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	now := time.Now()
	oldLive := allocateReviewCommandRun(t, store, layout, "old-live", ledger.PostModeLive, now.Add(-365*24*time.Hour))
	oldDryRun := allocateReviewCommandRun(t, store, layout, "old-dry", ledger.PostModeDryRun, now.Add(-8*24*time.Hour))
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	cmd, _ := newTestCommand(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		runner := buildReviewRunner(
			store,
			provider,
			adapter,
			profile,
			noopLimiter{},
			layout,
			opts.Stderr,
			nil,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := store.GetRun(context.Background(), oldLive.RunID); err != nil {
		t.Fatalf("old live GetRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(context.Background(), oldDryRun.RunID); err != nil {
		t.Fatalf("old dry-run GetRun error = %v, want nil", err)
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
		{name: "wrong host", args: []string{"--profile", "home", "review", "https://gitlab.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}},
		{name: "bad fail on", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fail-on", "urgent"}},
		{name: "negative agents", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-agents", "-1"}},
		{name: "negative concurrency", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--max-concurrency", "-1"}},
		{name: "rerun retry conflict", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun", "--retry-posts"}},
		{name: "session dry run", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--session", "daily"}},
		{name: "session no post", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--no-post", "--session", "daily"}},
		{name: "session retry posts", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts", "--session", "daily"}},
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

func TestReviewDryRunProgressWritesToStderr(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out, errOut := newTestCommandWithStderr(t, testConfig(), fakeFactory(runner), false)

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stderr := errOut.String()
	for _, want := range []string{
		`command="review" op="load_config" target="config"`,
		`command="review" op="parse_pr" target="pr"`,
		`command="review" op="resolve_profile" target="profile"`,
		`command="review" op="build_runtime" target="runtime"`,
		`command="review" op="execute_dry_run" target="pr"`,
		`command="review" op="render_result" target="stdout"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
	}
	if strings.Contains(out.String(), "cr progress") {
		t.Fatalf("stdout leaked progress = %q", out.String())
	}
}

func TestReviewQuietSuppressesProgressOnly(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out, errOut := newTestCommandWithStderr(t, testConfig(), fakeFactory(runner), true)

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want no progress output", errOut.String())
	}
	var decoded view.ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Run.PostMode != "dry_run" {
		t.Fatalf("decoded post mode = %q, want dry_run", decoded.Run.PostMode)
	}
}

func TestReviewQuietSuppressesProgressOnlyForTextOutput(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out, errOut := newTestCommandWithStderr(t, testConfig(), fakeFactory(runner), true)

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want no progress output", errOut.String())
	}
	if strings.Contains(out.String(), "cr progress") {
		t.Fatalf("stdout leaked progress = %q", out.String())
	}
	if !strings.Contains(out.String(), "Post mode: dry_run") {
		t.Fatalf("stdout = %q, want text dry-run render", out.String())
	}
}

func TestReviewDryRunRealRunnerQuietSuppressesProgressOnly(t *testing.T) {
	cfg := testConfig()
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	cmd, out, errOut := newTestCommandWithStderr(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		logger := newProgressLogger(opts)
		runner := buildReviewRunner(
			store,
			withProgressProvider(logger, provider),
			adapter,
			profile,
			noopLimiter{},
			statepaths.NewLayout(t.TempDir(), t.TempDir()),
			opts.Stderr,
			logger,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}, true)

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want no quiet progress output", errOut.String())
	}
	var decoded view.ReviewDryRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Run.PostMode != "dry_run" {
		t.Fatalf("decoded post mode = %q, want dry_run", decoded.Run.PostMode)
	}
}

func TestReviewDryRunRealRunnerWritesGitHubProgressToStderr(t *testing.T) {
	cfg := testConfig()
	ref, pr := reviewCommandPR(t)
	provider := &gitprovider.Fake{}
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	cmd, out, errOut := newTestCommandWithStderr(t, cfg, func(_ *cobra.Command, opts *root.Options, _ config.File, profile config.Profile, runtimeOpts RuntimeOptions) (Runtime, error) {
		logger := newProgressLogger(opts)
		runner := buildReviewRunner(
			store,
			withProgressProvider(logger, provider),
			adapter,
			profile,
			noopLimiter{},
			statepaths.NewLayout(t.TempDir(), t.TempDir()),
			opts.Stderr,
			logger,
			runtimeOptsWithWorkbench(t, runtimeOpts),
		)
		return Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}, false)

	if err := root.Execute(cmd, []string{"review", pr.URL, "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stderr := errOut.String()
	for _, want := range []string{
		`command="review" op="fetch_pr" target="pr"`,
		`command="review" op="fetch_diff" target="pr"`,
		`command="review" op="list_threads" target="threads"`,
		`command="review" op="run_llm_task" target="llm_task"`,
		`event=start`,
		`event=finish`,
		`task_id="orchestrator-selection"`,
		`phase="selection"`,
		`session_id="selection-session"`,
		`task_id="orchestrator-rollup"`,
		`phase="rollup"`,
		`session_id="rollup-session"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
	}
	if strings.Contains(out.String(), "cr progress") {
		t.Fatalf("stdout leaked progress = %q", out.String())
	}
}

func TestProgressProviderGetPRErrorWritesErrorBreadcrumb(t *testing.T) {
	var errOut bytes.Buffer
	provider := &gitprovider.Fake{}
	provider.SetError(gitprovider.OperationGetPR, gitprovider.WrapError(gitprovider.ErrRetryable, gitprovider.OperationGetPR, context.DeadlineExceeded))
	wrapped := withProgressProvider(progress.New(&errOut, false, nil), provider)

	_, err := wrapped.GetPR(context.Background(), gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29})
	if err == nil {
		t.Fatal("GetPR error = nil, want provider failure")
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, `event=error`) || !strings.Contains(stderr, `command="review" op="fetch_pr" target="pr"`) {
		t.Fatalf("stderr = %q, want error breadcrumb for fetch_pr", stderr)
	}
}

func TestProgressAdapterStartAndWaitWriteStructuredBreadcrumbs(t *testing.T) {
	var errOut bytes.Buffer
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	tokensIn := 11
	tokensOut := 7
	adapter.Queue(llm.FakeResult{
		SessionID: "sess-123",
		Response: llm.Response{
			Usage: llm.Usage{
				TokensIn:  &tokensIn,
				TokensOut: &tokensOut,
			},
		},
	})
	wrapped := withProgressAdapter(progress.New(&errOut, false, nil), adapter, "openai", "codex_cli")

	stream, err := wrapped.Start(context.Background(), llm.Request{
		Model:   "gpt-5.5",
		Effort:  "high",
		LogPath: filepath.Join(t.TempDir(), "selector.jsonl"),
		Prompt:  "prompt",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := stream.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	stderr := errOut.String()
	for _, want := range []string{
		`command="review" op="start_llm" target="llm"`,
		`provider="openai"`,
		`harness="codex_cli"`,
		`model="gpt-5.5"`,
		`effort="high"`,
		`log_file="selector.jsonl"`,
		`tokens_in="11"`,
		`tokens_out="7"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
	}
}

func TestProgressAdapterResumeErrorWritesErrorBreadcrumb(t *testing.T) {
	var errOut bytes.Buffer
	adapter := &llm.FakeAdapter{
		NameValue:           "fake-llm",
		SupportsResumeValue: true,
	}
	adapter.Queue(llm.FakeResult{StartErr: context.DeadlineExceeded})
	wrapped := withProgressAdapter(progress.New(&errOut, false, nil), adapter, "openai", "codex_cli")

	_, err := wrapped.Resume(context.Background(), "stored-session", llm.Request{Model: "gpt-5.5", Prompt: "prompt"})
	if err == nil {
		t.Fatal("Resume error = nil, want failure")
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, `command="review" op="resume_llm" target="llm"`) ||
		!strings.Contains(stderr, `event=error`) {
		t.Fatalf("stderr = %q, want resume error breadcrumb", stderr)
	}
}

func TestWithProgressProviderPreservesOptionalRangeDiffCapability(t *testing.T) {
	wrapped := withProgressProvider(progress.New(io.Discard, false, nil), &gitprovider.Fake{})
	if _, ok := wrapped.(interface {
		GetDiffBetweenRefs(context.Context, gitprovider.PRRef, string, string) (gitprovider.UnifiedDiff, error)
	}); ok {
		t.Fatalf("wrapped provider unexpectedly advertises GetDiffBetweenRefs")
	}
}

func TestFileTargetSanitizesRepoPath(t *testing.T) {
	got := fileTarget(" ./dir/../a\r\nb.go ")
	if got != "file:a__b.go" {
		t.Fatalf("fileTarget = %q, want sanitized repo path", got)
	}
}

func TestProgressPlannerWritesRunIDBreadcrumb(t *testing.T) {
	var errOut bytes.Buffer
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	provider := &gitprovider.Fake{}
	ref, pr := reviewCommandPR(t)
	if err := provider.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := provider.SetDiff(ref, gitprovider.UnifiedDiff{Raw: reviewSmallDiff("main.go")}); err != nil {
		t.Fatalf("SetDiff: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeReviewLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "no specialist needed"
	}`))
	adapter.Queue(fakeReviewLLMResult("rollup-session", reviewRollupJSON("comment", nil)))
	logger := progress.New(&errOut, false, nil)
	runner := buildReviewRunner(
		store,
		provider,
		adapter,
		testConfig().Profiles["home"],
		noopLimiter{},
		statepaths.NewLayout(t.TempDir(), t.TempDir()),
		&errOut,
		logger,
		runtimeOptsWithWorkbench(t, RuntimeOptions{PRRef: ref}),
	)

	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		RunID:           "run-123",
		PRKey:           "github_open-cli-collective_codereview-cli_29",
		PRURL:           pr.URL,
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeLive,
		SHA:             pr.Head.SHA,
		BaseSHA:         pr.Base.SHA,
		StartedAt:       time.Now().UTC(),
		ArtifactPath:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	_, err = runner.live.Planner.Live(context.Background(), pipeline.Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         testConfig().Profiles["home"],
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
	}, run)
	if err != nil {
		t.Fatalf("Planner.Live: %v", err)
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, `command="review" op="plan_live_review" target="pr"`) ||
		!strings.Contains(stderr, `run_id="run-123"`) {
		t.Fatalf("stderr = %q, want planner breadcrumb", stderr)
	}
}

func TestReviewDryRunTextProgressWritesStructuredStderrWithoutStdoutLeak(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, out, errOut := newTestCommandWithStderr(t, testConfig(), fakeFactory(runner), false)

	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "cr progress") {
		t.Fatalf("stdout leaked progress = %q", out.String())
	}
	if !strings.Contains(out.String(), "Post mode: dry_run") {
		t.Fatalf("stdout = %q, want text dry-run render", out.String())
	}
	assertProgressOutput(t, errOut.String(), []string{
		`command="review" op="load_config" target="config"`,
		`command="review" op="execute_dry_run" target="pr"`,
		`command="review" op="render_result" target="stdout"`,
	})
}

func assertProgressOutput(t *testing.T, stderr string, wants []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		t.Fatal("stderr has no progress lines")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "cr progress ") {
			t.Fatalf("stderr line = %q, want progress prefix", line)
		}
		if !strings.Contains(line, " event=") || !strings.Contains(line, ` command="`) || !strings.Contains(line, ` op="`) || !strings.Contains(line, ` target="`) {
			t.Fatalf("stderr line = %q, want structured progress fields", line)
		}
	}
	for _, want := range wants {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
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

func TestNewReviewDryRunMapsPlanSummary(t *testing.T) {
	tokensIn := 1200
	wall := int64(5000)
	result := testPipelineResult(false)
	result.Plan.Summary = reviewplan.Summary{
		Reviewers: []reviewplan.ReviewerSummary{{Name: "go:tests", Findings: 2}},
		Threads:   reviewplan.ThreadCounts{Considered: 3, Summarized: 2, Resolved: 1},
		Run: reviewplan.RunSummary{
			ToolVersion:       "0.0.0-test",
			Adapter:           "claude_cli",
			Model:             "sonnet",
			PostingIdentity:   "review-bot",
			SelectedReviewers: []string{"go:tests"},
			ReviewerCoverage: []reviewplan.ReviewerCoverageSummary{{
				AgentID:        "go:tests",
				Status:         "complete_broad",
				Scope:          []string{"main.go"},
				InspectedFiles: []string{"main.go"},
			}},
			WallDurationMS: &wall,
			Workstreams:    []reviewplan.WorkstreamUsage{{Name: "go:tests", Model: "sonnet", TokensIn: &tokensIn}},
		},
		Totals: reviewplan.AggregateUsage{TokensIn: &tokensIn},
	}

	rendered, err := newReviewDryRun(result)
	if err != nil {
		t.Fatalf("newReviewDryRun: %v", err)
	}
	summary := rendered.Summary
	if len(summary.Reviewers) != 1 || summary.Reviewers[0].Name != "go:tests" || summary.Reviewers[0].Findings != 2 {
		t.Fatalf("summary reviewers = %#v", summary.Reviewers)
	}
	if summary.Threads != (view.ReviewThreadCounts{Considered: 3, Summarized: 2, Resolved: 1}) {
		t.Fatalf("summary threads = %#v", summary.Threads)
	}
	run := summary.Run
	if run.ToolVersion != "0.0.0-test" || run.Adapter != "claude_cli" || run.Model != "sonnet" ||
		run.PostingIdentity != "review-bot" || len(run.SelectedReviewers) != 1 ||
		run.WallDurationMS == nil || *run.WallDurationMS != wall {
		t.Fatalf("summary run = %#v", run)
	}
	if len(run.Workstreams) != 1 || run.Workstreams[0].Name != "go:tests" ||
		run.Workstreams[0].TokensIn == nil || *run.Workstreams[0].TokensIn != tokensIn ||
		run.Workstreams[0].CostUSD != nil {
		t.Fatalf("summary workstreams = %#v", run.Workstreams)
	}
	if len(run.ReviewerCoverage) != 1 ||
		run.ReviewerCoverage[0].AgentID != "go:tests" ||
		run.ReviewerCoverage[0].Status != "complete_broad" ||
		len(run.ReviewerCoverage[0].InspectedFiles) != 1 ||
		run.ReviewerCoverage[0].InspectedFiles[0] != "main.go" {
		t.Fatalf("reviewer coverage = %#v", run.ReviewerCoverage)
	}
	if rendered.Summary.Totals.TokensIn == nil || *rendered.Summary.Totals.TokensIn != tokensIn || rendered.Summary.Totals.CostUSD != nil {
		t.Fatalf("summary totals = %#v", rendered.Summary.Totals)
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

func TestReviewMapsUnsafeAgentSourceError(t *testing.T) {
	runner := &fakeRunner{err: fmt.Errorf("%w: profile agent source agents is not trusted", agents.ErrUnsafeSource)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want runner error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
}

type fakeRunner struct {
	result          pipeline.Result
	err             error
	requests        []pipeline.Request
	liveResult      reviewrun.Result
	liveErr         error
	liveRequests    []pipeline.Request
	liveFlags       []reviewrun.Flags
	respondResult   threadrespond.Result
	respondErr      error
	respondRequests []threadrespond.Request
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context, string) error { return nil }

type cancelLimiter struct{}

func (cancelLimiter) Wait(context.Context, string) error { return context.Canceled }

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

func (r *fakeRunner) Respond(_ context.Context, req threadrespond.Request) (threadrespond.Result, error) {
	r.respondRequests = append(r.respondRequests, req)
	if r.respondErr != nil {
		return threadrespond.Result{}, r.respondErr
	}
	return r.respondResult, nil
}

func fakeFactory(runner *fakeRunner) RuntimeFactory {
	return func(*cobra.Command, *root.Options, config.File, config.Profile, RuntimeOptions) (Runtime, error) {
		return Runtime{Runner: runner, Responder: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}
}

func withReviewRuntimeSeams(
	t *testing.T,
	providerFactory func(config.GitConfig, githubprovider.TokenStore, githubprovider.Options) (gitprovider.GitProvider, gitprovider.Credential, error),
	identityResolver func(context.Context, gitprovider.GitProvider, gitprovider.Credential, githubprovider.TokenStore, config.Profile) (gitprovider.Identity, error),
	adapterFactory func(config.LLMConfig, *credstore.Store) (llm.Adapter, error),
) {
	t.Helper()
	originalProviderFactory := newGitProvider
	originalIdentityResolver := resolvePostingIdentityForRuntime
	originalAdapterFactory := newAdapterForRuntime
	newGitProvider = providerFactory
	resolvePostingIdentityForRuntime = identityResolver
	newAdapterForRuntime = adapterFactory
	t.Cleanup(func() {
		newGitProvider = originalProviderFactory
		resolvePostingIdentityForRuntime = originalIdentityResolver
		newAdapterForRuntime = originalAdapterFactory
	})
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
		Quiet:      true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out
}

func newTestCommandWithStderr(t *testing.T, cfg config.File, factory RuntimeFactory, quiet bool) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Quiet:      quiet,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out, &errOut
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

func testThreadRespondResult(outcome ledger.Outcome) threadrespond.Result {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return threadrespond.Result{
		Run: ledger.Run{
			RunID:           "respond-run-1",
			PRKey:           "github.com_open-cli-collective_codereview-cli_29",
			SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			PostMode:        ledger.PostModeLive,
			PostingIdentity: "review-bot",
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
		}},
		Plan: reviewplan.Plan{
			Outcome: reviewplan.OutcomeComment,
			Actions: []reviewplan.Action{
				{
					ActionID:  "thread_reply-1",
					Kind:      reviewplan.ActionKindThreadReply,
					ThreadID:  "thread-1",
					PlannedAt: now,
					Status:    reviewplan.ActionStatusPending,
					Required:  true,
					ThreadReply: &reviewplan.ThreadReplyPayload{
						Body:    "Summary",
						Summary: true,
					},
				},
				{
					ActionID:      "resolve_thread-1",
					Kind:          reviewplan.ActionKindResolveThread,
					ThreadID:      "thread-1",
					PlannedAt:     now,
					Status:        reviewplan.ActionStatusPending,
					Required:      true,
					ResolveThread: &reviewplan.ResolveThreadPayload{},
				},
			},
		},
		PlannedActions: []ledger.PlannedAction{
			{ActionID: "thread_reply-1", RunID: "respond-run-1", Kind: ledger.PlannedActionThreadReply, Status: ledger.PlannedActionPending, Required: true},
			{ActionID: "resolve_thread-1", RunID: "respond-run-1", Kind: ledger.PlannedActionResolveThread, Status: ledger.PlannedActionPending, Required: true},
		},
		Outbox: outbox.Result{Outcome: outcome, ExitCode: 0, Posted: 2},
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

func writeReviewAgent(t *testing.T, rootDir string) {
	t.Helper()
	writeReviewFile(t, filepath.Join(rootDir, "harness", "index.yaml"), "name: harness\ndescription: harness category\nowner: owner\n")
	writeReviewFile(t, filepath.Join(rootDir, "harness", "reviewer", "index.yaml"), "name: reviewer\ndescription: reviewer\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n")
	writeReviewFile(t, filepath.Join(rootDir, "harness", "reviewer", "prompt.md"), "Review carefully.")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
}

func writeReviewFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func assertReviewTestFile(t *testing.T, path, want string) {
	t.Helper()
	// #nosec G304 -- test paths are controlled by statedirtest.Hermetic/t.TempDir.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func approvalOverrideClassifierTestRequest(t *testing.T) approvaloverride.Request {
	t.Helper()
	return approvaloverride.Request{
		PR: gitprovider.PR{
			Title:  "Override",
			URL:    "https://example.test/pr/1",
			Author: gitprovider.Identity{Login: "author"},
		},
		PostingIdentity: gitprovider.Identity{Login: "review-bot"},
		LatestMarkerAt:  time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Candidates: []approvaloverride.Candidate{{
			ID:          "1",
			Source:      "issue_comment",
			Body:        "please approve",
			EffectiveAt: time.Date(2026, 6, 10, 12, 1, 0, 0, time.UTC),
		}},
		LLMTasksDir: filepath.Join(t.TempDir(), "llm-tasks"),
	}
}

type reviewCommandWorkbenchFixture struct {
	repoDir string
	ref     gitprovider.PRRef
	pr      gitprovider.PR
}

var reviewCommandFixtures sync.Map

func reviewCommandPR(t *testing.T) (gitprovider.PRRef, gitprovider.PR) {
	t.Helper()
	fixture := newReviewCommandWorkbenchFixture(t)
	reviewCommandFixtures.Store(reviewCommandFixtureKey(fixture.ref), fixture)
	return fixture.ref, fixture.pr
}

func reviewCommandMarkedThread(t *testing.T, pr gitprovider.PR, id, path string, line int) gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "old-run",
		ActionID: "old-action",
		Kind:     marker.ActionKindInlineComment,
		SHA:      pr.Head.SHA,
		BaseSHA:  pr.Base.SHA,
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	threadID := gitprovider.ThreadID(id)
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return gitprovider.InlineThread{
		ID:          threadID,
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   pr.Head.SHA,
		Comments: []gitprovider.ThreadComment{
			{
				ID:          gitprovider.CommentID(id + "-cr"),
				ThreadID:    threadID,
				Body:        action + "\nOriginal finding.",
				Author:      gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
				CommitSHA:   pr.Head.SHA,
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
				Body:        "Human confirmed this should be summarized.",
				Author:      gitprovider.Identity{Login: "human", ID: "human-id"},
				CommitSHA:   pr.Head.SHA,
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

func reviewCommandThreadAnalysisResult(threadID, sessionID string) llm.FakeResult {
	structured := fmt.Sprintf(`{"schema_version":1,"thread_id":%q,"decision":"summarize","summary":"Summary for future context.","resolve":true,"rationale":"human reply resolved the discussion"}`, threadID)
	return fakeReviewLLMResult(sessionID, structured)
}

func reviewCommandFixtureKey(ref gitprovider.PRRef) string {
	return fmt.Sprintf("%s/%s/%s/%d", ref.Host, ref.Owner, ref.Repo, ref.Number)
}

func newReviewCommandWorkbenchFixture(t *testing.T) reviewCommandWorkbenchFixture {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	repoDir := t.TempDir()
	reviewCommandGitMustSucceed(t, repoDir, "init", "-b", "main")
	reviewCommandGitMustSucceed(t, repoDir, "config", "user.name", "ReviewCmd Test")
	reviewCommandGitMustSucceed(t, repoDir, "config", "user.email", "reviewcmd@example.com")
	reviewCommandGitMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	writeReviewCommandFile(t, filepath.Join(repoDir, "main.go"), "package main\n\nvar changed = false\n")
	reviewCommandGitMustSucceed(t, repoDir, "add", "main.go")
	reviewCommandGitMustSucceed(t, repoDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(reviewCommandGitOutput(t, repoDir, "rev-parse", "HEAD"))
	reviewCommandGitMustSucceed(t, repoDir, "checkout", "-b", "feature")
	writeReviewCommandFile(t, filepath.Join(repoDir, "main.go"), "package main\n\nvar changed = true\n")
	reviewCommandGitMustSucceed(t, repoDir, "commit", "-am", "head")
	headSHA := strings.TrimSpace(reviewCommandGitOutput(t, repoDir, "rev-parse", "HEAD"))
	return reviewCommandWorkbenchFixture{
		repoDir: repoDir,
		ref:     ref,
		pr: gitprovider.PR{
			Ref:    ref,
			URL:    "https://github.com/open-cli-collective/codereview-cli/pull/29",
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
		},
	}
}

func reviewCommandGitMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(reviewCommandGitOutput(t, dir, args...))
}

func reviewCommandGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func reviewCommandGitCommand(ref gitprovider.PRRef) func(context.Context, string, ...string) ([]byte, error) {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "remote" && args[1] == "get-url" && args[2] == "origin" {
			return []byte(fmt.Sprintf("https://%s/%s/%s.git\n", ref.Host, ref.Owner, ref.Repo)), nil
		}
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			message := strings.TrimSpace(string(out))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
		}
		return out, nil
	}
}

func writeReviewCommandFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func runtimeOptsWithWorkbench(t *testing.T, opts RuntimeOptions) RuntimeOptions {
	t.Helper()
	opts.AutoUnlockWorkbenchOnExit = true
	fixture := newReviewCommandWorkbenchFixture(t)
	if opts.PRRef != (gitprovider.PRRef{}) {
		if cached, ok := reviewCommandFixtures.Load(reviewCommandFixtureKey(opts.PRRef)); ok {
			fixture = cached.(reviewCommandWorkbenchFixture)
		}
	}
	opts.ResolveRepoRoot = func(context.Context) (string, error) {
		return fixture.repoDir, nil
	}
	opts.GitCommand = reviewCommandGitCommand(fixture.ref)
	return opts
}

func allocateReviewCommandRun(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	prKey := "github_open-cli-collective_codereview-cli_29"
	return allocateReviewCommandRunForPRKey(t, store, layout, runID, prKey, mode, started)
}

func allocateReviewCommandRunForPRKey(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID, prKey string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	fixture := newReviewCommandWorkbenchFixture(t)
	baseSHA := fixture.pr.Base.SHA
	headSHA := fixture.pr.Head.SHA
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/29",
		RunID:           runID,
		SHA:             headSHA,
		BaseSHA:         baseSHA,
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    filepath.Join(layout.DataRoot, "runs", prKey, headSHA, baseSHA, "home__review-bot", runID),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func reviewSmallDiff(path string) string {
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

func fakeReviewLLMResult(sessionID, structured string) llm.FakeResult {
	return llm.FakeResult{
		SessionID: sessionID,
		Response:  llm.Response{StructuredOutput: []byte(structured), DurationMS: 1},
	}
}

func reviewRollupJSON(event string, ordered []string) string {
	payload := map[string]any{
		"schema_version":         1,
		"review_event":           event,
		"review_event_rationale": "policy",
		"dedupe_log":             []any{},
		"ordered_findings":       ordered,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal rollup: %v", err))
	}
	return string(data)
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

func TestNewAdapterCreatesCodexCLI(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "codex_cli" {
		t.Fatalf("adapter.Name = %q, want codex_cli", adapter.Name())
	}
}

func TestNewAdapterCreatesPiRPC(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderPi,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterPiRPC,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "pi_rpc" {
		t.Fatalf("adapter.Name = %q, want pi_rpc", adapter.Name())
	}
}

func TestNewAdapterCreatesClaudeCLIWithResume(t *testing.T) {
	adapter, err := newAdapter(config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
	}, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if adapter.Name() != "claude_cli" || !adapter.SupportsResume() {
		t.Fatalf("adapter = %s resume=%v, want claude_cli with resume", adapter.Name(), adapter.SupportsResume())
	}
}

func TestNewAdapterRejectsCodexCLINonSubscription(t *testing.T) {
	_, err := newAdapter(config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterCodexCLI,
		CredentialRef: "codereview/openai",
	}, nil)
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("newAdapter error = %v, want config.ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "requires provider openai with subscription auth") {
		t.Fatalf("newAdapter error = %v, want Codex compatibility guidance", err)
	}
}

func TestNewAdapterRejectsPiRPCNonSubscription(t *testing.T) {
	_, err := newAdapter(config.LLMConfig{
		Provider:      config.LLMProviderPi,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterPiRPC,
		CredentialRef: "codereview/pi",
	}, nil)
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("newAdapter error = %v, want config.ErrUnsupported", err)
	}
}
