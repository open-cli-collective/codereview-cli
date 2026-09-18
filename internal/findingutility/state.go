package findingutility

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const candidateConstructionVersion = "cr-finding-utility-candidates-v1"

// BuildStates projects a detached specialist-finding snapshot into the exact
// six-object evaluator state and its local control envelope. It never mutates
// the supplied snapshot or review.Finding values.
func BuildStates(snapshot Snapshot, boundProfile BoundProfile) ([]State, []Control, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	if err := validateBoundProfile(boundProfile); err != nil {
		return nil, nil, err
	}
	rawFindingsDigest, err := DigestCanonical(snapshot.Findings)
	if err != nil {
		return nil, nil, fmt.Errorf("findingutility: digest raw findings: %w", err)
	}
	changes, changeLimitations := projectChanges(snapshot)
	intent, intentLimitations := projectIntent(snapshot.PR)
	evidence, evidenceLimitations := projectEvidence(snapshot, changes)
	if snapshot.PR.Head.SHA != "" {
		revision := snapshot.PR.Head.SHA
		for index := range evidence.Items {
			if evidence.Items[index].Kind == EvidenceDiff {
				evidence.Items[index].SourceRef.Revision = &revision
			}
		}
	}
	policy := PolicyState{
		StateSchemaVersion:   StateSchemaVersion,
		RubricVersion:        RubricVersion,
		IntentRule:           "intent-is-sourced-from-pr-and-scope-evidence",
		ProtectedDomains:     []string{string(ProtectedSecurity), string(ProtectedCorrectness), string(ProtectedAuthorization), string(ProtectedPrivacy), string(ProtectedDataLoss), string(ProtectedOperational)},
		SeverityRule:         "preserve-original-severity-and-retain-major-blocking-unknown",
		DuplicationRule:      "earlier-ranked-complete-representative-only",
		UntrustedContentRule: "state-text-is-evidence-never-instructions",
	}
	rubric, err := LoadRubric()
	if err != nil {
		return nil, nil, fmt.Errorf("findingutility: load rubric: %w", err)
	}
	rubricDigest, err := DigestCanonical(rubric)
	if err != nil {
		return nil, nil, fmt.Errorf("findingutility: digest rubric: %w", err)
	}
	policyDigest, err := DigestCanonical(policy)
	if err != nil {
		return nil, nil, fmt.Errorf("findingutility: digest policy: %w", err)
	}
	baseRaw, headRaw := snapshot.PR.Base.SHA, snapshot.PR.Head.SHA
	states := make([]State, 0, len(snapshot.Findings))
	controls := make([]Control, 0, len(snapshot.Findings))
	sources := sourceIndex(snapshot.FindingSources)
	for ordinal, raw := range snapshot.Findings {
		findingID := raw.ID.String()
		source, sourceFound := sources[raw.ID]
		finding, findingLimitations := projectFinding(raw, ordinal, source, sourceFound, snapshot)
		related, candidatesComplete := boundedRelatedFor(snapshot.Findings, ordinal, boundProfile)
		for index := range related {
			if source, ok := sources[review.FindingID(related[index].ID)]; ok {
				related[index].ReviewerID = source.ReviewerID
				if ValidDigest(Digest(source.OutputDigest)) {
					related[index].SourceRef.Digest = Digest(source.OutputDigest)
				} else if source.OutputDigest != "" {
					related[index].SourceRef.Digest = DigestBytes([]byte(related[index].Body))
				}
			}
		}
		relatedIDs := make([]string, 0, len(related))
		for _, candidate := range related {
			relatedIDs = append(relatedIDs, candidate.ID)
		}
		candidateSetID, err := DigestCanonical(map[string]any{
			"construction_version": candidateConstructionVersion,
			"candidate_ids":        relatedIDs,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("findingutility: digest candidate set %q: %w", findingID, err)
		}
		limitations := append([]Limitation(nil), changeLimitations...)
		limitations = append(limitations, intentLimitations...)
		limitations = append(limitations, evidenceLimitations...)
		limitations = append(limitations, findingLimitations...)
		for _, candidate := range related {
			if source, ok := sources[review.FindingID(candidate.ID)]; ok && source.OutputDigest != "" && !ValidDigest(Digest(source.OutputDigest)) {
				limitations = append(limitations, limitation("related-source-output-digest-"+candidate.ID, LimitationMissingSource, []string{"related_findings.items[" + candidate.ID + "].source_ref.digest"}, "accepted output digest for a candidate is invalid", ImpactDecisionRelevant))
			}
		}
		if len(raw.Body) > boundProfile.MaxFindingBytes {
			limitations = append(limitations, limitation("finding-body-bound", LimitationTruncatedContext, []string{"finding.body"}, "finding body exceeds the bound profile", ImpactDecisionRelevant))
		}
		if !candidatesComplete {
			limitations = append(limitations, limitation("candidate-set-incomplete", LimitationCandidateSetIncomplete, []string{"related_findings"}, "candidate set exceeds the bound profile", ImpactDecisionRelevant))
		}
		if len(evidence.Items) > boundProfile.MaxEvidenceItems {
			limitations = append(limitations, limitation("evidence-item-bound", LimitationTruncatedContext, []string{"evidence.items"}, "evidence item count exceeds the bound profile", ImpactDecisionRelevant))
		}
		for _, item := range evidence.Items {
			if len([]byte(item.Content)) > boundProfile.MaxEvidenceItemBytes {
				limitations = append(limitations, limitation("evidence-bytes-bound", LimitationTruncatedContext, []string{"evidence.items[" + item.ID + "]"}, "evidence item exceeds the bound profile", ImpactDecisionRelevant))
				break
			}
		}
		if len(snapshot.PR.Base.SHA) == 0 || len(snapshot.PR.Head.SHA) == 0 {
			limitations = append(limitations, limitation("unresolved-revision", LimitationUnresolvedRevision, []string{"pull_request.base_sha", "pull_request.head_sha"}, "base and head revisions are required", ImpactDecisionRelevant))
		}
		state := State{
			Finding: finding,
			PullRequest: PullRequestState{
				ID:           prID(snapshot.PR),
				RepositoryID: repositoryID(snapshot.PR),
				BaseSHA:      baseRaw,
				HeadSHA:      headRaw,
				Title:        snapshot.PR.Title,
				Intent:       cloneIntent(intent),
				Changes:      cloneChanges(changes),
			},
			Evidence: cloneEvidence(evidence),
			RelatedFindings: RelatedFindingsState{
				CandidateSetID:      string(candidateSetID),
				ConstructionVersion: candidateConstructionVersion,
				CompleteForRule:     candidatesComplete,
				CandidateIDs:        relatedIDs,
				Items:               related,
			},
			Policy: clonePolicy(policy),
			InputLimitations: InputLimitations{
				Items:           limitations,
				SourceComplete:  sourceComplete(limitations),
				ContextComplete: contextComplete(limitations),
				Freshness:       FreshnessCurrent,
				BoundProfileID:  boundProfile.ID,
			},
		}
		if headRaw != "" {
			for index := range state.PullRequest.Changes {
				revision := headRaw
				state.PullRequest.Changes[index].SourceRef.Revision = &revision
			}
			for index := range state.RelatedFindings.Items {
				revision := headRaw
				state.RelatedFindings.Items[index].SourceRef.Revision = &revision
			}
			state.Finding.SourceRef.Revision = stringPtr(headRaw)
		}
		if state.PullRequest.BaseSHA == "" || state.PullRequest.HeadSHA == "" {
			state.InputLimitations.Freshness = FreshnessUnknown
		}
		if stateBytes, sizeErr := CanonicalJSON(state); sizeErr != nil {
			return nil, nil, fmt.Errorf("findingutility: serialize state %q: %w", findingID, sizeErr)
		} else if len(stateBytes) > boundProfile.MaxStateBytes || len(stateBytes) > boundProfile.MaxRequestTokens {
			code := "state-bytes-bound"
			description := "serialized state exceeds the bound profile"
			path := "state"
			if len(stateBytes) > boundProfile.MaxRequestTokens && len(stateBytes) <= boundProfile.MaxStateBytes {
				code = "request-token-bound"
				description = "serialized UTF-8 state exceeds the fixture request token bound"
				path = "state"
			}
			state.InputLimitations.Items = append(state.InputLimitations.Items, limitation(code, LimitationTruncatedContext, []string{path}, description, ImpactDecisionRelevant))
			state.InputLimitations.SourceComplete = false
			state.InputLimitations.ContextComplete = false
		}
		if err := validateStateReferences(state); err != nil {
			return nil, nil, fmt.Errorf("findingutility: validate state %q: %w", findingID, err)
		}
		stateDigest, err := DigestCanonical(state)
		if err != nil {
			return nil, nil, fmt.Errorf("findingutility: digest state %q: %w", findingID, err)
		}
		contextDigest, err := DigestCanonical(contextIdentity(state, snapshot.SourceArtifacts, boundProfile))
		if err != nil {
			return nil, nil, fmt.Errorf("findingutility: digest context %q: %w", findingID, err)
		}
		questions, err := Questions(rubric, state)
		if err != nil {
			return nil, nil, fmt.Errorf("findingutility: build questions %q: %w", findingID, err)
		}
		questionDigest := questions.Digest
		control := Control{
			RunID:                snapshot.RunID,
			TaskID:               "finding-utility-" + findingToken(raw.ID),
			FindingID:            raw.ID,
			RawFindingsDigest:    rawFindingsDigest,
			StateDigest:          stateDigest,
			QuestionsDigest:      questionDigest,
			ContextDigest:        contextDigest,
			RubricDigest:         rubricDigest,
			PolicyDigest:         policyDigest,
			BoundProfileDigest:   boundProfileDigest(boundProfile),
			BaseSHA:              baseRaw,
			HeadSHA:              headRaw,
			Mode:                 ModeAdvisory,
			Eligibility:          Eligibility{Status: EligibilityUnknown, AuthorityKind: AuthorityNone, ReasonCodes: []string{"eligibility_unknown"}},
			Protection:           protectionFromFinding(raw),
			VendorPermission:     VendorPermission{DataClass: DataClassSynthetic, Permitted: false},
			EvaluatorStatus:      EvaluatorNotRequested,
			StateValid:           !hasLimitationID(limitations, "invalid-change-shape"),
			Fresh:                state.InputLimitations.Freshness == FreshnessCurrent,
			AuditPersisted:       false,
			OriginalSeverity:     originalSeverity(raw.Severity),
			SourceComplete:       state.InputLimitations.SourceComplete,
			ContextComplete:      state.InputLimitations.ContextComplete,
			CandidateSetComplete: state.RelatedFindings.CompleteForRule,
			Limitations:          cloneLimitations(state.InputLimitations.Items),
		}
		states = append(states, state)
		controls = append(controls, control)
	}
	return states, controls, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if strings.TrimSpace(snapshot.RunID) == "" {
		return fmt.Errorf("findingutility: run ID is required")
	}
	if err := snapshot.PR.Ref.Validate(); err != nil {
		return fmt.Errorf("findingutility: PR ref: %w", err)
	}
	seen := make(map[review.FindingID]struct{}, len(snapshot.Findings))
	for _, finding := range snapshot.Findings {
		if finding.ID == "" {
			return fmt.Errorf("findingutility: finding ID is required")
		}
		if _, exists := seen[finding.ID]; exists {
			return fmt.Errorf("findingutility: duplicate finding ID %q", finding.ID)
		}
		seen[finding.ID] = struct{}{}
		if err := finding.Anchor.Validate(); err != nil {
			return fmt.Errorf("findingutility: finding %q anchor: %w", finding.ID, err)
		}
		if finding.Anchoring != "" && !finding.Anchoring.Valid() {
			return fmt.Errorf("findingutility: finding %q anchoring is invalid", finding.ID)
		}
	}
	seenSources := make(map[review.FindingID]struct{}, len(snapshot.FindingSources))
	for _, source := range snapshot.FindingSources {
		if source.FindingID == "" {
			return fmt.Errorf("findingutility: source finding ID is required")
		}
		if _, exists := seenSources[source.FindingID]; exists {
			return fmt.Errorf("findingutility: duplicate finding source %q", source.FindingID)
		}
		seenSources[source.FindingID] = struct{}{}
	}
	seenChanges := make(map[string]struct{}, len(snapshot.Changes))
	for _, change := range snapshot.Changes {
		if change.ID == "" {
			continue
		}
		if _, exists := seenChanges[change.ID]; exists {
			return fmt.Errorf("findingutility: duplicate change ID %q", change.ID)
		}
		seenChanges[change.ID] = struct{}{}
	}
	seenArtifacts := make(map[string]struct{}, len(snapshot.SourceArtifacts))
	for _, artifact := range snapshot.SourceArtifacts {
		if artifact.ID == "" {
			return fmt.Errorf("findingutility: source artifact ID is required")
		}
		if !validRelativePath(artifact.RelativePath) {
			return fmt.Errorf("findingutility: source artifact path %q is not contained", artifact.RelativePath)
		}
		if _, exists := seenArtifacts[artifact.ID]; exists {
			return fmt.Errorf("findingutility: duplicate source artifact ID %q", artifact.ID)
		}
		seenArtifacts[artifact.ID] = struct{}{}
	}
	return nil
}

func validateStateReferences(state State) error {
	if err := validateSourceRef(state.Finding.SourceRef); err != nil {
		return fmt.Errorf("finding source_ref: %w", err)
	}
	if err := validateLocation(state.Finding.Location); err != nil {
		return fmt.Errorf("finding location: %w", err)
	}
	if !ValidDigest(Digest(state.RelatedFindings.CandidateSetID)) {
		return fmt.Errorf("related_findings candidate_set_id is invalid")
	}
	if state.InputLimitations.BoundProfileID == "" {
		return fmt.Errorf("input_limitations bound_profile_id is required")
	}
	switch state.InputLimitations.Freshness {
	case FreshnessCurrent, FreshnessStale, FreshnessUnknown:
	default:
		return fmt.Errorf("input_limitations freshness %q is invalid", state.InputLimitations.Freshness)
	}
	for index, sourceRef := range state.PullRequest.Intent.SourceRefs {
		if err := validateSourceRef(sourceRef); err != nil {
			return fmt.Errorf("pull_request.intent.source_refs[%d]: %w", index, err)
		}
	}
	for index, nonGoal := range state.PullRequest.Intent.ExplicitNonGoals {
		if err := validateSourceRef(nonGoal.SourceRef); err != nil {
			return fmt.Errorf("pull_request.intent.explicit_non_goals[%d]: %w", index, err)
		}
	}
	for index, change := range state.PullRequest.Changes {
		if change.ID == "" {
			return fmt.Errorf("pull_request.changes[%d] has empty ID", index)
		}
		if change.ChangeKind != "added" && change.ChangeKind != "modified" && change.ChangeKind != "deleted" && change.ChangeKind != "renamed" {
			return fmt.Errorf("pull_request.changes[%d] has invalid kind %q", index, change.ChangeKind)
		}
		if err := validateSourceRef(change.SourceRef); err != nil {
			return fmt.Errorf("pull_request.changes[%d].source_ref: %w", index, err)
		}
	}
	limitationByID := make(map[string]Limitation, len(state.InputLimitations.Items))
	for index, item := range state.InputLimitations.Items {
		if item.ID == "" {
			return fmt.Errorf("input_limitations.items[%d] has empty ID", index)
		}
		if _, exists := limitationByID[item.ID]; exists {
			return fmt.Errorf("duplicate limitation ID %q", item.ID)
		}
		limitationByID[item.ID] = item
		switch item.Impact {
		case ImpactDecisionRelevant, ImpactIrrelevant, ImpactUnknown:
		default:
			return fmt.Errorf("limitation %q has invalid impact %q", item.ID, item.Impact)
		}
		if item.ImpactSource != nil {
			if err := validateSourceRef(*item.ImpactSource); err != nil {
				return fmt.Errorf("limitation %q impact_source: %w", item.ID, err)
			}
		}
	}
	evidenceByID := make(map[string]EvidenceItem, len(state.Evidence.Items))
	for index, item := range state.Evidence.Items {
		if item.ID == "" {
			return fmt.Errorf("evidence.items[%d] has empty ID", index)
		}
		if _, exists := evidenceByID[item.ID]; exists {
			return fmt.Errorf("duplicate evidence ID %q", item.ID)
		}
		if !validEvidenceKind(item.Kind) {
			return fmt.Errorf("evidence item %q has invalid kind %q", item.ID, item.Kind)
		}
		switch item.Availability {
		case EvidencePresent:
			if item.Content == "" {
				return fmt.Errorf("present evidence item %q has empty content", item.ID)
			}
		case EvidenceMissing, EvidenceOmitted:
			if item.Content != "" {
				return fmt.Errorf("missing evidence item %q has content", item.ID)
			}
		default:
			return fmt.Errorf("evidence item %q has invalid availability %q", item.ID, item.Availability)
		}
		if err := validateSourceRef(item.SourceRef); err != nil {
			return fmt.Errorf("evidence item %q source_ref: %w", item.ID, err)
		}
		if err := validateLocation(item.Location); err != nil {
			return fmt.Errorf("evidence item %q location: %w", item.ID, err)
		}
		for _, limitationID := range item.LimitationIDs {
			if _, exists := limitationByID[limitationID]; !exists {
				return fmt.Errorf("evidence item %q references unknown limitation %q", item.ID, limitationID)
			}
		}
		evidenceByID[item.ID] = item
	}
	for index, relation := range state.Evidence.Relations {
		if relation.FromID == "" || relation.ToID == "" {
			return fmt.Errorf("evidence relation %d has empty endpoint", index)
		}
		if _, exists := evidenceByID[relation.FromID]; !exists {
			return fmt.Errorf("evidence relation %d references unknown from_id %q", index, relation.FromID)
		}
		if _, exists := evidenceByID[relation.ToID]; !exists {
			return fmt.Errorf("evidence relation %d references unknown to_id %q", index, relation.ToID)
		}
		if !validEvidenceRelationKind(relation.Kind) {
			return fmt.Errorf("evidence relation %d has invalid kind %q", index, relation.Kind)
		}
		if err := validateSourceRef(relation.SourceRef); err != nil {
			return fmt.Errorf("evidence relation %d source_ref: %w", index, err)
		}
	}
	for _, evidenceID := range state.Finding.EvidenceIDs {
		if _, exists := evidenceByID[evidenceID]; !exists {
			return fmt.Errorf("finding references unknown evidence %q", evidenceID)
		}
	}
	for _, change := range state.PullRequest.Changes {
		for _, evidenceID := range change.EvidenceIDs {
			if _, exists := evidenceByID[evidenceID]; !exists {
				return fmt.Errorf("change %q references unknown evidence %q", change.ID, evidenceID)
			}
		}
	}
	seenCandidates := make(map[string]bool, len(state.RelatedFindings.CandidateIDs))
	for _, candidateID := range state.RelatedFindings.CandidateIDs {
		if seenCandidates[candidateID] {
			return fmt.Errorf("duplicate candidate ID %q", candidateID)
		}
		seenCandidates[candidateID] = true
		if _, exists := evidenceByID[candidateID]; exists {
			return fmt.Errorf("candidate ID %q collides with evidence ID", candidateID)
		}
	}
	for _, candidate := range state.RelatedFindings.Items {
		if !seenCandidates[candidate.ID] {
			return fmt.Errorf("related finding %q is absent from candidate_ids", candidate.ID)
		}
		if err := validateSourceRef(candidate.SourceRef); err != nil {
			return fmt.Errorf("related finding %q source_ref: %w", candidate.ID, err)
		}
		if err := validateLocation(candidate.Location); err != nil {
			return fmt.Errorf("related finding %q location: %w", candidate.ID, err)
		}
		for _, evidenceID := range candidate.EvidenceIDs {
			if _, exists := evidenceByID[evidenceID]; !exists {
				return fmt.Errorf("related finding %q references unknown evidence %q", candidate.ID, evidenceID)
			}
		}
	}
	if len(state.RelatedFindings.CandidateIDs) != len(state.RelatedFindings.Items) {
		return fmt.Errorf("candidate_ids and related finding items differ")
	}
	return nil
}

func validateSourceRef(ref SourceRef) error {
	if strings.TrimSpace(ref.SourceID) == "" {
		return fmt.Errorf("source_id is required")
	}
	if !validSourceKind(ref.Kind) {
		return fmt.Errorf("kind %q is invalid", ref.Kind)
	}
	if !ValidDigest(ref.Digest) {
		return fmt.Errorf("digest is invalid")
	}
	return nil
}

func validateLocation(location *Location) error {
	if location == nil {
		return nil
	}
	if strings.TrimSpace(location.Path) == "" || (location.Side != "base" && location.Side != "head") || location.LineStart <= 0 || location.LineEnd < location.LineStart {
		return fmt.Errorf("location is invalid")
	}
	return nil
}

func validSourceKind(kind SourceKind) bool {
	switch kind {
	case SourcePRTitle, SourcePRBody, SourceWorkItem, SourceRepositoryFile, SourceDiff, SourceTestResult, SourceReviewMetadata, SourceHumanScope:
		return true
	default:
		return false
	}
}

func validEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidenceCode, EvidenceDiff, EvidenceTest, EvidenceRequirement, EvidenceCaller, EvidenceConfiguration, EvidenceDocumentation:
		return true
	default:
		return false
	}
}

