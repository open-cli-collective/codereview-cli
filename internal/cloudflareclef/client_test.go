package cloudflareclef

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	testAccountID = "account123"
	testAPIKey    = "test-cloudflare-token"
)

var testLimits = systemone.Limits{MaxStateBytes: 4096, MaxRequestBytes: 8192}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func clefRequest(model string) systemone.Request {
	return systemone.Request{
		Model: model,
		State: json.RawMessage(`{"event":"review","severity":2}`),
		Questions: map[string]systemone.ChoiceQuestion{
			"route": {
				Instructions: "Choose the matching route.",
				Criteria: map[string]string{
					"merge": "the changes are equivalent",
					"split": "the changes differ",
				},
			},
		},
	}
}

func clefAnswer(choice string) string {
	return fmt.Sprintf(`{"type":"choice","choice":%q,"confidence":0.9,"probabilities":{"merge":0.9,"split":0.1}}`, choice)
}

func clefResult(model, usage, answer string) string {
	return fmt.Sprintf(`{"model":%q,"answers":{"route":%s},"usage":%s}`, model, answer, usage)
}

func clefSuccess(result string) []byte {
	return []byte(fmt.Sprintf(`{"success":true,"result":%s,"errors":[],"messages":[]}`, result))
}

func clefFailure(result, errorsJSON string) []byte {
	return []byte(fmt.Sprintf(`{"success":false,"result":%s,"errors":%s,"messages":[{"message":"private provider detail"}]}`, result, errorsJSON))
}

func testResponse(request *http.Request, status int, body []byte) *http.Response {
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
		AccountID:  testAccountID,
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

func TestPrepareAllowsOnlyClefSelectorsAndNoServiceTier(t *testing.T) {
	for _, model := range []string{"clef", "clef-flash"} {
		if _, err := Prepare(clefRequest(model), testLimits); err != nil {
			t.Errorf("Prepare(%q): %v (%s)", model, err, ErrorCode(err))
		}
	}
	for _, model := range []string{"", "clef ", "@cf/cloudflare/clef", "clef-pro", "CLEF"} {
		if _, err := Prepare(clefRequest(model), testLimits); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("Prepare(%q) = %v (%s), want invalid_input", model, err, ErrorCode(err))
		}
	}
	request := clefRequest("clef")
	request.ServiceTier = "priority"
	if _, err := Prepare(request, testLimits); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("nonempty service tier = %v (%s), want invalid_input", err, ErrorCode(err))
	}
}

func TestEndpointValidatesAccountAndModelWithoutCredentials(t *testing.T) {
	for _, model := range []string{"clef", "clef-flash"} {
		endpoint, err := Endpoint(testAccountID, model)
		want := "https://api.cloudflare.com/client/v4/accounts/account123/ai/run/@cf/cloudflare/" + model
		if err != nil || endpoint != want {
			t.Fatalf("Endpoint(%q) = %q, %v; want %q", model, endpoint, err, want)
		}
	}
	for _, selectors := range [][2]string{{"", "clef"}, {"account/path", "clef"}, {"account?x", "clef"}, {"account123", "other"}} {
		endpoint, err := Endpoint(selectors[0], selectors[1])
		if err == nil || endpoint != "" || ErrorCode(err) != "invalid_input" {
			t.Fatalf("invalid selectors returned endpoint %q, error %v", endpoint, err)
		}
	}
}

