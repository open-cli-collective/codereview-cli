// Package identityshadow runs private report identity diagnostics after the
// authoritative review path has completed.
package identityshadow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// Observer runs a private, diagnostic-only comparison after the authoritative
// review has completed. It returns no value that can affect review behavior.
type Observer interface {
	Observe(context.Context, reportidentitysnapshot.Snapshot, string)
}

// Options injects the fixed System One credential resolver and existing
// bounded transport/filesystem seams. New only stores these values.
type Options struct {
	ConfigPath          string
	Warnings            io.Writer
	ResolveSystemOneKey func(context.Context, string, string) (string, error)
	SystemOne           identityexperiment.SystemOneDependencies
	Files               identityexperiment.FileDependencies
	Now                 func() time.Time
}

type observer struct {
	options Options
}

// New constructs an observer without reading configuration, touching files,
// resolving credentials, or constructing a provider client.
func New(options Options) Observer {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Warnings == nil {
		options.Warnings = io.Discard
	}
	return observer{options: options}
}

type taskReceipt struct {
	Status            string   `json:"status"`
	ExpectedPairs     int      `json:"expected_pairs"`
	AttemptedPairs    int      `json:"attempted_pairs"`
	ProviderCalls     int      `json:"provider_calls"`
	CompletedPairs    int      `json:"completed_pairs"`
	AbstainedPairs    int      `json:"abstained_pairs"`
	FailedPairs       int      `json:"failed_pairs"`
	MissingPairs      int      `json:"missing_pairs"`
	KnownInputTokens  int64    `json:"known_input_tokens"`
	KnownOutputTokens int64    `json:"known_output_tokens"`
	EstimatedCostUSD  *float64 `json:"estimated_cost_usd"`
	UnknownCostCalls  int      `json:"unknown_cost_calls"`
}

type aggregateReceipt struct {
	SchemaVersion                int                                  `json:"schema_version"`
	Status                       string                               `json:"status"`
	ErrorCode                    string                               `json:"error_code,omitempty"`
	ComparisonStatus             string                               `json:"comparison_status"`
	StartedUTC                   string                               `json:"started_utc"`
	EndedUTC                     string                               `json:"ended_utc"`
	DurationMS                   int64                                `json:"duration_ms"`
	CredentialLookupDurationMS   int64                                `json:"credential_lookup_duration_ms"`
	InferenceDurationMS          int64                                `json:"inference_duration_ms"`
	ConfigFileSHA256             string                               `json:"config_file_sha256,omitempty"`
	ConfigSHA256                 string                               `json:"config_sha256,omitempty"`
	Configuration                *Config                              `json:"configuration,omitempty"`
	SnapshotSHA256               string                               `json:"snapshot_sha256"`
	SourceSHA256                 string                               `json:"source_sha256"`
	BaselineSHA256               string                               `json:"baseline_sha256"`
	ComparisonSHA256             string                               `json:"comparison_sha256,omitempty"`
	NativeMetrics                reportidentitysnapshot.NativeMetrics `json:"native_metrics"`
	ExpectedPairs                int                                  `json:"expected_pairs"`
	AttemptedPairs               int                                  `json:"attempted_pairs"`
	ProviderCalls                int                                  `json:"provider_calls"`
	CompletedPairs               int                                  `json:"completed_pairs"`
	AbstainedPairs               int                                  `json:"abstained_pairs"`
	FailedPairs                  int                                  `json:"failed_pairs"`
	MissingPairs                 int                                  `json:"missing_pairs"`
	KnownInputTokens             int64                                `json:"known_input_tokens"`
	KnownOutputTokens            int64                                `json:"known_output_tokens"`
	EstimatedCostUSD             *float64                             `json:"estimated_cost_usd"`
	UnknownCostCalls             int                                  `json:"unknown_cost_calls"`
	Cohort                       taskReceipt                          `json:"cohort"`
	PriorThreads                 taskReceipt                          `json:"prior_threads"`
	TaskReceiptsOnPublishFailure *retainedTaskReceipts                `json:"task_receipts_on_publish_failure,omitempty"`
}