func validEvidenceRelationKind(kind EvidenceRelationKind) bool {
	switch kind {
	case RelationCalls, RelationImplements, RelationTests, RelationConfigures, RelationContradicts, RelationSupports:
		return true
	default:
		return false
	}
}

func validateBoundProfile(profile BoundProfile) error {
	if strings.TrimSpace(profile.ID) == "" {
		return fmt.Errorf("findingutility: bound profile ID is required")
	}
	if profile.MaxStateBytes <= 0 || profile.MaxRequestTokens <= 0 || profile.MaxFindingBytes <= 0 || profile.MaxEvidenceItems <= 0 || profile.MaxEvidenceItemBytes <= 0 || profile.MaxRelatedFindings < 0 || profile.MaxRelatedFindingBytes <= 0 {
		return fmt.Errorf("findingutility: bound profile limits must be positive")
	}
	if strings.TrimSpace(profile.Tokenizer) == "" || strings.TrimSpace(profile.TokenizerVersion) == "" {
		return fmt.Errorf("findingutility: bound profile tokenizer identity is required")
	}
	return nil
}

func projectFinding(raw review.Finding, ordinal int, source FindingSource, sourceFound bool, snapshot Snapshot) (FindingState, []Limitation) {
	severity := originalSeverity(raw.Severity)
	sourceRef := SourceRef{SourceID: "finding:" + raw.ID.String(), Kind: SourceReviewMetadata, Digest: DigestBytes([]byte(raw.Body))}
	reviewerID := ""
	limitations := []Limitation(nil)
	if sourceFound {
		reviewerID = source.ReviewerID
		if ValidDigest(Digest(source.OutputDigest)) {
			sourceRef.Digest = Digest(source.OutputDigest)
		} else if source.OutputDigest != "" {
			limitations = append(limitations, limitation("source-output-digest", LimitationMissingSource, []string{"finding.source_ref.digest"}, "accepted output digest is invalid", ImpactDecisionRelevant))
		}
		if source.ReviewerID != "" {
			sourceRef.SourceID = "reviewer:" + source.ReviewerID
		}
		if source.TaskID == "" || !ValidDigest(Digest(source.TaskFingerprint)) || !ValidDigest(Digest(source.OutputDigest)) {
			limitations = append(limitations, limitation("source-provenance", LimitationMissingSource, []string{"finding.source_ref"}, "source task or accepted output digest is unavailable", ImpactDecisionRelevant))
		}
	} else {
		limitations = append(limitations, limitation("missing-source", LimitationMissingSource, []string{"finding.source_ref", "finding.reviewer_id"}, "reviewer ownership or accepted output provenance is unavailable", ImpactDecisionRelevant))
	}
	location := findingLocation(raw)
	evidenceIDs := []string(nil)
	for _, change := range snapshot.Changes {
		if change.Patch != "" {
			evidenceIDs = append(evidenceIDs, "change:"+change.ID)
		}
	}
	return FindingState{
		ID:                  raw.ID.String(),
		SourceOrdinal:       ordinal,
		ReviewerID:          reviewerID,
		Title:               "",
		Body:                raw.Body,
		OriginalSeverity:    severity,
		OriginalSeverityRaw: raw.Severity.String(),
		Location:            location,
		SourceRef:           sourceRef,
		EvidenceIDs:         evidenceIDs,
	}, limitations
}

