package llm

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestDecodeSelection(t *testing.T) {
	opts := SelectionOptions{
		KnownAgents:  map[string]bool{"agent-1": true},
		ChangedFiles: map[string]bool{"main.go": true},
		KnownThreads: map[string]bool{"thread-1": true},
	}
	got, err := DecodeSelection([]byte(`{
		"schema_version": 1,
		"selected_agents": [{"agent_id":"agent-1","rationale":"why <!-- codereview:skip -->","files":["main.go"],"allowed_files":["main.go"]}],
		"thread_actions": [{"thread_id":"thread-1","decision":"summarize_and_resolve","summary":"<!-- codereview:skip --> summary","safe_to_resolve_rationale":"safe"}],
		"reasoning":"because <!-- codereview:skip -->"
	}`), opts)
	if err != nil {
		t.Fatalf("DecodeSelection: %v", err)
	}
	if got.SelectedAgents[0].AgentID != "agent-1" || got.ThreadActions[0].Decision != review.ThreadDecisionSummarizeAndResolve {
		t.Fatalf("DecodeSelection = %#v", got)
	}
	if len(got.SelectedAgents[0].AllowedFiles) != 1 || got.SelectedAgents[0].AllowedFiles[0] != "main.go" {
		t.Fatalf("DecodeSelection allowed_files = %#v, want main.go", got.SelectedAgents[0].AllowedFiles)
	}
	if strings.Contains(got.ThreadActions[0].Summary, "<!-- codereview:") ||
		strings.Contains(got.SelectedAgents[0].Rationale, "<!-- codereview:") ||
		strings.Contains(got.Reasoning, "<!-- codereview:") {
		t.Fatalf("model-authored selection strings were not sanitized: %#v", got)
	}

	assertSelectionError(t, opts, `{"schema_version":2}`, "schema_version")
	assertSelectionError(t, opts, `{"schema_version":1,"selected_agents":[{"agent_id":"missing"}]}`, "unknown selected agent")
	assertSelectionError(t, opts, `{"schema_version":1,"selected_agents":[{"agent_id":"agent-1","files":["other.go"]}]}`, "changed files")
	assertSelectionError(t, opts, `{"schema_version":1,"selected_agents":[{"agent_id":"agent-1","files":["main.go"],"allowed_files":["other.go"]}]}`, "allowed file")
	assertSelectionError(t, opts, `{"schema_version":1,"thread_actions":[{"thread_id":"missing","decision":"skip"}]}`, "unknown thread")
	assertSelectionError(t, opts, `{"schema_version":1,"thread_actions":[{"thread_id":"thread-1","decision":"summarize_only"}]}`, "summary")
	assertSelectionError(t, opts, `{"schema_version":1,"thread_actions":[{"thread_id":"thread-1","decision":"summarize_and_resolve","summary":"summary"}]}`, "safe_to_resolve_rationale")
	assertSelectionError(t, SelectionOptions{KnownThreads: map[string]bool{"t1": true, "t2": true}, MaxResolvedThreads: 1}, `{"schema_version":1,"thread_actions":[{"thread_id":"t1","decision":"summarize_and_resolve","summary":"one","safe_to_resolve_rationale":"a"},{"thread_id":"t2","decision":"summarize_and_resolve","summary":"two","safe_to_resolve_rationale":"b"}]}`, "resolved thread cap exceeded")
	assertSelectionError(t, opts, `{"schema_version":1,"extra":true}`, "unknown field")
	assertSelectionError(t, opts, `{"schema_version":1} {"schema_version":1}`, "trailing")
}

