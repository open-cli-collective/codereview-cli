package benchmarkcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/mammoth"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type inferMammothFakeOutcome struct {
	choice    mammoth.Choice
	err       error
	attempted bool
	usage     *mammoth.Usage
}

type inferMammothFakeCall struct {
	task  mammoth.Task
	left  mammoth.Record
	right mammoth.Record
}

type inferMammothFakeClient struct {
	t        *testing.T
	outcomes []inferMammothFakeOutcome
	calls    []inferMammothFakeCall
}

func (client *inferMammothFakeClient) Compare(_ context.Context, task mammoth.Task, left, right mammoth.Record) (mammoth.Receipt, error) {
	client.t.Helper()
	client.calls = append(client.calls, inferMammothFakeCall{task: task, left: left, right: right})
	plan, err := mammoth.Prepare(task, left, right)
	if err != nil {
		client.t.Fatalf("mammoth.Prepare for fake call: %v", err)
	}
	index := len(client.calls) - 1
	outcome := inferMammothFakeOutcome{choice: mammoth.ChoiceSame, attempted: true}
	if index < len(client.outcomes) {
		outcome = client.outcomes[index]
	}
	receipt := mammoth.Receipt{
		RequestedModel: mammoth.Model,
		ReturnedModel:  mammoth.Model,
		RequestSHA256:  plan.RequestSHA256,
		ResponseSHA256: sha256Hex([]byte(fmt.Sprintf("synthetic-response-%d", index+1))),
		RequestID:      fmt.Sprintf("request-%d", index+1),
		Duration:       25 * time.Millisecond,
		Attempted:      outcome.attempted,
		HTTPStatus:     200,
		Usage:          outcome.usage,
	}
	if outcome.usage == nil && outcome.err == nil {
		receipt.Usage = &mammoth.Usage{InputTokens: int64(20 + index), OutputTokens: 2}
	}
	if outcome.err == nil {
		if outcome.choice == "" {
			outcome.choice = mammoth.ChoiceSame
		}
		receipt.Answer = &mammoth.Answer{Choice: outcome.choice, Probabilities: [3]float64{0.7, 0.2, 0.1}, Confidence: 0.7}
	}
	return receipt, outcome.err
}

type inferMammothTestState struct {
	client      *inferMammothFakeClient
	keyLookups  int
	clientOpens int
	options     mammoth.Options
}

func inferMammothTestDependencies(t *testing.T, outcomes []inferMammothFakeOutcome, apiKey string, keyPresent bool) (inferMammothDependencies, *inferMammothTestState) {
	t.Helper()
	state := &inferMammothTestState{client: &inferMammothFakeClient{t: t, outcomes: outcomes}}
	dependencies := inferMammothDependencies{
		lookupAPIKey: func() (string, bool) {
			state.keyLookups++
			return apiKey, keyPresent
		},
		newClient: func(options mammoth.Options) (mammothComparer, error) {
			state.clientOpens++
			state.options = options
			return state.client, nil
		},
		now:    func() time.Time { return time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC) },
		rename: os.Rename,
	}
	return dependencies, state
}

func newInferMammothTestCommand(t *testing.T, opts *root.Options, dependencies inferMammothDependencies) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	if opts == nil {
		opts = &root.Options{}
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}
	opts.Stdout = out
	opts.Stderr = errOut
	cmd, rootOpts := root.NewCommandWithOptions(opts)
	root.RegisterAll(cmd, rootOpts, func(rootCmd *cobra.Command, rootOptions *root.Options) {
		benchmark := &cobra.Command{Use: "benchmark", Short: "Benchmark test tree"}
		benchmark.AddCommand(newValidateCommand(rootOptions), newDoctorCommand(rootOptions), newRunCommand(rootOptions), newSelectCommand(rootOptions), newCompareCommand(rootOptions), newReplayCommand(rootOptions), newInferMammothCommandWithDependencies(rootOptions, dependencies))
		rootCmd.AddCommand(benchmark)
	})
	return cmd, out, errOut
}

func writeInferMammothInput(t *testing.T, dir string, input reportidentity.Input) (string, []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll fixture directory: %v", err)
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal fixture: %v", err)
	}
	path := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}
	return path, data
}

func inferMammothCohortInput(count int) reportidentity.Input {
	findings := make([]review.Finding, 0, count)
	for index := 0; index < count; index++ {
		finding := inferMammothFinding(fmt.Sprintf("f%d", index+1), review.SeverityMajor)
		if index == 0 {
			finding.Anchor = review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 22}
			finding.Anchoring = review.AnchoringFileLevelFallback
		}
		findings = append(findings, finding)
	}
	return reportidentity.Input{SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort, Findings: findings, Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{}}
}

func inferMammothPriorInput() reportidentity.Input {
	findingA := inferMammothFinding("f1", review.SeverityMajor)
	findingA.Anchoring = review.AnchoringInline
	return reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskPriorThread,
		Findings:      []review.Finding{findingA, inferMammothFinding("f2", review.SeverityMinor)},
		Threads: []reportidentity.Thread{
			{ID: "t1", Eligible: true, FilePath: "src/thread-a.go", Body: "Synthetic prior comment A."},
			{ID: "t-excluded", Eligible: false, FilePath: "src/thread-x.go", Body: "Synthetic ineligible comment."},
			{ID: "t2", Eligible: true, FilePath: "src/thread-b.go", Body: "Synthetic prior comment B."},
		},
		Decisions: []reportidentity.Decision{},
	}
}

