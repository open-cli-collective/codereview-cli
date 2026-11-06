// Package reportidentity replays caller-recorded finding and thread identity
// observations without invoking a provider or changing review behavior.
package reportidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	// SchemaVersion is the version accepted and emitted by the v1 replay wire format.
	SchemaVersion = 1
	// MaxFixtureBytes bounds the single JSON fixture read by the replay command.
	MaxFixtureBytes = 4 * 1024 * 1024
	maxFindings     = 100
	maxThreads      = 100
	maxDecisions    = 10_000
	maxPairs        = 10_000
	maxIDBytes      = 128
	maxPathBytes    = 4096
	maxBodyBytes    = 16 * 1024
)

// Task selects the identity relation represented by a replay input.
type Task string

const (
	// TaskCohort compares every unordered pair of findings in one report.
	TaskCohort Task = "cohort"
	// TaskPriorThread compares each finding with each eligible prior thread.
	TaskPriorThread Task = "prior_threads"
)

// Choice is one recorded observation for an expected pair.
type Choice string

const (
	// ChoiceSame records that both reports describe one corrective obligation.
	ChoiceSame Choice = "same"
	// ChoiceDifferent records distinct corrective obligations.
	ChoiceDifferent Choice = "different"
	// ChoiceAbstain records an unresolved comparison without a failure.
	ChoiceAbstain Choice = "abstain"
	// ChoiceError records a failed comparison attempt.
	ChoiceError Choice = "error"
	// ChoiceMissing appears in output when a pair has no recorded decision.
	ChoiceMissing Choice = "missing"
)

// Thread is the caller-supplied prior inline-thread record. Eligibility is
// explicit because the reduced planner thread type cannot recover it.
type Thread struct {
	ID       string `json:"thread_id"`
	Eligible bool   `json:"eligible"`
	FilePath string `json:"file_path"`
	Body     string `json:"body"`
}

// Decision is a caller-recorded observation. It carries no model metadata or
// free-form reason text so replay remains deterministic and safe to inspect.
type Decision struct {
	LeftID      string `json:"left_id"`
	RightID     string `json:"right_id"`
	Choice      Choice `json:"choice"`
	InputSHA256 string `json:"input_sha256"`
}

// Records contains the identity inputs without observations. It is the single
// input to Assemble, avoiding a second conflicting decision list.
type Records struct {
	SchemaVersion int              `json:"schema_version"`
	Task          Task             `json:"task"`
	Findings      []review.Finding `json:"findings"`
	Threads       []Thread         `json:"threads"`
}

// Input is the versioned replay wire object, including its recorded decisions.
type Input struct {
	SchemaVersion int              `json:"schema_version"`
	Task          Task             `json:"task"`
	Findings      []review.Finding `json:"findings"`
	Threads       []Thread         `json:"threads"`
	Decisions     []Decision       `json:"decisions"`
}

// Pair is one expected finding/finding or finding/thread comparison.
type Pair struct {
	LeftID      string `json:"left_id"`
	RightID     string `json:"right_id"`
	InputSHA256 string `json:"input_sha256"`
	Choice      Choice `json:"choice"`
}

// Group is a complete cohort component. MemberIDs retain finding input order.
type Group struct {
	RepresentativeID string   `json:"representative_id"`
	MemberIDs        []string `json:"member_ids"`
}

// Association records one same finding-to-prior-thread observation.
type Association struct {
	FindingID string `json:"finding_id"`
	ThreadID  string `json:"thread_id"`
}

// Issue identifies a missing, uncertain, failed, or contradictory pair.
type Issue struct {
	Code    string `json:"code"`
	LeftID  string `json:"left_id"`
	RightID string `json:"right_id"`
}

