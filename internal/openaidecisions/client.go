// Package openaidecisions implements a bounded single-question choice subset
// of the OpenAI Decisions HTTP API.
package openaidecisions

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
	errorRefused           errorCategory = "refused"
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
	case errorInvalidInput, errorContextUnverified, errorContextLimit, errorAuth, errorRateLimit, errorUpstream, errorTimeout, errorTransport, errorMalformedResponse, errorResponseTooLarge, errorTruncated, errorCancelled, errorRefused:
		return true
	default:
		return false
	}
}

// ErrorCode returns the stable, sanitized category for a client error.
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

// Choice is one string value that the model may return.
type Choice struct {
	Value       string
	Description string
}

// Request describes one named string-choice question. ExpectedModel is local
// response validation policy and is never sent to the API.
type Request struct {
	Model         string
	ExpectedModel string
	Input         string
	Name          string
	Instructions  string
	Choices       []Choice
}

// Limits bounds the input string and complete compact request body in bytes.
type Limits struct {
	MaxInputBytes   int
	MaxRequestBytes int
}

// Plan describes the exact compact bytes that Decide will send.
type Plan struct {
	RequestSHA256 string
	InputBytes    int
	RequestBytes  int
}

// Options configures the HTTPS endpoint, bearer key, timeout, and byte limits.
type Options struct {
	Endpoint   string
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
	Limits     Limits
}

// Usage reports validated token counts. Nil cache fields mean unknown.
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens *int64
	CacheWriteTokens  *int64
}

// Answer contains the returned choice and its probability distribution.
type Answer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// Receipt records sanitized metadata for one decision attempt.
type Receipt struct {
	RequestedModel string
	ReturnedModel  string
	RequestSHA256  string
	ResponseSHA256 string
	RequestID      string
	Answer         *Answer
	Usage          *Usage
	Attempted      bool
	HTTPStatus     int
	Duration       time.Duration
}

// Client sends one bounded choice request to its configured HTTPS endpoint.
type Client struct {
	apiKey     string
	endpoint   string
	httpClient *http.Client
	timeout    time.Duration
	limits     Limits
}

type wireRequest struct {
	Model     string         `json:"model"`
	Input     string         `json:"input"`
	Questions []wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         string       `json:"type"`
	Name         string       `json:"name"`
	Instructions string       `json:"instructions"`
	Choices      []wireChoice `json:"choices"`
}

type wireChoice struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

// Prepare validates, serializes, and hashes a request without a key or network.
func Prepare(request Request, limits Limits) (Plan, error) {
	plan, _, err := prepare(request, limits)
	return plan, err
}

func prepare(request Request, limits Limits) (Plan, []byte, error) {
	if !validLimits(limits) || !validText(request.Model) ||
		(request.ExpectedModel != "" && !validText(request.ExpectedModel)) ||
		!utf8.ValidString(request.Input) || !validText(request.Name) ||
		!validText(request.Instructions) || len(request.Choices) < 2 || len(request.Choices) > 255 {
		return Plan{}, nil, newError(errorInvalidInput, "openaidecisions: invalid request")
	}
	inputBytes := len([]byte(request.Input))
	if inputBytes > limits.MaxInputBytes {
		return Plan{}, nil, newError(errorContextLimit, "openaidecisions: input exceeds local limit")
	}

	choices := make([]wireChoice, len(request.Choices))
	seenValues := make(map[string]struct{}, len(request.Choices))
	for index, choice := range request.Choices {
		if !validText(choice.Value) || !validText(choice.Description) {
			return Plan{}, nil, newError(errorInvalidInput, "openaidecisions: invalid choice")
		}
		if _, exists := seenValues[choice.Value]; exists {
			return Plan{}, nil, newError(errorInvalidInput, "openaidecisions: duplicate choice value")
		}
		seenValues[choice.Value] = struct{}{}
		choices[index] = wireChoice(choice)
	}

	body, err := marshalCompact(wireRequest{
		Model: request.Model,
		Input: request.Input,
		Questions: []wireQuestion{{
			Type:         "choice",
			Name:         request.Name,
			Instructions: request.Instructions,
			Choices:      choices,
		}},
	})
	if err != nil {
		return Plan{}, nil, newError(errorInvalidInput, "openaidecisions: invalid request")
	}
	if len(body) > limits.MaxRequestBytes {
		return Plan{}, nil, newError(errorContextLimit, "openaidecisions: request exceeds local limit")
	}
	digest := sha256.Sum256(body)
	return Plan{
		RequestSHA256: hex.EncodeToString(digest[:]),
		InputBytes:    inputBytes,
		RequestBytes:  len(body),
	}, body, nil
}

