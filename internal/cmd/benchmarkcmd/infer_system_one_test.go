package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

type inferSystemOneFakeOutcome struct {
	choice        string
	err           error
	notAttempted  bool
	missingUsage  bool
	usage         *systemone.Usage
	returnedModel string
	requestSHA256 string
	answers       map[string]systemone.ChoiceAnswer
}

type inferSystemOneFakeClient struct {
	t        *testing.T
	limits   systemone.Limits
	outcomes []inferSystemOneFakeOutcome
	requests []systemone.Request
	decides  int
}

func (client *inferSystemOneFakeClient) Decide(_ context.Context, request systemone.Request) (systemone.Receipt, error) {
	client.t.Helper()
	client.requests = append(client.requests, request)
	client.decides++
	plan, err := systemone.Prepare(request, client.limits)
	if err != nil {
		client.t.Fatalf("systemone.Prepare for fake call: %v", err)
	}
	index := len(client.requests) - 1
	outcome := inferSystemOneFakeOutcome{choice: "a"}
	if index < len(client.outcomes) {
		outcome = client.outcomes[index]
	}
	expectedModel := request.ExpectedModel
	if expectedModel == "" {
		expectedModel = request.Model
	}
	receipt := systemone.Receipt{
		RequestedModel: request.Model,
		ReturnedModel:  expectedModel,
		RequestSHA256:  plan.RequestSHA256,
		ResponseSHA256: sha256Hex([]byte(fmt.Sprintf("synthetic-response-%d", index+1))),
		RequestID:      fmt.Sprintf("request-%d", index+1),
		Duration:       25 * time.Millisecond,
		Attempted:      !outcome.notAttempted,
		HTTPStatus:     http.StatusOK,
		Usage:          outcome.usage,
	}
	if outcome.requestSHA256 != "" {
		receipt.RequestSHA256 = outcome.requestSHA256
	}
	if outcome.returnedModel != "" {
		receipt.ReturnedModel = outcome.returnedModel
	}
	if !outcome.missingUsage && outcome.usage == nil {
		receipt.Usage = &systemone.Usage{InputTokens: int64(20 + index), OutputTokens: 2}
	}
	if outcome.err == nil {
		if outcome.answers != nil {
			receipt.Answers = outcome.answers
		} else {
			choice := outcome.choice
			if choice == "" {
				choice = "a"
			}
			receipt.Answers = map[string]systemone.ChoiceAnswer{
				"identity": {Choice: choice, Confidence: 0.7, Probabilities: map[string]float64{"a": 0.7, "b": 0.2, "c": 0.1}},
			}
		}
	}
	if outcome.notAttempted {
		receipt.HTTPStatus = 0
		receipt.ResponseSHA256 = ""
	}
	return receipt, outcome.err
}

type cancelOnCallDecider struct {
	limits systemone.Limits
	cancel context.CancelFunc
	calls  int
}

func (client *cancelOnCallDecider) Decide(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
	client.calls++
	plan, err := systemone.Prepare(request, client.limits)
	if err != nil {
		return systemone.Receipt{}, err
	}
	client.cancel()
	return systemone.Receipt{
		RequestedModel: request.Model,
		ReturnedModel:  request.ExpectedModel,
		RequestSHA256:  plan.RequestSHA256,
		Usage:          &systemone.Usage{InputTokens: 20, OutputTokens: 2},
		Attempted:      true,
		HTTPStatus:     http.StatusOK,
	}, ctx.Err()
}

type inferSystemOneTestState struct {
	client      *inferSystemOneFakeClient
	keyLookups  int
	keyNames    []string
	clientOpens int
	options     systemone.Options
	apiKey      string
	keyPresent  bool
	published   []string
}

func inferSystemOneTestDependencies(t *testing.T, outcomes []inferSystemOneFakeOutcome, apiKey string, keyPresent bool) (inferSystemOneDependencies, *inferSystemOneTestState) {
	t.Helper()
	state := &inferSystemOneTestState{apiKey: apiKey, keyPresent: keyPresent}
	dependencies := inferSystemOneDependencies{
		lookupAPIKey: func(name string) (string, bool) {
			state.keyLookups++
			state.keyNames = append(state.keyNames, name)
			return state.apiKey, state.keyPresent
		},
		newClient: func(options systemone.Options) (systemOneDecider, error) {
			state.clientOpens++
			state.options = options
			state.client = &inferSystemOneFakeClient{t: t, outcomes: outcomes, limits: options.Limits}
			return state.client, nil
		},
		now: func() time.Time { return time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC) },
		rename: func(oldPath, newPath string) error {
			state.published = append(state.published, filepath.Base(newPath))
			return os.Rename(oldPath, newPath)
		},
	}
	return dependencies, state
}

func newInferSystemOneTestCommand(t *testing.T, opts *root.Options, dependencies inferSystemOneDependencies) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	if opts == nil {
		opts = &root.Options{}
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	opts.Stdout = out
	opts.Stderr = errOut
	previousRuntime := identityProviderRuntimeFor
	identityProviderRuntimeFor = func(provider identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
		if provider.Kind != "system_one" {
			return identityProviderRuntime{}, fmt.Errorf("unexpected adapter kind %q", provider.Kind)
		}
		return identityProviderRuntime{
			dependencies: identityInferenceDependenciesForSystemOne(dependencies),
			backend:      inferSystemOneBackend(flags, dependencies),
		}, nil
	}
	t.Cleanup(func() { identityProviderRuntimeFor = previousRuntime })
	cmd, rootOpts := root.NewCommandWithOptions(opts)
	Register(cmd, rootOpts)
	return cmd, out, errOut
}

