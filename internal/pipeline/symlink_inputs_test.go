package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/symlinkmetadata"
)

func TestPrepareSymlinkMetadataUsesPinnedTreesForMovedAndDanglingLinks(t *testing.T) {
	repo := t.TempDir()
	relocationTestGit(t, repo, "init", "-q")
	relocationTestGit(t, repo, "config", "user.name", "CR Tests")
	relocationTestGit(t, repo, "config", "user.email", "cr-tests@example.invalid")
	for _, dir := range []string{"assets", "target", "links"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "target", "file.txt"), []byte("do not read target body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target/file.txt", filepath.Join(repo, "assets", "current")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink("../missing", filepath.Join(repo, "links", "dangling")); err != nil {
		t.Fatal(err)
	}
	relocationTestGit(t, repo, "add", "-A")
	relocationTestGit(t, repo, "commit", "-qm", "base")
	baseSHA := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))
	relocationTestGit(t, repo, "mv", "assets/current", "assets/renamed")
	relocationTestGit(t, repo, "mv", "links/dangling", "links/dangling-new")
	if err := os.MkdirAll(filepath.Join(repo, "relocated"), 0o700); err != nil {
		t.Fatal(err)
	}
	relocationTestGit(t, repo, "mv", "target/file.txt", "relocated/file.txt")
	if err := os.WriteFile(filepath.Join(repo, "residual.txt"), []byte("unrelated body change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relocationTestGit(t, repo, "add", "-A")
	relocationTestGit(t, repo, "commit", "-qm", "rename links and move target")
	headSHA := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))
	diff := string(relocationTestGit(t, repo, "diff", "--find-renames=100%", "--no-ext-diff", baseSHA, headSHA))
	parsed, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse actual Git diff: %v", err)
	}

	var commands [][]string
	gitCommand := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string(nil), args...))
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed Git executable against this temporary repository.
		cmd.Dir = dir
		return cmd.CombinedOutput()
	}
	paths := runartifact.FromDir(filepath.Join(t.TempDir(), "run"))
	paths.WorkbenchRepoDir = repo
	manifest, baseTree, headTree, err := prepareRelocationManifest(context.Background(), gitCommand, paths, baseSHA, headSHA, parsed.Patches)
	if err != nil {
		t.Fatalf("prepareRelocationManifest: %v", err)
	}
	if _, credited := relocationMovePaths(manifest.Moves)["assets/renamed"]; credited {
		t.Fatal("symlink rename received regular-file relocation credit")
	}
	artifact, err := prepareSymlinkMetadata(context.Background(), gitCommand, paths, baseSHA, headSHA, baseTree, headTree, parsed.Patches)
	if err != nil {
		t.Fatalf("prepareSymlinkMetadata: %v", err)
	}
	if len(artifact.Links) != 2 || artifact.Digest == "" {
		t.Fatalf("artifact links/digest = %d/%q, want both changed links and digest", len(artifact.Links), artifact.Digest)
	}
	byPath := map[string]symlinkmetadata.Link{}
	for _, link := range artifact.Links {
		byPath[link.Path] = link
	}
	moved := byPath["assets/renamed"]
	if moved.OldPath != "assets/current" || moved.Base == nil || moved.Head == nil {
		t.Fatalf("moved-link metadata = %#v", moved)
	}
	if moved.Base.Payload != "../target/file.txt" || moved.Base.Resolution != symlinkmetadata.ResolutionRegular || moved.Base.TargetBlobOID != baseTree["target/file.txt"].OID {
		t.Fatalf("base moved-link payload/resolution = %#v, want pinned regular target identity", moved.Base)
	}
	if moved.Head.Payload != "../target/file.txt" || moved.Head.Resolution != symlinkmetadata.ResolutionMissing || moved.Head.ResolvedPath != "target/file.txt" {
		t.Fatalf("head moved-link payload/resolution = %#v, want target-moved missing status", moved.Head)
	}
	dangling := byPath["links/dangling-new"]
	if dangling.Base == nil || dangling.Head == nil || dangling.Base.Resolution != symlinkmetadata.ResolutionMissing || dangling.Head.Resolution != symlinkmetadata.ResolutionMissing {
		t.Fatalf("dangling rename metadata = %#v, want missing in both pinned trees", dangling)
	}
	for _, command := range commands {
		if len(command) >= 3 && command[0] == "cat-file" && command[1] == "blob" && command[2] == baseTree["target/file.txt"].OID {
			t.Fatal("producer fetched target body instead of only the symlink payload")
		}
	}
	data, err := os.ReadFile(paths.SymlinkMetadataJSON)
	if err != nil {
		t.Fatalf("read symlink metadata artifact: %v", err)
	}
	decoded, _, err := symlinkmetadata.Decode(data, artifact.Digest, baseSHA, headSHA, "assets/renamed")
	if err != nil || decoded.Digest != artifact.Digest {
		t.Fatalf("decode written artifact = digest %q, err %v", decoded.Digest, err)
	}
}

