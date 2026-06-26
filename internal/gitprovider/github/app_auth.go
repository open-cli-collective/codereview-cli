package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// InstallationLookup carries repository context for GitHub App installation lookup.
type InstallationLookup struct {
	Owner string
	Repo  string
}

type githubAppAuth struct {
	mu             sync.Mutex
	issuer         string
	privateKey     *rsa.PrivateKey
	installationID string
	lookup         *InstallationLookup
	httpClient     *http.Client
	baseURL        *url.URL
	now            func() time.Time
	tokenValue     string
	expiresAt      time.Time
	identity       gitprovider.Identity
	permissions    map[string]string
}

type githubAppInstallation struct {
	ID      int64  `json:"id"`
	AppID   int64  `json:"app_id"`
	AppSlug string `json:"app_slug"`
}

type installationTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

func newGitHubAppFromConfig(ctx context.Context, profile string, store TokenStore, opts Options) (*Client, gitprovider.Credential, error) {
	if store == nil {
		return nil, gitprovider.Credential{}, fmt.Errorf("%w: token store is required", gitprovider.ErrAuth)
	}
	host, err := normalizeHost(opts.Host)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	baseURL, graphQLURL, err := resolveURLs(host, opts.BaseURL, opts.GraphQLURL)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	issuer := strings.TrimSpace(opts.AppID)
	if issuer == "" {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("github app app_id is required in config for github_app auth"))
	}
	privateKeyPEM, err := readRequiredCredential(store, profile, credentials.GitHubAppPrivateKeyKey)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	privateKey, err := parseGitHubAppPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", err)
	}
	installationID := strings.TrimSpace(opts.InstallationID)
	if !canResolveInstallation(installationID, opts.InstallationLookup) {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("github app installation discovery requires repository context; pin an installation id or run against a repository"))
	}
	auth := &githubAppAuth{
		issuer:         issuer,
		privateKey:     privateKey,
		installationID: strings.TrimSpace(installationID),
		lookup:         opts.InstallationLookup,
		httpClient:     httpClient,
		baseURL:        baseURL,
		now:            now,
	}
	token, identity, err := auth.refresh(ctx, "")
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	client := &Client{
		host:       host,
		token:      token,
		httpClient: httpClient,
		baseURL:    baseURL,
		graphQLURL: graphQLURL,
		appAuth:    auth,
	}
	credential := gitprovider.Credential{
		Type:        credentialTypeGitHubApp,
		Token:       token,
		Login:       identity.Login,
		ID:          identity.ID,
		DisplayName: identity.DisplayName,
	}
	if err := validateCredential("", credential); err != nil {
		return nil, gitprovider.Credential{}, err
	}
	return client, credential, nil
}

func (a *githubAppAuth) token(ctx context.Context, op gitprovider.Operation) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.tokenValue) != "" && a.now().Add(gitHubAppRefreshSkew).Before(a.expiresAt) {
		return a.tokenValue, nil
	}
	token, _, err := a.refreshLocked(ctx, op)
	return token, err
}

func (a *githubAppAuth) refresh(ctx context.Context, op gitprovider.Operation) (string, gitprovider.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked(ctx, op)
}

func (a *githubAppAuth) refreshLocked(ctx context.Context, op gitprovider.Operation) (string, gitprovider.Identity, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", gitprovider.Identity{}, gitprovider.WrapError(gitprovider.ErrAuth, op, err)
	}
	installation, err := a.resolveInstallation(ctx, op, jwt)
	if err != nil {
		return "", gitprovider.Identity{}, err
	}
	token, expiresAt, permissions, err := a.createInstallationToken(ctx, op, jwt, installation.ID)
	if err != nil {
		return "", gitprovider.Identity{}, err
	}
	identity := identityFromInstallation(installation)
	a.installationID = strconv.FormatInt(installation.ID, 10)
	a.tokenValue = token
	a.expiresAt = expiresAt
	a.identity = identity
	a.permissions = normalizeInstallationPermissions(permissions)
	return token, identity, nil
}

func (a *githubAppAuth) resolveInstallation(ctx context.Context, op gitprovider.Operation, jwt string) (githubAppInstallation, error) {
	if strings.TrimSpace(a.installationID) != "" {
		endpoint := restURL(a.baseURL, "app", "installations", a.installationID)
		var installation githubAppInstallation
		if err := a.doAppREST(ctx, op, http.MethodGet, endpoint, jwt, nil, &installation); err != nil {
			return githubAppInstallation{}, mapPinnedInstallationLookupError(op, a.installationID, err)
		}
		return installation, nil
	}
	endpoint := restURL(a.baseURL, "repos", a.lookup.Owner, a.lookup.Repo, "installation")
	var installation githubAppInstallation
	if err := a.doAppREST(ctx, op, http.MethodGet, endpoint, jwt, nil, &installation); err != nil {
		return githubAppInstallation{}, mapRepositoryInstallationLookupError(op, a.lookup.Owner, a.lookup.Repo, err)
	}
	return installation, nil
}