// New constructs a client and clones the injected HTTP client before changing
// timeout and redirect behavior.
func New(options Options) (*Client, error) {
	if !validEndpoint(options.Endpoint) || !validHeaderValue(options.APIKey) || !validLimits(options.Limits) || options.Timeout < 0 {
		return nil, newError(errorInvalidInput, "openaidecisions: invalid client options")
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
		endpoint:   options.Endpoint,
		httpClient: httpClient,
		timeout:    timeout,
		limits:     options.Limits,
	}, nil
}

// Decide sends one request and returns a sanitized receipt, including
// independently validated metadata when the answer is invalid or refused.
func (c *Client) Decide(ctx context.Context, request Request) (Receipt, error) {
	receipt := Receipt{RequestedModel: request.Model}
	if ctx == nil || c == nil || c.httpClient == nil {
		return receipt, newError(errorInvalidInput, "openaidecisions: invalid decision request")
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
		return receipt, newError(errorInvalidInput, "openaidecisions: could not create request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
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
			return receipt, newError(errorTimeout, "openaidecisions: request timed out")
		}
		return receipt, newError(errorTransport, "openaidecisions: request transport failed")
	}
	if response == nil {
		receipt.Duration = time.Since(started)
		return receipt, newError(errorTransport, "openaidecisions: request transport failed")
	}
	receipt.HTTPStatus = response.StatusCode
	receipt.RequestID = safeRequestID(response.Header, c.apiKey)
	if declaredResponseTooLarge(response) {
		_ = closeResponse(response.Body)
		receipt.Duration = time.Since(started)
		return receipt, newError(errorResponseTooLarge, "openaidecisions: response exceeds local limit")
	}
	responseBody, readErr, closeErr := readBoundedResponse(response)
	receipt.Duration = time.Since(started)
	if len(responseBody) > maxResponseBytes {
		return receipt, newError(errorResponseTooLarge, "openaidecisions: response exceeds local limit")
	}
	if readErr != nil {
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(readErr) {
			return receipt, newError(errorTimeout, "openaidecisions: request timed out")
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return receipt, newError(errorTruncated, "openaidecisions: response was truncated")
		}
		return receipt, newError(errorTransport, "openaidecisions: response could not be read")
	}
	if closeErr != nil {
		return receipt, newError(errorTransport, "openaidecisions: response could not be closed")
	}
	if contentLengthMismatch(response, len(responseBody)) {
		return receipt, newError(errorTruncated, "openaidecisions: response length did not match its declaration")
	}
	responseDigest := sha256.Sum256(responseBody)
	receipt.ResponseSHA256 = hex.EncodeToString(responseDigest[:])

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		receipt.ReturnedModel, receipt.Usage = responseMetadata(responseBody, expectedModel(request))
		return receipt, statusError(response.StatusCode)
	}
	answer, returnedModel, usage, parseErr := parseResponse(responseBody, request)
	if returnedModel != "" {
		receipt.ReturnedModel = returnedModel
	}
	if usage != nil {
		receipt.Usage = usage
	}
	if parseErr != nil {
		return receipt, parseErr
	}
	receipt.Answer = answer
	return receipt, nil
}

func validLimits(limits Limits) bool {
	return limits.MaxInputBytes > 0 && limits.MaxRequestBytes > 0
}

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) != "" && utf8.ValidString(value)
}

func validHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
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
		return newError(errorTimeout, "openaidecisions: request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return newError(errorCancelled, "openaidecisions: request was canceled")
	}
	return newError(errorTransport, "openaidecisions: request transport failed")
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newError(errorAuth, "openaidecisions: provider rejected authentication")
	case http.StatusTooManyRequests:
		return newError(errorRateLimit, "openaidecisions: provider rate limit reached")
	case http.StatusRequestEntityTooLarge:
		return newError(errorContextLimit, "openaidecisions: provider rejected request size")
	default:
		return newError(errorUpstream, "openaidecisions: provider returned an unsuccessful status")
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
