package openaidecisions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAPIKey   = "test-secret-key"
	testEndpoint = "https://api.example.test/v1/decisions"
)

var testLimits = Limits{MaxInputBytes: 4096, MaxRequestBytes: 8192}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type closeTrackingBody struct {
	reader    io.Reader
	closed    bool
	closeErr  error
	readError error
}

func (body *closeTrackingBody) Read(destination []byte) (int, error) {
	if body.readError != nil {
		return 0, body.readError
	}
	return body.reader.Read(destination)
}

func (body *closeTrackingBody) Close() error {
	body.closed = true
	return body.closeErr
}

func choiceRequest() Request {
	return Request{
		Model:        "provider/model-one",
		Input:        `Choose the route for <item> & "notes".`,
		Name:         "route",
		Instructions: "Select the route that matches the input.",
		Choices: []Choice{
			{Value: "merge", Description: "Combine the changes."},
			{Value: "separate", Description: "Keep them independent."},
		},
	}
}

func successBody(model string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"answers":[{"type":"choice","name":"route","choice":"merge","confidence":0.95,"probabilities":[{"value":"merge","probability":0.95},{"value":"separate","probability":0.05}]}],"usage":{"input_tokens":32,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":35}}`, model))
}

func response(request *http.Request, status int, body []byte) *http.Response {
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
		Endpoint:   testEndpoint,
		APIKey:     testAPIKey,
		HTTPClient: &http.Client{Transport: transport},
		Timeout:    timeout,
		Limits:     testLimits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestPrepareEmitsExactCompactWireRequestAndLocalHash(t *testing.T) {
	request := choiceRequest()
	request.Input = "é <item> & \"notes\""
	plan, err := Prepare(request, testLimits)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := `{"model":"provider/model-one","input":"é <item> & \"notes\"","questions":[{"type":"choice","name":"route","instructions":"Select the route that matches the input.","choices":[{"value":"merge","description":"Combine the changes."},{"value":"separate","description":"Keep them independent."}]}]}`
	prepared, body, err := prepare(request, testLimits)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if string(body) != want {
		t.Fatalf("request body mismatch\n got: %s\nwant: %s", body, want)
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		t.Fatal("request body has a trailing newline")
	}
	digest := sha256.Sum256([]byte(want))
	wantHash := hex.EncodeToString(digest[:])
	if plan != prepared || plan.RequestSHA256 != wantHash || plan.InputBytes != len([]byte(request.Input)) || plan.RequestBytes != len(want) {
		t.Fatalf("plan = %#v, want hash=%s input_bytes=%d request_bytes=%d", plan, wantHash, len([]byte(request.Input)), len(want))
	}
	if plan.InputBytes != 19 {
		t.Fatalf("UTF-8 input size = %d, want 19", plan.InputBytes)
	}

	request.ExpectedModel = "provider/model-resolved"
	withLocalPolicy, err := Prepare(request, testLimits)
	if err != nil {
		t.Fatalf("Prepare with expected model: %v", err)
	}
	if withLocalPolicy.RequestSHA256 != plan.RequestSHA256 {
		t.Fatal("local expected-model policy changed the wire request hash")
	}
	for _, forbidden := range []string{"service_tier", "cache_policy", "ExpectedModel"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("wire request contains unexpected field or fixed content %q: %s", forbidden, body)
		}
	}
}

func TestPrepareValidatesInputChoicesAndBothLimits(t *testing.T) {
	base := choiceRequest()
	tests := []struct {
		name    string
		change  func(*Request)
		limits  Limits
		wantErr string
	}{
		{name: "empty model", change: func(r *Request) { r.Model = "  " }, limits: testLimits, wantErr: "invalid_input"},
		{name: "empty expected model", change: func(r *Request) { r.ExpectedModel = "\t" }, limits: testLimits, wantErr: "invalid_input"},
		{name: "empty name", change: func(r *Request) { r.Name = " " }, limits: testLimits, wantErr: "invalid_input"},
		{name: "empty instructions", change: func(r *Request) { r.Instructions = "\n" }, limits: testLimits, wantErr: "invalid_input"},
		{name: "one choice", change: func(r *Request) { r.Choices = r.Choices[:1] }, limits: testLimits, wantErr: "invalid_input"},
		{name: "duplicate choice value", change: func(r *Request) { r.Choices[1].Value = r.Choices[0].Value }, limits: testLimits, wantErr: "invalid_input"},
		{name: "empty choice value", change: func(r *Request) { r.Choices[0].Value = " " }, limits: testLimits, wantErr: "invalid_input"},
		{name: "empty choice description", change: func(r *Request) { r.Choices[0].Description = "" }, limits: testLimits, wantErr: "invalid_input"},
		{name: "invalid limits", limits: Limits{MaxInputBytes: 0, MaxRequestBytes: 10}, wantErr: "invalid_input"},
		{name: "input over limit", limits: Limits{MaxInputBytes: 4, MaxRequestBytes: 8192}, wantErr: "context_limit"},
		{name: "wire request over limit", limits: Limits{MaxInputBytes: 4096, MaxRequestBytes: 32}, wantErr: "context_limit"},
		{name: "invalid UTF-8 input", change: func(r *Request) { r.Input = string([]byte{0xff}) }, limits: testLimits, wantErr: "invalid_input"},
		{name: "invalid UTF-8 value", change: func(r *Request) { r.Choices[0].Value = string([]byte{0xff}) }, limits: testLimits, wantErr: "invalid_input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Choices = append([]Choice(nil), base.Choices...)
			if test.change != nil {
				test.change(&request)
			}
			_, err := Prepare(request, test.limits)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if err == nil || ErrorCode(err) != test.wantErr {
				t.Fatalf("Prepare error = %v (%s), want %s", err, ErrorCode(err), test.wantErr)
			}
		})
	}
	tooMany := base
	tooMany.Choices = make([]Choice, 256)
	for i := range tooMany.Choices {
		tooMany.Choices[i] = Choice{Value: fmt.Sprintf("v%d", i), Description: "A choice."}
	}
	if _, err := Prepare(tooMany, testLimits); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("Prepare with too many choices = %v (%s), want invalid_input", err, ErrorCode(err))
	}
}

