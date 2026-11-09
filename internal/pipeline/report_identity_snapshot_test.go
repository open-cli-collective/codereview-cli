package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func TestCurrentPriorRootsSharePlannerOpeningEligibility(t *testing.T) {
	threads := []threadcontext.Thread{
		{
			ID:       "resolved-bot-root",
			Resolved: true,
			Anchor:   threadcontext.Anchor{Path: "root.go"},
			Comments: []threadcontext.Comment{
				snapshotComment("opening finding", true, true),
				snapshotComment("later human reply", false, false),
			},
		},
		{
			ID:     "human-root-bot-reply",
			Anchor: threadcontext.Anchor{Path: "human.go"},
			Comments: []threadcontext.Comment{
				snapshotComment("human's opening", false, true),
				snapshotComment("our marked reply", true, true),
			},
			Status: threadcontext.Status{CRAuthoredFinding: true},
		},
		{ID: "empty-thread", Anchor: threadcontext.Anchor{Path: "empty.go"}},
		{
			ID:     "settled-bot-root",
			Anchor: threadcontext.Anchor{Path: "settled.go"},
			Comments: []threadcontext.Comment{
				snapshotComment("original opening body", true, true),
				{
					Body:                      "settled summary body",
					AuthoredByPostingIdentity: true,
					HasThreadSummaryMarker:    true,
				},
			},
		},
		{
			ID:     "cross-file-bot-root",
			Anchor: threadcontext.Anchor{Path: "unrelated.go"},
			Comments: []threadcontext.Comment{
				snapshotComment("root from another file", true, true),
			},
		},
	}

	roots := currentPriorRoots(threads)
	wantRoots := []reportidentitysnapshot.PriorRoot{
		{ID: "resolved-bot-root", FilePath: "root.go", Body: "opening finding", Resolved: true},
		{ID: "settled-bot-root", FilePath: "settled.go", Body: "original opening body"},
		{ID: "cross-file-bot-root", FilePath: "unrelated.go", Body: "root from another file"},
	}
	if !reflect.DeepEqual(roots, wantRoots) {
		t.Fatalf("currentPriorRoots = %#v, want %#v", roots, wantRoots)
	}

	plannerThreads := existingFindingThreads(threads)
	wantPlannerThreads := []reviewplan.ExistingThread{
		{Path: "root.go", Body: "opening finding"},
		{Path: "settled.go", Body: "original opening body"},
		{Path: "unrelated.go", Body: "root from another file"},
	}
	if !reflect.DeepEqual(plannerThreads, wantPlannerThreads) {
		t.Fatalf("existingFindingThreads = %#v, want %#v", plannerThreads, wantPlannerThreads)
	}
}

