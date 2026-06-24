package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	// SchemaVersion is the structured-output schema version supported by this package.
	SchemaVersion = 1

	// DefaultMaxFindingsPerAgent is the default per-agent findings cap.
	DefaultMaxFindingsPerAgent = 50

	// DefaultMaxFindingBodyLen is the default max finding body length in runes.
	DefaultMaxFindingBodyLen = 8000

	// DefaultMaxResolvedThreads is the default selected thread resolution cap.
	DefaultMaxResolvedThreads = 25

	defaultMaxCoverageConstraints     = 10
	defaultMaxCoverageConstraintRunes = 300
)

// Selection is validated orchestrator selection output.
type Selection struct {
	SelectedAgents []SelectedAgent
	ThreadActions  []review.ThreadAction
	Reasoning      string
}

// SelectedAgent is one selected reviewer agent.
type SelectedAgent struct {
	AgentID      string
	Rationale    string
	Files        []string
	AllowedFiles []string
}

// SelectionOptions contains context needed to validate selection output.
type SelectionOptions struct {
	KnownAgents  map[string]bool
	ChangedFiles map[string]bool
	KnownThreads map[string]bool
	// MaxResolvedThreads uses DefaultMaxResolvedThreads when zero.
	MaxResolvedThreads int
}

// Findings is validated reviewer findings output.
type Findings struct {
	AgentID        string
	Findings       []review.Finding
	InspectedFiles []string
	SkippedFiles   []string
	Constraints    []string
}

// FindingIDGenerator assigns harness-owned finding IDs.
type FindingIDGenerator func() (review.FindingID, error)

// FindingsOptions contains context needed to validate finding output.
type FindingsOptions struct {
	KnownAgents  map[string]bool
	ChangedFiles map[string]bool
	NewFindingID FindingIDGenerator
	// MaxFindingsPerAgent uses DefaultMaxFindingsPerAgent when zero.
	MaxFindingsPerAgent int
	// MaxBodyLength uses DefaultMaxFindingBodyLen when zero.
	MaxBodyLength int
	SeverityCaps  map[review.Severity]int
}

// RollupOptions contains context needed to validate rollup output.
type RollupOptions struct {
	FindingSeverities         map[review.FindingID]review.Severity
	MajorEventRequestsChanges bool
}

type selectionWire struct {
	SchemaVersion  int                 `json:"schema_version"`
	SelectedAgents []selectedAgentWire `json:"selected_agents"`
	ThreadActions  []threadActionWire  `json:"thread_actions"`
	Reasoning      string              `json:"reasoning"`
}

type selectedAgentWire struct {
	AgentID      string   `json:"agent_id"`
	Rationale    string   `json:"rationale"`
	Files        []string `json:"files"`
	AllowedFiles []string `json:"allowed_files,omitempty"`
}

type threadActionWire struct {
	ThreadID               string `json:"thread_id"`
	Decision               string `json:"decision"`
	Summary                string `json:"summary"`
	SafeToResolveRationale string `json:"safe_to_resolve_rationale"`
}

type findingsWire struct {
	SchemaVersion  int           `json:"schema_version"`
	AgentID        string        `json:"agent_id"`
	InspectedFiles []string      `json:"inspected_files"`
	SkippedFiles   []string      `json:"skipped_files,omitempty"`
	Constraints    []string      `json:"constraints,omitempty"`
	Findings       []findingWire `json:"findings"`
}

type findingWire struct {
	FindingID json.RawMessage `json:"finding_id,omitempty"`
	Severity  string          `json:"severity"`
	FilePath  json.RawMessage `json:"file_path"`
	FileAlias json.RawMessage `json:"file"`
	Anchor    anchorWire      `json:"anchor"`
	Body      string          `json:"body"`
}

type anchorWire struct {
	Kind string `json:"kind"`
	Side string `json:"side,omitempty"`
	Line int    `json:"line,omitempty"`
}

