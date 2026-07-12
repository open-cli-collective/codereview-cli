// Package gitexec runs authenticated, non-interactive Git commands.
package gitexec

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// TokenSource supplies the current repository access token.
type TokenSource interface {
	AccessToken(context.Context) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(context.Context) (string, error)

// AccessToken returns the current token.
func (f TokenSourceFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

// Client runs Git with host-scoped HTTPS authentication.
type Client struct {
	host   string
	tokens TokenSource
}

// New constructs an authenticated Git client.
func New(host string, tokens TokenSource) (*Client, error) {
	host = strings.TrimSpace(strings.TrimSuffix(host, "/"))
	if host == "" || strings.Contains(host, "://") || strings.Contains(host, "/") {
		return nil, fmt.Errorf("gitexec: valid host is required")
	}
	if tokens == nil {
		return nil, fmt.Errorf("gitexec: token source is required")
	}
	return &Client{host: host, tokens: tokens}, nil
}

// Run executes Git in dir, or the current directory when dir is empty.
func (c *Client) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitexec: read access token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("gitexec: access token is empty")
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed git binary with structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnvironment(cmd.Environ(), c.host, header)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return output, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = runErr.Error()
	}
	detail = strings.ReplaceAll(detail, token, "[REDACTED]")
	detail = strings.ReplaceAll(detail, header, "[REDACTED]")
	return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
}

func gitEnvironment(base []string, host, header string) []string {
	env := make([]string, 0, len(base)+6)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") || key == "GIT_TERMINAL_PROMPT" || key == "LC_ALL" || key == "LANG" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://"+host+"/.extraHeader",
		"GIT_CONFIG_VALUE_0="+header,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"LANG=C",
	)
}