func TestReportIdentityBaselineCopiesAcceptedPlannerValuesAndExactKeys(t *testing.T) {
	wall := int64(500)
	duration := int64(20)
	tokensIn := 25
	tokensOut := 7
	cost := 0.125
	finding := reportSnapshotFinding("finding-1", "main.go", "Finding body")
	finding.Anchoring = review.AnchoringRollupOnly // This must not imply a prior-thread demotion.
	result := Result{
		Findings: []review.Finding{finding},
		Rollup: review.Rollup{
			OrderedFindings: []review.FindingID{"finding-1", "finding-2"},
			DedupeLog: []review.DedupeEntry{{
				Kept:    "finding-1",
				Dropped: []review.FindingID{"finding-2"},
				Reason:  "not captured in baseline",
			}},
		},
		Plan: reviewplan.Plan{
			AnchoredFindings: []reviewplan.AnchoredFinding{{
				FindingID: "finding-1",
				FilePath:  "main.go",
				Body:      "<!-- codereview:run-id=one --> Finding   body *Reply inline to this comment.*",
			}},
			Summary: reviewplan.Summary{Run: reviewplan.RunSummary{
				WallDurationMS: &wall,
				Workstreams: []reviewplan.WorkstreamUsage{
					{Name: orchestratorSelectionStage, Model: "selection-model", TokensIn: intPointerForSnapshotTest(900)},
					{
						Name:              orchestratorRollupStage,
						Model:             "rollup-model",
						TokensIn:          &tokensIn,
						TokensOut:         &tokensOut,
						CostUSD:           &cost,
						CostEstimated:     true,
						CostEstimateBasis: "catalog/revision-1",
						DurationMS:        &duration,
					},
				},
			}},
		},
	}
	roots := []reportidentitysnapshot.PriorRoot{
		{ID: "thread-1", FilePath: "main.go", Body: "Finding body"},
		{ID: "thread-empty", FilePath: "main.go", Body: "<!-- codereview:run-id=two -->"},
	}

	baseline := reportIdentityBaseline(result, roots)
	if baseline.SchemaVersion != 1 || baseline.CohortStatus != "accepted_rollup" || baseline.PriorRule != "planner_exact_path_normalized_body_v1" {
		t.Fatalf("baseline provenance = %#v", baseline)
	}
	if !reflect.DeepEqual(baseline.EffectiveOrderedIDs, []review.FindingID{"finding-1", "finding-2"}) {
		t.Fatalf("effective ordered IDs = %#v", baseline.EffectiveOrderedIDs)
	}
	if len(baseline.Dedupe) != 1 || baseline.Dedupe[0].Kept != "finding-1" || !reflect.DeepEqual(baseline.Dedupe[0].Dropped, []review.FindingID{"finding-2"}) {
		t.Fatalf("dedupe baseline = %#v", baseline.Dedupe)
	}
	if len(baseline.PriorKeys) != 3 || baseline.PriorKeys[0].RecordKind != "finding" || baseline.PriorKeys[0].RecordID != "finding-1" ||
		baseline.PriorKeys[1].RecordKind != "thread" || baseline.PriorKeys[1].RecordID != "thread-1" || baseline.PriorKeys[2].RecordID != "thread-empty" {
		t.Fatalf("prior keys do not preserve finding/root order and original IDs: %#v", baseline.PriorKeys)
	}
	key, ok := reviewplan.PriorFindingIdentityKey("main.go", result.Plan.AnchoredFindings[0].Body)
	if !ok {
		t.Fatal("planner returned invalid test key")
	}
	digest := sha256.Sum256([]byte(key))
	wantDigest := hex.EncodeToString(digest[:])
	if baseline.PriorKeys[0].KeySHA256 == nil || *baseline.PriorKeys[0].KeySHA256 != wantDigest ||
		baseline.PriorKeys[1].KeySHA256 == nil || *baseline.PriorKeys[1].KeySHA256 != wantDigest {
		t.Fatalf("prior key hashes = %#v, want finding/root hash %s", baseline.PriorKeys, wantDigest)
	}
	if baseline.PriorKeys[2].KeySHA256 != nil {
		t.Fatalf("empty planner key hash = %v, want null", *baseline.PriorKeys[2].KeySHA256)
	}
	metrics := baseline.Metrics
	if metrics.RunPlanningWallMS == nil || *metrics.RunPlanningWallMS != wall || !metrics.RollupPresent || metrics.RollupModel != "rollup-model" ||
		metrics.RollupDurationMS == nil || *metrics.RollupDurationMS != duration || metrics.RollupTokensIn == nil || *metrics.RollupTokensIn != int64(tokensIn) ||
		metrics.RollupTokensOut == nil || *metrics.RollupTokensOut != int64(tokensOut) || metrics.RollupCostUSD == nil || *metrics.RollupCostUSD != cost ||
		!metrics.RollupCostEstimated || metrics.RollupCostEstimateBasis != "catalog/revision-1" {
		t.Fatalf("native metrics = %#v", metrics)
	}

	result.Rollup.DedupeLog[0].Dropped[0] = "mutated"
	result.Plan.Summary.Run.Workstreams[1].TokensIn = intPointerForSnapshotTest(999)
	*result.Plan.Summary.Run.WallDurationMS = 999
	if baseline.Dedupe[0].Dropped[0] != "finding-2" || *baseline.Metrics.RollupTokensIn != int64(tokensIn) || *baseline.Metrics.RunPlanningWallMS != 500 {
		t.Fatalf("baseline aliases planner-owned values: %#v", baseline)
	}
}

func TestReportIdentityBaselineFallbackUsesPlannerOrderAndKeepsUnavailableMetricsNull(t *testing.T) {
	result := Result{
		Findings: []review.Finding{
			reportSnapshotFinding("finding-2", "b.go", "second"),
			reportSnapshotFinding("finding-1", "a.go", "first"),
		},
		Rollup: review.Rollup{Fallback: true},
		Plan:   reviewplan.Plan{},
	}
	baseline := reportIdentityBaseline(result, nil)
	if baseline.CohortStatus != "fallback_no_dedupe" || len(baseline.Dedupe) != 0 {
		t.Fatalf("fallback baseline = %#v", baseline)
	}
	if !reflect.DeepEqual(baseline.EffectiveOrderedIDs, []review.FindingID{"finding-2", "finding-1"}) {
		t.Fatalf("fallback effective order = %#v, want input order", baseline.EffectiveOrderedIDs)
	}
	metrics := baseline.Metrics
	if metrics.RunPlanningWallMS != nil || metrics.RollupPresent || metrics.RollupModel != "" || metrics.RollupDurationMS != nil || metrics.RollupTokensIn != nil ||
		metrics.RollupTokensOut != nil || metrics.RollupCostUSD != nil || metrics.RollupCostEstimated || metrics.RollupCostEstimateBasis != "" {
		t.Fatalf("absent native metrics were inferred: %#v", metrics)
	}

	unavailable := reportIdentityBaseline(Result{}, nil)
	if unavailable.CohortStatus != "unavailable" || unavailable.Metrics.RollupPresent {
		t.Fatalf("baseline without an accepted/fallback rollup = %#v, want unavailable", unavailable)
	}
}

