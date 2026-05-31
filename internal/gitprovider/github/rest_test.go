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

func TestRESTReadMethodsMapResponses(t *testing.T) {
	ref := testPRRef()
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	var diffAccept, rawAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/pulls/42" && r.Header.Get("Accept") == acceptDiff:
			diffAccept = r.Header.Get("Accept")
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go"))
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/pulls/42":
			writeJSON(t, w, prResponse{
				Title:   "Add adapter",
				HTMLURL: "https://github.com/open-cli/repo/pull/42",
				State:   "closed",
				Merged:  true,
				User:    userResponse{Login: "author", ID: 1, Name: "Author"},
				Head: branchResponse{Ref: "feature", SHA: "head-sha", Repo: &repoResponse{
					Name:  "fork",
					Owner: userResponse{Login: "author"},
				}},
				Base: branchResponse{Ref: "main", SHA: "base-sha", Repo: &repoResponse{
					Name:  "repo+name",
					Owner: userResponse{Login: "open cli"},
				}},
			})
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/contents/dir/file%20name.go":
			rawAccept = r.Header.Get("Accept")
			if got := r.URL.Query().Get("ref"); got != "refs/heads/feature branch" {
				t.Fatalf("contents ref query = %q", got)
			}
			_, _ = w.Write([]byte("package main\n"))
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/pulls/42/reviews":
			writeJSON(t, w, []reviewResponse{{
				ID:          100,
				Body:        "review body",
				User:        userResponse{Login: "reviewer", ID: 2, Name: "Reviewer"},
				State:       "APPROVED",
				CommitID:    "head-sha",
				HTMLURL:     "https://github.com/review",
				SubmittedAt: now,
			}})
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/issues/42/comments":
			writeJSON(t, w, []issueCommentResponse{{
				ID:        200,
				Body:      "comment body",
				User:      userResponse{Login: "commenter", ID: 3, Name: "Commenter"},
				HTMLURL:   "https://github.com/comment",
				CreatedAt: now,
				UpdatedAt: now.Add(time.Minute),
			}})
		default:
			t.Fatalf("unexpected request path %s", r.URL.String())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	pr, err := client.GetPR(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.State != gitprovider.PRStateMerged || pr.Head.Owner != "author" || pr.Base.SHA != "base-sha" {
		t.Fatalf("GetPR = %#v, want merged PR with branch repo identity", pr)
	}
	diff, err := client.GetDiff(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if !strings.Contains(diff.Raw, "diff --git") || diffAccept != acceptDiff {
		t.Fatalf("GetDiff = %#v accept=%q", diff, diffAccept)
	}
	contents, err := client.GetFileAtRef(context.Background(), ref, "refs/heads/feature branch", "dir/file name.go")
	if err != nil {
		t.Fatalf("GetFileAtRef: %v", err)
	}
	if string(contents) != "package main\n" || rawAccept != acceptRaw {
		t.Fatalf("GetFileAtRef = %q accept=%q", contents, rawAccept)
	}
	reviews, err := client.ListReviews(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].State != gitprovider.ReviewStateApproved || reviews[0].Event != review.ReviewEventApprove {
		t.Fatalf("ListReviews = %#v", reviews)
	}
	comments, err := client.ListIssueComments(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "200" || comments[0].Author.Login != "commenter" {
		t.Fatalf("ListIssueComments = %#v", comments)
	}
}

func TestRESTPagination(t *testing.T) {
	ref := testPRRef()
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.String()]++
		switch {
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/pulls/42/reviews" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", `<`+requestHostURL(r)+`/repos/open%20cli/repo+name/pulls/42/reviews?page=2>; rel="next"`)
			writeJSON(t, w, []reviewResponse{{ID: 1, State: "COMMENTED"}})
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/pulls/42/reviews" && r.URL.Query().Get("page") == "2":
			writeJSON(t, w, []reviewResponse{{ID: 2, State: "CHANGES_REQUESTED"}})
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/issues/42/comments" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", `<`+requestHostURL(r)+`/repos/open%20cli/repo+name/issues/42/comments?page=2>; rel="next"`)
			writeJSON(t, w, []issueCommentResponse{{ID: 3}})
		case r.URL.EscapedPath() == "/repos/open%20cli/repo+name/issues/42/comments" && r.URL.Query().Get("page") == "2":
			writeJSON(t, w, []issueCommentResponse{{ID: 4}})
		default:
			t.Fatalf("unexpected paginated request %s", r.URL.String())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	reviews, err := client.ListReviews(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 2 || reviews[1].State != gitprovider.ReviewStateChangesRequested {
		t.Fatalf("ListReviews = %#v, want two pages", reviews)
	}
	comments, err := client.ListIssueComments(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 2 || comments[1].ID != "4" {
		t.Fatalf("ListIssueComments = %#v, want two pages", comments)
	}
	if len(requests) != 4 {
		t.Fatalf("requests = %#v, want four paginated requests", requests)
	}
}

func TestRESTPaginationRejectsOffHostNextLink(t *testing.T) {
	ref := testPRRef()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.EscapedPath() != "/repos/open%20cli/repo+name/pulls/42/reviews" {
			t.Fatalf("unexpected request %s", r.URL.String())
		}
		w.Header().Set("Link", `<https://evil.example.com/repos/open%20cli/repo+name/pulls/42/reviews?page=2>; rel="next"`)
		writeJSON(t, w, []reviewResponse{{ID: 1, State: "COMMENTED"}})
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.ListReviews(context.Background(), ref)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ListReviews off-host next error = %v, want ErrValidation", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want only first-page request", requests)
	}
}

func TestHTTPErrorTaxonomy(t *testing.T) {
	tests := []struct {
		status int
		want   error
		not    error
	}{
		{status: http.StatusUnauthorized, want: gitprovider.ErrAuth},
		{status: http.StatusForbidden, want: gitprovider.ErrPermission},
		{status: http.StatusNotFound, want: gitprovider.ErrNotFound},
		{status: http.StatusConflict, want: gitprovider.ErrConflict},
		{status: http.StatusUnprocessableEntity, want: ErrValidation, not: gitprovider.ErrRetryable},
		{status: http.StatusTooManyRequests, want: gitprovider.ErrRetryable},
		{status: http.StatusInternalServerError, want: gitprovider.ErrRetryable},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"message":"failed"}`))
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})
			_, err := client.WhoAmI(context.Background(), gitprovider.Credential{Type: credentialTypePAT, Token: "token"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if tt.not != nil && errors.Is(err, tt.not) {
				t.Fatalf("error = %v, did not want %v", err, tt.not)
			}
		})
	}
}

func requestHostURL(r *http.Request) string {
	return "http://" + r.Host
}
