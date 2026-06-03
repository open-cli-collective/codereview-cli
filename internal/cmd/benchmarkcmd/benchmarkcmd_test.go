package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestValidateCommandSucceeds(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))

	if err := root.Execute(cmd, []string{"benchmark", "validate", suitePath}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), `Benchmark suite "suite1" is valid: 2 candidates, 2 cases`) {
		t.Fatalf("stdout = %q, want valid summary", out.String())
	}
}

func TestValidateCommandReportsUsageError(t *testing.T) {
	cmd, _ := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, strings.Replace(validBenchmarkSuite(t), "profile: home", "profile: missing", 1))

	err := root.Execute(cmd, []string{"benchmark", "validate", suitePath})
	if err == nil {
		t.Fatal("Execute error = nil, want validation error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("error = %v, want unknown profile detail", err)
	}
}

func TestDoctorJSONReportsSelectedReadiness(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := filepath.Join(t.TempDir(), "cr")
	if err := os.WriteFile(crBin, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile cr bin: %v", err)
	}
	// #nosec G302 -- this fixture must be executable for doctor readiness checks.
	if err := os.Chmod(crBin, 0o700); err != nil {
		t.Fatalf("Chmod cr bin: %v", err)
	}
	resultsDir := filepath.Join(t.TempDir(), "results")

	err := root.Execute(cmd, []string{
		"benchmark", "doctor", suitePath,
		"--candidate", "second",
		"--case", "case_two",
		"--results-dir", resultsDir,
		"--cr-bin", crBin,
		"--json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.SuiteID != "suite1" || got.SuitePath != suitePath {
		t.Fatalf("suite fields = %#v, want suite1 path %s", got, suitePath)
	}
	if got.ResolvedResultsDir != resultsDir || got.CRBin != crBin {
		t.Fatalf("resolved paths = results:%q cr:%q, want %q/%q", got.ResolvedResultsDir, got.CRBin, resultsDir, crBin)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].ID != "second" || got.Candidates[0].Model != "kimi" || got.Candidates[0].Effort != "low" {
		t.Fatalf("candidates = %#v, want selected second", got.Candidates)
	}
	if !got.Candidates[0].ProfileAvailable || got.Candidates[0].GitHost != "github.com" {
		t.Fatalf("profile readiness = %#v, want available github.com", got.Candidates[0])
	}
	if len(got.Candidates[0].AgentDirs) != 2 {
		t.Fatalf("agent dirs = %#v, want existing and missing dirs", got.Candidates[0].AgentDirs)
	}
	if !got.Candidates[0].AgentDirs[0].Exists || !got.Candidates[0].AgentDirs[0].IsDir {
		t.Fatalf("first agent dir = %#v, want existing dir", got.Candidates[0].AgentDirs[0])
	}
	if got.Candidates[0].AgentDirs[1].Exists || got.Candidates[0].AgentDirs[1].Warning == "" {
		t.Fatalf("second agent dir = %#v, want missing warning", got.Candidates[0].AgentDirs[1])
	}
	if len(got.Cases) != 1 || got.Cases[0].ID != "case_two" {
		t.Fatalf("cases = %#v, want selected case_two", got.Cases)
	}
	if len(got.Warnings) == 0 {
		t.Fatalf("warnings = %#v, want missing agent dir warning", got.Warnings)
	}
}

func TestDoctorJSONUsesDefaultExecutable(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))

	if err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath, "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.CRBin == "" {
		t.Fatalf("cr_bin = empty, want current executable")
	}
}

func TestDoctorJSONResolvesCRBinFromPATH(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	binDir := t.TempDir()
	binName := "cr-test-bin"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	crBin := filepath.Join(binDir, binName)
	if err := os.WriteFile(crBin, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile cr bin: %v", err)
	}
	// #nosec G302 -- this fixture must be executable for PATH resolution checks.
	if err := os.Chmod(crBin, 0o700); err != nil {
		t.Fatalf("Chmod cr bin: %v", err)
	}
	t.Setenv("PATH", binDir)

	if err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath, "--cr-bin", binName, "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.CRBin != crBin || len(got.Warnings) == 0 {
		t.Fatalf("cr_bin = %q warnings=%#v, want PATH-resolved %q plus missing agent-dir warning", got.CRBin, got.Warnings, crBin)
	}
}

