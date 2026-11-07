package reportidentityinference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const jevEndpoint = "https://api.typesafe.ai/v1/systemone"

func TestJevSystemOneWireCompatibility(t *testing.T) {
	left := Record{Kind: "finding", Finding: &Finding{
		ID: "f-left", Severity: "major", FilePath: "pkg/import/apply.go",
		Anchor: Anchor{Kind: "line", Side: "RIGHT", Line: 42}, Body: "Publish only after commit succeeds.", Anchoring: "inline",
	}}
	cohortRight := Record{Kind: "finding", Finding: &Finding{
		ID: "f-right", Severity: "major", FilePath: "pkg/events/publish.go",
		Anchor: Anchor{Kind: "file"}, Body: "Move publication after a successful commit.",
	}}
	priorRight := Record{Kind: "thread", Thread: &Thread{
		ID: "t-prior", Eligible: true, FilePath: "pkg/events/publish.go", Body: "The earlier report describes publication before commit.",
	}}
	tasks := []struct {
		name  string
		task  Task
		left  Record
		right Record
	}{
		{name: "cohort", task: TaskCohort, left: left, right: cohortRight},
		{name: "prior threads", task: TaskPriorThreads, left: left, right: priorRight},
	}
	choices := []struct {
		wire string
		want Choice
	}{
		{wire: "a", want: ChoiceSame},
		{wire: "b", want: ChoiceDifferent},
		{wire: "c", want: ChoiceAbstain},
	}

	for _, task := range tasks {
		t.Run(task.name, func(t *testing.T) {
			for _, choice := range choices {
				t.Run(choice.wire, func(t *testing.T) {
					request, err := Request("jev-1.14.0", "", task.task, task.left, task.right)
					if err != nil {
						t.Fatalf("Request: %v", err)
					}
					if request.ServiceTier != "" || request.ExpectedModel != "" {
						t.Fatalf("request has unexpected optional policy: %#v", request)
					}

					plan, receipt, body, err := decideWithJevTransport(t, request, "jev-1.14.0", choice.wire)
					if err != nil {
						t.Fatalf("Decide: %v", err)
					}
					wantState := expectedJevState(t, task.task, task.left, task.right)
					assertJevRequestBody(t, body, request, wantState)

					digest := sha256.Sum256(body)
					if sentHash := hex.EncodeToString(digest[:]); sentHash != plan.RequestSHA256 || receipt.RequestSHA256 != plan.RequestSHA256 {
						t.Fatalf("sent request hash = %s, plan hash = %s, receipt hash = %s", sentHash, plan.RequestSHA256, receipt.RequestSHA256)
					}
					if receipt.RequestedModel != "jev-1.14.0" || receipt.ReturnedModel != "jev-1.14.0" || receipt.HTTPStatus != http.StatusOK {
						t.Fatalf("receipt metadata = %#v", receipt)
					}
					if receipt.Usage == nil || receipt.Usage.InputTokens != 120 || receipt.Usage.OutputTokens != 12 {
						t.Fatalf("receipt usage = %#v, want 120 input and 12 output tokens", receipt.Usage)
					}

					answer, err := ParseAnswer(receipt.Answers)
					if err != nil {
						t.Fatalf("ParseAnswer: %v", err)
					}
					if answer.Choice != choice.want || answer.Probabilities != jevProbabilityArray(choice.wire) || answer.Confidence != 0.9 {
						t.Fatalf("ParseAnswer = %#v, want %s", answer, choice.want)
					}
				})
			}
		})
	}
}

