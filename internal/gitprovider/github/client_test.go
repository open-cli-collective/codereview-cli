package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestClientImplementsReadProviderOnly(_ *testing.T) {
	var _ readProvider = (*Client)(nil)
}

type readProvider interface {
	WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error)
	GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error)
	GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error)
	GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error)
	ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error)
	ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error)
	ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error)
	ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error)
	Capabilities() gitprovider.ProviderCaps
}

func TestCapabilities(t *testing.T) {
	client := mustClient(t, Options{Token: "token"})
	caps := client.Capabilities()
	if !caps.NativeFileLevelComments || !caps.ThreadResolution {
		t.Fatalf("Capabilities() = %#v, want native file comments and thread resolution", caps)
	}
}

func TestNewFromGitConfigBuildsPATClientAndCredential(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	client, credential, err := NewFromGitConfig(config.GitConfig{
		Host:          "github.example.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work",
	}, store, Options{})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	if client.Host() != "github.example.com" {
		t.Fatalf("Host() = %q, want github.example.com", client.Host())
	}
	if credential.Type != credentialTypePAT || credential.Token != "token" {
		t.Fatalf("credential = %#v, want PAT token", credential)
	}
	if got := client.baseURL.String(); got != "https://github.example.com/api/v3/" {
		t.Fatalf("baseURL = %q, want enterprise REST mapping", got)
	}
	if got := client.graphQLURL.String(); got != "https://github.example.com/api/graphql" {
		t.Fatalf("graphQLURL = %q, want enterprise GraphQL mapping", got)
	}
}

func TestNewRequiresExplicitHost(t *testing.T) {
	_, err := New(Options{Token: "token"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("New without host error = %v, want ErrValidation", err)
	}
}

func TestNewFromGitConfigRejectsReservedAuthModesAndHostConflict(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	for _, mode := range []config.GitAuthMode{config.GitAuthModeOAuthDevice, config.GitAuthModeGitHubApp} {
		_, _, err := NewFromGitConfig(config.GitConfig{
			Host:          "github.com",
			AuthMode:      mode,
			CredentialRef: "codereview/work",
		}, store, Options{})
		if !errors.Is(err, config.ErrUnsupported) {
			t.Fatalf("NewFromGitConfig(%s) error = %v, want ErrUnsupported", mode, err)
		}
	}

	_, _, err := NewFromGitConfig(config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work",
	}, store, Options{Host: "github.example.com"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("NewFromGitConfig host conflict error = %v, want ErrValidation", err)
	}
}

func TestNewFromGitConfigRejectsMissingTokenWithoutLeaking(t *testing.T) {
	_, _, err := NewFromGitConfig(config.GitConfig{
		Host:          "github.com",
		AuthMode:      config.GitAuthModePAT,
		CredentialRef: "codereview/work",
	}, tokenStore{"work": {}}, Options{})
	if !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("NewFromGitConfig missing token error = %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "token-value") {
		t.Fatalf("error leaked token material: %v", err)
	}
}

func TestWhoAmIUsesSuppliedCredentialAndHeaders(t *testing.T) {
	var gotAuth, gotAccept, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		if r.URL.Path != "/user" {
			t.Fatalf("path = %q, want /user", r.URL.Path)
		}
		writeJSON(t, w, userResponse{Login: "rianjs", ID: 123, Name: "Rian"})
	}))
	defer server.Close()

	client := mustClient(t, Options{
		Token:      "client-token",
		BaseURL:    server.URL,
		GraphQLURL: server.URL + "/graphql",
	})
	identity, err := client.WhoAmI(context.Background(), gitprovider.Credential{Type: credentialTypePAT, Token: "supplied-token"})
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if identity != (gitprovider.Identity{Login: "rianjs", ID: "123", DisplayName: "Rian"}) {
		t.Fatalf("WhoAmI identity = %#v", identity)
	}
	if gotAuth != "Bearer supplied-token" {
		t.Fatalf("Authorization = %q, want supplied token", gotAuth)
	}
	if gotAccept != acceptJSON {
		t.Fatalf("Accept = %q, want %q", gotAccept, acceptJSON)
	}
	if gotVersion == "" {
		t.Fatal("X-GitHub-Api-Version header is empty")
	}
}

func TestWhoAmIRejectsInvalidCredentialBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "client-token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	if _, err := client.WhoAmI(context.Background(), gitprovider.Credential{Type: "oauth", Token: "secret-token"}); !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("WhoAmI unsupported credential error = %v, want ErrAuth", err)
	}
	if _, err := client.WhoAmI(context.Background(), gitprovider.Credential{Type: credentialTypePAT, Token: "  "}); !errors.Is(err, gitprovider.ErrAuth) {
		t.Fatalf("WhoAmI blank token error = %v, want ErrAuth", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid credentials", requests)
	}
}

func TestPRScopedReadsRejectHostMismatchBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "github.com", Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.GetPR(context.Background(), gitprovider.PRRef{Host: "github.example.com", Owner: "o", Repo: "r", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR host mismatch error = %v, want ErrValidation", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no request on host mismatch", requests)
	}
}

func TestPRScopedReadsRejectSlashInOwnerRepoBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "github.com", Token: "token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.GetPR(context.Background(), gitprovider.PRRef{Host: "github.com", Owner: "open/cli", Repo: "repo", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR slash owner error = %v, want ErrValidation", err)
	}
	_, err = client.GetPR(context.Background(), gitprovider.PRRef{Host: "github.com", Owner: "open-cli", Repo: "repo/name", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR slash repo error = %v, want ErrValidation", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid path segments", requests)
	}
}

type tokenStore map[string]map[string]string

func (s tokenStore) Get(profile, key string) (string, error) {
	keys, ok := s[profile]
	if !ok {
		return "", errors.New("missing profile")
	}
	value, ok := keys[key]
	if !ok {
		return "", errors.New("missing key")
	}
	return value, nil
}

func mustClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.Token == "" {
		opts.Token = "token"
	}
	if opts.Host == "" {
		opts.Host = defaultHost
	}
	client, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func testPRRef() gitprovider.PRRef {
	return gitprovider.PRRef{Host: "github.com", Owner: "open cli", Repo: "repo+name", Number: 42}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode response: %v", err)
	}
}

func readGraphQLRequest(t *testing.T, r *http.Request) graphQLRequest {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("Unmarshal GraphQL request %s: %v", body, err)
	}
	return req
}
