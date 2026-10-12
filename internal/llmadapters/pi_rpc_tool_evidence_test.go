package llmadapters

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestPiRPCTraceCorrelatesInterleavedReadAndDiffCalls(t *testing.T) {
	var trace piRPCReviewerToolTrace
	observePiTraceJSON(t, &trace,
		`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"cr_read","args":{"path":"src/世界 file.go","offset":10,"limit":20}}`,
		`{"type":"tool_execution_start","toolCallId":"diff-1","toolName":"cr_diff","args":{"offset":30,"limit":40}}`,
		`{"type":"tool_execution_end","toolCallId":"diff-1","toolName":"cr_diff","isError":false,"result":{"details":{"codereview_tool_output":{"version":1,"bytes":40,"truncated":true}}}}`,
		`{"type":"tool_execution_end","toolCallId":"read-1","toolName":"cr_read","isError":false,"result":{"content":[{"type":"text","text":"private source text"}],"details":{"codereview_tool_output":{"version":1,"bytes":20,"truncated":false}}}}`,
		`{"type":"tool_execution_start","toolCallId":"read-2","toolName":"cr_read","args":{"path":"src/世界 file.go"}}`,
		`{"type":"tool_execution_end","toolCallId":"read-2","toolName":"cr_read","result":{"isError":true,"content":[{"type":"text","text":"private failure text"}]}}`,
	)
	trace.streamComplete = true
	got := trace.snapshot()
	want := &llm.ReviewerToolTrace{Version: 1, Source: "pi_rpc", StreamComplete: true, Calls: []llm.ReviewerToolCallEvidence{
		{CallID: "read-1", Tool: "cr_read", Path: "src/世界 file.go", ReadView: llm.ReviewerToolReadFile, Status: llm.ReviewerToolCallSucceeded, StartObserved: true, EndObserved: true, Range: &llm.ReviewerToolRange{Offset: 10, Limit: 20}, Output: &llm.ReviewerToolOutput{Bytes: 20}},
		{CallID: "diff-1", Tool: "cr_diff", Status: llm.ReviewerToolCallSucceeded, StartObserved: true, EndObserved: true, Range: &llm.ReviewerToolRange{Offset: 30, Limit: 40}, Output: &llm.ReviewerToolOutput{Bytes: 40, Truncated: true}},
		{CallID: "read-2", Tool: "cr_read", Path: "src/世界 file.go", ReadView: llm.ReviewerToolReadFile, Status: llm.ReviewerToolCallFailed, StartObserved: true, EndObserved: true, Range: &llm.ReviewerToolRange{}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace = %#v, want %#v", got, want)
	}
	data, err := json.Marshal(got)
	if err != nil || strings.Contains(string(data), "private") {
		t.Fatalf("trace leaked tool output: %s, err=%v", data, err)
	}
	got.Calls[0].Range.Offset = 999
	got.Calls[0].Output.Bytes = 999
	if !reflect.DeepEqual(trace.snapshot(), want) {
		t.Fatal("snapshot mutation changed collected trace")
	}
}

func TestPiRPCTraceDoesNotInferMissingOrAmbiguousExecution(t *testing.T) {
	start := `{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":{"path":"file.go"}}`
	end := `{"type":"tool_execution_end","toolCallId":"call","toolName":"cr_read","isError":false}`
	for _, tt := range []struct {
		name  string
		lines []string
		issue string
	}{
		{"start only", []string{start}, "missing_end"},
		{"end only", []string{end}, "missing_start"},
		{"end before start", []string{end, start}, "ambiguous_call_id"},
		{"duplicate start", []string{start, start, end}, "ambiguous_call_id"},
		{"duplicate end", []string{start, end, end}, "ambiguous_call_id"},
		{"mismatched tool", []string{start, strings.ReplaceAll(end, "cr_read", "cr_diff")}, "ambiguous_call_id"},
		{"missing outcome", []string{start, strings.ReplaceAll(end, `,"isError":false`, "")}, "outcome_unavailable"},
		{"missing identity", []string{strings.ReplaceAll(start, `"toolCallId":"call",`, "")}, "call_id_unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var trace piRPCReviewerToolTrace
			observePiTraceJSON(t, &trace, tt.lines...)
			got := trace.snapshot()
			if len(got.Calls) != 1 || got.Calls[0].Status != llm.ReviewerToolCallIncomplete || got.Calls[0].ProvenanceIssue != tt.issue {
				t.Fatalf("calls = %#v, want incomplete with %s", got.Calls, tt.issue)
			}
		})
	}
}

func TestPiRPCTraceIgnoresModelClaimsAndUnattributedDiffPaths(t *testing.T) {
	var trace piRPCReviewerToolTrace
	observePiTraceJSON(t, &trace,
		`{"type":"toolcall_start","toolCallId":"claim","toolName":"cr_read","args":{"path":"claim.go"}}`,
		`{"type":"tool_execution_start","toolCallId":"search","toolName":"cr_search","args":{"path":"search.go","query":"private query"}}`,
		`{"type":"tool_execution_end","toolCallId":"search","toolName":"cr_search","isError":false}`,
		`{"type":"tool_execution_start","toolCallId":"diff","toolName":"cr_diff","args":{"path":"claim.go"}}`,
		`{"type":"tool_execution_end","toolCallId":"diff","toolName":"cr_diff","isError":false,"result":{"content":[{"type":"text","text":"[cr-range offset=0 end=100 total=100 next_offset=-1]\n"}]}}`,
	)
	got := trace.snapshot()
	if len(got.Calls) != 1 || got.Calls[0].Tool != "cr_diff" || got.Calls[0].Path != "" || got.Calls[0].Output != nil {
		t.Fatalf("calls = %#v, want one pathless diff with unknown output bounds", got.Calls)
	}
	if got.StreamComplete {
		t.Fatal("a tool end must not establish that the stream ended cleanly")
	}
}

func TestPiRPCTraceDistinguishesSymlinkMetadataFromRegularBodyReads(t *testing.T) {
	stream := &piRPCStream{allowReviewerTools: true}
	for _, line := range []string{
		`{"type":"tool_execution_start","toolCallId":"metadata","toolName":"cr_read","args":{"path":"changed-link","view":"symlink","offset":20,"limit":40}}`,
		`{"type":"tool_execution_end","toolCallId":"metadata","toolName":"cr_read","isError":false,"result":{"content":[{"type":"text","text":"{\"payload_omitted_reason\":\"oversize\",\"resolved_path\":\"private-target\"}"}]}}`,
		`{"type":"tool_execution_start","toolCallId":"body","toolName":"cr_read","args":{"path":"changed-link"}}`,
		`{"type":"tool_execution_end","toolCallId":"body","toolName":"cr_read","isError":false}`,
	} {
		event, err := parsePiRPCEvent([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		stream.observeReviewerToolEvent(event)
	}
	got := stream.reviewerToolEvidence()
	if got.DiffStatus != llm.DiffToolStatusNotInvoked {
		t.Fatal("successful metadata/body reads must not satisfy the existing required-diff gate")
	}
	calls := got.Trace.Calls
	if len(calls) != 2 || calls[0].Path != "changed-link" || calls[1].Path != "changed-link" || calls[0].ReadView != llm.ReviewerToolReadSymlink || calls[1].ReadView != llm.ReviewerToolReadFile {
		t.Fatalf("calls = %#v, want separate metadata and file views for the same path", calls)
	}
	if calls[0].Range == nil || calls[0].Range.Offset != 20 || calls[0].Range.Limit != 40 || calls[0].Status != llm.ReviewerToolCallSucceeded || calls[0].Output != nil {
		t.Fatalf("metadata call = %#v, want observed success with requested metadata range and unknown transport bounds", calls[0])
	}
	data, err := json.Marshal(got.Trace)
	if err != nil || strings.Contains(string(data), "private-target") || strings.Contains(string(data), "payload_omitted_reason") {
		t.Fatalf("trace copied symlink artifact content: %s, err %v", data, err)
	}
}

func TestPiRPCTraceDoesNotAssumeAnInvalidViewIsAFileRead(t *testing.T) {
	for _, rawView := range []string{`null`, `false`, `1`, `"unknown"`, `"symlink "`, `"\ud800"`} {
		var trace piRPCReviewerToolTrace
		observePiTraceJSON(t, &trace,
			`{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":{"path":"file.go","view":`+rawView+`}}`,
			`{"type":"tool_execution_end","toolCallId":"call","toolName":"cr_read","isError":true}`,
		)
		call := trace.snapshot().Calls[0]
		if call.ReadView != "" || call.ProvenanceIssue != "read_view_unavailable" || call.Status != llm.ReviewerToolCallFailed {
			t.Fatalf("view %s became a file read: %#v", rawView, call)
		}
	}
	var trace piRPCReviewerToolTrace
	observePiTraceJSON(t, &trace, `{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":{"path":"file.go","view":""}}`)
	if call := trace.snapshot().Calls[0]; call.ReadView != llm.ReviewerToolReadFile {
		t.Fatalf("empty helper view = %#v, want ordinary file request", call)
	}
}

func TestPiRPCTraceRejectsCaseFoldArgumentAliasesBeforeAttribution(t *testing.T) {
	for _, args := range []string{
		`{"tool":"ignored","Tool":"cr_diff","path":"file.go"}`,
		`{"Tool":"cr_diff","path":"file.go"}`,
		`{"path":"changed-link","View":"symlink"}`,
		`{"path":"changed-link","view":"","VIEW":"symlink"}`,
		`{"path":"file.go","Path":"different.go"}`,
		`{"Path":"different.go"}`,
		`{"path":"file.go","Offset":100}`,
		`{"path":"file.go","limit":100,"LIMIT":1}`,
		`{"path":"file.go","offſet":100}`,
	} {
		var trace piRPCReviewerToolTrace
		observePiTraceJSON(t, &trace,
			`{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":`+args+`}`,
			`{"type":"tool_execution_end","toolCallId":"call","toolName":"cr_read","isError":false}`,
		)
		call := trace.snapshot().Calls[0]
		if call.Path != "" || call.ReadView != "" || call.Range != nil || call.ProvenanceIssue != "arguments_ambiguous" {
			t.Fatalf("case-fold arguments %s received attribution: %#v", args, call)
		}
	}
}

func TestPiRPCTraceDoesNotNormalizeOrPersistUnsafePathArguments(t *testing.T) {
	for _, path := range []string{"", "/private/file", "../file", "src/../file", "./file", "src//file", ".git/config", "src/.GIT/config", ".hg/config", "src/.HG/config", ".svn/config", "src/.SVN/config", "C:/private/file", `C:\private\file`, " file.go", "file.go ", "file\x00.go"} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			var trace piRPCReviewerToolTrace
			args, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: "read", toolStarted: true, toolArgs: args})
			trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: "read", toolCompleted: true, toolOutcomeKnown: true})
			call := trace.snapshot().Calls[0]
			if call.Path != "" || call.ProvenanceIssue != "path_unavailable" {
				t.Fatalf("call = %#v, want unattributed outcome", call)
			}
		})
	}
}

