// Package runartifact owns per-run artifact paths and run-kind markers shared
// by review lifecycle commands.
package runartifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	markerSchema = 1

	// KindReview marks artifacts for a full review run.
	KindReview = "review"
	// KindThreadResponse marks artifacts for a response-only thread lifecycle run.
	KindThreadResponse = "thread_response"
)

var markerFileByKind = map[string]string{
	KindReview:         "review-run.json",
	KindThreadResponse: "thread-response-run.json",
}

// ErrMarkerInvalid reports that an artifact marker is missing or malformed.
var ErrMarkerInvalid = errors.New("runartifact: marker invalid")

// Paths contains per-run artifact paths.
// The artifact root owns these fixed child names, so methods on this type return
// lifecycle-owned paths rather than arbitrary user input.
type Paths struct {
	Dir              string `json:"dir"`
	DiffPatch        string `json:"diff_patch"`
	SlicesDir        string `json:"slices_dir"`
	FindingsJSON     string `json:"findings_json"`
	RollupMarkdown   string `json:"rollup_markdown"`
	AgentSourcesJSON string `json:"agent_sources_json"`
	AgentLogsDir     string `json:"agent_logs_dir"`
	LLMTasksDir      string `json:"llm_tasks_dir"`
	DossierDir       string `json:"dossier_dir"`
	WorkbenchDir     string `json:"workbench_dir"`
	WorkbenchRepoDir string `json:"workbench_repo_dir"`
	WorkbenchScratch string `json:"workbench_scratch_dir"`
}

// ForRun returns the artifact paths for a generated run ID.
func ForRun(layout statepaths.Layout, ref gitprovider.PRRef, pr gitprovider.PR, profile, postingIdentity, runID string) (Paths, error) {
	prKey, err := statepaths.PRKey(ref.Host, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return Paths{}, err
	}
	if _, err := statepaths.ResumeScope(profile, postingIdentity); err != nil {
		return Paths{}, err
	}
	scopeHash := statepaths.KeyHash(prKey, pr.Head.SHA, pr.Base.SHA, profile, postingIdentity)
	dir := filepath.Join(layout.DataRoot, "runs", prKey, prref.ShortSHA(pr.Head.SHA), prref.ShortSHA(pr.Base.SHA), scopeHash, "run-"+statepaths.Encode(runID))
	return FromDir(dir), nil
}

// FromDir returns the artifact path set rooted at dir.
func FromDir(dir string) Paths {
	return Paths{
		Dir:              dir,
		DiffPatch:        filepath.Join(dir, "diff.patch"),
		SlicesDir:        filepath.Join(dir, "slices"),
		FindingsJSON:     filepath.Join(dir, "findings.json"),
		RollupMarkdown:   filepath.Join(dir, "rollup.md"),
		AgentSourcesJSON: filepath.Join(dir, "agent-sources.json"),
		AgentLogsDir:     filepath.Join(dir, "agent-logs"),
		LLMTasksDir:      filepath.Join(dir, "llm-tasks"),
		DossierDir:       filepath.Join(dir, "dossier"),
		WorkbenchDir:     filepath.Join(dir, "workbench"),
		WorkbenchRepoDir: filepath.Join(dir, "workbench", "repo"),
		WorkbenchScratch: filepath.Join(dir, "workbench", "scratch"),
	}
}

// SlicePatch returns the artifact path for an agent/file diff slice.
func (p Paths) SlicePatch(agentID, filePath string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("runartifact: agent ID is required")
	}
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("runartifact: file path is required")
	}
	return filepath.Join(p.SlicesDir, statepaths.Encode(agentID), statepaths.Encode(filePath)+".patch"), nil
}

// AgentLog returns the tailable LLM log path for an agent.
func (p Paths) AgentLog(agentID string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("runartifact: agent ID is required")
	}
	return filepath.Join(p.AgentLogsDir, statepaths.Encode(agentID)+".jsonl"), nil
}

// LLMTaskDir returns the artifact directory for one durable LLM task.
func (p Paths) LLMTaskDir(taskID string) (string, error) {
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("runartifact: LLM task ID is required")
	}
	return filepath.Join(p.LLMTasksDir, statepaths.Encode(taskID)), nil
}

// LLMTaskMetadata returns the metadata artifact path for one durable LLM task.
func (p Paths) LLMTaskMetadata(taskID string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "metadata.json"), nil
}