func projectIntent(pr gitprovider.PR) (Intent, []Limitation) {
	text := strings.TrimSpace(pr.Title)
	if strings.TrimSpace(pr.Body) != "" {
		if text != "" {
			text += "\n\n"
		}
		text += pr.Body
	}
	var value *string
	if text != "" {
		value = &text
	}
	limitations := []Limitation(nil)
	if strings.TrimSpace(pr.Title) == "" {
		limitations = append(limitations, limitation("missing-intent-title", LimitationMissingIntent, []string{"pull_request.title", "pull_request.intent.source_refs[0]"}, "pull request title intent is unavailable", ImpactDecisionRelevant))
	}
	if strings.TrimSpace(pr.Body) == "" {
		limitations = append(limitations, limitation("missing-intent-body", LimitationMissingIntent, []string{"pull_request.body", "pull_request.intent.source_refs[1]"}, "pull request body intent is unavailable", ImpactDecisionRelevant))
	}
	return Intent{Text: value, SourceRefs: []SourceRef{prSourceRef(pr, SourcePRTitle), prSourceRef(pr, SourcePRBody)}, ExplicitNonGoals: []ScopeStatement{}, Unresolved: []string{}}, limitations
}

func projectChanges(snapshot Snapshot) ([]Change, []Limitation) {
	changes := make([]Change, 0, len(snapshot.Changes))
	limitations := []Limitation(nil)
	for _, source := range snapshot.Changes {
		kind := source.Kind
		if kind == "" {
			switch {
			case source.OldPath == "":
				kind = "added"
			case source.NewPath == "":
				kind = "deleted"
			case source.OldPath != source.NewPath:
				kind = "renamed"
			default:
				kind = "modified"
			}
		}
		if source.ID == "" {
			limitations = append(limitations, limitation("invalid-change-shape", LimitationOther, []string{"pull_request.changes"}, "change source ID is unavailable", ImpactDecisionRelevant))
		}
		if !validChangeShape(source, kind) {
			limitations = append(limitations, limitation("invalid-change-shape", LimitationOther, []string{"pull_request.changes[" + source.ID + "]"}, "change headers conflict with the declared change kind", ImpactDecisionRelevant))
		}
		ref := SourceRef{SourceID: "change:" + source.ID, Kind: SourceDiff, Digest: DigestBytes([]byte(source.Patch))}
		oldPath, newPath := optionalString(source.OldPath), optionalString(source.NewPath)
		changes = append(changes, Change{ID: source.ID, OldPath: oldPath, NewPath: newPath, ChangeKind: kind, Diff: source.Patch, SourceRef: ref, EvidenceIDs: []string{"change:" + source.ID}, Complete: source.Complete})
		if !source.Complete {
			limitations = append(limitations, limitation("truncated-context", LimitationTruncatedContext, []string{"pull_request.changes[" + source.ID + "]"}, "supplied change coverage is incomplete", ImpactDecisionRelevant))
		}
	}
	return changes, limitations
}

