package benchmarkcmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
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

type inferSystemOneFlags struct {
	identityInferenceFlags
}

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

func newInferSystemOneCommand(opts *root.Options) *cobra.Command {
	return newInferSystemOneCommandWithDependencies(opts, defaultInferSystemOneDependencies())
}

func newInferSystemOneCommandWithDependencies(opts *root.Options, dependencies inferSystemOneDependencies) *cobra.Command {
	flags := inferSystemOneFlags{identityInferenceFlags: identityInferenceFlags{
		maxCalls: -1, endpoint: defaultSystemOneEndpoint, authScheme: "api-key", apiKeyEnv: defaultSystemOneKeyEnv,
		maxStateBytes: 6 * 1024, maxRequestBytes: 8 * 1024, timeout: defaultSystemOneTimeout,
	}}
	cmd := &cobra.Command{
		Use:   "infer-system-one <fixture.json>",
		Short: "Run explicit pairwise identity inference over a local fixture",
		Args:  exitcode.ExactArgs(1, "benchmark infer-system-one requires one fixture path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags.inputPriceSet = cmd.Flags().Changed("input-price-per-million")
			flags.outputPriceSet = cmd.Flags().Changed("output-price-per-million")
			if err := validateInferSystemOneFlags(flags); err != nil {
				return exitcode.Usage(err)
			}
			return runInferSystemOne(cmd.Context(), opts, args[0], flags, dependencies)
		},
	}
	addIdentityInferenceFlags(cmd, &flags.identityInferenceFlags, "HTTPS System One endpoint")
	cmd.Flags().StringVar(&flags.authScheme, "auth-scheme", flags.authScheme, "Authorization scheme: api-key or bearer")
	cmd.Flags().StringVar(&flags.serviceTier, "service-tier", "", "Optional provider service tier")
	addIdentityInferenceLimitsFlags(cmd, &flags.identityInferenceFlags)
	_ = cmd.MarkFlagRequired("model")
	return cmd
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

func validateInferSystemOneFlags(flags inferSystemOneFlags) error {
	commandName := "benchmark infer-system-one"
	if err := validateIdentityInferenceCoreFlags(commandName, flags.identityInferenceFlags); err != nil {
		return err
	}
	if _, ok := normalizedAuthScheme(flags.authScheme); !ok {
		return errors.New(commandName + ": --auth-scheme must be api-key or bearer")
	}
	if flags.serviceTier != "" && strings.TrimSpace(flags.serviceTier) == "" {
		return errors.New(commandName + ": --service-tier must not be whitespace-only")
	}
	return validateIdentityInferenceLimitsAndPrices(commandName, flags.identityInferenceFlags)
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

func runInferSystemOne(ctx context.Context, opts *root.Options, fixturePath string, flags inferSystemOneFlags, dependencies inferSystemOneDependencies) error {
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.newClient == nil || dependencies.rename == nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: command dependencies are unavailable"))
	}
	backend := inferSystemOneBackend(flags, dependencies)
	return runIdentityInference(ctx, opts, "benchmark infer-system-one", fixturePath, flags.identityInferenceFlags, identityInferenceDependencies{
		lookupAPIKey: dependencies.lookupAPIKey,
		now:          dependencies.now,
		rename:       dependencies.rename,
	}, backend)
}

func inferSystemOneBackend(flags inferSystemOneFlags, dependencies inferSystemOneDependencies) identityInferenceBackend {
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
			return systemOneIdentityDecider{client: client, flags: flags.identityInferenceFlags}, nil
		},
		errorCode: systemone.ErrorCode,
		estimateCost: func(usage *identityInferenceUsage, configuration *identityInferenceConfiguration) *float64 {
			if configuration == nil {
				return nil
			}
			return estimateIdentityInferenceCost(usage, *configuration)
		},
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
