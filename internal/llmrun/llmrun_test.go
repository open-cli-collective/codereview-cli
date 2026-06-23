package llmrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
)

func TestRunStructuredStepReturnsCachedCompletedStep(t *testing.T) {
	store := &fakeStore{completed: ledger.LLMStep{
		StepID:               "step-cached",
		RunID:                "run-1",
		Stage:                string(stagemodel.StageThreadAnalysis),
		ScopeKey:             "thread:thread-1",
		InputHash:            "input-hash",
		PromptHash:           HashPrompt("prompt"),
		Provider:             "openai",
		Adapter:              "fake",
		Model:                "gpt-5.4",
		Effort:               "medium",
		ProviderSessionID:    "provider-session",
		Status:               ledger.LLMStepStatusCompleted,
		StructuredOutputJSON: `{"ok":true}`,
		StartedAt:            testNow,
	}}
	adapter := &llm.FakeAdapter{NameValue: "fake"}

	got, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		InputHash: "input-hash",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-new" },
	}, decodeOK)
	if err != nil {
		t.Fatalf("RunStructuredStep: %v", err)
	}
	if !got.Cached || !got.Value.OK || got.Step.StepID != "step-cached" {
		t.Fatalf("result = %#v, want cached completed step", got)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want cache hit to skip adapter", adapter.Requests())
	}
	if len(store.inserted) != 0 {
		t.Fatalf("inserted steps = %#v, want none on cache hit", store.inserted)
	}
}

func TestRunStructuredStepCachedDecodeFailureFailsClosed(t *testing.T) {
	store := &fakeStore{completed: ledger.LLMStep{
		StepID:               "step-corrupt",
		RunID:                "run-1",
		Stage:                string(stagemodel.StageThreadAnalysis),
		ScopeKey:             "thread:thread-1",
		InputHash:            HashPrompt("prompt"),
		PromptHash:           HashPrompt("prompt"),
		Provider:             "openai",
		Adapter:              "fake",
		Model:                "gpt-5.4",
		Effort:               "medium",
		ProviderSessionID:    "provider-session",
		Status:               ledger.LLMStepStatusCompleted,
		StructuredOutputJSON: `not json`,
		StartedAt:            testNow,
	}}
	adapter := &llm.FakeAdapter{NameValue: "fake"}

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-new" },
	}, decodeOK)
	if err == nil || !strings.Contains(err.Error(), "cached") {
		t.Fatalf("RunStructuredStep error = %v, want cached decode error", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want corrupt cache to fail closed", adapter.Requests())
	}
}

func TestRunStructuredStepRejectsBlankAdapterName(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := blankNameAdapter{Adapter: &llm.FakeAdapter{}}

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, decodeOK)
	if err == nil || !strings.Contains(err.Error(), "adapter name") {
		t.Fatalf("RunStructuredStep error = %v, want adapter name error", err)
	}
	if len(store.lookups) != 0 {
		t.Fatalf("lookups = %#v, want validation before lookup", store.lookups)
	}
}

func TestRunStructuredStepMissPersistsAcceptedOutputBeforeReturning(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session",
		Response: llm.Response{
			StructuredOutput: []byte(`Here is the JSON: {"ok":true}`),
			Usage: llm.Usage{
				TokensIn:  intPtr(10),
				TokensOut: intPtr(2),
				CostUSD:   floatPtr(0.25),
			},
			DurationMS: 1200,
		},
	})

	got, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, decodeOK)
	if err != nil {
		t.Fatalf("RunStructuredStep: %v", err)
	}
	if got.Cached || !got.Value.OK {
		t.Fatalf("result = %#v, want uncached ok value", got)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted steps = %#v, want one", store.inserted)
	}
	if len(store.lookups) != 1 {
		t.Fatalf("lookups = %#v, want one", store.lookups)
	}
	lookup := store.lookups[0]
	if lookup.Provider != "openai" || lookup.Adapter != "fake" || lookup.Model != "gpt-5.4" || lookup.Effort != "medium" {
		t.Fatalf("lookup runtime identity = %#v", lookup)
	}
	step := store.inserted[0]
	if step.Status != ledger.LLMStepStatusCompleted || step.StructuredOutputJSON != `{"ok":true}` {
		t.Fatalf("persisted step = %#v, want completed accepted JSON", step)
	}
	if step.Provider != "openai" || step.Adapter != "fake" || step.ProviderSessionID != "provider-session" {
		t.Fatalf("persisted runtime metadata = %#v", step)
	}
}

