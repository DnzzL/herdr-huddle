package share

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/repo"
)

// fakeGit records what the orchestration asked for, because the order of these
// calls is the contract: the branch has to exist before anything is asked of
// GitHub, so a failure leaves something inspectable rather than a half-created
// remote state.
type fakeGit struct {
	root, branch, defaultBranch string
	slug                        repo.Slug
	branchExists                bool
	refs                        map[string]bool
	calls                       []string
	ensureErr                   error
	pushErr                     error
	defaultErr                  error
	pushed                      []string
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		root:          "/work/tree",
		branch:        "improve-pane",
		defaultBranch: "main",
		slug:          repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"},
		refs:          map[string]bool{"origin/main": true},
	}
}

func (f *fakeGit) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeGit) Root(context.Context) (string, error) {
	f.record("root")
	return f.root, nil
}

func (f *fakeGit) CurrentBranch(context.Context) (string, error) {
	f.record("current-branch")
	return f.branch, nil
}

func (f *fakeGit) DefaultBranch(context.Context) (string, error) {
	f.record("default-branch")
	if f.defaultErr != nil {
		return "", f.defaultErr
	}
	return f.defaultBranch, nil
}

func (f *fakeGit) Slug(context.Context) (repo.Slug, error) {
	f.record("slug")
	return f.slug, nil
}

func (f *fakeGit) BranchExists(_ context.Context, branch string) (bool, error) {
	f.record("branch-exists " + branch)
	return f.branchExists, nil
}

func (f *fakeGit) RefExists(_ context.Context, ref string) (bool, error) {
	f.record("ref-exists " + ref)
	return f.refs[ref], nil
}

func (f *fakeGit) EnsureBranch(_ context.Context, branch, baseRef, message string) (bool, error) {
	f.record("ensure-branch " + branch + " from " + baseRef)
	if f.ensureErr != nil {
		return false, f.ensureErr
	}
	if f.branchExists {
		return false, nil
	}
	return true, nil
}

func (f *fakeGit) Push(_ context.Context, branch string) error {
	f.record("push " + branch)
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = append(f.pushed, branch)
	return nil
}

// fakeForge records requests and answers them from a fixed table.
type fakeForge struct {
	repo          github.Repository
	repoErr       error
	viewer        string
	viewerErr     error
	existing      []github.PullRequest
	createErr     error
	collabErr     error
	hasAccess     bool
	accessErr     error
	creates       []github.CreatePullRequestRequest
	collabs       []string
	findCalls     int
	lastHeadOwner string
}

func (f *fakeForge) Repository(context.Context, string, string) (github.Repository, error) {
	if f.repoErr != nil {
		return github.Repository{}, f.repoErr
	}
	return f.repo, nil
}

func (f *fakeForge) Viewer(context.Context) (string, error) {
	if f.viewerErr != nil {
		return "", f.viewerErr
	}
	return f.viewer, nil
}

func (f *fakeForge) FindOpenPullRequests(_ context.Context, _, _, headOwner, _ string) ([]github.PullRequest, error) {
	f.findCalls++
	f.lastHeadOwner = headOwner
	return f.existing, nil
}

func (f *fakeForge) CreatePullRequest(_ context.Context, _, _ string, req github.CreatePullRequestRequest) (github.PullRequest, error) {
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		return github.PullRequest{}, f.createErr
	}
	return github.PullRequest{Number: 42, HTMLURL: "https://github.com/acme/demo/pull/42", Draft: true, State: "open"}, nil
}

func (f *fakeForge) AddCollaborator(_ context.Context, _, _, login string) error {
	if f.collabErr != nil {
		return f.collabErr
	}
	f.collabs = append(f.collabs, login)
	return nil
}

func (f *fakeForge) HasAccess(_ context.Context, _, _, _ string) (bool, error) {
	if f.accessErr != nil {
		return false, f.accessErr
	}
	return f.hasAccess, nil
}

