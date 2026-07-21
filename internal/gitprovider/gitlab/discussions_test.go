package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestListInlineThreadsMapsDiffDiscussionsAndPaginates(t *testing.T) {
	newLine := 12
	oldLine := 4
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/discussions" {
			t.Fatalf("path = %q, want discussions", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, []discussionResponse{
				{
					ID: "disc-left",
					Notes: []noteResponse{{
						ID: 30, Body: "old side", Author: userResponse{Username: "human", ID: 8},
						Resolvable: true, Resolved: false,
						Position: &positionResponse{HeadSHA: "headsha", OldPath: "legacy.go", NewPath: "legacy.go", PositionType: "text", OldLine: &oldLine},
					}},
				},
				{
					ID:    "disc-plain",
					Notes: []noteResponse{{ID: 40, Body: "no position note"}},
				},
			})
			return
		}
		w.Header().Set("Link", "<"+server.URL+r.URL.EscapedPath()+"?page=2&per_page=100>; rel=\"next\"")
		writeJSON(t, w, []discussionResponse{
			{
				ID: "disc-right",
				Notes: []noteResponse{
					{
						ID: 10, Body: "finding", Author: userResponse{Username: "review-bot", ID: 7},
						Resolvable: true, Resolved: true,
						Position: &positionResponse{HeadSHA: "headsha", OldPath: "main.go", NewPath: "main.go", PositionType: "text", NewLine: &newLine},
					},
					{
						ID: 11, Body: "reply", Author: userResponse{Username: "human", ID: 8},
						Resolvable: true, Resolved: true,
					},
					{ID: 12, Body: "changed this line", System: true},
				},
			},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	threads, err := client.ListInlineThreads(context.Background(), testPRRef())
	if err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("threads = %#v, want two diff discussions", threads)
	}
	first := threads[0]
	if first.ID != gitprovider.ThreadID("disc-right") || !first.Resolved || first.Path != "main.go" ||
		first.Side != review.DiffSideRight || first.Line != 12 || first.SubjectType != review.AnchorKindLine || first.CommitSHA != "headsha" {
		t.Fatalf("thread = %#v, want mapped right-side thread", first)
	}
	if len(first.Comments) != 2 {
		t.Fatalf("comments = %#v, want system notes excluded", first.Comments)
	}
	if first.Comments[0].ID != gitprovider.CommentID("10") || first.Comments[0].ThreadID != first.ID ||
		first.Comments[0].Author.Login != "review-bot" || first.Comments[0].Line != 12 {
		t.Fatalf("comment = %#v, want mapped first note", first.Comments[0])
	}
	second := threads[1]
	if second.ID != gitprovider.ThreadID("disc-left") || second.Resolved || second.Side != review.DiffSideLeft ||
		second.Line != 4 || second.Path != "legacy.go" {
		t.Fatalf("thread = %#v, want mapped left-side thread", second)
	}
}

func TestPostInlineCommentBuildsTextPosition(t *testing.T) {
	tests := []struct {
		name        string
		comment     gitprovider.InlineComment
		diff        string
		wantOldLine int
		wantNewLine int
	}{
		{
			name: "added line posts new line only",
			comment: gitprovider.InlineComment{
				CommitSHA: "headsha", Body: "finding", Path: "main.go",
				Side: review.DiffSideRight, Line: 2, SubjectType: review.AnchorKindLine,
			},
			diff:        "@@ -1,2 +1,3 @@\n context\n+added\n context2\n",
			wantNewLine: 2,
		},
		{
			name: "context line posts both sides",
			comment: gitprovider.InlineComment{
				CommitSHA: "headsha", Body: "finding", Path: "main.go",
				Side: review.DiffSideRight, Line: 3, SubjectType: review.AnchorKindLine,
			},
			diff:        "@@ -1,2 +1,3 @@\n context\n+added\n context2\n",
			wantOldLine: 2,
			wantNewLine: 3,
		},
		{
			name: "deleted line posts old line only",
			comment: gitprovider.InlineComment{
				CommitSHA: "headsha", Body: "finding", Path: "main.go",
				Side: review.DiffSideLeft, Line: 2, SubjectType: review.AnchorKindLine,
			},
			diff:        "@@ -1,2 +1,1 @@\n context\n-removed\n",
			wantOldLine: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posted positionRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42" && r.Method == http.MethodGet:
					writeJSON(t, w, mergeRequestResponse{
						State: "opened", SourceBranch: "feature", TargetBranch: "main",
						DiffRefs: diffRefsResponse{BaseSHA: "basesha", StartSHA: "startsha", HeadSHA: "headsha"},
					})
				case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/diffs":
					writeJSON(t, w, []diffFileResponse{{OldPath: "main.go", NewPath: "main.go", Diff: tt.diff}})
				case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/discussions" && r.Method == http.MethodPost:
					var request discussionRequest
					decodeJSON(t, r.Body, &request)
					if request.Position == nil {
						t.Fatal("position missing from discussion request")
					}
					posted = *request.Position
					writeJSON(t, w, discussionResponse{ID: "disc-1", Notes: []noteResponse{{ID: 99}}})
				default:
					t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
				}
			}))
			defer server.Close()
			client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
			id, err := client.PostInlineComment(context.Background(), testPRRef(), tt.comment)
			if err != nil {
				t.Fatalf("PostInlineComment: %v", err)
			}
			if id != gitprovider.CommentID("99") {
				t.Fatalf("id = %q, want 99", id)
			}
			if posted.BaseSHA != "basesha" || posted.StartSHA != "startsha" || posted.HeadSHA != "headsha" {
				t.Fatalf("position = %#v, want diff refs from merge request", posted)
			}
			if posted.PositionType != "text" || posted.OldPath != "main.go" || posted.NewPath != "main.go" {
				t.Fatalf("position = %#v, want text position on main.go", posted)
			}
			if posted.OldLine != tt.wantOldLine || posted.NewLine != tt.wantNewLine {
				t.Fatalf("position lines = old %d new %d, want old %d new %d", posted.OldLine, posted.NewLine, tt.wantOldLine, tt.wantNewLine)
			}
		})
	}
}

