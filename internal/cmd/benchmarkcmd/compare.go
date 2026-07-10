package benchmarkcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const (
	runStatusCompleted = "completed"
	runStatusFailed    = "failed"
	runStatusPartial   = "partial"

	failureNone                 = "none"
	failureMissingArtifact      = "missing_artifact"
	failureMissingReviewJSON    = "missing_review_json"
	failureInvalidReviewJSON    = "invalid_review_json"
	failureInvalidSelectionJSON = "invalid_selection_json"
	failureUsageError           = "usage_error"
	failureAuthConfigError      = "auth_config_error"
	failureUpstreamError        = "upstream_error"
	failureChildProcessError    = "child_process_error"
	failureChildExitNonzero     = "child_exit_nonzero"
	failureUnavailable          = "unavailable"
	failureSelectionError       = "selection_error"

	placementAnchorHit       = "anchor_overlap_hit"
	placementAnchorMiss      = "anchor_overlap_miss"
	placementMultipleAnchors = "multiple_anchor_overlaps"
	placementUnmatched       = "unmatched_finding"

	placementCaveat = "Anchor overlap is mechanical placement only and is not semantic correctness."
)

var severityOrderForCompare = []string{"blocking", "major", "minor", "nits"}

type compareFlags struct {
	jsonOutput bool
}

type comparisonReport struct {
	SchemaVersion   int                        `json:"schema_version"`
	Mode            string                     `json:"mode,omitempty"`
	SuiteID         string                     `json:"suite_id"`
	ResultsDir      string                     `json:"results_dir"`
	SourceArtifacts comparisonSourceArtifacts  `json:"source_artifacts"`
	Artifacts       comparisonArtifacts        `json:"artifacts"`
	PlacementCaveat string                     `json:"placement_caveat"`
	Candidates      []benchmarkCandidate       `json:"candidates"`
	Cases           []benchmarkCase            `json:"cases"`
	Runs            []comparisonRun            `json:"runs"`
	CaseTotals      []comparisonCaseTotal      `json:"case_totals"`
	CandidateTotals []comparisonCandidateTotal `json:"candidate_totals"`
	Warnings        []string                   `json:"warnings"`
}

type comparisonSourceArtifacts struct {
	SuiteSummary string `json:"suite_summary"`
	Manifest     string `json:"manifest"`
	SummaryJSONL string `json:"summary_jsonl"`
}

type comparisonArtifacts struct {
	ComparisonJSON     string `json:"comparison_json"`
	ComparisonMarkdown string `json:"comparison_md"`
}

type comparisonRun struct {
	RunID                  string                   `json:"run_id"`
	CandidateID            string                   `json:"candidate_id"`
	CaseID                 string                   `json:"case_id"`
	PRURL                  string                   `json:"pr_url"`
	RequestedReviewBaseSHA string                   `json:"requested_review_base_sha,omitempty"`
	RequestedReviewHeadSHA string                   `json:"requested_review_head_sha,omitempty"`
	ExpectedBaseSHA        string                   `json:"expected_base_sha,omitempty"`
	ExpectedHeadSHA        string                   `json:"expected_head_sha,omitempty"`
	ReviewBaseSHA          string                   `json:"review_base_sha,omitempty"`
	ReviewHeadSHA          string                   `json:"review_head_sha,omitempty"`
	CurrentBaseSHA         string                   `json:"current_base_sha,omitempty"`
	CurrentHeadSHA         string                   `json:"current_head_sha,omitempty"`
	Status                 string                   `json:"status"`
	ExitCode               int                      `json:"exit_code"`
	RetryCount             int                      `json:"retry_count"`
	FailureClassification  string                   `json:"failure_classification"`
	FindingCount           int                      `json:"finding_count"`
	SeverityCounts         map[string]int           `json:"severity_counts"`
	SelectedAgents         []benchmarkSelectedAgent `json:"selected_agents,omitempty"`
	ThreadActionCount      int                      `json:"thread_action_count,omitempty"`
	DurationMS             int64                    `json:"duration_ms"`
	Usage                  *benchmark.RunMetrics    `json:"usage,omitempty"`
	Artifacts              comparisonRunArtifacts   `json:"artifacts"`
	AnchorSummary          *anchorSummary           `json:"anchor_summary,omitempty"`
	AnchorResults          []anchorResult           `json:"anchor_results,omitempty"`
	UnmatchedFindings      []unmatchedFinding       `json:"unmatched_findings,omitempty"`
	Warnings               []string                 `json:"warnings"`
}

type comparisonRunArtifacts struct {
	ReviewJSON         string `json:"review_json,omitempty"`
	SelectionJSON      string `json:"selection_json,omitempty"`
	SelectionLog       string `json:"selection_log,omitempty"`
	RecipeJSON         string `json:"recipe_json,omitempty"`
	Stderr             string `json:"stderr"`
	MetricsJSON        string `json:"metrics_json"`
	ReviewArtifactPath string `json:"review_artifact_path,omitempty"`
}

type anchorSummary struct {
	AnchorOverlapHit       int `json:"anchor_overlap_hit"`
	AnchorOverlapMiss      int `json:"anchor_overlap_miss"`
	MultipleAnchorOverlaps int `json:"multiple_anchor_overlaps"`
	UnmatchedFinding       int `json:"unmatched_finding"`
}

