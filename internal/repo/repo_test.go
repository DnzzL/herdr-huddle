package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner stands in for the git binary. It records what was asked for so the
// tests can assert on the exact command, not just on the result: the argument
// list is the interface here, and getting it subtly wrong is the normal failure.
type fakeRunner struct {
	calls []string
	fn    func(args []string) (stdout, stderr string, exitCode int)
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.fn == nil {
		return nil, nil, 0, nil
	}
	stdout, stderr, code := f.fn(args)
	return []byte(stdout), []byte(stderr), code, nil
}

// called reports whether any recorded command contains every given fragment.
func (f *fakeRunner) called(fragments ...string) bool {
	for _, c := range f.calls {
		all := true
		for _, frag := range fragments {
			if !strings.Contains(c, frag) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func newTestRepo(fn func(args []string) (string, string, int)) (*Repo, *fakeRunner) {
	r := &fakeRunner{fn: fn}
	return &Repo{Dir: "/work/tree", Runner: r}, r
}

func TestParseSlug(t *testing.T) {
	good := []struct {
		url         string
		owner, name string
	}{
		{"https://github.com/DnzzL/herdr-huddle.git", "DnzzL", "herdr-huddle"},
		{"https://github.com/DnzzL/herdr-huddle", "DnzzL", "herdr-huddle"},
		{"https://github.com/DnzzL/herdr-huddle/", "DnzzL", "herdr-huddle"},
		{"git@github.com:DnzzL/herdr-huddle.git", "DnzzL", "herdr-huddle"},
		{"git@github.com:DnzzL/herdr-huddle", "DnzzL", "herdr-huddle"},
		{"ssh://git@github.com/DnzzL/herdr-huddle.git", "DnzzL", "herdr-huddle"},
		{"git://github.com/DnzzL/herdr-huddle.git", "DnzzL", "herdr-huddle"},
		{"https://github.com/DnzzL/herdr-huddle.git\n", "DnzzL", "herdr-huddle"},
		// A company org on GitHub Enterprise. The slug is the same shape; only
		// the API host differs, and that is the caller's problem to reject.
		{"git@github.example.com:acme/widget.git", "acme", "widget"},
	}
	for _, tc := range good {
		t.Run(tc.url, func(t *testing.T) {
			slug, err := ParseSlug(tc.url)
			if err != nil {
				t.Fatalf("ParseSlug(%q): %v", tc.url, err)
			}
			if slug.Owner != tc.owner || slug.Name != tc.name {
				t.Errorf("got %s/%s, want %s/%s", slug.Owner, slug.Name, tc.owner, tc.name)
			}
			if got := slug.String(); got != tc.owner+"/"+tc.name {
				t.Errorf("String() = %q", got)
			}
		})
	}

	bad := []string{
		"",
		"   ",
		"not a url",
		"https://github.com/onlyowner",
		// A subgroup, as GitLab allows and GitHub does not. Guessing which
		// segment is the repository would be worse than refusing.
		"git@gitlab.com:group/subgroup/project.git",
		"/local/path/to/repo",
	}
	for _, url := range bad {
		t.Run("reject "+url, func(t *testing.T) {
			if _, err := ParseSlug(url); err == nil {
				t.Errorf("ParseSlug(%q) succeeded, want an error", url)
			}
		})
	}
}

func TestRoot_UsesShowToplevel(t *testing.T) {
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		return "/work/tree\n", "", 0
	})
	got, err := r.Root(context.Background())
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != "/work/tree" {
		t.Errorf("Root = %q", got)
	}
	if !fake.called("rev-parse", "--show-toplevel") {
		t.Errorf("calls = %v", fake.calls)
	}
}

func TestCurrentBranch(t *testing.T) {
	r, _ := newTestRepo(func(args []string) (string, string, int) {
		return "improve-pane\n", "", 0
	})
	got, err := r.CurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if got != "improve-pane" {
		t.Errorf("CurrentBranch = %q", got)
	}
}

// A detached HEAD makes `rev-parse --abbrev-ref HEAD` print the literal string
// "HEAD". Treating that as a branch name would produce a share called
// herdr/HEAD, so it is an error the caller has to handle.
func TestCurrentBranch_DetachedHeadIsNotABranch(t *testing.T) {
	r, _ := newTestRepo(func(args []string) (string, string, int) {
		return "HEAD\n", "", 0
	})
	if _, err := r.CurrentBranch(context.Background()); !errors.Is(err, ErrDetachedHead) {
		t.Fatalf("err = %v, want ErrDetachedHead", err)
	}
}

// refs/remotes/origin/HEAD is a local symbolic ref, so resolving the default
// branch must not reach the network. If it ever grows a fetch, this test is
// what fails.
func TestDefaultBranch_ReadsLocalRemoteHeadOnly(t *testing.T) {
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		return "origin/trunk\n", "", 0
	})
	got, err := r.DefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk (the origin/ prefix stripped)", got)
	}
	if !fake.called("symbolic-ref", "--short", "refs/remotes/origin/HEAD") {
		t.Errorf("calls = %v", fake.calls)
	}
	for _, c := range fake.calls {
		if strings.Contains(c, "fetch") || strings.Contains(c, "ls-remote") {
			t.Errorf("DefaultBranch reached for the network: %q", c)
		}
	}
}

func TestDefaultBranch_UnresolvableIsRecognisable(t *testing.T) {
	r, _ := newTestRepo(func(args []string) (string, string, int) {
		return "", "fatal: ref refs/remotes/origin/HEAD is not a symbolic ref", 1
	})
	_, err := r.DefaultBranch(context.Background())
	if err == nil {
		t.Fatal("want an error when origin/HEAD cannot be resolved")
	}
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Errorf("err = %v, want it to wrap ErrNoDefaultBranch", err)
	}
}

