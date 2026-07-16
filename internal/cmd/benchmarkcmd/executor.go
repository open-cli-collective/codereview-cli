package benchmarkcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/appruntime"
	"github.com/open-cli-collective/codereview-cli/internal/benchmark"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/version"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// ReviewExecutor executes one benchmark review cell.
type ReviewExecutor interface {
	Execute(context.Context, reviewExecutionRequest) reviewExecutionResult
}

type reviewExecutionRequest struct {
	CRBin     string
	Args      []string
	SuiteDir  string
	Candidate benchmark.Candidate
	Case      benchmark.Case
}

type reviewExecutionResult struct {
	Review                *view.ReviewDryRun
	Stdout                []byte
	Stderr                []byte
	ExitCode              int
	Duration              time.Duration
	Err                   error
	Warnings              []string
	FailureClassification string
}

type subprocessExecutor struct {
	run func(context.Context, string, []string) reviewCommandResult
}

func (e subprocessExecutor) Execute(ctx context.Context, req reviewExecutionRequest) reviewExecutionResult {
	var child reviewCommandResult
	if e.run != nil {
		child = e.run(ctx, req.CRBin, req.Args)
	} else {
		start := time.Now()
		cmd := exec.CommandContext(ctx, req.CRBin, req.Args...) // #nosec G204 -- benchmark run intentionally invokes a validated cr binary.
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitCode := exitcode.Success
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
		child = reviewCommandResult{
			Stdout:   stdout.Bytes(),
			Stderr:   stderr.Bytes(),
			ExitCode: exitCode,
			Duration: time.Since(start),
			Err:      err,
		}
	}
	parsed, warnings := parseReviewDryRun(child.Stdout, child.ExitCode)
	return reviewExecutionResult{
		Review:                parsed,
		Stdout:                child.Stdout,
		Stderr:                child.Stderr,
		ExitCode:              child.ExitCode,
		Duration:              child.Duration,
		Err:                   child.Err,
		Warnings:              warnings,
		FailureClassification: classifyRuntimeFailure(child.ExitCode, child.Err, child.Stdout, parsed != nil),
	}
}

type inProcessExecutor struct {
	opts               *root.Options
	cfg                config.File
	logger             *progress.Logger
	backendFlagChanged bool
	open               func(context.Context, app.OpenRequest) (app.Runtime, error)
}

