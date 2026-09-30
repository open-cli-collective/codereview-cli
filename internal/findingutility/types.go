// Package findingutility contains the provider-neutral, advisory finding
// utility contract.  The package deliberately owns no review-planning or
// posting behavior: its effective decision is always keep in v1.
package findingutility

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// StateSchemaVersion and related values identify the frozen contract schemas.
const (
	StateSchemaVersion     = "cr-finding-state-v1"
	RubricVersion          = "cr-finding-utility-v1"
	PolicyVersion          = "cr-finding-policy-v1"
	LabelSchemaVersion     = "cr-finding-labels-v1"
	CanonicalizerVersion   = "cr-utility-canonical-json-v1"
	EvaluationProtocolV1   = "cr-finding-utility-evaluation-v1"
	RecordSchemaVersion    = 1
	ManifestSchemaVersion  = 1
	RubricSchemaVersion    = 1
	FixtureProfileVersion  = 1
	ModeAdvisory           = "advisory"
	BackendFixture         = "fixture"
	FixtureProfileID       = "evaluator-advisory-fixture-v1"
	FixtureOnlyModelSource = "fixture"
)

// Digest is a content digest in the form sha256:<64 lowercase hex digits>.
type Digest string

// Disposition is the policy output.  Advisory v1 never applies a suppression.
type Disposition string

// DispositionKeep and related values identify policy dispositions.
const (
	DispositionKeep                   Disposition = "keep"
	DispositionAbstain                Disposition = "abstain"
	DispositionSuppressLowValue       Disposition = "suppress_low_value"
	DispositionSuppressScopeExpansion Disposition = "suppress_scope_expansion"
	DispositionSuppressDuplicate      Disposition = "suppress_duplicate"
)

// Valid reports whether d is a supported disposition.
func (d Disposition) Valid() bool {
	switch d {
	case DispositionKeep, DispositionAbstain, DispositionSuppressLowValue,
		DispositionSuppressScopeExpansion, DispositionSuppressDuplicate:
		return true
	default:
		return false
	}
}

// BinaryID identifies one atomic probability question.
type BinaryID string

// BinaryGroundedInEvidence and related values identify the frozen Binary questions.
const (
	BinaryGroundedInEvidence             BinaryID = "grounded_in_evidence"
	BinaryIntroducedOrMateriallyAffected BinaryID = "introduced_or_materially_affected"
	BinaryRemediationRequiredForIntent   BinaryID = "remediation_required_for_intent"
	BinaryAdjacentImprovement            BinaryID = "adjacent_improvement"
	BinarySpeculative                    BinaryID = "speculative"
	BinaryActionable                     BinaryID = "actionable"
	BinaryMissingDecisionContext         BinaryID = "missing_decision_context"
	BinaryPossibleSecurityRisk           BinaryID = "possible_security_risk"
	BinaryPossibleCorrectnessRisk        BinaryID = "possible_correctness_risk"
	BinaryPossibleAuthorizationRisk      BinaryID = "possible_authorization_risk"
	BinaryPossiblePrivacyRisk            BinaryID = "possible_privacy_risk"
	BinaryPossibleDataLossRisk           BinaryID = "possible_data_loss_risk"
	BinaryPossibleOperationalRisk        BinaryID = "possible_operational_risk"
)

var allBinaryIDs = []BinaryID{
	BinaryGroundedInEvidence,
	BinaryIntroducedOrMateriallyAffected,
	BinaryRemediationRequiredForIntent,
	BinaryAdjacentImprovement,
	BinarySpeculative,
	BinaryActionable,
	BinaryMissingDecisionContext,
	BinaryPossibleSecurityRisk,
	BinaryPossibleCorrectnessRisk,
	BinaryPossibleAuthorizationRisk,
	BinaryPossiblePrivacyRisk,
	BinaryPossibleDataLossRisk,
	BinaryPossibleOperationalRisk,
}

func (id BinaryID) String() string { return string(id) }

// AllBinaryIDs returns the frozen v1 order.
func AllBinaryIDs() []BinaryID { return append([]BinaryID(nil), allBinaryIDs...) }

// QuestionType identifies the evaluator question shape.
type QuestionType string

// QuestionTypeBinary and related values identify supported question shapes.
const (
	QuestionTypeBinary QuestionType = "binary"
	QuestionTypeChoice QuestionType = "choice"
	QuestionTypeScore  QuestionType = "score"
)

// ChoiceOption describes one selectable answer option.
type ChoiceOption struct {
	Key      string `json:"key"`
	Criteria string `json:"criteria"`
}

// ScoreLevel describes one position in a score question.
type ScoreLevel struct {
	Position    int    `json:"position"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question is an expanded, evaluator-facing question definition.
type Question struct {
	ID           string            `json:"id"`
	Type         QuestionType      `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Options      []ChoiceOption    `json:"options,omitempty"`
	Levels       []ScoreLevel      `json:"levels,omitempty"`
}

// QuestionSet is the expanded evaluator question set and its digest.
type QuestionSet struct {
	SchemaVersion       int               `json:"schema_version"`
	RubricVersion       string            `json:"rubric_version"`
	Canonicalizer       string            `json:"canonicalizer_version"`
	IncludeUtilityScore bool              `json:"include_utility_score"`
	Questions           []Question        `json:"questions"`
	DuplicateOptionMap  map[string]string `json:"duplicate_option_map"`
	Digest              Digest            `json:"digest"`
}

