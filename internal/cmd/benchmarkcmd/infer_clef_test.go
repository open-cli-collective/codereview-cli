package benchmarkcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	clefTestAccountID = "account123"
	clefTestAPIKey    = "synthetic-cloudflare-token"
)

var clefTestLimits = systemone.Limits{MaxStateBytes: 6 * 1024, MaxRequestBytes: 8 * 1024}

type clefFakeOutcome struct {
	choice        string
	errorCode     string
	err           error
	noAttempt     bool
	returnedModel string
	requestSHA256 string
	usage         *cloudflareclef.Usage
}

type clefFakeClient struct {
	t                      *testing.T
	limits                 systemone.Limits
	outcomes               []clefFakeOutcome
	requests               []systemone.Request
	claimedVerifiedContext bool
}

func (client *clefFakeClient) Decide(_ context.Context, request systemone.Request) (cloudflareclef.Receipt, error) {
	client.t.Helper()
	client.requests = append(client.requests, request)
	plan, _, err := systemone.Encode(request, client.limits)
	if err != nil {
		client.t.Fatalf("systemone.Encode for fake call: %v", err)
	}
	index := len(client.requests) - 1
	outcome := clefFakeOutcome{choice: "a"}
	if index < len(client.outcomes) {
		outcome = client.outcomes[index]
	}
	returnedModel := request.ExpectedModel
	if returnedModel == "" {
		returnedModel = request.Model
	}
	if outcome.returnedModel != "" {
		returnedModel = outcome.returnedModel
	}
	receipt := cloudflareclef.Receipt{
		RequestedModel: request.Model,
		ReturnedModel:  returnedModel,
		RequestSHA256:  plan.RequestSHA256,
		ResponseSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("clef-synthetic-response-%d", index+1)))),
		RequestID:      fmt.Sprintf("clef-request-%d", index+1),
		Duration:       17 * time.Millisecond,
		Attempted:      !outcome.noAttempt,
		HTTPStatus:     http.StatusOK,
		ContextStatus:  "verified",
	}
	if outcome.requestSHA256 != "" {
		receipt.RequestSHA256 = outcome.requestSHA256
	}
	if outcome.usage == nil {
		inputTokens := int64(25 + index)
		outputTokens := int64(4)
		total := inputTokens + outputTokens
		receipt.Usage = &cloudflareclef.Usage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: &total}
	} else {
		receipt.Usage = cloneClefFakeUsage(outcome.usage)
	}
	if !outcome.noAttempt {
		choice := outcome.choice
		if choice == "" {
			choice = "a"
		}
		receipt.Answers = map[string]systemone.ChoiceAnswer{
			"identity": {Choice: choice, Confidence: 0.7, Probabilities: clefFakeProbabilities(choice)},
		}
	} else {
		receipt.ResponseSHA256 = ""
		receipt.HTTPStatus = 0
	}
	if outcome.errorCode == "truncated" {
		response := fmt.Sprintf(`{"model":%q,"truncated":true,"answers":{"identity":{"type":"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.7,"b":0.2,"c":0.1}}},"usage":{"input_tokens":25,"output_tokens":4}}`, returnedModel)
		_, outcome.err = systemone.Decode([]byte(response), request, client.limits)
	}
	client.claimedVerifiedContext = receipt.ContextStatus == "verified"
	return receipt, outcome.err
}

type clefCommandTestState struct {
	client       *clefFakeClient
	clientOpts   cloudflareclef.Options
	clientOpens  int
	keyLookups   int
	keyNames     []string
	keyPresent   bool
	apiKey       string
	renamedFiles []string
}

