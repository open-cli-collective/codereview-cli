package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type inferOpenAIDecisionsFakeOutcome struct {
	choice        string
	err           error
	usage         *openaidecisions.Usage
	missingUsage  bool
	returnedModel string
	requestSHA256 string
}

type inferOpenAIDecisionsFakeClient struct {
	t        *testing.T
	options  openaidecisions.Options
	outcomes []inferOpenAIDecisionsFakeOutcome
	requests []openaidecisions.Request
	decides  int
}

func (client *inferOpenAIDecisionsFakeClient) Decide(_ context.Context, request openaidecisions.Request) (openaidecisions.Receipt, error) {
	client.t.Helper()
	client.requests = append(client.requests, request)
	client.decides++
	plan, err := openaidecisions.Prepare(request, client.options.Limits)
	if err != nil {
		client.t.Fatalf("openaidecisions.Prepare for fake call: %v", err)
	}
	index := len(client.requests) - 1
	outcome := inferOpenAIDecisionsFakeOutcome{choice: "a"}
	if index < len(client.outcomes) {
		outcome = client.outcomes[index]
	}
	expectedModel := request.ExpectedModel
	if expectedModel == "" {
		expectedModel = request.Model
	}
	receipt := openaidecisions.Receipt{
		RequestedModel: request.Model,
		ReturnedModel:  expectedModel,
		RequestSHA256:  plan.RequestSHA256,
		ResponseSHA256: sha256Hex([]byte(fmt.Sprintf("synthetic-openai-response-%d", index+1))),
		RequestID:      fmt.Sprintf("openai-request-%d", index+1),
		Attempted:      true,
		HTTPStatus:     http.StatusOK,
		Duration:       18 * time.Millisecond,
		Usage:          outcome.usage,
	}
	if outcome.requestSHA256 != "" {
		receipt.RequestSHA256 = outcome.requestSHA256
	}
	if outcome.returnedModel != "" {
		receipt.ReturnedModel = outcome.returnedModel
	}
	if !outcome.missingUsage && outcome.usage == nil {
		cached, cacheWrite := int64(0), int64(0)
		receipt.Usage = &openaidecisions.Usage{InputTokens: 20 + int64(index), OutputTokens: 2, CachedInputTokens: &cached, CacheWriteTokens: &cacheWrite}
	}
	if outcome.err == nil {
		choice := outcome.choice
		if choice == "" {
			choice = "a"
		}
		receipt.Answer = &openaidecisions.Answer{
			Choice: choice, Confidence: 0.7,
			Probabilities: map[string]float64{"a": 0.7, "b": 0.2, "c": 0.1},
		}
	}
	return receipt, outcome.err
}

type inferOpenAIDecisionsTestState struct {
	client      *inferOpenAIDecisionsFakeClient
	keyLookups  int
	keyNames    []string
	clientOpens int
	options     openaidecisions.Options
	apiKey      string
	keyPresent  bool
	published   []string
}

func inferOpenAIDecisionsTestDependencies(t *testing.T, outcomes []inferOpenAIDecisionsFakeOutcome, apiKey string, keyPresent bool) (inferOpenAIDecisionsDependencies, *inferOpenAIDecisionsTestState) {
	t.Helper()
	state := &inferOpenAIDecisionsTestState{apiKey: apiKey, keyPresent: keyPresent}
	dependencies := inferOpenAIDecisionsDependencies{
		lookupAPIKey: func(name string) (string, bool) {
			state.keyLookups++
			state.keyNames = append(state.keyNames, name)
			return state.apiKey, state.keyPresent
		},
		newClient: func(options openaidecisions.Options) (openAIDecisionsDecider, error) {
			state.clientOpens++
			state.options = options
			state.client = &inferOpenAIDecisionsFakeClient{t: t, options: options, outcomes: outcomes}
			return state.client, nil
		},
		now: func() time.Time { return time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC) },
		rename: func(oldPath, newPath string) error {
			state.published = append(state.published, filepath.Base(newPath))
			return os.Rename(oldPath, newPath)
		},
	}
	return dependencies, state
}

