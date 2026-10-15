package view

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

func TestNewReviewDryRunJSONPreservesDiscussionModeAndThreadFailures(t *testing.T) {
	for _, tc := range []struct {
		name              string
		withoutDiscussion bool
		failures          []reviewplan.ThreadAnalysisFailureSummary
	}{
		{
			name: "ordinary review with isolated failure",
			failures: []reviewplan.ThreadAnalysisFailureSummary{
				{ThreadID: "thread-failed", Error: "thread provider failed"},
			},
		},
		{name: "discussion-free replay", withoutDiscussion: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := pipeline.Result{
				Run:               ledger.Run{RunID: "discussion-mode", PostMode: ledger.PostModeDryRun},
				WithoutDiscussion: tc.withoutDiscussion,
				Plan: reviewplan.Plan{Summary: reviewplan.Summary{Run: reviewplan.RunSummary{
					ThreadAnalysisFailures: tc.failures,
				}}},
			}
			rendered, err := NewReviewDryRun(result)
			if err != nil {
				t.Fatalf("NewReviewDryRun: %v", err)
			}
			var out bytes.Buffer
			if err := RenderReviewDryRunJSON(&out, rendered); err != nil {
				t.Fatalf("RenderReviewDryRunJSON: %v", err)
			}
			var decoded ReviewDryRun
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatalf("decode JSON: %v", err)
			}
			if decoded.Run.WithoutDiscussion != tc.withoutDiscussion {
				t.Fatalf("JSON discussion mode = %t, want %t", decoded.Run.WithoutDiscussion, tc.withoutDiscussion)
			}
			if !slices.Equal(decoded.Summary.Run.ThreadAnalysisFailures, tc.failures) {
				t.Fatalf("JSON thread failures = %#v, want %#v", decoded.Summary.Run.ThreadAnalysisFailures, tc.failures)
			}
		})
	}
}
