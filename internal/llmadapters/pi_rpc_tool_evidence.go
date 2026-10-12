package llmadapters

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

const (
	piRPCTraceMaxCalls         = 256
	piRPCTraceMaxIdentityBytes = 64 * 1024
	piRPCTraceMaxCallIDBytes   = 256
	piRPCTraceMaxPathBytes     = 4096
)

type piRPCReviewerToolTrace struct {
	entries        []piRPCReviewerToolCall
	byID           map[string]int
	identityBytes  int
	droppedEvents  uint64
	truncated      bool
	streamComplete bool
}

type piRPCReviewerToolCall struct {
	evidence  llm.ReviewerToolCallEvidence
	ambiguous bool
}

func (trace *piRPCReviewerToolTrace) observe(event piRPCEvent) {
	if (!event.toolStarted && !event.toolCompleted) || (event.toolName != "cr_read" && event.toolName != "cr_diff") {
		return
	}
	id := event.toolCallID
	if len(id) > piRPCTraceMaxCallIDBytes {
		trace.truncated = true
		id = ""
	}
	if !utf8.ValidString(id) || strings.TrimSpace(id) == "" {
		id = ""
	}
	if index, exists := trace.byID[id]; id != "" && exists {
		entry := &trace.entries[index]
		if event.toolStarted || entry.evidence.EndObserved || entry.evidence.Tool != event.toolName {
			entry.ambiguous = true
			entry.evidence.Status = llm.ReviewerToolCallIncomplete
			entry.evidence.ProvenanceIssue = "ambiguous_call_id"
			entry.evidence.Output = nil
		}
		if event.toolCompleted {
			entry.evidence.EndObserved = true
			if !entry.ambiguous {
				finishPiRPCToolCall(&entry.evidence, event)
			}
		}
		return
	}
	if len(trace.entries) >= piRPCTraceMaxCalls || trace.identityBytes+len(id) > piRPCTraceMaxIdentityBytes {
		trace.droppedEvents++
		trace.truncated = true
		return
	}
	entry := piRPCReviewerToolCall{evidence: llm.ReviewerToolCallEvidence{
		CallID: id, Tool: event.toolName, Status: llm.ReviewerToolCallIncomplete,
		StartObserved: event.toolStarted, EndObserved: event.toolCompleted,
	}}
	if event.toolStarted {
		trace.addArguments(&entry.evidence, event.toolArgs)
	} else {
		entry.ambiguous = true
		entry.evidence.ProvenanceIssue = "missing_start"
	}
	if id == "" {
		entry.ambiguous = true
		entry.evidence.ProvenanceIssue = "call_id_unavailable"
	} else {
		if trace.byID == nil {
			trace.byID = make(map[string]int)
		}
		trace.byID[id] = len(trace.entries)
	}
	trace.identityBytes += len(id) + len(entry.evidence.Path)
	trace.entries = append(trace.entries, entry)
}

func (trace *piRPCReviewerToolTrace) addArguments(call *llm.ReviewerToolCallEvidence, raw json.RawMessage) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		call.ProvenanceIssue = "arguments_unavailable"
		return
	}
	// The helper decodes into a Go struct, which accepts case-fold aliases.
	// Do not attribute a default view or exact path/range from a map that could
	// disagree with the helper about Tool, View, Path, Offset, or Limit. A Tool
	// alias can even override the extension's lowercase tool field depending on
	// the original parameter insertion order.
	for key := range args {
		for _, canonical := range []string{"tool", "path", "view", "offset", "limit"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				call.ProvenanceIssue = "arguments_ambiguous"
				return
			}
		}
	}
	requestedRange := &llm.ReviewerToolRange{}
	for name, target := range map[string]*int64{"offset": &requestedRange.Offset, "limit": &requestedRange.Limit} {
		if value, present := args[name]; present {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil || *target < 0 {
				call.ProvenanceIssue = "range_unavailable"
				requestedRange = nil
				break
			}
		}
	}
	call.Range = requestedRange
	if call.Tool != "cr_read" {
		return // cr_diff is a fixed whole-diff artifact, never a per-file read.
	}
	call.ReadView = llm.ReviewerToolReadFile
	if rawView, present := args["view"]; present {
		view, exact := piRPCExactString(rawView)
		switch {
		case exact && view == "symlink":
			call.ReadView = llm.ReviewerToolReadSymlink
		case exact && view == "":
			// An empty view selects the helper's ordinary-file branch.
		default:
			call.ReadView = ""
			call.ProvenanceIssue = "read_view_unavailable"
		}
	}
	path, exact := piRPCExactString(args["path"])
	if !exact || !canonicalPiRPCEvidencePath(path) {
		call.ProvenanceIssue = "path_unavailable"
		return
	}
	if len(path) > piRPCTraceMaxPathBytes || trace.identityBytes+len(call.CallID)+len(path) > piRPCTraceMaxIdentityBytes {
		trace.truncated = true
		call.ProvenanceIssue = "path_omitted"
		return
	}
	call.Path = path
}

