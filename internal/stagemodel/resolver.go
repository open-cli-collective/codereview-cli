// Package stagemodel resolves stage-specific LLM model choices from profile
// preferences and explicit runtime overrides.
package stagemodel

import (
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
)

// Stage identifies a durable LLM interaction point in the review system.
type Stage string

// Known LLM stages.
const (
	StageSelection        Stage = "selection"
	StageSynthesis        Stage = "synthesis"
	StageReviewer         Stage = "reviewer"
	StageThreadAnalysis   Stage = "thread_analysis"
	StageApprovalOverride Stage = "approval_override"
)

// Valid reports whether s is a known model-resolution stage.
func (s Stage) Valid() bool {
	switch s {
	case StageSelection, StageSynthesis, StageReviewer, StageThreadAnalysis, StageApprovalOverride:
		return true
	default:
		return false
	}
}

// Request describes one stage model-resolution request.
type Request struct {
	Profile        config.Profile
	Stage          Stage
	Tier           config.ModelTier
	FloorTier      config.ModelTier
	ModelOverride  string
	EffortOverride string
	DefaultEffort  string
}

// Result is the concrete model/effort selected for one LLM stage.
type Result struct {
	Stage    Stage
	Tier     config.ModelTier
	Model    string
	Effort   string
	Source   config.ModelMapSource
	Override bool
}

// ResolveStageModel resolves one concrete model and effort for a review stage.
func ResolveStageModel(req Request) (Result, error) {
	stage := Stage(strings.TrimSpace(string(req.Stage)))
	if !stage.Valid() {
		return Result{}, fmt.Errorf("stagemodel: stage %q is invalid", req.Stage)
	}
	tier := config.ModelTier(strings.TrimSpace(string(req.Tier)))
	effort := strings.TrimSpace(req.EffortOverride)
	if effort == "" {
		effort = strings.TrimSpace(req.DefaultEffort)
	}
	if model := strings.TrimSpace(req.ModelOverride); model != "" {
		return Result{
			Stage:    stage,
			Tier:     tier,
			Model:    model,
			Effort:   effort,
			Override: true,
		}, nil
	}
	if tier != "" && !tier.Valid() {
		return Result{}, fmt.Errorf("stagemodel: stage %s: model_tier %q is invalid; must be one of small, medium, large", stage, tier)
	}
	floorTier := config.ModelTier(strings.TrimSpace(string(req.FloorTier)))
	if floorTier != "" {
		if !floorTier.Valid() {
			return Result{}, fmt.Errorf("stagemodel: stage %s: model_tier floor %q is invalid; must be one of small, medium, large", stage, floorTier)
		}
		if tier == "" {
			tier = floorTier
		}
		tier = maxModelTier(tier, floorTier)
	}

	resolved, ok := config.ResolveModelTier(req.Profile.LLM, tier)
	if !ok {
		llmConfig := req.Profile.LLM
		return Result{}, fmt.Errorf("stagemodel: stage %s: model_tier %q is not mapped for provider %q adapter %q; add llm.model_map.%s to the profile's LLM runtime", stage, tier, llmConfig.Provider, llmConfig.Adapter, tier)
	}
	return Result{
		Stage:  stage,
		Tier:   resolved.Tier,
		Model:  resolved.Model,
		Effort: applyMaxEffort(req.Profile.LLM, resolved.Tier, effort),
		Source: resolved.Source,
	}, nil
}

// applyMaxEffort clamps effort to the tier's configured ceiling. Tiers without a
// ceiling, and efforts this CLI does not recognize, pass through unchanged.
func applyMaxEffort(llm config.LLMConfig, tier config.ModelTier, effort string) string {
	ceiling, ok := config.ResolveMaxEffort(llm, tier)
	if !ok {
		return effort
	}
	requested := modelprefs.Effort(strings.TrimSpace(effort))
	if !requested.Valid() {
		return effort
	}
	return string(modelprefs.MinEffort(requested, ceiling))
}

// ResolveFirstAvailable resolves the first mapped tier from tiers for req.
func ResolveFirstAvailable(req Request, tiers ...config.ModelTier) (Result, bool) {
	if len(tiers) == 0 {
		resolved, err := ResolveStageModel(req)
		return resolved, err == nil
	}
	for _, tier := range tiers {
		req.Tier = tier
		resolved, err := ResolveStageModel(req)
		if err == nil {
			return resolved, true
		}
	}
	return Result{}, false
}

func maxModelTier(left, right config.ModelTier) config.ModelTier {
	if modelTierRank(left) >= modelTierRank(right) {
		return left
	}
	return right
}

func modelTierRank(tier config.ModelTier) int {
	switch tier {
	case config.ModelTierSmall:
		return 1
	case config.ModelTierMedium:
		return 2
	case config.ModelTierLarge:
		return 3
	default:
		return 0
	}
}
