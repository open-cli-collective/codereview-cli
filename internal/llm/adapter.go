// Package llm defines provider-neutral LLM adapter contracts and structured
// output validation for review planning.
package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxValidationErrorSummaryLen = 500

var validationQuotedValueRE = regexp.MustCompile(`"([^"\\]|\\.)*"`)
var validationUnknownFieldRE = regexp.MustCompile(`json: unknown field "([^"\\]+)"`)

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

// CheckoutAccessLevel identifies how strongly an adapter can constrain checkout
// access for checkout-native review.
type CheckoutAccessLevel string

const (
	// CheckoutAccessNone means the adapter cannot inspect a caller-provided checkout.
	CheckoutAccessNone CheckoutAccessLevel = "none"
	// CheckoutAccessPermissionBounded means the adapter can inspect a checkout
	// through adapter/tool permissions, without an OS-level readonly guarantee.
	CheckoutAccessPermissionBounded CheckoutAccessLevel = "permission_bounded"
	// CheckoutAccessReadonly means the adapter can inspect a checkout with writes
	// constrained away from the checkout by adapter-enforced sandboxing.
	CheckoutAccessReadonly CheckoutAccessLevel = "readonly"
)

// CheckoutAccessCapable reports the adapter's checkout-native access level.
type CheckoutAccessCapable interface {
	CheckoutAccessLevel() CheckoutAccessLevel
}

// CheckoutReadonlyCapable reports whether an adapter can safely inspect a
// caller-provided read-only checkout with writes limited to a caller-owned
// scratch root.
type CheckoutReadonlyCapable interface {
	SupportsCheckoutReadonly() bool
}

// CheckoutAccessRequest describes bounded checkout access for a single LLM
// invocation. RootDir is the reviewer-visible read root; ScratchDir is the
// writable scratch root owned by the harness; AllowedFiles preserves the
// orchestrator's optional narrowing intent for logs and smoke-path assertions.
type CheckoutAccessRequest struct {
	RootDir            string
	ScratchDir         string
	AllowedFiles       []string
	MaxToolOutputBytes int
}

// CheckoutReadonlyRequest is the legacy name for CheckoutAccessRequest.
type CheckoutReadonlyRequest = CheckoutAccessRequest

// ErrCheckoutAccessUnsupported reports that an adapter cannot provide checkout
// access for checkout-native review.
var ErrCheckoutAccessUnsupported = errors.New("llm adapter: missing checkout access capability")

// ErrCheckoutReadonlyUnsupported reports that an adapter cannot safely provide
// checkout-readonly review access.
var ErrCheckoutReadonlyUnsupported = errors.New("llm adapter: missing checkout-readonly capability")

// AdapterCheckoutAccessLevel returns adapter's checkout access level.
func AdapterCheckoutAccessLevel(adapter Adapter) CheckoutAccessLevel {
	capable, ok := adapter.(CheckoutAccessCapable)
	if ok {
		switch level := capable.CheckoutAccessLevel(); level {
		case CheckoutAccessPermissionBounded, CheckoutAccessReadonly:
			return level
		case CheckoutAccessNone:
			return CheckoutAccessNone
		default:
			return CheckoutAccessNone
		}
	}
	if SupportsCheckoutReadonly(adapter) {
		return CheckoutAccessReadonly
	}
	return CheckoutAccessNone
}

// SupportsCheckoutAccess reports whether adapter can inspect a prepared checkout
// for checkout-native review.
func SupportsCheckoutAccess(adapter Adapter) bool {
	return AdapterCheckoutAccessLevel(adapter) != CheckoutAccessNone
}

// SupportsCheckoutReadonly reports whether adapter exposes the supplemental
// checkout-readonly capability.
func SupportsCheckoutReadonly(adapter Adapter) bool {
	if capable, ok := adapter.(CheckoutAccessCapable); ok {
		return capable.CheckoutAccessLevel() == CheckoutAccessReadonly
	}
	capable, ok := adapter.(CheckoutReadonlyCapable)
	return ok && capable.SupportsCheckoutReadonly()
}

