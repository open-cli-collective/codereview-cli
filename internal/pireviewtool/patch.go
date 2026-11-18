package pireviewtool

import (
	"context"
	"crypto/sha1" // #nosec G505 -- Git SHA-1 blob identity, not authentication.
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxPatchBytes     = 2 << 20
	minLongLine       = 8192
	patchAnchorBytes  = 24
	maxPatchEditBytes = 256
	maxPatchEdits     = 32
)

var singleLineHunk = regexp.MustCompile(`^@@ -[1-9][0-9]*(,1)? \+[1-9][0-9]*(,1)? @@( .*)?$`)

type patchEdit struct {
	oldOffset, newOffset int
	before, after        string
}

func readGeneratedPatch(ctx context.Context, config Config, request Request, file io.Reader) (string, error) {
	if filepath.Ext(request.Path) != ".patch" || request.Offset != 0 || request.Limit != 0 {
		return "", fmt.Errorf("%w: patch view requires an unranged .patch file read", ErrDenied)
	}
	blob := config.AddedPatchBlobs[request.Path]
	if blob == "" || config.BaseSHA == "" || config.HeadSHA == "" {
		return "", fmt.Errorf("%w: compact comparison requires a newly added patch proven by the pinned trees; inspect the outer diff for existing patches", ErrDenied)
	}
	input, err := io.ReadAll(io.LimitReader(file, maxPatchBytes+1))
	if err != nil {
		return "", err
	}
	if len(input) > maxPatchBytes || !utf8.Valid(input) || strings.ContainsRune(string(input), 0) {
		return "", fmt.Errorf("%w: patch is not bounded UTF-8 text", ErrDenied)
	}
	if patchBlobOID(input, len(blob)) != blob {
		return "", fmt.Errorf("%w: patch bytes do not match the pinned head blob", ErrDenied)
	}
	return generatedPatchReport(ctx, config, string(input))
}

func patchBlobOID(input []byte, hexLength int) string {
	var digest hash.Hash
	switch hexLength {
	case 40:
		digest = sha1.New() // #nosec G401 -- compare bytes to legacy Git blob identity.
	case 64:
		digest = sha256.New()
	default:
		return ""
	}
	fmt.Fprintf(digest, "blob %d\x00", len(input))
	_, _ = digest.Write(input)
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func generatedPatchReport(ctx context.Context, config Config, input string) (string, error) {
	var report strings.Builder
	fmt.Fprintf(&report, "Complete patch inspection (static only)\nBase: %s\nHead: %s\nPatch SHA-256: %x\n", config.BaseSHA, config.HeadSHA, sha256.Sum256([]byte(input)))
	lines := strings.SplitAfter(input, "\n")
	compared := false
	for i := 0; i < len(lines); i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line := lines[i]
		report.WriteString(line)
		if singleLineHunk.MatchString(strings.TrimRight(line, "\r\n")) && i+2 < len(lines) {
			before := strings.TrimSuffix(lines[i+1], "\n")
			next := i + 2
			oldEOF := lines[next] == "\\ No newline at end of file\n"
			if oldEOF {
				next++
			}
			if next < len(lines) && len(before) > minLongLine && strings.HasPrefix(before, "-") {
				after := strings.TrimSuffix(lines[next], "\n")
				if len(after) > minLongLine && strings.HasPrefix(after, "+") && after[1:] != before[1:] {
					oldText, newText := before[1:], after[1:]
					edits, err := compareGeneratedLine(ctx, oldText, newText)
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&report, "[One-line replacement at patch line %d; zero-based byte offsets]\nOld: %d bytes SHA-256 %x\nNew: %d bytes SHA-256 %x\n", i+2, len(oldText), sha256.Sum256([]byte(oldText)), len(newText), sha256.Sum256([]byte(newText)))
					if oldEOF {
						report.WriteString("Old line has no trailing newline.\n")
					}
					for _, edit := range edits {
						fmt.Fprintf(&report, "  old[%d:%d] %s -> new[%d:%d] %s; context %s | %s\n", edit.oldOffset, edit.oldOffset+len(edit.before), strconv.Quote(edit.before), edit.newOffset, edit.newOffset+len(edit.after), strconv.Quote(edit.after), strconv.Quote(oldText[max(0, edit.oldOffset-24):edit.oldOffset]), strconv.Quote(oldText[edit.oldOffset+len(edit.before):min(len(oldText), edit.oldOffset+len(edit.before)+24)]))
					}
					report.WriteString("All bytes outside the listed ranges matched exactly.\n")
					compared = true
					i = next
				}
			}
		}
		if report.Len() > config.MaxOutputBytes {
			return "", fmt.Errorf("%w: complete patch report exceeds output limit; use ordinary ranged reads", ErrDenied)
		}
	}
	if !compared {
		return "", fmt.Errorf("%w: no supported long one-line replacement; use ordinary patch inspection", ErrDenied)
	}
	return report.String(), nil
}

// Walk equal bytes and resynchronize with a nearby exact anchor. Even an
// ambiguous alignment cannot hide an edit: every byte is compared or emitted.
func compareGeneratedLine(ctx context.Context, oldText, newText string) ([]patchEdit, error) {
	var edits []patchEdit
	oldPos, newPos := 0, 0
	for oldPos < len(oldText) || newPos < len(newText) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for oldPos < len(oldText) && newPos < len(newText) && oldText[oldPos] == newText[newPos] {
			oldPos++
			newPos++
		}
		if oldPos == len(oldText) && newPos == len(newText) {
			break
		}
		oldRest, newRest := oldText[oldPos:], newText[newPos:]
		oldCount, newCount := len(oldRest), len(newRest)
		if a, b, ok := findPatchAnchor(oldRest, newRest); ok {
			oldCount, newCount = a, b
		}
		if len(edits) == maxPatchEdits || oldCount > maxPatchEditBytes || newCount > maxPatchEditBytes {
			return nil, fmt.Errorf("%w: generated comparison exceeds edit bounds; use ordinary ranged reads", ErrDenied)
		}
		edits = append(edits, patchEdit{oldPos, newPos, oldRest[:oldCount], newRest[:newCount]})
		oldPos += oldCount
		newPos += newCount
	}
	return edits, nil
}

func findPatchAnchor(oldText, newText string) (int, int, bool) {
	oldLimit := min(maxPatchEditBytes, len(oldText)-patchAnchorBytes)
	newLimit := min(maxPatchEditBytes, len(newText)-patchAnchorBytes)
	if oldLimit < 0 || newLimit < 0 {
		return 0, 0, false
	}
	anchors := make(map[string]int, oldLimit+1)
	for i := 0; i <= oldLimit; i++ {
		anchor := oldText[i : i+patchAnchorBytes]
		if _, exists := anchors[anchor]; !exists {
			anchors[anchor] = i
		}
	}
	oldCount, newCount := 0, 0
	cost := 2*maxPatchEditBytes + 1
	for j := 0; j <= newLimit; j++ {
		if i, ok := anchors[newText[j:j+patchAnchorBytes]]; ok && i+j > 0 && i+j < cost {
			oldCount, newCount, cost = i, j, i+j
		}
	}
	return oldCount, newCount, cost <= 2*maxPatchEditBytes
}