func newInferOpenAIDecisionsTestCommand(t *testing.T, dependencies inferOpenAIDecisionsDependencies) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	opts := &root.Options{Stdin: strings.NewReader("")}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	opts.Stdout = out
	opts.Stderr = errOut
	cmd, rootOpts := root.NewCommandWithOptions(opts)
	root.RegisterAll(cmd, rootOpts, func(rootCmd *cobra.Command, rootOptions *root.Options) {
		benchmark := &cobra.Command{Use: "benchmark", Short: "Benchmark test tree"}
		benchmark.AddCommand(newInferOpenAIDecisionsCommandWithDependencies(rootOptions, dependencies))
		rootCmd.AddCommand(benchmark)
	})
	return cmd, out, errOut
}

func inferOpenAIDecisionsRunArgs(fixturePath, outputDir string, maxCalls int) []string {
	return []string{"benchmark", "infer-openai-decisions", fixturePath, "--model", "provider/model-test", "--max-calls", fmt.Sprint(maxCalls), "--output-dir", outputDir}
}

func runInferOpenAIDecisionsTestCase(t *testing.T, input reportidentity.Input, extraArgs []string, outcomes []inferOpenAIDecisionsFakeOutcome, apiKey string, keyPresent bool) (*inferOpenAIDecisionsTestState, *identityInferenceSummary, *identityInferenceReceipts, string, error) {
	t.Helper()
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferOpenAIDecisionsTestDependencies(t, outcomes, apiKey, keyPresent)
	cmd, stdout, _ := newInferOpenAIDecisionsTestCommand(t, dependencies)
	args := inferOpenAIDecisionsRunArgs(fixturePath, outputDir, 100)
	args = append(args, extraArgs...)
	err := root.Execute(cmd, args)
	var summary identityInferenceSummary
	if stdout.Len() > 0 {
		if jsonErr := json.Unmarshal(stdout.Bytes(), &summary); jsonErr != nil {
			t.Fatalf("decode summary: %v; stdout=%s", jsonErr, stdout.String())
		}
	}
	var receipts identityInferenceReceipts
	if _, statErr := os.Stat(filepath.Join(outputDir, "receipts.json")); statErr == nil {
		if jsonErr := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); jsonErr != nil {
			t.Fatalf("decode receipts: %v", jsonErr)
		}
	}
	return state, &summary, &receipts, stdout.String(), err
}

func TestInferOpenAIDecisionsCommandDefaultsAndFixedFlags(t *testing.T) {
	dependencies, _ := inferOpenAIDecisionsTestDependencies(t, nil, "", false)
	cmd, _, _ := newInferOpenAIDecisionsTestCommand(t, dependencies)
	command, _, err := cmd.Find([]string{"benchmark", "infer-openai-decisions"})
	if err != nil {
		t.Fatalf("Find command: %v", err)
	}
	defaults := map[string]string{
		"endpoint":          defaultOpenAIDecisionsEndpoint,
		"api-key-env":       defaultOpenAIKeyEnv,
		"timeout":           defaultIdentityInferenceTimeout.String(),
		"max-state-bytes":   "6144",
		"max-request-bytes": "8192",
		"model":             "",
	}
	for name, want := range defaults {
		flag := command.Flags().Lookup(name)
		if flag == nil || flag.DefValue != want {
			t.Errorf("flag %q default = %#v, want %q", name, flag, want)
		}
	}
	for _, name := range []string{"auth-scheme", "service-tier", "effort"} {
		if command.Flags().Lookup(name) != nil {
			t.Errorf("unsupported flag %q is exposed", name)
		}
	}
}