func TestDoctorJSONWarnsForMissingCRBin(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))

	if err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath, "--cr-bin", "definitely-missing-cr-bin", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !strings.Contains(strings.Join(got.Warnings, "\n"), "was not found in PATH") {
		t.Fatalf("warnings = %#v, want missing cr-bin warning", got.Warnings)
	}
}

func TestDoctorDoesNotCreateResultsDir(t *testing.T) {
	cmd, _ := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")

	if err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath, "--results-dir", resultsDir}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(resultsDir); !os.IsNotExist(err) {
		t.Fatalf("results dir stat error = %v, want not created", err)
	}
}

func TestDoctorRejectsUnknownSelection(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "candidate", args: []string{"--candidate", "missing"}},
		{name: "case", args: []string{"--case", "missing"}},
		{name: "empty candidate", args: []string{"--candidate", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := newTestCommand(t)
			suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
			args := append([]string{"benchmark", "doctor", suitePath}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "filter") {
				t.Fatalf("error = %v, want selection detail", err)
			}
		})
	}
}

func TestDoctorWarnsForNonExecutableCRBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows executable readiness does not use Unix execute bits")
	}
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := filepath.Join(t.TempDir(), "cr")
	if err := os.WriteFile(crBin, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile cr bin: %v", err)
	}

	err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath, "--cr-bin", crBin, "--json"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, "\n"), "not executable") {
		t.Fatalf("warnings = %#v, want non-executable cr-bin warning", got.Warnings)
	}
}

func TestDoctorTextUsesDefaultResultsDir(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	wantResultsDir, err := filepath.Abs(filepath.Join(".cr-bench", "results", "suite1"))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	if err := root.Execute(cmd, []string{"benchmark", "doctor", suitePath}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stdout := out.String()
	for _, want := range []string{
		"Benchmark suite: suite1",
		"Candidates: 2",
		"Cases: 2",
		"Results dir: " + wantResultsDir,
		"candidate first profile=home available=true model=sonnet effort=high agent_dirs=1",
		"case case_one pr=https://github.com/open-cli-collective/codereview-cli/pull/1",
		"Warnings: 1",
		"agent dir",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want substring %q", stdout, want)
		}
	}
}

