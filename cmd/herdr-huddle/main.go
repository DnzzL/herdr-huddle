// Command herdr-huddle turns a live agent session into a GitHub draft PR two
// people can plan in, steer, and keep as a record.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/DnzzL/herdr-huddle/internal/target"
	"github.com/DnzzL/herdr-huddle/internal/thread"
	"github.com/DnzzL/herdr-huddle/internal/tui"
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
  herdr-huddle serve [--pane id] [--open|--closed] [--moderated] [--no-tunnel] [--addr host:port]
  herdr-huddle join [address] [--addr 127.0.0.1:8787]

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
  serve    Open the huddle: stream a share's agent pane to whoever gets in,
           and deliver what they type to the agent. The pane and the allowlist
           come from the same active share; --pane picks the share when several
           are active. Every joiner gets its own read-only stream of the pane,
           rendered at their own terminal's size and freshly painted from the
           top, and is checked before a single frame is observed. Nothing is
           ever written to the pane itself.
           --addr        where to listen; loopback by default.
           --pane        which pane to stream.
           --cols, --rows  what a joiner gets when it does not report its own
                         window size.
           --no-tunnel   do not start a tunnel; serve on this address only.
           --open        let anyone with the link and a GitHub identity in,
                         without asking. The link becomes the invitation, and
                         the admission lasts only as long as serve does.
           --closed      the allowlist or nothing, and nobody is asked — for a
                         serve nobody is sitting in front of.
           --moderated   put every instruction to you, with the words in front
                         of you, before the agent sees it. Letting somebody in
                         and letting them drive stop being one decision.
           --notify      announce the join line as a Herdr notification. For
                         the plugin action, whose output only reaches
                         herdr plugin log.
           By default the door knocks: somebody who is not on the allowlist
           yet proves who they are on GitHub, you are asked here, and one
           keypress lets them in for good — their pull-request comments
           included. Unless --no-tunnel is given, serve starts a quick tunnel
           (it needs cloudflared installed) and prints the one line to send.
           Every delivered instruction is recorded on the thread; the record is
           queued and retried if GitHub is unreachable, and delivery never
           waits for it.
  join     Join a huddle and see it as a room: the agent's pane above, who
           else is here and what the agent is doing below, and a line to type
           into.
             herdr-huddle join <address>      the line serve printed
           --addr        the share to join when no address is given.
           Presents the stored GitHub token, or runs GitHub's device flow in a
           browser when there is none. What you type is delivered to the agent
           immediately and recorded on the thread afterwards; a held or failed
           delivery is reported in the room rather than silently dropped.
           Ctrl-T switches the input line between the agent and the room:
           talking to the room reaches the people in it and never the agent,
           and is not posted to the thread. Up-arrow recalls what you sent. A
           dropped connection is retried with a backoff until you give up with
           Ctrl-C. Piped somewhere that is not a terminal, it writes the pane's
           bytes out plainly instead.