func TestDecideSendsExactCloudflareRequestAndHashesRawOuterResponse(t *testing.T) {
	for _, model := range []string{"clef", "clef-flash"} {
		t.Run(model, func(t *testing.T) {
			request := clefRequest(model)
			body := clefSuccess(clefResult(model, `{"input_tokens":0,"output_tokens":4}`, clefAnswer("merge")))
			var calls atomic.Int32
			client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				calls.Add(1)
				wantEndpoint := "https://api.cloudflare.com/client/v4/accounts/account123/ai/run/@cf/cloudflare/" + model
				if httpRequest.Method != http.MethodPost || httpRequest.URL.String() != wantEndpoint {
					t.Errorf("request target = %s %s, want POST %s", httpRequest.Method, httpRequest.URL, wantEndpoint)
				}
				if got := httpRequest.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
					t.Errorf("Authorization = %q", got)
				}
				if httpRequest.Header.Get("Content-Type") != "application/json" || httpRequest.Header.Get("Accept") != "application/json" {
					t.Errorf("request content headers = %#v", httpRequest.Header)
				}
				requestBody, err := io.ReadAll(httpRequest.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
				}
				if bytes.Contains(requestBody, []byte(testAPIKey)) {
					t.Error("API key appeared in request body")
				}
				var wire struct {
					Model     string         `json:"model"`
					State     map[string]any `json:"state"`
					Questions map[string]struct {
						Type         string            `json:"type"`
						Instructions string            `json:"instructions"`
						Criteria     map[string]string `json:"criteria"`
					} `json:"questions"`
				}
				if err := json.Unmarshal(requestBody, &wire); err != nil {
					t.Errorf("request body is not valid JSON: %v", err)
				}
				question := wire.Questions["route"]
				if wire.Model != model || wire.State["event"] != "review" || question.Type != "choice" || question.Instructions != "Choose the matching route." || question.Criteria["merge"] != "the changes are equivalent" {
					t.Errorf("unexpected wire fields: %#v", wire)
				}
				plan, encoded, err := systemone.Encode(request, testLimits)
				if err != nil {
					t.Errorf("systemone.Encode: %v", err)
				} else if got := fmt.Sprintf("%x", sha256.Sum256(requestBody)); got != plan.RequestSHA256 || !bytes.Equal(requestBody, encoded) {
					t.Errorf("wire bytes/hash differ from shared codec: hash=%s plan=%s", got, plan.RequestSHA256)
				}
				response := testResponse(httpRequest, http.StatusOK, body)
				response.Header.Set("CF-Ray", "ray-123")
				return response, nil
			})

			receipt, err := client.Decide(context.Background(), request)
			if err != nil {
				t.Fatalf("Decide: %v (%s)", err, ErrorCode(err))
			}
			if calls.Load() != 1 || !receipt.Attempted || receipt.HTTPStatus != http.StatusOK {
				t.Fatalf("request lifecycle = %#v calls=%d", receipt, calls.Load())
			}
			if receipt.ContextStatus != "unverified" || receipt.ReturnedModel != model || receipt.RequestedModel != model || receipt.RequestID != "ray-123" {
				t.Fatalf("receipt identity/context = %#v", receipt)
			}
			if receipt.RequestSHA256 == "" || receipt.ResponseSHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
				t.Fatalf("request/outer response hash = %#v", receipt)
			}
			if len(receipt.Answers) != 1 || receipt.Answers["route"].Choice != "merge" {
				t.Fatalf("answers = %#v", receipt.Answers)
			}
			if receipt.Usage == nil || *receipt.Usage != (Usage{InputTokens: 0, OutputTokens: 4}) {
				t.Fatalf("zero token usage = %#v", receipt.Usage)
			}
		})
	}
}

