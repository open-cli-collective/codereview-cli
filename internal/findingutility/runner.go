package findingutility

import (
	"context"
	"fmt"
	"time"
)

// RunAdvisory is the pure-slice coordinator boundary.  The artifact-session
// lifecycle and typed adapter are intentionally added by a later slice; until
// they are supplied, an enabled-but-unconfigured run records a content-free
// keep outcome and never falls back to the ordinary reviewer adapter.
func RunAdvisory(ctx context.Context, options Options, snapshot Snapshot) Outcome {
	if options.Profile.Mode == "" && options.Profile.Backend == "" && options.Profile.ProfileID == "" {
		// The production pipeline represents disabled utility with nil options;
		// a zero Options value is the package-level equivalent. Do not consume a
		// clock or ID source, call a factory, or touch the artifact root.
		return Outcome{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return keepOutcome(snapshot, nil, nil, ReasonEvaluatorFailure, "cancelled", err, options.Now, options.Warn) //nolint:misspell // Preserve the advisory outcome contract.
	}

	states, controls, stateErr := BuildStates(snapshot, options.Profile.BoundProfile)
	if stateErr != nil {
		return keepOutcome(snapshot, nil, nil, ReasonInvalidState, "invalid_input", stateErr, options.Now, options.Warn)
	}
	rubric, rubricErr := LoadRubric()
	if rubricErr != nil {
		return keepOutcome(snapshot, states, controls, ReasonUncalibratedPolicy, "rubric_unavailable", rubricErr, options.Now, options.Warn)
	}
	for index := range controls {
		questions, err := Questions(rubric, states[index])
		if err != nil {
			return keepOutcome(snapshot, states, controls, ReasonUncalibratedPolicy, "questions_unavailable", err, options.Now, options.Warn)
		}
		controls[index].QuestionSet = &questions
		controls[index].IncludeUtilityScore = options.Profile.IncludeUtilityScore
		controls[index].QuestionsDigest = questions.Digest
	}

	// Slice 1 has no lifecycle or provider implementation. Missing any of the
	// future execution prerequisites is therefore a configuration keep. Keep
	// the guard explicit so a later adapter cannot accidentally be reached by a
	// partially configured call.
	configurationReason := "configuration_unavailable"
	if options.Profile.Mode != ModeAdvisory {
		configurationReason = "mode_not_permitted"
	} else if options.Profile.Backend != BackendFixture || !options.Profile.FixtureOnly {
		configurationReason = "backend_not_permitted"
	} else if options.NewAdapter == nil || options.ResolveModel == nil {
		configurationReason = "adapter_or_model_unconfigured"
	}
	for index := range controls {
		controls[index].EvaluatorStatus = EvaluatorSkippedConfig
		controls[index].Execution.AllowedResolvedModels = append([]string(nil), options.Profile.AllowedResolvedModels...)
		if options.Profile.RequestedModel != "" {
			requested := options.Profile.RequestedModel
			controls[index].Execution.RequestedModel = &requested
		}
		if options.Profile.ProtocolVersion != "" {
			protocol := options.Profile.ProtocolVersion
			controls[index].Execution.ProtocolVersion = &protocol
		}
		if options.Profile.ClientVersion != "" {
			client := options.Profile.ClientVersion
			controls[index].Execution.ClientVersion = &client
		}
	}
	return keepOutcome(snapshot, states, controls, ReasonUncalibratedPolicy, configurationReason, nil, options.Now, options.Warn)
}

func keepOutcome(snapshot Snapshot, states []State, controls []Control, reason, warningCode string, cause error, nowFunc func() time.Time, warnFunc func(Warning)) Outcome {
	rawDigest, _ := DigestCanonical(snapshot.Findings)
	if len(controls) == 0 {
		controls = make([]Control, len(snapshot.Findings))
	}
	if nowFunc == nil {
		nowFunc = time.Now
	}
	now := nowFunc().UTC()
	if len(states) != len(snapshot.Findings) || len(controls) != len(snapshot.Findings) {
		controls = make([]Control, len(snapshot.Findings))
	}
	records := make([]EvaluationRecord, 0, len(snapshot.Findings))
	warnings := make([]Warning, 0, 1)
	for index, finding := range snapshot.Findings {
		control := Control{}
		if index < len(controls) {
			control = controls[index]
		}
		findingDigest, _ := DigestCanonical(finding)
		record := EvaluationRecord{
			RecordSchemaVersion: RecordSchemaVersion,
			RecordID:            fmt.Sprintf("%s/finding-utility-%s", snapshot.RunID, finding.ID.String()),
			RunID:               snapshot.RunID,
			FindingID:           finding.ID,
			SourceOrdinal:       index,
			OriginalSeverity:    originalSeverity(finding.Severity),
			RubricVersion:       RubricVersion,
			PolicyVersion:       PolicyVersion,
			RawFindingDigest:    findingDigest,
			RawFindingsDigest:   rawDigest,
			BaseSHA:             snapshot.PR.Base.SHA,
			HeadSHA:             snapshot.PR.Head.SHA,
			EvaluatorStatus:     EvaluatorSkippedConfig,
			CandidateStatus:     CandidateUnavailable,
			ProposedDecision:    DispositionKeep,
			EffectiveDecision:   DispositionKeep,
			ReasonCodes:         []string{reason},
			AuditStatus:         AuditStatusDegraded,
			CreatedAt:           now,
		}
		if control.StateDigest != "" {
			record.StateDigest = control.StateDigest
			record.ContextDigest = control.ContextDigest
			record.QuestionsDigest = control.QuestionsDigest
			record.RubricDigest = control.RubricDigest
			record.PolicyDigest = control.PolicyDigest
			record.BoundProfileDigest = control.BoundProfileDigest
			record.EvaluatorStatus = control.EvaluatorStatus
			if control.Execution.RequestedModel != nil {
				record.RequestedModel = *control.Execution.RequestedModel
			}
			if control.Execution.ProtocolVersion != nil {
				record.ProtocolVersion = *control.Execution.ProtocolVersion
			}
			if control.Execution.ClientVersion != nil {
				record.ClientVersion = *control.Execution.ClientVersion
			}
		}
		record.ReasonTrace = []ReasonTrace{{RuleID: reason, Result: ReasonVeto, Decision: dispositionPtr(DispositionKeep)}}
		records = append(records, record)
	}
	if warningCode != "" {
		warning := Warning{Code: warningCode, Message: "finding utility retained findings because advisory execution was unavailable"}
		if cause != nil {
			// Do not include cause text: evaluator input and provider errors are
			// intentionally content-free at this warning boundary.
			warning.Message = "finding utility retained findings because advisory execution was unavailable"
		}
		warnings = append(warnings, warning)
		if warnFunc != nil {
			warnFunc(warning)
		}
	}
	return Outcome{AuditStatus: AuditStatusDegraded, Records: records, Warnings: warnings}
}
