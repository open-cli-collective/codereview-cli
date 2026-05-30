// Package statepaths composes cr-specific data and cache paths.
package statepaths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/byteness/percent"
	"github.com/open-cli-collective/cli-common/statedir"
)

const (
	// Tool is the binary/tool name used with cli-common statedir resolvers.
	Tool = "cr"
	// AppDir is cr's app-specific subdirectory under data/cache roots.
	AppDir = "codereview"
)

const dirPerm = 0o700

// Layout composes paths under resolved cr data/cache roots.
type Layout struct {
	DataRoot  string
	CacheRoot string
}

// RunSpec identifies one review attempt's path tuple.
type RunSpec struct {
	Host            string
	Owner           string
	Repo            string
	PRNumber        int
	HeadSHA         string
	BaseSHA         string
	Profile         string
	PostingIdentity string
	Attempt         int
}

// RunPaths contains all per-run artifact paths.
type RunPaths struct {
	Dir            string
	DiffPatch      string
	SlicesDir      string
	FindingsJSON   string
	RollupMarkdown string
	AgentLogsDir   string
	LockFile       string
}

// NewLayout returns a path layout rooted at dataRoot and cacheRoot.
func NewLayout(dataRoot, cacheRoot string) Layout {
	return Layout{DataRoot: dataRoot, CacheRoot: cacheRoot}
}

// DefaultLayout resolves the current process data/cache layout without
// creating directories.
func DefaultLayout() (Layout, error) {
	dataRoot, err := DataRoot()
	if err != nil {
		return Layout{}, err
	}
	cacheRoot, err := CacheRoot()
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(dataRoot, cacheRoot), nil
}

// DefaultLayoutEnsured resolves and creates the current process data/cache app
// roots.
func DefaultLayoutEnsured() (Layout, error) {
	dataRoot, err := DataRootEnsured()
	if err != nil {
		return Layout{}, err
	}
	cacheRoot, err := CacheRootEnsured()
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(dataRoot, cacheRoot), nil
}

// LedgerDB returns the ledger database path.
func (l Layout) LedgerDB() string {
	return filepath.Join(l.DataRoot, "ledger.db")
}

// HTTPCacheDir returns the disposable git-provider HTTP cache directory.
func (l Layout) HTTPCacheDir() string {
	return filepath.Join(l.CacheRoot, "http")
}

// Run returns artifact and lock paths for spec.
func (l Layout) Run(spec RunSpec) (RunPaths, error) {
	prKey, err := PRKey(spec.Host, spec.Owner, spec.Repo, spec.PRNumber)
	if err != nil {
		return RunPaths{}, err
	}
	if err := validateSHA("head SHA", spec.HeadSHA); err != nil {
		return RunPaths{}, err
	}
	if err := validateSHA("base SHA", spec.BaseSHA); err != nil {
		return RunPaths{}, err
	}
	scope, err := ResumeScope(spec.Profile, spec.PostingIdentity)
	if err != nil {
		return RunPaths{}, err
	}
	attempt, err := FormatAttempt(spec.Attempt)
	if err != nil {
		return RunPaths{}, err
	}

	runDir := filepath.Join(l.DataRoot, "runs", prKey, spec.HeadSHA, spec.BaseSHA, scope, attempt)
	keyHash := KeyHash(prKey, spec.HeadSHA, spec.BaseSHA, spec.Profile, spec.PostingIdentity)
	lockFile := filepath.Join(l.DataRoot, "locks", prKey+"__"+spec.HeadSHA[:7]+"__"+keyHash+".lock")

	return RunPaths{
		Dir:            runDir,
		DiffPatch:      filepath.Join(runDir, "diff.patch"),
		SlicesDir:      filepath.Join(runDir, "slices"),
		FindingsJSON:   filepath.Join(runDir, "findings.json"),
		RollupMarkdown: filepath.Join(runDir, "rollup.md"),
		AgentLogsDir:   filepath.Join(runDir, "agent-logs"),
		LockFile:       lockFile,
	}, nil
}

// SlicePatch returns the artifact path for an agent/file diff slice.
func (p RunPaths) SlicePatch(agentID, filePath string) string {
	return filepath.Join(p.SlicesDir, Encode(agentID), Encode(filePath)+".patch")
}

