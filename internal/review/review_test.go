package review

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Severity
	}{
		{name: "blocking", input: "blocking", want: SeverityBlocking},
		{name: "trim and lower", input: " Major ", want: SeverityMajor},
		{name: "minor", input: "minor", want: SeverityMinor},
		{name: "nits", input: "nits", want: SeverityNits},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSeverity(tt.input)
			if err != nil {
				t.Fatalf("ParseSeverity(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseSeverity(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if !got.Valid() {
				t.Fatalf("%q.Valid() = false, want true", got)
			}
			if got.String() != string(tt.want) {
				t.Fatalf("%q.String() = %q, want %q", got, got.String(), tt.want)
			}
		})
	}
}

func TestParseSeverityRejectsInvalid(t *testing.T) {
	got, err := ParseSeverity("nit")
	if err == nil {
		t.Fatal("ParseSeverity(nit) error = nil, want error")
	}
	if got != "" {
		t.Fatalf("ParseSeverity(nit) = %q, want zero value", got)
	}
	if Severity("nit").Valid() {
		t.Fatal("Severity(nit).Valid() = true, want false")
	}
}

func TestSeverityOrderAndThreshold(t *testing.T) {
	wantOrder := []Severity{
		SeverityBlocking,
		SeverityMajor,
		SeverityMinor,
		SeverityNits,
	}
	if got := SeverityOrder(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("SeverityOrder() = %v, want %v", got, wantOrder)
	}

	got := SeverityOrder()
	got[0] = SeverityNits
	if again := SeverityOrder(); !reflect.DeepEqual(again, wantOrder) {
		t.Fatalf("SeverityOrder() returned mutable backing storage: %v", again)
	}

	tests := []struct {
		severity  Severity
		threshold Severity
		want      bool
	}{
		{SeverityBlocking, SeverityBlocking, true},
		{SeverityBlocking, SeverityNits, true},
		{SeverityMajor, SeverityBlocking, false},
		{SeverityMajor, SeverityMajor, true},
		{SeverityMajor, SeverityMinor, true},
		{SeverityMinor, SeverityMajor, false},
		{SeverityNits, SeverityNits, true},
		{Severity("unknown"), SeverityNits, false},
		{SeverityMajor, Severity("unknown"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.severity)+"/"+string(tt.threshold), func(t *testing.T) {
			if got := tt.severity.AtLeast(tt.threshold); got != tt.want {
				t.Fatalf("%q.AtLeast(%q) = %t, want %t", tt.severity, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestEnumParsingAndValidity(t *testing.T) {
	tests := []struct {
		name     string
		parse    func(string) (string, bool)
		valid    func(string) bool
		input    string
		want     string
		badInput string
		badValue string
	}{
		{
			name: "review event",
			parse: func(s string) (string, bool) {
				got, err := ParseReviewEvent(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ReviewEvent(s).Valid() },
			input: " Request_Changes ", want: "request_changes",
			badInput: "changes_requested", badValue: "changes_requested",
		},
		{
			name: "thread decision",
			parse: func(s string) (string, bool) {
				got, err := ParseThreadDecision(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ThreadDecision(s).Valid() },
			input: " Summarize_Only ", want: "summarize_only",
			badInput: "resolve", badValue: "resolve",
		},
		{
			name: "anchor kind",
			parse: func(s string) (string, bool) {
				got, err := ParseAnchorKind(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return AnchorKind(s).Valid() },
			input: " File ", want: "file",
			badInput: "range", badValue: "range",
		},
		{
			name: "diff side",
			parse: func(s string) (string, bool) {
				got, err := ParseDiffSide(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return DiffSide(s).Valid() },
			input: " right ", want: "RIGHT",
			badInput: "both", badValue: "BOTH",
		},
		{
			name: "anchoring",
			parse: func(s string) (string, bool) {
				got, err := ParseAnchoring(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return Anchoring(s).Valid() },
			input: " File-Level-Native ", want: "file-level-native",
			badInput: "file", badValue: "file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.parse(tt.input)
			if !ok {
				t.Fatalf("parse(%q) failed, want success", tt.input)
			}
			if got != tt.want {
				t.Fatalf("parse(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if !tt.valid(tt.want) {
				t.Fatalf("Valid(%q) = false, want true", tt.want)
			}
			if _, ok := tt.parse(tt.badInput); ok {
				t.Fatalf("parse(%q) succeeded, want error", tt.badInput)
			}
			if tt.valid(tt.badValue) {
				t.Fatalf("Valid(%q) = true, want false", tt.badValue)
			}
		})
	}
}

func TestAllEnumConstantsParseAndValidate(t *testing.T) {
	tests := []struct {
		name   string
		values []interface{ String() string }
		parse  func(string) (string, bool)
		valid  func(string) bool
	}{
		{
			name:   "review event",
			values: []interface{ String() string }{ReviewEventApprove, ReviewEventComment, ReviewEventRequestChanges},
			parse: func(s string) (string, bool) {
				got, err := ParseReviewEvent(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ReviewEvent(s).Valid() },
		},
		{
			name:   "thread decision",
			values: []interface{ String() string }{ThreadDecisionSkip, ThreadDecisionSummarizeOnly, ThreadDecisionSummarizeAndResolve},
			parse: func(s string) (string, bool) {
				got, err := ParseThreadDecision(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ThreadDecision(s).Valid() },
		},
		{
			name:   "thread response kind",
			values: []interface{ String() string }{ThreadResponseReply, ThreadResponseSummaryReply},
			parse: func(s string) (string, bool) {
				kind := ThreadResponseKind(normalizeLower(s))
				return kind.String(), kind.Valid()
			},
			valid: func(s string) bool { return ThreadResponseKind(s).Valid() },
		},
		{
			name:   "anchor kind",
			values: []interface{ String() string }{AnchorKindLine, AnchorKindFile},
			parse: func(s string) (string, bool) {
				got, err := ParseAnchorKind(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return AnchorKind(s).Valid() },
		},
		{
			name:   "diff side",
			values: []interface{ String() string }{DiffSideLeft, DiffSideRight},
			parse: func(s string) (string, bool) {
				got, err := ParseDiffSide(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return DiffSide(s).Valid() },
		},
		{
			name:   "anchoring",
			values: []interface{ String() string }{AnchoringInline, AnchoringFileLevelNative, AnchoringFileLevelFallback, AnchoringRollupOnly},
			parse: func(s string) (string, bool) {
				got, err := ParseAnchoring(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return Anchoring(s).Valid() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, typedValue := range tt.values {
				value := typedValue.String()
				t.Run(value, func(t *testing.T) {
					got, ok := tt.parse(value)
					if !ok {
						t.Fatalf("parse(%q) failed, want success", value)
					}
					if got != value {
						t.Fatalf("parse(%q) = %q, want %q", value, got, value)
					}
					if !tt.valid(value) {
						t.Fatalf("Valid(%q) = false, want true", value)
					}
				})
			}
		})
	}
}

func TestThreadResponseActionValidate(t *testing.T) {
	tests := []struct {
		name string
		in   ThreadResponseAction
		err  string
	}{
		{
			name: "reply",
			in:   ThreadResponseAction{Kind: ThreadResponseReply, ThreadID: "thread-1", Body: "Thanks."},
		},
		{
			name: "summary reply resolves",
			in:   ThreadResponseAction{Kind: ThreadResponseSummaryReply, ThreadID: "thread-1", Body: "Summary.", Resolve: true},
		},
		{
			name: "invalid kind",
			in:   ThreadResponseAction{Kind: ThreadResponseKind("other"), ThreadID: "thread-1", Body: "Body."},
			err:  "invalid",
		},
		{
			name: "missing thread",
			in:   ThreadResponseAction{Kind: ThreadResponseReply, Body: "Body."},
			err:  "ID",
		},
		{
			name: "missing body",
			in:   ThreadResponseAction{Kind: ThreadResponseReply, ThreadID: "thread-1"},
			err:  "body",
		},
		{
			name: "resolve requires summary reply",
			in:   ThreadResponseAction{Kind: ThreadResponseReply, ThreadID: "thread-1", Body: "Body.", Resolve: true},
			err:  "summary_reply",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if tt.err == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.err)
			}
		})
	}
}

func TestAnchorValidate(t *testing.T) {
	tests := []struct {
		name    string
		anchor  Anchor
		wantErr string
	}{
		{
			name: "line",
			anchor: Anchor{
				Kind: AnchorKindLine,
				Side: DiffSideRight,
				Line: 42,
			},
		},
		{
			name:   "file",
			anchor: Anchor{Kind: AnchorKindFile},
		},
		{
			name:    "line missing side",
			anchor:  Anchor{Kind: AnchorKindLine, Line: 42},
			wantErr: "invalid diff side",
		},
		{
			name:    "line missing line",
			anchor:  Anchor{Kind: AnchorKindLine, Side: DiffSideRight},
			wantErr: "positive line",
		},
		{
			name:    "file with side",
			anchor:  Anchor{Kind: AnchorKindFile, Side: DiffSideRight},
			wantErr: "cannot set side",
		},
		{
			name:    "file with line",
			anchor:  Anchor{Kind: AnchorKindFile, Line: 42},
			wantErr: "cannot set line",
		},
		{
			name:    "bad kind",
			anchor:  Anchor{Kind: AnchorKind("range")},
			wantErr: "invalid anchor kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.anchor.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestFindingIDOptionalityAndDomainShapes(t *testing.T) {
	raw := Finding{
		Severity: SeverityMajor,
		FilePath: "internal/review/review.go",
		Anchor: Anchor{
			Kind: AnchorKindLine,
			Side: DiffSideRight,
			Line: 42,
		},
		Body: "finding body",
	}
	if raw.ID.Assigned() {
		t.Fatal("zero Finding.ID Assigned() = true, want false")
	}

	raw.ID = FindingID("f-abc")
	raw.Anchoring = AnchoringInline
	if !raw.ID.Assigned() {
		t.Fatal("assigned Finding.ID Assigned() = false, want true")
	}

	rollup := Rollup{
		ReviewEvent:          ReviewEventRequestChanges,
		ReviewEventRationale: "major finding",
		DedupeLog: []DedupeEntry{
			{Kept: raw.ID, Dropped: []FindingID{"f-def"}, Reason: "same issue"},
		},
		OrderedFindings: []FindingID{raw.ID},
	}
	if rollup.OrderedFindings[0] != raw.ID {
		t.Fatalf("Rollup.OrderedFindings[0] = %q, want %q", rollup.OrderedFindings[0], raw.ID)
	}
	if rollup.DedupeLog[0].Dropped[0] != FindingID("f-def") {
		t.Fatalf("Rollup.DedupeLog[0].Dropped[0] = %q, want f-def", rollup.DedupeLog[0].Dropped[0])
	}
}

func TestFindingValidate(t *testing.T) {
	valid := Finding{
		Severity: SeverityMajor,
		FilePath: "internal/review/review.go",
		Anchor: Anchor{
			Kind: AnchorKindLine,
			Side: DiffSideRight,
			Line: 42,
		},
		Body:      "finding body",
		Anchoring: AnchoringInline,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Finding)
		wantErr string
	}{
		{
			name: "bad severity",
			mutate: func(f *Finding) {
				f.Severity = Severity("urgent")
			},
			wantErr: "invalid severity",
		},
		{
			name: "bad anchor",
			mutate: func(f *Finding) {
				f.Anchor.Line = 0
			},
			wantErr: "positive line",
		},
		{
			name: "bad anchoring",
			mutate: func(f *Finding) {
				f.Anchoring = Anchoring("comment")
			},
			wantErr: "invalid anchoring",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finding := valid
			tt.mutate(&finding)
			err := finding.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestProductionImportsAreStdlibOnly(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(testFile)
	repoRoot, modulePath := repoRootAndModule(t, dir)
	stdlib := stdlibImports(t, repoRoot)
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(file) != ".go" {
			return nil
		}
		if strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(path, modulePath+"/") || path == modulePath {
				t.Fatalf("production import %q is internal to this module", path)
			}
			if _, ok := stdlib[path]; !ok {
				t.Fatalf("production import %q is not in the standard library", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
}

func repoRootAndModule(t *testing.T, dir string) (string, string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Path}}")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list module: %v", err)
	}
	parts := strings.Fields(string(output))
	if len(parts) != 2 {
		t.Fatalf("go list module output = %q, want dir and path", output)
	}
	return parts[0], parts[1]
}

func stdlibImports(t *testing.T, repoRoot string) map[string]struct{} {
	t.Helper()
	cmd := exec.Command("go", "list", "std")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list std: %v", err)
	}
	imports := make(map[string]struct{})
	for _, path := range bytes.Fields(output) {
		imports[string(path)] = struct{}{}
	}
	return imports
}
