package reviewcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/appruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdtest"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/config/configtest"
	"github.com/open-cli-collective/codereview-cli/internal/gateio"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/plannedactions"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestReviewDryRunCallsRunnerAndRendersText(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	var gotRuntime app.OpenRequest
	var cleanupCalled bool
	cmd, out := newTestCommand(t, testConfig(), func(_ context.Context, opts app.OpenRequest) (app.Runtime, error) {
		gotRuntime = opts
		return app.Runtime{
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
	if len(req.AgentDirs) != 1 || req.AgentDirs[0] != "/tmp/agents" || !req.AllowSelfReview || !req.AllowSelfApprove || !req.NoResolveThreads || !req.MajorRequestChanges {
		t.Fatalf("request flags = %#v", req)
	}
	if req.SelectionModelOverride != "" || req.SelectionEffortOverride != "" ||
		req.SelectionPromptInstructions != "" ||
		req.ReviewerModelOverride != "" || req.ReviewerEffortOverride != "" || req.ReviewerFast {
		t.Fatalf("stage overrides = %#v, want empty when flags omitted", req)
	}
	if gotRuntime.MaxAgents != 3 || gotRuntime.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = %#v, want max agents/concurrency", gotRuntime)
	}
	if gotRuntime.Command != "review" || gotRuntime.Progress == nil || gotRuntime.Warnings != out {
		t.Fatalf("runtime command/progress/warnings = %#v/%#v/%#v, want review/progress/stdout-stderr", gotRuntime.Command, gotRuntime.Progress, gotRuntime.Warnings)
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	if text := out.String(); !strings.Contains(text, "Post mode: dry_run") || !strings.Contains(text, "Planned actions:") {
		t.Fatalf("stdout = %q, want dry-run render", text)
	}
}

func TestReviewCommandAcceptanceHarnessComposesDryRun(t *testing.T) {
	result := testPipelineResult(false)
	result.Plan.Summary = reviewplan.Summary{
		Reviewers: []reviewplan.ReviewerSummary{{Name: "harness:reviewer", Findings: 1}},
		Run: reviewplan.RunSummary{
			Adapter:           "fake-llm",
			Model:             "claude-sonnet-4-6",
			PostingIdentity:   "review-bot",
			SelectedReviewers: []string{"harness:reviewer"},
		},
	}
	runner := &fakeRunner{result: result}
	var gotRuntime app.OpenRequest
	var cleanupCalled bool
	cmd, out, errOut := newTestCommandWithStderr(t, testConfig(), func(_ context.Context, opts app.OpenRequest) (app.Runtime, error) {
		gotRuntime = opts
		if opts.Profile.Git.Credential.Name != "codereview/home" {
			t.Fatalf("runtime profile credential ref = %q, want repository-routed home profile", opts.Profile.Git.Credential.Name)
		}
		return app.Runtime{
			Runner:          runner,
			PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
			Cleanup:         func() { cleanupCalled = true },
		}, nil
	}, false)

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--json",
		"--quiet",
		"--agents-dir", "/tmp/agents",
		"--fail-on", "minor",
		"--max-agents", "3",
		"--max-concurrency", "2",
		"--allow-self-review",
		"--allow-self-approve",
		"--no-resolve-threads",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty when --quiet is set", errOut.String())
	}
	if !cleanupCalled {
		t.Fatal("runtime cleanup was not called")
	}
	if gotRuntime.MaxAgents != 3 || gotRuntime.MaxConcurrency != 2 {
		t.Fatalf("runtime opts = %#v, want max agents/concurrency", gotRuntime)
	}
	if gotRuntime.PRRef.Number != 29 || gotRuntime.PRRef.Owner != "open-cli-collective" || gotRuntime.PRRef.Repo != "codereview-cli" {
		t.Fatalf("runtime PR ref = %#v, want parsed issue fixture PR", gotRuntime.PRRef)
	}
	if gotRuntime.Command != "review" || gotRuntime.Progress == nil || gotRuntime.Warnings != errOut {
		t.Fatalf("runtime command/progress/warnings = %#v/%#v/%#v, want review/progress/stderr", gotRuntime.Command, gotRuntime.Progress, gotRuntime.Warnings)
	}
	if gotRuntime.RequireOpinionatedReviewAuthority {
		t.Fatal("dry-run runtime should not require opinionated review authority")
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
	if len(req.AgentDirs) != 1 || req.AgentDirs[0] != "/tmp/agents" || !req.AllowSelfReview || !req.AllowSelfApprove || !req.NoResolveThreads || !req.MajorRequestChanges {
		t.Fatalf("request flags = %#v", req)
	}

	var rendered map[string]any
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatalf("unmarshal dry-run JSON: %v\n%s", err, out.String())
	}
	assertJSONPath(t, rendered, "run", "run_id", "run-1")
	assertJSONPath(t, rendered, "run", "post_mode", "dry_run")
	if _, ok := rendered["findings"].([]any); !ok {
		t.Fatalf("findings JSON field = %T, want array", rendered["findings"])
	}
	if _, ok := rendered["actions"].([]any); !ok {
		t.Fatalf("actions JSON field = %T, want array", rendered["actions"])
	}
	assertJSONOmitsKeys(t, rendered, map[string]bool{
		"provider_session_id": true,
		"session_row_id":      true,
		"retry":               true,
		"ledger":              true,
		"outbox":              true,
		"gate":                true,
	})
}

func TestReviewUsesRepositoryProfileRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.Credential.Name = "codereview/work"
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
	cmd, _ := newTestCommand(t, cfg, func(_ context.Context, req app.OpenRequest) (app.Runtime, error) {
		if req.Profile.Git.Credential.Name != "codereview/work" {
			t.Fatalf("runtime profile credential ref = %q, want work route", req.Profile.Git.Credential.Name)
		}
		return app.Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	if err := root.Execute(cmd, []string{"review", "https://github.com/rianjs/bar/pull/29", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].ProfileName != "work" {
		t.Fatalf("request profile = %#v, want work", runner.requests)
	}
}

func TestReviewRejectsAmbiguousRepositoryProfileRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.Credential.Name = "codereview/work"
	cfg.Profiles["work"] = work
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called for ambiguous repository routes")
		return app.Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"review", "https://github.com/rianjs/bar/pull/29", "--dry-run"})
	if !errors.Is(err, config.ErrRepositoryProfileAmbiguous) {
		t.Fatalf("Execute error = %v, want ErrRepositoryProfileAmbiguous", err)
	}
	if !strings.Contains(err.Error(), "pass --profile with one of: home, work") {
		t.Fatalf("error = %v, want profile suggestions", err)
	}
}

