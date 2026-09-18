// Package share opens the GitHub draft pull request that is a shared thread.
//
// ADR-001 defines what a share is and ADR-002 the mechanics; this package is the
// orchestration that implements them, over two narrow interfaces so that
// everything except the network calls can be tested without a repository or a
// token.
package share

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/repo"
)

// BranchPrefix is the namespace a share's branch lives in, so shares are
// recognisable in a branch list and cannot collide with work branches.
const BranchPrefix = "herdr/"

// Git is the local repository a share is opened from. *repo.Repo implements it.
type Git interface {
	Root(ctx context.Context) (string, error)
	CurrentBranch(ctx context.Context) (string, error)
	DefaultBranch(ctx context.Context) (string, error)
	Slug(ctx context.Context) (repo.Slug, error)
	BranchExists(ctx context.Context, branch string) (bool, error)
	RefExists(ctx context.Context, ref string) (bool, error)
	EnsureBranch(ctx context.Context, branch, base, message string) (bool, error)
	Push(ctx context.Context, branch string) error
}

// Forge is the GitHub endpoints a share uses. *github.Client implements it.
type Forge interface {
	Viewer(ctx context.Context) (string, error)
	Repository(ctx context.Context, owner, repo string) (github.Repository, error)
	FindOpenPullRequests(ctx context.Context, owner, repo, headOwner, branch string) ([]github.PullRequest, error)
	CreatePullRequest(ctx context.Context, owner, repo string, req github.CreatePullRequestRequest) (github.PullRequest, error)
	AddCollaborator(ctx context.Context, owner, repo, login string) error
}

// Request is what the caller asks for. Every field is optional except the
// interfaces: a bare share derives its own slug and base, which is what makes
// the common case one word on a command line.
type Request struct {
	// Slug overrides the derived share slug.
	Slug string
	// Base overrides the ref the empty commit is cut from. A branch name
	// ("main") is resolved against the remote first, so the pull request's base
	// is the branch the operator means rather than a same-named local one.
	Base string
	// Invite is the list of GitHub logins to give read access. They become the
	// per-share allowlist of comments permitted to drive the agent.
	Invite []string
	// Title and Body override the generated pull request text.
	Title string
	Body  string
	// DryRun reports what would happen and changes nothing: no branch, no push,
	// no request.
	DryRun bool
}

// Result is what happened, in enough detail for the caller to print it and to
// record the share for the poller.
type Result struct {
	Repo   repo.Slug
	Slug   string
	Branch string
	// Base is the branch name in the repository, which is what the pull request
	// is opened against.
	Base string
	// BaseRef is the ref the empty commit was cut from, which may be a
	// remote-tracking ref ("origin/main") or, when the default branch could not
	// be resolved, a bare "HEAD".
	BaseRef string
	// BranchCreated is true when this call created the branch, and false when it
	// reused one that already existed.
	BranchCreated bool
	// Reused is true when an open pull request already existed for the branch.
	Reused bool
	// PullRequest is zero-valued on a dry run.
	PullRequest github.PullRequest
	Pushed      bool
	DryRun      bool
	Invited     []string
	// Allowlist is every login whose comments may drive the agent: the operator
	// themselves, then each successful invitation. The operator is on it because
	// otherwise the person who owns the agent could not steer it from the pull
	// request, which is the loop the whole product exists for.
	Allowlist []string
	// Warnings are things the operator should see but that did not stop the
	// share.
	Warnings []string
}