A share is the whole of it: share opens the thread, serve opens the huddle on
it, and join is everyone else's window in. The pull request is the artifact the
huddle leaves behind. The writes to GitHub are still made by poll.
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

	// What this acts on, asked rather than guessed (ADR-009). Invoked as a
	// Herdr action the working directory is the plugin's, so the pane is the
	// only source that can be right.
	what, targetWarnings := target.Resolve(ctx, dir, &herdr.Client{}, target.GitRoots{})
	for _, warning := range targetWarnings {
		fmt.Fprintf(os.Stdout, "warning   %s\n", warning)
	}
	if what.Root != "" {
		dir = what.Root
	}
	if what.FromPane {
		fmt.Printf("project   %s (from the agent in pane %s)\n", what.Root, what.PaneID)
	}

	// Before anything is created: the agent is read first and checked against
	// the project, because a mismatch discovered after the push is a branch
	// and a pull request nobody asked for. Resolve makes the two agree by
	// construction; this stays as the assertion that it did.
	origin, warnings := localOrigin(ctx)
	for _, warning := range warnings {
		fmt.Fprintf(os.Stdout, "warning   %s\n", warning)
	}
	if root, rootErr := (&repo.Repo{Dir: dir}).Root(ctx); rootErr == nil {
		if err := refuseForeignAgent(root, origin); err != nil {
			return err
		}
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
	// Which project the agent is in, as git sees it. A directory that is not a
	// repository leaves this empty, and the share is let through unchecked —
	// refusing on "I could not tell" would block the agents herdr-huddle is
	// least able to help.
	if agent.CWD != "" {
		if root, err := (&repo.Repo{Dir: agent.CWD}).Root(ctx); err == nil {
			origin.Root = root
		}
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

// refuseForeignAgent stops a share whose agent works in another project.
//
// `share` reads the repository from the process's working directory and the
// agent from the pane it runs in. Those are the same thing only by convention
// — run it from one project's pane while the agent works in another and you
// get a pull request on one repository bound to an agent in a second, with no
// sign that anything is wrong until the thread fills with a conversation about
// code it does not contain.
//
// A worktree of the same repository counts as another project: it is another
// branch, so another thread (ADR-007's scope).
func refuseForeignAgent(repoRoot string, origin share.Origin) error {
	if origin.Root == "" || repoRoot == "" {
		return nil // no pane, or a directory git knows nothing about
	}
	if filepath.Clean(origin.Root) == filepath.Clean(repoRoot) {
		return nil
	}
	return fmt.Errorf("the agent in pane %s works in %s, but this would open the thread on %s.\n"+
		"  A share binds one agent to one pull request, so run `share` from the agent's own project",
		origin.PaneID, origin.Root, repoRoot)
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
		Alert: func(title, body string) {
			notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := (&herdr.Client{}).Notify(notifyCtx, title, body, herdr.SoundRequest); err != nil {
				fmt.Fprintf(os.Stderr, "herdr-huddle: %s — %s (could not raise a notification: %v)\n", title, body, err)
			}
		},
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
	cols := fs.Int("cols", live.DefaultCols, "the width a joiner gets when it does not report its own")
	rows := fs.Int("rows", live.DefaultRows, "the height a joiner gets when it does not report its own")
	noTunnel := fs.Bool("no-tunnel", false, "do not start a tunnel; serve on this address only")
	open := fs.Bool("open", false, "let anyone with a GitHub identity in, without asking")
	closed := fs.Bool("closed", false, "the allowlist or nothing: never ask, never knock")
	moderated := fs.Bool("moderated", false, "approve every instruction before it reaches the agent")
	notify := fs.Bool("notify", false, "announce the join line as a Herdr notification (for a plugin action, whose output nobody sees)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: `serve` takes no positional arguments, got %q", errUsage, fs.Arg(0))
	}
	if *open && *closed {
		return fmt.Errorf("%w: --open and --closed are opposite doors; pass at most one", errUsage)
	}

	ctx, stop := withSignals()
	defer stop()

	// The pane this was invoked from, when it was not told which to stream.
	// An action has one; a shell in a pane has one too.
	invokedFrom := ""
	if *pane == "" {
		what, _ := target.Resolve(ctx, ".", &herdr.Client{}, target.GitRoots{})
		invokedFrom = what.PaneID
	}

	state, err := shareForServe(shareStore(), *pane, invokedFrom)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", *addr, err)
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
	}

	at := &console{in: bufio.NewReader(os.Stdin), out: os.Stderr, notify: notifier(ctx, logf)}
	// Reading starts now, so that anything typed before a question was asked
	// is recognisably older than it.
	at.listen()

	server := &live.Server{
		Pane:           state.Origin.PaneID,
		Cols:           *cols,
		Rows:           *rows,
		Observe:        &live.HerdrObserver{},
		Gate:           doorFor(shareStore(), state, *open, *closed, at, logf),
		Instructor:     live.HerdrInstructor{},
		Ledger:         threadLedger(ctx, state),
		Spool:          spoolFor(state),
		ThreadURL:      state.URL,
		Host:           operatorLogin(ctx),
		Moderate:       moderatorFor(*moderated, at),
		Status:         live.HerdrStatuser{},
		StatusInterval: live.DefaultStatusInterval,
		Log:            logf,
	}
	fmt.Fprintf(os.Stderr, "herdr-huddle: streaming %s for %s — the record is %s\n",
		state.Origin.PaneID, state.Repo, state.URL)
	fmt.Fprintf(os.Stderr, "herdr-huddle: the door is %s\n", doorLabel(*open, *closed))
	if *moderated {
		fmt.Fprintf(os.Stderr, "herdr-huddle: moderated — every instruction is put to you before the agent sees it\n")
	}

	announce := func(endpoint string) {
		if *notify {
			announceJoinLine(ctx, endpoint, logf)
		}
	}

	switch {
	case *noTunnel:
		printJoinLine("join from this machine:", ln.Addr().String())
		announce(ln.Addr().String())
	default:
		fmt.Fprintf(os.Stderr, "herdr-huddle: starting a tunnel…\n")
		tunnel, err := live.StartTunnel(ctx, "", "http://"+ln.Addr().String())
		switch {
		case err == nil:
			defer func() { _ = tunnel.Close() }()
			printJoinLine("live share ready — send them:", tunnel.URL)
			announce(tunnel.URL)
		case errors.Is(err, live.ErrTunnelBinaryMissing):
			fmt.Fprintf(os.Stderr, "herdr-huddle: cloudflared is not installed, so this share is local only.\n"+
				"  install it (nix profile install nixpkgs#cloudflared, or environment.systemPackages = [ pkgs.cloudflared ]) and re-run for a link to send.\n")
			printJoinLine("join from this machine:", ln.Addr().String())
			announce(ln.Addr().String())
		default:
			fmt.Fprintf(os.Stderr, "herdr-huddle: no tunnel: %v\n", err)
			printJoinLine("join from this machine:", ln.Addr().String())
			announce(ln.Addr().String())
		}
	}
	return server.Serve(ctx, ln)
}

