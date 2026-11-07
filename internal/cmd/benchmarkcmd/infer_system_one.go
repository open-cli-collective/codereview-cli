package benchmarkcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	inferSystemOneSchemaVersion = 1
	defaultSystemOneTimeout     = 30 * time.Second
	minimumSystemOneTimeout     = time.Second
	maximumSystemOneTimeout     = 120 * time.Second
	maximumSystemOneCalls       = 10_000
	defaultSystemOneEndpoint    = "https://inference.baseten.co/v1/systemone"
	defaultSystemOneKeyEnv      = "BASETEN_API_KEY"
)

type inferSystemOneFlags struct {
	model                 string
	expectedModel         string
	endpoint              string
	authScheme            string
	apiKeyEnv             string
	serviceTier           string
	maxStateBytes         int
	maxRequestBytes       int
	maxCalls              int
	outputDir             string
	timeout               time.Duration
	inputPricePerMillion  float64
	outputPricePerMillion float64
	inputPriceSet         bool
	outputPriceSet        bool
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

type inferSystemOnePair struct {
	pair    reportidentity.Pair
	request systemone.Request
	plan    systemone.Plan
}

type inferSystemOneConfiguration struct {
	Endpoint              string   `json:"endpoint"`
	RequestedModel        string   `json:"requested_model"`
	ExpectedModel         string   `json:"expected_model"`
	ServiceTier           *string  `json:"service_tier"`
	AuthScheme            string   `json:"auth_scheme"`
	APIKeyEnv             string   `json:"api_key_env"`
	MaxStateBytes         int      `json:"max_state_bytes"`
	MaxRequestBytes       int      `json:"max_request_bytes"`
	InputPricePerMillion  *float64 `json:"input_price_per_million"`
	OutputPricePerMillion *float64 `json:"output_price_per_million"`
}

type inferSystemOnePreflightError struct {
	LeftID      string `json:"left_id,omitempty"`
	RightID     string `json:"right_id,omitempty"`
	PairSHA256  string `json:"pair_sha256,omitempty"`
	ErrorCode   string `json:"error_code"`
	RequestSize int    `json:"request_bytes,omitempty"`
	StateSize   int    `json:"state_bytes,omitempty"`
}

type inferSystemOneUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type inferSystemOneAnswer struct {
	Choice        reportidentity.Choice `json:"choice"`
	Probabilities [3]float64            `json:"probabilities"`
	Confidence    float64               `json:"confidence"`
}

type inferSystemOneAttempt struct {
	LeftID               string                `json:"left_id"`
	RightID              string                `json:"right_id"`
	PairSHA256           string                `json:"pair_sha256"`
	PlannedRequestSHA256 string                `json:"planned_request_sha256"`
	RequestSHA256        string                `json:"request_sha256,omitempty"`
	ResponseSHA256       string                `json:"response_sha256,omitempty"`
	RequestID            string                `json:"request_id,omitempty"`
	RequestedModel       string                `json:"requested_model,omitempty"`
	ReturnedModel        string                `json:"returned_model,omitempty"`
	Answer               *inferSystemOneAnswer `json:"answer,omitempty"`
	Usage                *inferSystemOneUsage  `json:"usage,omitempty"`
	StartedUTC           string                `json:"started_utc"`
	DurationMS           int64                 `json:"duration_ms"`
	HTTPAttempted        bool                  `json:"http_attempted"`
	HTTPStatus           int                   `json:"http_status,omitempty"`
	EstimatedCostUSD     *float64              `json:"estimated_cost_usd"`
	ErrorCode            string                `json:"error_code,omitempty"`
	StateBytes           int                   `json:"state_bytes"`
	RequestBytes         int                   `json:"request_bytes"`
}

type inferSystemOneReceipts struct {
	SchemaVersion     int                            `json:"schema_version"`
	ProtocolVersion   int                            `json:"protocol_version"`
	Task              reportidentity.Task            `json:"task"`
	InputSHA256       string                         `json:"input_sha256"`
	StartedUTC        string                         `json:"started_utc"`
	Configuration     inferSystemOneConfiguration    `json:"configuration"`
	FindingCount      int                            `json:"finding_count"`
	ThreadCount       int                            `json:"thread_count"`
	ExpectedPairs     int                            `json:"expected_pairs"`
	AttemptedPairs    int                            `json:"attempted_pairs"`
	ProviderCalls     int                            `json:"provider_calls"`
	CompletedPairs    int                            `json:"completed_pairs"`
	AbstainedPairs    int                            `json:"abstained_pairs"`
	FailedPairs       int                            `json:"failed_pairs"`
	MissingPairs      int                            `json:"missing_pairs"`
	KnownInputTokens  int64                          `json:"known_input_tokens"`
	KnownOutputTokens int64                          `json:"known_output_tokens"`
	EstimatedCostUSD  *float64                       `json:"estimated_cost_usd"`
	UnknownCostCalls  int                            `json:"unknown_cost_calls"`
	Status            string                         `json:"status"`
	RunErrorCode      string                         `json:"run_error_code,omitempty"`
	PreflightErrors   []inferSystemOnePreflightError `json:"preflight_errors"`
	Attempts          []inferSystemOneAttempt        `json:"attempts"`
	knownCostUSD      float64                        `json:"-"`
	knownCostCount    int                            `json:"-"`
}

type inferSystemOneManifestFiles struct {
	FixtureSHA256  string `json:"fixture.json"`
	ReplaySHA256   string `json:"replay.json"`
	ReceiptsSHA256 string `json:"receipts.json"`
}

type inferSystemOneManifest struct {
	SchemaVersion int                         `json:"schema_version"`
	Files         inferSystemOneManifestFiles `json:"files"`
}

type inferSystemOneSummary struct {
	SchemaVersion     int                         `json:"schema_version"`
	Status            string                      `json:"status"`
	ExpectedPairs     int                         `json:"expected_pairs"`
	AttemptedPairs    int                         `json:"attempted_pairs"`
	ProviderCalls     int                         `json:"provider_calls"`
	CompletedPairs    int                         `json:"completed_pairs"`
	AbstainedPairs    int                         `json:"abstained_pairs"`
	FailedPairs       int                         `json:"failed_pairs"`
	MissingPairs      int                         `json:"missing_pairs"`
	OutputDir         string                      `json:"output_dir"`
	FixturePath       string                      `json:"fixture_path"`
	ReplayPath        string                      `json:"replay_path"`
	ReceiptsPath      string                      `json:"receipts_path"`
	ManifestPath      string                      `json:"manifest_path"`
	KnownInputTokens  int64                       `json:"known_input_tokens"`
	KnownOutputTokens int64                       `json:"known_output_tokens"`
	EstimatedCostUSD  *float64                    `json:"estimated_cost_usd"`
	UnknownCostCalls  int                         `json:"unknown_cost_calls"`
	Configuration     inferSystemOneConfiguration `json:"configuration"`
}

func newInferSystemOneCommand(opts *root.Options) *cobra.Command {
	return newInferSystemOneCommandWithDependencies(opts, defaultInferSystemOneDependencies())
}

func newInferSystemOneCommandWithDependencies(opts *root.Options, dependencies inferSystemOneDependencies) *cobra.Command {
	flags := inferSystemOneFlags{
		maxCalls:        -1,
		outputDir:       "",
		endpoint:        defaultSystemOneEndpoint,
		authScheme:      "api-key",
		apiKeyEnv:       defaultSystemOneKeyEnv,
		maxStateBytes:   6 * 1024,
		maxRequestBytes: 8 * 1024,
		timeout:         defaultSystemOneTimeout,
	}
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
	cmd.Flags().StringVar(&flags.model, "model", "", "Required model alias or ID to request")
	cmd.Flags().StringVar(&flags.expectedModel, "expected-model", "", "Expected response model ID; defaults to --model")
	cmd.Flags().IntVar(&flags.maxCalls, "max-calls", flags.maxCalls, "Maximum number of pairwise provider calls (required; 0-10000)")
	cmd.Flags().StringVar(&flags.outputDir, "output-dir", "", "New private output directory for the fixture, replay, receipts, and manifest (required)")
	cmd.Flags().StringVar(&flags.endpoint, "endpoint", flags.endpoint, "HTTPS System One endpoint")
	cmd.Flags().StringVar(&flags.apiKeyEnv, "api-key-env", flags.apiKeyEnv, "Environment variable name containing the API key")
	cmd.Flags().StringVar(&flags.authScheme, "auth-scheme", flags.authScheme, "Authorization scheme: api-key or bearer")
	cmd.Flags().StringVar(&flags.serviceTier, "service-tier", "", "Optional provider service tier")
	cmd.Flags().IntVar(&flags.maxStateBytes, "max-state-bytes", flags.maxStateBytes, "Maximum compact state bytes per request")
	cmd.Flags().IntVar(&flags.maxRequestBytes, "max-request-bytes", flags.maxRequestBytes, "Maximum complete request bytes")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", flags.timeout, "Per-request timeout from 1s through 120s")
	cmd.Flags().Float64Var(&flags.inputPricePerMillion, "input-price-per-million", 0, "Optional estimated USD per million input tokens")
	cmd.Flags().Float64Var(&flags.outputPricePerMillion, "output-price-per-million", 0, "Optional estimated USD per million output tokens")
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
	if strings.TrimSpace(flags.model) == "" {
		return errors.New("benchmark infer-system-one: --model is required")
	}
	if flags.expectedModel != "" && strings.TrimSpace(flags.expectedModel) == "" {
		return errors.New("benchmark infer-system-one: --expected-model must be nonempty when specified")
	}
	if flags.maxCalls < 0 || flags.maxCalls > maximumSystemOneCalls {
		return fmt.Errorf("benchmark infer-system-one: --max-calls must be in the range 0-%d", maximumSystemOneCalls)
	}
	if strings.TrimSpace(flags.outputDir) == "" {
		return errors.New("benchmark infer-system-one: --output-dir is required")
	}
	if flags.timeout < minimumSystemOneTimeout || flags.timeout > maximumSystemOneTimeout {
		return errors.New("benchmark infer-system-one: --timeout must be from 1s through 120s")
	}
	if !validSystemOneEndpoint(flags.endpoint) {
		return errors.New("benchmark infer-system-one: --endpoint must be a valid HTTPS URL without query, userinfo, or fragment")
	}
	if !validAPIKeyEnvName(flags.apiKeyEnv) {
		return errors.New("benchmark infer-system-one: --api-key-env must be an environment variable name")
	}
	if _, ok := normalizedAuthScheme(flags.authScheme); !ok {
		return errors.New("benchmark infer-system-one: --auth-scheme must be api-key or bearer")
	}
	if flags.serviceTier != "" && strings.TrimSpace(flags.serviceTier) == "" {
		return errors.New("benchmark infer-system-one: --service-tier must not be whitespace-only")
	}
	if flags.maxStateBytes <= 0 || flags.maxRequestBytes <= 0 {
		return errors.New("benchmark infer-system-one: byte limits must be positive")
	}
	if flags.inputPriceSet != flags.outputPriceSet {
		return errors.New("benchmark infer-system-one: both price flags must be supplied together")
	}
	if flags.inputPriceSet && (!finiteNonnegative(flags.inputPricePerMillion) || !finiteNonnegative(flags.outputPricePerMillion)) {
		return errors.New("benchmark infer-system-one: prices must be finite and nonnegative")
	}
	return nil
}

func validSystemOneEndpoint(value string) bool {
	if value == "" || strings.Contains(value, "#") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" && parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == ""
}

func validAPIKeyEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range []byte(value) {
		letter := character == '_' || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		digit := character >= '0' && character <= '9'
		if (index == 0 && !letter) || (index > 0 && !letter && !digit) {
			return false
		}
	}
	return true
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

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func configurationFor(flags inferSystemOneFlags) inferSystemOneConfiguration {
	authScheme, _ := normalizedAuthScheme(flags.authScheme)
	var tier *string
	if flags.serviceTier != "" {
		value := flags.serviceTier
		tier = &value
	}
	configuration := inferSystemOneConfiguration{
		Endpoint:        flags.endpoint,
		RequestedModel:  flags.model,
		ExpectedModel:   effectiveExpectedModel(flags),
		ServiceTier:     tier,
		AuthScheme:      authScheme,
		APIKeyEnv:       flags.apiKeyEnv,
		MaxStateBytes:   flags.maxStateBytes,
		MaxRequestBytes: flags.maxRequestBytes,
	}
	if flags.inputPriceSet {
		inputPrice := flags.inputPricePerMillion
		outputPrice := flags.outputPricePerMillion
		configuration.InputPricePerMillion = &inputPrice
		configuration.OutputPricePerMillion = &outputPrice
	}
	return configuration
}

func effectiveExpectedModel(flags inferSystemOneFlags) string {
	if flags.expectedModel != "" {
		return flags.expectedModel
	}
	return flags.model
}

func runInferSystemOne(ctx context.Context, opts *root.Options, fixturePath string, flags inferSystemOneFlags, dependencies inferSystemOneDependencies) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.newClient == nil || dependencies.rename == nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: command dependencies are unavailable"))
	}
	inputBytes, err := readReplayFixture(fixturePath)
	if err != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark infer-system-one: read fixture: %w", err))
	}
	input, err := reportidentity.Decode(inputBytes)
	if err != nil {
		return exitcode.Usage(errors.New("benchmark infer-system-one: invalid fixture"))
	}
	if len(input.Decisions) != 0 {
		return exitcode.Usage(errors.New("benchmark infer-system-one: fixture decisions must be empty"))
	}
	baseResult, err := reportidentity.Replay(input)
	if err != nil {
		return exitcode.Usage(errors.New("benchmark infer-system-one: invalid fixture"))
	}
	startedUTC := dependencies.now().UTC()
	inputDigest := sha256Hex(inputBytes)
	configuration := configurationFor(flags)
	pairs, preflightErrors := prepareInferSystemOnePairs(input, baseResult, flags)
	if len(baseResult.Pairs) > flags.maxCalls {
		preflightErrors = append(preflightErrors, inferSystemOnePreflightError{ErrorCode: "invalid_input"})
	}
	if err := checkInferSystemOneOutputDirNew(flags.outputDir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New("benchmark infer-system-one: output directory must be new"))
		}
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: could not inspect output directory"))
	}

	var client systemOneDecider
	if len(preflightErrors) == 0 && len(pairs) > 0 {
		apiKey, ok := dependencies.lookupAPIKey(flags.apiKeyEnv)
		if !ok || apiKey == "" {
			preflightErrors = append(preflightErrors, inferSystemOnePreflightError{ErrorCode: "auth"})
		} else {
			authScheme, _ := normalizedAuthScheme(flags.authScheme)
			client, err = dependencies.newClient(systemone.Options{
				Endpoint: flags.endpoint, APIKey: apiKey, AuthScheme: authScheme, Timeout: flags.timeout,
				Limits: systemone.Limits{MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes},
			})
			if err != nil {
				preflightErrors = append(preflightErrors, inferSystemOnePreflightError{ErrorCode: systemone.ErrorCode(err)})
			} else if client == nil {
				preflightErrors = append(preflightErrors, inferSystemOnePreflightError{ErrorCode: "invalid_input"})
			}
		}
	}

	outputDir, err := createInferSystemOneOutputDir(flags.outputDir)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New("benchmark infer-system-one: output directory must be new"))
		}
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: could not create output directory"))
	}

	decisions := make([]reportidentity.Decision, 0, len(pairs))
	receipts := inferSystemOneReceipts{
		SchemaVersion:   inferSystemOneSchemaVersion,
		ProtocolVersion: 1,
		Task:            input.Task,
		InputSHA256:     inputDigest,
		StartedUTC:      startedUTC.Format(time.RFC3339Nano),
		Configuration:   configuration,
		FindingCount:    len(input.Findings),
		ThreadCount:     len(input.Threads),
		ExpectedPairs:   len(baseResult.Pairs),
		Status:          "complete",
		PreflightErrors: append([]inferSystemOnePreflightError{}, preflightErrors...),
		Attempts:        []inferSystemOneAttempt{},
	}
	if len(preflightErrors) > 0 {
		receipts.Status = "error"
	} else {
		for _, pair := range pairs {
			if ctx.Err() != nil {
				receipts.Status = "error"
				receipts.RunErrorCode = inferSystemOneContextErrorCode(ctx.Err())
				break
			}
			receipts.AttemptedPairs++
			attemptStarted := dependencies.now().UTC()
			comparison, decideErr := client.Decide(ctx, pair.request)
			attempt, decision, errorCode := makeInferSystemOneAttempt(pair, comparison, decideErr, attemptStarted, dependencies.now, configuration)
			if ctx.Err() != nil && decideErr != nil {
				errorCode = inferSystemOneContextErrorCode(ctx.Err())
			}
			if errorCode == "" && attempt.Usage != nil && !canAddTokenTotals(receipts.KnownInputTokens, receipts.KnownOutputTokens, attempt.Usage) {
				errorCode = "malformed_response"
				decision = reportidentity.Decision{}
				attempt.Answer = nil
			}
			attempt.ErrorCode = errorCode
			receipts.Attempts = append(receipts.Attempts, attempt)
			if comparison.Attempted {
				receipts.ProviderCalls++
			}
			if attempt.Usage != nil && canAddTokenTotals(receipts.KnownInputTokens, receipts.KnownOutputTokens, attempt.Usage) {
				receipts.KnownInputTokens += attempt.Usage.InputTokens
				receipts.KnownOutputTokens += attempt.Usage.OutputTokens
			}
			if comparison.Attempted {
				if attempt.EstimatedCostUSD == nil {
					receipts.UnknownCostCalls++
				} else {
					receipts.knownCostUSD += *attempt.EstimatedCostUSD
					receipts.knownCostCount++
				}
			}
			if errorCode != "" {
				decision = reportidentity.Decision{LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, Choice: reportidentity.ChoiceError, InputSHA256: pair.pair.InputSHA256}
				receipts.FailedPairs++
				receipts.Status = "error"
			}
			decisions = append(decisions, decision)
			if errorCode != "" {
				break
			}
			receipts.CompletedPairs++
			if decision.Choice == reportidentity.ChoiceAbstain {
				receipts.AbstainedPairs++
			}
		}
	}

	receipts.MissingPairs = receipts.ExpectedPairs - receipts.CompletedPairs - receipts.FailedPairs
	if receipts.ProviderCalls == 0 {
		zero := 0.0
		receipts.EstimatedCostUSD = &zero
	} else if receipts.knownCostCount > 0 && finiteNonnegative(receipts.knownCostUSD) {
		knownCost := receipts.knownCostUSD
		receipts.EstimatedCostUSD = &knownCost
	}

	outputInput := reportidentity.Input{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      append([]review.Finding{}, input.Findings...),
		Threads:       append([]reportidentity.Thread{}, input.Threads...),
		Decisions:     decisions,
	}
	result, err := reportidentity.Replay(outputInput)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: assemble retained replay"))
	}
	if receipts.Status != "error" {
		receipts.Status = result.Status
	}
	fixtureBytes, err := json.Marshal(outputInput)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: encode retained fixture"))
	}
	replayBytes, err := json.Marshal(result)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: encode replay result"))
	}
	receiptsBytes, err := json.Marshal(receipts)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: encode receipts"))
	}
	if _, err := writeInferSystemOneArtifacts(outputDir, fixtureBytes, replayBytes, receiptsBytes, dependencies.rename); err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: output publication incomplete; manifest is not trustworthy"))
	}
	if err := writeInferSystemOneSummary(opts.Stdout, outputDir, receipts); err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: write summary"))
	}
	if len(preflightErrors) > 0 {
		return exitcode.Usage(errors.New("benchmark infer-system-one: preflight failed; retained originals were written"))
	}
	if receipts.FailedPairs > 0 {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: request failed; retained outputs were written"))
	}
	if receipts.RunErrorCode != "" {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-system-one: run was canceled; retained outputs were written"))
	}
	return nil
}

func inferSystemOneContextErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled" //nolint:misspell // Stable public error code is contract-defined.
	}
	return "transport"
}

func prepareInferSystemOnePairs(input reportidentity.Input, result reportidentity.Result, flags inferSystemOneFlags) ([]inferSystemOnePair, []inferSystemOnePreflightError) {
	findings := make(map[string]review.Finding, len(input.Findings))
	for _, finding := range input.Findings {
		findings[string(finding.ID)] = finding
	}
	threads := make(map[string]reportidentity.Thread, len(input.Threads))
	for _, thread := range input.Threads {
		threads[thread.ID] = thread
	}
	pairs := make([]inferSystemOnePair, 0, len(result.Pairs))
	var failures []inferSystemOnePreflightError
	limits := systemone.Limits{MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes}
	for _, pair := range result.Pairs {
		left, ok := findings[pair.LeftID]
		if !ok {
			failures = append(failures, inferSystemOnePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
			continue
		}
		leftRecord := inferenceFindingRecord(left)
		var rightRecord reportidentityinference.Record
		if input.Task == reportidentity.TaskCohort {
			right, ok := findings[pair.RightID]
			if !ok {
				failures = append(failures, inferSystemOnePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = inferenceFindingRecord(right)
		} else {
			right, ok := threads[pair.RightID]
			if !ok || !right.Eligible {
				failures = append(failures, inferSystemOnePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = inferenceThreadRecord(right)
		}
		request, err := reportidentityinference.Request(flags.model, flags.serviceTier, reportidentityinference.Task(input.Task), leftRecord, rightRecord)
		if err != nil {
			failures = append(failures, inferSystemOnePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
			continue
		}
		if flags.expectedModel != "" {
			request.ExpectedModel = flags.expectedModel
		}
		plan, err := systemone.Prepare(request, limits)
		if err != nil {
			failures = append(failures, inferSystemOnePreflightError{
				LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: systemone.ErrorCode(err),
			})
			continue
		}
		pairs = append(pairs, inferSystemOnePair{pair: pair, request: request, plan: plan})
	}
	return pairs, failures
}

func inferenceFindingRecord(finding review.Finding) reportidentityinference.Record {
	return reportidentityinference.Record{
		Kind: "finding",
		Finding: &reportidentityinference.Finding{
			ID: string(finding.ID), Severity: string(finding.Severity), FilePath: finding.FilePath,
			Anchor: reportidentityinference.Anchor{Kind: string(finding.Anchor.Kind), Side: string(finding.Anchor.Side), Line: finding.Anchor.Line},
			Body:   finding.Body, Anchoring: string(finding.Anchoring),
		},
	}
}

func inferenceThreadRecord(thread reportidentity.Thread) reportidentityinference.Record {
	return reportidentityinference.Record{Kind: "thread", Thread: &reportidentityinference.Thread{ID: thread.ID, Eligible: thread.Eligible, FilePath: thread.FilePath, Body: thread.Body}}
}

func makeInferSystemOneAttempt(pair inferSystemOnePair, receipt systemone.Receipt, decideErr error, started time.Time, now func() time.Time, configuration inferSystemOneConfiguration) (inferSystemOneAttempt, reportidentity.Decision, string) {
	if receipt.Duration == 0 && now != nil {
		receipt.Duration = now().Sub(started)
	}
	attempt := inferSystemOneAttempt{
		LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, PairSHA256: pair.pair.InputSHA256,
		PlannedRequestSHA256: pair.plan.RequestSHA256, RequestSHA256: receipt.RequestSHA256,
		ResponseSHA256: receipt.ResponseSHA256, RequestID: receipt.RequestID,
		RequestedModel: pair.request.Model, ReturnedModel: receipt.ReturnedModel,
		StartedUTC: started.UTC().Format(time.RFC3339Nano), DurationMS: receipt.Duration.Milliseconds(),
		HTTPAttempted: receipt.Attempted, HTTPStatus: receipt.HTTPStatus,
		StateBytes: pair.plan.StateBytes, RequestBytes: pair.plan.RequestBytes,
	}
	if receipt.Usage != nil && receipt.Usage.InputTokens >= 0 && receipt.Usage.OutputTokens >= 0 {
		attempt.Usage = &inferSystemOneUsage{InputTokens: receipt.Usage.InputTokens, OutputTokens: receipt.Usage.OutputTokens}
	}
	if receipt.Attempted {
		attempt.EstimatedCostUSD = estimateSystemOneCost(attempt.Usage, configuration)
	}
	if receipt.RequestSHA256 != "" && receipt.RequestSHA256 != pair.plan.RequestSHA256 {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	if decideErr != nil {
		return attempt, reportidentity.Decision{}, systemone.ErrorCode(decideErr)
	}
	if !receipt.Attempted {
		return attempt, reportidentity.Decision{}, "transport"
	}
	if receipt.RequestSHA256 != pair.plan.RequestSHA256 || receipt.RequestedModel != pair.request.Model || receipt.ReturnedModel != configuration.ExpectedModel || attempt.Usage == nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	answer, err := reportidentityinference.ParseAnswer(receipt.Answers)
	if err != nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	attempt.Answer = &inferSystemOneAnswer{Choice: reportidentity.Choice(answer.Choice), Probabilities: answer.Probabilities, Confidence: answer.Confidence}
	decision := reportidentity.Decision{LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, Choice: reportidentity.Choice(answer.Choice), InputSHA256: pair.pair.InputSHA256}
	return attempt, decision, ""
}

func estimateSystemOneCost(usage *inferSystemOneUsage, configuration inferSystemOneConfiguration) *float64 {
	if usage == nil || configuration.InputPricePerMillion == nil || configuration.OutputPricePerMillion == nil {
		return nil
	}
	inputCost := float64(usage.InputTokens) * *configuration.InputPricePerMillion / 1_000_000
	outputCost := float64(usage.OutputTokens) * *configuration.OutputPricePerMillion / 1_000_000
	total := inputCost + outputCost
	if !finiteNonnegative(inputCost) || !finiteNonnegative(outputCost) || !finiteNonnegative(total) {
		return nil
	}
	return &total
}

func canAddTokenTotals(currentInput, currentOutput int64, usage *inferSystemOneUsage) bool {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return false
	}
	maxInt64 := int64(^uint64(0) >> 1)
	return currentInput <= maxInt64-usage.InputTokens && currentOutput <= maxInt64-usage.OutputTokens
}

func createInferSystemOneOutputDir(path string) (string, error) {
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", err
	}
	// #nosec G302 -- the command contract requires an owner-only output directory.
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func checkInferSystemOneOutputDirNew(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return os.ErrExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeInferSystemOneArtifacts(outputDir string, fixture, replay, receipts []byte, rename func(string, string) error) ([]byte, error) {
	if rename == nil {
		rename = os.Rename
	}
	files := []struct {
		name string
		data []byte
	}{
		{name: "fixture.json", data: fixture},
		{name: "replay.json", data: replay},
		{name: "receipts.json", data: receipts},
	}
	for _, file := range files {
		if err := writeInferSystemOneFileAtomic(outputDir, file.name, file.data, rename); err != nil {
			return nil, err
		}
	}
	manifest := inferSystemOneManifest{SchemaVersion: inferSystemOneSchemaVersion, Files: inferSystemOneManifestFiles{
		FixtureSHA256: sha256Hex(fixture), ReplaySHA256: sha256Hex(replay), ReceiptsSHA256: sha256Hex(receipts),
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeInferSystemOneFileAtomic(outputDir, "manifest.json", data, rename); err != nil {
		return nil, err
	}
	return data, nil
}

func writeInferSystemOneFileAtomic(outputDir, name string, data []byte, rename func(string, string) error) error {
	temp, err := os.CreateTemp(outputDir, ".infer-system-one-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return rename(tempName, filepath.Join(outputDir, name))
}

func writeInferSystemOneSummary(w io.Writer, outputDir string, receipts inferSystemOneReceipts) error {
	paths := func(name string) string { return filepath.Join(outputDir, name) }
	summary := inferSystemOneSummary{
		SchemaVersion: inferSystemOneSchemaVersion, Status: receipts.Status,
		ExpectedPairs: receipts.ExpectedPairs, AttemptedPairs: receipts.AttemptedPairs, ProviderCalls: receipts.ProviderCalls,
		CompletedPairs: receipts.CompletedPairs, AbstainedPairs: receipts.AbstainedPairs, FailedPairs: receipts.FailedPairs,
		MissingPairs: receipts.MissingPairs, OutputDir: outputDir, FixturePath: paths("fixture.json"), ReplayPath: paths("replay.json"),
		ReceiptsPath: paths("receipts.json"), ManifestPath: paths("manifest.json"), KnownInputTokens: receipts.KnownInputTokens,
		KnownOutputTokens: receipts.KnownOutputTokens, EstimatedCostUSD: receipts.EstimatedCostUSD,
		UnknownCostCalls: receipts.UnknownCostCalls, Configuration: receipts.Configuration,
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(summary)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
