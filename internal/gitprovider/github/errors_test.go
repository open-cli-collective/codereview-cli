package github

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestTransportErrorsMapRetryableButContextCanceledDoesNot(t *testing.T) {
	canceledClient := mustClient(t, Options{
		Token:      "token",
		BaseURL:    "https://example.invalid/",
		GraphQLURL: "https://example.invalid/graphql",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		})},
	})
	_, err := canceledClient.WhoAmI(context.Background(), gitprovider.Credential{Type: credentialTypePAT, Token: "token"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context canceled error = %v, want context.Canceled", err)
	}
	if errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("context canceled error = %v, did not want ErrRetryable", err)
	}

	retryClient := mustClient(t, Options{
		Token:      "token",
		BaseURL:    "https://example.invalid/",
		GraphQLURL: "https://example.invalid/graphql",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})},
	})
	_, err = retryClient.WhoAmI(context.Background(), gitprovider.Credential{Type: credentialTypePAT, Token: "token"})
	if !errors.Is(err, gitprovider.ErrRetryable) {
		t.Fatalf("transport error = %v, want ErrRetryable", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
