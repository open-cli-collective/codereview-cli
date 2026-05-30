package review

import (
	"go/parser"
	"go/token"
	"io/fs"
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
		values []string
		parse  func(string) (string, bool)
		valid  func(string) bool
	}{
		{
			name:   "review event",
			values: []string{"approve", "comment", "request_changes"},
			parse: func(s string) (string, bool) {
				got, err := ParseReviewEvent(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ReviewEvent(s).Valid() },
		},
		{
			name:   "thread decision",
			values: []string{"skip", "summarize_only", "summarize_and_resolve"},
			parse: func(s string) (string, bool) {
				got, err := ParseThreadDecision(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return ThreadDecision(s).Valid() },
		},
		{
			name:   "anchor kind",
			values: []string{"line", "file"},
			parse: func(s string) (string, bool) {
				got, err := ParseAnchorKind(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return AnchorKind(s).Valid() },
		},
		{
			name:   "diff side",
			values: []string{"LEFT", "RIGHT"},
			parse: func(s string) (string, bool) {
				got, err := ParseDiffSide(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return DiffSide(s).Valid() },
		},
		{
			name:   "anchoring",
			values: []string{"inline", "file-level-native", "file-level-fallback", "rollup-only"},
			parse: func(s string) (string, bool) {
				got, err := ParseAnchoring(s)
				return got.String(), err == nil
			},
			valid: func(s string) bool { return Anchoring(s).Valid() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, value := range tt.values {
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

func TestProductionImportsAreStdlibOnly(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(testFile)
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
			if strings.Contains(path, ".") {
				t.Fatalf("production import %q is not stdlib-only", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
}