type anchorResult struct {
	AnchorID       string   `json:"anchor_id"`
	File           string   `json:"file"`
	Side           string   `json:"side"`
	StartLine      int      `json:"start_line"`
	EndLine        int      `json:"end_line"`
	PlacementLabel string   `json:"placement_label"`
	FindingIDs     []string `json:"finding_ids,omitempty"`
}

type unmatchedFinding struct {
	FindingID      string `json:"finding_id"`
	File           string `json:"file,omitempty"`
	Side           string `json:"side,omitempty"`
	Line           *int   `json:"line,omitempty"`
	PlacementLabel string `json:"placement_label"`
}

type comparisonCaseTotal struct {
	CaseID         string         `json:"case_id"`
	RunCount       int            `json:"run_count"`
	CompletedCount int            `json:"completed_count"`
	FailedCount    int            `json:"failed_count"`
	PartialCount   int            `json:"partial_count"`
	FindingCount   int            `json:"finding_count"`
	SeverityCounts map[string]int `json:"severity_counts"`
	AnchorSummary  *anchorSummary `json:"anchor_summary,omitempty"`
}

type comparisonCandidateTotal struct {
	CandidateID    string                `json:"candidate_id"`
	RunCount       int                   `json:"run_count"`
	CompletedCount int                   `json:"completed_count"`
	FailedCount    int                   `json:"failed_count"`
	PartialCount   int                   `json:"partial_count"`
	FindingCount   int                   `json:"finding_count"`
	SeverityCounts map[string]int        `json:"severity_counts"`
	DurationMS     int64                 `json:"duration_ms"`
	Usage          *benchmark.RunMetrics `json:"usage,omitempty"`
	AnchorSummary  *anchorSummary        `json:"anchor_summary,omitempty"`
}

type reviewArtifactRead struct {
	parsed   *view.ReviewDryRun
	warnings []string
	class    string
	ok       bool
}

func newCompareCommand(opts *root.Options) *cobra.Command {
	var flags compareFlags
	cmd := &cobra.Command{
		Use:   "compare <results-dir>",
		Short: "Compare benchmark results",
		Args:  exitcode.ExactArgs(1, "benchmark compare requires one results directory"),
		RunE: func(_ *cobra.Command, args []string) error {
			logger := root.NewProgressLogger(opts)
			comparison, err := writeComparisonArtifactsForResultsDirWithProgress(args[0], logger)
			if err != nil {
				return mapBenchmarkError(err)
			}
			return view.Render(opts.Stdout, flags.jsonOutput, comparison, func(io.Writer) error {
				return renderCompareText(opts, comparison)
			})
		},
	}
	root.AddJSONFlag(cmd, &flags.jsonOutput)
	return cmd
}

func writeComparisonArtifactsForResultsDir(resultsDir string) (comparisonReport, error) {
	return writeComparisonArtifactsForResultsDirWithProgress(resultsDir, progress.New(nil, true, nil))
}

func writeComparisonArtifactsForResultsDirWithProgress(resultsDir string, logger *progress.Logger) (comparisonReport, error) {
	resolveSpan := logger.Start("benchmark.compare", "load_summary", "results")
	summary, resolvedDir, err := loadBenchmarkSummaryForCompare(resultsDir)
	if err != nil {
		return comparisonReport{}, resolveSpan.End(err)
	}
	_ = resolveSpan.End(nil)
	buildSpan := logger.Start("benchmark.compare", "build_comparison", summary.SuiteID)
	comparison := buildComparison(summary, resolvedDir)
	_ = buildSpan.End(nil)
	jsonSpan := logger.Start("benchmark.compare", "write_json", summary.SuiteID)
	if err := writeJSONFile(comparison.Artifacts.ComparisonJSON, comparison); err != nil {
		return comparisonReport{}, jsonSpan.End(err)
	}
	_ = jsonSpan.End(nil)
	mdSpan := logger.Start("benchmark.compare", "write_markdown", summary.SuiteID)
	if err := writeArtifactFile(comparison.Artifacts.ComparisonMarkdown, []byte(renderComparisonMarkdown(comparison))); err != nil {
		return comparisonReport{}, mdSpan.End(err)
	}
	_ = mdSpan.End(nil)
	return comparison, nil
}

func loadBenchmarkSummaryForCompare(resultsDir string) (benchmarkSuiteSummary, string, error) {
	resolvedDir, err := resolveCompareResultsDir(resultsDir)
	if err != nil {
		return benchmarkSuiteSummary{}, "", err
	}
	path := filepath.Join(resolvedDir, "suite-summary.json")
	data, err := os.ReadFile(path) // #nosec G304 -- compare reads the benchmark-owned summary under caller-selected results dir.
	if err != nil {
		return benchmarkSuiteSummary{}, "", fmt.Errorf("%w: benchmark compare read suite-summary.json: %w", benchmark.ErrInvalid, err)
	}
	var summary benchmarkSuiteSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return benchmarkSuiteSummary{}, "", fmt.Errorf("%w: benchmark compare parse suite-summary.json: %w", benchmark.ErrInvalid, err)
	}
	if summary.SchemaVersion != benchmarkArtifactSchemaVersion {
		return benchmarkSuiteSummary{}, "", fmt.Errorf("%w: benchmark compare requires results schema_version %d; rerun benchmark results with the current cr version", benchmark.ErrInvalid, benchmarkArtifactSchemaVersion)
	}
	return summary, resolvedDir, nil
}

