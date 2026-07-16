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

// Provider identifies the git-host URL family a pull-request reference was
// parsed from.
type Provider string

// Providers recognized by ParsePullURL.
const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
)

// ParsePullURL parses a GitHub pull-request URL or a GitLab merge-request URL
// and reports which provider family the URL belongs to.
func ParsePullURL(raw string) (gitprovider.PRRef, Provider, error) {
	if ref, err := ParseGitHubPullURL(raw); err == nil {
		return ref, ProviderGitHub, nil
	}
	if ref, err := ParseGitLabMergeRequestURL(raw); err == nil {
		return ref, ProviderGitLab, nil
	}
	return gitprovider.PRRef{}, "", fmt.Errorf("PR must be a GitHub pull request URL or GitLab merge request URL")
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

// ParseGitLabMergeRequestURL parses a GitLab merge-request URL. Both the
// canonical `/-/merge_requests/<iid>` form and the legacy form without the
// `/-/` separator are accepted, and namespaces may be nested, so the parsed
// owner can contain slashes (for example `group/subgroup`).
func ParseGitLabMergeRequestURL(raw string) (gitprovider.PRRef, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitLab merge request URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	last := len(parts) - 1
	if last < 3 || parts[last-1] != "merge_requests" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitLab merge request URL")
	}
	projectParts := parts[:last-1]
	if projectParts[len(projectParts)-1] == "-" {
		projectParts = projectParts[:len(projectParts)-1]
	}
	if len(projectParts) < 2 {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitLab merge request URL")
	}
	for _, segment := range projectParts {
		if segment == "" || segment == "." || segment == ".." || segment == "-" {
			return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitLab merge request URL")
		}
	}
	number, err := strconv.Atoi(parts[last])
	if err != nil || number <= 0 {
		return gitprovider.PRRef{}, fmt.Errorf("PR number must be positive")
	}
	ref := gitprovider.PRRef{
		Host:   parsed.Host,
		Owner:  strings.Join(projectParts[:len(projectParts)-1], "/"),
		Repo:   projectParts[len(projectParts)-1],
		Number: number,
	}
	if err := ref.Validate(); err != nil {
		return gitprovider.PRRef{}, err
	}
	return ref, nil
}

// MatchProvider returns a usage-shaped error when a parsed PR URL's provider
// family does not match the configured git provider kind.
func MatchProvider(urlProvider Provider, configured string) error {
	if string(urlProvider) == configured {
		return nil
	}
	return fmt.Errorf("PR URL is a %s URL but the configured git provider is %q", urlProvider, configured)
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
