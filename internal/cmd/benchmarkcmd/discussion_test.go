package benchmarkcmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestReviewArgsPassesEffectiveDiscussionIsolation(t *testing.T) {
	optOut := false
	for _, tt := range []struct {
		name      string
		benchCase benchmark.Case
		want      bool
	}{
		{name: "unpinned"},
		{name: "pinned", benchCase: benchmark.Case{ReviewBaseSHA: "1111111", ReviewHeadSHA: "2222222"}, want: true},
		{name: "pinned opt out", benchCase: benchmark.Case{ReviewBaseSHA: "1111111", ReviewHeadSHA: "2222222", WithoutDiscussion: &optOut}},
		{name: "expected only", benchCase: benchmark.Case{ExpectedBaseSHA: "1111111", ExpectedHeadSHA: "2222222"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := reviewArgs(t.TempDir(), benchmark.Candidate{Profile: "home"}, tt.benchCase)
			if got := stringSliceContains(args, "--without-discussion"); got != tt.want {
				t.Fatalf("args = %#v, want --without-discussion=%t", args, tt.want)
			}
		})
	}
}

func TestInProcessExecutorPassesExplicitDiscussionOptOut(t *testing.T) {
	optOut := false
	var gotRequest pipeline.Request
	executor := inProcessExecutor{
		cfg: testConfig(),
		open: func(context.Context, app.OpenRequest) (app.Runtime, error) {
			return app.Runtime{Runner: benchmarkTestRunner{dryRun: func(_ context.Context, req pipeline.Request) (pipeline.Result, error) {
				gotRequest = req
				return pipeline.Result{Run: ledger.Run{RunID: "opt-out", ArtifactPath: t.TempDir()}}, nil
			}}}, nil
		},
	}
	result := executor.Execute(context.Background(), reviewExecutionRequest{
		Candidate: benchmark.Candidate{Profile: "home"},
		Case:      benchmark.Case{PR: "https://github.com/open-cli-collective/codereview-cli/pull/1", ReviewBaseSHA: "1111111", ReviewHeadSHA: "2222222", WithoutDiscussion: &optOut},
	})
	if result.Err != nil || gotRequest.WithoutDiscussion {
		t.Fatalf("Execute = %#v, request = %#v, want explicit opt out", result, gotRequest)
	}
}

func TestBenchmarkDiscussionIsolationCompatibility(t *testing.T) {
	optOut := false
	for _, tt := range []struct {
		name         string
		stdout       string
		exitCode     int
		optOut       *bool
		wantClass    string
		wantVerified bool
	}{
		{name: "supported", stdout: `{"run":{"without_discussion":true}}`, wantClass: failureNone, wantVerified: true},
		{name: "unknown old binary", stdout: `{"run":{"run_id":"old"}}`, wantClass: failureDiscussionIsolationUnverified},
		{name: "false marker", stdout: `{"run":{"without_discussion":false}}`, wantClass: failureDiscussionIsolationUnverified},
		{name: "null marker", stdout: `{"run":{"without_discussion":null}}`, wantClass: failureDiscussionIsolationUnverified},
		{name: "missing JSON", wantClass: failureMissingReviewJSON},
		{name: "invalid JSON", stdout: `{broken`, wantClass: failureInvalidReviewJSON},
		{name: "unsupported flag", exitCode: exitcode.UsageError, wantClass: failureUsageError},
		{name: "explicit old binary opt out", stdout: `{"run":{"run_id":"old"}}`, optOut: &optOut, wantClass: failureNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			benchCase := benchmark.Case{ID: "pinned", PR: "https://github.com/open-cli-collective/codereview-cli/pull/1", ReviewBaseSHA: "1111111", ReviewHeadSHA: "2222222", WithoutDiscussion: tt.optOut}
			executor := subprocessExecutor{run: func(_ context.Context, _ string, args []string) reviewCommandResult {
				calls++
				if got := stringSliceContains(args, "--without-discussion"); got != benchCase.EffectiveWithoutDiscussion() {
					t.Fatalf("args = %#v, want effective discussion flag", args)
				}
				return reviewCommandResult{Stdout: []byte(tt.stdout), Stderr: []byte("original stderr\n"), ExitCode: tt.exitCode}
			}}
			resultsDir := t.TempDir()
			run, err := executeBenchmarkRun(context.Background(), root.NewProgressLogger(&root.Options{}), resultsDir, resultsDir, "fake-cr", "run", benchmark.Candidate{ID: "candidate", Profile: "home"}, benchCase, executor)
			if err != nil {
				t.Fatalf("executeBenchmarkRun: %v", err)
			}
			if calls != 1 || run.ExitCode != tt.exitCode || run.FailureClassification != tt.wantClass || run.WithoutDiscussionVerified != tt.wantVerified || run.RequestedWithoutDiscussion != benchCase.EffectiveWithoutDiscussion() {
				t.Fatalf("calls=%d run=%#v, want class=%s verified=%t actual child exit=%d", calls, run, tt.wantClass, tt.wantVerified, tt.exitCode)
			}
			if tt.wantClass == failureDiscussionIsolationUnverified {
				if compareRunStatus(run, true) != runStatusFailed || !strings.Contains(strings.Join(run.Warnings, "\n"), "could not be verified") {
					t.Fatalf("run=%#v, want failed comparison and explicit capability warning", run)
				}
			}
			if got := discussionReadFileString(t, resultsDir, run.Artifacts.ReviewJSON); got != tt.stdout {
				t.Fatalf("review stdout changed: %q", got)
			}
			if got := discussionReadFileString(t, resultsDir, run.Artifacts.Stderr); got != "original stderr\n" {
				t.Fatalf("review stderr changed: %q", got)
			}
		})
	}
}

