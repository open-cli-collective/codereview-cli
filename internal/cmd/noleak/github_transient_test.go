package noleak

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
)

// These drive the real `cr review` command tree and the real GitHub client
// against the fake server, with the retry waits injected away.

func newTransientHarness(t *testing.T) *auditHarness {
	t.Helper()
	h := newAuditHarness(t)
	t.Chdir(t.TempDir())
	seedConfiguredCredentials(t, h)
	return h
}

func (h *auditHarness) reviewsPath() string {
	return fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", h.prRef.Owner, h.prRef.Repo, h.prRef.Number)
}

func TestLiveReviewSurvivesFailureOfEveryGitHubReadOnce(t *testing.T) {
	h := newTransientHarness(t)
	var mu sync.Mutex
	failed := map[string]bool{}
	statuses := []int{500, 502, 503, 504}
	h.githubFault = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.String() + " " + r.Header.Get("Accept")
		if r.Method != http.MethodGet || failed[key] {
			return false
		}
		failed[key] = true
		w.WriteHeader(statuses[len(failed)%len(statuses)])
		return true
	}

	stdout, stderr, err := h.run([]string{"review", h.prURL})
	if err != nil {
		t.Fatalf("review: %v\nstdout = %s\nstderr = %s", err, stdout, stderr)
	}
	if len(failed) < 3 {
		t.Fatalf("faulted reads = %v, want the run to have crossed several", failed)
	}
	if !strings.Contains(stdout, "Posts: posted=") || strings.Contains(stdout, "Status: error") {
		t.Fatalf("stdout = %q, want a completed review", stdout)
	}
	if h.llmCalls.Load() == 0 {
		t.Fatal("review never reached the model")
	}
}

func TestLiveReviewFailsClearlyWhenGitHubStaysDown(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra []string
		check func(t *testing.T, stdout string)
	}{
		{name: "text", check: func(t *testing.T, stdout string) {
			t.Helper()
			for _, want := range []string{"Status: error\n", "Exit code: 5\n", "status 502", "gave up after 6 attempts"} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("stdout = %q, want %q", stdout, want)
				}
			}
		}},
		{name: "json", extra: []string{"--json"}, check: func(t *testing.T, stdout string) {
			t.Helper()
			var rendered struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(stdout), &rendered); err != nil {
				t.Fatalf("stdout %q: %v", stdout, err)
			}
			if rendered.Status != "error" || !strings.Contains(rendered.Message, "gave up after 6 attempts") {
				t.Fatalf("rendered = %#v", rendered)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTransientHarness(t)
			var prReads atomic.Int32
			pullPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", h.prRef.Owner, h.prRef.Repo, h.prRef.Number)
			h.githubFault = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.EscapedPath() != pullPath {
					return false
				}
				prReads.Add(1)
				w.WriteHeader(http.StatusBadGateway)
				return true
			}

			stdout, stderr, err := h.run(append([]string{"review", h.prURL}, tt.extra...))
			if err == nil {
				t.Fatalf("review error = nil; stdout = %s", stdout)
			}
			if code := exitcode.FromError(err); code != exitcode.UpstreamError {
				t.Fatalf("exit = %d, want %d: %v", code, exitcode.UpstreamError, err)
			}
			if !strings.Contains(err.Error(), "gave up after 6 attempts") {
				t.Fatalf("error = %q, want the attempts named", err)
			}
			if prReads.Load() != 6 {
				t.Fatalf("PR reads = %d, want 6", prReads.Load())
			}
			if !strings.Contains(stderr, `op="execute_live"`) || !strings.Contains(stderr, "status=error") {
				t.Fatalf("stderr = %q, want the failing progress record", stderr)
			}
			if h.llmCalls.Load() != 0 {
				t.Fatalf("LLM calls = %d, want none", h.llmCalls.Load())
			}
			tt.check(t, stdout)
			h.assertNoLeaks(t, "stdout", []byte(stdout))
			h.assertNoLeaks(t, "stderr", []byte(stderr))
			h.assertNoLeaks(t, "returned error", []byte(err.Error()))
		})
	}
}

// reviewHost makes POST /reviews stateful: a kept body appears in later GETs.
type reviewHost struct {
	mu    sync.Mutex
	posts int
	kept  []string
}

