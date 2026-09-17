package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DnzzL/herdr-huddle/internal/github"
)

// The scope actually requested is a least-privilege decision, and the subtle
// case is the 404: an unauthenticated read of a private repo is indistinguishable
// from a repo that does not exist, and both require the broad scope.
func TestScopesForRepo(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantScopes string
		wantErr    bool
	}{
		{name: "public", status: 200, body: `{"private":false}`, wantScopes: "public_repo"},
		{name: "private, visible", status: 200, body: `{"private":true}`, wantScopes: "repo"},
		{name: "invisible, unauthenticated", status: 404, body: `{"message":"Not Found"}`, wantScopes: "repo"},
		{name: "server error is not a scope decision", status: 500, body: `{"message":"boom"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			c := &github.Client{BaseURL: srv.URL, HTTP: srv.Client()}
			scopes, err := ScopesForRepo(context.Background(), c, "acme", "demo")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ScopesForRepo: %v", err)
			}
			if len(scopes) != 1 || scopes[0] != tc.wantScopes {
				t.Errorf("scopes = %v, want [%s]", scopes, tc.wantScopes)
			}
		})
	}
}

// The probe must not carry a token: it runs precisely when none is stored yet.
func TestScopesForRepo_ProbesWithoutToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"private":false}`))
	}))
	t.Cleanup(srv.Close)

	c := &github.Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: ""}
	if _, err := ScopesForRepo(context.Background(), c, "acme", "demo"); err != nil {
		t.Fatalf("ScopesForRepo: %v", err)
	}
	if auth != "" {
		t.Errorf("probe sent Authorization: %q", auth)
	}
}
