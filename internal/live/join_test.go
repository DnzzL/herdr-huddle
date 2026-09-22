package live

import (
	"context"
	"strings"
	"testing"
)

// The joiner's whole job: present a token, draw what comes back.
func TestJoinDrawsTheStream(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "hello from the pane")}}
	addr, stop := serveTest(t, obs)
	defer stop()

	var out strings.Builder
	if err := Join(context.Background(), addr, "test-token", &out); err != nil {
		t.Fatalf("Join errored: %v", err)
	}
	if out.String() != "hello from the pane" {
		t.Errorf("drew %q, want the pane's frames", out.String())
	}
}

// The gate's refusal is the joiner's error message; silence here would send
// them debugging their terminal.
func TestJoinSurfacesARefusal(t *testing.T) {
	obs := &fakeObserver{}
	srv := newTestServer(obs)
	srv.Gate = &Gate{Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"pagbrl"}}
	addr, stop := startServer(t, srv)
	defer stop()

	err := Join(context.Background(), addr, "some-token", &strings.Builder{})
	if err == nil {
		t.Fatal("a refused join must be an error, not an empty screen")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error = %v, want the gate's reason", err)
	}
	if obs.openedCount() != 0 {
		t.Errorf("opened %d streams for a refused joiner, want 0", obs.openedCount())
	}
}

// `serve` prints an https:// tunnel URL; tests and LAN use give a bare
// address. Either arrives as one argument, so the scheme is inferred rather
// than typed.
func TestWSEndpoint(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"127.0.0.1:8787", "ws://127.0.0.1:8787"},
		{"http://127.0.0.1:8787", "ws://127.0.0.1:8787"},
		{"https://lobby-shepherd.trycloudflare.com", "wss://lobby-shepherd.trycloudflare.com"},
		{"ws://host:1", "ws://host:1"},
		{"wss://host", "wss://host"},
		{"https://host/some/path", "wss://host/some/path"},
	}
	for _, c := range cases {
		got, err := wsEndpoint(c.in)
		if err != nil {
			t.Errorf("wsEndpoint(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("wsEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An empty target is a usage error, not a silent connection to nowhere.
func TestWSEndpointRefusesAnEmptyTarget(t *testing.T) {
	if _, err := wsEndpoint("  "); err == nil {
		t.Error("an empty endpoint must be refused")
	}
}

// The token is the gate's whole credential: handing it to a network in the
// clear would defeat the pairing it exists for (ADR-006 — wss:// or loopback
// only).
func TestJoinRefusesToSendATokenInTheClear(t *testing.T) {
	cases := []struct{ endpoint, want string }{
		{"ws://192.168.1.50:8787", "clear"},
		{"ws://example.invalid:8787", "clear"},
		{"ws://10.0.0.1:8787", "clear"},
	}
	for _, c := range cases {
		err := Join(context.Background(), c.endpoint, "secret-token", &strings.Builder{})
		if err == nil {
			t.Errorf("Join(%q) succeeded, must refuse", c.endpoint)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Join(%q) error = %v, want it to say the token would travel in the clear", c.endpoint, err)
		}
	}
}

// Loopback and TLS are the two ways the token is allowed to travel.
func TestTokenMayTravelOverLoopbackAndTLS(t *testing.T) {
	for _, endpoint := range []string{"ws://127.0.0.1:8787", "ws://localhost:1", "ws://[::1]:1", "wss://anywhere.example"} {
		if err := tokenTravelsSafely(endpoint); err != nil {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"ws://192.168.1.50:1", "ws://8.8.8.8:1"} {
		if err := tokenTravelsSafely(endpoint); err == nil {
			t.Errorf("endpoint %q must not be allowed to carry the token", endpoint)
		}
	}
}
