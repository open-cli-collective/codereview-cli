package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const testGitHubWriteBodyLimit = 60_000

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
			requireJSONExact(t, mapWithoutKey(body, "comments"), map[string]any{
				"commit_id": "head-sha",
				"event":     "REQUEST_CHANGES",
				"body":      "review body",
			})
			comments, ok := body["comments"].([]any)
			if !ok || len(comments) != 1 {
				t.Fatalf("review comments = %#v, want one comment", body["comments"])
			}
			comment, ok := comments[0].(map[string]any)
			if !ok {
				t.Fatalf("review comment[0] = %#v, want object", comments[0])
			}
			requireJSONExact(t, comment, map[string]any{
				"body": "line body",
				"path": "dir/file.go",
				"side": "RIGHT",
				"line": float64(9),
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
		Comments: []gitprovider.InlineComment{{
			CommitSHA:   "head-sha",
			Body:        "line body",
			Path:        "dir/file.go",
			Side:        review.DiffSideRight,
			Line:        9,
			SubjectType: review.AnchorKindLine,
		}},
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
	badBundledReview := gitprovider.ReviewRequest{
		CommitSHA: "head-sha",
		Event:     review.ReviewEventComment,
		Body:      "body",
		Comments: []gitprovider.InlineComment{{
			CommitSHA:   "head-sha",
			Body:        "file body",
			Path:        "dir/file.go",
			SubjectType: review.AnchorKindFile,
		}},
	}
	if _, err := client.SubmitReview(context.Background(), ref, badBundledReview); !errors.Is(err, ErrValidation) {
		t.Fatalf("SubmitReview bundled file-level error = %v, want ErrValidation", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid inputs", requests)
	}
}

func TestRESTWritesAcceptMarkerBodiesAtUTF8ByteLimit(t *testing.T) {
	inlineBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit, marker.ActionKindInlineComment, "")
	issueBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit, marker.ActionKindRollupComment, marker.RollupOutcomeRequestChanges)
	reviewBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit, marker.ActionKindSubmitReview, "")
	bundledBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit, marker.ActionKindInlineComment, "")

	calls := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireJSONWrite(t, r)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Read request body: %v", err)
		}
		if len(raw) <= testGitHubWriteBodyLimit {
			t.Fatalf("serialized JSON body is %d bytes, want it to exceed the %d-byte content limit", len(raw), testGitHubWriteBodyLimit)
		}
		var payload struct {
			Body     string `json:"body"`
			Event    string `json:"event"`
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}
		switch r.URL.EscapedPath() {
		case "/repos/open%20cli/repo+name/pulls/42/comments":
			requirePreservedMarkerBody(t, payload.Body, inlineBody)
		case "/repos/open%20cli/repo+name/issues/42/comments":
			requirePreservedMarkerBody(t, payload.Body, issueBody)
		case "/repos/open%20cli/repo+name/pulls/42/reviews":
			requirePreservedMarkerBody(t, payload.Body, reviewBody)
			if payload.Event != "REQUEST_CHANGES" {
				t.Fatalf("review event = %q, want REQUEST_CHANGES", payload.Event)
			}
			if len(payload.Comments) != 1 {
				t.Fatalf("bundled review comments = %d, want 1", len(payload.Comments))
			}
			requirePreservedMarkerBody(t, payload.Comments[0].Body, bundledBody)
		default:
			t.Fatalf("unexpected write path %s", r.URL.String())
		}
		calls <- struct{}{}
		writeJSON(t, w, map[string]any{"id": 301})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	inline := validLineComment()
	inline.Body = inlineBody
	if _, err := client.PostInlineComment(context.Background(), testPRRef(), inline); err != nil {
		t.Fatalf("PostInlineComment at limit: %v", err)
	}
	if _, err := client.PostIssueComment(context.Background(), testPRRef(), issueBody); err != nil {
		t.Fatalf("PostIssueComment at limit: %v", err)
	}
	reviewComment := validLineComment()
	reviewComment.Body = bundledBody
	if _, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "head-sha",
		Event:     review.ReviewEventRequestChanges,
		Body:      reviewBody,
		Comments:  []gitprovider.InlineComment{reviewComment},
	}); err != nil {
		t.Fatalf("SubmitReview at limit: %v", err)
	}
	if got := len(calls); got != 3 {
		t.Fatalf("HTTP write calls = %d, want 3", got)
	}
}