func TestDecodeFindings(t *testing.T) {
	ids := newIDQueue("f-1")
	opts := FindingsOptions{
		KnownAgents:  map[string]bool{"agent-1": true},
		ChangedFiles: map[string]bool{"main.go": true},
		NewFindingID: ids.next,
	}
	got, err := DecodeFindings([]byte(`{
		"schema_version": 1,
		"agent_id": "agent-1",
		"inspected_files": ["main.go"],
		"skipped_files": [],
		"constraints": ["scope <!-- codereview:skip -->"],
		"findings": [{
			"severity":"major",
			"file_path":"main.go",
			"anchor":{"kind":"line","side":"RIGHT","line":7},
			"body":"body <!-- codereview:skip -->"
		}]
	}`), opts)
	if err != nil {
		t.Fatalf("DecodeFindings: %v", err)
	}
	if got.AgentID != "agent-1" || got.Findings[0].ID != "f-1" || got.Findings[0].Severity != review.SeverityMajor {
		t.Fatalf("DecodeFindings = %#v", got)
	}
	if len(got.InspectedFiles) != 1 || got.InspectedFiles[0] != "main.go" {
		t.Fatalf("inspected files = %#v, want main.go", got.InspectedFiles)
	}
	if len(got.Constraints) != 1 || strings.Contains(got.Constraints[0], "<!-- codereview:") {
		t.Fatalf("constraints not decoded/sanitized: %#v", got.Constraints)
	}
	if strings.Contains(got.Findings[0].Body, "<!-- codereview:") {
		t.Fatalf("finding body was not sanitized: %q", got.Findings[0].Body)
	}

	aliasGot, err := DecodeFindings([]byte(`{
		"schema_version": 1,
		"agent_id": "agent-1",
		"inspected_files": ["main.go"],
		"findings": [{
			"severity":"major",
			"file":"main.go",
			"anchor":{"kind":"file"},
			"body":"body"
		}]
	}`), FindingsOptions{
		KnownAgents:  opts.KnownAgents,
		ChangedFiles: opts.ChangedFiles,
		NewFindingID: newIDQueue("f-alias").next,
	})
	if err != nil {
		t.Fatalf("DecodeFindings file alias: %v", err)
	}
	if aliasGot.Findings[0].FilePath != "main.go" || aliasGot.Findings[0].ID != "f-alias" {
		t.Fatalf("alias finding = %#v, want canonical file path and assigned ID", aliasGot.Findings[0])
	}

	matchingAliasGot, err := DecodeFindings([]byte(`{
		"schema_version": 1,
		"agent_id": "agent-1",
		"inspected_files": ["main.go"],
		"findings": [{
			"severity":"major",
			"file_path":"main.go",
			"file":"main.go",
			"anchor":{"kind":"file"},
			"body":"body"
		}]
	}`), FindingsOptions{
		KnownAgents:  opts.KnownAgents,
		ChangedFiles: opts.ChangedFiles,
		NewFindingID: newIDQueue("f-matching-alias").next,
	})
	if err != nil {
		t.Fatalf("DecodeFindings matching file alias: %v", err)
	}
	if matchingAliasGot.Findings[0].FilePath != "main.go" {
		t.Fatalf("matching alias file path = %q, want main.go", matchingAliasGot.Findings[0].FilePath)
	}

	baseOpts := FindingsOptions{KnownAgents: map[string]bool{"agent-1": true}, ChangedFiles: map[string]bool{"main.go": true}, NewFindingID: newIDQueue("f-1", "f-2").next}
	assertFindingsError(t, baseOpts, `{"schema_version":1,"agent_id":"agent-1","inspected_files":[],"findings":[]}`, "inspected_files")
	assertFindingsError(t, baseOpts, `{"schema_version":1,"agent_id":"agent-1","inspected_files":["other.go"],"findings":[]}`, "inspected_files entry")
	assertFindingsError(t, baseOpts, `{"schema_version":1,"agent_id":"agent-1","inspected_files":["main.go","main.go"],"findings":[]}`, "duplicate inspected_files")
	assertFindingsError(t, baseOpts, `{"schema_version":1,"agent_id":"agent-1","inspected_files":["main.go"],"skipped_files":["other.go"],"findings":[]}`, "skipped_files entry")
	assertFindingsError(t, baseOpts, `{"schema_version":1,"agent_id":"agent-1","inspected_files":["main.go"],"constraints":["  "],"findings":[]}`, "constraints")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":2,"agent_id":"agent-1","findings":[]`), "schema_version")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[],"extra":true`), "unknown field")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"missing","findings":[]`), "unknown findings agent")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"finding_id":"model-id","severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "finding_id")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"finding_id":null,"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "finding_id")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"bad","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "severity")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","file":"other.go","anchor":{"kind":"file"},"body":"body"}]`), "file and file_path")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":null,"file":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "file_path must be a string")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file":null,"anchor":{"kind":"file"},"body":"body"}]`), "file must be a string")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file":"other.go","anchor":{"kind":"file"},"body":"body"}]`), "changed files")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file":"main.go","anchor":{"kind":"file"},"body":"body","extra":true}]`), "unknown field")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"other.go","anchor":{"kind":"file"},"body":"body"}]`), "changed files")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"line","side":"RIGHT"},"body":"body"}]`), "line anchor requires a positive line")
	assertFindingsError(t, baseOpts, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"  "}]`), "finding body length out of bounds")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: newIDQueue("f-1").next, MaxBodyLength: 3}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"toolong"}]`), "finding body length out of bounds")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: newIDQueue("f-1", "f-2").next, MaxFindingsPerAgent: 1}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"},{"severity":"minor","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "findings cap exceeded")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: newIDQueue("f-1").next, SeverityCaps: map[review.Severity]int{review.SeverityMajor: 0}}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "major severity cap exceeded")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: nil}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[]`), "generator")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: func() (review.FindingID, error) { return "", errors.New("id failed") }}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "id failed")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: newIDQueue("").next}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "blank")
	assertFindingsError(t, FindingsOptions{KnownAgents: baseOpts.KnownAgents, ChangedFiles: baseOpts.ChangedFiles, NewFindingID: newIDQueue("dup", "dup").next}, findingsFixture(`"schema_version":1,"agent_id":"agent-1","findings":[{"severity":"major","file_path":"main.go","anchor":{"kind":"file"},"body":"body"},{"severity":"minor","file_path":"main.go","anchor":{"kind":"file"},"body":"body"}]`), "duplicate")
}

func findingsFixture(fields string) string {
	return `{"inspected_files":["main.go"],` + fields + `}`
}

func TestDecodeRollup(t *testing.T) {
	known := map[review.FindingID]review.Severity{
		"f-1": review.SeverityMajor,
		"f-2": review.SeverityMajor,
		"f-3": review.SeverityMinor,
	}
	got, err := DecodeRollup([]byte(`{
		"schema_version": 1,
		"review_event": "comment",
		"review_event_rationale": "rationale <!-- codereview:skip -->",
		"dedupe_log": [{"kept":"f-1","dropped":["f-2"],"reason":"same <!-- codereview:skip -->"}],
		"ordered_findings": ["f-1","f-3"]
	}`), RollupOptions{FindingSeverities: known})
	if err != nil {
		t.Fatalf("DecodeRollup: %v", err)
	}
	if got.ReviewEvent != review.ReviewEventComment || len(got.OrderedFindings) != 2 || got.DedupeLog[0].Kept != "f-1" {
		t.Fatalf("DecodeRollup = %#v", got)
	}
	if strings.Contains(got.ReviewEventRationale, "<!-- codereview:") || strings.Contains(got.DedupeLog[0].Reason, "<!-- codereview:") {
		t.Fatalf("rollup strings were not sanitized: %#v", got)
	}

	assertRollupError(t, known, `{"schema_version":2,"review_event":"comment","review_event_rationale":"x","ordered_findings":["f-1","f-2","f-3"]}`, "schema_version")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","ordered_findings":["f-1","f-2","f-3"],"extra":true}`, "unknown field")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"bad","review_event_rationale":"x","ordered_findings":["f-1","f-2","f-3"]}`, "review event")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"  ","ordered_findings":["f-1","f-2","f-3"]}`, "rationale")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"missing","dropped":["f-2"],"reason":"x"}],"ordered_findings":["f-1","f-3"]}`, "unknown dedupe kept")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":["missing"],"reason":"x"}],"ordered_findings":["f-1","f-2","f-3"]}`, "unknown dedupe dropped")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":[],"reason":"x"}],"ordered_findings":["f-1","f-2","f-3"]}`, "drop at least one finding")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":["f-2"],"reason":"  "}],"ordered_findings":["f-1","f-3"]}`, "dedupe reason")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":["f-1"],"reason":"x"}],"ordered_findings":["f-2","f-3"]}`, "cannot be dropped")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-2","dropped":["f-3"],"reason":"x"},{"kept":"f-1","dropped":["f-2"],"reason":"x"}],"ordered_findings":["f-1"]}`, "kept ID")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":["f-2"],"reason":"x"},{"kept":"f-3","dropped":["f-2"],"reason":"x"}],"ordered_findings":["f-1","f-3"]}`, "dropped more than once")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","dedupe_log":[{"kept":"f-1","dropped":["f-2"],"reason":"x"}],"ordered_findings":["f-1","f-2","f-3"]}`, "cannot be ordered")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","ordered_findings":["missing","f-1","f-2","f-3"]}`, "unknown ordered finding ID")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","ordered_findings":["f-1","f-1","f-2","f-3"]}`, "appears more than once")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","ordered_findings":["f-1","f-2"]}`, "does not cover")
	assertRollupError(t, known, `{"schema_version":1,"review_event":"approve","review_event_rationale":"x","ordered_findings":["f-1","f-2","f-3"]}`, "major findings cannot approve")
	assertRollupError(t, map[review.FindingID]review.Severity{"f-1": review.SeverityBlocking}, `{"schema_version":1,"review_event":"comment","review_event_rationale":"x","ordered_findings":["f-1"]}`, "blocking")
	assertRollupError(t, map[review.FindingID]review.Severity{"f-1": review.SeverityMinor}, `{"schema_version":1,"review_event":"request_changes","review_event_rationale":"x","ordered_findings":["f-1"]}`, "request_changes")
	if _, err := DecodeRollup([]byte(`{"schema_version":1,"review_event":"request_changes","review_event_rationale":"x","ordered_findings":["f-1"]}`), RollupOptions{FindingSeverities: map[review.FindingID]review.Severity{"f-1": review.SeverityMajor}, MajorEventRequestsChanges: true}); err != nil {
		t.Fatalf("DecodeRollup major request_changes with policy: %v", err)
	}
}

func assertSelectionError(t *testing.T, opts SelectionOptions, body string, want string) {
	t.Helper()
	_, err := DecodeSelection([]byte(body), opts)
	assertErrContains(t, err, want)
}

func assertFindingsError(t *testing.T, opts FindingsOptions, body string, want string) {
	t.Helper()
	_, err := DecodeFindings([]byte(body), opts)
	assertErrContains(t, err, want)
}

func assertRollupError(t *testing.T, known map[review.FindingID]review.Severity, body string, want string) {
	t.Helper()
	_, err := DecodeRollup([]byte(body), RollupOptions{FindingSeverities: known})
	assertErrContains(t, err, want)
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err, want)
	}
}

type idQueue struct {
	ids []review.FindingID
}

func newIDQueue(ids ...review.FindingID) *idQueue {
	return &idQueue{ids: ids}
}

func (q *idQueue) next() (review.FindingID, error) {
	if len(q.ids) == 0 {
		return "", errors.New("empty id queue")
	}
	id := q.ids[0]
	q.ids = q.ids[1:]
	return id, nil
}
