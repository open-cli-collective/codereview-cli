package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestListRepositoryInvitationsMapsPagesAndUsesUserToken(t *testing.T) {
	page1 := readInvitationFixture(t, "repository_invitations_page1.json")
	page2 := readInvitationFixture(t, "repository_invitations_page2.json")
	var requests []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/user/repository_invitations" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
			t.Fatalf("Authorization = %q, want user token", got)
		}
		if got := r.Header.Get("Accept"); got != acceptJSON {
			t.Fatalf("Accept = %q, want %q", got, acceptJSON)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write(page2)
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Fatalf("per_page = %q, want 100", got)
		}
		w.Header().Set("Link", `<`+server.URL+`/user/repository_invitations?per_page=100&page=2>; rel="next"`)
		_, _ = w.Write(page1)
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "user-token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	invitations, err := client.ListRepositoryInvitations(context.Background())
	if err != nil {
		t.Fatalf("ListRepositoryInvitations: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want two pages", requests)
	}
	want := []gitprovider.RepositoryInvitation{
		{
			ID:         "301001",
			Owner:      "example-org",
			Repo:       "service",
			Inviter:    gitprovider.Identity{Login: "example-owner", ID: "9002"},
			Invitee:    gitprovider.Identity{Login: "review-bot", ID: "9100"},
			Permission: "write",
			CreatedAt:  time.Date(2026, 10, 30, 14, 52, 50, 0, time.UTC),
			URL:        "https://github.com/example-org/service/invitations",
		},
		{
			ID:         "301002",
			Owner:      "other-org",
			Repo:       "tooling",
			Inviter:    gitprovider.Identity{Login: "other-owner", ID: "9004"},
			Invitee:    gitprovider.Identity{Login: "review-bot", ID: "9100"},
			Permission: "read",
			CreatedAt:  time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
			Expired:    true,
			URL:        "https://github.com/other-org/tooling/invitations",
		},
		{
			ID:         "301003",
			Owner:      "example-org",
			Repo:       "docs",
			Invitee:    gitprovider.Identity{Login: "review-bot", ID: "9100"},
			Permission: "maintain",
			CreatedAt:  time.Date(2026, 10, 31, 9, 15, 0, 0, time.UTC),
			URL:        "https://github.com/example-org/docs/invitations",
		},
	}
	if len(invitations) != len(want) {
		t.Fatalf("invitations = %#v, want %d", invitations, len(want))
	}
	for i := range want {
		got := invitations[i]
		if !got.CreatedAt.Equal(want[i].CreatedAt) {
			t.Fatalf("invitation %d CreatedAt = %s, want %s", i, got.CreatedAt, want[i].CreatedAt)
		}
		got.CreatedAt = want[i].CreatedAt
		if got != want[i] {
			t.Fatalf("invitation %d = %#v, want %#v", i, got, want[i])
		}
	}
}

func TestListRepositoryInvitationsRejectsInvitationWithoutRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]any{{"id": 5, "repository": map[string]any{"name": "service"}}})
	}))
	defer server.Close()
	client := mustClient(t, Options{BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	_, err := client.ListRepositoryInvitations(context.Background())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ListRepositoryInvitations error = %v, want ErrValidation", err)
	}
}

func TestAcceptRepositoryInvitationPatchesWithoutBody(t *testing.T) {
	var method, path, auth string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := mustClient(t, Options{Token: "user-token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

	if err := client.AcceptRepositoryInvitation(context.Background(), "301001"); err != nil {
		t.Fatalf("AcceptRepositoryInvitation: %v", err)
	}
	if method != http.MethodPatch || path != "/user/repository_invitations/301001" || auth != "Bearer user-token" || len(body) != 0 {
		t.Fatalf("request = %s %s auth=%q body=%q, want bodiless PATCH with user token", method, path, auth, body)
	}
}

func TestAcceptRepositoryInvitationMapsHostFailures(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusNotFound, want: gitprovider.ErrNotFound},
		{status: http.StatusForbidden, want: gitprovider.ErrPermission},
		{status: http.StatusUnauthorized, want: gitprovider.ErrAuth},
		{status: http.StatusConflict, want: gitprovider.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"message":"echoed user-token"}`))
			}))
			defer server.Close()
			client := mustClient(t, Options{Token: "user-token", BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})

			err := client.AcceptRepositoryInvitation(context.Background(), "301001")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), "user-token") {
				t.Fatalf("error leaked token material: %v", err)
			}
		})
	}
}

func TestAcceptRepositoryInvitationRejectsNonNumericIDBeforeRequest(t *testing.T) {
	client := mustClient(t, Options{BaseURL: "http://127.0.0.1:1", GraphQLURL: "http://127.0.0.1:1/graphql"})
	for _, id := range []gitprovider.InvitationID{"", "abc", "../1", "-4", "0"} {
		if err := client.AcceptRepositoryInvitation(context.Background(), id); !errors.Is(err, ErrValidation) {
			t.Fatalf("AcceptRepositoryInvitation(%q) error = %v, want ErrValidation", id, err)
		}
	}
}

func TestRepositoryInvitationsRejectGitHubAppClients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()
	client := mustClient(t, Options{BaseURL: server.URL, GraphQLURL: server.URL + "/graphql"})
	client.appAuth = &githubAppAuth{}

	if _, err := client.ListRepositoryInvitations(context.Background()); !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("ListRepositoryInvitations error = %v, want config.ErrUnsupported", err)
	}
	if err := client.AcceptRepositoryInvitation(context.Background(), "1"); !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("AcceptRepositoryInvitation error = %v, want config.ErrUnsupported", err)
	}
}

func readInvitationFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- fixed fixture names under testdata.
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}
