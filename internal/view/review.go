package view

import (
	"encoding/json"
	"fmt"
	"io"
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
}

// ReviewThreadCounts summarizes PR discussion thread handling.
type ReviewThreadCounts struct {
	Considered int `json:"considered"`
	Summarized int `json:"summarized"`
	Resolved   int `json:"resolved"`
}

// ReviewRunSummary is the execution metadata rendered in the rollup footer.
type ReviewRunSummary struct {
	ToolVersion       string             `json:"tool_version,omitempty"`
	Adapter           string             `json:"adapter,omitempty"`
	Model             string             `json:"model,omitempty"`
	PostingIdentity   string             `json:"posting_identity,omitempty"`
	SelectedReviewers []string           `json:"selected_reviewers,omitempty"`
	WallDurationMS    *int64             `json:"wall_duration_ms"`
	Workstreams       []ReviewWorkstream `json:"workstreams"`
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
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	FindingID     string          `json:"finding_id,omitempty"`
	ThreadID      string          `json:"thread_id,omitempty"`
	Status        string          `json:"status"`
	Required      bool            `json:"required"`
	MarkerOmitted bool            `json:"marker_omitted"`
	Payload       json.RawMessage `json:"payload"`
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
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
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
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
