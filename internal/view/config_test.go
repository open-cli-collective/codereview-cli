package view

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestRenderConfigTextSecretsManagementProfiles(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), nil)
	show.Backend = "memory"
	show.BackendSource = "config"
	show.SecretsProfiles = []config.EffectiveSecretsProfile{
		{
			ID:        config.LegacyProjectedSecretsProfileID,
			Label:     "Legacy default",
			Backend:   "memory",
			IsDefault: true,
			Source:    config.EffectiveSecretsProfileSourceProjectedLegacy,
		},
		{
			ID:      "personal-keychain",
			Label:   "Personal Keychain",
			Backend: "keychain",
			Source:  config.EffectiveSecretsProfileSourceConfigured,
		},
	}

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Keyring backend: memory",
		"Keyring backend source: config",
		"Secrets management profiles:",
		"  - legacy-default: Legacy default (memory) [default]",
		"    Source: projected_legacy",
		"  - personal-keychain: Personal Keychain (keychain)",
		"    Source: configured",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderConfigTextSelectedSecretsManagement(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), nil)
	show.ActiveSecretsProfile = &ConfigSecretsProfile{
		ID:      "work-file",
		Label:   "Work File Store",
		Backend: "file",
		Source:  "configured",
	}

	if err := RenderConfigText(&out, show); err != nil {
		t.Fatalf("RenderConfigText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Selected secrets management: Work File Store (file)",
		"Selected secrets management source: configured",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
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
    medium: claude-sonnet-4-6 (built_in)
    large: claude-opus-4-8 (built_in)
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

func TestRenderConfigPathText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigPath{
		ConfigPath: "/tmp/codereview/config.yml",
		ConfigDir:  "/tmp/codereview",
	}

	if err := RenderConfigPathText(&out, result); err != nil {
		t.Fatalf("RenderConfigPathText: %v", err)
	}
	want := "Config path: /tmp/codereview/config.yml\nConfig dir: /tmp/codereview\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigPathJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigPath{
		ConfigPath: "/tmp/codereview/config.yml",
		ConfigDir:  "/tmp/codereview",
	}

	if err := RenderConfigPathJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigPathJSON: %v", err)
	}
	var decoded ConfigPath
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if decoded != result {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigDefaultText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigDefault{DefaultProfile: "work"}

	if err := RenderConfigDefaultText(&out, result); err != nil {
		t.Fatalf("RenderConfigDefaultText: %v", err)
	}
	want := "Default profile: work\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigDefaultJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigDefault{DefaultProfile: "work"}

	if err := RenderConfigDefaultJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigDefaultJSON: %v", err)
	}
	var decoded ConfigDefault
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if decoded != result {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigRetentionText(t *testing.T) {
	var out bytes.Buffer
	result := NewConfigRetention(config.RetentionConfig{
		MaxAgeDays:  intPtr(30),
		Enforcement: config.RetentionManualOnly,
	})

	if err := RenderConfigRetentionText(&out, result); err != nil {
		t.Fatalf("RenderConfigRetentionText: %v", err)
	}
	want := "Data retention:\n  Max age days: 30\n  Enforcement: manual_only\n"
	if out.String() != want {
		t.Fatalf("retention text = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigRetentionJSON(t *testing.T) {
	var out bytes.Buffer
	result := NewConfigRetention(config.RetentionConfig{
		MaxAgeDays:  intPtr(0),
		Enforcement: config.RetentionAtWrite,
	})

	if err := RenderConfigRetentionJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigRetentionJSON: %v", err)
	}
	var got ConfigRetention
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.MaxAgeDays != 0 || got.Enforcement != "at_write" {
		t.Fatalf("retention JSON = %#v, want keep forever at_write", got)
	}
}

func TestRenderConfigRoutesText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigRoutes{
		Routes: []ConfigRoute{
			{Profile: "home", Host: "github.com", Namespace: "open-cli-collective"},
			{Profile: "work", Host: "github.com", Namespace: "rianjs", Repos: []string{"bar", "baz"}},
		},
	}

	if err := RenderConfigRoutesText(&out, result); err != nil {
		t.Fatalf("RenderConfigRoutesText: %v", err)
	}
	want := "Routes:\n  - home: github.com/open-cli-collective\n  - work: github.com/rianjs [bar, baz]\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigRoutesTextEmpty(t *testing.T) {
	var out bytes.Buffer

	if err := RenderConfigRoutesText(&out, ConfigRoutes{}); err != nil {
		t.Fatalf("RenderConfigRoutesText: %v", err)
	}
	if out.String() != "Routes: none\n" {
		t.Fatalf("text output = %q, want empty routes text", out.String())
	}
}

func TestRenderConfigRoutesJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigRoutes{
		Routes: []ConfigRoute{
			{Profile: "home", Host: "github.com", Namespace: "open-cli-collective"},
			{Profile: "work", Host: "github.com", Namespace: "rianjs", Repos: []string{"bar", "baz"}},
		},
	}

	if err := RenderConfigRoutesJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigRoutesJSON: %v", err)
	}
	var decoded ConfigRoutes
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigSecretsProfilesText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfiles{
		Profiles: []ConfigSecretsProfile{
			{ID: "legacy-default", Label: "Legacy default", Backend: "memory", Source: "projected_legacy", IsDefault: true},
			{ID: "work", Label: "Work Keychain", Backend: "keychain", Source: "configured"},
		},
	}

	if err := RenderConfigSecretsProfilesText(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfilesText: %v", err)
	}
	want := "Secrets management profiles:\n  - legacy-default: Legacy default (memory, projected_legacy) [default]\n  - work: Work Keychain (keychain, configured)\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigSecretsProfilesJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfiles{
		Profiles: []ConfigSecretsProfile{
			{ID: "work", Label: "Work Keychain", Backend: "keychain", Source: "configured"},
		},
	}

	if err := RenderConfigSecretsProfilesJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfilesJSON: %v", err)
	}
	var decoded ConfigSecretsProfiles
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigSecretsProfileText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfile{
		ID:        "legacy-default",
		Label:     "Legacy default",
		Backend:   "memory",
		Source:    "projected_legacy",
		IsDefault: true,
	}

	if err := RenderConfigSecretsProfileText(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileText: %v", err)
	}
	want := "Secrets profile: legacy-default\nLabel: Legacy default\nBackend: memory\nSource: projected_legacy\nDefault: true\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigSecretsProfileTextWithOnePasswordDetails(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfile{
		ID:      "work-op",
		Backend: "op",
		BackendInfo: &ConfigSecretsProfileBackendDetails{
			OnePassword: &ConfigSecretsProfileOnePassword{
				Timeout:                "5s",
				VaultID:                "vault-123",
				ServiceAccountTokenEnv: "OP_SERVICE_ACCOUNT_TOKEN",
			},
		},
		Source: "configured",
	}

	if err := RenderConfigSecretsProfileText(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileText with 1Password: %v", err)
	}
	want := "" +
		"Secrets profile: work-op\n" +
		"Backend: op\n" +
		"1Password timeout: 5s\n" +
		"1Password vault id: vault-123\n" +
		"1Password service token env: OP_SERVICE_ACCOUNT_TOKEN\n" +
		"Source: configured\n" +
		"Default: false\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigSecretsProfileJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfile{
		ID:      "work",
		Label:   "Work Keychain",
		Backend: "keychain",
		Source:  "configured",
	}

	if err := RenderConfigSecretsProfileJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileJSON: %v", err)
	}
	var decoded ConfigSecretsProfile
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigSecretsProfileJSONWithOnePasswordDetails(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfile{
		ID:      "work-op",
		Backend: "op-desktop",
		BackendInfo: &ConfigSecretsProfileBackendDetails{
			OnePassword: &ConfigSecretsProfileOnePassword{
				Timeout:           "5s",
				VaultID:           "vault-123",
				DesktopAccountEnv: "OP_DESKTOP_ACCOUNT_ID",
			},
		},
		Source: "configured",
	}

	if err := RenderConfigSecretsProfileJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileJSON with 1Password: %v", err)
	}
	var decoded ConfigSecretsProfile
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigSecretsProfileDefaultText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfileDefault{
		DefaultProfile: &ConfigSecretsProfile{
			ID:      "work",
			Label:   "Work Keychain",
			Backend: "keychain",
			Source:  "configured",
		},
	}

	if err := RenderConfigSecretsProfileDefaultText(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileDefaultText: %v", err)
	}
	want := "Default secrets profile: work\nLabel: Work Keychain\nBackend: keychain\nSource: configured\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigSecretsProfileDefaultTextNone(t *testing.T) {
	var out bytes.Buffer

	if err := RenderConfigSecretsProfileDefaultText(&out, ConfigSecretsProfileDefault{}); err != nil {
		t.Fatalf("RenderConfigSecretsProfileDefaultText: %v", err)
	}
	if out.String() != "Default secrets profile: none\n" {
		t.Fatalf("text output = %q, want none text", out.String())
	}
}

func TestRenderConfigSecretsProfileDefaultJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigSecretsProfileDefault{
		DefaultProfile: &ConfigSecretsProfile{
			ID:      "work",
			Label:   "Work Keychain",
			Backend: "keychain",
			Source:  "configured",
		},
	}

	if err := RenderConfigSecretsProfileDefaultJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigSecretsProfileDefaultJSON: %v", err)
	}
	var decoded ConfigSecretsProfileDefault
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigResolveProfileText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigResolveProfile{
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/1",
		ResolvedProfile: "work",
		Source:          "repository_route",
		GitHost:         "github.com",
		MatchedRoute: &ConfigRoute{
			Profile:   "work",
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
	}

	if err := RenderConfigResolveProfileText(&out, result); err != nil {
		t.Fatalf("RenderConfigResolveProfileText: %v", err)
	}
	want := "PR URL: https://github.com/open-cli-collective/codereview-cli/pull/1\nResolved profile: work\nSource: repository_route\nMatched route: github.com/open-cli-collective [codereview-cli]\nGit host: github.com\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigResolveProfileJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigResolveProfile{
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/1",
		ResolvedProfile: "home",
		Source:          "default_profile",
		GitHost:         "github.com",
	}

	if err := RenderConfigResolveProfileJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigResolveProfileJSON: %v", err)
	}
	var decoded ConfigResolveProfile
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigResolveProfileJSONIncludesMatchedRoute(t *testing.T) {
	var out bytes.Buffer
	result := ConfigResolveProfile{
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/1",
		ResolvedProfile: "work",
		Source:          "repository_route",
		GitHost:         "github.com",
		MatchedRoute: &ConfigRoute{
			Profile:   "work",
			Host:      "github.com",
			Namespace: "open-cli-collective",
			Repos:     []string{"codereview-cli"},
		},
	}

	if err := RenderConfigResolveProfileJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigResolveProfileJSON: %v", err)
	}
	var decoded ConfigResolveProfile
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
	}
}

