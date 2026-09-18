package findingutility

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ReasonInvalidState and the other reason codes identify policy outcomes.
const (
	ReasonInvalidState                   = "invalid_state"
	ReasonStaleInput                     = "stale_input"
	ReasonResumeMismatch                 = "resume_mismatch"
	ReasonInputNotPermitted              = "input_not_permitted"
	ReasonEvaluatorFailure               = "evaluator_failure"
	ReasonInvalidResponse                = "invalid_response"
	ReasonModelMismatch                  = "model_mismatch"
	ReasonAuditFailure                   = "audit_failure"
	ReasonSeverityRetained               = "severity_retained"
	ReasonProtectedDomain                = "protected_domain"
	ReasonProtectionUnknown              = "protection_unknown"
	ReasonUncalibratedPolicy             = "uncalibrated_policy"
	ReasonModelProtectionSignal          = "model_protection_signal"
	ReasonEligibilityUnknown             = "eligibility_unknown"
	ReasonIneligible                     = "ineligible"
	ReasonIncompleteContext              = "incomplete_context"
	ReasonContextUncertain               = "context_uncertain"
	ReasonRequired                       = "required"
	ReasonUseful                         = "useful"
	ReasonUnclassified                   = "unclassified"
	ReasonLowConfidence                  = "low_confidence"
	ReasonUncertainAnswer                = "uncertain_answer"
	ReasonAnswerConflict                 = "answer_conflict"
	ReasonScoreUncertain                 = "score_uncertain"
	ReasonDuplicateUnresolved            = "duplicate_unresolved"
	ReasonDuplicateInvalidRepresentative = "duplicate_invalid_representative"
	ReasonDuplicateRepresentativeNotKept = "duplicate_representative_not_kept"
	ReasonUnstableEvaluation             = "unstable_evaluation"
	ReasonStabilityUnverified            = "stability_unverified"
	ReasonNecessitySignal                = "necessity_signal"
	ReasonNecessityRetention             = "necessity_retention_band"
	ReasonUtilitySignal                  = "utility_signal"
)

// ValidateAnswers validates the complete typed evaluator response. It never
// repairs distributions, fills missing options, or coerces a response type.
func ValidateAnswers(questions QuestionSet, response EvaluationResponse, tolerance NumericTolerance) error {
	if tolerance.ProbabilitySum < 0 || tolerance.ScoreMean < 0 || !IsFinite(tolerance.ProbabilitySum) || !IsFinite(tolerance.ScoreMean) {
		return fmt.Errorf("findingutility: numeric tolerance must be finite and non-negative")
	}
	if response.Answers == nil {
		return fmt.Errorf("findingutility: answers are required")
	}
	known := make(map[string]Question, len(questions.Questions))
	for _, question := range questions.Questions {
		known[question.ID] = question
	}
	for id := range response.Answers {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("findingutility: unknown answer %q", id)
		}
	}
	for _, question := range questions.Questions {
		answer, ok := response.Answers[question.ID]
		if !ok {
			return fmt.Errorf("findingutility: missing answer %q", question.ID)
		}
		if answer.Status != AnswerStatusPresent {
			return fmt.Errorf("findingutility: answer %q is not present", question.ID)
		}
		if answer.Type != question.Type {
			return fmt.Errorf("findingutility: answer %q type %q does not match %q", question.ID, answer.Type, question.Type)
		}
		switch question.Type {
		case QuestionTypeNoul:
			if answer.Noul == nil || (response.decoded && !answer.Noul.present) || answer.Choice != nil || answer.Score != nil || !finiteProbability(answer.Noul.PTrue) {
				return fmt.Errorf("findingutility: Noul %q has invalid p_true", question.ID)
			}
		case QuestionTypeChoice:
			if answer.Choice == nil || answer.Noul != nil || answer.Score != nil {
				return fmt.Errorf("findingutility: Choice %q is missing", question.ID)
			}
			if err := validateChoice(question, *answer.Choice, tolerance.ProbabilitySum); err != nil {
				return fmt.Errorf("findingutility: Choice %q: %w", question.ID, err)
			}
		case QuestionTypeScore:
			if answer.Score == nil || answer.Noul != nil || answer.Choice != nil {
				return fmt.Errorf("findingutility: Score %q is missing", question.ID)
			}
			if err := validateScore(question, *answer.Score, tolerance); err != nil {
				return fmt.Errorf("findingutility: Score %q: %w", question.ID, err)
			}
		default:
			return fmt.Errorf("findingutility: unsupported answer type %q", question.Type)
		}
	}
	return nil
}

func validateChoice(question Question, answer ChoiceAnswer, tolerance float64) error {
	if strings.TrimSpace(answer.Choice) == "" || !finiteProbability(answer.Confidence) {
		return fmt.Errorf("choice and confidence are required")
	}
	allowed := make(map[string]bool, len(question.Options))
	for _, option := range question.Options {
		allowed[option.Key] = true
	}
	if !allowed[answer.Choice] {
		return fmt.Errorf("unknown choice %q", answer.Choice)
	}
	if len(answer.Probabilities) != len(allowed) {
		return fmt.Errorf("probabilities must contain every option exactly once")
	}
	sum := 0.0
	for option := range allowed {
		probability, ok := answer.Probabilities[option]
		if !ok || !finiteProbability(probability) {
			return fmt.Errorf("invalid probability for option %q", option)
		}
		sum += probability
	}
	for option := range answer.Probabilities {
		if !allowed[option] {
			return fmt.Errorf("unknown probability option %q", option)
		}
	}
	if math.Abs(sum-1) > tolerance {
		return fmt.Errorf("probabilities sum to %.12g, want 1 within %.12g", sum, tolerance)
	}
	return nil
}