func clefTestDependencies(t *testing.T, outcomes []clefFakeOutcome, keyPresent bool) (inferClefDependencies, *clefCommandTestState) {
	t.Helper()
	state := &clefCommandTestState{apiKey: clefTestAPIKey, keyPresent: keyPresent}
	dependencies := inferClefDependencies{
		lookupAPIKey: func(name string) (string, bool) {
			state.keyLookups++
			state.keyNames = append(state.keyNames, name)
			return state.apiKey, state.keyPresent
		},
		newClient: func(options cloudflareclef.Options) (clefDecider, error) {
			state.clientOpens++
			state.clientOpts = options
			state.client = &clefFakeClient{t: t, limits: options.Limits, outcomes: outcomes}
			return state.client, nil
		},
		now: func() time.Time { return time.Date(2026, time.November, 7, 14, 30, 0, 0, time.UTC) },
		rename: func(oldPath, newPath string) error {
			state.renamedFiles = append(state.renamedFiles, filepath.Base(newPath))
			return os.Rename(oldPath, newPath)
		},
	}
	return dependencies, state
}

func newInferClefTestCommand(t *testing.T, dependencies inferClefDependencies) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	opts := &root.Options{Stdin: strings.NewReader("")}
	stdout := &bytes.Buffer{}
	opts.Stdout = stdout
	opts.Stderr = &bytes.Buffer{}
	command, rootOptions := root.NewCommandWithOptions(opts)
	root.RegisterAll(command, rootOptions, func(rootCommand *cobra.Command, rootOpts *root.Options) {
		benchmark := &cobra.Command{Use: "benchmark", Short: "Benchmark test tree"}
		benchmark.AddCommand(newInferClefCommandWithDependencies(rootOpts, dependencies))
		rootCommand.AddCommand(benchmark)
	})
	return command, stdout
}

func clefRunArgs(fixturePath, outputDir, model, accountID string, maxCalls int) []string {
	return []string{
		"benchmark", "infer-clef", fixturePath,
		"--model", model,
		"--account-id", accountID,
		"--allow-unverified-context",
		"--max-calls", fmt.Sprint(maxCalls),
		"--output-dir", outputDir,
	}
}

func runInferClefTest(t *testing.T, input reportidentity.Input, outputDir, model, accountID string, maxCalls int, dependencies inferClefDependencies, extraArgs ...string) (*bytes.Buffer, error) {
	t.Helper()
	fixtureDir := filepath.Join(t.TempDir(), "fixture-source")
	fixturePath := writeClefFixture(t, fixtureDir, input)
	args := append(clefRunArgs(fixturePath, outputDir, model, accountID, maxCalls), extraArgs...)
	command, stdout := newInferClefTestCommand(t, dependencies)
	command.SetArgs(args)
	return stdout, command.Execute()
}

func writeClefFixture(t *testing.T, directory string, input reportidentity.Input) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll fixture directory: %v", err)
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal fixture: %v", err)
	}
	path := filepath.Join(directory, "fixture.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}
	return path
}

func clefCohortInput(t *testing.T, findingCount int) reportidentity.Input {
	t.Helper()
	input := readClefSyntheticFixture(t, "cohort.json")
	if findingCount < 0 || findingCount > len(input.Findings) {
		t.Fatalf("cohort subset size %d is outside shared fixture size %d", findingCount, len(input.Findings))
	}
	input.Findings = input.Findings[:findingCount]
	return input
}

func clefPriorThreadInput(t *testing.T) reportidentity.Input {
	t.Helper()
	return readClefSyntheticFixture(t, "prior-threads.json")
}

func readClefSyntheticFixture(t *testing.T, name string) reportidentity.Input {
	t.Helper()
	data, err := readReplayFixture(filepath.Join("testdata", "identity-smoke", name))
	if err != nil {
		t.Fatalf("read shared synthetic fixture %s: %v", name, err)
	}
	input, err := reportidentity.Decode(data)
	if err != nil {
		t.Fatalf("decode shared synthetic fixture %s: %v", name, err)
	}
	return input
}

func cloneClefFakeUsage(source *cloudflareclef.Usage) *cloudflareclef.Usage {
	if source == nil {
		return nil
	}
	copyUsage := *source
	if source.TotalTokens != nil {
		total := *source.TotalTokens
		copyUsage.TotalTokens = &total
	}
	return &copyUsage
}

