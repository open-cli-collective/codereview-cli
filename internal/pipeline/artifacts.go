package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

// ReviewerRelocationAssessments stores relocation assessments for one reviewer.
type ReviewerRelocationAssessments struct {
	AgentID     string                           `json:"agent_id"`
	Assessments []llm.RelocationAssessmentRecord `json:"assessments"`
}

type coverageArtifact struct {
	SchemaVersion  int                                  `json:"schema_version"`
	BaseSHA        string                               `json:"base_sha"`
	HeadSHA        string                               `json:"head_sha"`
	ManifestDigest string                               `json:"manifest_digest"`
	Moves          []relocationMove                     `json:"moves"`
	Reviewers      []reviewplan.ReviewerCoverageSummary `json:"reviewers"`
	Assessments    []ReviewerRelocationAssessments      `json:"relocation_assessments"`
	Failures       []ReviewerFailure                    `json:"failures"`
}

func relocationAssessmentArtifacts(results []llm.Findings, evidenceByAgent ...map[string]*llm.ReviewerToolEvidence) []ReviewerRelocationAssessments {
	var out []ReviewerRelocationAssessments
	var evidence map[string]*llm.ReviewerToolEvidence
	if len(evidenceByAgent) > 0 {
		evidence = evidenceByAgent[0]
	}
	for _, result := range results {
		if len(result.RelocationAssessments) == 0 {
			continue
		}
		records := append([]llm.RelocationAssessmentRecord(nil), result.RelocationAssessments...)
		if reviewerToolEvidenceForcesIncomplete(evidence[result.AgentID]) {
			for i := range records {
				records[i].Valid = false
				records[i].ReviewedFiles = nil
				records[i].Diagnostic = "primary reviewer tool evidence was incomplete"
			}
		}
		out = append(out, ReviewerRelocationAssessments{AgentID: result.AgentID, Assessments: records})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

func writeCoverageArtifact(paths ArtifactPaths, relocations relocationReviewState, reviewers []reviewplan.ReviewerCoverageSummary, assessments []ReviewerRelocationAssessments, failures []ReviewerFailure) (string, error) {
	if strings.TrimSpace(paths.CoverageJSON) == "" {
		return "", fmt.Errorf("pipeline: coverage artifact path is required")
	}
	artifact := coverageArtifact{
		SchemaVersion: 1, BaseSHA: relocations.Manifest.BaseSHA, HeadSHA: relocations.Manifest.HeadSHA,
		ManifestDigest: relocations.Manifest.Digest, Moves: append([]relocationMove(nil), relocations.Manifest.Moves...),
		Reviewers:   append([]reviewplan.ReviewerCoverageSummary(nil), reviewers...),
		Assessments: append([]ReviewerRelocationAssessments(nil), assessments...),
		Failures:    append([]ReviewerFailure(nil), failures...),
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := fsatomic.WriteFileAtomic(paths.CoverageJSON, data, 0o600); err != nil {
		return "", fmt.Errorf("pipeline: write coverage artifact: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func writeReviewerInputArtifacts(paths ArtifactPaths, rawDiff string) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.DiffPatch, []byte(rawDiff), 0o600); err != nil {
		return fmt.Errorf("pipeline: write diff: %w", err)
	}
	return nil
}

func writeArtifacts(paths ArtifactPaths, patches []FilePatch, catalog agents.Catalog, selection llm.Selection, findings []review.Finding, rollup string, reviewerRuntime map[string]reviewerRuntimeResolution, modelCatalog *modelcatalog.Catalog) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create slices dir: %w", err)
	}
	sourceJSON, err := json.MarshalIndent(agentSourcesArtifactFromCatalog(catalog, reviewerRuntime, modelCatalog), "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(paths.AgentSourcesJSON, append(sourceJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write agent source provenance: %w", err)
	}
	for _, selected := range selection.SelectedAgents {
		for _, file := range selected.Files {
			patch, ok := findPatch(patches, file)
			if !ok {
				continue
			}
			path, err := paths.SlicePatch(selected.AgentID, file)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("pipeline: create slice dir: %w", err)
			}
			if err := fsatomic.WriteFileAtomic(path, []byte(patch.Patch), 0o600); err != nil {
				return fmt.Errorf("pipeline: write slice: %w", err)
			}
		}
	}
	findingsJSON, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(paths.FindingsJSON, append(findingsJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write findings: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.RollupMarkdown, []byte(rollup+"\n"), 0o600); err != nil {
		return fmt.Errorf("pipeline: write rollup: %w", err)
	}
	return nil
}

// These private read models mirror the workbench metadata JSON consumed by prompts.
type workbenchMetadataArtifact struct {
	SchemaVersion     int                        `json:"schema_version"`
	SourceRepoRoot    string                     `json:"source_repo_root"`
	CheckoutMode      string                     `json:"checkout_mode"`
	PR                workbenchPRIdentity        `json:"pr"`
	Base              workbenchBranchArtifact    `json:"base"`
	Head              workbenchBranchArtifact    `json:"head"`
	RepoPath          string                     `json:"repo_path"`
	ScratchPath       string                     `json:"scratch_path"`
	ChangedFiles      []string                   `json:"changed_files,omitempty"`
	FingerprintInputs workbenchFingerprintInputs `json:"fingerprint_inputs"`
}

type workbenchPRIdentity struct {
	Host   string `json:"host"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

type workbenchBranchArtifact struct {
	Host  string `json:"host,omitempty"`
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Name  string `json:"name,omitempty"`
	Ref   string `json:"ref,omitempty"`
	SHA   string `json:"sha"`
}

type workbenchFingerprintInputs struct {
	PR             workbenchPRIdentity `json:"pr"`
	BaseSHA        string              `json:"base_sha"`
	HeadSHA        string              `json:"head_sha"`
	CheckoutMode   string              `json:"checkout_mode"`
	ChangedFiles   []string            `json:"changed_files,omitempty"`
	SourceRepoRoot string              `json:"source_repo_root"`
}

func agentSourcesArtifactFromCatalog(catalog agents.Catalog, reviewerRuntime map[string]reviewerRuntimeResolution, modelCatalog *modelcatalog.Catalog) agentSourcesArtifact {
	artifact := agentSourcesArtifact{
		Sources: append([]agents.SourceInfo(nil), catalog.Sources...),
		Agents:  make([]agentProvenanceArtifact, 0, len(catalog.Agents)),
	}
	if modelCatalog != nil {
		artifact.CatalogRevision = modelCatalog.Revision()
		artifact.CatalogSource = modelCatalog.Source().Kind
	}
	for i := range artifact.Sources {
		artifact.Sources[i].Warnings = append([]string(nil), catalog.Sources[i].Warnings...)
	}
	for _, agent := range catalog.Agents {
		runtime, ok := reviewerRuntime[agent.ID]
		var runtimePtr *reviewerRuntimeResolution
		if ok {
			runtimeCopy := runtime
			runtimePtr = &runtimeCopy
		}
		artifact.Agents = append(artifact.Agents, agentProvenanceArtifact{
			ID:              agent.ID,
			Provenance:      agent.Provenance.String(),
			Source:          agent.Provenance.SourceInfo(),
			ReviewerRuntime: runtimePtr,
		})
	}
	return artifact
}

func reviewerRuntimeArtifact(req Request, catalog agents.Catalog, selection llm.Selection, fastDelivered string, fastRequested, fastIgnored bool) map[string]reviewerRuntimeResolution {
	if fastRequested && fastDelivered != "fast" && fastDelivered != "standard" {
		fastDelivered = "unknown"
	}
	if len(selection.SelectedAgents) == 0 {
		return nil
	}
	agentsByID := make(map[string]agents.Agent, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		agentsByID[agent.ID] = agent
	}
	out := make(map[string]reviewerRuntimeResolution, len(selection.SelectedAgents))
	for _, selected := range selection.SelectedAgents {
		agent, ok := agentsByID[selected.AgentID]
		if !ok {
			continue
		}
		resolution, err := resolveReviewerRuntime(req, agent)
		if err != nil {
			continue
		}
		resolution.Fast = fastRequested
		resolution.FastIgnored = fastIgnored
		if fastRequested {
			resolution.FastDelivered = fastDelivered
		}
		out[selected.AgentID] = resolution
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func reviewerFastDelivery(requested bool, sessions []sessionDraft) string {
	if !requested {
		return ""
	}
	delivered := ""
	for _, session := range sessions {
		switch session.Response.Usage.Speed {
		case "":
			// A draft that reports no speed does not contradict one that does.
		case "standard":
			return "standard"
		case "fast":
			if delivered == "" {
				delivered = "fast"
			}
		default:
			delivered = "unknown"
		}
	}
	if delivered == "" {
		return "unknown"
	}
	return delivered
}
