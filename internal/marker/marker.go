// Package marker renders and parses codereview PR comment markers.
package marker

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	htmlCommentOpen  = "<!-- "
	htmlCommentClose = " -->"
	markerPrefix     = "codereview:"
	skipMarker       = htmlCommentOpen + markerPrefix + "skip" + htmlCommentClose
)

// Action marker kind values.
const (
	ActionKindInlineComment = "inline_comment"
	ActionKindThreadReply   = "thread_reply"
	ActionKindRollupComment = "rollup_comment"
	ActionKindSubmitReview  = "submit_review"
)

// Rollup outcome values.
const (
	RollupOutcomeApproved        = "approved"
	RollupOutcomeRequestChanges  = "request_changes"
	RollupOutcomeComment         = "comment"
	RollupOutcomeNothingToReview = "nothing_to_review"
)

// ActionMarker identifies a body-bearing action emitted by a codereview run.
type ActionMarker struct {
	RunID    string
	ActionID string
	Kind     string
	SHA      string
	BaseSHA  string
	Outcome  string
}

// ThreadSummaryMarker identifies a codereview-authored thread summary.
type ThreadSummaryMarker struct {
	RunID    string
	ActionID string
}

// RenderSkip returns the canonical marker that disables codereview for a PR.
func RenderSkip() string {
	return skipMarker
}

// RenderAction returns the canonical marker for a body-bearing action.
func RenderAction(marker ActionMarker) (string, error) {
	if err := validateAction(marker); err != nil {
		return "", err
	}

	rendered := htmlCommentOpen + markerPrefix +
		"run-id=" + marker.RunID +
		":action=" + marker.ActionID +
		":kind=" + marker.Kind +
		":sha=" + marker.SHA +
		":base=" + marker.BaseSHA
	if marker.Outcome != "" {
		rendered += ":outcome=" + marker.Outcome
	}
	return rendered + htmlCommentClose, nil
}

// ParseAction parses one canonical body-bearing action marker.
func ParseAction(text string) (ActionMarker, error) {
	payload, err := commentPayload(text)
	if err != nil {
		return ActionMarker{}, err
	}
	if !strings.HasPrefix(payload, markerPrefix) {
		return ActionMarker{}, fmt.Errorf("marker prefix missing")
	}

	parts := strings.Split(strings.TrimPrefix(payload, markerPrefix), ":")
	if len(parts) != 5 && len(parts) != 6 {
		return ActionMarker{}, fmt.Errorf("action marker has %d fields, want 5 or 6", len(parts))
	}

	marker := ActionMarker{}
	if marker.RunID, err = parseField(parts[0], "run-id"); err != nil {
		return ActionMarker{}, err
	}
	if marker.ActionID, err = parseField(parts[1], "action"); err != nil {
		return ActionMarker{}, err
	}
	if marker.Kind, err = parseField(parts[2], "kind"); err != nil {
		return ActionMarker{}, err
	}
	if marker.SHA, err = parseField(parts[3], "sha"); err != nil {
		return ActionMarker{}, err
	}
	if marker.BaseSHA, err = parseField(parts[4], "base"); err != nil {
		return ActionMarker{}, err
	}
	if len(parts) == 6 {
		if marker.Outcome, err = parseField(parts[5], "outcome"); err != nil {
			return ActionMarker{}, err
		}
	}
	if err := validateAction(marker); err != nil {
		return ActionMarker{}, err
	}
	return marker, nil
}

// FindActions returns all valid canonical action markers in body order.
func FindActions(body string) []ActionMarker {
	var markers []ActionMarker
	for _, candidate := range findCodereviewComments(body) {
		marker, err := ParseAction(candidate)
		if err == nil {
			markers = append(markers, marker)
		}
	}
	return markers
}

// RenderThreadSummary returns the canonical marker for a thread summary.
func RenderThreadSummary(marker ThreadSummaryMarker) (string, error) {
	if err := validateThreadSummary(marker); err != nil {
		return "", err
	}
	return htmlCommentOpen + markerPrefix +
		"thread-summary" +
		":run-id=" + marker.RunID +
		":action=" + marker.ActionID +
		htmlCommentClose, nil
}

// ParseThreadSummary parses one canonical thread-summary marker.
func ParseThreadSummary(text string) (ThreadSummaryMarker, error) {
	payload, err := commentPayload(text)
	if err != nil {
		return ThreadSummaryMarker{}, err
	}
	if !strings.HasPrefix(payload, markerPrefix) {
		return ThreadSummaryMarker{}, fmt.Errorf("marker prefix missing")
	}

	parts := strings.Split(strings.TrimPrefix(payload, markerPrefix), ":")
	if len(parts) != 3 {
		return ThreadSummaryMarker{}, fmt.Errorf("thread-summary marker has %d fields, want 3", len(parts))
	}
	if parts[0] != "thread-summary" {
		return ThreadSummaryMarker{}, fmt.Errorf("thread-summary marker prefix %q is invalid", parts[0])
	}

	runID, err := parseField(parts[1], "run-id")
	if err != nil {
		return ThreadSummaryMarker{}, err
	}
	actionID, err := parseField(parts[2], "action")
	if err != nil {
		return ThreadSummaryMarker{}, err
	}
	marker := ThreadSummaryMarker{
		RunID:    runID,
		ActionID: actionID,
	}
	if err := validateThreadSummary(marker); err != nil {
		return ThreadSummaryMarker{}, err
	}
	return marker, nil
}

