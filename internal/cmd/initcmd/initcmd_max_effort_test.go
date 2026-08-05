package initcmd

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// A runtime round trip must preserve every LLMConfig field init does not edit.
// max_effort has no init editor, so a drop here silently discards a user's
// hand-written cost ceiling.
func TestLLMRuntimeDraftRoundTripPreservesMaxEffort(t *testing.T) {
	original := config.LLMConfig{
		Provider:  config.LLMProviderOpenAI,
		Auth:      config.LLMAuthSubscription,
		Adapter:   config.LLMAdapterCodexCLI,
		ModelMap:  config.ModelMap{"large": "gpt-5.6-sol"},
		MaxEffort: config.EffortMap{"large": "medium"},
	}

	got := initLLMRuntimeDraftFromConfig(original).exportConfig()

	if len(got.MaxEffort) != 1 || got.MaxEffort["large"] != "medium" {
		t.Fatalf("max_effort after round trip = %#v, want large=medium", got.MaxEffort)
	}
	if len(got.ModelMap) != 1 || got.ModelMap["large"] != "gpt-5.6-sol" {
		t.Fatalf("model_map after round trip = %#v", got.ModelMap)
	}
}

func TestLLMRuntimeIdentityKeyDistinguishesMaxEffort(t *testing.T) {
	base := initLLMRuntimeDraft{
		Provider: config.LLMProviderOpenAI,
		Auth:     config.LLMAuthSubscription,
		Adapter:  config.LLMAdapterCodexCLI,
		ModelMap: config.ModelMap{"large": "gpt-5.6-sol"},
	}
	capped := base
	capped.MaxEffort = config.EffortMap{"large": "medium"}

	if base.identityKey() == capped.identityKey() {
		t.Fatalf("identityKey collides for runtimes differing only by max_effort")
	}
}

func TestCloneInitLLMConfigDeepCopiesMaxEffort(t *testing.T) {
	original := config.LLMConfig{MaxEffort: config.EffortMap{"large": "medium"}}
	cloned := cloneInitLLMConfig(original)
	cloned.MaxEffort["large"] = "high"

	if original.MaxEffort["large"] != "medium" {
		t.Fatalf("clone aliased max_effort: original = %#v", original.MaxEffort)
	}
}
