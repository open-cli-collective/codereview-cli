package benchmarkcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"go.yaml.in/yaml/v3"
)

const (
	identitySuiteTask    = "report_identity"
	maxBenchmarkSuiteYML = 16 << 20
)

type identityProviderConfig struct {
	Name        string
	Kind        string
	Endpoint    string
	AccountID   string
	APIKeyEnv   string
	AuthScheme  string
	ServiceTier string
}

type identityCandidateConfig struct {
	ID            string
	Provider      string
	Model         string
	ExpectedModel string
}

type identitySuiteConfig struct {
	Task       string
	Fixture    string
	SuitePath  string
	SuiteSHA   string
	Providers  map[string]identityProviderConfig
	Candidates map[string]identityCandidateConfig
}

type identityProviderRuntime struct {
	dependencies identityInferenceDependencies
	backend      identityInferenceBackend
}

// identityProviderRuntimeFor is the single adapter registration point. Future
// provider leaves add a kind here while retaining the common runner.
var identityProviderRuntimeFor = defaultIdentityProviderRuntime

func defaultIdentityProviderRuntime(provider identityProviderConfig, flags identityInferenceFlags) (identityProviderRuntime, error) {
	switch provider.Kind {
	case "system_one":
		dependencies := defaultInferSystemOneDependencies()
		return identityProviderRuntime{
			dependencies: identityInferenceDependenciesForSystemOne(dependencies),
			backend:      inferSystemOneBackend(flags, dependencies),
		}, nil
	case "openai_decisions":
		return openAIDecisionsIdentityProviderRuntime(provider, flags), nil
	case "cloudflare_clef":
		return newIdentityClefProviderRuntime(provider, flags)
	default:
		return identityProviderRuntime{}, fmt.Errorf("unknown provider adapter kind %q", provider.Kind)
	}
}

func loadIdentitySuite(path string) (identitySuiteConfig, []byte, error) {
	data, err := readBenchmarkSuiteBytes(path)
	if err != nil {
		return identitySuiteConfig{}, nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return identitySuiteConfig{}, nil, fmt.Errorf("resolve suite path: %w", err)
	}
	suite, err := parseIdentitySuite(data)
	if err != nil {
		return identitySuiteConfig{}, nil, err
	}
	fixture := suite.Fixture
	if !filepath.IsAbs(fixture) {
		fixture = filepath.Join(filepath.Dir(abs), fixture)
	}
	suite.Fixture = filepath.Clean(fixture)
	suite.SuitePath = abs
	suite.SuiteSHA = sha256Hex(data)
	return suite, data, nil
}

func readBenchmarkSuiteBytes(path string) ([]byte, error) {
	// #nosec G304 -- the benchmark suite path is explicit command input.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBenchmarkSuiteYML+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxBenchmarkSuiteYML {
		return nil, fmt.Errorf("suite exceeds %d bytes", maxBenchmarkSuiteYML)
	}
	return data, nil
}

func parseIdentitySuite(data []byte) (identitySuiteConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return identitySuiteConfig{}, fmt.Errorf("invalid identity suite YAML: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return identitySuiteConfig{}, errors.New("identity suite must contain exactly one YAML document")
		}
		return identitySuiteConfig{}, fmt.Errorf("invalid trailing identity suite YAML: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return identitySuiteConfig{}, errors.New("identity suite root must be a mapping")
	}
	rootNode := document.Content[0]
	if err := rejectIdentityYAMLAliases(rootNode); err != nil {
		return identitySuiteConfig{}, err
	}
	rootMap, err := identityYAMLMapping(rootNode, "identity suite", map[string]bool{
		"task": true, "fixture": true, "providers": true, "candidates": true,
	})
	if err != nil {
		return identitySuiteConfig{}, err
	}
	for _, key := range []string{"task", "fixture", "providers"} {
		if rootMap[key] == nil {
			return identitySuiteConfig{}, fmt.Errorf("identity suite requires %q", key)
		}
	}
	task, err := identityYAMLString(rootMap["task"], "task")
	if err != nil {
		return identitySuiteConfig{}, err
	}
	if task != identitySuiteTask {
		return identitySuiteConfig{}, fmt.Errorf("unsupported identity suite task %q", task)
	}
	fixture, err := identityYAMLString(rootMap["fixture"], "fixture")
	if err != nil {
		return identitySuiteConfig{}, err
	}
	providers, err := parseIdentityProviders(rootMap["providers"])
	if err != nil {
		return identitySuiteConfig{}, err
	}
	candidates := map[string]identityCandidateConfig{}
	if node := rootMap["candidates"]; node != nil {
		candidates, err = parseIdentityCandidates(node, providers)
		if err != nil {
			return identitySuiteConfig{}, err
		}
	}
	return identitySuiteConfig{Task: task, Fixture: fixture, Providers: providers, Candidates: candidates}, nil
}

