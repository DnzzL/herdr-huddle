package live

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// approverStub is the operator at their terminal: it records who it was asked
// about and answers the way the test says.
type approverStub struct {
	mu    sync.Mutex
	asked []string
	yes   bool
	err   error
}

func (a *approverStub) Approve(_ context.Context, login string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, login)
	return a.yes, a.err
}

func (a *approverStub) askedAbout() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

// rememberer stands in for the share record the admission is written to.
type rememberer struct {
	mu     sync.Mutex
	logins []string
	err    error
}

func (r *rememberer) remember(login string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.logins = append(r.logins, login)
	return nil
}

func (r *rememberer) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logins...)
}

// The whole point of ADR-007's door: somebody nobody predicted turns up with a
// link, proves who they are, and one answer from the operator lets them in —
// onto the allowlist, which is also the list the comment path reads.
func TestKnockAdmitsAndRemembers(t *testing.T) {
	approver := &approverStub{yes: true}
	record := &rememberer{}
	gate := &Gate{
		Verify:    &verifierStub{login: "newcomer"},
		Allowlist: []string{"operator"},
		Admit:     approver,
		Remember:  record.remember,
	}

	login, err := gate.Authorize(context.Background(), "their-token")
	if err != nil {
		t.Fatalf("an approved knock must be admitted: %v", err)
	}
	if login != "newcomer" {
		t.Errorf("login = %q, want the login GitHub proved", login)
	}
	if got := approver.askedAbout(); len(got) != 1 || got[0] != "newcomer" {
		t.Errorf("asked about %v, want exactly [newcomer]", got)
	}
	if got := record.recorded(); len(got) != 1 || got[0] != "newcomer" {
		t.Errorf("recorded %v on the share, want [newcomer] — their comments depend on it", got)
	}
}

// A second window, or a reconnect after a dropped tunnel, must not make the
// operator answer the same question again.
func TestKnockIsAskedOncePerLogin(t *testing.T) {
	approver := &approverStub{yes: true}
	gate := &Gate{Verify: &verifierStub{login: "newcomer"}, Allowlist: []string{"operator"}, Admit: approver}

	for i := 0; i < 3; i++ {
		if _, err := gate.Authorize(context.Background(), "their-token"); err != nil {
			t.Fatalf("connection %d refused: %v", i+1, err)
		}
	}
	if got := approver.askedAbout(); len(got) != 1 {
		t.Errorf("asked %d times, want once: %v", len(got), got)
	}
}

// A refusal has to be a refusal — and it must not leave the login on the list.
func TestKnockRefusedStaysOut(t *testing.T) {
	approver := &approverStub{yes: false}
	record := &rememberer{}
	gate := &Gate{
		Verify:    &verifierStub{login: "stranger"},
		Allowlist: []string{"operator"},
		Admit:     approver,
		Remember:  record.remember,
	}

	_, err := gate.Authorize(context.Background(), "their-token")
	if err == nil {
		t.Fatal("a refused knock must be an error, not an empty screen")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error = %v, want it to name who was refused", err)
	}
	if got := record.recorded(); len(got) != 0 {
		t.Errorf("recorded %v, want nothing: nobody was let in", got)
	}
	// The next connection must ask again, not inherit the refusal as a ban.
	approver.yes = true
	if _, err := gate.Authorize(context.Background(), "their-token"); err != nil {
		t.Errorf("a second knock must be answerable: %v", err)
	}
}

// Nobody to ask is a refusal, never an admission: an unattended `serve` with a
// closed stdin must not become an open one.
func TestUnanswerableKnockRefuses(t *testing.T) {
	gate := &Gate{
		Verify:    &verifierStub{login: "stranger"},
		Allowlist: []string{"operator"},
		Admit:     &approverStub{yes: true, err: errors.New("stdin is closed")},
	}
	if _, err := gate.Authorize(context.Background(), "their-token"); err == nil {
		t.Fatal("a knock nobody could answer must refuse")
	}
}

// --open is the operator deciding once that the link is the invitation, for
// the length of this huddle — and not one minute longer. Writing a passer-by
// to the share's allowlist would make their pull-request comments deliverable
// forever, which is a different decision from the one --open expresses.
func TestOpenDoorAdmitsWithoutAskingAndWithoutRemembering(t *testing.T) {
	approver := &approverStub{}
	record := &rememberer{}
	gate := &Gate{
		Verify:   &verifierStub{login: "guest"},
		Open:     true,
		Admit:    approver,
		Remember: record.remember,
	}
	if _, err := gate.Authorize(context.Background(), "their-token"); err != nil {
		t.Fatalf("an open door must admit a proven identity: %v", err)
	}
	if got := approver.askedAbout(); len(got) != 0 {
		t.Errorf("asked %v, want nobody asked on an open door", got)
	}
	if got := record.recorded(); len(got) != 0 {
		t.Errorf("wrote %v to the share; an open admission lasts only as long as the server", got)
	}
	// It still holds for this server, so a second connection is not a second
	// round trip to GitHub's allowlist check.
	if !gate.known("guest") {
		t.Error("the admission must last for this session")
	}
}

// --closed is today's behaviour, and it must still be reachable: the
// allowlist, and no question.
func TestClosedDoorRefusesWithoutAsking(t *testing.T) {
	gate := &Gate{Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"operator"}}
	_, err := gate.Authorize(context.Background(), "their-token")
	if err == nil {
		t.Fatal("a closed door must refuse a login it does not hold")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error = %v, want it to say why", err)
	}
}

// A share with nobody on it and no way in is a startup failure, not a server
// that accepts connections it will always refuse.
func TestEmptyAllowlistWithAClosedDoorRefusesToStart(t *testing.T) {
	srv := newTestServer(&fakeObserver{})
	srv.Gate = &Gate{Verify: &verifierStub{login: "anyone"}}
	if err := srv.serveGuard(); err == nil {
		t.Fatal("an empty allowlist behind a closed door must refuse to start")
	}
	// The same empty list with a door that can open is an ordinary beginning.
	srv.Gate.Admit = &approverStub{yes: true}
	if err := srv.serveGuard(); err != nil {
		t.Errorf("an empty allowlist with a knocking door is a fine start: %v", err)
	}
}

// The share record cannot always be written — a retired share, a read-only
// config dir — and that must not undo an admission the operator just made.
func TestAnAdmissionSurvivesAFailedRecord(t *testing.T) {
	gate := &Gate{
		Verify:    &verifierStub{login: "guest"},
		Allowlist: []string{"operator"},
		Admit:     &approverStub{yes: true},
		Remember:  (&rememberer{err: errors.New("no such share")}).remember,
	}
	if _, err := gate.Authorize(context.Background(), "their-token"); err != nil {
		t.Fatalf("the person is already in; a failed write must not eject them: %v", err)
	}
}
