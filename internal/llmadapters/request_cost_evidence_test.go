package llmadapters

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

const costWire = `{"id":"resp-synthetic","model":"observed-model","service_tier":"priority","status":"completed","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":0}},"output_text":"{\"ok\":true}"}`

type costRoundTrip func(*http.Request) (*http.Response, error)

func (f costRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func costBodyAdapter(t *testing.T, status int, body io.ReadCloser) *APIAdapter {
	t.Helper()
	a, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "synthetic-test-key", HTTPClient: &http.Client{Transport: costRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Body: body, Header: make(http.Header)}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func oneCostAttempt(t *testing.T, r Response) llm.RequestCostAttempt {
	t.Helper()
	if r.RequestCostEvidence == nil || len(r.RequestCostEvidence.Attempts) != 1 {
		t.Fatalf("missing bounded receipt: %#v", r.RequestCostEvidence)
	}
	return r.RequestCostEvidence.Attempts[0]
}

func TestOpenAIRequestCostRetainsUsageAndObservedIdentity(t *testing.T) {
	a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(costWire)))
	_, response, err := a.execute(context.Background(), Request{Model: "requested-model", Fast: true})
	if err != nil {
		t.Fatal(err)
	}
	r := oneCostAttempt(t, response)
	if r.RequestedModel != "requested-model" || *r.ObservedModel != "observed-model" || *r.RequestedServiceTier != "fast" || *r.ObservedServiceTier != "priority" || *r.Usage.CacheWriteTokens != 0 {
		t.Fatalf("collapsed request/delivered values: %#v", r)
	}
	if r.TransportMode != "opaque" || response.RequestCostEvidence.AttemptCountExact {
		t.Fatal("injected client gained ownership")
	}
	*r.Usage.InputTokens = 999
	if *response.RequestCostEvidence.Attempts[0].Usage.InputTokens != 999 {
		t.Fatal("unexpected local pointer semantics")
	}
	clone := llm.CloneRequestCostEvidence(response.RequestCostEvidence)
	*clone.Attempts[0].Usage.InputTokens = 1
	if *response.RequestCostEvidence.Attempts[0].Usage.InputTokens != 999 {
		t.Fatal("detached copy aliases evidence")
	}
}

func TestOpenAIRequestCostInvalidCountersMissingNullAndDuplicateFields(t *testing.T) {
	for _, value := range []string{`null`, `-1`, `10.5`, `"10"`, `true`, `[]`, `9223372036854775808`} {
		body := strings.Replace(costWire, `"cache_write_tokens":0`, `"cache_write_tokens":`+value, 1)
		a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
		_, response, _ := a.execute(context.Background(), Request{Model: "request"})
		r := oneCostAttempt(t, response)
		if r.Usage == nil || *r.Usage.InputTokens != 10 || !slices.Contains(r.Issues, llm.CostGapUsage) {
			t.Fatalf("invalid counter erased safe usage or became eligible: %s %#v", value, r)
		}
		if value == `-1` && (r.Usage.CacheWriteTokens == nil || *r.Usage.CacheWriteTokens != -1) {
			t.Fatal("negative observation not retained")
		}
		if value != `-1` && r.Usage.CacheWriteTokens != nil {
			t.Fatal("invalid/null counter manufactured a value")
		}
	}
	for _, body := range []string{
		strings.Replace(costWire, `"usage":{`, `"usage":null,"ignored_usage":{`, 1),
		strings.Replace(costWire, `"input_tokens_details":{`, `"input_tokens_details":null,"ignored_details":{`, 1),
		strings.Replace(costWire, `"cache_write_tokens":0`, `"cache_write_tokens":0,"cache_write_tokens":1`, 1),
		strings.Replace(costWire, `"id":"resp-synthetic"`, `"id":"first","id":"second"`, 1),
		strings.Replace(costWire, `"input_tokens":10`, `"input_tokens":10,"input_tokens":11`, 1),
		strings.Replace(costWire, `"cached_tokens":2`, `"cached_tokens":11`, 1),
		strings.Replace(costWire, `"total_tokens":13`, `"total_tokens":14`, 1),
	} {
		a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
		_, response, _ := a.execute(context.Background(), Request{Model: "request"})
		r := oneCostAttempt(t, response)
		if !slices.Contains(r.Issues, llm.CostGapUsage) && !slices.Contains(r.Issues, llm.CostGapDuplicate) {
			t.Fatalf("malformed evidence became eligible: %#v", r)
		}
	}
}