func executeLegacyInferSystemOne(t *testing.T, cmd *cobra.Command, args []string) error {
	t.Helper()
	translated, err := translateLegacyInferSystemOneArgs(t, args)
	if err != nil {
		return err
	}
	return root.Execute(cmd, translated)
}

func translateLegacyInferSystemOneArgs(t *testing.T, args []string) ([]string, error) {
	t.Helper()
	if len(args) < 3 || args[0] != "benchmark" || args[1] != "infer-system-one" {
		return nil, fmt.Errorf("unexpected legacy test args: %#v", args)
	}
	fixturePath := args[2]
	endpoint, apiKeyEnv, authScheme, serviceTier := defaultSystemOneEndpoint, defaultSystemOneKeyEnv, "api-key", ""
	forwarded := make([]string, 0, len(args))
	for index := 3; index < len(args); index++ {
		name := args[index]
		if !strings.HasPrefix(name, "--") {
			return nil, fmt.Errorf("unexpected positional legacy test argument %q", name)
		}
		if index+1 >= len(args) {
			return nil, fmt.Errorf("missing value for %s", name)
		}
		value := args[index+1]
		index++
		switch name {
		case "--endpoint":
			endpoint = value
		case "--api-key-env":
			apiKeyEnv = value
		case "--auth-scheme":
			authScheme = value
		case "--service-tier":
			serviceTier = value
		case "--output-dir":
			forwarded = append(forwarded, "--results-dir", value)
		default:
			forwarded = append(forwarded, name, value)
		}
	}
	suitePath := filepath.Join(filepath.Dir(fixturePath), "identity-test-suite.yml")
	suite := fmt.Sprintf("task: report_identity\nfixture: %q\nproviders:\n  test:\n    kind: system_one\n    endpoint: %q\n    api_key_env: %q\n    auth_scheme: %q\n", filepath.Base(fixturePath), endpoint, apiKeyEnv, authScheme)
	if serviceTier != "" {
		suite += fmt.Sprintf("    service_tier: %q\n", serviceTier)
	}
	if err := os.WriteFile(suitePath, []byte(suite), 0o600); err != nil {
		return nil, fmt.Errorf("write test identity suite: %w", err)
	}
	return append([]string{"benchmark", "run", suitePath, "--provider", "test"}, forwarded...), nil
}

func writeInferSystemOneInput(t *testing.T, dir string, input reportidentity.Input) (string, []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll fixture directory: %v", err)
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal fixture: %v", err)
	}
	path := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}
	return path, data
}

func inferSystemOneCohortInput(count int) reportidentity.Input {
	findings := make([]review.Finding, 0, count)
	for index := 0; index < count; index++ {
		finding := inferSystemOneFinding(fmt.Sprintf("f%d", index+1), review.SeverityMajor)
		if index == 0 {
			finding.Anchor = review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 22}
			finding.Anchoring = review.AnchoringFileLevelFallback
		}
		findings = append(findings, finding)
	}
	return reportidentity.Input{SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort, Findings: findings, Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{}}
}

func inferSystemOnePriorInput() reportidentity.Input {
	findingA := inferSystemOneFinding("f1", review.SeverityMajor)
	findingA.Anchoring = review.AnchoringInline
	return reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskPriorThread,
		Findings:      []review.Finding{findingA, inferSystemOneFinding("f2", review.SeverityMinor)},
		Threads: []reportidentity.Thread{
			{ID: "t1", Eligible: true, FilePath: "src/thread-a.go", Body: "Synthetic prior comment A."},
			{ID: "t-excluded", Eligible: false, FilePath: "src/thread-x.go", Body: "Synthetic ineligible comment."},
			{ID: "t2", Eligible: true, FilePath: "src/thread-b.go", Body: "Synthetic prior comment B."},
		},
		Decisions: []reportidentity.Decision{},
	}
}

func inferSystemOneFinding(id string, severity review.Severity) review.Finding {
	return review.Finding{
		ID:       review.FindingID(id),
		Severity: severity,
		FilePath: "src/" + id + ".go",
		Anchor:   review.Anchor{Kind: review.AnchorKindFile},
		Body:     "Synthetic finding text for " + id,
	}
}

func inferSystemOneRunArgs(fixturePath, outputDir string, maxCalls int) []string {
	return []string{"benchmark", "infer-system-one", fixturePath, "--model", "provider/model-test", "--max-calls", fmt.Sprint(maxCalls), "--output-dir", outputDir}
}

func assertInferSystemOneFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}

func readInferSystemOneOutput(t *testing.T, outputDir, name string) []byte {
	t.Helper()
	// #nosec G304 -- outputDir is created by this test under t.TempDir.
	data, err := os.ReadFile(filepath.Join(outputDir, name))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return data
}

func inferSystemOneRunFlags() []string {
	return []string{"--endpoint", "https://custom.example.test/v2/system-one", "--auth-scheme", "bearer", "--api-key-env", "ALT_TOKEN", "--expected-model", "provider/model-resolved", "--service-tier", "batch", "--max-state-bytes", "4096", "--max-request-bytes", "7168"}
}

func runInferSystemOneTestCase(t *testing.T, input reportidentity.Input, args []string, outcomes []inferSystemOneFakeOutcome, apiKey string, keyPresent bool) (*inferSystemOneTestState, *identityInferenceSummary, *identityInferenceReceipts, string, string, error) {
	t.Helper()
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferSystemOneTestDependencies(t, outcomes, apiKey, keyPresent)
	cmd, stdout, stderr := newInferSystemOneTestCommand(t, nil, dependencies)
	commandArgs := inferSystemOneRunArgs(fixturePath, outputDir, 100)
	commandArgs = append(commandArgs, args...)
	err := executeLegacyInferSystemOne(t, cmd, commandArgs)
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
	return state, &summary, &receipts, stdout.String(), stderr.String(), err
}

