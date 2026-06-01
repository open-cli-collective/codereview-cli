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
	Findings        []ReviewFinding `json:"findings"`
	Actions         []ReviewAction  `json:"actions"`
	Artifacts       ReviewArtifacts `json:"artifacts"`
	FailOnTriggered bool            `json:"fail_on_triggered"`
}

// ReviewRun describes the durable run envelope.
type ReviewRun struct {
	RunID        string `json:"run_id"`
	PRURL        string `json:"pr_url"`
	PRKey        string `json:"pr_key"`
	PostMode     string `json:"post_mode"`
	Outcome      string `json:"outcome"`
	ArtifactPath string `json:"artifact_path"`
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
