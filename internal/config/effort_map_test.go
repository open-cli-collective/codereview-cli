package config

import "testing"

func TestEffortMapValidationAndRuntimeIdentity(t *testing.T) {
	for _, tc := range []struct {
		tier, effort string
		valid        bool
	}{
		{"small", "max", true}, {"medium", "low", true}, {"large", "medium", true},
		{"huge", "low", false}, {"small", "ultra", false}, {"small", "", false},
	} {
		cfg := validFile()
		cfg.LLMRuntimes["home-llm"] = LLMConfig{Provider: LLMProviderOpenAI, Auth: LLMAuthSubscription,
			Adapter: LLMAdapterCodexCLI, EffortMap: EffortMap{tc.tier: tc.effort}}
		if err := Validate(cfg); (err == nil) != tc.valid {
			t.Fatalf("%s/%s: %v, want valid=%v", tc.tier, tc.effort, err, tc.valid)
		}
	}
	base := LLMConfig{Provider: LLMProviderOpenAI, Auth: LLMAuthSubscription, Adapter: LLMAdapterCodexCLI}
	changed := base
	changed.EffortMap = EffortMap{" small ": " max "}
	normalized := changed.normalized()
	if normalized.EffortMap["small"] != "max" || llmRuntimeIdentityKey(base) == llmRuntimeIdentityKey(changed) {
		t.Fatal("effort_map must normalize and distinguish runtime identities")
	}
	normalized.EffortMap["small"] = "low"
	if changed.EffortMap[" small "] != " max " {
		t.Fatal("normalization aliases effort_map")
	}
}
