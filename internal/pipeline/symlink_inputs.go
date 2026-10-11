package pipeline

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/symlinkmetadata"
)

func prepareSymlinkMetadata(ctx context.Context, gitCommand func(context.Context, string, ...string) ([]byte, error), paths ArtifactPaths, baseSHA, headSHA string, baseTree, headTree map[string]treeEntry, patches []FilePatch) (symlinkmetadata.Artifact, error) {
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
	linksByPath := make(map[string]symlinkmetadata.Link)
	for _, patch := range patches {
		oldPath := patch.OldPath
		if oldPath == "" {
			oldPath = patch.Path
		}
		baseEntry, hasBase := baseTree[oldPath]
		headEntry, hasHead := headTree[patch.Path]
		baseIsLink := hasBase && baseEntry.Type == "blob" && baseEntry.Mode == "120000"
		headIsLink := hasHead && headEntry.Type == "blob" && headEntry.Mode == "120000"
		if !baseIsLink && !headIsLink {
			continue
		}
		link, exists := linksByPath[patch.Path]
		if !exists {
			link = symlinkmetadata.Link{OldPath: oldPath, Path: patch.Path}
		} else if link.OldPath != oldPath {
			return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: ambiguous changed symlink path %q", patch.Path)
		}
		if baseIsLink {
			if link.Base == nil {
				side, err := pinnedSymlinkSide(ctx, command, paths.WorkbenchRepoDir, baseEntry, oldPath, baseTree)
				if err != nil {
					return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: inspect pinned base symlink %q: %w", oldPath, err)
				}
				link.Base = &side
			}
		}
		if headIsLink {
			if link.Head == nil {
				side, err := pinnedSymlinkSide(ctx, command, paths.WorkbenchRepoDir, headEntry, patch.Path, headTree)
				if err != nil {
					return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: inspect pinned head symlink %q: %w", patch.Path, err)
				}
				link.Head = &side
			}
		}
		linksByPath[patch.Path] = link
	}
	links := make([]symlinkmetadata.Link, 0, len(linksByPath))
	for _, link := range linksByPath {
		links = append(links, link)
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Path == links[j].Path {
			return links[i].OldPath < links[j].OldPath
		}
		return links[i].Path < links[j].Path
	})
	artifact, err := symlinkmetadata.New(strings.ToLower(baseSHA), strings.ToLower(headSHA), links)
	if err != nil {
		return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: build symlink metadata: %w", err)
	}
	if strings.TrimSpace(paths.SymlinkMetadataJSON) == "" {
		return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: symlink metadata artifact path is required")
	}
	data, err := artifact.Marshal()
	if err != nil {
		return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: encode symlink metadata: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.SymlinkMetadataJSON, append(data, '\n'), 0o600); err != nil {
		return symlinkmetadata.Artifact{}, fmt.Errorf("pipeline: write symlink metadata: %w", err)
	}
	return artifact, nil
}

func pinnedSymlinkSide(ctx context.Context, command func(context.Context, string, ...string) ([]byte, error), repoDir string, entry treeEntry, linkPath string, tree map[string]treeEntry) (symlinkmetadata.Side, error) {
	side := symlinkmetadata.Side{Path: linkPath, Mode: entry.Mode, BlobOID: entry.OID}
	sizeRaw, err := command(ctx, repoDir, "cat-file", "-s", entry.OID)
	if err != nil {
		return side, fmt.Errorf("read symlink blob size: %w", err)
	}
	sizeText := strings.TrimSpace(string(sizeRaw))
	size, err := strconv.Atoi(sizeText)
	if err != nil || size < 0 {
		return side, fmt.Errorf("symlink blob size %q is outside the supported bound", sizeText)
	}
	side.PayloadSize = size
	if size > symlinkmetadata.MaxPayloadBytes {
		side.PayloadOmittedReason = "oversize"
		side.Resolution = symlinkmetadata.ResolutionUnsupported
		return side, nil
	}
	payload, err := command(ctx, repoDir, "cat-file", "blob", entry.OID)
	if err != nil {
		return side, fmt.Errorf("read symlink blob: %w", err)
	}
	if len(payload) != size {
		return side, fmt.Errorf("symlink blob size changed while reading")
	}
	if utf8.Valid(payload) {
		side.Payload = string(payload)
	} else {
		side.PayloadBase64 = base64.StdEncoding.EncodeToString(payload)
		side.Resolution = symlinkmetadata.ResolutionUnsupported
		return side, nil
	}
	resolution, resolvedPath, target, err := resolveSymlinkTarget(linkPath, payload, tree)
	if err != nil {
		return side, err
	}
	side.Resolution = resolution
	side.ResolvedPath = resolvedPath
	if target != nil {
		side.TargetMode = target.Mode
		side.TargetBlobOID = target.OID
	}
	return side, nil
}

func resolveSymlinkTarget(linkPath string, payload []byte, tree map[string]treeEntry) (string, string, *treeEntry, error) {
	if len(payload) == 0 || len(payload) > symlinkmetadata.MaxPayloadBytes || strings.IndexByte(string(payload), 0) >= 0 {
		return symlinkmetadata.ResolutionUnsupported, "", nil, nil
	}
	value := string(payload)
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) || hasWindowsDrivePrefix(value) {
		return symlinkmetadata.ResolutionOutside, "", nil, nil
	}
	if strings.ContainsRune(value, '\\') {
		return symlinkmetadata.ResolutionUnsupported, "", nil, nil
	}
	components := strings.Split(value, "/")
	stack := strings.Split(path.Dir(linkPath), "/")
	if len(stack) == 1 && stack[0] == "." {
		stack = nil
	}
	for index, component := range components {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return symlinkmetadata.ResolutionOutside, "", nil, nil
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, component)
			current := strings.Join(stack, "/")
			if component == ".git" {
				return symlinkmetadata.ResolutionUnsupported, current, nil, nil
			}
			if index < len(components)-1 {
				if entry, ok := tree[current]; ok && entry.Type == "blob" && entry.Mode == "120000" {
					return symlinkmetadata.ResolutionLinkedAncestor, current, nil, nil
				}
			}
		}
	}
	resolved := strings.Join(stack, "/")
	if resolved == "" {
		return symlinkmetadata.ResolutionOutside, "", nil, nil
	}
	if target, ok := tree[resolved]; ok {
		switch {
		case target.Type == "blob" && (target.Mode == "100644" || target.Mode == "100755"):
			return symlinkmetadata.ResolutionRegular, resolved, &target, nil
		case target.Type == "blob" && target.Mode == "120000":
			return symlinkmetadata.ResolutionLinkedTarget, resolved, nil, nil
		default:
			return symlinkmetadata.ResolutionUnsupported, resolved, nil, nil
		}
	}
	for candidate := range tree {
		if strings.HasPrefix(candidate, resolved+"/") {
			return symlinkmetadata.ResolutionUnsupported, resolved, nil, nil
		}
	}
	return symlinkmetadata.ResolutionMissing, resolved, nil, nil
}

func hasWindowsDrivePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	return value[0] >= 'a' && value[0] <= 'z' || value[0] >= 'A' && value[0] <= 'Z'
}
