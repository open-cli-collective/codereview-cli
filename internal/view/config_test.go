package view

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestRenderConfigTextStoredCredentialRefs(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), []CredentialStatus{
		credentialStatus("git", "codereview/work", "pat", "git_token", true),
		credentialStatus("reviewer_credentials", "codereview/work-reviewer", "pat", "git_token", false),
		credentialStatus("llm", "codereview/work-llm", "api_key", "anthropic_api_key", true),
	})

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Profile: work",
		"Credential ref: codereview/work",
		"Credential ref: codereview/work-reviewer",
		"Credential ref: codereview/work-llm",
		"git_token: present",
		"anthropic_api_key: present",
		"Allow self approve: true",
		"Resolve threads: never",
		"Max age days: 90",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "secret") {
		t.Fatalf("text output contains secret wording:\n%s", got)
	}
}

func TestRenderConfigTextSubscriptionCredentialIsAdapterManaged(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("home", homeProfile(), dataConfig(), []CredentialStatus{
		credentialStatus("git", "codereview/home", "pat", "git_token", false),
	})

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Reviewer credentials: self-review uses git credentials") {
		t.Fatalf("text output missing self-review note:\n%s", got)
	}
	if !strings.Contains(got, "Credential ref: adapter-managed; not stored by cr") {
		t.Fatalf("text output missing adapter-managed credential note:\n%s", got)
	}
}

func TestRenderConfigTextOpenAIAPIKeyStatus(t *testing.T) {
	var out bytes.Buffer
	profile := workProfile()
	profile.LLM.Provider = config.LLMProviderOpenAI
	profile.LLM.Adapter = config.LLMAdapterOpenAIAPI
	show := NewConfigShow("work", profile, dataConfig(), []CredentialStatus{
		credentialStatus("llm", "codereview/work-llm", "api_key", "openai_api_key", true),
	})

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "openai_api_key: present") {
		t.Fatalf("text output missing OpenAI key status:\n%s", got)
	}
	if strings.Contains(got, "anthropic_api_key") {
		t.Fatalf("text output used Anthropic key for OpenAI profile:\n%s", got)
	}
}

func TestRenderConfigTextAgentSourceStatus(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("home", homeProfile(), dataConfig(), nil)
	show.AgentSources = []agents.SourceInfo{
		{
			Kind:            agents.SourceProfile,
			Label:           "org-reviewers",
			ProvenanceLabel: "profile:org-reviewers",
			ConfiguredPath:  "/Library/Application Support/codereview/agents",
			CanonicalPath:   "/Library/Application Support/codereview/agents",
			Present:         true,
			Status:          agents.SourceStatusAvailable,
			Fingerprint:     "sha256:abc123def456",
			Warnings:        []string{"canonical path is inside Git worktree /repo"},
		},
		{
			Kind:            agents.SourceProfile,
			Label:           "missing",
			ProvenanceLabel: "profile:missing",
			ConfiguredPath:  "/missing",
			Status:          agents.SourceStatusMissing,
			Error:           "agents: read source /missing: no such file or directory",
		},
	}

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Agent sources:",
		"/Library/Application Support/codereview/agents (available)",
		"Fingerprint: sha256:abc123def456",
		"Warning: canonical path is inside Git worktree /repo",
		"/missing (missing)",
		"Error: agents: read source /missing",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderConfigTextExactHomeShape(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("home", homeProfile(), dataConfig(), []CredentialStatus{
		credentialStatus("git", "codereview/home", "pat", "git_token", false),
	})

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	want := `Profile: home
Git:
  Host: github.com
  Auth mode: pat
  Credential ref: codereview/home
Reviewer credentials: self-review uses git credentials
LLM:
  Provider: anthropic
  Auth: subscription
  Adapter: claude_cli
  Credential ref: adapter-managed; not stored by cr
  Model map:
    small: <unset> (unset)
    medium: sonnet (built_in)
    large: opus (built_in)
Credentials:
  - git: codereview/home (pat)
    git_token: missing
Review policy:
  Major event: comment
  Allow self approve: false
Data retention:
  Max age days: 90
  Enforcement: at_write
`
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigJSON(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), []CredentialStatus{
		credentialStatus("git", "codereview/work", "pat", "git_token", true),
	})

	if err := RenderConfigJSON(&out, show); err != nil {
		t.Fatalf("RenderConfigJSON: %v", err)
	}
	var decoded ConfigShow
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if decoded.ActiveProfile != "work" {
		t.Fatalf("active_profile = %q, want work", decoded.ActiveProfile)
	}
	if decoded.LLMCredential.Mode != "stored_ref" || decoded.LLMCredential.Ref != "codereview/work-llm" {
		t.Fatalf("llm_credential = %#v, want stored ref", decoded.LLMCredential)
	}
	if len(decoded.CredentialRefs) != 1 || decoded.CredentialRefs[0].Ref != "codereview/work" {
		t.Fatalf("credential_refs = %#v, want git ref", decoded.CredentialRefs)
	}
}

