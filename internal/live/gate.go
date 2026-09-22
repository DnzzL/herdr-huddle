package live

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/DnzzL/herdr-huddle/internal/github"
)

// Verifier resolves a GitHub token to the login it belongs to.
type Verifier interface {
	Login(ctx context.Context, token string) (string, error)
}

// GitHubVerifier answers from GitHub's own /user endpoint — the only source
// that can say whose token this is. Nothing this side asserts about a
// connection is worth anything until GitHub has confirmed it.
type GitHubVerifier struct {
	// BaseURL is injectable for tests, as everywhere else in this repo.
	BaseURL string
}

func (v GitHubVerifier) Login(ctx context.Context, token string) (string, error) {
	return (&github.Client{Token: token, BaseURL: v.BaseURL}).Viewer(ctx)
}

// Gate decides who may join a live share.
//
// ADR-006 makes this the entire boundary: the tunnel in front of the stream
// cannot carry Cloudflare Access, so a public endpoint is exactly as open as
// this check. It therefore runs before the first frame is observed, and an
// empty allowlist refuses everyone — the same rule the comment path applies.
type Gate struct {
	// Verify asks GitHub whose token is presented.
	Verify Verifier
	// Allowlist is the share's logins, read once when the server starts.
	Allowlist []string
}

// ErrNoToken is refused before GitHub is contacted at all.
var ErrNoToken = errors.New("live: no GitHub token was presented")

// Authorize checks a joiner's token and returns the login it proved.
//
// The errors are written for the joiner, not the log: they are the only
// explanation that person will ever get, and "you are not on this share's
// allowlist" is actionable where silence is not.
func (g *Gate) Authorize(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrNoToken
	}
	if len(g.Allowlist) == 0 {
		return "", errors.New("live: this share has an empty allowlist, so nobody may join")
	}
	if g.Verify == nil {
		return "", errors.New("live: the gate has no verifier, so nobody may join")
	}
	login, err := g.Verify.Login(ctx, token)
	if err != nil {
		return "", fmt.Errorf("live: GitHub could not confirm the token: %w", err)
	}
	for _, allowed := range g.Allowlist {
		if strings.EqualFold(allowed, login) {
			return login, nil
		}
	}
	return "", fmt.Errorf("live: @%s is not on this share's allowlist", login)
}