func TestInferSystemOneCohortPublishesGenericConfigurationCostAndReplay(t *testing.T) {
	input := inferSystemOneCohortInput(3)
	baseDir := t.TempDir()
	fixturePath, inputBytes := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	outcomes := []inferSystemOneFakeOutcome{
		{choice: "a", usage: &systemone.Usage{InputTokens: 100, OutputTokens: 20}},
		{choice: "b", usage: &systemone.Usage{InputTokens: 200, OutputTokens: 40}},
		{choice: "c", usage: &systemone.Usage{InputTokens: 0, OutputTokens: 0}},
	}
	dependencies, state := inferSystemOneTestDependencies(t, outcomes, "synthetic-test-key", true)
	cmd, stdout, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	args := inferSystemOneRunArgs(fixturePath, outputDir, 3)
	args = append(args, inferSystemOneRunFlags()...)
	args = append(args, "--input-price-per-million", "2", "--output-price-per-million", "5")
	if err := executeLegacyInferSystemOne(t, cmd, args); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if state.keyLookups != 1 || len(state.keyNames) != 1 || state.keyNames[0] != "ALT_TOKEN" || state.clientOpens != 1 || state.options.APIKey != "synthetic-test-key" {
		t.Fatalf("credential/client use = %#v", state)
	}
	if want := []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"}; !reflect.DeepEqual(state.published, want) {
		t.Fatalf("artifact publication order = %v, want %v", state.published, want)
	}
	if state.options.Endpoint != "https://custom.example.test/v2/system-one" || state.options.AuthScheme != "Bearer" || state.options.Timeout != defaultSystemOneTimeout || state.options.Limits != (systemone.Limits{MaxStateBytes: 4096, MaxRequestBytes: 7168}) {
		t.Fatalf("System One client options = %#v", state.options)
	}
	if state.client == nil || len(state.client.requests) != 3 || state.client.requests[0].Model != "provider/model-test" || state.client.requests[0].ExpectedModel != "provider/model-resolved" || state.client.requests[0].ServiceTier != "batch" {
		t.Fatalf("captured requests = %#v", state.client)
	}
	var summary identityInferenceSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("decode stdout summary: %v", err)
	}
	if summary.Status != "unresolved" || summary.ExpectedPairs != 3 || summary.AttemptedPairs != 3 || summary.ProviderCalls != 3 || summary.CompletedPairs != 3 || summary.AbstainedPairs != 1 || summary.MissingPairs != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.Configuration.AuthScheme != "Bearer" || summary.Configuration.APIKeyEnv != "ALT_TOKEN" || summary.Configuration.ExpectedModel != "provider/model-resolved" || summary.Configuration.ServiceTier == nil || *summary.Configuration.ServiceTier != "batch" {
		t.Fatalf("summary config = %#v", summary.Configuration)
	}
	if summary.EstimatedCostUSD == nil || math.Abs(*summary.EstimatedCostUSD-0.0009) > 1e-12 || summary.UnknownCostCalls != 0 {
		t.Fatalf("summary cost = %#v unknown=%d", summary.EstimatedCostUSD, summary.UnknownCostCalls)
	}
	if !strings.Contains(stdout.String(), outputDir) || strings.Contains(stdout.String(), "synthetic-test-key") {
		t.Fatalf("summary paths/key = %s", stdout.String())
	}
	assertInferSystemOneFileMode(t, outputDir, 0o700)
	for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
		assertInferSystemOneFileMode(t, filepath.Join(outputDir, name), 0o600)
	}
	fixtureBytes := readInferSystemOneOutput(t, outputDir, "fixture.json")
	replayBytes := readInferSystemOneOutput(t, outputDir, "replay.json")
	receiptsBytes := readInferSystemOneOutput(t, outputDir, "receipts.json")
	manifestBytes := readInferSystemOneOutput(t, outputDir, "manifest.json")
	var manifest identityInferenceManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Files.FixtureSHA256 != sha256Hex(fixtureBytes) || manifest.Files.ReplaySHA256 != sha256Hex(replayBytes) || manifest.Files.ReceiptsSHA256 != sha256Hex(receiptsBytes) {
		t.Fatalf("manifest hashes = %#v", manifest.Files)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(receiptsBytes, &receipts); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if receipts.InputSHA256 != sha256Hex(inputBytes) || receipts.FindingCount != 3 || receipts.ThreadCount != 0 || receipts.KnownInputTokens != 300 || receipts.KnownOutputTokens != 60 {
		t.Fatalf("receipt rollup = %#v", receipts)
	}
	if receipts.Attempts[0].EstimatedCostUSD == nil || math.Abs(*receipts.Attempts[0].EstimatedCostUSD-0.0003) > 1e-12 || receipts.Attempts[1].EstimatedCostUSD == nil || math.Abs(*receipts.Attempts[1].EstimatedCostUSD-0.0006) > 1e-12 {
		t.Fatalf("attempt costs = %#v", receipts.Attempts)
	}
	var generated reportidentity.Input
	if err := json.Unmarshal(fixtureBytes, &generated); err != nil || len(generated.Findings) != len(input.Findings) || len(generated.Decisions) != 3 {
		t.Fatalf("generated fixture = %#v err=%v", generated, err)
	}
	var replay reportidentity.Result
	if err := json.Unmarshal(replayBytes, &replay); err != nil || replay.Status != "unresolved" || len(replay.Pairs) != 3 {
		t.Fatalf("generated replay = %#v err=%v", replay, err)
	}
	if state.client.requests[0].ExpectedModel == "" || strings.Contains(string(state.client.requests[0].State), "synthetic-test-key") {
		t.Fatal("key was included in request or expected model not propagated")
	}
	assertInferSystemOneRetentionAndReplay(t, outputDir, input)
}