// Result is the deterministic replay output. Original input records are kept
// intact and remain present even when pair observations are incomplete.
type Result struct {
	SchemaVersion         int                  `json:"schema_version"`
	Task                  Task                 `json:"task"`
	Status                string               `json:"status"`
	Findings              []review.Finding     `json:"findings"`
	Threads               []Thread             `json:"threads"`
	Pairs                 []Pair               `json:"pairs"`
	Groups                []Group              `json:"groups"`
	Associations          []Association        `json:"associations"`
	Issues                []Issue              `json:"issues"`
	RollupDedupeLog       []review.DedupeEntry `json:"rollup_dedupe_log"`
	RollupOrderedFindings []review.FindingID   `json:"rollup_ordered_findings"`
}

// Decode parses a versioned replay fixture with recursive unknown-field and
// duplicate-key rejection. It never reads files or performs any I/O itself.
func Decode(data []byte) (Input, error) {
	if len(data) > MaxFixtureBytes {
		return Input{}, fmt.Errorf("reportidentity: fixture exceeds %d bytes", MaxFixtureBytes)
	}
	if !utf8.Valid(data) {
		return Input{}, errors.New("reportidentity: fixture is not valid UTF-8")
	}
	if !json.Valid(data) {
		return Input{}, errors.New("reportidentity: fixture is not exactly one valid JSON value")
	}
	if err := validateJSONUnicodeEscapes(data); err != nil {
		return Input{}, err
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Input{}, err
	}

	rootFields := fieldSet("schema_version", "task", "findings", "threads", "decisions")
	root, err := objectFields(data, "fixture", rootFields, rootFields)
	if err != nil {
		return Input{}, err
	}
	version, err := integerField(root, "schema_version", "fixture")
	if err != nil {
		return Input{}, err
	}
	if version != SchemaVersion {
		return Input{}, fmt.Errorf("reportidentity: unsupported schema_version %d", version)
	}
	taskValue, err := stringField(root, "task", "fixture")
	if err != nil {
		return Input{}, err
	}
	task := Task(taskValue)
	if task != TaskCohort && task != TaskPriorThread {
		return Input{}, fmt.Errorf("reportidentity: invalid task %q", taskValue)
	}

	findingItems, err := arrayField(root, "findings", "fixture")
	if err != nil {
		return Input{}, err
	}
	if len(findingItems) > maxFindings {
		return Input{}, fmt.Errorf("reportidentity: findings exceed limit %d", maxFindings)
	}
	threadItems, err := arrayField(root, "threads", "fixture")
	if err != nil {
		return Input{}, err
	}
	if len(threadItems) > maxThreads {
		return Input{}, fmt.Errorf("reportidentity: threads exceed limit %d", maxThreads)
	}
	decisionItems, err := arrayField(root, "decisions", "fixture")
	if err != nil {
		return Input{}, err
	}
	if len(decisionItems) > maxDecisions {
		return Input{}, fmt.Errorf("reportidentity: decisions exceed limit %d", maxDecisions)
	}
	if countExpectedPairs(task, len(findingItems), len(threadItems)) > maxPairs {
		return Input{}, fmt.Errorf("reportidentity: expected pairs exceed limit %d", maxPairs)
	}

	findings := make([]review.Finding, 0, len(findingItems))
	for index, raw := range findingItems {
		finding, err := decodeFinding(raw, index)
		if err != nil {
			return Input{}, err
		}
		findings = append(findings, finding)
	}
	threads := make([]Thread, 0, len(threadItems))
	for index, raw := range threadItems {
		thread, err := decodeThread(raw, index)
		if err != nil {
			return Input{}, err
		}
		threads = append(threads, thread)
	}
	decisions := make([]Decision, 0, len(decisionItems))
	for index, raw := range decisionItems {
		decision, err := decodeDecision(raw, index)
		if err != nil {
			return Input{}, err
		}
		decisions = append(decisions, decision)
	}

	input := Input{
		SchemaVersion: version,
		Task:          task,
		Findings:      findings,
		Threads:       threads,
		Decisions:     decisions,
	}
	if _, err := Replay(input); err != nil {
		return Input{}, err
	}
	return input, nil
}

