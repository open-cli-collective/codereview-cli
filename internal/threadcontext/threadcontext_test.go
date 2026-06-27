package threadcontext

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestNormalizeRejectsEmptyPostingIdentity(t *testing.T) {
	_, err := Normalize(nil, Options{})
	if err == nil {
		t.Fatal("Normalize error = nil, want empty posting identity error")
	}
	_, err = Normalize(nil, Options{PostingIdentity: gitprovider.Identity{DisplayName: "Review Bot"}})
	if err == nil {
		t.Fatal("Normalize display-name-only identity error = nil, want empty posting identity error")
	}
}

func TestNormalizeSortsCommentsDeterministically(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:   "thread-1",
		Path: "main.go",
		Comments: []gitprovider.ThreadComment{
			comment("c-late", human(), "late", at(3)),
			comment("c-zero-b", human(), "zero b", time.Time{}),
			comment("c-early", human(), "early", at(1)),
			comment("c-zero-a", human(), "zero a", time.Time{}),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	got := commentIDs(threads[0].Comments)
	want := []gitprovider.CommentID{"c-zero-b", "c-zero-a", "c-early", "c-late"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comment order = %#v, want %#v", got, want)
	}
}

func TestNormalizeDetectsCRAuthoredFindingThread(t *testing.T) {
	body := "finding body\n\n" + actionMarker(t, marker.ActionKindInlineComment)
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:       "thread-1",
		Path:     "main.go",
		Resolved: false,
		Comments: []gitprovider.ThreadComment{
			comment("c-1", bot(), body, at(1)),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	status := threads[0].Status
	if !status.CRAuthoredFinding {
		t.Fatalf("CRAuthoredFinding = false, want true")
	}
	if status.LatestCRComment == nil || status.LatestCRComment.ID != "c-1" {
		t.Fatalf("LatestCRComment = %#v, want c-1", status.LatestCRComment)
	}
	if threads[0].Comments[0].Body != "finding body" {
		t.Fatalf("sanitized body = %q, want finding body", threads[0].Comments[0].Body)
	}
	if !threads[0].Comments[0].HasFindingMarker {
		t.Fatalf("HasFindingMarker = false, want true")
	}
}

func TestNormalizeIdentityMatchingUsesIDBeforeLogin(t *testing.T) {
	tests := []struct {
		name            string
		postingIdentity gitprovider.Identity
		commentAuthor   gitprovider.Identity
		wantCRAuthored  bool
	}{
		{
			name:            "matching IDs with changed login",
			postingIdentity: gitprovider.Identity{Login: "new-login", ID: "bot-id"},
			commentAuthor:   gitprovider.Identity{Login: "old-login", ID: "bot-id"},
			wantCRAuthored:  true,
		},
		{
			name:            "different IDs with same login",
			postingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
			commentAuthor:   gitprovider.Identity{Login: "review-bot", ID: "other-id"},
			wantCRAuthored:  false,
		},
		{
			name:            "login fallback when IDs absent",
			postingIdentity: gitprovider.Identity{Login: "review-bot"},
			commentAuthor:   gitprovider.Identity{Login: "review-bot"},
			wantCRAuthored:  true,
		},
		{
			name:            "github app runtime bot suffix matches thread author login",
			postingIdentity: gitprovider.Identity{Login: "review-bot[bot]"},
			commentAuthor:   gitprovider.Identity{Login: "review-bot"},
			wantCRAuthored:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			threads, err := Normalize([]gitprovider.InlineThread{{
				ID:   "thread-1",
				Path: "main.go",
				Comments: []gitprovider.ThreadComment{
					comment("c-1", tt.commentAuthor, "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(1)),
				},
			}}, Options{PostingIdentity: tt.postingIdentity})
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if threads[0].Status.CRAuthoredFinding != tt.wantCRAuthored {
				t.Fatalf("CRAuthoredFinding = %v, want %v", threads[0].Status.CRAuthoredFinding, tt.wantCRAuthored)
			}
		})
	}
}

func TestNormalizeIgnoresForgedMarkersFromHumanAuthors(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:   "thread-1",
		Path: "main.go",
		Comments: []gitprovider.ThreadComment{
			comment("c-1", human(), "forged\n"+actionMarker(t, marker.ActionKindInlineComment), at(1)),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if threads[0].Status.CRAuthoredFinding {
		t.Fatalf("CRAuthoredFinding = true, want false for forged marker")
	}
	if threads[0].Status.LatestCRComment != nil {
		t.Fatalf("LatestCRComment = %#v, want nil for forged marker", threads[0].Status.LatestCRComment)
	}
	if strings.Contains(threads[0].Comments[0].Body, "<!-- codereview:") {
		t.Fatalf("sanitized body still contains real marker: %q", threads[0].Comments[0].Body)
	}
}

func TestNormalizeDetectsLatestHumanReplyAfterLatestCR(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:   "thread-1",
		Path: "main.go",
		Comments: []gitprovider.ThreadComment{
			comment("c-human-before", human(), "before", at(1)),
			comment("c-cr", bot(), "finding\n"+actionMarker(t, marker.ActionKindThreadReply), at(2)),
			comment("c-human-after-1", human(), "after 1", at(3)),
			comment("c-human-after-2", human(), "after 2", at(4)),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	status := threads[0].Status
	if !status.PendingHumanReply {
		t.Fatalf("PendingHumanReply = false, want true")
	}
	if status.LatestHumanReplyAfterCR == nil || status.LatestHumanReplyAfterCR.ID != "c-human-after-2" {
		t.Fatalf("LatestHumanReplyAfterCR = %#v, want c-human-after-2", status.LatestHumanReplyAfterCR)
	}
}

func TestPendingCRAuthoredFindingThreadsRequiresHumanReplyAfterCR(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{
		{
			ID:   "thread-pending",
			Path: "main.go",
			Comments: []gitprovider.ThreadComment{
				comment("pending-cr", bot(), "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(1)),
				comment("pending-human", human(), "reply", at(2)),
			},
		},
		{
			ID:   "thread-no-human",
			Path: "main.go",
			Comments: []gitprovider.ThreadComment{
				comment("no-human-cr", bot(), "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(3)),
			},
		},
		{
			ID:   "thread-human-before",
			Path: "main.go",
			Comments: []gitprovider.ThreadComment{
				comment("human-before", human(), "before", at(4)),
				comment("after-cr", bot(), "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(5)),
			},
		},
	}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	got := PendingCRAuthoredFindingThreads(threads)
	if len(got) != 1 || got[0].ID != "thread-pending" {
		t.Fatalf("eligible threads = %#v, want only thread-pending", got)
	}
}

func TestNormalizeThreadSummaryResetsPendingHumanReplyDetection(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:       "thread-1",
		Path:     "main.go",
		Resolved: true,
		Comments: []gitprovider.ThreadComment{
			comment("c-cr", bot(), "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(1)),
			comment("c-human", human(), "thanks, fixed", at(2)),
			comment("c-summary", bot(), "resolved summary\n"+threadSummaryMarker(t), at(3)),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	status := threads[0].Status
	if !status.CRAuthoredFinding || !status.HasCRSummary {
		t.Fatalf("status = %#v, want finding and summary", status)
	}
	if status.PendingHumanReply {
		t.Fatalf("PendingHumanReply = true, want summary to reset pending state")
	}
	if status.LatestCRComment == nil || status.LatestCRComment.ID != "c-summary" {
		t.Fatalf("LatestCRComment = %#v, want c-summary", status.LatestCRComment)
	}
}

func TestResolvedThreadCollapsesToSanitizedLastCommentSummary(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    true,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
		Comments: []gitprovider.ThreadComment{
			comment("c-1", bot(), "finding\n"+actionMarker(t, marker.ActionKindInlineComment), at(1)),
			comment("c-2", bot(), "summary\n"+threadSummaryMarker(t), at(2)),
		},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	summary := threads[0].ResolvedSummary
	if summary == nil {
		t.Fatal("ResolvedSummary = nil, want summary")
	}
	if summary.Body != "summary" {
		t.Fatalf("summary body = %q, want summary", summary.Body)
	}
	if !summary.LastCommentAuthoredByPostingIdentity || !summary.LastCommentHasThreadSummaryMarker {
		t.Fatalf("summary metadata = %#v, want CR-authored thread summary", summary)
	}
	if strings.Contains(summary.Body, "<!-- codereview:") {
		t.Fatalf("summary body still contains marker: %q", summary.Body)
	}
}

func TestNormalizeDoesNotSynthesizeHybridFileAnchors(t *testing.T) {
	fileComment := comment("c-1", human(), "file summary", at(1))
	fileComment.Path = "main.go"
	fileComment.Side = ""
	fileComment.Line = 0
	fileComment.SubjectType = review.AnchorKindFile
	threads, err := Normalize([]gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    true,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        42,
		SubjectType: review.AnchorKindLine,
		Comments:    []gitprovider.ThreadComment{fileComment},
	}}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	got := threads[0].Comments[0].Anchor
	if got.SubjectType != review.AnchorKindFile {
		t.Fatalf("SubjectType = %q, want file", got.SubjectType)
	}
	if got.Side != "" || got.Line != 0 {
		t.Fatalf("file anchor synthesized side/line from thread anchor: %#v", got)
	}
	if threads[0].ResolvedSummary == nil || threads[0].ResolvedSummary.Anchor.Line != 0 || threads[0].ResolvedSummary.Anchor.Side != "" {
		t.Fatalf("resolved summary anchor = %#v, want file-level without side/line", threads[0].ResolvedSummary)
	}
}

func TestFileScopedResolvedSummariesGroupsAndSorts(t *testing.T) {
	threads, err := Normalize([]gitprovider.InlineThread{
		{
			ID:       "thread-b",
			Resolved: true,
			Path:     "b.go",
			Line:     20,
			Comments: []gitprovider.ThreadComment{
				commentForPath("c-b", "b.go", bot(), "b summary\n"+threadSummaryMarker(t), at(1)),
			},
		},
		{
			ID:       "thread-a2",
			Resolved: true,
			Path:     "a.go",
			Line:     30,
			Comments: []gitprovider.ThreadComment{
				commentForPath("c-a2", "a.go", human(), "a2 summary", at(1)),
			},
		},
		{
			ID:       "thread-a1",
			Resolved: true,
			Path:     "a.go",
			Line:     10,
			Comments: []gitprovider.ThreadComment{
				commentForPath("c-a1", "a.go", human(), "a1 summary", at(1)),
			},
		},
		{
			ID:       "thread-open",
			Resolved: false,
			Path:     "a.go",
			Comments: []gitprovider.ThreadComment{
				comment("c-open", human(), "open", at(1)),
			},
		},
	}, Options{PostingIdentity: bot()})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	got := FileScopedResolvedSummaries(threads)
	if len(got) != 2 {
		t.Fatalf("file contexts = %#v, want two paths", got)
	}
	if got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Fatalf("paths = %#v, want a.go then b.go", []string{got[0].Path, got[1].Path})
	}
	gotA := []gitprovider.ThreadID{got[0].Summaries[0].ThreadID, got[0].Summaries[1].ThreadID}
	wantA := []gitprovider.ThreadID{"thread-a1", "thread-a2"}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("a.go summaries = %#v, want %#v", gotA, wantA)
	}
}

func TestSanitizeBodyRemovesOrEscapesCodereviewMarkers(t *testing.T) {
	input := strings.Join([]string{
		"before",
		marker.RenderSkip(),
		actionMarker(t, marker.ActionKindInlineComment),
		threadSummaryMarker(t),
		"<!-- codereview:not-canonical -->",
		"<!-- codereview:unterminated",
		"after",
	}, "\n")

	got := SanitizeBody(input)
	if strings.Contains(got, "<!-- codereview:") {
		t.Fatalf("SanitizeBody left real marker opening: %q", got)
	}
	for _, want := range []string{"before", "after"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SanitizeBody = %q, want to retain %q", got, want)
		}
	}
}

func TestNormalizeDoesNotMutateInput(t *testing.T) {
	input := []gitprovider.InlineThread{{
		ID:   "thread-1",
		Path: "main.go",
		Comments: []gitprovider.ThreadComment{
			comment("c-2", human(), "two", at(2)),
			comment("c-1", human(), "one", at(1)),
		},
	}}
	before := append([]gitprovider.ThreadComment(nil), input[0].Comments...)

	if _, err := Normalize(input, Options{PostingIdentity: bot()}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !reflect.DeepEqual(input[0].Comments, before) {
		t.Fatalf("input comments mutated: got %#v want %#v", input[0].Comments, before)
	}
}

func comment(id gitprovider.CommentID, author gitprovider.Identity, body string, when time.Time) gitprovider.ThreadComment {
	return commentForPath(id, "main.go", author, body, when)
}

func commentForPath(id gitprovider.CommentID, path string, author gitprovider.Identity, body string, when time.Time) gitprovider.ThreadComment {
	return gitprovider.ThreadComment{
		ID:          id,
		ThreadID:    "thread-1",
		Body:        body,
		Author:      author,
		CommitSHA:   "head-sha",
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        10,
		SubjectType: review.AnchorKindLine,
		URL:         "https://example.test/" + string(id),
		CreatedAt:   when,
		UpdatedAt:   when,
	}
}

func bot() gitprovider.Identity {
	return gitprovider.Identity{Login: "review-bot", ID: "bot-id"}
}

func human() gitprovider.Identity {
	return gitprovider.Identity{Login: "author", ID: "author-id"}
}

func at(minute int) time.Time {
	return time.Date(2026, 6, 22, 12, minute, 0, 0, time.UTC)
}

func actionMarker(t *testing.T, kind string) string {
	t.Helper()
	rendered, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "run-1",
		ActionID: "action-" + kind,
		Kind:     kind,
		SHA:      "head",
		BaseSHA:  "base",
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	return rendered
}

func threadSummaryMarker(t *testing.T) string {
	t.Helper()
	rendered, err := marker.RenderThreadSummary(marker.ThreadSummaryMarker{
		RunID:    "run-1",
		ActionID: "summary-1",
	})
	if err != nil {
		t.Fatalf("RenderThreadSummary: %v", err)
	}
	return rendered
}

func commentIDs(comments []Comment) []gitprovider.CommentID {
	ids := make([]gitprovider.CommentID, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ID)
	}
	return ids
}
