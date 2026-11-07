package benchmarkcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/mammoth"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	inferMammothSchemaVersion = 1
	defaultMammothTimeout     = 30 * time.Second
	minimumMammothTimeout     = time.Second
	maximumMammothTimeout     = 120 * time.Second
	maximumMammothCalls       = 10_000
	mammothInputPricePerToken = 0.10 / 1_000_000
)

type inferMammothFlags struct {
	maxCalls  int
	outputDir string
	timeout   time.Duration
}

type mammothComparer interface {
	Compare(context.Context, mammoth.Task, mammoth.Record, mammoth.Record) (mammoth.Receipt, error)
}

type inferMammothDependencies struct {
	lookupAPIKey func() (string, bool)
	newClient    func(mammoth.Options) (mammothComparer, error)
	now          func() time.Time
	rename       func(string, string) error
}

type inferMammothPair struct {
	pair  reportidentity.Pair
	left  mammoth.Record
	right mammoth.Record
	plan  mammoth.Plan
}

type inferMammothPreflightError struct {
	LeftID      string `json:"left_id,omitempty"`
	RightID     string `json:"right_id,omitempty"`
	PairSHA256  string `json:"pair_sha256,omitempty"`
	ErrorCode   string `json:"error_code"`
	RequestSize int    `json:"request_bytes,omitempty"`
	StateSize   int    `json:"state_bytes,omitempty"`
}

type inferMammothUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type inferMammothAnswer struct {
	Choice        reportidentity.Choice `json:"choice"`
	Probabilities [3]float64            `json:"probabilities"`
	Confidence    float64               `json:"confidence"`
}

type inferMammothAttempt struct {
	LeftID               string              `json:"left_id"`
	RightID              string              `json:"right_id"`
	PairSHA256           string              `json:"pair_sha256"`
	PlannedRequestSHA256 string              `json:"planned_request_sha256"`
	RequestSHA256        string              `json:"request_sha256,omitempty"`
	ResponseSHA256       string              `json:"response_sha256,omitempty"`
	RequestID            string              `json:"request_id,omitempty"`
	RequestedModel       string              `json:"requested_model,omitempty"`
	ReturnedModel        string              `json:"returned_model,omitempty"`
	Answer               *inferMammothAnswer `json:"answer,omitempty"`
	Usage                *inferMammothUsage  `json:"usage,omitempty"`
	StartedUTC           string              `json:"started_utc"`
	DurationMS           int64               `json:"duration_ms"`
	HTTPAttempted        bool                `json:"http_attempted"`
	HTTPStatus           int                 `json:"http_status,omitempty"`
	EstimatedCostUSD     *float64            `json:"estimated_cost_usd"`
	ErrorCode            string              `json:"error_code,omitempty"`
	StateBytes           int                 `json:"state_bytes"`
	RequestBytes         int                 `json:"request_bytes"`
}

type inferMammothReceipts struct {
	SchemaVersion     int                          `json:"schema_version"`
	ProtocolVersion   int                          `json:"protocol_version"`
	Task              reportidentity.Task          `json:"task"`
	InputSHA256       string                       `json:"input_sha256"`
	StartedUTC        string                       `json:"started_utc"`
	FindingCount      int                          `json:"finding_count"`
	ThreadCount       int                          `json:"thread_count"`
	ExpectedPairs     int                          `json:"expected_pairs"`
	AttemptedPairs    int                          `json:"attempted_pairs"`
	ProviderCalls     int                          `json:"provider_calls"`
	CompletedPairs    int                          `json:"completed_pairs"`
	AbstainedPairs    int                          `json:"abstained_pairs"`
	FailedPairs       int                          `json:"failed_pairs"`
	MissingPairs      int                          `json:"missing_pairs"`
	KnownInputTokens  int64                        `json:"known_input_tokens"`
	KnownOutputTokens int64                        `json:"known_output_tokens"`
	EstimatedCostUSD  float64                      `json:"estimated_cost_usd"`
	UnknownCostCalls  int                          `json:"unknown_cost_calls"`
	Status            string                       `json:"status"`
	RunErrorCode      string                       `json:"run_error_code,omitempty"`
	PreflightErrors   []inferMammothPreflightError `json:"preflight_errors"`
	Attempts          []inferMammothAttempt        `json:"attempts"`
}

type inferMammothManifestFiles struct {
	FixtureSHA256  string `json:"fixture.json"`
	ReplaySHA256   string `json:"replay.json"`
	ReceiptsSHA256 string `json:"receipts.json"`
}

