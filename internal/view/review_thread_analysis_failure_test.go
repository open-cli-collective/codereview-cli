package view

import (
	"fmt"
	"reflect"
	"strings"

	"encoding/json"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

func TestReviewSummaryJSONReportsUnanalyzedThreads(t *testing.T) {
	summary := reviewplan.Summary{Run: reviewplan.RunSummary{ThreadAnalysisFailures: []reviewplan.ThreadAnalysisFailureSummary{{ThreadID: "failed-thread", Error: "invalid output"}}}}
	data, err := json.Marshal(newReviewSummary(summary))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			Failures []reviewplan.ThreadAnalysisFailureSummary `json:"thread_analysis_failures"`
		} `json:"run"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Run.Failures) != 1 || got.Run.Failures[0].ThreadID != "failed-thread" || got.Run.Failures[0].Error != "invalid output" {
		t.Fatalf("summary JSON = %s, want explicit unanalyzed thread", data)
	}
}

func TestReviewSummaryJSONRetainsCompleteLargeThreadFailureCollection(t *testing.T) {
	failures := make([]reviewplan.ThreadAnalysisFailureSummary, 2500)
	for i := range failures {
		failures[i] = reviewplan.ThreadAnalysisFailureSummary{ThreadID: fmt.Sprintf("thread-%04d", i), Error: strings.Repeat("界", 800)}
	}
	summary := reviewplan.Summary{Run: reviewplan.RunSummary{ThreadAnalysisFailures: failures}}
	data, err := json.Marshal(newReviewSummary(summary))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			Failures []reviewplan.ThreadAnalysisFailureSummary `json:"thread_analysis_failures"`
		} `json:"run"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Run.Failures, failures) {
		t.Fatal("JSON summary sampled or lost complete thread failure evidence")
	}
}
