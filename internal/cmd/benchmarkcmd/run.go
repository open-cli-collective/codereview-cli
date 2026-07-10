package benchmarkcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const (
	benchmarkArtifactSchemaVersion = 2
	benchmarkModeReview            = "review"
	benchmarkModeSelection         = "selection"
	runTimestampLayout             = "2006-01-02T150405Z"
	artifactDirPerm                = 0o700
	artifactFilePerm               = 0o600
)

type reviewCommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	Err      error
}

var (
	// Test seams for this package. Tests that mutate these must not run in
	// parallel with other benchmarkcmd tests.
	benchmarkNow     = func() time.Time { return time.Now().UTC() }
	runReviewCommand = runReviewCommandReal
)

type benchmarkSuiteSummary struct {
	SchemaVersion      int                   `json:"schema_version"`
	Mode               string                `json:"mode,omitempty"`
	SuiteID            string                `json:"suite_id"`
	SuitePath          string                `json:"suite_path"`
	SuiteSHA256        string                `json:"suite_sha256"`
	StartedAt          string                `json:"started_at"`
	CompletedAt        string                `json:"completed_at"`
	ResultsDir         string                `json:"results_dir"`
	CRBin              string                `json:"cr_bin"`
	SelectedCandidates []benchmarkCandidate  `json:"selected_candidates"`
	SelectedCases      []benchmarkCase       `json:"selected_cases"`
	RunCount           int                   `json:"run_count"`
	SuccessCount       int                   `json:"success_count"`
	FailureCount       int                   `json:"failure_count"`
	DurationMS         int64                 `json:"duration_ms"`
	SeverityCounts     map[string]int        `json:"severity_counts"`
	Usage              *benchmark.RunMetrics `json:"usage,omitempty"`
	Runs               []benchmarkRun        `json:"runs"`
	Artifacts          suiteArtifacts        `json:"artifacts"`
}

type benchmarkManifest struct {
	SchemaVersion      int                    `json:"schema_version"`
	Mode               string                 `json:"mode,omitempty"`
	SuiteID            string                 `json:"suite_id"`
	SuitePath          string                 `json:"suite_path"`
	SuiteSHA256        string                 `json:"suite_sha256"`
	StartedAt          string                 `json:"started_at"`
	CompletedAt        string                 `json:"completed_at"`
	ResultsDir         string                 `json:"results_dir"`
	CRBin              string                 `json:"cr_bin"`
	SelectedCandidates []benchmarkCandidate   `json:"selected_candidates"`
	SelectedCases      []benchmarkCase        `json:"selected_cases"`
	Runs               []benchmarkManifestRun `json:"runs"`
}

type benchmarkManifestRun struct {
	RunID       string       `json:"run_id"`
	CandidateID string       `json:"candidate_id"`
	CaseID      string       `json:"case_id"`
	Artifacts   runArtifacts `json:"artifacts"`
}

type benchmarkCandidate struct {
	ID             string                   `json:"id"`
	Profile        string                   `json:"profile"`
	Stages         benchmarkCandidateStages `json:"stages"`
	MaxAgents      int                      `json:"max_agents,omitempty"`
	MaxConcurrency int                      `json:"max_concurrency,omitempty"`
}

type benchmarkCandidateStages struct {
	Selection benchmarkSelectionStage  `json:"selection"`
	Reviewers benchmarkReviewerStage   `json:"reviewers,omitempty"`
	Synthesis *benchmarkSynthesisStage `json:"synthesis,omitempty"`
}

type benchmarkSelectionStage struct {
	Model  string               `json:"model,omitempty"`
	Effort string               `json:"effort,omitempty"`
	Prompt *benchmarkPromptFile `json:"prompt,omitempty"`
}

type benchmarkSynthesisStage = benchmarkSelectionStage

type benchmarkReviewerStage struct {
	Model     string              `json:"model,omitempty"`
	ModelTier string              `json:"model_tier,omitempty"`
	Effort    string              `json:"effort,omitempty"`
	AgentDirs []benchmarkAgentDir `json:"agent_dirs,omitempty"`
}

