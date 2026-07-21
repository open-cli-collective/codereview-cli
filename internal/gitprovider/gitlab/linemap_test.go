package gitlab

import "testing"

const sampleDiff = "@@ -1,4 +1,5 @@\n context1\n-removed\n+added1\n+added2\n context2\n@@ -10,2 +11,2 @@\n context3\n-old-tail\n+new-tail\n"

func TestAnchorForNewLine(t *testing.T) {
	tests := []struct {
		name    string
		line    int
		found   bool
		changed bool
		old     int
	}{
		{name: "leading context", line: 1, found: true, old: 1},
		{name: "first added", line: 2, found: true, changed: true},
		{name: "second added", line: 3, found: true, changed: true},
		{name: "trailing context", line: 4, found: true, old: 3},
		{name: "second hunk context", line: 11, found: true, old: 10},
		{name: "second hunk added", line: 12, found: true, changed: true},
		{name: "outside hunks", line: 8, found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anchor := anchorForNewLine(sampleDiff, tt.line)
			if anchor.found != tt.found || anchor.changed != tt.changed || anchor.counterpart != tt.old {
				t.Fatalf("anchorForNewLine(%d) = %#v, want found=%v changed=%v counterpart=%d", tt.line, anchor, tt.found, tt.changed, tt.old)
			}
		})
	}
}

func TestAnchorForOldLine(t *testing.T) {
	tests := []struct {
		name    string
		line    int
		found   bool
		changed bool
		new     int
	}{
		{name: "leading context", line: 1, found: true, new: 1},
		{name: "removed", line: 2, found: true, changed: true},
		{name: "trailing context", line: 3, found: true, new: 4},
		{name: "second hunk removed", line: 11, found: true, changed: true},
		{name: "outside hunks", line: 7, found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anchor := anchorForOldLine(sampleDiff, tt.line)
			if anchor.found != tt.found || anchor.changed != tt.changed || anchor.counterpart != tt.new {
				t.Fatalf("anchorForOldLine(%d) = %#v, want found=%v changed=%v counterpart=%d", tt.line, anchor, tt.found, tt.changed, tt.new)
			}
		})
	}
}

func TestAnchorHandlesMalformedHunkHeader(t *testing.T) {
	if anchor := anchorForNewLine("@@ garbage @@\n+x\n", 1); anchor.found {
		t.Fatalf("anchor = %#v, want not found for malformed header", anchor)
	}
}
