package findingutility

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	artifactSourceFile      = "source.json"
	artifactManifestFile    = "manifest.json"
	artifactRecordsFile     = "records.json"
	artifactRawFindingsFile = "raw-findings.json"
	artifactEffectiveFile   = "effective-findings.json"
)

type auditSourceArtifact struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	Digest       string `json:"digest"`
	Bytes        []byte `json:"bytes_base64"`
}

type auditSource struct {
	SchemaVersion   int                   `json:"schema_version"`
	RunID           string                `json:"run_id"`
	PR              gitprovider.PR        `json:"pr"`
	RawFindings     []review.Finding      `json:"raw_findings"`
	FindingSources  []FindingSource       `json:"finding_sources"`
	Changes         []ChangeSource        `json:"changes"`
	SourceArtifacts []auditSourceArtifact `json:"source_artifacts"`
}

type auditRecords struct {
	SchemaVersion     int                `json:"schema_version"`
	Mode              string             `json:"mode"`
	RunID             string             `json:"run_id"`
	CohortInputDigest Digest             `json:"cohort_input_digest"`
	RawFindingsDigest Digest             `json:"raw_findings_digest"`
	Records           []EvaluationRecord `json:"records"`
}

func validateNoCallRecord(record EvaluationRecord, runID string) error {
	if record.RecordSchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("record schema version is invalid")
	}
	if record.RecordID == "" {
		return fmt.Errorf("record ID is required")
	}
	if record.RunID != runID {
		return fmt.Errorf("record run ID does not match manifest")
	}
	if record.OriginalSeverity == "" || record.RubricVersion != RubricVersion || record.PolicyVersion != PolicyVersion {
		return fmt.Errorf("no-call record definition identity is incomplete")
	}
	for name, digest := range map[string]Digest{
		"raw finding":   record.RawFindingDigest,
		"raw findings":  record.RawFindingsDigest,
		"state":         record.StateDigest,
		"context":       record.ContextDigest,
		"questions":     record.QuestionsDigest,
		"rubric":        record.RubricDigest,
		"policy":        record.PolicyDigest,
		"bound profile": record.BoundProfileDigest,
	} {
		if !ValidDigest(digest) {
			return fmt.Errorf("no-call record %s digest is invalid", name)
		}
	}
	switch record.EvaluatorStatus {
	case "", EvaluatorNotRequested, EvaluatorSkippedPermission, EvaluatorSkippedConfig:
	case EvaluatorSucceeded, EvaluatorTimeout, EvaluatorCancelled, EvaluatorTransportError, EvaluatorProviderError, EvaluatorInvalidResponse, EvaluatorStaleResult:
		return fmt.Errorf("evaluated records are not supported by this audit writer")
	default:
		return fmt.Errorf("evaluated records are not supported by this audit writer")
	}
	if record.ProposedDecision != DispositionKeep || record.EffectiveDecision != DispositionKeep {
		return fmt.Errorf("no-call record must keep both proposed and effective decisions")
	}
	if record.CandidateStatus != "" && record.CandidateStatus != CandidateUnavailable {
		return fmt.Errorf("no-call record must have candidate_status unavailable")
	}
	if record.ModelCandidateDecision != nil || len(record.Answers) != 0 || record.RawResponseArtifact != nil {
		return fmt.Errorf("no-call record must not contain evaluator output")
	}
	if record.ResolvedModel != "" || record.ProviderRequestID != "" || record.InputFingerprint != "" || record.ResultFingerprint != "" || record.AttemptID != "" || record.RepeatID != "" || record.PerturbationID != "" {
		return fmt.Errorf("no-call record contains execution output")
	}
	if record.CacheSource != "" && record.CacheSource != "no_call" {
		return fmt.Errorf("no-call record has invalid cache source")
	}
	return nil
}