// LLMTaskValidatedOutput returns the validated structured output path for one task.
func (p Paths) LLMTaskValidatedOutput(taskID string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "validated-output.json"), nil
}

// LLMTaskRawAttempt returns the raw structured output path for a failed attempt.
func (p Paths) LLMTaskRawAttempt(taskID, attempt string) (string, error) {
	dir, err := p.LLMTaskDir(taskID)
	if err != nil {
		return "", err
	}
	attempt = strings.TrimSpace(attempt)
	if attempt == "" {
		return "", fmt.Errorf("runartifact: LLM task attempt is required")
	}
	return filepath.Join(dir, statepaths.Encode(attempt)+".json"), nil
}

// DossierRawPath returns a raw dossier artifact path by file name.
func (p Paths) DossierRawPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "raw"), name)
}

// DossierSummaryPath returns a summary dossier artifact path by file name.
func (p Paths) DossierSummaryPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "summary"), name)
}

// DossierFinalPath returns a reviewer-facing dossier artifact path by file name.
func (p Paths) DossierFinalPath(name string) (string, error) {
	return dossierChildPath(filepath.Join(p.DossierDir, "final"), name)
}

// DossierIndexPath returns the dossier index artifact path.
func (p Paths) DossierIndexPath() string {
	return filepath.Join(p.DossierDir, "index.json")
}

// WorkbenchMetadataPath returns the workbench metadata artifact path.
func (p Paths) WorkbenchMetadataPath() string {
	return filepath.Join(p.WorkbenchDir, "metadata.json")
}

func dossierChildPath(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("runartifact: dossier artifact name is required")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("runartifact: dossier artifact name must be a file name")
	}
	if strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator)) {
		return "", fmt.Errorf("runartifact: dossier artifact name must be a file name")
	}
	return filepath.Join(dir, name), nil
}

// Marker identifies the lifecycle kind attached to one run artifact root.
type Marker struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	RunID         string `json:"run_id"`
}

// WriteMarker persists the run-kind discriminator for an artifact root.
func WriteMarker(artifactPath, kind, runID string) error {
	if _, err := markerFile(kind); err != nil {
		return err
	}
	data, err := json.MarshalIndent(Marker{
		SchemaVersion: markerSchema,
		Kind:          kind,
		RunID:         runID,
	}, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFileAtomic(MarkerPath(artifactPath, kind), append(data, '\n'), 0o600)
}

// MarkerMatches reports whether an artifact root carries a valid marker for
// the expected run kind and run ID.
func MarkerMatches(artifactPath, kind, runID string) bool {
	found, err := ReadMarker(artifactPath, kind)
	return err == nil && found.Kind == kind && found.RunID == runID
}

// ReadMarker reads and validates the marker for the requested run kind.
func ReadMarker(artifactPath, kind string) (Marker, error) {
	path := MarkerPath(artifactPath, kind)
	// #nosec G304 -- marker paths are derived from artifact roots owned by cr.
	data, err := os.ReadFile(path)
	if err != nil {
		return Marker{}, err
	}
	var found Marker
	if err := json.Unmarshal(data, &found); err != nil {
		return Marker{}, fmt.Errorf("%w: %s: %w", ErrMarkerInvalid, path, err)
	}
	if found.SchemaVersion != markerSchema {
		return Marker{}, fmt.Errorf("%w: %s schema_version %d", ErrMarkerInvalid, path, found.SchemaVersion)
	}
	if found.Kind != kind {
		return Marker{}, fmt.Errorf("%w: %s kind %q", ErrMarkerInvalid, path, found.Kind)
	}
	if strings.TrimSpace(found.RunID) == "" {
		return Marker{}, fmt.Errorf("%w: %s run_id is required", ErrMarkerInvalid, path)
	}
	return found, nil
}

// MarkerPath returns the marker path for an artifact root and run kind.
func MarkerPath(artifactPath, kind string) string {
	name, err := markerFile(kind)
	if err != nil {
		return filepath.Join(artifactPath, "unknown-run-kind.json")
	}
	return filepath.Join(artifactPath, name)
}

func markerFile(kind string) (string, error) {
	name, ok := markerFileByKind[kind]
	if !ok {
		return "", fmt.Errorf("runartifact: unsupported run kind %q", kind)
	}
	return name, nil
}
