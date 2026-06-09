package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestCompareCommandWritesArtifactsAndJSONWithoutConfig(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	writeComparisonFixture(t, summary)
	writeReviewJSON(t, summary.Runs[0].Artifacts.ReviewJSON, view.ReviewDryRun{
		Run:            view.ReviewRun{RunID: "child-run-1", ArtifactPath: "/outside/durable-review"},
		RollupMarkdown: "UNIQUE_ROLLUP_TEXT_SHOULD_NOT_LEAK",
		Findings: []view.ReviewFinding{
			{ID: "hit", Severity: "major", FilePath: "main.go", Side: "RIGHT", Line: intPtrForBenchmark(2), Body: "UNIQUE_BODY_SHOULD_NOT_LEAK"},
			{ID: "unmatched", Severity: "minor", FilePath: "other.go", Side: "RIGHT", Line: intPtrForBenchmark(9), Body: "UNIQUE_UNMATCHED_BODY_SHOULD_NOT_LEAK"},
		},
	})

	cmd, out := newCompareOnlyTestCommand(filepath.Join(t.TempDir(), "missing-config.yml"))
	oldRunner := runReviewCommand
	runReviewCommand = func(context.Context, string, []string) reviewCommandResult {
		t.Fatal("compare must not invoke review command")
		return reviewCommandResult{}
	}
	t.Cleanup(func() { runReviewCommand = oldRunner })

	if err := root.Execute(cmd, []string{"benchmark", "compare", resultsDir, "--json"}); err != nil {
		t.Fatalf("Execute compare: %v", err)
	}
	var got comparisonReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal compare JSON: %v\n%s", err, out.String())
	}
	if got.SchemaVersion != benchmarkArtifactSchemaVersion || got.SuiteID != "suite1" || len(got.Runs) != 1 {
		t.Fatalf("comparison = %#v, want schema, suite, and one run", got)
	}
	if got.Runs[0].Status != runStatusCompleted || got.Runs[0].FailureClassification != failureNone {
		t.Fatalf("run status/class = %s/%s, want completed/none", got.Runs[0].Status, got.Runs[0].FailureClassification)
	}
	if got.Runs[0].PRURL == "" || got.Runs[0].RequestedReviewBaseSHA != "1111111" || got.Runs[0].ReviewBaseSHA != "review-base" || got.Runs[0].CurrentHeadSHA != "current-head" {
		t.Fatalf("run identity = %#v, want PR and requested/observed/current SHAs", got.Runs[0])
	}
	if got.Runs[0].AnchorSummary == nil || got.Runs[0].AnchorSummary.AnchorOverlapHit != 1 || got.Runs[0].AnchorSummary.AnchorOverlapMiss != 1 || got.Runs[0].AnchorSummary.UnmatchedFinding != 1 {
		t.Fatalf("anchor summary = %#v, want hit, miss, unmatched", got.Runs[0].AnchorSummary)
	}
	assertFileContains(t, got.Artifacts.ComparisonJSON, `"placement_caveat"`)
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "Anchor overlap is mechanical placement only")
	if strings.Contains(out.String(), "UNIQUE_BODY_SHOULD_NOT_LEAK") || strings.Contains(out.String(), "UNIQUE_ROLLUP_TEXT_SHOULD_NOT_LEAK") {
		t.Fatalf("stdout JSON leaked raw review text:\n%s", out.String())
	}
	assertExactJSONKeys(t, out.Bytes(), []string{
		"schema_version",
		"mode",
		"suite_id",
		"results_dir",
		"source_artifacts",
		"artifacts",
		"placement_caveat",
		"candidates",
		"cases",
		"runs",
		"case_totals",
		"candidate_totals",
		"warnings",
	})
	for _, artifact := range []string{got.Artifacts.ComparisonJSON, got.Artifacts.ComparisonMarkdown} {
		body := readFile(t, artifact)
		for _, forbidden := range []string{"UNIQUE_BODY_SHOULD_NOT_LEAK", "UNIQUE_UNMATCHED_BODY_SHOULD_NOT_LEAK", "UNIQUE_ROLLUP_TEXT_SHOULD_NOT_LEAK"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s leaked %q:\n%s", artifact, forbidden, body)
			}
		}
	}
}