type retainedTaskReceipts struct {
	Cohort       identityexperiment.Receipts `json:"cohort"`
	PriorThreads identityexperiment.Receipts `json:"prior_threads"`
}

func (runner observer) Observe(parent context.Context, snapshot reportidentitysnapshot.Snapshot, attemptDir string) {
	if parent == nil {
		parent = context.Background()
	}
	snapshot = reportidentitysnapshot.Clone(snapshot)
	started := runner.options.Now()
	receipt := aggregateReceipt{
		SchemaVersion: 1, Status: "invalid_input", ComparisonStatus: "unavailable",
		StartedUTC: started.UTC().Format(time.RFC3339Nano), NativeMetrics: snapshot.Baseline.Metrics,
	}
	priorIDMap, sourceInputErr := makePriorIDMap(snapshot.PriorRoots)
	sourceRecord, sourceBytes, sourceErr := makeSourceArtifact(snapshot, priorIDMap)
	if sourceErr == nil {
		receipt.SnapshotSHA256 = sourceRecord.SnapshotSHA256
		receipt.SourceSHA256 = digest(sourceBytes)
		_, baselineBytes, baselineErr := makeBaselineArtifact(snapshot, sourceRecord.SnapshotSHA256)
		if baselineErr == nil {
			receipt.BaselineSHA256 = digest(baselineBytes)
		} else {
			sourceErr = baselineErr
		}
	}
	if err := secureAttemptDirectory(attemptDir, runner.options.Files); err != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.warn("artifact_failure", attemptDir)
		return
	}
	if sourceErr != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.finishReceipt(attemptDir, &receipt, started, nil, false)
		runner.warn("artifact_failure", attemptDir)
		return
	}
	_, baselineBytes, _ := makeBaselineArtifact(snapshot, sourceRecord.SnapshotSHA256)
	if err := atomicWritePrivate(attemptDir, sourceFileName, sourceBytes, runner.options.Files); err != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.finishReceipt(attemptDir, &receipt, started, nil, false)
		runner.warn("artifact_failure", attemptDir)
		return
	}
	if err := atomicWritePrivate(attemptDir, baselineFileName, baselineBytes, runner.options.Files); err != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.finishReceipt(attemptDir, &receipt, started, nil, false)
		runner.warn("artifact_failure", attemptDir)
		return
	}
	if sourceInputErr != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	if err := parent.Err(); err != nil {
		receipt.Status, receipt.ErrorCode = statusForContext(err), statusForContext(err)
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}

	configBytes, err := readConfigBytes(runner.options.ConfigPath)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_config", "invalid_config"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	receipt.ConfigFileSHA256 = digest(configBytes)
	config, err := parseConfig(configBytes)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_config", "invalid_config"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	configBytesCanonical, err := encodeCanonical(config)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_config", "invalid_config"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	receipt.ConfigSHA256 = digest(configBytesCanonical)
	receipt.Configuration = &config

	cohortInput, priorInput, priorIDMap, err := buildTaskInputs(snapshot)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	backendConfiguration := runnerConfiguration(config, reportidentity.TaskCohort, receipt.ConfigSHA256)
	backend := identityexperiment.SystemOneBackend(backendConfiguration, runner.options.SystemOne)
	cohortBytes, err := encodeCanonical(cohortInput)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	priorBytes, err := encodeCanonical(priorInput)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	cohortPrepared, err := identityexperiment.Prepare(cohortBytes, runnerConfiguration(config, reportidentity.TaskCohort, receipt.ConfigSHA256), backend)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	priorPrepared, err := identityexperiment.Prepare(priorBytes, runnerConfiguration(config, reportidentity.TaskPriorThread, receipt.ConfigSHA256), backend)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
		runner.finishReceipt(attemptDir, &receipt, started, nil, true)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	receipt.ExpectedPairs = cohortPrepared.ExpectedPairs() + priorPrepared.ExpectedPairs()
	if totalRequestBytes := cohortPrepared.TotalRequestBytes() + priorPrepared.TotalRequestBytes(); totalRequestBytes > config.Budget.MaxTotalRequestBytes || receipt.ExpectedPairs > config.Budget.MaxCalls {
		cohortPrepared = cohortPrepared.WithPreflightError("budget_exceeded")
		priorPrepared = priorPrepared.WithPreflightError("budget_exceeded")
		receipt.Status, receipt.ErrorCode = "budget_exceeded", "budget_exceeded"
	} else if len(cohortPrepared.PreflightErrors()) > 0 || len(priorPrepared.PreflightErrors()) > 0 {
		cohortPrepared = cohortPrepared.WithPreflightError("invalid_input")
		priorPrepared = priorPrepared.WithPreflightError("invalid_input")
		receipt.Status, receipt.ErrorCode = "invalid_input", "invalid_input"
	}
	cohortPublisher, err := identityexperiment.ReserveOutput(filepath.Join(attemptDir, "cohort"), runner.options.Files)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.finishReceipt(attemptDir, &receipt, started, nil, false)
		runner.warn(receipt.Status, attemptDir)
		return
	}
	priorPublisher, err := identityexperiment.ReserveOutput(filepath.Join(attemptDir, "prior_threads"), runner.options.Files)
	if err != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		runner.finishReceipt(attemptDir, &receipt, started, nil, false)
		runner.warn(receipt.Status, attemptDir)
		return
	}

	dependencies := identityexperiment.Dependencies{Now: runner.options.Now}
	preflightFailure := receipt.ErrorCode
	if preflightFailure == "" && receipt.ExpectedPairs > 0 {
		if err := parent.Err(); err != nil {
			preflightFailure = statusForContext(err)
			receipt.Status, receipt.ErrorCode = preflightFailure, preflightFailure
		} else if runner.options.ResolveSystemOneKey == nil {
			preflightFailure = "auth"
			receipt.Status, receipt.ErrorCode = preflightFailure, preflightFailure
		} else {
			lookupStarted := runner.options.Now()
			apiKey, lookupErr := runner.options.ResolveSystemOneKey(parent, config.Provider.Credential.Store, config.Provider.Credential.Name)
			receipt.CredentialLookupDurationMS = nonnegativeMilliseconds(runner.options.Now().Sub(lookupStarted))
			if contextErr := parent.Err(); contextErr != nil {
				preflightFailure = statusForContext(contextErr)
				receipt.Status, receipt.ErrorCode = preflightFailure, preflightFailure
			} else if lookupErr != nil || apiKey == "" {
				preflightFailure = "auth"
				receipt.Status, receipt.ErrorCode = preflightFailure, preflightFailure
			} else {
				dependencies.ResolveAPIKey = func(context.Context) (string, error) { return apiKey, nil }
			}
		}
	}

	var cohortResult, priorResult identityexperiment.Result
	var cohortManifest, priorManifest *identityexperiment.Manifest
	var inferenceStarted time.Time
	var inferenceContext = parent
	var cancel context.CancelFunc = func() {}
	if preflightFailure == "" && receipt.ExpectedPairs > 0 {
		inferenceStarted = runner.options.Now()
		inferenceContext, cancel = context.WithTimeout(parent, config.InferenceLimit)
	}
	cohortPreparedForExecute := cohortPrepared
	priorPreparedForExecute := priorPrepared
	if preflightFailure != "" {
		cohortPreparedForExecute = cohortPreparedForExecute.WithPreflightError(preflightFailure)
		priorPreparedForExecute = priorPreparedForExecute.WithPreflightError(preflightFailure)
	}
	cohortResult = identityexperiment.Execute(inferenceContext, cohortPreparedForExecute, dependencies, backend)
	cohortFailed := cohortResult.Err() != nil || cohortResult.Receipts.Status == "error"
	if cohortFailed {
		cohortFailure := taskFailureCode(cohortResult)
		if cohortFailure == "" {
			cohortFailure = "malformed_response"
		}
		priorPreparedForExecute = priorPrepared.WithPreflightError(cohortFailure)
		priorResult = identityexperiment.Execute(inferenceContext, priorPreparedForExecute, identityexperiment.Dependencies{Now: runner.options.Now}, backend)
	} else {
		priorResult = identityexperiment.Execute(inferenceContext, priorPreparedForExecute, dependencies, backend)
	}
	cancel()
	if !inferenceStarted.IsZero() {
		receipt.InferenceDurationMS = nonnegativeMilliseconds(runner.options.Now().Sub(inferenceStarted))
	}

	cohortManifestValue, cohortPublishErr := cohortPublisher.Publish(cohortResult)
	if cohortPublishErr == nil {
		cohortManifest = &cohortManifestValue
	}
	priorManifestValue, priorPublishErr := priorPublisher.Publish(priorResult)
	if priorPublishErr == nil {
		priorManifest = &priorManifestValue
	}
	if cohortPublishErr != nil || priorPublishErr != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		receipt.TaskReceiptsOnPublishFailure = &retainedTaskReceipts{
			Cohort: cohortResult.Receipts, PriorThreads: priorResult.Receipts,
		}
	}
	setTaskReceipts(&receipt, cohortResult, priorResult)
	if receipt.ErrorCode == "" {
		receipt.ErrorCode = taskFailureCode(cohortResult)
		if receipt.ErrorCode == "" {
			receipt.ErrorCode = taskFailureCode(priorResult)
		}
		if receipt.ErrorCode != "" {
			receipt.Status = normalizeStatus(receipt.ErrorCode)
		} else if cohortResult.Receipts.MissingPairs+priorResult.Receipts.MissingPairs > 0 || cohortResult.Receipts.AbstainedPairs+priorResult.Receipts.AbstainedPairs > 0 {
			receipt.Status = "unresolved"
		} else {
			receipt.Status = "complete"
		}
	}

	comparison := compareIdentity(snapshot, cohortResult.Replay, priorResult.Replay, priorIDMap)
	comparisonBytes, comparisonErr := encodeCanonical(comparison)
	if comparisonErr == nil {
		if err := atomicWritePrivate(attemptDir, comparisonFileName, comparisonBytes, runner.options.Files); err == nil {
			receipt.ComparisonSHA256 = digest(comparisonBytes)
			receipt.ComparisonStatus = "available"
		} else {
			comparisonErr = err
		}
	}
	if comparisonErr != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		receipt.ComparisonStatus = "unavailable"
	}
	manifests := map[string]nestedManifest{}
	if cohortManifest != nil {
		manifestBytes, _ := encodeCanonical(cohortManifest)
		manifests["cohort"] = nestedManifest{SHA256: digest(manifestBytes), Manifest: *cohortManifest}
	}
	if priorManifest != nil {
		manifestBytes, _ := encodeCanonical(priorManifest)
		manifests["prior_threads"] = nestedManifest{SHA256: digest(manifestBytes), Manifest: *priorManifest}
	}
	allTaskArtifactsPublished := cohortManifest != nil && priorManifest != nil
	runner.finishReceipt(attemptDir, &receipt, started, manifests, allTaskArtifactsPublished && comparisonErr == nil)
	if receipt.Status != "complete" && receipt.Status != "unresolved" {
		runner.warn(receipt.Status, attemptDir)
	}
}