func TestReviewExplicitProfileBypassesRepositoryRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.Credential.Name = "codereview/work"
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
	cmd, _ := newTestCommand(t, cfg, func(_ context.Context, req app.OpenRequest) (app.Runtime, error) {
		if req.Profile.Git.Credential.Name != "codereview/home" {
			t.Fatalf("runtime profile credential ref = %q, want explicit home", req.Profile.Git.Credential.Name)
		}
		return app.Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
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
	work.Git.Credential.Name = "codereview/work"
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
	cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called for an empty explicit profile")
		return app.Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "", "review", "https://github.com/rianjs/bar/pull/29", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("Execute error = %v, want empty profile failure", err)
	}
}

func TestReviewUnmatchedRepositoryRequiresProfileOrRoute(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["home"]
	work.Git.Credential.Name = "codereview/work"
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
	cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called for an unmatched repository")
		return app.Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
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
	work.Git.Credential.Name = "codereview/work"
	cfg.Profiles["work"] = work
	cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called when route profile host mismatches")
		return app.Runtime{}, nil
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
	cmd, out := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called for help")
		return app.Runtime{}, nil
	})

	if err := root.Execute(cmd, []string{"review", "--help"}); err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"already approved the PR",
		"--rerun to bypass these local gates",
		"--rerun reuse the PR's original reviewer cohort",
		"--session scopes only the orchestrator conversation",
		"--fresh-session",
		"reselects the reviewer cohort",
		"--fast requests fast execution for reviewer agents only",
		"approval override request newer than that marker",
		"--retry-posts is recovery-only",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help = %q, want substring %q", text, want)
		}
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
			cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
				factoryCalled = true
				return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
			cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
				factoryCalled = true
				return app.Runtime{Runner: &fakeRunner{liveResult: testLiveResult(false)}}, nil
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
			cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
				factoryCalled = true
				return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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