func TestComparePreservesMatrixOrderAndAggregatesTotals(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.SelectedCandidates = append(summary.SelectedCandidates, benchmarkCandidate{
		ID:      "second",
		Profile: "work",
		Stages: benchmarkCandidateStages{
			Selection: benchmarkSelectionStage{Model: "kimi"},
		},
	})
	summary.SelectedCases = append(summary.SelectedCases, benchmarkCase{ID: "case_two", PR: "https://github.com/open-cli-collective/codereview-cli/pull/2"})
	summary.Runs = []benchmarkRun{
		matrixFixtureRun(resultsDir, "0001-c01-k01-first-case_one", "first", "case_one", 0, failureNone, 2, map[string]int{"major": 1, "minor": 1}, 10),
		matrixFixtureRun(resultsDir, "0002-c01-k02-first-case_two", "first", "case_two", 0, failureNone, 1, map[string]int{"nits": 1}, 20),
		matrixFixtureRun(resultsDir, "0003-c02-k01-second-case_one", "second", "case_one", 5, failureUpstreamError, 0, map[string]int{}, 30),
		matrixFixtureRun(resultsDir, "0004-c02-k02-second-case_two", "second", "case_two", 0, failureNone, 3, map[string]int{"major": 2, "advice": 1}, 40),
	}
	writeComparisonFixture(t, summary)
	for _, run := range summary.Runs {
		if run.ExitCode == 0 {
			writeReviewJSON(t, run.Artifacts.ReviewJSON, view.ReviewDryRun{Run: view.ReviewRun{RunID: run.ReviewRunID}})
		}
	}

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	wantRunOrder := []string{
		"0001-c01-k01-first-case_one",
		"0002-c01-k02-first-case_two",
		"0003-c02-k01-second-case_one",
		"0004-c02-k02-second-case_two",
	}
	for i, want := range wantRunOrder {
		if got.Runs[i].RunID != want {
			t.Fatalf("run order[%d] = %s, want %s", i, got.Runs[i].RunID, want)
		}
	}
	if len(got.CaseTotals) != 2 || got.CaseTotals[0].CaseID != "case_one" || got.CaseTotals[1].CaseID != "case_two" {
		t.Fatalf("case totals order = %#v, want suite case order", got.CaseTotals)
	}
	if got.CaseTotals[0].RunCount != 2 || got.CaseTotals[0].CompletedCount != 1 || got.CaseTotals[0].FailedCount != 1 || got.CaseTotals[0].FindingCount != 2 {
		t.Fatalf("case_one total = %#v, want 2 runs, 1 completed, 1 failed, 2 findings", got.CaseTotals[0])
	}
	if len(got.CandidateTotals) != 2 || got.CandidateTotals[0].CandidateID != "first" || got.CandidateTotals[1].CandidateID != "second" {
		t.Fatalf("candidate totals order = %#v, want suite candidate order", got.CandidateTotals)
	}
	if got.CandidateTotals[0].RunCount != 2 || got.CandidateTotals[0].FindingCount != 3 || got.CandidateTotals[0].DurationMS != 30 {
		t.Fatalf("first candidate total = %#v, want aggregate findings/duration", got.CandidateTotals[0])
	}
	if got.CandidateTotals[1].RunCount != 2 || got.CandidateTotals[1].CompletedCount != 1 || got.CandidateTotals[1].FailedCount != 1 || got.CandidateTotals[1].SeverityCounts["advice"] != 1 {
		t.Fatalf("second candidate total = %#v, want failure and unknown severity aggregate", got.CandidateTotals[1])
	}
}