func inferMammothFinding(id string, severity review.Severity) review.Finding {
	return review.Finding{
		ID:       review.FindingID(id),
		Severity: severity,
		FilePath: "src/" + id + ".go",
		Anchor:   review.Anchor{Kind: review.AnchorKindFile},
		Body:     "Synthetic finding text for " + id,
	}
}

func inferMammothRunArgs(fixturePath, outputDir string, maxCalls int) []string {
	return []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", fmt.Sprint(maxCalls), "--output-dir", outputDir}
}

func assertMammothFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}

func readMammothOutput(t *testing.T, outputDir, name string) []byte {
	t.Helper()
	// #nosec G304 -- outputDir is created by this test under t.TempDir.
	data, err := os.ReadFile(filepath.Join(outputDir, name))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return data
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal JSON: %v", err)
	}
	return data
}

func replayPairHash(t *testing.T, input reportidentity.Input, left, right string) string {
	t.Helper()
	result, err := reportidentity.Assemble(reportidentity.Records{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      input.Findings,
		Threads:       input.Threads,
	}, []reportidentity.Decision{})
	if err != nil {
		t.Fatalf("Assemble original pair list: %v", err)
	}
	for _, pair := range result.Pairs {
		if pair.LeftID == left && pair.RightID == right {
			return pair.InputSHA256
		}
	}
	t.Fatalf("pair %s/%s was not returned", left, right)
	return ""
}

