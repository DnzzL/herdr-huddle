// Command herdr-huddle turns a live agent session into a GitHub draft PR two
// people can plan in, steer, and keep as a record.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/auth"
	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/live"
	"github.com/DnzzL/herdr-huddle/internal/poll"
	"github.com/DnzzL/herdr-huddle/internal/repo"
	"github.com/DnzzL/herdr-huddle/internal/session"
	"github.com/DnzzL/herdr-huddle/internal/share"
	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// clientID is the OAuth App client id, injected at build time:
//
//	go build -ldflags "-X main.clientID=Iv1.0123456789abcdef"
//
// It is not a secret. In the device flow the client id is public by design —
// that is exactly why this flow was chosen over one needing a client secret.
var clientID string

const (
	serviceName = "herdr-huddle"
	accountName = "github.com"
)

// errUsage marks a misuse of the command line, which exits 2 so a wrapper can
// tell it apart from an operational failure.
var errUsage = errors.New("usage")

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "herdr-huddle: "+err.Error())
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

// shareStore is the one place that knows where the share records live. Three
// commands read them; a second spelling of this path would let `serve` stream a
// different file from the one `poll` writes.
func shareStore() share.Store {
	return share.Store{Path: filepath.Join(configDir(), "shares.json")}
}

// defaultLiveAddr is where the live stream is served. Loopback, because the
// only thing that should reach it today is a joiner on this machine or a tunnel
// the operator put there deliberately (ADR-005). A port that will not open is
// also what stops a second server starting by accident.
const defaultLiveAddr = "127.0.0.1:8787"

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}
	switch args[0] {
	case "auth":
		return runAuth(args[1:])
	case "share":
		return runShare(args[1:])
	case "poll":
		return runPoll(args[1:])
	case "serve":
		return runServe(args[1:])
	case "join":
		return runJoin(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `herdr-huddle — a GitHub draft PR as the shared thread for an agent session

Usage:
  herdr-huddle auth login [--repo owner/name] [--public]
  herdr-huddle auth status
  herdr-huddle auth logout
  herdr-huddle share [--slug name] [--base ref] [--invite @user]... [--dry-run]
  herdr-huddle poll [--once] [--interval 10s]
  herdr-huddle serve [--pane id] [--addr 127.0.0.1:8787] [--cols n] [--rows n]
  herdr-huddle join [--addr 127.0.0.1:8787]

Auth:
  login    Authorize with GitHub via the device flow and store the token.
           --repo scopes the token to that repo's visibility.
           --public forces the narrow public_repo scope.
  status   Report who the stored token belongs to.
  logout   Forget the stored token.

Share:
  share    Open the shared thread: an empty branch, pushed, with a draft pull
           request on it. Running it again reuses the existing branch and pull
           request rather than opening a second one.
           --slug   the name after herdr/; defaults to the branch, or to the
                    directory name when the branch is the default one.
           --base   the branch the pull request targets; defaults to the
                    remote's default branch.
           --invite give a user read access and add them to the allowlist of
                    comments that may drive the agent. Repeatable.
           --dry-run  report what would happen and change nothing.

Poll:
  poll     Keep every share in step with its agent: post what the agent has
           said as a comment, deliver /agent comments to it, and retire a share
           whose agent or pane is gone. Started by the plugin's startup hook,
           and locked so that two copies never deliver an instruction twice.
           --once      make one pass and exit, reporting what it did.
           --interval  how often to look, default 10s.

Serve and join:
  serve    Stream a share's agent pane to whoever the gate lets in. The pane
           and the allowlist come from the same active share, because the
           allowlist is what gates the endpoint; --pane picks the share when
           several are active. Every joiner gets its own read-only stream of
           the pane, freshly painted from the top, and is checked against the
           allowlist before a single frame is observed. Nothing is written to
           the pane; the way to steer the agent is still a comment on the
           thread.
           --addr        where to listen; loopback by default.
           --pane        which pane to stream.
           --cols, --rows  the viewport the stream is rendered at.
           --no-tunnel   do not start a tunnel; serve on this address only.
           Unless --no-tunnel is given, serve starts a quick tunnel (it needs
           cloudflared installed) and prints the one line to send. Without
           cloudflared it says so and serves locally instead.
  join     Join a live share: draw the stream in this terminal until it ends.
             herdr-huddle join <address>      the line serve printed
           --addr        the share to join when no address is given.
           Presents the stored GitHub token, or runs GitHub's device flow in a
           browser when there is none, and is refused without the share's
           allowlist. One-way: a joiner sends only its identity.

A share is the whole of it: share opens the thread, serve streams its agent
live, and join is the other person's window into it. The writes to GitHub are
still made by poll.
`)
}

