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
}

type githubAppInstallation struct {
	ID      int64  `json:"id"`
	AppID   int64  `json:"app_id"`
	AppSlug string `json:"app_slug"`
}

type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
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
	issuer, err := readRequiredCredential(store, profile, credentials.GitHubAppIDKey)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	privateKeyPEM, err := readRequiredCredential(store, profile, credentials.GitHubAppPrivateKeyKey)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	privateKey, err := parseGitHubAppPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", err)
	}
	installationID, err := readOptionalCredential(store, profile, credentials.GitHubAppInstallationIDKey)
	if err != nil {
		return nil, gitprovider.Credential{}, err
	}
	if !canResolveInstallation(installationID, opts.InstallationLookup) {
		return nil, gitprovider.Credential{}, gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("github app credential %s/%s is required without repository context", profile, credentials.GitHubAppInstallationIDKey))
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
	token, expiresAt, err := a.createInstallationToken(ctx, op, jwt, installation.ID)
	if err != nil {
		return "", gitprovider.Identity{}, err
	}
	identity := identityFromInstallation(installation)
	a.installationID = strconv.FormatInt(installation.ID, 10)
	a.tokenValue = token
	a.expiresAt = expiresAt
	a.identity = identity
	return token, identity, nil
}

func (a *githubAppAuth) resolveInstallation(ctx context.Context, op gitprovider.Operation, jwt string) (githubAppInstallation, error) {
	if strings.TrimSpace(a.installationID) != "" {
		endpoint := restURL(a.baseURL, "app", "installations", a.installationID)
		var installation githubAppInstallation
		if err := a.doAppREST(ctx, op, http.MethodGet, endpoint, jwt, nil, &installation); err != nil {
			return githubAppInstallation{}, err
		}
		return installation, nil
	}
	endpoint := restURL(a.baseURL, "repos", a.lookup.Owner, a.lookup.Repo, "installation")
	var installation githubAppInstallation
	if err := a.doAppREST(ctx, op, http.MethodGet, endpoint, jwt, nil, &installation); err != nil {
		return githubAppInstallation{}, err
	}
	return installation, nil
}

func (a *githubAppAuth) createInstallationToken(ctx context.Context, op gitprovider.Operation, jwt string, installationID int64) (string, time.Time, error) {
	endpoint := restURL(a.baseURL, "app", "installations", strconv.FormatInt(installationID, 10), "access_tokens")
	var response installationTokenResponse
	if err := a.doAppREST(ctx, op, http.MethodPost, endpoint, jwt, map[string]any{}, &response); err != nil {
		return "", time.Time{}, err
	}
	if strings.TrimSpace(response.Token) == "" {
		return "", time.Time{}, gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("github app installation token is empty"))
	}
	if response.ExpiresAt.IsZero() {
		return "", time.Time{}, gitprovider.WrapError(gitprovider.ErrAuth, op, fmt.Errorf("github app installation token expiry is empty"))
	}
	return response.Token, response.ExpiresAt, nil
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

func readOptionalCredential(store TokenStore, profile, key string) (string, error) {
	exists, err := store.Exists(profile, key)
	if err != nil {
		return "", gitprovider.WrapError(gitprovider.ErrAuth, "", fmt.Errorf("check github app credential %s/%s: %w", profile, key, err))
	}
	if !exists {
		return "", nil
	}
	return readRequiredCredential(store, profile, key)
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