func TestPiRPCTraceBoundsCallsAndRetainsCompletionOfEarlierCalls(t *testing.T) {
	var trace piRPCReviewerToolTrace
	for i := 0; i < piRPCTraceMaxCalls+1; i++ {
		trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: fmt.Sprintf("call-%d", i), toolStarted: true, toolArgs: json.RawMessage(`{"path":"file.go"}`)})
	}
	trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: "call-0", toolCompleted: true, toolOutcomeKnown: true})
	trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: fmt.Sprintf("call-%d", piRPCTraceMaxCalls), toolCompleted: true, toolOutcomeKnown: true})
	got := trace.snapshot()
	if len(got.Calls) != piRPCTraceMaxCalls || !got.Truncated || got.DroppedEvents != 2 || got.Calls[0].Status != llm.ReviewerToolCallSucceeded || len(trace.byID) != piRPCTraceMaxCalls {
		t.Fatalf("trace bounds = calls %d, dropped %d, truncated %t, first %#v, identities %d", len(got.Calls), got.DroppedEvents, got.Truncated, got.Calls[0], len(trace.byID))
	}
}

func TestPiRPCTraceBoundsIdentityBytesWithoutTruncatingIdentity(t *testing.T) {
	var trace piRPCReviewerToolTrace
	for i := 0; i < 40; i++ {
		args, err := json.Marshal(map[string]string{"path": strings.Repeat("x", piRPCTraceMaxPathBytes)})
		if err != nil {
			t.Fatal(err)
		}
		trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: fmt.Sprintf("read-%d", i), toolStarted: true, toolArgs: args})
	}
	if got := trace.snapshot(); !got.Truncated || trace.identityBytes > piRPCTraceMaxIdentityBytes {
		t.Fatalf("trace = %#v, identity bytes %d", got, trace.identityBytes)
	}
	last := trace.snapshot().Calls[39]
	if last.Path != "" || last.ProvenanceIssue != "path_omitted" {
		t.Fatalf("last = %#v, want omitted path rather than a different truncated path", last)
	}
	trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: strings.Repeat("i", piRPCTraceMaxCallIDBytes+1), toolStarted: true, toolArgs: json.RawMessage(`{"path":"small.go"}`)})
	last = trace.snapshot().Calls[40]
	if last.CallID != "" || last.ProvenanceIssue != "call_id_unavailable" {
		t.Fatalf("oversized identity = %#v, want unavailable", last)
	}
}