// runShare implements the share command.
func runShare(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	slug := fs.String("slug", "", "the name after herdr/ (default: derived from the branch)")
	base := fs.String("base", "", "the branch the pull request targets (default: the remote's default branch)")
	dryRun := fs.Bool("dry-run", false, "report what would happen and change nothing")
	var invites stringList
	fs.Var(&invites, "invite", "give a user read access and add them to the allowlist (repeatable, with or without @)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: `share` takes no positional arguments, got %q", errUsage, fs.Arg(0))
	}

	ctx, stop := withSignals()
	defer stop()

	token := ""
	if !*dryRun {
		// A dry run needs no token: it makes no requests.
		loaded, _, err := defaultStore().Load(ctx)
		if err != nil {
			return err
		}
		token = loaded
	}

	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	result, err := share.Open(ctx, &repo.Repo{Dir: dir}, &github.Client{Token: token}, share.Request{
		Slug:   *slug,
		Base:   *base,
		Invite: invites,
		DryRun: *dryRun,
	})
	// The result is printed even when Open failed, because a dry run or a
	// partial share has already told the operator something useful.
	if result.Branch != "" {
		printShareResult(os.Stdout, result)
	}
	if err != nil {
		return err
	}

	if result.DryRun {
		fmt.Println("\nDry run: nothing was created, pushed or requested.")
		return nil
	}

	// The poller reads this. A failure to record the share is reported but does
	// not undo the pull request that now exists.
	origin, warnings := localOrigin(ctx)
	for _, warning := range warnings {
		fmt.Fprintf(os.Stdout, "warning   %s\n", warning)
	}

	store := shareStore()
	state, resumed, err := recordShare(store, result, origin, time.Now())
	if err != nil {
		return fmt.Errorf("the pull request is open, but recording it for the poller failed: %w", err)
	}
	if resumed {
		fmt.Println("resumed   the existing thread: what has already been posted and delivered is not repeated")
	}
	if origin.PaneID == "" {
		// Nothing to drive or sync from, so the poller retires the share. Saying
		// so here is the difference between that being deliberate and looking
		// like a bug.
		return nil
	}
	fmt.Printf("agent     %s in pane %s\n", state.Origin.Agent, state.Origin.PaneID)
	if state.Origin.Session != "" {
		fmt.Printf("session   %s\n", state.Origin.Session)
	} else {
		fmt.Println("session   none found; the terminal will be read instead and labelled partial")
	}
	return nil
}

