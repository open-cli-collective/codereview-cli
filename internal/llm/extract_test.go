package llm

import "testing"

func TestExtractJSONObjects(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"bare object", `{"a":1}`, []string{`{"a":1}`}},
		{"leading and trailing prose", `Sure! Here it is: {"a":1} Hope that helps.`, []string{`{"a":1}`}},
		{"nested objects and arrays", `prose {"a":{"b":1},"c":[{"d":2}]} prose`, []string{`{"a":{"b":1},"c":[{"d":2}]}`}},
		{"braces inside strings", `note {"msg":"use { and } freely"} end`, []string{`{"msg":"use { and } freely"}`}},
		{"escaped quotes inside strings", `{"msg":"she said \"hi\" {ok}"}`, []string{`{"msg":"she said \"hi\" {ok}"}`}},
		{"prose braces alongside one valid object", `Here is {the thing}: {"a":1}`, []string{`{"a":1}`}},
		{"valid object nested in invalid prose braces", `before {note {"ok":true}} after`, []string{`{"ok":true}`}},
		{"markdown fenced object", "```json\n{\"a\":1}\n```", []string{`{"a":1}`}},
		{"single object inside top-level array", `[{"a":1}]`, []string{`{"a":1}`}},
		{"multiple objects inside top-level array", `[{"a":1},{"a":2}]`, []string{`{"a":1}`, `{"a":2}`}},
		{"two objects", `{"a":1} and {"a":2}`, []string{`{"a":1}`, `{"a":2}`}},
		{"tag preamble drafting an object", `<think>{"a":0}</think>{"a":1}`, []string{`{"a":0}`, `{"a":1}`}},
		{"empty braces in preamble", `<think>not {}.</think>{"a":1}`, []string{`{}`, `{"a":1}`}},
		{"zero objects", `no json here`, nil},
		{"unbalanced brace", `broken {"a":1`, nil},
		{"truncated after preamble", `<think>x</think>{"a":`, nil},
		{"empty input", ``, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSONObjects([]byte(tc.input))
			if len(got) != len(tc.want) {
				t.Fatalf("extractJSONObjects(%q) = %q, want %q", tc.input, got, tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Fatalf("extractJSONObjects(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
				}
			}
		})
	}
}
