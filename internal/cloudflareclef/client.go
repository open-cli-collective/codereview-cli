// Package cloudflareclef sends the bounded System One choice subset to
// Cloudflare Workers AI Clef models.
package cloudflareclef

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 1 << 20
	maxRequestIDSize = 128
	baseEndpoint     = "https://api.cloudflare.com/client/v4/accounts/"
)

const (
	errorInvalidInput      = "invalid_input"
	errorContextLimit      = "context_limit"
	errorAuth              = "auth"
	errorRateLimit         = "rate_limit"
	errorUpstream          = "upstream"
	errorTimeout           = "timeout"
	errorTransport         = "transport"
	errorMalformedResponse = "malformed_response"
	errorResponseTooLarge  = "response_too_large"
	errorTruncated         = "truncated"
	errorCancelled         = "cancelled" //nolint:misspell // Stable public error code matches other inference clients.
)

type clientError struct {
	code    string
	message string
}

func (e *clientError) Error() string { return e.message }

func newError(code, message string) error {
	return &clientError{code: code, message: message}
}

// ErrorCode returns a stable sanitized category for client or codec errors.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var clientErr *clientError
	if errors.As(err, &clientErr) {
		return clientErr.code
	}
	return systemone.ErrorCode(err)
}

// Options configures account routing, authentication, timeout, and local
// request limits. The endpoint is fixed by the selected account and model.
type Options struct {
	AccountID  string
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
	Limits     systemone.Limits
}

// Usage reports token usage returned by Cloudflare. TotalTokens is nil when
// the provider did not report a total.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  *int64
}

// Receipt records sanitized metadata for one decision attempt. ContextStatus
// is always "unverified": the provider may silently truncate long state.
type Receipt struct {
	RequestedModel string
	ReturnedModel  string
	RequestSHA256  string
	ResponseSHA256 string
	RequestID      string
	Answers        map[string]systemone.ChoiceAnswer
	Usage          *Usage
	Attempted      bool
	HTTPStatus     int
	Duration       time.Duration
	ContextStatus  string
}

// Client sends one bounded request to the fixed Cloudflare Workers AI route.
type Client struct {
	accountID  string
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
	limits     systemone.Limits
}

// Prepare validates the Clef model selector and produces a System One request
// plan without using credentials or the network.
func Prepare(request systemone.Request, limits systemone.Limits) (systemone.Plan, error) {
	if !validModel(request.Model) || request.ServiceTier != "" {
		return systemone.Plan{}, newError(errorInvalidInput, "cloudflareclef: invalid request")
	}
	return systemone.Prepare(request, limits)
}

// Endpoint validates account and model selectors and returns their fixed
// Workers AI route without credentials or a network request.
func Endpoint(accountID, model string) (string, error) {
	if !validAccountID(accountID) || !validModel(model) {
		return "", newError(errorInvalidInput, "cloudflareclef: invalid account or model")
	}
	return baseEndpoint + accountID + "/ai/run/@cf/cloudflare/" + model, nil
}

// New constructs a client with a cloned HTTP client, bounded timeout, and no
// redirect following.
func New(options Options) (*Client, error) {
	if !validAccountID(options.AccountID) || !validAPIKey(options.APIKey) || !validLimits(options.Limits) || options.Timeout < 0 {
		return nil, newError(errorInvalidInput, "cloudflareclef: invalid client options")
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
		accountID:  options.AccountID,
		apiKey:     options.APIKey,
		httpClient: httpClient,
		timeout:    timeout,
		limits:     options.Limits,
	}, nil
}

