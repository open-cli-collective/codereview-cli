package findingutility

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CanonicalJSON marshals v and returns the strict canonical JSON form used by
// all utility digests. JSON object keys are sorted, array order is preserved,
// and duplicate keys/non-UTF-8 input are rejected.
func CanonicalJSON(v any) ([]byte, error) {
	if err := ValidateCanonicalStruct(v); err != nil {
		return nil, err
	}
	value, err := canonicalTypedValue(reflect.ValueOf(v))
	if err != nil {
		return nil, fmt.Errorf("findingutility: marshal canonical value: %w", err)
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// CanonicalizeJSON parses strict JSON and emits its canonical representation.
func CanonicalizeJSON(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("findingutility: JSON is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := parseJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("findingutility: trailing JSON tokens")
		}
		return nil, fmt.Errorf("findingutility: trailing JSON tokens: %w", err)
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DecodeStrict decodes a JSON object after enforcing duplicate-key and
// trailing-token rules. Unknown object fields are rejected by the decoder.
func DecodeStrict(data []byte, dst any) error {
	if _, err := CanonicalizeJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("findingutility: strict decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("findingutility: trailing JSON tokens")
		}
		return fmt.Errorf("findingutility: trailing JSON tokens: %w", err)
	}
	return nil
}

// DigestCanonical hashes the canonical JSON bytes with the project digest
// prefix. The prefix is part of the stored identity, not presentation text.
func DigestCanonical(v any) (Digest, error) {
	data, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return DigestBytes(data), nil
}

// DigestBytes hashes bytes without transforming them first. Use this for raw
// fixture responses and source artifacts whose exact bytes are significant.
func DigestBytes(data []byte) Digest {
	digest := sha256.Sum256(data)
	return Digest("sha256:" + hex.EncodeToString(digest[:]))
}

// ValidDigest reports whether d uses the required sha256 representation.
func ValidDigest(d Digest) bool {
	value := string(d)
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil && strings.ToLower(value[len("sha256:"):]) == value[len("sha256:"):]
}

func parseJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("findingutility: decode JSON: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("findingutility: decode object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("findingutility: object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return nil, fmt.Errorf("findingutility: duplicate JSON object key %q", key)
				}
				seen[key] = struct{}{}
				child, err := parseJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				if err == nil {
					err = fmt.Errorf("expected object close, got %v", end)
				}
				return nil, fmt.Errorf("findingutility: %w", err)
			}
			return object, nil
		case '[':
			var array []any
			for decoder.More() {
				child, err := parseJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, child)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				if err == nil {
					err = fmt.Errorf("expected array close, got %v", end)
				}
				return nil, fmt.Errorf("findingutility: %w", err)
			}
			return array, nil
		default:
			return nil, fmt.Errorf("findingutility: unexpected JSON delimiter %q", value)
		}
	case string, bool, nil:
		return value, nil
	case json.Number:
		return value, nil
	default:
		return nil, fmt.Errorf("findingutility: unsupported JSON token %T", token)
	}
}

func writeCanonical(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := marshalJSONNoHTML(value)
		if err != nil {
			return fmt.Errorf("findingutility: encode string: %w", err)
		}
		out.Write(encoded)
	case json.Number:
		canonical, err := canonicalNumber(value.String())
		if err != nil {
			return err
		}
		out.WriteString(canonical)
	case canonicalTypedNumber:
		out.WriteString(string(value))
	case canonicalRawJSON:
		out.Write(value)
	case []any:
		out.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			encoded, err := marshalJSONNoHTML(key)
			if err != nil {
				return fmt.Errorf("findingutility: encode object key: %w", err)
			}
			out.Write(encoded)
			out.WriteByte(':')
			if err := writeCanonical(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("findingutility: canonical value contains unsupported type %T", value)
	}
	return nil
}

type canonicalTypedNumber string
type canonicalRawJSON []byte

var (
	jsonNumberType     = reflect.TypeOf(json.Number(""))
	jsonRawMessageType = reflect.TypeOf(json.RawMessage(nil))
	byteSliceType      = reflect.TypeOf([]byte(nil))
)