// Rubric is the frozen semantic definition loaded from rubric-v1.json.
type Rubric struct {
	SchemaVersion        int        `json:"schema_version"`
	StateSchemaVersion   string     `json:"state_schema_version"`
	RubricVersion        string     `json:"rubric_version"`
	PolicyVersion        string     `json:"policy_version"`
	LabelSchemaVersion   string     `json:"label_schema_version"`
	CanonicalizerVersion string     `json:"canonicalizer_version"`
	IncludeUtilityScore  bool       `json:"include_utility_score"`
	SharedInstruction    string     `json:"shared_instruction_prefix"`
	Questions            []Question `json:"questions"`
}

// SourceKind identifies the origin of a state value.
type SourceKind string

// SourcePRTitle and related values identify supported source kinds.
const (
	SourcePRTitle        SourceKind = "pr_title"
	SourcePRBody         SourceKind = "pr_body"
	SourceWorkItem       SourceKind = "work_item"
	SourceRepositoryFile SourceKind = "repository_file"
	SourceDiff           SourceKind = "diff"
	SourceTestResult     SourceKind = "test_result"
	SourceReviewMetadata SourceKind = "review_metadata"
	SourceHumanScope     SourceKind = "human_scope_statement"
)

// SourceRef identifies the source and revision behind a state value.
type SourceRef struct {
	SourceID string     `json:"source_id"`
	Kind     SourceKind `json:"kind"`
	Revision *string    `json:"revision"`
	Digest   Digest     `json:"digest"`
	URI      *string    `json:"uri"`
}

