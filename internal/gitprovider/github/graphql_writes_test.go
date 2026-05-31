package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestGraphQLWriteMethodsMapRequests(t *testing.T) {
	ref := testPRRef()
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		req := readGraphQLRequestAtEndpoint(t, r)
		switch call {
		case 1:
			if req.Variables["threadID"] != "thread-1" || req.Variables["body"] != "reply body" {
				t.Fatalf("reply variables = %#v", req.Variables)
			}
			if containsInterpolatedExpression(req.Query, "reply body") {
				t.Fatalf("query interpolated reply body: %s", req.Query)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"addPullRequestReviewThreadReply": map[string]any{
				"comment": map[string]any{"id": "reply-comment-node", "databaseId": 401},
			}}})
		case 2:
			if req.Variables["threadID"] != "thread-1" {
				t.Fatalf("resolve variables = %#v", req.Variables)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
				"thread": map[string]any{"id": "thread-1", "isResolved": true},
			}}})
		default:
			t.Fatalf("unexpected GraphQL write call %d", call)
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	commentID, err := client.ReplyToThread(context.Background(), ref, "thread-1", "reply body")
	if err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}
	if commentID != "reply-comment-node" {
		t.Fatalf("reply comment ID = %q, want reply-comment-node", commentID)
	}
	if err := client.ResolveThread(context.Background(), ref, "thread-1"); err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}
}

func TestGraphQLWritesValidateBeforeRequest(t *testing.T) {
	ref := testPRRef()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	if _, err := client.ReplyToThread(context.Background(), gitprovider.PRRef{Host: "github.example.com", Owner: "o", Repo: "r", Number: 1}, "thread-1", "body"); !errors.Is(err, ErrValidation) {
		t.Fatalf("ReplyToThread invalid ref error = %v, want ErrValidation", err)
	}
	if err := client.ResolveThread(context.Background(), gitprovider.PRRef{Host: "github.example.com", Owner: "o", Repo: "r", Number: 1}, "thread-1"); !errors.Is(err, ErrValidation) {
		t.Fatalf("ResolveThread invalid ref error = %v, want ErrValidation", err)
	}
	if _, err := client.ReplyToThread(context.Background(), ref, "", "body"); err == nil {
		t.Fatal("ReplyToThread empty thread error = nil")
	}
	if _, err := client.ReplyToThread(context.Background(), ref, "thread-1", " "); err == nil {
		t.Fatal("ReplyToThread blank body error = nil")
	}
	if err := client.ResolveThread(context.Background(), ref, " "); err == nil {
		t.Fatal("ResolveThread blank thread error = nil")
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid inputs", requests)
	}
}

func TestResolveThreadAlreadyResolvedIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		writeJSON(t, w, map[string]any{"errors": []graphQLError{{
			Type:    "UNPROCESSABLE",
			Message: "Review thread is already resolved",
		}}})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	if err := client.ResolveThread(context.Background(), testPRRef(), "thread-1"); err != nil {
		t.Fatalf("ResolveThread already resolved error = %v, want success", err)
	}
}

func TestAlreadyResolvedGraphQLErrorIsResolveThreadLocal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		writeJSON(t, w, map[string]any{"errors": []graphQLError{{
			Type:    "UNPROCESSABLE",
			Message: "Review thread is already resolved",
		}}})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.ReplyToThread(context.Background(), testPRRef(), "thread-1", "body")
	if !errors.Is(err, ErrUnhandledGraphQL) {
		t.Fatalf("ReplyToThread already-resolved error = %v, want shared classifier fallback", err)
	}
}