// validateVerificationInputs checks the independent identity required to
// write or verify an audit. The verifier must never silently fall back to
// checking only the serialized marker when the expected profile or snapshot
// is absent.
func validateVerificationInputs(inputs VerificationInputs, requireFiles bool) error {
	if err := validateSnapshot(inputs.Snapshot); err != nil {
		return fmt.Errorf("snapshot is incomplete: %w", err)
	}
	if err := validateRubric(inputs.Rubric); err != nil {
		return fmt.Errorf("rubric is incomplete: %w", err)
	}
	if err := validateFixtureProfile(inputs.Profile); err != nil {
		return fmt.Errorf("profile is incomplete: %w", err)
	}
	if !ValidDigest(inputs.Profile.BoundProfile.Digest) || inputs.Profile.BoundProfile.Digest != boundProfileDigest(inputs.Profile.BoundProfile) {
		return fmt.Errorf("profile bound digest is invalid")
	}
	if !ValidDigest(inputs.Profile.BoundProfile.SelectionRuleDigest) {
		return fmt.Errorf("profile selection-rule digest is invalid")
	}
	if !ValidDigest(Digest(inputs.ExpectedCohortDigest)) {
		return fmt.Errorf("expected cohort digest is required")
	}
	if requireFiles && inputs.Files == nil {
		return fmt.Errorf("audit files are required")
	}
	// BuildStates is intentionally tied to the embedded v1 rubric. Reject a
	// structurally valid but different rubric before an empty cohort could skip
	// per-record identity reconstruction.
	expectedRubric, err := LoadRubric()
	if err != nil {
		return fmt.Errorf("load embedded rubric: %w", err)
	}
	expectedRubricDigest, err := DigestCanonical(expectedRubric)
	if err != nil {
		return fmt.Errorf("digest embedded rubric: %w", err)
	}
	actualRubricDigest, err := DigestCanonical(inputs.Rubric)
	if err != nil {
		return fmt.Errorf("digest expected rubric: %w", err)
	}
	if actualRubricDigest != expectedRubricDigest {
		return fmt.Errorf("expected rubric does not match embedded rubric")
	}
	return nil
}

func sameSnapshotIdentity(left, right Snapshot) bool {
	return left.RunID == right.RunID &&
		CanonicalValueEqual(left.PR, right.PR) &&
		sameCanonicalList(left.Findings, right.Findings) &&
		sameCanonicalList(left.FindingSources, right.FindingSources) &&
		sameCanonicalList(left.Changes, right.Changes) &&
		sameCanonicalList(left.SourceArtifacts, right.SourceArtifacts)
}

// validateAuditIdentity rebuilds the state/question/control identity from the
// independent snapshot and profile, then checks every persisted no-call keep
// record against it. In particular, revisions are part of the record identity
// even when they are empty because the source snapshot is unresolved.
func validateAuditIdentity(inputs VerificationInputs, raw []review.Finding, records []EvaluationRecord) error {
	if len(raw) != len(records) {
		return fmt.Errorf("audit records do not cover raw findings")
	}
	states, controls, err := BuildStates(inputs.Snapshot, inputs.Profile.BoundProfile)
	if err != nil {
		return fmt.Errorf("rebuild expected state: %w", err)
	}
	if len(states) != len(records) || len(controls) != len(records) {
		return fmt.Errorf("reconstructed identity coverage does not match records")
	}
	rawDigest, err := DigestCanonical(raw)
	if err != nil {
		return fmt.Errorf("digest raw findings: %w", err)
	}
	expectedRubricDigest, err := DigestCanonical(inputs.Rubric)
	if err != nil {
		return fmt.Errorf("digest expected rubric: %w", err)
	}
	for index, record := range records {
		if err := validateNoCallRecord(record, inputs.Snapshot.RunID); err != nil {
			return fmt.Errorf("record %d: %w", index, err)
		}
		findingDigest, err := DigestCanonical(raw[index])
		if err != nil {
			return fmt.Errorf("digest finding %d: %w", index, err)
		}
		control := controls[index]
		if record.FindingID != raw[index].ID || record.SourceOrdinal != index || record.OriginalSeverity != control.OriginalSeverity || record.RawFindingDigest != findingDigest || record.RawFindingsDigest != rawDigest || record.StateDigest != control.StateDigest || record.ContextDigest != control.ContextDigest || record.QuestionsDigest != control.QuestionsDigest || record.RubricDigest != control.RubricDigest || record.PolicyDigest != control.PolicyDigest || record.BoundProfileDigest != control.BoundProfileDigest || record.RubricDigest != expectedRubricDigest || record.BaseSHA != control.BaseSHA || record.HeadSHA != control.HeadSHA {
			return fmt.Errorf("record %d state, revision, or definition identity mismatch", index)
		}
	}
	return nil
}

