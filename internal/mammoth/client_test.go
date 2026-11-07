package mammoth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "test-secret-api-key"

const expectedCancelledErrorCode = "cancelled" //nolint:misspell // Stable public error code is contract-defined.

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type closeTrackingBody struct {
	reader io.Reader
	closed bool
}

func (body *closeTrackingBody) Read(data []byte) (int, error) { return body.reader.Read(data) }

func (body *closeTrackingBody) Close() error {
	body.closed = true
	return nil
}

func successfulResponse(choice string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"answers":{"identity":{"type":"choice","choice":%q,"confidence":0.9,"probabilities":{"a":0.9,"b":0.08,"c":0.02}}},"usage":{"input_tokens":150,"output_tokens":10}}`, Model, choice))
}

func testHTTPResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}

func testClient(t *testing.T, timeout time.Duration, transport roundTripFunc) *Client {
	t.Helper()
	client, err := New(Options{
		APIKey:     testAPIKey,
		HTTPClient: &http.Client{Transport: transport},
		Timeout:    timeout,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestCompareChoiceMappingAndRequestContract(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first complete report")
	right := findingRecord("f-right", "src/b.go", "second complete report")
	plan, err := Prepare(TaskCohort, left, right)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tests := []struct {
		wireChoice string
		wantChoice Choice
	}{
		{wireChoice: "a", wantChoice: ChoiceSame},
		{wireChoice: "b", wantChoice: ChoiceDifferent},
		{wireChoice: "c", wantChoice: ChoiceAbstain},
	}
	for _, test := range tests {
		t.Run(string(test.wantChoice), func(t *testing.T) {
			body := successfulResponse(test.wireChoice)
			responseBody := &closeTrackingBody{reader: bytes.NewReader(body)}
			var calls atomic.Int32
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				if request.Method != http.MethodPost {
					t.Errorf("method = %q, want POST", request.Method)
				}
				if request.URL.String() != endpoint {
					t.Errorf("endpoint = %q, want fixed endpoint", request.URL.String())
				}
				if request.Header.Get("Authorization") != "Api-Key "+testAPIKey {
					t.Errorf("authorization scheme or key mismatch")
				}
				if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
					t.Errorf("content negotiation headers = %#v", request.Header)
				}
				requestBytes, readErr := io.ReadAll(request.Body)
				if readErr != nil {
					t.Errorf("read request body: %v", readErr)
				}
				requestDigest := fmt.Sprintf("%x", sha256.Sum256(requestBytes))
				if requestDigest != plan.RequestSHA256 {
					t.Errorf("request digest = %s, want plan digest %s", requestDigest, plan.RequestSHA256)
				}
				if bytes.Contains(requestBytes, []byte(testAPIKey)) {
					t.Errorf("request body contains API key")
				}
				response := testHTTPResponse(request, http.StatusOK, body)
				response.Body = responseBody
				response.Header.Set("X-Request-ID", "request-123")
				return response, nil
			})

			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err != nil {
				t.Fatalf("Compare: %v (%s)", err, ErrorCode(err))
			}
			if calls.Load() != 1 || !receipt.Attempted {
				t.Fatalf("calls = %d attempted = %v, want one attempted request", calls.Load(), receipt.Attempted)
			}
			if receipt.RequestedModel != Model || receipt.ReturnedModel != Model || receipt.RequestSHA256 != plan.RequestSHA256 {
				t.Fatalf("model or request receipt = %#v", receipt)
			}
			if receipt.HTTPStatus != http.StatusOK || receipt.RequestID != "request-123" || !responseBody.closed {
				t.Fatalf("response metadata or close = %#v closed=%v", receipt, responseBody.closed)
			}
			if receipt.Answer == nil || receipt.Answer.Choice != test.wantChoice {
				t.Fatalf("answer = %#v, want %q", receipt.Answer, test.wantChoice)
			}
			if receipt.Answer.Probabilities != [3]float64{0.9, 0.08, 0.02} || receipt.Answer.Confidence != 0.9 {
				t.Fatalf("answer scores = %#v", receipt.Answer)
			}
			if receipt.Usage == nil || *receipt.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) {
				t.Fatalf("usage = %#v", receipt.Usage)
			}
			wantResponseSHA := fmt.Sprintf("%x", sha256.Sum256(body))
			if receipt.ResponseSHA256 != wantResponseSHA {
				t.Fatalf("response digest = %s, want %s", receipt.ResponseSHA256, wantResponseSHA)
			}
			if receipt.Duration < 0 {
				t.Fatalf("duration = %v", receipt.Duration)
			}
		})
	}
}

func TestComparePriorThreadRecordAndPreflightLimitBeforeHTTP(t *testing.T) {
	left := findingRecord("f-prior", "src/file.go", "finding report")
	right := threadRecord("thread-prior", true, "src/file.go", "retained thread report")
	plan, err := Prepare(TaskPriorThreads, left, right)
	if err != nil {
		t.Fatalf("Prepare prior thread: %v", err)
	}
	var calls atomic.Int32
	var sent []byte
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		var readErr error
		sent, readErr = io.ReadAll(request.Body)
		if readErr != nil {
			return nil, readErr
		}
		return testHTTPResponse(request, http.StatusOK, successfulResponse("c")), nil
	})
	receipt, err := client.Compare(context.Background(), TaskPriorThreads, left, right)
	if err != nil {
		t.Fatalf("Compare prior thread: %v (%s)", err, ErrorCode(err))
	}
	if calls.Load() != 1 || receipt.RequestSHA256 != plan.RequestSHA256 || receipt.Answer == nil || receipt.Answer.Choice != ChoiceAbstain {
		t.Fatalf("prior-thread result = %#v calls=%d", receipt, calls.Load())
	}
	if !bytes.Contains(sent, []byte(`"kind":"thread","thread":{"thread_id":"thread-prior","eligible":true`)) || bytes.Contains(sent, []byte(`"thread":{"severity"`)) {
		t.Fatalf("prior-thread request has wrong record shape: %s", sent)
	}

	oversize := findingRecord("f-large", "src/large.go", stringsOf('x', maxStateBytes))
	calls.Store(0)
	receipt, err = client.Compare(context.Background(), TaskCohort, left, oversize)
	if err == nil || ErrorCode(err) != "context_limit" || receipt.Attempted || calls.Load() != 0 {
		t.Fatalf("oversize compare = %#v error=%v code=%s calls=%d", receipt, err, ErrorCode(err), calls.Load())
	}
}

func TestNewValidatesAndCopiesHTTPClient(t *testing.T) {
	redirectCalls := 0
	originalRedirect := func(*http.Request, []*http.Request) error {
		redirectCalls++
		return nil
	}
	original := &http.Client{Timeout: 9 * time.Second, CheckRedirect: originalRedirect, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := testHTTPResponse(request, http.StatusFound, nil)
		response.Header.Set("Location", "https://untrusted.example/redirect")
		return response, nil
	})}
	client, err := New(Options{APIKey: testAPIKey, HTTPClient: original, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.httpClient == original || client.httpClient.Timeout != 2*time.Second || original.Timeout != 9*time.Second || original.CheckRedirect == nil {
		t.Fatalf("injected HTTP client was not independently cloned")
	}
	if client.httpClient.CheckRedirect == nil {
		t.Fatal("redirects are not disabled")
	}
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
	if err == nil || ErrorCode(err) != "upstream" {
		t.Fatalf("redirect response error = %v (%s), want upstream status", err, ErrorCode(err))
	}
	if !receipt.Attempted || receipt.HTTPStatus != http.StatusFound || redirectCalls != 0 {
		t.Fatalf("redirect attempt = %#v, callback calls %d", receipt, redirectCalls)
	}
}

func TestNewDefaultsTimeoutToThirtySeconds(t *testing.T) {
	client, err := New(Options{APIKey: testAPIKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.timeout != 30*time.Second || client.httpClient.Timeout != 30*time.Second {
		t.Fatalf("default timeout = %v/%v, want 30s", client.timeout, client.httpClient.Timeout)
	}
}

func TestCompareDoesNotRetryTransportOrHTTPFailures(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	for _, test := range []struct {
		name       string
		transport  roundTripFunc
		wantCode   string
		wantStatus int
	}{
		{
			name: "transport error",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("transport exposed test-secret-api-key and response body")
			},
			wantCode: "transport",
		},
		{
			name: "server failure",
			transport: func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, http.StatusServiceUnavailable, []byte("server echoed test-secret-api-key")), nil
			},
			wantCode: "upstream", wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return test.transport(request)
			})
			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if calls.Load() != 1 || !receipt.Attempted || receipt.HTTPStatus != test.wantStatus {
				t.Fatalf("calls = %d receipt = %#v", calls.Load(), receipt)
			}
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "server echoed") {
				t.Fatalf("error leaked sensitive text: %v", err)
			}
			if strings.Contains(fmt.Sprint(receipt), testAPIKey) || strings.Contains(fmt.Sprint(receipt), "server echoed") {
				t.Fatalf("receipt leaked key or response text: %#v", receipt)
			}
			if test.wantStatus != 0 && receipt.ResponseSHA256 == "" {
				t.Fatal("complete bounded error response was not hashed")
			}
		})
	}
}

func TestCompareStatusErrorCategories(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	tests := []struct {
		status int
		code   string
	}{
		{status: http.StatusUnauthorized, code: "auth"},
		{status: http.StatusForbidden, code: "auth"},
		{status: http.StatusTooManyRequests, code: "rate_limit"},
		{status: http.StatusRequestEntityTooLarge, code: "context_limit"},
		{status: http.StatusBadRequest, code: "upstream"},
		{status: http.StatusInternalServerError, code: "upstream"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, test.status, []byte("provider diagnostic")), nil
			})
			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err == nil || ErrorCode(err) != test.code || receipt.HTTPStatus != test.status {
				t.Fatalf("status %d error=%v code=%s receipt=%#v; want %s", test.status, err, ErrorCode(err), receipt, test.code)
			}
			if strings.Contains(err.Error(), "provider diagnostic") {
				t.Fatalf("status error leaked body: %v", err)
			}
		})
	}
}

func TestComparePreservesSafeMetadataOnResponseErrors(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	valid := successfulResponse("a")
	withField := func(name, value string) []byte {
		return []byte(strings.Replace(string(valid), `,"usage"`, `,"`+name+`":`+value+`,"usage"`, 1))
	}
	tests := []struct {
		name      string
		status    int
		body      []byte
		wantCode  string
		wantModel bool
		wantUsage bool
	}{
		{name: "truncated output", status: http.StatusOK, body: withField("truncated", "true"), wantCode: "truncated", wantModel: true, wantUsage: true},
		{name: "context overflow", status: http.StatusOK, body: withField("context_overflow", "true"), wantCode: "context_limit", wantModel: true, wantUsage: true},
		{name: "truncated output without metadata", status: http.StatusOK, body: []byte(`{"truncated":true}`), wantCode: "truncated"},
		{name: "context overflow without metadata", status: http.StatusOK, body: []byte(`{"context_overflow":true}`), wantCode: "context_limit"},
		{name: "provider error", status: http.StatusOK, body: []byte(strings.Replace(string(valid), `,"answers"`, `,"error":{"detail":"synthetic"},"answers"`, 1)), wantCode: "upstream", wantModel: true, wantUsage: true},
		{name: "unauthorized status", status: http.StatusUnauthorized, body: valid, wantCode: "auth", wantModel: true, wantUsage: true},
		{name: "rate limit status", status: http.StatusTooManyRequests, body: valid, wantCode: "rate_limit", wantModel: true, wantUsage: true},
		{name: "upstream status", status: http.StatusServiceUnavailable, body: valid, wantCode: "upstream", wantModel: true, wantUsage: true},
		{name: "malformed body", status: http.StatusUnauthorized, body: []byte(`{"model":`), wantCode: "auth"},
		{name: "duplicate body key", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"model":"`+Model+`"`, `"model":"`+Model+`","model":"`+Model+`"`, 1)), wantCode: "auth"},
		{name: "mismatched model", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"model":"`+Model+`"`, `"model":"other-model"`, 1)), wantCode: "auth"},
		{name: "invalid usage", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"input_tokens":150`, `"input_tokens":null`, 1)), wantCode: "auth"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, test.status, test.body), nil
			})
			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("Compare error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if receipt.HTTPStatus != test.status || receipt.Answer != nil {
				t.Fatalf("error receipt status/answer = %d/%#v", receipt.HTTPStatus, receipt.Answer)
			}
			if (receipt.ReturnedModel == Model) != test.wantModel {
				t.Fatalf("returned model present = %v, want %v", receipt.ReturnedModel == Model, test.wantModel)
			}
			if (receipt.Usage != nil) != test.wantUsage {
				t.Fatalf("usage present = %v, want %v", receipt.Usage != nil, test.wantUsage)
			}
			if test.wantUsage && *receipt.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) {
				t.Fatalf("usage = %#v, want known provider usage", receipt.Usage)
			}
		})
	}
}