func parseIdentityProviders(node *yaml.Node) (map[string]identityProviderConfig, error) {
	entries, err := identityYAMLMapping(node, "providers", nil)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("identity suite providers must be nonempty")
	}
	providers := make(map[string]identityProviderConfig, len(entries))
	for name, value := range entries {
		kindNode := identityYAMLMapValue(value, "kind")
		if kindNode == nil {
			return nil, fmt.Errorf("provider %q requires %q", name, "kind")
		}
		kind, err := identityYAMLString(kindNode, "provider kind")
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		if kind != "system_one" && kind != "openai_decisions" && kind != "cloudflare_clef" {
			return nil, fmt.Errorf("provider %q has unknown adapter kind %q", name, kind)
		}
		allowedFields := map[string]bool{"kind": true, "api_key_env": true}
		requiredFields := []string{"api_key_env"}
		switch kind {
		case "system_one":
			allowedFields["endpoint"] = true
			allowedFields["auth_scheme"] = true
			allowedFields["service_tier"] = true
			requiredFields = append(requiredFields, "endpoint", "auth_scheme")
		case "openai_decisions":
			allowedFields["endpoint"] = true
			requiredFields = append(requiredFields, "endpoint")
		case "cloudflare_clef":
			allowedFields["account_id"] = true
			requiredFields = append(requiredFields, "account_id")
		}
		fields, err := identityYAMLMapping(value, "provider "+name, allowedFields)
		if err != nil {
			return nil, err
		}
		for _, field := range requiredFields {
			if fields[field] == nil {
				return nil, fmt.Errorf("provider %q requires %q", name, field)
			}
		}
		provider := identityProviderConfig{Name: name, Kind: kind}
		if provider.APIKeyEnv, err = identityYAMLString(fields["api_key_env"], "api_key_env"); err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		if !validAPIKeyEnvName(provider.APIKeyEnv) {
			return nil, fmt.Errorf("provider %q api_key_env must be an environment variable name", name)
		}
		if kind == "cloudflare_clef" {
			if provider.AccountID, err = identityYAMLString(fields["account_id"], "account_id"); err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			if _, err := identityClefEndpoint(provider, "clef"); err != nil {
				return nil, fmt.Errorf("provider %q account_id must be an ASCII Cloudflare account ID", name)
			}
			providers[name] = provider
			continue
		}
		if provider.Endpoint, err = identityYAMLString(fields["endpoint"], "endpoint"); err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		if !validIdentityInferenceEndpoint(provider.Endpoint) {
			return nil, fmt.Errorf("provider %q endpoint must be a valid HTTPS URL without query, userinfo, or fragment", name)
		}
		switch kind {
		case "system_one":
			if provider.AuthScheme, err = identityYAMLString(fields["auth_scheme"], "auth_scheme"); err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			if _, valid := normalizedAuthScheme(provider.AuthScheme); !valid {
				return nil, fmt.Errorf("provider %q auth_scheme must be api-key or bearer", name)
			}
			if fields["service_tier"] != nil {
				if provider.ServiceTier, err = identityYAMLString(fields["service_tier"], "service_tier"); err != nil {
					return nil, fmt.Errorf("provider %q: %w", name, err)
				}
			}
		case "openai_decisions":
			provider.AuthScheme = "bearer"
		}
		providers[name] = provider
	}
	return providers, nil
}