func TestResolveSymlinkTargetClassifiesWithoutFollowingLinks(t *testing.T) {
	regular := treeEntry{Mode: "100644", Type: "blob", OID: strings.Repeat("a", 40), Path: "target/file"}
	linked := treeEntry{Mode: "120000", Type: "blob", OID: strings.Repeat("b", 40), Path: "target/link"}
	ancestor := treeEntry{Mode: "120000", Type: "blob", OID: strings.Repeat("c", 40), Path: "target/parent"}
	for _, tc := range []struct {
		name       string
		linkPath   string
		payload    string
		tree       map[string]treeEntry
		wantStatus string
		wantPath   string
		wantTarget bool
	}{
		{name: "regular", linkPath: "links/current", payload: "../target/file", tree: map[string]treeEntry{"target/file": regular}, wantStatus: symlinkmetadata.ResolutionRegular, wantPath: "target/file", wantTarget: true},
		{name: "missing", linkPath: "links/current", payload: "../missing", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionMissing, wantPath: "missing"},
		{name: "absolute", linkPath: "links/current", payload: "/outside", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "windows drive absolute", linkPath: "links/current", payload: "C:\\outside\\target", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "windows drive relative", linkPath: "links/current", payload: "C:outside", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "windows UNC", linkPath: "links/current", payload: "\\\\server\\share\\target", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "windows rooted path", linkPath: "links/current", payload: "\\outside\\target", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "ambiguous backslash path", linkPath: "links/current", payload: "..\\target\\file", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionUnsupported},
		{name: "escaping", linkPath: "links/current", payload: "../../outside", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionOutside},
		{name: "git metadata", linkPath: "links/current", payload: "../.git/config", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionUnsupported, wantPath: ".git/config"},
		{name: "chained target", linkPath: "links/current", payload: "../target/link", tree: map[string]treeEntry{"target/link": linked}, wantStatus: symlinkmetadata.ResolutionLinkedTarget, wantPath: "target/link"},
		{name: "cycle endpoint is not followed", linkPath: "links/current", payload: "../target/link", tree: map[string]treeEntry{"target/link": linked, "links/current": linked}, wantStatus: symlinkmetadata.ResolutionLinkedTarget, wantPath: "target/link"},
		{name: "linked ancestor", linkPath: "links/current", payload: "../target/parent/child", tree: map[string]treeEntry{"target/parent": ancestor}, wantStatus: symlinkmetadata.ResolutionLinkedAncestor, wantPath: "target/parent/child"},
		{name: "NUL unsupported", linkPath: "links/current", payload: "../target\x00/file", tree: map[string]treeEntry{}, wantStatus: symlinkmetadata.ResolutionUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, resolved, target, err := resolveSymlinkTarget(tc.linkPath, []byte(tc.payload), tc.tree)
			if err != nil || status != tc.wantStatus || resolved != tc.wantPath || (target != nil) != tc.wantTarget {
				t.Fatalf("resolve = %q, %q, %#v, %v; want %q, %q, target=%t", status, resolved, target, err, tc.wantStatus, tc.wantPath, tc.wantTarget)
			}
		})
	}
}

