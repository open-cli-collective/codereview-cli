package openaidecisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

func parseResponse(data []byte, request Request) (*Answer, string, *Usage, error) {
	root, model, usage, metadataErr := decodeResponseMetadata(data, expectedModel(request))
	if root == nil {
		return nil, "", nil, metadataErr
	}
	if category, failed := incompleteSignal(root); failed {
		if category == errorContextLimit {
			return nil, model, usage, newError(category, "openaidecisions: provider reported a context limit")
		}
		if category == errorTruncated {
			return nil, model, usage, newError(category, "openaidecisions: provider reported incomplete output")
		}
		return nil, model, usage, newError(errorMalformedResponse, "openaidecisions: malformed response")
	}
	if rawError, exists := root["error"]; exists && !isJSONNull(rawError) {
		return nil, model, usage, newError(errorUpstream, "openaidecisions: provider reported an error")
	}
	if metadataErr != nil {
		return nil, model, usage, metadataErr
	}
	answer, refused, ok := parseAnswer(root["answers"], request)
	if !ok {
		return nil, model, usage, newError(errorMalformedResponse, "openaidecisions: response answer was invalid")
	}
	if refused {
		return nil, model, usage, newError(errorRefused, "openaidecisions: provider refused the question")
	}
	return answer, model, usage, nil
}

func decodeResponseMetadata(data []byte, expectedModel string) (map[string]json.RawMessage, string, *Usage, error) {
	if len(data) == 0 || !utf8.Valid(data) || !json.Valid(data) || rejectDuplicateKeys(data) != nil {
		return nil, "", nil, newError(errorMalformedResponse, "openaidecisions: malformed response")
	}
	root, ok := object(data)
	if !ok {
		return nil, "", nil, newError(errorMalformedResponse, "openaidecisions: malformed response")
	}
	model, ok := jsonString(root["model"])
	if !ok || model != expectedModel {
		return root, "", nil, newError(errorMalformedResponse, "openaidecisions: response model did not match expectation")
	}
	usageValue, ok := parseUsage(root["usage"])
	if !ok {
		return root, model, nil, newError(errorMalformedResponse, "openaidecisions: response usage was invalid")
	}
	return root, model, &usageValue, nil
}

func responseMetadata(data []byte, expectedModel string) (string, *Usage) {
	_, model, usage, _ := decodeResponseMetadata(data, expectedModel)
	return model, usage
}

func parseUsage(raw json.RawMessage) (Usage, bool) {
	fields, ok := object(raw)
	if !ok {
		return Usage{}, false
	}
	inputTokens, ok := jsonInt64(fields["input_tokens"])
	if !ok || inputTokens < 0 {
		return Usage{}, false
	}
	outputTokens, ok := jsonInt64(fields["output_tokens"])
	if !ok || outputTokens < 0 {
		return Usage{}, false
	}
	usage := Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
	detailsRaw, exists := fields["input_tokens_details"]
	if !exists || isJSONNull(detailsRaw) {
		return usage, true
	}
	details, ok := object(detailsRaw)
	if !ok {
		return Usage{}, false
	}
	if rawCached, exists := details["cached_tokens"]; exists && !isJSONNull(rawCached) {
		cached, valid := jsonInt64(rawCached)
		if !valid || cached < 0 || cached > inputTokens {
			return Usage{}, false
		}
		usage.CachedInputTokens = int64Pointer(cached)
	}
	if rawCacheWrite, exists := details["cache_write_tokens"]; exists && !isJSONNull(rawCacheWrite) {
		cacheWrite, valid := jsonInt64(rawCacheWrite)
		if !valid || cacheWrite < 0 || cacheWrite > inputTokens {
			return Usage{}, false
		}
		usage.CacheWriteTokens = int64Pointer(cacheWrite)
	}
	return usage, true
}

func int64Pointer(value int64) *int64 { return &value }

