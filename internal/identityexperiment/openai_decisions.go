package identityexperiment

import (
	"context"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
)

// OpenAIDecisionsDecider is the Decisions client seam.
type OpenAIDecisionsDecider interface {
	Decide(context.Context, openaidecisions.Request) (openaidecisions.Receipt, error)
}

// OpenAIDecisionsClientFactory builds a Decisions client for the identity runner.
type OpenAIDecisionsClientFactory func(openaidecisions.Options) (OpenAIDecisionsDecider, error)

type openAIDecisionsIdentityDecider struct {
	client        OpenAIDecisionsDecider
	configuration Configuration
}

// OpenAIDecisionsBackend adapts Decisions to the shared identity runner.
func OpenAIDecisionsBackend(config Configuration, newClient OpenAIDecisionsClientFactory) Backend {
	if newClient == nil {
		newClient = func(o openaidecisions.Options) (OpenAIDecisionsDecider, error) { return openaidecisions.New(o) }
	}
	limits := openaidecisions.Limits{MaxInputBytes: config.MaxStateBytes, MaxRequestBytes: config.MaxRequestBytes}
	return Backend{
		Prepare: func(projection reportidentityinference.Projection) (Plan, error) {
			plan, err := openaidecisions.Prepare(decisionsRequest(config, projection), limits)
			if err != nil {
				return Plan{}, err
			}
			return Plan{RequestSHA256: plan.RequestSHA256, StateBytes: plan.InputBytes, RequestBytes: plan.RequestBytes}, nil
		},
		NewClient: func(apiKey string) (Decider, error) {
			client, err := newClient(openaidecisions.Options{
				Endpoint: config.Endpoint, APIKey: apiKey, Timeout: time.Duration(config.TimeoutMS) * time.Millisecond, Limits: limits,
			})
			if err != nil || client == nil {
				return nil, err
			}
			return openAIDecisionsIdentityDecider{client: client, configuration: cloneConfiguration(config)}, nil
		},
		ErrorCode: openaidecisions.ErrorCode,
		EstimateCost: func(usage *Usage, configuration *Configuration) *float64 {
			if configuration == nil || usage == nil || usage.CachedInputTokens == nil || usage.CacheWriteTokens == nil ||
				*usage.CachedInputTokens != 0 || *usage.CacheWriteTokens != 0 {
				return nil
			}
			return EstimateCost(usage, *configuration)
		},
		Provider: "openai_decisions",
	}
}

func (decider openAIDecisionsIdentityDecider) Decide(ctx context.Context, projection reportidentityinference.Projection) (Receipt, error) {
	providerReceipt, err := decider.client.Decide(ctx, decisionsRequest(decider.configuration, projection))
	receipt := Receipt{
		RequestedModel: providerReceipt.RequestedModel, ReturnedModel: providerReceipt.ReturnedModel,
		RequestSHA256: providerReceipt.RequestSHA256, ResponseSHA256: providerReceipt.ResponseSHA256,
		RequestID: providerReceipt.RequestID, Attempted: providerReceipt.Attempted,
		HTTPStatus: providerReceipt.HTTPStatus, Duration: providerReceipt.Duration,
	}
	if providerReceipt.Answer != nil {
		receipt.Answer = &reportidentityinference.ChoiceAnswer{
			Choice: providerReceipt.Answer.Choice, Confidence: providerReceipt.Answer.Confidence,
			Probabilities: cloneFloat64Map(providerReceipt.Answer.Probabilities),
		}
	}
	if providerReceipt.Usage != nil {
		receipt.Usage = &Usage{
			InputTokens: providerReceipt.Usage.InputTokens, OutputTokens: providerReceipt.Usage.OutputTokens,
			CachedInputTokens: cloneInt64(providerReceipt.Usage.CachedInputTokens),
			CacheWriteTokens:  cloneInt64(providerReceipt.Usage.CacheWriteTokens),
		}
	}
	return receipt, err
}

func decisionsRequest(config Configuration, projection reportidentityinference.Projection) openaidecisions.Request {
	return openaidecisions.Request{
		Model: config.RequestedModel, ExpectedModel: config.ExpectedModel,
		Input: string(projection.State), Name: projection.QuestionName, Instructions: projection.Question.Instructions,
		Choices: []openaidecisions.Choice{
			{Value: "a", Description: projection.Question.Criteria["a"]},
			{Value: "b", Description: projection.Question.Criteria["b"]},
			{Value: "c", Description: projection.Question.Criteria["c"]},
		},
	}
}
