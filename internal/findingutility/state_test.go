package findingutility

import (
	"encoding/json"
	"testing"
)

func TestBuildStatesProjectsDetachedRawFindingsAndSixStateObjects(t *testing.T) {
	snapshot := testSnapshot()
	original := append([]byte(nil), snapshot.Findings[0].Body...)
	states, controls, err := BuildStates(snapshot, testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != len(snapshot.Findings) || len(controls) != len(snapshot.Findings) {
		t.Fatalf("states=%d controls=%d", len(states), len(controls))
	}
	if states[0].Finding.Title != "" || states[0].Finding.OriginalSeverity != "minor" || states[0].Finding.OriginalSeverityRaw != "minor" || states[0].Finding.Location.Side != "head" {
		t.Fatalf("finding projection = %#v", states[0].Finding)
	}
	if states[1].Finding.OriginalSeverity != "nit" || states[1].RelatedFindings.Items[0].ID != "F-001" {
		t.Fatalf("nits/candidate mapping = %#v", states[1])
	}
	if string(snapshot.Findings[0].Body) != string(original) {
		t.Fatal("BuildStates mutated raw finding")
	}
	encoded, err := json.Marshal(states[0])
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"finding", "pull_request", "evidence", "related_findings", "policy", "input_limitations"} {
		if _, ok := object[key]; !ok {
			t.Fatalf("state missing top-level object %q", key)
		}
	}
	if len(object) != 6 {
		t.Fatalf("state top-level keys = %d, want 6", len(object))
	}
	if !controls[0].StateValid || !controls[0].Fresh || controls[0].Mode != ModeAdvisory || controls[0].VendorPermission.Permitted {
		t.Fatalf("control safety envelope = %#v", controls[0])
	}
	if !ValidDigest(controls[0].RawFindingsDigest) || !ValidDigest(controls[0].StateDigest) || !ValidDigest(controls[0].ContextDigest) {
		t.Fatalf("control digests = %#v", controls[0])
	}
}

func TestBuildStatesMarksMissingSourceAndIntentDecisionRelevant(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.FindingSources = nil
	snapshot.PR.Title = ""
	snapshot.PR.Body = ""
	states, controls, err := BuildStates(snapshot, testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].InputLimitations.SourceComplete || states[0].InputLimitations.ContextComplete || controls[0].SourceComplete || controls[0].ContextComplete {
		t.Fatalf("incomplete source/context was marked complete: %#v %#v", states[0].InputLimitations, controls[0])
	}
	if !hasLimitation(states[0].InputLimitations.Items, LimitationMissingSource) || !hasLimitation(states[0].InputLimitations.Items, LimitationMissingIntent) {
		t.Fatalf("missing limitations = %#v", states[0].InputLimitations.Items)
	}
}

