package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real git binary against a local bare repository. The fake
// runner in repo_test.go asserts which commands are issued; only this file can
// show that the commands are the right ones. commit-tree in particular is easy
// to get subtly wrong — a missing parent, or a tree that is not the base's —
// and the failure would be a pull request whose diff is nonsense, which no unit
// test would catch.
//
// There is no network and no GitHub here: origin is a directory.

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// A fixed identity so the tests do not depend on the machine's config, and
	// so commit-tree has a committer to record.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitFixture returns a worktree whose origin is a local bare repository, with
// one commit on main pushed, and origin/HEAD pointing at it.
func newGitFixture(t *testing.T) (work, remote string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")

	gitRun(t, "", "init", "--bare", "-b", "main", remote)
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "initial")
	gitRun(t, work, "push", "--set-upstream", "origin", "main")
	// Refuse any accidental network use: a local path is the only remote.
	gitRun(t, work, "remote", "set-head", "origin", "main")
	return work, remote
}

func TestRepo_EndToEndAgainstALocalRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the git binary")
	}
	work, remote := newGitFixture(t)
	ctx := context.Background()
	r := &Repo{Dir: work}

	def, err := r.DefaultBranch(ctx)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if def != "main" {
		t.Fatalf("DefaultBranch = %q, want main", def)
	}
	slug, err := r.Slug(ctx)
	if err == nil {
		// A local path has no owner/name. Refusing it is the point: a share
		// must not be attempted against something that is not a GitHub remote.
		t.Errorf("Slug = %v on a filesystem remote, want an error", slug)
	}
	if !strings.Contains(err.Error(), "cannot parse remote url") {
		t.Errorf("err = %v, want it to say the remote url cannot be parsed", err)
	}

	// The initial push left origin/main one commit behind the local main only if
	// something else moved; capture both sides to compare against later.
	baseSHA := gitRun(t, work, "rev-parse", "origin/main")

	created, err := r.EnsureBranch(ctx, "herdr/x", "origin/main",
		"open the thread\n\nthe pull request is where the conversation goes")
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}

	branchSHA := gitRun(t, work, "rev-parse", "refs/heads/herdr/x")
	if branchSHA == baseSHA {
		t.Error("the branch points at the base, so there is no commit for a pull request to be opened from")
	}
	// Exactly one parent, and it is the base: an empty commit on top.
	if parent := gitRun(t, work, "rev-parse", "herdr/x^"); parent != baseSHA {
		t.Errorf("parent = %s, want the base %s", parent, baseSHA)
	}
	// The tree is the base's tree, so the commit changes nothing. This is what
	// makes the pull request start empty.
	tree := gitRun(t, work, "rev-parse", "herdr/x^{tree}")
	baseTree := gitRun(t, work, "rev-parse", "origin/main^{tree}")
	if tree != baseTree {
		t.Error("the empty commit has a different tree from its parent")
	}
	// Exactly one parent, not two: a merge would make the diff unreadable.
	if parents := strings.Fields(gitRun(t, work, "rev-list", "--parents", "-n", "1", "herdr/x")); len(parents) != 2 {
		t.Errorf("commit has %d parents, want exactly 1", len(parents)-1)
	}
	// The message is recorded verbatim; what it says is share's business.
	if msg := gitRun(t, work, "log", "-1", "--format=%s", "herdr/x"); msg != "open the thread" {
		t.Errorf("subject = %q", msg)
	}
	if body := gitRun(t, work, "log", "-1", "--format=%b", "herdr/x"); strings.TrimSpace(body) != "the pull request is where the conversation goes" {
		t.Errorf("body = %q, want the multi-line message preserved", body)
	}

	// The whole reason commit-tree is used: the operator's worktree is untouched.
	// A share must be safe to open in the middle of unrelated work.
	if head := gitRun(t, work, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		t.Errorf("HEAD is on %q, want main — the share moved the worktree", head)
	}
	if status := gitRun(t, work, "status", "--porcelain"); status != "" {
		t.Errorf("worktree is dirty after a share:\n%s", status)
	}

	// Re-running must reuse the branch untouched, which is ADR-002's idempotency
	// rule. A new commit here would mean a second share silently rewrote history.
	created2, err := r.EnsureBranch(ctx, "herdr/x", "origin/main", "open the thread")
	if err != nil {
		t.Fatalf("second EnsureBranch: %v", err)
	}
	if created2 {
		t.Error("created = true on the second call, want the branch reused")
	}
	if again := gitRun(t, work, "rev-parse", "refs/heads/herdr/x"); again != branchSHA {
		t.Errorf("the branch moved on a second share: %s -> %s", branchSHA, again)
	}

	// And the push actually puts it on the remote.
	if err := r.Push(ctx, "herdr/x"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	onRemote := gitRun(t, "", "--git-dir", remote, "rev-parse", "refs/heads/herdr/x")
	if onRemote != branchSHA {
		t.Errorf("remote has %s, want %s", onRemote, branchSHA)
	}
}

