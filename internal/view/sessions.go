package view

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
)

// SessionsList is the presentation model for `cr sessions list`.
type SessionsList struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionsShow is the presentation model for `cr sessions show`.
type SessionsShow struct {
	Session SessionSummary `json:"session"`
}

// SessionsDelete is the presentation model for `cr sessions delete`.
type SessionsDelete struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// SessionSummary describes one named LLM session.
type SessionSummary struct {
	Name              string    `json:"name"`
	Profile           string    `json:"profile"`
	Provider          string    `json:"provider"`
	Adapter           string    `json:"adapter"`
	Model             string    `json:"model"`
	Host              string    `json:"host"`
	ProviderSessionID string    `json:"provider_session_id"`
	CreatedAt         time.Time `json:"created_at"`
	LastUsedAt        time.Time `json:"last_used_at"`
}

// NewSessionsList builds a named-session list presentation model.
func NewSessionsList(sessions []ledger.NamedSession) SessionsList {
	out := SessionsList{Sessions: make([]SessionSummary, 0, len(sessions))}
	for _, session := range sessions {
		out.Sessions = append(out.Sessions, newSessionSummary(session))
	}
	return out
}

// NewSessionsShow builds a named-session detail presentation model.
func NewSessionsShow(session ledger.NamedSession) SessionsShow {
	return SessionsShow{Session: newSessionSummary(session)}
}

// NewSessionsDelete builds a named-session deletion presentation model.
func NewSessionsDelete(name string) SessionsDelete {
	return SessionsDelete{Name: name, Deleted: true}
}

// RenderSessionsListText writes a stable human-readable session list.
func RenderSessionsListText(w io.Writer, result SessionsList) error {
	if len(result.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "Sessions: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "Sessions:"); err != nil {
		return err
	}
	for _, session := range result.Sessions {
		if _, err := fmt.Fprintf(w, "  - %s\n", session.Name); err != nil {
			return err
		}
		if err := writeKV(w, "    Profile", session.Profile); err != nil {
			return err
		}
		if err := writeKV(w, "    Provider", session.Provider); err != nil {
			return err
		}
		if err := writeKV(w, "    Adapter", session.Adapter); err != nil {
			return err
		}
		if err := writeKV(w, "    Model", session.Model); err != nil {
			return err
		}
		if err := writeKV(w, "    Host", session.Host); err != nil {
			return err
		}
		if err := writeKV(w, "    Last used", session.LastUsedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

// RenderSessionsListJSON writes a named-session list as indented JSON.
func RenderSessionsListJSON(w io.Writer, result SessionsList) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// RenderSessionsShowText writes stable human-readable session details.
func RenderSessionsShowText(w io.Writer, result SessionsShow) error {
	session := result.Session
	if err := writeKV(w, "Session", session.Name); err != nil {
		return err
	}
	if err := writeKV(w, "Profile", session.Profile); err != nil {
		return err
	}
	if err := writeKV(w, "Provider", session.Provider); err != nil {
		return err
	}
	if err := writeKV(w, "Adapter", session.Adapter); err != nil {
		return err
	}
	if err := writeKV(w, "Model", session.Model); err != nil {
		return err
	}
	if err := writeKV(w, "Host", session.Host); err != nil {
		return err
	}
	if err := writeKV(w, "Provider session", session.ProviderSessionID); err != nil {
		return err
	}
	if err := writeKV(w, "Created", session.CreatedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	return writeKV(w, "Last used", session.LastUsedAt.Format(time.RFC3339))
}

// RenderSessionsShowJSON writes a named-session detail as indented JSON.
func RenderSessionsShowJSON(w io.Writer, result SessionsShow) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// RenderSessionsDeleteText writes a named-session deletion result.
func RenderSessionsDeleteText(w io.Writer, result SessionsDelete) error {
	_, err := fmt.Fprintf(w, "Deleted session: %s\n", result.Name)
	return err
}

// RenderSessionsDeleteJSON writes a named-session deletion result as JSON.
func RenderSessionsDeleteJSON(w io.Writer, result SessionsDelete) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func newSessionSummary(session ledger.NamedSession) SessionSummary {
	return SessionSummary{
		Name:              session.Name,
		Profile:           session.Profile,
		Provider:          session.Provider,
		Adapter:           session.Adapter,
		Model:             session.Model,
		Host:              session.Host,
		ProviderSessionID: session.ProviderSessionID,
		CreatedAt:         session.CreatedAt,
		LastUsedAt:        session.LastUsedAt,
	}
}
