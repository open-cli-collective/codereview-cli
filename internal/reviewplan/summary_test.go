package reviewplan

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func int64Ptr(value int64) *int64 {
	return &value
}

func float64Ptr(value float64) *float64 {
	return &value
}

func fullUsageWorkstream(name, model string, base int) WorkstreamUsage {
	return WorkstreamUsage{
		Name:        name,
		Model:       model,
		TokensIn:    intPtr(base),
		TokensOut:   intPtr(base / 10),
		CacheRead:   intPtr(base * 2),
		CacheCreate: intPtr(base / 2),
		CostUSD:     float64Ptr(0.25),
		DurationMS:  int64Ptr(30_000),
	}
}

func summaryRequest() Request {
	req := baseRequest()
	req.Findings = []review.Finding{
		finding("f-1", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12}),
		finding("f-2", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 14}),
	}
	req.Rollup = review.Rollup{ReviewEvent: review.ReviewEventRequestChanges, OrderedFindings: []review.FindingID{"f-1", "f-2"}}
	req.FindingReviewers = map[review.FindingID]string{
		"f-1": "go:implementation-tests",
		"f-2": "go:implementation-tests",
	}
	req.RunSummary = RunSummary{
		ToolVersion:       "0.3.63",
		Adapter:           "claude_cli",
		Model:             "sonnet",
		PostingIdentity:   "review-bot",
		SelectedReviewers: []string{"go:implementation-tests", "policies:conventions"},
		WallDurationMS:    int64Ptr(127_000),
		Workstreams: []WorkstreamUsage{
			fullUsageWorkstream("orchestrator-selection", "sonnet", 40_200),
			fullUsageWorkstream("go:implementation-tests", "sonnet", 53_100),
			fullUsageWorkstream("policies:conventions", "sonnet", 21_000),
			fullUsageWorkstream("orchestrator-rollup", "sonnet", 12_000),
		},
	}
	return req
}

