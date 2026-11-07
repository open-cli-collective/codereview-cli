// Package systemone implements the bounded choice-question subset of the
// System One HTTP protocol used by explicit benchmark inference.
package systemone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTimeout    = 30 * time.Second
	maxResponseBytes  = 1 << 20
	maxRequestIDBytes = 128
)

type errorCategory string

const (
	errorInvalidInput      errorCategory = "invalid_input"
	errorContextUnverified errorCategory = "context_unverified"
	errorContextLimit      errorCategory = "context_limit"
	errorAuth              errorCategory = "auth"
	errorRateLimit         errorCategory = "rate_limit"
	errorUpstream          errorCategory = "upstream"
	errorTimeout           errorCategory = "timeout"
	errorTransport         errorCategory = "transport"
	errorMalformedResponse errorCategory = "malformed_response"
	errorResponseTooLarge  errorCategory = "response_too_large"
	errorTruncated         errorCategory = "truncated"
	errorCancelled         errorCategory = "cancelled" //nolint:misspell // Stable public error code is contract-defined.
)

type clientError struct {
	category errorCategory
	message  string
}

func (e *clientError) Error() string { return e.message }

func newError(category errorCategory, message string) error {
	if !knownErrorCategory(category) {
		category = errorTransport
	}
	return &clientError{category: category, message: message}
}

func knownErrorCategory(category errorCategory) bool {
	switch category {
	case errorInvalidInput, errorContextUnverified, errorContextLimit, errorAuth, errorRateLimit, errorUpstream, errorTimeout, errorTransport, errorMalformedResponse, errorResponseTooLarge, errorTruncated, errorCancelled:
		return true
	default:
		return false
	}
}

// ErrorCode maps a client error to its stable, sanitized category.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var clientErr *clientError
	if errors.As(err, &clientErr) {
		return string(clientErr.category)
	}
	return string(errorTransport)
}

// ChoiceQuestion describes one named choice question.
type ChoiceQuestion struct {
	Instructions string
	Criteria     map[string]string
}

// Request contains the wire request and optional local response-model policy.
// ExpectedModel is not serialized; an empty value requires the response model
// to match Model exactly.
type Request struct {
	Model         string
	ServiceTier   string
	State         json.RawMessage
	Questions     map[string]ChoiceQuestion
	ExpectedModel string
}

// Limits bounds the compact state and complete request body in bytes.
type Limits struct {
	MaxStateBytes   int
	MaxRequestBytes int
}

// Plan describes the exact bytes that Decide will send.
type Plan struct {
	RequestSHA256 string
	StateBytes    int
	RequestBytes  int
}

// Options configures endpoint, authentication, timeout, and local request limits.
type Options struct {
	Endpoint   string
	APIKey     string
	AuthScheme string
	HTTPClient *http.Client
	Timeout    time.Duration
	Limits     Limits
}

// ChoiceAnswer contains one returned choice and its probability distribution.
type ChoiceAnswer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// Usage reports token usage returned by the provider.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Receipt records sanitized metadata for one decision attempt.
type Receipt struct {
	RequestedModel string
	ReturnedModel  string
	RequestSHA256  string
	ResponseSHA256 string
	RequestID      string
	Answers        map[string]ChoiceAnswer
	Usage          *Usage
	Duration       time.Duration
	Attempted      bool
	HTTPStatus     int
}

// Client sends one bounded System One request to its configured HTTPS endpoint.
type Client struct {
	apiKey     string
	authScheme string
	endpoint   string
	httpClient *http.Client
	timeout    time.Duration
	limits     Limits
}

