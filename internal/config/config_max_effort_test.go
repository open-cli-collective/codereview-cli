package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
)

func TestValidateAcceptsMaxEffortCeiling(t *testing.T) {
	cfg := validFile()
	runtime := cfg.LLMRuntimes["home-llm"]
	runtime.MaxEffort = EffortMap{"large": "medium"}
	cfg.LLMRuntimes["home-llm"] = runtime
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}
}

func TestValidateRejectsUnknownMaxEffortTier(t *testing.T) {
	cfg := validFile()
	runtime := cfg.LLMRuntimes["home-llm"]
	runtime.MaxEffort = EffortMap{"enormous": "medium"}
	cfg.LLMRuntimes["home-llm"] = runtime
	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "max_effort") {
		t.Fatalf("Validate error = %v, want max_effort mention", err)
	}
}

func TestValidateRejectsUnknownMaxEffortValue(t *testing.T) {
	cfg := validFile()
	runtime := cfg.LLMRuntimes["home-llm"]
	runtime.MaxEffort = EffortMap{"large": "ultra"}
	cfg.LLMRuntimes["home-llm"] = runtime
	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
		t.Fatalf("Validate error = %v, want valid-value mention", err)
	}
}

func TestValidateRejectsMaxEffortUnsupportedByRuntime(t *testing.T) {
	cfg := validFile()
	runtime := cfg.LLMRuntimes["home-llm"]
	runtime.MaxEffort = EffortMap{"large": "xhigh"}
	cfg.LLMRuntimes["home-llm"] = runtime
	err := Validate(cfg)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `effort "xhigh" is unsupported`) {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestValidateAcceptsExtendedMaxEffortForPiRPC(t *testing.T) {
	cfg := validFile()
	cfg.LLMRuntimes["home-llm"] = LLMConfig{
		Provider:  LLMProviderPi,
		Auth:      LLMAuthSubscription,
		Adapter:   LLMAdapterPiRPC,
		MaxEffort: EffortMap{"small": "max"},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestResolveMaxEffortReportsUncappedTiers(t *testing.T) {
	llm := LLMConfig{MaxEffort: EffortMap{"large": "medium"}}
	got, ok := ResolveMaxEffort(llm, ModelTierLarge)
	if !ok || got != modelprefs.EffortMedium {
		t.Fatalf("ResolveMaxEffort(large) = %q, %v; want medium, true", got, ok)
	}
	if _, ok := ResolveMaxEffort(llm, ModelTierMedium); ok {
		t.Fatalf("ResolveMaxEffort(medium) reported a ceiling, want uncapped")
	}
	if _, ok := ResolveMaxEffort(llm, ModelTier("bogus")); ok {
		t.Fatalf("ResolveMaxEffort(bogus) reported a ceiling, want uncapped")
	}
}

func TestLLMConfigNormalizedTrimsAndCopiesMaxEffort(t *testing.T) {
	original := LLMConfig{MaxEffort: EffortMap{" large ": " medium "}}
	normalized := original.normalized()
	if got := normalized.MaxEffort["large"]; got != "medium" {
		t.Fatalf("normalized max_effort = %q, want trimmed medium", got)
	}
	normalized.MaxEffort["large"] = "high"

	if got := original.MaxEffort[" large "]; got != " medium " {
		t.Fatalf("original max_effort changed through normalized copy: %q", got)
	}
	if got := normalized.MaxEffort["large"]; got != "high" {
		t.Fatalf("normalized max_effort = %q, want independent copy", got)
	}
}

func TestNormalizeProjectsInlineRuntimesWithDistinctMaxEffort(t *testing.T) {
	base := Profile{LLM: LLMConfig{
		Provider: LLMProviderOpenAI,
		Auth:     LLMAuthSubscription,
		Adapter:  LLMAdapterCodexCLI,
		ModelMap: ModelMap{"large": "sol"},
	}}
	capped := base
	capped.LLM.MaxEffort = EffortMap{"large": "medium"}

	normalized := Normalize(File{Profiles: map[string]Profile{
		"base":   base,
		"capped": capped,
	}})
	baseRuntime := normalized.Profiles["base"].LLMRuntime
	cappedRuntime := normalized.Profiles["capped"].LLMRuntime
	if baseRuntime == "" || cappedRuntime == "" || baseRuntime == cappedRuntime {
		t.Fatalf("inline runtime identities = %q/%q, want distinct runtimes", baseRuntime, cappedRuntime)
	}
	if got := normalized.LLMRuntimes[cappedRuntime].MaxEffort["large"]; got != "medium" {
		t.Fatalf("capped runtime max_effort = %q, want medium", got)
	}
}
