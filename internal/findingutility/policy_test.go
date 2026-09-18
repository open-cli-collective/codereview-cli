package findingutility

import (
	"math"
	"testing"
)

func TestDecideFailsClosedForUncalibratedRuntimeThresholds(t *testing.T) {
	control, questions := testControl(t, false, false)
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.9,
		NoulActionable:                     0.1,
	})
	thresholds := testThresholds(false)
	decision := Decide(control, answers, thresholds)
	if decision.ProposedDecision != DispositionKeep || decision.EffectiveDecision != DispositionKeep || !hasReason(decision, ReasonUncalibratedPolicy) {
		t.Fatalf("runtime test-only threshold decision = %#v", decision)
	}
}

func TestExportedDecideCannotAuthorizeSuppression(t *testing.T) {
	control, questions := testControl(t, false, false)
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.99,
		NoulActionable:                     0.9,
	})
	thresholds := testThresholds(false)
	thresholds.TestOnly = false
	thresholds.CalibrationManifestDigest = DigestBytes([]byte("calibration"))
	thresholds.RubricDigest = DigestBytes([]byte("rubric"))
	thresholds.PolicyDigest = DigestBytes([]byte("policy"))
	thresholds.QuestionsDigest = DigestBytes([]byte("questions"))
	thresholds.ModelConditionDigest = DigestBytes([]byte("model"))
	thresholds.BoundProfileDigest = DigestBytes([]byte("profile"))
	thresholds.EligibilityRuleDigest = DigestBytes([]byte("eligibility"))
	thresholds.ArtifactDigest = DigestBytes([]byte("artifact"))
	thresholds.CalibrationVersion = "calibration-v1"
	thresholds.ApprovalIDs = []string{"approval-1"}
	decision := Decide(control, answers, thresholds)
	if decision.ProposedDecision != DispositionKeep || decision.EffectiveDecision != DispositionKeep || !hasReason(decision, ReasonUncalibratedPolicy) {
		t.Fatalf("exported Decide authorized a suppression: %#v", decision)
	}
}

func TestDecideExercisesThreeSuppressionSignaturesButKeepsEffectively(t *testing.T) {
	cases := []struct {
		name   string
		choice string
		values map[NoulID]float64
		want   Disposition
	}{
		{name: "low value", choice: choiceLowValue, values: map[NoulID]float64{NoulGroundedInEvidence: 0.1, NoulIntroducedOrMateriallyAffected: 0.1, NoulAdjacentImprovement: 0.1, NoulSpeculative: 0.99, NoulActionable: 0.9}, want: DispositionSuppressLowValue},
		{name: "scope expansion", choice: choiceScopeExpansion, values: map[NoulID]float64{NoulGroundedInEvidence: 0.9, NoulIntroducedOrMateriallyAffected: 0.1, NoulAdjacentImprovement: 0.9, NoulSpeculative: 0.1, NoulActionable: 0.9}, want: DispositionSuppressScopeExpansion},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			control, questions := testControl(t, false, false)
			answers := testAnswers(questions, test.choice, test.values)
			decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
			if decision.ProposedDecision != test.want || decision.EffectiveDecision != DispositionKeep {
				t.Fatalf("decision = %#v, want proposal %s/effective keep", decision, test.want)
			}
		})
	}
}

func TestDecideDuplicateRequiresValidCandidateAndProtectedNoulsRetain(t *testing.T) {
	control, questions := testControl(t, false, true)
	answers := testAnswers(questions, choiceDuplicate, map[NoulID]float64{
		NoulGroundedInEvidence:             0.9,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.9,
		NoulSpeculative:                    0.1,
		NoulActionable:                     0.9,
	})
	duplicate := answers[duplicateRepresentativeID]
	duplicate.Choice.Choice = "candidate_0"
	for option := range duplicate.Choice.Probabilities {
		duplicate.Choice.Probabilities[option] = 0
	}
	duplicate.Choice.Probabilities["candidate_0"] = 1
	answers[duplicateRepresentativeID] = duplicate
	decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
	if decision.ProposedDecision != DispositionSuppressDuplicate || decision.EffectiveDecision != DispositionKeep {
		t.Fatalf("valid duplicate decision = %#v", decision)
	}

	for _, id := range []NoulID{NoulPossibleSecurityRisk, NoulPossibleCorrectnessRisk, NoulPossibleAuthorizationRisk, NoulPossiblePrivacyRisk, NoulPossibleDataLossRisk, NoulPossibleOperationalRisk} {
		protected := cloneAnswers(answers)
		value := protected[id.String()]
		value.Noul.PTrue = 0.3 // false_retain equality is retaining.
		protected[id.String()] = value
		decision = decideWithTestOnlyThresholds(control, protected, testThresholds(false))
		if decision.ProposedDecision != DispositionKeep || !hasReason(decision, ReasonModelProtectionSignal) {
			t.Fatalf("protected Noul %s decision = %#v", id, decision)
		}
	}
}