func TestRenderConfigClearTextIncludesResetFields(t *testing.T) {
	var out bytes.Buffer
	result := ConfigClear{
		Backend:              "file",
		BackendSource:        "config",
		Cleared:              []ClearedCredentialRef{{Ref: "codereview/work", Keys: []string{"git_token"}}},
		ConfigProfileRemoved: "work",
		DefaultProfile:       "home",
		ConfigPathRemoved:    "/tmp/codereview/config.yml",
		Cache:                &CacheClear{Path: "/tmp/codereview-cache", Status: "removed"},
	}

	if err := RenderConfigClearText(&out, result); err != nil {
		t.Fatalf("RenderConfigClearText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Config profile removed: work",
		"Default profile: home",
		"Config path removed: /tmp/codereview/config.yml",
		"Cache path: /tmp/codereview-cache",
		"Cache status: removed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderConfigClearTextIncludesDryRun(t *testing.T) {
	var out bytes.Buffer
	result := ConfigClear{
		Backend:       "file",
		BackendSource: "config",
		DryRun:        true,
		Cleared:       []ClearedCredentialRef{{Ref: "codereview/work", Keys: []string{"git_token"}}},
	}

	if err := RenderConfigClearText(&out, result); err != nil {
		t.Fatalf("RenderConfigClearText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Dry run: true",
		"Credential targets:",
		"codereview/work: 1 key(s)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Cleared credentials:") {
		t.Fatalf("dry-run text included mutating heading:\n%s", got)
	}
}

func TestRenderConfigClearTextIncludesCacheErrorWithoutPath(t *testing.T) {
	var out bytes.Buffer
	result := ConfigClear{
		Backend:       "file",
		BackendSource: "config",
		Cache:         &CacheClear{Status: "error", Error: "xdg cache unavailable"},
	}

	if err := RenderConfigClearText(&out, result); err != nil {
		t.Fatalf("RenderConfigClearText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Cache status: error",
		"Cache error: xdg cache unavailable",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Cache path:") {
		t.Fatalf("text output included empty cache path:\n%s", got)
	}
}

func homeProfile() config.Profile {
	return config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/home",
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
	}
}

func credentialStatus(purpose, ref, mode, key string, present bool) CredentialStatus {
	status := "missing"
	if present {
		status = "present"
	}
	return CredentialStatus{
		Purpose: purpose,
		Ref:     ref,
		Mode:    mode,
		Keys: []KeyStatus{{
			Key:     key,
			Present: &present,
			Status:  status,
		}},
	}
}

func workProfile() config.Profile {
	return config.Profile{
		Git: config.GitConfig{
			Host:          "github.com",
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/work",
			IdentityCache: "rianjs",
		},
		ReviewerCredentials: &config.ReviewerCredentials{
			AuthMode:      config.GitAuthModePAT,
			CredentialRef: "codereview/work-reviewer",
			IdentityCache: "acme-review-bot",
		},
		LLM: config.LLMConfig{
			Provider:      config.LLMProviderAnthropic,
			Auth:          config.LLMAuthAPIKey,
			Adapter:       config.LLMAdapterAnthropicAPI,
			CredentialRef: "codereview/work-llm",
		},
		AgentSources: []string{"~/dev/work-reviewers"},
		ReviewPolicy: config.ReviewPolicy{
			MajorEvent:       config.ReviewMajorEventRequestChanges,
			AllowSelfApprove: true,
			ResolveThreads:   config.ResolveThreadsNever,
			ResolveAfter:     "48h",
		},
	}
}

func dataConfig() config.DataConfig {
	return config.DataConfig{
		Retention: config.RetentionConfig{
			MaxAgeDays:  intPtr(90),
			Enforcement: config.RetentionAtWrite,
		},
	}
}

func intPtr(value int) *int {
	return &value
}
