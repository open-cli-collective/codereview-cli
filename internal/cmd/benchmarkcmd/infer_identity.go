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

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	identityInferenceSchemaVersion  = 1
	defaultIdentityInferenceTimeout = 30 * time.Second
	minimumIdentityInferenceTimeout = time.Second
	maximumIdentityInferenceTimeout = 120 * time.Second
	maximumIdentityInferenceCalls   = 10_000
)

type identityInferenceFlags struct {
	model                 string
	expectedModel         string
	endpoint              string
	apiKeyEnv             string
	maxStateBytes         int
	maxRequestBytes       int
	maxCalls              int
	outputDir             string
	timeout               time.Duration
	inputPricePerMillion  float64
	outputPricePerMillion float64
	inputPriceSet         bool
	outputPriceSet        bool
	task                  string
	suitePath             string
	suiteSHA256           string
	candidateID           *string
	providerName          string
	adapterKind           string
	// These settings are provider-specific at the CLI. OpenAI uses fixed
	// Bearer authentication and does not set a service tier.
	authScheme  string
	serviceTier string
}

type identityInferencePlan struct {
	RequestSHA256 string
	StateBytes    int
	RequestBytes  int
}

type identityInferencePair struct {
	pair       reportidentity.Pair
	projection reportidentityinference.Projection
	plan       identityInferencePlan
}

type identityInferenceUsage struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens       *int64 `json:"total_tokens,omitempty"`
}

type identityInferenceAnswer struct {
	Choice        reportidentity.Choice `json:"choice"`
	Probabilities [3]float64            `json:"probabilities"`
	Confidence    float64               `json:"confidence"`
}

type identityInferenceConfiguration struct {
	Task                  string   `json:"task"`
	SuitePath             string   `json:"suite_path"`
	SuiteSHA256           string   `json:"suite_sha256"`
	CandidateID           *string  `json:"candidate_id"`
	ProviderName          string   `json:"provider_name"`
	AdapterKind           string   `json:"adapter_kind"`
	Endpoint              string   `json:"endpoint"`
	RequestedModel        string   `json:"requested_model"`
	ExpectedModel         string   `json:"expected_model"`
	ServiceTier           *string  `json:"service_tier"`
	AuthScheme            string   `json:"auth_scheme"`
	APIKeyEnv             string   `json:"api_key_env"`
	MaxStateBytes         int      `json:"max_state_bytes"`
	MaxRequestBytes       int      `json:"max_request_bytes"`
	TimeoutMS             int64    `json:"timeout_ms"`
	MaxCalls              int      `json:"max_calls"`
	InputPricePerMillion  *float64 `json:"input_price_per_million"`
	OutputPricePerMillion *float64 `json:"output_price_per_million"`
	Provider              string   `json:"provider,omitempty"`
	ContextStatus         string   `json:"context_status,omitempty"`
}

type identityInferencePreflightError struct {
	LeftID      string `json:"left_id,omitempty"`
	RightID     string `json:"right_id,omitempty"`
	PairSHA256  string `json:"pair_sha256,omitempty"`
	ErrorCode   string `json:"error_code"`
	RequestSize int    `json:"request_bytes,omitempty"`
	StateSize   int    `json:"state_bytes,omitempty"`
}

type identityInferenceAttempt struct {
	LeftID               string                   `json:"left_id"`
	RightID              string                   `json:"right_id"`
	PairSHA256           string                   `json:"pair_sha256"`
	PlannedRequestSHA256 string                   `json:"planned_request_sha256"`
	RequestSHA256        string                   `json:"request_sha256,omitempty"`
	ResponseSHA256       string                   `json:"response_sha256,omitempty"`
	RequestID            string                   `json:"request_id,omitempty"`
	RequestedModel       string                   `json:"requested_model,omitempty"`
	ReturnedModel        string                   `json:"returned_model,omitempty"`
	Answer               *identityInferenceAnswer `json:"answer,omitempty"`
	Usage                *identityInferenceUsage  `json:"usage,omitempty"`
	StartedUTC           string                   `json:"started_utc"`
	DurationMS           int64                    `json:"duration_ms"`
	HTTPAttempted        bool                     `json:"http_attempted"`
	HTTPStatus           int                      `json:"http_status,omitempty"`
	EstimatedCostUSD     *float64                 `json:"estimated_cost_usd"`
	ErrorCode            string                   `json:"error_code,omitempty"`
	StateBytes           int                      `json:"state_bytes"`
	RequestBytes         int                      `json:"request_bytes"`
	ContextStatus        string                   `json:"context_status,omitempty"`
}

