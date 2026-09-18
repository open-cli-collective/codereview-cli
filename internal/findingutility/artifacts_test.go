package findingutility

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestWriteAndVerifyAuditPreservesRawAndEffectiveProjection(t *testing.T) {
	snapshot := testSnapshot()
	cohortDigest := DigestBytes([]byte("cohort-input"))
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]EvaluationRecord, 0, len(snapshot.Findings))
	for index := range snapshot.Findings {
		record := testAuditRecord(t, snapshot, index, controls[index])
		record.AuditStatus = AuditStatusComplete
		records = append(records, record)
	}
	bundle := AuditBundle{
		Snapshot: snapshot,
		Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohortDigest},
		Records:  records, RawFindings: snapshot.Findings, EffectiveFindings: append([]review.Finding(nil), snapshot.Findings...),
		VerificationInputs: testVerificationInputs(t, snapshot, cohortDigest),
	}
	root := t.TempDir()
	manifest, err := WriteAudit(root, bundle, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != AuditStatusComplete || manifest.FindingCount != len(snapshot.Findings) || len(manifest.Files) != 4 {
		t.Fatalf("manifest = %#v", manifest)
	}
	cohortDir := filepath.Join(root, string(cohortDigest)[len("sha256:"):])
	manifestBytes, err := os.ReadFile(filepath.Join(cohortDir, artifactManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range manifest.Files {
		content, err := os.ReadFile(filepath.Join(cohortDir, file.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		files[file.RelativePath] = content
	}
	if err := VerifyArtifact(manifestBytes, VerificationInputs{Snapshot: snapshot, Files: files, Rubric: DefaultRubric(), Profile: profile, ExpectedCohortDigest: string(cohortDigest)}); err != nil {
		t.Fatalf("VerifyArtifact rejected committed bundle: %v", err)
	}
	if !bytes.Equal(files[artifactRawFindingsFile], files[artifactEffectiveFile]) {
		t.Fatal("raw/effective files are not byte-identical")
	}
}

func TestVerifyArtifactRejectsTamperedOrNonAdvisoryFiles(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]EvaluationRecord, len(snapshot.Findings))
	for index := range snapshot.Findings {
		records[index] = testAuditRecord(t, snapshot, index, controls[index])
	}
	cohort := DigestBytes([]byte("tamper-cohort"))
	manifest, err := WriteAudit(t.TempDir(), AuditBundle{Snapshot: snapshot, Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohort}, Records: records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings, VerificationInputs: testVerificationInputs(t, snapshot, cohort)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Mode != ModeAdvisory {
		t.Fatal("audit mode was not advisory")
	}
	if err := VerifyArtifact([]byte(`{"schema_version":1,"mode":"review"}`), VerificationInputs{}); err == nil {
		t.Fatal("non-advisory manifest must fail")
	}
}

func TestWriteAuditRejectsEvaluatedOrMalformedRecordsBeforeWriting(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Findings = snapshot.Findings[:1]
	rawDigest, err := DigestCanonical(snapshot.Findings)
	if err != nil {
		t.Fatal(err)
	}
	findingDigest, err := DigestCanonical(snapshot.Findings[0])
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	record := testAuditRecord(t, snapshot, 0, controls[0])
	record.RawFindingDigest = findingDigest
	record.RawFindingsDigest = rawDigest
	record.EvaluatorStatus = EvaluatorSucceeded
	record.Answers = AnswerSet{"unexpected": {Type: QuestionTypeNoul, Status: AnswerStatusPresent, Noul: &NoulAnswer{PTrue: 0}}}
	cohortDigest := DigestBytes([]byte("pre-write-reject"))
	root := t.TempDir()
	_, err = WriteAudit(root, AuditBundle{Snapshot: snapshot, Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohortDigest}, Records: []EvaluationRecord{record}, RawFindings: snapshot.Findings[:1], EffectiveFindings: snapshot.Findings[:1], VerificationInputs: testVerificationInputs(t, snapshot, cohortDigest)}, time.Unix(10, 0))
	if err == nil {
		t.Fatal("evaluated/malformed audit record unexpectedly committed")
	}
	if _, statErr := os.Stat(filepath.Join(root, string(cohortDigest)[len("sha256:"):])); !os.IsNotExist(statErr) {
		t.Fatalf("rejected audit left a cohort directory behind: %v", statErr)
	}
}

func TestWriteAuditRejectsInconsistentStateOrRevisionIdentityBeforeCommit(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	cohort := DigestBytes([]byte("identity-reject"))
	for _, test := range []struct {
		name   string
		mutate func(*EvaluationRecord)
	}{
		{name: "state", mutate: func(record *EvaluationRecord) { record.StateDigest = DigestBytes([]byte("wrong-state")) }},
		{name: "base revision", mutate: func(record *EvaluationRecord) { record.BaseSHA = "wrong-base" }},
		{name: "head revision", mutate: func(record *EvaluationRecord) { record.HeadSHA = "wrong-head" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := make([]EvaluationRecord, len(snapshot.Findings))
			for index := range snapshot.Findings {
				records[index] = testAuditRecord(t, snapshot, index, controls[index])
			}
			test.mutate(&records[0])
			root := t.TempDir()
			_, err := WriteAudit(root, AuditBundle{
				Snapshot: snapshot,
				Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohort},
				Records:  records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings,
				VerificationInputs: testVerificationInputs(t, snapshot, cohort),
			}, time.Unix(10, 0))
			if err == nil {
				t.Fatal("inconsistent audit identity unexpectedly committed")
			}
			cohortPath := filepath.Join(root, string(cohort)[len("sha256:"):])
			if _, statErr := os.Stat(filepath.Join(cohortPath, artifactManifestFile)); !os.IsNotExist(statErr) {
				t.Fatalf("rejected identity left a commit marker behind: %v", statErr)
			}
		})
	}
}

func TestVerifyArtifactRequiresExpectedProfileAndReconstructedIdentity(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	cohort := DigestBytes([]byte("profile-required"))
	records := make([]EvaluationRecord, len(snapshot.Findings))
	for index := range snapshot.Findings {
		records[index] = testAuditRecord(t, snapshot, index, controls[index])
	}
	root := t.TempDir()
	manifest, err := WriteAudit(root, AuditBundle{
		Snapshot: snapshot,
		Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohort},
		Records:  records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings,
		VerificationInputs: testVerificationInputs(t, snapshot, cohort),
	}, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	cohortDir := filepath.Join(root, string(cohort)[len("sha256:"):])
	manifestBytes, err := os.ReadFile(filepath.Join(cohortDir, artifactManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.RelativePath], err = os.ReadFile(filepath.Join(cohortDir, file.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyArtifact(manifestBytes, VerificationInputs{Snapshot: snapshot, Files: files, Rubric: DefaultRubric(), ExpectedCohortDigest: string(cohort)}); err == nil {
		t.Fatal("verification without expected profile unexpectedly succeeded")
	}
}

func TestWriteAuditRequiresCompleteVerificationInputs(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	cohort := DigestBytes([]byte("verification-required"))
	records := make([]EvaluationRecord, len(snapshot.Findings))
	for index := range snapshot.Findings {
		records[index] = testAuditRecord(t, snapshot, index, controls[index])
	}
	root := t.TempDir()
	_, err = WriteAudit(root, AuditBundle{
		Snapshot: snapshot,
		Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohort},
		Records:  records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings,
	}, time.Unix(10, 0))
	if err == nil {
		t.Fatal("audit without verification inputs unexpectedly committed")
	}
	if entries, readErr := os.ReadDir(root); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 0 {
		t.Fatalf("incomplete verification inputs created audit artifacts: %v", entries)
	}
}

func TestWriteAuditRejectsSymlinkArtifactRoot(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]EvaluationRecord, len(snapshot.Findings))
	for index := range snapshot.Findings {
		records[index] = testAuditRecord(t, snapshot, index, controls[index])
	}
	root := t.TempDir()
	outside := t.TempDir()
	symlinkRoot := filepath.Join(root, "symlink-root")
	if err := os.Symlink(outside, symlinkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cohortDigest := DigestBytes([]byte("symlink-reject"))
	if _, err := WriteAudit(symlinkRoot, AuditBundle{Snapshot: snapshot, Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohortDigest}, Records: records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings, VerificationInputs: testVerificationInputs(t, snapshot, cohortDigest)}, time.Unix(10, 0)); err == nil {
		t.Fatal("symlink artifact root unexpectedly accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, string(cohortDigest)[len("sha256:"):], artifactManifestFile)); !os.IsNotExist(err) {
		t.Fatalf("symlink escape wrote outside root: %v", err)
	}
}

func TestWriteAuditRejectsSymlinkedArtifactRootAncestor(t *testing.T) {
	snapshot := testSnapshot()
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	_, controls, err := BuildStates(snapshot, profile.BoundProfile)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]EvaluationRecord, len(snapshot.Findings))
	for index := range snapshot.Findings {
		records[index] = testAuditRecord(t, snapshot, index, controls[index])
	}
	parent := t.TempDir()
	outside := t.TempDir()
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(outside, linkedParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root := filepath.Join(linkedParent, "owned-audit")
	cohort := DigestBytes([]byte("ancestor-symlink-reject"))
	if _, err := WriteAudit(root, AuditBundle{
		Snapshot: snapshot,
		Manifest: Manifest{RunID: snapshot.RunID, CohortInputDigest: cohort},
		Records:  records, RawFindings: snapshot.Findings, EffectiveFindings: snapshot.Findings,
		VerificationInputs: testVerificationInputs(t, snapshot, cohort),
	}, time.Unix(10, 0)); err == nil {
		t.Fatal("symlinked artifact root ancestor unexpectedly accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "owned-audit", string(cohort)[len("sha256:"):], artifactManifestFile)); !os.IsNotExist(err) {
		t.Fatalf("symlinked ancestor wrote outside owner root: %v", err)
	}
}

func testAuditRecord(t *testing.T, snapshot Snapshot, index int, control Control) EvaluationRecord {
	t.Helper()
	finding := snapshot.Findings[index]
	findingDigest, err := DigestCanonical(finding)
	if err != nil {
		t.Fatal(err)
	}
	rawDigest, err := DigestCanonical(snapshot.Findings)
	if err != nil {
		t.Fatal(err)
	}
	return EvaluationRecord{
		RecordSchemaVersion: RecordSchemaVersion,
		RecordID:            "record-" + finding.ID.String(),
		RunID:               snapshot.RunID,
		FindingID:           finding.ID,
		SourceOrdinal:       index,
		OriginalSeverity:    originalSeverity(finding.Severity),
		RawFindingDigest:    findingDigest,
		RawFindingsDigest:   rawDigest,
		BaseSHA:             snapshot.PR.Base.SHA,
		HeadSHA:             snapshot.PR.Head.SHA,
		StateDigest:         control.StateDigest,
		ContextDigest:       control.ContextDigest,
		QuestionsDigest:     control.QuestionsDigest,
		RubricVersion:       RubricVersion,
		RubricDigest:        control.RubricDigest,
		PolicyVersion:       PolicyVersion,
		PolicyDigest:        control.PolicyDigest,
		BoundProfileDigest:  control.BoundProfileDigest,
		ProposedDecision:    DispositionKeep,
		EffectiveDecision:   DispositionKeep,
		EvaluatorStatus:     EvaluatorSkippedConfig,
		CandidateStatus:     CandidateUnavailable,
		AuditStatus:         AuditStatusDegraded,
	}
}
