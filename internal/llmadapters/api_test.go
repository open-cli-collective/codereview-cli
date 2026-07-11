package llmadapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

func TestAnthropicAPIAdapterRequestAndResponse(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Fatalf("request = %s %s, want POST /v1/messages", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "anthropic-key" {
			t.Fatalf("x-api-key = %q, want anthropic-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got != defaultAnthropicVersion {
			t.Fatalf("anthropic-version = %q, want %s", got, defaultAnthropicVersion)
		}
		if got := r.Header.Get("anthropic-beta"); got != "" {
			t.Fatalf("anthropic-beta = %q, want empty without fast request", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode body: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("request body includes tools: %#v", body)
		}
		if _, ok := body["speed"]; ok {
			t.Fatalf("request body includes speed without fast request: %#v", body)
		}
		if got := body["model"]; got != "claude-sonnet-4-6" {
			t.Fatalf("model = %#v, want claude-sonnet-4-6", got)
		}
		if got, ok := body["max_tokens"].(float64); !ok || got <= 0 {
			t.Fatalf("max_tokens = %#v, want positive number", body["max_tokens"])
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %#v, want one message", body["messages"])
		}
		message, ok := messages[0].(map[string]any)
		if !ok || message["role"] != "user" || message["content"] != "prompt" {
			t.Fatalf("message = %#v, want user prompt", messages[0])
		}
		_, _ = fmt.Fprint(w, `{"id":"msg_1","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input_tokens":7,"output_tokens":11,"cache_read_input_tokens":3,"cache_creation_input_tokens":null}}`)
	}))
	defer server.Close()

	adapter, err := newAPIAdapter(apiAnthropic, APIOptions{APIKey: "anthropic-key", BaseURL: server.URL}) // #nosec G101 -- test credential placeholder.
	if err != nil {
		t.Fatalf("newAPIAdapter: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "anthropic.jsonl")
	stream, err := adapter.Start(context.Background(), Request{Model: "claude-sonnet-4-6", Prompt: "prompt", LogPath: logPath})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !sawRequest {
		t.Fatal("server saw no request")
	}
	if stream.SessionID() != "msg_1" {
		t.Fatalf("SessionID = %q, want msg_1", stream.SessionID())
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want JSON text", response.StructuredOutput)
	}
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 7 {
		t.Fatalf("TokensIn = %#v, want 7", response.Usage.TokensIn)
	}
	if response.Usage.TokensOut == nil || *response.Usage.TokensOut != 11 {
		t.Fatalf("TokensOut = %#v, want 11", response.Usage.TokensOut)
	}
	if response.Usage.CacheRead == nil || *response.Usage.CacheRead != 3 {
		t.Fatalf("CacheRead = %#v, want 3", response.Usage.CacheRead)
	}
	if response.Usage.CacheCreate != nil || response.Usage.CostUSD != nil {
		t.Fatalf("Usage = %#v, want nil cache create and cost", response.Usage)
	}
	assertLogContains(t, logPath, `"msg_1"`)
}

func TestAnthropicAPIAdapterFastRequest(t *testing.T) {
	adapter, err := newAPIAdapter(apiAnthropic, APIOptions{
		APIKey:         "anthropic-key",
		FastModeModels: []string{"claude-opus-4-8"},
	}) // #nosec G101 -- test credential placeholder.
	if err != nil {
		t.Fatalf("newAPIAdapter: %v", err)
	}
	req := Request{Model: "claude-opus-4-8", Prompt: "prompt", Fast: true}
	_, body, err := adapter.buildProviderRequest(req)
	if err != nil {
		t.Fatalf("buildProviderRequest: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("Unmarshal request: %v", err)
	}
	if payload["speed"] != "fast" {
		t.Fatalf("request body = %#v, want speed fast", payload)
	}
	httpReq, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	adapter.applyHeaders(httpReq, req)
	if got := httpReq.Header.Get("anthropic-beta"); got != "fast-mode-2026-02-01" {
		t.Fatalf("anthropic-beta = %q, want fast-mode-2026-02-01", got)
	}
}

func TestParseAnthropicResponseSpeed(t *testing.T) {
	for _, speed := range []string{"fast", "standard"} {
		t.Run(speed, func(t *testing.T) {
			_, response, err := parseAnthropicResponse([]byte(fmt.Sprintf(`{"id":"msg_1","content":[{"type":"text","text":"{}"}],"usage":{"speed":%q}}`, speed)))
			if err != nil {
				t.Fatalf("parseAnthropicResponse: %v", err)
			}
			if response.Usage.Speed != speed {
				t.Fatalf("speed = %q, want %q", response.Usage.Speed, speed)
			}
		})
	}
}

func TestOpenAIAPIAdapterRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Fatalf("request = %s %s, want POST /v1/responses", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("Authorization = %q, want bearer key", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode body: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("request body includes tools: %#v", body)
		}
		if body["store"] != false {
			t.Fatalf("store = %#v, want false", body["store"])
		}
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != "high" {
			t.Fatalf("reasoning = %#v, want effort high", body["reasoning"])
		}
		if body["model"] != "gpt-5.4" || body["input"] != "prompt" {
			t.Fatalf("request body = %#v, want model and input", body)
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{"input_tokens":5,"output_tokens":12,"input_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer server.Close()

	adapter, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "openai-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("newAPIAdapter: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "openai.jsonl")
	stream, err := adapter.Start(context.Background(), Request{Model: "gpt-5.4", Effort: "high", Prompt: "prompt", LogPath: logPath})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if stream.SessionID() != "resp_1" {
		t.Fatalf("SessionID = %q, want resp_1", stream.SessionID())
	}
	if string(response.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("StructuredOutput = %s, want JSON text", response.StructuredOutput)
	}
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 5 {
		t.Fatalf("TokensIn = %#v, want 5", response.Usage.TokensIn)
	}
	if response.Usage.TokensOut == nil || *response.Usage.TokensOut != 12 {
		t.Fatalf("TokensOut = %#v, want 12", response.Usage.TokensOut)
	}
	if response.Usage.CacheRead == nil || *response.Usage.CacheRead != 2 {
		t.Fatalf("CacheRead = %#v, want 2", response.Usage.CacheRead)
	}
	if response.Usage.CacheCreate != nil || response.Usage.CostUSD != nil {
		t.Fatalf("Usage = %#v, want nil cache create and cost", response.Usage)
	}
	assertLogContains(t, logPath, `"resp_1"`)
}

func TestAPIAdapterFromConfig(t *testing.T) {
	for _, tt := range []struct {
		name     string
		cfg      config.LLMConfig
		apiKey   string
		want     string
		wantKey  string
		storeKey string
	}{
		{
			name: "anthropic",
			cfg: config.LLMConfig{
				Provider:      config.LLMProviderAnthropic,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterAnthropicAPI,
				CredentialRef: "codereview/work-llm",
			},
			apiKey:   "stored-value",
			want:     "anthropic_api",
			wantKey:  "stored-value",
			storeKey: credentials.AnthropicAPIKeyKey,
		},
		{
			name: "openai",
			cfg: config.LLMConfig{
				Provider:      config.LLMProviderOpenAI,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterOpenAIAPI,
				CredentialRef: "codereview/work-llm",
			},
			apiKey:   "openai-stored-value",
			want:     "openai_api",
			wantKey:  "openai-stored-value",
			storeKey: credentials.OpenAIAPIKeyKey,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &apiTestStore{values: map[string]map[string]string{
				"work-llm": {tt.storeKey: tt.apiKey},
			}}
			adapter, err := NewAPIAdapterFromConfig(tt.cfg, store, APIOptions{BaseURL: "https://example.invalid"})
			if err != nil {
				t.Fatalf("NewAPIAdapterFromConfig: %v", err)
			}
			if adapter.Name() != tt.want || adapter.apiKey != tt.wantKey {
				t.Fatalf("adapter = %s key=%q, want %s stored key", adapter.Name(), adapter.apiKey, tt.want)
			}
			if len(store.calls) != 1 || store.calls[0] != "work-llm/"+tt.storeKey {
				t.Fatalf("store calls = %#v, want work-llm %s", store.calls, tt.storeKey)
			}
		})
	}

	t.Run("legacy key is not a fallback", func(t *testing.T) {
		store := &apiTestStore{values: map[string]map[string]string{
			"work-llm": {credentials.LegacyLLMAPIKeyKey: "stored-value"},
		}}
		_, err := NewAPIAdapterFromConfig(config.LLMConfig{
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/work-llm",
		}, store, APIOptions{})
		if !errors.Is(err, ErrAPIAdapterConfig) {
			t.Fatalf("NewAPIAdapterFromConfig error = %v, want ErrAPIAdapterConfig", err)
		}
		if !strings.Contains(err.Error(), credentials.AnthropicAPIKeyKey) {
			t.Fatalf("NewAPIAdapterFromConfig error = %v, want expected key", err)
		}
		if len(store.calls) != 1 || store.calls[0] != "work-llm/"+credentials.AnthropicAPIKeyKey {
			t.Fatalf("store calls = %#v, want provider-specific key", store.calls)
		}
	})

	for _, tt := range []struct {
		name string
		cfg  config.LLMConfig
	}{
		{
			name: "subscription refused",
			cfg: config.LLMConfig{
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterAnthropicAPI,
			},
		},
		{
			name: "provider mismatch refused",
			cfg: config.LLMConfig{
				Provider:      config.LLMProviderOpenAI,
				Auth:          config.LLMAuthAPIKey,
				Adapter:       config.LLMAdapterAnthropicAPI,
				CredentialRef: "codereview/work-llm",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &apiTestStore{values: map[string]map[string]string{
				"work-llm": {credentials.AnthropicAPIKeyKey: "stored-value"},
			}}
			if _, err := NewAPIAdapterFromConfig(tt.cfg, store, APIOptions{}); !errors.Is(err, ErrAPIAdapterConfig) {
				t.Fatalf("NewAPIAdapterFromConfig error = %v, want ErrAPIAdapterConfig", err)
			}
		})
	}

	if _, err := NewAPIAdapterFromConfig(config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterOpenAIAPI,
		CredentialRef: "codereview/work-llm",
	}, nil, APIOptions{}); !errors.Is(err, ErrAPIAdapterConfig) {
		t.Fatalf("nil store error = %v, want ErrAPIAdapterConfig", err)
	}
	if _, err := NewAPIAdapterFromConfig(config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterAnthropicAPI,
		CredentialRef: "codereview/missing",
	}, nil, APIOptions{}); !errors.Is(err, ErrAPIAdapterConfig) || !strings.Contains(err.Error(), "requires provider anthropic") {
		t.Fatalf("mismatch with nil store error = %v, want provider/adapter validation", err)
	}
}

func TestAPIAdapterFromConfigUsesCachingReaderAcrossRepeatedReads(t *testing.T) {
	base := &apiTestStore{values: map[string]map[string]string{
		"work-llm": {credentials.OpenAIAPIKeyKey: "cached-openai-key"},
	}}
	reader := credentials.CachingReader("llm-store", base)
	cfg := config.LLMConfig{
		Provider:      config.LLMProviderOpenAI,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterOpenAIAPI,
		CredentialRef: "codereview/work-llm",
	}

	first, err := NewAPIAdapterFromConfig(cfg, reader, APIOptions{BaseURL: "https://example.invalid"})
	if err != nil {
		t.Fatalf("first NewAPIAdapterFromConfig: %v", err)
	}
	second, err := NewAPIAdapterFromConfig(cfg, reader, APIOptions{BaseURL: "https://example.invalid"})
	if err != nil {
		t.Fatalf("second NewAPIAdapterFromConfig: %v", err)
	}
	if first.apiKey != "cached-openai-key" || second.apiKey != "cached-openai-key" {
		t.Fatalf("api keys = (%q,%q), want cached-openai-key", first.apiKey, second.apiKey)
	}
	if len(base.calls) != 1 || base.calls[0] != "work-llm/"+credentials.OpenAIAPIKeyKey {
		t.Fatalf("store calls = %#v, want one cached read", base.calls)
	}
}

func TestAPIAdapterFailures(t *testing.T) {
	if _, err := newAPIAdapter(apiAnthropic, APIOptions{APIKey: "key", MaxTokens: -1}); !errors.Is(err, ErrAPIAdapterConfig) {
		t.Fatalf("negative max tokens error = %v, want ErrAPIAdapterConfig", err)
	}

	t.Run("no network for invalid model", func(t *testing.T) {
		var called bool
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		defer server.Close()
		adapter, err := newAPIAdapter(apiAnthropic, APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		_, err = adapter.Start(context.Background(), Request{Prompt: "prompt"})
		if !errors.Is(err, ErrAPIAdapterConfig) {
			t.Fatalf("Start error = %v, want ErrAPIAdapterConfig", err)
		}
		if called {
			t.Fatal("server was called for invalid model")
		}
	})

	t.Run("non 2xx status", func(t *testing.T) {
		apiKey := "sk-llm-no-leak-canary-0001"            // #nosec G101 -- distinctive test canary, not a real API key.
		responseSecret := "llm-upstream-body-secret-0002" // #nosec G101 -- distinctive test canary, not a real secret.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
				t.Fatalf("Authorization = %q, want bearer API key", got)
			}
			http.Error(w, "secret body "+responseSecret+" "+apiKey, http.StatusUnauthorized)
		}))
		defer server.Close()
		adapter, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: apiKey, BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		logPath := filepath.Join(t.TempDir(), "error.jsonl")
		stream, err := adapter.Start(context.Background(), Request{Model: "gpt", Prompt: "prompt", LogPath: logPath})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "openai_api") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("Wait error = %v, want provider and status", err)
		}
		if leakErr := credstore.NoLeakAssertion([]byte(err.Error()), apiKey, responseSecret); leakErr != nil {
			t.Fatalf("error leaked response body or API key: %v", leakErr)
		}
		if data, readErr := os.ReadFile(logPath); readErr == nil { // #nosec G304 -- logPath is under t.TempDir.
			if leakErr := credstore.NoLeakAssertion(data, apiKey, responseSecret); leakErr != nil {
				t.Fatalf("response log leaked response body or API key: %v", leakErr)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("ReadFile(%s): %v", logPath, readErr)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":`)
		}))
		defer server.Close()
		adapter, err := newAPIAdapter(apiAnthropic, APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		stream, err := adapter.Start(context.Background(), Request{Model: "claude", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "malformed response JSON") {
			t.Fatalf("Wait error = %v, want malformed JSON", err)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"resp"}`)
		}))
		defer server.Close()
		adapter, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		stream, err := adapter.Start(context.Background(), Request{Model: "gpt", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no text output") {
			t.Fatalf("Wait error = %v, want no text output", err)
		}
	})

	t.Run("log failure does not discard successful response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"resp_1","output_text":"{\"ok\":true}","usage":{}}`)
		}))
		defer server.Close()
		adapter, err := newAPIAdapter(apiOpenAI, APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		stream, err := adapter.Start(context.Background(), Request{
			Model:   "gpt",
			Prompt:  "prompt",
			LogPath: filepath.Join(t.TempDir(), "missing", "openai.jsonl"),
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		response, err := stream.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if string(response.StructuredOutput) != `{"ok":true}` {
			t.Fatalf("StructuredOutput = %s, want JSON text", response.StructuredOutput)
		}
	})

	t.Run("wait cancellation cancels request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()
		adapter, err := newAPIAdapter(apiAnthropic, APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("newAPIAdapter: %v", err)
		}
		stream, err := adapter.Start(context.Background(), Request{Model: "claude", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = stream.Wait(waitCtx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait error = %v, want context canceled", err)
		}
	})
}

func TestOpenAIAPIOutputTextFallback(t *testing.T) {
	sessionID, response, err := parseOpenAIResponse([]byte(`{"id":"resp_2","output_text":"{\"fallback\":true}","output":[],"usage":{}}`))
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if sessionID != "resp_2" {
		t.Fatalf("sessionID = %q, want resp_2", sessionID)
	}
	if string(response.StructuredOutput) != `{"fallback":true}` {
		t.Fatalf("StructuredOutput = %s, want fallback output text", response.StructuredOutput)
	}
}

func TestAPIResponseLogAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.jsonl")
	if err := writeAPIResponseLog(path, []byte(`{"id":"first"}`)); err != nil {
		t.Fatalf("writeAPIResponseLog(first): %v", err)
	}
	if err := writeAPIResponseLog(path, []byte(`{"id":"second"}`)); err != nil {
		t.Fatalf("writeAPIResponseLog(second): %v", err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path created with t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if got := string(data); !strings.Contains(got, `"first"`) || !strings.Contains(got, `"second"`) {
		t.Fatalf("log = %q, want both appended entries", got)
	}
}

type apiTestStore struct {
	values map[string]map[string]string
	calls  []string
}

func (s *apiTestStore) Get(profile string, key string) (string, error) {
	s.calls = append(s.calls, profile+"/"+key)
	if keys, ok := s.values[profile]; ok {
		if value, ok := keys[key]; ok {
			return value, nil
		}
	}
	return "", errors.New("missing credential")
}

func assertLogContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path created with t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("log = %q, want %q", data, want)
	}
}