func TestRunCountsUnverifiedIsolationAsFailureAndPreservesProvenance(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		return reviewCommandResult{Stdout: []byte(`{"run":{"run_id":"old"}}`), ExitCode: exitcode.Success}
	})
	resultsDir := filepath.Join(t.TempDir(), "results")
	if err := root.Execute(cmd, []string{"benchmark", "run", suitePath, "--case", "case_two", "--candidate", "first", "--in-process", "--results-dir", resultsDir, "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var summary benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("summary JSON: %v", err)
	}
	if summary.SuccessCount != 0 || summary.FailureCount != 1 || !summary.SelectedCases[0].WithoutDiscussion || !summary.Runs[0].RequestedWithoutDiscussion || summary.Runs[0].WithoutDiscussionVerified {
		t.Fatalf("summary=%#v, want requested but unverified isolation counted as failure", summary)
	}
	var manifest benchmarkManifest
	if err := json.Unmarshal([]byte(discussionReadFileString(t, resultsDir, summary.Artifacts.Manifest)), &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if !manifest.SelectedCases[0].WithoutDiscussion || !manifest.Runs[0].RequestedWithoutDiscussion || manifest.Runs[0].WithoutDiscussionVerified {
		t.Fatalf("manifest=%#v, want requested/verified policy", manifest)
	}
	comparison := buildComparison(summary, summary.ResultsDir)
	if !comparison.Runs[0].RequestedWithoutDiscussion || comparison.Runs[0].WithoutDiscussionVerified || comparison.Runs[0].Status != runStatusFailed {
		t.Fatalf("comparison=%#v, want unverified isolation failure", comparison.Runs[0])
	}
}

func discussionReadFileString(t *testing.T, resultsDir, path string) string {
	t.Helper()
	fixture, err := os.OpenRoot(resultsDir)
	if err != nil {
		t.Fatalf("OpenRoot %s: %v", resultsDir, err)
	}
	defer fixture.Close()
	relative, err := filepath.Rel(resultsDir, path)
	if err != nil {
		t.Fatalf("relative fixture path %s: %v", path, err)
	}
	data, err := fixture.ReadFile(relative)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

func TestCompareRechecksDiscussionIsolationAgainstCurrentArtifact(t *testing.T) {
	for _, tt := range []struct {
		name         string
		currentJSON  string
		missing      bool
		wantClass    string
		wantVerified bool
		wantStatus   string
	}{
		{name: "unchanged verified artifact", currentJSON: `{"run":{"without_discussion":true}}`, wantClass: failureNone, wantVerified: true, wantStatus: runStatusCompleted},
		{name: "marker removed", currentJSON: `{"run":{"run_id":"old"}}`, wantClass: failureDiscussionIsolationUnverified, wantStatus: runStatusFailed},
		{name: "marker changed to false", currentJSON: `{"run":{"without_discussion":false}}`, wantClass: failureDiscussionIsolationUnverified, wantStatus: runStatusFailed},
		{name: "marker changed to null", currentJSON: `{"run":{"without_discussion":null}}`, wantClass: failureDiscussionIsolationUnverified, wantStatus: runStatusFailed},
		{name: "invalid JSON", currentJSON: `{broken`, wantClass: failureInvalidReviewJSON, wantStatus: runStatusFailed},
		{name: "empty JSON", wantClass: failureMissingReviewJSON, wantStatus: runStatusFailed},
		{name: "missing artifact", missing: true, wantClass: failureMissingArtifact, wantStatus: runStatusFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resultsDir := t.TempDir()
			summary := comparisonFixtureSummary(resultsDir)
			summary.Runs[0].RequestedWithoutDiscussion = true
			summary.Runs[0].WithoutDiscussionVerified = true
			writeComparisonFixture(t, summary)
			artifact := summary.Runs[0].Artifacts.ReviewJSON
			writeLog(t, artifact, `{"run":{"without_discussion":true}}`)
			writeLog(t, summary.Runs[0].Artifacts.Stderr, "original stderr\n")
			initial := buildComparison(summary, resultsDir)
			if !initial.Runs[0].WithoutDiscussionVerified || initial.Runs[0].Status != runStatusCompleted {
				t.Fatalf("initial comparison = %#v, want verified completed run", initial.Runs[0])
			}
			if tt.missing {
				if err := os.Remove(artifact); err != nil {
					t.Fatalf("Remove review JSON: %v", err)
				}
			} else {
				writeLog(t, artifact, tt.currentJSON)
			}
			summaryBefore := discussionReadFileString(t, resultsDir, summary.Artifacts.SuiteSummary)
			comparison, err := writeComparisonArtifactsForResultsDir(resultsDir)
			if err != nil {
				t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
			}
			row := comparison.Runs[0]
			if row.WithoutDiscussionVerified != tt.wantVerified || row.Status != tt.wantStatus || row.FailureClassification != tt.wantClass || row.ExitCode != exitcode.Success || !row.RequestedWithoutDiscussion {
				t.Fatalf("row = %#v, want status=%s class=%s verified=%t actual exit=0", row, tt.wantStatus, tt.wantClass, tt.wantVerified)
			}
			if !tt.wantVerified && !strings.Contains(strings.Join(row.Warnings, "\n"), "discussion isolation could not be verified") {
				t.Fatalf("warnings = %#v, want current-artifact isolation warning", row.Warnings)
			}
			if got := discussionReadFileString(t, resultsDir, summary.Artifacts.SuiteSummary); got != summaryBefore {
				t.Fatal("compare changed the saved suite summary")
			}
			if got := discussionReadFileString(t, resultsDir, summary.Runs[0].Artifacts.Stderr); got != "original stderr\n" {
				t.Fatalf("compare changed raw stderr: %q", got)
			}
			if !tt.missing {
				if got := discussionReadFileString(t, resultsDir, artifact); got != tt.currentJSON {
					t.Fatalf("compare changed raw review JSON: %q", got)
				}
			}
		})
	}
}

func TestCompareDiscussionVerificationCannotUpgradeSavedExecution(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs[0].RequestedWithoutDiscussion = true
	summary.Runs[0].WithoutDiscussionVerified = false
	summary.Runs[0].FailureClassification = failureDiscussionIsolationUnverified
	writeReviewJSON(t, summary.Runs[0].Artifacts.ReviewJSON, view.ReviewDryRun{Run: view.ReviewRun{WithoutDiscussion: true}})
	comparison := buildComparison(summary, resultsDir)
	row := comparison.Runs[0]
	if row.WithoutDiscussionVerified || row.Status != runStatusFailed || row.FailureClassification != failureDiscussionIsolationUnverified {
		t.Fatalf("row = %#v, want original unverified execution to remain failed", row)
	}
}

func TestCompareDiscussionRecheckPreservesChildFailure(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs[0].RequestedWithoutDiscussion = true
	summary.Runs[0].WithoutDiscussionVerified = true
	summary.Runs[0].ExitCode = exitcode.UpstreamError
	summary.Runs[0].FailureClassification = failureUpstreamError
	writeReviewJSON(t, summary.Runs[0].Artifacts.ReviewJSON, view.ReviewDryRun{})
	row := buildComparison(summary, resultsDir).Runs[0]
	if row.WithoutDiscussionVerified || row.Status != runStatusFailed || row.ExitCode != exitcode.UpstreamError || row.FailureClassification != failureUpstreamError {
		t.Fatalf("row = %#v, want original child failure/exit with invalidated verification", row)
	}
}