func TestDecideKeepsIndependentProtectionButExposesUtilityCandidate(t *testing.T) {
	control, questions := testControl(t, false, false)
	control.Protection.Status = ProtectionProtected
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.99,
		NoulActionable:                     0.9,
	})
	decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
	if decision.ProposedDecision != DispositionKeep || decision.EffectiveDecision != DispositionKeep || !hasReason(decision, ReasonProtectedDomain) || decision.ModelCandidateDecision == nil || *decision.ModelCandidateDecision != DispositionSuppressLowValue {
		t.Fatalf("protected candidate separation = %#v", decision)
	}
}

func TestDecideGuardsSeverityEligibilityContextAndStability(t *testing.T) {
	baseAnswers := func(t *testing.T) AnswerSet {
		_, questions := testControl(t, false, false)
		return testAnswers(questions, choiceLowValue, map[NoulID]float64{NoulGroundedInEvidence: 0.1, NoulIntroducedOrMateriallyAffected: 0.1, NoulAdjacentImprovement: 0.1, NoulSpeculative: 0.99, NoulActionable: 0.9})
	}
	checks := []struct {
		name   string
		mutate func(*Control)
		reason string
	}{
		{name: "major severity", mutate: func(control *Control) { control.OriginalSeverity = "major" }, reason: ReasonSeverityRetained},
		{name: "unknown eligibility", mutate: func(control *Control) { control.Eligibility.Status = EligibilityUnknown }, reason: ReasonEligibilityUnknown},
		{name: "ineligible", mutate: func(control *Control) { control.Eligibility.Status = EligibilityIneligible }, reason: ReasonIneligible},
		{name: "missing context", mutate: func(control *Control) { control.ContextComplete = false }, reason: ReasonIncompleteContext},
		{name: "unstable", mutate: func(control *Control) { control.StabilityReceipt = &StabilityReceipt{Successful: true, Unstable: true} }, reason: ReasonUnstableEvaluation},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			control, _ := testControl(t, false, false)
			control.StabilityReceipt = &StabilityReceipt{Successful: true}
			thresholds := testThresholds(false)
			if check.name == "unstable" {
				thresholds.StabilityParameters.Required = true
			}
			check.mutate(&control)
			decision := decideWithTestOnlyThresholds(control, baseAnswers(t), thresholds)
			want := DispositionKeep
			if check.reason == ReasonUnstableEvaluation {
				want = DispositionAbstain
			}
			if decision.ProposedDecision != want || !hasReason(decision, check.reason) {
				t.Fatalf("decision = %#v, want %s", decision, check.reason)
			}
		})
	}
}

func TestCandidateGuardsApplyCompletenessAndNecessityBeforeProposal(t *testing.T) {
	control, questions := testControl(t, false, false)
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.99,
		NoulActionable:                     0.9,
	})
	control.ContextComplete = false
	control.Limitations = []Limitation{{ID: "context", Code: LimitationOther, Impact: ImpactDecisionRelevant}}
	decision := decideWithTestOnlyThresholds(control, answers, testThresholds(false))
	if decision.ProposedDecision != DispositionKeep || decision.CandidateStatus != CandidateUnavailable || decision.ModelCandidateDecision != nil || !hasReason(decision, ReasonIncompleteContext) {
		t.Fatalf("incomplete candidate was not unavailable/retained: %#v", decision)
	}

	control, questions = testControl(t, false, false)
	answers = testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.99,
		NoulActionable:                     0.9,
		NoulRemediationRequiredForIntent:   0.9,
	})
	decision = decideWithTestOnlyThresholds(control, answers, testThresholds(false))
	if decision.ProposedDecision != DispositionKeep || decision.EffectiveDecision != DispositionKeep || !hasReason(decision, ReasonNecessitySignal) || decision.ModelCandidateDecision == nil || *decision.ModelCandidateDecision != DispositionKeep {
		t.Fatalf("necessity guard did not retain candidate/proposal: %#v", decision)
	}
}

func TestRemediationRetentionBandVetoesCandidateAndProposal(t *testing.T) {
	control, questions := testControl(t, false, false)
	thresholds := testThresholds(false)
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{
		NoulGroundedInEvidence:             0.1,
		NoulIntroducedOrMateriallyAffected: 0.1,
		NoulAdjacentImprovement:            0.1,
		NoulSpeculative:                    0.99,
		NoulActionable:                     0.9,
		NoulRemediationRequiredForIntent:   0.75,
	})

	candidate := decideUtility(control, answers, thresholds, true)
	if candidate.CandidateStatus != CandidateAvailable || candidate.ProposedDecision != DispositionKeep || !hasReason(candidate, ReasonNecessityRetention) {
		t.Fatalf("retention-band candidate = %#v", candidate)
	}
	proposal := decideWithTestOnlyThresholds(control, answers, thresholds)
	if proposal.ProposedDecision != DispositionKeep || proposal.EffectiveDecision != DispositionKeep || proposal.ModelCandidateDecision == nil || *proposal.ModelCandidateDecision != DispositionKeep || !hasReason(proposal, ReasonNecessityRetention) {
		t.Fatalf("retention-band proposal = %#v", proposal)
	}
}

