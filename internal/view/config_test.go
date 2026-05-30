package view

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestRenderConfigTextStoredCredentialRefs(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: "pat"},
		{Purpose: "reviewer_credentials", Ref: "codereview/work-reviewer", Mode: "pat"},
		{Purpose: "llm", Ref: "codereview/work-llm", Mode: "api_key"},
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
	show := NewConfigShow("home", homeProfile(), dataConfig(), []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/home", Mode: "pat"},
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

func TestRenderConfigJSON(t *testing.T) {
	var out bytes.Buffer
	show := NewConfigShow("work", workProfile(), dataConfig(), []config.CredentialRef{
		{Purpose: "git", Ref: "codereview/work", Mode: "pat"},
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
