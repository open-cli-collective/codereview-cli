package benchmarkcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
)

const identityClefSuiteYAML = `task: report_identity
fixture: ./fixture.json
providers:
  cloudflare:
    kind: cloudflare_clef
    account_id: account123
    api_key_env: CLOUDFLARE_AUTH_TOKEN
candidates:
  clef:
    provider: cloudflare
    model: clef
  flash:
    provider: cloudflare
    model: clef-flash
`

func TestIdentityClefRunDirectAndCandidateSelectorsUseTheSharedRunner(t *testing.T) {
	for _, test := range []struct {
		name        string
		selector    []string
		model       string
		candidateID *string
	}{
		{name: "direct clef", selector: []string{"--provider", "cloudflare", "--model", "clef"}, model: "clef"},
		{name: "direct clef-flash", selector: []string{"--provider", "cloudflare", "--model", "clef-flash"}, model: "clef-flash"},
		{name: "candidate clef", selector: []string{"--candidate", "clef"}, model: "clef", candidateID: identityClefStringPtr("clef")},
		{name: "candidate clef-flash", selector: []string{"--candidate", "flash"}, model: "clef-flash", candidateID: identityClefStringPtr("flash")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input := clefCohortInput(t, 2)
			suitePath := writeIdentityClefSuite(t, dir, input, []byte(identityClefSuiteYAML))
			outputDir := filepath.Join(dir, "output")
			state, runtimeCalls := setIdentityClefRuntimeForTest(t, nil, true)

			args := append([]string{"benchmark", "run", suitePath}, test.selector...)
			args = append(args, "--max-calls", "10", "--results-dir", outputDir, "--allow-unverified-context")
			cmd, stdout, _, _ := newReplayCommandForTest(t, nil)
			if err := root.Execute(cmd, args); err != nil {
				t.Fatalf("execute benchmark run: %v", err)
			}

			var summary identityInferenceSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("decode summary: %v; stdout=%s", err, stdout.String())
			}
			var receipts identityInferenceReceipts
			if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
				t.Fatalf("decode receipts: %v", err)
			}
			configuration := receipts.Configuration
			wantEndpoint, err := cloudflareclef.Endpoint(clefTestAccountID, test.model)
			if err != nil {
				t.Fatalf("resolve expected Clef endpoint: %v", err)
			}
			if !reflect.DeepEqual(summary.Configuration, configuration) {
				t.Fatalf("summary configuration %#v differs from receipts %#v", summary.Configuration, configuration)
			}
			if configuration.Task != identitySuiteTask || configuration.SuitePath != suitePath || configuration.SuiteSHA256 != sha256Hex([]byte(identityClefSuiteYAML)) ||
				(configuration.CandidateID == nil) != (test.candidateID == nil) || configuration.ProviderName != "cloudflare" ||
				configuration.AdapterKind != "cloudflare_clef" || configuration.Provider != "cloudflare_clef" ||
				configuration.Endpoint != wantEndpoint || configuration.RequestedModel != test.model || configuration.ExpectedModel != test.model ||
				configuration.APIKeyEnv != "CLOUDFLARE_AUTH_TOKEN" || configuration.AuthScheme != "Bearer" || configuration.ServiceTier != nil ||
				configuration.ContextStatus != "unverified" || configuration.TimeoutMS != 30_000 || configuration.MaxCalls != 10 {
				t.Fatalf("resolved Clef configuration = %#v", configuration)
			}
			if !reflect.DeepEqual(configuration.CandidateID, test.candidateID) {
				t.Fatalf("candidate ID = %#v, want %#v", configuration.CandidateID, test.candidateID)
			}
			if *runtimeCalls != 1 || state.keyLookups != 1 || !reflect.DeepEqual(state.keyNames, []string{"CLOUDFLARE_AUTH_TOKEN"}) || state.clientOpens != 1 || len(state.client.requests) != 1 {
				t.Fatalf("fake runtime/provider counts = runtimes:%d lookups:%d names:%v clients:%d requests:%d", *runtimeCalls, state.keyLookups, state.keyNames, state.clientOpens, len(state.client.requests))
			}
			if state.clientOpts.AccountID != clefTestAccountID || state.clientOpts.APIKey != clefTestAPIKey {
				t.Fatalf("Cloudflare client options = %#v", state.clientOpts)
			}
			if len(receipts.Attempts) != 1 || receipts.Attempts[0].ContextStatus != "unverified" || !receipts.Attempts[0].HTTPAttempted || receipts.Attempts[0].RequestedModel != test.model || receipts.Attempts[0].ReturnedModel != test.model {
				t.Fatalf("Clef attempt metadata = %#v", receipts.Attempts)
			}
			if !state.client.claimedVerifiedContext {
				t.Fatal("fake provider did not return the deliberately conflicting verified status")
			}
			assertClefReplayMatchesRetainedFixture(t, outputDir)
			assertClefOutputMode(t, outputDir, 0o700)
			for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
				assertClefOutputMode(t, filepath.Join(outputDir, name), 0o600)
			}
			var manifest identityInferenceManifest
			if err := json.Unmarshal(readClefOutput(t, outputDir, "manifest.json"), &manifest); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			if manifest.Files.ReceiptsSHA256 != sha256Hex(readClefOutput(t, outputDir, "receipts.json")) {
				t.Fatalf("manifest receipt hash = %q", manifest.Files.ReceiptsSHA256)
			}
			if bytes.Contains(readClefOutput(t, outputDir, "receipts.json"), []byte(clefTestAPIKey)) {
				t.Fatal("API key appeared in retained receipts")
			}
			var retained reportidentity.Input
			if err := json.Unmarshal(readClefOutput(t, outputDir, "fixture.json"), &retained); err != nil {
				t.Fatalf("decode retained fixture: %v", err)
			}
			if !reflect.DeepEqual(retained.Findings, input.Findings) || !reflect.DeepEqual(retained.Threads, input.Threads) || len(retained.Decisions) != 1 {
				t.Fatalf("retained originals or decision count changed: %#v", retained)
			}
		})
	}
}

