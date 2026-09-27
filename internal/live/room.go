package live

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// Viewport bounds. A joiner asks for its own terminal size (ADR-007) and the
// server believes it only this far: the numbers arrive from the network, and
// they become arguments to a child process that allocates a grid from them.
const (
	MinCols = 20
	MaxCols = 500
	MinRows = 5
	MaxRows = 200
)

// clampViewport turns what a joiner asked for into what will be observed.
// Zero means "no preference", which is the server's own default — a client
// that cannot read its window size must not be letterboxed into 20x5.
func clampViewport(cols, rows, defCols, defRows int) (int, int) {
	if cols <= 0 {
		cols = defCols
	}
	if rows <= 0 {
		rows = defRows
	}
	if cols <= 0 {
		cols = DefaultCols
	}
	if rows <= 0 {
		rows = DefaultRows
	}
	return clamp(cols, MinCols, MaxCols), clamp(rows, MinRows, MaxRows)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Statuser reports what the agent in a pane is doing.
//
// It is the piece ADR-005 named and did not build: a joiner watching a silent
// pane cannot tell a thinking agent from a finished one, and "the agent is
// waiting on its operator" is the single most useful thing the room can say.
type Statuser interface {
	Status(ctx context.Context, pane string) (string, error)
}

// HerdrStatuser reads the status from the same CLI everything else here uses.
type HerdrStatuser struct {
	// Bin overrides the herdr executable, as elsewhere.
	Bin string
}

func (h HerdrStatuser) Status(ctx context.Context, pane string) (string, error) {
	agent, err := (&herdr.Client{Bin: h.Bin}).Agent(ctx, pane)
	if err != nil {
		return "", err
	}
	return agent.Status, nil
}

// DefaultStatusInterval is how often the agent's state is re-read while anyone
// is in the room. Nothing is read when the room is empty.
const DefaultStatusInterval = 2 * time.Second

// seatBuffer is how many records may be owed to one joiner before that joiner
// is considered gone.
//
// A joiner who cannot keep up is ended rather than slowed down, and ending
// them is what frees their observer. Dropping records instead is not an option
// on this channel: it carries frames, and a dropped frame is a viewport that
// never paints correctly again.
const seatBuffer = 256

// seat is one joiner's place in the room, and the only thing allowed to write
// to their connection.
//
// A connection has three writers without it — the frame pump, the delivery
// acknowledgement, and the room broadcast — so the single goroutine below is
// not a convenience but the thing that makes those writes safe.
type seat struct {
	login string
	// typingUntil is when this seat's typing claim lapses. Guarded by the
	// room's lock, because it is only ever read while building a roster.
	typingUntil time.Time
	out         chan []byte
	// closed is shut when the seat's writer has stopped, so send never blocks
	// on a departed joiner.
	closed chan struct{}
	once   sync.Once
}

func newSeat(login string) *seat {
	return &seat{login: login, out: make(chan []byte, seatBuffer), closed: make(chan struct{})}
}

// send queues one record. It reports false when the joiner is gone or so far
// behind that they should be.
func (s *seat) send(line []byte) bool {
	select {
	case <-s.closed:
		return false
	default:
	}
	select {
	case s.out <- line:
		return true
	case <-s.closed:
		return false
	default:
		// Too far behind to catch up. Closing is the honest outcome: the
		// alternative is an unbounded queue holding frames for someone who is
		// not reading them.
		s.close()
		return false
	}
}

// sendFrame queues a record built from a Frame.
func (s *seat) sendFrame(f Frame) bool {
	line, err := json.Marshal(f)
	if err != nil {
		return true // Frame is a plain struct; this cannot fail.
	}
	return s.send(append(line, '\n'))
}

func (s *seat) close() { s.once.Do(func() { close(s.closed) }) }

// room is who is connected and what the agent is doing.
//
// It holds no connections, only seats, so nothing here knows about sockets:
// that is what lets the whole of presence be tested without one.
type room struct {
	mu     sync.Mutex
	seats  []*seat
	agent  string
	thread string
	// host is the operator's login: the person whose machine and agent this
	// is. They are in the room without being connected to it — they are at the
	// pane — so a joiner told "you are the only one here" while the operator
	// watches is being told something false.
	host string
}

// TypingTTL is how long a typing claim stands without being renewed.
//
// It exists because the claim is made by a client that can die mid-sentence:
// without a lapse, a joiner who closed their laptop would appear to be typing
// for the rest of the session, and a signal that is sometimes a ghost is worse
// than no signal.
const TypingTTL = 5 * time.Second

func (r *room) join(s *seat) {
	r.mu.Lock()
	r.seats = append(r.seats, s)
	r.mu.Unlock()
	r.announce()
}

func (r *room) leave(s *seat) {
	r.mu.Lock()
	kept := r.seats[:0]
	for _, other := range r.seats {
		if other != s {
			kept = append(kept, other)
		}
	}
	r.seats = kept
	r.mu.Unlock()
	r.announce()
}

// occupied reports whether anyone is in the room. The status watcher reads it
// so an empty room costs no herdr calls.
func (r *room) occupied() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seats) > 0
}