type rollupWire struct {
	SchemaVersion        int               `json:"schema_version"`
	ReviewEvent          string            `json:"review_event"`
	ReviewEventRationale string            `json:"review_event_rationale"`
	DedupeLog            []dedupeEntryWire `json:"dedupe_log"`
	OrderedFindings      []string          `json:"ordered_findings"`
}

type dedupeEntryWire struct {
	Kept    string   `json:"kept"`
	Dropped []string `json:"dropped"`
	Reason  string   `json:"reason"`
}

// DecodeSelection validates and maps Selection structured output.
func DecodeSelection(data []byte, opts SelectionOptions) (Selection, error) {
	var wire selectionWire
	if err := decodeStrict(data, &wire); err != nil {
		return Selection{}, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return Selection{}, fmt.Errorf("llm: unsupported selection schema_version %d", wire.SchemaVersion)
	}
	maxResolved := opts.MaxResolvedThreads
	if maxResolved == 0 {
		maxResolved = DefaultMaxResolvedThreads
	}

	selection := Selection{Reasoning: sanitize(wire.Reasoning)}
	for _, agent := range wire.SelectedAgents {
		if strings.TrimSpace(agent.AgentID) == "" || !opts.KnownAgents[agent.AgentID] {
			return Selection{}, fmt.Errorf("llm: unknown selected agent %q", agent.AgentID)
		}
		for _, file := range agent.Files {
			if strings.TrimSpace(file) == "" || !opts.ChangedFiles[file] {
				return Selection{}, fmt.Errorf("llm: selected file %q is not in changed files", file)
			}
		}
		for _, file := range agent.AllowedFiles {
			if strings.TrimSpace(file) == "" || !opts.ChangedFiles[file] {
				return Selection{}, fmt.Errorf("llm: allowed file %q is not in changed files", file)
			}
		}
		selection.SelectedAgents = append(selection.SelectedAgents, SelectedAgent{
			AgentID:      agent.AgentID,
			Rationale:    sanitize(agent.Rationale),
			Files:        append([]string(nil), agent.Files...),
			AllowedFiles: append([]string(nil), agent.AllowedFiles...),
		})
	}

	resolved := 0
	for _, action := range wire.ThreadActions {
		if strings.TrimSpace(action.ThreadID) == "" || !opts.KnownThreads[action.ThreadID] {
			return Selection{}, fmt.Errorf("llm: unknown thread %q", action.ThreadID)
		}
		decision, err := review.ParseThreadDecision(action.Decision)
		if err != nil {
			return Selection{}, err
		}
		if decision == review.ThreadDecisionSummarizeOnly || decision == review.ThreadDecisionSummarizeAndResolve {
			if strings.TrimSpace(action.Summary) == "" {
				return Selection{}, fmt.Errorf("llm: summary is required for %s", decision)
			}
		}
		if decision == review.ThreadDecisionSummarizeAndResolve {
			resolved++
			if strings.TrimSpace(action.SafeToResolveRationale) == "" {
				return Selection{}, fmt.Errorf("llm: safe_to_resolve_rationale is required")
			}
		}
		if resolved > maxResolved {
			return Selection{}, fmt.Errorf("llm: resolved thread cap exceeded")
		}
		selection.ThreadActions = append(selection.ThreadActions, review.ThreadAction{
			ThreadID:               action.ThreadID,
			Decision:               decision,
			Summary:                sanitize(action.Summary),
			SafeToResolveRationale: sanitize(action.SafeToResolveRationale),
		})
	}
	return selection, nil
}