func parseAnswer(raw json.RawMessage, request Request) (*Answer, bool, bool) {
	var answers []json.RawMessage
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &answers) != nil || len(answers) != 1 {
		return nil, false, false
	}
	fields, ok := object(answers[0])
	if !ok {
		return nil, false, false
	}
	typeValue, ok := jsonString(fields["type"])
	if !ok {
		return nil, false, false
	}
	name, ok := jsonString(fields["name"])
	if !ok || name != request.Name {
		return nil, false, false
	}
	if typeValue == "refusal" {
		if !hasOnlyFields(fields, "type", "name") {
			return nil, false, false
		}
		return nil, true, true
	}
	if typeValue != "choice" || !hasOnlyFields(fields, "type", "name", "choice", "confidence", "probabilities") {
		return nil, false, false
	}
	choice, ok := jsonString(fields["choice"])
	if !ok {
		return nil, false, false
	}
	allowed := make(map[string]struct{}, len(request.Choices))
	for _, candidate := range request.Choices {
		allowed[candidate.Value] = struct{}{}
	}
	if _, exists := allowed[choice]; !exists {
		return nil, false, false
	}
	confidence, ok := jsonFloat64(fields["confidence"])
	if !ok || !finiteUnit(confidence) {
		return nil, false, false
	}
	var probabilityItems []json.RawMessage
	probabilityRaw := fields["probabilities"]
	if len(probabilityRaw) == 0 || isJSONNull(probabilityRaw) || json.Unmarshal(probabilityRaw, &probabilityItems) != nil || len(probabilityItems) != len(request.Choices) {
		return nil, false, false
	}
	probabilities := make(map[string]float64, len(request.Choices))
	total := 0.0
	for _, item := range probabilityItems {
		probabilityFields, valid := object(item)
		if !valid || !hasOnlyFields(probabilityFields, "value", "probability") {
			return nil, false, false
		}
		value, valid := jsonString(probabilityFields["value"])
		if !valid {
			return nil, false, false
		}
		if _, exists := allowed[value]; !exists {
			return nil, false, false
		}
		if _, duplicate := probabilities[value]; duplicate {
			return nil, false, false
		}
		probability, valid := jsonFloat64(probabilityFields["probability"])
		if !valid || !finiteUnit(probability) {
			return nil, false, false
		}
		probabilities[value] = probability
		total += probability
	}
	if len(probabilities) != len(allowed) || total < 0.98 || total > 1.02 {
		return nil, false, false
	}
	return &Answer{Choice: choice, Confidence: confidence, Probabilities: probabilities}, false, true
}

func incompleteSignal(root map[string]json.RawMessage) (errorCategory, bool) {
	for _, name := range []string{"context_overflow", "context_limit_exceeded", "input_too_long", "prompt_too_long"} {
		if raw, exists := root[name]; exists {
			flag, valid := jsonBool(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			if flag {
				return errorContextLimit, true
			}
		}
	}
	for _, name := range []string{"truncated", "partial", "incomplete", "output_truncated"} {
		if raw, exists := root[name]; exists {
			flag, valid := jsonBool(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			if flag {
				return errorTruncated, true
			}
		}
	}
	for _, name := range []string{"finish_reason", "status"} {
		if raw, exists := root[name]; exists {
			value, valid := jsonString(raw)
			if !valid {
				return errorMalformedResponse, true
			}
			switch strings.ToLower(value) {
			case "context_limit", "context_overflow", "context_length", "context_length_exceeded", "input_too_long", "prompt_too_long":
				return errorContextLimit, true
			case "length", "max_tokens", "truncated", "partial", "incomplete":
				return errorTruncated, true
			}
		}
	}
	if raw, exists := root["incomplete_details"]; exists && !isJSONNull(raw) {
		return errorTruncated, true
	}
	return "", false
}

func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func hasOnlyFields(fields map[string]json.RawMessage, required ...string) bool {
	if len(fields) != len(required) {
		return false
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return false
		}
	}
	return true
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

func jsonInt64(raw json.RawMessage) (int64, bool) {
	var value int64
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func jsonFloat64(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func finiteUnit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := scanJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("invalid delimiter")
	}
	return nil
}
