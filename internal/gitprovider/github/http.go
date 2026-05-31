package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const (
	acceptJSON = "application/vnd.github+json"
	acceptDiff = "application/vnd.github.v3.diff"
	acceptRaw  = "application/vnd.github.raw"
)

func (c *Client) doREST(ctx context.Context, op gitprovider.Operation, method, endpoint, token, accept string, out any) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	setHeaders(req, token, accept)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, mapTransportError(op, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, nil, gitprovider.WrapError(gitprovider.ErrRetryable, op, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Header, mapHTTPStatus(op, resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, resp.Header, fmt.Errorf("%w: decode GitHub response: %w", ErrValidation, err)
		}
	}
	return body, resp.Header, nil
}

func (c *Client) doRESTJSON(ctx context.Context, op gitprovider.Operation, method, endpoint string, in any, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	setHeaders(req, c.token, acceptJSON)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mapTransportError(op, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mapWriteHTTPStatus(op, resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%w: decode GitHub response: %w", ErrValidation, err)
		}
	}
	return nil
}

func doRESTPages[T any](ctx context.Context, c *Client, op gitprovider.Operation, endpoint, accept string) ([]T, error) {
	var all []T
	next := endpoint
	for next != "" {
		var page []T
		_, header, err := c.doREST(ctx, op, http.MethodGet, next, c.token, accept, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next, err = c.nextPageURL(header.Get("Link"))
		if err != nil {
			return nil, err
		}
	}
	return all, nil
}

func (c *Client) doGraphQL(ctx context.Context, op gitprovider.Operation, query string, variables map[string]any, out any) error {
	return c.doGraphQLWithErrorMapper(ctx, op, query, variables, out, mapGraphQLErrors)
}

func (c *Client) doGraphQLWithErrorMapper(ctx context.Context, op gitprovider.Operation, query string, variables map[string]any, out any, mapErrors func(gitprovider.Operation, []graphQLError) error) error {
	payload, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphQLURL.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	setHeaders(req, c.token, acceptJSON)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mapTransportError(op, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mapHTTPStatus(op, resp.StatusCode, body)
	}

	var envelope graphQLResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("%w: decode GraphQL response: %w", ErrValidation, err)
	}
	if len(envelope.Errors) > 0 {
		return mapErrors(op, envelope.Errors)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("%w: empty GraphQL data", ErrUnhandledGraphQL)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%w: decode GraphQL data: %w", ErrValidation, err)
	}
	return nil
}

func setHeaders(req *http.Request, token, accept string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "codereview-cli")
}

func mapTransportError(op gitprovider.Operation, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
	}
	return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
}

func mapHTTPStatus(op gitprovider.Operation, status int, body []byte) error {
	err := fmt.Errorf("github: status %d: %s", status, sanitizedBody(body))
	switch status {
	case http.StatusUnauthorized:
		return gitprovider.WrapError(gitprovider.ErrAuth, op, err)
	case http.StatusForbidden:
		return gitprovider.WrapError(gitprovider.ErrPermission, op, err)
	case http.StatusNotFound:
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, err)
	case http.StatusConflict:
		return gitprovider.WrapError(gitprovider.ErrConflict, op, err)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", ErrValidation, err)
	case http.StatusTooManyRequests:
		return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
	default:
		if status >= 500 && status <= 599 {
			return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
		}
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
}

func mapWriteHTTPStatus(op gitprovider.Operation, status int, body []byte) error {
	err := fmt.Errorf("github: status %d: %s", status, sanitizedBody(body))
	if status == http.StatusUnprocessableEntity {
		if isCommitBoundWriteOperation(op) && isStaleSHABody(body) {
			return gitprovider.WrapError(gitprovider.ErrStaleSHA, op, err)
		}
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return mapHTTPStatus(op, status, body)
}

func isCommitBoundWriteOperation(op gitprovider.Operation) bool {
	return op == gitprovider.OperationPostInlineComment || op == gitprovider.OperationSubmitReview
}

func isStaleSHABody(body []byte) bool {
	normalized := strings.ToLower(string(body))
	return strings.Contains(normalized, "commit_id") &&
		strings.Contains(normalized, "not the head commit")
}

func sanitizedBody(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) > 256 {
		body = body[:256]
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	return string(body)
}

func restURL(base *url.URL, parts ...string) string {
	copied := *base
	escaped := make([]string, 0, len(parts)+1)
	if copied.Path != "" && copied.Path != "/" {
		escaped = append(escaped, strings.Trim(copied.Path, "/"))
	}
	escaped = append(escaped, parts...)
	copied.Path = "/" + path.Join(escaped...)
	return copied.String()
}

func restFileURL(base *url.URL, owner, repo, filePath, gitRef string) (string, error) {
	normalizedPath, err := validateFilePath(filePath)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(restURL(base, "repos", owner, repo, "contents"))
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + normalizedPath
	q := u.Query()
	q.Set("ref", gitRef)
	u.RawQuery = q.Encode()
	return u.String(), nil
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

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		pieces := strings.Split(part, ";")
		if len(pieces) < 2 {
			continue
		}
		if !strings.Contains(pieces[1], `rel="next"`) {
			continue
		}
		link := strings.TrimSpace(pieces[0])
		link = strings.TrimPrefix(link, "<")
		link = strings.TrimSuffix(link, ">")
		return link
	}
	return ""
}

func (c *Client) nextPageURL(header string) (string, error) {
	next := nextLink(header)
	if next == "" {
		return "", nil
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("%w: invalid pagination URL", ErrValidation)
	}
	if !parsed.IsAbs() {
		parsed = c.baseURL.ResolveReference(parsed)
	}
	if parsed.Scheme != c.baseURL.Scheme || !strings.EqualFold(parsed.Host, c.baseURL.Host) {
		return "", fmt.Errorf("%w: pagination URL host does not match GitHub REST host", ErrValidation)
	}
	basePath := c.baseURL.EscapedPath()
	if basePath == "" {
		basePath = "/"
	}
	if basePath != "/" && !strings.HasPrefix(parsed.EscapedPath(), basePath) {
		return "", fmt.Errorf("%w: pagination URL path escapes GitHub REST base", ErrValidation)
	}
	return parsed.String(), nil
}

func stringIDFromInt(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
