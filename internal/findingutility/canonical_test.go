package findingutility

import (
	"math"
	"testing"
)

func TestCanonicalJSONOrdersObjectsAndPreservesArrays(t *testing.T) {
	left, err := CanonicalizeJSON([]byte(` { "b": 2, "a": [3, 1] } `))
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalizeJSON([]byte(`{"a":[3,1],"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != `{"a":[3,1],"b":2}` || string(left) != string(right) {
		t.Fatalf("canonical bytes = %s and %s", left, right)
	}
	changed, err := CanonicalizeJSON([]byte(`{"a":[1,3],"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) == string(left) {
		t.Fatal("array order must affect canonical identity")
	}
}

func TestCanonicalJSONRejectsAmbiguousOrNonfiniteInput(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"a":1,"a":2}`),
		[]byte(`{"a":1} {"b":2}`),
		{0xff, 0xfe},
	} {
		if _, err := CanonicalizeJSON(input); err == nil {
			t.Fatalf("CanonicalizeJSON(%q) unexpectedly succeeded", input)
		}
	}
	if _, err := CanonicalJSON(math.NaN()); err == nil {
		t.Fatal("NaN must not be canonical JSON")
	}
	if err := ValidateCanonicalStruct(struct{ Value float64 }{Value: math.Inf(1)}); err == nil {
		t.Fatal("non-finite struct values must be rejected")
	}
}

func TestCanonicalJSONNormalizesNumericSpellings(t *testing.T) {
	for _, input := range []string{`1.0`, `1e0`, `1000.0`, `1e3`, `-0`} {
		canonical, err := CanonicalizeJSON([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if string(canonical) != "1" && input != "1000.0" && input != "1e3" && input != "-0" {
			t.Fatalf("unexpected canonical number for %s: %s", input, canonical)
		}
	}
	for _, pair := range [][2]string{{`1.0`, `1e0`}, {`1000.0`, `1e3`}, {`-0`, `0`}} {
		left, err := CanonicalizeJSON([]byte(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		right, err := CanonicalizeJSON([]byte(pair[1]))
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Fatalf("%s and %s differ: %s vs %s", pair[0], pair[1], left, right)
		}
	}
}

func TestCanonicalJSONDoesNotHTMLEscapeStrings(t *testing.T) {
	canonical, err := CanonicalJSON(map[string]string{"text": "<tag>&"})
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"text":"<tag>&"}` {
		t.Fatalf("canonical string = %s", canonical)
	}
}

func TestCanonicalJSONUsesFrozenTypedFloatFormatting(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{value: 0, want: "0"},
		{value: math.Copysign(0, -1), want: "0"},
		{value: 1.0, want: "1"},
		{value: 1e-9, want: "1e-09"},
		{value: 1e20, want: "1e+20"},
		{value: 1e21, want: "1e+21"},
		{value: math.SmallestNonzeroFloat64, want: "5e-324"},
	}
	for _, test := range cases {
		got, err := CanonicalJSON(test.value)
		if err != nil {
			t.Fatalf("CanonicalJSON(%g): %v", test.value, err)
		}
		if string(got) != test.want {
			t.Fatalf("CanonicalJSON(%g) = %s, want %s", test.value, got, test.want)
		}
	}
	got, err := CanonicalJSON(struct {
		Value float64 `json:"value"`
	}{Value: 1e-9})
	if err != nil || string(got) != `{"value":1e-09}` {
		t.Fatalf("typed struct float = %s, err=%v", got, err)
	}
}

func TestCanonicalJSONRejectsInvalidTypedUTF8(t *testing.T) {
	bad := string([]byte{0xff, 0xfe})
	if _, err := CanonicalJSON(struct {
		Text string `json:"text"`
	}{Text: bad}); err == nil {
		t.Fatal("invalid UTF-8 struct string must fail")
	}
	if _, err := CanonicalJSON(map[string]string{bad: "value"}); err == nil {
		t.Fatal("invalid UTF-8 map key must fail")
	}
}

func TestDecodeStrictRejectsUnknownFields(t *testing.T) {
	var profile DevelopmentProfile
	if err := DecodeStrict([]byte(`{"schema_version":1,"unknown":true}`), &profile); err == nil {
		t.Fatal("unknown fields must fail strict decoding")
	}
}