func resolveCompareResultsDir(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		return "", fmt.Errorf("%w: benchmark compare results directory is required", benchmark.ErrInvalid)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("benchmark: resolve compare results dir: %w", err)
	}
	return filepath.Clean(abs), nil
}

func buildComparison(summary benchmarkSuiteSummary, resultsDir string) comparisonReport {
	if summaryMode(summary) == benchmarkModeSelection {
		return buildSelectionComparison(summary, resultsDir)
	}
	comparison := comparisonReport{
		SchemaVersion: benchmarkArtifactSchemaVersion,
		Mode:          benchmarkModeReview,
		SuiteID:       summary.SuiteID,
		ResultsDir:    resultsDir,
		SourceArtifacts: comparisonSourceArtifacts{
			SuiteSummary: filepath.Join(resultsDir, "suite-summary.json"),
			Manifest:     filepath.Join(resultsDir, "manifest.json"),
			SummaryJSONL: filepath.Join(resultsDir, "summary.jsonl"),
		},
		Artifacts: comparisonArtifacts{
			ComparisonJSON:     filepath.Join(resultsDir, "comparison.json"),
			ComparisonMarkdown: filepath.Join(resultsDir, "comparison.md"),
		},
		PlacementCaveat: placementCaveat,
		Candidates:      append([]benchmarkCandidate(nil), summary.SelectedCandidates...),
		Cases:           append([]benchmarkCase(nil), summary.SelectedCases...),
		Warnings:        []string{},
	}

	casesByID := make(map[string]benchmarkCase, len(summary.SelectedCases))
	for _, benchCase := range summary.SelectedCases {
		casesByID[benchCase.ID] = benchCase
	}
	caseTotals := make(map[string]*comparisonCaseTotal, len(summary.SelectedCases))
	for _, benchCase := range summary.SelectedCases {
		caseTotals[benchCase.ID] = &comparisonCaseTotal{CaseID: benchCase.ID, SeverityCounts: map[string]int{}}
	}
	candidateTotals := make(map[string]*comparisonCandidateTotal, len(summary.SelectedCandidates))
	for _, candidate := range summary.SelectedCandidates {
		candidateTotals[candidate.ID] = &comparisonCandidateTotal{CandidateID: candidate.ID, SeverityCounts: map[string]int{}}
	}

	for _, run := range summary.Runs {
		benchCase := casesByID[run.CaseID]
		read := readReviewArtifactForCompare(resultsDir, run)
		row := comparisonRun{
			RunID:                  run.RunID,
			CandidateID:            run.CandidateID,
			CaseID:                 run.CaseID,
			PRURL:                  run.PRURL,
			RequestedReviewBaseSHA: run.RequestedReviewBaseSHA,
			RequestedReviewHeadSHA: run.RequestedReviewHeadSHA,
			ExpectedBaseSHA:        run.ExpectedBaseSHA,
			ExpectedHeadSHA:        run.ExpectedHeadSHA,
			ReviewBaseSHA:          run.ReviewBaseSHA,
			ReviewHeadSHA:          run.ReviewHeadSHA,
			CurrentBaseSHA:         run.CurrentBaseSHA,
			CurrentHeadSHA:         run.CurrentHeadSHA,
			Status:                 compareRunStatus(run, read.ok),
			ExitCode:               run.ExitCode,
			RetryCount:             run.RetryCount,
			FailureClassification:  compareFailureClassification(run, read),
			FindingCount:           run.FindingCount,
			SeverityCounts:         copySeverityCounts(run.SeverityCounts),
			DurationMS:             run.DurationMS,
			Usage:                  run.Usage,
			Artifacts: comparisonRunArtifacts{
				ReviewJSON:         run.Artifacts.ReviewJSON,
				SelectionJSON:      run.Artifacts.SelectionJSON,
				SelectionLog:       run.Artifacts.SelectionLog,
				RecipeJSON:         run.Artifacts.RecipeJSON,
				Stderr:             run.Artifacts.Stderr,
				MetricsJSON:        run.Artifacts.MetricsJSON,
				ReviewArtifactPath: run.ReviewArtifactPath,
			},
			Warnings: appendUniqueWarnings(nil, run.Warnings...),
		}
		row.Warnings = appendUniqueWarnings(row.Warnings, read.warnings...)
		if len(benchCase.Anchors) > 0 {
			if read.parsed != nil {
				row.AnchorSummary, row.AnchorResults, row.UnmatchedFindings = compareAnchors(benchCase.Anchors, read.parsed.Findings)
			} else {
				row.Warnings = appendUniqueWarnings(row.Warnings, "anchor placement unavailable: review JSON could not be parsed")
			}
		}
		comparison.Runs = append(comparison.Runs, row)
		accumulateCaseTotal(caseTotals[run.CaseID], row)
		accumulateCandidateTotal(candidateTotals[run.CandidateID], row)
	}

	for _, benchCase := range summary.SelectedCases {
		if total := caseTotals[benchCase.ID]; total != nil {
			comparison.CaseTotals = append(comparison.CaseTotals, *total)
		}
	}
	for _, candidate := range summary.SelectedCandidates {
		if total := candidateTotals[candidate.ID]; total != nil {
			comparison.CandidateTotals = append(comparison.CandidateTotals, *total)
		}
	}
	return comparison
}

