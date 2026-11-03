package github

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// retryRecorder injects the wait and records what the policy asked for.
type retryRecorder struct {
	delays []time.Duration
	events []RetryEvent
}

func (r *retryRecorder) policy() RetryPolicy {
	return RetryPolicy{
		Sleep: func(_ context.Context, d time.Duration) error {
			r.delays = append(r.delays, d)
			return nil
		},
		Jitter:  func(d time.Duration) time.Duration { return d },
		OnRetry: func(event RetryEvent) { r.events = append(r.events, event) },
	}
}

func retryClient(t *testing.T, serverURL string, policy RetryPolicy) *Client {
	t.Helper()
	return mustClient(t, Options{Token: "token", BaseURL: serverURL, GraphQLURL: serverURL + "/graphql", Retry: policy})
}

func patCredential() gitprovider.Credential {
	return gitprovider.Credential{Type: credentialTypePAT, Token: "token"}
}

// failingThenOK answers with the given statuses in turn, then with okBody.
func failingThenOK(calls *atomic.Int32, okBody string, statuses ...int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1))
		if n <= len(statuses) {
			w.WriteHeader(statuses[n-1])
			return
		}
		_, _ = w.Write([]byte(okBody))
	})
}

func TestReadRetriesServerErrorsThenSucceeds(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(failingThenOK(&calls, `{"login":"octocat","id":1}`, status))
			defer server.Close()
			recorder := &retryRecorder{}

			identity, err := retryClient(t, server.URL, recorder.policy()).WhoAmI(context.Background(), patCredential())
			if err != nil {
				t.Fatalf("WhoAmI: %v", err)
			}
			if identity.Login != "octocat" || calls.Load() != 2 {
				t.Fatalf("identity = %#v after %d calls, want octocat after 2", identity, calls.Load())
			}
			if len(recorder.events) != 1 {
				t.Fatalf("events = %#v, want one", recorder.events)
			}
			event := recorder.events[0]
			if event.Op != gitprovider.OperationWhoAmI || event.Attempt != 1 || event.MaxAttempts != defaultRetryMaxAttempts || event.Delay != defaultRetryBaseDelay || !strings.Contains(event.Reason, "status") {
				t.Fatalf("event = %#v", event)
			}
		})
	}
}

func TestReadRetriesConnectionReset(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			if tcp, ok := conn.(interface{ SetLinger(int) error }); ok {
				_ = tcp.SetLinger(0) // Close with RST rather than FIN.
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"login":"octocat","id":1}`))
	}))
	defer server.Close()

	if _, err := retryClient(t, server.URL, (&retryRecorder{}).policy()).WhoAmI(context.Background(), patCredential()); err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestGraphQLReadRetriesServerErrorAndRetryableEnvelope(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			writeJSON(t, w, map[string]any{"errors": []graphQLError{{Type: "INTERNAL", Message: "server failed"}}})
		default:
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`))
		}
	}))
	defer server.Close()

	threads, err := retryClient(t, server.URL, (&retryRecorder{}).policy()).ListInlineThreads(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	if len(threads) != 0 || calls.Load() != 3 {
		t.Fatalf("threads = %#v after %d calls, want none after 3", threads, calls.Load())
	}
}