type wireRequest struct {
	Model       string                  `json:"model"`
	ServiceTier string                  `json:"service_tier,omitempty"`
	State       json.RawMessage         `json:"state"`
	Questions   map[string]wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// Prepare validates and hashes an exact compact request without a key or network.
func Prepare(request Request, limits Limits) (Plan, error) {
	plan, _, err := prepare(request, limits)
	return plan, err
}

func prepare(request Request, limits Limits) (Plan, []byte, error) {
	if !validLimits(limits) || !validText(request.Model) || (request.ServiceTier != "" && !validText(request.ServiceTier)) || (request.ExpectedModel != "" && !validText(request.ExpectedModel)) {
		return Plan{}, nil, newError(errorInvalidInput, "systemone: invalid request")
	}
	state, err := compactState(request.State)
	if err != nil {
		return Plan{}, nil, newError(errorInvalidInput, "systemone: invalid request state")
	}
	if len(state) > limits.MaxStateBytes {
		return Plan{}, nil, newError(errorContextLimit, "systemone: request state exceeds local limit")
	}
	if len(request.Questions) == 0 {
		return Plan{}, nil, newError(errorInvalidInput, "systemone: at least one question is required")
	}
	questions := make(map[string]wireQuestion, len(request.Questions))
	for name, question := range request.Questions {
		if !validText(name) || !validText(question.Instructions) || len(question.Criteria) < 2 {
			return Plan{}, nil, newError(errorInvalidInput, "systemone: invalid question")
		}
		criteria := make(map[string]string, len(question.Criteria))
		for label, description := range question.Criteria {
			if !validText(label) || !validText(description) {
				return Plan{}, nil, newError(errorInvalidInput, "systemone: invalid criterion")
			}
			criteria[label] = description
		}
		questions[name] = wireQuestion{Type: "choice", Instructions: question.Instructions, Criteria: criteria}
	}
	body, err := marshalCompact(wireRequest{
		Model:       request.Model,
		ServiceTier: request.ServiceTier,
		State:       json.RawMessage(state),
		Questions:   questions,
	})
	if err != nil {
		return Plan{}, nil, newError(errorInvalidInput, "systemone: invalid request")
	}
	if len(body) > limits.MaxRequestBytes {
		return Plan{}, nil, newError(errorContextLimit, "systemone: request exceeds local limit")
	}
	digest := sha256.Sum256(body)
	return Plan{
		RequestSHA256: hex.EncodeToString(digest[:]),
		StateBytes:    len(state),
		RequestBytes:  len(body),
	}, body, nil
}

// New constructs a client and clones the injected HTTP client before changing
// timeout or redirect behavior.
func New(options Options) (*Client, error) {
	if !validEndpoint(options.Endpoint) || options.APIKey == "" || !validHeaderValue(options.APIKey) || !validAuthScheme(options.AuthScheme) || !validLimits(options.Limits) || options.Timeout < 0 {
		return nil, newError(errorInvalidInput, "systemone: invalid client options")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	var httpClient *http.Client
	if options.HTTPClient == nil {
		httpClient = &http.Client{}
	} else {
		clientCopy := *options.HTTPClient
		httpClient = &clientCopy
	}
	httpClient.Timeout = timeout
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		apiKey:     options.APIKey,
		authScheme: options.AuthScheme,
		endpoint:   options.Endpoint,
		httpClient: httpClient,
		timeout:    timeout,
		limits:     options.Limits,
	}, nil
}

// Decide sends one complete request and returns normalized answers plus a
// sanitized receipt, including independently validated metadata on failures.
func (c *Client) Decide(ctx context.Context, request Request) (Receipt, error) {
	receipt := Receipt{RequestedModel: request.Model}
	if ctx == nil || c == nil || c.httpClient == nil {
		return receipt, newError(errorInvalidInput, "systemone: invalid decision request")
	}
	plan, requestBody, err := prepare(request, c.limits)
	if err != nil {
		return receipt, err
	}
	receipt.RequestSHA256 = plan.RequestSHA256
	if ctx.Err() != nil {
		return receipt, contextError(ctx.Err())
	}
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if requestContext.Err() != nil {
		return receipt, contextError(requestContext.Err())
	}
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return receipt, newError(errorInvalidInput, "systemone: could not create request")
	}
	httpRequest.Header.Set("Authorization", c.authScheme+" "+c.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")

	started := time.Now()
	receipt.Attempted = true
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		receipt.Duration = time.Since(started)
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(err) {
			return receipt, newError(errorTimeout, "systemone: request timed out")
		}
		return receipt, newError(errorTransport, "systemone: request transport failed")
	}
	if response == nil {
		receipt.Duration = time.Since(started)
		return receipt, newError(errorTransport, "systemone: request transport failed")
	}
	receipt.HTTPStatus = response.StatusCode
	receipt.RequestID = safeRequestID(response.Header, c.apiKey)
	if declaredResponseTooLarge(response) {
		_ = closeResponse(response.Body)
		receipt.Duration = time.Since(started)
		return receipt, newError(errorResponseTooLarge, "systemone: response exceeds local limit")
	}
	responseBody, readErr, closeErr := readBoundedResponse(response)
	receipt.Duration = time.Since(started)
	if len(responseBody) > maxResponseBytes {
		return receipt, newError(errorResponseTooLarge, "systemone: response exceeds local limit")
	}
	if readErr != nil {
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(readErr) {
			return receipt, newError(errorTimeout, "systemone: request timed out")
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return receipt, newError(errorTruncated, "systemone: response was truncated")
		}
		return receipt, newError(errorTransport, "systemone: response could not be read")
	}
	if closeErr != nil {
		return receipt, newError(errorTransport, "systemone: response could not be closed")
	}
	if contentLengthMismatch(response, len(responseBody)) {
		return receipt, newError(errorTruncated, "systemone: response length did not match its declaration")
	}
	responseDigest := sha256.Sum256(responseBody)
	receipt.ResponseSHA256 = hex.EncodeToString(responseDigest[:])
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		receipt.ReturnedModel, receipt.Usage = responseMetadata(responseBody, expectedModel(request))
		return receipt, statusError(response.StatusCode)
	}
	answers, returnedModel, usage, parseErr := parseResponse(responseBody, request)
	if returnedModel != "" {
		receipt.ReturnedModel = returnedModel
	}
	if usage != nil {
		receipt.Usage = usage
	}
	if parseErr != nil {
		return receipt, parseErr
	}
	receipt.Answers = answers
	return receipt, nil
}