func clefFakeProbabilities(choice string) map[string]float64 {
	values := map[string]float64{"a": 0.2, "b": 0.1, "c": 0.0}
	switch choice {
	case "a":
		values = map[string]float64{"a": 0.7, "b": 0.2, "c": 0.1}
	case "b":
		values = map[string]float64{"a": 0.2, "b": 0.7, "c": 0.1}
	case "c":
		values = map[string]float64{"a": 0.1, "b": 0.2, "c": 0.7}
	}
	return values
}

func readClefOutput(t *testing.T, outputDir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, name)) // #nosec G304 -- tests read generated artifacts under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return data
}

func assertClefReplayMatchesRetainedFixture(t *testing.T, outputDir string) {
	t.Helper()
	var input reportidentity.Input
	if err := json.Unmarshal(readClefOutput(t, outputDir, "fixture.json"), &input); err != nil {
		t.Fatalf("decode retained fixture for replay: %v", err)
	}
	got, err := reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("independent replay: %v", err)
	}
	var want reportidentity.Result
	if err := json.Unmarshal(readClefOutput(t, outputDir, "replay.json"), &want); err != nil {
		t.Fatalf("decode retained replay: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retained replay differs from independent replay\n got: %#v\nwant: %#v", got, want)
	}
}

func assertClefOutputMode(t *testing.T, path string, want os.FileMode) {
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

func TestInferClefRoutesBothSelectorsThroughSharedProjection(t *testing.T) {
	for _, model := range []string{"clef", "clef-flash"} {
		t.Run(model, func(t *testing.T) {
			outputDir := filepath.Join(t.TempDir(), "out")
			dependencies, state := clefTestDependencies(t, nil, true)
			input := clefCohortInput(t, 2)
			_, err := runInferClefTest(t, input, outputDir, model, clefTestAccountID, 10, dependencies)
			if err != nil {
				t.Fatalf("infer-clef: %v", err)
			}
			if state.keyLookups != 1 || state.clientOpens != 1 || len(state.client.requests) != 1 {
				t.Fatalf("preflight/client counts: key lookups=%d clients=%d requests=%d", state.keyLookups, state.clientOpens, len(state.client.requests))
			}
			if state.keyNames[0] != defaultClefAPIKeyEnv || state.clientOpts.AccountID != clefTestAccountID || state.clientOpts.APIKey != clefTestAPIKey || state.clientOpts.Timeout != defaultIdentityInferenceTimeout || state.clientOpts.Limits != clefTestLimits {
				t.Fatalf("Cloudflare client options = %#v, key names=%v", state.clientOpts, state.keyNames)
			}
			request := state.client.requests[0]
			if request.Model != model || request.ExpectedModel != model || request.ServiceTier != "" {
				t.Fatalf("request selection = %#v", request)
			}
			projection, err := reportidentityinference.Project(reportidentityinference.TaskCohort, inferenceFindingRecord(input.Findings[0]), inferenceFindingRecord(input.Findings[1]))
			if err != nil {
				t.Fatalf("Project expected request: %v", err)
			}
			if !bytes.Equal(request.State, projection.State) || len(request.Questions) != 1 {
				t.Fatalf("request did not use the shared projection: state=%s questions=%#v", request.State, request.Questions)
			}
			question, ok := request.Questions[projection.QuestionName]
			if !ok || question.Instructions != projection.Question.Instructions || !reflect.DeepEqual(question.Criteria, projection.Question.Criteria) {
				t.Fatalf("request question diverged from shared projection: %#v", request.Questions)
			}
			plan, encoded, err := systemone.Encode(request, clefTestLimits)
			if err != nil {
				t.Fatalf("System One encode: %v", err)
			}
			cloudflarePlan, err := cloudflareclef.Prepare(request, clefTestLimits)
			if err != nil || cloudflarePlan != plan || fmt.Sprintf("%x", sha256.Sum256(encoded)) != plan.RequestSHA256 {
				t.Fatalf("Cloudflare/System One planned bytes differ: cloudflare=%#v systemone=%#v err=%v", cloudflarePlan, plan, err)
			}

			var receipts identityInferenceReceipts
			if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
				t.Fatalf("decode receipts: %v", err)
			}
			wantEndpoint, err := cloudflareclef.Endpoint(clefTestAccountID, model)
			if err != nil || receipts.Configuration.Endpoint != wantEndpoint || receipts.Configuration.Provider != "cloudflare_clef" || receipts.Configuration.ContextStatus != "unverified" || receipts.Configuration.AuthScheme != "Bearer" || receipts.Configuration.ServiceTier != nil {
				t.Fatalf("configuration provenance = %#v, endpoint err=%v", receipts.Configuration, err)
			}
			if len(receipts.Attempts) != 1 || receipts.Attempts[0].ContextStatus != "unverified" || receipts.Attempts[0].PlannedRequestSHA256 != plan.RequestSHA256 || receipts.Attempts[0].RequestSHA256 != plan.RequestSHA256 {
				t.Fatalf("planned/actual attempt receipt = %#v", receipts.Attempts)
			}
			if !state.client.claimedVerifiedContext {
				t.Fatal("fake client did not claim verified context for adapter override assertion")
			}
			if bytes.Contains(readClefOutput(t, outputDir, "receipts.json"), []byte(clefTestAPIKey)) {
				t.Fatal("API key appeared in retained receipts")
			}
		})
	}
}