// RawFindingProjection is the immutable JSON projection used by both raw and
// effective audit files. It intentionally uses the existing review.Finding
// serialization and never adds policy fields to that type.
func RawFindingProjection(findings []review.Finding) ([]byte, error) {
	if findings == nil {
		findings = []review.Finding{}
	}
	if err := ValidateCanonicalStruct(findings); err != nil {
		return nil, err
	}
	return json.Marshal(findings)
}

// WriteAudit writes a run-owned audit bundle and commits its manifest last.
// The caller owns the run lock; this helper never starts a background writer.
func WriteAudit(root string, bundle AuditBundle, now time.Time) (Manifest, error) {
	if strings.TrimSpace(root) == "" {
		return Manifest{}, fmt.Errorf("findingutility: audit root is required")
	}
	verification := bundle.VerificationInputs
	if err := validateVerificationInputs(verification, false); err != nil {
		return Manifest{}, fmt.Errorf("findingutility: verification inputs: %w", err)
	}
	if !sameSnapshotIdentity(bundle.Snapshot, verification.Snapshot) {
		return Manifest{}, fmt.Errorf("findingutility: audit snapshot does not match verification inputs")
	}
	if len(bundle.RawFindings) != len(bundle.EffectiveFindings) || len(bundle.Records) != len(bundle.RawFindings) {
		return Manifest{}, fmt.Errorf("findingutility: audit records must cover every raw finding exactly once")
	}
	if bundle.Manifest.RunID == "" || bundle.Manifest.RunID != verification.Snapshot.RunID {
		return Manifest{}, fmt.Errorf("findingutility: manifest run ID is required")
	}
	if bundle.Manifest.CohortInputDigest == "" || string(bundle.Manifest.CohortInputDigest) != verification.ExpectedCohortDigest {
		return Manifest{}, fmt.Errorf("findingutility: manifest cohort digest does not match verification inputs")
	}
	if !sameCanonicalList(bundle.RawFindings, verification.Snapshot.Findings) {
		return Manifest{}, fmt.Errorf("findingutility: raw findings do not match verification inputs")
	}
	if err := validateAuditIdentity(verification, bundle.RawFindings, bundle.Records); err != nil {
		return Manifest{}, fmt.Errorf("findingutility: audit identity: %w", err)
	}
	if bundle.Snapshot.RunID != "" {
		if bundle.Snapshot.RunID != bundle.Manifest.RunID || !CanonicalValueEqual(bundle.Snapshot.Findings, bundle.RawFindings) {
			return Manifest{}, fmt.Errorf("findingutility: audit snapshot does not match raw findings")
		}
	}
	seenFindingIDs := make(map[review.FindingID]bool, len(bundle.RawFindings))
	for index, finding := range bundle.RawFindings {
		if finding.ID == "" || seenFindingIDs[finding.ID] {
			return Manifest{}, fmt.Errorf("findingutility: raw findings contain duplicate or empty ID")
		}
		seenFindingIDs[finding.ID] = true
		if bundle.Records[index].FindingID != finding.ID {
			return Manifest{}, fmt.Errorf("findingutility: record %d does not match raw finding %s", index, finding.ID)
		}
		if err := validateNoCallRecord(bundle.Records[index], bundle.Manifest.RunID); err != nil {
			return Manifest{}, fmt.Errorf("findingutility: record %d: %w", index, err)
		}
	}
	seenArtifacts := make(map[string]bool, len(bundle.Snapshot.SourceArtifacts))
	for _, artifact := range bundle.Snapshot.SourceArtifacts {
		if artifact.ID == "" || seenArtifacts[artifact.ID] || !validRelativePath(artifact.RelativePath) || !ValidDigest(Digest(artifact.Digest)) || (len(artifact.Bytes) > 0 && Digest(artifact.Digest) != DigestBytes(artifact.Bytes)) {
			return Manifest{}, fmt.Errorf("findingutility: source artifact %q is invalid", artifact.ID)
		}
		seenArtifacts[artifact.ID] = true
	}
	rawBytes, err := RawFindingProjection(bundle.RawFindings)
	if err != nil {
		return Manifest{}, fmt.Errorf("findingutility: encode raw findings: %w", err)
	}
	effectiveBytes, err := RawFindingProjection(bundle.EffectiveFindings)
	if err != nil {
		return Manifest{}, fmt.Errorf("findingutility: encode effective findings: %w", err)
	}
	if !bytes.Equal(rawBytes, effectiveBytes) {
		return Manifest{}, fmt.Errorf("findingutility: raw and effective projections differ")
	}
	rawDigest, err := DigestCanonical(append([]review.Finding{}, bundle.RawFindings...))
	if err != nil {
		return Manifest{}, err
	}
	recordsBytes, err := json.Marshal(auditRecords{
		SchemaVersion:     RecordSchemaVersion,
		Mode:              ModeAdvisory,
		RunID:             bundle.Manifest.RunID,
		CohortInputDigest: bundle.Manifest.CohortInputDigest,
		RawFindingsDigest: rawDigest,
		Records:           append([]EvaluationRecord{}, bundle.Records...),
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("findingutility: encode records: %w", err)
	}
	cohortDigest := bundle.Manifest.CohortInputDigest
	if !ValidDigest(cohortDigest) {
		return Manifest{}, fmt.Errorf("findingutility: manifest cohort digest is invalid")
	}
	cohortDir := filepath.Join(root, strings.TrimPrefix(string(cohortDigest), "sha256:"))
	if err := safeArtifactRoot(root, cohortDir); err != nil {
		return Manifest{}, err
	}
	if info, err := os.Lstat(cohortDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return Manifest{}, fmt.Errorf("findingutility: audit cohort path is not a regular directory")
		}
		entries, readErr := os.ReadDir(cohortDir)
		if readErr != nil {
			return Manifest{}, fmt.Errorf("findingutility: inspect existing audit cohort: %w", readErr)
		}
		if len(entries) != 0 {
			return Manifest{}, fmt.Errorf("findingutility: committed or incomplete audit cohort already exists")
		}
	} else if !os.IsNotExist(err) {
		return Manifest{}, fmt.Errorf("findingutility: inspect existing audit cohort: %w", err)
	}
	if err := os.MkdirAll(cohortDir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("findingutility: create audit directory: %w", err)
	}
	if err := safeArtifactRoot(root, cohortDir); err != nil {
		return Manifest{}, err
	}
	source := auditSource{
		SchemaVersion:   1,
		RunID:           bundle.Manifest.RunID,
		PR:              bundle.Snapshot.PR,
		RawFindings:     append([]review.Finding{}, bundle.RawFindings...),
		FindingSources:  append([]FindingSource{}, bundle.Snapshot.FindingSources...),
		Changes:         append([]ChangeSource{}, bundle.Snapshot.Changes...),
		SourceArtifacts: []auditSourceArtifact{},
	}
	for _, artifact := range bundle.Snapshot.SourceArtifacts {
		source.SourceArtifacts = append(source.SourceArtifacts, auditSourceArtifact{
			ID:           artifact.ID,
			RelativePath: artifact.RelativePath,
			Digest:       artifact.Digest,
			Bytes:        append([]byte(nil), artifact.Bytes...),
		})
	}
	sourceBytes, err := json.Marshal(source)
	if err != nil {
		return Manifest{}, err
	}
	files := map[string][]byte{
		artifactSourceFile:      sourceBytes,
		artifactRecordsFile:     recordsBytes,
		artifactRawFindingsFile: rawBytes,
		artifactEffectiveFile:   effectiveBytes,
	}
	manifest := bundle.Manifest
	manifest.SchemaVersion = ManifestSchemaVersion
	manifest.Mode = ModeAdvisory
	manifest.CohortInputDigest = cohortDigest
	manifest.FindingCount = len(bundle.RawFindings)
	manifest.RecordCount = len(bundle.Records)
	manifest.FixtureOnly = true
	if manifest.StartedAt.IsZero() {
		manifest.StartedAt = now.UTC()
	}
	manifest.CompletedAt = now.UTC()
	manifest.Status = AuditStatusComplete
	for _, record := range bundle.Records {
		if record.EffectiveDecision != DispositionKeep {
			return Manifest{}, fmt.Errorf("findingutility: effective decision for %s is not keep", record.FindingID)
		}
		if record.AuditStatus != AuditStatusComplete {
			manifest.Status = AuditStatusDegraded
		}
	}
	manifest.Files = make([]AuditFile, 0, len(files))
	for _, name := range []string{artifactSourceFile, artifactRecordsFile, artifactRawFindingsFile, artifactEffectiveFile} {
		data := files[name]
		path := filepath.Join(cohortDir, name)
		if err := fsatomic.WriteFileAtomic(path, data, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("findingutility: write %s: %w", name, err)
		}
		manifest.Files = append(manifest.Files, AuditFile{RelativePath: name, Digest: DigestBytes(data), Bytes: int64(len(data))})
	}
	manifest.InputManifestDigest, err = DigestCanonical(map[string]any{"raw_findings_digest": rawDigest, "cohort_input_digest": cohortDigest, "files": manifest.Files})
	if err != nil {
		return Manifest{}, err
	}
	manifest.RecordDigest = DigestBytes(recordsBytes)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, err
	}
	verification.Files = make(map[string][]byte, len(files))
	for name, data := range files {
		verification.Files[name] = append([]byte(nil), data...)
	}
	if err := safeArtifactRoot(root, cohortDir); err != nil {
		return Manifest{}, err
	}
	if err := VerifyArtifact(manifestBytes, verification); err != nil {
		return Manifest{}, fmt.Errorf("findingutility: verify audit before commit: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(filepath.Join(cohortDir, artifactManifestFile), manifestBytes, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("findingutility: commit audit manifest: %w", err)
	}
	return manifest, nil
}