// Location identifies a source file range.
type Location struct {
	Path      string `json:"path"`
	Side      string `json:"side"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// FindingState is the normalized finding presented to an evaluator.
type FindingState struct {
	ID                  string    `json:"id"`
	SourceOrdinal       int       `json:"source_ordinal"`
	ReviewerID          string    `json:"reviewer_id"`
	Title               string    `json:"title"`
	Body                string    `json:"body"`
	OriginalSeverity    string    `json:"original_severity"`
	OriginalSeverityRaw string    `json:"original_severity_raw"`
	Location            *Location `json:"location"`
	SourceRef           SourceRef `json:"source_ref"`
	EvidenceIDs         []string  `json:"evidence_ids"`
}

// Intent describes the requested change and its explicit non-goals.
type Intent struct {
	Text             *string          `json:"text"`
	SourceRefs       []SourceRef      `json:"source_refs"`
	ExplicitNonGoals []ScopeStatement `json:"explicit_non_goals"`
	Unresolved       []string         `json:"unresolved"`
}

// ScopeStatement records one scoped statement and its source.
type ScopeStatement struct {
	Text      string    `json:"text"`
	SourceRef SourceRef `json:"source_ref"`
}

// Change describes one pull-request change and its evidence links.
type Change struct {
	ID          string    `json:"id"`
	OldPath     *string   `json:"old_path"`
	NewPath     *string   `json:"new_path"`
	ChangeKind  string    `json:"change_kind"`
	Diff        string    `json:"diff"`
	SourceRef   SourceRef `json:"source_ref"`
	EvidenceIDs []string  `json:"evidence_ids"`
	Complete    bool      `json:"complete"`
}

// PullRequestState is the normalized pull-request context.
type PullRequestState struct {
	ID           string   `json:"id"`
	RepositoryID string   `json:"repository_id"`
	BaseSHA      string   `json:"base_sha"`
	HeadSHA      string   `json:"head_sha"`
	Title        string   `json:"title"`
	Intent       Intent   `json:"intent"`
	Changes      []Change `json:"changes"`
}

// EvidenceAvailability describes whether a piece of evidence is available.
type EvidenceAvailability string

// EvidencePresent and related values identify evidence availability states.
const (
	EvidencePresent EvidenceAvailability = "present"
	EvidenceMissing EvidenceAvailability = "missing"
	EvidenceOmitted EvidenceAvailability = "omitted"
)

// EvidenceKind identifies the form of an evidence item.
type EvidenceKind string

// EvidenceCode and related values identify supported evidence kinds.
const (
	EvidenceCode          EvidenceKind = "code"
	EvidenceDiff          EvidenceKind = "diff"
	EvidenceTest          EvidenceKind = "test"
	EvidenceRequirement   EvidenceKind = "requirement"
	EvidenceCaller        EvidenceKind = "caller"
	EvidenceConfiguration EvidenceKind = "configuration"
	EvidenceDocumentation EvidenceKind = "documentation"
)

// EvidenceItem is one bounded piece of evaluator evidence.
type EvidenceItem struct {
	ID            string               `json:"id"`
	Kind          EvidenceKind         `json:"kind"`
	Content       string               `json:"content"`
	SourceRef     SourceRef            `json:"source_ref"`
	Location      *Location            `json:"location"`
	Availability  EvidenceAvailability `json:"availability"`
	LimitationIDs []string             `json:"limitation_ids"`
}

// EvidenceRelationKind identifies a relationship between evidence items.
type EvidenceRelationKind string

// RelationCalls and related values identify supported evidence relationships.
const (
	RelationCalls       EvidenceRelationKind = "calls"
	RelationImplements  EvidenceRelationKind = "implements"
	RelationTests       EvidenceRelationKind = "tests"
	RelationConfigures  EvidenceRelationKind = "configures"
	RelationContradicts EvidenceRelationKind = "contradicts"
	RelationSupports    EvidenceRelationKind = "supports"
)

// EvidenceRelation connects two evidence items.
type EvidenceRelation struct {
	FromID    string               `json:"from_id"`
	ToID      string               `json:"to_id"`
	Kind      EvidenceRelationKind `json:"kind"`
	SourceRef SourceRef            `json:"source_ref"`
}

// EvidenceState contains the evidence items and their relations.
type EvidenceState struct {
	Items     []EvidenceItem     `json:"items"`
	Relations []EvidenceRelation `json:"relations"`
}

// RelatedFinding is a candidate representative for another finding.
type RelatedFinding struct {
	ID                  string    `json:"id"`
	SourceOrdinal       int       `json:"source_ordinal"`
	ReviewerID          string    `json:"reviewer_id"`
	Title               string    `json:"title"`
	Body                string    `json:"body"`
	OriginalSeverity    string    `json:"original_severity"`
	OriginalSeverityRaw string    `json:"original_severity_raw"`
	Location            *Location `json:"location"`
	SourceRef           SourceRef `json:"source_ref"`
	EvidenceIDs         []string  `json:"evidence_ids"`
}

// RelatedFindingsState contains the ordered representative candidates.
type RelatedFindingsState struct {
	CandidateSetID      string           `json:"candidate_set_id"`
	ConstructionVersion string           `json:"construction_version"`
	CompleteForRule     bool             `json:"complete_for_rule"`
	CandidateIDs        []string         `json:"candidate_ids"`
	Items               []RelatedFinding `json:"items"`
}

// PolicyState records the policy rules bound into evaluator state.
type PolicyState struct {
	StateSchemaVersion   string   `json:"state_schema_version"`
	RubricVersion        string   `json:"rubric_version"`
	IntentRule           string   `json:"intent_rule"`
	ProtectedDomains     []string `json:"protected_domains"`
	SeverityRule         string   `json:"severity_rule"`
	DuplicationRule      string   `json:"duplication_rule"`
	UntrustedContentRule string   `json:"untrusted_content_rule"`
}

// LimitationCode identifies why evaluator context is limited.
type LimitationCode string

// LimitationMissingIntent and related values identify limitation codes.
const (
	LimitationMissingIntent          LimitationCode = "missing_intent"
	LimitationMissingSource          LimitationCode = "missing_source"
	LimitationTruncatedContext       LimitationCode = "truncated_context"
	LimitationUnresolvedRevision     LimitationCode = "unresolved_revision"
	LimitationConflictingScope       LimitationCode = "conflicting_scope"
	LimitationUnavailableCaller      LimitationCode = "unavailable_caller"
	LimitationCandidateSetIncomplete LimitationCode = "candidate_set_incomplete"
	LimitationRedaction              LimitationCode = "redaction"
	LimitationOther                  LimitationCode = "other"
)

// LimitationImpact describes the effect of a limitation on policy decisions.
type LimitationImpact string

// ImpactDecisionRelevant and related values identify limitation impacts.
const (
	ImpactDecisionRelevant LimitationImpact = "decision_relevant"
	ImpactIrrelevant       LimitationImpact = "irrelevant"
	ImpactUnknown          LimitationImpact = "unknown"
)

// Limitation describes one bounded input limitation.
type Limitation struct {
	ID            string           `json:"id"`
	Code          LimitationCode   `json:"code"`
	AffectedPaths []string         `json:"affected_paths"`
	Description   string           `json:"description"`
	Impact        LimitationImpact `json:"impact"`
	ImpactSource  *SourceRef       `json:"impact_source"`
}

// Freshness describes the recency of supplied context.
type Freshness string

// FreshnessCurrent and related values identify context freshness states.
const (
	FreshnessCurrent Freshness = "current"
	FreshnessStale   Freshness = "stale"
	FreshnessUnknown Freshness = "unknown"
)

// InputLimitations summarizes limitations on evaluator input.
type InputLimitations struct {
	Items           []Limitation `json:"items"`
	SourceComplete  bool         `json:"source_complete"`
	ContextComplete bool         `json:"context_complete"`
	Freshness       Freshness    `json:"freshness"`
	BoundProfileID  string       `json:"bound_profile_id"`
}

// State has exactly the six evaluator state objects defined by rubric v1.
type State struct {
	Finding          FindingState         `json:"finding"`
	PullRequest      PullRequestState     `json:"pull_request"`
	Evidence         EvidenceState        `json:"evidence"`
	RelatedFindings  RelatedFindingsState `json:"related_findings"`
	Policy           PolicyState          `json:"policy"`
	InputLimitations InputLimitations     `json:"input_limitations"`
}

// FindingSource identifies the reviewer output that produced a finding.
type FindingSource struct {
	FindingID       review.FindingID `json:"finding_id"`
	ReviewerID      string           `json:"reviewer_id"`
	SourceOrdinal   int              `json:"source_ordinal"`
	TaskID          string           `json:"task_id"`
	TaskFingerprint string           `json:"task_fingerprint"`
	OutputDigest    string           `json:"output_digest"`
}

// ChangeSource identifies one source diff and its completeness.
type ChangeSource struct {
	ID       string `json:"id"`
	OldPath  string `json:"old_path"`
	NewPath  string `json:"new_path"`
	Kind     string `json:"kind"`
	Patch    string `json:"patch"`
	Complete bool   `json:"complete"`
}

// SourceArtifact contains an exact source file payload and digest.
type SourceArtifact struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	Digest       string `json:"digest"`
	Bytes        []byte `json:"bytes"`
}

// Snapshot is the immutable input bundle for one advisory run.
type Snapshot struct {
	RunID           string           `json:"run_id"`
	PR              gitprovider.PR   `json:"pr"`
	Findings        []review.Finding `json:"findings"`
	FindingSources  []FindingSource  `json:"finding_sources"`
	Changes         []ChangeSource   `json:"changes"`
	SourceArtifacts []SourceArtifact `json:"source_artifacts"`
	ArtifactRoot    string           `json:"artifact_root"`
}

// ProtectedDomain identifies a domain that must not be suppressed casually.
type ProtectedDomain string

// ProtectedSecurity and related values identify protected domains.
const (
	ProtectedSecurity      ProtectedDomain = "security"
	ProtectedCorrectness   ProtectedDomain = "correctness"
	ProtectedAuthorization ProtectedDomain = "authorization"
	ProtectedPrivacy       ProtectedDomain = "privacy"
	ProtectedDataLoss      ProtectedDomain = "data_loss"
	ProtectedOperational   ProtectedDomain = "operational"
)

// ProtectionStatus describes the protection evidence available for a finding.
type ProtectionStatus string

// ProtectionProtected and related values identify protection states.
const (
	ProtectionProtected      ProtectionStatus = "protected"
	ProtectionNotEstablished ProtectionStatus = "not_established"
	ProtectionUnknown        ProtectionStatus = "unknown"
)

// ProtectionEvidence records evidence for a protected domain.
type ProtectionEvidence struct {
	Domain       ProtectedDomain `json:"domain"`
	SourceKind   string          `json:"source_kind"`
	SourceID     string          `json:"source_id"`
	SourceDigest Digest          `json:"source_digest"`
	Detail       string          `json:"detail"`
}

// Protection summarizes protected domains and supporting evidence.
type Protection struct {
	Status   ProtectionStatus     `json:"status"`
	Domains  []ProtectedDomain    `json:"domains"`
	Evidence []ProtectionEvidence `json:"evidence"`
}

// AuthorityKind identifies the source of an eligibility authority.
type AuthorityKind string

// AuthorityHumanAttestation and related values identify authority kinds.
const (
	AuthorityHumanAttestation      AuthorityKind = "human_attestation"
	AuthorityApprovedDeterministic AuthorityKind = "approved_deterministic_rule"
	AuthorityNone                  AuthorityKind = "none"
)

// EligibilityStatus describes whether a finding may be evaluated.
type EligibilityStatus string

// EligibilityEligible and related values identify eligibility states.
const (
	EligibilityEligible   EligibilityStatus = "eligible"
	EligibilityIneligible EligibilityStatus = "ineligible"
	EligibilityUnknown    EligibilityStatus = "unknown"
)

// Eligibility records the authority and digests supporting evaluation.
type Eligibility struct {
	Status             EligibilityStatus `json:"status"`
	AuthorityKind      AuthorityKind     `json:"authority_kind"`
	AuthorityID        *string           `json:"authority_id"`
	AuthorityDigest    *Digest           `json:"authority_digest"`
	BoundFindingDigest *Digest           `json:"bound_finding_digest"`
	BoundContextDigest *Digest           `json:"bound_context_digest"`
	ReasonCodes        []string          `json:"reason_codes"`
	ApprovedAt         *time.Time        `json:"approved_at"`
}

// VendorDataClass identifies the data class sent to a vendor.
type VendorDataClass string

// DataClassPublic and related values identify vendor data classes.
const (
	DataClassPublic    VendorDataClass = "public"
	DataClassSynthetic VendorDataClass = "synthetic"
	DataClassPrivate   VendorDataClass = "private"
)

// VendorPermission records whether a vendor may receive the data class.
type VendorPermission struct {
	DataClass        VendorDataClass `json:"data_class"`
	PermissionID     *string         `json:"permission_id"`
	PermissionDigest *Digest         `json:"permission_digest"`
	Permitted        bool            `json:"permitted"`
}

// Execution records model and deadline constraints for evaluation.
type Execution struct {
	RequestedModel        *string    `json:"requested_model"`
	AllowedResolvedModels []string   `json:"allowed_resolved_models"`
	ProtocolVersion       *string    `json:"protocol_version"`
	ClientVersion         *string    `json:"client_version"`
	DeadlineMS            *int       `json:"deadline_ms"`
	StartedAt             *time.Time `json:"started_at"`
}

// BoundProfile contains the limits and identity bound to evaluator state.
type BoundProfile struct {
	ID                     string `json:"id"`
	Digest                 Digest `json:"digest"`
	MaxStateBytes          int    `json:"max_state_bytes"`
	MaxRequestTokens       int    `json:"max_request_tokens"`
	MaxFindingBytes        int    `json:"max_finding_bytes"`
	MaxEvidenceItems       int    `json:"max_evidence_items"`
	MaxEvidenceItemBytes   int    `json:"max_evidence_item_bytes"`
	MaxRelatedFindings     int    `json:"max_related_findings"`
	MaxRelatedFindingBytes int    `json:"max_related_finding_bytes"`
	Tokenizer              string `json:"tokenizer"`
	TokenizerVersion       string `json:"tokenizer_version"`
	SelectionRuleDigest    Digest `json:"selection_rule_digest"`
}

// DevelopmentProfile is the checked-in synthetic evaluator profile.
type DevelopmentProfile struct {
	SchemaVersion          int              `json:"schema_version"`
	ProfileID              string           `json:"profile_id"`
	Mode                   string           `json:"mode"`
	Backend                string           `json:"backend"`
	FixtureOnly            bool             `json:"fixture_only"`
	IncludeUtilityScore    bool             `json:"include_utility_score"`
	ThresholdSet           *ThresholdSet    `json:"threshold_set"`
	EligibilityAuthorities []string         `json:"eligibility_authorities"`
	TotalDeadlineMS        int              `json:"total_deadline_ms"`
	MaxConcurrency         int              `json:"max_concurrency"`
	NumericTolerance       NumericTolerance `json:"numeric_tolerance"`
	ProtocolVersion        string           `json:"protocol_version"`
	ClientVersion          string           `json:"client_version"`
	VendorPermission       VendorPermission `json:"vendor_permission"`
	BoundProfile           BoundProfile     `json:"bound_profile"`
	RequestedModel         string           `json:"requested_model"`
	AllowedResolvedModels  []string         `json:"allowed_resolved_models"`
}

// NumericTolerance bounds accepted evaluator probability and score values.
type NumericTolerance struct {
	ProbabilitySum float64 `json:"probability_sum"`
	ScoreMean      float64 `json:"score_mean"`
}

// BinaryBand contains retain and suppress thresholds for one Binary.
type BinaryBand struct {
	FalseRetain   float64                 `json:"false_retain"`
	TrueRetain    float64                 `json:"true_retain"`
	FalseSuppress map[Disposition]float64 `json:"false_suppress"`
	TrueSuppress  map[Disposition]float64 `json:"true_suppress"`
}

// ChoiceGate contains confidence thresholds for one choice.
type ChoiceGate struct {
	Confidence          float64 `json:"confidence"`
	SelectedProbability float64 `json:"selected_probability"`
}

// ChoiceGates contains primary and duplicate choice thresholds.
type ChoiceGates struct {
	Primary                 PrimaryChoiceGates   `json:"primary"`
	DuplicateRepresentative DuplicateChoiceGates `json:"duplicate_representative"`
}

// PrimaryChoiceGates contains thresholds for primary utility choices.
type PrimaryChoiceGates struct {
	Retain         ChoiceGate `json:"retain"`
	LowValue       ChoiceGate `json:"low_value"`
	ScopeExpansion ChoiceGate `json:"scope_expansion"`
	Duplicate      ChoiceGate `json:"duplicate"`
}

// DuplicateChoiceGates contains thresholds for duplicate choices.
type DuplicateChoiceGates struct {
	Retain   ChoiceGate `json:"retain"`
	Suppress ChoiceGate `json:"suppress"`
}

// ScoreGate contains thresholds for utility score decisions.
type ScoreGate struct {
	ConfidenceRetain   float64 `json:"confidence_retain"`
	ConfidenceSuppress float64 `json:"confidence_suppress"`
	LowMassSuppress    float64 `json:"low_mass_suppress"`
	HighMassRetain     float64 `json:"high_mass_retain"`
}

// StabilityParameters configures repeated-evaluation stability checks.
type StabilityParameters struct {
	Required bool `json:"required"`
}

// ThresholdSet is the calibrated policy threshold set.
type ThresholdSet struct {
	ID                        string                  `json:"id"`
	Version                   string                  `json:"version"`
	CalibrationManifestDigest Digest                  `json:"calibration_manifest_digest"`
	RubricDigest              Digest                  `json:"rubric_digest"`
	PolicyDigest              Digest                  `json:"policy_digest"`
	QuestionsDigest           Digest                  `json:"questions_digest"`
	ModelConditionDigest      Digest                  `json:"model_condition_digest"`
	BoundProfileDigest        Digest                  `json:"bound_profile_digest"`
	EligibilityRuleDigest     Digest                  `json:"eligibility_rule_digest"`
	IncludeUtilityScore       bool                    `json:"include_utility_score"`
	BinaryBands               map[BinaryID]BinaryBand `json:"binary_bands"`
	ChoiceGates               ChoiceGates             `json:"choice_gates"`
	ScoreGate                 ScoreGate               `json:"score_gate"`
	StabilityParameters       StabilityParameters     `json:"stability_parameters"`
	Weights                   map[string]float64      `json:"weights"`
	ApprovalIDs               []string                `json:"approval_ids"`
	ArtifactDigest            Digest                  `json:"artifact_digest"`
	CalibrationVersion        string                  `json:"calibration_version"`
	TestOnly                  bool                    `json:"test_only"`
}

// AnswerStatus describes the presence and validity of an answer.
type AnswerStatus string

// AnswerStatusPresent and related values identify answer states.
const (
	AnswerStatusPresent AnswerStatus = "present"
	AnswerStatusMissing AnswerStatus = "missing"
	AnswerStatusInvalid AnswerStatus = "invalid"
)

// BinaryAnswer contains the probability assigned to a Binary being true.
type BinaryAnswer struct {
	PTrue   float64 `json:"p_true"`
	present bool
}

// UnmarshalJSON keeps the typed contract strict at the JSON boundary. A
// missing or null p_true must not silently become the valid probability zero.
func (answer *BinaryAnswer) UnmarshalJSON(data []byte) error {
	var value struct {
		PTrue *float64 `json:"p_true"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("findingutility: trailing Binary JSON")
		}
		return err
	}
	if value.PTrue == nil {
		return fmt.Errorf("findingutility: Binary p_true is required and cannot be null")
	}
	answer.PTrue = *value.PTrue
	answer.present = true
	return nil
}

