package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gate"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
)

const expectedIdentityShadowCanceledStatus = "cancelled" //nolint:misspell // Stable diagnostic category.

type identityShadowObserverFunc func(context.Context, reportidentitysnapshot.Snapshot, string)

func (observer identityShadowObserverFunc) Observe(ctx context.Context, snapshot reportidentitysnapshot.Snapshot, attemptDir string) {
	observer(ctx, snapshot, attemptDir)
}

func TestCloneIdentityShadowSnapshotAndAttemptPathOwnsValuesAndUsesRunArtifactPath(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("Mkdir run artifact directory: %v", err)
	}
	keyHash := "key-hash"
	planningMS := int64(17)
	cost := 0.42
	snapshot := &reportidentitysnapshot.Snapshot{
		Source: reportidentitysnapshot.Source{RunID: "run-1"},
		Findings: []review.Finding{{
			ID: "finding-1", Severity: review.SeverityMajor, FilePath: "src/a.go",
			Anchor: review.Anchor{Kind: review.AnchorKindFile}, Body: "original finding body",
		}},
		PriorRoots: []reportidentitysnapshot.PriorRoot{{ID: "thread-1", FilePath: "src/b.go", Body: "original root body"}},
		Baseline: reportidentitysnapshot.Baseline{
			SchemaVersion: 1, EffectiveOrderedIDs: []review.FindingID{"finding-1"},
			Dedupe:    []reportidentitysnapshot.Dedupe{{Kept: "finding-1", Dropped: []review.FindingID{"finding-2"}}},
			PriorKeys: []reportidentitysnapshot.PriorKey{{RecordKind: "finding", RecordID: "finding-1", KeySHA256: &keyHash}},
			Metrics:   reportidentitysnapshot.NativeMetrics{RunPlanningWallMS: &planningMS, RollupCostUSD: &cost},
		},
	}
	cloned, attemptDir, present, err := cloneIdentityShadowSnapshotAndAttemptPath(pipeline.ArtifactPathsFromDir(runDir), snapshot)
	if err != nil || !present {
		t.Fatalf("clone/path result: present=%t err=%v", present, err)
	}
	if filepath.Dir(filepath.Dir(attemptDir)) != runDir || !strings.HasPrefix(filepath.Base(attemptDir), "attempt-") {
		t.Fatalf("attempt directory = %q, want a fresh child of %q/identity-shadow", attemptDir, runDir)
	}
	if _, err := os.Lstat(attemptDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt path was created by app: stat error=%v", err)
	}
	cloned.Findings[0].Body = "observer mutation"
	cloned.PriorRoots[0].Body = "observer mutation"
	cloned.Baseline.EffectiveOrderedIDs[0] = "observer mutation"
	cloned.Baseline.Dedupe[0].Dropped[0] = "observer mutation"
	*cloned.Baseline.PriorKeys[0].KeySHA256 = "observer mutation"
	*cloned.Baseline.Metrics.RunPlanningWallMS = 99
	*cloned.Baseline.Metrics.RollupCostUSD = 99
	if snapshot.Findings[0].Body != "original finding body" || snapshot.PriorRoots[0].Body != "original root body" ||
		snapshot.Baseline.EffectiveOrderedIDs[0] != "finding-1" || snapshot.Baseline.Dedupe[0].Dropped[0] != "finding-2" ||
		*snapshot.Baseline.PriorKeys[0].KeySHA256 != "key-hash" || *snapshot.Baseline.Metrics.RunPlanningWallMS != 17 || *snapshot.Baseline.Metrics.RollupCostUSD != 0.42 {
		t.Fatalf("observer-owned copy mutated the source snapshot: %#v", snapshot)
	}
	if _, _, present, err := cloneIdentityShadowSnapshotAndAttemptPath(pipeline.ArtifactPathsFromDir(runDir), nil); err != nil || present {
		t.Fatalf("nil snapshot preparation = present:%t err:%v", present, err)
	}
}

