package gitprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestFakeImplementsGitProvider(_ *testing.T) {
	var _ GitProvider = (*Fake)(nil)
}

func TestFakeCapabilities(t *testing.T) {
	var fake Fake
	caps := ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true}
	fake.SetCapabilities(caps)
	if got := fake.Capabilities(); got != caps {
		t.Fatalf("Capabilities() = %#v, want %#v", got, caps)
	}
}

func TestFakeFileAndTreeReadsAreKeyedByFullSelectorAndCopied(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	var fake Fake
	if err := fake.SetFileAtRef(ref, "refs/heads/main", ".codereview/agents/a.yml", []byte("agent")); err != nil {
		t.Fatalf("SetFileAtRef: %v", err)
	}
	if err := fake.SetTreeAtRef(ref, "refs/heads/main", ".codereview/agents", []TreeEntry{{Path: "a.yml", Type: "blob", SHA: "sha1"}}); err != nil {
		t.Fatalf("SetTreeAtRef: %v", err)
	}

	file, err := fake.GetFileAtRef(ctx, ref, ".codereview/agents/a.yml", "refs/heads/main")
	if err != nil {
		t.Fatalf("GetFileAtRef: %v", err)
	}
	file[0] = 'x'
	fileAgain, err := fake.GetFileAtRef(ctx, ref, ".codereview/agents/a.yml", "refs/heads/main")
	if err != nil {
		t.Fatalf("GetFileAtRef again: %v", err)
	}
	if string(fileAgain) != "agent" {
		t.Fatalf("GetFileAtRef returned mutable backing storage: %q", fileAgain)
	}
	if _, err := fake.GetFileAtRef(ctx, ref, ".codereview/agents/a.yml", "refs/heads/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFileAtRef wrong git ref error = %v, want ErrNotFound", err)
	}
	if _, err := fake.GetFileAtRef(ctx, ref, ".codereview/agents/missing.yml", "refs/heads/main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFileAtRef wrong path error = %v, want ErrNotFound", err)
	}

	tree, err := fake.ListTreeAtRef(ctx, ref, "refs/heads/main", ".codereview/agents")
	if err != nil {
		t.Fatalf("ListTreeAtRef: %v", err)
	}
	tree[0].SHA = "mutated"
	treeAgain, err := fake.ListTreeAtRef(ctx, ref, "refs/heads/main", ".codereview/agents")
	if err != nil {
		t.Fatalf("ListTreeAtRef again: %v", err)
	}
	if treeAgain[0].SHA != "sha1" {
		t.Fatalf("ListTreeAtRef returned mutable backing storage: %#v", treeAgain)
	}
	if _, err := fake.ListTreeAtRef(ctx, ref, "refs/heads/main", ".codereview/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListTreeAtRef wrong path error = %v, want ErrNotFound", err)
	}
}

func TestFakeThreadReadsAreCopied(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	var fake Fake
	threads := []InlineThread{{
		ID:       "thread-1",
		Resolved: true,
		Comments: []ThreadComment{{
			ID:   "comment-1",
			Body: "body",
		}},
	}}
	if err := fake.SetInlineThreads(ref, threads); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}

	got, err := fake.ListInlineThreads(ctx, ref)
	if err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	got[0].Comments[0].Body = "mutated"
	again, err := fake.ListInlineThreads(ctx, ref)
	if err != nil {
		t.Fatalf("ListInlineThreads again: %v", err)
	}
	if again[0].Comments[0].Body != "body" {
		t.Fatalf("ListInlineThreads returned mutable backing storage: %#v", again)
	}
}

func TestFakeRecordsWritesByPRRefAndCopies(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	other := PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "other", Number: 99}
	var fake Fake

	id, err := fake.PostInlineComment(ctx, ref, testInlineComment())
	if err != nil {
		t.Fatalf("PostInlineComment: %v", err)
	}
	if id == "" {
		t.Fatal("PostInlineComment ID is empty, want generated ID")
	}
	if len(fake.RecordedInlineComments(other)) != 0 {
		t.Fatalf("RecordedInlineComments(other) = non-empty, want empty")
	}
	records := fake.RecordedInlineComments(ref)
	if len(records) != 1 {
		t.Fatalf("RecordedInlineComments(ref) len = %d, want 1", len(records))
	}
	records[0].Body = "mutated"
	again := fake.RecordedInlineComments(ref)
	if again[0].Body != "line comment" {
		t.Fatalf("RecordedInlineComments returned mutable backing storage: %#v", again)
	}

	_, err = fake.PostInlineComment(ctx, ref, InlineComment{CommitSHA: "", Body: "bad"})
	if err == nil {
		t.Fatal("PostInlineComment missing commit error = nil, want error")
	}
	if got := len(fake.RecordedInlineComments(ref)); got != 1 {
		t.Fatalf("RecordedInlineComments after invalid write len = %d, want 1", got)
	}
}