func TestInferSystemOnePriorThreadsRetainsIneligibleThreadsAndUnknownCost(t *testing.T) {
	input := inferSystemOnePriorInput()
	outcomes := []inferSystemOneFakeOutcome{{choice: "a"}, {choice: "b"}, {choice: "a"}, {choice: "c"}}
	state, summary, receipts, stdout, _, err := runInferSystemOneTestCase(t, input, nil, outcomes, "key", true)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if summary.ExpectedPairs != 4 || summary.ProviderCalls != 4 || summary.CompletedPairs != 4 || summary.UnknownCostCalls != 4 || summary.EstimatedCostUSD != nil {
		t.Fatalf("summary = %#v", summary)
	}
	if state.client == nil || len(state.client.requests) != 4 || summary.Configuration.ServiceTier != nil {
		t.Fatalf("requests/config = %#v %#v", state.client, summary.Configuration)
	}
	eligibleThreads := []reportidentity.Thread{input.Threads[0], input.Threads[2]}
	for index, request := range state.client.requests {
		stateValue := decodeInferenceState(t, request)
		finding := input.Findings[index/len(eligibleThreads)]
		thread := eligibleThreads[index%len(eligibleThreads)]
		if stateValue.ProtocolVersion != 1 || stateValue.Task != "prior_threads" || stateValue.Left.Kind != "finding" || stateValue.Right.Kind != "thread" {
			t.Fatalf("prior-thread state tags = %#v", stateValue)
		}
		gotFinding := stateValue.Left.Finding
		if gotFinding.ID != string(finding.ID) || gotFinding.Severity != string(finding.Severity) || gotFinding.FilePath != finding.FilePath || gotFinding.Body != finding.Body || gotFinding.Anchoring != string(finding.Anchoring) || gotFinding.Anchor.Kind != string(finding.Anchor.Kind) || gotFinding.Anchor.Side != string(finding.Anchor.Side) || gotFinding.Anchor.Line != finding.Anchor.Line {
			t.Fatalf("finding projection lost fields: got=%#v source=%#v", gotFinding, finding)
		}
		gotThread := stateValue.Right.Thread
		if gotThread.ID != thread.ID || gotThread.Eligible != thread.Eligible || gotThread.FilePath != thread.FilePath || gotThread.Body != thread.Body {
			t.Fatalf("thread projection lost fields: got=%#v source=%#v", gotThread, thread)
		}
	}
	var generated reportidentity.Input
	if err := json.Unmarshal(readInferSystemOneOutput(t, summary.OutputDir, "fixture.json"), &generated); err != nil || len(generated.Threads) != 3 || len(generated.Decisions) != 4 {
		t.Fatalf("prior-thread fixture = %#v err=%v", generated, err)
	}
	var decoded identityInferenceSummary
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || decoded.EstimatedCostUSD != nil || decoded.UnknownCostCalls != 4 {
		t.Fatalf("decoded summary = %#v err=%v", decoded, err)
	}
	if receipts.Attempts[0].EstimatedCostUSD != nil || receipts.Attempts[0].Usage == nil {
		t.Fatalf("no-price attempt should retain usage and unknown estimate: %#v", receipts.Attempts[0])
	}
	assertInferSystemOneRetentionAndReplay(t, summary.OutputDir, input)
}

