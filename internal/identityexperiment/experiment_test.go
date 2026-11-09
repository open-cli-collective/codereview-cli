package identityexperiment

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type testDecider struct {
	calls  int
	failAt int
}

func (decider *testDecider) Decide(_ context.Context, projection reportidentityinference.Projection) (Receipt, error) {
	decider.calls++
	receipt := Receipt{
		RequestedModel: "model/test", ReturnedModel: "model/test",
		RequestSHA256: SHA256(projection.State), Attempted: true,
		Usage: &Usage{InputTokens: 10, OutputTokens: 2},
	}
	if decider.calls == decider.failAt {
		return receipt, errors.New("synthetic transport failure")
	}
	receipt.Answer = &reportidentityinference.ChoiceAnswer{
		Choice: "a", Confidence: 0.9,
		Probabilities: map[string]float64{"a": 0.9, "b": 0.08, "c": 0.02},
	}
	return receipt, nil
}

func runnerTestBackend(decider *testDecider) Backend {
	return Backend{
		Prepare: func(projection reportidentityinference.Projection) (Plan, error) {
			return Plan{
				RequestSHA256: SHA256(projection.State),
				StateBytes:    len(projection.State),
				RequestBytes:  len(projection.State) + 7,
			}, nil
		},
		NewClient: func(string) (Decider, error) { return decider, nil },
		ErrorCode: func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 {
			return nil
		},
		Provider: "test",
	}
}

func fixtureBytes(t *testing.T, findingCount int) []byte {
	t.Helper()
	findings := make([]review.Finding, 0, findingCount)
	for index := 0; index < findingCount; index++ {
		findings = append(findings, review.Finding{
			ID: review.FindingID("f" + string(rune('1'+index))), Severity: review.SeverityMajor,
			FilePath: "src/example.go", Anchor: review.Anchor{Kind: review.AnchorKindFile},
			Body: "Synthetic finding body.",
		})
	}
	input := reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskCohort,
		Findings:      findings,
		Threads:       []reportidentity.Thread{},
		Decisions:     []reportidentity.Decision{},
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return data
}

func testConfiguration() Configuration {
	return Configuration{
		Task: "report_identity", APIKeyEnv: "IDENTITY_TEST_KEY", RequestedModel: "model/test",
		ExpectedModel: "model/test", MaxCalls: 100, MaxStateBytes: 6144, MaxRequestBytes: 8192,
	}
}

func testDependencies() Dependencies {
	return Dependencies{
		LookupAPIKey: func(string) (string, bool) { return "synthetic-key", true },
		Now:          time.Now,
	}
}

func TestPrepareRetainsFixtureAndReturnsDefensivePreflightErrors(t *testing.T) {
	original := fixtureBytes(t, 3)
	decider := &testDecider{}
	configuration := testConfiguration()
	configuration.MaxCalls = 1
	prepared, err := Prepare(original, configuration, runnerTestBackend(decider))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.ExpectedPairs() != 3 || prepared.TotalRequestBytes() == 0 {
		t.Fatalf("prepared counts = pairs:%d request_bytes:%d", prepared.ExpectedPairs(), prepared.TotalRequestBytes())
	}
	original[0] = 'x'
	preflight := prepared.PreflightErrors()
	if len(preflight) != 1 || preflight[0].ErrorCode != "invalid_input" {
		t.Fatalf("preflight errors = %#v", preflight)
	}
	preflight[0].ErrorCode = "changed"
	withExtra := prepared.WithPreflightError("budget_exceeded")
	if got := prepared.PreflightErrors(); len(got) != 1 || got[0].ErrorCode != "invalid_input" {
		t.Fatalf("original prepared state changed: %#v", got)
	}
	if got := withExtra.PreflightErrors(); len(got) != 2 || got[1].ErrorCode != "budget_exceeded" {
		t.Fatalf("copied prepared state = %#v", got)
	}
	result := Execute(context.Background(), prepared, testDependencies(), runnerTestBackend(decider))
	if result.Err() != nil {
		t.Fatalf("Execute: %v", result.Err())
	}
	if result.Receipts.InputSHA256 != SHA256(fixtureBytes(t, 3)) || len(result.Fixture.Findings) != 3 {
		t.Fatalf("retained fixture identity changed: receipts=%#v findings=%d", result.Receipts, len(result.Fixture.Findings))
	}
	if result.Receipts.ProtocolVersion != reportidentityinference.ProtocolVersion {
		t.Fatalf("receipt protocol_version = %d, want %d", result.Receipts.ProtocolVersion, reportidentityinference.ProtocolVersion)
	}
}

