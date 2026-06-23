// Package plannedactions converts review plans into durable ledger actions.
package plannedactions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// FromReviewPlan converts one planner-local action into a ledger planned action.
func FromReviewPlan(runID string, action reviewplan.Action) (ledger.PlannedAction, error) {
	payload, err := Payload(action)
	if err != nil {
		return ledger.PlannedAction{}, err
	}
	planned := ledger.PlannedAction{
		ActionID:    action.ActionID,
		RunID:       runID,
		Kind:        LedgerKind(action.Kind),
		PlannedAt:   action.PlannedAt,
		PayloadJSON: string(payload),
		Status:      LedgerStatus(action.Status),
		Required:    action.Required,
	}
	if action.FindingID.Assigned() {
		id := action.FindingID.String()
		planned.FindingID = &id
	}
	if strings.TrimSpace(action.ThreadID) != "" {
		planned.ThreadID = &action.ThreadID
	}
	return planned, nil
}

// Payload returns the outbox payload JSON for action.
func Payload(action reviewplan.Action) ([]byte, error) {
	switch action.Kind {
	case reviewplan.ActionKindInlineComment:
		if action.InlineComment == nil {
			return nil, fmt.Errorf("plannedactions: inline payload missing")
		}
		return json.Marshal(outbox.InlineCommentPayload{
			Body:         action.InlineComment.Body,
			Path:         action.InlineComment.Path,
			Side:         action.InlineComment.Side,
			Line:         action.InlineComment.Line,
			SubjectType:  action.InlineComment.SubjectType,
			DiffPosition: action.InlineComment.DiffPosition,
		})
	case reviewplan.ActionKindThreadReply:
		if action.ThreadReply == nil {
			return nil, fmt.Errorf("plannedactions: thread reply payload missing")
		}
		return json.Marshal(outbox.ThreadReplyPayload{
			Body:    action.ThreadReply.Body,
			Summary: action.ThreadReply.Summary,
		})
	case reviewplan.ActionKindResolveThread:
		return json.Marshal(outbox.ResolveThreadPayload{})
	case reviewplan.ActionKindRollupComment:
		if action.RollupComment == nil {
			return nil, fmt.Errorf("plannedactions: rollup payload missing")
		}
		return json.Marshal(outbox.RollupCommentPayload{Body: action.RollupComment.Body})
	case reviewplan.ActionKindSubmitReview:
		if action.SubmitReview == nil {
			return nil, fmt.Errorf("plannedactions: submit review payload missing")
		}
		return json.Marshal(outbox.SubmitReviewPayload{
			Body:  action.SubmitReview.Body,
			Event: action.SubmitReview.Event,
		})
	default:
		return nil, fmt.Errorf("plannedactions: unknown action kind %q", action.Kind)
	}
}

// LedgerKind maps reviewplan action kinds into ledger kinds.
func LedgerKind(kind reviewplan.ActionKind) ledger.PlannedActionKind {
	switch kind {
	case reviewplan.ActionKindInlineComment:
		return ledger.PlannedActionInlineComment
	case reviewplan.ActionKindThreadReply:
		return ledger.PlannedActionThreadReply
	case reviewplan.ActionKindResolveThread:
		return ledger.PlannedActionResolveThread
	case reviewplan.ActionKindRollupComment:
		return ledger.PlannedActionRollupComment
	case reviewplan.ActionKindSubmitReview:
		return ledger.PlannedActionSubmitReview
	default:
		return ledger.PlannedActionKind(kind)
	}
}

// LedgerStatus maps reviewplan statuses into ledger statuses.
func LedgerStatus(status reviewplan.ActionStatus) ledger.PlannedActionStatus {
	switch status {
	case reviewplan.ActionStatusPending:
		return ledger.PlannedActionPending
	case reviewplan.ActionStatusPlannedOnly:
		return ledger.PlannedActionPlannedOnly
	default:
		return ledger.PlannedActionStatus(status)
	}
}
