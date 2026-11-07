package mammoth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseResponseValidation(t *testing.T) {
	valid := string(successfulResponse("a"))
	validIdentity := `"identity":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.08,"c":0.02}}`
	modelField := `"model":"` + Model + `"`
	tests := []struct {
		name         string
		body         string
		wantCode     string
		wantReturned bool
		wantUsage    bool
		wantAnswer   bool
	}{
		{name: "empty", body: "", wantCode: "malformed_response"},
		{name: "invalid JSON", body: `{"model":`, wantCode: "malformed_response"},
		{name: "root is not object", body: `[]`, wantCode: "malformed_response"},
		{name: "duplicate model", body: `{"model":"` + Model + `","model":"` + Model + `"}`, wantCode: "malformed_response"},
		{name: "mismatched model", body: strings.Replace(valid, modelField, `"model":"other-model"`, 1), wantCode: "malformed_response"},
		{name: "missing model", body: strings.Replace(valid, modelField+",", "", 1), wantCode: "malformed_response"},
		{name: "missing answers", body: `{"model":"` + Model + `","usage":{"input_tokens":1,"output_tokens":1}}`, wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "missing identity", body: `{"model":"` + Model + `","answers":{},"usage":{"input_tokens":1,"output_tokens":1}}`, wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "additional answer", body: `{"model":"` + Model + `","answers":{"identity":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.08,"c":0.02}},"other":{}},"usage":{"input_tokens":1,"output_tokens":1}}`, wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "duplicate choice", body: strings.Replace(valid, `"choice":"a"`, `"choice":"a","choice":"b"`, 1), wantCode: "malformed_response"},
		{name: "wrong answer type", body: strings.Replace(valid, `"type":"choice"`, `"type":"freeform"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "unknown choice", body: strings.Replace(valid, `"choice":"a"`, `"choice":"insufficient_evidence"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "unknown nested answer field", body: strings.Replace(valid, validIdentity, `"identity":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.08,"c":0.02},"reason":"private"}`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "missing probability", body: strings.Replace(valid, `,"c":0.02`, "", 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "extra probability", body: strings.Replace(valid, `"c":0.02`, `"c":0.02,"d":0`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "duplicate probability", body: strings.Replace(valid, `"a":0.9`, `"a":0.9,"a":0.1`, 1), wantCode: "malformed_response"},
		{name: "probability out of range", body: strings.Replace(valid, `"b":0.08`, `"b":-0.08`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "probability sum outside tolerance", body: strings.Replace(valid, `"a":0.9`, `"a":0.85`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "probability sum at tolerance boundary", body: strings.Replace(valid, `"a":0.9,"b":0.08,"c":0.02`, `"a":0.98,"b":0,"c":0`, 1), wantReturned: true, wantUsage: true, wantAnswer: true},
		{name: "nonfinite probability", body: strings.Replace(valid, `"a":0.9`, `"a":1e999`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "confidence out of range", body: strings.Replace(valid, `"confidence":0.9`, `"confidence":1.01`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "missing usage field", body: strings.Replace(valid, `,"output_tokens":10`, "", 1), wantCode: "malformed_response", wantReturned: true},
		{name: "extra usage field", body: strings.Replace(valid, `"output_tokens":10`, `"output_tokens":10,"cache_tokens":1`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "negative usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":-1`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "fractional usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":150.5`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "null input usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":null`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "null output usage", body: strings.Replace(valid, `"output_tokens":10`, `"output_tokens":null`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "null confidence", body: strings.Replace(valid, `"confidence":0.9`, `"confidence":null`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null probabilities accepted as zero", body: strings.Replace(valid, `"probabilities":{"a":0.9,"b":0.08,"c":0.02}`, `"probabilities":{"a":1,"b":null,"c":null}`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "context overflow signal", body: strings.Replace(valid, `,"usage"`, `,"context_overflow":true,"usage"`, 1), wantCode: "context_limit", wantReturned: true, wantUsage: true},
		{name: "context length finish reason", body: strings.Replace(valid, `,"usage"`, `,"finish_reason":"context_length_exceeded","usage"`, 1), wantCode: "context_limit", wantReturned: true, wantUsage: true},
		{name: "truncated signal", body: strings.Replace(valid, `,"usage"`, `,"truncated":true,"usage"`, 1), wantCode: "truncated", wantReturned: true, wantUsage: true},
		{name: "partial status", body: strings.Replace(valid, `,"usage"`, `,"status":"incomplete","usage"`, 1), wantCode: "truncated", wantReturned: true, wantUsage: true},
		{name: "incomplete details", body: strings.Replace(valid, `,"usage"`, `,"incomplete_details":{"reason":"length"},"usage"`, 1), wantCode: "truncated", wantReturned: true, wantUsage: true},
		{name: "provider error extension", body: strings.Replace(valid, `,"answers"`, `,"error":{"detail":"private response text"},"answers"`, 1), wantCode: "upstream", wantReturned: true, wantUsage: true},
		{name: "null truncated indicator", body: strings.Replace(valid, `,"usage"`, `,"truncated":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null status indicator", body: strings.Replace(valid, `,"usage"`, `,"status":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null finish reason indicator", body: strings.Replace(valid, `,"usage"`, `,"finish_reason":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "harmless extensions", body: strings.Replace(valid, `,"usage"`, `,"status":"completed","finish_reason":"stop","provider":{"note":"safe extension"},"usage"`, 1), wantReturned: true, wantUsage: true, wantAnswer: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, returnedModel, usage, err := parseResponse([]byte(test.body))
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("parseResponse error = %v (%s)", err, ErrorCode(err))
				}
			} else if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("parseResponse error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if (returnedModel != "") != test.wantReturned {
				t.Fatalf("returned model present = %v, want %v", returnedModel != "", test.wantReturned)
			}
			if (usage != nil) != test.wantUsage {
				t.Fatalf("usage present = %v, want %v", usage != nil, test.wantUsage)
			}
			if (answer != nil) != test.wantAnswer {
				t.Fatalf("answer present = %v, want %v", answer != nil, test.wantAnswer)
			}
			if err != nil && strings.Contains(err.Error(), "private response text") {
				t.Fatalf("error leaked response text: %v", err)
			}
		})
	}
}

func TestCompareBoundedAndTruncatedResponses(t *testing.T) {
	left := findingRecord("f-left", "src/a.go", "first report")
	right := findingRecord("f-right", "src/b.go", "second report")
	tests := []struct {
		name       string
		makeBody   func(*http.Request) *http.Response
		wantCode   string
		wantHash   bool
		wantClosed bool
	}{
		{
			name: "declared response too large",
			makeBody: func(request *http.Request) *http.Response {
				body := &closeTrackingBody{reader: bytes.NewReader(bytesOf('x', maxResponseBytes+1))}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": []string{fmtInt(maxResponseBytes + 1)}}, Body: body, ContentLength: maxResponseBytes + 1, Request: request}
			},
			wantCode: "response_too_large", wantClosed: true,
		},
		{
			name: "undeclared response too large",
			makeBody: func(request *http.Request) *http.Response {
				body := &closeTrackingBody{reader: bytes.NewReader(bytesOf('x', maxResponseBytes+1))}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: -1, Request: request}
			},
			wantCode: "response_too_large", wantClosed: true,
		},
		{
			name: "declared response length mismatch",
			makeBody: func(request *http.Request) *http.Response {
				body := []byte(successfulResponse("a"))
				tracked := &closeTrackingBody{reader: bytes.NewReader(body)}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": []string{fmtInt(len(body) + 1)}}, Body: tracked, ContentLength: int64(len(body) + 1), Request: request}
			},
			wantCode: "truncated", wantClosed: true,
		},
		{
			name: "unexpected EOF",
			makeBody: func(request *http.Request) *http.Response {
				body := &unexpectedEOFBody{data: []byte(`{"model":"partial`)}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: -1, Request: request}
			},
			wantCode: "truncated", wantClosed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wasClosed func() bool
			client := testClient(t, time.Second, func(request *http.Request) (*http.Response, error) {
				response := test.makeBody(request)
				switch body := response.Body.(type) {
				case *closeTrackingBody:
					wasClosed = func() bool { return body.closed }
				case *unexpectedEOFBody:
					wasClosed = func() bool { return body.closed }
				}
				return response, nil
			})
			receipt, err := client.Compare(context.Background(), TaskCohort, left, right)
			if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("Compare error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if (receipt.ResponseSHA256 != "") != test.wantHash {
				t.Fatalf("response hash = %q, want present=%v", receipt.ResponseSHA256, test.wantHash)
			}
			if test.wantClosed && (wasClosed == nil || !wasClosed()) {
				t.Fatal("response body was not closed")
			}
		})
	}
}

type unexpectedEOFBody struct {
	data   []byte
	read   bool
	closed bool
}

func (body *unexpectedEOFBody) Read(destination []byte) (int, error) {
	if body.read {
		return 0, io.ErrUnexpectedEOF
	}
	body.read = true
	return copy(destination, body.data), nil
}

func (body *unexpectedEOFBody) Close() error {
	body.closed = true
	return nil
}

func fmtInt(value int) string {
	return strconv.Itoa(value)
}
