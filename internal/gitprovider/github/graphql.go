package github

import (
	"encoding/json"
	"errors"
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
	err := errors.New("github graphql: provider returned an error")
	classification := strings.ToUpper(first.Type)
	if classification == "" && first.Extensions != nil {
		if value, ok := first.Extensions["type"].(string); ok {
			classification = strings.ToUpper(value)
		}
		if value, ok := first.Extensions["code"].(string); ok && classification == "" {
			classification = strings.ToUpper(value)
		}
	}
	message := strings.ToUpper(first.Message)
	switch {
	case strings.Contains(classification, "UNAUTHENTICATED"), strings.Contains(classification, "UNAUTHORIZED"), strings.Contains(message, "AUTHENTICATION"):
		return gitprovider.WrapError(gitprovider.ErrAuth, op, err)
	case strings.Contains(classification, "FORBIDDEN"), strings.Contains(message, "FORBIDDEN"):
		return gitprovider.WrapError(gitprovider.ErrPermission, op, err)
	case strings.Contains(classification, "NOT_FOUND"), strings.Contains(classification, "NOT FOUND"), strings.Contains(message, "NOT FOUND"):
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, err)
	case strings.Contains(classification, "RATE_LIMIT"), strings.Contains(classification, "RATE_LIMITED"), strings.Contains(message, "RATE LIMIT"):
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
	case strings.Contains(classification, "TIMEOUT"), strings.Contains(classification, "INTERNAL"), strings.Contains(classification, "SERVER"):
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
	default:
		return fmt.Errorf("%w: %w", ErrUnhandledGraphQL, err)
	}
}
