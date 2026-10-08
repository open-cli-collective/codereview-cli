package llmadapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

// ErrAPIAdapterConfig reports invalid direct API adapter configuration.
var ErrAPIAdapterConfig = errors.New("llm api: invalid configuration")

var errAPIResponseOverLimit = errors.New("llm api: response body exceeds limit")

const (
	defaultAnthropicBaseURL = "https://api.anthropic.com/"
	defaultOpenAIBaseURL    = "https://api.openai.com/"
	defaultAnthropicVersion = "2023-06-01"
	defaultAPIMaxTokens     = 4096
	defaultAPIClientTimeout = 10 * time.Minute
	apiResponseLogLimit     = 4 * 1024 * 1024
)

type apiKind string

const (
	apiAnthropic apiKind = "anthropic_api"
	apiOpenAI    apiKind = "openai_api"
)

// APIOptions configures direct HTTP LLM adapters.
type APIOptions struct {
	APIKey           string
	HTTPClient       *http.Client
	BaseURL          string
	MaxTokens        int
	AnthropicVersion string
	FastModeModels   []string
}

// APIAdapter calls a direct provider HTTP API as an LLM adapter.
type APIAdapter struct {
	kind              apiKind
	apiKey            string
	httpClient        *http.Client
	ownedOpenAIClient *http.Client
	baseURL           *url.URL
	maxTokens         int
	anthropicVersion  string
	fastModeModels    []string
}

var _ llm.Adapter = (*APIAdapter)(nil)

// NewAPIAdapterFromConfig resolves an API-key LLM adapter from profile config.
func NewAPIAdapterFromConfig(llmConfig config.LLMConfig, store credentials.Reader, opts APIOptions) (*APIAdapter, error) {
	kind, err := apiKindFromConfig(llmConfig)
	if err != nil {
		return nil, err
	}
	if llmConfig.Auth != config.LLMAuthAPIKey {
		return nil, fmt.Errorf("%w: API adapters require api_key auth", ErrAPIAdapterConfig)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: token store is required", ErrAPIAdapterConfig)
	}
	parsed, err := credentials.ParseRef(llmConfig.Credential.Name)
	if err != nil {
		return nil, err
	}
	key, err := credentials.KeyForPurpose(config.CredentialRef{
		Purpose:  "llm",
		Ref:      llmConfig.Credential.Name,
		Mode:     string(llmConfig.Auth),
		Provider: string(llmConfig.Provider),
	})
	if err != nil {
		return nil, err
	}
	apiKey, err := store.Get(parsed.Profile, key)
	if err != nil {
		return nil, fmt.Errorf("%w: read llm credential %s/%s: %w", ErrAPIAdapterConfig, llmConfig.Credential.Name, key, err)
	}
	opts.APIKey = apiKey
	return newAPIAdapter(kind, opts)
}

func apiKindFromConfig(llmConfig config.LLMConfig) (apiKind, error) {
	// The adapter's transport endpoint and credential namespace are fixed Go
	// behavior. Catalog metadata may describe capabilities and defaults, but it
	// must not be able to relabel an adapter into another provider.
	switch llmConfig.Adapter {
	case config.LLMAdapterAnthropicAPI:
		if llmConfig.Provider != config.LLMProviderAnthropic {
			return "", fmt.Errorf("%w: %s requires provider %s", ErrAPIAdapterConfig, llmConfig.Adapter, config.LLMProviderAnthropic)
		}
		return apiAnthropic, nil
	case config.LLMAdapterOpenAIAPI:
		if llmConfig.Provider != config.LLMProviderOpenAI {
			return "", fmt.Errorf("%w: %s requires provider %s", ErrAPIAdapterConfig, llmConfig.Adapter, config.LLMProviderOpenAI)
		}
		return apiOpenAI, nil
	case config.LLMAdapterClaudeCLI, config.LLMAdapterCodexCLI, config.LLMAdapterPiRPC:
		return "", fmt.Errorf("%w: adapter %q is not an API adapter", ErrAPIAdapterConfig, llmConfig.Adapter)
	default:
		return "", fmt.Errorf("%w: unsupported API adapter %q", ErrAPIAdapterConfig, llmConfig.Adapter)
	}
}

