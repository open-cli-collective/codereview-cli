package reviewplan

import (
	"strings"
	"testing"
)

func TestWriteRunFooterMarksEstimatedCost(t *testing.T) {
	var out strings.Builder
	cost := 1.23
	totals := AggregateUsage{CostUSD: &cost, CostEstimated: true}
	writeRunFooter(&out, RunSummary{ToolVersion: "1.0.0"}, totals)
	if got := out.String(); !strings.Contains(got, "~$1.23 (est.)") {
		t.Fatalf("want estimated cost marker; got:\n%s", got)
	}
}

func TestWriteRunFooterRealCostUnmarked(t *testing.T) {
	var out strings.Builder
	cost := 2.50
	totals := AggregateUsage{CostUSD: &cost, CostEstimated: false}
	writeRunFooter(&out, RunSummary{ToolVersion: "1.0.0"}, totals)
	got := out.String()
	if !strings.Contains(got, "$2.50") || strings.Contains(got, "est.") {
		t.Fatalf("want plain cost without est. marker; got:\n%s", got)
	}
}

func TestAnyCostEstimated(t *testing.T) {
	if !anyCostEstimated([]WorkstreamUsage{{CostEstimated: false}, {CostEstimated: true}}) {
		t.Fatal("want true when any workstream is estimated")
	}
	if anyCostEstimated([]WorkstreamUsage{{CostEstimated: false}}) {
		t.Fatal("want false when none are estimated")
	}
}