func TestInferMammothCohortCallsEveryPairAndPublishesReplayableManifest(t *testing.T) {
	baseDir := t.TempDir()
	input := inferMammothCohortInput(3)
	fixturePath, inputBytes := writeInferMammothInput(t, baseDir, input)
	outputDir := filepath.Join(baseDir, "mammoth-output")
	dependencies, state := inferMammothTestDependencies(t, []inferMammothFakeOutcome{
		{choice: mammoth.ChoiceSame, attempted: true},
		{choice: mammoth.ChoiceDifferent, attempted: true},
		{choice: mammoth.ChoiceAbstain, attempted: true},
	}, "synthetic-test-key", true)
	var renameOrder []string
	dependencies.rename = func(oldPath, newPath string) error {
		renameOrder = append(renameOrder, filepath.Base(newPath))
		return os.Rename(oldPath, newPath)
	}
	cmd, stdout, stderr := newInferMammothTestCommand(t, &root.Options{
		ConfigPath:  filepath.Join(baseDir, "missing-config.yml"),
		CatalogPath: filepath.Join(baseDir, "missing-catalog"),
		Profile:     "missing-profile",
	}, dependencies)
	args := []string{"--profile", "missing-profile", "--catalog", filepath.Join(baseDir, "missing-catalog"), "--backend", "invalid-backend"}
	args = append(args, inferMammothRunArgs(fixturePath, outputDir, 3)...)
	if err := root.Execute(cmd, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if stdout.Len() == 0 || stdout.Bytes()[len(stdout.Bytes())-1] != '\n' || bytes.Count(stdout.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("stdout = %q, want one compact JSON line", stdout.String())
	}
	if strings.Contains(stdout.String(), "Synthetic finding text") || strings.Contains(stdout.String(), "synthetic-test-key") {
		t.Fatalf("stdout contains sensitive input or key: %q", stdout.String())
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
	}
	if summary.Status != "unresolved" || summary.ExpectedPairs != 3 || summary.AttemptedPairs != 3 || summary.ProviderCalls != 3 || summary.CompletedPairs != 3 || summary.AbstainedPairs != 1 || summary.FailedPairs != 0 || summary.MissingPairs != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if state.keyLookups != 1 || state.clientOpens != 1 || len(state.client.calls) != 3 {
		t.Fatalf("key/client/calls = %d/%d/%d, want 1/1/3", state.keyLookups, state.clientOpens, len(state.client.calls))
	}
	if state.options.APIKey != "synthetic-test-key" || state.options.Timeout != defaultMammothTimeout {
		t.Fatalf("client options = %#v", state.options)
	}
	wantPairs := [][2]string{{"f1", "f2"}, {"f1", "f3"}, {"f2", "f3"}}
	wantChoices := []mammoth.Choice{mammoth.ChoiceSame, mammoth.ChoiceDifferent, mammoth.ChoiceAbstain}
	for index, call := range state.client.calls {
		if call.task != mammoth.TaskCohort || call.left.Kind != "finding" || call.right.Kind != "finding" {
			t.Fatalf("call[%d] task/kinds = %q/%q/%q", index, call.task, call.left.Kind, call.right.Kind)
		}
		if call.left.Finding == nil || call.right.Finding == nil || call.left.Finding.ID != wantPairs[index][0] || call.right.Finding.ID != wantPairs[index][1] {
			t.Fatalf("call[%d] IDs = %#v/%#v", index, call.left.Finding, call.right.Finding)
		}
	}
	if got := state.client.calls[0].left.Finding; got.Anchor != (mammoth.Anchor{Kind: "line", Side: "RIGHT", Line: 22}) || got.Anchoring != string(review.AnchoringFileLevelFallback) {
		t.Fatalf("line anchor/anchoring mapping = %#v", got)
	}

	assertMammothFileMode(t, outputDir, 0o700)
	for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
		assertMammothFileMode(t, filepath.Join(outputDir, name), 0o600)
	}
	fixtureBytes := readMammothOutput(t, outputDir, "fixture.json")
	replayBytes := readMammothOutput(t, outputDir, "replay.json")
	receiptsBytes := readMammothOutput(t, outputDir, "receipts.json")
	manifestBytes := readMammothOutput(t, outputDir, "manifest.json")
	var manifest inferMammothManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("Unmarshal manifest: %v", err)
	}
	if manifest.SchemaVersion != inferMammothSchemaVersion || manifest.Files.FixtureSHA256 != sha256Hex(fixtureBytes) || manifest.Files.ReplaySHA256 != sha256Hex(replayBytes) || manifest.Files.ReceiptsSHA256 != sha256Hex(receiptsBytes) {
		t.Fatalf("manifest hashes = %#v", manifest)
	}
	if !reflect.DeepEqual(renameOrder, []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"}) {
		t.Fatalf("publication order = %#v, want manifest last", renameOrder)
	}
	if summary.FixturePath != filepath.Join(outputDir, "fixture.json") || summary.ReplayPath != filepath.Join(outputDir, "replay.json") || summary.ReceiptsPath != filepath.Join(outputDir, "receipts.json") || summary.ManifestPath != filepath.Join(outputDir, "manifest.json") {
		t.Fatalf("summary paths = %#v", summary)
	}
	if !bytes.Equal(fixtureBytes, mustMarshalJSON(t, reportidentity.Input{
		SchemaVersion: input.SchemaVersion,
		Task:          input.Task,
		Findings:      input.Findings,
		Threads:       input.Threads,
		Decisions: []reportidentity.Decision{
			{LeftID: "f1", RightID: "f2", Choice: reportidentity.ChoiceSame, InputSHA256: replayPairHash(t, input, "f1", "f2")},
			{LeftID: "f1", RightID: "f3", Choice: reportidentity.ChoiceDifferent, InputSHA256: replayPairHash(t, input, "f1", "f3")},
			{LeftID: "f2", RightID: "f3", Choice: reportidentity.ChoiceAbstain, InputSHA256: replayPairHash(t, input, "f2", "f3")},
		},
	})) {
		t.Fatalf("generated fixture does not contain expected decisions: %s", fixtureBytes)
	}
	if strings.Contains(string(receiptsBytes), "Synthetic finding text") || strings.Contains(string(receiptsBytes), "src/f1.go") || strings.Contains(string(receiptsBytes), "synthetic-test-key") {
		t.Fatalf("receipts contain report text, paths, or key: %s", receiptsBytes)
	}
	var receipts inferMammothReceipts
	if err := json.Unmarshal(receiptsBytes, &receipts); err != nil {
		t.Fatalf("Unmarshal receipts: %v", err)
	}
	if receipts.InputSHA256 != sha256Hex(inputBytes) || receipts.FindingCount != 3 || receipts.ThreadCount != 0 || receipts.KnownInputTokens != 63 || receipts.KnownOutputTokens != 6 || receipts.UnknownCostCalls != 0 || len(receipts.Attempts) != 3 {
		t.Fatalf("receipts summary = %#v", receipts)
	}
	for index, attempt := range receipts.Attempts {
		if attempt.PlannedRequestSHA256 == "" || attempt.RequestSHA256 != attempt.PlannedRequestSHA256 || attempt.Answer == nil || attempt.Answer.Choice != reportidentity.Choice(wantChoices[index]) {
			t.Fatalf("attempt[%d] = %#v", index, attempt)
		}
	}

	var generatedInput reportidentity.Input
	if _, err := reportidentity.Decode(fixtureBytes); err != nil {
		t.Fatalf("Decode generated fixture: %v", err)
	}
	if err := json.Unmarshal(fixtureBytes, &generatedInput); err != nil {
		t.Fatalf("Unmarshal generated fixture: %v", err)
	}
	if !reflect.DeepEqual(generatedInput.Findings, input.Findings) || !reflect.DeepEqual(generatedInput.Threads, input.Threads) {
		t.Fatalf("generated fixture changed originals: %#v", generatedInput)
	}
	replayCmd, replayOut, replayErr := newInferMammothTestCommand(t, nil, dependencies)
	if err := root.Execute(replayCmd, []string{"benchmark", "replay", filepath.Join(outputDir, "fixture.json")}); err != nil {
		t.Fatalf("Replay generated fixture: %v", err)
	}
	if replayErr.Len() != 0 || !bytes.Equal(bytes.TrimSuffix(replayOut.Bytes(), []byte{'\n'}), replayBytes) {
		t.Fatalf("separate replay differs from replay.json; stderr=%q", replayErr.String())
	}
}

