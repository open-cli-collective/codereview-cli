package llmadapters

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

// RequestCostEvidenceSource opts in by concrete adapter ownership, never by
// model prefix. Start's synchronous configuration errors occur before dispatch.
func (a *APIAdapter) RequestCostEvidenceSource() string {
	if a.kind == apiOpenAI {
		return "openai_responses"
	}
	return ""
}

func newSingleSendOpenAIClient() *http.Client {
	// Pin a fresh standard transport, not mutable http.DefaultTransport. With
	// GetBody cleared and no idempotency header, consumed request bodies cannot
	// be replayed. Explicit outer retries produce independent receipts.
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport, Timeout: defaultAPIClientTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func officialResponsesEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "https" && u.Host == "api.openai.com" && u.User == nil && u.Path == "/v1/responses" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func (a *APIAdapter) newCostAttempt(req Request) *llm.RequestCostAttempt {
	if a.kind != apiOpenAI {
		return nil
	}
	attempt := &llm.RequestCostAttempt{RuntimeID: "openai-api-key", RequestedModel: req.Model, EndpointKind: "custom_or_unverified", TransportMode: "opaque", Dispatch: "not_dispatched", BodyState: "missing", Issues: []llm.CostEvidenceGap{}}
	if req.Fast {
		tier := "fast"
		attempt.RequestedServiceTier = &tier
	}
	if a.ownedOpenAIClient != nil && a.httpClient == a.ownedOpenAIClient {
		attempt.TransportMode = "single_send_v1"
	}
	return attempt
}

func singleCostEvidence(a *llm.RequestCostAttempt) *llm.RequestCostEvidence {
	if a == nil {
		return nil
	}
	return llm.NormalizeRequestCostEvidence(&llm.RequestCostEvidence{Version: 1, Source: "openai_responses", Scope: "invocation", AttemptsObserved: 1, AttemptCountExact: a.Dispatch == "not_dispatched" || a.TransportMode == "single_send_v1", Gaps: []llm.CostEvidenceGap{}, Attempts: []llm.RequestCostAttempt{*a}})
}

// costObject isolates known keys, detecting duplicates without last-key-wins.
// Full-document syntax must be validated by the caller first. Unknown keys are
// ignored and no arbitrary provider metadata enters the returned evidence.
func costObject(data []byte, known ...string) (map[string]json.RawMessage, map[string]bool, bool) {
	allowed := map[string]bool{}
	for _, key := range known {
		allowed[key] = true
	}
	fields := map[string]json.RawMessage{}
	duplicates := map[string]bool{}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, nil, false
	}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, false
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return nil, nil, false
		}
		if !allowed[key] {
			continue
		}
		if _, exists := fields[key]; exists {
			duplicates[key] = true
		}
		fields[key] = raw
	}
	_, err = d.Token()
	return fields, duplicates, err == nil
}

func observeCostEnvelope(a *llm.RequestCostAttempt, body []byte) {
	if a == nil {
		return
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		a.BodyState = "malformed"
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapInvalid)
		return
	}
	fields, duplicates, ok := costObject(body, "id", "model", "service_tier", "status", "usage")
	if !ok {
		a.BodyState = "malformed"
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapInvalid)
		return
	}
	for _, field := range []struct {
		key    string
		target **string
		limit  int
	}{{"id", &a.ProviderResponseID, 256}, {"model", &a.ObservedModel, 256}, {"service_tier", &a.ObservedServiceTier, 64}, {"status", &a.ResponseStatus, 64}} {
		raw, exists := fields[field.key]
		if duplicates[field.key] {
			a.Issues = llm.AddCostGap(a.Issues, llm.CostGapDuplicate)
			continue
		}
		if !exists || bytes.Equal(raw, []byte("null")) {
			continue
		}
		var value string
		if !llm.CostJSONUnicodeValid(raw) || json.Unmarshal(raw, &value) != nil || !llm.SafeCostString(value, field.limit) {
			a.Issues = llm.AddCostGap(a.Issues, llm.CostGapIdentity)
			continue
		}
		*field.target = &value
	}
	if duplicates["usage"] {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapDuplicate)
		return
	}
	raw, exists := fields["usage"]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return
	}
	usage, dups, ok := costObject(raw, "input_tokens", "output_tokens", "total_tokens", "input_tokens_details")
	if !ok {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapUsage)
		return
	}
	u := &llm.OpenAIRequestUsage{}
	a.Usage = u
	for _, field := range []struct {
		key    string
		target **int64
	}{{"input_tokens", &u.InputTokens}, {"output_tokens", &u.OutputTokens}, {"total_tokens", &u.TotalTokens}} {
		decodeCostCounter(a, usage, dups, field.key, field.target)
	}
	if dups["input_tokens_details"] {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapDuplicate)
		return
	}
	details, exists := usage["input_tokens_details"]
	if !exists || bytes.Equal(details, []byte("null")) {
		return
	}
	detailFields, detailDups, ok := costObject(details, "cached_tokens", "cache_write_tokens")
	if !ok {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapUsage)
		return
	}
	decodeCostCounter(a, detailFields, detailDups, "cached_tokens", &u.CacheReadTokens)
	decodeCostCounter(a, detailFields, detailDups, "cache_write_tokens", &u.CacheWriteTokens)
}

func decodeCostCounter(a *llm.RequestCostAttempt, fields map[string]json.RawMessage, duplicates map[string]bool, key string, target **int64) {
	if duplicates[key] {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapDuplicate)
		return
	}
	raw, exists := fields[key]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		a.Issues = llm.AddCostGap(a.Issues, llm.CostGapUsage)
		return
	}
	*target = &value
}