func TestNewValidatesOptionsAndClonesHTTPClient(t *testing.T) {
	original := &http.Client{Timeout: time.Hour}
	client, err := New(Options{Endpoint: testEndpoint, APIKey: testAPIKey, HTTPClient: original, Limits: testLimits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.httpClient == original || client.httpClient.Timeout != defaultTimeout || original.Timeout != time.Hour {
		t.Fatalf("HTTP client clone/timeout incorrect: clone=%p timeout=%s original_timeout=%s", client.httpClient, client.httpClient.Timeout, original.Timeout)
	}
	if client.timeout != defaultTimeout {
		t.Fatalf("timeout = %s, want %s", client.timeout, defaultTimeout)
	}
	invalid := []Options{
		{Endpoint: "http://api.example.test/decisions", APIKey: testAPIKey, Limits: testLimits},
		{Endpoint: "https://user:pass@api.example.test/decisions", APIKey: testAPIKey, Limits: testLimits},
		{Endpoint: "https://api.example.test/decisions?x=1", APIKey: testAPIKey, Limits: testLimits},
		{Endpoint: "https://api.example.test/decisions?", APIKey: testAPIKey, Limits: testLimits},
		{Endpoint: "https://api.example.test/decisions#fragment", APIKey: testAPIKey, Limits: testLimits},
		{Endpoint: testEndpoint, APIKey: "", Limits: testLimits},
		{Endpoint: testEndpoint, APIKey: "with space", Limits: testLimits},
		{Endpoint: testEndpoint, APIKey: "key\r\nInjected: x", Limits: testLimits},
		{Endpoint: testEndpoint, APIKey: testAPIKey, Timeout: -time.Second, Limits: testLimits},
		{Endpoint: testEndpoint, APIKey: testAPIKey, Limits: Limits{MaxInputBytes: 0, MaxRequestBytes: 1}},
	}
	for i, options := range invalid {
		if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("New invalid option %d = %v (%s), want invalid_input", i, err, ErrorCode(err))
		}
	}
}

func TestDecideSendsBearerRequestAndReturnsValidatedReceipt(t *testing.T) {
	body := successBody("provider/model-one")
	responseBody := &closeTrackingBody{reader: bytes.NewReader(body)}
	var calls atomic.Int32
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.String() != testEndpoint {
			t.Errorf("request target = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		requestBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		plan, err := Prepare(choiceRequest(), testLimits)
		if err != nil {
			t.Errorf("Prepare request: %v", err)
		} else if got := sha256.Sum256(requestBody); hex.EncodeToString(got[:]) != plan.RequestSHA256 {
			t.Errorf("sent body hash differs from prepared request")
		}
		response := response(request, http.StatusOK, nil)
		response.Body = responseBody
		response.ContentLength = int64(len(body))
		response.Header.Set("X-Request-ID", "req-123")
		return response, nil
	})
	receipt, err := client.Decide(context.Background(), choiceRequest())
	if err != nil {
		t.Fatalf("Decide: %v (%s)", err, ErrorCode(err))
	}
	if calls.Load() != 1 || !responseBody.closed {
		t.Fatalf("calls=%d body_closed=%t", calls.Load(), responseBody.closed)
	}
	if !receipt.Attempted || receipt.HTTPStatus != http.StatusOK || receipt.Duration <= 0 || receipt.RequestID != "req-123" {
		t.Fatalf("receipt attempt metadata = %#v", receipt)
	}
	wantRequest, _ := Prepare(choiceRequest(), testLimits)
	wantResponseHash := sha256.Sum256(body)
	if receipt.RequestSHA256 != wantRequest.RequestSHA256 || receipt.ResponseSHA256 != hex.EncodeToString(wantResponseHash[:]) || receipt.RequestedModel != "provider/model-one" || receipt.ReturnedModel != "provider/model-one" {
		t.Fatalf("receipt hashes/model = %#v", receipt)
	}
	if receipt.Answer == nil || receipt.Answer.Choice != "merge" || receipt.Answer.Confidence != 0.95 || len(receipt.Answer.Probabilities) != 2 || receipt.Answer.Probabilities["separate"] != 0.05 {
		t.Fatalf("answer = %#v", receipt.Answer)
	}
	if receipt.Usage == nil || receipt.Usage.InputTokens != 32 || receipt.Usage.OutputTokens != 3 || receipt.Usage.CachedInputTokens == nil || *receipt.Usage.CachedInputTokens != 4 || receipt.Usage.CacheWriteTokens == nil || *receipt.Usage.CacheWriteTokens != 2 {
		t.Fatalf("usage = %#v", receipt.Usage)
	}
}

func TestDecideUsesExpectedModelOnlyAsLocalResponsePolicy(t *testing.T) {
	request := choiceRequest()
	request.Model = "provider/model-alias"
	request.ExpectedModel = "provider/model-resolved"
	body := successBody(request.ExpectedModel)
	client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		requestBody, _ := io.ReadAll(httpRequest.Body)
		if bytes.Contains(requestBody, []byte(request.ExpectedModel)) || bytes.Contains(requestBody, []byte("expected_model")) {
			t.Errorf("local expected model was sent: %s", requestBody)
		}
		return response(httpRequest, http.StatusOK, body), nil
	})
	receipt, err := client.Decide(context.Background(), request)
	if err != nil || receipt.ReturnedModel != request.ExpectedModel || receipt.Answer == nil {
		t.Fatalf("Decide alias result: returned=%q answer=%#v err=%v (%s)", receipt.ReturnedModel, receipt.Answer, err, ErrorCode(err))
	}
}

