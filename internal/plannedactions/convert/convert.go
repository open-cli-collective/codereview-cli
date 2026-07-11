// Package convert translates planner actions into their current ledger representation.
package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// FromReviewPlan converts one planner action into a ledger planned action.
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
		return json.Marshal(action.InlineComment)
	case reviewplan.ActionKindThreadReply:
		if action.ThreadReply == nil {
			return nil, fmt.Errorf("plannedactions: thread reply payload missing")
		}
		return json.Marshal(action.ThreadReply)
	case reviewplan.ActionKindResolveThread:
		return json.Marshal(reviewplan.ResolveThreadPayload{})
	case reviewplan.ActionKindRollupComment:
		if action.RollupComment == nil {
			return nil, fmt.Errorf("plannedactions: rollup payload missing")
		}
		return json.Marshal(action.RollupComment)
	case reviewplan.ActionKindSubmitReview:
		if action.SubmitReview == nil {
			return nil, fmt.Errorf("plannedactions: submit review payload missing")
		}
		return json.Marshal(action.SubmitReview)
	default:
		return nil, fmt.Errorf("plannedactions: unknown action kind %q", action.Kind)
	}
}

// LedgerKind maps a planner action kind into its ledger alias.
func LedgerKind(kind reviewplan.ActionKind) ledger.PlannedActionKind { return kind }

// LedgerStatus maps a planner action status into its ledger alias.
func LedgerStatus(status reviewplan.ActionStatus) ledger.PlannedActionStatus { return status }
