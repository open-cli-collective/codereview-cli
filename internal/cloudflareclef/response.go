package cloudflareclef

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

type envelope struct {
	root    map[string]json.RawMessage
	result  json.RawMessage
	success bool
	errors  []json.RawMessage
}

func decodeEnvelope(data []byte) (envelope, error) {
	if len(data) == 0 || !utf8.Valid(data) || !json.Valid(data) || rejectDuplicateKeys(data) != nil {
		return envelope{}, newError(errorMalformedResponse, "cloudflareclef: malformed response")
	}
	root, ok := jsonObject(data)
	if !ok {
		return envelope{}, newError(errorMalformedResponse, "cloudflareclef: malformed response")
	}
	success, ok := jsonBool(root["success"])
	if !ok {
		return envelope{}, newError(errorMalformedResponse, "cloudflareclef: malformed response")
	}
	var errorsList []json.RawMessage
	if raw, exists := root["errors"]; exists {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(trimmed, &errorsList) != nil || errorsList == nil {
			return envelope{}, newError(errorMalformedResponse, "cloudflareclef: malformed response")
		}
	}
	result := root["result"]
	if success {
		if _, ok := jsonObject(result); !ok {
			return envelope{}, newError(errorMalformedResponse, "cloudflareclef: successful response result was invalid")
		}
	}
	return envelope{root: root, result: result, success: success, errors: errorsList}, nil
}

func (e envelope) resultObject() (map[string]json.RawMessage, bool) {
	return jsonObject(e.result)
}

func decodeResult(rawResult json.RawMessage, request systemone.Request, limits systemone.Limits) (systemone.Response, *int64, bool, error) {
	result, ok := jsonObject(rawResult)
	if !ok {
		return systemone.Response{}, nil, true, newError(errorMalformedResponse, "cloudflareclef: malformed response")
	}
	var totalTokens *int64
	var totalErr error
	if rawUsage, exists := result["usage"]; exists {
		if usageFields, ok := jsonObject(rawUsage); ok {
			if rawTotal, present := usageFields["total_tokens"]; present {
				var total int64
				if isJSONNull(rawTotal) || json.Unmarshal(rawTotal, &total) != nil || total < 0 {
					totalErr = newError(errorMalformedResponse, "cloudflareclef: response usage was invalid")
				} else {
					totalTokens = &total
				}
			}
		}
	}

	projectedResult, err := projectUsageValue(rawResult)
	if err != nil {
		return systemone.Response{}, nil, true, newError(errorMalformedResponse, "cloudflareclef: malformed response")
	}
	decoded, decodeErr := systemone.Decode(projectedResult, request, limits)
	if totalErr != nil {
		return decoded, nil, false, totalErr
	}
	return decoded, totalTokens, true, decodeErr
}

func projectUsageValue(rawResult json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(rawResult))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("invalid result object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("invalid result key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key != "usage" {
			continue
		}
		end := decoder.InputOffset()
		start := end - int64(len(value))
		if start < 0 || start > end || end > int64(len(rawResult)) || !bytes.Equal(rawResult[start:end], value) {
			return nil, errors.New("could not locate result usage value")
		}
		usageFields, ok := jsonObject(value)
		if !ok {
			return rawResult, nil
		}
		projectedUsage := []byte{'{'}
		for _, name := range []string{"input_tokens", "output_tokens"} {
			if tokenValue, present := usageFields[name]; present {
				if len(projectedUsage) > 1 {
					projectedUsage = append(projectedUsage, ',')
				}
				projectedUsage = append(projectedUsage, '"')
				projectedUsage = append(projectedUsage, name...)
				projectedUsage = append(projectedUsage, '"', ':')
				projectedUsage = append(projectedUsage, tokenValue...)
			}
		}
		projectedUsage = append(projectedUsage, '}')
		projectedResult := make([]byte, 0, len(rawResult)-len(value)+len(projectedUsage))
		projectedResult = append(projectedResult, rawResult[:start]...)
		projectedResult = append(projectedResult, projectedUsage...)
		projectedResult = append(projectedResult, rawResult[end:]...)
		return projectedResult, nil
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("invalid result object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing result data")
		}
		return nil, err
	}
	return rawResult, nil
}

func applyMetadata(receipt *Receipt, response systemone.Response, totalTokens *int64, totalValid bool) {
	if response.Model != "" {
		receipt.ReturnedModel = response.Model
	}
	if response.Usage != nil && totalValid {
		receipt.Usage = &Usage{
			InputTokens:  response.Usage.InputTokens,
			OutputTokens: response.Usage.OutputTokens,
			TotalTokens:  totalTokens,
		}
	}
}

func incompleteSignal(fields map[string]json.RawMessage) (string, bool) {
	for _, name := range []string{"context_overflow", "context_limit_exceeded", "input_too_long", "prompt_too_long"} {
		if raw, exists := fields[name]; exists {
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
		if raw, exists := fields[name]; exists {
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
		if raw, exists := fields[name]; exists {
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
	if raw, exists := fields["incomplete_details"]; exists && !isJSONNull(raw) {
		return errorTruncated, true
	}
	return "", false
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
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