func TestUnifiedSystemOnePreparationMatchesSavedJevSmokeHashes(t *testing.T) {
	tests := []struct {
		name string
		path string
		want map[string]string
	}{
		{
			name: "cohort",
			path: "testdata/identity-smoke/cohort.json",
			want: map[string]string{
				"f1/f2": "989a0aa5968b5387281e6e35707e43825d8619ea6b0cb79d21613a6f7a2968d5",
				"f1/f3": "f67bf42a1c4c95058f16f0f88eb32c7b1539ff1298be0ac79a24b85837d0b532",
				"f2/f3": "b96fb00360b97c9bba8bbe9da37bdf8dcfb794a6ead36e241a1a7c26c82a7c5a",
			},
		},
		{
			name: "prior threads",
			path: "testdata/identity-smoke/prior-threads.json",
			want: map[string]string{
				"f1/t1": "aa8966981ad05746f780eb6b4b71baca3d3939157a8847982117feed36751903",
			},
		},
	}
	flags := identityInferenceFlags{model: "jev-1.14.0", maxStateBytes: 6 * 1024, maxRequestBytes: 8 * 1024}
	backend := inferSystemOneBackend(flags, inferSystemOneDependencies{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatalf("read smoke fixture: %v", err)
			}
			input, err := reportidentity.Decode(data)
			if err != nil {
				t.Fatalf("decode smoke fixture: %v", err)
			}
			result, err := reportidentity.Replay(input)
			if err != nil {
				t.Fatalf("replay smoke fixture: %v", err)
			}
			findings := make(map[string]review.Finding, len(input.Findings))
			for _, finding := range input.Findings {
				findings[string(finding.ID)] = finding
			}
			threads := make(map[string]reportidentity.Thread, len(input.Threads))
			for _, thread := range input.Threads {
				threads[thread.ID] = thread
			}
			got := make(map[string]string, len(result.Pairs))
			for _, pair := range result.Pairs {
				left := inferenceFindingRecord(findings[pair.LeftID])
				var right reportidentityinference.Record
				if input.Task == reportidentity.TaskCohort {
					right = inferenceFindingRecord(findings[pair.RightID])
				} else {
					right = inferenceThreadRecord(threads[pair.RightID])
				}
				projection, err := reportidentityinference.Project(reportidentityinference.Task(input.Task), left, right)
				if err != nil {
					t.Fatalf("project %s/%s: %v", pair.LeftID, pair.RightID, err)
				}
				plan, err := backend.prepare(projection)
				if err != nil {
					t.Fatalf("prepare %s/%s: %v", pair.LeftID, pair.RightID, err)
				}
				got[pair.LeftID+"/"+pair.RightID] = plan.RequestSHA256
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("prepared request hashes = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestInferSystemOneExplicitZeroPricesAreKnownZero(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	state, summary, receipts, _, _, err := runInferSystemOneTestCase(t, input, []string{"--input-price-per-million", "0", "--output-price-per-million", "0"}, []inferSystemOneFakeOutcome{{usage: &systemone.Usage{InputTokens: 100, OutputTokens: 20}}}, "key", true)
	if err != nil || state.client == nil {
		t.Fatalf("zero-price run err=%v state=%#v", err, state)
	}
	if summary.EstimatedCostUSD == nil || *summary.EstimatedCostUSD != 0 || summary.UnknownCostCalls != 0 || receipts.EstimatedCostUSD == nil || *receipts.EstimatedCostUSD != 0 {
		t.Fatalf("zero-price aggregate = summary:%#v receipts:%#v", summary.EstimatedCostUSD, receipts.EstimatedCostUSD)
	}
	if summary.Configuration.InputPricePerMillion == nil || summary.Configuration.OutputPricePerMillion == nil || *summary.Configuration.InputPricePerMillion != 0 || *summary.Configuration.OutputPricePerMillion != 0 {
		t.Fatalf("explicit zero prices lost in configuration: %#v", summary.Configuration)
	}
	if receipts.Attempts[0].EstimatedCostUSD == nil || *receipts.Attempts[0].EstimatedCostUSD != 0 {
		t.Fatalf("zero-price attempt estimate = %#v", receipts.Attempts[0].EstimatedCostUSD)
	}
}

func TestInferSystemOneZeroPairRunsNeedNoKeyAndHaveKnownZeroCost(t *testing.T) {
	prior := inferSystemOnePriorInput()
	prior.Findings = []review.Finding{inferSystemOneFinding("f1", review.SeverityMajor)}
	for index := range prior.Threads {
		prior.Threads[index].Eligible = false
	}
	inputs := []struct {
		name  string
		input reportidentity.Input
	}{
		{name: "singleton cohort", input: reportidentity.Input{SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort, Findings: []review.Finding{inferSystemOneFinding("single", review.SeverityMinor)}, Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{}}},
		{name: "no eligible threads", input: prior},
	}
	for _, test := range inputs {
		t.Run(test.name, func(t *testing.T) {
			state, summary, receipts, _, _, err := runInferSystemOneTestCase(t, test.input, nil, nil, "", false)
			if err != nil || state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
				t.Fatalf("zero-pair result: err=%v state=%#v", err, state)
			}
			if summary.Status != "complete" || summary.ExpectedPairs != 0 || summary.ProviderCalls != 0 || summary.UnknownCostCalls != 0 || summary.EstimatedCostUSD == nil || *summary.EstimatedCostUSD != 0 {
				t.Fatalf("zero-pair summary = %#v", summary)
			}
			if receipts.Configuration.RequestedModel != "provider/model-test" || receipts.Attempts == nil || len(receipts.Attempts) != 0 {
				t.Fatalf("zero-pair receipts = %#v", receipts)
			}
			assertInferSystemOneRetentionAndReplay(t, summary.OutputDir, test.input)
		})
	}
}

func assertInferSystemOneRetentionAndReplay(t *testing.T, outputDir string, input reportidentity.Input) {
	t.Helper()
	var retained reportidentity.Input
	if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "fixture.json"), &retained); err != nil {
		t.Fatalf("decode retained fixture: %v", err)
	}
	if !reflect.DeepEqual(retained.Findings, input.Findings) || !reflect.DeepEqual(retained.Threads, input.Threads) {
		t.Fatalf("original records changed: got=%#v source=%#v", retained, input)
	}
	replayed, err := reportidentity.Replay(retained)
	if err != nil {
		t.Fatalf("independent replay: %v", err)
	}
	replayedBytes, err := json.Marshal(replayed)
	if err != nil {
		t.Fatalf("encode independent replay: %v", err)
	}
	if saved := readInferSystemOneOutput(t, outputDir, "replay.json"); !bytes.Equal(saved, replayedBytes) {
		t.Fatalf("saved replay differs from independently replayed fixture: saved=%s replayed=%s", saved, replayedBytes)
	}
}

