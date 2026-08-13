package view

import (
	"fmt"
	"io"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// ReviewDryRun is the presentation model for `cr review --dry-run`.
type ReviewDryRun struct {
	Run             ReviewRun       `json:"run"`
	Quota           *ReviewQuota    `json:"quota,omitempty"`
	RollupMarkdown  string          `json:"rollup_markdown"`
	Summary         ReviewSummary   `json:"summary"`
	Findings        []ReviewFinding `json:"findings"`
	Actions         []ReviewAction  `json:"actions"`
	Artifacts       ReviewArtifacts `json:"artifacts"`
	FailOnTriggered bool            `json:"fail_on_triggered"`
}

// ReviewSummary mirrors the derived rollup metadata the rendered comment was
// built from. Usage fields are nullable; null means not reported, never zero.
type ReviewSummary struct {
	Reviewers []ReviewReviewerSummary `json:"reviewers"`
	Threads   ReviewThreadCounts      `json:"threads"`
	Run       ReviewRunSummary        `json:"run"`
	Totals    ReviewWorkstreamTotals  `json:"totals"`
}

// ReviewReviewerSummary is one reviewer row with its rendered finding count.
type ReviewReviewerSummary struct {
	Name     string `json:"name"`
	Findings int    `json:"findings"`
	// Ran is false when the reviewer produced no result. Without it a failed
	// reviewer serializes as findings: 0, which a consumer cannot tell from a
	// genuinely clean one. Omitted when coverage says nothing either way, so an
	// unknown status is not reported as a failure.
	Ran *bool `json:"ran,omitempty"`
}

// ReviewReviewerCoverageSummary describes reviewer coverage rendered in the
// rollup summary.
type ReviewReviewerCoverageSummary struct {
	AgentID        string   `json:"agent_id"`
	Status         string   `json:"status"`
	Scope          []string `json:"scope,omitempty"`
	InspectedFiles []string `json:"inspected_files,omitempty"`
	SkippedFiles   []string `json:"skipped_files,omitempty"`
	Constraints    []string `json:"constraints,omitempty"`
	Diagnostic     string   `json:"diagnostic,omitempty"`
}

// ReviewThreadCounts summarizes PR discussion thread handling.
type ReviewThreadCounts struct {
	Considered int `json:"considered"`
	Summarized int `json:"summarized"`
	Resolved   int `json:"resolved"`
}

// ReviewRunSummary is the execution metadata rendered in the rollup footer.
type ReviewRunSummary struct {
	ToolVersion       string                          `json:"tool_version,omitempty"`
	Adapter           string                          `json:"adapter,omitempty"`
	Model             string                          `json:"model,omitempty"`
	PostingIdentity   string                          `json:"posting_identity,omitempty"`
	SelectedReviewers []string                        `json:"selected_reviewers,omitempty"`
	ReviewerCoverage  []ReviewReviewerCoverageSummary `json:"reviewer_coverage,omitempty"`
	WallDurationMS    *int64                          `json:"wall_duration_ms"`
	Workstreams       []ReviewWorkstream              `json:"workstreams"`
}

// ReviewWorkstream is adapter-reported usage for one workstream.
type ReviewWorkstream struct {
	Name        string   `json:"name"`
	Model       string   `json:"model,omitempty"`
	TokensIn    *int     `json:"tokens_in"`
	TokensOut   *int     `json:"tokens_out"`
	CacheRead   *int     `json:"cache_read"`
	CacheCreate *int     `json:"cache_create"`
	CostUSD     *float64 `json:"cost_usd"`
	DurationMS  *int64   `json:"duration_ms"`
}

// ReviewWorkstreamTotals holds run-wide aggregates; each field is non-null
// only when every workstream reported it.
type ReviewWorkstreamTotals struct {
	TokensIn          *int     `json:"tokens_in"`
	TokensOut         *int     `json:"tokens_out"`
	CacheRead         *int     `json:"cache_read"`
	CacheCreate       *int     `json:"cache_create"`
	CostUSD           *float64 `json:"cost_usd"`
	ComputeDurationMS *int64   `json:"compute_duration_ms"`
}

// ReviewLive is the presentation model for live `cr review`.
type ReviewLive struct {
	Run             ReviewRun       `json:"run"`
	Status          string          `json:"status"`
	Decision        string          `json:"decision,omitempty"`
	Message         string          `json:"message,omitempty"`
	Outbox          ReviewOutbox    `json:"outbox"`
	Artifacts       ReviewArtifacts `json:"artifacts"`
	FailOnTriggered bool            `json:"fail_on_triggered"`
}

// ReviewRun describes the durable run envelope.
type ReviewRun struct {
	RunID          string `json:"run_id"`
	PRURL          string `json:"pr_url"`
	PRKey          string `json:"pr_key"`
	PostMode       string `json:"post_mode"`
	Outcome        string `json:"outcome"`
	ArtifactPath   string `json:"artifact_path"`
	BaseSHA        string `json:"base_sha,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	CurrentBaseSHA string `json:"current_base_sha,omitempty"`
	CurrentHeadSHA string `json:"current_head_sha,omitempty"`
}

// ReviewOutbox summarizes live posting state.
type ReviewOutbox struct {
	Outcome        string `json:"outcome,omitempty"`
	ExitCode       int    `json:"exit_code"`
	Posted         int    `json:"posted"`
	Pending        int    `json:"pending"`
	FailedTerminal int    `json:"failed_terminal"`
	Aborted        bool   `json:"aborted"`
}

// ReviewQuota describes adapter quota when the adapter supports it.
type ReviewQuota struct {
	BlockRemainingPct  float64 `json:"block_remaining_pct"`
	WeeklyRemainingPct float64 `json:"weekly_remaining_pct"`
	Low                bool    `json:"low"`
}

// ReviewFinding is one dry-run finding summary.
type ReviewFinding struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	FilePath  string `json:"file_path"`
	Anchoring string `json:"anchoring"`
	Side      string `json:"side,omitempty"`
	Line      *int   `json:"line,omitempty"`
	Body      string `json:"body"`
}

// ReviewAction is one planned action.
type ReviewAction struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	FindingID     string `json:"finding_id,omitempty"`
	ThreadID      string `json:"thread_id,omitempty"`
	Status        string `json:"status"`
	Required      bool   `json:"required"`
	MarkerOmitted bool   `json:"marker_omitted"`
	Payload       any    `json:"payload"`
}

// ReviewArtifacts lists dry-run artifact paths.
type ReviewArtifacts struct {
	Dir            string `json:"dir"`
	DiffPatch      string `json:"diff_patch"`
	SlicesDir      string `json:"slices_dir"`
	FindingsJSON   string `json:"findings_json"`
	RollupMarkdown string `json:"rollup_markdown"`
	AgentLogsDir   string `json:"agent_logs_dir"`
}

// NewReviewDryRun maps a pipeline result to the shared dry-run presentation model.
func NewReviewDryRun(result pipeline.Result) (ReviewDryRun, error) {
	outcome := ledger.OutcomeDryRun.String()
	if result.Run.Outcome != nil {
		outcome = result.Run.Outcome.String()
	}
	rendered := ReviewDryRun{
		Run: ReviewRun{
			RunID:        result.Run.RunID,
			PRURL:        result.PR.URL,
			PRKey:        result.PRKey,
			PostMode:     result.Run.PostMode.String(),
			Outcome:      outcome,
			ArtifactPath: result.Run.ArtifactPath,
			BaseSHA:      result.ReviewBaseSHA,
			HeadSHA:      result.ReviewHeadSHA,
		},
		RollupMarkdown:  result.Plan.RollupMarkdown,
		Summary:         newReviewSummary(result.Plan.Summary),
		FailOnTriggered: result.FailOnTriggered,
		Artifacts: ReviewArtifacts{
			Dir:            result.Artifacts.Dir,
			DiffPatch:      result.Artifacts.DiffPatch,
			SlicesDir:      result.Artifacts.SlicesDir,
			FindingsJSON:   result.Artifacts.FindingsJSON,
			RollupMarkdown: result.Artifacts.RollupMarkdown,
			AgentLogsDir:   result.Artifacts.AgentLogsDir,
		},
	}
	if result.QuotaSupported {
		rendered.Quota = &ReviewQuota{
			BlockRemainingPct:  result.Quota.BlockRemainingPct,
			WeeklyRemainingPct: result.Quota.WeeklyRemainingPct,
			Low:                result.QuotaLow,
		}
	}
	if result.CurrentBaseSHA != "" && result.CurrentBaseSHA != result.ReviewBaseSHA {
		rendered.Run.CurrentBaseSHA = result.CurrentBaseSHA
	}
	if result.CurrentHeadSHA != "" && result.CurrentHeadSHA != result.ReviewHeadSHA {
		rendered.Run.CurrentHeadSHA = result.CurrentHeadSHA
	}
	for _, finding := range result.Plan.AnchoredFindings {
		rendered.Findings = append(rendered.Findings, newReviewFinding(finding))
	}
	planned := map[string]ledger.PlannedAction{}
	for _, action := range result.PlannedActions {
		planned[action.ActionID] = action
	}
	for _, action := range result.Plan.Actions {
		renderedAction, err := newReviewAction(action, planned[action.ActionID])
		if err != nil {
			return ReviewDryRun{}, err
		}
		rendered.Actions = append(rendered.Actions, renderedAction)
	}
	return rendered, nil
}

func newReviewSummary(summary reviewplan.Summary) ReviewSummary {
	// Arrays serialize as [], never null, so JSON consumers see one shape.
	out := ReviewSummary{
		Reviewers: []ReviewReviewerSummary{},
		Threads: ReviewThreadCounts{
			Considered: summary.Threads.Considered,
			Summarized: summary.Threads.Summarized,
			Resolved:   summary.Threads.Resolved,
		},
		Run: ReviewRunSummary{
			ToolVersion:       summary.Run.ToolVersion,
			Adapter:           summary.Run.Adapter,
			Model:             summary.Run.Model,
			PostingIdentity:   summary.Run.PostingIdentity,
			SelectedReviewers: summary.Run.SelectedReviewers,
			ReviewerCoverage:  []ReviewReviewerCoverageSummary{},
			WallDurationMS:    summary.Run.WallDurationMS,
			Workstreams:       []ReviewWorkstream{},
		},
		Totals: ReviewWorkstreamTotals{
			TokensIn:          summary.Totals.TokensIn,
			TokensOut:         summary.Totals.TokensOut,
			CacheRead:         summary.Totals.CacheRead,
			CacheCreate:       summary.Totals.CacheCreate,
			CostUSD:           summary.Totals.CostUSD,
			ComputeDurationMS: summary.Totals.ComputeDurationMS,
		},
	}
	produced := reviewplan.ReviewersProducedResults(summary.Run.ReviewerCoverage)
	for _, reviewer := range summary.Reviewers {
		row := ReviewReviewerSummary{Name: reviewer.Name, Findings: reviewer.Findings}
		if ran, known := produced[reviewer.Name]; known {
			row.Ran = &ran
		}
		out.Reviewers = append(out.Reviewers, row)
	}
	for _, coverage := range summary.Run.ReviewerCoverage {
		out.Run.ReviewerCoverage = append(out.Run.ReviewerCoverage, ReviewReviewerCoverageSummary{
			AgentID:        coverage.AgentID,
			Status:         coverage.Status,
			Scope:          coverage.Scope,
			InspectedFiles: coverage.InspectedFiles,
			SkippedFiles:   coverage.SkippedFiles,
			Constraints:    coverage.Constraints,
			Diagnostic:     coverage.Diagnostic,
		})
	}
	for _, workstream := range summary.Run.Workstreams {
		out.Run.Workstreams = append(out.Run.Workstreams, ReviewWorkstream{
			Name:        workstream.Name,
			Model:       workstream.Model,
			TokensIn:    workstream.TokensIn,
			TokensOut:   workstream.TokensOut,
			CacheRead:   workstream.CacheRead,
			CacheCreate: workstream.CacheCreate,
			CostUSD:     workstream.CostUSD,
			DurationMS:  workstream.DurationMS,
		})
	}
	return out
}

func newReviewFinding(finding reviewplan.AnchoredFinding) ReviewFinding {
	out := ReviewFinding{
		ID:        finding.FindingID.String(),
		Severity:  finding.Severity.String(),
		FilePath:  finding.FilePath,
		Anchoring: finding.Anchoring.String(),
		Body:      finding.Body,
	}
	if finding.Side != nil {
		out.Side = finding.Side.String()
	}
	if finding.Line != nil {
		line := *finding.Line
		out.Line = &line
	}
	return out
}

func newReviewAction(action reviewplan.Action, planned ledger.PlannedAction) (ReviewAction, error) {
	status := action.Status
	payload, err := action.Payload()
	if err != nil {
		return ReviewAction{}, fmt.Errorf("review: action %q: %w", action.ActionID, err)
	}
	if planned.ActionID != "" {
		if planned.PayloadDecodeError != nil {
			return ReviewAction{}, fmt.Errorf("review: planned action %q payload: %w", planned.ActionID, planned.PayloadDecodeError)
		}
		payload, err = planned.Payload()
		if err != nil {
			return ReviewAction{}, fmt.Errorf("review: planned action %q: %w", planned.ActionID, err)
		}
		if planned.Status != "" {
			status = reviewplan.ActionStatus(planned.Status.String())
		}
	}
	out := ReviewAction{
		ID:            action.ActionID,
		Kind:          string(action.Kind),
		Status:        string(status),
		Required:      action.Required,
		MarkerOmitted: action.Marker.BodyBearing,
		Payload:       payload,
	}
	if action.FindingID.Assigned() {
		out.FindingID = action.FindingID.String()
	}
	if strings.TrimSpace(action.ThreadID) != "" {
		out.ThreadID = action.ThreadID
	}
	return out, nil
}

// RenderReviewDryRunText writes a human-readable dry-run summary.
func RenderReviewDryRunText(w io.Writer, result ReviewDryRun) error {
	if result.Quota != nil {
		label := "Quota"
		if result.Quota.Low {
			label = "Quota low"
		}
		if _, err := fmt.Fprintf(w, "%s: block %.0f%%, weekly %.0f%%\n", label, result.Quota.BlockRemainingPct, result.Quota.WeeklyRemainingPct); err != nil {
			return err
		}
	}
	if err := writeKV(w, "Run", result.Run.RunID); err != nil {
		return err
	}
	if err := writeKV(w, "Post mode", result.Run.PostMode); err != nil {
		return err
	}
	if err := writeKV(w, "Outcome", result.Run.Outcome); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "PR", result.Run.PRURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if result.RollupMarkdown != "" {
		if _, err := fmt.Fprintln(w, result.RollupMarkdown); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "Planned actions:"); err != nil {
		return err
	}
	if len(result.Actions) == 0 {
		if _, err := fmt.Fprintln(w, "  - none"); err != nil {
			return err
		}
	} else {
		for _, action := range result.Actions {
			if _, err := fmt.Fprintf(w, "  - %s %s [%s]", action.ID, action.Kind, action.Status); err != nil {
				return err
			}
			if action.Required {
				if _, err := fmt.Fprint(w, " required"); err != nil {
					return err
				}
			}
			if action.MarkerOmitted {
				if _, err := fmt.Fprint(w, " marker: omitted in dry-run"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w, "Artifacts:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Directory", result.Artifacts.Dir); err != nil {
		return err
	}
	if err := writeKV(w, "  Findings", result.Artifacts.FindingsJSON); err != nil {
		return err
	}
	return writeKV(w, "  Rollup", result.Artifacts.RollupMarkdown)
}

// RenderReviewDryRunJSON writes a dry-run summary as indented JSON.
func RenderReviewDryRunJSON(w io.Writer, result ReviewDryRun) error {
	return RenderJSON(w, result)
}

// RenderReviewLiveText writes a human-readable live review summary.
func RenderReviewLiveText(w io.Writer, result ReviewLive) error {
	if err := writeKV(w, "Run", result.Run.RunID); err != nil {
		return err
	}
	if err := writeKV(w, "Status", result.Status); err != nil {
		return err
	}
	if result.Decision != "" {
		if err := writeKV(w, "Decision", result.Decision); err != nil {
			return err
		}
	}
	if err := writeOptionalKV(w, "Outcome", result.Outbox.Outcome); err != nil {
		return err
	}
	if result.Outbox.ExitCode != 0 {
		if err := writeKV(w, "Exit code", fmt.Sprint(result.Outbox.ExitCode)); err != nil {
			return err
		}
	}
	if result.Message != "" {
		if err := writeKV(w, "Message", result.Message); err != nil {
			return err
		}
	}
	if result.FailOnTriggered {
		if err := writeKV(w, "Fail-on", "triggered"); err != nil {
			return err
		}
	}
	if err := writeOptionalKV(w, "PR", result.Run.PRURL); err != nil {
		return err
	}
	if result.Run.ArtifactPath != "" {
		if err := writeKV(w, "Artifacts", result.Run.ArtifactPath); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Posts: posted=%d pending=%d failed_terminal=%d\n", result.Outbox.Posted, result.Outbox.Pending, result.Outbox.FailedTerminal); err != nil {
		return err
	}
	return nil
}

// RenderReviewLiveJSON writes a live review summary as indented JSON.
func RenderReviewLiveJSON(w io.Writer, result ReviewLive) error {
	return RenderJSON(w, result)
}
