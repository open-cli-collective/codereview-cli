package findingutility

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func testSnapshot() Snapshot {
	return Snapshot{
		RunID: "run-test-1",
		PR: gitprovider.PR{
			Ref:   gitprovider.PRRef{Host: "github.com", Owner: "org", Repo: "repo", Number: 7},
			Title: "Improve validation",
			Body:  "Deliver the validation change safely.",
			State: gitprovider.PRStateOpen,
			Head:  gitprovider.PRBranchRef{SHA: "head-sha"},
			Base:  gitprovider.PRBranchRef{SHA: "base-sha"},
		},
		Findings: []review.Finding{
			{ID: "F-001", Severity: review.SeverityMinor, FilePath: "internal/example.go", Anchor: review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12}, Body: "Validate the caller before use."},
			{ID: "F-002", Severity: review.SeverityNits, FilePath: "internal/example.go", Anchor: review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 14}, Body: "Consider a future cleanup."},
		},
		FindingSources: []FindingSource{
			{FindingID: "F-001", ReviewerID: "reviewer-1", SourceOrdinal: 0, TaskID: "review-task-1", TaskFingerprint: "sha256:1111111111111111111111111111111111111111111111111111111111111111", OutputDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
			{FindingID: "F-002", ReviewerID: "reviewer-1", SourceOrdinal: 1, TaskID: "review-task-1", TaskFingerprint: "sha256:1111111111111111111111111111111111111111111111111111111111111111", OutputDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		},
		Changes:         []ChangeSource{{ID: "change-1", OldPath: "internal/example.go", NewPath: "internal/example.go", Kind: "modified", Patch: "@@ -12 +12 @@\n- old\n+ new\n", Complete: true}},
		SourceArtifacts: []SourceArtifact{{ID: "source-1", RelativePath: "internal/example.go", Digest: string(DigestBytes([]byte("new"))), Bytes: []byte("new")}},
	}
}

func testBoundProfile() BoundProfile {
	return BoundProfile{
		ID:                     FixtureProfileID,
		MaxStateBytes:          49152,
		MaxRequestTokens:       65536,
		MaxFindingBytes:        8192,
		MaxEvidenceItems:       16,
		MaxEvidenceItemBytes:   4096,
		MaxRelatedFindings:     16,
		MaxRelatedFindingBytes: 8192,
		Tokenizer:              "serialized-utf8",
		TokenizerVersion:       "v1",
	}
}

func testVerificationInputs(t *testing.T, snapshot Snapshot, cohort Digest) VerificationInputs {
	t.Helper()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	return VerificationInputs{
		Snapshot:             snapshot,
		Rubric:               DefaultRubric(),
		Profile:              profile,
		ExpectedCohortDigest: string(cohort),
	}
}

func testQuestions(t *testing.T, includeScore bool, related bool) QuestionSet {
	t.Helper()
	rubric := DefaultRubric()
	rubric.IncludeUtilityScore = includeScore
	state := State{}
	if related {
		state.RelatedFindings = RelatedFindingsState{Items: []RelatedFinding{{ID: "F-000", SourceOrdinal: 0, OriginalSeverity: "minor", Body: "same condition"}}}
	}
	questions, err := Questions(rubric, state)
	if err != nil {
		t.Fatal(err)
	}
	return questions
}

func testThresholds(includeScore bool) ThresholdSet {
	bands := make(map[NoulID]NoulBand, len(allNoulIDs))
	for _, id := range allNoulIDs {
		bands[id] = NoulBand{
			FalseRetain: 0.3,
			TrueRetain:  0.7,
			FalseSuppress: map[Disposition]float64{
				DispositionSuppressLowValue:       0.2,
				DispositionSuppressScopeExpansion: 0.2,
				DispositionSuppressDuplicate:      0.2,
			},
			TrueSuppress: map[Disposition]float64{
				DispositionSuppressLowValue:       0.8,
				DispositionSuppressScopeExpansion: 0.8,
				DispositionSuppressDuplicate:      0.8,
			},
		}
	}
	return ThresholdSet{
		ID:                  "test-only-thresholds",
		Version:             "test-v1",
		IncludeUtilityScore: includeScore,
		NoulBands:           bands,
		ChoiceGates: ChoiceGates{
			Primary: PrimaryChoiceGates{
				Retain:         ChoiceGate{Confidence: 0.7, SelectedProbability: 0.7},
				LowValue:       ChoiceGate{Confidence: 0.8, SelectedProbability: 0.8},
				ScopeExpansion: ChoiceGate{Confidence: 0.8, SelectedProbability: 0.8},
				Duplicate:      ChoiceGate{Confidence: 0.8, SelectedProbability: 0.8},
			},
			DuplicateRepresentative: DuplicateChoiceGates{
				Retain:   ChoiceGate{Confidence: 0.7, SelectedProbability: 0.7},
				Suppress: ChoiceGate{Confidence: 0.8, SelectedProbability: 0.8},
			},
		},
		ScoreGate: ScoreGate{ConfidenceRetain: 0.7, ConfidenceSuppress: 0.8, LowMassSuppress: 0.7, HighMassRetain: 0.7},
		Weights:   map[string]float64{},
		TestOnly:  true,
	}
}

func testControl(t *testing.T, includeScore bool, related bool) (Control, QuestionSet) {
	t.Helper()
	questions := testQuestions(t, includeScore, related)
	control := Control{
		RunID:                "run-test-1",
		FindingID:            "F-001",
		Mode:                 ModeAdvisory,
		Eligibility:          Eligibility{Status: EligibilityEligible, AuthorityKind: AuthorityApprovedDeterministic},
		Protection:           Protection{Status: ProtectionNotEstablished},
		VendorPermission:     VendorPermission{DataClass: DataClassSynthetic, Permitted: true},
		EvaluatorStatus:      EvaluatorSucceeded,
		StateValid:           true,
		Fresh:                true,
		OriginalSeverity:     "minor",
		SourceComplete:       true,
		ContextComplete:      true,
		CandidateSetComplete: true,
		IncludeUtilityScore:  includeScore,
		QuestionSet:          &questions,
	}
	return control, questions
}

func testAnswers(questions QuestionSet, primary string, values map[NoulID]float64) AnswerSet {
	answers := make(AnswerSet, len(questions.Questions))
	for _, question := range questions.Questions {
		switch question.Type {
		case QuestionTypeNoul:
			value := 0.1
			if override, ok := values[NoulID(question.ID)]; ok {
				value = override
			}
			answers[question.ID] = Answer{Type: QuestionTypeNoul, Status: AnswerStatusPresent, Noul: &NoulAnswer{PTrue: value}}
		case QuestionTypeChoice:
			choice := "none"
			if question.ID == primaryUtilityQuestionID {
				choice = primary
			}
			probabilities := make(map[string]float64, len(question.Options))
			for _, option := range question.Options {
				probabilities[option.Key] = 0
			}
			probabilities[choice] = 1
			answers[question.ID] = Answer{Type: QuestionTypeChoice, Status: AnswerStatusPresent, Choice: &ChoiceAnswer{Choice: choice, Probabilities: probabilities, Confidence: 1}}
		case QuestionTypeScore:
			answers[question.ID] = Answer{Type: QuestionTypeScore, Status: AnswerStatusPresent, Score: &ScoreAnswer{Score: 0, Legend: []string{"harmful_or_noise", "marginal", "useful", "essential"}, Probabilities: []float64{1, 0, 0, 0}, Confidence: 1}}
		}
	}
	return answers
}
