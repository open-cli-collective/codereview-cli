// Package benchmarkcmd wires the `cr benchmark` command surface.
package benchmarkcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type doctorFlags struct {
	candidates []string
	cases      []string
	resultsDir string
	crBin      string
	jsonOutput bool
}

type doctorReport struct {
	SuiteID            string            `json:"suite_id"`
	SuitePath          string            `json:"suite_path"`
	ResolvedResultsDir string            `json:"resolved_results_dir"`
	CRBin              string            `json:"cr_bin"`
	Candidates         []doctorCandidate `json:"candidates"`
	Cases              []doctorCase      `json:"cases"`
	Warnings           []string          `json:"warnings"`
}

type doctorCandidate struct {
	ID               string       `json:"id"`
	Profile          string       `json:"profile"`
	ProfileAvailable bool         `json:"profile_available"`
	GitHost          string       `json:"git_host,omitempty"`
	Stages           doctorStages `json:"stages"`
}

type doctorStages struct {
	Selection doctorSelectionStage `json:"selection"`
	Reviewers doctorReviewerStage  `json:"reviewers,omitempty"`
}

type doctorSelectionStage struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

type doctorReviewerStage struct {
	Model     string           `json:"model,omitempty"`
	Effort    string           `json:"effort,omitempty"`
	AgentDirs []doctorAgentDir `json:"agent_dirs"`
}

type doctorAgentDir struct {
	Configured string `json:"configured"`
	Resolved   string `json:"resolved"`
	Exists     bool   `json:"exists"`
	IsDir      bool   `json:"is_dir"`
	Warning    string `json:"warning,omitempty"`
}

type doctorCase struct {
	ID string `json:"id"`
	PR string `json:"pr"`
}

// Register attaches the benchmark command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Validate, inspect, and run benchmark suites",
	}
	cmd.AddCommand(newValidateCommand(opts), newDoctorCommand(opts), newRunCommand(opts), newCompareCommand(opts))
	rootCmd.AddCommand(cmd)
}

func newValidateCommand(opts *root.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <suite.yml>",
		Short: "Validate a benchmark suite",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("benchmark validate requires one suite path"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			suite, err := loadAndValidateSuite(opts, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(opts.Stdout, "Benchmark suite %q is valid: %d candidates, %d cases\n", suite.Suite.ID, len(suite.Candidates), len(suite.Cases))
			return err
		},
	}
}

