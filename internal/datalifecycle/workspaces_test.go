package datalifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

// writeWorkspace creates a workspace in the layout's workspaces root, owned by
// runDir when it is non-empty, and returns its path.
func writeWorkspace(t *testing.T, root, name, runDir string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	writeFile(t, filepath.Join(dir, "r", "main.go"), "package main")
	if runDir != "" {
		data, err := json.Marshal(runartifact.WorkspaceOwner{SchemaVersion: 1, RunDir: runDir, Role: runartifact.WorkspaceRoleReviewer, AgentID: "harness:alpha"})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		writeFile(t, filepath.Join(dir, runartifact.WorkspaceOwnerFile), string(data))
	}
	return dir
}

func TestPruneSweepsWorkspacesWhoseRunIsGone(t *testing.T) {
	layout := testLayout(t)
	root := layout.WorkspacesRoot()
	kept := testRun(layout, "kept", ledger.PostModeLive, testNow())
	expired := testRun(layout, "expired", ledger.PostModeDryRun, testNow().Add(-2*DryRunRetention))
	writeFile(t, filepath.Join(kept.ArtifactPath, "rollup.md"), "keep")
	writeFile(t, filepath.Join(expired.ArtifactPath, "rollup.md"), "expire")

	keptWorkspace := writeWorkspace(t, root, "aaaaaaaaaaaa", kept.ArtifactPath)
	expiredWorkspace := writeWorkspace(t, root, "bbbbbbbbbbbb", expired.ArtifactPath)
	goneWorkspace := writeWorkspace(t, root, "cccccccccccc", filepath.Join(layout.DataRoot, "runs", "deleted-long-ago"))
	staleUnowned := writeWorkspace(t, root, "dddddddddddd", "")
	backdate(t, staleUnowned, testNow().Add(-2*OrphanGrace))
	freshUnowned := writeWorkspace(t, root, "eeeeeeeeeeee", "")
	backdate(t, freshUnowned, testNow().Add(-OrphanGrace/2))
	store := &fakeStore{runs: []ledger.Run{kept, expired}}
	opts := Options{Layout: layout, Store: store, Now: testNow}

	dry, err := Prune(context.Background(), opts, PruneOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	want := []string{expiredWorkspace, goneWorkspace, staleUnowned}
	if got := orphanPaths(dry.OrphansRemoved); !reflect.DeepEqual(got, want) {
		t.Fatalf("dry-run orphans = %#v, want the workspaces of the expiring and missing runs plus the stale unowned one %#v", got, want)
	}
	for _, path := range want {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed %s: %v", path, err)
		}
	}

	result, err := Prune(context.Background(), opts, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := runItemIDs(result.DeletedRuns); !reflect.DeepEqual(got, []string{"expired"}) {
		t.Fatalf("deleted runs = %#v, want expired", got)
	}
	if got := orphanPaths(result.OrphansRemoved); !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans removed = %#v, want %#v", got, want)
	}
	for _, path := range want {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s stat err = %v, want removed", path, err)
		}
	}
	for _, path := range []string{keptWorkspace, freshUnowned} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed, want kept: %v", path, err)
		}
	}
}

func TestPruneNeverFollowsSymlinkedWorkspaces(t *testing.T) {
	layout := testLayout(t)
	target := writeWorkspace(t, t.TempDir(), "outside", filepath.Join(layout.DataRoot, "runs", "gone"))
	if err := os.MkdirAll(layout.WorkspacesRoot(), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(layout.WorkspacesRoot(), "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: &fakeStore{}, Now: testNow}, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(result.OrphansRemoved) != 0 {
		t.Fatalf("orphans removed = %#v, want the symlinked workspace left alone", result.OrphansRemoved)
	}
	if _, err := os.Stat(filepath.Join(target, "r", "main.go")); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
}

func TestShowCountsWorkspaceBytesAndOrphans(t *testing.T) {
	layout := testLayout(t)
	live := testRun(layout, "live", ledger.PostModeLive, testNow())
	writeFile(t, filepath.Join(live.ArtifactPath, "rollup.md"), "12345")
	owned := writeWorkspace(t, layout.WorkspacesRoot(), "aaaaaaaaaaaa", live.ArtifactPath)
	orphan := writeWorkspace(t, layout.WorkspacesRoot(), "bbbbbbbbbbbb", filepath.Join(layout.DataRoot, "runs", "gone"))
	ownedBytes, err := dirBytes(owned)
	if err != nil {
		t.Fatalf("dirBytes: %v", err)
	}
	orphanBytes, err := dirBytes(orphan)
	if err != nil {
		t.Fatalf("dirBytes: %v", err)
	}

	stats, err := Show(context.Background(), Options{Layout: layout, Store: &fakeStore{runs: []ledger.Run{live}}, Now: testNow})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if stats.OrphanCount != 1 || stats.OrphanBytes != orphanBytes {
		t.Fatalf("orphans = %d (%d bytes), want the one workspace without a run (%d bytes)", stats.OrphanCount, stats.OrphanBytes, orphanBytes)
	}
	if want := 5 + ownedBytes + orphanBytes; stats.ArtifactBytes != want {
		t.Fatalf("ArtifactBytes = %d, want run artifacts plus workspaces = %d", stats.ArtifactBytes, want)
	}
}

// An owner record that exists but cannot be read or parsed says nothing about
// whether the run is gone, so prune keeps the workspace and warns instead of
// treating it as unowned.
func TestPruneKeepsWorkspacesWhoseOwnerCannotBeRead(t *testing.T) {
	layout := testLayout(t)
	root := layout.WorkspacesRoot()
	corrupt := writeWorkspace(t, root, "aaaaaaaaaaaa", "")
	writeFile(t, filepath.Join(corrupt, runartifact.WorkspaceOwnerFile), "{not json")
	backdate(t, corrupt, testNow().Add(-2*OrphanGrace))
	kept := []string{corrupt}

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		unreadable := writeWorkspace(t, root, "bbbbbbbbbbbb", filepath.Join(layout.DataRoot, "runs", "gone"))
		ownerFile := filepath.Join(unreadable, runartifact.WorkspaceOwnerFile)
		if err := os.Chmod(ownerFile, 0o000); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(ownerFile, 0o600) })
		backdate(t, unreadable, testNow().Add(-2*OrphanGrace))
		kept = append(kept, unreadable)
	}

	result, err := Prune(context.Background(), Options{Layout: layout, Store: &fakeStore{}, Now: testNow}, PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(result.OrphansRemoved) != 0 {
		t.Fatalf("orphans removed = %#v, want workspaces with unreadable owners kept", orphanPaths(result.OrphansRemoved))
	}
	if len(result.Warnings) != len(kept) {
		t.Fatalf("warnings = %#v, want one per kept workspace %#v", result.Warnings, kept)
	}
	for i, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", path, err)
		}
		if !strings.Contains(result.Warnings[i], path) {
			t.Fatalf("warning %q does not name %s", result.Warnings[i], path)
		}
	}
}
