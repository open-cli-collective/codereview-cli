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

func TestValidateEffortForRuntimeRejectsExtendedEffortForOtherRuntimes(t *testing.T) {
	llm := LLMConfig{
		Provider: LLMProviderAnthropic,
		Auth:     LLMAuthSubscription,
		Adapter:  LLMAdapterClaudeCLI,
	}
	err := ValidateEffortForRuntime(llm, "xhigh")
	if err == nil || !errors.Is(err, ErrUnsupportedEffort) || !strings.Contains(err.Error(), `effort "xhigh" is unsupported`) || !strings.Contains(err.Error(), "claude_cli") {
		t.Fatalf("ValidateEffortForRuntime error = %v", err)
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
