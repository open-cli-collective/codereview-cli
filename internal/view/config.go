// Package view renders user-facing command output from typed values.
package view

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// ConfigShow is the presentation model for `cr config show`.
type ConfigShow struct {
	ActiveProfile  string              `json:"active_profile"`
	Profile        config.Profile      `json:"profile"`
	Data           config.DataConfig   `json:"data"`
	Backend        string              `json:"backend,omitempty"`
	BackendSource  string              `json:"backend_source,omitempty"`
	CredentialRef  string              `json:"credential_ref,omitempty"`
	CredentialRefs []CredentialStatus  `json:"credential_refs"`
	LLMCredential  LLMCredential       `json:"llm_credential"`
	AgentSources   []agents.SourceInfo `json:"agent_sources,omitempty"`
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
	Key      string `json:"key"`
	Required bool   `json:"required"`
	Present  *bool  `json:"present,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
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
	if err := renderConfigModelMap(w, show.Profile.LLM); err != nil {
		return err
	}

	if len(show.AgentSources) > 0 {
		if _, err := fmt.Fprintln(w, "Agent sources:"); err != nil {
			return err
		}
		for _, source := range show.AgentSources {
			if _, err := fmt.Fprintf(w, "  - %s (%s)\n", source.ConfiguredPath, source.Status); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Canonical path", source.CanonicalPath); err != nil {
				return err
			}
			if err := writeOptionalKV(w, "    Fingerprint", source.Fingerprint); err != nil {
				return err
			}
			for _, warning := range source.Warnings {
				if _, err := fmt.Fprintf(w, "    Warning: %s\n", warning); err != nil {
					return err
				}
			}
			if err := writeOptionalKV(w, "    Error", source.Error); err != nil {
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
				label := key.Key
				if !key.Required {
					label += " (optional)"
				}
				if _, err := fmt.Fprintf(w, "    %s: %s\n", label, key.Status); err != nil {
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

func renderConfigModelMap(w io.Writer, llm config.LLMConfig) error {
	if _, err := fmt.Fprintln(w, "  Model map:"); err != nil {
		return err
	}
	effective := config.EffectiveModelMap(llm)
	for _, tier := range config.ModelTiers() {
		model := "<unset>"
		source := "unset"
		if resolved, ok := effective[tier]; ok {
			model = resolved.Model
			source = string(resolved.Source)
		}
		if _, err := fmt.Fprintf(w, "    %s: %s (%s)\n", tier, model, source); err != nil {
			return err
		}
	}
	return nil
}

// RenderConfigJSON writes the config summary as indented JSON.
func RenderConfigJSON(w io.Writer, show ConfigShow) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(show)
}

// ConfigPath is the presentation model for `cr config path`.
type ConfigPath struct {
	ConfigPath string `json:"config_path"`
	ConfigDir  string `json:"config_dir"`
}

// RenderConfigPathText writes a stable human-readable config-path summary.
func RenderConfigPathText(w io.Writer, result ConfigPath) error {
	if err := writeKV(w, "Config path", result.ConfigPath); err != nil {
		return err
	}
	return writeKV(w, "Config dir", result.ConfigDir)
}

// RenderConfigPathJSON writes the config-path summary as indented JSON.
func RenderConfigPathJSON(w io.Writer, result ConfigPath) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// ConfigDefault is the presentation model for `cr config default get/set`.
type ConfigDefault struct {
	DefaultProfile string `json:"default_profile"`
}

// RenderConfigDefaultText writes a stable human-readable default-profile summary.
func RenderConfigDefaultText(w io.Writer, result ConfigDefault) error {
	return writeKV(w, "Default profile", result.DefaultProfile)
}

// RenderConfigDefaultJSON writes the default-profile summary as indented JSON.
func RenderConfigDefaultJSON(w io.Writer, result ConfigDefault) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// ConfigRoutes is the presentation model for `cr config route list`.
type ConfigRoutes struct {
	Routes []ConfigRoute `json:"routes"`
}

// ConfigRoute is one repository-profile route.
type ConfigRoute struct {
	Profile   string   `json:"profile"`
	Host      string   `json:"host"`
	Namespace string   `json:"namespace"`
	Repos     []string `json:"repos,omitempty"`
}

// RenderConfigRoutesText writes a stable human-readable route listing.
func RenderConfigRoutesText(w io.Writer, result ConfigRoutes) error {
	if len(result.Routes) == 0 {
		_, err := fmt.Fprintln(w, "Routes: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "Routes:"); err != nil {
		return err
	}
	for _, route := range result.Routes {
		target := route.Host + "/" + route.Namespace
		if len(route.Repos) > 0 {
			target += " [" + strings.Join(route.Repos, ", ") + "]"
		}
		if _, err := fmt.Fprintf(w, "  - %s: %s\n", route.Profile, target); err != nil {
			return err
		}
	}
	return nil
}

// RenderConfigRoutesJSON writes the route listing as indented JSON.
func RenderConfigRoutesJSON(w io.Writer, result ConfigRoutes) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// ConfigResolveProfile is the presentation model for `cr config resolve-profile`.
type ConfigResolveProfile struct {
	PRURL           string       `json:"pr_url"`
	ResolvedProfile string       `json:"resolved_profile"`
	Source          string       `json:"source"`
	GitHost         string       `json:"git_host"`
	MatchedRoute    *ConfigRoute `json:"matched_route,omitempty"`
}

// RenderConfigResolveProfileText writes a stable human-readable resolution summary.
func RenderConfigResolveProfileText(w io.Writer, result ConfigResolveProfile) error {
	if err := writeKV(w, "PR URL", result.PRURL); err != nil {
		return err
	}
	if err := writeKV(w, "Resolved profile", result.ResolvedProfile); err != nil {
		return err
	}
	if err := writeKV(w, "Source", result.Source); err != nil {
		return err
	}
	if result.MatchedRoute != nil {
		target := result.MatchedRoute.Host + "/" + result.MatchedRoute.Namespace
		if len(result.MatchedRoute.Repos) > 0 {
			target += " [" + strings.Join(result.MatchedRoute.Repos, ", ") + "]"
		}
		if err := writeKV(w, "Matched route", target); err != nil {
			return err
		}
	}
	return writeKV(w, "Git host", result.GitHost)
}

// RenderConfigResolveProfileJSON writes the resolution summary as indented JSON.
func RenderConfigResolveProfileJSON(w io.Writer, result ConfigResolveProfile) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// ConfigAgentSources is the presentation model for `cr config agent-source`.
type ConfigAgentSources struct {
	ActiveProfile string   `json:"active_profile"`
	AgentSources  []string `json:"agent_sources"`
}

// RenderConfigAgentSourcesText writes a stable human-readable agent-source list.
func RenderConfigAgentSourcesText(w io.Writer, result ConfigAgentSources) error {
	if err := writeKV(w, "Profile", result.ActiveProfile); err != nil {
		return err
	}
	if len(result.AgentSources) == 0 {
		_, err := fmt.Fprintln(w, "Agent sources: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "Agent sources:"); err != nil {
		return err
	}
	for _, source := range result.AgentSources {
		if _, err := fmt.Fprintf(w, "  - %s\n", source); err != nil {
			return err
		}
	}
	return nil
}

// RenderConfigAgentSourcesJSON writes the agent-source list as indented JSON.
func RenderConfigAgentSourcesJSON(w io.Writer, result ConfigAgentSources) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// ConfigClear is the presentation model for `cr config clear`.
type ConfigClear struct {
	Backend              string                 `json:"backend"`
	BackendSource        string                 `json:"backend_source"`
	DryRun               bool                   `json:"dry_run"`
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
	if result.DryRun {
		if err := writeKV(w, "Dry run", "true"); err != nil {
			return err
		}
	}
	heading := "Cleared credentials:"
	if result.DryRun {
		heading = "Credential targets:"
	}
	if _, err := fmt.Fprintln(w, heading); err != nil {
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
