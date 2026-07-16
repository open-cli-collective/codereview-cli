package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	positionTypeText = "text"
	positionTypeFile = "file"
)

type positionResponse struct {
	BaseSHA      string `json:"base_sha"`
	StartSHA     string `json:"start_sha"`
	HeadSHA      string `json:"head_sha"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	PositionType string `json:"position_type"`
	OldLine      *int   `json:"old_line"`
	NewLine      *int   `json:"new_line"`
}

type discussionResponse struct {
	ID    string         `json:"id"`
	Notes []noteResponse `json:"notes"`
}

type positionRequest struct {
	BaseSHA      string `json:"base_sha"`
	StartSHA     string `json:"start_sha"`
	HeadSHA      string `json:"head_sha"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	PositionType string `json:"position_type"`
	OldLine      int    `json:"old_line,omitempty"`
	NewLine      int    `json:"new_line,omitempty"`
}

type discussionRequest struct {
	Body     string           `json:"body"`
	Position *positionRequest `json:"position,omitempty"`
}

type noteRequest struct {
	Body string `json:"body"`
}

// ListInlineThreads returns merge-request discussions anchored to the diff.
func (c *Client) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	endpoint := withQuery(c.discussionsURL(ref), url.Values{"per_page": {"100"}})
	payloads, err := doRESTPages[discussionResponse](ctx, c, gitprovider.OperationListInlineThreads, endpoint)
	if err != nil {
		return nil, err
	}
	threads := make([]gitprovider.InlineThread, 0, len(payloads))
	for _, payload := range payloads {
		position := firstPosition(payload.Notes)
		if position == nil {
			continue
		}
		thread := gitprovider.InlineThread{
			ID:          gitprovider.ThreadID(payload.ID),
			Resolved:    discussionResolved(payload.Notes),
			Path:        positionPath(*position),
			Side:        positionSide(*position),
			Line:        positionLine(*position),
			SubjectType: positionSubjectType(*position),
			CommitSHA:   position.HeadSHA,
		}
		for _, note := range payload.Notes {
			if note.System {
				continue
			}
			thread.Comments = append(thread.Comments, gitprovider.ThreadComment{
				ID:          gitprovider.CommentID(stringIDFromInt(note.ID)),
				ThreadID:    thread.ID,
				Body:        note.Body,
				Author:      identityFromUser(note.Author),
				CommitSHA:   position.HeadSHA,
				Path:        thread.Path,
				Side:        thread.Side,
				Line:        thread.Line,
				SubjectType: thread.SubjectType,
				CreatedAt:   note.CreatedAt,
				UpdatedAt:   note.UpdatedAt,
			})
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

// PostInlineComment starts a new diff discussion on the merge request.
func (c *Client) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, comment gitprovider.InlineComment) (gitprovider.CommentID, error) {
	op := gitprovider.OperationPostInlineComment
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if err := comment.Validate(); err != nil {
		return "", err
	}
	mr, err := c.getMergeRequest(ctx, op, ref)
	if err != nil {
		return "", err
	}
	if err := requireCurrentHead(op, mr, comment.CommitSHA); err != nil {
		return "", err
	}
	position, err := c.buildPosition(ctx, op, ref, mr, comment)
	if err != nil {
		return "", err
	}
	var response discussionResponse
	if err := c.doRESTJSON(ctx, op, http.MethodPost, c.discussionsURL(ref), discussionRequest{Body: comment.Body, Position: position}, &response); err != nil {
		return "", err
	}
	if len(response.Notes) == 0 || response.Notes[0].ID <= 0 {
		return "", fmt.Errorf("%w: discussion note ID missing from successful GitLab response", ErrValidation)
	}
	return gitprovider.CommentID(stringIDFromInt(response.Notes[0].ID)), nil
}

// ReplyToThread adds a note to an existing merge-request discussion.
func (c *Client) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	op := gitprovider.OperationReplyToThread
	if err := c.validatePRRef(ref); err != nil {
		return "", err
	}
	if strings.TrimSpace(string(threadID)) == "" {
		return "", fmt.Errorf("%w: thread ID is required", ErrValidation)
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("%w: thread reply body is required", ErrValidation)
	}
	var response noteResponse
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "discussions", url.PathEscape(string(threadID)), "notes")
	if err := c.doRESTJSON(ctx, op, http.MethodPost, endpoint, noteRequest{Body: body}, &response); err != nil {
		return "", err
	}
	if response.ID <= 0 {
		return "", fmt.Errorf("%w: note ID missing from successful GitLab response", ErrValidation)
	}
	return gitprovider.CommentID(stringIDFromInt(response.ID)), nil
}

