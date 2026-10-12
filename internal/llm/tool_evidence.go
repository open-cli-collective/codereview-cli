package llm

// ReviewerToolTrace is a bounded record of execution events from one adapter
// invocation. It is telemetry, not proof of meaningful inspection or coverage.
// A nil trace means this adapter did not provide the per-call contract.
type ReviewerToolTrace struct {
	Version        int                        `json:"version"`
	Source         string                     `json:"source"`
	StreamComplete bool                       `json:"stream_complete"`
	Truncated      bool                       `json:"truncated"`
	DroppedEvents  uint64                     `json:"dropped_events"`
	Calls          []ReviewerToolCallEvidence `json:"calls"`
}

// ReviewerToolCallStatus describes an observed, correlated execution outcome.
type ReviewerToolCallStatus string

const (
	// ReviewerToolCallIncomplete means the trace cannot establish an outcome.
	ReviewerToolCallIncomplete ReviewerToolCallStatus = "incomplete"
	// ReviewerToolCallSucceeded means a matched execution end explicitly succeeded.
	ReviewerToolCallSucceeded ReviewerToolCallStatus = "succeeded"
	// ReviewerToolCallFailed means a matched execution end explicitly failed.
	ReviewerToolCallFailed ReviewerToolCallStatus = "failed"
)

// ReviewerToolCallEvidence retains only identity, request range and outcome.
// Path is the unmodified, canonical repository-relative cr_read argument, not
// an assignment or a claim that the entire file was read. cr_diff has no path.
// Missing/ambiguous observations are explicit; raw arguments and output are
// deliberately not copied into metadata.
type ReviewerToolCallEvidence struct {
	CallID          string                 `json:"call_id,omitempty"`
	Tool            string                 `json:"tool"`
	Path            string                 `json:"path,omitempty"`
	ReadView        ReviewerToolReadView   `json:"read_view,omitempty"`
	Status          ReviewerToolCallStatus `json:"status"`
	StartObserved   bool                   `json:"start_observed"`
	EndObserved     bool                   `json:"end_observed"`
	ProvenanceIssue string                 `json:"provenance_issue,omitempty"`
	Range           *ReviewerToolRange     `json:"requested_range,omitempty"`
	Output          *ReviewerToolOutput    `json:"output,omitempty"`
}

// ReviewerToolReadView distinguishes ordinary file-body requests from pinned
// symlink metadata. Neither view establishes complete or meaningful inspection.
type ReviewerToolReadView string

const (
	// ReviewerToolReadFile is an ordinary cr_read request without a special view.
	ReviewerToolReadFile ReviewerToolReadView = "file"
	// ReviewerToolReadSymlink is a cr_read view=symlink request. Its ranges cover
	// pinned metadata, not the symlink destination or a head regular-file body.
	ReviewerToolReadSymlink ReviewerToolReadView = "symlink"
)

// ReviewerToolRange is the requested byte range. Zero values select the tool's
// default bounded read; they do not establish that all bytes were returned.
type ReviewerToolRange struct {
	Offset int64 `json:"offset"`
	Limit  int64 `json:"limit"`
}

// ReviewerToolOutput describes transport bounds reported by the CR extension.
// A nil output means unknown, not untruncated. Even Truncated=false says nothing
// about whether the helper returned only a range of the underlying file/diff.
type ReviewerToolOutput struct {
	Bytes     int64 `json:"bytes"`
	Truncated bool  `json:"truncated"`
}

// CloneReviewerToolEvidence prevents mutable traces from crossing attempt or
// lifecycle snapshot boundaries.
func CloneReviewerToolEvidence(evidence *ReviewerToolEvidence) *ReviewerToolEvidence {
	if evidence == nil {
		return nil
	}
	cloned := *evidence
	if evidence.Trace != nil {
		trace := *evidence.Trace
		trace.Calls = append([]ReviewerToolCallEvidence{}, evidence.Trace.Calls...)
		for i := range trace.Calls {
			if value := trace.Calls[i].Range; value != nil {
				copiedRange := *value
				trace.Calls[i].Range = &copiedRange
			}
			if value := trace.Calls[i].Output; value != nil {
				copiedOutput := *value
				trace.Calls[i].Output = &copiedOutput
			}
		}
		cloned.Trace = &trace
	}
	return &cloned
}
