// Package prref parses and normalizes pull-request references.
package prref

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// ShortSHA returns the SHA trimmed of surrounding whitespace and truncated to 12 characters.
func ShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ParseGitHubPullURL parses the v1 GitHub pull-request URL form.
func ParseGitHubPullURL(raw string) (gitprovider.PRRef, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitHub pull request URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitHub pull request URL")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return gitprovider.PRRef{}, fmt.Errorf("PR number must be positive")
	}
	ref := gitprovider.PRRef{
		Host:   parsed.Host,
		Owner:  parts[0],
		Repo:   parts[1],
		Number: number,
	}
	if err := ref.Validate(); err != nil {
		return gitprovider.PRRef{}, err
	}
	return ref, nil
}

// SameHost reports whether two host strings identify the same host.
func SameHost(left, right string) bool {
	return normalizeHost(left) == normalizeHost(right)
}

func normalizeHost(raw string) string {
	host := strings.TrimSpace(raw)
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return strings.ToLower(strings.TrimSuffix(host, "/"))
}
