// Package approvaloverride classifies explicit author override requests.
package approvaloverride

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
)

const schemaVersion = 1

// Candidate is one PR-author-authored comment eligible for override classification.
type Candidate struct {
	ID          string
	Source      string
	Author      gitprovider.Identity
	Body        string
	URL         string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	EffectiveAt time.Time
}

// Request is the full classifier input after deterministic Go filtering.
type Request struct {
	PR              gitprovider.PR
	PostingIdentity gitprovider.Identity
	LatestMarkerAt  time.Time
	Candidates      []Candidate
	LogPath         string
	LLMTasksDir     string
	Now             func() time.Time
	NewSessionRowID func() string
}

// Result is the classifier's decision.
type Result struct {
	Approve bool
}

// Classifier decides whether filtered comments request approval override.
type Classifier interface {
	ClassifyApprovalOverride(context.Context, Request) (Result, error)
}

// LLMClassifier implements Classifier with structured output.
type LLMClassifier struct {
	adapter llm.Adapter
	model   string
	effort  string
}

// NewLLMClassifier builds an LLM-backed approval override classifier.
func NewLLMClassifier(adapter llm.Adapter, model, effort string) *LLMClassifier {
	return &LLMClassifier{
		adapter: adapter,
		model:   strings.TrimSpace(model),
		effort:  strings.TrimSpace(effort),
	}
}

// ClassifyApprovalOverride classifies explicit approval override requests.
func (c *LLMClassifier) ClassifyApprovalOverride(ctx context.Context, req Request) (Result, error) {
	if c == nil || c.adapter == nil {
		return Result{}, fmt.Errorf("approvaloverride: adapter is required")
	}
	if strings.TrimSpace(c.model) == "" {
		return Result{}, fmt.Errorf("approvaloverride: model is required")
	}
	if err := ensureLogDir(req.LogPath); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(req.LLMTasksDir) == "" {
		return Result{}, fmt.Errorf("approvaloverride: llm task artifact directory is required")
	}
	result, err := llmlifecycle.RunStructured(ctx, llmlifecycle.Request{
		Adapter:         c.adapter,
		TaskID:          "approval-override",
		Phase:           string(stagemodel.StageApprovalOverride),
		AllowNoRunCache: true,
		Paths:           llmlifecycle.Paths{LLMTasksDir: req.LLMTasksDir},
		Model:           c.model,
		Effort:          c.effort,
		LogPath:         req.LogPath,
		Prompt:          BuildPrompt(req),
		FailureStatus:   llmlifecycle.StatusFailedIsolated,
		Now:             requestClock(req),
		NewSessionRowID: requestSessionRowID(req),
	}, DecodeResponse)
	if err != nil {
		return Result{}, err
	}
	return Result{Approve: result.Value.ApprovalOverrideRequested}, nil
}

// Response is the strict classifier schema.
type Response struct {
	SchemaVersion             int  `json:"schema_version"`
	ApprovalOverrideRequested bool `json:"approval_override_requested"`
}

// DecodeResponse validates a classifier structured-output payload.
func DecodeResponse(data []byte) (Response, error) {
	var raw struct {
		SchemaVersion             *int  `json:"schema_version"`
		ApprovalOverrideRequested *bool `json:"approval_override_requested"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Response{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Response{}, fmt.Errorf("approvaloverride: trailing JSON tokens")
	}
	if raw.SchemaVersion == nil {
		return Response{}, fmt.Errorf("approvaloverride: schema_version is required")
	}
	if *raw.SchemaVersion != schemaVersion {
		return Response{}, fmt.Errorf("approvaloverride: schema_version = %d, want %d", *raw.SchemaVersion, schemaVersion)
	}
	if raw.ApprovalOverrideRequested == nil {
		return Response{}, fmt.Errorf("approvaloverride: approval_override_requested is required")
	}
	return Response{
		SchemaVersion:             *raw.SchemaVersion,
		ApprovalOverrideRequested: *raw.ApprovalOverrideRequested,
	}, nil
}

// BuildPrompt returns the deterministic prompt for one override classification.
func BuildPrompt(req Request) string {
	candidates := append([]Candidate(nil), req.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i].EffectiveAt
		right := candidates[j].EffectiveAt
		if left.Equal(right) {
			return candidates[i].ID < candidates[j].ID
		}
		return left.Before(right)
	})

	var b strings.Builder
	b.WriteString("You classify whether pull request author comments explicitly ask the codereview agent to approve the pull request without another review pass.\n")
	b.WriteString("Return JSON only with schema_version=1 and approval_override_requested boolean.\n")
	b.WriteString("Set approval_override_requested=true only for a clear request to approve, override, bypass, or accept low-value review comments. Do not infer approval from disagreement, thanks, or code-change descriptions.\n\n")
	b.WriteString("Pull request:\n")
	fmt.Fprintf(&b, "- title: %q\n", req.PR.Title)
	fmt.Fprintf(&b, "- url: %q\n", req.PR.URL)
	fmt.Fprintf(&b, "- author_login: %q\n", req.PR.Author.Login)
	fmt.Fprintf(&b, "- posting_identity_login: %q\n", req.PostingIdentity.Login)
	fmt.Fprintf(&b, "- latest_codereview_marker_at: %q\n\n", req.LatestMarkerAt.Format(time.RFC3339Nano))
	b.WriteString("Candidate comments, already filtered to PR author comments newer than the latest codereview marker:\n")
	for i, candidate := range candidates {
		fmt.Fprintf(&b, "\nCandidate %d:\n", i+1)
		fmt.Fprintf(&b, "id: %q\n", candidate.ID)
		fmt.Fprintf(&b, "source: %q\n", candidate.Source)
		fmt.Fprintf(&b, "effective_at: %q\n", candidate.EffectiveAt.Format(time.RFC3339Nano))
		fmt.Fprintf(&b, "url: %q\n", candidate.URL)
		b.WriteString("body:\n")
		b.WriteString(candidate.Body)
		if !strings.HasSuffix(candidate.Body, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nReturn exactly: {\"schema_version\":1,\"approval_override_requested\":true|false}\n")
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

func requestClock(req Request) func() time.Time {
	if req.Now != nil {
		return func() time.Time { return req.Now().UTC() }
	}
	return func() time.Time { return time.Now().UTC() }
}

func requestSessionRowID(req Request) func() string {
	if req.NewSessionRowID != nil {
		return req.NewSessionRowID
	}
	return uuid.NewString
}