func TestDeriveSlug(t *testing.T) {
	tests := []struct {
		name          string
		branch        string
		defaultBranch string
		dirName       string
		want          string
	}{
		{"a work branch names the workstream", "improve-pane", "main", "herdr-huddle", "improve-pane"},
		{"on the default branch, the directory is more use than herdr/main", "main", "main", "herdr-huddle", "herdr-huddle"},
		{"a master-based repository is the same case", "master", "master", "docket", "docket"},
		{"slashes in a branch become hyphens", "feat/Some_Thing", "main", "x", "feat-some-thing"},
		{"runs of punctuation collapse", "user/ISSUE-42--fix", "main", "x", "user-issue-42-fix"},
		{"a detached head has no name to use", "HEAD", "main", "herdr-huddle", "herdr-huddle"},
		// The default branch could not be resolved, so there is no way to know
		// whether the current branch is it. A name that says nothing is still a
		// name that says nothing: herdr/main is a name that names no workstream.
		{"unresolved default branch, on main", "main", "", "herdr-huddle", "herdr-huddle"},
		{"unresolved default branch, on master", "master", "", "docket", "docket"},
		{"unresolved default branch, on trunk", "trunk", "", "docket", "docket"},
		// A work branch still names the workstream even with no default branch.
		{"unresolved default branch, on a work branch", "topic", "", "docket", "topic"},
		{"case is irrelevant when deciding if a name says anything", "Main", "", "docket", "docket"},
		{"a name with nothing usable in it falls back", "main", "main", "", "share"},
		{"case is folded", "ImprovePane", "main", "x", "improvepane"},
		{"leading and trailing punctuation is dropped", "/weird/", "main", "x", "weird"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveSlug(tt.branch, tt.defaultBranch, tt.dirName); got != tt.want {
				t.Errorf("DeriveSlug(%q, %q, %q) = %q, want %q",
					tt.branch, tt.defaultBranch, tt.dirName, got, tt.want)
			}
		})
	}
}

// The slug reaches a git ref, so nothing DeriveSlug returns may be invalid.
func TestDeriveSlug_AlwaysProducesAValidRefComponent(t *testing.T) {
	inputs := []string{"", " ", "-", "--", "..", "a..b", "@{", "~^:?", "日本語", "??", "/"}
	for _, branch := range inputs {
		for _, dir := range inputs {
			got := DeriveSlug(branch, "main", dir)
			if err := repo.ValidateBranchName(BranchPrefix + got); err != nil {
				t.Errorf("DeriveSlug(%q, main, %q) = %q, which is not a valid branch: %v", branch, dir, got, err)
			}
		}
	}
}