func TestReportIdentityBaselineKeepsExplicitGroupsWithoutNativeRollupTelemetry(t *testing.T) {
	result := Result{
		Findings: []review.Finding{
			reportSnapshotFinding("finding-1", "a.go", "first"),
			reportSnapshotFinding("finding-2", "b.go", "second"),
		},
		Rollup: review.Rollup{
			OrderedFindings: []review.FindingID{"finding-1"},
			DedupeLog:       []review.DedupeEntry{{Kept: "finding-1", Dropped: []review.FindingID{"finding-2"}}},
		},
		Plan: reviewplan.Plan{},
	}

	baseline := reportIdentityBaseline(result, nil)
	if baseline.CohortStatus != "accepted_rollup" || !reflect.DeepEqual(baseline.EffectiveOrderedIDs, []review.FindingID{"finding-1"}) ||
		len(baseline.Dedupe) != 1 || baseline.Dedupe[0].Kept != "finding-1" || !reflect.DeepEqual(baseline.Dedupe[0].Dropped, []review.FindingID{"finding-2"}) {
		t.Fatalf("explicit planner partition was lost with absent telemetry: %#v", baseline)
	}
	if baseline.Metrics.RollupPresent || baseline.Metrics.RollupModel != "" || baseline.Metrics.RollupDurationMS != nil || baseline.Metrics.RollupTokensIn != nil ||
		baseline.Metrics.RollupTokensOut != nil || baseline.Metrics.RollupCostUSD != nil || baseline.Metrics.RollupCostEstimated || baseline.Metrics.RollupCostEstimateBasis != "" {
		t.Fatalf("absent native rollup telemetry was inferred: %#v", baseline.Metrics)
	}
}

func TestCaptureReportIdentityCopiesCurrentValuesAndKeepsSnapshotOffJSON(t *testing.T) {
	req := Request{
		ProfileName:       "review-profile",
		PostingIdentity:   gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
		ToolVersion:       "0.0.0-test",
		WithoutDiscussion: true,
	}
	finding := reportSnapshotFinding("finding-1", "main.go", "report body")
	thread := threadcontext.Thread{
		ID:       "thread-1",
		Resolved: true,
		Anchor:   threadcontext.Anchor{Path: "prior.go"},
		Comments: []threadcontext.Comment{snapshotComment("opening body", true, true)},
	}
	result := Result{
		Run:            ledgerRunForSnapshotTest("run-1"),
		PRKey:          "owner/repo#1",
		CurrentBaseSHA: "current-base",
		CurrentHeadSHA: "current-head",
		ReviewBaseSHA:  "review-base",
		ReviewHeadSHA:  "review-head",
		Findings:       []review.Finding{finding},
		Plan: reviewplan.Plan{AnchoredFindings: []reviewplan.AnchoredFinding{{
			FindingID: finding.ID,
			FilePath:  finding.FilePath,
			Body:      finding.Body,
		}}},
	}
	snapshot := captureReportIdentitySnapshot(req, result, []threadcontext.Thread{thread})
	if snapshot.Source.RunID != "run-1" || snapshot.Source.PRKey != "owner/repo#1" || snapshot.Source.Profile != "review-profile" ||
		snapshot.Source.PostingIdentity != "review-bot" || snapshot.Source.CurrentBaseSHA != "current-base" || snapshot.Source.CurrentHeadSHA != "current-head" ||
		snapshot.Source.ReviewBaseSHA != "review-base" || snapshot.Source.ReviewHeadSHA != "review-head" || snapshot.Source.CRVersion != "0.0.0-test" || !snapshot.Source.WithoutDiscussion {
		t.Fatalf("snapshot provenance = %#v", snapshot.Source)
	}
	if !reflect.DeepEqual(snapshot.Findings, result.Findings) || len(snapshot.PriorRoots) != 1 || snapshot.PriorRoots[0].ID != "thread-1" ||
		snapshot.PriorRoots[0].FilePath != "prior.go" || snapshot.PriorRoots[0].Body != "opening body" || !snapshot.PriorRoots[0].Resolved {
		t.Fatalf("snapshot source values = %#v", snapshot)
	}
	snapshot.Findings[0].Body = "observer mutation"
	snapshot.PriorRoots[0].Body = "observer mutation"
	if result.Findings[0].Body != "report body" || thread.Comments[0].Body != "opening body" {
		t.Fatal("snapshot mutations changed authoritative finding or thread values")
	}

	result.ReportIdentity = snapshot
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal result: %v", err)
	}
	if strings.Contains(string(encoded), "ReportIdentity") || strings.Contains(string(encoded), "report_identity") {
		t.Fatalf("private snapshot field escaped into Result JSON: %s", encoded)
	}
}

