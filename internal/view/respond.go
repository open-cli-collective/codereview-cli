package view

import (
	"encoding/json"
	"fmt"
	"io"
)

// RespondResult is the presentation model for `cr respond`.
type RespondResult struct {
	PRURL      string          `json:"pr_url"`
	DryRun     bool            `json:"dry_run"`
	Considered int             `json:"considered"`
	Replied    int             `json:"replied"`
	Resolved   int             `json:"resolved"`
	Skipped    int             `json:"skipped"`
	Threads    []RespondThread `json:"threads"`
}

// RespondThread describes one thread the responder evaluated.
type RespondThread struct {
	ThreadID string `json:"thread_id"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Decision string `json:"decision"`
	Reply    string `json:"reply,omitempty"`
	Resolved bool   `json:"resolved"`
	Posted   bool   `json:"posted"`
}

// RenderRespondText writes a stable human-readable responder summary.
func RenderRespondText(w io.Writer, result RespondResult) error {
	mode := "live"
	if result.DryRun {
		mode = "dry-run"
	}
	if err := writeKV(w, "PR", result.PRURL); err != nil {
		return err
	}
	if err := writeKV(w, "Mode", mode); err != nil {
		return err
	}
	if err := writeKV(w, "Threads considered", fmt.Sprint(result.Considered)); err != nil {
		return err
	}
	if err := writeKV(w, "Replied", fmt.Sprint(result.Replied)); err != nil {
		return err
	}
	if err := writeKV(w, "Resolved", fmt.Sprint(result.Resolved)); err != nil {
		return err
	}
	if err := writeKV(w, "Skipped", fmt.Sprint(result.Skipped)); err != nil {
		return err
	}
	for _, thread := range result.Threads {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		anchor := thread.Path
		if anchor != "" && thread.Line > 0 {
			anchor = fmt.Sprintf("%s:%d", thread.Path, thread.Line)
		}
		if err := writeKV(w, "Thread", thread.ThreadID); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "Location", anchor); err != nil {
			return err
		}
		if err := writeKV(w, "Decision", thread.Decision); err != nil {
			return err
		}
		if err := writeKV(w, "Resolved", fmt.Sprint(thread.Resolved)); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "Reply", thread.Reply); err != nil {
			return err
		}
	}
	return nil
}

// RenderRespondJSON writes the responder summary as indented JSON.
func RenderRespondJSON(w io.Writer, result RespondResult) error {
	if result.Threads == nil {
		result.Threads = []RespondThread{}
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
