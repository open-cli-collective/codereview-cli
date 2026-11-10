package benchmarkcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const validIdentitySuiteYAML = `task: report_identity
fixture: ./fixture.json
providers:
  primary:
    kind: system_one
    endpoint: https://primary.example/v1/systemone
    credential:
      store: local-os
      name: codereview/primary
    auth_scheme: bearer
  secondary:
    kind: system_one
    endpoint: https://secondary.example/v1/systemone
    credential:
      store: local-os
      name: codereview/secondary
    auth_scheme: api-key
candidates:
  saved:
    provider: secondary
    model: model-candidate
    expected_model: model-resolved
`

const validOpenAIDecisionsSuiteYAML = `task: report_identity
fixture: ./fixture.json
providers:
  decisions:
    kind: openai_decisions
    endpoint: https://api.openai.com/v1/decisions
    credential:
      store: local-os
      name: codereview/openai
candidates:
  saved:
    provider: decisions
    model: gpt-6-luna
    expected_model: gpt-6-luna
`

func TestParseIdentitySuiteIsStrictAndAcceptsSameKindProviders(t *testing.T) {
	suite, err := parseIdentitySuite([]byte(validIdentitySuiteYAML))
	if err != nil {
		t.Fatalf("parse valid identity suite: %v", err)
	}
	if suite.Task != identitySuiteTask || len(suite.Providers) != 2 || len(suite.Candidates) != 1 {
		t.Fatalf("parsed identity suite = %#v", suite)
	}
	if suite.Providers["primary"].Kind != "system_one" || suite.Providers["secondary"].Kind != "system_one" {
		t.Fatalf("same-kind providers were not retained: %#v", suite.Providers)
	}
	if got := suite.Candidates["saved"]; got.Provider != "secondary" || got.Model != "model-candidate" || got.ExpectedModel != "model-resolved" {
		t.Fatalf("candidate = %#v", got)
	}
}

func TestParseIdentitySuiteRejectsInvalidSchema(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown root key", data: strings.Replace(validIdentitySuiteYAML, "fixture: ./fixture.json", "fixture: ./fixture.json\nextra: value", 1)},
		{name: "duplicate root key", data: strings.Replace(validIdentitySuiteYAML, "fixture: ./fixture.json", "task: report_identity\nfixture: ./fixture.json", 1)},
		{name: "null task", data: strings.Replace(validIdentitySuiteYAML, "task: report_identity", "task: null", 1)},
		{name: "numeric fixture", data: strings.Replace(validIdentitySuiteYAML, "fixture: ./fixture.json", "fixture: 4", 1)},
		{name: "null providers", data: strings.Replace(validIdentitySuiteYAML, "providers:\n  primary:", "providers: null\n  primary:", 1)},
		{name: "empty providers", data: strings.Replace(validIdentitySuiteYAML, "providers:\n  primary:\n    kind: system_one\n    endpoint: https://primary.example/v1/systemone\n    credential:\n      store: local-os\n      name: codereview/primary\n    auth_scheme: bearer\n  secondary:\n    kind: system_one\n    endpoint: https://secondary.example/v1/systemone\n    credential:\n      store: local-os\n      name: codereview/secondary\n    auth_scheme: api-key", "providers: {}", 1)},
		{name: "duplicate provider name", data: strings.Replace(validIdentitySuiteYAML, "  secondary:\n    kind: system_one", "  primary:\n    kind: system_one\n    endpoint: https://duplicate.example/v1/systemone\n    credential:\n      store: local-os\n      name: codereview/duplicate\n    auth_scheme: bearer\n  secondary:\n    kind: system_one", 1)},
		{name: "unknown provider key", data: strings.Replace(validIdentitySuiteYAML, "auth_scheme: bearer", "auth_scheme: bearer\n    secret: inline", 1)},
		{name: "duplicate provider key", data: strings.Replace(validIdentitySuiteYAML, "kind: system_one\n    endpoint: https://primary.example", "kind: system_one\n    kind: system_one\n    endpoint: https://primary.example", 1)},
		{name: "null provider endpoint", data: strings.Replace(validIdentitySuiteYAML, "endpoint: https://primary.example/v1/systemone", "endpoint: null", 1)},
		{name: "unknown adapter kind", data: strings.Replace(validIdentitySuiteYAML, "kind: system_one", "kind: unknown_adapter", 1)},
		{name: "endpoint query", data: strings.Replace(validIdentitySuiteYAML, "https://primary.example/v1/systemone", "https://primary.example/v1/systemone?key=hidden", 1)},
		{name: "unknown environment credential field", data: strings.Replace(validIdentitySuiteYAML, "    kind: system_one", "    kind: system_one\n    api_key_env: PRIMARY_KEY", 1)},
		{name: "missing credential", data: strings.Replace(validIdentitySuiteYAML, "    credential:\n      store: local-os\n      name: codereview/primary\n", "", 1)},
		{name: "invalid credential name", data: strings.Replace(validIdentitySuiteYAML, "name: codereview/primary", "name: other/primary", 1)},
		{name: "bad auth scheme", data: strings.Replace(validIdentitySuiteYAML, "auth_scheme: bearer", "auth_scheme: basic", 1)},
		{name: "blank service tier", data: strings.Replace(validIdentitySuiteYAML, "auth_scheme: bearer", "auth_scheme: bearer\n    service_tier: '  '", 1)},
		{name: "duplicate candidate field", data: strings.Replace(validIdentitySuiteYAML, "provider: secondary", "provider: secondary\n    provider: primary", 1)},
		{name: "duplicate candidate name", data: strings.Replace(validIdentitySuiteYAML, "    expected_model: model-resolved", "    expected_model: model-resolved\n  saved:\n    provider: primary\n    model: duplicate-model", 1)},
		{name: "numeric candidate model", data: strings.Replace(validIdentitySuiteYAML, "model: model-candidate", "model: 42", 1)},
		{name: "null candidate model", data: strings.Replace(validIdentitySuiteYAML, "model: model-candidate", "model: null", 1)},
		{name: "unknown candidate field", data: strings.Replace(validIdentitySuiteYAML, "expected_model: model-resolved", "expected_model: model-resolved\n    default: true", 1)},
		{name: "missing candidate provider", data: strings.Replace(validIdentitySuiteYAML, "provider: secondary", "provider: missing", 1)},
		{name: "alias", data: strings.Replace(validIdentitySuiteYAML, "model: model-candidate\n    expected_model: model-resolved", "model: &model model-candidate\n    expected_model: *model", 1)},
		{name: "merge key", data: "task: report_identity\nfixture: ./fixture.json\nproviders:\n  common: &common\n    kind: system_one\n    endpoint: https://primary.example/v1/systemone\n    credential:\n      store: local-os\n      name: codereview/primary\n    auth_scheme: bearer\n  p:\n    <<: *common\n"},
		{name: "trailing document", data: validIdentitySuiteYAML + "\n---\ntask: report_identity\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseIdentitySuite([]byte(test.data)); err == nil {
				t.Fatalf("parseIdentitySuite accepted invalid suite:\n%s", test.data)
			}
		})
	}
}

