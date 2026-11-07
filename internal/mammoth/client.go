package mammoth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
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

// Options configures key injection, request timeout, and a test transport.
type Options struct {
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Client sends fixed protocol-v1 identity requests to the Baseten endpoint.
type Client struct {
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
}

// Receipt records sanitized metadata for one attempted comparison.
type Receipt struct {
	RequestedModel string        `json:"requested_model"`
	ReturnedModel  string        `json:"returned_model,omitempty"`
	RequestSHA256  string        `json:"request_sha256"`
	ResponseSHA256 string        `json:"response_sha256,omitempty"`
	RequestID      string        `json:"request_id,omitempty"`
	Answer         *Answer       `json:"answer,omitempty"`
	Usage          *Usage        `json:"usage,omitempty"`
	Duration       time.Duration `json:"duration"`
	Attempted      bool          `json:"attempted"`
	HTTPStatus     int           `json:"http_status,omitempty"`
}

// New constructs a client. The injected HTTP client is cloned before redirect
// handling or timeout settings are changed.
func New(options Options) (*Client, error) {
	if options.APIKey == "" || !validHeaderValue(options.APIKey) || options.Timeout < 0 {
		return nil, newError(errorInvalidInput, "mammoth: invalid client options")
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

	return &Client{apiKey: options.APIKey, httpClient: httpClient, timeout: timeout}, nil
}

// Compare sends one complete finding-identity pair and returns a normalized
// answer plus a sanitized receipt, including partial metadata on failure.
func (c *Client) Compare(ctx context.Context, task Task, left, right Record) (Receipt, error) {
	receipt := Receipt{RequestedModel: Model}
	if ctx == nil || c == nil || c.httpClient == nil {
		return receipt, newError(errorInvalidInput, "mammoth: invalid comparison")
	}
	plan, requestBody, err := prepare(task, left, right)
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
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return receipt, newError(errorInvalidInput, "mammoth: could not create request")
	}
	request.Header.Set("Authorization", "Api-Key "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	receipt.Attempted = true
	response, err := c.httpClient.Do(request)
	if err != nil {
		receipt.Duration = time.Since(started)
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(err) {
			return receipt, newError(errorTimeout, "mammoth: request timed out")
		}
		return receipt, newError(errorTransport, "mammoth: request transport failed")
	}
	if response == nil {
		receipt.Duration = time.Since(started)
		return receipt, newError(errorTransport, "mammoth: request transport failed")
	}
	receipt.HTTPStatus = response.StatusCode
	receipt.RequestID = safeRequestID(response.Header, c.apiKey)
	if declaredResponseTooLarge(response) {
		_ = closeResponse(response.Body)
		receipt.Duration = time.Since(started)
		return receipt, newError(errorResponseTooLarge, "mammoth: response exceeds local limit")
	}
	responseBody, readErr, closeErr := readBoundedResponse(response)
	receipt.Duration = time.Since(started)
	if len(responseBody) > maxResponseBytes {
		return receipt, newError(errorResponseTooLarge, "mammoth: response exceeds local limit")
	}
	if readErr != nil {
		if requestContext.Err() != nil {
			return receipt, contextError(requestContext.Err())
		}
		if isTimeout(readErr) {
			return receipt, newError(errorTimeout, "mammoth: request timed out")
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return receipt, newError(errorTruncated, "mammoth: response was truncated")
		}
		return receipt, newError(errorTransport, "mammoth: response could not be read")
	}
	if closeErr != nil {
		return receipt, newError(errorTransport, "mammoth: response could not be closed")
	}
	if contentLengthMismatch(response, len(responseBody)) {
		return receipt, newError(errorTruncated, "mammoth: response length did not match its declaration")
	}
	responseDigest := sha256.Sum256(responseBody)
	receipt.ResponseSHA256 = fmt.Sprintf("%x", responseDigest)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		receipt.ReturnedModel, receipt.Usage = responseMetadata(responseBody)
		return receipt, statusError(response.StatusCode)
	}
	answer, returnedModel, usage, parseErr := parseResponse(responseBody)
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

func validHeaderValue(value string) bool {
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return value != ""
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(errorTimeout, "mammoth: request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return newError(errorCancelled, "mammoth: request was canceled")
	}
	return newError(errorTransport, "mammoth: request transport failed")
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newError(errorAuth, "mammoth: provider rejected authentication")
	case http.StatusTooManyRequests:
		return newError(errorRateLimit, "mammoth: provider rate limit reached")
	case http.StatusRequestEntityTooLarge:
		return newError(errorContextLimit, "mammoth: provider rejected request size")
	default:
		return newError(errorUpstream, "mammoth: provider returned an unsuccessful status")
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

func parseResponse(data []byte) (*Answer, string, *Usage, error) {
	root, model, usage, metadataErr := decodeResponseMetadata(data)
	if root == nil {
		return nil, "", nil, metadataErr
	}
	if category, failed := incompleteSignal(root); failed {
		if category == errorContextLimit {
			return nil, model, usage, newError(category, "mammoth: provider reported a context limit")
		}
		if category == errorTruncated {
			return nil, model, usage, newError(category, "mammoth: provider reported incomplete output")
		}
		return nil, model, usage, newError(errorMalformedResponse, "mammoth: malformed response")
	}
	if rawError, exists := root["error"]; exists && !isJSONNull(rawError) {
		return nil, model, usage, newError(errorUpstream, "mammoth: provider reported an error")
	}
	if metadataErr != nil {
		return nil, model, usage, metadataErr
	}
	answer, ok := parseAnswer(root["answers"])
	if !ok {
		return nil, model, usage, newError(errorMalformedResponse, "mammoth: response answer was invalid")
	}
	return &answer, model, usage, nil
}

func decodeResponseMetadata(data []byte) (map[string]json.RawMessage, string, *Usage, error) {
	if len(data) == 0 || !utf8.Valid(data) || !json.Valid(data) || rejectDuplicateKeys(data) != nil {
		return nil, "", nil, newError(errorMalformedResponse, "mammoth: malformed response")
	}
	root, ok := object(data)
	if !ok {
		return nil, "", nil, newError(errorMalformedResponse, "mammoth: malformed response")
	}
	model, ok := jsonString(root["model"])
	if !ok || model != Model {
		return root, "", nil, newError(errorMalformedResponse, "mammoth: response model did not match request")
	}
	usage, ok := parseUsage(root["usage"])
	if !ok {
		return root, model, nil, newError(errorMalformedResponse, "mammoth: response usage was invalid")
	}
	return root, model, &usage, nil
}

func responseMetadata(data []byte) (string, *Usage) {
	_, model, usage, err := decodeResponseMetadata(data)
	if err != nil || usage == nil {
		return "", nil
	}
	return model, usage
}

func parseUsage(raw json.RawMessage) (Usage, bool) {
	fields, ok := object(raw)
	if !ok || !hasOnlyFields(fields, "input_tokens", "output_tokens") {
		return Usage{}, false
	}
	inputTokens, ok := jsonInt64(fields["input_tokens"])
	if !ok {
		return Usage{}, false
	}
	outputTokens, ok := jsonInt64(fields["output_tokens"])
	if !ok || inputTokens < 0 || outputTokens < 0 {
		return Usage{}, false
	}
	return Usage{InputTokens: inputTokens, OutputTokens: outputTokens}, true
}

func parseAnswer(raw json.RawMessage) (Answer, bool) {
	answers, ok := object(raw)
	if !ok || len(answers) != 1 {
		return Answer{}, false
	}
	identity, exists := answers["identity"]
	if !exists {
		return Answer{}, false
	}
	fields, ok := object(identity)
	if !ok || !hasOnlyFields(fields, "type", "choice", "confidence", "probabilities") {
		return Answer{}, false
	}
	typeValue, ok := jsonString(fields["type"])
	if !ok || typeValue != "choice" {
		return Answer{}, false
	}
	choiceCode, ok := jsonString(fields["choice"])
	if !ok {
		return Answer{}, false
	}
	var choice Choice
	switch choiceCode {
	case "a":
		choice = ChoiceSame
	case "b":
		choice = ChoiceDifferent
	case "c":
		choice = ChoiceAbstain
	default:
		return Answer{}, false
	}
	confidence, ok := jsonFloat64(fields["confidence"])
	if !ok || !finiteUnit(confidence) {
		return Answer{}, false
	}
	probabilityFields, ok := object(fields["probabilities"])
	if !ok || !hasOnlyFields(probabilityFields, "a", "b", "c") {
		return Answer{}, false
	}
	var probabilities [3]float64
	for index, label := range [...]string{"a", "b", "c"} {
		probability, valid := jsonFloat64(probabilityFields[label])
		if !valid || !finiteUnit(probability) {
			return Answer{}, false
		}
		probabilities[index] = probability
	}
	probabilitySum := probabilities[0] + probabilities[1] + probabilities[2]
	if probabilitySum < 0.98 || probabilitySum > 1.02 {
		return Answer{}, false
	}
	return Answer{Choice: choice, Probabilities: probabilities, Confidence: confidence}, true
}

func incompleteSignal(root map[string]json.RawMessage) (errorCategory, bool) {
	for _, name := range []string{"context_overflow", "context_limit_exceeded", "input_too_long", "prompt_too_long"} {
		if raw, exists := root[name]; exists {
			flag, valid := jsonBool(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			if flag {
				return errorContextLimit, true
			}
		}
	}
	for _, name := range []string{"truncated", "partial", "incomplete", "output_truncated"} {
		if raw, exists := root[name]; exists {
			flag, valid := jsonBool(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			if flag {
				return errorTruncated, true
			}
		}
	}
	for _, name := range []string{"finish_reason", "status"} {
		if raw, exists := root[name]; exists {
			value, valid := jsonString(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			switch strings.ToLower(value) {
			case "context_limit", "context_overflow", "context_length", "context_length_exceeded", "input_too_long", "prompt_too_long":
				return errorContextLimit, true
			case "length", "max_tokens", "truncated", "partial", "incomplete":
				return errorTruncated, true
			}
		}
	}
	if raw, exists := root["incomplete_details"]; exists && !isJSONNull(raw) {
		return errorTruncated, true
	}
	return "", false
}

func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func hasOnlyFields(fields map[string]json.RawMessage, required ...string) bool {
	if len(fields) != len(required) {
		return false
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return false
		}
	}
	return true
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func jsonInt64(raw json.RawMessage) (int64, bool) {
	var value int64
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func jsonFloat64(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func finiteUnit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := scanJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("invalid delimiter")
	}
	return nil
}

var _ = fmt.Sprintf