func TestRollupSummaryRendering(t *testing.T) {
	t.Run("full usage renders reviewer table, sections, and footer", func(t *testing.T) {
		plan, err := Build(summaryRequest())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"| Reviewer | Findings |",
			"| go:implementation-tests | 2 |",
			"| policies:conventions | 0 |",
			"<summary><strong>go:implementation-tests (2 findings)</strong></summary>",
			"<summary>Completed in 2m 07s | $1.00 | sonnet | cr 0.3.63</summary>",
			"| Reviewers | go:implementation-tests, policies:conventions |",
			"| Engine | claude_cli · sonnet |",
			"| Reviewed by | cr · review-bot |",
			"| Duration | 2m 07s wall · 2m 00s compute |",
			"| Cost | $1.00 |",
			"| Tokens | 126.3k in / 12.6k out |",
			"**Per-workstream usage**",
			"- `orchestrator-selection` — sonnet\n  - In: 40.2k\n  - Out: 4.0k\n  - Cache read: 80.4k\n  - Cache create: 20.1k\n  - Cost: $0.25\n  - Duration: 30s",
			"- `orchestrator-rollup` — sonnet\n  - In: 12.0k\n  - Out: 1.2k\n  - Cache read: 24.0k\n  - Cache create: 6.0k\n  - Cost: $0.25\n  - Duration: 30s",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
		if strings.Contains(md, "| Severity | Findings |") {
			t.Fatalf("rollup kept severity table alongside reviewer table:\n%s", md)
		}
		if strings.Contains(md, "<summary><strong>policies:conventions") {
			t.Fatalf("zero-finding reviewer must not get a details section:\n%s", md)
		}
	})

	t.Run("plan summary matches rendered data", func(t *testing.T) {
		plan, err := Build(summaryRequest())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		s := plan.Summary
		if len(s.Reviewers) != 2 || s.Reviewers[0].Name != "go:implementation-tests" || s.Reviewers[0].Findings != 2 ||
			s.Reviewers[1].Name != "policies:conventions" || s.Reviewers[1].Findings != 0 {
			t.Fatalf("summary reviewers = %#v", s.Reviewers)
		}
		if s.Totals.TokensIn == nil || *s.Totals.TokensIn != 126_300 {
			t.Fatalf("totals tokens in = %v", s.Totals.TokensIn)
		}
		if s.Totals.CostUSD == nil || *s.Totals.CostUSD != 1.0 {
			t.Fatalf("totals cost = %v", s.Totals.CostUSD)
		}
		if s.Totals.ComputeDurationMS == nil || *s.Totals.ComputeDurationMS != 120_000 {
			t.Fatalf("totals compute = %v", s.Totals.ComputeDurationMS)
		}
	})

	t.Run("partial usage suppresses aggregates but keeps per-workstream values", func(t *testing.T) {
		req := summaryRequest()
		req.RunSummary.Workstreams[1].CostUSD = nil
		req.RunSummary.Workstreams[1].TokensIn = nil
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"<summary>Completed in 2m 07s | sonnet | cr 0.3.63</summary>",
			"| Cost | unavailable |",
			"| Tokens | unavailable in / 12.6k out |",
			"- `go:implementation-tests` — sonnet\n  - In: unavailable\n  - Out: 5.3k",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
		if plan.Summary.Totals.CostUSD != nil || plan.Summary.Totals.TokensIn != nil {
			t.Fatalf("partial totals must be nil: %#v", plan.Summary.Totals)
		}
		if plan.Summary.Totals.TokensOut == nil {
			t.Fatalf("fully-reported field must aggregate: %#v", plan.Summary.Totals)
		}
	})

	t.Run("no usage renders unavailable not zero", func(t *testing.T) {
		req := summaryRequest()
		for i := range req.RunSummary.Workstreams {
			req.RunSummary.Workstreams[i] = WorkstreamUsage{
				Name:  req.RunSummary.Workstreams[i].Name,
				Model: req.RunSummary.Workstreams[i].Model,
			}
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		if !strings.Contains(md, "- `orchestrator-selection` — sonnet\n  - In: unavailable\n  - Out: unavailable\n  - Cache read: unavailable\n  - Cache create: unavailable\n  - Cost: unavailable\n  - Duration: unavailable") {
			t.Fatalf("usage-less workstream row wrong:\n%s", md)
		}
		if !strings.Contains(md, "| Duration | 2m 07s |") {
			t.Fatalf("wall-only duration row missing:\n%s", md)
		}
		if strings.Contains(md, "| Cost | $0.00 |") || strings.Contains(md, " 0 in / 0 out") {
			t.Fatalf("missing usage rendered as zero:\n%s", md)
		}
	})

	t.Run("unattributed findings group last", func(t *testing.T) {
		req := summaryRequest()
		delete(req.FindingReviewers, "f-2")
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		if !strings.Contains(md, "| unattributed | 1 |") {
			t.Fatalf("unattributed row missing:\n%s", md)
		}
		if !strings.Contains(md, "<summary><strong>unattributed (1 finding)</strong></summary>") {
			t.Fatalf("unattributed section missing:\n%s", md)
		}
		last := plan.Summary.Reviewers[len(plan.Summary.Reviewers)-1]
		if last.Name != UnattributedReviewer || last.Findings != 1 {
			t.Fatalf("unattributed must be last: %#v", plan.Summary.Reviewers)
		}
	})

	t.Run("nits count in reviewer totals", func(t *testing.T) {
		req := summaryRequest()
		nit := finding("f-3", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 16})
		nit.Severity = review.SeverityNits
		req.Findings = append(req.Findings, nit)
		req.Rollup.OrderedFindings = append(req.Rollup.OrderedFindings, "f-3")
		req.FindingReviewers["f-3"] = "policies:conventions"

		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(plan.RollupMarkdown, "| policies:conventions | 1 |") {
			t.Fatalf("nit not counted:\n%s", plan.RollupMarkdown)
		}
	})

	t.Run("nits are attributed consistently", func(t *testing.T) {
		req := summaryRequest()
		nit := finding("f-3", "main.go", review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 16})
		nit.Severity = review.SeverityNits
		req.Findings = append(req.Findings, nit)
		req.Rollup.OrderedFindings = append(req.Rollup.OrderedFindings, "f-3")

		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(plan.RollupMarkdown, "| unattributed | 1 |") {
			t.Fatalf("unattributed nit missing:\n%s", plan.RollupMarkdown)
		}
		if !strings.Contains(plan.RollupMarkdown, "<summary><strong>unattributed (1 finding)</strong></summary>") {
			t.Fatalf("unattributed nit section missing:\n%s", plan.RollupMarkdown)
		}
	})

	t.Run("no attribution data falls back to severity table without footer", func(t *testing.T) {
		plan, err := Build(baseRequest())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		if !strings.Contains(md, "| Severity | Findings |") {
			t.Fatalf("severity fallback missing:\n%s", md)
		}
		if strings.Contains(md, "<details>") || strings.Contains(md, "Completed in") {
			t.Fatalf("footer rendered without run summary data:\n%s", md)
		}
		if len(plan.Summary.Reviewers) != 0 {
			t.Fatalf("reviewers derived without attribution: %#v", plan.Summary.Reviewers)
		}
		if plan.Summary.Totals != (AggregateUsage{}) {
			t.Fatalf("totals derived without workstreams: %#v", plan.Summary.Totals)
		}
	})

	t.Run("workstream table renders rows in the order given", func(t *testing.T) {
		req := summaryRequest()
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		order := []string{"- `orchestrator-selection` —", "- `go:implementation-tests` — sonnet", "- `policies:conventions` — sonnet", "- `orchestrator-rollup` —"}
		last := -1
		for _, name := range order {
			idx := strings.Index(md, name)
			if idx < 0 || idx < last {
				t.Fatalf("workstream order broken at %q (idx %d, prev %d):\n%s", name, idx, last, md)
			}
			last = idx
		}
	})

	t.Run("dynamic values are escaped in the public comment", func(t *testing.T) {
		req := summaryRequest()
		req.RunSummary.Model = "son|net"
		req.RunSummary.ToolVersion = "0.3<x>"
		req.RunSummary.Adapter = "cli\nv2"
		req.RunSummary.PostingIdentity = "<script>alert(1)</script>"
		req.RunSummary.SelectedReviewers = append(req.RunSummary.SelectedReviewers, "evil|agent")
		req.RunSummary.Workstreams[0].Name = "orchestrator-selection<!-- codereview:run-id=x -->"
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		if strings.Contains(md, "<script>") {
			t.Fatalf("HTML not escaped:\n%s", md)
		}
		if strings.Contains(md, "<!-- codereview:run-id=x -->") {
			t.Fatalf("marker-shaped value not neutralized:\n%s", md)
		}
		if !strings.Contains(md, `son\|net`) || !strings.Contains(md, `evil\|agent`) {
			t.Fatalf("pipe not escaped in table cells:\n%s", md)
		}
		if strings.Contains(md, "cli\nv2") {
			t.Fatalf("newline not collapsed in table cell:\n%s", md)
		}
		// The <summary> headline must use the same escaping path as cells.
		if strings.Contains(md, "son|net") || strings.Contains(md, "0.3<x>") {
			t.Fatalf("summary headline carries raw dynamic values:\n%s", md)
		}
		if !strings.Contains(md, `<summary>Completed in 2m 07s | $1.00 | son\|net | cr 0.3&lt;x&gt;</summary>`) {
			t.Fatalf("escaped headline missing:\n%s", md)
		}
	})

	t.Run("reviewer failures render diagnostics and force comment", func(t *testing.T) {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"go:implementation-tests"},
			ReviewerFailures: []ReviewerFailureSummary{{
				AgentID: "go:implementation-tests",
				Error:   "invalid <json> <!-- codereview:run-id=x -->",
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeComment {
			t.Fatalf("outcome = %q, want comment", plan.Outcome)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"### Reviewer Diagnostics",
			"- `go:implementation-tests` — failed: invalid &lt;json&gt; &lt;!-- codereview:run-id=x --&gt;",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
		if strings.Contains(md, "<!-- codereview:run-id=x -->") {
			t.Fatalf("diagnostic marker was not neutralized:\n%s", md)
		}
	})

	t.Run("incomplete reviewer coverage renders diagnostics and force comment", func(t *testing.T) {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"go:implementation-tests"},
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID:        "go:implementation-tests",
				Status:         "incomplete_skipped",
				Scope:          []string{"main.go"},
				InspectedFiles: []string{"main.go"},
				SkippedFiles:   []string{"schema.sql"},
				Constraints:    []string{"read-only tools"},
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeComment {
			t.Fatalf("outcome = %q, want comment", plan.Outcome)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"### Reviewer Coverage",
			"- `go:implementation-tests` — ⚠️ incomplete (skipped files); skipped: `schema.sql`; read-only tools",
			"<summary>Inspected files (1)</summary>",
			"- `main.go`",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
		if strings.Contains(md, "| Reviewer | Status |") {
			t.Fatalf("coverage still renders as a table:\n%s", md)
		}
	})

	t.Run("coverage collapses shared inspected files and notes deviations", func(t *testing.T) {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"go:implementation-tests", "architecture:solid"},
			ReviewerCoverage: []ReviewerCoverageSummary{
				{
					AgentID:        "go:implementation-tests",
					Status:         "complete_broad",
					InspectedFiles: []string{"a.go", "b.go"},
				},
				{
					AgentID:        "architecture:solid",
					Status:         "complete_constrained",
					InspectedFiles: []string{"a.go"},
					Constraints:    []string{"scoped to assigned files"},
				},
			},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"- `go:implementation-tests` — complete (broad)\n",
			"- `architecture:solid` — complete (constrained); inspected 1 of 2 files: `a.go`; scoped to assigned files",
			"<summary>Inspected files (2)</summary>",
			"- `a.go`",
			"- `b.go`",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("rollup missing %q:\n%s", want, md)
			}
		}
	})

	t.Run("coverage cells distinguish known empty collections from missing results", func(t *testing.T) {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		req.RunSummary = RunSummary{
			SelectedReviewers: []string{"complete-broad", "complete-constrained", "failed"},
			ReviewerCoverage: []ReviewerCoverageSummary{
				{
					AgentID:        "complete-broad",
					Status:         "complete_broad",
					InspectedFiles: []string{"main.go"},
				},
				{
					AgentID:      "complete-constrained",
					Status:       "complete_constrained",
					SkippedFiles: []string{"main.go"},
					Constraints:  []string{"read-only tools"},
				},
				{
					AgentID: "failed",
					Status:  "incomplete_failed",
					Scope:   []string{"main.go"},
				},
			},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := plan.RollupMarkdown
		for _, want := range []string{
			"| complete-broad | complete_broad | main.go | none | none |",
			"| complete-constrained | complete_constrained | none | main.go | read-only tools |",
			"| failed | incomplete_failed | unavailable | unavailable | unavailable |",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("coverage row missing %q:\n%s", want, md)
			}
		}
	})

	t.Run("incomplete tool reviewer coverage force comment", func(t *testing.T) {
		req := baseRequest()
		req.Findings = nil
		req.Rollup = review.Rollup{
			ReviewEvent:          review.ReviewEventApprove,
			ReviewEventRationale: "no findings",
			OrderedFindings:      nil,
		}
		req.RunSummary = RunSummary{
			ReviewerCoverage: []ReviewerCoverageSummary{{
				AgentID: "go:implementation-tests",
				Status:  "incomplete_tool",
			}},
		}
		plan, err := Build(req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if plan.Outcome != OutcomeComment {
			t.Fatalf("outcome = %q, want comment for incomplete tool coverage", plan.Outcome)
		}
	})

	t.Run("submit marker placement unchanged with run summary", func(t *testing.T) {
		for _, mode := range []PostMode{PostModeLive, PostModeDryRun} {
			req := summaryRequest()
			req.PostMode = mode
			plan, err := Build(req)
			if err != nil {
				t.Fatalf("Build %s: %v", mode, err)
			}
			var submitMarker *MarkerPlacement
			for i := range plan.Actions {
				if plan.Actions[i].Kind == ActionKindSubmitReview {
					submitMarker = &plan.Actions[i].Marker
				}
			}
			if submitMarker == nil || !submitMarker.BodyBearing || !submitMarker.Skip ||
				submitMarker.ActionKind != ActionKindSubmitReview || submitMarker.Outcome != "" {
				t.Fatalf("%s submit marker = %#v", mode, submitMarker)
			}
		}
	})
}