func buildSelectionComparison(summary benchmarkSuiteSummary, resultsDir string) comparisonReport {
	comparison := comparisonReport{
		SchemaVersion: benchmarkArtifactSchemaVersion,
		Mode:          benchmarkModeSelection,
		SuiteID:       summary.SuiteID,
		ResultsDir:    resultsDir,
		SourceArtifacts: comparisonSourceArtifacts{
			SuiteSummary: filepath.Join(resultsDir, "suite-summary.json"),
			Manifest:     filepath.Join(resultsDir, "manifest.json"),
			SummaryJSONL: filepath.Join(resultsDir, "summary.jsonl"),
		},
		Artifacts: comparisonArtifacts{
			ComparisonJSON:     filepath.Join(resultsDir, "comparison.json"),
			ComparisonMarkdown: filepath.Join(resultsDir, "comparison.md"),
		},
		Candidates: append([]benchmarkCandidate(nil), summary.SelectedCandidates...),
		Cases:      append([]benchmarkCase(nil), summary.SelectedCases...),
		Warnings:   []string{},
	}

	caseTotals := make(map[string]*comparisonCaseTotal, len(summary.SelectedCases))
	for _, benchCase := range summary.SelectedCases {
		caseTotals[benchCase.ID] = &comparisonCaseTotal{CaseID: benchCase.ID, SeverityCounts: map[string]int{}}
	}
	candidateTotals := make(map[string]*comparisonCandidateTotal, len(summary.SelectedCandidates))
	for _, candidate := range summary.SelectedCandidates {
		candidateTotals[candidate.ID] = &comparisonCandidateTotal{CandidateID: candidate.ID, SeverityCounts: map[string]int{}}
	}
	for _, run := range summary.Runs {
		row := comparisonRun{
			RunID:                  run.RunID,
			CandidateID:            run.CandidateID,
			CaseID:                 run.CaseID,
			PRURL:                  run.PRURL,
			RequestedReviewBaseSHA: run.RequestedReviewBaseSHA,
			RequestedReviewHeadSHA: run.RequestedReviewHeadSHA,
			ExpectedBaseSHA:        run.ExpectedBaseSHA,
			ExpectedHeadSHA:        run.ExpectedHeadSHA,
			ReviewBaseSHA:          run.ReviewBaseSHA,
			ReviewHeadSHA:          run.ReviewHeadSHA,
			CurrentBaseSHA:         run.CurrentBaseSHA,
			CurrentHeadSHA:         run.CurrentHeadSHA,
			Status:                 selectionRunStatus(run),
			ExitCode:               run.ExitCode,
			RetryCount:             run.RetryCount,
			FailureClassification:  selectionFailureClassification(run),
			SelectedAgents:         append([]benchmarkSelectedAgent(nil), run.SelectedAgents...),
			ThreadActionCount:      run.ThreadActionCount,
			DurationMS:             run.DurationMS,
			Usage:                  run.Usage,
			Artifacts: comparisonRunArtifacts{
				ReviewJSON:         run.Artifacts.ReviewJSON,
				SelectionJSON:      run.Artifacts.SelectionJSON,
				SelectionLog:       run.Artifacts.SelectionLog,
				RecipeJSON:         run.Artifacts.RecipeJSON,
				Stderr:             run.Artifacts.Stderr,
				MetricsJSON:        run.Artifacts.MetricsJSON,
				ReviewArtifactPath: run.ReviewArtifactPath,
			},
			Warnings: appendUniqueWarnings(nil, run.Warnings...),
		}
		comparison.Runs = append(comparison.Runs, row)
		accumulateCaseTotal(caseTotals[run.CaseID], row)
		accumulateCandidateTotal(candidateTotals[run.CandidateID], row)
	}
	for _, benchCase := range summary.SelectedCases {
		if total := caseTotals[benchCase.ID]; total != nil {
			comparison.CaseTotals = append(comparison.CaseTotals, *total)
		}
	}
	for _, candidate := range summary.SelectedCandidates {
		if total := candidateTotals[candidate.ID]; total != nil {
			comparison.CandidateTotals = append(comparison.CandidateTotals, *total)
		}
	}
	return comparison
}

func appendUniqueWarnings(dst []string, warnings ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(warnings))
	unique := dst[:0]
	for _, warning := range slices.Concat(dst, warnings) {
		if _, ok := seen[warning]; ok {
			continue
		}
		seen[warning] = struct{}{}
		unique = append(unique, warning)
	}
	if len(unique) == 0 {
		return nil
	}
	return unique
}