func TestInferOpenAIDecisionsZeroPairsNeedNoKeyOrClient(t *testing.T) {
	input := inferSystemOneCohortInput(1)
	state, summary, receipts, _, err := runInferOpenAIDecisionsTestCase(t, input, nil, nil, "", false)
	if err != nil || state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
		t.Fatalf("zero-pair result: err=%v state=%#v", err, state)
	}
	if summary.Status != "complete" || summary.ExpectedPairs != 0 || summary.ProviderCalls != 0 || summary.UnknownCostCalls != 0 || summary.EstimatedCostUSD == nil || *summary.EstimatedCostUSD != 0 {
		t.Fatalf("zero-pair summary = %#v", summary)
	}
	if receipts.Attempts == nil || len(receipts.Attempts) != 0 || receipts.Configuration.Provider != "openai_decisions" {
		t.Fatalf("zero-pair receipts = %#v", receipts)
	}
}

func TestInferOpenAIDecisionsPreflightPrecedesKeyLookupAndClientCreation(t *testing.T) {
	input := inferSystemOneCohortInput(3)
	state, summary, receipts, _, err := runInferOpenAIDecisionsTestCase(t, input, []string{"--max-calls", "2"}, nil, "synthetic-key", true)
	if err == nil || state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
		t.Fatalf("preflight result: err=%v state=%#v", err, state)
	}
	if summary.Status != "error" || summary.ExpectedPairs != 3 || summary.ProviderCalls != 0 || receipts.RunErrorCode != "" || len(receipts.PreflightErrors) == 0 {
		t.Fatalf("preflight outputs = summary:%#v receipts:%#v", summary, receipts)
	}
	var retained reportidentity.Input
	if err := json.Unmarshal(readInferSystemOneOutput(t, summary.OutputDir, "fixture.json"), &retained); err != nil || len(retained.Findings) != len(input.Findings) {
		t.Fatalf("preflight did not retain original fixture: findings=%d err=%v", len(retained.Findings), err)
	}
}

