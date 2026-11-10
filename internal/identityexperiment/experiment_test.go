package identityexperiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
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

func priorInput(findingBodies, threadBodies []string, longPaths bool) reportidentity.Input {
	findings := make([]review.Finding, len(findingBodies))
	threads := make([]reportidentity.Thread, len(threadBodies))
	for index, body := range findingBodies {
		path := fmt.Sprintf("src/finding-%03d.go", index)
		if longPaths {
			path = strings.Repeat("p", 4096)
		}
		findings[index] = review.Finding{
			ID: review.FindingID(fmt.Sprintf("f%03d", index)), Severity: review.SeverityMajor,
			FilePath: path, Anchor: review.Anchor{Kind: review.AnchorKindFile}, Body: body,
		}
	}
	for index, body := range threadBodies {
		path := fmt.Sprintf("src/thread-%03d.go", index)
		if longPaths {
			path = strings.Repeat("t", 4096)
		}
		threads[index] = reportidentity.Thread{
			ID: fmt.Sprintf("thread-%03d", index), Eligible: true, FilePath: path, Body: body,
		}
	}
	return reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskPriorThread,
		Findings:      findings,
		Threads:       threads,
		Decisions:     []reportidentity.Decision{},
	}
}

func marshalFixture(t *testing.T, input reportidentity.Input) []byte {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return data
}

func expectedBoundaryFixture(input reportidentity.Input, replay reportidentity.Result) reportidentity.Input {
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
	}
}

func encodeExpectedBoundaryFixture(input reportidentity.Input) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(input); err != nil {
		return nil, err
	}
	return append([]byte(nil), bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})...), nil
}

func priorThreadBoundaryInput(t *testing.T, targetBytes int) ([]byte, []byte) {
	t.Helper()
	findingBodies := make([]string, 100)
	threadBodies := make([]string, 100)
	for index := range findingBodies {
		findingBodies[index] = "x"
		threadBodies[index] = "x"
	}
	input := priorInput(findingBodies, threadBodies, false)
	replay, err := reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("Replay minimal fixture: %v", err)
	}
	baseBytes, err := encodeExpectedBoundaryFixture(expectedBoundaryFixture(input, replay))
	if err != nil {
		t.Fatalf("encode expected base fixture: %v", err)
	}
	if len(baseBytes) >= targetBytes {
		t.Fatalf("base expected fixture size = %d, want below target %d", len(baseBytes), targetBytes)
	}

	extraBytes := targetBytes - len(baseBytes)
	recordCount := len(findingBodies) + len(threadBodies)
	baseExtra, remainder := extraBytes/recordCount, extraBytes%recordCount
	if 1+baseExtra > 16*1024 {
		t.Fatalf("boundary body length %d exceeds schema bound", 1+baseExtra)
	}
	for index := range findingBodies {
		extra := baseExtra
		if index < remainder {
			extra++
		}
		findingBodies[index] = strings.Repeat("x", 1+extra)
	}
	for index := range threadBodies {
		extra := baseExtra
		if len(findingBodies)+index < remainder {
			extra++
		}
		threadBodies[index] = strings.Repeat("x", 1+extra)
	}

	input = priorInput(findingBodies, threadBodies, false)
	replay, err = reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("Replay boundary fixture: %v", err)
	}
	expectedBytes, err := encodeExpectedBoundaryFixture(expectedBoundaryFixture(input, replay))
	if err != nil || len(expectedBytes) != targetBytes {
		t.Fatalf("independent expected fixture bytes=%d err=%v, want %d", len(expectedBytes), err, targetBytes)
	}
	inputJSON := marshalFixture(t, input)
	if len(inputJSON) >= reportidentity.MaxFixtureBytes {
		t.Fatalf("original source fixture size = %d, want below limit %d", len(inputJSON), reportidentity.MaxFixtureBytes)
	}
	if _, err := reportidentity.Decode(inputJSON); err != nil {
		t.Fatalf("Decode original boundary fixture: %v", err)
	}
	return inputJSON, expectedBytes
}

func testConfiguration() Configuration {
	return Configuration{
		Task: "report_identity", CredentialStore: "local-os", CredentialName: "codereview/identity-test",
		CredentialKey: "system_one_api_key", RequestedModel: "model/test",
		ExpectedModel: "model/test", MaxCalls: 100, MaxStateBytes: 6144, MaxRequestBytes: 8192,
	}
}

