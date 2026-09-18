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
	FixtureProfileID       = "jev-advisory-fixture-v1"
	FixtureOnlyModelSource = "fixture"
)

// Digest is a content digest in the form sha256:<64 lowercase hex digits>.
type Digest string

// Disposition is the policy output.  Advisory v1 never applies a suppression.
type Disposition string

const (
	DispositionKeep                   Disposition = "keep"
	DispositionAbstain                Disposition = "abstain"
	DispositionSuppressLowValue       Disposition = "suppress_low_value"
	DispositionSuppressScopeExpansion Disposition = "suppress_scope_expansion"
	DispositionSuppressDuplicate      Disposition = "suppress_duplicate"
)

func (d Disposition) Valid() bool {
	switch d {
	case DispositionKeep, DispositionAbstain, DispositionSuppressLowValue,
		DispositionSuppressScopeExpansion, DispositionSuppressDuplicate:
		return true
	default:
		return false
	}
}

// NoulID identifies one atomic probability question.
type NoulID string

const (
	NoulGroundedInEvidence             NoulID = "grounded_in_evidence"
	NoulIntroducedOrMateriallyAffected NoulID = "introduced_or_materially_affected"
	NoulRemediationRequiredForIntent   NoulID = "remediation_required_for_intent"
	NoulAdjacentImprovement            NoulID = "adjacent_improvement"
	NoulSpeculative                    NoulID = "speculative"
	NoulActionable                     NoulID = "actionable"
	NoulMissingDecisionContext         NoulID = "missing_decision_context"
	NoulPossibleSecurityRisk           NoulID = "possible_security_risk"
	NoulPossibleCorrectnessRisk        NoulID = "possible_correctness_risk"
	NoulPossibleAuthorizationRisk      NoulID = "possible_authorization_risk"
	NoulPossiblePrivacyRisk            NoulID = "possible_privacy_risk"
	NoulPossibleDataLossRisk           NoulID = "possible_data_loss_risk"
	NoulPossibleOperationalRisk        NoulID = "possible_operational_risk"
)

var allNoulIDs = []NoulID{
	NoulGroundedInEvidence,
	NoulIntroducedOrMateriallyAffected,
	NoulRemediationRequiredForIntent,
	NoulAdjacentImprovement,
	NoulSpeculative,
	NoulActionable,
	NoulMissingDecisionContext,
	NoulPossibleSecurityRisk,
	NoulPossibleCorrectnessRisk,
	NoulPossibleAuthorizationRisk,
	NoulPossiblePrivacyRisk,
	NoulPossibleDataLossRisk,
	NoulPossibleOperationalRisk,
}

func (id NoulID) String() string { return string(id) }

// AllNoulIDs returns the frozen v1 order.
func AllNoulIDs() []NoulID { return append([]NoulID(nil), allNoulIDs...) }

type QuestionType string

const (
	QuestionTypeNoul   QuestionType = "noul"
	QuestionTypeChoice QuestionType = "choice"
	QuestionTypeScore  QuestionType = "score"
)

type ChoiceOption struct {
	Key      string `json:"key"`
	Criteria string `json:"criteria"`
}

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

type SourceKind string

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

type SourceRef struct {
	SourceID string     `json:"source_id"`
	Kind     SourceKind `json:"kind"`
	Revision *string    `json:"revision"`
	Digest   Digest     `json:"digest"`
	URI      *string    `json:"uri"`
}