func TestCompareSelectionModeShowsSelectedReviewers(t *testing.T) {
	resultsDir := t.TempDir()
	runDir := filepath.Join(resultsDir, "0001-c01-k01-first-case_one")
	summary := benchmarkSuiteSummary{
		SchemaVersion: benchmarkArtifactSchemaVersion,
		Mode:          benchmarkModeSelection,
		SuiteID:       "suite1",
		ResultsDir:    resultsDir,
		SelectedCandidates: []benchmarkCandidate{{
			ID:      "first",
			Profile: "home",
			Stages: benchmarkCandidateStages{
				Selection: benchmarkSelectionStage{Model: "sonnet", Effort: "high"},
			},
		}},
		SelectedCases: []benchmarkCase{{
			ID: "case_one",
			PR: "https://github.com/open-cli-collective/codereview-cli/pull/1",
		}},
		Runs: []benchmarkRun{
			{
				RunID:                 "0001-c01-k01-first-case_one",
				CandidateID:           "first",
				CaseID:                "case_one",
				PRURL:                 "https://github.com/open-cli-collective/codereview-cli/pull/1",
				ExitCode:              0,
				FailureClassification: failureNone,
				SelectedAgents:        []benchmarkSelectedAgent{{AgentID: "harness:alpha", Files: []string{"main.go"}}},
				ThreadActionCount:     1,
				Artifacts: runArtifacts{
					Dir:           runDir,
					SelectionJSON: filepath.Join(runDir, "selection.json"),
					SelectionLog:  filepath.Join(runDir, "agent-logs", "orchestrator-selection.jsonl"),
					RecipeJSON:    filepath.Join(runDir, "recipe.json"),
					Stderr:        filepath.Join(runDir, "stderr.txt"),
					MetricsJSON:   filepath.Join(runDir, "metrics.json"),
				},
			},
			{
				RunID:                 "0002-c01-k02-first-case_two",
				CandidateID:           "first",
				CaseID:                "case_one",
				PRURL:                 "https://github.com/open-cli-collective/codereview-cli/pull/1",
				ExitCode:              1,
				FailureClassification: failureInvalidSelectionJSON,
				Artifacts: runArtifacts{
					Dir:           filepath.Join(resultsDir, "0002-c01-k02-first-case_two"),
					SelectionJSON: filepath.Join(resultsDir, "0002-c01-k02-first-case_two", "selection.json"),
					SelectionLog:  filepath.Join(resultsDir, "0002-c01-k02-first-case_two", "agent-logs", "orchestrator-selection.jsonl"),
					RecipeJSON:    filepath.Join(resultsDir, "0002-c01-k02-first-case_two", "recipe.json"),
					Stderr:        filepath.Join(resultsDir, "0002-c01-k02-first-case_two", "stderr.txt"),
					MetricsJSON:   filepath.Join(resultsDir, "0002-c01-k02-first-case_two", "metrics.json"),
				},
				Warnings: []string{"structured output invalid after retry"},
			},
		},
		Artifacts: suiteArtifacts{
			Manifest:           filepath.Join(resultsDir, "manifest.json"),
			SummaryJSONL:       filepath.Join(resultsDir, "summary.jsonl"),
			SuiteSummary:       filepath.Join(resultsDir, "suite-summary.json"),
			Report:             filepath.Join(resultsDir, "report.md"),
			ComparisonJSON:     filepath.Join(resultsDir, "comparison.json"),
			ComparisonMarkdown: filepath.Join(resultsDir, "comparison.md"),
		},
	}
	writeComparisonFixture(t, summary)

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	if got.Mode != benchmarkModeSelection || got.Runs[0].Status != runStatusCompleted || got.Runs[0].SelectedAgents[0].AgentID != "harness:alpha" {
		t.Fatalf("comparison = %#v, want selector-mode completed run with selected reviewer", got)
	}
	if got.Runs[1].Status != runStatusFailed || got.Runs[1].FailureClassification != failureInvalidSelectionJSON {
		t.Fatalf("failed selector run = %#v, want failed invalid-selection classification", got.Runs[1])
	}
	assertFileContains(t, got.Artifacts.ComparisonJSON, `"mode": "selection"`)
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "Selector Benchmark Comparison")
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "harness:alpha")
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "invalid_selection_json")
}

func TestCompareConsumesRealBenchmarkSelectArtifacts(t *testing.T) {
	selectCmd, _ := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile) (reviewcmd.SelectionRuntime, error) {
			return reviewcmd.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, _ pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			return pipeline.SelectionResult{
				Artifacts: pipeline.ArtifactPathsFromDir(req.ArtifactDir),
				Selection: llm.Selection{
					SelectedAgents: []llm.SelectedAgent{{AgentID: "harness:alpha", Files: []string{"main.go"}}},
				},
				SelectionSession: pipeline.SelectionSession{
					Response: llm.Response{
						StructuredOutput: []byte(`{"schema_version":1,"selected_agents":[{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]}],"thread_actions":[],"reasoning":"ok"}`),
					},
				},
			}, nil
		},
	)

	if err := root.Execute(selectCmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
	}); err != nil {
		t.Fatalf("Execute select: %v", err)
	}

	compareCmd, out := newCompareOnlyTestCommand(filepath.Join(t.TempDir(), "missing-config.yml"))
	if err := root.Execute(compareCmd, []string{"benchmark", "compare", resultsDir, "--json"}); err != nil {
		t.Fatalf("Execute compare: %v", err)
	}
	var got comparisonReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal compare JSON: %v\n%s", err, out.String())
	}
	if got.Mode != benchmarkModeSelection || len(got.Runs) != 1 || got.Runs[0].SelectedAgents[0].AgentID != "harness:alpha" {
		t.Fatalf("comparison = %#v, want real selector comparison output", got)
	}
	assertFileContains(t, got.Artifacts.ComparisonJSON, `"selected_agents"`)
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "Selector Benchmark Comparison")
}

