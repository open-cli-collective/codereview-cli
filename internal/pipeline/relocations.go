package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

const relocationManifestSchemaVersion = 1

const reviewerContextContractVersion = "1"

type treeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

type relocationMove struct {
	OldPath string `json:"old_path"`
	Path    string `json:"path"`
	BlobOID string `json:"blob_oid"`
	Mode    string `json:"mode"`
}

type relocationManifest struct {
	SchemaVersion int              `json:"schema_version"`
	BaseSHA       string           `json:"base_sha"`
	HeadSHA       string           `json:"head_sha"`
	Moves         []relocationMove `json:"moves"`
	Digest        string           `json:"digest"`
}

type relocationReviewState struct {
	Manifest relocationManifest
	HeadTree map[string]treeEntry
}

type relocationAssignment struct {
	ManifestDigest   string
	MoveCount        int
	AssignmentDigest string
	Moves            []relocationMove
}

// parseTreeInventory decodes `git ls-tree -r -z --full-tree` output without
// treating path bytes as line- or whitespace-delimited data.
func parseTreeInventory(data []byte) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	if len(data) == 0 {
		return entries, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("pipeline: ls-tree output is missing its NUL terminator")
	}
	for _, record := range bytes.Split(data[:len(data)-1], []byte{0}) {
		if len(record) == 0 {
			return nil, fmt.Errorf("pipeline: ls-tree output contains an empty record")
		}
		metadata, path, ok := bytes.Cut(record, []byte{'\t'})
		if !ok || len(path) == 0 {
			return nil, fmt.Errorf("pipeline: malformed ls-tree record")
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 {
			return nil, fmt.Errorf("pipeline: malformed ls-tree metadata")
		}
		mode, objectType, oid := fields[0], fields[1], fields[2]
		if _, err := strconv.ParseUint(mode, 8, 32); err != nil || len(mode) < 5 || len(mode) > 6 {
			return nil, fmt.Errorf("pipeline: malformed ls-tree mode %q", mode)
		}
		if objectType != "blob" && objectType != "commit" {
			return nil, fmt.Errorf("pipeline: malformed ls-tree object type %q", objectType)
		}
		if (len(oid) != 40 && len(oid) != 64) || strings.ToLower(oid) != oid {
			return nil, fmt.Errorf("pipeline: malformed ls-tree object ID")
		}
		if _, err := hex.DecodeString(oid); err != nil {
			return nil, fmt.Errorf("pipeline: malformed ls-tree object ID")
		}
		entryPath := string(path)
		if _, exists := entries[entryPath]; exists {
			return nil, fmt.Errorf("pipeline: duplicate ls-tree path")
		}
		entries[entryPath] = treeEntry{Mode: mode, Type: objectType, OID: oid, Path: entryPath}
	}
	return entries, nil
}

func certifyRelocations(base, head map[string]treeEntry, patches []FilePatch) ([]relocationMove, error) {
	var candidates []FilePatch
	oldCounts := map[string]int{}
	newCounts := map[string]int{}
	for _, patch := range patches {
		if patch.OldPath == "" || patch.Path == "" || patch.OldPath == patch.Path || patch.Deleted {
			continue
		}
		candidates = append(candidates, patch)
		oldCounts[patch.OldPath]++
		newCounts[patch.Path]++
	}
	for _, patch := range candidates {
		if oldCounts[patch.OldPath] != 1 || newCounts[patch.Path] != 1 {
			return nil, fmt.Errorf("pipeline: ambiguous relocation candidate paths")
		}
	}
	var moves []relocationMove
	for _, patch := range candidates {
		old, oldExists := base[patch.OldPath]
		newEntry, newExists := head[patch.Path]
		_, oldStillPresent := head[patch.OldPath]
		_, newWasPresent := base[patch.Path]
		if !oldExists || !newExists || oldStillPresent || newWasPresent || !regularBlob(old) || !regularBlob(newEntry) {
			continue
		}
		if old.Mode != newEntry.Mode || old.OID != newEntry.OID {
			continue
		}
		moves = append(moves, relocationMove{
			OldPath: patch.OldPath,
			Path:    patch.Path,
			BlobOID: old.OID,
			Mode:    old.Mode,
		})
	}
	sort.Slice(moves, func(i, j int) bool {
		if moves[i].OldPath == moves[j].OldPath {
			return moves[i].Path < moves[j].Path
		}
		return moves[i].OldPath < moves[j].OldPath
	})
	return moves, nil
}

