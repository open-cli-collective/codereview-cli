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
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
)

func identityShadowSystemOneKeyResolver(cfg config.File, stores *runtimeCredentialStores) func(context.Context, string, string) (string, error) {
	return func(ctx context.Context, storeID, name string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		location := config.CredentialLocation{Store: storeID, Name: name}
		_, reader, err := stores.Open(location)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := credentials.ReadIdentityAPIKey(ctx, cfg, reader, location, "system_one")
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return key, nil
	}
}

func cloneIdentityShadowSnapshotAndAttemptPath(paths pipeline.ArtifactPaths, snapshot *reportidentitysnapshot.Snapshot) (reportidentitysnapshot.Snapshot, string, bool, error) {
	if snapshot == nil {
		return reportidentitysnapshot.Snapshot{}, "", false, nil
	}
	clone := reportidentitysnapshot.Clone(*snapshot)
	attemptDir, err := paths.IdentityShadowAttempt(uuid.NewString())
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
	cloned, attemptDir, present, err := cloneIdentityShadowSnapshotAndAttemptPath(paths, snapshot)
	if !present {
		runner.warnIdentityShadowSkipped()
		return
	}
	if err != nil {
		writeIdentityShadowWarning(runner.pipeline.Warnings, "artifact_failure", paths.Dir)
		return
	}
	runner.identityShadow.Observe(ctx, cloned, attemptDir)
}

func (runner reviewRunner) warnIdentityShadowSkipped() {
	writeIdentityShadowWarning(runner.pipeline.Warnings, "skipped_no_snapshot", "")
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
