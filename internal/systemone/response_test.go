package systemone

import (
	"strings"
	"testing"
)

func TestParseResponseStrictlyMatchesRequestQuestionsAndLabels(t *testing.T) {
	request := genericRequest()
	valid := string(successfulResponse(request.Model))
	tests := []struct {
		name         string
		body         string
		wantCode     string
		wantReturned bool
		wantUsage    bool
		wantAnswers  bool
	}{
		{name: "empty", body: "", wantCode: "malformed_response"},
		{name: "invalid JSON", body: `{"model":`, wantCode: "malformed_response"},
		{name: "root scalar", body: `1`, wantCode: "malformed_response"},
		{name: "duplicate model", body: `{"model":"provider/model-one","model":"provider/model-one"}`, wantCode: "malformed_response"},
		{name: "mismatched model", body: strings.Replace(valid, `"model":"provider/model-one"`, `"model":"other-model"`, 1), wantCode: "malformed_response"},
		{name: "missing model", body: strings.Replace(valid, `"model":"provider/model-one",`, "", 1), wantCode: "malformed_response"},
		{name: "missing usage", body: strings.Replace(valid, `,"usage":{"input_tokens":150,"output_tokens":10}`, "", 1), wantCode: "malformed_response", wantReturned: true},
		{name: "null input usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":null`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "null output usage", body: strings.Replace(valid, `"output_tokens":10`, `"output_tokens":null`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "negative usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":-1`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "fractional usage", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":150.5`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "extra usage", body: strings.Replace(valid, `"output_tokens":10`, `"output_tokens":10,"cache_tokens":1`, 1), wantCode: "malformed_response", wantReturned: true},
		{name: "duplicate usage key", body: strings.Replace(valid, `"input_tokens":150`, `"input_tokens":150,"input_tokens":151`, 1), wantCode: "malformed_response"},
		{name: "missing answers", body: `{"model":"provider/model-one","usage":{"input_tokens":1,"output_tokens":1}}`, wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "missing question", body: strings.Replace(valid, `,"beta":{"type":"choice","choice":"enabled","confidence":0.8,"probabilities":{"enabled":0.75,"disabled":0.25}}`, "", 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "extra question", body: strings.Replace(valid, `"disabled":0.25}}},"usage"`, `"disabled":0.25}},"extra":{"type":"choice","choice":"x","confidence":1,"probabilities":{"x":0.5,"y":0.5}}},"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "duplicate question answer key", body: strings.Replace(valid, `,"beta":{"type":"choice"`, `,"alpha":{"type":"choice","choice":"merge","confidence":0.9,"probabilities":{"merge":0.9,"separate":0.08,"unknown":0.02}},"beta":{"type":"choice"`, 1), wantCode: "malformed_response"},
		{name: "wrong answer type", body: strings.Replace(valid, `"type":"choice"`, `"type":"freeform"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "unknown choice label", body: strings.Replace(valid, `"choice":"merge"`, `"choice":"a"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "missing probability label", body: strings.Replace(valid, `,"unknown":0.02`, "", 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "extra probability label", body: strings.Replace(valid, `"unknown":0.02`, `"unknown":0.02,"extra":0`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "duplicate probability label", body: strings.Replace(valid, `"merge":0.9`, `"merge":0.9,"merge":0.1`, 1), wantCode: "malformed_response"},
		{name: "probability outside range", body: strings.Replace(valid, `"separate":0.08`, `"separate":-0.08`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "probability sum outside tolerance", body: strings.Replace(valid, `"merge":0.9`, `"merge":0.85`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "probability boundary accepted", body: strings.Replace(valid, `"merge":0.9,"separate":0.08,"unknown":0.02`, `"merge":0.98,"separate":0,"unknown":0`, 1), wantReturned: true, wantUsage: true, wantAnswers: true},
		{name: "nonfinite probability", body: strings.Replace(valid, `"merge":0.9`, `"merge":1e999`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null confidence", body: strings.Replace(valid, `"confidence":0.9`, `"confidence":null`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null probabilities cannot become zero", body: strings.Replace(valid, `"probabilities":{"merge":0.9,"separate":0.08,"unknown":0.02}`, `"probabilities":{"merge":1,"separate":null,"unknown":null}`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "duplicate choice field", body: strings.Replace(valid, `"choice":"merge"`, `"choice":"merge","choice":"separate"`, 1), wantCode: "malformed_response"},
		{name: "unknown answer field", body: strings.Replace(valid, `"confidence":0.9`, `"confidence":0.9,"reason":"private"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "truncated signal", body: strings.Replace(valid, `,"usage"`, `,"truncated":true,"usage"`, 1), wantCode: "truncated", wantReturned: true, wantUsage: true},
		{name: "context overflow signal", body: strings.Replace(valid, `,"usage"`, `,"context_overflow":true,"usage"`, 1), wantCode: "context_limit", wantReturned: true, wantUsage: true},
		{name: "null truncated indicator", body: strings.Replace(valid, `,"usage"`, `,"truncated":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null status indicator", body: strings.Replace(valid, `,"usage"`, `,"status":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "null finish reason indicator", body: strings.Replace(valid, `,"usage"`, `,"finish_reason":null,"usage"`, 1), wantCode: "malformed_response", wantReturned: true, wantUsage: true},
		{name: "provider error", body: strings.Replace(valid, `,"answers"`, `,"error":{"detail":"private provider text"},"answers"`, 1), wantCode: "upstream", wantReturned: true, wantUsage: true},
		{name: "harmless extensions", body: strings.Replace(valid, `,"usage"`, `,"status":"completed","finish_reason":"stop","provider":{"note":"safe"},"usage"`, 1), wantReturned: true, wantUsage: true, wantAnswers: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answers, model, usage, err := parseResponse([]byte(test.body), request)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("parseResponse error = %v (%s)", err, ErrorCode(err))
				}
			} else if err == nil || ErrorCode(err) != test.wantCode {
				t.Fatalf("parseResponse error = %v (%s), want %s", err, ErrorCode(err), test.wantCode)
			}
			if (model != "") != test.wantReturned || (usage != nil) != test.wantUsage || (answers != nil) != test.wantAnswers {
				t.Fatalf("metadata/results: model=%q usage=%#v answers=%#v", model, usage, answers)
			}
		})
	}
}

func TestParseResponsePreservesSafeMetadataBeforeExplicitFailures(t *testing.T) {
	request := genericRequest()
	valid := string(successfulResponse(request.Model))
	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{name: "truncated with usage", body: strings.Replace(valid, `,"usage"`, `,"truncated":true,"usage"`, 1), code: "truncated"},
		{name: "context overflow with usage", body: strings.Replace(valid, `,"usage"`, `,"context_overflow":true,"usage"`, 1), code: "context_limit"},
		{name: "provider error with usage", body: strings.Replace(valid, `,"answers"`, `,"error":{},"answers"`, 1), code: "upstream"},
		{name: "truncated without metadata", body: `{"truncated":true}`, code: "truncated"},
		{name: "context overflow without metadata", body: `{"context_overflow":true}`, code: "context_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			answers, model, usage, err := parseResponse([]byte(test.body), request)
			if err == nil || ErrorCode(err) != test.code || answers != nil {
				t.Fatalf("parseResponse = answers:%#v model:%q usage:%#v err:%v (%s)", answers, model, usage, err, ErrorCode(err))
			}
			if strings.Contains(test.name, "with usage") && (model != request.Model || usage == nil || *usage != (Usage{InputTokens: 150, OutputTokens: 10})) {
				t.Fatalf("validated metadata was lost: model=%q usage=%#v", model, usage)
			}
			if strings.Contains(test.name, "without metadata") && (model != "" || usage != nil) {
				t.Fatalf("untrusted metadata was retained: model=%q usage=%#v", model, usage)
			}
		})
	}
}

func TestParseResponseExpectedModelOverrideIsLocal(t *testing.T) {
	request := genericRequest()
	request.Model = "provider/model-alias"
	response := successfulResponse("provider/model-resolved")
	if _, model, usage, err := parseResponse(response, request); err == nil || ErrorCode(err) != "malformed_response" || model != "" || usage != nil {
		t.Fatalf("unexpected resolved ID accepted without policy: model=%q usage=%#v err=%v", model, usage, err)
	}
	request.ExpectedModel = "provider/model-resolved"
	answers, model, usage, err := parseResponse(response, request)
	if err != nil || model != request.ExpectedModel || usage == nil || len(answers) != len(request.Questions) {
		t.Fatalf("explicit resolved ID result: model=%q usage=%#v answers=%#v err=%v", model, usage, answers, err)
	}
}
