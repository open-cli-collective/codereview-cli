// Package statepaths composes cr-specific data and cache paths.
package statepaths

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// AppDir is the config/credential service directory name. It is retained
	// for config-scope callers; data/cache roots use Tool directly.
	AppDir = "codereview"
)

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

// LockSpec identifies one review resume key's lock file.
type LockSpec struct {
	Host            string
	Owner           string
	Repo            string
	PRNumber        int
	HeadSHA         string
	BaseSHA         string
	Profile         string
	PostingIdentity string
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
	lockFile, err := l.LockFile(LockSpec{
		Host:            spec.Host,
		Owner:           spec.Owner,
		Repo:            spec.Repo,
		PRNumber:        spec.PRNumber,
		HeadSHA:         spec.HeadSHA,
		BaseSHA:         spec.BaseSHA,
		Profile:         spec.Profile,
		PostingIdentity: spec.PostingIdentity,
	})
	if err != nil {
		return RunPaths{}, err
	}

	runDir := filepath.Join(l.DataRoot, "runs", prKey, spec.HeadSHA, spec.BaseSHA, scope, attempt)

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

// LockFile returns the advisory run-lock path for spec's resume key.
func (l Layout) LockFile(spec LockSpec) (string, error) {
	prKey, err := PRKey(spec.Host, spec.Owner, spec.Repo, spec.PRNumber)
	if err != nil {
		return "", err
	}
	if err := validateSHA("head SHA", spec.HeadSHA); err != nil {
		return "", err
	}
	if err := validateSHA("base SHA", spec.BaseSHA); err != nil {
		return "", err
	}
	if _, err := ResumeScope(spec.Profile, spec.PostingIdentity); err != nil {
		return "", err
	}
	keyHash := KeyHash(prKey, spec.HeadSHA, spec.BaseSHA, spec.Profile, spec.PostingIdentity)
	return filepath.Join(l.DataRoot, "locks", prKey+"__"+spec.HeadSHA[:7]+"__"+keyHash+".lock"), nil
}

// SlicePatch returns the artifact path for an agent/file diff slice.
func (p RunPaths) SlicePatch(agentID, filePath string) (string, error) {
	if err := requireNonEmpty("agent ID", agentID); err != nil {
		return "", err
	}
	if err := requireNonEmpty("file path", filePath); err != nil {
		return "", err
	}
	return filepath.Join(p.SlicesDir, Encode(agentID), Encode(filePath)+".patch"), nil
}

// AgentLog returns the tailable JSONL log path for an agent.
func (p RunPaths) AgentLog(agentID string) (string, error) {
	if err := requireNonEmpty("agent ID", agentID); err != nil {
		return "", err
	}
	return filepath.Join(p.AgentLogsDir, Encode(agentID)+".jsonl"), nil
}

// DataRoot resolves cr's per-binary data root without creating it.
func DataRoot() (string, error) {
	return (statedir.Data{Tool: Tool}).DataDir()
}

// DataRootEnsured resolves and creates cr's per-binary data root.
func DataRootEnsured() (string, error) {
	return (statedir.Data{Tool: Tool}).DataDirEnsured()
}

// CacheRoot resolves cr's per-binary cache root without creating it.
func CacheRoot() (string, error) {
	return (statedir.Cache{Tool: Tool}).CacheDir()
}

// CacheRootEnsured resolves and creates cr's per-binary cache root.
func CacheRootEnsured() (string, error) {
	return (statedir.Cache{Tool: Tool}).CacheDirEnsured()
}

// LegacyDataRoot returns the historical nested data root for layout.
func LegacyDataRoot(layout Layout) string {
	return filepath.Join(layout.DataRoot, AppDir)
}

// LegacyCacheRoot returns the historical nested cache root for layout.
func LegacyCacheRoot(layout Layout) string {
	return filepath.Join(layout.CacheRoot, AppDir)
}

// LegacyDataRootExists reports whether layout still has unmigrated legacy data.
func LegacyDataRootExists(layout Layout) (bool, error) {
	return legacyRootOrTempExists(LegacyDataRoot(layout), "data")
}

// MigrateLegacyDataRoot moves data written by releases that nested state under
// the per-binary root's historical AppDir child.
func MigrateLegacyDataRoot(layout Layout) error {
	return migrateLegacyRoot(layout.DataRoot, LegacyDataRoot(layout), "data")
}

// MigrateLegacyCacheRoot moves cache written by releases that nested state
// under the per-binary root's historical AppDir child.
func MigrateLegacyCacheRoot(layout Layout) error {
	return migrateLegacyRoot(layout.CacheRoot, LegacyCacheRoot(layout), "cache")
}

func legacyRootExists(path, kind string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("statepaths: inspecting legacy %s root: %w", kind, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("statepaths: legacy %s root %s is not a directory", kind, path)
	}
	return true, nil
}

func legacyRootOrTempExists(path, kind string) (bool, error) {
	legacyExists, err := legacyRootExists(path, kind)
	if err != nil {
		return false, err
	}
	tempExists, err := legacyRootExists(path+".migrating", kind)
	if err != nil {
		return false, err
	}
	return legacyExists || tempExists, nil
}

func migrateLegacyRoot(root, legacyRoot, kind string) error {
	tempRoot := legacyRoot + ".migrating"
	var sourceRoot string
	if tempExists, err := legacyRootExists(tempRoot, kind); err != nil {
		return err
	} else if tempExists {
		legacyExists, err := legacyRootExists(legacyRoot, kind)
		if err != nil {
			return err
		}
		if legacyExists {
			return fmt.Errorf("statepaths: cannot migrate legacy %s root %s: migration temp %s already exists", kind, legacyRoot, tempRoot)
		}
		sourceRoot = tempRoot
	} else {
		legacyExists, err := legacyRootExists(legacyRoot, kind)
		if err != nil {
			return err
		}
		if !legacyExists {
			return nil
		}
		if err := os.Rename(legacyRoot, tempRoot); err != nil {
			return fmt.Errorf("statepaths: staging legacy %s root %s: %w", kind, legacyRoot, err)
		}
		sourceRoot = tempRoot
	}

	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		return fmt.Errorf("statepaths: reading legacy %s root: %w", kind, err)
	}
	if len(entries) == 0 {
		if err := os.Remove(sourceRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("statepaths: removing empty legacy %s root: %w", kind, err)
		}
		return nil
	}
	for _, entry := range entries {
		target := filepath.Join(root, entry.Name())
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("statepaths: cannot migrate legacy %s root %s: target %s already exists", kind, legacyRoot, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("statepaths: inspecting %s migration target: %w", kind, err)
		}
	}
	for _, entry := range entries {
		source := filepath.Join(sourceRoot, entry.Name())
		target := filepath.Join(root, entry.Name())
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("statepaths: migrating legacy %s entry %s: %w", kind, source, err)
		}
	}
	if err := os.Remove(sourceRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("statepaths: removing legacy %s root: %w", kind, err)
	}
	return nil
}