// DecodeFindings validates and maps Findings structured output.
func DecodeFindings(data []byte, opts FindingsOptions) (Findings, error) {
	var wire findingsWire
	if err := decodeStrict(data, &wire); err != nil {
		return Findings{}, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return Findings{}, fmt.Errorf("llm: unsupported findings schema_version %d", wire.SchemaVersion)
	}
	if strings.TrimSpace(wire.AgentID) == "" || !opts.KnownAgents[wire.AgentID] {
		return Findings{}, fmt.Errorf("llm: unknown findings agent %q", wire.AgentID)
	}
	maxFindings := opts.MaxFindingsPerAgent
	if maxFindings == 0 {
		maxFindings = DefaultMaxFindingsPerAgent
	}
	maxBody := opts.MaxBodyLength
	if maxBody == 0 {
		maxBody = DefaultMaxFindingBodyLen
	}
	if len(wire.Findings) > maxFindings {
		return Findings{}, fmt.Errorf("llm: findings cap exceeded")
	}
	if opts.NewFindingID == nil {
		return Findings{}, fmt.Errorf("llm: finding ID generator is required")
	}

	seenIDs := map[review.FindingID]bool{}
	severityCounts := map[review.Severity]int{}
	inspected, err := decodeCoverageFiles("inspected_files", wire.InspectedFiles, opts.ChangedFiles)
	if err != nil {
		return Findings{}, err
	}
	if len(inspected) == 0 {
		return Findings{}, fmt.Errorf("llm: inspected_files must contain at least one changed file")
	}
	skipped, err := decodeCoverageFiles("skipped_files", wire.SkippedFiles, opts.ChangedFiles)
	if err != nil {
		return Findings{}, err
	}
	if err := validateCoverageFileDisjoint(inspected, skipped); err != nil {
		return Findings{}, err
	}
	constraints, err := decodeCoverageStrings("constraints", wire.Constraints)
	if err != nil {
		return Findings{}, err
	}

	result := Findings{
		AgentID:        wire.AgentID,
		InspectedFiles: inspected,
		SkippedFiles:   skipped,
		Constraints:    constraints,
	}
	for _, found := range wire.Findings {
		if len(found.FindingID) > 0 {
			return Findings{}, fmt.Errorf("llm: model-supplied finding_id is not allowed")
		}
		severity, err := review.ParseSeverity(found.Severity)
		if err != nil {
			return Findings{}, err
		}
		severityCounts[severity]++
		if capValue, ok := opts.SeverityCaps[severity]; ok && severityCounts[severity] > capValue {
			return Findings{}, fmt.Errorf("llm: %s severity cap exceeded", severity)
		}
		filePath, err := found.normalizedFilePath()
		if err != nil {
			return Findings{}, err
		}
		if strings.TrimSpace(filePath) == "" || !opts.ChangedFiles[filePath] {
			return Findings{}, fmt.Errorf("llm: finding file %q is not in changed files", filePath)
		}
		if len(strings.TrimSpace(found.Body)) == 0 || utf8.RuneCountInString(found.Body) > maxBody {
			return Findings{}, fmt.Errorf("llm: finding body length out of bounds")
		}
		anchor, err := decodeAnchor(found.Anchor)
		if err != nil {
			return Findings{}, err
		}
		id, err := opts.NewFindingID()
		if err != nil {
			return Findings{}, err
		}
		if !id.Assigned() {
			return Findings{}, fmt.Errorf("llm: finding ID generator returned blank ID")
		}
		if seenIDs[id] {
			return Findings{}, fmt.Errorf("llm: finding ID generator returned duplicate ID %q", id)
		}
		seenIDs[id] = true
		finding := review.Finding{
			ID:       id,
			Severity: severity,
			FilePath: filePath,
			Anchor:   anchor,
			Body:     sanitize(found.Body),
		}
		if err := finding.Validate(); err != nil {
			return Findings{}, err
		}
		result.Findings = append(result.Findings, finding)
	}
	return result, nil
}

func decodeCoverageFiles(name string, files []string, changedFiles map[string]bool) ([]string, error) {
	out := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" || !changedFiles[file] {
			return nil, fmt.Errorf("llm: %s entry %q is not in changed files", name, file)
		}
		if seen[file] {
			return nil, fmt.Errorf("llm: duplicate %s entry %q", name, file)
		}
		seen[file] = true
		out = append(out, file)
	}
	return out, nil
}

