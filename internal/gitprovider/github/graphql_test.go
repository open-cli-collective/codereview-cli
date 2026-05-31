package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestListTreeAtRefUsesGraphQLVariables(t *testing.T) {
	ref := testPRRef()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readGraphQLRequestAtEndpoint(t, r)
		if req.Variables["owner"] != ref.Owner || req.Variables["repo"] != ref.Repo {
			t.Fatalf("variables = %#v, want owner/repo", req.Variables)
		}
		if req.Variables["expression"] != "base-sha:.codereview/agents" {
			t.Fatalf("expression = %#v", req.Variables["expression"])
		}
		if containsInterpolatedExpression(req.Query, "base-sha:.codereview/agents") {
			t.Fatalf("query interpolated expression: %s", req.Query)
		}
		writeJSON(t, w, map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": map[string]any{
						"entries": []map[string]any{
							{"name": "security.yml", "path": ".codereview/agents/security.yml", "type": "blob", "oid": "blob-sha"},
							{"name": "backend", "path": ".codereview/agents/backend", "type": "tree", "oid": "tree-sha"},
							{"name": "shared", "path": "vendor/shared", "type": "commit", "oid": "submodule-sha"},
						},
					},
				},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	entries, err := client.ListTreeAtRef(context.Background(), ref, "base-sha", ".codereview/agents")
	if err != nil {
		t.Fatalf("ListTreeAtRef: %v", err)
	}
	want := []gitprovider.TreeEntry{
		{Path: ".codereview/agents/security.yml", Type: "blob", SHA: "blob-sha"},
		{Path: ".codereview/agents/backend", Type: "tree", SHA: "tree-sha"},
		{Path: "vendor/shared", Type: "commit", SHA: "submodule-sha"},
	}
	if len(entries) != len(want) || entries[0] != want[0] || entries[1] != want[1] || entries[2] != want[2] {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
}

func TestListInlineThreadsPaginatesThreadsAndNestedComments(t *testing.T) {
	ref := testPRRef()
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		req := readGraphQLRequestAtEndpoint(t, r)
		switch call {
		case 1:
			if req.Variables["threadAfter"] != nil {
				t.Fatalf("first threadAfter = %#v, want nil", req.Variables["threadAfter"])
			}
			writeJSON(t, w, threadPageResponse("cursor-thread-2", true, []map[string]any{
				threadNodeResponse("thread-1", true, "main.go", 10, "RIGHT", true, "cursor-comment-2", []map[string]any{
					commentNodeResponse("comment-1", 101, "first", "reviewer", "head-sha", "main.go", 10, "RIGHT", now),
				}),
			}))
		case 2:
			if req.Variables["threadID"] != "thread-1" || req.Variables["commentAfter"] != "cursor-comment-2" {
				t.Fatalf("nested comment variables = %#v", req.Variables)
			}
			writeJSON(t, w, nestedCommentPageResponse(false, "", []map[string]any{
				commentNodeResponse("comment-2", 102, "second", "reviewer", "head-sha", "main.go", 11, "RIGHT", now.Add(time.Minute)),
			}))
		case 3:
			if req.Variables["threadAfter"] != "cursor-thread-2" {
				t.Fatalf("second threadAfter = %#v", req.Variables["threadAfter"])
			}
			writeJSON(t, w, threadPageResponse("", false, []map[string]any{
				threadNodeResponse("thread-2", false, "other.go", 5, "LEFT", false, "", []map[string]any{
					commentNodeResponse("comment-3", 103, "third", "reviewer2", "base-sha", "other.go", 5, "LEFT", now),
				}),
			}))
		default:
			t.Fatalf("unexpected GraphQL call %d", call)
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	threads, err := client.ListInlineThreads(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("threads len = %d, want 2: %#v", len(threads), threads)
	}
	if threads[0].ID != "thread-1" || !threads[0].Resolved || len(threads[0].Comments) != 2 {
		t.Fatalf("first thread = %#v, want resolved with two comments", threads[0])
	}
	if threads[0].Side != review.DiffSideRight || threads[0].SubjectType != review.AnchorKindLine || threads[0].CommitSHA != "head-sha" {
		t.Fatalf("first thread mapped fields = %#v", threads[0])
	}
	if threads[0].Comments[1].ID != "comment-2" || threads[0].Comments[1].Author.Login != "reviewer" {
		t.Fatalf("nested comment not appended: %#v", threads[0].Comments)
	}
	if threads[0].Comments[1].Author.ID != "1102" || threads[0].Comments[1].Author.DisplayName != "reviewer name" {
		t.Fatalf("nested comment author = %#v, want id and display name", threads[0].Comments[1].Author)
	}
	if threads[1].Side != review.DiffSideLeft || threads[1].Comments[0].CommitSHA != "base-sha" {
		t.Fatalf("second thread = %#v", threads[1])
	}
}

func TestGraphQLErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  graphQLError
		want error
		not  error
	}{
		{name: "unauthenticated type", err: graphQLError{Type: "UNAUTHENTICATED", Message: "bad credentials"}, want: gitprovider.ErrAuth},
		{name: "authentication message", err: graphQLError{Message: "authentication required"}, want: gitprovider.ErrAuth},
		{name: "forbidden", err: graphQLError{Type: "FORBIDDEN", Message: "forbidden"}, want: gitprovider.ErrPermission},
		{name: "not found", err: graphQLError{Type: "NOT_FOUND", Message: "not found"}, want: gitprovider.ErrNotFound},
		{name: "rate limited", err: graphQLError{Type: "RATE_LIMITED", Message: "rate limit"}, want: gitprovider.ErrRetryable},
		{name: "extensions type", err: graphQLError{Message: "missing", Extensions: map[string]any{"type": "NOT_FOUND"}}, want: gitprovider.ErrNotFound},
		{name: "extensions code", err: graphQLError{Message: "limited", Extensions: map[string]any{"code": "RATE_LIMITED"}}, want: gitprovider.ErrRetryable},
		{name: "internal", err: graphQLError{Type: "INTERNAL", Message: "server failed"}, want: gitprovider.ErrRetryable},
		{name: "timeout", err: graphQLError{Type: "TIMEOUT", Message: "timed out"}, want: gitprovider.ErrRetryable},
		{name: "fallback", err: graphQLError{Type: "SOMETHING_ELSE", Message: "bad query"}, want: ErrUnhandledGraphQL, not: gitprovider.ErrRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = readGraphQLRequestAtEndpoint(t, r)
				writeJSON(t, w, map[string]any{"errors": []graphQLError{tt.err}})
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})
			_, err := client.ListTreeAtRef(context.Background(), testPRRef(), "base", ".codereview")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if tt.not != nil && errors.Is(err, tt.not) {
				t.Fatalf("error = %v, did not want %v", err, tt.not)
			}
		})
	}
}

