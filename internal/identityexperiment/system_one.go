package identityexperiment

import (
	"context"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

// SystemOneDecider is the narrow client seam used by SystemOneBackend.
type SystemOneDecider interface {
	Decide(context.Context, systemone.Request) (systemone.Receipt, error)
}

// SystemOneDependencies injects the provider client factory used after
// successful preparation and output reservation.
type SystemOneDependencies struct {
	NewClient func(systemone.Options) (SystemOneDecider, error)
}

type systemOneAdapter struct {
	client        SystemOneDecider
	configuration Configuration
}

// SystemOneBackend adapts the shared System One transport to the common
// identity experiment runner. Configuration carries model, endpoint, auth,
// limits, timeout, and optional pricing.
func SystemOneBackend(configuration Configuration, dependencies SystemOneDependencies) Backend {
	newClient := dependencies.NewClient
	if newClient == nil {
		newClient = func(options systemone.Options) (SystemOneDecider, error) {
			return systemone.New(options)
		}
	}
	provider := configuration.Provider
	if provider == "" {
		provider = "system_one"
	}
	return Backend{
		Prepare: func(projection reportidentityinference.Projection) (Plan, error) {
			plan, err := systemone.Prepare(systemOneRequest(configuration, projection), systemOneLimits(configuration))
			if err != nil {
				return Plan{}, err
			}
			return Plan{RequestSHA256: plan.RequestSHA256, StateBytes: plan.StateBytes, RequestBytes: plan.RequestBytes}, nil
		},
		NewClient: func(apiKey string) (Decider, error) {
			authScheme, _ := normalizedSystemOneAuthScheme(configuration.AuthScheme)
			client, err := newClient(systemone.Options{
				Endpoint: configuration.Endpoint, APIKey: apiKey, AuthScheme: authScheme,
				Timeout: time.Duration(configuration.TimeoutMS) * time.Millisecond,
				Limits:  systemOneLimits(configuration),
			})
			if err != nil || client == nil {
				return nil, err
			}
			return systemOneAdapter{client: client, configuration: cloneConfiguration(configuration)}, nil
		},
		ErrorCode: systemone.ErrorCode,
		EstimateCost: func(usage *Usage, configuration *Configuration) *float64 {
			if configuration == nil {
				return nil
			}
			return EstimateCost(usage, *configuration)
		},
		Provider:      provider,
		ContextStatus: configuration.ContextStatus,
	}
}

func (adapter systemOneAdapter) Decide(ctx context.Context, projection reportidentityinference.Projection) (Receipt, error) {
	providerReceipt, err := adapter.client.Decide(ctx, systemOneRequest(adapter.configuration, projection))
	receipt := Receipt{
		RequestedModel: providerReceipt.RequestedModel, ReturnedModel: providerReceipt.ReturnedModel,
		RequestSHA256: providerReceipt.RequestSHA256, ResponseSHA256: providerReceipt.ResponseSHA256,
		RequestID: providerReceipt.RequestID, Attempted: providerReceipt.Attempted,
		HTTPStatus: providerReceipt.HTTPStatus, Duration: providerReceipt.Duration,
	}
	if providerReceipt.Usage != nil {
		receipt.Usage = &Usage{InputTokens: providerReceipt.Usage.InputTokens, OutputTokens: providerReceipt.Usage.OutputTokens}
	}
	if len(providerReceipt.Answers) == 1 {
		if answer, exists := providerReceipt.Answers["identity"]; exists {
			receipt.Answer = &reportidentityinference.ChoiceAnswer{
				Choice: answer.Choice, Confidence: answer.Confidence, Probabilities: cloneFloat64Map(answer.Probabilities),
			}
		}
	}
	return receipt, err
}

func systemOneRequest(configuration Configuration, projection reportidentityinference.Projection) systemone.Request {
	request := systemone.Request{
		Model: configuration.RequestedModel,
		State: append([]byte(nil), projection.State...),
		Questions: map[string]systemone.ChoiceQuestion{
			projection.QuestionName: {
				Instructions: projection.Question.Instructions,
				Criteria:     cloneStringMap(projection.Question.Criteria),
			},
		},
	}
	if configuration.ServiceTier != nil {
		request.ServiceTier = *configuration.ServiceTier
	}
	if configuration.ExpectedModel != "" {
		request.ExpectedModel = configuration.ExpectedModel
	}
	return request
}

func systemOneLimits(configuration Configuration) systemone.Limits {
	return systemone.Limits{MaxStateBytes: configuration.MaxStateBytes, MaxRequestBytes: configuration.MaxRequestBytes}
}

func normalizedSystemOneAuthScheme(value string) (string, bool) {
	switch value {
	case "api-key", "Api-Key":
		return "Api-Key", true
	case "bearer", "Bearer":
		return "Bearer", true
	default:
		return "", false
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneFloat64Map(source map[string]float64) map[string]float64 {
	clone := make(map[string]float64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
