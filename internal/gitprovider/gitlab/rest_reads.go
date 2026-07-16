package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type userResponse struct {
	Username string `json:"username"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
}

type diffRefsResponse struct {
	BaseSHA  string `json:"base_sha"`
	StartSHA string `json:"start_sha"`
	HeadSHA  string `json:"head_sha"`
}

type mergeRequestResponse struct {
	IID             int64            `json:"iid"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	State           string           `json:"state"`
	WebURL          string           `json:"web_url"`
	Author          userResponse     `json:"author"`
	SourceBranch    string           `json:"source_branch"`
	TargetBranch    string           `json:"target_branch"`
	SHA             string           `json:"sha"`
	DiffRefs        diffRefsResponse `json:"diff_refs"`
	SourceProjectID int64            `json:"source_project_id"`
	TargetProjectID int64            `json:"target_project_id"`
}

type projectResponse struct {
	PathWithNamespace string `json:"path_with_namespace"`
}

type diffFileResponse struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
}

type approvalsResponse struct {
	ApprovedBy []struct {
		User userResponse `json:"user"`
	} `json:"approved_by"`
}

type noteResponse struct {
	ID         int64             `json:"id"`
	Type       string            `json:"type"`
	Body       string            `json:"body"`
	Author     userResponse      `json:"author"`
	System     bool              `json:"system"`
	Resolvable bool              `json:"resolvable"`
	Resolved   bool              `json:"resolved"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Position   *positionResponse `json:"position"`
}

type treeEntryResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Path string `json:"path"`
}

// WhoAmI returns the identity for the supplied credential.
func (c *Client) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	if err := validateCredential(gitprovider.OperationWhoAmI, creds); err != nil {
		return gitprovider.Identity{}, err
	}
	var user userResponse
	_, _, err := c.doRESTWithToken(ctx, gitprovider.OperationWhoAmI, http.MethodGet, restURL(c.baseURL, "user"), creds.Token, acceptJSON, &user)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	return identityFromUser(user), nil
}

// GetPR returns one merge request snapshot.
func (c *Client) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.PR{}, err
	}
	payload, err := c.getMergeRequest(ctx, gitprovider.OperationGetPR, ref)
	if err != nil {
		return gitprovider.PR{}, err
	}
	state, err := prState(payload.State)
	if err != nil {
		return gitprovider.PR{}, err
	}
	headOwner, headRepo := ref.Owner, ref.Repo
	if payload.SourceProjectID != 0 && payload.SourceProjectID != payload.TargetProjectID {
		owner, repo, err := c.projectPath(ctx, gitprovider.OperationGetPR, payload.SourceProjectID)
		if err != nil {
			return gitprovider.PR{}, err
		}
		headOwner, headRepo = owner, repo
	}
	headSHA := payload.DiffRefs.HeadSHA
	if headSHA == "" {
		headSHA = payload.SHA
	}
	return gitprovider.PR{
		Ref:    ref,
		Title:  payload.Title,
		Body:   payload.Description,
		URL:    payload.WebURL,
		State:  state,
		Author: identityFromUser(payload.Author),
		Head: gitprovider.PRBranchRef{
			Host:  c.host,
			Owner: headOwner,
			Repo:  headRepo,
			Name:  payload.SourceBranch,
			Ref:   "refs/heads/" + payload.SourceBranch,
			SHA:   headSHA,
		},
		Base: gitprovider.PRBranchRef{
			Host:  c.host,
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Name:  payload.TargetBranch,
			Ref:   "refs/heads/" + payload.TargetBranch,
			SHA:   payload.DiffRefs.BaseSHA,
		},
	}, nil
}

func (c *Client) getMergeRequest(ctx context.Context, op gitprovider.Operation, ref gitprovider.PRRef) (mergeRequestResponse, error) {
	var payload mergeRequestResponse
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number))
	if _, _, err := c.doREST(ctx, op, http.MethodGet, endpoint, acceptJSON, &payload); err != nil {
		return mergeRequestResponse{}, err
	}
	return payload, nil
}

func (c *Client) projectPath(ctx context.Context, op gitprovider.Operation, projectID int64) (owner, repo string, err error) {
	var payload projectResponse
	endpoint := restURL(c.baseURL, "projects", fmt.Sprint(projectID))
	if _, _, err := c.doREST(ctx, op, http.MethodGet, endpoint, acceptJSON, &payload); err != nil {
		return "", "", err
	}
	full := strings.Trim(payload.PathWithNamespace, "/")
	slash := strings.LastIndex(full, "/")
	if slash <= 0 || slash == len(full)-1 {
		return "", "", fmt.Errorf("%w: unexpected project path %q", ErrValidation, payload.PathWithNamespace)
	}
	return full[:slash], full[slash+1:], nil
}

// GetDiff returns the raw unified diff for a merge request. Newer GitLab
// versions serve it directly; older ones fall back to reconstruction from the
// per-file diffs listing.
func (c *Client) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "raw_diffs")
	body, _, err := c.doREST(ctx, gitprovider.OperationGetDiff, http.MethodGet, endpoint, acceptAny, nil)
	if err != nil {
		// GitLab instances older than 15.7 do not serve raw_diffs; the
		// per-file diffs listing still covers them.
		if errors.Is(err, gitprovider.ErrNotFound) {
			return c.reconstructMergeRequestDiff(ctx, ref)
		}
		return gitprovider.UnifiedDiff{}, err
	}
	return gitprovider.UnifiedDiff{Raw: string(body)}, nil
}

func (c *Client) reconstructMergeRequestDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	files, err := c.listMergeRequestDiffs(ctx, gitprovider.OperationGetDiff, ref)
	if err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	raw := reconstructUnifiedDiff(files)
	if raw == "" {
		return gitprovider.UnifiedDiff{}, gitprovider.WrapError(gitprovider.ErrNotFound, gitprovider.OperationGetDiff,
			errors.New("gitlab: merge request diff unavailable"))
	}
	return gitprovider.UnifiedDiff{Raw: raw}, nil
}

func (c *Client) listMergeRequestDiffs(ctx context.Context, op gitprovider.Operation, ref gitprovider.PRRef) ([]diffFileResponse, error) {
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "diffs"), url.Values{"per_page": {"100"}})
	return doRESTPages[diffFileResponse](ctx, c, op, endpoint)
}

// GetDiffBetweenRefs returns the raw unified diff between two git refs in the
// merge request's target repository.
func (c *Client) GetDiffBetweenRefs(ctx context.Context, ref gitprovider.PRRef, baseSHA, headSHA string) (gitprovider.UnifiedDiff, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	baseSHA = strings.TrimSpace(baseSHA)
	headSHA = strings.TrimSpace(headSHA)
	if baseSHA == "" {
		return gitprovider.UnifiedDiff{}, fmt.Errorf("%w: base SHA is required", ErrValidation)
	}
	if headSHA == "" {
		return gitprovider.UnifiedDiff{}, fmt.Errorf("%w: head SHA is required", ErrValidation)
	}
	var payload struct {
		Diffs []diffFileResponse `json:"diffs"`
	}
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "repository", "compare"), url.Values{
		"from": {baseSHA},
		"to":   {headSHA},
	})
	if _, _, err := c.doREST(ctx, gitprovider.OperationGetDiffBetweenRefs, http.MethodGet, endpoint, acceptJSON, &payload); err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	return gitprovider.UnifiedDiff{Raw: reconstructUnifiedDiff(payload.Diffs)}, nil
}

// reconstructUnifiedDiff synthesizes standard git-format unified diff text
// from GitLab per-file diffs, which carry hunks without the "diff --git"
// header lines.
func reconstructUnifiedDiff(files []diffFileResponse) string {
	var b strings.Builder
	for _, f := range files {
		newPath := f.NewPath
		oldPath := f.OldPath
		if newPath == "" && oldPath == "" {
			continue
		}
		if newPath == "" {
			newPath = oldPath
		}
		if oldPath == "" {
			oldPath = newPath
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", oldPath, newPath)
		if f.RenamedFile {
			fmt.Fprintf(&b, "rename from %s\n", oldPath)
			fmt.Fprintf(&b, "rename to %s\n", newPath)
		}
		diff := f.Diff
		if strings.TrimSpace(diff) == "" {
			// A pure rename carries no hunks and is already fully described.
			// Anything else without hunks is binary or collapsed as too
			// large; keep the change visible with a header-only entry.
			if !f.RenamedFile {
				fmt.Fprintf(&b, "Binary files a/%s and b/%s differ\n", oldPath, newPath)
			}
			continue
		}
		switch {
		case f.NewFile:
			b.WriteString("--- /dev/null\n")
			fmt.Fprintf(&b, "+++ b/%s\n", newPath)
		case f.DeletedFile:
			fmt.Fprintf(&b, "--- a/%s\n", oldPath)
			b.WriteString("+++ /dev/null\n")
		default:
			fmt.Fprintf(&b, "--- a/%s\n", oldPath)
			fmt.Fprintf(&b, "+++ b/%s\n", newPath)
		}
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// GetFileAtRef returns raw file contents at a git ref.
func (c *Client) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, filePath string) ([]byte, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitRef) == "" {
		return nil, fmt.Errorf("%w: git ref is required", ErrValidation)
	}
	normalized, err := validateFilePath(filePath)
	if err != nil {
		return nil, err
	}
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "repository", "files", url.PathEscape(normalized), "raw"), url.Values{"ref": {gitRef}})
	body, _, err := c.doREST(ctx, gitprovider.OperationGetFileAtRef, http.MethodGet, endpoint, acceptAny, nil)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// ListTreeAtRef returns the git tree entries at a ref and path.
func (c *Client) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, treePath string) ([]gitprovider.TreeEntry, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitRef) == "" {
		return nil, fmt.Errorf("%w: git ref is required", ErrValidation)
	}
	values := url.Values{"ref": {gitRef}, "per_page": {"100"}}
	if trimmed := strings.Trim(strings.TrimSpace(treePath), "/"); trimmed != "" {
		values.Set("path", trimmed)
	}
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "repository", "tree"), values)
	payloads, err := doRESTPages[treeEntryResponse](ctx, c, gitprovider.OperationListTreeAtRef, endpoint)
	if err != nil {
		return nil, err
	}
	entries := make([]gitprovider.TreeEntry, 0, len(payloads))
	for _, payload := range payloads {
		entries = append(entries, gitprovider.TreeEntry{
			Path: payload.Path,
			Type: payload.Type,
			SHA:  payload.ID,
		})
	}
	return entries, nil
}

// ListReviews maps current merge-request approvals to provider-neutral
// reviews. GitLab has no first-class review object: approvals are current
// state (revoking removes them), and review summaries are posted as notes.
func (c *Client) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	var payload approvalsResponse
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "approvals")
	if _, _, err := c.doREST(ctx, gitprovider.OperationListReviews, http.MethodGet, endpoint, acceptJSON, &payload); err != nil {
		return nil, err
	}
	reviews := make([]gitprovider.Review, 0, len(payload.ApprovedBy))
	for _, approval := range payload.ApprovedBy {
		reviews = append(reviews, gitprovider.Review{
			ID:     gitprovider.ReviewID("approval-" + stringIDFromInt(approval.User.ID)),
			Author: identityFromUser(approval.User),
			State:  gitprovider.ReviewStateApproved,
			Event:  review.ReviewEventApprove,
		})
	}
	return reviews, nil
}

// ListIssueComments returns non-system merge-request notes without a diff
// position, GitLab's equivalent of pull-request issue comments.
func (c *Client) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	endpoint := withQuery(restURL(c.baseURL, "projects", projectSegment(ref), "merge_requests", fmt.Sprint(ref.Number), "notes"), url.Values{"per_page": {"100"}})
	payloads, err := doRESTPages[noteResponse](ctx, c, gitprovider.OperationListIssueComments, endpoint)
	if err != nil {
		return nil, err
	}
	comments := make([]gitprovider.IssueComment, 0, len(payloads))
	for _, payload := range payloads {
		if payload.System || payload.Position != nil {
			continue
		}
		comments = append(comments, gitprovider.IssueComment{
			ID:        gitprovider.CommentID(stringIDFromInt(payload.ID)),
			Body:      payload.Body,
			Author:    identityFromUser(payload.Author),
			CreatedAt: payload.CreatedAt,
			UpdatedAt: payload.UpdatedAt,
		})
	}
	return comments, nil
}

func identityFromUser(user userResponse) gitprovider.Identity {
	return gitprovider.Identity{
		Login:       user.Username,
		ID:          stringIDFromInt(user.ID),
		DisplayName: user.Name,
	}
}

func prState(state string) (gitprovider.PRState, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened", "locked":
		return gitprovider.PRStateOpen, nil
	case "closed":
		return gitprovider.PRStateClosed, nil
	case "merged":
		return gitprovider.PRStateMerged, nil
	default:
		return "", fmt.Errorf("%w: unknown merge request state %q", ErrValidation, state)
	}
}

func validateFilePath(filePath string) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("%w: path is required", ErrValidation)
	}
	normalized := strings.Trim(filePath, "/")
	if normalized == "" {
		return "", fmt.Errorf("%w: path is required", ErrValidation)
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "" {
			return "", fmt.Errorf("%w: path must not contain empty segments", ErrValidation)
		}
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: path must not contain dot segments", ErrValidation)
		}
	}
	return normalized, nil
}
