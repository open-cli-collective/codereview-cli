// Package identityexperiment contains the command-independent report identity
// inference runner shared by benchmark tooling and the opt-in identity shadow.
package identityexperiment

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// SchemaVersion is the version of the inference receipt and manifest formats.
const SchemaVersion = 1

// Plan describes the request bytes preflighted for one pair.
type Plan struct {
	RequestSHA256 string
	StateBytes    int
	RequestBytes  int
}

// Usage is the normalized provider token usage recorded in an attempt.
type Usage struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens       *int64 `json:"total_tokens,omitempty"`
}

// Answer is the normalized identity answer recorded in an attempt.
type Answer struct {
	Choice        reportidentity.Choice `json:"choice"`
	Probabilities [3]float64            `json:"probabilities"`
	Confidence    float64               `json:"confidence"`
}

// Configuration is the provenance and limit information retained for a run.
type Configuration struct {
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
	CredentialStore       string   `json:"credential_store"`
	CredentialName        string   `json:"credential_name"`
	CredentialKey         string   `json:"credential_key"`
	MaxStateBytes         int      `json:"max_state_bytes"`
	MaxRequestBytes       int      `json:"max_request_bytes"`
	TimeoutMS             int64    `json:"timeout_ms"`
	MaxCalls              int      `json:"max_calls"`
	InputPricePerMillion  *float64 `json:"input_price_per_million"`
	OutputPricePerMillion *float64 `json:"output_price_per_million"`
	Provider              string   `json:"provider,omitempty"`
	ContextStatus         string   `json:"context_status,omitempty"`
}

// PreflightError describes one pair or run-wide issue discovered before calls.
type PreflightError struct {
	LeftID      string `json:"left_id,omitempty"`
	RightID     string `json:"right_id,omitempty"`
	PairSHA256  string `json:"pair_sha256,omitempty"`
	ErrorCode   string `json:"error_code"`
	RequestSize int    `json:"request_bytes,omitempty"`
	StateSize   int    `json:"state_bytes,omitempty"`
}

// Attempt is the bounded provider metadata retained for one expected pair.
type Attempt struct {
	LeftID               string   `json:"left_id"`
	RightID              string   `json:"right_id"`
	PairSHA256           string   `json:"pair_sha256"`
	PlannedRequestSHA256 string   `json:"planned_request_sha256"`
	RequestSHA256        string   `json:"request_sha256,omitempty"`
	ResponseSHA256       string   `json:"response_sha256,omitempty"`
	RequestID            string   `json:"request_id,omitempty"`
	RequestedModel       string   `json:"requested_model,omitempty"`
	ReturnedModel        string   `json:"returned_model,omitempty"`
	Answer               *Answer  `json:"answer,omitempty"`
	Usage                *Usage   `json:"usage,omitempty"`
	StartedUTC           string   `json:"started_utc"`
	DurationMS           int64    `json:"duration_ms"`
	HTTPAttempted        bool     `json:"http_attempted"`
	HTTPStatus           int      `json:"http_status,omitempty"`
	EstimatedCostUSD     *float64 `json:"estimated_cost_usd"`
	ErrorCode            string   `json:"error_code,omitempty"`
	StateBytes           int      `json:"state_bytes"`
	RequestBytes         int      `json:"request_bytes"`
	ContextStatus        string   `json:"context_status,omitempty"`
}

