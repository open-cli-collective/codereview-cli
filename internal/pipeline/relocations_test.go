package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

func TestParseTreeInventoryPreservesNULTerminatedPathsAndModes(t *testing.T) {
	blob := strings.Repeat("a", 40)
	data := []byte("100644 blob " + blob + "\tpath with tab\tand newline\n\x00" +
		"100755 blob " + strings.Repeat("b", 40) + "\ttools/run\x00")

	got, err := parseTreeInventory(data)
	if err != nil {
		t.Fatalf("parseTreeInventory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("inventory has %d entries, want 2", len(got))
	}
	if entry := got["path with tab\tand newline\n"]; entry.Mode != "100644" || entry.Type != "blob" || entry.OID != blob {
		t.Fatalf("tab/newline path entry = %#v", entry)
	}
	if entry := got["tools/run"]; entry.Mode != "100755" || entry.Type != "blob" || entry.OID != strings.Repeat("b", 40) {
		t.Fatalf("executable entry = %#v", entry)
	}
}

func TestParseTreeInventoryRejectsMalformedAndDuplicateEntries(t *testing.T) {
	blob := strings.Repeat("a", 40)
	for name, data := range map[string][]byte{
		"missing NUL terminator": []byte("100644 blob " + blob + "\tfile.go"),
		"missing path separator": []byte("100644 blob " + blob + " file.go\x00"),
		"duplicate path":         []byte("100644 blob " + blob + "\tfile.go\x00100755 blob " + blob + "\tfile.go\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTreeInventory(data); err == nil {
				t.Fatal("parseTreeInventory returned no error")
			}
		})
	}
}

func TestPrepareRelocationManifestUsesPinnedGitTreesAndWritesAtomically(t *testing.T) {
	repo := t.TempDir()
	relocationTestGit(t, repo, "init", "-q")
	relocationTestGit(t, repo, "config", "user.name", "CR Tests")
	relocationTestGit(t, repo, "config", "user.email", "cr-tests@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "old.go"), []byte("package moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relocationTestGit(t, repo, "add", "old.go")
	relocationTestGit(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))
	relocationTestGit(t, repo, "mv", "old.go", "new.go")
	relocationTestGit(t, repo, "commit", "-qm", "move")
	head := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))

	paths := ArtifactPaths{WorkbenchRepoDir: repo, RelocationsJSON: filepath.Join(t.TempDir(), "relocations.json")}
	command := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		// #nosec G204 -- fixed Git executable and test-supplied args operate only on this temporary repository.
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		return cmd.CombinedOutput()
	}
	manifest, tree, err := prepareRelocationManifest(context.Background(), command, paths, base, head, []FilePatch{{OldPath: "old.go", Path: "new.go"}})
	if err != nil {
		t.Fatalf("prepareRelocationManifest: %v", err)
	}
	if len(manifest.Moves) != 1 || manifest.Moves[0].OldPath != "old.go" || manifest.Moves[0].Path != "new.go" {
		t.Fatalf("manifest moves = %#v", manifest.Moves)
	}
	if len(tree) != 1 || tree["new.go"].OID != manifest.Moves[0].BlobOID {
		t.Fatalf("head tree = %#v, want verified new.go blob", tree)
	}
	data, err := os.ReadFile(paths.RelocationsJSON)
	if err != nil {
		t.Fatalf("read relocation artifact: %v", err)
	}
	if !strings.Contains(string(data), `"digest": "`+manifest.Digest+`"`) {
		t.Fatalf("relocation artifact omitted digest %s: %s", manifest.Digest, data)
	}

	if _, _, err := prepareRelocationManifest(context.Background(), command, paths, base, base, nil); err == nil || !strings.Contains(err.Error(), "HEAD does not match") {
		t.Fatalf("mismatched HEAD error = %v, want pinned identity error", err)
	}
	missingBase := strings.Repeat("0", len(base))
	if missingBase == base {
		missingBase = strings.Repeat("1", len(base))
	}
	if _, _, err := prepareRelocationManifest(context.Background(), command, paths, missingBase, head, nil); err == nil || !strings.Contains(err.Error(), "pinned base commit") {
		t.Fatalf("missing base error = %v, want pinned base resolution error", err)
	}
}

func relocationTestGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	// #nosec G204 -- fixed Git executable and test-supplied args operate only on this temporary repository.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func TestCertifyRelocationsRequiresUniqueSameBlobRegularFilePairs(t *testing.T) {
	base := map[string]treeEntry{
		"old/z.go":          relocationTreeEntry("100644", "a", "old/z.go"),
		"old/a.go":          relocationTreeEntry("100644", "a", "old/a.go"),
		"mode/old":          relocationTreeEntry("100644", "c", "mode/old"),
		"blob/old":          relocationTreeEntry("100644", "d", "blob/old"),
		"link/old":          relocationTreeEntry("120000", "e", "link/old"),
		"unrelated/deleted": relocationTreeEntry("100644", "f", "unrelated/deleted"),
		"same/path.go":      relocationTreeEntry("100644", "9", "same/path.go"),
	}
	head := map[string]treeEntry{
		"new/z.go":        relocationTreeEntry("100644", "a", "new/z.go"),
		"new/a.go":        relocationTreeEntry("100644", "a", "new/a.go"),
		"mode/new":        relocationTreeEntry("100755", "c", "mode/new"),
		"blob/new":        relocationTreeEntry("100644", "1", "blob/new"),
		"link/new":        relocationTreeEntry("120000", "e", "link/new"),
		"unrelated/added": relocationTreeEntry("100644", "f", "unrelated/added"),
		"same/path.go":    relocationTreeEntry("100644", "9", "same/path.go"),
	}

	pairs := []FilePatch{{OldPath: "old/z.go", Path: "new/z.go"}, {OldPath: "old/a.go", Path: "new/a.go"}}
	got, err := certifyRelocations(base, head, pairs)
	if err != nil {
		t.Fatalf("certifyRelocations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("certified moves = %#v, want both explicit pairs even though their blobs match", got)
	}
	if got[0].OldPath != "old/a.go" || got[0].Path != "new/a.go" || got[0].BlobOID != strings.Repeat("a", 40) || got[0].Mode != "100644" {
		t.Fatalf("first certified move = %#v", got[0])
	}
	if got[1].OldPath != "old/z.go" || got[1].Path != "new/z.go" {
		t.Fatalf("second certified move = %#v", got[1])
	}
	if unrelated, err := certifyRelocations(base, head, nil); err != nil || len(unrelated) != 0 {
		t.Fatalf("unrelated equal-blob addition/deletion was inferred as a relocation: moves=%#v err=%v", unrelated, err)
	}
	if _, err := certifyRelocations(base, head, []FilePatch{{OldPath: "old/z.go", Path: "new/z.go"}, {OldPath: "old/z.go", Path: "new/a.go"}}); err == nil {
		t.Fatal("colliding candidate old paths were accepted")
	}
	if _, err := certifyRelocations(base, head, []FilePatch{{OldPath: "old/z.go", Path: "new/z.go"}, {OldPath: "old/a.go", Path: "new/z.go"}}); err == nil {
		t.Fatal("colliding candidate new paths were accepted")
	}
}

func relocationTreeEntry(mode, marker, path string) treeEntry {
	return treeEntry{Mode: mode, Type: "blob", OID: strings.Repeat(marker, 40), Path: path}
}

func TestValidatePatchlessRenamesRequiresCertifiedEvidenceOrHunks(t *testing.T) {
	move := relocationMove{OldPath: "old.go", Path: "new.go", BlobOID: strings.Repeat("a", 40), Mode: "100644"}
	patch := FilePatch{OldPath: move.OldPath, Path: move.Path}
	if err := validatePatchlessRenames([]FilePatch{patch}, []relocationMove{move}); err != nil {
		t.Fatalf("certified patchless move: %v", err)
	}
	if err := validatePatchlessRenames([]FilePatch{patch}, nil); err == nil {
		t.Fatal("uncertified patchless rename returned no error")
	}
	patch.Hunks = []reviewplan.DiffHunk{{OldStart: 1, NewStart: 1}}
	if err := validatePatchlessRenames([]FilePatch{patch}, nil); err != nil {
		t.Fatalf("rename with provider hunk: %v", err)
	}
}