// ChoiceAnswer contains a selected choice and its probability distribution.
type ChoiceAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// ScoreAnswer contains a numeric utility score and its distribution.
type ScoreAnswer struct {
	Score         float64   `json:"score"`
	Legend        []string  `json:"legend"`
	Probabilities []float64 `json:"probabilities"`
	Confidence    float64   `json:"confidence"`
}

// Answer is one typed evaluator response.
type Answer struct {
	Type   QuestionType  `json:"type"`
	Binary *BinaryAnswer `json:"binary,omitempty"`
	Choice *ChoiceAnswer `json:"choice,omitempty"`
	Score  *ScoreAnswer  `json:"score,omitempty"`
	Status AnswerStatus  `json:"status,omitempty"`
}

// AnswerSet maps question IDs to typed evaluator responses.
type AnswerSet map[string]Answer

// EvaluationResponse is the strict typed response from an evaluator.
type EvaluationResponse struct {
	SchemaVersion  int       `json:"schema_version"`
	RequestedModel string    `json:"requested_model"`
	ResolvedModel  string    `json:"resolved_model"`
	RequestID      string    `json:"request_id"`
	Answers        AnswerSet `json:"answers"`
	decoded        bool
}

// UnmarshalJSON decodes an evaluator response with strict field checking.
func (response *EvaluationResponse) UnmarshalJSON(data []byte) error {
	type responseAlias EvaluationResponse
	var value responseAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("findingutility: trailing evaluation response JSON")
		}
		return err
	}
	*response = EvaluationResponse(value)
	response.decoded = true
	return nil
}

