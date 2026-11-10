package benchmarkcmd

import (
	"context"
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
	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
)

const (
	identityInferenceSchemaVersion  = identityexperiment.SchemaVersion
	defaultIdentityInferenceTimeout = 30 * time.Second
	minimumIdentityInferenceTimeout = time.Second
	maximumIdentityInferenceTimeout = 120 * time.Second
	maximumIdentityInferenceCalls   = 10_000
)

type identityInferenceFlags struct {
	model                 string
	expectedModel         string
	endpoint              string
	credentialStore       string
	credentialName        string
	credentialKey         string
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

type identityInferencePlan = identityexperiment.Plan
type identityInferenceUsage = identityexperiment.Usage
type identityInferenceConfiguration = identityexperiment.Configuration
type identityInferenceReceipts = identityexperiment.Receipts
type identityInferenceManifest = identityexperiment.Manifest
type identityInferenceSummary = identityexperiment.Summary
type identityInferenceReceipt = identityexperiment.Receipt
type identityInferenceDecider = identityexperiment.Decider

// identityInferenceBackend is a command-local compatibility adapter for the
// provider leaves and existing command tests. The shared runner receives its
// exported identityexperiment.Backend projection through shared().
type identityInferenceBackend struct {
	prepare       func(reportidentityinference.Projection) (identityInferencePlan, error)
	newClient     func(string) (identityInferenceDecider, error)
	errorCode     func(error) string
	estimateCost  func(*identityInferenceUsage, *identityInferenceConfiguration) *float64
	provider      string
	contextStatus string
}

func (backend identityInferenceBackend) shared() identityexperiment.Backend {
	return identityexperiment.Backend{
		Prepare: backend.prepare, NewClient: backend.newClient, ErrorCode: backend.errorCode,
		EstimateCost: backend.estimateCost, Provider: backend.provider, ContextStatus: backend.contextStatus,
	}
}

func identityInferenceDependenciesForSystemOne(dependencies inferSystemOneDependencies) identityInferenceDependencies {
	return identityInferenceDependencies{
		resolveAPIKey: dependencies.resolveAPIKey, now: dependencies.now, rename: dependencies.rename,
		lstat: dependencies.lstat, mkdir: dependencies.mkdir, chmod: dependencies.chmod,
	}
}

type identityInferenceDependencies struct {
	resolveAPIKey func(context.Context) (string, error)
	now           func() time.Time
	rename        func(string, string) error
	lstat         func(string) (os.FileInfo, error)
	mkdir         func(string, os.FileMode) error
	chmod         func(string, os.FileMode) error
}

func runIdentityInference(ctx context.Context, opts *root.Options, commandName, fixturePath string, flags identityInferenceFlags, dependencies identityInferenceDependencies, backend identityInferenceBackend) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.now == nil || dependencies.resolveAPIKey == nil || dependencies.rename == nil || backend.prepare == nil || backend.newClient == nil || backend.errorCode == nil || backend.estimateCost == nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": command dependencies are unavailable"))
	}
	inputBytes, err := readReplayFixture(fixturePath)
	if err != nil {
		return exitcode.With(exitcode.Failure, fmt.Errorf(commandName+": read fixture: %w", err))
	}
	configuration := identityInferenceConfigurationFor(flags, backend)
	prepared, err := identityexperiment.Prepare(inputBytes, configuration, backend.shared())
	if err != nil {
		return exitcode.Usage(errors.New(commandName + ": invalid fixture"))
	}

	publisher, err := identityexperiment.ReserveOutput(flags.outputDir, identityexperiment.FileDependencies{
		Lstat: dependencies.lstat, Mkdir: dependencies.mkdir, Chmod: dependencies.chmod, Rename: dependencies.rename,
	})
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitcode.Usage(errors.New(commandName + ": results directory must be new"))
		}
		if identityexperiment.IsOutputInspectionError(err) {
			return exitcode.With(exitcode.Failure, errors.New(commandName+": could not inspect output directory"))
		}
		return exitcode.With(exitcode.Failure, errors.New(commandName+": could not create output directory"))
	}

	result := identityexperiment.Execute(ctx, prepared, identityexperiment.Dependencies{
		ResolveAPIKey: dependencies.resolveAPIKey,
		Now:           dependencies.now,
	}, backend.shared())
	if result.Err() != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": assemble retained replay"))
	}
	if _, err := publisher.Publish(result); err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": output publication incomplete; manifest is not trustworthy"))
	}
	if err := writeIdentityInferenceSummary(opts.Stdout, flags.outputDir, result.Receipts); err != nil {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": write summary"))
	}
	if len(result.Receipts.PreflightErrors) > 0 {
		return exitcode.Usage(errors.New(commandName + ": preflight failed; retained originals were written"))
	}
	if result.Receipts.FailedPairs > 0 {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": request failed; retained outputs were written"))
	}
	if result.Receipts.RunErrorCode != "" {
		return exitcode.With(exitcode.Failure, errors.New(commandName+": run was canceled; retained outputs were written"))
	}
	return nil
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
		ServiceTier: tier, AuthScheme: authScheme,
		CredentialStore: flags.credentialStore, CredentialName: flags.credentialName, CredentialKey: flags.credentialKey,
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

func validIdentityInferenceEndpoint(value string) bool {
	if value == "" || strings.Contains(value, "#") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" && parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == ""
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
	return identityexperiment.EstimateCost(usage, configuration)
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
	return identityexperiment.SHA256(data)
}

func cloneFloat64Map(source map[string]float64) map[string]float64 {
	clone := make(map[string]float64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
