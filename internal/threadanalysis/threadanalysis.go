// Package threadanalysis turns normalized inline thread context into reusable
// lifecycle decisions through the durable LLM execution boundary.
package threadanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmrun"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

const (
	inputSchemaVersion  = 1
	outputSchemaVersion = 1
)

// Decision is the reusable lifecycle action selected for one thread.
type Decision string

// Thread lifecycle decisions.
const (
	DecisionSkip        Decision = "skip"
	DecisionReplyOnly   Decision = "reply_only"
	DecisionAcknowledge Decision = "acknowledge"
	DecisionClarify     Decision = "clarify"
	DecisionConcede     Decision = "concede"
	DecisionSummarize   Decision = "summarize"
)

// Valid reports whether d is a known thread analysis decision.
func (d Decision) Valid() bool {
	switch d {
	case DecisionSkip, DecisionReplyOnly, DecisionAcknowledge, DecisionClarify, DecisionConcede, DecisionSummarize:
		return true
	default:
		return false
	}
}

// Result is the domain result returned by thread analysis.
type Result struct {
	ThreadID  string   `json:"thread_id"`
	Decision  Decision `json:"decision"`
	ReplyBody string   `json:"reply_body,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Resolve   bool     `json:"resolve"`
	Rationale string   `json:"rationale,omitempty"`
}

// Options are explicit call-site supplied dependencies for one analysis run.
type Options struct {
	Store           llmrun.Store
	RunID           string
	Provider        string
	Adapter         llm.Adapter
	Model           string
	Effort          string
	LogPath         string
	ResumeSessionID string
	Now             func() time.Time
	NewStepID       func() string
}

// AnalyzeThread analyzes one normalized inline thread through llmrun.
func AnalyzeThread(ctx context.Context, opts Options, thread threadcontext.Thread) (Result, error) {
	var zero Result
	if err := validateOptions(opts); err != nil {
		return zero, err
	}
	threadID := strings.TrimSpace(string(thread.ID))
	if threadID == "" {
		return zero, fmt.Errorf("threadanalysis: thread ID is required")
	}
	input := analysisInputForThread(threadID, thread)
	inputHash, err := hashInput(input)
	if err != nil {
		return zero, err
	}
	prompt, err := promptForInput(input)
	if err != nil {
		return zero, err
	}
	result, err := llmrun.RunStructuredStep(ctx, opts.Store, llmrun.Request{
		RunID:           opts.RunID,
		Stage:           stagemodel.StageThreadAnalysis,
		ScopeKey:        "thread:" + threadID,
		InputHash:       inputHash,
		Provider:        opts.Provider,
		Adapter:         opts.Adapter,
		Model:           opts.Model,
		Effort:          opts.Effort,
		Prompt:          prompt,
		LogPath:         opts.LogPath,
		ResumeSessionID: opts.ResumeSessionID,
		Now:             opts.Now,
		NewStepID:       opts.NewStepID,
	}, decodeResultForThread(threadID))
	if err != nil {
		return zero, err
	}
	return result.Value, nil
}

// AnalyzeThreads analyzes threads serially, preserving input order.
func AnalyzeThreads(ctx context.Context, opts Options, threads []threadcontext.Thread) ([]Result, error) {
	results := make([]Result, 0, len(threads))
	for _, thread := range threads {
		result, err := AnalyzeThread(ctx, opts, thread)
		if err != nil {
			return nil, fmt.Errorf("threadanalysis: analyze thread %s: %w", string(thread.ID), err)
		}
		results = append(results, result)
	}
	return results, nil
}

func validateOptions(opts Options) error {
	if opts.Store == nil {
		return fmt.Errorf("threadanalysis: store is required")
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return fmt.Errorf("threadanalysis: run ID is required")
	}
	if strings.TrimSpace(opts.Provider) == "" {
		return fmt.Errorf("threadanalysis: provider is required")
	}
	if opts.Adapter == nil {
		return fmt.Errorf("threadanalysis: adapter is required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return fmt.Errorf("threadanalysis: model is required")
	}
	if opts.NewStepID == nil {
		return fmt.Errorf("threadanalysis: step ID generator is required")
	}
	return nil
}

func decodeResultForThread(threadID string) llm.Decoder[Result] {
	return func(data []byte) (Result, error) {
		var raw resultOutput
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&raw); err != nil {
			return Result{}, fmt.Errorf("threadanalysis: decode result: %w", err)
		}
		var extra struct{}
		if err := decoder.Decode(&extra); err != io.EOF {
			return Result{}, fmt.Errorf("threadanalysis: decode result: trailing data is not allowed")
		}
		if raw.SchemaVersion != outputSchemaVersion {
			return Result{}, fmt.Errorf("threadanalysis: schema_version = %d, want %d", raw.SchemaVersion, outputSchemaVersion)
		}
		result := Result{
			ThreadID:  strings.TrimSpace(raw.ThreadID),
			Decision:  Decision(strings.ToLower(strings.TrimSpace(string(raw.Decision)))),
			ReplyBody: sanitize(raw.ReplyBody),
			Summary:   sanitize(raw.Summary),
			Resolve:   raw.Resolve,
			Rationale: sanitize(raw.Rationale),
		}
		if result.ThreadID != threadID {
			return Result{}, fmt.Errorf("threadanalysis: thread_id = %q, want %q", result.ThreadID, threadID)
		}
		if !result.Decision.Valid() {
			return Result{}, fmt.Errorf("threadanalysis: decision %q is invalid", result.Decision)
		}
		if err := validateResult(result); err != nil {
			return Result{}, err
		}
		return result, nil
	}
}

func validateResult(result Result) error {
	if result.Decision != DecisionSkip && strings.TrimSpace(result.Rationale) == "" {
		return fmt.Errorf("threadanalysis: rationale is required for decision %q", result.Decision)
	}
	switch result.Decision {
	case DecisionSkip:
		if result.ReplyBody != "" {
			return fmt.Errorf("threadanalysis: reply_body is not allowed for decision %q", result.Decision)
		}
		if result.Summary != "" {
			return fmt.Errorf("threadanalysis: summary is not allowed for decision %q", result.Decision)
		}
		if result.Resolve {
			return fmt.Errorf("threadanalysis: resolve must be false for decision %q", result.Decision)
		}
	case DecisionReplyOnly:
		if result.ReplyBody == "" {
			return fmt.Errorf("threadanalysis: reply_body is required for decision %q", result.Decision)
		}
		if result.Summary != "" {
			return fmt.Errorf("threadanalysis: summary is not allowed for decision %q", result.Decision)
		}
		if result.Resolve {
			return fmt.Errorf("threadanalysis: resolve must be false for decision %q", result.Decision)
		}
	case DecisionAcknowledge, DecisionConcede:
		if result.ReplyBody == "" {
			return fmt.Errorf("threadanalysis: reply_body is required for decision %q", result.Decision)
		}
		if result.Resolve && result.Summary == "" {
			return fmt.Errorf("threadanalysis: summary is required when resolve is true")
		}
	case DecisionClarify:
		if result.ReplyBody == "" {
			return fmt.Errorf("threadanalysis: reply_body is required for decision %q", result.Decision)
		}
		if result.Summary != "" {
			return fmt.Errorf("threadanalysis: summary is not allowed for decision %q", result.Decision)
		}
		if result.Resolve {
			return fmt.Errorf("threadanalysis: resolve must be false for decision %q", result.Decision)
		}
	case DecisionSummarize:
		if result.ReplyBody != "" {
			return fmt.Errorf("threadanalysis: reply_body is not allowed for decision %q", result.Decision)
		}
		if result.Summary == "" {
			return fmt.Errorf("threadanalysis: summary is required for decision %q", result.Decision)
		}
		if !result.Resolve {
			return fmt.Errorf("threadanalysis: resolve must be true for decision %q", result.Decision)
		}
	}
	return nil
}

func analysisInputForThread(threadID string, thread threadcontext.Thread) analysisInput {
	comments := make([]analysisComment, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		comments = append(comments, analysisComment{
			ID:                        sanitize(string(comment.ID)),
			ThreadID:                  sanitize(string(comment.ThreadID)),
			Body:                      sanitize(comment.Body),
			Author:                    analysisIdentityFor(comment.Author.Login, comment.Author.ID, comment.Author.DisplayName),
			Anchor:                    analysisAnchorFor(comment.Anchor),
			URL:                       sanitize(comment.URL),
			CreatedAt:                 formatTime(comment.CreatedAt),
			UpdatedAt:                 formatTime(comment.UpdatedAt),
			AuthoredByPostingIdentity: comment.AuthoredByPostingIdentity,
			HasFindingMarker:          comment.HasFindingMarker,
			HasThreadReplyMarker:      comment.HasThreadReplyMarker,
			HasThreadSummaryMarker:    comment.HasThreadSummaryMarker,
		})
	}
	return analysisInput{
		SchemaVersion: inputSchemaVersion,
		ThreadID:      sanitize(threadID),
		Resolved:      thread.Resolved,
		Anchor:        analysisAnchorFor(thread.Anchor),
		Status: analysisStatus{
			CRAuthoredFinding:       thread.Status.CRAuthoredFinding,
			HasCRSummary:            thread.Status.HasCRSummary,
			LatestCRCommentID:       commentID(thread.Status.LatestCRComment),
			LatestHumanReplyAfterCR: commentID(thread.Status.LatestHumanReplyAfterCR),
			PendingHumanReply:       thread.Status.PendingHumanReply,
		},
		Comments: comments,
	}
}

func promptForInput(input analysisInput) (string, error) {
	contextJSON, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return "", fmt.Errorf("threadanalysis: encode prompt context: %w", err)
	}
	prompt := strings.Join([]string{
		"Analyze this inline code-review discussion thread.",
		"Return JSON only. Do not include markdown fences or prose outside JSON.",
		"Use schema_version 1 and fields: thread_id, decision, reply_body, summary, resolve, rationale.",
		"Decisions: skip, reply_only, acknowledge, clarify, concede, summarize.",
		"Use reply_only only when a non-resolving inline reply is enough.",
		"Use acknowledge or concede when replying to a human and optionally resolving with a durable summary.",
		"Use clarify when a human-facing question is needed and do not resolve.",
		"Use summarize only when no reply is needed and the thread should be resolved with a durable summary.",
		"Thread context JSON:",
		string(contextJSON),
	}, "\n\n")
	if strings.Contains(prompt, "<!-- codereview:") {
		return "", fmt.Errorf("threadanalysis: prompt contains live codereview marker")
	}
	return prompt, nil
}

func hashInput(input analysisInput) (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("threadanalysis: encode input hash: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func analysisAnchorFor(anchor threadcontext.Anchor) analysisAnchor {
	return analysisAnchor{
		Path:        sanitize(anchor.Path),
		Side:        sanitize(string(anchor.Side)),
		Line:        anchor.Line,
		SubjectType: sanitize(string(anchor.SubjectType)),
		CommitSHA:   sanitize(anchor.CommitSHA),
	}
}

func analysisIdentityFor(login, id, displayName string) analysisIdentity {
	return analysisIdentity{
		Login:       sanitize(login),
		ID:          sanitize(id),
		DisplayName: sanitize(displayName),
	}
}

func commentID(comment *threadcontext.Comment) string {
	if comment == nil {
		return ""
	}
	return sanitize(string(comment.ID))
}

func sanitize(value string) string {
	return threadcontext.SanitizeBody(value)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

type resultOutput struct {
	SchemaVersion int      `json:"schema_version"`
	ThreadID      string   `json:"thread_id"`
	Decision      Decision `json:"decision"`
	ReplyBody     string   `json:"reply_body"`
	Summary       string   `json:"summary"`
	Resolve       bool     `json:"resolve"`
	Rationale     string   `json:"rationale"`
}

type analysisInput struct {
	SchemaVersion int               `json:"schema_version"`
	ThreadID      string            `json:"thread_id"`
	Resolved      bool              `json:"resolved"`
	Anchor        analysisAnchor    `json:"anchor"`
	Status        analysisStatus    `json:"status"`
	Comments      []analysisComment `json:"comments"`
}

type analysisAnchor struct {
	Path        string `json:"path"`
	Side        string `json:"side"`
	Line        int    `json:"line"`
	SubjectType string `json:"subject_type"`
	CommitSHA   string `json:"commit_sha"`
}

type analysisStatus struct {
	CRAuthoredFinding       bool   `json:"cr_authored_finding"`
	HasCRSummary            bool   `json:"has_cr_summary"`
	LatestCRCommentID       string `json:"latest_cr_comment_id"`
	LatestHumanReplyAfterCR string `json:"latest_human_reply_after_cr_id"`
	PendingHumanReply       bool   `json:"pending_human_reply"`
}

type analysisComment struct {
	ID                        string           `json:"id"`
	ThreadID                  string           `json:"thread_id"`
	Body                      string           `json:"body"`
	Author                    analysisIdentity `json:"author"`
	Anchor                    analysisAnchor   `json:"anchor"`
	URL                       string           `json:"url"`
	CreatedAt                 string           `json:"created_at"`
	UpdatedAt                 string           `json:"updated_at"`
	AuthoredByPostingIdentity bool             `json:"authored_by_posting_identity"`
	HasFindingMarker          bool             `json:"has_finding_marker"`
	HasThreadReplyMarker      bool             `json:"has_thread_reply_marker"`
	HasThreadSummaryMarker    bool             `json:"has_thread_summary_marker"`
}

type analysisIdentity struct {
	Login       string `json:"login"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}