func TestReadGivesUpAfterBoundedBackoffWithClearError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	recorder := &retryRecorder{}

	_, err := retryClient(t, server.URL, recorder.policy()).GetPR(context.Background(), testPRRef())
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("error = %v, want ErrRetryable", err)
	}
	for _, want := range []string{"GetPR", "status 502", "gave up after 6 attempts"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if calls.Load() != defaultRetryMaxAttempts {
		t.Fatalf("calls = %d, want %d", calls.Load(), defaultRetryMaxAttempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if len(recorder.delays) != len(want) {
		t.Fatalf("delays = %v, want %v", recorder.delays, want)
	}
	for i := range want {
		if recorder.delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", recorder.delays, want)
		}
	}
}

func TestDefaultJitterStaysWithinBackoff(t *testing.T) {
	for range 200 {
		if got := equalJitter(8 * time.Second); got < 4*time.Second || got > 8*time.Second {
			t.Fatalf("equalJitter = %s, want 4s to 8s", got)
		}
	}
}

func TestRetryStopsAtElapsedBudget(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "100")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := mustClient(t, Options{
		Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql",
		Now: func() time.Time { return now },
		Retry: RetryPolicy{
			MaxElapsed: 3 * time.Minute,
			Jitter:     func(d time.Duration) time.Duration { return d },
			Sleep: func(_ context.Context, d time.Duration) error {
				now = now.Add(d)
				return nil
			},
		},
	})

	_, err := client.GetPR(context.Background(), testPRRef())
	if !errors.Is(err, gitprovider.ErrRetryable) || !strings.Contains(err.Error(), "gave up after 2 attempts over 1m40s") {
		t.Fatalf("error = %v, want elapsed-budget failure", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    int
		header    map[string]string
		body      string
		wantDelay time.Duration
	}{
		{name: "503 seconds", status: 503, header: map[string]string{"Retry-After": "7"}, wantDelay: 7 * time.Second},
		{name: "503 http date", status: 503, header: map[string]string{"Retry-After": now.Add(9 * time.Second).UTC().Format(http.TimeFormat)}, wantDelay: 9 * time.Second},
		{name: "shorter than backoff keeps backoff", status: 503, header: map[string]string{"Retry-After": "0"}, wantDelay: time.Second},
		{name: "unparseable falls back to backoff", status: 503, header: map[string]string{"Retry-After": "soon"}, wantDelay: time.Second},
		{name: "429 retry after", status: 429, header: map[string]string{"Retry-After": "30"}, wantDelay: 30 * time.Second},
		{name: "secondary limit as 403 retry after", status: 403, header: map[string]string{"Retry-After": "45"}, wantDelay: 45 * time.Second},
		{name: "secondary limit as 403 message only", status: 403, body: `{"message":"You have exceeded a secondary rate limit."}`, wantDelay: time.Minute},
		{name: "429 no headers", status: 429, wantDelay: time.Minute},
		{name: "exhausted quota reset", status: 403, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(now.Add(20*time.Second).Unix(), 10)}, wantDelay: 20 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					for key, value := range tt.header {
						w.Header().Set(key, value)
					}
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
					return
				}
				_, _ = w.Write([]byte(`{"login":"octocat","id":1}`))
			}))
			defer server.Close()
			recorder := &retryRecorder{}
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql", Now: func() time.Time { return now }, Retry: recorder.policy()})

			if _, err := client.WhoAmI(context.Background(), patCredential()); err != nil {
				t.Fatalf("WhoAmI: %v", err)
			}
			if len(recorder.delays) != 1 || recorder.delays[0] != tt.wantDelay {
				t.Fatalf("delays = %v, want [%s]", recorder.delays, tt.wantDelay)
			}
		})
	}
}

func TestRetryRefusesWaitBeyondLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1800")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	recorder := &retryRecorder{}

	_, err := retryClient(t, server.URL, recorder.policy()).GetPR(context.Background(), testPRRef())
	if !errors.Is(err, gitprovider.ErrRetryable) || errors.Is(err, gitprovider.ErrPermission) {
		t.Fatalf("error = %v, want rate limit as ErrRetryable", err)
	}
	if !strings.Contains(err.Error(), "GitHub asked to wait 30m0s") {
		t.Fatalf("error = %q, want the requested wait", err)
	}
	if calls.Load() != 1 || len(recorder.delays) != 0 {
		t.Fatalf("calls = %d delays = %v, want one call and no wait", calls.Load(), recorder.delays)
	}
}

func TestNonTransientFailuresAreNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "404 fails fast", status: 404, want: gitprovider.ErrNotFound},
		{name: "401", status: 401, want: gitprovider.ErrAuth},
		{name: "403 without rate limit signal", status: 403, body: `{"message":"Resource not accessible"}`, want: gitprovider.ErrPermission},
		{name: "409", status: 409, want: gitprovider.ErrConflict},
		{name: "422", status: 422, want: ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			recorder := &retryRecorder{}

			_, err := retryClient(t, server.URL, recorder.policy()).GetDiff(context.Background(), testPRRef())
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if calls.Load() != 1 || len(recorder.delays) != 0 {
				t.Fatalf("calls = %d delays = %v, want one call and no wait", calls.Load(), recorder.delays)
			}
		})
	}
}

func TestWritesAreNotRetriedByTheClient(t *testing.T) {
	ref := testPRRef()
	sha := "head-sha"
	tests := []struct {
		name  string
		write func(*Client) error
	}{
		{name: "inline comment", write: func(c *Client) error {
			_, err := c.PostInlineComment(context.Background(), ref, gitprovider.InlineComment{CommitSHA: sha, Body: "body", Path: "a.go", Side: review.DiffSideRight, Line: 1, SubjectType: review.AnchorKindLine})
			return err
		}},
		{name: "issue comment", write: func(c *Client) error {
			_, err := c.PostIssueComment(context.Background(), ref, "body")
			return err
		}},
		{name: "review", write: func(c *Client) error {
			_, err := c.SubmitReview(context.Background(), ref, gitprovider.ReviewRequest{CommitSHA: sha, Event: review.ReviewEventComment, Body: "body"})
			return err
		}},
		{name: "thread reply", write: func(c *Client) error {
			_, err := c.ReplyToThread(context.Background(), ref, "thread-1", "body")
			return err
		}},
		{name: "resolve thread", write: func(c *Client) error {
			return c.ResolveThread(context.Background(), ref, "thread-1")
		}},
		{name: "accept invitation", write: func(c *Client) error {
			return c.AcceptRepositoryInvitation(context.Background(), "7")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer server.Close()
			recorder := &retryRecorder{}

			err := tt.write(retryClient(t, server.URL, recorder.policy()))
			if !errors.Is(err, gitprovider.ErrRetryable) {
				t.Fatalf("error = %v, want ErrRetryable", err)
			}
			if calls.Load() != 1 || len(recorder.delays) != 0 {
				t.Fatalf("calls = %d delays = %v, want one send and no wait", calls.Load(), recorder.delays)
			}
		})
	}
}

