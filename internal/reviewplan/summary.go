package reviewplan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// UnattributedReviewer labels rendered findings with no reviewer attribution.
const UnattributedReviewer = "unattributed"

const unavailableValue = "unavailable"

// Summary is the complete derived rollup metadata contract. The rendered
// rollup comment and dry-run JSON both consume this object, so they cannot
// disagree.
type Summary struct {
	Reviewers []ReviewerSummary
	Threads   ThreadCounts
	Run       RunSummary
	// Totals is derived from Run.Workstreams by Build; it lives here rather
	// than on the caller-supplied RunSummary so callers cannot populate a
	// value that Build would silently overwrite.
	Totals AggregateUsage
}

// ReviewerSummary is one reviewer row in the rollup summary table.
type ReviewerSummary struct {
	Name     string
	Findings int
}

// ReviewerCoverageSummary describes whether a selected reviewer actually
// covered its assignment. It is compact by design so rollups can include it
// without pulling diff or file content into context.
type ReviewerCoverageSummary struct {
	AgentID        string
	Status         string
	Scope          []string
	InspectedFiles []string
	SkippedFiles   []string
	Constraints    []string
	Diagnostic     string
}

// ThreadCounts summarizes PR discussion thread handling.
type ThreadCounts struct {
	Considered int
	Summarized int
	Resolved   int
}

// RunSummary is the execution metadata input for the rollup footer. Usage
// fields are nullable: nil means the adapter did not report the value.
type RunSummary struct {
	// ToolVersion is the raw version string (e.g. "0.3.63"); the renderer
	// adds the "cr " prefix.
	ToolVersion       string
	Adapter           string
	Model             string
	PostingIdentity   string
	SelectedReviewers []string
	ReviewerFailures  []ReviewerFailureSummary
	ReviewerCoverage  []ReviewerCoverageSummary
	WallDurationMS    *int64
	Workstreams       []WorkstreamUsage
}

// ReviewerFailureSummary is a reviewer task diagnostic rendered in the rollup.
type ReviewerFailureSummary struct {
	AgentID string
	Error   string
}

// WorkstreamUsage is adapter-reported usage for one workstream: the reserved
// orchestrator stage names or a reviewer agent ID.
type WorkstreamUsage struct {
	Name        string
	Model       string
	TokensIn    *int
	TokensOut   *int
	CacheRead   *int
	CacheCreate *int
	CostUSD     *float64
	// CostEstimated is true when CostUSD was derived from token prices because
	// the adapter did not report a real cost.
	CostEstimated bool
	DurationMS    *int64
}

// AggregateUsage holds run-wide totals. Each field is non-nil only when every
// workstream reports it; availability is decided here in data, once, so the
// rendered footer and dry-run JSON cannot disagree.
type AggregateUsage struct {
	TokensIn          *int
	TokensOut         *int
	CacheRead         *int
	CacheCreate       *int
	CostUSD           *float64
	CostEstimated     bool
	ComputeDurationMS *int64
}

func (r RunSummary) hasData() bool {
	return r.ToolVersion != "" || r.Adapter != "" || r.Model != "" ||
		r.PostingIdentity != "" || len(r.SelectedReviewers) > 0 ||
		len(r.ReviewerFailures) > 0 || len(r.ReviewerCoverage) > 0 || r.WallDurationMS != nil ||
		len(r.Workstreams) > 0
}

func (b *builder) deriveSummary(rendered []review.Finding) Summary {
	run := b.req.RunSummary
	threads := threadSummaryCounts(b.req.ThreadActions, b.req.ProviderCaps)
	threads = addThreadCounts(threads, threadResponseSummaryCounts(b.req.ThreadResponses, b.req.ProviderCaps))
	summary := Summary{
		Threads: threads,
		Run:     run,
		Totals:  aggregateUsage(run.Workstreams),
	}
	// Without any attribution data the rollup keeps its severity-table
	// shape; an all-"unattributed" reviewer table would be noise.
	if len(b.req.FindingReviewers) > 0 || len(run.SelectedReviewers) > 0 {
		summary.Reviewers = b.reviewerSummaries(rendered)
	}
	return summary
}

func addThreadCounts(left, right ThreadCounts) ThreadCounts {
	return ThreadCounts{
		Considered: left.Considered + right.Considered,
		Summarized: left.Summarized + right.Summarized,
		Resolved:   left.Resolved + right.Resolved,
	}
}

