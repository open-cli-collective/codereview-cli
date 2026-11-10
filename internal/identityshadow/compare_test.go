package identityshadow

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestCompareIdentityCountsAllFourCohortCellsAndUnresolvedChoices(t *testing.T) {
	snapshot := comparisonSnapshot()
	cohortInput, _, priorIDMap, err := buildTaskInputs(snapshot)
	if err != nil {
		t.Fatalf("buildTaskInputs: %v", err)
	}
	base, err := reportidentity.Replay(cohortInput)
	if err != nil {
		t.Fatalf("Replay(cohort): %v", err)
	}
	choices := map[string]reportidentity.Choice{
		"A/B": reportidentity.ChoiceSame,
		"A/C": reportidentity.ChoiceSame,
		"A/D": reportidentity.ChoiceDifferent,
		"A/E": reportidentity.ChoiceDifferent,
		"B/C": reportidentity.ChoiceAbstain,
		"B/D": reportidentity.ChoiceError,
		"B/E": reportidentity.ChoiceDifferent,
		"C/D": reportidentity.ChoiceDifferent,
		"C/E": reportidentity.ChoiceMissing,
		"D/E": reportidentity.ChoiceDifferent,
	}
	cohort := resultWithChoices(t, cohortInput, base, choices)
	_, priorInput, _, err := buildTaskInputs(snapshot)
	if err != nil {
		t.Fatalf("buildTaskInputs(prior): %v", err)
	}
	priorBase, err := reportidentity.Replay(priorInput)
	if err != nil {
		t.Fatalf("Replay(prior): %v", err)
	}
	prior := resultWithDefaultChoice(t, priorInput, priorBase, reportidentity.ChoiceDifferent)

	comparison := compareIdentity(snapshot, cohort, prior, priorIDMap)
	section := comparison.Cohort
	if section.ExpectedPairs != 10 || section.BaselineSame != 2 || section.BaselineDifferent != 8 || section.BaselineUnknown != 0 {
		t.Fatalf("cohort baseline counts = %#v", section)
	}
	if section.Cells.BaselineSameShadowSame == 0 || section.Cells.BaselineSameShadowDifferent == 0 ||
		section.Cells.BaselineDifferentShadowSame == 0 || section.Cells.BaselineDifferentShadowDifferent == 0 {
		t.Fatalf("cohort cells = %#v", section.Cells)
	}
	if section.ShadowAbstain != 1 || section.ShadowError != 1 || section.ShadowMissing != 1 {
		t.Fatalf("cohort unresolved prediction counts = %#v", section)
	}
	if section.ComparablePairs != section.Agreements+section.Disagreements || section.ExpectedPairs != section.ComparablePairs+section.UnresolvedPairs {
		t.Fatalf("cohort denominator identities fail: %#v", section)
	}
	if section.ComparablePairs != 7 || section.Agreements != 5 || section.Disagreements != 2 || section.UnresolvedPairs != 3 {
		t.Fatalf("cohort agreement counts = %#v", section)
	}
}

