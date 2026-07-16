package gitexec

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWithUsernamePairsUsernameWithToken(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nenv > \"$GITEXEC_RECORD.env\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITEXEC_RECORD", record)

	client, err := NewWithUsername("gitlab.example.com", TokenSourceFunc(func(context.Context) (string, error) {
		return "glpat-secret", nil
	}), "oauth2")
	if err != nil {
		t.Fatalf("NewWithUsername: %v", err)
	}
	if _, err := client.Run(context.Background(), "", "fetch", "https://gitlab.example.com/group/subgroup/project.git"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env, err := os.ReadFile(record + ".env") // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatal(err)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("oauth2:glpat-secret"))
	if !strings.Contains(string(env), wantAuth) {
		t.Fatalf("git environment = %s, want oauth2 basic auth", env)
	}
}

func TestNewWithUsernameRejectsInvalidUsername(t *testing.T) {
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })
	for _, username := range []string{"", "  ", "user:name"} {
		if _, err := NewWithUsername("gitlab.example.com", tokens, username); err == nil {
			t.Errorf("NewWithUsername(%q) error = nil, want error", username)
		}
	}
}
