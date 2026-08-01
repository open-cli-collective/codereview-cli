// Package threadanalysis turns normalized inline thread context into reusable
// lifecycle decisions through the durable LLM execution boundary.
package threadanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/review"
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
	Store           llmlifecycle.Store
	RunID           string
	Adapter         llm.Adapter
	Model           string
	Effort          string
	LogPath         string
	LifecyclePaths  llmlifecycle.Paths
	Progress        llmlifecycle.Progress
	Now             func() time.Time
	NewStepID       func() string
	ResumeSessionID string
	OnSessionID     func(string) error
}

// AnalyzeThread analyzes one normalized inline thread through the durable LLM lifecycle.
func AnalyzeThread(ctx context.Context, opts Options, thread threadcontext.Thread) (Result, error) {
	result, _, err := analyzeThread(ctx, opts, thread)
	return result, err
}

func analyzeThread(ctx context.Context, opts Options, thread threadcontext.Thread) (Result, llmlifecycle.SessionDraft, error) {
	var zero Result
	if err := validateOptions(opts); err != nil {
		return zero, llmlifecycle.SessionDraft{}, err
	}
	threadID := strings.TrimSpace(string(thread.ID))
	if threadID == "" {
		return zero, llmlifecycle.SessionDraft{}, fmt.Errorf("threadanalysis: thread ID is required")
	}
	request, err := lifecycleRequestForThread(opts, threadID, thread)
	if err != nil {
		return zero, llmlifecycle.SessionDraft{}, err
	}
	result, err := llmlifecycle.RunStructured(ctx, request, decodeResultForThread(threadID))
	if err != nil {
		return zero, result.Draft, err
	}
	return result.Value, result.Draft, nil
}

// AnalyzeThreads analyzes normalized inline threads in order, using logPath to
// select each thread's provider log artifact.
func AnalyzeThreads(ctx context.Context, opts Options, threads []threadcontext.Thread, logPath func(threadcontext.Thread) (string, error)) ([]Result, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if logPath == nil {
		return nil, fmt.Errorf("threadanalysis: log path function is required")
	}
	results := make([]Result, 0, len(threads))
	for _, thread := range threads {
		var err error
		opts.LogPath, err = logPath(thread)
		if err != nil {
			return nil, err
		}
		result, draft, err := analyzeThread(ctx, opts, thread)
		if sessionID := strings.TrimSpace(draft.ProviderReportedSessionID); sessionID != "" {
			if opts.OnSessionID != nil {
				if checkpointErr := opts.OnSessionID(sessionID); checkpointErr != nil {
					return nil, checkpointErr
				}
			}
			opts.ResumeSessionID = sessionID
		}
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// ValidateCachedThread verifies that a cached analysis task still matches the
// current normalized thread input without invoking the provider.
func ValidateCachedThread(opts Options, thread threadcontext.Thread) error {
	if err := validateOptions(opts); err != nil {
		return err
	}
	threadID := strings.TrimSpace(string(thread.ID))
	if threadID == "" {
		return fmt.Errorf("threadanalysis: thread ID is required")
	}
	request, err := lifecycleRequestForThread(opts, threadID, thread)
	if err != nil {
		return err
	}
	meta, ok, err := llmlifecycle.ReadMetadata(opts.LifecyclePaths, request.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("threadanalysis: cached metadata for thread %s is required", threadID)
	}
	return llmlifecycle.ValidateMetadata(meta, request, opts.Adapter.Name())
}

func lifecycleRequestForThread(opts Options, threadID string, thread threadcontext.Thread) (llmlifecycle.Request, error) {
	input := analysisInputForThread(threadID, thread)
	prompt, err := promptForInput(input)
	if err != nil {
		return llmlifecycle.Request{}, err
	}
	return llmlifecycle.Request{
		Store:           opts.Store,
		Adapter:         opts.Adapter,
		RunID:           opts.RunID,
		TaskID:          "thread-analysis-" + threadID,
		Phase:           string(stagemodel.StageThreadAnalysis),
		Paths:           opts.LifecyclePaths,
		Model:           opts.Model,
		Effort:          opts.Effort,
		LogPath:         opts.LogPath,
		Prompt:          prompt,
		Progress:        opts.Progress,
		Now:             opts.Now,
		NewSessionRowID: opts.NewStepID,
		ResumeSessionID: opts.ResumeSessionID,
	}, nil
}

// ResponseActions converts thread-analysis decisions into concrete thread
// responses that can be planned through reviewplan/ledger/outbox.
func ResponseActions(results []Result) []review.ThreadResponseAction {
	responses := make([]review.ThreadResponseAction, 0, len(results))
	for _, result := range results {
		response, ok := ResponseAction(result)
		if ok {
			responses = append(responses, response)
		}
	}
	return responses
}

// ResponseAction converts one thread-analysis decision into a concrete response.
func ResponseAction(result Result) (review.ThreadResponseAction, bool) {
	threadID := strings.TrimSpace(result.ThreadID)
	switch result.Decision {
	case DecisionSkip:
		return review.ThreadResponseAction{}, false
	case DecisionReplyOnly, DecisionClarify:
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseReply,
			ThreadID:  threadID,
			Body:      result.ReplyBody,
			Rationale: result.Rationale,
		}, true
	case DecisionAcknowledge, DecisionConcede:
		if strings.TrimSpace(result.Summary) == "" {
			return review.ThreadResponseAction{
				Kind:      review.ThreadResponseReply,
				ThreadID:  threadID,
				Body:      result.ReplyBody,
				Rationale: result.Rationale,
			}, true
		}
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseSummaryReply,
			ThreadID:  threadID,
			Body:      combineReplyAndSummary(result.ReplyBody, result.Summary),
			Resolve:   result.Resolve,
			Rationale: result.Rationale,
		}, true
	case DecisionSummarize:
		return review.ThreadResponseAction{
			Kind:      review.ThreadResponseSummaryReply,
			ThreadID:  threadID,
			Body:      result.Summary,
			Resolve:   true,
			Rationale: result.Rationale,
		}, true
	default:
		return review.ThreadResponseAction{}, false
	}
}

func combineReplyAndSummary(reply, summary string) string {
	reply = threadcontext.SanitizeBody(reply)
	summary = threadcontext.SanitizeBody(summary)
	if reply == "" {
		return summary
	}
	if summary == "" {
		return reply
	}
	return reply + "\n\nSummary:\n" + summary
}

func validateOptions(opts Options) error {
	if opts.Store == nil {
		return fmt.Errorf("threadanalysis: store is required")
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return fmt.Errorf("threadanalysis: run ID is required")
	}
	if strings.TrimSpace(opts.LifecyclePaths.LLMTasksDir) == "" {
		return fmt.Errorf("threadanalysis: llm task artifact directory is required")
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
