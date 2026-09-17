package auth

import (
	"context"
	"errors"

	"github.com/DnzzL/herdr-huddle/internal/github"
)

// ScopesForRepo picks the narrowest OAuth scope that can open a draft PR in a
// repo, without holding a token yet.
//
// The probe is unauthenticated, so a private repo answers 404 rather than 403 —
// indistinguishable from a repo that does not exist. Both cases are treated as
// private, which asks for the broader `repo` scope: under-asking produces a
// confusing 404 later, while over-asking costs a scope the operator can see
// and decline on the consent screen.
func ScopesForRepo(ctx context.Context, c *github.Client, owner, repo string) ([]string, error) {
	private, err := c.RepoIsPrivate(ctx, owner, repo)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			return ScopesFor(true), nil
		}
		return nil, err
	}
	return ScopesFor(private), nil
}
