package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestRepositoryInvitationsAreUnsupportedWithoutRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()
	client := mustClient(t, Options{BaseURL: server.URL})

	if _, err := client.ListRepositoryInvitations(context.Background()); !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("ListRepositoryInvitations error = %v, want config.ErrUnsupported", err)
	}
	if err := client.AcceptRepositoryInvitation(context.Background(), "1"); !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("AcceptRepositoryInvitation error = %v, want config.ErrUnsupported", err)
	}
}