type inferMammothManifest struct {
	SchemaVersion int                       `json:"schema_version"`
	Files         inferMammothManifestFiles `json:"files"`
}

type inferMammothSummary struct {
	SchemaVersion     int     `json:"schema_version"`
	Status            string  `json:"status"`
	ExpectedPairs     int     `json:"expected_pairs"`
	AttemptedPairs    int     `json:"attempted_pairs"`
	ProviderCalls     int     `json:"provider_calls"`
	CompletedPairs    int     `json:"completed_pairs"`
	AbstainedPairs    int     `json:"abstained_pairs"`
	FailedPairs       int     `json:"failed_pairs"`
	MissingPairs      int     `json:"missing_pairs"`
	OutputDir         string  `json:"output_dir"`
	FixturePath       string  `json:"fixture_path"`
	ReplayPath        string  `json:"replay_path"`
	ReceiptsPath      string  `json:"receipts_path"`
	ManifestPath      string  `json:"manifest_path"`
	KnownInputTokens  int64   `json:"known_input_tokens"`
	KnownOutputTokens int64   `json:"known_output_tokens"`
	EstimatedCostUSD  float64 `json:"estimated_cost_usd"`
	UnknownCostCalls  int     `json:"unknown_cost_calls"`
}

func newInferMammothCommand(opts *root.Options) *cobra.Command {
	return newInferMammothCommandWithDependencies(opts, defaultInferMammothDependencies())
}

func newInferMammothCommandWithDependencies(opts *root.Options, dependencies inferMammothDependencies) *cobra.Command {
	flags := inferMammothFlags{maxCalls: -1, timeout: defaultMammothTimeout}
	cmd := &cobra.Command{
		Use:   "infer-mammoth <fixture.json>",
		Short: "Run explicit pairwise identity inference over a local fixture",
		Args:  exitcode.ExactArgs(1, "benchmark infer-mammoth requires one fixture path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateInferMammothFlags(flags); err != nil {
				return exitcode.Usage(err)
			}
			return runInferMammoth(cmd.Context(), opts, args[0], flags, dependencies)
		},
	}
	cmd.Flags().IntVar(&flags.maxCalls, "max-calls", flags.maxCalls, "Maximum number of pairwise provider calls (required; 0-10000)")
	cmd.Flags().StringVar(&flags.outputDir, "output-dir", "", "New private output directory for the fixture, replay, receipts, and manifest (required)")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", flags.timeout, "Per-request timeout from 1s through 120s")
	return cmd
}