// Replay validates a decoded input and assembles its deterministic result.
func Replay(input Input) (Result, error) {
	if input.Decisions == nil {
		return Result{}, errors.New("reportidentity: decisions must be an array")
	}
	return Assemble(recordsFor(input), input.Decisions)
}

// Assemble joins injected observations with expected pairs and returns the
// deterministic result without I/O or a provider callback.
func Assemble(records Records, decisions []Decision) (Result, error) {
	if err := validateRecords(records); err != nil {
		return Result{}, err
	}
	if len(decisions) > maxDecisions {
		return Result{}, fmt.Errorf("reportidentity: decisions exceed limit %d", maxDecisions)
	}
	if countExpectedPairs(records.Task, len(records.Findings), len(records.Threads)) > maxPairs {
		return Result{}, fmt.Errorf("reportidentity: expected pairs exceed limit %d", maxPairs)
	}

	expected, expectedByKey, findingIndexes, threadIndexes, err := enumeratePairs(records)
	if err != nil {
		return Result{}, err
	}
	for index, decision := range decisions {
		key, err := normalizeDecisionKey(records.Task, decision, findingIndexes, threadIndexes, expectedByKey)
		if err != nil {
			return Result{}, fmt.Errorf("reportidentity: decisions[%d]: %w", index, err)
		}
		pair := expectedByKey[key]
		if pair.decisionSeen {
			return Result{}, fmt.Errorf("reportidentity: decisions[%d] repeats pair %q/%q", index, pair.leftID, pair.rightID)
		}
		if !validChoice(decision.Choice) || decision.Choice == ChoiceMissing {
			return Result{}, fmt.Errorf("reportidentity: decisions[%d] has invalid choice %q", index, decision.Choice)
		}
		if !validDigest(decision.InputSHA256) {
			return Result{}, fmt.Errorf("reportidentity: decisions[%d] input_sha256 must be 64 lowercase hexadecimal characters", index)
		}
		if decision.InputSHA256 != pair.inputSHA256 {
			return Result{}, fmt.Errorf("reportidentity: decisions[%d] input_sha256 does not match pair projection", index)
		}
		pair.decisionSeen = true
		pair.choice = decision.Choice
	}

	result := Result{
		SchemaVersion:         SchemaVersion,
		Task:                  records.Task,
		Findings:              append([]review.Finding{}, records.Findings...),
		Threads:               append([]Thread{}, records.Threads...),
		Pairs:                 make([]Pair, 0, len(expected)),
		Groups:                []Group{},
		Associations:          []Association{},
		Issues:                []Issue{},
		RollupDedupeLog:       []review.DedupeEntry{},
		RollupOrderedFindings: []review.FindingID{},
	}
	for _, pair := range expected {
		choice := pair.choice
		if !pair.decisionSeen {
			choice = ChoiceMissing
		}
		result.Pairs = append(result.Pairs, Pair{
			LeftID:      pair.leftID,
			RightID:     pair.rightID,
			InputSHA256: pair.inputSHA256,
			Choice:      choice,
		})
	}

	if records.Task == TaskCohort {
		assembleCohort(&result, records.Findings, expected)
	} else {
		assemblePriorThreads(&result, expected)
	}
	return result, nil
}

func recordsFor(input Input) Records {
	return Records{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      input.Findings,
		Threads:       input.Threads,
	}
}

