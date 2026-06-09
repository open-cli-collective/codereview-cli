package benchmarkcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/reviewcmd"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
)

var (
	openSelectionRuntime = reviewcmd.OpenSelectionRuntime
	runSelectionOnly     = pipeline.SelectionOnly
)

type selectFlags struct {
	candidates []string
	cases      []string
	resultsDir string
	jsonOutput bool
}

type selectionRuntimeState struct {
	profileName string
	profile     config.Profile
	runtime     reviewcmd.SelectionRuntime
	err         error
}

func newSelectCommand(opts *root.Options) *cobra.Command {
	var flags selectFlags
	cmd := &cobra.Command{
		Use:   "select <suite.yml>",
		Short: "Run selector-only benchmark suites",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("benchmark select requires one suite path"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			summary, err := runBenchmarkSelectionSuite(cmd.Context(), cmd, opts, flags, args[0])
			if err != nil {
				return err
			}
			if flags.jsonOutput {
				enc := json.NewEncoder(opts.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}
			return renderSelectionText(opts, summary)
		},
	}
	cmd.Flags().StringArrayVar(&flags.candidates, "candidate", nil, "Candidate ID to run")
	cmd.Flags().StringArrayVar(&flags.cases, "case", nil, "Case ID to run")
	cmd.Flags().StringVar(&flags.resultsDir, "results-dir", "", "Benchmark select output directory; defaults to .cr-bench/results/<suite-id>/select/<timestamp> under the current working directory")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	return cmd
}

func runBenchmarkSelectionSuite(ctx context.Context, cmd *cobra.Command, opts *root.Options, flags selectFlags, suitePath string) (benchmarkSuiteSummary, error) {
	suite, cfg, err := loadConfigAndSuiteWithValidator(opts, suitePath, benchmark.ValidateForSelection)
	if err != nil {
		return benchmarkSuiteSummary{}, err
	}
	selectedCandidates, selectedCases, err := benchmark.Select(suite, flags.candidates, flags.cases)
	if err != nil {
		return benchmarkSuiteSummary{}, mapBenchmarkError(err)
	}
	started := benchmarkNow().UTC()
	resultsDir, err := resolveSelectResultsDir(suite.Suite.ID, flags.resultsDir, started)
	if err != nil {
		return benchmarkSuiteSummary{}, err
	}
	suiteHash, err := suiteFileSHA256(suite.Path)
	if err != nil {
		return benchmarkSuiteSummary{}, err
	}
	if err := os.MkdirAll(resultsDir, artifactDirPerm); err != nil {
		return benchmarkSuiteSummary{}, fmt.Errorf("benchmark: create results dir: %w", err)
	}

	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	runtimes := map[string]selectionRuntimeState{}
	resolveRuntime := func(profileName string) selectionRuntimeState {
		if state, ok := runtimes[profileName]; ok {
			return state
		}
		resolvedName, profile, resolveErr := config.ResolveProfile(cfg, profileName)
		state := selectionRuntimeState{
			profileName: resolvedName,
			profile:     profile,
			err:         resolveErr,
		}
		if resolveErr == nil {
			runtime, runtimeErr := openSelectionRuntime(ctx, opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, profile)
			state.runtime = runtime
			state.err = runtimeErr
			if runtimeErr == nil && runtime.Cleanup != nil {
				cleanups = append(cleanups, runtime.Cleanup)
			}
		}
		runtimes[profileName] = state
		return state
	}

	suiteDir := filepath.Dir(suite.Path)
	summary := benchmarkSuiteSummary{
		SchemaVersion:      benchmarkArtifactSchemaVersion,
		Mode:               benchmarkModeSelection,
		SuiteID:            suite.Suite.ID,
		SuitePath:          suite.Path,
		SuiteSHA256:        suiteHash,
		StartedAt:          started.Format(time.RFC3339),
		ResultsDir:         resultsDir,
		SelectedCandidates: summarizeCandidates(suiteDir, selectedCandidates),
		SelectedCases:      summarizeCases(selectedCases),
		SeverityCounts:     map[string]int{},
		Artifacts: suiteArtifacts{
			Manifest:           filepath.Join(resultsDir, "manifest.json"),
			SummaryJSONL:       filepath.Join(resultsDir, "summary.jsonl"),
			SuiteSummary:       filepath.Join(resultsDir, "suite-summary.json"),
			Report:             filepath.Join(resultsDir, "report.md"),
			ComparisonJSON:     filepath.Join(resultsDir, "comparison.json"),
			ComparisonMarkdown: filepath.Join(resultsDir, "comparison.md"),
		},
	}

	matrixIndex := 0
	for candidateIndex, candidate := range selectedCandidates {
		state := resolveRuntime(candidate.Profile)
		for caseIndex, benchCase := range selectedCases {
			matrixIndex++
			runID := benchmarkRunID(matrixIndex, candidateIndex, caseIndex, candidate, benchCase)
			runSummary, runErr := executeBenchmarkSelectRun(ctx, suiteDir, resultsDir, runID, candidate, benchCase, state)
			if runErr != nil {
				return benchmarkSuiteSummary{}, runErr
			}
			summary.Runs = append(summary.Runs, runSummary)
			summary.RunCount++
			if runSummary.ExitCode == exitcode.Success {
				summary.SuccessCount++
			} else {
				summary.FailureCount++
			}
			if runSummary.Usage != nil {
				mergeUsage(&summary, *runSummary.Usage)
			}
		}
	}

	completed := benchmarkNow().UTC()
	summary.CompletedAt = completed.Format(time.RFC3339)
	summary.DurationMS = durationMS(completed.Sub(started))
	if err := writeSuiteArtifacts(summary); err != nil {
		return benchmarkSuiteSummary{}, err
	}
	if _, err := writeComparisonArtifactsForResultsDir(summary.ResultsDir); err != nil {
		return benchmarkSuiteSummary{}, fmt.Errorf("benchmark: write comparison artifacts after suite artifacts were written to %s; rerun `cr benchmark compare %s`: %w", summary.ResultsDir, summary.ResultsDir, err)
	}
	return summary, nil
}

func executeBenchmarkSelectRun(ctx context.Context, suiteDir, resultsDir, runID string, candidate benchmark.Candidate, benchCase benchmark.Case, state selectionRuntimeState) (benchmarkRun, error) {
	runDir := filepath.Join(resultsDir, runID)
	if err := os.MkdirAll(runDir, artifactDirPerm); err != nil {
		return benchmarkRun{}, fmt.Errorf("benchmark: create run dir %s: %w", runID, err)
	}
	artifacts, err := selectionRunArtifacts(runDir)
	if err != nil {
		return benchmarkRun{}, err
	}
	recipe := benchmarkRunRecipe{
		Candidate: summarizeCandidates(suiteDir, []benchmark.Candidate{candidate})[0],
		Case:      summarizeCases([]benchmark.Case{benchCase})[0],
	}
	if err := writeJSONFile(artifacts.RecipeJSON, recipe); err != nil {
		return benchmarkRun{}, err
	}

	runSummary := benchmarkRun{
		RunID:                  runID,
		CandidateID:            candidate.ID,
		CaseID:                 benchCase.ID,
		PRURL:                  benchCase.PR,
		RequestedReviewBaseSHA: benchCase.ReviewBaseSHA,
		RequestedReviewHeadSHA: benchCase.ReviewHeadSHA,
		ExpectedBaseSHA:        benchCase.ExpectedBaseSHA,
		ExpectedHeadSHA:        benchCase.ExpectedHeadSHA,
		ExitCode:               exitcode.Success,
		RetryCount:             0,
		SeverityCounts:         map[string]int{},
		Warnings:               []string{},
		Artifacts:              artifacts,
	}

	start := benchmarkNow().UTC()
	var (
		stderrBody       []byte
		rawSelectionJSON []byte
	)
	if state.err != nil {
		runSummary.ExitCode = exitcode.Failure
		runSummary.FailureClassification = classifySelectionFailure(state.err)
		runSummary.Warnings = append(runSummary.Warnings, state.err.Error())
		stderrBody = append(stderrBody, []byte(state.err.Error()+"\n")...)
	} else {
		selectionPromptInstructions, promptErr := loadSelectionPromptInstructions(suiteDir, candidate.Stages.Selection.Prompt)
		if promptErr != nil {
			runSummary.ExitCode = exitcode.Failure
			runSummary.FailureClassification = classifySelectionFailure(promptErr)
			runSummary.Warnings = append(runSummary.Warnings, promptErr.Error())
			stderrBody = append(stderrBody, []byte(promptErr.Error()+"\n")...)
		} else {
			ref, parseErr := prref.ParseGitHubPullURL(benchCase.PR)
			if parseErr != nil {
				runSummary.ExitCode = exitcode.Failure
				runSummary.FailureClassification = classifySelectionFailure(parseErr)
				runSummary.Warnings = append(runSummary.Warnings, parseErr.Error())
				stderrBody = append(stderrBody, []byte(parseErr.Error()+"\n")...)
			} else {
				result, runErr := runSelectionOnly(ctx, pipeline.Options{
					Provider: state.runtime.Provider,
					Adapter:  state.runtime.Adapter,
				}, pipeline.SelectionRequest{
					PRRef:                       ref,
					ProfileName:                 state.profileName,
					Profile:                     state.profile,
					AgentDirs:                   append([]string(nil), candidate.Stages.Reviewers.AgentDirs...),
					ArtifactDir:                 runDir,
					ReviewBaseSHA:               benchCase.ReviewBaseSHA,
					ReviewHeadSHA:               benchCase.ReviewHeadSHA,
					SelectionModelOverride:      candidate.Stages.Selection.Model,
					SelectionEffortOverride:     candidate.Stages.Selection.Effort,
					SelectionPromptInstructions: selectionPromptInstructions,
				})
				rawSelectionJSON = append([]byte(nil), result.SelectionSession.Response.StructuredOutput...)
				if result.ReviewBaseSHA != "" {
					runSummary.ReviewBaseSHA = result.ReviewBaseSHA
				}
				if result.ReviewHeadSHA != "" {
					runSummary.ReviewHeadSHA = result.ReviewHeadSHA
				}
				if result.CurrentBaseSHA != "" {
					runSummary.CurrentBaseSHA = result.CurrentBaseSHA
				}
				if result.CurrentHeadSHA != "" {
					runSummary.CurrentHeadSHA = result.CurrentHeadSHA
				}
				if runErr != nil {
					runSummary.ExitCode = exitcode.Failure
					runSummary.FailureClassification = classifySelectionFailure(runErr)
					runSummary.Warnings = append(runSummary.Warnings, runErr.Error())
					stderrBody = append(stderrBody, []byte(runErr.Error()+"\n")...)
				} else {
					runSummary.FailureClassification = failureNone
					runSummary.SelectedAgents = summarizeSelectedAgents(result.Selection.SelectedAgents)
					runSummary.ThreadActionCount = len(result.Selection.ThreadActions)
				}
			}
		}
	}
	runSummary.DurationMS = durationMS(benchmarkNow().UTC().Sub(start))
	if usage, usageErr := benchmark.ExtractRunMetrics(runDir); usageErr != nil {
		runSummary.Warnings = append(runSummary.Warnings, fmt.Sprintf("usage metrics unavailable: %s", usageErr.Error()))
	} else if usage.HasData() {
		runSummary.Usage = &usage
	}
	if err := writeArtifactFile(artifacts.SelectionJSON, rawSelectionJSON); err != nil {
		return benchmarkRun{}, err
	}
	if err := writeArtifactFile(artifacts.Stderr, stderrBody); err != nil {
		return benchmarkRun{}, err
	}
	if err := writeJSONFile(artifacts.MetricsJSON, runSummary); err != nil {
		return benchmarkRun{}, err
	}
	return runSummary, nil
}

func resolveSelectResultsDir(suiteID, configured string, started time.Time) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		path = filepath.Join(".cr-bench", "results", suiteID, "select", started.UTC().Format(runTimestampLayout))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("benchmark: resolve select results dir: %w", err)
	}
	return abs, nil
}