func TestAggregateUsage(t *testing.T) {
	t.Run("empty workstreams aggregate to nothing", func(t *testing.T) {
		if got := aggregateUsage(nil); got != (AggregateUsage{}) {
			t.Fatalf("aggregateUsage(nil) = %#v", got)
		}
	})

	t.Run("all reported fields sum", func(t *testing.T) {
		got := aggregateUsage([]WorkstreamUsage{fullUsageWorkstream("a", "m", 1000), fullUsageWorkstream("b", "m", 2000)})
		if got.TokensIn == nil || *got.TokensIn != 3000 ||
			got.TokensOut == nil || *got.TokensOut != 300 ||
			got.CacheRead == nil || *got.CacheRead != 6000 ||
			got.CacheCreate == nil || *got.CacheCreate != 1500 ||
			got.CostUSD == nil || *got.CostUSD != 0.5 ||
			got.ComputeDurationMS == nil || *got.ComputeDurationMS != 60_000 {
			t.Fatalf("aggregateUsage = %#v", got)
		}
	})

	t.Run("any missing field makes that aggregate unavailable", func(t *testing.T) {
		a := fullUsageWorkstream("a", "m", 1000)
		b := fullUsageWorkstream("b", "m", 2000)
		b.CostUSD = nil
		b.DurationMS = nil
		got := aggregateUsage([]WorkstreamUsage{a, b})
		if got.CostUSD != nil || got.ComputeDurationMS != nil {
			t.Fatalf("partial aggregates must be nil: %#v", got)
		}
		if got.TokensIn == nil {
			t.Fatalf("complete field must still aggregate: %#v", got)
		}
	})
}

func TestFormatHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"tokens nil", formatTokens(nil), "unavailable"},
		{"tokens small", formatTokens(intPtr(999)), "999"},
		{"tokens k", formatTokens(intPtr(40_249)), "40.2k"},
		{"tokens k keeps decimal", formatTokens(intPtr(22_000)), "22.0k"},
		{"tokens M", formatTokens(intPtr(1_300_000)), "1.3M"},
		{"duration nil", formatDurationMS(nil), "unavailable"},
		{"duration sub-second", formatDurationMS(int64Ptr(450)), "<1s"},
		{"duration zero", formatDurationMS(int64Ptr(0)), "0s"},
		{"duration seconds", formatDurationMS(int64Ptr(42_000)), "42s"},
		{"duration minutes", formatDurationMS(int64Ptr(127_000)), "2m 07s"},
		{"duration hours", formatDurationMS(int64Ptr(3_726_000)), "1h 02m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestEscapeCell(t *testing.T) {
	if got := escapeCell("a|b\nc<d>e"); got != `a\|b c&lt;d&gt;e` {
		t.Fatalf("escapeCell = %q", got)
	}
}