func newAPIAdapter(kind apiKind, opts APIOptions) (*APIAdapter, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("%w: API key is required", ErrAPIAdapterConfig)
	}
	if opts.MaxTokens < 0 {
		return nil, fmt.Errorf("%w: max tokens must be non-negative", ErrAPIAdapterConfig)
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultAPIMaxTokens
	}
	baseURL, err := resolveAPIBaseURL(defaultAPIBaseURL(kind), opts.BaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	var ownedOpenAIClient *http.Client
	if httpClient == nil {
		if kind == apiOpenAI {
			ownedOpenAIClient = newSingleSendOpenAIClient()
			httpClient = ownedOpenAIClient
		} else {
			httpClient = &http.Client{Timeout: defaultAPIClientTimeout}
		}
	}
	version := strings.TrimSpace(opts.AnthropicVersion)
	if version == "" {
		version = defaultAnthropicVersion
	}
	return &APIAdapter{
		kind:              kind,
		apiKey:            opts.APIKey,
		httpClient:        httpClient,
		ownedOpenAIClient: ownedOpenAIClient,
		baseURL:           baseURL,
		maxTokens:         maxTokens,
		anthropicVersion:  version,
		fastModeModels:    append([]string(nil), opts.FastModeModels...),
	}, nil
}

func defaultAPIBaseURL(kind apiKind) string {
	switch kind {
	case apiAnthropic:
		return defaultAnthropicBaseURL
	case apiOpenAI:
		return defaultOpenAIBaseURL
	default:
		return ""
	}
}

func resolveAPIBaseURL(defaultBase string, override string) (*url.URL, error) {
	value := strings.TrimSpace(override)
	if value == "" {
		value = defaultBase
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%w: invalid API base URL %q", ErrAPIAdapterConfig, value)
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		copied := *parsed
		copied.Path += "/"
		parsed = &copied
	}
	return parsed, nil
}

// Name returns the adapter name.
func (a *APIAdapter) Name() string { return string(a.kind) }

// SupportsResume reports whether API session resume is implemented.
func (a *APIAdapter) SupportsResume() bool { return false }

// SupportsCacheAccounting reports whether provider usage can include cache fields.
func (a *APIAdapter) SupportsCacheAccounting() bool { return true }

// SupportsCostReporting reports whether provider responses include cost.
func (a *APIAdapter) SupportsCostReporting() bool { return false }

// Quota reports unsupported quota for direct API adapters.
func (a *APIAdapter) Quota(context.Context) (Quota, bool, error) {
	return Quota{}, false, nil
}

// Resume is unsupported until API session persistence is designed.
func (a *APIAdapter) Resume(context.Context, string, Request) (Stream, error) {
	return nil, fmt.Errorf("llm api: resume unsupported for %s", a.kind)
}

// Start begins one provider HTTP request and returns its stream handle.
func (a *APIAdapter) Start(ctx context.Context, req Request) (Stream, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrAPIAdapterConfig)
	}
	if err := validateFastMode(a.Name(), a.fastModeModels, req); err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithCancel(ctx)
	stream := &apiStream{
		baseStream: llm.NewBaseStream(cancel),
	}
	go stream.run(reqCtx, a, req)
	return stream, nil
}

type apiStream struct {
	baseStream
}

func (s *apiStream) run(ctx context.Context, adapter *APIAdapter, req Request) {
	defer s.Cancel()
	start := time.Now()
	sessionID, response, err := adapter.execute(ctx, req)
	recordRequestDuration(&response, start, err)
	s.SetSessionID(sessionID)
	s.Finish(response, err)
}