func (runner observer) finishReceipt(attemptDir string, receipt *aggregateReceipt, started time.Time, nested map[string]nestedManifest, allowManifest bool) {
	ended := runner.options.Now()
	receipt.EndedUTC = ended.UTC().Format(time.RFC3339Nano)
	receipt.DurationMS = nonnegativeMilliseconds(ended.Sub(started))
	receiptBytes, err := encodeCanonical(receipt)
	if err != nil || atomicWritePrivate(attemptDir, receiptFileName, receiptBytes, runner.options.Files) != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
		return
	}
	if !allowManifest {
		return
	}
	files := map[string]string{
		sourceFileName:   receipt.SourceSHA256,
		baselineFileName: receipt.BaselineSHA256,
		receiptFileName:  digest(receiptBytes),
	}
	if receipt.ComparisonSHA256 != "" {
		files[comparisonFileName] = receipt.ComparisonSHA256
	}
	manifest := aggregateManifest{
		SchemaVersion: 1, Status: receipt.Status, ComparisonStatus: receipt.ComparisonStatus,
		Files: files, TaskManifests: nested,
	}
	manifestBytes, err := encodeCanonical(manifest)
	if err != nil || atomicWritePrivate(attemptDir, manifestFileName, manifestBytes, runner.options.Files) != nil {
		receipt.Status, receipt.ErrorCode = "artifact_failure", "artifact_failure"
	}
}

