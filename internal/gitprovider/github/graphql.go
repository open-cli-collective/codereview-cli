package github

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors,omitempty"`
}

type graphQLError struct {
	Message    string         `json:"message"`
	Type       string         `json:"type,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

func mapGraphQLErrors(op gitprovider.Operation, gqlErrors []graphQLError) error {
	if len(gqlErrors) == 0 {
		return nil
	}
	first := gqlErrors[0]
	classification, message := graphQLErrorClassification(first)
	switch {
	case strings.Contains(classification, "UNAUTHENTICATED"), strings.Contains(classification, "UNAUTHORIZED"), strings.Contains(message, "AUTHENTICATION"):
		return gitprovider.WrapError(gitprovider.ErrAuth, op, graphQLErrorCause("authentication"))
	case strings.Contains(classification, "FORBIDDEN"), strings.Contains(message, "FORBIDDEN"):
		return gitprovider.WrapError(gitprovider.ErrPermission, op, graphQLErrorCause("permission"))
	case strings.Contains(classification, "NOT_FOUND"), strings.Contains(classification, "NOT FOUND"), strings.Contains(message, "NOT FOUND"):
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, graphQLErrorCause("not found"))
	case strings.Contains(classification, "RATE_LIMIT"), strings.Contains(classification, "RATE_LIMITED"), strings.Contains(message, "RATE LIMIT"):
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, graphQLErrorCause("rate limit"))
	case strings.Contains(classification, "TIMEOUT"), strings.Contains(classification, "INTERNAL"), strings.Contains(classification, "SERVER"):
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, graphQLErrorCause("retryable"))
	default:
		return fmt.Errorf("%w: %w", ErrUnhandledGraphQL, graphQLErrorCause(""))
	}
}

func graphQLErrorClassification(err graphQLError) (string, string) {
	classification := strings.ToUpper(strings.TrimSpace(err.Type))
	if classification == "" && err.Extensions != nil {
		if value, ok := err.Extensions["type"].(string); ok {
			classification = strings.ToUpper(strings.TrimSpace(value))
		}
		if value, ok := err.Extensions["code"].(string); ok && classification == "" {
			classification = strings.ToUpper(strings.TrimSpace(value))
		}
	}
	return classification, strings.ToUpper(strings.TrimSpace(err.Message))
}

func graphQLErrorCause(kind string) error {
	if kind == "" {
		return fmt.Errorf("github graphql: provider returned an error")
	}
	return fmt.Errorf("github graphql: provider returned a %s error", kind)
}
