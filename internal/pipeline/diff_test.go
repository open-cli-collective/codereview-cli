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

func TestParseUnifiedDiffRejectsBadHunkHeader(t *testing.T) {
	_, err := parseUnifiedDiff("diff --git a/main.go b/main.go\n@@ bad @@\n")
	if err == nil {
		t.Fatal("parseUnifiedDiff error = nil, want bad hunk failure")
	}
}
