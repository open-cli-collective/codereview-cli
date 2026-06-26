package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type pullCommentRequest struct {
	Body        string `json:"body"`
	CommitID    string `json:"commit_id"`
	Path        string `json:"path"`
	Side        string `json:"side,omitempty"`
	Line        int    `json:"line,omitempty"`
	SubjectType string `json:"subject_type,omitempty"`
}

type issueCommentRequest struct {
	Body string `json:"body"`
}

type reviewRequest struct {
	CommitID string `json:"commit_id"`
	Event    string `json:"event"`
	Body     string `json:"body"`
	Comments []reviewCommentRequest `json:"comments,omitempty"`
}

type reviewCommentRequest struct {
	Body string `json:"body"`
	Path string `json:"path"`
	Side string `json:"side,omitempty"`
	Line int    `json:"line,omitempty"`
}

type commentWriteResponse struct {
	ID int64 `json:"id"`
}

type reviewWriteResponse struct {
	ID int64 `json:"id"`
}

// PostInlineComment posts a line- or file-scoped pull request review comment.
func (c *Client) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, comment gitprovider.InlineComment) (gitprovider.CommentID, error) {
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := comment.Validate(); err != nil {
		return "", err
	}
	payload := pullCommentRequest{
		Body:     comment.Body,
		CommitID: comment.CommitSHA,
		Path:     comment.Path,
	}
	switch comment.SubjectType {
	case review.AnchorKindLine:
		payload.Side = string(comment.Side)
		payload.Line = comment.Line
	case review.AnchorKindFile:
		payload.SubjectType = "file"
	default:
		return "", fmt.Errorf("%w: unsupported inline comment subject type %q", ErrValidation, comment.SubjectType)
	}
	var response commentWriteResponse
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number), "comments")
	if err := c.doRESTJSON(ctx, gitprovider.OperationPostInlineComment, http.MethodPost, endpoint, payload, &response); err != nil {
		return "", err
	}
	id, err := requireWriteID("pull request comment ID", response.ID)
	if err != nil {
		return "", err
	}
	return gitprovider.CommentID(id), nil
}

// PostIssueComment posts a pull-request issue comment.
func (c *Client) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := requireWriteValue("issue comment body", body); err != nil {
		return "", err
	}
	var response commentWriteResponse
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "issues", fmt.Sprint(ref.Number), "comments")
	if err := c.doRESTJSON(ctx, gitprovider.OperationPostIssueComment, http.MethodPost, endpoint, issueCommentRequest{Body: body}, &response); err != nil {
		return "", err
	}
	id, err := requireWriteID("issue comment ID", response.ID)
	if err != nil {
		return "", err
	}
	return gitprovider.CommentID(id), nil
}

// SubmitReview submits the final pull-request review event.
func (c *Client) SubmitReview(ctx context.Context, ref gitprovider.PRRef, request gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := request.Validate(); err != nil {
		return "", err
	}
	event, err := reviewEvent(request.Event)
	if err != nil {
		return "", err
	}
	var response reviewWriteResponse
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number), "reviews")
	payload := reviewRequest{CommitID: request.CommitSHA, Event: event, Body: request.Body}
	if len(request.Comments) > 0 {
		payload.Comments = make([]reviewCommentRequest, 0, len(request.Comments))
		for _, comment := range request.Comments {
			entry := reviewCommentRequest{
				Body: comment.Body,
				Path: comment.Path,
			}
			switch comment.SubjectType {
			case review.AnchorKindLine:
				entry.Side = string(comment.Side)
				entry.Line = comment.Line
			default:
				return "", fmt.Errorf("%w: unsupported bundled review comment subject type %q", ErrValidation, comment.SubjectType)
			}
			payload.Comments = append(payload.Comments, entry)
		}
	}
	if err := c.doRESTJSON(ctx, gitprovider.OperationSubmitReview, http.MethodPost, endpoint, payload, &response); err != nil {
		return "", err
	}
	id, err := requireWriteID("review ID", response.ID)
	if err != nil {
		return "", err
	}
	return gitprovider.ReviewID(id), nil
}

func reviewEvent(event review.ReviewEvent) (string, error) {
	switch event {
	case review.ReviewEventApprove:
		return "APPROVE", nil
	case review.ReviewEventComment:
		return "COMMENT", nil
	case review.ReviewEventRequestChanges:
		return "REQUEST_CHANGES", nil
	default:
		return "", fmt.Errorf("%w: invalid review event %q", ErrValidation, event)
	}
}

func requireWriteValue(name string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrValidation, name)
	}
	return nil
}

func requireWriteID(name string, id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("%w: %s missing from successful GitHub response", ErrValidation, name)
	}
	return stringIDFromInt(id), nil
}
