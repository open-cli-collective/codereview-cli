package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestRESTWriteMethodsMapRequests(t *testing.T) {
	ref := testPRRef()
	commentCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireJSONWrite(t, r)
		switch r.URL.EscapedPath() {
		case "/repos/open%20cli/repo+name/pulls/42/comments":
			commentCalls++
			body := readJSONMap(t, r)
			switch commentCalls {
			case 1:
				requireJSONExact(t, body, map[string]any{
					"body":      "line body",
					"commit_id": "head-sha",
					"path":      "dir/file.go",
					"side":      "RIGHT",
					"line":      float64(9),
				})
				writeJSON(t, w, map[string]any{"id": 101})
			case 2:
				requireJSONExact(t, body, map[string]any{
					"body":         "file body",
					"commit_id":    "head-sha",
					"path":         "dir/file.go",
					"subject_type": "file",
				})
				writeJSON(t, w, map[string]any{"id": 102})
			default:
				t.Fatalf("unexpected pull comment call %d", commentCalls)
			}
		case "/repos/open%20cli/repo+name/issues/42/comments":
			body := readJSONMap(t, r)
			requireJSONExact(t, body, map[string]any{"body": "rollup body"})
			writeJSON(t, w, map[string]any{"id": 201})
		case "/repos/open%20cli/repo+name/pulls/42/reviews":
			body := readJSONMap(t, r)
			requireJSONExact(t, body, map[string]any{
				"commit_id": "head-sha",
				"event":     "REQUEST_CHANGES",
				"body":      "review body",
			})
			writeJSON(t, w, map[string]any{"id": 301})
		default:
			t.Fatalf("unexpected write path %s", r.URL.String())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	lineID, err := client.PostInlineComment(context.Background(), ref, gitprovider.InlineComment{
		CommitSHA:   "head-sha",
		Body:        "line body",
		Path:        "dir/file.go",
		Side:        review.DiffSideRight,
		Line:        9,
		SubjectType: review.AnchorKindLine,
	})
	if err != nil {
		t.Fatalf("PostInlineComment line: %v", err)
	}
	if lineID != "101" {
		t.Fatalf("line comment ID = %q, want 101", lineID)
	}

	fileID, err := client.PostInlineComment(context.Background(), ref, gitprovider.InlineComment{
		CommitSHA:   "head-sha",
		Body:        "file body",
		Path:        "dir/file.go",
		SubjectType: review.AnchorKindFile,
	})
	if err != nil {
		t.Fatalf("PostInlineComment file: %v", err)
	}
	if fileID != "102" {
		t.Fatalf("file comment ID = %q, want 102", fileID)
	}

	issueID, err := client.PostIssueComment(context.Background(), ref, "rollup body")
	if err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if issueID != "201" {
		t.Fatalf("issue comment ID = %q, want 201", issueID)
	}

	reviewID, err := client.SubmitReview(context.Background(), ref, gitprovider.ReviewRequest{
		CommitSHA: "head-sha",
		Event:     review.ReviewEventRequestChanges,
		Body:      "review body",
	})
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if reviewID != "301" {
		t.Fatalf("review ID = %q, want 301", reviewID)
	}
}

func TestRESTWritesValidateBeforeRequest(t *testing.T) {
	ref := testPRRef()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	if _, err := client.PostInlineComment(context.Background(), gitprovider.PRRef{Host: "github.example.com", Owner: "o", Repo: "r", Number: 1}, validLineComment()); !errors.Is(err, ErrValidation) {
		t.Fatalf("PostInlineComment invalid ref error = %v, want ErrValidation", err)
	}
	invalidInline := validLineComment()
	invalidInline.CommitSHA = ""
	if _, err := client.PostInlineComment(context.Background(), ref, invalidInline); err == nil {
		t.Fatal("PostInlineComment invalid payload error = nil")
	}
	if _, err := client.PostIssueComment(context.Background(), ref, "  "); err == nil {
		t.Fatal("PostIssueComment blank body error = nil")
	}
	invalidReview := gitprovider.ReviewRequest{CommitSHA: "head-sha", Event: "bad", Body: "body"}
	if _, err := client.SubmitReview(context.Background(), ref, invalidReview); err == nil {
		t.Fatal("SubmitReview invalid payload error = nil")
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid inputs", requests)
	}
}

func TestRESTWriteErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want error
		not  error
	}{
		{name: "auth", code: http.StatusUnauthorized, body: `{"message":"bad credentials"}`, want: gitprovider.ErrAuth},
		{name: "permission", code: http.StatusForbidden, body: `{"message":"forbidden"}`, want: gitprovider.ErrPermission},
		{name: "not found", code: http.StatusNotFound, body: `{"message":"missing"}`, want: gitprovider.ErrNotFound},
		{name: "conflict", code: http.StatusConflict, body: `{"message":"already exists"}`, want: gitprovider.ErrConflict},
		{name: "retryable", code: http.StatusTooManyRequests, body: `{"message":"rate limited"}`, want: gitprovider.ErrRetryable},
		{name: "stale sha", code: http.StatusUnprocessableEntity, body: `{"message":"commit_id head-sha is not the head commit for this pull request"}`, want: gitprovider.ErrStaleSHA},
		{name: "generic validation", code: http.StatusUnprocessableEntity, body: `{"message":"line is not part of the diff"}`, want: ErrValidation, not: gitprovider.ErrStaleSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireJSONWrite(t, r)
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

			_, err := client.PostInlineComment(context.Background(), testPRRef(), validLineComment())
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if tt.not != nil && errors.Is(err, tt.not) {
				t.Fatalf("error = %v, did not want %v", err, tt.not)
			}
		})
	}
}