type identityInferenceReceipts struct {
	SchemaVersion     int                               `json:"schema_version"`
	ProtocolVersion   int                               `json:"protocol_version"`
	Task              reportidentity.Task               `json:"task"`
	InputSHA256       string                            `json:"input_sha256"`
	StartedUTC        string                            `json:"started_utc"`
	Configuration     identityInferenceConfiguration    `json:"configuration"`
	FindingCount      int                               `json:"finding_count"`
	ThreadCount       int                               `json:"thread_count"`
	ExpectedPairs     int                               `json:"expected_pairs"`
	AttemptedPairs    int                               `json:"attempted_pairs"`
	ProviderCalls     int                               `json:"provider_calls"`
	CompletedPairs    int                               `json:"completed_pairs"`
	AbstainedPairs    int                               `json:"abstained_pairs"`
	FailedPairs       int                               `json:"failed_pairs"`
	MissingPairs      int                               `json:"missing_pairs"`
	KnownInputTokens  int64                             `json:"known_input_tokens"`
	KnownOutputTokens int64                             `json:"known_output_tokens"`
	EstimatedCostUSD  *float64                          `json:"estimated_cost_usd"`
	UnknownCostCalls  int                               `json:"unknown_cost_calls"`
	Status            string                            `json:"status"`
	RunErrorCode      string                            `json:"run_error_code,omitempty"`
	PreflightErrors   []identityInferencePreflightError `json:"preflight_errors"`
	Attempts          []identityInferenceAttempt        `json:"attempts"`
	knownCostUSD      float64                           `json:"-"`
	knownCostCount    int                               `json:"-"`
}

type identityInferenceManifestFiles struct {
	FixtureSHA256  string `json:"fixture.json"`
	ReplaySHA256   string `json:"replay.json"`
	ReceiptsSHA256 string `json:"receipts.json"`
}

type identityInferenceManifest struct {
	SchemaVersion int                            `json:"schema_version"`
	Files         identityInferenceManifestFiles `json:"files"`
}

type identityInferenceSummary struct {
	SchemaVersion     int                            `json:"schema_version"`
	Status            string                         `json:"status"`
	ExpectedPairs     int                            `json:"expected_pairs"`
	AttemptedPairs    int                            `json:"attempted_pairs"`
	ProviderCalls     int                            `json:"provider_calls"`
	CompletedPairs    int                            `json:"completed_pairs"`
	AbstainedPairs    int                            `json:"abstained_pairs"`
	FailedPairs       int                            `json:"failed_pairs"`
	MissingPairs      int                            `json:"missing_pairs"`
	OutputDir         string                         `json:"output_dir"`
	FixturePath       string                         `json:"fixture_path"`
	ReplayPath        string                         `json:"replay_path"`
	ReceiptsPath      string                         `json:"receipts_path"`
	ManifestPath      string                         `json:"manifest_path"`
	KnownInputTokens  int64                          `json:"known_input_tokens"`
	KnownOutputTokens int64                          `json:"known_output_tokens"`
	EstimatedCostUSD  *float64                       `json:"estimated_cost_usd"`
	UnknownCostCalls  int                            `json:"unknown_cost_calls"`
	Configuration     identityInferenceConfiguration `json:"configuration"`
}

type identityInferenceReceipt struct {
	RequestedModel string
	ReturnedModel  string
	RequestSHA256  string
	ResponseSHA256 string
	RequestID      string
	Answer         *reportidentityinference.ChoiceAnswer
	Usage          *identityInferenceUsage
	Duration       time.Duration
	Attempted      bool
	HTTPStatus     int
	ContextStatus  string
}

type identityInferenceDecider interface {
	Decide(context.Context, reportidentityinference.Projection) (identityInferenceReceipt, error)
}

