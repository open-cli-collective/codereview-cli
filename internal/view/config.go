// Package view renders user-facing command output from typed values.
package view

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// ConfigShow is the presentation model for `cr config show`.
type ConfigShow struct {
	ActiveProfile  string                 `json:"active_profile"`
	Profile        config.Profile         `json:"profile"`
	Data           config.DataConfig      `json:"data"`
	CredentialRefs []config.CredentialRef `json:"credential_refs"`
	LLMCredential  LLMCredential          `json:"llm_credential"`
}

// LLMCredential describes how cr accounts for LLM credentials.
type LLMCredential struct {
	Mode string `json:"mode"`
	Ref  string `json:"ref,omitempty"`
}

// NewConfigShow builds the config presentation model.
func NewConfigShow(profileName string, profile config.Profile, data config.DataConfig, refs []config.CredentialRef) ConfigShow {
	llmCredential := LLMCredential{Mode: "adapter_managed"}
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		llmCredential = LLMCredential{Mode: "stored_ref", Ref: profile.LLM.CredentialRef}
	}
	return ConfigShow{
		ActiveProfile:  profileName,
		Profile:        profile,
		Data:           data,
		CredentialRefs: refs,
		LLMCredential:  llmCredential,
	}
}

// RenderConfigText writes a stable human-readable config summary.
func RenderConfigText(w io.Writer, show ConfigShow) error {
	if _, err := fmt.Fprintf(w, "Profile: %s\n", show.ActiveProfile); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Git:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Host", show.Profile.Git.Host); err != nil {
		return err
	}
	if err := writeKV(w, "  Auth mode", string(show.Profile.Git.AuthMode)); err != nil {
		return err
	}
	if err := writeKV(w, "  Credential ref", show.Profile.Git.CredentialRef); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Identity cache", show.Profile.Git.IdentityCache); err != nil {
		return err
	}

	if show.Profile.ReviewerCredentials == nil {
		if _, err := fmt.Fprintln(w, "Reviewer credentials: self-review uses git credentials"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "Reviewer credentials:"); err != nil {
			return err
		}
		if err := writeKV(w, "  Auth mode", string(show.Profile.ReviewerCredentials.AuthMode)); err != nil {
			return err
		}
		if err := writeKV(w, "  Credential ref", show.Profile.ReviewerCredentials.CredentialRef); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "  Identity cache", show.Profile.ReviewerCredentials.IdentityCache); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "LLM:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Provider", string(show.Profile.LLM.Provider)); err != nil {
		return err
	}
	if err := writeKV(w, "  Auth", string(show.Profile.LLM.Auth)); err != nil {
		return err
	}
	if err := writeKV(w, "  Adapter", string(show.Profile.LLM.Adapter)); err != nil {
		return err
	}
	if show.LLMCredential.Mode == "stored_ref" {
		if err := writeKV(w, "  Credential ref", show.LLMCredential.Ref); err != nil {
			return err
		}
	} else if err := writeKV(w, "  Credential ref", "adapter-managed; not stored by cr"); err != nil {
		return err
	}

	if len(show.Profile.AgentSources) > 0 {
		if _, err := fmt.Fprintln(w, "Agent sources:"); err != nil {
			return err
		}
		for _, source := range show.Profile.AgentSources {
			if _, err := fmt.Fprintf(w, "  - %s\n", source); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintln(w, "Review policy:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Major event", string(show.Profile.ReviewPolicy.MajorEvent)); err != nil {
		return err
	}
	if err := writeKV(w, "  Allow self approve", fmt.Sprint(show.Profile.ReviewPolicy.AllowSelfApprove)); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Resolve threads", string(show.Profile.ReviewPolicy.ResolveThreads)); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "  Resolve after", show.Profile.ReviewPolicy.ResolveAfter); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "Data retention:"); err != nil {
		return err
	}
	if err := writeKV(w, "  Max age days", fmt.Sprint(show.Data.Retention.MaxAgeDaysValue())); err != nil {
		return err
	}
	return writeKV(w, "  Enforcement", string(show.Data.Retention.Enforcement))
}

// RenderConfigJSON writes the config summary as indented JSON.
func RenderConfigJSON(w io.Writer, show ConfigShow) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(show)
}

func writeKV(w io.Writer, key, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\n", key, value)
	return err
}

func writeOptionalKV(w io.Writer, key, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return writeKV(w, key, value)
}