// threadLedger is the record half of steering: delivered instructions are
// posted to the pull request with the operator's token.
//
// A missing or dead token is not fatal — delivery never waits for the record
// (ADR-005), so instructions arrive live and their records queue until the
// ledger works again. The warning is printed once, at startup, because the
// first failed post would otherwise be the first sign.
func threadLedger(ctx context.Context, state share.State) *thread.InstructionLedger {
	owner, name := state.Owner()
	store := auth.TokenStore{}
	token, _, err := store.Load(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		fmt.Fprintf(os.Stderr, "herdr-huddle: no usable operator token: instructions will still reach the agent, but their records will queue until you run `herdr-huddle auth login`\n")
	}
	return &thread.InstructionLedger{
		Client: &github.Client{Token: token},
		Owner:  owner,
		Repo:   name,
		Number: state.Number,
	}
}

// doorFor builds the gate ADR-007 describes: the allowlist, plus a way to say
// yes to somebody who is not on it yet.
//
// Knocking is the default because it is the only arrangement that is both
// frictionless for the guest — the link is the whole invitation — and an
// actual decision by the operator, taken with the asker's proven login in
// front of them. --open trades the decision for convenience; --closed keeps
// today's behaviour for an unattended serve.
func doorFor(store share.Store, state share.State, open, closed bool, at *console, logf func(string, ...any)) *live.Gate {
	gate := &live.Gate{
		Verify:    live.GitHubVerifier{},
		Allowlist: state.Allowlist,
		Open:      open,
		Remember:  rememberJoiner(store, state.Key()),
		Log:       logf,
	}
	if !open && !closed {
		gate.Admit = at
	}
	return gate
}

// moderatorFor returns the thing that approves instructions, or nil for the
// default — anyone the door admitted steers the agent directly.
func moderatorFor(on bool, at *console) live.Moderator {
	if !on {
		return nil
	}
	return live.ModeratorFunc(at.ApproveInstruction)
}

func doorLabel(open, closed bool) string {
	switch {
	case open:
		return "open: anyone with the link and a GitHub identity is let in, for as long as this runs"
	case closed:
		return "closed: only the share's allowlist, and nobody is asked"
	default:
		return "knock: somebody new asks here, and you answer"
	}
}

// console is the operator answering questions at their own terminal.
//
// There is one of these and not two, although two things ask questions — the
// door and the moderator — because both read the same stdin. Two readers on
// one terminal interleave their prompts and race for the answer.
//
// Three properties it has to have, and each cost a bug to learn:
//
//   - **One question at a time, in arrival order.** The turn is a channel and
//     not a mutex, because Go serves blocked channel receives first-come-first-
//     served and makes no such promise for a mutex. With two people waiting,
//     the operator should be asked about the one who asked first.
//   - **A question can be abandoned.** Reading stdin blocks, so a joiner who
//     sends an instruction and disconnects would otherwise leave the operator
//     staring at a prompt about somebody who is gone — while holding the turn,
//     so nobody else can be admitted either. Every wait watches its context.
//   - **An answer answers the question in front of it.** A single reader
//     goroutine owns stdin, and anything typed before a question was asked is
//     discarded rather than applied to it. Otherwise an idle "y" sitting in the
//     buffer silently admits the next person who knocks.
type console struct {
	in  *bufio.Reader
	out io.Writer
	// notify raises the question on the screen the operator is actually
	// looking at, which is their agent and not this log.
	notify func(title, body string)

	start   sync.Once
	turn    chan struct{}
	answers chan answer
	waiting atomic.Int64
}

