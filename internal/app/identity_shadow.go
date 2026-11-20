package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/identityshadow"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
)

func identityShadowSystemOneKeyResolver(cfg config.File, stores *runtimeCredentialStores) func(context.Context, string, string) (string, error) {
	resolve := identityJudgeKeyResolver(cfg, stores)
	return func(ctx context.Context, storeID, name string) (string, error) {
		return resolve(ctx, "system_one", identityshadow.Credential{Store: storeID, Name: name})
	}
}

func identityJudgeKeyResolver(cfg config.File, stores *runtimeCredentialStores) func(context.Context, string, identityshadow.Credential) (string, error) {
	return func(ctx context.Context, kind string, ref identityshadow.Credential) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		location := config.CredentialLocation{Store: ref.Store, Name: ref.Name}
		_, reader, err := stores.Open(location)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := credentials.ReadIdentityAPIKey(ctx, cfg, reader, location, kind)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return key, nil
	}
}

func cloneIdentityShadowSnapshotAndAttemptPath(paths pipeline.ArtifactPaths, snapshot *reportidentitysnapshot.Snapshot, judge bool) (reportidentitysnapshot.Snapshot, string, bool, error) {
	if snapshot == nil {
		return reportidentitysnapshot.Snapshot{}, "", false, nil
	}
	clone := reportidentitysnapshot.Clone(*snapshot)
	id := uuid.NewString()
	if judge {
		attemptDir, err := paths.JudgeAttempt(id)
		return clone, attemptDir, true, err
	}
	attemptDir, err := paths.IdentityShadowAttempt(id)
	if err != nil {
		return reportidentitysnapshot.Snapshot{}, "", true, err
	}
	return clone, attemptDir, true, nil
}

func (runner reviewRunner) observeDryRunIdentityShadow(ctx context.Context, result pipeline.Result, authoritativeErr error) {
	if runner.identityShadow == nil {
		return
	}
	if authoritativeErr != nil {
		runner.warnIdentityShadowSkipped()
		return
	}
	runner.observeIdentityShadow(ctx, result.Artifacts, result.ReportIdentity)
}

func (runner reviewRunner) observeLiveIdentityShadow(ctx context.Context, result reviewrun.Result, flags reviewrun.Flags, authoritativeErr error) {
	if runner.identityShadow == nil {
		return
	}
	pipelineResult, eligible := liveIdentityShadowPipelineResult(result, flags, authoritativeErr)
	if !eligible {
		runner.warnIdentityShadowSkipped()
		return
	}
	runner.observeIdentityShadow(ctx, pipelineResult.Artifacts, pipelineResult.ReportIdentity)
}

func liveIdentityShadowPipelineResult(result reviewrun.Result, flags reviewrun.Flags, authoritativeErr error) (*pipeline.Result, bool) {
	if authoritativeErr != nil || flags.RetryPosts || result.Decision.Kind != gate.DecisionFresh || result.Pipeline == nil || result.Outbox.Aborted || result.ExitCode != 0 || result.Outbox.ExitCode != 0 {
		return nil, false
	}
	switch result.Outbox.Outcome {
	case ledger.OutcomeApproved, ledger.OutcomeRequestChanges, ledger.OutcomeComment, ledger.OutcomeNothingToReview:
		return result.Pipeline, true
	case ledger.OutcomeIncomplete, ledger.OutcomeDryRun, ledger.OutcomeAborted, ledger.OutcomeFailed:
		return nil, false
	default:
		return nil, false
	}
}

func (runner reviewRunner) observeIdentityShadow(ctx context.Context, paths pipeline.ArtifactPaths, snapshot *reportidentitysnapshot.Snapshot) {
	if runner.identityShadow == nil {
		return
	}
	cloned, attemptDir, present, err := cloneIdentityShadowSnapshotAndAttemptPath(paths, snapshot, runner.judgeEnabled)
	if !present {
		runner.warnIdentityShadowSkipped()
		return
	}
	if err != nil {
		runner.warnIdentityDiagnostic("artifact_failure", paths.Dir)
		return
	}
	runner.identityShadow.Observe(ctx, cloned, attemptDir)
}

func (runner reviewRunner) warnIdentityShadowSkipped() {
	runner.warnIdentityDiagnostic("skipped_no_snapshot", "")
}

func (runner reviewRunner) warnIdentityDiagnostic(status, path string) {
	if runner.judgeEnabled && runner.pipeline.Warnings != nil {
		_, _ = fmt.Fprintf(runner.pipeline.Warnings, "cr judge prior-threads status=%s artifacts=%q\n", status, path)
		return
	}
	writeIdentityShadowWarning(runner.pipeline.Warnings, status, path)
}

func writeIdentityShadowWarning(warnings io.Writer, status, artifactPath string) {
	if warnings == nil {
		return
	}
	if artifactPath == "" {
		_, _ = fmt.Fprintf(warnings, "cr identity-shadow status=%s\n", status)
		return
	}
	path := strings.NewReplacer("\n", "_", "\r", "_", "\t", "_").Replace(artifactPath)
	_, _ = fmt.Fprintf(warnings, "cr identity-shadow status=%s artifacts=%q\n", status, path)
}

// MaxPriorJudgeInputBytes bounds a local captured input or prior-thread fixture.
const MaxPriorJudgeInputBytes = identityshadow.MaxJudgeInputBytes

// PriorJudgeReport is the advisory report for a local judge run.
type PriorJudgeReport = identityshadow.JudgeReport

// JudgePriorThreads runs the advisory judge from a saved local input.
func JudgePriorThreads(ctx context.Context, input []byte, configPath, outputDir string, resolve func(context.Context, string, config.CredentialLocation) (string, error)) (PriorJudgeReport, error) {
	if resolve == nil {
		return PriorJudgeReport{}, fmt.Errorf("judge: credential resolver is required")
	}
	return identityshadow.RunPriorJudge(ctx, identityshadow.JudgeOptions{
		ConfigPath: configPath,
		ExactKey:   reviewplan.PriorFindingIdentityKey,
		ResolveKey: func(ctx context.Context, kind string, ref identityshadow.Credential) (string, error) {
			return resolve(ctx, kind, config.CredentialLocation{Store: ref.Store, Name: ref.Name})
		},
	}, input, outputDir)
}