func TestOpenAIRequestCostNoTextMalformedOutputAndErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		bodyState  string
	}{
		{"completed refusal", strings.Replace(costWire, `"output_text":"{\"ok\":true}"`, `"output":[{"type":"message","content":[{"type":"refusal","refusal":"synthetic-secret-canary"}]}]`, 1), 200, "complete"},
		{"incomplete", strings.Replace(strings.Replace(costWire, `"status":"completed"`, `"status":"incomplete"`, 1), `"output_text":"{\"ok\":true}"`, `"output":[]`, 1), 200, "complete"},
		{"output shape", strings.Replace(costWire, `"output_text":"{\"ok\":true}"`, `"output":42`, 1), 200, "complete"},
		{"HTTP error", costWire, 429, "complete"},
		{"truncated", costWire[:len(costWire)-1], 200, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := costBodyAdapter(t, tc.status, io.NopCloser(strings.NewReader(tc.body)))
			_, response, err := a.execute(context.Background(), Request{Model: "request"})
			if err == nil {
				t.Fatal("task error converted to success")
			}
			r := oneCostAttempt(t, response)
			if r.BodyState != tc.bodyState || *r.HTTPStatus != tc.status {
				t.Fatalf("receipt=%#v", r)
			}
			if tc.bodyState == "complete" && (r.Usage == nil || *r.Usage.InputTokens != 10) {
				t.Fatal("no-text/error discarded usage")
			}
			if tc.bodyState != "complete" && r.Usage != nil {
				t.Fatal("malformed JSON prefix scraped")
			}
			encoded, _ := json.Marshal(response.RequestCostEvidence)
			if strings.Contains(string(encoded), "synthetic-secret-canary") {
				t.Fatal("refusal text leaked into cost evidence")
			}
		})
	}
}

type failingCostBody struct{}

func (failingCostBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingCostBody) Close() error             { return nil }
func TestOpenAIRequestCostTransportBodyLimitAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		body  io.ReadCloser
		state string
	}{{failingCostBody{}, "read_error"}, {io.NopCloser(strings.NewReader(strings.Repeat("x", apiResponseLogLimit+1))), "over_limit"}} {
		a := costBodyAdapter(t, 200, tc.body)
		_, r, err := a.execute(context.Background(), Request{Model: "request"})
		if err == nil {
			t.Fatal("body error lost")
		}
		receipt := oneCostAttempt(t, r)
		if receipt.BodyState != tc.state || receipt.Dispatch != "possibly_dispatched" || receipt.Usage != nil {
			t.Fatalf("receipt=%#v", receipt)
		}
	}
	var calls atomic.Int32
	a, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "synthetic-test-key", HTTPClient: &http.Client{Transport: costRoundTrip(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.DeadlineExceeded })}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, r, _ := a.execute(ctx, Request{Model: "request"})
	if calls.Load() != 0 || oneCostAttempt(t, r).Dispatch != "not_dispatched" {
		t.Fatal("pre-Do cancel dispatched")
	}
	_, r, _ = a.execute(context.Background(), Request{Model: "request"})
	if calls.Load() != 1 || oneCostAttempt(t, r).Dispatch != "possibly_dispatched" {
		t.Fatal("Do error incorrectly free")
	}
	ctx, cancel = context.WithCancel(context.Background())
	a.httpClient = &http.Client{Transport: costRoundTrip(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(costWire)), Header: make(http.Header)}, nil
	})}
	_, r, _ = a.execute(ctx, Request{Model: "request"})
	if oneCostAttempt(t, r).Usage == nil {
		t.Fatal("complete response lost on cancellation")
	}
}