func selectionRunArtifacts(runDir string) (runArtifacts, error) {
	selectionLog, err := pipeline.ArtifactPathsFromDir(runDir).AgentLog("orchestrator-selection")
	if err != nil {
		return runArtifacts{}, err
	}
	return runArtifacts{
		Dir:           runDir,
		SelectionJSON: filepath.Join(runDir, "selection.json"),
		SelectionLog:  selectionLog,
		RecipeJSON:    filepath.Join(runDir, "recipe.json"),
		Stderr:        filepath.Join(runDir, "stderr.txt"),
		MetricsJSON:   filepath.Join(runDir, "metrics.json"),
	}, nil
}

func loadSelectionPromptInstructions(suiteDir, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", nil
	}
	resolved := resolveStagePath(suiteDir, configured)
	data, err := os.ReadFile(resolved) // #nosec G304 -- validated benchmark prompt path is explicit suite input.
	if err != nil {
		return "", fmt.Errorf("benchmark: read selection prompt %q: %w", configured, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func summarizeSelectedAgents(selected []llm.SelectedAgent) []benchmarkSelectedAgent {
	out := make([]benchmarkSelectedAgent, 0, len(selected))
	for _, agent := range selected {
		out = append(out, benchmarkSelectedAgent{
			AgentID: agent.AgentID,
			Files:   append([]string(nil), agent.Files...),
		})
	}
	return out
}

func classifySelectionFailure(err error) string {
	if err == nil {
		return failureNone
	}
	if strings.Contains(err.Error(), "structured output invalid after retry") {
		return failureInvalidSelectionJSON
	}
	return failureSelectionError
}

func renderSelectionText(opts *root.Options, summary benchmarkSuiteSummary) error {
	if _, err := fmt.Fprintf(opts.Stdout, "Benchmark suite: %s\n", summary.SuiteID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Results dir: %s\n", summary.ResultsDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Runs: %d success=%d failure=%d\n", summary.RunCount, summary.SuccessCount, summary.FailureCount); err != nil {
		return err
	}
	for _, run := range summary.Runs {
		if _, err := fmt.Fprintf(opts.Stdout, "- %s candidate=%s case=%s exit=%d selected_reviewers=%s\n", run.RunID, run.CandidateID, run.CaseID, run.ExitCode, selectedAgentsCell(run.SelectedAgents)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(opts.Stdout, "Artifacts:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "  Manifest: %s\n", summary.Artifacts.Manifest); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "  Summary: %s\n", summary.Artifacts.SuiteSummary); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "  JSONL: %s\n", summary.Artifacts.SummaryJSONL); err != nil {
		return err
	}
	_, err := fmt.Fprintf(opts.Stdout, "  Report: %s\n", summary.Artifacts.Report)
	return err
}

func renderSelectionReportMarkdown(summary benchmarkSuiteSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Selector Benchmark Report: %s\n\n", summary.SuiteID)
	fmt.Fprintf(&b, "- Results dir: `%s`\n", summary.ResultsDir)
	fmt.Fprintf(&b, "- Runs: %d\n", summary.RunCount)
	fmt.Fprintf(&b, "- Success: %d\n", summary.SuccessCount)
	fmt.Fprintf(&b, "- Failure: %d\n", summary.FailureCount)
	if summary.Usage != nil && summary.Usage.HasTokenUsage() {
		fmt.Fprintf(&b, "- Tokens: %d total (%d input, %d output, %d cache read, %d cache write)\n", summary.Usage.Tokens.TotalTokens, summary.Usage.Tokens.Input, summary.Usage.Tokens.Output, summary.Usage.Tokens.CacheRead, summary.Usage.Tokens.CacheWrite)
	}
	if summary.Usage != nil && summary.Usage.HasCostUsage() {
		fmt.Fprintf(&b, "- Cost: $%.6f\n", summary.Usage.Cost.Total)
	}
	b.WriteString("\n")
	b.WriteString("| Run | Candidate | Case | Exit | Failure | Selected Reviewers | Thread Actions | Tokens | Cost |\n")
	b.WriteString("| --- | --- | --- | ---: | --- | --- | ---: | ---: | ---: |\n")
	for _, run := range summary.Runs {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d | %s | %s | %d | %s | %s |\n",
			run.RunID,
			markdownCell(run.CandidateID),
			markdownCell(run.CaseID),
			run.ExitCode,
			markdownCell(run.FailureClassification),
			selectedAgentsMarkdownCell(run.SelectedAgents),
			run.ThreadActionCount,
			usageTokensCell(run.Usage),
			usageCostCell(run.Usage),
		)
	}
	return b.String()
}

func selectedAgentsCell(agents []benchmarkSelectedAgent) string {
	ids := selectedAgentIDs(agents)
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ",")
}

func selectedAgentsMarkdownCell(agents []benchmarkSelectedAgent) string {
	ids := selectedAgentIDs(agents)
	if len(ids) == 0 {
		return "none"
	}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, "`"+markdownCode(id)+"`")
	}
	return strings.Join(values, ", ")
}

func selectedAgentIDs(agents []benchmarkSelectedAgent) []string {
	out := make([]string, 0, len(agents))
	for _, agent := range agents {
		out = append(out, agent.AgentID)
	}
	return out
}
