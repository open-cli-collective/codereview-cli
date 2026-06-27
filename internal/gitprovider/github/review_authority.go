package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

type collaboratorPermissionResponse struct {
	Permission string `json:"permission"`
	RoleName   string `json:"role_name"`
}

// ReviewAuthority reports whether identity can create an opinionated review for ref.
func (c *Client) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	if err := c.validatePRRef(ref); err != nil {
		return gitprovider.ReviewAuthority{}, err
	}
	login := strings.TrimSpace(identity.Login)
	if login == "" {
		return gitprovider.ReviewAuthority{}, fmt.Errorf("%w: identity login is required", ErrValidation)
	}
	endpoint, err := collaboratorPermissionURL(c.baseURL, ref.Owner, ref.Repo, login)
	if err != nil {
		return gitprovider.ReviewAuthority{}, err
	}
	var response collaboratorPermissionResponse
	if _, _, err := c.doREST(ctx, gitprovider.OperationReviewAuthority, http.MethodGet, endpoint, acceptJSON, &response); err != nil {
		if errors.Is(err, gitprovider.ErrNotFound) {
			return gitprovider.ReviewAuthority{}, nil
		}
		return gitprovider.ReviewAuthority{}, err
	}
	return gitprovider.ReviewAuthority{
		Eligible:   eligibleReviewAuthority(response.Permission, response.RoleName),
		Permission: strings.TrimSpace(response.Permission),
		RoleName:   strings.TrimSpace(response.RoleName),
	}, nil
}

func collaboratorPermissionURL(base *url.URL, owner, repo, login string) (string, error) {
	u, err := url.Parse(restURL(base, "repos", owner, repo, "collaborators"))
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + login + "/permission"
	u.RawPath = basePath + "/" + url.PathEscape(login) + "/permission"
	return u.String(), nil
}

func eligibleReviewAuthority(permission, roleName string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "admin", "write":
		return true
	case "read", "none":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(roleName)) {
	case "admin", "write", "maintain":
		return true
	case "read", "none", "triage":
		return false
	}
	return false
}
