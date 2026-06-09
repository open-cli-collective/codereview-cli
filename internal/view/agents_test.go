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
		"Provenance: repo@refs/heads/main:abc1234",
		"Note: Repo-local agents loaded from base branch refs/heads/main at abc1234",
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
	if decoded.TrustNote == "" || decoded.Repo == nil || decoded.Repo.Provenance != "repo@refs/heads/main:abc1234" {
		t.Fatalf("repo/trust = (%#v,%q), want repo note", decoded.Repo, decoded.TrustNote)
	}
}

func TestRenderAgentsListIncludesFilesystemSourceDetails(t *testing.T) {
	result := NewAgentsList(filesystemAgentsCatalog())

	var out bytes.Buffer
	if err := RenderAgentsListText(&out, result); err != nil {
		t.Fatalf("RenderAgentsListText: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Provenance: profile:org-reviewers",
		"Source canonical path: /Library/Application Support/codereview/agents",
		"Source fingerprint: sha256:abc123def4567890abc123def4567890",
		"Source warning: canonical path is inside Git worktree",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want %q", text, want)
		}
	}

	out.Reset()
	if err := RenderAgentsListJSON(&out, result); err != nil {
		t.Fatalf("RenderAgentsListJSON: %v", err)
	}
	var decoded AgentsList
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(decoded.Sources) != 1 || decoded.Sources[0].Fingerprint != "sha256:abc123def4567890abc123def4567890" {
		t.Fatalf("sources = %#v, want fingerprinted source", decoded.Sources)
	}
	if len(decoded.Agents) != 1 || decoded.Agents[0].Source.CanonicalPath == "" {
		t.Fatalf("agents = %#v, want structured source", decoded.Agents)
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
		"Note: Repo-local agents loaded from base branch refs/heads/main at abc1234",
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
	if decoded.Agent.Provenance != "repo@refs/heads/main:abc1234" {
		t.Fatalf("provenance = %q, want repo@refs/heads/main:abc1234", decoded.Agent.Provenance)
	}
}

func testAgentsCatalog() agents.Catalog {
	return agents.Catalog{
		Agents: []agents.Agent{{
			ID:          "harness:architecture",
			Name:        "architecture",
			Category:    agents.Category{Name: "harness", Description: "Engineering reviewers.", Owner: "rianjs"},
			Description: "Reviews architecture.",
			ModelTier:   "medium",
			Effort:      "medium",
			FileGlobs:   []string{"**/*.go"},
			AppliesWhen: []string{"Go files changed"},
			Prompt:      "Read the diff carefully.\n",
			Provenance:  agents.Provenance{Kind: agents.SourceRepo, Ref: "refs/heads/main", SHA: "abc123456789"},
		}},
		Repo: &agents.RepoInfo{Ref: "refs/heads/main", SHA: "abc123456789", Provenance: "repo@refs/heads/main:abc1234"},
	}
}

func filesystemAgentsCatalog() agents.Catalog {
	source := agents.SourceInfo{
		Kind:            agents.SourceProfile,
		Label:           "org-reviewers",
		ProvenanceLabel: "profile:org-reviewers",
		ConfiguredPath:  "/Library/Application Support/codereview/agents",
		CanonicalPath:   "/Library/Application Support/codereview/agents",
		Present:         true,
		Status:          agents.SourceStatusAvailable,
		Fingerprint:     "sha256:abc123def4567890abc123def4567890",
		Warnings:        []string{"canonical path is inside Git worktree /Library/Application Support/codereview; PR authors may be able to mutate it"},
	}
	return agents.Catalog{
		Sources: []agents.SourceInfo{source},
		Agents: []agents.Agent{{
			ID:          "harness:architecture",
			Name:        "architecture",
			Category:    agents.Category{Name: "harness"},
			Description: "Reviews architecture.",
			Prompt:      "Read the diff carefully.\n",
			Provenance: agents.Provenance{
				Kind:           agents.SourceProfile,
				Label:          "org-reviewers",
				ConfiguredPath: source.ConfiguredPath,
				CanonicalPath:  source.CanonicalPath,
				Fingerprint:    source.Fingerprint,
				Warnings:       source.Warnings,
			},
		}},
	}
}
