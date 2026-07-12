//go:build !windows

package gitexec

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rotatingTokens struct {
	values []string
	calls  int
}

func (s *rotatingTokens) AccessToken(context.Context) (string, error) {
	value := s.values[s.calls]
	s.calls++
	return value, nil
}

func TestClientUsesFreshHostScopedTokenWithoutLeakingArguments(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GITEXEC_RECORD.args\"\nenv > \"$GITEXEC_RECORD.env\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITEXEC_RECORD", record)

	tokens := &rotatingTokens{values: []string{"first-secret", "second-secret"}}
	client, err := New("github.example.com", tokens)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 2 {
		if _, err := client.Run(context.Background(), "", "fetch", "https://github.example.com/acme/repo.git"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	args, err := os.ReadFile(record + ".args") // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "secret") {
		t.Fatalf("arguments leaked token: %s", args)
	}
	env, err := os.ReadFile(record + ".env") // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatal(err)
	}
	got := string(env)
	if !strings.Contains(got, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(got, "GIT_CONFIG_KEY_0=http.https://github.example.com/.extraHeader") {
		t.Fatalf("git environment missing safety config: %s", got)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:second-secret"))
	if !strings.Contains(got, wantAuth) {
		t.Fatalf("git environment did not use refreshed token: %s", got)
	}
}

func TestClientRedactsTokenFromGitError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nenv | grep GIT_CONFIG_VALUE_0 >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const token = "distinct-secret-token"
	client, err := New("github.example.com", TokenSourceFunc(func(context.Context) (string, error) { return token, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Run(context.Background(), "", "fetch", "https://github.example.com/acme/repo.git")
	if err == nil {
		t.Fatal("Run succeeded, want git failure")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))) {
		t.Fatalf("Run error leaked token: %v", err)
	}
}