func parseIdentityCandidates(node *yaml.Node, providers map[string]identityProviderConfig) (map[string]identityCandidateConfig, error) {
	entries, err := identityYAMLMapping(node, "candidates", nil)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]identityCandidateConfig, len(entries))
	for name, value := range entries {
		fields, err := identityYAMLMapping(value, "candidate "+name, map[string]bool{
			"provider": true, "model": true, "expected_model": true,
		})
		if err != nil {
			return nil, err
		}
		for _, field := range []string{"provider", "model"} {
			if fields[field] == nil {
				return nil, fmt.Errorf("candidate %q requires %q", name, field)
			}
		}
		candidate := identityCandidateConfig{ID: name}
		if candidate.Provider, err = identityYAMLString(fields["provider"], "provider"); err != nil {
			return nil, fmt.Errorf("candidate %q: %w", name, err)
		}
		if _, exists := providers[candidate.Provider]; !exists {
			return nil, fmt.Errorf("candidate %q references unknown provider %q", name, candidate.Provider)
		}
		if candidate.Model, err = identityYAMLString(fields["model"], "model"); err != nil {
			return nil, fmt.Errorf("candidate %q: %w", name, err)
		}
		if fields["expected_model"] != nil {
			if candidate.ExpectedModel, err = identityYAMLString(fields["expected_model"], "expected_model"); err != nil {
				return nil, fmt.Errorf("candidate %q: %w", name, err)
			}
		}
		if provider := providers[candidate.Provider]; provider.Kind == "cloudflare_clef" {
			if _, err := identityClefEndpoint(provider, candidate.Model); err != nil {
				return nil, fmt.Errorf("candidate %q: Cloudflare Clef model must be clef or clef-flash", name)
			}
		}
		candidates[name] = candidate
	}
	return candidates, nil
}

func identityYAMLMapping(node *yaml.Node, label string, allowed map[string]bool) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", label)
	}
	values := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" || strings.TrimSpace(keyNode.Value) == "" || keyNode.Value == "<<" {
			return nil, fmt.Errorf("%s keys must be nonblank strings and may not be merge keys", label)
		}
		key := keyNode.Value
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate key %q", label, key)
		}
		if allowed != nil && !allowed[key] {
			return nil, fmt.Errorf("%s contains unknown field %q", label, key)
		}
		values[key] = node.Content[i+1]
	}
	return values, nil
}

func identityYAMLMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func identityYAMLString(node *yaml.Node, label string) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || strings.TrimSpace(node.Value) == "" {
		return "", fmt.Errorf("%s must be a nonblank string", label)
	}
	return node.Value, nil
}

func rejectIdentityYAMLAliases(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return errors.New("identity suite may not contain YAML aliases")
	}
	for _, child := range node.Content {
		if err := rejectIdentityYAMLAliases(child); err != nil {
			return err
		}
	}
	return nil
}

func explicitIdentitySuite(data []byte) (bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return false, fmt.Errorf("invalid benchmark suite YAML: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("benchmark suite must contain exactly one YAML document")
		}
		return false, fmt.Errorf("invalid trailing benchmark suite YAML: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, errors.New("benchmark suite root must be a mapping")
	}
	rootNode := document.Content[0]
	var taskNode *yaml.Node
	taskSeen := false
	for i := 0; i+1 < len(rootNode.Content); i += 2 {
		keyNode := rootNode.Content[i]
		if keyNode.Kind == yaml.ScalarNode && keyNode.Value == "task" {
			if taskSeen {
				return false, errors.New("benchmark suite contains duplicate task key")
			}
			taskSeen = true
			taskNode = rootNode.Content[i+1]
		}
	}
	if !taskSeen {
		return false, nil
	}
	if taskNode.Kind != yaml.ScalarNode || taskNode.Tag != "!!str" || strings.TrimSpace(taskNode.Value) == "" || taskNode.Alias != nil {
		return false, errors.New("benchmark suite task must be a nonblank string")
	}
	return true, nil
}

