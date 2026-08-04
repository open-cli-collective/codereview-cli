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

// ReviewerWorkspaceMode identifies how an adapter can use a prepared reviewer
// workspace.
type ReviewerWorkspaceMode string

// Diff tool evidence terminal states.
const (
	// ReviewerWorkspaceNone means the adapter cannot inspect a caller-provided workspace.
	ReviewerWorkspaceNone ReviewerWorkspaceMode = "none"
	// ReviewerWorkspacePermissionBounded means the adapter can inspect a workspace
	// through adapter/tool permissions.
	ReviewerWorkspacePermissionBounded ReviewerWorkspaceMode = "permission_bounded"
	// ReviewerWorkspaceWrite means the adapter can run against a writable
	// disposable workspace.
	ReviewerWorkspaceWrite ReviewerWorkspaceMode = "workspace_write"
)

// ReviewerWorkspaceCapable reports the adapter's reviewer workspace support.
type ReviewerWorkspaceCapable interface {
	ReviewerWorkspaceMode() ReviewerWorkspaceMode
}

// ReviewerWorkspaceRequest describes the disposable workspace for one reviewer
// task. RepoDir is the reviewer-visible working tree root and ScratchDir owns
// per-invocation writable directories. Env is appended before invocation-owned
// toolchain paths are applied.
type ReviewerWorkspaceRequest struct {
	RepoDir            string
	ScratchDir         string
	DiffPath           string
	Env                []string
	AllowedFiles       []string
	MaxToolOutputBytes int
}

// ErrReviewerWorkspaceUnsupported reports that an adapter cannot use a prepared
// reviewer workspace.
var ErrReviewerWorkspaceUnsupported = errors.New("llm adapter: missing reviewer workspace capability")

// AdapterReviewerWorkspaceMode returns the adapter's reviewer workspace mode.
func AdapterReviewerWorkspaceMode(adapter Adapter) ReviewerWorkspaceMode {
	capable, ok := adapter.(ReviewerWorkspaceCapable)
	if ok {
		switch level := capable.ReviewerWorkspaceMode(); level {
		case ReviewerWorkspacePermissionBounded, ReviewerWorkspaceWrite:
			return level
		case ReviewerWorkspaceNone:
			return ReviewerWorkspaceNone
		default:
			return ReviewerWorkspaceNone
		}
	}
	return ReviewerWorkspaceNone
}

// SupportsReviewerWorkspace reports whether adapter can use a prepared reviewer
// workspace.
func SupportsReviewerWorkspace(adapter Adapter) bool {
	return AdapterReviewerWorkspaceMode(adapter) != ReviewerWorkspaceNone
}

// RequireReviewerWorkspace returns a stable error when adapter cannot inspect a
// prepared reviewer workspace.
func RequireReviewerWorkspace(adapter Adapter) error {
	if SupportsReviewerWorkspace(adapter) {
		return nil
	}
	name := "<nil>"
	if adapter != nil {
		name = adapter.Name()
	}
	return fmt.Errorf("%w: %s", ErrReviewerWorkspaceUnsupported, name)
}

// Request describes one LLM invocation.
type Request struct {
	Model   string
	Effort  string
	Prompt  string
	LogPath string
	Fast    bool

	DurableSession              bool
	ReviewerWorkspace           *ReviewerWorkspaceRequest
	FreshValidationRetrySession bool
	OnValidationRetry           func(*Request) error
}

// Stream is a started LLM request.
type Stream interface {
	SessionID() string
	Wait(context.Context) (Response, error)
}

// Response is the completed LLM result.
type Response struct {
	StructuredOutput     []byte
	Usage                Usage
	DurationMS           int64
	ReviewerToolEvidence *ReviewerToolEvidence
}

// DiffToolStatus records the terminal state of the required reviewer diff tool.
type DiffToolStatus string

const (
	// DiffToolStatusNotInvoked means the reviewer never invoked cr_diff.
	DiffToolStatusNotInvoked DiffToolStatus = "not_invoked"
	// DiffToolStatusIncomplete means the reviewer started but did not complete cr_diff.
	DiffToolStatusIncomplete DiffToolStatus = "incomplete"
	// DiffToolStatusSucceeded means the reviewer completed cr_diff without failure.
	DiffToolStatusSucceeded DiffToolStatus = "succeeded"
	// DiffToolStatusFailed means the reviewer observed a cr_diff failure.
	DiffToolStatusFailed DiffToolStatus = "failed"
)

// ReviewerToolEvidence records bounded, machine-significant reviewer tool state.
// It is absent when the adapter does not provide reviewer tool evidence.
type ReviewerToolEvidence struct {
	DiffStatus     DiffToolStatus `json:"diff_status"`
	DiffDiagnostic string         `json:"diff_diagnostic,omitempty"`
}

// Usage records nullable usage metrics.
type Usage struct {
	TokensIn    *int
	TokensOut   *int
	CacheRead   *int
	CacheCreate *int
	CostUSD     *float64
	Speed       string
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

// RunStructuredWithSessionResume runs a structured request, starting from an
// existing provider session ID when provided.
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
	if retryReq.OnValidationRetry != nil {
		if err := retryReq.OnValidationRetry(&retryReq); err != nil {
			return StructuredResult[T]{Response: response, SessionID: sessionID, ValidationAttempts: attempts}, err
		}
	}
	retryReq.Prompt = retryPrompt(req.Prompt, decodeErr)
	retryResumeSessionID := ""
	if !retryReq.FreshValidationRetrySession {
		retryResumeSessionID = sessionID
	}
	if strings.TrimSpace(retryResumeSessionID) == "" && !retryReq.FreshValidationRetrySession {
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
	if req.ReviewerWorkspace != nil {
		if err := RequireReviewerWorkspace(adapter); err != nil {
			return "", Response{}, err
		}
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