func TestCompareIdentityPriorRuleUsesExactHashEqualityAndUnknownKeys(t *testing.T) {
	snapshot := comparisonSnapshot()
	_, priorInput, priorIDMap, err := buildTaskInputs(snapshot)
	if err != nil {
		t.Fatalf("buildTaskInputs: %v", err)
	}
	base, err := reportidentity.Replay(priorInput)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	prior := resultWithDefaultChoice(t, priorInput, base, reportidentity.ChoiceSame)
	cohortInput, _, _, _ := buildTaskInputs(snapshot)
	cohortBase, _ := reportidentity.Replay(cohortInput)
	cohort := resultWithDefaultChoice(t, cohortInput, cohortBase, reportidentity.ChoiceDifferent)

	section := compareIdentity(snapshot, cohort, prior, priorIDMap).PriorExact
	if section.ExpectedPairs != len(snapshot.Findings)*len(snapshot.PriorRoots) {
		t.Fatalf("prior pair universe = %d", section.ExpectedPairs)
	}
	if section.BaselineUnknown != 2 {
		t.Fatalf("nil key hashes should leave one finding's prior pairs unknown; counts=%#v", section)
	}
	if section.ComparablePairs != section.Agreements+section.Disagreements || section.ExpectedPairs != section.ComparablePairs+section.UnresolvedPairs {
		t.Fatalf("prior denominator identities fail: %#v", section)
	}
	firstRootReplayID := priorIDMap[snapshot.PriorRoots[0].ID]
	secondRootReplayID := priorIDMap[snapshot.PriorRoots[1].ID]
	var firstRootPair, secondRootPair *PairComparison
	for index := range section.Pairs {
		pair := &section.Pairs[index]
		if pair.LeftID != "A" {
			continue
		}
		switch pair.RightID {
		case firstRootReplayID:
			firstRootPair = pair
		case secondRootReplayID:
			secondRootPair = pair
		}
	}
	if firstRootPair == nil || secondRootPair == nil {
		t.Fatalf("comparison omitted A/prior-root pairs: root IDs=%q/%q pairs=%#v", firstRootReplayID, secondRootReplayID, section.Pairs)
	}
	if firstRootPair.BaselineChoice != baselineSame || firstRootPair.Disposition != "agreement" || firstRootPair.PredictionChoice != reportidentity.ChoiceSame {
		t.Fatalf("A/first-prior-root comparison = %#v, want baseline same/agreement with the unchanged same prediction", firstRootPair)
	}
	if secondRootPair.BaselineChoice != baselineDifferent || secondRootPair.Disposition != "disagreement" || secondRootPair.PredictionChoice != reportidentity.ChoiceSame {
		t.Fatalf("A/second-prior-root comparison = %#v, want baseline different/disagreement with the unchanged same prediction", secondRootPair)
	}

	badMap := cloneStringMap(priorIDMap)
	badMap[snapshot.PriorRoots[0].ID] = "not-the-deterministic-id"
	boundSection := compareIdentity(snapshot, cohort, prior, badMap).PriorExact
	if boundSection.ExpectedPairs != section.ExpectedPairs || boundSection.BaselineUnknown != section.ExpectedPairs || boundSection.UnresolvedPairs != section.ExpectedPairs {
		t.Fatalf("invalid source binding must make the full prior universe unresolved: %#v", boundSection)
	}
	for _, pair := range boundSection.Pairs {
		if pair.DiagnosticCode != "invalid_source_binding" {
			t.Fatalf("diagnostic = %q, want invalid_source_binding", pair.DiagnosticCode)
		}
	}
}