func projectEvidence(snapshot Snapshot, changes []Change) (EvidenceState, []Limitation) {
	items := make([]EvidenceItem, 0, len(changes)+4+len(snapshot.SourceArtifacts))
	limitations := []Limitation(nil)
	titleLimitations := []string(nil)
	if strings.TrimSpace(snapshot.PR.Title) == "" {
		titleLimitations = []string{"missing-intent-title"}
	}
	bodyLimitations := []string(nil)
	if strings.TrimSpace(snapshot.PR.Body) == "" {
		bodyLimitations = []string{"missing-intent-body"}
	}
	items = append(items, EvidenceItem{ID: "intent:title", Kind: EvidenceRequirement, Content: snapshot.PR.Title, SourceRef: prSourceRef(snapshot.PR, SourcePRTitle), Availability: availability(snapshot.PR.Title), LimitationIDs: titleLimitations})
	items = append(items, EvidenceItem{ID: "intent:body", Kind: EvidenceRequirement, Content: snapshot.PR.Body, SourceRef: prSourceRef(snapshot.PR, SourcePRBody), Availability: availability(snapshot.PR.Body), LimitationIDs: bodyLimitations})
	callerRef := SourceRef{SourceID: "evidence:caller", Kind: SourceReviewMetadata, Digest: DigestBytes(nil)}
	items = append(items, EvidenceItem{ID: "evidence:caller", Kind: EvidenceCaller, Content: "", SourceRef: callerRef, Availability: EvidenceMissing, LimitationIDs: []string{"unavailable-caller"}})
	limitations = append(limitations, limitation("unavailable-caller", LimitationUnavailableCaller, []string{"evidence.items[evidence:caller]"}, "caller evidence was not supplied in the immutable advisory snapshot", ImpactDecisionRelevant))
	testRef := SourceRef{SourceID: "evidence:test", Kind: SourceTestResult, Digest: DigestBytes(nil)}
	items = append(items, EvidenceItem{ID: "evidence:test", Kind: EvidenceTest, Content: "", SourceRef: testRef, Availability: EvidenceMissing, LimitationIDs: []string{"unavailable-test-evidence"}})
	limitations = append(limitations, limitation("unavailable-test-evidence", LimitationOther, []string{"evidence.items[evidence:test]"}, "test evidence was not supplied in the immutable advisory snapshot", ImpactDecisionRelevant))
	for _, change := range changes {
		changeLimitations := []string(nil)
		if !change.Complete {
			changeLimitations = []string{"truncated-context"}
		}
		items = append(items, EvidenceItem{ID: "change:" + change.ID, Kind: EvidenceDiff, Content: change.Diff, SourceRef: change.SourceRef, Availability: availability(change.Diff), LimitationIDs: changeLimitations})
	}
	for _, artifact := range snapshot.SourceArtifacts {
		artifactLimitations := []string(nil)
		if len(artifact.Bytes) == 0 {
			artifactLimitations = []string{"missing-source"}
		}
		digest := Digest(artifact.Digest)
		if !ValidDigest(digest) || (len(artifact.Bytes) > 0 && digest != DigestBytes(artifact.Bytes)) {
			artifactLimitations = append(artifactLimitations, "source-digest-invalid")
			digest = DigestBytes(artifact.Bytes)
		}
		if len(artifactLimitations) > 0 {
			limitations = append(limitations, limitation("artifact-source-limitation-"+artifact.ID, LimitationMissingSource, []string{"evidence.items[artifact:" + artifact.ID + "]"}, "source artifact bytes or digest are unavailable", ImpactDecisionRelevant))
		}
		items = append(items, EvidenceItem{ID: "artifact:" + artifact.ID, Kind: EvidenceCode, Content: string(artifact.Bytes), SourceRef: SourceRef{SourceID: artifact.ID, Kind: SourceRepositoryFile, Revision: optionalString(snapshot.PR.Head.SHA), Digest: digest}, Availability: availabilityBytes(artifact.Bytes), LimitationIDs: artifactLimitations})
	}
	if len(changes) == 0 {
		limitations = append(limitations, limitation("missing-source", LimitationMissingSource, []string{"evidence.items"}, "no changed-hunk evidence is available", ImpactDecisionRelevant))
	}
	return EvidenceState{Items: items, Relations: []EvidenceRelation{}}, limitations
}

