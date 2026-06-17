package threadreply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestDecodeResponseStrictSchema(t *testing.T) {
	valid, err := DecodeResponse([]byte(`{"schema_version":1,"decision":"reply_only","reply":"thanks"}`))
	if err != nil {
		t.Fatalf("DecodeResponse valid: %v", err)
	}
	if valid.Decision != "reply_only" || valid.Reply != "thanks" {
		t.Fatalf("DecodeResponse = %#v, want reply_only/thanks", valid)
	}

	for _, raw := range []string{
		`{"schema_version":2,"decision":"skip","reply":""}`,
		`{"schema_version":1,"decision":"skip","reply":"","why":"because"}`,
		`{"schema_version":1,"reply":"hi"}`,
		`{"schema_version":1,"decision":"bogus","reply":"hi"}`,
		`{"decision":"skip","reply":""}`,
	} {
		if _, err := DecodeResponse([]byte(raw)); err == nil {
			t.Fatalf("DecodeResponse(%s) error = nil, want error", raw)
		}
	}
}

func TestParseDecision(t *testing.T) {
	for _, value := range []string{"skip", "reply_only", "ACKNOWLEDGE_AND_RESOLVE", " reply_only "} {
		if _, err := ParseDecision(value); err != nil {
			t.Fatalf("ParseDecision(%q): %v", value, err)
		}
	}
	if _, err := ParseDecision("resolve"); err == nil {
		t.Fatal("ParseDecision(resolve) error = nil, want error")
	}
}