func canonicalPiRPCEvidencePath(path string) bool {
	// The helper cleans paths and trims whitespace. Attribute only an already
	// canonical argument so a normalization cannot silently name another file.
	if path == "" || strings.TrimSpace(path) != path || !utf8.ValidString(path) || strings.ContainsAny(path, "\\\x00") || strings.HasPrefix(path, "/") {
		return false
	}
	if len(path) >= 2 && path[1] == ':' {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		switch strings.ToLower(part) {
		case ".git", ".hg", ".svn":
			return false
		}
	}
	return true
}

func finishPiRPCToolCall(call *llm.ReviewerToolCallEvidence, event piRPCEvent) {
	if !event.toolOutcomeKnown {
		call.ProvenanceIssue = "outcome_unavailable"
		return
	}
	call.Status = llm.ReviewerToolCallSucceeded
	if event.toolFailed {
		call.Status = llm.ReviewerToolCallFailed
	}
	call.Output = event.toolOutput
}

func (trace *piRPCReviewerToolTrace) snapshot() *llm.ReviewerToolTrace {
	result := &llm.ReviewerToolTrace{
		Version: 1, Source: "pi_rpc", StreamComplete: trace.streamComplete,
		Truncated: trace.truncated, DroppedEvents: trace.droppedEvents,
		Calls: make([]llm.ReviewerToolCallEvidence, len(trace.entries)),
	}
	for i, entry := range trace.entries {
		result.Calls[i] = entry.evidence
		if !entry.evidence.EndObserved && entry.evidence.ProvenanceIssue == "" {
			result.Calls[i].ProvenanceIssue = "missing_end"
		}
	}
	return llm.CloneReviewerToolEvidence(&llm.ReviewerToolEvidence{Trace: result}).Trace
}

func piRPCBoolPresent(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false"))
}

func piRPCToolOutcomeKnown(event, result map[string]json.RawMessage, failed bool) bool {
	if failed {
		return true // A malformed field must never erase an explicit failure.
	}
	known := false
	for _, object := range []map[string]json.RawMessage{event, result} {
		if value, present := object["isError"]; present {
			if !piRPCBoolPresent(value) {
				return false
			}
			known = true
		}
	}
	return known
}

func piRPCToolCallID(raw map[string]json.RawMessage) string {
	var id string
	for _, key := range []string{"toolCallId", "tool_call_id"} {
		if value, present := raw[key]; present {
			decoded, exact := piRPCExactString(value)
			if !exact || decoded == "" || (id != "" && id != decoded) {
				return ""
			}
			id = decoded
		}
	}
	return id
}

// encoding/json replaces invalid UTF-8 and unpaired UTF-16 surrogates. Identity
// fields must reject those inputs rather than collide with a real U+FFFD name.
func piRPCExactString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	var decoded string
	if len(raw) < 2 || raw[0] != '"' || !utf8.Valid(raw) || json.Unmarshal(raw, &decoded) != nil {
		return "", false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return "", false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return "", false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+7 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return "", false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return "", false
		}
		i += 6
	}
	return decoded, true
}

func parsePiRPCToolOutput(raw json.RawMessage) *llm.ReviewerToolOutput {
	var details struct {
		Output *struct {
			Version   int    `json:"version"`
			Bytes     *int64 `json:"bytes"`
			Truncated *bool  `json:"truncated"`
		} `json:"codereview_tool_output"`
	}
	if json.Unmarshal(raw, &details) != nil || details.Output == nil {
		return nil
	}
	output := details.Output
	if output.Version != 1 || output.Bytes == nil || *output.Bytes < 0 || *output.Bytes > 1024*1024 || output.Truncated == nil {
		return nil
	}
	return &llm.ReviewerToolOutput{Bytes: *output.Bytes, Truncated: *output.Truncated}
}