func TestRESTWritesRejectMarkerBodiesOverUTF8ByteLimitBeforeRequest(t *testing.T) {
	inlineBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit+1, marker.ActionKindInlineComment, "")
	issueBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit+1, marker.ActionKindRollupComment, marker.RollupOutcomeRequestChanges)
	reviewBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit+1, marker.ActionKindSubmitReview, "")
	bundledBody := markerBodyAtUTF8Size(t, testGitHubWriteBodyLimit+1, marker.ActionKindInlineComment, "")

	calls := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls <- struct{}{}
		writeJSON(t, w, map[string]any{"id": 301})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	inline := validLineComment()
	inline.Body = inlineBody
	reviewComment := validLineComment()
	reviewComment.Body = bundledBody
	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "inline comment",
			call: func() error {
				_, err := client.PostInlineComment(context.Background(), testPRRef(), inline)
				return err
			},
		},
		{
			name: "issue comment",
			call: func() error {
				_, err := client.PostIssueComment(context.Background(), testPRRef(), issueBody)
				return err
			},
		},
		{
			name: "review body",
			call: func() error {
				_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
					CommitSHA: "head-sha",
					Event:     review.ReviewEventRequestChanges,
					Body:      reviewBody,
				})
				return err
			},
		},
		{
			name: "bundled review comment",
			call: func() error {
				_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
					CommitSHA: "head-sha",
					Event:     review.ReviewEventRequestChanges,
					Body:      "review body",
					Comments:  []gitprovider.InlineComment{reviewComment},
				})
				return err
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), "60001 UTF-8 bytes") || !strings.Contains(err.Error(), "60000") {
				t.Fatalf("error = %q, want actual and maximum body byte counts", err)
			}
			if strings.Contains(err.Error(), "Preserve this complete finding") {
				t.Fatalf("error exposed review body content: %q", err)
			}
		})
	}
	if got := len(calls); got != 0 {
		t.Fatalf("HTTP write calls = %d, want none for oversized bodies", got)
	}
}

func TestRESTWriteErrorTaxonomy(t *testing.T) {
	secret := "ghp_rest_write_taxonomy_no_leak_canary_0002" // #nosec G101 -- distinctive test canary, not a real token.
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
		{name: "non head-commit 422 with commit id", code: http.StatusUnprocessableEntity, body: `{"message":"commit_id does not match the expected format"}`, want: ErrValidation, not: gitprovider.ErrStaleSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireJSONWrite(t, r)
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body + secret))
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
			if leakErr := credstore.NoLeakAssertion([]byte(err.Error()), secret); leakErr != nil {
				t.Fatalf("error leaked REST write canary: %v", leakErr)
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

func markerBodyAtUTF8Size(t *testing.T, size int, kind string, outcome string) string {
	t.Helper()
	actionMarker, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "test-run",
		ActionID: "test-action",
		Kind:     kind,
		SHA:      strings.Repeat("a", 40),
		BaseSHA:  strings.Repeat("b", 40),
		Outcome:  outcome,
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	prefix := marker.RenderSkip() + "\n" + actionMarker + "\n\n## Verdict: REQUEST_CHANGES\n\n## Findings\n\n- [P1] Preserve this complete finding.\n\n"
	if len(prefix) > size {
		t.Fatalf("marker body prefix is %d bytes, exceeds requested %d-byte body", len(prefix), size)
	}
	paddingBytes := size - len(prefix)
	body := prefix + strings.Repeat("é", paddingBytes/2)
	if paddingBytes%2 == 1 {
		body += "x"
	}
	if got := len(body); got != size {
		t.Fatalf("body size = %d UTF-8 bytes, want %d", got, size)
	}
	if !strings.Contains(body, "é") {
		t.Fatal("body does not contain multibyte UTF-8 content")
	}
	return body
}

func requirePreservedMarkerBody(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("captured body size = %d UTF-8 bytes, want %d; body was truncated or changed", len(got), len(want))
	}
	if !strings.Contains(got, marker.RenderSkip()) || len(marker.FindActions(got)) != 1 {
		t.Fatal("captured body is missing its outbox markers")
	}
	if !strings.Contains(got, "## Verdict: REQUEST_CHANGES") || !strings.Contains(got, "[P1] Preserve this complete finding.") {
		t.Fatal("captured body is missing its verdict or finding")
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

func mapWithoutKey(input map[string]any, key string) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
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