// localOrigin binds a share to the agent it is driving (ADR-003). Without this
// the poller knows where to post and not what to drive.
//
// It never fails: a share opened outside a Herdr pane is still a useful thread,
// and the operator is told why nothing will be synced.
func localOrigin(ctx context.Context) (share.Origin, []string) {
	pane := os.Getenv("HERDR_PANE_ID")
	if pane == "" {
		return share.Origin{}, []string{
			"not running inside a Herdr pane, so this share is not bound to an agent: herdr-huddle will post nothing and deliver nothing. Re-run `share` from the agent's pane to sync it.",
		}
	}

	client := &herdr.Client{}
	agent, err := client.Agent(ctx, pane)
	if err != nil {
		return share.Origin{}, []string{fmt.Sprintf("could not read the agent in pane %s, so this share is not bound to one: %v", pane, err)}
	}

	origin := share.Origin{Agent: agent.Label(), PaneID: agent.PaneID, CWD: agent.CWD}
	if origin.PaneID == "" {
		origin.PaneID = pane
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	src, err := session.Resolve(home, agent)
	if err != nil {
		return origin, []string{fmt.Sprintf("could not look for the transcript: %v", err)}
	}
	// Record how the transcript was found even when it was not, so the share
	// says the same thing the poller will say on its first pass.
	origin.Session = src.Path
	origin.Kind = src.Kind
	origin.Partial = src.Partial
	if src.Path != "" {
		origin.SessionID, _ = session.SessionID(src.Path)
		return origin, nil
	}
	return origin, []string{"no session file was found for this pane: " + src.Reason}
}

// recordShare stores a share for the poller, bound to the origin it was opened
// from.
//
// This is a function of its own because the wiring here, not the parts, is what
// breaks: a record written without its origin looks exactly like a share opened
// outside a pane. The poller then retires it on its first pass, and a thread that
// looks perfectly healthy never receives a single turn. That happened, and the
// parts all had tests.
func recordShare(store share.Store, result share.Result, origin share.Origin, now time.Time) (share.State, bool, error) {
	return store.Record(share.FromResult(result, origin, now), seedTranscript(origin.Kind, origin.Session))
}

// seedTranscript is where a brand new share's transcript cursor starts: the end
// of the session, so opening a share does not dump the conversation the
// operator has already had into the thread.
func seedTranscript(kind, path string) string {
	if path == "" {
		return ""
	}
	cursor, err := thread.EndCursor(kind, path)
	if err != nil {
		return ""
	}
	return cursor
}

// runPoll keeps the threads and their agents in step. ADR-001 makes it a
// long-lived process because it has to watch in both directions: the GitHub
// side has no webhook to receive and the agent side has no way to be told.
func runPoll(args []string) error {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	once := fs.Bool("once", false, "make one pass and exit")
	interval := fs.Duration("interval", 0, "how often to look for changes")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: `poll` takes no positional arguments, got %q", errUsage, fs.Arg(0))
	}

	ctx, stop := withSignals()
	defer stop()

	// The lock is taken before anything else, including reading a token: a
	// second poller must do nothing at all, not even touch the keychain.
	var release func()
	if !*once {
		var err error
		release, err = poll.Acquire(filepath.Join(configDir(), "poll.lock"))
		if err != nil {
			return err
		}
		defer release()
	}

	intervalValue := effectiveInterval(*interval)
	shares := shareStore()
	poller := &poll.Poller{
		Shares: shares,
		Herdr:  &herdr.Client{},
		Forge:  &lazyForge{store: defaultStore()},
		Options: poll.Options{
			Interval: intervalValue,
		},
		Log: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
		},
	}

	if *once {
		// Deliberately no lock: a one-shot pass is for looking at what the
		// poller would do, and refusing to answer because the daemon is running
		// would make it useless exactly when it is wanted.
		out, err := poller.Once(ctx)
		printPollResult(os.Stdout, out)
		return err
	}

	fmt.Fprintf(os.Stderr, "herdr-huddle: polling every %s\n", intervalValue)
	return poller.Run(ctx)
}

// lazyForge defers loading the token until a GitHub call actually needs one.
//
// Two things fall out of that. A poller with nothing to do — no shares, or only
// retired ones — never asks for a token at all, so it does not fail on a machine
// where nobody has logged in yet. And a daemon that started before `auth login`
// starts working once the login happens, instead of sitting there quietly until
// the plugin is restarted.
type lazyForge struct {
	store  *auth.TokenStore
	client *github.Client
}

// The token is read once and kept: a pass happens every ten seconds, and the
// keychain is reached through a subprocess on Linux.
func (f *lazyForge) load(ctx context.Context) (*github.Client, error) {
	if f.client != nil {
		return f.client, nil
	}
	token, _, err := f.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	f.client = &github.Client{Token: token}
	return f.client, nil
}

func (f *lazyForge) CreateComment(ctx context.Context, owner, repoName string, number int, body string) (github.Comment, error) {
	client, err := f.load(ctx)
	if err != nil {
		return github.Comment{}, err
	}
	return client.CreateComment(ctx, owner, repoName, number, body)
}

func (f *lazyForge) ListComments(ctx context.Context, owner, repoName string, number int, since time.Time, etag string) (github.Comments, error) {
	client, err := f.load(ctx)
	if err != nil {
		return github.Comments{}, err
	}
	return client.ListComments(ctx, owner, repoName, number, since, etag)
}

func (f *lazyForge) Acknowledge(ctx context.Context, owner, repoName string, commentID int64, content string) error {
	client, err := f.load(ctx)
	if err != nil {
		return err
	}
	return client.Acknowledge(ctx, owner, repoName, commentID, content)
}

// effectiveInterval reports the interval that will actually be used, so the
// banner does not say "0s" when the default is doing the work.
func effectiveInterval(flagValue time.Duration) time.Duration {
	if flagValue > 0 {
		return flagValue
	}
	return poll.DefaultInterval
}