func regularBlob(entry treeEntry) bool {
	return entry.Type == "blob" && (entry.Mode == "100644" || entry.Mode == "100755")
}

func prepareRelocationManifest(ctx context.Context, gitCommand func(context.Context, string, ...string) ([]byte, error), paths ArtifactPaths, baseSHA, headSHA string, patches []FilePatch) (relocationManifest, map[string]treeEntry, error) {
	command := gitCommand
	if command == nil {
		command = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed command with structured arguments.
			if dir != "" {
				cmd.Dir = dir
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
			}
			return out, nil
		}
	}
	baseSHA = strings.ToLower(strings.TrimSpace(baseSHA))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if !fullCommitOID(baseSHA) || !fullCommitOID(headSHA) {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: relocation inventory requires full base and head commit IDs")
	}
	head, err := command(ctx, paths.WorkbenchRepoDir, "rev-parse", "HEAD")
	if err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: verify relocation workbench HEAD: %w", err)
	}
	if strings.TrimSpace(string(head)) != headSHA {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: relocation workbench HEAD does not match the pinned head commit")
	}
	for _, want := range []struct{ name, sha string }{{"base", baseSHA}, {"head", headSHA}} {
		resolved, err := command(ctx, paths.WorkbenchRepoDir, "rev-parse", "--verify", want.sha+"^{commit}")
		if err != nil {
			return relocationManifest{}, nil, fmt.Errorf("pipeline: verify pinned %s commit: %w", want.name, err)
		}
		if strings.TrimSpace(string(resolved)) != want.sha {
			return relocationManifest{}, nil, fmt.Errorf("pipeline: pinned %s commit does not resolve to its full expected ID", want.name)
		}
	}
	baseRaw, err := command(ctx, paths.WorkbenchRepoDir, "ls-tree", "-r", "-z", "--full-tree", baseSHA)
	if err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: inventory pinned base tree: %w", err)
	}
	headRaw, err := command(ctx, paths.WorkbenchRepoDir, "ls-tree", "-r", "-z", "--full-tree", headSHA)
	if err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: inventory pinned head tree: %w", err)
	}
	base, err := parseTreeInventory(baseRaw)
	if err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: parse pinned base tree: %w", err)
	}
	headTree, err := parseTreeInventory(headRaw)
	if err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: parse pinned head tree: %w", err)
	}
	moves, err := certifyRelocations(base, headTree, patches)
	if err != nil {
		return relocationManifest{}, nil, err
	}
	if err := validatePatchlessRenames(patches, moves); err != nil {
		return relocationManifest{}, nil, err
	}
	manifest, err := newRelocationManifest(baseSHA, headSHA, moves)
	if err != nil {
		return relocationManifest{}, nil, err
	}
	if strings.TrimSpace(paths.RelocationsJSON) == "" {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: relocation manifest path is required")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return relocationManifest{}, nil, err
	}
	if err := fsatomic.WriteFileAtomic(paths.RelocationsJSON, append(data, '\n'), 0o600); err != nil {
		return relocationManifest{}, nil, fmt.Errorf("pipeline: write relocation manifest: %w", err)
	}
	return manifest, headTree, nil
}

func fullCommitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func newRelocationManifest(baseSHA, headSHA string, moves []relocationMove) (relocationManifest, error) {
	moves = append([]relocationMove(nil), moves...)
	sort.Slice(moves, func(i, j int) bool {
		if moves[i].OldPath == moves[j].OldPath {
			return moves[i].Path < moves[j].Path
		}
		return moves[i].OldPath < moves[j].OldPath
	})
	manifest := relocationManifest{SchemaVersion: relocationManifestSchemaVersion, BaseSHA: baseSHA, HeadSHA: headSHA, Moves: moves}
	canonical, err := json.Marshal(struct {
		SchemaVersion int              `json:"schema_version"`
		BaseSHA       string           `json:"base_sha"`
		HeadSHA       string           `json:"head_sha"`
		Moves         []relocationMove `json:"moves"`
	}{manifest.SchemaVersion, manifest.BaseSHA, manifest.HeadSHA, manifest.Moves})
	if err != nil {
		return relocationManifest{}, err
	}
	digest := sha256.Sum256(canonical)
	manifest.Digest = hex.EncodeToString(digest[:])
	return manifest, nil
}

