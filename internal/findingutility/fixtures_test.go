package findingutility

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"math"
	"testing"
)

// These fixtures are executable contract vectors, not documentation-only
// placeholders. Keep their schema strict so a case cannot silently stop being
// exercised when the checked-in data changes.
//
//go:embed testdata/policy-cases.json
var policyCasesFixture []byte

//go:embed testdata/advisory-golden.json
var advisoryGoldenFixture []byte

//go:embed testdata/fixture-evaluation.json
var evaluationFixtureData []byte

type policyFixture struct {
	SchemaVersion         int                   `json:"schema_version"`
	PolicyVersion         string                `json:"policy_version"`
	ThresholdsAreTestOnly bool                  `json:"thresholds_are_test_only"`
	Coverage              []string              `json:"coverage"`
	BinaryIDs             []string              `json:"binary_ids"`
	StrictThresholds      []strictThresholdCase `json:"strict_threshold_cases"`
	Cases                 []policyCase          `json:"cases"`
}

type strictThresholdCase struct {
	ID       string `json:"id"`
	Answer   string `json:"answer"`
	Expected string `json:"expected"`
}

type policyCase struct {
	ID                  string `json:"id"`
	Primary             string `json:"primary"`
	Representative      string `json:"representative"`
	Eligibility         string `json:"eligibility"`
	ExpectedProposed    string `json:"expected_proposed"`
	ExpectedEffective   string `json:"expected_effective"`
	Introduced          *bool  `json:"introduced"`
	RemediationRequired *bool  `json:"remediation_required"`
	Score               string `json:"score"`
	Answer              string `json:"answer"`
	Expected            string `json:"expected"`
	Body                string `json:"body"`
	StateField          string `json:"state_field"`
}

type goldenFixture struct {
	SchemaVersion              int             `json:"schema_version"`
	ProfileID                  string          `json:"profile_id"`
	RunID                      string          `json:"run_id"`
	CohortInputDigest          Digest          `json:"cohort_input_digest"`
	EffectiveDecisionInvariant Disposition     `json:"effective_decision_invariant"`
	Findings                   []goldenFinding `json:"findings"`
	Invariance                 []string        `json:"invariance"`
}

type goldenFinding struct {
	FindingID         string      `json:"finding_id"`
	Severity          string      `json:"severity"`
	Body              string      `json:"body"`
	ExpectedProposed  Disposition `json:"expected_proposed_decision"`
	ExpectedEffective Disposition `json:"expected_effective_decision"`
}

type evaluationFixture struct {
	SchemaVersion         int              `json:"schema_version"`
	Backend               string           `json:"backend"`
	RequestedModel        string           `json:"requested_model"`
	AllowedResolvedModels []string         `json:"allowed_resolved_models"`
	Cases                 []evaluationCase `json:"cases"`
}

type evaluationCase struct {
	ID              string  `json:"id"`
	StateDigest     Digest  `json:"state_digest"`
	QuestionsDigest Digest  `json:"questions_digest"`
	RequestedModel  string  `json:"requested_model"`
	ResolvedModel   string  `json:"resolved_model"`
	ResponseBase64  *string `json:"response_base64"`
	WaitError       *string `json:"wait_error"`
	Deadline        bool    `json:"deadline"`
}