// runServe streams a share's pane to whoever the gate lets in.
//
// ADR-006 makes `serve` own the door: it starts a quick tunnel when cloudflared
// is available and prints the one line to send. Nobody configures a port, an
// address, or a tunnel. What gates that public endpoint is the share's
// allowlist, checked before the first frame, which is why the pane and the
// allowlist come from the same record below.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", defaultLiveAddr, "where to listen for joiners")
	pane := fs.String("pane", "", "the pane to stream (default: the pane the active share is bound to)")
	cols := fs.Int("cols", live.DefaultCols, "the width the stream is rendered at")
	rows := fs.Int("rows", live.DefaultRows, "the height the stream is rendered at")
	noTunnel := fs.Bool("no-tunnel", false, "do not start a tunnel; serve on this address only")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: `serve` takes no positional arguments, got %q", errUsage, fs.Arg(0))
	}

	state, err := shareForServe(shareStore(), *pane)
	if err != nil {
		return err
	}

	ctx, stop := withSignals()
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", *addr, err)
	}

	server := &live.Server{
		Pane:    state.Origin.PaneID,
		Cols:    *cols,
		Rows:    *rows,
		Observe: &live.HerdrObserver{},
		Gate:    &live.Gate{Verify: live.GitHubVerifier{}, Allowlist: state.Allowlist},
		Log: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
		},
	}
	fmt.Fprintf(os.Stderr, "herdr-huddle: streaming %s for %s (%dx%d)\n",
		state.Origin.PaneID, state.Repo, *cols, *rows)

	switch {
	case *noTunnel:
		printJoinLine("join from this machine:", ln.Addr().String())
	default:
		fmt.Fprintf(os.Stderr, "herdr-huddle: starting a tunnel…\n")
		tunnel, err := live.StartTunnel(ctx, "", "http://"+ln.Addr().String())
		switch {
		case err == nil:
			defer func() { _ = tunnel.Close() }()
			printJoinLine("live share ready — send them:", tunnel.URL)
		case errors.Is(err, live.ErrTunnelBinaryMissing):
			fmt.Fprintf(os.Stderr, "herdr-huddle: cloudflared is not installed, so this share is local only.\n"+
				"  install it (nix profile install nixpkgs#cloudflared, or environment.systemPackages = [ pkgs.cloudflared ]) and re-run for a link to send.\n")
			printJoinLine("join from this machine:", ln.Addr().String())
		default:
			fmt.Fprintf(os.Stderr, "herdr-huddle: no tunnel: %v\n", err)
			printJoinLine("join from this machine:", ln.Addr().String())
		}
	}
	return server.Serve(ctx, ln)
}

// printJoinLine is the whole instruction the collaborator receives: one line
// to send, one to run.
func printJoinLine(what, endpoint string) {
	fmt.Fprintf(os.Stderr, "herdr-huddle: %s\n  herdr-huddle join %s\n", what, endpoint)
}

// shareForServe resolves the pane *and* the gate from one active share.
//
// They cannot come from different places: the allowlist is the only thing
// standing between a public endpoint and the pane (ADR-006), so a stream with
// no share behind it has nothing to gate with and is refused here rather than
// served open. --pane picks which share when several are active; it never
// invents one.
func shareForServe(store share.Store, pane string) (share.State, error) {
	states, err := store.Load()
	if err != nil {
		return share.State{}, err
	}
	var active []share.State
	for _, state := range states {
		if state.Active() && state.Origin.PaneID != "" {
			active = append(active, state)
		}
	}

	if pane != "" {
		var matching []share.State
		for _, state := range active {
			if state.Origin.PaneID == pane {
				matching = append(matching, state)
			}
		}
		switch len(matching) {
		case 1:
			return matching[0], nil
		case 0:
			return share.State{}, fmt.Errorf("no active share is bound to pane %s, so it has no allowlist to gate the stream: run `share` from that pane first", pane)
		default:
			return share.State{}, fmt.Errorf("pane %s is bound to more than one active share, so which allowlist gates the stream is ambiguous", pane)
		}
	}

	switch len(active) {
	case 1:
		return active[0], nil
	case 0:
		return share.State{}, errors.New("no active share is bound to a pane, so there is nothing to stream and no allowlist to gate it: run `share` from the agent's pane, or pass --pane")
	default:
		panes := make([]string, 0, len(active))
		for _, state := range active {
			panes = append(panes, state.Origin.PaneID)
		}
		return share.State{}, fmt.Errorf("more than one active share is bound to a pane (%s), so which to stream is ambiguous: pass --pane", strings.Join(panes, ", "))
	}
}