// VerifyArtifact verifies a committed manifest and the already-read files
// beneath it. It never performs I/O: callers must first resolve and validate
// the contained artifact directory and pass exact manifest-relative bytes.
func VerifyArtifact(data []byte, inputs VerificationInputs) error {
	if err := validateVerificationInputs(inputs, true); err != nil {
		return fmt.Errorf("findingutility: verification inputs: %w", err)
	}
	var manifest Manifest
	if err := DecodeStrict(data, &manifest); err != nil {
		return fmt.Errorf("findingutility: decode manifest: %w", err)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.Mode != ModeAdvisory || !manifest.FixtureOnly || (manifest.Status != AuditStatusComplete && manifest.Status != AuditStatusDegraded && manifest.Status != AuditStatusFailed) {
		return fmt.Errorf("findingutility: unsupported audit manifest")
	}
	if inputs.Snapshot.RunID != "" && manifest.RunID != inputs.Snapshot.RunID {
		return fmt.Errorf("findingutility: audit run ID mismatch")
	}
	if inputs.ExpectedCohortDigest != "" && string(manifest.CohortInputDigest) != inputs.ExpectedCohortDigest {
		return fmt.Errorf("findingutility: audit cohort digest mismatch")
	}
	if !ValidDigest(manifest.CohortInputDigest) || !ValidDigest(manifest.InputManifestDigest) || !ValidDigest(manifest.RecordDigest) {
		return fmt.Errorf("findingutility: audit manifest digest is invalid")
	}
	if manifest.FindingCount != manifest.RecordCount || manifest.RecordCount < 0 {
		return fmt.Errorf("findingutility: audit counts do not agree")
	}
	files := make(map[string][]byte, len(manifest.Files))
	seen := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		if file.RelativePath == artifactManifestFile || strings.Contains(file.RelativePath, "..") || filepath.IsAbs(file.RelativePath) || filepath.Base(file.RelativePath) != file.RelativePath || seen[file.RelativePath] {
			return fmt.Errorf("findingutility: invalid manifest file path %q", file.RelativePath)
		}
		seen[file.RelativePath] = true
		content, ok := inputs.Files[file.RelativePath]
		if !ok {
			return fmt.Errorf("findingutility: missing audit file %q", file.RelativePath)
		}
		if int64(len(content)) != file.Bytes || DigestBytes(content) != file.Digest {
			return fmt.Errorf("findingutility: audit file %q digest or size mismatch", file.RelativePath)
		}
		files[file.RelativePath] = append([]byte(nil), content...)
	}
	for name := range inputs.Files {
		if !seen[name] {
			return fmt.Errorf("findingutility: unlisted audit file %q", name)
		}
	}
	for _, required := range []string{artifactSourceFile, artifactRecordsFile, artifactRawFindingsFile, artifactEffectiveFile} {
		if _, ok := files[required]; !ok {
			return fmt.Errorf("findingutility: required audit file %q is absent", required)
		}
	}
	var source auditSource
	if err := DecodeStrict(files[artifactSourceFile], &source); err != nil || source.SchemaVersion != 1 || source.RunID != manifest.RunID {
		return fmt.Errorf("findingutility: source manifest is invalid")
	}
	var recordEnvelope auditRecords
	if err := DecodeStrict(files[artifactRecordsFile], &recordEnvelope); err != nil {
		return fmt.Errorf("findingutility: decode records: %w", err)
	}
	if recordEnvelope.SchemaVersion != RecordSchemaVersion || recordEnvelope.Mode != ModeAdvisory || recordEnvelope.RunID != manifest.RunID || recordEnvelope.CohortInputDigest != manifest.CohortInputDigest || !ValidDigest(recordEnvelope.RawFindingsDigest) {
		return fmt.Errorf("findingutility: records envelope is invalid")
	}
	records := recordEnvelope.Records
	for index, record := range records {
		if err := validateNoCallRecord(record, manifest.RunID); err != nil {
			return fmt.Errorf("findingutility: record %d: %w", index, err)
		}
	}
	var raw, effective []review.Finding
	if err := DecodeStrict(files[artifactRawFindingsFile], &raw); err != nil {
		return fmt.Errorf("findingutility: decode raw findings: %w", err)
	}
	if err := DecodeStrict(files[artifactEffectiveFile], &effective); err != nil {
		return fmt.Errorf("findingutility: decode effective findings: %w", err)
	}
	if len(raw) != manifest.FindingCount || len(effective) != len(raw) || len(records) != len(raw) || !bytes.Equal(files[artifactRawFindingsFile], files[artifactEffectiveFile]) {
		return fmt.Errorf("findingutility: raw/effective/record coverage mismatch")
	}
	rawDigest, err := DigestCanonical(raw)
	if err != nil {
		return err
	}
	if recordEnvelope.RawFindingsDigest != rawDigest {
		return fmt.Errorf("findingutility: records raw finding digest mismatch")
	}
	if len(source.RawFindings) != len(raw) {
		return fmt.Errorf("findingutility: source raw finding coverage mismatch")
	}
	sourceRaw, err := RawFindingProjection(source.RawFindings)
	if err != nil {
		return fmt.Errorf("findingutility: source raw finding projection: %w", err)
	}
	if !bytes.Equal(sourceRaw, files[artifactRawFindingsFile]) {
		return fmt.Errorf("findingutility: source raw finding projection mismatch")
	}
	if inputs.Snapshot.RunID != "" {
		if source.RunID != inputs.Snapshot.RunID || !CanonicalValueEqual(source.PR, inputs.Snapshot.PR) || !sameCanonicalList(source.FindingSources, inputs.Snapshot.FindingSources) || !sameCanonicalList(source.Changes, inputs.Snapshot.Changes) {
			return fmt.Errorf("findingutility: source snapshot mismatch")
		}
		sourceArtifacts := make([]SourceArtifact, 0, len(source.SourceArtifacts))
		for _, artifact := range source.SourceArtifacts {
			if !validRelativePath(artifact.RelativePath) || !ValidDigest(Digest(artifact.Digest)) || (len(artifact.Bytes) > 0 && Digest(artifact.Digest) != DigestBytes(artifact.Bytes)) {
				return fmt.Errorf("findingutility: source artifact %q is invalid", artifact.ID)
			}
			sourceArtifacts = append(sourceArtifacts, SourceArtifact{ID: artifact.ID, RelativePath: artifact.RelativePath, Digest: artifact.Digest, Bytes: append([]byte(nil), artifact.Bytes...)})
		}
		if !sameCanonicalList(sourceArtifacts, inputs.Snapshot.SourceArtifacts) {
			return fmt.Errorf("findingutility: source artifact snapshot mismatch")
		}
	}
	expectedRaw, err := RawFindingProjection(inputs.Snapshot.Findings)
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedRaw, files[artifactRawFindingsFile]) {
		return fmt.Errorf("findingutility: snapshot raw finding projection mismatch")
	}
	if err := validateAuditIdentity(inputs, raw, records); err != nil {
		return fmt.Errorf("findingutility: audit identity: %w", err)
	}
	if DigestBytes(files[artifactRecordsFile]) != manifest.RecordDigest {
		return fmt.Errorf("findingutility: record digest mismatch")
	}
	inputManifestDigest, err := DigestCanonical(map[string]any{"raw_findings_digest": rawDigest, "cohort_input_digest": manifest.CohortInputDigest, "files": manifest.Files})
	if err != nil || inputManifestDigest != manifest.InputManifestDigest {
		return fmt.Errorf("findingutility: input manifest digest mismatch")
	}
	byID := make(map[string]bool, len(raw))
	for index, finding := range raw {
		byID[finding.ID.String()] = true
		if records[index].FindingID != finding.ID || records[index].EffectiveDecision != DispositionKeep || records[index].RawFindingsDigest != rawDigest {
			return fmt.Errorf("findingutility: record %d does not preserve advisory finding %s", index, finding.ID)
		}
	}
	for _, record := range records {
		if !byID[record.FindingID.String()] {
			return fmt.Errorf("findingutility: record references unknown finding %s", record.FindingID)
		}
	}
	return nil
}