func TestInferMammothPriorThreadsCallsOnlyEligiblePairsAndKeepsAllThreads(t *testing.T) {
	baseDir := t.TempDir()
	input := inferMammothPriorInput()
	fixturePath, _ := writeInferMammothInput(t, baseDir, input)
	outputDir := filepath.Join(baseDir, "prior-output")
	dependencies, state := inferMammothTestDependencies(t, []inferMammothFakeOutcome{
		{choice: mammoth.ChoiceSame, attempted: true},
		{choice: mammoth.ChoiceSame, attempted: true},
		{choice: mammoth.ChoiceDifferent, attempted: true},
		{choice: mammoth.ChoiceAbstain, attempted: true},
	}, "synthetic-test-key", true)
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	args := append(inferMammothRunArgs(fixturePath, outputDir, 4), "--timeout", "2s")
	if err := root.Execute(cmd, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if state.options.Timeout != 2*time.Second {
		t.Fatalf("client timeout = %s, want 2s", state.options.Timeout)
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
	}
	if summary.Status != "unresolved" || summary.ExpectedPairs != 4 || summary.AttemptedPairs != 4 || summary.CompletedPairs != 4 || summary.AbstainedPairs != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	wantPairs := [][2]string{{"f1", "t1"}, {"f1", "t2"}, {"f2", "t1"}, {"f2", "t2"}}
	for index, call := range state.client.calls {
		if call.task != mammoth.TaskPriorThreads || call.left.Kind != "finding" || call.right.Kind != "thread" || call.left.Finding == nil || call.right.Thread == nil || call.right.Finding != nil {
			t.Fatalf("call[%d] = %#v", index, call)
		}
		if call.left.Finding.ID != wantPairs[index][0] || call.right.Thread.ID != wantPairs[index][1] || !call.right.Thread.Eligible {
			t.Fatalf("call[%d] IDs/eligibility = %s/%s/%v", index, call.left.Finding.ID, call.right.Thread.ID, call.right.Thread.Eligible)
		}
	}
	if state.client.calls[0].left.Finding.Anchoring != string(review.AnchoringInline) {
		t.Fatalf("finding anchoring = %q, want preserved inline", state.client.calls[0].left.Finding.Anchoring)
	}
	for _, call := range state.client.calls {
		if call.right.Thread == nil || strings.Contains(call.right.Kind, "finding") {
			t.Fatalf("thread record acquired finding attributes: %#v", call.right)
		}
	}

	fixtureBytes := readMammothOutput(t, outputDir, "fixture.json")
	var generated reportidentity.Input
	if err := json.Unmarshal(fixtureBytes, &generated); err != nil {
		t.Fatalf("Unmarshal generated fixture: %v", err)
	}
	if !reflect.DeepEqual(generated.Findings, input.Findings) || !reflect.DeepEqual(generated.Threads, input.Threads) {
		t.Fatalf("generated fixture dropped or changed original records: %#v", generated)
	}
	if len(generated.Threads) != 3 || generated.Threads[1].ID != "t-excluded" || generated.Threads[1].Eligible {
		t.Fatalf("ineligible original thread was not retained: %#v", generated.Threads)
	}
	replayBytes := readMammothOutput(t, outputDir, "replay.json")
	var result reportidentity.Result
	if err := json.Unmarshal(replayBytes, &result); err != nil {
		t.Fatalf("Unmarshal replay: %v", err)
	}
	if !reflect.DeepEqual(result.Associations, []reportidentity.Association{{FindingID: "f1", ThreadID: "t1"}, {FindingID: "f1", ThreadID: "t2"}}) {
		t.Fatalf("many-to-many associations = %#v", result.Associations)
	}
}

func TestInferMammothZeroPairTasksNeedNoKeyOrClient(t *testing.T) {
	prior := inferMammothPriorInput()
	prior.Findings = []review.Finding{inferMammothFinding("f1", review.SeverityMajor)}
	prior.Threads = []reportidentity.Thread{{ID: "only-ineligible", Eligible: false, FilePath: "src/old.go", Body: "Synthetic excluded thread."}}
	cases := []struct {
		name  string
		input reportidentity.Input
	}{
		{name: "empty cohort", input: reportidentity.Input{SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort, Findings: []review.Finding{}, Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{}}},
		{name: "singleton cohort", input: reportidentity.Input{SchemaVersion: reportidentity.SchemaVersion, Task: reportidentity.TaskCohort, Findings: []review.Finding{inferMammothFinding("single", review.SeverityMinor)}, Threads: []reportidentity.Thread{}, Decisions: []reportidentity.Decision{}}},
		{name: "prior threads without eligible targets", input: prior},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			baseDir := t.TempDir()
			fixturePath, _ := writeInferMammothInput(t, baseDir, test.input)
			outputDir := filepath.Join(baseDir, "zero-output")
			dependencies, state := inferMammothTestDependencies(t, nil, "", false)
			cmd, stdout, stderr := newInferMammothTestCommand(t, nil, dependencies)
			if err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 0)); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			var summary inferMammothSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
			}
			if stderr.Len() != 0 || summary.Status != "complete" || summary.ExpectedPairs != 0 || summary.AttemptedPairs != 0 || summary.ProviderCalls != 0 {
				t.Fatalf("stderr/summary = %q/%#v", stderr.String(), summary)
			}
			if state.keyLookups != 0 || state.clientOpens != 0 || len(state.client.calls) != 0 {
				t.Fatalf("zero-pair task performed work: key/client/calls=%d/%d/%d", state.keyLookups, state.clientOpens, len(state.client.calls))
			}
			for _, name := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
				if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
					t.Fatalf("Stat(%s): %v", name, err)
				}
			}
		})
	}
}