// This _test.go-only assembly seam redirects the adapter-owned standard TLS
// transport to a synthetic local server. No production HTTPClient trust toggle
// is exposed, and arbitrary injected clients remain opaque.
func ownedCostTestAdapter(t *testing.T, handler http.Handler) (*APIAdapter, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	a, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "synthetic-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	transport := a.ownedOpenAIClient.Transport.(*http.Transport)
	transport.Proxy = nil // Route this synthetic owned-transport fixture locally even when HTTPS_PROXY is set.
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: server.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	return a, server
}
func TestOpenAIOwnedClientStopsRedirectsAndHasNoReplayBody(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		var calls atomic.Int32
		a, _ := ownedCostTestAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", "https://api.openai.com/again")
			w.WriteHeader(status)
		}))
		_, response, err := a.execute(context.Background(), Request{Model: "requested", Prompt: "synthetic"})
		if err == nil || calls.Load() != 1 {
			t.Fatalf("redirect %d calls=%d err=%v", status, calls.Load(), err)
		}
		r := oneCostAttempt(t, response)
		if r.TransportMode != "single_send_v1" || r.EndpointKind != "official_global" {
			t.Fatal("owned transport classification missing")
		}
	}
	a, _ := ownedCostTestAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, costWire)
	}))
	original := a.httpClient
	// Inspect the outgoing GetBody through a test-only opaque wrapper. The
	// wrapper is intentionally NOT eligible as a single-send producer.
	a.httpClient = &http.Client{Transport: costRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil {
			t.Error("replayable body")
		}
		return original.Transport.RoundTrip(r)
	})}
	_, response, err := a.execute(context.Background(), Request{Model: "requested"})
	if err != nil {
		t.Fatal(err)
	}
	if oneCostAttempt(t, response).TransportMode != "opaque" {
		t.Fatal("test wrapper inherited owned trust")
	}
}

func TestOpenAICostEnvelopeUnknownFieldsAndUnsafeValues(t *testing.T) {
	for _, field := range []string{"id", "model", "service_tier", "status"} {
		var body map[string]json.RawMessage
		if json.Unmarshal([]byte(costWire), &body) != nil {
			t.Fatal("fixture")
		}
		delete(body, field)
		body["unknown_future"] = json.RawMessage(`{"any":[1,2,3]}`)
		encoded, _ := json.Marshal(body)
		a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(string(encoded))))
		_, response, _ := a.execute(context.Background(), Request{Model: "requested"})
		r := oneCostAttempt(t, response)
		if len(r.Issues) == 0 || r.Usage == nil {
			t.Fatal("unknown fields discarded known usage or missing identity inferred")
		}
	}
	for _, unsafe := range []string{strings.Repeat("x", 257), "model\nsecret", "model\x00secret"} {
		encoded, _ := json.Marshal(unsafe)
		body := strings.Replace(costWire, `"observed-model"`, string(encoded), 1)
		a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
		_, response, _ := a.execute(context.Background(), Request{Model: "requested"})
		if oneCostAttempt(t, response).ObservedModel != nil {
			t.Fatal("unsafe identity retained/truncated")
		}
	}
	if officialResponsesEndpoint("https://api.openai.com/v1/responses?secret=x") || officialResponsesEndpoint("https://gateway.example/v1/responses") || officialResponsesEndpoint("http://api.openai.com/v1/responses") {
		t.Fatal("unverified route qualified")
	}
}

