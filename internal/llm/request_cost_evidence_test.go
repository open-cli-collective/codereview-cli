package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type costFakeAdapter struct {
	*FakeAdapter
	waits []error
}

func (a *costFakeAdapter) Queue(r FakeResult) {
	a.waits = append(a.waits, r.WaitErr)
	r.WaitErr = nil
	a.FakeAdapter.Queue(r)
}
func (a *costFakeAdapter) Start(ctx context.Context, req Request) (Stream, error) {
	stream, err := a.FakeAdapter.Start(ctx, req)
	var waitErr error
	if len(a.waits) > 0 {
		waitErr = a.waits[0]
		a.waits = a.waits[1:]
	}
	if err != nil {
		return nil, err
	}
	return costEvidenceStream{Stream: stream, waitErr: waitErr}, nil
}

type costEvidenceStream struct {
	Stream
	waitErr error
}

func (s costEvidenceStream) Wait(ctx context.Context) (Response, error) {
	response, err := s.Stream.Wait(ctx)
	if err != nil {
		return response, err
	}
	return response, s.waitErr
}
func (*costFakeAdapter) RequestCostEvidenceSource() string { return "openai_responses" }
func costPtr[T any](v T) *T                                { return &v }
func costResponse(id, text string) Response {
	return Response{StructuredOutput: []byte(text), RequestCostEvidence: &RequestCostEvidence{
		Version: 1, Source: "openai_responses", Scope: "invocation", AttemptsObserved: 1, AttemptCountExact: true,
		Attempts: []RequestCostAttempt{{RuntimeID: "openai-api-key", EndpointKind: "official_global", TransportMode: "single_send_v1", Dispatch: "possibly_dispatched", HTTPStatus: costPtr(200), BodyState: "complete", ProviderResponseID: costPtr(id), ObservedModel: costPtr("observed"), ObservedServiceTier: costPtr("default"), ResponseStatus: costPtr("completed"), RequestedModel: "requested", Usage: &OpenAIRequestUsage{InputTokens: costPtr(int64(10)), OutputTokens: costPtr(int64(3)), CacheReadTokens: costPtr(int64(2)), CacheWriteTokens: costPtr(int64(0)), TotalTokens: costPtr(int64(13))}}},
	}}
}
func costDecode(b []byte) (bool, error) {
	if string(b) == `{"ok":true}` {
		return true, nil
	}
	return false, errors.New("invalid synthetic output")
}

func TestRequestCostCollectorValidationAndTerminalPaths(t *testing.T) {
	for _, tc := range []struct {
		name         string
		second       FakeResult
		callback     bool
		wantAttempts int
		wantErr      bool
	}{
		{"correction success", FakeResult{Response: costResponse("B", `{"ok":true}`)}, false, 2, false},
		{"two invalid", FakeResult{Response: costResponse("B", `invalid`)}, false, 2, true},
		{"correction start", FakeResult{StartErr: errors.New("start")}, false, 2, true},
		{"correction wait", FakeResult{Response: costResponse("B", ``), WaitErr: errors.New("wait")}, false, 2, true},
		{"correction callback", FakeResult{}, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &costFakeAdapter{FakeAdapter: &FakeAdapter{}}
			first := costResponse("A", `invalid`)
			a.Queue(FakeResult{Response: first})
			a.Queue(tc.second)
			req := Request{Model: "requested", FreshValidationRetrySession: true}
			if tc.callback {
				req.OnValidationRetry = func(*Request) error { return errors.New("callback") }
			}
			got, err := RunStructuredWithSessionResume(context.Background(), a, "", req, costDecode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			e := got.Response.RequestCostEvidence
			if e == nil || len(e.Attempts) != tc.wantAttempts || e.AttemptsObserved != uint64(tc.wantAttempts) {
				t.Fatalf("history = %#v", e)
			}
			if !tc.wantErr && string(got.AcceptedOutput) != `{"ok":true}` {
				t.Fatal("accepted output replaced by initial attempt")
			}
			if *e.Attempts[0].ProviderResponseID != "A" || e.Attempts[0].Ordinal != 1 || e.Attempts[0].ValidationPhase != "initial" {
				t.Fatal("initial receipt lost")
			}
			if tc.wantAttempts == 2 && (e.Attempts[1].Ordinal != 2 || e.Attempts[1].ValidationPhase != "correction" || e.Attempts[1].AdapterAttempt != 1) {
				t.Fatal("phase ordering lost")
			}
			if tc.name == "correction start" && e.Attempts[1].Dispatch != "not_dispatched" {
				t.Fatal("Start failure is not explicit non-dispatch")
			}
			*first.RequestCostEvidence.Attempts[0].Usage.InputTokens = 999
			*got.ValidationAttempts[0].Response.RequestCostEvidence.Attempts[0].Usage.InputTokens = 777
			if *e.Attempts[0].Usage.InputTokens != 10 {
				t.Fatal("history aliases adapter or validation snapshot")
			}
		})
	}
}