// Receipts is the unchanged benchmark receipt wire object.
type Receipts struct {
	SchemaVersion     int                 `json:"schema_version"`
	ProtocolVersion   int                 `json:"protocol_version"`
	Task              reportidentity.Task `json:"task"`
	InputSHA256       string              `json:"input_sha256"`
	StartedUTC        string              `json:"started_utc"`
	Configuration     Configuration       `json:"configuration"`
	FindingCount      int                 `json:"finding_count"`
	ThreadCount       int                 `json:"thread_count"`
	ExpectedPairs     int                 `json:"expected_pairs"`
	AttemptedPairs    int                 `json:"attempted_pairs"`
	ProviderCalls     int                 `json:"provider_calls"`
	CompletedPairs    int                 `json:"completed_pairs"`
	AbstainedPairs    int                 `json:"abstained_pairs"`
	FailedPairs       int                 `json:"failed_pairs"`
	MissingPairs      int                 `json:"missing_pairs"`
	KnownInputTokens  int64               `json:"known_input_tokens"`
	KnownOutputTokens int64               `json:"known_output_tokens"`
	EstimatedCostUSD  *float64            `json:"estimated_cost_usd"`
	UnknownCostCalls  int                 `json:"unknown_cost_calls"`
	Status            string              `json:"status"`
	RunErrorCode      string              `json:"run_error_code,omitempty"`
	PreflightErrors   []PreflightError    `json:"preflight_errors"`
	Attempts          []Attempt           `json:"attempts"`
	knownCostUSD      float64             `json:"-"`
	knownCostCount    int                 `json:"-"`
}

// Receipt is the provider-neutral result of one Decider call.
type Receipt struct {
	RequestedModel string
	ReturnedModel  string
	RequestSHA256  string
	ResponseSHA256 string
	RequestID      string
	Answer         *reportidentityinference.ChoiceAnswer
	Usage          *Usage
	Duration       time.Duration
	Attempted      bool
	HTTPStatus     int
	ContextStatus  string
}

// Decider makes one comparison for a fully prepared report projection.
type Decider interface {
	Decide(context.Context, reportidentityinference.Projection) (Receipt, error)
}

// Backend contains provider-specific preparation, construction, validation
// and cost behavior. The runner remains responsible for the pair loop.
type Backend struct {
	Prepare       func(reportidentityinference.Projection) (Plan, error)
	NewClient     func(string) (Decider, error)
	ErrorCode     func(error) string
	EstimateCost  func(*Usage, *Configuration) *float64
	Provider      string
	ContextStatus string
}

// Dependencies are the only runtime seams used by Execute.
type Dependencies struct {
	ResolveAPIKey func(context.Context) (string, error)
	Now           func() time.Time
}

type pair struct {
	replayPair reportidentity.Pair
	projection reportidentityinference.Projection
	plan       Plan
}

// Prepared is an immutable, local-only preparation of a complete fixture.
// Its input bytes, pair list and projection storage are intentionally private.
type Prepared struct {
	inputJSON           []byte
	input               reportidentity.Input
	baseReplay          reportidentity.Result
	inputSHA256         string
	configuration       Configuration
	pairs               []pair
	preflightErrors     []PreflightError
	expectedPairs       int
	totalRequestBytes   int64
	retainOriginalInput bool
	valid               bool
}

// Result contains the retained typed outputs and exact bytes used by the
// shared Publisher. The encoded payloads cannot be modified by callers.
type Result struct {
	Fixture  reportidentity.Input
	Replay   reportidentity.Result
	Receipts Receipts

	fixtureJSON  []byte
	replayJSON   []byte
	receiptsJSON []byte
	err          error
}

// Err reports an unexpected assembly or encoding failure. Comparison failures
// are represented in Receipts and do not become Result errors.
func (result Result) Err() error {
	return result.err
}