func TestParseOpenAIDecisionsProviderUsesExplicitEndpointAndFixedBearer(t *testing.T) {
	suite, err := parseIdentitySuite([]byte(validOpenAIDecisionsSuiteYAML))
	if err != nil {
		t.Fatalf("parse OpenAI Decisions identity suite: %v", err)
	}
	provider := suite.Providers["decisions"]
	if provider.Name != "decisions" || provider.Kind != "openai_decisions" || provider.Endpoint != "https://api.openai.com/v1/decisions" || provider.Credential != (config.CredentialLocation{Store: "local-os", Name: "codereview/openai"}) || provider.AuthScheme != "bearer" || provider.ServiceTier != "" {
		t.Fatalf("OpenAI Decisions provider = %#v", provider)
	}
	if candidate := suite.Candidates["saved"]; candidate.Provider != provider.Name || candidate.Model != "gpt-6-luna" || candidate.ExpectedModel != "gpt-6-luna" {
		t.Fatalf("OpenAI Decisions candidate = %#v", candidate)
	}

	invalid := []struct {
		name string
		text string
	}{
		{name: "configurable auth scheme", text: strings.Replace(validOpenAIDecisionsSuiteYAML, "    kind: openai_decisions", "    kind: openai_decisions\n    auth_scheme: api-key", 1)},
		{name: "configurable service tier", text: strings.Replace(validOpenAIDecisionsSuiteYAML, "    kind: openai_decisions", "    kind: openai_decisions\n    service_tier: batch", 1)},
		{name: "account ID", text: strings.Replace(validOpenAIDecisionsSuiteYAML, "    kind: openai_decisions", "    kind: openai_decisions\n    account_id: abc123", 1)},
		{name: "missing endpoint", text: strings.Replace(validOpenAIDecisionsSuiteYAML, "    endpoint: https://api.openai.com/v1/decisions\n", "", 1)},
		{name: "missing credential", text: strings.Replace(validOpenAIDecisionsSuiteYAML, "    credential:\n      store: local-os\n      name: codereview/openai\n", "", 1)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseIdentitySuite([]byte(test.text)); err == nil {
				t.Fatalf("parseIdentitySuite accepted invalid OpenAI Decisions provider:\n%s", test.text)
			}
		})
	}
}

