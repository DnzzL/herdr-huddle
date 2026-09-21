package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreatePullRequest(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotMethod string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/acme/demo/pull/42","draft":true,"state":"open"}`))
	})

	pr, err := c.CreatePullRequest(context.Background(), "acme", "demo", CreatePullRequestRequest{
		Title: "Plan: improve-pane",
		Head:  "herdr/improve-pane",
		Base:  "main",
		Body:  "the plan so far",
		Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/acme/demo/pulls" {
		t.Errorf("%s %s, want POST /repos/acme/demo/pulls", gotMethod, gotPath)
	}
	if pr.Number != 42 || pr.HTMLURL != "https://github.com/acme/demo/pull/42" {
		t.Errorf("pr = %+v", pr)
	}
	if gotBody["draft"] != true {
		t.Errorf("draft = %v, want true — a share is always a draft", gotBody["draft"])
	}
	if gotBody["head"] != "herdr/improve-pane" || gotBody["base"] != "main" {
		t.Errorf("head/base = %v/%v", gotBody["head"], gotBody["base"])
	}
}

// A repository with pull requests disabled, or a head that GitHub cannot see,
// answers 422. The message is the useful part and must survive.
func TestCreatePullRequest_ValidationErrorIsExplained(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"No commits between main and herdr/x"}]}`))
	})
	_, err := c.CreatePullRequest(context.Background(), "acme", "demo", CreatePullRequestRequest{Head: "herdr/x", Base: "main"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("err = %v, want GitHub's message", err)
	}
}

// The head filter needs the owner qualified and the branch escaped, because a
// share slug can contain a slash and an unescaped one changes the query.
func TestFindOpenPullRequest(t *testing.T) {
	var gotQuery string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/repos/acme/demo/pulls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/acme/demo/pull/7","draft":true,"state":"open"}]`))
	})

	prs, err := c.FindOpenPullRequests(context.Background(), "acme", "demo", "acme", "herdr/improve-pane")
	if err != nil {
		t.Fatalf("FindOpenPullRequests: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d pull requests, want 1", len(prs))
	}
	if prs[0].Number != 7 {
		t.Errorf("pr = %+v", prs[0])
	}
	if !strings.Contains(gotQuery, "state=open") {
		t.Errorf("query = %q, want state=open", gotQuery)
	}
	if !strings.Contains(gotQuery, "head=acme%3Aherdr%2Fimprove-pane") {
		t.Errorf("query = %q, want the head filter escaped as acme:herdr/improve-pane", gotQuery)
	}
}

// No pull request is not an error: it is the normal case for a first share, and
// the caller needs to tell it apart from a request that failed.
func TestFindOpenPullRequest_NoneFoundIsNotAnError(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[]`))
	})
	prs, err := c.FindOpenPullRequests(context.Background(), "acme", "demo", "acme", "herdr/x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(prs) != 0 {
		t.Errorf("got %d pull requests, want none", len(prs))
	}
}

// Several open pull requests can share a head if the base differs. Taking the
// first is arbitrary, so the count is reported instead of hidden: the caller can
// say something rather than silently picking one.
func TestFindOpenPullRequest_ReportsAmbiguity(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"number":7,"html_url":"u7"},{"number":8,"html_url":"u8"}]`))
	})
	prs, err := c.FindOpenPullRequests(context.Background(), "acme", "demo", "acme", "herdr/x")
	if err != nil || len(prs) != 2 {
		t.Fatalf("len = %d, err = %v, want 2", len(prs), err)
	}
	if prs[0].Number != 7 {
		t.Errorf("first number = %d, want 7", prs[0].Number)
	}
}

// The invite is once per share, and a second attempt must not be reported as a
// failure... except that GitHub does report it as one: measured, inviting a
// `write` collaborator answers 422 Validation Failed rather than the 204 the
// documentation implies. The 201/204 cases below are still the happy paths.
func TestAddCollaborator(t *testing.T) {
	for _, status := range []int{201, 204} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var gotMethod, gotPath string
			var body map[string]any
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(status)
				if status == 201 {
					_, _ = w.Write([]byte(`{"invitee":{"login":"bob"}}`))
				}
			})
			if err := c.AddCollaborator(context.Background(), "acme", "demo", "bob"); err != nil {
				t.Fatalf("AddCollaborator: %v", err)
			}
			if gotMethod != http.MethodPut || gotPath != "/repos/acme/demo/collaborators/bob" {
				t.Errorf("%s %s", gotMethod, gotPath)
			}
			if body["permission"] != "pull" {
				t.Errorf("permission = %v, want pull (ADR-001: read access is enough to comment)", body["permission"])
			}
		})
	}
}

