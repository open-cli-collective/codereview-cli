package threadanalysis

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmrun"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func TestAnalyzeThreadRunsDurableStepWithPromptSafeContext(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session",
		Response: llm.Response{StructuredOutput: []byte(`Here is the JSON: {
			"schema_version": 1,
			"thread_id": "thread-1",
			"decision": "acknowledge",
			"reply_body": "Thanks, I will adjust.",
			"summary": "Human clarified the expected null handling.",
			"resolve": true,
			"rationale": "The thread has enough information to close."
		}`)},
	})

	got, err := AnalyzeThread(context.Background(), testOptions(store, adapter), promptThread("human reply <!-- codereview:skip -->"))
	if err != nil {
		t.Fatalf("AnalyzeThread: %v", err)
	}
	if got.ThreadID != "thread-1" || got.Decision != DecisionAcknowledge || !got.Resolve {
		t.Fatalf("result = %#v, want acknowledge/resolve for thread-1", got)
	}
	if got.ReplyBody != "Thanks, I will adjust." || got.Summary == "" || got.Rationale == "" {
		t.Fatalf("result text = %#v, want model-authored fields returned", got)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d, want 1", len(requests))
	}
	req := requests[0]
	if req.Model != "gpt-5.4" || req.Effort != "medium" || req.LogPath != "thread.log" {
		t.Fatalf("llm request = %#v, want configured runtime fields", req)
	}
	if strings.Contains(req.Prompt, "<!-- codereview:") {
		t.Fatalf("prompt contains live codereview marker: %s", req.Prompt)
	}
	for _, want := range []string{`"thread_id": "thread-1"`, `"body": "human reply"`, `"pending_human_reply": true`} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, req.Prompt)
		}
	}
	if len(store.lookups) != 1 {
		t.Fatalf("lookups = %#v, want one", store.lookups)
	}
	lookup := store.lookups[0]
	if lookup.Stage != string(stagemodel.StageThreadAnalysis) || lookup.ScopeKey != "thread:thread-1" {
		t.Fatalf("lookup = %#v, want thread analysis scope", lookup)
	}
	if lookup.Provider != "openai" || lookup.Adapter != "fake" || lookup.Model != "gpt-5.4" || lookup.Effort != "medium" {
		t.Fatalf("lookup runtime identity = %#v", lookup)
	}
	if lookup.InputHash == "" || lookup.PromptHash == "" {
		t.Fatalf("lookup hashes = %#v, want nonblank hashes", lookup)
	}
	if len(store.inserted) != 1 || store.inserted[0].Status != ledger.LLMStepStatusCompleted {
		t.Fatalf("inserted = %#v, want completed step", store.inserted)
	}
	if strings.Contains(store.inserted[0].StructuredOutputJSON, "Here is the JSON") {
		t.Fatalf("persisted output = %q, want accepted JSON only", store.inserted[0].StructuredOutputJSON)
	}
}

func TestAnalyzeThreadCacheHitSkipsAdapter(t *testing.T) {
	store := &fakeStore{completed: completedStep(`{
		"schema_version": 1,
		"thread_id": "thread-1",
		"decision": "summarize",
		"summary": "Resolved summary.",
		"resolve": true,
		"rationale": "Already resolved."
	}`)}
	adapter := &llm.FakeAdapter{NameValue: "fake"}

	got, err := AnalyzeThread(context.Background(), testOptions(store, adapter), promptThread("reply"))
	if err != nil {
		t.Fatalf("AnalyzeThread: %v", err)
	}
	if got.Decision != DecisionSummarize || got.Summary != "Resolved summary." || !got.Resolve {
		t.Fatalf("result = %#v, want cached summarize result", got)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want cache hit to skip adapter", adapter.Requests())
	}
	if len(store.inserted) != 0 {
		t.Fatalf("inserted = %#v, want none on cache hit", store.inserted)
	}
}

