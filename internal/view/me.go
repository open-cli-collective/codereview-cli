package view

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/open-cli-collective/codereview-cli/internal/identity"
)

// MeResult is the presentation model for `cr me`.
type MeResult struct {
	Profiles []MeProfile `json:"profiles"`
}

// MeProfile describes one live identity lookup.
type MeProfile struct {
	Profile               string `json:"profile"`
	CredentialSource      string `json:"credential_source"`
	Host                  string `json:"host"`
	Login                 string `json:"login"`
	ID                    string `json:"id"`
	DisplayName           string `json:"display_name"`
	PreviousIdentityCache string `json:"previous_identity_cache"`
	IdentityCacheUpdated  bool   `json:"identity_cache_updated"`
}

// NewMeResult builds the me presentation model.
func NewMeResult(results []identity.ProfileResult) MeResult {
	profiles := make([]MeProfile, 0, len(results))
	for _, result := range results {
		profiles = append(profiles, MeProfile{
			Profile:               result.Profile,
			CredentialSource:      string(result.CredentialSource),
			Host:                  result.Host,
			Login:                 result.Identity.Login,
			ID:                    result.Identity.ID,
			DisplayName:           result.Identity.DisplayName,
			PreviousIdentityCache: result.PreviousIdentityCache,
			IdentityCacheUpdated:  result.IdentityCacheUpdated,
		})
	}
	return MeResult{Profiles: profiles}
}

// RenderMeText writes a stable human-readable identity summary.
func RenderMeText(w io.Writer, result MeResult) error {
	for i, profile := range result.Profiles {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if err := writeKV(w, "Profile", profile.Profile); err != nil {
			return err
		}
		if err := writeKV(w, "Credential source", profile.CredentialSource); err != nil {
			return err
		}
		if err := writeKV(w, "Host", profile.Host); err != nil {
			return err
		}
		if err := writeKV(w, "Login", profile.Login); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "ID", profile.ID); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "Display name", profile.DisplayName); err != nil {
			return err
		}
		if err := writeOptionalKV(w, "Previous identity cache", profile.PreviousIdentityCache); err != nil {
			return err
		}
		if err := writeKV(w, "Identity cache updated", fmt.Sprint(profile.IdentityCacheUpdated)); err != nil {
			return err
		}
	}
	return nil
}

// RenderMeJSON writes the identity summary as indented JSON.
func RenderMeJSON(w io.Writer, result MeResult) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