// answer is a line the operator typed, and when they typed it.
//
// The time is the whole point. Draining the channel before asking is not
// enough: a line already read and waiting to be handed over is in flight, not
// in the channel, so it survives the drain and answers the next question. An
// answer older than the question was never an answer to it.
type answer struct {
	text string
	at   time.Time
}

// listen starts reading the terminal. It is called when `serve` starts, not
// when the first question is asked — otherwise "typed before the question" and
// "typed after it" are the same thing, because nothing was reading before.
func (c *console) listen() {
	c.start.Do(func() {
		c.turn = make(chan struct{}, 1)
		c.turn <- struct{}{}
		c.answers = make(chan answer)
		go func() {
			defer close(c.answers)
			for {
				line, err := c.in.ReadString('\n')
				if line != "" {
					c.answers <- answer{text: line, at: time.Now()}
				}
				if err != nil {
					return
				}
			}
		}()
	})
}

// Approve answers the door.
func (c *console) Approve(ctx context.Context, login string) (bool, error) {
	return c.put(ctx,
		"@"+login+" is at the door", "let them into the huddle?",
		fmt.Sprintf("@%s is at the door (github.com/%s).\n  let them into the huddle?", login, login))
}

// ApproveInstruction answers for one instruction on a moderated share.
//
// The words are shown in full before the question, because the question is
// about the words: "let @ana send something" is not a decision anybody can
// make.
func (c *console) ApproveInstruction(ctx context.Context, login, text string) (bool, error) {
	return c.put(ctx,
		"@"+login+" wants to steer the agent", firstLine(text),
		fmt.Sprintf("@%s wants to send the agent:\n\n    %s\n\n  send it?", login, indent(text)))
}

// put waits its turn, asks, and reads one answer. Anything but yes is no.
func (c *console) put(ctx context.Context, title, body, question string) (bool, error) {
	c.listen()

	behind := c.waiting.Add(1) - 1
	defer c.waiting.Add(-1)

	select {
	case <-c.turn:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { c.turn <- struct{}{} }()

	if c.notify != nil {
		c.notify(title, body)
	}

	queue := ""
	if behind > 0 {
		queue = fmt.Sprintf(" (%d more waiting)", behind)
	}
	// The bell is for the operator who is looking at their agent rather than
	// at this terminal — the same reason the notification exists.
	asked := time.Now()
	fmt.Fprintf(c.out, "\a\nherdr-huddle: %s%s [y/N] ", question, queue)

	for {
		select {
		case got, ok := <-c.answers:
			if !ok {
				return false, errors.New("nothing to read the answer from: the terminal is closed")
			}
			if got.at.Before(asked) {
				continue // typed before the question; it answered something else
			}
			reply := strings.ToLower(strings.TrimSpace(got.text))
			return reply == "y" || reply == "yes", nil
		case <-ctx.Done():
			fmt.Fprintf(c.out, "\nherdr-huddle: never mind — they are gone\n")
			return false, ctx.Err()
		}
	}
}

// firstLine keeps a notification body to something a toast can hold.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 140 {
		s = s[:139] + "…"
	}
	return s
}

// indent lays a multi-line instruction out under the question so it reads as
// a quotation rather than as more of the prompt.
func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// notifier raises a notification through Herdr, which already owns that
// surface for agent state changes — one place to configure, one place they
// appear. A failure is swallowed: a missing toast must never stop the question
// being asked.
func notifier(ctx context.Context, logf func(string, ...any)) func(title, body string) {
	client := &herdr.Client{}
	return func(title, body string) {
		notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := client.Notify(notifyCtx, "herdr-huddle: "+title, body, herdr.SoundRequest); err != nil {
			// Not fatal — the question is still on this terminal and the bell
			// still rang — but silence here would hide a notification path
			// that has been broken all session.
			logf("could not raise a notification: %v", err)
		}
	}
}