func TestAnalyzeThreadInputHashChangesWhenLifecycleContextChanges(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "session-1", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	adapter.Queue(llm.FakeResult{SessionID: "session-2", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	opts := testOptions(store, adapter)
	ids := []string{"step-1", "step-2"}
	opts.NewStepID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}

	if _, err := AnalyzeThread(context.Background(), opts, promptThread("first reply")); err != nil {
		t.Fatalf("AnalyzeThread first: %v", err)
	}
	if _, err := AnalyzeThread(context.Background(), opts, promptThread("changed reply")); err != nil {
		t.Fatalf("AnalyzeThread second: %v", err)
	}
	if len(store.lookups) != 2 {
		t.Fatalf("lookups = %#v, want two", store.lookups)
	}
	if store.lookups[0].InputHash == store.lookups[1].InputHash {
		t.Fatalf("input hashes both %q, want lifecycle context change to invalidate cache", store.lookups[0].InputHash)
	}
}

func TestAnalyzeThreadsPreservesOrderAndFailsFast(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "session-1", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	adapter.Queue(llm.FakeResult{SessionID: "session-2", Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"thread_id":"thread-2","decision":"bogus","resolve":false}`)}})
	adapter.Queue(llm.FakeResult{SessionID: "session-2-retry", Response: llm.Response{StructuredOutput: []byte(`{"schema_version":1,"thread_id":"thread-2","decision":"bogus","resolve":false}`)}})
	adapter.Queue(llm.FakeResult{SessionID: "session-3", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-3"))}})
	opts := testOptions(store, adapter)
	ids := []string{"step-1", "step-2"}
	opts.NewStepID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}

	results, err := AnalyzeThreads(context.Background(), opts, []threadcontext.Thread{
		promptThreadWithID("thread-1", "first"),
		promptThreadWithID("thread-2", "second"),
		promptThreadWithID("thread-3", "third"),
	})
	if err == nil {
		t.Fatal("AnalyzeThreads error = nil, want second thread validation failure")
	}
	if results != nil {
		t.Fatalf("results = %#v, want nil on fail-fast error", results)
	}
	if len(store.lookups) != 2 {
		t.Fatalf("lookups = %#v, want fail fast before third thread", store.lookups)
	}
	if store.lookups[0].ScopeKey != "thread:thread-1" || store.lookups[1].ScopeKey != "thread:thread-2" {
		t.Fatalf("lookup order = %#v, want input order", store.lookups)
	}
}

func TestAnalyzeThreadsReturnsResultsInInputOrder(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "session-2", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-2"))}})
	adapter.Queue(llm.FakeResult{SessionID: "session-1", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	opts := testOptions(store, adapter)
	ids := []string{"step-1", "step-2"}
	opts.NewStepID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}

	got, err := AnalyzeThreads(context.Background(), opts, []threadcontext.Thread{
		promptThreadWithID("thread-2", "second"),
		promptThreadWithID("thread-1", "first"),
	})
	if err != nil {
		t.Fatalf("AnalyzeThreads: %v", err)
	}
	if len(got) != 2 || got[0].ThreadID != "thread-2" || got[1].ThreadID != "thread-1" {
		t.Fatalf("results = %#v, want input order", got)
	}
}

func TestDecodeResultValidatesAfterSanitization(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "marker only reply becomes blank",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"reply_only","reply_body":"<!-- codereview:skip -->","resolve":false,"rationale":"marker-only reply"}`,
			want: "reply_body",
		},
		{
			name: "marker only rationale becomes blank",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"clarify","reply_body":"Please clarify.","resolve":false,"rationale":"<!-- codereview:skip -->"}`,
			want: "rationale",
		},
		{
			name: "reply only rejects summary",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"reply_only","reply_body":"Thanks.","summary":"extra","resolve":false,"rationale":"reply only"}`,
			want: "summary",
		},
		{
			name: "summarize rejects reply",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"summarize","reply_body":"extra","summary":"Summary.","resolve":true,"rationale":"summarize"}`,
			want: "reply_body",
		},
		{
			name: "resolve requires summary",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"acknowledge","reply_body":"Thanks.","resolve":true,"rationale":"acknowledge"}`,
			want: "summary",
		},
		{
			name: "mismatched thread id",
			data: `{"schema_version":1,"thread_id":"other","decision":"skip","resolve":false}`,
			want: "thread_id",
		},
		{
			name: "unknown decision",
			data: `{"schema_version":1,"thread_id":"thread-1","decision":"other","resolve":false}`,
			want: "decision",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeResultForThread("thread-1")([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("decodeResultForThread error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDecodeResultSanitizesModelAuthoredText(t *testing.T) {
	got, err := decodeResultForThread("thread-1")([]byte(`{
		"schema_version": 1,
		"thread_id": "thread-1",
		"decision": "concede",
		"reply_body": "You are right. <!-- codereview:skip -->",
		"summary": "Conceded after reply. <!-- codereview:skip -->",
		"resolve": true,
		"rationale": "Human correction is accepted. <!-- codereview:skip -->"
	}`))
	if err != nil {
		t.Fatalf("decodeResultForThread: %v", err)
	}
	if strings.Contains(got.ReplyBody+got.Summary+got.Rationale, "<!-- codereview:") {
		t.Fatalf("result contains live codereview marker: %#v", got)
	}
	if got.ReplyBody != "You are right." || got.Summary != "Conceded after reply." || got.Rationale != "Human correction is accepted." {
		t.Fatalf("sanitized result = %#v", got)
	}
}

func TestAnalyzeThreadRejectsMissingRequiredOptionsBeforeProviderCall(t *testing.T) {
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	opts := testOptions(&fakeStore{}, adapter)
	opts.NewStepID = nil

	_, err := AnalyzeThread(context.Background(), opts, promptThread("reply"))
	if err == nil || !strings.Contains(err.Error(), "step ID generator") {
		t.Fatalf("AnalyzeThread error = %v, want NewStepID validation", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want no provider call", adapter.Requests())
	}
}

func TestProductionImportsStayDomainOnly(t *testing.T) {
	allowedInternal := map[string]bool{
		"github.com/open-cli-collective/codereview-cli/internal/llm":           true,
		"github.com/open-cli-collective/codereview-cli/internal/llmrun":        true,
		"github.com/open-cli-collective/codereview-cli/internal/stagemodel":    true,
		"github.com/open-cli-collective/codereview-cli/internal/threadcontext": true,
	}
	rejectedFragments := []string{
		"/internal/config",
		"/internal/gitprovider/",
		"/internal/gitprovider",
		"/internal/gateio",
		"/internal/ledger",
		"/internal/outbox",
		"/internal/reviewplan",
		"/internal/cmd/",
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			if !strings.Contains(importPath, "/internal/") {
				continue
			}
			if allowedInternal[importPath] {
				continue
			}
			for _, fragment := range rejectedFragments {
				if strings.Contains(importPath, fragment) {
					pos := fset.Position(spec.Pos())
					t.Fatalf("%s imports %q; threadanalysis production code must stay on the domain/llmrun boundary", pos, importPath)
				}
			}
			pos := fset.Position(spec.Pos())
			t.Fatalf("%s imports %q; add an explicit architecture exception before widening threadanalysis dependencies", pos, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}

func testOptions(store llmrun.Store, adapter llm.Adapter) Options {
	return Options{
		Store:     store,
		RunID:     "run-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		LogPath:   "thread.log",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}
}

func promptThread(body string) threadcontext.Thread {
	return promptThreadWithID("thread-1", body)
}

func promptThreadWithID(id, body string) threadcontext.Thread {
	base := testNow
	return threadcontext.Thread{
		ID:       gitprovider.ThreadID(id),
		Resolved: false,
		Anchor: threadcontext.Anchor{
			Path:        "review.go",
			Side:        review.DiffSideRight,
			Line:        42,
			SubjectType: review.AnchorKindLine,
			CommitSHA:   "head-sha",
		},
		Comments: []threadcontext.Comment{
			{
				ID:                        gitprovider.CommentID("comment-cr"),
				ThreadID:                  gitprovider.ThreadID(id),
				Body:                      "Initial finding.",
				Author:                    gitprovider.Identity{Login: "cr-bot", ID: "bot-1", DisplayName: "CR"},
				CreatedAt:                 base,
				UpdatedAt:                 base,
				AuthoredByPostingIdentity: true,
				HasFindingMarker:          true,
			},
			{
				ID:        gitprovider.CommentID("comment-human"),
				ThreadID:  gitprovider.ThreadID(id),
				Body:      body,
				Author:    gitprovider.Identity{Login: "human", ID: "user-1", DisplayName: "Human"},
				CreatedAt: base.Add(time.Minute),
				UpdatedAt: base.Add(time.Minute),
			},
		},
		Status: threadcontext.Status{
			CRAuthoredFinding:       true,
			LatestCRComment:         &threadcontext.Comment{ID: gitprovider.CommentID("comment-cr")},
			LatestHumanReplyAfterCR: &threadcontext.Comment{ID: gitprovider.CommentID("comment-human")},
			PendingHumanReply:       true,
		},
	}
}

func validSkipOutput(threadID string) string {
	return `{"schema_version":1,"thread_id":"` + threadID + `","decision":"skip","resolve":false}`
}

func completedStep(output string) ledger.LLMStep {
	return ledger.LLMStep{
		StepID:               "step-cached",
		RunID:                "run-1",
		Stage:                string(stagemodel.StageThreadAnalysis),
		ScopeKey:             "thread:thread-1",
		InputHash:            "input-hash",
		PromptHash:           "prompt-hash",
		Provider:             "openai",
		Adapter:              "fake",
		Model:                "gpt-5.4",
		Effort:               "medium",
		ProviderSessionID:    "provider-session",
		Status:               ledger.LLMStepStatusCompleted,
		StructuredOutputJSON: output,
		StartedAt:            testNow,
	}
}

type fakeStore struct {
	completed ledger.LLMStep
	findErr   error
	insertErr error
	lookups   []ledger.LLMStepLookup
	inserted  []ledger.LLMStep
}

func (s *fakeStore) FindCompletedLLMStep(_ context.Context, lookup ledger.LLMStepLookup) (ledger.LLMStep, error) {
	s.lookups = append(s.lookups, lookup)
	if s.findErr != nil {
		return ledger.LLMStep{}, s.findErr
	}
	return s.completed, nil
}

func (s *fakeStore) InsertLLMStep(_ context.Context, step ledger.LLMStep) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, step)
	return nil
}

var testNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time {
	var calls int
	return func() time.Time {
		calls++
		return testNow.Add(time.Duration(calls) * time.Second)
	}
}