func TestCompareCancellationAndTimeoutAttemptSemantics(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")

	t.Run("pre-canceled", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, time.Second, func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected call")
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		receipt, err := client.Compare(ctx, TaskCohort, left, right)
		if err == nil || ErrorCode(err) != expectedCancelledErrorCode || receipt.Attempted || receipt.RequestSHA256 == "" || calls.Load() != 0 {
			t.Fatalf("pre-cancel result = %#v error=%v code=%s calls=%d", receipt, err, ErrorCode(err), calls.Load())
		}
	})

	t.Run("caller cancellation during request", func(t *testing.T) {
		var calls atomic.Int32
		started := make(chan struct{})
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		type compareResult struct {
			receipt Receipt
			err     error
		}
		completed := make(chan compareResult, 1)
		go func() {
			receipt, err := client.Compare(ctx, TaskCohort, left, right)
			completed <- compareResult{receipt: receipt, err: err}
		}()
		<-started
		cancel()
		outcome := <-completed
		receipt, err := outcome.receipt, outcome.err
		if err == nil || ErrorCode(err) != expectedCancelledErrorCode || !receipt.Attempted || calls.Load() != 1 {
			t.Fatalf("cancel result = %#v error=%v code=%s calls=%d", receipt, err, ErrorCode(err), calls.Load())
		}
	})

	t.Run("client timeout", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, 10*time.Millisecond, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
		if err == nil || ErrorCode(err) != "timeout" || !receipt.Attempted || calls.Load() != 1 {
			t.Fatalf("timeout result = %#v error=%v code=%s calls=%d", receipt, err, ErrorCode(err), calls.Load())
		}
	})
}