func TestDecideAppliesExpectedModelPolicyAndUsageProjection(t *testing.T) {
	request := clefRequest("clef")
	request.ExpectedModel = "clef-release-2026"
	body := clefSuccess(clefResult(request.ExpectedModel, `{"input_tokens":11,"output_tokens":3,"total_tokens":14,"cache_tokens":9,"provider_extension":{"note":"ignored"}}`, clefAnswer("merge")))
	client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		return testResponse(httpRequest, http.StatusOK, body), nil
	})
	receipt, err := client.Decide(context.Background(), request)
	if err != nil {
		t.Fatalf("Decide with model policy: %v (%s)", err, ErrorCode(err))
	}
	if receipt.ReturnedModel != request.ExpectedModel || receipt.Usage == nil || receipt.Usage.TotalTokens == nil || *receipt.Usage.TotalTokens != 14 {
		t.Fatalf("policy or projected usage = %#v", receipt)
	}
	if receipt.ResponseSHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
		t.Fatalf("hash must cover exact outer bytes: %s", receipt.ResponseSHA256)
	}

	bodyWithoutTotal := clefSuccess(clefResult("clef", `{"input_tokens":1,"output_tokens":2}`, clefAnswer("merge")))
	client = testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		return testResponse(httpRequest, http.StatusOK, bodyWithoutTotal), nil
	})
	receipt, err = client.Decide(context.Background(), clefRequest("clef"))
	if err != nil || receipt.Usage == nil || receipt.Usage.TotalTokens != nil {
		t.Fatalf("missing optional total = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
}

func TestDecidePreservesLargeRawResultMetadataDuringUsageProjection(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
	}{
		{name: "less-than bytes", metadata: strings.Repeat("<", 180_000)},
		{name: "raw U+2028 UTF-8", metadata: strings.Repeat("\u2028", 300_000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := `{"model":"clef","metadata":"` + test.metadata + `","answers":{"route":` + clefAnswer("merge") + `},"usage" : { "input_tokens" : 5, "output_tokens": 2, "total_tokens": 7, "cache_tokens": 9 }}`
			body := []byte(`{"success":true,"result":` + result + `,"errors":[],"messages":[]}`)
			if len(body) >= maxResponseBytes {
				t.Fatalf("test fixture must remain under the raw outer response bound: %d", len(body))
			}
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				return testResponse(request, http.StatusOK, body), nil
			})
			receipt, err := client.Decide(context.Background(), clefRequest("clef"))
			if err != nil {
				t.Fatalf("Decide: %v (%s), raw body bytes=%d", err, ErrorCode(err), len(body))
			}
			if receipt.ReturnedModel != "clef" || len(receipt.Answers) != 1 || receipt.Answers["route"].Choice != "merge" {
				t.Fatalf("validated answer/model lost during projection: %#v", receipt)
			}
			if receipt.Usage == nil || receipt.Usage.InputTokens != 5 || receipt.Usage.OutputTokens != 2 || receipt.Usage.TotalTokens == nil || *receipt.Usage.TotalTokens != 7 {
				t.Fatalf("base/optional usage lost during projection: %#v", receipt.Usage)
			}
			if receipt.ResponseSHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) || receipt.ContextStatus != "unverified" {
				t.Fatalf("raw receipt metadata changed: hash=%q context=%q", receipt.ResponseSHA256, receipt.ContextStatus)
			}
		})
	}
}

