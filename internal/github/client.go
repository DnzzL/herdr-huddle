// Package github is a minimal GitHub REST client: only the endpoints
// herdr-huddle needs, and only the behaviour it depends on.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DefaultBaseURL is the GitHub REST API root. It is a field so tests can point
// a client at a local server.
const DefaultBaseURL = "https://api.github.com"

// ErrNotFound reports a 404. Callers distinguish it because an unauthenticated
// read of a private repo also 404s — the repo exists but we cannot see it.
var ErrNotFound = errors.New("github: not found")

// APIError is a non-2xx response. RateLimited is called out separately because
// the poller's whole ETag strategy exists to avoid it, so the state has to be
// recognisable rather than inferred from the message.
type APIError struct {
	Status      int
	Message     string
	RateLimited bool
	Reset       string
}

func (e *APIError) Error() string {
	if e.RateLimited {
		return fmt.Sprintf("github: rate limited (%d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("github: %d: %s", e.Status, e.Message)
}

// Client talks to the GitHub REST API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do performs one request and decodes a JSON response into out, which may be
// nil. body is encoded as JSON when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, payload)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	// The version header pins behaviour: without it GitHub may serve an older
	// API shape.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// GitHub rejects requests with no User-Agent.
	req.Header.Set("User-Agent", "herdr-huddle")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			// A 204 or an empty body is not an error for callers that do not
			// need a response.
			return nil
		}
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}

// apiError turns a failed response into an *APIError, tolerating bodies that
// are not JSON (GitHub's edge returns HTML on 5xx).
func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	message := strings.TrimSpace(string(raw))
	var decoded struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &decoded); err == nil && decoded.Message != "" {
		message = decoded.Message
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	// Keep HTML out of logs and PR bodies; a 500-page is not a useful message.
	if strings.HasPrefix(message, "<") {
		message = fmt.Sprintf("%s (non-JSON response body)", http.StatusText(resp.StatusCode))
	}
	if len(message) > 500 {
		message = message[:500] + "…"
	}

	err := &APIError{
		Status:  resp.StatusCode,
		Message: message,
		Reset:   resp.Header.Get("X-RateLimit-Reset"),
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		err.RateLimited = true
	}
	// A 403 is a rate limit only when the quota is actually exhausted.
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		err.RateLimited = true
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	}
	return err
}

// RepoIsPrivate reports whether a repo is private.
//
// Called before a token exists, to pick the narrowest OAuth scope. GitHub
// answers 404 rather than 403 to an unauthenticated read of a private repo, so
// ErrNotFound means "private, or does not exist, or not visible to us" — the
// caller must treat it as needing the broader scope.
func (c *Client) RepoIsPrivate(ctx context.Context, owner, repo string) (bool, error) {
	if owner == "" || repo == "" {
		return false, errors.New("github: owner and repo are required")
	}
	if strings.ContainsAny(owner, "/ ") || strings.ContainsAny(repo, "/ ") {
		return false, fmt.Errorf("github: invalid owner/repo %q/%q", owner, repo)
	}

	var out struct {
		Private bool `json:"private"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return out.Private, nil
}

// Viewer returns the login of the authenticated user.
//
// Used as a liveness check on a stored token: a revoked or under-scoped token
// should fail here, when the operator runs `auth status`, rather than halfway
// through opening a pull request. An empty login is treated as an error so a
// malformed response cannot read as success.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	if out.Login == "" {
		return "", errors.New("github: /user returned no login")
	}
	return out.Login, nil
}
