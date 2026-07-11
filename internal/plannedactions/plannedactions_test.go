package plannedactions_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	plannedactions "github.com/open-cli-collective/codereview-cli/internal/plannedactions/convert"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

func TestFromReviewPlanThreadReply(t *testing.T) {
	action := reviewplan.Action{
		ActionID:  "reply-1",
		Kind:      reviewplan.ActionKindThreadReply,
		ThreadID:  "thread-1",
		PlannedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Status:    reviewplan.ActionStatusPending,
		Required:  true,
		ThreadReply: &reviewplan.ThreadReplyPayload{
			Body:    "summary body",
			Summary: true,
		},
	}
	got, err := plannedactions.FromReviewPlan("run-1", action)
	if err != nil {
		t.Fatalf("FromReviewPlan: %v", err)
	}
	if got.Kind != ledger.PlannedActionThreadReply || got.Status != ledger.PlannedActionPending || !got.Required {
		t.Fatalf("planned action = %#v, want required pending thread reply", got)
	}
	if got.ThreadID == nil || *got.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %#v, want thread-1", got.ThreadID)
	}
	var payload outbox.ThreadReplyPayload
	if err := json.Unmarshal([]byte(got.PayloadJSON), &payload); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	if payload.Body != "summary body" || !payload.Summary {
		t.Fatalf("payload = %#v, want summary body", payload)
	}
}

func TestPayloadRejectsMissingThreadReplyPayload(t *testing.T) {
	_, err := plannedactions.Payload(reviewplan.Action{Kind: reviewplan.ActionKindThreadReply})
	if err == nil {
		t.Fatal("Payload error = nil, want missing payload error")
	}
}