func TestInferSystemOnePreflightFailuresDoNotLookupKeyOrCallClient(t *testing.T) {
	oversized := inferSystemOneCohortInput(2)
	oversized.Findings[1].Body = strings.Repeat("x", 7000)
	tests := []struct {
		name        string
		input       reportidentity.Input
		maxCalls    int
		apiKey      string
		keyPresent  bool
		wantCode    string
		wantLookups int
	}{
		{name: "missing key", input: inferSystemOneCohortInput(2), maxCalls: 1, wantCode: "auth", wantLookups: 1},
		{name: "too many pairs", input: inferSystemOneCohortInput(3), maxCalls: 2, apiKey: "unused", keyPresent: true, wantCode: "invalid_input"},
		{name: "oversized request", input: oversized, maxCalls: 1, apiKey: "unused", keyPresent: true, wantCode: "context_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseDir := t.TempDir()
			fixturePath, inputBytes := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), test.input)
			outputDir := filepath.Join(baseDir, "output")
			dependencies, state := inferSystemOneTestDependencies(t, nil, test.apiKey, test.keyPresent)
			cmd, stdout, _ := newInferSystemOneTestCommand(t, nil, dependencies)
			args := inferSystemOneRunArgs(fixturePath, outputDir, test.maxCalls)
			err := executeLegacyInferSystemOne(t, cmd, args)
			if err == nil || state.keyLookups != test.wantLookups || state.clientOpens != 0 {
				t.Fatalf("preflight result err=%v state=%#v", err, state)
			}
			var summary identityInferenceSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("decode summary: %v", err)
			}
			var receipts identityInferenceReceipts
			if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
				t.Fatalf("decode receipts: %v", err)
			}
			if receipts.Status != "error" || receipts.ProviderCalls != 0 || receipts.EstimatedCostUSD == nil || *receipts.EstimatedCostUSD != 0 || receipts.UnknownCostCalls != 0 {
				t.Fatalf("preflight receipts = %#v", receipts)
			}
			if len(receipts.PreflightErrors) == 0 || receipts.PreflightErrors[0].ErrorCode != test.wantCode {
				t.Fatalf("preflight errors = %#v, want code %s", receipts.PreflightErrors, test.wantCode)
			}
			generated := readInferSystemOneOutput(t, outputDir, "fixture.json")
			var preserved reportidentity.Input
			if err := json.Unmarshal(generated, &preserved); err != nil || len(preserved.Findings) != len(test.input.Findings) {
				t.Fatalf("preflight did not preserve originals: err=%v fixture=%s", err, generated)
			}
			if receipts.InputSHA256 != sha256Hex(inputBytes) || summary.ProviderCalls != 0 {
				t.Fatalf("preflight hashes/counts = %#v", receipts)
			}
		})
	}
}

func TestInferSystemOneStopsAfterErrorAndReportsMixedKnownUnknownCosts(t *testing.T) {
	input := inferSystemOneCohortInput(4)
	outcomes := []inferSystemOneFakeOutcome{
		{choice: "a", usage: &systemone.Usage{InputTokens: 100, OutputTokens: 0}},
		{err: errors.New("synthetic provider failure"), missingUsage: true},
	}
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferSystemOneTestDependencies(t, outcomes, "key", true)
	cmd, stdout, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	args := inferSystemOneRunArgs(fixturePath, outputDir, 6)
	args = append(args, "--input-price-per-million", "1", "--output-price-per-million", "0")
	err := executeLegacyInferSystemOne(t, cmd, args)
	if err == nil || state.client == nil || state.client.decides != 2 {
		t.Fatalf("expected stop-on-first-error: err=%v state=%#v", err, state)
	}
	var summary identityInferenceSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if receipts.AttemptedPairs != 2 || receipts.ProviderCalls != 2 || receipts.CompletedPairs != 1 || receipts.FailedPairs != 1 || receipts.MissingPairs != 4 {
		t.Fatalf("stop-on-error counts = %#v", receipts)
	}
	if receipts.UnknownCostCalls != 1 || receipts.EstimatedCostUSD == nil || math.Abs(*receipts.EstimatedCostUSD-0.0001) > 1e-12 {
		t.Fatalf("mixed cost rollup = cost:%#v unknown:%d", receipts.EstimatedCostUSD, receipts.UnknownCostCalls)
	}
	if receipts.Attempts[1].EstimatedCostUSD != nil || receipts.Attempts[1].Usage != nil || receipts.Attempts[1].ErrorCode != "transport" {
		t.Fatalf("failed attempt metadata = %#v", receipts.Attempts[1])
	}
	var generated reportidentity.Input
	if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "fixture.json"), &generated); err != nil || len(generated.Decisions) != 2 || generated.Decisions[1].Choice != reportidentity.ChoiceError {
		t.Fatalf("partial fixture = %#v err=%v", generated, err)
	}
}

func TestInferSystemOneAttemptedPairCanHaveNoHTTPCall(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	outcomes := []inferSystemOneFakeOutcome{{err: context.Canceled, notAttempted: true, missingUsage: true}}
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferSystemOneTestDependencies(t, outcomes, "key", true)
	cmd, stdout, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	err := executeLegacyInferSystemOne(t, cmd, inferSystemOneRunArgs(fixturePath, outputDir, 1))
	if err == nil || state.client == nil || state.client.decides != 1 {
		t.Fatalf("pre-HTTP result err=%v state=%#v", err, state)
	}
	var summary identityInferenceSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatal(err)
	}
	if receipts.AttemptedPairs != 1 || receipts.ProviderCalls != 0 || receipts.UnknownCostCalls != 0 || receipts.EstimatedCostUSD == nil || *receipts.EstimatedCostUSD != 0 {
		t.Fatalf("attempt/HTTP/cost counts = %#v", receipts)
	}
	if receipts.Attempts[0].HTTPAttempted || receipts.Attempts[0].ErrorCode != "transport" {
		t.Fatalf("attempt receipt = %#v", receipts.Attempts[0])
	}
}

func TestInferSystemOneCancellationReachesActiveCallAndRetainsMissingPairs(t *testing.T) {
	input := inferSystemOneCohortInput(3)
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, state := inferSystemOneTestDependencies(t, nil, "key", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelClient := &cancelOnCallDecider{limits: systemone.Limits{MaxStateBytes: 6 * 1024, MaxRequestBytes: 8 * 1024}, cancel: cancel}
	dependencies.newClient = func(options systemone.Options) (systemOneDecider, error) {
		state.clientOpens++
		state.options = options
		cancelClient.limits = options.Limits
		return cancelClient, nil
	}
	cmd, stdout, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	cmd.SetContext(ctx)
	args := inferSystemOneRunArgs(fixturePath, outputDir, 3)
	args = append(args, "--input-price-per-million", "0", "--output-price-per-million", "0")
	err := executeLegacyInferSystemOne(t, cmd, args)
	if err == nil || cancelClient.calls != 1 {
		t.Fatalf("canceled active call err=%v calls=%d", err, cancelClient.calls)
	}
	var summary identityInferenceSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatal(err)
	}
	if receipts.ProviderCalls != 1 || receipts.FailedPairs != 1 || receipts.MissingPairs != 2 || receipts.Attempts[0].ErrorCode != expectedCancelCode {
		t.Fatalf("active cancellation receipt = %#v", receipts)
	}
	if receipts.UnknownCostCalls != 0 || receipts.EstimatedCostUSD == nil || *receipts.EstimatedCostUSD != 0 {
		t.Fatalf("active cancellation cost = %#v unknown=%d", receipts.EstimatedCostUSD, receipts.UnknownCostCalls)
	}
}

