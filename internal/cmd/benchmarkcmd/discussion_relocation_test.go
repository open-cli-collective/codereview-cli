package benchmarkcmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// Exercise the benchmark-to-view boundary with the union of replay and
// relocation artifact fields. The injected runner makes no provider/LLM calls.
func TestPinnedDiscussionFreeBenchmarkPreservesRelocationArtifactPaths(t *testing.T) {
	resultsDir := t.TempDir()
	pipelineDir := t.TempDir()
	paths := pipeline.ArtifactPaths{
		Dir:             pipelineDir,
		RelocationsJSON: filepath.Join(pipelineDir, "relocations.json"),
		CoverageJSON:    filepath.Join(pipelineDir, "coverage.json"),
	}
	candidate := benchmark.Candidate{ID: "candidate", Profile: "home"}
	benchCase := benchmark.Case{
		ID:            "pinned",
		PR:            "https://github.com/open-cli-collective/codereview-cli/pull/1",
		ReviewBaseSHA: "1111111",
		ReviewHeadSHA: "2222222",
	}
	executor := inProcessExecutor{
		cfg: testConfig(),
		open: func(context.Context, app.OpenRequest) (app.Runtime, error) {
			return app.Runtime{Runner: benchmarkTestRunner{dryRun: func(_ context.Context, req pipeline.Request) (pipeline.Result, error) {
				if !req.WithoutDiscussion || req.ReviewBaseSHA != benchCase.ReviewBaseSHA || req.ReviewHeadSHA != benchCase.ReviewHeadSHA {
					t.Fatalf("pipeline request = %#v, want pinned discussion-free recipe", req)
				}
				return pipeline.Result{
					Run:               ledger.Run{RunID: "pinned-relocation", ArtifactPath: pipelineDir},
					PR:                gitprovider.PR{URL: req.PRURL},
					Artifacts:         paths,
					ReviewBaseSHA:     req.ReviewBaseSHA,
					ReviewHeadSHA:     req.ReviewHeadSHA,
					WithoutDiscussion: req.WithoutDiscussion,
				}, nil
			}}}, nil
		},
	}
	run, err := executeBenchmarkRun(context.Background(), root.NewProgressLogger(nil), resultsDir, resultsDir, benchmarkInProcessCRBin, "run", candidate, benchCase, executor)
	if err != nil {
		t.Fatalf("executeBenchmarkRun: %v", err)
	}
	if !run.RequestedWithoutDiscussion || !run.WithoutDiscussionVerified || run.FailureClassification != failureNone {
		t.Fatalf("run = %#v, want verified discussion-free execution", run)
	}
	var review view.ReviewDryRun
	if err := json.Unmarshal([]byte(discussionReadFileString(t, resultsDir, run.Artifacts.ReviewJSON)), &review); err != nil {
		t.Fatalf("review JSON: %v", err)
	}
	if !review.Run.WithoutDiscussion || review.Run.BaseSHA != benchCase.ReviewBaseSHA || review.Run.HeadSHA != benchCase.ReviewHeadSHA || review.Artifacts.RelocationsJSON != paths.RelocationsJSON || review.Artifacts.CoverageJSON != paths.CoverageJSON {
		t.Fatalf("review = %#v, want pinned replay marker and relocation/coverage paths together", review)
	}
	comparison := buildComparison(benchmarkSuiteSummary{
		SuiteID:            "pinned-relocation",
		SelectedCandidates: summarizeCandidates(resultsDir, []benchmark.Candidate{candidate}),
		SelectedCases:      summarizeCases([]benchmark.Case{benchCase}),
		Runs:               []benchmarkRun{run},
	}, resultsDir)
	if !comparison.Runs[0].WithoutDiscussionVerified || comparison.Runs[0].Status != runStatusCompleted {
		t.Fatalf("comparison = %#v, want current artifact to verify discussion isolation", comparison.Runs[0])
	}
}