func readReviewArtifactForCompare(resultsDir string, run benchmarkRun) reviewArtifactRead {
	path, ok, warning := benchmarkArtifactPathInResultsDir(resultsDir, run.Artifacts.ReviewJSON)
	if !ok {
		return reviewArtifactRead{warnings: []string{fmt.Sprintf("review JSON unavailable: %s", warning)}, class: failureMissingArtifact}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path was constrained to the benchmark results directory.
	if err != nil {
		return reviewArtifactRead{warnings: []string{fmt.Sprintf("review JSON unavailable: %v", err)}, class: failureMissingArtifact}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return reviewArtifactRead{warnings: []string{"review JSON was empty"}, class: failureMissingReviewJSON}
	}
	var parsed view.ReviewDryRun
	if err := json.Unmarshal(data, &parsed); err != nil {
		return reviewArtifactRead{warnings: []string{fmt.Sprintf("review JSON parse failed: %s", err.Error())}, class: failureInvalidReviewJSON}
	}
	return reviewArtifactRead{parsed: &parsed, ok: true, class: failureNone}
}

func benchmarkArtifactPathInResultsDir(resultsDir, artifactPath string) (string, bool, string) {
	if strings.TrimSpace(artifactPath) == "" {
		return "", false, "path is empty"
	}
	path := artifactPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(resultsDir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err.Error()
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(resultsDir, abs)
	if err != nil {
		return "", false, err.Error()
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", false, fmt.Sprintf("%s is outside results dir", artifactPath)
	}
	realResultsDir, err := filepath.EvalSymlinks(resultsDir)
	if err != nil {
		return "", false, fmt.Sprintf("results dir cannot be resolved: %v", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return abs, true, ""
		}
		return "", false, fmt.Sprintf("%s cannot be inspected: %v", artifactPath, err)
	}
	realArtifact, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Sprintf("%s cannot be resolved: %v", artifactPath, err)
		}
		return "", false, fmt.Sprintf("%s cannot be resolved: %v", artifactPath, err)
	}
	realRel, err := filepath.Rel(realResultsDir, realArtifact)
	if err != nil {
		return "", false, err.Error()
	}
	if realRel == "." || strings.HasPrefix(realRel, ".."+string(os.PathSeparator)) || realRel == ".." || filepath.IsAbs(realRel) {
		return "", false, fmt.Sprintf("%s resolves outside results dir", artifactPath)
	}
	return abs, true, ""
}

func compareFailureClassification(run benchmarkRun, read reviewArtifactRead) string {
	if run.FailureClassification != "" && run.FailureClassification != failureNone {
		return run.FailureClassification
	}
	switch run.ExitCode {
	case exitcode.UsageError:
		return failureUsageError
	case exitcode.AuthConfigError:
		return failureAuthConfigError
	case exitcode.UpstreamError:
		return failureUpstreamError
	}
	if run.ExitCode != exitcode.Success {
		return failureChildExitNonzero
	}
	if read.class != "" && read.class != failureNone {
		return read.class
	}
	if read.ok {
		return failureNone
	}
	return failureUnavailable
}

func compareRunStatus(run benchmarkRun, reviewJSONOK bool) string {
	if run.ExitCode != exitcode.Success {
		return runStatusFailed
	}
	if reviewJSONOK {
		return runStatusCompleted
	}
	return runStatusPartial
}

func compareAnchors(anchors []benchmark.Anchor, findings []view.ReviewFinding) (*anchorSummary, []anchorResult, []unmatchedFinding) {
	if len(anchors) == 0 {
		return nil, nil, nil
	}
	summary := &anchorSummary{}
	results := make([]anchorResult, 0, len(anchors))
	matchedFinding := make([]bool, len(findings))
	for _, anchor := range anchors {
		if len(anchor.Lines) < 2 {
			continue
		}
		result := anchorResult{
			AnchorID:  anchor.ID,
			File:      anchor.File,
			Side:      anchor.Side,
			StartLine: anchor.Lines[0],
			EndLine:   anchor.Lines[1],
		}
		for i, finding := range findings {
			if findingOverlapsAnchor(finding, anchor) {
				result.FindingIDs = append(result.FindingIDs, finding.ID)
				matchedFinding[i] = true
			}
		}
		switch len(result.FindingIDs) {
		case 0:
			result.PlacementLabel = placementAnchorMiss
			summary.AnchorOverlapMiss++
		case 1:
			result.PlacementLabel = placementAnchorHit
			summary.AnchorOverlapHit++
		default:
			result.PlacementLabel = placementMultipleAnchors
			summary.MultipleAnchorOverlaps++
		}
		results = append(results, result)
	}
	var unmatched []unmatchedFinding
	for i, finding := range findings {
		if matchedFinding[i] {
			continue
		}
		row := unmatchedFinding{
			FindingID:      finding.ID,
			File:           finding.FilePath,
			Side:           finding.Side,
			PlacementLabel: placementUnmatched,
		}
		if finding.Line != nil {
			line := *finding.Line
			row.Line = &line
		}
		unmatched = append(unmatched, row)
		summary.UnmatchedFinding++
	}
	return summary, results, unmatched
}

func findingOverlapsAnchor(finding view.ReviewFinding, anchor benchmark.Anchor) bool {
	if finding.Line == nil || len(anchor.Lines) < 2 {
		return false
	}
	line := *finding.Line
	return finding.FilePath == anchor.File && finding.Side == anchor.Side && anchor.Lines[0] <= line && line <= anchor.Lines[1]
}

func accumulateCaseTotal(total *comparisonCaseTotal, run comparisonRun) {
	if total == nil {
		return
	}
	total.RunCount++
	accumulateRunStatus(&total.CompletedCount, &total.FailedCount, &total.PartialCount, run.Status)
	total.FindingCount += run.FindingCount
	mergeSeverityCounts(total.SeverityCounts, run.SeverityCounts)
	total.AnchorSummary = addAnchorSummary(total.AnchorSummary, run.AnchorSummary)
}

