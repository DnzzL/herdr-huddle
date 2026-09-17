package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeClock makes the polling loop's timing observable and instant. Waiting on
// the real clock would make these tests slow and flaky; without an injectable
// clock the expiry path could not be tested at all.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// deviceServer fakes GitHub's two device-flow endpoints. tokenResponses are
// returned in order, one per poll.
func deviceServer(t *testing.T, c *fakeClock, tokenResponses ...string) (*httptest.Server, *DeviceFlow) {
	t.Helper()
	var polls int
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("device code request used %s, want POST", r.Method)
		}
		user, pass, _ := r.BasicAuth()
		if user != "" || pass != "" {
			t.Errorf("device flow must not send credentials, got basic auth %q:%q", user, pass)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_code":"dev-123","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("device_code"); got != "dev-123" {
			t.Errorf("device_code = %q", got)
		}
		if got := r.Form.Get("client_id"); got != "cid_test" {
			t.Errorf("client_id = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		i := polls
		polls++
		if i >= len(tokenResponses) {
			t.Errorf("polled %d times, only %d responses scripted", polls, len(tokenResponses))
			_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
			return
		}
		_, _ = io.WriteString(w, tokenResponses[i])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, &DeviceFlow{
		ClientID:      "cid_test",
		Scopes:        []string{"public_repo"},
		HTTP:          srv.Client(),
		DeviceCodeURL: srv.URL + "/login/device/code",
		TokenURL:      srv.URL + "/login/oauth/access_token",
		Now:           c.Now,
		Sleep:         c.Sleep,
	}
}

func TestDeviceFlow_HappyPath(t *testing.T) {
	c := newFakeClock()
	_, flow := deviceServer(t, c,
		`{"error":"authorization_pending"}`,
		`{"error":"authorization_pending"}`,
		`{"access_token":"gho_token","token_type":"bearer","scope":"public_repo"}`,
	)

	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if code.UserCode != "WDJB-MJHT" {
		t.Errorf("UserCode = %q", code.UserCode)
	}
	if code.VerificationURI != "https://github.com/login/device" {
		t.Errorf("VerificationURI = %q", code.VerificationURI)
	}
	if code.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", code.Interval)
	}
	if code.ExpiresIn != 900*time.Second {
		t.Errorf("ExpiresIn = %v, want 15m", code.ExpiresIn)
	}

	tok, err := flow.Wait(context.Background(), code)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if tok.AccessToken != "gho_token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if tok.Scope != "public_repo" {
		t.Errorf("Scope = %q", tok.Scope)
	}
	if len(c.sleeps) != 3 {
		t.Fatalf("expected one wait per poll, got %v", c.sleeps)
	}
	for i, d := range c.sleeps {
		if d != 5*time.Second {
			t.Errorf("sleep %d = %v, want 5s", i, d)
		}
	}
}

// GitHub asks clients to back off when they poll too fast; ignoring slow_down
// gets the client throttled for the rest of the flow.
func TestDeviceFlow_SlowDownIncreasesInterval(t *testing.T) {
	c := newFakeClock()
	_, flow := deviceServer(t, c,
		`{"error":"slow_down"}`,
		`{"access_token":"gho_token","token_type":"bearer"}`,
	)

	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := flow.Wait(context.Background(), code); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(c.sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2", c.sleeps)
	}
	if want := 5 * time.Second; c.sleeps[0] != want {
		t.Errorf("first sleep = %v, want %v", c.sleeps[0], want)
	}
	if want := 10 * time.Second; c.sleeps[1] != want {
		t.Errorf("after slow_down sleep = %v, want %v (interval + 5s)", c.sleeps[1], want)
	}
}

func TestDeviceFlow_Errors(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     error
	}{
		{"access denied", `{"error":"access_denied"}`, ErrAccessDenied},
		{"expired by server", `{"error":"expired_token"}`, ErrExpired},
		{"unknown error code", `{"error":"incorrect_device_code"}`, ErrDeviceFlowFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClock()
			_, flow := deviceServer(t, c, tc.response)
			code, err := flow.Start(context.Background())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := flow.Wait(context.Background(), code); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The client must stop on its own deadline, not poll forever if the server
// never reports expiry.
func TestDeviceFlow_StopsAtDeadline(t *testing.T) {
	c := newFakeClock()
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/device/code") {
			_, _ = io.WriteString(w, `{"device_code":"dev-123","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":20,"interval":5}`)
			return
		}
		polls++
		_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
	}))
	t.Cleanup(srv.Close)

	flow := &DeviceFlow{
		ClientID: "cid_test", HTTP: srv.Client(),
		DeviceCodeURL: srv.URL + "/login/device/code",
		TokenURL:      srv.URL + "/login/oauth/access_token",
		Now:           c.Now, Sleep: c.Sleep,
	}
	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The server always answers authorization_pending, so only the client-side
	// deadline can end the loop: 20s at 5s intervals is 4 polls.
	if _, err := flow.Wait(context.Background(), code); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if polls != 4 {
		t.Errorf("polled %d times, want 4 (20s deadline at 5s intervals)", polls)
	}
	if len(c.sleeps) != 4 {
		t.Errorf("slept %v, want 4 waits", c.sleeps)
	}
}

