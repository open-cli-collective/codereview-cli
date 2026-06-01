package view

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
)

// AgentsList is the presentation model for `cr agents list`.
type AgentsList struct {
	Agents    []AgentSummary   `json:"agents"`
	Repo      *agents.RepoInfo `json:"repo,omitempty"`
	TrustNote string           `json:"trust_note,omitempty"`
}

// AgentSummary is one row in `cr agents list`.
type AgentSummary struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Provenance  string `json:"provenance"`
}

// AgentsShow is the presentation model for `cr agents show`.
type AgentsShow struct {
	Agent     AgentDetail      `json:"agent"`
	Repo      *agents.RepoInfo `json:"repo,omitempty"`
	TrustNote string           `json:"trust_note,omitempty"`
}

// AgentDetail describes one loaded agent.
type AgentDetail struct {
	ID                   string   `json:"id"`
	Category             string   `json:"category"`
	CategoryDescription  string   `json:"category_description,omitempty"`
	CategoryOwner        string   `json:"category_owner,omitempty"`
	Name                 string   `json:"name"`
	Description          string   `json:"description,omitempty"`
	Model                string   `json:"model,omitempty"`
	Effort               string   `json:"effort,omitempty"`
	FileGlobs            []string `json:"file_globs,omitempty"`
	AppliesWhen          []string `json:"applies_when,omitempty"`
	NeedsFullFileContent bool     `json:"needs_full_file_content"`
	Prompt               string   `json:"prompt"`
	Provenance           string   `json:"provenance"`
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
			Model:       agent.Model,
			Effort:      agent.Effort,
			Provenance:  agent.Provenance.String(),
		})
	}
	return AgentsList{Agents: summaries, Repo: catalog.Repo, TrustNote: trustNote(catalog)}
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
			Model:                agent.Model,
			Effort:               agent.Effort,
			FileGlobs:            append([]string(nil), agent.FileGlobs...),
			AppliesWhen:          append([]string(nil), agent.AppliesWhen...),
			NeedsFullFileContent: agent.NeedsFullFileContent,
			Prompt:               agent.Prompt,
			Provenance:           agent.Provenance.String(),
		},
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
			if err := writeOptionalKV(w, "    Model", agent.Model); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Effort", agent.Effort); err != nil {
				return err
			}
			if err := writeKV(w, "    Provenance", agent.Provenance); err != nil {
				return err
			}
		}
	}
	return renderTrustNote(w, result.TrustNote)
}

// RenderAgentsListJSON writes the agent list as indented JSON.
func RenderAgentsListJSON(w io.Writer, result AgentsList) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
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
	if err := writeOptionalKV(w, "  Model", agent.Model); err != nil {
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
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
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