func relatedFor(findings []review.Finding, ordinal int) []RelatedFinding {
	type ranked struct {
		finding review.Finding
		ordinal int
	}
	rankedFindings := make([]ranked, 0, len(findings))
	for index, finding := range findings {
		rankedFindings = append(rankedFindings, ranked{finding: finding, ordinal: index})
	}
	sort.SliceStable(rankedFindings, func(i, j int) bool {
		left, right := rankedFindings[i], rankedFindings[j]
		leftWeight, rightWeight := severityRank(left.finding.Severity), severityRank(right.finding.Severity)
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		if left.ordinal != right.ordinal {
			return left.ordinal < right.ordinal
		}
		return left.finding.ID.String() < right.finding.ID.String()
	})
	current := findings[ordinal]
	currentRank := -1
	for index, value := range rankedFindings {
		if value.finding.ID == current.ID {
			currentRank = index
			break
		}
	}
	if currentRank <= 0 {
		return []RelatedFinding{}
	}
	out := make([]RelatedFinding, 0, currentRank)
	for _, value := range rankedFindings[:currentRank] {
		out = append(out, RelatedFinding{ID: value.finding.ID.String(), SourceOrdinal: value.ordinal, ReviewerID: "", Title: "", Body: value.finding.Body, OriginalSeverity: originalSeverity(value.finding.Severity), OriginalSeverityRaw: value.finding.Severity.String(), Location: findingLocation(value.finding), SourceRef: SourceRef{SourceID: "finding:" + value.finding.ID.String(), Kind: SourceReviewMetadata, Digest: DigestBytes([]byte(value.finding.Body))}, EvidenceIDs: []string{}})
	}
	return out
}

