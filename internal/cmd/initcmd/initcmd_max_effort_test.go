package initcmd

import (
	"os"
	"path/filepath"
	"strings"
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

func TestInitNonInteractivePreservesMaxEffortThroughConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	existing := basicProfile("work")
	existing.LLM.ModelMap = config.ModelMap{"large": "gpt-5.6-sol"}
	existing.LLM.MaxEffort = config.EffortMap{"large": "medium"}
	if err := config.Save(path, config.File{Profiles: map[string]config.Profile{"work": existing}}); err != nil {
		t.Fatalf("Save initial config: %v", err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test path is controlled by t.TempDir.
	if err != nil {
		t.Fatalf("Read initial config: %v", err)
	}
	if !strings.Contains(string(data), "max_effort:") {
		t.Fatalf("initial config = %q, want max_effort YAML", data)
	}

	flags := defaultNonInteractiveInitOptionsForTest()
	flags.replaceProfile = true
	_, _, err = runNonInteractiveInitWithFakeStore(t, path, "work", strings.NewReader(""), flags, newFakeInitStore(nil))
	if err != nil {
		t.Fatalf("non-interactive init: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if got := loaded.Profiles["work"].LLM.MaxEffort["large"]; got != "medium" {
		t.Fatalf("saved max_effort.large = %q, want medium", got)
	}
}
