package gitlab

import (
	"strconv"
	"strings"
)

// lineAnchor classifies one target line of a per-file diff and carries the
// counterpart line number on the other diff side when the line is unchanged
// context. GitLab diff-note positions require both sides for context lines.
type lineAnchor struct {
	found       bool
	changed     bool
	counterpart int
}

// anchorForNewLine locates 1-based line number on the new side of a per-file
// unified diff fragment (hunks only, as served by the GitLab diffs API).
func anchorForNewLine(diff string, line int) lineAnchor {
	return anchorForLine(diff, line, false)
}

// anchorForOldLine locates a 1-based line number on the old side.
func anchorForOldLine(diff string, line int) lineAnchor {
	return anchorForLine(diff, line, true)
}

func anchorForLine(diff string, target int, oldSide bool) lineAnchor {
	lines := strings.Split(diff, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	oldLine, newLine := 0, 0
	inHunk := false
	for _, raw := range lines {
		if strings.HasPrefix(raw, "@@") {
			oldStart, newStart, ok := parseHunkHeader(raw)
			if !ok {
				return lineAnchor{}
			}
			oldLine, newLine = oldStart, newStart
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			if !oldSide && newLine == target {
				return lineAnchor{found: true, changed: true}
			}
			newLine++
		case strings.HasPrefix(raw, "-"):
			if oldSide && oldLine == target {
				return lineAnchor{found: true, changed: true}
			}
			oldLine++
		case strings.HasPrefix(raw, "\\"):
			// "\ No newline at end of file" advances neither side.
		default:
			if oldSide && oldLine == target {
				return lineAnchor{found: true, counterpart: newLine}
			}
			if !oldSide && newLine == target {
				return lineAnchor{found: true, counterpart: oldLine}
			}
			oldLine++
			newLine++
		}
	}
	return lineAnchor{}
}

func parseHunkHeader(header string) (oldStart, newStart int, ok bool) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, false
	}
	oldStart, ok = parseHunkStart(strings.TrimPrefix(fields[1], "-"))
	if !ok {
		return 0, 0, false
	}
	newStart, ok = parseHunkStart(strings.TrimPrefix(fields[2], "+"))
	if !ok {
		return 0, 0, false
	}
	return oldStart, newStart, true
}

func parseHunkStart(spec string) (int, bool) {
	if comma := strings.Index(spec, ","); comma >= 0 {
		spec = spec[:comma]
	}
	start, err := strconv.Atoi(spec)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}
