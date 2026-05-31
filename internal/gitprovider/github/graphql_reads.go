package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const treeQuery = `
query($owner: String!, $repo: String!, $expression: String!) {
  repository(owner: $owner, name: $repo) {
    object(expression: $expression) {
      ... on Tree {
        entries {
          name
          path
          type
          oid
        }
      }
    }
  }
}`

const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $threadAfter: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $threadAfter) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          path
          line
          diffSide
          comments(first: 100) {
            pageInfo {
              hasNextPage
              endCursor
            }
            nodes {
              id
              databaseId
              body
              author {
                login
                ... on User {
                  id: databaseId
                  name
                }
              }
              commit {
                oid
              }
              path
              line
              diffSide
              url
              createdAt
              updatedAt
            }
          }
        }
      }
    }
  }
}`

const threadCommentsQuery = `
query($threadID: ID!, $commentAfter: String) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $commentAfter) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          databaseId
          body
          author {
            login
            ... on User {
              id: databaseId
              name
            }
          }
          commit {
            oid
          }
          path
          line
          diffSide
          url
          createdAt
          updatedAt
        }
      }
    }
  }
}`

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type treeData struct {
	Repository struct {
		Object *struct {
			Entries []struct {
				Name string `json:"name"`
				Path string `json:"path"`
				Type string `json:"type"`
				OID  string `json:"oid"`
			} `json:"entries"`
		} `json:"object"`
	} `json:"repository"`
}

type threadsData struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads threadConnection `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type threadCommentsData struct {
	Node struct {
		Comments commentConnection `json:"comments"`
	} `json:"node"`
}

type threadConnection struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []threadNode `json:"nodes"`
}

type commentConnection struct {
	PageInfo pageInfo      `json:"pageInfo"`
	Nodes    []commentNode `json:"nodes"`
}

type threadNode struct {
	ID         string            `json:"id"`
	IsResolved bool              `json:"isResolved"`
	Path       string            `json:"path"`
	Line       int               `json:"line"`
	DiffSide   string            `json:"diffSide"`
	Comments   commentConnection `json:"comments"`
}

type commentNode struct {
	ID         string       `json:"id"`
	DatabaseID int64        `json:"databaseId"`
	Body       string       `json:"body"`
	Author     userResponse `json:"author"`
	Commit     struct {
		OID string `json:"oid"`
	} `json:"commit"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	DiffSide  string    `json:"diffSide"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ListTreeAtRef returns tree entries for a path at a git ref.
func (c *Client) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, treePath string) ([]gitprovider.TreeEntry, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitRef) == "" {
		return nil, fmt.Errorf("%w: git ref is required", ErrValidation)
	}
	expression := gitRef
	if strings.TrimSpace(treePath) != "" {
		expression += ":" + treePath
	}
	var data treeData
	err := c.doGraphQL(ctx, gitprovider.OperationListTreeAtRef, treeQuery, map[string]any{
		"owner":      ref.Owner,
		"repo":       ref.Repo,
		"expression": expression,
	}, &data)
	if err != nil {
		return nil, err
	}
	if data.Repository.Object == nil {
		return nil, gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationListTreeAtRef, fmt.Errorf("tree %q not found", expression))
	}
	entries := make([]gitprovider.TreeEntry, 0, len(data.Repository.Object.Entries))
	for _, entry := range data.Repository.Object.Entries {
		entryType, err := normalizeTreeEntryType(entry.Type)
		if err != nil {
			return nil, err
		}
		entryPath := entry.Path
		if entryPath == "" {
			entryPath = entry.Name
		}
		entries = append(entries, gitprovider.TreeEntry{
			Path: entryPath,
			Type: entryType,
			SHA:  entry.OID,
		})
	}
	return entries, nil
}

