// Package llm defines provider-neutral LLM adapter contracts and structured
// output validation for review planning.
package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxValidationErrorSummaryLen = 500

var validationQuotedValueRE = regexp.MustCompile(`"([^"\\]|\\.)*"`)

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
	Wait(context.Context) (Response, error)
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

// StructuredResult contains the validated structured value and adapter metadata.
type StructuredResult[T any] struct {
	Value     T
	Response  Response
	SessionID string
}

// RunStructured runs a structured-output request and retries one validation
// failure with a deterministic correction prompt. On retry success, the
// returned Response is the final successful attempt's response; this helper does
// not aggregate usage or duration across attempts.
func RunStructured[T any](ctx context.Context, adapter Adapter, req Request, decode Decoder[T]) (T, Response, error) {
	var zero T
	result, err := RunStructuredWithSession(ctx, adapter, req, decode)
	if err != nil {
		return zero, result.Response, err
	}
	return result.Value, result.Response, nil
}

// RunStructuredWithSession is RunStructured plus the provider session id from
// the successful attempt. It preserves the same retry-once validation behavior.
func RunStructuredWithSession[T any](ctx context.Context, adapter Adapter, req Request, decode Decoder[T]) (StructuredResult[T], error) {
	var zero T
	sessionID, response, err := runOnceWithSession(ctx, adapter, req)
	if err != nil {
		return StructuredResult[T]{Response: response}, err
	}
	value, decodeErr := decode(response.StructuredOutput)
	if decodeErr == nil {
		return StructuredResult[T]{Value: value, Response: response, SessionID: sessionID}, nil
	}

	retryReq := req
	retryReq.Prompt = retryPrompt(req.Prompt, decodeErr)
	retrySessionID, retryResponse, err := runOnceWithSession(ctx, adapter, retryReq)
	if err != nil {
		return StructuredResult[T]{Response: retryResponse}, err
	}
	retryValue, retryErr := decode(retryResponse.StructuredOutput)
	if retryErr != nil {
		return StructuredResult[T]{Value: zero, Response: retryResponse, SessionID: retrySessionID}, fmt.Errorf("structured output invalid after retry: first: %w; second: %w", decodeErr, retryErr)
	}
	return StructuredResult[T]{Value: retryValue, Response: retryResponse, SessionID: retrySessionID}, nil
}

func runOnce(ctx context.Context, adapter Adapter, req Request) (Response, error) {
	_, response, err := runOnceWithSession(ctx, adapter, req)
	return response, err
}

func runOnceWithSession(ctx context.Context, adapter Adapter, req Request) (string, Response, error) {
	stream, err := adapter.Start(ctx, req)
	if err != nil {
		return "", Response{}, err
	}
	response, err := stream.Wait(ctx)
	return stream.SessionID(), response, err
}

func retryPrompt(prompt string, err error) string {
	return prompt + "\n\nThe previous structured output failed validation: " + validationErrorSummary(err) + "\nReturn corrected JSON only."
}

func validationErrorSummary(err error) string {
	summary := strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
	summary = validationQuotedValueRE.ReplaceAllString(summary, `"<value>"`)
	return truncateRunes(summary, maxValidationErrorSummaryLen)
}

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes]) + "..."
}
