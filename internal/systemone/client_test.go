package systemone

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

const (
	testAPIKey         = "test-secret-api-key"
	expectedCancelCode = "cancelled" //nolint:misspell // Stable public error code is contract-defined.
	testEndpoint       = "https://systemone.example.test/v1/decide"
)

var testLimits = Limits{MaxStateBytes: 6 * 1024, MaxRequestBytes: 8 * 1024}

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

type unexpectedEOFBody struct {
	data   []byte
	read   bool
	closed bool
}

func (body *unexpectedEOFBody) Read(destination []byte) (int, error) {
	if body.read {
		return 0, io.ErrUnexpectedEOF
	}
	body.read = true
	return copy(destination, body.data), nil
}

func (body *unexpectedEOFBody) Close() error {
	body.closed = true
	return nil
}

func genericRequest() Request {
	return Request{
		Model: "provider/model-one",
		State: json.RawMessage(`{"value":"<x> & \"q\"","count":2}`),
		Questions: map[string]ChoiceQuestion{
			"alpha": {
				Instructions: "Choose the matching action.",
				Criteria: map[string]string{
					"merge":    "the actions are equivalent",
					"separate": "the actions remain independent",
					"unknown":  "the evidence is incomplete",
				},
			},
			"beta": {
				Instructions: "Choose the resulting state.",
				Criteria: map[string]string{
					"enabled":  "the feature is enabled",
					"disabled": "the feature is disabled",
				},
			},
		},
	}
}

func successfulResponse(model string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"answers":{"alpha":{"type":"choice","choice":"merge","confidence":0.9,"probabilities":{"merge":0.9,"separate":0.08,"unknown":0.02}},"beta":{"type":"choice","choice":"enabled","confidence":0.8,"probabilities":{"enabled":0.75,"disabled":0.25}}},"usage":{"input_tokens":150,"output_tokens":10}}`, model))
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

func testClient(t *testing.T, scheme string, timeout time.Duration, transport roundTripFunc) *Client {
	t.Helper()
	client, err := New(Options{
		Endpoint:   testEndpoint,
		APIKey:     testAPIKey,
		AuthScheme: scheme,
		HTTPClient: &http.Client{Transport: transport},
		Timeout:    timeout,
		Limits:     testLimits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestPrepareGoldenGenericRequestAndLocalHashPolicy(t *testing.T) {
	request := genericRequest()
	plan, body, err := prepare(request, testLimits)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := `{"model":"provider/model-one","state":{"value":"<x> & \"q\"","count":2},"questions":{"alpha":{"type":"choice","instructions":"Choose the matching action.","criteria":{"merge":"the actions are equivalent","separate":"the actions remain independent","unknown":"the evidence is incomplete"}},"beta":{"type":"choice","instructions":"Choose the resulting state.","criteria":{"disabled":"the feature is disabled","enabled":"the feature is enabled"}}}}`
	if string(body) != want {
		t.Fatalf("request body mismatch\n got: %s\nwant: %s", body, want)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(want)))
	if plan.RequestSHA256 != wantHash || plan.RequestBytes != len(want) || plan.StateBytes != len(request.State) {
		t.Fatalf("plan = %#v, want hash=%s request_bytes=%d state_bytes=%d", plan, wantHash, len(want), len(request.State))
	}

	secondModel := request
	secondModel.Model = "provider/model-two"
	secondPlan, _, err := prepare(secondModel, testLimits)
	if err != nil {
		t.Fatalf("prepare second model: %v", err)
	}
	if secondPlan.RequestSHA256 == plan.RequestSHA256 {
		t.Fatal("different model produced the same request hash")
	}

	resolvedPolicy := request
	resolvedPolicy.ExpectedModel = "provider/model-one-resolved"
	policyPlan, _, err := prepare(resolvedPolicy, testLimits)
	if err != nil {
		t.Fatalf("prepare response-model policy: %v", err)
	}
	if policyPlan.RequestSHA256 != plan.RequestSHA256 {
		t.Fatal("local ExpectedModel policy changed wire request hash")
	}
}

func TestPrepareOptionalServiceTierAndGenericStateRoots(t *testing.T) {
	request := genericRequest()
	request.ServiceTier = ""
	plan, body, err := prepare(request, testLimits)
	if err != nil {
		t.Fatalf("prepare without service tier: %v", err)
	}
	if bytes.Contains(body, []byte(`"service_tier"`)) {
		t.Fatalf("empty service tier was serialized: %s", body)
	}

	request.ServiceTier = "default"
	withTier, _, err := prepare(request, testLimits)
	if err != nil {
		t.Fatalf("prepare with service tier: %v", err)
	}
	if withTier.RequestSHA256 == plan.RequestSHA256 {
		t.Fatal("explicit service tier did not affect the wire request")
	}

	for _, state := range []string{`""`, `[]`, `{}`} {
		request.State = json.RawMessage(state)
		if _, err := Prepare(request, testLimits); err != nil {
			t.Errorf("Prepare state %s: %v", state, err)
		}
	}
	for _, state := range []string{`null`, `1`, `true`, ``, `"bad`} {
		request.State = json.RawMessage(state)
		if _, err := Prepare(request, testLimits); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("Prepare state %q = %v (%s), want invalid_input", state, err, ErrorCode(err))
		}
	}
}

