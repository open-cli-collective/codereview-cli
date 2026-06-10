package gitprovider

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// Credential carries already-resolved credential material. This package never
// reads config, keyrings, environment variables, or files.
type Credential struct {
	Type        string
	Token       string
	Login       string
	ID          string
	DisplayName string
}

// PRRef identifies one pull request on one host.
type PRRef struct {
	Host   string
	Owner  string
	Repo   string
	Number int
}

// Validate checks that r can be used as a stable provider and ledger key.
func (r PRRef) Validate() error {
	if err := requireNonEmpty("host", r.Host); err != nil {
		return err
	}
	if err := requireNonEmpty("owner", r.Owner); err != nil {
		return err
	}
	if err := requireNonEmpty("repo", r.Repo); err != nil {
		return err
	}
	if r.Number <= 0 {
		return fmt.Errorf("PR number must be positive")
	}
	return nil
}

// Identity describes a git-host user without binding callers to one host's
// account schema.
type Identity struct {
	Login       string
	ID          string
	DisplayName string
}

// PRBranchRef identifies a base or head ref. Host/Owner/Repo are explicit
// because a pull request head may live in a fork.
type PRBranchRef struct {
	Host  string
	Owner string
	Repo  string
	Name  string
	Ref   string
	SHA   string
}

// PRState is a normalized provider-neutral pull request state.
type PRState string

// Pull request states.
const (
	PRStateOpen   PRState = "open"
	PRStateClosed PRState = "closed"
	PRStateMerged PRState = "merged"
)

// Valid reports whether s is one of the known pull request states.
func (s PRState) Valid() bool {
	switch s {
	case PRStateOpen, PRStateClosed, PRStateMerged:
		return true
	default:
		return false
	}
}

// PR is the provider-neutral pull request snapshot used by later pipeline
// stages.
type PR struct {
	Ref    PRRef
	Title  string
	URL    string
	State  PRState
	Author Identity
	Head   PRBranchRef
	Base   PRBranchRef
}

// UnifiedDiff is a provider-neutral unified diff payload.
type UnifiedDiff struct {
	Raw string
}

// ProviderCaps advertises host features that affect posting choices.
type ProviderCaps struct {
	NativeFileLevelComments bool
	ThreadResolution        bool
}

type (
	// CommentID identifies a provider comment.
	CommentID string
	// ThreadID identifies a provider inline review thread.
	ThreadID string
	// ReviewID identifies a provider pull-request review.
	ReviewID string
)

// InlineComment is the write request for posting an inline comment.
type InlineComment struct {
	CommitSHA    string
	Body         string
	Path         string
	Side         review.DiffSide
	Line         int
	SubjectType  review.AnchorKind
	DiffPosition int
}

// Validate checks provider-seam invariants for an inline comment write.
func (c InlineComment) Validate() error {
	if err := requireNonEmpty("commit SHA", c.CommitSHA); err != nil {
		return err
	}
	if err := requireNonEmpty("body", c.Body); err != nil {
		return err
	}
	if err := requireNonEmpty("path", c.Path); err != nil {
		return err
	}
	switch c.SubjectType {
	case review.AnchorKindLine:
		if !c.Side.Valid() {
			return fmt.Errorf("line comment requires a valid side")
		}
		if c.Line <= 0 {
			return fmt.Errorf("line comment requires a positive line")
		}
	case review.AnchorKindFile:
		if c.Side != "" {
			return fmt.Errorf("file comment cannot set side")
		}
		if c.Line != 0 {
			return fmt.Errorf("file comment cannot set line")
		}
		if c.DiffPosition != 0 {
			return fmt.Errorf("file comment cannot set diff position")
		}
	default:
		return fmt.Errorf("invalid subject type %q", c.SubjectType)
	}
	return nil
}

// ReviewRequest is the write request for submitting a pull request review.
type ReviewRequest struct {
	CommitSHA string
	Event     review.ReviewEvent
	Body      string
}

// Validate checks provider-seam invariants for a review submission.
func (r ReviewRequest) Validate() error {
	if err := requireNonEmpty("commit SHA", r.CommitSHA); err != nil {
		return err
	}
	if !r.Event.Valid() {
		return fmt.Errorf("invalid review event %q", r.Event)
	}
	return requireNonEmpty("body", r.Body)
}

// TreeEntry is one git tree entry at a ref.
type TreeEntry struct {
	Path string
	Type string
	SHA  string
}

// IssueComment is a provider-neutral issue or pull-request comment.
type IssueComment struct {
	ID        CommentID
	Body      string
	Author    Identity
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReviewState is a normalized provider-neutral pull request review state.
type ReviewState string

// Pull request review states.
const (
	ReviewStateApproved         ReviewState = "approved"
	ReviewStateChangesRequested ReviewState = "changes_requested"
	ReviewStateCommented        ReviewState = "commented"
	ReviewStateDismissed        ReviewState = "dismissed"
	ReviewStatePending          ReviewState = "pending"
)

// Valid reports whether s is one of the known review states.
func (s ReviewState) Valid() bool {
	switch s {
	case ReviewStateApproved, ReviewStateChangesRequested, ReviewStateCommented, ReviewStateDismissed, ReviewStatePending:
		return true
	default:
		return false
	}
}

// Review is a provider-neutral pull-request review record.
type Review struct {
	ID          ReviewID
	Body        string
	Author      Identity
	State       ReviewState
	Event       review.ReviewEvent
	CommitSHA   string
	URL         string
	SubmittedAt time.Time
}

// ThreadComment is one host comment inside an inline thread.
type ThreadComment struct {
	ID          CommentID
	ThreadID    ThreadID
	Body        string
	Author      Identity
	CommitSHA   string
	Path        string
	Side        review.DiffSide
	Line        int
	SubjectType review.AnchorKind
	URL         string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// InlineThread is a provider-neutral inline review thread.
type InlineThread struct {
	ID          ThreadID
	Resolved    bool
	Path        string
	Side        review.DiffSide
	Line        int
	SubjectType review.AnchorKind
	CommitSHA   string
	Comments    []ThreadComment
}

func requireNonEmpty(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}
