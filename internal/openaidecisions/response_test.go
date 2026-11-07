package openaidecisions

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParseResponseAcceptsExactNamedChoiceAndProbabilityArray(t *testing.T) {
	request := choiceRequest()
	answer, model, usage, err := parseResponse(successBody(request.Model), request)
	if err != nil {
		t.Fatalf("parseResponse: %v (%s)", err, ErrorCode(err))
	}
	if model != request.Model || answer == nil || answer.Choice != "merge" || answer.Confidence != 0.95 || len(answer.Probabilities) != 2 || answer.Probabilities["merge"] != 0.95 || answer.Probabilities["separate"] != 0.05 {
		t.Fatalf("parsed result: model=%q answer=%#v", model, answer)
	}
	if usage == nil || usage.InputTokens != 32 || usage.OutputTokens != 3 || usage.CachedInputTokens == nil || *usage.CachedInputTokens != 4 || usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestParseResponseStrictlyRejectsMalformedAnswersAndJSON(t *testing.T) {
	request := choiceRequest()
	valid := string(successBody(request.Model))
	duplicateAnswer := `{"type":"choice","name":"route","choice":"merge","confidence":0.95,"probabilities":[{"value":"merge","probability":0.95},{"value":"separate","probability":0.05}]}`
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "invalid JSON", body: `{"model":`},
		{name: "root scalar", body: `1`},
		{name: "root null", body: `null`},
		{name: "invalid UTF-8", body: string(append(append([]byte(nil), successBody(request.Model)...), 0xff))},
		{name: "duplicate model field", body: strings.Replace(valid, `"model":"provider/model-one"`, `"model":"provider/model-one","model":"provider/model-one"`, 1)},
		{name: "duplicate answer name field", body: strings.Replace(valid, `"name":"route"`, `"name":"route","name":"route"`, 1)},
		{name: "duplicate choice field", body: strings.Replace(valid, `"choice":"merge"`, `"choice":"merge","choice":"separate"`, 1)},
		{name: "duplicate probability field", body: strings.Replace(valid, `"probability":0.95`, `"probability":0.95,"probability":0.05`, 1)},
		{name: "answers missing", body: `{"model":"provider/model-one","usage":{"input_tokens":32,"output_tokens":3}}`},
		{name: "answers null", body: strings.Replace(valid, `"answers":[`, `"answers":null, "unused":[`, 1)},
		{name: "answers scalar", body: strings.Replace(valid, `"answers":[`, `"answers":true, "unused":[`, 1)},
		{name: "answers object", body: strings.Replace(valid, `"answers":[`, `"answers":{}, "unused":[`, 1)},
		{name: "empty answer array", body: strings.Replace(valid, `"answers":[{"type":"choice","name":"route","choice":"merge","confidence":0.95,"probabilities":[{"value":"merge","probability":0.95},{"value":"separate","probability":0.05}]}]`, `"answers":[]`, 1)},
		{name: "extra answer", body: strings.Replace(valid, `}],"usage"`, `},`+duplicateAnswer+`],"usage"`, 1)},
		{name: "answer null", body: strings.Replace(valid, `"answers":[{"type":"choice","name":"route","choice":"merge","confidence":0.95,"probabilities":[{"value":"merge","probability":0.95},{"value":"separate","probability":0.05}]}]`, `"answers":[null]`, 1)},
		{name: "wrong answer name", body: strings.Replace(valid, `"name":"route"`, `"name":"other"`, 1)},
		{name: "null answer name", body: strings.Replace(valid, `"name":"route"`, `"name":null`, 1)},
		{name: "wrong answer type", body: strings.Replace(valid, `"type":"choice"`, `"type":"prose"`, 1)},
		{name: "boolean choice", body: strings.Replace(valid, `"choice":"merge"`, `"choice":true`, 1)},
		{name: "null choice", body: strings.Replace(valid, `"choice":"merge"`, `"choice":null`, 1)},
		{name: "unknown choice", body: strings.Replace(valid, `"choice":"merge"`, `"choice":"other"`, 1)},
		{name: "null confidence", body: strings.Replace(valid, `"confidence":0.95`, `"confidence":null`, 1)},
		{name: "boolean confidence", body: strings.Replace(valid, `"confidence":0.95`, `"confidence":true`, 1)},
		{name: "missing confidence", body: strings.Replace(valid, `,"confidence":0.95`, "", 1)},
		{name: "confidence out of range", body: strings.Replace(valid, `"confidence":0.95`, `"confidence":1.01`, 1)},
		{name: "nonfinite confidence", body: strings.Replace(valid, `"confidence":0.95`, `"confidence":1e999`, 1)},
		{name: "null probabilities", body: strings.Replace(valid, `"probabilities":[`, `"probabilities":null, "unused":[`, 1)},
		{name: "probabilities scalar", body: strings.Replace(valid, `"probabilities":[`, `"probabilities":false, "unused":[`, 1)},
		{name: "missing probability value", body: strings.Replace(valid, `"value":"merge"`, `"value":"absent"`, 1)},
		{name: "boolean probability value", body: strings.Replace(valid, `"value":"merge"`, `"value":true`, 1)},
		{name: "null probability", body: strings.Replace(valid, `"probability":0.95`, `"probability":null`, 1)},
		{name: "negative probability", body: strings.Replace(valid, `"probability":0.05`, `"probability":-0.05`, 1)},
		{name: "probability above one", body: strings.Replace(valid, `"probability":0.95`, `"probability":1.1`, 1)},
		{name: "probability sum too low", body: strings.Replace(valid, `"probability":0.95`, `"probability":0.90`, 1)},
		{name: "extra probability item", body: strings.Replace(valid, `{"value":"separate","probability":0.05}`, `{"value":"separate","probability":0.05},{"value":"other","probability":0}`, 1)},
		{name: "duplicate probability value", body: strings.Replace(valid, `{"value":"separate","probability":0.05}`, `{"value":"merge","probability":0.05}`, 1)},
		{name: "probability item extra field", body: strings.Replace(valid, `"probability":0.95`, `"probability":0.95,"label":"private"`, 1)},
		{name: "answer extra field", body: strings.Replace(valid, `"confidence":0.95`, `"confidence":0.95,"reason":"private prose"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			if test.name == "extra answer" && !json.Valid(body) {
				t.Fatal("extra-answer fixture must be valid JSON")
			}
			answer, _, _, err := parseResponse(body, request)
			if err == nil || ErrorCode(err) != "malformed_response" || answer != nil {
				t.Fatalf("parseResponse = answer:%#v err:%v (%s), want malformed_response", answer, err, ErrorCode(err))
			}
		})
	}
}

func TestParseResponseValidatesExpectedModelBeforeRetainingMetadata(t *testing.T) {
	request := choiceRequest()
	request.ExpectedModel = "provider/model-resolved"
	response := successBody(request.ExpectedModel)
	answer, model, usage, err := parseResponse(response, request)
	if err != nil || answer == nil || model != request.ExpectedModel || usage == nil {
		t.Fatalf("expected model result: model=%q answer=%#v usage=%#v err=%v", model, answer, usage, err)
	}
	answer, model, usage, err = parseResponse(successBody(request.Model), request)
	if err == nil || ErrorCode(err) != "malformed_response" || answer != nil || model != "" || usage != nil {
		t.Fatalf("unexpected model result: model=%q answer=%#v usage=%#v err=%v (%s)", model, answer, usage, err, ErrorCode(err))
	}
}

func TestParseResponsePreservesValidUsageOnAnswerAndIncompleteFailures(t *testing.T) {
	valid := string(successBody("provider/model-one"))
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "malformed choice", body: strings.Replace(valid, `"choice":"merge"`, `"choice":true`, 1), code: "malformed_response"},
		{name: "truncated", body: strings.Replace(valid, `,"usage"`, `,"truncated":true,"usage"`, 1), code: "truncated"},
		{name: "context overflow", body: strings.Replace(valid, `,"usage"`, `,"context_overflow":true,"usage"`, 1), code: "context_limit"},
		{name: "provider error", body: strings.Replace(valid, `,"answers"`, `,"error":{"message":"private provider prose"},"answers"`, 1), code: "upstream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, model, usage, err := parseResponse([]byte(test.body), choiceRequest())
			if err == nil || ErrorCode(err) != test.code || answer != nil {
				t.Fatalf("parseResponse = answer:%#v model:%q usage:%#v err:%v (%s)", answer, model, usage, err, ErrorCode(err))
			}
			if model != "provider/model-one" || usage == nil || usage.InputTokens != 32 || usage.OutputTokens != 3 {
				t.Fatalf("validated metadata lost: model=%q usage=%#v", model, usage)
			}
		})
	}
}

func TestParseResponseRejectsInvalidUsageAndDoesNotTrustIt(t *testing.T) {
	valid := string(successBody("provider/model-one"))
	tests := []struct {
		name              string
		body              string
		wantReturnedModel bool
	}{
		{name: "missing usage", body: strings.Replace(valid, `,"usage":`, `,"unused":`, 1), wantReturnedModel: true},
		{name: "missing input tokens", body: strings.Replace(valid, `"input_tokens":32,`, "", 1), wantReturnedModel: true},
		{name: "missing output tokens", body: strings.Replace(valid, `,"output_tokens":3`, "", 1), wantReturnedModel: true},
		{name: "null input tokens", body: strings.Replace(valid, `"input_tokens":32`, `"input_tokens":null`, 1), wantReturnedModel: true},
		{name: "negative output tokens", body: strings.Replace(valid, `"output_tokens":3`, `"output_tokens":-1`, 1), wantReturnedModel: true},
		{name: "fractional input tokens", body: strings.Replace(valid, `"input_tokens":32`, `"input_tokens":32.5`, 1), wantReturnedModel: true},
		{name: "string input tokens", body: strings.Replace(valid, `"input_tokens":32`, `"input_tokens":"32"`, 1), wantReturnedModel: true},
		{name: "overflow input tokens", body: strings.Replace(valid, `"input_tokens":32`, `"input_tokens":9223372036854775808`, 1), wantReturnedModel: true},
		{name: "negative cached tokens", body: strings.Replace(valid, `"cached_tokens":4`, `"cached_tokens":-1`, 1), wantReturnedModel: true},
		{name: "fractional cached tokens", body: strings.Replace(valid, `"cached_tokens":4`, `"cached_tokens":4.5`, 1), wantReturnedModel: true},
		{name: "string cached tokens", body: strings.Replace(valid, `"cached_tokens":4`, `"cached_tokens":"4"`, 1), wantReturnedModel: true},
		{name: "cached tokens exceed input", body: strings.Replace(valid, `"cached_tokens":4`, `"cached_tokens":33`, 1), wantReturnedModel: true},
		{name: "invalid cache write tokens", body: strings.Replace(valid, `"cache_write_tokens":2`, `"cache_write_tokens":false`, 1), wantReturnedModel: true},
		{name: "details scalar", body: strings.Replace(valid, `"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2}`, `"input_tokens_details":true`, 1), wantReturnedModel: true},
		{name: "duplicate usage field", body: strings.Replace(valid, `"input_tokens":32`, `"input_tokens":32,"input_tokens":33`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, model, usage, err := parseResponse([]byte(test.body), choiceRequest())
			if err == nil || ErrorCode(err) != "malformed_response" || answer != nil || usage != nil {
				t.Fatalf("parseResponse = answer:%#v model:%q usage:%#v err:%v (%s)", answer, model, usage, err, ErrorCode(err))
			}
			if test.wantReturnedModel && model != "provider/model-one" {
				t.Fatalf("matched model metadata lost on usage error: %q", model)
			}
			if !test.wantReturnedModel && model != "" {
				t.Fatalf("untrusted model metadata retained: %q", model)
			}
		})
	}
}

func TestParseUsageAllowsUnknownCacheCountsButRejectsMalformedCacheMetadata(t *testing.T) {
	validBase := `{"input_tokens":12,"output_tokens":3}`
	for _, raw := range []string{
		validBase,
		`{"input_tokens":12,"output_tokens":3,"input_tokens_details":null}`,
		`{"input_tokens":12,"output_tokens":3,"input_tokens_details":{}}`,
		`{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":null,"cache_write_tokens":null}}`,
		`{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"other_detail":{"ok":true}}}`,
	} {
		usage, ok := parseUsage(json.RawMessage(raw))
		if !ok || usage.InputTokens != 12 || usage.OutputTokens != 3 || usage.CachedInputTokens != nil || usage.CacheWriteTokens != nil {
			t.Errorf("parseUsage(%s) = %#v, %t; expected base counts and unknown cache counts", raw, usage, ok)
		}
	}
	for _, test := range []struct {
		raw        string
		cached     *int64
		cacheWrite *int64
	}{
		{raw: `{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":12}}`, cached: int64Pointer(0), cacheWrite: int64Pointer(12)},
		{raw: `{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":null,"cache_write_tokens":0}}`, cacheWrite: int64Pointer(0)},
	} {
		usage, ok := parseUsage(json.RawMessage(test.raw))
		if !ok || usage.InputTokens != 12 || !sameOptionalInt64(usage.CachedInputTokens, test.cached) || !sameOptionalInt64(usage.CacheWriteTokens, test.cacheWrite) {
			t.Errorf("parseUsage valid edge %s = %#v, %t", test.raw, usage, ok)
		}
	}
}

func sameOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func TestParseResponseHandlesRefusalAndStrictIncompleteFlags(t *testing.T) {
	request := choiceRequest()
	refusal := []byte(`{"model":"provider/model-one","answers":[{"type":"refusal","name":"route"}],"usage":{"input_tokens":1,"output_tokens":0}}`)
	answer, model, usage, err := parseResponse(refusal, request)
	if err == nil || ErrorCode(err) != "refused" || answer != nil || model != request.Model || usage == nil {
		t.Fatalf("refusal = answer:%#v model:%q usage:%#v err:%v (%s)", answer, model, usage, err, ErrorCode(err))
	}
	for _, test := range []struct {
		name string
		flag string
		code string
	}{
		{name: "truncated", flag: `"truncated":true`, code: "truncated"},
		{name: "context overflow", flag: `"context_overflow":true`, code: "context_limit"},
		{name: "null flag", flag: `"truncated":null`, code: "malformed_response"},
		{name: "wrong flag type", flag: `"context_overflow":"yes"`, code: "malformed_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(string(successBody(request.Model)), `,"usage"`, `,`+test.flag+`,"usage"`, 1)
			answer, model, usage, err := parseResponse([]byte(body), request)
			if err == nil || ErrorCode(err) != test.code || answer != nil || model != request.Model || usage == nil {
				t.Fatalf("parseResponse = answer:%#v model:%q usage:%#v err:%v (%s)", answer, model, usage, err, ErrorCode(err))
			}
		})
	}
}

