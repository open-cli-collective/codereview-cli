package view

import (
	"fmt"
	"io"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
)

// AgentsList is the presentation model for `cr agents list`.
type AgentsList struct {
	Agents    []AgentSummary      `json:"agents"`
	Sources   []agents.SourceInfo `json:"sources,omitempty"`
	Repo      *agents.RepoInfo    `json:"repo,omitempty"`
	TrustNote string              `json:"trust_note,omitempty"`
}

// AgentSummary is one row in `cr agents list`.
type AgentSummary struct {
	ID          string            `json:"id"`
	Category    string            `json:"category"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	ModelTier   string            `json:"model_tier,omitempty"`
	ModelID     string            `json:"model_id,omitempty"`
	Effort      string            `json:"effort,omitempty"`
	Provenance  string            `json:"provenance"`
	Source      agents.SourceInfo `json:"source"`
}

// AgentsShow is the presentation model for `cr agents show`.
type AgentsShow struct {
	Agent     AgentDetail         `json:"agent"`
	Sources   []agents.SourceInfo `json:"sources,omitempty"`
	Repo      *agents.RepoInfo    `json:"repo,omitempty"`
	TrustNote string              `json:"trust_note,omitempty"`
}

// AgentDetail describes one loaded agent.
type AgentDetail struct {
	ID                   string            `json:"id"`
	Category             string            `json:"category"`
	CategoryDescription  string            `json:"category_description,omitempty"`
	CategoryOwner        string            `json:"category_owner,omitempty"`
	Name                 string            `json:"name"`
	Description          string            `json:"description,omitempty"`
	ModelTier            string            `json:"model_tier,omitempty"`
	ModelID              string            `json:"model_id,omitempty"`
	Effort               string            `json:"effort,omitempty"`
	FileGlobs            []string          `json:"file_globs,omitempty"`
	AppliesWhen          []string          `json:"applies_when,omitempty"`
	NeedsFullFileContent bool              `json:"needs_full_file_content"`
	Prompt               string            `json:"prompt"`
	Provenance           string            `json:"provenance"`
	Source               agents.SourceInfo `json:"source"`
}

// NewAgentsList builds the list presentation model.
func NewAgentsList(catalog agents.Catalog) AgentsList {
	summaries := make([]AgentSummary, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		summaries = append(summaries, AgentSummary{
			ID:          agent.ID,
			Category:    agent.Category.Name,
			Name:        agent.Name,
			Description: agent.Description,
			ModelTier:   agent.ModelTier,
			ModelID:     agent.ModelID,
			Effort:      agent.Effort,
			Provenance:  agent.Provenance.String(),
			Source:      agent.Provenance.SourceInfo(),
		})
	}
	return AgentsList{Agents: summaries, Sources: cloneSources(catalog.Sources), Repo: catalog.Repo, TrustNote: trustNote(catalog)}
}

// NewAgentsShow builds the detail presentation model.
func NewAgentsShow(agent agents.Agent, catalog agents.Catalog) AgentsShow {
	return AgentsShow{
		Agent: AgentDetail{
			ID:                   agent.ID,
			Category:             agent.Category.Name,
			CategoryDescription:  agent.Category.Description,
			CategoryOwner:        agent.Category.Owner,
			Name:                 agent.Name,
			Description:          agent.Description,
			ModelTier:            agent.ModelTier,
			ModelID:              agent.ModelID,
			Effort:               agent.Effort,
			FileGlobs:            append([]string(nil), agent.FileGlobs...),
			AppliesWhen:          append([]string(nil), agent.AppliesWhen...),
			NeedsFullFileContent: agent.NeedsFullFileContent,
			Prompt:               agent.Prompt,
			Provenance:           agent.Provenance.String(),
			Source:               agent.Provenance.SourceInfo(),
		},
		Sources:   cloneSources(catalog.Sources),
		Repo:      catalog.Repo,
		TrustNote: trustNote(catalog),
	}
}

// RenderAgentsListText writes a stable human-readable agent list.
func RenderAgentsListText(w io.Writer, result AgentsList) error {
	if len(result.Agents) == 0 {
		if _, err := fmt.Fprintln(w, "Agents: none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "Agents:"); err != nil {
			return err
		}
		for _, agent := range result.Agents {
			if _, err := fmt.Fprintf(w, "  - %s\n", agent.ID); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Description", agent.Description); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Model tier", agent.ModelTier); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Model ID", agent.ModelID); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Effort", agent.Effort); err != nil {
				return err
			}
			if err := writeKV(w, "    Provenance", agent.Provenance); err != nil {
				return err
			}
			if err := renderSourceDetails(w, "    ", agent.Source); err != nil {
				return err
			}
		}
	}
	return renderTrustNote(w, result.TrustNote)
}

// RenderAgentsListJSON writes the agent list as indented JSON.
func RenderAgentsListJSON(w io.Writer, result AgentsList) error {
	return RenderJSON(w, result)
}

// RenderAgentsShowText writes a stable human-readable agent detail.
func RenderAgentsShowText(w io.Writer, result AgentsShow) error {
	agent := result.Agent
	if err := writeKV(w, "Agent", agent.ID); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "Description", agent.Description); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Category:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Name", agent.Category); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Description", agent.CategoryDescription); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Owner", agent.CategoryOwner); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Runtime:"); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Model tier", agent.ModelTier); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Model ID", agent.ModelID); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Effort", agent.Effort); err != nil {
		return err
	}
	if err := writeKV(w, "  Needs full file content", fmt.Sprint(agent.NeedsFullFileContent)); err != nil {
		return err
	}
	if err := renderList(w, "File globs", agent.FileGlobs); err != nil {
		return err
	}
	if err := renderList(w, "Applies when", agent.AppliesWhen); err != nil {
		return err
	}
	if err := writeKV(w, "Provenance", agent.Provenance); err != nil {
		return err
	}
	if err := renderSourceDetails(w, "", agent.Source); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Prompt:"); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, agent.Prompt); err != nil {
		return err
	}
	if agent.Prompt == "" || agent.Prompt[len(agent.Prompt)-1] != '\n' {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return renderTrustNote(w, result.TrustNote)
}

// RenderAgentsShowJSON writes the agent detail as indented JSON.
func RenderAgentsShowJSON(w io.Writer, result AgentsShow) error {
	return RenderJSON(w, result)
}

func trustNote(catalog agents.Catalog) string {
	if catalog.Repo == nil {
		return ""
	}
	return catalog.Repo.TrustNote()
}

func renderTrustNote(w io.Writer, note string) error {
	if note == "" {
		return nil
	}
	return writeKV(w, "Note", note)
}

func renderSourceDetails(w io.Writer, prefix string, source agents.SourceInfo) error {
	if source.CanonicalPath != "" {
		if err := writeKV(w, prefix+"Source canonical path", source.CanonicalPath); err != nil {
			return err
		}
	}
	if source.Fingerprint != "" {
		if err := writeKV(w, prefix+"Source fingerprint", source.Fingerprint); err != nil {
			return err
		}
	}
	for _, warning := range source.Warnings {
		if _, err := fmt.Fprintf(w, "%sSource warning: %s\n", prefix, warning); err != nil {
			return err
		}
	}
	return nil
}

func cloneSources(sources []agents.SourceInfo) []agents.SourceInfo {
	if len(sources) == 0 {
		return nil
	}
	out := make([]agents.SourceInfo, len(sources))
	copy(out, sources)
	for i := range out {
		out[i].Warnings = append([]string(nil), sources[i].Warnings...)
	}
	return out
}

func renderList(w io.Writer, title string, values []string) error {
	if len(values) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, title+":"); err != nil {
		return err
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(w, "  - %s\n", value); err != nil {
			return err
		}
	}
	return nil
}