func TestPrepareRejectsInvalidRequestAndEnforcesBothByteLimits(t *testing.T) {
	valid := genericRequest()
	tests := []struct {
		name    string
		request Request
		limits  Limits
		code    string
	}{
		{name: "empty model", request: requestWith(func(request *Request) { request.Model = " " }), limits: testLimits, code: "invalid_input"},
		{name: "empty service tier is allowed", request: requestWith(func(request *Request) { request.ServiceTier = "" }), limits: testLimits},
		{name: "empty question name", request: requestWith(func(request *Request) {
			request.Questions[""] = request.Questions["alpha"]
			delete(request.Questions, "alpha")
		}), limits: testLimits, code: "invalid_input"},
		{name: "empty instructions", request: requestWith(func(request *Request) {
			q := request.Questions["alpha"]
			q.Instructions = "  "
			request.Questions["alpha"] = q
		}), limits: testLimits, code: "invalid_input"},
		{name: "only one criterion", request: requestWith(func(request *Request) {
			q := request.Questions["alpha"]
			q.Criteria = map[string]string{"one": "only"}
			request.Questions["alpha"] = q
		}), limits: testLimits, code: "invalid_input"},
		{name: "invalid limits", request: valid, limits: Limits{MaxStateBytes: 0, MaxRequestBytes: 8192}, code: "invalid_input"},
		{name: "state over limit", request: requestWith(func(request *Request) {
			request.State = json.RawMessage(`{"body":"` + strings.Repeat("x", 1000) + `"}`)
		}), limits: Limits{MaxStateBytes: 256, MaxRequestBytes: 8192}, code: "context_limit"},
		{name: "request over limit", request: valid, limits: Limits{MaxStateBytes: 4096, MaxRequestBytes: 256}, code: "context_limit"},
		{name: "duplicate state key", request: requestWith(func(request *Request) { request.State = json.RawMessage(`{"x":1,"x":2}`) }), limits: testLimits, code: "invalid_input"},
		{name: "invalid UTF-8 state", request: requestWith(func(request *Request) { request.State = json.RawMessage([]byte{'"', 0xff, '"'}) }), limits: testLimits, code: "invalid_input"},
		{name: "invalid UTF-8 question", request: requestWith(func(request *Request) {
			request.Questions[string([]byte{0xff})] = ChoiceQuestion{Instructions: "x", Criteria: map[string]string{"a": "a", "b": "b"}}
		}), limits: testLimits, code: "invalid_input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Prepare(test.request, test.limits)
			if test.code == "" {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("Prepare error = %v (%s), want %s", err, ErrorCode(err), test.code)
			}
		})
	}
}

func requestWith(change func(*Request)) Request {
	request := genericRequest()
	change(&request)
	return request
}