func TestBuildPromptIncludesThreadContext(t *testing.T) {
	req := Request{
		PR: gitprovider.PR{
			Title:  "Add retries",
			URL:    "https://example.test/pr/7",
			Author: gitprovider.Identity{Login: "author"},
		},
		PostingIdentity: gitprovider.Identity{Login: "review-bot"},
		Path:            "internal/api/client.go",
		Line:            42,
		OriginalFinding: "This nil check is missing.",
		Comments: []Comment{
			{Author: "review-bot", Body: "This nil check is missing.", FromCR: true},
			{Author: "author", Body: "Fixed in latest commit."},
		},
	}
	prompt := BuildPrompt(req)
	for _, want := range []string{
		"Return JSON only",
		"Add retries",
		"internal/api/client.go",
		"line: 42",
		"This nil check is missing.",
		"Comment 1 by review-bot (reviewer)",
		"Comment 2 by author",
		"Fixed in latest commit.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestLLMClassifierReturnsDecisionAndReply(t *testing.T) {
	adapter := &llm.FakeAdapter{}
	adapter.Queue(llm.FakeResult{
		SessionID: "session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"schema_version":1,"decision":"acknowledge_and_resolve","reply":"Thanks, resolving."}`)},
	})
	classifier := NewLLMClassifier(adapter, "small-model", "low")

	result, err := classifier.ClassifyThreadReply(context.Background(), Request{
		PR:       gitprovider.PR{Title: "t", URL: "u"},
		Comments: []Comment{{Author: "author", Body: "done"}},
	})
	if err != nil {
		t.Fatalf("ClassifyThreadReply: %v", err)
	}
	if result.Decision != DecisionAcknowledgeAndResolve || result.Reply != "Thanks, resolving." {
		t.Fatalf("result = %#v, want acknowledge_and_resolve with reply", result)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want 1", len(requests))
	}
	if requests[0].Model != "small-model" || requests[0].Effort != "low" {
		t.Fatalf("request = %#v, want configured model/effort", requests[0])
	}
}

func TestLLMClassifierSkipClearsReply(t *testing.T) {
	adapter := &llm.FakeAdapter{}
	adapter.Queue(llm.FakeResult{
		Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"decision":"skip","reply":""}`)},
	})
	classifier := NewLLMClassifier(adapter, "small-model", "low")
	result, err := classifier.ClassifyThreadReply(context.Background(), Request{Comments: []Comment{{Author: "a", Body: "b"}}})
	if err != nil {
		t.Fatalf("ClassifyThreadReply: %v", err)
	}
	if result.Decision != DecisionSkip || result.Reply != "" {
		t.Fatalf("result = %#v, want skip with empty reply", result)
	}
}

func TestLLMClassifierRejectsEmptyReplyForAction(t *testing.T) {
	adapter := &llm.FakeAdapter{}
	adapter.Queue(llm.FakeResult{
		Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"decision":"reply_only","reply":"   "}`)},
	})
	adapter.Queue(llm.FakeResult{
		Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"decision":"reply_only","reply":"   "}`)},
	})
	classifier := NewLLMClassifier(adapter, "small-model", "low")
	if _, err := classifier.ClassifyThreadReply(context.Background(), Request{Comments: []Comment{{Author: "a", Body: "b"}}}); err == nil {
		t.Fatal("ClassifyThreadReply error = nil, want error for empty reply")
	}
}

func markerComment(login, body string) gitprovider.ThreadComment {
	return gitprovider.ThreadComment{
		Author:    gitprovider.Identity{Login: login},
		Body:      "<!-- codereview:run-id=r1:action=a1:kind=inline_comment:sha=abc123:base=def456 -->\n" + body,
		CreatedAt: time.Now().UTC(),
	}
}

func plainComment(login, body string) gitprovider.ThreadComment {
	return gitprovider.ThreadComment{
		Author:    gitprovider.Identity{Login: login},
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
}

func TestSelectCandidatesPicksReplyOnCRThread(t *testing.T) {
	pr := gitprovider.PR{Title: "t", URL: "u"}
	posting := gitprovider.Identity{Login: "review-bot"}
	threads := []gitprovider.InlineThread{
		{
			ID:       "open-with-reply",
			Resolved: false,
			Path:     "a.go",
			Line:     10,
			Comments: []gitprovider.ThreadComment{
				markerComment("review-bot", "Nil check missing."),
				plainComment("author", "Fixed it."),
			},
		},
		{
			ID:       "already-resolved",
			Resolved: true,
			Comments: []gitprovider.ThreadComment{
				markerComment("review-bot", "Finding."),
				plainComment("author", "done"),
			},
		},
		{
			ID:       "no-human-reply",
			Resolved: false,
			Comments: []gitprovider.ThreadComment{
				markerComment("review-bot", "Finding only."),
			},
		},
		{
			ID:       "cr-had-last-word",
			Resolved: false,
			Comments: []gitprovider.ThreadComment{
				markerComment("review-bot", "Finding."),
				plainComment("author", "why?"),
				plainComment("review-bot", "Because X."),
			},
		},
		{
			ID:       "human-thread",
			Resolved: false,
			Comments: []gitprovider.ThreadComment{
				plainComment("author", "Question from a human."),
				plainComment("other", "answer"),
			},
		},
	}

	candidates := SelectCandidates(pr, posting, threads)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 (%+v)", len(candidates), candidateIDs(candidates))
	}
	got := candidates[0]
	if got.Thread.ID != "open-with-reply" {
		t.Fatalf("candidate thread = %q, want open-with-reply", got.Thread.ID)
	}
	if got.Request.Path != "a.go" || got.Request.Line != 10 {
		t.Fatalf("request anchor = %q:%d, want a.go:10", got.Request.Path, got.Request.Line)
	}
	if got.Request.OriginalFinding != "Nil check missing." {
		t.Fatalf("original finding = %q, want marker stripped", got.Request.OriginalFinding)
	}
	if len(got.Request.Comments) != 2 || !got.Request.Comments[0].FromCR || got.Request.Comments[1].FromCR {
		t.Fatalf("comments = %+v, want first from cr and second from human", got.Request.Comments)
	}
}

func candidateIDs(candidates []Candidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, string(c.Thread.ID))
	}
	return ids
}

func TestSelectCandidatesIgnoresSameLoginWithoutMarker(t *testing.T) {
	pr := gitprovider.PR{Title: "t", URL: "u"}
	posting := gitprovider.Identity{Login: "shared-account"}
	threads := []gitprovider.InlineThread{
		{
			ID:       "human-from-shared-account",
			Resolved: false,
			Comments: []gitprovider.ThreadComment{
				plainComment("shared-account", "A human comment, no marker."),
				plainComment("other", "reply"),
			},
		},
	}
	if got := SelectCandidates(pr, posting, threads); len(got) != 0 {
		t.Fatalf("candidates = %d, want 0 for unmarked thread", len(got))
	}
}

func TestStripMarkersRemovesAllMarkers(t *testing.T) {
	body := "<!-- codereview:run-id=r1:action=a1:kind=inline_comment:sha=abc:base=def -->\nReal text here."
	if got := stripMarkers(body); got != "Real text here." {
		t.Fatalf("stripMarkers = %q, want %q", got, "Real text here.")
	}
}