func TestReviewEmptyStageOverrideErrorPrecedence(t *testing.T) {
	cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory was called for empty stage overrides")
		return app.Runtime{}, nil
	})

	err := root.Execute(cmd, []string{
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/29",
		"--dry-run",
		"--selection-effort", "xhigh",
		"--selection-prompt", " ",
	})
	if err == nil || err.Error() != "--selection-effort must be one of low, medium, high" {
		t.Fatalf("Execute error = %v, want selection-effort precedence", err)
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
			cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
				factoryCalled = true
				return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
	cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		factoryCalled = true
		return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
	cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		factoryCalled = true
		return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
			cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
				factoryCalled = true
				return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
	cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		factoryCalled = true
		return app.Runtime{Runner: &fakeRunner{result: testPipelineResult(false)}}, nil
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
	tests := []struct {
		name           string
		maxAgeDays     int
		enforcement    config.RetentionEnforcement
		wantLiveMaxAge time.Duration
		wantForever    bool
		wantManualOnly bool
	}{
		{
			name:           "manual forever",
			maxAgeDays:     0,
			enforcement:    config.RetentionManualOnly,
			wantForever:    true,
			wantManualOnly: true,
		},
		{
			name:           "automatic positive max age",
			maxAgeDays:     30,
			wantLiveMaxAge: 30 * 24 * time.Hour,
			wantManualOnly: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Data.Retention = config.RetentionConfig{
				MaxAgeDays:  &tt.maxAgeDays,
				Enforcement: tt.enforcement,
			}
			runner := &fakeRunner{result: testPipelineResult(false)}
			var got app.OpenRequest
			cmd, _ := newTestCommand(t, cfg, func(_ context.Context, opts app.OpenRequest) (app.Runtime, error) {
				got = opts
				return app.Runtime{Runner: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
			})

			if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got.Retention.LiveForever != tt.wantForever || got.Retention.LiveMaxAge != tt.wantLiveMaxAge || got.RetentionManualOnly != tt.wantManualOnly {
				t.Fatalf("runtime retention = %#v manual %v, want forever=%v max_age=%s manual=%v", got.Retention, got.RetentionManualOnly, tt.wantForever, tt.wantLiveMaxAge, tt.wantManualOnly)
			}
		})
	}
}

func TestRetentionPolicyFromConfigDefaultsWhenMaxAgeOmitted(t *testing.T) {
	got := appruntime.RetentionPolicyFromConfig(config.RetentionConfig{})
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
	if !runner.liveRequests[0].Rerun || runner.liveRequests[0].FreshSession {
		t.Fatalf("pipeline request = %#v, want rerun with reusable provider session", runner.liveRequests[0])
	}
}