func (host *reviewHost) fault(h *auditHarness, onPost func(attempt int) (keep bool, status int)) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.EscapedPath() != h.reviewsPath() {
			return false
		}
		host.mu.Lock()
		defer host.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			host.posts++
			var payload struct {
				Body string `json:"body"`
			}
			raw, err := io.ReadAll(r.Body)
			if err == nil {
				err = json.Unmarshal(raw, &payload)
			}
			if err != nil {
				h.t.Errorf("decode review: %v", err)
			}
			keep, status := onPost(host.posts)
			if keep {
				host.kept = append(host.kept, payload.Body)
			}
			w.WriteHeader(status)
			if status < 300 {
				_, _ = w.Write([]byte(`{"id":301}`))
			}
		default:
			reviews := make([]map[string]any, 0, len(host.kept))
			for i, body := range host.kept {
				reviews = append(reviews, map[string]any{
					"id":           301 + i,
					"body":         body,
					"state":        "COMMENTED",
					"commit_id":    h.headSHA,
					"user":         map[string]any{"login": "reviewer-user", "id": 1002},
					"submitted_at": h.now.Format("2006-01-02T15:04:05Z"),
				})
			}
			writeHTTPJSON(h.t, w, reviews)
		}
		return true
	}
}

func TestLiveReviewThatLandedAndReturned502IsNotPostedTwice(t *testing.T) {
	h := newTransientHarness(t)
	host := &reviewHost{}
	h.githubFault = host.fault(h, func(int) (bool, int) { return true, http.StatusBadGateway })

	stdout, stderr, err := h.run([]string{"review", h.prURL})
	if err != nil {
		t.Fatalf("review: %v\nstdout = %s\nstderr = %s", err, stdout, stderr)
	}
	if host.posts != 1 || len(host.kept) != 1 {
		t.Fatalf("review POSTs = %d, kept = %d, want exactly one of each", host.posts, len(host.kept))
	}
	if !strings.Contains(stdout, "pending=0") || !strings.Contains(stdout, "failed_terminal=0") {
		t.Fatalf("stdout = %q, want nothing left pending", stdout)
	}
}

func TestLiveReviewThatDidNotLandIsResent(t *testing.T) {
	h := newTransientHarness(t)
	host := &reviewHost{}
	h.githubFault = host.fault(h, func(attempt int) (bool, int) {
		if attempt == 1 {
			return false, http.StatusBadGateway
		}
		return true, http.StatusOK
	})

	stdout, stderr, err := h.run([]string{"review", h.prURL})
	if err != nil {
		t.Fatalf("review: %v\nstdout = %s\nstderr = %s", err, stdout, stderr)
	}
	if host.posts != 2 || len(host.kept) != 1 {
		t.Fatalf("review POSTs = %d, kept = %d, want two sends and one kept", host.posts, len(host.kept))
	}
}

func TestLiveReviewFailedMarkerCheckStopsWithNonZeroExitAndNoSecondPost(t *testing.T) {
	h := newTransientHarness(t)
	host := &reviewHost{}
	reviewFault := host.fault(h, func(int) (bool, int) { return true, http.StatusBadGateway })
	h.githubFault = func(w http.ResponseWriter, r *http.Request) bool {
		host.mu.Lock()
		posted := host.posts > 0
		host.mu.Unlock()
		// Reads after the failed write cannot confirm anything.
		if posted && r.Method == http.MethodGet && r.URL.EscapedPath() == h.reviewsPath() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return reviewFault(w, r)
	}

	stdout, stderr, err := h.run([]string{"review", h.prURL})
	if err == nil {
		t.Fatalf("review error = nil; stdout = %s", stdout)
	}
	if code := exitcode.FromError(err); code != exitcode.UpstreamError {
		t.Fatalf("exit = %d, want upstream: %v", code, err)
	}
	if host.posts != 1 {
		t.Fatalf("review POSTs = %d, want 1", host.posts)
	}
	if !strings.Contains(stdout, "post(s) still pending") || !strings.Contains(stdout, "run cr review again") {
		t.Fatalf("stdout = %q\nstderr = %q, want the pending cause and next step", stdout, stderr)
	}
}