// Decide sends one complete, bounded request and returns validated answers
// with an explicit unverified-context status.
func (c *Client) Decide(ctx context.Context, request systemone.Request) (Receipt, error) {
	receipt := Receipt{RequestedModel: request.Model, ContextStatus: "unverified"}
	if ctx == nil || c == nil || c.httpClient == nil {
		return receipt, newError(errorInvalidInput, "cloudflareclef: invalid decision request")
	}
	if _, err := Prepare(request, c.limits); err != nil {
		return receipt, err
	}
	plan, requestBody, err := systemone.Encode(request, c.limits)
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
	endpoint, err := Endpoint(c.accountID, request.Model)
	if err != nil {
		return receipt, err
	}
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return receipt, newError(errorInvalidInput, "cloudflareclef: could not create request")
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
			return receipt, newError(errorTimeout, "cloudflareclef: request timed out")
		}
		return receipt, newError(errorTransport, "cloudflareclef: request transport failed")
	}
	if response == nil {
		receipt.Duration = time.Since(started)
		return receipt, newError(errorTransport, "cloudflareclef: request transport failed")
	}
	receipt.HTTPStatus = response.StatusCode
	receipt.RequestID = safeRequestID(response.Header, c.apiKey)
	if declaredResponseTooLarge(response) {
		_ = closeResponse(response.Body)
		receipt.Duration = time.Since(started)
		return receipt, newError(errorResponseTooLarge, "cloudflareclef: response exceeds local limit")
	}
	responseBody, readErr, closeErr := readBoundedResponse(response)
	receipt.Duration = time.Since(started)
	if len(responseBody) > maxResponseBytes {
		return receipt, newError(errorResponseTooLarge, "cloudflareclef: response exceeds local limit")
	}
	if readErr != nil {
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(readErr) {
			return receipt, newError(errorTimeout, "cloudflareclef: request timed out")
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return receipt, newError(errorTruncated, "cloudflareclef: response was truncated")
		}
		return receipt, newError(errorTransport, "cloudflareclef: response could not be read")
	}
	if closeErr != nil {
		return receipt, newError(errorTransport, "cloudflareclef: response could not be closed")
	}
	if contentLengthMismatch(response, len(responseBody)) {
		return receipt, newError(errorTruncated, "cloudflareclef: response length did not match its declaration")
	}
	responseDigest := sha256.Sum256(responseBody)
	receipt.ResponseSHA256 = hex.EncodeToString(responseDigest[:])

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if envelope, envelopeErr := decodeEnvelope(responseBody); envelopeErr == nil {
			if result, ok := envelope.resultObject(); ok {
				decoded, total, totalValid, _ := decodeResult(result, request, c.limits)
				applyMetadata(&receipt, decoded, total, totalValid)
			}
		}
		return receipt, statusError(response.StatusCode)
	}

	envelope, err := decodeEnvelope(responseBody)
	if err != nil {
		return receipt, err
	}
	rootCategory, rootIncomplete := incompleteSignal(envelope.root)
	var decoded systemone.Response
	var total *int64
	var totalValid bool
	var decodeErr error
	if result, ok := envelope.resultObject(); ok {
		resultCategory, resultIncomplete := incompleteSignal(result)
		decoded, total, totalValid, decodeErr = decodeResult(result, request, c.limits)
		applyMetadata(&receipt, decoded, total, totalValid)
		if rootIncomplete {
			return receipt, newError(rootCategory, incompleteMessage(rootCategory))
		}
		if resultIncomplete {
			return receipt, newError(resultCategory, incompleteMessage(resultCategory))
		}
	} else if rootIncomplete {
		return receipt, newError(rootCategory, incompleteMessage(rootCategory))
	}
	if !envelope.success || len(envelope.errors) > 0 {
		return receipt, newError(errorUpstream, "cloudflareclef: provider reported an unsuccessful response")
	}
	if _, ok := envelope.resultObject(); !ok {
		return receipt, newError(errorMalformedResponse, "cloudflareclef: successful response result was invalid")
	}
	if decodeErr != nil {
		return receipt, decodeErr
	}
	receipt.Answers = decoded.Answers
	return receipt, nil
}

func validModel(model string) bool {
	return model == "clef" || model == "clef-flash"
}

func validAccountID(accountID string) bool {
	if len(accountID) < 1 || len(accountID) > 64 {
		return false
	}
	for _, character := range []byte(accountID) {
		letter := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		digit := character >= '0' && character <= '9'
		if !letter && !digit {
			return false
		}
	}
	return true
}

func validAPIKey(key string) bool {
	if key == "" {
		return false
	}
	for _, character := range []byte(key) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validLimits(limits systemone.Limits) bool {
	return limits.MaxStateBytes > 0 && limits.MaxRequestBytes > 0
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(errorTimeout, "cloudflareclef: request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return newError(errorCancelled, "cloudflareclef: request was canceled")
	}
	return newError(errorTransport, "cloudflareclef: request transport failed")
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newError(errorAuth, "cloudflareclef: provider rejected authentication")
	case http.StatusTooManyRequests:
		return newError(errorRateLimit, "cloudflareclef: provider rate limit reached")
	case http.StatusRequestEntityTooLarge:
		return newError(errorContextLimit, "cloudflareclef: provider rejected request size")
	default:
		return newError(errorUpstream, "cloudflareclef: provider returned an unsuccessful status")
	}
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

func readBoundedResponse(response *http.Response) ([]byte, error, error) {
	if response.Body == nil {
		return nil, nil, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := closeResponse(response.Body)
	return body, readErr, closeErr
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
	for _, name := range []string{"CF-Ray", "X-Request-ID", "X-Cf-Request-Id"} {
		value := header.Get(name)
		if value == "" || len(value) > maxRequestIDSize || strings.Contains(value, apiKey) {
			continue
		}
		safe := true
		for _, character := range []byte(value) {
			if !validRequestIDCharacter(character) {
				safe = false
				break
			}
		}
		if safe {
			return value
		}
	}
	return ""
}

func validRequestIDCharacter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", rune(character))
}

func incompleteMessage(category string) string {
	if category == errorContextLimit {
		return "cloudflareclef: provider reported a context limit"
	}
	if category == errorTruncated {
		return "cloudflareclef: provider reported incomplete output"
	}
	return "cloudflareclef: malformed response"
}
