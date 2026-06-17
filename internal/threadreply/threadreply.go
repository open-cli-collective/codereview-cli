// Package threadreply classifies how cr should respond to human replies on its
// own open review-comment threads and whether the thread is now addressed.
package threadreply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

const schemaVersion = 1

// Decision is the responder's chosen action for one review-comment thread.
type Decision string

// Decision values.
const (
	// DecisionSkip leaves the thread untouched.
	DecisionSkip Decision = "skip"
	// DecisionReplyOnly posts a contextual reply but keeps the thread open
	// (for example to answer a question or push back on a reply).
	DecisionReplyOnly Decision = "reply_only"
	// DecisionAcknowledgeAndResolve posts an acknowledgement reply and resolves
	// the thread because the finding has been addressed or conceded.
	DecisionAcknowledgeAndResolve Decision = "acknowledge_and_resolve"
)

// Valid reports whether d is one of the known decisions.
func (d Decision) Valid() bool {
	switch d {
	case DecisionSkip, DecisionReplyOnly, DecisionAcknowledgeAndResolve:
		return true
	default:
		return false
	}
}

// ParseDecision parses a responder decision.
func ParseDecision(value string) (Decision, error) {
	decision := Decision(strings.ToLower(strings.TrimSpace(value)))
	if !decision.Valid() {
		return "", fmt.Errorf("invalid thread reply decision %q", value)
	}
	return decision, nil
}

// Comment is one comment in a review-comment thread.
type Comment struct {
	Author    string
	Body      string
	FromCR    bool
	CreatedAt time.Time
}

// Request is the classifier input for one review-comment thread.
type Request struct {
	PR              gitprovider.PR
	PostingIdentity gitprovider.Identity
	Path            string
	Line            int
	OriginalFinding string
	Comments        []Comment
	LogPath         string
}

// Result is the responder's decision for one thread.
type Result struct {
	Decision Decision
	Reply    string
}

// Classifier decides how to respond to replies on a cr-authored thread.
type Classifier interface {
	ClassifyThreadReply(context.Context, Request) (Result, error)
}

// LLMClassifier implements Classifier with structured output.
type LLMClassifier struct {
	adapter llm.Adapter
	model   string
	effort  string
}

// NewLLMClassifier builds an LLM-backed thread-reply classifier.
func NewLLMClassifier(adapter llm.Adapter, model, effort string) *LLMClassifier {
	return &LLMClassifier{
		adapter: adapter,
		model:   strings.TrimSpace(model),
		effort:  strings.TrimSpace(effort),
	}
}

// ClassifyThreadReply decides how cr should respond to a thread's latest reply.
func (c *LLMClassifier) ClassifyThreadReply(ctx context.Context, req Request) (Result, error) {
	if c == nil || c.adapter == nil {
		return Result{}, fmt.Errorf("threadreply: adapter is required")
	}
	if strings.TrimSpace(c.model) == "" {
		return Result{}, fmt.Errorf("threadreply: model is required")
	}
	if err := ensureLogDir(req.LogPath); err != nil {
		return Result{}, err
	}
	value, _, err := llm.RunStructured(ctx, c.adapter, llm.Request{
		Model:   c.model,
		Effort:  c.effort,
		Prompt:  BuildPrompt(req),
		LogPath: req.LogPath,
	}, DecodeResponse)
	if err != nil {
		return Result{}, err
	}
	decision, err := ParseDecision(value.Decision)
	if err != nil {
		return Result{}, err
	}
	reply := strings.TrimSpace(value.Reply)
	if decision != DecisionSkip && reply == "" {
		return Result{}, fmt.Errorf("threadreply: decision %q requires a non-empty reply", decision)
	}
	if decision == DecisionSkip {
		reply = ""
	}
	return Result{Decision: decision, Reply: reply}, nil
}

// Response is the strict classifier schema.
type Response struct {
	SchemaVersion int    `json:"schema_version"`
	Decision      string `json:"decision"`
	Reply         string `json:"reply"`
}

// DecodeResponse validates a classifier structured-output payload.
func DecodeResponse(data []byte) (Response, error) {
	var raw struct {
		SchemaVersion *int    `json:"schema_version"`
		Decision      *string `json:"decision"`
		Reply         *string `json:"reply"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Response{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Response{}, fmt.Errorf("threadreply: trailing JSON tokens")
	}
	if raw.SchemaVersion == nil {
		return Response{}, fmt.Errorf("threadreply: schema_version is required")
	}
	if *raw.SchemaVersion != schemaVersion {
		return Response{}, fmt.Errorf("threadreply: schema_version = %d, want %d", *raw.SchemaVersion, schemaVersion)
	}
	if raw.Decision == nil {
		return Response{}, fmt.Errorf("threadreply: decision is required")
	}
	if _, err := ParseDecision(*raw.Decision); err != nil {
		return Response{}, err
	}
	reply := ""
	if raw.Reply != nil {
		reply = *raw.Reply
	}
	return Response{
		SchemaVersion: *raw.SchemaVersion,
		Decision:      *raw.Decision,
		Reply:         reply,
	}, nil
}

// BuildPrompt returns the deterministic prompt for one thread-reply classification.
func BuildPrompt(req Request) string {
	var b strings.Builder
	b.WriteString("You are a code-review assistant deciding how to respond to a human reply on a review-comment thread that you (the reviewer) opened.\n")
	b.WriteString("Read the original finding and the full thread, then choose exactly one decision:\n")
	b.WriteString("- \"acknowledge_and_resolve\": the reply shows the finding is fixed, already handled, or you concede the point; post a brief acknowledgement and the thread will be resolved.\n")
	b.WriteString("- \"reply_only\": the reply asks a question or pushes back and still needs a contextual answer; post a reply and keep the thread open.\n")
	b.WriteString("- \"skip\": no useful response is possible or the latest comment is your own; take no action.\n\n")
	b.WriteString("Return JSON only with schema_version=1, decision, and reply. For skip, reply must be empty. For the other decisions, reply must be a concise, courteous in-thread message.\n\n")
	b.WriteString("Pull request:\n")
	fmt.Fprintf(&b, "- title: %q\n", req.PR.Title)
	fmt.Fprintf(&b, "- url: %q\n", req.PR.URL)
	fmt.Fprintf(&b, "- reviewer_login: %q\n", req.PostingIdentity.Login)
	if strings.TrimSpace(req.Path) != "" {
		fmt.Fprintf(&b, "- file: %q\n", req.Path)
	}
	if req.Line > 0 {
		fmt.Fprintf(&b, "- line: %d\n", req.Line)
	}
	b.WriteString("\nOriginal finding:\n")
	b.WriteString(strings.TrimSpace(req.OriginalFinding))
	b.WriteString("\n\nThread (oldest first):\n")
	for i, comment := range req.Comments {
		speaker := comment.Author
		if comment.FromCR {
			speaker = comment.Author + " (reviewer)"
		}
		fmt.Fprintf(&b, "\nComment %d by %s:\n", i+1, speaker)
		b.WriteString(strings.TrimSpace(comment.Body))
		b.WriteByte('\n')
	}
	b.WriteString("\nReturn exactly: {\"schema_version\":1,\"decision\":\"...\",\"reply\":\"...\"}\n")
	return b.String()
}

func ensureLogDir(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o750)
}
