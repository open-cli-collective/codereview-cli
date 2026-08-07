package stagemodel

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestResolveStageModelUsesConfiguredTierMapping(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
		ModelMap: config.ModelMap{"medium": "profile-medium-model"},
	}}

	got, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageThreadAnalysis,
		Tier:          config.ModelTierMedium,
		DefaultEffort: "medium",
	})
	if err != nil {
		t.Fatalf("ResolveStageModel: %v", err)
	}
	if got.Stage != StageThreadAnalysis {
		t.Fatalf("Stage = %q, want %q", got.Stage, StageThreadAnalysis)
	}
	if got.Tier != config.ModelTierMedium {
		t.Fatalf("Tier = %q, want %q", got.Tier, config.ModelTierMedium)
	}
	if got.Model != "profile-medium-model" {
		t.Fatalf("Model = %q, want profile-medium-model", got.Model)
	}
	if got.Effort != "medium" {
		t.Fatalf("Effort = %q, want medium", got.Effort)
	}
	if got.Source != config.ModelMapSourceConfig {
		t.Fatalf("Source = %q, want %q", got.Source, config.ModelMapSourceConfig)
	}
	if got.Override {
		t.Fatalf("Override = true, want false")
	}
}

func TestResolveStageModelAppliesEffortOverrideWithoutBypassingTier(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}}

	got, err := ResolveStageModel(Request{
		Profile:        profile,
		Stage:          StageSelection,
		Tier:           config.ModelTierMedium,
		EffortOverride: "high",
		DefaultEffort:  "medium",
	})
	if err != nil {
		t.Fatalf("ResolveStageModel: %v", err)
	}
	if got.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", got.Model)
	}
	if got.Effort != "high" {
		t.Fatalf("Effort = %q, want high", got.Effort)
	}
	if got.Source != config.ModelMapSourceBuiltIn {
		t.Fatalf("Source = %q, want %q", got.Source, config.ModelMapSourceBuiltIn)
	}
}

func TestResolveStageModelAppliesTierFloor(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
		ModelMap: config.ModelMap{
			"small": "profile-small-model",
			"large": "profile-large-model",
		},
	}}

	got, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageReviewer,
		Tier:          config.ModelTierSmall,
		FloorTier:     config.ModelTierLarge,
		DefaultEffort: "medium",
	})
	if err != nil {
		t.Fatalf("ResolveStageModel: %v", err)
	}
	if got.Tier != config.ModelTierLarge {
		t.Fatalf("Tier = %q, want %q", got.Tier, config.ModelTierLarge)
	}
	if got.Model != "profile-large-model" {
		t.Fatalf("Model = %q, want profile-large-model", got.Model)
	}
	if got.Source != config.ModelMapSourceConfig {
		t.Fatalf("Source = %q, want %q", got.Source, config.ModelMapSourceConfig)
	}
}

func TestResolveStageModelBypassesTierForExplicitOverride(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderPi,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterPiRPC,
	}}

	got, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageThreadAnalysis,
		Tier:          config.ModelTierLarge,
		ModelOverride: "operator-chosen-model",
		DefaultEffort: "low",
	})
	if err != nil {
		t.Fatalf("ResolveStageModel: %v", err)
	}
	if got.Model != "operator-chosen-model" {
		t.Fatalf("Model = %q, want operator-chosen-model", got.Model)
	}
	if got.Effort != "low" {
		t.Fatalf("Effort = %q, want low", got.Effort)
	}
	if !got.Override {
		t.Fatalf("Override = false, want true")
	}
	if got.Source != "" {
		t.Fatalf("Source = %q, want empty source for explicit override", got.Source)
	}
}

func TestResolveStageModelErrorsForUnmappedTier(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderPi,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterPiRPC,
	}}

	_, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageThreadAnalysis,
		Tier:          config.ModelTierSmall,
		DefaultEffort: "low",
	})
	if err == nil {
		t.Fatal("ResolveStageModel returned nil error, want unmapped tier error")
	}
	if !strings.Contains(err.Error(), "thread_analysis") || !strings.Contains(err.Error(), "model_tier") {
		t.Fatalf("error = %q, want stage and model_tier context", err)
	}
	// The tier an agent asked for is not the operator's vocabulary; the error
	// has to name the config entry that resolves it.
	if !strings.Contains(err.Error(), "llm.model_map.small") {
		t.Fatalf("error = %q, want the config entry that fixes it", err)
	}
}

func TestResolveStageModelMapsSmallTierForClaudeCLI(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterClaudeCLI,
	}}

	resolved, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageReviewer,
		Tier:          config.ModelTierSmall,
		DefaultEffort: "medium",
	})
	if err != nil {
		t.Fatalf("ResolveStageModel: %v", err)
	}
	if resolved.Model != "claude-sonnet-5" || resolved.Source != config.ModelMapSourceBuiltIn {
		t.Fatalf("resolved = %#v, want the built-in Claude CLI small model", resolved)
	}
}

func TestResolveStageModelErrorsForInvalidTierBeforeApplyingFloor(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
	}}

	_, err := ResolveStageModel(Request{
		Profile:       profile,
		Stage:         StageReviewer,
		Tier:          config.ModelTier("flagship"),
		FloorTier:     config.ModelTierSmall,
		DefaultEffort: "medium",
	})
	if err == nil {
		t.Fatal("ResolveStageModel returned nil error, want invalid tier error")
	}
	if !strings.Contains(err.Error(), `model_tier "flagship" is invalid`) {
		t.Fatalf("error = %q, want invalid tier context", err)
	}
}

func TestResolveFirstAvailableUsesFirstConfiguredTier(t *testing.T) {
	// anthropic_api ships no built-in map, so the profile's own entries are the
	// whole map and small stays genuinely unmapped for the fallback to find.
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthAPIKey,
		Adapter:  config.LLMAdapterAnthropicAPI,
		ModelMap: config.ModelMap{string(config.ModelTierMedium): "claude-sonnet-5"},
	}}

	got, ok := ResolveFirstAvailable(Request{
		Profile:       profile,
		Stage:         StageApprovalOverride,
		DefaultEffort: "low",
	}, config.ModelTierSmall, config.ModelTierMedium)
	if !ok {
		t.Fatal("ResolveFirstAvailable ok = false, want medium fallback")
	}
	if got.Tier != config.ModelTierMedium {
		t.Fatalf("Tier = %q, want %q", got.Tier, config.ModelTierMedium)
	}
	if got.Model != "claude-sonnet-5" {
		t.Fatalf("Model = %q, want claude-sonnet-5", got.Model)
	}
	if got.Effort != "low" {
		t.Fatalf("Effort = %q, want low", got.Effort)
	}
}
