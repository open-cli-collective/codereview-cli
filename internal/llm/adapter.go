// Package llm defines provider-neutral LLM adapter contracts and structured
// output validation for review planning.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxValidationErrorSummaryLen = 500

var validationQuotedValueRE = regexp.MustCompile(`"([^"\\]|\\.)*"`)

// ErrStructuredOutputInvalidAfterRetry marks a structured-output request whose
// initial response and single validation retry both failed decoding.
var ErrStructuredOutputInvalidAfterRetry = errors.New("structured output invalid after retry")

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

type decodedValue[T any] struct {
	Value    T
	Response Response
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
	return RunStructuredWithSessionResume(ctx, adapter, "", req, decode)
}

// RunStructuredWithSessionResume is RunStructuredWithSession starting from an
// existing provider session id when provided.
func RunStructuredWithSessionResume[T any](ctx context.Context, adapter Adapter, resumeSessionID string, req Request, decode Decoder[T]) (StructuredResult[T], error) {
	var zero T
	sessionID, response, err := runOnceWithSession(ctx, adapter, resumeSessionID, req)
	if err != nil {
		return StructuredResult[T]{Response: response}, err
	}
	decoded, decodeErr := decodeResponse(response, decode)
	if decodeErr == nil {
		return StructuredResult[T]{Value: decoded.Value, Response: decoded.Response, SessionID: sessionID}, nil
	}

	retryReq := req
	retryReq.Prompt = retryPrompt(req.Prompt, decodeErr)
	retryResumeSessionID := sessionID
	if strings.TrimSpace(retryResumeSessionID) == "" {
		retryResumeSessionID = resumeSessionID
	}
	retrySessionID, retryResponse, err := runOnceWithSession(ctx, adapter, retryResumeSessionID, retryReq)
	if err != nil {
		return StructuredResult[T]{Response: retryResponse}, err
	}
	retryDecoded, retryErr := decodeResponse(retryResponse, decode)
	if retryErr != nil {
		return StructuredResult[T]{Value: zero, Response: retryResponse, SessionID: retrySessionID}, fmt.Errorf("%w: first: %w; second: %w", ErrStructuredOutputInvalidAfterRetry, decodeErr, retryErr)
	}
	return StructuredResult[T]{Value: retryDecoded.Value, Response: retryDecoded.Response, SessionID: retrySessionID}, nil
}

func runOnceWithSession(ctx context.Context, adapter Adapter, resumeSessionID string, req Request) (string, Response, error) {
	var (
		stream Stream
		err    error
	)
	if strings.TrimSpace(resumeSessionID) != "" && adapter.SupportsResume() {
		stream, err = adapter.Resume(ctx, resumeSessionID, req)
	} else {
		stream, err = adapter.Start(ctx, req)
	}
	if err != nil {
		return "", Response{}, err
	}
	response, err := stream.Wait(ctx)
	return stream.SessionID(), response, err
}

func retryPrompt(prompt string, err error) string {
	return prompt + "\n\nThe previous structured output failed validation: " + validationErrorSummary(err) + "\nReturn corrected JSON only. Do not wrap the JSON in markdown fences, add prose, or include any leading or trailing text."
}

func decodeResponse[T any](response Response, decode Decoder[T]) (decodedValue[T], error) {
	value, err := decode(response.StructuredOutput)
	if err == nil {
		return decodedValue[T]{Value: value, Response: response}, nil
	}
	recovered, ok := extractSingleJSONObject(response.StructuredOutput)
	if !ok {
		var zero T
		return decodedValue[T]{Value: zero, Response: response}, err
	}
	recoveredValue, recoveredErr := decode(recovered)
	if recoveredErr != nil {
		var zero T
		return decodedValue[T]{Value: zero, Response: response}, recoveredErr
	}
	return decodedValue[T]{Value: recoveredValue, Response: response}, nil
}

func extractSingleJSONObject(data []byte) ([]byte, bool) {
	var candidates [][]byte
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
			continue
		case '{':
		default:
			continue
		}
		end, ok := objectEnd(data, i)
		if !ok {
			continue
		}
		prefix := data[:i]
		suffix := data[end+1:]
		if hasArrayWrapperAdjacentToObject(data[:i], data[end+1:]) {
			i = end
			continue
		}
		if isJSONValueSequence(prefix) || isJSONValueSequence(suffix) || hasUnclosedJSONContainer(prefix) {
			i = end
			continue
		}
		candidate := bytes.TrimSpace(data[i : end+1])
		if json.Valid(candidate) {
			candidates = append(candidates, candidate)
		}
		i = end
	}
	if len(candidates) != 1 {
		return nil, false
	}
	return append([]byte(nil), candidates[0]...), true
}

func hasArrayWrapperAdjacentToObject(prefix []byte, suffix []byte) bool {
	prefix = bytes.TrimSpace(prefix)
	suffix = bytes.TrimSpace(suffix)
	return len(prefix) > 0 && prefix[len(prefix)-1] == '[' ||
		len(suffix) > 0 && suffix[0] == ']'
}

func isJSONValueSequence(data []byte) bool {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return errors.Is(err, io.EOF)
		}
	}
}

func hasUnclosedJSONContainer(data []byte) bool {
	var stack []byte
	inString := false
	escaped := false
	for _, ch := range data {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}':
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				stack = stack[:len(stack)-1]
			}
		case ']':
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return len(stack) > 0
}

func objectEnd(data []byte, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(data); i++ {
		ch := data[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
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