// Reuse is the whole idempotency rule from ADR-002, so the assertion is about
// what did *not* happen: no commit, no branch, nothing created.
func TestEnsureBranch_ReusesExistingLocalBranch(t *testing.T) {
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "refs/heads/herdr/improve-pane") {
			return "abc123\n", "", 0
		}
		t.Errorf("unexpected command during reuse: %v", args)
		return "", "", 1
	})
	created, err := r.EnsureBranch(context.Background(), "herdr/improve-pane", "origin/main", "msg")
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if created {
		t.Error("created = true, want false when the branch already exists")
	}
	for _, c := range fake.calls {
		if strings.Contains(c, "commit-tree") || strings.HasPrefix(c, "branch ") {
			t.Errorf("existing branch was recreated: %q", c)
		}
	}
}

// A branch that exists only on the remote is still an existing share: the local
// clone simply has not checked it out. Creating a second one would split the PR.
func TestEnsureBranch_ReusesBranchThatExistsOnlyOnTheRemote(t *testing.T) {
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "refs/remotes/origin/herdr/improve-pane") {
			return "abc123\n", "", 0
		}
		return "", "", 1
	})
	created, err := r.EnsureBranch(context.Background(), "herdr/improve-pane", "origin/main", "msg")
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if created {
		t.Error("created = true, want false for a branch present on the remote")
	}
	if fake.called("commit-tree") {
		t.Error("an empty commit was made for a branch that already exists")
	}
}

// The empty commit is built with commit-tree rather than by checking out and
// running `git commit --allow-empty`, so the operator's worktree and HEAD are
// untouched. Asserting the exact plumbing is the point: it is what makes that
// guarantee true.
func TestEnsureBranch_CreatesAnEmptyCommitOnTheBase(t *testing.T) {
	const sha = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "refs/heads/") || strings.Contains(joined, "refs/remotes/"):
			return "", "", 1 // does not exist yet
		case strings.Contains(joined, "rev-parse") && strings.Contains(joined, "origin/main^{tree}"):
			return "tree4f2a\n", "", 0
		case strings.Contains(joined, "commit-tree"):
			return sha + "\n", "", 0
		case strings.HasPrefix(joined, "branch "):
			return "", "", 0
		}
		t.Errorf("unexpected command: %v", args)
		return "", "", 1
	})

	created, err := r.EnsureBranch(context.Background(), "herdr/improve-pane", "origin/main", "open the thread")
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a new branch")
	}
	want := []string{
		"rev-parse origin/main^{tree}",
		"commit-tree tree4f2a -p origin/main -m open the thread",
		"branch herdr/improve-pane " + sha,
	}
	for _, w := range want {
		if !fake.called(w) {
			t.Errorf("missing command %q in %v", w, fake.calls)
		}
	}
	// Nothing may move the worktree.
	for _, c := range fake.calls {
		if strings.Contains(c, "checkout") || strings.Contains(c, "switch") {
			t.Errorf("EnsureBranch moved the worktree: %q", c)
		}
	}
}

// The SHA is read off stdout and used as an argument to the next command, so a
// trailing newline would end up inside a ref. This has bitten the real git CLI
// integration more than once.
func TestEnsureBranch_TrimsCommitTreeOutput(t *testing.T) {
	const sha = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "refs/heads/") || strings.Contains(joined, "refs/remotes/"):
			return "", "", 1
		case strings.Contains(joined, "rev-parse"):
			return "tree4f2a\n", "", 0
		case strings.Contains(joined, "commit-tree"):
			return sha + "\n\n", "", 0
		}
		return "", "", 0
	})
	if _, err := r.EnsureBranch(context.Background(), "b", "origin/main", "m"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if !fake.called("branch b " + sha) {
		t.Errorf("branch was created with an untrimmed sha: %v", fake.calls)
	}
}

func TestRemoteURL(t *testing.T) {
	r, fake := newTestRepo(func(args []string) (string, string, int) {
		return "git@github.com:DnzzL/herdr-huddle.git\n", "", 0
	})
	got, err := r.RemoteURL(context.Background())
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if got != "git@github.com:DnzzL/herdr-huddle.git" {
		t.Errorf("RemoteURL = %q", got)
	}
	if !fake.called("remote", "get-url", "origin") {
		t.Errorf("calls = %v", fake.calls)
	}
}

// A failing git command must surface git's own words. "exit status 128" tells
// the operator nothing; "fatal: not a git repository" tells them everything.
func TestFailuresCarryStderr(t *testing.T) {
	r, _ := newTestRepo(func(args []string) (string, string, int) {
		return "", "fatal: not a git repository (or any parent up to mount point /)", 128
	})
	_, err := r.Root(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("err = %v, want it to quote git's stderr", err)
	}
	if !strings.Contains(err.Error(), "rev-parse") {
		t.Errorf("err = %v, want it to name the command that failed", err)
	}
}

// The package must not accept a branch name that would be interpreted as an
// option or as a path by git.
func TestBranchNameValidation(t *testing.T) {
	for _, bad := range []string{"", "-x", "--upload-pack=evil", "a b", "a..b", "a~b", "a^b", "a:b", "a\\b", "a\nb"} {
		t.Run(bad, func(t *testing.T) {
			if err := ValidateBranchName(bad); err == nil {
				t.Errorf("ValidateBranchName(%q) accepted it", bad)
			}
		})
	}
	for _, ok := range []string{"herdr/improve-pane", "main", "feat/x_1.2", "herdr/herdr-huddle"} {
		t.Run("ok "+ok, func(t *testing.T) {
			if err := ValidateBranchName(ok); err != nil {
				t.Errorf("ValidateBranchName(%q): %v", ok, err)
			}
		})
	}
}
