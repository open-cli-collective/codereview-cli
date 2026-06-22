package github

import (
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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestNewFromGitConfigBuildsGitHubAppClientAndRefreshesToken(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	privateKey, privateKeyPEM := testPrivateKey(t)
	tokenRequests := 0
	var apiAuths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			assertJWTAuth(t, r, "12345", &privateKey.PublicKey)
			writeJSON(t, w, map[string]any{
				"id":       42,
				"app_id":   12345,
				"app_slug": "cr-reviewer",
			})
		case "/app/installations/42/access_tokens":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			assertJWTAuth(t, r, "12345", &privateKey.PublicKey)
			tokenRequests++
			writeJSON(t, w, map[string]any{
				"token":      fmt.Sprintf("installation-token-%d", tokenRequests),
				"expires_at": now.Add(time.Hour).Format(time.RFC3339),
			})
		case "/repos/open-cli/codereview-cli/pulls/76":
			apiAuths = append(apiAuths, r.Header.Get("Authorization"))
			writeJSON(t, w, prResponse{
				Title: "GitHub App auth",
				State: "open",
				User:  userResponse{Login: "author"},
				Head:  branchResponse{Ref: "feature", SHA: "head-sha"},
				Base:  branchResponse{Ref: "main", SHA: "base-sha"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client, credential, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", privateKeyPEM), Options{
		BaseURL:        server.URL,
		GraphQLURL:     server.URL + "/graphql",
		Now:            func() time.Time { return now },
		InstallationID: "42",
	})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	if credential.Type != credentialTypeGitHubApp || credential.Token != "installation-token-1" || credential.Login != "cr-reviewer[bot]" {
		t.Fatalf("credential = %#v, want github app token and bot login", credential)
	}
	identity, err := client.WhoAmI(context.Background(), credential)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if identity != (gitprovider.Identity{Login: "cr-reviewer[bot]", DisplayName: "cr-reviewer"}) {
		t.Fatalf("WhoAmI identity = %#v", identity)
	}

	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 76}
	if _, err := client.GetPR(context.Background(), ref); err != nil {
		t.Fatalf("GetPR first: %v", err)
	}
	now = now.Add(59 * time.Minute)
	if _, err := client.GetPR(context.Background(), ref); err != nil {
		t.Fatalf("GetPR refresh: %v", err)
	}
	wantAuths := []string{"Bearer installation-token-1", "Bearer installation-token-2"}
	if fmt.Sprint(apiAuths) != fmt.Sprint(wantAuths) {
		t.Fatalf("API auths = %#v, want %#v", apiAuths, wantAuths)
	}
	if tokenRequests != 2 {
		t.Fatalf("tokenRequests = %d, want refresh before second API call", tokenRequests)
	}
}

func TestNewFromGitConfigUsesRepositoryInstallationLookup(t *testing.T) {
	const repoInstallationToken = "repo-installation-token" // #nosec G101 -- distinctive test canary, not a real token.
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v3/repos/open-cli/codereview-cli/installation":
			assertJWTAuth(t, r, "app-client-id")
			writeJSON(t, w, map[string]any{
				"id":       77,
				"app_id":   12345,
				"app_slug": "cr-reviewer",
			})
		case "/api/v3/app/installations/77/access_tokens":
			assertJWTAuth(t, r, "app-client-id")
			writeJSON(t, w, map[string]any{
				"token":      repoInstallationToken,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	_, credential, err := NewFromGitConfig(githubAppGitConfigWithAppID("codereview/app", "app-client-id"), githubAppStore(t, "app", "app-client-id", privateKey), Options{
		BaseURL:    server.URL + "/api/v3",
		GraphQLURL: server.URL + "/api/graphql",
		InstallationLookup: &InstallationLookup{
			Owner: "open-cli",
			Repo:  "codereview-cli",
		},
	})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	if credential.Token != repoInstallationToken || credential.Login != "cr-reviewer[bot]" {
		t.Fatalf("credential = %#v, want repo lookup app token", credential)
	}
}

func TestNewFromGitConfigExplainsMissingRepositoryInstallation(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/open-cli/codereview-cli/installation":
			assertJWTAuth(t, r, "app-client-id")
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	_, _, err := NewFromGitConfig(githubAppGitConfigWithAppID("codereview/app", "app-client-id"), githubAppStore(t, "app", "app-client-id", privateKey), Options{
		BaseURL:    server.URL,
		GraphQLURL: server.URL + "/graphql",
		InstallationLookup: &InstallationLookup{
			Owner: "open-cli",
			Repo:  "codereview-cli",
		},
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("NewFromGitConfig error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "github app is not installed for open-cli/codereview-cli or cannot access that repository") {
		t.Fatalf("NewFromGitConfig error = %v, want repository installation detail", err)
	}
}

func TestNewFromGitConfigExplainsMissingPinnedInstallation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			assertJWTAuth(t, r, "12345")
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	_, _, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", testPrivateKeyPEM(t)), Options{
		BaseURL:        server.URL,
		GraphQLURL:     server.URL + "/graphql",
		InstallationID: "42",
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("NewFromGitConfig error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "github app installation 42 was not found for this app") {
		t.Fatalf("NewFromGitConfig error = %v, want pinned installation detail", err)
	}
}

func TestGitHubAppClientUsesInstallationTokenForRESTWritesAndGraphQL(t *testing.T) {
	var writeAuth, graphQLAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			assertJWTAuth(t, r, "12345")
			writeJSON(t, w, map[string]any{"id": 42, "app_id": 12345, "app_slug": "cr-reviewer"})
		case "/app/installations/42/access_tokens":
			assertJWTAuth(t, r, "12345")
			writeJSON(t, w, map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case "/repos/open-cli/codereview-cli/issues/76/comments":
			writeAuth = r.Header.Get("Authorization")
			writeJSON(t, w, commentWriteResponse{ID: 1001})
		case "/graphql":
			graphQLAuth = r.Header.Get("Authorization")
			writeJSON(t, w, map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
								"nodes":    []any{},
							},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client, _, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", testPrivateKeyPEM(t)), Options{
		BaseURL:        server.URL,
		GraphQLURL:     server.URL + "/graphql",
		InstallationID: "42",
	})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "codereview-cli", Number: 76}
	if _, err := client.PostIssueComment(context.Background(), ref, "review body"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if _, err := client.ListInlineThreads(context.Background(), ref); err != nil {
		t.Fatalf("ListInlineThreads: %v", err)
	}
	if writeAuth != "Bearer installation-token" || graphQLAuth != "Bearer installation-token" {
		t.Fatalf("auths = write:%q graphql:%q, want installation token", writeAuth, graphQLAuth)
	}
}

func TestNewFromGitConfigRequiresInstallationIDWithoutLookup(t *testing.T) {
	_, _, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", testPrivateKeyPEM(t)), Options{})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("NewFromGitConfig error = %v, want ErrAuth", err)
	}
	if !strings.Contains(err.Error(), "installation discovery requires repository context") {
		t.Fatalf("NewFromGitConfig error = %v, want repository-context detail", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("error leaked private key material: %v", err)
	}
}

func TestNewFromGitConfigRequiresGitHubAppIdentity(t *testing.T) {
	const installationToken = "installation-token" // #nosec G101 -- distinctive test canary, not a real token.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/app/installations/42":
			assertJWTAuth(t, r, "12345")
			writeJSON(t, w, map[string]any{"id": 42, "app_id": 12345})
		case "/app/installations/42/access_tokens":
			assertJWTAuth(t, r, "12345")
			writeJSON(t, w, map[string]any{
				"token":      installationToken,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	_, _, err := NewFromGitConfig(githubAppGitConfig("codereview/app"), githubAppStore(t, "app", "12345", testPrivateKeyPEM(t)), Options{
		BaseURL:        server.URL,
		GraphQLURL:     server.URL + "/graphql",
		InstallationID: "42",
	})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("NewFromGitConfig error = %v, want ErrAuth", err)
	}
	if !strings.Contains(err.Error(), "github app identity") {
		t.Fatalf("NewFromGitConfig error = %v, want github app identity detail", err)
	}
}

func githubAppGitConfig(ref string) config.GitConfig {
	return githubAppGitConfigWithAppID(ref, "12345")
}

func githubAppGitConfigWithAppID(ref string, appID string) config.GitConfig {
	return config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModeGitHubApp,
		GitHubApp:     &config.GitHubAppConfig{AppID: appID},
		CredentialRef: ref,
	}
}

func githubAppStore(_ *testing.T, profile, appID, privateKey string) tokenStore {
	values := map[string]string{
		credentials.GitHubAppPrivateKeyKey: privateKey,
	}
	return tokenStore{profile: values}
}

func testPrivateKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, string(pem.EncodeToMemory(block))
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, pem := testPrivateKey(t)
	return pem
}

func assertJWTAuth(t *testing.T, r *http.Request, wantIssuer string, publicKeys ...*rsa.PublicKey) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("Authorization = %q, want Bearer JWT", auth)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	headerPayload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode JWT header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerPayload, &header); err != nil {
		t.Fatalf("unmarshal JWT header: %v", err)
	}
	if header["alg"] != "RS256" {
		t.Fatalf("JWT alg = %q, want RS256", header["alg"])
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var payload struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal JWT payload: %v", err)
	}
	if payload.Iss != wantIssuer {
		t.Fatalf("JWT issuer = %q, want %q", payload.Iss, wantIssuer)
	}
	if payload.Iat == 0 || payload.Exp == 0 || payload.Exp-payload.Iat > 10*60 {
		t.Fatalf("JWT times = iat:%d exp:%d, want <=10m window", payload.Iat, payload.Exp)
	}
	if len(publicKeys) == 0 || publicKeys[0] == nil {
		return
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKeys[0], crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify JWT signature: %v", err)
	}
}