func TestIdentityClefRunRequiresContextAcknowledgementBeforeRuntimeEvenForEmptyFixture(t *testing.T) {
	dir := t.TempDir()
	input := clefCohortInput(t, 0)
	suitePath := writeIdentityClefSuite(t, dir, input, []byte(identityClefSuiteYAML))
	tests := []struct {
		name string
		flag []string
	}{
		{name: "missing acknowledgement"},
		{name: "explicit false acknowledgement", flag: []string{"--allow-unverified-context=false"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, runtimeCalls := setIdentityClefRuntimeForTest(t, nil, true)
			outputDir := filepath.Join(dir, fmt.Sprintf("output-%d", index))
			args := []string{"benchmark", "run", suitePath, "--provider", "cloudflare", "--model", "clef", "--max-calls", "0", "--results-dir", outputDir}
			args = append(args, test.flag...)
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			if err := root.Execute(cmd, args); err == nil {
				t.Fatal("Clef run succeeded without acknowledging unverified context")
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing acknowledgement created output or stat failed: %v", err)
			}
			if *runtimeCalls != 0 || state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("missing acknowledgement reached runtime/provider: runtimes=%d lookups=%d clients=%d", *runtimeCalls, state.keyLookups, state.clientOpens)
			}
		})
	}
}

func TestIdentityClefSuiteRejectsGenericProviderFields(t *testing.T) {
	for _, field := range []struct{ name, value string }{
		{name: "endpoint", value: "https://example.invalid/v1/clef"},
		{name: "auth_scheme", value: "bearer"},
		{name: "service_tier", value: "priority"},
	} {
		t.Run(field.name, func(t *testing.T) {
			data := strings.Replace(identityClefSuiteYAML, "    api_key_env: CLOUDFLARE_AUTH_TOKEN", "    api_key_env: CLOUDFLARE_AUTH_TOKEN\n    "+field.name+": "+field.value, 1)
			if _, err := parseIdentitySuite([]byte(data)); err == nil {
				t.Fatalf("identity suite accepted Clef provider field %q", field.name)
			}
		})
	}
}

