package gitprovider

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestPRRefValidate(t *testing.T) {
	valid := testPRRef()
	tests := []struct {
		name    string
		mutate  func(*PRRef)
		wantErr string
	}{
		{name: "valid"},
		{name: "missing host", mutate: func(ref *PRRef) { ref.Host = "" }, wantErr: "host"},
		{name: "blank host", mutate: func(ref *PRRef) { ref.Host = "  " }, wantErr: "host"},
		{name: "missing owner", mutate: func(ref *PRRef) { ref.Owner = "" }, wantErr: "owner"},
		{name: "blank owner", mutate: func(ref *PRRef) { ref.Owner = "  " }, wantErr: "owner"},
		{name: "missing repo", mutate: func(ref *PRRef) { ref.Repo = "" }, wantErr: "repo"},
		{name: "blank repo", mutate: func(ref *PRRef) { ref.Repo = "  " }, wantErr: "repo"},
		{name: "bad number", mutate: func(ref *PRRef) { ref.Number = 0 }, wantErr: "PR number"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := valid
			if tt.mutate != nil {
				tt.mutate(&ref)
			}
			err := ref.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestInlineCommentValidate(t *testing.T) {
	validLine := testInlineComment()
	validFile := InlineComment{
		CommitSHA:   "abc123",
		Body:        "file comment",
		Path:        "main.go",
		SubjectType: review.AnchorKindFile,
	}
	tests := []struct {
		name    string
		comment InlineComment
		wantErr string
	}{
		{name: "valid line", comment: validLine},
		{name: "valid file", comment: validFile},
		{name: "missing commit", comment: mutateInline(validLine, func(c *InlineComment) { c.CommitSHA = "" }), wantErr: "commit SHA"},
		{name: "blank commit", comment: mutateInline(validLine, func(c *InlineComment) { c.CommitSHA = "  " }), wantErr: "commit SHA"},
		{name: "missing body", comment: mutateInline(validLine, func(c *InlineComment) { c.Body = "" }), wantErr: "body"},
		{name: "blank body", comment: mutateInline(validLine, func(c *InlineComment) { c.Body = "  " }), wantErr: "body"},
		{name: "missing path", comment: mutateInline(validLine, func(c *InlineComment) { c.Path = "" }), wantErr: "path"},
		{name: "blank path", comment: mutateInline(validLine, func(c *InlineComment) { c.Path = "  " }), wantErr: "path"},
		{name: "line missing side", comment: mutateInline(validLine, func(c *InlineComment) { c.Side = "" }), wantErr: "side"},
		{name: "line missing line", comment: mutateInline(validLine, func(c *InlineComment) { c.Line = 0 }), wantErr: "line"},
		{name: "file has side", comment: mutateInline(validFile, func(c *InlineComment) { c.Side = review.DiffSideRight }), wantErr: "side"},
		{name: "file has line", comment: mutateInline(validFile, func(c *InlineComment) { c.Line = 1 }), wantErr: "line"},
		{name: "file has diff position", comment: mutateInline(validFile, func(c *InlineComment) { c.DiffPosition = 1 }), wantErr: "diff position"},
		{name: "bad subject", comment: mutateInline(validLine, func(c *InlineComment) { c.SubjectType = "range" }), wantErr: "subject"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.comment.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestReviewRequestValidate(t *testing.T) {
	valid := testReviewRequest()
	tests := []struct {
		name    string
		request ReviewRequest
		wantErr string
	}{
		{name: "valid", request: valid},
		{name: "missing commit", request: mutateReview(valid, func(r *ReviewRequest) { r.CommitSHA = "" }), wantErr: "commit SHA"},
		{name: "blank commit", request: mutateReview(valid, func(r *ReviewRequest) { r.CommitSHA = "  " }), wantErr: "commit SHA"},
		{name: "bad event", request: mutateReview(valid, func(r *ReviewRequest) { r.Event = "changes_requested" }), wantErr: "review event"},
		{name: "missing body", request: mutateReview(valid, func(r *ReviewRequest) { r.Body = "" }), wantErr: "body"},
		{name: "blank body", request: mutateReview(valid, func(r *ReviewRequest) { r.Body = "  " }), wantErr: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestStateEnumsValidate(t *testing.T) {
	prStates := []PRState{PRStateOpen, PRStateClosed, PRStateMerged}
	for _, state := range prStates {
		if !state.Valid() {
			t.Fatalf("PRState(%q).Valid() = false, want true", state)
		}
	}
	if PRState("OPEN").Valid() {
		t.Fatal(`PRState("OPEN").Valid() = true, want false`)
	}

	reviewStates := []ReviewState{
		ReviewStateApproved,
		ReviewStateChangesRequested,
		ReviewStateCommented,
		ReviewStateDismissed,
		ReviewStatePending,
	}
	for _, state := range reviewStates {
		if !state.Valid() {
			t.Fatalf("ReviewState(%q).Valid() = false, want true", state)
		}
	}
	if ReviewState("APPROVED").Valid() {
		t.Fatal(`ReviewState("APPROVED").Valid() = true, want false`)
	}
}

func mutateInline(comment InlineComment, mutate func(*InlineComment)) InlineComment {
	mutate(&comment)
	return comment
}

func mutateReview(request ReviewRequest, mutate func(*ReviewRequest)) ReviewRequest {
	mutate(&request)
	return request
}

func testPRRef() PRRef {
	return PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 14}
}

func testInlineComment() InlineComment {
	return InlineComment{
		CommitSHA:   "abc123",
		Body:        "line comment",
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        12,
		SubjectType: review.AnchorKindLine,
	}
}

func testReviewRequest() ReviewRequest {
	return ReviewRequest{
		CommitSHA: "abc123",
		Event:     review.ReviewEventComment,
		Body:      "review body",
	}
}