type identityInferenceBackend struct {
	prepare       func(reportidentityinference.Projection) (identityInferencePlan, error)
	newClient     func(string) (identityInferenceDecider, error)
	errorCode     func(error) string
	estimateCost  func(*identityInferenceUsage, *identityInferenceConfiguration) *float64
	provider      string
	contextStatus string
}

type identityInferenceDependencies struct {
	lookupAPIKey func(string) (string, bool)
	now          func() time.Time
	rename       func(string, string) error
}

func runIdentityInference(ctx context.Context, opts *root.Options, commandName, fixturePath string, flags identityInferenceFlags, dependencies identityInferenceDependencies, backend identityInferenceBackend) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.rename == nil || backend.prepare == nil || backend.newClient == nil || backend.errorCode == nil || backend.estimateCost == nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": command dependencies are unavailable"))
	}
	inputBytes, err := readReplayFixture(fixturePath)
	if err != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf(commandName+": read fixture: %w", err))
	}
	input, err := reportidentity.Decode(inputBytes)
	if err != nil {
		return exitcode.Usage(errors.New(commandName + ": invalid fixture"))
	}
	if len(input.Decisions) != 0 {
		return exitcode.Usage(errors.New(commandName + ": fixture decisions must be empty"))
	}
	baseResult, err := reportidentity.Replay(input)
	if err != nil {
		return exitcode.Usage(errors.New(commandName + ": invalid fixture"))
	}
	startedUTC := dependencies.now().UTC()
	inputDigest := sha256Hex(inputBytes)
	configuration := identityInferenceConfigurationFor(flags, backend)
	pairs, preflightErrors := prepareIdentityInferencePairs(input, baseResult, backend)
	if len(baseResult.Pairs) > flags.maxCalls {
		preflightErrors = append(preflightErrors, identityInferencePreflightError{ErrorCode: "invalid_input"})
	}
	if err := checkIdentityInferenceOutputDirNew(flags.outputDir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New(commandName + ": results directory must be new"))
		}
		return exitcode.With(exitcode.Failure, errors.New(commandName+": could not inspect output directory"))
	}

	var client identityInferenceDecider
	if len(preflightErrors) == 0 && len(pairs) > 0 {
		apiKey, ok := dependencies.lookupAPIKey(flags.apiKeyEnv)
		if !ok || apiKey == "" {
			preflightErrors = append(preflightErrors, identityInferencePreflightError{ErrorCode: "auth"})
		} else {
			client, err = backend.newClient(apiKey)
			if err != nil {
				preflightErrors = append(preflightErrors, identityInferencePreflightError{ErrorCode: backend.errorCode(err)})
			} else if client == nil {
				preflightErrors = append(preflightErrors, identityInferencePreflightError{ErrorCode: "invalid_input"})
			}
		}
	}

	outputDir, err := createIdentityInferenceOutputDir(flags.outputDir)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New(commandName + ": results directory must be new"))
		}
		return exitcode.With(exitcode.Failure, errors.New(commandName+": could not create output directory"))
	}

	decisions := make([]reportidentity.Decision, 0, len(pairs))
	receipts := identityInferenceReceipts{
		SchemaVersion:   identityInferenceSchemaVersion,
		ProtocolVersion: 1,
		Task:            input.Task,
		InputSHA256:     inputDigest,
		StartedUTC:      startedUTC.Format(time.RFC3339Nano),
		Configuration:   configuration,
		FindingCount:    len(input.Findings),
		ThreadCount:     len(input.Threads),
		ExpectedPairs:   len(baseResult.Pairs),
		Status:          "complete",
		PreflightErrors: append([]identityInferencePreflightError{}, preflightErrors...),
		Attempts:        []identityInferenceAttempt{},
	}
	if len(preflightErrors) > 0 {
		receipts.Status = "error"
	} else {
		for _, pair := range pairs {
			if ctx.Err() != nil {
				receipts.Status = "error"
				receipts.RunErrorCode = identityInferenceContextErrorCode(ctx.Err())
				break
			}
			receipts.AttemptedPairs++
			attemptStarted := dependencies.now().UTC()
			comparison, decideErr := client.Decide(ctx, pair.projection)
			attempt, decision, errorCode := makeIdentityInferenceAttempt(pair, comparison, decideErr, attemptStarted, dependencies.now, &configuration, backend)
			if ctx.Err() != nil && decideErr != nil {
				errorCode = identityInferenceContextErrorCode(ctx.Err())
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
		return exitcode.With(exitcode.Failure, errors.New(commandName+": assemble retained replay"))
	}
	if receipts.Status != "error" {
		receipts.Status = result.Status
	}
	fixtureBytes, err := json.Marshal(outputInput)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": encode retained fixture"))
	}
	replayBytes, err := json.Marshal(result)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": encode replay result"))
	}
	receiptsBytes, err := json.Marshal(receipts)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": encode receipts"))
	}
	if _, err := writeIdentityInferenceArtifacts(outputDir, fixtureBytes, replayBytes, receiptsBytes, dependencies.rename); err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": output publication incomplete; manifest is not trustworthy"))
	}
	if err := writeIdentityInferenceSummary(opts.Stdout, outputDir, receipts); err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": write summary"))
	}
	if len(preflightErrors) > 0 {
		return exitcode.Usage(errors.New(commandName + ": preflight failed; retained originals were written"))
	}
	if receipts.FailedPairs > 0 {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": request failed; retained outputs were written"))
	}
	if receipts.RunErrorCode != "" {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": run was canceled; retained outputs were written"))
	}
	return nil
}