func TestExecuteZeroPairsDoesNotLookUpKeyOrConstructClient(t *testing.T) {
	configuration := testConfiguration()
	prepared, err := Prepare(fixtureBytes(t, 0), configuration, runnerTestBackend(&testDecider{}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	backend := runnerTestBackend(&testDecider{})
	backend.NewClient = func(string) (Decider, error) {
		t.Fatal("zero-pair run constructed a client")
		return nil, nil
	}
	result := Execute(context.Background(), prepared, Dependencies{
		LookupAPIKey: func(string) (string, bool) {
			t.Fatal("zero-pair run looked up a key")
			return "", false
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil || result.Receipts.ExpectedPairs != 0 || result.Receipts.ProviderCalls != 0 || result.Receipts.EstimatedCostUSD == nil || *result.Receipts.EstimatedCostUSD != 0 {
		t.Fatalf("zero-pair result = %#v err=%v", result.Receipts, result.Err())
	}
}

func TestExecuteStopsAtFirstErrorAndRetainsMissingPairCount(t *testing.T) {
	decider := &testDecider{failAt: 2}
	backend := runnerTestBackend(decider)
	prepared, err := Prepare(fixtureBytes(t, 3), testConfiguration(), backend)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	result := Execute(context.Background(), prepared, testDependencies(), backend)
	if result.Err() != nil {
		t.Fatalf("Execute: %v", result.Err())
	}
	if decider.calls != 2 || result.Receipts.AttemptedPairs != 2 || result.Receipts.ProviderCalls != 2 ||
		result.Receipts.CompletedPairs != 1 || result.Receipts.FailedPairs != 1 || result.Receipts.MissingPairs != 1 {
		t.Fatalf("fail-fast receipt accounting = %#v calls=%d", result.Receipts, decider.calls)
	}
	if result.Receipts.Attempts[1].ErrorCode != "transport" || result.Receipts.Attempts[1].Usage == nil {
		t.Fatalf("failed attempt did not retain bounded metadata: %#v", result.Receipts.Attempts[1])
	}
}

func TestAttemptRejectsNegativeTotalUsageMetadata(t *testing.T) {
	negative := int64(-1)
	configuration := &Configuration{RequestedModel: "model/test", ExpectedModel: "model/test"}
	preparedPair := pair{replayPair: reportidentity.Pair{LeftID: "f1", RightID: "f2", InputSHA256: strings.Repeat("a", 64)}, plan: Plan{RequestSHA256: "planned-hash"}}
	receipt := Receipt{
		RequestedModel: "model/test", ReturnedModel: "model/test", RequestSHA256: "planned-hash", Attempted: true,
		Answer: &reportidentityinference.ChoiceAnswer{Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.9, "b": 0.08, "c": 0.02}},
		Usage:  &Usage{InputTokens: 20, OutputTokens: 2, TotalTokens: &negative},
	}
	backend := Backend{ErrorCode: func(error) string { return "transport" }}
	attempt, _, code := makeAttempt(preparedPair, receipt, nil, time.Now(), nil, configuration, backend)
	if code != "malformed_response" || attempt.Usage != nil {
		t.Fatalf("negative total metadata was trusted: attempt=%#v code=%q", attempt, code)
	}
}

func TestEstimateCostPreservesUnknownZeroAndFiniteSemantics(t *testing.T) {
	configuration := Configuration{}
	usage := &Usage{InputTokens: 100, OutputTokens: 20}
	if EstimateCost(usage, configuration) != nil {
		t.Fatal("unset price pair produced an estimate")
	}
	zero := 0.0
	configuration.InputPricePerMillion = &zero
	configuration.OutputPricePerMillion = &zero
	if got := EstimateCost(usage, configuration); got == nil || *got != 0 {
		t.Fatalf("explicit zero prices = %#v", got)
	}
	inputPrice, outputPrice := 2.0, 5.0
	configuration.InputPricePerMillion = &inputPrice
	configuration.OutputPricePerMillion = &outputPrice
	if got := EstimateCost(usage, configuration); got == nil || math.Abs(*got-0.0003) > 1e-12 {
		t.Fatalf("positive prices estimate = %#v", got)
	}
	if EstimateCost(nil, configuration) != nil {
		t.Fatal("missing usage produced an estimate")
	}
	maxFloat := math.MaxFloat64
	configuration.InputPricePerMillion = &maxFloat
	configuration.OutputPricePerMillion = &maxFloat
	if EstimateCost(&Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}, configuration) != nil {
		t.Fatal("nonfinite cost estimate was returned")
	}
}
