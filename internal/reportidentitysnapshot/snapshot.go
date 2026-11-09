// Package reportidentitysnapshot contains value-only source and baseline data
// for private report-identity diagnostics.
package reportidentitysnapshot

import "github.com/open-cli-collective/codereview-cli/internal/review"

// Source identifies the review invocation whose report values were captured.
type Source struct {
	RunID             string `json:"run_id"`
	PRKey             string `json:"pr_key"`
	Profile           string `json:"profile"`
	PostingIdentity   string `json:"posting_identity"`
	CurrentBaseSHA    string `json:"current_base_sha"`
	CurrentHeadSHA    string `json:"current_head_sha"`
	ReviewBaseSHA     string `json:"review_base_sha"`
	ReviewHeadSHA     string `json:"review_head_sha"`
	CRVersion         string `json:"cr_version"`
	WithoutDiscussion bool   `json:"without_discussion"`
}

// PriorRoot retains the opening comment of a planner-eligible prior finding
// thread. Resolved is source metadata; it does not change identity eligibility.
type PriorRoot struct {
	ID       string `json:"id"`
	FilePath string `json:"file_path"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved"`
}

// Dedupe is one accepted rollup representative and its dropped finding IDs.
type Dedupe struct {
	Kept    review.FindingID   `json:"kept"`
	Dropped []review.FindingID `json:"dropped"`
}

// PriorKey is the exact current planner identity key, represented only by its
// digest. A nil digest means the planner key was invalid.
type PriorKey struct {
	RecordKind string  `json:"record_kind"`
	RecordID   string  `json:"record_id"`
	KeySHA256  *string `json:"key_sha256"`
}

// NativeMetrics copies nullable native planning and rollup measurements. The
// rollup values cover the whole rollup workstream, not identity-only work.
type NativeMetrics struct {
	RunPlanningWallMS       *int64   `json:"run_planning_wall_ms"`
	RollupPresent           bool     `json:"rollup_present"`
	RollupModel             string   `json:"rollup_model"`
	RollupDurationMS        *int64   `json:"rollup_duration_ms"`
	RollupTokensIn          *int64   `json:"rollup_tokens_in"`
	RollupTokensOut         *int64   `json:"rollup_tokens_out"`
	RollupCostUSD           *float64 `json:"rollup_cost_usd"`
	RollupCostEstimated     bool     `json:"rollup_cost_estimated"`
	RollupCostEstimateBasis string   `json:"rollup_cost_estimate_basis"`
}

// Baseline records the accepted planner rollup partition and exact prior rule.
// It is observed behavior, not a gold label or finding-validity judgment.
type Baseline struct {
	SchemaVersion       int                `json:"schema_version"`
	CohortStatus        string             `json:"cohort_status"`
	EffectiveOrderedIDs []review.FindingID `json:"effective_ordered_ids"`
	Dedupe              []Dedupe           `json:"dedupe"`
	PriorRule           string             `json:"prior_rule"`
	PriorKeys           []PriorKey         `json:"prior_keys"`
	Metrics             NativeMetrics      `json:"metrics"`
}

// Snapshot freezes the exact report values and the planner baseline observed
// for one authoritative review plan.
type Snapshot struct {
	Source     Source           `json:"source"`
	Findings   []review.Finding `json:"findings"`
	PriorRoots []PriorRoot      `json:"prior_roots"`
	Baseline   Baseline         `json:"baseline"`
}

// Clone returns a complete deep copy of snapshot, including every mutable
// slice and nullable metric/hash pointer.
func Clone(snapshot Snapshot) Snapshot {
	out := snapshot
	out.Findings = cloneSlice(snapshot.Findings)
	out.PriorRoots = cloneSlice(snapshot.PriorRoots)
	out.Baseline.EffectiveOrderedIDs = cloneSlice(snapshot.Baseline.EffectiveOrderedIDs)
	out.Baseline.Dedupe = cloneSlice(snapshot.Baseline.Dedupe)
	for i := range out.Baseline.Dedupe {
		out.Baseline.Dedupe[i].Dropped = cloneSlice(snapshot.Baseline.Dedupe[i].Dropped)
	}
	out.Baseline.PriorKeys = cloneSlice(snapshot.Baseline.PriorKeys)
	for i := range out.Baseline.PriorKeys {
		out.Baseline.PriorKeys[i].KeySHA256 = clonePointer(snapshot.Baseline.PriorKeys[i].KeySHA256)
	}
	out.Baseline.Metrics = cloneMetrics(snapshot.Baseline.Metrics)
	return out
}

func cloneMetrics(metrics NativeMetrics) NativeMetrics {
	metrics.RunPlanningWallMS = clonePointer(metrics.RunPlanningWallMS)
	metrics.RollupDurationMS = clonePointer(metrics.RollupDurationMS)
	metrics.RollupTokensIn = clonePointer(metrics.RollupTokensIn)
	metrics.RollupTokensOut = clonePointer(metrics.RollupTokensOut)
	metrics.RollupCostUSD = clonePointer(metrics.RollupCostUSD)
	return metrics
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	out := make([]T, len(values))
	copy(out, values)
	return out
}
