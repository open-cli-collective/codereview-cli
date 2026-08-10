package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type approveRequest struct {
	SHA string `json:"sha"`
}

// PostIssueComment posts a merge-request note.
func (c *Client) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("%w: issue comment body is required", ErrValidation)
	}
	var response noteResponse
	if err := c.doRESTJSON(ctx, gitprovider.OperationPostIssueComment, http.MethodPost, c.notesURL(ref), noteRequest{Body: body}, &response); err != nil {
		return "", err
	}
	if response.ID <= 0 {
		return "", fmt.Errorf("%w: note ID missing from successful GitLab response", ErrValidation)
	}
	return gitprovider.CommentID(stringIDFromInt(response.ID)), nil
}

// SubmitReview posts the final review verdict. GitLab has no single review
// submission primitive, so the approval state changes first (approve for
// approve events, revoke for request-changes events) and the summary body is
// then posted as a merge-request note. Ordering matters for retries: the
// summary note carries the idempotency marker, so it must be the last write.
func (c *Client) SubmitReview(ctx context.Context, ref gitprovider.PRRef, request gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	op := gitprovider.OperationSubmitReview
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := request.Validate(); err != nil {
		return "", err
	}
	if len(request.Comments) > 0 {
		return "", fmt.Errorf("%w: GitLab does not support bundling inline comments into a review submission", ErrValidation)
	}
	mr, err := c.getMergeRequest(ctx, op, ref)
	if err != nil {
		return "", err
	}
	if err := requireCurrentHead(op, mr, request.CommitSHA); err != nil {
		return "", err
	}
	switch request.Event {
	case review.ReviewEventApprove:
		if err := c.approve(ctx, ref, request.CommitSHA); err != nil {
			return "", err
		}
	case review.ReviewEventRequestChanges:
		if err := c.unapprove(ctx, ref); err != nil {
			return "", err
		}
	case review.ReviewEventComment:
	default:
		return "", fmt.Errorf("%w: invalid review event %q", ErrValidation, request.Event)
	}
	var response noteResponse
	if err := c.doRESTJSON(ctx, op, http.MethodPost, c.notesURL(ref), noteRequest{Body: request.Body}, &response); err != nil {
		return "", err
	}
	if response.ID <= 0 {
		return "", fmt.Errorf("%w: note ID missing from successful GitLab response", ErrValidation)
	}
	return gitprovider.ReviewID(stringIDFromInt(response.ID)), nil
}

// approve applies the caller's approval to the merge request. GitLab responds
// 401 when the caller already holds a standing approval, indistinguishable by
// status from a credential failure, so a standing approval is detected first
// and treated as already satisfied.
func (c *Client) approve(ctx context.Context, ref gitprovider.PRRef, commitSHA string) error {
	op := gitprovider.OperationSubmitReview
	approved, err := c.currentUserApproved(ctx, op, ref)
	if err != nil {
		return err
	}
	if approved {
		return nil
	}
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "approve")
	err = c.doRESTJSON(ctx, op, http.MethodPost, endpoint, approveRequest{SHA: commitSHA}, nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitprovider.ErrConflict):
		// GitLab responds 409 when the supplied SHA no longer matches the
		// merge request head.
		return gitprovider.WrapError(gitprovider.ErrStaleSHA, op, err)
	case errors.Is(err, gitprovider.ErrAuth):
		// The approve endpoint responds 401 when the authenticated user is
		// not allowed to approve (for example self-approval restrictions),
		// which is a permission problem rather than a credential one.
		return gitprovider.WrapError(gitprovider.ErrPermission, op, err)
	default:
		return err
	}
}

// currentUserApproved reports whether the authenticated user already has a
// standing approval on the merge request. The current-user lookup only happens
// when the merge request has approvals at all, so the common fresh-approve
// path costs one extra read.
func (c *Client) currentUserApproved(ctx context.Context, op gitprovider.Operation, ref gitprovider.PRRef) (bool, error) {
	var approvals approvalsResponse
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "approvals")
	if _, _, err := c.doREST(ctx, op, http.MethodGet, endpoint, acceptJSON, &approvals); err != nil {
		return false, err
	}
	if len(approvals.ApprovedBy) == 0 {
		return false, nil
	}
	var user userResponse
	if _, _, err := c.doREST(ctx, op, http.MethodGet, restURL(c.baseURL, "user"), acceptJSON, &user); err != nil {
		return false, err
	}
	if user.ID <= 0 {
		// Without a usable current-user ID the standing approval cannot be
		// attributed; fall through to the approve attempt.
		return false, nil
	}
	for _, approval := range approvals.ApprovedBy {
		if approval.User.ID == user.ID {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) unapprove(ctx context.Context, ref gitprovider.PRRef) error {
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "unapprove")
	err := c.doRESTJSON(ctx, gitprovider.OperationSubmitReview, http.MethodPost, endpoint, nil, nil)
	// 404 means the posting identity had no active approval to revoke.
	if err != nil && !errors.Is(err, gitprovider.ErrNotFound) {
		return err
	}
	return nil
}

func (c *Client) notesURL(ref gitprovider.PRRef) string {
	return restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "notes")
}
