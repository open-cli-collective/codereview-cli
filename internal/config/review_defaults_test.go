package config

import "testing"

func TestReviewDefaultsUpgradeRunsOncePerCodexRuntime(t *testing.T) {
	cfg := validFile()
	cfg.LLMRuntimes["codex"] = LLMConfig{Provider: LLMProviderOpenAI, Auth: LLMAuthSubscription,
		Adapter: LLMAdapterCodexCLI, ModelMap: ModelMap{"medium": "gpt-6-sol"},
		MaxEffort: EffortMap{"small": "low"}}
	upgraded, changed := UpgradeReviewDefaults(cfg, "codex")
	runtime := upgraded.LLMRuntimes["codex"]
	if !changed || runtime.ModelMap["medium"] != "gpt-6.1-sol" || runtime.EffortMap["small"] != "max" || runtime.MaxEffort != nil {
		t.Fatalf("upgrade: %#v, changed=%v", runtime, changed)
	}
	if cfg.LLMRuntimes["codex"].DefaultsVersion != 0 {
		t.Fatal("upgrade mutated its input before it was saved")
	}
	runtime.ModelMap["medium"] = "custom-model"
	delete(runtime.EffortMap, "small")
	runtime.MaxEffort = EffortMap{"large": "low"}
	upgraded.LLMRuntimes["codex"] = runtime
	again, changed := UpgradeReviewDefaults(upgraded, "codex")
	runtime = again.LLMRuntimes["codex"]
	if changed || runtime.ModelMap["medium"] != "custom-model" || runtime.EffortMap["small"] != "" || runtime.MaxEffort["large"] != "low" {
		t.Fatal("repeat upgrade overwrote later user edits")
	}
	for _, name := range []string{"home-llm", "work-llm", "missing"} {
		if _, changed := UpgradeReviewDefaults(cfg, name); changed {
			t.Fatalf("upgraded non-Codex runtime %s", name)
		}
	}
	cfg.LLMRuntimes["api"] = LLMConfig{Provider: LLMProviderOpenAI, Auth: LLMAuthAPIKey, Adapter: LLMAdapterOpenAIAPI}
	if _, changed := UpgradeReviewDefaults(cfg, "api"); changed {
		t.Fatal("upgraded the API adapter without verifying its model/tool compatibility")
	}
}