func TestListInlineThreadsRejectsUnknownDiffSide(t *testing.T) {
	ref := testPRRef()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		writeJSON(t, w, threadPageResponse("", false, []map[string]any{
			threadNodeResponse("thread-1", false, "main.go", 10, "SIDEWAYS", false, "", nil),
		}))
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.ListInlineThreads(context.Background(), ref)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ListInlineThreads unknown side error = %v, want ErrValidation", err)
	}
}

func TestListTreeAtRefMapsMissingObjectToNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readGraphQLRequestAtEndpoint(t, r)
		writeJSON(t, w, map[string]any{
			"data": map[string]any{
				"repository": map[string]any{"object": nil},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.ListTreeAtRef(context.Background(), testPRRef(), "base", ".codereview")
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("ListTreeAtRef missing object error = %v, want ErrNotFound", err)
	}
}

func readGraphQLRequestAtEndpoint(t *testing.T, r *http.Request) graphQLRequest {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Fatalf("GraphQL method = %s, want POST", r.Method)
	}
	if r.URL.Path != "/graphql" {
		t.Fatalf("GraphQL path = %q, want /graphql", r.URL.Path)
	}
	return readGraphQLRequest(t, r)
}

func containsInterpolatedExpression(query, expression string) bool {
	return len(query) > 0 && len(expression) > 0 && strings.Contains(query, expression)
}

func threadPageResponse(endCursor string, hasNext bool, nodes []map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
		"nodes":    nodes,
	}}}}}
}

func nestedCommentPageResponse(hasNext bool, endCursor string, nodes []map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"node": map[string]any{"comments": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
		"nodes":    nodes,
	}}}}
}

func threadNodeResponse(id string, resolved bool, path string, line int, side string, commentsHasNext bool, commentsCursor string, comments []map[string]any) map[string]any {
	return map[string]any{
		"id":         id,
		"isResolved": resolved,
		"path":       path,
		"line":       line,
		"diffSide":   side,
		"comments": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": commentsHasNext, "endCursor": commentsCursor},
			"nodes":    comments,
		},
	}
}

func commentNodeResponse(id string, databaseID int, body, author, commit, path string, line int, side string, at time.Time) map[string]any {
	return map[string]any{
		"id":         id,
		"databaseId": databaseID,
		"body":       body,
		"author":     map[string]any{"login": author, "id": databaseID + 1000, "name": author + " name"},
		"commit":     map[string]any{"oid": commit},
		"path":       path,
		"line":       line,
		"diffSide":   side,
		"url":        "https://github.com/comment/" + id,
		"createdAt":  at.Format(time.RFC3339),
		"updatedAt":  at.Add(time.Second).Format(time.RFC3339),
	}
}