func safeArtifactRoot(root, child string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absChild)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("findingutility: artifact path escapes root")
	}
	if info, statErr := os.Lstat(absRoot); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("findingutility: artifact root is a symlink")
		}
		if !info.IsDir() {
			return fmt.Errorf("findingutility: artifact root is not a directory")
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("findingutility: inspect artifact root: %w", statErr)
	}
	if err := rejectSymlinkAncestors(absRoot); err != nil {
		return fmt.Errorf("findingutility: unsafe artifact root ancestor: %w", err)
	}
	if err := rejectSymlinkDescendants(absRoot, absChild); err != nil {
		return fmt.Errorf("findingutility: unsafe artifact root: %w", err)
	}
	return nil
}

// rejectSymlinkAncestors walks every existing component above and including
// the owned root. A missing root is allowed so MkdirAll can create it, but a
// pre-existing symlink in its parent chain is not: lexical containment alone
// would otherwise permit the cohort to be redirected outside the owner.
func rejectSymlinkAncestors(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	var components []string
	for current := absPath; ; current = filepath.Dir(current) {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	for index := len(components) - 1; index >= 0; index-- {
		current := components[index]
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 && !trustedSystemAncestorSymlink(current) {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

// macOS exposes /var and /tmp as stable aliases to /private/var and
// /private/tmp. They are outside the caller-owned root and are safe only when
// they resolve to those exact system targets; arbitrary symlinked ancestors
// remain rejected.
func trustedSystemAncestorSymlink(path string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	clean := filepath.Clean(path)
	if clean != "/var" && clean != "/tmp" {
		return false
	}
	target, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return false
	}
	return target == filepath.Join("/private", clean)
}

func rejectSymlinkDescendants(root, child string) error {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return err
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

func sameCanonicalList(left, right any) bool {
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.IsValid() && rightValue.IsValid() && leftValue.Kind() == reflect.Slice && rightValue.Kind() == reflect.Slice && leftValue.Len() == 0 && rightValue.Len() == 0 {
		return true
	}
	return CanonicalValueEqual(left, right)
}