// RequireCheckoutAccess returns a stable error when adapter cannot inspect the
// prepared checkout for checkout-native review.
func RequireCheckoutAccess(adapter Adapter) error {
	if SupportsCheckoutAccess(adapter) {
		return nil
	}
	name := "<nil>"
	if adapter != nil {
		name = adapter.Name()
	}
	return fmt.Errorf("%w: %s", ErrCheckoutAccessUnsupported, name)
}

// RequireCheckoutReadonly returns a stable error when adapter does not expose
// the supplemental checkout-readonly capability.
func RequireCheckoutReadonly(adapter Adapter) error {
	if SupportsCheckoutReadonly(adapter) {
		return nil
	}
	name := "<nil>"
	if adapter != nil {
		name = adapter.Name()
	}
	return fmt.Errorf("%w: %s", ErrCheckoutReadonlyUnsupported, name)
}

// Request describes one LLM invocation.
type Request struct {
	Model   string
	Effort  string
	Prompt  string
	LogPath string

	CheckoutAccess   *CheckoutAccessRequest
	CheckoutReadonly *CheckoutReadonlyRequest
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
	Value              T
	Response           Response
	SessionID          string
	ValidationAttempts []StructuredValidationAttempt
	AcceptedOutput     []byte
}

// StructuredValidationAttempt records one failed schema-validation attempt.
type StructuredValidationAttempt struct {
	Label       string
	SessionID   string
	Response    Response
	DecodeError error
}

// StructuredValidationError carries both invalid structured-output attempts
// when the validation correction retry also fails.
type StructuredValidationError struct {
	Attempts []StructuredValidationAttempt
}

func (e *StructuredValidationError) Error() string {
	first, second := "unknown", "unknown"
	if len(e.Attempts) > 0 && e.Attempts[0].DecodeError != nil {
		first = e.Attempts[0].DecodeError.Error()
	}
	if len(e.Attempts) > 1 && e.Attempts[1].DecodeError != nil {
		second = e.Attempts[1].DecodeError.Error()
	}
	return fmt.Sprintf("%s: first: %s; second: %s", ErrStructuredOutputInvalidAfterRetry, first, second)
}

// Is matches ErrStructuredOutputInvalidAfterRetry for errors.Is callers.
func (e *StructuredValidationError) Is(target error) bool {
	return target == ErrStructuredOutputInvalidAfterRetry
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
		return StructuredResult[T]{Response: response, SessionID: sessionID}, err
	}
	value, acceptedOutput, decodeErr := decodeStructuredAccepted(decode, response.StructuredOutput)
	if decodeErr == nil {
		return StructuredResult[T]{Value: value, Response: response, SessionID: sessionID, AcceptedOutput: acceptedOutput}, nil
	}
	attempts := []StructuredValidationAttempt{{
		Label:       "initial",
		SessionID:   sessionID,
		Response:    cloneResponse(response),
		DecodeError: decodeErr,
	}}

	retryReq := req
	retryReq.Prompt = retryPrompt(req.Prompt, decodeErr)
	retryResumeSessionID := sessionID
	if strings.TrimSpace(retryResumeSessionID) == "" {
		retryResumeSessionID = resumeSessionID
	}
	retrySessionID, retryResponse, err := runOnceWithSession(ctx, adapter, retryResumeSessionID, retryReq)
	if err != nil {
		return StructuredResult[T]{Response: retryResponse, SessionID: retrySessionID, ValidationAttempts: attempts}, err
	}
	retryValue, retryAcceptedOutput, retryErr := decodeStructuredAccepted(decode, retryResponse.StructuredOutput)
	if retryErr != nil {
		attempts = append(attempts, StructuredValidationAttempt{
			Label:       "retry",
			SessionID:   retrySessionID,
			Response:    cloneResponse(retryResponse),
			DecodeError: retryErr,
		})
		return StructuredResult[T]{Value: zero, Response: retryResponse, SessionID: retrySessionID, ValidationAttempts: attempts}, &StructuredValidationError{Attempts: attempts}
	}
	return StructuredResult[T]{Value: retryValue, Response: retryResponse, SessionID: retrySessionID, ValidationAttempts: attempts, AcceptedOutput: retryAcceptedOutput}, nil
}

