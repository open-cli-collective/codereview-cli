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
	// Workstreams holds the stages that ran in this round; a stage skipped by
	// reuse (e.g. selection under a reused reviewer cohort) is absent rather
	// than present with empty usage, which is what lets AggregateUsage's
	// every-workstream-reports rule produce totals for the round.
	Workstreams []WorkstreamUsage
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

// writeReviewerTable renders the headline per-reviewer counts.
//
// A reviewer that never produced a result must not be shown as "0". Zero
// findings and "did not run" are the same number and opposite meanings: the
// first says the code is clean, the second says nothing was examined. Rendering
// both as 0 let a run where four of five reviewers failed to start read as a
// clean review, with the failure visible only further down in the coverage
// section that a reader skimming the summary never reaches.
func writeReviewerTable(out *strings.Builder, reviewers []ReviewerSummary, coverage []ReviewerCoverageSummary) {
	produced := make(map[string]bool, len(coverage))
	for _, entry := range coverage {
		produced[entry.AgentID] = coverageResultProduced(entry.Status)
	}
	out.WriteString("| Reviewer | Findings |\n")
	out.WriteString("|----------|----------|\n")
	for _, reviewer := range reviewers {
		// Absent from coverage means nothing was reported either way; only an
		// explicit non-producing status is called out, so this cannot mask a
		// genuine zero.
		if ran, known := produced[reviewer.Name]; known && !ran {
			fmt.Fprintf(out, "| %s | ⚠️ did not run |\n", escapeCell(reviewer.Name))
			continue
		}
		fmt.Fprintf(out, "| %s | %d |\n", escapeCell(reviewer.Name), reviewer.Findings)
	}
	out.WriteString("\n")
}

func writeReviewerFailureDiagnostics(out *strings.Builder, failures []ReviewerFailureSummary) {
	if len(failures) == 0 {
		return
	}
	out.WriteString("### Reviewer Diagnostics\n\n")
	for _, failure := range failures {
		fmt.Fprintf(out, "- %s — failed: %s\n", codeSpan(failure.AgentID), escapeCell(failure.Error))
	}
	out.WriteString("\n")
}

// GitHub tables offer no column-width control, so variable-length values
// (paths, constraint prose) wrap mid-word and repeat per row. Coverage is
// rendered as a list instead: shared inspected files collapse into one
// details block, per-reviewer lines carry only deviations, and unknown
// fields are omitted rather than rendered as "unavailable".
func writeReviewerCoverageDiagnostics(out *strings.Builder, coverage []ReviewerCoverageSummary) {
	if len(coverage) == 0 {
		return
	}
	out.WriteString("### Reviewer Coverage\n\n")
	union := inspectedFileUnion(coverage)
	for _, entry := range coverage {
		fmt.Fprintf(out, "- %s — %s", codeSpan(entry.AgentID), coverageStatusLabel(entry.Status))
		var notes []string
		if len(entry.InspectedFiles) > 0 && !stringSetsEqual(entry.InspectedFiles, union) {
			assignedNoun := "files"
			if len(entry.InspectedFiles) == 1 {
				assignedNoun = "file"
			}
			notes = append(notes, fmt.Sprintf("inspected %d assigned %s (%d inspected across reviewers): %s",
				len(entry.InspectedFiles), assignedNoun, len(union), codeSpanList(entry.InspectedFiles)))
		}
		if len(entry.SkippedFiles) > 0 {
			notes = append(notes, "skipped: "+codeSpanList(entry.SkippedFiles))
		} else if coverageResultProduced(entry.Status) {
			notes = append(notes, "skipped: none")
		}
		if len(entry.Constraints) > 0 {
			// Constraints are independent sentences of reviewer prose; a
			// space joins them without stacking punctuation.
			notes = append(notes, "constraints: "+escapeCell(strings.Join(entry.Constraints, " ")))
		} else if coverageResultProduced(entry.Status) {
			notes = append(notes, "constraints: none")
		}
		if strings.TrimSpace(entry.Diagnostic) != "" {
			notes = append(notes, escapeCell(entry.Diagnostic))
		}
		if len(notes) > 0 {
			out.WriteString("; ")
			out.WriteString(strings.Join(notes, "; "))
		}
		out.WriteString("\n")
	}
	if len(union) > 0 {
		fmt.Fprintf(out, "\n<details>\n<summary>Inspected files (%d)</summary>\n\n", len(union))
		for _, file := range union {
			fmt.Fprintf(out, "- %s\n", codeSpan(file))
		}
		out.WriteString("\n</details>\n")
	}
	out.WriteString("\n")
}

