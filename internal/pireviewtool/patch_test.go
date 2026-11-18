package pireviewtool

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedPatchThreeEdits(t *testing.T) {
	const readable = "diff --git a/src.js b/src.js\n--- a/src.js\n+++ b/src.js\n@@ -1 +1 @@\n-return false;\n+return true;\n"
	const header = "diff --git a/dist.js b/dist.js\n--- a/dist.js\n+++ b/dist.js\n@@ -1,1 +1,1 @@\n"
	const eof = "\\ No newline at end of file\n"
	old := patchBundle(252_000)
	newText := old[:12_000] + "FIRST" + old[12_000:100_000] + "SECOND" + old[100_000:200_000] + "THIRD" + old[200_000:]
	patch := readable + header + "-" + old + "\n" + eof + "+" + newText + "\n" + eof
	dir := t.TempDir()
	writePatchFixture(t, filepath.Join(dir, "bundle.patch"), patch)
	config := Config{
		RepoDir: dir, BaseSHA: "base-fixture", HeadSHA: "head-fixture", MaxOutputBytes: 4096,
		AddedPatchBlobs: map[string]string{"bundle.patch": patchBlobOID([]byte(patch), 40)},
	}
	request := Request{Tool: ToolRead, Path: "bundle.patch", View: "patch"}
	report, err := Execute(t.Context(), config, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		readable + header, "Head: head-fixture", "Patch SHA-256:",
		"Old line has no trailing newline.",
		"old[12000:12000] \"\" -> new[12000:12005] \"FIRST\"",
		"old[100000:100000] \"\" -> new[100005:100011] \"SECOND\"",
		"old[200000:200000] \"\" -> new[200011:200016] \"THIRD\"",
		"All bytes outside the listed ranges matched exactly.\n" + eof,
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q from report:\n%s", want, report)
		}
	}
	if len(report) > config.MaxOutputBytes || strings.Count(report, " -> new[") != 3 {
		t.Fatalf("unexpected report size or edits: %s", report)
	}
	writePatchFixture(t, filepath.Join(dir, "bundle.patch"), patch+"altered after checkout\n")
	if output, err := Execute(t.Context(), config, request); !errors.Is(err, ErrDenied) || output != "" || !strings.Contains(err.Error(), "pinned head blob") {
		t.Fatalf("mismatched head blob must not return evidence: %q, %v", output, err)
	}
	writePatchFixture(t, filepath.Join(dir, "bundle.patch"), patch)
	raw, err := Execute(t.Context(), config, Request{Tool: ToolRead, Path: request.Path})
	if err != nil || strings.Contains(raw, "THIRD") || !strings.Contains(raw, "next_offset") {
		t.Fatalf("ordinary read should still require ranges: %q, %v", raw[:min(200, len(raw))], err)
	}
	config.MaxOutputBytes = 200
	if output, err := Execute(t.Context(), config, request); !errors.Is(err, ErrDenied) || output != "" {
		t.Fatalf("over-budget report must not return partial evidence: %q, %v", output, err)
	}
}

func TestGeneratedLineReconstructsAdditionalChangedByte(t *testing.T) {
	before := patchBundle(100_000)
	after := before[:100] + "x" + before[100:1000] + before[1003:20_000] + "yy" + before[20_001:90_000] + "!" + before[90_001:] + "tail"
	edits, err := compareGeneratedLine(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	var reconstructed strings.Builder
	previous := 0
	for _, edit := range edits {
		reconstructed.WriteString(before[previous:edit.oldOffset])
		if reconstructed.Len() != edit.newOffset || before[edit.oldOffset:edit.oldOffset+len(edit.before)] != edit.before {
			t.Fatalf("inconsistent edit: %+v", edit)
		}
		reconstructed.WriteString(edit.after)
		previous = edit.oldOffset + len(edit.before)
	}
	reconstructed.WriteString(before[previous:])
	if reconstructed.String() != after || len(edits) != 5 {
		t.Fatalf("edits must account for all changes, including the extra byte and tail: %+v", edits)
	}
}

func TestGeneratedPatchViewDoesNotExemptOtherPatches(t *testing.T) {
	dir := t.TempDir()
	config := Config{RepoDir: dir, BaseSHA: "base-fixture", HeadSHA: "head-fixture", MaxOutputBytes: 4096}
	cases := map[string]string{
		"ordinary.patch":    "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"large.txt":         fmt.Sprintf("@@ -1 +1 @@\n-%s\n+%s\n", patchBundle(10_000), patchBundle(10_001)),
		"unsupported.patch": "@@ -1,2 +1,2 @@\n-" + patchBundle(10_000) + "\n+changed\n context\n",
		"replacement.patch": "@@ -1 +1 @@\n-" + strings.Repeat("a", 10_000) + "\n+" + strings.Repeat("b", 10_000) + "\n",
		"binary.patch":      "unified patch\x00",
		"too-large.patch":   strings.Repeat("a", maxPatchBytes+1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			writePatchFixture(t, filepath.Join(dir, name), content)
			config.AddedPatchBlobs = map[string]string{name: patchBlobOID([]byte(content), 64)}
			result, err := Execute(t.Context(), config, Request{Tool: ToolRead, Path: name, View: "patch"})
			if !errors.Is(err, ErrDenied) || result != "" {
				t.Fatalf("unexpected report: %q, %v", result, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := compareGeneratedLine(ctx, "old", "new"); !errors.Is(err, context.Canceled) {
		t.Fatalf("compare must honor cancellation: %v", err)
	}
}

func patchBundle(n int) string {
	rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- reproducible synthetic bundle bytes.
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(out)
}

func writePatchFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