func TestSubmitReviewStaleSHA422(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireJSONWrite(t, r)
		if r.URL.EscapedPath() != "/repos/open%20cli/repo+name/pulls/42/reviews" {
			t.Fatalf("path = %s, want reviews path", r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"commit_id head-sha is not the head commit for this pull request"}`))
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "head-sha",
		Event:     review.ReviewEventComment,
		Body:      "review body",
	})
	if !errors.Is(err, gitprovider.ErrStaleSHA) {
		t.Fatalf("SubmitReview error = %v, want ErrStaleSHA", err)
	}
}

func TestPostIssueCommentStaleLooking422IsValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireJSONWrite(t, r)
		if r.URL.EscapedPath() != "/repos/open%20cli/repo+name/issues/42/comments" {
			t.Fatalf("path = %s, want issue comments path", r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"commit_id head-sha is not the head commit for this pull request"}`))
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.PostIssueComment(context.Background(), testPRRef(), "rollup body")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("PostIssueComment error = %v, want ErrValidation", err)
	}
	if errors.Is(err, gitprovider.ErrStaleSHA) {
		t.Fatalf("PostIssueComment error = %v, did not want ErrStaleSHA", err)
	}
}

func TestRESTWriteSuccessfulResponsesRequireIDs(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
	}{
		{
			name: "inline comment",
			call: func(client *Client) error {
				_, err := client.PostInlineComment(context.Background(), testPRRef(), validLineComment())
				return err
			},
		},
		{
			name: "issue comment",
			call: func(client *Client) error {
				_, err := client.PostIssueComment(context.Background(), testPRRef(), "rollup body")
				return err
			},
		},
		{
			name: "review",
			call: func(client *Client) error {
				_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
					CommitSHA: "head-sha",
					Event:     review.ReviewEventComment,
					Body:      "review body",
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireJSONWrite(t, r)
				writeJSON(t, w, map[string]any{"id": 0})
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

			err := tt.call(client)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestSubmitReviewMapsEvents(t *testing.T) {
	tests := []struct {
		name  string
		event review.ReviewEvent
		want  string
	}{
		{name: "approve", event: review.ReviewEventApprove, want: "APPROVE"},
		{name: "comment", event: review.ReviewEventComment, want: "COMMENT"},
		{name: "request changes", event: review.ReviewEventRequestChanges, want: "REQUEST_CHANGES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireJSONWrite(t, r)
				body := readJSONMap(t, r)
				if body["event"] != tt.want {
					t.Fatalf("event = %#v, want %q; full body=%#v", body["event"], tt.want, body)
				}
				writeJSON(t, w, map[string]any{"id": 301})
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

			id, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
				CommitSHA: "head-sha",
				Event:     tt.event,
				Body:      "review body",
			})
			if err != nil {
				t.Fatalf("SubmitReview: %v", err)
			}
			if id != "301" {
				t.Fatalf("review ID = %q, want 301", id)
			}
		})
	}
}

func validLineComment() gitprovider.InlineComment {
	return gitprovider.InlineComment{
		CommitSHA:   "head-sha",
		Body:        "line body",
		Path:        "dir/file.go",
		Side:        review.DiffSideRight,
		Line:        9,
		SubjectType: review.AnchorKindLine,
	}
}

func requireJSONWrite(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", r.Method)
	}
	if r.Header.Get("Authorization") != "Bearer token" {
		t.Fatalf("Authorization = %q, want bearer token", r.Header.Get("Authorization"))
	}
	if r.Header.Get("Accept") != acceptJSON {
		t.Fatalf("Accept = %q, want %q", r.Header.Get("Accept"), acceptJSON)
	}
	if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func readJSONMap(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("Decode request body: %v", err)
	}
	return body
}

func requireJSONExact(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("body = %#v, want exactly %#v", got, want)
	}
	for key, wantValue := range want {
		if gotValue, ok := got[key]; !ok || gotValue != wantValue {
			t.Fatalf("body[%q] = %#v (present=%v), want %#v; full body=%#v", key, gotValue, ok, wantValue, got)
		}
	}
}
