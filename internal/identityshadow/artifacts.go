// Package identityshadow runs private report identity diagnostics after the
// authoritative review path has completed.
package identityshadow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	sourceFileName     = "source.json"
	baselineFileName   = "baseline.json"
	comparisonFileName = "comparison.json"
	receiptFileName    = "receipt.json"
	manifestFileName   = "manifest.json"
)

type sourceDigestPayload struct {
	Source     reportidentitysnapshot.Source      `json:"source"`
	Findings   []review.Finding                   `json:"findings"`
	PriorRoots []reportidentitysnapshot.PriorRoot `json:"prior_roots"`
	PriorIDMap map[string]string                  `json:"prior_id_map"`
}

type sourceArtifact struct {
	SchemaVersion  int                                `json:"schema_version"`
	Source         reportidentitysnapshot.Source      `json:"source"`
	Findings       []review.Finding                   `json:"findings"`
	PriorRoots     []reportidentitysnapshot.PriorRoot `json:"prior_roots"`
	PriorIDMap     map[string]string                  `json:"prior_id_map"`
	SnapshotSHA256 string                             `json:"snapshot_sha256"`
}

type baselineArtifact struct {
	SchemaVersion  int                             `json:"schema_version"`
	SnapshotSHA256 string                          `json:"snapshot_sha256"`
	CRVersion      string                          `json:"cr_version"`
	Baseline       reportidentitysnapshot.Baseline `json:"baseline"`
}

type aggregateManifest struct {
	SchemaVersion    int                       `json:"schema_version"`
	Status           string                    `json:"status"`
	ComparisonStatus string                    `json:"comparison_status"`
	Files            map[string]string         `json:"files"`
	TaskManifests    map[string]nestedManifest `json:"task_manifests"`
}

type nestedManifest struct {
	SHA256   string                      `json:"sha256"`
	Manifest identityexperiment.Manifest `json:"manifest"`
}

func encodeCanonical(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, errors.New("identity shadow: encode artifact")
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func digest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func makeSourceArtifact(snapshot reportidentitysnapshot.Snapshot, priorIDMap map[string]string) (sourceArtifact, []byte, error) {
	findings := cloneFindings(snapshot.Findings)
	roots := cloneRoots(snapshot.PriorRoots)
	ids := cloneStringMap(priorIDMap)
	payload := sourceDigestPayload{Source: snapshot.Source, Findings: findings, PriorRoots: roots, PriorIDMap: ids}
	encodedPayload, err := encodeCanonical(payload)
	if err != nil {
		return sourceArtifact{}, nil, err
	}
	artifact := sourceArtifact{
		SchemaVersion: 1, Source: snapshot.Source, Findings: findings, PriorRoots: roots,
		PriorIDMap: ids, SnapshotSHA256: digest(encodedPayload),
	}
	encoded, err := encodeCanonical(artifact)
	if err != nil {
		return sourceArtifact{}, nil, err
	}
	return artifact, encoded, nil
}

func makeBaselineArtifact(snapshot reportidentitysnapshot.Snapshot, snapshotSHA256 string) (baselineArtifact, []byte, error) {
	artifact := baselineArtifact{
		SchemaVersion: 1, SnapshotSHA256: snapshotSHA256, CRVersion: snapshot.Source.CRVersion,
		Baseline: reportidentitysnapshot.Clone(snapshot).Baseline,
	}
	encoded, err := encodeCanonical(artifact)
	if err != nil {
		return baselineArtifact{}, nil, err
	}
	return artifact, encoded, nil
}

func secureAttemptDirectory(attemptDir string, dependencies identityexperiment.FileDependencies) error {
	if !validAttemptPath(attemptDir) {
		return errors.New("identity shadow: invalid attempt directory")
	}
	lstat := lstatFunc(dependencies)
	mkdir := mkdirFunc(dependencies)
	chmod := chmodFunc(dependencies)
	parent := filepath.Dir(attemptDir)
	if err := secureExistingOrCreateDirectory(parent, lstat, mkdir, chmod); err != nil {
		return err
	}
	if _, err := lstat(attemptDir); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("identity shadow: inspect attempt directory: %w", err)
	}
	if err := mkdir(attemptDir, 0o700); err != nil {
		return fmt.Errorf("identity shadow: create attempt directory: %w", err)
	}
	if err := chmod(attemptDir, 0o700); err != nil {
		_ = os.Remove(attemptDir)
		return fmt.Errorf("identity shadow: secure attempt directory: %w", err)
	}
	return nil
}

func secureExistingOrCreateDirectory(path string, lstat func(string) (os.FileInfo, error), mkdir func(string, os.FileMode) error, chmod func(string, os.FileMode) error) error {
	info, err := lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := mkdir(path, 0o700); err != nil {
			return fmt.Errorf("identity shadow: create private parent: %w", err)
		}
		if err := chmod(path, 0o700); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("identity shadow: secure private parent: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("identity shadow: inspect private parent: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("identity shadow: private parent is not a directory")
	}
	if err := chmod(path, 0o700); err != nil {
		return fmt.Errorf("identity shadow: secure private parent: %w", err)
	}
	return nil
}

func validAttemptPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(filepath.Dir(path)) != "identity-shadow" {
		return false
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "attempt-") || len(name) != len("attempt-")+36 {
		return false
	}
	value := strings.TrimPrefix(name, "attempt-")
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func lstatFunc(dependencies identityexperiment.FileDependencies) func(string) (os.FileInfo, error) {
	if dependencies.Lstat != nil {
		return dependencies.Lstat
	}
	return os.Lstat
}

func mkdirFunc(dependencies identityexperiment.FileDependencies) func(string, os.FileMode) error {
	if dependencies.Mkdir != nil {
		return dependencies.Mkdir
	}
	return os.Mkdir
}

func chmodFunc(dependencies identityexperiment.FileDependencies) func(string, os.FileMode) error {
	if dependencies.Chmod != nil {
		return dependencies.Chmod
	}
	return os.Chmod
}

func atomicWritePrivate(directory, name string, data []byte, dependencies identityexperiment.FileDependencies) error {
	if filepath.Base(name) != name || name == "" {
		return errors.New("identity shadow: invalid artifact name")
	}
	destination := filepath.Join(directory, name)
	if _, err := lstatFunc(dependencies)(destination); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("identity shadow: inspect artifact: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".identity-shadow-*.tmp")
	if err != nil {
		return fmt.Errorf("identity shadow: create temporary artifact: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("identity shadow: secure temporary artifact: %w", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("identity shadow: write temporary artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("identity shadow: sync temporary artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("identity shadow: close temporary artifact: %w", err)
	}
	rename := dependencies.Rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(temporaryName, destination); err != nil {
		return fmt.Errorf("identity shadow: publish artifact: %w", err)
	}
	return nil
}

func cloneStringMap(values map[string]string) map[string]string {
	copyOfValues := make(map[string]string, len(values))
	for key, value := range values {
		copyOfValues[key] = value
	}
	return copyOfValues
}

func cloneRoots(values []reportidentitysnapshot.PriorRoot) []reportidentitysnapshot.PriorRoot {
	copyOfValues := make([]reportidentitysnapshot.PriorRoot, len(values))
	copy(copyOfValues, values)
	return copyOfValues
}

func cloneFindings(findings []review.Finding) []review.Finding {
	result := make([]review.Finding, len(findings))
	copy(result, findings)
	return result
}