const expectedCancelCode = "cancelled" //nolint:misspell // Stable System One error category.

func TestInferSystemOneContradictoryTriangleRemainsUnresolved(t *testing.T) {
	input := inferSystemOneCohortInput(3)
	outcomes := []inferSystemOneFakeOutcome{{choice: "a"}, {choice: "a"}, {choice: "b"}}
	state, summary, _, _, _, err := runInferSystemOneTestCase(t, input, nil, outcomes, "key", true)
	if err != nil || state.client == nil || len(state.client.requests) != 3 || summary.Status != "unresolved" {
		t.Fatalf("triangle result err=%v summary=%#v state=%#v", err, summary, state)
	}
	var replay reportidentity.Result
	if err := json.Unmarshal(readInferSystemOneOutput(t, summary.OutputDir, "replay.json"), &replay); err != nil || len(replay.Issues) == 0 {
		t.Fatalf("triangle replay issues=%#v err=%v", replay.Issues, err)
	}
}

func TestInferSystemOneFlagsFailBeforeCreatingOutput(t *testing.T) {
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), inferSystemOneCohortInput(2))
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing model", args: []string{"benchmark", "infer-system-one", fixturePath, "--max-calls", "1", "--output-dir", filepath.Join(baseDir, "missing-model")}},
		{name: "missing max calls", args: []string{"benchmark", "infer-system-one", fixturePath, "--model", "m", "--output-dir", filepath.Join(baseDir, "missing-calls")}},
		{name: "missing output", args: []string{"benchmark", "infer-system-one", fixturePath, "--model", "m", "--max-calls", "1"}},
		{name: "whitespace model", args: []string{"benchmark", "infer-system-one", fixturePath, "--model", " ", "--max-calls", "1", "--output-dir", filepath.Join(baseDir, "blank-model")}},
		{name: "invalid HTTPS endpoint", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "http"), 1), "--endpoint", "http://bad.example")},
		{name: "endpoint query", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "query"), 1), "--endpoint", "https://example.test/path?q=secret")},
		{name: "invalid key environment name", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "bad-env"), 1), "--api-key-env", "bad-name")},
		{name: "invalid auth scheme", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "bad-auth"), 1), "--auth-scheme", "basic")},
		{name: "nonpositive state limit", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "bad-state-limit"), 1), "--max-state-bytes", "0")},
		{name: "one price only", args: append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "one-price"), 1), "--input-price-per-million", "0")},
		{name: "negative price", args: append(append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "negative-price"), 1), "--input-price-per-million", "-1"), "--output-price-per-million", "1")},
		{name: "nonfinite price", args: append(append(inferSystemOneRunArgs(fixturePath, filepath.Join(baseDir, "nan-price"), 1), "--input-price-per-million", "NaN"), "--output-price-per-million", "1")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies, state := inferSystemOneTestDependencies(t, nil, "key", true)
			cmd, _, _ := newInferSystemOneTestCommand(t, nil, dependencies)
			err := executeLegacyInferSystemOne(t, cmd, test.args)
			if err == nil || state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("flag error=%v state=%#v", err, state)
			}
			for index, arg := range test.args {
				if arg == "--output-dir" {
					outputPath := test.args[index+1]
					if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("invalid flags created output %s or stat failed: %v", outputPath, statErr)
					}
				}
			}
		})
	}
}

func TestInferSystemOneDoesNotOverwriteExistingOutputOrFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), inferSystemOneCohortInput(2))
	existing := filepath.Join(baseDir, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies, state := inferSystemOneTestDependencies(t, nil, "key", true)
	cmd, _, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	if err := executeLegacyInferSystemOne(t, cmd, inferSystemOneRunArgs(fixturePath, existing, 1)); err == nil {
		t.Fatal("existing output directory was accepted")
	}
	if state.keyLookups != 0 || state.clientOpens != 0 {
		t.Fatalf("existing directory caused external access: %#v", state)
	}
	if data := readInferSystemOneOutput(t, existing, "marker"); string(data) != "keep" {
		t.Fatalf("existing marker changed: %q", data)
	}

	target := filepath.Join(baseDir, "target")
	link := filepath.Join(baseDir, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dependencies, state = inferSystemOneTestDependencies(t, nil, "key", true)
	cmd, _, _ = newInferSystemOneTestCommand(t, nil, dependencies)
	if err := executeLegacyInferSystemOne(t, cmd, inferSystemOneRunArgs(fixturePath, link, 1)); err == nil {
		t.Fatal("symlink output directory was accepted")
	}
	if state.keyLookups != 0 || state.clientOpens != 0 {
		t.Fatalf("symlink output caused external access: %#v", state)
	}
}

func TestInferSystemOneReservationFailuresDoNotUseCredentialsOrClient(t *testing.T) {
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(t.TempDir(), "input"), inferSystemOneCohortInput(2))
	tests := []struct {
		name      string
		configure func(*inferSystemOneDependencies)
	}{
		{
			name: "mkdir denied",
			configure: func(dependencies *inferSystemOneDependencies) {
				dependencies.mkdir = func(string, os.FileMode) error { return os.ErrPermission }
			},
		},
		{
			name: "chmod denied",
			configure: func(dependencies *inferSystemOneDependencies) {
				dependencies.chmod = func(string, os.FileMode) error { return os.ErrPermission }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputDir := filepath.Join(t.TempDir(), "output")
			dependencies, state := inferSystemOneTestDependencies(t, nil, "key", true)
			test.configure(&dependencies)
			cmd, _, _ := newInferSystemOneTestCommand(t, nil, dependencies)
			if err := executeLegacyInferSystemOne(t, cmd, inferSystemOneRunArgs(fixturePath, outputDir, 1)); err == nil {
				t.Fatal("reservation failure was not returned")
			}
			if state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
				t.Fatalf("reservation failure used credentials/client: %#v", state)
			}
			if _, err := os.Lstat(filepath.Join(outputDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reservation failure published manifest or stat err=%v", err)
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reservation failure left output directory: %v", err)
			}
		})
	}
}