func TestRunExecutesSelectedMatrixAndWritesArtifacts(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := writeExecutableCRBin(t)
	resultsDir := filepath.Join(t.TempDir(), "results")
	var invocations []reviewInvocation
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(_ context.Context, gotCRBin string, args []string) reviewCommandResult {
		invocations = append(invocations, reviewInvocation{crBin: gotCRBin, args: append([]string(nil), args...)})
		switch len(invocations) {
		case 1:
			return reviewCommandResult{
				Stdout:   reviewDryRunJSON(t, "child-run-1", "major"),
				Stderr:   []byte("first stderr\n"),
				ExitCode: 0,
				Duration: 1500 * time.Millisecond,
			}
		case 2:
			return reviewCommandResult{
				Stdout:   reviewDryRunJSON(t, "child-run-2"),
				Stderr:   []byte("second stderr\n"),
				ExitCode: 7,
				Duration: 2500 * time.Millisecond,
			}
		case 3:
			return reviewCommandResult{
				Stdout:   reviewDryRunJSON(t, "child-run-3", "minor"),
				Stderr:   []byte("third stderr\n"),
				ExitCode: 0,
				Duration: 3500 * time.Millisecond,
			}
		default:
			return reviewCommandResult{
				Stdout:   reviewDryRunJSON(t, "child-run-4"),
				Stderr:   []byte("fourth stderr\n"),
				ExitCode: 0,
				Duration: 4500 * time.Millisecond,
			}
		}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--results-dir", resultsDir,
		"--cr-bin", crBin,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ResultsDir != resultsDir || got.CRBin != crBin || got.RunCount != 4 || got.SuccessCount != 3 || got.FailureCount != 1 {
		t.Fatalf("summary = %#v, want full 2x2 matrix with one failure", got)
	}
	if got.SuiteSHA256 == "" || got.StartedAt != fixedBenchmarkTime().Format(time.RFC3339) || got.CompletedAt != fixedBenchmarkTime().Format(time.RFC3339) {
		t.Fatalf("suite provenance/timestamps = hash:%q started:%q completed:%q", got.SuiteSHA256, got.StartedAt, got.CompletedAt)
	}
	if got.SeverityCounts["major"] != 1 || got.SeverityCounts["minor"] != 1 {
		t.Fatalf("severity counts = %#v, want major and minor findings", got.SeverityCounts)
	}
	if len(got.SelectedCandidates) != 2 || got.SelectedCandidates[0].ID != "first" || got.SelectedCandidates[1].ID != "second" {
		t.Fatalf("selected candidates = %#v, want both candidates", got.SelectedCandidates)
	}
	if len(got.SelectedCandidates[0].AgentDirs) != 1 || got.SelectedCandidates[0].AgentDirs[0].DirMetadataHash == "" {
		t.Fatalf("first agent dir metadata = %#v, want metadata hash", got.SelectedCandidates[0].AgentDirs)
	}
	if len(got.SelectedCandidates[1].AgentDirs) != 2 || got.SelectedCandidates[1].AgentDirs[1].Warning == "" {
		t.Fatalf("second agent dir metadata = %#v, want missing-dir warning", got.SelectedCandidates[1].AgentDirs)
	}
	if len(got.Runs) != 4 ||
		got.Runs[0].RunID != "0001-c01-k01-first-case_one" ||
		got.Runs[1].RunID != "0002-c01-k02-first-case_two" ||
		got.Runs[2].RunID != "0003-c02-k01-second-case_one" ||
		got.Runs[3].RunID != "0004-c02-k02-second-case_two" {
		t.Fatalf("runs = %#v, want indexed run IDs", got.Runs)
	}
	if got.Runs[0].FindingCount != 1 || got.Runs[0].ReviewRunID != "child-run-1" ||
		got.Runs[1].ExitCode != 7 ||
		got.Runs[2].ExitCode != 0 || got.Runs[2].ReviewRunID != "child-run-3" ||
		got.Runs[3].ExitCode != 0 || got.Runs[3].ReviewRunID != "child-run-4" {
		t.Fatalf("run summaries = %#v, want parsed first run, early failure, and later completion", got.Runs)
	}
	if len(invocations) != 4 {
		t.Fatalf("invocations = %d, want 4", len(invocations))
	}
	wantFirstArgs := []string{
		"--profile", "home",
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/1",
		"--dry-run", "--json",
		"--llm-model", "sonnet",
		"--llm-effort", "high",
		"--agents-dir", got.SelectedCandidates[0].AgentDirs[0].Resolved,
		"--max-agents", "5",
		"--max-concurrency", "3",
	}
	if strings.Join(invocations[0].args, "\x00") != strings.Join(wantFirstArgs, "\x00") {
		t.Fatalf("first args = %#v, want %#v", invocations[0].args, wantFirstArgs)
	}
	wantSecondArgs := []string{
		"--profile", "home",
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/2",
		"--dry-run", "--json",
		"--llm-model", "sonnet",
		"--llm-effort", "high",
		"--agents-dir", got.SelectedCandidates[0].AgentDirs[0].Resolved,
		"--max-agents", "5",
		"--max-concurrency", "3",
	}
	if strings.Join(invocations[1].args, "\x00") != strings.Join(wantSecondArgs, "\x00") {
		t.Fatalf("second args = %#v, want %#v", invocations[1].args, wantSecondArgs)
	}
	wantThirdArgs := []string{
		"--profile", "home",
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/1",
		"--dry-run", "--json",
		"--llm-model", "kimi",
		"--llm-effort", "low",
		"--agents-dir", got.SelectedCandidates[1].AgentDirs[0].Resolved,
		"--agents-dir", got.SelectedCandidates[1].AgentDirs[1].Resolved,
	}
	if strings.Join(invocations[2].args, "\x00") != strings.Join(wantThirdArgs, "\x00") {
		t.Fatalf("third args = %#v, want %#v", invocations[2].args, wantThirdArgs)
	}
	wantFourthArgs := []string{
		"--profile", "home",
		"review", "https://github.com/open-cli-collective/codereview-cli/pull/2",
		"--dry-run", "--json",
		"--llm-model", "kimi",
		"--llm-effort", "low",
		"--agents-dir", got.SelectedCandidates[1].AgentDirs[0].Resolved,
		"--agents-dir", got.SelectedCandidates[1].AgentDirs[1].Resolved,
	}
	if strings.Join(invocations[3].args, "\x00") != strings.Join(wantFourthArgs, "\x00") {
		t.Fatalf("fourth args = %#v, want %#v", invocations[3].args, wantFourthArgs)
	}
	for _, invocation := range invocations {
		if invocation.crBin != crBin {
			t.Fatalf("cr bin = %q, want %q", invocation.crBin, crBin)
		}
		if !stringSliceContains(invocation.args, "--dry-run") || !stringSliceContains(invocation.args, "--json") {
			t.Fatalf("args = %#v, want every child invocation to include --dry-run and --json", invocation.args)
		}
		for _, forbidden := range []string{"--no-post", "--rerun", "--retry-posts", "--approve", "--session", "--allow-self-approve"} {
			if stringSliceContains(invocation.args, forbidden) {
				t.Fatalf("args = %#v, contains forbidden live/posting flag %s", invocation.args, forbidden)
			}
		}
	}
	assertFileContains(t, got.Artifacts.Manifest, `"suite_id": "suite1"`)
	assertFileContains(t, got.Artifacts.SuiteSummary, `"failure_count": 1`)
	assertFileContains(t, got.Artifacts.SummaryJSONL, `"run_id":"0001-c01-k01-first-case_one"`)
	assertFileContains(t, got.Artifacts.Report, "| `0004-c02-k02-second-case_two` |")
	assertFileContains(t, got.Artifacts.Report, "| n/a | n/a |")
	assertFileContains(t, got.Runs[0].Artifacts.ReviewJSON, `"run_id":"child-run-1"`)
	assertFileContains(t, got.Runs[1].Artifacts.Stderr, "second stderr")
	assertFileContains(t, got.Runs[0].Artifacts.MetricsJSON, `"finding_count": 1`)
	assertBenchmarkArtifactJSON(t, got)
}

func TestRunExtractsUsageMetricsFromReviewArtifacts(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := writeExecutableCRBin(t)
	resultsDir := filepath.Join(t.TempDir(), "results")
	artifactPath := t.TempDir()
	logDir := filepath.Join(artifactPath, "agent-logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll agent logs: %v", err)
	}
	writeLog(t, filepath.Join(logDir, "frontend%3Afrontend-code-reviewer.jsonl"), `{"type":"turn_start"}
{"type":"message_end","message":{"provider":"opencode-go","model":"qwen3.6-plus","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1,"totalTokens":18,"cost":{"input":0.1,"output":0.2,"cacheRead":0.01,"cacheWrite":0.03,"total":0.34}},"stopReason":"stop"}}
`)
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		return reviewCommandResult{Stdout: reviewDryRunJSONWithArtifact(t, "child-run-1", artifactPath, "minor"), ExitCode: 0}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
		"--cr-bin", crBin,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Usage == nil || got.Usage.Tokens.TotalTokens != 18 || got.Usage.Cost.Total != 0.34 {
		t.Fatalf("suite usage = %#v, want aggregate provider usage", got.Usage)
	}
	if got.Runs[0].Usage == nil || got.Runs[0].Usage.Phases[0].Name != "frontend:frontend-code-reviewer" {
		t.Fatalf("run usage = %#v, want phase metrics", got.Runs[0].Usage)
	}
	assertFileContains(t, got.Artifacts.SuiteSummary, `"usage"`)
	assertFileContains(t, got.Artifacts.SummaryJSONL, `"total_tokens":18`)
	assertFileContains(t, got.Artifacts.Report, "Tokens")
	assertFileContains(t, got.Runs[0].Artifacts.MetricsJSON, `"cost"`)
}