func TestDecideSupportsArbitraryQuestionsLabelsAndAuthSchemes(t *testing.T) {
	request := genericRequest()
	plan, err := Prepare(request, testLimits)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, scheme := range []struct{ option, header string }{{"Api-Key", "Api-Key " + testAPIKey}, {"Bearer", "Bearer " + testAPIKey}} {
		t.Run(scheme.option, func(t *testing.T) {
			body := successfulResponse(request.Model)
			responseBody := &closeTrackingBody{reader: bytes.NewReader(body)}
			var calls atomic.Int32
			client := testClient(t, scheme.option, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				calls.Add(1)
				if httpRequest.Method != http.MethodPost || httpRequest.URL.String() != testEndpoint {
					t.Errorf("request target = %s %s", httpRequest.Method, httpRequest.URL)
				}
				if got := httpRequest.Header.Get("Authorization"); got != scheme.header {
					t.Errorf("Authorization = %q, want selected scheme", got)
				}
				if httpRequest.Header.Get("Content-Type") != "application/json" || httpRequest.Header.Get("Accept") != "application/json" {
					t.Errorf("request content headers = %#v", httpRequest.Header)
				}
				requestBytes, readErr := io.ReadAll(httpRequest.Body)
				if readErr != nil {
					t.Errorf("read request: %v", readErr)
				}
				if got := fmt.Sprintf("%x", sha256.Sum256(requestBytes)); got != plan.RequestSHA256 {
					t.Errorf("sent request hash = %s, want %s", got, plan.RequestSHA256)
				}
				if bytes.Contains(requestBytes, []byte(testAPIKey)) {
					t.Error("API key appeared in request body")
				}
				response := testHTTPResponse(httpRequest, http.StatusOK, body)
				response.Body = responseBody
				response.Header.Set("X-Request-ID", "request-123")
				return response, nil
			})

			receipt, err := client.Decide(context.Background(), request)
			if err != nil {
				t.Fatalf("Decide: %v (%s)", err, ErrorCode(err))
			}
			if calls.Load() != 1 || !receipt.Attempted || !responseBody.closed {
				t.Fatalf("calls=%d attempted=%v bodyClosed=%v", calls.Load(), receipt.Attempted, responseBody.closed)
			}
			if receipt.RequestedModel != request.Model || receipt.ReturnedModel != request.Model || receipt.RequestSHA256 != plan.RequestSHA256 {
				t.Fatalf("receipt model/hash = %#v", receipt)
			}
			if receipt.RequestID != "request-123" || receipt.HTTPStatus != http.StatusOK {
				t.Fatalf("response metadata = %#v", receipt)
			}
			if len(receipt.Answers) != 2 || receipt.Answers["alpha"].Choice != "merge" || receipt.Answers["beta"].Choice != "enabled" {
				t.Fatalf("generic answers = %#v", receipt.Answers)
			}
			if got := receipt.Answers["alpha"].Probabilities; got["merge"] != 0.9 || got["separate"] != 0.08 || got["unknown"] != 0.02 {
				t.Fatalf("arbitrary labels lost: %#v", got)
			}
			if receipt.Usage == nil || *receipt.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) {
				t.Fatalf("usage = %#v", receipt.Usage)
			}
			if receipt.ResponseSHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
				t.Fatalf("response hash = %s", receipt.ResponseSHA256)
			}
		})
	}
}

func TestDecideExpectedModelOverrideIsLocalAndExact(t *testing.T) {
	request := genericRequest()
	request.Model = "provider/model-alias"
	request.ExpectedModel = "provider/model-resolved-42"
	plan, err := Prepare(request, testLimits)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	withoutPolicy := request
	withoutPolicy.ExpectedModel = ""
	defaultPlan, err := Prepare(withoutPolicy, testLimits)
	if err != nil || defaultPlan.RequestSHA256 != plan.RequestSHA256 {
		t.Fatalf("local response policy changed wire hash: %v %#v %#v", err, plan, defaultPlan)
	}
	client := testClient(t, "Api-Key", time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		return testHTTPResponse(httpRequest, http.StatusOK, successfulResponse(request.ExpectedModel)), nil
	})
	receipt, err := client.Decide(context.Background(), request)
	if err != nil || receipt.ReturnedModel != request.ExpectedModel || receipt.RequestedModel != request.Model || receipt.RequestSHA256 != plan.RequestSHA256 {
		t.Fatalf("explicit alias resolution = %#v, %v", receipt, err)
	}

	for _, expected := range []string{"", "provider/model-wrong"} {
		request.ExpectedModel = expected
		client := testClient(t, "Api-Key", time.Second, func(httpRequest *http.Request) (*http.Response, error) {
			return testHTTPResponse(httpRequest, http.StatusOK, successfulResponse("provider/model-resolved-42")), nil
		})
		receipt, err := client.Decide(context.Background(), request)
		if err == nil || ErrorCode(err) != "malformed_response" || receipt.Usage != nil || receipt.ReturnedModel != "" || receipt.Answers != nil {
			t.Fatalf("expected model %q result = %#v, %v (%s)", expected, receipt, err, ErrorCode(err))
		}
	}
}