func TestNewAndCompareRejectInvalidInputsWithoutHTTP(t *testing.T) {
	for _, options := range []Options{
		{Timeout: time.Second},
		{APIKey: testAPIKey, Timeout: -time.Second},
		{APIKey: "bad\nkey", Timeout: time.Second},
	} {
		if client, err := New(options); err == nil || client != nil || ErrorCode(err) != "invalid_input" {
			t.Fatalf("New(%#v) = %v, %v", options, client, err)
		}
	}
	if ErrorCode(nil) != "" || ErrorCode(errors.New("unknown")) != "transport" {
		t.Fatal("ErrorCode did not return stable default categories")
	}
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	var calls atomic.Int32
	client := testClient(t, time.Second, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})
	//nolint:staticcheck // SA1012: nil is intentionally tested because Compare must reject a nil context.
	if receipt, err := client.Compare(nil, TaskCohort, left, right); err == nil || ErrorCode(err) != "invalid_input" || receipt.Attempted {
		t.Fatalf("nil context result = %#v error=%v", receipt, err)
	}
	if receipt, err := client.Compare(context.Background(), "unknown", left, right); err == nil || ErrorCode(err) != "invalid_input" || receipt.Attempted {
		t.Fatalf("invalid pair result = %#v error=%v", receipt, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs invoked HTTP %d times", calls.Load())
	}
}

