package pipeline

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func TestParseUnifiedDiffFileShapes(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 1111111..2222222 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,2 +1,3 @@",
		" package main",
		"-var old = true",
		"+var newValue = true",
		"+var another = true",
		"diff --git a/new.go b/new.go",
		"new file mode 100644",
		"index 0000000..3333333",
		"--- /dev/null",
		"+++ b/new.go",
		"@@ -0,0 +1,2 @@",
		"+package newpkg",
		"+const Added = true",
		"diff --git a/deleted.go b/deleted.go",
		"deleted file mode 100644",
		"index 4444444..0000000",
		"--- a/deleted.go",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-package deleted",
		"-const Removed = true",
		"diff --git a/old.go b/newname.go",
		"similarity index 88%",
		"rename from old.go",
		"rename to newname.go",
		"--- a/old.go",
		"+++ b/newname.go",
		"@@ -10,2 +10,2 @@",
		"-old",
		"+new",
		"diff --git a/image.png b/image.png",
		"index 5555555..6666666 100644",
		"Binary files a/image.png and b/image.png differ",
		"",
	}, "\n")

	got, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(got.Patches) != 5 {
		t.Fatalf("patches len = %d, want 5", len(got.Patches))
	}
	if len(got.PlanDiff.Files) != len(got.Patches) {
		t.Fatalf("plan files len = %d, want patches len", len(got.PlanDiff.Files))
	}

	modified := got.Patches[0]
	if modified.OldPath != "main.go" || modified.Path != "main.go" || modified.Deleted || modified.Binary {
		t.Fatalf("modified patch = %#v", modified)
	}
	if len(modified.Hunks) != 1 || modified.Hunks[0].OldStart != 1 || modified.Hunks[0].OldEnd != 2 ||
		modified.Hunks[0].NewStart != 1 || modified.Hunks[0].NewEnd != 3 ||
		modified.Hunks[0].FallbackSide != review.DiffSideRight || modified.Hunks[0].FallbackLine != 1 {
		t.Fatalf("modified hunk = %#v", modified.Hunks)
	}

	added := got.Patches[1]
	if added.Path != "new.go" || added.Deleted || added.Binary {
		t.Fatalf("added patch = %#v", added)
	}
	if added.Hunks[0].OldEnd < 0 {
		t.Fatalf("added old hunk end = %d, want non-negative empty range", added.Hunks[0].OldEnd)
	}
	if added.Hunks[0].FallbackSide != review.DiffSideRight || added.Hunks[0].FallbackLine != 1 {
		t.Fatalf("added hunk = %#v", added.Hunks[0])
	}

	deleted := got.Patches[2]
	if deleted.Path != "deleted.go" || !deleted.Deleted || deleted.Binary {
		t.Fatalf("deleted patch = %#v", deleted)
	}
	if deleted.Hunks[0].FallbackSide != review.DiffSideLeft || deleted.Hunks[0].FallbackLine != 1 {
		t.Fatalf("deleted hunk = %#v", deleted.Hunks[0])
	}

	renamed := got.Patches[3]
	if renamed.OldPath != "old.go" || renamed.Path != "newname.go" {
		t.Fatalf("renamed patch = %#v", renamed)
	}

	binary := got.Patches[4]
	if binary.Path != "image.png" || !binary.Binary || len(binary.Hunks) != 0 {
		t.Fatalf("binary patch = %#v", binary)
	}
}

func TestParseUnifiedDiffMultiHunkPositionsIncrease(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 1111111..2222222 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,1 +1,1 @@",
		"-one",
		"+two",
		"@@ -20,1 +20,1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	got, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(got.Patches) != 1 || len(got.Patches[0].Hunks) != 2 {
		t.Fatalf("parsed hunks = %#v", got.Patches)
	}
	first := got.Patches[0].Hunks[0].DiffPosition
	second := got.Patches[0].Hunks[1].DiffPosition
	if first <= 0 || second <= first {
		t.Fatalf("diff positions = %d, %d; want increasing positive positions", first, second)
	}
}

