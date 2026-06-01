package view

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
)

func TestRenderAgentsListTextIncludesProvenanceAndTrustNote(t *testing.T) {
	result := NewAgentsList(testAgentsCatalog())

	var out bytes.Buffer
	if err := RenderAgentsListText(&out, result); err != nil {
		t.Fatalf("RenderAgentsListText: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Agents:",
		"  - harness:architecture",
		"Description: Reviews architecture.",
		"Provenance: repo@main:abc1234",
		"Note: Repo-local agents loaded from base branch main at abc1234",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want %q", text, want)
		}
	}
}

func TestRenderAgentsListJSON(t *testing.T) {
	result := NewAgentsList(testAgentsCatalog())

	var out bytes.Buffer
	if err := RenderAgentsListJSON(&out, result); err != nil {
		t.Fatalf("RenderAgentsListJSON: %v", err)
	}
	var decoded AgentsList
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(decoded.Agents) != 1 || decoded.Agents[0].ID != "harness:architecture" {
		t.Fatalf("agents = %#v, want one harness agent", decoded.Agents)
	}
	if decoded.TrustNote == "" || decoded.Repo == nil || decoded.Repo.Provenance != "repo@main:abc1234" {
		t.Fatalf("repo/trust = (%#v,%q), want repo note", decoded.Repo, decoded.TrustNote)
	}
}

func TestRenderAgentsShowTextIncludesPrompt(t *testing.T) {
	catalog := testAgentsCatalog()
	result := NewAgentsShow(catalog.Agents[0], catalog)

	var out bytes.Buffer
	if err := RenderAgentsShowText(&out, result); err != nil {
		t.Fatalf("RenderAgentsShowText: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Agent: harness:architecture",
		"Category:",
		"File globs:",
		"Applies when:",
		"Prompt:",
		"Read the diff carefully.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want %q", text, want)
		}
	}
}

func TestRenderAgentsShowJSON(t *testing.T) {
	catalog := testAgentsCatalog()
	result := NewAgentsShow(catalog.Agents[0], catalog)

	var out bytes.Buffer
	if err := RenderAgentsShowJSON(&out, result); err != nil {
		t.Fatalf("RenderAgentsShowJSON: %v", err)
	}
	var decoded AgentsShow
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Agent.ID != "harness:architecture" || decoded.Agent.Prompt == "" {
		t.Fatalf("agent = %#v, want detail with prompt", decoded.Agent)
	}
	if decoded.Agent.Provenance != "repo@main:abc1234" {
		t.Fatalf("provenance = %q, want repo@main:abc1234", decoded.Agent.Provenance)
	}
}

func testAgentsCatalog() agents.Catalog {
	return agents.Catalog{
		Agents: []agents.Agent{{
			ID:          "harness:architecture",
			Name:        "architecture",
			Category:    agents.Category{Name: "harness", Description: "Engineering reviewers.", Owner: "rianjs"},
			Description: "Reviews architecture.",
			Model:       "sonnet",
			Effort:      "medium",
			FileGlobs:   []string{"**/*.go"},
			AppliesWhen: []string{"Go files changed"},
			Prompt:      "Read the diff carefully.\n",
			Provenance:  agents.Provenance{Kind: agents.SourceRepo, Ref: "main", SHA: "abc123456789"},
		}},
		Repo: &agents.RepoInfo{Ref: "main", SHA: "abc123456789", Provenance: "repo@main:abc1234"},
	}
}
