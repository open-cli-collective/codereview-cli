package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFakeAdapterAndRunStructured(t *testing.T) {
	t.Run("captures requests and retries validation failure once", func(t *testing.T) {
		adapter := &FakeAdapter{}
		adapter.Queue(FakeResult{SessionID: "s1", Response: Response{StructuredOutput: []byte(`{"bad":true}`)}})
		adapter.Queue(FakeResult{SessionID: "s2", Response: Response{StructuredOutput: []byte(`"ok"`), Usage: Usage{TokensIn: intPtr(1)}}})

		got, response, err := RunStructured(context.Background(), adapter, Request{Model: "model", Prompt: "prompt"}, func(data []byte) (string, error) {
			if string(data) != `"ok"` {
				return "", errors.New("bad json")
			}
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("RunStructured: %v", err)
		}
		if got != "ok" || response.Usage.TokensIn == nil || *response.Usage.TokensIn != 1 {
			t.Fatalf("RunStructured = %q %#v, want ok response with nullable usage", got, response)
		}
		requests := adapter.Requests()
		if len(requests) != 2 {
			t.Fatalf("requests = %d, want retry", len(requests))
		}
		if !strings.Contains(requests[1].Prompt, "failed validation") || !strings.Contains(requests[1].Prompt, "bad json") {
			t.Fatalf("retry prompt = %q, want validation suffix", requests[1].Prompt)
		}
	})

	t.Run("two invalid outputs fail", func(t *testing.T) {
		adapter := &FakeAdapter{}
		adapter.Queue(FakeResult{Response: Response{StructuredOutput: []byte(`bad1`)}})
		adapter.Queue(FakeResult{Response: Response{StructuredOutput: []byte(`bad2`)}})
		_, _, err := RunStructured(context.Background(), adapter, Request{Prompt: "prompt"}, func([]byte) (string, error) {
			return "", errors.New("invalid")
		})
		if err == nil {
			t.Fatal("RunStructured error = nil, want validation failure")
		}
		if got := len(adapter.Requests()); got != 2 {
			t.Fatalf("requests = %d, want one retry", got)
		}
	})

	t.Run("start and wait errors are not schema retries", func(t *testing.T) {
		startErr := errors.New("start failed")
		adapter := &FakeAdapter{}
		adapter.Queue(FakeResult{StartErr: startErr})
		if _, _, err := RunStructured(context.Background(), adapter, Request{}, func([]byte) (string, error) { return "unused", nil }); !errors.Is(err, startErr) {
			t.Fatalf("start error = %v, want %v", err, startErr)
		}

		waitErr := errors.New("wait failed")
		adapter = &FakeAdapter{}
		adapter.Queue(FakeResult{WaitErr: waitErr})
		if _, _, err := RunStructured(context.Background(), adapter, Request{}, func([]byte) (string, error) { return "unused", nil }); !errors.Is(err, waitErr) {
			t.Fatalf("wait error = %v, want %v", err, waitErr)
		}
	})
}

func TestFakeAdapterQuotaAndResume(t *testing.T) {
	quotaErr := errors.New("quota failed")
	adapter := &FakeAdapter{
		NameValue:                    "fake-test",
		SupportsResumeValue:          true,
		SupportsCacheAccountingValue: true,
		SupportsCostReportingValue:   false,
		QuotaValue:                   Quota{BlockRemainingPct: 10, WeeklyRemainingPct: -1},
		QuotaSupported:               true,
		QuotaErr:                     quotaErr,
	}
	if adapter.Name() != "fake-test" || !adapter.SupportsResume() || !adapter.SupportsCacheAccounting() || adapter.SupportsCostReporting() {
		t.Fatalf("fake capabilities not reported as configured")
	}
	quota, supported, err := adapter.Quota(context.Background())
	if !errors.Is(err, quotaErr) || !supported || quota.BlockRemainingPct != 10 || quota.WeeklyRemainingPct != -1 {
		t.Fatalf("Quota = %#v %v %v, want configured tuple", quota, supported, err)
	}

	adapter.Queue(FakeResult{SessionID: "resume-session", Response: Response{StructuredOutput: []byte(`{}`)}})
	stream, err := adapter.Resume(context.Background(), "old", Request{Prompt: "resume"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if stream.SessionID() != "resume-session" {
		t.Fatalf("SessionID = %q, want resume-session", stream.SessionID())
	}
}

func intPtr(value int) *int {
	return &value
}