// AgentLog returns the tailable JSONL log path for an agent.
func (p RunPaths) AgentLog(agentID string) string {
	return filepath.Join(p.AgentLogsDir, Encode(agentID)+".jsonl")
}

// DataRoot resolves cr's app data root without creating it.
func DataRoot() (string, error) {
	dir, err := (statedir.Data{Tool: Tool}).DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, AppDir), nil
}

// DataRootEnsured resolves and creates cr's app data root.
func DataRootEnsured() (string, error) {
	dir, err := (statedir.Data{Tool: Tool}).DataDirEnsured()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, AppDir)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return "", fmt.Errorf("statepaths: creating data root: %w", err)
	}
	return root, nil
}

// CacheRoot resolves cr's app cache root without creating it.
func CacheRoot() (string, error) {
	dir, err := (statedir.Cache{Tool: Tool}).CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, AppDir), nil
}

// CacheRootEnsured resolves and creates cr's app cache root.
func CacheRootEnsured() (string, error) {
	dir, err := (statedir.Cache{Tool: Tool}).CacheDirEnsured()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, AppDir)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return "", fmt.Errorf("statepaths: creating cache root: %w", err)
	}
	return root, nil
}

// Encode percent-encodes every rune outside [A-Za-z0-9._-].
func Encode(value string) string {
	return percent.Encode(value, disallowedRunes(value))
}

// Decode reverses Encode.
func Decode(value string) string {
	return percent.Decode(value)
}

// PRKey returns the encoded PR key path segment.
func PRKey(host, owner, repo string, number int) (string, error) {
	if err := requireNonEmpty("host", host); err != nil {
		return "", err
	}
	if err := requireNonEmpty("owner", owner); err != nil {
		return "", err
	}
	if err := requireNonEmpty("repo", repo); err != nil {
		return "", err
	}
	if number <= 0 {
		return "", fmt.Errorf("statepaths: PR number must be positive")
	}
	return strings.Join([]string{Encode(host), Encode(owner), Encode(repo), strconv.Itoa(number)}, "_"), nil
}

// ResumeScope returns the encoded profile/posting-identity path segment.
func ResumeScope(profile, postingIdentity string) (string, error) {
	if err := requireNonEmpty("profile", profile); err != nil {
		return "", err
	}
	if err := requireNonEmpty("posting identity", postingIdentity); err != nil {
		return "", err
	}
	return Encode(profile) + "__" + Encode(postingIdentity), nil
}

// KeyHash returns the 12-character hash over the full resume-key tuple.
func KeyHash(prKey, headSHA, baseSHA, profile, postingIdentity string) string {
	hash := sha256.Sum256(lengthPrefixedTuple(prKey, headSHA, baseSHA, profile, postingIdentity))
	return hex.EncodeToString(hash[:])[:12]
}

// FormatAttempt returns the zero-padded run attempt path segment.
func FormatAttempt(attempt int) (string, error) {
	if attempt <= 0 {
		return "", fmt.Errorf("statepaths: attempt must be positive")
	}
	return fmt.Sprintf("%03d", attempt), nil
}

func lengthPrefixedTuple(values ...string) []byte {
	var b strings.Builder
	for _, value := range values {
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func disallowedRunes(value string) string {
	seen := make(map[rune]struct{})
	var b strings.Builder
	for _, r := range value {
		if isUnreserved(r) {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		b.WriteRune(r)
	}
	return b.String()
}

func isUnreserved(r rune) bool {
	return r >= 'A' && r <= 'Z' ||
		r >= 'a' && r <= 'z' ||
		r >= '0' && r <= '9' ||
		r == '.' ||
		r == '_' ||
		r == '-'
}

func validateSHA(name, value string) error {
	if len(value) != 40 {
		return fmt.Errorf("statepaths: %s must be full 40-character lowercase hex", name)
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("statepaths: %s must be full 40-character lowercase hex", name)
		}
	}
	return nil
}

func requireNonEmpty(name, value string) error {
	if value == "" {
		return fmt.Errorf("statepaths: %s must not be empty", name)
	}
	return nil
}
