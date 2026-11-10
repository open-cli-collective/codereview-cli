package identityshadow

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const priorRuleV1 = "planner_exact_path_normalized_body_v1"

type baselineChoice string

const (
	baselineSame      baselineChoice = "same"
	baselineDifferent baselineChoice = "different"
	baselineUnknown   baselineChoice = "unknown"
)

// Comparison is the private, hash-bound comparison for one snapshot.
type Comparison struct {
	SchemaVersion  int               `json:"schema_version"`
	SourceSHA256   string            `json:"source_sha256"`
	SnapshotSHA256 string            `json:"snapshot_sha256"`
	BaselineSHA256 string            `json:"baseline_sha256"`
	Cohort         ComparisonSection `json:"cohort"`
	PriorExact     ComparisonSection `json:"prior_exact_normalization"`
}

// ComparisonSection records full-universe counts for one identity task.
type ComparisonSection struct {
	ExpectedPairs         int              `json:"expected_pairs"`
	BaselineSame          int              `json:"baseline_same"`
	BaselineDifferent     int              `json:"baseline_different"`
	BaselineUnknown       int              `json:"baseline_unknown"`
	ShadowSame            int              `json:"shadow_same"`
	ShadowDifferent       int              `json:"shadow_different"`
	ShadowAbstain         int              `json:"shadow_abstain"`
	ShadowError           int              `json:"shadow_error"`
	ShadowMissing         int              `json:"shadow_missing"`
	ComparablePairs       int              `json:"comparable_pairs"`
	Agreements            int              `json:"agreements"`
	Disagreements         int              `json:"disagreements"`
	UnresolvedPairs       int              `json:"unresolved_pairs"`
	InvalidPredictionRows int              `json:"invalid_prediction_rows"`
	Cells                 DirectionalCells `json:"cells"`
	Pairs                 []PairComparison `json:"pairs"`
}

// DirectionalCells records the four same/different comparison cells.
type DirectionalCells struct {
	BaselineSameShadowSame           int `json:"baseline_same_shadow_same"`
	BaselineSameShadowDifferent      int `json:"baseline_same_shadow_different"`
	BaselineDifferentShadowSame      int `json:"baseline_different_shadow_same"`
	BaselineDifferentShadowDifferent int `json:"baseline_different_shadow_different"`
}

// PairComparison records one expected pair and its diagnostic disposition.
type PairComparison struct {
	LeftID           string                `json:"left_id"`
	RightID          string                `json:"right_id"`
	InputSHA256      string                `json:"input_sha256"`
	BaselineChoice   baselineChoice        `json:"baseline_choice"`
	PredictionChoice reportidentity.Choice `json:"prediction_choice"`
	Disposition      string                `json:"disposition"`
	DiagnosticCode   string                `json:"diagnostic_code,omitempty"`
}