func TestInferMammothPreflightFailuresRetainOriginalsWithoutCallingClient(t *testing.T) {
	oversized := inferMammothCohortInput(2)
	oversized.Findings[0].Body = strings.Repeat("x", 7500)
	cases := []struct {
		name        string
		input       reportidentity.Input
		maxCalls    int
		apiKey      string
		keyPresent  bool
		wantCode    string
		wantLookups int
	}{
		{name: "missing key", input: inferMammothCohortInput(2), maxCalls: 1, wantCode: "auth", wantLookups: 1},
		{name: "maximum calls too small", input: inferMammothCohortInput(3), maxCalls: 2, apiKey: "unused-key", keyPresent: true, wantCode: "invalid_input"},
		{name: "request exceeds local byte bound", input: oversized, maxCalls: 1, apiKey: "unused-key", keyPresent: true, wantCode: "context_limit"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			baseDir := t.TempDir()
			fixturePath, inputBytes := writeInferMammothInput(t, baseDir, test.input)
			outputDir := filepath.Join(baseDir, "preflight-output")
			dependencies, state := inferMammothTestDependencies(t, nil, test.apiKey, test.keyPresent)
			cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
			err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, test.maxCalls))
			if exitcode.FromError(err) != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage 2 (err %v)", exitcode.FromError(err), err)
			}
			var summary inferMammothSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
			}
			if summary.Status != "error" || summary.ExpectedPairs == 0 || summary.AttemptedPairs != 0 || summary.ProviderCalls != 0 || summary.MissingPairs != summary.ExpectedPairs {
				t.Fatalf("preflight summary = %#v", summary)
			}
			if state.keyLookups != test.wantLookups || state.clientOpens != 0 || len(state.client.calls) != 0 {
				t.Fatalf("preflight invoked key/client: %d/%d/%d", state.keyLookups, state.clientOpens, len(state.client.calls))
			}
			receiptsBytes := readMammothOutput(t, outputDir, "receipts.json")
			var receipts inferMammothReceipts
			if err := json.Unmarshal(receiptsBytes, &receipts); err != nil {
				t.Fatalf("Unmarshal preflight receipts: %v", err)
			}
			foundCode := false
			for _, failure := range receipts.PreflightErrors {
				if failure.ErrorCode == test.wantCode {
					foundCode = true
				}
			}
			if !foundCode || receipts.InputSHA256 != sha256Hex(inputBytes) {
				t.Fatalf("preflight receipts = %#v, want code %q and input digest", receipts, test.wantCode)
			}
			fixtureBytes := readMammothOutput(t, outputDir, "fixture.json")
			var generated reportidentity.Input
			if err := json.Unmarshal(fixtureBytes, &generated); err != nil {
				t.Fatalf("Unmarshal retained fixture: %v", err)
			}
			if !reflect.DeepEqual(generated.Findings, test.input.Findings) || !reflect.DeepEqual(generated.Threads, test.input.Threads) || len(generated.Decisions) != 0 {
				t.Fatalf("preflight fixture changed originals or recorded decisions: %#v", generated)
			}
			if strings.Contains(stdout.String(), test.input.Findings[0].Body) {
				t.Fatal("stdout contains original report body")
			}
		})
	}
}

