package benchmarkcmd

import (
	"context"
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

// #nosec G101 -- this names an environment variable; it is not secret material.
const defaultClefAPIKeyEnv = "CLOUDFLARE_AUTH_TOKEN"

type inferClefFlags struct {
	identityInferenceFlags
	accountID string
}

type clefDecider interface {
	Decide(context.Context, systemone.Request) (cloudflareclef.Receipt, error)
}

type inferClefDependencies struct {
	lookupAPIKey func(string) (string, bool)
	newClient    func(cloudflareclef.Options) (clefDecider, error)
	now          func() time.Time
	rename       func(string, string) error
}

type clefProjectionDecider struct {
	client clefDecider
	flags  identityInferenceFlags
}

func defaultInferClefDependencies() inferClefDependencies {
	return inferClefDependencies{
		lookupAPIKey: func(name string) (string, bool) { return os.LookupEnv(name) },
		newClient: func(options cloudflareclef.Options) (clefDecider, error) {
			client, err := cloudflareclef.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func inferClefBackend(flags inferClefFlags, dependencies inferClefDependencies) identityInferenceBackend {
	requestFor := func(projection reportidentityinference.Projection) systemone.Request {
		return clefRequestFromProjection(flags.model, effectiveIdentityInferenceExpectedModel(flags.identityInferenceFlags), projection)
	}
	limits := systemone.Limits{MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes}
	return identityInferenceBackend{
		prepare: func(projection reportidentityinference.Projection) (identityInferencePlan, error) {
			plan, err := cloudflareclef.Prepare(requestFor(projection), limits)
			if err != nil {
				return identityInferencePlan{}, err
			}
			return identityInferencePlan{RequestSHA256: plan.RequestSHA256, StateBytes: plan.StateBytes, RequestBytes: plan.RequestBytes}, nil
		},
		newClient: func(apiKey string) (identityInferenceDecider, error) {
			client, err := dependencies.newClient(cloudflareclef.Options{
				AccountID: flags.accountID,
				APIKey:    apiKey,
				Timeout:   flags.timeout,
				Limits:    limits,
			})
			if err != nil || client == nil {
				return nil, err
			}
			return clefProjectionDecider{client: client, flags: flags.identityInferenceFlags}, nil
		},
		errorCode: cloudflareclef.ErrorCode,
		estimateCost: func(usage *identityInferenceUsage, configuration *identityInferenceConfiguration) *float64 {
			if usage == nil || configuration == nil {
				return nil
			}
			return estimateIdentityInferenceCost(usage, *configuration)
		},
		provider:      "cloudflare_clef",
		contextStatus: "unverified",
	}
}

func (decider clefProjectionDecider) Decide(ctx context.Context, projection reportidentityinference.Projection) (identityInferenceReceipt, error) {
	request := clefRequestFromProjection(decider.flags.model, effectiveIdentityInferenceExpectedModel(decider.flags), projection)
	providerReceipt, err := decider.client.Decide(ctx, request)
	receipt := identityInferenceReceipt{
		RequestedModel: providerReceipt.RequestedModel, ReturnedModel: providerReceipt.ReturnedModel,
		RequestSHA256: providerReceipt.RequestSHA256, ResponseSHA256: providerReceipt.ResponseSHA256,
		RequestID: providerReceipt.RequestID, Attempted: providerReceipt.Attempted,
		HTTPStatus: providerReceipt.HTTPStatus, Duration: providerReceipt.Duration,
		ContextStatus: "unverified",
	}
	if len(providerReceipt.Answers) == 1 {
		if answer, ok := providerReceipt.Answers[projection.QuestionName]; ok {
			receipt.Answer = &reportidentityinference.ChoiceAnswer{
				Choice: answer.Choice, Confidence: answer.Confidence,
				Probabilities: cloneClefProbabilities(answer.Probabilities),
			}
		}
	}
	if providerReceipt.Usage != nil {
		receipt.Usage = &identityInferenceUsage{
			InputTokens: providerReceipt.Usage.InputTokens, OutputTokens: providerReceipt.Usage.OutputTokens,
			TotalTokens: cloneClefTotalPointer(providerReceipt.Usage.TotalTokens),
		}
	}
	return receipt, err
}

func cloneClefProbabilities(source map[string]float64) map[string]float64 {
	if source == nil {
		return nil
	}
	clone := make(map[string]float64, len(source))
	for label, probability := range source {
		clone[label] = probability
	}
	return clone
}

func cloneClefTotalPointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func clefRequestFromProjection(model, expectedModel string, projection reportidentityinference.Projection) systemone.Request {
	return systemone.Request{
		Model:         model,
		ExpectedModel: expectedModel,
		State:         projection.State,
		Questions: map[string]systemone.ChoiceQuestion{
			projection.QuestionName: {
				Instructions: projection.Question.Instructions,
				Criteria:     projection.Question.Criteria,
			},
		},
	}
}