func boundedRelatedFor(findings []review.Finding, ordinal int, profile BoundProfile) ([]RelatedFinding, bool) {
	related := relatedFor(findings, ordinal)
	complete := true
	if profile.MaxRelatedFindings >= 0 && len(related) > profile.MaxRelatedFindings {
		related = related[:profile.MaxRelatedFindings]
		complete = false
	}
	if profile.MaxRelatedFindingBytes > 0 {
		for index, candidate := range related {
			if len([]byte(candidate.Body)) > profile.MaxRelatedFindingBytes {
				related = related[:index]
				complete = false
				break
			}
		}
	}
	return related, complete
}

func sourceIndex(sources []FindingSource) map[review.FindingID]FindingSource {
	result := make(map[review.FindingID]FindingSource, len(sources))
	for _, source := range sources {
		result[source.FindingID] = source
	}
	return result
}

func originalSeverity(severity review.Severity) string {
	switch severity {
	case review.SeverityBlocking:
		return "blocking"
	case review.SeverityMajor:
		return "major"
	case review.SeverityMinor:
		return "minor"
	case review.SeverityNits:
		return "nit"
	default:
		return "unknown"
	}
}

func severityRank(severity review.Severity) int {
	return severityRankString(originalSeverity(severity))
}

func severityRankString(severity string) int {
	switch severity {
	case "blocking":
		return 5
	case "major":
		return 4
	case "minor":
		return 3
	case "nit":
		return 2
	default:
		return 1
	}
}

