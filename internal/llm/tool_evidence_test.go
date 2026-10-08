package llm

import (
	"reflect"
	"testing"
)

func TestCloneResponseSnapshotsReviewerToolTrace(t *testing.T) {
	evidence := &ReviewerToolEvidence{DiffStatus: DiffToolStatusSucceeded, Trace: &ReviewerToolTrace{
		Version: 1, Source: "pi_rpc", Calls: []ReviewerToolCallEvidence{{
			CallID: "read", Tool: "cr_read", Path: "src/file.go", Status: ReviewerToolCallSucceeded,
			Range: &ReviewerToolRange{Offset: 20, Limit: 50}, Output: &ReviewerToolOutput{Bytes: 50},
		}},
	}}
	cloned := cloneResponse(Response{ReviewerToolEvidence: evidence}).ReviewerToolEvidence
	if !reflect.DeepEqual(cloned, evidence) {
		t.Fatalf("clone = %#v, want %#v", cloned, evidence)
	}
	cloned.DiffStatus = DiffToolStatusFailed
	cloned.Trace.Truncated = true
	cloned.Trace.Calls[0].Path = "different.go"
	cloned.Trace.Calls[0].Range.Offset = 100
	cloned.Trace.Calls[0].Output.Truncated = true
	if evidence.DiffStatus != DiffToolStatusSucceeded || evidence.Trace.Truncated || evidence.Trace.Calls[0].Path != "src/file.go" || evidence.Trace.Calls[0].Range.Offset != 20 || evidence.Trace.Calls[0].Output.Truncated {
		t.Fatalf("clone mutated original: %#v", evidence)
	}
	if CloneReviewerToolEvidence(nil) != nil {
		t.Fatal("absent evidence must stay absent")
	}
}
