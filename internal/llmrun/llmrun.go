// Package llmrun provides durable execution for structured LLM steps.
package llmrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
)

// Store is the durable step storage used by RunStructuredStep.
type Store interface {
	FindCompletedLLMStep(context.Context, ledger.LLMStepLookup) (ledger.LLMStep, error)
	InsertLLMStep(context.Context, ledger.LLMStep) error
}

// Request describes one durable structured LLM step.
type Request struct {
	RunID           string
	Stage           stagemodel.Stage
	ScopeKey        string
	InputHash       string
	Provider        string
	Adapter         llm.Adapter
	Model           string
	Effort          string
	Prompt          string
	LogPath         string
	ResumeSessionID string
	Now             func() time.Time
	NewStepID       func() string
}

// Result is the durable structured LLM step result.
type Result[T any] struct {
	Value  T
	Step   ledger.LLMStep
	Cached bool
}

// HashPrompt returns the SHA-256 hash used for prompt identity.
func HashPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// RunStructuredStep runs or reuses one durable structured LLM step.
func RunStructuredStep[T any](ctx context.Context, store Store, req Request, decode llm.Decoder[T]) (Result[T], error) {
	var zero Result[T]
	if err := validateRequest(store, req, decode); err != nil {
		return zero, err
	}
	now := req.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	promptHash := HashPrompt(req.Prompt)
	inputHash := strings.TrimSpace(req.InputHash)
	if inputHash == "" {
		inputHash = promptHash
	}
	adapterName := strings.TrimSpace(req.Adapter.Name())
	if adapterName == "" {
		return zero, fmt.Errorf("llmrun: adapter name is required")
	}
	lookup := ledger.LLMStepLookup{
		RunID:      req.RunID,
		Stage:      string(req.Stage),
		ScopeKey:   req.ScopeKey,
		InputHash:  inputHash,
		PromptHash: promptHash,
		Provider:   req.Provider,
		Adapter:    adapterName,
		Model:      req.Model,
		Effort:     req.Effort,
	}
	cached, err := store.FindCompletedLLMStep(ctx, lookup)
	if err == nil {
		value, decodeErr := decode([]byte(cached.StructuredOutputJSON))
		if decodeErr != nil {
			return zero, fmt.Errorf("llmrun: decode cached step %s: %w", cached.StepID, decodeErr)
		}
		return Result[T]{Value: value, Step: cached, Cached: true}, nil
	}
	if !errors.Is(err, ledger.ErrNotFound) {
		return zero, fmt.Errorf("llmrun: find completed step: %w", err)
	}

	stepID := strings.TrimSpace(req.NewStepID())
	if stepID == "" {
		return zero, fmt.Errorf("llmrun: step ID generator returned blank ID")
	}
	started := now()
	structured, runErr := llm.RunStructuredWithSessionResume(ctx, req.Adapter, req.ResumeSessionID, llm.Request{
		Model:   req.Model,
		Effort:  req.Effort,
		Prompt:  req.Prompt,
		LogPath: req.LogPath,
	}, decode)
	completed := now()
	step := stepFromResult(req, adapterName, stepID, inputHash, promptHash, started, completed, structured)
	if runErr != nil {
		step.Status = ledger.LLMStepStatusFailed
		message := runErr.Error()
		step.Error = &message
		if insertErr := store.InsertLLMStep(ctx, step); insertErr != nil {
			return zero, fmt.Errorf("llmrun: persist failed step: %w; original error: %v", insertErr, runErr)
		}
		return zero, runErr
	}

	step.Status = ledger.LLMStepStatusCompleted
	step.StructuredOutputJSON = string(structured.AcceptedOutput)
	if strings.TrimSpace(step.StructuredOutputJSON) == "" {
		return zero, fmt.Errorf("llmrun: accepted structured output is empty")
	}
	if insertErr := store.InsertLLMStep(ctx, step); insertErr != nil {
		return zero, fmt.Errorf("llmrun: persist completed step: %w", insertErr)
	}
	return Result[T]{Value: structured.Value, Step: step}, nil
}

func validateRequest[T any](store Store, req Request, decode llm.Decoder[T]) error {
	if store == nil {
		return fmt.Errorf("llmrun: store is required")
	}
	if req.Adapter == nil {
		return fmt.Errorf("llmrun: adapter is required")
	}
	if decode == nil {
		return fmt.Errorf("llmrun: decoder is required")
	}
	for field, value := range map[string]string{
		"run_id":    req.RunID,
		"stage":     string(req.Stage),
		"scope_key": req.ScopeKey,
		"provider":  req.Provider,
		"model":     req.Model,
		"prompt":    req.Prompt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("llmrun: %s is required", field)
		}
	}
	if !req.Stage.Valid() {
		return fmt.Errorf("llmrun: stage %q is invalid", req.Stage)
	}
	if req.NewStepID == nil {
		return fmt.Errorf("llmrun: step ID generator is required")
	}
	return nil
}

func stepFromResult[T any](req Request, adapterName, stepID, inputHash, promptHash string, started, completed time.Time, result llm.StructuredResult[T]) ledger.LLMStep {
	providerSessionID := strings.TrimSpace(result.SessionID)
	if providerSessionID == "" {
		providerSessionID = stepID
	}
	return ledger.LLMStep{
		StepID:            stepID,
		RunID:             req.RunID,
		Stage:             string(req.Stage),
		ScopeKey:          req.ScopeKey,
		InputHash:         inputHash,
		PromptHash:        promptHash,
		Provider:          req.Provider,
		Adapter:           adapterName,
		Model:             req.Model,
		Effort:            req.Effort,
		ProviderSessionID: providerSessionID,
		StartedAt:         started,
		CompletedAt:       &completed,
		DurationMS:        &result.Response.DurationMS,
		TokensIn:          intPtrToInt64(result.Response.Usage.TokensIn),
		TokensOut:         intPtrToInt64(result.Response.Usage.TokensOut),
		CacheRead:         intPtrToInt64(result.Response.Usage.CacheRead),
		CacheCreate:       intPtrToInt64(result.Response.Usage.CacheCreate),
		CostUSD:           result.Response.Usage.CostUSD,
	}
}

func intPtrToInt64(value *int) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}