func TestNewValidatesEndpointAuthenticationAndClonesHTTPClient(t *testing.T) {
	redirectCalls := 0
	originalRedirect := func(*http.Request, []*http.Request) error {
		redirectCalls++
		return nil
	}
	var calls atomic.Int32
	original := &http.Client{Timeout: 9 * time.Second, CheckRedirect: originalRedirect, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := testHTTPResponse(request, http.StatusFound, nil)
		response.Header.Set("Location", "https://other.example.test/redirect")
		return response, nil
	})}
	options := Options{Endpoint: testEndpoint, APIKey: testAPIKey, AuthScheme: "Api-Key", HTTPClient: original, Timeout: 2 * time.Second, Limits: testLimits}
	client, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.httpClient == original || client.httpClient.Timeout != 2*time.Second || original.Timeout != 9*time.Second || original.CheckRedirect == nil {
		t.Fatal("injected HTTP client was not independently cloned")
	}
	request := genericRequest()
	receipt, err := client.Decide(context.Background(), request)
	if err == nil || ErrorCode(err) != "upstream" {
		t.Fatalf("redirect response error = %v (%s), want upstream status", err, ErrorCode(err))
	}
	if !receipt.Attempted || receipt.HTTPStatus != http.StatusFound || calls.Load() != 1 || redirectCalls != 0 {
		t.Fatalf("redirect attempt=%#v calls=%d callback=%d", receipt, calls.Load(), redirectCalls)
	}

	for _, endpoint := range []string{"http://systemone.example.test", "", "https:///path", "https://user:pass@systemone.example.test", "https://systemone.example.test/path?query=1", "https://systemone.example.test/path#fragment", "https://systemone.example.test/path?"} {
		options.Endpoint = endpoint
		if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("New endpoint %q error=%v (%s), want invalid_input", endpoint, err, ErrorCode(err))
		}
	}
	options.Endpoint = testEndpoint
	for _, scheme := range []string{"", "api-key", "Basic"} {
		options.AuthScheme = scheme
		if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("New auth scheme %q error=%v (%s), want invalid_input", scheme, err, ErrorCode(err))
		}
	}
	options.AuthScheme = "Api-Key"
	options.APIKey = "bad\nkey"
	if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("unsafe API key error = %v (%s), want invalid_input", err, ErrorCode(err))
	}
}

func TestNewDefaultsTimeoutAndRejectsInvalidLimits(t *testing.T) {
	client, err := New(Options{Endpoint: testEndpoint, APIKey: testAPIKey, AuthScheme: "Bearer", Limits: testLimits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.timeout != 30*time.Second || client.httpClient.Timeout != 30*time.Second {
		t.Fatalf("default timeout = %v/%v, want 30s", client.timeout, client.httpClient.Timeout)
	}
	if _, err := New(Options{Endpoint: testEndpoint, APIKey: testAPIKey, AuthScheme: "Bearer", Timeout: -time.Second, Limits: testLimits}); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("negative timeout error = %v (%s)", err, ErrorCode(err))
	}
	if _, err := New(Options{Endpoint: testEndpoint, APIKey: testAPIKey, AuthScheme: "Bearer", Limits: Limits{MaxStateBytes: 0, MaxRequestBytes: 8}}); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("invalid limits error = %v (%s)", err, ErrorCode(err))
	}
}

func TestDecideDoesNotRetryAndSanitizesTransportAndStatusErrors(t *testing.T) {
	request := genericRequest()
	for _, test := range []struct {
		name       string
		transport  roundTripFunc
		wantCode   string
		wantStatus int
	}{
		{name: "transport error", transport: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport included test-secret-api-key and body")
		}, wantCode: "transport"},
		{name: "server failure", transport: func(request *http.Request) (*http.Response, error) {
			return testHTTPResponse(request, http.StatusServiceUnavailable, []byte("server echoed test-secret-api-key")), nil
		}, wantCode: "upstream", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, "Api-Key", time.Second, func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return test.transport(request)
			})
			receipt, err := client.Decide(context.Background(), request)
			if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if calls.Load() != 1 || !receipt.Attempted || receipt.HTTPStatus != test.wantStatus {
				t.Fatalf("calls=%d receipt=%#v", calls.Load(), receipt)
			}
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "server echoed") || strings.Contains(fmt.Sprint(receipt), testAPIKey) || strings.Contains(fmt.Sprint(receipt), "server echoed") {
				t.Fatalf("key or provider text leaked: error=%v receipt=%#v", err, receipt)
			}
			if test.wantStatus != 0 && receipt.ResponseSHA256 == "" {
				t.Fatal("complete bounded non-2xx body was not hashed")
			}
		})
	}
}