func validateRecords(records Records) error {
	if records.SchemaVersion != SchemaVersion {
		return fmt.Errorf("reportidentity: unsupported schema_version %d", records.SchemaVersion)
	}
	if records.Task != TaskCohort && records.Task != TaskPriorThread {
		return fmt.Errorf("reportidentity: invalid task %q", records.Task)
	}
	if records.Findings == nil || records.Threads == nil {
		return errors.New("reportidentity: findings and threads must be non-nil arrays")
	}
	if len(records.Findings) > maxFindings {
		return fmt.Errorf("reportidentity: findings exceed limit %d", maxFindings)
	}
	if len(records.Threads) > maxThreads {
		return fmt.Errorf("reportidentity: threads exceed limit %d", maxThreads)
	}
	if records.Task == TaskCohort && len(records.Threads) != 0 {
		return errors.New("reportidentity: cohort task requires an empty threads array")
	}
	if countExpectedPairs(records.Task, len(records.Findings), len(records.Threads)) > maxPairs {
		return fmt.Errorf("reportidentity: expected pairs exceed limit %d", maxPairs)
	}

	ids := make(map[string]struct{}, len(records.Findings)+len(records.Threads))
	for index, finding := range records.Findings {
		id := string(finding.ID)
		if err := validateID(id, fmt.Sprintf("findings[%d].finding_id", index)); err != nil {
			return err
		}
		if _, exists := ids[id]; exists {
			return fmt.Errorf("reportidentity: duplicate ID %q", id)
		}
		ids[id] = struct{}{}
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("reportidentity: findings[%d]: %w", index, err)
		}
		if !validExactEnum(string(finding.Severity), review.SeverityOrder()) {
			return fmt.Errorf("reportidentity: findings[%d].severity must use an exact lower-case enum", index)
		}
		if err := validatePathBody(finding.FilePath, finding.Body, fmt.Sprintf("findings[%d]", index)); err != nil {
			return err
		}
		if !validExactAnchor(finding.Anchor) {
			return fmt.Errorf("reportidentity: findings[%d].anchor must use exact lower-case enum values", index)
		}
		if finding.Anchoring != "" && !finding.Anchoring.Valid() {
			return fmt.Errorf("reportidentity: findings[%d].anchoring is invalid", index)
		}
	}
	for index, thread := range records.Threads {
		if err := validateID(thread.ID, fmt.Sprintf("threads[%d].thread_id", index)); err != nil {
			return err
		}
		if _, exists := ids[thread.ID]; exists {
			return fmt.Errorf("reportidentity: duplicate ID %q", thread.ID)
		}
		ids[thread.ID] = struct{}{}
		if err := validatePathBody(thread.FilePath, thread.Body, fmt.Sprintf("threads[%d]", index)); err != nil {
			return err
		}
	}
	return nil
}

func decodeFinding(data []byte, index int) (review.Finding, error) {
	where := fmt.Sprintf("findings[%d]", index)
	fields, err := objectFields(data, where, fieldSet("finding_id", "severity", "file_path", "anchor", "body", "anchoring"), fieldSet("finding_id", "severity", "file_path", "anchor", "body"))
	if err != nil {
		return review.Finding{}, err
	}
	id, err := stringField(fields, "finding_id", where)
	if err != nil {
		return review.Finding{}, err
	}
	severity, err := stringField(fields, "severity", where)
	if err != nil {
		return review.Finding{}, err
	}
	filePath, err := stringField(fields, "file_path", where)
	if err != nil {
		return review.Finding{}, err
	}
	anchor, err := decodeAnchor(fields["anchor"], where+".anchor")
	if err != nil {
		return review.Finding{}, err
	}
	body, err := stringField(fields, "body", where)
	if err != nil {
		return review.Finding{}, err
	}
	finding := review.Finding{
		ID:       review.FindingID(id),
		Severity: review.Severity(severity),
		FilePath: filePath,
		Anchor:   anchor,
		Body:     body,
	}
	if raw, present := fields["anchoring"]; present {
		anchoring, err := decodeString(raw, where+".anchoring")
		if err != nil {
			return review.Finding{}, err
		}
		if anchoring == "" {
			return review.Finding{}, fmt.Errorf("reportidentity: %s.anchoring must be omitted or a valid enum", where)
		}
		finding.Anchoring = review.Anchoring(anchoring)
	}
	if err := validateID(id, where+".finding_id"); err != nil {
		return review.Finding{}, err
	}
	if err := finding.Validate(); err != nil {
		return review.Finding{}, fmt.Errorf("reportidentity: %s: %w", where, err)
	}
	if !validExactEnum(severity, review.SeverityOrder()) {
		return review.Finding{}, fmt.Errorf("reportidentity: %s.severity must use an exact lower-case enum", where)
	}
	if err := validatePathBody(filePath, body, where); err != nil {
		return review.Finding{}, err
	}
	if !validExactAnchor(anchor) {
		return review.Finding{}, fmt.Errorf("reportidentity: %s.anchor must use exact lower-case enum values", where)
	}
	return finding, nil
}