// reviewerSummaries lists every selected reviewer in selection order with its
// rendered finding count (zero included), then any attributed reviewers
// missing from the selection (sorted), then unattributed findings last.
func (b *builder) reviewerSummaries(rendered []review.Finding) []ReviewerSummary {
	counts := map[string]int{}
	unattributed := 0
	for _, finding := range rendered {
		name := b.req.FindingReviewers[finding.ID]
		if name == "" {
			unattributed++
			continue
		}
		counts[name]++
	}
	var out []ReviewerSummary
	seen := map[string]bool{}
	for _, name := range b.req.RunSummary.SelectedReviewers {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, ReviewerSummary{Name: name, Findings: counts[name]})
		delete(counts, name)
	}
	extras := make([]string, 0, len(counts))
	for name := range counts {
		extras = append(extras, name)
	}
	sort.Strings(extras)
	for _, name := range extras {
		out = append(out, ReviewerSummary{Name: name, Findings: counts[name]})
	}
	if unattributed > 0 {
		out = append(out, ReviewerSummary{Name: UnattributedReviewer, Findings: unattributed})
	}
	return out
}

func aggregateUsage(workstreams []WorkstreamUsage) AggregateUsage {
	if len(workstreams) == 0 {
		return AggregateUsage{}
	}
	return AggregateUsage{
		TokensIn:          sumInts(workstreams, func(w WorkstreamUsage) *int { return w.TokensIn }),
		TokensOut:         sumInts(workstreams, func(w WorkstreamUsage) *int { return w.TokensOut }),
		CacheRead:         sumInts(workstreams, func(w WorkstreamUsage) *int { return w.CacheRead }),
		CacheCreate:       sumInts(workstreams, func(w WorkstreamUsage) *int { return w.CacheCreate }),
		CostUSD:           sumFloats(workstreams, func(w WorkstreamUsage) *float64 { return w.CostUSD }),
		CostEstimated:     allCostEstimated(workstreams),
		ComputeDurationMS: sumDurations(workstreams, func(w WorkstreamUsage) *int64 { return w.DurationMS }),
	}
}

// allCostEstimated reports whether every workstream's cost was estimated, so the
// aggregate is marked an estimate only when it is fully estimated — a mostly-real
// total is not labeled "(est.)".
func allCostEstimated(workstreams []WorkstreamUsage) bool {
	if len(workstreams) == 0 {
		return false
	}
	for _, w := range workstreams {
		if !w.CostEstimated {
			return false
		}
	}
	return true
}

func sumInts(workstreams []WorkstreamUsage, field func(WorkstreamUsage) *int) *int {
	total := 0
	for _, w := range workstreams {
		value := field(w)
		if value == nil {
			return nil
		}
		total += *value
	}
	return &total
}

func sumFloats(workstreams []WorkstreamUsage, field func(WorkstreamUsage) *float64) *float64 {
	total := 0.0
	for _, w := range workstreams {
		value := field(w)
		if value == nil {
			return nil
		}
		total += *value
	}
	return &total
}

func sumDurations(workstreams []WorkstreamUsage, field func(WorkstreamUsage) *int64) *int64 {
	var total int64
	for _, w := range workstreams {
		value := field(w)
		if value == nil {
			return nil
		}
		total += *value
	}
	return &total
}

func writeReviewerTable(out *strings.Builder, reviewers []ReviewerSummary) {
	out.WriteString("| Reviewer | Findings |\n")
	out.WriteString("|----------|----------|\n")
	for _, reviewer := range reviewers {
		fmt.Fprintf(out, "| %s | %d |\n", escapeCell(reviewer.Name), reviewer.Findings)
	}
	out.WriteString("\n")
}

func writeReviewerFailureDiagnostics(out *strings.Builder, failures []ReviewerFailureSummary) {
	if len(failures) == 0 {
		return
	}
	out.WriteString("### Reviewer Diagnostics\n\n")
	out.WriteString("| Reviewer | Status | Diagnostic |\n")
	out.WriteString("|----------|--------|------------|\n")
	for _, failure := range failures {
		fmt.Fprintf(out, "| %s | failed | %s |\n", escapeCell(failure.AgentID), escapeCell(failure.Error))
	}
	out.WriteString("\n")
}

func writeReviewerCoverageDiagnostics(out *strings.Builder, coverage []ReviewerCoverageSummary) {
	if len(coverage) == 0 {
		return
	}
	out.WriteString("### Reviewer Coverage\n\n")
	out.WriteString("| Reviewer | Status | Inspected | Skipped | Constraints |\n")
	out.WriteString("|----------|--------|-----------|---------|-------------|\n")
	for _, entry := range coverage {
		fmt.Fprintf(out, "| %s | %s | %s | %s | %s |\n",
			escapeCell(entry.AgentID),
			escapeCell(entry.Status),
			escapeCell(orUnavailable(strings.Join(entry.InspectedFiles, ", "))),
			escapeCell(orUnavailable(strings.Join(entry.SkippedFiles, ", "))),
			escapeCell(coverageAnnotationCell(entry)),
		)
	}
	out.WriteString("\n")
}

