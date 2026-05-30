// Package review defines provider-neutral review-domain values.
package review

import (
	"fmt"
	"strings"
)

// Severity is a reviewer finding severity.
type Severity string

// Severity values, ordered from most to least severe.
const (
	SeverityBlocking Severity = "blocking"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityNits     Severity = "nits"
)

var severityOrder = []Severity{
	SeverityBlocking,
	SeverityMajor,
	SeverityMinor,
	SeverityNits,
}

var severityWeights = map[Severity]int{
	SeverityBlocking: 4,
	SeverityMajor:    3,
	SeverityMinor:    2,
	SeverityNits:     1,
}

// ParseSeverity parses a severity value from the structured review domain.
func ParseSeverity(value string) (Severity, error) {
	severity := Severity(normalizeLower(value))
	if !severity.Valid() {
		return "", invalidValue("severity", value)
	}
	return severity, nil
}

// String returns the wire/domain representation.
func (s Severity) String() string {
	return string(s)
}

// Valid reports whether s is one of the known severity values.
func (s Severity) Valid() bool {
	_, ok := severityWeights[s]
	return ok
}

// AtLeast reports whether s is at least as severe as threshold.
func (s Severity) AtLeast(threshold Severity) bool {
	weight, ok := severityWeights[s]
	if !ok {
		return false
	}
	thresholdWeight, ok := severityWeights[threshold]
	if !ok {
		return false
	}
	return weight >= thresholdWeight
}

// SeverityOrder returns severities from most to least severe.
func SeverityOrder() []Severity {
	return append([]Severity(nil), severityOrder...)
}

// ReviewEvent is the final review submission event.
//
//nolint:revive // ReviewEvent is the design term and avoids ambiguity with generic events.
type ReviewEvent string

// ReviewEvent values.
const (
	ReviewEventApprove        ReviewEvent = "approve"
	ReviewEventComment        ReviewEvent = "comment"
	ReviewEventRequestChanges ReviewEvent = "request_changes"
)

// ParseReviewEvent parses a review event.
func ParseReviewEvent(value string) (ReviewEvent, error) {
	event := ReviewEvent(normalizeLower(value))
	if !event.Valid() {
		return "", invalidValue("review event", value)
	}
	return event, nil
}

// String returns the wire/domain representation.
func (e ReviewEvent) String() string {
	return string(e)
}

// Valid reports whether e is one of the known review event values.
func (e ReviewEvent) Valid() bool {
	switch e {
	case ReviewEventApprove, ReviewEventComment, ReviewEventRequestChanges:
		return true
	default:
		return false
	}
}

// ThreadDecision is the orchestrator's decision for a review thread.
type ThreadDecision string

// ThreadDecision values.
const (
	ThreadDecisionSkip                ThreadDecision = "skip"
	ThreadDecisionSummarizeOnly       ThreadDecision = "summarize_only"
	ThreadDecisionSummarizeAndResolve ThreadDecision = "summarize_and_resolve"
)

// ParseThreadDecision parses a thread decision.
func ParseThreadDecision(value string) (ThreadDecision, error) {
	decision := ThreadDecision(normalizeLower(value))
	if !decision.Valid() {
		return "", invalidValue("thread decision", value)
	}
	return decision, nil
}

// String returns the wire/domain representation.
func (d ThreadDecision) String() string {
	return string(d)
}

// Valid reports whether d is one of the known thread decision values.
func (d ThreadDecision) Valid() bool {
	switch d {
	case ThreadDecisionSkip, ThreadDecisionSummarizeOnly, ThreadDecisionSummarizeAndResolve:
		return true
	default:
		return false
	}
}

// AnchorKind identifies whether a finding is line- or file-scoped.
type AnchorKind string

// AnchorKind values.
const (
	AnchorKindLine AnchorKind = "line"
	AnchorKindFile AnchorKind = "file"
)

// ParseAnchorKind parses an anchor kind.
func ParseAnchorKind(value string) (AnchorKind, error) {
	kind := AnchorKind(normalizeLower(value))
	if !kind.Valid() {
		return "", invalidValue("anchor kind", value)
	}
	return kind, nil
}

// String returns the wire/domain representation.
func (k AnchorKind) String() string {
	return string(k)
}

// Valid reports whether k is one of the known anchor kind values.
func (k AnchorKind) Valid() bool {
	switch k {
	case AnchorKindLine, AnchorKindFile:
		return true
	default:
		return false
	}
}

// DiffSide identifies which side of a pull-request diff an anchor targets.
type DiffSide string

// DiffSide values.
const (
	DiffSideLeft  DiffSide = "LEFT"
	DiffSideRight DiffSide = "RIGHT"
)

