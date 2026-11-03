package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// statusSequenceServer answers each request with the next status in statuses,
// repeating the last one, and a file body on 200.
func statusSequenceServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(requests.Add(1))
		status := statuses[min(n, len(statuses))-1]
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte("package main\n"))
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func retryTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})
	client.readRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	return client
}

func TestGetFileAtRefRetriesTransientServerError(t *testing.T) {
	server, requests := statusSequenceServer(t, http.StatusInternalServerError, http.StatusGatewayTimeout, http.StatusOK)
	client := retryTestClient(t, server)

	contents, err := client.GetFileAtRef(context.Background(), testPRRef(), "head-sha", "dir/file.go")
	if err != nil {
		t.Fatalf("GetFileAtRef: %v", err)
	}
	if string(contents) != "package main\n" {
		t.Fatalf("contents = %q", contents)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestGetFileAtRefGivesUpAfterRetries(t *testing.T) {
	server, requests := statusSequenceServer(t, http.StatusBadGateway)
	client := retryTestClient(t, server)

	_, err := client.GetFileAtRef(context.Background(), testPRRef(), "head-sha", "dir/file.go")
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3 (one try and two retries)", got)
	}
}

func TestGetFileAtRefDoesNotRetryNonTransientStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusUnauthorized} {
		server, requests := statusSequenceServer(t, status, http.StatusOK)
		client := retryTestClient(t, server)

		if _, err := client.GetFileAtRef(context.Background(), testPRRef(), "head-sha", "dir/file.go"); err == nil {
			t.Fatalf("status %d: err = nil, want an error", status)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("status %d: requests = %d, want 1", status, got)
		}
	}
}

func TestRESTWriteIsNotRetried(t *testing.T) {
	server, requests := statusSequenceServer(t, http.StatusInternalServerError, http.StatusOK)
	client := retryTestClient(t, server)

	_, _, err := client.doREST(context.Background(), gitprovider.OperationGetFileAtRef, http.MethodPatch, server.URL+"/user/repository_invitations/1", acceptJSON, nil)
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestReadRetryStopsWhenContextIsCanceled(t *testing.T) {
	server, requests := statusSequenceServer(t, http.StatusServiceUnavailable)
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})
	client.readRetryDelays = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	start := time.Now()
	_, err := client.GetFileAtRef(ctx, testPRRef(), "head-sha", "dir/file.go")
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("err = %v, want the last ErrRetryable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("elapsed = %s, want the wait to end with the context", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
