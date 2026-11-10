package reportidentitysnapshot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestCloneDeepCopiesEveryMutableSnapshotValue(t *testing.T) {
	keyHash := "key-hash"
	wall := int64(10)
	duration := int64(20)
	tokensIn := int64(30)
	tokensOut := int64(40)
	cost := 1.25
	original := Snapshot{
		Source: Source{RunID: "run-1"},
		Findings: []review.Finding{{
			ID:       "finding-1",
			Severity: review.SeverityMajor,
			FilePath: "main.go",
			Body:     "finding body",
		}},
		PriorRoots: []PriorRoot{{ID: "thread-1", FilePath: "main.go", Body: "prior body", Resolved: true}},
		Baseline: Baseline{
			SchemaVersion:       1,
			CohortStatus:        "accepted_rollup",
			EffectiveOrderedIDs: []review.FindingID{"finding-1"},
			Dedupe:              []Dedupe{{Kept: "finding-1", Dropped: []review.FindingID{"finding-2"}}},
			PriorRule:           "planner_exact_path_normalized_body_v1",
			PriorKeys:           []PriorKey{{RecordKind: "finding", RecordID: "finding-1", KeySHA256: &keyHash}},
			Metrics: NativeMetrics{
				RunPlanningWallMS:       &wall,
				RollupPresent:           true,
				RollupModel:             "rollup-model",
				RollupDurationMS:        &duration,
				RollupTokensIn:          &tokensIn,
				RollupTokensOut:         &tokensOut,
				RollupCostUSD:           &cost,
				RollupCostEstimated:     true,
				RollupCostEstimateBasis: "catalog/pricing",
			},
		},
	}

	cloned := Clone(original)
	cloned.Findings[0].Body = "changed finding"
	cloned.PriorRoots[0].Body = "changed prior"
	cloned.Baseline.EffectiveOrderedIDs[0] = "changed-order"
	cloned.Baseline.Dedupe[0].Dropped[0] = "changed-dropped"
	*cloned.Baseline.PriorKeys[0].KeySHA256 = "changed-key-hash"
	*cloned.Baseline.Metrics.RunPlanningWallMS = 101
	*cloned.Baseline.Metrics.RollupDurationMS = 102
	*cloned.Baseline.Metrics.RollupTokensIn = 103
	*cloned.Baseline.Metrics.RollupTokensOut = 104
	*cloned.Baseline.Metrics.RollupCostUSD = 105.0

	if original.Findings[0].Body != "finding body" || original.PriorRoots[0].Body != "prior body" {
		t.Fatalf("clone changed source text: finding=%q prior=%q", original.Findings[0].Body, original.PriorRoots[0].Body)
	}
	if original.Baseline.EffectiveOrderedIDs[0] != "finding-1" || original.Baseline.Dedupe[0].Dropped[0] != "finding-2" {
		t.Fatalf("clone changed baseline slices: %#v", original.Baseline)
	}
	if *original.Baseline.PriorKeys[0].KeySHA256 != "key-hash" || *original.Baseline.Metrics.RunPlanningWallMS != wall ||
		*original.Baseline.Metrics.RollupDurationMS != duration || *original.Baseline.Metrics.RollupTokensIn != tokensIn ||
		*original.Baseline.Metrics.RollupTokensOut != tokensOut || *original.Baseline.Metrics.RollupCostUSD != cost {
		t.Fatalf("clone changed baseline pointers: %#v", original.Baseline)
	}
	if reflect.DeepEqual(cloned, original) {
		t.Fatal("mutating the clone did not change it")
	}
}

func TestClonePreservesNilAndEmptySlicesAndNullableJSON(t *testing.T) {
	original := Snapshot{
		Findings:   []review.Finding{},
		PriorRoots: nil,
		Baseline: Baseline{
			EffectiveOrderedIDs: []review.FindingID{},
			Dedupe:              nil,
			PriorKeys:           []PriorKey{{RecordKind: "thread", RecordID: "thread-1"}},
		},
	}
	cloned := Clone(original)
	if cloned.Findings == nil || cloned.PriorRoots != nil || cloned.Baseline.EffectiveOrderedIDs == nil || cloned.Baseline.Dedupe != nil {
		t.Fatalf("Clone changed nil/empty slice shape: %#v", cloned)
	}
	encoded, err := json.Marshal(cloned)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, want := range []string{`"findings":[]`, `"prior_roots":null`, `"effective_ordered_ids":[]`, `"key_sha256":null`, `"run_planning_wall_ms":null`, `"rollup_cost_usd":null`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("snapshot JSON missing %s: %s", want, encoded)
		}
	}
}