func TestDecideRefusalPreservesValidatedMetadata(t *testing.T) {
	body := []byte(`{"model":"provider/model-one","answers":[{"type":"refusal","name":"route"}],"usage":{"input_tokens":7,"output_tokens":0}}`)
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, body), nil
	})
	receipt, err := client.Decide(context.Background(), choiceRequest())
	if err == nil || ErrorCode(err) != "refused" || receipt.Answer != nil {
		t.Fatalf("refusal = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
	if receipt.ReturnedModel != "provider/model-one" || receipt.Usage == nil || receipt.Usage.InputTokens != 7 || receipt.ResponseSHA256 == "" {
		t.Fatalf("refusal metadata was lost: %#v", receipt)
	}
	if strings.Contains(err.Error(), "route") || strings.Contains(err.Error(), "refusal") && strings.Contains(err.Error(), "provider prose") {
		t.Fatalf("refusal error exposed provider content: %v", err)
	}
}

func TestDecidePreservesMetadataOnBadAnswerNon2xxAndIncomplete(t *testing.T) {
	valid := string(successBody("provider/model-one"))
	tests := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "invalid answer", status: http.StatusOK, body: strings.Replace(valid, `"choice":"merge"`, `"choice":true`, 1), code: "malformed_response"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: valid, code: "rate_limit"},
		{name: "incomplete", status: http.StatusOK, body: strings.Replace(valid, `,"usage"`, `,"truncated":true,"usage"`, 1), code: "truncated"},
		{name: "context overflow", status: http.StatusOK, body: strings.Replace(valid, `,"usage"`, `,"context_overflow":true,"usage"`, 1), code: "context_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				return response(request, test.status, []byte(test.body)), nil
			})
			receipt, err := client.Decide(context.Background(), choiceRequest())
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("Decide error = %v (%s), want %s", err, ErrorCode(err), test.code)
			}
			if receipt.ReturnedModel != "provider/model-one" || receipt.Usage == nil || receipt.Answer != nil || receipt.ResponseSHA256 == "" {
				t.Fatalf("validated metadata/response hash missing: %#v", receipt)
			}
		})
	}
}

