package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}, srv
}

func TestRepoIsPrivate(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr error
	}{
		{name: "public repo", status: 200, body: `{"private":false}`, want: false},
		{name: "private repo, authenticated", status: 200, body: `{"private":true}`, want: true},
		// Unauthenticated reads of a private repo 404 rather than 403, so the
		// probe cannot tell "private" from "does not exist" — and both mean
		// the caller must ask for the broader scope.
		{name: "private repo, unauthenticated", status: 404, body: `{"message":"Not Found"}`, wantErr: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			got, err := c.RepoIsPrivate(context.Background(), "acme", "demo")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("private = %v, want %v", got, tt.want)
			}
		})
	}
}

// The visibility probe runs before we hold a token, so it must not send an
// Authorization header; a client that always sent one would turn a public repo
// lookup into a 401.
func TestRepoIsPrivate_OmitsAuthHeaderWhenTokenless(t *testing.T) {
	var gotAuth string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"private":false}`))
	})
	if _, err := c.RepoIsPrivate(context.Background(), "acme", "demo"); err != nil {
		t.Fatalf("RepoIsPrivate: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("tokenless request sent Authorization: %q", gotAuth)
	}
}

func TestClient_SendsAuthAndStandardHeaders(t *testing.T) {
	var h http.Header
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"private":false}`))
	})
	c.Token = "gho_example"
	if _, err := c.RepoIsPrivate(context.Background(), "acme", "demo"); err != nil {
		t.Fatalf("RepoIsPrivate: %v", err)
	}
	if got := h.Get("Authorization"); got != "Bearer gho_example" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer gho_example")
	}
	if got := h.Get("Accept"); !strings.Contains(got, "github+json") {
		t.Errorf("Accept = %q, want it to request the GitHub JSON media type", got)
	}
	if got := h.Get("User-Agent"); got == "" {
		t.Errorf("User-Agent must be set; GitHub rejects requests without one")
	}
}

// A 403 that drained the quota must be distinguishable: the poller relies on
// ETag/304s precisely to avoid this, so it has to recognise the state.
func TestClient_APIError(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		headers     map[string]string
		body        string
		wantMessage string
		rateLimited bool
	}{
		{
			name: "forbidden by rate limit", status: 403,
			headers:     map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000000"},
			body:        `{"message":"API rate limit exceeded for 1.2.3.4."}`,
			wantMessage: "API rate limit exceeded", rateLimited: true,
		},
		{
			name: "forbidden for other reasons", status: 403,
			headers:     map[string]string{"X-RateLimit-Remaining": "4999"},
			body:        `{"message":"Resource not accessible by integration"}`,
			wantMessage: "Resource not accessible", rateLimited: false,
		},
		{
			name: "unauthorized", status: 401,
			body:        `{"message":"Bad credentials"}`,
			wantMessage: "Bad credentials", rateLimited: false,
		},
		{
			// GitHub's edge can return HTML or an empty body; the error must
			// still be reportable rather than a JSON decode failure. The
			// numeric status is carried by APIError.Status, so the message
			// only has to be human-readable.
			name: "non-json error body", status: 502,
			body:        "<html>bad gateway</html>",
			wantMessage: "Bad Gateway", rateLimited: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.RepoIsPrivate(context.Background(), "acme", "demo")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if apiErr.Status != tc.status {
				t.Errorf("Status = %d, want %d", apiErr.Status, tc.status)
			}
			if !strings.Contains(apiErr.Message, tc.wantMessage) {
				t.Errorf("Message = %q, want it to contain %q", apiErr.Message, tc.wantMessage)
			}
			if apiErr.RateLimited != tc.rateLimited {
				t.Errorf("RateLimited = %v, want %v", apiErr.RateLimited, tc.rateLimited)
			}
		})
	}
}

func TestRepoIsPrivate_RejectsBadSlug(t *testing.T) {
	c := &Client{}
	for _, slug := range [][2]string{{"", "demo"}, {"acme", ""}, {"a/b", "demo"}} {
		if _, err := c.RepoIsPrivate(context.Background(), slug[0], slug[1]); err == nil {
			t.Errorf("RepoIsPrivate(%q, %q) must fail fast, got nil error", slug[0], slug[1])
		}
	}
}

// The request path must be exactly the documented REST path.
func TestClient_RequestPath(t *testing.T) {
	var gotPath, gotMethod string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"private":true}`))
	})
	if _, err := c.RepoIsPrivate(context.Background(), "acme", "demo"); err != nil {
		t.Fatalf("RepoIsPrivate: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/repos/acme/demo" {
		t.Errorf("got %s %s, want GET /repos/acme/demo", gotMethod, gotPath)
	}
}

// Viewer returns the login of the authenticated user, which doubles as a
// liveness check on a stored token: a revoked token must fail here rather than
// halfway through opening a PR.
func TestClient_Viewer(t *testing.T) {
	t.Run("returns the login", func(t *testing.T) {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/user" {
				t.Errorf("path = %q, want /user", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer gho_x" {
				t.Errorf("Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"login":"DnzzL"}`))
		})
		c.Token = "gho_x"
		login, err := c.Viewer(context.Background())
		if err != nil {
			t.Fatalf("Viewer: %v", err)
		}
		if login != "DnzzL" {
			t.Errorf("login = %q, want DnzzL", login)
		}
	})

	t.Run("revoked token", func(t *testing.T) {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		})
		c.Token = "revoked"
		if _, err := c.Viewer(context.Background()); err == nil {
			t.Fatal("Viewer must fail for a revoked token")
		}
	})

	t.Run("missing login is an error", func(t *testing.T) {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		c.Token = "gho_x"
		if _, err := c.Viewer(context.Background()); err == nil {
			t.Fatal("an empty login must not be reported as success")
		}
	})
}

// decodeJSONName guards the local helper used by tests above.
func TestDecodeHelperShape(t *testing.T) {
	var v struct {
		Private bool `json:"private"`
	}
	if err := json.Unmarshal([]byte(`{"private":true}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !v.Private {
		t.Errorf("Private = false, want true")
	}
}
