package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestPostIssueCommentPostsNote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/notes" {
			t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
		}
		var request noteRequest
		decodeJSON(t, r.Body, &request)
		if request.Body != "rollup body" {
			t.Fatalf("body = %q, want rollup body", request.Body)
		}
		writeJSON(t, w, noteResponse{ID: 61})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	id, err := client.PostIssueComment(context.Background(), testPRRef(), "rollup body")
	if err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if id != gitprovider.CommentID("61") {
		t.Fatalf("id = %q, want 61", id)
	}
}

func submitReviewServer(t *testing.T, headSHA string, approveStatus, unapproveStatus int, calls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42":
			*calls = append(*calls, "get")
			writeJSON(t, w, mergeRequestResponse{
				State: "opened", SourceBranch: "feature", TargetBranch: "main",
				DiffRefs: diffRefsResponse{BaseSHA: "basesha", StartSHA: "startsha", HeadSHA: headSHA},
			})
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/approve":
			var request approveRequest
			decodeJSON(t, r.Body, &request)
			*calls = append(*calls, "approve:"+request.SHA)
			if approveStatus != http.StatusCreated {
				w.WriteHeader(approveStatus)
				return
			}
			writeJSON(t, w, map[string]any{"state": "approved"})
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/unapprove":
			*calls = append(*calls, "unapprove")
			if unapproveStatus != http.StatusCreated {
				w.WriteHeader(unapproveStatus)
				return
			}
			writeJSON(t, w, map[string]any{"state": "unapproved"})
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/notes":
			var request noteRequest
			decodeJSON(t, r.Body, &request)
			*calls = append(*calls, "note:"+request.Body)
			writeJSON(t, w, noteResponse{ID: 88})
		default:
			t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
		}
	}))
}

func TestSubmitReviewApproveAppliesApprovalThenPostsSummaryNote(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "headsha", http.StatusCreated, http.StatusCreated, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	id, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventApprove,
		Body:      "review summary",
	})
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if id != gitprovider.ReviewID("88") {
		t.Fatalf("id = %q, want 88", id)
	}
	if !reflect.DeepEqual(calls, []string{"get", "approve:headsha", "note:review summary"}) {
		t.Fatalf("calls = %#v, want approval before summary note", calls)
	}
}

func TestSubmitReviewRequestChangesRevokesApprovalAndIgnoresMissingApproval(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "headsha", http.StatusCreated, http.StatusNotFound, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	if _, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventRequestChanges,
		Body:      "needs work",
	}); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"get", "unapprove", "note:needs work"}) {
		t.Fatalf("calls = %#v, want unapprove then note", calls)
	}
}

func TestSubmitReviewCommentOnlyPostsNote(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "headsha", http.StatusCreated, http.StatusCreated, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	if _, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventComment,
		Body:      "just a comment",
	}); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"get", "note:just a comment"}) {
		t.Fatalf("calls = %#v, want note only", calls)
	}
}

func TestSubmitReviewRejectsStaleCommitBeforeWriting(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "newer-head", http.StatusCreated, http.StatusCreated, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "stale-head",
		Event:     review.ReviewEventApprove,
		Body:      "review summary",
	})
	if !errors.Is(err, gitprovider.ErrStaleSHA) {
		t.Fatalf("SubmitReview error = %v, want ErrStaleSHA", err)
	}
	if !reflect.DeepEqual(calls, []string{"get"}) {
		t.Fatalf("calls = %#v, want no writes after stale detection", calls)
	}
}

func TestSubmitReviewMapsApproveConflictToStaleSHA(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "headsha", http.StatusConflict, http.StatusCreated, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventApprove,
		Body:      "review summary",
	})
	if !errors.Is(err, gitprovider.ErrStaleSHA) {
		t.Fatalf("SubmitReview error = %v, want ErrStaleSHA", err)
	}
}

func TestSubmitReviewMapsApproveUnauthorizedToPermission(t *testing.T) {
	var calls []string
	server := submitReviewServer(t, "headsha", http.StatusUnauthorized, http.StatusCreated, &calls)
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventApprove,
		Body:      "review summary",
	})
	if !errors.Is(err, gitprovider.ErrPermission) {
		t.Fatalf("SubmitReview error = %v, want ErrPermission", err)
	}
}

func TestSubmitReviewRejectsBundledComments(t *testing.T) {
	client := mustClient(t, Options{Host: "gitlab.example.com"})
	_, err := client.SubmitReview(context.Background(), testPRRef(), gitprovider.ReviewRequest{
		CommitSHA: "headsha",
		Event:     review.ReviewEventComment,
		Body:      "summary",
		Comments: []gitprovider.InlineComment{{
			CommitSHA: "headsha", Body: "inline", Path: "main.go",
			Side: review.DiffSideRight, Line: 2, SubjectType: review.AnchorKindLine,
		}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("SubmitReview error = %v, want ErrValidation for bundled comments", err)
	}
}

func TestReviewAuthorityMapsAccessLevels(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		accessLevel  int
		wantEligible bool
		wantName     string
	}{
		{name: "developer eligible", status: http.StatusOK, accessLevel: 30, wantEligible: true, wantName: "developer"},
		{name: "maintainer eligible", status: http.StatusOK, accessLevel: 40, wantEligible: true, wantName: "maintainer"},
		{name: "reporter ineligible", status: http.StatusOK, accessLevel: 20, wantEligible: false, wantName: "reporter"},
		{name: "non member", status: http.StatusNotFound, wantEligible: false, wantName: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/members/all/7" {
					t.Fatalf("path = %q, want members lookup", r.URL.EscapedPath())
				}
				if tt.status != http.StatusOK {
					w.WriteHeader(tt.status)
					return
				}
				writeJSON(t, w, memberResponse{AccessLevel: tt.accessLevel})
			}))
			defer server.Close()
			client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
			authority, err := client.ReviewAuthority(context.Background(), testPRRef(), gitprovider.Identity{Login: "review-bot", ID: "7"})
			if err != nil {
				t.Fatalf("ReviewAuthority: %v", err)
			}
			if authority.Eligible != tt.wantEligible || authority.Permission != tt.wantName {
				t.Fatalf("authority = %#v, want eligible=%v permission=%q", authority, tt.wantEligible, tt.wantName)
			}
		})
	}
}

func TestReviewAuthorityRequiresIdentityID(t *testing.T) {
	client := mustClient(t, Options{Host: "gitlab.example.com"})
	_, err := client.ReviewAuthority(context.Background(), testPRRef(), gitprovider.Identity{Login: "review-bot"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ReviewAuthority error = %v, want ErrValidation", err)
	}
}