func canonicalTypedValue(value reflect.Value) (any, error) {
	if !value.IsValid() {
		return nil, nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		return canonicalTypedValue(value.Elem())
	}
	if value.Type() == jsonNumberType {
		canonical, err := canonicalNumber(value.Interface().(json.Number).String())
		if err != nil {
			return nil, err
		}
		return canonicalTypedNumber(canonical), nil
	}
	if value.Type() == jsonRawMessageType {
		canonical, err := CanonicalizeJSON(value.Bytes())
		if err != nil {
			return nil, err
		}
		return canonicalRawJSON(canonical), nil
	}
	if value.CanInterface() {
		if marshaler, ok := value.Interface().(json.Marshaler); ok {
			data, err := marshaler.MarshalJSON()
			if err != nil {
				return nil, err
			}
			canonical, err := CanonicalizeJSON(data)
			if err != nil {
				return nil, err
			}
			return canonicalRawJSON(canonical), nil
		}
	}
	switch value.Kind() {
	case reflect.Bool:
		return value.Bool(), nil
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return nil, fmt.Errorf("findingutility: invalid UTF-8 string")
		}
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return canonicalTypedNumber(strconv.FormatInt(value.Int(), 10)), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return canonicalTypedNumber(strconv.FormatUint(value.Uint(), 10)), nil
	case reflect.Float32:
		return canonicalTypedNumber(formatTypedFloat(value.Float(), 32)), nil
	case reflect.Float64:
		return canonicalTypedNumber(formatTypedFloat(value.Float(), 64)), nil
	case reflect.Slice:
		if value.Type() == byteSliceType {
			if value.IsNil() {
				return nil, nil
			}
			return base64.StdEncoding.EncodeToString(value.Bytes()), nil
		}
		items := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			item, err := canonicalTypedValue(value.Index(index))
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", index, err)
			}
			items[index] = item
		}
		return items, nil
	case reflect.Array:
		items := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			item, err := canonicalTypedValue(value.Index(index))
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", index, err)
			}
			items[index] = item
		}
		return items, nil
	case reflect.Map:
		if value.IsNil() {
			return nil, nil
		}
		object := make(map[string]any, value.Len())
		for _, key := range value.MapKeys() {
			name, err := canonicalMapKey(key)
			if err != nil {
				return nil, err
			}
			item, err := canonicalTypedValue(value.MapIndex(key))
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", name, err)
			}
			object[name] = item
		}
		return object, nil
	case reflect.Struct:
		object := make(map[string]any)
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			tag := field.Tag.Get("json")
			name, options := splitJSONTag(tag)
			if name == "-" {
				continue
			}
			fieldValue := value.Field(index)
			if field.Anonymous && name == "" {
				child, err := canonicalTypedValue(fieldValue)
				if err != nil {
					return nil, err
				}
				if childObject, ok := child.(map[string]any); ok {
					for key, item := range childObject {
						object[key] = item
					}
					continue
				}
			}
			if name == "" {
				name = field.Name
			}
			if options["omitempty"] && isEmptyJSONValue(fieldValue) {
				continue
			}
			item, err := canonicalTypedValue(fieldValue)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", field.Name, err)
			}
			object[name] = item
		}
		return object, nil
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.Interface, reflect.Pointer, reflect.UnsafePointer:
		return canonicalTypedFallback(value)
	default:
		return canonicalTypedFallback(value)
	}
}

func canonicalTypedFallback(value reflect.Value) (any, error) {
	data, err := marshalJSONNoHTML(value.Interface())
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalizeJSON(data)
	if err != nil {
		return nil, err
	}
	return canonicalRawJSON(canonical), nil
}

func formatTypedFloat(value float64, bitSize int) string {
	if value == 0 {
		return "0"
	}
	return strconv.FormatFloat(value, 'g', -1, bitSize)
}

func canonicalMapKey(key reflect.Value) (string, error) {
	switch key.Kind() {
	case reflect.String:
		if !utf8.ValidString(key.String()) {
			return "", fmt.Errorf("findingutility: invalid UTF-8 map key")
		}
		return key.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(key.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(key.Uint(), 10), nil
	case reflect.Invalid, reflect.Bool, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.Struct, reflect.UnsafePointer:
		return "", fmt.Errorf("findingutility: unsupported map key type %s", key.Type())
	default:
		return "", fmt.Errorf("findingutility: unsupported map key type %s", key.Type())
	}
}

func splitJSONTag(tag string) (string, map[string]bool) {
	parts := strings.Split(tag, ",")
	name := ""
	if len(parts) > 0 {
		name = parts[0]
	}
	options := make(map[string]bool)
	for _, option := range parts[1:] {
		options[option] = true
	}
	return name, options
}

func isEmptyJSONValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Array:
		return value.Len() == 0
	case reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.Struct, reflect.UnsafePointer:
		return false
	}
	return false
}