func TestRenderConfigAgentSourcesText(t *testing.T) {
	var out bytes.Buffer
	result := ConfigAgentSources{
		ActiveProfile: "home",
		AgentSources:  []string{"~/agents", "../shared/agents"},
	}

	if err := RenderConfigAgentSourcesText(&out, result); err != nil {
		t.Fatalf("RenderConfigAgentSourcesText: %v", err)
	}
	want := "Profile: home\nAgent sources:\n  - ~/agents\n  - ../shared/agents\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigAgentSourcesTextNone(t *testing.T) {
	var out bytes.Buffer
	result := ConfigAgentSources{ActiveProfile: "home"}

	if err := RenderConfigAgentSourcesText(&out, result); err != nil {
		t.Fatalf("RenderConfigAgentSourcesText: %v", err)
	}
	want := "Profile: home\nAgent sources: none\n"
	if out.String() != want {
		t.Fatalf("text output = %q, want %q", out.String(), want)
	}
}

func TestRenderConfigAgentSourcesJSON(t *testing.T) {
	var out bytes.Buffer
	result := ConfigAgentSources{
		ActiveProfile: "work",
		AgentSources:  []string{"./agents"},
	}

	if err := RenderConfigAgentSourcesJSON(&out, result); err != nil {
		t.Fatalf("RenderConfigAgentSourcesJSON: %v", err)
	}
	var decoded ConfigAgentSources
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("decoded = %#v, want %#v", decoded, result)
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

func TestRenderConfigClearTextIncludesSelectedSecretsManagement(t *testing.T) {
	var out bytes.Buffer
	result := ConfigClear{
		Backend:       "file",
		BackendSource: "secrets_profile",
		ActiveSecretsProfile: &ConfigSecretsProfile{
			ID:      "work-file",
			Label:   "Work File Store",
			Backend: "file",
			Source:  "configured",
		},
	}

	if err := RenderConfigClearText(&out, result); err != nil {
		t.Fatalf("RenderConfigClearText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Selected secrets management: Work File Store (file)",
		"Selected secrets management source: configured",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
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
			Key:      key,
			Required: true,
			Present:  &present,
			Status:   status,
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