func TestCompareIdentityInvalidPredictionBindingMakesWholeTaskUnusable(t *testing.T) {
	snapshot := comparisonSnapshot()
	cohortInput, _, priorIDMap, err := buildTaskInputs(snapshot)
	if err != nil {
		t.Fatalf("buildTaskInputs: %v", err)
	}
	base, err := reportidentity.Replay(cohortInput)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	valid := resultWithDefaultChoice(t, cohortInput, base, reportidentity.ChoiceSame)

	for name, mutate := range map[string]func(*reportidentity.Result){
		"stale hash": func(result *reportidentity.Result) {
			result.Pairs[0].InputSHA256 = strings.Repeat("0", 64)
		},
		"unknown pair": func(result *reportidentity.Result) {
			result.Pairs = append(result.Pairs, reportidentity.Pair{LeftID: "unknown", RightID: "pair", InputSHA256: strings.Repeat("a", 64), Choice: reportidentity.ChoiceSame})
		},
		"duplicate pair": func(result *reportidentity.Result) {
			result.Pairs = append(result.Pairs, result.Pairs[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			invalid.Pairs = append([]reportidentity.Pair{}, valid.Pairs...)
			mutate(&invalid)
			priorInput, err := priorInputFor(snapshot)
			if err != nil {
				t.Fatalf("priorInputFor: %v", err)
			}
			priorBase, err := reportidentity.Replay(priorInput)
			if err != nil {
				t.Fatalf("Replay(prior): %v", err)
			}
			prior := resultWithDefaultChoice(t, priorInput, priorBase, reportidentity.ChoiceDifferent)
			section := compareIdentity(snapshot, invalid, prior, priorIDMap).Cohort
			if section.InvalidPredictionRows != 1 || section.ShadowMissing != section.ExpectedPairs || section.UnresolvedPairs != section.ExpectedPairs || section.ComparablePairs != 0 {
				t.Fatalf("invalid prediction set was not wholly unresolved: %#v", section)
			}
			for _, pair := range section.Pairs {
				if pair.DiagnosticCode != "invalid_prediction_binding" || pair.PredictionChoice != reportidentity.ChoiceMissing {
					t.Fatalf("invalid pair binding result = %#v", pair)
				}
			}
		})
	}
}

func comparisonSnapshot() reportidentitysnapshot.Snapshot {
	findings := []review.Finding{
		comparisonFinding("A"), comparisonFinding("B"), comparisonFinding("C"), comparisonFinding("D"), comparisonFinding("E"),
	}
	rootA := reportidentitysnapshot.PriorRoot{ID: "review-thread-1", FilePath: "src/a.go", Body: "Prior finding body"}
	rootB := reportidentitysnapshot.PriorRoot{ID: "thread/β", FilePath: "src/b.go", Body: "Another prior body", Resolved: true}
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	return reportidentitysnapshot.Snapshot{
		Source:   reportidentitysnapshot.Source{RunID: "run-1", CRVersion: "test-version"},
		Findings: findings, PriorRoots: []reportidentitysnapshot.PriorRoot{rootA, rootB},
		Baseline: reportidentitysnapshot.Baseline{
			SchemaVersion: 1, CohortStatus: "accepted_rollup",
			EffectiveOrderedIDs: []review.FindingID{"A", "C", "E"},
			Dedupe: []reportidentitysnapshot.Dedupe{
				{Kept: "A", Dropped: []review.FindingID{"B"}},
				{Kept: "A", Dropped: []review.FindingID{}},
				{Kept: "C", Dropped: []review.FindingID{"D"}},
			},
			PriorRule: priorRuleV1,
			PriorKeys: []reportidentitysnapshot.PriorKey{
				{RecordKind: "finding", RecordID: "A", KeySHA256: &hashA},
				{RecordKind: "finding", RecordID: "B", KeySHA256: &hashB},
				{RecordKind: "finding", RecordID: "C", KeySHA256: nil},
				{RecordKind: "finding", RecordID: "D", KeySHA256: &hashB},
				{RecordKind: "finding", RecordID: "E", KeySHA256: &hashA},
				{RecordKind: "thread", RecordID: rootA.ID, KeySHA256: &hashA},
				{RecordKind: "thread", RecordID: rootB.ID, KeySHA256: &hashB},
			},
		},
	}
}

func comparisonFinding(id string) review.Finding {
	return review.Finding{
		ID: review.FindingID(id), Severity: review.SeverityMajor, FilePath: "src/" + strings.ToLower(id) + ".go",
		Anchor: review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 10},
		Body:   "Independent corrective obligation " + id,
	}
}

func resultWithChoices(t *testing.T, input reportidentity.Input, base reportidentity.Result, choices map[string]reportidentity.Choice) reportidentity.Result {
	t.Helper()
	decisions := make([]reportidentity.Decision, 0, len(base.Pairs))
	for _, pair := range base.Pairs {
		choice, exists := choices[pair.LeftID+"/"+pair.RightID]
		if !exists {
			t.Fatalf("no test prediction for pair %s/%s", pair.LeftID, pair.RightID)
		}
		if choice == reportidentity.ChoiceMissing {
			continue
		}
		decisions = append(decisions, reportidentity.Decision{LeftID: pair.LeftID, RightID: pair.RightID, Choice: choice, InputSHA256: pair.InputSHA256})
	}
	input.Decisions = decisions
	result, err := reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("Replay(decisions): %v", err)
	}
	return result
}

func resultWithDefaultChoice(t *testing.T, input reportidentity.Input, base reportidentity.Result, choice reportidentity.Choice) reportidentity.Result {
	t.Helper()
	decisions := make([]reportidentity.Decision, len(base.Pairs))
	for index, pair := range base.Pairs {
		decisions[index] = reportidentity.Decision{LeftID: pair.LeftID, RightID: pair.RightID, Choice: choice, InputSHA256: pair.InputSHA256}
	}
	input.Decisions = decisions
	result, err := reportidentity.Replay(input)
	if err != nil {
		t.Fatalf("Replay(decisions): %v", err)
	}
	return result
}

func priorInputFor(snapshot reportidentitysnapshot.Snapshot) (reportidentity.Input, error) {
	_, prior, _, err := buildTaskInputs(snapshot)
	if err != nil {
		return reportidentity.Input{}, err
	}
	return prior, nil
}