func (e inProcessExecutor) Execute(ctx context.Context, req reviewExecutionRequest) (result reviewExecutionResult) {
	started := time.Now()
	var stderr bytes.Buffer
	defer func() {
		result.Duration = time.Since(started)
		if recovered := recover(); recovered != nil {
			result = reviewExecutionResult{
				ExitCode:              -1,
				Duration:              result.Duration,
				Err:                   fmt.Errorf("review pipeline panic: %v", recovered),
				FailureClassification: failureChildProcessError,
			}
			fmt.Fprintln(&stderr, result.Err)
		}
		result.Stderr = append([]byte(nil), stderr.Bytes()...)
	}()

	ref, urlProvider, err := prref.ParsePullURL(req.Case.PR)
	if err != nil {
		return inProcessReviewFailure(exitcode.Usage(err), &stderr)
	}
	profileName, profile, err := config.ResolveProfile(e.cfg, req.Candidate.Profile)
	if err != nil {
		return inProcessReviewFailure(cmdruntime.MapRunError(err), &stderr)
	}
	if !prref.SameHost(ref.Host, profile.Git.Host) {
		return inProcessReviewFailure(exitcode.Usage(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host)), &stderr)
	}
	if err := prref.MatchProvider(urlProvider, string(profile.Git.ProviderKind())); err != nil {
		return inProcessReviewFailure(exitcode.Usage(err), &stderr)
	}
	selectionPrompt, err := loadSelectionPromptInstructions(req.SuiteDir, req.Candidate.Stages.Selection.Prompt)
	if err != nil {
		return inProcessReviewFailure(exitcode.Usage(err), &stderr)
	}

	open := e.open
	if open == nil {
		open = app.Open
	}
	backend := ""
	if e.opts != nil {
		backend = e.opts.Backend
	}
	runtime, err := open(ctx, app.OpenRequest{
		Config:              e.cfg,
		Profile:             profile,
		Backend:             backend,
		BackendFlagChanged:  e.backendFlagChanged,
		Command:             "benchmark.run",
		Progress:            e.logger,
		Warnings:            &stderr,
		PRRef:               ref,
		MaxAgents:           req.Candidate.MaxAgents,
		MaxConcurrency:      req.Candidate.MaxConcurrency,
		Retention:           appruntime.RetentionPolicyFromConfig(e.cfg.Data.Retention),
		RetentionManualOnly: e.cfg.Data.Retention.Enforcement == config.RetentionManualOnly,
	})
	if err != nil {
		return inProcessReviewFailure(cmdruntime.MapRunError(err), &stderr)
	}
	if runtime.Cleanup != nil {
		defer runtime.Cleanup()
	}
	if runtime.Runner == nil {
		return inProcessReviewFailure(fmt.Errorf("review: runtime runner is required"), &stderr)
	}

	agentDirs := make([]string, 0, len(req.Candidate.Stages.Reviewers.AgentDirs))
	for _, dir := range req.Candidate.Stages.Reviewers.AgentDirs {
		agentDirs = append(agentDirs, benchmark.ResolveSuitePath(req.SuiteDir, dir))
	}
	pipelineResult, err := runtime.Runner.DryRun(ctx, pipeline.Request{
		PRRef:                       ref,
		PRURL:                       req.Case.PR,
		ProfileName:                 profileName,
		Profile:                     profile,
		PostingIdentity:             runtime.PostingIdentity,
		AgentDirs:                   agentDirs,
		NoResolveThreads:            profile.ReviewPolicy.ResolveThreads == config.ResolveThreadsNever,
		MajorRequestChanges:         profile.ReviewPolicy.MajorEvent == config.ReviewMajorEventRequestChanges,
		SelectionModelOverride:      req.Candidate.Stages.Selection.Model,
		SelectionEffortOverride:     req.Candidate.Stages.Selection.Effort,
		SelectionPromptInstructions: selectionPrompt,
		ReviewerModelOverride:       req.Candidate.Stages.Reviewers.Model,
		ReviewerModelTierOverride:   req.Candidate.Stages.Reviewers.ModelTier,
		ReviewerEffortOverride:      req.Candidate.Stages.Reviewers.Effort,
		ReviewBaseSHA:               req.Case.ReviewBaseSHA,
		ReviewHeadSHA:               req.Case.ReviewHeadSHA,
		ToolVersion:                 version.Version,
	})
	if err != nil {
		return inProcessReviewFailure(cmdruntime.MapRunError(err), &stderr)
	}
	rendered, err := view.NewReviewDryRun(pipelineResult)
	if err != nil {
		return inProcessReviewFailure(err, &stderr)
	}
	var stdout bytes.Buffer
	if err := view.RenderJSON(&stdout, rendered); err != nil {
		return inProcessReviewFailure(err, &stderr)
	}
	result.Review = &rendered
	result.Stdout = stdout.Bytes()
	result.ExitCode = exitcode.Success
	result.FailureClassification = failureNone
	return result
}

func inProcessReviewFailure(err error, stderr *bytes.Buffer) reviewExecutionResult {
	fmt.Fprintln(stderr, err)
	exitCode := exitcode.FromError(err)
	classification := classifyInProcessFailure(err)
	if classification == failureChildProcessError {
		exitCode = -1
	}
	return reviewExecutionResult{
		ExitCode:              exitCode,
		Err:                   err,
		FailureClassification: classification,
	}
}

func classifyInProcessFailure(err error) string {
	if err == nil {
		return failureNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failureChildProcessError
	}
	switch exitcode.FromError(err) {
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