func prepareIdentityInferencePairs(input reportidentity.Input, result reportidentity.Result, backend identityInferenceBackend) ([]identityInferencePair, []identityInferencePreflightError) {
	findings := make(map[string]review.Finding, len(input.Findings))
	for _, finding := range input.Findings {
		findings[string(finding.ID)] = finding
	}
	threads := make(map[string]reportidentity.Thread, len(input.Threads))
	for _, thread := range input.Threads {
		threads[thread.ID] = thread
	}
	pairs := make([]identityInferencePair, 0, len(result.Pairs))
	var failures []identityInferencePreflightError
	for _, pair := range result.Pairs {
		left, ok := findings[pair.LeftID]
		if !ok {
			failures = append(failures, identityInferencePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
			continue
		}
		leftRecord := inferenceFindingRecord(left)
		var rightRecord reportidentityinference.Record
		if input.Task == reportidentity.TaskCohort {
			right, ok := findings[pair.RightID]
			if !ok {
				failures = append(failures, identityInferencePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = inferenceFindingRecord(right)
		} else {
			right, ok := threads[pair.RightID]
			if !ok || !right.Eligible {
				failures = append(failures, identityInferencePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = inferenceThreadRecord(right)
		}
		projection, err := reportidentityinference.Project(reportidentityinference.Task(input.Task), leftRecord, rightRecord)
		if err != nil {
			failures = append(failures, identityInferencePreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
			continue
		}
		plan, err := backend.prepare(projection)
		if err != nil {
			failures = append(failures, identityInferencePreflightError{
				LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: backend.errorCode(err),
			})
			continue
		}
		pairs = append(pairs, identityInferencePair{pair: pair, projection: projection, plan: plan})
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

func makeIdentityInferenceAttempt(pair identityInferencePair, receipt identityInferenceReceipt, decideErr error, started time.Time, now func() time.Time, configuration *identityInferenceConfiguration, backend identityInferenceBackend) (identityInferenceAttempt, reportidentity.Decision, string) {
	if receipt.Duration == 0 && now != nil {
		receipt.Duration = now().Sub(started)
	}
	attempt := identityInferenceAttempt{
		LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, PairSHA256: pair.pair.InputSHA256,
		PlannedRequestSHA256: pair.plan.RequestSHA256, RequestSHA256: receipt.RequestSHA256,
		ResponseSHA256: receipt.ResponseSHA256, RequestID: receipt.RequestID,
		RequestedModel: configuration.RequestedModel, ReturnedModel: receipt.ReturnedModel,
		ContextStatus: receipt.ContextStatus,
		StartedUTC:    started.UTC().Format(time.RFC3339Nano), DurationMS: receipt.Duration.Milliseconds(),
		HTTPAttempted: receipt.Attempted, HTTPStatus: receipt.HTTPStatus,
		StateBytes: pair.plan.StateBytes, RequestBytes: pair.plan.RequestBytes,
	}
	usageValid := true
	if receipt.Usage != nil {
		usageValid = validIdentityInferenceUsage(receipt.Usage)
		if usageValid {
			attempt.Usage = cloneIdentityInferenceUsage(receipt.Usage)
		}
	}
	if receipt.Attempted {
		attempt.EstimatedCostUSD = backend.estimateCost(attempt.Usage, configuration)
	}
	if !usageValid {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	if receipt.RequestSHA256 != "" && receipt.RequestSHA256 != pair.plan.RequestSHA256 {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	if decideErr != nil {
		return attempt, reportidentity.Decision{}, backend.errorCode(decideErr)
	}
	if !receipt.Attempted {
		return attempt, reportidentity.Decision{}, "transport"
	}
	if receipt.RequestSHA256 != pair.plan.RequestSHA256 || receipt.RequestedModel != configuration.RequestedModel || receipt.ReturnedModel != configuration.ExpectedModel || attempt.Usage == nil || receipt.Answer == nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	answer, err := reportidentityinference.ParseChoice(*receipt.Answer)
	if err != nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	attempt.Answer = &identityInferenceAnswer{Choice: reportidentity.Choice(answer.Choice), Probabilities: answer.Probabilities, Confidence: answer.Confidence}
	decision := reportidentity.Decision{LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, Choice: reportidentity.Choice(answer.Choice), InputSHA256: pair.pair.InputSHA256}
	return attempt, decision, ""
}

func validIdentityInferenceUsage(usage *identityInferenceUsage) bool {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return false
	}
	if usage.CachedInputTokens != nil && (*usage.CachedInputTokens < 0 || *usage.CachedInputTokens > usage.InputTokens) {
		return false
	}
	if usage.CacheWriteTokens != nil && (*usage.CacheWriteTokens < 0 || *usage.CacheWriteTokens > usage.InputTokens) {
		return false
	}
	return usage.TotalTokens == nil || *usage.TotalTokens >= 0
}

func cloneIdentityInferenceUsage(usage *identityInferenceUsage) *identityInferenceUsage {
	clone := *usage
	if usage.CachedInputTokens != nil {
		value := *usage.CachedInputTokens
		clone.CachedInputTokens = &value
	}
	if usage.CacheWriteTokens != nil {
		value := *usage.CacheWriteTokens
		clone.CacheWriteTokens = &value
	}
	if usage.TotalTokens != nil {
		value := *usage.TotalTokens
		clone.TotalTokens = &value
	}
	return &clone
}

func identityInferenceConfigurationFor(flags identityInferenceFlags, backend identityInferenceBackend) identityInferenceConfiguration {
	authScheme := flags.authScheme
	switch authScheme {
	case "api-key":
		authScheme = "Api-Key"
	case "bearer":
		authScheme = "Bearer"
	}
	var tier *string
	if flags.serviceTier != "" {
		value := flags.serviceTier
		tier = &value
	}
	configuration := identityInferenceConfiguration{
		Task: flags.task, SuitePath: flags.suitePath, SuiteSHA256: flags.suiteSHA256, CandidateID: flags.candidateID,
		ProviderName: flags.providerName, AdapterKind: flags.adapterKind,
		Endpoint: flags.endpoint, RequestedModel: flags.model, ExpectedModel: effectiveIdentityInferenceExpectedModel(flags),
		ServiceTier: tier, AuthScheme: authScheme, APIKeyEnv: flags.apiKeyEnv,
		MaxStateBytes: flags.maxStateBytes, MaxRequestBytes: flags.maxRequestBytes,
		TimeoutMS: flags.timeout.Milliseconds(), MaxCalls: flags.maxCalls,
		Provider: backend.provider, ContextStatus: backend.contextStatus,
	}
	if flags.inputPriceSet {
		inputPrice := flags.inputPricePerMillion
		outputPrice := flags.outputPricePerMillion
		configuration.InputPricePerMillion = &inputPrice
		configuration.OutputPricePerMillion = &outputPrice
	}
	return configuration
}

func effectiveIdentityInferenceExpectedModel(flags identityInferenceFlags) string {
	if flags.expectedModel != "" {
		return flags.expectedModel
	}
	return flags.model
}

func identityInferenceContextErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled" //nolint:misspell // Stable public error code is contract-defined.
	}
	return "transport"
}

func validIdentityInferenceEndpoint(value string) bool {
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

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validateIdentityInferenceCoreFlags(commandName string, flags identityInferenceFlags) error {
	if strings.TrimSpace(flags.model) == "" {
		return errors.New(commandName + ": --model is required")
	}
	if flags.expectedModel != "" && strings.TrimSpace(flags.expectedModel) == "" {
		return errors.New(commandName + ": --expected-model must be nonempty when specified")
	}
	if flags.maxCalls < 0 || flags.maxCalls > maximumIdentityInferenceCalls {
		return fmt.Errorf("%s: --max-calls must be in the range 0-%d", commandName, maximumIdentityInferenceCalls)
	}
	if strings.TrimSpace(flags.outputDir) == "" {
		return errors.New(commandName + ": --results-dir is required")
	}
	if flags.timeout < minimumIdentityInferenceTimeout || flags.timeout > maximumIdentityInferenceTimeout {
		return errors.New(commandName + ": --timeout must be from 1s through 120s")
	}
	if !validIdentityInferenceEndpoint(flags.endpoint) {
		return errors.New(commandName + ": --endpoint must be a valid HTTPS URL without query, userinfo, or fragment")
	}
	if !validAPIKeyEnvName(flags.apiKeyEnv) {
		return errors.New(commandName + ": --api-key-env must be an environment variable name")
	}
	return nil
}

func validateIdentityInferenceLimitsAndPrices(commandName string, flags identityInferenceFlags) error {
	if flags.maxStateBytes <= 0 || flags.maxRequestBytes <= 0 {
		return errors.New(commandName + ": byte limits must be positive")
	}
	if flags.inputPriceSet != flags.outputPriceSet {
		return errors.New(commandName + ": both price flags must be supplied together")
	}
	if flags.inputPriceSet && (!finiteNonnegative(flags.inputPricePerMillion) || !finiteNonnegative(flags.outputPricePerMillion)) {
		return errors.New(commandName + ": prices must be finite and nonnegative")
	}
	return nil
}

func validateIdentityInferenceFlags(commandName string, flags identityInferenceFlags) error {
	if err := validateIdentityInferenceCoreFlags(commandName, flags); err != nil {
		return err
	}
	return validateIdentityInferenceLimitsAndPrices(commandName, flags)
}

func estimateIdentityInferenceCost(usage *identityInferenceUsage, configuration identityInferenceConfiguration) *float64 {
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

func canAddTokenTotals(currentInput, currentOutput int64, usage *identityInferenceUsage) bool {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return false
	}
	maxInt64 := int64(^uint64(0) >> 1)
	return currentInput <= maxInt64-usage.InputTokens && currentOutput <= maxInt64-usage.OutputTokens
}

func createIdentityInferenceOutputDir(path string) (string, error) {
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

func checkIdentityInferenceOutputDirNew(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return os.ErrExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeIdentityInferenceArtifacts(outputDir string, fixture, replay, receipts []byte, rename func(string, string) error) ([]byte, error) {
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
		if err := writeIdentityInferenceFileAtomic(outputDir, file.name, file.data, rename); err != nil {
			return nil, err
		}
	}
	manifest := identityInferenceManifest{SchemaVersion: identityInferenceSchemaVersion, Files: identityInferenceManifestFiles{
		FixtureSHA256: sha256Hex(fixture), ReplaySHA256: sha256Hex(replay), ReceiptsSHA256: sha256Hex(receipts),
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeIdentityInferenceFileAtomic(outputDir, "manifest.json", data, rename); err != nil {
		return nil, err
	}
	return data, nil
}

func writeIdentityInferenceFileAtomic(outputDir, name string, data []byte, rename func(string, string) error) error {
	temp, err := os.CreateTemp(outputDir, ".infer-identity-*.tmp")
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

func writeIdentityInferenceSummary(w io.Writer, outputDir string, receipts identityInferenceReceipts) error {
	paths := func(name string) string { return filepath.Join(outputDir, name) }
	summary := identityInferenceSummary{
		SchemaVersion: identityInferenceSchemaVersion, Status: receipts.Status,
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
