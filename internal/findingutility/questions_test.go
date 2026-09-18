package findingutility

import (
	"encoding/json"
	"math"
	"testing"
)

func TestRubricAndQuestionsFreezeThirteenNoulsAndDynamicCandidates(t *testing.T) {
	rubric, err := LoadRubric()
	if err != nil {
		t.Fatal(err)
	}
	if len(rubric.Questions) != 16 {
		t.Fatalf("rubric question count = %d, want 16", len(rubric.Questions))
	}
	questions, err := Questions(rubric, State{RelatedFindings: RelatedFindingsState{Items: []RelatedFinding{{ID: "F-older"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(questions.Questions) != 15 {
		t.Fatalf("disabled-score question count = %d, want 15", len(questions.Questions))
	}
	seen := map[NoulID]bool{}
	for _, question := range questions.Questions {
		if question.Type == QuestionTypeNoul {
			seen[NoulID(question.ID)] = true
			if len(question.Criteria) != 2 || question.Criteria["true"] == "" || question.Criteria["false"] == "" {
				t.Fatalf("Noul %s lacks exact true/false criteria", question.ID)
			}
		}
		if question.ID == primaryUtilityQuestionID && question.Instructions[:len(sharedInstructionPrefix)] != sharedInstructionPrefix {
			t.Fatal("shared instruction prefix was not prepended")
		}
		if question.ID == duplicateRepresentativeID {
			if len(question.Options) != 3 || question.Options[2].Key != "candidate_0" || questions.DuplicateOptionMap["candidate_0"] != "F-older" {
				t.Fatalf("dynamic duplicate options = %#v map=%#v", question.Options, questions.DuplicateOptionMap)
			}
		}
	}
	if len(seen) != len(allNoulIDs) {
		t.Fatalf("Noul IDs = %#v, want %d", seen, len(allNoulIDs))
	}
	if !ValidDigest(questions.Digest) {
		t.Fatalf("question digest %q is invalid", questions.Digest)
	}

	rubric.IncludeUtilityScore = true
	withScore, err := Questions(rubric, State{})
	if err != nil {
		t.Fatal(err)
	}
	if !withScore.IncludeUtilityScore || len(withScore.Questions) != 16 {
		t.Fatalf("score-enabled questions = %#v", withScore)
	}
}

func TestValidateAnswersRejectsRepairAndAcceptsExactFixtureResponse(t *testing.T) {
	questions := testQuestions(t, false, false)
	answers := testAnswers(questions, choiceRequired, map[NoulID]float64{})
	response := EvaluationResponse{SchemaVersion: 1, RequestedModel: "fixture:utility-v1", ResolvedModel: "fixture:utility-v1", Answers: answers}
	if err := ValidateAnswers(questions, response, NumericTolerance{ProbabilitySum: 0, ScoreMean: 0}); err != nil {
		t.Fatalf("exact fixture response rejected: %v", err)
	}

	bad := cloneAnswers(answers)
	delete(bad, NoulGroundedInEvidence.String())
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("missing required Noul must fail")
	}
	bad = cloneAnswers(answers)
	choice := bad[primaryUtilityQuestionID]
	choice.Choice.Probabilities[choiceRequired] = math.NaN()
	bad[primaryUtilityQuestionID] = choice
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("NaN probability must fail")
	}
	bad = cloneAnswers(answers)
	choice = bad[primaryUtilityQuestionID]
	choice.Choice.Probabilities[choiceRequired] = 0.5
	bad[primaryUtilityQuestionID] = choice
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("unrepaired probability sum must fail")
	}
	bad = cloneAnswers(answers)
	bad["unknown"] = Answer{Type: QuestionTypeNoul, Noul: &NoulAnswer{PTrue: 0}}
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("unknown answer ID must fail")
	}
}

func TestNoulJSONRejectsMissingAndNullProbability(t *testing.T) {
	for _, input := range []string{`{}`, `{"p_true":null}`} {
		var answer NoulAnswer
		if err := json.Unmarshal([]byte(input), &answer); err == nil {
			t.Fatalf("Noul JSON %s unexpectedly accepted", input)
		}
	}
	var answer NoulAnswer
	if err := json.Unmarshal([]byte(`{"p_true":0}`), &answer); err != nil {
		t.Fatalf("zero p_true rejected: %v", err)
	}
}

func TestOperationalRiskWordingMatchesFrozenRubric(t *testing.T) {
	rubric := DefaultRubric()
	for _, question := range rubric.Questions {
		if question.ID != string(NoulPossibleOperationalRisk) {
			continue
		}
		if question.Criteria["true"] != "The claim plausibly concerns availability, latency, resource exhaustion, cost escalation, deployment, rollout, rollback, retries, observability needed for operations, or recovery behavior." || question.Criteria["false"] != "Adequate context establishes a matter without plausible operational consequence. A claim is not non-operational merely because the current load is small." {
			t.Fatalf("operational-risk criteria drifted: %#v", question.Criteria)
		}
		return
	}
	t.Fatal("operational-risk Noul missing")
}

func TestValidateAnswersChecksScoreLegendAndWeightedMean(t *testing.T) {
	questions := testQuestions(t, true, false)
	answers := testAnswers(questions, choiceRequired, map[NoulID]float64{})
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: answers}, NumericTolerance{}); err != nil {
		t.Fatalf("valid score rejected: %v", err)
	}
	bad := cloneAnswers(answers)
	score := bad[utilityScoreQuestionID]
	score.Score.Legend[1] = "leaked-label"
	bad[utilityScoreQuestionID] = score
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("score legend mismatch must fail")
	}
	bad = cloneAnswers(answers)
	score = bad[utilityScoreQuestionID]
	score.Score.Score = 3
	bad[utilityScoreQuestionID] = score
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: bad}, NumericTolerance{}); err == nil {
		t.Fatal("score weighted-mean mismatch must fail")
	}
}

func TestFixtureProfileIsStrictAndSyntheticOnly(t *testing.T) {
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProfileID != FixtureProfileID || profile.VendorPermission.Permitted || profile.RequestedModel[:len("fixture:")] != "fixture:" {
		t.Fatalf("fixture profile leaked non-synthetic permission/model: %#v", profile)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if string(object["numeric_tolerance"]) != "0" {
		t.Fatalf("fixture profile numeric_tolerance = %s, want scalar 0", object["numeric_tolerance"])
	}
}
