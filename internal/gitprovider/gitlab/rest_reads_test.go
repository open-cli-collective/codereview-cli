package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestWhoAmIMapsUserAndUsesSuppliedToken(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/user" {
			t.Fatalf("path = %q, want /user", r.URL.EscapedPath())
		}
		authorization = r.Header.Get("Authorization")
		writeJSON(t, w, userResponse{Username: "review-bot", ID: 7, Name: "Review Bot"})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	identity, err := client.WhoAmI(context.Background(), gitprovider.Credential{Type: "pat", Token: "other-token"})
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if identity.Login != "review-bot" || identity.ID != "7" || identity.DisplayName != "Review Bot" {
		t.Fatalf("identity = %#v, want mapped user", identity)
	}
	if authorization != "Bearer other-token" {
		t.Fatalf("Authorization = %q, want supplied credential token", authorization)
	}
}

func TestGetPRMapsMergeRequestAndResolvesForkSource(t *testing.T) {
	ref := testPRRef()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/" + testProjectPath() + "/merge_requests/42":
			writeJSON(t, w, mergeRequestResponse{
				IID:          42,
				Title:        "Add feature",
				Description:  "Body text",
				State:        "opened",
				WebURL:       "https://gitlab.example.com/open cli/sub group/repo+name/-/merge_requests/42",
				Author:       userResponse{Username: "author", ID: 3},
				SourceBranch: "feature",
				TargetBranch: "main",
				SHA:          "headsha",
				DiffRefs: diffRefsResponse{
					BaseSHA:  "basesha",
					StartSHA: "startsha",
					HeadSHA:  "headsha",
				},
				SourceProjectID: 777,
				TargetProjectID: 778,
			})
		case "/projects/777":
			writeJSON(t, w, projectResponse{PathWithNamespace: "fork-group/fork-repo"})
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	pr, err := client.GetPR(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.Title != "Add feature" || pr.Body != "Body text" || pr.State != gitprovider.PRStateOpen {
		t.Fatalf("pr = %#v, want mapped merge request", pr)
	}
	if pr.Author.Login != "author" || pr.Author.ID != "3" {
		t.Fatalf("author = %#v, want mapped author", pr.Author)
	}
	wantHead := gitprovider.PRBranchRef{Host: "gitlab.example.com", Owner: "fork-group", Repo: "fork-repo", Name: "feature", Ref: "refs/heads/feature", SHA: "headsha"}
	if pr.Head != wantHead {
		t.Fatalf("head = %#v, want %#v", pr.Head, wantHead)
	}
	wantBase := gitprovider.PRBranchRef{Host: "gitlab.example.com", Owner: ref.Owner, Repo: ref.Repo, Name: "main", Ref: "refs/heads/main", SHA: "basesha"}
	if pr.Base != wantBase {
		t.Fatalf("base = %#v, want %#v", pr.Base, wantBase)
	}
}

func TestGetPRMapsStates(t *testing.T) {
	tests := []struct {
		state string
		want  gitprovider.PRState
	}{
		{state: "opened", want: gitprovider.PRStateOpen},
		{state: "locked", want: gitprovider.PRStateOpen},
		{state: "closed", want: gitprovider.PRStateClosed},
		{state: "merged", want: gitprovider.PRStateMerged},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, mergeRequestResponse{State: tt.state, SourceBranch: "feature", TargetBranch: "main"})
		}))
		client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
		pr, err := client.GetPR(context.Background(), testPRRef())
		server.Close()
		if err != nil {
			t.Fatalf("GetPR(%s): %v", tt.state, err)
		}
		if pr.State != tt.want {
			t.Errorf("state %q = %q, want %q", tt.state, pr.State, tt.want)
		}
	}
}

func TestGetDiffReturnsRawDiff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/raw_diffs" {
			t.Fatalf("path = %q, want raw_diffs", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte("diff --git a/main.go b/main.go\n"))
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	diff, err := client.GetDiff(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if diff.Raw != "diff --git a/main.go b/main.go\n" {
		t.Fatalf("diff = %q, want raw payload", diff.Raw)
	}
}

func TestGetDiffFallsBackToDiffsListingWhenRawDiffsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/" + testProjectPath() + "/merge_requests/42/raw_diffs":
			w.WriteHeader(http.StatusNotFound)
		case "/projects/" + testProjectPath() + "/merge_requests/42/diffs":
			writeJSON(t, w, []diffFileResponse{
				{OldPath: "main.go", NewPath: "main.go", Diff: "@@ -1,2 +1,2 @@\n-old\n+new\n context\n"},
				{OldPath: "gone.go", NewPath: "gone.go", DeletedFile: true, Diff: "@@ -1 +0,0 @@\n-bye\n"},
				{OldPath: "old-name.go", NewPath: "new-name.go", RenamedFile: true},
				{OldPath: "logo.png", NewPath: "logo.png"},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	diff, err := client.GetDiff(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	want := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,2 +1,2 @@",
		"-old",
		"+new",
		" context",
		"diff --git a/gone.go b/gone.go",
		"--- a/gone.go",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-bye",
		"diff --git a/old-name.go b/new-name.go",
		"rename from old-name.go",
		"rename to new-name.go",
		"diff --git a/logo.png b/logo.png",
		"Binary files a/logo.png and b/logo.png differ",
		"",
	}, "\n")
	if diff.Raw != want {
		t.Fatalf("diff = %q, want %q", diff.Raw, want)
	}
}

