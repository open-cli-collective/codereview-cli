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
	ActiveProfile  string             `json:"active_profile"`
	Profile        config.Profile     `json:"profile"`
	Data           config.DataConfig  `json:"data"`
	Backend        string             `json:"backend,omitempty"`
	BackendSource  string             `json:"backend_source,omitempty"`
	CredentialRef  string             `json:"credential_ref,omitempty"`
	CredentialRefs []CredentialStatus `json:"credential_refs"`
	LLMCredential  LLMCredential      `json:"llm_credential"`
}

// CredentialStatus reports key presence for one declared credential ref.
type CredentialStatus struct {
	Purpose string      `json:"purpose"`
	Ref     string      `json:"ref"`
	Mode    string      `json:"mode"`
	Keys    []KeyStatus `json:"keys,omitempty"`
}

// KeyStatus reports one non-secret key name and whether a value exists.
type KeyStatus struct {
	Key     string `json:"key"`
	Present *bool  `json:"present,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// LLMCredential describes how cr accounts for LLM credentials.
type LLMCredential struct {
	Mode string `json:"mode"`
	Ref  string `json:"ref,omitempty"`
}

// NewConfigShow builds the config presentation model.
func NewConfigShow(profileName string, profile config.Profile, data config.DataConfig, refs []CredentialStatus) ConfigShow {
	llmCredential := LLMCredential{Mode: "adapter_managed"}
	if profile.LLM.Auth == config.LLMAuthAPIKey {
		llmCredential = LLMCredential{Mode: "stored_ref", Ref: profile.LLM.CredentialRef}
	}
	credentialRef := profile.Git.CredentialRef
	return ConfigShow{
		ActiveProfile:  profileName,
		Profile:        profile,
		Data:           data,
		CredentialRef:  credentialRef,
		CredentialRefs: refs,
		LLMCredential:  llmCredential,
	}
}

// RenderConfigText writes a stable human-readable config summary.
func RenderConfigText(w io.Writer, show ConfigShow) error {
	if _, err := fmt.Fprintf(w, "Profile: %s\n", show.ActiveProfile); err != nil {
		return err
	}
	if show.Backend != "" {
		if err := writeKV(w, "Keyring backend", show.Backend); err != nil {
			return err
		}
	}
	if show.BackendSource != "" {
		if err := writeKV(w, "Keyring backend source", show.BackendSource); err != nil {
			return err
		}
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

	if len(show.CredentialRefs) > 0 {
		if _, err := fmt.Fprintln(w, "Credentials:"); err != nil {
			return err
		}
		for _, ref := range show.CredentialRefs {
			if _, err := fmt.Fprintf(w, "  - %s: %s (%s)\n", ref.Purpose, ref.Ref, ref.Mode); err != nil {
				return err
			}
			for _, key := range ref.Keys {
				if _, err := fmt.Fprintf(w, "    %s: %s\n", key.Key, key.Status); err != nil {
					return err
				}
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

// ConfigClear is the presentation model for `cr config clear`.
type ConfigClear struct {
	Backend              string                 `json:"backend"`
	BackendSource        string                 `json:"backend_source"`
	Cleared              []ClearedCredentialRef `json:"cleared"`
	ConfigProfileRemoved string                 `json:"config_profile_removed,omitempty"`
	DefaultProfile       string                 `json:"default_profile,omitempty"`
	ConfigPathRemoved    string                 `json:"config_path_removed,omitempty"`
	Cache                *CacheClear            `json:"cache,omitempty"`
}

// ClearedCredentialRef describes the keys removed from one credential ref.
type ClearedCredentialRef struct {
	Ref  string   `json:"ref"`
	Keys []string `json:"keys"`
}

// CacheClear describes cache cleanup performed by `config clear --all`.
type CacheClear struct {
	Path   string `json:"path,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// RenderConfigClearText writes a stable human-readable clear summary.
func RenderConfigClearText(w io.Writer, result ConfigClear) error {
	if err := writeKV(w, "Keyring backend", result.Backend); err != nil {
		return err
	}
	if err := writeKV(w, "Keyring backend source", result.BackendSource); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Cleared credentials:"); err != nil {
		return err
	}
	for _, cleared := range result.Cleared {
		if _, err := fmt.Fprintf(w, "  - %s: %d key(s)\n", cleared.Ref, len(cleared.Keys)); err != nil {
			return err
		}
	}
	if result.ConfigProfileRemoved != "" {
		if err := writeKV(w, "Config profile removed", result.ConfigProfileRemoved); err != nil {
			return err
		}
	}
	if result.DefaultProfile != "" {
		if err := writeKV(w, "Default profile", result.DefaultProfile); err != nil {
			return err
		}
	}
	if result.ConfigPathRemoved != "" {
		if err := writeKV(w, "Config path removed", result.ConfigPathRemoved); err != nil {
			return err
		}
	}
	if result.Cache != nil {
		if result.Cache.Path != "" {
			if err := writeKV(w, "Cache path", result.Cache.Path); err != nil {
				return err
			}
		}
		if result.Cache.Status != "" {
			if err := writeKV(w, "Cache status", result.Cache.Status); err != nil {
				return err
			}
		}
		if result.Cache.Error != "" {
			return writeKV(w, "Cache error", result.Cache.Error)
		}
	}
	return nil
}

// RenderConfigClearJSON writes the clear summary as indented JSON.
func RenderConfigClearJSON(w io.Writer, result ConfigClear) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// CredentialWrite is the JSON envelope for `cr set-credential`.
type CredentialWrite struct {
	Ref           string `json:"ref"`
	Key           string `json:"key"`
	Backend       string `json:"backend,omitempty"`
	BackendSource string `json:"backend_source,omitempty"`
	Written       bool   `json:"written"`
	Error         string `json:"error,omitempty"`
}

// RenderCredentialWriteJSON writes a set-credential result envelope.
func RenderCredentialWriteJSON(w io.Writer, result CredentialWrite) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
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