func TestReviewFreshSessionPropagatesWithoutChangingRunMode(t *testing.T) {
	liveRunner := &fakeRunner{liveResult: testLiveResult(false)}
	liveCmd, _ := newTestCommand(t, testConfig(), fakeFactory(liveRunner))
	if err := root.Execute(liveCmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--rerun", "--fresh-session", "--session", "daily"}); err != nil {
		t.Fatalf("live Execute: %v", err)
	}
	if len(liveRunner.liveRequests) != 1 || !liveRunner.liveRequests[0].Rerun || !liveRunner.liveRequests[0].FreshSession || liveRunner.liveRequests[0].SessionName != "daily" {
		t.Fatalf("live request = %#v, want rerun/fresh named session", liveRunner.liveRequests)
	}

	dryRunner := &fakeRunner{result: testPipelineResult(false)}
	dryCmd, _ := newTestCommand(t, testConfig(), fakeFactory(dryRunner))
	if err := root.Execute(dryCmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fresh-session"}); err != nil {
		t.Fatalf("dry-run Execute: %v", err)
	}
	if len(dryRunner.requests) != 1 || !dryRunner.requests[0].FreshSession || dryRunner.requests[0].Rerun {
		t.Fatalf("dry-run request = %#v, want fresh provider session with ordinary local gates", dryRunner.requests)
	}
}

func TestReviewFastPropagatesToPipelineRequest(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, testConfig(), fakeFactory(runner))
	if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fast", "--reviewer-model", "claude-opus-4-8"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 || !runner.requests[0].ReviewerFast {
		t.Fatalf("requests = %#v, want reviewer fast", runner.requests)
	}
}

func TestReviewFastPreferencePrecedence(t *testing.T) {
	for _, tt := range []struct {
		name        string
		profileFast bool
		flags       []string
		want        bool
	}{
		{name: "default off"},
		{name: "profile default", profileFast: true, want: true},
		{name: "flag enables", flags: []string{"--fast"}, want: true},
		{name: "explicit fast false overrides profile", profileFast: true, flags: []string{"--fast=false"}},
		{name: "no-fast overrides profile", profileFast: true, flags: []string{"--no-fast"}},
		{name: "explicit no-fast false enables", flags: []string{"--no-fast=false"}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			profile := cfg.Profiles["home"]
			profile.Fast = tt.profileFast
			cfg.Profiles["home"] = profile
			runner := &fakeRunner{result: testPipelineResult(false)}
			cmd, _ := newTestCommand(t, cfg, fakeFactory(runner))
			args := []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"}
			args = append(args, tt.flags...)
			if err := root.Execute(cmd, args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(runner.requests) != 1 || runner.requests[0].ReviewerFast != tt.want {
				t.Fatalf("requests = %#v, want ReviewerFast=%t", runner.requests, tt.want)
			}
		})
	}
}

