// Package plannedactions defines the canonical planned-action vocabulary.
package plannedactions

import (
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// ActionKind identifies a planned host-side action.
type ActionKind string

// Action kind values.
const (
	ActionKindInlineComment ActionKind = "inline_comment"
	ActionKindThreadReply   ActionKind = "thread_reply"
	ActionKindResolveThread ActionKind = "resolve_thread"
	ActionKindRollupComment ActionKind = "rollup_comment"
	ActionKindSubmitReview  ActionKind = "submit_review"
)

// String returns the persisted action kind.
func (k ActionKind) String() string { return string(k) }

// Valid reports whether k is a known action kind.
func (k ActionKind) Valid() bool {
	switch k {
	case ActionKindInlineComment, ActionKindThreadReply, ActionKindResolveThread, ActionKindRollupComment, ActionKindSubmitReview:
		return true
	default:
		return false
	}
}

// ActionStatus records the lifecycle status of a planned action.
type ActionStatus string

// Action status values.
const (
	ActionStatusPending        ActionStatus = "pending"
	ActionStatusPosted         ActionStatus = "posted"
	ActionStatusFailedTerminal ActionStatus = "failed_terminal"
	ActionStatusSuperseded     ActionStatus = "superseded"
	ActionStatusPlannedOnly    ActionStatus = "planned_only"
)

// String returns the persisted action status.
func (s ActionStatus) String() string { return string(s) }

// Valid reports whether s is a known action status.
func (s ActionStatus) Valid() bool {
	switch s {
	case ActionStatusPending, ActionStatusPosted, ActionStatusFailedTerminal, ActionStatusSuperseded, ActionStatusPlannedOnly:
		return true
	default:
		return false
	}
}

// Action is the canonical metadata and typed payload for one planned action.
type Action struct {
	ActionID  string
	Kind      ActionKind
	FindingID review.FindingID
	ThreadID  string
	PlannedAt time.Time
	Status    ActionStatus
	Required  bool

	InlineComment *InlineCommentPayload
	ThreadReply   *ThreadReplyPayload
	ResolveThread *ResolveThreadPayload
	RollupComment *RollupCommentPayload
	SubmitReview  *SubmitReviewPayload
}

// InlineCommentPayload is the provider-neutral inline comment payload.
type InlineCommentPayload struct {
	Body         string            `json:"body"`
	Path         string            `json:"path"`
	Side         review.DiffSide   `json:"side"`
	Line         int               `json:"line"`
	SubjectType  review.AnchorKind `json:"subject_type"`
	DiffPosition int               `json:"diff_position"`
}

// ThreadReplyPayload is the thread reply payload.
type ThreadReplyPayload struct {
	Body    string `json:"body"`
	Summary bool   `json:"summary"`
}

// ResolveThreadPayload is the resolve-thread payload.
type ResolveThreadPayload struct{}

// RollupCommentPayload is the rollup comment payload.
type RollupCommentPayload struct {
	Body string `json:"body"`
}

// SubmitReviewPayload is the submit-review payload.
type SubmitReviewPayload struct {
	Body  string             `json:"body"`
	Event review.ReviewEvent `json:"event"`
}