func validateScore(question Question, answer ScoreAnswer, tolerance NumericTolerance) error {
	if !IsFinite(answer.Score) || answer.Score < 0 || answer.Score > float64(len(question.Levels)-1) || !finiteProbability(answer.Confidence) {
		return fmt.Errorf("invalid score or confidence")
	}
	if len(answer.Legend) != len(question.Levels) || len(answer.Probabilities) != len(question.Levels) {
		return fmt.Errorf("legend and probabilities must match the configured levels")
	}
	sum := 0.0
	weighted := 0.0
	for index, level := range question.Levels {
		if answer.Legend[index] != level.Label {
			return fmt.Errorf("legend[%d] = %q, want %q", index, answer.Legend[index], level.Label)
		}
		probability := answer.Probabilities[index]
		if !finiteProbability(probability) {
			return fmt.Errorf("invalid probability at level %d", index)
		}
		sum += probability
		weighted += float64(level.Position) * probability
	}
	if math.Abs(sum-1) > tolerance.ProbabilitySum {
		return fmt.Errorf("probabilities sum to %.12g, want 1 within %.12g", sum, tolerance.ProbabilitySum)
	}
	if math.Abs(weighted-answer.Score) > tolerance.ScoreMean {
		return fmt.Errorf("score %.12g does not match weighted mean %.12g", answer.Score, weighted)
	}
	return nil
}

// Decide applies the safe production policy. A nil, invalid, or test-only
// threshold artifact cannot enable suppression.
func Decide(control Control, answers AnswerSet, thresholds ThresholdSet) Decision {
	return decide(control, answers, &thresholds, false)
}

// decideWithTestOnlyThresholds is intentionally package-private. Tests may
// exercise terminal policy branches with signed-shaped synthetic thresholds,
// but no production caller can use a test-only artifact to authorize a
// suppression through Decide.
func decideWithTestOnlyThresholds(control Control, answers AnswerSet, thresholds ThresholdSet) Decision {
	return decide(control, answers, &thresholds, true)
}