func TestInferClefRequiresUnverifiedAcknowledgementAndValidRouteBeforeCredentials(t *testing.T) {
	fixtureDir := t.TempDir()
	fixture := writeClefFixture(t, fixtureDir, clefCohortInput(t, 2))
	for _, test := range []struct {
		name        string
		model       string
		accountID   string
		acknowledge bool
		extra       []string
	}{
		{name: "missing unverified context acknowledgement", model: "clef", accountID: clefTestAccountID},
		{name: "invalid model", model: "@cf/cloudflare/clef", accountID: clefTestAccountID, acknowledge: true},
		{name: "missing model", accountID: clefTestAccountID, acknowledge: true},
		{name: "account path injection", model: "clef", accountID: "../account", acknowledge: true},
		{name: "missing account", model: "clef", acknowledge: true},
		{name: "invalid call cap", model: "clef", accountID: clefTestAccountID, acknowledge: true, extra: []string{"--max-calls=-1"}},
		{name: "unpaired prices", model: "clef", accountID: clefTestAccountID, acknowledge: true, extra: []string{"--input-price-per-million=1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputDir := filepath.Join(t.TempDir(), "out")
			dependencies, state := clefTestDependencies(t, nil, true)
			args := []string{"benchmark", "infer-clef", fixture, "--model", test.model, "--account-id", test.accountID, "--max-calls", "10", "--output-dir", outputDir}
			if test.acknowledge {
				args = append(args, "--allow-unverified-context")
			}
			args = append(args, test.extra...)
			command, _ := newInferClefTestCommand(t, dependencies)
			command.SetArgs(args)
			err := command.Execute()
			if err == nil || state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("invalid invocation result=%v keyLookups=%d clientOpens=%d", err, state.keyLookups, state.clientOpens)
			}
			if _, err := os.Stat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid invocation created output directory: stat error=%v", err)
			}
		})
	}

	command := newInferClefCommandWithDependencies(nil, inferClefDependencies{})
	usage := command.Flags().Lookup("allow-unverified-context").Usage
	if !strings.Contains(usage, "silently truncate") || !strings.Contains(usage, "full input") {
		t.Fatalf("context acknowledgement help = %q", usage)
	}
	for _, forbidden := range []string{"endpoint", "auth-scheme", "service-tier"} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Fatalf("Clef command unexpectedly exposes --%s", forbidden)
		}
	}
}