func newDoctorCommand(opts *root.Options) *cobra.Command {
	var flags doctorFlags
	cmd := &cobra.Command{
		Use:   "doctor <suite.yml>",
		Short: "Inspect benchmark suite readiness",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitcode.Usage(fmt.Errorf("benchmark doctor requires one suite path"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			suite, cfg, err := loadConfigAndSuite(opts, args[0])
			if err != nil {
				return err
			}
			report, err := buildDoctorReport(suite, cfg, flags)
			if err != nil {
				return mapBenchmarkError(err)
			}
			if flags.jsonOutput {
				enc := json.NewEncoder(opts.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			return renderDoctorText(opts, report)
		},
	}
	cmd.Flags().StringArrayVar(&flags.candidates, "candidate", nil, "Candidate ID to inspect")
	cmd.Flags().StringArrayVar(&flags.cases, "case", nil, "Case ID to inspect")
	cmd.Flags().StringVar(&flags.resultsDir, "results-dir", "", "Benchmark results directory; defaults to .cr-bench/results/<suite-id> under the current working directory")
	cmd.Flags().StringVar(&flags.crBin, "cr-bin", "", "cr binary to inspect")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
	return cmd
}

func loadAndValidateSuite(opts *root.Options, suitePath string) (benchmark.SuiteFile, error) {
	suite, _, err := loadConfigAndSuite(opts, suitePath)
	return suite, err
}

func loadConfigAndSuite(opts *root.Options, suitePath string) (benchmark.SuiteFile, config.File, error) {
	suite, err := benchmark.LoadFile(suitePath)
	if err != nil {
		return benchmark.SuiteFile{}, config.File{}, mapBenchmarkError(err)
	}
	path, err := configPath(opts)
	if err != nil {
		return benchmark.SuiteFile{}, config.File{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return benchmark.SuiteFile{}, config.File{}, cmderr.Config(err)
	}
	if err := benchmark.ValidateForRun(suite, cfg); err != nil {
		return benchmark.SuiteFile{}, config.File{}, mapBenchmarkError(err)
	}
	return suite, cfg, nil
}

func buildDoctorReport(suite benchmark.SuiteFile, cfg config.File, flags doctorFlags) (doctorReport, error) {
	selectedCandidates, selectedCases, err := benchmark.Select(suite, flags.candidates, flags.cases)
	if err != nil {
		return doctorReport{}, err
	}
	resultsDir, err := resolveResultsDir(suite.Suite.ID, flags.resultsDir)
	if err != nil {
		return doctorReport{}, err
	}
	crBin, warnings := resolveCRBin(flags.crBin)
	report := doctorReport{
		SuiteID:            suite.Suite.ID,
		SuitePath:          suite.Path,
		ResolvedResultsDir: resultsDir,
		CRBin:              crBin,
		Candidates:         make([]doctorCandidate, 0, len(selectedCandidates)),
		Cases:              make([]doctorCase, 0, len(selectedCases)),
		Warnings:           warnings,
	}
	suiteDir := filepath.Dir(suite.Path)
	for _, candidate := range selectedCandidates {
		profile, ok := cfg.Profiles[candidate.Profile]
		out := doctorCandidate{
			ID:               candidate.ID,
			Profile:          candidate.Profile,
			ProfileAvailable: ok,
			Stages: doctorStages{
				Selection: doctorSelectionStage{
					Model:  candidate.Stages.Selection.Model,
					Effort: candidate.Stages.Selection.Effort,
					Prompt: candidate.Stages.Selection.Prompt,
				},
				Reviewers: doctorReviewerStage{
					Model:     candidate.Stages.Reviewers.Model,
					Effort:    candidate.Stages.Reviewers.Effort,
					AgentDirs: make([]doctorAgentDir, 0, len(candidate.Stages.Reviewers.AgentDirs)),
				},
			},
		}
		if ok {
			out.GitHost = profile.Git.Host
		}
		for _, dir := range candidate.Stages.Reviewers.AgentDirs {
			agentDir := inspectAgentDir(suiteDir, dir)
			if agentDir.Warning != "" {
				report.Warnings = append(report.Warnings, fmt.Sprintf("candidate %s agent dir %s: %s", candidate.ID, dir, agentDir.Warning))
			}
			out.Stages.Reviewers.AgentDirs = append(out.Stages.Reviewers.AgentDirs, agentDir)
		}
		report.Candidates = append(report.Candidates, out)
	}
	for _, benchCase := range selectedCases {
		report.Cases = append(report.Cases, doctorCase{ID: benchCase.ID, PR: benchCase.PR})
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	return report, nil
}

func renderDoctorText(opts *root.Options, report doctorReport) error {
	if _, err := fmt.Fprintf(opts.Stdout, "Benchmark suite: %s\n", report.SuiteID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Candidates: %d\n", len(report.Candidates)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Cases: %d\n", len(report.Cases)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Results dir: %s\n", report.ResolvedResultsDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "cr binary: %s\n", report.CRBin); err != nil {
		return err
	}
	for _, candidate := range report.Candidates {
		if _, err := fmt.Fprintf(
			opts.Stdout,
			"- candidate %s profile=%s available=%t selection=%s/%s reviewers=%s/%s reviewer_agent_dirs=%d\n",
			candidate.ID,
			candidate.Profile,
			candidate.ProfileAvailable,
			candidate.Stages.Selection.Model,
			candidate.Stages.Selection.Effort,
			candidate.Stages.Reviewers.Model,
			candidate.Stages.Reviewers.Effort,
			len(candidate.Stages.Reviewers.AgentDirs),
		); err != nil {
			return err
		}
	}
	for _, benchCase := range report.Cases {
		if _, err := fmt.Fprintf(opts.Stdout, "- case %s pr=%s\n", benchCase.ID, benchCase.PR); err != nil {
			return err
		}
	}
	if len(report.Warnings) == 0 {
		_, err := fmt.Fprintln(opts.Stdout, "Warnings: 0")
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "Warnings: %d\n", len(report.Warnings)); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		if _, err := fmt.Fprintf(opts.Stdout, "- %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func resolveResultsDir(suiteID, configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		path = filepath.Join(".cr-bench", "results", suiteID)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("benchmark: resolve results dir: %w", err)
	}
	return abs, nil
}

func resolveCRBin(configured string) (string, []string) {
	var warnings []string
	path := strings.TrimSpace(configured)
	if path == "" {
		exe, err := os.Executable()
		if err != nil || strings.TrimSpace(exe) == "" {
			return "<unresolved>", []string{"could not resolve current cr binary"}
		}
		return exe, warnings
	}
	if hasPathSeparator(path) {
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err == nil {
				path = abs
			}
		}
		if err := checkExecutable(path); err != nil {
			warnings = append(warnings, fmt.Sprintf("cr binary %s is not available: %v", path, err))
		}
		return path, warnings
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return path, []string{fmt.Sprintf("cr binary %s was not found in PATH", path)}
	}
	return resolved, warnings
}

func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable")
	}
	return nil
}

func inspectAgentDir(suiteDir, configured string) doctorAgentDir {
	resolved := configured
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(suiteDir, configured)
	}
	abs, err := filepath.Abs(resolved)
	if err == nil {
		resolved = abs
	}
	out := doctorAgentDir{Configured: configured, Resolved: resolved}
	info, err := os.Stat(resolved)
	if err != nil {
		out.Warning = fmt.Sprintf("not available: %v", err)
		return out
	}
	out.Exists = true
	out.IsDir = info.IsDir()
	if !out.IsDir {
		out.Warning = "not a directory"
	}
	return out
}

func mapBenchmarkError(err error) error {
	if errors.Is(err, benchmark.ErrInvalid) {
		return exitcode.Usage(err)
	}
	return err
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func hasPathSeparator(path string) bool {
	return strings.ContainsRune(path, os.PathSeparator) || strings.Contains(path, "/") || runtime.GOOS == "windows" && strings.Contains(path, `\`)
}
