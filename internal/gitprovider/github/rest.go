package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

type userResponse struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
}

type repoResponse struct {
	Name  string       `json:"name"`
	Owner userResponse `json:"owner"`
}

type branchResponse struct {
	Ref  string        `json:"ref"`
	SHA  string        `json:"sha"`
	Repo *repoResponse `json:"repo"`
}

type prResponse struct {
	Title   string         `json:"title"`
	HTMLURL string         `json:"html_url"`
	State   string         `json:"state"`
	Merged  bool           `json:"merged"`
	User    userResponse   `json:"user"`
	Head    branchResponse `json:"head"`
	Base    branchResponse `json:"base"`
}

type reviewResponse struct {
	ID          int64        `json:"id"`
	Body        string       `json:"body"`
	User        userResponse `json:"user"`
	State       string       `json:"state"`
	CommitID    string       `json:"commit_id"`
	HTMLURL     string       `json:"html_url"`
	SubmittedAt time.Time    `json:"submitted_at"`
}

type issueCommentResponse struct {
	ID        int64        `json:"id"`
	Body      string       `json:"body"`
	User      userResponse `json:"user"`
	HTMLURL   string       `json:"html_url"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type pullFileResponse struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Patch            string `json:"patch"`
}

// WhoAmI returns the identity for the supplied credential.
func (c *Client) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	if err := validateCredential(gitprovider.OperationWhoAmI, creds); err != nil {
		return gitprovider.Identity{}, err
	}
	if creds.Type == credentialTypeGitHubApp {
		return gitprovider.Identity{
			Login:       creds.Login,
			ID:          creds.ID,
			DisplayName: creds.DisplayName,
		}, nil
	}
	var user userResponse
	_, _, err := c.doRESTWithToken(ctx, gitprovider.OperationWhoAmI, http.MethodGet, restURL(c.baseURL, "user"), creds.Token, acceptJSON, &user)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	return identityFromUser(user), nil
}

// GetPR returns one pull request snapshot.
func (c *Client) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.PR{}, err
	}
	var payload prResponse
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number))
	_, _, err := c.doREST(ctx, gitprovider.OperationGetPR, http.MethodGet, endpoint, acceptJSON, &payload)
	if err != nil {
		return gitprovider.PR{}, err
	}
	state, err := prState(payload.State, payload.Merged)
	if err != nil {
		return gitprovider.PR{}, err
	}
	return gitprovider.PR{
		Ref:    ref,
		Title:  payload.Title,
		URL:    payload.HTMLURL,
		State:  state,
		Author: identityFromUser(payload.User),
		Head:   c.branchRef(payload.Head, ref),
		Base:   c.branchRef(payload.Base, ref),
	}, nil
}

// GetDiff returns the raw unified diff for a pull request.
func (c *Client) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number))
	body, _, err := c.doREST(ctx, gitprovider.OperationGetDiff, http.MethodGet, endpoint, acceptDiff, nil)
	if err != nil {
		// GitHub returns 406 from the diff endpoint once a diff exceeds its
		// size limit. The paginated files endpoint still serves per-file
		// patches, so reconstruct a unified diff from those instead of failing.
		if errors.Is(err, gitprovider.ErrDiffTooLarge) {
			return c.reconstructPullDiff(ctx, ref)
		}
		return gitprovider.UnifiedDiff{}, err
	}
	return gitprovider.UnifiedDiff{Raw: string(body)}, nil
}

// reconstructPullDiff rebuilds a unified diff from the paginated pull request
// files endpoint when the diff endpoint reports the diff is too large.
//
// This is a best-effort representation of the change:
//   - Files whose patch GitHub omits (binary blobs or individually oversized
//     files) are emitted as a header-only entry rather than hunks.
//   - GitHub caps this listing at 3000 files; pull requests larger than that are
//     still truncated.
//
// The result is standard git-format unified diff text so the existing diff
// parser and reviewers consume it unchanged.
func (c *Client) reconstructPullDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number), "files") + "?per_page=100"
	files, err := doRESTPages[pullFileResponse](ctx, c, gitprovider.OperationGetDiff, endpoint, acceptJSON)
	if err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	raw := reconstructUnifiedDiff(files)
	if raw == "" {
		return gitprovider.UnifiedDiff{}, gitprovider.WrapError(gitprovider.ErrDiffTooLarge, gitprovider.OperationGetDiff,
			errors.New("github: diff exceeds the host size limit and the files endpoint returned no patches"))
	}
	return gitprovider.UnifiedDiff{Raw: raw}, nil
}

// reconstructUnifiedDiff synthesizes standard git-format unified diff text from
// the pull request files listing. GitHub's per-file patch omits the
// "diff --git"/"---"/"+++" header lines, so they are rebuilt here from the file
// status and (for renames) the previous filename.
func reconstructUnifiedDiff(files []pullFileResponse) string {
	var b strings.Builder
	for _, f := range files {
		newPath := f.Filename
		if newPath == "" {
			continue
		}
		oldPath := f.PreviousFilename
		if oldPath == "" {
			oldPath = newPath
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", oldPath, newPath)
		if f.Status == "renamed" {
			fmt.Fprintf(&b, "rename from %s\n", oldPath)
			fmt.Fprintf(&b, "rename to %s\n", newPath)
		}
		if f.Patch == "" {
			// A pure rename has no patch and is already fully described above.
			// Anything else without a patch is a binary or oversized file; mark
			// it so the change stays visible even though there are no hunks.
			if f.Status != "renamed" {
				fmt.Fprintf(&b, "Binary files a/%s and b/%s differ\n", oldPath, newPath)
			}
			continue
		}
		switch f.Status {
		case "added":
			b.WriteString("--- /dev/null\n")
			fmt.Fprintf(&b, "+++ b/%s\n", newPath)
		case "removed":
			fmt.Fprintf(&b, "--- a/%s\n", oldPath)
			b.WriteString("+++ /dev/null\n")
		default:
			fmt.Fprintf(&b, "--- a/%s\n", oldPath)
			fmt.Fprintf(&b, "+++ b/%s\n", newPath)
		}
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// GetDiffBetweenRefs returns the raw unified diff between two git refs in the
// pull request's base repository.
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
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "compare", baseSHA+"..."+headSHA)
	body, _, err := c.doREST(ctx, gitprovider.OperationGetDiffBetweenRefs, http.MethodGet, endpoint, acceptDiff, nil)
	if err != nil {
		return gitprovider.UnifiedDiff{}, err
	}
	return gitprovider.UnifiedDiff{Raw: string(body)}, nil
}

// GetFileAtRef returns raw file contents at a git ref.
func (c *Client) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, filePath string) ([]byte, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitRef) == "" {
		return nil, fmt.Errorf("%w: git ref is required", ErrValidation)
	}
	endpoint, err := restFileURL(c.baseURL, ref.Owner, ref.Repo, filePath, gitRef)
	if err != nil {
		return nil, err
	}
	body, _, err := c.doREST(ctx, gitprovider.OperationGetFileAtRef, http.MethodGet, endpoint, acceptRaw, nil)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// ListReviews returns all pull request reviews.
func (c *Client) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "pulls", fmt.Sprint(ref.Number), "reviews")
	payloads, err := doRESTPages[reviewResponse](ctx, c, gitprovider.OperationListReviews, endpoint, acceptJSON)
	if err != nil {
		return nil, err
	}
	reviews := make([]gitprovider.Review, 0, len(payloads))
	for _, payload := range payloads {
		state, event, err := reviewState(payload.State)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, gitprovider.Review{
			ID:          gitprovider.ReviewID(stringIDFromInt(payload.ID)),
			Body:        payload.Body,
			Author:      identityFromUser(payload.User),
			State:       state,
			Event:       event,
			CommitSHA:   payload.CommitID,
			URL:         payload.HTMLURL,
			SubmittedAt: payload.SubmittedAt,
		})
	}
	return reviews, nil
}

// ListIssueComments returns all issue comments on the pull request's issue.
func (c *Client) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	if err := c.validatePRRef(ref); err != nil {
		return nil, err
	}
	endpoint := restURL(c.baseURL, "repos", ref.Owner, ref.Repo, "issues", fmt.Sprint(ref.Number), "comments")
	payloads, err := doRESTPages[issueCommentResponse](ctx, c, gitprovider.OperationListIssueComments, endpoint, acceptJSON)
	if err != nil {
		return nil, err
	}
	comments := make([]gitprovider.IssueComment, 0, len(payloads))
	for _, payload := range payloads {
		comments = append(comments, gitprovider.IssueComment{
			ID:        gitprovider.CommentID(stringIDFromInt(payload.ID)),
			Body:      payload.Body,
			Author:    identityFromUser(payload.User),
			URL:       payload.HTMLURL,
			CreatedAt: payload.CreatedAt,
			UpdatedAt: payload.UpdatedAt,
		})
	}
	return comments, nil
}

func identityFromUser(user userResponse) gitprovider.Identity {
	return gitprovider.Identity{
		Login:       user.Login,
		ID:          stringIDFromInt(user.ID),
		DisplayName: user.Name,
	}
}

func (c *Client) branchRef(branch branchResponse, ref gitprovider.PRRef) gitprovider.PRBranchRef {
	owner := ref.Owner
	repoName := ref.Repo
	if branch.Repo != nil {
		if branch.Repo.Owner.Login != "" {
			owner = branch.Repo.Owner.Login
		}
		if branch.Repo.Name != "" {
			repoName = branch.Repo.Name
		}
	}
	return gitprovider.PRBranchRef{
		Host:  c.host,
		Owner: owner,
		Repo:  repoName,
		Name:  branch.Ref,
		// GitHub pull request refs are branch names, not full refspecs.
		Ref: "refs/heads/" + branch.Ref,
		SHA: branch.SHA,
	}
}

func prState(state string, merged bool) (gitprovider.PRState, error) {
	if merged {
		return gitprovider.PRStateMerged, nil
	}
	normalized := gitprovider.PRState(strings.ToLower(strings.TrimSpace(state)))
	if !normalized.Valid() {
		return "", fmt.Errorf("%w: unknown PR state %q", ErrValidation, state)
	}
	return normalized, nil
}

func reviewState(state string) (gitprovider.ReviewState, review.ReviewEvent, error) {
	normalized := strings.ToLower(strings.TrimSpace(state))
	switch normalized {
	case "approved":
		return gitprovider.ReviewStateApproved, review.ReviewEventApprove, nil
	case "changes_requested":
		return gitprovider.ReviewStateChangesRequested, review.ReviewEventRequestChanges, nil
	case "commented":
		return gitprovider.ReviewStateCommented, review.ReviewEventComment, nil
	case "dismissed":
		return gitprovider.ReviewStateDismissed, "", nil
	case "pending":
		return gitprovider.ReviewStatePending, "", nil
	default:
		return "", "", fmt.Errorf("%w: unknown review state %q", ErrValidation, state)
	}
}