func TestValidateRelocationAssessmentRequiresExactDigestsAndPinnedEvidence(t *testing.T) {
	assignment := relocationAssignment{
		ManifestDigest: "manifest-1", AssignmentDigest: "assignment-1", MoveCount: 2,
		Moves: []relocationMove{
			{OldPath: "old/a.go", Path: "new/a.go", BlobOID: strings.Repeat("a", 40), Mode: "100644"},
			{OldPath: "old/b.go", Path: "new/b.go", BlobOID: strings.Repeat("b", 40), Mode: "100644"},
		},
	}
	head := map[string]treeEntry{
		"new/a.go":       relocationTreeEntry("100644", "a", "new/a.go"),
		"new/b.go":       relocationTreeEntry("100644", "b", "new/b.go"),
		"src/imports.go": relocationTreeEntry("100644", "c", "src/imports.go"),
	}
	valid := &llm.RelocationAssessment{
		ManifestDigest: "manifest-1", AssignmentDigest: "assignment-1", PathImpactReviewed: true,
		EvidenceFiles: []string{"src/imports.go"}, Basis: "checked imports and workspace configuration",
	}
	findings := llm.Findings{RelocationAssessment: valid, InspectedFiles: []string{"new/a.go"}, ContextFiles: []string{"src/imports.go"}}
	got, diagnostic := validateRelocationAssessment(findings, assignment, head)
	if diagnostic != "" || len(got) != 2 || got[0] != "new/a.go" || got[1] != "new/b.go" {
		t.Fatalf("valid assessment = %#v, %q", got, diagnostic)
	}

	for name, mutate := range map[string]func(*llm.RelocationAssessment, *llm.Findings, map[string]treeEntry){
		"missing": func(_ *llm.RelocationAssessment, f *llm.Findings, _ map[string]treeEntry) {
			f.RelocationAssessment = nil
		},
		"wrong manifest digest": func(a *llm.RelocationAssessment, _ *llm.Findings, _ map[string]treeEntry) {
			a.ManifestDigest = "wrong"
		},
		"wrong assignment digest": func(a *llm.RelocationAssessment, _ *llm.Findings, _ map[string]treeEntry) {
			a.AssignmentDigest = "wrong"
		},
		"unresolved impact": func(a *llm.RelocationAssessment, _ *llm.Findings, _ map[string]treeEntry) {
			a.PathImpactReviewed = false
		},
		"empty basis":          func(a *llm.RelocationAssessment, _ *llm.Findings, _ map[string]treeEntry) { a.Basis = "  " },
		"empty evidence":       func(a *llm.RelocationAssessment, _ *llm.Findings, _ map[string]treeEntry) { a.EvidenceFiles = nil },
		"uninspected evidence": func(_ *llm.RelocationAssessment, f *llm.Findings, _ map[string]treeEntry) { f.ContextFiles = nil },
		"missing pinned evidence": func(_ *llm.RelocationAssessment, _ *llm.Findings, tree map[string]treeEntry) {
			delete(tree, "src/imports.go")
		},
	} {
		t.Run(name, func(t *testing.T) {
			assessment := *valid
			assessment.EvidenceFiles = append([]string(nil), valid.EvidenceFiles...)
			caseFindings := findings
			caseFindings.RelocationAssessment = &assessment
			caseFindings.ContextFiles = append([]string(nil), findings.ContextFiles...)
			caseHead := map[string]treeEntry{}
			for path, entry := range head {
				caseHead[path] = entry
			}
			mutate(&assessment, &caseFindings, caseHead)
			got, diagnostic := validateRelocationAssessment(caseFindings, assignment, caseHead)
			if diagnostic == "" || len(got) != 0 {
				t.Fatalf("invalid assessment = %#v, %q; want no relocation credit and a diagnostic", got, diagnostic)
			}
		})
	}
}

func TestExplicitRelocationSkipOverridesOtherwiseValidAssessment(t *testing.T) {
	assignment := relocationAssignment{
		ManifestDigest: "manifest", AssignmentDigest: "assignment", MoveCount: 2,
		Moves: []relocationMove{{Path: "new/a.go"}, {Path: "new/b.go"}},
	}
	findings := llm.Findings{
		RelocationAssessment: &llm.RelocationAssessment{
			ManifestDigest: "manifest", AssignmentDigest: "assignment", PathImpactReviewed: true,
			EvidenceFiles: []string{"new/b.go"}, Basis: "reviewed paths",
		},
		InspectedFiles: []string{"new/b.go"}, SkippedFiles: []string{"new/a.go"},
	}
	got, diagnostic := validateRelocationAssessment(findings, assignment, map[string]treeEntry{
		"new/a.go": relocationTreeEntry("100644", "a", "new/a.go"),
		"new/b.go": relocationTreeEntry("100644", "b", "new/b.go"),
	})
	if diagnostic != "" || len(got) != 1 || got[0] != "new/b.go" {
		t.Fatalf("explicit skip assessment = %#v, %q; want only new/b.go reviewed", got, diagnostic)
	}
}

func TestCoverageMissingFilesIncludesSkippedAndUnresolvedResidualFiles(t *testing.T) {
	got := coverageMissingFiles(
		[]string{"ordinary.go", "new/a.go", "new/b.go"},
		[]string{"ordinary.go"},
		[]string{"new/a.go"},
		[]string{"new/b.go"},
	)
	if len(got) != 1 || got[0] != "new/a.go" {
		t.Fatalf("missing = %#v, want the skipped unreviewed move only", got)
	}
}

func TestRelocationAssignmentDigestBindsManifestAgentAndExactScope(t *testing.T) {
	move := relocationMove{OldPath: "old.go", Path: "new.go", BlobOID: strings.Repeat("a", 40), Mode: "100644"}
	state := relocationReviewState{Manifest: relocationManifest{Digest: "manifest", Moves: []relocationMove{move}}}
	baseline := state.assignment("agent-a", []string{"new.go", "ordinary.go"})
	reordered := state.assignment("agent-a", []string{"ordinary.go", "new.go"})
	otherScope := state.assignment("agent-a", []string{"new.go"})
	otherAgent := state.assignment("agent-b", []string{"new.go", "ordinary.go"})
	state.Manifest.Digest = "changed-manifest"
	otherManifest := state.assignment("agent-a", []string{"new.go", "ordinary.go"})
	if baseline.AssignmentDigest != reordered.AssignmentDigest {
		t.Fatal("assignment digest changed when only scope ordering changed")
	}
	for name, digest := range map[string]string{
		"scope":    otherScope.AssignmentDigest,
		"agent":    otherAgent.AssignmentDigest,
		"manifest": otherManifest.AssignmentDigest,
	} {
		if digest == baseline.AssignmentDigest {
			t.Errorf("assignment digest did not change for %s", name)
		}
	}
}
