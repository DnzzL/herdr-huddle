// Package repo reads and prepares the local git repository a share is opened
// from.
//
// It shells out to git rather than linking a library. The operator's git already
// knows their remotes, their worktrees and their configuration, and a
// reimplementation would disagree with it exactly where it matters most —
// worktrees, which is the layout herdr itself creates.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// DefaultRemote is the remote a share is pushed to.
const DefaultRemote = "origin"

var (
	// ErrDetachedHead is returned when the worktree is not on a branch.
	ErrDetachedHead = errors.New("repo: HEAD is detached, so there is no branch to name the share after")
	// ErrNoDefaultBranch is returned when the remote's default branch cannot be
	// determined from local refs alone.
	ErrNoDefaultBranch = errors.New("repo: cannot determine the remote's default branch")
)

// Runner runs a git command and reports its exit status. Tests substitute a
// fake so no test needs a repository on disk.
type Runner interface {
	Run(ctx context.Context, dir string, args []string) (stdout, stderr []byte, exitCode int, err error)
}

// ExecRunner runs git as a subprocess.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, dir string, args []string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// A non-zero exit is a result, not a failure to run: the caller decides
		// what it means. Only a failure to start git at all is an error here.
		return stdout.Bytes(), stderr.Bytes(), exit.ExitCode(), nil
	}
	return stdout.Bytes(), stderr.Bytes(), -1, err
}

// Error is a git command that ran and failed. It carries git's own stderr
// because that is the part an operator can act on: "not a git repository" tells
// them everything, "exit status 128" tells them nothing.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// Repo is a repository on disk.
type Repo struct {
	// Dir is the directory git commands run in. Anything inside the worktree
	// works; git resolves upward.
	Dir string
	// Runner is the command runner, defaulting to ExecRunner.
	Runner Runner
	// Remote is the remote to read and push to, defaulting to DefaultRemote.
	Remote string
}

func (r *Repo) runner() Runner {
	if r.Runner != nil {
		return r.Runner
	}
	return ExecRunner{}
}

func (r *Repo) remote() string {
	if r.Remote != "" {
		return r.Remote
	}
	return DefaultRemote
}

// run executes git and returns stdout with surrounding whitespace removed. The
// trimming matters: git output is used as arguments to the next command, and a
// trailing newline inside a ref name produces a confusing failure much later.
func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	stdout, stderr, code, err := r.runner().Run(ctx, r.Dir, args)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", &Error{Args: args, ExitCode: code, Stderr: string(stderr)}
	}
	return strings.TrimSpace(string(stdout)), nil
}

// Root returns the top level of the worktree.
func (r *Repo) Root(ctx context.Context) (string, error) {
	return r.run(ctx, "rev-parse", "--show-toplevel")
}

// CurrentBranch returns the branch the worktree is on. A detached HEAD is an
// error rather than the literal string "HEAD", because callers use this to name
// things and "herdr/HEAD" is a worse outcome than a clear failure.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", ErrDetachedHead
	}
	return out, nil
}

// DefaultBranch returns the remote's default branch, for example "main".
//
// It reads refs/remotes/<remote>/HEAD, a local symbolic ref, so it needs no
// network. It does not fall back to guessing from `git remote show`, which
// would, and it does not fall back to "main", which would be wrong on any
// repository that uses something else.
func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "symbolic-ref", "--short", "refs/remotes/"+r.remote()+"/HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDefaultBranch, err)
	}
	return strings.TrimPrefix(out, r.remote()+"/"), nil
}

// RemoteURL returns the configured URL of the remote.
func (r *Repo) RemoteURL(ctx context.Context) (string, error) {
	return r.run(ctx, "remote", "get-url", r.remote())
}

// Slug resolves the remote to an owner and repository name.
func (r *Repo) Slug(ctx context.Context) (Slug, error) {
	url, err := r.RemoteURL(ctx)
	if err != nil {
		return Slug{}, err
	}
	return ParseSlug(url)
}

// EnsureBranch makes sure the branch exists, creating it on top of base with a
// single empty commit if it does not, and reports whether it created it.
//
// This is the whole of ADR-002's idempotency rule, kept here rather than in the
// caller so there is one place to read it: a branch that exists locally, or only
// on the remote because this clone has not checked it out, is an existing share
// and is reused untouched. The pull request is the thread, and a second branch
// would split it.
//
// The commit is built with commit-tree, so the operator's worktree and HEAD are
// never touched. A share must be safe to open in the middle of unrelated work.
func (r *Repo) EnsureBranch(ctx context.Context, branch, base, message string) (bool, error) {
	if err := ValidateBranchName(branch); err != nil {
		return false, err
	}
	if err := validateRef(base); err != nil {
		return false, err
	}
	if exists, err := r.BranchExists(ctx, branch); err != nil {
		return false, err
	} else if exists {
		return false, nil
	}

	// The tree of the base, committed again with the base as its parent: a
	// commit that changes nothing, which is what makes the branch a valid pull
	// request head without inventing content.
	tree, err := r.run(ctx, "rev-parse", base+"^{tree}")
	if err != nil {
		return false, err
	}
	sha, err := r.run(ctx, "commit-tree", tree, "-p", base, "-m", message)
	if err != nil {
		return false, err
	}
	if _, err := r.run(ctx, "branch", branch, sha); err != nil {
		return false, err
	}
	return true, nil
}