func TestIdentityShadowSystemOneResolverUsesSelectedStoreAndRegistryCleanup(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	seed, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(), Backend: credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open synthetic file credential store: %v", err)
	}
	defer seed.Close()
	if err := seed.Set("identity", credentials.SystemOneAPIKeyKey, "synthetic-system-one-value", credstore.WithOverwrite()); err != nil {
		t.Fatalf("seed System One key: %v", err)
	}
	if err := seed.Set("identity", credentials.OpenAIAPIKeyKey, "wrong-provider-value", credstore.WithOverwrite()); err != nil {
		t.Fatalf("seed OpenAI key: %v", err)
	}

	cfg := config.File{Secrets: config.SecretsConfig{Stores: map[string]config.SecretsStore{
		"identity-file": {
			DisplayName: "Synthetic identity store",
			Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
		},
	}}}
	stores := newRuntimeCredentialStores(cfg, "", false, "review", nil)
	resolve := identityShadowSystemOneKeyResolver(cfg, stores)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolve(ctx, "identity-file", "codereview/identity"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolver error = %v, want context.Canceled", err)
	}
	if len(stores.stores) != 0 {
		t.Fatalf("canceled resolver opened stores: %#v", stores.stores)
	}

	key, err := resolve(context.Background(), "identity-file", "codereview/identity")
	if err != nil || key != "synthetic-system-one-value" {
		t.Fatalf("selected store resolver = %q, %v", key, err)
	}
	opened, ok := stores.stores["identity-file"]
	if !ok || opened.reader == nil {
		t.Fatalf("selected store was not retained by the runtime registry: %#v", stores.stores)
	}
	stores.Close()
	if _, err := opened.reader.Get("identity", credentials.SystemOneAPIKeyKey); !errors.Is(err, credstore.ErrStoreClosed) {
		t.Fatalf("identity reader after runtime cleanup = %v, want closed store", err)
	}
}

func TestLiveIdentityShadowEligibilityRequiresFreshSuccessfulTerminalPipeline(t *testing.T) {
	pipelineResult := &pipeline.Result{}
	tests := []struct {
		name     string
		result   reviewrun.Result
		flags    reviewrun.Flags
		runErr   error
		eligible bool
	}{
		{name: "approved", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeApproved)}, eligible: true},
		{name: "request changes", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeRequestChanges)}, eligible: true},
		{name: "comment", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment)}, eligible: true},
		{name: "nothing to review", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeNothingToReview)}, eligible: true},
		{name: "authoritative error", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment)}, runErr: errors.New("synthetic review failure")},
		{name: "retry posts", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment)}, flags: reviewrun.Flags{RetryPosts: true}},
		{name: "no new pipeline", result: reviewrun.Result{Outbox: outboxResult(ledger.OutcomeApproved)}},
		{name: "aborted outbox", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outbox.Result{Outcome: ledger.OutcomeAborted, Aborted: true}}},
		{name: "incomplete outbox", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeIncomplete)}},
		{name: "dry run outcome", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeDryRun)}},
		{name: "authoritative exit", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionFresh}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment), ExitCode: 1}},
		{name: "resume with fresh pipeline is still recovery", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionResume}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment)}},
		{name: "repair with fresh pipeline is still recovery", result: reviewrun.Result{Decision: gate.Decision{Kind: gate.DecisionRepair}, Pipeline: pipelineResult, Outbox: outboxResult(ledger.OutcomeComment)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := liveIdentityShadowPipelineResult(test.result, test.flags, test.runErr)
			if ok != test.eligible || (ok && got != pipelineResult) {
				t.Fatalf("eligibility = (%p, %t), want pipeline:%t", got, ok, test.eligible)
			}
		})
	}
}

func outboxResult(outcome ledger.Outcome) outbox.Result {
	return outbox.Result{Outcome: outcome}
}

func TestIdentityShadowWarningsAreSanitizedAndPathFailureIsIsolated(t *testing.T) {
	var warnings strings.Builder
	writeIdentityShadowWarning(&warnings, "skipped_no_snapshot", "")
	writeIdentityShadowWarning(&warnings, "artifact_failure", "private/path\nwith-control")
	if got := warnings.String(); !strings.Contains(got, "status=skipped_no_snapshot") || !strings.Contains(got, `artifacts="private/path_with-control"`) || strings.Contains(got, "\nwith-control") {
		t.Fatalf("shadow warning was not sanitized: %q", got)
	}

	disabled := reviewRunner{pipeline: pipeline.Options{Warnings: &warnings}}
	beforeDisabled := warnings.String()
	disabled.observeIdentityShadow(context.Background(), pipeline.ArtifactPaths{}, nil)
	if warnings.String() != beforeDisabled {
		t.Fatalf("disabled observer wrote a warning: %q", warnings.String())
	}

	enabled := reviewRunner{
		pipeline: pipeline.Options{Warnings: &warnings},
		identityShadow: identityShadowObserverFunc(func(context.Context, reportidentitysnapshot.Snapshot, string) {
			t.Fatal("invalid run-artifact path invoked the observer")
		}),
	}
	snapshot := &reportidentitysnapshot.Snapshot{Source: reportidentitysnapshot.Source{RunID: "run-1"}}
	enabled.observeIdentityShadow(context.Background(), pipeline.ArtifactPaths{}, snapshot)
	if !strings.Contains(warnings.String(), "status=artifact_failure") {
		t.Fatalf("path failure warning = %q", warnings.String())
	}
	beforeMissing := warnings.Len()
	enabled.observeIdentityShadow(context.Background(), pipeline.ArtifactPaths{}, nil)
	if !strings.Contains(warnings.String()[beforeMissing:], "status=skipped_no_snapshot") {
		t.Fatalf("missing snapshot warning = %q", warnings.String()[beforeMissing:])
	}
}

