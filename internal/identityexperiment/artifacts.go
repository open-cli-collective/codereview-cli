package identityexperiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
)

var errOutputInspection = errors.New("identityexperiment: output directory inspection failed")

// ManifestFiles contains the digests of the three retained payload files.
type ManifestFiles struct {
	FixtureSHA256  string `json:"fixture.json"`
	ReplaySHA256   string `json:"replay.json"`
	ReceiptsSHA256 string `json:"receipts.json"`
}

// Manifest is written last and covers the unchanged three-file payload.
type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	Files         ManifestFiles `json:"files"`
}

// Summary is the unchanged benchmark command's stdout result shape.
type Summary struct {
	SchemaVersion     int           `json:"schema_version"`
	Status            string        `json:"status"`
	ExpectedPairs     int           `json:"expected_pairs"`
	AttemptedPairs    int           `json:"attempted_pairs"`
	ProviderCalls     int           `json:"provider_calls"`
	CompletedPairs    int           `json:"completed_pairs"`
	AbstainedPairs    int           `json:"abstained_pairs"`
	FailedPairs       int           `json:"failed_pairs"`
	MissingPairs      int           `json:"missing_pairs"`
	OutputDir         string        `json:"output_dir"`
	FixturePath       string        `json:"fixture_path"`
	ReplayPath        string        `json:"replay_path"`
	ReceiptsPath      string        `json:"receipts_path"`
	ManifestPath      string        `json:"manifest_path"`
	KnownInputTokens  int64         `json:"known_input_tokens"`
	KnownOutputTokens int64         `json:"known_output_tokens"`
	EstimatedCostUSD  *float64      `json:"estimated_cost_usd"`
	UnknownCostCalls  int           `json:"unknown_cost_calls"`
	Configuration     Configuration `json:"configuration"`
}

// FileDependencies contains direct filesystem seams for reservation and
// atomic artifact publication.
type FileDependencies struct {
	Lstat  func(string) (os.FileInfo, error)
	Mkdir  func(string, os.FileMode) error
	Chmod  func(string, os.FileMode) error
	Rename func(string, string) error
}

// Publisher owns one already-reserved output directory. Its path and lifecycle
// cannot be supplied or reset by a caller.
type Publisher struct {
	mu     sync.Mutex
	dir    string
	rename func(string, string) error
	used   bool
}

// SHA256 returns the lowercase hexadecimal SHA-256 digest used by the report
// identity artifacts and their request provenance.
func SHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func encodeFixture(input reportidentity.Input) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(input); err != nil {
		return nil, err
	}
	return append([]byte(nil), bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})...), nil
}

// ReserveOutput refuses every existing destination and reserves a fresh
// owner-only directory before callers perform credential or provider work.
func ReserveOutput(outputDir string, dependencies FileDependencies) (*Publisher, error) {
	lstat := dependencies.Lstat
	if lstat == nil {
		lstat = os.Lstat
	}
	mkdir := dependencies.Mkdir
	if mkdir == nil {
		mkdir = os.Mkdir
	}
	chmod := dependencies.Chmod
	if chmod == nil {
		chmod = os.Chmod
	}
	if dependencies.Rename == nil {
		dependencies.Rename = os.Rename
	}

	_, err := lstat(outputDir)
	if err == nil {
		return nil, os.ErrExist
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errOutputInspection, err)
	}
	if err := mkdir(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("identityexperiment: create output directory: %w", err)
	}
	// #nosec G302 -- the artifact contract requires an owner-only directory.
	if err := chmod(outputDir, 0o700); err != nil {
		_ = os.Remove(outputDir)
		return nil, fmt.Errorf("identityexperiment: secure output directory: %w", err)
	}
	return &Publisher{dir: outputDir, rename: dependencies.Rename}, nil
}

// IsOutputInspectionError reports whether reservation failed while inspecting
// the destination, before any directory was created.
func IsOutputInspectionError(err error) bool {
	return errors.Is(err, errOutputInspection)
}

// Publish writes the retained payload atomically and writes the manifest last.
// A publisher cannot be reused after the first publication attempt.
func (publisher *Publisher) Publish(result Result) (Manifest, error) {
	if publisher == nil || publisher.dir == "" {
		return Manifest{}, errors.New("identityexperiment: output publisher is unavailable")
	}
	if result.err != nil || len(result.fixtureJSON) == 0 || len(result.replayJSON) == 0 || len(result.receiptsJSON) == 0 {
		return Manifest{}, errors.New("identityexperiment: result is unavailable for publication")
	}

	publisher.mu.Lock()
	if publisher.used {
		publisher.mu.Unlock()
		return Manifest{}, errors.New("identityexperiment: output publisher was already used")
	}
	publisher.used = true
	directory := publisher.dir
	rename := publisher.rename
	publisher.mu.Unlock()

	if rename == nil {
		rename = os.Rename
	}
	files := []struct {
		name string
		data []byte
	}{
		{name: "fixture.json", data: result.fixtureJSON},
		{name: "replay.json", data: result.replayJSON},
		{name: "receipts.json", data: result.receiptsJSON},
	}
	for _, file := range files {
		if err := writeAtomic(directory, file.name, file.data, rename); err != nil {
			return Manifest{}, err
		}
	}
	manifest := Manifest{SchemaVersion: SchemaVersion, Files: ManifestFiles{
		FixtureSHA256:  SHA256(result.fixtureJSON),
		ReplaySHA256:   SHA256(result.replayJSON),
		ReceiptsSHA256: SHA256(result.receiptsJSON),
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, errors.New("identityexperiment: encode manifest")
	}
	if err := writeAtomic(directory, "manifest.json", data, rename); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func writeAtomic(outputDir, name string, data []byte, rename func(string, string) error) error {
	temp, err := os.CreateTemp(outputDir, ".infer-identity-*.tmp")
	if err != nil {
		return fmt.Errorf("identityexperiment: create temporary artifact: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("identityexperiment: secure temporary artifact: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("identityexperiment: write temporary artifact: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("identityexperiment: close temporary artifact: %w", err)
	}
	if err := rename(tempName, filepath.Join(outputDir, name)); err != nil {
		return fmt.Errorf("identityexperiment: publish artifact: %w", err)
	}
	return nil
}
