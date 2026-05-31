package view

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/identity"
)

func TestRenderMeText(t *testing.T) {
	result := NewMeResult([]identity.ProfileResult{{
		Profile:               "work",
		CredentialSource:      identity.SourceReviewer,
		Host:                  "github.com",
		Identity:              gitprovider.Identity{Login: "bot", ID: "123", DisplayName: "Review Bot"},
		PreviousIdentityCache: "old-bot",
		IdentityCacheUpdated:  true,
	}})

	var out bytes.Buffer
	if err := RenderMeText(&out, result); err != nil {
		t.Fatalf("RenderMeText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Profile: work",
		"Credential source: reviewer_credentials",
		"Host: github.com",
		"Login: bot",
		"ID: 123",
		"Display name: Review Bot",
		"Previous identity cache: old-bot",
		"Identity cache updated: true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderMeJSON(t *testing.T) {
	result := NewMeResult([]identity.ProfileResult{{
		Profile:          "home",
		CredentialSource: identity.SourceGit,
		Host:             "github.com",
		Identity:         gitprovider.Identity{Login: "rianjs"},
	}})

	var out bytes.Buffer
	if err := RenderMeJSON(&out, result); err != nil {
		t.Fatalf("RenderMeJSON: %v", err)
	}
	want := `{
  "profiles": [
    {
      "profile": "home",
      "credential_source": "git",
      "host": "github.com",
      "login": "rianjs",
      "id": "",
      "display_name": "",
      "previous_identity_cache": "",
      "identity_cache_updated": false
    }
  ]
}
`
	if got := out.String(); got != want {
		t.Fatalf("JSON = %q, want %q", got, want)
	}
}