func (runner observer) warn(category, attemptDir string) {
	path := strings.NewReplacer("\n", "_", "\r", "_", "\t", "_").Replace(attemptDir)
	_, _ = fmt.Fprintf(runner.options.Warnings, "cr identity-shadow status=%s artifacts=%q\n", normalizeStatus(category), path)
}

func readConfigBytes(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("identity shadow config path is missing")
	}
	// #nosec G304 -- the standalone configuration path is explicit opt-in input.
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("identity shadow config is unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxConfigBytes {
		return nil, errors.New("identity shadow config cannot be read")
	}
	return data, nil
}

func makePriorIDMap(roots []reportidentitysnapshot.PriorRoot) (map[string]string, error) {
	ids := make(map[string]string, len(roots))
	originals := make(map[string]struct{}, len(roots))
	replayIDs := make(map[string]string, len(roots))
	var invalid bool
	for _, root := range roots {
		if _, duplicate := originals[root.ID]; duplicate || !utf8.ValidString(root.ID) {
			invalid = true
			continue
		}
		originals[root.ID] = struct{}{}
		replayID := deterministicThreadID(root.ID)
		if prior, collision := replayIDs[replayID]; collision && prior != root.ID {
			invalid = true
			continue
		}
		replayIDs[replayID] = root.ID
		ids[root.ID] = replayID
	}
	if invalid {
		return ids, errors.New("identity shadow prior-root ID mapping is invalid")
	}
	return ids, nil
}

