package llm

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
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode body: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("request body includes tools: %#v", body)
		}
		if got := body["model"]; got != "claude-3-5-sonnet" {
			t.Fatalf("model = %#v, want claude-3-5-sonnet", got)
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

	adapter, err := NewAnthropicAPIAdapter(APIOptions{APIKey: "anthropic-key", BaseURL: server.URL}) // #nosec G101 -- test credential placeholder.
	if err != nil {
		t.Fatalf("NewAnthropicAPIAdapter: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "anthropic.jsonl")
	stream, err := adapter.Start(context.Background(), Request{Model: "claude-3-5-sonnet", Prompt: "prompt", LogPath: logPath})
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
	if response.Usage.CacheRead == nil || *response.Usage.CacheRead != 3 {
		t.Fatalf("CacheRead = %#v, want 3", response.Usage.CacheRead)
	}
	if response.Usage.CacheCreate != nil || response.Usage.CostUSD != nil {
		t.Fatalf("Usage = %#v, want nil cache create and cost", response.Usage)
	}
	assertLogContains(t, logPath, `"msg_1"`)
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
		if body["model"] != "gpt-5.1" || body["input"] != "prompt" {
			t.Fatalf("request body = %#v, want model and input", body)
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{"input_tokens":0,"output_tokens":12,"input_tokens_details":{"cached_tokens":0}}}`)
	}))
	defer server.Close()

	adapter, err := NewOpenAIAPIAdapter(APIOptions{APIKey: "openai-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAIAPIAdapter: %v", err)
	}
	stream, err := adapter.Start(context.Background(), Request{Model: "gpt-5.1", Effort: "high", Prompt: "prompt"})
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
	if response.Usage.TokensIn == nil || *response.Usage.TokensIn != 0 {
		t.Fatalf("TokensIn = %#v, want explicit zero pointer", response.Usage.TokensIn)
	}
	if response.Usage.CacheRead == nil || *response.Usage.CacheRead != 0 {
		t.Fatalf("CacheRead = %#v, want explicit zero pointer", response.Usage.CacheRead)
	}
	if response.Usage.CacheCreate != nil || response.Usage.CostUSD != nil {
		t.Fatalf("Usage = %#v, want nil cache create and cost", response.Usage)
	}
}

func TestAPIAdapterFromConfig(t *testing.T) {
	store := &apiTestStore{values: map[string]map[string]string{
		"work-llm": {credentials.LLMAPIKeyKey: "stored-key"},
	}}
	adapter, err := NewAPIAdapterFromConfig(config.LLMConfig{
		Provider:      config.LLMProviderAnthropic,
		Auth:          config.LLMAuthAPIKey,
		Adapter:       config.LLMAdapterAnthropicAPI,
		CredentialRef: "codereview/work-llm",
	}, store, APIOptions{BaseURL: "https://example.invalid"})
	if err != nil {
		t.Fatalf("NewAPIAdapterFromConfig: %v", err)
	}
	if adapter.Name() != "anthropic_api" || adapter.apiKey != "stored-key" {
		t.Fatalf("adapter = %s key=%q, want anthropic_api stored key", adapter.Name(), adapter.apiKey)
	}
	if len(store.calls) != 1 || store.calls[0] != "work-llm/"+credentials.LLMAPIKeyKey {
		t.Fatalf("store calls = %#v, want work-llm llm key", store.calls)
	}

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

func TestAPIAdapterFailures(t *testing.T) {
	if _, err := NewAnthropicAPIAdapter(APIOptions{APIKey: "key", MaxTokens: -1}); !errors.Is(err, ErrAPIAdapterConfig) {
		t.Fatalf("negative max tokens error = %v, want ErrAPIAdapterConfig", err)
	}

	t.Run("no network for invalid model", func(t *testing.T) {
		var called bool
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		defer server.Close()
		adapter, err := NewAnthropicAPIAdapter(APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("NewAnthropicAPIAdapter: %v", err)
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
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret body", http.StatusUnauthorized)
		}))
		defer server.Close()
		adapter, err := NewOpenAIAPIAdapter(APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("NewOpenAIAPIAdapter: %v", err)
		}
		stream, err := adapter.Start(context.Background(), Request{Model: "gpt", Prompt: "prompt"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = stream.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "openai_api") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("Wait error = %v, want provider and status", err)
		}
		if strings.Contains(err.Error(), "secret body") {
			t.Fatalf("error leaked response body: %v", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":`)
		}))
		defer server.Close()
		adapter, err := NewAnthropicAPIAdapter(APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("NewAnthropicAPIAdapter: %v", err)
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
		adapter, err := NewOpenAIAPIAdapter(APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("NewOpenAIAPIAdapter: %v", err)
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

	t.Run("wait cancellation cancels request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()
		adapter, err := NewAnthropicAPIAdapter(APIOptions{APIKey: "key", BaseURL: server.URL})
		if err != nil {
			t.Fatalf("NewAnthropicAPIAdapter: %v", err)
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