type Location struct {
	Path      string `json:"path"`
	Side      string `json:"side"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

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

type Intent struct {
	Text             *string          `json:"text"`
	SourceRefs       []SourceRef      `json:"source_refs"`
	ExplicitNonGoals []ScopeStatement `json:"explicit_non_goals"`
	Unresolved       []string         `json:"unresolved"`
}

type ScopeStatement struct {
	Text      string    `json:"text"`
	SourceRef SourceRef `json:"source_ref"`
}

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

type PullRequestState struct {
	ID           string   `json:"id"`
	RepositoryID string   `json:"repository_id"`
	BaseSHA      string   `json:"base_sha"`
	HeadSHA      string   `json:"head_sha"`
	Title        string   `json:"title"`
	Intent       Intent   `json:"intent"`
	Changes      []Change `json:"changes"`
}

type EvidenceAvailability string

const (
	EvidencePresent EvidenceAvailability = "present"
	EvidenceMissing EvidenceAvailability = "missing"
	EvidenceOmitted EvidenceAvailability = "omitted"
)

type EvidenceKind string

const (
	EvidenceCode          EvidenceKind = "code"
	EvidenceDiff          EvidenceKind = "diff"
	EvidenceTest          EvidenceKind = "test"
	EvidenceRequirement   EvidenceKind = "requirement"
	EvidenceCaller        EvidenceKind = "caller"
	EvidenceConfiguration EvidenceKind = "configuration"
	EvidenceDocumentation EvidenceKind = "documentation"
)

type EvidenceItem struct {
	ID            string               `json:"id"`
	Kind          EvidenceKind         `json:"kind"`
	Content       string               `json:"content"`
	SourceRef     SourceRef            `json:"source_ref"`
	Location      *Location            `json:"location"`
	Availability  EvidenceAvailability `json:"availability"`
	LimitationIDs []string             `json:"limitation_ids"`
}

type EvidenceRelationKind string

const (
	RelationCalls       EvidenceRelationKind = "calls"
	RelationImplements  EvidenceRelationKind = "implements"
	RelationTests       EvidenceRelationKind = "tests"
	RelationConfigures  EvidenceRelationKind = "configures"
	RelationContradicts EvidenceRelationKind = "contradicts"
	RelationSupports    EvidenceRelationKind = "supports"
)

type EvidenceRelation struct {
	FromID    string               `json:"from_id"`
	ToID      string               `json:"to_id"`
	Kind      EvidenceRelationKind `json:"kind"`
	SourceRef SourceRef            `json:"source_ref"`
}

type EvidenceState struct {
	Items     []EvidenceItem     `json:"items"`
	Relations []EvidenceRelation `json:"relations"`
}

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

type RelatedFindingsState struct {
	CandidateSetID      string           `json:"candidate_set_id"`
	ConstructionVersion string           `json:"construction_version"`
	CompleteForRule     bool             `json:"complete_for_rule"`
	CandidateIDs        []string         `json:"candidate_ids"`
	Items               []RelatedFinding `json:"items"`
}

type PolicyState struct {
	StateSchemaVersion   string   `json:"state_schema_version"`
	RubricVersion        string   `json:"rubric_version"`
	IntentRule           string   `json:"intent_rule"`
	ProtectedDomains     []string `json:"protected_domains"`
	SeverityRule         string   `json:"severity_rule"`
	DuplicationRule      string   `json:"duplication_rule"`
	UntrustedContentRule string   `json:"untrusted_content_rule"`
}

type LimitationCode string

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

type LimitationImpact string

const (
	ImpactDecisionRelevant LimitationImpact = "decision_relevant"
	ImpactIrrelevant       LimitationImpact = "irrelevant"
	ImpactUnknown          LimitationImpact = "unknown"
)

type Limitation struct {
	ID            string           `json:"id"`
	Code          LimitationCode   `json:"code"`
	AffectedPaths []string         `json:"affected_paths"`
	Description   string           `json:"description"`
	Impact        LimitationImpact `json:"impact"`
	ImpactSource  *SourceRef       `json:"impact_source"`
}

type Freshness string

const (
	FreshnessCurrent Freshness = "current"
	FreshnessStale   Freshness = "stale"
	FreshnessUnknown Freshness = "unknown"
)

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

type FindingSource struct {
	FindingID       review.FindingID `json:"finding_id"`
	ReviewerID      string           `json:"reviewer_id"`
	SourceOrdinal   int              `json:"source_ordinal"`
	TaskID          string           `json:"task_id"`
	TaskFingerprint string           `json:"task_fingerprint"`
	OutputDigest    string           `json:"output_digest"`
}

type ChangeSource struct {
	ID       string `json:"id"`
	OldPath  string `json:"old_path"`
	NewPath  string `json:"new_path"`
	Kind     string `json:"kind"`
	Patch    string `json:"patch"`
	Complete bool   `json:"complete"`
}

type SourceArtifact struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	Digest       string `json:"digest"`
	Bytes        []byte `json:"bytes"`
}

type Snapshot struct {
	RunID           string           `json:"run_id"`
	PR              gitprovider.PR   `json:"pr"`
	Findings        []review.Finding `json:"findings"`
	FindingSources  []FindingSource  `json:"finding_sources"`
	Changes         []ChangeSource   `json:"changes"`
	SourceArtifacts []SourceArtifact `json:"source_artifacts"`
	ArtifactRoot    string           `json:"artifact_root"`
}

type ProtectedDomain string

const (
	ProtectedSecurity      ProtectedDomain = "security"
	ProtectedCorrectness   ProtectedDomain = "correctness"
	ProtectedAuthorization ProtectedDomain = "authorization"
	ProtectedPrivacy       ProtectedDomain = "privacy"
	ProtectedDataLoss      ProtectedDomain = "data_loss"
	ProtectedOperational   ProtectedDomain = "operational"
)

type ProtectionStatus string

const (
	ProtectionProtected      ProtectionStatus = "protected"
	ProtectionNotEstablished ProtectionStatus = "not_established"
	ProtectionUnknown        ProtectionStatus = "unknown"
)

type ProtectionEvidence struct {
	Domain       ProtectedDomain `json:"domain"`
	SourceKind   string          `json:"source_kind"`
	SourceID     string          `json:"source_id"`
	SourceDigest Digest          `json:"source_digest"`
	Detail       string          `json:"detail"`
}

type Protection struct {
	Status   ProtectionStatus     `json:"status"`
	Domains  []ProtectedDomain    `json:"domains"`
	Evidence []ProtectionEvidence `json:"evidence"`
}

type AuthorityKind string

const (
	AuthorityHumanAttestation      AuthorityKind = "human_attestation"
	AuthorityApprovedDeterministic AuthorityKind = "approved_deterministic_rule"
	AuthorityNone                  AuthorityKind = "none"
)

type EligibilityStatus string

const (
	EligibilityEligible   EligibilityStatus = "eligible"
	EligibilityIneligible EligibilityStatus = "ineligible"
	EligibilityUnknown    EligibilityStatus = "unknown"
)

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

type VendorDataClass string

const (
	DataClassPublic    VendorDataClass = "public"
	DataClassSynthetic VendorDataClass = "synthetic"
	DataClassPrivate   VendorDataClass = "private"
)

type VendorPermission struct {
	DataClass        VendorDataClass `json:"data_class"`
	PermissionID     *string         `json:"permission_id"`
	PermissionDigest *Digest         `json:"permission_digest"`
	Permitted        bool            `json:"permitted"`
}

type Execution struct {
	RequestedModel        *string    `json:"requested_model"`
	AllowedResolvedModels []string   `json:"allowed_resolved_models"`
	ProtocolVersion       *string    `json:"protocol_version"`
	ClientVersion         *string    `json:"client_version"`
	DeadlineMS            *int       `json:"deadline_ms"`
	StartedAt             *time.Time `json:"started_at"`
}

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

type NumericTolerance struct {
	ProbabilitySum float64 `json:"probability_sum"`
	ScoreMean      float64 `json:"score_mean"`
}

type NoulBand struct {
	FalseRetain   float64                 `json:"false_retain"`
	TrueRetain    float64                 `json:"true_retain"`
	FalseSuppress map[Disposition]float64 `json:"false_suppress"`
	TrueSuppress  map[Disposition]float64 `json:"true_suppress"`
}

type ChoiceGate struct {
	Confidence          float64 `json:"confidence"`
	SelectedProbability float64 `json:"selected_probability"`
}

type ChoiceGates struct {
	Primary                 PrimaryChoiceGates   `json:"primary"`
	DuplicateRepresentative DuplicateChoiceGates `json:"duplicate_representative"`
}

type PrimaryChoiceGates struct {
	Retain         ChoiceGate `json:"retain"`
	LowValue       ChoiceGate `json:"low_value"`
	ScopeExpansion ChoiceGate `json:"scope_expansion"`
	Duplicate      ChoiceGate `json:"duplicate"`
}

type DuplicateChoiceGates struct {
	Retain   ChoiceGate `json:"retain"`
	Suppress ChoiceGate `json:"suppress"`
}

type ScoreGate struct {
	ConfidenceRetain   float64 `json:"confidence_retain"`
	ConfidenceSuppress float64 `json:"confidence_suppress"`
	LowMassSuppress    float64 `json:"low_mass_suppress"`
	HighMassRetain     float64 `json:"high_mass_retain"`
}

type StabilityParameters struct {
	Required bool `json:"required"`
}

type ThresholdSet struct {
	ID                        string              `json:"id"`
	Version                   string              `json:"version"`
	CalibrationManifestDigest Digest              `json:"calibration_manifest_digest"`
	RubricDigest              Digest              `json:"rubric_digest"`
	PolicyDigest              Digest              `json:"policy_digest"`
	QuestionsDigest           Digest              `json:"questions_digest"`
	ModelConditionDigest      Digest              `json:"model_condition_digest"`
	BoundProfileDigest        Digest              `json:"bound_profile_digest"`
	EligibilityRuleDigest     Digest              `json:"eligibility_rule_digest"`
	IncludeUtilityScore       bool                `json:"include_utility_score"`
	NoulBands                 map[NoulID]NoulBand `json:"noul_bands"`
	ChoiceGates               ChoiceGates         `json:"choice_gates"`
	ScoreGate                 ScoreGate           `json:"score_gate"`
	StabilityParameters       StabilityParameters `json:"stability_parameters"`
	Weights                   map[string]float64  `json:"weights"`
	ApprovalIDs               []string            `json:"approval_ids"`
	ArtifactDigest            Digest              `json:"artifact_digest"`
	CalibrationVersion        string              `json:"calibration_version"`
	TestOnly                  bool                `json:"test_only"`
}

type AnswerStatus string

const (
	AnswerStatusPresent AnswerStatus = "present"
	AnswerStatusMissing AnswerStatus = "missing"
	AnswerStatusInvalid AnswerStatus = "invalid"
)

type NoulAnswer struct {
	PTrue   float64 `json:"p_true"`
	present bool
}

// UnmarshalJSON keeps the typed contract strict at the JSON boundary. A
// missing or null p_true must not silently become the valid probability zero.
func (answer *NoulAnswer) UnmarshalJSON(data []byte) error {
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
			return fmt.Errorf("findingutility: trailing Noul JSON")
		}
		return err
	}
	if value.PTrue == nil {
		return fmt.Errorf("findingutility: Noul p_true is required and cannot be null")
	}
	answer.PTrue = *value.PTrue
	answer.present = true
	return nil
}

type ChoiceAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type ScoreAnswer struct {
	Score         float64   `json:"score"`
	Legend        []string  `json:"legend"`
	Probabilities []float64 `json:"probabilities"`
	Confidence    float64   `json:"confidence"`
}

type Answer struct {
	Type   QuestionType  `json:"type"`
	Noul   *NoulAnswer   `json:"noul,omitempty"`
	Choice *ChoiceAnswer `json:"choice,omitempty"`
	Score  *ScoreAnswer  `json:"score,omitempty"`
	Status AnswerStatus  `json:"status,omitempty"`
}

type AnswerSet map[string]Answer

type EvaluationResponse struct {
	SchemaVersion  int       `json:"schema_version"`
	RequestedModel string    `json:"requested_model"`
	ResolvedModel  string    `json:"resolved_model"`
	RequestID      string    `json:"request_id"`
	Answers        AnswerSet `json:"answers"`
	decoded        bool
}

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

type ReasonResult string

const (
	ReasonPass         ReasonResult = "pass"
	ReasonVeto         ReasonResult = "veto"
	ReasonUncertain    ReasonResult = "uncertain"
	ReasonNotEvaluated ReasonResult = "not_evaluated"
)

type ReasonTrace struct {
	RuleID         string         `json:"rule_id"`
	InputPaths     []string       `json:"input_paths"`
	ObservedValues map[string]any `json:"observed_values"`
	ThresholdPaths []string       `json:"threshold_paths"`
	Result         ReasonResult   `json:"result"`
	Decision       *Disposition   `json:"decision"`
}

type CandidateStatus string

const (
	CandidateAvailable   CandidateStatus = "available"
	CandidateUnavailable CandidateStatus = "unavailable"
)

type Decision struct {
	ProposedDecision       Disposition     `json:"proposed_decision"`
	EffectiveDecision      Disposition     `json:"effective_decision"`
	ReasonCodes            []string        `json:"reason_codes"`
	ReasonTrace            []ReasonTrace   `json:"reason_trace"`
	CandidateStatus        CandidateStatus `json:"candidate_status"`
	ModelCandidateDecision *Disposition    `json:"model_candidate_decision"`
}

type EvaluatorStatus string

const (
	EvaluatorNotRequested      EvaluatorStatus = "not_requested"
	EvaluatorSkippedPermission EvaluatorStatus = "skipped_permission"
	EvaluatorSkippedConfig     EvaluatorStatus = "skipped_configuration"
	EvaluatorSucceeded         EvaluatorStatus = "succeeded"
	EvaluatorTimeout           EvaluatorStatus = "timeout"
	EvaluatorCancelled         EvaluatorStatus = "cancelled"
	EvaluatorTransportError    EvaluatorStatus = "transport_error"
	EvaluatorProviderError     EvaluatorStatus = "provider_error"
	EvaluatorInvalidResponse   EvaluatorStatus = "invalid_response"
	EvaluatorStaleResult       EvaluatorStatus = "stale_result"
)

type AuditStatus string

const (
	AuditStatusComplete AuditStatus = "complete"
	AuditStatusDegraded AuditStatus = "degraded"
	AuditStatusFailed   AuditStatus = "failed"
)

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

type Latency struct {
	QueueMS    *int64 `json:"queue_ms"`
	ProviderMS *int64 `json:"provider_ms"`
	AuditMS    *int64 `json:"audit_ms"`
	TotalMS    *int64 `json:"total_ms"`
	DeadlineMS *int   `json:"deadline_ms"`
}

type RawResponseArtifact struct {
	RelativePath string `json:"relative_path"`
	Digest       Digest `json:"digest"`
	Bytes        int64  `json:"bytes"`
}

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

type StabilityReceipt struct {
	Digest           Digest `json:"digest"`
	Successful       bool   `json:"successful"`
	ProtectionSignal bool   `json:"protection_signal"`
	Unstable         bool   `json:"unstable"`
}

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

type AdapterFactory func(Invocation) (llm.Adapter, error)

type Invocation struct {
	Version                string            `json:"version"`
	Request                EvaluationRequest `json:"request"`
	InputFingerprint       Digest            `json:"input_fingerprint"`
	Prompt                 string            `json:"prompt"`
	ExpectedResolvedModels []string          `json:"expected_resolved_models"`
	FixtureDigest          Digest            `json:"fixture_digest"`
	DataClass              VendorDataClass   `json:"data_class"`
}

type EvaluationRequest struct {
	ProtocolVersion string      `json:"protocol_version"`
	Model           string      `json:"model"`
	State           State       `json:"state"`
	Questions       QuestionSet `json:"questions"`
}

type Warning struct {
	Code      string           `json:"code"`
	FindingID review.FindingID `json:"finding_id"`
	Message   string           `json:"message"`
}

type Options struct {
	Profile      DevelopmentProfile
	NewAdapter   AdapterFactory
	ResolveModel func(string) (string, error)
	Now          func() time.Time
	NewAttemptID func() string
	Warn         func(Warning)
}

type Outcome struct {
	AuditPath   string             `json:"audit_path"`
	AuditStatus AuditStatus        `json:"audit_status"`
	Records     []EvaluationRecord `json:"records"`
	Warnings    []Warning          `json:"warnings"`
}

type AuditFile struct {
	RelativePath string `json:"relative_path"`
	Digest       Digest `json:"digest"`
	Bytes        int64  `json:"bytes"`
}

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

type VerificationInputs struct {
	Snapshot             Snapshot
	Files                map[string][]byte
	Rubric               Rubric
	Profile              DevelopmentProfile
	ExpectedCohortDigest string
}

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