func findingLocation(finding review.Finding) *Location {
	if finding.Anchor.Kind != review.AnchorKindLine || finding.Anchor.Line <= 0 {
		return nil
	}
	side := ""
	switch finding.Anchor.Side {
	case review.DiffSideLeft:
		side = "base"
	case review.DiffSideRight:
		side = "head"
	}
	if side == "" {
		return nil
	}
	return &Location{Path: finding.FilePath, Side: side, LineStart: finding.Anchor.Line, LineEnd: finding.Anchor.Line}
}

func protectionFromFinding(finding review.Finding) Protection {
	if finding.Severity == review.SeverityBlocking || finding.Severity == review.SeverityMajor {
		return Protection{Status: ProtectionUnknown, Domains: []ProtectedDomain{}, Evidence: []ProtectionEvidence{}}
	}
	return Protection{Status: ProtectionNotEstablished, Domains: []ProtectedDomain{}, Evidence: []ProtectionEvidence{}}
}

func prID(pr gitprovider.PR) string {
	return fmt.Sprintf("%s/%s/%s#%d", pr.Ref.Host, pr.Ref.Owner, pr.Ref.Repo, pr.Ref.Number)
}

func repositoryID(pr gitprovider.PR) string {
	return pr.Ref.Host + "/" + pr.Ref.Owner + "/" + pr.Ref.Repo
}

