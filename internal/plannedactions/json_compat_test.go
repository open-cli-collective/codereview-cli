package plannedactions

import (
	"encoding/json"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestPayloadJSONCompatibility(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{
			name: "inline comment",
			payload: InlineCommentPayload{
				Body:         "inline body",
				Path:         "internal/example.go",
				Side:         review.DiffSideRight,
				Line:         42,
				SubjectType:  review.AnchorKindLine,
				DiffPosition: 7,
			},
			want: `{"body":"inline body","path":"internal/example.go","side":"RIGHT","line":42,"subject_type":"line","diff_position":7}`,
		},
		{
			name:    "thread reply",
			payload: ThreadReplyPayload{Body: "reply body", Summary: true},
			want:    `{"body":"reply body","summary":true}`,
		},
		{
			name:    "resolve thread",
			payload: ResolveThreadPayload{},
			want:    `{}`,
		},
		{
			name:    "rollup comment",
			payload: RollupCommentPayload{Body: "rollup body"},
			want:    `{"body":"rollup body"}`,
		},
		{
			name:    "submit review",
			payload: SubmitReviewPayload{Body: "review body", Event: review.ReviewEventRequestChanges},
			want:    `{"body":"review body","event":"request_changes"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("JSON = %s, want %s", got, tt.want)
			}
		})
	}
}