// ADR-001 calls this out as a hard limit rather than something to work around:
// inviting needs admin on the repository, and on a work org the operator often
// does not have it. The error has to say so, because "403 Forbidden" alone sends
// people looking in the wrong place.
func TestAddCollaborator_ExplainsMissingAdmin(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"Must have admin rights to Repository."}`))
	})
	err := c.AddCollaborator(context.Background(), "acme", "demo", "bob")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("err = %v, want it to mention the admin requirement", err)
	}
}

// The invite is for the other person's login, and the allowlist is built from
// it, so a malformed login must be refused before it is used as a URL path
// segment.
func TestAddCollaborator_RejectsNonLogin(t *testing.T) {
	c, _ := testClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was made for a malformed login: %s", r.URL.Path)
	})
	for _, bad := range []string{"", "@bob", "bob/../admin", "bob?x=1", "bob bob"} {
		if err := c.AddCollaborator(context.Background(), "acme", "demo", bad); err == nil {
			t.Errorf("AddCollaborator(%q) succeeded, want an error", bad)
		}
	}
}

// Whether someone can already read the repository is answered by the status code
// alone, and that answer decides whether a failed invitation is a failure.
func TestHasAccess(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   bool
	}{
		// 204: a collaborator, whatever their role.
		{http.StatusNoContent, true},
		// 404: not a collaborator. GitHub answers 404 rather than 403 so that it
		// does not disclose who has access to a private repository.
		{http.StatusNotFound, false},
	} {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var gotPath string
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
			})
			got, err := c.HasAccess(context.Background(), "acme", "demo", "pagbrl")
			if err != nil {
				t.Fatalf("HasAccess: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasAccess = %v, want %v", got, tt.want)
			}
			if gotPath != "/repos/acme/demo/collaborators/pagbrl" {
				t.Errorf("path = %q", gotPath)
			}
		})
	}
}

// A status that is neither 204 nor 404 is a real failure, and must not be read as
// "they have no access": treating an outage as a no would drop a collaborator off
// the allowlist and silently stop their comments reaching the agent.
func TestHasAccess_ReportsARealFailure(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
	})
	if _, err := c.HasAccess(context.Background(), "acme", "demo", "bob"); err == nil {
		t.Error("want an error, not a silent no-access")
	}
}

// The login becomes a URL path segment here too.
func TestHasAccess_RejectsNonLogin(t *testing.T) {
	c, _ := testClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was made for a malformed login: %s", r.URL.Path)
	})
	if _, err := c.HasAccess(context.Background(), "acme", "demo", "bob/../admin"); err == nil {
		t.Error("want an error for a path that escapes the endpoint")
	}
}

// The default branch decides what a share's pull request targets. GitHub is
// authoritative for it, which matters when a repository has no origin/HEAD to
// read it from — the case a `git remote add` + `git fetch` layout produces.
func TestRepository_ReadsDefaultBranch(t *testing.T) {
	var gotPath string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"private":false,"default_branch":"trunk","full_name":"acme/demo"}`))
	})

	repo, err := c.Repository(context.Background(), "acme", "demo")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if gotPath != "/repos/acme/demo" {
		t.Errorf("path = %q", gotPath)
	}
	if repo.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", repo.DefaultBranch)
	}
	if repo.Private {
		t.Error("Private = true")
	}
}

// A response without the field must not be read as a branch named "". An empty
// base is rejected by GitHub with a 422 that says nothing useful.
func TestRepository_MissingDefaultBranchIsEmpty(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"private":true}`))
	})
	repo, err := c.Repository(context.Background(), "acme", "demo")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q, want empty", repo.DefaultBranch)
	}
	if !repo.Private {
		t.Error("Private = false")
	}
}

func TestRepository_RejectsBadSlug(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request was made for a slug that should have been refused")
	})
	for _, slug := range [][2]string{{"", "demo"}, {"acme", ""}, {"a/b", "demo"}, {"acme", "a b"}} {
		if _, err := c.Repository(context.Background(), slug[0], slug[1]); err == nil {
			t.Errorf("Repository(%q, %q) must fail fast, got nil error", slug[0], slug[1])
		}
	}
}