// Open opens the share, or reports the one that already exists.
//
// The order is deliberate: the branch is prepared before anything is asked of
// GitHub, so a share that fails leaves a local branch the operator can inspect
// rather than a half-created remote state.
func Open(ctx context.Context, git Git, forge Forge, req Request) (Result, error) {
	var res Result
	res.DryRun = req.DryRun

	root, err := git.Root(ctx)
	if err != nil {
		return res, err
	}
	slug, err := git.Slug(ctx)
	if err != nil {
		return res, err
	}
	// A share talks to the github.com API. An enterprise host would be sent to
	// the wrong endpoint, so it is refused here rather than half-working.
	if !slug.IsGitHub() {
		return res, fmt.Errorf("share: %s is not on github.com, and this build only talks to the github.com API", slug.Host)
	}
	res.Repo = slug

	defaultBranch, err := resolveBase(ctx, git, req.Base)
	if err != nil {
		return res, err
	}
	res.Base, res.BaseRef = defaultBranch.name, defaultBranch.ref
	if res.Base == "" {
		res.Warnings = append(res.Warnings, "could not determine the remote's default branch from local refs, "+
			"so the empty commit is cut from the local HEAD and the pull request diff may include "+
			"commits upstream already has")
	}

	branch := req.Slug
	if branch == "" {
		current, err := git.CurrentBranch(ctx)
		if err != nil {
			return res, err
		}
		branch = DeriveSlug(current, defaultBranch.name, path.Base(root))
	} else if normalised := slugify(branch); normalised != branch {
		// An explicit name is not quietly rewritten: the operator asked for this
		// ref, and finding a share under a different name than they typed would
		// be worse than being told.
		return res, fmt.Errorf("share: --slug %q is not usable as a branch name; try %q", req.Slug, normalised)
	}
	res.Slug = branch
	res.Branch = BranchPrefix + branch
	if err := repo.ValidateBranchName(res.Branch); err != nil {
		return res, err
	}

	// The base has to be settled before anything is created. A repository
	// configured with `git remote add` and `git fetch` has no origin/HEAD for
	// the local answer to come from, and GitHub knows the same fact; without it
	// the pull request would be rejected with a 422 that says nothing useful,
	// after the branch had already been pushed.
	if res.Base == "" && !req.DryRun {
		metadata, err := forge.Repository(ctx, slug.Owner, slug.Name)
		if err != nil {
			return res, fmt.Errorf("share: could not determine the base branch: %w (pass --base)", err)
		}
		if metadata.DefaultBranch == "" {
			return res, fmt.Errorf("share: GitHub reports no default branch for %s; pass --base <branch>", slug)
		}
		res.Base = metadata.DefaultBranch
		res.Warnings = append(res.Warnings, "using "+metadata.DefaultBranch+" as the pull request base, from GitHub")
	}

	if req.DryRun {
		exists, err := git.BranchExists(ctx, res.Branch)
		if err != nil {
			return res, err
		}
		res.BranchCreated = !exists
		// The pull request is not looked up on a dry run, because reporting
		// "an open pull request exists" without a request would mean either a
		// request during a dry run or a claim that might be wrong.
		return res, nil
	}

	// The check that the token works, and the last thing before anything is
	// written. It happens here and not later because a share that fails on an
	// unusable token must not leave a pushed branch behind: every following
	// request needs the same token, so there is no state in which continuing past
	// this point works.
	operator, err := forge.Viewer(ctx)
	if err != nil {
		return res, fmt.Errorf("share: could not read your GitHub login: %w", err)
	}
	if operator != "" {
		// First on the allowlist, because otherwise the person who owns the
		// agent could not steer it from the pull request.
		res.Allowlist = append(res.Allowlist, operator)
	}

	res.BranchCreated, err = git.EnsureBranch(ctx, res.Branch, res.BaseRef, commitMessage(res.Slug))
	if err != nil {
		return res, err
	}
	if err := git.Push(ctx, res.Branch); err != nil {
		return res, err
	}
	res.Pushed = true
	// The head filter takes the owner of the repository holding the branch. The
	// branch was pushed to origin, so that is the repository's owner, not
	// whoever is authenticated.
	existing, err := forge.FindOpenPullRequests(ctx, slug.Owner, slug.Name, slug.Owner, res.Branch)
	if err != nil {
		return res, err
	}
	if len(existing) > 0 {
		res.PullRequest = existing[0]
		res.Reused = true
		if len(existing) > 1 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%d open pull requests share the branch %s; using #%d and leaving the others alone",
				len(existing), res.Branch, existing[0].Number))
		}
	} else {
		pr, err := forge.CreatePullRequest(ctx, slug.Owner, slug.Name, github.CreatePullRequestRequest{
			Title: title(req.Title, res.Slug),
			Head:  res.Branch,
			Base:  res.Base,
			Body:  body(req.Body, res.Slug),
			// Always a draft: ADR-001 opens a share before any code exists.
			Draft: true,
		})
		if err != nil {
			return res, err
		}
		res.PullRequest = pr
	}

	for _, login := range req.Invite {
		login = strings.TrimPrefix(strings.TrimSpace(login), "@")
		if login == "" {
			continue
		}
		if err := forge.AddCollaborator(ctx, slug.Owner, slug.Name, login); err != nil {
			// The thread exists at this point, so a failed invitation is
			// reported and the share is kept: an operator who lacks admin on a
			// work org (ADR-001) must still get their pull request.
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not invite %s: %v", login, err))
			continue
		}
		res.Invited = append(res.Invited, login)
		res.Allowlist = append(res.Allowlist, login)
	}

	return res, nil
}