func runIdentitySuite(ctx context.Context, cmd *cobra.Command, opts *root.Options, run runFlags, identity identityRunFlags, suitePath string) error {
	suite, _, err := loadIdentitySuite(suitePath)
	if err != nil {
		return exitcode.Usage(fmt.Errorf("benchmark run: invalid report identity suite: %w", err))
	}
	if err := validateIdentityRunSurface(cmd, run, identity); err != nil {
		return exitcode.Usage(err)
	}
	identity.candidates = run.candidates
	if err := selectIdentityRun(cmd, suite, &identity); err != nil {
		return exitcode.Usage(err)
	}
	identity.flags.task = suite.Task
	identity.flags.suitePath = suite.SuitePath
	identity.flags.suiteSHA256 = suite.SuiteSHA
	identity.flags.outputDir = run.resultsDir
	identity.flags.inputPriceSet = cmd.Flags().Changed("input-price-per-million")
	identity.flags.outputPriceSet = cmd.Flags().Changed("output-price-per-million")
	provider := suite.Providers[identity.flags.providerName]
	identity.flags.apiKeyEnv = provider.APIKeyEnv
	identity.flags.adapterKind = provider.Kind
	if provider.Kind == "cloudflare_clef" {
		endpoint, err := identityClefEndpoint(provider, identity.flags.model)
		if err != nil {
			return exitcode.Usage(fmt.Errorf("benchmark run: %w", err))
		}
		identity.flags.endpoint = endpoint
		identity.flags.authScheme = "bearer"
		identity.flags.serviceTier = ""
		if !identity.allowUnverifiedCtx {
			return exitcode.Usage(errors.New("benchmark run: --allow-unverified-context is required because Cloudflare Clef may silently truncate state"))
		}
	} else {
		identity.flags.endpoint = provider.Endpoint
		identity.flags.authScheme = provider.AuthScheme
		identity.flags.serviceTier = provider.ServiceTier
		if cmd.Flags().Changed("allow-unverified-context") {
			return exitcode.Usage(errors.New("benchmark run: --allow-unverified-context is supported only by providers that require context acknowledgement"))
		}
	}
	if err := validateIdentityInferenceFlags("benchmark run", identity.flags); err != nil {
		return exitcode.Usage(err)
	}
	runtime, err := identityProviderRuntimeFor(provider, identity.flags)
	if err != nil {
		return exitcode.Usage(fmt.Errorf("benchmark run: %w", err))
	}
	return runIdentityInference(ctx, opts, "benchmark run", suite.Fixture, identity.flags, runtime.dependencies, runtime.backend)
}

type runIdentityFlags struct {
	flags              identityInferenceFlags
	provider           string
	candidates         []string
	allowUnverifiedCtx bool
}

// alias retained locally to keep the flag state specific to this command.
type identityRunFlags = runIdentityFlags

func validateIdentityRunSurface(cmd *cobra.Command, run runFlags, identity identityRunFlags) error {
	for _, name := range []string{"case", "cr-bin", "in-process", "json"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("benchmark run: --%s is not supported for report_identity suites", name)
		}
	}
	if strings.TrimSpace(run.resultsDir) == "" {
		return errors.New("benchmark run: --results-dir is required for report_identity suites")
	}
	if !cmd.Flags().Changed("max-calls") {
		return errors.New("benchmark run: --max-calls is required for report_identity suites")
	}
	if identity.flags.maxCalls < 0 || identity.flags.maxCalls > maximumIdentityInferenceCalls {
		return fmt.Errorf("benchmark run: --max-calls must be in the range 0-%d", maximumIdentityInferenceCalls)
	}
	return validateIdentityInferenceLimitsAndPrices("benchmark run", identity.flags)
}

func selectIdentityRun(cmd *cobra.Command, suite identitySuiteConfig, identity *identityRunFlags) error {
	if len(identity.candidates) > 1 {
		return errors.New("benchmark run: report_identity suites accept exactly one --candidate")
	}
	modelChanged := cmd.Flags().Changed("model")
	providerChanged := cmd.Flags().Changed("provider")
	expectedChanged := cmd.Flags().Changed("expected-model")
	if len(identity.candidates) == 1 {
		if providerChanged || modelChanged || expectedChanged {
			return errors.New("benchmark run: --candidate cannot be combined with --provider, --model, or --expected-model")
		}
		candidate, exists := suite.Candidates[identity.candidates[0]]
		if !exists {
			return fmt.Errorf("benchmark run: unknown candidate %q", identity.candidates[0])
		}
		identity.flags.candidateID = &candidate.ID
		identity.flags.providerName = candidate.Provider
		identity.flags.model = candidate.Model
		identity.flags.expectedModel = candidate.ExpectedModel
	} else {
		if !providerChanged || !modelChanged {
			return errors.New("benchmark run: select either one --candidate or both --provider and --model")
		}
		if strings.TrimSpace(identity.provider) == "" {
			return errors.New("benchmark run: --provider must be nonblank")
		}
		if strings.TrimSpace(identity.flags.model) == "" {
			return errors.New("benchmark run: --model must be nonblank")
		}
		if expectedChanged && strings.TrimSpace(identity.flags.expectedModel) == "" {
			return errors.New("benchmark run: --expected-model must be nonblank when specified")
		}
		if _, exists := suite.Providers[identity.provider]; !exists {
			return fmt.Errorf("benchmark run: unknown provider %q", identity.provider)
		}
		identity.flags.providerName = identity.provider
	}
	if strings.TrimSpace(identity.flags.model) == "" {
		return errors.New("benchmark run: model must be nonblank")
	}
	return nil
}