func TestPostInlineCommentBuildsFilePositionWithRenamePaths(t *testing.T) {
	var posted positionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42" && r.Method == http.MethodGet:
			writeJSON(t, w, mergeRequestResponse{
				State: "opened", SourceBranch: "feature", TargetBranch: "main",
				DiffRefs: diffRefsResponse{BaseSHA: "basesha", StartSHA: "startsha", HeadSHA: "headsha"},
			})
		case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/diffs":
			writeJSON(t, w, []diffFileResponse{{OldPath: "old-name.go", NewPath: "new-name.go", RenamedFile: true}})
		case r.URL.EscapedPath() == "/projects/"+testProjectPath()+"/merge_requests/42/discussions" && r.Method == http.MethodPost:
			var request discussionRequest
			decodeJSON(t, r.Body, &request)
			posted = *request.Position
			writeJSON(t, w, discussionResponse{ID: "disc-1", Notes: []noteResponse{{ID: 99}}})
		default:
			t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
		}
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.PostInlineComment(context.Background(), testPRRef(), gitprovider.InlineComment{
		CommitSHA: "headsha", Body: "file finding", Path: "new-name.go", SubjectType: review.AnchorKindFile,
	})
	if err != nil {
		t.Fatalf("PostInlineComment: %v", err)
	}
	if posted.PositionType != "file" || posted.OldPath != "old-name.go" || posted.NewPath != "new-name.go" {
		t.Fatalf("position = %#v, want file position with rename paths", posted)
	}
	if posted.OldLine != 0 || posted.NewLine != 0 {
		t.Fatalf("position = %#v, want no lines on file position", posted)
	}
}

func TestPostInlineCommentRejectsStaleCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected write %s %q", r.Method, r.URL.EscapedPath())
		}
		writeJSON(t, w, mergeRequestResponse{
			State: "opened", SourceBranch: "feature", TargetBranch: "main",
			DiffRefs: diffRefsResponse{BaseSHA: "basesha", StartSHA: "startsha", HeadSHA: "newer-head"},
		})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.PostInlineComment(context.Background(), testPRRef(), gitprovider.InlineComment{
		CommitSHA: "stale-head", Body: "finding", Path: "main.go",
		Side: review.DiffSideRight, Line: 2, SubjectType: review.AnchorKindLine,
	})
	if !errors.Is(err, gitprovider.ErrStaleSHA) {
		t.Fatalf("PostInlineComment error = %v, want ErrStaleSHA", err)
	}
}

func TestReplyToThreadPostsDiscussionNote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/discussions/disc-1/notes" {
			t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
		}
		var request noteRequest
		decodeJSON(t, r.Body, &request)
		if request.Body != "reply body" {
			t.Fatalf("body = %q, want reply body", request.Body)
		}
		writeJSON(t, w, noteResponse{ID: 55})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	id, err := client.ReplyToThread(context.Background(), testPRRef(), gitprovider.ThreadID("disc-1"), "reply body")
	if err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}
	if id != gitprovider.CommentID("55") {
		t.Fatalf("id = %q, want 55", id)
	}
}

func TestResolveThreadPutsResolved(t *testing.T) {
	resolved := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/projects/"+testProjectPath()+"/merge_requests/42/discussions/disc-1" {
			t.Fatalf("unexpected request %s %q", r.Method, r.URL.EscapedPath())
		}
		resolved = r.URL.Query().Get("resolved")
		writeJSON(t, w, discussionResponse{ID: "disc-1"})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	if err := client.ResolveThread(context.Background(), testPRRef(), gitprovider.ThreadID("disc-1")); err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}
	if resolved != "true" {
		t.Fatalf("resolved = %q, want true", resolved)
	}
}

func decodeJSON(t *testing.T, body io.Reader, out any) {
	t.Helper()
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		t.Fatalf("Unmarshal %s: %v", payload, err)
	}
}