func decodeCoverageStrings(name string, values []string) ([]string, error) {
	if len(values) > defaultMaxCoverageConstraints {
		return nil, fmt.Errorf("llm: %s cap exceeded", name)
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = sanitize(value)
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("llm: %s entries must be non-empty", name)
		}
		if utf8.RuneCountInString(value) > defaultMaxCoverageConstraintRunes {
			return nil, fmt.Errorf("llm: %s entry length out of bounds", name)
		}
		if seen[value] {
			return nil, fmt.Errorf("llm: duplicate %s entry %q", name, value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

func validateCoverageFileDisjoint(inspected, skipped []string) error {
	seen := map[string]bool{}
	for _, file := range inspected {
		seen[file] = true
	}
	for _, file := range skipped {
		if seen[file] {
			return fmt.Errorf("llm: coverage file %q cannot be both inspected and skipped", file)
		}
	}
	return nil
}

func (wire findingWire) normalizedFilePath() (string, error) {
	filePath, hasFilePath, err := decodeOptionalStringField("file_path", wire.FilePath)
	if err != nil {
		return "", err
	}
	fileAlias, hasFileAlias, err := decodeOptionalStringField("file", wire.FileAlias)
	if err != nil {
		return "", err
	}
	switch {
	case hasFilePath && hasFileAlias:
		if filePath != fileAlias {
			return "", fmt.Errorf("llm: finding file and file_path values differ")
		}
		return filePath, nil
	case hasFilePath:
		return filePath, nil
	case hasFileAlias:
		return fileAlias, nil
	default:
		return "", nil
	}
}

func decodeOptionalStringField(name string, raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, fmt.Errorf("llm: finding %s must be a string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, fmt.Errorf("llm: finding %s must be a string", name)
	}
	return value, true, nil
}

// DecodeRollup validates and maps Rollup structured output.
func DecodeRollup(data []byte, opts RollupOptions) (review.Rollup, error) {
	var wire rollupWire
	if err := decodeStrict(data, &wire); err != nil {
		return review.Rollup{}, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return review.Rollup{}, fmt.Errorf("llm: unsupported rollup schema_version %d", wire.SchemaVersion)
	}
	event, err := review.ParseReviewEvent(wire.ReviewEvent)
	if err != nil {
		return review.Rollup{}, err
	}
	if strings.TrimSpace(wire.ReviewEventRationale) == "" {
		return review.Rollup{}, fmt.Errorf("llm: review_event_rationale is required")
	}
	dropped, dedupeLog, err := decodeDedupeLog(wire.DedupeLog, opts.FindingSeverities)
	if err != nil {
		return review.Rollup{}, err
	}
	ordered, err := decodeOrderedFindings(wire.OrderedFindings, opts.FindingSeverities, dropped)
	if err != nil {
		return review.Rollup{}, err
	}
	if err := validateReviewEvent(event, ordered, opts); err != nil {
		return review.Rollup{}, err
	}
	return review.Rollup{
		ReviewEvent:          event,
		ReviewEventRationale: sanitize(wire.ReviewEventRationale),
		DedupeLog:            dedupeLog,
		OrderedFindings:      ordered,
	}, nil
}

func decodeStrict(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("llm: trailing JSON tokens")
		}
		return err
	}
	return nil
}

func decodeAnchor(wire anchorWire) (review.Anchor, error) {
	kind, err := review.ParseAnchorKind(wire.Kind)
	if err != nil {
		return review.Anchor{}, err
	}
	anchor := review.Anchor{Kind: kind, Line: wire.Line}
	if wire.Side != "" {
		side, err := review.ParseDiffSide(wire.Side)
		if err != nil {
			return review.Anchor{}, err
		}
		anchor.Side = side
	}
	if err := anchor.Validate(); err != nil {
		return review.Anchor{}, err
	}
	return anchor, nil
}

func decodeDedupeLog(entries []dedupeEntryWire, known map[review.FindingID]review.Severity) (map[review.FindingID]bool, []review.DedupeEntry, error) {
	dropped := map[review.FindingID]bool{}
	kept := map[review.FindingID]bool{}
	var result []review.DedupeEntry
	for _, entry := range entries {
		keptID := review.FindingID(entry.Kept)
		if !knownID(known, keptID) {
			return nil, nil, fmt.Errorf("llm: unknown dedupe kept ID %q", keptID)
		}
		if dropped[keptID] {
			return nil, nil, fmt.Errorf("llm: dedupe kept ID %q cannot be dropped", keptID)
		}
		kept[keptID] = true
		if len(entry.Dropped) == 0 {
			return nil, nil, fmt.Errorf("llm: dedupe entry must drop at least one finding")
		}
		if strings.TrimSpace(entry.Reason) == "" {
			return nil, nil, fmt.Errorf("llm: dedupe reason is required")
		}
		mapped := review.DedupeEntry{Kept: keptID, Reason: sanitize(entry.Reason)}
		for _, rawDropped := range entry.Dropped {
			id := review.FindingID(rawDropped)
			if !knownID(known, id) {
				return nil, nil, fmt.Errorf("llm: unknown dedupe dropped ID %q", id)
			}
			if id == keptID {
				return nil, nil, fmt.Errorf("llm: dedupe kept ID cannot be dropped")
			}
			if kept[id] {
				return nil, nil, fmt.Errorf("llm: dedupe kept ID %q cannot be dropped", id)
			}
			if dropped[id] {
				return nil, nil, fmt.Errorf("llm: finding %q dropped more than once", id)
			}
			dropped[id] = true
			mapped.Dropped = append(mapped.Dropped, id)
		}
		result = append(result, mapped)
	}
	return dropped, result, nil
}

func decodeOrderedFindings(raw []string, known map[review.FindingID]review.Severity, dropped map[review.FindingID]bool) ([]review.FindingID, error) {
	seen := map[review.FindingID]bool{}
	ordered := make([]review.FindingID, 0, len(raw))
	for _, rawID := range raw {
		id := review.FindingID(rawID)
		if !knownID(known, id) {
			return nil, fmt.Errorf("llm: unknown ordered finding ID %q", id)
		}
		if dropped[id] {
			return nil, fmt.Errorf("llm: dropped finding %q cannot be ordered", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("llm: ordered finding %q appears more than once", id)
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	for id := range known {
		if dropped[id] {
			continue
		}
		if !seen[id] {
			return nil, fmt.Errorf("llm: ordered_findings does not cover finding %q", id)
		}
	}
	return ordered, nil
}

func validateReviewEvent(event review.ReviewEvent, ordered []review.FindingID, opts RollupOptions) error {
	var blocking, major int
	for _, id := range ordered {
		switch opts.FindingSeverities[id] {
		case review.SeverityBlocking:
			blocking++
		case review.SeverityMajor:
			major++
		case review.SeverityMinor, review.SeverityNits:
		default:
			return fmt.Errorf("llm: invalid severity for ordered finding %q", id)
		}
	}
	switch {
	case blocking > 0:
		if event != review.ReviewEventRequestChanges {
			return fmt.Errorf("llm: blocking findings require request_changes")
		}
	case major > 0:
		if event == review.ReviewEventApprove {
			return fmt.Errorf("llm: major findings cannot approve")
		}
		if event == review.ReviewEventRequestChanges && !opts.MajorEventRequestsChanges {
			return fmt.Errorf("llm: major findings cannot request_changes under current policy")
		}
	case event == review.ReviewEventRequestChanges:
		return fmt.Errorf("llm: request_changes requires blocking findings or major policy")
	}
	return nil
}

func knownID(known map[review.FindingID]review.Severity, id review.FindingID) bool {
	if !id.Assigned() {
		return false
	}
	_, ok := known[id]
	return ok
}

func sanitize(value string) string {
	return marker.SanitizeModelContent(value)
}
