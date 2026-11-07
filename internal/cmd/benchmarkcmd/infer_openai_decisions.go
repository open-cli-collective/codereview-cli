package benchmarkcmd

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
)

const (
	defaultOpenAIDecisionsEndpoint = "https://api.openai.com/v1/decisions"
	defaultOpenAIKeyEnv            = "OPENAI_API_KEY"
)

type inferOpenAIDecisionsFlags struct {
	identityInferenceFlags
}

type openAIDecisionsDecider interface {
	Decide(context.Context, openaidecisions.Request) (openaidecisions.Receipt, error)
}

type inferOpenAIDecisionsDependencies struct {
	lookupAPIKey func(string) (string, bool)
	newClient    func(openaidecisions.Options) (openAIDecisionsDecider, error)
	now          func() time.Time
	rename       func(string, string) error
}

type openAIDecisionsIdentityDecider struct {
	client openAIDecisionsDecider
	flags  identityInferenceFlags
}

func newInferOpenAIDecisionsCommand(opts *root.Options) *cobra.Command {
	return newInferOpenAIDecisionsCommandWithDependencies(opts, defaultInferOpenAIDecisionsDependencies())
}

func newInferOpenAIDecisionsCommandWithDependencies(opts *root.Options, dependencies inferOpenAIDecisionsDependencies) *cobra.Command {
	flags := inferOpenAIDecisionsFlags{identityInferenceFlags: identityInferenceFlags{
		maxCalls: -1, endpoint: defaultOpenAIDecisionsEndpoint, apiKeyEnv: defaultOpenAIKeyEnv,
		authScheme: "bearer", maxStateBytes: 6 * 1024, maxRequestBytes: 8 * 1024,
		timeout: defaultIdentityInferenceTimeout,
	}}
	cmd := &cobra.Command{
		Use:   "infer-openai-decisions <fixture.json>",
		Short: "Run explicit pairwise identity inference with OpenAI Decisions",
		Args:  exitcode.ExactArgs(1, "benchmark infer-openai-decisions requires one fixture path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags.inputPriceSet = cmd.Flags().Changed("input-price-per-million")
			flags.outputPriceSet = cmd.Flags().Changed("output-price-per-million")
			if err := validateIdentityInferenceFlags("benchmark infer-openai-decisions", flags.identityInferenceFlags); err != nil {
				return exitcode.Usage(err)
			}
			return runInferOpenAIDecisions(cmd.Context(), opts, args[0], flags, dependencies)
		},
	}
	addIdentityInferenceFlags(cmd, &flags.identityInferenceFlags, "HTTPS OpenAI Decisions endpoint")
	addIdentityInferenceLimitsFlags(cmd, &flags.identityInferenceFlags)
	_ = cmd.MarkFlagRequired("model")
	return cmd
}

func defaultInferOpenAIDecisionsDependencies() inferOpenAIDecisionsDependencies {
	return inferOpenAIDecisionsDependencies{
		lookupAPIKey: func(name string) (string, bool) { return os.LookupEnv(name) },
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

func runInferOpenAIDecisions(ctx context.Context, opts *root.Options, fixturePath string, flags inferOpenAIDecisionsFlags, dependencies inferOpenAIDecisionsDependencies) error {
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.newClient == nil || dependencies.rename == nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-openai-decisions: command dependencies are unavailable"))
	}
	backend := inferOpenAIDecisionsBackend(flags, dependencies)
	return runIdentityInference(ctx, opts, "benchmark infer-openai-decisions", fixturePath, flags.identityInferenceFlags, identityInferenceDependencies{
		lookupAPIKey: dependencies.lookupAPIKey,
		now:          dependencies.now,
		rename:       dependencies.rename,
	}, backend)
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
