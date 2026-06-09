package llm

import "encoding/json"

// extractSingleJSONObject returns the single balanced top-level JSON object
// found in data, if exactly one syntactically valid candidate exists. Zero or
// multiple valid candidates return ok=false: ambiguous output must not be
// recovered.
func extractSingleJSONObject(data []byte) ([]byte, bool) {
	var (
		candidate []byte
		found     bool
	)
	for i := 0; i < len(data); i++ {
		if data[i] != '{' {
			continue
		}
		span, end, balanced := scanBalancedObject(data, i)
		if !balanced {
			continue
		}
		if json.Valid(span) {
			if found {
				return nil, false
			}
			candidate = span
			found = true
		}
		i = end
	}
	return candidate, found
}

// scanBalancedObject scans a {...} span starting at data[start], tracking
// string literals and escapes so braces inside strings are ignored. It returns
// the span, the index of its closing brace, and whether it balanced.
func scanBalancedObject(data []byte, start int) ([]byte, int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(data); i++ {
		c := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return data[start : i+1], i, true
			}
		}
	}
	return nil, len(data), false
}
