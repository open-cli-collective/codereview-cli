package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func writeArtifacts(paths ArtifactPaths, rawDiff string, patches []FilePatch, catalog agents.Catalog, selection llm.Selection, findings []review.Finding, rollup string, reviewerRuntime map[string]reviewerRuntimeResolution) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create slices dir: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.DiffPatch, []byte(rawDiff), 0o600); err != nil {
		return fmt.Errorf("pipeline: write diff: %w", err)
	}
	sourceJSON, err := json.MarshalIndent(agentSourcesArtifactFromCatalog(catalog, reviewerRuntime), "", "  ")
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

func agentSourcesArtifactFromCatalog(catalog agents.Catalog, reviewerRuntime map[string]reviewerRuntimeResolution) agentSourcesArtifact {
	artifact := agentSourcesArtifact{
		Sources: append([]agents.SourceInfo(nil), catalog.Sources...),
		Agents:  make([]agentProvenanceArtifact, 0, len(catalog.Agents)),
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

func reviewerRuntimeArtifact(req Request, catalog agents.Catalog, selection llm.Selection) map[string]reviewerRuntimeResolution {
	if strings.TrimSpace(req.ReviewerModelOverride) != "" {
		if !req.ReviewerFast {
			return nil
		}
		out := make(map[string]reviewerRuntimeResolution, len(selection.SelectedAgents))
		for _, selected := range selection.SelectedAgents {
			out[selected.AgentID] = reviewerRuntimeResolution{Mode: "override", ResolvedModel: strings.TrimSpace(req.ReviewerModelOverride), Fast: true}
		}
		return out
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
		resolution, err := resolveAgentModel(req.Profile, req.ReviewerModelTierOverride, agent)
		if err != nil {
			continue
		}
		resolution.Fast = req.ReviewerFast
		out[selected.AgentID] = resolution
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