func (a *APIAdapter) execute(ctx context.Context, req Request) (sessionID string, response Response, resultErr error) {
	cost := a.newCostAttempt(req)
	defer func() { response.RequestCostEvidence = singleCostEvidence(cost) }()
	endpoint, requestBody, err := a.buildProviderRequest(req)
	if err != nil {
		return "", Response{}, err
	}
	if cost != nil && officialResponsesEndpoint(endpoint) {
		cost.EndpointKind = "official_global"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", Response{}, err
	}
	a.applyHeaders(httpReq, req)
	if cost != nil {
		// Clearing GetBody blocks consumed-body transport/redirect replay for
		// the owned client. Injected clients remain explicitly opaque.
		httpReq.GetBody = nil
		if err := ctx.Err(); err != nil {
			return "", Response{}, err
		}
		cost.Dispatch = "possibly_dispatched"
	}
	httpResp, err := a.httpClient.Do(httpReq)
	if cost != nil && httpResp != nil {
		status := httpResp.StatusCode
		cost.HTTPStatus = &status
	}
	if err != nil {
		if isTransientTransportError(err) {
			return "", Response{}, fmt.Errorf("%w: %w", ErrTransient, err)
		}
		return "", Response{}, err
	}
	defer httpResp.Body.Close()
	if cost != nil {
		status := httpResp.StatusCode
		cost.HTTPStatus = &status
	}
	responseBody, err := readAPIResponseBody(httpResp.Body)
	if err != nil {
		if cost != nil {
			cost.BodyState = "read_error"
			if errors.Is(err, errAPIResponseOverLimit) {
				cost.BodyState = "over_limit"
			}
		}
		return "", Response{}, err
	}
	if cost != nil {
		cost.BodyState = "complete"
		observeCostEnvelope(cost, responseBody)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		statusErr := fmt.Errorf("llm api %s: provider returned %s", a.kind, httpResp.Status)
		if classifyHTTPStatusTransient(httpResp.StatusCode) {
			return "", Response{}, fmt.Errorf("%w: %w", ErrTransient, statusErr)
		}
		return "", Response{}, statusErr
	}
	// Response logging is best-effort; a provider response should not be
	// discarded because the caller's local log path is unavailable.
	_ = writeAPIResponseLog(req.LogPath, responseBody)
	return a.parseProviderResponse(responseBody)
}

// isTransientTransportError reports whether an http.Client.Do error is a
// retryable transport failure (a timeout, a reset connection, or an unexpected
// EOF). A canceled request context is intentionally not treated as transient.
func isTransientTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	lowered := strings.ToLower(err.Error())
	return strings.Contains(lowered, "connection reset") || strings.Contains(lowered, "unexpected eof")
}

func (a *APIAdapter) buildProviderRequest(req Request) (string, []byte, error) {
	switch a.kind {
	case apiAnthropic:
		payload := anthropicRequest{
			Model:     req.Model,
			MaxTokens: a.maxTokens,
			Messages:  []anthropicMessage{{Role: "user", Content: req.Prompt}},
		}
		if req.Fast {
			payload.Speed = "fast"
		}
		body, err := json.Marshal(payload)
		return a.url("v1/messages"), body, err
	case apiOpenAI:
		payload := openAIRequest{
			Model:           req.Model,
			Input:           req.Prompt,
			Store:           false,
			MaxOutputTokens: a.maxTokens,
		}
		if strings.TrimSpace(req.Effort) != "" {
			payload.Reasoning = &openAIReasoning{Effort: req.Effort}
		}
		if req.Fast {
			payload.ServiceTier = "fast"
		}
		body, err := json.Marshal(payload)
		return a.url("v1/responses"), body, err
	default:
		return "", nil, fmt.Errorf("%w: unknown API adapter %q", ErrAPIAdapterConfig, a.kind)
	}
}

