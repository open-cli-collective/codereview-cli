package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const addThreadReplyMutation = `
mutation($threadID: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $threadID, body: $body}) {
    comment {
      id
      databaseId
    }
  }
}`

const resolveThreadMutation = `
mutation($threadID: ID!) {
  resolveReviewThread(input: {threadId: $threadID}) {
    thread {
      id
      isResolved
    }
  }
}`

type addThreadReplyData struct {
	AddPullRequestReviewThreadReply struct {
		Comment struct {
			ID         string `json:"id"`
			DatabaseID int64  `json:"databaseId"`
		} `json:"comment"`
	} `json:"addPullRequestReviewThreadReply"`
}

// ReplyToThread posts a reply to an existing pull request review thread.
func (c *Client) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := requireWriteValue("thread ID", string(threadID)); err != nil {
		return "", err
	}
	if err := requireWriteValue("thread reply body", body); err != nil {
		return "", err
	}
	var data addThreadReplyData
	err := c.doGraphQL(ctx, gitprovider.OperationReplyToThread, addThreadReplyMutation, map[string]any{
		"threadID": string(threadID),
		"body":     body,
	}, &data)
	if err != nil {
		return "", err
	}
	id := data.AddPullRequestReviewThreadReply.Comment.ID
	if strings.TrimSpace(id) == "" {
		var idErr error
		// GitProvider IDs are provider-owned strings; GitHub databaseId matches
		// the REST numeric ID form already used for review and issue comments.
		id, idErr = requireWriteID("thread reply comment ID", data.AddPullRequestReviewThreadReply.Comment.DatabaseID)
		if idErr != nil {
			return "", idErr
		}
	}
	return gitprovider.CommentID(id), nil
}

// ResolveThread resolves an existing pull request review thread.
func (c *Client) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	if err := c.validatePRRef(ref); err != nil {
		return err
	}
	if err := requireWriteValue("thread ID", string(threadID)); err != nil {
		return err
	}
	return c.doGraphQLWithErrorMapper(ctx, gitprovider.OperationResolveThread, resolveThreadMutation, map[string]any{
		"threadID": string(threadID),
	}, nil, mapResolveThreadGraphQLErrors)
}

func mapResolveThreadGraphQLErrors(op gitprovider.Operation, gqlErrors []graphQLError) error {
	if isAlreadyResolvedGraphQLError(gqlErrors) {
		return nil
	}
	if isGitHubAppThreadResolutionUnsupportedGraphQLError(gqlErrors) {
		return gitprovider.WrapError(gitprovider.ErrThreadResolutionUnsupported, op,
			fmt.Errorf("github graphql: GitHub App integrations cannot resolve review threads (resource not accessible by integration)"))
	}
	return mapGraphQLErrors(op, gqlErrors)
}

func isAlreadyResolvedGraphQLError(gqlErrors []graphQLError) bool {
	if len(gqlErrors) != 1 {
		return false
	}
	err := gqlErrors[0]
	return strings.EqualFold(strings.TrimSpace(err.Type), "UNPROCESSABLE") &&
		strings.EqualFold(strings.TrimSpace(err.Message), "Review thread is already resolved")
}

func isGitHubAppThreadResolutionUnsupportedGraphQLError(gqlErrors []graphQLError) bool {
	if len(gqlErrors) != 1 {
		return false
	}
	err := gqlErrors[0]
	classification := strings.ToUpper(strings.TrimSpace(err.Type))
	if classification == "" && err.Extensions != nil {
		if value, ok := err.Extensions["type"].(string); ok {
			classification = strings.ToUpper(strings.TrimSpace(value))
		}
		if value, ok := err.Extensions["code"].(string); ok && classification == "" {
			classification = strings.ToUpper(strings.TrimSpace(value))
		}
	}
	message := strings.ToUpper(strings.TrimSpace(err.Message))
	return strings.Contains(classification, "FORBIDDEN") &&
		strings.Contains(message, "RESOURCE NOT ACCESSIBLE BY INTEGRATION")
}