func TestInferOpenAIDecisionsProjectsSharedPairsAndAccountsForCacheCost(t *testing.T) {
	input := inferSystemOnePriorInput()
	zero := int64(0)
	nonzero := int64(1)
	unknownUsage := &openaidecisions.Usage{InputTokens: 100, OutputTokens: 20}
	outcomes := []inferOpenAIDecisionsFakeOutcome{
		{usage: &openaidecisions.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &zero, CacheWriteTokens: &zero}},
		{usage: &openaidecisions.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &nonzero, CacheWriteTokens: &zero}},
		{usage: unknownUsage},
		{usage: &openaidecisions.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &zero, CacheWriteTokens: &zero}},
	}
	extra := []string{
		"--model", "provider/model-alias",
		"--expected-model", "provider/model-resolved",
		"--endpoint", "https://api.example.test/custom/v3/decisions",
		"--api-key-env", "ALT_OPENAI_KEY",
		"--timeout", "10s",
		"--max-state-bytes", "4096",
		"--max-request-bytes", "7168",
		"--input-price-per-million", "2",
		"--output-price-per-million", "5",
	}
	state, summary, receipts, stdout, err := runInferOpenAIDecisionsTestCase(t, input, extra, outcomes, "synthetic-key", true)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if state.keyLookups != 1 || !reflect.DeepEqual(state.keyNames, []string{"ALT_OPENAI_KEY"}) || state.clientOpens != 1 || state.options.APIKey != "synthetic-key" {
		t.Fatalf("credential/client use = %#v", state)
	}
	if state.options.Endpoint != "https://api.example.test/custom/v3/decisions" || state.options.Timeout != 10*time.Second || state.options.Limits != (openaidecisions.Limits{MaxInputBytes: 4096, MaxRequestBytes: 7168}) {
		t.Fatalf("OpenAI client options = %#v", state.options)
	}
	if state.client == nil || len(state.client.requests) != 4 {
		t.Fatalf("captured OpenAI requests = %#v", state.client)
	}
	baseResult, err := reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("Replay fixture: %v", err)
	}
	findings := map[string]review.Finding{}
	for _, finding := range input.Findings {
		findings[string(finding.ID)] = finding
	}
	threads := map[string]reportidentity.Thread{}
	for _, thread := range input.Threads {
		threads[thread.ID] = thread
	}
	for index, pair := range baseResult.Pairs {
		projection, projectErr := reportidentityinference.Project(reportidentityinference.Task(input.Task), inferenceFindingRecord(findings[pair.LeftID]), inferenceThreadRecord(threads[pair.RightID]))
		if projectErr != nil {
			t.Fatalf("Project pair %d: %v", index, projectErr)
		}
		request := state.client.requests[index]
		if request.Input != string(projection.State) || request.Name != projection.QuestionName || request.Instructions != projection.Question.Instructions {
			t.Fatalf("request %d does not use shared projection: %#v", index, request)
		}
		if len(request.Choices) != 3 || request.Choices[0].Value != "a" || request.Choices[1].Value != "b" || request.Choices[2].Value != "c" {
			t.Fatalf("choice order = %#v", request.Choices)
		}
		for choiceIndex, label := range []string{"a", "b", "c"} {
			if request.Choices[choiceIndex].Description != projection.Question.Criteria[label] {
				t.Errorf("request %d criterion %q differs from shared projection", index, label)
			}
		}
		if request.Model != "provider/model-alias" || request.ExpectedModel != "provider/model-resolved" {
			t.Errorf("request %d model policy = %q/%q", index, request.Model, request.ExpectedModel)
		}
	}
	if summary.Status != "complete" || summary.ExpectedPairs != 4 || summary.AttemptedPairs != 4 || summary.ProviderCalls != 4 || summary.CompletedPairs != 4 || summary.MissingPairs != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.Configuration.Provider != "openai_decisions" || summary.Configuration.Endpoint != state.options.Endpoint || summary.Configuration.AuthScheme != "Bearer" || summary.Configuration.APIKeyEnv != "ALT_OPENAI_KEY" || summary.Configuration.ExpectedModel != "provider/model-resolved" || summary.Configuration.ContextStatus != "" {
		t.Fatalf("OpenAI config = %#v", summary.Configuration)
	}
	if summary.KnownInputTokens != 400 || summary.KnownOutputTokens != 80 || summary.UnknownCostCalls != 2 || summary.EstimatedCostUSD == nil || math.Abs(*summary.EstimatedCostUSD-0.0006) > 1e-12 {
		t.Fatalf("usage/cost rollup = %#v", summary)
	}
	if summary.AttemptedPairs != 4 || receipts.Attempts[1].EstimatedCostUSD != nil || receipts.Attempts[2].EstimatedCostUSD != nil || receipts.Attempts[0].EstimatedCostUSD == nil {
		t.Fatalf("cache cost policy = summary:%#v attempts:%#v", summary, receipts.Attempts)
	}
	if !strings.Contains(stdout, summary.OutputDir) || strings.Contains(stdout, "synthetic-key") {
		t.Fatalf("summary output exposed a key or lost paths: %s", stdout)
	}
	for _, attempt := range receipts.Attempts {
		if attempt.Usage == nil || strings.Contains(string(mustJSON(t, attempt)), "synthetic-key") {
			t.Fatalf("attempt usage/key = %#v", attempt)
		}
	}
	var retained reportidentity.Input
	if err := json.Unmarshal(readInferSystemOneOutput(t, summary.OutputDir, "fixture.json"), &retained); err != nil || len(retained.Threads) != 3 || retained.Threads[1].Eligible || len(retained.Decisions) != 4 {
		t.Fatalf("retained fixture lost an ineligible thread or decision: %#v err=%v", retained, err)
	}
	if strings.Contains(string(readInferSystemOneOutput(t, summary.OutputDir, "receipts.json")), "account_id") {
		t.Fatal("shared receipt schema contains provider-specific account identity")
	}
	wantPublished := []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"}
	if !reflect.DeepEqual(state.published, wantPublished) {
		t.Fatalf("artifact order = %v, want %v", state.published, wantPublished)
	}
}

