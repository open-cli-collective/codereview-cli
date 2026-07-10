package benchmarkcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

var (
	openSelectionRuntime = func(ctx context.Context, backend string, backendFlagChanged bool, cfg config.File, profile config.Profile, ref gitprovider.PRRef) (app.SelectionRuntime, error) {
		return app.OpenSelection(ctx, app.SelectionOpenRequest{
			Config:             cfg,
			Profile:            profile,
			Backend:            backend,
			BackendFlagChanged: backendFlagChanged,
			PRRef:              ref,
		})
	}
	runSelectionOnly = pipeline.SelectionOnly
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
	runtime     app.SelectionRuntime
	err         error
}

type selectionRuntimeKey struct {
	profileName string
	host        string
	owner       string
	repo        string
}

type selectionRuntimeResolver func(profileName string, ref gitprovider.PRRef) selectionRuntimeState

func newSelectCommand(opts *root.Options) *cobra.Command {
	var flags selectFlags
	cmd := &cobra.Command{
		Use:   "select <suite.yml>",
		Short: "Run selector-only benchmark suites",
		Args:  exitcode.ExactArgs(1, "benchmark select requires one suite path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			summary, err := runBenchmarkSelectionSuite(cmd.Context(), cmd, opts, flags, args[0])
			if err != nil {
				return err
			}
			return view.Render(opts.Stdout, flags.jsonOutput, summary, func(io.Writer) error {
				return renderSelectionText(opts, summary)
			})
		},
	}
	cmd.Flags().StringArrayVar(&flags.candidates, "candidate", nil, "Candidate ID to run")
	cmd.Flags().StringArrayVar(&flags.cases, "case", nil, "Case ID to run")
	cmd.Flags().StringVar(&flags.resultsDir, "results-dir", "", "Benchmark select output directory; defaults to .cr-bench/results/<suite-id>/select/<timestamp> under the current working directory")
	root.AddJSONFlag(cmd, &flags.jsonOutput)
	return cmd
}