func TestRenderReportMarkdownTreatsActivityOnlyUsageAsUnavailable(t *testing.T) {
	report := renderReportMarkdown(benchmarkSuiteSummary{
		SuiteID:      "suite1",
		ResultsDir:   "/tmp/results",
		RunCount:     1,
		SuccessCount: 1,
		Runs: []benchmarkRun{
			{
				RunID:        "run1",
				CandidateID:  "candidate1",
				CaseID:       "case1",
				FindingCount: 1,
				Usage: &benchmark.RunMetrics{
					Turns:     2,
					ToolCalls: 3,
				},
			},
		},
		Usage: &benchmark.RunMetrics{
			Turns:     2,
			ToolCalls: 3,
		},
	})
	if strings.Contains(report, "- Tokens: 0 total") || strings.Contains(report, "- Cost: $0.000000") {
		t.Fatalf("report rendered activity-only summary as provider usage:\n%s", report)
	}
	if !strings.Contains(report, "| `run1` | `candidate1` | `case1` | 0 | 1 | n/a | n/a |") {
		t.Fatalf("report missing activity-only n/a row:\n%s", report)
	}
}

func TestRunInvalidChildJSONWarnsAndContinues(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := writeExecutableCRBin(t)
	var invocations int
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		invocations++
		if invocations == 1 {
			return reviewCommandResult{Stdout: []byte("{bad json"), ExitCode: 0}
		}
		return reviewCommandResult{Stdout: reviewDryRunJSON(t, "child-run-2", "minor"), ExitCode: 0}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--candidate", "first",
		"--results-dir", filepath.Join(t.TempDir(), "results"),
		"--cr-bin", crBin,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if invocations != 2 || got.RunCount != 2 || got.SuccessCount != 2 {
		t.Fatalf("invocations=%d summary=%#v, want both runs completed", invocations, got)
	}
	if len(got.Runs[0].Warnings) == 0 || !strings.Contains(got.Runs[0].Warnings[0], "parse failed") {
		t.Fatalf("first run warnings = %#v, want JSON parse warning", got.Runs[0].Warnings)
	}
	assertFileContains(t, got.Runs[0].Artifacts.ReviewJSON, "{bad json")
	if got.Runs[1].FindingCount != 1 || got.SeverityCounts["minor"] != 1 {
		t.Fatalf("second run/summary = %#v counts=%#v, want later parsed run", got.Runs[1], got.SeverityCounts)
	}
}

func TestRunReviewCommandRealCapturesStreamsAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script subprocess fixture is POSIX-only")
	}
	script := filepath.Join(t.TempDir(), "fake-cr")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'stdout body'\nprintf 'stderr body' >&2\nexit 7\n"), 0o600); err != nil {
		t.Fatalf("WriteFile script: %v", err)
	}
	// #nosec G302 -- subprocess smoke test requires an executable script fixture.
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatalf("Chmod script: %v", err)
	}

	got := runReviewCommandReal(context.Background(), script, []string{"--ignored"})
	if string(got.Stdout) != "stdout body" || string(got.Stderr) != "stderr body" || got.ExitCode != 7 || got.Err == nil {
		t.Fatalf("result = stdout:%q stderr:%q exit:%d err:%v, want split streams and exit 7", got.Stdout, got.Stderr, got.ExitCode, got.Err)
	}
	if got.Duration < 0 {
		t.Fatalf("duration = %s, want non-negative", got.Duration)
	}
}

func TestRunRejectsMissingCRBinBeforeArtifacts(t *testing.T) {
	cmd, _ := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")
	var invocations int
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		invocations++
		return reviewCommandResult{}
	})

	err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--results-dir", resultsDir,
		"--cr-bin", filepath.Join(t.TempDir(), "missing-cr"),
	})
	if err == nil {
		t.Fatal("Execute error = nil, want missing cr-bin usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if invocations != 0 {
		t.Fatalf("invocations = %d, want none", invocations)
	}
	if _, statErr := os.Stat(resultsDir); !os.IsNotExist(statErr) {
		t.Fatalf("results dir stat error = %v, want not created", statErr)
	}
}

func TestRunRejectsInvalidSelectionAndSuite(t *testing.T) {
	tests := []struct {
		name string
		body string
		args []string
		want string
	}{
		{name: "unknown candidate", body: validBenchmarkSuite(t), args: []string{"--candidate", "missing"}, want: "unknown candidate"},
		{name: "invalid suite", body: strings.Replace(validBenchmarkSuite(t), "profile: home", "profile: missing", 1), args: nil, want: "unknown profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := newTestCommand(t)
			suitePath := writeBenchmarkSuite(t, tt.body)
			var invocations int
			withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
				invocations++
				return reviewCommandResult{}
			})

			args := append([]string{"benchmark", "run", suitePath}, tt.args...)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if invocations != 0 {
				t.Fatalf("invocations = %d, want none", invocations)
			}
		})
	}
}