func TestIdentityShadowDiagnosticStatusesCannotChangeAuthoritativeDryRunValues(t *testing.T) {
	tests := []struct {
		name            string
		cancel          bool
		artifactFailure bool
		wantWarning     string
	}{
		{name: "complete observer"},
		{name: "invalid config", wantWarning: "invalid_config"},
		{name: "canceled after review", cancel: true, wantWarning: expectedIdentityShadowCanceledStatus},
		{name: "attempt path failure", artifactFailure: true, wantWarning: "artifact_failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runDir := filepath.Join(t.TempDir(), "run")
			if err := os.Mkdir(runDir, 0o700); err != nil {
				t.Fatalf("Mkdir run: %v", err)
			}
			snapshot := &reportidentitysnapshot.Snapshot{
				Source: reportidentitysnapshot.Source{RunID: "run-1"},
				Findings: []review.Finding{{
					ID: "finding-1", Severity: review.SeverityMajor, FilePath: "src/a.go",
					Anchor: review.Anchor{Kind: review.AnchorKindFile}, Body: "authoritative report body",
				}},
				Baseline: reportidentitysnapshot.Baseline{
					EffectiveOrderedIDs: []review.FindingID{"finding-1"},
					Dedupe:              []reportidentitysnapshot.Dedupe{{Kept: "finding-1", Dropped: []review.FindingID{"finding-2"}}},
				},
			}
			result := pipeline.Result{
				Run:             ledger.Run{RunID: "run-1"},
				Findings:        append([]review.Finding{}, snapshot.Findings...),
				PlannedActions:  []ledger.PlannedAction{{RunID: "run-1"}},
				FailOnTriggered: true,
				ReportIdentity:  snapshot,
				Artifacts:       pipeline.ArtifactPathsFromDir(runDir),
			}
			if test.artifactFailure {
				result.Artifacts = pipeline.ArtifactPaths{}
			}
			beforeRunID := result.Run.RunID
			beforeFindings := append([]review.Finding{}, result.Findings...)
			beforeActions := append([]ledger.PlannedAction{}, result.PlannedActions...)
			beforeSnapshot := reportidentitysnapshot.Clone(*result.ReportIdentity)
			var warnings strings.Builder
			observerCalls := 0
			runner := reviewRunner{
				pipeline: pipeline.Options{Warnings: &warnings},
				identityShadow: identityShadowObserverFunc(func(ctx context.Context, observed reportidentitysnapshot.Snapshot, _ string) {
					observerCalls++
					if test.cancel && !errors.Is(ctx.Err(), context.Canceled) {
						t.Errorf("observer context error = %v, want context.Canceled", ctx.Err())
					}
					if test.wantWarning != "" && !test.artifactFailure {
						writeIdentityShadowWarning(&warnings, test.wantWarning, "private-attempt")
					}
					observed.Findings[0].Body = "observer body mutation"
					observed.Baseline.EffectiveOrderedIDs[0] = "observer ID mutation"
					observed.Baseline.Dedupe[0].Dropped[0] = "observer drop mutation"
				}),
			}
			ctx := context.Background()
			if test.cancel {
				canceledCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceledCtx
			}
			runner.observeDryRunIdentityShadow(ctx, result, nil)

			if observerCalls != 1 && !test.artifactFailure {
				t.Fatalf("observer calls = %d, want 1", observerCalls)
			}
			if observerCalls != 0 && test.artifactFailure {
				t.Fatalf("observer calls after path failure = %d, want 0", observerCalls)
			}
			if result.Run.RunID != beforeRunID || !reflect.DeepEqual(result.Findings, beforeFindings) ||
				!reflect.DeepEqual(result.PlannedActions, beforeActions) || !result.FailOnTriggered ||
				!reflect.DeepEqual(*result.ReportIdentity, beforeSnapshot) {
				t.Fatalf("shadow diagnostic changed authoritative result: %#v", result)
			}
			if test.wantWarning != "" && !strings.Contains(warnings.String(), "status="+test.wantWarning) {
				t.Fatalf("diagnostic warning = %q, want status %s", warnings.String(), test.wantWarning)
			}
		})
	}
}