func testDependencies() Dependencies {
	return Dependencies{
		ResolveAPIKey: func(context.Context) (string, error) { return "synthetic-key", nil },
		Now:           time.Now,
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
		ResolveAPIKey: func(context.Context) (string, error) {
			t.Fatal("zero-pair run looked up a key")
			return "", nil
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil || result.Receipts.ExpectedPairs != 0 || result.Receipts.ProviderCalls != 0 || result.Receipts.EstimatedCostUSD == nil || *result.Receipts.EstimatedCostUSD != 0 {
		t.Fatalf("zero-pair result = %#v err=%v", result.Receipts, result.Err())
	}
}

func TestExecuteStopsAfterResolverCancelsContext(t *testing.T) {
	inputJSON := fixtureBytes(t, 3)
	prepared, err := Prepare(inputJSON, testConfiguration(), runnerTestBackend(&testDecider{}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	decider := &testDecider{}
	backend := runnerTestBackend(decider)
	clientOpens := 0
	backend.NewClient = func(string) (Decider, error) {
		clientOpens++
		return decider, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyResolutions := 0
	result := Execute(ctx, prepared, Dependencies{
		ResolveAPIKey: func(context.Context) (string, error) {
			keyResolutions++
			cancel()
			return "synthetic-key", nil
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil {
		t.Fatalf("Execute: %v", result.Err())
	}
	if keyResolutions != 1 || clientOpens != 0 || decider.calls != 0 {
		t.Fatalf("resolver cancellation used later stages: resolutions=%d clients=%d decisions=%d", keyResolutions, clientOpens, decider.calls)
	}
	const expectedCanceledReceiptCode = "cancelled" //nolint:misspell // Stable public error code is contract-defined.
	if result.Receipts.Status != "error" || result.Receipts.ExpectedPairs != 3 || result.Receipts.MissingPairs != 3 ||
		result.Receipts.ProviderCalls != 0 || len(result.Receipts.PreflightErrors) != 1 || result.Receipts.PreflightErrors[0].ErrorCode != expectedCanceledReceiptCode {
		t.Fatalf("canceled preflight receipt = %#v", result.Receipts)
	}
	retained, err := reportidentity.Decode(result.fixtureJSON)
	if err != nil {
		t.Fatalf("Decode retained originals: %v", err)
	}
	if !bytes.Equal(result.fixtureJSON, inputJSON) || !reflect.DeepEqual(retained.Findings, result.Fixture.Findings) || len(retained.Decisions) != 0 {
		t.Fatalf("canceled run did not retain original no-decision fixture")
	}
	replay, err := reportidentity.Replay(retained)
	if err != nil || len(replay.Pairs) != 3 {
		t.Fatalf("Replay retained originals: pairs=%d err=%v", len(replay.Pairs), err)
	}
	for _, replayPair := range replay.Pairs {
		if replayPair.Choice != reportidentity.ChoiceMissing {
			t.Fatalf("canceled run retained pair choice %q, want missing", replayPair.Choice)
		}
	}
}

func TestPrepareRetainsReplayableOriginalWhenFullPriorFixtureExceedsLimit(t *testing.T) {
	body := strings.Repeat("x", 16*1024)
	input := priorInput(make([]string, 100), make([]string, 100), false)
	for index := range input.Findings {
		input.Findings[index].Body = body
	}
	for index := range input.Threads {
		input.Threads[index].Body = body
	}
	inputJSON := marshalFixture(t, input)
	if len(inputJSON) >= reportidentity.MaxFixtureBytes {
		t.Fatalf("source fixture size = %d, want below replay limit %d", len(inputJSON), reportidentity.MaxFixtureBytes)
	}
	plans, clients := 0, 0
	backend := Backend{
		Prepare: func(projection reportidentityinference.Projection) (Plan, error) {
			plans++
			return Plan{RequestSHA256: SHA256(projection.State), StateBytes: len(projection.State), RequestBytes: len(projection.State)}, nil
		},
		NewClient:    func(string) (Decider, error) { clients++; return nil, nil },
		ErrorCode:    func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 { return nil },
	}
	configuration := testConfiguration()
	configuration.MaxCalls = 10_000
	prepared, err := Prepare(inputJSON, configuration, backend)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.ExpectedPairs() != 10_000 || prepared.TotalRequestBytes() != 0 || plans != 0 {
		t.Fatalf("oversize preflight plans=%d pairs=%d bytes=%d", plans, prepared.ExpectedPairs(), prepared.TotalRequestBytes())
	}
	preflight := prepared.PreflightErrors()
	if len(preflight) != 1 || preflight[0].ErrorCode != "retained_fixture_too_large" {
		t.Fatalf("retained-size preflight = %#v", preflight)
	}
	result := Execute(context.Background(), prepared, Dependencies{
		ResolveAPIKey: func(context.Context) (string, error) {
			t.Fatal("oversize retained fixture resolved a credential")
			return "", nil
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil || clients != 0 || result.Receipts.ExpectedPairs != 10_000 || result.Receipts.MissingPairs != 10_000 || result.Receipts.ProviderCalls != 0 {
		t.Fatalf("oversize execution = receipts:%#v clients:%d err:%v", result.Receipts, clients, result.Err())
	}
	if !bytes.Equal(result.fixtureJSON, inputJSON) {
		t.Fatal("oversize preflight did not retain the exact original fixture bytes")
	}
	retained, err := reportidentity.Decode(result.fixtureJSON)
	if err != nil {
		t.Fatalf("retained original does not Decode: %v", err)
	}
	replay, err := reportidentity.Replay(retained)
	if err != nil || len(replay.Pairs) != 10_000 {
		t.Fatalf("retained original replay pairs=%d err=%v", len(replay.Pairs), err)
	}
	for _, replayPair := range replay.Pairs {
		if replayPair.Choice != reportidentity.ChoiceMissing {
			t.Fatalf("oversize retained pair has choice %q, want missing", replayPair.Choice)
		}
	}
}

func TestPrepareKeepsZeroPairHTMLFixtureCompactAndReplayable(t *testing.T) {
	bodies := make([]string, 100)
	for index := range bodies {
		bodies[index] = strings.Repeat("<tag>", 3000)
	}
	input := priorInput(bodies, []string{}, false)
	inputJSON, err := encodeFixture(input)
	if err != nil {
		t.Fatalf("encode source fixture: %v", err)
	}
	escapedInput, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal escaped comparison fixture: %v", err)
	}
	if len(inputJSON) >= reportidentity.MaxFixtureBytes || len(escapedInput) <= reportidentity.MaxFixtureBytes {
		t.Fatalf("HTML source/escaped sizes = %d/%d, want below/above %d", len(inputJSON), len(escapedInput), reportidentity.MaxFixtureBytes)
	}
	plans := 0
	backend := Backend{
		Prepare: func(reportidentityinference.Projection) (Plan, error) {
			plans++
			return Plan{}, nil
		},
		NewClient: func(string) (Decider, error) {
			t.Fatal("zero-pair run constructed a client")
			return nil, nil
		},
		ErrorCode:    func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 { return nil },
	}
	configuration := testConfiguration()
	configuration.MaxCalls = 0
	prepared, err := Prepare(inputJSON, configuration, backend)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(prepared.PreflightErrors()) != 0 || prepared.ExpectedPairs() != 0 || plans != 0 {
		t.Fatalf("zero-pair HTML preflight=%#v pairs=%d plans=%d", prepared.PreflightErrors(), prepared.ExpectedPairs(), plans)
	}
	result := Execute(context.Background(), prepared, Dependencies{
		ResolveAPIKey: func(context.Context) (string, error) {
			t.Fatal("zero-pair HTML fixture resolved a credential")
			return "", nil
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil || !bytes.Equal(result.fixtureJSON, inputJSON) {
		t.Fatalf("zero-pair HTML output err=%v bytes_match=%t", result.Err(), bytes.Equal(result.fixtureJSON, inputJSON))
	}
	if _, err := reportidentity.Decode(result.fixtureJSON); err != nil {
		t.Fatalf("compact HTML fixture does not Decode: %v", err)
	}
}

func TestPrepareAcceptsExactRetainedFixtureBoundary(t *testing.T) {
	inputJSON, expectedBytes := priorThreadBoundaryInput(t, reportidentity.MaxFixtureBytes)
	if len(expectedBytes) != reportidentity.MaxFixtureBytes {
		t.Fatalf("independent expected fixture bytes = %d, want %d", len(expectedBytes), reportidentity.MaxFixtureBytes)
	}
	plans := 0
	clients := 0
	backend := Backend{
		Prepare: func(projection reportidentityinference.Projection) (Plan, error) {
			plans++
			return Plan{RequestSHA256: strings.Repeat("a", 64), RequestBytes: len(projection.State)}, nil
		},
		NewClient: func(string) (Decider, error) {
			clients++
			return &testDecider{}, nil
		},
		ErrorCode:    func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 { return nil },
	}
	configuration := testConfiguration()
	configuration.MaxCalls = 10_000
	prepared, err := Prepare(inputJSON, configuration, backend)
	if err != nil {
		t.Fatalf("Prepare exact-boundary fixture: %v", err)
	}
	if preflight := prepared.PreflightErrors(); len(preflight) != 0 || prepared.ExpectedPairs() != 10_000 || plans != 10_000 || clients != 0 {
		t.Fatalf("exact-boundary preparation preflight=%#v pairs=%d plans=%d clients=%d", preflight, prepared.ExpectedPairs(), plans, clients)
	}
}

func TestPrepareRefusesCanonicalUnicodeExpansionBeforeRequestPlanning(t *testing.T) {
	body := "x" + strings.Repeat("\u2028", 5461)
	input := priorInput(make([]string, 100), make([]string, 100), true)
	for index := range input.Findings {
		input.Findings[index].Body = body
	}
	for index := range input.Threads {
		input.Threads[index].Body = "root"
	}
	inputJSON := marshalFixture(t, input)
	inputJSON = []byte(strings.ReplaceAll(string(inputJSON), `\u2028`, "\u2028"))
	if len(inputJSON) >= reportidentity.MaxFixtureBytes {
		t.Fatalf("valid Unicode source size = %d, want below %d", len(inputJSON), reportidentity.MaxFixtureBytes)
	}
	plans := 0
	backend := Backend{
		Prepare: func(reportidentityinference.Projection) (Plan, error) {
			plans++
			return Plan{RequestSHA256: strings.Repeat("a", 64), RequestBytes: 1}, nil
		},
		NewClient:    func(string) (Decider, error) { return nil, errors.New("unused") },
		ErrorCode:    func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 { return nil },
	}
	configuration := testConfiguration()
	configuration.MaxCalls = 10_000
	prepared, err := Prepare(inputJSON, configuration, backend)
	if err != nil {
		t.Fatalf("Prepare Unicode fixture: %v", err)
	}
	if preflight := prepared.PreflightErrors(); len(preflight) != 1 || preflight[0].ErrorCode != "retained_fixture_too_large" || plans != 0 {
		t.Fatalf("Unicode expansion preflight=%#v plans=%d", preflight, plans)
	}
}

func TestPrepareRefusesExactOneByteOverRetainedFixtureBoundary(t *testing.T) {
	inputJSON, expectedBytes := priorThreadBoundaryInput(t, reportidentity.MaxFixtureBytes+1)
	if len(expectedBytes) != reportidentity.MaxFixtureBytes+1 {
		t.Fatalf("independent expected fixture bytes = %d, want %d", len(expectedBytes), reportidentity.MaxFixtureBytes+1)
	}
	backendPlans, clients, keyResolutions := 0, 0, 0
	backend := Backend{
		Prepare: func(reportidentityinference.Projection) (Plan, error) {
			backendPlans++
			return Plan{RequestSHA256: strings.Repeat("a", 64), RequestBytes: 1}, nil
		},
		NewClient: func(string) (Decider, error) {
			clients++
			return &testDecider{}, nil
		},
		ErrorCode:    func(error) string { return "transport" },
		EstimateCost: func(*Usage, *Configuration) *float64 { return nil },
	}
	configuration := testConfiguration()
	configuration.MaxCalls = 10_000
	prepared, err := Prepare(inputJSON, configuration, backend)
	if err != nil {
		t.Fatalf("Prepare one-over-boundary fixture: %v", err)
	}
	if preflight := prepared.PreflightErrors(); len(preflight) != 1 || preflight[0].ErrorCode != "retained_fixture_too_large" || prepared.ExpectedPairs() != 10_000 || backendPlans != 0 {
		t.Fatalf("one-over-boundary preflight=%#v pairs=%d plans=%d", preflight, prepared.ExpectedPairs(), backendPlans)
	}
	result := Execute(context.Background(), prepared, Dependencies{
		ResolveAPIKey: func(context.Context) (string, error) {
			keyResolutions++
			return "synthetic-key", nil
		},
		Now: time.Now,
	}, backend)
	if result.Err() != nil || keyResolutions != 0 || backendPlans != 0 || clients != 0 || result.Receipts.ProviderCalls != 0 || result.Receipts.MissingPairs != 10_000 {
		t.Fatalf("one-over-boundary execution err=%v keys=%d plans=%d clients=%d receipts=%#v", result.Err(), keyResolutions, backendPlans, clients, result.Receipts)
	}
	if !bytes.Equal(result.fixtureJSON, inputJSON) {
		t.Fatal("one-over-boundary preflight did not retain the exact original fixture bytes")
	}
	retained, err := reportidentity.Decode(result.fixtureJSON)
	if err != nil || len(retained.Decisions) != 0 {
		t.Fatalf("retained original fixture decisions=%d err=%v", len(retained.Decisions), err)
	}
	replay, err := reportidentity.Replay(retained)
	if err != nil || len(replay.Pairs) != 10_000 {
		t.Fatalf("retained original replay pairs=%d err=%v", len(replay.Pairs), err)
	}
	for _, replayPair := range replay.Pairs {
		if replayPair.Choice != reportidentity.ChoiceMissing {
			t.Fatalf("retained original pair choice = %q, want missing", replayPair.Choice)
		}
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