// Prepare validates and preflights a retained fixture without performing I/O.
func Prepare(inputJSON []byte, configuration Configuration, backend Backend) (Prepared, error) {
	if backend.Prepare == nil || backend.ErrorCode == nil {
		return Prepared{}, errors.New("identityexperiment: backend is unavailable")
	}
	retainedJSON := append([]byte(nil), inputJSON...)
	input, err := reportidentity.Decode(retainedJSON)
	if err != nil || len(input.Decisions) != 0 {
		return Prepared{}, errors.New("identityexperiment: invalid fixture")
	}
	baseReplay, err := reportidentity.Replay(input)
	if err != nil {
		return Prepared{}, errors.New("identityexperiment: invalid fixture")
	}

	prepared := Prepared{
		inputJSON:     retainedJSON,
		input:         cloneInput(input),
		baseReplay:    baseReplay,
		inputSHA256:   SHA256(retainedJSON),
		configuration: cloneConfiguration(configuration),
		expectedPairs: len(baseReplay.Pairs),
		valid:         true,
	}
	prospective, err := prospectiveFixture(input, baseReplay)
	if err != nil {
		return Prepared{}, errors.New("identityexperiment: invalid fixture")
	}
	prospectiveJSON, err := encodeFixture(prospective)
	if err != nil {
		return Prepared{}, errors.New("identityexperiment: invalid fixture")
	}
	if len(prospectiveJSON) > reportidentity.MaxFixtureBytes {
		prepared.preflightErrors = []PreflightError{{ErrorCode: "retained_fixture_too_large"}}
		prepared.retainOriginalInput = true
		return prepared, nil
	}
	prepared.pairs, prepared.preflightErrors, prepared.totalRequestBytes = preparePairs(input, baseReplay, backend)
	if len(baseReplay.Pairs) > configuration.MaxCalls {
		prepared.preflightErrors = append(prepared.preflightErrors, PreflightError{ErrorCode: "invalid_input"})
	}
	return prepared, nil
}

// ExpectedPairs returns the full replay pair count, including preflight misses.
func (prepared Prepared) ExpectedPairs() int {
	return prepared.expectedPairs
}

// TotalRequestBytes returns the sum of successfully prepared request sizes.
func (prepared Prepared) TotalRequestBytes() int64 {
	return prepared.totalRequestBytes
}

// PreflightErrors returns an independent copy of preparation errors.
func (prepared Prepared) PreflightErrors() []PreflightError {
	return append([]PreflightError{}, prepared.preflightErrors...)
}

// WithPreflightError returns a copy with one run-wide preflight error appended.
func (prepared Prepared) WithPreflightError(code string) Prepared {
	clonedPrepared := prepared
	clonedPrepared.inputJSON = append([]byte(nil), prepared.inputJSON...)
	clonedPrepared.input = cloneInput(prepared.input)
	clonedPrepared.configuration = cloneConfiguration(prepared.configuration)
	clonedPrepared.pairs = append([]pair(nil), prepared.pairs...)
	clonedPrepared.preflightErrors = append(append([]PreflightError{}, prepared.preflightErrors...), PreflightError{ErrorCode: code})
	return clonedPrepared
}

func preparePairs(input reportidentity.Input, result reportidentity.Result, backend Backend) ([]pair, []PreflightError, int64) {
	findings := make(map[string]review.Finding, len(input.Findings))
	for _, finding := range input.Findings {
		findings[string(finding.ID)] = finding
	}
	threads := make(map[string]reportidentity.Thread, len(input.Threads))
	for _, thread := range input.Threads {
		threads[thread.ID] = thread
	}
	pairs := make([]pair, 0, len(result.Pairs))
	var failures []PreflightError
	var totalRequestBytes int64
	for _, replayPair := range result.Pairs {
		left, ok := findings[replayPair.LeftID]
		if !ok {
			failures = append(failures, pairPreflightError(replayPair, "invalid_input"))
			continue
		}
		leftRecord := findingRecord(left)
		var rightRecord reportidentityinference.Record
		if input.Task == reportidentity.TaskCohort {
			right, ok := findings[replayPair.RightID]
			if !ok {
				failures = append(failures, pairPreflightError(replayPair, "invalid_input"))
				continue
			}
			rightRecord = findingRecord(right)
		} else {
			right, ok := threads[replayPair.RightID]
			if !ok || !right.Eligible {
				failures = append(failures, pairPreflightError(replayPair, "invalid_input"))
				continue
			}
			rightRecord = threadRecord(right)
		}
		projection, err := reportidentityinference.Project(reportidentityinference.Task(input.Task), leftRecord, rightRecord)
		if err != nil {
			failures = append(failures, pairPreflightError(replayPair, "invalid_input"))
			continue
		}
		plan, err := backend.Prepare(projection)
		if err != nil {
			failures = append(failures, pairPreflightError(replayPair, safeErrorCode(backend, err)))
			continue
		}
		pairs = append(pairs, pair{replayPair: replayPair, projection: cloneProjection(projection), plan: plan})
		totalRequestBytes += int64(plan.RequestBytes)
	}
	return pairs, failures, totalRequestBytes
}