type expectedPairKey struct {
	left  string
	right string
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// compareIdentity compares the frozen accepted planner behavior with the two
// shadow outputs. It has no filesystem, network, ledger or request-building
// effects; reportidentity only derives the expected replay pair universes.
func compareIdentity(
	snapshot reportidentitysnapshot.Snapshot,
	cohort reportidentity.Result,
	prior reportidentity.Result,
	priorIDMap map[string]string,
) Comparison {
	comparison := Comparison{SchemaVersion: 1}
	sourceRecord, sourceBytes, sourceErr := makeSourceArtifact(snapshot, priorIDMap)
	if sourceErr == nil {
		comparison.SourceSHA256 = digest(sourceBytes)
		comparison.SnapshotSHA256 = sourceRecord.SnapshotSHA256
		_, baselineBytes, baselineErr := makeBaselineArtifact(snapshot, sourceRecord.SnapshotSHA256)
		if baselineErr == nil {
			comparison.BaselineSHA256 = digest(baselineBytes)
		}
	}
	comparison.Cohort = compareSection(snapshot, reportidentity.TaskCohort, cohort, priorIDMap)
	comparison.PriorExact = compareSection(snapshot, reportidentity.TaskPriorThread, prior, priorIDMap)
	return comparison
}

func compareSection(
	snapshot reportidentitysnapshot.Snapshot,
	task reportidentity.Task,
	shadow reportidentity.Result,
	priorIDMap map[string]string,
) ComparisonSection {
	section := ComparisonSection{Pairs: []PairComparison{}}
	records, sourceCode := comparisonRecords(snapshot, task, priorIDMap)
	baseReplay, err := reportidentity.Replay(reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion, Task: task,
		Findings:  append([]review.Finding{}, records.Findings...),
		Threads:   append([]reportidentity.Thread{}, records.Threads...),
		Decisions: []reportidentity.Decision{},
	})
	if err != nil {
		return section
	}
	pairs := baseReplay.Pairs
	section.ExpectedPairs = len(pairs)
	var baselineChoices map[expectedPairKey]baselineChoice
	baselineCodes := map[expectedPairKey]string{}
	invalidSourceCode := sourceCode
	if task == reportidentity.TaskCohort {
		baselineChoices, invalidSourceCode = cohortBaselineChoices(snapshot, pairs)
		if invalidSourceCode != "" {
			baselineCodes = codesForPairs(pairs, invalidSourceCode)
		}
	} else {
		priorChoices, priorCodes, priorBindingOK := priorBaselineChoices(snapshot, pairs, priorIDMap)
		baselineChoices = priorChoices
		baselineCodes = priorCodes
		if !priorBindingOK {
			for _, pair := range pairs {
				baselineChoices[pairKey(pair)] = baselineUnknown
				baselineCodes[pairKey(pair)] = "invalid_source_binding"
			}
		}
	}

	predictions, invalidRows, invalidCode := bindPredictions(task, shadow, pairs)
	section.InvalidPredictionRows = invalidRows
	for _, pair := range pairs {
		key := pairKey(pair)
		baseline := baselineChoices[key]
		if baseline == "" {
			baseline = baselineUnknown
			if baselineCodes[key] == "" {
				baselineCodes[key] = "baseline_coverage_incomplete"
			}
		}
		switch baseline {
		case baselineSame:
			section.BaselineSame++
		case baselineDifferent:
			section.BaselineDifferent++
		case baselineUnknown:
			section.BaselineUnknown++
		}

		prediction := reportidentity.ChoiceMissing
		diagnostic := baselineCodes[key]
		if invalidRows > 0 {
			diagnostic = "invalid_prediction_binding"
		} else if actual, ok := predictions[key]; ok {
			prediction = actual
		} else if diagnostic == "" {
			diagnostic = "shadow_missing"
		}
		comparisonPair := PairComparison{
			LeftID: pair.LeftID, RightID: pair.RightID, InputSHA256: pair.InputSHA256,
			BaselineChoice: baseline, PredictionChoice: prediction,
		}
		switch prediction {
		case reportidentity.ChoiceSame:
			section.ShadowSame++
		case reportidentity.ChoiceDifferent:
			section.ShadowDifferent++
		case reportidentity.ChoiceAbstain:
			section.ShadowAbstain++
			if diagnostic == "" {
				diagnostic = "shadow_abstain"
			}
		case reportidentity.ChoiceError:
			section.ShadowError++
			if diagnostic == "" {
				diagnostic = "shadow_error"
			}
		case reportidentity.ChoiceMissing:
			section.ShadowMissing++
			if diagnostic == "" {
				diagnostic = "shadow_missing"
			}
		default:
			section.ShadowMissing++
			if diagnostic == "" {
				diagnostic = "shadow_missing"
			}
		}

		if baseline != baselineUnknown && (prediction == reportidentity.ChoiceSame || prediction == reportidentity.ChoiceDifferent) {
			section.ComparablePairs++
			if baseline == baselineSame && prediction == reportidentity.ChoiceSame {
				section.Cells.BaselineSameShadowSame++
			} else if baseline == baselineSame && prediction == reportidentity.ChoiceDifferent {
				section.Cells.BaselineSameShadowDifferent++
			} else if baseline == baselineDifferent && prediction == reportidentity.ChoiceSame {
				section.Cells.BaselineDifferentShadowSame++
			} else if baseline == baselineDifferent && prediction == reportidentity.ChoiceDifferent {
				section.Cells.BaselineDifferentShadowDifferent++
			}
			if (baseline == baselineSame) == (prediction == reportidentity.ChoiceSame) {
				section.Agreements++
				comparisonPair.Disposition = "agreement"
			} else {
				section.Disagreements++
				comparisonPair.Disposition = "disagreement"
			}
		} else {
			section.UnresolvedPairs++
			comparisonPair.Disposition = "unresolved"
			if diagnostic == "" {
				diagnostic = invalidCode
			}
			if diagnostic == "" {
				diagnostic = invalidSourceCode
			}
		}
		if diagnostic != "" && comparisonPair.Disposition == "unresolved" {
			comparisonPair.DiagnosticCode = diagnostic
		}
		section.Pairs = append(section.Pairs, comparisonPair)
	}
	return section
}

