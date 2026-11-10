package identityshadow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const maxConfigBytes = 64 * 1024

// Config is the standalone, opt-in identity shadow configuration. It is not
// part of the user's normal Codex or review profile configuration.
type Config struct {
	SchemaVersion  int           `json:"schema_version" yaml:"schema_version"`
	Provider       Provider      `json:"provider" yaml:"provider"`
	Model          string        `json:"model" yaml:"model"`
	ExpectedModel  string        `json:"expected_model" yaml:"expected_model"`
	Limits         Limits        `json:"limits" yaml:"limits"`
	Budget         Budget        `json:"budget" yaml:"budget"`
	Timeout        time.Duration `json:"-" yaml:"-"`
	TimeoutText    string        `json:"timeout" yaml:"timeout"`
	InferenceLimit time.Duration `json:"-" yaml:"-"`
	InferenceText  string        `json:"inference_timeout" yaml:"inference_timeout"`
	ServiceTier    *string       `json:"service_tier,omitempty" yaml:"service_tier,omitempty"`
	Prices         *Prices       `json:"prices,omitempty" yaml:"prices,omitempty"`
}

// Provider selects the sole supported standalone provider and its existing
// credential reference. The API key itself is never stored in Config.
type Provider struct {
	Kind       string     `json:"kind" yaml:"kind"`
	Endpoint   string     `json:"endpoint" yaml:"endpoint"`
	Credential Credential `json:"credential" yaml:"credential"`
	AuthScheme string     `json:"auth_scheme" yaml:"auth_scheme"`
}

// Credential contains only an existing store and credential name reference.
type Credential struct {
	Store string `json:"store" yaml:"store"`
	Name  string `json:"name" yaml:"name"`
}

// Limits bounds each System One request.
type Limits struct {
	MaxStateBytes   int `json:"max_state_bytes" yaml:"max_state_bytes"`
	MaxRequestBytes int `json:"max_request_bytes" yaml:"max_request_bytes"`
}

// Budget bounds the complete pair universe across both tasks.
type Budget struct {
	MaxCalls             int   `json:"max_calls" yaml:"max_calls"`
	MaxTotalRequestBytes int64 `json:"max_total_request_bytes" yaml:"max_total_request_bytes"`
}

// Prices are optional accounting inputs. Both prices must be present.
type Prices struct {
	InputPerMillion  float64 `json:"input_per_million" yaml:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million" yaml:"output_per_million"`
}