// runJoin joins a live share: present who you are, then draw what the pane
// shows, until it ends or Ctrl-C.
//
// Phase 2 is one-way: the joiner sends only the hello, and the way to steer
// the agent is still a comment on the thread (ADR-005).
func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	addr := fs.String("addr", defaultLiveAddr, "the share to join when no address is given")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("%w: `join` takes at most one address, got %q and %q", errUsage, fs.Arg(0), fs.Arg(1))
	}
	endpoint := *addr
	if fs.NArg() == 1 {
		endpoint = fs.Arg(0)
	}

	ctx, stop := withSignals()
	defer stop()

	token, err := joinToken(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "herdr-huddle: joining %s (Ctrl-C to leave)\n", endpoint)
	err = live.Join(ctx, endpoint, token, os.Stdout)
	if ctx.Err() != nil {
		// The operator left. Ending the connection is how they leave, so the
		// error that follows from it is the leaving, not a failure to report.
		return nil
	}
	return err
}

// joinToken is the pairing half of the gate: the stored token when there is
// one, GitHub's device flow in the browser otherwise.
//
// The device-flow token is deliberately not saved — a scope-less identity
// token must not overwrite the repo-scoped token `auth login` stores.
func joinToken(ctx context.Context) (string, error) {
	store := auth.TokenStore{}
	if token, _, err := store.Load(ctx); err == nil && strings.TrimSpace(token) != "" {
		return token, nil
	}
	clientID := resolveClientID()
	if clientID == "" {
		return "", errors.New("no stored token and no client id to pair with: run `herdr-huddle auth login` first, or set HERDR_HUDDLE_CLIENT_ID")
	}
	flow := auth.DeviceFlow{ClientID: clientID}
	code, err := flow.Start(ctx)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "herdr-huddle: to join, open %s and enter %s\n", code.VerificationURI, code.UserCode)
	token, err := flow.Wait(ctx, code)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

// printPollResult reports one pass in the order the work happened.
func printPollResult(w io.Writer, out poll.Result) {
	for _, line := range out.Posted {
		fmt.Fprintf(w, "posted    %s\n", line)
	}
	for _, line := range out.Injected {
		fmt.Fprintf(w, "delivered %s\n", line)
	}
	for _, refusal := range out.Refused {
		fmt.Fprintf(w, "refused   @%s: %s\n", refusal.Author, refusal.Reason)
	}
	for _, branch := range out.Retired {
		fmt.Fprintf(w, "retired   %s\n", branch)
	}
	for _, warning := range out.Warnings {
		fmt.Fprintf(w, "warning   %s\n", warning)
	}
	if len(out.Posted)+len(out.Injected)+len(out.Refused)+len(out.Retired)+len(out.Warnings) == 0 {
		fmt.Fprintln(w, "nothing to do")
	}
}

// printShareResult reports what happened, in the order the pieces came into
// existence, so a failure partway through is legible.
func printShareResult(w io.Writer, res share.Result) {
	verb := "created"
	if res.Reused {
		verb = "reused"
	}
	pr := fmt.Sprintf("#%d %s (%s, %s)", res.PullRequest.Number, res.PullRequest.HTMLURL, draftLabel(res.PullRequest.Draft), verb)
	if res.DryRun {
		pr = "would be opened"
	}
	base := res.Base
	if base == "" {
		base = "(unknown)"
	}
	branch := res.Branch + " (reused)"
	if res.BranchCreated {
		branch = res.Branch + " (created)"
	}

	fmt.Fprintf(w, "repo      %s\n", res.Repo)
	fmt.Fprintf(w, "branch    %s\n", branch)
	fmt.Fprintf(w, "base      %s  from %s\n", base, res.BaseRef)
	fmt.Fprintf(w, "pull      %s\n", pr)
	if len(res.Allowlist) > 0 {
		fmt.Fprintf(w, "allowlist %s\n", strings.Join(res.Allowlist, " "))
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "warning   %s\n", warning)
	}
}

func draftLabel(draft bool) string {
	if draft {
		return "draft"
	}
	return "ready"
}

// stringList collects a repeatable flag, and splits a comma-separated value so
// --invite=bob,carol works too.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func runAuth(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected `auth login`, `auth status` or `auth logout`", errUsage)
	}
	switch args[0] {
	case "login":
		return authLogin(args[1:])
	case "status":
		return authStatus(args[1:])
	case "logout":
		return authLogout(args[1:])
	default:
		return fmt.Errorf("%w: unknown auth command %q", errUsage, args[0])
	}
}

