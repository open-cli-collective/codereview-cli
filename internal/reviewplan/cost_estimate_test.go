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

func TestAllCostEstimated(t *testing.T) {
	if allCostEstimated([]WorkstreamUsage{{CostEstimated: true}, {CostEstimated: false}}) {
		t.Fatal("partially estimated should not mark the aggregate")
	}
	if !allCostEstimated([]WorkstreamUsage{{CostEstimated: true}, {CostEstimated: true}}) {
		t.Fatal("fully estimated should mark the aggregate")
	}
	if allCostEstimated(nil) {
		t.Fatal("empty should be false")
	}
}
