package benchmarkcmd

import (
	"context"
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
)

type inferOpenAIDecisionsFlags struct {
	identityInferenceFlags
}

type openAIDecisionsDecider interface {
	Decide(context.Context, openaidecisions.Request) (openaidecisions.Receipt, error)
}

type inferOpenAIDecisionsDependencies struct {
	resolveAPIKey func(context.Context) (string, error)
	newClient     func(openaidecisions.Options) (openAIDecisionsDecider, error)
	now           func() time.Time
	rename        func(string, string) error
}

type openAIDecisionsIdentityDecider struct {
	client openAIDecisionsDecider
	flags  identityInferenceFlags
}

func defaultInferOpenAIDecisionsDependencies() inferOpenAIDecisionsDependencies {
	return inferOpenAIDecisionsDependencies{
		newClient: func(options openaidecisions.Options) (openAIDecisionsDecider, error) {
			client, err := openaidecisions.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func inferOpenAIDecisionsBackend(flags inferOpenAIDecisionsFlags, dependencies inferOpenAIDecisionsDependencies) identityInferenceBackend {
	requestFor := func(projection reportidentityinference.Projection) openaidecisions.Request {
		return openaidecisions.Request{
			Model: flags.model, ExpectedModel: effectiveIdentityInferenceExpectedModel(flags.identityInferenceFlags),
			Input: string(projection.State), Name: projection.QuestionName, Instructions: projection.Question.Instructions,
			Choices: []openaidecisions.Choice{
				{Value: "a", Description: projection.Question.Criteria["a"]},
				{Value: "b", Description: projection.Question.Criteria["b"]},
				{Value: "c", Description: projection.Question.Criteria["c"]},
			},
		}
	}
	limits := openaidecisions.Limits{MaxInputBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes}
	return identityInferenceBackend{
		prepare: func(projection reportidentityinference.Projection) (identityInferencePlan, error) {
			plan, err := openaidecisions.Prepare(requestFor(projection), limits)
			if err != nil {
				return identityInferencePlan{}, err
			}
			return identityInferencePlan{RequestSHA256: plan.RequestSHA256, StateBytes: plan.InputBytes, RequestBytes: plan.RequestBytes}, nil
		},
		newClient: func(apiKey string) (identityInferenceDecider, error) {
			client, err := dependencies.newClient(openaidecisions.Options{
				Endpoint: flags.endpoint, APIKey: apiKey, Timeout: flags.timeout, Limits: limits,
			})
			if err != nil || client == nil {
				return nil, err
			}
			return openAIDecisionsIdentityDecider{client: client, flags: flags.identityInferenceFlags}, nil
		},
		errorCode: openaidecisions.ErrorCode,
		estimateCost: func(usage *identityInferenceUsage, configuration *identityInferenceConfiguration) *float64 {
			if usage == nil || configuration == nil || usage.CachedInputTokens == nil || usage.CacheWriteTokens == nil ||
				*usage.CachedInputTokens != 0 || *usage.CacheWriteTokens != 0 {
				return nil
			}
			return estimateIdentityInferenceCost(usage, *configuration)
		},
		provider: "openai_decisions",
	}
}

func (decider openAIDecisionsIdentityDecider) Decide(ctx context.Context, projection reportidentityinference.Projection) (identityInferenceReceipt, error) {
	request := openaidecisions.Request{
		Model: decider.flags.model, ExpectedModel: effectiveIdentityInferenceExpectedModel(decider.flags),
		Input: string(projection.State), Name: projection.QuestionName, Instructions: projection.Question.Instructions,
		Choices: []openaidecisions.Choice{
			{Value: "a", Description: projection.Question.Criteria["a"]},
			{Value: "b", Description: projection.Question.Criteria["b"]},
			{Value: "c", Description: projection.Question.Criteria["c"]},
		},
	}
	providerReceipt, err := decider.client.Decide(ctx, request)
	receipt := identityInferenceReceipt{
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
		receipt.Usage = &identityInferenceUsage{
			InputTokens: providerReceipt.Usage.InputTokens, OutputTokens: providerReceipt.Usage.OutputTokens,
			CachedInputTokens: cloneInt64Pointer(providerReceipt.Usage.CachedInputTokens),
			CacheWriteTokens:  cloneInt64Pointer(providerReceipt.Usage.CacheWriteTokens),
		}
	}
	return receipt, err
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
