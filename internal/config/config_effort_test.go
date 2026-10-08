package config

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateEffortForRuntimeAllowsExtendedPiEffort(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderPi,
		Auth:     LLMAuthSubscription,
		Adapter:  LLMAdapterPiRPC,
	}
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		if err := ValidateEffortForRuntime(llm, effort); err != nil {
			t.Fatalf("ValidateEffortForRuntime(%q): %v", effort, err)
		}
	}
}

func TestValidateEffortForRuntimeAllowsExtendedClaudeCLIEffort(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderAnthropic,
		Auth:     LLMAuthSubscription,
		Adapter:  LLMAdapterClaudeCLI,
	}
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		if err := ValidateEffortForRuntime(llm, effort); err != nil {
			t.Fatalf("ValidateEffortForRuntime(%q): %v", effort, err)
		}
	}
}

func TestValidateEffortForRuntimeRejectsExtendedEffortForAnthropicAPI(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderAnthropic,
		Auth:     LLMAuthAPIKey,
		Adapter:  LLMAdapterAnthropicAPI,
	}
	for _, effort := range []string{"xhigh", "max"} {
		err := ValidateEffortForRuntime(llm, effort)
		if err == nil || !errors.Is(err, ErrUnsupportedEffort) || !strings.Contains(err.Error(), `effort "`+effort+`" is unsupported`) || !strings.Contains(err.Error(), "anthropic_api") {
			t.Fatalf("ValidateEffortForRuntime(%q) error = %v", effort, err)
		}
	}
}

func TestValidateEffortForRuntimeRejectsUnknownEffort(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderPi,
		Auth:     LLMAuthSubscription,
		Adapter:  LLMAdapterPiRPC,
	}
	err := ValidateEffortForRuntime(llm, "ultra")
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `effort "ultra" is invalid`) {
		t.Fatalf("ValidateEffortForRuntime error = %v", err)
	}
}

func TestValidateEffortForRuntimeAllowsKnownProviderAdapterWithOmittedAuth(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderOpenAI,
		Adapter:  LLMAdapterOpenAIAPI,
	}
	if err := ValidateEffortForRuntime(llm, "medium"); err != nil {
		t.Fatalf("ValidateEffortForRuntime: %v", err)
	}
}