func buildTaskInputs(snapshot reportidentitysnapshot.Snapshot) (reportidentity.Input, reportidentity.Input, map[string]string, error) {
	priorIDMap, err := makePriorIDMap(snapshot.PriorRoots)
	if err != nil {
		return reportidentity.Input{}, reportidentity.Input{}, priorIDMap, err
	}
	findings := append([]review.Finding{}, snapshot.Findings...)
	if findings == nil {
		findings = []review.Finding{}
	}
	threads := make([]reportidentity.Thread, 0, len(snapshot.PriorRoots))
	for _, root := range snapshot.PriorRoots {
		threads = append(threads, reportidentity.Thread{
			ID: priorIDMap[root.ID], Eligible: true, FilePath: root.FilePath, Body: root.Body,
		})
	}
	cohort := reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort,
		Findings: append([]review.Finding{}, findings...), Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{},
	}
	prior := reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskPriorThread,
		Findings: append([]review.Finding{}, findings...), Threads: threads, Decisions: []reportidentity.Decision{},
	}
	return cohort, prior, priorIDMap, nil
}

func runnerConfiguration(config Config, task reportidentity.Task, configSHA256 string) identityexperiment.Configuration {
	configuration := identityexperiment.Configuration{
		Task: string(task), SuitePath: "identity-shadow", SuiteSHA256: configSHA256,
		ProviderName: config.Provider.Kind, AdapterKind: config.Provider.Kind,
		Endpoint: config.Provider.Endpoint, RequestedModel: config.Model, ExpectedModel: config.ExpectedModel,
		ServiceTier: cloneStringPointer(config.ServiceTier), AuthScheme: config.Provider.AuthScheme,
		CredentialStore: config.Provider.Credential.Store, CredentialName: config.Provider.Credential.Name,
		CredentialKey: "system_one_api_key", MaxStateBytes: config.Limits.MaxStateBytes,
		MaxRequestBytes: config.Limits.MaxRequestBytes, TimeoutMS: config.Timeout.Milliseconds(),
		MaxCalls: config.Budget.MaxCalls, Provider: config.Provider.Kind,
	}
	if config.Prices != nil {
		input, output := config.Prices.InputPerMillion, config.Prices.OutputPerMillion
		configuration.InputPricePerMillion = &input
		configuration.OutputPricePerMillion = &output
	}
	return configuration
}

