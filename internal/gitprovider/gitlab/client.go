// Package gitlab adapts the GitLab REST API to gitprovider read and write models.
package gitlab

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

const credentialTypePAT = "pat"

// ErrValidation identifies non-retryable adapter input or GitLab validation failures.
var ErrValidation = errors.New("gitlab: validation error")

// Options configures a GitLab client.
type Options struct {
	Host       string
	Token      string
	HTTPClient *http.Client
	BaseURL    string
}

// Client is a GitLab merge-request adapter implementing gitprovider.GitProvider.
type Client struct {
	host       string
	token      string
	httpClient *http.Client
	baseURL    *url.URL
}

// NewFromGitConfig builds a GitLab client and credential from config plus a token store.
func NewFromGitConfig(git config.GitConfig, store credentials.Reader, opts Options) (*Client, gitprovider.Credential, error) {
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
	parsed, err := credentials.ParseRef(git.Credential.Name)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	if git.AuthMode != config.GitAuthModePAT {
		return nil, gitprovider.Credential{}, fmt.Errorf("%w: git auth_mode %q for provider gitlab", config.ErrUnsupported, git.AuthMode)
	}
	key, err := credentials.KeyForPurpose(config.CredentialRef{
		Purpose: "git",
		Ref:     git.Credential.Name,
		Mode:    string(git.AuthMode),
	})
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	token, err := store.Get(parsed.Profile, key)
	if err != nil {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("read git credential: %w", err))
	}
	credential := gitprovider.Credential{Type: credentialTypePAT, Token: token}
	if err := validateCredential("", credential); err != nil {
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

// New builds a GitLab client from explicit options.
func New(opts Options) (*Client, error) {
	normalizedHost, err := normalizeHost(opts.Host)
	if err != nil {
		return nil, err
	}
	credential := gitprovider.Credential{Type: credentialTypePAT, Token: opts.Token}
	if err := validateCredential("", credential); err != nil {
		return nil, err
	}
	baseURL, err := resolveBaseURL(normalizedHost, opts.BaseURL)
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
	}, nil
}

// Host returns the normalized host this client is bound to.
func (c *Client) Host() string {
	if c == nil {
		return ""
	}
	return c.host
}

// Capabilities returns GitLab feature support.
func (c *Client) Capabilities() gitprovider.ProviderCaps {
	return gitprovider.ProviderCaps{
		NativeFileLevelComments: true,
		ThreadResolution:        true,
		BundleInlineOnSubmit:    false,
		ReviewSummaryAsComment:  true,
		HeadRefNamespace:        "merge-requests",
	}
}

func validateCredential(op gitprovider.Operation, creds gitprovider.Credential) error {
	if creds.Type != credentialTypePAT {
		return gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("unsupported credential type %q", creds.Type))
	}
	if strings.TrimSpace(creds.Token) == "" {
		return gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("credential token is required"))
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
	// GitLab namespaces may nest, so owners can contain slashes; every
	// segment must still be a plain path element.
	if err := validatePathSegments("owner", ref.Owner); err != nil {
		return err
	}
	if strings.Contains(ref.Repo, "/") {
		return fmt.Errorf("%w: repo must not contain slash", ErrValidation)
	}
	if err := validatePathSegments("repo", ref.Repo); err != nil {
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

// resolveBaseURL maps a host to its REST v4 base. gitlab.com and self-managed
// instances share the same layout.
func resolveBaseURL(host, baseOverride string) (*url.URL, error) {
	base := baseOverride
	if base == "" {
		base = "https://" + host + "/api/v4/"
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("%w: invalid REST base URL %q", ErrValidation, base)
	}
	return ensureTrailingSlash(baseURL), nil
}

func ensureTrailingSlash(u *url.URL) *url.URL {
	copied := *u
	if !strings.HasSuffix(copied.Path, "/") {
		copied.Path += "/"
	}
	return &copied
}

func validatePathSegments(name, value string) error {
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: %s must not contain empty or dot segments", ErrValidation, name)
		}
	}
	return nil
}