func decide(control Control, answers AnswerSet, thresholds *ThresholdSet, allowTestOnly bool) Decision {
	decision := keepDecision()
	if !allowTestOnly {
		return withReason(decision, ReasonUncalibratedPolicy)
	}
	if !control.StateValid {
		return withReason(decision, ReasonInvalidState)
	}
	if !control.Fresh {
		return withReason(decision, ReasonStaleInput)
	}
	if control.Mode != ModeAdvisory {
		return withReason(decision, ReasonInputNotPermitted)
	}
	if !control.VendorPermission.Permitted {
		return withReason(decision, ReasonInputNotPermitted)
	}
	if control.EvaluatorStatus != "" && control.EvaluatorStatus != EvaluatorSucceeded {
		return withReason(decision, evaluatorReason(control.EvaluatorStatus))
	}
	if thresholds == nil || !validThresholdSet(*thresholds) {
		return withReason(decision, ReasonUncalibratedPolicy)
	}
	if !thresholds.TestOnly {
		return withReason(decision, ReasonUncalibratedPolicy)
	}
	if control.IncludeUtilityScore != thresholds.IncludeUtilityScore {
		return withReason(decision, ReasonUncalibratedPolicy)
	}
	if control.QuestionSet == nil {
		return withReason(decision, ReasonInvalidResponse)
	}
	if err := ValidateAnswers(*control.QuestionSet, EvaluationResponse{Answers: answers}, NumericTolerance{}); err != nil {
		return withReason(decision, ReasonInvalidResponse)
	}

	candidate := decideUtility(control, answers, *thresholds, true)
	if candidate.CandidateStatus == CandidateAvailable {
		candidateDecision := candidate.ProposedDecision
		decision.ModelCandidateDecision = &candidateDecision
		decision.CandidateStatus = CandidateAvailable
	}
	decision.ReasonTrace = append(decision.ReasonTrace, candidate.ReasonTrace...)
	for _, reason := range candidate.ReasonCodes {
		if !hasReason(decision, reason) {
			decision.ReasonCodes = append(decision.ReasonCodes, reason)
		}
	}
	modelProtectionSignal := hasProtectionNoulSignal(control, answers, *thresholds)

	// Independent guards are applied after the counterfactual utility layer so
	// protected/major/ineligible findings retain a visible model candidate while
	// never allowing that candidate to control the guarded proposal.
	if control.OriginalSeverity == "" || control.OriginalSeverity == "blocking" || control.OriginalSeverity == "major" || control.OriginalSeverity == "unknown" {
		decision = withReason(decision, ReasonSeverityRetained)
		if modelProtectionSignal {
			decision = withReason(decision, ReasonModelProtectionSignal)
		}
		return decision
	}
	if control.Protection.Status == ProtectionProtected {
		decision = withReason(decision, ReasonProtectedDomain)
		if modelProtectionSignal {
			decision = withReason(decision, ReasonModelProtectionSignal)
		}
		return decision
	}
	if control.Protection.Status == "" || control.Protection.Status == ProtectionUnknown {
		decision = withReason(decision, ReasonProtectionUnknown)
		if modelProtectionSignal {
			decision = withReason(decision, ReasonModelProtectionSignal)
		}
		return decision
	}
	if modelProtectionSignal {
		return withReason(decision, ReasonModelProtectionSignal)
	}
	if control.Eligibility.Status == "" || control.Eligibility.Status == EligibilityUnknown || (control.Eligibility.Status == EligibilityEligible && control.Eligibility.AuthorityKind != AuthorityHumanAttestation && control.Eligibility.AuthorityKind != AuthorityApprovedDeterministic) {
		return withReason(decision, ReasonEligibilityUnknown)
	}
	if control.Eligibility.Status == EligibilityIneligible {
		return withReason(decision, ReasonIneligible)
	}
	if !control.SourceComplete || !control.ContextComplete || !control.CandidateSetComplete {
		return withReason(decision, ReasonIncompleteContext)
	}
	if hasDecisionRelevantLimitation(control.Limitations) {
		return withReason(decision, ReasonIncompleteContext)
	}

	action := suppressionActionForPrimary(answers)
	if answers[NoulMissingDecisionContext.String()].Noul != nil {
		classification := classifyNoul(NoulMissingDecisionContext, action, answers, *thresholds)
		if classification != noulFalse {
			if classification == noulInvalid {
				return withReason(decision, ReasonInvalidResponse)
			}
			return withAbstainReason(decision, ReasonContextUncertain)
		}
	}
	if answers[NoulRemediationRequiredForIntent.String()].Noul != nil {
		if remediationRetentionBand(answers, *thresholds, action) {
			return withReason(decision, ReasonNecessityRetention)
		}
		classification := classifyNoul(NoulRemediationRequiredForIntent, action, answers, *thresholds)
		if classification == noulInvalid {
			return withReason(decision, ReasonInvalidResponse)
		}
		if classification != noulFalse {
			if classification == noulTrue {
				return withReason(decision, ReasonNecessitySignal)
			}
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
	}

	primary, ok := answers[primaryUtilityQuestionID]
	if !ok || primary.Choice == nil {
		return withReason(decision, ReasonInvalidResponse)
	}
	if !choicePassesGate(primary.Choice, questionSetOrEmpty(control.QuestionSet), primaryUtilityQuestionID, *thresholds, primary.Choice.Choice) {
		return withAbstainReason(decision, ReasonLowConfidence)
	}
	switch primary.Choice.Choice {
	case choiceRequired:
		return withReason(decision, ReasonRequired)
	case choiceUsefulNonblocking:
		return withReason(decision, ReasonUseful)
	case choiceInsufficientContext:
		return withAbstainReason(decision, ReasonContextUncertain)
	case choiceOther:
		return withAbstainReason(decision, ReasonUnclassified)
	case choiceLowValue:
		if reason := nonDuplicateRepresentativeGuard(control, answers, *thresholds); reason != "" {
			return withAbstainReason(decision, reason)
		}
		if !lowValueSignature(answers, *thresholds) {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
		if err := scoreGuard(control, answers, *thresholds); err != "" {
			if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
				return withReason(decision, err)
			}
			return withAbstainReason(decision, err)
		}
		decision.ProposedDecision = DispositionSuppressLowValue
	case choiceScopeExpansion:
		if reason := nonDuplicateRepresentativeGuard(control, answers, *thresholds); reason != "" {
			return withAbstainReason(decision, reason)
		}
		if !scopeExpansionSignature(answers, *thresholds) {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
		if err := scoreGuard(control, answers, *thresholds); err != "" {
			if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
				return withReason(decision, err)
			}
			return withAbstainReason(decision, err)
		}
		decision.ProposedDecision = DispositionSuppressScopeExpansion
	case choiceDuplicate:
		if !duplicateSignature(answers, *thresholds) {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
		if err := scoreGuard(control, answers, *thresholds); err != "" {
			if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
				return withReason(decision, err)
			}
			return withAbstainReason(decision, err)
		}
		if err := duplicateRepresentative(control, answers, *thresholds); err != "" {
			return withAbstainReason(decision, err)
		}
		decision.ProposedDecision = DispositionSuppressDuplicate
	default:
		return withReason(decision, ReasonInvalidResponse)
	}
	if thresholds.StabilityParameters.Required {
		if control.StabilityReceipt == nil {
			return withAbstainReason(decision, ReasonStabilityUnverified)
		}
		if control.StabilityReceipt.Unstable || !control.StabilityReceipt.Successful {
			return withAbstainReason(decision, ReasonUnstableEvaluation)
		}
		if control.StabilityReceipt.ProtectionSignal {
			return withReason(decision, ReasonModelProtectionSignal)
		}
	}
	decision.EffectiveDecision = DispositionKeep
	return decision
}

func decideUtility(control Control, answers AnswerSet, thresholds ThresholdSet, candidateOnly bool) Decision {
	decision := keepDecision()
	if !control.SourceComplete || !control.ContextComplete || !control.CandidateSetComplete || hasDecisionRelevantLimitation(control.Limitations) {
		return withReason(decision, ReasonIncompleteContext)
	}
	primary := answers[primaryUtilityQuestionID]
	if primary.Choice == nil || !choicePassesGate(primary.Choice, questionSetOrEmpty(control.QuestionSet), primaryUtilityQuestionID, thresholds, primary.Choice.Choice) {
		return withAbstainReason(decision, ReasonLowConfidence)
	}
	decision.CandidateStatus = CandidateAvailable
	action := suppressionActionForPrimary(answers)
	if answers[NoulMissingDecisionContext.String()].Noul == nil || answers[NoulRemediationRequiredForIntent.String()].Noul == nil {
		return withAbstainReason(decision, ReasonInvalidResponse)
	}
	missingContext := classifyNoul(NoulMissingDecisionContext, action, answers, thresholds)
	if missingContext == noulInvalid {
		return withReason(decision, ReasonInvalidResponse)
	}
	if missingContext != noulFalse {
		return withAbstainReason(decision, ReasonContextUncertain)
	}
	necessity := classifyNoul(NoulRemediationRequiredForIntent, action, answers, thresholds)
	if necessity == noulInvalid {
		return withReason(decision, ReasonInvalidResponse)
	}
	if remediationRetentionBand(answers, thresholds, action) {
		return withReason(decision, ReasonNecessityRetention)
	}
	if necessity == noulTrue {
		return withReason(decision, ReasonNecessitySignal)
	}
	if necessity != noulFalse {
		return withAbstainReason(decision, ReasonUncertainAnswer)
	}
	switch primary.Choice.Choice {
	case choiceRequired:
		return withReason(decision, ReasonRequired)
	case choiceUsefulNonblocking:
		return withReason(decision, ReasonUseful)
	case choiceInsufficientContext:
		return withAbstainReason(decision, ReasonContextUncertain)
	case choiceOther:
		return withAbstainReason(decision, ReasonUnclassified)
	case choiceLowValue:
		if reason := nonDuplicateRepresentativeGuard(control, answers, thresholds); reason != "" {
			return withAbstainReason(decision, reason)
		}
		if lowValueSignature(answers, thresholds) {
			if err := scoreGuard(control, answers, thresholds); err != "" {
				if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
					return withReason(decision, err)
				}
				return withAbstainReason(decision, err)
			}
			decision.ProposedDecision = DispositionSuppressLowValue
		} else {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
	case choiceScopeExpansion:
		if reason := nonDuplicateRepresentativeGuard(control, answers, thresholds); reason != "" {
			return withAbstainReason(decision, reason)
		}
		if scopeExpansionSignature(answers, thresholds) {
			if err := scoreGuard(control, answers, thresholds); err != "" {
				if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
					return withReason(decision, err)
				}
				return withAbstainReason(decision, err)
			}
			decision.ProposedDecision = DispositionSuppressScopeExpansion
		} else {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
	case choiceDuplicate:
		if !duplicateSignature(answers, thresholds) {
			return withAbstainReason(decision, ReasonUncertainAnswer)
		}
		if candidateOnly {
			if err := scoreGuard(control, answers, thresholds); err != "" {
				if err == ReasonUtilitySignal || err == ReasonInvalidResponse {
					return withReason(decision, err)
				}
				return withAbstainReason(decision, err)
			}
			if err := duplicateRepresentative(control, answers, thresholds); err != "" {
				return withAbstainReason(decision, err)
			}
			decision.ProposedDecision = DispositionSuppressDuplicate
		} else {
			return withAbstainReason(decision, ReasonDuplicateUnresolved)
		}
	default:
		return withAbstainReason(decision, ReasonUnclassified)
	}
	if decision.ProposedDecision == DispositionSuppressLowValue || decision.ProposedDecision == DispositionSuppressScopeExpansion || decision.ProposedDecision == DispositionSuppressDuplicate {
		decision = applyStabilityGuard(decision, control, thresholds)
	}
	return decision
}

func suppressionActionForPrimary(answers AnswerSet) Disposition {
	answer, ok := answers[primaryUtilityQuestionID]
	if !ok || answer.Choice == nil {
		return DispositionSuppressLowValue
	}
	switch answer.Choice.Choice {
	case choiceScopeExpansion:
		return DispositionSuppressScopeExpansion
	case choiceDuplicate:
		return DispositionSuppressDuplicate
	default:
		return DispositionSuppressLowValue
	}
}

type noulClassification string

const (
	noulFalse     noulClassification = "false"
	noulTrue      noulClassification = "true"
	noulUncertain noulClassification = "uncertain"
	noulInvalid   noulClassification = "invalid"
)

func classifyNoul(id NoulID, action Disposition, answers AnswerSet, thresholds ThresholdSet) noulClassification {
	answer, ok := answers[id.String()]
	if !ok || answer.Noul == nil || !finiteProbability(answer.Noul.PTrue) {
		return noulInvalid
	}
	band, ok := thresholds.NoulBands[id]
	if !ok {
		return noulInvalid
	}
	falseSuppress, falseOK := band.FalseSuppress[action]
	trueSuppress, trueOK := band.TrueSuppress[action]
	if !falseOK || !trueOK {
		return noulInvalid
	}
	if answer.Noul.PTrue < falseSuppress {
		return noulFalse
	}
	if answer.Noul.PTrue > trueSuppress {
		return noulTrue
	}
	return noulUncertain
}

// remediationRetentionBand is the conservative interval between the
// retention boundary and the stricter suppression boundary for
// remediation_required_for_intent. A probability in this band must veto both
// the model candidate and the guarded proposal. Equality with true_retain
// remains uncertain, while classifyNoul retains the strict false-suppression
// comparison and the strict true-suppression comparison.
func remediationRetentionBand(answers AnswerSet, thresholds ThresholdSet, action Disposition) bool {
	answer, ok := answers[NoulRemediationRequiredForIntent.String()]
	if !ok || answer.Noul == nil || !finiteProbability(answer.Noul.PTrue) {
		return false
	}
	band, ok := thresholds.NoulBands[NoulRemediationRequiredForIntent]
	trueSuppress, suppressOK := band.TrueSuppress[action]
	if !ok || !suppressOK || !finiteProbability(band.TrueRetain) || !finiteProbability(trueSuppress) {
		return false
	}
	return answer.Noul.PTrue > band.TrueRetain && answer.Noul.PTrue <= trueSuppress
}

func lowValueSignature(answers AnswerSet, thresholds ThresholdSet) bool {
	if classifyNoul(NoulAdjacentImprovement, DispositionSuppressLowValue, answers, thresholds) != noulFalse {
		return false
	}
	grounded := classifyNoul(NoulGroundedInEvidence, DispositionSuppressLowValue, answers, thresholds)
	actionable := classifyNoul(NoulActionable, DispositionSuppressLowValue, answers, thresholds)
	speculative := classifyNoul(NoulSpeculative, DispositionSuppressLowValue, answers, thresholds)
	if grounded == noulInvalid || actionable == noulInvalid || speculative == noulInvalid || !definiteNoul(grounded) || !definiteNoul(actionable) || !definiteNoul(speculative) {
		return false
	}
	if grounded != noulFalse && actionable != noulFalse && speculative != noulTrue {
		return false
	}
	return definiteNoul(classifyNoul(NoulIntroducedOrMateriallyAffected, DispositionSuppressLowValue, answers, thresholds))
}

func scopeExpansionSignature(answers AnswerSet, thresholds ThresholdSet) bool {
	return classifyNoul(NoulGroundedInEvidence, DispositionSuppressScopeExpansion, answers, thresholds) == noulTrue &&
		classifyNoul(NoulActionable, DispositionSuppressScopeExpansion, answers, thresholds) == noulTrue &&
		classifyNoul(NoulAdjacentImprovement, DispositionSuppressScopeExpansion, answers, thresholds) == noulTrue &&
		classifyNoul(NoulSpeculative, DispositionSuppressScopeExpansion, answers, thresholds) == noulFalse &&
		classifyNoul(NoulIntroducedOrMateriallyAffected, DispositionSuppressScopeExpansion, answers, thresholds) == noulFalse
}

func duplicateSignature(answers AnswerSet, thresholds ThresholdSet) bool {
	return classifyNoul(NoulGroundedInEvidence, DispositionSuppressDuplicate, answers, thresholds) == noulTrue &&
		classifyNoul(NoulActionable, DispositionSuppressDuplicate, answers, thresholds) == noulTrue &&
		classifyNoul(NoulSpeculative, DispositionSuppressDuplicate, answers, thresholds) == noulFalse &&
		definiteNoul(classifyNoul(NoulAdjacentImprovement, DispositionSuppressDuplicate, answers, thresholds)) &&
		definiteNoul(classifyNoul(NoulIntroducedOrMateriallyAffected, DispositionSuppressDuplicate, answers, thresholds))
}

func definiteNoul(value noulClassification) bool { return value == noulFalse || value == noulTrue }

func scoreGuard(control Control, answers AnswerSet, thresholds ThresholdSet) string {
	if !thresholds.IncludeUtilityScore {
		return ""
	}
	if control.QuestionSet == nil || !control.QuestionSet.IncludeUtilityScore || !control.IncludeUtilityScore {
		return ReasonScoreUncertain
	}
	answer, ok := answers[utilityScoreQuestionID]
	if !ok || answer.Score == nil {
		return ReasonScoreUncertain
	}
	levelCount := len(answer.Score.Probabilities)
	if levelCount != 4 {
		return ReasonInvalidResponse
	}
	highMass := answer.Score.Probabilities[2] + answer.Score.Probabilities[3]
	lowMass := answer.Score.Probabilities[0] + answer.Score.Probabilities[1]
	if highMass > thresholds.ScoreGate.HighMassRetain && answer.Score.Confidence > thresholds.ScoreGate.ConfidenceRetain {
		return ReasonUtilitySignal
	}
	if !(lowMass > thresholds.ScoreGate.LowMassSuppress && answer.Score.Confidence > thresholds.ScoreGate.ConfidenceSuppress) {
		return ReasonScoreUncertain
	}
	return ""
}

func applyStabilityGuard(decision Decision, control Control, thresholds ThresholdSet) Decision {
	if !thresholds.StabilityParameters.Required {
		return decision
	}
	if control.StabilityReceipt == nil {
		return withAbstainReason(decision, ReasonStabilityUnverified)
	}
	if control.StabilityReceipt.Unstable || !control.StabilityReceipt.Successful {
		return withAbstainReason(decision, ReasonUnstableEvaluation)
	}
	if control.StabilityReceipt.ProtectionSignal {
		return withReason(decision, ReasonModelProtectionSignal)
	}
	return decision
}

func duplicateRepresentative(control Control, answers AnswerSet, thresholds ThresholdSet) string {
	answer := answers[duplicateRepresentativeID]
	if answer.Choice == nil {
		return ReasonDuplicateUnresolved
	}
	gate := thresholds.ChoiceGates.DuplicateRepresentative.Suppress
	if answer.Choice.Choice == "none" || answer.Choice.Choice == choiceInsufficientContext {
		gate = thresholds.ChoiceGates.DuplicateRepresentative.Retain
	}
	if !choicePassesGateWithGate(answer.Choice, gate, answer.Choice.Choice) {
		return ReasonLowConfidence
	}
	if answer.Choice.Choice == "none" {
		return ReasonDuplicateUnresolved
	}
	if answer.Choice.Choice == choiceInsufficientContext {
		return ReasonDuplicateUnresolved
	}
	if control.QuestionSet == nil {
		return ReasonDuplicateInvalidRepresentative
	}
	representative, ok := control.QuestionSet.DuplicateOptionMap[answer.Choice.Choice]
	if !ok || representative == "" {
		return ReasonDuplicateInvalidRepresentative
	}
	return ""
}

func nonDuplicateRepresentativeGuard(_ Control, answers AnswerSet, thresholds ThresholdSet) string {
	answer, ok := answers[duplicateRepresentativeID]
	if !ok || answer.Choice == nil {
		return ReasonInvalidResponse
	}
	if !choicePassesGateWithGate(answer.Choice, thresholds.ChoiceGates.DuplicateRepresentative.Retain, answer.Choice.Choice) {
		return ReasonLowConfidence
	}
	switch answer.Choice.Choice {
	case "none":
		return ""
	case choiceInsufficientContext:
		return ReasonDuplicateUnresolved
	default:
		// A concrete representative selection conflicts with declaring a
		// different suppression class safely disposable.
		return ReasonAnswerConflict
	}
}

func choicePassesGate(answer *ChoiceAnswer, _ QuestionSet, id string, thresholds ThresholdSet, choice string) bool {
	if id == duplicateRepresentativeID {
		gate := thresholds.ChoiceGates.DuplicateRepresentative.Suppress
		if choice == "none" || choice == choiceInsufficientContext {
			gate = thresholds.ChoiceGates.DuplicateRepresentative.Retain
		}
		return choicePassesGateWithGate(answer, gate, choice)
	}
	gate := primaryChoiceGate(thresholds.ChoiceGates.Primary, choice)
	return choicePassesGateWithGate(answer, gate, choice)
}

func primaryChoiceGate(gates PrimaryChoiceGates, choice string) ChoiceGate {
	switch choice {
	case choiceLowValue:
		return gates.LowValue
	case choiceScopeExpansion:
		return gates.ScopeExpansion
	case choiceDuplicate:
		return gates.Duplicate
	default:
		return gates.Retain
	}
}

func choicePassesGateWithGate(answer *ChoiceAnswer, gate ChoiceGate, choice string) bool {
	if answer == nil || !finiteProbability(answer.Confidence) {
		return false
	}
	probability, ok := answer.Probabilities[choice]
	if !ok || !(answer.Confidence > gate.Confidence && probability > gate.SelectedProbability) {
		return false
	}
	for option, value := range answer.Probabilities {
		if option != choice && value >= probability {
			return false
		}
	}
	return true
}

func hasProtectionNoulSignal(_ Control, answers AnswerSet, thresholds ThresholdSet) bool {
	protected := []NoulID{NoulPossibleSecurityRisk, NoulPossibleCorrectnessRisk, NoulPossibleAuthorizationRisk, NoulPossiblePrivacyRisk, NoulPossibleDataLossRisk, NoulPossibleOperationalRisk}
	for _, id := range protected {
		answer := answers[id.String()]
		band, ok := thresholds.NoulBands[id]
		if !ok || answer.Noul == nil || !finiteProbability(answer.Noul.PTrue) {
			return true
		}
		if answer.Noul.PTrue >= band.FalseRetain {
			return true
		}
	}
	return false
}

func validThresholdSet(thresholds ThresholdSet) bool {
	if strings.TrimSpace(thresholds.ID) == "" || strings.TrimSpace(thresholds.Version) == "" || thresholds.Weights == nil || len(thresholds.Weights) != 0 {
		return false
	}
	if !thresholds.TestOnly {
		for _, digest := range []Digest{thresholds.CalibrationManifestDigest, thresholds.RubricDigest, thresholds.PolicyDigest, thresholds.QuestionsDigest, thresholds.ModelConditionDigest, thresholds.BoundProfileDigest, thresholds.EligibilityRuleDigest, thresholds.ArtifactDigest} {
			if !ValidDigest(digest) {
				return false
			}
		}
		if strings.TrimSpace(thresholds.CalibrationVersion) == "" || len(thresholds.ApprovalIDs) == 0 {
			return false
		}
	}
	if len(thresholds.NoulBands) != len(allNoulIDs) {
		return false
	}
	for _, id := range allNoulIDs {
		band, ok := thresholds.NoulBands[id]
		if !ok || !finiteProbability(band.FalseRetain) || !finiteProbability(band.TrueRetain) || band.TrueRetain <= band.FalseRetain {
			return false
		}
		protected := id == NoulPossibleSecurityRisk || id == NoulPossibleCorrectnessRisk || id == NoulPossibleAuthorizationRisk || id == NoulPossiblePrivacyRisk || id == NoulPossibleDataLossRisk || id == NoulPossibleOperationalRisk
		if protected {
			continue
		}
		for _, action := range []Disposition{DispositionSuppressLowValue, DispositionSuppressScopeExpansion, DispositionSuppressDuplicate} {
			low, lowOK := band.FalseSuppress[action]
			high, highOK := band.TrueSuppress[action]
			if !lowOK || !highOK || !finiteProbability(low) || !finiteProbability(high) || low >= band.FalseRetain || low >= high || high <= band.TrueRetain {
				return false
			}
		}
	}
	primary := thresholds.ChoiceGates.Primary
	for _, gate := range []ChoiceGate{primary.Retain, primary.LowValue, primary.ScopeExpansion, primary.Duplicate} {
		if !finiteProbability(gate.Confidence) || !finiteProbability(gate.SelectedProbability) {
			return false
		}
	}
	if primary.LowValue.Confidence <= primary.Retain.Confidence || primary.LowValue.SelectedProbability <= primary.Retain.SelectedProbability || primary.ScopeExpansion.Confidence <= primary.Retain.Confidence || primary.ScopeExpansion.SelectedProbability <= primary.Retain.SelectedProbability || primary.Duplicate.Confidence <= primary.Retain.Confidence || primary.Duplicate.SelectedProbability <= primary.Retain.SelectedProbability {
		return false
	}
	for _, gate := range []ChoiceGate{thresholds.ChoiceGates.DuplicateRepresentative.Retain, thresholds.ChoiceGates.DuplicateRepresentative.Suppress} {
		if !finiteProbability(gate.Confidence) || !finiteProbability(gate.SelectedProbability) || gate.Confidence < 0 || gate.SelectedProbability < 0 || gate.Confidence > 1 || gate.SelectedProbability > 1 {
			return false
		}
	}
	if thresholds.ChoiceGates.DuplicateRepresentative.Suppress.Confidence <= thresholds.ChoiceGates.DuplicateRepresentative.Retain.Confidence || thresholds.ChoiceGates.DuplicateRepresentative.Suppress.SelectedProbability <= thresholds.ChoiceGates.DuplicateRepresentative.Retain.SelectedProbability {
		return false
	}
	if thresholds.IncludeUtilityScore {
		gate := thresholds.ScoreGate
		if !finiteProbability(gate.ConfidenceRetain) || !finiteProbability(gate.ConfidenceSuppress) || !finiteProbability(gate.LowMassSuppress) || !finiteProbability(gate.HighMassRetain) || gate.ConfidenceSuppress <= gate.ConfidenceRetain {
			return false
		}
	}
	return true
}

func keepDecision() Decision {
	return Decision{ProposedDecision: DispositionKeep, EffectiveDecision: DispositionKeep, CandidateStatus: CandidateUnavailable, ModelCandidateDecision: nil, ReasonCodes: []string{}, ReasonTrace: []ReasonTrace{}}
}

func withReason(decision Decision, reason string) Decision {
	decision.ProposedDecision = DispositionKeep
	decision.EffectiveDecision = DispositionKeep
	if reason != "" && !hasReason(decision, reason) {
		decision.ReasonCodes = append(decision.ReasonCodes, reason)
	}
	decision.ReasonTrace = append(decision.ReasonTrace, ReasonTrace{RuleID: reason, Result: ReasonVeto, Decision: dispositionPtr(DispositionKeep)})
	return decision
}

func withAbstainReason(decision Decision, reason string) Decision {
	decision.ProposedDecision = DispositionAbstain
	decision.EffectiveDecision = DispositionKeep
	if reason != "" && !hasReason(decision, reason) {
		decision.ReasonCodes = append(decision.ReasonCodes, reason)
	}
	decision.ReasonTrace = append(decision.ReasonTrace, ReasonTrace{RuleID: reason, Result: ReasonUncertain, Decision: dispositionPtr(DispositionAbstain)})
	return decision
}

func hasReason(decision Decision, reason string) bool {
	for _, item := range decision.ReasonCodes {
		if item == reason {
			return true
		}
	}
	return false
}

func dispositionPtr(value Disposition) *Disposition { return &value }

func evaluatorReason(status EvaluatorStatus) string {
	switch status {
	case EvaluatorInvalidResponse:
		return ReasonInvalidResponse
	case EvaluatorStaleResult:
		return ReasonStaleInput
	case EvaluatorSkippedPermission:
		return ReasonInputNotPermitted
	case EvaluatorSkippedConfig:
		return ReasonUncalibratedPolicy
	case EvaluatorNotRequested, EvaluatorSucceeded, EvaluatorTimeout, EvaluatorCancelled, EvaluatorTransportError, EvaluatorProviderError:
		return ReasonEvaluatorFailure
	default:
		return ReasonEvaluatorFailure
	}
}

func hasDecisionRelevantLimitation(limitations []Limitation) bool {
	for _, item := range limitations {
		if item.Impact == ImpactDecisionRelevant || item.Impact == ImpactUnknown {
			return true
		}
	}
	return false
}

func finiteProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func questionSetOrEmpty(value *QuestionSet) QuestionSet {
	if value == nil {
		return QuestionSet{}
	}
	return *value
}

// FinalizeDuplicates applies cohort-level representative validation in raw
// order. It returns a detached copy and leaves advisory effective decisions at
// keep regardless of proposal changes.
func FinalizeDuplicates(records []EvaluationRecord) []EvaluationRecord {
	out := cloneRecords(records)
	ranked := make([]int, len(out))
	for index := range ranked {
		ranked[index] = index
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		a, b := out[ranked[left]], out[ranked[right]]
		leftSeverity, rightSeverity := severityRankString(a.OriginalSeverity), severityRankString(b.OriginalSeverity)
		if leftSeverity != rightSeverity {
			return leftSeverity > rightSeverity
		}
		if a.SourceOrdinal != b.SourceOrdinal {
			return a.SourceOrdinal < b.SourceOrdinal
		}
		return a.FindingID.String() < b.FindingID.String()
	})
	rankByIndex := make(map[int]int, len(out))
	byID := make(map[string]int, len(out))
	for rank, index := range ranked {
		rankByIndex[index] = rank
		byID[duplicateKey(out[index].RunID, out[index].FindingID)] = index
		out[index].EffectiveDecision = DispositionKeep
	}
	state := make([]int, len(out))
	valid := make([]bool, len(out))
	var finalize func(int) bool
	finalize = func(index int) bool {
		if state[index] == 1 {
			// A representative cycle is invalid for every member reached by
			// this recursion. The caller marks its own edge as invalid too.
			return false
		}
		if state[index] == 2 {
			return valid[index]
		}
		state[index] = 1
		record := &out[index]
		if record.ProposedDecision == DispositionSuppressDuplicate {
			representativeIndex, ok := byID[duplicateKey(record.RunID, record.DuplicateRepresentativeID)]
			if !ok || representativeIndex == index || rankByIndex[representativeIndex] >= rankByIndex[index] {
				invalidateDuplicate(record, ReasonDuplicateInvalidRepresentative)
				state[index] = 2
				valid[index] = false
				return false
			}
			if out[representativeIndex].RunID != record.RunID {
				invalidateDuplicate(record, ReasonDuplicateInvalidRepresentative)
				state[index] = 2
				valid[index] = false
				return false
			}
			if !finalize(representativeIndex) {
				invalidateDuplicate(record, ReasonDuplicateRepresentativeNotKept)
				state[index] = 2
				valid[index] = false
				return false
			}
		}
		state[index] = 2
		valid[index] = record.ProposedDecision == DispositionKeep
		return valid[index]
	}
	for _, index := range ranked {
		finalize(index)
	}
	// Apply the same earlier-ranked representative rule to the diagnostic
	// candidate layer. A guarded keep cannot make a suppressed candidate
	// representative valid; both layers retain their own proposal identity.
	for _, index := range ranked {
		record := &out[index]
		if record.ModelCandidateDecision == nil || *record.ModelCandidateDecision != DispositionSuppressDuplicate {
			continue
		}
		representativeIndex, ok := byID[duplicateKey(record.RunID, record.DuplicateRepresentativeID)]
		if !ok || representativeIndex == index || rankByIndex[representativeIndex] >= rankByIndex[index] {
			invalidateCandidateDuplicate(record, ReasonDuplicateInvalidRepresentative)
			continue
		}
		representative := out[representativeIndex].ModelCandidateDecision
		if representative == nil || *representative != DispositionKeep {
			invalidateCandidateDuplicate(record, ReasonDuplicateRepresentativeNotKept)
		}
	}
	return out
}

