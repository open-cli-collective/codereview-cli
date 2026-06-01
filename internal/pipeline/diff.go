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
	rest := strings.TrimSpace(strings.TrimPrefix(line, "diff --git "))
	oldPath, rest, ok := consumeDiffPath(rest)
	if !ok {
		return "", ""
	}
	newPath, _, ok := consumeDiffPath(strings.TrimSpace(rest))
	if !ok {
		return "", ""
	}
	return trimDiffPrefix(oldPath), trimDiffPrefix(newPath)
}

func parseHeaderPath(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
	}
	return trimDiffPrefix(value)
}

func trimDiffPrefix(path string) string {
	path = strings.TrimSpace(strings.Trim(path, `"`))
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}
	return path
}

func consumeDiffPath(raw string) (string, string, bool) {
	if raw == "" {
		return "", "", false
	}
	if raw[0] != '"' {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			return "", "", false
		}
		return fields[0], strings.TrimPrefix(raw, fields[0]), true
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] != '"' {
			continue
		}
		token := raw[:i+1]
		unquoted, err := strconv.Unquote(token)
		if err != nil {
			return "", "", false
		}
		return unquoted, raw[i+1:], true
	}
	return "", "", false
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
		OldEnd:       hunkRangeEnd(oldStart, oldCount),
		NewStart:     newStart,
		NewEnd:       hunkRangeEnd(newStart, newCount),
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

func hunkRangeEnd(start, count int) int {
	if count <= 0 {
		return start
	}
	return start + count - 1
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