func TestReviewRetryPostsUsesEffectiveFastPreference(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["home"]
	profile.Fast = true
	cfg.Profiles["home"] = profile

	t.Run("profile fast rejects before runtime construction", func(t *testing.T) {
		factoryCalled := false
		cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
			factoryCalled = true
			return app.Runtime{}, nil
		})
		err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts"})
		if exitcode.FromError(err) != exitcode.UsageError || factoryCalled {
			t.Fatalf("Execute error = %v, factory called = %t, want usage before runtime", err, factoryCalled)
		}
	})

	t.Run("no-fast permits retry", func(t *testing.T) {
		runner := &fakeRunner{liveResult: testLiveResult(false)}
		cmd, _ := newTestCommand(t, cfg, fakeFactory(runner))
		if err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts", "--no-fast"}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(runner.liveRequests) != 1 || runner.liveRequests[0].ReviewerFast {
			t.Fatalf("live requests = %#v, want standard-speed retry", runner.liveRequests)
		}
	})
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
		{name: "fresh session retry posts", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts", "--fresh-session"}},
		{name: "fast retry posts", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--retry-posts", "--fast"}},
		{name: "fast flag conflict", args: []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run", "--fast", "--no-fast"}},
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
	result.PlannedActions[0].PayloadDecodeError = errors.New("invalid payload JSON")

	_, err := view.NewReviewDryRun(result)
	if err == nil {
		t.Fatal("newReviewDryRun error = nil, want invalid payload failure")
	}
	if !strings.Contains(err.Error(), "invalid payload JSON") {
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

	rendered, err := view.NewReviewDryRun(result)
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
	return func(context.Context, app.OpenRequest) (app.Runtime, error) {
		return app.Runtime{Runner: runner, Responder: runner, PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"}}, nil
	}
}

func assertJSONPath(t *testing.T, root any, path ...any) {
	t.Helper()
	current := root
	for _, segment := range path[:len(path)-1] {
		switch key := segment.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("JSON path %v segment %q reached %T, want object", path, key, current)
			}
			current = object[key]
		case int:
			array, ok := current.([]any)
			if !ok {
				t.Fatalf("JSON path %v segment %d reached %T, want array", path, key, current)
			}
			if key < 0 || key >= len(array) {
				t.Fatalf("JSON path %v segment %d out of bounds len %d", path, key, len(array))
			}
			current = array[key]
		default:
			t.Fatalf("unsupported JSON path segment %T", segment)
		}
	}
	want := path[len(path)-1]
	if got := current; got != want {
		t.Fatalf("JSON path %v = %#v, want %#v", path[:len(path)-1], got, want)
	}
}

func assertJSONOmitsKeys(t *testing.T, value any, forbidden map[string]bool) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbidden[key] {
				t.Fatalf("dry-run JSON leaked harness-only key %q", key)
			}
			assertJSONOmitsKeys(t, child, forbidden)
		}
	case []any:
		for _, child := range typed {
			assertJSONOmitsKeys(t, child, forbidden)
		}
	}
}

func newTestCommand(t *testing.T, cfg config.File, factory RuntimeFactory) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	opts := &root.Options{
		ConfigPath: path,
		Quiet:      true,
		Stdin:      strings.NewReader(""),
	}
	cmd, out, _ := cmdtest.New(opts, func(cmd *cobra.Command, opts *root.Options) {
		RegisterWithFactory(cmd, opts, factory)
	})
	opts.Stderr = out
	return cmd, out
}

func newTestCommandWithStderr(t *testing.T, cfg config.File, factory RuntimeFactory, quiet bool) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	cmd, out, errOut := cmdtest.New(&root.Options{
		ConfigPath: path,
		Quiet:      quiet,
		Stdin:      strings.NewReader(""),
	}, func(cmd *cobra.Command, opts *root.Options) {
		RegisterWithFactory(cmd, opts, factory)
	})
	return cmd, out, errOut
}

func testConfig() config.File {
	return configtest.File()
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
				Action: plannedactions.Action{
					ActionID: "inline_comment-1", Kind: reviewplan.ActionKindInlineComment, FindingID: "finding-1",
					PlannedAt: now, Status: reviewplan.ActionStatusPlannedOnly,
					InlineComment: &reviewplan.InlineCommentPayload{
						Body: "Fix this", Path: "main.go", Side: review.DiffSideRight, Line: 2, SubjectType: review.AnchorKindLine,
					},
				},
				Marker: reviewplan.MarkerPlacement{BodyBearing: true},
			}},
		},
		PlannedActions: []ledger.PlannedAction{{
			Action: plannedactions.Action{
				ActionID: "inline_comment-1", Kind: ledger.PlannedActionInlineComment, FindingID: "finding-1", PlannedAt: now,
				InlineComment: &plannedactions.InlineCommentPayload{Body: "Fix this", Path: "main.go"}, Status: ledger.PlannedActionPlannedOnly,
			},
			RunID: "run-1",
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

func writeReviewFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestFakeFactoryErrorIsReturned(t *testing.T) {
	factoryErr := errors.New("factory failed")
	cmd, _ := newTestCommand(t, testConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		return app.Runtime{}, factoryErr
	})
	err := root.Execute(cmd, []string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29", "--dry-run"})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Execute error = %v, want factory error", err)
	}
}