func duplicateKey(runID string, findingID interface{ String() string }) string {
	return runID + "\x00" + findingID.String()
}

func invalidateDuplicate(record *EvaluationRecord, reason string) {
	record.ProposedDecision = DispositionAbstain
	record.EffectiveDecision = DispositionKeep
	if !containsString(record.ReasonCodes, reason) {
		record.ReasonCodes = append(record.ReasonCodes, reason)
	}
}

func invalidateCandidateDuplicate(record *EvaluationRecord, reason string) {
	value := DispositionAbstain
	record.ModelCandidateDecision = &value
	if !containsString(record.ReasonCodes, reason) {
		record.ReasonCodes = append(record.ReasonCodes, reason)
	}
}

func cloneRecords(records []EvaluationRecord) []EvaluationRecord {
	out := make([]EvaluationRecord, len(records))
	copy(out, records)
	for index := range out {
		out[index].ProtectionEvidence = append([]ProtectionEvidence(nil), records[index].ProtectionEvidence...)
		out[index].ReasonCodes = append([]string(nil), records[index].ReasonCodes...)
		out[index].ReasonTrace = cloneReasonTrace(records[index].ReasonTrace)
		out[index].Answers = cloneAnswers(records[index].Answers)
		if records[index].RawResponseArtifact != nil {
			value := *records[index].RawResponseArtifact
			out[index].RawResponseArtifact = &value
		}
		if records[index].ModelCandidateDecision != nil {
			value := *records[index].ModelCandidateDecision
			out[index].ModelCandidateDecision = &value
		}
		cloneUsage(&out[index].Usage, records[index].Usage)
		cloneLatency(&out[index].Latency, records[index].Latency)
	}
	return out
}

