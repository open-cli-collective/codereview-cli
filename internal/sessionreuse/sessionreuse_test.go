package sessionreuse

import (
	"strings"
	"testing"
)

func TestCheckCompatibility(t *testing.T) {
	active := Scope{Name: "epic", Profile: "work", Provider: "anthropic", Adapter: "claude_cli", Model: "", Host: "github.com"}
	stored := Normalize(active)

	if warning, err := Check(stored, active); err != nil || warning != "" {
		t.Fatalf("Check matching = warning %q err %v, want clean", warning, err)
	}

	tests := []struct {
		name   string
		mutate func(*Scope)
		want   string
	}{
		{name: "profile", mutate: func(s *Scope) { s.Profile = "home" }, want: "profile mismatch"},
		{name: "provider", mutate: func(s *Scope) { s.Provider = "openai" }, want: "provider mismatch"},
		{name: "adapter", mutate: func(s *Scope) { s.Adapter = "openai_api" }, want: "adapter mismatch"},
		{name: "model", mutate: func(s *Scope) { s.Model = "sonnet" }, want: "model mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := active
			tt.mutate(&changed)
			if _, err := Check(stored, changed); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check error = %v, want %q", err, tt.want)
			}
		})
	}

	crossHost := active
	crossHost.Host = "github.enterprise"
	warning, err := Check(stored, crossHost)
	if err != nil {
		t.Fatalf("Check cross host: %v", err)
	}
	if !strings.Contains(warning, "host mismatch") || !strings.Contains(warning, "continuing") {
		t.Fatalf("warning = %q, want host warning", warning)
	}
}

func TestNormalizeModel(t *testing.T) {
	if got := NormalizeModel("  "); got != "default" {
		t.Fatalf("NormalizeModel blank = %q, want default", got)
	}
	if got := NormalizeModel(" sonnet "); got != "sonnet" {
		t.Fatalf("NormalizeModel = %q, want sonnet", got)
	}
}