// commands run under a context that a Ctrl-C cancels, so an interrupted login
// stops polling GitHub rather than spinning until the code expires.
func withSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func authLogin(args []string) error {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository to scope the token for, as owner/name")
	public := fs.Bool("public", false, "request the narrow public_repo scope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %s", errUsage, err)
	}

	id := resolveClientID()
	if id == "" {
		return errors.New("no OAuth client_id available. Register an OAuth App at " +
			"https://github.com/settings/applications/new (enable Device Flow, no callback URL needed), " +
			"then build with -ldflags \"-X main.clientID=<id>\" or set HERDR_HUDDLE_CLIENT_ID")
	}

	ctx, stop := withSignals()
	defer stop()

	scopes, err := scopesFor(ctx, *repo, *public)
	if err != nil {
		return err
	}

	flow := &auth.DeviceFlow{ClientID: id, Scopes: scopes}
	code, err := flow.Start(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Open %s\nEnter code: %s\n", code.VerificationURI, code.UserCode)
	fmt.Printf("Waiting for approval (the code expires in %s)…\n", code.ExpiresIn)

	token, err := flow.Wait(ctx, code)
	if err != nil {
		return err
	}

	backend, err := defaultStore().Save(ctx, token.AccessToken)
	if err != nil {
		return err
	}
	fmt.Printf("Authorized. Token stored in the %s (granted scopes: %s)\n", backend, orNone(token.Scope))
	if backend == "file" {
		fmt.Fprintln(os.Stderr, "warning: no OS keychain was available, so the token is in a 0600 file instead.")
		fmt.Fprintln(os.Stderr, "         anyone who can read that file as your user can act as you on GitHub.")
	}
	return nil
}

// scopesFor picks the narrowest scope that will work, probing the repo's
// visibility when one was named.
func scopesFor(ctx context.Context, repo string, forcePublic bool) ([]string, error) {
	switch {
	case forcePublic:
		return auth.ScopesFor(false), nil
	case repo != "":
		owner, name, err := splitRepo(repo)
		if err != nil {
			return nil, err
		}
		scopes, err := auth.ScopesForRepo(ctx, &github.Client{}, owner, name)
		if err != nil {
			return nil, fmt.Errorf("could not determine the visibility of %s: %w", repo, err)
		}
		return scopes, nil
	default:
		// With no repo named there is no way to know, and under-asking would
		// fail later with a confusing 404.
		fmt.Fprintln(os.Stderr, "note: no --repo given, requesting the broad `repo` scope; pass --repo owner/name to narrow it")
		return auth.ScopesFor(true), nil
	}
}

func authStatus(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: `auth status` takes no arguments", errUsage)
	}
	ctx, stop := withSignals()
	defer stop()

	token, source, err := defaultStore().Load(ctx)
	if err != nil {
		return err
	}
	// Verifying against GitHub is the point: a revoked or under-scoped token
	// must fail here rather than halfway through opening a pull request.
	login, err := (&github.Client{Token: token}).Viewer(ctx)
	if err != nil {
		return fmt.Errorf("the token from the %s is not usable: %w", source, err)
	}
	fmt.Printf("Authenticated as %s (token from the %s)\n", login, source)
	return nil
}

func authLogout(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: `auth logout` takes no arguments", errUsage)
	}
	ctx, stop := withSignals()
	defer stop()

	if err := defaultStore().Delete(ctx); err != nil {
		return err
	}
	fmt.Println("Removed the stored token.")
	return nil
}

func resolveClientID() string {
	if v := strings.TrimSpace(os.Getenv("HERDR_HUDDLE_CLIENT_ID")); v != "" {
		return v
	}
	return strings.TrimSpace(clientID)
}

// defaultStore keeps state where Herdr gives plugins a config directory, so the
// CLI and the plugin's hooks agree on where the token lives.
func defaultStore() *auth.TokenStore {
	return &auth.TokenStore{
		Service:     serviceName,
		Account:     accountName,
		FallbackDir: configDir(),
	}
}

func configDir() string {
	if d := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, serviceName)
	}
	return "."
}

// splitRepo validates the owner/name form. The parts end up in an API path, so
// anything beyond a single slash is rejected rather than escaped and hoped for.
func splitRepo(slug string) (owner, repo string, err error) {
	parts := strings.Split(strings.TrimSpace(slug), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: --repo must be owner/name, got %q", errUsage, slug)
	}
	return parts[0], parts[1], nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none reported"
	}
	return s
}
