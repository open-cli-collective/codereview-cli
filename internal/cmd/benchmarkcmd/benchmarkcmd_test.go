package benchmarkcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
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
