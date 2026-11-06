package reportidentity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestDecodeRejectsMalformedWireInputs(t *testing.T) {
	valid := `{"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":[]}`
	threadInput := `{"schema_version":1,"task":"prior_threads","findings":[],"threads":[{"thread_id":"t1","eligible":true,"file_path":"a.go","body":"note"}],"decisions":[]}`
	cases := []struct {
		name string
		data string
	}{
		{name: "unknown root field", data: strings.TrimSuffix(valid, "}") + `,"extra":true}`},
		{name: "unknown nested finding field", data: `{"schema_version":1,"task":"cohort","findings":[{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":{"kind":"file"},"body":"note","extra":1}],"threads":[],"decisions":[]}`},
		{name: "case variant is not an alias", data: strings.Replace(threadInput, `"eligible":true`, `"eligible":true,"Eligible":false`, 1)},
		{name: "duplicate root key", data: `{"schema_version":1,"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":[]}`},
		{name: "duplicate nested key", data: `{"schema_version":1,"task":"cohort","findings":[{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":{"kind":"file","kind":"file"},"body":"note"}],"threads":[],"decisions":[]}`},
		{name: "duplicate escaped key", data: `{"schema_version":1,"task":"prior_threads","findings":[],"threads":[{"thread_id":"t1","eligible":true,"\u0065ligible":false,"file_path":"a.go","body":"note"}],"decisions":[]}`},
		{name: "missing array", data: `{"schema_version":1,"task":"cohort","findings":[],"threads":[]}`},
		{name: "null array", data: `{"schema_version":1,"task":"cohort","findings":null,"threads":[],"decisions":[]}`},
		{name: "null required string", data: `{"schema_version":1,"task":"cohort","findings":[{"finding_id":null,"severity":"major","file_path":"a.go","anchor":{"kind":"file"},"body":"note"}],"threads":[],"decisions":[]}`},
		{name: "missing anchor kind", data: `{"schema_version":1,"task":"cohort","findings":[{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":{},"body":"note"}],"threads":[],"decisions":[]}`},
		{name: "null eligible", data: `{"schema_version":1,"task":"prior_threads","findings":[],"threads":[{"thread_id":"t1","eligible":null,"file_path":"a.go","body":"note"}],"decisions":[]}`},
		{name: "trailing JSON value", data: valid + ` {}`},
		{name: "unsupported version", data: strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1)},
		{name: "unknown task", data: strings.Replace(valid, `"cohort"`, `"review"`, 1)},
		{name: "cohort threads must be empty", data: strings.Replace(threadInput, `"prior_threads"`, `"cohort"`, 1)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode([]byte(test.data)); err == nil {
				t.Fatalf("Decode(%s) succeeded, want error", test.name)
			}
		})
	}

	if _, err := Decode([]byte{0xff}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Decode(invalid UTF-8) error = %v, want UTF-8 error", err)
	}
	invalidScalar := strings.Replace(valid, `"decisions":[]`, `"decisions":[],"unused":"\ud800"`, 1)
	if _, err := Decode([]byte(invalidScalar)); err == nil || !strings.Contains(err.Error(), "surrogate") {
		t.Fatalf("Decode(unpaired surrogate) error = %v, want surrogate error", err)
	}
	pairedScalar := strings.Replace(valid, `"decisions":[]`, `"decisions":[],"unused":"\ud83d\ude00"`, 1)
	if _, err := Decode([]byte(pairedScalar)); err == nil {
		t.Fatal("Decode(paired scalar with unknown field) succeeded, want unknown-field error")
	}
}