func TestInferMammothStopsAtFirstProviderErrorAndMarksLaterPairsMissing(t *testing.T) {
	baseDir := t.TempDir()
	input := inferMammothCohortInput(4)
	fixturePath, _ := writeInferMammothInput(t, baseDir, input)
	outputDir := filepath.Join(baseDir, "failed-output")
	dependencies, state := inferMammothTestDependencies(t, []inferMammothFakeOutcome{
		{choice: mammoth.ChoiceSame, attempted: true, usage: &mammoth.Usage{InputTokens: 30, OutputTokens: 4}},
		{err: errors.New("RAW-MAMMOTH-RESPONSE-SENTINEL"), attempted: true},
	}, "synthetic-test-key", true)
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 6))
	if exitcode.FromError(err) != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure 1 (err %v)", exitcode.FromError(err), err)
	}
	if len(state.client.calls) != 2 {
		t.Fatalf("Compare calls = %d, want stop after second pair", len(state.client.calls))
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
	}
	if summary.Status != "error" || summary.ExpectedPairs != 6 || summary.AttemptedPairs != 2 || summary.ProviderCalls != 2 || summary.CompletedPairs != 1 || summary.FailedPairs != 1 || summary.MissingPairs != 4 || summary.UnknownCostCalls != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if strings.Contains(stdout.String(), "RAW-MAMMOTH-RESPONSE-SENTINEL") || strings.Contains(err.Error(), "RAW-MAMMOTH-RESPONSE-SENTINEL") {
		t.Fatal("raw client error escaped the sanitized command boundary")
	}
	receiptsBytes := readMammothOutput(t, outputDir, "receipts.json")
	if strings.Contains(string(receiptsBytes), "RAW-MAMMOTH-RESPONSE-SENTINEL") {
		t.Fatal("raw client error was written to receipts")
	}
	var receipts inferMammothReceipts
	if err := json.Unmarshal(receiptsBytes, &receipts); err != nil {
		t.Fatalf("Unmarshal receipts: %v", err)
	}
	if len(receipts.Attempts) != 2 || receipts.Attempts[0].Answer == nil || receipts.Attempts[0].Answer.Choice != reportidentity.ChoiceSame || receipts.Attempts[1].ErrorCode != "transport" {
		t.Fatalf("attempts = %#v", receipts.Attempts)
	}
	var generated reportidentity.Input
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "fixture.json"), &generated); err != nil {
		t.Fatalf("Unmarshal retained fixture: %v", err)
	}
	if len(generated.Decisions) != 2 || generated.Decisions[0].Choice != reportidentity.ChoiceSame || generated.Decisions[1].Choice != reportidentity.ChoiceError {
		t.Fatalf("recorded decisions = %#v", generated.Decisions)
	}
	var replay reportidentity.Result
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "replay.json"), &replay); err != nil {
		t.Fatalf("Unmarshal replay: %v", err)
	}
	if replay.Status != "error" || replay.Pairs[0].Choice != reportidentity.ChoiceSame || replay.Pairs[1].Choice != reportidentity.ChoiceError {
		t.Fatalf("replay status/pairs = %q/%#v", replay.Status, replay.Pairs)
	}
	for _, pair := range replay.Pairs[2:] {
		if pair.Choice != reportidentity.ChoiceMissing {
			t.Fatalf("unattempted pair = %#v, want missing", pair)
		}
	}
}

func TestInferMammothSeparatesPairAttemptFromHTTPCall(t *testing.T) {
	baseDir := t.TempDir()
	fixturePath, _ := writeInferMammothInput(t, baseDir, inferMammothCohortInput(2))
	outputDir := filepath.Join(baseDir, "unattempted-http-output")
	dependencies, _ := inferMammothTestDependencies(t, []inferMammothFakeOutcome{{err: errors.New("synthetic pre-HTTP cancellation"), attempted: false}}, "synthetic-test-key", true)
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 1))
	if exitcode.FromError(err) != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure 1 (err %v)", exitcode.FromError(err), err)
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v", err)
	}
	if summary.AttemptedPairs != 1 || summary.ProviderCalls != 0 || summary.FailedPairs != 1 || summary.UnknownCostCalls != 0 {
		t.Fatalf("summary = %#v, want one pair attempt and zero HTTP calls", summary)
	}
	var receipts inferMammothReceipts
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("Unmarshal receipts: %v", err)
	}
	if len(receipts.Attempts) != 1 || receipts.Attempts[0].HTTPAttempted || receipts.Attempts[0].Usage != nil {
		t.Fatalf("attempt receipt = %#v", receipts.Attempts)
	}
}

func TestInferMammothCancellationReachesActiveComparisonAndRetainsMissingPairs(t *testing.T) {
	type cancelingClient struct {
		started chan struct{}
		calls   []inferMammothFakeCall
	}
	client := &cancelingClient{started: make(chan struct{})}
	clientCompare := func(ctx context.Context, task mammoth.Task, left, right mammoth.Record) (mammoth.Receipt, error) {
		client.calls = append(client.calls, inferMammothFakeCall{task: task, left: left, right: right})
		plan, err := mammoth.Prepare(task, left, right)
		if err != nil {
			t.Fatalf("Prepare cancellation fixture: %v", err)
		}
		close(client.started)
		<-ctx.Done()
		return mammoth.Receipt{RequestedModel: mammoth.Model, RequestSHA256: plan.RequestSHA256, Attempted: true, Duration: 15 * time.Millisecond}, ctx.Err()
	}
	dependencies, state := inferMammothTestDependencies(t, nil, "synthetic-test-key", true)
	dependencies.newClient = func(options mammoth.Options) (mammothComparer, error) {
		state.clientOpens++
		state.options = options
		return mammothCompareFunc(clientCompare), nil
	}
	baseDir := t.TempDir()
	fixturePath, _ := writeInferMammothInput(t, baseDir, inferMammothCohortInput(3))
	outputDir := filepath.Join(baseDir, "canceled-output")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	cmd.SetContext(ctx)
	go func() {
		<-client.started
		cancel()
	}()
	err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 3))
	if exitcode.FromError(err) != exitcode.Failure {
		t.Fatalf("exit code = %d, want failure 1 (err %v)", exitcode.FromError(err), err)
	}
	if len(client.calls) != 1 || state.keyLookups != 1 || state.clientOpens != 1 {
		t.Fatalf("canceled comparison counts = calls:%d keys:%d clients:%d", len(client.calls), state.keyLookups, state.clientOpens)
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v\n%s", err, stdout.String())
	}
	if summary.Status != "error" || summary.AttemptedPairs != 1 || summary.ProviderCalls != 1 || summary.FailedPairs != 1 || summary.MissingPairs != 2 {
		t.Fatalf("canceled summary = %#v", summary)
	}
	var receipts inferMammothReceipts
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "receipts.json"), &receipts); err != nil {
		t.Fatalf("Unmarshal cancellation receipts: %v", err)
	}
	if len(receipts.Attempts) != 1 || receipts.Attempts[0].ErrorCode != "cancelled" { //nolint:misspell // Stable error category from internal/mammoth.
		t.Fatalf("canceled attempts = %#v", receipts.Attempts)
	}
	var generated reportidentity.Input
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "fixture.json"), &generated); err != nil {
		t.Fatalf("Unmarshal cancellation fixture: %v", err)
	}
	if len(generated.Decisions) != 1 || generated.Decisions[0].Choice != reportidentity.ChoiceError {
		t.Fatalf("canceled decisions = %#v", generated.Decisions)
	}
}