func marshalJSONNoHTML(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	data := buffer.Bytes()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	return data, nil
}

func canonicalNumber(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("findingutility: empty JSON number")
	}
	if strings.ContainsAny(value, ".eE") {
		return canonicalDecimal(value)
	}
	if strings.HasPrefix(value, "+") {
		return "", fmt.Errorf("findingutility: invalid JSON number %q", value)
	}
	if value == "-0" {
		return "0", nil
	}
	if _, ok := new(bigInt).SetString(value, 10); !ok {
		return "", fmt.Errorf("findingutility: invalid JSON number %q", value)
	}
	return value, nil
}

// bigInt is a tiny alias kept here to avoid exposing a math/big value from the
// contract. It is only used to validate exact integer spellings.
type bigInt struct{}

func (*bigInt) SetString(value string, base int) (*bigInt, bool) {
	if base != 10 || value == "" {
		return nil, false
	}
	start := 0
	if value[0] == '-' {
		start = 1
	}
	if start == len(value) {
		return nil, false
	}
	if value[start] == '0' && len(value)-start > 1 {
		return nil, false
	}
	for _, r := range value[start:] {
		if r < '0' || r > '9' {
			return nil, false
		}
	}
	return &bigInt{}, true
}

func canonicalDecimal(value string) (string, error) {
	if strings.HasPrefix(value, "+") {
		return "", fmt.Errorf("findingutility: invalid JSON number %q", value)
	}
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = value[1:]
	}
	if value == "" {
		return "", fmt.Errorf("findingutility: invalid JSON number")
	}
	exponent := 0
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		parsed, err := strconv.Atoi(value[index+1:])
		if err != nil || parsed < -10000 || parsed > 10000 {
			return "", fmt.Errorf("findingutility: invalid JSON exponent")
		}
		exponent = parsed
		value = value[:index]
	}
	point := strings.IndexByte(value, '.')
	frac := 0
	if point >= 0 {
		frac = len(value) - point - 1
		value = value[:point] + value[point+1:]
	}
	if value == "" {
		return "", fmt.Errorf("findingutility: invalid JSON decimal")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("findingutility: invalid JSON decimal")
		}
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", nil
	}
	position := len(value) - frac + exponent
	for position < len(value) && value[len(value)-1] == '0' {
		value = value[:len(value)-1]
	}
	var result string
	switch {
	case position <= 0:
		result = "0." + strings.Repeat("0", -position) + value
	case position >= len(value):
		result = value + strings.Repeat("0", position-len(value))
	default:
		result = value[:position] + "." + value[position:]
	}
	if strings.Contains(result, ".") {
		result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	}
	if result == "" || result == "-0" {
		return "0", nil
	}
	if sign != "" && result != "0" {
		result = sign + result
	}
	if parsed, err := strconv.ParseFloat(result, 64); err == nil && math.IsInf(parsed, 0) {
		return "", fmt.Errorf("findingutility: non-finite JSON number")
	}
	return result, nil
}

// CanonicalValueEqual compares two JSON values after canonicalization. It is
// useful for verifier tests and avoids Go's map iteration order.
func CanonicalValueEqual(left, right any) bool {
	a, errA := CanonicalJSON(left)
	b, errB := CanonicalJSON(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// IsFinite reports whether a float can be serialized under the contract.
func IsFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// ValidateCanonicalStruct catches unsupported non-finite values before a
// caller computes a digest. It is intentionally small and recursive.
func ValidateCanonicalStruct(value any) error {
	return validateFinite(reflect.ValueOf(value), "root")
}

func validateFinite(value reflect.Value, path string) error {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		if !IsFinite(value.Float()) {
			return fmt.Errorf("findingutility: non-finite value at %s", path)
		}
	case reflect.String:
		if value.Type() != jsonNumberType && !utf8.ValidString(value.String()) {
			return fmt.Errorf("findingutility: invalid UTF-8 string at %s", path)
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			return validateFinite(value.Elem(), path)
		}
	case reflect.Array, reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			if err := validateFinite(value.Index(index), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			if key.Kind() == reflect.String && !utf8.ValidString(key.String()) {
				return fmt.Errorf("findingutility: invalid UTF-8 map key at %s", path)
			}
			if err := validateFinite(value.MapIndex(key), path); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath != "" {
				continue
			}
			if err := validateFinite(value.Field(index), path+"."+value.Type().Field(index).Name); err != nil {
				return err
			}
		}
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// These kinds do not contain recursively inspectable values.
	}
	return nil
}