func (a *APIAdapter) applyHeaders(req *http.Request, llmReq Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	switch a.kind {
	case apiAnthropic:
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", a.anthropicVersion)
		if llmReq.Fast {
			req.Header.Set("anthropic-beta", "fast-mode-2026-02-01")
		}
	case apiOpenAI:
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
}

func (a *APIAdapter) url(path string) string {
	endpoint := *a.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return endpoint.String()
}

func (a *APIAdapter) parseProviderResponse(body []byte) (string, Response, error) {
	switch a.kind {
	case apiAnthropic:
		return parseAnthropicResponse(body)
	case apiOpenAI:
		return parseOpenAIResponse(body)
	default:
		return "", Response{}, fmt.Errorf("%w: unknown API adapter %q", ErrAPIAdapterConfig, a.kind)
	}
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	Speed     string             `json:"speed,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	ID      string                  `json:"id"`
	Content []anthropicContentBlock `json:"content"`
	Usage   anthropicUsage          `json:"usage"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens              *int   `json:"input_tokens"`
	OutputTokens             *int   `json:"output_tokens"`
	CacheReadInputTokens     *int   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int   `json:"cache_creation_input_tokens"`
	Speed                    string `json:"speed"`
	CacheCreation            struct {
		Ephemeral5mInputTokens *int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens *int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func parseAnthropicResponse(body []byte) (string, Response, error) {
	var payload anthropicResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", Response{}, fmt.Errorf("llm api anthropic_api: malformed response JSON: %w", err)
	}
	text := strings.Builder{}
	for _, block := range payload.Content {
		if block.Type == "text" && block.Text != "" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return payload.ID, Response{}, errors.New("llm api anthropic_api: no text output")
	}
	return payload.ID, Response{
		StructuredOutput: []byte(text.String()),
		Usage: Usage{
			TokensIn:      payload.Usage.InputTokens,
			TokensOut:     payload.Usage.OutputTokens,
			CacheRead:     payload.Usage.CacheReadInputTokens,
			CacheCreate:   payload.Usage.CacheCreationInputTokens,
			CacheCreate5m: payload.Usage.CacheCreation.Ephemeral5mInputTokens,
			CacheCreate1h: payload.Usage.CacheCreation.Ephemeral1hInputTokens,
			Speed:         payload.Usage.Speed,
		},
	}, nil
}

type openAIRequest struct {
	Model           string           `json:"model"`
	Input           string           `json:"input"`
	Store           bool             `json:"store"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Reasoning       *openAIReasoning `json:"reasoning,omitempty"`
	ServiceTier     string           `json:"service_tier,omitempty"`
}

type openAIReasoning struct {
	Effort string `json:"effort"`
}

type openAIResponse struct {
	ID          string         `json:"id"`
	OutputText  string         `json:"output_text"`
	Output      []openAIOutput `json:"output"`
	Usage       openAIUsage    `json:"usage"`
	ServiceTier string         `json:"service_tier"`
}

type openAIOutput struct {
	Type    string                `json:"type"`
	Content []openAIOutputContent `json:"content"`
}

type openAIOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openAIUsage struct {
	InputTokens        *int                     `json:"input_tokens"`
	OutputTokens       *int                     `json:"output_tokens"`
	InputTokensDetails openAIInputTokensDetails `json:"input_tokens_details"`
}

type openAIInputTokensDetails struct {
	CachedTokens     *int `json:"cached_tokens"`
	CacheWriteTokens *int `json:"cache_write_tokens"`
}

func parseOpenAIResponse(body []byte) (string, Response, error) {
	var payload openAIResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", Response{}, fmt.Errorf("llm api openai_api: malformed response JSON: %w", err)
	}
	text := strings.Builder{}
	for _, output := range payload.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" && content.Text != "" {
				text.WriteString(content.Text)
			}
		}
	}
	if text.Len() == 0 && payload.OutputText != "" {
		text.WriteString(payload.OutputText)
	}
	speed := ""
	switch strings.ToLower(strings.TrimSpace(payload.ServiceTier)) {
	case "fast", "priority":
		speed = "fast"
	case "standard", "default":
		speed = "standard"
	}
	// OpenAI's input total includes cache reads and writes. Keep that total
	// intact rather than treating cache writes as an additional input bucket.
	response := Response{
		StructuredOutput: []byte(text.String()),
		Usage: Usage{
			TokensIn:    payload.Usage.InputTokens,
			TokensOut:   payload.Usage.OutputTokens,
			CacheRead:   payload.Usage.InputTokensDetails.CachedTokens,
			CacheCreate: payload.Usage.InputTokensDetails.CacheWriteTokens,
			Speed:       speed,
		},
	}
	if text.Len() == 0 {
		return payload.ID, response, errors.New("llm api openai_api: no text output")
	}
	return payload.ID, response, nil
}

func readAPIResponseBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, apiResponseLogLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > apiResponseLogLimit {
		return nil, errAPIResponseOverLimit
	}
	return body, nil
}

func writeAPIResponseLog(path string, body []byte) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	// #nosec G304 -- log path is an explicit caller-provided request field.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return err
	}
	_, err = file.Write([]byte("\n"))
	return err
}