func validLimits(limits Limits) bool {
	return limits.MaxStateBytes > 0 && limits.MaxRequestBytes > 0
}

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) != "" && utf8.ValidString(value)
}

func validHeaderValue(value string) bool {
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return value != ""
}

func validAuthScheme(value string) bool {
	return value == "Api-Key" || value == "Bearer"
}

func validEndpoint(value string) bool {
	if value == "" || strings.Contains(value, "#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	return true
}

func compactState(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !utf8.Valid(trimmed) || !json.Valid(trimmed) || rejectDuplicateKeys(trimmed) != nil {
		return nil, errors.New("invalid state")
	}
	switch trimmed[0] {
	case '{', '[', '"':
	default:
		return nil, errors.New("invalid state root")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func marshalCompact(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	return append([]byte(nil), encoded[:len(encoded)-1]...), nil
}

func expectedModel(request Request) string {
	if request.ExpectedModel != "" {
		return request.ExpectedModel
	}
	return request.Model
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(errorTimeout, "systemone: request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return newError(errorCancelled, "systemone: request was canceled")
	}
	return newError(errorTransport, "systemone: request transport failed")
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newError(errorAuth, "systemone: provider rejected authentication")
	case http.StatusTooManyRequests:
		return newError(errorRateLimit, "systemone: provider rate limit reached")
	case http.StatusRequestEntityTooLarge:
		return newError(errorContextLimit, "systemone: provider rejected request size")
	default:
		return newError(errorUpstream, "systemone: provider returned an unsuccessful status")
	}
}

func readBoundedResponse(response *http.Response) ([]byte, error, error) {
	if response.Body == nil {
		return nil, nil, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := closeResponse(response.Body)
	return body, readErr, closeErr
}

func declaredResponseTooLarge(response *http.Response) bool {
	if response.ContentLength > maxResponseBytes {
		return true
	}
	declared := response.Header.Get("Content-Length")
	if declared == "" {
		return false
	}
	length, err := strconv.ParseInt(declared, 10, 64)
	return err == nil && length > maxResponseBytes
}

func closeResponse(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	return body.Close()
}

func contentLengthMismatch(response *http.Response, bodyBytes int) bool {
	if response.ContentLength >= 0 && response.ContentLength != int64(bodyBytes) {
		return true
	}
	declared := response.Header.Get("Content-Length")
	if declared == "" {
		return false
	}
	length, err := strconv.ParseInt(declared, 10, 64)
	return err != nil || length < 0 || length != int64(bodyBytes)
}

func safeRequestID(header http.Header, apiKey string) string {
	value := header.Get("X-Request-ID")
	if value == "" {
		value = header.Get("X-Baseten-Request-ID")
	}
	if len(value) == 0 || len(value) > maxRequestIDBytes || (apiKey != "" && strings.Contains(value, apiKey)) {
		return ""
	}
	for _, character := range []byte(value) {
		if !validRequestIDCharacter(character) {
			return ""
		}
	}
	return value
}

func validRequestIDCharacter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", rune(character))
}