// ReasonResult describes the result of one policy rule evaluation.
type ReasonResult string

// ReasonPass and related values identify policy rule results.
const (
	ReasonPass         ReasonResult = "pass"
	ReasonVeto         ReasonResult = "veto"
	ReasonUncertain    ReasonResult = "uncertain"
	ReasonNotEvaluated ReasonResult = "not_evaluated"
)

// ReasonTrace records one policy rule's inputs and result.
type ReasonTrace struct {
	RuleID         string         `json:"rule_id"`
	InputPaths     []string       `json:"input_paths"`
	ObservedValues map[string]any `json:"observed_values"`
	ThresholdPaths []string       `json:"threshold_paths"`
	Result         ReasonResult   `json:"result"`
	Decision       *Disposition   `json:"decision"`
}

// CandidateStatus describes whether a model candidate is available.
type CandidateStatus string

// CandidateAvailable and related values identify candidate states.
const (
	CandidateAvailable   CandidateStatus = "available"
	CandidateUnavailable CandidateStatus = "unavailable"
)

// Decision contains proposed and effective policy outcomes.
type Decision struct {
	ProposedDecision       Disposition     `json:"proposed_decision"`
	EffectiveDecision      Disposition     `json:"effective_decision"`
	ReasonCodes            []string        `json:"reason_codes"`
	ReasonTrace            []ReasonTrace   `json:"reason_trace"`
	CandidateStatus        CandidateStatus `json:"candidate_status"`
	ModelCandidateDecision *Disposition    `json:"model_candidate_decision"`
}

