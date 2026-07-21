package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
)

var errTokenNotFound = errors.New("token not found")

func TestClientImplementsGitProvider(_ *testing.T) {
	var _ gitprovider.GitProvider = (*Client)(nil)
	var _ outbox.Provider = (*Client)(nil)
}

func TestCapabilities(t *testing.T) {
	client := mustClient(t, Options{Token: "token"})
	caps := client.Capabilities()
	if !caps.NativeFileLevelComments || !caps.ThreadResolution || caps.BundleInlineOnSubmit {
		t.Fatalf("Capabilities() = %#v, want native file comments and thread resolution without bundling", caps)
	}
	if !caps.ReviewSummaryAsComment {
		t.Fatalf("Capabilities() = %#v, want review summaries as comments", caps)
	}
	if caps.HeadRefNamespace != "merge-requests" {
		t.Fatalf("HeadRefNamespace = %q, want merge-requests", caps.HeadRefNamespace)
	}
}

func TestNewFromGitConfigBuildsPATClientAndCredential(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	client, credential, err := NewFromGitConfig(config.GitConfig{
		Provider:   config.GitProviderGitLab,
		Host:       "gitlab.example.com",
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
	}, store, Options{})
	if err != nil {
		t.Fatalf("NewFromGitConfig: %v", err)
	}
	if client.Host() != "gitlab.example.com" {
		t.Fatalf("Host() = %q, want gitlab.example.com", client.Host())
	}
	if credential.Type != credentialTypePAT || credential.Token != "token" {
		t.Fatalf("credential = %#v, want PAT token", credential)
	}
	if got := client.baseURL.String(); got != "https://gitlab.example.com/api/v4/" {
		t.Fatalf("baseURL = %q, want REST v4 mapping", got)
	}
}

func TestNewFromGitConfigRejectsNonPATAuthModes(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	for _, mode := range []config.GitAuthMode{config.GitAuthModeGitHubApp, config.GitAuthModeOAuthDevice} {
		_, _, err := NewFromGitConfig(config.GitConfig{
			Provider:   config.GitProviderGitLab,
			Host:       "gitlab.example.com",
			AuthMode:   mode,
			Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
		}, store, Options{})
		if !errors.Is(err, config.ErrUnsupported) {
			t.Fatalf("NewFromGitConfig(%q) error = %v, want ErrUnsupported", mode, err)
		}
	}
}

func TestNewFromGitConfigRejectsConflictingOptionsHost(t *testing.T) {
	store := tokenStore{"work": {credentials.GitTokenKey: "token"}}
	_, _, err := NewFromGitConfig(config.GitConfig{
		Host:       "gitlab.example.com",
		AuthMode:   config.GitAuthModePAT,
		Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/work"},
	}, store, Options{Host: "gitlab.other.com"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("NewFromGitConfig error = %v, want ErrValidation", err)
	}
}

func TestHTTPErrorTaxonomy(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusUnauthorized, want: gitprovider.ErrAuth},
		{status: http.StatusForbidden, want: gitprovider.ErrPermission},
		{status: http.StatusNotFound, want: gitprovider.ErrNotFound},
		{status: http.StatusConflict, want: gitprovider.ErrConflict},
		{status: http.StatusBadRequest, want: ErrValidation},
		{status: http.StatusUnprocessableEntity, want: ErrValidation},
		{status: http.StatusTooManyRequests, want: gitprovider.ErrRetryable},
		{status: http.StatusBadGateway, want: gitprovider.ErrRetryable},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
			_, _ = w.Write([]byte(`{"message":"secret detail"}`))
		}))
		client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
		_, err := client.GetPR(context.Background(), testPRRef())
		server.Close()
		if !errors.Is(err, tt.want) {
			t.Errorf("status %d error = %v, want %v", tt.status, err, tt.want)
		}
	}
}

func TestRESTHTTPErrorDoesNotLeakSecretBearingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token glpat-secret is invalid"}`))
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.GetPR(context.Background(), testPRRef())
	if err == nil || strings.Contains(err.Error(), "glpat-secret") {
		t.Fatalf("error = %v, want redacted body", err)
	}
}

func TestRESTPaginationRejectsOffHostNextLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://attacker.example.com/api/v4/steal>; rel="next"`)
		writeJSON(t, w, []noteResponse{})
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})
	_, err := client.ListIssueComments(context.Background(), testPRRef())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ListIssueComments error = %v, want ErrValidation for off-host pagination", err)
	}
}

func TestValidatePRRefRejectsHostMismatchAndDotSegments(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests++
	}))
	defer server.Close()
	client := mustClient(t, Options{Host: "gitlab.example.com", BaseURL: server.URL})

	_, err := client.GetPR(context.Background(), gitprovider.PRRef{Host: "gitlab.other.com", Owner: "group", Repo: "repo", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR host mismatch error = %v, want ErrValidation", err)
	}
	_, err = client.GetPR(context.Background(), gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group/../evil", Repo: "repo", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR dot owner error = %v, want ErrValidation", err)
	}
	_, err = client.GetPR(context.Background(), gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group", Repo: "re/po", Number: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("GetPR slashed repo error = %v, want ErrValidation", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests for invalid refs", requests)
	}
}

type tokenStore map[string]map[string]string

func (s tokenStore) Exists(profile, key string) (bool, error) {
	keys, ok := s[profile]
	if !ok {
		return false, nil
	}
	_, ok = keys[key]
	return ok, nil
}

func (s tokenStore) Get(profile, key string) (string, error) {
	keys, ok := s[profile]
	if !ok {
		return "", errTokenNotFound
	}
	value, ok := keys[key]
	if !ok {
		return "", errTokenNotFound
	}
	return value, nil
}

func mustClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.Token == "" {
		opts.Token = "token"
	}
	if opts.Host == "" {
		opts.Host = "gitlab.com"
	}
	client, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// testPRRef exercises URL-hostile characters and a nested namespace so path
// escaping stays covered by every wire test.
func testPRRef() gitprovider.PRRef {
	return gitprovider.PRRef{Host: "gitlab.example.com", Owner: "open cli/sub group", Repo: "repo+name", Number: 42}
}

// testProjectPath is testPRRef's project path as GitLab expects it encoded.
func testProjectPath() string {
	return url.PathEscape("open cli/sub group/repo+name")
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode response: %v", err)
	}
}
