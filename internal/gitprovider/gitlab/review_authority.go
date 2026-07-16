package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// GitLab access levels relevant to merge-request approval eligibility.
const accessLevelDeveloper = 30

type memberResponse struct {
	AccessLevel int `json:"access_level"`
}

// ReviewAuthority reports whether the resolved posting identity can approve
// merge requests on the target project. GitLab requires at least Developer
// access to approve.
func (c *Client) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	op := gitprovider.OperationReviewAuthority
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.ReviewAuthority{}, err
	}
	userID := strings.TrimSpace(identity.ID)
	if userID == "" {
		return gitprovider.ReviewAuthority{}, fmt.Errorf("%w: identity ID is required", ErrValidation)
	}
	var payload memberResponse
	endpoint := restURL(c.baseURL, "projects", projectSegment(ref), "members", "all", url.PathEscape(userID))
	if _, _, err := c.doREST(ctx, op, http.MethodGet, endpoint, acceptJSON, &payload); err != nil {
		if errors.Is(err, gitprovider.ErrNotFound) {
			return gitprovider.ReviewAuthority{}, nil
		}
		return gitprovider.ReviewAuthority{}, err
	}
	return gitprovider.ReviewAuthority{
		Eligible:   payload.AccessLevel >= accessLevelDeveloper,
		Permission: accessLevelName(payload.AccessLevel),
	}, nil
}

func accessLevelName(level int) string {
	switch level {
	case 5:
		return "minimal"
	case 10:
		return "guest"
	case 15:
		return "planner"
	case 20:
		return "reporter"
	case 30:
		return "developer"
	case 40:
		return "maintainer"
	case 50:
		return "owner"
	default:
		return fmt.Sprintf("access_level_%d", level)
	}
}