func TestOpenAIRequestCostEachMissingOrNullCounterRemainsUnknown(t *testing.T) {
	for _, field := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens"} {
		for _, missing := range []bool{false, true} {
			var body map[string]any
			if err := json.Unmarshal([]byte(costWire), &body); err != nil {
				t.Fatal(err)
			}
			usage := body["usage"].(map[string]any)
			target := usage
			if field == "cached_tokens" || field == "cache_write_tokens" {
				target = usage["input_tokens_details"].(map[string]any)
			}
			if missing {
				delete(target, field)
			} else {
				target[field] = nil
			}
			encoded, _ := json.Marshal(body)
			a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(string(encoded))))
			_, response, _ := a.execute(context.Background(), Request{Model: "request"})
			receipt := oneCostAttempt(t, response)
			if receipt.Usage == nil || !slices.Contains(receipt.Issues, llm.CostGapUsage) {
				t.Fatalf("%s absence manufactured zero", field)
			}
		}
	}
}
func TestOpenAIRequestCostConstructionAndUnknownDeliveredTier(t *testing.T) {
	a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(costWire)))
	_, response, err := a.execute(nil, Request{Model: "request"}) //nolint:staticcheck // SA1012: nil deliberately tests request-construction failure before dispatch.
	if err == nil || oneCostAttempt(t, response).Dispatch != "not_dispatched" {
		t.Fatal("construction failure dispatched")
	}
	body := strings.Replace(costWire, `"service_tier":"priority"`, `"service_tier":"future-tier"`, 1)
	a = costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
	_, response, err = a.execute(context.Background(), Request{Model: "request"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := oneCostAttempt(t, response)
	if receipt.ObservedServiceTier == nil || *receipt.ObservedServiceTier != "future-tier" || !slices.Contains(receipt.Issues, llm.CostGapIdentity) {
		t.Fatal("unknown delivered tier inferred or discarded")
	}
}
func TestOpenAIOwnedExplicitRetryProducesIndependentReceipts(t *testing.T) {
	var calls atomic.Int32
	a, _ := ownedCostTestAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"message":"synthetic transient"}}`)
			return
		}
		_, _ = io.WriteString(w, costWire)
	}))
	result, err := llm.RunStructuredWithSessionResume(context.Background(), a, "", Request{Model: "requested"}, func(b []byte) (bool, error) {
		var payload struct {
			OK bool `json:"ok"`
		}
		err := json.Unmarshal(b, &payload)
		return payload.OK, err
	})
	if err != nil || !result.Value || calls.Load() != 2 || len(result.Response.RequestCostEvidence.Attempts) != 2 {
		t.Fatalf("explicit retry err=%v history=%#v", err, result.Response.RequestCostEvidence)
	}
	if !slices.Contains(result.Response.RequestCostEvidence.Attempts[0].Issues, llm.CostGapDispatch) {
		t.Fatal("HTTP error erased by later success")
	}
}

func TestOpenAIRequestCostUnicodeIdentityIsNeverReplaced(t *testing.T) {
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"\ud800\u0041"`} {
		body := strings.Replace(costWire, `"observed-model"`, raw, 1)
		a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
		_, response, _ := a.execute(context.Background(), Request{Model: "request"})
		receipt := oneCostAttempt(t, response)
		if receipt.ObservedModel != nil || receipt.Usage == nil || !slices.Contains(receipt.Issues, llm.CostGapIdentity) {
			t.Fatal("surrogate changed identity or erased unrelated usage")
		}
	}
	body := strings.Replace(costWire, `"observed-model"`, "\"\xff\"", 1)
	a := costBodyAdapter(t, 200, io.NopCloser(strings.NewReader(body)))
	_, response, _ := a.execute(context.Background(), Request{Model: "request"})
	receipt := oneCostAttempt(t, response)
	if receipt.BodyState != "malformed" || receipt.ObservedModel != nil || receipt.Usage != nil {
		t.Fatal("invalid UTF-8 whole document prefix parsed")
	}
}
