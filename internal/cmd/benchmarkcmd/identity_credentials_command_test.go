package benchmarkcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
	"github.com/spf13/cobra"
)

const (
	identityCredentialTestStoreID = "test-file"
	identityCredentialPassphrase  = "synthetic-test-passphrase"
)

type identityCredentialCommandState struct {
	clients     []*inferSystemOneFakeClient
	apiKeys     []string
	clientOpens int
}

func (state *identityCredentialCommandState) decideCalls() int {
	var calls int
	for _, client := range state.clients {
		calls += client.decides
	}
	return calls
}

func isolateIdentityCredentialCommandEnvironment(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", identityCredentialPassphrase)
	t.Setenv(credentials.BackendEnvVar(), "")
	return base
}

func seedIdentityCredentialFileStore(t *testing.T) {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open isolated credential store: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = store.Close()
		}
	})
	for _, seed := range []struct {
		profile string
		key     string
		value   string
	}{
		{profile: "alpha", key: credentials.SystemOneAPIKeyKey, value: "synthetic-alpha"},
		{profile: "beta", key: credentials.SystemOneAPIKeyKey, value: "synthetic-beta"},
		{profile: "alpha", key: credentials.OpenAIAPIKeyKey, value: "synthetic-openai-alpha-decoy"},
		{profile: "beta", key: credentials.OpenAIAPIKeyKey, value: "synthetic-openai-beta-decoy"},
	} {
		if err := store.Set(seed.profile, seed.key, seed.value, credstore.WithOverwrite()); err != nil {
			t.Fatalf("Seed isolated credential key %s: %v", seed.key, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close isolated credential seed store: %v", err)
	}
	closed = true
}

func identityCredentialTestConfig(includeDeclaredGitRef bool) config.File {
	cfg := config.File{Secrets: config.SecretsConfig{Stores: map[string]config.SecretsStore{
		identityCredentialTestStoreID: {
			DisplayName: "Test File Store",
			Backend: config.SecretsStoreBackend{
				Kind: config.SecretsBackendKind(credstore.BackendFile),
			},
		},
	}}}
	if includeDeclaredGitRef {
		cfg.Profiles = map[string]config.Profile{
			"alpha": {
				Git: config.GitConfig{
					Host:       "github.com",
					AuthMode:   config.GitAuthModePAT,
					Credential: config.CredentialLocation{Store: identityCredentialTestStoreID, Name: "codereview/alpha"},
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		}
	}
	return cfg
}

func saveIdentityCredentialTestConfig(t *testing.T, path string, includeDeclaredGitRef bool) {
	t.Helper()
	if err := config.Save(path, identityCredentialTestConfig(includeDeclaredGitRef)); err != nil {
		t.Fatalf("Save isolated credential config: %v", err)
	}
}

func identityCredentialSuiteYAML(alphaStore, alphaRef, betaStore, betaRef string) string {
	return fmt.Sprintf(`task: report_identity
fixture: ./fixture.json
providers:
  alpha:
    kind: system_one
    endpoint: https://example.invalid/v1/systemone
    credential:
      store: %s
      name: %s
    auth_scheme: api-key
  beta:
    kind: system_one
    endpoint: https://example.invalid/v1/systemone
    credential:
      store: %s
      name: %s
    auth_scheme: api-key
candidates:
  selected:
    provider: beta
    model: provider/model-test
    expected_model: provider/model-test
`, alphaStore, alphaRef, betaStore, betaRef)
}

func writeIdentityCredentialSuite(t *testing.T, dir, alphaStore, alphaRef, betaStore, betaRef string) (string, int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("Create identity credential fixture directory: %v", err)
	}
	_, fixture := writeInferSystemOneInput(t, dir, inferSystemOneCohortInput(2))
	if len(fixture) == 0 {
		t.Fatal("identity credential fixture is empty")
	}
	suitePath := filepath.Join(dir, "identity.yml")
	if err := os.WriteFile(suitePath, []byte(identityCredentialSuiteYAML(alphaStore, alphaRef, betaStore, betaRef)), 0o600); err != nil {
		t.Fatalf("Write identity credential suite: %v", err)
	}
	return suitePath, len(fixture)
}

func newIdentityCredentialCommand(t *testing.T, configPath string) (*cobra.Command, *identityCredentialCommandState) {
	t.Helper()
	state := &identityCredentialCommandState{}
	dependencies := inferSystemOneDependencies{
		now:    time.Now,
		rename: os.Rename,
		newClient: func(options systemone.Options) (systemOneDecider, error) {
			state.clientOpens++
			state.apiKeys = append(state.apiKeys, options.APIKey)
			client := &inferSystemOneFakeClient{t: t, limits: options.Limits}
			state.clients = append(state.clients, client)
			return client, nil
		},
	}
	cmd, _, _ := newInferSystemOneTestCommand(t, &root.Options{ConfigPath: configPath}, dependencies)
	return cmd, state
}

func executeIdentityCredentialRun(t *testing.T, configPath, suitePath, resultsDir string, selection ...string) (*identityCredentialCommandState, error) {
	t.Helper()
	cmd, state := newIdentityCredentialCommand(t, configPath)
	args := []string{"benchmark", "run", suitePath}
	args = append(args, selection...)
	args = append(args, "--max-calls", "1", "--results-dir", resultsDir)
	return state, root.Execute(cmd, args)
}

func assertIdentityCredentialRun(t *testing.T, state *identityCredentialCommandState, resultsDir, provider, credentialRef, apiKey, candidate string) {
	t.Helper()
	if state.clientOpens != 1 || len(state.clients) != 1 || state.decideCalls() != 1 || len(state.apiKeys) != 1 {
		t.Fatalf("provider activity: clients=%d decisions=%d captured keys=%d", state.clientOpens, state.decideCalls(), len(state.apiKeys))
	}
	if state.apiKeys[0] != apiKey {
		t.Fatal("constructed provider received a key different from the selected credential ref")
	}
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readInferSystemOneOutput(t, resultsDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("Decode benchmark receipts: %v", err)
	}
	configuration := receipts.Configuration
	if configuration.ProviderName != provider || configuration.CredentialStore != identityCredentialTestStoreID ||
		configuration.CredentialName != credentialRef || configuration.CredentialKey != credentials.SystemOneAPIKeyKey ||
		receipts.ExpectedPairs != 1 || receipts.ProviderCalls != 1 || receipts.AttemptedPairs != 1 {
		t.Fatalf("benchmark receipt did not retain the selected provider and credential ref: config=%#v calls=%d expected=%d attempted=%d", configuration, receipts.ProviderCalls, receipts.ExpectedPairs, receipts.AttemptedPairs)
	}
	if candidate == "" {
		if configuration.CandidateID != nil {
			t.Fatalf("direct selection receipt candidate = %q, want none", *configuration.CandidateID)
		}
		return
	}
	if configuration.CandidateID == nil || *configuration.CandidateID != candidate {
		t.Fatalf("candidate receipt = %v, want %q", configuration.CandidateID, candidate)
	}
}

func assertNoIdentityCredentialProviderActivity(t *testing.T, state *identityCredentialCommandState) {
	t.Helper()
	if state.clientOpens != 0 || len(state.clients) != 0 || state.decideCalls() != 0 {
		t.Fatalf("failed credential selection used provider: clients=%d decisions=%d", state.clientOpens, state.decideCalls())
	}
}

func assertIdentityCredentialAuthPreflight(t *testing.T, resultsDir, storeID, credentialRef string) {
	t.Helper()
	var receipts identityInferenceReceipts
	if err := json.Unmarshal(readInferSystemOneOutput(t, resultsDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("Decode failed credential receipts: %v", err)
	}
	configuration := receipts.Configuration
	if configuration.ProviderName != "alpha" || configuration.CredentialStore != storeID ||
		configuration.CredentialName != credentialRef || configuration.CredentialKey != credentials.SystemOneAPIKeyKey ||
		receipts.Status != "error" || receipts.ExpectedPairs != 1 || receipts.ProviderCalls != 0 ||
		receipts.AttemptedPairs != 0 || receipts.CompletedPairs != 0 || receipts.MissingPairs != 1 ||
		len(receipts.PreflightErrors) != 1 || receipts.PreflightErrors[0].ErrorCode != "auth" {
		t.Fatalf("credential failure receipt = %#v", receipts)
	}
}

func TestBenchmarkRunResolvesIdentityCredentialsThroughConfiguredFileStore(t *testing.T) {
	base := isolateIdentityCredentialCommandEnvironment(t)
	seedIdentityCredentialFileStore(t)
	configPath := filepath.Join(base, "config.yml")
	saveIdentityCredentialTestConfig(t, configPath, false)
	suitePath, fixtureSize := writeIdentityCredentialSuite(t, filepath.Join(base, "input"), identityCredentialTestStoreID, "codereview/alpha", identityCredentialTestStoreID, "codereview/beta")
	if fixtureSize == 0 {
		t.Fatal("comparison fixture must contain a pair")
	}

	t.Run("direct provider selection", func(t *testing.T) {
		resultsDir := filepath.Join(base, "results-direct")
		state, err := executeIdentityCredentialRun(t, configPath, suitePath, resultsDir,
			"--provider", "alpha", "--model", "provider/model-test")
		if err != nil {
			t.Fatalf("benchmark run direct provider selection: %v", err)
		}
		assertIdentityCredentialRun(t, state, resultsDir, "alpha", "codereview/alpha", "synthetic-alpha", "")
	})

	t.Run("candidate provider selection", func(t *testing.T) {
		resultsDir := filepath.Join(base, "results-candidate")
		state, err := executeIdentityCredentialRun(t, configPath, suitePath, resultsDir, "--candidate", "selected")
		if err != nil {
			t.Fatalf("benchmark run candidate provider selection: %v", err)
		}
		assertIdentityCredentialRun(t, state, resultsDir, "beta", "codereview/beta", "synthetic-beta", "selected")
	})

	t.Run("missing secret", func(t *testing.T) {
		missingSuite, _ := writeIdentityCredentialSuite(t, filepath.Join(base, "missing-secret"), identityCredentialTestStoreID, "codereview/missing", identityCredentialTestStoreID, "codereview/beta")
		state, err := executeIdentityCredentialRun(t, configPath, missingSuite, filepath.Join(base, "results-missing-secret"),
			"--provider", "alpha", "--model", "provider/model-test")
		if err == nil {
			t.Fatal("benchmark run succeeded without the selected secret")
		}
		assertNoIdentityCredentialProviderActivity(t, state)
		assertIdentityCredentialAuthPreflight(t, filepath.Join(base, "results-missing-secret"), identityCredentialTestStoreID, "codereview/missing")
	})

	t.Run("invalid credential ref", func(t *testing.T) {
		invalidSuite, _ := writeIdentityCredentialSuite(t, filepath.Join(base, "invalid-ref"), identityCredentialTestStoreID, "other/invalid", identityCredentialTestStoreID, "codereview/beta")
		resultsDir := filepath.Join(base, "results-invalid-ref")
		state, err := executeIdentityCredentialRun(t, configPath, invalidSuite, resultsDir,
			"--provider", "alpha", "--model", "provider/model-test")
		if err == nil {
			t.Fatal("benchmark run accepted an invalid credential ref")
		}
		assertNoIdentityCredentialProviderActivity(t, state)
		if _, statErr := os.Lstat(resultsDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("invalid credential ref reserved output directory: %v", statErr)
		}
	})

	t.Run("missing configured store", func(t *testing.T) {
		missingStoreSuite, _ := writeIdentityCredentialSuite(t, filepath.Join(base, "missing-store"), "missing-store", "codereview/alpha", identityCredentialTestStoreID, "codereview/beta")
		resultsDir := filepath.Join(base, "results-missing-store")
		state, err := executeIdentityCredentialRun(t, configPath, missingStoreSuite, resultsDir,
			"--provider", "alpha", "--model", "provider/model-test")
		if err == nil {
			t.Fatal("benchmark run accepted a missing configured store")
		}
		assertNoIdentityCredentialProviderActivity(t, state)
		assertIdentityCredentialAuthPreflight(t, resultsDir, "missing-store", "codereview/alpha")
	})

	t.Run("declared Git credential ref rejects identity key", func(t *testing.T) {
		conflictingConfigPath := filepath.Join(base, "conflicting-config.yml")
		saveIdentityCredentialTestConfig(t, conflictingConfigPath, true)
		declaredKeys, keysErr := credentials.ExpectedKeysForConfigRef(identityCredentialTestConfig(true), "codereview/alpha")
		if keysErr != nil || len(declaredKeys) != 1 || declaredKeys[0] != credentials.GitTokenKey {
			t.Fatalf("declared Git ref keys = %v, err=%v; want only %s", declaredKeys, keysErr, credentials.GitTokenKey)
		}
		resultsDir := filepath.Join(base, "results-conflicting-ref")
		state, err := executeIdentityCredentialRun(t, conflictingConfigPath, suitePath, resultsDir,
			"--provider", "alpha", "--model", "provider/model-test")
		if err == nil {
			t.Fatal("benchmark run accepted an identity key under a declared Git-only credential ref")
		}
		assertNoIdentityCredentialProviderActivity(t, state)
		assertIdentityCredentialAuthPreflight(t, resultsDir, identityCredentialTestStoreID, "codereview/alpha")
	})
}