func TestDecidePreservesValidatedMetadataOnFailures(t *testing.T) {
	request := genericRequest()
	valid := successfulResponse(request.Model)
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
		{name: "provider error", status: http.StatusOK, body: []byte(strings.Replace(string(valid), `,"answers"`, `,"error":{"detail":"synthetic"},"answers"`, 1)), wantCode: "upstream", wantModel: true, wantUsage: true},
		{name: "truncated without metadata", status: http.StatusOK, body: []byte(`{"truncated":true}`), wantCode: "truncated"},
		{name: "context overflow without metadata", status: http.StatusOK, body: []byte(`{"context_overflow":true}`), wantCode: "context_limit"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: valid, wantCode: "auth", wantModel: true, wantUsage: true},
		{name: "rate limited", status: http.StatusTooManyRequests, body: valid, wantCode: "rate_limit", wantModel: true, wantUsage: true},
		{name: "upstream status", status: http.StatusServiceUnavailable, body: valid, wantCode: "upstream", wantModel: true, wantUsage: true},
		{name: "malformed body", status: http.StatusUnauthorized, body: []byte(`{"model":`), wantCode: "auth"},
		{name: "duplicate JSON", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"model":"`+request.Model+`"`, `"model":"`+request.Model+`","model":"`+request.Model+`"`, 1)), wantCode: "auth"},
		{name: "mismatched model", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"model":"`+request.Model+`"`, `"model":"other-model"`, 1)), wantCode: "auth"},
		{name: "invalid usage", status: http.StatusUnauthorized, body: []byte(strings.Replace(string(valid), `"input_tokens":150`, `"input_tokens":null`, 1)), wantCode: "auth", wantModel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, "Api-Key", time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				return testHTTPResponse(httpRequest, test.status, test.body), nil
			})
			receipt, err := client.Decide(context.Background(), request)
			if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("Decide error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if receipt.HTTPStatus != test.status || receipt.Answers != nil {
				t.Fatalf("failure receipt status/answers = %d/%#v", receipt.HTTPStatus, receipt.Answers)
			}
			if (receipt.ReturnedModel != "") != test.wantModel || (receipt.Usage != nil) != test.wantUsage {
				t.Fatalf("failure receipt metadata = model:%q usage:%#v", receipt.ReturnedModel, receipt.Usage)
			}
			if test.wantUsage && *receipt.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) {
				t.Fatalf("usage = %#v", receipt.Usage)
			}
		})
	}
}

