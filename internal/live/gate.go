package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/DnzzL/herdr-huddle/internal/github"
)

// Verifier resolves a GitHub token to the login it belongs to.
type Verifier interface {
	Login(ctx context.Context, token string) (string, error)
}

// GitHubVerifier answers from GitHub's own /user endpoint — the only source
// that can say whose token this is. Nothing this side asserts about a
// connection is worth anything until GitHub has confirmed it.
type GitHubVerifier struct {
	// BaseURL is injectable for tests, as everywhere else in this repo.
	BaseURL string
}

func (v GitHubVerifier) Login(ctx context.Context, token string) (string, error) {
	return (&github.Client{Token: token, BaseURL: v.BaseURL}).Viewer(ctx)
}

// Approver answers the door for a proven login the allowlist does not know.
//
// ADR-007 makes this the default way in: the allowlist was written before
// anyone had asked to join, and this asks the same operator the same question
// at the moment there is something to decide. An error means the question
// could not be put — an unattended `serve`, a closed stdin — and is a refusal,
// never an admission.
type Approver interface {
	Approve(ctx context.Context, login string) (bool, error)
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, login string) (bool, error)

func (f ApproverFunc) Approve(ctx context.Context, login string) (bool, error) { return f(ctx, login) }

// Gate decides who may join a live share.
//
// ADR-006 makes this the entire boundary: the tunnel in front of the stream
// cannot carry Cloudflare Access, so a public endpoint is exactly as open as
// this check. It therefore runs before the first frame is observed.
//
// ADR-007 gives it a second way to say yes. An allowlist entry is a decision
// already made; Admit is the same decision made live. Both end in the same
// place — the login on the share's allowlist — so the stream door and the
// comment door are never governed by two different lists.
type Gate struct {
	// Verify asks GitHub whose token is presented.
	Verify Verifier
	// Allowlist is the share's logins, read once when the server starts and
	// extended by every admission.
	Allowlist []string
	// Admit is asked about a proven login the allowlist does not hold. Nil is
	// a closed door: the allowlist or nothing.
	Admit Approver
	// Open admits any login GitHub confirms, without asking. The link is then
	// the whole invitation — the operator's choice, never the default
	// (ADR-007). An open admission lasts as long as the server does and is
	// never persisted: being convenient for the length of a huddle is not the
	// same decision as trusting somebody's pull-request comments forever.
	Open bool
	// Remember persists a newly admitted login onto the share, so that the
	// poller delivers their comments too and a reconnect does not knock twice.
	// It is called for a knock the operator answered and for nothing else.
	// A failure to persist is logged, not fatal: the person is already in.
	Remember func(login string) error
	// Log receives one line per door event.
	Log func(format string, args ...any)

	// mu serialises the door. Two people knocking at once must become two
	// questions in a row on one terminal, not two prompts writing over each
	// other — and the allowlist below is read and written from every
	// connection's goroutine.
	mu sync.Mutex
}

// ErrNoToken is refused before GitHub is contacted at all.
var ErrNoToken = errors.New("live: no GitHub token was presented")

// notAllowed marks the gate having identified somebody and decided against
// them. It is the *only* gate failure a client must not retry.
//
// The distinction is load-bearing for reconnect (ADR-008): GitHub being
// unreachable, an operator who stepped away from the knock, a share with no
// allowlist yet — all of those may work on the next attempt, and treating them
// as refusals would end a huddle over a network blip. Being turned away is the
// one that will not change by asking again.
type notAllowed struct{ error }

// NotAllowed reports whether an error is the gate turning a known person away.
func NotAllowed(err error) bool {
	var turned notAllowed
	return errors.As(err, &turned)
}

// Authorize checks a joiner's token and returns the login it proved.
//
// The errors are written for the joiner, not the log: they are the only
// explanation that person will ever get, and "you are not on this share's
// allowlist" is actionable where silence is not.
func (g *Gate) Authorize(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrNoToken
	}
	if g.Verify == nil {
		return "", errors.New("live: the gate has no verifier, so nobody may join")
	}
	if !g.canAdmit() && len(g.Allowlist) == 0 {
		return "", errors.New("live: this share has an empty allowlist and no way to let anyone in, so nobody may join")
	}
	login, err := g.Verify.Login(ctx, token)
	if err != nil {
		return "", fmt.Errorf("live: GitHub could not confirm the token: %w", err)
	}
	if g.known(login) {
		return login, nil
	}
	if g.Open {
		g.admit(login, "admitted @%s for this session: the share is open, so the link was the invitation")
		return login, nil
	}
	if g.Admit == nil {
		return "", notAllowed{fmt.Errorf("live: @%s is not on this share's allowlist", login)}
	}

	// One question at a time: the approver is a person at a terminal.
	g.mu.Lock()
	defer g.mu.Unlock()
	// Someone may have been let in while this connection waited for the lock.
	if g.knownLocked(login) {
		return login, nil
	}
	ok, err := g.Admit.Approve(ctx, login)
	if err != nil {
		g.logf("could not ask about @%s: %v", login, err)
		return "", fmt.Errorf("live: nobody could be asked whether to let @%s in: %w", login, err)
	}
	if !ok {
		g.logf("refused @%s at the door", login)
		return "", notAllowed{fmt.Errorf("live: @%s was not let into this share", login)}
	}
	g.rememberLocked(login, "let @%s in")
	return login, nil
}

// canAdmit reports whether the gate has any way to say yes to a login the
// allowlist does not already hold.
func (g *Gate) canAdmit() bool { return g.Open || g.Admit != nil }

func (g *Gate) known(login string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.knownLocked(login)
}

func (g *Gate) knownLocked(login string) bool {
	for _, allowed := range g.Allowlist {
		if strings.EqualFold(allowed, login) {
			return true
		}
	}
	return false
}

// admit adds the login to this server's allowlist, for as long as it runs.
// The in-memory list is what stops the next connection from asking again.
func (g *Gate) admit(login, format string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.knownLocked(login) {
		return
	}
	g.admitLocked(login, format)
}

func (g *Gate) admitLocked(login, format string) {
	g.Allowlist = append(g.Allowlist, login)
	g.logf(format, login)
}

// rememberLocked admits the login and writes it onto the share.
//
// Only an answered knock reaches here, and the difference from admitLocked is
// the whole point: the persisted list is what makes that person's
// pull-request comments deliverable — ADR-005's one-list rule — and that is a
// decision about somebody, not a convenience for the length of a session. An
// --open admission is deliberately not written (ADR-007).
func (g *Gate) rememberLocked(login, format string) {
	g.admitLocked(login, format)
	if g.Remember == nil {
		return
	}
	if err := g.Remember(login); err != nil {
		g.logf("@%s is in, but could not be written to the share record, so their comments will not be delivered until you re-share: %v", login, err)
	}
}

func (g *Gate) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log(format, args...)
	}
}