func TestGetDiffBetweenRefsReconstructsCompare(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/repository/compare" {
			t.Fatalf("path = %q, want compare", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("from") != "base" || r.URL.Query().Get("to") != "head" {
			t.Fatalf("query = %q, want from=base to=head", r.URL.RawQuery)
		}
		writeJSON(t, w, map[string]any{
			"diffs": []diffFileResponse{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1 +1 @@\n-x\n+y\n"}},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	diff, err := client.GetDiffBetweenRefs(context.Background(), testPRRef(), "base", "head")
	if err != nil {
		t.Fatalf("GetDiffBetweenRefs: %v", err)
	}
	if !strings.Contains(diff.Raw, "diff --git a/a.go b/a.go") || !strings.Contains(diff.Raw, "+y") {
		t.Fatalf("diff = %q, want reconstructed compare diff", diff.Raw)
	}
}

func TestGetFileAtRefFetchesRawContents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/repository/files/docs%2Fguide.md/raw" {
			t.Fatalf("path = %q, want escaped file path", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("ref") != "headsha" {
			t.Fatalf("ref = %q, want headsha", r.URL.Query().Get("ref"))
		}
		_, _ = w.Write([]byte("# Guide\n"))
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	body, err := client.GetFileAtRef(context.Background(), testPRRef(), "headsha", "docs/guide.md")
	if err != nil {
		t.Fatalf("GetFileAtRef: %v", err)
	}
	if string(body) != "# Guide\n" {
		t.Fatalf("body = %q, want raw file", body)
	}
	if _, err := client.GetFileAtRef(context.Background(), testPRRef(), "headsha", "../escape"); !errors.Is(err, ErrValidation) {
		t.Fatalf("GetFileAtRef dot path error = %v, want ErrValidation", err)
	}
}

func TestListTreeAtRefPaginates(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/repository/tree" {
			t.Fatalf("path = %q, want repository tree", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, []treeEntryResponse{{ID: "sha-2", Type: "blob", Path: ".codereview/agents/security/prompt.md"}})
			return
		}
		if got := r.URL.Query().Get("path"); got != ".codereview/agents" {
			t.Fatalf("path query = %q, want .codereview/agents", got)
		}
		w.Header().Set("Link", "<"+server.URL+r.URL.EscapedPath()+"?page=2&per_page=100>; rel=\"next\"")
		writeJSON(t, w, []treeEntryResponse{{ID: "sha-1", Type: "tree", Path: ".codereview/agents/security"}})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	entries, err := client.ListTreeAtRef(context.Background(), testPRRef(), "headsha", ".codereview/agents")
	if err != nil {
		t.Fatalf("ListTreeAtRef: %v", err)
	}
	want := []gitprovider.TreeEntry{
		{Path: ".codereview/agents/security", Type: "tree", SHA: "sha-1"},
		{Path: ".codereview/agents/security/prompt.md", Type: "blob", SHA: "sha-2"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
}

func TestListReviewsMapsApprovals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/approvals" {
			t.Fatalf("path = %q, want approvals", r.URL.EscapedPath())
		}
		writeJSON(t, w, map[string]any{
			"approved_by": []map[string]any{
				{"user": userResponse{Username: "review-bot", ID: 7}},
				{"user": userResponse{Username: "human", ID: 8}},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	reviews, err := client.ListReviews(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("reviews = %#v, want two approvals", reviews)
	}
	if reviews[0].State != gitprovider.ReviewStateApproved || reviews[0].Event != review.ReviewEventApprove {
		t.Fatalf("review = %#v, want approved state", reviews[0])
	}
	if reviews[0].Author.Login != "review-bot" || reviews[0].ID != gitprovider.ReviewID("approval-7") {
		t.Fatalf("review = %#v, want mapped approver", reviews[0])
	}
}

func TestListIssueCommentsFiltersSystemAndDiffNotes(t *testing.T) {
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	line := 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/notes" {
			t.Fatalf("path = %q, want notes", r.URL.EscapedPath())
		}
		writeJSON(t, w, []noteResponse{
			{ID: 1, Body: "plain comment", Author: userResponse{Username: "human", ID: 8}, CreatedAt: created, UpdatedAt: created},
			{ID: 2, Body: "approved this merge request", System: true},
			{ID: 3, Body: "diff comment", Position: &positionResponse{NewPath: "main.go", NewLine: &line}},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	comments, err := client.ListIssueComments(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want only the plain note", comments)
	}
	if comments[0].ID != gitprovider.CommentID("1") || comments[0].Body != "plain comment" || !comments[0].CreatedAt.Equal(created) {
		t.Fatalf("comment = %#v, want mapped plain note", comments[0])
	}
}