func TestInferClefEmptyFixtureCreatesNoClientOrKeyLookup(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "out")
	dependencies, state := clefTestDependencies(t, nil, false)
	input := clefCohortInput(t, 0)
	_, err := runInferClefTest(t, input, outputDir, "clef", clefTestAccountID, 10, dependencies)
	if err != nil {
		t.Fatalf("empty fixture inference: %v", err)
	}
	if state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
		t.Fatalf("empty fixture performed credential/client work: lookups=%d clients=%d", state.keyLookups, state.clientOpens)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if receipts.ProviderCalls != 0 || receipts.EstimatedCostUSD == nil || *receipts.EstimatedCostUSD != 0 {
		t.Fatalf("empty fixture accounting = calls:%d cost:%v", receipts.ProviderCalls, receipts.EstimatedCostUSD)
	}
}

func TestInferClefCallBudgetPreflightAvoidsCredentials(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "out")
	dependencies, state := clefTestDependencies(t, nil, true)
	_, err := runInferClefTest(t, clefCohortInput(t, 3), outputDir, "clef", clefTestAccountID, 2, dependencies)
	if err == nil || state.keyLookups != 0 || state.clientOpens != 0 || state.client != nil {
		t.Fatalf("budget preflight result=%v lookups=%d clients=%d", err, state.keyLookups, state.clientOpens)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode preflight receipts: %v", err)
	}
	if receipts.ExpectedPairs != 3 || receipts.ProviderCalls != 0 || receipts.FailedPairs != 0 || len(receipts.PreflightErrors) == 0 {
		t.Fatalf("budget preflight receipt = %#v", receipts)
	}
}

func TestInferClefRetainsCohortAndPriorThreadInputsAndWritesManifestLast(t *testing.T) {
	t.Run("cohort", func(t *testing.T) {
		input := clefCohortInput(t, 3)
		outputDir := filepath.Join(t.TempDir(), "out")
		dependencies, state := clefTestDependencies(t, []clefFakeOutcome{{choice: "a"}, {choice: "b"}, {choice: "c"}}, true)
		_, err := runInferClefTest(t, input, outputDir, "clef", clefTestAccountID, 10, dependencies, "--input-price-per-million", "12.5", "--output-price-per-million", "8")
		if err != nil {
			t.Fatalf("cohort inference: %v", err)
		}
		var receipts identityInferenceReceipts
		if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
			t.Fatalf("decode receipts: %v", err)
		}
		if receipts.ExpectedPairs != 3 || receipts.CompletedPairs != 3 || receipts.ProviderCalls != 3 || receipts.MissingPairs != 0 || receipts.KnownInputTokens != 78 || receipts.KnownOutputTokens != 12 || receipts.EstimatedCostUSD == nil || receipts.UnknownCostCalls != 0 {
			t.Fatalf("cohort counts/accounting = %#v", receipts)
		}
		if math.Abs(*receipts.EstimatedCostUSD-(float64(78)*12.5/1_000_000+float64(12)*8/1_000_000)) > 1e-12 {
			t.Fatalf("estimated cost = %g", *receipts.EstimatedCostUSD)
		}
		var retained reportidentity.Input
		if err := json.Unmarshal(readClefOutput(t, outputDir, "fixture.json"), &retained); err != nil {
			t.Fatalf("decode retained fixture: %v", err)
		}
		if !reflect.DeepEqual(retained.Findings, input.Findings) || !reflect.DeepEqual(retained.Threads, input.Threads) || len(retained.Decisions) != 3 {
			t.Fatalf("cohort source records or decisions changed: retained=%#v", retained)
		}
		assertClefReplayMatchesRetainedFixture(t, outputDir)
		assertClefOutputMode(t, outputDir, 0o700)
		for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
			assertClefOutputMode(t, filepath.Join(outputDir, name), 0o600)
		}
		if want := []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"}; !reflect.DeepEqual(state.renamedFiles, want) {
			t.Fatalf("publication order = %v, want %v", state.renamedFiles, want)
		}
	})

	t.Run("prior threads", func(t *testing.T) {
		input := clefPriorThreadInput(t)
		outputDir := filepath.Join(t.TempDir(), "out")
		dependencies, state := clefTestDependencies(t, []clefFakeOutcome{{choice: "a"}}, true)
		_, err := runInferClefTest(t, input, outputDir, "clef-flash", clefTestAccountID, 10, dependencies)
		if err != nil {
			t.Fatalf("prior-thread inference: %v", err)
		}
		var receipts identityInferenceReceipts
		if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
			t.Fatalf("decode receipts: %v", err)
		}
		if receipts.ExpectedPairs != 1 || receipts.CompletedPairs != 1 || receipts.ProviderCalls != 1 || receipts.MissingPairs != 0 || receipts.ThreadCount != 2 || receipts.UnknownCostCalls != 1 || receipts.EstimatedCostUSD != nil {
			t.Fatalf("prior-thread counts/cost = %#v", receipts)
		}
		if len(state.client.requests) != 1 {
			t.Fatalf("eligible pair request count = %d", len(state.client.requests))
		}
		for index, request := range state.client.requests {
			if request.Model != "clef-flash" || len(request.Questions) != 1 {
				t.Errorf("prior-thread request %d = %#v", index, request)
			}
		}
		var retained reportidentity.Input
		if err := json.Unmarshal(readClefOutput(t, outputDir, "fixture.json"), &retained); err != nil {
			t.Fatalf("decode retained prior fixture: %v", err)
		}
		if !reflect.DeepEqual(retained.Findings, input.Findings) || !reflect.DeepEqual(retained.Threads, input.Threads) || retained.Threads[1].Eligible || len(retained.Decisions) != 1 {
			t.Fatalf("prior-thread originals/ineligible thread changed: %#v", retained)
		}
		assertClefReplayMatchesRetainedFixture(t, outputDir)
	})
}