func TestProbabilityValidationRejectsNaNAndInfinityValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.01, 1.01} {
		if finiteUnit(value) {
			t.Errorf("finiteUnit(%v) = true", value)
		}
	}
	if !finiteUnit(0) || !finiteUnit(1) {
		t.Fatal("finiteUnit rejected an endpoint probability")
	}
}

func TestRefusalAndMalformedAnswerErrorsAreStatic(t *testing.T) {
	request := choiceRequest()
	providerText := "private provider prose and a secret-key"
	body := []byte(strings.Replace(string(successBody(request.Model)), `"choice":"merge"`, `"choice":"`+providerText+`"`, 1))
	_, _, _, err := parseResponse(body, request)
	if err == nil || ErrorCode(err) != "malformed_response" || strings.Contains(err.Error(), providerText) {
		t.Fatalf("unsafe malformed-response error: %v (%s)", err, ErrorCode(err))
	}
	refusal := []byte(`{"model":"provider/model-one","answers":[{"type":"refusal","name":"route"}],"usage":{"input_tokens":1,"output_tokens":0}}`)
	_, _, _, err = parseResponse(refusal, request)
	if err == nil || ErrorCode(err) != "refused" || strings.Contains(err.Error(), "route") {
		t.Fatalf("unsafe refusal error: %v (%s)", err, ErrorCode(err))
	}
}