func TestJevSystemOneAcceptsExplicitResolvedModelForAlias(t *testing.T) {
	request, err := Request("jev-latest", "", TaskCohort,
		findingRecord("f-left", "pkg/import/apply.go", "Publish only after commit succeeds."),
		findingRecord("f-right", "pkg/events/publish.go", "Move publication after commit succeeds."),
	)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	request.ExpectedModel = "jev-1.14.0"

	plan, receipt, body, err := decideWithJevTransport(t, request, "jev-1.14.0", "a")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if receipt.RequestedModel != "jev-latest" || receipt.ReturnedModel != "jev-1.14.0" {
		t.Fatalf("alias receipt models = requested %q, returned %q", receipt.RequestedModel, receipt.ReturnedModel)
	}
	answer, err := ParseAnswer(receipt.Answers)
	if err != nil || answer.Choice != ChoiceSame {
		t.Fatalf("alias response answer = %#v, error = %v, want same", answer, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode sent request: %v", err)
	}
	var sentModel string
	if err := json.Unmarshal(wire["model"], &sentModel); err != nil || sentModel != "jev-latest" {
		t.Fatalf("sent model = %q, error = %v, want alias jev-latest", sentModel, err)
	}
	if _, exists := wire["expected_model"]; exists {
		t.Fatal("local expected-model policy was sent to TypeSafe")
	}
	digest := sha256.Sum256(body)
	if sentHash := hex.EncodeToString(digest[:]); sentHash != plan.RequestSHA256 || receipt.RequestSHA256 != plan.RequestSHA256 {
		t.Fatalf("sent request hash = %s, plan hash = %s, receipt hash = %s", sentHash, plan.RequestSHA256, receipt.RequestSHA256)
	}
}

func TestJevSystemOneRejectsWrongReturnedModelWithoutAnswers(t *testing.T) {
	request, err := Request("jev-1.14.0", "", TaskCohort,
		findingRecord("f-left", "pkg/import/apply.go", "Publish only after commit succeeds."),
		findingRecord("f-right", "pkg/events/publish.go", "Move publication after commit succeeds."),
	)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	_, receipt, _, err := decideWithJevTransport(t, request, "jev-1.13.0", "a")
	if code := systemone.ErrorCode(err); code != "malformed_response" {
		t.Fatalf("wrong-model error code = %q (error %v), want malformed_response", code, err)
	}
	if len(receipt.Answers) != 0 {
		t.Fatalf("wrong-model response retained answers: %#v", receipt.Answers)
	}
}

func decideWithJevTransport(t *testing.T, request systemone.Request, responseModel, choice string) (systemone.Plan, systemone.Receipt, []byte, error) {
	t.Helper()
	limits := systemone.Limits{MaxStateBytes: 6144, MaxRequestBytes: 8192}
	plan, err := systemone.Prepare(request, limits)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var body []byte
	client, err := systemone.New(systemone.Options{
		Endpoint: jevEndpoint, APIKey: "synthetic-key", AuthScheme: "Bearer", Limits: limits,
		HTTPClient: &http.Client{Transport: identityRoundTrip(func(httpRequest *http.Request) (*http.Response, error) {
			if httpRequest.Method != http.MethodPost || httpRequest.URL.String() != jevEndpoint {
				t.Fatalf("request target = %s %s", httpRequest.Method, httpRequest.URL)
			}
			if httpRequest.Header.Get("Authorization") != "Bearer synthetic-key" {
				t.Fatalf("Authorization = %q, want Bearer synthetic-key", httpRequest.Header.Get("Authorization"))
			}
			if httpRequest.Header.Get("Content-Type") != "application/json" || httpRequest.Header.Get("Accept") != "application/json" {
				t.Fatalf("request content headers = %#v", httpRequest.Header)
			}
			var readErr error
			body, readErr = io.ReadAll(httpRequest.Body)
			if readErr != nil {
				return nil, readErr
			}
			responseBody := jevSyntheticResponse(t, responseModel, choice)
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(responseBody)), ContentLength: int64(len(responseBody)), Request: httpRequest,
			}, nil
		})},
	})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	receipt, decideErr := client.Decide(context.Background(), request)
	return plan, receipt, body, decideErr
}

func jevSyntheticResponse(t *testing.T, model, choice string) []byte {
	t.Helper()
	probabilities := jevProbabilities(choice)
	response := struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}{Model: model}
	response.Answers = map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Confidence    float64            `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	}{"identity": {Type: "choice", Choice: choice, Confidence: 0.9, Probabilities: probabilities}}
	response.Usage.InputTokens = 120
	response.Usage.OutputTokens = 12
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode synthetic TypeSafe-shaped response: %v", err)
	}
	return encoded
}

func jevProbabilities(choice string) map[string]float64 {
	switch choice {
	case "a":
		return map[string]float64{"a": 0.9, "b": 0.08, "c": 0.02}
	case "b":
		return map[string]float64{"a": 0.08, "b": 0.9, "c": 0.02}
	default:
		return map[string]float64{"a": 0.08, "b": 0.02, "c": 0.9}
	}
}

func jevProbabilityArray(choice string) [3]float64 {
	switch choice {
	case "a":
		return [3]float64{0.9, 0.08, 0.02}
	case "b":
		return [3]float64{0.08, 0.9, 0.02}
	default:
		return [3]float64{0.08, 0.02, 0.9}
	}
}

func expectedJevState(t *testing.T, task Task, left, right Record) []byte {
	t.Helper()
	state := stateWire{
		ProtocolVersion: 1,
		Task:            task,
		Left:            jevExpectedRecordWire(left),
		Right:           jevExpectedRecordWire(right),
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode expected state: %v", err)
	}
	return encoded
}

func jevExpectedRecordWire(record Record) recordWire {
	wire := recordWire{Kind: record.Kind}
	if record.Finding != nil {
		finding := record.Finding
		wire.Finding = &findingWire{
			ID: finding.ID, Severity: finding.Severity, FilePath: finding.FilePath,
			Anchor: anchorWire{Kind: finding.Anchor.Kind, Side: finding.Anchor.Side, Line: finding.Anchor.Line},
			Body:   finding.Body, Anchoring: finding.Anchoring,
		}
	}
	if record.Thread != nil {
		thread := record.Thread
		wire.Thread = &threadWire{ID: thread.ID, Eligible: thread.Eligible, FilePath: thread.FilePath, Body: thread.Body}
	}
	return wire
}

func assertJevRequestBody(t *testing.T, body []byte, request systemone.Request, wantState []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if _, exists := fields["service_tier"]; exists {
		t.Fatal("empty optional service_tier was sent")
	}
	var wire struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode TypeSafe request: %v", err)
	}
	if wire.Model != request.Model || !bytes.Equal(wire.State, wantState) {
		t.Fatalf("sent model/state = %q/%s, want %q/%s", wire.Model, wire.State, request.Model, wantState)
	}
	if len(wire.Questions) != 1 {
		t.Fatalf("sent questions = %#v, want only identity", wire.Questions)
	}
	question, exists := wire.Questions["identity"]
	if !exists || question.Type != "choice" || question.Instructions != identityInstructions || !reflect.DeepEqual(question.Criteria, map[string]string{
		"a": criterionSame, "b": criterionDifferent, "c": criterionAbstain,
	}) {
		t.Fatalf("sent identity question = %#v", question)
	}
}