// EvaluatorStatus describes the evaluator lifecycle result.
type EvaluatorStatus string

// EvaluatorNotRequested and related values identify evaluator lifecycle states.
const (
	EvaluatorNotRequested      EvaluatorStatus = "not_requested"
	EvaluatorSkippedPermission EvaluatorStatus = "skipped_permission"
	EvaluatorSkippedConfig     EvaluatorStatus = "skipped_configuration"
	EvaluatorSucceeded         EvaluatorStatus = "succeeded"
	EvaluatorTimeout           EvaluatorStatus = "timeout"
	EvaluatorCancelled         EvaluatorStatus = "cancelled" //nolint:misspell // Preserve the serialized evaluator status contract.
	EvaluatorTransportError    EvaluatorStatus = "transport_error"
	EvaluatorProviderError     EvaluatorStatus = "provider_error"
	EvaluatorInvalidResponse   EvaluatorStatus = "invalid_response"
	EvaluatorStaleResult       EvaluatorStatus = "stale_result"
)

// AuditStatus describes the persisted audit lifecycle result.
type AuditStatus string

// AuditStatusComplete and related values identify audit states.
const (
	AuditStatusComplete AuditStatus = "complete"
	AuditStatusDegraded AuditStatus = "degraded"
	AuditStatusFailed   AuditStatus = "failed"
)

