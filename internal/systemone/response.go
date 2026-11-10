package systemone

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

func parseResponse(data []byte, request Request) (map[string]ChoiceAnswer, string, *Usage, error) {
	root, model, usage, metadataErr := decodeResponseMetadata(data, expectedModel(request))
	if root == nil {
		return nil, "", nil, metadataErr
	}
	if category, failed := incompleteSignal(root); failed {
		if category == errorContextLimit {
			return nil, model, usage, newError(category, "systemone: provider reported a context limit")
		}
		if category == errorTruncated {
			return nil, model, usage, newError(category, "systemone: provider reported incomplete output")
		}
		return nil, model, usage, newError(errorMalformedResponse, "systemone: malformed response")
	}
	if rawError, exists := root["error"]; exists && !isJSONNull(rawError) {
		return nil, model, usage, newError(errorUpstream, "systemone: provider reported an error")
	}
	if metadataErr != nil {
		return nil, model, usage, metadataErr
	}
	answers, ok := parseAnswers(root["answers"], request.Questions)
	if !ok {
		return nil, model, usage, newError(errorMalformedResponse, "systemone: response answers were invalid")
	}
	return answers, model, usage, nil
}

func decodeResponseMetadata(data []byte, expectedModel string) (map[string]json.RawMessage, string, *Usage, error) {
	if len(data) == 0 || !utf8.Valid(data) || !json.Valid(data) || rejectDuplicateKeys(data) != nil {
		return nil, "", nil, newError(errorMalformedResponse, "systemone: malformed response")
	}
	root, ok := object(data)
	if !ok {
		return nil, "", nil, newError(errorMalformedResponse, "systemone: malformed response")
	}
	model, ok := jsonString(root["model"])
	if !ok || model != expectedModel {
		return root, "", nil, newError(errorMalformedResponse, "systemone: response model did not match expectation")
	}
	usageValue, ok := parseUsage(root["usage"])
	if !ok {
		return root, model, nil, newError(errorMalformedResponse, "systemone: response usage was invalid")
	}
	return root, model, &usageValue, nil
}

func responseMetadata(data []byte, expectedModel string) (string, *Usage) {
	_, model, usage, _ := decodeResponseMetadata(data, expectedModel)
	return model, usage
}

func parseUsage(raw json.RawMessage) (Usage, bool) {
	fields, ok := object(raw)
	if !ok || !hasOnlyFields(fields, "input_tokens", "output_tokens") {
		return Usage{}, false
	}
	inputTokens, ok := jsonInt64(fields["input_tokens"])
	if !ok {
		return Usage{}, false
	}
	outputTokens, ok := jsonInt64(fields["output_tokens"])
	if !ok || inputTokens < 0 || outputTokens < 0 {
		return Usage{}, false
	}
	return Usage{InputTokens: inputTokens, OutputTokens: outputTokens}, true
}

func parseAnswers(raw json.RawMessage, questions map[string]ChoiceQuestion) (map[string]ChoiceAnswer, bool) {
	fields, ok := object(raw)
	if !ok || len(fields) != len(questions) {
		return nil, false
	}
	answers := make(map[string]ChoiceAnswer, len(questions))
	for name, question := range questions {
		answerRaw, exists := fields[name]
		if !exists {
			return nil, false
		}
		answerFields, ok := object(answerRaw)
		if !ok || !hasOnlyFields(answerFields, "type", "choice", "confidence", "probabilities") {
			return nil, false
		}
		typeValue, ok := jsonString(answerFields["type"])
		if !ok || typeValue != "choice" {
			return nil, false
		}
		choice, ok := jsonString(answerFields["choice"])
		if !ok {
			return nil, false
		}
		if _, allowed := question.Criteria[choice]; !allowed {
			return nil, false
		}
		confidence, ok := jsonFloat64(answerFields["confidence"])
		if !ok || !finiteUnit(confidence) {
			return nil, false
		}
		probabilityFields, ok := object(answerFields["probabilities"])
		if !ok || !sameKeys(probabilityFields, question.Criteria) {
			return nil, false
		}
		probabilities := make(map[string]float64, len(question.Criteria))
		total := 0.0
		for label := range question.Criteria {
			probability, valid := jsonFloat64(probabilityFields[label])
			if !valid || !finiteUnit(probability) {
				return nil, false
			}
			probabilities[label] = probability
			total += probability
		}
		if total < 0.98 || total > 1.02 {
			return nil, false
		}
		answers[name] = ChoiceAnswer{Choice: choice, Confidence: confidence, Probabilities: probabilities}
	}
	return answers, true
}

func sameKeys(left map[string]json.RawMessage, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range right {
		if _, exists := left[key]; !exists {
			return false
		}
	}
	return true
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
