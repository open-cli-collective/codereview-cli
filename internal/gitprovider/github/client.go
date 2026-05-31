// Package github adapts GitHub REST and GraphQL APIs to gitprovider read models.
package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const (
	defaultHost       = "github.com"
	credentialTypePAT = "pat"
)

var (
	// ErrValidation identifies non-retryable adapter input or GitHub validation failures.
	ErrValidation = errors.New("github: validation error")
	// ErrUnhandledGraphQL identifies non-retryable GraphQL errors not covered by the provider taxonomy.
	ErrUnhandledGraphQL = errors.New("github: unhandled graphql error")
)

// TokenStore is the minimal credential-store dependency needed by the adapter factory.
type TokenStore interface {
	Get(profile, key string) (string, error)
}

// Options configures a GitHub client.
type Options struct {
	Host       string
	Token      string
	HTTPClient *http.Client
	BaseURL    string
	GraphQLURL string
}

// Client is a GitHub read adapter. CR-10 adds write methods and full
// gitprovider.GitProvider satisfaction.
type Client struct {
	host       string
	token      string
	httpClient *http.Client
	baseURL    *url.URL
	graphQLURL *url.URL
}

// NewFromGitConfig builds a GitHub client and credential from config plus a token store.
func NewFromGitConfig(git config.GitConfig, store TokenStore, opts Options) (*Client, gitprovider.Credential, error) {
	if git.AuthMode != config.GitAuthModePAT {
		return nil, gitprovider.Credential{}, fmt.Errorf("%w: git auth_mode %q", config.ErrUnsupported, git.AuthMode)
	}
	host, err := normalizeHost(git.Host)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	if opts.Host != "" {
		optHost, err := normalizeHost(opts.Host)
		if err != nil {
			return nil, gitprovider.Credential{}, err
		}
		if optHost != host {
			return nil, gitprovider.Credential{}, fmt.Errorf("%w: options host %q conflicts with config host %q", ErrValidation, optHost, host)
		}
	}
	if store == nil {
		return nil, gitprovider.Credential{}, fmt.Errorf("%w: token store is required", gitprovider.ErrAuth)
	}
	parsed, err := credentials.ParseRef(git.CredentialRef)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	key, err := credentials.KeyForPurpose(config.CredentialRef{
		Purpose: "git",
		Ref:     git.CredentialRef,
		Mode:    string(git.AuthMode),
	})
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	token, err := store.Get(parsed.Profile, key)
	if err != nil {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, fmt.Errorf("read git credential: %w", err))
	}
	credential := gitprovider.Credential{Type: credentialTypePAT, Token: token}
	if err := validateCredential(credential); err != nil {
		return nil, gitprovider.Credential{}, err
	}
	opts.Host = host
	opts.Token = token
	client, err := New(opts)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	return client, credential, nil
}

// New builds a GitHub client from explicit options.
func New(opts Options) (*Client, error) {
	normalizedHost, err := normalizeHost(opts.Host)
	if err != nil {
		return nil, err
	}
	credential := gitprovider.Credential{Type: credentialTypePAT, Token: opts.Token}
	if err := validateCredential(credential); err != nil {
		return nil, err
	}
	baseURL, graphQLURL, err := resolveURLs(normalizedHost, opts.BaseURL, opts.GraphQLURL)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		host:       normalizedHost,
		token:      opts.Token,
		httpClient: httpClient,
		baseURL:    baseURL,
		graphQLURL: graphQLURL,
	}, nil
}

// Host returns the normalized host this client is bound to.
func (c *Client) Host() string {
	if c == nil {
		return ""
	}
	return c.host
}

// Capabilities returns GitHub feature support.
func (c *Client) Capabilities() gitprovider.ProviderCaps {
	return gitprovider.ProviderCaps{
		NativeFileLevelComments: true,
		ThreadResolution:        true,
	}
}

func validateCredential(creds gitprovider.Credential) error {
	if creds.Type != credentialTypePAT {
		return gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, fmt.Errorf("unsupported credential type %q", creds.Type))
	}
	if strings.TrimSpace(creds.Token) == "" {
		return gitprovider.WrapError(gitprovider.ErrAuth, gitprovider.OperationWhoAmI, fmt.Errorf("credential token is required"))
	}
	return nil
}

func (c *Client) validatePRRef(ref gitprovider.PRRef) error {
	if c == nil {
		return fmt.Errorf("%w: nil client", ErrValidation)
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if !strings.EqualFold(ref.Host, c.host) {
		return fmt.Errorf("%w: PR host %q does not match client host %q", ErrValidation, ref.Host, c.host)
	}
	if err := validatePathSegment("owner", ref.Owner); err != nil {
		return err
	}
	if err := validatePathSegment("repo", ref.Repo); err != nil {
		return err
	}
	return nil
}

func normalizeHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("%w: host is required", ErrValidation)
	}
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("%w: invalid host %q", ErrValidation, host)
		}
		host = parsed.Host
	}
	return strings.ToLower(strings.TrimSuffix(host, "/")), nil
}

func resolveURLs(host, baseOverride, graphQLOverride string) (*url.URL, *url.URL, error) {
	base := baseOverride
	graphQL := graphQLOverride
	if base == "" {
		if host == defaultHost {
			base = "https://api.github.com/"
		} else {
			base = "https://" + host + "/api/v3/"
		}
	}
	if graphQL == "" {
		if host == defaultHost {
			graphQL = "https://api.github.com/graphql"
		} else {
			graphQL = "https://" + host + "/api/graphql"
		}
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, nil, fmt.Errorf("%w: invalid REST base URL %q", ErrValidation, base)
	}
	graphQLURL, err := url.Parse(graphQL)
	if err != nil || graphQLURL.Scheme == "" || graphQLURL.Host == "" {
		return nil, nil, fmt.Errorf("%w: invalid GraphQL URL %q", ErrValidation, graphQL)
	}
	return ensureTrailingSlash(baseURL), graphQLURL, nil
}

func ensureTrailingSlash(u *url.URL) *url.URL {
	copied := *u
	if !strings.HasSuffix(copied.Path, "/") {
		copied.Path += "/"
	}
	return &copied
}

func validatePathSegment(name, value string) error {
	if strings.Contains(value, "/") {
		return fmt.Errorf("%w: %s must not contain slash", ErrValidation, name)
	}
	return nil
}