func cloneReasonTrace(values []ReasonTrace) []ReasonTrace {
	out := make([]ReasonTrace, len(values))
	for index, value := range values {
		out[index] = value
		out[index].InputPaths = append([]string(nil), value.InputPaths...)
		out[index].ThresholdPaths = append([]string(nil), value.ThresholdPaths...)
		if value.ObservedValues != nil {
			out[index].ObservedValues = make(map[string]any, len(value.ObservedValues))
			for key, observed := range value.ObservedValues {
				out[index].ObservedValues[key] = observed
			}
		}
		if value.Decision != nil {
			decision := *value.Decision
			out[index].Decision = &decision
		}
	}
	return out
}

func cloneUsage(dst *Usage, source Usage) {
	*dst = source
	if source.InputTokens != nil {
		value := *source.InputTokens
		dst.InputTokens = &value
	}
	if source.OutputTokens != nil {
		value := *source.OutputTokens
		dst.OutputTokens = &value
	}
	if source.SourceUnits != nil {
		value := *source.SourceUnits
		dst.SourceUnits = &value
	}
	if source.ObservedCost != nil {
		value := *source.ObservedCost
		dst.ObservedCost = &value
	}
	if source.ObservedCurrency != nil {
		value := *source.ObservedCurrency
		dst.ObservedCurrency = &value
	}
	if source.EstimatedCost != nil {
		value := *source.EstimatedCost
		dst.EstimatedCost = &value
	}
	if source.PricingSource != nil {
		value := *source.PricingSource
		dst.PricingSource = &value
	}
	if source.PricingVersion != nil {
		value := *source.PricingVersion
		dst.PricingVersion = &value
	}
}