// A branch that exists on the remote but not locally is an existing share, which
// happens whenever a second clone or a fresh worktree is used.
func TestRepo_BranchExistsFindsRemoteOnlyBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the git binary")
	}
	work, _ := newGitFixture(t)
	ctx := context.Background()
	r := &Repo{Dir: work}

	if exists, err := r.BranchExists(ctx, "herdr/never-made"); err != nil || exists {
		t.Fatalf("exists = %v, err = %v, want false and nil", exists, err)
	}

	// Create it locally and push, then delete the local ref so only the remote
	// knows about it.
	if _, err := r.EnsureBranch(ctx, "herdr/elsewhere", "origin/main", "msg"); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(ctx, "herdr/elsewhere"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "update-ref", "-d", "refs/heads/herdr/elsewhere")
	gitRun(t, work, "fetch", "origin")

	exists, err := r.BranchExists(ctx, "herdr/elsewhere")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Error("exists = false for a branch that is on the remote")
	}

	// And EnsureBranch must not duplicate it.
	created, err := r.EnsureBranch(ctx, "herdr/elsewhere", "origin/main", "msg")
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if created {
		t.Error("created = true for a branch already on the remote, which would split the thread")
	}
}

// The slug comes from the real remote url, not a string the test supplied. Pushing
// is what has to stay local, so only the url is swapped here.
func TestRepo_SlugFromAGitHubRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the git binary")
	}
	work, _ := newGitFixture(t)
	gitRun(t, work, "remote", "set-url", "origin", "git@github.com:acme/demo.git")

	slug, err := (&Repo{Dir: work}).Slug(context.Background())
	if err != nil {
		t.Fatalf("Slug: %v", err)
	}
	if slug.Owner != "acme" || slug.Name != "demo" || !slug.IsGitHub() {
		t.Errorf("Slug = %+v, want acme/demo on github.com", slug)
	}
}

// Detached HEAD in a real repository, not a fake returning the string "HEAD".
func TestRepo_DetachedHead(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the git binary")
	}
	work, _ := newGitFixture(t)
	gitRun(t, work, "checkout", "--detach", "HEAD")

	_, err := (&Repo{Dir: work}).CurrentBranch(context.Background())
	if err == nil {
		t.Fatal("want an error for a detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("err = %v, want it to explain the problem", err)
	}
}

// A branch name that reaches git as an option is an injection site, so it must be
// refused before any command runs, not merely escaped.
func TestRepo_RefusesOptionShapedBranchNames(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the git binary")
	}
	work, _ := newGitFixture(t)
	r := &Repo{Dir: work}

	if _, err := r.EnsureBranch(context.Background(), "--upload-pack=touch /tmp/pwned", "origin/main", "m"); err == nil {
		t.Fatal("EnsureBranch accepted a branch name that git would read as an option")
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("something executed")
	}
}