// FindThreadSummaries returns all valid canonical thread-summary markers in body order.
func FindThreadSummaries(body string) []ThreadSummaryMarker {
	var markers []ThreadSummaryMarker
	for _, candidate := range findCodereviewComments(body) {
		marker, err := ParseThreadSummary(candidate)
		if err == nil {
			markers = append(markers, marker)
		}
	}
	return markers
}

// SanitizeModelContent escapes codereview marker openings in model-authored text.
// It leaves unrelated HTML comment syntax to the caller's output-context escaping.
func SanitizeModelContent(text string) string {
	return strings.ReplaceAll(text, htmlCommentOpen+markerPrefix, "&lt;!-- "+markerPrefix)
}

func commentPayload(text string) (string, error) {
	if !strings.HasPrefix(text, htmlCommentOpen) {
		return "", fmt.Errorf("marker must start with %q", htmlCommentOpen)
	}
	if !strings.HasSuffix(text, htmlCommentClose) {
		return "", fmt.Errorf("marker must end with %q", htmlCommentClose)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(text, htmlCommentOpen), htmlCommentClose)
	if payload == "" {
		return "", fmt.Errorf("marker payload is empty")
	}
	return payload, nil
}

func parseField(part, name string) (string, error) {
	prefix := name + "="
	if !strings.HasPrefix(part, prefix) {
		return "", fmt.Errorf("marker field %q must be %q", part, name)
	}
	value := strings.TrimPrefix(part, prefix)
	if err := validateValue(name, value); err != nil {
		return "", err
	}
	return value, nil
}

func validateAction(marker ActionMarker) error {
	if err := validateValue("run ID", marker.RunID); err != nil {
		return err
	}
	if err := validateValue("action ID", marker.ActionID); err != nil {
		return err
	}
	if err := validateValue("kind", marker.Kind); err != nil {
		return err
	}
	if err := validateValue("SHA", marker.SHA); err != nil {
		return err
	}
	if err := validateValue("base SHA", marker.BaseSHA); err != nil {
		return err
	}
	if !validActionKind(marker.Kind) {
		return fmt.Errorf("invalid action marker kind %q", marker.Kind)
	}
	if marker.Kind == ActionKindRollupComment {
		if err := validateValue("outcome", marker.Outcome); err != nil {
			return err
		}
		if !validRollupOutcome(marker.Outcome) {
			return fmt.Errorf("invalid rollup outcome %q", marker.Outcome)
		}
		return nil
	}
	if marker.Outcome != "" {
		return fmt.Errorf("outcome is only valid for %q markers", ActionKindRollupComment)
	}
	return nil
}

func validateThreadSummary(marker ThreadSummaryMarker) error {
	if err := validateValue("run ID", marker.RunID); err != nil {
		return err
	}
	return validateValue("action ID", marker.ActionID)
}

func validateValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if strings.Contains(value, ":") {
		return fmt.Errorf("%s must not contain ':'", name)
	}
	if strings.Contains(value, "-->") {
		return fmt.Errorf("%s must not contain HTML comment terminator", name)
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%s must not contain whitespace", name)
		}
	}
	return nil
}

func validActionKind(kind string) bool {
	switch kind {
	case ActionKindInlineComment, ActionKindThreadReply, ActionKindRollupComment, ActionKindSubmitReview:
		return true
	default:
		return false
	}
}

func validRollupOutcome(outcome string) bool {
	switch outcome {
	case RollupOutcomeApproved, RollupOutcomeRequestChanges, RollupOutcomeComment, RollupOutcomeNothingToReview:
		return true
	default:
		return false
	}
}

func findCodereviewComments(body string) []string {
	const startNeedle = htmlCommentOpen + markerPrefix

	var comments []string
	searchFrom := 0
	for searchFrom < len(body) {
		start := strings.Index(body[searchFrom:], startNeedle)
		if start == -1 {
			break
		}
		start += searchFrom
		payloadStart := start + len(startNeedle)
		end := strings.Index(body[payloadStart:], htmlCommentClose)
		nextStart := strings.Index(body[payloadStart:], startNeedle)
		if nextStart != -1 && (end == -1 || nextStart < end) {
			searchFrom = payloadStart + nextStart
			continue
		}
		if end == -1 {
			searchFrom = payloadStart
			continue
		}
		end += payloadStart + len(htmlCommentClose)
		comments = append(comments, body[start:end])
		searchFrom = end
	}
	return comments
}
