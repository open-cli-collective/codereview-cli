package benchmarkcmd

import (
	"context"
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	defaultSystemOneTimeout  = defaultIdentityInferenceTimeout
	minimumSystemOneTimeout  = minimumIdentityInferenceTimeout
	maximumSystemOneTimeout  = maximumIdentityInferenceTimeout
	maximumSystemOneCalls    = maximumIdentityInferenceCalls
	defaultSystemOneEndpoint = "https://inference.baseten.co/v1/systemone"
	defaultSystemOneKeyEnv   = "BASETEN_API_KEY"
)

type systemOneDecider interface {
	Decide(context.Context, systemone.Request) (systemone.Receipt, error)
}

type inferSystemOneDependencies struct {
	lookupAPIKey func(string) (string, bool)
	newClient    func(systemone.Options) (systemOneDecider, error)
	now          func() time.Time
	rename       func(string, string) error
}

type systemOneIdentityDecider struct {
	client systemOneDecider
	flags  identityInferenceFlags
}

func defaultInferSystemOneDependencies() inferSystemOneDependencies {
	return inferSystemOneDependencies{
		lookupAPIKey: func(name string) (string, bool) { return os.LookupEnv(name) },
		newClient: func(options systemone.Options) (systemOneDecider, error) {
			client, err := systemone.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func normalizedAuthScheme(value string) (string, bool) {
	switch value {
	case "api-key":
		return "Api-Key", true
	case "bearer":
		return "Bearer", true
	default:
		return "", false
	}
}

func inferSystemOneBackend(flags identityInferenceFlags, dependencies inferSystemOneDependencies) identityInferenceBackend {
	requestFor := func(projection reportidentityinference.Projection) systemone.Request {
		request := systemone.Request{
			Model: flags.model, ServiceTier: flags.serviceTier, State: projection.State,
			Questions: map[string]systemone.ChoiceQuestion{
				projection.QuestionName: {
					Instructions: projection.Question.Instructions,
					Criteria:     projection.Question.Criteria,
				},
			},
		}
		if flags.expectedModel != "" {
			request.ExpectedModel = flags.expectedModel
		}
		return request
	}
	return identityInferenceBackend{
		prepare: func(projection reportidentityinference.Projection) (identityInferencePlan, error) {
			plan, err := systemone.Prepare(requestFor(projection), systemone.Limits{MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes})
			if err != nil {
				return identityInferencePlan{}, err
			}
			return identityInferencePlan{RequestSHA256: plan.RequestSHA256, StateBytes: plan.StateBytes, RequestBytes: plan.RequestBytes}, nil
		},
		newClient: func(apiKey string) (identityInferenceDecider, error) {
			authScheme, _ := normalizedAuthScheme(flags.authScheme)
			client, err := dependencies.newClient(systemone.Options{
				Endpoint: flags.endpoint, APIKey: apiKey, AuthScheme: authScheme, Timeout: flags.timeout,
				Limits: systemone.Limits{MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes},
			})
			if err != nil || client == nil {
				return nil, err
			}
			return systemOneIdentityDecider{client: client, flags: flags}, nil
		},
		errorCode: systemone.ErrorCode,
		estimateCost: func(usage *identityInferenceUsage, configuration *identityInferenceConfiguration) *float64 {
			if configuration == nil {
				return nil
			}
			return estimateIdentityInferenceCost(usage, *configuration)
		},
		provider: "system_one",
	}
}

func (decider systemOneIdentityDecider) Decide(ctx context.Context, projection reportidentityinference.Projection) (identityInferenceReceipt, error) {
	request := systemone.Request{
		Model: decider.flags.model, ServiceTier: decider.flags.serviceTier, State: projection.State,
		Questions: map[string]systemone.ChoiceQuestion{
			projection.QuestionName: {
				Instructions: projection.Question.Instructions,
				Criteria:     projection.Question.Criteria,
			},
		},
	}
	if decider.flags.expectedModel != "" {
		request.ExpectedModel = decider.flags.expectedModel
	}
	providerReceipt, err := decider.client.Decide(ctx, request)
	receipt := identityInferenceReceipt{
		RequestedModel: providerReceipt.RequestedModel, ReturnedModel: providerReceipt.ReturnedModel,
		RequestSHA256: providerReceipt.RequestSHA256, ResponseSHA256: providerReceipt.ResponseSHA256,
		RequestID: providerReceipt.RequestID, Attempted: providerReceipt.Attempted,
		HTTPStatus: providerReceipt.HTTPStatus, Duration: providerReceipt.Duration,
	}
	if providerReceipt.Usage != nil {
		receipt.Usage = &identityInferenceUsage{InputTokens: providerReceipt.Usage.InputTokens, OutputTokens: providerReceipt.Usage.OutputTokens}
	}
	if len(providerReceipt.Answers) == 1 {
		if answer, exists := providerReceipt.Answers[projection.QuestionName]; exists {
			receipt.Answer = &reportidentityinference.ChoiceAnswer{
				Choice: answer.Choice, Confidence: answer.Confidence, Probabilities: cloneFloat64Map(answer.Probabilities),
			}
		}
	}
	return receipt, err
}

func cloneFloat64Map(source map[string]float64) map[string]float64 {
	clone := make(map[string]float64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