type benchmarkPromptFile struct {
	Configured    string `json:"configured"`
	Resolved      string `json:"resolved"`
	Exists        bool   `json:"exists"`
	IsDir         bool   `json:"is_dir"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	Warning       string `json:"warning,omitempty"`
}

type benchmarkAgentDir struct {
	Configured      string `json:"configured"`
	Resolved        string `json:"resolved"`
	Exists          bool   `json:"exists"`
	IsDir           bool   `json:"is_dir"`
	DirMetadataHash string `json:"dir_metadata_hash,omitempty"`
	Warning         string `json:"warning,omitempty"`
}

type benchmarkCase struct {
	ID              string             `json:"id"`
	PR              string             `json:"pr"`
	ReviewBaseSHA   string             `json:"review_base_sha,omitempty"`
	ReviewHeadSHA   string             `json:"review_head_sha,omitempty"`
	ExpectedBaseSHA string             `json:"expected_base_sha,omitempty"`
	ExpectedHeadSHA string             `json:"expected_head_sha,omitempty"`
	Anchors         []benchmark.Anchor `json:"anchors,omitempty"`
}

type benchmarkRun struct {
	RunID                  string                   `json:"run_id"`
	CandidateID            string                   `json:"candidate_id"`
	CaseID                 string                   `json:"case_id"`
	PRURL                  string                   `json:"pr_url"`
	RequestedReviewBaseSHA string                   `json:"requested_review_base_sha,omitempty"`
	RequestedReviewHeadSHA string                   `json:"requested_review_head_sha,omitempty"`
	ExpectedBaseSHA        string                   `json:"expected_base_sha,omitempty"`
	ExpectedHeadSHA        string                   `json:"expected_head_sha,omitempty"`
	ReviewRunID            string                   `json:"review_run_id,omitempty"`
	ReviewArtifactPath     string                   `json:"review_artifact_path,omitempty"`
	ReviewBaseSHA          string                   `json:"review_base_sha,omitempty"`
	ReviewHeadSHA          string                   `json:"review_head_sha,omitempty"`
	CurrentBaseSHA         string                   `json:"current_base_sha,omitempty"`
	CurrentHeadSHA         string                   `json:"current_head_sha,omitempty"`
	ExitCode               int                      `json:"exit_code"`
	RetryCount             int                      `json:"retry_count"`
	FailureClassification  string                   `json:"failure_classification"`
	DurationMS             int64                    `json:"duration_ms"`
	FindingCount           int                      `json:"finding_count"`
	SeverityCounts         map[string]int           `json:"severity_counts"`
	SelectedAgents         []benchmarkSelectedAgent `json:"selected_agents,omitempty"`
	ThreadActionCount      int                      `json:"thread_action_count,omitempty"`
	Usage                  *benchmark.RunMetrics    `json:"usage,omitempty"`
	Warnings               []string                 `json:"warnings"`
	Artifacts              runArtifacts             `json:"artifacts"`
}

type runArtifacts struct {
	Dir           string `json:"dir"`
	ReviewJSON    string `json:"review_json,omitempty"`
	SelectionJSON string `json:"selection_json,omitempty"`
	SelectionLog  string `json:"selection_log,omitempty"`
	RecipeJSON    string `json:"recipe_json,omitempty"`
	Stderr        string `json:"stderr"`
	MetricsJSON   string `json:"metrics_json"`
}

type benchmarkSelectedAgent struct {
	AgentID string   `json:"agent_id"`
	Files   []string `json:"files,omitempty"`
}

type suiteArtifacts struct {
	Manifest           string `json:"manifest"`
	SummaryJSONL       string `json:"summary_jsonl"`
	SuiteSummary       string `json:"suite_summary"`
	Report             string `json:"report"`
	ComparisonJSON     string `json:"comparison_json"`
	ComparisonMarkdown string `json:"comparison_md"`
}

type runFlags struct {
	candidates []string
	cases      []string
	resultsDir string
	crBin      string
	jsonOutput bool
}

func newRunCommand(opts *root.Options) *cobra.Command {
	var flags runFlags
	cmd := &cobra.Command{
		Use:   "run <suite.yml>",
		Short: "Run a benchmark suite",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("benchmark run requires one suite path"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			summary, err := runBenchmarkSuite(cmd.Context(), opts, flags, args[0])
			if err != nil {
				return err
			}
			if flags.jsonOutput {
				enc := json.NewEncoder(opts.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}
			return renderRunText(opts, summary)
		},
	}
	cmd.Flags().StringArrayVar(&flags.candidates, "candidate", nil, "Candidate ID to run")
	cmd.Flags().StringArrayVar(&flags.cases, "case", nil, "Case ID to run")
	cmd.Flags().StringVar(&flags.resultsDir, "results-dir", "", "Benchmark run output directory; defaults to .cr-bench/results/<suite-id>/<timestamp> under the current working directory")
	cmd.Flags().StringVar(&flags.crBin, "cr-bin", "", "cr binary to run; defaults to the current cr binary")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	return cmd
}

func runBenchmarkSuite(ctx context.Context, opts *root.Options, flags runFlags, suitePath string) (benchmarkSuiteSummary, error) {
	logger := newProgressLogger(opts)
	suiteSpan := logger.Start("benchmark.run", "load_suite", "suite")
	suite, _, err := loadConfigAndSuite(opts, suitePath)
	if err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(suiteSpan, err)
	}
	suiteSpan.End(nil)
	selectSpan := logger.Start("benchmark.run", "select_matrix", suite.Suite.ID)
	selectedCandidates, selectedCases, err := benchmark.Select(suite, flags.candidates, flags.cases)
	if err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(selectSpan, mapBenchmarkError(err))
	}
	selectSpan.End(nil)
	resolveSpan := logger.Start("benchmark.run", "resolve_cr_bin", suite.Suite.ID)
	crBin, err := resolveRunCRBin(flags.crBin)
	if err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(resolveSpan, mapBenchmarkError(err))
	}
	resolveSpan.End(nil)
	started := benchmarkNow().UTC()
	resultsSpan := logger.Start("benchmark.run", "prepare_results", suite.Suite.ID)
	resultsDir, err := resolveRunResultsDir(suite.Suite.ID, flags.resultsDir, started)
	if err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(resultsSpan, err)
	}
	suiteHash, err := suiteFileSHA256(suite.Path)
	if err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(resultsSpan, err)
	}
	if err := os.MkdirAll(resultsDir, artifactDirPerm); err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(resultsSpan, fmt.Errorf("benchmark: create results dir: %w", err))
	}
	resultsSpan.End(nil)

	suiteDir := filepath.Dir(suite.Path)
	summary := benchmarkSuiteSummary{
		SchemaVersion:      benchmarkArtifactSchemaVersion,
		Mode:               benchmarkModeReview,
		SuiteID:            suite.Suite.ID,
		SuitePath:          suite.Path,
		SuiteSHA256:        suiteHash,
		StartedAt:          started.Format(time.RFC3339),
		ResultsDir:         resultsDir,
		CRBin:              crBin,
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
			runSummary, err := executeBenchmarkRun(ctx, logger, suiteDir, resultsDir, crBin, runID, candidate, benchCase)
			if err != nil {
				return benchmarkSuiteSummary{}, err
			}
			summary.Runs = append(summary.Runs, runSummary)
			summary.RunCount++
			if runSummary.ExitCode == exitcode.Success {
				summary.SuccessCount++
			} else {
				summary.FailureCount++
			}
			mergeSeverityCounts(summary.SeverityCounts, runSummary.SeverityCounts)
			if runSummary.Usage != nil {
				mergeUsage(&summary, *runSummary.Usage)
			}
		}
	}

	completed := benchmarkNow().UTC()
	summary.CompletedAt = completed.Format(time.RFC3339)
	summary.DurationMS = durationMS(completed.Sub(started))
	writeArtifactsSpan := logger.Start("benchmark.run", "write_suite_artifacts", suite.Suite.ID)
	if err := writeSuiteArtifacts(summary); err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(writeArtifactsSpan, err)
	}
	writeArtifactsSpan.End(nil)
	compareSpan := logger.Start("benchmark.run", "write_comparison", suite.Suite.ID)
	if _, err := writeComparisonArtifactsForResultsDir(summary.ResultsDir); err != nil {
		return benchmarkSuiteSummary{}, endProgressSpan(compareSpan, fmt.Errorf("benchmark: write comparison artifacts after suite artifacts were written to %s; rerun `cr benchmark compare %s`: %w", summary.ResultsDir, summary.ResultsDir, err))
	}
	compareSpan.End(nil)
	return summary, nil
}

func executeBenchmarkRun(ctx context.Context, logger *progress.Logger, suiteDir, resultsDir, crBin, runID string, candidate benchmark.Candidate, benchCase benchmark.Case) (benchmarkRun, error) {
	runSpan := logger.Start("benchmark.run", "execute_run", runID)
	runDir := filepath.Join(resultsDir, runID)
	if err := os.MkdirAll(runDir, artifactDirPerm); err != nil {
		return benchmarkRun{}, endProgressSpan(runSpan, fmt.Errorf("benchmark: create run dir %s: %w", runID, err))
	}
	artifacts := runArtifacts{
		Dir:         runDir,
		ReviewJSON:  filepath.Join(runDir, "review.json"),
		Stderr:      filepath.Join(runDir, "stderr.txt"),
		MetricsJSON: filepath.Join(runDir, "metrics.json"),
	}
	args := reviewArgs(suiteDir, candidate, benchCase)
	childSpan := logger.Start("benchmark.run", "child_review", runID)
	child := runReviewCommand(ctx, crBin, args)
	childSpan.End(child.Err)

	runSummary := benchmarkRun{
		RunID:                  runID,
		CandidateID:            candidate.ID,
		CaseID:                 benchCase.ID,
		PRURL:                  benchCase.PR,
		RequestedReviewBaseSHA: benchCase.ReviewBaseSHA,
		RequestedReviewHeadSHA: benchCase.ReviewHeadSHA,
		ExpectedBaseSHA:        benchCase.ExpectedBaseSHA,
		ExpectedHeadSHA:        benchCase.ExpectedHeadSHA,
		ExitCode:               child.ExitCode,
		RetryCount:             0,
		DurationMS:             durationMS(child.Duration),
		SeverityCounts:         map[string]int{},
		Warnings:               []string{},
		Artifacts:              artifacts,
	}
	if child.Err != nil && child.ExitCode < 0 {
		runSummary.Warnings = append(runSummary.Warnings, fmt.Sprintf("review command failed to start or complete: %s", child.Err.Error()))
	}
	parsed, warnings := parseReviewDryRun(child.Stdout, child.ExitCode)
	runSummary.FailureClassification = classifyRuntimeFailure(child.ExitCode, child.Err, child.Stdout, parsed != nil)
	if parsed != nil {
		runSummary.ReviewRunID = parsed.Run.RunID
		runSummary.ReviewArtifactPath = parsed.Run.ArtifactPath
		runSummary.ReviewBaseSHA = parsed.Run.BaseSHA
		runSummary.ReviewHeadSHA = parsed.Run.HeadSHA
		runSummary.CurrentBaseSHA = parsed.Run.CurrentBaseSHA
		runSummary.CurrentHeadSHA = parsed.Run.CurrentHeadSHA
		runSummary.FindingCount = len(parsed.Findings)
		runSummary.SeverityCounts = severityCounts(parsed.Findings)
		if usage, err := benchmark.ExtractRunMetrics(parsed.Run.ArtifactPath); err != nil {
			runSummary.Warnings = append(runSummary.Warnings, fmt.Sprintf("usage metrics unavailable: %s", err.Error()))
		} else if usage.HasData() {
			runSummary.Usage = &usage
		}
	} else {
		runSummary.Warnings = append(runSummary.Warnings, warnings...)
	}

	if err := writeArtifactFile(artifacts.ReviewJSON, child.Stdout); err != nil {
		return benchmarkRun{}, endProgressSpan(runSpan, err)
	}
	if err := writeArtifactFile(artifacts.Stderr, child.Stderr); err != nil {
		return benchmarkRun{}, endProgressSpan(runSpan, err)
	}
	if err := writeJSONFile(artifacts.MetricsJSON, runSummary); err != nil {
		return benchmarkRun{}, endProgressSpan(runSpan, err)
	}
	runSpan.End(nil)
	return runSummary, nil
}

func runReviewCommandReal(ctx context.Context, crBin string, args []string) reviewCommandResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, crBin, args...) // #nosec G204 -- benchmark run intentionally invokes a validated cr binary.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := exitcode.Success
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			err = ctx.Err()
			exitCode = -1
		} else {
			exitCode = -1
		}
	}
	return reviewCommandResult{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: exitCode,
		Duration: time.Since(start),
		Err:      err,
	}
}

func reviewArgs(suiteDir string, candidate benchmark.Candidate, benchCase benchmark.Case) []string {
	args := []string{"--profile", candidate.Profile, "--quiet", "review", benchCase.PR, "--dry-run", "--json"}
	if benchCase.ReviewBaseSHA != "" {
		args = append(args, "--review-base-sha", benchCase.ReviewBaseSHA)
	}
	if benchCase.ReviewHeadSHA != "" {
		args = append(args, "--review-head-sha", benchCase.ReviewHeadSHA)
	}
	if candidate.Stages.Selection.Model != "" {
		args = append(args,
			"--selection-model", candidate.Stages.Selection.Model,
		)
	}
	if candidate.Stages.Selection.Effort != "" {
		args = append(args,
			"--selection-effort", candidate.Stages.Selection.Effort,
		)
	}
	if candidate.Stages.Selection.Prompt != "" {
		args = append(args, "--selection-prompt", resolveStagePath(suiteDir, candidate.Stages.Selection.Prompt))
	}
	if candidate.Stages.Reviewers.Model != "" {
		args = append(args, "--reviewer-model", candidate.Stages.Reviewers.Model)
	}
	if candidate.Stages.Reviewers.ModelTier != "" {
		args = append(args, "--reviewer-model-tier", candidate.Stages.Reviewers.ModelTier)
	}
	if candidate.Stages.Reviewers.Effort != "" {
		args = append(args, "--reviewer-effort", candidate.Stages.Reviewers.Effort)
	}
	for _, dir := range candidate.Stages.Reviewers.AgentDirs {
		args = append(args, "--agents-dir", resolveAgentDir(suiteDir, dir))
	}
	if candidate.MaxAgents > 0 {
		args = append(args, "--max-agents", fmt.Sprint(candidate.MaxAgents))
	}
	if candidate.MaxConcurrency > 0 {
		args = append(args, "--max-concurrency", fmt.Sprint(candidate.MaxConcurrency))
	}
	return args
}

func resolveRunCRBin(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		exe, err := os.Executable()
		if err != nil || strings.TrimSpace(exe) == "" {
			return "", fmt.Errorf("%w: cr binary could not be resolved", benchmark.ErrInvalid)
		}
		path = exe
	} else if hasPathSeparator(path) {
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err == nil {
				path = abs
			}
		}
	} else {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", fmt.Errorf("%w: cr binary %q was not found in PATH", benchmark.ErrInvalid, path)
		}
		path = resolved
	}
	if err := checkExecutable(path); err != nil {
		return "", fmt.Errorf("%w: cr binary %s is not executable: %w", benchmark.ErrInvalid, path, err)
	}
	return path, nil
}

func resolveRunResultsDir(suiteID, configured string, started time.Time) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		path = filepath.Join(".cr-bench", "results", suiteID, started.UTC().Format(runTimestampLayout))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("benchmark: resolve run results dir: %w", err)
	}
	return abs, nil
}

func benchmarkRunID(matrixIndex, candidateIndex, caseIndex int, candidate benchmark.Candidate, benchCase benchmark.Case) string {
	return fmt.Sprintf("%04d-c%02d-k%02d-%s-%s", matrixIndex, candidateIndex+1, caseIndex+1, candidate.ID, benchCase.ID)
}

func summaryMode(summary benchmarkSuiteSummary) string {
	switch strings.TrimSpace(summary.Mode) {
	case "", benchmarkModeReview:
		return benchmarkModeReview
	case benchmarkModeSelection:
		return benchmarkModeSelection
	default:
		return strings.TrimSpace(summary.Mode)
	}
}

func summarizeCandidates(suiteDir string, candidates []benchmark.Candidate) []benchmarkCandidate {
	out := make([]benchmarkCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, benchmarkCandidate{
			ID:      candidate.ID,
			Profile: candidate.Profile,
			Stages: benchmarkCandidateStages{
				Selection: benchmarkSelectionStage{
					Model:  candidate.Stages.Selection.Model,
					Effort: candidate.Stages.Selection.Effort,
					Prompt: summarizePromptFile(suiteDir, candidate.Stages.Selection.Prompt),
				},
				Reviewers: benchmarkReviewerStage{
					Model:     candidate.Stages.Reviewers.Model,
					ModelTier: candidate.Stages.Reviewers.ModelTier,
					Effort:    candidate.Stages.Reviewers.Effort,
					AgentDirs: summarizeAgentDirs(suiteDir, candidate.Stages.Reviewers.AgentDirs),
				},
				Synthesis: summarizeOptionalSynthesisStage(suiteDir, candidate.Stages.Synthesis),
			},
			MaxAgents:      candidate.MaxAgents,
			MaxConcurrency: candidate.MaxConcurrency,
		})
	}
	return out
}

func summarizeOptionalSynthesisStage(suiteDir string, stage benchmark.SelectionStage) *benchmarkSynthesisStage {
	if !isOptionalStageConfigured(stage) {
		return nil
	}
	return &benchmarkSynthesisStage{
		Model:  stage.Model,
		Effort: stage.Effort,
		Prompt: summarizePromptFile(suiteDir, stage.Prompt),
	}
}

func isOptionalStageConfigured(stage benchmark.SelectionStage) bool {
	return strings.TrimSpace(stage.Model) != "" ||
		strings.TrimSpace(stage.Effort) != "" ||
		strings.TrimSpace(stage.Prompt) != ""
}

func summarizePromptFile(suiteDir, configured string) *benchmarkPromptFile {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return nil
	}
	resolved := resolveStagePath(suiteDir, configured)
	out := &benchmarkPromptFile{
		Configured: configured,
		Resolved:   resolved,
	}
	info, err := os.Stat(resolved)
	if err != nil {
		out.Warning = fmt.Sprintf("prompt file unavailable: %v", err)
		return out
	}
	out.Exists = true
	out.IsDir = info.IsDir()
	if info.IsDir() {
		out.Warning = "prompt path is a directory"
		return out
	}
	data, err := os.ReadFile(resolved) // #nosec G304 -- benchmark prompt path is constrained by suite validation and results summarization.
	if err != nil {
		out.Warning = fmt.Sprintf("prompt file unreadable: %v", err)
		return out
	}
	sum := sha256.Sum256(data)
	out.ContentSHA256 = hex.EncodeToString(sum[:])
	return out
}

func summarizeAgentDirs(suiteDir string, dirs []string) []benchmarkAgentDir {
	out := make([]benchmarkAgentDir, 0, len(dirs))
	for _, dir := range dirs {
		inspected := inspectAgentDir(suiteDir, dir)
		agentDir := benchmarkAgentDir{
			Configured: inspected.Configured,
			Resolved:   inspected.Resolved,
			Exists:     inspected.Exists,
			IsDir:      inspected.IsDir,
			Warning:    inspected.Warning,
		}
		if agentDir.Exists && agentDir.IsDir {
			metadataHash, err := hashAgentDirMetadata(agentDir.Resolved)
			if err == nil {
				agentDir.DirMetadataHash = metadataHash
			} else if agentDir.Warning == "" {
				agentDir.Warning = fmt.Sprintf("metadata hash unavailable: %v", err)
			}
		}
		out = append(out, agentDir)
	}
	return out
}

func resolveStagePath(suiteDir, path string) string {
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(suiteDir, path)
}

func summarizeCases(cases []benchmark.Case) []benchmarkCase {
	out := make([]benchmarkCase, 0, len(cases))
	for _, benchCase := range cases {
		out = append(out, benchmarkCase{
			ID:              benchCase.ID,
			PR:              benchCase.PR,
			ReviewBaseSHA:   benchCase.ReviewBaseSHA,
			ReviewHeadSHA:   benchCase.ReviewHeadSHA,
			ExpectedBaseSHA: benchCase.ExpectedBaseSHA,
			ExpectedHeadSHA: benchCase.ExpectedHeadSHA,
			Anchors:         append([]benchmark.Anchor(nil), benchCase.Anchors...),
		})
	}
	return out
}

type benchmarkRunRecipe struct {
	Candidate benchmarkCandidate `json:"candidate"`
	Case      benchmarkCase      `json:"case"`
}

func parseReviewDryRun(stdout []byte, exitCode int) (*view.ReviewDryRun, []string) {
	if len(bytes.TrimSpace(stdout)) == 0 {
		if exitCode != exitcode.Success {
			return nil, nil
		}
		return nil, []string{"review JSON stdout was empty"}
	}
	var parsed view.ReviewDryRun
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return nil, []string{fmt.Sprintf("review JSON parse failed: %s", err.Error())}
	}
	return &parsed, nil
}

func severityCounts(findings []view.ReviewFinding) map[string]int {
	counts := map[string]int{}
	for _, finding := range findings {
		counts[finding.Severity]++
	}
	return counts
}

func classifyRuntimeFailure(exitCode int, err error, stdout []byte, parsed bool) string {
	if err != nil && exitCode < 0 {
		return failureChildProcessError
	}
	switch exitCode {
	case exitcode.Success:
		if parsed {
			return failureNone
		}
		if len(bytes.TrimSpace(stdout)) == 0 {
			return failureMissingReviewJSON
		}
		return failureInvalidReviewJSON
	case exitcode.UsageError:
		return failureUsageError
	case exitcode.AuthConfigError:
		return failureAuthConfigError
	case exitcode.UpstreamError:
		return failureUpstreamError
	default:
		return failureChildExitNonzero
	}
}

func mergeSeverityCounts(dst, src map[string]int) {
	for severity, count := range src {
		dst[severity] += count
	}
}

func mergeUsage(summary *benchmarkSuiteSummary, usage benchmark.RunMetrics) {
	if !usage.HasData() {
		return
	}
	if summary.Usage == nil {
		summary.Usage = &benchmark.RunMetrics{}
	}
	summary.Usage.LLMCalls += usage.LLMCalls
	summary.Usage.Turns += usage.Turns
	summary.Usage.ToolCalls += usage.ToolCalls
	summary.Usage.ToolResults += usage.ToolResults
	summary.Usage.Tokens.Available = summary.Usage.Tokens.Available || usage.Tokens.Available
	summary.Usage.Tokens.Input += usage.Tokens.Input
	summary.Usage.Tokens.Output += usage.Tokens.Output
	summary.Usage.Tokens.CacheRead += usage.Tokens.CacheRead
	summary.Usage.Tokens.CacheWrite += usage.Tokens.CacheWrite
	summary.Usage.Tokens.TotalTokens += usage.Tokens.TotalTokens
	summary.Usage.Cost.Available = summary.Usage.Cost.Available || usage.Cost.Available
	summary.Usage.Cost.Input += usage.Cost.Input
	summary.Usage.Cost.Output += usage.Cost.Output
	summary.Usage.Cost.CacheRead += usage.Cost.CacheRead
	summary.Usage.Cost.CacheWrite += usage.Cost.CacheWrite
	summary.Usage.Cost.Total += usage.Cost.Total
}

func resolveAgentDir(suiteDir, configured string) string {
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(suiteDir, configured)
}

func hashAgentDirMetadata(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			_, _ = fmt.Fprintf(hash, "dir:%s\n", rel)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			_, _ = fmt.Fprintf(hash, "other:%s:%s\n", rel, info.Mode().String())
			return nil
		}
		// Keep benchmark summaries out of prompt contents; this is provenance
		// metadata, not a reproducibility hash of agent source bytes.
		_, _ = fmt.Fprintf(hash, "file:%s:%d:%s\n", rel, info.Size(), info.Mode().String())
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func suiteFileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- benchmark suite path has already been loaded and validated.
	if err != nil {
		return "", fmt.Errorf("benchmark: read suite for hash: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeSuiteArtifacts(summary benchmarkSuiteSummary) error {
	manifest := benchmarkManifest{
		SchemaVersion:      benchmarkArtifactSchemaVersion,
		Mode:               summaryMode(summary),
		SuiteID:            summary.SuiteID,
		SuitePath:          summary.SuitePath,
		SuiteSHA256:        summary.SuiteSHA256,
		StartedAt:          summary.StartedAt,
		CompletedAt:        summary.CompletedAt,
		ResultsDir:         summary.ResultsDir,
		CRBin:              summary.CRBin,
		SelectedCandidates: summary.SelectedCandidates,
		SelectedCases:      summary.SelectedCases,
		Runs:               make([]benchmarkManifestRun, 0, len(summary.Runs)),
	}
	for _, run := range summary.Runs {
		manifest.Runs = append(manifest.Runs, benchmarkManifestRun{
			RunID:       run.RunID,
			CandidateID: run.CandidateID,
			CaseID:      run.CaseID,
			Artifacts:   run.Artifacts,
		})
	}
	if err := writeJSONFile(summary.Artifacts.Manifest, manifest); err != nil {
		return err
	}
	if err := writeSummaryJSONL(summary.Artifacts.SummaryJSONL, summary.Runs); err != nil {
		return err
	}
	if err := writeJSONFile(summary.Artifacts.SuiteSummary, summary); err != nil {
		return err
	}
	return writeArtifactFile(summary.Artifacts.Report, []byte(renderReportMarkdown(summary)))
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("benchmark: marshal %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	return writeArtifactFile(path, data)
}

func writeSummaryJSONL(path string, runs []benchmarkRun) error {
	var buf bytes.Buffer
	for _, run := range runs {
		line, err := json.Marshal(run)
		if err != nil {
			return fmt.Errorf("benchmark: marshal summary line: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeArtifactFile(path, buf.Bytes())
}

func writeArtifactFile(path string, data []byte) error {
	if err := fsatomic.WriteFileAtomic(path, data, artifactFilePerm); err != nil {
		return fmt.Errorf("benchmark: write %s: %w", path, err)
	}
	if err := os.Chmod(path, artifactFilePerm); err != nil {
		return fmt.Errorf("benchmark: chmod %s: %w", path, err)
	}
	return nil
}

func renderRunText(opts *root.Options, summary benchmarkSuiteSummary) error {
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
		if _, err := fmt.Fprintf(opts.Stdout, "- %s candidate=%s case=%s exit=%d findings=%d\n", run.RunID, run.CandidateID, run.CaseID, run.ExitCode, run.FindingCount); err != nil {
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

func renderReportMarkdown(summary benchmarkSuiteSummary) string {
	if summaryMode(summary) == benchmarkModeSelection {
		return renderSelectionReportMarkdown(summary)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark Report: %s\n\n", summary.SuiteID)
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
	b.WriteString("| Run | Candidate | Case | Exit | Findings | Tokens | Cost |\n")
	b.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: |\n")
	for _, run := range summary.Runs {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d | %d | %s | %s |\n", run.RunID, run.CandidateID, run.CaseID, run.ExitCode, run.FindingCount, usageTokensCell(run.Usage), usageCostCell(run.Usage))
	}
	return b.String()
}

func usageTokensCell(usage *benchmark.RunMetrics) string {
	if usage == nil || !usage.HasTokenUsage() {
		return "n/a"
	}
	return fmt.Sprintf("%d", usage.Tokens.TotalTokens)
}

func usageCostCell(usage *benchmark.RunMetrics) string {
	if usage == nil || !usage.HasCostUsage() {
		return "n/a"
	}
	return fmt.Sprintf("$%.6f", usage.Cost.Total)
}

func durationMS(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return duration.Milliseconds()
}