func TestPiRPCTraceMarksMalformedArgumentsAndUnknownOutputBounds(t *testing.T) {
	for _, args := range []string{`null`, `[]`, `{}`, `{"path":"file.go","offset":null}`, `{"path":"file.go","limit":-1}`, `{"path":"file.go","offset":"0"}`} {
		var trace piRPCReviewerToolTrace
		trace.observe(piRPCEvent{toolName: "cr_read", toolCallID: "read", toolStarted: true, toolArgs: json.RawMessage(args)})
		if call := trace.snapshot().Calls[0]; call.ProvenanceIssue == "" {
			t.Fatalf("args %s unexpectedly have complete provenance: %#v", args, call)
		}
	}
	for _, details := range []string{`null`, `{}`, `{"codereview_tool_output":{"version":2,"bytes":1,"truncated":false}}`, `{"codereview_tool_output":{"version":1,"bytes":-1,"truncated":false}}`, `{"codereview_tool_output":{"version":1,"bytes":1}}`} {
		if got := parsePiRPCToolOutput(json.RawMessage(details)); got != nil {
			t.Fatalf("details %s = %#v, want unknown", details, got)
		}
	}
}

func TestPiRPCTraceDoesNotChangeLegacyDiffGateOrUnsupportedEvidence(t *testing.T) {
	stream := &piRPCStream{allowReviewerTools: true}
	stream.observeReviewerToolEvent(piRPCEvent{toolName: "cr_diff", toolCompleted: true})
	got := stream.reviewerToolEvidence()
	if got.DiffStatus != llm.DiffToolStatusSucceeded || got.Trace.Calls[0].Status != llm.ReviewerToolCallIncomplete {
		t.Fatalf("evidence = %#v, want unchanged legacy gate and conservative new trace", got)
	}
	unsupported := &piRPCStream{}
	unsupported.observeReviewerToolEvent(piRPCEvent{toolName: "cr_read", toolStarted: true})
	if unsupported.reviewerToolEvidence() != nil {
		t.Fatal("non-reviewer invocation must not claim trace support")
	}
}