func (state relocationReviewState) assignment(agentID string, scope []string) relocationAssignment {
	scope = copySortedStrings(scope)
	inScope := stringSet(scope)
	moves := make([]relocationMove, 0)
	for _, move := range state.Manifest.Moves {
		if inScope[move.Path] {
			moves = append(moves, move)
		}
	}
	canonical, _ := json.Marshal(struct {
		ManifestDigest string           `json:"manifest_digest"`
		AgentID        string           `json:"agent_id"`
		Scope          []string         `json:"scope"`
		Moves          []relocationMove `json:"moves"`
	}{state.Manifest.Digest, agentID, scope, moves})
	digest := sha256.Sum256(canonical)
	return relocationAssignment{
		ManifestDigest:   state.Manifest.Digest,
		MoveCount:        len(moves),
		AssignmentDigest: hex.EncodeToString(digest[:]),
		Moves:            moves,
	}
}

func relocationMovePaths(moves []relocationMove) map[string]bool {
	paths := make(map[string]bool, len(moves))
	for _, move := range moves {
		paths[move.Path] = true
	}
	return paths
}

func relocationReviewablePatchPaths(patches []FilePatch, moves []relocationMove) []string {
	paths := reviewablePatchPaths(patches)
	seen := stringSet(paths)
	for _, move := range moves {
		if !seen[move.Path] {
			paths = append(paths, move.Path)
			seen[move.Path] = true
		}
	}
	return copySortedStrings(paths)
}

func relocationContentlessPaths(patches []FilePatch, moves []relocationMove) map[string]bool {
	contentless := contentlessPatchPaths(patches)
	for _, move := range moves {
		delete(contentless, move.Path)
	}
	return contentless
}

func validateRelocationAssessment(findings llm.Findings, assignment relocationAssignment, headTree map[string]treeEntry) ([]string, string) {
	if assignment.MoveCount == 0 {
		return nil, ""
	}
	assessment := findings.RelocationAssessment
	if assessment == nil {
		return nil, "relocation assessment was not provided"
	}
	if assessment.ManifestDigest != assignment.ManifestDigest || assessment.AssignmentDigest != assignment.AssignmentDigest {
		return nil, "relocation assessment digest did not match this assignment"
	}
	if !assessment.PathImpactReviewed {
		return nil, "relocation path-impact review was not confirmed"
	}
	if strings.TrimSpace(assessment.Basis) == "" || len(assessment.EvidenceFiles) == 0 {
		return nil, "relocation assessment requires a basis and evidence files"
	}
	evidence := stringSet(append(append([]string(nil), findings.InspectedFiles...), findings.ContextFiles...))
	for _, path := range assessment.EvidenceFiles {
		entry, exists := headTree[path]
		if !evidence[path] || !exists || !regularBlob(entry) || !safeRepoRelativePath(path) {
			return nil, "relocation assessment evidence must be inspected, safe pinned-head files"
		}
	}
	skipped := stringSet(findings.SkippedFiles)
	reviewed := make([]string, 0, assignment.MoveCount)
	for _, move := range assignment.Moves {
		if !skipped[move.Path] {
			reviewed = append(reviewed, move.Path)
		}
	}
	return copySortedStrings(reviewed), ""
}

func validatePatchlessRenames(patches []FilePatch, moves []relocationMove) error {
	certified := make(map[string]bool, len(moves))
	for _, move := range moves {
		certified[move.OldPath+"\x00"+move.Path] = true
	}
	for _, patch := range patches {
		if patch.OldPath == "" || patch.Path == "" || patch.OldPath == patch.Path || patch.Deleted || len(patch.Hunks) > 0 {
			continue
		}
		if certified[patch.OldPath+"\x00"+patch.Path] {
			continue
		}
		return fmt.Errorf("pipeline: rename %q to %q has no certified identical-file evidence or meaningful provider diff", patch.OldPath, patch.Path)
	}
	return nil
}

func safeHeadContextPaths(entries map[string]treeEntry) map[string]bool {
	paths := make(map[string]bool, len(entries))
	for path, entry := range entries {
		if regularBlob(entry) && safeRepoRelativePath(path) {
			paths[path] = true
		}
	}
	return paths
}

func safeRepoRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	if len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '/' {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
	}
	return true
}