func TestDryRunReportIdentityCaptureIsOptInAndPlanPreserving(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	req.ToolVersion = "0.0.0-test"
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")

	run := func(capture bool, runID string) Result {
		t.Helper()
		store := openPipelineStore(t)
		adapter := &llm.FakeAdapter{NameValue: "snapshot-plan-equivalence"}
		adapter.Queue(fakeLLMResult("selection-session", selectionJSONForFiles("harness:alpha", "main.go", "other.go"), 10, 2))
		adapter.Queue(fakeLLMResult("reviewer-session", `{
			"schema_version": 1,
			"agent_id": "harness:alpha",
			"inspected_files": ["main.go", "other.go"],
			"skipped_files": [],
			"constraints": [],
			"findings": [
				{"severity": "major", "file_path": "main.go", "anchor": {"kind": "line", "side": "RIGHT", "line": 2}, "body": "First finding"},
				{"severity": "minor", "file_path": "other.go", "anchor": {"kind": "line", "side": "RIGHT", "line": 2}, "body": "Second finding"}
			]
		}`, 20, 4))
		adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1", "finding-2"}), 30, 6))
		result, err := dryRunForTest(ctx, Options{
			Provider:              provider,
			Adapter:               adapter,
			Store:                 store,
			Layout:                statepaths.NewLayout(t.TempDir(), t.TempDir()),
			Now:                   fixedNow,
			NewRunID:              func() string { return runID },
			NewSessionRowID:       sequence("snapshot-session"),
			NewFindingID:          findingSequence("finding"),
			NewActionID:           actionSequence(),
			MaxConcurrency:        2,
			CaptureReportIdentity: capture,
		}, req)
		closeStore(t, store)
		if err != nil {
			t.Fatalf("DryRun(capture=%t): %v", capture, err)
		}
		return result
	}

	off := run(false, "run-snapshot-off")
	on := run(true, "run-snapshot-on")
	if off.ReportIdentity != nil {
		t.Fatalf("disabled run captured a report snapshot: %#v", off.ReportIdentity)
	}
	if on.ReportIdentity == nil {
		t.Fatal("enabled run did not capture a report snapshot")
	}
	if len(on.Findings) != 2 || !reflect.DeepEqual(on.ReportIdentity.Baseline.EffectiveOrderedIDs, []review.FindingID{"finding-1", "finding-2"}) {
		t.Fatalf("test fixture did not retain the ordered two-finding cohort: findings=%#v baseline=%#v", on.Findings, on.ReportIdentity.Baseline)
	}
	if !reflect.DeepEqual(off.Plan, on.Plan) || !reflect.DeepEqual(off.Findings, on.Findings) || !reflect.DeepEqual(off.Rollup, on.Rollup) || off.FailOnTriggered != on.FailOnTriggered {
		t.Fatal("diagnostic snapshot capture changed authoritative findings, rollup, plan, or fail-on result")
	}
	if on.ReportIdentity.Source.RunID != "run-snapshot-on" || !reflect.DeepEqual(on.ReportIdentity.Findings, on.Findings) {
		t.Fatalf("enabled snapshot did not retain this run's values: %#v", on.ReportIdentity)
	}
}

func snapshotComment(body string, ours, findingMarker bool) threadcontext.Comment {
	return threadcontext.Comment{
		Body:                      body,
		AuthoredByPostingIdentity: ours,
		HasFindingMarker:          findingMarker,
	}
}

func reportSnapshotFinding(id, path, body string) review.Finding {
	return review.Finding{
		ID:       review.FindingID(id),
		Severity: review.SeverityMajor,
		FilePath: path,
		Anchor:   review.Anchor{Kind: review.AnchorKindFile},
		Body:     body,
	}
}

func intPointerForSnapshotTest(value int) *int {
	return &value
}

func ledgerRunForSnapshotTest(runID string) ledger.Run {
	return ledger.Run{RunID: runID}
}