func TestBuildStatesRejectsInvalidSnapshotWithoutGuessing(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.RunID = ""
	if _, _, err := BuildStates(snapshot, testBoundProfile()); err == nil {
		t.Fatal("missing run ID must fail")
	}
	snapshot = testSnapshot()
	snapshot.Findings[1].ID = snapshot.Findings[0].ID
	if _, _, err := BuildStates(snapshot, testBoundProfile()); err == nil {
		t.Fatal("duplicate finding ID must fail")
	}
	snapshot = testSnapshot()
	snapshot.Findings[0].Severity = "unknown"
	states, controls, err := BuildStates(snapshot, testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Finding.OriginalSeverity != "unknown" || controls[0].OriginalSeverity != "unknown" {
		t.Fatalf("unknown source severity was not retained: %#v %#v", states[0].Finding, controls[0])
	}
}

func TestBuildStatesMarksBoundOverflowDecisionRelevant(t *testing.T) {
	snapshot := testSnapshot()
	profile := testBoundProfile()
	profile.MaxFindingBytes = 4
	profile.MaxEvidenceItemBytes = 4
	profile.MaxRelatedFindings = 0
	states, controls, err := BuildStates(snapshot, profile)
	if err != nil {
		t.Fatal(err)
	}
	if states[0].InputLimitations.ContextComplete || controls[0].ContextComplete || controls[1].CandidateSetComplete {
		t.Fatalf("bound overflow was not retained as incomplete: state=%#v controls=%#v", states[0].InputLimitations, controls)
	}
	if !hasLimitation(states[0].InputLimitations.Items, LimitationTruncatedContext) || !hasLimitation(states[1].InputLimitations.Items, LimitationCandidateSetIncomplete) {
		t.Fatalf("bound limitations = %#v / %#v", states[0].InputLimitations.Items, states[1].InputLimitations.Items)
	}
}

func TestBuildStatesDoesNotShareMutableNestedSlices(t *testing.T) {
	snapshot := testSnapshot()
	states, _, err := BuildStates(snapshot, testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	states[0].PullRequest.Changes[0].EvidenceIDs[0] = "changed"
	states[0].Evidence.Items[0].Content = "changed"
	if states[1].Evidence.Items[0].Content == "changed" || snapshot.Changes[0].ID != "change-1" || snapshot.PR.Title != "Improve validation" {
		t.Fatal("state projection shares mutable source data")
	}
}

func TestBuildStatesBindsCanonicalDefinitionsAndExplicitMissingEvidence(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.PR.Title = ""
	states, controls, err := BuildStates(snapshot, testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].PullRequest.Intent.Text == nil || !hasLimitationID(states[0].InputLimitations.Items, "missing-intent-title") {
		t.Fatalf("missing title intent was not retained explicitly: %#v", states[0].PullRequest.Intent)
	}
	for _, want := range []string{"evidence:caller", "evidence:test"} {
		found := false
		for _, item := range states[0].Evidence.Items {
			if item.ID == want && item.Availability == EvidenceMissing && item.Content == "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing evidence item %q not explicit: %#v", want, states[0].Evidence.Items)
		}
	}
	rubric := DefaultRubric()
	rubricDigest, err := DigestCanonical(rubric)
	if err != nil {
		t.Fatal(err)
	}
	policyDigest, err := DigestCanonical(states[0].Policy)
	if err != nil {
		t.Fatal(err)
	}
	questions, err := Questions(rubric, states[0])
	if err != nil {
		t.Fatal(err)
	}
	if controls[0].RubricDigest != rubricDigest || controls[0].PolicyDigest != policyDigest || controls[0].QuestionsDigest != questions.Digest {
		t.Fatalf("definition digests are not bound canonically: control=%#v rubric=%s policy=%s questions=%s", controls[0], rubricDigest, policyDigest, questions.Digest)
	}
	rubric.Questions[0].Instructions += " tampered"
	changedRubricDigest, err := DigestCanonical(rubric)
	if err != nil || changedRubricDigest == rubricDigest {
		t.Fatalf("rubric definition tampering did not change digest: %s/%v", changedRubricDigest, err)
	}
	policy := states[0].Policy
	policy.IntentRule += " tampered"
	changedPolicyDigest, err := DigestCanonical(policy)
	if err != nil || changedPolicyDigest == policyDigest {
		t.Fatalf("policy definition tampering did not change digest: %s/%v", changedPolicyDigest, err)
	}
}

func TestValidateStateReferencesRejectsDanglingLimitations(t *testing.T) {
	states, _, err := BuildStates(testSnapshot(), testBoundProfile())
	if err != nil {
		t.Fatal(err)
	}
	states[0].Evidence.Items[0].LimitationIDs = append(states[0].Evidence.Items[0].LimitationIDs, "dangling-limitation")
	if err := validateStateReferences(states[0]); err == nil {
		t.Fatal("dangling limitation reference unexpectedly accepted")
	}
}

func TestDecodeStrictRejectsLabelLeakage(t *testing.T) {
	var state State
	if err := DecodeStrict([]byte(`{"adjudicator_label":"keep"}`), &state); err == nil {
		t.Fatal("adjudicator labels must not enter evaluator state")
	}
}

func hasLimitation(values []Limitation, want LimitationCode) bool {
	for _, value := range values {
		if value.Code == want {
			return true
		}
	}
	return false
}