// rememberJoiner writes an admitted login onto the share, so the poller
// delivers their pull-request comments too. One list, two doors (ADR-005).
func rememberJoiner(store share.Store, key string) func(string) error {
	return func(login string) error {
		_, err := store.Allow(key, login)
		return err
	}
}

// operatorLogin asks GitHub who is running this, so the room can name the
// person at the pane.
//
// The operator is in the huddle without being connected to it — they are
// sitting at the terminal — so without this a joiner alone in the room is told
// they are the only one there while the operator watches over their shoulder
// (ADR-007).
//
// It fails soft, to nothing. A missing or dead token already warns on the
// ledger path a few lines below, and a room that does not name its host is the
// behaviour this replaced, not a broken one.
func operatorLogin(ctx context.Context) string {
	store := auth.TokenStore{}
	token, _, err := store.Load(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	login, err := (&github.Client{Token: token}).Viewer(ctx)
	if err != nil {
		return ""
	}
	return login
}

// spoolFor is where this share's owed instruction records are written down.
//
// One file per share, never one shared file: the records are posted to a
// specific pull request, so a restart that recovered another share's queue
// would file one thread's instructions under another's — silently, and into
// the thing that is supposed to be the record.
func spoolFor(state share.State) live.Spool {
	return live.FileSpool{Path: filepath.Join(configDir(), "pending", spoolName(state.Key())+".json")}
}

// spoolName turns a share key into a filename that is both readable and
// unambiguous: the key with the awkward characters flattened, plus a short
// digest of the original so two keys that flatten alike cannot collide.
func spoolName(key string) string {
	// The dot is deliberately not kept: a branch called `a/../b` would
	// otherwise put `..` in a filename. It costs a little readability on
	// branches with dots in them, and the digest is what carries uniqueness
	// anyway.
	flat := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			flat = append(flat, r)
		default:
			flat = append(flat, '-')
		}
	}
	name := strings.Trim(string(flat), "-")
	if name == "" {
		name = "share"
	}
	sum := sha256.Sum256([]byte(key))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// printJoinLine is the whole instruction the collaborator receives: one line
// to send, one to run.
func printJoinLine(what, endpoint string) {
	fmt.Fprintf(os.Stderr, "herdr-huddle: %s\n  herdr-huddle join %s\n", what, endpoint)
}

// announceJoinLine puts the same line on the operator's screen.
//
// A plugin action's output goes to `herdr plugin log` and nowhere else
// (ADR-009), so a huddle opened that way would be a tunnel whose URL nobody
// ever reads. A failure is logged and no more: the line is still on stderr for
// whoever can see stderr.
func announceJoinLine(ctx context.Context, endpoint string, logf func(string, ...any)) {
	notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := (&herdr.Client{}).Notify(notifyCtx,
		"herdr-huddle: the huddle is open",
		"herdr-huddle join "+endpoint,
		herdr.SoundDone)
	if err != nil {
		logf("could not announce the join line: %v", err)
	}
}

// shareForServe resolves the pane *and* the gate from one active share.
//
// They cannot come from different places: the allowlist is the only thing
// standing between a public endpoint and the pane (ADR-006), so a stream with
// no share behind it has nothing to gate with and is refused here rather than
// served open. --pane picks which share when several are active; it never
// invents one.
func shareForServe(store share.Store, pane, invokedFrom string) (share.State, error) {
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

	// Where the command was invoked from, when it was not told. A Herdr action
	// is titled "on this pane" and knows which one has focus, so preferring it
	// is what makes the title true; without it the action streams whichever
	// share happens to be active (ADR-009).
	if invokedFrom != "" {
		var here []share.State
		for _, state := range active {
			if state.Origin.PaneID == invokedFrom {
				here = append(here, state)
			}
		}
		if len(here) == 1 {
			return here[0], nil
		}
		// Being in a pane is itself a request for *that* pane. Falling back to
		// whichever share happens to be active would stream somebody else's
		// agent — the same silent mis-binding `share` was just taught to
		// refuse, and over a public tunnel.
		return share.State{}, fmt.Errorf("no active share is bound to pane %s, so there is nothing to stream here: run `share` from this pane first, or pass --pane to stream another", invokedFrom)
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
		return share.State{}, fmt.Errorf("more than one active share is bound to a pane (%s), so which to stream is ambiguous: run this from the agent's pane, or pass --pane", strings.Join(panes, ", "))
	}
}