type mammothCompareFunc func(context.Context, mammoth.Task, mammoth.Record, mammoth.Record) (mammoth.Receipt, error)

func (compare mammothCompareFunc) Compare(ctx context.Context, task mammoth.Task, left, right mammoth.Record) (mammoth.Receipt, error) {
	return compare(ctx, task, left, right)
}

func TestInferMammothCohortContradictoryTriangleStaysUnresolved(t *testing.T) {
	baseDir := t.TempDir()
	fixturePath, _ := writeInferMammothInput(t, baseDir, inferMammothCohortInput(3))
	outputDir := filepath.Join(baseDir, "triangle-output")
	dependencies, _ := inferMammothTestDependencies(t, []inferMammothFakeOutcome{
		{choice: mammoth.ChoiceSame, attempted: true},
		{choice: mammoth.ChoiceSame, attempted: true},
		{choice: mammoth.ChoiceDifferent, attempted: true},
	}, "synthetic-test-key", true)
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	if err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 3)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var summary inferMammothSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("Unmarshal summary: %v", err)
	}
	if summary.Status != "unresolved" || summary.FailedPairs != 0 || summary.MissingPairs != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	var replay reportidentity.Result
	if err := json.Unmarshal(readMammothOutput(t, outputDir, "replay.json"), &replay); err != nil {
		t.Fatalf("Unmarshal replay: %v", err)
	}
	if replay.Status != "unresolved" || len(replay.Groups) != 3 || !reflect.DeepEqual(replay.Issues, []reportidentity.Issue{{Code: "contradiction", LeftID: "f2", RightID: "f3"}}) {
		t.Fatalf("contradictory replay = %#v", replay)
	}
}