// ListInlineThreads returns inline review threads and their comments.
func (c *Client) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	var threads []gitprovider.InlineThread
	var after any
	for {
		var data threadsData
		err := c.doGraphQL(ctx, gitprovider.OperationListInlineThreads, reviewThreadsQuery, map[string]any{
			"owner":       ref.Owner,
			"repo":        ref.Repo,
			"number":      ref.Number,
			"threadAfter": after,
		}, &data)
		if err != nil {
			return nil, err
		}
		for _, node := range data.Repository.PullRequest.ReviewThreads.Nodes {
			comments := make([]gitprovider.ThreadComment, 0, len(node.Comments.Nodes))
			for _, comment := range node.Comments.Nodes {
				mapped, err := mapThreadComment(node.ID, comment)
				if err != nil {
					return nil, err
				}
				comments = append(comments, mapped)
			}
			if node.Comments.PageInfo.HasNextPage {
				more, err := c.fetchThreadComments(ctx, node.ID, node.Comments.PageInfo.EndCursor)
				if err != nil {
					return nil, err
				}
				comments = append(comments, more...)
			}
			side, err := parseDiffSide(node.DiffSide)
			if err != nil {
				return nil, err
			}
			threads = append(threads, gitprovider.InlineThread{
				ID:          gitprovider.ThreadID(node.ID),
				Resolved:    node.IsResolved,
				Path:        node.Path,
				Side:        side,
				Line:        node.Line,
				SubjectType: anchorKind(node.Line),
				CommitSHA:   firstCommitSHA(comments),
				Comments:    comments,
			})
		}
		if !data.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage {
			return threads, nil
		}
		after = data.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
	}
}

func (c *Client) fetchThreadComments(ctx context.Context, threadID string, after string) ([]gitprovider.ThreadComment, error) {
	var comments []gitprovider.ThreadComment
	cursor := any(after)
	for {
		var data threadCommentsData
		err := c.doGraphQL(ctx, gitprovider.OperationListInlineThreads, threadCommentsQuery, map[string]any{
			"threadID":     threadID,
			"commentAfter": cursor,
		}, &data)
		if err != nil {
			return nil, err
		}
		for _, comment := range data.Node.Comments.Nodes {
			mapped, err := mapThreadComment(threadID, comment)
			if err != nil {
				return nil, err
			}
			comments = append(comments, mapped)
		}
		if !data.Node.Comments.PageInfo.HasNextPage {
			return comments, nil
		}
		cursor = data.Node.Comments.PageInfo.EndCursor
	}
}

func mapThreadComment(threadID string, comment commentNode) (gitprovider.ThreadComment, error) {
	id := comment.ID
	if id == "" {
		id = stringIDFromInt(comment.DatabaseID)
	}
	side, err := parseDiffSide(comment.DiffSide)
	if err != nil {
		return gitprovider.ThreadComment{}, err
	}
	return gitprovider.ThreadComment{
		ID:          gitprovider.CommentID(id),
		ThreadID:    gitprovider.ThreadID(threadID),
		Body:        comment.Body,
		Author:      identityFromUser(comment.Author),
		CommitSHA:   comment.Commit.OID,
		Path:        comment.Path,
		Side:        side,
		Line:        comment.Line,
		SubjectType: anchorKind(comment.Line),
		URL:         comment.URL,
		CreatedAt:   comment.CreatedAt,
		UpdatedAt:   comment.UpdatedAt,
	}, nil
}

func normalizeTreeEntryType(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "blob":
		return "blob", nil
	case "tree":
		return "tree", nil
	case "commit":
		return "commit", nil
	default:
		return "", fmt.Errorf("%w: unknown tree entry type %q", ErrValidation, value)
	}
}

func parseDiffSide(value string) (review.DiffSide, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	side, err := review.ParseDiffSide(value)
	if err != nil {
		return "", fmt.Errorf("%w: unknown diff side %q", ErrValidation, value)
	}
	return side, nil
}

func anchorKind(line int) review.AnchorKind {
	if line > 0 {
		return review.AnchorKindLine
	}
	return review.AnchorKindFile
}

func firstCommitSHA(comments []gitprovider.ThreadComment) string {
	for _, comment := range comments {
		if comment.CommitSHA != "" {
			return comment.CommitSHA
		}
	}
	return ""
}