func prSourceRef(pr gitprovider.PR, kind SourceKind) SourceRef {
	value := ""
	switch kind {
	case SourcePRTitle:
		value = pr.Title
	case SourcePRBody:
		value = pr.Body
	case SourceWorkItem, SourceRepositoryFile, SourceDiff, SourceTestResult, SourceReviewMetadata, SourceHumanScope:
		// These source kinds do not carry pull-request title or body text.
	}
	return SourceRef{SourceID: prID(pr) + ":" + string(kind), Kind: kind, Revision: optionalString(pr.Head.SHA), Digest: DigestBytes([]byte(value))}
}

func contextIdentity(state State, artifacts []SourceArtifact, profile BoundProfile) any {
	return map[string]any{"state": state, "source_artifacts": artifacts, "bound_profile": profile, "selection_rule": candidateConstructionVersion}
}

func boundProfileDigest(profile BoundProfile) Digest {
	profile.Digest = ""
	digest, err := DigestCanonical(profile)
	if err != nil {
		return ""
	}
	return digest
}

func findingToken(id review.FindingID) string {
	digest := DigestBytes([]byte(id.String()))
	return strings.TrimPrefix(string(digest), "sha256:")
}

func sourceComplete(limitations []Limitation) bool {
	for _, item := range limitations {
		if item.Code == LimitationMissingSource || item.Code == LimitationMissingIntent {
			return false
		}
	}
	return true
}

func contextComplete(limitations []Limitation) bool {
	for _, item := range limitations {
		if item.Impact != ImpactIrrelevant {
			return false
		}
	}
	return true
}

func limitation(id string, code LimitationCode, paths []string, description string, impact LimitationImpact) Limitation {
	return Limitation{ID: id, Code: code, AffectedPaths: append([]string(nil), paths...), Description: description, Impact: impact}
}

func availability(value string) EvidenceAvailability {
	if strings.TrimSpace(value) == "" {
		return EvidenceMissing
	}
	return EvidencePresent
}

func availabilityBytes(value []byte) EvidenceAvailability {
	if len(value) == 0 {
		return EvidenceMissing
	}
	return EvidencePresent
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	clone := value
	return &clone
}

func stringPtr(value string) *string {
	clone := value
	return &clone
}

func cloneChanges(changes []Change) []Change {
	out := make([]Change, len(changes))
	copy(out, changes)
	for index := range out {
		out[index].EvidenceIDs = append([]string(nil), changes[index].EvidenceIDs...)
	}
	return out
}

func cloneIntent(intent Intent) Intent {
	clone := intent
	clone.SourceRefs = append([]SourceRef(nil), intent.SourceRefs...)
	clone.ExplicitNonGoals = append([]ScopeStatement(nil), intent.ExplicitNonGoals...)
	clone.Unresolved = append([]string(nil), intent.Unresolved...)
	if intent.Text != nil {
		text := *intent.Text
		clone.Text = &text
	}
	return clone
}

func cloneEvidence(evidence EvidenceState) EvidenceState {
	clone := EvidenceState{
		Items:     append([]EvidenceItem(nil), evidence.Items...),
		Relations: append([]EvidenceRelation(nil), evidence.Relations...),
	}
	for index := range clone.Items {
		clone.Items[index].LimitationIDs = append([]string(nil), evidence.Items[index].LimitationIDs...)
	}
	return clone
}

func clonePolicy(policy PolicyState) PolicyState {
	clone := policy
	clone.ProtectedDomains = append([]string(nil), policy.ProtectedDomains...)
	return clone
}

func cloneLimitations(limitations []Limitation) []Limitation {
	out := make([]Limitation, len(limitations))
	copy(out, limitations)
	for index := range out {
		out[index].AffectedPaths = append([]string(nil), limitations[index].AffectedPaths...)
	}
	return out
}

func validChangeShape(source ChangeSource, kind string) bool {
	switch kind {
	case "added":
		return source.OldPath == "" && source.NewPath != ""
	case "deleted":
		return source.OldPath != "" && source.NewPath == ""
	case "renamed":
		return source.OldPath != "" && source.NewPath != "" && source.OldPath != source.NewPath
	case "modified":
		return source.OldPath != "" && source.NewPath != "" && source.OldPath == source.NewPath
	default:
		return false
	}
}

func hasLimitationID(limitations []Limitation, id string) bool {
	for _, item := range limitations {
		if item.ID == id {
			return true
		}
	}
	return false
}

func validRelativePath(value string) bool {
	if value == "" || filepath.IsAbs(value) {
		return false
	}
	clean := filepath.Clean(value)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