// ReviewersProducedResults maps each reviewer to whether it actually produced
// a result. Both the rendered rollup and the JSON view derive their
// "did not run" state from this, so the two cannot disagree -- which is what
// Summary's contract promises and what a markdown-only fix would have broken.
func ReviewersProducedResults(coverage []ReviewerCoverageSummary) map[string]bool {
	produced := make(map[string]bool, len(coverage))
	for _, entry := range coverage {
		produced[entry.AgentID] = coverageResultProduced(entry.Status)
	}
	return produced
}

func coverageResultProduced(status string) bool {
	switch strings.TrimSpace(status) {
	case "complete_broad", "complete_constrained", "incomplete_skipped":
		return true
	default:
		return false
	}
}

// coverageStatusLabel humanizes the coverage status enum; the healthy states
// stay quiet and the exceptional ones lead with a marker so they stand out
// in the list. Unknown values pass through untranslated.
func coverageStatusLabel(status string) string {
	switch status {
	case "complete_broad":
		return "complete (broad)"
	case "complete_constrained":
		return "complete (constrained)"
	case "incomplete_skipped":
		return "⚠️ incomplete (skipped files)"
	case "incomplete_tool":
		return "⚠️ incomplete (tool failure)"
	case "incomplete_failed":
		return "⚠️ failed"
	case "incomplete_unassigned":
		return "⚠️ unassigned"
	default:
		return escapeCell(status)
	}
}

func inspectedFileUnion(coverage []ReviewerCoverageSummary) []string {
	seen := map[string]bool{}
	var union []string
	for _, entry := range coverage {
		for _, file := range entry.InspectedFiles {
			if !seen[file] {
				seen[file] = true
				union = append(union, file)
			}
		}
	}
	sort.Strings(union)
	return union
}

func stringSetsEqual(a, b []string) bool {
	set := map[string]bool{}
	for _, value := range a {
		set[value] = true
	}
	if len(set) != len(b) {
		return false
	}
	for _, value := range b {
		if !set[value] {
			return false
		}
	}
	return true
}

// codeSpan wraps a dynamic value in a markdown code span; backticks are
// replaced so the value cannot terminate the span early.
func codeSpan(value string) string {
	cleaned := strings.NewReplacer("`", "'", "\n", " ", "\r", " ").Replace(sanitize(value))
	return "`" + strings.TrimSpace(cleaned) + "`"
}

func codeSpanList(values []string) string {
	spans := make([]string, 0, len(values))
	for _, value := range values {
		spans = append(spans, codeSpan(value))
	}
	return strings.Join(spans, ", ")
}

func writeRunFooter(out *strings.Builder, run RunSummary, totals AggregateUsage) {
	if !run.hasData() {
		return
	}
	out.WriteString("\n---\n<details>\n<summary>Completed in ")
	out.WriteString(formatDurationMS(run.WallDurationMS))
	// The summary line has no field labels, so an unknown cost is omitted
	// rather than rendered as a bare "unavailable"; the labeled Cost row in
	// the table below still reports it.
	if totals.CostUSD != nil {
		out.WriteString(" | ")
		out.WriteString(formatUSDEst(totals.CostUSD, totals.CostEstimated))
	}
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
		// Nested bullets rather than an eight-column table: GitHub gives
		// tables no width control, so workstream names and headers wrap
		// mid-word. Labels make each value self-describing, so unknown
		// values stay as "unavailable" here (unlike the unlabeled summary
		// line, which omits them).
		for _, workstream := range run.Workstreams {
			fmt.Fprintf(out, "- %s — %s\n", codeSpan(workstream.Name), escapeCell(orUnavailable(workstream.Model)))
			fmt.Fprintf(out, "  - In: %s\n", formatTokens(workstream.TokensIn))
			fmt.Fprintf(out, "  - Out: %s\n", formatTokens(workstream.TokensOut))
			fmt.Fprintf(out, "  - Cache read: %s\n", formatTokens(workstream.CacheRead))
			fmt.Fprintf(out, "  - Cache create: %s\n", formatTokens(workstream.CacheCreate))
			fmt.Fprintf(out, "  - Cost: %s\n", formatUSDEst(workstream.CostUSD, workstream.CostEstimated))
			fmt.Fprintf(out, "  - Duration: %s\n", formatDurationMS(workstream.DurationMS))
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
