package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// noRequests fails the test if the handler is reached, for the validation paths
// that must not spend a request.
func noRequests(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestListComments_SendsTheConditionalRequest(t *testing.T) {
	var gotSince string
	var gotConditional string
	var gotQuery string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if want := "/repos/acme/demo/issues/42/comments"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		gotQuery = r.URL.RawQuery
		gotSince = r.URL.Query().Get("since")
		gotConditional = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `W/"abc123"`)
		_, _ = io.WriteString(w, `[{"id":11,"body":"/agent do the thing","author_association":"COLLABORATOR","created_at":"2026-09-18T10:00:00Z","html_url":"https://x/11","user":{"login":"bob"}}]`)
	})

	since := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	got, err := c.ListComments(context.Background(), "acme", "demo", 42, since, `W/"old"`)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if gotConditional != `W/"old"` {
		t.Errorf("If-None-Match = %q, want the etag passed in", gotConditional)
	}
	if gotSince != "2026-09-18T09:00:00Z" {
		t.Errorf("since = %q, want an RFC3339 UTC timestamp", gotSince)
	}
	if !strings.Contains(gotQuery, "direction=desc") {
		t.Errorf("query = %q, want newest first", gotQuery)
	}
	if got.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q, want the response's", got.ETag)
	}
	if got.NotModified {
		t.Error("NotModified = true on a 200")
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d comments, want 1", len(got.Items))
	}
	if got.Items[0].Author() != "bob" {
		t.Errorf("Author() = %q, want bob (read through user.login)", got.Items[0].Author())
	}
	if got.Items[0].Association != "COLLABORATOR" {
		t.Errorf("Association = %q", got.Items[0].Association)
	}
}

func TestListComments_NotModifiedIsNotAnError(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"abc123"`)
		w.WriteHeader(http.StatusNotModified)
	})

	got, err := c.ListComments(context.Background(), "acme", "demo", 42, time.Time{}, `W/"abc123"`)
	if err != nil {
		t.Fatalf("a 304 must not be an error for a poller: %v", err)
	}
	if !got.NotModified {
		t.Error("NotModified = false")
	}
	if got.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q, want the etag kept so the next poll can reuse it", got.ETag)
	}
}

func TestListComments_MissingArrayIsAnError(t *testing.T) {
	// JSON null decodes into a nil slice without complaint, which would read as
	// "nobody has commented" forever. It is a shape change, so it is an error.
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `null`)
	})

	_, err := c.ListComments(context.Background(), "acme", "demo", 42, time.Time{}, "")
	if err == nil {
		t.Fatal("want an error for a response with no comments array")
	}
	if !strings.Contains(err.Error(), "no comments array") {
		t.Errorf("error = %v, want it to name the missing array", err)
	}
}

func TestListComments_RefusesANumberlessRequest(t *testing.T) {
	c, _ := testClient(t, noRequests(t))
	if _, err := c.ListComments(context.Background(), "acme", "demo", 0, time.Time{}, ""); err == nil {
		t.Fatal("want an error for pull request 0")
	}
	if _, err := c.ListComments(context.Background(), "acme/demo", "demo", 1, time.Time{}, ""); err == nil {
		t.Fatal("want an error for a slug that is not a single path segment")
	}
}

func TestCreateComment_PostsAndDecodesTheResponse(t *testing.T) {
	var body map[string]string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if want := "/repos/acme/demo/issues/42/comments"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":99,"body":"**Agent** hi","author_association":"OWNER","html_url":"https://x/99","user":{"login":"me"}}`)
	})

	got, err := c.CreateComment(context.Background(), "acme", "demo", 42, "**Agent** hi")
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if body["body"] != "**Agent** hi" {
		t.Errorf("posted body = %q", body["body"])
	}
	if got.ID != 99 || got.HTMLURL != "https://x/99" {
		t.Errorf("decoded comment = %+v", got)
	}
}

func TestCreateComment_RefusesContentGitHubWouldReject(t *testing.T) {
	c, _ := testClient(t, noRequests(t))

	over := strings.Repeat("x", MaxCommentRunes+1)
	_, err := c.CreateComment(context.Background(), "acme", "demo", 42, over)
	if err == nil {
		t.Fatal("want an error for a comment over GitHub's limit")
	}
	// The limit is in characters, so a multi-byte body within the limit is fine
	// while one past it is not: the error must name the real count.
	if !strings.Contains(err.Error(), "65537") {
		t.Errorf("error = %v, want it to count characters", err)
	}

	_, err = c.CreateComment(context.Background(), "acme", "demo", 42, "")
	if err == nil {
		t.Fatal("want an error for an empty comment")
	}
	_, err = c.CreateComment(context.Background(), "acme", "demo", 0, "hi")
	if err == nil {
		t.Fatal("want an error for pull request 0")
	}
}

func TestCreateComment_CountsCharactersNotBytes(t *testing.T) {
	var posted int
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		posted++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"user":{"login":"me"}}`)
	})

	// Three bytes per rune: 30,000 runes is 90,000 bytes, under the limit in
	// characters. Counting bytes would refuse a body GitHub accepts.
	if _, err := c.CreateComment(context.Background(), "acme", "demo", 42, strings.Repeat("→", 30000)); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if posted != 1 {
		t.Errorf("posted %d times, want 1", posted)
	}
}

func TestAcknowledge(t *testing.T) {
	var path, content string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		content = body["content"]
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	})

	if err := c.Acknowledge(context.Background(), "acme", "demo", 1234, "eyes"); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if want := "/repos/acme/demo/issues/comments/1234/reactions"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if content != "eyes" {
		t.Errorf("content = %q, want eyes", content)
	}
}

func TestAcknowledge_RefusesWhatGitHubWouldReject(t *testing.T) {
	c, _ := testClient(t, noRequests(t))
	if err := c.Acknowledge(context.Background(), "acme", "demo", 1, "construction"); err == nil {
		t.Fatal("want an error for a reaction GitHub does not have")
	}
	if err := c.Acknowledge(context.Background(), "acme", "demo", 0, "eyes"); err == nil {
		t.Fatal("want an error for comment 0")
	}
	if err := c.Acknowledge(context.Background(), "acme/demo", "demo", 1, "eyes"); err == nil {
		t.Fatal("want an error for a malformed slug")
	}
}