func TestInferClefExpectedModelOverrideAndUnverifiedReceipts(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "out")
	dependencies, _ := clefTestDependencies(t, nil, true)
	_, err := runInferClefTest(t, clefCohortInput(t, 2), outputDir, "clef", clefTestAccountID, 10, dependencies, "--expected-model", "clef-build-42")
	if err != nil {
		t.Fatalf("alias inference: %v", err)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if receipts.Configuration.RequestedModel != "clef" || receipts.Configuration.ExpectedModel != "clef-build-42" || len(receipts.Attempts) != 1 {
		t.Fatalf("model policy configuration = %#v", receipts.Configuration)
	}
	if attempt := receipts.Attempts[0]; attempt.RequestedModel != "clef" || attempt.ReturnedModel != "clef-build-42" || attempt.ContextStatus != "unverified" {
		t.Fatalf("model policy/forced context status attempt = %#v", attempt)
	}
}

func TestInferClefKeepsMissingTotalUnknownWhileEstimatingFromKnownBaseTokens(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "out")
	dependencies, _ := clefTestDependencies(t, []clefFakeOutcome{{
		choice: "a", usage: &cloudflareclef.Usage{InputTokens: 10, OutputTokens: 2},
	}}, true)
	_, err := runInferClefTest(t, clefCohortInput(t, 2), outputDir, "clef", clefTestAccountID, 10, dependencies,
		"--input-price-per-million", "4", "--output-price-per-million", "1")
	if err != nil {
		t.Fatalf("inference with unknown total: %v", err)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode receipts: %v", err)
	}
	if len(receipts.Attempts) != 1 || receipts.Attempts[0].Usage == nil || receipts.Attempts[0].Usage.TotalTokens != nil {
		t.Fatalf("missing provider total was not retained as unknown: %#v", receipts.Attempts)
	}
	wantCost := float64(10)*4/1_000_000 + float64(2)*1/1_000_000
	if receipts.Attempts[0].EstimatedCostUSD == nil || math.Abs(*receipts.Attempts[0].EstimatedCostUSD-wantCost) > 1e-12 {
		t.Fatalf("base-token cost = %v, want %g", receipts.Attempts[0].EstimatedCostUSD, wantCost)
	}
}