func TestCompareRedactsAPIKeyFromRequestID(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	tests := []struct {
		name        string
		requestID   string
		wantRequest string
	}{
		{name: "exact API key", requestID: testAPIKey},
		{name: "API key embedded in request ID", requestID: "req-" + testAPIKey + "-tail"},
		{name: "ordinary request ID", requestID: "request-123", wantRequest: "request-123"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				response := testHTTPResponse(request, http.StatusUnauthorized, nil)
				response.Header.Set("X-Request-ID", test.requestID)
				return response, nil
			})
			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err == nil || ErrorCode(err) != "auth" {
				t.Fatalf("Compare error = %v (%s), want auth", err, ErrorCode(err))
			}
			if receipt.RequestID != test.wantRequest {
				t.Fatalf("receipt request ID = %q, want %q", receipt.RequestID, test.wantRequest)
			}
			serializedReceipt, marshalErr := json.Marshal(receipt)
			if marshalErr != nil {
				t.Fatalf("marshal receipt: %v", marshalErr)
			}
			serializedError, marshalErr := json.Marshal(err.Error())
			if marshalErr != nil {
				t.Fatalf("marshal error: %v", marshalErr)
			}
			if bytes.Contains(serializedReceipt, []byte(testAPIKey)) || bytes.Contains(serializedError, []byte(testAPIKey)) {
				t.Fatalf("serialized receipt or error contains API key: receipt=%s error=%s", serializedReceipt, serializedError)
			}
		})
	}
}