func TestPiRPCTraceRejectsNormalizedIdentityCollisions(t *testing.T) {
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"x\ud800y"`, `"\ud800\u0041"`, "\"bad\xff\""} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			if decoded, exact := piRPCExactString(json.RawMessage(raw)); exact {
				t.Fatalf("identity %s normalized to %q", raw, decoded)
			}
			var trace piRPCReviewerToolTrace
			observePiTraceJSON(t, &trace,
				`{"type":"tool_execution_start","toolCallId":`+raw+`,"toolName":"cr_read","args":{"path":"file.go"}}`,
				`{"type":"tool_execution_end","toolCallId":"�","toolName":"cr_read","isError":false}`,
			)
			for _, call := range trace.snapshot().Calls {
				if call.Status == llm.ReviewerToolCallSucceeded {
					t.Fatalf("malformed identity established success: %#v", call)
				}
			}
			var pathTrace piRPCReviewerToolTrace
			observePiTraceJSON(t, &pathTrace, `{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":{"path":`+raw+`}}`)
			if call := pathTrace.snapshot().Calls[0]; call.Path != "" || call.ProvenanceIssue != "path_unavailable" {
				t.Fatalf("malformed path established identity: %#v", call)
			}
		})
	}
	for raw, want := range map[string]string{`"�"`: "�", `"\ufffd"`: "�", `"\ud83d\ude00"`: "😀", `"\\ud800"`: `\ud800`} {
		if got, exact := piRPCExactString(json.RawMessage(raw)); !exact || got != want {
			t.Fatalf("valid identity %s = %q, exact %t, want %q", raw, got, exact, want)
		}
	}
}

func TestPiRPCTraceRejectsMalformedOutcomeAndConflictingIDAliases(t *testing.T) {
	for _, raw := range []string{`"false"`, `null`, `0`} {
		var trace piRPCReviewerToolTrace
		observePiTraceJSON(t, &trace,
			`{"type":"tool_execution_start","toolCallId":"call","toolName":"cr_read","args":{"path":"file.go"}}`,
			`{"type":"tool_execution_end","toolCallId":"call","toolName":"cr_read","isError":`+raw+`,"result":{"isError":false}}`,
		)
		if got := trace.snapshot().Calls[0]; got.Status != llm.ReviewerToolCallIncomplete || got.ProvenanceIssue != "outcome_unavailable" {
			t.Fatalf("malformed outcome %s = %#v", raw, got)
		}
	}
	event, err := parsePiRPCEvent([]byte(`{"type":"tool_execution_end","toolCallId":"call","tool_call_id":"different","toolName":"cr_read","isError":"malformed","result":{"isError":true}}`))
	if err != nil || event.toolCallID != "" || !event.toolOutcomeKnown || !event.toolFailed {
		t.Fatalf("event = %#v, err %v, want missing identity and explicit failure", event, err)
	}
}

func observePiTraceJSON(t *testing.T, trace *piRPCReviewerToolTrace, lines ...string) {
	t.Helper()
	for _, line := range lines {
		event, err := parsePiRPCEvent([]byte(line))
		if err != nil {
			t.Fatalf("parsePiRPCEvent: %v", err)
		}
		trace.observe(event)
	}
}