func TestOpen_FirstShare(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{viewer: "operator"}
	res, err := Open(context.Background(), git, forge, Request{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Branch != "herdr/improve-pane" {
		t.Errorf("Branch = %q", res.Branch)
	}
	if !res.BranchCreated {
		t.Error("BranchCreated = false, want true")
	}
	if res.Reused {
		t.Error("Reused = true on a first share")
	}
	if !res.Pushed {
		t.Error("Pushed = false")
	}
	if res.PullRequest.Number != 42 {
		t.Errorf("PullRequest = %+v", res.PullRequest)
	}
	if len(forge.creates) != 1 {
		t.Fatalf("made %d pull requests, want 1", len(forge.creates))
	}
	req := forge.creates[0]
	if req.Head != "herdr/improve-pane" || req.Base != "main" {
		t.Errorf("head/base = %q/%q, want herdr/improve-pane/main", req.Head, req.Base)
	}
	if !req.Draft {
		t.Error("Draft = false: ADR-001 opens a share as a draft, before any code exists")
	}
	if req.Title == "" || req.Body == "" {
		t.Errorf("title/body = %q/%q, want both filled in", req.Title, req.Body)
	}
	// The commit is cut from the remote-tracking ref, so the diff means "this
	// workstream against upstream".
	if !git.calledWith("ensure-branch herdr/improve-pane from origin/main") {
		t.Errorf("calls = %v", git.calls)
	}
	// The branch filter is owner:branch, and the owner is the repository
	// holding the branch rather than whoever is authenticated.
	if forge.lastHeadOwner != "acme" {
		t.Errorf("head owner = %q, want acme", forge.lastHeadOwner)
	}
}

// The branch has to be ready before GitHub is asked anything. A share that fails
// should leave a branch the operator can look at, not a remote branch with no
// pull request and no way to find out why.
func TestOpen_PreparesTheBranchBeforeTalkingToGitHub(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	if _, err := Open(context.Background(), git, forge, Request{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ensureIdx, pushIdx := -1, -1
	for i, c := range git.calls {
		switch {
		case strings.HasPrefix(c, "ensure-branch"):
			ensureIdx = i
		case strings.HasPrefix(c, "push"):
			pushIdx = i
		}
	}
	if ensureIdx < 0 || pushIdx < 0 {
		t.Fatalf("calls = %v, want both an ensure and a push", git.calls)
	}
	if ensureIdx > pushIdx {
		t.Errorf("calls = %v, want the branch prepared before it is pushed", git.calls)
	}
	if forge.findCalls != 1 {
		t.Errorf("findCalls = %d, want the branch to be pushed before the pull request is looked up", forge.findCalls)
	}
}

// ADR-002: the pull request is the thread, so a second share must find it rather
// than split the conversation across two.
func TestOpen_SecondShareReusesTheOpenPullRequest(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	git.branchExists = true
	forge.existing = []github.PullRequest{{Number: 7, HTMLURL: "https://github.com/acme/demo/pull/7", Draft: true, State: "open"}}

	res, err := Open(context.Background(), git, forge, Request{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.Reused {
		t.Error("Reused = false, want true")
	}
	if res.BranchCreated {
		t.Error("BranchCreated = true, want false")
	}
	if res.PullRequest.Number != 7 {
		t.Errorf("PullRequest = %+v, want the existing #7", res.PullRequest)
	}
	if len(forge.creates) != 0 {
		t.Errorf("created %d pull requests, want none", len(forge.creates))
	}
}

// Two open pull requests on one branch is possible when the base differs. The
// first is used, but the operator is told rather than left to discover it.
func TestOpen_WarnsWhenSeveralPullRequestsShareTheBranch(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	git.branchExists = true
	forge.existing = []github.PullRequest{{Number: 7}, {Number: 8}}

	res, err := Open(context.Background(), git, forge, Request{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.PullRequest.Number != 7 {
		t.Errorf("used #%d, want the first", res.PullRequest.Number)
	}
	if !hasWarning(res.Warnings, "2 open pull requests") {
		t.Errorf("Warnings = %v, want one naming the ambiguity", res.Warnings)
	}
}

// A base that cannot be resolved locally must not be guessed: defaulting to
// "main" would produce a diff against the wrong branch on every repository that
// uses something else, and the operator would have no way to tell. GitHub knows
// the same fact origin/HEAD mirrors, so it is asked.
func TestOpen_UnresolvableBaseComesFromGitHub(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{repo: github.Repository{DefaultBranch: "trunk"}}
	git.defaultErr = repo.ErrNoDefaultBranch

	res, err := Open(context.Background(), git, forge, Request{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Base != "trunk" {
		t.Errorf("Base = %q, want trunk", res.Base)
	}
	// The commit is still cut from the local HEAD, which is all that is known.
	if !git.calledWith("ensure-branch herdr/improve-pane from HEAD") {
		t.Errorf("calls = %v, want the commit cut from HEAD", git.calls)
	}
	if !hasWarning(res.Warnings, "default branch") {
		t.Errorf("Warnings = %v, want one explaining the local refs were no use", res.Warnings)
	}
	if !hasWarning(res.Warnings, "trunk") {
		t.Errorf("Warnings = %v, want one saying where the base came from", res.Warnings)
	}
	if forge.creates[0].Base != "trunk" {
		t.Errorf("pull request base = %q, want trunk", forge.creates[0].Base)
	}
}

// A dry run makes no requests, so it cannot fill the base in from GitHub and must
// say so rather than claim a base it has not read.
func TestOpen_DryRunDoesNotAskGitHubForTheBase(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{repo: github.Repository{DefaultBranch: "trunk"}}
	git.defaultErr = repo.ErrNoDefaultBranch

	res, err := Open(context.Background(), git, forge, Request{DryRun: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Base != "" {
		t.Errorf("Base = %q, want empty", res.Base)
	}
	if forge.findCalls != 0 {
		t.Errorf("a dry run made %d requests", forge.findCalls)
	}
}

// If neither the local refs nor GitHub can say what the base is, the share must
// stop before creating anything. Failing later would leave a pushed branch and no
// pull request, with the reason buried in a 422.
func TestOpen_NoBaseAtAllStopsBeforeMutating(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{repoErr: errors.New("403 Forbidden")}
	git.defaultErr = repo.ErrNoDefaultBranch

	_, err := Open(context.Background(), git, forge, Request{})
	if err == nil {
		t.Fatal("want an error when no base can be determined")
	}
	if !strings.Contains(err.Error(), "--base") {
		t.Errorf("err = %v, want it to say how to proceed", err)
	}
	for _, c := range git.calls {
		if strings.HasPrefix(c, "ensure-branch") || strings.HasPrefix(c, "push") {
			t.Errorf("the repository was changed: %q", c)
		}
	}
	if len(forge.creates) != 0 {
		t.Error("a pull request was created without a base")
	}
}

// An empty default_branch would reach GitHub as a missing field.
func TestOpen_EmptyDefaultBranchFromGitHubStops(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	git.defaultErr = repo.ErrNoDefaultBranch

	if _, err := Open(context.Background(), git, forge, Request{}); err == nil {
		t.Fatal("want an error when GitHub reports no default branch")
	}
}

func TestOpen_BaseOverride(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	res, err := Open(context.Background(), git, forge, Request{Base: "release-2"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Base != "release-2" {
		t.Errorf("Base = %q", res.Base)
	}
	// The override exists only locally, so the local branch is used rather than
	// an origin/ ref that does not resolve.
	if !git.calledWith("ensure-branch herdr/improve-pane from release-2") {
		t.Errorf("calls = %v", git.calls)
	}
}

// An explicit name is the operator's, so it is refused with a suggestion rather
// than quietly rewritten into something they did not type.
func TestOpen_UnusableSlugOverrideIsRefused(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	_, err := Open(context.Background(), git, forge, Request{Slug: "My Thread!"})
	if err == nil {
		t.Fatal("want an error for a slug that is not a valid ref")
	}
	if !strings.Contains(err.Error(), "my-thread") {
		t.Errorf("err = %v, want it to suggest the usable form", err)
	}
	if !strings.Contains(err.Error(), "--slug") {
		t.Errorf("err = %v, want it to name the flag", err)
	}
	if len(forge.creates) != 0 {
		t.Error("a pull request was created for a refused slug")
	}
}

func TestOpen_SlugOverride(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	res, err := Open(context.Background(), git, forge, Request{Slug: "my-thread"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Branch != "herdr/my-thread" {
		t.Errorf("Branch = %q", res.Branch)
	}
	if !strings.Contains(forge.creates[0].Title, "my-thread") {
		t.Errorf("title = %q, want it to name the slug", forge.creates[0].Title)
	}
}

// Nothing may be created, pushed or requested on a dry run. It exists so the
// operator can see what a share would do before it does it.
func TestOpen_DryRunChangesNothing(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	res, err := Open(context.Background(), git, forge, Request{DryRun: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.DryRun {
		t.Error("DryRun = false")
	}
	if !res.BranchCreated {
		t.Error("BranchCreated = false, want true: the branch does not exist yet")
	}
	for _, c := range git.calls {
		if strings.HasPrefix(c, "ensure-branch") || strings.HasPrefix(c, "push") {
			t.Errorf("a dry run changed the repository: %q", c)
		}
	}
	if len(git.pushed) != 0 {
		t.Errorf("pushed = %v", git.pushed)
	}
	if forge.findCalls != 0 || len(forge.creates) != 0 {
		t.Errorf("a dry run made requests: find=%d create=%d", forge.findCalls, len(forge.creates))
	}
	if res.PullRequest.Number != 0 {
		t.Errorf("PullRequest = %+v, want zero on a dry run", res.PullRequest)
	}
}

// A non-github.com remote is refused rather than sent to the github.com API,
// which would either 404 confusingly or, worse, act on a same-named repository.
func TestOpen_RefusesANonGitHubHost(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	git.slug = repo.Slug{Host: "github.example.com", Owner: "acme", Name: "widget"}

	_, err := Open(context.Background(), git, forge, Request{})
	if err == nil {
		t.Fatal("want an error for an enterprise host")
	}
	if !strings.Contains(err.Error(), "github.example.com") {
		t.Errorf("err = %v, want it to name the host", err)
	}
}

// Invitations become the allowlist of people whose comments may drive the agent,
// so each one that succeeded is reported and each one that failed is explained
// without losing the pull request that already exists.
func TestOpen_InvitesAndKeepsTheShareWhenAnInviteFails(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	res, err := Open(context.Background(), git, forge, Request{Invite: []string{"@bob", "carol"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(res.Invited) != 2 {
		t.Errorf("Invited = %v, want both logins with the @ stripped", res.Invited)
	}
	if forge.collabs[0] != "bob" {
		t.Errorf("invited %q, want bob", forge.collabs[0])
	}

	// Now the failure case: the operator lacks admin on a work org. ADR-001 says
	// that is a hard limit rather than something to work around, so the share
	// must survive it.
	git2, forge2 := newFakeGit(), &fakeForge{collabErr: errors.New("needs admin rights")}
	res2, err := Open(context.Background(), git2, forge2, Request{Invite: []string{"bob"}})
	if err != nil {
		t.Fatalf("a failed invite must not fail the share: %v", err)
	}
	if len(res2.Invited) != 0 {
		t.Errorf("Invited = %v, want none", res2.Invited)
	}
	if !hasWarning(res2.Warnings, "could not invite bob") {
		t.Errorf("Warnings = %v, want the failure reported", res2.Warnings)
	}
	if res2.PullRequest.Number == 0 {
		t.Error("the pull request was lost when the invite failed")
	}
}

// A failed invitation means "you could not be given access", not "you have no
// access": pagbrl already had write access to the repository this was measured
// on, and GitHub answers 422 for that rather than the 204 its documentation
// implies. Treating it as a failure left the collaborator off the allowlist,
// which is the one thing that lets their /agent comments reach the agent.
func TestOpen_InvitesSomeoneWhoAlreadyHasAccess(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{
		viewer:    "operator",
		collabErr: errors.New("github: 422: Validation Failed"),
		hasAccess: true,
	}
	res, err := Open(context.Background(), git, forge, Request{Invite: []string{"@pagbrl"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !contains(res.Allowlist, "pagbrl") {
		t.Errorf("Allowlist = %v, want pagbrl on it", res.Allowlist)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none: the person can already read the repository", res.Warnings)
	}
	if contains(res.Invited, "pagbrl") {
		t.Error("Invited claims an invitation was sent; nobody was invited, they already had access")
	}
}

// Someone who can genuinely not be given access is reported, and is not
// allowlisted: the warning is the operator's only signal that the person they
// meant to share with cannot comment.
func TestOpen_WarnsWhenTheInviteFailsAndTheyHaveNoAccess(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{
		viewer:    "operator",
		collabErr: errors.New("github: 422: Validation Failed"),
	}
	res, err := Open(context.Background(), git, forge, Request{Invite: []string{"typoo"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if contains(res.Allowlist, "typoo") {
		t.Errorf("Allowlist = %v, want nobody who has no access", res.Allowlist)
	}
	if !hasWarning(res.Warnings, "could not invite typoo") {
		t.Errorf("Warnings = %v, want the failure reported", res.Warnings)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// A push failure has to stop the share: opening a pull request against a branch
// GitHub cannot see would fail anyway, with a worse message.
func TestOpen_PushFailureStopsBeforeCreatingThePullRequest(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail func(*fakeGit)
	}{
		{"branch cannot be prepared", func(g *fakeGit) { g.ensureErr = errors.New("no such remote") }},
		{"push is refused", func(g *fakeGit) { g.pushErr = errors.New("remote rejected: protected branch") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			git, forge := newFakeGit(), &fakeForge{}
			tt.fail(git)

			_, err := Open(context.Background(), git, forge, Request{})
			if err == nil {
				t.Fatal("want an error")
			}
			if len(forge.creates) != 0 {
				t.Error("a pull request was created although the branch is not on the remote")
			}
			if forge.findCalls != 0 {
				t.Error("GitHub was asked about a branch that was never pushed")
			}
		})
	}
}

func hasWarning(warnings []string, fragment string) bool {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}

func (f *fakeGit) calledWith(want string) bool {
	for _, c := range f.calls {
		if c == want {
			return true
		}
	}
	return false
}

// The allowlist decides whose comments may drive the agent, and the operator has
// to be on it: otherwise the person who owns the agent cannot steer it from the
// pull request, which is the loop the product exists for.
func TestOpen_AllowlistContainsOperatorAndInvitees(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{viewer: "operator"}
	res, err := Open(context.Background(), git, forge, Request{Invite: []string{"bob"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(res.Allowlist) != 2 || res.Allowlist[0] != "operator" || res.Allowlist[1] != "bob" {
		t.Errorf("Allowlist = %v, want [operator bob]", res.Allowlist)
	}
	st := FromResult(res, Origin{PaneID: "wQ:p1", Agent: "pi", CWD: "/tmp/x", Kind: "pi", Session: "/tmp/x/s.jsonl"}, time.Now())
	if len(st.Allowlist) != 2 {
		t.Errorf("recorded allowlist = %v, want the same two logins", st.Allowlist)
	}
	if st.Origin.PaneID != "wQ:p1" {
		t.Errorf("recorded origin = %+v, want the pane the share was opened from", st.Origin)
	}
}

// Reading the operator's login is the first GitHub call, and every later request
// needs the same token. An unusable token therefore has to stop the share before
// anything is written, rather than after a branch has been pushed.
func TestOpen_UnusableTokenStopsBeforeWritingAnything(t *testing.T) {
	for _, tt := range []struct {
		name   string
		dryRun bool
		want   bool
	}{
		{"a share", false, true},
		// A dry run makes no requests, so it cannot be blocked by a token.
		{"a dry run", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			git, forge := newFakeGit(), &fakeForge{viewerErr: errors.New("401 Unauthorized")}
			_, err := Open(context.Background(), git, forge, Request{DryRun: tt.dryRun})
			if !tt.want {
				if err != nil {
					t.Fatalf("a dry run must not need a token: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error for an unusable token")
			}
			if !strings.Contains(err.Error(), "401") {
				t.Errorf("err = %v, want it to carry the cause", err)
			}
			for _, c := range git.calls {
				if strings.HasPrefix(c, "ensure-branch") || strings.HasPrefix(c, "push") {
					t.Errorf("the repository was changed before the token was checked: %q", c)
				}
			}
			if len(forge.creates) != 0 {
				t.Error("a pull request was attempted with an unusable token")
			}
		})
	}
}

func TestFromResult_RecordsTheShareThePollerNeeds(t *testing.T) {
	res := Result{
		Repo:        repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"},
		Branch:      "herdr/improve-pane",
		Base:        "main",
		PullRequest: github.PullRequest{Number: 42, HTMLURL: "https://github.com/acme/demo/pull/42"},
		Allowlist:   []string{"operator", "bob"},
	}
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	origin := Origin{PaneID: "wQ:p1", Agent: "pi", CWD: "/tmp/x", Kind: "pi", Session: "/tmp/x/s.jsonl", SessionID: "abc"}
	st := FromResult(res, origin, now)
	if st.Key() != "acme/demo#herdr/improve-pane" {
		t.Errorf("Key = %q", st.Key())
	}
	if st.Number != 42 || st.URL == "" || st.Base != "main" {
		t.Errorf("record = %+v", st)
	}
	// Without this the poller sees a share with no agent, retires it, and the
	// thread never receives a transcript — the failure that reaching this level
	// of the code with no origin causes.
	if st.Origin != origin {
		t.Errorf("Origin = %+v, want %+v", st.Origin, origin)
	}
	if !st.CreatedAt.Equal(now) || !st.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v/%v", st.CreatedAt, st.UpdatedAt)
	}
}

// The commit and the pull request are the first things a second person sees, so
// what they say is part of the interface rather than a detail.
func TestOpen_WordingExplainsTheThread(t *testing.T) {
	msg := commitMessage("improve-pane")
	subject, body, found := strings.Cut(msg, "\n\n")
	if !found || strings.TrimSpace(body) == "" {
		t.Fatalf("commitMessage = %q, want a subject and a body", msg)
	}
	if !strings.Contains(subject, "improve-pane") {
		t.Errorf("subject = %q, want it to name the workstream", subject)
	}
	if !strings.Contains(body, "empty on purpose") {
		t.Errorf("body = %q, want it to explain the empty commit", body)
	}
	// Long subjects get truncated in logs and in the pull request list.
	if len(subject) > 72 {
		t.Errorf("subject is %d characters: %q", len(subject), subject)
	}
	for _, line := range strings.Split(msg, "\n") {
		if len(line) > 75 {
			t.Errorf("line is %d characters: %q", len(line), line)
		}
	}

	git, forge := newFakeGit(), &fakeForge{}
	if _, err := Open(context.Background(), git, forge, Request{}); err != nil {
		t.Fatal(err)
	}
	created := forge.creates[0]
	if created.Title != "Plan: improve-pane" {
		t.Errorf("title = %q", created.Title)
	}
	if !strings.Contains(created.Body, "improve-pane") {
		t.Errorf("body = %q, want it to name the workstream", created.Body)
	}
	if !strings.Contains(created.Body, "diff") {
		t.Errorf("body = %q, want it to say where the work goes", created.Body)
	}
	// ADR-003 moves the conversation to the comments, so a body that promises it
	// is in the body sends the reader looking in the wrong place.
	if strings.Contains(created.Body, "in this body") {
		t.Errorf("body = %q, claims the conversation lands in the body", created.Body)
	}
	if !strings.Contains(created.Body, "comments") {
		t.Errorf("body = %q, want it to say the conversation arrives as comments", created.Body)
	}
}

// A caller that supplies its own title keeps it.
func TestOpen_TitleOverride(t *testing.T) {
	git, forge := newFakeGit(), &fakeForge{}
	if _, err := Open(context.Background(), git, forge, Request{Title: "Custom", Body: "Custom body"}); err != nil {
		t.Fatal(err)
	}
	if forge.creates[0].Title != "Custom" || forge.creates[0].Body != "Custom body" {
		t.Errorf("got %q / %q, want the overrides", forge.creates[0].Title, forge.creates[0].Body)
	}
}