func runBenchmarkSelectionSuite(ctx context.Context, cmd *cobra.Command, opts *root.Options, flags selectFlags, suitePath string) (benchmarkSuiteSummary, error) {
	logger := root.NewProgressLogger(opts)
	suiteSpan := logger.Start("benchmark.select", "load_suite", "suite")
	suite, cfg, err := loadConfigAndSuiteWithValidator(opts, suitePath, benchmark.ValidateForSelection)
	if err != nil {
		return benchmarkSuiteSummary{}, suiteSpan.End(err)
	}
	_ = suiteSpan.End(nil)
	selectSpan := logger.Start("benchmark.select", "select_matrix", suite.Suite.ID)
	selectedCandidates, selectedCases, err := benchmark.Select(suite, flags.candidates, flags.cases)
	if err != nil {
		return benchmarkSuiteSummary{}, selectSpan.End(mapBenchmarkError(err))
	}
	_ = selectSpan.End(nil)
	started := benchmarkNow().UTC()
	resultsSpan := logger.Start("benchmark.select", "prepare_results", suite.Suite.ID)
	resultsDir, err := resolveSelectResultsDir(suite.Suite.ID, flags.resultsDir, started)
	if err != nil {
		return benchmarkSuiteSummary{}, resultsSpan.End(err)
	}
	suiteHash, err := suiteFileSHA256(suite.Path)
	if err != nil {
		return benchmarkSuiteSummary{}, resultsSpan.End(err)
	}
	if err := os.MkdirAll(resultsDir, artifactDirPerm); err != nil {
		return benchmarkSuiteSummary{}, resultsSpan.End(fmt.Errorf("benchmark: create results dir: %w", err))
	}
	_ = resultsSpan.End(nil)

	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	runtimes := map[selectionRuntimeKey]selectionRuntimeState{}
	resolveRuntime := func(profileName string, ref gitprovider.PRRef) selectionRuntimeState {
		runtimeSpan := logger.Start("benchmark.select", "open_runtime", profileName)
		resolvedName, profile, resolveErr := config.ResolveProfile(cfg, profileName)
		state := selectionRuntimeState{
			profileName: resolvedName,
			profile:     profile,
		}
		if resolveErr != nil {
			state.err = cmdruntime.MapRunError(resolveErr)
		}
		key := selectionRuntimeKey{profileName: resolvedName, host: ref.Host, owner: ref.Owner, repo: ref.Repo}
		if resolveErr == nil {
			if state, ok := runtimes[key]; ok {
				_ = runtimeSpan.End(state.err)
				return state
			}
		}
		if resolveErr == nil {
			runtime, runtimeErr := openSelectionRuntime(ctx, opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, profile, ref)
			state.runtime = runtime
			state.err = cmdruntime.MapRunError(runtimeErr)
			if runtimeErr == nil && runtime.Cleanup != nil {
				cleanups = append(cleanups, runtime.Cleanup)
			}
		}
		if resolveErr == nil {
			runtimes[key] = state
		}
		if state.err != nil {
			_ = runtimeSpan.End(state.err)
		} else {
			_ = runtimeSpan.End(nil)
		}
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
		for caseIndex, benchCase := range selectedCases {
			matrixIndex++
			runID := benchmarkRunID(matrixIndex, candidateIndex, caseIndex, candidate, benchCase)
			runSummary, runErr := executeBenchmarkSelectRun(ctx, logger, suiteDir, resultsDir, runID, candidate, benchCase, resolveRuntime)
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
	writeArtifactsSpan := logger.Start("benchmark.select", "write_suite_artifacts", suite.Suite.ID)
	if err := writeSuiteArtifacts(summary); err != nil {
		return benchmarkSuiteSummary{}, writeArtifactsSpan.End(err)
	}
	_ = writeArtifactsSpan.End(nil)
	compareSpan := logger.Start("benchmark.select", "write_comparison", suite.Suite.ID)
	if _, err := writeComparisonArtifactsForResultsDir(summary.ResultsDir); err != nil {
		return benchmarkSuiteSummary{}, compareSpan.End(fmt.Errorf("benchmark: write comparison artifacts after suite artifacts were written to %s; rerun `cr benchmark compare %s`: %w", summary.ResultsDir, summary.ResultsDir, err))
	}
	_ = compareSpan.End(nil)
	return summary, nil
}

func executeBenchmarkSelectRun(ctx context.Context, logger *progress.Logger, suiteDir, resultsDir, runID string, candidate benchmark.Candidate, benchCase benchmark.Case, resolveRuntime selectionRuntimeResolver) (benchmarkRun, error) {
	runSpan := logger.Start("benchmark.select", "execute_run", runID)
	runDir := filepath.Join(resultsDir, runID)
	if err := os.MkdirAll(runDir, artifactDirPerm); err != nil {
		return benchmarkRun{}, runSpan.End(fmt.Errorf("benchmark: create run dir %s: %w", runID, err))
	}
	artifacts, err := selectionRunArtifacts(runDir)
	if err != nil {
		return benchmarkRun{}, runSpan.End(err)
	}
	recipe := benchmarkRunRecipe{
		Candidate: summarizeCandidates(suiteDir, []benchmark.Candidate{candidate})[0],
		Case:      summarizeCases([]benchmark.Case{benchCase})[0],
	}
	if err := writeJSONFile(artifacts.RecipeJSON, recipe); err != nil {
		return benchmarkRun{}, runSpan.End(err)
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
	parseSpan := logger.Start("benchmark.select", "parse_pr", runID)
	ref, err := prref.ParseGitHubPullURL(benchCase.PR)
	if err != nil {
		recordSelectionRunFailure(&runSummary, &stderrBody, err)
		_ = parseSpan.End(err)
		finalized, ferr := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
		return finalized, runSpan.End(ferr)
	}
	_ = parseSpan.End(nil)

	state := resolveRuntime(candidate.Profile, ref)
	if state.err != nil {
		recordSelectionRunFailure(&runSummary, &stderrBody, state.err)
		finalized, err := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
		return finalized, runSpan.End(err)
	}
	postingIdentity, err := benchmarkSelectionPostingIdentity(state.profile)
	if err != nil {
		recordSelectionRunFailure(&runSummary, &stderrBody, err)
		finalized, ferr := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
		return finalized, runSpan.End(ferr)
	}

	promptSpan := logger.Start("benchmark.select", "load_prompt", runID)
	selectionPromptInstructions, err := loadSelectionPromptInstructions(suiteDir, candidate.Stages.Selection.Prompt)
	if err != nil {
		recordSelectionRunFailure(&runSummary, &stderrBody, err)
		_ = promptSpan.End(err)
		finalized, ferr := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
		return finalized, runSpan.End(ferr)
	}
	_ = promptSpan.End(nil)

	selectionSpan := logger.Start("benchmark.select", "selection_pipeline", runID)
	selectionResult, err := runSelectionOnly(ctx, pipeline.Options{
		Provider:  state.runtime.Provider,
		Adapter:   state.runtime.Adapter,
		MaxAgents: candidate.MaxAgents,
	}, pipeline.SelectionRequest{
		PRRef:                       ref,
		ProfileName:                 state.profileName,
		Profile:                     state.profile,
		PostingIdentity:             postingIdentity,
		AgentDirs:                   append([]string(nil), candidate.Stages.Reviewers.AgentDirs...),
		ArtifactDir:                 runDir,
		ReviewBaseSHA:               benchCase.ReviewBaseSHA,
		ReviewHeadSHA:               benchCase.ReviewHeadSHA,
		SelectionModelOverride:      candidate.Stages.Selection.Model,
		SelectionEffortOverride:     candidate.Stages.Selection.Effort,
		SelectionPromptInstructions: selectionPromptInstructions,
	})
	rawSelectionJSON = append([]byte(nil), selectionResult.SelectionSession.Response.StructuredOutput...)
	if selectionResult.ReviewBaseSHA != "" {
		runSummary.ReviewBaseSHA = selectionResult.ReviewBaseSHA
	}
	if selectionResult.ReviewHeadSHA != "" {
		runSummary.ReviewHeadSHA = selectionResult.ReviewHeadSHA
	}
	if selectionResult.CurrentBaseSHA != "" {
		runSummary.CurrentBaseSHA = selectionResult.CurrentBaseSHA
	}
	if selectionResult.CurrentHeadSHA != "" {
		runSummary.CurrentHeadSHA = selectionResult.CurrentHeadSHA
	}
	if err != nil {
		recordSelectionRunFailure(&runSummary, &stderrBody, err)
		_ = selectionSpan.End(err)
		finalized, ferr := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
		return finalized, runSpan.End(ferr)
	}
	_ = selectionSpan.End(nil)

	runSummary.FailureClassification = failureNone
	runSummary.SelectedAgents = summarizeSelectedAgents(selectionResult.Selection.SelectedAgents)
	runSummary.ThreadActionCount = len(selectionResult.Selection.ThreadActions)
	finalized, err := finalizeSelectionRun(runSummary, start, rawSelectionJSON, stderrBody)
	if err != nil {
		return benchmarkRun{}, runSpan.End(err)
	}
	_ = runSpan.End(nil)
	return finalized, nil
}

func finalizeSelectionRun(runSummary benchmarkRun, started time.Time, rawSelectionJSON, stderrBody []byte) (benchmarkRun, error) {
	runSummary.DurationMS = durationMS(benchmarkNow().UTC().Sub(started))
	artifacts := runSummary.Artifacts
	if usage, usageErr := benchmark.ExtractRunMetrics(artifacts.Dir); usageErr != nil {
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

func benchmarkSelectionPostingIdentity(profile config.Profile) (gitprovider.Identity, error) {
	if profile.ReviewerCredentials != nil {
		login := strings.TrimSpace(profile.ReviewerCredentials.IdentityCache)
		if login == "" {
			return gitprovider.Identity{}, fmt.Errorf("benchmark: reviewer identity cache is required for selection; run `cr me` for this profile before benchmark select")
		}
		return gitprovider.Identity{Login: login}, nil
	}
	login := strings.TrimSpace(profile.Git.IdentityCache)
	if login == "" {
		return gitprovider.Identity{}, fmt.Errorf("benchmark: git identity cache is required for selection; run `cr me` for this profile before benchmark select")
	}
	return gitprovider.Identity{Login: login}, nil
}

func recordSelectionRunFailure(runSummary *benchmarkRun, stderrBody *[]byte, err error) {
	runSummary.ExitCode = exitcode.FromError(err)
	runSummary.FailureClassification = classifySelectionFailure(err)
	runSummary.Warnings = append(runSummary.Warnings, err.Error())
	*stderrBody = append(*stderrBody, []byte(err.Error()+"\n")...)
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
	if errors.Is(err, pipeline.ErrStructuredOutputInvalidAfterRetry) {
		return failureInvalidSelectionJSON
	}
	switch exitcode.FromError(err) {
	case exitcode.UsageError:
		return failureUsageError
	case exitcode.AuthConfigError:
		return failureAuthConfigError
	case exitcode.UpstreamError:
		return failureUpstreamError
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