// Encode percent-encodes every rune outside [A-Za-z0-9._-].
func Encode(value string) string {
	return percent.Encode(value, disallowedRunes(value))
}

func encodePRKeySegment(value string) string {
	return encodeDelimiterSegment(value)
}

func encodeDelimiterSegment(value string) string {
	return percent.Encode(value, disallowedRunes(value)+"_")
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
	return strings.Join([]string{encodePRKeySegment(host), encodePRKeySegment(owner), encodePRKeySegment(repo), strconv.Itoa(number)}, "_"), nil
}

// ResumeScope returns the encoded profile/posting-identity path segment.
func ResumeScope(profile, postingIdentity string) (string, error) {
	if err := requireNonEmpty("profile", profile); err != nil {
		return "", err
	}
	if err := requireNonEmpty("posting identity", postingIdentity); err != nil {
		return "", err
	}
	return encodeDelimiterSegment(profile) + "__" + encodeDelimiterSegment(postingIdentity), nil
}

// KeyHash returns the 12-character hash over the full resume-key tuple.
func KeyHash(prKey, headSHA, baseSHA, profile, postingIdentity string) string {
	hash := sha256.Sum256(lengthPrefixedTuple(prKey, headSHA, baseSHA, profile, postingIdentity))
	return hex.EncodeToString(hash[:])[:12]
}

// FormatAttempt returns the run attempt path segment. Attempts below 1000 are
// zero-padded for readability; lexicographic sorting is not guaranteed after
// that because the design intentionally does not cap attempt numbers at 999.
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