func TestDecodeEnforcesBoundsEnumsIDsAndRequiredContent(t *testing.T) {
	if _, err := Decode(bytes.Repeat([]byte{' '}, MaxFixtureBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Decode(over-size) error = %v, want size error", err)
	}

	baseFinding := `{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":{"kind":"file"},"body":"note"}`
	tooManyFindings := `{"schema_version":1,"task":"cohort","findings":[` + strings.TrimSuffix(strings.Repeat(baseFinding+",", maxFindings+1), ",") + `],"threads":[],"decisions":[]}`
	if _, err := Decode([]byte(tooManyFindings)); err == nil || !strings.Contains(err.Error(), "findings exceed") {
		t.Fatalf("Decode(too many findings) error = %v, want count error", err)
	}

	thread := `{"thread_id":"t1","eligible":false,"file_path":"a.go","body":"note"}`
	tooManyThreads := `{"schema_version":1,"task":"prior_threads","findings":[],"threads":[` + strings.TrimSuffix(strings.Repeat(thread+",", maxThreads+1), ",") + `],"decisions":[]}`
	if _, err := Decode([]byte(tooManyThreads)); err == nil || !strings.Contains(err.Error(), "threads exceed") {
		t.Fatalf("Decode(too many threads) error = %v, want count error", err)
	}

	decision := `{"left_id":"f1","right_id":"f2","choice":"same","input_sha256":"` + strings.Repeat("0", 64) + `"}`
	tooManyDecisions := `{"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":[` + strings.TrimSuffix(strings.Repeat(decision+",", maxDecisions+1), ",") + `]}`
	if _, err := Decode([]byte(tooManyDecisions)); err == nil || !strings.Contains(err.Error(), "decisions exceed") {
		t.Fatalf("Decode(too many decisions) error = %v, want count error", err)
	}

	badValues := []string{
		strings.Replace(baseFinding, `"severity":"major"`, `"severity":"Major"`, 1),
		strings.Replace(baseFinding, `"kind":"file"`, `"kind":"FILE"`, 1),
		strings.Replace(baseFinding, `"finding_id":"f1"`, `"finding_id":"bad id"`, 1),
		strings.Replace(baseFinding, `"finding_id":"f1"`, `"finding_id":""`, 1),
		strings.Replace(baseFinding, `"file_path":"a.go"`, `"file_path":"   "`, 1),
		strings.Replace(baseFinding, `"body":"note"`, `"body":"\t"`, 1),
		strings.Replace(baseFinding, `"anchor":{"kind":"file"}`, `"anchor":{"kind":"line"}`, 1),
		strings.Replace(baseFinding, `"anchor":{"kind":"file"}`, `"anchor":{"kind":"file","side":"RIGHT"}`, 1),
		strings.Replace(baseFinding, `"anchor":{"kind":"file"}`, `"anchor":{"kind":"file","side":""}`, 1),
		strings.Replace(baseFinding, `"anchor":{"kind":"file"}`, `"anchor":{"kind":"file","line":0}`, 1),
		strings.Replace(baseFinding, `"anchor":{"kind":"file"}`, `"anchor":{"kind":"file","side":"","line":0}`, 1),
	}
	for index, finding := range badValues {
		data := `{"schema_version":1,"task":"cohort","findings":[` + finding + `],"threads":[],"decisions":[]}`
		if _, err := Decode([]byte(data)); err == nil {
			t.Errorf("Decode(invalid finding %d) succeeded", index)
		}
	}
	if _, err := Decode([]byte(`{"schema_version":1,"task":"cohort","findings":[],"threads":[],"decisions":null}`)); err == nil {
		t.Fatal("Decode(null decisions) succeeded")
	}
}

func TestDecodeEnforcesRecordAndDecisionStringBounds(t *testing.T) {
	longID := strings.Repeat("a", maxIDBytes+1)
	longPath := strings.Repeat("p", maxPathBytes+1)
	longBody := strings.Repeat("b", maxBodyBytes+1)
	cases := []struct {
		name    string
		finding string
	}{
		{name: "ID bytes", finding: `{"finding_id":"` + longID + `","severity":"major","file_path":"a.go","anchor":{"kind":"file"},"body":"note"}`},
		{name: "path bytes", finding: `{"finding_id":"f1","severity":"major","file_path":"` + longPath + `","anchor":{"kind":"file"},"body":"note"}`},
		{name: "body bytes", finding: `{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":{"kind":"file"},"body":"` + longBody + `"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			data := `{"schema_version":1,"task":"cohort","findings":[` + test.finding + `],"threads":[],"decisions":[]}`
			if _, err := Decode([]byte(data)); err == nil {
				t.Fatal("Decode succeeded, want bound error")
			}
		})
	}
}

func TestPairInputSHA256Golden(t *testing.T) {
	records := Records{
		SchemaVersion: SchemaVersion,
		Task:          TaskCohort,
		Findings: []review.Finding{
			{
				ID:        "f.alpha",
				Severity:  review.SeverityMajor,
				FilePath:  "src/alpha.go",
				Anchor:    review.Anchor{Kind: review.AnchorKindFile},
				Body:      "Finding <one> & two",
				Anchoring: review.AnchoringInline,
			},
			finding("f.beta", review.SeverityMinor),
		},
		Threads: []Thread{},
	}
	result, err := Assemble(records, []Decision{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(result.Pairs) != 1 {
		t.Fatalf("pair count = %d, want 1", len(result.Pairs))
	}
	const want = "a13cd31e7bc86da40da17bbab1f5b1a0e87ae4e58970adce97b3e9d808a04b1c"
	if result.Pairs[0].InputSHA256 != want {
		t.Fatalf("input_sha256 = %q, want %q", result.Pairs[0].InputSHA256, want)
	}
}

func TestCompleteCohortAssemblesStableGroupsAndRollupProjection(t *testing.T) {
	records := Records{
		SchemaVersion: SchemaVersion,
		Task:          TaskCohort,
		Findings: []review.Finding{
			finding("f1", review.SeverityMajor),
			finding("f2", review.SeverityBlocking),
			finding("f3", review.SeverityBlocking),
			finding("f4", review.SeverityMinor),
		},
		Threads: []Thread{},
	}
	decisions := decisionsFor(t, records, map[pairKey]Choice{
		{left: "f1", right: "f2"}: ChoiceSame,
		{left: "f1", right: "f3"}: ChoiceSame,
		{left: "f1", right: "f4"}: ChoiceDifferent,
		{left: "f2", right: "f3"}: ChoiceSame,
		{left: "f2", right: "f4"}: ChoiceDifferent,
		{left: "f3", right: "f4"}: ChoiceDifferent,
	})
	result, err := Assemble(records, decisions)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if result.Status != statusComplete {
		t.Fatalf("status = %q, want complete", result.Status)
	}
	wantGroups := []Group{
		{RepresentativeID: "f2", MemberIDs: []string{"f1", "f2", "f3"}},
		{RepresentativeID: "f4", MemberIDs: []string{"f4"}},
	}
	if !reflect.DeepEqual(result.Groups, wantGroups) {
		t.Fatalf("groups = %#v, want %#v", result.Groups, wantGroups)
	}
	wantDedupe := []review.DedupeEntry{{Kept: "f2", Dropped: []review.FindingID{"f1", "f3"}, Reason: "Recorded identity match"}}
	if !reflect.DeepEqual(result.RollupDedupeLog, wantDedupe) {
		t.Fatalf("dedupe log = %#v, want %#v", result.RollupDedupeLog, wantDedupe)
	}
	if want := []review.FindingID{"f2", "f4"}; !reflect.DeepEqual(result.RollupOrderedFindings, want) {
		t.Fatalf("ordered findings = %#v, want %#v", result.RollupOrderedFindings, want)
	}
	if len(result.Issues) != 0 || len(result.Associations) != 0 {
		t.Fatalf("issues/associations = %#v/%#v, want empty", result.Issues, result.Associations)
	}
	assertRollupCompatibility(t, records.Findings, result)
}

func TestCompleteAllDifferentAndEmptyCohorts(t *testing.T) {
	records := cohortRecords(review.SeverityNits, review.SeverityMajor, review.SeverityMinor)
	choices := make(map[pairKey]Choice)
	base, err := Assemble(records, []Decision{})
	if err != nil {
		t.Fatalf("Assemble missing: %v", err)
	}
	for _, pair := range base.Pairs {
		choices[pairKey{left: pair.LeftID, right: pair.RightID}] = ChoiceDifferent
	}
	result, err := Assemble(records, decisionsFor(t, records, choices))
	if err != nil {
		t.Fatalf("Assemble all-different: %v", err)
	}
	if result.Status != statusComplete || len(result.Groups) != 3 {
		t.Fatalf("all-different status/groups = %q/%#v", result.Status, result.Groups)
	}
	if want := []review.FindingID{"f2", "f3", "f1"}; !reflect.DeepEqual(result.RollupOrderedFindings, want) {
		t.Fatalf("all-different order = %#v, want %#v", result.RollupOrderedFindings, want)
	}
	assertRollupCompatibility(t, records.Findings, result)

	empty := Records{SchemaVersion: SchemaVersion, Task: TaskCohort, Findings: []review.Finding{}, Threads: []Thread{}}
	emptyResult, err := Assemble(empty, []Decision{})
	if err != nil {
		t.Fatalf("Assemble empty cohort: %v", err)
	}
	if emptyResult.Status != statusComplete || emptyResult.Pairs == nil || emptyResult.Groups == nil || emptyResult.Issues == nil || emptyResult.RollupOrderedFindings == nil {
		t.Fatalf("empty cohort result = %#v, want complete and non-null arrays", emptyResult)
	}

	single := Records{SchemaVersion: SchemaVersion, Task: TaskCohort, Findings: []review.Finding{finding("only", review.SeverityMinor)}, Threads: []Thread{}}
	singleResult, err := Assemble(single, []Decision{})
	if err != nil {
		t.Fatalf("Assemble singleton: %v", err)
	}
	if singleResult.Status != statusComplete || !reflect.DeepEqual(singleResult.Groups, []Group{{RepresentativeID: "only", MemberIDs: []string{"only"}}}) {
		t.Fatalf("singleton result = %#v", singleResult)
	}
}

func TestCohortUncertaintyAndContradictionsUseSingletonFallback(t *testing.T) {
	records := cohortRecords(review.SeverityMajor, review.SeverityMinor, review.SeverityNits)
	tests := []struct {
		name          string
		choices       map[pairKey]Choice
		wantStatus    string
		wantIssueCode []string
	}{
		{
			name:          "missing pair",
			choices:       map[pairKey]Choice{{left: "f1", right: "f2"}: ChoiceSame},
			wantStatus:    statusUnresolved,
			wantIssueCode: []string{"missing", "missing"},
		},
		{
			name: "abstain isolates all pairs",
			choices: map[pairKey]Choice{
				{left: "f1", right: "f2"}: ChoiceSame,
				{left: "f1", right: "f3"}: ChoiceAbstain,
				{left: "f2", right: "f3"}: ChoiceSame,
			},
			wantStatus:    statusUnresolved,
			wantIssueCode: []string{"abstain"},
		},
		{
			name: "error status wins",
			choices: map[pairKey]Choice{
				{left: "f1", right: "f2"}: ChoiceSame,
				{left: "f1", right: "f3"}: ChoiceError,
			},
			wantStatus:    statusError,
			wantIssueCode: []string{"error", "missing"},
		},
		{
			name: "same same different triangle",
			choices: map[pairKey]Choice{
				{left: "f1", right: "f2"}: ChoiceSame,
				{left: "f1", right: "f3"}: ChoiceDifferent,
				{left: "f2", right: "f3"}: ChoiceSame,
			},
			wantStatus:    statusUnresolved,
			wantIssueCode: []string{"contradiction"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Assemble(records, decisionsFor(t, records, test.choices))
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			if len(result.Groups) != len(records.Findings) {
				t.Fatalf("groups = %#v, want all singleton groups", result.Groups)
			}
			codes := make([]string, len(result.Issues))
			for index, issue := range result.Issues {
				codes[index] = issue.Code
			}
			if !reflect.DeepEqual(codes, test.wantIssueCode) {
				t.Fatalf("issue codes = %#v, want %#v", codes, test.wantIssueCode)
			}
			if len(result.RollupDedupeLog) != 0 {
				t.Fatalf("dedupe log = %#v, want empty fallback", result.RollupDedupeLog)
			}
			if want := []review.FindingID{"f1", "f2", "f3"}; !reflect.DeepEqual(result.RollupOrderedFindings, want) {
				t.Fatalf("fallback order = %#v, want %#v", result.RollupOrderedFindings, want)
			}
			assertRollupCompatibility(t, records.Findings, result)
		})
	}

	larger := cohortRecords(review.SeverityMinor, review.SeverityMajor, review.SeverityBlocking, review.SeverityNits)
	choices := map[pairKey]Choice{
		{left: "f1", right: "f2"}: ChoiceSame,
		{left: "f1", right: "f3"}: ChoiceSame,
		{left: "f1", right: "f4"}: ChoiceDifferent,
		{left: "f2", right: "f3"}: ChoiceSame,
		{left: "f2", right: "f4"}: ChoiceSame,
		{left: "f3", right: "f4"}: ChoiceDifferent,
	}
	result, err := Assemble(larger, decisionsFor(t, larger, choices))
	if err != nil {
		t.Fatalf("Assemble larger contradiction: %v", err)
	}
	if result.Status != statusUnresolved || !reflect.DeepEqual(result.Issues, []Issue{{Code: "contradiction", LeftID: "f1", RightID: "f4"}, {Code: "contradiction", LeftID: "f3", RightID: "f4"}}) {
		t.Fatalf("larger contradiction result = %#v", result)
	}
}

func TestCohortRetainsOriginalOptionalValuesAndStableDecisionOrdering(t *testing.T) {
	records := cohortRecords(review.SeverityMajor, review.SeverityMinor, review.SeverityNits)
	records.Findings[0].Anchor = review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 22}
	records.Findings[0].Anchoring = review.AnchoringFileLevelFallback
	choices := map[pairKey]Choice{
		{left: "f1", right: "f2"}: ChoiceSame,
		{left: "f1", right: "f3"}: ChoiceSame,
		{left: "f2", right: "f3"}: ChoiceSame,
	}
	decisions := decisionsFor(t, records, choices)
	first, err := Assemble(records, decisions)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for left, right := 0, len(decisions)-1; left < right; left, right = left+1, right-1 {
		decisions[left], decisions[right] = decisions[right], decisions[left]
	}
	second, err := Assemble(records, decisions)
	if err != nil {
		t.Fatalf("Assemble shuffled decisions: %v", err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("decision order changed output:\n%s\n%s", firstJSON, secondJSON)
	}
	if !reflect.DeepEqual(first.Findings, records.Findings) {
		t.Fatalf("findings = %#v, want preserved originals %#v", first.Findings, records.Findings)
	}
}

func TestReplayIsIndependentOfFixtureFormattingAndDecisionOrder(t *testing.T) {
	records := cohortRecords(review.SeverityMajor, review.SeverityMinor, review.SeverityNits)
	input := Input{
		SchemaVersion: records.SchemaVersion,
		Task:          records.Task,
		Findings:      records.Findings,
		Threads:       records.Threads,
		Decisions: decisionsFor(t, records, map[pairKey]Choice{
			{left: "f1", right: "f2"}: ChoiceSame,
			{left: "f1", right: "f3"}: ChoiceDifferent,
			{left: "f2", right: "f3"}: ChoiceDifferent,
		}),
	}
	compact, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal compact fixture: %v", err)
	}
	formatted, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		t.Fatalf("Marshal formatted fixture: %v", err)
	}
	formattedInput, err := Decode(formatted)
	if err != nil {
		t.Fatalf("Decode formatted fixture: %v", err)
	}
	compactInput, err := Decode(compact)
	if err != nil {
		t.Fatalf("Decode compact fixture: %v", err)
	}
	formattedResult, err := Replay(formattedInput)
	if err != nil {
		t.Fatalf("Replay formatted fixture: %v", err)
	}
	compactResult, err := Replay(compactInput)
	if err != nil {
		t.Fatalf("Replay compact fixture: %v", err)
	}
	formattedJSON, _ := json.Marshal(formattedResult)
	compactJSON, _ := json.Marshal(compactResult)
	if !bytes.Equal(formattedJSON, compactJSON) {
		t.Fatalf("fixture formatting changed output:\n%s\n%s", formattedJSON, compactJSON)
	}

	input.Decisions[0], input.Decisions[len(input.Decisions)-1] = input.Decisions[len(input.Decisions)-1], input.Decisions[0]
	shuffled, err := json.MarshalIndent(input, "", "\t")
	if err != nil {
		t.Fatalf("Marshal shuffled fixture: %v", err)
	}
	shuffledInput, err := Decode(shuffled)
	if err != nil {
		t.Fatalf("Decode shuffled fixture: %v", err)
	}
	shuffledResult, err := Replay(shuffledInput)
	if err != nil {
		t.Fatalf("Replay shuffled fixture: %v", err)
	}
	shuffledJSON, _ := json.Marshal(shuffledResult)
	if !bytes.Equal(compactJSON, shuffledJSON) {
		t.Fatalf("decision order changed output:\n%s\n%s", compactJSON, shuffledJSON)
	}
}

func TestReverseCohortPairIsNormalizedAndDuplicateReverseIsRejected(t *testing.T) {
	records := cohortRecords(review.SeverityMajor, review.SeverityMinor)
	base := decisionsFor(t, records, map[pairKey]Choice{{left: "f1", right: "f2"}: ChoiceDifferent})
	base[0].LeftID, base[0].RightID = base[0].RightID, base[0].LeftID
	result, err := Assemble(records, base)
	if err != nil {
		t.Fatalf("Assemble reversed pair: %v", err)
	}
	if result.Pairs[0].LeftID != "f1" || result.Pairs[0].RightID != "f2" || result.Pairs[0].Choice != ChoiceDifferent {
		t.Fatalf("normalized pair = %#v", result.Pairs[0])
	}
	duplicate := append(append([]Decision(nil), base...), base[0])
	duplicate[1].LeftID, duplicate[1].RightID = duplicate[1].RightID, duplicate[1].LeftID
	if _, err := Assemble(records, duplicate); err == nil || !strings.Contains(err.Error(), "repeats pair") {
		t.Fatalf("Assemble reversed duplicate error = %v, want duplicate error", err)
	}
}

func TestPriorThreadsPreserveManyToManyAndEligibility(t *testing.T) {
	records := Records{
		SchemaVersion: SchemaVersion,
		Task:          TaskPriorThread,
		Findings: []review.Finding{
			finding("f1", review.SeverityMajor),
			finding("f2", review.SeverityMinor),
		},
		Threads: []Thread{
			{ID: "t1", Eligible: true, FilePath: "a.go", Body: "earlier comment"},
			{ID: "t2", Eligible: true, FilePath: "b.go", Body: "second earlier comment"},
			{ID: "t3", Eligible: false, FilePath: "c.go", Body: "ineligible old comment"},
		},
	}
	decisions := decisionsFor(t, records, map[pairKey]Choice{
		{left: "f1", right: "t1"}: ChoiceSame,
		{left: "f1", right: "t2"}: ChoiceSame,
		{left: "f2", right: "t1"}: ChoiceDifferent,
	})
	result, err := Assemble(records, decisions)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if result.Status != statusUnresolved {
		t.Fatalf("status = %q, want unresolved", result.Status)
	}
	wantPairs := []Pair{
		{LeftID: "f1", RightID: "t1", InputSHA256: decisions[0].InputSHA256, Choice: ChoiceSame},
		{LeftID: "f1", RightID: "t2", InputSHA256: decisions[1].InputSHA256, Choice: ChoiceSame},
		{LeftID: "f2", RightID: "t1", InputSHA256: decisions[2].InputSHA256, Choice: ChoiceDifferent},
		{LeftID: "f2", RightID: "t2", InputSHA256: pairHashFor(t, records, "f2", "t2"), Choice: ChoiceMissing},
	}
	if !reflect.DeepEqual(result.Pairs, wantPairs) {
		t.Fatalf("pairs = %#v, want %#v", result.Pairs, wantPairs)
	}
	if want := []Association{{FindingID: "f1", ThreadID: "t1"}, {FindingID: "f1", ThreadID: "t2"}}; !reflect.DeepEqual(result.Associations, want) {
		t.Fatalf("associations = %#v, want %#v", result.Associations, want)
	}
	if !reflect.DeepEqual(result.Threads, records.Threads) || len(result.Groups) != 0 || len(result.RollupDedupeLog) != 0 || len(result.RollupOrderedFindings) != 0 {
		t.Fatalf("prior result did not preserve originals or empty projection arrays: %#v", result)
	}
	if !reflect.DeepEqual(result.Issues, []Issue{{Code: "missing", LeftID: "f2", RightID: "t2"}}) {
		t.Fatalf("issues = %#v", result.Issues)
	}
}

func TestPriorThreadEmptyAndRecordedErrorCases(t *testing.T) {
	empty := Records{SchemaVersion: SchemaVersion, Task: TaskPriorThread, Findings: []review.Finding{}, Threads: []Thread{}}
	emptyResult, err := Assemble(empty, []Decision{})
	if err != nil {
		t.Fatalf("Assemble empty prior task: %v", err)
	}
	if emptyResult.Status != statusComplete || emptyResult.Pairs == nil || emptyResult.Associations == nil || emptyResult.Issues == nil {
		t.Fatalf("empty prior result = %#v", emptyResult)
	}

	records := Records{
		SchemaVersion: SchemaVersion,
		Task:          TaskPriorThread,
		Findings:      []review.Finding{finding("f1", review.SeverityMajor)},
		Threads:       []Thread{{ID: "t1", Eligible: true, FilePath: "a.go", Body: "earlier comment"}, {ID: "t2", Eligible: true, FilePath: "b.go", Body: "older comment"}},
	}
	result, err := Assemble(records, decisionsFor(t, records, map[pairKey]Choice{
		{left: "f1", right: "t1"}: ChoiceSame,
		{left: "f1", right: "t2"}: ChoiceError,
	}))
	if err != nil {
		t.Fatalf("Assemble recorded error: %v", err)
	}
	if result.Status != statusError || !reflect.DeepEqual(result.Associations, []Association{{FindingID: "f1", ThreadID: "t1"}}) {
		t.Fatalf("recorded error result = %#v", result)
	}
}

func TestInvalidPairsChoicesAndHashesAreRejected(t *testing.T) {
	cohort := cohortRecords(review.SeverityMajor, review.SeverityMinor)
	valid := decisionsFor(t, cohort, map[pairKey]Choice{{left: "f1", right: "f2"}: ChoiceSame})[0]
	validCases := []struct {
		name      string
		decision  Decision
		records   Records
		wantError string
	}{
		{name: "unknown", decision: Decision{LeftID: "missing", RightID: "f2", Choice: ChoiceSame, InputSHA256: valid.InputSHA256}, records: cohort, wantError: "not an expected"},
		{name: "self", decision: Decision{LeftID: "f1", RightID: "f1", Choice: ChoiceSame, InputSHA256: valid.InputSHA256}, records: cohort, wantError: "self-pairs"},
		{name: "invalid choice", decision: Decision{LeftID: "f1", RightID: "f2", Choice: "maybe", InputSHA256: valid.InputSHA256}, records: cohort, wantError: "invalid choice"},
		{name: "missing is not an observation", decision: Decision{LeftID: "f1", RightID: "f2", Choice: ChoiceMissing, InputSHA256: valid.InputSHA256}, records: cohort, wantError: "invalid choice"},
		{name: "wrong hash", decision: Decision{LeftID: "f1", RightID: "f2", Choice: ChoiceAbstain, InputSHA256: strings.Repeat("0", 64)}, records: cohort, wantError: "does not match"},
		{name: "uppercase hash", decision: Decision{LeftID: "f1", RightID: "f2", Choice: ChoiceSame, InputSHA256: strings.Repeat("A", 64)}, records: cohort, wantError: "lowercase hexadecimal"},
		{name: "short hash", decision: Decision{LeftID: "f1", RightID: "f2", Choice: ChoiceSame, InputSHA256: "abc"}, records: cohort, wantError: "lowercase hexadecimal"},
	}
	for _, test := range validCases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Assemble(test.records, []Decision{test.decision})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Assemble error = %v, want substring %q", err, test.wantError)
			}
		})
	}

	prior := Records{SchemaVersion: SchemaVersion, Task: TaskPriorThread, Findings: []review.Finding{finding("f1", review.SeverityMajor)}, Threads: []Thread{{ID: "t1", Eligible: false, FilePath: "a.go", Body: "note"}}}
	ineligible := Decision{LeftID: "f1", RightID: "t1", Choice: ChoiceSame, InputSHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	if _, err := Assemble(prior, []Decision{ineligible}); err == nil || !strings.Contains(err.Error(), "ineligible") {
		t.Fatalf("ineligible pair error = %v, want ineligible", err)
	}

	priorEligible := Records{SchemaVersion: SchemaVersion, Task: TaskPriorThread, Findings: []review.Finding{finding("f1", review.SeverityMajor)}, Threads: []Thread{{ID: "t1", Eligible: true, FilePath: "a.go", Body: "note"}}}
	reversed := decisionsFor(t, priorEligible, map[pairKey]Choice{{left: "f1", right: "t1"}: ChoiceSame})[0]
	reversed.LeftID, reversed.RightID = reversed.RightID, reversed.LeftID
	if _, err := Assemble(priorEligible, []Decision{reversed}); err == nil {
		t.Fatal("reversed prior pair unexpectedly accepted")
	}
}

func TestDuplicateIDsAcrossNamespacesAreRejected(t *testing.T) {
	records := Records{
		SchemaVersion: SchemaVersion,
		Task:          TaskPriorThread,
		Findings:      []review.Finding{finding("same", review.SeverityMajor)},
		Threads:       []Thread{{ID: "same", Eligible: false, FilePath: "a.go", Body: "note"}},
	}
	if _, err := Assemble(records, []Decision{}); err == nil || !strings.Contains(err.Error(), "duplicate ID") {
		t.Fatalf("Assemble duplicate IDs error = %v, want duplicate ID", err)
	}
}

func TestCompatibilityProjectionPassesProductionRollupDecoder(t *testing.T) {
	records := cohortRecords(review.SeverityMajor, review.SeverityMinor)
	complete, err := Assemble(records, decisionsFor(t, records, map[pairKey]Choice{{left: "f1", right: "f2"}: ChoiceSame}))
	if err != nil {
		t.Fatalf("Assemble complete: %v", err)
	}
	assertRollupCompatibility(t, records.Findings, complete)

	fallback, err := Assemble(records, []Decision{})
	if err != nil {
		t.Fatalf("Assemble fallback: %v", err)
	}
	assertRollupCompatibility(t, records.Findings, fallback)
}

func TestDirectReplayRejectsNullSliceRepresentations(t *testing.T) {
	input := Input{SchemaVersion: SchemaVersion, Task: TaskCohort, Findings: []review.Finding{}, Threads: []Thread{}}
	if _, err := Replay(input); err == nil || !strings.Contains(err.Error(), "decisions must be an array") {
		t.Fatalf("Replay(nil decisions) error = %v", err)
	}
	input.Decisions = []Decision{}
	input.Threads = nil
	if _, err := Replay(input); err == nil || !strings.Contains(err.Error(), "non-nil arrays") {
		t.Fatalf("Replay(nil threads) error = %v", err)
	}
}

func cohortRecords(severities ...review.Severity) Records {
	findings := make([]review.Finding, 0, len(severities))
	for index, severity := range severities {
		findings = append(findings, finding(fmt.Sprintf("f%d", index+1), severity))
	}
	return Records{SchemaVersion: SchemaVersion, Task: TaskCohort, Findings: findings, Threads: []Thread{}}
}

func finding(id string, severity review.Severity) review.Finding {
	return review.Finding{
		ID:       review.FindingID(id),
		Severity: severity,
		FilePath: "src/" + id + ".go",
		Anchor:   review.Anchor{Kind: review.AnchorKindFile},
		Body:     "Synthetic finding for " + id,
	}
}

func decisionsFor(t *testing.T, records Records, choices map[pairKey]Choice) []Decision {
	t.Helper()
	result, err := Assemble(records, []Decision{})
	if err != nil {
		t.Fatalf("Assemble expected pair list: %v", err)
	}
	decisions := make([]Decision, 0, len(choices))
	for _, pair := range result.Pairs {
		choice, ok := choices[pairKey{left: pair.LeftID, right: pair.RightID}]
		if !ok {
			continue
		}
		decisions = append(decisions, Decision{LeftID: pair.LeftID, RightID: pair.RightID, Choice: choice, InputSHA256: pair.InputSHA256})
	}
	if len(decisions) != len(choices) {
		t.Fatalf("built %d decisions, want %d", len(decisions), len(choices))
	}
	return decisions
}

func pairHashFor(t *testing.T, records Records, left, right string) string {
	t.Helper()
	result, err := Assemble(records, []Decision{})
	if err != nil {
		t.Fatalf("Assemble expected pairs: %v", err)
	}
	for _, pair := range result.Pairs {
		if pair.LeftID == left && pair.RightID == right {
			return pair.InputSHA256
		}
	}
	t.Fatalf("pair %s/%s not found", left, right)
	return ""
}

func assertRollupCompatibility(t *testing.T, findings []review.Finding, result Result) {
	t.Helper()
	known := make(map[review.FindingID]review.Severity, len(findings))
	for _, finding := range findings {
		known[finding.ID] = finding.Severity
	}
	wire := struct {
		SchemaVersion        int                  `json:"schema_version"`
		ReviewEvent          string               `json:"review_event"`
		ReviewEventRationale string               `json:"review_event_rationale"`
		DedupeLog            []review.DedupeEntry `json:"dedupe_log"`
		OrderedFindings      []review.FindingID   `json:"ordered_findings"`
	}{SchemaVersion: 1, ReviewEvent: "comment", ReviewEventRationale: "Synthetic replay compatibility assertion", DedupeLog: result.RollupDedupeLog, OrderedFindings: result.RollupOrderedFindings}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("Marshal synthetic rollup: %v", err)
	}
	if _, err := llm.DecodeRollup(data, llm.RollupOptions{FindingSeverities: known}); err != nil {
		t.Fatalf("DecodeRollup compatibility: %v", err)
	}
}