func TestDecideScoreUsesStrictGatesAndPositiveUtilityVeto(t *testing.T) {
	control, questions := testControl(t, true, false)
	thresholds := testThresholds(true)
	answers := testAnswers(questions, choiceLowValue, map[NoulID]float64{NoulGroundedInEvidence: 0.1, NoulIntroducedOrMateriallyAffected: 0.1, NoulAdjacentImprovement: 0.1, NoulSpeculative: 0.99, NoulActionable: 0.9})
	decision := decideWithTestOnlyThresholds(control, answers, thresholds)
	if decision.ProposedDecision != DispositionSuppressLowValue {
		t.Fatalf("low score should permit candidate, got %#v", decision)
	}
	positive := cloneAnswers(answers)
	score := positive[utilityScoreQuestionID]
	score.Score.Score = 2
	score.Score.Probabilities = []float64{0, 0, 1, 0}
	score.Score.Confidence = 0.9
	positive[utilityScoreQuestionID] = score
	if err := ValidateAnswers(questions, EvaluationResponse{Answers: positive}, NumericTolerance{}); err != nil {
		t.Fatalf("positive score response invalid before policy: %v; score=%#v", err, score.Score)
	}
	decision = decideWithTestOnlyThresholds(control, positive, thresholds)
	if decision.ProposedDecision != DispositionKeep || !hasReason(decision, ReasonUtilitySignal) {
		t.Fatalf("positive score must veto suppression: %#v", decision)
	}
	uncertain := cloneAnswers(answers)
	score = uncertain[utilityScoreQuestionID]
	score.Score.Confidence = thresholds.ScoreGate.ConfidenceSuppress
	uncertain[utilityScoreQuestionID] = score
	decision = decideWithTestOnlyThresholds(control, uncertain, thresholds)
	if decision.ProposedDecision != DispositionAbstain || !hasReason(decision, ReasonScoreUncertain) {
		t.Fatalf("score gate equality must abstain: %#v", decision)
	}
	invalid := cloneAnswers(answers)
	score = invalid[utilityScoreQuestionID]
	score.Score.Probabilities[0] = math.NaN()
	invalid[utilityScoreQuestionID] = score
	decision = decideWithTestOnlyThresholds(control, invalid, thresholds)
	if decision.ProposedDecision != DispositionKeep || !hasReason(decision, ReasonInvalidResponse) {
		t.Fatalf("invalid score must retain: %#v", decision)
	}
}

func TestFinalizeDuplicatesPreservesOrderAndInvalidatesChains(t *testing.T) {
	records := []EvaluationRecord{
		{FindingID: "F-003", SourceOrdinal: 2, ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "F-002"},
		{FindingID: "F-001", SourceOrdinal: 0, ProposedDecision: DispositionKeep, EffectiveDecision: DispositionKeep},
		{FindingID: "F-002", SourceOrdinal: 1, ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "F-001"},
	}
	out := FinalizeDuplicates(records)
	if out[0].FindingID != "F-003" || out[1].FindingID != "F-001" || out[2].FindingID != "F-002" {
		t.Fatalf("finalizer changed raw order: %#v", out)
	}
	if out[2].ProposedDecision != DispositionSuppressDuplicate || out[2].EffectiveDecision != DispositionKeep {
		t.Fatalf("valid duplicate was not retained as proposal: %#v", out[2])
	}
	if out[0].ProposedDecision != DispositionAbstain || !hasRecordReason(out[0], ReasonDuplicateRepresentativeNotKept) {
		t.Fatalf("dependent duplicate was not invalidated: %#v", out[0])
	}

	cycle := []EvaluationRecord{
		{FindingID: "A", SourceOrdinal: 0, ProposedDecision: DispositionSuppressDuplicate, DuplicateRepresentativeID: "B"},
		{FindingID: "B", SourceOrdinal: 1, ProposedDecision: DispositionSuppressDuplicate, DuplicateRepresentativeID: "A"},
	}
	for _, record := range FinalizeDuplicates(cycle) {
		if record.ProposedDecision != DispositionAbstain || record.EffectiveDecision != DispositionKeep {
			t.Fatalf("cycle record not fail-closed: %#v", record)
		}
	}
}

func TestFinalizeDuplicatesRanksSeverityBeforeOrdinal(t *testing.T) {
	records := []EvaluationRecord{
		{RunID: "run", FindingID: "minor", SourceOrdinal: 0, OriginalSeverity: "minor", ProposedDecision: DispositionSuppressDuplicate, EffectiveDecision: DispositionKeep, DuplicateRepresentativeID: "major"},
		{RunID: "run", FindingID: "major", SourceOrdinal: 1, OriginalSeverity: "major", ProposedDecision: DispositionKeep, EffectiveDecision: DispositionKeep},
	}
	out := FinalizeDuplicates(records)
	if out[0].ProposedDecision != DispositionSuppressDuplicate || hasRecordReason(out[0], ReasonDuplicateInvalidRepresentative) {
		t.Fatalf("severity-first representative was rejected: %#v", out)
	}
}

func hasRecordReason(record EvaluationRecord, reason string) bool {
	for _, value := range record.ReasonCodes {
		if value == reason {
			return true
		}
	}
	return false
}