func TestRunDefaultResultsDirUsesTimestamp(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	got, err := resolveRunResultsDir("suite1", "", time.Date(2026, 6, 3, 18, 45, 12, 999, time.UTC))
	if err != nil {
		t.Fatalf("resolveRunResultsDir: %v", err)
	}
	want := filepath.Join(tmp, ".cr-bench", "results", "suite1", "2026-06-03T184512Z")
	if got != want {
		t.Fatalf("results dir = %q, want %q", got, want)
	}
}

func TestRunReusesExplicitResultsDirAndOverwritesOwnedArtifacts(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	crBin := writeExecutableCRBin(t)
	resultsDir := t.TempDir()
	unknownPath := filepath.Join(resultsDir, "unknown.txt")
	runDir := filepath.Join(resultsDir, "0001-c01-k01-first-case_one")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("MkdirAll run dir: %v", err)
	}
	permissiveArtifacts := []string{
		filepath.Join(resultsDir, "manifest.json"),
		filepath.Join(resultsDir, "summary.jsonl"),
		filepath.Join(resultsDir, "suite-summary.json"),
		filepath.Join(resultsDir, "report.md"),
		filepath.Join(runDir, "review.json"),
		filepath.Join(runDir, "stderr.txt"),
		filepath.Join(runDir, "metrics.json"),
	}
	for _, path := range permissiveArtifacts {
		writePermissiveBenchmarkArtifact(t, path, "old summary")
	}
	if err := os.WriteFile(unknownPath, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("WriteFile unknown: %v", err)
	}
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		return reviewCommandResult{Stdout: reviewDryRunJSON(t, "child-run-1"), ExitCode: 0}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
		"--cr-bin", crBin,
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	assertFileContains(t, got.Artifacts.SummaryJSONL, `"run_id":"0001-c01-k01-first-case_one"`)
	if strings.Contains(readFile(t, got.Artifacts.SummaryJSONL), "old summary") {
		t.Fatalf("summary.jsonl still contains old content")
	}
	assertFileContains(t, unknownPath, "keep me")
	if runtime.GOOS != "windows" {
		for _, path := range permissiveArtifacts {
			assertFileMode(t, path, 0o600)
		}
	}
}

func newTestCommand(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(cfgPath, testConfig()); err != nil {
		t.Fatalf("config Save: %v", err)
	}
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: cfgPath,
		Stdout:     &out,
		Stderr:     &bytes.Buffer{},
	})
	Register(cmd, opts)
	return cmd, &out
}

func writeBenchmarkSuite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile suite: %v", err)
	}
	return path
}

func validBenchmarkSuite(t *testing.T) string {
	t.Helper()
	agentDir := t.TempDir()
	missingAgentDir := filepath.Join(t.TempDir(), "missing")
	body := `
suite:
  id: suite1
  name: Suite One
  version: 1
candidates:
  - id: first
    profile: home
    model: sonnet
    effort: high
    agent_dirs:
      - AGENT_DIR
    max_agents: 5
    max_concurrency: 3
  - id: second
    profile: home
    model: kimi
    effort: low
    agent_dirs:
      - AGENT_DIR
      - MISSING_AGENT_DIR
cases:
  - id: case_one
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
  - id: case_two
    pr: https://github.com/open-cli-collective/codereview-cli/pull/2
`
	body = strings.ReplaceAll(body, "AGENT_DIR", agentDir)
	return strings.ReplaceAll(body, "MISSING_AGENT_DIR", missingAgentDir)
}

func testConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
			},
		},
	}
}

type reviewInvocation struct {
	crBin string
	args  []string
}

