package reviewplan

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// Minor/nit-only findings must approve even when the rollup model proposes a
// comment — non-blocking, non-major findings are suggestions, not gates.
func TestEffectiveReviewEventMinorOnlyApproves(t *testing.T) {
	minorOnly := []review.Finding{
		{Severity: review.SeverityMinor},
		{Severity: review.SeverityNits},
	}
	cases := []struct {
		name   string
		rollup review.ReviewEvent
	}{
		{"rollup proposed comment", review.ReviewEventComment},
		{"rollup proposed request-changes", review.ReviewEventRequestChanges},
		{"rollup unset", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveReviewEvent(tc.rollup, minorOnly, EventOptions{})
			if got != review.ReviewEventApprove {
				t.Fatalf("got %q, want approve", got)
			}
		})
	}
}

// Blocking/major findings still govern: the clamp only forces approval, it never
// downgrades a genuine blocking/major signal.
func TestEffectiveReviewEventRespectsBlockingAndMajor(t *testing.T) {
	withMajor := []review.Finding{{Severity: review.SeverityMinor}, {Severity: review.SeverityMajor}}
	if got := effectiveReviewEvent(review.ReviewEventComment, withMajor, EventOptions{}); got != review.ReviewEventComment {
		t.Fatalf("major present: got %q, want comment", got)
	}
	withBlocking := []review.Finding{{Severity: review.SeverityBlocking}}
	if got := effectiveReviewEvent("", withBlocking, EventOptions{}); got != review.ReviewEventRequestChanges {
		t.Fatalf("blocking present: got %q, want request-changes", got)
	}
	// An empty diff with no findings approves.
	if got := effectiveReviewEvent("", nil, EventOptions{}); got != review.ReviewEventApprove {
		t.Fatalf("no findings: got %q, want approve", got)
	}
}
