// Package gitexec runs authenticated, non-interactive Git commands.
package gitexec

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// TokenSource supplies the current repository access token.
type TokenSource interface {
	AccessToken(context.Context) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(context.Context) (string, error)

// AccessToken returns the current token.
func (f TokenSourceFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

// defaultBasicAuthUsername is the GitHub HTTPS transport convention.
const defaultBasicAuthUsername = "x-access-token"

const (
	fetchMaxAttempts = 4
	fetchBaseDelay   = 2 * time.Second
)

// transientFetchOutput matches the messages git and curl print, under the LC_ALL=C
// this client forces, for a failure another attempt can fix. A fetch the host
// refuses for good, such as an unadvertised SHA, matches none of them.
var transientFetchOutput = regexp.MustCompile(`(?i)returned error: (429|5\d\d)|rpc failed|connection reset|recv failure|failed to connect|could not resolve host|timed out|empty reply from server|early eof|unexpected disconnect|remote end hung up|tls connection|ssl_read|gnutls_|http/2 stream`)

// RetryEvent describes one scheduled retry of a git fetch.
type RetryEvent struct {
	Attempt     int // the attempt that just failed, starting at 1
	MaxAttempts int
	Delay       time.Duration
	Reason      string
}

// Client runs Git with host-scoped HTTPS authentication.
type Client struct {
	host     string
	tokens   TokenSource
	username string
	// OnRetry observes each scheduled fetch retry.
	OnRetry func(RetryEvent)
	// sleep waits between fetch attempts. Tests replace it to avoid real waits.
	sleep func(context.Context, time.Duration) error
}

// New constructs an authenticated Git client using the GitHub HTTPS username
// convention.
func New(host string, tokens TokenSource) (*Client, error) {
	return NewWithUsername(host, tokens, defaultBasicAuthUsername)
}

// NewWithUsername constructs an authenticated Git client whose HTTPS basic
// auth pairs the given username with the token (for example GitLab expects
// "oauth2").
func NewWithUsername(host string, tokens TokenSource, username string) (*Client, error) {
	host = strings.TrimSpace(strings.TrimSuffix(host, "/"))
	if host == "" || strings.Contains(host, "://") || strings.Contains(host, "/") {
		return nil, fmt.Errorf("gitexec: valid host is required")
	}
	if tokens == nil {
		return nil, fmt.Errorf("gitexec: token source is required")
	}
	username = strings.TrimSpace(username)
	if username == "" || strings.Contains(username, ":") {
		return nil, fmt.Errorf("gitexec: valid basic auth username is required")
	}
	return &Client{host: host, tokens: tokens, username: username, sleep: sleepContext}, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run executes Git in dir, or the current directory when dir is empty. A fetch
// only reads, so one that fails transiently is repeated within a small bound.
func (c *Client) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "fetch" {
		return c.runOnce(ctx, dir, args...)
	}
	for attempt := 1; ; attempt++ {
		output, err := c.runOnce(ctx, dir, args...)
		if err == nil || ctx.Err() != nil || !transientFetchFailure(err) {
			return output, err
		}
		if attempt >= fetchMaxAttempts {
			return nil, gitprovider.WrapError(gitprovider.ErrRetryable, "", fmt.Errorf("%w (gave up after %d attempts)", err, attempt))
		}
		backoff := fetchBaseDelay << (attempt - 1)
		delay := backoff/2 + rand.N(backoff/2+1) // #nosec G404 -- backoff spacing, not a secret.
		if c.OnRetry != nil {
			c.OnRetry(RetryEvent{Attempt: attempt, MaxAttempts: fetchMaxAttempts, Delay: delay, Reason: err.Error()})
		}
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			if errors.Is(sleepErr, context.Canceled) {
				return nil, context.Canceled
			}
			return nil, gitprovider.WrapError(gitprovider.ErrRetryable, "", err)
		}
	}
}

// commandError keeps git's own output apart from its arguments, which carry
// PR-controlled ref names that must not decide whether a failure is transient.
type commandError struct {
	args   []string
	detail string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), e.detail)
}

func transientFetchFailure(err error) bool {
	var failure *commandError
	return errors.As(err, &failure) && transientFetchOutput.MatchString(failure.detail)
}

func (c *Client) runOnce(ctx context.Context, dir string, args ...string) ([]byte, error) {
	header := ""
	token := ""
	if requiresAuthentication(c.host, args) {
		var err error
		token, err = c.tokens.AccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("gitexec: read access token: %w", err)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, fmt.Errorf("gitexec: access token is empty")
		}
		header = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+token))
	}
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
	if token != "" {
		detail = strings.ReplaceAll(detail, token, "[REDACTED]")
	}
	if header != "" {
		detail = strings.ReplaceAll(detail, header, "[REDACTED]")
	}
	return nil, &commandError{args: args, detail: detail}
}

func requiresAuthentication(host string, args []string) bool {
	if len(args) < 2 || args[0] != "fetch" {
		return false
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		remote, err := url.Parse(arg)
		return err == nil && remote.Scheme == "https" && strings.EqualFold(remote.Host, host)
	}
	return false
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
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"LANG=C")
	if header != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://"+host+"/.extraHeader",
			"GIT_CONFIG_VALUE_0="+header)
	}
	return env
}