func TestFakeValidatesThreadAndIssueWrites(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	var fake Fake

	if _, err := fake.ReplyToThread(ctx, ref, "", "body"); err == nil {
		t.Fatal("ReplyToThread empty thread ID error = nil, want error")
	}
	if _, err := fake.ReplyToThread(ctx, ref, "thread-1", ""); err == nil {
		t.Fatal("ReplyToThread empty body error = nil, want error")
	}
	if err := fake.ResolveThread(ctx, ref, ""); err == nil {
		t.Fatal("ResolveThread empty thread ID error = nil, want error")
	}
	if _, err := fake.PostIssueComment(ctx, ref, ""); err == nil {
		t.Fatal("PostIssueComment empty body error = nil, want error")
	}

	if _, err := fake.ReplyToThread(ctx, ref, "thread-1", "body"); err != nil {
		t.Fatalf("ReplyToThread valid: %v", err)
	}
	if err := fake.ResolveThread(ctx, ref, "thread-1"); err != nil {
		t.Fatalf("ResolveThread valid: %v", err)
	}
	if _, err := fake.PostIssueComment(ctx, ref, "body"); err != nil {
		t.Fatalf("PostIssueComment valid: %v", err)
	}
	if got := fake.RecordedThreadReplies(ref); len(got) != 1 || got[0].ThreadID != "thread-1" {
		t.Fatalf("RecordedThreadReplies = %#v, want one thread-1 reply", got)
	}
	if got := fake.RecordedResolvedThreads(ref); len(got) != 1 || got[0] != "thread-1" {
		t.Fatalf("RecordedResolvedThreads = %#v, want one thread-1 resolution", got)
	}
	if got := fake.RecordedIssueComments(ref); len(got) != 1 || got[0] != "body" {
		t.Fatalf("RecordedIssueComments = %#v, want one body comment", got)
	}
}

func TestFakeInjectedErrorsCoverErrorReturningMethods(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	tests := []struct {
		op     Operation
		invoke func(*Fake) error
	}{
		{OperationWhoAmI, func(f *Fake) error {
			_, err := f.WhoAmI(ctx, Credential{Type: "pat", Token: "token"})
			return err
		}},
		{OperationGetPR, func(f *Fake) error {
			_, err := f.GetPR(ctx, ref)
			return err
		}},
		{OperationGetDiff, func(f *Fake) error {
			_, err := f.GetDiff(ctx, ref)
			return err
		}},
		{OperationGetFileAtRef, func(f *Fake) error {
			_, err := f.GetFileAtRef(ctx, ref, "README.md", "base-sha")
			return err
		}},
		{OperationListTreeAtRef, func(f *Fake) error {
			_, err := f.ListTreeAtRef(ctx, ref, "base-sha", ".codereview")
			return err
		}},
		{OperationListInlineThreads, func(f *Fake) error {
			_, err := f.ListInlineThreads(ctx, ref)
			return err
		}},
		{OperationListReviews, func(f *Fake) error {
			_, err := f.ListReviews(ctx, ref)
			return err
		}},
		{OperationListIssueComments, func(f *Fake) error {
			_, err := f.ListIssueComments(ctx, ref)
			return err
		}},
		{OperationPostInlineComment, func(f *Fake) error {
			_, err := f.PostInlineComment(ctx, ref, testInlineComment())
			return err
		}},
		{OperationReplyToThread, func(f *Fake) error {
			_, err := f.ReplyToThread(ctx, ref, "thread-1", "body")
			return err
		}},
		{OperationResolveThread, func(f *Fake) error {
			return f.ResolveThread(ctx, ref, "thread-1")
		}},
		{OperationPostIssueComment, func(f *Fake) error {
			_, err := f.PostIssueComment(ctx, ref, "body")
			return err
		}},
		{OperationSubmitReview, func(f *Fake) error {
			_, err := f.SubmitReview(ctx, ref, testReviewRequest())
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(string(tt.op), func(t *testing.T) {
			var fake Fake
			fake.SetError(tt.op, WrapError(ErrRetryable, tt.op, nil))
			err := tt.invoke(&fake)
			if !errors.Is(err, ErrRetryable) {
				t.Fatalf("%s injected error = %v, want ErrRetryable", tt.op, err)
			}
		})
	}
}

func TestFakeReadModelsUseDownstreamFields(t *testing.T) {
	ctx := context.Background()
	ref := testPRRef()
	var fake Fake
	pr := PR{
		Ref:    ref,
		Title:  "Add provider",
		URL:    "https://github.com/open-cli-collective/codereview-cli/pull/14",
		State:  "open",
		Author: Identity{Login: "rianjs", ID: "user-1", DisplayName: "Rian"},
		Head:   PRBranchRef{Host: "github.com", Owner: "rianjs", Repo: "codereview-cli", Name: "feature", Ref: "refs/heads/feature", SHA: "head-sha"},
		Base:   PRBranchRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Name: "main", Ref: "refs/heads/main", SHA: "base-sha"},
	}
	if err := fake.SetPR(ref, pr); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := fake.SetReviews(ref, []Review{{ID: "review-1", Body: "sha=head-sha base=base-sha", Author: pr.Author, Event: review.ReviewEventComment, CommitSHA: "head-sha"}}); err != nil {
		t.Fatalf("SetReviews: %v", err)
	}
	if err := fake.SetIssueComments(ref, []IssueComment{{ID: "comment-1", Body: "sha=head-sha base=base-sha", Author: pr.Author}}); err != nil {
		t.Fatalf("SetIssueComments: %v", err)
	}

	gotPR, err := fake.GetPR(ctx, ref)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if gotPR.Head.Owner != "rianjs" || gotPR.Base.Owner != "open-cli-collective" {
		t.Fatalf("GetPR branch repo identity = head %#v base %#v", gotPR.Head, gotPR.Base)
	}
	gotReviews, err := fake.ListReviews(ctx, ref)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(gotReviews) != 1 || gotReviews[0].CommitSHA != "head-sha" || gotReviews[0].Author.Login != "rianjs" {
		t.Fatalf("ListReviews = %#v, want commit SHA and author fields", gotReviews)
	}
	gotComments, err := fake.ListIssueComments(ctx, ref)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(gotComments) != 1 || gotComments[0].Author.Login != "rianjs" {
		t.Fatalf("ListIssueComments = %#v, want author field", gotComments)
	}
}
