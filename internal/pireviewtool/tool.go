// Package pireviewtool implements the bounded inspection surface used by Pi
// RPC reviewers. It intentionally has no shell or repository mutation API.
package pireviewtool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ToolRead reads one confined repository file.
	ToolRead = "cr_read"
	// ToolSearch searches repository files for a literal string.
	ToolSearch = "cr_search"
	// ToolList lists regular files below a confined repository path.
	ToolList = "cr_list"
	// ToolDiff reads the fixed pinned diff artifact.
	ToolDiff = "cr_diff"
)

const (
	maxConfiguredOutputBytes = 1024 * 1024
	maxConfiguredTimeoutMS   = 60_000
)

// ErrDenied marks tool requests outside the fixed read-only contract.
var ErrDenied = errors.New("pi reviewer tool request denied")

// Config fixes the roots and output limit for one reviewer invocation.
type Config struct {
	RepoDir        string   `json:"repo_dir"`
	DiffPath       string   `json:"diff_path"`
	AllowedFiles   []string `json:"allowed_files,omitempty"`
	MaxOutputBytes int      `json:"max_output_bytes"`
	TimeoutMS      int      `json:"timeout_ms"`
}

// Run implements the strict stdin/stdout protocol used by the generated Pi
// extension. It returns a process exit code and never accepts tool arguments on
// the command line.
func Run(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "--config" || strings.TrimSpace(args[1]) == "" {
		_, _ = fmt.Fprintln(stderr, "pi reviewer tool: expected --config <path>")
		return 2
	}
	configFile, err := os.Open(filepath.Clean(args[1])) // #nosec G304 -- path is supplied by the CR-owned generated extension.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pi reviewer tool: open config: %v\n", err)
		return 1
	}
	defer configFile.Close()
	var config Config
	if err := decodeStrictJSON(io.LimitReader(configFile, 1024*1024), &config); err != nil {
		_, _ = fmt.Fprintf(stderr, "pi reviewer tool: decode config: %v\n", err)
		return 1
	}
	if config.MaxOutputBytes <= 0 || config.MaxOutputBytes > maxConfiguredOutputBytes || config.TimeoutMS <= 0 || config.TimeoutMS > maxConfiguredTimeoutMS {
		_, _ = fmt.Fprintln(stderr, "pi reviewer tool: invalid output or timeout bound")
		return 1
	}
	var request Request
	if err := decodeStrictJSON(io.LimitReader(stdin, 1024*1024), &request); err != nil {
		_, _ = fmt.Fprintf(stderr, "pi reviewer tool: decode request: %v\n", err)
		return 1
	}
	ctx := parent
	cancel := func() {}
	if config.TimeoutMS > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(config.TimeoutMS)*time.Millisecond)
	}
	defer cancel()
	output, err := Execute(ctx, config, request)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pi reviewer tool: %v\n", err)
		return 1
	}
	_, _ = stdout.Write([]byte(output))
	return 0
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// Request is one CR-owned reviewer tool invocation.
type Request struct {
	Tool   string `json:"tool"`
	Path   string `json:"path,omitempty"`
	Query  string `json:"query,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Execute performs one bounded, read-only inspection operation.
func Execute(ctx context.Context, config Config, request Request) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if config.MaxOutputBytes <= 0 {
		return "", fmt.Errorf("%w: invalid output limit", ErrDenied)
	}
	root, err := validateRepoRoot(config.RepoDir)
	if err != nil {
		return "", err
	}
	var output []byte
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("%w: open repository root: %w", ErrDenied, err)
	}
	defer rootHandle.Close()
	switch request.Tool {
	case ToolRead:
		path, err := confinedPath(root, request.Path, false)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, path)
		file, openErr := rootHandle.Open(rel)
		if openErr != nil {
			return "", fmt.Errorf("read %q: %w", request.Path, openErr)
		}
		output, err = readRange(file, request.Offset, request.Limit, config.MaxOutputBytes)
		_ = file.Close()
		if err != nil {
			return "", fmt.Errorf("read %q: %w", request.Path, err)
		}
	case ToolSearch:
		if request.Query == "" {
			return "", fmt.Errorf("%w: search query is required", ErrDenied)
		}
		path, err := confinedPath(root, request.Path, true)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, path)
		output, err = search(ctx, rootHandle.FS(), rel, request.Query, config.MaxOutputBytes)
		if err != nil {
			return "", err
		}
	case ToolList:
		path, err := confinedPath(root, request.Path, true)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, path)
		output, err = list(ctx, rootHandle.FS(), rel, config.MaxOutputBytes)
		if err != nil {
			return "", err
		}
	case ToolDiff:
		output, err = readFixedDiff(config.DiffPath, request.Offset, request.Limit, config.MaxOutputBytes)
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%w: unknown tool %q", ErrDenied, request.Tool)
	}
	return boundOutput(output, config.MaxOutputBytes), nil
}

func validateRepoRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: repository root must be absolute", ErrDenied)
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("%w: repository root: %w", ErrDenied, err)
	}
	if isLinkLike(info) || !info.IsDir() {
		return "", fmt.Errorf("%w: repository root is not a real directory", ErrDenied)
	}
	return root, nil
}

func resolveRepoPath(root, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "."
	}
	if filepath.IsAbs(requested) || filepath.VolumeName(requested) != "" {
		return "", fmt.Errorf("%w: absolute or volume-qualified path", ErrDenied)
	}
	clean := filepath.Clean(requested)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path traversal", ErrDenied)
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if isVCSMetadataDir(component) {
			return "", fmt.Errorf("%w: VCS metadata is not reviewer-visible", ErrDenied)
		}
	}
	target := filepath.Join(root, clean)
	if filepath.VolumeName(root) != filepath.VolumeName(target) {
		return "", fmt.Errorf("%w: cross-volume path", ErrDenied)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: path escape", ErrDenied)
	}
	return target, nil
}

func confinedPath(root, requested string, allowDir bool) (string, error) {
	target, err := resolveRepoPath(root, requested)
	if err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(root, target)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("%w: repository root disappeared", ErrDenied)
	}
	current := root
	if rel != "." {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, statErr := os.Lstat(current)
			if statErr != nil {
				return "", fmt.Errorf("inspect %q: %w", requested, statErr)
			}
			if isLinkLike(info) || !sameFileSystem(rootInfo, info) {
				return "", fmt.Errorf("%w: linked or cross-volume path component", ErrDenied)
			}
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("inspect %q: %w", requested, err)
	}
	if isLinkLike(info) || !sameFileSystem(rootInfo, info) || (!allowDir && !info.Mode().IsRegular()) {
		return "", fmt.Errorf("%w: unsupported path type", ErrDenied)
	}
	return target, nil
}

func readFixedDiff(path string, offset int64, limit, maxBytes int) ([]byte, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: fixed diff path is invalid", ErrDenied)
	}
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("fixed diff: %w", err)
	}
	if isLinkLike(info) || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: fixed diff is not a regular file", ErrDenied)
	}
	file, err := os.Open(filepath.Clean(path)) // #nosec G304 -- the fixed path is CR-owned configuration and checked above.
	if err != nil {
		return nil, fmt.Errorf("fixed diff: %w", err)
	}
	defer file.Close()
	data, err := readRange(file, offset, limit, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("fixed diff: %w", err)
	}
	return data, nil
}

func readRange(file *os.File, offset int64, limit, maxBytes int) ([]byte, error) {
	if offset < 0 || limit < 0 {
		return nil, fmt.Errorf("%w: range offset and limit must be non-negative", ErrDenied)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	total := info.Size()
	if offset > total {
		return nil, fmt.Errorf("%w: range offset %d exceeds file size %d", ErrDenied, offset, total)
	}
	if offset == 0 && limit == 0 && total <= int64(maxBytes) {
		return io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	}
	want := limit
	if want == 0 || want > maxBytes {
		want = maxBytes
	}
	remaining := total - offset
	if int64(want) > remaining {
		want = int(remaining)
	}
rangeLoop:
	for {
		end := offset + int64(want)
		next := end
		if end >= total {
			next = -1
		}
		header := []byte(fmt.Sprintf("[cr-range offset=%d end=%d total=%d next_offset=%d]\n", offset, end, total, next))
		available := maxBytes - len(header)
		if available < 0 {
			return nil, fmt.Errorf("%w: output cap is too small for range metadata", ErrDenied)
		}
		if want > available {
			want = available
			continue
		}
		data := make([]byte, want)
		if want > 0 {
			n, readErr := file.ReadAt(data, offset)
			data = data[:n]
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
		}
		if end < total && !utf8.Valid(data) {
			for trim := 1; trim < utf8.UTFMax && trim < len(data); trim++ {
				if utf8.Valid(data[:len(data)-trim]) {
					want = len(data) - trim
					continue rangeLoop
				}
			}
		}
		return append(header, data...), nil
	}
}

func search(ctx context.Context, rootFS fs.FS, start, query string, maxBytes int) ([]byte, error) {
	var out bytes.Buffer
	err := walkRegularFiles(ctx, rootFS, start, func(path string) error {
		file, err := rootFS.Open(path)
		if err != nil {
			return err
		}
		collector := newSearchMatchCollector(maxBytes)
		binary, scanErr := searchFile(ctx, file, query, collector)
		_ = file.Close()
		if scanErr != nil {
			return scanErr
		}
		if binary {
			return nil
		}
		for _, match := range collector.matches {
			fmt.Fprintf(&out, "%s:%d:%s\n", filepath.ToSlash(path), match.line, match.text)
			if out.Len() >= maxBytes {
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type searchMatch struct {
	line int
	text string
}

// searchMatchStorageOverhead conservatively charges retained slice and string
// metadata against the same deterministic budget as match text.
const searchMatchStorageOverhead = 32

type searchMatchCollector struct {
	matches       []searchMatch
	retainedBytes int
	maxBytes      int
}

func newSearchMatchCollector(maxBytes int) *searchMatchCollector {
	return &searchMatchCollector{maxBytes: maxBytes}
}

func (c *searchMatchCollector) add(match searchMatch) {
	storageBytes := len(match.text) + searchMatchStorageOverhead
	if c.retainedBytes+storageBytes > c.maxBytes {
		return
	}
	c.matches = append(c.matches, match)
	c.retainedBytes += storageBytes
}

func searchFile(ctx context.Context, file io.Reader, query string, collector *searchMatchCollector) (bool, error) {
	if len(query) > maxConfiguredOutputBytes {
		return false, fmt.Errorf("%w: search query is too large", ErrDenied)
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	queryBytes := []byte(query)
	lineNumber := 1
	lineMatched := false
	lineBinary := false
	fileBinary := false
	lineBytes := 0
	preview := make([]byte, 0, min(collector.maxBytes, 4096))
	tail := make([]byte, 0, max(0, len(queryBytes)-1))
	finishLine := func() {
		if lineMatched && !lineBinary && collector.retainedBytes < collector.maxBytes {
			text := string(bytes.ToValidUTF8(preview, []byte("�")))
			if lineBytes > len(preview) {
				text += "…"
			}
			collector.add(searchMatch{line: lineNumber, text: text})
		}
		lineNumber++
		lineMatched = false
		lineBinary = false
		lineBytes = 0
		preview = preview[:0]
		tail = tail[:0]
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		fragment, readErr := reader.ReadSlice('\n')
		hasNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hasNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if bytes.IndexByte(fragment, 0) >= 0 {
			lineBinary = true
			fileBinary = true
		}
		lineBytes += len(fragment)
		if len(preview) < cap(preview) {
			remaining := cap(preview) - len(preview)
			if remaining > len(fragment) {
				remaining = len(fragment)
			}
			preview = append(preview, fragment[:remaining]...)
		}
		candidate := make([]byte, 0, len(tail)+len(fragment))
		candidate = append(candidate, tail...)
		candidate = append(candidate, fragment...)
		if bytes.Contains(candidate, queryBytes) {
			lineMatched = true
		}
		keep := min(max(0, len(queryBytes)-1), len(candidate))
		tail = append(tail[:0], candidate[len(candidate)-keep:]...)
		if hasNewline {
			finishLine()
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			if lineBytes > 0 || len(preview) > 0 {
				finishLine()
			}
			return fileBinary, nil
		default:
			return false, readErr
		}
	}
}

func list(ctx context.Context, rootFS fs.FS, start string, maxBytes int) ([]byte, error) {
	paths := make([]string, 0)
	collectedBytes := 0
	err := walkRegularFiles(ctx, rootFS, start, func(path string) error {
		path = filepath.ToSlash(path)
		paths = append(paths, path)
		collectedBytes += len(path) + 1
		if collectedBytes >= maxBytes {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out bytes.Buffer
	for _, path := range paths {
		out.WriteString(path)
		out.WriteByte('\n')
		if out.Len() >= maxBytes {
			break
		}
	}
	return out.Bytes(), nil
}

func walkRegularFiles(ctx context.Context, rootFS fs.FS, root string, visit func(string) error) error {
	rootInfo, err := fs.Stat(rootFS, ".")
	if err != nil {
		return err
	}
	return fs.WalkDir(rootFS, filepath.ToSlash(root), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != root && isVCSMetadataDir(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if path != root && (isLinkLike(info) || !sameFileSystem(rootInfo, info)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return visit(path)
	})
}

func isVCSMetadataDir(name string) bool {
	switch {
	case strings.EqualFold(name, ".git"), strings.EqualFold(name, ".hg"), strings.EqualFold(name, ".svn"):
		return true
	default:
		return false
	}
}

func boundOutput(data []byte, maxBytes int) string {
	if len(data) <= maxBytes {
		return string(data)
	}
	const marker = "[truncated]\n"
	if maxBytes <= len(marker) {
		return marker[:maxBytes]
	}
	return string(data[:maxBytes-len(marker)]) + marker
}