func coverageAnnotationCell(entry ReviewerCoverageSummary) string {
	parts := append([]string(nil), entry.Constraints...)
	if strings.TrimSpace(entry.Diagnostic) != "" {
		parts = append(parts, entry.Diagnostic)
	}
	return orUnavailable(strings.Join(parts, "; "))
}

func writeRunFooter(out *strings.Builder, run RunSummary, totals AggregateUsage) {
	if !run.hasData() {
		return
	}
	out.WriteString("\n---\n<details>\n<summary>Completed in ")
	out.WriteString(formatDurationMS(run.WallDurationMS))
	out.WriteString(" | ")
	out.WriteString(formatUSDEst(totals.CostUSD, totals.CostEstimated))
	out.WriteString(" | ")
	out.WriteString(escapeCell(orUnavailable(run.Model)))
	out.WriteString(" | cr ")
	out.WriteString(escapeCell(orUnavailable(run.ToolVersion)))
	out.WriteString("</summary>\n\n")

	out.WriteString("| Field | Value |\n|---|---|\n")
	writeFooterRow(out, "Model", orUnavailable(run.Model))
	writeFooterRow(out, "Reviewers", orUnavailable(strings.Join(run.SelectedReviewers, ", ")))
	engine := unavailableValue
	if run.Adapter != "" || run.Model != "" {
		engine = orUnavailable(run.Adapter) + " · " + orUnavailable(run.Model)
	}
	writeFooterRow(out, "Engine", engine)
	reviewedBy := unavailableValue
	if strings.TrimSpace(run.PostingIdentity) != "" {
		reviewedBy = "cr · " + run.PostingIdentity
	}
	writeFooterRow(out, "Reviewed by", reviewedBy)
	duration := formatDurationMS(run.WallDurationMS)
	if totals.ComputeDurationMS != nil {
		duration += " wall · " + formatDurationMS(totals.ComputeDurationMS) + " compute"
	}
	writeFooterRow(out, "Duration", duration)
	writeFooterRow(out, "Cost", formatUSDEst(totals.CostUSD, totals.CostEstimated))
	tokens := unavailableValue
	if totals.TokensIn != nil || totals.TokensOut != nil {
		tokens = formatTokens(totals.TokensIn) + " in / " + formatTokens(totals.TokensOut) + " out"
	}
	writeFooterRow(out, "Tokens", tokens)

	if len(run.Workstreams) > 0 {
		out.WriteString("\n**Per-workstream usage**\n\n")
		out.WriteString("| Workstream | Model | In | Out | Cache read | Cache create | Cost | Duration |\n")
		out.WriteString("|---|---|---:|---:|---:|---:|---:|---:|\n")
		for _, workstream := range run.Workstreams {
			fmt.Fprintf(out, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
				escapeCell(workstream.Name),
				escapeCell(orUnavailable(workstream.Model)),
				formatTokens(workstream.TokensIn),
				formatTokens(workstream.TokensOut),
				formatTokens(workstream.CacheRead),
				formatTokens(workstream.CacheCreate),
				formatUSDEst(workstream.CostUSD, workstream.CostEstimated),
				formatDurationMS(workstream.DurationMS),
			)
		}
	}
	out.WriteString("\n</details>\n")
}

func writeFooterRow(out *strings.Builder, field, value string) {
	fmt.Fprintf(out, "| %s | %s |\n", field, escapeCell(value))
}

func orUnavailable(value string) string {
	if strings.TrimSpace(value) == "" {
		return unavailableValue
	}
	return value
}

var cellEscaper = strings.NewReplacer(
	"|", "\\|",
	"\n", " ",
	"\r", " ",
	"<", "&lt;",
	">", "&gt;",
)

// escapeCell makes a dynamic value safe for the public rollup comment:
// marker-prefix escaping plus markdown-table and HTML neutralization.
func escapeCell(value string) string {
	return strings.TrimSpace(cellEscaper.Replace(sanitize(value)))
}

func formatTokens(value *int) string {
	if value == nil {
		return unavailableValue
	}
	v := float64(*value)
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%d", *value)
	}
}

// formatUSDEst renders a dollar amount, marking it as an estimate
// ("~$X.XX (est.)") when the cost was derived from token prices rather than
// reported by the adapter.
func formatUSDEst(value *float64, estimated bool) string {
	if value == nil {
		return unavailableValue
	}
	if estimated {
		return fmt.Sprintf("~$%.2f (est.)", *value)
	}
	return fmt.Sprintf("$%.2f", *value)
}

func formatDurationMS(value *int64) string {
	if value == nil {
		return unavailableValue
	}
	totalSeconds := *value / 1000
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	case totalSeconds == 0 && *value > 0:
		return "<1s"
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
