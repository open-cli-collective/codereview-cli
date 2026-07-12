package benchmarkcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestSelectExecutesSelectedMatrixAndWritesArtifacts(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")

	var (
		runtimeCalls int
		runtimeRefs  []gitprovider.PRRef
		requests     []pipeline.SelectionRequest
	)
	withBenchmarkSelectSeams(t,
		func(_ context.Context, _ string, _ bool, _ config.File, _ config.Profile, ref gitprovider.PRRef) (app.SelectionRuntime, error) {
			runtimeCalls++
			runtimeRefs = append(runtimeRefs, ref)
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, _ pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			requests = append(requests, req)
			artifacts := pipeline.ArtifactPathsFromDir(req.ArtifactDir)
			if err := os.MkdirAll(artifacts.AgentLogsDir, 0o700); err != nil {
				t.Fatalf("MkdirAll agent logs: %v", err)
			}
			if len(requests) == 1 {
				return pipeline.SelectionResult{
					Artifacts:      artifacts,
					ReviewBaseSHA:  "review-base-1",
					ReviewHeadSHA:  "review-head-1",
					CurrentBaseSHA: "current-base-1",
					CurrentHeadSHA: "current-head-1",
					Selection: llm.Selection{
						SelectedAgents: []llm.SelectedAgent{{AgentID: "harness:alpha", Files: []string{"main.go"}}},
						ThreadActions:  []review.ThreadAction{{ThreadID: "thread-1"}},
					},
					SelectionSession: pipeline.SelectionSession{
						ProviderSessionID: "selection-session-1",
						Model:             "claude-sonnet-4-6",
						Effort:            "high",
						Response: llm.Response{
							StructuredOutput: []byte(`{"schema_version":1,"selected_agents":[{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]}],"thread_actions":[],"reasoning":"ok"}`),
						},
					},
				}, nil
			}
			return pipeline.SelectionResult{
				Artifacts:      artifacts,
				ReviewBaseSHA:  "1111111",
				ReviewHeadSHA:  "2222222",
				CurrentBaseSHA: "current-base-2",
				CurrentHeadSHA: "current-head-2",
				SelectionSession: pipeline.SelectionSession{
					ProviderSessionID: "selection-session-2",
					Model:             "claude-sonnet-4-6",
					Effort:            "high",
					Response: llm.Response{
						StructuredOutput: []byte(`{"bad":true}`),
					},
				},
			}, fmt.Errorf("%w: first: unknown selected agent; second: %w", pipeline.ErrStructuredOutputInvalidAfterRetry, fmt.Errorf("unknown selected agent"))
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--results-dir", resultsDir,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Mode != benchmarkModeSelection || got.ResultsDir != resultsDir || got.CRBin != "" || got.RunCount != 2 || got.SuccessCount != 1 || got.FailureCount != 1 {
		t.Fatalf("summary = %#v, want selector-mode 1x2 matrix with one failure", got)
	}
	if runtimeCalls != 1 {
		t.Fatalf("runtime calls = %d, want one shared runtime for the selected profile", runtimeCalls)
	}
	if len(runtimeRefs) != 1 || runtimeRefs[0].Owner != "open-cli-collective" || runtimeRefs[0].Repo != "codereview-cli" {
		t.Fatalf("runtime refs = %#v, want PR repository context", runtimeRefs)
	}
	if got.SelectedCandidates[0].Stages.Selection.Prompt == nil || got.SelectedCandidates[0].Stages.Selection.Prompt.ContentSHA256 == "" {
		t.Fatalf("selection prompt summary = %#v, want prompt provenance hash", got.SelectedCandidates[0].Stages.Selection.Prompt)
	}
	if len(requests) != 2 {
		t.Fatalf("selection requests = %#v, want two matrix executions", requests)
	}
	if requests[0].ProfileName != "home" || requests[0].SelectionModelOverride != "claude-sonnet-4-6" || requests[0].SelectionEffortOverride != "high" {
		t.Fatalf("first request = %#v, want first candidate selection overrides", requests[0])
	}
	if !strings.Contains(requests[0].SelectionPromptInstructions, "Use applies_when when selecting reviewers.") {
		t.Fatalf("first prompt instructions = %q, want loaded prompt body", requests[0].SelectionPromptInstructions)
	}
	if len(requests[0].AgentDirs) != 1 || requests[0].AgentDirs[0] == "" {
		t.Fatalf("first request agent dirs = %#v, want reviewer catalog dirs", requests[0].AgentDirs)
	}
	if !strings.HasPrefix(requests[0].ArtifactDir, resultsDir+string(os.PathSeparator)) || !strings.HasSuffix(requests[0].ArtifactDir, filepath.Join("0001-c01-k01-first-case_one")) {
		t.Fatalf("first artifact dir = %q, want selector run rooted under explicit results dir", requests[0].ArtifactDir)
	}
	if !strings.HasPrefix(requests[1].ArtifactDir, resultsDir+string(os.PathSeparator)) || !strings.HasSuffix(requests[1].ArtifactDir, filepath.Join("0002-c01-k02-first-case_two")) {
		t.Fatalf("second artifact dir = %q, want selector run rooted under explicit results dir", requests[1].ArtifactDir)
	}
	if requests[1].ReviewBaseSHA != "1111111" || requests[1].ReviewHeadSHA != "2222222" {
		t.Fatalf("second request = %#v, want pinned review SHAs", requests[1])
	}
	if got.Runs[0].FailureClassification != failureNone || len(got.Runs[0].SelectedAgents) != 1 || got.Runs[0].SelectedAgents[0].AgentID != "harness:alpha" || got.Runs[0].ThreadActionCount != 1 {
		t.Fatalf("first run = %#v, want selected reviewer and thread action", got.Runs[0])
	}
	if got.Runs[0].Artifacts.ReviewJSON != "" {
		t.Fatalf("first run review json path = %q, want none for selector benchmark", got.Runs[0].Artifacts.ReviewJSON)
	}
	if got.Runs[0].Artifacts.SelectionLog == "" || got.Runs[0].Artifacts.SelectionJSON == "" || got.Runs[0].Artifacts.RecipeJSON == "" {
		t.Fatalf("first run artifacts = %#v, want selector-owned artifact paths", got.Runs[0].Artifacts)
	}
	if got.Runs[1].FailureClassification != failureInvalidSelectionJSON || len(got.Runs[1].SelectedAgents) != 0 {
		t.Fatalf("second run = %#v, want invalid-selection failure with no selected reviewers", got.Runs[1])
	}
	assertFileContains(t, got.Artifacts.SuiteSummary, `"mode": "selection"`)
	assertFileContains(t, got.Artifacts.Manifest, `"mode": "selection"`)
	assertFileContains(t, got.Artifacts.Report, "Selected Reviewers")
	assertFileContains(t, got.Runs[0].Artifacts.SelectionJSON, `"agent_id":"harness:alpha"`)
	assertFileContains(t, got.Runs[1].Artifacts.SelectionJSON, `{"bad":true}`)
	assertFileContains(t, got.Runs[1].Artifacts.Stderr, "structured output invalid after retry")
	assertFileContains(t, got.Runs[0].Artifacts.RecipeJSON, `"candidate"`)
	assertFileContains(t, got.Runs[0].Artifacts.RecipeJSON, `"case"`)
}

func TestSelectProgressWritesToStderr(t *testing.T) {
	cmd, out, errOut := newTestCommandWithStderr(t, false)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, _ pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			artifacts := pipeline.ArtifactPathsFromDir(req.ArtifactDir)
			if err := os.MkdirAll(artifacts.AgentLogsDir, 0o700); err != nil {
				t.Fatalf("MkdirAll agent logs: %v", err)
			}
			return pipeline.SelectionResult{
				Artifacts: artifacts,
				Selection: llm.Selection{
					SelectedAgents: []llm.SelectedAgent{{AgentID: "harness:alpha"}},
				},
				SelectionSession: pipeline.SelectionSession{
					Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"selected_agents":[{"agent_id":"harness:alpha"}],"thread_actions":[]}`)},
				},
			}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(errOut.String(), `command="benchmark.select" op="selection_pipeline"`) {
		t.Fatalf("stderr = %q, want selection_pipeline progress", errOut.String())
	}
	if !strings.Contains(errOut.String(), `command="benchmark.select" op="write_comparison"`) {
		t.Fatalf("stderr = %q, want write_comparison progress", errOut.String())
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
}

func TestSelectRecordsRuntimeFailuresWithoutInvokingSelection(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))

	var selectionCalls int
	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{}, fmt.Errorf("runtime open failed")
		},
		func(context.Context, pipeline.Options, pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			selectionCalls++
			return pipeline.SelectionResult{}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if selectionCalls != 0 {
		t.Fatalf("selection calls = %d, want none after runtime failure", selectionCalls)
	}
	if got.RunCount != 1 || got.SuccessCount != 0 || got.FailureCount != 1 || got.Runs[0].FailureClassification != failureSelectionError {
		t.Fatalf("summary = %#v, want one recorded selector failure", got)
	}
	assertFileContains(t, got.Runs[0].Artifacts.Stderr, "runtime open failed")
	if got.Runs[0].Artifacts.ReviewJSON != "" {
		t.Fatalf("review json path = %q, want none for selector runtime failure", got.Runs[0].Artifacts.ReviewJSON)
	}
}

func TestSelectMapsRuntimeOpenErrorsAtCommandBoundary(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{}, fmt.Errorf("runtime open failed: %w", config.ErrInvalid)
		},
		func(context.Context, pipeline.Options, pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			t.Fatal("selection should not run after runtime open failure")
			return pipeline.SelectionResult{}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Runs[0].ExitCode != exitcode.UsageError || got.Runs[0].FailureClassification != failureUsageError {
		t.Fatalf("run exit/class = %d/%s, want usage/%s", got.Runs[0].ExitCode, got.Runs[0].FailureClassification, failureUsageError)
	}
	assertFileContains(t, got.Runs[0].Artifacts.Stderr, "runtime open failed")
}

func TestSelectSupportsCandidateWithoutReviewerStage(t *testing.T) {
	cmd, out := newTestCommand(t)
	body := `
suite:
  id: suite1
candidates:
  - id: first
    profile: home
    stages:
      selection:
        model: claude-sonnet-4-6
        effort: high
cases:
  - id: case_one
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`
	suitePath := writeBenchmarkSuite(t, body)

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, _ pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			if len(req.AgentDirs) != 0 {
				t.Fatalf("agent dirs = %#v, want none for selector-only candidate", req.AgentDirs)
			}
			return pipeline.SelectionResult{
				Artifacts: pipeline.ArtifactPathsFromDir(req.ArtifactDir),
			}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.RunCount != 1 || got.SuccessCount != 1 || got.FailureCount != 0 {
		t.Fatalf("summary = %#v, want successful selector-only run without reviewer stage", got)
	}
}

func TestSelectRecipePreservesOptionalSynthesisStage(t *testing.T) {
	cmd, out := newTestCommand(t)
	synthesisPrompt := filepath.Join(t.TempDir(), "synthesis-v1.md")
	if err := os.WriteFile(synthesisPrompt, []byte("Summarize reviewer findings."), 0o600); err != nil {
		t.Fatalf("WriteFile synthesis prompt: %v", err)
	}
	suitePath := writeBenchmarkSuite(t, withBenchmarkSynthesisStage(validBenchmarkSuite(t), synthesisPrompt))

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, _ pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			return pipeline.SelectionResult{
				Artifacts: pipeline.ArtifactPathsFromDir(req.ArtifactDir),
				SelectionSession: pipeline.SelectionSession{
					Response: llm.Response{
						StructuredOutput: []byte(`{"schema_version":1,"selected_agents":[],"thread_actions":[],"reasoning":"ok"}`),
					},
				},
			}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	var recipe benchmarkRunRecipe
	if data, err := os.ReadFile(got.Runs[0].Artifacts.RecipeJSON); err != nil {
		t.Fatalf("ReadFile recipe: %v", err)
	} else if err := json.Unmarshal(data, &recipe); err != nil {
		t.Fatalf("Unmarshal recipe: %v", err)
	}
	if recipe.Candidate.Stages.Synthesis == nil ||
		recipe.Candidate.Stages.Synthesis.Model != "claude-opus-4-8" ||
		recipe.Candidate.Stages.Synthesis.Prompt == nil ||
		recipe.Candidate.Stages.Synthesis.Prompt.ContentSHA256 == "" {
		t.Fatalf("recipe synthesis stage = %#v, want preserved synthesis prompt metadata", recipe.Candidate.Stages.Synthesis)
	}
}

func TestSelectRecordsPerRunFailureWhenSelectorExceedsCandidateMaxAgents(t *testing.T) {
	cmd, out := newTestCommand(t)
	body := strings.Replace(validBenchmarkSuite(t), "    max_agents: 5", "    max_agents: 1", 1)
	suitePath := writeBenchmarkSuite(t, body)

	withBenchmarkSelectSeams(t,
		func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error) {
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(_ context.Context, opts pipeline.Options, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			if opts.MaxAgents != 1 {
				t.Fatalf("pipeline max agents = %d, want candidate max_agents 1", opts.MaxAgents)
			}
			artifacts := pipeline.ArtifactPathsFromDir(req.ArtifactDir)
			return pipeline.SelectionResult{
				Artifacts: artifacts,
				SelectionSession: pipeline.SelectionSession{
					ProviderSessionID: "selection-session-over-cap",
					Response: llm.Response{
						StructuredOutput: []byte(`{"schema_version":1,"selected_agents":[{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]},{"agent_id":"harness:beta","rationale":"main","files":["main.go"]}],"thread_actions":[],"reasoning":"too many"}`),
					},
				},
			}, fmt.Errorf("pipeline: selected agents 2 exceeds max 1")
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.RunCount != 1 || got.SuccessCount != 0 || got.FailureCount != 1 {
		t.Fatalf("summary = %#v, want one recorded over-cap selector failure", got)
	}
	if got.Runs[0].FailureClassification != failureSelectionError {
		t.Fatalf("run failure classification = %q, want generic selector failure", got.Runs[0].FailureClassification)
	}
	assertFileContains(t, got.Runs[0].Artifacts.SelectionJSON, `"harness:beta"`)
	assertFileContains(t, got.Runs[0].Artifacts.Stderr, "selected agents 2 exceeds max 1")
}

func TestSelectRecordsPromptReadFailuresPerRun(t *testing.T) {
	cmd, out := newTestCommand(t)
	promptPath := filepath.Join(t.TempDir(), "selection.md")
	if err := os.WriteFile(promptPath, []byte("Use applies_when when selecting reviewers."), 0o600); err != nil {
		t.Fatalf("WriteFile prompt: %v", err)
	}
	body := fmt.Sprintf(`
suite:
  id: suite1
candidates:
  - id: first
    profile: home
    stages:
      selection:
        model: claude-sonnet-4-6
        effort: high
        prompt: %s
cases:
  - id: case_one
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`, promptPath)
	suitePath := writeBenchmarkSuite(t, body)
	var selectionCalls int

	withBenchmarkSelectSeams(t,
		func(_ context.Context, _ string, _ bool, _ config.File, profile config.Profile, _ gitprovider.PRRef) (app.SelectionRuntime, error) {
			if len(profile.AgentSources) != 0 {
				t.Fatalf("profile = %#v, expected test config profile", profile)
			}
			_ = os.Remove(promptPath)
			return app.SelectionRuntime{Cleanup: func() {}}, nil
		},
		func(context.Context, pipeline.Options, pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
			selectionCalls++
			return pipeline.SelectionResult{}, nil
		},
	)

	if err := root.Execute(cmd, []string{
		"benchmark", "select", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if selectionCalls != 0 {
		t.Fatalf("selection calls = %d, want prompt read failure before selection execution", selectionCalls)
	}
	if got.RunCount != 1 || got.FailureCount != 1 || got.Runs[0].FailureClassification != failureSelectionError {
		t.Fatalf("summary = %#v, want recorded prompt read failure", got)
	}
	assertFileContains(t, got.Runs[0].Artifacts.Stderr, "read selection prompt")
}

func withBenchmarkSelectSeams(
	t *testing.T,
	runtimeOpener func(context.Context, string, bool, config.File, config.Profile, gitprovider.PRRef) (app.SelectionRuntime, error),
	runner func(context.Context, pipeline.Options, pipeline.SelectionRequest) (pipeline.SelectionResult, error),
) {
	t.Helper()
	oldNow := benchmarkNow
	oldOpener := openSelectionRuntime
	benchmarkNow = func() time.Time { return fixedBenchmarkTime() }
	openSelectionRuntime = func(ctx context.Context, backend string, changed bool, cfg config.File, profile config.Profile, ref gitprovider.PRRef) (app.SelectionRuntime, error) {
		runtime, err := runtimeOpener(ctx, backend, changed, cfg, profile, ref)
		if err == nil {
			runtime.Select = func(ctx context.Context, req pipeline.SelectionRequest) (pipeline.SelectionResult, error) {
				return runner(ctx, pipeline.Options{MaxAgents: req.MaxAgents}, req)
			}
		}
		return runtime, err
	}
	t.Cleanup(func() {
		benchmarkNow = oldNow
		openSelectionRuntime = oldOpener
	})
}

func TestSelectionReportMarkdownListsSelectedReviewers(t *testing.T) {
	report := renderSelectionReportMarkdown(benchmarkSuiteSummary{
		SuiteID:      "suite1",
		ResultsDir:   "/tmp/results",
		RunCount:     1,
		SuccessCount: 1,
		Runs: []benchmarkRun{{
			RunID:                 "run1",
			CandidateID:           "candidate1",
			CaseID:                "case1",
			FailureClassification: failureNone,
			SelectedAgents:        []benchmarkSelectedAgent{{AgentID: "harness:alpha"}},
		}},
	})
	if !strings.Contains(report, "Selected Reviewers") || !strings.Contains(report, "`harness:alpha`") {
		t.Fatalf("report = %s, want selected reviewer table", report)
	}
}
