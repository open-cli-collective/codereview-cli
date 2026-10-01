package config

// CurrentReviewDefaultsVersion marks the one-time Codex model/effort upgrade.
const CurrentReviewDefaultsVersion = 1

// UpgradeReviewDefaults updates a shared Codex runtime once. Subsequent user
// edits, including removing mappings or adding ceilings, remain untouched.
func UpgradeReviewDefaults(cfg File, runtimeName string) (File, bool) {
	runtime, ok := cfg.LLMRuntimes[runtimeName]
	if !ok || runtime.Provider != LLMProviderOpenAI || runtime.Adapter != LLMAdapterCodexCLI ||
		runtime.DefaultsVersion >= CurrentReviewDefaultsVersion {
		return cfg, false
	}
	cfg = cfg.normalized()
	runtime.ModelMap = ModelMap{"small": "gpt-6-luna", "medium": "gpt-6.1-sol", "large": "gpt-6.1-sol"}
	runtime.EffortMap = EffortMap{"small": "max", "medium": "low", "large": "medium"}
	runtime.MaxEffort = nil
	runtime.DefaultsVersion = CurrentReviewDefaultsVersion
	cfg.LLMRuntimes[runtimeName] = runtime
	return cfg, true
}