// Usage records evaluator resource usage and pricing metadata.
type Usage struct {
	InputTokens      *int     `json:"input_tokens"`
	OutputTokens     *int     `json:"output_tokens"`
	SourceUnits      *int     `json:"source_units"`
	ObservedCost     *float64 `json:"observed_cost"`
	ObservedCurrency *string  `json:"observed_currency"`
	EstimatedCost    *float64 `json:"estimated_cost"`
	PricingSource    *string  `json:"pricing_source"`
	PricingVersion   *string  `json:"pricing_version"`
}

// Latency records the elapsed stages of evaluator execution.
type Latency struct {
	QueueMS    *int64 `json:"queue_ms"`
	ProviderMS *int64 `json:"provider_ms"`
	AuditMS    *int64 `json:"audit_ms"`
	TotalMS    *int64 `json:"total_ms"`
	DeadlineMS *int   `json:"deadline_ms"`
}

// RawResponseArtifact identifies the persisted raw evaluator response.
type RawResponseArtifact struct {
	RelativePath string `json:"relative_path"`
	Digest       Digest `json:"digest"`
	Bytes        int64  `json:"bytes"`
}

// EvaluationRecord is the durable per-finding evaluation and audit record.
type EvaluationRecord struct {
	RecordSchemaVersion        int                  `json:"record_schema_version"`
	RecordID                   string               `json:"record_id"`
	RunID                      string               `json:"run_id"`
	FindingID                  review.FindingID     `json:"finding_id"`
	SourceOrdinal              int                  `json:"source_ordinal"`
	OriginalSeverity           string               `json:"original_severity"`
	RawFindingDigest           Digest               `json:"raw_finding_digest"`
	RawFindingsDigest          Digest               `json:"raw_findings_digest"`
	BaseSHA                    string               `json:"base_sha"`
	HeadSHA                    string               `json:"head_sha"`
	StateDigest                Digest               `json:"state_digest"`
	ContextDigest              Digest               `json:"context_digest"`
	QuestionsDigest            Digest               `json:"questions_digest"`
	RubricVersion              string               `json:"rubric_version"`
	RubricDigest               Digest               `json:"rubric_digest"`
	PolicyVersion              string               `json:"policy_version"`
	PolicyDigest               Digest               `json:"policy_digest"`
	ThresholdSetID             string               `json:"threshold_set_id"`
	ThresholdSetDigest         Digest               `json:"threshold_set_digest"`
	CalibrationVersion         string               `json:"calibration_version"`
	BoundProfileDigest         Digest               `json:"bound_profile_digest"`
	EligibilityAuthorityDigest Digest               `json:"eligibility_authority_digest"`
	ProtectionEvidence         []ProtectionEvidence `json:"protection_evidence"`
	RequestedModel             string               `json:"requested_model"`
	ResolvedModel              string               `json:"resolved_model"`
	ProtocolVersion            string               `json:"protocol_version"`
	ClientVersion              string               `json:"client_version"`
	EvaluatorStatus            EvaluatorStatus      `json:"evaluator_status"`
	Answers                    AnswerSet            `json:"answers"`
	RawResponseArtifact        *RawResponseArtifact `json:"raw_response_artifact"`
	CandidateStatus            CandidateStatus      `json:"candidate_status"`
	ModelCandidateDecision     *Disposition         `json:"model_candidate_decision"`
	ProposedDecision           Disposition          `json:"proposed_decision"`
	EffectiveDecision          Disposition          `json:"effective_decision"`
	ReasonCodes                []string             `json:"reason_codes"`
	ReasonTrace                []ReasonTrace        `json:"reason_trace"`
	DuplicateRepresentativeID  review.FindingID     `json:"duplicate_representative_id"`
	StabilityReceiptDigest     Digest               `json:"stability_receipt_digest"`
	Usage                      Usage                `json:"usage"`
	Latency                    Latency              `json:"latency"`
	AuditStatus                AuditStatus          `json:"audit_status"`
	CreatedAt                  time.Time            `json:"created_at"`
	AttemptID                  string               `json:"attempt_id"`
	RepeatID                   string               `json:"repeat_id"`
	PerturbationID             string               `json:"perturbation_id"`
	ProviderRequestID          string               `json:"provider_request_id"`
	CacheSource                string               `json:"cache_source"`
	InputFingerprint           Digest               `json:"input_fingerprint"`
	ResultFingerprint          Digest               `json:"result_fingerprint"`
}

// StabilityReceipt records the result of repeated-evaluation checks.
type StabilityReceipt struct {
	Digest           Digest `json:"digest"`
	Successful       bool   `json:"successful"`
	ProtectionSignal bool   `json:"protection_signal"`
	Unstable         bool   `json:"unstable"`
}