func TestIdentityClefInvalidAccountAndAnyInvalidCandidateModelFailBeforeCredentials(t *testing.T) {
	validFixture := clefCohortInput(t, 0)
	tests := []struct {
		name     string
		suite    string
		selector []string
	}{
		{
			name:     "invalid account ID",
			suite:    strings.Replace(identityClefSuiteYAML, "account_id: account123", "account_id: ../account", 1),
			selector: []string{"--provider", "cloudflare", "--model", "clef"},
		},
		{
			name:     "invalid selected model",
			suite:    identityClefSuiteYAML,
			selector: []string{"--provider", "cloudflare", "--model", "@cf/cloudflare/clef"},
		},
		{
			name:     "invalid model in unselected candidate",
			suite:    strings.Replace(identityClefSuiteYAML, "    model: clef-flash", "    model: '@cf/cloudflare/clef'", 1),
			selector: []string{"--candidate", "clef"},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			suitePath := writeIdentityClefSuite(t, dir, validFixture, []byte(test.suite))
			state, runtimeCalls := setIdentityClefRuntimeForTest(t, nil, true)
			outputDir := filepath.Join(dir, "output")
			args := append([]string{"benchmark", "run", suitePath}, test.selector...)
			args = append(args, "--max-calls", "0", "--results-dir", outputDir, "--allow-unverified-context")
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			if err := root.Execute(cmd, args); err == nil {
				t.Fatal("invalid account/model configuration succeeded")
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid account/model created output or stat failed: %v", err)
			}
			if state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("invalid account/model touched credentials/client: lookups=%d clients=%d", state.keyLookups, state.clientOpens)
			}
			if index != 1 && *runtimeCalls != 0 {
				t.Fatalf("invalid account/candidate model reached runtime %d times", *runtimeCalls)
			}
		})
	}
}

func TestIdentityClefRunEmptyAndIncompleteBudgetPreflightsAvoidCredentials(t *testing.T) {
	for _, test := range []struct {
		name          string
		findingCount  int
		maxCalls      int
		wantSucceeded bool
		wantPairs     int
	}{
		{name: "empty fixture", findingCount: 0, maxCalls: 0, wantSucceeded: true},
		{name: "incomplete budget", findingCount: 3, maxCalls: 0, wantPairs: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input := clefCohortInput(t, test.findingCount)
			suitePath := writeIdentityClefSuite(t, dir, input, []byte(identityClefSuiteYAML))
			state, runtimeCalls := setIdentityClefRuntimeForTest(t, nil, true)
			outputDir := filepath.Join(dir, "output")
			args := []string{"benchmark", "run", suitePath, "--provider", "cloudflare", "--model", "clef", "--max-calls", fmt.Sprint(test.maxCalls), "--results-dir", outputDir, "--allow-unverified-context"}
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			err := root.Execute(cmd, args)
			if test.wantSucceeded && err != nil {
				t.Fatalf("empty run: %v", err)
			}
			if !test.wantSucceeded && err == nil {
				t.Fatal("incomplete pair budget unexpectedly succeeded")
			}
			if *runtimeCalls != 1 || state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("preflight used credentials/client: runtimes=%d lookups=%d clients=%d", *runtimeCalls, state.keyLookups, state.clientOpens)
			}
			var receipts identityInferenceReceipts
			if err := json.Unmarshal(readClefOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
				t.Fatalf("decode preflight receipts: %v", err)
			}
			if receipts.ProviderCalls != 0 || receipts.ExpectedPairs != test.wantPairs {
				t.Fatalf("preflight accounting = calls:%d pairs:%d, want 0/%d", receipts.ProviderCalls, receipts.ExpectedPairs, test.wantPairs)
			}
			if test.wantPairs > 0 && len(receipts.PreflightErrors) == 0 {
				t.Fatalf("budget preflight has no retained error: %#v", receipts.PreflightErrors)
			}
		})
	}
}

func setIdentityClefRuntimeForTest(t *testing.T, outcomes []clefFakeOutcome, keyPresent bool) (*clefCommandTestState, *int) {
	t.Helper()
	dependencies, state := clefTestDependencies(t, outcomes, keyPresent)
	previous := identityProviderRuntimeFor
	runtimeCalls := 0
	identityProviderRuntimeFor = func(provider identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
		runtimeCalls++
		return identityClefProviderRuntime(provider, flags, dependencies)
	}
	t.Cleanup(func() { identityProviderRuntimeFor = previous })
	return state, &runtimeCalls
}

func writeIdentityClefSuite(t *testing.T, directory string, input reportidentity.Input, suite []byte) string {
	t.Helper()
	writeClefFixture(t, directory, input)
	path := filepath.Join(directory, "identity.yml")
	if err := os.WriteFile(path, suite, 0o600); err != nil {
		t.Fatalf("write identity suite: %v", err)
	}
	return path
}

func identityClefStringPtr(value string) *string { return &value }