func TestCompareClassifiesFailuresAndPathEscapes(t *testing.T) {
	resultsDir := t.TempDir()
	outsideReview := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(outsideReview, []byte(`{"findings":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile outside review: %v", err)
	}
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs = []benchmarkRun{
		{
			RunID:                 "usage",
			CandidateID:           "first",
			CaseID:                "case_one",
			ExitCode:              2,
			RetryCount:            0,
			FailureClassification: failureUsageError,
			SeverityCounts:        map[string]int{},
			Artifacts:             runArtifacts{ReviewJSON: filepath.Join(resultsDir, "usage", "review.json")},
		},
		{
			RunID:                 "missing",
			CandidateID:           "first",
			CaseID:                "case_one",
			ExitCode:              0,
			RetryCount:            0,
			FailureClassification: failureNone,
			SeverityCounts:        map[string]int{},
			Artifacts:             runArtifacts{ReviewJSON: filepath.Join(resultsDir, "missing", "review.json")},
		},
		{
			RunID:                 "invalid",
			CandidateID:           "first",
			CaseID:                "case_one",
			ExitCode:              0,
			RetryCount:            0,
			FailureClassification: failureNone,
			SeverityCounts:        map[string]int{},
			Artifacts:             runArtifacts{ReviewJSON: filepath.Join(resultsDir, "invalid", "review.json")},
		},
		{
			RunID:                 "escape",
			CandidateID:           "first",
			CaseID:                "case_one",
			ExitCode:              0,
			RetryCount:            0,
			FailureClassification: failureNone,
			SeverityCounts:        map[string]int{},
			Artifacts:             runArtifacts{ReviewJSON: outsideReview},
		},
	}
	writeComparisonFixture(t, summary)
	writeLog(t, summary.Runs[0].Artifacts.ReviewJSON, "")
	if err := os.MkdirAll(filepath.Dir(summary.Runs[2].Artifacts.ReviewJSON), 0o700); err != nil {
		t.Fatalf("MkdirAll invalid run: %v", err)
	}
	writeLog(t, summary.Runs[2].Artifacts.ReviewJSON, "{bad json")

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	classes := map[string]string{}
	statuses := map[string]string{}
	for _, run := range got.Runs {
		classes[run.RunID] = run.FailureClassification
		statuses[run.RunID] = run.Status
	}
	if classes["usage"] != failureUsageError || statuses["usage"] != runStatusFailed {
		t.Fatalf("usage class/status = %s/%s, want usage_error/failed", classes["usage"], statuses["usage"])
	}
	if classes["missing"] != failureMissingArtifact || statuses["missing"] != runStatusPartial {
		t.Fatalf("missing class/status = %s/%s, want missing_artifact/partial", classes["missing"], statuses["missing"])
	}
	if classes["invalid"] != failureInvalidReviewJSON || statuses["invalid"] != runStatusPartial {
		t.Fatalf("invalid class/status = %s/%s, want invalid_review_json/partial", classes["invalid"], statuses["invalid"])
	}
	if classes["escape"] != failureMissingArtifact || statuses["escape"] != runStatusPartial {
		t.Fatalf("escape class/status = %s/%s, want missing_artifact/partial", classes["escape"], statuses["escape"])
	}
	for _, run := range got.Runs {
		if run.AnchorSummary != nil {
			t.Fatalf("run %s anchor summary = %#v, want nil when review placement is unavailable", run.RunID, run.AnchorSummary)
		}
	}
}

func TestCompareDeduplicatesInvalidJSONWarnings(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	invalidReviewJSON := "{bad json"
	var review view.ReviewDryRun
	parseErr := json.Unmarshal([]byte(invalidReviewJSON), &review)
	if parseErr == nil {
		t.Fatal("invalid review JSON unexpectedly parsed")
	}
	parseWarning := "review JSON parse failed: " + parseErr.Error()
	anchorWarning := "anchor placement unavailable: review JSON could not be parsed"
	summary.Runs[0].Warnings = []string{
		"runtime warning",
		parseWarning,
		parseWarning,
		anchorWarning,
	}
	writeComparisonFixture(t, summary)
	writeLog(t, summary.Runs[0].Artifacts.ReviewJSON, invalidReviewJSON)

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	wantWarnings := []string{"runtime warning", parseWarning, anchorWarning}
	if len(got.Runs[0].Warnings) != len(wantWarnings) {
		t.Fatalf("warnings = %#v, want %#v", got.Runs[0].Warnings, wantWarnings)
	}
	for i, want := range wantWarnings {
		if got.Runs[0].Warnings[i] != want {
			t.Fatalf("warnings[%d] = %q, want %q; warnings=%#v", i, got.Runs[0].Warnings[i], want, got.Runs[0].Warnings)
		}
	}
	comparisonJSON := readFile(t, got.Artifacts.ComparisonJSON)
	if strings.Count(comparisonJSON, parseWarning) != 1 {
		t.Fatalf("comparison JSON contains parse warning %d times, want once:\n%s", strings.Count(comparisonJSON, parseWarning), comparisonJSON)
	}
	if strings.Count(comparisonJSON, anchorWarning) != 1 {
		t.Fatalf("comparison JSON contains anchor warning %d times, want once:\n%s", strings.Count(comparisonJSON, anchorWarning), comparisonJSON)
	}
}

func TestAppendUniqueWarningsPreservesFirstSeenOrder(t *testing.T) {
	got := appendUniqueWarnings([]string{"first", "second", "first"}, "third", "second", "fourth")
	want := []string{"first", "second", "third", "fourth"}
	if len(got) != len(want) {
		t.Fatalf("warnings = %#v, want %#v", got, want)
	}
	for i, wantWarning := range want {
		if got[i] != wantWarning {
			t.Fatalf("warnings[%d] = %q, want %q; warnings=%#v", i, got[i], wantWarning, got)
		}
	}
	if got := appendUniqueWarnings(nil); got != nil {
		t.Fatalf("empty warnings = %#v, want nil", got)
	}
}

func TestCompareRejectsSymlinkReviewJSONEscape(t *testing.T) {
	resultsDir := t.TempDir()
	outsideReview := filepath.Join(t.TempDir(), "review.json")
	writeReviewJSON(t, outsideReview, view.ReviewDryRun{Run: view.ReviewRun{RunID: "outside"}})
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs[0].Artifacts.ReviewJSON = filepath.Join(resultsDir, "symlinked", "review.json")
	writeComparisonFixture(t, summary)
	if err := os.MkdirAll(filepath.Dir(summary.Runs[0].Artifacts.ReviewJSON), 0o700); err != nil {
		t.Fatalf("MkdirAll symlink dir: %v", err)
	}
	if err := os.Symlink(outsideReview, summary.Runs[0].Artifacts.ReviewJSON); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	if got.Runs[0].FailureClassification != failureMissingArtifact || got.Runs[0].Status != runStatusPartial {
		t.Fatalf("symlink class/status = %s/%s, want missing_artifact/partial", got.Runs[0].FailureClassification, got.Runs[0].Status)
	}
	if got.Runs[0].AnchorSummary != nil {
		t.Fatalf("symlink anchor summary = %#v, want nil", got.Runs[0].AnchorSummary)
	}
}

func TestCompareRejectsDanglingSymlinkReviewJSON(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs[0].Artifacts.ReviewJSON = filepath.Join(resultsDir, "symlinked", "review.json")
	writeComparisonFixture(t, summary)
	if err := os.MkdirAll(filepath.Dir(summary.Runs[0].Artifacts.ReviewJSON), 0o700); err != nil {
		t.Fatalf("MkdirAll symlink dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-review.json"), summary.Runs[0].Artifacts.ReviewJSON); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	if got.Runs[0].FailureClassification != failureMissingArtifact || got.Runs[0].Status != runStatusPartial {
		t.Fatalf("dangling symlink class/status = %s/%s, want missing_artifact/partial", got.Runs[0].FailureClassification, got.Runs[0].Status)
	}
}

func TestComparisonMarkdownRendersUnknownSeverities(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.Runs[0].SeverityCounts = map[string]int{"advice": 2, "major": 1}
	writeComparisonFixture(t, summary)
	writeReviewJSON(t, summary.Runs[0].Artifacts.ReviewJSON, view.ReviewDryRun{Run: view.ReviewRun{RunID: "child-run-1"}})

	got, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err != nil {
		t.Fatalf("writeComparisonArtifactsForResultsDir: %v", err)
	}
	markdown := readFile(t, got.Artifacts.ComparisonMarkdown)
	if !strings.Contains(markdown, "Nits | advice | Duration ms") {
		t.Fatalf("markdown missing sorted unknown severity header:\n%s", markdown)
	}
	if !strings.Contains(markdown, "| `first` | `case_one` | completed | 0 | none | 2 | 0 | 1 | 0 | 0 | 2 |") {
		t.Fatalf("markdown missing unknown severity count:\n%s", markdown)
	}
}

func TestComparisonMarkdownEscapesTableCells(t *testing.T) {
	report := comparisonReport{
		SuiteID:         "suite|one",
		ResultsDir:      "/tmp/results",
		PlacementCaveat: placementCaveat,
		Cases:           []benchmarkCase{{ID: "case|one", PR: "https://example.test/pr/1"}},
		Runs: []comparisonRun{{
			RunID:                 "run|one",
			CandidateID:           "candidate|one",
			CaseID:                "case|one",
			Status:                runStatusCompleted,
			FailureClassification: failureNone,
			SeverityCounts:        map[string]int{},
			Artifacts: comparisonRunArtifacts{
				ReviewJSON:  "/tmp/results/review|one.json",
				Stderr:      "/tmp/results/stderr.txt",
				MetricsJSON: "/tmp/results/metrics.json",
			},
		}},
		CaseTotals:      []comparisonCaseTotal{{CaseID: "case|one", SeverityCounts: map[string]int{}}},
		CandidateTotals: []comparisonCandidateTotal{{CandidateID: "candidate|one", SeverityCounts: map[string]int{}}},
	}

	markdown := renderComparisonMarkdown(report)
	for _, want := range []string{"candidate\\|one", "case\\|one", "review\\|one.json", "run\\|one"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing escaped %q:\n%s", want, markdown)
		}
	}
}

func TestCompareRejectsUnsupportedSchema(t *testing.T) {
	resultsDir := t.TempDir()
	summary := comparisonFixtureSummary(resultsDir)
	summary.SchemaVersion = benchmarkArtifactSchemaVersion - 1
	writeComparisonFixture(t, summary)

	_, err := writeComparisonArtifactsForResultsDir(resultsDir)
	if err == nil {
		t.Fatal("writeComparisonArtifactsForResultsDir error = nil, want unsupported schema")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error = %v, want schema_version", err)
	}
}

func TestCompareAnchorsPlacementLabels(t *testing.T) {
	anchors := []benchmark.Anchor{
		{ID: "multiple", File: "main.go", Side: "RIGHT", Lines: []int{1, 3}},
		{ID: "miss", File: "main.go", Side: "RIGHT", Lines: []int{10, 12}},
	}
	findings := []view.ReviewFinding{
		{ID: "first", FilePath: "main.go", Side: "RIGHT", Line: intPtrForBenchmark(2)},
		{ID: "second", FilePath: "main.go", Side: "RIGHT", Line: intPtrForBenchmark(3)},
		{ID: "wrong-file", FilePath: "other.go", Side: "RIGHT", Line: intPtrForBenchmark(2)},
		{ID: "wrong-side", FilePath: "main.go", Side: "LEFT", Line: intPtrForBenchmark(2)},
		{ID: "no-line", FilePath: "main.go", Side: "RIGHT"},
	}

	summary, results, unmatched := compareAnchors(anchors, findings)
	if summary == nil || summary.MultipleAnchorOverlaps != 1 || summary.AnchorOverlapMiss != 1 || summary.AnchorOverlapHit != 0 || summary.UnmatchedFinding != 3 {
		t.Fatalf("summary = %#v, want multiple=1 miss=1 unmatched=3", summary)
	}
	if len(results) != 2 || results[0].PlacementLabel != placementMultipleAnchors || results[1].PlacementLabel != placementAnchorMiss {
		t.Fatalf("results = %#v, want multiple then miss", results)
	}
	if len(unmatched) != 3 || unmatched[0].PlacementLabel != placementUnmatched || unmatched[2].FindingID != "no-line" {
		t.Fatalf("unmatched = %#v, want three unmatched findings", unmatched)
	}
	noAnchorSummary, noAnchorResults, noAnchorUnmatched := compareAnchors(nil, findings)
	if noAnchorSummary != nil || noAnchorResults != nil || noAnchorUnmatched != nil {
		t.Fatalf("no-anchor output = %#v %#v %#v, want omitted", noAnchorSummary, noAnchorResults, noAnchorUnmatched)
	}
	badSummary, badResults, badUnmatched := compareAnchors([]benchmark.Anchor{{ID: "bad", File: "main.go", Side: "RIGHT", Lines: []int{1}}}, findings)
	if badSummary == nil || len(badResults) != 0 || badSummary.AnchorOverlapMiss != 0 || len(badUnmatched) != len(findings) {
		t.Fatalf("bad-anchor output = %#v %#v %#v, want no anchor result and unmatched findings", badSummary, badResults, badUnmatched)
	}
}

func TestRunWritesComparisonArtifactsAndEmbedsAnchors(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, benchmarkSuiteWithAnchor(t))
	crBin := writeExecutableCRBin(t)
	resultsDir := filepath.Join(t.TempDir(), "results")
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		return reviewCommandResult{Stdout: reviewDryRunJSONWithAnchorFinding(t, "child-run-1", "main.go", "RIGHT", 2), ExitCode: 0}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
		"--cr-bin", crBin,
		"--json",
	}); err != nil {
		t.Fatalf("Execute run: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal run JSON: %v\n%s", err, out.String())
	}
	if got.SchemaVersion != benchmarkArtifactSchemaVersion || got.Runs[0].RetryCount != 0 || got.Runs[0].FailureClassification != failureNone {
		t.Fatalf("run schema/retry/class = %d/%d/%s", got.SchemaVersion, got.Runs[0].RetryCount, got.Runs[0].FailureClassification)
	}
	if len(got.SelectedCases[0].Anchors) != 1 {
		t.Fatalf("selected case anchors = %#v, want one anchor", got.SelectedCases[0].Anchors)
	}
	assertFileContains(t, got.Artifacts.ComparisonJSON, `"anchor_overlap_hit": 1`)
	assertFileContains(t, got.Artifacts.ComparisonMarkdown, "anchor_overlap_hit")
}

func newCompareOnlyTestCommand(configPath string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: configPath,
		Stdout:     &out,
		Stderr:     &bytes.Buffer{},
	})
	Register(cmd, opts)
	return cmd, &out
}

func comparisonFixtureSummary(resultsDir string) benchmarkSuiteSummary {
	runDir := filepath.Join(resultsDir, "0001-c01-k01-first-case_one")
	return benchmarkSuiteSummary{
		SchemaVersion: benchmarkArtifactSchemaVersion,
		SuiteID:       "suite1",
		ResultsDir:    resultsDir,
		SelectedCandidates: []benchmarkCandidate{{
			ID:      "first",
			Profile: "home",
			Stages: benchmarkCandidateStages{
				Selection: benchmarkSelectionStage{Model: "sonnet", Effort: "high"},
			},
		}},
		SelectedCases: []benchmarkCase{{
			ID: "case_one",
			PR: "https://github.com/open-cli-collective/codereview-cli/pull/1",
			Anchors: []benchmark.Anchor{
				{ID: "expected-hit", File: "main.go", Side: "RIGHT", Lines: []int{1, 3}},
				{ID: "expected-miss", File: "main.go", Side: "RIGHT", Lines: []int{10, 12}},
			},
		}},
		Runs: []benchmarkRun{{
			RunID:                  "0001-c01-k01-first-case_one",
			CandidateID:            "first",
			CaseID:                 "case_one",
			PRURL:                  "https://github.com/open-cli-collective/codereview-cli/pull/1",
			RequestedReviewBaseSHA: "1111111",
			RequestedReviewHeadSHA: "2222222",
			ExpectedBaseSHA:        "aaaaaaa",
			ExpectedHeadSHA:        "bbbbbbb",
			ReviewBaseSHA:          "review-base",
			ReviewHeadSHA:          "review-head",
			CurrentBaseSHA:         "current-base",
			CurrentHeadSHA:         "current-head",
			ExitCode:               0,
			RetryCount:             0,
			FailureClassification:  failureNone,
			FindingCount:           2,
			SeverityCounts:         map[string]int{"major": 1, "minor": 1},
			Artifacts: runArtifacts{
				Dir:         runDir,
				ReviewJSON:  filepath.Join(runDir, "review.json"),
				Stderr:      filepath.Join(runDir, "stderr.txt"),
				MetricsJSON: filepath.Join(runDir, "metrics.json"),
			},
			ReviewArtifactPath: "/outside/durable-review",
		}},
		Artifacts: suiteArtifacts{
			Manifest:           filepath.Join(resultsDir, "manifest.json"),
			SummaryJSONL:       filepath.Join(resultsDir, "summary.jsonl"),
			SuiteSummary:       filepath.Join(resultsDir, "suite-summary.json"),
			Report:             filepath.Join(resultsDir, "report.md"),
			ComparisonJSON:     filepath.Join(resultsDir, "comparison.json"),
			ComparisonMarkdown: filepath.Join(resultsDir, "comparison.md"),
		},
	}
}

func matrixFixtureRun(resultsDir, runID, candidateID, caseID string, exitCode int, failureClass string, findingCount int, severities map[string]int, durationMS int64) benchmarkRun {
	runDir := filepath.Join(resultsDir, runID)
	return benchmarkRun{
		RunID:                 runID,
		CandidateID:           candidateID,
		CaseID:                caseID,
		PRURL:                 "https://github.com/open-cli-collective/codereview-cli/pull/1",
		ExitCode:              exitCode,
		RetryCount:            0,
		FailureClassification: failureClass,
		FindingCount:          findingCount,
		SeverityCounts:        severities,
		DurationMS:            durationMS,
		ReviewRunID:           "review-" + runID,
		Artifacts: runArtifacts{
			Dir:         runDir,
			ReviewJSON:  filepath.Join(runDir, "review.json"),
			Stderr:      filepath.Join(runDir, "stderr.txt"),
			MetricsJSON: filepath.Join(runDir, "metrics.json"),
		},
	}
}

func writeComparisonFixture(t *testing.T, summary benchmarkSuiteSummary) {
	t.Helper()
	if err := os.MkdirAll(summary.ResultsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll results: %v", err)
	}
	if len(summary.Runs) > 0 {
		artifactDir := summary.Runs[0].Artifacts.Dir
		switch {
		case summary.Runs[0].Artifacts.ReviewJSON != "":
			artifactDir = filepath.Dir(summary.Runs[0].Artifacts.ReviewJSON)
		case summary.Runs[0].Artifacts.SelectionJSON != "":
			artifactDir = filepath.Dir(summary.Runs[0].Artifacts.SelectionJSON)
		}
		if artifactDir != "" {
			if err := os.MkdirAll(artifactDir, 0o700); err != nil {
				t.Fatalf("MkdirAll run: %v", err)
			}
		}
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatalf("Marshal summary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(summary.ResultsDir, "suite-summary.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile suite-summary: %v", err)
	}
}

func writeReviewJSON(t *testing.T, path string, review view.ReviewDryRun) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll review dir: %v", err)
	}
	data, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("Marshal review: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile review JSON: %v", err)
	}
}

func assertExactJSONKeys(t *testing.T, data []byte, want []string) {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal JSON object: %v\n%s", err, string(data))
	}
	if len(got) != len(want) {
		t.Fatalf("JSON keys = %#v, want exactly %#v", keysOf(got), want)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON keys = %#v, missing %q", keysOf(got), key)
		}
	}
}

func keysOf(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func benchmarkSuiteWithAnchor(t *testing.T) string {
	t.Helper()
	agentDir := t.TempDir()
	body := `
suite:
  id: suite1
  name: Suite One
  version: 1
candidates:
  - id: first
    profile: home
    stages:
      selection:
        model: sonnet
        effort: high
      reviewers:
        model: sonnet
        effort: high
        agent_dirs:
          - AGENT_DIR
cases:
  - id: case_one
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
    anchors:
      - id: expected-hit
        file: main.go
        side: RIGHT
        lines: [1, 3]
`
	return strings.ReplaceAll(body, "AGENT_DIR", agentDir)
}

func reviewDryRunJSONWithAnchorFinding(t *testing.T, runID, file, side string, line int) []byte {
	t.Helper()
	data, err := json.Marshal(view.ReviewDryRun{
		Run: view.ReviewRun{
			RunID:        runID,
			PRURL:        "https://github.com/open-cli-collective/codereview-cli/pull/1",
			PRKey:        "github.com_open-cli-collective_codereview-cli_1",
			PostMode:     "dry_run",
			Outcome:      "dry_run",
			ArtifactPath: "/tmp/" + runID,
			BaseSHA:      "review-base",
			HeadSHA:      "review-head",
		},
		Findings: []view.ReviewFinding{{
			ID:        "finding-1",
			Severity:  "major",
			FilePath:  file,
			Anchoring: "inline",
			Side:      side,
			Line:      intPtrForBenchmark(line),
			Body:      "body must not be copied to comparison",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal review dry-run: %v", err)
	}
	return data
}