func parseConfig(data []byte) (Config, error) {
	if len(data) == 0 || len(data) > maxConfigBytes {
		return Config{}, errors.New("identity shadow config size is invalid")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Config{}, errors.New("identity shadow config is not valid YAML")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("identity shadow config must contain exactly one YAML document")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return Config{}, errors.New("identity shadow config root must be a mapping")
	}
	root := document.Content[0]
	if err := rejectYAMLAliases(root); err != nil {
		return Config{}, err
	}
	fields, err := mappingFields(root, "config", setOf(
		"schema_version", "provider", "model", "expected_model", "limits", "budget", "timeout", "inference_timeout", "service_tier", "prices",
	))
	if err != nil {
		return Config{}, err
	}
	for _, required := range []string{"schema_version", "provider", "model", "expected_model", "limits", "budget", "timeout", "inference_timeout"} {
		if fields[required] == nil {
			return Config{}, fmt.Errorf("identity shadow config requires %q", required)
		}
	}

	var config Config
	if config.SchemaVersion, err = integerValue(fields["schema_version"], "schema_version"); err != nil {
		return Config{}, err
	}
	if config.SchemaVersion != 1 {
		return Config{}, errors.New("identity shadow config schema_version must be 1")
	}
	if config.Provider, err = parseProvider(fields["provider"]); err != nil {
		return Config{}, err
	}
	if config.Model, err = stringValue(fields["model"], "model"); err != nil {
		return Config{}, err
	}
	if config.ExpectedModel, err = stringValue(fields["expected_model"], "expected_model"); err != nil {
		return Config{}, err
	}
	if config.Limits, err = parseLimits(fields["limits"]); err != nil {
		return Config{}, err
	}
	if config.Budget, err = parseBudget(fields["budget"]); err != nil {
		return Config{}, err
	}
	if config.TimeoutText, err = stringValue(fields["timeout"], "timeout"); err != nil {
		return Config{}, err
	}
	if config.Timeout, err = parseTimeout(config.TimeoutText, "timeout"); err != nil {
		return Config{}, err
	}
	if config.InferenceText, err = stringValue(fields["inference_timeout"], "inference_timeout"); err != nil {
		return Config{}, err
	}
	if config.InferenceLimit, err = parseTimeout(config.InferenceText, "inference_timeout"); err != nil {
		return Config{}, err
	}
	if config.Timeout > config.InferenceLimit {
		return Config{}, errors.New("identity shadow timeout must not exceed inference_timeout")
	}
	if fields["service_tier"] != nil {
		value, err := stringValue(fields["service_tier"], "service_tier")
		if err != nil {
			return Config{}, err
		}
		config.ServiceTier = &value
	}
	if fields["prices"] != nil {
		config.Prices, err = parsePrices(fields["prices"])
		if err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func parseProvider(node *yaml.Node) (Provider, error) {
	fields, err := mappingFields(node, "provider", setOf("kind", "endpoint", "credential", "auth_scheme"))
	if err != nil {
		return Provider{}, err
	}
	for _, required := range []string{"kind", "endpoint", "credential", "auth_scheme"} {
		if fields[required] == nil {
			return Provider{}, fmt.Errorf("identity shadow provider requires %q", required)
		}
	}
	var provider Provider
	if provider.Kind, err = stringValue(fields["kind"], "provider.kind"); err != nil {
		return Provider{}, err
	}
	if provider.Kind != "system_one" {
		return Provider{}, errors.New("identity shadow provider.kind must be system_one")
	}
	if provider.Endpoint, err = stringValue(fields["endpoint"], "provider.endpoint"); err != nil {
		return Provider{}, err
	}
	if !validHTTPSURL(provider.Endpoint) {
		return Provider{}, errors.New("identity shadow provider.endpoint must be HTTPS without userinfo, query, or fragment")
	}
	if provider.AuthScheme, err = stringValue(fields["auth_scheme"], "provider.auth_scheme"); err != nil {
		return Provider{}, err
	}
	if provider.AuthScheme != "api-key" && provider.AuthScheme != "bearer" {
		return Provider{}, errors.New("identity shadow provider.auth_scheme must be api-key or bearer")
	}
	credentialFields, err := mappingFields(fields["credential"], "provider.credential", setOf("store", "name"))
	if err != nil {
		return Provider{}, err
	}
	for _, required := range []string{"store", "name"} {
		if credentialFields[required] == nil {
			return Provider{}, fmt.Errorf("identity shadow provider.credential requires %q", required)
		}
	}
	if provider.Credential.Store, err = stringValue(credentialFields["store"], "provider.credential.store"); err != nil {
		return Provider{}, err
	}
	if provider.Credential.Name, err = stringValue(credentialFields["name"], "provider.credential.name"); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

func parseLimits(node *yaml.Node) (Limits, error) {
	fields, err := mappingFields(node, "limits", setOf("max_state_bytes", "max_request_bytes"))
	if err != nil {
		return Limits{}, err
	}
	for _, required := range []string{"max_state_bytes", "max_request_bytes"} {
		if fields[required] == nil {
			return Limits{}, fmt.Errorf("identity shadow limits requires %q", required)
		}
	}
	var limits Limits
	if limits.MaxStateBytes, err = integerValue(fields["max_state_bytes"], "limits.max_state_bytes"); err != nil {
		return Limits{}, err
	}
	if limits.MaxRequestBytes, err = integerValue(fields["max_request_bytes"], "limits.max_request_bytes"); err != nil {
		return Limits{}, err
	}
	if limits.MaxStateBytes < 1 || limits.MaxRequestBytes < limits.MaxStateBytes || limits.MaxRequestBytes > 1<<20 {
		return Limits{}, errors.New("identity shadow limits are outside supported bounds")
	}
	return limits, nil
}

func parseBudget(node *yaml.Node) (Budget, error) {
	fields, err := mappingFields(node, "budget", setOf("max_calls", "max_total_request_bytes"))
	if err != nil {
		return Budget{}, err
	}
	for _, required := range []string{"max_calls", "max_total_request_bytes"} {
		if fields[required] == nil {
			return Budget{}, fmt.Errorf("identity shadow budget requires %q", required)
		}
	}
	var budget Budget
	if budget.MaxCalls, err = integerValue(fields["max_calls"], "budget.max_calls"); err != nil {
		return Budget{}, err
	}
	if budget.MaxTotalRequestBytes, err = integer64Value(fields["max_total_request_bytes"], "budget.max_total_request_bytes"); err != nil {
		return Budget{}, err
	}
	if budget.MaxCalls < 0 || budget.MaxCalls > 10_000 || budget.MaxTotalRequestBytes < 0 || budget.MaxTotalRequestBytes > 1<<30 {
		return Budget{}, errors.New("identity shadow budget is outside supported bounds")
	}
	return budget, nil
}

func parsePrices(node *yaml.Node) (*Prices, error) {
	fields, err := mappingFields(node, "prices", setOf("input_per_million", "output_per_million"))
	if err != nil {
		return nil, err
	}
	if fields["input_per_million"] == nil || fields["output_per_million"] == nil {
		return nil, errors.New("identity shadow prices requires both input_per_million and output_per_million")
	}
	input, err := floatValue(fields["input_per_million"], "prices.input_per_million")
	if err != nil {
		return nil, err
	}
	output, err := floatValue(fields["output_per_million"], "prices.output_per_million")
	if err != nil {
		return nil, err
	}
	if input < 0 || output < 0 || math.IsInf(input, 0) || math.IsInf(output, 0) || math.IsNaN(input) || math.IsNaN(output) {
		return nil, errors.New("identity shadow prices must be finite and nonnegative")
	}
	return &Prices{InputPerMillion: input, OutputPerMillion: output}, nil
}

func parseTimeout(value, label string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < time.Second || duration > 120*time.Second {
		return 0, fmt.Errorf("identity shadow %s must be from 1s through 120s", label)
	}
	return duration, nil
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" && parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == "" && !strings.ContainsAny(value, "#\r\n\t ")
}

func mappingFields(node *yaml.Node, label string, allowed map[string]struct{}) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, fmt.Errorf("identity shadow %s must be a mapping", label)
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || strings.TrimSpace(key.Value) == "" || key.Value == "<<" {
			return nil, fmt.Errorf("identity shadow %s keys must be nonblank strings", label)
		}
		if _, duplicate := fields[key.Value]; duplicate {
			return nil, fmt.Errorf("identity shadow %s contains a duplicate key", label)
		}
		if _, known := allowed[key.Value]; !known {
			return nil, fmt.Errorf("identity shadow %s contains an unknown field", label)
		}
		fields[key.Value] = node.Content[index+1]
	}
	return fields, nil
}

func stringValue(node *yaml.Node, label string) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || strings.TrimSpace(node.Value) == "" {
		return "", fmt.Errorf("identity shadow %s must be a nonblank string", label)
	}
	return node.Value, nil
}