// decodeStructuredAccepted strict-decodes data, then on failure recovers a
// response that wraps exactly one balanced top-level JSON object in surrounding
// prose by decoding the extracted object with the same schema decoder. When the
// extracted object also fails the schema, that error is returned because it
// describes the real schema violation; otherwise the strict error stands.
func decodeStructuredAccepted[T any](decode Decoder[T], data []byte) (T, []byte, error) {
	value, err := decode(data)
	if err == nil {
		return value, data, nil
	}
	var zero T
	extracted, ok := extractSingleJSONObject(data)
	if !ok || bytes.Equal(extracted, data) {
		return zero, nil, err
	}
	extractedValue, extractedErr := decode(extracted)
	if extractedErr != nil {
		return zero, nil, extractedErr
	}
	return extractedValue, extracted, nil
}

// runOnceWithSession runs a single attempt and retries transient provider
// failures with bounded jittered exponential backoff. A non-transient error, a
// canceled context, or exhausting the retry budget returns the last error
// (still wrapping ErrTransient when applicable, so a give-up remains
// classifiable). Decode-validation retries are handled one layer up.
func runOnceWithSession(ctx context.Context, adapter Adapter, resumeSessionID string, req Request) (string, Response, error) {
	for attempt := 0; ; attempt++ {
		sid, resp, err := runOnceAttempt(ctx, adapter, resumeSessionID, req)
		if err == nil {
			return sid, resp, nil
		}
		if attempt >= activeRetryPolicy.MaxRetries || !errors.Is(err, ErrTransient) || ctx.Err() != nil {
			return sid, resp, err
		}
		if waitErr := sleepBackoff(ctx, activeRetryPolicy, attempt); waitErr != nil {
			return sid, resp, err
		}
	}
}

func runOnceAttempt(ctx context.Context, adapter Adapter, resumeSessionID string, req Request) (string, Response, error) {
	var (
		stream Stream
		err    error
	)
	if err := requireRequestCheckoutCapability(adapter, req); err != nil {
		return "", Response{}, err
	}
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

func requestCheckoutAccess(req Request) *CheckoutAccessRequest {
	if req.CheckoutAccess != nil {
		return req.CheckoutAccess
	}
	return req.CheckoutReadonly
}

func requireRequestCheckoutCapability(adapter Adapter, req Request) error {
	if req.CheckoutReadonly != nil {
		return RequireCheckoutReadonly(adapter)
	}
	if req.CheckoutAccess != nil {
		return RequireCheckoutAccess(adapter)
	}
	return nil
}

func cloneResponse(response Response) Response {
	response.StructuredOutput = append([]byte(nil), response.StructuredOutput...)
	return response
}

func retryPrompt(prompt string, err error) string {
	return prompt + "\n\nThe previous structured output failed validation: " + validationErrorSummary(err) + "\nReturn corrected JSON only. Do not wrap the JSON in markdown fences, add prose, or include any leading or trailing text."
}

func validationErrorSummary(err error) string {
	summary := strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
	unknownField := safeUnknownFieldName(summary)
	summary = validationQuotedValueRE.ReplaceAllString(summary, `"<value>"`)
	if unknownField != "" {
		summary = strings.Replace(summary, `json: unknown field "<value>"`, fmt.Sprintf(`json: unknown field %q`, unknownField), 1)
	}
	return truncateRunes(summary, maxValidationErrorSummaryLen)
}

func safeUnknownFieldName(summary string) string {
	matches := validationUnknownFieldRE.FindStringSubmatch(summary)
	if len(matches) != 2 || !safeJSONFieldName(matches[1]) {
		return ""
	}
	return matches[1]
}

func safeJSONFieldName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
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