func decodeAnchor(data []byte, where string) (review.Anchor, error) {
	fields, err := objectFields(data, where, fieldSet("kind", "side", "line"), fieldSet("kind"))
	if err != nil {
		return review.Anchor{}, err
	}
	kind, err := stringField(fields, "kind", where)
	if err != nil {
		return review.Anchor{}, err
	}
	if kind == string(review.AnchorKindFile) {
		if _, present := fields["side"]; present {
			return review.Anchor{}, fmt.Errorf("reportidentity: %s.side must be omitted for a file anchor", where)
		}
		if _, present := fields["line"]; present {
			return review.Anchor{}, fmt.Errorf("reportidentity: %s.line must be omitted for a file anchor", where)
		}
	}
	anchor := review.Anchor{Kind: review.AnchorKind(kind)}
	if raw, present := fields["side"]; present {
		side, err := decodeString(raw, where+".side")
		if err != nil {
			return review.Anchor{}, err
		}
		anchor.Side = review.DiffSide(side)
	}
	if raw, present := fields["line"]; present {
		line, err := decodeInteger(raw, where+".line")
		if err != nil {
			return review.Anchor{}, err
		}
		anchor.Line = line
	}
	if err := anchor.Validate(); err != nil {
		return review.Anchor{}, fmt.Errorf("reportidentity: %s: %w", where, err)
	}
	if anchor.Kind != review.AnchorKindFile && anchor.Kind != review.AnchorKindLine {
		return review.Anchor{}, fmt.Errorf("reportidentity: %s.kind must use an exact lower-case enum", where)
	}
	if anchor.Side != "" && anchor.Side != review.DiffSideLeft && anchor.Side != review.DiffSideRight {
		return review.Anchor{}, fmt.Errorf("reportidentity: %s.side must use an exact upper-case enum", where)
	}
	return anchor, nil
}

func decodeThread(data []byte, index int) (Thread, error) {
	where := fmt.Sprintf("threads[%d]", index)
	fields, err := objectFields(data, where, fieldSet("thread_id", "eligible", "file_path", "body"), fieldSet("thread_id", "eligible", "file_path", "body"))
	if err != nil {
		return Thread{}, err
	}
	id, err := stringField(fields, "thread_id", where)
	if err != nil {
		return Thread{}, err
	}
	eligible, err := booleanField(fields, "eligible", where)
	if err != nil {
		return Thread{}, err
	}
	filePath, err := stringField(fields, "file_path", where)
	if err != nil {
		return Thread{}, err
	}
	body, err := stringField(fields, "body", where)
	if err != nil {
		return Thread{}, err
	}
	thread := Thread{ID: id, Eligible: eligible, FilePath: filePath, Body: body}
	if err := validateID(id, where+".thread_id"); err != nil {
		return Thread{}, err
	}
	if err := validatePathBody(filePath, body, where); err != nil {
		return Thread{}, err
	}
	return thread, nil
}