// setTyping records that somebody has started or stopped composing.
//
// It announces only when the *set* of typists changes, not on every renewal:
// the client renews while the person keeps typing, and a repaint per keystroke
// would be the room shouting.
func (r *room) setTyping(s *seat, on bool) {
	r.mu.Lock()
	before := r.typistsLocked()
	if on {
		s.typingUntil = time.Now().Add(TypingTTL)
	} else {
		s.typingUntil = time.Time{}
	}
	changed := !sameLogins(before, r.typistsLocked())
	r.mu.Unlock()
	if changed {
		r.announce()
	}
}

// expireTyping drops claims that have lapsed, and tells the room when any did.
//
// The comparison is against the raw claim, not against typistsLocked: that
// helper already hides a lapsed claim, so asking it what changed would always
// answer "nothing" and the room would keep showing somebody typing long after
// they stopped.
func (r *room) expireTyping() {
	r.mu.Lock()
	now := time.Now()
	lapsed := false
	for _, s := range r.seats {
		if !s.typingUntil.IsZero() && !s.typingUntil.After(now) {
			s.typingUntil = time.Time{}
			lapsed = true
		}
	}
	r.mu.Unlock()
	if lapsed {
		r.announce()
	}
}

// typistsLocked is who is composing right now. The caller holds mu.
func (r *room) typistsLocked() []string {
	now := time.Now()
	var list []string
	for _, s := range r.seats {
		if !s.typingUntil.IsZero() && s.typingUntil.After(now) {
			list = append(list, s.login)
		}
	}
	return dedupe(list)
}

func sameLogins(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// setAgent records the agent's status and announces it if it moved. Returning
// early on an unchanged status is what keeps a 2-second tick from being a
// 2-second broadcast.
func (r *room) setAgent(status string) {
	r.mu.Lock()
	if r.agent == status {
		r.mu.Unlock()
		return
	}
	r.agent = status
	r.mu.Unlock()
	r.announce()
}

// membersLocked is the roster as the room records carry it: one entry per
// person, sorted, however many windows they have open. The caller holds mu.
func (r *room) membersLocked() []string {
	list := make([]string, 0, len(r.seats))
	for _, s := range r.seats {
		list = append(list, s.login)
	}
	return dedupe(list)
}

// dedupe folds a list of logins to one entry per person, sorted — however many
// windows they have open, and whatever case GitHub spelled them in.
func dedupe(logins []string) []string {
	seen := make(map[string]bool, len(logins))
	list := make([]string, 0, len(logins))
	for _, login := range logins {
		key := strings.ToLower(login)
		if seen[key] {
			continue
		}
		seen[key] = true
		list = append(list, login)
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i]) < strings.ToLower(list[j]) })
	return list
}

// announce sends every seat the room record, each addressed to its own
// occupant: You is what lets a joiner find itself in the roster.
func (r *room) announce() {
	r.mu.Lock()
	base := Frame{
		Type:    TypeRoom,
		Members: r.membersLocked(),
		Agent:   r.agent,
		Thread:  r.thread,
		Host:    r.host,
	}
	typists := r.typistsLocked()
	seats := append([]*seat(nil), r.seats...)
	r.mu.Unlock()

	for _, s := range seats {
		record := base
		record.You = s.login
		// Nobody is told they are typing: a client knows what its own hands
		// are doing, and echoing it back would fight the input line.
		record.Typing = without(typists, s.login)
		s.sendFrame(record)
	}
}

// without drops one login from a list, case-insensitively.
func without(logins []string, drop string) []string {
	var kept []string
	for _, login := range logins {
		if !strings.EqualFold(login, drop) {
			kept = append(kept, login)
		}
	}
	return kept
}

// broadcast sends one record to everyone.
func (r *room) broadcast(f Frame) {
	r.mu.Lock()
	seats := append([]*seat(nil), r.seats...)
	r.mu.Unlock()
	for _, s := range seats {
		s.sendFrame(f)
	}
}

// watchAgent keeps the room's agent status current for as long as anyone is
// in it. A status that cannot be read becomes StatusUnknown rather than a
// stale one: a joiner told "working" about an agent that is gone is worse off
// than one told nothing.
// watchAgent is the room's heartbeat: it lapses stale typing claims and keeps
// the agent's status current, for as long as anyone is in the room.
//
// The two are on one ticker rather than two because they share the same
// condition — an empty room costs nothing — and because a server with no
// Statuser must still lapse typing, which is why the nil check below guards
// the status call and not the loop.
func (r *room) watchAgent(ctx context.Context, pane string, status Statuser, every time.Duration) {
	if every <= 0 {
		every = DefaultStatusInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.occupied() {
				continue
			}
			r.expireTyping()
			if status == nil || pane == "" {
				continue
			}
			got, err := status.Status(ctx, pane)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				got = herdr.StatusUnknown
			}
			r.setAgent(got)
		}
	}
}