func pairPreflightError(replayPair reportidentity.Pair, code string) PreflightError {
	return PreflightError{LeftID: replayPair.LeftID, RightID: replayPair.RightID, PairSHA256: replayPair.InputSHA256, ErrorCode: code}
}

func prospectiveFixture(input reportidentity.Input, replay reportidentity.Result) (reportidentity.Input, error) {
	decisions := make([]reportidentity.Decision, 0, len(replay.Pairs))
	for _, pair := range replay.Pairs {
		decisions = append(decisions, reportidentity.Decision{
			LeftID: pair.LeftID, RightID: pair.RightID, Choice: reportidentity.ChoiceDifferent, InputSHA256: pair.InputSHA256,
		})
	}
	return reportidentity.Input{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      append([]review.Finding{}, input.Findings...),
		Threads:       append([]reportidentity.Thread{}, input.Threads...),
		Decisions:     decisions,
	}, nil
}

func findingRecord(finding review.Finding) reportidentityinference.Record {
	return reportidentityinference.Record{
		Kind: "finding",
		Finding: &reportidentityinference.Finding{
			ID: string(finding.ID), Severity: string(finding.Severity), FilePath: finding.FilePath,
			Anchor: reportidentityinference.Anchor{Kind: string(finding.Anchor.Kind), Side: string(finding.Anchor.Side), Line: finding.Anchor.Line},
			Body:   finding.Body, Anchoring: string(finding.Anchoring),
		},
	}
}

func threadRecord(thread reportidentity.Thread) reportidentityinference.Record {
	return reportidentityinference.Record{Kind: "thread", Thread: &reportidentityinference.Thread{ID: thread.ID, Eligible: thread.Eligible, FilePath: thread.FilePath, Body: thread.Body}}
}