// Fails closed: a new write must be added here deliberately, never inherit retry.
func TestRetrySafeOperationClassifiesEveryOperation(t *testing.T) {
	want := map[gitprovider.Operation]bool{
		gitprovider.OperationWhoAmI:                     true,
		gitprovider.OperationReviewAuthority:            true,
		gitprovider.OperationGetPR:                      true,
		gitprovider.OperationGetDiff:                    true,
		gitprovider.OperationGetDiffBetweenRefs:         true,
		gitprovider.OperationGetFileAtRef:               true,
		gitprovider.OperationListTreeAtRef:              true,
		gitprovider.OperationListInlineThreads:          true,
		gitprovider.OperationListReviews:                true,
		gitprovider.OperationListIssueComments:          true,
		gitprovider.OperationListRepositoryInvitations:  true,
		gitprovider.OperationPostInlineComment:          false,
		gitprovider.OperationReplyToThread:              false,
		gitprovider.OperationResolveThread:              false,
		gitprovider.OperationPostIssueComment:           false,
		gitprovider.OperationSubmitReview:               false,
		gitprovider.OperationAcceptRepositoryInvitation: false,
		gitprovider.Operation("SomethingNew"):           false,
	}
	for op, safe := range want {
		if got := retrySafeOperation(op); got != safe {
			t.Errorf("retrySafeOperation(%s) = %t, want %t", op, got, safe)
		}
	}
}

func TestRetryStopsWhenContextIsCanceledDuringWait(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := retryClient(t, server.URL, RetryPolicy{Sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}})

	if _, err := client.GetPR(ctx, testPRRef()); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestRetryNeverWaitsPastContextDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder := &retryRecorder{}

	_, err := retryClient(t, server.URL, recorder.policy()).GetPR(ctx, testPRRef())
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("error = %v, want ErrRetryable", err)
	}
	if calls.Load() != 1 || len(recorder.delays) != 0 {
		t.Fatalf("calls = %d delays = %v, want one call and no wait", calls.Load(), recorder.delays)
	}
}

func TestCertificateFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	recorder := &retryRecorder{}
	client := mustClient(t, Options{
		Token: "token", BaseURL: "https://example.invalid/", GraphQLURL: "https://example.invalid/graphql",
		Retry: recorder.policy(),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, &tls.CertificateVerificationError{Err: errors.New("unknown authority")}
		})},
	})

	if _, err := client.GetPR(context.Background(), testPRRef()); !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("error = %v, want ErrRetryable classification", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestAppTokenMintRetriesEvenForAWrite(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	_, privateKeyPEM := testPrivateKey(t)
	var mints, reviews atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42":
			writeJSON(t, w, map[string]any{"id": 42, "app_id": 12345, "app_slug": "cr-reviewer"})
		case "/app/installations/42/access_tokens":
			// The first mint is the client's construction; the second is the
			// refresh a write triggers after the token has expired.
			if mints.Add(1) == 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			writeJSON(t, w, map[string]any{"token": "installation-token", "expires_at": now.Add(time.Hour).Format(time.RFC3339)})
		default:
			reviews.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer server.Close()
	recorder := &retryRecorder{}

	client, _, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", privateKeyPEM), Options{
		BaseURL: server.URL, GraphQLURL: server.URL + "/graphql",
		Now:            func() time.Time { return now },
		InstallationID: "42",
		Retry:          recorder.policy(),
	})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	now = now.Add(2 * time.Hour)

	_, err = client.PostIssueComment(context.Background(), gitprovider.PRRef{Host: "github.com", Owner: "o", Repo: "r", Number: 1}, "body")
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("error = %v, want the write's own 502", err)
	}
	if mints.Load() != 3 {
		t.Fatalf("token mints = %d, want initial + failed + retried", mints.Load())
	}
	if reviews.Load() != 1 {
		t.Fatalf("comment sends = %d, want 1", reviews.Load())
	}
}

func TestThreadCommentBotAuthorMatchesPostingIdentity(t *testing.T) {
	payload, err := os.ReadFile("testdata/review_threads_bot_author.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	threads, err := retryClient(t, server.URL, RetryPolicy{}).ListInlineThreads(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	comments := threads[0].Comments
	posting := gitprovider.Identity{Login: "cr-reviewer[bot]", DisplayName: "cr-reviewer"}
	if !comments[0].Author.Same(posting) {
		t.Fatalf("app author = %#v, want it to match the posting identity", comments[0].Author)
	}
	if comments[1].Author.Same(posting) {
		t.Fatalf("person author = %#v, must not match the app identity", comments[1].Author)
	}
}
