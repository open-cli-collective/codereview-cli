package cloudflareclef

import (
	"encoding/json"
	"testing"
)

func TestDecodeEnvelopeRequiresStrictCloudflareWrapper(t *testing.T) {
	validResult := clefResult("clef", `{"input_tokens":1,"output_tokens":2}`, clefAnswer("merge"))
	valid := []byte(`{"success":true,"result":` + validResult + `,"errors":[],"messages":[{"info":"ignored"}],"metadata":{"extension":true}}`)
	envelope, err := decodeEnvelope(valid)
	if err != nil || !envelope.success || len(envelope.errors) != 0 {
		t.Fatalf("valid envelope = %#v, %v (%s)", envelope, err, ErrorCode(err))
	}
	if _, ok := envelope.resultObject(); !ok {
		t.Fatal("valid result object was not retained")
	}

	failed, err := decodeEnvelope([]byte(`{"success":false,"result":null,"errors":[{"message":"private"}]}`))
	if err != nil || failed.success || len(failed.errors) != 1 {
		t.Fatalf("valid provider failure envelope = %#v, %v (%s)", failed, err, ErrorCode(err))
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "empty", body: nil},
		{name: "malformed JSON", body: []byte(`{"success":`)},
		{name: "invalid UTF8", body: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
		{name: "root scalar", body: []byte(`1`)},
		{name: "missing success", body: []byte(`{"result":{}}`)},
		{name: "non-boolean success", body: []byte(`{"success":"yes","result":{}}`)},
		{name: "success null result", body: []byte(`{"success":true,"result":null}`)},
		{name: "success array result", body: []byte(`{"success":true,"result":[]}`)},
		{name: "errors null", body: []byte(`{"success":false,"errors":null}`)},
		{name: "errors object", body: []byte(`{"success":false,"errors":{}}`)},
		{name: "duplicate root key", body: []byte(`{"success":true,"success":false,"result":{}}`)},
		{name: "duplicate nested usage before projection", body: []byte(`{"success":true,"result":{"usage":{"input_tokens":1,"input_tokens":2}},"errors":[]}`)},
		{name: "duplicate ignored message metadata", body: []byte(`{"success":true,"result":{},"messages":[{"message":"a","message":"b"}]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeEnvelope(test.body); err == nil || ErrorCode(err) != "malformed_response" {
				t.Fatalf("decodeEnvelope error = %v (%s), want malformed_response", err, ErrorCode(err))
			}
		})
	}
}

func TestIncompleteSignalChecksRootAndNestedResult(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields map[string]json.RawMessage
		code   string
		failed bool
	}{
		{name: "context overflow", fields: map[string]json.RawMessage{"context_overflow": json.RawMessage(`true`)}, code: "context_limit", failed: true},
		{name: "context status", fields: map[string]json.RawMessage{"status": json.RawMessage(`"context_length_exceeded"`)}, code: "context_limit", failed: true},
		{name: "truncated", fields: map[string]json.RawMessage{"truncated": json.RawMessage(`true`)}, code: "truncated", failed: true},
		{name: "incomplete details", fields: map[string]json.RawMessage{"incomplete_details": json.RawMessage(`{"reason":"length"}`)}, code: "truncated", failed: true},
		{name: "malformed signal", fields: map[string]json.RawMessage{"truncated": json.RawMessage(`null`)}, code: "malformed_response", failed: true},
		{name: "false signals ignored", fields: map[string]json.RawMessage{"truncated": json.RawMessage(`false`), "context_overflow": json.RawMessage(`false`)}, failed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, failed := incompleteSignal(test.fields)
			if code != test.code || failed != test.failed {
				t.Fatalf("incompleteSignal = %q,%v want %q,%v", code, failed, test.code, test.failed)
			}
		})
	}
}
