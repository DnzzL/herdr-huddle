package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// MaxCommentRunes is GitHub's limit on issue, pull request and comment bodies.
// It is measured in characters, not bytes, and the API rejects anything longer
// with a 422 rather than truncating it — so callers must fit their content, and
// this constant is what they fit it to.
const MaxCommentRunes = 65536

// Comment is the subset of GitHub's issue-comment object the thread needs.
//
// These objects are the thread: with the body reduced to a header (ADR-003),
// the conversation is entirely comments.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	// Association is the commenter's relationship to the repository — OWNER,
	// MEMBER, COLLABORATOR, CONTRIBUTOR, NONE. It is one of the three gates a
	// comment must pass to reach the agent, so an empty value must deny rather
	// than fall through.
	Association string    `json:"author_association"`
	CreatedAt   time.Time `json:"created_at"`
	HTMLURL     string    `json:"html_url"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Author is the login that wrote the comment.
func (c Comment) Author() string { return c.User.Login }

// Comments is one page of a thread's comments.
//
// NotModified and ETag exist so the poller can re-ask every 10s without spending
// its hourly quota to be told nothing changed (ADR-001).
type Comments struct {
	Items       []Comment
	ETag        string
	NotModified bool
}

// ListComments reads a pull request's comments, newest first.
//
// `since` filters on update time, so an edited old comment comes back — which is
// what a caller wants, because an edit can turn a harmless comment into an
// instruction. Newest first keeps the response useful when a thread is longer
// than one page: the comments the poller has not seen are the recent ones.
//
// An empty etag sends an unconditional request. A returned NotModified means
// nothing changed and the request did not count against the rate limit.
func (c *Client) ListComments(ctx context.Context, owner, repo string, number int, since time.Time, etag string) (Comments, error) {
	if err := validateRepo(owner, repo); err != nil {
		return Comments{}, err
	}
	if number <= 0 {
		return Comments{}, fmt.Errorf("github: comment list needs a pull request number, got %d", number)
	}

	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("sort", "created")
	q.Set("direction", "desc")
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}

	headers := map[string]string{}
	if etag != "" {
		headers["If-None-Match"] = etag
	}

	// A pointer to the slice, so a response that omits the array is an error
	// instead of an empty thread. Without this, "the shape changed" and "nobody
	// has commented" decode identically.
	var raw *[]Comment
	hdrs, err := c.doWith(ctx, http.MethodGet, repoPath(owner, repo)+"/issues/"+itoa(number)+"/comments?"+q.Encode(), nil, headers, &raw)
	if errors.Is(err, errNotModified) {
		return Comments{ETag: etag, NotModified: true}, nil
	}
	if err != nil {
		return Comments{}, err
	}
	if raw == nil {
		return Comments{}, errors.New("github: comment list response had no comments array")
	}
	return Comments{Items: *raw, ETag: hdrs.Get("ETag")}, nil
}

// CreateComment posts a comment to a pull request.
//
// The body is checked against GitHub's limit here rather than at the API, so an
// over-long turn is a local, named error instead of a 422 from a request that
// already counted.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (Comment, error) {
	if err := validateRepo(owner, repo); err != nil {
		return Comment{}, err
	}
	if number <= 0 {
		return Comment{}, fmt.Errorf("github: comment needs a pull request number, got %d", number)
	}
	if n := len([]rune(body)); n > MaxCommentRunes {
		return Comment{}, fmt.Errorf("github: comment is %d characters, over GitHub's limit of %d", n, MaxCommentRunes)
	}
	if body == "" {
		return Comment{}, errors.New("github: refusing to post an empty comment")
	}

	var out Comment
	path := repoPath(owner, repo) + "/issues/" + itoa(number) + "/comments"
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, &out); err != nil {
		return Comment{}, err
	}
	return out, nil
}

// reactionContent is GitHub's closed set of reactions. It is checked locally
// because an unknown content is a 422 from a request that already counted
// against the quota, on a call whose only job is to say "seen".
var reactionContent = map[string]bool{
	"+1": true, "-1": true, "laugh": true, "confused": true,
	"heart": true, "hooray": true, "rocket": true, "eyes": true,
}

// Acknowledge reacts to a comment.
//
// ADR-001 asks for a 🚧 on a comment that arrived while the agent was busy but
// is still going to be delivered. GitHub has no such reaction; "eyes" is the
// one that means "seen, working on it".
func (c *Client) Acknowledge(ctx context.Context, owner, repo string, commentID int64, content string) error {
	if err := validateRepo(owner, repo); err != nil {
		return err
	}
	if commentID <= 0 {
		return fmt.Errorf("github: reaction needs a comment id, got %d", commentID)
	}
	if !reactionContent[content] {
		return fmt.Errorf("github: %q is not a reaction", content)
	}
	path := repoPath(owner, repo) + "/issues/comments/" + itoa64(commentID) + "/reactions"
	return c.do(ctx, http.MethodPost, path, map[string]string{"content": content}, nil)
}

func itoa(n int) string     { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }
