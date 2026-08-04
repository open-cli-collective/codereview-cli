package llmadapters

import (
	"errors"
	"testing"
)

func TestClassifyHTTPStatusTransient(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true}, {500, true}, {502, true}, {503, true}, {504, true}, {529, true},
		{200, false}, {400, false}, {401, false}, {404, false},
	}
	for _, tc := range cases {
		if got := classifyHTTPStatusTransient(tc.status); got != tc.want {
			t.Errorf("classifyHTTPStatusTransient(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestIsTransientCLIDetail(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"Overloaded", true}, {"model is overloaded_error", true}, {"rate limit exceeded", true},
		{"rate_limit_error", true}, {"got HTTP 529", true}, {"upstream returned 503", true},
		{"request timed out", true}, {"connection reset by peer", true}, {"service unavailable", true},
		{"invalid prompt", false}, {"tool use error", false}, {"", false},
	}
	for _, tc := range cases {
		if got := isTransientCLIDetail(tc.detail); got != tc.want {
			t.Errorf("isTransientCLIDetail(%q) = %v, want %v", tc.detail, got, tc.want)
		}
	}
}

func TestIsMissingSessionCLIDetail(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		// The two forms Claude reports, from issue #538.
		{"exit 1 before init — No conversation found with session ID: cf06076d-7175-4112-9cda-dbecea14a4a7", true},
		{"source session e34a5ddf-3343-479f-866a-20c863967420 not found", true},
		{"session not found", true},
		// A provider failure is not a missing conversation, and must keep its
		// own classification so it is retried rather than restarted fresh.
		{"model is overloaded_error", false},
		{"request timed out", false},
		{"not found", false},
		{"source session e34a5ddf is fine", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isMissingSessionCLIDetail(tc.detail); got != tc.want {
			t.Errorf("isMissingSessionCLIDetail(%q) = %v, want %v", tc.detail, got, tc.want)
		}
	}
}

func TestClassifyCLIDetail(t *testing.T) {
	base := errors.New("boom")

	missing := classifyCLIDetail(base, "No conversation found with session ID: abc")
	if !errors.Is(missing, ErrMissingProviderSession) {
		t.Errorf("missing-session detail did not wrap ErrMissingProviderSession: %v", missing)
	}
	if errors.Is(missing, ErrTransient) {
		t.Errorf("missing-session detail must not also be transient: %v", missing)
	}

	transient := classifyCLIDetail(base, "upstream returned 503")
	if !errors.Is(transient, ErrTransient) {
		t.Errorf("transient detail did not wrap ErrTransient: %v", transient)
	}
	if errors.Is(transient, ErrMissingProviderSession) {
		t.Errorf("transient detail must not be treated as a missing session: %v", transient)
	}

	plain := classifyCLIDetail(base, "invalid prompt")
	if errors.Is(plain, ErrTransient) || errors.Is(plain, ErrMissingProviderSession) {
		t.Errorf("unclassified detail gained a sentinel: %v", plain)
	}
	if !errors.Is(plain, base) {
		t.Errorf("classifyCLIDetail dropped the underlying error: %v", plain)
	}
}