func TestDecideFailsClosedOnMissingOrInvalidUsage(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage string
	}{
		{name: "missing usage"},
		{name: "missing input", usage: `{"output_tokens":2}`},
		{name: "invalid total null", usage: `{"input_tokens":1,"output_tokens":2,"total_tokens":null}`},
		{name: "invalid total negative", usage: `{"input_tokens":1,"output_tokens":2,"total_tokens":-1}`},
		{name: "invalid total string", usage: `{"input_tokens":1,"output_tokens":2,"total_tokens":"3"}`},
		{name: "invalid total overflow", usage: `{"input_tokens":1,"output_tokens":2,"total_tokens":9223372036854775808}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := `{"model":"clef","answers":{"route":` + clefAnswer("merge") + `}`
			if test.usage != "" {
				result += `,"usage":` + test.usage
			}
			result += `}`
			body := clefSuccess(result)
			client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				return testResponse(httpRequest, http.StatusOK, body), nil
			})
			receipt, err := client.Decide(context.Background(), clefRequest("clef"))
			if err == nil || ErrorCode(err) != "malformed_response" {
				t.Fatalf("result = %#v, error=%v (%s), want malformed_response", receipt, err, ErrorCode(err))
			}
			if receipt.ReturnedModel != "clef" || receipt.Usage != nil {
				t.Fatalf("metadata for untrusted usage = %#v", receipt)
			}
		})
	}
}

func TestDecidePreservesSafeUsageOnBadAnswer(t *testing.T) {
	body := clefSuccess(clefResult("clef", `{"input_tokens":5,"output_tokens":2,"total_tokens":7,"future":true}`, clefAnswer("outside-criteria")))
	client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		return testResponse(httpRequest, http.StatusOK, body), nil
	})
	receipt, err := client.Decide(context.Background(), clefRequest("clef"))
	if err == nil || ErrorCode(err) != "malformed_response" || receipt.Answers != nil {
		t.Fatalf("invalid answer result = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
	if receipt.ReturnedModel != "clef" || receipt.Usage == nil || receipt.Usage.InputTokens != 5 || receipt.Usage.TotalTokens == nil || *receipt.Usage.TotalTokens != 7 {
		t.Fatalf("safe result metadata was lost: %#v", receipt)
	}
}

func TestDecideRejectsExplicitContextAndTruncationSignalsWithMetadata(t *testing.T) {
	validResult := clefResult("clef", `{"input_tokens":5,"output_tokens":2}`, clefAnswer("merge"))
	for _, test := range []struct {
		name string
		body []byte
		code string
	}{
		{name: "root context overflow", body: []byte(strings.Replace(string(clefSuccess(validResult)), `,"result"`, `,"context_overflow":true,"result"`, 1)), code: "context_limit"},
		{name: "nested truncation", body: clefSuccess(strings.Replace(validResult, `,"answers"`, `,"truncated":true,"answers"`, 1)), code: "truncated"},
		{name: "nested incomplete status", body: clefSuccess(strings.Replace(validResult, `,"answers"`, `,"finish_reason":"length","answers"`, 1)), code: "truncated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				return testResponse(request, http.StatusOK, test.body), nil
			})
			receipt, err := client.Decide(context.Background(), clefRequest("clef"))
			if err == nil || ErrorCode(err) != test.code || receipt.ContextStatus != "unverified" {
				t.Fatalf("signal result = %#v, %v (%s), want %s", receipt, err, ErrorCode(err), test.code)
			}
			if receipt.ReturnedModel != "clef" || receipt.Usage == nil || receipt.Usage.InputTokens != 5 || receipt.Answers != nil {
				t.Fatalf("trusted metadata on incomplete response = %#v", receipt)
			}
		})
	}
}

func TestDecideRejectsProviderFailureAndNonSuccessEnvelopes(t *testing.T) {
	validResult := clefResult("clef", `{"input_tokens":5,"output_tokens":2}`, clefAnswer("merge"))
	for _, test := range []struct {
		name   string
		status int
		body   []byte
		code   string
	}{
		{name: "success false", status: http.StatusOK, body: clefFailure(validResult, `[{"code":"private","message":"secret detail"}]`), code: "upstream"},
		{name: "nonempty errors", status: http.StatusOK, body: []byte(fmt.Sprintf(`{"success":true,"result":%s,"errors":[{"message":"private"}]}`, validResult)), code: "upstream"},
		{name: "401", status: http.StatusUnauthorized, body: clefFailure(validResult, `[{"message":"private auth details"}]`), code: "auth"},
		{name: "429", status: http.StatusTooManyRequests, body: clefFailure(validResult, `[]`), code: "rate_limit"},
		{name: "413", status: http.StatusRequestEntityTooLarge, body: clefFailure(validResult, `[]`), code: "context_limit"},
		{name: "500 malformed body", status: http.StatusInternalServerError, body: []byte("gateway failed with private details"), code: "upstream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
				return testResponse(httpRequest, test.status, test.body), nil
			})
			receipt, err := client.Decide(context.Background(), clefRequest("clef"))
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("error = %v (%s), want %s", err, ErrorCode(err), test.code)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("provider or credential text leaked in error: %q", err)
			}
			if receipt.Answers != nil {
				t.Fatalf("failed response returned answers: %#v", receipt.Answers)
			}
			if test.status < 300 && (receipt.ReturnedModel != "clef" || receipt.Usage == nil) {
				t.Fatalf("safe failure metadata = %#v", receipt)
			}
			if test.status == http.StatusUnauthorized && (receipt.ReturnedModel != "clef" || receipt.Usage == nil) {
				t.Fatalf("safe HTTP error metadata = %#v", receipt)
			}
			if receipt.ResponseSHA256 != fmt.Sprintf("%x", sha256.Sum256(test.body)) {
				t.Fatalf("response hash = %q", receipt.ResponseSHA256)
			}
		})
	}
}

func TestDecideRejectsWrongModelAndNeverClaimsVerifiedContext(t *testing.T) {
	body := clefSuccess(clefResult("another-model", `{"input_tokens":5,"output_tokens":2}`, clefAnswer("merge")))
	client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		return testResponse(httpRequest, http.StatusOK, body), nil
	})
	receipt, err := client.Decide(context.Background(), clefRequest("clef"))
	if err == nil || ErrorCode(err) != "malformed_response" || receipt.ReturnedModel != "" || receipt.Usage != nil {
		t.Fatalf("wrong model receipt = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
	if receipt.ContextStatus != "unverified" {
		t.Fatalf("context status = %q", receipt.ContextStatus)
	}
}

func TestNewValidatesAccountKeyTimeoutAndLimits(t *testing.T) {
	base := Options{AccountID: testAccountID, APIKey: testAPIKey, Limits: testLimits}
	for _, account := range []string{"", "../account", "abc-123", strings.Repeat("a", 65), "ümlaut"} {
		options := base
		options.AccountID = account
		if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("New account %q = %v (%s), want invalid_input", account, err, ErrorCode(err))
		}
	}
	for _, key := range []string{"", "has space", "line\nbreak", "tab\tvalue", "ümlaut"} {
		options := base
		options.APIKey = key
		if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
			t.Errorf("New key %q = %v (%s), want invalid_input", key, err, ErrorCode(err))
		}
	}
	options := base
	options.Timeout = -time.Second
	if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("negative timeout = %v (%s), want invalid_input", err, ErrorCode(err))
	}
	options = base
	options.Limits = systemone.Limits{}
	if _, err := New(options); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("invalid limits = %v (%s), want invalid_input", err, ErrorCode(err))
	}
}

func TestDecideDoesNotAttemptInvalidOrPrecancelledRequest(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, time.Second, func(httpRequest *http.Request) (*http.Response, error) {
		calls.Add(1)
		return testResponse(httpRequest, http.StatusOK, clefSuccess(clefResult("clef", `{"input_tokens":1,"output_tokens":1}`, clefAnswer("merge")))), nil
	})
	request := clefRequest("not-clef")
	receipt, err := client.Decide(context.Background(), request)
	if err == nil || ErrorCode(err) != "invalid_input" || receipt.Attempted || calls.Load() != 0 {
		t.Fatalf("invalid request = %#v, %v (%s), calls=%d", receipt, err, ErrorCode(err), calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err = client.Decide(ctx, clefRequest("clef"))
	if err == nil || ErrorCode(err) != errorCancelled || receipt.Attempted || receipt.RequestSHA256 == "" || calls.Load() != 0 {
		t.Fatalf("precancelled request = %#v, %v (%s), calls=%d", receipt, err, ErrorCode(err), calls.Load())
	}
}

func TestDecideTimeoutCancellationAndNoRetryOrRedirect(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		client := testClient(t, 15*time.Millisecond, func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		receipt, err := client.Decide(context.Background(), clefRequest("clef"))
		if err == nil || ErrorCode(err) != "timeout" || !receipt.Attempted {
			t.Fatalf("timeout result = %#v, %v (%s)", receipt, err, ErrorCode(err))
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		started := make(chan struct{})
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan struct {
			receipt Receipt
			err     error
		}, 1)
		go func() {
			receipt, err := client.Decide(ctx, clefRequest("clef"))
			result <- struct {
				receipt Receipt
				err     error
			}{receipt: receipt, err: err}
		}()
		<-started
		cancel()
		got := <-result
		if got.err == nil || ErrorCode(got.err) != errorCancelled || !got.receipt.Attempted {
			t.Fatalf("cancellation result = %#v, %v (%s)", got.receipt, got.err, ErrorCode(got.err))
		}
	})

	t.Run("one attempt and no redirects", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := testResponse(request, http.StatusTemporaryRedirect, []byte(`{"success":false,"errors":[],"messages":[]}`))
			response.Header.Set("Location", "https://example.invalid/redirect")
			return response, nil
		})
		receipt, err := client.Decide(context.Background(), clefRequest("clef"))
		if err == nil || ErrorCode(err) != "upstream" || calls.Load() != 1 || receipt.HTTPStatus != http.StatusTemporaryRedirect {
			t.Fatalf("redirect result = %#v, %v (%s), calls=%d", receipt, err, ErrorCode(err), calls.Load())
		}
	})
}

type closeTrackingBody struct {
	reader io.Reader
	closed bool
}

func (body *closeTrackingBody) Read(destination []byte) (int, error) {
	return body.reader.Read(destination)
}
func (body *closeTrackingBody) Close() error {
	body.closed = true
	return nil
}

func TestDecideBoundsAndHashesOnlyCompleteResponses(t *testing.T) {
	t.Run("declared oversize", func(t *testing.T) {
		responseBody := &closeTrackingBody{reader: bytes.NewReader(nil)}
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			response := testResponse(request, http.StatusOK, nil)
			response.ContentLength = maxResponseBytes + 1
			response.Body = responseBody
			return response, nil
		})
		receipt, err := client.Decide(context.Background(), clefRequest("clef"))
		if err == nil || ErrorCode(err) != "response_too_large" || !responseBody.closed || receipt.ResponseSHA256 != "" {
			t.Fatalf("declared oversize result = %#v, %v (%s), closed=%v", receipt, err, ErrorCode(err), responseBody.closed)
		}
	})

	t.Run("actual oversize", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), maxResponseBytes+1)
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			response := testResponse(request, http.StatusOK, body)
			response.ContentLength = -1
			return response, nil
		})
		receipt, err := client.Decide(context.Background(), clefRequest("clef"))
		if err == nil || ErrorCode(err) != "response_too_large" || receipt.ResponseSHA256 != "" {
			t.Fatalf("actual oversize result = %#v, %v (%s)", receipt, err, ErrorCode(err))
		}
	})

	t.Run("content length mismatch", func(t *testing.T) {
		body := clefSuccess(clefResult("clef", `{"input_tokens":1,"output_tokens":1}`, clefAnswer("merge")))
		client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
			response := testResponse(request, http.StatusOK, body)
			response.ContentLength++
			return response, nil
		})
		receipt, err := client.Decide(context.Background(), clefRequest("clef"))
		if err == nil || ErrorCode(err) != "truncated" || receipt.ResponseSHA256 != "" {
			t.Fatalf("length mismatch result = %#v, %v (%s)", receipt, err, ErrorCode(err))
		}
	})
}

func TestDecideRedactsCredentialEchoFromRequestID(t *testing.T) {
	body := clefFailure("null", `[]`)
	client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
		response := testResponse(request, http.StatusUnauthorized, body)
		response.Header.Set("CF-Ray", "prefix-"+testAPIKey+"-suffix")
		response.Header.Set("X-Request-ID", "safe-id-42")
		return response, nil
	})
	receipt, err := client.Decide(context.Background(), clefRequest("clef"))
	if err == nil || ErrorCode(err) != "auth" || receipt.RequestID != "safe-id-42" {
		t.Fatalf("credential-echo response = %#v, %v (%s)", receipt, err, ErrorCode(err))
	}
}