// runJoin joins a live share: present who you are, then draw the room until
// it ends or Ctrl-C.
//
// On a terminal that is a terminal, this is the TUI — the pane on top, the
// room below it, and a line to type into (ADR-007). Piped somewhere, it falls
// back to writing the pane's bytes out unadorned, which is the only honest
// thing to do with chrome that has nothing to composite onto.
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

	if !tui.IsTerminal(os.Stdout) || !tui.IsTerminal(os.Stdin) {
		session, err := live.Dial(ctx, endpoint, token)
		if err != nil {
			return err
		}
		defer session.Close()
		fmt.Fprintf(os.Stderr, "herdr-huddle: joined %s (Ctrl-C to leave)\n", endpoint)
		return joinPlainly(ctx, session)
	}

	app := &tui.App{
		In:   os.Stdin,
		Out:  os.Stdout,
		Hint: "enter sends · ctrl-t switches between the agent and the room · ctrl-c leaves",
	}
	// A dialer rather than a connection: a dropped tunnel is a reconnect, not
	// the end of the huddle, and every redial reads the window size afresh
	// because it may have changed while we were away.
	return app.Run(ctx, func(ctx context.Context) (tui.Client, error) {
		cols, rows := tui.Size(os.Stdout)
		// The viewport is asked for before the first frame, because the server
		// starts this joiner's observer at whatever size the hello carried
		// (ADR-007). Getting it afterwards would cost a restart on arrival.
		session, err := live.Dial(ctx, endpoint, token, live.WithViewport(cols, tui.PaneRows(rows)))
		if err != nil {
			return nil, err
		}
		return session, nil
	})
}

// joinPlainly is the fallback for a join whose output is not a terminal: the
// pane's bytes to stdout, everything the room says to stderr, and lines from
// stdin as instructions.
func joinPlainly(ctx context.Context, session *live.Session) error {
	drawDone := make(chan error, 1)
	go func() { drawDone <- session.Draw(os.Stdout, os.Stderr) }()

	sayCh := make(chan string)
	go func() {
		defer close(sayCh)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			sayCh <- scanner.Text()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Ctrl-C: ending the connection is how they leave, so the error
			// that follows from it is the leaving, not a failure to report.
			return nil
		case err := <-drawDone:
			return err
		case line, ok := <-sayCh:
			if !ok {
				sayCh = nil // stdin ended; keep watching the stream
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := session.Say(line); err != nil {
				return fmt.Errorf("could not send: %w", err)
			}
		}
	}
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
	// This is printed on the failure path too, so every line has to be true of
	// what actually happened. A share that stopped at the base branch used to
	// report a branch "reused" and pull request "#0 (created)" — sending the
	// operator to look for neither.
	verb := "created"
	if res.Reused {
		verb = "reused"
	}
	pr := fmt.Sprintf("#%d %s (%s, %s)", res.PullRequest.Number, res.PullRequest.HTMLURL, draftLabel(res.PullRequest.Draft), verb)
	switch {
	case res.DryRun:
		pr = "would be opened"
	case res.PullRequest.Number == 0:
		pr = "not opened"
	}
	base := res.Base
	if base == "" {
		base = "(unknown)"
	}
	branch := res.Branch + " (not created)"
	switch {
	case res.BranchCreated:
		branch = res.Branch + " (created)"
	case res.Pushed, res.DryRun:
		branch = res.Branch + " (reused)"
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

// defaultStore keeps the token in one place, so the CLI and the plugin's hooks
// agree on where it lives whoever started them.
func defaultStore() *auth.TokenStore {
	return &auth.TokenStore{
		Service:     serviceName,
		Account:     accountName,
		FallbackDir: configDir(),
	}
}

func configDir() string {
	// Deliberately not HERDR_PLUGIN_CONFIG_DIR. Herdr sets that for a plugin
	// action and for the startup hook, and not for a shell — so honouring it
	// put the token `auth login` wrote somewhere the poller never looked
	// (ADR-009). One directory, whoever is asking; this override exists for
	// tests and for anyone who wants to move it deliberately.
	if d := os.Getenv("HERDR_HUDDLE_CONFIG_DIR"); d != "" {
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