func withBenchmarkRunSeams(t *testing.T, now time.Time, runner func(context.Context, string, []string) reviewCommandResult) {
	t.Helper()
	oldNow := benchmarkNow
	oldRunner := runReviewCommand
	benchmarkNow = func() time.Time { return now }
	runReviewCommand = runner
	t.Cleanup(func() {
		benchmarkNow = oldNow
		runReviewCommand = oldRunner
	})
}

func fixedBenchmarkTime() time.Time {
	return time.Date(2026, 6, 3, 18, 45, 12, 0, time.UTC)
}

func reviewDryRunJSON(t *testing.T, runID string, severities ...string) []byte {
	t.Helper()
	return reviewDryRunJSONWithArtifact(t, runID, "/tmp/"+runID, severities...)
}

func reviewDryRunJSONWithArtifact(t *testing.T, runID, artifactPath string, severities ...string) []byte {
	t.Helper()
	findings := make([]view.ReviewFinding, 0, len(severities))
	for i, severity := range severities {
		findings = append(findings, view.ReviewFinding{
			ID:       "finding-" + severity,
			Severity: severity,
			FilePath: "main.go",
			Body:     "finding body",
			Line:     intPtrForBenchmark(i + 1),
		})
	}
	data, err := json.Marshal(view.ReviewDryRun{
		Run: view.ReviewRun{
			RunID:        runID,
			PRURL:        "https://github.com/open-cli-collective/codereview-cli/pull/1",
			PRKey:        "github.com_open-cli-collective_codereview-cli_1",
			PostMode:     "dry_run",
			Outcome:      "dry_run",
			ArtifactPath: artifactPath,
		},
		Findings: findings,
		Artifacts: view.ReviewArtifacts{
			Dir:            artifactPath,
			FindingsJSON:   filepath.Join(artifactPath, "findings.json"),
			RollupMarkdown: filepath.Join(artifactPath, "rollup.md"),
		},
	})
	if err != nil {
		t.Fatalf("Marshal review dry-run: %v", err)
	}
	return data
}

func writeExecutableCRBin(t *testing.T) string {
	t.Helper()
	name := "cr"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile cr bin: %v", err)
	}
	// #nosec G302 -- benchmark run tests require an executable cr fixture.
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("Chmod cr bin: %v", err)
	}
	return path
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	if !strings.Contains(readFile(t, path), want) {
		t.Fatalf("%s does not contain %q", path, want)
	}
}

func assertBenchmarkArtifactJSON(t *testing.T, summary benchmarkSuiteSummary) {
	t.Helper()
	var manifest benchmarkManifest
	readJSONFile(t, summary.Artifacts.Manifest, &manifest)
	if manifest.SuiteID != summary.SuiteID || len(manifest.Runs) != summary.RunCount || manifest.Runs[3].RunID != summary.Runs[3].RunID {
		t.Fatalf("manifest = %#v, want suite and run list matching summary", manifest)
	}
	var suiteSummary benchmarkSuiteSummary
	readJSONFile(t, summary.Artifacts.SuiteSummary, &suiteSummary)
	if suiteSummary.RunCount != summary.RunCount || suiteSummary.FailureCount != summary.FailureCount || len(suiteSummary.Runs) != summary.RunCount {
		t.Fatalf("suite summary artifact = %#v, want run/failure counts from command output", suiteSummary)
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, summary.Artifacts.SummaryJSONL)), "\n")
	if len(lines) != summary.RunCount {
		t.Fatalf("summary JSONL lines = %d, want %d", len(lines), summary.RunCount)
	}
	for i, line := range lines {
		var run benchmarkRun
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			t.Fatalf("Unmarshal summary line %d: %v\n%s", i, err, line)
		}
		if run.RunID != summary.Runs[i].RunID || run.Artifacts.ReviewJSON == "" {
			t.Fatalf("summary line %d = %#v, want run %s with artifact paths", i, run, summary.Runs[i].RunID)
		}
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads generated artifact path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("Unmarshal %s: %v\n%s", path, err, string(data))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads generated artifact path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

func writeLog(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile log: %v", err)
	}
}

func writePermissiveBenchmarkArtifact(t *testing.T, path, body string) {
	t.Helper()
	// #nosec G306 -- this intentionally creates a permissive preexisting artifact for regression coverage.
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile permissive artifact: %v", err)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func intPtrForBenchmark(value int) *int {
	return &value
}