func setTaskReceipts(receipt *aggregateReceipt, cohort, prior identityexperiment.Result) {
	receipt.Cohort = taskReceiptFrom(cohort.Receipts)
	receipt.PriorThreads = taskReceiptFrom(prior.Receipts)
	receipt.ExpectedPairs = receipt.Cohort.ExpectedPairs + receipt.PriorThreads.ExpectedPairs
	receipt.AttemptedPairs = receipt.Cohort.AttemptedPairs + receipt.PriorThreads.AttemptedPairs
	receipt.ProviderCalls = receipt.Cohort.ProviderCalls + receipt.PriorThreads.ProviderCalls
	receipt.CompletedPairs = receipt.Cohort.CompletedPairs + receipt.PriorThreads.CompletedPairs
	receipt.AbstainedPairs = receipt.Cohort.AbstainedPairs + receipt.PriorThreads.AbstainedPairs
	receipt.FailedPairs = receipt.Cohort.FailedPairs + receipt.PriorThreads.FailedPairs
	receipt.MissingPairs = receipt.Cohort.MissingPairs + receipt.PriorThreads.MissingPairs
	receipt.KnownInputTokens = receipt.Cohort.KnownInputTokens + receipt.PriorThreads.KnownInputTokens
	receipt.KnownOutputTokens = receipt.Cohort.KnownOutputTokens + receipt.PriorThreads.KnownOutputTokens
	receipt.UnknownCostCalls = receipt.Cohort.UnknownCostCalls + receipt.PriorThreads.UnknownCostCalls
	if receipt.Cohort.EstimatedCostUSD != nil && receipt.PriorThreads.EstimatedCostUSD != nil {
		cost := *receipt.Cohort.EstimatedCostUSD + *receipt.PriorThreads.EstimatedCostUSD
		receipt.EstimatedCostUSD = &cost
	}
}

func taskReceiptFrom(receipts identityexperiment.Receipts) taskReceipt {
	return taskReceipt{
		Status: receipts.Status, ExpectedPairs: receipts.ExpectedPairs,
		AttemptedPairs: receipts.AttemptedPairs, ProviderCalls: receipts.ProviderCalls,
		CompletedPairs: receipts.CompletedPairs, AbstainedPairs: receipts.AbstainedPairs,
		FailedPairs: receipts.FailedPairs, MissingPairs: receipts.MissingPairs,
		KnownInputTokens: receipts.KnownInputTokens, KnownOutputTokens: receipts.KnownOutputTokens,
		EstimatedCostUSD: cloneFloatPointer(receipts.EstimatedCostUSD), UnknownCostCalls: receipts.UnknownCostCalls,
	}
}

func taskFailureCode(result identityexperiment.Result) string {
	if result.Err() != nil {
		return "invalid_input"
	}
	if result.Receipts.RunErrorCode != "" {
		return normalizeStatus(result.Receipts.RunErrorCode)
	}
	for _, preflight := range result.Receipts.PreflightErrors {
		if preflight.ErrorCode != "" {
			return normalizeStatus(preflight.ErrorCode)
		}
	}
	for _, attempt := range result.Receipts.Attempts {
		if attempt.ErrorCode != "" {
			return normalizeStatus(attempt.ErrorCode)
		}
	}
	if result.Receipts.Status == "error" {
		return "malformed_response"
	}
	return ""
}

func statusForContext(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled" //nolint:misspell // Stable diagnostic category is contract-defined.
	}
	return "invalid_input"
}

func normalizeStatus(code string) string {
	switch code {
	case "invalid_config", "invalid_input", "budget_exceeded", "auth", "timeout", "cancelled", "malformed_response", "artifact_failure", "skipped_no_snapshot", "context_limit", "context_unverified", "rate_limit", "upstream", "transport", "response_too_large", "truncated": //nolint:misspell // Stable diagnostic category is contract-defined.
		return code
	case "":
		return "invalid_input"
	default:
		return "inference_error"
	}
}

func nonnegativeMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}