func TestInferMammothInvalidInputsAndFlagsDoNotCreateOutput(t *testing.T) {
	baseDir := t.TempDir()
	input := inferMammothCohortInput(2)
	fixturePath, _ := writeInferMammothInput(t, baseDir, input)
	_, err := reportidentity.Assemble(reportidentity.Records{SchemaVersion: input.SchemaVersion, Task: input.Task, Findings: input.Findings, Threads: input.Threads}, []reportidentity.Decision{})
	if err != nil {
		t.Fatalf("Assemble pair hash fixture: %v", err)
	}
	validHash := replayPairHash(t, input, "f1", "f2")
	withDecision := input
	withDecision.Decisions = []reportidentity.Decision{{LeftID: "f1", RightID: "f2", Choice: reportidentity.ChoiceSame, InputSHA256: validHash}}
	decisionPath, _ := writeInferMammothInput(t, filepath.Join(baseDir, "with-decision"), withDecision)
	invalidJSONPath := filepath.Join(baseDir, "invalid.json")
	if err := os.WriteFile(invalidJSONPath, []byte(`{"schema_version":1,"task":"cohort","findings":null,"threads":[],"decisions":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile invalid JSON: %v", err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{name: "missing max-calls", args: []string{"benchmark", "infer-mammoth", fixturePath, "--output-dir"}},
		{name: "missing output directory", args: []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", "1"}},
		{name: "wrong fixture argument count", args: []string{"benchmark", "infer-mammoth", fixturePath, fixturePath, "--max-calls", "1", "--output-dir", filepath.Join(baseDir, "wrong-args")}},
		{name: "negative max-calls", args: []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", "-1", "--output-dir", filepath.Join(baseDir, "negative")}},
		{name: "max-calls above limit", args: []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", "10001", "--output-dir", filepath.Join(baseDir, "too-many")}},
		{name: "timeout out of range", args: []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", "1", "--timeout", "0s", "--output-dir", filepath.Join(baseDir, "timeout")}},
		{name: "unsupported output format flag", args: []string{"benchmark", "infer-mammoth", fixturePath, "--max-calls", "1", "--json", "--output-dir", filepath.Join(baseDir, "json-flag")}},
		{name: "fixture already has decisions", args: []string{"benchmark", "infer-mammoth", decisionPath, "--max-calls", "1", "--output-dir", filepath.Join(baseDir, "existing-decisions")}},
		{name: "malformed JSON", args: []string{"benchmark", "infer-mammoth", invalidJSONPath, "--max-calls", "1", "--output-dir", filepath.Join(baseDir, "bad-json")}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dependencies, state := inferMammothTestDependencies(t, nil, "synthetic-test-key", true)
			cmd, _, _ := newInferMammothTestCommand(t, nil, dependencies)
			err := root.Execute(cmd, test.args)
			if exitcode.FromError(err) != exitcode.UsageError {
				t.Fatalf("Execute error = %v (exit %d), want usage", err, exitcode.FromError(err))
			}
			outputDir := ""
			for index, argument := range test.args {
				if argument == "--output-dir" && index+1 < len(test.args) {
					outputDir = test.args[index+1]
				}
			}
			if outputDir != "" {
				if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid invocation created output directory %q (Lstat error %v)", outputDir, err)
				}
			}
			if state.keyLookups != 0 || state.clientOpens != 0 || len(state.client.calls) != 0 {
				t.Fatalf("invalid invocation touched inference: key/client/calls=%d/%d/%d", state.keyLookups, state.clientOpens, len(state.client.calls))
			}
		})
	}
}

func TestInferMammothDoesNotOverwriteExistingOutputDirectoryOrFollowSymlink(t *testing.T) {
	baseDir := t.TempDir()
	fixturePath, _ := writeInferMammothInput(t, baseDir, inferMammothCohortInput(2))
	existing := filepath.Join(baseDir, "existing-output")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatalf("Mkdir existing output: %v", err)
	}
	sentinel := filepath.Join(existing, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}
	dependencies, state := inferMammothTestDependencies(t, nil, "synthetic-test-key", true)
	cmd, _, _ := newInferMammothTestCommand(t, nil, dependencies)
	err := root.Execute(cmd, inferMammothRunArgs(fixturePath, existing, 1))
	if exitcode.FromError(err) != exitcode.UsageError {
		t.Fatalf("existing output error = %v (exit %d), want usage", err, exitcode.FromError(err))
	}
	// #nosec G304 -- sentinel is created by this test under t.TempDir.
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
		t.Fatalf("existing sentinel = %q, %v", got, err)
	}
	if state.keyLookups != 0 || state.clientOpens != 0 || len(state.client.calls) != 0 {
		t.Fatalf("existing output path touched inference: %d/%d/%d", state.keyLookups, state.clientOpens, len(state.client.calls))
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires a Unix-like filesystem")
	}
	target := filepath.Join(baseDir, "symlink-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir symlink target: %v", err)
	}
	link := filepath.Join(baseDir, "output-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Symlink unavailable: %v", err)
	}
	dependencies, state = inferMammothTestDependencies(t, nil, "synthetic-test-key", true)
	cmd, _, _ = newInferMammothTestCommand(t, nil, dependencies)
	err = root.Execute(cmd, inferMammothRunArgs(fixturePath, link, 1))
	if exitcode.FromError(err) != exitcode.UsageError {
		t.Fatalf("symlink output error = %v (exit %d), want usage", err, exitcode.FromError(err))
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target entries = %#v, %v; want empty", entries, err)
	}
	if state.keyLookups != 0 || state.clientOpens != 0 || len(state.client.calls) != 0 {
		t.Fatalf("symlink output path touched inference: %d/%d/%d", state.keyLookups, state.clientOpens, len(state.client.calls))
	}
}

func TestInferMammothPartialPublicationLeavesManifestAbsent(t *testing.T) {
	baseDir := t.TempDir()
	fixturePath, _ := writeInferMammothInput(t, baseDir, inferMammothCohortInput(2))
	outputDir := filepath.Join(baseDir, "partial-output")
	dependencies, state := inferMammothTestDependencies(t, []inferMammothFakeOutcome{{choice: mammoth.ChoiceSame, attempted: true}}, "synthetic-test-key", true)
	var renameOrder []string
	dependencies.rename = func(oldPath, newPath string) error {
		name := filepath.Base(newPath)
		renameOrder = append(renameOrder, name)
		if name == "receipts.json" {
			return errors.New("synthetic rename failure")
		}
		return os.Rename(oldPath, newPath)
	}
	cmd, stdout, _ := newInferMammothTestCommand(t, nil, dependencies)
	err := root.Execute(cmd, inferMammothRunArgs(fixturePath, outputDir, 1))
	if exitcode.FromError(err) != exitcode.Failure {
		t.Fatalf("publication error = %v (exit %d), want failure", err, exitcode.FromError(err))
	}
	if stdout.Len() != 0 || len(state.client.calls) != 1 || !reflect.DeepEqual(renameOrder, []string{"fixture.json", "replay.json", "receipts.json"}) {
		t.Fatalf("publication state stdout/calls/order = %q/%d/%#v", stdout.String(), len(state.client.calls), renameOrder)
	}
	for _, name := range []string{"fixture.json", "replay.json"} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("Stat published %s: %v", name, err)
		}
	}
	for _, name := range []string{"receipts.json", "manifest.json"} {
		if _, err := os.Lstat(filepath.Join(outputDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial output contains %s (Lstat error %v)", name, err)
		}
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("ReadDir partial output: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file leaked after failed publication: %s", entry.Name())
		}
	}
}