func TestInferSystemOnePartialPublicationLeavesManifestAbsent(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	baseDir := t.TempDir()
	fixturePath, _ := writeInferSystemOneInput(t, filepath.Join(baseDir, "input"), input)
	outputDir := filepath.Join(baseDir, "output")
	dependencies, _ := inferSystemOneTestDependencies(t, []inferSystemOneFakeOutcome{{choice: "a"}}, "key", true)
	originalRename := dependencies.rename
	dependencies.rename = func(oldPath, newPath string) error {
		if filepath.Base(newPath) == "replay.json" {
			return errors.New("synthetic rename failure")
		}
		return originalRename(oldPath, newPath)
	}
	cmd, _, _ := newInferSystemOneTestCommand(t, nil, dependencies)
	if err := executeLegacyInferSystemOne(t, cmd, inferSystemOneRunArgs(fixturePath, outputDir, 1)); err == nil {
		t.Fatal("publication failure was not returned")
	}
	if _, err := os.Stat(filepath.Join(outputDir, "fixture.json")); err != nil {
		t.Fatalf("first artifact was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists after partial publication or stat err=%v", err)
	}
}

func decodeInferenceState(t *testing.T, request systemone.Request) struct {
	ProtocolVersion int    `json:"protocol_version"`
	Task            string `json:"task"`
	Left            struct {
		Kind    string `json:"kind"`
		Finding struct {
			ID       string `json:"finding_id"`
			Severity string `json:"severity"`
			FilePath string `json:"file_path"`
			Anchor   struct {
				Kind string `json:"kind"`
				Side string `json:"side,omitempty"`
				Line int    `json:"line,omitempty"`
			} `json:"anchor"`
			Body      string `json:"body"`
			Anchoring string `json:"anchoring,omitempty"`
		} `json:"finding"`
	} `json:"left"`
	Right struct {
		Kind   string `json:"kind"`
		Thread struct {
			ID       string `json:"thread_id"`
			Eligible bool   `json:"eligible"`
			FilePath string `json:"file_path"`
			Body     string `json:"body"`
		} `json:"thread"`
	} `json:"right"`
} {
	t.Helper()
	var state struct {
		ProtocolVersion int    `json:"protocol_version"`
		Task            string `json:"task"`
		Left            struct {
			Kind    string `json:"kind"`
			Finding struct {
				ID       string `json:"finding_id"`
				Severity string `json:"severity"`
				FilePath string `json:"file_path"`
				Anchor   struct {
					Kind string `json:"kind"`
					Side string `json:"side,omitempty"`
					Line int    `json:"line,omitempty"`
				} `json:"anchor"`
				Body      string `json:"body"`
				Anchoring string `json:"anchoring,omitempty"`
			} `json:"finding"`
		} `json:"left"`
		Right struct {
			Kind   string `json:"kind"`
			Thread struct {
				ID       string `json:"thread_id"`
				Eligible bool   `json:"eligible"`
				FilePath string `json:"file_path"`
				Body     string `json:"body"`
			} `json:"thread"`
		} `json:"right"`
	}
	if err := json.Unmarshal(request.State, &state); err != nil {
		t.Fatalf("decode inference state: %v", err)
	}
	return state
}

func TestInferSystemOneInvalidFakeAnswerFailsClosedAndRecordsHashMismatch(t *testing.T) {
	input := inferSystemOneCohortInput(2)
	tests := []struct {
		name    string
		outcome inferSystemOneFakeOutcome
		code    string
	}{
		{name: "unknown choice", outcome: inferSystemOneFakeOutcome{answers: map[string]systemone.ChoiceAnswer{"identity": {Choice: "x", Confidence: 0.7, Probabilities: map[string]float64{"a": 0.7, "b": 0.2, "c": 0.1}}}}, code: "malformed_response"},
		{name: "planned hash mismatch", outcome: inferSystemOneFakeOutcome{requestSHA256: "different-hash"}, code: "malformed_response"},
		{name: "wrong returned model", outcome: inferSystemOneFakeOutcome{returnedModel: "provider/other-model"}, code: "malformed_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, summary, receipts, _, _, err := runInferSystemOneTestCase(t, input, nil, []inferSystemOneFakeOutcome{test.outcome}, "key", true)
			if err == nil || state.client == nil || len(receipts.Attempts) != 1 || receipts.Attempts[0].ErrorCode != test.code || receipts.Attempts[0].Answer != nil {
				t.Fatalf("fail-closed result err=%v summary=%#v receipts=%#v", err, summary, receipts)
			}
		})
	}
}