// Push pushes the branch to the remote.
func (r *Repo) Push(ctx context.Context, branch string) error {
	if err := ValidateBranchName(branch); err != nil {
		return err
	}
	_, err := r.run(ctx, "push", r.remote(), branch)
	return err
}

// BranchExists reports whether the branch exists locally, or on the remote and
// simply not checked out here. Both mean the share already exists.
//
// It is separate from EnsureBranch so that a dry run can report what would
// happen without creating anything.
func (r *Repo) BranchExists(ctx context.Context, branch string) (bool, error) {
	if err := ValidateBranchName(branch); err != nil {
		return false, err
	}
	local, err := r.RefExists(ctx, "refs/heads/"+branch)
	if err != nil || local {
		return local, err
	}
	return r.RefExists(ctx, "refs/remotes/"+r.remote()+"/"+branch)
}

// RefExists asks git whether a ref resolves. A missing ref is an exit status
// rather than a failure, so only a failure to run git is an error.
func (r *Repo) RefExists(ctx context.Context, ref string) (bool, error) {
	_, _, code, err := r.runner().Run(ctx, r.Dir, []string{"rev-parse", "--verify", "--quiet", ref})
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// Slug identifies a repository on a forge.
type Slug struct {
	Host  string
	Owner string
	Name  string
}

// String returns the owner and name, as the GitHub API path uses them.
func (s Slug) String() string { return s.Owner + "/" + s.Name }

// IsGitHub reports whether the repository is on github.com. A share talks to the
// github.com API, so an enterprise host is refused rather than sent to the wrong
// endpoint.
func (s Slug) IsGitHub() bool { return s.Host == "github.com" }

// scpLike matches the shorthand git accepts for ssh remotes,
// [user@]host:path, which has no scheme to split on.
var scpLike = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9._-]+):(.+)$`)

// ParseSlug extracts the host, owner and repository name from a git remote URL.
//
// It accepts the forms git writes and people actually use: https, ssh, git and
// the scp-like shorthand. It requires exactly two path segments. A GitLab-style
// subgroup (group/subgroup/project) is refused rather than guessed at, because
// choosing the wrong segment silently would aim a pull request at the wrong
// repository.
func ParseSlug(remoteURL string) (Slug, error) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return Slug{}, errors.New("repo: empty remote url")
	}
	if strings.ContainsAny(raw, " \t\n") {
		return Slug{}, fmt.Errorf("repo: remote url %q contains whitespace", raw)
	}

	var host, path string
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:] // drop any userinfo
		}
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return Slug{}, fmt.Errorf("repo: remote url %q has no path", raw)
		}
		host, path = rest[:slash], rest[slash+1:]
	} else if m := scpLike.FindStringSubmatch(raw); m != nil {
		host, path = m[1], m[2]
	} else {
		return Slug{}, fmt.Errorf("repo: cannot parse remote url %q", raw)
	}

	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Slug{}, fmt.Errorf("repo: remote url %q is not an owner/repo path", raw)
	}
	return Slug{Host: host, Owner: parts[0], Name: parts[1]}, nil
}

// ValidateBranchName rejects names git would refuse, plus the ones it would
// accept as an option rather than as a name.
//
// The option case is the one that matters: a branch name reaches a command line,
// so "-x" is an injection site, and --upload-pack is enough to run a command.
// A dash is cheap to refuse, and no real branch starts with one.
func ValidateBranchName(name string) error {
	if err := validateRef(name); err != nil {
		return err
	}
	if name == "HEAD" {
		return errors.New("repo: HEAD is not a branch name")
	}
	return nil
}

func validateRef(ref string) error {
	switch {
	case ref == "":
		return errors.New("repo: empty ref")
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("repo: ref %q starts with a dash and would be read as an option", ref)
	case strings.HasSuffix(ref, "/"), strings.HasSuffix(ref, "."):
		return fmt.Errorf("repo: ref %q ends with an invalid character", ref)
	case strings.Contains(ref, ".."), strings.Contains(ref, "@{"):
		return fmt.Errorf("repo: ref %q contains an invalid sequence", ref)
	case strings.ContainsAny(ref, " \t\n\r~^:?*[\\"):
		return fmt.Errorf("repo: ref %q contains an invalid character", ref)
	}
	return nil
}