func TestRequestCostCollectorTransientPhasesAndExhaustion(t *testing.T) {
	old := activeRetryPolicy
	activeRetryPolicy = retryPolicy{MaxRetries: 2}
	t.Cleanup(func() { activeRetryPolicy = old })
	for _, success := range []bool{false, true} {
		a := &costFakeAdapter{FakeAdapter: &FakeAdapter{}}
		for i := 0; i < 3; i++ {
			waitErr := error(ErrTransient)
			if success && i == 2 {
				waitErr = nil
			}
			a.Queue(FakeResult{Response: costResponse(fmt.Sprint(i), `{"ok":true}`), WaitErr: waitErr})
		}
		got, err := RunStructuredWithSessionResume(context.Background(), a, "", Request{Model: "requested"}, costDecode)
		if (err == nil) != success || len(got.Response.RequestCostEvidence.Attempts) != 3 {
			t.Fatalf("err=%v history=%#v", err, got.Response.RequestCostEvidence)
		}
		for i, a := range got.Response.RequestCostEvidence.Attempts {
			if a.Ordinal != uint32(i+1) || a.AdapterAttempt != uint32(i+1) {
				t.Fatal("attempt budget inferred or reset")
			}
		}
	}
}

func TestRequestCostCollectorMissingUnsupportedAndCancellation(t *testing.T) {
	a := &costFakeAdapter{FakeAdapter: &FakeAdapter{}}
	a.Queue(FakeResult{Response: Response{StructuredOutput: []byte(`{"ok":true}`)}})
	got, err := RunStructuredWithSessionResume(context.Background(), a, "", Request{Model: "requested"}, costDecode)
	if err != nil || !slices.Contains(got.Response.RequestCostEvidence.Gaps, CostGapMissing) {
		t.Fatal("missing expected receipt was omitted")
	}
	plain := &FakeAdapter{}
	plain.Queue(FakeResult{Response: costResponse("ignored", `{"ok":true}`)})
	unsupported, _ := RunStructuredWithSessionResume(context.Background(), plain, "", Request{}, costDecode)
	if unsupported.Response.RequestCostEvidence != nil {
		t.Fatal("unsupported producer gained collection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled, _ := RunStructuredWithSessionResume(ctx, &costFakeAdapter{FakeAdapter: &FakeAdapter{}}, "", Request{Model: "requested"}, costDecode)
	if len(canceled.Response.RequestCostEvidence.Attempts) != 1 || canceled.Response.RequestCostEvidence.Attempts[0].Dispatch != "not_dispatched" {
		t.Fatal("canceled Start omitted")
	}
}

func TestRequestCostBoundsCounterSaturationAndDetachedToolTrace(t *testing.T) {
	collector := newRequestCostCollector(&costFakeAdapter{FakeAdapter: &FakeAdapter{}})
	for i := 0; i < 65; i++ {
		collector.observe(costResponse(fmt.Sprint(i), ""), "initial", uint32(i+1))
	}
	e := collector.snapshot()
	if len(e.Attempts) != 64 || e.AttemptsObserved != 65 || e.DroppedAttempts != 1 || !e.Truncated || e.AttemptCountExact {
		t.Fatalf("bounds = %#v", e)
	}
	if *e.Attempts[0].ProviderResponseID != "0" || *e.Attempts[63].ProviderResponseID != "63" {
		t.Fatal("retained last success instead of prefix")
	}
	if SaturatingCostAdd(math.MaxUint64, 1) != math.MaxUint64 {
		t.Fatal("counter wrapped")
	}
	original := costResponse("clone", "")
	original.ReviewerToolEvidence = &ReviewerToolEvidence{DiffStatus: DiffToolStatusSucceeded}
	copy := cloneResponse(original)
	copy.RequestCostEvidence.Attempts[0].Issues = append(copy.RequestCostEvidence.Attempts[0].Issues, CostGapInvalid)
	*copy.RequestCostEvidence.Attempts[0].Usage.CacheWriteTokens = 9
	copy.ReviewerToolEvidence.DiffStatus = DiffToolStatusFailed
	if *original.RequestCostEvidence.Attempts[0].Usage.CacheWriteTokens != 0 || original.ReviewerToolEvidence.DiffStatus != DiffToolStatusSucceeded {
		t.Fatal("response ownership regression")
	}
	data, _ := RequestCostEvidenceBytes(e)
	if len(data) > MaxRequestCostEvidenceBytes-RequestCostReservedBytes {
		t.Fatal("evidence exceeded budget")
	}
}

func TestRequestCostDecoderMalformedOptionalAndIdentityBounds(t *testing.T) {
	for _, raw := range []string{`[]`, `{"version":2}`, `{"version":1,"version":1}`, `{"version":"bad"}`, strings.Repeat(" ", MaxRequestCostEvidenceBytes) + `{}`} {
		e := DecodeRequestCostEvidence([]byte(raw))
		if e == nil || e.AttemptCountExact || len(e.Gaps) == 0 {
			t.Fatalf("malformed envelope became complete: %s", raw[:min(len(raw), 40)])
		}
	}
	e := costResponse("safe", "").RequestCostEvidence
	e.Attempts[0].ObservedModel = costPtr("malicious\nmodel")
	e.Attempts[0].ProviderResponseID = costPtr(strings.Repeat("x", 257))
	n := NormalizeRequestCostEvidence(e)
	if n.Attempts[0].ObservedModel != nil || n.Attempts[0].ProviderResponseID != nil {
		t.Fatal("invalid identity retained")
	}
	if e.Attempts[0].ObservedModel == nil {
		t.Fatal("normalization mutated input")
	}
}

func TestRequestCostDigestNullZeroAndOrder(t *testing.T) {
	e := costResponse("A", "").RequestCostEvidence
	e.Attempts[0].Usage.CacheWriteTokens = nil
	first, _ := RequestCostEvidenceDigest(e)
	e.Attempts[0].Usage.CacheWriteTokens = costPtr(int64(0))
	second, _ := RequestCostEvidenceDigest(e)
	if first == second {
		t.Fatal("null equals numeric zero")
	}
	e.Attempts = append(e.Attempts, costResponse("B", "").RequestCostEvidence.Attempts[0])
	e.AttemptsObserved = 2
	third, _ := RequestCostEvidenceDigest(e)
	slices.Reverse(e.Attempts)
	fourth, _ := RequestCostEvidenceDigest(e)
	if third == fourth {
		t.Fatal("chronological order absent from digest")
	}
	clone := CloneRequestCostEvidence(e)
	if !reflect.DeepEqual(NormalizeRequestCostEvidence(clone), NormalizeRequestCostEvidence(e)) {
		t.Fatal("clone changed values")
	}
	data, _ := RequestCostEvidenceBytes(e)
	var decoded RequestCostEvidence
	if json.Unmarshal(data, &decoded) != nil {
		t.Fatal("canonical bytes are not JSON")
	}
}

type costBackoffContext struct {
	context.Context
	canceled bool
	done     chan struct{}
}

func (c *costBackoffContext) Done() <-chan struct{} {
	if !c.canceled {
		c.canceled = true
		close(c.done)
	}
	return c.done
}
func (c *costBackoffContext) Err() error {
	if c.canceled {
		return context.Canceled
	}
	return nil
}
func TestRequestCostCancellationInsideBackoffRetainsReceipt(t *testing.T) {
	old := activeRetryPolicy
	activeRetryPolicy = retryPolicy{MaxRetries: 3, Base: 1_000_000_000, Multiplier: 2}
	t.Cleanup(func() { activeRetryPolicy = old })
	ctx := &costBackoffContext{Context: context.Background(), done: make(chan struct{})}
	a := &costFakeAdapter{FakeAdapter: &FakeAdapter{}}
	a.Queue(FakeResult{Response: costResponse("observed-before-backoff", ``), WaitErr: ErrTransient})
	got, err := RunStructuredWithSessionResume(ctx, a, "", Request{Model: "requested"}, costDecode)
	if err == nil || !ctx.canceled || len(a.Requests()) != 1 || len(got.Response.RequestCostEvidence.Attempts) < 1 || *got.Response.RequestCostEvidence.Attempts[0].ProviderResponseID != "observed-before-backoff" {
		t.Fatal("canceled backoff erased or retried receipt")
	}
}
func TestRequestCostTransientBudgetsAcrossValidationPhases(t *testing.T) {
	old := activeRetryPolicy
	activeRetryPolicy = retryPolicy{MaxRetries: 1}
	t.Cleanup(func() { activeRetryPolicy = old })
	a := &costFakeAdapter{FakeAdapter: &FakeAdapter{}}
	for _, r := range []FakeResult{
		{Response: costResponse("1", ""), WaitErr: ErrTransient}, {Response: costResponse("2", "invalid")},
		{Response: costResponse("3", ""), WaitErr: ErrTransient}, {Response: costResponse("4", `{"ok":true}`)},
	} {
		a.Queue(r)
	}
	got, err := RunStructuredWithSessionResume(context.Background(), a, "", Request{Model: "requested", FreshValidationRetrySession: true}, costDecode)
	if err != nil || len(got.Response.RequestCostEvidence.Attempts) != 4 || len(got.ValidationAttempts) != 1 {
		t.Fatalf("err=%v result=%#v", err, got)
	}
	for i, r := range got.Response.RequestCostEvidence.Attempts {
		if r.Ordinal != uint32(i+1) || r.AdapterAttempt != uint32(i%2+1) {
			t.Fatal("phase budget reset global ordinal")
		}
	}
}

func TestRequestCostEvidenceCanonicalGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/request-cost-evidence-v1.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var e RequestCostEvidence
	if json.Unmarshal(want, &e) != nil {
		t.Fatal("golden fixture")
	}
	got, err := RequestCostEvidenceBytes(&e)
	if err != nil || string(got) != string(want) {
		t.Fatalf("canonical bytes changed: %s %v", got, err)
	}
	digest, err := RequestCostEvidenceDigest(&e)
	if err != nil || digest != "40a0d89467879b363d192692bb0df2599c92fb476b4e1f7db8ad88368ad842d0" {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
}

func TestRequestCostRejectsUnicodeReplacementIdentities(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`"\ud800"`), []byte(`"\udc00"`), []byte(`"\ud800\u0041"`), {'"', 0xff, '"'}} {
		if CostJSONUnicodeValid(raw) {
			t.Fatalf("invalid Unicode accepted: %q", raw)
		}
	}
	for _, raw := range []string{`"\ud83d\ude00"`, `"\\ud800"`, `"valid\u0041"`, `"literal �"`} {
		if !CostJSONUnicodeValid([]byte(raw)) {
			t.Fatalf("valid Unicode rejected: %s", raw)
		}
	}
	if RequestCostJSONValid([]byte(`{"identity":"\ud800"}`)) {
		t.Fatal("optional cost decoder could replace identity")
	}
}