func defaultInferMammothDependencies() inferMammothDependencies {
	return inferMammothDependencies{
		lookupAPIKey: func() (string, bool) { return os.LookupEnv("BASETEN_API_KEY") },
		newClient: func(options mammoth.Options) (mammothComparer, error) {
			client, err := mammoth.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func validateInferMammothFlags(flags inferMammothFlags) error {
	if flags.maxCalls < 0 || flags.maxCalls > maximumMammothCalls {
		return fmt.Errorf("benchmark infer-mammoth: --max-calls must be in the range 0-%d", maximumMammothCalls)
	}
	if strings.TrimSpace(flags.outputDir) == "" {
		return errors.New("benchmark infer-mammoth: --output-dir is required")
	}
	if flags.timeout < minimumMammothTimeout || flags.timeout > maximumMammothTimeout {
		return errors.New("benchmark infer-mammoth: --timeout must be from 1s through 120s")
	}
	return nil
}

func runInferMammoth(ctx context.Context, opts *root.Options, fixturePath string, flags inferMammothFlags, dependencies inferMammothDependencies) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.now == nil || dependencies.lookupAPIKey == nil || dependencies.newClient == nil || dependencies.rename == nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: command dependencies are unavailable"))
	}
	inputBytes, err := readReplayFixture(fixturePath)
	if err != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark infer-mammoth: read fixture: %w", err))
	}
	input, err := reportidentity.Decode(inputBytes)
	if err != nil {
		return exitcode.Usage(errors.New("benchmark infer-mammoth: invalid fixture"))
	}
	if len(input.Decisions) != 0 {
		return exitcode.Usage(errors.New("benchmark infer-mammoth: fixture decisions must be empty"))
	}
	baseResult, err := reportidentity.Replay(input)
	if err != nil {
		return exitcode.Usage(errors.New("benchmark infer-mammoth: invalid fixture"))
	}
	startedUTC := dependencies.now().UTC()
	inputDigest := sha256Hex(inputBytes)
	pairs, preflightErrors := prepareInferMammothPairs(input, baseResult)
	if len(pairs) > flags.maxCalls {
		preflightErrors = append(preflightErrors, inferMammothPreflightError{ErrorCode: "invalid_input"})
	}
	if err := checkInferMammothOutputDirNew(flags.outputDir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New("benchmark infer-mammoth: output directory must be new"))
		}
		return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark infer-mammoth: inspect output directory: %w", err))
	}

	var client mammothComparer
	if len(preflightErrors) == 0 && len(pairs) > 0 {
		apiKey, ok := dependencies.lookupAPIKey()
		if !ok || apiKey == "" {
			preflightErrors = append(preflightErrors, inferMammothPreflightError{ErrorCode: "auth"})
		} else {
			client, err = dependencies.newClient(mammoth.Options{APIKey: apiKey, Timeout: flags.timeout})
			if err != nil {
				preflightErrors = append(preflightErrors, inferMammothPreflightError{ErrorCode: mammoth.ErrorCode(err)})
			} else if client == nil {
				preflightErrors = append(preflightErrors, inferMammothPreflightError{ErrorCode: "invalid_input"})
			}
		}
	}

	outputDir, err := createInferMammothOutputDir(flags.outputDir)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New("benchmark infer-mammoth: output directory must be new"))
		}
		return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark infer-mammoth: create output directory: %w", err))
	}

	decisions := make([]reportidentity.Decision, 0, len(pairs))
	receipts := inferMammothReceipts{
		SchemaVersion:   inferMammothSchemaVersion,
		ProtocolVersion: 1,
		Task:            input.Task,
		InputSHA256:     inputDigest,
		StartedUTC:      startedUTC.Format(time.RFC3339Nano),
		FindingCount:    len(input.Findings),
		ThreadCount:     len(input.Threads),
		ExpectedPairs:   len(baseResult.Pairs),
		Status:          "complete",
		PreflightErrors: append([]inferMammothPreflightError{}, preflightErrors...),
		Attempts:        []inferMammothAttempt{},
	}
	if len(preflightErrors) > 0 {
		receipts.Status = "error"
	} else if len(pairs) > 0 {
		for _, pair := range pairs {
			if ctx.Err() != nil {
				receipts.Status = "error"
				receipts.RunErrorCode = inferMammothContextErrorCode(ctx.Err())
				break
			}
			receipts.AttemptedPairs++
			attemptStarted := dependencies.now().UTC()
			comparison, compareErr := client.Compare(ctx, mammoth.Task(input.Task), pair.left, pair.right)
			receipt, decision, errCode := makeInferMammothAttempt(pair, comparison, compareErr, attemptStarted, func() time.Time { return dependencies.now().UTC() })
			if ctx.Err() != nil && compareErr != nil {
				errCode = inferMammothContextErrorCode(ctx.Err())
			}
			receipt.ErrorCode = errCode
			receipts.Attempts = append(receipts.Attempts, receipt)
			if comparison.Attempted {
				receipts.ProviderCalls++
			}
			if comparison.Usage != nil {
				receipts.KnownInputTokens += comparison.Usage.InputTokens
				receipts.KnownOutputTokens += comparison.Usage.OutputTokens
				receipts.EstimatedCostUSD += estimateMammothCost(comparison.Usage.InputTokens)
			} else if comparison.Attempted {
				receipts.UnknownCostCalls++
			}
			if errCode != "" {
				decision = reportidentity.Decision{
					LeftID:      pair.pair.LeftID,
					RightID:     pair.pair.RightID,
					Choice:      reportidentity.ChoiceError,
					InputSHA256: pair.pair.InputSHA256,
				}
				receipts.FailedPairs++
				receipts.Status = "error"
			}
			decisions = append(decisions, decision)
			if errCode != "" {
				break
			}
			receipts.CompletedPairs++
			if decision.Choice == reportidentity.ChoiceAbstain {
				receipts.AbstainedPairs++
			}
		}
	}

	receipts.MissingPairs = receipts.ExpectedPairs - receipts.CompletedPairs - receipts.FailedPairs

	outputInput := reportidentity.Input{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      append([]review.Finding{}, input.Findings...),
		Threads:       append([]reportidentity.Thread{}, input.Threads...),
		Decisions:     decisions,
	}
	result, err := reportidentity.Replay(outputInput)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: assemble retained replay"))
	}
	if receipts.Status != "error" {
		receipts.Status = result.Status
	}
	fixtureBytes, err := json.Marshal(outputInput)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: encode retained fixture"))
	}
	replayBytes, err := json.Marshal(result)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: encode replay result"))
	}
	receiptsBytes, err := json.Marshal(receipts)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: encode receipts"))
	}
	manifestBytes, err := writeInferMammothArtifacts(outputDir, fixtureBytes, replayBytes, receiptsBytes, dependencies.rename)
	if err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: output publication incomplete; manifest is not trustworthy"))
	}
	_ = manifestBytes

	if err := writeInferMammothSummary(opts.Stdout, outputDir, receipts); err != nil {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: write summary"))
	}
	if len(preflightErrors) > 0 {
		return exitcode.Usage(errors.New("benchmark infer-mammoth: preflight failed; retained originals were written"))
	}
	if receipts.FailedPairs > 0 {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: request failed; retained outputs were written"))
	}
	if receipts.RunErrorCode != "" {
		return exitcode.With(exitcode.Failure, errors.New("benchmark infer-mammoth: run was canceled; retained outputs were written"))
	}
	return nil
}

func inferMammothContextErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled" //nolint:misspell // stable category matches internal/mammoth.ErrorCode.
	}
	return "transport"
}

func prepareInferMammothPairs(input reportidentity.Input, result reportidentity.Result) ([]inferMammothPair, []inferMammothPreflightError) {
	findings := make(map[string]review.Finding, len(input.Findings))
	for _, finding := range input.Findings {
		findings[string(finding.ID)] = finding
	}
	threads := make(map[string]reportidentity.Thread, len(input.Threads))
	for _, thread := range input.Threads {
		threads[thread.ID] = thread
	}
	pairs := make([]inferMammothPair, 0, len(result.Pairs))
	var failures []inferMammothPreflightError
	for _, pair := range result.Pairs {
		left, ok := findings[pair.LeftID]
		if !ok {
			failures = append(failures, inferMammothPreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
			continue
		}
		leftRecord := mammothFindingRecord(left)
		var rightRecord mammoth.Record
		if input.Task == reportidentity.TaskCohort {
			right, ok := findings[pair.RightID]
			if !ok {
				failures = append(failures, inferMammothPreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = mammothFindingRecord(right)
		} else {
			right, ok := threads[pair.RightID]
			if !ok || !right.Eligible {
				failures = append(failures, inferMammothPreflightError{LeftID: pair.LeftID, RightID: pair.RightID, PairSHA256: pair.InputSHA256, ErrorCode: "invalid_input"})
				continue
			}
			rightRecord = mammothThreadRecord(right)
		}
		plan, err := mammoth.Prepare(mammoth.Task(input.Task), leftRecord, rightRecord)
		if err != nil {
			failures = append(failures, inferMammothPreflightError{
				LeftID:      pair.LeftID,
				RightID:     pair.RightID,
				PairSHA256:  pair.InputSHA256,
				ErrorCode:   mammoth.ErrorCode(err),
				RequestSize: 0,
				StateSize:   0,
			})
			continue
		}
		pairs = append(pairs, inferMammothPair{pair: pair, left: leftRecord, right: rightRecord, plan: plan})
	}
	return pairs, failures
}

func mammothFindingRecord(finding review.Finding) mammoth.Record {
	return mammoth.Record{
		Kind: "finding",
		Finding: &mammoth.Finding{
			ID:        string(finding.ID),
			Severity:  string(finding.Severity),
			FilePath:  finding.FilePath,
			Anchor:    mammoth.Anchor{Kind: string(finding.Anchor.Kind), Side: string(finding.Anchor.Side), Line: finding.Anchor.Line},
			Body:      finding.Body,
			Anchoring: string(finding.Anchoring),
		},
	}
}

func mammothThreadRecord(thread reportidentity.Thread) mammoth.Record {
	return mammoth.Record{
		Kind:   "thread",
		Thread: &mammoth.Thread{ID: thread.ID, Eligible: thread.Eligible, FilePath: thread.FilePath, Body: thread.Body},
	}
}

func makeInferMammothAttempt(pair inferMammothPair, receipt mammoth.Receipt, compareErr error, started time.Time, now func() time.Time) (inferMammothAttempt, reportidentity.Decision, string) {
	startedUTC := started.UTC().Format(time.RFC3339Nano)
	if now != nil && receipt.Duration == 0 {
		receipt.Duration = now().Sub(started)
	}
	attempt := inferMammothAttempt{
		LeftID:               pair.pair.LeftID,
		RightID:              pair.pair.RightID,
		PairSHA256:           pair.pair.InputSHA256,
		PlannedRequestSHA256: pair.plan.RequestSHA256,
		RequestSHA256:        receipt.RequestSHA256,
		ResponseSHA256:       receipt.ResponseSHA256,
		RequestID:            receipt.RequestID,
		RequestedModel:       receipt.RequestedModel,
		ReturnedModel:        receipt.ReturnedModel,
		StartedUTC:           startedUTC,
		DurationMS:           receipt.Duration.Milliseconds(),
		HTTPAttempted:        receipt.Attempted,
		HTTPStatus:           receipt.HTTPStatus,
		StateBytes:           pair.plan.StateBytes,
		RequestBytes:         pair.plan.RequestBytes,
	}
	if receipt.Usage != nil {
		attempt.Usage = &inferMammothUsage{InputTokens: receipt.Usage.InputTokens, OutputTokens: receipt.Usage.OutputTokens}
		cost := estimateMammothCost(receipt.Usage.InputTokens)
		attempt.EstimatedCostUSD = &cost
	}
	if receipt.Answer != nil {
		choice := reportidentity.Choice(receipt.Answer.Choice)
		attempt.Answer = &inferMammothAnswer{Choice: choice, Probabilities: receipt.Answer.Probabilities, Confidence: receipt.Answer.Confidence}
		if compareErr == nil {
			if receipt.RequestSHA256 != pair.plan.RequestSHA256 {
				return attempt, reportidentity.Decision{}, "malformed_response"
			}
			if !receipt.Attempted {
				return attempt, reportidentity.Decision{}, "transport"
			}
			if choice != reportidentity.ChoiceSame && choice != reportidentity.ChoiceDifferent && choice != reportidentity.ChoiceAbstain {
				return attempt, reportidentity.Decision{}, "malformed_response"
			}
			decision := reportidentity.Decision{LeftID: pair.pair.LeftID, RightID: pair.pair.RightID, Choice: choice, InputSHA256: pair.pair.InputSHA256}
			return attempt, decision, ""
		}
	}
	if attempt.RequestSHA256 == "" && receipt.RequestSHA256 == "" {
		attempt.RequestSHA256 = pair.plan.RequestSHA256
	}
	if compareErr != nil {
		return attempt, reportidentity.Decision{}, mammoth.ErrorCode(compareErr)
	}
	return attempt, reportidentity.Decision{}, "malformed_response"
}

func estimateMammothCost(inputTokens int64) float64 {
	return float64(inputTokens) * mammothInputPricePerToken
}

func createInferMammothOutputDir(path string) (string, error) {
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

func checkInferMammothOutputDirNew(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return os.ErrExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeInferMammothArtifacts(outputDir string, fixture, replay, receipts []byte, rename func(string, string) error) ([]byte, error) {
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
		if err := writeInferMammothFileAtomic(outputDir, file.name, file.data, rename); err != nil {
			return nil, err
		}
	}
	manifest := inferMammothManifest{
		SchemaVersion: inferMammothSchemaVersion,
		Files: inferMammothManifestFiles{
			FixtureSHA256:  sha256Hex(fixture),
			ReplaySHA256:   sha256Hex(replay),
			ReceiptsSHA256: sha256Hex(receipts),
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeInferMammothFileAtomic(outputDir, "manifest.json", data, rename); err != nil {
		return nil, err
	}
	return data, nil
}

func writeInferMammothFileAtomic(outputDir, name string, data []byte, rename func(string, string) error) error {
	temp, err := os.CreateTemp(outputDir, ".infer-mammoth-*.tmp")
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
	if err := rename(tempName, filepath.Join(outputDir, name)); err != nil {
		return err
	}
	return nil
}

func writeInferMammothSummary(w io.Writer, outputDir string, receipts inferMammothReceipts) error {
	paths := func(name string) string { return filepath.Join(outputDir, name) }
	summary := inferMammothSummary{
		SchemaVersion:     inferMammothSchemaVersion,
		Status:            receipts.Status,
		ExpectedPairs:     receipts.ExpectedPairs,
		AttemptedPairs:    receipts.AttemptedPairs,
		ProviderCalls:     receipts.ProviderCalls,
		CompletedPairs:    receipts.CompletedPairs,
		AbstainedPairs:    receipts.AbstainedPairs,
		FailedPairs:       receipts.FailedPairs,
		MissingPairs:      receipts.MissingPairs,
		OutputDir:         outputDir,
		FixturePath:       paths("fixture.json"),
		ReplayPath:        paths("replay.json"),
		ReceiptsPath:      paths("receipts.json"),
		ManifestPath:      paths("manifest.json"),
		KnownInputTokens:  receipts.KnownInputTokens,
		KnownOutputTokens: receipts.KnownOutputTokens,
		EstimatedCostUSD:  receipts.EstimatedCostUSD,
		UnknownCostCalls:  receipts.UnknownCostCalls,
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(summary)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