func TestPolicyAndGoldenFixturesExecuteAllDeclaredBranches(t *testing.T) {
	var fixture policyFixture
	if err := DecodeStrict(policyCasesFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || fixture.PolicyVersion != PolicyVersion || !fixture.ThresholdsAreTestOnly {
		t.Fatalf("policy fixture identity = %#v", fixture)
	}
	if len(fixture.BinaryIDs) != len(allBinaryIDs) {
		t.Fatalf("policy fixture Binary count = %d, want %d", len(fixture.BinaryIDs), len(allBinaryIDs))
	}
	for index, id := range allBinaryIDs {
		if fixture.BinaryIDs[index] != id.String() {
			t.Fatalf("policy fixture Binary[%d] = %q, want %q", index, fixture.BinaryIDs[index], id)
		}
	}
	for _, thresholdCase := range fixture.StrictThresholds {
		if thresholdCase.Expected == "" || thresholdCase.Answer == "" {
			t.Fatalf("strict threshold case is not executable: %#v", thresholdCase)
		}
		executeStrictThresholdFixture(t, thresholdCase)
	}

	coverage := make(map[string]bool, len(fixture.Coverage))
	for _, value := range fixture.Coverage {
		coverage[value] = true
	}
	for _, required := range []string{ReasonInvalidState, ReasonStaleInput, ReasonInputNotPermitted, ReasonUncalibratedPolicy, ReasonEligibilityUnknown, ReasonIncompleteContext, ReasonRequired, "useful_nonblocking", "insufficient_context", "other", ReasonDuplicateInvalidRepresentative, ReasonDuplicateRepresentativeNotKept} {
		if !coverage[required] {
			t.Fatalf("policy fixture omits coverage for %q", required)
		}
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			executePolicyFixtureCase(t, testCase)
		})
	}

	var golden goldenFixture
	if err := DecodeStrict(advisoryGoldenFixture, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.SchemaVersion != 1 || golden.ProfileID != FixtureProfileID || golden.EffectiveDecisionInvariant != DispositionKeep || !ValidDigest(golden.CohortInputDigest) {
		t.Fatalf("golden fixture identity = %#v", golden)
	}
	for _, required := range []string{"raw_findings", "finding_order", "severity", "rollup", "review_event", "inline_actions", "fail_on", "outbox", "resume_retry", "github_output"} {
		found := false
		for _, value := range golden.Invariance {
			if value == required {
				found = true
			}
		}
		if !found {
			t.Fatalf("golden fixture omits invariance %q", required)
		}
	}
	executeGoldenFixture(t, golden)
}

func executeStrictThresholdFixture(t *testing.T, testCase strictThresholdCase) {
	t.Helper()
	thresholds := testThresholds(false)
	control, questions := testControl(t, false, false)
	answers := testAnswers(questions, choiceLowValue, map[BinaryID]float64{})
	switch testCase.ID {
	case "false_suppress_equality":
		answers[BinaryGroundedInEvidence.String()].Binary.PTrue = 0.2
		if got := classifyBinary(BinaryGroundedInEvidence, DispositionSuppressLowValue, answers, thresholds); got != binaryUncertain {
			t.Fatalf("false suppress equality = %s, want uncertain", got)
		}
	case "true_suppress_equality":
		answers[BinaryGroundedInEvidence.String()].Binary.PTrue = 0.8
		if got := classifyBinary(BinaryGroundedInEvidence, DispositionSuppressLowValue, answers, thresholds); got != binaryUncertain {
			t.Fatalf("true suppress equality = %s, want uncertain", got)
		}
	case "protected_false_retain_equality":
		answers[BinaryPossibleOperationalRisk.String()].Binary.PTrue = 0.3
		if !hasProtectionBinarySignal(control, answers, thresholds) {
			t.Fatal("protected false-retain equality did not retain")
		}
	case "choice_confidence_equality":
		choice := answers[primaryUtilityQuestionID].Choice
		choice.Confidence = thresholds.ChoiceGates.Primary.Retain.Confidence
		if choicePassesGateWithGate(choice, thresholds.ChoiceGates.Primary.Retain, choice.Choice) {
			t.Fatal("choice confidence equality unexpectedly passed")
		}
	case "choice_probability_equality":
		choice := answers[primaryUtilityQuestionID].Choice
		choice.Confidence = 1
		choice.Probabilities[choice.Choice] = thresholds.ChoiceGates.Primary.Retain.SelectedProbability
		choice.Probabilities[choiceOther] = 1 - choice.Probabilities[choice.Choice]
		if choicePassesGateWithGate(choice, thresholds.ChoiceGates.Primary.Retain, choice.Choice) {
			t.Fatal("choice probability equality unexpectedly passed")
		}
	case "score_confidence_equality":
		control, questions = testControl(t, true, false)
		thresholds = testThresholds(true)
		answers = testAnswers(questions, choiceLowValue, map[BinaryID]float64{})
		score := answers[utilityScoreQuestionID]
		score.Score.Confidence = thresholds.ScoreGate.ConfidenceSuppress
		answers[utilityScoreQuestionID] = score
		if got := scoreGuard(control, answers, thresholds); got != ReasonScoreUncertain {
			t.Fatalf("score confidence equality = %q, want %s", got, ReasonScoreUncertain)
		}
	default:
		t.Fatalf("unknown strict threshold fixture case %q", testCase.ID)
	}
}

