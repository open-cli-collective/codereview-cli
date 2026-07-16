package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

const (
	acceptJSON = "application/json"
	acceptAny  = "*/*"
)

func (c *Client) doREST(ctx context.Context, op gitprovider.Operation, method, endpoint, accept string, out any) ([]byte, http.Header, error) {
	return c.doRESTWithToken(ctx, op, method, endpoint, c.token, accept, out)
}

func (c *Client) doRESTWithToken(ctx context.Context, op gitprovider.Operation, method, endpoint, token, accept string, out any) ([]byte, http.Header, error) {
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
			return nil, resp.Header, fmt.Errorf("%w: decode GitLab response: %w", ErrValidation, err)
		}
	}
	return body, resp.Header, nil
}

func (c *Client) doRESTJSON(ctx context.Context, op gitprovider.Operation, method, endpoint string, in any, out any) error {
	var reader io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	setHeaders(req, c.token, acceptJSON)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%w: decode GitLab response: %w", ErrValidation, err)
		}
	}
	return nil
}

func doRESTPages[T any](ctx context.Context, c *Client, op gitprovider.Operation, endpoint string) ([]T, error) {
	var all []T
	next := endpoint
	for next != "" {
		var page []T
		_, header, err := c.doREST(ctx, op, http.MethodGet, next, acceptJSON, &page)
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

func setHeaders(req *http.Request, token, accept string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "codereview-cli")
}

func mapTransportError(op gitprovider.Operation, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return gitprovider.WrapError(gitprovider.ErrRetryable, op, err)
}

func mapHTTPStatus(op gitprovider.Operation, status int, body []byte) error {
	err := httpStatusError(status, body)
	switch status {
	case http.StatusUnauthorized:
		return gitprovider.WrapError(gitprovider.ErrAuth, op, err)
	case http.StatusForbidden:
		return gitprovider.WrapError(gitprovider.ErrPermission, op, err)
	case http.StatusNotFound:
		return gitprovider.WrapError(gitprovider.ErrNotFound, op, err)
	case http.StatusConflict:
		return gitprovider.WrapError(gitprovider.ErrConflict, op, err)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
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

func httpStatusError(status int, body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return fmt.Errorf("gitlab: status %d", status)
	}
	return fmt.Errorf("gitlab: status %d (response body redacted)", status)
}

// restURL joins pre-escaped path parts onto the REST base. Parts must already
// be percent-encoded (for example via url.PathEscape) because GitLab encodes
// whole project paths and file paths as single path segments.
func restURL(base *url.URL, parts ...string) string {
	joined := strings.TrimSuffix(base.String(), "/")
	for _, part := range parts {
		joined += "/" + part
	}
	return joined
}

func projectSegment(ref gitprovider.PRRef) string {
	return url.PathEscape(ref.Owner + "/" + ref.Repo)
}

func withQuery(endpoint string, values url.Values) string {
	if len(values) == 0 {
		return endpoint
	}
	return endpoint + "?" + values.Encode()
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
		return "", fmt.Errorf("%w: pagination URL host does not match GitLab REST host", ErrValidation)
	}
	basePath := c.baseURL.EscapedPath()
	if basePath == "" {
		basePath = "/"
	}
	if basePath != "/" && !strings.HasPrefix(parsed.EscapedPath(), basePath) {
		return "", fmt.Errorf("%w: pagination URL path escapes GitLab REST base", ErrValidation)
	}
	return parsed.String(), nil
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

func stringIDFromInt(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
