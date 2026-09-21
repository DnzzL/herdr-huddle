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
	"regexp"
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

// errNotModified reports a 304 in response to a conditional request. It is not
// an error for a poller — it means "nothing changed, and this request did not
// count against the rate limit" — so it is distinguished from *APIError.
var errNotModified = errors.New("github: not modified")

// do performs one request and decodes a JSON response into out, which may be
// nil. body is encoded as JSON when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.doWith(ctx, method, path, body, nil, out)
	return err
}

// doWith is do with conditional-request headers, returning the response headers
// so a caller can remember an ETag.
func (c *Client) doWith(ctx context.Context, method, path string, body any, headers map[string]string, out any) (http.Header, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("github: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, payload)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header, errNotModified
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, apiError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			// A 204 or an empty body is not an error for callers that do not
			// need a response.
			return resp.Header, nil
		}
		return resp.Header, fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return resp.Header, nil
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
// Repository is the subset of repository metadata a share needs.
type Repository struct {
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// Repository reads repository metadata.
func (c *Client) Repository(ctx context.Context, owner, repo string) (Repository, error) {
	if err := validateRepo(owner, repo); err != nil {
		return Repository{}, err
	}
	var out Repository
	if err := c.do(ctx, http.MethodGet, repoPath(owner, repo), nil, &out); err != nil {
		return Repository{}, err
	}
	return out, nil
}

// validateRepo rejects a slug that would not be a single path segment. The parts
// end up in a URL path, so anything beyond that is refused rather than escaped
// and hoped for.
func validateRepo(owner, repo string) error {
	if owner == "" || repo == "" {
		return errors.New("github: owner and repo are required")
	}
	if strings.ContainsAny(owner, "/ ") || strings.ContainsAny(repo, "/ ") {
		return fmt.Errorf("github: invalid owner/repo %q/%q", owner, repo)
	}
	return nil
}

// RepoIsPrivate reports whether a repository is private. It is the Repository
// call, reduced to the one field its caller needs.
//
// Used unauthenticated to decide how wide a token's scope must be: a private or
// invisible repository needs "repo", anything else needs only "public_repo".
func (c *Client) RepoIsPrivate(ctx context.Context, owner, repo string) (bool, error) {
	metadata, err := c.Repository(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	return metadata.Private, nil
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

// PullRequest is the subset of GitHub's pull request object a share uses.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	State   string `json:"state"`
}

// CreatePullRequestRequest is the body of a pull request creation.
type CreatePullRequestRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	// Draft is not optional in practice: ADR-001 opens a share as a draft so the
	// thread can exist before the code does.
	Draft bool `json:"draft"`
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// CreatePullRequest opens a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo string, req CreatePullRequestRequest) (PullRequest, error) {
	var out PullRequest
	if err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", req, &out); err != nil {
		return PullRequest{}, err
	}
	return out, nil
}

// FindOpenPullRequests returns the open pull requests whose head is the given
// owner's branch.
//
// It returns a slice rather than one result because more than one open pull
// request can share a head when the base differs, and silently taking one would
// hide that. No results is not an error: it is the normal state before a first
// share.
func (c *Client) FindOpenPullRequests(ctx context.Context, owner, repo, headOwner, branch string) ([]PullRequest, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("head", headOwner+":"+branch)
	q.Set("per_page", "10")

	var out []PullRequest
	if err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/pulls?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// loginRE is GitHub's own rule for a username: alphanumerics and single
// hyphens, never leading or trailing a hyphen.
//
// The trailing case is expressible without a lookahead — a hyphen must be
// followed by an alphanumeric — which matters because Go's regexp is RE2 and
// has no lookahead to reach for. The length limit is checked separately, since
// RE2 has no bounded-repetition-plus-assertion either.
var loginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9])*$`)

// maxLoginLen is GitHub's limit on a username.
const maxLoginLen = 39

// AddCollaborator invites a user with read access, which ADR-001 identifies as
// the least a commenter needs.
//
// Documentation and measurement disagree here: the API documentation implies a
// second invite answers 204, but inviting someone who already has access was
// measured to answer 422 Validation Failed. A caller that has to know whether
// the person can read the repository asks HasAccess rather than reading this
// error.
func (c *Client) AddCollaborator(ctx context.Context, owner, repo, login string) error {
	// The login becomes a URL path segment, so it is checked against GitHub's
	// own rules rather than trusted.
	if !loginRE.MatchString(login) || len(login) > maxLoginLen {
		return fmt.Errorf("github: %q is not a GitHub login", login)
	}
	path := repoPath(owner, repo) + "/collaborators/" + url.PathEscape(login)
	err := c.do(ctx, http.MethodPut, path, map[string]string{"permission": "pull"}, nil)
	if err == nil {
		return nil
	}
	// ADR-001 records this as a hard limit rather than something to work
	// around: inviting needs admin on the repository. Saying so saves the
	// operator from hunting for a bug that is a permission.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		return fmt.Errorf("cannot invite %s to %s: inviting a collaborator needs admin rights on the repository: %w",
			login, repoPath(owner, repo), err)
	}
	return err
}

// HasAccess reports whether a login can already read the repository.
//
// It exists because an invitation is a request for access, and access that is
// already there is not a failure: measured, inviting a `write` collaborator
// answers 422 rather than the documented 204. Without this, asking to share a
// thread with someone who can already read it would leave them off the
// allowlist, and their comments would be ignored on a pull request they can see.
func (c *Client) HasAccess(ctx context.Context, owner, repo, login string) (bool, error) {
	if !loginRE.MatchString(login) || len(login) > maxLoginLen {
		return false, fmt.Errorf("github: %q is not a GitHub login", login)
	}
	// 204 for a collaborator, 404 for anyone else: the status is the answer, so
	// there is nothing to decode.
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/collaborators/"+url.PathEscape(login), nil, nil)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}
