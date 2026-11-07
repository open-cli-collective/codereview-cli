package systemone

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestEncodeMatchesPrepareAndExistingWireHash(t *testing.T) {
	request := genericRequest()
	wantPlan, wantBody, err := prepare(request, testLimits)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gotPlan, gotBody, err := Encode(request, testLimits)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if gotPlan != wantPlan || !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("Encode result differs from existing wire plan: plan=%#v want=%#v body=%s", gotPlan, wantPlan, gotBody)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(gotBody)); got != wantPlan.RequestSHA256 {
		t.Fatalf("wire hash = %s, want %s", got, wantPlan.RequestSHA256)
	}
}

func TestDecodeSuccessAndPreservesMetadataOnBadAnswer(t *testing.T) {
	request := genericRequest()
	response, err := Decode(successfulResponse(request.Model), request, testLimits)
	if err != nil {
		t.Fatalf("Decode valid response: %v (%s)", err, ErrorCode(err))
	}
	if response.Model != request.Model || response.Usage == nil || *response.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) {
		t.Fatalf("validated metadata = %#v", response)
	}
	if len(response.Answers) != len(request.Questions) || response.Answers["alpha"].Choice != "merge" {
		t.Fatalf("answers = %#v", response.Answers)
	}

	badAnswer := strings.Replace(string(successfulResponse(request.Model)), `"choice":"merge"`, `"choice":"unknown-choice"`, 1)
	response, err = Decode([]byte(badAnswer), request, testLimits)
	if err == nil || ErrorCode(err) != "malformed_response" {
		t.Fatalf("bad answer error = %v (%s), want malformed_response", err, ErrorCode(err))
	}
	if response.Model != request.Model || response.Usage == nil || *response.Usage != (Usage{InputTokens: 150, OutputTokens: 10}) || response.Answers != nil {
		t.Fatalf("bad answer metadata was not retained safely: %#v", response)
	}
}

func TestDecodeRejectsWrongModelInvalidRequestOversizeAndDuplicates(t *testing.T) {
	request := genericRequest()
	wrongModel, err := Decode(successfulResponse("other/model"), request, testLimits)
	if err == nil || ErrorCode(err) != "malformed_response" || wrongModel.Model != "" || wrongModel.Usage != nil {
		t.Fatalf("wrong model result = %#v, %v (%s)", wrongModel, err, ErrorCode(err))
	}

	invalidRequest := request
	invalidRequest.Questions = nil
	if _, err := Decode(successfulResponse(request.Model), invalidRequest, testLimits); err == nil || ErrorCode(err) != "invalid_input" {
		t.Fatalf("invalid request error = %v (%s), want invalid_input", err, ErrorCode(err))
	}

	oversize := bytes.Repeat([]byte(" "), maxResponseBytes+1)
	if _, err := Decode(oversize, request, testLimits); err == nil || ErrorCode(err) != "response_too_large" {
		t.Fatalf("oversize response error = %v (%s), want response_too_large", err, ErrorCode(err))
	}

	duplicate := []byte(`{"model":"provider/model-one","model":"provider/model-one"}`)
	if _, err := Decode(duplicate, request, testLimits); err == nil || ErrorCode(err) != "malformed_response" {
		t.Fatalf("duplicate-key response error = %v (%s), want malformed_response", err, ErrorCode(err))
	}
}

func TestDecodeUsesExpectedModelPolicy(t *testing.T) {
	request := genericRequest()
	request.ExpectedModel = "provider/model-resolved"
	response, err := Decode(successfulResponse(request.ExpectedModel), request, testLimits)
	if err != nil || response.Model != request.ExpectedModel || response.Usage == nil || response.Answers == nil {
		t.Fatalf("Decode with expected model policy = %#v, %v (%s)", response, err, ErrorCode(err))
	}
}