// Control contains the identities and gates controlling one finding evaluation.
type Control struct {
	RunID                string            `json:"run_id"`
	TaskID               string            `json:"task_id"`
	FindingID            review.FindingID  `json:"finding_id"`
	RawFindingsDigest    Digest            `json:"raw_findings_digest"`
	StateDigest          Digest            `json:"state_digest"`
	QuestionsDigest      Digest            `json:"questions_digest"`
	ContextDigest        Digest            `json:"context_digest"`
	RubricDigest         Digest            `json:"rubric_digest"`
	PolicyDigest         Digest            `json:"policy_digest"`
	BoundProfileDigest   Digest            `json:"bound_profile_digest"`
	BaseSHA              string            `json:"base_sha"`
	HeadSHA              string            `json:"head_sha"`
	Mode                 string            `json:"mode"`
	Eligibility          Eligibility       `json:"eligibility"`
	Protection           Protection        `json:"protection"`
	ThresholdSet         *ThresholdSet     `json:"threshold_set"`
	StabilityReceipt     *StabilityReceipt `json:"stability_receipt"`
	VendorPermission     VendorPermission  `json:"vendor_permission"`
	Execution            Execution         `json:"execution"`
	EvaluatorStatus      EvaluatorStatus   `json:"evaluator_status"`
	StateValid           bool              `json:"-"`
	Fresh                bool              `json:"-"`
	AuditPersisted       bool              `json:"-"`
	OriginalSeverity     string            `json:"-"`
	SourceComplete       bool              `json:"-"`
	ContextComplete      bool              `json:"-"`
	CandidateSetComplete bool              `json:"-"`
	Limitations          []Limitation      `json:"-"`
	IncludeUtilityScore  bool              `json:"-"`
	QuestionSet          *QuestionSet      `json:"-"`
}

// AdapterFactory constructs an evaluator adapter for one invocation.
type AdapterFactory func(Invocation) (llm.Adapter, error)

// Invocation contains the request and identity passed to an evaluator adapter.
type Invocation struct {
	Version                string            `json:"version"`
	Request                EvaluationRequest `json:"request"`
	InputFingerprint       Digest            `json:"input_fingerprint"`
	Prompt                 string            `json:"prompt"`
	ExpectedResolvedModels []string          `json:"expected_resolved_models"`
	FixtureDigest          Digest            `json:"fixture_digest"`
	DataClass              VendorDataClass   `json:"data_class"`
}

// EvaluationRequest is the provider-neutral evaluator request.
type EvaluationRequest struct {
	ProtocolVersion string      `json:"protocol_version"`
	Model           string      `json:"model"`
	State           State       `json:"state"`
	Questions       QuestionSet `json:"questions"`
}

// Warning describes a non-fatal advisory-run condition.
type Warning struct {
	Code      string           `json:"code"`
	FindingID review.FindingID `json:"finding_id"`
	Message   string           `json:"message"`
}

// Options configures one advisory utility run.
type Options struct {
	Profile      DevelopmentProfile
	NewAdapter   AdapterFactory
	ResolveModel func(string) (string, error)
	Now          func() time.Time
	NewAttemptID func() string
	Warn         func(Warning)
}

// Outcome contains the advisory run's audit result and warnings.
type Outcome struct {
	AuditPath   string             `json:"audit_path"`
	AuditStatus AuditStatus        `json:"audit_status"`
	Records     []EvaluationRecord `json:"records"`
	Warnings    []Warning          `json:"warnings"`
}

// AuditFile identifies one file in a committed audit bundle.
type AuditFile struct {
	RelativePath string `json:"relative_path"`
	Digest       Digest `json:"digest"`
	Bytes        int64  `json:"bytes"`
}

// Manifest identifies the committed audit bundle and its files.
type Manifest struct {
	SchemaVersion       int         `json:"schema_version"`
	Mode                string      `json:"mode"`
	RunID               string      `json:"run_id"`
	CohortInputDigest   Digest      `json:"cohort_input_digest"`
	Status              AuditStatus `json:"status"`
	FindingCount        int         `json:"finding_count"`
	RecordCount         int         `json:"record_count"`
	Files               []AuditFile `json:"files"`
	InputManifestDigest Digest      `json:"input_manifest_digest"`
	RecordDigest        Digest      `json:"record_digest"`
	StartedAt           time.Time   `json:"started_at"`
	CompletedAt         time.Time   `json:"completed_at"`
	Warnings            []Warning   `json:"warnings"`
	FixtureOnly         bool        `json:"fixture_only"`
}

// VerificationInputs supplies independent identity for audit verification.
type VerificationInputs struct {
	Snapshot             Snapshot
	Files                map[string][]byte
	Rubric               Rubric
	Profile              DevelopmentProfile
	ExpectedCohortDigest string
}

// AuditBundle contains the inputs and records to commit as an audit.
type AuditBundle struct {
	Snapshot          Snapshot
	Manifest          Manifest
	Records           []EvaluationRecord
	RawFindings       []review.Finding
	EffectiveFindings []review.Finding
	// VerificationInputs is the independent expected identity used to verify
	// every record before the manifest becomes the committed audit marker.
	// It is deliberately carried by the bundle so a writer cannot produce an
	// apparently complete audit without the expected snapshot, rubric, and
	// bound development profile.
	VerificationInputs VerificationInputs `json:"-"`
}
