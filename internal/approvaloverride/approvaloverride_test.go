package approvaloverride

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestDecodeResponseStrictSchema(t *testing.T) {
	valid, err := DecodeResponse([]byte(`{"schema_version":1,"approval_override_requested":true}`))
	if err != nil {
		t.Fatalf("DecodeResponse valid: %v", err)
	}
	if !valid.ApprovalOverrideRequested {
		t.Fatal("ApprovalOverrideRequested = false, want true")
	}

	for _, raw := range []string{
		`{"schema_version":2,"approval_override_requested":true}`,
		`{"schema_version":1,"approval_override_requested":true,"why":"because"}`,
		`{"schema_version":1}`,
	} {
		if _, err := DecodeResponse([]byte(raw)); err == nil {
			t.Fatalf("DecodeResponse(%s) error = nil, want error", raw)
		}
	}
}

func TestBuildPromptIncludesFilteredCandidates(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	req := Request{
		PR: gitprovider.PR{
			Title:  "Fast path approval",
			URL:    "https://example.test/pr/1",
			Author: gitprovider.Identity{Login: "author"},
		},
		PostingIdentity: gitprovider.Identity{Login: "review-bot"},
		LatestMarkerAt:  now.Add(-time.Minute),
		Candidates: []Candidate{
			{ID: "late", Source: "issue_comment", Body: "please approve, these comments are low-value", EffectiveAt: now},
			{ID: "early", Source: "review", Body: "not an override", EffectiveAt: now.Add(-time.Second)},
		},
	}

	prompt := BuildPrompt(req)
	for _, want := range []string{
		"Return JSON only",
		"Fast path approval",
		"please approve, these comments are low-value",
		"Candidate 1:\nid: \"early\"",
		"Candidate 2:\nid: \"late\"",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestLLMClassifierReturnsBoolean(t *testing.T) {
	adapter := &llm.FakeAdapter{}
	adapter.Queue(llm.FakeResult{
		SessionID: "session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"schema_version":1,"approval_override_requested":true}`)},
	})
	classifier := NewLLMClassifier(adapter, "small-model", "low")

	result, err := classifier.ClassifyApprovalOverride(context.Background(), Request{
		PR: gitprovider.PR{
			Title:  "Fast path approval",
			URL:    "https://example.test/pr/1",
			Author: gitprovider.Identity{Login: "author"},
		},
		Candidates: []Candidate{{ID: "1", Source: "issue_comment", Body: "please approve", EffectiveAt: time.Now().UTC()}},
	})
	if err != nil {
		t.Fatalf("ClassifyApprovalOverride: %v", err)
	}
	if !result.Approve {
		t.Fatal("Approve = false, want true")
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want 1", len(requests))
	}
	if requests[0].Model != "small-model" || requests[0].Effort != "low" {
		t.Fatalf("request = %#v, want configured model/effort", requests[0])
	}
}
