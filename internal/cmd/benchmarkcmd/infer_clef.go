package benchmarkcmd

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

// #nosec G101 -- this names an environment variable; it is not secret material.
const defaultClefAPIKeyEnv = "CLOUDFLARE_AUTH_TOKEN"

type inferClefFlags struct {
	identityInferenceFlags
	accountID              string
	allowUnverifiedContext bool
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

func newInferClefCommand(opts *root.Options) *cobra.Command {
	return newInferClefCommandWithDependencies(opts, defaultInferClefDependencies())
}

func newInferClefCommandWithDependencies(opts *root.Options, dependencies inferClefDependencies) *cobra.Command {
	flags := inferClefFlags{identityInferenceFlags: identityInferenceFlags{
		maxCalls: -1, apiKeyEnv: defaultClefAPIKeyEnv, authScheme: "bearer",
		maxStateBytes: 6 * 1024, maxRequestBytes: 8 * 1024,
		timeout: defaultIdentityInferenceTimeout,
	}}
	cmd := &cobra.Command{
		Use:   "infer-clef <fixture.json>",
		Short: "Run explicit pairwise identity inference with Cloudflare Clef",
		Args:  exitcode.ExactArgs(1, "benchmark infer-clef requires one fixture path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags.inputPriceSet = cmd.Flags().Changed("input-price-per-million")
			flags.outputPriceSet = cmd.Flags().Changed("output-price-per-million")
			if err := validateInferClefFlags(&flags); err != nil {
				return exitcode.Usage(err)
			}
			return runInferClef(cmd.Context(), opts, args[0], flags, dependencies)
		},
	}
	cmd.Flags().StringVar(&flags.model, "model", "", "Required model selector: clef or clef-flash")
	cmd.Flags().StringVar(&flags.expectedModel, "expected-model", "", "Expected response model ID; defaults to --model")
	cmd.Flags().StringVar(&flags.accountID, "account-id", "", "Required Cloudflare account ID")
	cmd.Flags().BoolVar(&flags.allowUnverifiedContext, "allow-unverified-context", false, "Required acknowledgement: Cloudflare may silently truncate state without attesting that it consumed the full input")
	cmd.Flags().IntVar(&flags.maxCalls, "max-calls", flags.maxCalls, "Maximum number of pairwise provider calls (required; 0-10000)")
	cmd.Flags().StringVar(&flags.outputDir, "output-dir", "", "New private output directory for the fixture, replay, receipts, and manifest (required)")
	cmd.Flags().StringVar(&flags.apiKeyEnv, "api-key-env", flags.apiKeyEnv, "Environment variable name containing the Cloudflare API token")
	cmd.Flags().IntVar(&flags.maxStateBytes, "max-state-bytes", flags.maxStateBytes, "Maximum compact state bytes per request")
	cmd.Flags().IntVar(&flags.maxRequestBytes, "max-request-bytes", flags.maxRequestBytes, "Maximum complete request bytes")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", flags.timeout, "Per-request timeout from 1s through 120s")
	cmd.Flags().Float64Var(&flags.inputPricePerMillion, "input-price-per-million", 0, "Optional estimated USD per million input tokens")
	cmd.Flags().Float64Var(&flags.outputPricePerMillion, "output-price-per-million", 0, "Optional estimated USD per million output tokens")
	return cmd
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

func validateInferClefFlags(flags *inferClefFlags) error {
	endpoint, err := cloudflareclef.Endpoint(flags.accountID, flags.model)
	if err != nil {
		return errors.New("benchmark infer-clef: --account-id must be an ASCII Cloudflare account ID and --model must be clef or clef-flash")
	}
	if !flags.allowUnverifiedContext {
		return errors.New("benchmark infer-clef: --allow-unverified-context is required because Clef can silently truncate state")
	}
	flags.endpoint = endpoint
	return validateIdentityInferenceFlags("benchmark infer-clef", flags.identityInferenceFlags)
}

func runInferClef(ctx context.Context, opts *root.Options, fixturePath string, flags inferClefFlags, dependencies inferClefDependencies) error {
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.newClient == nil || dependencies.rename == nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-clef: command dependencies are unavailable"))
	}
	backend := inferClefBackend(flags, dependencies)
	return runIdentityInference(ctx, opts, "benchmark infer-clef", fixturePath, flags.identityInferenceFlags, identityInferenceDependencies{
		lookupAPIKey: dependencies.lookupAPIKey,
		now:          dependencies.now,
		rename:       dependencies.rename,
	}, backend)
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