func accumulateCandidateTotal(total *comparisonCandidateTotal, run comparisonRun) {
	if total == nil {
		return
	}
	total.RunCount++
	accumulateRunStatus(&total.CompletedCount, &total.FailedCount, &total.PartialCount, run.Status)
	total.FindingCount += run.FindingCount
	total.DurationMS += run.DurationMS
	mergeSeverityCounts(total.SeverityCounts, run.SeverityCounts)
	total.AnchorSummary = addAnchorSummary(total.AnchorSummary, run.AnchorSummary)
	if run.Usage != nil {
		if total.Usage == nil {
			total.Usage = &benchmark.RunMetrics{}
		}
		total.Usage.Add(*run.Usage)
	}
}

func accumulateRunStatus(completed, failed, partial *int, status string) {
	switch status {
	case runStatusCompleted:
		(*completed)++
	case runStatusFailed:
		(*failed)++
	case runStatusPartial:
		(*partial)++
	}
}

func addAnchorSummary(total, run *anchorSummary) *anchorSummary {
	if run == nil {
		return total
	}
	if total == nil {
		total = &anchorSummary{}
	}
	total.AnchorOverlapHit += run.AnchorOverlapHit
	total.AnchorOverlapMiss += run.AnchorOverlapMiss
	total.MultipleAnchorOverlaps += run.MultipleAnchorOverlaps
	total.UnmatchedFinding += run.UnmatchedFinding
	return total
}

func copySeverityCounts(counts map[string]int) map[string]int {
	out := map[string]int{}
	for severity, count := range counts {
		out[severity] = count
	}
	return out
}

func selectionRunStatus(run benchmarkRun) string {
	if run.ExitCode == exitcode.Success {
		return runStatusCompleted
	}
	return runStatusFailed
}

func selectionFailureClassification(run benchmarkRun) string {
	if strings.TrimSpace(run.FailureClassification) != "" {
		return run.FailureClassification
	}
	if run.ExitCode == exitcode.Success {
		return failureNone
	}
	return failureSelectionError
}

func renderSelectionComparisonMarkdown(comparison comparisonReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Selector Benchmark Comparison: %s\n\n", comparison.SuiteID)
	fmt.Fprintf(&b, "- Results dir: `%s`\n\n", comparison.ResultsDir)
	b.WriteString("## Candidate x Case Status\n\n")
	b.WriteString("| Candidate | Case | Status | Exit | Failure | Selected Reviewers | Thread Actions | Duration ms | Tokens | Cost |\n")
	b.WriteString("| --- | --- | --- | ---: | --- | --- | ---: | ---: | ---: | ---: |\n")
	for _, run := range comparison.Runs {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %d | %s | %s | %d | %d | %s | %s |\n",
			markdownCode(run.CandidateID),
			markdownCode(run.CaseID),
			run.Status,
			run.ExitCode,
			markdownCode(run.FailureClassification),
			selectedAgentsMarkdownCell(run.SelectedAgents),
			run.ThreadActionCount,
			run.DurationMS,
			usageTokensCell(run.Usage),
			usageCostCell(run.Usage),
		)
	}
	b.WriteString("\n## Cases\n\n")
	for _, benchCase := range comparison.Cases {
		fmt.Fprintf(&b, "### `%s`\n\n", markdownCode(benchCase.ID))
		fmt.Fprintf(&b, "- PR: `%s`\n", markdownCode(benchCase.PR))
		writeOptionalMarkdownKV(&b, "Requested base SHA", benchCase.ReviewBaseSHA)
		writeOptionalMarkdownKV(&b, "Requested head SHA", benchCase.ReviewHeadSHA)
		writeOptionalMarkdownKV(&b, "Expected base SHA", benchCase.ExpectedBaseSHA)
		writeOptionalMarkdownKV(&b, "Expected head SHA", benchCase.ExpectedHeadSHA)
		b.WriteString("\n")
		b.WriteString("| Candidate | Status | Failure | Selected Reviewers | Thread Actions |\n")
		b.WriteString("| --- | --- | --- | --- | ---: |\n")
		for _, run := range comparison.Runs {
			if run.CaseID != benchCase.ID {
				continue
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %d |\n",
				markdownCode(run.CandidateID),
				run.Status,
				markdownCode(run.FailureClassification),
				selectedAgentsMarkdownCell(run.SelectedAgents),
				run.ThreadActionCount,
			)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Case Totals\n\n")
	b.WriteString("| Case | Runs | Completed | Failed | Partial |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, total := range comparison.CaseTotals {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d |\n", markdownCode(total.CaseID), total.RunCount, total.CompletedCount, total.FailedCount, total.PartialCount)
	}
	b.WriteString("\n## Candidate Totals\n\n")
	b.WriteString("| Candidate | Runs | Completed | Failed | Partial | Duration ms | Tokens | Cost |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, total := range comparison.CandidateTotals {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d | %s | %s |\n",
			markdownCode(total.CandidateID),
			total.RunCount,
			total.CompletedCount,
			total.FailedCount,
			total.PartialCount,
			total.DurationMS,
			usageTokensCell(total.Usage),
			usageCostCell(total.Usage),
		)
	}
	b.WriteString("\n## Raw Artifacts\n\n")
	for _, run := range comparison.Runs {
		fmt.Fprintf(&b, "- `%s`: selection `%s`, log `%s`, metrics `%s`, stderr `%s`\n",
			markdownCode(run.RunID),
			markdownCode(run.Artifacts.SelectionJSON),
			markdownCode(run.Artifacts.SelectionLog),
			markdownCode(run.Artifacts.MetricsJSON),
			markdownCode(run.Artifacts.Stderr),
		)
	}
	return b.String()
}

