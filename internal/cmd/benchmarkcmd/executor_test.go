package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
)

func TestRunRejectsInProcessWithCRBinBeforeArtifacts(t *testing.T) {
	cmd, _ := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")

	err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--results-dir", resultsDir,
		"--in-process",
		"--cr-bin", "cr",
	})
	if err == nil || exitcode.FromError(err) != exitcode.UsageError {
		t.Fatalf("Execute error = %v, want usage error", err)
	}
	if !strings.Contains(err.Error(), "--in-process and --cr-bin") {
		t.Fatalf("error = %v, want flag conflict", err)
	}
	if _, statErr := os.Stat(resultsDir); !os.IsNotExist(statErr) {
		t.Fatalf("results dir stat error = %v, want not created", statErr)
	}
}

func TestRunInProcessManifestUsesExplicitMarker(t *testing.T) {
	cmd, out := newTestCommand(t)
	suitePath := writeBenchmarkSuite(t, validBenchmarkSuite(t))
	resultsDir := filepath.Join(t.TempDir(), "results")
	withBenchmarkRunSeams(t, fixedBenchmarkTime(), func(context.Context, string, []string) reviewCommandResult {
		return reviewCommandResult{Stdout: reviewDryRunJSON(t, "in-process-run"), ExitCode: exitcode.Success}
	})

	if err := root.Execute(cmd, []string{
		"benchmark", "run", suitePath,
		"--candidate", "first",
		"--case", "case_one",
		"--results-dir", resultsDir,
		"--in-process",
		"--json",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got benchmarkSuiteSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("summary JSON: %v", err)
	}
	if got.CRBin != benchmarkInProcessCRBin {
		t.Fatalf("cr_bin = %q, want %q", got.CRBin, benchmarkInProcessCRBin)
	}
	var manifest benchmarkManifest
	data, err := os.ReadFile(got.Artifacts.Manifest)
	if err != nil {
		t.Fatalf("ReadFile manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if manifest.CRBin != benchmarkInProcessCRBin {
		t.Fatalf("manifest cr_bin = %q, want %q", manifest.CRBin, benchmarkInProcessCRBin)
	}
}

func TestInProcessExecutorOpensAndCleansRuntimePerCell(t *testing.T) {
	var openCount, cleanupCount int
	var openRequests []app.OpenRequest
	var pipelineRequests []pipeline.Request
	var progressOutput bytes.Buffer
	opts := &root.Options{Stderr: &progressOutput}
	logger := root.NewProgressLogger(opts)
	executor := inProcessExecutor{
		opts:   opts,
		cfg:    testConfig(),
		logger: logger,
		open: func(_ context.Context, req app.OpenRequest) (app.Runtime, error) {
			if cleanupCount != openCount {
				t.Fatalf("cleanup count before open = %d, want %d", cleanupCount, openCount)
			}
			openCount++
			openRequests = append(openRequests, req)
			runID := fmt.Sprintf("in-process-run-%d", req.PRRef.Number)
			return app.Runtime{
				Runner: benchmarkTestRunner{dryRun: func(_ context.Context, req pipeline.Request) (pipeline.Result, error) {
					pipelineRequests = append(pipelineRequests, req)
					return pipeline.Result{
						Run: ledger.Run{RunID: runID, ArtifactPath: t.TempDir()},
						PR:  gitprovider.PR{URL: req.PRURL},
					}, nil
				}},
				Cleanup: func() { cleanupCount++ },
			}, nil
		},
	}
	candidate := benchmark.Candidate{
		Profile:        "home",
		MaxAgents:      5,
		MaxConcurrency: 3,
		Stages: benchmark.CandidateStages{
			Selection: benchmark.SelectionStage{Model: "selector"},
			Reviewers: benchmark.ReviewerStage{Model: "reviewer"},
		},
	}
	cases := []benchmark.Case{
		{PR: "https://github.com/open-cli-collective/codereview-cli/pull/1"},
		{PR: "https://github.com/open-cli-collective/codereview-cli/pull/2", ReviewBaseSHA: "1111111", ReviewHeadSHA: "2222222"},
	}
	for _, benchCase := range cases {
		got := executor.Execute(context.Background(), reviewExecutionRequest{Candidate: candidate, Case: benchCase})
		if got.Err != nil || got.Review == nil || got.FailureClassification != failureNone {
			t.Fatalf("Execute(%s) = %#v, want typed success", benchCase.PR, got)
		}
		var persisted map[string]any
		if err := json.Unmarshal(got.Stdout, &persisted); err != nil {
			t.Fatalf("stdout JSON: %v", err)
		}
	}
	if openCount != 2 || cleanupCount != 2 {
		t.Fatalf("runtime lifecycle = opens:%d cleanups:%d, want 2/2", openCount, cleanupCount)
	}
	if openRequests[0].PRRef.Number != 1 || openRequests[1].PRRef.Number != 2 {
		t.Fatalf("open PR refs = %d/%d, want per-case refs", openRequests[0].PRRef.Number, openRequests[1].PRRef.Number)
	}
	for _, req := range openRequests {
		if req.MaxAgents != 5 || req.MaxConcurrency != 3 || req.Command != "benchmark.run" || req.Progress != logger || req.Warnings == nil {
			t.Fatalf("open request = %#v, want candidate limits and benchmark sinks", req)
		}
	}
	if pipelineRequests[1].ReviewBaseSHA != "1111111" || pipelineRequests[1].ReviewHeadSHA != "2222222" {
		t.Fatalf("second pipeline request = %#v, want case SHAs", pipelineRequests[1])
	}
	for _, req := range pipelineRequests {
		if req.ReviewerEffortOverride != "" {
			t.Fatalf("reviewer effort override = %q, want inherited effort", req.ReviewerEffortOverride)
		}
	}
}

func TestInProcessExecutorRecoversPipelinePanicAfterCleanup(t *testing.T) {
	cleanupCount := 0
	executor := inProcessExecutor{
		cfg: testConfig(),
		open: func(context.Context, app.OpenRequest) (app.Runtime, error) {
			return app.Runtime{
				Runner: benchmarkTestRunner{dryRun: func(context.Context, pipeline.Request) (pipeline.Result, error) {
					panic("boom")
				}},
				Cleanup: func() { cleanupCount++ },
			}, nil
		},
	}
	got := executor.Execute(context.Background(), reviewExecutionRequest{
		Candidate: benchmark.Candidate{Profile: "home"},
		Case:      benchmark.Case{PR: "https://github.com/open-cli-collective/codereview-cli/pull/1"},
	})
	if got.ExitCode != -1 || got.FailureClassification != failureChildProcessError || got.Err == nil {
		t.Fatalf("panic result = %#v, want isolated child-process failure", got)
	}
	if cleanupCount != 1 || !strings.Contains(string(got.Stderr), "review pipeline panic: boom") {
		t.Fatalf("cleanup=%d stderr=%q, want cleanup before recovered failure", cleanupCount, got.Stderr)
	}
}

func TestClassifyInProcessFailureMatchesSubprocessTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "none", want: failureNone},
		{name: "usage", err: exitcode.Usage(errors.New("usage")), want: failureUsageError},
		{name: "auth", err: exitcode.AuthConfig(errors.New("auth")), want: failureAuthConfigError},
		{name: "upstream", err: exitcode.Upstream(errors.New("upstream")), want: failureUpstreamError},
		{name: "generic", err: errors.New("failed"), want: failureChildExitNonzero},
		{name: "canceled", err: context.Canceled, want: failureChildProcessError},
		{name: "deadline", err: context.DeadlineExceeded, want: failureChildProcessError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyInProcessFailure(tt.err); got != tt.want {
				t.Fatalf("classification = %q, want %q", got, tt.want)
			}
		})
	}
}

type benchmarkTestRunner struct {
	dryRun func(context.Context, pipeline.Request) (pipeline.Result, error)
}

func (r benchmarkTestRunner) DryRun(ctx context.Context, req pipeline.Request) (pipeline.Result, error) {
	return r.dryRun(ctx, req)
}

func (benchmarkTestRunner) Live(context.Context, pipeline.Request, reviewrun.Flags) (reviewrun.Result, error) {
	return reviewrun.Result{}, errors.New("unexpected live review")
}