func TestResponseMetadataRejectsDuplicateKeysInAnyNestedObject(t *testing.T) {
	valid := string(successBody("provider/model-one"))
	for _, duplicate := range []struct {
		name string
		from string
		to   string
	}{
		{name: "top-level", from: `"model":"provider/model-one"`, to: `"model":"provider/model-one","model":"provider/model-one"`},
		{name: "answer", from: `"choice":"merge"`, to: `"choice":"merge","choice":"separate"`},
		{name: "usage", from: `"input_tokens":32`, to: `"input_tokens":32,"input_tokens":32`},
		{name: "probability", from: `"probability":0.95`, to: `"probability":0.95,"probability":0.95`},
	} {
		t.Run(duplicate.name, func(t *testing.T) {
			body := strings.Replace(valid, duplicate.from, duplicate.to, 1)
			_, model, usage, err := parseResponse([]byte(body), choiceRequest())
			if err == nil || ErrorCode(err) != "malformed_response" || model != "" || usage != nil {
				t.Fatalf("duplicate key retained metadata: model=%q usage=%#v err=%v (%s)", model, usage, err, ErrorCode(err))
			}
		})
	}
	invalidUTF8 := bytes.Replace(successBody("provider/model-one"), []byte("merge"), []byte{0xff, 'e', 'r', 'g', 'e'}, 1)
	if _, model, usage, err := parseResponse(invalidUTF8, choiceRequest()); err == nil || ErrorCode(err) != "malformed_response" || model != "" || usage != nil {
		t.Fatalf("invalid UTF-8 response metadata: model=%q usage=%#v err=%v (%s)", model, usage, err, ErrorCode(err))
	}
}