// ParseDiffSide parses a diff side.
func ParseDiffSide(value string) (DiffSide, error) {
	side := DiffSide(normalizeUpper(value))
	if !side.Valid() {
		return "", invalidValue("diff side", value)
	}
	return side, nil
}

// String returns the wire/domain representation.
func (s DiffSide) String() string {
	return string(s)
}

// Valid reports whether s is one of the known diff side values.
func (s DiffSide) Valid() bool {
	switch s {
	case DiffSideLeft, DiffSideRight:
		return true
	default:
		return false
	}
}

// Anchoring records how a finding will be represented in the provider.
type Anchoring string

// Anchoring values.
const (
	AnchoringInline            Anchoring = "inline"
	AnchoringFileLevelNative   Anchoring = "file-level-native"
	AnchoringFileLevelFallback Anchoring = "file-level-fallback"
	AnchoringRollupOnly        Anchoring = "rollup-only"
)

// ParseAnchoring parses an anchoring value.
func ParseAnchoring(value string) (Anchoring, error) {
	anchoring := Anchoring(normalizeLower(value))
	if !anchoring.Valid() {
		return "", invalidValue("anchoring", value)
	}
	return anchoring, nil
}

// String returns the wire/domain representation.
func (a Anchoring) String() string {
	return string(a)
}

// Valid reports whether a is one of the known anchoring values.
func (a Anchoring) Valid() bool {
	switch a {
	case AnchoringInline, AnchoringFileLevelNative, AnchoringFileLevelFallback, AnchoringRollupOnly:
		return true
	default:
		return false
	}
}

// FindingID identifies a finding after harness-side validation assigns it.
type FindingID string

// String returns the finding ID string.
func (id FindingID) String() string {
	return string(id)
}

// Assigned reports whether the finding has a harness-assigned ID.
func (id FindingID) Assigned() bool {
	return id != ""
}

// Anchor is the provider-neutral location requested for a finding.
type Anchor struct {
	Kind AnchorKind `json:"kind"`
	Side DiffSide   `json:"side,omitempty"`
	Line int        `json:"line,omitempty"`
}

// Validate checks domain-local anchor field consistency.
func (a Anchor) Validate() error {
	switch a.Kind {
	case AnchorKindLine:
		if !a.Side.Valid() {
			return invalidValue("diff side", a.Side.String())
		}
		if a.Line <= 0 {
			return fmt.Errorf("line anchor requires a positive line")
		}
	case AnchorKindFile:
		if a.Side != "" {
			return fmt.Errorf("file anchor cannot set side")
		}
		if a.Line != 0 {
			return fmt.Errorf("file anchor cannot set line")
		}
	default:
		return invalidValue("anchor kind", a.Kind.String())
	}
	return nil
}

// Finding is a provider-neutral review finding.
type Finding struct {
	ID        FindingID `json:"finding_id,omitempty"`
	Severity  Severity  `json:"severity"`
	FilePath  string    `json:"file_path"`
	Anchor    Anchor    `json:"anchor"`
	Body      string    `json:"body"`
	Anchoring Anchoring `json:"anchoring,omitempty"`
}

// Validate checks domain-local finding field consistency.
func (f Finding) Validate() error {
	if !f.Severity.Valid() {
		return invalidValue("severity", f.Severity.String())
	}
	if err := f.Anchor.Validate(); err != nil {
		return err
	}
	if f.Anchoring != "" && !f.Anchoring.Valid() {
		return invalidValue("anchoring", f.Anchoring.String())
	}
	return nil
}

// ThreadAction is the orchestrator's chosen action for an existing PR thread.
type ThreadAction struct {
	ThreadID               string         `json:"thread_id"`
	Decision               ThreadDecision `json:"decision"`
	Summary                string         `json:"summary,omitempty"`
	SafeToResolveRationale string         `json:"safe_to_resolve_rationale,omitempty"`
}

// DedupeEntry records finding IDs collapsed during rollup planning.
type DedupeEntry struct {
	Kept    FindingID   `json:"kept"`
	Dropped []FindingID `json:"dropped"`
	Reason  string      `json:"reason"`
}

// Rollup is the provider-neutral review rollup selected by the orchestrator.
type Rollup struct {
	ReviewEvent          ReviewEvent   `json:"review_event"`
	ReviewEventRationale string        `json:"review_event_rationale"`
	DedupeLog            []DedupeEntry `json:"dedupe_log"`
	OrderedFindings      []FindingID   `json:"ordered_findings"`
}

func normalizeLower(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeUpper(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func invalidValue(kind, value string) error {
	return fmt.Errorf("invalid %s %q", kind, value)
}