// Execute runs the prepared pairs sequentially and retains all original data.
func Execute(ctx context.Context, prepared Prepared, dependencies Dependencies, backend Backend) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if !prepared.valid {
		return Result{err: errors.New("identityexperiment: invalid preparation")}
	}
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	startedUTC := now().UTC()
	configuration := cloneConfiguration(prepared.configuration)
	preflightErrors := prepared.PreflightErrors()
	var client Decider
	if len(preflightErrors) == 0 && len(prepared.pairs) > 0 {
		if dependencies.ResolveAPIKey == nil || backend.NewClient == nil || backend.ErrorCode == nil || backend.EstimateCost == nil {
			preflightErrors = append(preflightErrors, PreflightError{ErrorCode: "invalid_input"})
		} else if ctx.Err() != nil {
			preflightErrors = append(preflightErrors, PreflightError{ErrorCode: contextErrorCode(ctx.Err())})
		} else {
			apiKey, resolveErr := dependencies.ResolveAPIKey(ctx)
			if ctx.Err() != nil {
				preflightErrors = append(preflightErrors, PreflightError{ErrorCode: contextErrorCode(ctx.Err())})
			} else if resolveErr != nil || apiKey == "" {
				preflightErrors = append(preflightErrors, PreflightError{ErrorCode: "auth"})
			} else {
				var err error
				client, err = backend.NewClient(apiKey)
				if err != nil {
					preflightErrors = append(preflightErrors, PreflightError{ErrorCode: safeErrorCode(backend, err)})
				} else if client == nil {
					preflightErrors = append(preflightErrors, PreflightError{ErrorCode: "invalid_input"})
				}
			}
		}
	}

	decisions := make([]reportidentity.Decision, 0, len(prepared.pairs))
	receipts := Receipts{
		SchemaVersion: SchemaVersion, ProtocolVersion: reportidentityinference.ProtocolVersion,
		Task: prepared.input.Task, InputSHA256: prepared.inputSHA256,
		StartedUTC: startedUTC.Format(time.RFC3339Nano), Configuration: configuration,
		FindingCount: len(prepared.input.Findings), ThreadCount: len(prepared.input.Threads),
		ExpectedPairs: prepared.expectedPairs, Status: "complete",
		PreflightErrors: append([]PreflightError{}, preflightErrors...), Attempts: []Attempt{},
	}
	if len(preflightErrors) > 0 {
		receipts.Status = "error"
	} else {
		for _, preparedPair := range prepared.pairs {
			if ctx.Err() != nil {
				receipts.Status = "error"
				receipts.RunErrorCode = contextErrorCode(ctx.Err())
				break
			}
			receipts.AttemptedPairs++
			attemptStarted := now().UTC()
			comparison, decideErr := client.Decide(ctx, cloneProjection(preparedPair.projection))
			attempt, decision, errorCode := makeAttempt(preparedPair, comparison, decideErr, attemptStarted, now, &configuration, backend)
			if ctx.Err() != nil && decideErr != nil {
				errorCode = contextErrorCode(ctx.Err())
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
				decision = reportidentity.Decision{LeftID: preparedPair.replayPair.LeftID, RightID: preparedPair.replayPair.RightID, Choice: reportidentity.ChoiceError, InputSHA256: preparedPair.replayPair.InputSHA256}
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
		SchemaVersion: prepared.input.SchemaVersion,
		Task:          prepared.input.Task,
		Findings:      append([]review.Finding{}, prepared.input.Findings...),
		Threads:       append([]reportidentity.Thread{}, prepared.input.Threads...),
		Decisions:     decisions,
	}
	replay, err := reportidentity.Replay(outputInput)
	if err != nil {
		return Result{Fixture: outputInput, Receipts: receipts, err: errors.New("identityexperiment: assemble retained replay")}
	}
	if receipts.Status != "error" {
		receipts.Status = replay.Status
	}
	var fixtureJSON []byte
	if prepared.retainOriginalInput {
		fixtureJSON = append([]byte(nil), prepared.inputJSON...)
	} else {
		fixtureJSON, err = encodeFixture(outputInput)
		if err != nil {
			return Result{Fixture: outputInput, Replay: replay, Receipts: receipts, err: errors.New("identityexperiment: encode retained fixture")}
		}
	}
	replayJSON, err := json.Marshal(replay)
	if err != nil {
		return Result{Fixture: outputInput, Replay: replay, Receipts: receipts, err: errors.New("identityexperiment: encode replay result")}
	}
	receiptsJSON, err := json.Marshal(receipts)
	if err != nil {
		return Result{Fixture: outputInput, Replay: replay, Receipts: receipts, err: errors.New("identityexperiment: encode receipts")}
	}
	return Result{
		Fixture: outputInput, Replay: replay, Receipts: receipts,
		fixtureJSON: fixtureJSON, replayJSON: replayJSON, receiptsJSON: receiptsJSON,
	}
}

func makeAttempt(preparedPair pair, receipt Receipt, decideErr error, started time.Time, now func() time.Time, configuration *Configuration, backend Backend) (Attempt, reportidentity.Decision, string) {
	if receipt.Duration == 0 && now != nil {
		receipt.Duration = now().Sub(started)
	}
	attempt := Attempt{
		LeftID: preparedPair.replayPair.LeftID, RightID: preparedPair.replayPair.RightID,
		PairSHA256: preparedPair.replayPair.InputSHA256, PlannedRequestSHA256: preparedPair.plan.RequestSHA256,
		RequestSHA256: receipt.RequestSHA256, ResponseSHA256: receipt.ResponseSHA256,
		RequestID: receipt.RequestID, RequestedModel: configuration.RequestedModel,
		ReturnedModel: receipt.ReturnedModel, ContextStatus: receipt.ContextStatus,
		StartedUTC: started.UTC().Format(time.RFC3339Nano), DurationMS: receipt.Duration.Milliseconds(),
		HTTPAttempted: receipt.Attempted, HTTPStatus: receipt.HTTPStatus,
		StateBytes: preparedPair.plan.StateBytes, RequestBytes: preparedPair.plan.RequestBytes,
	}
	usageValid := true
	if receipt.Usage != nil {
		usageValid = validUsage(receipt.Usage)
		if usageValid {
			attempt.Usage = cloneUsage(receipt.Usage)
		}
	}
	if receipt.Attempted && backend.EstimateCost != nil {
		attempt.EstimatedCostUSD = backend.EstimateCost(attempt.Usage, configuration)
	}
	if !usageValid {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	if receipt.RequestSHA256 != "" && receipt.RequestSHA256 != preparedPair.plan.RequestSHA256 {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	if decideErr != nil {
		return attempt, reportidentity.Decision{}, safeErrorCode(backend, decideErr)
	}
	if !receipt.Attempted {
		return attempt, reportidentity.Decision{}, "transport"
	}
	if receipt.RequestSHA256 != preparedPair.plan.RequestSHA256 || receipt.RequestedModel != configuration.RequestedModel || receipt.ReturnedModel != configuration.ExpectedModel || attempt.Usage == nil || receipt.Answer == nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	answer, err := reportidentityinference.ParseChoice(*receipt.Answer)
	if err != nil {
		return attempt, reportidentity.Decision{}, "malformed_response"
	}
	attempt.Answer = &Answer{Choice: reportidentity.Choice(answer.Choice), Probabilities: answer.Probabilities, Confidence: answer.Confidence}
	decision := reportidentity.Decision{LeftID: preparedPair.replayPair.LeftID, RightID: preparedPair.replayPair.RightID, Choice: reportidentity.Choice(answer.Choice), InputSHA256: preparedPair.replayPair.InputSHA256}
	return attempt, decision, ""
}

func validUsage(usage *Usage) bool {
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

func cloneUsage(usage *Usage) *Usage {
	clone := *usage
	clone.CachedInputTokens = cloneInt64(usage.CachedInputTokens)
	clone.CacheWriteTokens = cloneInt64(usage.CacheWriteTokens)
	clone.TotalTokens = cloneInt64(usage.TotalTokens)
	return &clone
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

// EstimateCost applies the optional caller-provided input/output prices.
func EstimateCost(usage *Usage, configuration Configuration) *float64 {
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

func canAddTokenTotals(currentInput, currentOutput int64, usage *Usage) bool {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return false
	}
	maxInt64 := int64(^uint64(0) >> 1)
	return currentInput <= maxInt64-usage.InputTokens && currentOutput <= maxInt64-usage.OutputTokens
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func contextErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled" //nolint:misspell // Stable public error code is contract-defined.
	}
	return "transport"
}

func safeErrorCode(backend Backend, err error) string {
	if backend.ErrorCode == nil {
		return "invalid_input"
	}
	code := backend.ErrorCode(err)
	if code == "" {
		return "invalid_input"
	}
	return code
}

func cloneInput(input reportidentity.Input) reportidentity.Input {
	input.Findings = append([]review.Finding{}, input.Findings...)
	input.Threads = append([]reportidentity.Thread{}, input.Threads...)
	input.Decisions = append([]reportidentity.Decision{}, input.Decisions...)
	return input
}

func cloneConfiguration(configuration Configuration) Configuration {
	configuration.CandidateID = cloneString(configuration.CandidateID)
	configuration.ServiceTier = cloneString(configuration.ServiceTier)
	configuration.InputPricePerMillion = cloneFloat(configuration.InputPricePerMillion)
	configuration.OutputPricePerMillion = cloneFloat(configuration.OutputPricePerMillion)
	return configuration
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProjection(projection reportidentityinference.Projection) reportidentityinference.Projection {
	projection.State = append(json.RawMessage(nil), projection.State...)
	criteria := projection.Question.Criteria
	projection.Question.Criteria = make(map[string]string, len(criteria))
	for key, value := range criteria {
		projection.Question.Criteria[key] = value
	}
	return projection
}
