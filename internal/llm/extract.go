package llm

import "encoding/json"

// extractJSONObjects returns every balanced, syntactically valid top-level JSON
// object in data, in order. Invalid spans are re-scanned one byte past their
// opening brace so prose braces wrapping a valid object cannot hide it; the
// worst case is quadratic in nesting depth, which is irrelevant at LLM
// response sizes.
func extractJSONObjects(data []byte) [][]byte {
	var candidates [][]byte
	for i := 0; i < len(data); i++ {
		if data[i] != '{' {
			continue
		}
		span, end, balanced := scanBalancedObject(data, i)
		if !balanced || !json.Valid(span) {
			continue
		}
		candidates = append(candidates, span)
		i = end
	}
	return candidates
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