// base is the branch a share is opened against, as both a name and a ref.
type base struct {
	// name is the branch name in the repository, for the pull request.
	name string
	// ref is what the empty commit is cut from.
	ref string
}

// resolveBase decides what the share is opened against.
//
// An explicit --base is taken at its word. Otherwise the remote's default branch
// is resolved from local refs and the empty commit is cut from the
// remote-tracking ref, so the diff means "this workstream against upstream". When
// that cannot be resolved the name is left empty for the caller to fill in from
// GitHub, because a guessed "main" would silently produce a diff against the
// wrong branch on every repository that uses something else.
func resolveBase(ctx context.Context, git Git, override string) (base, error) {
	if override != "" {
		if err := repo.ValidateBranchName(override); err != nil {
			return base{}, err
		}
		// Prefer the remote-tracking ref so the diff is against upstream, and
		// fall back to the name itself for a base that exists only locally.
		ref := "origin/" + override
		if onRemote, err := git.RefExists(ctx, ref); err == nil && !onRemote {
			ref = override
		}
		return base{name: override, ref: ref}, nil
	}

	name, err := git.DefaultBranch(ctx)
	if err != nil {
		return base{ref: "HEAD"}, nil
	}
	return base{name: name, ref: "origin/" + name}, nil
}

// commitMessage is the message of the single empty commit a share is built on.
func commitMessage(slug string) string {
	// A body as well as a subject: this commit is the first thing a collaborator
	// sees in the log, and an empty commit with no explanation reads as a mistake.
	return "herdr-huddle: open a shared thread for " + slug + "\n\n" +
		"This commit is empty on purpose. The pull request is the thread:\n" +
		"the conversation goes in its body and the work goes on this branch."
}

// title is the pull request title.
func title(override, slug string) string {
	if override != "" {
		return override
	}
	return "Plan: " + slug
}

// body is the pull request body, used only until the first transcript sync
// replaces it.
func body(override, slug string) string {
	if override != "" {
		return override
	}
	return fmt.Sprintf(
		"Shared planning thread for `%s`, opened before any code exists.\n\n"+
			"herdr-huddle keeps the agent's conversation in this body and the work in the diff. "+
			"Comments are injected into the agent only when they start with `/agent`, come from a "+
			"collaborator, and are on this share's allowlist.", slug)
}

// anonymousBranchNames are branch names that say nothing about the work. The
// remote's default branch is the real answer, and DeriveSlug is given it, but
// these stand in when it cannot be resolved: `herdr/main` names no workstream,
// and avoiding it is the specific case ADR-002 calls out.
var anonymousBranchNames = map[string]bool{
	"main": true, "master": true, "trunk": true, "develop": true, "development": true, "head": true,
}

// DeriveSlug derives the share slug from the branch, the repository's default
// branch and the directory name.
//
// A branch names the workstream, so it is the first choice. On the default
// branch there is no workstream name to borrow, and `herdr/main` would be a name
// that says nothing; the directory says more, and `herdr/docket` is at least
// recognisable.
func DeriveSlug(branch, defaultBranch, dirName string) string {
	if branch != "" && branch != defaultBranch && !anonymousBranchNames[strings.ToLower(branch)] {
		if s := slugify(branch); s != "" {
			return s
		}
	}
	if s := slugify(dirName); s != "" {
		return s
	}
	// Never empty: the branch is "herdr/" plus this, and "herdr/" is invalid.
	return "share"
}

// slugify reduces a name to what is safe in a git ref: lower case, runs of
// anything else collapsed to a single hyphen, no leading or trailing hyphen.
func slugify(s string) string {
	var out strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			out.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(out.String(), "-")
}