func (a *githubAppAuth) createInstallationToken(ctx context.Context, op gitprovider.Operation, jwt string, installationID int64) (string, time.Time, map[string]string, error) {
	endpoint := restURL(a.baseURL, "app", "installations", strconv.FormatInt(installationID, 10), "access_tokens")
	var response installationTokenResponse
	if err := a.doAppREST(ctx, op, http.MethodPost, endpoint, jwt, map[string]any{}, &response); err != nil {
		return "", time.Time{}, nil, mapCreateInstallationTokenError(op, strconv.FormatInt(installationID, 10), err)
	}
	if strings.TrimSpace(response.Token) == "" {
		return "", time.Time{}, nil, gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("github app installation token is empty"))
	}
	if response.ExpiresAt.IsZero() {
		return "", time.Time{}, nil, gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("github app installation token expiry is empty"))
	}
	return response.Token, response.ExpiresAt, response.Permissions, nil
}

func (a *githubAppAuth) installationPermissions(ctx context.Context, op gitprovider.Operation) (map[string]string, error) {
	if _, err := a.token(ctx, op); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.permissions) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(a.permissions))
	for key, value := range a.permissions {
		out[key] = value
	}
	return out, nil
}

func normalizeInstallationPermissions(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapPinnedInstallationLookupError(op gitprovider.Operation, installationID string, err error) error {
	switch {
	case errors.Is(err, gitprovider.ErrNotFound):
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, fmt.Errorf("github app installation %s was not found for this app; check the installation id configured on the review profile: %w", installationID, err))
	case errors.Is(err, gitprovider.ErrPermission):
		return gitprovider.WrapError(gitprovider.ErrPermission, op, fmt.Errorf("github app installation %s is not accessible to this app; check the installation id configured on the review profile: %w", installationID, err))
	default:
		return err
	}
}

func mapRepositoryInstallationLookupError(op gitprovider.Operation, owner, repo string, err error) error {
	repository := strings.Trim(strings.TrimSpace(owner)+"/"+strings.TrimSpace(repo), "/")
	switch {
	case errors.Is(err, gitprovider.ErrNotFound):
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, fmt.Errorf("github app is not installed for %s or cannot access that repository; install the app for this repository or pin the correct installation id on the review profile: %w", repository, err))
	case errors.Is(err, gitprovider.ErrPermission):
		return gitprovider.WrapError(gitprovider.ErrPermission, op, fmt.Errorf("github app cannot access installation information for %s; check app installation access and permissions: %w", repository, err))
	default:
		return err
	}
}

func mapCreateInstallationTokenError(op gitprovider.Operation, installationID string, err error) error {
	switch {
	case errors.Is(err, gitprovider.ErrNotFound):
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, fmt.Errorf("github app installation %s was not found while creating an installation token: %w", installationID, err))
	case errors.Is(err, gitprovider.ErrPermission):
		return gitprovider.WrapError(gitprovider.ErrPermission, op, fmt.Errorf("github app installation %s cannot create an installation token with the requested access; check app installation access and permissions: %w", installationID, err))
	default:
		return err
	}
}

func (a *githubAppAuth) doAppREST(ctx context.Context, op gitprovider.Operation, method, endpoint, jwt string, in any, out any) error {
	var body *bytes.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	setHeaders(req, jwt, acceptJSON)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return mapTransportError(op, err)
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mapHTTPStatus(op, resp.StatusCode, responseBody)
	}
	if out != nil {
		if err := json.Unmarshal(responseBody, out); err != nil {
			return fmt.Errorf("%w: decode GitHub response: %w", ErrValidation, err)
		}
	}
	return nil
}

func (a *githubAppAuth) jwt() (string, error) {
	now := a.now()
	header := map[string]string{"typ": "JWT", "alg": "RS256"}
	payload := map[string]any{
		"iat": now.Add(-gitHubAppJWTBackdate).Unix(),
		"exp": now.Add(gitHubAppJWTLifetime).Unix(),
		"iss": a.issuer,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign github app jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func readRequiredCredential(store TokenStore, profile, key string) (string, error) {
	value, err := store.Get(profile, key)
	if err != nil {
		return "", gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("read github app credential %s/%s: %w", profile, key, err))
	}
	if strings.TrimSpace(value) == "" {
		return "", gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("github app credential %s/%s is required", profile, key))
	}
	return value, nil
}

func canResolveInstallation(installationID string, lookup *InstallationLookup) bool {
	if strings.TrimSpace(installationID) != "" {
		return true
	}
	return lookup != nil && strings.TrimSpace(lookup.Owner) != "" && strings.TrimSpace(lookup.Repo) != ""
}

func parseGitHubAppPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("parse github app private key: PEM block is required")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parse github app private key: RSA private key is required")
	}
	return key, nil
}

func identityFromInstallation(installation githubAppInstallation) gitprovider.Identity {
	slug := strings.TrimSpace(installation.AppSlug)
	if slug == "" {
		return gitprovider.Identity{}
	}
	return gitprovider.Identity{
		Login:       slug + "[bot]",
		DisplayName: slug,
	}
}