func TestPrepareSymlinkMetadataCapturesChangedPayloadAndFileTypeTransitions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseLink bool
		headLink bool
	}{
		{name: "changed payload", baseLink: true, headLink: true},
		{name: "regular to symlink", headLink: true},
		{name: "symlink to regular", baseLink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			relocationTestGit(t, repo, "init", "-q")
			relocationTestGit(t, repo, "config", "user.name", "CR Tests")
			relocationTestGit(t, repo, "config", "user.email", "cr-tests@example.invalid")
			if err := os.MkdirAll(filepath.Join(repo, "links"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, path), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeFile("old-target", "old target\n")
			writeFile("new-target", "new target\n")
			if tc.baseLink {
				if err := os.Symlink("../old-target", filepath.Join(repo, "links", "current")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				writeFile("links/current", "regular file\n")
			}
			relocationTestGit(t, repo, "add", "-A")
			relocationTestGit(t, repo, "commit", "-qm", "base")
			baseSHA := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))
			if tc.headLink {
				if err := os.Remove(filepath.Join(repo, "links", "current")); err != nil {
					t.Fatal(err)
				}
				payload := "../new-target"
				if tc.name == "regular to symlink" {
					payload = "../old-target"
				}
				if err := os.Symlink(payload, filepath.Join(repo, "links", "current")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(filepath.Join(repo, "links", "current")); err != nil {
					t.Fatal(err)
				}
				writeFile("links/current", "regular replacement\n")
			}
			relocationTestGit(t, repo, "add", "-A")
			relocationTestGit(t, repo, "commit", "-qm", "head")
			headSHA := strings.TrimSpace(string(relocationTestGit(t, repo, "rev-parse", "HEAD")))
			diff := string(relocationTestGit(t, repo, "diff", "--no-ext-diff", baseSHA, headSHA))
			parsed, err := parseUnifiedDiff(diff)
			if err != nil || len(parsed.Patches) == 0 {
				t.Fatalf("parse type/payload diff: patches=%#v err=%v diff=%s", parsed.Patches, err, diff)
			}
			for _, patch := range parsed.Patches {
				if patch.Path != "links/current" {
					t.Fatalf("unexpected changed path %q in type/payload diff", patch.Path)
				}
			}
			paths := runartifact.FromDir(filepath.Join(t.TempDir(), "run"))
			paths.WorkbenchRepoDir = repo
			_, baseTree, headTree, err := prepareRelocationManifest(context.Background(), nil, paths, baseSHA, headSHA, parsed.Patches)
			if err != nil {
				t.Fatalf("prepareRelocationManifest: %v", err)
			}
			artifact, err := prepareSymlinkMetadata(context.Background(), nil, paths, baseSHA, headSHA, baseTree, headTree, parsed.Patches)
			if err != nil {
				t.Fatalf("prepareSymlinkMetadata: %v", err)
			}
			if len(artifact.Links) != 1 {
				t.Fatalf("metadata links = %#v, want one type/payload change", artifact.Links)
			}
			link := artifact.Links[0]
			if (link.Base != nil) != tc.baseLink || (link.Head != nil) != tc.headLink {
				t.Fatalf("base/head metadata presence = %t/%t, want %t/%t", link.Base != nil, link.Head != nil, tc.baseLink, tc.headLink)
			}
			if tc.name == "changed payload" && (link.Base.Payload != "../old-target" || link.Head.Payload != "../new-target" || link.Base.BlobOID == link.Head.BlobOID) {
				t.Fatalf("changed link payload identity = base %#v/head %#v", link.Base, link.Head)
			}
		})
	}
}

func TestPrepareSymlinkMetadataMarksOversizeUnavailableWithoutFetchingBlob(t *testing.T) {
	entry := treeEntry{Mode: "120000", Type: "blob", OID: strings.Repeat("d", 40), Path: "links/current"}
	ordinary := treeEntry{Mode: "120000", Type: "blob", OID: strings.Repeat("e", 40), Path: "links/ordinary"}
	var fetchedOversizeBlob bool
	command := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "cat-file" && args[1] == "blob" {
			if args[2] == entry.OID {
				fetchedOversizeBlob = true
			}
			if args[2] == ordinary.OID {
				return []byte("target"), nil
			}
		}
		if len(args) == 3 && args[0] == "cat-file" && args[1] == "-s" {
			if args[2] == entry.OID {
				return []byte(strconv.Itoa(symlinkmetadata.MaxPayloadBytes + 1)), nil
			}
			if args[2] == ordinary.OID {
				return []byte(strconv.Itoa(len("target"))), nil
			}
		}
		return nil, nil
	}
	paths := runartifact.FromDir(filepath.Join(t.TempDir(), "run"))
	baseTree := map[string]treeEntry{"links/current": entry, "links/ordinary": ordinary}
	headTree := map[string]treeEntry{"links/current": entry, "links/ordinary": ordinary}
	patches := []FilePatch{{OldPath: "links/current", Path: "links/current"}, {OldPath: "links/ordinary", Path: "links/ordinary"}}
	artifact, err := prepareSymlinkMetadata(context.Background(), command, paths, strings.Repeat("a", 40), strings.Repeat("b", 40), baseTree, headTree, patches)
	if err != nil {
		t.Fatalf("prepareSymlinkMetadata: %v; oversized payload should not abort unrelated symlink review", err)
	}
	if fetchedOversizeBlob {
		t.Fatal("oversize blob was fetched before its size was classified unavailable")
	}
	if len(artifact.Links) != 2 {
		t.Fatalf("oversize metadata links = %#v, want oversized and ordinary obligations", artifact.Links)
	}
	byPath := map[string]symlinkmetadata.Link{}
	for _, link := range artifact.Links {
		byPath[link.Path] = link
	}
	for _, side := range []*symlinkmetadata.Side{byPath["links/current"].Base, byPath["links/current"].Head} {
		if side == nil || side.PayloadSize != symlinkmetadata.MaxPayloadBytes+1 || side.PayloadOmittedReason != "oversize" || side.Resolution != symlinkmetadata.ResolutionUnsupported || side.Payload != "" {
			t.Fatalf("oversize side = %#v, want explicit unavailable payload metadata", side)
		}
	}
	if ordinarySide := byPath["links/ordinary"].Head; ordinarySide == nil || ordinarySide.Payload != "target" || ordinarySide.PayloadSize != len("target") {
		t.Fatalf("unrelated ordinary symlink side = %#v, want inspected payload alongside oversize link", ordinarySide)
	}
}