func cloneLatency(dst *Latency, source Latency) {
	*dst = source
	if source.QueueMS != nil {
		value := *source.QueueMS
		dst.QueueMS = &value
	}
	if source.ProviderMS != nil {
		value := *source.ProviderMS
		dst.ProviderMS = &value
	}
	if source.AuditMS != nil {
		value := *source.AuditMS
		dst.AuditMS = &value
	}
	if source.TotalMS != nil {
		value := *source.TotalMS
		dst.TotalMS = &value
	}
	if source.DeadlineMS != nil {
		value := *source.DeadlineMS
		dst.DeadlineMS = &value
	}
}

func cloneAnswers(answers AnswerSet) AnswerSet {
	if answers == nil {
		return nil
	}
	out := make(AnswerSet, len(answers))
	for key, answer := range answers {
		clone := answer
		if answer.Noul != nil {
			value := *answer.Noul
			clone.Noul = &value
		}
		if answer.Choice != nil {
			value := *answer.Choice
			value.Probabilities = map[string]float64{}
			for option, probability := range answer.Choice.Probabilities {
				value.Probabilities[option] = probability
			}
			clone.Choice = &value
		}
		if answer.Score != nil {
			value := *answer.Score
			value.Legend = append([]string(nil), answer.Score.Legend...)
			value.Probabilities = append([]float64(nil), answer.Score.Probabilities...)
			clone.Score = &value
		}
		out[key] = clone
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