func executePolicyFixtureCase(t *testing.T, testCase policyCase) {
	t.Helper()
	switch testCase.ID {
	case "duplicate_self", "duplicate_forward", "duplicate_cycle":
		records := duplicateFixtureRecords(testCase.ID)
		out := FinalizeDuplicates(records)
		for _, record := range out {
			if testCase.ID == "duplicate_forward" && record.FindingID != "current" {
				continue
			}
			if string(record.ProposedDecision) != testCase.ExpectedProposed || string(record.EffectiveDecision) != testCase.ExpectedEffective {
				t.Fatalf("finalized records = %#v, want %s/%s", out, testCase.ExpectedProposed, testCase.ExpectedEffective)
			}
		}
		return
	case "label_leakage":
		var state State
		if err := DecodeStrict([]byte(`{"adjudicator_label":"keep"}`), &state); err == nil {
			t.Fatal("label leakage fixture unexpectedly decoded")
		}
		return
	case "prompt_injection_data":
		state := State{Finding: FindingState{ID: "F-prompt", Body: testCase.Body}}
		questions, err := Questions(DefaultRubric(), state)
		if err != nil {
			t.Fatal(err)
		}
		for _, question := range questions.Questions {
			if bytes.Contains([]byte(question.Instructions), []byte(testCase.Body)) {
				t.Fatal("prompt-injection source text entered evaluator instructions")
			}
		}
		return
	case "missing_score":
		control, questions := testControl(t, true, false)
		answers := testAnswers(questions, choiceLowValue, map[BinaryID]float64{})
		delete(answers, utilityScoreQuestionID)
		decision := decideWithTestOnlyThresholds(control, answers, testThresholds(true))
		assertFixtureDecision(t, decision, testCase)
		return
	case "invalid_nan":
		control, questions := testControl(t, false, false)
		answers := testAnswers(questions, choiceLowValue, map[BinaryID]float64{})
		value := answers[BinaryGroundedInEvidence.String()]
		value.Binary.PTrue = math.NaN()
		answers[BinaryGroundedInEvidence.String()] = value
		decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
		assertFixtureDecision(t, decision, testCase)
		return
	}
	includeScore := testCase.Score == "requested_missing"
	related := testCase.ID == "suppress_duplicate_candidate"
	control, questions := testControl(t, includeScore, related)
	if testCase.Eligibility == "unknown" {
		control.Eligibility.Status = EligibilityUnknown
	}
	answers := testAnswers(questions, testCase.Primary, map[BinaryID]float64{})
	if testCase.ID == "keep_required_outside_diff" {
		answers[BinaryRemediationRequiredForIntent.String()].Binary.PTrue = 0.9
	}
	if testCase.ID == "suppress_low_value_candidate" {
		answers = testAnswers(questions, choiceLowValue, map[BinaryID]float64{BinaryGroundedInEvidence: 0.1, BinaryIntroducedOrMateriallyAffected: 0.1, BinaryAdjacentImprovement: 0.1, BinarySpeculative: 0.99, BinaryActionable: 0.9})
	}
	if testCase.ID == "suppress_scope_expansion_candidate" {
		answers = testAnswers(questions, choiceScopeExpansion, map[BinaryID]float64{BinaryGroundedInEvidence: 0.9, BinaryIntroducedOrMateriallyAffected: 0.1, BinaryAdjacentImprovement: 0.9, BinarySpeculative: 0.1, BinaryActionable: 0.9})
	}
	if testCase.ID == "suppress_duplicate_candidate" {
		answers[BinaryGroundedInEvidence.String()].Binary.PTrue = 0.9
		answers[BinaryActionable.String()].Binary.PTrue = 0.9
		answers[BinarySpeculative.String()].Binary.PTrue = 0.1
		answers[BinaryAdjacentImprovement.String()].Binary.PTrue = 0.1
		answers[BinaryIntroducedOrMateriallyAffected.String()].Binary.PTrue = 0.1
		duplicate := answers[duplicateRepresentativeID]
		duplicate.Choice.Choice = "candidate_0"
		for option := range duplicate.Choice.Probabilities {
			duplicate.Choice.Probabilities[option] = 0
		}
		duplicate.Choice.Probabilities["candidate_0"] = 1
		answers[duplicateRepresentativeID] = duplicate
	}
	decision := decideWithTestOnlyThresholds(control, answers, testThresholds(includeScore))
	assertFixtureDecision(t, decision, testCase)
}