// ResolveThread marks a merge-request discussion resolved.
func (c *Client) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	op := gitprovider.OperationResolveThread
	if err := c.validatePRRef(ref); err != nil {
		return err
	}
	if strings.TrimSpace(string(threadID)) == "" {
		return fmt.Errorf("%w: thread ID is required", ErrValidation)
	}
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "discussions", url.PathEscape(string(threadID))), url.Values{"resolved": {"true"}})
	return c.doRESTJSON(ctx, op, http.MethodPut, endpoint, nil, nil)
}

func (c *Client) discussionsURL(ref gitprovider.PRRef) string {
	return restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "discussions")
}

// buildPosition maps a provider-neutral inline comment onto a GitLab diff-note
// position. Unchanged context lines require both old and new line numbers, so
// the counterpart is computed from the merge request's per-file diff.
func (c *Client) buildPosition(ctx context.Context, op gitprovider.Operation, ref gitprovider.PRRef, mr mergeRequestResponse, comment gitprovider.InlineComment) (*positionRequest, error) {
	position := &positionRequest{
		BaseSHA:  mr.DiffRefs.BaseSHA,
		StartSHA: mr.DiffRefs.StartSHA,
		HeadSHA:  mr.DiffRefs.HeadSHA,
		OldPath:  comment.Path,
		NewPath:  comment.Path,
	}
	files, err := c.listMergeRequestDiffs(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	file, ok := diffFileForPath(files, comment.Path, comment.Side)
	if ok {
		position.OldPath = file.OldPath
		position.NewPath = file.NewPath
		if position.OldPath == "" {
			position.OldPath = position.NewPath
		}
		if position.NewPath == "" {
			position.NewPath = position.OldPath
		}
	}
	if comment.SubjectType == review.AnchorKindFile {
		position.PositionType = positionTypeFile
		return position, nil
	}
	position.PositionType = positionTypeText
	switch comment.Side {
	case review.DiffSideRight:
		position.NewLine = comment.Line
		if ok {
			if anchor := anchorForNewLine(file.Diff, comment.Line); anchor.found && !anchor.changed {
				position.OldLine = anchor.counterpart
			}
		}
	case review.DiffSideLeft:
		position.OldLine = comment.Line
		if ok {
			if anchor := anchorForOldLine(file.Diff, comment.Line); anchor.found && !anchor.changed {
				position.NewLine = anchor.counterpart
			}
		}
	default:
		return nil, fmt.Errorf("%w: unsupported diff side %q", ErrValidation, comment.Side)
	}
	return position, nil
}

func diffFileForPath(files []diffFileResponse, path string, side review.DiffSide) (diffFileResponse, bool) {
	for _, file := range files {
		if side == review.DiffSideLeft && file.OldPath == path {
			return file, true
		}
		if file.NewPath == path || file.OldPath == path {
			return file, true
		}
	}
	return diffFileResponse{}, false
}

func requireCurrentHead(op gitprovider.Operation, mr mergeRequestResponse, commitSHA string) error {
	head := mr.DiffRefs.HeadSHA
	if head == "" {
		head = mr.SHA
	}
	if head == "" || strings.EqualFold(head, commitSHA) {
		return nil
	}
	return gitprovider.WrapError(gitprovider.ErrStaleSHA, op,
		fmt.Errorf("gitlab: commit %q is not the current merge request head %q", commitSHA, head))
}

func firstPosition(notes []noteResponse) *positionResponse {
	for _, note := range notes {
		if note.Position != nil {
			return note.Position
		}
	}
	return nil
}

func discussionResolved(notes []noteResponse) bool {
	resolvable := false
	for _, note := range notes {
		if !note.Resolvable {
			continue
		}
		resolvable = true
		if !note.Resolved {
			return false
		}
	}
	return resolvable
}

func positionPath(position positionResponse) string {
	if position.NewLine == nil && position.OldLine != nil && position.OldPath != "" {
		return position.OldPath
	}
	if position.NewPath != "" {
		return position.NewPath
	}
	return position.OldPath
}

func positionSide(position positionResponse) review.DiffSide {
	if position.PositionType == positionTypeFile {
		return ""
	}
	if position.NewLine == nil && position.OldLine != nil {
		return review.DiffSideLeft
	}
	return review.DiffSideRight
}

func positionLine(position positionResponse) int {
	if position.PositionType == positionTypeFile {
		return 0
	}
	if position.NewLine != nil {
		return *position.NewLine
	}
	if position.OldLine != nil {
		return *position.OldLine
	}
	return 0
}

func positionSubjectType(position positionResponse) review.AnchorKind {
	if position.PositionType == positionTypeFile {
		return review.AnchorKindFile
	}
	return review.AnchorKindLine
}