func renderCompareText(opts *root.Options, comparison comparisonReport) error {
	title := "Benchmark comparison"
	if comparison.Mode == benchmarkModeSelection {
		title = "Selector benchmark comparison"
	}
	w := stickyWriter{w: opts.Stdout}
	w.printf("%s: %s\n", title, comparison.SuiteID)
	w.printf("Results dir: %s\n", comparison.ResultsDir)
	w.printf("Runs: %d\n", len(comparison.Runs))
	w.println("Artifacts:")
	w.printf("  Comparison JSON: %s\n", comparison.Artifacts.ComparisonJSON)
	w.printf("  Comparison Markdown: %s\n", comparison.Artifacts.ComparisonMarkdown)
	return w.err
}

func renderComparisonMarkdown(comparison comparisonReport) string {
	if comparison.Mode == benchmarkModeSelection {
		return renderSelectionComparisonMarkdown(comparison)
	}
	var b strings.Builder
	severityColumns := comparisonSeverityColumns(comparison)
	fmt.Fprintf(&b, "# Benchmark Comparison: %s\n\n", comparison.SuiteID)
	fmt.Fprintf(&b, "- Results dir: `%s`\n", comparison.ResultsDir)
	fmt.Fprintf(&b, "- Placement caveat: %s\n\n", comparison.PlacementCaveat)
	b.WriteString("## Source Artifacts\n\n")
	fmt.Fprintf(&b, "- Suite summary: `%s`\n", comparison.SourceArtifacts.SuiteSummary)
	fmt.Fprintf(&b, "- Manifest: `%s`\n", comparison.SourceArtifacts.Manifest)
	fmt.Fprintf(&b, "- JSONL: `%s`\n\n", comparison.SourceArtifacts.SummaryJSONL)
	b.WriteString("## Candidate x Case Status\n\n")
	b.WriteString("| Candidate | Case | Status | Exit | Failure | Findings |")
	for _, severity := range severityColumns {
		fmt.Fprintf(&b, " %s |", severityHeader(severity))
	}
	b.WriteString(" Duration ms | Tokens | Cost |\n")
	b.WriteString("| --- | --- | --- | ---: | --- | ---: |")
	for range severityColumns {
		b.WriteString(" ---: |")
	}
	b.WriteString(" ---: | ---: | ---: |\n")
	for _, run := range comparison.Runs {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %d | %s | %d |",
			markdownCode(run.CandidateID),
			markdownCode(run.CaseID),
			run.Status,
			run.ExitCode,
			run.FailureClassification,
			run.FindingCount,
		)
		for _, severity := range severityColumns {
			fmt.Fprintf(&b, " %d |", run.SeverityCounts[severity])
		}
		fmt.Fprintf(&b, " %d | %s | %s |\n",
			run.DurationMS,
			usageTokensCell(run.Usage),
			usageCostCell(run.Usage),
		)
	}
	b.WriteString("\n## Cases\n\n")
	for _, benchCase := range comparison.Cases {
		fmt.Fprintf(&b, "### `%s`\n\n", markdownCode(benchCase.ID))
		fmt.Fprintf(&b, "- PR: `%s`\n", markdownCode(benchCase.PR))
		writeOptionalMarkdownKV(&b, "Requested base SHA", benchCase.ReviewBaseSHA)
		writeOptionalMarkdownKV(&b, "Requested head SHA", benchCase.ReviewHeadSHA)
		writeOptionalMarkdownKV(&b, "Expected base SHA", benchCase.ExpectedBaseSHA)
		writeOptionalMarkdownKV(&b, "Expected head SHA", benchCase.ExpectedHeadSHA)
		b.WriteString("\n")
		b.WriteString("| Candidate | Status | Findings | Anchor hit | Anchor miss | Multiple anchor overlaps | Unmatched findings |\n")
		b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: |\n")
		for _, run := range comparison.Runs {
			if run.CaseID != benchCase.ID {
				continue
			}
			summary := anchorSummary{}
			if run.AnchorSummary != nil {
				summary = *run.AnchorSummary
			}
			fmt.Fprintf(&b, "| `%s` | %s | %d | %d | %d | %d | %d |\n", markdownCode(run.CandidateID), run.Status, run.FindingCount, summary.AnchorOverlapHit, summary.AnchorOverlapMiss, summary.MultipleAnchorOverlaps, summary.UnmatchedFinding)
		}
		b.WriteString("\n")
		for _, run := range comparison.Runs {
			if run.CaseID != benchCase.ID || len(run.AnchorResults) == 0 {
				continue
			}
			fmt.Fprintf(&b, "Anchor placement for candidate `%s`:\n\n", markdownCode(run.CandidateID))
			b.WriteString("| Anchor | File | Side | Lines | Placement | Finding IDs |\n")
			b.WriteString("| --- | --- | --- | ---: | --- | --- |\n")
			for _, anchor := range run.AnchorResults {
				fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d-%d | %s | %s |\n", markdownCode(anchor.AnchorID), markdownCode(anchor.File), markdownCode(anchor.Side), anchor.StartLine, anchor.EndLine, anchor.PlacementLabel, markdownFindingIDs(anchor.FindingIDs))
			}
			if len(run.UnmatchedFindings) > 0 {
				b.WriteString("\nUnmatched findings:\n\n")
				b.WriteString("| Finding | File | Side | Line | Placement |\n")
				b.WriteString("| --- | --- | --- | ---: | --- |\n")
				for _, finding := range run.UnmatchedFindings {
					fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s |\n", markdownCode(finding.FindingID), markdownCode(finding.File), markdownCode(finding.Side), optionalLineCell(finding.Line), finding.PlacementLabel)
				}
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("## Case Totals\n\n")
	b.WriteString("| Case | Runs | Completed | Failed | Partial | Findings |")
	for _, severity := range severityColumns {
		fmt.Fprintf(&b, " %s |", severityHeader(severity))
	}
	b.WriteString(" Anchor hit | Anchor miss | Multiple anchor overlaps | Unmatched findings |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |")
	for range severityColumns {
		b.WriteString(" ---: |")
	}
	b.WriteString(" ---: | ---: | ---: | ---: |\n")
	for _, total := range comparison.CaseTotals {
		anchors := anchorSummary{}
		if total.AnchorSummary != nil {
			anchors = *total.AnchorSummary
		}
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d |",
			markdownCode(total.CaseID),
			total.RunCount,
			total.CompletedCount,
			total.FailedCount,
			total.PartialCount,
			total.FindingCount,
		)
		for _, severity := range severityColumns {
			fmt.Fprintf(&b, " %d |", total.SeverityCounts[severity])
		}
		fmt.Fprintf(&b, " %d | %d | %d | %d |\n", anchors.AnchorOverlapHit, anchors.AnchorOverlapMiss, anchors.MultipleAnchorOverlaps, anchors.UnmatchedFinding)
	}
	b.WriteString("\n")
	b.WriteString("## Candidate Totals\n\n")
	b.WriteString("| Candidate | Runs | Completed | Failed | Partial | Findings |")
	for _, severity := range severityColumns {
		fmt.Fprintf(&b, " %s |", severityHeader(severity))
	}
	b.WriteString(" Duration ms | Tokens | Cost |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |")
	for range severityColumns {
		b.WriteString(" ---: |")
	}
	b.WriteString(" ---: | ---: | ---: |\n")
	for _, total := range comparison.CandidateTotals {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d |",
			markdownCode(total.CandidateID),
			total.RunCount,
			total.CompletedCount,
			total.FailedCount,
			total.PartialCount,
			total.FindingCount,
		)
		for _, severity := range severityColumns {
			fmt.Fprintf(&b, " %d |", total.SeverityCounts[severity])
		}
		fmt.Fprintf(&b, " %d | %s | %s |\n",
			total.DurationMS,
			usageTokensCell(total.Usage),
			usageCostCell(total.Usage),
		)
	}
	b.WriteString("\n## Raw Artifacts\n\n")
	b.WriteString("| Run | Review JSON | Stderr | Metrics | Durable review artifacts |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, run := range comparison.Runs {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | `%s` | `%s` |\n", markdownCode(run.RunID), markdownCode(run.Artifacts.ReviewJSON), markdownCode(run.Artifacts.Stderr), markdownCode(run.Artifacts.MetricsJSON), markdownCode(run.Artifacts.ReviewArtifactPath))
	}
	return b.String()
}