func TestRunStructuredStepPersistenceFailureAfterSuccessReturnsError(t *testing.T) {
	insertErr := errors.New("store down")
	store := &fakeStore{findErr: ledger.ErrNotFound, insertErr: insertErr}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "provider-session", Response: llm.Response{StructuredOutput: []byte(`{"ok":true}`)}})

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, decodeOK)
	if !errors.Is(err, insertErr) {
		t.Fatalf("RunStructuredStep error = %v, want insert error", err)
	}
}

func TestRunStructuredStepRejectsBlankAcceptedOutputOnSuccess(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "provider-session", Response: llm.Response{StructuredOutput: nil}})

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, func([]byte) (okResponse, error) {
		return okResponse{OK: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "accepted structured output") {
		t.Fatalf("RunStructuredStep error = %v, want accepted output invariant error", err)
	}
	if len(store.inserted) != 0 {
		t.Fatalf("inserted steps = %#v, want no completed row with blank output", store.inserted)
	}
}

func TestRunStructuredStepPersistsFailedStep(t *testing.T) {
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{SessionID: "provider-session-1", Response: llm.Response{StructuredOutput: []byte(`bad1`)}})
	adapter.Queue(llm.FakeResult{SessionID: "provider-session-2", Response: llm.Response{StructuredOutput: []byte(`bad2`)}})

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, decodeOK)
	if err == nil {
		t.Fatal("RunStructuredStep error = nil, want decode failure")
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted steps = %#v, want failed step", store.inserted)
	}
	step := store.inserted[0]
	if step.Status != ledger.LLMStepStatusFailed {
		t.Fatalf("step.Status = %q, want failed", step.Status)
	}
	if step.Error == nil || *step.Error == "" {
		t.Fatalf("step.Error = %#v, want error text", step.Error)
	}
}

func TestRunStructuredStepPersistsProviderStartFailure(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	store := &fakeStore{findErr: ledger.ErrNotFound}
	adapter := &llm.FakeAdapter{NameValue: "fake"}
	adapter.Queue(llm.FakeResult{StartErr: providerErr})

	_, err := RunStructuredStep(context.Background(), store, Request{
		RunID:     "run-1",
		Stage:     stagemodel.StageThreadAnalysis,
		ScopeKey:  "thread:thread-1",
		Provider:  "openai",
		Adapter:   adapter,
		Model:     "gpt-5.4",
		Effort:    "medium",
		Prompt:    "prompt",
		Now:       fixedClock(),
		NewStepID: func() string { return "step-1" },
	}, decodeOK)
	if !errors.Is(err, providerErr) {
		t.Fatalf("RunStructuredStep error = %v, want provider error", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted steps = %#v, want failed step", store.inserted)
	}
	step := store.inserted[0]
	if step.Status != ledger.LLMStepStatusFailed || step.ProviderSessionID != "step-1" {
		t.Fatalf("failed step = %#v, want failed with step ID fallback session", step)
	}
}

type okResponse struct {
	OK bool `json:"ok"`
}

func decodeOK(data []byte) (okResponse, error) {
	var out okResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return okResponse{}, err
	}
	if !out.OK {
		return okResponse{}, errors.New("ok must be true")
	}
	return out, nil
}

type fakeStore struct {
	completed ledger.LLMStep
	findErr   error
	insertErr error
	lookups   []ledger.LLMStepLookup
	inserted  []ledger.LLMStep
}

type blankNameAdapter struct {
	llm.Adapter
}

func (blankNameAdapter) Name() string { return "" }

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

func intPtr(value int) *int {
	return &value
}

func floatPtr(value float64) *float64 {
	return &value
}
