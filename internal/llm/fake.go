package llm

import (
	"context"
	"fmt"
	"sync"
)

// FakeAdapter is a deterministic Adapter test double. Configure exported fields
// before concurrent use; adapter methods lock around captured requests/results.
type FakeAdapter struct {
	mu sync.Mutex

	NameValue                    string
	SupportsResumeValue          bool
	SupportsCacheAccountingValue bool
	SupportsCostReportingValue   bool

	QuotaValue     Quota
	QuotaSupported bool
	QuotaErr       error

	requests []Request
	resumes  []ResumeRequest
	results  []FakeResult
}

// FakeResult is one queued fake Start result.
type FakeResult struct {
	SessionID string
	Response  Response
	StartErr  error
	WaitErr   error
}

// ResumeRequest is one captured Resume invocation.
type ResumeRequest struct {
	SessionID string
	Request   Request
}

// Name returns the configured adapter name.
func (f *FakeAdapter) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NameValue != "" {
		return f.NameValue
	}
	return "fake"
}

// SupportsResume reports whether the fake supports session resume.
func (f *FakeAdapter) SupportsResume() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SupportsResumeValue
}

// SupportsCacheAccounting reports whether cache usage metrics are supported.
func (f *FakeAdapter) SupportsCacheAccounting() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SupportsCacheAccountingValue
}

// SupportsCostReporting reports whether cost metrics are supported.
func (f *FakeAdapter) SupportsCostReporting() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SupportsCostReportingValue
}

// Quota returns the configured quota tuple.
func (f *FakeAdapter) Quota(context.Context) (Quota, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.QuotaValue, f.QuotaSupported, f.QuotaErr
}

// Queue appends one result for a future Start call.
func (f *FakeAdapter) Queue(result FakeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, result)
}

// Requests returns captured Start requests.
func (f *FakeAdapter) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Resumes returns captured Resume requests.
func (f *FakeAdapter) Resumes() []ResumeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ResumeRequest(nil), f.resumes...)
}

// Start captures req and returns the next queued fake stream.
func (f *FakeAdapter) Start(ctx context.Context, req Request) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.results) == 0 {
		return nil, fmt.Errorf("llm fake: no queued result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result.StartErr != nil {
		return nil, result.StartErr
	}
	return fakeStream{result: result}, nil
}

// Resume is unsupported by the fake unless the caller explicitly opts in.
func (f *FakeAdapter) Resume(ctx context.Context, sessionID string, req Request) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.SupportsResumeValue {
		return nil, fmt.Errorf("llm fake: resume unsupported")
	}
	f.resumes = append(f.resumes, ResumeRequest{SessionID: sessionID, Request: req})
	if len(f.results) == 0 {
		return nil, fmt.Errorf("llm fake: no queued result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result.StartErr != nil {
		return nil, result.StartErr
	}
	return fakeStream{result: result}, nil
}

type fakeStream struct {
	result FakeResult
}

func (s fakeStream) SessionID() string {
	return s.result.SessionID
}

func (s fakeStream) Wait(ctx context.Context) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if s.result.WaitErr != nil {
		return Response{}, s.result.WaitErr
	}
	return s.result.Response, nil
}