func comparisonSeverityColumns(comparison comparisonReport) []string {
	known := map[string]bool{}
	columns := append([]string(nil), severityOrderForCompare...)
	for _, severity := range columns {
		known[severity] = true
	}
	unknown := map[string]bool{}
	for _, run := range comparison.Runs {
		for severity := range run.SeverityCounts {
			if !known[severity] {
				unknown[severity] = true
			}
		}
	}
	for _, total := range comparison.CandidateTotals {
		for severity := range total.SeverityCounts {
			if !known[severity] {
				unknown[severity] = true
			}
		}
	}
	var sorted []string
	for severity := range unknown {
		sorted = append(sorted, severity)
	}
	sort.Strings(sorted)
	return append(columns, sorted...)
}

func severityHeader(severity string) string {
	switch severity {
	case "blocking":
		return "Blocking"
	case "major":
		return "Major"
	case "minor":
		return "Minor"
	case "nits":
		return "Nits"
	default:
		return severity
	}
}

func writeOptionalMarkdownKV(b *strings.Builder, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(b, "- %s: `%s`\n", label, markdownCode(value))
}

func markdownFindingIDs(ids []string) string {
	if len(ids) == 0 {
		return "n/a"
	}
	copied := append([]string(nil), ids...)
	sort.Strings(copied)
	for i, id := range copied {
		copied[i] = "`" + markdownCode(id) + "`"
	}
	return strings.Join(copied, ", ")
}

func markdownCode(value string) string {
	value = strings.ReplaceAll(value, "`", "\\`")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func optionalLineCell(line *int) string {
	if line == nil {
		return "n/a"
	}
	return fmt.Sprint(*line)
}
