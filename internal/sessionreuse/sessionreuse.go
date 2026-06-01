// Package sessionreuse validates named LLM session reuse scope.
package sessionreuse

import (
	"fmt"
	"strings"
)

// Scope is the compatibility tuple for one named provider session.
type Scope struct {
	Name     string
	Profile  string
	Provider string
	Adapter  string
	Model    string
	Host     string
}

// NormalizeModel returns the canonical stored model value.
func NormalizeModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "default"
	}
	return model
}

// Normalize returns scope with trimmed fields and normalized model.
func Normalize(scope Scope) Scope {
	scope.Name = strings.TrimSpace(scope.Name)
	scope.Profile = strings.TrimSpace(scope.Profile)
	scope.Provider = strings.TrimSpace(scope.Provider)
	scope.Adapter = strings.TrimSpace(scope.Adapter)
	scope.Model = NormalizeModel(scope.Model)
	scope.Host = strings.TrimSpace(scope.Host)
	return scope
}

// Validate rejects incomplete scope tuples.
func Validate(scope Scope) error {
	scope = Normalize(scope)
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "name", value: scope.Name},
		{name: "profile", value: scope.Profile},
		{name: "provider", value: scope.Provider},
		{name: "adapter", value: scope.Adapter},
		{name: "model", value: scope.Model},
		{name: "host", value: scope.Host},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("session %s is required", field.name)
		}
	}
	return nil
}

// Check compares stored and active scopes. The returned warning is non-empty
// only for allowed host drift.
func Check(stored Scope, active Scope) (string, error) {
	stored = Normalize(stored)
	active = Normalize(active)
	if err := Validate(active); err != nil {
		return "", err
	}
	if err := Validate(stored); err != nil {
		return "", err
	}
	if stored.Name != active.Name {
		return "", fmt.Errorf("session %q name mismatch: stored %q, active %q", active.Name, stored.Name, active.Name)
	}
	for _, field := range []struct {
		name   string
		stored string
		active string
	}{
		{name: "profile", stored: stored.Profile, active: active.Profile},
		{name: "provider", stored: stored.Provider, active: active.Provider},
		{name: "adapter", stored: stored.Adapter, active: active.Adapter},
		{name: "model", stored: stored.Model, active: active.Model},
	} {
		if field.stored != field.active {
			return "", fmt.Errorf("session %q %s mismatch: stored %q, active %q; use a different --session name or align the profile", active.Name, field.name, field.stored, field.active)
		}
	}
	if stored.Host != active.Host {
		return fmt.Sprintf("session %q host mismatch: stored %q, active %q; continuing", active.Name, stored.Host, active.Host), nil
	}
	return "", nil
}
