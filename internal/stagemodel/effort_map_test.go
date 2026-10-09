package stagemodel

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestIndependentModelAndEffortMaps(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI,
		ModelMap:  config.ModelMap{"small": "gpt-6-luna", "medium": "gpt-6.1-sol", "large": "gpt-6.1-sol"},
		EffortMap: config.EffortMap{"small": "max", "medium": "low", "large": "medium"},
		MaxEffort: config.EffortMap{"large": "medium"},
	}}
	for _, tc := range []struct {
		name                                                 string
		stage                                                Stage
		tier, floor                                          config.ModelTier
		modelOverride, effortOverride, wantModel, wantEffort string
	}{
		{"small reviewer", StageReviewer, config.ModelTierSmall, "", "", "", "gpt-6-luna", "max"},
		{"medium reviewer", StageReviewer, config.ModelTierMedium, "", "", "", "gpt-6.1-sol", "low"},
		{"large reviewer", StageReviewer, config.ModelTierLarge, "", "", "", "gpt-6.1-sol", "medium"},
		{"selection", StageSelection, config.ModelTierMedium, "", "", "", "gpt-6.1-sol", "low"},
		{"synthesis", StageSynthesis, config.ModelTierMedium, "", "", "", "gpt-6.1-sol", "low"},
		{"thread analysis", StageThreadAnalysis, config.ModelTierMedium, "", "", "", "gpt-6.1-sol", "low"},
		{"approval classifier", StageApprovalOverride, config.ModelTierSmall, "", "", "", "gpt-6-luna", "max"},
		{"post-floor effort", StageReviewer, config.ModelTierSmall, config.ModelTierLarge, "", "", "gpt-6.1-sol", "medium"},
		{"independent model override", StageReviewer, config.ModelTierSmall, "", "custom-model", "", "custom-model", "max"},
		{"effort override wins", StageReviewer, config.ModelTierLarge, "", "", "max", "gpt-6.1-sol", "max"},
		{"both overrides", StageSelection, config.ModelTierMedium, "", "custom-model", "max", "custom-model", "max"},
		{"exact model without tier", StageReviewer, "", "", "custom-model", "", "custom-model", "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveStageModel(Request{Profile: profile, Stage: tc.stage, Tier: tc.tier, FloorTier: tc.floor,
				ModelOverride: tc.modelOverride, EffortOverride: tc.effortOverride, DefaultEffort: "low"})
			if err != nil || got.Model != tc.wantModel || got.Effort != tc.wantEffort {
				t.Fatalf("got %#v, %v; want %s / %s", got, err, tc.wantModel, tc.wantEffort)
			}
		})
	}
	profile.LLM.MaxEffort["small"] = "medium"
	got, err := ResolveStageModel(Request{Profile: profile, Stage: StageReviewer, Tier: config.ModelTierSmall, DefaultEffort: "low"})
	if err != nil || got.Effort != "medium" {
		t.Fatalf("ceiling must cap mapped max effort: %#v, %v", got, err)
	}
}

func TestEffortPreferenceOverridesBuiltInPreset(t *testing.T) {
	profile := config.Profile{LLM: config.LLMConfig{
		Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI,
		EffortMap: config.EffortMap{"small": "high"},
	}}
	got, err := ResolveStageModel(Request{Profile: profile, Stage: StageReviewer, Tier: config.ModelTierSmall, DefaultEffort: "low"})
	if err != nil || got.Model != "gpt-6-luna" || got.Effort != "high" || got.Source != config.ModelMapSourceBuiltIn {
		t.Fatalf("built-in model with configured effort: %#v, %v", got, err)
	}
}
