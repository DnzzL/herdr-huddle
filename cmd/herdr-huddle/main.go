// Command herdr-huddle turns a live agent session into a GitHub draft PR two
// people can plan in, steer, and keep as a record.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/DnzzL/herdr-huddle/internal/auth"
	"github.com/DnzzL/herdr-huddle/internal/github"
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

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}
	switch args[0] {
	case "auth":
		return runAuth(args[1:])
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

Auth:
  login    Authorize with GitHub via the device flow and store the token.
           --repo scopes the token to that repo's visibility.
           --public forces the narrow public_repo scope.
  status   Report who the stored token belongs to.
  logout   Forget the stored token.
`)
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
