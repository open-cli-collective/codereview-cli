package llm

import "testing"

func TestExtractSingleJSONObject(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{"bare object", `{"a":1}`, `{"a":1}`, true},
		{"leading and trailing prose", `Sure! Here it is: {"a":1} Hope that helps.`, `{"a":1}`, true},
		{"nested objects and arrays", `prose {"a":{"b":1},"c":[{"d":2}]} prose`, `{"a":{"b":1},"c":[{"d":2}]}`, true},
		{"braces inside strings", `note {"msg":"use { and } freely"} end`, `{"msg":"use { and } freely"}`, true},
		{"escaped quotes inside strings", `{"msg":"she said \"hi\" {ok}"}`, `{"msg":"she said \"hi\" {ok}"}`, true},
		{"prose braces alongside one valid object", `Here is {the thing}: {"a":1}`, `{"a":1}`, true},
		{"markdown fenced object", "```json\n{\"a\":1}\n```", `{"a":1}`, true},
		{"zero objects", `no json here`, "", false},
		{"two objects", `{"a":1} and {"a":2}`, "", false},
		{"unbalanced brace", `broken {"a":1`, "", false},
		{"empty input", ``, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractSingleJSONObject([]byte(tc.input))
			if ok != tc.wantOK {
				t.Fatalf("extractSingleJSONObject(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if string(got) != tc.want {
				t.Fatalf("extractSingleJSONObject(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
