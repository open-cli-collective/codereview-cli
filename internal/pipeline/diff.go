package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

var hunkHeaderRE = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

// FilePatch is one whole-file diff slice.
type FilePatch struct {
	OldPath string
	Path    string
	Patch   string
	Binary  bool
	Deleted bool
	Hunks   []reviewplan.DiffHunk
}

// ParsedDiff contains planner metadata plus whole-file patches.
type ParsedDiff struct {
	PlanDiff reviewplan.Diff
	Patches  []FilePatch
}

func parseUnifiedDiff(raw string) (ParsedDiff, error) {
	var result ParsedDiff
	var current *FilePatch
	var patch strings.Builder
	patchLine := 0

	flush := func() {
		if current == nil {
			return
		}
		current.Patch = patch.String()
		if current.Path == "" {
			current.Path = current.OldPath
		}
		if current.OldPath == "" {
			current.OldPath = current.Path
		}
		result.Patches = append(result.Patches, *current)
		result.PlanDiff.Files = append(result.PlanDiff.Files, reviewplan.DiffFile{
			OldPath: current.OldPath,
			Path:    current.Path,
			Binary:  current.Binary,
			Deleted: current.Deleted,
			Hunks:   append([]reviewplan.DiffHunk(nil), current.Hunks...),
		})
	}

	for _, line := range splitLines(raw) {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			oldPath, newPath := parseDiffGitPaths(line)
			current = &FilePatch{OldPath: oldPath, Path: newPath}
			patch.Reset()
			patchLine = 0
		}
		if current == nil {
			continue
		}
		patch.WriteString(line)
		patchLine++
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "--- "):
			path := parseHeaderPath(strings.TrimPrefix(trimmed, "--- "))
			if path != "" {
				current.OldPath = path
			}
		case strings.HasPrefix(trimmed, "+++ "):
			path := parseHeaderPath(strings.TrimPrefix(trimmed, "+++ "))
			if path != "" {
				current.Path = path
			} else if strings.TrimSpace(strings.TrimPrefix(trimmed, "+++ ")) == "/dev/null" {
				current.Deleted = true
			}
		case strings.HasPrefix(trimmed, "deleted file mode"):
			current.Deleted = true
		case strings.HasPrefix(trimmed, "rename from "):
			current.OldPath = strings.TrimPrefix(trimmed, "rename from ")
		case strings.HasPrefix(trimmed, "rename to "):
			current.Path = strings.TrimPrefix(trimmed, "rename to ")
		case strings.HasPrefix(trimmed, "Binary files ") || strings.HasPrefix(trimmed, "GIT binary patch"):
			current.Binary = true
		case strings.HasPrefix(trimmed, "@@ "):
			hunk, err := parseHunkHeader(trimmed, patchLine)
			if err != nil {
				return ParsedDiff{}, err
			}
			current.Hunks = append(current.Hunks, hunk)
		}
	}
	flush()
	return result, nil
}

func splitLines(raw string) []string {
	if raw == "" {
		return nil
	}
	lines := strings.SplitAfter(raw, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseDiffGitPaths(line string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 4 {
		return "", ""
	}
	return trimDiffPrefix(fields[2]), trimDiffPrefix(fields[3])
}

func parseHeaderPath(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "/dev/null" {
		return ""
	}
	value = strings.Trim(value, `"`)
	return trimDiffPrefix(value)
}

func trimDiffPrefix(path string) string {
	path = strings.TrimSpace(strings.Trim(path, `"`))
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}
	return path
}

func parseHunkHeader(line string, diffPosition int) (reviewplan.DiffHunk, error) {
	matches := hunkHeaderRE.FindStringSubmatch(line)
	if matches == nil {
		return reviewplan.DiffHunk{}, fmt.Errorf("pipeline: parse hunk header %q", line)
	}
	oldStart, oldCount, err := hunkRange(matches[1], matches[2])
	if err != nil {
		return reviewplan.DiffHunk{}, err
	}
	newStart, newCount, err := hunkRange(matches[3], matches[4])
	if err != nil {
		return reviewplan.DiffHunk{}, err
	}
	hunk := reviewplan.DiffHunk{
		OldStart:     oldStart,
		OldEnd:       oldStart + oldCount - 1,
		NewStart:     newStart,
		NewEnd:       newStart + newCount - 1,
		DiffPosition: diffPosition,
	}
	switch {
	case newCount > 0:
		hunk.FallbackSide = review.DiffSideRight
		hunk.FallbackLine = newStart
	case oldCount > 0:
		hunk.FallbackSide = review.DiffSideLeft
		hunk.FallbackLine = oldStart
	}
	return hunk, nil
}

func hunkRange(startRaw, countRaw string) (int, int, error) {
	start, err := strconv.Atoi(startRaw)
	if err != nil {
		return 0, 0, err
	}
	if countRaw == "" {
		return start, 1, nil
	}
	count, err := strconv.Atoi(countRaw)
	if err != nil {
		return 0, 0, err
	}
	return start, count, nil
}