func TestRequestCostBaseStreamReturnsDetachedSnapshots(t *testing.T) {
	stream := NewBaseStream(func() {})
	source := costResponse("source", `{"ok":true}`)
	source.Usage.TokensIn = costPtr(10)
	source.ReviewerToolEvidence = &ReviewerToolEvidence{DiffStatus: DiffToolStatusSucceeded, Trace: &ReviewerToolTrace{Version: 1, Source: "pi_rpc", Calls: []ReviewerToolCallEvidence{{CallID: "one"}}}}
	stream.Finish(source, nil)
	*source.RequestCostEvidence.Attempts[0].Usage.InputTokens = 999
	*source.Usage.TokensIn = 999
	source.ReviewerToolEvidence.Trace.Calls[0].CallID = "mutated"
	first, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*first.RequestCostEvidence.Attempts[0].Usage.InputTokens = 777
	*first.Usage.TokensIn = 777
	first.ReviewerToolEvidence.Trace.Calls[0].CallID = "again"
	second, err := stream.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *second.RequestCostEvidence.Attempts[0].Usage.InputTokens != 10 || *second.Usage.TokensIn != 10 || second.ReviewerToolEvidence.Trace.Calls[0].CallID != "one" {
		t.Fatal("Finish or Wait retained shared mutable response")
	}
	results := make(chan Response, 2)
	for i := 0; i < 2; i++ {
		go func() { r, _ := stream.Wait(context.Background()); results <- r }()
	}
	a, b := <-results, <-results
	*a.RequestCostEvidence.Attempts[0].Usage.InputTokens = 666
	if *b.RequestCostEvidence.Attempts[0].Usage.InputTokens != 10 {
		t.Fatal("concurrent Wait copies alias")
	}
}
