package view

import (
	"bytes"
	"encoding/json"
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
	var decoded MeResult
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(decoded.Profiles) != 1 || decoded.Profiles[0].CredentialSource != "git" || decoded.Profiles[0].Login != "rianjs" {
		t.Fatalf("decoded = %#v, want one git profile", decoded)
	}
}