func TestDecideDropsMetadataForMismatchedModel(t *testing.T) {
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, successBody("provider/other-model")), nil
	})
	receipt, err := client.Decide(context.Background(), choiceRequest())
	if err == nil || ErrorCode(err) != "malformed_response" || receipt.ReturnedModel != "" || receipt.Usage != nil || receipt.Answer != nil {
		t.Fatalf("mismatched model metadata = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
}

func TestDecideRequestIDIsRedactedAndSafeErrorsDoNotExposeSecrets(t *testing.T) {
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		result := response(request, http.StatusUnauthorized, []byte(`{"error":"provider text contains test-secret-key"}`))
		result.Header.Set("X-Request-ID", "req-test-secret-key")
		return result, nil
	})
	receipt, err := client.Decide(context.Background(), choiceRequest())
	if err == nil || ErrorCode(err) != "auth" || receipt.RequestID != "" {
		t.Fatalf("auth result = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
	if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "provider text") {
		t.Fatalf("error exposed secret/provider text: %v", err)
	}
	if receipt.ResponseSHA256 == "" {
		t.Fatal("complete bounded error response was not hashed")
	}
}

func TestDecideHonorsCancellationTimeoutAndDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	cancelCtx, cancel := context.WithCancel(context.Background())
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	receipt, err := client.Decide(cancelCtx, choiceRequest())
	if err == nil || ErrorCode(err) != "cancelled" || !receipt.Attempted || calls.Load() != 1 {
		t.Fatalf("cancel result = %#v, %v (%s), calls=%d", receipt, err, ErrorCode(err), calls.Load())
	}

	var timeoutCalls atomic.Int32
	timeoutClient := testClient(t, 10*time.Millisecond, func(request *http.Request) (*http.Response, error) {
		timeoutCalls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	timeoutReceipt, timeoutErr := timeoutClient.Decide(context.Background(), choiceRequest())
	if timeoutErr == nil || ErrorCode(timeoutErr) != "timeout" || !timeoutReceipt.Attempted || timeoutCalls.Load() != 1 {
		t.Fatalf("timeout result = %#v, %v (%s), calls=%d", timeoutReceipt, timeoutErr, ErrorCode(timeoutErr), timeoutCalls.Load())
	}

	var retryCalls atomic.Int32
	retryClient := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		retryCalls.Add(1)
		return response(request, http.StatusServiceUnavailable, []byte(`{"error":"unavailable"}`)), nil
	})
	if _, retryErr := retryClient.Decide(context.Background(), choiceRequest()); retryErr == nil || ErrorCode(retryErr) != "upstream" {
		t.Fatalf("retry fixture error = %v (%s)", retryErr, ErrorCode(retryErr))
	}
	if retryCalls.Load() != 1 {
		t.Fatalf("request was retried %d times", retryCalls.Load()-1)
	}
}

func TestDecideDoesNotFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		result := response(request, http.StatusFound, []byte(`{"location":"private"}`))
		result.Header.Set("Location", "https://other.example.test/next")
		return result, nil
	})
	receipt, err := client.Decide(context.Background(), choiceRequest())
	if err == nil || ErrorCode(err) != "upstream" || receipt.HTTPStatus != http.StatusFound || calls.Load() != 1 {
		t.Fatalf("redirect result = %#v, %v (%s), calls=%d", receipt, err, ErrorCode(err), calls.Load())
	}
}

func TestDecideBoundsReadsAndHandlesBodyErrorsAndClose(t *testing.T) {
	t.Run("declared too large", func(t *testing.T) {
		body := &closeTrackingBody{reader: bytes.NewReader([]byte("unused"))}
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			result := response(request, http.StatusOK, nil)
			result.Body = body
			result.ContentLength = maxResponseBytes + 1
			return result, nil
		})
		receipt, err := client.Decide(context.Background(), choiceRequest())
		if err == nil || ErrorCode(err) != "response_too_large" || !body.closed || receipt.ResponseSHA256 != "" {
			t.Fatalf("oversized result = %#v, %v (%s), body_closed=%t", receipt, err, ErrorCode(err), body.closed)
		}
	})
	t.Run("streamed too large", func(t *testing.T) {
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			result := response(request, http.StatusOK, bytes.Repeat([]byte("x"), maxResponseBytes+1))
			result.ContentLength = -1
			return result, nil
		})
		receipt, err := client.Decide(context.Background(), choiceRequest())
		if err == nil || ErrorCode(err) != "response_too_large" || receipt.ResponseSHA256 != "" {
			t.Fatalf("streamed oversized result = %#v, %v (%s)", receipt, err, ErrorCode(err))
		}
	})
	t.Run("read error", func(t *testing.T) {
		body := &closeTrackingBody{readError: errors.New("private read failure")}
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			result := response(request, http.StatusOK, nil)
			result.Body = body
			return result, nil
		})
		receipt, err := client.Decide(context.Background(), choiceRequest())
		if err == nil || ErrorCode(err) != "transport" || !body.closed || receipt.ResponseSHA256 != "" || strings.Contains(err.Error(), "private") {
			t.Fatalf("read error result = %#v, %v (%s), closed=%t", receipt, err, ErrorCode(err), body.closed)
		}
	})
	t.Run("close error", func(t *testing.T) {
		body := &closeTrackingBody{reader: bytes.NewReader(successBody("provider/model-one")), closeErr: errors.New("private close failure")}
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			result := response(request, http.StatusOK, nil)
			result.Body = body
			result.ContentLength = int64(len(successBody("provider/model-one")))
			return result, nil
		})
		receipt, err := client.Decide(context.Background(), choiceRequest())
		if err == nil || ErrorCode(err) != "transport" || !body.closed || receipt.ResponseSHA256 != "" || strings.Contains(err.Error(), "private") {
			t.Fatalf("close error result = %#v, %v (%s), closed=%t", receipt, err, ErrorCode(err), body.closed)
		}
	})
	t.Run("length mismatch", func(t *testing.T) {
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			body := successBody("provider/model-one")
			result := response(request, http.StatusOK, body)
			result.ContentLength = int64(len(body) + 1)
			return result, nil
		})
		receipt, err := client.Decide(context.Background(), choiceRequest())
		if err == nil || ErrorCode(err) != "truncated" || receipt.ResponseSHA256 != "" {
			t.Fatalf("length mismatch result = %#v, %v (%s)", receipt, err, ErrorCode(err))
		}
	})
}

func TestErrorCodeIsStableAndUnknownErrorsAreSanitized(t *testing.T) {
	if ErrorCode(nil) != "" || ErrorCode(errors.New("secret details")) != "transport" {
		t.Fatalf("unexpected ErrorCode behavior: nil=%q unknown=%q", ErrorCode(nil), ErrorCode(errors.New("secret details")))
	}
	if ErrorCode(newError(errorRefused, "static refusal")) != "refused" {
		t.Fatal("refusal error code was not stable")
	}
}
