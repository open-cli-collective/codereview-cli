package benchmarkcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestReplayCommandIsOfflineAndOnlyReadsItsExplicitFixture(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("CR_PROFILE", "missing-profile")
	t.Setenv("OPENAI_API_KEY", "unused-synthetic-value")
	fixture := writeReplayInput(t, filepath.Join(temp, "fixture.json"), reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskCohort,
		Findings: []review.Finding{
			replayFinding("f1", review.SeverityMajor),
			replayFinding("f2", review.SeverityMinor),
		},
		Threads:   []reportidentity.Thread{},
		Decisions: []reportidentity.Decision{},
	})

	missingConfig := filepath.Join(temp, "no-config.yml")
	missingCatalog := filepath.Join(temp, "no-catalog")
	cmd, out, errOut, opts := newReplayCommandForTest(t, &root.Options{
		ConfigPath:  missingConfig,
		CatalogPath: missingCatalog,
		Profile:     "missing-profile",
	})
	args := []string{"--profile", "another-missing-profile", "--catalog", missingCatalog, "--" + credstore.BackendFlagName, "invalid-backend", "benchmark", "replay", fixture}
	if err := root.Execute(cmd, args); err != nil {
		t.Fatalf("Execute replay with invalid inherited state: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" || !strings.HasPrefix(out.String(), "{") || !strings.HasSuffix(out.String(), "}\n") {
		t.Fatalf("stdout = %q, want one compact JSON object plus newline", out.String())
	}
	var result reportidentity.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal result: %v\n%s", err, out.String())
	}
	if result.Status != "unresolved" || len(result.Pairs) != 1 || result.Pairs[0].Choice != reportidentity.ChoiceMissing {
		t.Fatalf("result = %#v, want unresolved one-pair result", result)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty on unresolved success", errOut.String())
	}
	if opts.Profile != "another-missing-profile" {
		t.Fatalf("root parsed profile = %q, want supplied inherited flag", opts.Profile)
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatalf("ReadDir fixture directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(fixture) {
		t.Fatalf("replay created unexpected files: %#v", entries)
	}
}

func TestReplayCommandErrorAndInvalidInputExitCodes(t *testing.T) {
	records := reportidentity.Records{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskCohort,
		Findings:      []review.Finding{replayFinding("f1", review.SeverityMajor), replayFinding("f2", review.SeverityMinor)},
		Threads:       []reportidentity.Thread{},
	}
	empty, err := reportidentity.Assemble(records, []reportidentity.Decision{})
	if err != nil {
		t.Fatalf("Assemble empty decisions: %v", err)
	}
	input := reportidentity.Input{
		SchemaVersion: records.SchemaVersion,
		Task:          records.Task,
		Findings:      records.Findings,
		Threads:       records.Threads,
		Decisions: []reportidentity.Decision{{
			LeftID:      empty.Pairs[0].LeftID,
			RightID:     empty.Pairs[0].RightID,
			Choice:      reportidentity.ChoiceError,
			InputSHA256: empty.Pairs[0].InputSHA256,
		}},
	}
	temp := t.TempDir()
	errorFixture := writeReplayInput(t, filepath.Join(temp, "recorded-error.json"), input)
	cmd, out, errOut, _ := newReplayCommandForTest(t, nil)
	err = root.Execute(cmd, []string{"benchmark", "replay", errorFixture})
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("recorded error exit = %d, want 1 (err %v)", got, err)
	}
	var result reportidentity.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("recorded error stdout is not full JSON result: %v\n%s", err, out.String())
	}
	if result.Status != "error" || !strings.Contains(err.Error(), "recorded comparisons failed") {
		t.Fatalf("recorded error status/error = %q/%v", result.Status, err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("command-level stderr = %q; process entrypoint owns returned-error diagnostics", errOut.String())
	}

	invalidFixture := filepath.Join(temp, "invalid.json")
	if err := os.WriteFile(invalidFixture, []byte(`{"schema_version":1,"task":"cohort","findings":null,"threads":[],"decisions":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile invalid fixture: %v", err)
	}
	cmd, out, _, _ = newReplayCommandForTest(t, nil)
	err = root.Execute(cmd, []string{"benchmark", "replay", invalidFixture})
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("invalid fixture exit = %d, want 2 (err %v)", got, err)
	}
	if out.Len() != 0 {
		t.Fatalf("invalid fixture stdout = %q, want no result", out.String())
	}
	for _, test := range []struct {
		name   string
		anchor string
	}{
		{name: "empty side", anchor: `{"kind":"file","side":""}`},
		{name: "zero line", anchor: `{"kind":"file","line":0}`},
		{name: "both zero fields", anchor: `{"kind":"file","side":"","line":0}`},
	} {
		path := filepath.Join(temp, "invalid-anchor-"+strings.ReplaceAll(test.name, " ", "-")+".json")
		data := `{"schema_version":1,"task":"cohort","findings":[{"finding_id":"f1","severity":"major","file_path":"a.go","anchor":` + test.anchor + `,"body":"Synthetic finding."}],"threads":[],"decisions":[]}`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("WriteFile invalid anchor fixture: %v", err)
		}
		cmd, out, _, _ = newReplayCommandForTest(t, nil)
		err = root.Execute(cmd, []string{"benchmark", "replay", path})
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("invalid file-anchor exit = %d, want 2 (err %v)", got, err)
		}
		if out.Len() != 0 {
			t.Fatalf("invalid file-anchor stdout = %q, want empty", out.String())
		}
	}

	cmd, out, _, _ = newReplayCommandForTest(t, nil)
	err = root.Execute(cmd, []string{"benchmark", "replay", filepath.Join(temp, "missing.json")})
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("unreadable fixture exit = %d, want 1 (err %v)", got, err)
	}
	if out.Len() != 0 {
		t.Fatalf("unreadable fixture stdout = %q, want empty", out.String())
	}
}

func TestReplaySyntheticExamples(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fixtureDir := filepath.Join(filepath.Dir(source), "testdata", "replay")
	for _, test := range []struct {
		name           string
		wantStatus     string
		wantExit       int
		wantPairs      int
		wantAssociates int
	}{
		{name: "complete-cohort.json", wantStatus: "complete", wantExit: exitcode.Success, wantPairs: 3},
		{name: "prior-thread-unresolved.json", wantStatus: "unresolved", wantExit: exitcode.Success, wantPairs: 2, wantAssociates: 1},
		{name: "recorded-error.json", wantStatus: "error", wantExit: exitcode.Failure, wantPairs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd, out, _, _ := newReplayCommandForTest(t, nil)
			err := root.Execute(cmd, []string{"benchmark", "replay", filepath.Join(fixtureDir, test.name)})
			if got := exitcode.FromError(err); got != test.wantExit {
				t.Fatalf("exit code = %d, want %d (err %v)", got, test.wantExit, err)
			}
			var result reportidentity.Result
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("Unmarshal result: %v\n%s", err, out.String())
			}
			if result.Status != test.wantStatus || len(result.Pairs) != test.wantPairs || len(result.Associations) != test.wantAssociates {
				t.Fatalf("result status/pairs/associations = %q/%d/%d", result.Status, len(result.Pairs), len(result.Associations))
			}
		})
	}
}

func TestReplayCommandRejectsFlagsAndWrongArity(t *testing.T) {
	fixture := writeReplayInput(t, filepath.Join(t.TempDir(), "fixture.json"), reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskCohort,
		Findings:      []review.Finding{},
		Threads:       []reportidentity.Thread{},
		Decisions:     []reportidentity.Decision{},
	})
	for _, args := range [][]string{
		{"benchmark", "replay"},
		{"benchmark", "replay", fixture, fixture},
		{"benchmark", "replay", fixture, "--json"},
		{"benchmark", "replay", fixture, "--output", filepath.Join(t.TempDir(), "out.json")},
	} {
		cmd, out, _, _ := newReplayCommandForTest(t, nil)
		err := root.Execute(cmd, args)
		if exitcode.FromError(err) != exitcode.UsageError {
			t.Errorf("Execute(%q) error = %v (exit %d), want usage", args, err, exitcode.FromError(err))
		}
		if out.Len() != 0 {
			t.Errorf("Execute(%q) stdout = %q, want empty", args, out.String())
		}
	}
}

func TestReplayCommandReportsStdoutWriteFailure(t *testing.T) {
	fixture := writeReplayInput(t, filepath.Join(t.TempDir(), "fixture.json"), reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskCohort,
		Findings:      []review.Finding{},
		Threads:       []reportidentity.Thread{},
		Decisions:     []reportidentity.Decision{},
	})
	cmd := newReplayRootWithOptions(&root.Options{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: &bytes.Buffer{}})
	err := root.Execute(cmd, []string{"benchmark", "replay", fixture})
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("stdout failure exit = %d, want 1 (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "write result") {
		t.Fatalf("stdout failure error = %v, want write result", err)
	}
}

func TestBenchmarkCommandRegistrationKeepsExistingCommands(t *testing.T) {
	cmd, _, _, opts := newReplayCommandForTest(t, nil)
	benchmark, _, err := cmd.Find([]string{"benchmark"})
	if err != nil {
		t.Fatalf("Find benchmark: %v", err)
	}
	if benchmark == nil {
		t.Fatal("benchmark command not registered")
	}
	want := []string{"compare", "doctor", "infer-system-one", "replay", "run", "select", "validate"}
	got := make([]string, 0, len(benchmark.Commands()))
	for _, child := range benchmark.Commands() {
		got = append(got, child.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("benchmark commands = %#v, want %#v", got, want)
	}
	if opts == nil {
		t.Fatal("root options are nil")
	}
}

func newReplayCommandForTest(t *testing.T, opts *root.Options) (*cobra.Command, *bytes.Buffer, *bytes.Buffer, *root.Options) {
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
	Register(cmd, rootOpts)
	return cmd, out, errOut, rootOpts
}

func newReplayRootWithOptions(opts *root.Options) *cobra.Command {
	cmd, rootOpts := root.NewCommandWithOptions(opts)
	Register(cmd, rootOpts)
	return cmd
}

func writeReplayInput(t *testing.T, path string, input reportidentity.Input) string {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}
	return path
}

func replayFinding(id string, severity review.Severity) review.Finding {
	return review.Finding{ID: review.FindingID(id), Severity: severity, FilePath: "src/" + id + ".go", Anchor: review.Anchor{Kind: review.AnchorKindFile}, Body: "Synthetic finding for " + id}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}