func comparisonRecords(snapshot reportidentitysnapshot.Snapshot, task reportidentity.Task, priorIDMap map[string]string) (reportidentity.Records, string) {
	records := reportidentity.Records{
		SchemaVersion: reportidentity.SchemaVersion, Task: task,
		Findings: append([]review.Finding{}, snapshot.Findings...), Threads: []reportidentity.Thread{},
	}
	if records.Findings == nil {
		records.Findings = []review.Finding{}
	}
	if task != reportidentity.TaskPriorThread {
		return records, ""
	}
	replayToOriginal := make(map[string]string, len(snapshot.PriorRoots))
	code := ""
	for _, root := range snapshot.PriorRoots {
		replayID, exists := priorIDMap[root.ID]
		if !exists || replayID == "" || replayID != deterministicThreadID(root.ID) {
			code = "invalid_source_binding"
			replayID = deterministicThreadID(root.ID)
		}
		if _, duplicate := replayToOriginal[replayID]; duplicate {
			return records, "invalid_source_binding"
		}
		replayToOriginal[replayID] = root.ID
		records.Threads = append(records.Threads, reportidentity.Thread{
			ID: replayID, Eligible: true, FilePath: root.FilePath, Body: root.Body,
		})
	}
	return records, code
}

func cohortBaselineChoices(snapshot reportidentitysnapshot.Snapshot, pairs []reportidentity.Pair) (map[expectedPairKey]baselineChoice, string) {
	choices := make(map[expectedPairKey]baselineChoice, len(pairs))
	baseline := snapshot.Baseline
	if baseline.SchemaVersion != 1 {
		return allUnknown(pairs), "baseline_schema_unavailable"
	}
	ids := make(map[string]struct{}, len(snapshot.Findings))
	for _, finding := range snapshot.Findings {
		id := string(finding.ID)
		if id == "" {
			return allUnknown(pairs), "baseline_input_invalid"
		}
		if _, exists := ids[id]; exists {
			return allUnknown(pairs), "baseline_input_invalid"
		}
		ids[id] = struct{}{}
	}
	groups := make(map[string]string, len(ids))
	switch baseline.CohortStatus {
	case "fallback_no_dedupe":
		if len(baseline.Dedupe) != 0 {
			return allUnknown(pairs), "baseline_partition_invalid"
		}
		ordered, ok := validOrderedIDs(baseline.EffectiveOrderedIDs, ids)
		if !ok || len(ordered) != len(ids) {
			return allUnknown(pairs), "baseline_coverage_incomplete"
		}
		for id := range ids {
			groups[id] = id
		}
	case "accepted_rollup":
		droppedToKept := make(map[string]string)
		keptSet := make(map[string]struct{})
		for _, entry := range baseline.Dedupe {
			kept := string(entry.Kept)
			if _, exists := ids[kept]; !exists {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
			keptSet[kept] = struct{}{}
			for _, dropped := range entry.Dropped {
				droppedID := string(dropped)
				if _, exists := ids[droppedID]; !exists {
					return allUnknown(pairs), "baseline_partition_invalid"
				}
				if _, duplicate := droppedToKept[droppedID]; duplicate {
					return allUnknown(pairs), "baseline_partition_invalid"
				}
				droppedToKept[droppedID] = kept
			}
		}
		ordered, ok := validOrderedIDs(baseline.EffectiveOrderedIDs, ids)
		if !ok {
			return allUnknown(pairs), "baseline_partition_invalid"
		}
		for kept := range keptSet {
			if _, isDropped := droppedToKept[kept]; isDropped {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
			if _, ordered := ordered[kept]; !ordered {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
		}
		for dropped, kept := range droppedToKept {
			if dropped == kept {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
			if _, keptWasDropped := droppedToKept[kept]; keptWasDropped {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
			if _, survives := ordered[kept]; !survives {
				return allUnknown(pairs), "baseline_partition_invalid"
			}
		}
		if len(ordered) != len(ids)-len(droppedToKept) {
			return allUnknown(pairs), "baseline_coverage_incomplete"
		}
		for id := range ids {
			if kept, exists := droppedToKept[id]; exists {
				groups[id] = kept
			} else {
				groups[id] = id
			}
		}
	default:
		return allUnknown(pairs), "baseline_cohort_unavailable"
	}
	for _, pair := range pairs {
		left, leftOK := groups[pair.LeftID]
		right, rightOK := groups[pair.RightID]
		if !leftOK || !rightOK {
			return allUnknown(pairs), "baseline_coverage_incomplete"
		}
		if left == right {
			choices[pairKey(pair)] = baselineSame
		} else {
			choices[pairKey(pair)] = baselineDifferent
		}
	}
	return choices, ""
}

func priorBaselineChoices(snapshot reportidentitysnapshot.Snapshot, pairs []reportidentity.Pair, priorIDMap map[string]string) (map[expectedPairKey]baselineChoice, map[expectedPairKey]string, bool) {
	choices := make(map[expectedPairKey]baselineChoice, len(pairs))
	codes := make(map[expectedPairKey]string)
	baseline := snapshot.Baseline
	if baseline.SchemaVersion != 1 || baseline.PriorRule != priorRuleV1 {
		return allUnknown(pairs), codesForPairs(pairs, "baseline_rule_unavailable"), true
	}
	keyValues := make(map[string]*string, len(baseline.PriorKeys))
	invalidRecords := make(map[string]string)
	validRecordIDs := make(map[string]map[string]struct{}, 2)
	validRecordIDs["finding"] = make(map[string]struct{}, len(snapshot.Findings))
	for _, finding := range snapshot.Findings {
		validRecordIDs["finding"][string(finding.ID)] = struct{}{}
	}
	validRecordIDs["thread"] = make(map[string]struct{}, len(snapshot.PriorRoots))
	for _, root := range snapshot.PriorRoots {
		validRecordIDs["thread"][root.ID] = struct{}{}
	}
	for _, record := range baseline.PriorKeys {
		knownIDs, knownKind := validRecordIDs[record.RecordKind]
		if !knownKind {
			return allUnknown(pairs), codesForPairs(pairs, "baseline_keys_invalid"), true
		}
		if _, knownID := knownIDs[record.RecordID]; !knownID {
			return allUnknown(pairs), codesForPairs(pairs, "baseline_keys_invalid"), true
		}
		key := record.RecordKind + "\x00" + record.RecordID
		if _, duplicate := keyValues[key]; duplicate {
			invalidRecords[key] = "baseline_keys_invalid"
			continue
		}
		keyValues[key] = record.KeySHA256
		if record.KeySHA256 != nil && !digestPattern.MatchString(*record.KeySHA256) {
			invalidRecords[key] = "baseline_key_invalid"
		}
	}
	for _, finding := range snapshot.Findings {
		key := "finding\x00" + string(finding.ID)
		if _, exists := keyValues[key]; !exists {
			invalidRecords[key] = "baseline_key_missing"
		}
	}
	for _, root := range snapshot.PriorRoots {
		key := "thread\x00" + root.ID
		if _, exists := keyValues[key]; !exists {
			invalidRecords[key] = "baseline_key_missing"
		}
	}

	replayToOriginal := make(map[string]string, len(priorIDMap))
	bindingOK := len(priorIDMap) == len(snapshot.PriorRoots)
	for _, root := range snapshot.PriorRoots {
		replayID, exists := priorIDMap[root.ID]
		if !exists || replayID == "" || replayID != deterministicThreadID(root.ID) {
			bindingOK = false
			continue
		}
		if _, duplicate := replayToOriginal[replayID]; duplicate {
			bindingOK = false
			continue
		}
		replayToOriginal[replayID] = root.ID
	}
	if !bindingOK {
		return allUnknown(pairs), codesForPairs(pairs, "invalid_source_binding"), false
	}
	for _, pair := range pairs {
		rootID, mapped := replayToOriginal[pair.RightID]
		leftHash := keyValues["finding\x00"+pair.LeftID]
		rightHash := keyValues["thread\x00"+rootID]
		keyLeft := "finding\x00" + pair.LeftID
		keyRight := "thread\x00" + rootID
		if !mapped {
			choices[pairKey(pair)] = baselineUnknown
			codes[pairKey(pair)] = "invalid_source_binding"
			continue
		}
		if code := invalidRecords[keyLeft]; code != "" {
			choices[pairKey(pair)] = baselineUnknown
			codes[pairKey(pair)] = code
			continue
		}
		if code := invalidRecords[keyRight]; code != "" {
			choices[pairKey(pair)] = baselineUnknown
			codes[pairKey(pair)] = code
			continue
		}
		if leftHash == nil || rightHash == nil {
			choices[pairKey(pair)] = baselineUnknown
			codes[pairKey(pair)] = "baseline_key_invalid"
			continue
		}
		if *leftHash == *rightHash {
			choices[pairKey(pair)] = baselineSame
		} else {
			choices[pairKey(pair)] = baselineDifferent
		}
	}
	return choices, codes, true
}

func validOrderedIDs(values []review.FindingID, known map[string]struct{}) (map[string]struct{}, bool) {
	ordered := make(map[string]struct{}, len(values))
	for _, raw := range values {
		id := string(raw)
		if _, exists := known[id]; !exists {
			return nil, false
		}
		if _, duplicate := ordered[id]; duplicate {
			return nil, false
		}
		ordered[id] = struct{}{}
	}
	return ordered, true
}

func bindPredictions(task reportidentity.Task, shadow reportidentity.Result, expected []reportidentity.Pair) (map[expectedPairKey]reportidentity.Choice, int, string) {
	bound := make(map[expectedPairKey]reportidentity.Choice, len(expected))
	expectedByIDs := make(map[expectedPairKey]reportidentity.Pair, len(expected))
	for _, pair := range expected {
		expectedByIDs[pairKey(pair)] = pair
	}
	invalidRows := 0
	if shadow.Task != task {
		invalidRows++
	}
	seen := make(map[expectedPairKey]struct{}, len(shadow.Pairs))
	for _, row := range shadow.Pairs {
		key := expectedPairKey{left: row.LeftID, right: row.RightID}
		want, exists := expectedByIDs[key]
		if !exists {
			invalidRows++
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			invalidRows++
			continue
		}
		seen[key] = struct{}{}
		if row.InputSHA256 != want.InputSHA256 || !digestPattern.MatchString(row.InputSHA256) || !validPrediction(row.Choice) {
			invalidRows++
			continue
		}
		bound[key] = row.Choice
	}
	if invalidRows > 0 {
		return map[expectedPairKey]reportidentity.Choice{}, invalidRows, "invalid_prediction_binding"
	}
	return bound, 0, ""
}

func validPrediction(choice reportidentity.Choice) bool {
	switch choice {
	case reportidentity.ChoiceSame, reportidentity.ChoiceDifferent, reportidentity.ChoiceAbstain, reportidentity.ChoiceError, reportidentity.ChoiceMissing:
		return true
	default:
		return false
	}
}

func allUnknown(pairs []reportidentity.Pair) map[expectedPairKey]baselineChoice {
	unknown := make(map[expectedPairKey]baselineChoice, len(pairs))
	for _, pair := range pairs {
		unknown[pairKey(pair)] = baselineUnknown
	}
	return unknown
}

func codesForPairs(pairs []reportidentity.Pair, code string) map[expectedPairKey]string {
	codes := make(map[expectedPairKey]string, len(pairs))
	for _, pair := range pairs {
		codes[pairKey(pair)] = code
	}
	return codes
}

func pairKey(pair reportidentity.Pair) expectedPairKey {
	return expectedPairKey{left: pair.LeftID, right: pair.RightID}
}

func deterministicThreadID(original string) string {
	if validReplayID(original) {
		return original
	}
	value := sha256.Sum256([]byte(original))
	return "shadow_thread_" + hex.EncodeToString(value[:])
}

func validReplayID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