func assertFixtureDecision(t *testing.T, decision Decision, testCase policyCase) {
	t.Helper()
	if string(decision.ProposedDecision) != testCase.ExpectedProposed || string(decision.EffectiveDecision) != testCase.ExpectedEffective {
		t.Fatalf("decision=%#v, want %s/%s", decision, testCase.ExpectedProposed, testCase.ExpectedEffective)
	}
}

func duplicateFixtureRecords(id string) []EvaluationRecord {
	switch id {
	case "duplicate_self":
		return []EvaluationRecord{{RunID: "run", FindingID: "self", SourceOrdinal: 0, OriginalSeverity: "minor", ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "self"}}
	case "duplicate_forward":
		return []EvaluationRecord{{RunID: "run", FindingID: "current", SourceOrdinal: 0, OriginalSeverity: "minor", ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "later"}, {RunID: "run", FindingID: "later", SourceOrdinal: 1, OriginalSeverity: "minor", ProposedDecision: DispositionKeep, EffectiveDecision: DispositionKeep}}
	default:
		return []EvaluationRecord{{RunID: "run", FindingID: "a", SourceOrdinal: 0, OriginalSeverity: "minor", ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "b"}, {RunID: "run", FindingID: "b", SourceOrdinal: 1, OriginalSeverity: "minor", ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "a"}}
	}
}

func executeGoldenFixture(t *testing.T, golden goldenFixture) {
	t.Helper()
	for _, finding := range golden.Findings {
		control, questions := testControl(t, false, false)
		control.OriginalSeverity = finding.Severity
		answers := testAnswers(questions, choiceRequired, map[BinaryID]float64{})
		switch finding.FindingID {
		case "F-002":
			answers = testAnswers(questions, choiceScopeExpansion, map[BinaryID]float64{BinaryGroundedInEvidence: 0.9, BinaryIntroducedOrMateriallyAffected: 0.1, BinaryAdjacentImprovement: 0.9, BinarySpeculative: 0.1, BinaryActionable: 0.9})
		case "F-003":
			control.OriginalSeverity = "major"
			answers = testAnswers(questions, choiceLowValue, map[BinaryID]float64{BinaryGroundedInEvidence: 0.1, BinaryIntroducedOrMateriallyAffected: 0.1, BinaryAdjacentImprovement: 0.1, BinarySpeculative: 0.99, BinaryActionable: 0.9})
		}
		decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
		if decision.ProposedDecision != finding.ExpectedProposed || decision.EffectiveDecision != finding.ExpectedEffective {
			t.Fatalf("golden %s decision=%#v, want %s/%s", finding.FindingID, decision, finding.ExpectedProposed, finding.ExpectedEffective)
		}
	}
}

func TestFixtureEvaluationExecutesResponsesAndFailureCases(t *testing.T) {
	var fixture evaluationFixture
	if err := DecodeStrict(evaluationFixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || fixture.Backend != BackendFixture || fixture.RequestedModel == "" || len(fixture.AllowedResolvedModels) == 0 {
		t.Fatalf("evaluation fixture identity = %#v", fixture)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			if !ValidDigest(testCase.StateDigest) || !ValidDigest(testCase.QuestionsDigest) || testCase.RequestedModel != fixture.RequestedModel || testCase.ResolvedModel != fixture.RequestedModel {
				t.Fatalf("fixture case identity = %#v", testCase)
			}
			if testCase.ResponseBase64 != nil {
				data, err := base64.StdEncoding.DecodeString(*testCase.ResponseBase64)
				if err != nil {
					t.Fatal(err)
				}
				var response EvaluationResponse
				err = DecodeStrict(data, &response)
				if testCase.ID == "malformed-response" {
					if err == nil {
						t.Fatal("malformed response fixture unexpectedly decoded")
					}
				} else if err != nil {
					t.Fatalf("fixture response rejected: %v", err)
				}
			}
			if testCase.ID == "missing-case" && testCase.WaitError == nil {
				t.Fatal("missing fixture case lacks wait error")
			}
			if testCase.ID == "deadline-case" && !testCase.Deadline {
				t.Fatal("deadline fixture case is not marked deadline")
			}
		})
	}
}