func TestInferClefTruncationFailsStopWithPartialRecordsAndUsage(t *testing.T) {
	input := clefCohortInput(t, 3)
	outputDir := filepath.Join(t.TempDir(), "out")
	dependencies, state := clefTestDependencies(t, []clefFakeOutcome{{errorCode: "truncated"}, {choice: "b"}}, true)
	_, err := runInferClefTest(t, input, outputDir, "clef", clefTestAccountID, 10, dependencies)
	if err == nil {
		t.Fatal("truncated provider result unexpectedly succeeded")
	}
	if state.client == nil || len(state.client.requests) != 1 {
		t.Fatalf("truncation did not fail-stop after one call: %#v", state.client)
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("decode partial receipts: %v", err)
	}
	if receipts.ExpectedPairs != 3 || receipts.AttemptedPairs != 1 || receipts.ProviderCalls != 1 || receipts.FailedPairs != 1 || receipts.MissingPairs != 2 || len(receipts.Attempts) != 1 {
		t.Fatalf("partial counts = %#v", receipts)
	}
	attempt := receipts.Attempts[0]
	if attempt.ErrorCode != "truncated" || attempt.ContextStatus != "unverified" || attempt.Usage == nil || attempt.Usage.InputTokens != 25 || attempt.Usage.OutputTokens != 4 || attempt.RequestSHA256 == "" {
		t.Fatalf("partial attempt metadata = %#v", attempt)
	}
	var retained reportidentity.Input
	if err := json.Unmarshal(readClefOutput(t, outputDir, "fixture.json"), &retained); err != nil {
		t.Fatalf("decode retained partial fixture: %v", err)
	}
	if !reflect.DeepEqual(retained.Findings, input.Findings) || !reflect.DeepEqual(retained.Threads, input.Threads) || len(retained.Decisions) != 1 || retained.Decisions[0].Choice != reportidentity.ChoiceError {
		t.Fatalf("partial replay did not preserve originals and one error decision: %#v", retained)
	}
	assertClefReplayMatchesRetainedFixture(t, outputDir)
}

func TestClefProjectionAdapterPreservesReceiptFieldsOnError(t *testing.T) {
	request := clefRequestFromProjection("clef", "clef", reportidentityinference.Projection{
		State: []byte(`{"state":true}`), QuestionName: "identity",
		Question: reportidentityinference.Question{Instructions: "same projection question", Criteria: map[string]string{"a": "same", "b": "different", "c": "abstain"}},
	})
	total := int64(27)
	providerReceipt := cloudflareclef.Receipt{
		RequestedModel: "clef", ReturnedModel: "clef", RequestSHA256: "request-hash", ResponseSHA256: "response-hash", RequestID: "request-id",
		Usage: &cloudflareclef.Usage{InputTokens: 23, OutputTokens: 4, TotalTokens: &total}, Attempted: true, HTTPStatus: http.StatusOK,
		ContextStatus: "verified",
	}
	decider := clefProjectionDecider{client: &oneShotClefDecider{receipt: providerReceipt}, flags: identityInferenceFlags{model: "clef"}}
	got, err := decider.Decide(context.Background(), reportidentityinference.Projection{QuestionName: "identity"})
	if err == nil {
		t.Fatal("expected fake provider error")
	}
	if got.RequestSHA256 != providerReceipt.RequestSHA256 || got.ResponseSHA256 != providerReceipt.ResponseSHA256 || got.RequestID != providerReceipt.RequestID || got.Usage == nil || got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 27 || got.ContextStatus != "unverified" {
		t.Fatalf("provider error metadata mapping = %#v", got)
	}
	if request.ServiceTier != "" || request.Model != "clef" {
		t.Fatalf("request used non-Clef settings: %#v", request)
	}
}

type oneShotClefDecider struct {
	receipt cloudflareclef.Receipt
}

func (decider *oneShotClefDecider) Decide(context.Context, systemone.Request) (cloudflareclef.Receipt, error) {
	return decider.receipt, errors.New("synthetic provider failure")
}