func integerValue(node *yaml.Node, label string) (int, error) {
	value, err := integer64Value(node, label)
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("identity shadow %s is outside integer bounds", label)
	}
	return int(value), nil
}

func integer64Value(node *yaml.Node, label string) (int64, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, fmt.Errorf("identity shadow %s must be an integer", label)
	}
	text := strings.ReplaceAll(node.Value, "_", "")
	value, err := strconv.ParseInt(text, 0, 64)
	if err != nil {
		value, err = strconv.ParseInt(text, 10, 64)
	}
	if err != nil {
		return 0, fmt.Errorf("identity shadow %s must be an integer", label)
	}
	return value, nil
}

func floatValue(node *yaml.Node, label string) (float64, error) {
	if node == nil || node.Kind != yaml.ScalarNode || (node.Tag != "!!float" && node.Tag != "!!int") {
		return 0, fmt.Errorf("identity shadow %s must be a number", label)
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(node.Value, "_", ""), 64)
	if err != nil {
		return 0, fmt.Errorf("identity shadow %s must be a number", label)
	}
	return value, nil
}

func rejectYAMLAliases(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return errors.New("identity shadow config may not contain YAML aliases")
	}
	for _, child := range node.Content {
		if err := rejectYAMLAliases(child); err != nil {
			return err
		}
	}
	return nil
}

func setOf(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