func decodeDecision(data []byte, index int) (Decision, error) {
	where := fmt.Sprintf("decisions[%d]", index)
	fields, err := objectFields(data, where, fieldSet("left_id", "right_id", "choice", "input_sha256"), fieldSet("left_id", "right_id", "choice", "input_sha256"))
	if err != nil {
		return Decision{}, err
	}
	left, err := stringField(fields, "left_id", where)
	if err != nil {
		return Decision{}, err
	}
	right, err := stringField(fields, "right_id", where)
	if err != nil {
		return Decision{}, err
	}
	choice, err := stringField(fields, "choice", where)
	if err != nil {
		return Decision{}, err
	}
	digest, err := stringField(fields, "input_sha256", where)
	if err != nil {
		return Decision{}, err
	}
	if err := validateID(left, where+".left_id"); err != nil {
		return Decision{}, err
	}
	if err := validateID(right, where+".right_id"); err != nil {
		return Decision{}, err
	}
	decision := Decision{LeftID: left, RightID: right, Choice: Choice(choice), InputSHA256: digest}
	if !validChoice(decision.Choice) || decision.Choice == ChoiceMissing {
		return Decision{}, fmt.Errorf("reportidentity: %s.choice is invalid", where)
	}
	if !validDigest(digest) {
		return Decision{}, fmt.Errorf("reportidentity: %s.input_sha256 must be 64 lowercase hexadecimal characters", where)
	}
	return decision, nil
}

func objectFields(data []byte, where string, allowed, required map[string]struct{}) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("reportidentity: %s must be an object: %w", where, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("reportidentity: %s must be an object", where)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("reportidentity: %s has unknown field %q", where, key)
		}
	}
	for key := range required {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("reportidentity: %s is missing required field %q", where, key)
		}
	}
	return fields, nil
}

func fieldSet(fields ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	return set
}

func stringField(fields map[string]json.RawMessage, name, where string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("reportidentity: %s is missing required field %q", where, name)
	}
	return decodeString(raw, where+"."+name)
}

func decodeString(raw json.RawMessage, where string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("reportidentity: %s must be a non-null string", where)
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("reportidentity: %s must be a string: %w", where, err)
	}
	return value, nil
}

func integerField(fields map[string]json.RawMessage, name, where string) (int, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, fmt.Errorf("reportidentity: %s is missing required field %q", where, name)
	}
	return decodeInteger(raw, where+"."+name)
}

func decodeInteger(raw json.RawMessage, where string) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return 0, fmt.Errorf("reportidentity: %s must be a non-null integer", where)
	}
	var value int
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return 0, fmt.Errorf("reportidentity: %s must be an integer: %w", where, err)
	}
	return value, nil
}

func booleanField(fields map[string]json.RawMessage, name, where string) (bool, error) {
	raw, ok := fields[name]
	if !ok {
		return false, fmt.Errorf("reportidentity: %s is missing required field %q", where, name)
	}
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) != "true" && string(trimmed) != "false" {
		return false, fmt.Errorf("reportidentity: %s.%s must be a non-null boolean", where, name)
	}
	return string(trimmed) == "true", nil
}

func arrayField(fields map[string]json.RawMessage, name, where string) ([]json.RawMessage, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, fmt.Errorf("reportidentity: %s is missing required field %q", where, name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("reportidentity: %s.%s must be a non-null array", where, name)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, fmt.Errorf("reportidentity: %s.%s must be an array: %w", where, name, err)
	}
	return values, nil
}

func validateID(value, where string) error {
	if len(value) == 0 || len(value) > maxIDBytes {
		return fmt.Errorf("reportidentity: %s must contain 1 to %d bytes", where, maxIDBytes)
	}
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return fmt.Errorf("reportidentity: %s must use only printable ASCII letters, digits, '.', '_' and '-'", where)
	}
	return nil
}

func validatePathBody(filePath, body, where string) error {
	if filePath == "" || strings.TrimSpace(filePath) == "" {
		return fmt.Errorf("reportidentity: %s.file_path must be non-empty and not whitespace-only", where)
	}
	if !utf8.ValidString(filePath) || !utf8.ValidString(body) {
		return fmt.Errorf("reportidentity: %s path and body must be valid UTF-8", where)
	}
	if len(filePath) > maxPathBytes {
		return fmt.Errorf("reportidentity: %s.file_path exceeds %d bytes", where, maxPathBytes)
	}
	if body == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("reportidentity: %s.body must be non-empty and not whitespace-only", where)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("reportidentity: %s.body exceeds %d bytes", where, maxBodyBytes)
	}
	return nil
}