func TestLoadIdentitySuiteResolvesFixtureAndHashesExactSuiteBytes(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixturePath, []byte(`{"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	suitePath := filepath.Join(dir, "identity.yml")
	if err := os.WriteFile(suitePath, []byte(validIdentitySuiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	suite, data, err := loadIdentitySuite(suitePath)
	if err != nil {
		t.Fatalf("load identity suite: %v", err)
	}
	absSuite, err := filepath.Abs(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	if suite.SuitePath != absSuite || suite.Fixture != fixturePath || suite.SuiteSHA != sha256Hex(data) {
		t.Fatalf("resolved identity suite = %#v, suite path %q fixture %q hash %q", suite, absSuite, fixturePath, sha256Hex(data))
	}
}

func TestIdentitySelectionRequiresOneExplicitCompleteSelector(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "identity.yml")
	if err := os.WriteFile(suitePath, []byte(validIdentitySuiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	resolverCalls, keyLookups := 0, 0
	previous := identityProviderRuntimeFor
	identityProviderRuntimeFor = func(_ identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
		resolverCalls++
		dependencies := inferSystemOneDependencies{
			resolveAPIKey: func(context.Context) (string, error) { keyLookups++; return "unused", nil },
			newClient: func(systemone.Options) (systemOneDecider, error) {
				t.Fatal("invalid selection opened a provider client")
				return nil, nil
			},
			now: time.Now, rename: os.Rename,
		}
		return identityProviderRuntime{
			dependencies: identityInferenceDependenciesForSystemOne(dependencies),
			backend:      inferSystemOneBackend(flags, dependencies),
		}, nil
	}
	t.Cleanup(func() { identityProviderRuntimeFor = previous })

	tests := []struct {
		name  string
		flags []string
	}{
		{name: "no implicit provider or model"},
		{name: "missing model", flags: []string{"--provider", "primary"}},
		{name: "missing provider", flags: []string{"--model", "model"}},
		{name: "unknown provider", flags: []string{"--provider", "missing", "--model", "model"}},
		{name: "unknown candidate", flags: []string{"--candidate", "missing"}},
		{name: "multiple candidates", flags: []string{"--candidate", "saved", "--candidate", "saved"}},
		{name: "candidate with empty provider", flags: []string{"--candidate", "saved", "--provider", ""}},
		{name: "candidate with empty model", flags: []string{"--candidate", "saved", "--model", ""}},
		{name: "candidate with empty expected model", flags: []string{"--candidate", "saved", "--expected-model", ""}},
		{name: "incomplete direct selection with empty values", flags: []string{"--provider", "", "--model", ""}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputDir := filepath.Join(dir, fmt.Sprintf("output-%d", index))
			flags := append([]string{"benchmark", "run", suitePath, "--max-calls", "0", "--results-dir", outputDir}, test.flags...)
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			if err := root.Execute(cmd, flags); err == nil {
				t.Fatal("invalid selector succeeded")
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid selector created output or stat failed: %v", err)
			}
		})
	}
	if resolverCalls != 0 || keyLookups != 0 {
		t.Fatalf("invalid selections resolved provider %d times and looked up key %d times", resolverCalls, keyLookups)
	}
}

func TestIdentityRunDirectAndCandidateSelectionPublishResolvedConfiguration(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	fixtureBytes := []byte(`{"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":[]}`)
	if err := os.WriteFile(fixturePath, fixtureBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	suitePath := filepath.Join(dir, "identity.yml")
	suiteBytes := []byte(validIdentitySuiteYAML)
	if err := os.WriteFile(suitePath, suiteBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := identityProviderRuntimeFor
	keyLookups, clientOpens := 0, 0
	identityProviderRuntimeFor = func(_ identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
		dependencies := inferSystemOneDependencies{
			resolveAPIKey: func(context.Context) (string, error) { keyLookups++; return "unused", nil },
			newClient: func(systemone.Options) (systemOneDecider, error) {
				clientOpens++
				return nil, nil
			},
			now:    func() time.Time { return time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC) },
			rename: os.Rename,
		}
		return identityProviderRuntime{
			dependencies: identityInferenceDependenciesForSystemOne(dependencies),
			backend:      inferSystemOneBackend(flags, dependencies),
		}, nil
	}
	t.Cleanup(func() { identityProviderRuntimeFor = previous })

	tests := []struct {
		name              string
		selector          []string
		wantProvider      string
		wantModel         string
		wantExpected      string
		wantEndpoint      string
		wantCredential    config.CredentialLocation
		wantCredentialKey string
		wantAuthScheme    string
		wantCandidateID   *string
	}{
		{
			name: "direct", selector: []string{"--provider", "primary", "--model", "direct-model"},
			wantProvider: "primary", wantModel: "direct-model", wantExpected: "direct-model",
			wantEndpoint: "https://primary.example/v1/systemone", wantCredential: config.CredentialLocation{Store: "local-os", Name: "codereview/primary"},
			wantCredentialKey: "system_one_api_key", wantAuthScheme: "Bearer",
		},
		{
			name: "candidate", selector: []string{"--candidate", "saved"},
			wantProvider: "secondary", wantModel: "model-candidate", wantExpected: "model-resolved",
			wantEndpoint: "https://secondary.example/v1/systemone", wantCredential: config.CredentialLocation{Store: "local-os", Name: "codereview/secondary"},
			wantCredentialKey: "system_one_api_key", wantAuthScheme: "Api-Key",
			wantCandidateID: stringPtr("saved"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputDir := filepath.Join(dir, "run-"+test.name)
			args := append([]string{"benchmark", "run", suitePath}, test.selector...)
			args = append(args, "--max-calls", "0", "--results-dir", outputDir)
			cmd, stdout, _, _ := newReplayCommandForTest(t, nil)
			if err := root.Execute(cmd, args); err != nil {
				t.Fatalf("execute identity run: %v", err)
			}
			var summary identityInferenceSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("decode summary: %v; stdout=%s", err, stdout.String())
			}
			var receipts identityInferenceReceipts
			if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
				t.Fatalf("decode receipts: %v", err)
			}
			configuration := receipts.Configuration
			if !reflect.DeepEqual(summary.Configuration, configuration) {
				t.Fatalf("stdout configuration %#v differs from receipts %#v", summary.Configuration, configuration)
			}
			if configuration.Task != identitySuiteTask || configuration.SuitePath != suitePath || configuration.SuiteSHA256 != sha256Hex(suiteBytes) ||
				configuration.ProviderName != test.wantProvider || configuration.AdapterKind != "system_one" || configuration.Provider != "system_one" ||
				configuration.Endpoint != test.wantEndpoint || configuration.RequestedModel != test.wantModel || configuration.ExpectedModel != test.wantExpected ||
				configuration.CredentialStore != test.wantCredential.Store || configuration.CredentialName != test.wantCredential.Name ||
				configuration.CredentialKey != test.wantCredentialKey || configuration.AuthScheme != test.wantAuthScheme ||
				configuration.TimeoutMS != 30_000 || configuration.MaxCalls != 0 {
				t.Fatalf("resolved configuration = %#v", configuration)
			}
			if !reflect.DeepEqual(configuration.CandidateID, test.wantCandidateID) {
				t.Fatalf("candidate ID = %#v, want %#v", configuration.CandidateID, test.wantCandidateID)
			}
			if summary.ProviderCalls != 0 || keyLookups != 0 || clientOpens != 0 {
				t.Fatalf("empty pair run used provider: calls=%d key lookups=%d client opens=%d", summary.ProviderCalls, keyLookups, clientOpens)
			}
			var rawReceipts map[string]json.RawMessage
			if err := json.Unmarshal(readInferSystemOneOutput(t, outputDir, "receipts.json"), &rawReceipts); err != nil {
				t.Fatal(err)
			}
			var rawConfiguration map[string]json.RawMessage
			if err := json.Unmarshal(rawReceipts["configuration"], &rawConfiguration); err != nil {
				t.Fatal(err)
			}
			if test.wantCandidateID == nil && string(rawConfiguration["candidate_id"]) != "null" {
				t.Fatalf("direct candidate_id JSON = %s, want null", rawConfiguration["candidate_id"])
			}
		})
	}
}

func TestReviewRunRejectsExplicitIdentityOnlyFlags(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "review.yml")
	if err := os.WriteFile(suitePath, []byte("suite: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := [][]string{
		{"--provider", "primary"}, {"--model", "m"}, {"--expected-model", "m"}, {"--max-calls", "0"},
		{"--max-state-bytes", "6144"}, {"--max-request-bytes", "8192"}, {"--timeout", "30s"},
		{"--input-price-per-million", "0"}, {"--output-price-per-million", "0"}, {"--allow-unverified-context=false"},
	}
	for index, extra := range tests {
		t.Run(strings.Join(extra, "_"), func(t *testing.T) {
			outputDir := filepath.Join(dir, fmt.Sprintf("review-output-%d", index))
			args := append([]string{"benchmark", "run", suitePath}, extra...)
			args = append(args, "--results-dir", outputDir)
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			err := root.Execute(cmd, args)
			if err == nil {
				t.Fatalf("review suite accepted identity-only flags %v", extra)
			}
			if !strings.Contains(err.Error(), "only supported for report_identity suites") {
				t.Fatalf("review identity flag error = %v, want the explicit review-suite routing diagnostic", err)
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("review flag rejection created output or stat failed: %v", err)
			}
		})
	}
}

func TestIdentityRunRejectsExplicitReviewOnlyFlagsAndSystemOneContextAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "identity.yml")
	if err := os.WriteFile(suitePath, []byte(validIdentitySuiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := identityProviderRuntimeFor
	runtimeCalls, keyLookups := 0, 0
	identityProviderRuntimeFor = func(_ identityProviderConfig, _ identityInferenceFlags) (identityProviderRuntime, error) {
		runtimeCalls++
		return identityProviderRuntime{dependencies: identityInferenceDependencies{
			resolveAPIKey: func(context.Context) (string, error) { keyLookups++; return "key", nil },
			now:           time.Now,
			rename:        os.Rename,
		}}, errors.New("invalid options reached provider adapter")
	}
	t.Cleanup(func() { identityProviderRuntimeFor = previous })

	tests := []struct {
		name       string
		flag       []string
		wantInText string
	}{
		{name: "case", flag: []string{"--case", "case-one"}, wantInText: "--case is not supported for report_identity suites"},
		{name: "cr-bin", flag: []string{"--cr-bin", "./cr"}, wantInText: "--cr-bin is not supported for report_identity suites"},
		{name: "in-process", flag: []string{"--in-process"}, wantInText: "--in-process is not supported for report_identity suites"},
		{name: "json", flag: []string{"--json"}, wantInText: "--json is not supported for report_identity suites"},
		{name: "explicit false json", flag: []string{"--json=false"}, wantInText: "--json is not supported for report_identity suites"},
		{name: "system one context acknowledgement", flag: []string{"--allow-unverified-context=false"}, wantInText: "supported only by providers that require context acknowledgement"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputDir := filepath.Join(dir, fmt.Sprintf("identity-output-%d", index))
			args := []string{"benchmark", "run", suitePath, "--provider", "primary", "--model", "model", "--max-calls", "0", "--results-dir", outputDir}
			args = append(args, test.flag...)
			cmd, _, _, _ := newReplayCommandForTest(t, nil)
			err := root.Execute(cmd, args)
			if err == nil || !strings.Contains(err.Error(), test.wantInText) {
				t.Fatalf("identity flag error = %v, want message containing %q", err, test.wantInText)
			}
			if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid identity flag created output or stat failed: %v", err)
			}
		})
	}
	if runtimeCalls != 0 || keyLookups != 0 {
		t.Fatalf("invalid flags resolved provider %d times and looked up key %d times", runtimeCalls, keyLookups)
	}
}

func TestExplicitIdentitySuiteDetectionRejectsDuplicateAndMalformedTask(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "duplicate task", data: "task: report_identity\ntask: other\n"},
		{name: "null task", data: "task: null\n"},
		{name: "sequence task", data: "task: [report_identity]\n"},
		{name: "malformed yaml", data: "task: [\n"},
		{name: "trailing review document", data: "suite: {}\n---\ntask: report_identity\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := explicitIdentitySuite([]byte(test.data)); err == nil {
				t.Fatalf("explicitIdentitySuite accepted %s", test.name)
			}
		})
	}
	if isIdentity, err := explicitIdentitySuite([]byte("suite: {}\n")); err != nil || isIdentity {
		t.Fatalf("review suite detection = %t, %v; want false, nil", isIdentity, err)
	}
	if isIdentity, err := explicitIdentitySuite([]byte("task: future_task\n")); err != nil || !isIdentity {
		t.Fatalf("explicit future task detection = %t, %v; want true, nil", isIdentity, err)
	}
}

func TestUnknownExplicitTaskDoesNotFallBackToReviewSuite(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "unsupported.yml")
	if err := os.WriteFile(suitePath, []byte("task: future_task\nfixture: ./fixture.json\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(dir, "output")
	cmd, _, _, _ := newReplayCommandForTest(t, nil)
	err := root.Execute(cmd, []string{"benchmark", "run", suitePath, "--results-dir", outputDir, "--max-calls", "0"})
	if err == nil || !strings.Contains(err.Error(), "unsupported identity suite task") {
		t.Fatalf("unsupported task error = %v, want explicit identity task failure", err)
	}
	if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported task created output or stat failed: %v", err)
	}
}

func stringPtr(value string) *string { return &value }