func TestInferOpenAIDecisionsRejectsUntrustedUsageExtensions(t *testing.T) {
	negative := int64(-1)
	tooManyCached := int64(21)
	tests := []struct {
		name  string
		usage *openaidecisions.Usage
	}{
		{name: "negative cache count", usage: &openaidecisions.Usage{InputTokens: 20, OutputTokens: 2, CachedInputTokens: &negative}},
		{name: "cache count above input", usage: &openaidecisions.Usage{InputTokens: 20, OutputTokens: 2, CachedInputTokens: &tooManyCached}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := inferSystemOneCohortInput(2)
			state, summary, receipts, _, err := runInferOpenAIDecisionsTestCase(t, input, nil, []inferOpenAIDecisionsFakeOutcome{{usage: test.usage}}, "key", true)
			if err == nil || state.client == nil || len(receipts.Attempts) != 1 || receipts.Attempts[0].ErrorCode != "malformed_response" || receipts.Attempts[0].Usage != nil || summary.FailedPairs != 1 {
				t.Fatalf("invalid cache metadata result: err=%v summary=%#v attempt=%#v", err, summary, receipts.Attempts)
			}
		})
	}
}

func TestMakeIdentityInferenceAttemptRejectsNegativeTotalMetadata(t *testing.T) {
	negative := int64(-1)
	configuration := &identityInferenceConfiguration{RequestedModel: "provider/model", ExpectedModel: "provider/model"}
	planHash := "planned-hash"
	pair := identityInferencePair{plan: identityInferencePlan{RequestSHA256: planHash}}
	receipt := identityInferenceReceipt{
		RequestedModel: "provider/model", ReturnedModel: "provider/model", RequestSHA256: planHash,
		Attempted: true,
		Answer: &reportidentityinference.ChoiceAnswer{
			Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.9, "b": 0.08, "c": 0.02},
		},
		Usage: &identityInferenceUsage{InputTokens: 20, OutputTokens: 2, TotalTokens: &negative},
	}
	backend := identityInferenceBackend{
		errorCode:    func(error) string { return "transport" },
		estimateCost: func(_ *identityInferenceUsage, _ *identityInferenceConfiguration) *float64 { return nil },
	}
	attempt, _, code := makeIdentityInferenceAttempt(pair, receipt, nil, time.Now(), nil, configuration, backend)
	if code != "malformed_response" || attempt.Usage != nil {
		t.Fatalf("negative total metadata was trusted: attempt=%#v code=%q", attempt, code)
	}
}

func TestInferOpenAIDecisionsFailsClosedOnProviderReceiptMismatches(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	tests := []struct {
		name    string
		outcome inferOpenAIDecisionsFakeOutcome
	}{
		{name: "wrong actual request hash", outcome: inferOpenAIDecisionsFakeOutcome{requestSHA256: "different-hash"}},
		{name: "wrong returned model", outcome: inferOpenAIDecisionsFakeOutcome{returnedModel: "provider/other-model"}},
		{name: "invalid choice", outcome: inferOpenAIDecisionsFakeOutcome{choice: "x"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, summary, receipts, _, err := runInferOpenAIDecisionsTestCase(t, input, []string{"--expected-model", "provider/model-resolved"}, []inferOpenAIDecisionsFakeOutcome{test.outcome}, "key", true)
			if err == nil || state.client == nil || len(receipts.Attempts) != 1 || receipts.Attempts[0].ErrorCode != "malformed_response" || receipts.Attempts[0].Answer != nil || summary.FailedPairs != 1 {
				t.Fatalf("fail-closed result: err=%v summary=%#v attempts=%#v", err, summary, receipts.Attempts)
			}
		})
	}
}

type openAIRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn openAIRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestInferOpenAIDecisionsRefusalPreservesUsageAndFailsStop(t *testing.T) {
	input := inferSystemOneCohortInput(3)
	var calls int
	var optionsSeen openaidecisions.Options
	dependencies, state := inferOpenAIDecisionsTestDependencies(t, nil, "synthetic-key", true)
	dependencies.newClient = func(options openaidecisions.Options) (openAIDecisionsDecider, error) {
		state.clientOpens++
		state.options = options
		optionsSeen = options
		return openaidecisions.New(openaidecisions.Options{
			Endpoint: options.Endpoint, APIKey: options.APIKey, Timeout: options.Timeout, Limits: options.Limits,
			HTTPClient: &http.Client{Transport: openAIRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Header.Get("Authorization") != "Bearer synthetic-key" {
					t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
				}
				body := []byte(`{"model":"provider/model-test","answers":[{"type":"refusal","name":"identity"}],"usage":{"input_tokens":10,"output_tokens":0}}`)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
			})},
		})
	}
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	cmd, stdout, _ := newInferOpenAIDecisionsTestCommand(t, dependencies)
	args := inferOpenAIDecisionsRunArgs(fixturePath, outputDir, 100)
	args = append(args, "--input-price-per-million", "1", "--output-price-per-million", "1")
	err := root.Execute(cmd, args)
	var summary identityInferenceSummary
	if stdout.Len() > 0 {
		if jsonErr := json.Unmarshal(stdout.Bytes(), &summary); jsonErr != nil {
			t.Fatalf("decode summary: %v", jsonErr)
		}
	}
	receiptsBytes := readInferSystemOneOutput(t, outputDir, "receipts.json")
	var receipts identityInferenceReceipts
	if jsonErr := json.Unmarshal(receiptsBytes, &receipts); jsonErr != nil {
		t.Fatalf("decode receipts: %v", jsonErr)
	}
	if err == nil || calls != 1 || optionsSeen.APIKey != "synthetic-key" || summary.FailedPairs != 1 || summary.MissingPairs != 2 || summary.ProviderCalls != 1 {
		t.Fatalf("refusal fail-stop: err=%v calls=%d summary=%#v", err, calls, summary)
	}
	if len(receipts.Attempts) != 1 || receipts.Attempts[0].ErrorCode != "refused" || receipts.Attempts[0].Answer != nil || receipts.Attempts[0].Usage == nil || receipts.Attempts[0].Usage.InputTokens != 10 || receipts.Attempts[0].EstimatedCostUSD != nil || summary.UnknownCostCalls != 1 || summary.KnownInputTokens != 10 {
		t.Fatalf("refusal receipt did not retain validated metadata/unknown cache cost: %#v", receipts.Attempts)
	}
	if strings.Contains(string(receiptsBytes), "synthetic-key") || strings.Contains(string(receiptsBytes), "provider prose") {
		t.Fatal("refusal artifacts exposed key or provider prose")
	}
}

func TestInferOpenAIDecisionsPartialWriteDoesNotPublishManifest(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferOpenAIDecisionsTestDependencies(t, nil, "key", true)
	originalRename := dependencies.rename
	dependencies.rename = func(oldPath, newPath string) error {
		if filepath.Base(newPath) == "replay.json" {
			return errors.New("synthetic rename failure")
		}
		return originalRename(oldPath, newPath)
	}
	cmd, _, _ := newInferOpenAIDecisionsTestCommand(t, dependencies)
	if err := root.Execute(cmd, inferOpenAIDecisionsRunArgs(fixturePath, outputDir, 1)); err == nil || state.client == nil || state.client.decides != 1 {
		t.Fatalf("write error was not reported after one request: err=%v state=%#v", err, state)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "fixture.json")); err != nil {
		t.Fatalf("fixture was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists after partial publication or stat err=%v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}