func validExactEnum(value string, choices []review.Severity) bool {
	for _, choice := range choices {
		if value == string(choice) {
			return true
		}
	}
	return false
}

func validExactAnchor(anchor review.Anchor) bool {
	return (anchor.Kind == review.AnchorKindFile && anchor.Side == "" && anchor.Line == 0) ||
		(anchor.Kind == review.AnchorKindLine && (anchor.Side == review.DiffSideLeft || anchor.Side == review.DiffSideRight) && anchor.Line > 0)
}

func validChoice(choice Choice) bool {
	switch choice {
	case ChoiceSame, ChoiceDifferent, ChoiceAbstain, ChoiceError:
		return true
	case ChoiceMissing:
		return false
	default:
		return false
	}
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range []byte(value) {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func countExpectedPairs(task Task, findings, threads int) int {
	if task == TaskCohort {
		return findings * (findings - 1) / 2
	}
	return findings * threads
}

func validateJSONUnicodeEscapes(data []byte) error {
	for index := 0; index < len(data); {
		if data[index] != '"' {
			index++
			continue
		}
		index++
		for index < len(data) && data[index] != '"' {
			if data[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(data) {
				return errors.New("reportidentity: invalid string escape")
			}
			if data[index+1] != 'u' {
				index += 2
				continue
			}
			codepoint, ok := hexCodepoint(data[index+2 : index+6])
			if !ok {
				return errors.New("reportidentity: invalid Unicode escape")
			}
			if codepoint >= 0xdc00 && codepoint <= 0xdfff {
				return errors.New("reportidentity: unpaired low-surrogate escape")
			}
			if codepoint >= 0xd800 && codepoint <= 0xdbff {
				second := index + 6
				if second+6 > len(data) || data[second] != '\\' || data[second+1] != 'u' {
					return errors.New("reportidentity: unpaired high-surrogate escape")
				}
				low, ok := hexCodepoint(data[second+2 : second+6])
				if !ok || low < 0xdc00 || low > 0xdfff {
					return errors.New("reportidentity: unpaired high-surrogate escape")
				}
				index = second + 6
				continue
			}
			index += 6
		}
		if index < len(data) {
			index++
		}
	}
	return nil
}

func hexCodepoint(data []byte) (uint16, bool) {
	if len(data) != 4 {
		return 0, false
	}
	var value uint16
	for _, character := range data {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("reportidentity: decode fixture: %w", err)
	}
	if err := scanJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("reportidentity: fixture contains trailing JSON")
		}
		return fmt.Errorf("reportidentity: fixture has trailing data: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("reportidentity: read JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("reportidentity: JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("reportidentity: duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("reportidentity: read JSON object value: %w", err)
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("reportidentity: invalid JSON object")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("reportidentity: read JSON array value: %w", err)
			}
			if err := scanJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("reportidentity: invalid JSON array")
		}
	default:
		return errors.New("reportidentity: invalid JSON delimiter")
	}
	return nil
}

type pairKey struct {
	left  string
	right string
}

type expectedPair struct {
	leftIndex    int
	rightIndex   int
	leftID       string
	rightID      string
	inputSHA256  string
	decisionSeen bool
	choice       Choice
}

type pairProjection struct {
	SchemaVersion int  `json:"schema_version"`
	Task          Task `json:"task"`
	Left          any  `json:"left"`
	Right         any  `json:"right"`
}

func pairDigest(task Task, left, right any) (string, error) {
	projection := pairProjection{SchemaVersion: SchemaVersion, Task: task, Left: left, Right: right}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(projection); err != nil {
		return "", fmt.Errorf("reportidentity: encode pair projection: %w", err)
	}
	compact := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	digest := sha256.Sum256(compact)
	return hex.EncodeToString(digest[:]), nil
}