func TestDecideCancellationTimeoutAndAttemptSemantics(t *testing.T) {
	request := genericRequest()
	t.Run("pre-canceled", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, "Api-Key", time.Second, func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		receipt, err := client.Decide(ctx, request)
		if err == nil || ErrorCode(err) != expectedCancelCode || receipt.Attempted || receipt.RequestSHA256 == "" || calls.Load() != 0 {
			t.Fatalf("pre-cancel result = %#v error=%v calls=%d", receipt, err, calls.Load())
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		started := make(chan struct{})
		client := testClient(t, "Api-Key", time.Second, func(request *http.Request) (*http.Response, error) {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			receipt Receipt
			err     error
		}
		done := make(chan result, 1)
		go func() { receipt, err := client.Decide(ctx, request); done <- result{receipt, err} }()
		<-started
		cancel()
		got := <-done
		if got.err == nil || ErrorCode(got.err) != expectedCancelCode || !got.receipt.Attempted {
			t.Fatalf("cancellation result = %#v error=%v", got.receipt, got.err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, "Api-Key", 10*time.Millisecond, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		receipt, err := client.Decide(context.Background(), request)
		if err == nil || ErrorCode(err) != "timeout" || !receipt.Attempted || calls.Load() != 1 {
			t.Fatalf("timeout result = %#v error=%v calls=%d", receipt, err, calls.Load())
		}
	})
}

func TestDecideRedactsAPIKeyFromRequestIDAndError(t *testing.T) {
	request := genericRequest()
	tests := []struct{ name, requestID, wantID string }{
		{name: "exact key", requestID: testAPIKey},
		{name: "embedded key", requestID: "req-" + testAPIKey + "-tail"},
		{name: "ordinary ID", requestID: "request-123", wantID: "request-123"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, "Api-Key", time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				response := testHTTPResponse(httpRequest, http.StatusUnauthorized, nil)
				response.Header.Set("X-Request-ID", test.requestID)
				return response, nil
			})
			receipt, err := client.Decide(context.Background(), request)
			if err == nil || ErrorCode(err) != "auth" || receipt.RequestID != test.wantID {
				t.Fatalf("receipt=%#v error=%v", receipt, err)
			}
			serializedReceipt, marshalErr := json.Marshal(receipt)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if bytes.Contains(serializedReceipt, []byte(testAPIKey)) || strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("API key leaked in receipt/error: %s / %v", serializedReceipt, err)
			}
		})
	}
}

func TestDecideRejectsInvalidRequestBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, "Api-Key", time.Second, func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })
	request := genericRequest()
	request.Questions = map[string]ChoiceQuestion{}
	receipt, err := client.Decide(context.Background(), request)
	if err == nil || ErrorCode(err) != "invalid_input" || receipt.Attempted || calls.Load() != 0 {
		t.Fatalf("invalid request result = %#v error=%v calls=%d", receipt, err, calls.Load())
	}
	//nolint:staticcheck // SA1012: nil is intentionally tested because Decide must reject a nil context.
	receipt, err = client.Decide(nil, genericRequest())
	if err == nil || ErrorCode(err) != "invalid_input" || receipt.Attempted || calls.Load() != 0 {
		t.Fatalf("nil context result = %#v error=%v calls=%d", receipt, err, calls.Load())
	}
}

func TestDecideBoundsAndClosesResponseBodies(t *testing.T) {
	request := genericRequest()
	tests := []struct {
		name string
		body func(*http.Request) (*http.Response, func() bool)
		code string
	}{
		{
			name: "declared oversized",
			body: func(request *http.Request) (*http.Response, func() bool) {
				body := &closeTrackingBody{reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, maxResponseBytes+1))}
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": {fmt.Sprint(maxResponseBytes + 1)}}, Body: body, ContentLength: maxResponseBytes + 1, Request: request}
				return response, func() bool { return body.closed }
			},
			code: "response_too_large",
		},
		{
			name: "stream oversized",
			body: func(request *http.Request) (*http.Response, func() bool) {
				body := &closeTrackingBody{reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, maxResponseBytes+1))}
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: -1, Request: request}
				return response, func() bool { return body.closed }
			},
			code: "response_too_large",
		},
		{
			name: "declared length mismatch",
			body: func(request *http.Request) (*http.Response, func() bool) {
				data := successfulResponse(request.URL.Host)
				body := &closeTrackingBody{reader: bytes.NewReader(data)}
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": {fmt.Sprint(len(data) + 1)}}, Body: body, ContentLength: int64(len(data) + 1), Request: request}
				return response, func() bool { return body.closed }
			},
			code: "truncated",
		},
		{
			name: "unexpected EOF",
			body: func(request *http.Request) (*http.Response, func() bool) {
				body := &unexpectedEOFBody{data: []byte(`{"model":"partial`)}
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: -1, Request: request}
				return response, func() bool { return body.closed }
			},
			code: "truncated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wasClosed func() bool
			client := testClient(t, "Api-Key", time.Second, func(request *http.Request) (*http.Response, error) {
				response, closed := test.body(request)
				wasClosed = closed
				return response, nil
			})
			receipt, err := client.Decide(context.Background(), request)
			if err == nil || ErrorCode(err) != test.code || receipt.ResponseSHA256 != "" {
				t.Fatalf("result = %#v error=%v (%s)", receipt, err, ErrorCode(err))
			}
			if wasClosed == nil || !wasClosed() {
				t.Fatal("response body was not closed")
			}
		})
	}
}