func TestDeviceFlow_ContextCancelled(t *testing.T) {
	c := newFakeClock()
	_, flow := deviceServer(t, c, `{"error":"authorization_pending"}`)
	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := flow.Wait(ctx, code); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A transport-level failure is not a protocol error; it must surface as-is so
// the operator sees "connection refused", not "authorization failed".
func TestDeviceFlow_ServerErrorIsNotProtocolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	t.Cleanup(srv.Close)
	c := newFakeClock()
	flow := &DeviceFlow{
		ClientID: "cid_test", HTTP: srv.Client(),
		DeviceCodeURL: srv.URL, TokenURL: srv.URL,
		Now: c.Now, Sleep: c.Sleep,
	}
	if _, err := flow.Start(context.Background()); err == nil {
		t.Fatal("Start must fail on a 502")
	}
}

func TestDeviceFlow_StartRequestShape(t *testing.T) {
	var form url.Values
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		_, _ = io.WriteString(w, `{"device_code":"d","user_code":"u","verification_uri":"v","expires_in":1,"interval":1}`)
	}))
	t.Cleanup(srv.Close)

	c := newFakeClock()
	flow := &DeviceFlow{
		ClientID: "cid_test", Scopes: []string{"public_repo", "read:org"},
		HTTP: srv.Client(), DeviceCodeURL: srv.URL, TokenURL: srv.URL,
		Now: c.Now, Sleep: c.Sleep,
	}
	if _, err := flow.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := form.Get("client_id"); got != "cid_test" {
		t.Errorf("client_id = %q", got)
	}
	if got := form.Get("scope"); got != "public_repo read:org" {
		t.Errorf("scope = %q, want scopes joined by a space", got)
	}
	if !strings.Contains(accept, "json") {
		t.Errorf("Accept = %q, want application/json so GitHub answers with JSON", accept)
	}
}

// A missing client_id is a build-configuration error; failing loudly at the
// start beats sending an empty client_id and getting an opaque 404.
func TestDeviceFlow_RequiresClientID(t *testing.T) {
	flow := &DeviceFlow{}
	if _, err := flow.Start(context.Background()); err == nil {
		t.Fatal("Start must fail without a client_id")
	}
}

func TestDeviceFlow_DefaultIntervalWhenServerOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"device_code":"d","user_code":"u","verification_uri":"v","expires_in":900}`)
	}))
	t.Cleanup(srv.Close)
	c := newFakeClock()
	flow := &DeviceFlow{ClientID: "cid", HTTP: srv.Client(), DeviceCodeURL: srv.URL, TokenURL: srv.URL, Now: c.Now, Sleep: c.Sleep}
	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if code.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want the 5s default", code.Interval)
	}
}

// A wrong client_id is the single most likely misconfiguration — it is the only
// thing this tool needs configured — and GitHub reports it as a bare 404.
// Observed against the real endpoint, so the hint is not speculation.
func TestDeviceFlow_StartExplainsUnknownClientID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Not Found"}`)
	}))
	t.Cleanup(srv.Close)

	c := newFakeClock()
	flow := &DeviceFlow{
		ClientID: "Ov23liFAKEfake", HTTP: srv.Client(),
		DeviceCodeURL: srv.URL, TokenURL: srv.URL, Now: c.Now, Sleep: c.Sleep,
	}
	_, err := flow.Start(context.Background())
	if !errors.Is(err, ErrDeviceFlowFailed) {
		t.Fatalf("err = %v, want ErrDeviceFlowFailed", err)
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Errorf("error must name client_id as the likely cause, got: %v", err)
	}
}

// If expires_in is absent the deadline would otherwise be "now", and Wait
// would give up before polling once.
func TestDeviceFlow_DefaultExpiryWhenServerOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"device_code":"d","user_code":"u","verification_uri":"v"}`)
	}))
	t.Cleanup(srv.Close)

	c := newFakeClock()
	flow := &DeviceFlow{
		ClientID: "cid", HTTP: srv.Client(), DeviceCodeURL: srv.URL, TokenURL: srv.URL,
		Now: c.Now, Sleep: c.Sleep,
	}
	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if code.ExpiresIn <= 0 {
		t.Fatalf("ExpiresIn = %v, want a positive default", code.ExpiresIn)
	}
	if code.ExpiresIn != 15*time.Minute {
		t.Errorf("ExpiresIn = %v, want 15m", code.ExpiresIn)
	}
}
