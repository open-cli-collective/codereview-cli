package identityshadow

import (
	"strings"
	"testing"
	"time"
)

const validConfigYAML = "schema_version: 1\n" +
	"provider:\n" +
	"  kind: system_one\n" +
	"  endpoint: https://inference.example.test/v1/systemone\n" +
	"  credential:\n" +
	"    store: local-os\n" +
	"    name: codereview/identity-baseten\n" +
	"  auth_scheme: api-key\n" +
	"model: decision-models/mammoth-test\n" +
	"expected_model: decision-models/mammoth-test-2026-11\n" +
	"limits:\n" +
	"  max_state_bytes: 6144\n" +
	"  max_request_bytes: 8192\n" +
	"budget:\n" +
	"  max_calls: 40\n" +
	"  max_total_request_bytes: 327680\n" +
	"timeout: 10s\n" +
	"inference_timeout: 30s\n"

func TestParseConfigStrictSchemaAndBounds(t *testing.T) {
	config, err := parseConfig([]byte(validConfigYAML))
	if err != nil {
		t.Fatalf("parseConfig(valid): %v", err)
	}
	if config.Provider.Credential.Store != "local-os" || config.Provider.Credential.Name != "codereview/identity-baseten" {
		t.Fatalf("credential reference = %#v", config.Provider.Credential)
	}
	if config.Timeout != 10*time.Second || config.InferenceLimit != 30*time.Second {
		t.Fatalf("timeouts = %s / %s", config.Timeout, config.InferenceLimit)
	}
	if config.Budget.MaxCalls != 40 || config.Budget.MaxTotalRequestBytes != 327680 {
		t.Fatalf("budget = %#v", config.Budget)
	}

	for name, replacement := range map[string]string{
		"unknown top-level field":   strings.Replace(validConfigYAML, "timeout: 10s", "api_key_env: SECRET\ntimeout: 10s", 1),
		"old timeout alias":         strings.Replace(validConfigYAML, "inference_timeout: 30s", "total_timeout: 30s\ninference_timeout: 30s", 1),
		"integer string coercion":   strings.Replace(validConfigYAML, "max_calls: 40", "max_calls: \"40\"", 1),
		"string coercion":           strings.Replace(validConfigYAML, "model: decision-models/mammoth-test", "model: true", 1),
		"unsupported provider":      strings.Replace(validConfigYAML, "kind: system_one", "kind: openai_decisions", 1),
		"invalid endpoint":          strings.Replace(validConfigYAML, "https://inference.example.test/v1/systemone", "https://user:pass@inference.example.test/v1/systemone?debug=1", 1),
		"timeout exceeds inference": strings.Replace(validConfigYAML, "timeout: 10s", "timeout: 31s", 1),
		"state exceeds request":     strings.Replace(validConfigYAML, "max_state_bytes: 6144", "max_state_bytes: 9000", 1),
		"partial prices":            validConfigYAML + "prices:\n  input_per_million: 0.1\n",
		"nonfinite prices":          validConfigYAML + "prices:\n  input_per_million: .nan\n  output_per_million: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(replacement)); err == nil {
				t.Fatal("parseConfig unexpectedly accepted invalid input")
			}
		})
	}
}

func TestParseConfigRejectsDuplicateAliasesAndMultipleDocuments(t *testing.T) {
	duplicate := strings.Replace(validConfigYAML, "timeout: 10s", "timeout: 10s\ntimeout: 11s", 1)
	aliased := strings.Replace(validConfigYAML, "provider:\n", "provider: &selected\n", 1)
	aliased = strings.Replace(aliased, "model: decision-models/mammoth-test", "model: *selected", 1)
	for name, content := range map[string]string{
		"duplicate key":      duplicate,
		"alias":              aliased,
		"multiple documents": validConfigYAML + "---\nschema_version: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(content)); err == nil {
				t.Fatal("parseConfig unexpectedly accepted invalid YAML")
			}
		})
	}
}

func TestParseConfigRejectsOversizedFile(t *testing.T) {
	if _, err := parseConfig([]byte(strings.Repeat("x", maxConfigBytes+1))); err == nil {
		t.Fatal("parseConfig accepted a file above the read cap")
	}
}