func TestParseUnifiedDiffQuotedPathsWithSpaces(t *testing.T) {
	raw := strings.Join([]string{
		`diff --git "a/path with space.go" "b/path with space.go"`,
		"index 1111111..2222222 100644",
		`--- "a/path with space.go"`,
		`+++ "b/path with space.go"`,
		"@@ -1,1 +1,1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	got, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(got.Patches) != 1 || got.Patches[0].OldPath != "path with space.go" || got.Patches[0].Path != "path with space.go" {
		t.Fatalf("patches = %#v, want quoted path with spaces", got.Patches)
	}
}

// Git leaves paths with spaces unquoted in "diff --git" headers and appends a
// tab to "---"/"+++" paths that contain a space. These shapes mirror its output.
func TestParseUnifiedDiffUnquotedPathsWithSpaces(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/data/2024-06-08 - a.json b/data/2024-06-08 - a.json",
		"deleted file mode 100644",
		"index 1111111..0000000",
		"--- a/data/2024-06-08 - a.json\t",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-{}",
		"diff --git a/data/2024-06-08 - b.json b/data/2024-06-08 - b.json",
		"deleted file mode 100644",
		"index 2222222..0000000",
		"--- a/data/2024-06-08 - b.json\t",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-{}",
		"diff --git a/data/2024-06-09 - c.json b/data/2024-06-09 - c.json",
		"new file mode 100644",
		"index 0000000..3333333",
		"--- /dev/null",
		"+++ b/data/2024-06-09 - c.json\t",
		"@@ -0,0 +1 @@",
		"+{}",
		"diff --git a/data/empty old.txt b/data/empty old.txt",
		"deleted file mode 100644",
		"index e69de29..0000000",
		"diff --git a/data/empty new.txt b/data/empty new.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"diff --git a/data/blob old.bin b/data/blob old.bin",
		"deleted file mode 100644",
		"index 4444444..0000000",
		"Binary files a/data/blob old.bin and /dev/null differ",
		"diff --git a/data/blob new.bin b/data/blob new.bin",
		"new file mode 100644",
		"index 0000000..5555555",
		"Binary files /dev/null and b/data/blob new.bin differ",
		"diff --git a/scripts/run me.sh b/scripts/run me.sh",
		"old mode 100644",
		"new mode 100755",
		"diff --git a/docs/old name.md b/docs/new name.md",
		"similarity index 100%",
		"rename from docs/old name.md",
		"rename to docs/new name.md",
		"diff --git a/docs/moved file.md b/docs/moved file 2.md",
		"similarity index 80%",
		"rename from docs/moved file.md",
		"rename to docs/moved file 2.md",
		"index 6666666..7777777 100644",
		"--- a/docs/moved file.md\t",
		"+++ b/docs/moved file 2.md\t",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	got, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	want := []struct {
		oldPath, path          string
		deleted, added, binary bool
	}{
		{oldPath: "data/2024-06-08 - a.json", path: "data/2024-06-08 - a.json", deleted: true},
		{oldPath: "data/2024-06-08 - b.json", path: "data/2024-06-08 - b.json", deleted: true},
		{oldPath: "data/2024-06-09 - c.json", path: "data/2024-06-09 - c.json", added: true},
		{oldPath: "data/empty old.txt", path: "data/empty old.txt", deleted: true},
		{oldPath: "data/empty new.txt", path: "data/empty new.txt", added: true},
		{oldPath: "data/blob old.bin", path: "data/blob old.bin", deleted: true, binary: true},
		{oldPath: "data/blob new.bin", path: "data/blob new.bin", added: true, binary: true},
		{oldPath: "scripts/run me.sh", path: "scripts/run me.sh"},
		{oldPath: "docs/old name.md", path: "docs/new name.md"},
		{oldPath: "docs/moved file.md", path: "docs/moved file 2.md"},
	}
	if len(got.Patches) != len(want) || len(got.PlanDiff.Files) != len(want) {
		t.Fatalf("patches = %d, plan files = %d, want %d", len(got.Patches), len(got.PlanDiff.Files), len(want))
	}
	for i, w := range want {
		patch := got.Patches[i]
		if patch.OldPath != w.oldPath || patch.Path != w.path || patch.Deleted != w.deleted || patch.Added != w.added || patch.Binary != w.binary {
			t.Errorf("patch %d = old %q path %q deleted %v added %v binary %v, want %+v",
				i, patch.OldPath, patch.Path, patch.Deleted, patch.Added, patch.Binary, w)
		}
		if plan := got.PlanDiff.Files[i]; plan.OldPath != w.oldPath || plan.Path != w.path {
			t.Errorf("plan file %d = old %q path %q, want old %q path %q", i, plan.OldPath, plan.Path, w.oldPath, w.path)
		}
	}
}

func TestParseUnifiedDiffQuotedRenamePaths(t *testing.T) {
	raw := strings.Join([]string{
		`diff --git "a/docs/caf\303\251 notes.md" "b/docs/caf\303\251 notes 2.md"`,
		"similarity index 100%",
		`rename from "docs/caf\303\251 notes.md"`,
		`rename to "docs/caf\303\251 notes 2.md"`,
		"",
	}, "\n")

	got, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(got.Patches) != 1 || got.Patches[0].OldPath != "docs/café notes.md" || got.Patches[0].Path != "docs/café notes 2.md" {
		t.Fatalf("patches = %#v, want decoded rename paths", got.Patches)
	}
}

func TestParseUnifiedDiffRejectsBadHunkHeader(t *testing.T) {
	_, err := parseUnifiedDiff("diff --git a/main.go b/main.go\n@@ bad @@\n")
	if err == nil {
		t.Fatal("parseUnifiedDiff error = nil, want bad hunk failure")
	}
}

func TestParseUnifiedDiffPreservesPatchlessRemovedStatus(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/removed.txt b/removed.txt",
		"--- a/removed.txt",
		"+++ /dev/null",
		"diff --git a/removed.bin b/removed.bin",
		"--- a/removed.bin",
		"+++ /dev/null",
		"",
	}, "\n")

	parsed, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(parsed.Patches) != 2 {
		t.Fatalf("patches = %#v, want two patchless deletions", parsed.Patches)
	}
	for _, patch := range parsed.Patches {
		if !patch.Deleted || patch.Binary || patch.Path == "" || patch.OldPath != patch.Path || len(patch.Hunks) != 0 {
			t.Fatalf("patchless deletion = %#v, want Deleted with no Binary flag or hunks", patch)
		}
	}
}

func TestParseUnifiedDiffMarksAddedFiles(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/pkg/__init__.py b/pkg/__init__.py",
		"new file mode 100644",
		"index 0000000..e69de29",
		"diff --git a/pkg/mod.py b/pkg/mod.py",
		"new file mode 100644",
		"index 0000000..5716ca5",
		"--- /dev/null",
		"+++ b/pkg/mod.py",
		"@@ -0,0 +1 @@",
		"+x = 1",
		"",
	}, "\n")

	parsed, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("parseUnifiedDiff: %v", err)
	}
	if len(parsed.Patches) != 2 {
		t.Fatalf("patches = %#v, want two added files", parsed.Patches)
	}
	empty, withContent := parsed.Patches[0], parsed.Patches[1]
	if !empty.Added || empty.Deleted || empty.Path != "pkg/__init__.py" || len(empty.Hunks) != 0 {
		t.Fatalf("empty added file = %#v, want Added with no hunks", empty)
	}
	if !withContent.Added || withContent.Path != "pkg/mod.py" || len(withContent.Hunks) != 1 {
		t.Fatalf("added file with content = %#v, want Added with one hunk", withContent)
	}
}
