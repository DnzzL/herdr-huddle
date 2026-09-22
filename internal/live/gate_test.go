package live

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// verifierStub answers the one question the gate asks GitHub: whose token is
// this?
type verifierStub struct {
	login string
	err   error
	// seen records the tokens that were verified, in order.
	seen []string
}

func (v *verifierStub) Login(_ context.Context, token string) (string, error) {
	v.seen = append(v.seen, token)
	return v.login, v.err
}

func TestGateLetsAnAllowlistedLoginThrough(t *testing.T) {
	gate := &Gate{Verify: &verifierStub{login: "PagBRL"}, Allowlist: []string{"DnzzL", "pagbrl"}}
	login, err := gate.Authorize(context.Background(), "token-1")
	if err != nil {
		t.Fatalf("Authorize errored: %v", err)
	}
	if login != "PagBRL" {
		t.Errorf("login = %q, want the login GitHub reported", login)
	}
}

// GitHub logins are case-insensitive, and a hand-typed allowlist entry should
// not be able to lock the real person out.
func TestGateMatchesTheAllowlistCaseInsensitively(t *testing.T) {
	gate := &Gate{Verify: &verifierStub{login: "PagBRL"}, Allowlist: []string{"pagbrl"}}
	if _, err := gate.Authorize(context.Background(), "t"); err != nil {
		t.Errorf("Authorize errored for a case difference: %v", err)
	}
}

// Nothing is presented, so nothing can be checked.
func TestGateRefusesAMissingToken(t *testing.T) {
	verify := &verifierStub{login: "pagbrl"}
	gate := &Gate{Verify: verify, Allowlist: []string{"pagbrl"}}
	_, err := gate.Authorize(context.Background(), "   ")
	if err == nil {
		t.Fatal("a missing token must be refused")
	}
	if len(verify.seen) != 0 {
		t.Errorf("GitHub was asked about an empty token: %v", verify.seen)
	}
}

// A token GitHub cannot confirm is refused with GitHub's reason, because
// "who are you" and "is this token dead" are different problems to debug.
func TestGateRefusesATokenGitHubCannotConfirm(t *testing.T) {
	gate := &Gate{
		Verify:    &verifierStub{err: errors.New("github: 401 Bad credentials")},
		Allowlist: []string{"pagbrl"},
	}
	_, err := gate.Authorize(context.Background(), "dead")
	if err == nil {
		t.Fatal("an unverifiable token must be refused")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want GitHub's reason to survive", err)
	}
}

// The same rule at startup: an empty allowlist means nobody can join, so
// saying so beats holding a port that leads nowhere.
func TestServerRefusesToStartWithAnEmptyAllowlist(t *testing.T) {
	srv := &Server{
		Pane: "w1:p1", Observe: &fakeObserver{},
		Gate: &Gate{Verify: &verifierStub{login: "DnzzL"}, Allowlist: nil},
	}
	err := srv.serveGuard()
	if err == nil {
		t.Fatal("an empty allowlist must stop the server from starting")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error = %v, want it to name the allowlist", err)
	}
}

// The whole point of the gate: a real, valid login that this share never
// allowed.
func TestGateRefusesALoginThatIsNotAllowlisted(t *testing.T) {
	gate := &Gate{Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"DnzzL", "pagbrl"}}
	_, err := gate.Authorize(context.Background(), "valid-token")
	if err == nil {
		t.Fatal("a login off the allowlist must be refused")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error = %v, want it to name the refused login", err)
	}
}

// Mirrors the thread's marker rule: an empty allowlist refuses everything
// rather than trusting by default. A public endpoint with an empty list is a
// bug to surface, not a door to open.
func TestGateRefusesEveryoneWhenTheAllowlistIsEmpty(t *testing.T) {
	gate := &Gate{Verify: &verifierStub{login: "DnzzL"}, Allowlist: nil}
	_, err := gate.Authorize(context.Background(), "t")
	if err == nil {
		t.Fatal("an empty allowlist must refuse, like the comment path does")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v, want it to say the allowlist is empty, not that this login is missing from it", err)
	}
}

// The gate is the boundary for a public endpoint, so a server that lost it
// must not start rather than run open.
func TestServerRefusesToStartWithoutAGate(t *testing.T) {
	srv := &Server{Pane: "w1:p1", Observe: &fakeObserver{}}
	err := srv.serveGuard()
	if err == nil {
		t.Fatal("a server with no gate must refuse to start")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Errorf("error = %v, want it to name the gate", err)
	}
}

// A garbled first record must not have its contents echoed: the hello carries
// the GitHub token, and parse errors quote what they failed on. The reason a
// joiner is dropped goes to the operator's log; the token never does.
func TestTheHelloErrorNeverEchoesTheToken(t *testing.T) {
	secret := "gho_super_secret_token_value"
	obs := &fakeObserver{}
	srv := newTestServer(obs)
	srv.GateTimeout = 2 * time.Second
	// The server logs its refusal *after* writing it to the joiner, so the
	// log is read from another goroutine: guard it, and wait for the line
	// rather than racing it.
	var mu sync.Mutex
	var logged []string
	srv.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	addr, stop := startServer(t, srv)
	defer stop()

	conn := dialSilent(t, addr)
	for _, garbage := range []string{
		"not json at all " + secret + "\n",
		`[` + secret + "]" + "\n",
		`{"bytes":"x",` + `"token":"` + secret + `"` + "\n",
	} {
		if _, err := io.WriteString(conn, garbage); err != nil {
			t.Fatalf("write: %v", err)
		}
		break // one connection, one first record
	}

	var out bytes.Buffer
	err := Render(conn, &out)
	joined := out.String() + "\n"
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := len(logged) > 0
		if done {
			for _, line := range logged {
				joined += line + "\n"
			}
		}
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		joined += err.Error() + "\n"
	}
	if strings.Contains(joined, secret) {
		t.Errorf("the token leaked into what the joiner or the log was told:\n%s", joined)
	}
	if err == nil {
		t.Error("a garbled first record must still be refused")
	}
}
