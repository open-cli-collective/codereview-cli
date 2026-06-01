// Package llm defines provider-neutral LLM adapter contracts and structured
// output validation for review planning.
package llm

import (
	"context"
	"fmt"
)

// Adapter is the provider-neutral LLM boundary.
type Adapter interface {
	Name() string
	SupportsResume() bool
	SupportsCacheAccounting() bool
	SupportsCostReporting() bool
	Quota(context.Context) (Quota, bool, error)
	Start(context.Context, Request) (Stream, error)
	Resume(context.Context, string, Request) (Stream, error)
}

// Request describes one LLM invocation.
type Request struct {
	Model   string
	Effort  string
	Prompt  string
	LogPath string
}

// Stream is a started LLM request.
type Stream interface {
	SessionID() string
	Wait() (Response, error)
}

// Response is the completed LLM result.
type Response struct {
	StructuredOutput []byte
	Usage            Usage
	DurationMS       int64
}

// Usage records nullable usage metrics.
type Usage struct {
	TokensIn    *int
	TokensOut   *int
	CacheRead   *int
	CacheCreate *int
	CostUSD     *float64
}

// Quota records adapter quota state. A value of -1 means unknown.
type Quota struct {
	BlockRemainingPct  float64
	WeeklyRemainingPct float64
}

// Decoder validates and maps structured output bytes.
type Decoder[T any] func([]byte) (T, error)

// RunStructured runs a structured-output request and retries one validation
// failure with a deterministic correction prompt.
func RunStructured[T any](ctx context.Context, adapter Adapter, req Request, decode Decoder[T]) (T, Response, error) {
	var zero T
	response, err := runOnce(ctx, adapter, req)
	if err != nil {
		return zero, response, err
	}
	value, decodeErr := decode(response.StructuredOutput)
	if decodeErr == nil {
		return value, response, nil
	}

	retryReq := req
	retryReq.Prompt = retryPrompt(req.Prompt, decodeErr)
	retryResponse, err := runOnce(ctx, adapter, retryReq)
	if err != nil {
		return zero, retryResponse, err
	}
	retryValue, retryErr := decode(retryResponse.StructuredOutput)
	if retryErr != nil {
		return zero, retryResponse, fmt.Errorf("structured output invalid after retry: first: %w; second: %w", decodeErr, retryErr)
	}
	return retryValue, retryResponse, nil
}

func runOnce(ctx context.Context, adapter Adapter, req Request) (Response, error) {
	stream, err := adapter.Start(ctx, req)
	if err != nil {
		return Response{}, err
	}
	return stream.Wait()
}

func retryPrompt(prompt string, err error) string {
	return prompt + "\n\nThe previous structured output failed validation: " + err.Error() + "\nReturn corrected JSON only."
}
