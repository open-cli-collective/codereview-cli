package reviewplan

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestEffectiveReviewEvent(t *testing.T) {
	blocking := []review.Finding{{Severity: review.SeverityMinor}, {Severity: review.SeverityBlocking}}
	major := []review.Finding{{Severity: review.SeverityMinor}, {Severity: review.SeverityMajor}}
	minorOnly := []review.Finding{{Severity: review.SeverityMinor}, {Severity: review.SeverityNits}}
	commentPolicy := EventOptions{}
	requestChangesPolicy := EventOptions{MajorEventRequestsChanges: true}
	selfAuthored := EventOptions{PostingIdentityIsPRAuthor: true}

	const (
		unset          = review.ReviewEvent("")
		approve        = review.ReviewEventApprove
		comment        = review.ReviewEventComment
		requestChanges = review.ReviewEventRequestChanges
	)
	cases := []struct {
		name     string
		findings []review.Finding
		opts     EventOptions
		proposed review.ReviewEvent
		want     review.ReviewEvent
	}{
		{"blocking unset", blocking, commentPolicy, unset, requestChanges},
		{"blocking approve", blocking, commentPolicy, approve, requestChanges},
		{"blocking comment", blocking, commentPolicy, comment, requestChanges},
		{"blocking comment under request_changes policy", blocking, requestChangesPolicy, comment, requestChanges},

		{"major unset under comment policy", major, commentPolicy, unset, comment},
		{"major approve under comment policy", major, commentPolicy, approve, comment},
		{"major comment under comment policy", major, commentPolicy, comment, comment},
		{"major request_changes under comment policy", major, commentPolicy, requestChanges, comment},

		{"major unset under request_changes policy", major, requestChangesPolicy, unset, requestChanges},
		{"major approve under request_changes policy", major, requestChangesPolicy, approve, requestChanges},
		{"major comment under request_changes policy", major, requestChangesPolicy, comment, comment},
		{"major request_changes under request_changes policy", major, requestChangesPolicy, requestChanges, requestChanges},

		{"minor only unset", minorOnly, commentPolicy, unset, approve},
		{"minor only comment", minorOnly, commentPolicy, comment, approve},
		{"minor only request_changes", minorOnly, commentPolicy, requestChanges, approve},
		{"minor only request_changes when self-authored", minorOnly, selfAuthored, requestChanges, comment},

		{"no findings", nil, commentPolicy, unset, approve},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveReviewEvent(tc.proposed, tc.findings, tc.opts); got != tc.want {
				t.Fatalf("effectiveReviewEvent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildValidatesProposedRollupEvent(t *testing.T) {
	unknown := summaryRequest()
	unknown.Rollup.ReviewEvent = "merge"
	if _, err := Build(unknown); err == nil || !strings.Contains(err.Error(), "invalid rollup review event") {
		t.Fatalf("Build with unknown proposal error = %v, want invalid rollup review event", err)
	}

	unset := summaryRequest()
	unset.Rollup.ReviewEvent = ""
	if _, err := Build(unset); err != nil {
		t.Fatalf("Build with no proposal: %v", err)
	}
}
